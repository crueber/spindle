package setup

// token_fields_test.go — the static-token row editor round-trips the lifecycle
// columns (06_server_http.md §8.2.1): `name` and `expires_at` are ordinary
// element fields, so the page renders them from the struct tag and the one
// Coerce path writes them back. No bespoke column list exists.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
)

func TestStaticTokenRowEditorCarriesLifecycleFields(t *testing.T) {
	c := config.Defaults()
	c.Server.Auth.Tokens = []config.StaticToken{{
		Principal: "ci-bot",
		Name:      "ci-old",
		Token:     "tok-old-secret",
		Write:     true,
		ExpiresAt: "2026-12-31T23:59:59Z",
	}}
	groups := BuildSchema(c, config.Defaults())
	f, err := findField(groups, "server.auth.tokens")
	if err != nil {
		t.Fatal(err)
	}
	rows := RenderRows(f)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want element + add row", len(rows))
	}
	for _, name := range []string{"name", "expires_at", "principal", "write", "token", "token_env"} {
		cell := rowCell(t, rows[0], name)
		if cell.Name != name {
			t.Fatalf("missing row column %q", name)
		}
	}
	if got := rowCell(t, rows[0], "name").Value; got != "ci-old" {
		t.Fatalf("name cell = %q", got)
	}
	if got := rowCell(t, rows[0], "expires_at").Value; got != "2026-12-31T23:59:59Z" {
		t.Fatalf("expires_at cell = %q", got)
	}
	if rowCell(t, rows[0], "expires_at").Secret {
		t.Fatal("expires_at is not a secret")
	}
}

func TestStaticTokenRowEditorCoercesLifecycleFields(t *testing.T) {
	base := roundTripConfig(t)
	groups := BuildSchema(base, config.Defaults())
	values := FormValues(groups)

	// The page edits the existing row's label/expiry and adds a new row.
	add := len(base.Server.Auth.Tokens)
	rowKey := func(field string) string { return fmt.Sprintf("server.auth.tokens#%d.%s", add, field) }
	values.Set("server.auth.tokens#0.name", "ci-rotated")
	values.Set("server.auth.tokens#0.expires_at", "2027-06-01T00:00:00Z")
	values.Set(rowKey("name"), "ci-new")
	values.Set(rowKey("principal"), "ci-bot")
	values.Set(rowKey("token"), "fresh-secret")
	values.Set(rowKey("write"), "true")
	values.Set(rowKey("expires_at"), "2027-07-01T00:00:00Z")

	got, err := ParseValues(values, cloneConfig(t, base))
	if err != nil {
		t.Fatalf("coerce: %v", err)
	}
	if len(got.Server.Auth.Tokens) != len(base.Server.Auth.Tokens)+1 {
		t.Fatalf("tokens = %d, want one added", len(got.Server.Auth.Tokens))
	}
	first := got.Server.Auth.Tokens[0]
	if first.Name != "ci-rotated" || first.ExpiresAt != "2027-06-01T00:00:00Z" {
		t.Fatalf("edited row = %+v", first)
	}
	added := got.Server.Auth.Tokens[len(got.Server.Auth.Tokens)-1]
	if added.Name != "ci-new" || added.Principal != "ci-bot" ||
		added.Token != "fresh-secret" || !added.Write ||
		added.ExpiresAt != "2027-07-01T00:00:00Z" {
		t.Fatalf("added row = %+v", added)
	}

	// The coerced config validates (the save path's second gate).
	if _, errs := config.Validate(got); len(errs) != 0 {
		t.Fatalf("coerced config does not validate: %v", errs)
	}

	// A malformed expiry is stored by coercion (it is a string) and rejected
	// by Validate with the exact row key the page marks.
	values.Set("server.auth.tokens#0.expires_at", "31/12/2026")
	bad, err := ParseValues(values, cloneConfig(t, base))
	if err != nil {
		t.Fatalf("coerce malformed expiry: %v", err)
	}
	_, errs := config.Validate(bad)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "server.auth.tokens[0].expires_at") {
			found = true
		}
	}
	if !found {
		t.Fatalf("validation did not name the row key: %v", errs)
	}
}
