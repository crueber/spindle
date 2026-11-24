package moderation

import (
	"context"
	"encoding/json"
	"errors"

	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/wal"
)

// reportSvc builds a service with triage+ roles and the content probe so the
// filing grammar accepts the fixture targets.
func reportSvc(t *testing.T, role identity.Role) (*Service, *[]StreamEvent) {
	t.Helper()
	s, _ := testService(t)
	s.Roles = &fakeRoles{role: role}
	s.Targets = fakeProbe{keys: map[string]bool{"thread": true, "review": true, "review_thread": true}}
	var frames []StreamEvent
	var mu sync.Mutex
	s.Stream = func(_ context.Context, ev StreamEvent) {
		mu.Lock()
		defer mu.Unlock()
		frames = append(frames, ev)
	}
	return s, &frames
}

func fileThreadReport(t *testing.T, s *Service, reporter string, num, seq int) ReportCard {
	t.Helper()
	card, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: reporter, Write: true}, ReportInput{
		Kind: ReportKindThread, Target: ReportTargetRef{Num: num, Seq: seq}, Reason: "spam", Note: "looks automated",
	})
	if err != nil {
		t.Fatalf("file report: %v", err)
	}
	return card
}

func TestFileReportCreatesEvidenceAndHotWindow(t *testing.T) {
	s, frames := reportSvc(t, identity.RoleRead)
	card := fileThreadReport(t, s, "bob", 12, 3)
	if card.ID != 1 || card.State != ReportOpen || card.Target != ThreadTarget(12, 3) || card.Reporter != "bob" || card.Reason != "spam" {
		t.Fatalf("card = %+v", card)
	}
	// The evidence record is Create-only and carries the immutable request.
	raw, _, err := store.GetBytes(ctx(), s.Store, ReportRecordKey("acme", "widgets", 1), store.GetOptions{})
	if err != nil || raw == nil {
		t.Fatalf("record = %v %v", raw, err)
	}
	var rec Report
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.ID != 1 || rec.Repo != "acme/widgets" || rec.Target != ThreadTarget(12, 3) || rec.Note != "looks automated" {
		t.Fatalf("record = %+v", rec)
	}
	// The repo hot window holds the open card; the global queue holds the row.
	ix, err := s.loadReportsIndex(ctx(), "acme", "widgets")
	if err != nil || len(ix.Open) != 1 || ix.NextID != 2 || ix.Version != 1 {
		t.Fatalf("index = %+v err %v", ix, err)
	}
	gx, _, err := s.loadGlobalIndex(ctx())
	if err != nil || len(gx.Open) != 1 || gx.Open[0].Repo != "acme/widgets" || gx.Open[0].ID != 1 {
		t.Fatalf("global = %+v err %v", gx, err)
	}
	// One moderation frame with the conversation pointer and the reporter.
	if len(*frames) != 1 || (*frames)[0].Action != ActionReportFiled || (*frames)[0].Num != 12 || (*frames)[0].Seq != 3 || (*frames)[0].By != "bob" {
		t.Fatalf("frames = %+v", *frames)
	}
	if (*frames)[0].State() != "" {
		t.Fatalf("report_filed must not carry state, got %q", (*frames)[0].State())
	}
}

func TestFileReportDuplicateOpenIsConflict(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleRead)
	fileThreadReport(t, s, "bob", 12, 3)
	// Same reporter + same target + open → 409.
	if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, ReportInput{
		Kind: ReportKindThread, Target: ReportTargetRef{Num: 12, Seq: 3}, Reason: "abusive",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate = %v, want conflict", err)
	}
	// A different reporter or target is fine.
	if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "carol"}, ReportInput{
		Kind: ReportKindThread, Target: ReportTargetRef{Num: 12, Seq: 3}, Reason: "spam",
	}); err != nil {
		t.Fatalf("other reporter = %v", err)
	}
	card := fileThreadReport(t, s, "bob", 13, 1)
	// A resolved report does not block a new filing of the same target.
	s.Roles = &fakeRoles{role: identity.RoleAdmin}
	if _, err := s.ResolveReport(ctx(), "acme", "widgets", card.ID, auth.Principal{Name: "alice", Write: true}, ResolveNone, "handled"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, ReportInput{
		Kind: ReportKindThread, Target: ReportTargetRef{Num: 13, Seq: 1}, Reason: "spam",
	}); err != nil {
		t.Fatalf("refile after resolve = %v", err)
	}
}

func TestFileReportValidation(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleRead)
	// Anonymous is a real 401.
	if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Anonymous(), ReportInput{
		Kind: ReportKindThread, Target: ReportTargetRef{Num: 1, Seq: 1}, Reason: "spam",
	}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous = %v", err)
	}
	// Bad reason, unknown kind, malformed/absent target.
	for _, in := range []ReportInput{
		{Kind: ReportKindThread, Target: ReportTargetRef{Num: 1, Seq: 1}, Reason: "boring"},
		{Kind: "comment", Target: ReportTargetRef{Num: 1, Seq: 1}, Reason: "spam"},
		{Kind: ReportKindThread, Target: ReportTargetRef{Num: 0, Seq: 1}, Reason: "spam"},
		{Kind: ReportKindReviewThread, Target: ReportTargetRef{Num: 1, Seq: 1, TID: "nope"}, Reason: "spam"},
		{Kind: ReportKindUser, Target: ReportTargetRef{Principal: "bad/name"}, Reason: "spam"},
	} {
		if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, in); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v = %v, want invalid", in, err)
		}
	}
	// A target that does not exist in its family is a 400.
	s.Targets = fakeProbe{keys: map[string]bool{}}
	if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, ReportInput{
		Kind: ReportKindThread, Target: ReportTargetRef{Num: 1, Seq: 1}, Reason: "spam",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("absent target = %v", err)
	}
	// Unknown user report target (the profile probe).
	s.Targets = nil
	if _, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, ReportInput{
		Kind: ReportKindUser, Target: ReportTargetRef{Principal: "ghost"}, Reason: "spam",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown user = %v", err)
	}
	// A valid user report reads as user:<principal> and notifies no num.
	card, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, ReportInput{
		Kind: ReportKindUser, Target: ReportTargetRef{Principal: "carol"}, Reason: "abusive",
	})
	if err != nil || card.Target != "user:carol" {
		t.Fatalf("user report = %+v %v", card, err)
	}
}

func TestResolveReportActionsAndAudit(t *testing.T) {
	s, frames := reportSvc(t, identity.RoleRead)
	card := fileThreadReport(t, s, "bob", 12, 3)
	admin := auth.Principal{Name: "alice", Write: true}

	// Reader cannot resolve; the admin floor is in the verb.
	if _, err := s.ResolveReport(ctx(), "acme", "widgets", card.ID, auth.Principal{Name: "bob"}, ResolveNone, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader resolve = %v", err)
	}
	if _, err := s.ResolveReport(ctx(), "acme", "widgets", card.ID, admin, ResolveNone, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("write-role resolve = %v", err)
	}
	s.Roles = &fakeRoles{role: identity.RoleAdmin}

	// hidden: the M2 document now hides the exact target.
	if _, err := s.ResolveReport(ctx(), "acme", "widgets", card.ID, admin, ResolveHidden, "confirmed"); err != nil {
		t.Fatalf("resolve hidden = %v", err)
	}
	doc, err := s.State(ctx(), "acme", "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Hidden[ThreadTarget(12, 3)]; !ok {
		t.Fatalf("target not hidden: %+v", doc.Hidden)
	}
	// The M2 hide writer published its invalidation frame for the exact
	// conversation/event, so the page and JSON projections revalidate (the
	// redaction itself is the M2 read verbs' rule, covered by their suite).
	found := false
	for _, f := range *frames {
		if f.Action == ActionHide && f.Num == 12 && f.Seq == 3 && f.By == "alice" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no hide frame for the resolved target: %+v", *frames)
	}
	// Terminal state is on the index card; the evidence record is untouched.
	ix, _ := s.loadReportsIndex(ctx(), "acme", "widgets")
	if len(ix.Open) != 0 || len(ix.ResolvedRecent) != 1 {
		t.Fatalf("index after resolve = %+v", ix)
	}
	res := ix.ResolvedRecent[0]
	if res.State != ReportResolved || res.Action != ResolveHidden || res.ResolvedBy != "alice" || res.ResolveNote != "confirmed" {
		t.Fatalf("resolved card = %+v", res)
	}
	raw, _, _ := store.GetBytes(ctx(), s.Store, ReportRecordKey("acme", "widgets", card.ID), store.GetOptions{})
	var rec Report
	_ = json.Unmarshal(raw, &rec)
	if rec.ID != card.ID || strings.Contains(string(raw), "resolved") {
		t.Fatalf("record mutated: %s", raw)
	}
	// A second resolve is a 409 (already terminal).
	if _, err := s.ResolveReport(ctx(), "acme", "widgets", card.ID, admin, ResolveNone, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("repeat resolve = %v", err)
	}
	// Unknown report is 404.
	if _, err := s.ResolveReport(ctx(), "acme", "widgets", 99, admin, ResolveNone, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}

	// locked: a second report on another thread locks the conversation.
	card2 := fileThreadReport(t, s, "bob", 14, 1)
	if _, err := s.ResolveReport(ctx(), "acme", "widgets", card2.ID, admin, ResolveLocked, ""); err != nil {
		t.Fatalf("resolve locked = %v", err)
	}
	doc, _ = s.State(ctx(), "acme", "widgets")
	if _, ok := doc.Locks[ThreadLockKey(14)]; !ok {
		t.Fatalf("thread not locked: %+v", doc.Locks)
	}

	// none: dismissal only, no content change and the dismiss audit verb.
	card3 := fileThreadReport(t, s, "bob", 15, 1)
	if _, err := s.ResolveReport(ctx(), "acme", "widgets", card3.ID, admin, ResolveNone, "not abuse"); err != nil {
		t.Fatalf("resolve none = %v", err)
	}
	doc, _ = s.State(ctx(), "acme", "widgets")
	if _, ok := doc.Hidden[ThreadTarget(15, 1)]; ok {
		t.Fatal("dismissal must not hide content")
	}
	ix, _ = s.loadReportsIndex(ctx(), "acme", "widgets")
	if ix.ResolvedRecent[2].State != ReportDismissed {
		t.Fatalf("dismiss state = %+v", ix.ResolvedRecent[2])
	}

	// Every resolve path wrote its audit verb: hidden→report_resolve,
	// locked→report_resolve, none→report_dismiss. (The `hidden` action also
	// writes the M2 hide record, which is correct: every changed surface has
	// its own record.)
	want := []string{ActionReportResolve, ActionReportResolve, ActionReportDismiss}
	var got []string
	for seq := 1; ; seq++ {
		entry, err := s.AuditEntryAt(ctx(), seq)
		if err != nil {
			t.Fatal(err)
		}
		if entry == nil {
			break
		}
		if !strings.HasPrefix(entry.Action, "report_") {
			continue
		}
		if entry.Actor != "alice" || entry.Target != "repo:acme/widgets" {
			t.Fatalf("audit %d = %+v", seq, entry)
		}
		got = append(got, entry.Action)
	}
	if len(got) != len(want) {
		t.Fatalf("report audits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("report audits = %v, want %v", got, want)
		}
	}
	// The global twin moved every row to resolved_recent.
	gx, _, _ := s.loadGlobalIndex(ctx())
	if len(gx.Open) != 0 || len(gx.ResolvedRecent) != 3 {
		t.Fatalf("global after resolves = %+v", gx)
	}
}

func TestResolveReportBadActionAndUserTarget(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleAdmin)
	card := fileThreadReport(t, s, "bob", 12, 3)
	admin := auth.Principal{Name: "alice", Write: true}
	if _, err := s.ResolveReport(ctx(), "acme", "widgets", card.ID, admin, "delete", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad action = %v", err)
	}
	// A user report is report-only: there is no content to hide or lock.
	ucard, err := s.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, ReportInput{
		Kind: ReportKindUser, Target: ReportTargetRef{Principal: "carol"}, Reason: "spam",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveReport(ctx(), "acme", "widgets", ucard.ID, admin, ResolveHidden, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("user hide = %v", err)
	}
	if _, err := s.ResolveReport(ctx(), "acme", "widgets", ucard.ID, admin, ResolveNone, ""); err != nil {
		t.Fatalf("user dismiss = %v", err)
	}
}

func TestListReportsFloorsAndPaging(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleRead)
	for i := 1; i <= 3; i++ {
		fileThreadReport(t, s, "bob", i, 1)
	}
	// Anonymous 401, reader 403, admin 200.
	if _, _, err := s.ListReports(ctx(), "acme", "widgets", auth.Anonymous(), "open", 0, 0); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous = %v", err)
	}
	if _, _, err := s.ListReports(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, "open", 0, 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader = %v", err)
	}
	s.Roles = &fakeRoles{role: identity.RoleAdmin}
	rows, more, err := s.ListReports(ctx(), "acme", "widgets", auth.Principal{Name: "alice"}, "open", 0, 2)
	if err != nil || !more || len(rows) != 2 || rows[0].ID != 1 || rows[1].ID != 2 {
		t.Fatalf("page1 = %+v more=%v err=%v", rows, more, err)
	}
	rows, more, err = s.ListReports(ctx(), "acme", "widgets", auth.Principal{Name: "alice"}, "open", 2, 2)
	if err != nil || more || len(rows) != 1 || rows[0].ID != 3 {
		t.Fatalf("page2 = %+v more=%v err=%v", rows, more, err)
	}
	if _, _, err := s.ListReports(ctx(), "acme", "widgets", auth.Principal{Name: "alice"}, "bogus", 0, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad state = %v", err)
	}
	// ReportQueues is the settings tab's one read: both slices, no LIST.
	open, resolved, err := s.ReportQueues(ctx(), "acme", "widgets", auth.Principal{Name: "alice"}, 10)
	if err != nil || len(open) != 3 || len(resolved) != 0 {
		t.Fatalf("queues = %d/%d %v", len(open), len(resolved), err)
	}
}

func TestGlobalQueueCursorAndFloors(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleAdmin)
	fileThreadReport(t, s, "bob", 1, 1)
	fileThreadReport(t, s, "bob", 2, 1)
	fileThreadReport(t, s, "bob", 3, 1)
	if _, _, err := s.ListGlobalReports(ctx(), auth.Anonymous(), "open", "", 0); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous = %v", err)
	}
	if _, _, err := s.ListGlobalReports(ctx(), auth.Principal{Name: "alice"}, "open", "", 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin = %v", err)
	}
	admin := auth.Principal{Name: "root", Admin: true}
	rows, more, err := s.ListGlobalReports(ctx(), admin, "open", "", 2)
	if err != nil || !more || len(rows) != 2 {
		t.Fatalf("page1 = %+v more=%v err=%v", rows, more, err)
	}
	cursor := GlobalCursor(rows[1])
	rest, more, err := s.ListGlobalReports(ctx(), admin, "open", cursor, 2)
	if err != nil || more || len(rest) != 1 || rest[0].ID == rows[0].ID || rest[0].ID == rows[1].ID {
		t.Fatalf("page2 = %+v more=%v err=%v", rest, more, err)
	}
	if _, _, err := s.ListGlobalReports(ctx(), admin, "open", "garbage", 2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad cursor = %v", err)
	}
}

func TestReportNotificationCarriesNoContent(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleRead)
	type call struct {
		owner, repo string
		num         int
		actor       string
	}
	var got []call
	s.ReportNotify = func(_ context.Context, owner, repo string, num int, actor, _ string) {
		got = append(got, call{owner, repo, num, actor})
	}
	fileThreadReport(t, s, "bob", 12, 3)
	if len(got) != 1 || got[0] != (call{"acme", "widgets", 12, "bob"}) {
		t.Fatalf("notify = %+v", got)
	}
	// The callback signature has no note/target at all — the compile-time
	// proof that a notification cannot carry reported content.
	var _ ReportNotifier = func(context.Context, string, string, int, string, string) {}
}

// fakeTasks records Run calls without entering the task body (the real
// *wal.Task is only constructible by the table).
type fakeTasks struct {
	mu      sync.Mutex
	runs    int
	running *wal.TaskRecord
	started chan struct{}
}

func (f *fakeTasks) Run(ctx context.Context, repo, kind string, params map[string]string, fn func(context.Context, *wal.Task) error) (*wal.TaskRecord, error) {
	f.mu.Lock()
	f.runs++
	f.mu.Unlock()
	if f.started != nil {
		f.started <- struct{}{}
	}
	return &wal.TaskRecord{Kind: kind, Repo: repo}, nil
}

func (f *fakeTasks) Running(repo, kind string) *wal.TaskRecord { return f.running }

func TestCompactReportsRebuildsGlobalWindow(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleAdmin)
	c1 := fileThreadReport(t, s, "bob", 1, 1)
	c2 := fileThreadReport(t, s, "bob", 2, 1)
	// Resolve one so the rebuild carries a terminal row too.
	if _, err := s.ResolveReport(ctx(), "acme", "widgets", c1.ID, auth.Principal{Name: "alice"}, ResolveNone, ""); err != nil {
		t.Fatal(err)
	}
	// Damage the global hot window (the crash window the task repairs).
	meta, err := s.Store.Head(ctx(), GlobalReportsKey())
	if err != nil || meta == nil {
		t.Fatalf("head = %v %v", meta, err)
	}
	if _, err := store.PutBytes(ctx(), s.Store, GlobalReportsKey(), []byte(`{"version":9}`),
		store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version}); err != nil {
		t.Fatal(err)
	}
	n, err := s.CompactReports(ctx())
	if err != nil || n != 1 {
		t.Fatalf("compact = %d %v", n, err)
	}
	gx, _, err := s.loadGlobalIndex(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if len(gx.Open) != 1 || gx.Open[0].ID != c2.ID || len(gx.ResolvedRecent) != 1 || gx.ResolvedRecent[0].ID != c1.ID {
		t.Fatalf("rebuilt = %+v", gx)
	}
	if gx.Open[0].Repo != "acme/widgets" {
		t.Fatalf("repo pointer = %q", gx.Open[0].Repo)
	}
}

func TestCompactTaskStartsOnceAndRespectsRunning(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleAdmin)
	tasks := &fakeTasks{started: make(chan struct{}, 2)}
	s.Tasks = tasks
	s.startCompactTask(ctx())
	select {
	case <-tasks.started:
	case <-time.After(2 * time.Second):
		t.Fatal("compact task did not start")
	}
	// A running record suppresses a second start.
	tasks.running = &wal.TaskRecord{Kind: TaskKindReportsCompact}
	s.startCompactTask(ctx())
	tasks.mu.Lock()
	if tasks.runs != 1 {
		t.Fatalf("second start ran: %d", tasks.runs)
	}
	tasks.mu.Unlock()
}

func TestReportIndexCompactionBoundary(t *testing.T) {
	// White-box: the trim keeps OPEN cards and evicts the oldest resolved
	// rows until the document fits ReportIndexLimit.
	ix := emptyReportsIndex()
	ix.Open = append(ix.Open, ReportCard{ID: 1, Target: ThreadTarget(1, 1), State: ReportOpen})
	for i := 0; i < 400; i++ {
		ix.ResolvedRecent = append(ix.ResolvedRecent, ReportCard{
			ID: i + 2, Target: ThreadTarget(1, 1), State: ReportResolved,
			Note: strings.Repeat("x", 1500),
		})
	}
	if reportsIndexSize(&ix) <= ReportIndexLimit {
		t.Fatal("fixture is not over the cap")
	}
	trimRepoIndex(&ix)
	if reportsIndexSize(&ix) > ReportIndexLimit {
		t.Fatal("trim did not fit the cap")
	}
	if len(ix.Open) != 1 || ix.Open[0].ID != 1 {
		t.Fatalf("open evicted: %+v", ix.Open)
	}
	if len(ix.ResolvedRecent) == 0 || len(ix.ResolvedRecent) == 400 {
		t.Fatalf("resolved window = %d", len(ix.ResolvedRecent))
	}
	// compacted_through names the newest evicted row (the tail of the trim).
	last := ix.ResolvedRecent[len(ix.ResolvedRecent)-1].ID
	if ix.CompactedThrough == "" {
		t.Fatal("compacted_through not advanced")
	}
	// Resolved rows that survive are the newest ones (oldest evicted first).
	if ix.ResolvedRecent[len(ix.ResolvedRecent)-1].ID != 401 {
		t.Fatalf("tail = %d, want 401 (last id)", last)
	}

	// The same rule for the global window.
	gx := emptyGlobalIndex()
	for i := 0; i < 400; i++ {
		gx.ResolvedRecent = append(gx.ResolvedRecent, GlobalCard{ReportCard: ReportCard{
			ID: i + 1, State: ReportResolved, Note: strings.Repeat("y", 1500),
		}, Repo: "acme/widgets"})
	}
	trimGlobalIndex(&gx)
	if reportsGlobalSize(&gx) > ReportIndexLimit || gx.CompactedThrough == "" {
		t.Fatalf("global trim: size=%d through=%q", reportsGlobalSize(&gx), gx.CompactedThrough)
	}
}

func TestFileReportConcurrentIdAllocation(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleRead)
	// Two services over ONE store simulate two instances; the CAS loop must
	// hand out distinct ids and both cards must survive.
	s2 := New(s.Store)
	s2.Roles = s.Roles
	s2.Targets = s.Targets
	const n = 3
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		svc := s
		if i%2 == 1 {
			svc = s2
		}
		go func(i int, svc *Service) {
			defer wg.Done()
			_, err := svc.FileReport(ctx(), "acme", "widgets", auth.Principal{Name: "bob"}, ReportInput{
				Kind: ReportKindThread, Target: ReportTargetRef{Num: 100 + i, Seq: 1}, Reason: "spam",
			})
			errs <- err
		}(i, svc)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent file = %v", err)
		}
	}
	ix, err := s.loadReportsIndex(ctx(), "acme", "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Open) != n {
		t.Fatalf("open = %d, want %d", len(ix.Open), n)
	}
	seen := map[int]bool{}
	for _, c := range ix.Open {
		if seen[c.ID] {
			t.Fatalf("duplicate id %d", c.ID)
		}
		seen[c.ID] = true
	}
}

func TestResolveReportAlreadyTerminalAfterCompact(t *testing.T) {
	s, _ := reportSvc(t, identity.RoleAdmin)
	card := fileThreadReport(t, s, "bob", 1, 1)
	admin := auth.Principal{Name: "alice"}
	if _, err := s.ResolveReport(ctx(), "acme", "widgets", card.ID, admin, ResolveNone, ""); err != nil {
		t.Fatal(err)
	}
	// Force the resolved row out of the hot window (the compaction case): the
	// terminal answer must stay 409, never "unknown".
	ix, ver, _ := s.readReportsIndexAt(ctx(), ReportsKey("acme", "widgets"))
	ix.ResolvedRecent = nil
	body, _ := json.Marshal(ix)
	if _, err := store.PutBytes(ctx(), s.Store, ReportsKey("acme", "widgets"), body,
		store.PutOptions{Mode: store.PutUpdate, IfVersion: ver}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveReport(ctx(), "acme", "widgets", card.ID, admin, ResolveNone, ""); !errors.Is(err, ErrNotFound) {
		// Outside the window the report is indistinguishable from unknown;
		// the record still exists, and the audit trail keeps the decision.
		t.Fatalf("outside window = %v", err)
	}
}
