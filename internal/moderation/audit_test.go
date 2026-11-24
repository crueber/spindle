package moderation

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
)

func TestAuditRecordsBlockAndUnblock(t *testing.T) {
	s, _ := testService(t)
	if _, _, err := s.Block(ctx(), "alice", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Unblock(ctx(), "alice", "bob"); err != nil {
		t.Fatal(err)
	}
	first, err := s.AuditEntryAt(ctx(), 1)
	if err != nil || first == nil {
		t.Fatalf("audit 1 = (%v, %v)", first, err)
	}
	if first.Action != ActionBlock || first.Actor != "alice" || first.Target != "user:bob" ||
		first.At != "2026-11-21T12:00:00Z" || first.Seq != 1 {
		t.Fatalf("audit 1 = %+v", first)
	}
	second, err := s.AuditEntryAt(ctx(), 2)
	if err != nil || second == nil || second.Action != ActionUnblock {
		t.Fatalf("audit 2 = (%+v, %v)", second, err)
	}

	entries, more, err := s.AuditSince(ctx(), 0, 10)
	if err != nil || more || len(entries) != 2 {
		t.Fatalf("since = (%d, %v, %v)", len(entries), more, err)
	}
	// A page that ends exactly at the last record reports more=false; a page
	// with one record and more behind reports more=true.
	entries, more, err = s.AuditSince(ctx(), 0, 1)
	if err != nil || !more || len(entries) != 1 {
		t.Fatalf("page = (%d, %v, %v)", len(entries), more, err)
	}
	entries, more, err = s.AuditSince(ctx(), 2, 10)
	if err != nil || more || len(entries) != 0 {
		t.Fatalf("tail = (%d, %v, %v)", len(entries), more, err)
	}
}

func TestAuditGapEndsPage(t *testing.T) {
	s, _ := testService(t)
	// Simulate a reserved-but-unwritten seq: create 1 and 3, leave 2 absent.
	for _, seq := range []int{1, 3} {
		body := []byte(`{"seq":` + itoa(seq) + `,"action":"block"}`)
		if _, err := store.PutBytes(ctx(), s.Store, AuditKey(seq), body,
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
	}
	entries, more, err := s.AuditSince(ctx(), 0, 10)
	if err != nil || more || len(entries) != 1 || entries[0].Seq != 1 {
		t.Fatalf("gap page = (%v, %v, %v)", entries, more, err)
	}
}

func TestAuditValidation(t *testing.T) {
	s, _ := testService(t)
	if _, err := s.AuditEntryAt(ctx(), 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("seq 0 = %v", err)
	}
	if e, err := s.AuditEntryAt(ctx(), 99); err != nil || e != nil {
		t.Fatalf("absent = (%v, %v), want (nil, nil)", e, err)
	}
	// Corrupt allocator fails closed.
	if _, err := store.PutBytes(ctx(), s.Store, AuditStateKey(), []byte("{bad"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if err := s.appendAudit(ctx(), "alice", ActionBlock, "user:bob", "", ""); err == nil {
		t.Fatal("corrupt allocator must error")
	}
}

func TestAuditSeqAllocatorIncrements(t *testing.T) {
	s, _ := testService(t)
	for i := 0; i < 3; i++ {
		if err := s.appendAudit(ctx(), "alice", ActionBlock, "user:u"+itoa(i), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	raw, _, err := store.GetBytes(ctx(), s.Store, AuditStateKey(), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var st auditState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.NextSeq != 4 {
		t.Fatalf("next_seq = %d, want 4", st.NextSeq)
	}
	if _, more, err := s.AuditSince(ctx(), 0, 50); err != nil || more {
		t.Fatalf("since = (%v, %v)", more, err)
	}
}

// --- filters + query (PLAN-ADMIN §8.6) ---------------------------------------

// auditFixture writes a fixed log covering every filter dimension (explicit
// seqs and instants: the filter is about the record, not the clock).
func auditFixture(t *testing.T) *Service {
	t.Helper()
	s, _ := testService(t)
	rows := []struct {
		actor, action, target, at string
	}{
		{"alice", "config_save", "instance:config", "2026-11-10T10:00:00Z"},
		{"alice", "access_grant", "repo:acme/widgets", "2026-11-12T10:00:00Z"},
		{"bob", "hide", "repo:acme/gadgets", "2026-11-14T10:00:00Z"},
		{"bob", "transfer", "repo:beta/widgets", "2026-11-16T10:00:00Z"},
		{"carol", "packages_gc", "instance:packages", "2026-11-18T10:00:00Z"},
	}
	for i, r := range rows {
		body := []byte(`{"seq":` + itoa(i+1) + `,"at":"` + r.at + `","actor":"` + r.actor +
			`","action":"` + r.action + `","target":"` + r.target + `"}`)
		if _, err := store.PutBytes(ctx(), s.Store, AuditKey(i+1), body,
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func auditSeqs(entries []AuditEntry) []int {
	out := make([]int, len(entries))
	for i, e := range entries {
		out[i] = e.Seq
	}
	return out
}

func TestAuditQueryFiltersAloneAndCombined(t *testing.T) {
	s := auditFixture(t)
	cases := []struct {
		name   string
		filter AuditFilter
		want   []int
	}{
		{"none", AuditFilter{}, []int{1, 2, 3, 4, 5}},
		{"action", AuditFilter{Action: "HIDE"}, []int{3}},
		{"actor", AuditFilter{Actor: "ALICE"}, []int{1, 2}},
		{"repo", AuditFilter{Repo: "acme/widgets"}, []int{2}},
		{"instance", AuditFilter{Repo: "-"}, []int{1, 5}},
		{"since", AuditFilter{Since: mustTime(t, "2026-11-14T00:00:00Z")}, []int{3, 4, 5}},
		{"combined", AuditFilter{Actor: "bob", Since: mustTime(t, "2026-11-15T00:00:00Z")}, []int{4}},
		{"no match", AuditFilter{Actor: "nobody"}, nil},
	}
	for _, c := range cases {
		page, err := s.AuditQuery(ctx(), 0, 10, c.filter)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := auditSeqs(page.Entries)
		if len(got) != len(c.want) {
			t.Fatalf("%s: seqs = %v, want %v", c.name, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: seqs = %v, want %v", c.name, got, c.want)
			}
		}
	}
}

func TestAuditQueryPagingSurvivesFilters(t *testing.T) {
	s := auditFixture(t)
	// Page size 1 under the alice filter: two records, so one page + one more.
	first, err := s.AuditQuery(ctx(), 0, 1, AuditFilter{Actor: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != 1 || first.Entries[0].Seq != 1 || !first.More {
		t.Fatalf("first page = %+v", first)
	}
	if first.Next != 1 {
		t.Fatalf("next cursor = %d, want 1", first.Next)
	}
	second, err := s.AuditQuery(ctx(), first.Next, 1, AuditFilter{Actor: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Entries) != 1 || second.Entries[0].Seq != 2 {
		t.Fatalf("second page = %+v", second)
	}
	if second.More {
		t.Fatalf("second page reports more: %+v", second)
	}
	// The cursor is the SCAN position even when a page ends on a non-match:
	// from #2, bob's hide at #3 with a page size of 1 leaves more behind.
	bob, err := s.AuditQuery(ctx(), 2, 1, AuditFilter{Actor: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bob.Entries) != 1 || bob.Entries[0].Seq != 3 || !bob.More {
		t.Fatalf("bob page = %+v", bob)
	}
}

// TestAuditQueryScanBoundIsHonest: a filter that matches nothing stops at the
// scan bound and reports the cursor it reached, never an unbounded walk.
func TestAuditQueryScanBoundIsHonest(t *testing.T) {
	s, _ := testService(t)
	for i := 0; i < auditScanMax+5; i++ {
		if err := s.appendAudit(ctx(), "alice", "hide", "repo:acme/w", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.AuditQuery(ctx(), 0, 10, AuditFilter{Actor: "nobody"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 0 || !page.More || page.Scanned != auditScanMax || page.Next != auditScanMax {
		t.Fatalf("bounded scan = %+v", page)
	}
	// The next request resumes from the scan cursor and finds the rest.
	tail, err := s.AuditQuery(ctx(), page.Next, 10, AuditFilter{Actor: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tail.Entries) != 5 || tail.More {
		t.Fatalf("tail = %d entries, more=%v", len(tail.Entries), tail.More)
	}
}

func mustTime(t *testing.T, v string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t.Fatal(err)
	}
	return at
}
