package httpsec

import (
	"net/http"
	"testing"
)

func TestSafeNextMatrix(t *testing.T) {
	rows := []struct {
		next, fallback, want string
	}{
		// Accepted: rooted same-host paths.
		{"/ok/path", "/", "/ok/path"},
		{"/ok?q=1&x=2", "/", "/ok?q=1&x=2"},
		{"/", "/", "/"},
		// Rejected: authority forms, schemes, backslashes.
		{"/\\evil.com", "/", "/"},
		{"/\\\\evil.com", "/", "/"},
		{"//evil.com", "/", "/"},
		{"///evil.com", "/", "/"},
		{"https://evil.com", "/", "/"},
		{"http://evil.com/x", "/", "/"},
		{"javascript:alert(1)", "/", "/"},
		{"evil.com", "/", "/"},
		// Rejected: percent-encoded backslash / scheme-relative forms.
		{"/%5Cevil.com", "/", "/"},
		{"/%5cevil.com", "/", "/"},
		{"/%2F%2Fevil.com", "/", "/"},
		{"/ok%zz", "/", "/"},
		// Rejected: whitespace and control characters (CRLF header splitting).
		{" /evil.com", "/", "/"},
		{"/ok\t/x", "/", "/"},
		{"/ok\r\nSet-Cookie: pwn=1", "/", "/"},
		{"/ok\x00", "/", "/"},
		// Empty falls back.
		{"", "/", "/"},
		{"", "/repo/x", "/repo/x"},
		// A bad target keeps a usable caller fallback.
		{"//evil.com", "/repo/x", "/repo/x"},
		{"/\\evil.com", "/repo/x", "/repo/x"},
		// An unusable fallback itself collapses to "/".
		{"/ok", "//bad", "/ok"},
		{"", "//bad", "/"},
	}
	for _, tc := range rows {
		if got := SafeNext(tc.next, tc.fallback); got != tc.want {
			t.Errorf("SafeNext(%q, %q) = %q, want %q", tc.next, tc.fallback, got, tc.want)
		}
	}
}

func TestApplySetsTheSharedHeaderSet(t *testing.T) {
	h := http.Header{}
	Apply(h)
	want := map[string]string{
		"Content-Security-Policy": ContentSecurityPolicy,
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "same-origin",
		"X-Content-Type-Options":  "nosniff",
	}
	for name, val := range want {
		if got := h.Get(name); got != val {
			t.Errorf("%s = %q, want %q", name, got, val)
		}
	}
	// A route-level policy wins (the release-asset sandbox CSP).
	stricter := http.Header{}
	stricter.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	Apply(stricter)
	if got := stricter.Get("Content-Security-Policy"); got != "default-src 'none'; sandbox" {
		t.Fatalf("stricter CSP overwritten: %q", got)
	}
	if got := stricter.Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("missing XFO on stricter response: %q", got)
	}
}
