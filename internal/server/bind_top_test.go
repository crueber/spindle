package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubTop claims one /v2 request and reports true (the OCI registry shape).
type stubTop struct {
	claimed int
}

func (s *stubTop) Handle(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/v2/" {
		return false
	}
	s.claimed++
	w.WriteHeader(http.StatusTeapot)
	return true
}

// TestChainTopRoutesNonRepoNamespace proves the top-level seam is reachable
// through the real router: /v2/ is not an owner/repo path, so it must not fall
// into repoDispatch (docs/features/14 §3).
func TestChainTopRoutesNonRepoNamespace(t *testing.T) {
	s, _ := newTestServer(t, nil)
	stub := &stubTop{}
	s.ChainTop(stub)
	h := s.Handler() // the route tree is built after chaining

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/v2/", nil))
	if rec.Code != http.StatusTeapot || stub.claimed != 1 {
		t.Fatalf("v2 route = %d claimed=%d", rec.Code, stub.claimed)
	}
	// A chained surface that declines the path leaves the 404 (it owns the
	// namespace, not the whole server).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/v2/token", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("declined = %d, want 404", rec.Code)
	}
}

func TestChainTopNilSafe(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.ChainTop()
	rec := httptest.NewRecorder()
	s.topServe(rec, httptest.NewRequest("GET", "http://x/v2/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nil chain = %d", rec.Code)
	}
}
