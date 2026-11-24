// The whole site script: the palette control and the demo's play policy. No network,
// no storage beyond the palette choice, nothing the content depends on.
(function () {
  var root = document.documentElement;
  var KEY = 'spindle-site-theme';
  var MODES = ['dark', 'light', 'hc-dark'];

  function saved() {
    try {
      var t = localStorage.getItem(KEY);
      return MODES.indexOf(t) >= 0 ? t : null;
    } catch (e) { return null; }
  }

  function system() {
    if (window.matchMedia) {
      if (window.matchMedia('(prefers-contrast: more)').matches) return 'hc-dark';
      if (window.matchMedia('(prefers-color-scheme: light)').matches) return 'light';
    }
    return 'dark';
  }

  function apply(mode) {
    root.setAttribute('data-theme', mode);
    var buttons = document.querySelectorAll('[data-theme-set]');
    for (var i = 0; i < buttons.length; i++) {
      buttons[i].setAttribute('aria-pressed', String(buttons[i].getAttribute('data-theme-set') === mode));
    }
  }

  apply(saved() || system());

  document.addEventListener('click', function (e) {
    var b = e.target.closest && e.target.closest('[data-theme-set]');
    if (!b) return;
    var mode = b.getAttribute('data-theme-set');
    try { localStorage.setItem(KEY, mode); } catch (err) { /* persistence is optional */ }
    apply(mode);
  });

  // The demo autoplays only while it is on screen, and never when the visitor asks for less
  // motion — the poster is the whole content then, and the controls still work.
  var video = document.querySelector('video[data-autoplay]');
  var less = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)');

  function play() {
    if (!video || (less && less.matches)) return;
    video.preload = 'auto';
    var p = video.play();
    if (p && p.catch) p.catch(function () { /* autoplay refused: controls remain */ });
  }

  if (video) {
    if ('IntersectionObserver' in window) {
      new IntersectionObserver(function (entries) {
        entries.forEach(function (entry) {
          if (entry.isIntersecting) play(); else video.pause();
        });
      }, { threshold: 0.3 }).observe(video);
    }
    if (less && less.addEventListener) {
      less.addEventListener('change', function () {
        if (less.matches) video.pause(); else play();
      });
    }
  }

  // Concept clips: the still is the default. A clip loads and animates only after the visitor
  // asks for it, and Pause (or a switch to reduced motion) puts the still back and drops the GIF
  // from the document, so a looping animation is never out of the visitor's control.
  function stopConcept(figure) {
    var button = figure.querySelector('.concept-play');
    var anim = figure.querySelector('.concept-media .anim');
    figure.classList.remove('playing');
    if (anim) anim.removeAttribute('src');
    if (button) {
      button.setAttribute('aria-pressed', 'false');
      button.textContent = 'Play';
      button.setAttribute('aria-label', 'Play ' + button.getAttribute('data-label'));
    }
  }

  document.querySelectorAll('.concept').forEach(function (figure) {
    var button = figure.querySelector('.concept-play');
    if (!button) return;
    button.addEventListener('click', function () {
      if (less && less.matches) return;
      if (figure.classList.contains('playing')) { stopConcept(figure); return; }

      var anim = figure.querySelector('.concept-media .anim');
      if (!anim) {
        anim = document.createElement('img');
        anim.className = 'anim';
        anim.width = 640;
        anim.height = 360;
        anim.decoding = 'async';
        anim.alt = button.getAttribute('data-anim-alt') || '';
        figure.querySelector('.concept-media').insertBefore(anim, button);
      }
      anim.src = button.getAttribute('data-gif');
      figure.classList.add('playing');
      button.setAttribute('aria-pressed', 'true');
      button.textContent = 'Pause';
      button.setAttribute('aria-label', 'Pause ' + button.getAttribute('data-label'));
    });
  });

  if (less && less.addEventListener) {
    less.addEventListener('change', function () {
      if (!less.matches) return;
      document.querySelectorAll('.concept.playing').forEach(stopConcept);
    });
  }

  root.classList.add('concepts-ready');
})();
