// Runs synchronously in <head>, before first paint: resolve the palette and flag JS support.
// The same resolution order as the app: saved choice, else the OS side (high contrast when the
// OS asks for it), else dark — the shipped default. A static file so the CSP can forbid inline
// scripts. localStorage failures resolve like an absent value.
(function () {
  var root = document.documentElement;
  root.classList.add('js');
  var known = { dark: 1, light: 1, 'hc-dark': 1 };
  var choice = null;
  try { choice = localStorage.getItem('spindle-site-theme'); } catch (e) { /* storage blocked */ }
  if (!known[choice]) {
    choice = 'dark';
    if (window.matchMedia) {
      if (window.matchMedia('(prefers-contrast: more)').matches) choice = 'hc-dark';
      else if (window.matchMedia('(prefers-color-scheme: light)').matches) choice = 'light';
    }
  }
  root.setAttribute('data-theme', choice);
})();
