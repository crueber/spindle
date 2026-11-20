// tasks.js — background-task progress for a server-started task (the /import
// card). Island spec §2.7 (docs/go/12_web_ui.md).
//
// Markup contract (templates/pages/forms/import.html):
//   <div data-task-id="i…" data-task-stream="/api/v1/repos/imports/i…">
//     <div data-task-bars></div>              ← one row per progress label
//     <pre data-task-log></pre>               ← log tail (replaced wholesale)
//   </div>
//
// `data-task-id` is the **import/service id** — the id the status endpoint
// accepts (repoimport stamps it on the wire; the WAL table's own id stays
// internal). The stream URL and the "check status" link use the same key.
//
// Transport: the shared fetch stream from sse.js, not EventSource —
// EventSource cannot tell a 401 from a drop, retries forever, and gives no
// place to stop at a bounded attempt (docs/go/12_web_ui.md §2.6). It also
// keeps fetch() in one module (the CI grep rule).
//
// Behaviour: paint progress bars + log, and always end honestly — a terminal
// result navigates to the imported repository; a terminal error, a 404
// (pruned past retention), a 401 or an exhausted retry budget renders the
// reason and a real link instead of a spinner (law 7: no silent waiting).
//
// Pure helpers (taskBars/taskDone/taskRedirect/taskFailureText/taskLogTail)
// are DOM-free and covered by web/test/unit/tasks.test.js.

import { connect } from "./sse.js";

/** MAX_RETRIES bounds consecutive transport errors before the card gives up. */
export const MAX_RETRIES = 5;

function num(v) {
  if (v === null || v === undefined || v === "") return null;
  const n = Number(v);
  return Number.isFinite(n) ? n : null;
}

function clamp(v, lo, hi) {
  return Math.min(hi, Math.max(lo, v));
}

/**
 * taskBars maps one `progress` packet to its bar row:
 * {label, text, percent} — percent is null when the total is unknown
 * (indeterminate; rendered as a full dim track, never a fake 0%). A packet
 * without a label falls back to the card's `data-task-labels` text.
 */
export function taskBars(progress, fallbackLabel = "") {
  if (!progress || typeof progress !== "object") return null;
  const label = String(progress.label ?? "").trim();
  const unit = String(progress.unit ?? "").trim();
  const done = num(progress.done);
  const total = num(progress.total);
  const given = num(progress.percent);
  let percent = null;
  if (given !== null) percent = clamp(given, 0, 100);
  else if (done !== null && total !== null && total > 0) percent = clamp((done / total) * 100, 0, 100);
  let text = "";
  if (done !== null) {
    const suffix = unit ? ` ${unit}` : "";
    text = total !== null && total > 0 ? `${done}/${total}${suffix}` : `${done}${suffix}`;
  }
  return { label: label || String(fallbackLabel ?? "").trim() || "progress", text, percent };
}

/**
 * taskDone reads the verdict off a frame:
 *   kind "result" → "ok"; "error" → "failed";
 *   kind "task"   → "ok"/"failed" only once the record carries `finished`.
 * Anything else is still "running" — never guess a terminal state.
 */
export function taskDone(kind, record) {
  if (kind === "result") return "ok";
  if (kind === "error") return "failed";
  // Only a task record carries a verdict; anything else is still running.
  if (kind !== "task") return "running";
  if (!record || typeof record !== "object" || !record.finished) return "running";
  return record.ok === false ? "failed" : "ok";
}

const REPO_PATH = /^[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/;

/**
 * taskRedirect returns the in-app path of the imported repository
 * ("/owner/name"), or "" when the payload names none (never navigate on a
 * guess). A terminal `result` and a finished task record both carry `repo`
 * as "owner/name".
 */
export function taskRedirect(payload) {
  if (!payload || typeof payload !== "object") return "";
  const repo = String(payload.repo ?? "")
    .trim()
    .replace(/^\/+|\/+$/g, "");
  return REPO_PATH.test(repo) ? `/${repo}` : "";
}

/** taskFailureText is the honest one-line reason for a failed import. */
export function taskFailureText(kind, payload) {
  if (kind === "error" && payload && typeof payload === "object") {
    const message = String(payload.message ?? "").trim();
    const status = num(payload.status);
    if (message) return status ? `${message} (${status})` : message;
  }
  if (kind === "task" && payload && typeof payload === "object") {
    const summary = String(payload.summary ?? "").trim();
    if (summary) return summary;
  }
  return "import failed";
}

/** taskLogTail returns the record's log tail as an array of lines. */
export function taskLogTail(record) {
  const tail = record && Array.isArray(record.log_tail) ? record.log_tail : [];
  return tail.map((line) => String(line ?? "")).filter((line) => line !== "");
}

// --- DOM wiring ---------------------------------------------------------------

const live = new Map(); // card element → { cancel }

function node(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

function readJSON(frame) {
  try {
    return JSON.parse(frame.data);
  } catch {
    return null;
  }
}

function barRow(label) {
  const row = node("div", "grid gap-1");
  const head = node("div", "flex flex-wrap items-baseline gap-2 text-sm");
  head.append(node("span", "font-medium", label));
  const value = node("span", "muted mono ml-auto text-[11px]", "");
  head.append(value);
  const track = node("div", "h-1.5 overflow-hidden rounded bg-[var(--pd-raised)]");
  track.setAttribute("role", "progressbar");
  track.setAttribute("aria-label", label);
  track.setAttribute("aria-valuemin", "0");
  track.setAttribute("aria-valuemax", "100");
  const fill = node("div", "h-full bg-[var(--pd-accent)]");
  fill.style.width = "0%";
  track.append(fill);
  row.append(head, track);
  return { row, value, track, fill };
}

function start(card) {
  const id = card.getAttribute("data-task-id") || "";
  const url = card.getAttribute("data-task-stream") || "";
  if (!id || !url) return;
  const barsEl = card.querySelector("[data-task-bars]");
  const logEl = card.querySelector("[data-task-log]");
  const fallbackLabel = card.getAttribute("data-task-labels") || "";
  const rows = new Map();
  let conn = null;
  let finished = false;
  let painted = false;
  let errors = 0;
  let lastLine = "";
  let lastRecord = null;

  const setBar = (view) => {
    if (!barsEl || !view) return;
    if (!painted) {
      // First frame wins over the server's "starting…" placeholder row.
      barsEl.replaceChildren();
      painted = true;
    }
    let ref = rows.get(view.label);
    if (!ref) {
      ref = barRow(view.label);
      rows.set(view.label, ref);
      barsEl.append(ref.row);
    }
    if (view.text) ref.value.textContent = view.text;
    if (view.percent === null) {
      ref.fill.style.width = "100%";
      ref.fill.classList.add("opacity-40");
      ref.track.removeAttribute("aria-valuenow");
    } else {
      const pct = Math.round(view.percent);
      ref.fill.style.width = `${pct}%`;
      ref.fill.classList.remove("opacity-40");
      ref.track.setAttribute("aria-valuenow", String(pct));
    }
  };

  const setLog = (lines) => {
    if (!logEl || !lines || !lines.length) return;
    logEl.textContent = lines.join("\n");
    lastLine = lines[lines.length - 1];
  };

  const appendLine = (text) => {
    const line = String(text ?? "").trim();
    if (!logEl || !line || line === lastLine) return;
    const existing = logEl.textContent || "";
    logEl.textContent = existing ? `${existing}\n${line}` : line;
    lastLine = line;
    logEl.scrollTop = logEl.scrollHeight;
  };

  const stop = () => {
    if (conn) {
      conn.cancel();
      conn = null;
    }
  };

  const note = (kind, message, href) => {
    const box = node("div", "grid gap-2");
    box.setAttribute("role", "status");
    box.append(node("p", kind === "failed" ? "warn-line" : "text-sm font-medium", message));
    if (href) {
      const link = node("a", "link text-sm", href.label);
      link.href = href.url;
      box.append(link);
    }
    if (barsEl) barsEl.replaceChildren(box);
  };

  const settle = (kind, payload) => {
    if (finished) return;
    finished = true;
    stop();
    card.dataset.taskState = "done";
    if (taskDone(kind, payload) === "ok") {
      const target = taskRedirect(payload) || taskRedirect(lastRecord);
      if (target) {
        setBar({ label: "import", text: "complete", percent: 100 });
        note("ok", `import complete — opening ${target}`);
        window.location.assign(target);
        return;
      }
      note("ok", "import complete", { label: "explore repositories", url: "/explore" });
      return;
    }
    note("failed", taskFailureText(kind, payload), { label: "back to the import form", url: "/import" });
    setLog(taskLogTail(lastRecord));
  };

  conn = connect({
    url,
    onFrame: (frame) => {
      errors = 0;
      card.dataset.taskState = "live";
      const payload = readJSON(frame);
      if (frame.event === "progress") {
        setBar(taskBars(payload, fallbackLabel));
        return;
      }
      if (frame.event === "notice") {
        appendLine(payload && typeof payload === "object" ? payload.text : frame.data);
        return;
      }
      if (frame.event === "task") {
        if (payload && typeof payload === "object") {
          lastRecord = payload;
          setLog(taskLogTail(payload));
          if (payload.progress) setBar(taskBars(payload.progress, fallbackLabel));
        }
        return;
      }
      if (frame.event === "result" || frame.event === "error") settle(frame.event, payload);
    },
    onError: (err) => {
      errors += 1;
      card.dataset.taskState = "error";
      if (err && err.status === 404) {
        // Pruned past retention (1h): the ring is gone, the repo is the truth.
        finished = true;
        stop();
        note("failed", "this import is no longer tracked (finished over an hour ago)", {
          label: "look for the repository",
          url: "/explore",
        });
        return;
      }
      if (errors < MAX_RETRIES) return;
      // Out of retries: if the last record already reached a verdict, report
      // it; otherwise say what is true — the task itself is unaffected.
      if (lastRecord && taskDone("task", lastRecord) !== "running") {
        settle("task", lastRecord);
        return;
      }
      finished = true;
      stop();
      note("failed", "lost the progress stream — the import keeps running", {
        label: "check its status",
        url,
      });
    },
  });
  live.set(card, { cancel: stop });
}

export function initTasks(root = document) {
  for (const card of root.querySelectorAll("[data-task-id][data-task-stream]")) {
    if (live.has(card) || card.dataset.islandReady) continue;
    card.dataset.islandReady = "1";
    start(card);
  }
}

// A 401 from the stream stops it (sse.js) and asks the app to re-auth: turn
// that into a sentence on every live card instead of a dead bar.
function onAuthRequired() {
  for (const card of live.keys()) {
    const barsEl = card.querySelector("[data-task-bars]");
    if (!barsEl) continue;
    const box = node("div", "grid gap-2");
    box.setAttribute("role", "status");
    box.append(node("p", "warn-line", "sign in again to follow this import — it keeps running"));
    const link = node("a", "link text-sm", "reload the page");
    link.href = window.location.href;
    box.append(link);
    barsEl.replaceChildren(box);
  }
}

export function cleanup() {
  for (const [card, handle] of live) {
    handle.cancel();
    live.delete(card);
  }
}

if (typeof document !== "undefined" && document.body) {
  document.body.addEventListener("ui:auth-required", onAuthRequired);
  // htmx removes the card before swapping: stop the reader with it (one
  // closer per stream — sse.js §Concurrency).
  document.body.addEventListener("htmx:beforeCleanupElement", (e) => {
    const target = e.detail && e.detail.el;
    if (!target) return;
    const cards = target.matches("[data-task-id]") ? [target] : target.querySelectorAll("[data-task-id]");
    for (const card of cards) {
      const handle = live.get(card);
      if (!handle) continue;
      handle.cancel();
      live.delete(card);
    }
  });
}

if (typeof window !== "undefined") {
  window.addEventListener("pagehide", cleanup);
}
