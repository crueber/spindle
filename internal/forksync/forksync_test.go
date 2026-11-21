package forksync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

type fakeRoles struct{ role string }

func (f fakeRoles) Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc) {
	return identity.Role(f.role), nil
}
func (f fakeRoles) CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError {
	return nil
}

type fakeRefs struct {
	branches map[string][2]string // repo → branch, sha
	ancestor map[string]bool      // "a>b" → verdict
	err      error
}

func (f *fakeRefs) DefaultBranch(ctx context.Context, repo string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	v, ok := f.branches[repo]
	if !ok {
		return "", "", errors.New("no refs")
	}
	return v[0], v[1], nil
}
func (f *fakeRefs) IsAncestor(ctx context.Context, repo, a, b string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.ancestor[a+">"+b], nil
}

type fakeFetcher struct {
	mu    sync.Mutex
	tip   string
	err   error
	calls int
}

func (f *fakeFetcher) Fetch(ctx context.Context, repo, upstreamRepo, branch string, progress func(label string, done uint64)) (string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	progress("fetching objects", 1)
	if f.err != nil {
		return "", f.err
	}
	return f.tip, nil
}

type publishCall struct {
	repo, branch, oldSHA, newSHA string
}

type fakePublisher struct {
	mu    sync.Mutex
	calls []publishCall
	err   error
}

func (f *fakePublisher) FastForward(ctx context.Context, repo, branch, oldSHA, newSHA string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, publishCall{repo, branch, oldSHA, newSHA})
	return nil
}

type fakeLogger struct {
	notices  []string
	progress []string
}

func (l *fakeLogger) Notice(s string) { l.notices = append(l.notices, s) }
func (l *fakeLogger) Progress(label string, done, total uint64, unit string) {
	l.progress = append(l.progress, label)
}

type fakeTasks struct {
	mu      sync.Mutex
	next    int
	records map[string]TaskInfo
	running map[string]string
	done    map[string]string
}

func newFakeTasks() *fakeTasks {
	return &fakeTasks{records: map[string]TaskInfo{}, running: map[string]string{}, done: map[string]string{}}
}

func (t *fakeTasks) Start(ctx context.Context, repo, kind string, params map[string]string, fn func(ctx context.Context, tl TaskLogger) error) (string, error) {
	key := repo + "/" + kind
	t.mu.Lock()
	// Single-flight model: one body per (repo, kind); later starts join.
	if id, ok := t.running[key]; ok {
		t.mu.Unlock()
		return id, nil
	}
	if id, ok := t.done[key]; ok {
		t.mu.Unlock()
		return id, nil
	}
	t.next++
	id := "task-" + string(rune('0'+t.next))
	t.running[key] = id
	t.records[id] = TaskInfo{ID: id, Kind: kind, Repo: repo}
	t.mu.Unlock()
	logger := &fakeLogger{}
	ok := fn(ctx, logger) == nil
	t.mu.Lock()
	rec := t.records[id]
	rec.OK = &ok
	t.records[id] = rec
	t.done[key] = id
	delete(t.running, key)
	t.mu.Unlock()
	return id, nil
}

func (t *fakeTasks) Get(id string) (TaskInfo, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.records[id]
	return r, ok
}

func newSvc(t *testing.T) (*Service, *fakeRefs, *fakeFetcher, *fakePublisher, *fakeTasks) {
	t.Helper()
	st := store.NewMemory()
	if _, err := store.PutBytes(context.Background(), st, "repos/me/fork/fork.json", []byte(`{"parent":"acme/base"}`), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	refs := &fakeRefs{branches: map[string][2]string{
		"acme/base": {"main", "aaaa"},
		"me/fork":   {"main", "bbbb"},
	}, ancestor: map[string]bool{"bbbb>aaaa": true}}
	fetch := &fakeFetcher{tip: "aaaa"}
	pub := &fakePublisher{}
	tasks := newFakeTasks()
	svc := &Service{
		Store: st, Roles: fakeRoles{string(identity.RoleAdmin)}, Refs: refs, Fetcher: fetch,
		Publisher: pub, Tasks: tasks,
		Guard: func(raw string) error { return nil },
		UpstreamURL: func(parent string) (string, error) {
			return "https://git.example.com/" + parent + ".git", nil
		},
	}
	return svc, refs, fetch, pub, tasks
}

func p() auth.Principal { return auth.Principal{Name: "me"} }

func TestSyncFastForward(t *testing.T) {
	svc, _, fetch, pub, tasks := newSvc(t)
	id, err := svc.Start(context.Background(), "me", "fork", p())
	if err != nil {
		t.Fatal(err)
	}
	if fetch.calls != 1 {
		t.Fatalf("fetch calls %d", fetch.calls)
	}
	if len(pub.calls) != 1 || pub.calls[0].oldSHA != "bbbb" || pub.calls[0].newSHA != "aaaa" {
		t.Fatalf("publish %+v", pub.calls)
	}
	if info, ok := tasks.Get(id); !ok || info.OK == nil || !*info.OK {
		t.Fatalf("task %+v", info)
	}
	doc, err := svc.LastOutcome(context.Background(), "me", "fork")
	if err != nil || doc == nil {
		t.Fatalf("sidecar %v %v", doc, err)
	}
	if doc.Result != ResultFastForward || doc.UpstreamSHA != "aaaa" {
		t.Fatalf("sidecar %+v", doc)
	}
}

func TestSyncAlreadyCurrentSkipsFetch(t *testing.T) {
	svc, refs, fetch, pub, _ := newSvc(t)
	refs.branches["me/fork"] = [2]string{"main", "aaaa"}
	if _, err := svc.Start(context.Background(), "me", "fork", p()); err != nil {
		t.Fatal(err)
	}
	if fetch.calls != 0 {
		t.Fatalf("fetch calls %d, want 0", fetch.calls)
	}
	if len(pub.calls) != 0 {
		t.Fatalf("published %+v", pub.calls)
	}
	doc, _ := svc.LastOutcome(context.Background(), "me", "fork")
	if doc.Result != ResultCurrent {
		t.Fatalf("result %q", doc.Result)
	}
}

func TestSyncDivergedLinksPR(t *testing.T) {
	svc, refs, _, pub, _ := newSvc(t)
	refs.ancestor["bbbb>aaaa"] = false
	if _, err := svc.Start(context.Background(), "me", "fork", p()); err != nil {
		t.Fatal(err)
	}
	if len(pub.calls) != 0 {
		t.Fatalf("diverged must not publish: %+v", pub.calls)
	}
	doc, _ := svc.LastOutcome(context.Background(), "me", "fork")
	if doc.Result != ResultDiverged {
		t.Fatalf("result %q", doc.Result)
	}
	if !strings.Contains(doc.PRURL, "/acme/base/pulls/new") || !strings.Contains(doc.PRURL, "head=me%2Ffork") {
		t.Fatalf("pr url %q", doc.PRURL)
	}
}

func TestSyncUpstreamUnreachable(t *testing.T) {
	svc, _, fetch, pub, tasks := newSvc(t)
	fetch.err = errors.New("connection refused")
	id, err := svc.Start(context.Background(), "me", "fork", p())
	if err != nil {
		t.Fatal(err) // the task carries the failure
	}
	info, _ := tasks.Get(id)
	if info.OK == nil || *info.OK {
		t.Fatalf("task should fail: %+v", info)
	}
	if len(pub.calls) != 0 {
		t.Fatal("published on a failed fetch")
	}
	doc, _ := svc.LastOutcome(context.Background(), "me", "fork")
	if doc.Result != ResultFailed || !strings.Contains(doc.Message, "connection refused") {
		t.Fatalf("sidecar %+v", doc)
	}
}

func TestSyncSSRFDenied(t *testing.T) {
	svc, _, fetch, _, _ := newSvc(t)
	svc.Guard = func(raw string) error { return errors.New("resolves to a non-public address (refused)") }
	if _, err := svc.Start(context.Background(), "me", "fork", p()); err != nil {
		t.Fatal(err)
	}
	if fetch.calls != 0 {
		t.Fatalf("fetch ran past the SSRF gate (%d calls)", fetch.calls)
	}
	doc, _ := svc.LastOutcome(context.Background(), "me", "fork")
	if doc.Result != ResultFailed || !strings.Contains(doc.Message, "non-public") {
		t.Fatalf("sidecar %+v", doc)
	}
}

func TestSyncSingleFlight(t *testing.T) {
	svc, _, fetch, _, tasks := newSvc(t)
	id1, err := svc.Start(context.Background(), "me", "fork", p())
	if err != nil {
		t.Fatal(err)
	}
	id2, err := svc.Start(context.Background(), "me", "fork", p())
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("ids %q %q", id1, id2)
	}
	if len(tasks.records) != 1 {
		t.Fatalf("records %d", len(tasks.records))
	}
	if fetch.calls != 1 {
		t.Fatalf("fetch calls %d", fetch.calls)
	}
}

func TestSyncProgressPackets(t *testing.T) {
	svc, _, _, _, _ := newSvc(t)
	log := &fakeLogger{}
	if err := svc.Run(context.Background(), "me/fork", "acme/base", p(), log); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(log.progress, ",")
	for _, want := range []string{"upstream refs", "fetch", "ancestry", "publish", "done"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing progress %q in %q", want, joined)
		}
	}
	if len(log.notices) == 0 {
		t.Fatal("no notices")
	}
}

func TestSyncNotAFork(t *testing.T) {
	svc, _, _, _, _ := newSvc(t)
	if _, err := svc.Start(context.Background(), "acme", "base", p()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want invalid, got %v", err)
	}
}

func TestSyncPublishConflict(t *testing.T) {
	svc, _, _, pub, tasks := newSvc(t)
	pub.err = errors.New("stale old-oid")
	id, _ := svc.Start(context.Background(), "me", "fork", p())
	info, _ := tasks.Get(id)
	if info.OK == nil || *info.OK {
		t.Fatalf("conflict must fail the task: %+v", info)
	}
}

func do(h *Handler, method, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHTTPStartAndOutcome(t *testing.T) {
	svc, _, _, _, _ := newSvc(t)
	h := &Handler{Svc: svc}
	w := do(h, "POST", "/me/fork/api/sync-fork")
	if w.Code != http.StatusAccepted {
		t.Fatalf("post %d %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["task"]; !ok {
		t.Fatalf("no task: %v", out)
	}
	g := do(h, "GET", "/me/fork/api/sync-fork")
	if g.Code != 200 || !strings.Contains(g.Body.String(), "fast-forward") {
		t.Fatalf("get %d %s", g.Code, g.Body.String())
	}
	if g := do(h, "POST", "/me/fork/releases/x"); g.Code != http.StatusNotFound {
		t.Fatalf("foreign path %d", g.Code)
	}
	if g := do(h, "PUT", "/me/fork/api/sync-fork"); g.Code != http.StatusMethodNotAllowed {
		t.Fatalf("put %d", g.Code)
	}
}

func TestHTTPGates(t *testing.T) {
	svc, _, _, _, _ := newSvc(t)
	svc.Roles = fakeRoles{string(identity.RoleRead)}
	h := &Handler{Svc: svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "reader"}, nil
	}}
	if w := do(h, "POST", "/me/fork/api/sync-fork"); w.Code != http.StatusForbidden {
		t.Fatalf("reader: %d", w.Code)
	}
	svc.Roles = fakeRoles{}
	h = &Handler{Svc: svc}
	if w := do(h, "POST", "/me/fork/api/sync-fork"); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", w.Code)
	}
}
