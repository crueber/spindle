package moderation

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// captureLogger records warnings (the audit-drop path is otherwise silent in
// tests).
type captureLogger struct{ msgs []string }

func (c *captureLogger) WarnContext(_ context.Context, msg string, _ ...any) {
	c.msgs = append(c.msgs, msg)
}

func TestAuditFailureIsLoggedNotFatal(t *testing.T) {
	s, _ := testService(t)
	log := &captureLogger{}
	s.Logger = log

	// A corrupt allocator makes the audit write fail AFTER the block commits.
	if _, err := store.PutBytes(ctx(), s.Store, AuditStateKey(), []byte("{bad"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := s.Block(ctx(), "alice", "bob"); err != nil || !changed {
		t.Fatalf("block with broken audit = (%v, %v)", changed, err)
	}
	if len(log.msgs) != 1 {
		t.Fatalf("warnings = %v", log.msgs)
	}
	// The block itself is on the bucket (the audit gap never reverts state).
	if blocked, err := s.BlockedBy(ctx(), "alice", "bob"); err != nil || !blocked {
		t.Fatalf("block lost = (%v, %v)", blocked, err)
	}
	if _, changed, err := s.Unblock(ctx(), "alice", "bob"); err != nil || !changed {
		t.Fatalf("unblock with broken audit = (%v, %v)", changed, err)
	}
	if len(log.msgs) != 2 {
		t.Fatalf("warnings = %v", log.msgs)
	}
}

func TestNilNamespacesDefaults(t *testing.T) {
	s := New(store.NewMemory())
	if _, changed, err := s.Block(ctx(), "alice", "ghost"); err != nil || !changed {
		t.Fatalf("nil namespaces block = (%v, %v)", changed, err)
	}
	// With no org probe every owner is treated as a user namespace (the
	// documented fail-closed default for an unwired probe).
	if blocked, _, err := s.BlockedFromRepo(ctx(), "alice", "r", "ghost"); err != nil || !blocked {
		t.Fatalf("nil namespaces gate = (%v, %v)", blocked, err)
	}
	if _, err := s.BlockedBy(ctx(), "alice", "ghost"); err != nil {
		t.Fatal(err)
	}
}

func TestNilClockAndZeroVersion(t *testing.T) {
	s := New(store.NewMemory())
	s.Now = nil
	if s.nowUTC().IsZero() {
		t.Fatal("nil clock must still answer")
	}
	if blockVersionOf(BlocksDoc{}) != blocksVersion {
		t.Fatal("zero version must read as v1")
	}
}

func TestHandlerNilAuthIsAnonymous(t *testing.T) {
	svc, _ := testService(t)
	h := &Handler{Svc: svc}
	rec := do(h, "GET", "/api/v1/users/alice/blocks")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("nil auth = %d", rec.Code)
	}
}

func TestHandlerNilNamespacesSkipsProbe(t *testing.T) {
	svc, _ := testService(t)
	svc.Namespaces = nil
	h := &Handler{Svc: svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "root", Admin: true}, nil
	}}
	// "ghost" is a valid spelling and is not probed, so the (empty) list
	// answers — the nil-probe degradation is documented.
	if rec := do(h, "GET", "/api/v1/users/ghost/blocks"); rec.Code != http.StatusOK {
		t.Fatalf("nil probe = %d", rec.Code)
	}
}

func TestUnknownSubjectProbeError(t *testing.T) {
	s, ns := testService(t)
	ns.err = errors.New("identity down")
	if _, _, err := s.Block(ctx(), "alice", "bob"); err == nil || errors.Is(err, ErrInvalid) {
		t.Fatalf("probe failure = %v (must be a 503-class error, not a validation 400)", err)
	}
}

func TestWriteErrAndStatusForBranches(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, http.StatusOK},
		{ErrInvalid, http.StatusBadRequest},
		{ErrNotFound, http.StatusNotFound},
		{ErrForbidden, http.StatusForbidden},
		{ErrUnauthorized, http.StatusUnauthorized},
		{ErrConflict, http.StatusConflict},
		{errors.New("other"), http.StatusServiceUnavailable},
	}
	for _, c := range cases {
		if got := statusFor(c.err); got != c.want {
			t.Fatalf("statusFor(%v) = %d, want %d", c.err, got, c.want)
		}
		if c.err == nil {
			continue // writeErr is never called with nil (statusFor is)
		}
		rec := httptest.NewRecorder()
		writeErr(rec, c.err)
		if rec.Code != c.want {
			t.Fatalf("writeErr(%v) = %d, want %d", c.err, rec.Code, c.want)
		}
	}
	// AuthError kinds each map.
	for _, c := range []struct {
		kind auth.AuthErrorKind
		want int
	}{
		{auth.ErrForbidden, http.StatusForbidden},
		{auth.ErrUnavailable, http.StatusServiceUnavailable},
		{auth.ErrInvalid, http.StatusUnauthorized},
	} {
		rec := httptest.NewRecorder()
		writeErr(rec, &auth.AuthError{Kind: c.kind, Why: "x"})
		if rec.Code != c.want {
			t.Fatalf("auth kind %v = %d, want %d", c.kind, rec.Code, c.want)
		}
		if c.kind == auth.ErrUnavailable && rec.Header().Get("Retry-After") != "15" {
			t.Fatal("unavailable must carry Retry-After")
		}
	}
}

func TestWriteJSONEncodeFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, make(chan int))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("encode failure = %d", rec.Code)
	}
}

func TestAuditCorruptRecordAndStoreError(t *testing.T) {
	s, _ := testService(t)
	if _, err := store.PutBytes(ctx(), s.Store, AuditKey(1), []byte("{bad"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuditEntryAt(ctx(), 1); err == nil || errors.Is(err, ErrInvalid) {
		t.Fatalf("corrupt record = %v (must be a server fault)", err)
	}
	if _, _, err := s.AuditSince(ctx(), 0, 10); err == nil || errors.Is(err, ErrInvalid) {
		t.Fatalf("since corrupt = %v", err)
	}
	s2, _ := testService(t)
	s2.Store = errStore{}
	if _, _, err := s2.AuditSince(ctx(), 0, 10); err == nil {
		t.Fatal("store error must surface")
	}
	if _, err := s2.AuditEntryAt(ctx(), 1); err == nil {
		t.Fatal("store error must surface")
	}
	if err := s2.appendAudit(ctx(), "alice", ActionBlock, "user:bob", "", ""); err == nil {
		t.Fatal("allocator store error must surface")
	}
	// Page-size normalization.
	if _, _, err := s2.AuditSince(ctx(), -5, 0); err == nil {
		t.Fatal("store error must surface on the normalized page too")
	}
}

func TestReserveAuditSeqExhaustion(t *testing.T) {
	fs := &failingStore{ObjectStore: store.NewMemory(), failPut: 100}
	s := New(fs)
	if _, err := s.reserveAuditSeq(ctx()); !errors.Is(err, ErrConflict) {
		t.Fatalf("exhaustion = %v", err)
	}
}

func TestUpperVersionRefused(t *testing.T) {
	s, _ := testService(t)
	raw := []byte(`{"version":2,"blocked":[]}`)
	if _, err := store.PutBytes(ctx(), s.Store, BlocksKey("alice"), raw,
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BlockedBy(ctx(), "alice", "bob"); err == nil || errors.Is(err, ErrInvalid) {
		t.Fatalf("version = %v (must be a 503-class fault, not a client 400)", err)
	}
}

func TestHandleShortPaths(t *testing.T) {
	h := mustHandler(t, auth.Principal{Name: "alice"})
	for _, path := range []string{"/api", "/api/v1", "/api/v1/users", "/api/v1/users//blocks"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		if h.Handle(rec, req) {
			t.Fatalf("%s claimed", path)
		}
	}
}

func TestProbeErrorIs503(t *testing.T) {
	svc, ns := testService(t)
	ns.err = errors.New("identity down")
	h := &Handler{Svc: svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "alice"}, nil
	}}
	if rec := do(h, "GET", "/api/v1/users/alice/blocks"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("probe error = %d", rec.Code)
	}
	if rec := do(h, "PUT", "/api/v1/users/alice/blocks/bob"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("probe error (mutate) = %d", rec.Code)
	}
}

func TestStoreOutageOnEveryVerb(t *testing.T) {
	svc, _ := testService(t)
	svc.Store = errStore{}
	h := &Handler{Svc: svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "alice"}, nil
	}}
	if rec := do(h, "GET", "/api/v1/users/alice/blocks"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("get outage = %d", rec.Code)
	}
	if rec := do(h, "DELETE", "/api/v1/users/alice/blocks/bob"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("delete outage = %d", rec.Code)
	}
}

func TestAuditRecordExistsConflict(t *testing.T) {
	s, _ := testService(t)
	// A pre-existing record at the next seq makes the Create fail (412).
	if _, err := store.PutBytes(ctx(), s.Store, AuditKey(1), []byte(`{"seq":1}`),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if err := s.appendAudit(ctx(), "alice", ActionBlock, "user:bob", "", ""); err == nil {
		t.Fatal("duplicate audit seq must error")
	}
}

func TestAuditStateZeroNormalizes(t *testing.T) {
	s, _ := testService(t)
	if _, err := store.PutBytes(ctx(), s.Store, AuditStateKey(), []byte(`{"next_seq":0}`),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	seq, err := s.reserveAuditSeq(ctx())
	if err != nil || seq != 1 {
		t.Fatalf("seq = (%d, %v)", seq, err)
	}
}

func TestDeletedBlockDocReadsEmpty(t *testing.T) {
	s, _ := testService(t)
	if _, _, err := s.Block(ctx(), "alice", "bob"); err != nil {
		t.Fatal(err)
	}
	// Delete the object underneath the cache: the next read revalidates and
	// takes the absent branch (no stale cache hit).
	raw, _, err := store.GetBytes(ctx(), s.Store, BlocksKey("alice"), store.GetOptions{})
	if err != nil || raw == nil {
		t.Fatal("seed read")
	}
	if err := s.Store.Delete(ctx(), BlocksKey("alice"), ""); err != nil {
		t.Fatal(err)
	}
	doc, err := s.Blocks(ctx(), "alice")
	if err != nil || len(doc.Blocked) != 0 {
		t.Fatalf("after delete = (%+v, %v)", doc, err)
	}
}

func TestUnblockStoreError(t *testing.T) {
	s, _ := testService(t)
	if _, _, err := s.Block(ctx(), "alice", "bob"); err != nil {
		t.Fatal(err)
	}
	s.Store = errStore{}
	if _, _, err := s.Unblock(ctx(), "alice", "bob"); err == nil {
		t.Fatal("store error must surface on unblock")
	}
}

func TestFreshServiceParsesStoredDoc(t *testing.T) {
	s, _ := testService(t)
	if _, _, err := s.Block(ctx(), "alice", "bob"); err != nil {
		t.Fatal(err)
	}
	// A second service (no warm cache) parses the stored document.
	fresh := New(s.Store)
	fresh.Namespaces = s.Namespaces
	if blocked, err := fresh.BlockedBy(ctx(), "alice", "bob"); err != nil || !blocked {
		t.Fatalf("fresh parse = (%v, %v)", blocked, err)
	}
}

func TestAuthErrorOnGetList(t *testing.T) {
	svc, _ := testService(t)
	h := &Handler{Svc: svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "x"}
	}}
	if rec := do(h, "GET", "/api/v1/users/alice/blocks"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("auth error on list = %d", rec.Code)
	}
}

func TestDirectVerbsOnStoreOutage(t *testing.T) {
	s, _ := testService(t)
	s.Store = errStore{}
	if _, _, err := s.Block(ctx(), "alice", "bob"); err == nil {
		t.Fatal("block store error must surface")
	}
	if _, _, err := s.Unblock(ctx(), "alice", "bob"); err == nil {
		t.Fatal("unblock store error must surface")
	}
}

// putErrStore reads normally but fails writes (the reserve-then-create audit
// path's write leg).
type putErrStore struct{ store.ObjectStore }

func (putErrStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	return store.ObjectMeta{}, errors.New("put boom")
}

func TestReserveAuditSeqPutError(t *testing.T) {
	s := New(putErrStore{ObjectStore: store.NewMemory()})
	if _, err := s.reserveAuditSeq(ctx()); err == nil {
		t.Fatal("put error must surface")
	}
}
