// pushmirror_test.go — Forgejo #623 on-push fan-out: a landed client
// push notifies the hook fire-and-forget on the pushPipeline both
// transports share; refusals/failures never notify; nil hook never
// fires. Server-side publishes never enter pushPipeline (the sync.go:434
// bypass) — pinned structurally by the absence of any hook call outside
// pushPipeline.
package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/wal"
)

func onPushServer(t *testing.T, eng *fakeEngine, hook func(id git.RepoId, refs []PushedRef, byJob bool)) *Server {
	t.Helper()
	s, _ := newTestServer(t, func(o *Options) {
		o.Engine = eng
		o.OnPush = hook
	})
	eng.exists = true
	eng.placement = Placement{Serve: true}
	return s
}

func TestOnPushFiresAfterLandedPush(t *testing.T) {
	eng := &fakeEngine{}
	var mu sync.Mutex
	var got []string
	var gotRefs []PushedRef
	s := onPushServer(t, eng, func(id git.RepoId, refs []PushedRef, byJob bool) {
		mu.Lock()
		got = append(got, id.String())
		gotRefs = append(gotRefs, refs...)
		mu.Unlock()
	})
	root := t.TempDir()
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	repo, err := eng.Repo(ctx, mustRepoID(t, "o/r"), true, git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	zero := strings.Repeat("0", 40)
	oid := "1111111111111111111111111111111111111111"
	// Delete-only (no tips → no connectivity walk on the fake repo):
	// the fake engine publishes ok → landed → hook fires.
	preq := &git.PushRequest{
		Commands: []git.PushCommand{{Old: oid, New: zero, Ref: "refs/heads/main"}},
		Caps:     []string{"report-status", "side-band-64k"},
	}
	var out strings.Builder
	p := auth.Principal{Name: "ada", Write: true}
	// pushPipeline ingests via the layer: nil pack skips ingest, and the
	// fake engine publishes ok.
	if err := s.pushPipeline(ctx, mustRepoID(t, "o/r"), p, repo, preq, nil, 1<<20, &out); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if !strings.Contains(out.String(), "ok refs/heads/main") {
		t.Fatalf("report = %q", out.String())
	}
	// Fire-and-forget: poll for the hook (never block the test on it).
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("on-push hook never fired (got %d)", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if got[0] != "o/r" {
		t.Errorf("hook repo = %v", got)
	}
	// Threads run creation needs the landed ref's NEW oid: a delete-only
	// push (zero new oid) must still report the ref and its zero value.
	if len(gotRefs) != 1 || gotRefs[0].Ref != "refs/heads/main" || strings.Trim(gotRefs[0].New, "0") != "" {
		t.Fatalf("landed refs = %+v", gotRefs)
	}
}

func TestOnPushSilentOnRefusalAndFailure(t *testing.T) {
	eng := &fakeEngine{}
	fired := false
	s := onPushServer(t, eng, func(id git.RepoId, refs []PushedRef, byJob bool) { fired = true })
	root := t.TempDir()
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	repo, err := eng.Repo(ctx, mustRepoID(t, "o/r"), true, git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	p := auth.Principal{Name: "ada", Write: true}
	// All-refused push (per-ref errors, nothing landed): no fan-out.
	eng.pubPerRef = []wal.RefResult{}
	// Use the managed-ref refusal path: refs/pull/** never lands.
	preq := &git.PushRequest{
		Commands: []git.PushCommand{{Old: strings.Repeat("0", 40), New: "1111111111111111111111111111111111111111", Ref: "refs/pull/1/head"}},
		Caps:     []string{"report-status"},
	}
	var out strings.Builder
	if err := s.pushPipeline(ctx, mustRepoID(t, "o/r"), p, repo, preq, nil, 1<<20, &out); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if fired {
		t.Error("refused push fanned out")
	}
	// Nil hook: a landed push fires nothing (and never panics).
	// Delete-only (no tips → no connectivity walk on the fake repo).
	eng.pubPerRef = nil
	s2, _ := newTestServer(t, func(o *Options) { o.Engine = eng })
	eng.exists = true
	eng.placement = Placement{Serve: true}
	preq2 := &git.PushRequest{
		Commands: []git.PushCommand{{Old: "1111111111111111111111111111111111111111", New: strings.Repeat("0", 40), Ref: "refs/heads/main"}},
		Caps:     []string{"report-status"},
	}
	var out2 strings.Builder
	if err := s2.pushPipeline(ctx, mustRepoID(t, "o/r"), p, repo, preq2, nil, 1<<20, &out2); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if !strings.Contains(out2.String(), "ok refs/heads/main") {
		t.Fatalf("report = %q", out2.String())
	}
}

// TestPushWakesEventsBridge pins the round-2 blocker's first half: a landed
// client push wakes the events bridge, which is what turns a PR head push into
// a pull_request `synchronize` run within seconds instead of waiting for
// events.sweep_interval (default 5 minutes). A refused push never wakes it.
func TestPushWakesEventsBridge(t *testing.T) {
	eng := &fakeEngine{}
	var mu sync.Mutex
	var woke []string
	s, _ := newTestServer(t, func(o *Options) {
		o.Engine = eng
		o.Notifier = func(repo string) {
			mu.Lock()
			woke = append(woke, repo)
			mu.Unlock()
		}
	})
	eng.exists = true
	eng.placement = Placement{Serve: true}
	root := t.TempDir()
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	repo, err := eng.Repo(ctx, mustRepoID(t, "o/r"), true, git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	zero := strings.Repeat("0", 40)
	p := auth.Principal{Name: "ada", Write: true}
	// A landed push wakes exactly once.
	landed := &git.PushRequest{
		Commands: []git.PushCommand{{Old: "1111111111111111111111111111111111111111", New: zero, Ref: "refs/heads/main"}},
		Caps:     []string{"report-status"},
	}
	var out strings.Builder
	if err := s.pushPipeline(ctx, mustRepoID(t, "o/r"), p, repo, landed, nil, 1<<20, &out); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	mu.Lock()
	n := len(woke)
	mu.Unlock()
	if n != 1 || woke[0] != "o/r" {
		t.Fatalf("wake calls = %v, want exactly [o/r]", woke)
	}
	// A refused push (managed ref never lands) must not wake anything: there
	// is no ref event to deliver.
	refused := &git.PushRequest{
		Commands: []git.PushCommand{{Old: zero, New: "1111111111111111111111111111111111111111", Ref: "refs/pull/1/head"}},
		Caps:     []string{"report-status"},
	}
	var out2 strings.Builder
	if err := s.pushPipeline(ctx, mustRepoID(t, "o/r"), p, repo, refused, nil, 1<<20, &out2); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(woke) != 1 {
		t.Fatalf("refused push woke the bridge: %v", woke)
	}
}

// TestJobTokenPushIsMarkedAndDoesNotWake pins round 3's GITHUB_TOKEN rule at
// the push funnel: a push authenticated by a Threads job token (JobRepo set)
// still lands and fans out, but it is marked byJob and never wakes the events
// bridge — a `contents: write` job's own push can retrigger neither a `push`
// run (the Threads hook refuses byJob) nor a `synchronize`. A human push right
// after is unmarked and wakes the bridge.
func TestJobTokenPushIsMarkedAndDoesNotWake(t *testing.T) {
	eng := &fakeEngine{}
	var mu sync.Mutex
	var woke []string
	var marked []bool
	s, _ := newTestServer(t, func(o *Options) {
		o.Engine = eng
		o.OnPush = func(id git.RepoId, refs []PushedRef, byJob bool) {
			mu.Lock()
			marked = append(marked, byJob)
			mu.Unlock()
		}
		o.Notifier = func(repo string) {
			mu.Lock()
			woke = append(woke, repo)
			mu.Unlock()
		}
	})
	eng.exists = true
	eng.placement = Placement{Serve: true}
	root := t.TempDir()
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	repo, err := eng.Repo(ctx, mustRepoID(t, "o/r"), true, git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	zero := strings.Repeat("0", 40)
	push := func(p auth.Principal) {
		t.Helper()
		preq := &git.PushRequest{
			Commands: []git.PushCommand{{Old: "1111111111111111111111111111111111111111", New: zero, Ref: "refs/heads/main"}},
			Caps:     []string{"report-status"},
		}
		var out strings.Builder
		if err := s.pushPipeline(ctx, mustRepoID(t, "o/r"), p, repo, preq, nil, 1<<20, &out); err != nil {
			t.Fatalf("pipeline: %v", err)
		}
		if !strings.Contains(out.String(), "ok refs/heads/main") {
			t.Fatalf("report = %q (the job push must still land)", out.String())
		}
	}
	waitMarked := func(want int) []bool {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			got := append([]bool(nil), marked...)
			mu.Unlock()
			if len(got) >= want {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("on-push hook fired %d times, want %d", len(got), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	push(auth.Principal{Name: "ci:runner1", JobRepo: "o/r", JobPush: true})
	got := waitMarked(1)
	if !got[0] {
		t.Fatal("a job-token push was not marked byJob")
	}
	mu.Lock()
	if len(woke) != 0 {
		t.Fatalf("a job-token push woke the events bridge: %v", woke)
	}
	mu.Unlock()
	// A human push right after is unmarked and wakes the bridge.
	push(auth.Principal{Name: "ada", Write: true})
	got = waitMarked(2)
	if got[1] {
		t.Fatal("a human push was marked byJob")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(woke)
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("wake calls = %d, want 1", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
