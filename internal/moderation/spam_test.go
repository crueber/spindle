package moderation

// spam_test.go — PLAN-V2 §M.7's advisory spam signal: below/at threshold, the
// 24h window (fake clock), one report per burst, no scoring for anonymous, the
// fixed ring size, and the counter's trigger condition (the returned flag the
// server turns into spindle_spam_flag_total).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// spamBody is long enough to clear SpamMinBody.
const spamBody = "Buy cheap followers now at example-spam dot com, limited offer"

func testSpamService(t *testing.T) (*Service, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	s := New(store.NewMemory())
	s.Now = func() time.Time { return now }
	return s, &now
}

// spamReports lists the open reports a repo's index holds for the reserved
// reporter.
func spamReports(t *testing.T, s *Service, owner, repo string) []ReportCard {
	t.Helper()
	ix, err := s.loadReportsIndex(ctx(), owner, repo)
	if err != nil {
		t.Fatalf("loadReportsIndex: %v", err)
	}
	var out []ReportCard
	for _, c := range ix.Open {
		if c.Reporter == SpamReporter {
			out = append(out, c)
		}
	}
	return out
}

func TestSpamBelowThresholdFilesNothing(t *testing.T) {
	s, _ := testSpamService(t)
	if s.NoteBody(ctx(), "acme", "widgets", "carol", spamBody) {
		t.Fatal("two repeats must not file")
	}
	if s.NoteBody(ctx(), "acme", "widgets", "carol", spamBody) {
		t.Fatal("two repeats must not file")
	}
	if got := spamReports(t, s, "acme", "widgets"); len(got) != 0 {
		t.Fatalf("below threshold filed %d reports", len(got))
	}
	if n := s.BodyObservations("carol"); n != 2 {
		t.Fatalf("ring = %d entries, want 2", n)
	}
}

func TestSpamAtThresholdFilesOneReport(t *testing.T) {
	s, now := testSpamService(t)
	var filed bool
	for i := 0; i < SpamRepeats; i++ {
		*now = now.Add(time.Minute)
		filed = s.NoteBody(ctx(), "acme", "widgets", "carol", spamBody)
	}
	if !filed {
		t.Fatal("the third repeat must file")
	}
	reports := spamReports(t, s, "acme", "widgets")
	if len(reports) != 1 {
		t.Fatalf("filed %d reports, want 1", len(reports))
	}
	got := reports[0]
	if got.Kind != ReportKindUser || got.Target != "user:carol" || got.Reason != "spam" {
		t.Fatalf("report = %+v", got)
	}
	if !strings.Contains(got.Note, "3 times") {
		t.Fatalf("note %q does not name the repetition count", got.Note)
	}
}

// TestSpamOneReportPerBurst pins the anti-dup rule: the same body keeps
// arriving, and the cooldown + the consumed ring mean exactly one report.
func TestSpamOneReportPerBurst(t *testing.T) {
	s, now := testSpamService(t)
	filed := 0
	for i := 0; i < 2*SpamRepeats; i++ {
		*now = now.Add(time.Minute)
		if s.NoteBody(ctx(), "acme", "widgets", "carol", spamBody) {
			filed++
		}
	}
	if filed != 1 {
		t.Fatalf("filed %d times in one burst, want 1", filed)
	}
	if got := spamReports(t, s, "acme", "widgets"); len(got) != 1 {
		t.Fatalf("reports = %d, want 1", len(got))
	}
	// A NEW body in the same burst is a second signal, but the first report is
	// still OPEN and the M3 duplicate rule keys on (reporter, target): no
	// second card until a moderator resolves the first (documented in 16 §13).
	other := spamBody + " (second offer)"
	for i := 0; i < SpamRepeats; i++ {
		*now = now.Add(time.Minute)
		if s.NoteBody(ctx(), "acme", "widgets", "carol", other) {
			t.Fatal("filed a second spam report while the first is open")
		}
	}
	if got := spamReports(t, s, "acme", "widgets"); len(got) != 1 {
		t.Fatalf("reports = %d, want 1", len(got))
	}
}

// TestSpamWindowExpiry drives the fake clock past the window: old repeats are
// not counted, so a body that appeared twice yesterday and once today does not
// trip.
func TestSpamWindowExpiry(t *testing.T) {
	s, now := testSpamService(t)
	if s.NoteBody(ctx(), "acme", "widgets", "carol", spamBody) {
		t.Fatal("first repeat must not file")
	}
	*now = now.Add(SpamWindow + time.Hour)
	if s.NoteBody(ctx(), "acme", "widgets", "carol", spamBody) {
		t.Fatal("an expired observation must not count")
	}
	*now = now.Add(time.Minute)
	if s.NoteBody(ctx(), "acme", "widgets", "carol", spamBody) {
		t.Fatal("two live observations are below threshold")
	}
	if got := spamReports(t, s, "acme", "widgets"); len(got) != 0 {
		t.Fatalf("expired observations filed %d reports", len(got))
	}
	*now = now.Add(time.Minute)
	if !s.NoteBody(ctx(), "acme", "widgets", "carol", spamBody) {
		t.Fatal("three live observations must file")
	}
}

func TestSpamNoScoringForAnonymous(t *testing.T) {
	s, now := testSpamService(t)
	for i := 0; i < 5; i++ {
		*now = now.Add(time.Minute)
		if s.NoteBody(ctx(), "acme", "widgets", "", spamBody) {
			t.Fatal("anonymous writes must not be scored")
		}
	}
	if s.ScoredPrincipals() != 0 {
		t.Fatalf("anonymous principal was tracked: %d", s.ScoredPrincipals())
	}
}

func TestSpamIgnoresShortBodies(t *testing.T) {
	s, now := testSpamService(t)
	for i := 0; i < 5; i++ {
		*now = now.Add(time.Minute)
		if s.NoteBody(ctx(), "acme", "widgets", "carol", "LGTM") {
			t.Fatal("a short body must not file")
		}
	}
	if s.BodyObservations("carol") != 0 {
		t.Fatal("short bodies must not enter the ring")
	}
}

func TestSpamRingIsBounded(t *testing.T) {
	s, now := testSpamService(t)
	for i := 0; i < SpamRingLimit+40; i++ {
		*now = now.Add(time.Second)
		s.NoteBody(ctx(), "acme", "widgets", "carol", fmt.Sprintf("%s %04d", spamBody, i))
	}
	if got := s.BodyObservations("carol"); got != SpamRingLimit {
		t.Fatalf("ring = %d, want %d", got, SpamRingLimit)
	}
}

// TestSpamDoesNotDoubleFileWhenAnOpenReportExists covers the store-side guard:
// a second burst for the same user+repo hits the M3 duplicate rule and is
// swallowed (filed=false → no counter increment, no error).
func TestSpamDoesNotDoubleFileWhenAnOpenReportExists(t *testing.T) {
	s, now := testSpamService(t)
	for i := 0; i < SpamRepeats; i++ {
		*now = now.Add(time.Minute)
		s.NoteBody(ctx(), "acme", "widgets", "carol", spamBody)
	}
	// A fresh burst of a DIFFERENT body after the cooldown would file, but the
	// first report is still open → the duplicate rule refuses it.
	*now = now.Add(SpamWindow + time.Minute)
	other := spamBody + " (variant two)"
	filed := false
	for i := 0; i < SpamRepeats; i++ {
		*now = now.Add(time.Minute)
		filed = s.NoteBody(ctx(), "acme", "widgets", "carol", other)
	}
	if filed {
		t.Fatal("a second open spam report was filed for the same user+repo")
	}
	if got := spamReports(t, s, "acme", "widgets"); len(got) != 1 {
		t.Fatalf("reports = %d, want 1 (the open one)", len(got))
	}
}

func TestScoredPrincipalsAreBounded(t *testing.T) {
	s, now := testSpamService(t)
	for i := 0; i < SpamMaxPrincipals+5; i++ {
		*now = now.Add(time.Second)
		s.NoteBody(ctx(), "acme", "widgets", fmt.Sprintf("user%05d", i), spamBody)
	}
	if got := s.ScoredPrincipals(); got > SpamMaxPrincipals {
		t.Fatalf("scored principals = %d, bound %d", got, SpamMaxPrincipals)
	}
}

// TestSpamFilingFailureIsLoggedNotFatal pins the advisory contract: a store
// that refuses the report write returns false (no counter increment), never an
// error into the write the scorer was observing.
func TestSpamFilingFailureIsLoggedNotFatal(t *testing.T) {
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	// Fail every Put with a real error (not a CAS precondition, which the
	// verb's own retry loop turns into ErrConflict and the scorer treats as
	// an already-open report).
	s := New(hardFailStore{store.NewMemory()})
	s.Now = func() time.Time { return now }
	logged := 0
	s.Logger = warnCounter{&logged}
	for i := 0; i < SpamRepeats; i++ {
		now = now.Add(time.Minute)
		s.Now = func() time.Time { return now }
		if s.NoteBody(ctx(), "acme", "widgets", "carol", spamBody) {
			t.Fatal("a failed filing reported success")
		}
	}
	if logged == 0 {
		t.Fatal("a failed filing was silent")
	}
}

// hardFailStore refuses every Put with a plain error.
type hardFailStore struct{ store.ObjectStore }

func (hardFailStore) Put(context.Context, string, store.PutBody, store.PutOptions) (store.ObjectMeta, error) {
	return store.ObjectMeta{}, errors.New("store unavailable")
}

// warnCounter is a logger that counts warnings (the package's logger seam).
type warnCounter struct{ n *int }

func (w warnCounter) WarnContext(context.Context, string, ...any) { *w.n++ }

// TestRecordAdmin pins the exported administrative audit writer: it needs an
// actor and an action, and a valid call lands one immutable record.
func TestRecordAdmin(t *testing.T) {
	s, _ := testService(t)
	if err := s.RecordAdmin(ctx(), "", "config_save", "instance:config", ""); err == nil {
		t.Fatal("an actor-less audit record was accepted")
	}
	if err := s.RecordAdmin(ctx(), "alice", "", "instance:config", ""); err == nil {
		t.Fatal("an action-less audit record was accepted")
	}
	if err := s.RecordAdmin(ctx(), "alice", ActionConfigSave, "instance:config", "restart=rate_limit.enabled"); err != nil {
		t.Fatalf("RecordAdmin: %v", err)
	}
	e, err := s.AuditEntryAt(ctx(), 1)
	if err != nil || e == nil {
		t.Fatalf("audit entry = %+v, %v", e, err)
	}
	if e.Actor != "alice" || e.Action != ActionConfigSave || e.Target != "instance:config" {
		t.Fatalf("entry = %+v", e)
	}
	if !strings.Contains(e.Context, "rate_limit.enabled") {
		t.Fatalf("detail = %q", e.Context)
	}
}
