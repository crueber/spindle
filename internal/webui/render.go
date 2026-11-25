package webui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"

	"github.com/crueber/spindle/internal/cachepolicy"
	"github.com/crueber/spindle/internal/httpsec"
	"github.com/crueber/spindle/internal/server/auth"
)

//go:embed all:templates
var tplFS embed.FS

// Chrome is the layout data every page gets: identity, nav visibility, and the
// bits of the site header/footer that depend on them.
type Chrome struct {
	Title     string
	Path      string
	Principal auth.Principal
	Version   string

	SignedIn   bool
	Admin      bool
	ShowLogin  bool
	LoginHref  string
	ShowCreate bool
	// ShowAdmin is the site-nav Administration link: present when the visitor
	// can actually use the area (open mode, or a signed-in host admin), hidden
	// otherwise — never shown-then-403'd (the org-tabs precedent).
	ShowAdmin bool
	Initials  string
}

// PageData is what a page template receives: {{.Chrome}} plus {{.Data}}.
//
// Repo is the repository chrome (masthead + tab strip) for a page scoped to a
// repository, and nil everywhere else. The LAYOUT renders it above the page's
// own content, so a repo-scoped page can never forget the chrome and a page
// that already carries one (the repo group's templates) simply leaves it nil
// (12_web_ui.md §2.2.1).
type PageData struct {
	Chrome Chrome
	Repo   *RepoChrome
	Data   any
}

// chrome builds the layout model from the principal (the server-side
// equivalent of web/src/lib/identity.js navModel).
func (s *Server) chrome(r *http.Request, p auth.Principal, title string) Chrome {
	signedIn := s.signedIn(p)
	return Chrome{
		Title:     title,
		Path:      r.URL.Path,
		Principal: p,
		Version:   s.env.Version,
		SignedIn:  signedIn,
		Admin:     p.Admin,
		// OIDC is the only authentication and boot validation guarantees the
		// login trio, so the sign-in affordance is always the OIDC entry.
		ShowLogin: !signedIn,
		LoginHref: "/_auth/login?next=" + url.QueryEscape(r.URL.Path),
		// Forgejo #466: the create button shares the identity gate.
		ShowCreate: signedIn,
		// Forgejo #371: keys/invitations are per-principal surfaces and live
		// ONLY in the identity menu (an anonymous viewer has nothing to manage
		// there), never in the primary nav. The Administration link follows the
		// area's real access rule (PLAN-ADMIN §1.2): a signed-in host admin, or
		// the restricted boot (where /admin is the only surface anyway). That
		// is exactly "can use it", so no visitor is offered a link that 403s.
		ShowAdmin: p.Admin,
		Initials:  initials(p.Name),
	}
}

func initials(name string) string {
	if name == "" {
		return "?"
	}
	r := []rune(strings.ToUpper(name))
	if len(r) == 1 {
		return string(r)
	}
	return string(r[0])
}

// parseTemplates builds one template set per page file, each composed of the
// base layout, every partial, and that page.
func (s *Server) parseTemplates() error {
	s.tpl = map[string]*template.Template{}
	// Group pages live one level down; the shared error page lives at
	// templates/pages/error.html. Both are pages in their own right.
	pages, err := fs.Glob(tplFS, "templates/pages/*/*.html")
	if err != nil {
		return err
	}
	shared, err := fs.Glob(tplFS, "templates/pages/*.html")
	if err != nil {
		return err
	}
	for _, p := range append(pages, shared...) {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "templates/pages/"), ".html")
		t, err := template.New("base").Funcs(s.funcs()).ParseFS(tplFS,
			"templates/base.html", "templates/partials/*.html", p)
		if err != nil {
			return fmt.Errorf("webui: parse %s: %w", name, err)
		}
		s.tpl[name] = t
	}
	return nil
}

// render writes a page with a strong ETag and no-cache (the shell rule, so a
// deploy is visible on the next request).
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, data PageData) {
	t, ok := s.tpl[name]
	if !ok {
		fail(w, r, http.StatusInternalServerError, "page template missing: "+name)
		return
	}
	data = projectPrimary(t, data)
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		// A template failure must not recurse into the error page (which is
		// itself a template): log the detail and write the minimal document.
		log.Printf("webui render %s: %v", name, err)
		fail(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	body := buf.Bytes()
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	h := w.Header()
	httpsec.Apply(h)
	h.Set("Content-Type", "text/html; charset=utf-8")
	// Authenticated page bodies must never enter a shared cache. A page that
	// owns a tighter class (the audit viewer's and the search pages' no-store)
	// sets it before the render and keeps it.
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-cache, private")
	}
	// A no-store body has no revalidation story, so it carries no ETag: a
	// conditional request could otherwise confirm a stale principal's answer
	// (20_search.md §8, §12.11). cachepolicy.NoStore is the one such class.
	if h.Get("Cache-Control") != cachepolicy.NoStore {
		h.Set("ETag", etag)
		if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// page renders with the layout model built from the principal.
func (s *Server) page(w http.ResponseWriter, r *http.Request, p auth.Principal, status int, name, title string, data any) {
	s.pageRepo(w, r, p, nil, status, name, title, data)
}

// classAttrRe finds every class attribute in a rendered fragment; the one
// primary rule counts button nodes, so token membership (not substring order)
// decides whether a node is `.btn.primary`.
var classAttrRe = regexp.MustCompile(`class="([^"]*)"`)

// countPrimaryButtons counts `.btn.primary` nodes in rendered HTML. It is the
// renderer's own reading of the page, shared with the tests so the rule cannot
// be satisfied by the source and broken by a conditional branch.
func countPrimaryButtons(body []byte) int {
	n := 0
	for _, m := range classAttrRe.FindAllSubmatch(body, -1) {
		var btn, primary bool
		for _, tok := range strings.Fields(string(m[1])) {
			switch tok {
			case "btn":
				btn = true
			case "primary":
				primary = true
			}
		}
		if btn && primary {
			n++
		}
	}
	return n
}

// repoChromeOf finds the repository chrome a page renders. The layout owns it
// for collab/form pages (PageData.Repo); the repo group's own templates carry
// it inside their content, as the `Repo *RepoChrome` field of their page data
// (12_web_ui.md §2.2.1). One lookup covers both shapes.
func repoChromeOf(d PageData) *RepoChrome {
	if d.Repo != nil {
		return d.Repo
	}
	rv := reflect.ValueOf(d.Data)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	f := rv.FieldByName("Repo")
	if !f.IsValid() || f.Kind() != reflect.Pointer || f.IsNil() {
		return nil
	}
	c, _ := f.Interface().(*RepoChrome)
	return c
}

// projectPrimary applies style-guideline F3 in ONE place: the masthead Clone
// is the page's primary action exactly when the page body renders none of its
// own. It probes the content template with Clone demoted, counts the page's
// own `.btn.primary` nodes, and promotes Clone only when that count is zero —
// so conditional CTAs (a held run's approve, a commit's Create tag, a signed
// out issue's absent composer) need no per-page list and no handler flags.
// A page that still renders two primaries of its own is a template bug the
// one-primary matrix test catches; the probe never invents an action.
func projectPrimary(t *template.Template, d PageData) PageData {
	c := repoChromeOf(d)
	if c == nil {
		return d
	}
	content := t.Lookup("content")
	if content == nil {
		return d
	}
	c.ClonePrimary = false
	var buf bytes.Buffer
	if err := content.Execute(&buf, d); err != nil {
		// The full render reports the same error with the template detail.
		return d
	}
	c.ClonePrimary = countPrimaryButtons(buf.Bytes()) == 0
	return d
}

// pageRepo renders a repo-scoped page: the same layout model plus the shared
// repository chrome (masthead + tab strip), which the layout places above the
// page's own content. It is the ONE attach point — a group never re-renders
// the header itself (12_web_ui.md §2.2.1). chrome must already be gated: build
// it with repoChromeFor/repoChromeFrom AFTER the handler's own read gate ran.
//
// F3 (one amber primary) is applied by projectPrimary: the masthead Clone is
// promoted to the page's primary action only when the page body renders no
// primary of its own (style guideline §1 F3).
func (s *Server) pageRepo(w http.ResponseWriter, r *http.Request, p auth.Principal, chrome *RepoChrome, status int, name, title string, data any) {
	s.render(w, r, status, name, PageData{Chrome: s.chrome(r, p, title), Repo: chrome, Data: data})
}

// errorPage renders the shared error page (HTML) or a plain-text body when the
// caller asks for text (the API's convention).
func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if w.Header().Get("Content-Type") == "" && headerWantsText(r) {
		http.Error(w, msg, status)
		return
	}
	if _, ok := s.tpl["error"]; !ok {
		fail(w, r, status, msg)
		return
	}
	anon := auth.Principal{Name: "anonymous", Anonymous: true}
	s.render(w, r, status, "error", PageData{
		Chrome: s.chrome(r, anon, statusTitle(status)),
		Data: struct {
			Status  int
			Message string
		}{status, msg},
	})
}

// fail writes the smallest possible HTML error document, with no template
// involvement. It is the last resort for the renderer itself (a missing or
// broken template must never turn into a crash or a recursion).
func fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	httpsec.Apply(w.Header())
	if headerWantsText(r) {
		http.Error(w, msg, status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, private")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	fmt.Fprintf(w, "<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\"><title>%s</title></head>"+
		"<body><h1>%s</h1><p>%s</p><p><a href=\"/\">back home</a></p></body></html>",
		template.HTMLEscapeString(statusTitle(status)), template.HTMLEscapeString(statusTitle(status)),
		template.HTMLEscapeString(msg))
}

func statusTitle(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "Sign in required"
	case http.StatusForbidden:
		return "Forbidden"
	case http.StatusNotFound:
		return "Not found"
	case http.StatusServiceUnavailable:
		return "Unavailable"
	default:
		return "Error"
	}
}
