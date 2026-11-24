package main

// admin_config_token_fields_test.go — the Configuration page's save/coerce path
// for the static-token lifecycle fields (06_server_http.md §8.2.1): the row
// editor's `name` and `expires_at` cells are ordinary posted cells, a valid
// value is written to the canonical file, and a malformed expiry is a 422 that
// names the row key and leaves the file untouched.

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestAdminConfigTokenLifecycleFields(t *testing.T) {
	ui, path, _ := adminConfigPageHarness(t)

	rec := adminConfigPageDo(t, ui, http.MethodGet, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config = %d", rec.Code)
	}
	form := scrapeAdminConfigForm(t, rec.Body.String())
	form.Set("op", "save")
	form.Set("cfg.server.auth.tokens#0.name", "alice-2026h2")
	form.Set("cfg.server.auth.tokens#0.expires_at", "2027-06-01T00:00:00Z")

	rec = adminConfigPageDo(t, ui, http.MethodPost, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /admin/config = %d: %s", rec.Code, rec.Body.String())
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(strings.Fields(string(saved)), " ")
	for _, want := range []string{
		`name = "alice-2026h2"`,
		`expires_at = "2027-06-01T00:00:00Z"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("saved file lacks %q:\n%s", want, saved)
		}
	}

	// A malformed expiry is rejected on the same path and names the row.
	rec = adminConfigPageDo(t, ui, http.MethodGet, nil)
	bad := scrapeAdminConfigForm(t, rec.Body.String())
	bad.Set("op", "save")
	bad.Set("cfg.server.auth.tokens#0.expires_at", "2027-06-01")
	rec = adminConfigPageDo(t, ui, http.MethodPost, bad)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("malformed expires_at = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "server.auth.tokens[0].expires_at") {
		t.Fatalf("422 does not name the row key: %s", rec.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(saved) {
		t.Fatalf("a rejected save changed the file")
	}
}
