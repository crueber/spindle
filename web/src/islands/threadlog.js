// threadlog.js — live job log panels (docs/features/15_threads.md §6; PAR-190).
//
// A page carries one log region per job (`data-threadlog` + `data-threadlog-url`).
// The panel replays stored frames as JSON, then tails the same URL as an SSE
// fetch stream (sse.js — no EventSource, no new transport) and renders each
// frame by its kind:
//
//   • output / system text, with the runner's capture time in a gutter at each
//     line start (the time is the frame's, not a browser guess);
//   • step_start / step_end markers as boundaries with the step's conclusion
//     and duration;
//   • group_start / group_end as native <details> sections that collapse when
//     the group closes (a finished group is folded, a live one stays open);
//   • truncated markers as visible notes: a cap is never silent.
//
// Frames are deduped by sequence. A sequence gap (a frame the runner could not
// write) is rendered as a named gap, never closed up. A re-run's attempt reset
// re-bases the cursor instead of dropping the new attempt's frames (the old
// panel would have treated them as duplicates forever).
//
// If the stream cannot be established (a proxy that buffers streams, an old
// runtime, a transport bug), the island keeps the replayed log and says so. A
// 401 marks the panel and waits: an expired session must not kill the log — the
// tail resumes from the same cursor once the session is renewed.
//
// Pure helpers are headless-tested in web/test/unit/threadlog.test.js.

import { connect } from "./sse.js";

/** frameTime renders a frame's RFC 3339 capture time as "HH:MM:SS" ("" when
 * absent or unparseable — a missing time is omitted, never invented). */
export function frameTime(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const pad = (n) => String(n).padStart(2, "0");
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

/** markerLabel renders one structural marker as the text a reader sees. */
export function markerLabel(frame) {
  if (!frame) return "";
  const step = Number(frame.step) || 0;
  switch (frame.kind) {
    case "step_start":
      return `▸ step ${step}: ${frame.name || "(unnamed)"}`;
    case "step_end":
      return `◂ step ${step}: ${frame.conclusion || "done"}${frame.duration_ms ? ` in ${durationText(frame.duration_ms)}` : ""}${frame.exit_code ? ` (exit ${frame.exit_code})` : ""}`;
    case "group_start":
      return `▸ group: ${frame.name || "(unnamed)"}`;
    case "group_end":
      return "◂ group end";
    case "truncated":
      return frame.text || "output truncated";
    default:
      return "";
  }
}

/** durationText renders a millisecond count compactly (marker durations). */
export function durationText(ms) {
  const n = Number(ms) || 0;
  if (n <= 0) return "";
  if (n < 1000) return `${n}ms`;
  const s = Math.round(n / 100) / 10;
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${String(Math.round(s % 60)).padStart(2, "0")}s`;
}

/** sequenceStep reports how a frame advances the dedupe cursor, and whether a
 * gap or an attempt reset must be named. */
export function sequenceStep(frame, lastSeq, attempt, lastAttempt) {
  const seq = Number(frame?.seq) || 0;
  const att = Number(frame?.attempt) || 0;
  const base = { seq: seq > 0 ? seq : lastSeq, attempt: att || lastAttempt };
  if (att > 0 && lastAttempt > 0 && att !== lastAttempt) {
    // A new attempt restarts at seq 1: re-base, never drop.
    return { ...base, action: "attempt" };
  }
  if (seq > 0 && lastSeq > 0 && seq > lastSeq + 1) {
    return { ...base, action: "gap", from: lastSeq + 1, to: seq - 1 };
  }
  if (seq > 0 && seq <= lastSeq) return { ...base, action: "duplicate" };
  return { ...base, action: "append" };
}

/** isTerminal reports the job states no further frame can follow. */
export function isTerminal(state) {
  return state === "success" || state === "failure" || state === "cancelled";
}

/** storedText renders one JSON log window as text (the no-stream fallback and
 * the download-shaped view of the same frames). */
export function storedText(page) {
  if (!page || !Array.isArray(page.frames) || page.frames.length === 0) return "";
  let out = "";
  let last = 0;
  for (const frame of page.frames) {
    const step = sequenceStep(frame, last, page.attempt, 0);
    if (step.action === "gap") out += `[spindle: log frames ${step.from}..${step.to} missing]\n`;
    if (step.action === "duplicate") continue;
    const t = frameTime(frame.time);
    if (frame.kind) {
      out += (t ? `${t} ` : "") + markerLabel(frame) + "\n";
    } else {
      const text = typeof frame.text === "string" ? frame.text : "";
      if (text) out += (t && text.startsWith("\n") ? `${t} ` : "") + text;
    }
    last = step.seq;
  }
  return out.endsWith("\n") || !out ? out : out + "\n";
}

const attached = new WeakSet();

function payload(frame) {
  try {
    return JSON.parse(frame.data);
  } catch {
    return null;
  }
}

/** renderer builds the DOM sequence for one panel. */
function renderer(el, doc = document) {
  const state = {
    last: 0,
    attempt: 0,
    touched: false,
    atLineStart: true,
    bodies: [el],
    groups: [],
  };

  const clearPlaceholder = () => {
    if (state.touched) return;
    el.textContent = "";
    state.touched = true;
  };

  const body = () => state.bodies[state.bodies.length - 1] || el;

  const textPre = () => {
    const b = body();
    const last = b.lastElementChild;
    if (last && last.tagName === "PRE") return last;
    const pre = doc.createElement("pre");
    pre.className = "thread-log-text";
    b.appendChild(pre);
    return pre;
  };

  // startLine guarantees the next block begins at a line boundary; the log's
  // own bytes are never reflowed (a chunk split mid-line stays one line).
  const startLine = () => {
    if (state.atLineStart) return;
    textPre().appendChild(doc.createTextNode("\n"));
    state.atLineStart = true;
  };

  const line = (text, cls) => {
    clearPlaceholder();
    startLine();
    const span = doc.createElement("span");
    span.className = cls || "thread-log-line";
    span.textContent = text;
    const pre = textPre();
    pre.appendChild(span);
    pre.appendChild(doc.createTextNode("\n"));
  };

  const stamp = (pre, t) => {
    if (!t) return;
    const ts = doc.createElement("span");
    ts.className = "thread-log-time";
    ts.textContent = `${t} `;
    pre.appendChild(ts);
  };

  /** text appends captured output verbatim, stamping each non-empty line start
   * with the frame's capture time. A line that continues from the previous
   * frame gets no stamp (the frame does not know when that line began). */
  const appendText = (frame) => {
    const t = frameTime(frame.time);
    const text = typeof frame.text === "string" ? frame.text : "";
    if (!text) return;
    clearPlaceholder();
    const pre = textPre();
    let i = 0;
    while (i < text.length) {
      if (state.atLineStart && text[i] !== "\n") stamp(pre, t);
      const j = text.indexOf("\n", i);
      if (j < 0) {
        pre.appendChild(doc.createTextNode(text.slice(i)));
        state.atLineStart = false;
        return;
      }
      pre.appendChild(doc.createTextNode(text.slice(i, j + 1)));
      state.atLineStart = true;
      i = j + 1;
    }
  };

  const openGroup = (frame) => {
    clearPlaceholder();
    startLine();
    const details = doc.createElement("details");
    details.className = "thread-log-group";
    details.open = true;
    const summary = doc.createElement("summary");
    summary.className = "thread-log-summary";
    summary.textContent = `▸ ${frame.name || "(unnamed)"}`;
    details.appendChild(summary);
    const inner = doc.createElement("div");
    inner.className = "thread-log-body";
    details.appendChild(inner);
    body().appendChild(details);
    state.bodies.push(inner);
    state.groups.push(details);
  };

  const closeGroup = () => {
    const details = state.groups.pop();
    if (!details) {
      line("◂ group end (no open group)", "thread-log-line thread-log-note");
      return;
    }
    if (state.bodies.length > 1) state.bodies.pop();
    details.open = false;
    details.classList.add("thread-log-group-done");
    state.atLineStart = true;
  };

  const apply = (frame) => {
    const step = sequenceStep(frame, state.last, state.attempt, state.attempt);
    if (step.action === "duplicate") return;
    if (step.action === "attempt") {
      line("[spindle: attempt " + step.attempt + "]", "thread-log-line thread-log-note");
      state.last = 0;
      state.attempt = step.attempt;
    } else if (step.action === "gap") {
      line(`[spindle: log frames ${step.from}..${step.to} missing — the runner could not write them]`, "thread-log-line thread-log-note");
    }
    state.last = Math.max(state.last, Number(frame.seq) || state.last);
    if (frame.kind === "group_start") {
      openGroup(frame);
      return;
    }
    if (frame.kind === "group_end") {
      closeGroup();
      return;
    }
    if (frame.kind) {
      line(markerLabel(frame), "thread-log-line thread-log-marker");
      return;
    }
    appendText(frame);
  };

  const finish = (jobState) => {
    // A live group that never closed is collapsed on completion (the runner
    // closes it at step end, but a log that stops mid-group must not swallow
    // the rest of the panel).
    while (state.groups.length > 0) closeGroup();
    if (!state.touched) {
      const note = doc.createElement("p");
      note.className = "thread-log-note";
      note.textContent = jobState ? `(no log output · ${jobState})` : "(no log output)";
      el.appendChild(note);
      state.touched = true;
    }
    el.setAttribute("data-threadlog-state", jobState || "");
    el.removeAttribute("aria-busy");
  };

  return { apply, finish, state };
}

/** attach wires one panel and returns its cancel handle.
 *
 * Replay first, tail second: one bounded JSON window renders the stored frames
 * immediately (a reader never waits on a stream to see history), then — while
 * the job is still live — the SSE stream tails from the last sequence. If the
 * stream cannot be established at all, the panel keeps the replayed log and
 * says so instead of silently spinning.
 */
export function attach(el, open = connect, fetchImpl = globalThis.fetch) {
  const url = el.getAttribute("data-threadlog-url");
  if (!url) return null;
  const view = renderer(el);
  const state = { stopped: false, failures: 0, done: false };
  // aria-busy while the panel is still loading (live tail or replay); every
  // terminal path (done frame, fallback, cancel, replay error) clears it. No-JS
  // pages never set it.
  el.setAttribute("aria-busy", "true");
  let handle = null;

  const stop = () => {
    state.stopped = true;
    el.removeAttribute("aria-busy");
    if (handle) handle.cancel();
  };

  const tail = () => {
    if (state.done || state.stopped) return;
    handle = open({
      // The cursor is read at connect time; a reconnect re-reads the same URL,
      // and the sequence dedupe makes the replay idempotent (no duplication,
      // no loss when the transport drops).
      url: `${url}?after=${view.state.last}&attempt=${view.state.attempt}`,
      events: ["log", "done", "error"],
      retryUnauthorized: true,
      onFrame(frame) {
        if (frame.event === "log") {
          const data = payload(frame);
          if (data) view.apply(data);
          return;
        }
        if (frame.event === "done") {
          const data = payload(frame);
          state.done = true;
          view.finish(data && data.state ? data.state : "");
          stop();
          return;
        }
        if (frame.event === "error") {
          const data = payload(frame);
          state.failures += 1;
          if (state.failures >= 2) {
            el.setAttribute("data-threadlog-fallback", "1");
            if (!view.state.touched) el.textContent = "(no log output yet)";
            stop();
            return;
          }
          const note = data && data.message ? data.message : "";
          view.apply({ kind: "truncated", text: `[stream error${note ? ": " + note : ""}]`, seq: 0, attempt: view.state.attempt });
        }
      },
      onError() {
        state.failures += 1;
        if (state.failures >= 2) {
          el.setAttribute("data-threadlog-fallback", "1");
          if (!view.state.touched) el.textContent = "(log stream unavailable; the stored log is shown)";
          stop();
        }
      },
      onAuthRequired() {
        el.setAttribute("data-threadlog-auth", "expired");
        if (!view.state.touched) {
          el.textContent = "(session expired — sign in again; this log resumes where it stopped)";
          view.state.touched = true;
        }
      },
    });
  };

  const replay = async () => {
    try {
      const res = await fetchImpl(`${url}?accept=json&n=1000`, { credentials: "same-origin", cache: "no-store" });
      if (!res.ok) throw new Error(`log ${res.status}`);
      const page = await res.json();
      for (const frame of page.frames || []) view.apply(frame);
      view.state.attempt = Number(page.attempt) || view.state.attempt;
      el.setAttribute("data-threadlog-state", page.state || "");
      if (!view.state.touched) el.textContent = page.state && isTerminal(page.state) ? `(no log output · ${page.state})` : "(no log output yet)";
      if (isTerminal(page.state)) {
        state.done = true;
        el.removeAttribute("aria-busy");
        return;
      }
    } catch (err) {
      el.textContent = `(log unavailable: ${err && err.message ? err.message : err})`;
      state.done = true;
      el.removeAttribute("aria-busy");
      return;
    }
    tail();
  };
  replay();

  return {
    cancel() {
      state.stopped = true;
      el.removeAttribute("aria-busy");
      if (handle) handle.cancel();
    },
  };
}

export function init(root = document) {
  for (const el of root.querySelectorAll("[data-threadlog]")) {
    if (attached.has(el)) continue;
    attached.add(el);
    el.setAttribute("data-island-ready", "1");
    const stop = attach(el);
    if (!stop) continue;
    el._threadlogStop = stop;
  }
}
