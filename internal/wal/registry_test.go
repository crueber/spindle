// registry_test.go — registry lifecycle + persisted-state behavior (05 §5.1).
package wal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/store"
)

func store2() store.ObjectStore { return store.NewMemory() }

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	cfg.WAL.BatchWindow = config.Duration(5 * time.Millisecond)
	cfg.WAL.FreshnessTTL = 0 // always check
	return cfg
}

func newTestRegistry(t *testing.T) (*Registry, store.ObjectStore) {
	t.Helper()
	st := store.NewMemory()
	r := NewRegistry(context.Background(), st, testConfig(t))
	t.Cleanup(r.Close)
	return r, st
}

func TestRegistry_CreateOpenDelete(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()

	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if h.ID != "acme/api" {
		t.Fatalf("id = %q", h.ID)
	}
	// Duplicate create → ErrAlreadyExists.
	if _, err := r.Create(ctx, "acme/api", git.Sha1); err == nil {
		t.Fatal("duplicate create succeeded")
	}

	// Open returns the same handle (fast path + single-flight).
	h2, err := r.Open(ctx, "acme/api")
	if err != nil || h2 != h {
		t.Fatalf("open: %v (same=%v)", err, h2 == h)
	}

	// Manifest exists in the store.
	ok, err := store.Exists(ctx, st, "repos/acme/api/manifest.pb")
	if err != nil || !ok {
		t.Fatalf("manifest exists: %v %v", ok, err)
	}

	// Delete: manifest gone, local dir gone, opens fail.
	if _, err := r.Delete(ctx, "acme/api"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if ok, _ := store.Exists(ctx, st, "repos/acme/api/manifest.pb"); ok {
		t.Fatal("manifest survived delete")
	}
	if _, err := os.Stat(filepath.Join(r.vals.cacheDir, "acme", "api.git")); !os.IsNotExist(err) {
		t.Fatal("local dir survived delete")
	}
	if _, err := r.Open(ctx, "acme/api"); err == nil {
		t.Fatal("open after delete succeeded")
	}
}

func TestRegistry_List(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	for _, id := range []string{"a/one", "a/two", "b/three"} {
		if _, err := r.Create(ctx, id, git.Sha1); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	r.listing.mu.Lock() // force a refresh on next List
	r.listing.at = time.Time{}
	r.listing.mu.Unlock()
	got := r.List()
	want := map[string]bool{"a/one": true, "a/two": true, "b/three": true}
	if len(got) != len(want) {
		t.Fatalf("list = %v, want %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("unexpected repo %q in %v", id, got)
		}
	}
}

func TestRegistry_StateCorruptionFallsBackToDefaults(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()

	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Corrupt the state file; the bucket is the truth so open must not fail.
	if err := os.WriteFile(filepath.Join(h.Dir(), "walgit-state.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	r2 := NewRegistry(ctx, r.Store(), testConfig(t))
	defer r2.Close()
	h2, err := r2.Open(ctx, "acme/api")
	if err != nil {
		t.Fatalf("open with corrupt state: %v", err)
	}
	st := loadState(h2.Dir())
	if st.AppliedSeq != 0 || st.ManifestVersion != "" {
		t.Fatalf("state = %+v, want zero-value defaults", st)
	}
	if !st.PacksReady() {
		t.Fatal("zero state should count as packs_ready")
	}
}

func TestRegistry_OpenWithoutManifest(t *testing.T) {
	r, _ := newTestRegistry(t)
	if _, err := r.Open(context.Background(), "no/such"); err == nil {
		t.Fatal("open of unknown repo succeeded")
	}
}

// TestRegistry_OnCreateFiresOncePerBirth pins the Forgejo #364 birth
// observer: exactly one call with the repo id on a fresh create, silence
// on duplicate creates (412 losers), opens, and invalid ids — and a nil
// observer never panics.
func TestRegistry_OnCreateFiresOncePerBirth(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	var got []string
	r.OnCreate = func(_ context.Context, id string) { got = append(got, id) }

	if _, err := r.Create(ctx, "acme/api", git.Sha1); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Duplicate create races the winner: 412, no second birth.
	if _, err := r.Create(ctx, "acme/api", git.Sha1); err == nil {
		t.Fatal("duplicate create succeeded")
	}
	// Opens and invalid ids never fire.
	if _, err := r.Open(ctx, "acme/api"); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := r.Create(ctx, "bad id!", git.Sha1); err == nil {
		t.Fatal("invalid create succeeded")
	}
	if len(got) != 1 || got[0] != "acme/api" {
		t.Fatalf("OnCreate calls = %v, want [acme/api]", got)
	}

	// Nil observer: births still succeed.
	r2, _ := newTestRegistry(t)
	if _, err := r2.Create(ctx, "acme/other", git.Sha1); err != nil {
		t.Fatalf("create without observer: %v", err)
	}
}

// TestRegistry_Invalidate covers the move's quiesce step (PLAN-V2 §3 T.1): the
// handle and the local serving copy go, bucket objects stay, the listing
// snapshot is dropped, and the call is idempotent.
func TestRegistry_Invalidate(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/moved", git.Sha1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	dir := h.Dir()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("serving copy missing after create: %v", err)
	}
	// Warm the listing snapshot so the invalidation has something to drop.
	_ = r.List()
	if err := r.Invalidate("acme/moved"); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if r.Get("acme/moved") != nil {
		t.Fatal("handle survived Invalidate")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("serving copy survived Invalidate: %v", err)
	}
	// Bucket truth is untouched (the non-destructive contract).
	if ok, err := store.Exists(ctx, st, manifestKey("acme/moved")); err != nil || !ok {
		t.Fatalf("manifest must survive Invalidate: %v %v", ok, err)
	}
	r.listing.mu.Lock()
	snapshot := r.listing.repos
	r.listing.mu.Unlock()
	if snapshot != nil {
		t.Fatal("listing snapshot survived Invalidate")
	}
	// Idempotent, and a bad id is rejected rather than silently ignored.
	if err := r.Invalidate("acme/moved"); err != nil {
		t.Fatalf("second invalidate: %v", err)
	}
	if err := r.Invalidate("bad id!"); err == nil {
		t.Fatal("invalid id must error")
	}
	// The repo still opens afterwards (bucket truth).
	if _, err := r.Open(ctx, "acme/moved"); err != nil {
		t.Fatalf("re-open after Invalidate: %v", err)
	}
}

// TestRegistry_VerifyRepo covers the move's verification probe: a healthy repo
// passes, a misdirected manifest and a corrupt one fail with a named detail.
func TestRegistry_VerifyRepo(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	if _, err := r.Create(ctx, "acme/ok", git.Sha1); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.VerifyRepo(ctx, "acme/ok"); err != nil {
		t.Fatalf("verify healthy repo: %v", err)
	}
	// Unknown repo → a not-found error.
	if err := r.VerifyRepo(ctx, "acme/ghost"); err == nil {
		t.Fatal("unknown repo must not verify")
	}
	// A manifest whose repo field names something else fails the way a move
	// would leave it if the retargeting were skipped.
	misdirected := (&pbManifest{FormatVersion: 1, Repo: "acme/elsewhere", ObjectFormat: "sha1", Revision: 2}).Marshal()
	if _, err := store.PutBytes(ctx, st, manifestKey("acme/misdirected"), misdirected,
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	err := r.VerifyRepo(ctx, "acme/misdirected")
	if err == nil || !strings.Contains(err.Error(), "names repo") {
		t.Fatalf("misdirected manifest = %v, want a named failure", err)
	}
	// A corrupt manifest fails at the decode.
	if _, err := store.PutBytes(ctx, st, manifestKey("acme/corrupt"), []byte("{nope"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if err := r.VerifyRepo(ctx, "acme/corrupt"); err == nil {
		t.Fatal("corrupt manifest must not verify")
	}
}
