// ops.js — run a maintenance op and follow its progress (WP9 G12).
//
// Markup contract (partials/repo_wal_ops.html):
//   <section data-ops-panel>
//     <form data-op-run data-op-url="/o/r/api/ops/fsck">
//       <input name="connectivity" …> … <button type=submit>run</button>
//     </form>
//     <pre data-op-log></pre>
//     <div data-op-task></div>
//     <span data-op-state></span>
//   </section>
//
// Transport: POST the existing …/api/ops/{op} endpoint and read the response
// as an SSE fetch stream, reusing sse.js's frame parser. It is deliberately
// NOT the reconnecting reader: a retried POST after a terminal frame would
// start a *new* op, so a drop is reported honestly instead of re-running
// maintenance. No EventSource (it cannot POST), no second transport.
//
// Pure helpers (opQuery/opProgressText/opTaskLines/opResultView) are DOM-free
// and covered by web/test/unit/ops.test.js.

import { parseFrame } from "./sse.js";

/** opQuery builds the query string from [name, value] pairs (blanks skipped). */
export function opQuery(entries) {
  const params = new URLSearchParams();
  for (const [name, value] of entries) {
    if (!name) continue;
    const text = String(value ?? "").trim();
    if (text === "") continue;
    params.append(name, text);
  }
  return params.toString();
}

function payload(frame) {
  try {
    return JSON.parse(frame.data);
  } catch {
    return frame.data;
  }
}

/** opProgressText renders one progress packet as a bounded log line. */
export function opProgressText(progress, fallbackLabel = "") {
  if (!progress || typeof progress !== "object") return "";
  const label = String(progress.label || fallbackLabel || "").trim();
  const unit = String(progress.unit || "").trim();
  const done = progress.done;
  const total = progress.total;
  const percent = progress.percent;
  let detail = "";
  if (done !== undefined && done !== null) {
    detail = total !== undefined && total !== null ? `${done}/${total}${unit ? " " + unit : ""}` : `${done}${unit ? " " + unit : ""}`;
  } else if (percent !== undefined && percent !== null) {
    detail = `${percent}%`;
  }
  return [label || "progress", detail].filter(Boolean).join(" — ");
}

/** opTaskLines renders a TaskRecord's status and bounded log tail. */
export function opTaskLines(record) {
  if (!record || typeof record !== "object") return { title: "", lines: [] };
  const kind = String(record.kind || "op").trim();
  const state = record.finished ? (record.ok === false ? "failed" : "ok") : "running";
  const title = `${kind} — ${state}`;
  const lines = [];
  if (record.summary) lines.push(String(record.summary));
  if (record.hostname) lines.push(`host: ${record.hostname}`);
  const tail = Array.isArray(record.log_tail) ? record.log_tail : [];
  for (const line of tail) {
    if (line !== undefined && line !== null && String(line) !== "") lines.push(String(line));
  }
  return { title, lines, state };
}

/** opResultView reads the terminal verdict off a result/error frame. */
export function opResultView(kind, body) {
  if (kind === "error") {
    const message = body && typeof body === "object" ? String(body.message || "").trim() : "";
    const status = body && typeof body === "object" && body.status ? ` (${body.status})` : "";
    return { state: "failed", text: (message || "the op failed") + status };
  }
  const record = body && typeof body === "object" ? body.task : null;
  if (record && typeof record === "object") {
    return { state: record.ok === false ? "failed" : "ok", text: String(record.summary || "op finished") };
  }
  return { state: "ok", text: "op finished" };
}

// --- DOM wiring ---------------------------------------------------------------

const live = new Map(); // panel → AbortController

function node(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

function part(panel, attr) {
  return panel ? panel.querySelector(`[${attr}]`) : null;
}

function appendLine(log, text, state) {
  const line = String(text ?? "").trim();
  if (!log || !line) return;
  log.textContent = log.textContent ? `${log.textContent}\n${line}` : line;
  log.scrollTop = log.scrollHeight;
}

function renderTask(box, record) {
  if (!box) return;
  const view = opTaskLines(record);
  box.replaceChildren();
  if (!view.title) return;
  box.append(node("p", "text-sm font-medium", view.title));
  if (view.lines.length) {
    const pre = node("pre", "code-view max-h-40 overflow-y-auto p-2 text-xs", view.lines.join("\n"));
    box.append(pre);
  }
}

function setState(panel, state) {
  const chip = part(panel, "data-op-state");
  if (!chip) return;
  chip.hidden = false;
  chip.textContent = state;
  chip.className = state === "failed" ? "chip-closed" : state === "ok" ? "chip-open" : "chip-draft";
}

function begin(form) {
  const panel = form.closest("[data-ops-panel]");
  const url = form.getAttribute("data-op-url") || "";
  if (!panel || !url) return;

  const log = part(panel, "data-op-log");
  const task = part(panel, "data-op-task");
  const previous = live.get(panel);
  if (previous) previous.abort();

  const ctrl = new AbortController();
  live.set(panel, ctrl);
  const qs = opQuery(new FormData(form).entries());
  const target = `${url}${qs ? "?" + qs : ""}`;
  appendLine(log, `→ ${form.querySelector("code")?.textContent || "op"}${qs ? "?" + qs : ""}`);
  setState(panel, "running");

  (async () => {
    let res;
    try {
      res = await fetch(target, {
        method: "POST",
        credentials: "same-origin",
        cache: "no-store",
        headers: { Accept: "text/event-stream" },
        signal: ctrl.signal,
      });
    } catch (err) {
      if (!ctrl.signal.aborted) {
        appendLine(log, `the request failed: ${err.message}`);
        setState(panel, "failed");
      }
      return;
    }
    if (res.status === 401) {
      document.body.dispatchEvent(new CustomEvent("ui:auth-required"));
      setState(panel, "failed");
      return;
    }
    if (!res.ok || !res.body) {
      const text = await res.text().catch(() => "");
      appendLine(log, `the op was refused (${res.status})${text ? ": " + text.trim() : ""}`);
      setState(panel, "failed");
      return;
    }

    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    let terminal = false;
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let i;
      while ((i = buf.indexOf("\n\n")) >= 0) {
        const raw = buf.slice(0, i);
        buf = buf.slice(i + 2);
        const frame = parseFrame(raw);
        if (!frame) continue;
        const body = payload(frame);
        if (frame.event === "notice") {
          appendLine(log, body && typeof body === "object" ? body.text : frame.data);
        } else if (frame.event === "progress") {
          appendLine(log, opProgressText(body));
        } else if (frame.event === "task") {
          renderTask(task, body);
        } else if (frame.event === "result" || frame.event === "error") {
          terminal = true;
          const view = opResultView(frame.event, body);
          const record = body && typeof body === "object" ? body.task : null;
          if (record) renderTask(task, record);
          appendLine(log, view.text);
          setState(panel, view.state);
        }
      }
    }
    if (!terminal && !ctrl.signal.aborted) {
      appendLine(log, "the progress stream ended before a verdict — check recent ops below");
      setState(panel, "failed");
    }
    if (live.get(panel) === ctrl) live.delete(panel);
  })();
}

export function initOps(root = document) {
  for (const form of root.querySelectorAll("[data-op-run]")) {
    if (form.dataset.islandReady) continue;
    form.dataset.islandReady = "1";
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      begin(form);
    });
  }
  for (const button of root.querySelectorAll("[data-op-clear]")) {
    if (button.dataset.islandReady) continue;
    button.dataset.islandReady = "1";
    button.addEventListener("click", () => {
      const panel = button.closest("[data-ops-panel]");
      const log = part(panel, "data-op-log");
      const task = part(panel, "data-op-task");
      const chip = part(panel, "data-op-state");
      if (log) log.textContent = "";
      if (task) task.replaceChildren();
      if (chip) {
        chip.hidden = true;
        chip.textContent = "";
      }
    });
  }
}

export function cleanup() {
  for (const ctrl of live.values()) ctrl.abort();
  live.clear();
}

if (typeof window !== "undefined") {
  window.addEventListener("pagehide", cleanup);
}
