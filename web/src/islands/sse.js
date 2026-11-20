// sse.js — the one server-sent-events reader (D-WEB-14, island spec §3).
//
// Why not the htmx SSE extension: EventSource cannot POST, cannot read JSON
// payloads, cannot send headers, retries forever on 401 and cannot coalesce a
// frame storm. Our streams are cookie-authenticated fetch streams whose frames
// drive *effects* (refetch a fragment, update a badge), so the transport stays
// here and htmx is used only for those effects (hx-trigger="ui:sse-… from:body").
//
// Pure helpers (parseFrame/backoffMs/coalesce) are headless-testable and covered
// by web/test/unit/sse-island.test.js.

/** parseFrame parses one SSE frame (the text between blank lines). */
export function parseFrame(text) {
  let event = "message";
  let id = "";
  let retry = null;
  const data = [];
  for (const line of String(text).split("\n")) {
    if (!line || line.startsWith(":")) continue;
    const i = line.indexOf(":");
    const field = i < 0 ? line : line.slice(0, i);
    let value = i < 0 ? "" : line.slice(i + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    switch (field) {
      case "event":
        event = value || "message";
        break;
      case "id":
        id = value;
        break;
      case "retry":
        if (/^\d+$/.test(value)) retry = Number(value);
        break;
      case "data":
        data.push(value);
        break;
      default:
        break; // unknown fields are ignored per the SSE spec
    }
  }
  if (data.length === 0 && event === "message") return null;
  return { event, data: data.join("\n"), id, retry };
}

/** backoffMs: 1s → 30s, capped, with light jitter (reconnect storms hurt). */
export function backoffMs(attempt, base = 1000, max = 30000) {
  const raw = Math.min(max, base * 2 ** Math.max(0, attempt));
  return Math.round(raw * (0.8 + Math.random() * 0.2));
}

/** coalesce returns a trailing debounce (last call wins after `ms` idle). */
export function coalesce(fn, ms = 250) {
  let t = null;
  const wrapped = (...args) => {
    if (t) clearTimeout(t);
    t = setTimeout(() => {
      t = null;
      fn(...args);
    }, ms);
  };
  wrapped.flush = () => {
    if (t) {
      clearTimeout(t);
      t = null;
      fn();
    }
  };
  wrapped.cancel = () => {
    if (t) clearTimeout(t);
    t = null;
  };
  return wrapped;
}

/**
 * connect opens one stream and keeps it open until cancel() or the signal
 * aborts. onFrame receives parsed frames; onError receives transport errors.
 * A 401 stops the stream and dispatches `ui:auth-required` on body.
 */
export function connect({ url, signal, onFrame, onError, events, retryBase = 1000, retryMax = 30000 }) {
  let stopped = false;
  let attempt = 0;
  const ctrl = new AbortController();
  if (signal) {
    if (signal.aborted) ctrl.abort();
    else signal.addEventListener("abort", () => ctrl.abort(), { once: true });
  }
  const sleep = (ms) =>
    new Promise((resolve) => {
      if (ctrl.signal.aborted) return resolve();
      const timer = setTimeout(done, ms);
      function done() {
        clearTimeout(timer);
        ctrl.signal.removeEventListener("abort", done);
        resolve();
      }
      ctrl.signal.addEventListener("abort", done, { once: true });
    });

  async function run() {
    while (!stopped && !ctrl.signal.aborted) {
      try {
        const res = await fetch(url, {
          credentials: "same-origin",
          cache: "no-store",
          headers: { Accept: "text/event-stream" },
          signal: ctrl.signal,
        });
        if (res.status === 401) {
          stopped = true;
          document.body.dispatchEvent(new CustomEvent("ui:auth-required"));
          return;
        }
        if (!res.ok || !res.body) {
          // The status rides the error so a caller can tell "unknown id"
          // (404 — nothing to retry) from a transport drop (tasks.js).
          const err = new Error(`stream ${res.status}`);
          err.status = res.status;
          throw err;
        }
        attempt = 0;
        const reader = res.body.getReader();
        const dec = new TextDecoder();
        let buf = "";
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
            if (frame.retry) retryBase = Math.min(frame.retry, retryMax);
            if (events && events.length && !events.includes(frame.event)) continue;
            if (onFrame) onFrame(frame);
          }
        }
        if (stopped || ctrl.signal.aborted) return;
        throw new Error("stream ended");
      } catch (err) {
        if (stopped || ctrl.signal.aborted) return;
        if (onError) onError(err);
        await sleep(backoffMs(attempt++, retryBase, retryMax));
      }
    }
  }
  run();
  return {
    cancel() {
      stopped = true;
      ctrl.abort();
    },
  };
}

// --- DOM wiring ---------------------------------------------------------------

const live = new WeakMap(); // element → { cancel }

function payload(frame) {
  try {
    return JSON.parse(frame.data);
  } catch {
    return frame.data;
  }
}

export function init(root = document) {
  for (const el of root.querySelectorAll("[data-sse]")) {
    if (live.has(el)) continue;
    const url = el.getAttribute("data-sse-url");
    if (!url) continue;
    const events = (el.getAttribute("data-sse-events") || "")
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean);
    const prefix = el.getAttribute("data-sse-dispatch") || "ui:sse";
    const mode = el.getAttribute("data-sse-mode") || "invalidate";
    const retryBase = Number(el.getAttribute("data-sse-retry-base") || 1000);
    const retryMax = Number(el.getAttribute("data-sse-retry-max") || 30000);

    const fire = (name, detail) => {
      // Both spellings are dispatched: `ui:sse:<event>` (contract) and
      // `ui:sse-<event>` (the hx-trigger names pages use, e.g. ui:sse-collab).
      for (const n of [`${prefix}:${name}`, `${prefix}-${name}`]) {
        const ev = new CustomEvent(n, { detail, bubbles: true });
        el.dispatchEvent(ev);
        document.body.dispatchEvent(ev);
      }
    };

    // invalidate: coalesce bursts into one refresh signal.
    const invalidate = coalesce((name, detail) => fire(name, detail), 250);

    const conn = connect({
      url,
      retryBase,
      retryMax,
      events,
      onFrame: (frame) => {
        el.dataset.sseState = "live";
        const detail = { event: frame.event, data: payload(frame) };
        if (mode === "append") {
          const target = el.getAttribute("data-sse-target");
          const list = target ? document.querySelector(target) : null;
          const seq = detail.data && detail.data.seq;
          if (list && seq != null && list.querySelector(`[data-seq="${seq}"]`)) return;
          fire(frame.event, detail);
          return;
        }
        if (mode === "envelope") {
          fire("envelope", detail);
          return;
        }
        invalidate(frame.event, detail);
      },
      onError: () => {
        el.dataset.sseState = "error";
      },
    });
    live.set(el, conn);
  }
}

export function cleanup() {
  for (const el of document.querySelectorAll("[data-sse]")) {
    const conn = live.get(el);
    if (conn) {
      conn.cancel();
      live.delete(el);
    }
  }
}
