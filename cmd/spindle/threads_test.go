package main

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/threads"
)

// The on-push Threads hook (docs/features/15 §2): only branch create/update
// refs with a real new commit start a run; tags, notes and deletes do not.

func TestThreadsRefTarget(t *testing.T) {
	sha := strings.Repeat("a", 40)
	cases := []struct {
		name   string
		ref    server.PushedRef
		branch string
		ok     bool
	}{
		{"branch create", server.PushedRef{Ref: "refs/heads/main", New: sha}, "main", true},
		{"branch delete", server.PushedRef{Ref: "refs/heads/main", New: strings.Repeat("0", 40)}, "", false},
		{"tag", server.PushedRef{Ref: "refs/tags/v1", New: sha}, "", false},
		{"notes", server.PushedRef{Ref: "refs/notes/commits", New: sha}, "", false},
		{"pull ref", server.PushedRef{Ref: "refs/pull/1/head", New: sha}, "", false},
		{"empty branch", server.PushedRef{Ref: "refs/heads/", New: sha}, "", false},
		{"empty oid", server.PushedRef{Ref: "refs/heads/main", New: ""}, "", false},
	}
	for _, tc := range cases {
		branch, ok := threadsRefTarget(tc.ref)
		if ok != tc.ok || branch != tc.branch {
			t.Errorf("%s: threadsRefTarget(%+v) = %q,%v want %q,%v", tc.name, tc.ref, branch, ok, tc.branch, tc.ok)
		}
	}
}

func TestIsZeroOID(t *testing.T) {
	if !isZeroOID(strings.Repeat("0", 40)) || !isZeroOID(strings.Repeat("0", 64)) || !isZeroOID("") {
		t.Fatal("zero oids must be deletes")
	}
	if isZeroOID(strings.Repeat("a", 40)) {
		t.Fatal("a real oid is not zero")
	}
}

// TestOnPushHookNilWithoutWiring pins the nil-safe combination.
func TestOnPushHookNilWithoutWiring(t *testing.T) {
	if onPushHookOf(nil) != nil {
		t.Fatal("nil collab must yield a nil hook")
	}
	if onPushHookOf(&collabWiring{}) != nil {
		t.Fatal("a wiring without services must yield a nil hook")
	}
}

// compile-time seam assertions stay pinned in threads.go; restate the role
// seam so a signature drift in identity fails here too.
var _ threads.RoleService = (*identity.Service)(nil)
