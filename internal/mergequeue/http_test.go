package mergequeue

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/server/auth"
)

func newHTTP(t *testing.T) (*Handler, *harness) {
	t.Helper()
	h := newHarness(t)
	// Hold the queue: the API tests assert records, not driver progress.
	if err := h.svc.Pause(context.Background(), "o", "r", "main", "", maint()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	h.pulls.cand = &pulls.QueueCandidate{Num: 1, State: "open", BaseRef: "refs/heads/main", Author: "author@example.com", HeadSHA: strings.Repeat("a", 40)}
	hn := &Handler{Svc: h.svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return maint(), nil
	}}
	return hn, h
}

func do(h *Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHTTPRoutes(t *testing.T) {
	h, hh := newHTTP(t)
	base := "/o/r/api/merge-queue"

	// Empty list.
	rec := do(h, http.MethodGet, base, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	var v View
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.Repo != "o/r" {
		t.Fatalf("list body = %s (%v)", rec.Body.String(), err)
	}

	// Enqueue.
	rec = do(h, http.MethodPost, base+"/enqueue", `{"base":"main","number":1,"priority":0}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("enqueue = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"position":1`) {
		t.Fatalf("enqueue response has no position: %s", rec.Body.String())
	}
	// The recorded credential is never emitted.
	if strings.Contains(rec.Body.String(), "principal") || strings.Contains(rec.Body.String(), "groups") {
		t.Fatalf("enqueue response leaked the credential: %s", rec.Body.String())
	}
	// Duplicate.
	if rec = do(h, http.MethodPost, base+"/enqueue", `{"base":"main","number":1}`); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate = %d %s", rec.Code, rec.Body.String())
	}
	// The PR entry.
	rec = do(h, http.MethodGet, "/o/r/api/pulls/1/merge-queue", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"pending"`) {
		t.Fatalf("entry = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(h, http.MethodGet, "/o/r/api/pulls/9/merge-queue", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing entry = %d", rec.Code)
	}

	// Dequeue (the author is the fake candidate's author, but this principal
	// is the maintainer).
	rec = do(h, http.MethodPost, base+"/dequeue", `{"base":"main","number":1}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"cancelled"`) {
		t.Fatalf("dequeue = %d %s", rec.Code, rec.Body.String())
	}
	// Pause / resume.
	if rec = do(h, http.MethodPost, base+"/pause", `{"base":"main","reason":"freeze"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("pause = %d %s", rec.Code, rec.Body.String())
	}
	if rec = do(h, http.MethodPost, base+"/resume", `{"base":"main"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("resume = %d %s", rec.Code, rec.Body.String())
	}
	// Method and shape refusals.
	if rec = do(h, http.MethodPut, base, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("put list = %d", rec.Code)
	}
	if rec = do(h, http.MethodGet, base+"/enqueue", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("get enqueue = %d", rec.Code)
	}
	if rec = do(h, http.MethodGet, base+"/pause", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("get pause = %d", rec.Code)
	}
	if rec = do(h, http.MethodPost, base+"/enqueue", `{"base":"main","number":1,"bogus":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d %s", rec.Code, rec.Body.String())
	}
	if rec = do(h, http.MethodPost, base+"/enqueue", `{`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json = %d", rec.Code)
	}
	if rec = do(h, http.MethodPost, base+"/enqueue", `{"base":"main","number":1}{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing json = %d", rec.Code)
	}
	if rec = do(h, http.MethodPost, base+"/dequeue", `{"base":"","number":0}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad dequeue = %d %s", rec.Code, rec.Body.String())
	}
	// A non-queue path under the lane is not ours; the serve surface 404s.
	if rec = do(h, http.MethodGet, "/o/r/api/pulls/1/diff", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("non-route = %d", rec.Code)
	}
	if rec = do(h, http.MethodGet, "/o/r/queue", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("page path = %d", rec.Code)
	}
	// A malformed repo is not a queue route either.
	if rec = do(h, http.MethodGet, "/o//api/merge-queue", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bad repo = %d", rec.Code)
	}
	// Unauthorized principal maps to a real 401 (the auth seam).
	anon := &Handler{Svc: hh.svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Anonymous(), nil
	}}
	rec = do(anon, http.MethodGet, base, "")
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("anon = %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

func TestHTTPWriteErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{fmtWrap(ErrNotFound), http.StatusNotFound},
		{fmtWrap(ErrConflict), http.StatusConflict},
		{fmtWrap(ErrForbidden), http.StatusForbidden},
		{fmtWrap(ErrUnauthorized), http.StatusUnauthorized},
		{fmtWrap(ErrInvalid), http.StatusUnprocessableEntity},
		{fmtWrap(ErrUnavailable), http.StatusServiceUnavailable},
		{errors.New("boom"), http.StatusInternalServerError},
		{&auth.AuthError{Kind: auth.ErrForbidden, Why: "no"}, http.StatusForbidden},
		{&auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}, http.StatusServiceUnavailable},
		{&auth.AuthError{Kind: auth.ErrInvalid, Why: "bad"}, http.StatusUnauthorized},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		writeErr(rec, c.err)
		if rec.Code != c.want {
			t.Errorf("%v → %d, want %d", c.err, rec.Code, c.want)
		}
	}
}

func fmtWrap(err error) error { return errors.Join(errors.New("context"), err) }

func TestHTTPUnavailableService(t *testing.T) {
	h, hh := newHTTP(t)
	hh.svc.Pulls = nil // enqueue refuses before any record
	rec := do(h, http.MethodPost, "/o/r/api/merge-queue/enqueue", `{"base":"main","number":1}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no pulls = %d %s", rec.Code, rec.Body.String())
	}
}
