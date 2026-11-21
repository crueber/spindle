// collab_orgbirth_test.go — Forgejo #364: the repo-birth observer
// (orgBirthObserver, the reg.OnCreate wiring in buildCollab) emits
// repo_created into the org log for org-owned births and stays silent
// for user-owned births (one GetOrg probe per birth, cold path only —
// the push budget test pins the ≤1 bound). It builds identity + notify
// directly instead of via buildCollab: task-kind registrations panic on
// a second buildCollab in one test binary (see collab_discovery_test.go).
package main

import (
	"context"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/notify"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/wal"
)

func TestOrgBirthObserverEmitsForOrgsOnly(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, cfg)
	defer reg.Close()

	ident := identity.New(st, cfg)
	notifySvc, _ := newNotifyService(st, ident)
	if notifySvc.OrgOwner == nil {
		t.Fatal("composition must wire notify.OrgOwner to identity (org owners would 403)")
	}
	reg.OnCreate = orgBirthObserver(ident, notifySvc)

	if _, err := ident.CreateOrg(ctx, "acme", "Acme", "", "amy@example.com"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	// Org-owned birth → one repo_created in the org log.
	if _, err := reg.Create(ctx, "acme/api", git.Sha1); err != nil {
		t.Fatalf("create org repo: %v", err)
	}
	events, more, err := notifySvc.ListOrgActivity(ctx, "acme", 0, 0)
	if err != nil {
		t.Fatalf("ListOrgActivity: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Action == "repo_created" && e.Title == "repo acme/api created" {
			found = true
		}
	}
	if !found || more {
		t.Fatalf("org log = %+v more=%v, want repo_created + no more", events, more)
	}

	// User-owned birth → silence (no org log state for the user).
	if _, err := reg.Create(ctx, "bob/personal", git.Sha1); err != nil {
		t.Fatalf("create user repo: %v", err)
	}
	if got, _, err := notifySvc.ListOrgActivity(ctx, "bob", 0, 0); err != nil || len(got) != 0 {
		t.Fatalf("user org log = %+v, %v — want empty (no phantom org state)", got, err)
	}

	// Dotted owner (can never be an org spelling) → silence without probe.
	if _, err := reg.Create(ctx, "bob.jones/repo", git.Sha1); err != nil {
		t.Fatalf("create dotted-owner repo: %v", err)
	}
	if ok, _ := store.Exists(ctx, st, notify.OrgEventKey("bob.jones", 1)); ok {
		t.Fatalf("phantom org event for a non-org spelling")
	}
}
