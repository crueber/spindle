package git

import (
	"strings"
	"testing"
	"time"
)

func TestIsManagedRefAndReason(t *testing.T) {
	for _, tc := range []struct {
		ref  string
		want bool
	}{
		{"refs/pull/1/head", true},
		{"refs/pull/2/merge", true},
		{"refs/heads/main", false},
		{"refs/tags/v1", false},
		{"", false},
		{"refs/heads/refs/pull/1", false}, // prefix match only at the start
	} {
		if got := IsManagedRef(tc.ref); got != tc.want {
			t.Errorf("IsManagedRef(%q) = %v, want %v", tc.ref, got, tc.want)
		}
	}
	// The wire refusal carries the rule name the policy Verdict convention uses.
	reason := ManagedRefReason()
	if !strings.Contains(reason, ManagedRefRule) || !strings.HasPrefix(reason, "rejected by rule '") {
		t.Fatalf("ManagedRefReason() = %q", reason)
	}
}

func TestParseGitDate(t *testing.T) {
	want := time.Date(2026, 9, 11, 12, 34, 56, 0, time.UTC)
	got := parseGitDate("2026-09-11T12:34:56Z")
	if !got.Equal(want) {
		t.Fatalf("parseGitDate(valid) = %v, want %v", got, want)
	}
	// A present-but-unparseable date must not fail a push: zero time, no error.
	if got := parseGitDate("not a date"); !got.IsZero() {
		t.Fatalf("parseGitDate(garbage) = %v, want zero", got)
	}
}
