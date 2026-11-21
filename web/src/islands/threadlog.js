// threadlog.js — live job log panels (docs/features/15_threads.md §6).
//
// A page carries one <pre data-threadlog data-threadlog-url> per job. This
// island attaches to the run's log stream (`log` frames carry {seq,text}, the
// terminal `done` frame carries the job state) over the SAME fetch-stream
// transport as sse.js — no EventSource, no new transport — appends the text in
// sequence order (a retried frame is idempotent by seq, so the panel dedupes),
// and stops when the server says done.
//
// If the stream cannot be established (a proxy that buffers streams, an old
// runtime, a transport bug), the island falls back ONCE to the JSON window at
// the same URL (`?accept=json`) and renders the stored frames with an explicit
// note: a reader is never left staring at "loading log…" (law 7).
//
// The pure helpers are headless-testable and covered by
// web/test/unit/threadlog.test.js.

import { connect } from "./sse.js";

/** formatFrame renders one log frame as text ("" for a duplicate sequence). */
export function formatFrame(frame, lastSeq) {
  if (!frame) return "";
  const seq = Number(frame.seq) || 0;
  if (seq > 0 && seq <= lastSeq) return "";
  const text = typeof frame.text === "string" ? frame.text : "";
  return text.endsWith("\n") ? text : text + "\n";
}

/** nextSeq is the highest sequence seen (frames are append-only). */
export function nextSeq(frame, lastSeq) {
  const seq = Number(frame?.seq) || 0;
  return seq > lastSeq ? seq : lastSeq;
}

/** isTerminal reports the job states no further frame can follow. */
export function isTerminal(state) {
  return state === "success" || state === "failure" || state === "cancelled";
}

/** storedText renders one JSON log window (the fallback path). */
export function storedText(page) {
  if (!page || !Array.isArray(page.frames) || page.frames.length === 0) return "";
  let out = "";
  let last = 0;
  for (const frame of page.frames) {
    out += formatFrame(frame, last);
    last = nextSeq(frame, last);
  }
  return out;
}

const attached = new WeakSet();

function payload(frame) {
  try {
    return JSON.parse(frame.data);
  } catch {
    return null;
  }
}

/** append writes text into the panel, replacing the placeholder once. */
function append(el, text, state) {
  if (!text) return;
  if (state.touched === false) {
    el.textContent = "";
    state.touched = true;
  }
  el.appendChild(document.createTextNode(text));
  el.scrollTop = el.scrollHeight;
}

/** attach wires one <pre> panel and returns its cancel handle.
 *
 * Replay first, tail second: one bounded JSON window renders the stored frames
 * immediately (a reader never waits on a stream to see history), then — while
 * the job is still live — the SSE stream tails from the last sequence. If the
 * stream cannot be established at all, the panel keeps the replayed log and says
 * so instead of silently spinning.
 */
export function attach(el, open = connect, fetchImpl = globalThis.fetch) {
  const url = el.getAttribute("data-threadlog-url");
  if (!url) return null;
  const state = { touched: false, last: 0, stopped: false, failures: 0, live: false };
  let handle = null;
  let done = false;

  const stop = () => {
    state.stopped = true;
    if (handle) handle.cancel();
  };

  const tail = () => {
    if (done || state.stopped) return;
    handle = open({
      url: `${url}?after=${state.last}`,
      events: ["log", "done", "error"],
      onFrame(frame) {
        if (frame.event === "log") {
          const data = payload(frame);
          const text = formatFrame(data, state.last);
          state.last = nextSeq(data, state.last);
          append(el, text, state);
          return;
        }
        if (frame.event === "done") {
          const data = payload(frame);
          const jobState = data && data.state ? data.state : "";
          if (!state.touched) el.textContent = jobState ? `(no log output · ${jobState})` : "(no log output)";
          el.setAttribute("data-threadlog-state", jobState);
          done = true;
          stop();
          return;
        }
        if (frame.event === "error") {
          const data = payload(frame);
          state.failures += 1;
          if (state.failures >= 2) {
            el.setAttribute("data-threadlog-fallback", "1");
            el.setAttribute("data-threadlog-state", "stored");
            if (!state.touched) el.textContent = "(no log output yet)";
            stop();
            return;
          }
          append(el, `\n[stream error${data && data.message ? ": " + data.message : ""}]\n`, state);
        }
      },
      onError() {
        state.failures += 1;
        if (state.failures >= 2) {
          el.setAttribute("data-threadlog-fallback", "1");
          if (!state.touched) el.textContent = "(log stream unavailable; the stored log is shown)";
          stop();
        }
      },
    });
  };

  const replay = async () => {
    try {
      const res = await fetchImpl(`${url}?accept=json&n=500`, { credentials: "same-origin", cache: "no-store" });
      if (!res.ok) throw new Error(`log ${res.status}`);
      const page = await res.json();
      const text = storedText(page);
      append(el, text, state);
      state.last = nextSeq({ seq: page.frames && page.frames.length ? page.frames[page.frames.length - 1].seq : 0 }, state.last);
      el.setAttribute("data-threadlog-state", page.state || "");
      if (!state.touched) el.textContent = page.state && isTerminal(page.state) ? `(no log output · ${page.state})` : "(no log output yet)";
      if (isTerminal(page.state)) {
        done = true;
        return;
      }
    } catch (err) {
      el.textContent = `(log unavailable: ${err && err.message ? err.message : err})`;
      done = true;
      return;
    }
    tail();
  };
  replay();

  return {
    cancel() {
      state.stopped = true;
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
