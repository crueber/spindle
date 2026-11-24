// moderation_notify_test.go — the PLAN-V2 §M.3 report_filed notification: the
// moderation class reaches the repo admins with reason "moderation", and the
// stored object carries no reported content (no note, no target, no body) —
// the emission's arguments have no such field at all.
package notify

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

func TestEmitModerationCarriesNoReportedContent(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "acme", "repo")
	x.addProfile("admin@example.com", "bob@example.com")

	x.svc.EmitModeration(ctx(), "acme/repo", 7, "bob@example.com", "2026-11-20T10:00:00Z",
		[]string{"admin@example.com"})

	raw, _, err := store.GetBytes(ctx(), x.svc.Store, NotifIndexKey("admin@example.com"), store.GetOptions{})
	if err != nil || raw == nil {
		t.Fatalf("index = %v %v", raw, err)
	}
	var ix IndexDoc
	if err := json.Unmarshal(raw, &ix); err != nil {
		t.Fatal(err)
	}
	if len(ix.Entries) != 1 {
		t.Fatalf("entries = %+v", ix.Entries)
	}
	en := ix.Entries[0]
	if en.Reason != ReasonModeration || en.Kind != "issue" || en.Num != 7 {
		t.Fatalf("entry = %+v", en)
	}
	if en.Title != "A new moderation report needs review" {
		t.Fatalf("title = %q (must be generic, never report content)", en.Title)
	}
	// The notification object itself carries no reported body/note/target.
	rawN, _, _ := store.GetBytes(ctx(), x.svc.Store, NotifKey("admin@example.com", en.ID), store.GetOptions{})
	if rawN == nil {
		t.Fatal("notification object missing")
	}
	body := string(rawN)
	for _, forbidden := range []string{`"note"`, `"target"`, `"body"`, "looks automated"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("notification carries %q: %s", forbidden, body)
		}
	}
	// The actor never notifies themself, and the moderation class does not
	// pull watchers/authors in.
	x.svc.EmitModeration(ctx(), "acme/repo", 7, "admin@example.com", "", []string{"admin@example.com"})
	if n := countNotifs(t, x, "admin@example.com"); n != 1 {
		t.Fatalf("actor notified themself: %d", n)
	}
}
