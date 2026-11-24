// events_hooks_test.go — the bridge sink adapter end to end: a bridge batch
// becomes a signed repo-hook delivery with the real push payload, and a repo
// with no push subscriber records/delivers nothing.
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crueber/spindle/internal/events"
	"github.com/crueber/spindle/internal/notify"
	"github.com/crueber/spindle/internal/store"
)

type hookPost struct {
	event     string
	signature string
	body      []byte
}

type hookReceiver struct {
	mu    sync.Mutex
	posts []hookPost
	down  bool
}

func (r *hookReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	if !r.down {
		r.posts = append(r.posts, hookPost{
			event: req.Header.Get("X-Walgit-Event"), signature: req.Header.Get("X-Walgit-Signature"), body: body,
		})
	}
	r.mu.Unlock()
	if r.down {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (r *hookReceiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.posts)
}

func (r *hookReceiver) at(i int) hookPost {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.posts[i]
}

type fakeCommits struct{ calls int }

func (f *fakeCommits) Commits(_ context.Context, _, _, _ string) []notify.PushCommit {
	f.calls++
	return []notify.PushCommit{{
		ID: strings.Repeat("b", 40), Author: "Amy", Email: "amy@example.com",
		Message: "land the fix", At: "2026-11-21T10:00:00Z",
	}}
}

func bridgeBatch() []events.RefEvent {
	return []events.RefEvent{{
		Action: events.ActionUpdate, RefType: "branch", RefName: "refs/heads/main",
		Old: strings.Repeat("a", 40), New: strings.Repeat("b", 40),
		Pusher: "amy@example.com", Repo: "acme/repo",
		Walgit: events.Walgit{SchemaVersion: 1, Seq: "77", EntryKind: events.KindPushWire, RequestID: "req-9"},
	}}
}

func TestRepoHooksSinkDeliversSignedPushPayload(t *testing.T) {
	svc := notify.New(store.NewMemory(), nil)
	recv := &hookReceiver{}
	srv := httptest.NewServer(recv)
	defer srv.Close()
	hookURL := srv.URL
	if _, err := svc.CreateHook(context.Background(), "acme", "repo", "amy@example.com", notify.HookSpec{
		URL: &hookURL, Events: []string{notify.TopicPush}, Secret: strPtr("s3cr3t"),
	}); err != nil {
		t.Fatal(err)
	}

	commits := &fakeCommits{}
	sink := &repoHooksSink{svc: svc, commits: commits}
	if err := sink.Deliver(context.Background(), "acme/repo", bridgeBatch()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if commits.calls != 1 {
		t.Fatalf("commit listing calls = %d, want 1", commits.calls)
	}
	svc.DeliverRepo(context.Background(), "acme", "repo")
	if recv.count() != 1 {
		t.Fatalf("deliveries = %d, want 1", recv.count())
	}
	post := recv.at(0)
	if post.event != notify.TopicBranchUpdated {
		t.Fatalf("X-Walgit-Event = %q", post.event)
	}
	if !strings.HasPrefix(post.signature, "sha256=") {
		t.Fatalf("unsigned delivery: %q", post.signature)
	}
	var ev notify.ActivityEvent
	if err := json.Unmarshal(post.body, &ev); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Class  string `json:"class"`
		Detail struct {
			Ref        string              `json:"ref"`
			RefType    string              `json:"ref_type"`
			Before     string              `json:"before"`
			After      string              `json:"after"`
			Repository string              `json:"repository"`
			Pusher     string              `json:"pusher"`
			Commits    []notify.PushCommit `json:"commits"`
			Walgit     struct {
				Seq string `json:"seq"`
			} `json:"_walgit"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	d := payload.Detail
	if payload.Class != "push" || d.Ref != "refs/heads/main" || d.RefType != "branch" ||
		d.Before != strings.Repeat("a", 40) || d.After != strings.Repeat("b", 40) ||
		d.Repository != "acme/repo" || d.Pusher != "amy@example.com" ||
		len(d.Commits) != 1 || d.Commits[0].Message != "land the fix" || d.Walgit.Seq != "77" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestRepoHooksSinkSkipsReposWithoutPushHooks(t *testing.T) {
	svc := notify.New(store.NewMemory(), nil)
	recv := &hookReceiver{}
	srv := httptest.NewServer(recv)
	defer srv.Close()
	hookURL := srv.URL
	if _, err := svc.CreateHook(context.Background(), "acme", "repo", "amy@example.com", notify.HookSpec{
		URL: &hookURL, Events: []string{notify.ActionCommented},
	}); err != nil {
		t.Fatal(err)
	}
	commits := &fakeCommits{}
	sink := &repoHooksSink{svc: svc, commits: commits}
	if err := sink.Deliver(context.Background(), "acme/repo", bridgeBatch()); err != nil {
		t.Fatal(err)
	}
	if commits.calls != 0 {
		t.Fatalf("no subscriber must cost no commit listing, got %d", commits.calls)
	}
	svc.DeliverRepo(context.Background(), "acme", "repo")
	if recv.count() != 0 {
		t.Fatalf("collab-only hook received %d push deliveries", recv.count())
	}
	// A repo with no hooks at all is a no-op (and no error).
	if err := sink.Deliver(context.Background(), "acme/empty", bridgeBatch()); err != nil {
		t.Fatalf("repo without hooks: %v", err)
	}
	if err := sink.Deliver(context.Background(), "not-a-repo", bridgeBatch()); err != nil {
		t.Fatalf("malformed repo must be ignored: %v", err)
	}
	if err := (*repoHooksSink)(nil).Deliver(context.Background(), "acme/repo", bridgeBatch()); err != nil {
		t.Fatalf("nil sink: %v", err)
	}
}

func TestPushCommitReaderDegradesToNil(t *testing.T) {
	r := &pushCommitReader{}
	if got := r.Commits(context.Background(), "acme/repo", "", strings.Repeat("b", 40)); got != nil {
		t.Fatalf("unwired reader = %+v", got)
	}
	if got := (*pushCommitReader)(nil).Commits(context.Background(), "acme/repo", "", "b"); got != nil {
		t.Fatalf("nil reader = %+v", got)
	}
}

func strPtr(s string) *string { return &s }
