package main

// setup_backup_test.go — defect 2 of the round-3 settings audit: the first
// canonicalising /admin/config save keeps the operator's previous file as
// spindle.toml.bak-<utc> (once), a no-op save writes nothing, and the page says
// both next to Save. The harness helpers live in setup_audit3_test.go.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/webui"
)

// TestAdminConfigCanonicalSaveKeepsOneBackup is defect 2's rendered contract: a
// hand-written file is canonicalised once, its previous text is kept beside
// it, the page says so, and later saves neither rewrite nor back up again.
func TestAdminConfigCanonicalSaveKeepsOneBackup(t *testing.T) {
	canonical, err := config.MarshalTOML(adminConfigPageFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	handWritten := append([]byte("# operator note: keep this line across saves\n"), canonical...)
	ui, path := adminConfigPageHarnessFile(t, handWritten)

	// The page states the contract next to Save (one primary on the page).
	rec := adminConfigPageDo(t, ui, http.MethodGet, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config = %d", rec.Code)
	}
	page := rec.Body.String()
	for _, want := range []string{"full canonical", "spindle.toml.bak-", "comments in a hand-written file are not preserved"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the Save copy is missing %q", want)
		}
	}
	if got := strings.Count(page, `class="btn primary"`); got != 1 {
		t.Fatalf("primaries on /admin/config = %d, want 1", got)
	}

	// First save (zero edits) canonicalises; the previous text is preserved.
	form := scrapeAdminConfigForm(t, page)
	form.Set("op", "save")
	rec = adminConfigPageDo(t, ui, http.MethodPost, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /admin/config = %d: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "backup=spindle.toml.bak-") {
		t.Fatalf("post-save redirect = %q, want the backup name", loc)
	}
	backups, _ := filepath.Glob(path + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one", backups)
	}
	kept, err := os.ReadFile(backups[0])
	if err != nil || !bytes.Equal(kept, handWritten) {
		t.Fatalf("backup is not the operator's original file (err=%v)", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "operator note") {
		t.Fatal("the canonical rewrite kept a comment the encoder cannot represent")
	}
	// The values are unchanged: the rewritten document decodes to the same
	// canonical bytes as the pre-save file.
	before, err := config.LoadSetupBase(filepath.Dir(path), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := config.MarshalTOML(before)
	if !bytes.Equal(want, saved) {
		t.Fatalf("zero-edit save changed values:\n--- want ---\n%s\n--- got ---\n%s", want, saved)
	}
	// The saved page names the backup it kept.
	if rec = adminConfigPageDo(t, ui, http.MethodGet, nil); rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", loc, rec.Code)
	}
	got := adminConfigPageDoURL(t, ui, loc)
	if !strings.Contains(got.Body.String(), "was kept beside it as") {
		t.Fatal("the saved banner does not name the backup")
	}

	// Second save: byte-identical, no new backup, no backup parameter.
	rec = adminConfigPageDo(t, ui, http.MethodGet, nil)
	form = scrapeAdminConfigForm(t, rec.Body.String())
	form.Set("op", "save")
	rec = adminConfigPageDo(t, ui, http.MethodPost, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("second POST /admin/config = %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "backup=spindle") {
		t.Fatalf("a no-op save reports a backup: %q", loc)
	}
	again, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(again, saved) {
		t.Fatalf("a no-op save changed the file (err=%v)", err)
	}
	if all, _ := filepath.Glob(path + ".bak-*"); len(all) != 1 {
		sort.Strings(all)
		t.Fatalf("backups after the second save = %v, want one", all)
	}
}

// adminConfigPageDoURL issues a GET against an absolute path on the page provider.
func adminConfigPageDoURL(t *testing.T, ui *webui.Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://example.com"+path, nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	if !ui.Serve(rec, req) {
		t.Fatalf("GET %s was not answered by the page provider", path)
	}
	return rec
}
