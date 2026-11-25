package config

import (
	"strings"
	"testing"
)

// TestValidateRepoWebsite is the fail-closed shape table: the scheme allowlist
// is the security rule (a settings value is rendered as an anchor), and the
// bound mirrors the description budget.
func TestValidateRepoWebsite(t *testing.T) {
	t.Parallel()
	long := "https://example.com/" + strings.Repeat("a", MaxRepoWebsiteBytes)
	cases := []struct {
		name  string
		value string
		ok    bool
		// want names a fragment the refusal message must carry (the key).
		want string
	}{
		{name: "empty is unset", value: "", ok: true},
		{name: "https bare", value: "https://example.com", ok: true},
		{name: "https path", value: "https://example.com/docs/start", ok: true},
		{name: "http with query and fragment", value: "http://example.com/a?b=1&c=2#top", ok: true},
		{name: "uppercase scheme", value: "HTTPS://Example.COM/Path", ok: true},
		{name: "port and trailing slash", value: "https://example.com:8443/x/", ok: true},
		{name: "percent-encoded space", value: "https://example.com/a%20b", ok: true},
		{name: "unicode path", value: "https://example.com/über", ok: true},
		{name: "javascript scheme", value: "javascript:alert(1)", want: "absolute http or https"},
		{name: "data scheme", value: "data:text/html,<script>alert(1)</script>", want: "absolute http or https"},
		{name: "file scheme", value: "file:///etc/passwd", want: "absolute http or https"},
		{name: "scheme-relative", value: "//example.com/x", want: "absolute http or https"},
		{name: "ftp scheme", value: "ftp://example.com/x", want: "absolute http or https"},
		{name: "credentials", value: "https://user:pass@example.com/", want: "credentials"},
		{name: "user only credentials", value: "https://user@example.com/", want: "credentials"},
		{name: "no host", value: "https://", want: "host"},
		{name: "hostless path", value: "https:///docs", want: "host"},
		{name: "opaque http", value: "http:example.com", want: "host"},
		{name: "unparseable", value: "https://exa mple.com/", want: "whitespace"},
		{name: "leading space", value: " https://example.com", want: "whitespace"},
		{name: "trailing space", value: "https://example.com ", want: "whitespace"},
		{name: "interior space", value: "https://example.com/a b", want: "whitespace"},
		{name: "tab", value: "https://example.com/\tx", want: "whitespace"},
		{name: "control byte", value: "https://example.com/\x7f", want: "control"},
		{name: "over the bound", value: long, want: "limit is 512"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateRepoWebsite(tc.value)
			if tc.ok {
				if err != nil {
					t.Fatalf("ValidateRepoWebsite(%q) = %v, want nil", tc.value, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateRepoWebsite(%q) = nil, want a refusal", tc.value)
			}
			if !strings.HasPrefix(err.Error(), "repo settings: website") {
				t.Fatalf("error must name the key: %v", err)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v missing %q", err, tc.want)
			}
		})
	}
}

// TestRepoSettingsWebsiteRoundTrip pins the additive key: it parses with the
// document, extracts through WebsiteOf (the fail-open display helper), and
// never leaks into the merged host config.
func TestRepoSettingsWebsiteRoundTrip(t *testing.T) {
	t.Parallel()
	body := []byte("description = \"docs\"\nwebsite = \"https://example.com/docs?ref=readme\"\n\n[bundles]\nmain_only = true\n")
	rs, err := ParseRepoSettings(body)
	if err != nil {
		t.Fatalf("ParseRepoSettings: %v", err)
	}
	if rs.Website != "https://example.com/docs?ref=readme" {
		t.Fatalf("website = %q", rs.Website)
	}
	if got := WebsiteOf(body); got != rs.Website {
		t.Fatalf("WebsiteOf = %q, want %q", got, rs.Website)
	}
	// Merge keeps the host config untouched: website is display metadata.
	base := Defaults()
	merged, err := rs.Merge(base)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !merged.Bundles.MainOnly {
		t.Fatalf("merge dropped the allowed section")
	}
	// Config has no website field at all (compile-time): the passenger rides
	// the document, never the merged host config.

	// An invalid value refuses the whole document (fail closed at publish).
	if _, err := ParseRepoSettings([]byte("website = \"javascript:alert(1)\"\n")); err == nil {
		t.Fatal("javascript: website must be rejected by ParseRepoSettings")
	}

	// Unset, empty bodies and unparseable docs all read as "" (fail-open).
	for _, b := range [][]byte{nil, {}, []byte("  \n"), []byte("[broken\n"), []byte("description = \"x\"\n")} {
		if got := WebsiteOf(b); got != "" {
			t.Fatalf("WebsiteOf(%q) = %q, want \"\"", b, got)
		}
	}
}
