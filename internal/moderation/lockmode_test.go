package moderation

import (
	"errors"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
)

// lockmode_test.go — §10.1 lock modes: the default (`all`) is unchanged, a
// mode switch keeps the evidence, the mode is audited, and Apply enforces the
// maintain floor for a mode-only delta.

func TestLockThreadModes(t *testing.T) {
	s, _, _ := hideSvc(t, identity.RoleMaintain)
	actor := auth.Principal{Name: "alice"}

	// Absent mode = all (today's semantics).
	if _, changed, err := s.LockThread(ctx(), "acme", "widget", 1, actor, "flame war", ""); err != nil || !changed {
		t.Fatalf("lock = %v, %v", changed, err)
	}
	locked, mode, reason, _, err := s.Snapshot(ctx(), "acme", "widget", 1, nil)
	if err != nil || !locked || mode != LockModeAll || reason != "flame war" {
		t.Fatalf("snapshot = %v %q %q %v", locked, mode, reason, err)
	}
	// Same mode again is a no-op (the reason is not rewritten).
	if _, changed, _ := s.LockThread(ctx(), "acme", "widget", 1, actor, "ignored", LockModeAll); changed {
		t.Fatal("repeat lock in the same mode reported a change")
	}
	// Switching mode keeps the first moderator's words.
	if _, changed, err := s.LockThread(ctx(), "acme", "widget", 1, auth.Principal{Name: "root", Admin: true}, "ignored", LockModeCollaborators); err != nil || !changed {
		t.Fatalf("mode switch = %v, %v", changed, err)
	}
	doc, _ := s.State(ctx(), "acme", "widget")
	e := doc.Locks[ThreadLockKey(1)]
	if e.Mode != LockModeCollaborators || e.By != "alice" || e.Reason != "flame war" {
		t.Fatalf("lock entry = %+v (mode switch must not rewrite evidence)", e)
	}
	// The switch is audited with both modes.
	var sawSwitch bool
	for seq := 1; ; seq++ {
		rec, aerr := s.AuditEntryAt(ctx(), seq)
		if aerr != nil || rec == nil {
			break
		}
		if rec.Action == ActionLock && strings.Contains(rec.Context, "mode=collaborators") &&
			strings.Contains(rec.Context, "prev_mode=all") {
			sawSwitch = true
		}
	}
	if !sawSwitch {
		t.Fatal("mode switch not audited")
	}
	// Unknown mode is refused before any write.
	if _, _, err := s.LockThread(ctx(), "acme", "widget", 2, actor, "r", "everyone"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad mode err = %v", err)
	}
	// Unlock removes the mode with the lock.
	if _, changed, err := s.UnlockThread(ctx(), "acme", "widget", 1, actor); err != nil || !changed {
		t.Fatalf("unlock = %v, %v", changed, err)
	}
	if locked, mode, _, _, _ := s.Snapshot(ctx(), "acme", "widget", 1, nil); locked || mode != "" {
		t.Fatalf("after unlock = %v %q", locked, mode)
	}
}

func TestApplyLockModeDeltaNeedsMaintain(t *testing.T) {
	s, roles, _ := hideSvc(t, identity.RoleWrite)
	actor := auth.Principal{Name: "alice"}
	roles.role = identity.RoleMaintain
	if _, _, err := s.LockThread(ctx(), "acme", "widget", 3, actor, "cool off", LockModeAll); err != nil {
		t.Fatal(err)
	}
	doc, _ := s.State(ctx(), "acme", "widget")

	// A writer PUT that only widens the mode is a lock delta → 403.
	roles.role = identity.RoleWrite
	want := doc
	want.Locks[ThreadLockKey(3)] = LockState{By: doc.Locks[ThreadLockKey(3)].By, At: doc.Locks[ThreadLockKey(3)].At,
		Reason: doc.Locks[ThreadLockKey(3)].Reason, Mode: LockModeCollaborators}
	if _, err := s.Apply(ctx(), "acme", "widget", actor, want); !errors.Is(err, ErrForbidden) {
		t.Fatalf("write mode delta err = %v", err)
	}

	// Maintain flips it; a repeat is a no-op (no revision churn).
	roles.role = identity.RoleMaintain
	ver, err := s.Apply(ctx(), "acme", "widget", actor, want)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.State(ctx(), "acme", "widget")
	if after.Locks[ThreadLockKey(3)].Mode != LockModeCollaborators || after.Locks[ThreadLockKey(3)].Reason != "cool off" {
		t.Fatalf("after apply = %+v", after.Locks)
	}
	repeat := want
	repeat.Version = ver
	if ver2, err := s.Apply(ctx(), "acme", "widget", actor, repeat); err != nil || ver2 != ver {
		t.Fatalf("no-op apply = %d, %v (want %d)", ver2, err, ver)
	}

	// An invalid mode on an added entry is refused before the write.
	bad := after
	bad.Locks = map[string]LockState{}
	for k, v := range after.Locks {
		bad.Locks[k] = v
	}
	bad.Locks[ThreadLockKey(4)] = LockState{By: "x", Reason: "y", Mode: "nobody"}
	if _, err := s.Apply(ctx(), "acme", "widget", actor, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad added mode err = %v", err)
	}

	// The repo-wide envelope carries its own mode and audits it.
	flat := after
	flat.Locked = true
	flat.LockReason = "freeze"
	flat.LockMode = LockModeCollaborators
	if _, err := s.Apply(ctx(), "acme", "widget", actor, flat); err != nil {
		t.Fatal(err)
	}
	locked, mode, reason, _, _ := s.Snapshot(ctx(), "acme", "widget", 99, nil)
	if !locked || mode != LockModeCollaborators || reason != "freeze" {
		t.Fatalf("flat envelope = %v %q %q", locked, mode, reason)
	}
}
