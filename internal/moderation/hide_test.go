package moderation

// hide_test.go — M2 (PLAN-V2 §M.2/M.6): the repo moderation document, the
// hide/unhide/lock/unlock verbs, their floors, the one-writer PUT diff, the
// audit records (actor + target + previous state) and the SSE frame.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

type fakeRoles struct {
	role    identity.Role
	resErr  error
	readErr *auth.AuthError
}

func (f *fakeRoles) Resolve(context.Context, string, string, auth.Principal) (identity.Role, *identity.AccessDoc) {
	if f.resErr != nil {
		return "", nil
	}
	return f.role, nil
}

func (f *fakeRoles) CheckRead(context.Context, string, string, auth.Principal) *auth.AuthError {
	return f.readErr
}

type fakeProbe struct {
	keys map[string]bool
	err  error
}

func (p fakeProbe) Exists(_ context.Context, owner, repo string, t Target) (bool, error) {
	if p.err != nil {
		return false, p.err
	}
	switch t.Kind {
	case TargetThread:
		return p.keys["thread"], nil
	case TargetReview:
		return p.keys["review"], nil
	case TargetReviewThread:
		return p.keys["review_thread"], nil
	}
	return false, nil
}

func hideSvc(t *testing.T, role identity.Role) (*Service, *fakeRoles, *[]StreamEvent) {
	s, _ := testService(t)
	r := &fakeRoles{role: role}
	s.Roles = r
	events := &[]StreamEvent{}
	s.Stream = func(_ context.Context, ev StreamEvent) { *events = append(*events, ev) }
	return s, r, events
}

func TestTargetGrammar(t *testing.T) {
	cases := []struct {
		target string
		want   Target
	}{
		{ThreadTarget(1, 2), Target{Kind: TargetThread, Num: 1, Seq: 2}},
		{ReviewTarget(0x2a, 0x1f), Target{Kind: TargetReview, Num: 42, Seq: 31}},
		{ReviewThreadTarget(7, "deadbeef", 3), Target{Kind: TargetReviewThread, Num: 7, Seq: 3, TID: "deadbeef"}},
		// Minimum widths grow naturally; seq 0 is the opened event (the
		// thread/PR description).
		{"thread:1234567:event:000000000001", Target{Kind: TargetThread, Num: 0x1234567, Seq: 1}},
		{"thread:000001:event:000000000000", Target{Kind: TargetThread, Num: 1, Seq: 0}},
	}
	for _, c := range cases {
		got, err := ParseTarget(c.target)
		if err != nil || got != c.want {
			t.Fatalf("ParseTarget(%q) = (%+v, %v), want %+v", c.target, got, err, c.want)
		}
	}
	for _, bad := range []string{
		"", "thread", "thread:1:event:1", "thread:000001:ev:000000000001",
		"thread:000001:event:00000000000g", "review:000001", "review:000001:000000000000:1",
		"review_thread:000001:xyz:000000000001", "review_thread:000001:deadbeef:1",
		"user:alice", "THREAD:000001:event:000000000001", "thread:000000:event:000000000001",
	} {
		if _, err := ParseTarget(bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ParseTarget(%q) err = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestHideUnhideIdempotentAndAudit(t *testing.T) {
	s, _, events := hideSvc(t, identity.RoleTriage)
	target := ThreadTarget(3, 7)

	v1, changed, err := s.Hide(ctx(), "acme", "widget", auth.Principal{Name: "Alice"}, target, "spam")
	if err != nil || !changed || v1 != 1 {
		t.Fatalf("hide = (%d, %v, %v)", v1, changed, err)
	}
	doc, err := s.State(ctx(), "acme", "widget")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Version != 1 || doc.UpdatedAt == "" {
		t.Fatalf("doc = %+v", doc)
	}
	if h := doc.Hidden[target]; h.By != "alice" || h.Reason != "spam" || h.At == "" {
		t.Fatalf("hidden = %+v", h)
	}
	// Idempotent: no version move, no second audit, no second frame.
	v2, changed, err := s.Hide(ctx(), "acme", "widget", auth.Principal{Name: "bob"}, target, "spam")
	if err != nil || changed || v2 != v1 {
		t.Fatalf("repeat hide = (%d, %v, %v)", v2, changed, err)
	}
	if got := auditCount(t, s); got != 1 {
		t.Fatalf("audit = %d, want 1", got)
	}
	if len(*events) != 1 {
		t.Fatalf("frames = %d, want 1", len(*events))
	}
	e, err := s.AuditEntryAt(ctx(), 1)
	if err != nil || e == nil {
		t.Fatalf("audit 1 = (%v, %v)", e, err)
	}
	if e.Actor != "alice" || e.Action != ActionHide || e.Target != "repo:acme/widget" ||
		e.Reason != "spam" || e.Context != "target="+target+";prev=visible" {
		t.Fatalf("audit = %+v", e)
	}

	// Unhide is idempotent too, and its audit names the previous state.
	if _, changed, err := s.Unhide(ctx(), "acme", "widget", auth.Principal{Name: "carol"}, target); err != nil || !changed {
		t.Fatalf("unhide = (%v, %v)", changed, err)
	}
	if _, changed, err := s.Unhide(ctx(), "acme", "widget", auth.Principal{Name: "carol"}, target); err != nil || changed {
		t.Fatalf("repeat unhide = (%v, %v)", changed, err)
	}
	e, _ = s.AuditEntryAt(ctx(), 2)
	if e == nil || e.Action != ActionUnhide || e.Context != "target="+target+";prev=hidden" {
		t.Fatalf("unhide audit = %+v", e)
	}
	if got := auditCount(t, s); got != 2 {
		t.Fatalf("audit total = %d, want 2", got)
	}
}

func TestHideReasonAndTargetValidation(t *testing.T) {
	s, _, _ := hideSvc(t, identity.RoleTriage)
	actor := auth.Principal{Name: "alice"}
	if _, _, err := s.Hide(ctx(), "acme", "widget", actor, ThreadTarget(1, 1), "malicious"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad reason err = %v", err)
	}
	if _, _, err := s.Hide(ctx(), "acme", "widget", actor, "user:alice", "spam"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad target err = %v", err)
	}
	// Blank reason defaults to "other".
	if _, _, err := s.Hide(ctx(), "acme", "widget", actor, ThreadTarget(1, 1), ""); err != nil {
		t.Fatal(err)
	}
	doc, _ := s.State(ctx(), "acme", "widget")
	if doc.Hidden[ThreadTarget(1, 1)].Reason != DefaultHiddenReason {
		t.Fatalf("default reason = %q", doc.Hidden[ThreadTarget(1, 1)].Reason)
	}
}

func TestModerationFloors(t *testing.T) {
	actor := auth.Principal{Name: "alice"}
	cases := []struct {
		name  string
		role  identity.Role
		verb  func(*Service) error
		want  error
		allow bool
	}{
		{"read hide", identity.RoleRead, func(s *Service) error {
			_, _, err := s.Hide(ctx(), "acme", "widget", actor, ThreadTarget(1, 1), "spam")
			return err
		}, ErrForbidden, false},
		{"triage hide", identity.RoleTriage, func(s *Service) error {
			_, _, err := s.Hide(ctx(), "acme", "widget", actor, ThreadTarget(1, 1), "spam")
			return err
		}, nil, true},
		{"triage lock", identity.RoleTriage, func(s *Service) error {
			_, _, err := s.LockThread(ctx(), "acme", "widget", 1, actor, "calm down", LockModeAll)
			return err
		}, ErrForbidden, false},
		{"maintain lock", identity.RoleMaintain, func(s *Service) error {
			_, _, err := s.LockThread(ctx(), "acme", "widget", 1, actor, "calm down", LockModeAll)
			return err
		}, nil, true},
		{"write unlock", identity.RoleWrite, func(s *Service) error {
			_, _, err := s.UnlockThread(ctx(), "acme", "widget", 1, actor)
			return err
		}, ErrForbidden, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, roles, _ := hideSvc(t, c.role)
			roles.role = c.role
			err := c.verb(s)
			if c.allow && err != nil {
				t.Fatalf("allowed verb failed: %v", err)
			}
			if !c.allow && !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
	// Anonymous is a real 401, not a 403.
	s, _, _ := hideSvc(t, identity.Role(""))
	_, _, err := s.Hide(ctx(), "acme", "widget", auth.Anonymous(), ThreadTarget(1, 1), "spam")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous err = %v", err)
	}
	// Host admins always pass.
	_, _, err = s.LockThread(ctx(), "acme", "widget", 1, auth.Principal{Name: "root", Admin: true}, "ops", LockModeAll)
	if err != nil {
		t.Fatalf("admin lock = %v", err)
	}
}

func TestThreadLockComposesWithRepoWideLock(t *testing.T) {
	s, _, _ := hideSvc(t, identity.RoleMaintain)
	actor := auth.Principal{Name: "alice"}
	_, changed, err := s.LockThread(ctx(), "acme", "widget", 4, actor, "flame war", LockModeAll)
	if err != nil || !changed {
		t.Fatalf("lock = (%v, %v)", changed, err)
	}
	if _, changed, err := s.LockThread(ctx(), "acme", "widget", 4, actor, "different", LockModeAll); err != nil || changed {
		t.Fatalf("repeat lock = (%v, %v)", changed, err)
	}
	locked, _, reason, rev, err := s.Snapshot(ctx(), "acme", "widget", 4, nil)
	if err != nil || !locked || reason != "flame war" || rev != 1 {
		t.Fatalf("snapshot = (%v, %q, %d, %v)", locked, reason, rev, err)
	}
	// Another conversation is unaffected.
	if locked, _, _, _, _ := s.Snapshot(ctx(), "acme", "widget", 5, nil); locked {
		t.Fatal("lock leaked to another conversation")
	}
	if _, changed, err := s.UnlockThread(ctx(), "acme", "widget", 4, actor); err != nil || !changed {
		t.Fatalf("unlock = (%v, %v)", changed, err)
	}
	if _, changed, err := s.UnlockThread(ctx(), "acme", "widget", 4, actor); err != nil || changed {
		t.Fatalf("repeat unlock = (%v, %v)", changed, err)
	}
	if locked, _, _, _, _ := s.Snapshot(ctx(), "acme", "widget", 4, nil); locked {
		t.Fatal("still locked after unlock")
	}
	// The plan's flat envelope composes: a repo-wide lock locks every thread.
	doc, _ := s.State(ctx(), "acme", "widget")
	doc.Locked, doc.LockReason = true, "repo freeze"
	if _, err := s.Apply(ctx(), "acme", "widget", actor, doc); err != nil {
		t.Fatal(err)
	}
	if locked, _, reason, _, _ := s.Snapshot(ctx(), "acme", "widget", 9, nil); !locked || reason != "repo freeze" {
		t.Fatalf("repo-wide lock = (%v, %q)", locked, reason)
	}
}

func TestApplyDiffsFloorsAndCAS(t *testing.T) {
	s, roles, _ := hideSvc(t, identity.RoleTriage)
	actor := auth.Principal{Name: "alice"}
	target := ReviewTarget(2, 3)

	// Create with a hide (triage floor suffices).
	doc, _ := s.State(ctx(), "acme", "widget")
	if doc.Version != 0 {
		t.Fatalf("absent doc version = %d, want 0", doc.Version)
	}
	doc.Hidden[target] = Hidden{By: "forged", At: "forged", Reason: "spam"}
	v, err := s.Apply(ctx(), "acme", "widget", actor, doc)
	if err != nil || v != 1 {
		t.Fatalf("apply hide = (%d, %v)", v, err)
	}
	got, _ := s.State(ctx(), "acme", "widget")
	// The caller's by/at are evidence-forgery attempts: the server stamps them.
	if got.Hidden[target].By != "alice" || got.Hidden[target].At == "forged" {
		t.Fatalf("hidden = %+v", got.Hidden[target])
	}

	// Stale version is 409: another writer moves the document first.
	stale := got
	if _, _, err := s.Hide(ctx(), "acme", "widget", actor, ThreadTarget(9, 9), "spam"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx(), "acme", "widget", actor, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale apply err = %v", err)
	}
	got, _ = s.State(ctx(), "acme", "widget")

	// A lock delta needs maintain; triage is refused.
	want := got
	want.Locked = true
	want.LockReason = "freeze"
	if _, err := s.Apply(ctx(), "acme", "widget", actor, want); !errors.Is(err, ErrForbidden) {
		t.Fatalf("triage lock err = %v", err)
	}
	roles.role = identity.RoleMaintain
	if _, err := s.Apply(ctx(), "acme", "widget", actor, want); err != nil {
		t.Fatalf("maintain lock = %v", err)
	}

	// A hidden-removal delta now needs triage (still maintain, so fine).
	cur, _ := s.State(ctx(), "acme", "widget")
	delete(cur.Hidden, target)
	if _, err := s.Apply(ctx(), "acme", "widget", actor, cur); err != nil {
		t.Fatalf("unhide apply = %v", err)
	}
	after, _ := s.State(ctx(), "acme", "widget")
	if _, ok := after.Hidden[target]; ok {
		t.Fatal("hidden entry survived the removal delta")
	}
	if after.Locked != true || after.LockedBy != "alice" {
		t.Fatalf("lock fields = %+v", after)
	}
	// The audit trail has one record per changed family with prev state.
	if got := auditCount(t, s); got != 4 {
		t.Fatalf("audit = %d, want 4 (hide, lock, unhide… )", got)
	}
	var sawHidePrev, sawLockPrev, sawUnhidePrev bool
	for seq := 1; ; seq++ {
		e, err := s.AuditEntryAt(ctx(), seq)
		if err != nil || e == nil {
			break
		}
		switch {
		case e.Action == ActionHide && e.Context == "target="+target+";prev=visible":
			sawHidePrev = true
		case e.Action == ActionLock && e.Context == "scope=repo;mode=all;prev=unlocked":
			sawLockPrev = true
		case e.Action == ActionUnhide && e.Context == "target="+target+";prev=hidden":
			sawUnhidePrev = true
		}
	}
	if !sawHidePrev || !sawLockPrev || !sawUnhidePrev {
		t.Fatalf("audit prev states = hide:%v lock:%v unhide:%v", sawHidePrev, sawLockPrev, sawUnhidePrev)
	}

	// A bad lock key is refused before any write.
	cur, _ = s.State(ctx(), "acme", "widget")
	cur.Locks["bogus"] = LockState{By: "x", Reason: "y"}
	if _, err := s.Apply(ctx(), "acme", "widget", actor, cur); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad lock key err = %v", err)
	}
	if _, err := s.Apply(ctx(), "acme", "widget", actor, Doc{Version: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative version err = %v", err)
	}
	// No-op apply (identical doc) is a changed=false success.
	same, _ := s.State(ctx(), "acme", "widget")
	if _, err := s.Apply(ctx(), "acme", "widget", actor, same); err != nil {
		t.Fatalf("no-op apply = %v", err)
	}
}

func TestApplyProbesTargets(t *testing.T) {
	s, _, _ := hideSvc(t, identity.RoleTriage)
	s.Targets = fakeProbe{keys: map[string]bool{"review": false}}
	doc, _ := s.State(ctx(), "acme", "widget")
	doc.Hidden[ReviewTarget(1, 1)] = Hidden{Reason: "spam"}
	if _, err := s.Apply(ctx(), "acme", "widget", auth.Principal{Name: "alice"}, doc); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing target err = %v", err)
	}
	s.Targets = fakeProbe{err: errors.New("store down")}
	if _, _, err := s.Hide(ctx(), "acme", "widget", auth.Principal{Name: "alice"}, ReviewTarget(1, 1), "spam"); err == nil {
		t.Fatal("probe error was swallowed")
	}
}

func TestSnapshotVisitsAndDocForReadGate(t *testing.T) {
	s, _, _ := hideSvc(t, identity.RoleTriage)
	actor := auth.Principal{Name: "alice"}
	for _, target := range []string{ThreadTarget(1, 2), ReviewTarget(1, 1)} {
		if _, _, err := s.Hide(ctx(), "acme", "widget", actor, target, "abusive"); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	locked, _, _, rev, err := s.Snapshot(ctx(), "acme", "widget", 1, func(target, by, at, reason string) {
		seen = append(seen, target+"|"+by+"|"+reason)
	})
	if err != nil || locked || rev != 2 {
		t.Fatalf("snapshot = (%v, %d, %v)", locked, rev, err)
	}
	if len(seen) != 2 || seen[0] != ReviewTarget(1, 1)+"|alice|abusive" || seen[1] != ThreadTarget(1, 2)+"|alice|abusive" {
		t.Fatalf("visited = %v", seen)
	}

	// DocFor is read-gated: the identity hook's refusal is the API's 403/401.
	_, roles, _ := hideSvc(t, identity.RoleRead)
	roles.readErr = &auth.AuthError{Kind: auth.ErrForbidden, Why: "read access required"}
	if err := roles.CheckRead(ctx(), "", "", auth.Principal{}); err == nil {
		t.Fatal("fake read gate did not refuse")
	}
	s.Roles = roles
	if _, err := s.DocFor(ctx(), "acme", "widget", auth.Principal{Name: "bob"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-gated doc err = %v", err)
	}
	roles.readErr = nil
	if _, err := s.DocFor(ctx(), "acme", "widget", auth.Principal{Name: "bob"}); err != nil {
		t.Fatalf("read-gated doc = %v", err)
	}
	// Anonymous with no Roles and a public read is allowed.
	s.Roles = nil
	if _, err := s.DocFor(ctx(), "acme", "widget", auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous doc err = %v", err)
	}
}

func TestStreamFrameState(t *testing.T) {
	cases := map[string]string{
		ActionHide: "hidden", ActionUnhide: "visible",
		ActionLock: "locked", ActionUnlock: "unlocked", "apply": "apply",
	}
	for action, want := range cases {
		if got := (StreamEvent{Action: action}).State(); got != want {
			t.Fatalf("State(%q) = %q, want %q", action, got, want)
		}
	}
	// A frame carries the action and the resulting state, never a body.
	ev := StreamEvent{Action: ActionHide, Num: 3, Seq: 9}
	if ev.State() != "hidden" || ev.Action != ActionHide {
		t.Fatalf("frame = %+v", ev)
	}
}

func TestDocCorruptFailsClosed(t *testing.T) {
	s, _, _ := hideSvc(t, identity.RoleTriage)
	if _, err := store.PutBytes(ctx(), s.Store, ModerationKey("acme", "widget"), []byte("{"), store.PutOptions{ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.Snapshot(ctx(), "acme", "widget", 1, nil); err == nil {
		t.Fatal("corrupt doc read as empty")
	}
	// Unknown future version is refused rather than guessed.
	if _, err := store.PutBytes(ctx(), s.Store, ModerationKey("acme", "bad"), []byte(`{"version":99}`), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.Snapshot(ctx(), "acme", "bad", 1, nil); err == nil {
		t.Fatal("future version read as current")
	}
}

// --- HTTP surface -------------------------------------------------------------

func repoHandler(t *testing.T, role identity.Role) (*Handler, *Service) {
	t.Helper()
	s, _ := testService(t)
	s.Roles = &fakeRoles{role: role}
	h := &Handler{Svc: s, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "alice"}, nil
	}}
	return h, s
}

func TestRepoModerationRoutes(t *testing.T) {
	h, _ := repoHandler(t, identity.RoleTriage)
	req := httptest.NewRequest(http.MethodGet, "/acme/widgets/api/moderation", nil)
	rec := httptest.NewRecorder()
	if !h.Handle(rec, req) || rec.Code != http.StatusOK {
		t.Fatalf("GET = %d (handled=%v)", rec.Code, rec.Code != http.StatusNotFound)
	}
	var doc Doc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != 0 || doc.Hidden == nil || doc.Locks == nil {
		t.Fatalf("absent doc = %+v", doc)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q", cc)
	}
	// Both lanes answer; an unrelated repo path falls through.
	browser := httptest.NewRequest(http.MethodGet, "/acme/widgets/api-browser/moderation", nil)
	if rec := httptest.NewRecorder(); !h.Handle(rec, browser) || rec.Code != http.StatusOK {
		t.Fatalf("browser lane = %d", rec.Code)
	}
	other := httptest.NewRequest(http.MethodGet, "/acme/widgets/api/issues", nil)
	if rec := httptest.NewRecorder(); h.Handle(rec, other) {
		t.Fatal("unrelated repo route was swallowed")
	}
	// Method not allowed.
	rec = httptest.NewRecorder()
	if !h.Handle(rec, httptest.NewRequest(http.MethodPost, "/acme/widgets/api/moderation", nil)) || rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rec.Code)
	}
}

func TestRepoModerationPutFloorsAndBodies(t *testing.T) {
	put := func(h *Handler, body, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/acme/widgets/api/moderation", strings.NewReader(body))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h.Handle(rec, req)
		return rec
	}
	h, s := repoHandler(t, identity.RoleTriage)
	// Cross-origin is refused; same-origin is accepted.
	if rec := put(h, `{}`, "https://evil.example"); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin = %d", rec.Code)
	}
	// Unknown fields are 400 (fail closed).
	if rec := put(h, `{"bogus":1}`, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", rec.Code)
	}
	// Hide delta at triage.
	body := `{"version":0,"hidden":{"thread:000001:event:000000000002":{"reason":"spam"}}}`
	if rec := put(h, body, "http://"+h.testHost()); rec.Code != http.StatusOK {
		t.Fatalf("hide put = %d: %s", rec.Code, rec.Body.String())
	}
	// A stale version is 409.
	rec := put(h, body, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale = %d: %s", rec.Code, rec.Body.String())
	}
	// A lock delta at triage is 403.
	doc, _ := s.State(ctx(), "acme", "widgets")
	lock := `{"version":` + strconv.Itoa(doc.Version) + `,"locked":true,"lock_reason":"freeze"}`
	if rec := put(h, lock, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("triage lock = %d", rec.Code)
	}
	// Anonymous is a real 401.
	h.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) { return auth.Anonymous(), nil }
	if rec := put(h, `{"version":1}`, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", rec.Code)
	}
}

// testHost is the request host the same-origin check compares against.
func (h *Handler) testHost() string { return "example.com" }

func TestRepoModerationGetFailsClosed(t *testing.T) {
	s, _ := testService(t)
	s.Roles = &fakeRoles{readErr: &auth.AuthError{Kind: auth.ErrForbidden, Why: "read access required"}}
	h := &Handler{Svc: s, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "bob"}, nil
	}}
	rec := do(h, "GET", "/acme/widgets/api/moderation")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-gated GET = %d", rec.Code)
	}
	// Anonymous, no Roles seam: 401.
	s.Roles = nil
	h.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) { return auth.Anonymous(), nil }
	if rec := do(h, "GET", "/acme/widgets/api/moderation"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET = %d", rec.Code)
	}
}

// --- CAS + cache integrity -----------------------------------------------------

func TestHideCASRetriesThenSucceeds(t *testing.T) {
	fs := &failingStore{ObjectStore: store.NewMemory(), failPut: 2}
	s := New(fs)
	s.Roles = &fakeRoles{role: identity.RoleTriage}
	if _, changed, err := s.Hide(ctx(), "acme", "widget", auth.Principal{Name: "alice"}, ThreadTarget(1, 1), "spam"); err != nil || !changed {
		t.Fatalf("hide with CAS contention = (%v, %v)", changed, err)
	}
}

func TestHideCASExhaustion(t *testing.T) {
	fs := &failingStore{ObjectStore: store.NewMemory(), failPut: 100}
	s := New(fs)
	s.Roles = &fakeRoles{role: identity.RoleTriage}
	if _, _, err := s.Hide(ctx(), "acme", "widget", auth.Principal{Name: "alice"}, ThreadTarget(1, 1), "spam"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestDocCacheIsNotMutableByCallers(t *testing.T) {
	s, _, _ := hideSvc(t, identity.RoleTriage)
	actor := auth.Principal{Name: "alice"}
	if _, _, err := s.Hide(ctx(), "acme", "widget", actor, ThreadTarget(1, 1), "spam"); err != nil {
		t.Fatal(err)
	}
	first, _ := s.State(ctx(), "acme", "widget")
	delete(first.Hidden, ThreadTarget(1, 1))
	second, _ := s.State(ctx(), "acme", "widget")
	if _, ok := second.Hidden[ThreadTarget(1, 1)]; !ok {
		t.Fatal("caller mutated the cached document")
	}
	// A nil-stream service still hides (the frame seam is optional).
	s.Stream = nil
	if _, _, err := s.Hide(ctx(), "acme", "widget", actor, ThreadTarget(1, 2), "spam"); err != nil {
		t.Fatal(err)
	}
}

func TestLockKeyValidationAndAuditDrop(t *testing.T) {
	s, _, _ := hideSvc(t, identity.RoleMaintain)
	actor := auth.Principal{Name: "alice"}
	// A valid added lock key stores a stamped entry (by/at server-side).
	doc, _ := s.State(ctx(), "acme", "widget")
	doc.Locks[ThreadLockKey(2)] = LockState{By: "forged", Reason: "freeze"}
	if _, err := s.Apply(ctx(), "acme", "widget", actor, doc); err != nil {
		t.Fatal(err)
	}
	got, _ := s.State(ctx(), "acme", "widget")
	if got.Locks[ThreadLockKey(2)].By != "alice" {
		t.Fatalf("lock entry = %+v", got.Locks[ThreadLockKey(2)])
	}
	for _, bad := range []string{"", "thread", "thread:zzzzzz", "lock:000001", "thread:000000"} {
		if _, ok := parseThreadLockKey(bad); ok {
			t.Fatalf("parseThreadLockKey(%q) accepted", bad)
		}
	}
	if n, ok := parseThreadLockKey("thread:00002a"); !ok || n != 42 {
		t.Fatalf("parseThreadLockKey = (%d, %v)", n, ok)
	}
	// Audit failure is logged, never fatal (the block precedent).
	log := &captureLogger{}
	s.Logger = log
	if _, err := store.PutBytes(ctx(), s.Store, AuditStateKey(), []byte("{bad"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	before := len(log.msgs)
	if _, _, err := s.Hide(ctx(), "acme", "widget", actor, ThreadTarget(3, 3), "spam"); err != nil {
		t.Fatalf("hide with broken audit = %v", err)
	}
	if len(log.msgs) != before+1 {
		t.Fatalf("warnings = %v", log.msgs)
	}
}

// --- remaining floor/branch arms ---------------------------------------------

func TestFloorsWithNilRolesFallback(t *testing.T) {
	s := New(store.NewMemory())
	// Write passes a triage floor, fails the maintain floor.
	if _, _, err := s.Hide(ctx(), "acme", "w", auth.Principal{Name: "w", Write: true}, ThreadTarget(1, 1), "spam"); err != nil {
		t.Fatalf("write hide = %v", err)
	}
	if _, _, err := s.LockThread(ctx(), "acme", "w", 1, auth.Principal{Name: "w", Write: true}, "r", LockModeAll); !errors.Is(err, ErrForbidden) {
		t.Fatalf("write lock = %v", err)
	}
	// A plain authenticated principal is read: refused.
	if _, _, err := s.Unhide(ctx(), "acme", "w", auth.Principal{Name: "u"}, ThreadTarget(1, 1)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read unhide = %v", err)
	}
	// Anonymous is 401.
	if _, _, err := s.UnlockThread(ctx(), "acme", "w", 1, auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous unlock = %v", err)
	}
	// Bad conversation numbers are 400 at the floor-passing path.
	if _, _, err := s.LockThread(ctx(), "acme", "w", 0, auth.Principal{Name: "a", Admin: true}, "r", LockModeAll); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero lock = %v", err)
	}
	if _, _, err := s.UnlockThread(ctx(), "acme", "w", 0, auth.Principal{Name: "a", Admin: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero unlock = %v", err)
	}
}

func TestRequireReadArms(t *testing.T) {
	s := New(store.NewMemory())
	// Write short-circuits the read gate.
	if err := s.requireRead(ctx(), "o", "r", auth.Principal{Name: "w", Write: true}); err != nil {
		t.Fatalf("write read = %v", err)
	}
	s.Roles = &fakeRoles{readErr: &auth.AuthError{Kind: auth.ErrUnavailable, Why: "store down"}}
	if err := s.requireRead(ctx(), "o", "r", auth.Principal{Name: "u"}); err == nil || !strings.Contains(err.Error(), "identity unavailable") {
		t.Fatalf("unavailable = %v", err)
	}
	s.Roles = &fakeRoles{readErr: &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "auth required"}}
	if err := s.requireRead(ctx(), "o", "r", auth.Principal{Name: "u"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unauthorized = %v", err)
	}
	if !errors.Is(requireRoleErr(s, auth.Anonymous()), ErrUnauthorized) {
		t.Fatal("anonymous default arm")
	}
}

// requireRoleErr exercises requireRole's nil-Roles anonymous arm directly.
func requireRoleErr(s *Service, p auth.Principal) error {
	return s.requireRole(ctx(), "o", "r", p, identity.RoleTriage)
}

func TestRoleRankTable(t *testing.T) {
	want := map[identity.Role]int{
		identity.RoleRead: 1, identity.RoleTriage: 2, identity.RoleWrite: 3,
		identity.RoleMaintain: 4, identity.RoleAdmin: 5, "": 0, "bogus": 0,
	}
	for role, n := range want {
		if got := roleRank(role); got != n {
			t.Fatalf("roleRank(%q) = %d, want %d", role, got, n)
		}
	}
}

func TestStreamStampsTime(t *testing.T) {
	s, _, _ := hideSvc(t, identity.RoleTriage)
	var got StreamEvent
	s.Stream = func(_ context.Context, ev StreamEvent) { got = ev }
	s.stream(ctx(), "acme", "widget", StreamEvent{Action: ActionLock})
	if got.At == "" || got.Repo != "acme/widget" {
		t.Fatalf("frame = %+v", got)
	}
}

func TestApplyLockAddRemoveAndFlatUnlock(t *testing.T) {
	s, _, _ := hideSvc(t, identity.RoleMaintain)
	actor := auth.Principal{Name: "alice"}
	// Add a per-conversation lock and the repo-wide envelope.
	doc, _ := s.State(ctx(), "acme", "widget")
	doc.Locks[ThreadLockKey(3)] = LockState{Reason: "cooldown"}
	doc.Locked, doc.LockReason = true, "repo freeze"
	if _, err := s.Apply(ctx(), "acme", "widget", actor, doc); err != nil {
		t.Fatal(err)
	}
	// Remove both: the flat unlock arm and the lock-removal audit.
	cur, _ := s.State(ctx(), "acme", "widget")
	delete(cur.Locks, ThreadLockKey(3))
	cur.Locked, cur.LockReason = false, ""
	if _, err := s.Apply(ctx(), "acme", "widget", actor, cur); err != nil {
		t.Fatal(err)
	}
	after, _ := s.State(ctx(), "acme", "widget")
	if len(after.Locks) != 0 || after.Locked || after.LockedBy != "" {
		t.Fatalf("after unlock = %+v", after)
	}
	var sawLockRemove, sawUnlock bool
	for seq := 1; ; seq++ {
		e, err := s.AuditEntryAt(ctx(), seq)
		if err != nil || e == nil {
			break
		}
		if e.Action == ActionUnlock && strings.HasPrefix(e.Context, "lock=thread:000003") {
			sawLockRemove = true
		}
		if e.Action == ActionUnlock && e.Context == "scope=repo;mode=;prev=locked" {
			sawUnlock = true
		}
	}
	if !sawLockRemove || !sawUnlock {
		t.Fatalf("audit = lock-remove:%v unlock:%v", sawLockRemove, sawUnlock)
	}
}

func TestApplyErrorArms(t *testing.T) {
	s, _, _ := hideSvc(t, identity.RoleRead)
	actor := auth.Principal{Name: "alice"}
	// A hidden delta at read is 403.
	doc, _ := s.State(ctx(), "acme", "widget")
	doc.Hidden[ThreadTarget(1, 1)] = Hidden{Reason: "spam"}
	if _, err := s.Apply(ctx(), "acme", "widget", actor, doc); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read hide delta = %v", err)
	}
	// A malformed key is 400 even at the triage floor.
	s.Roles = &fakeRoles{role: identity.RoleTriage}
	doc, _ = s.State(ctx(), "acme", "widget")
	doc.Hidden["bogus"] = Hidden{Reason: "spam"}
	if _, err := s.Apply(ctx(), "acme", "widget", actor, doc); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad key = %v", err)
	}
	// A probe error propagates.
	s.Targets = fakeProbe{err: errors.New("store down")}
	doc, _ = s.State(ctx(), "acme", "widget")
	doc.Hidden[ThreadTarget(2, 2)] = Hidden{Reason: "spam"}
	if _, err := s.Apply(ctx(), "acme", "widget", actor, doc); err == nil {
		t.Fatal("probe error swallowed")
	}
	// A corrupt document fails the apply closed.
	if _, err := store.PutBytes(ctx(), s.Store, ModerationKey("acme", "widget"), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx(), "acme", "widget", actor, Doc{}); err == nil {
		t.Fatal("corrupt apply read as empty")
	}
	// A future version document is refused (never guessed).
	if _, err := store.PutBytes(ctx(), s.Store, ModerationKey("acme", "future"), []byte(`{"version":99}`), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx(), "acme", "future", actor, Doc{}); err == nil {
		t.Fatal("future version apply succeeded")
	}
}

// getFailStore fails every Get (the loadDoc error arm).
type getFailStore struct{ store.ObjectStore }

func (g getFailStore) Get(context.Context, string, store.GetOptions) (store.GetResult, error) {
	return nil, errors.New("get down")
}

func TestLoadErrorsFailClosed(t *testing.T) {
	s := New(getFailStore{ObjectStore: store.NewMemory()})
	s.Roles = &fakeRoles{role: identity.RoleTriage}
	if _, _, _, _, err := s.Snapshot(ctx(), "acme", "widget", 1, nil); err == nil {
		t.Fatal("snapshot read error swallowed")
	}
	if _, err := s.Apply(ctx(), "acme", "widget", auth.Principal{Name: "a"}, Doc{}); err == nil {
		t.Fatal("apply read error swallowed")
	}
	if _, _, err := s.Hide(ctx(), "acme", "widget", auth.Principal{Name: "a"}, ThreadTarget(1, 1), "spam"); err == nil {
		t.Fatal("hide read error swallowed")
	}
}

func TestMutateNonPreconditionError(t *testing.T) {
	s := New(putErrStore{ObjectStore: store.NewMemory()})
	s.Roles = &fakeRoles{role: identity.RoleTriage}
	// The doc Put happens first; its error must surface (not a silent allow).
	if _, _, err := s.Hide(ctx(), "acme", "widget", auth.Principal{Name: "a"}, ThreadTarget(1, 1), "spam"); err == nil {
		t.Fatal("put error swallowed")
	}
}

func TestHideProbeRefusalAndWriteErrors(t *testing.T) {
	s, _, _ := hideSvc(t, identity.RoleTriage)
	s.Targets = fakeProbe{keys: map[string]bool{"thread": false}}
	actor := auth.Principal{Name: "alice"}
	if _, _, err := s.Hide(ctx(), "acme", "widget", actor, ThreadTarget(1, 1), "spam"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("probe refusal = %v", err)
	}
	// Every mutating verb surfaces a write error (no silent allow). The
	// seeded document gives unhide/unlock real deltas to attempt.
	mem := store.NewMemory()
	seed := `{"version":1,"locks":{"thread:000001":{"by":"x","at":"t","reason":"r"}},"hidden":{"` +
		ThreadTarget(1, 1) + `":{"by":"x","at":"t","reason":"spam"}}}`
	if _, err := store.PutBytes(ctx(), mem, ModerationKey("acme", "widget"), []byte(seed), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	broken := New(putErrStore{ObjectStore: mem})
	broken.Roles = &fakeRoles{role: identity.RoleMaintain}
	if _, _, err := broken.Unhide(ctx(), "acme", "widget", actor, ThreadTarget(1, 1)); err == nil {
		t.Fatal("unhide write error swallowed")
	}
	if _, _, err := broken.LockThread(ctx(), "acme", "widget", 2, actor, "r", LockModeAll); err == nil {
		t.Fatal("lock write error swallowed")
	}
	if _, _, err := broken.UnlockThread(ctx(), "acme", "widget", 1, actor); err == nil {
		t.Fatal("unlock write error swallowed")
	}
	if _, err := broken.Apply(ctx(), "acme", "widget", actor, Doc{}); err == nil {
		t.Fatal("apply write error swallowed")
	}
}

func TestNormalizeStoredBareDoc(t *testing.T) {
	s, _, _ := hideSvc(t, identity.RoleTriage)
	if _, err := store.PutBytes(ctx(), s.Store, ModerationKey("acme", "bare"), []byte(`{"version":1}`), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.State(ctx(), "acme", "bare")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Hidden == nil || doc.Locks == nil {
		t.Fatalf("bare doc maps = %#v / %#v", doc.Hidden, doc.Locks)
	}
}

func TestRepoModerationHandlerAuthAndOriginArms(t *testing.T) {
	s, _ := testService(t)
	bad := func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrForbidden, Why: "nope"}
	}
	h := &Handler{Svc: s, Auth: bad}
	if rec := do(h, "GET", "/acme/widgets/api/moderation"); rec.Code != http.StatusForbidden {
		t.Fatalf("GET auth error = %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	h.Handle(rec, httptest.NewRequest(http.MethodPut, "/acme/widgets/api/moderation", strings.NewReader(`{}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT auth error = %d", rec.Code)
	}
	// An unreadable body is 400.
	h.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "alice"}, nil
	}
	req := httptest.NewRequest(http.MethodPut, "/acme/widgets/api/moderation", nil)
	req.Body = errReader{}
	if rec := httptest.NewRecorder(); (func() bool { return h.Handle(rec, req) })() && rec.Code != http.StatusBadRequest {
		t.Fatalf("unreadable body = %d", rec.Code)
	}
	// Same-origin: a malformed or hostless Origin is refused.
	if apiSameOrigin(&http.Request{Header: http.Header{"Origin": []string{"http://["}}}) {
		t.Fatal("malformed origin accepted")
	}
	if apiSameOrigin(&http.Request{Header: http.Header{"Referer": []string{"https:///nohost"}}}) {
		t.Fatal("hostless referer accepted")
	}
}

// errReader fails every Read.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read down") }
func (errReader) Close() error             { return nil }
