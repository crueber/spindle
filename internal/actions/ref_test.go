package actions

import (
	"strings"
	"testing"
)

// TestParseRefTable pins every accepted spelling and the refusals that keep a
// `uses:` value from becoming a path, a git option or an ambiguous identity.
func TestParseRefTable(t *testing.T) {
	for _, tc := range []struct {
		raw                    string
		owner, repo, path, ref string
		host                   string
		pinned                 bool
	}{
		{"actions/checkout@v4", "actions", "checkout", "", "v4", "", false},
		{"acme/tools/actions/setup@v1.2.3", "acme", "tools", "actions/setup", "v1.2.3", "", false},
		{"acme/tools@0123456789abcdef0123456789abcdef01234567", "acme", "tools", "", "0123456789abcdef0123456789abcdef01234567", "", true},
		{"https://code.forgejo.org/actions/setup-go@v5", "actions", "setup-go", "", "v5", "code.forgejo.org", false},
		{"https://git.example.com:8443/acme/repo/sub/dir@main", "acme", "repo", "sub/dir", "main", "git.example.com:8443", false},
	} {
		ref, err := ParseRef(tc.raw)
		if err != nil {
			t.Fatalf("%q: %v", tc.raw, err)
		}
		if ref.Owner != tc.owner || ref.Repo != tc.repo || ref.Path != tc.path || ref.Ref != tc.ref || ref.Host != tc.host {
			t.Fatalf("%q = %+v", tc.raw, ref)
		}
		if ref.CommitSHA() != tc.pinned {
			t.Fatalf("%q CommitSHA = %v, want %v", tc.raw, ref.CommitSHA(), tc.pinned)
		}
	}
	for _, raw := range []string{
		"", "actions", "actions/checkout", "actions/checkout@", "@v1", "actions/checkout@-x",
		"actions/checkout@v1..2", "actions//checkout@v1", "a/b/../c@v1", "a/b/c/@v1",
		"ftp://host/a/b@v1", "https:///a/b@v1", "a b/c@v1",
		"a/b@v1 v2", "a/b\tv1@x", "a/b@v1:2", strings.Repeat("a", 600) + "/b@v1",
	} {
		if _, err := ParseRef(raw); err == nil {
			t.Fatalf("%q parsed, want a refusal", raw)
		}
	}
}

func TestRefIdentityAndString(t *testing.T) {
	ref, err := ParseRef("acme/tools/sub@v1")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Repository() != "acme/tools" || ref.Identity() != "acme/tools" {
		t.Fatalf("identity = %q/%q", ref.Repository(), ref.Identity())
	}
	if ref.String() != "acme/tools/sub@v1" {
		t.Fatalf("String = %q", ref.String())
	}
	ext, _ := ParseRef("https://git.example.com/acme/tools@v1")
	if ext.Identity() != "git.example.com/acme/tools" {
		t.Fatalf("external identity = %q", ext.Identity())
	}
	if IsCommitSHA("0123") || IsCommitSHA(strings.ToUpper("0123456789abcdef0123456789abcdef01234567")) {
		t.Fatal("CommitSHA must require 40 lowercase hex characters")
	}
}
