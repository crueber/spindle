package notify

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// pushEvents is a small batch with one branch update and one tag create.
func pushEvents() []PushEvent {
	return []PushEvent{
		{
			Seq: 41, Action: ActionUpdateWire, RefType: "branch",
			RefName: "refs/heads/main", Before: strings.Repeat("a", 40), After: strings.Repeat("b", 40),
			Pusher: "amy@example.com", EntryKind: "push", RequestID: "req-1",
			Commits: []PushCommit{{
				ID: strings.Repeat("b", 40), Author: "Amy", Email: "amy@example.com",
				Message: "fix the thing", At: "2026-11-21T10:00:00Z",
			}},
		},
		{
			Seq: 42, Action: ActionCreateWire, RefType: "tag",
			RefName: "refs/tags/v1.0.0", Before: strings.Repeat("0", 40), After: strings.Repeat("c", 40),
			Pusher: "amy@example.com", EntryKind: "push", RequestID: "req-1",
		},
	}
}

func TestPushTopicsVocabularyAndValidation(t *testing.T) {
	topics := PushTopics()
	if len(topics) != 7 || topics[0] != TopicPush {
		t.Fatalf("PushTopics() = %v", topics)
	}
	for _, topic := range topics {
		if err := validateHookEvents([]string{topic}); err != nil {
			t.Errorf("repo hook must accept %q: %v", topic, err)
		}
		if !IsPushTopic(topic) {
			t.Errorf("%q not recognized as a push topic", topic)
		}
	}
	// Unknown stays rejected; org hooks reject every push topic (no repo).
	if err := validateHookEvents([]string{"branch.exploded"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown topic error = %v", err)
	}
	for _, topic := range topics {
		if err := validateOrgHookEvents([]string{topic}); !errors.Is(err, ErrInvalid) {
			t.Errorf("org hook must reject %q, got %v", topic, err)
		}
	}
	if err := validateOrgHookEvents(OrgHookTopics()); err != nil {
		t.Fatalf("OrgHookTopics must validate: %v", err)
	}
	for _, topic := range PushTopics() {
		if contains(OrgHookTopics(), topic) {
			t.Errorf("org vocabulary must not offer %q", topic)
		}
	}
	if err := validateHookEvents(RepoHookTopics()); err != nil {
		t.Fatalf("RepoHookTopics must validate: %v", err)
	}
}

func TestPushTopicMappingAndUmbrellaMatch(t *testing.T) {
	cases := []struct{ action, refType, want string }{
		{ActionCreateWire, "branch", TopicBranchCreated},
		{ActionUpdateWire, "branch", TopicBranchUpdated},
		{ActionDeleteWire, "branch", TopicBranchDeleted},
		{ActionCreateWire, "tag", TopicTagCreated},
		{ActionUpdateWire, "tag", TopicTagUpdated},
		{ActionDeleteWire, "tag", TopicTagDeleted},
		{ActionUpdateWire, "", TopicPush}, // e.g. refs/notes/*
		{"weird", "branch", TopicPush},
	}
	for _, c := range cases {
		if got := pushTopic(c.action, c.refType); got != c.want {
			t.Errorf("pushTopic(%q,%q) = %q, want %q", c.action, c.refType, got, c.want)
		}
	}
	// "push" selects the family; a concrete filter selects only itself.
	if !hookMatches([]string{TopicPush}, TopicBranchCreated) {
		t.Fatal("push umbrella must match branch.created")
	}
	if !hookMatches([]string{TopicPush}, TopicPush) {
		t.Fatal("push umbrella must match a bare push event")
	}
	if hookMatches([]string{TopicBranchCreated}, TopicBranchUpdated) {
		t.Fatal("concrete topic must not match a sibling")
	}
	if hookMatches([]string{"commented"}, TopicBranchUpdated) {
		t.Fatal("collaboration filter must not match push")
	}
}

func TestPushSinkAppendsTopics(t *testing.T) {
	x := newHarness(t)
	if err := x.svc.PushSink(ctx(), "acme/repo", pushEvents()); err != nil {
		t.Fatal(err)
	}
	// Two events, contiguous activity seqs, action = the concrete topic.
	first := x.svc.readActivity(ctx(), "acme", "repo", 1)
	second := x.svc.readActivity(ctx(), "acme", "repo", 2)
	if first == nil || second == nil {
		t.Fatalf("push events not appended: %+v %+v", first, second)
	}
	if first.Action != TopicBranchUpdated || first.Kind != "repo" || first.Actor != "amy@example.com" {
		t.Fatalf("first event = %+v", first)
	}
	if second.Action != TopicTagCreated {
		t.Fatalf("second event = %+v", second)
	}
	var payload activityPayload
	if err := json.Unmarshal(first.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Class != "push" || len(payload.Recipients) != 0 || payload.FanoutPending {
		t.Fatalf("payload = %+v", payload)
	}
	raw, _ := json.Marshal(payload.Detail)
	var detail PushDetail
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Ref != "refs/heads/main" || detail.RefType != "branch" ||
		detail.Before != strings.Repeat("a", 40) || detail.After != strings.Repeat("b", 40) ||
		detail.Repository != "acme/repo" || detail.Pusher != "amy@example.com" {
		t.Fatalf("detail = %+v", detail)
	}
	if detail.Walgit.Seq != "41" || detail.Walgit.EntryKind != "push" || detail.Walgit.RequestID != "req-1" {
		t.Fatalf("walgit block = %+v", detail.Walgit)
	}
	if len(detail.Commits) != 1 || detail.Commits[0].Message != "fix the thing" {
		t.Fatalf("commits = %+v", detail.Commits)
	}
	// A batch reserved ONE block: next emission continues after it.
	x.svc.EmitIssue(ctx(), "acme/repo", 3, "subscribed", "amy@example.com", "", "commented", []string{})
	if ev := x.svc.readActivity(ctx(), "acme", "repo", 3); ev == nil {
		t.Fatal("seq after a push batch should be 3")
	}
}

func TestWantsPush(t *testing.T) {
	x := newHarness(t)
	if ok, err := x.svc.WantsPush(ctx(), "acme", "repo"); err != nil || ok {
		t.Fatalf("no hooks: %v %v", ok, err)
	}
	// Collaboration-only hook: no push history is recorded for it.
	if _, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{
		URL: strPtr("https://example.com/hook"), Events: []string{"commented"},
	}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := x.svc.WantsPush(ctx(), "acme", "repo"); ok {
		t.Fatal("collab-only hook must not want push")
	}
	// Inactive push hook counts for nothing.
	h, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{
		URL: strPtr("https://example.com/push"), Events: []string{TopicPush}, Active: boolPtr(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := x.svc.WantsPush(ctx(), "acme", "repo"); ok {
		t.Fatal("inactive hook must not want push")
	}
	if _, err := x.svc.PatchHook(ctx(), "acme", "repo", h.ID, HookSpec{Active: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := x.svc.WantsPush(ctx(), "acme", "repo"); !ok {
		t.Fatal("active push hook must want push")
	}
}

// TestPushDeliveryEndToEnd drives the real per-hook delivery path against a
// local receiver: method, keeper headers, HMAC, payload fields.
func TestPushDeliveryEndToEnd(t *testing.T) {
	x := newHarness(t)
	rec := &pushRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	h, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{
		URL: strPtr(srv.URL), Events: []string{TopicPush}, Secret: strPtr("s3cr3t"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := x.svc.PushSink(ctx(), "acme/repo", pushEvents()); err != nil {
		t.Fatal(err)
	}
	x.svc.DeliverRepo(ctx(), "acme", "repo")

	if rec.count() != 2 {
		t.Fatalf("deliveries = %d, want 2 (branch update + tag create)", rec.count())
	}
	first := rec.at(0)
	if first.method != http.MethodPost {
		t.Fatalf("method = %q", first.method)
	}
	if first.event != TopicBranchUpdated {
		t.Fatalf("X-Walgit-Event = %q", first.event)
	}
	if first.contentType != "application/json" || first.delivery == "" {
		t.Fatalf("keeper headers = %+v", first)
	}
	if !strings.HasPrefix(first.signature, "sha256=") {
		t.Fatalf("signature = %q", first.signature)
	}
	var ev ActivityEvent
	if err := json.Unmarshal(first.body, &ev); err != nil {
		t.Fatal(err)
	}
	var payload activityPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(payload.Detail)
	var detail PushDetail
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Ref != "refs/heads/main" || detail.After != strings.Repeat("b", 40) ||
		detail.Repository != "acme/repo" || len(detail.Commits) != 1 {
		t.Fatalf("delivered detail = %+v", detail)
	}
	if rec.at(1).event != TopicTagCreated {
		t.Fatalf("second X-Walgit-Event = %q", rec.at(1).event)
	}
	if cur := x.svc.readCursor(ctx(), "acme", "repo", h.ID); cur != 2 {
		t.Fatalf("cursor = %d, want 2", cur)
	}
}

// TestPushDeliveryFilteredProducesNone: a hook subscribed only to a
// collaboration action receives no push delivery, while its cursor advances.
func TestPushDeliveryFilteredProducesNone(t *testing.T) {
	x := newHarness(t)
	rec := &pushRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	h, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{
		URL: strPtr(srv.URL), Events: []string{"commented"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := x.svc.PushSink(ctx(), "acme/repo", pushEvents()); err != nil {
		t.Fatal(err)
	}
	x.svc.DeliverRepo(ctx(), "acme", "repo")
	if rec.count() != 0 {
		t.Fatalf("filtered hook received %d push deliveries", rec.count())
	}
	if cur := x.svc.readCursor(ctx(), "acme", "repo", h.ID); cur != 2 {
		t.Fatalf("filtered cursor = %d, want 2 (advanced past)", cur)
	}
}

// TestPushDeliveryRetriesOn500: a 500 holds the cursor; a later pass redelivers
// the same event and only then advances.
func TestPushDeliveryRetriesOn500(t *testing.T) {
	x := newHarness(t)
	rec := &pushRecorder{fail: true}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	h, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{
		URL: strPtr(srv.URL), Events: []string{TopicPush},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := x.svc.PushSink(ctx(), "acme/repo", pushEvents()); err != nil {
		t.Fatal(err)
	}
	x.svc.DeliverRepo(ctx(), "acme", "repo")
	if cur := x.svc.readCursor(ctx(), "acme", "repo", h.ID); cur != 0 {
		t.Fatalf("cursor after 500 = %d, want 0", cur)
	}
	// The failed attempt is on the ring (status 500), with the body retained.
	d := x.svc.ReadDeliveries(ctx(), "acme", "repo", h.ID)
	if len(d.Entries) != 1 || d.Entries[0].Status != 500 || len(d.Entries[0].Body) == 0 {
		t.Fatalf("deliveries after 500 = %+v", d.Entries)
	}
	rec.setFail(false)
	x.svc.DeliverRepo(ctx(), "acme", "repo")
	if cur := x.svc.readCursor(ctx(), "acme", "repo", h.ID); cur != 2 {
		t.Fatalf("cursor after recovery = %d, want 2", cur)
	}
	if rec.count() != 2 { // the 500 attempt lives on the ring, not in the receiver's 2xx log
		t.Fatalf("successful posts = %d, want 2", rec.count())
	}
}

// TestPushRedeliverReplaysBytes: redeliver re-sends the retained body with the
// same X-Walgit-Delivery id.
func TestPushRedeliverReplaysBytes(t *testing.T) {
	x := newHarness(t)
	rec := &pushRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	h, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{
		URL: strPtr(srv.URL), Events: []string{TopicPush}, Secret: strPtr("s3cr3t"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := x.svc.PushSink(ctx(), "acme/repo", pushEvents()); err != nil {
		t.Fatal(err)
	}
	x.svc.DeliverRepo(ctx(), "acme", "repo")
	original := rec.at(0)

	got, err := x.svc.RedeliverHook(ctx(), "acme", "repo", h.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != 1 || got.Status != 200 {
		t.Fatalf("redeliver row = %+v", got)
	}
	replay := rec.at(2)
	if string(replay.body) != string(original.body) {
		t.Fatal("redeliver body differs from the original")
	}
	if replay.delivery != original.delivery {
		t.Fatalf("delivery id changed: %q vs %q", replay.delivery, original.delivery)
	}
	if replay.signature != original.signature {
		t.Fatal("signature changed on replay")
	}
	if cur := x.svc.readCursor(ctx(), "acme", "repo", h.ID); cur != 2 {
		t.Fatalf("redeliver must not move the cursor: %d", cur)
	}
}

func TestPushSinkRejectsBadRepo(t *testing.T) {
	x := newHarness(t)
	if err := x.svc.PushSink(ctx(), "not-a-repo", pushEvents()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad repo error = %v", err)
	}
	if err := x.svc.PushSink(ctx(), "acme/repo", nil); err != nil {
		t.Fatalf("empty batch = %v", err)
	}
}

// --- local receiver ---------------------------------------------------------

type pushPost struct {
	method      string
	contentType string
	delivery    string
	signature   string
	event       string
	body        []byte
}

type pushRecorder struct {
	mu    sync.Mutex
	fail  bool
	posts []pushPost
}

func (r *pushRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body := make([]byte, 0)
	buf := make([]byte, 4096)
	for {
		n, err := req.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	r.mu.Lock()
	fail := r.fail
	if !fail {
		r.posts = append(r.posts, pushPost{
			method: req.Method, contentType: req.Header.Get("Content-Type"),
			delivery: req.Header.Get("X-Walgit-Delivery"), signature: req.Header.Get("X-Walgit-Signature"),
			event: req.Header.Get("X-Walgit-Event"), body: body,
		})
	}
	r.mu.Unlock()
	if fail {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (r *pushRecorder) setFail(v bool) {
	r.mu.Lock()
	r.fail = v
	r.mu.Unlock()
}

func (r *pushRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.posts)
}

func (r *pushRecorder) at(i int) pushPost {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.posts[i]
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
