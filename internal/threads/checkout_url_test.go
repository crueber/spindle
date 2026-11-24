package threads

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCheckoutBaseSelection pins the checkout URL rule (docs/threads.md §4.4):
//
//  1. a configured server.public_url WINS — it is the operator's declaration
//     of an address job containers can reach, while the request origin may be
//     a browser-only name (walgit.localhost) that resolves on the host but
//     never inside a container;
//  2. without one, the protocol request's origin is used (X-Forwarded-Proto
//     honoured, the host keeping its port), which is the address the runner
//     itself reached;
//  3. nothing configured and no host yields no URL (the runner then reports
//     "no checkout URL in the job payload").
func TestCheckoutBaseSelection(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://walgit.localhost:8080/api/v1/threads/runners/fetch", nil)

	// Origin fallback: no configured public URL.
	h := &Handler{Svc: &Service{}}
	if got := h.checkoutBase(req); got != "http://walgit.localhost:8080" {
		t.Fatalf("origin fallback = %q", got)
	}
	// Nil-safe (a handler whose service is not wired keeps the origin).
	if got := (&Handler{}).checkoutBase(req); got != "http://walgit.localhost:8080" {
		t.Fatalf("nil service = %q", got)
	}

	// The configured base wins, even against a different request origin, and a
	// trailing slash is trimmed.
	h.Svc.CheckoutBaseURL = "https://git.example.com/"
	if got := h.checkoutBase(req); got != "https://git.example.com" {
		t.Fatalf("configured base = %q", got)
	}

	// Whitespace is not a configured base.
	h.Svc.CheckoutBaseURL = "   "
	if got := h.checkoutBase(req); got != "http://walgit.localhost:8080" {
		t.Fatalf("blank base = %q", got)
	}
	// A malformed public value is ignored (the origin still works) rather than
	// poisoning the checkout URL.
	for _, bad := range []string{"walgit.localhost:8080", "ftp://git.example.com", "git.example.com"} {
		h.Svc.CheckoutBaseURL = bad
		if got := h.checkoutBase(req); got != "http://walgit.localhost:8080" {
			t.Fatalf("malformed base %q = %q, want the origin", bad, got)
		}
	}

	// Origin fallback keeps the X-Forwarded-Proto rule.
	req.Header.Set("X-Forwarded-Proto", "https")
	h.Svc.CheckoutBaseURL = ""
	if got := h.checkoutBase(req); got != "https://walgit.localhost:8080" {
		t.Fatalf("proxied origin = %q", got)
	}

	// No host and no configured base: no URL at all.
	noHost := httptest.NewRequest(http.MethodPost, "/x", nil)
	noHost.Host = ""
	if got := h.checkoutBase(noHost); got != "" {
		t.Fatalf("empty host = %q", got)
	}

	// A TLS request keeps https on the origin path.
	tlsReq := httptest.NewRequest(http.MethodPost, "https://spindle.example.com/x", nil)
	tlsReq.TLS = &tls.ConnectionState{}
	if got := h.checkoutBase(tlsReq); got != "https://spindle.example.com" {
		t.Fatalf("tls origin = %q", got)
	}
}
