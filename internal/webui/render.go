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
	"strings"

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

	Mode       string
	SignedIn   bool
	Admin      bool
	ShowLogin  bool
	LoginHref  string
	ShowCreate bool
	ShowSetup  bool
	Initials   string
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
	mode := s.authMode()
	signedIn := s.signedIn(p)
	return Chrome{
		Title:     title,
		Path:      r.URL.Path,
		Principal: p,
		Version:   s.env.Version,
		Mode:      mode,
		SignedIn:  signedIn,
		Admin:     p.Admin,
		// The browser login exists only for the OIDC flow.
		ShowLogin: !signedIn && mode == "oidc",
		LoginHref: "/_auth/login?next=" + url.QueryEscape(r.URL.Path),
		// Forgejo #466: the create button shares the identity gate.
		ShowCreate: signedIn,
		// Forgejo #371: keys/invitations are per-principal surfaces and live
		// ONLY in the identity menu (an anonymous viewer has nothing to manage
		// there), never in the primary nav. Setup stays in the nav for exactly
		// the first-run/unsecured case: no signed-in principal and an auth mode
		// that cannot mint one ("" or "none"), so a fresh instance can be
		// configured; otherwise a signed-in admin reaches /setup from the menu.
		ShowSetup: !signedIn && (mode == "" || mode == "none"),
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
	// Authenticated page bodies must never enter a shared cache.
	h.Set("Cache-Control", "no-cache, private")
	h.Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
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

// pageRepo renders a repo-scoped page: the same layout model plus the shared
// repository chrome (masthead + tab strip), which the layout places above the
// page's own content. It is the ONE attach point — a group never re-renders
// the header itself (12_web_ui.md §2.2.1). chrome must already be gated: build
// it with repoChromeFor/repoChromeFrom AFTER the handler's own read gate ran.
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
