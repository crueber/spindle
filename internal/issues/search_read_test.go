package issues

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

// search_read_test.go — the P4 read projection internal/search consumes
// (search_read.go). The fixtures are the real issue mutators, so the index
// cards, immutable events and the moderation fold are the same objects the
// page/JSON lanes serve.

func TestSearchProjectionMetadataAndBodies(t *testing.T) {
	s := testService(newFakeRoles())
	th := mustCreate(t, s, "acme", "repo", janeP, "Crash on empty ref", "the description needle")
	if _, err := s.AddComment(reqCtx(), "acme", "repo", th.Num, bobP, "a comment needle"); err != nil {
		t.Fatal(err)
	}
	// A no-body timeline event is scanned (it counts) but contributes no text.
	raw, _ := json.Marshal(Event{Seq: 2, Type: EventLabelsChanged, Actor: "mod", At: "2026-09-04T12:00:00Z", Added: []string{"bug"}})
	mustPut(t, s, EventKey("acme", "repo", th.Num, 2), raw)
	// A non-opened/commented event that happens to carry a body is still not
	// searchable text: only the thread title is metadata, and only the opened
	// and commented events are body/comment text.
	nonBody := "not searchable text"
	raw, _ = json.Marshal(Event{Seq: 3, Type: EventTitleChanged, Actor: "mod", At: "2026-09-04T12:00:00Z", Body: &nonBody, From: strPtr("old"), To: strPtr("new")})
	mustPut(t, s, EventKey("acme", "repo", th.Num, 3), raw)

	proj, err := s.SearchProjection(reqCtx(), "acme", "repo", 10, 10)
	if err != nil {
		t.Fatalf("SearchProjection: %v", err)
	}
	if !proj.IndexComplete {
		t.Fatalf("index complete = false: %+v", proj)
	}
	if len(proj.Cards) != 1 || proj.Cards[0].Title != "Crash on empty ref" {
		t.Fatalf("cards = %+v", proj.Cards)
	}
	if proj.ThreadsScanned != 1 || proj.EventsScanned != 4 || proj.ThreadsTruncated || proj.EventsTruncated {
		t.Fatalf("scan = %+v", proj)
	}
	if len(proj.Threads) != 1 || len(proj.Threads[0].Texts) != 2 {
		t.Fatalf("threads = %+v", proj.Threads)
	}
	if proj.Threads[0].Texts[0].Kind != "body" || proj.Threads[0].Texts[1].Kind != "comment" {
		t.Fatalf("text kinds = %+v", proj.Threads[0].Texts)
	}

	// Metadata-only: no event and no moderation read.
	meta, err := s.SearchProjection(reqCtx(), "acme", "repo", 0, 0)
	if err != nil {
		t.Fatalf("metadata projection: %v", err)
	}
	if meta.ThreadsScanned != 0 || meta.EventsScanned != 0 || len(meta.Threads) != 0 {
		t.Fatalf("metadata scan = %+v", meta)
	}
	if len(meta.Cards) != 1 {
		t.Fatalf("metadata cards = %+v", meta.Cards)
	}
}

func TestSearchProjectionRedactionAndModerationError(t *testing.T) {
	s := testService(newFakeRoles())
	th := mustCreate(t, s, "acme", "repo", janeP, "redacted", "secret needle")
	if _, err := s.AddComment(reqCtx(), "acme", "repo", th.Num, bobP, "public needle"); err != nil {
		t.Fatal(err)
	}
	mod := newFakeModeration()
	mod.hide(hiddenTarget(th.Num, 0))
	s.Moderation = mod

	proj, err := s.SearchProjection(reqCtx(), "acme", "repo", 10, 10)
	if err != nil {
		t.Fatalf("SearchProjection: %v", err)
	}
	if len(proj.Threads) != 1 || len(proj.Threads[0].Texts) != 1 {
		t.Fatalf("redacted texts = %+v", proj.Threads)
	}
	if got := proj.Threads[0].Texts[0].Text; strings.Contains(got, "secret") {
		t.Fatalf("hidden body leaked: %q", got)
	}

	// A moderation read failure fails the projection closed.
	mod.err = errString("moderation down")
	if _, err := s.SearchProjection(reqCtx(), "acme", "repo", 10, 10); err == nil {
		t.Fatal("moderation failure was swallowed")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// listFailStore is a store whose LIST fails while exact-key GETs still work:
// the search projection must fail closed rather than serving a partial scan.
type listFailStore struct{ store.ObjectStore }

func (l listFailStore) List(context.Context, string, string, func(store.ObjectMeta) error) error {
	return errString("list unavailable")
}

func TestSearchProjectionStoreFailures(t *testing.T) {
	s := testService(newFakeRoles())
	mustCreate(t, s, "acme", "repo", janeP, "one", "body")
	orig := s.Store

	// A failing event LIST fails the body scan.
	s.Store = listFailStore{orig}
	if _, err := s.SearchProjection(reqCtx(), "acme", "repo", 10, 10); err == nil {
		t.Fatal("event LIST failure was swallowed")
	}

	// A failing header LIST fails the index fallback.
	s.Store = orig
	raw, _ := json.Marshal(Index{Version: 99, Open: []Card{}, ClosedRecent: []Card{}})
	mustPut(t, s, IndexKey("acme", "repo"), raw)
	s.Store = listFailStore{orig}
	if _, err := s.SearchProjection(reqCtx(), "acme", "repo", 0, 0); err == nil {
		t.Fatal("header LIST failure was swallowed")
	}
}

func TestSearchProjectionIndexFallback(t *testing.T) {
	s := testService(newFakeRoles())
	mustCreate(t, s, "acme", "repo", janeP, "one", "b")
	mustCreate(t, s, "acme", "repo", janeP, "two", "b")

	// Overwrite the index without the current card projection: the header
	// scan is the fallback, and index_complete stays false.
	raw, _ := json.Marshal(Index{Version: 99, Open: []Card{}, ClosedRecent: []Card{}})
	mustPut(t, s, IndexKey("acme", "repo"), raw)
	proj, err := s.SearchProjection(reqCtx(), "acme", "repo", 10, 10)
	if err != nil {
		t.Fatalf("SearchProjection: %v", err)
	}
	if proj.IndexComplete || len(proj.Cards) != 2 {
		t.Fatalf("fallback projection = %+v", proj)
	}

	// An empty repository is a complete empty answer, not an index gap.
	empty, err := s.SearchProjection(reqCtx(), "acme", "empty", 10, 10)
	if err != nil {
		t.Fatalf("empty projection: %v", err)
	}
	if !empty.IndexComplete || len(empty.Cards) != 0 {
		t.Fatalf("empty projection = %+v", empty)
	}

	// A corrupt index fails closed.
	mustPut(t, s, IndexKey("acme", "bad"), []byte("{bad"))
	if _, err := s.SearchProjection(reqCtx(), "acme", "bad", 10, 10); err == nil {
		t.Fatal("corrupt index was swallowed")
	}

	// A corrupt header during the fallback scan fails closed too.
	mustPut(t, s, IndexKey("acme", "badhdr"), raw)
	mustPut(t, s, CounterKey("acme", "badhdr"), []byte(`{"next":2}`))
	mustPut(t, s, ThreadKey("acme", "badhdr", 1), []byte("{bad"))
	if _, err := s.SearchProjection(reqCtx(), "acme", "badhdr", 10, 10); err == nil {
		t.Fatal("corrupt header was swallowed")
	}

	// Non-issue/pr threads are not search candidates (the shared family is
	// typed, and the scan must not invent a kind).
	mustPut(t, s, IndexKey("acme", "other"), raw)
	mustPut(t, s, CounterKey("acme", "other"), []byte(`{"next":2}`))
	th := Thread{Num: 1, Kind: "other", Title: "not a thread", State: StateOpen, Author: "a"}
	body, _ := json.Marshal(th)
	mustPut(t, s, ThreadKey("acme", "other", 1), body)
	other, err := s.SearchProjection(reqCtx(), "acme", "other", 10, 10)
	if err != nil {
		t.Fatalf("other projection: %v", err)
	}
	if len(other.Cards) != 0 {
		t.Fatalf("unknown kind leaked: %+v", other.Cards)
	}

	// A corrupt event in an otherwise valid thread fails the body projection
	// closed (the searcher maps it to an unavailable repository).
	t1 := mustCreate(t, s, "acme", "evbad", janeP, "bad event", "b")
	mustPut(t, s, EventKey("acme", "evbad", t1.Num, 1), []byte("{bad"))
	if _, err := s.SearchProjection(reqCtx(), "acme", "evbad", 10, 10); err == nil {
		t.Fatal("corrupt event was swallowed")
	}
}

func TestSearchProjectionClosedCard(t *testing.T) {
	s := testService(newFakeRoles())
	th := mustCreate(t, s, "acme", "repo", janeP, "closed thread", "body")
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", th.Num, janeP, IssuePatch{State: strPtr(StateClosed)}); err != nil {
		t.Fatal(err)
	}
	proj, err := s.SearchProjection(reqCtx(), "acme", "repo", 0, 0)
	if err != nil {
		t.Fatalf("SearchProjection: %v", err)
	}
	if len(proj.Cards) != 1 || proj.Cards[0].State != StateClosed || !proj.IndexComplete {
		t.Fatalf("closed card = %+v", proj)
	}
}

func TestSearchProjectionCaps(t *testing.T) {
	s := testService(newFakeRoles())
	var last *Thread
	for i := 0; i < 12; i++ {
		last = mustCreate(t, s, "acme", "repo", janeP, "thread", "body")
	}
	for i := 0; i < 5; i++ {
		if _, err := s.AddComment(reqCtx(), "acme", "repo", last.Num, bobP, "comment"); err != nil {
			t.Fatal(err)
		}
	}
	proj, err := s.SearchProjection(reqCtx(), "acme", "repo", 5, 100)
	if err != nil {
		t.Fatalf("SearchProjection: %v", err)
	}
	if !proj.ThreadsTruncated || proj.ThreadsScanned != 5 {
		t.Fatalf("thread cap = %+v", proj)
	}

	// The event cap stops mid-thread (events_truncated) and leaves later
	// threads unread (threads_truncated).
	events, err := s.SearchProjection(reqCtx(), "acme", "repo", 12, 3)
	if err != nil {
		t.Fatalf("event projection: %v", err)
	}
	if !events.EventsTruncated || events.EventsScanned != 3 {
		t.Fatalf("event cap = %+v", events)
	}
	if !events.ThreadsTruncated {
		t.Fatalf("event cap did not mark the unread threads: %+v", events)
	}

	// A zero event budget with a thread cap is the same honest truncation.
	none, err := s.SearchProjection(reqCtx(), "acme", "repo", 2, 0)
	if err != nil {
		t.Fatalf("zero event projection: %v", err)
	}
	if !none.ThreadsTruncated || none.ThreadsScanned != 0 {
		t.Fatalf("zero event budget = %+v", none)
	}

	// scanEventsBounded's zero-limit guard.
	if got, more, err := s.scanEventsBounded(reqCtx(), "acme", "repo", 1, 0); err != nil || !more || got != nil {
		t.Fatalf("zero-limit events = %+v more=%v err=%v", got, more, err)
	}
}
