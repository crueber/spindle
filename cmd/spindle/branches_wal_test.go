// branches_wal_test.go — the shipped branches composition end-to-end through
// the real WAL: create/delete refs through the publish funnel, the default
// branch and a policy-protected ref refused before any write, and the deleted
// ref gone after a registry restart (the bucket is the repository — no local
// cache state may resurrect it).
package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/branches"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/wal"
)

// headView is the tiny api.RepoView the default-branch closure needs; unused
// methods are promoted from the nil embedded interface (never called here).
type headView struct {
	api.RepoView
	ref api.Ref
	ok  bool
	err error
}

func (v headView) Head(context.Context, git.RepoId) (api.Ref, bool, error) {
	return v.ref, v.ok, v.err
}

func TestBranchesEndToEndRealWAL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary absent")
	}
	ctx := context.Background()
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, testTagsConfig(t))
	defer reg.Close()

	h, err := reg.Create(ctx, "o/r", git.Sha1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sha := seedCommit(t, h.Repo().Path)
	view := headView{ref: api.Ref{Name: "refs/heads/main", SHA: sha}, ok: true}
	svc := newBranchesService(st, nil, reg, "git", view)
	principal := auth.Principal{Name: "jane", Write: true}

	// Create a branch from a commit: one REF_UPDATE through the WAL funnel.
	if err := svc.Create(ctx, "o", "r", principal, "feature", sha); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if live, ok := liveTagSHA(t, reg, "o/r", "refs/heads/feature"); !ok || live != sha {
		t.Fatalf("live branch = %q ok=%v, want %s", live, ok, sha)
	}
	// Recreating the same branch is a CAS conflict, never a move.
	if err := svc.Create(ctx, "o", "r", principal, "feature", sha); !errors.Is(err, branches.ErrConflict) {
		t.Fatalf("recreate = %v, want conflict", err)
	}

	// The default branch is refused before any write.
	if err := svc.Delete(ctx, "o", "r", principal, "main"); !errors.Is(err, branches.ErrForbidden) || !strings.Contains(err.Error(), "default branch") {
		t.Fatalf("default delete = %v", err)
	}
	if _, ok := liveTagSHA(t, reg, "o/r", "refs/heads/feature"); !ok {
		t.Fatal("default refusal must not touch feature")
	}

	// A protected ref: the stored policy document (the settings tab's own)
	// denies the delete by name, and nothing is published.
	policyDoc := `{"version":1,"rules":[{"name":"keep-release","match":{"refs":["refs/heads/release/*"]},"effect":{"protect":{"restricts":["delete"]}}}]}`
	if _, err := store.PutBytes(ctx, st, branches.PolicyKey("o", "r"), []byte(policyDoc), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Create(ctx, "o", "r", principal, "release/1", sha); err != nil {
		t.Fatalf("create release branch: %v", err)
	}
	if err := svc.Delete(ctx, "o", "r", principal, "release/1"); !errors.Is(err, branches.ErrForbidden) || !strings.Contains(err.Error(), "rejected by rule 'keep-release'") {
		t.Fatalf("protected delete = %v", err)
	}
	if _, ok := liveTagSHA(t, reg, "o/r", "refs/heads/release/1"); !ok {
		t.Fatal("protected delete removed the ref")
	}

	// Delete the branch: the ref is gone from the snapshot...
	if err := svc.Delete(ctx, "o", "r", principal, "feature"); err != nil {
		t.Fatalf("delete branch: %v", err)
	}
	if _, ok := liveTagSHA(t, reg, "o/r", "refs/heads/feature"); ok {
		t.Fatal("ref still live after delete")
	}
	// ...and stays gone after a cold restart (fresh cache, same bucket).
	reg.Close()
	reg2 := wal.NewRegistry(ctx, st, testTagsConfig(t))
	defer reg2.Close()
	if _, ok := liveTagSHA(t, reg2, "o/r", "refs/heads/feature"); ok {
		t.Fatal("deleted ref resurrected after replay")
	}
	if _, ok := liveTagSHA(t, reg2, "o/r", "refs/heads/release/1"); !ok {
		t.Fatal("untouched branch lost after replay")
	}
}

func TestBranchesServiceUnwiredDefault(t *testing.T) {
	// apiEnv nil → the composition's closure reports an error and delete
	// fails closed (no default verifiable).
	svc := newBranchesService(store.NewMemory(), nil, nil, "git", apiEnvRepo(nil))
	svc.Store = nil // unused before the default check
	err := svc.Delete(context.Background(), "o", "r", auth.Principal{Name: "jane", Write: true}, "x")
	if !errors.Is(err, branches.ErrUnavailable) {
		t.Fatalf("unwired default = %v", err)
	}
}
