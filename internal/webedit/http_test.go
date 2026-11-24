package webedit

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

func newHTTPHarness(t *testing.T) (*harness, *Handler) {
	t.Helper()
	h := newHarness(t)
	handler := &Handler{Svc: h.svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return writer(), nil
	}}
	return h, handler
}

func do(t *testing.T, handler *Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func jsonBody(content string, extra map[string]any) string {
	m := map[string]any{"message": "edit", "content": base64.StdEncoding.EncodeToString([]byte(content))}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestHTTPEditHappyBothLanes(t *testing.T) {
	h, handler := newHTTPHarness(t)
	body := jsonBody("bye\n", map[string]any{"expected_blob_sha": testSHA})
	for _, lane := range []string{"api", "api-browser"} {
		rec := do(t, handler, http.MethodPut, "/o/r/"+lane+"/contents?ref=main&path=hello.txt", body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s status = %d body=%s", lane, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Location"); got != "/o/r/blob/main/hello.txt" {
			t.Fatalf("Location = %q", got)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("cache = %q", rec.Header().Get("Cache-Control"))
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("json: %v", err)
		}
		commit, _ := out["commit"].(map[string]any)
		if commit == nil || commit["sha"] != "2222222222222222222222222222222222222222" || out["branch"] != "main" {
			t.Fatalf("body = %v", out)
		}
	}
	if len(h.refs.calls) != 2 {
		t.Fatalf("publishes = %d", len(h.refs.calls))
	}
}

func TestHTTPCreateDeleteAndPR(t *testing.T) {
	h, handler := newHTTPHarness(t)
	// POST create.
	rec := do(t, handler, http.MethodPost, "/o/r/api/contents?ref=main&path=new.txt", jsonBody("hi\n", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	// POST on an existing path is a 409 naming the blob.
	rec = do(t, handler, http.MethodPost, "/o/r/api/contents?ref=main&path=hello.txt", jsonBody("x", nil))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("existing create = %d %s", rec.Code, rec.Body.String())
	}
	// DELETE requires expected_blob_sha.
	rec = do(t, handler, http.MethodDelete, "/o/r/api/contents?ref=main&path=hello.txt", `{"message":"rm"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete no sha = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, handler, http.MethodDelete, "/o/r/api/contents?ref=main&path=hello.txt",
		`{"message":"rm","expected_blob_sha":"`+testSHA+`"}`)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Location"), "/o/r/tree/") {
		t.Fatalf("delete = %d %s loc=%s", rec.Code, rec.Body.String(), rec.Header().Get("Location"))
	}
	// New branch + PR.
	rec = do(t, handler, http.MethodPut, "/o/r/api/contents?ref=main&path=hello.txt",
		jsonBody("x", map[string]any{"expected_blob_sha": testSHA, "new_branch": "fix/x", "open_pr": true}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("pr create = %d %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/o/r/pull/7" {
		t.Fatalf("pr Location = %q", loc)
	}
	if !strings.Contains(rec.Body.String(), `"pr":{"num":7}`) {
		t.Fatalf("pr body = %s", rec.Body.String())
	}
	_ = h
}

func TestHTTPErrorMatrix(t *testing.T) {
	_, handler := newHTTPHarness(t)
	cases := []struct {
		name, method, target, body string
		auth                       auth.Principal
		want                       int
	}{
		{"anonymous", http.MethodPut, "/o/r/api/contents?ref=main&path=hello.txt", jsonBody("x", nil), auth.Anonymous(), http.StatusUnauthorized},
		{"missing ref", http.MethodPut, "/o/r/api/contents?path=hello.txt", jsonBody("x", nil), writer(), http.StatusBadRequest},
		{"missing path", http.MethodPut, "/o/r/api/contents?ref=main", jsonBody("x", nil), writer(), http.StatusBadRequest},
		{"bad path", http.MethodPut, "/o/r/api/contents?ref=main&path=../x", jsonBody("x", nil), writer(), http.StatusBadRequest},
		{"bad json", http.MethodPut, "/o/r/api/contents?ref=main&path=hello.txt", `{`, writer(), http.StatusBadRequest},
		{"bad base64", http.MethodPut, "/o/r/api/contents?ref=main&path=hello.txt", `{"message":"m","content":"!!!"}`, writer(), http.StatusBadRequest},
		{"method", http.MethodGet, "/o/r/api/contents?ref=main&path=hello.txt", "", writer(), http.StatusMethodNotAllowed},
		{"unknown ref", http.MethodPut, "/o/r/api/contents?ref=nope&path=hello.txt", jsonBody("x", map[string]any{"expected_blob_sha": testSHA}), writer(), http.StatusNotFound},
		{"blob moved", http.MethodPut, "/o/r/api/contents?ref=main&path=hello.txt", jsonBody("x", map[string]any{"expected_blob_sha": "9" + strings.Repeat("1", 39)}), writer(), http.StatusConflict},
		{"binary", http.MethodPut, "/o/r/api/contents?ref=main&path=hello.txt", jsonBody("\x00", map[string]any{"expected_blob_sha": testSHA}), writer(), http.StatusUnsupportedMediaType},
		{"blank message", http.MethodPut, "/o/r/api/contents?ref=main&path=hello.txt", `{"message":"  ","content":"aGk=","expected_blob_sha":"` + testSHA + `"}`, writer(), http.StatusBadRequest},
	}
	for _, tc := range cases {
		handler.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) { return tc.auth, nil }
		rec := do(t, handler, tc.method, tc.target, tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s = %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
	// The 401 carries WWW-Authenticate.
	handler.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) { return auth.Anonymous(), nil }
	rec := do(t, handler, http.MethodPut, "/o/r/api/contents?ref=main&path=hello.txt", jsonBody("x", nil))
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("401 missing WWW-Authenticate")
	}
	// A body over the cap is 413.
	h2 := newHarness(t)
	h2.svc.MaxBytes = 4
	h2handler := &Handler{Svc: h2.svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) { return writer(), nil }}
	rec = do(t, h2handler, http.MethodPut, "/o/r/api/contents?ref=main&path=hello.txt", jsonBody(strings.Repeat("x", 1024), map[string]any{"expected_blob_sha": testSHA}))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body cap = %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPHandleScope(t *testing.T) {
	_, handler := newHTTPHarness(t)
	for _, target := range []string{
		"/o/r/api/tags", "/o/r/api/contents/extra", "/api/v1/repos", "/o/r/tree",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		if handler.Handle(rec, req) {
			t.Errorf("Handle(%s) claimed a foreign path", target)
		}
	}
	req := httptest.NewRequest(http.MethodPut, "/o/r.git/api/contents?ref=main&path=x", strings.NewReader(jsonBody("x", nil)))
	rec := httptest.NewRecorder()
	if !handler.Handle(rec, req) {
		t.Fatal("git-suffixed lane not claimed")
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("git-suffixed lane = %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPNilAuthIsAnonymous(t *testing.T) {
	h := newHarness(t)
	handler := &Handler{Svc: h.svc}
	rec := do(t, handler, http.MethodPut, "/o/r/api/contents?ref=main&path=hello.txt", jsonBody("x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("nil auth = %d", rec.Code)
	}
}
