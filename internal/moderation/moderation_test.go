package moderation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// --- fakes -------------------------------------------------------------------

type fakeNamespaces struct {
	orgs  map[string]bool
	users map[string]bool
	err   error
}

func (f *fakeNamespaces) UserExists(_ context.Context, username string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.users[username], nil
}

func (f *fakeNamespaces) IsOrg(_ context.Context, owner string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.orgs[owner], nil
}

func testService(t *testing.T) (*Service, *fakeNamespaces) {
	t.Helper()
	ns := &fakeNamespaces{
		orgs:  map[string]bool{"acme": true},
		users: map[string]bool{"alice": true, "bob": true, "carol": true},
	}
	s := New(store.NewMemory())
	s.Now = func() time.Time { return time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) }
	s.Namespaces = ns
	return s, ns
}

func ctx() context.Context { return context.Background() }

// --- block list --------------------------------------------------------------

func TestBlockUnblockIdempotent(t *testing.T) {
	s, _ := testService(t)

	v1, changed, err := s.Block(ctx(), "Alice", "Bob")
	if err != nil || !changed || v1 != 1 {
		t.Fatalf("block = (%d, %v, %v)", v1, changed, err)
	}
	doc, err := s.Blocks(ctx(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Blocked) != 1 || doc.Blocked[0] != "bob" || doc.Version != 1 || doc.UpdatedAt == "" {
		t.Fatalf("doc = %+v", doc)
	}

	// Idempotent: the state does not change, the version does not move, and
	// no second audit record is written.
	v2, changed, err := s.Block(ctx(), "alice", "bob")
	if err != nil || changed || v2 != v1 {
		t.Fatalf("repeat block = (%d, %v, %v)", v2, changed, err)
	}
	if got := auditCount(t, s); got != 1 {
		t.Fatalf("audit after repeat block = %d, want 1", got)
	}

	// Unblock is idempotent the same way.
	if _, changed, err := s.Unblock(ctx(), "alice", "bob"); err != nil || !changed {
		t.Fatalf("unblock = (%v, %v)", changed, err)
	}
	if _, changed, err := s.Unblock(ctx(), "alice", "bob"); err != nil || changed {
		t.Fatalf("repeat unblock = (%v, %v)", changed, err)
	}
	doc, _ = s.Blocks(ctx(), "alice")
	if len(doc.Blocked) != 0 {
		t.Fatalf("after unblock = %+v", doc)
	}
	if got := auditCount(t, s); got != 2 {
		t.Fatalf("audit total = %d, want 2 (one block + one unblock)", got)
	}
}

// auditCount walks the create-only audit family (a test-only LIST: the
// production reader is exact-key).
func auditCount(t *testing.T, s *Service) int {
	t.Helper()
	n := 0
	if err := s.Store.List(ctx(), "moderation/audit/", "", func(store.ObjectMeta) error {
		n++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBlockValidation(t *testing.T) {
	s, _ := testService(t)
	cases := []struct {
		name         string
		blocker, who string
		wantErr      error
	}{
		{"invalid blocker", "has space", "bob", ErrInvalid},
		{"invalid subject", "alice", "no/slash", ErrInvalid},
		{"self", "alice", "Alice", ErrInvalid},
		{"unknown subject", "alice", "nobody", ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := s.Block(ctx(), c.blocker, c.who); !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
		})
	}
}

func TestBlockLimit(t *testing.T) {
	s, _ := testService(t)
	for i := 0; i < BlockLimit; i++ {
		name := "u" + itoa(i)
		s.Namespaces.(*fakeNamespaces).users[name] = true
		if _, _, err := s.Block(ctx(), "alice", name); err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
	}
	if _, _, err := s.Block(ctx(), "alice", "bob"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("over-limit err = %v, want ErrInvalid", err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestBlockedByDirections(t *testing.T) {
	s, _ := testService(t)
	if _, _, err := s.Block(ctx(), "alice", "bob"); err != nil {
		t.Fatal(err)
	}
	if blocked, err := s.BlockedBy(ctx(), "alice", "bob"); err != nil || !blocked {
		t.Fatalf("alice→bob = (%v, %v)", blocked, err)
	}
	// Directional: bob blocking nothing means alice is free.
	if blocked, err := s.BlockedBy(ctx(), "bob", "alice"); err != nil || blocked {
		t.Fatalf("bob→alice = (%v, %v)", blocked, err)
	}
	// Self and blank are never blocked.
	for _, pair := range [][2]string{{"alice", "alice"}, {"", "bob"}, {"alice", ""}} {
		if blocked, err := s.BlockedBy(ctx(), pair[0], pair[1]); err != nil || blocked {
			t.Fatalf("%v = (%v, %v)", pair, blocked, err)
		}
	}
}

func TestBlockedFromRepoUserNamespace(t *testing.T) {
	s, ns := testService(t)
	ns.users["dave"] = true // dave owns repos but is not an org
	if _, _, err := s.Block(ctx(), "dave", "bob"); err != nil {
		t.Fatal(err)
	}
	blocked, reason, err := s.BlockedFromRepo(ctx(), "dave", "app", "bob")
	if err != nil || !blocked || reason == "" {
		t.Fatalf("dave/app bob = (%v, %q, %v)", blocked, reason, err)
	}
	// The owner always keeps their own namespace.
	if blocked, _, _ := s.BlockedFromRepo(ctx(), "dave", "app", "dave"); blocked {
		t.Fatal("owner blocked out of own namespace")
	}
	// The blocked principal's own namespace is unaffected (A blocks B must
	// not restrict A acting on B's content).
	if blocked, _, _ := s.BlockedFromRepo(ctx(), "bob", "app", "dave"); blocked {
		t.Fatal("reverse direction must not be restricted")
	}
}

func TestBlockedFromRepoOrgExempt(t *testing.T) {
	s, ns := testService(t)
	// alice blocked bob, but repos owned by the org are the org's surface.
	if _, _, err := s.Block(ctx(), "alice", "bob"); err != nil {
		t.Fatal(err)
	}
	ns.orgs["alice-org"] = true
	if blocked, _, err := s.BlockedFromRepo(ctx(), "alice-org", "app", "bob"); err != nil || blocked {
		t.Fatalf("org repo = (%v, %v), want not blocked", blocked, err)
	}
}

func TestBlockedFromRepoGateErrorFailsClosed(t *testing.T) {
	s, ns := testService(t)
	ns.err = errors.New("store down")
	if _, _, err := s.BlockedFromRepo(ctx(), "dave", "app", "bob"); err == nil {
		t.Fatal("gate error must surface (consumers fail closed)")
	}
}

// --- cache / cost ------------------------------------------------------------

type countingStore struct {
	store.ObjectStore
	mu   sync.Mutex
	gets int
}

func (c *countingStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	c.mu.Lock()
	c.gets++
	c.mu.Unlock()
	return c.ObjectStore.Get(ctx, key, opts)
}

func (c *countingStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets
}

func TestBlockGateAddsNoExtraReadWhenCached(t *testing.T) {
	base := store.NewMemory()
	cs := &countingStore{ObjectStore: base}
	ns := &fakeNamespaces{orgs: map[string]bool{}, users: map[string]bool{"alice": true, "bob": true}}
	s := New(cs)
	s.Namespaces = ns
	if _, _, err := s.Block(ctx(), "alice", "bob"); err != nil {
		t.Fatal(err)
	}
	cs.mu.Lock()
	cs.gets = 0
	cs.mu.Unlock()
	if blocked, _, err := s.BlockedFromRepo(ctx(), "alice", "r", "bob"); err != nil || !blocked {
		t.Fatalf("gate = (%v, %v)", blocked, err)
	}
	if got := cs.count(); got != 1 {
		t.Fatalf("block gate store GETs = %d, want 1 (one conditional read)", got)
	}
	// A cached call still makes exactly ONE read — the conditional GET that
	// revalidates the stamped version — and no body re-parse (the store
	// answers NotModified). "Cached" never means "stale".
	if blocked, _, err := s.BlockedFromRepo(ctx(), "alice", "r", "bob"); err != nil || !blocked {
		t.Fatalf("cached gate = (%v, %v)", blocked, err)
	}
	if got := cs.count(); got != 2 {
		t.Fatalf("cached gate GETs = %d, want 2 total", got)
	}
}

func TestCacheSeesUnblockImmediately(t *testing.T) {
	s, _ := testService(t)
	if _, _, err := s.Block(ctx(), "alice", "bob"); err != nil {
		t.Fatal(err)
	}
	if blocked, _ := s.BlockedBy(ctx(), "alice", "bob"); !blocked {
		t.Fatal("expected blocked")
	}
	if _, _, err := s.Unblock(ctx(), "alice", "bob"); err != nil {
		t.Fatal(err)
	}
	if blocked, err := s.BlockedBy(ctx(), "alice", "bob"); err != nil || blocked {
		t.Fatalf("after unblock = (%v, %v) — cache must revalidate, not serve stale", blocked, err)
	}
}

// --- error paths --------------------------------------------------------------

type failingStore struct {
	store.ObjectStore
	failPut int // number of Put calls to fail with a precondition error
	mu      sync.Mutex
}

func (f *failingStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	f.mu.Lock()
	if f.failPut > 0 {
		f.failPut--
		f.mu.Unlock()
		return store.ObjectMeta{}, &store.StoreError{Kind: store.ErrKindPreconditionFailed, Key: key}
	}
	f.mu.Unlock()
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func TestBlockCASRetriesThenSucceeds(t *testing.T) {
	fs := &failingStore{ObjectStore: store.NewMemory(), failPut: 2}
	ns := &fakeNamespaces{orgs: map[string]bool{}, users: map[string]bool{"alice": true, "bob": true}}
	s := New(fs)
	s.Namespaces = ns
	if _, changed, err := s.Block(ctx(), "alice", "bob"); err != nil || !changed {
		t.Fatalf("block with CAS contention = (%v, %v)", changed, err)
	}
}

func TestBlockCASExhaustion(t *testing.T) {
	fs := &failingStore{ObjectStore: store.NewMemory(), failPut: 100}
	ns := &fakeNamespaces{orgs: map[string]bool{}, users: map[string]bool{"alice": true, "bob": true}}
	s := New(fs)
	s.Namespaces = ns
	if _, _, err := s.Block(ctx(), "alice", "bob"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestCorruptDocFailsClosed(t *testing.T) {
	s, _ := testService(t)
	if _, err := store.PutBytes(ctx(), s.Store, BlocksKey("alice"), []byte("{not json"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BlockedBy(ctx(), "alice", "bob"); err == nil {
		t.Fatal("corrupt block doc must error (fail closed), not read as empty")
	}
	if _, err := store.PutBytes(ctx(), s.Store, BlocksKey("alice"), []byte(`{"version":9,"blocked":[]}`),
		store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BlockedBy(ctx(), "alice", "bob"); err == nil {
		t.Fatal("unknown version must be refused")
	}
}

func TestCount(t *testing.T) {
	s, _ := testService(t)
	if _, _, err := s.Block(ctx(), "alice", "bob"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Count(ctx(), "alice"); err != nil || n != 1 {
		t.Fatalf("count = (%d, %v)", n, err)
	}
}

func TestCanonicalList(t *testing.T) {
	got := canonicalList([]string{" Bob ", "bob", "CAROL", ""})
	if len(got) != 2 || got[0] != "bob" || got[1] != "carol" {
		t.Fatalf("canonical = %v", got)
	}
}
