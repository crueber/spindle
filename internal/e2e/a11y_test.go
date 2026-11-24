package e2e

// a11y_test.go — the rendered accessibility smoke (docs/go/15_testing.md §6.6).
//
// The floor AGENTS.md §2 names (skip link, one h1, named controls, keyboard
// focus, no horizontal pan at ~390 px, a clean console) is a property of the
// RENDERED page: a DOM dump from httptest cannot see a CSS rule that hides a
// control, a focus ring that never paints, or a layout that pans. So this test
// boots the real binary, seeds a repository with an issue, a pull request and a
// release, signs in as an admin through the in-test OIDC provider, and drives
// headless Chromium over CDP across a fixed page set.
//
// Tier: its own (`make a11y`), because it needs a browser. Without one it
// SKIPS locally; with SPINDLE_A11Y=1 (CI) a missing or broken browser is a
// FAILURE — a skipped gate is not a gate.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// a11yViewport is one viewport the page set is checked at. The narrow one is
// AGENTS.md §2's ~390 px mobile check; the desktop one catches controls that are
// hidden (and therefore unnamed) only at small widths.
type a11yViewport struct {
	name   string
	width  int
	height int
	mobile bool
}

var a11yViewports = []a11yViewport{
	{name: "390px", width: 390, height: 844, mobile: true},
	{name: "desktop", width: 1280, height: 900, mobile: false},
}

// a11yReport is the probe's answer for one rendered page.
type a11yReport struct {
	URL            string `json:"url"`
	FirstFocusable string `json:"firstFocusable"`
	FirstIsSkip    bool   `json:"firstIsSkip"`
	SkipHref       string `json:"skipHref"`
	SkipTargetTag  string `json:"skipTargetTag"`
	H1             []struct {
		Text    string `json:"text"`
		Visible bool   `json:"visible"`
	} `json:"h1"`
	Unnamed          []a11yElement `json:"unnamed"`
	PositiveTabindex []a11yElement `json:"positiveTabindex"`
	HiddenFocusable  []a11yElement `json:"hiddenFocusable"`
	InteractiveCount int           `json:"interactiveCount"`
	ScrollWidth      int           `json:"scrollWidth"`
	ClientWidth      int           `json:"clientWidth"`
}

// a11yElement names one offending control: a CSS-path selector and a snippet of
// its markup, so a failure says which control on which page.
type a11yElement struct {
	Selector string `json:"selector"`
	HTML     string `json:"html"`
}

func (e a11yElement) String() string {
	return fmt.Sprintf("%s  (%s)", e.Selector, e.HTML)
}

// a11yProbeJS is the whole page check, evaluated once per page. It returns the
// facts; the Go side decides what is a violation, so the selectors and the rules
// stay readable and the failure text is built in one place.
//
// The hidden-control rule: a control that is not rendered but that keyboard
// focus can still land on is invisible focus — the hazard behind the "nothing
// focusable behind display:none" rule. display:none itself takes a control out
// of the tab order entirely (focus() is a no-op), which is why the probe asks the
// browser with focus() instead of pattern-matching CSS.
const a11yProbeJS = `(() => {
  const SEL = 'a[href],area[href],button,input,select,textarea,summary,[tabindex],[contenteditable="true"],[role="button"],[role="link"],[role="menuitem"],[role="tab"],[role="checkbox"]';
  const LABELABLE = {INPUT: 1, SELECT: 1, TEXTAREA: 1, METER: 1, OUTPUT: 1, PROGRESS: 1};
  function path(el) {
    const parts = [];
    let n = el;
    while (n && n.nodeType === 1 && parts.length < 8) {
      let s = n.tagName.toLowerCase();
      if (n.id) { s += '#' + n.id; parts.unshift(s); break; }
      const cls = (typeof n.className === 'string' ? n.className : '').trim().split(/\s+/).filter(Boolean).slice(0, 2);
      if (cls.length) s += '.' + cls.join('.');
      const p = n.parentElement;
      if (p) {
        const same = Array.prototype.filter.call(p.children, function (c) { return c.tagName === n.tagName; });
        if (same.length > 1) s += ':nth-of-type(' + (same.indexOf(n) + 1) + ')';
      }
      parts.unshift(s);
      n = n.parentElement;
    }
    return parts.join(' > ');
  }
  function describe(el) {
    return {selector: path(el), html: (el.outerHTML || '').replace(/\s+/g, ' ').slice(0, 200)};
  }
  function visible(el) {
    const cs = getComputedStyle(el);
    if (cs.display === 'none' || cs.visibility === 'hidden') return false;
    return el.getClientRects().length > 0;
  }
  function tabbable(el) {
    if (el.disabled) return false;
    if (el.tagName === 'INPUT' && el.type === 'hidden') return false;
    const ti = el.getAttribute('tabindex');
    if (ti !== null && Number(ti) < 0) return false;
    return true;
  }
  function accName(el) {
    const al = el.getAttribute('aria-label');
    if (al && al.trim()) return al.trim();
    const lb = el.getAttribute('aria-labelledby');
    if (lb) {
      const t = lb.split(/\s+/).map(function (id) { const r = document.getElementById(id); return r ? r.textContent : ''; })
        .join(' ').replace(/\s+/g, ' ').trim();
      if (t) return t;
    }
    const tag = el.tagName;
    if (tag === 'INPUT' && (el.type === 'submit' || el.type === 'reset' || el.type === 'button')) {
      if (el.value && el.value.trim()) return el.value.trim();
    }
    if (tag === 'INPUT' && el.type === 'image' && el.alt && el.alt.trim()) return el.alt.trim();
    if (LABELABLE[tag]) {
      const labels = el.labels ? Array.prototype.slice.call(el.labels) : [];
      const t = labels.map(function (l) { return l.textContent; }).join(' ').replace(/\s+/g, ' ').trim();
      if (t) return t;
      if (el.id) {
        const l = document.querySelector('label[for="' + CSS.escape(el.id) + '"]');
        if (l && l.textContent.trim()) return l.textContent.replace(/\s+/g, ' ').trim();
      }
    }
    const text = (el.textContent || '').replace(/\s+/g, ' ').trim();
    if (text) return text;
    const imgs = Array.prototype.map.call(el.querySelectorAll('img[alt]'), function (i) { return i.alt; })
      .join(' ').replace(/\s+/g, ' ').trim();
    if (imgs) return imgs;
    const title = el.getAttribute('title');
    if (title && title.trim()) return title.trim();
    return '';
  }
  const all = Array.prototype.slice.call(document.querySelectorAll(SEL));
  const focusables = all.filter(tabbable);
  const shown = focusables.filter(visible);
  const unnamed = shown.filter(function (el) { return !accName(el); }).map(describe);
  const positive = all.filter(function (el) {
    const v = el.getAttribute('tabindex');
    return v !== null && Number(v) > 0;
  }).map(describe);
  const hidden = [];
  focusables.forEach(function (el) {
    if (visible(el)) return;
    // display:none (and visibility:hidden) cannot take focus at all, so the
    // honest assertion is the stronger one: a control keyboard focus CAN land
    // on must be rendered. Ask the browser (focus() is a no-op for a display:none
    // subtree); a CSS collapse that leaves a control tabbable — opacity 0, clip,
    // zero size, parked off-screen — lands and fails.
    const prev = document.activeElement;
    let landed = false;
    try { el.focus({preventScroll: true}); landed = document.activeElement === el; } catch (e) { landed = false; }
    if (prev && prev !== el && prev.focus) { try { prev.focus({preventScroll: true}); } catch (e) {} }
    if (landed) hidden.push(describe(el));
  });
  const skip = document.querySelector('a.skip-link[href="#app"]');
  const main = document.getElementById('app');
  const first = shown.length ? shown[0] : null;
  return {
    url: location.href,
    firstFocusable: first ? path(first) : '',
    firstIsSkip: !!skip && first === skip,
    skipHref: skip ? skip.getAttribute('href') : '',
    skipTargetTag: main ? main.tagName.toLowerCase() : '',
    h1: Array.prototype.map.call(document.querySelectorAll('h1'), function (h) {
      return {text: (h.textContent || '').replace(/\s+/g, ' ').trim().slice(0, 120), visible: visible(h)};
    }),
    unnamed: unnamed,
    positiveTabindex: positive,
    hiddenFocusable: hidden,
    interactiveCount: shown.length,
    scrollWidth: document.documentElement.scrollWidth,
    clientWidth: document.documentElement.clientWidth
  };
})()`

// a11yFocusJS reports where focus landed (used after one real Tab) and whether
// the browser considers it keyboard-visible with a painted outline.
const a11yFocusJS = `(() => {
  const a = document.activeElement;
  const cs = a ? getComputedStyle(a) : null;
  const cls = a && typeof a.className === 'string' ? a.className : '';
  return {
    selector: a ? a.tagName.toLowerCase() + (cls ? '.' + cls.trim().split(/\s+/).join('.') : '') : '',
    isSkip: /(^|\s)skip-link(\s|$)/.test(cls),
    focusVisible: !!(a && a.matches && a.matches(':focus-visible')),
    outlineStyle: cs ? cs.outlineStyle : '',
    outlineWidth: cs ? cs.outlineWidth : ''
  };
})()`

// a11yLoginJS submits the stub IdP's sign-in form as alice (a member of the
// configured admin group). The defaults the form renders are already alice +
// spindle-admin; setting them makes the test independent of those defaults.
const a11yLoginJS = `(() => {
  const f = document.querySelector('form');
  if (!f) return 'no-form';
  const email = f.querySelector('input[name="email"]');
  const groups = f.querySelector('input[name="groups"]');
  if (email) email.value = 'alice@example.com';
  if (groups) groups.value = 'spindle-admin';
  if (f.requestSubmit) f.requestSubmit(); else f.submit();
  return 'submitted';
})()`

// TestA11yPreflight is the CI guard: it runs only when SPINDLE_A11Y=1 and fails
// (never skips) when the browser is missing or will not start. Locally the smoke
// is opt-in and skips quietly, which is why CI states the requirement explicitly.
func TestA11yPreflight(t *testing.T) {
	if testing.Short() {
		t.Skip("a11y preflight runs in its own tier (make a11y)")
	}
	if os.Getenv("SPINDLE_A11Y") != "1" {
		t.Skip("a11y smoke is opt-in locally: make a11y; CI sets SPINDLE_A11Y=1")
	}
	bin, err := findChromium()
	if err != nil {
		t.Fatalf("SPINDLE_A11Y=1 requires a browser: %v", err)
	}
	b := launchBrowser(t) // fatal in CI if it cannot start
	b.connect()
	t.Logf("a11y browser: %s (DevTools %s)", bin, b.devtools)
}

// TestA11yRenderedPages is the smoke itself: the fixed page set, at both
// viewports, over a real instance reached through the canonical host.
func TestA11yRenderedPages(t *testing.T) {
	if testing.Short() {
		t.Skip("rendered a11y smoke runs in its own tier (make a11y)")
	}
	requireModernGit(t)
	requireBrowser(t)

	g := newGitClient(t)
	s, _, adminTok := a11yServer(t)
	owner := testOwner
	repo := uniqueRepo("a11y")
	repoLane := s.base + "/" + owner + "/" + repo + "/api"

	// ---- fixtures: code, an issue, a pull request, a release ----------------
	work, _ := pushFreshRepo(t, g, s, repo, "README.md", "# a11y fixture\n\nSeeded for the rendered accessibility smoke.\n")
	g.run(work, "checkout", "-b", "feature")
	g.commitFile(work, "feature.txt", "the feature\n", "feature commit")
	g.run(work, "push", "origin", "feature")

	issue := mustAPI(t, "POST", repoLane+"/issues",
		`{"title":"A11y fixture issue","body":"Rendered by the accessibility smoke."}`, adminTok)
	issueNum := int(jget(t, issue, "thread", "num").(float64))
	pr := mustAPI(t, "POST", repoLane+"/pulls",
		`{"title":"A11y fixture pull","base_ref":"refs/heads/main","head_ref":"refs/heads/feature","body":"fixes #`+strconv.Itoa(issueNum)+`"}`,
		adminTok)
	prNum := int(jget(t, pr, "thread", "num").(float64))
	// A release names an existing tag, so tag main (not the feature branch).
	g.run(work, "tag", "v1", "main")
	g.run(work, "push", "origin", "refs/tags/v1")
	mustAPI(t, "PUT", repoLane+"/releases/v1", `{"name":"v1","body":"first release"}`, adminTok)

	// ---- browser: sign in as an admin through the stub IdP -------------------
	base := fmt.Sprintf("http://walgit.localhost:%d", s.port)
	b := launchBrowser(t)
	b.connect()
	b.setViewport(1280, 900, false)
	b.navigate(base + "/_auth/login?next=%2Fadmin")
	b.waitFor(20*time.Second, "the IdP sign-in form", `!!document.querySelector('form input[name="email"]')`)
	if err := b.evaluate(a11yLoginJS, nil); err != nil {
		t.Fatalf("submit the IdP sign-in form: %v", err)
	}
	b.waitFor(30*time.Second, "the admin page after sign-in", fmt.Sprintf(
		`location.origin === %s && location.pathname === "/admin" && document.readyState === "complete"`, strconv.Quote(base)))
	var h1 struct {
		Text string `json:"text"`
	}
	if err := b.evaluate(`({text: (document.querySelector('h1') || {}).textContent || ''})`, &h1); err != nil {
		t.Fatalf("read the admin h1: %v", err)
	}
	if !strings.Contains(h1.Text, "Administration") {
		t.Fatalf("after sign-in /admin h1 = %q, want the Administration page (is the session admin?)", strings.TrimSpace(h1.Text))
	}

	// ---- the fixed page set -------------------------------------------------
	pages := []struct{ name, path string }{
		{"landing", "/"},
		{"repo code", "/" + owner + "/" + repo},
		{"issue", fmt.Sprintf("/%s/%s/issues/%d", owner, repo, issueNum)},
		{"pull request", fmt.Sprintf("/%s/%s/pull/%d", owner, repo, prNum)},
		{"releases", "/" + owner + "/" + repo + "/releases"},
		{"admin", "/admin"},
		{"admin config", "/admin/config"},
	}
	for _, page := range pages {
		for _, vp := range a11yViewports {
			label := fmt.Sprintf("%s at %s", page.name, vp.name)
			b.console = nil
			b.setViewport(vp.width, vp.height, vp.mobile)
			b.navigate(base + page.path)

			var r a11yReport
			if err := b.evaluate(a11yProbeJS, &r); err != nil {
				t.Fatalf("%s: probe: %v", label, err)
			}
			var problems []string
			if want := base + page.path; r.URL != want {
				problems = append(problems, fmt.Sprintf("rendered URL = %s, want %s (a gate or redirect changed the page)", r.URL, want))
			}
			if b.docStatus != 200 {
				problems = append(problems, fmt.Sprintf("document status = %d for %s, want 200", b.docStatus, b.docURL))
			}
			problems = append(problems, a11yProblems(r, vp)...)
			for _, entry := range b.console {
				if a11yAllowedConsoleError(entry) {
					continue
				}
				problems = append(problems, "console: "+entry)
			}
			if len(problems) > 0 {
				t.Fatalf("%s (%s):\n  %s", label, r.URL, strings.Join(problems, "\n  "))
			}

			// Keyboard: the first Tab lands on the skip link, which is
			// keyboard-visible and paints an outline; Enter jumps to <main>.
			var f struct {
				Selector     string `json:"selector"`
				IsSkip       bool   `json:"isSkip"`
				FocusVisible bool   `json:"focusVisible"`
				OutlineStyle string `json:"outlineStyle"`
				OutlineWidth string `json:"outlineWidth"`
			}
			b.key("Tab", "Tab", 9)
			if err := b.evaluate(a11yFocusJS, &f); err != nil {
				t.Fatalf("%s: read focus: %v", label, err)
			}
			if !f.IsSkip {
				t.Fatalf("%s: first Tab focused %q, want the skip link", label, f.Selector)
			}
			if !f.FocusVisible {
				t.Fatalf("%s: the skip link does not match :focus-visible after a real Tab", label)
			}
			if f.OutlineStyle == "none" || f.OutlineWidth == "0px" {
				t.Fatalf("%s: the skip link's focus ring is invisible (outline %s %s)", label, f.OutlineStyle, f.OutlineWidth)
			}
			b.key("Enter", "Enter", 13)
			b.waitFor(5*time.Second, "the skip link to reach #app "+label, `location.hash === "#app"`)
		}
	}
}

// a11yProblems turns one probe report into violations, each naming the page and
// the offending selector.
func a11yProblems(r a11yReport, vp a11yViewport) []string {
	var out []string
	if r.SkipHref != "#app" {
		out = append(out, fmt.Sprintf("skip link href = %q, want #app", r.SkipHref))
	}
	if r.SkipTargetTag != "main" {
		out = append(out, fmt.Sprintf("skip link target #app is <%s>, want <main>", r.SkipTargetTag))
	}
	if !r.FirstIsSkip {
		out = append(out, fmt.Sprintf("first focusable element is %q, want the skip link", r.FirstFocusable))
	}
	switch len(r.H1) {
	case 1:
		if !r.H1[0].Visible {
			out = append(out, "the page's single h1 is not rendered")
		}
	case 0:
		out = append(out, "page has no h1")
	default:
		var texts []string
		for _, h := range r.H1 {
			texts = append(texts, strconv.Quote(h.Text))
		}
		out = append(out, fmt.Sprintf("page has %d h1 elements, want exactly one: %s", len(r.H1), strings.Join(texts, ", ")))
	}
	if r.InteractiveCount == 0 {
		out = append(out, "probe found no visible interactive controls (selectors or renderer changed?)")
	}
	for _, el := range r.Unnamed {
		out = append(out, "control with no accessible name: "+el.String())
	}
	for _, el := range r.PositiveTabindex {
		out = append(out, "positive tabindex puts document order out of the tab order: "+el.String())
	}
	for _, el := range r.HiddenFocusable {
		out = append(out, "keyboard focus lands on a control that is not rendered: "+el.String())
	}
	if vp.mobile && r.ScrollWidth != r.ClientWidth {
		out = append(out, fmt.Sprintf("page pans horizontally at %dpx: scrollWidth %d, clientWidth %d", vp.width, r.ScrollWidth, r.ClientWidth))
	}
	return out
}

// a11yAllowedConsoleError is the allow-list, matched by exact text and URL. It
// holds ONE entry: the engine's own 404 for /favicon.ico, which the browser
// requests on every page and the app does not ship, so the message is not page
// code. Anything else — including another network 4xx — fails the page.
func a11yAllowedConsoleError(entry string) bool {
	return strings.HasPrefix(entry, "log[network] ") &&
		strings.Contains(entry, "the server responded with a status of 404") &&
		strings.Contains(entry, "/favicon.ico")
}

// a11yServer boots the real binary with a complete OIDC config pointed at the
// in-test IdP, on the canonical host the browser drives, and returns the admin
// bearer the fixture API calls use.
func a11yServer(t *testing.T) (*e2eServer, *oidp, string) {
	t.Helper()
	bin := buildSpindle(t)
	dataDir := t.TempDir()
	port := freePort(t)
	idp := newOIDP(t)
	public := fmt.Sprintf("http://walgit.localhost:%d", port)
	writeSpindleConfig(t, dataDir, fmt.Sprintf(`
[server]
listen = "0.0.0.0:%d"
public_url = %q
auto_create_on_push = true

%s
[store]
backend = "filesystem"
root = %q

[cache]
dir = %q
`, port, public, idp.configBlock(), filepath.Join(dataDir, "store"), filepath.Join(dataDir, "cache")))
	token := idp.idToken("alice@example.com", "spindle-admin")
	e2eDefaultBearer = token
	s := startServer(t, bin, dataDir, port)
	t.Cleanup(s.stop)
	return s, idp, token
}
