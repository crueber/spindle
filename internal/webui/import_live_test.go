// import_live_test.go — the Import page's own SSRF gate reads the LIVE [import]
// policy (config.LiveImport), so a policy saved on /admin/config applies to the
// next submit instead of the next restart. This is the page half of the
// 2026-11-25 defect: the save succeeded, the redirect listed the keys as
// restart-required, and the page kept answering
// `source host "…" resolves to a private address (import.allow_private_networks=false)`.
package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/repoimport"
	"github.com/crueber/spindle/internal/store"
)

// newLiveImportHarness is the forms harness with a live policy holder wired the
// way composition wires it: Env.LiveImport over the boot cfg.
func newLiveImportHarness(t *testing.T) (*formsHarness, *config.LiveImport) {
	t.Helper()
	h := newFormsHarness(t, "none")
	h.cfg.Import.AllowPrivateNetworks = false
	h.cfg.Import.AllowFileURLs = false
	h.cfg.Import.URLAllowlist = nil
	live := config.NewLiveImport(h.cfg)
	h.s.env.LiveImport = live
	h.s.env.API = &api.Env{Repos: &formsFakeRegistry{}, Store: store.NewMemory()}
	return h, live
}

// publish is the after-save state an admin's /admin/config write produces.
func publishImport(t *testing.T, live *config.LiveImport, mutate func(*config.Import)) {
	t.Helper()
	imp := config.ImportPolicy(live, nil)
	mutate(&imp)
	live.Set(imp)
}

func TestFormsImportMirrorSSRFReadsLivePolicy(t *testing.T) {
	h, live := newLiveImportHarness(t)
	mir := &formsFakeMirror{}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: &formsFakeImport{}, Mirror: mir})

	submit := func() *httptest.ResponseRecorder {
		return h.request(t, http.MethodPost, "/import", url.Values{
			"kind": {"mirror"}, "source_url": {"file:///srv/src.git"},
			"owner": {"alice"}, "name": {"mirror-live"}, "schedule": {"daily"},
		}, "writer", nil)
	}

	w := submit()
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "file:// sources are disabled (import.allow_file_urls=false)")
	if len(mir.synced) != 0 {
		t.Fatalf("a refused mirror must not sync: %v", mir.synced)
	}

	// No restart: the save publishes the section and the same submit passes.
	publishImport(t, live, func(imp *config.Import) { imp.AllowFileURLs = true })
	w = submit()
	formsExpectStatus(t, w, http.StatusSeeOther)
	formsExpectLocation(t, w, "/alice/mirror-live")
	if len(mir.synced) != 1 {
		t.Fatalf("synced = %v, want the accepted mirror", mir.synced)
	}
}

// The reported sentence: a loopback source with an empty allowlist needs the
// dangerous confirm (a host admin), and the private-address refusal is exactly
// what the operator saw until the restart.
func TestFormsImportPrivateNetworkSentenceReadsLivePolicy(t *testing.T) {
	h, live := newLiveImportHarness(t)
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: &formsFakeImport{}, Mirror: &formsFakeMirror{}})
	h.s.env.API = &api.Env{Repos: &formsFakeRegistry{}, Store: store.NewMemory()}

	submit := func() *httptest.ResponseRecorder {
		return h.request(t, http.MethodPost, "/import", url.Values{
			"kind": {"mirror"}, "source_url": {"http://127.0.0.1/src.git"},
			"owner": {"alice"}, "name": {"priv-live"}, "schedule": {"daily"}, "dangerous": {"on"},
		}, "admin", nil)
	}

	w := submit()
	formsExpectStatus(t, w, http.StatusBadRequest)
	// The rendered page HTML-escapes the host quotes, so assert the sentence
	// body — the operator's reported text.
	formsExpectContains(t, w, "resolves to a private address (import.allow_private_networks=false)")

	publishImport(t, live, func(imp *config.Import) { imp.AllowPrivateNetworks = true })
	w = submit()
	formsExpectStatus(t, w, http.StatusSeeOther)
	if body := w.Body.String(); strings.Contains(body, "import.allow_private_networks=false") {
		t.Fatalf("the sentence survived the live publish: %s", formsTruncate(body))
	}
}

// The one-shot import path gates through ParseRequest, which reads the same
// holder — a live allowlist addition accepts the source without a restart.
func TestFormsImportOnceReadsLiveAllowlist(t *testing.T) {
	h, live := newLiveImportHarness(t)
	h.cfg.Import.AllowPrivateNetworks = true
	h.cfg.Import.URLAllowlist = []string{"other.example"}
	live.Set(h.cfg.Import) // the harness edited the startup config after wiring

	imp := &formsFakeImport{res: &repoimport.BeginResult{TaskID: "task-live"}}
	formsUse(t, FormsDeps{Identity: &formsFakeIdentity{}, Import: imp, Mirror: &formsFakeMirror{}})

	submit := func() *httptest.ResponseRecorder {
		return h.request(t, http.MethodPost, "/import", url.Values{
			"kind": {"import"}, "source_url": {"https://github.com/crueber/spindle.git"},
			"owner": {"alice"}, "name": {"once-live"},
		}, "writer", nil)
	}

	w := submit()
	formsExpectStatus(t, w, http.StatusBadRequest)
	formsExpectContains(t, w, "is not in import.url_allowlist")

	publishImport(t, live, func(ip *config.Import) { ip.URLAllowlist = []string{"github.com"} })
	w = submit()
	formsExpectStatus(t, w, http.StatusOK)
	formsExpectContains(t, w, `data-task-id="task-live"`)
}
