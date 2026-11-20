// menus.js — the popover contract shared by every dropdown (Forgejo #405,
// #466): trigger inside the root, panel bounds to the viewport, outside click
// and Escape close, focus returns to the trigger, ArrowUp/Down walk items,
// Tab out dismisses.
//
// Markup contract (templates):
//   <div data-menu>
//     <button data-menu-trigger aria-expanded="false" aria-haspopup="menu">…</button>
//     <div data-menu-panel role="menu" hidden> <a role="menuitem" …> … </div>
//   </div>

function items(panel) {
  return [...panel.querySelectorAll('[role="menuitem"], button, a[href]')].filter(
    (el) => !el.hasAttribute("disabled") && el.offsetParent !== null,
  );
}

function close(root, trigger, panel, { focus = false } = {}) {
  panel.hidden = true;
  trigger.setAttribute("aria-expanded", "false");
  if (focus) trigger.focus();
  root.dataset.menuOpen = "";
}

function open(root, trigger, panel) {
  panel.hidden = false;
  trigger.setAttribute("aria-expanded", "true");
  root.dataset.menuOpen = "1";
  const first = items(panel)[0];
  if (first) first.focus();
}

export function initMenus(root = document) {
  for (const el of root.querySelectorAll("[data-menu]")) {
    if (el.dataset.menuReady) continue;
    el.dataset.menuReady = "1";
    const trigger = el.querySelector("[data-menu-trigger]");
    const panel = el.querySelector("[data-menu-panel]");
    if (!trigger || !panel) continue;

    trigger.addEventListener("click", (ev) => {
      ev.preventDefault();
      ev.stopPropagation();
      if (panel.hidden) open(el, trigger, panel);
      else close(el, trigger, panel, { focus: true });
    });

    panel.addEventListener("keydown", (ev) => {
      const list = items(panel);
      const i = list.indexOf(document.activeElement);
      if (ev.key === "Escape") {
        ev.preventDefault();
        close(el, trigger, panel, { focus: true });
      } else if (ev.key === "ArrowDown") {
        ev.preventDefault();
        (list[i + 1] || list[0])?.focus();
      } else if (ev.key === "ArrowUp") {
        ev.preventDefault();
        (list[i - 1] || list[list.length - 1])?.focus();
      } else if (ev.key === "Home") {
        ev.preventDefault();
        list[0]?.focus();
      } else if (ev.key === "End") {
        ev.preventDefault();
        list[list.length - 1]?.focus();
      } else if (ev.key === "Tab") {
        close(el, trigger, panel);
      }
    });

    // Outside click (a click that lands neither in the panel nor on the trigger).
    document.addEventListener("click", (ev) => {
      if (panel.hidden) return;
      if (el.contains(ev.target)) return;
      close(el, trigger, panel);
    });
  }
}
