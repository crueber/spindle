// forkmove_test.go — the fork pointer families across a repository move
// (PLAN-V2 §T.1; forkmove.go). Each rewrite is checked for: the pointer
// follows, the untouched neighbours stay, and a second pass (the transfer
// task's retry) converges to the same state without a write.
package pulls

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

func seedJSON(t *testing.T, e *testEnv, key string, doc any) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutBytes(context.Background(), e.store, key, raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
}

func readForks(t *testing.T, e *testEnv, owner, repo string) *ForksIndex {
	t.Helper()
	raw, _, err := store.GetBytes(context.Background(), e.store, ForksKey(owner, repo), store.GetOptions{})
	if err != nil || raw == nil {
		return nil
	}
	fx, perr := parseForks(raw)
	if perr != nil {
		t.Fatal(perr)
	}
	return fx
}

func readFork(t *testing.T, e *testEnv, owner, repo string) *ForkDoc {
	t.Helper()
	raw, _, err := store.GetBytes(context.Background(), e.store, ForkKey(owner, repo), store.GetOptions{})
	if err != nil || raw == nil {
		return nil
	}
	var doc ForkDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		t.Fatal(jerr)
	}
	return &doc
}

func TestForkParentOf(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	if _, ok, err := e.svc.ParentOf(ctx, "acme", "plain"); err != nil || ok {
		t.Fatalf("non-fork = %v, %v", ok, err)
	}
	seedJSON(t, e, ForkKey("acme", "kid"), ForkDoc{Parent: "acme/root", ForkedAt: "2026-01-01T00:00:00Z", Version: 1})
	if p, ok, err := e.svc.ParentOf(ctx, "acme", "kid"); err != nil || !ok || p != "acme/root" {
		t.Fatalf("ParentOf = %q, %v, %v", p, ok, err)
	}
	// Corrupt provenance is not a parent (the forkread fail-closed rule), not
	// an error that blocks the whole move.
	if _, err := store.PutBytes(ctx, e.store, ForkKey("acme", "bad"), []byte("{bad"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := e.svc.ParentOf(ctx, "acme", "bad"); err != nil || ok {
		t.Fatalf("corrupt doc = %v, %v", ok, err)
	}
}

func TestForkChildMoved(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	seedJSON(t, e, ForksKey("acme", "root"), ForksIndex{Version: 2, Forks: []ForkEntry{
		{Repo: "acme/kid", ForkedAt: "2026-01-01T00:00:00Z"},
		{Repo: "acme/other", ForkedAt: "2026-01-02T00:00:00Z"},
	}})
	changed, err := e.svc.ChildMoved(ctx, "acme", "root", "acme/kid", "beta/kid")
	if err != nil || !changed {
		t.Fatalf("ChildMoved = %v, %v", changed, err)
	}
	fx := readForks(t, e, "acme", "root")
	if len(fx.Forks) != 2 || fx.Forks[0].Repo != "beta/kid" || fx.Forks[1].Repo != "acme/other" {
		t.Fatalf("rows = %+v", fx.Forks)
	}
	if fx.Version != 3 {
		t.Fatalf("version = %d, want 3", fx.Version)
	}
	if fx.Forks[1].ForkedAt != "2026-01-02T00:00:00Z" {
		t.Fatal("neighbour row must be untouched")
	}
	// Retry: no old row, no write, no version bump.
	before, _, _ := store.GetBytes(ctx, e.store, ForksKey("acme", "root"), store.GetOptions{})
	if changed, err := e.svc.ChildMoved(ctx, "acme", "root", "acme/kid", "beta/kid"); err != nil || changed {
		t.Fatalf("retry = %v, %v", changed, err)
	}
	after, _, _ := store.GetBytes(ctx, e.store, ForksKey("acme", "root"), store.GetOptions{})
	if string(before) != string(after) {
		t.Fatal("retry must not rewrite the index")
	}
	// Ghost rule: an absent index writes nothing.
	if changed, err := e.svc.ChildMoved(ctx, "ghost", "root", "a/b", "c/d"); err != nil || changed {
		t.Fatalf("absent index = %v, %v", changed, err)
	}
	// Corrupt index fails closed (a real read error, never a silent drop).
	if _, err := store.PutBytes(ctx, e.store, ForksKey("corrupt", "root"), []byte("{bad"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ChildMoved(ctx, "corrupt", "root", "a/b", "c/d"); err == nil {
		t.Fatal("corrupt index must error")
	}
}

func TestForkParentMoved(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	// The moved parent's index (already at the new prefix) lists two kids;
	// one is a grandchild whose Root is the moved parent.
	seedJSON(t, e, ForksKey("beta", "root"), ForksIndex{Version: 1, Forks: []ForkEntry{
		{Repo: "acme/kid1"}, {Repo: "carol/kid2"}, {Repo: "gone/child"},
	}})
	seedJSON(t, e, ForkKey("acme", "kid1"), ForkDoc{Parent: "acme/root", Root: "acme/root", Version: 1})
	seedJSON(t, e, ForkKey("carol", "kid2"), ForkDoc{Parent: "acme/kid1", Root: "acme/root", Version: 3})
	// A child that is not connected to the moved parent must not change.
	seedJSON(t, e, ForkKey("dave", "unrelated"), ForkDoc{Parent: "x/y", Root: "x/y", Version: 1})

	n, err := e.svc.ParentMoved(ctx, "acme/root", "beta/root")
	if err != nil || n != 2 {
		t.Fatalf("ParentMoved = %d, %v; want 2 children", n, err)
	}
	if doc := readFork(t, e, "acme", "kid1"); doc.Parent != "beta/root" || doc.Root != "beta/root" || doc.Version != 2 {
		t.Fatalf("kid1 = %+v", doc)
	}
	if doc := readFork(t, e, "carol", "kid2"); doc.Parent != "acme/kid1" || doc.Root != "beta/root" || doc.Version != 4 {
		t.Fatalf("grandchild root = %+v", doc)
	}
	if doc := readFork(t, e, "dave", "unrelated"); doc.Parent != "x/y" {
		t.Fatalf("unrelated child changed: %+v", doc)
	}
	// Retry converges without a write.
	before, _, _ := store.GetBytes(ctx, e.store, ForkKey("acme", "kid1"), store.GetOptions{})
	if n, err := e.svc.ParentMoved(ctx, "acme/root", "beta/root"); err != nil || n != 0 {
		t.Fatalf("retry = %d, %v", n, err)
	}
	after, _, _ := store.GetBytes(ctx, e.store, ForkKey("acme", "kid1"), store.GetOptions{})
	if string(before) != string(after) {
		t.Fatal("retry must not rewrite fork.json")
	}
	// A moved parent with no index is a leaf: zero children, no error.
	if n, err := e.svc.ParentMoved(ctx, "solo/leaf", "solo/leaf2"); err != nil || n != 0 {
		t.Fatalf("leaf = %d, %v", n, err)
	}
	// Corrupt child provenance fails the pass closed.
	if _, err := store.PutBytes(ctx, e.store, ForkKey("bad", "kid"), []byte("{bad"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	seedJSON(t, e, ForksKey("beta", "p2"), ForksIndex{Version: 1, Forks: []ForkEntry{{Repo: "bad/kid"}}})
	if _, err := e.svc.ParentMoved(ctx, "acme/p2", "beta/p2"); err == nil {
		t.Fatal("corrupt child must error")
	}
}

func TestForkVerifyMoved(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	// A moved child whose parent index was not rewritten yet.
	seedJSON(t, e, ForkKey("beta", "kid"), ForkDoc{Parent: "acme/root", Version: 1})
	seedJSON(t, e, ForksKey("acme", "root"), ForksIndex{Version: 1, Forks: []ForkEntry{{Repo: "acme/kid"}}})
	if err := e.svc.VerifyMoved(ctx, "acme/kid", "beta/kid"); err == nil {
		t.Fatal("stale parent row must fail verification")
	}
	if _, err := e.svc.ChildMoved(ctx, "acme", "root", "acme/kid", "beta/kid"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.VerifyMoved(ctx, "acme/kid", "beta/kid"); err != nil {
		t.Fatalf("consistent child: %v", err)
	}
	// A moved parent whose child pointer is stale.
	seedJSON(t, e, ForksKey("beta", "root"), ForksIndex{Version: 1, Forks: []ForkEntry{{Repo: "eve/kid"}}})
	seedJSON(t, e, ForkKey("eve", "kid"), ForkDoc{Parent: "acme/root", Root: "acme/root", Version: 1})
	if err := e.svc.VerifyMoved(ctx, "acme/root", "beta/root"); err == nil {
		t.Fatal("stale child pointer must fail verification")
	}
	if _, err := e.svc.ParentMoved(ctx, "acme/root", "beta/root"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.VerifyMoved(ctx, "acme/root", "beta/root"); err != nil {
		t.Fatalf("consistent parent: %v", err)
	}
	// A deleted child (no provenance) is not stale state.
	seedJSON(t, e, ForksKey("beta", "root2"), ForksIndex{Version: 1, Forks: []ForkEntry{{Repo: "gone/kid"}}})
	if err := e.svc.VerifyMoved(ctx, "acme/root2", "beta/root2"); err != nil {
		t.Fatalf("ghost child: %v", err)
	}
	// A non-fork, childless repo verifies trivially.
	if err := e.svc.VerifyMoved(ctx, "acme/plain", "beta/plain"); err != nil {
		t.Fatalf("plain repo: %v", err)
	}
	// Bad ids are rejected, not silently skipped.
	if err := e.svc.VerifyMoved(ctx, "acme/x", "no-slash"); err == nil {
		t.Fatal("bad destination id must error")
	}
	if _, err := e.svc.ParentMoved(ctx, "acme/x", "no-slash"); err == nil {
		t.Fatal("bad destination id must error on ParentMoved too")
	}
}

func TestSplitRepoID(t *testing.T) {
	cases := []struct {
		in   string
		o, r string
		ok   bool
	}{
		{"acme/app", "acme", "app", true},
		{"/app", "", "app", false},
		{"acme/", "acme", "", false},
		{"acme", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		o, r, ok := splitRepoID(c.in)
		if o != c.o || r != c.r || ok != c.ok {
			t.Errorf("splitRepoID(%q) = %q,%q,%v want %q,%q,%v", c.in, o, r, ok, c.o, c.r, c.ok)
		}
	}
}
