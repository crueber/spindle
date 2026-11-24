package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// The artifact half of the protocol client (15 §2.5): raw blob streams, the
// manifest, and finalize, against a scripted HTTP server.

func TestClientArtifactBlobRoundTrip(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotLen, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
			gotLen = r.Header.Get("Content-Length")
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			_ = json.NewEncoder(w).Encode(threads.ArtifactBlobView{Index: 0, Size: int64(len(b)), SHA256: "abc"})
		default:
			w.Header().Set("Content-Length", "5")
			w.Header().Set("X-Spindle-Artifact-Path", "dist/a.txt")
			_, _ = io.WriteString(w, "alpha")
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "wrt_runner")
	view, err := c.PutArtifactBlob(context.Background(), "wjt_job", "jr_ref", 2, "build", 0, "dist/a.txt", 5, strings.NewReader("alpha"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if view.Size != 5 || view.SHA256 != "abc" {
		t.Fatalf("view = %+v", view)
	}
	if gotPath != "/api/v1/threads/jobs/jr_ref/attempt/2/artifacts/build/blobs/0" {
		t.Fatalf("path = %q", gotPath)
	}
	if !strings.Contains(gotQuery, "path=dist%2Fa.txt") || !strings.Contains(gotQuery, "size=5") {
		t.Fatalf("query = %q", gotQuery)
	}
	if gotAuth != "Bearer wjt_job" || gotLen != "5" || gotBody != "alpha" {
		t.Fatalf("headers/body = %q %q %q", gotAuth, gotLen, gotBody)
	}
	body, size, err := c.GetArtifactBlob(context.Background(), "wjt_job", "jr_ref", 2, "build", 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	b, _ := io.ReadAll(body)
	_ = body.Close()
	if string(b) != "alpha" || size != 5 {
		t.Fatalf("blob = %q (%d)", b, size)
	}
}

func TestClientArtifactErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusTeapot)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "wrt_runner")
	if _, err := c.PutArtifactBlob(context.Background(), "t", "jr", 1, "b", 0, "a", 1, strings.NewReader("x")); err == nil {
		t.Fatal("418 put accepted")
	}
	if _, _, err := c.GetArtifactBlob(context.Background(), "t", "jr", 1, "b", 0); err == nil {
		t.Fatal("418 blob read accepted")
	}
	if _, err := c.ArtifactManifest(context.Background(), "t", "jr", 1, "b"); err == nil {
		t.Fatal("418 manifest accepted")
	}
	if _, err := c.FinalizeArtifact(context.Background(), "t", "jr", 1, threads.ArtifactFinalizeInput{Name: "b"}); err == nil {
		t.Fatal("418 finalize accepted")
	}
}

func TestClientArtifactErrorMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "wrt_runner")
	if _, err := c.PutArtifactBlob(context.Background(), "t", "jr", 1, "b", 0, "a", 1, strings.NewReader("x")); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("401 put = %v", err)
	}
	if _, _, err := c.GetArtifactBlob(context.Background(), "t", "jr", 1, "b", 0); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("401 get = %v", err)
	}
	// Finalize's manifest decoding is the shared do() path; a 401 there maps to
	// the runner's fatal credential class.
	if _, err := c.FinalizeArtifact(context.Background(), "t", "jr", 1, threads.ArtifactFinalizeInput{Name: "b"}); err == nil {
		t.Fatal("401 finalize accepted")
	}
}

func TestClientArtifactBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "{")
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "wrt_runner")
	if _, err := c.PutArtifactBlob(context.Background(), "t", "jr", 1, "b", 0, "a", 1, strings.NewReader("x")); err == nil {
		t.Fatal("bad blob JSON accepted")
	}
	if _, err := c.ArtifactManifest(context.Background(), "t", "jr", 1, "b"); err == nil {
		t.Fatal("bad manifest JSON accepted")
	}
}

func TestClientArtifactManifestAndFinalizeSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(threads.ArtifactView{Artifact: threads.Artifact{Name: "build"}})
			return
		}
		_ = json.NewEncoder(w).Encode(threads.Artifact{Name: "build", TotalSize: 3})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "wrt_runner")
	man, err := c.ArtifactManifest(context.Background(), "t", "jr", 1, "build")
	if err != nil || man.Name != "build" || man.TotalSize != 3 {
		t.Fatalf("manifest = %+v (%v)", man, err)
	}
	view, err := c.FinalizeArtifact(context.Background(), "t", "jr", 1, threads.ArtifactFinalizeInput{Name: "build"})
	if err != nil || view.Name != "build" {
		t.Fatalf("finalize = %+v (%v)", view, err)
	}
	// A 409 answer is the runner's superseded class, and an empty error body
	// still yields a readable message.
	for _, status := range []int{http.StatusConflict, http.StatusBadRequest} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		c := NewClient(s.URL, "wrt_runner")
		if _, err := c.PutArtifactBlob(context.Background(), "t", "jr", 1, "b", 0, "a", 1, strings.NewReader("x")); err == nil || strings.TrimSpace(err.Error()) == "" {
			t.Fatalf("status %d put = %v", status, err)
		}
		if _, _, err := c.GetArtifactBlob(context.Background(), "t", "jr", 1, "b", 0); err == nil {
			t.Fatalf("status %d get accepted", status)
		}
		s.Close()
	}
}
