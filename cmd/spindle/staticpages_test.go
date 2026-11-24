// staticpages_test.go — the Feature 19 composition shape (docs/features/19
// §5–§7): the constructor refuses an incomplete boot, the per-repo settings
// adapter fails closed, the visibility adapter asks exactly the anonymous
// question, and a Handler built from these seams answers one Host only.
package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/staticpages"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/wal"
)

// staticTestRepoView is a RepoView whose only meaningful method is Settings:
// the embedded interface fills the rest (a nil embedded value panics only if
// an unused method is called, which would be a test bug).
type staticTestRepoView struct {
	api.RepoView
	doc api.SettingsDoc
	err error
}

func (f staticTestRepoView) Settings(context.Context, git.RepoId) (api.SettingsDoc, error) {
	return f.doc, f.err
}

// staticTestSettings is the handler's Settings seam.
type staticTestSettings struct {
	site staticpages.SiteSettings
	err  error
}

func (f staticTestSettings) Site(context.Context, string, string) (staticpages.SiteSettings, error) {
	return f.site, f.err
}

// staticTestVisible is the handler's Visible seam.
type staticTestVisible struct {
	ok  bool
	err error
}

func (f staticTestVisible) CanRead(context.Context, string, string) (bool, error) {
	return f.ok, f.err
}

// staticTestDirs returns a fixed serving copy.
type staticTestDirs struct{ dir string }

func (d staticTestDirs) Dir(context.Context, string) (string, error) { return d.dir, nil }

// staticTestGit serves an in-memory tree. lookups counts Lookup calls so a
// test can prove the visibility gate runs before any git work.
type staticTestGit struct {
	entries map[string]staticpages.Entry
	blobs   map[string]string
	lookups int
}

func (g *staticTestGit) Resolve(context.Context, string, string) (string, error) {
	return "commit1", nil
}

func (g *staticTestGit) Lookup(_ context.Context, _, _, path string) (staticpages.Entry, bool, error) {
	g.lookups++
	e, ok := g.entries[path]
	return e, ok, nil
}

func (g *staticTestGit) ReadBlob(_ context.Context, _, oid string, w io.Writer) error {
	body, ok := g.blobs[oid]
	if !ok {
		return staticpages.ErrNotFound
	}
	_, err := io.WriteString(w, body)
	return err
}

// TestNewStaticPagesHandlerNilSeams pins the constructor's fail-closed guard:
// a boot missing any seam returns nil, so serve.go never starts a listener that
// cannot serve (a restricted boot has no store and no identity).
func TestNewStaticPagesHandlerNilSeams(t *testing.T) {
	cfg := config.Defaults()
	view := staticTestRepoView{}
	env := &api.Env{Repo: view}
	// A non-nil registry and identity are only needed by the positive case;
	// they are cheap in-memory values.
	reg := newStaticTestRegistry(t)
	ident := identity.New(store.NewMemory(), cfg)

	cases := []struct {
		name    string
		cfg     *config.Config
		ident   *identity.Service
		reg     *wal.Registry
		apiEnv  *api.Env
		wantNil bool
	}{
		{name: "nil config", cfg: nil, ident: ident, reg: reg, apiEnv: env, wantNil: true},
		{name: "nil identity", cfg: cfg, ident: nil, reg: reg, apiEnv: env, wantNil: true},
		{name: "nil registry", cfg: cfg, ident: ident, reg: nil, apiEnv: env, wantNil: true},
		{name: "nil api env", cfg: cfg, ident: ident, reg: reg, apiEnv: nil, wantNil: true},
		{name: "nil repo view", cfg: cfg, ident: ident, reg: reg, apiEnv: &api.Env{}, wantNil: true},
		{name: "complete", cfg: cfg, ident: ident, reg: reg, apiEnv: env, wantNil: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newStaticPagesHandler(tc.cfg, tc.ident, tc.reg, tc.apiEnv, nil)
			if tc.wantNil && h != nil {
				t.Fatalf("handler = %v, want nil", h)
			}
			if !tc.wantNil && h == nil {
				t.Fatal("handler = nil, want a wired handler")
			}
		})
	}
}

func newStaticTestRegistry(t *testing.T) *wal.Registry {
	t.Helper()
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	reg := wal.NewRegistry(context.Background(), store.NewMemory(), cfg)
	t.Cleanup(reg.Close)
	return reg
}

// TestStaticSiteSettingsFailClosed pins the publication adapter (19 §5): only a
// readable document with [static_site] enabled=true is published; every other
// shape — missing view, read error, empty/invalid TOML, absent or false key —
// resolves to the zero value, so the handler 404s instead of guessing.
func TestStaticSiteSettingsFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		view    api.RepoView
		want    staticpages.SiteSettings
		wantErr bool
	}{
		{name: "nil view", view: nil},
		{name: "read error", view: staticTestRepoView{err: errors.New("store down")}},
		{name: "empty document", view: staticTestRepoView{doc: api.SettingsDoc{}}},
		{name: "invalid document", view: staticTestRepoView{doc: api.SettingsDoc{TOML: "{oops"}}},
		{name: "section absent", view: staticTestRepoView{doc: api.SettingsDoc{TOML: "[features]\nissues = true\n"}}},
		{name: "explicitly disabled", view: staticTestRepoView{doc: api.SettingsDoc{TOML: "[static_site]\nenabled = false\n"}}},
		{
			name: "published",
			view: staticTestRepoView{doc: api.SettingsDoc{TOML: "[static_site]\nenabled = true\nref = \"gh-pages\"\nroot = \"public\"\n"}},
			want: staticpages.SiteSettings{Enabled: true, Ref: "gh-pages", Root: "public"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := staticSiteSettings{view: tc.view}.Site(context.Background(), "acme", "site")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("settings = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestStaticSiteVisibleIsAnonymous pins the visibility adapter (19 §2): the
// listener has no credentials, so it asks exactly identity's anonymous read
// question — public repos pass, private ones and a missing service do not.
func TestStaticSiteVisibleIsAnonymous(t *testing.T) {
	cfg := config.Defaults()
	st := store.NewMemory()
	ident := identity.New(st, cfg)
	ctx := context.Background()

	if ok, err := (staticSiteVisible{ident: nil}).CanRead(ctx, "acme", "site"); err != nil || ok {
		t.Fatalf("nil identity: ok=%v err=%v, want false/nil", ok, err)
	}
	// No access.json: the legacy default is public, so anonymous reads pass.
	if ok, err := (staticSiteVisible{ident: ident}).CanRead(ctx, "acme", "site"); err != nil || !ok {
		t.Fatalf("public repo: ok=%v err=%v, want true/nil", ok, err)
	}
	// A private repo must not be visible to the anonymous listener.
	if _, err := ident.PutAccess(ctx, "acme", "private", "", identity.VisibilityPrivate, nil); err != nil {
		t.Fatalf("PutAccess: %v", err)
	}
	if ok, err := (staticSiteVisible{ident: ident}).CanRead(ctx, "acme", "private"); err != nil || ok {
		t.Fatalf("private repo: ok=%v err=%v, want false/nil", ok, err)
	}
}

// TestStaticPagesHandlerComposition drives a Handler built from the same seam
// shapes the constructor supplies — no server, no registry, no real git — and
// pins the four composition facts that matter: one Host only, the visibility
// gate before any git work, publication from the settings document, and a real
// 200 for an index blob.
func TestStaticPagesHandlerComposition(t *testing.T) {
	const body = "<h1>static</h1>"
	gitStub := func() *staticTestGit {
		return &staticTestGit{
			entries: map[string]staticpages.Entry{
				"index.html": {Mode: "100644", Type: "blob", OID: "oid-index", Size: int64(len(body))},
			},
			blobs: map[string]string{"oid-index": body},
		}
	}
	build := func(visible staticpages.Visible, settings staticpages.Settings, g *staticTestGit) *staticpages.Handler {
		return staticpages.New(staticpages.Options{
			Host:       "static.localhost",
			IndexFiles: []string{"index.html"},
			Dirs:       staticTestDirs{dir: "/repos/acme/site"},
			Git:        g,
			Settings:   settings,
			Visible:    visible,
		})
	}
	published := staticTestSettings{site: staticpages.SiteSettings{Enabled: true}}

	cases := []struct {
		name      string
		handler   func(*staticTestGit) *staticpages.Handler
		url       string
		wantCode  int
		wantBody  string
		wantType  string
		wantCalls int
	}{
		{
			name:      "wrong host is a bodiless 404",
			handler:   func(g *staticTestGit) *staticpages.Handler { return build(staticTestVisible{ok: true}, published, g) },
			url:       "http://app.localhost/acme/site/",
			wantCode:  http.StatusNotFound,
			wantCalls: 0,
		},
		{
			name:      "published public site serves its index",
			handler:   func(g *staticTestGit) *staticpages.Handler { return build(staticTestVisible{ok: true}, published, g) },
			url:       "http://static.localhost/acme/site/",
			wantCode:  http.StatusOK,
			wantBody:  body,
			wantType:  "text/html; charset=utf-8",
			wantCalls: 1,
		},
		{
			name:      "private repo is 404 before any git work",
			handler:   func(g *staticTestGit) *staticpages.Handler { return build(staticTestVisible{ok: false}, published, g) },
			url:       "http://static.localhost/acme/site/",
			wantCode:  http.StatusNotFound,
			wantBody:  "not found",
			wantCalls: 0,
		},
		{
			name: "visibility error is 404",
			handler: func(g *staticTestGit) *staticpages.Handler {
				return build(staticTestVisible{err: errors.New("boom")}, published, g)
			},
			url:       "http://static.localhost/acme/site/",
			wantCode:  http.StatusNotFound,
			wantBody:  "not found",
			wantCalls: 0,
		},
		{
			name: "unpublished repo is 404",
			handler: func(g *staticTestGit) *staticpages.Handler {
				return build(staticTestVisible{ok: true}, staticTestSettings{}, g)
			},
			url:       "http://static.localhost/acme/site/",
			wantCode:  http.StatusNotFound,
			wantBody:  "not found",
			wantCalls: 0,
		},
		{
			name: "unreadable settings document is 404",
			handler: func(g *staticTestGit) *staticpages.Handler {
				return build(staticTestVisible{ok: true}, staticTestSettings{err: errors.New("store down")}, g)
			},
			url:       "http://static.localhost/acme/site/",
			wantCode:  http.StatusNotFound,
			wantBody:  "not found",
			wantCalls: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gitStub()
			rec := httptest.NewRecorder()
			tc.handler(g).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.url, nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if got := rec.Body.String(); got != tc.wantBody {
				t.Fatalf("body = %q, want %q", got, tc.wantBody)
			}
			if tc.wantType != "" {
				if got := rec.Header().Get("Content-Type"); got != tc.wantType {
					t.Fatalf("Content-Type = %q, want %q", got, tc.wantType)
				}
			}
			if g.lookups != tc.wantCalls {
				t.Fatalf("git lookups = %d, want %d", g.lookups, tc.wantCalls)
			}
		})
	}
}
