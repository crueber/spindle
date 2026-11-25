// index_roundtrip_test.go — the shared repos/<o>/<r>/issues/index.json is ONE
// document two packages write: 02 (issues) owns the shape and stamps
// card_version/pinned, 03 (pulls) appends PR cards. Because pulls rewrites the
// whole document on every PR mutation, its mirror must be lossless: a
// whitelist struct drops the NEXT additive field (envelope or card) on the
// first read-modify-write, so the field-wise fix alone would ship the same bug
// class one release later. Law 5: additive JSON fields round-trip unchanged.
//
// These tests pin the structure, not a list of known fields: unknown keys on
// the envelope, on an issue card and on a PR card all survive the PR path, and
// a document with no extras marshals to exactly the same bytes as before.
package pulls

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

// reindex reads the shared index document back as raw objects so a test can
// assert keys the Go structs do not name.
func reindex(t *testing.T, e *testEnv) (map[string]json.RawMessage, []map[string]json.RawMessage) {
	t.Helper()
	raw, _, err := store.GetBytes(ctx(), e.store, IndexKey("o", "r"), store.GetOptions{})
	if err != nil {
		t.Fatalf("re-read shared index: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode shared index: %v\n%s", err, raw)
	}
	var rows []map[string]json.RawMessage
	if open, ok := envelope["open"]; ok {
		if err := json.Unmarshal(open, &rows); err != nil {
			t.Fatalf("decode open cards: %v\n%s", err, open)
		}
	}
	var closed []map[string]json.RawMessage
	if recent, ok := envelope["closed_recent"]; ok {
		if err := json.Unmarshal(recent, &closed); err != nil {
			t.Fatalf("decode closed cards: %v\n%s", err, recent)
		}
	}
	return envelope, append(rows, closed...)
}

// cardRow finds one card object by its num.
func cardRow(t *testing.T, rows []map[string]json.RawMessage, num int) map[string]json.RawMessage {
	t.Helper()
	for _, row := range rows {
		var n int
		if err := json.Unmarshal(row["num"], &n); err == nil && n == num {
			return row
		}
	}
	t.Fatalf("index has no card #%d", num)
	return nil
}

// seedIndex writes the document 02 would write, with one issue card, one PR
// card and the shared counter, each carrying an unknown additive key.
func seedIndex(t *testing.T, e *testEnv) {
	t.Helper()
	const seeded = `{"version":4,"compacted_through":"","open":[` +
		`{"num":1,"kind":"issue","title":"first","state":"open","state_reason":null,` +
		`"labels":[],"assignees":[],"milestone":null,"author":"jane@example.com",` +
		`"created_at":"2026-09-04T12:00:00Z","updated_at":"2026-09-04T12:00:00Z",` +
		`"comment_count":0,"future_issue":{"keep":true}},` +
		`{"num":2,"kind":"pr","title":"second","state":"open","state_reason":null,` +
		`"labels":[],"assignees":[],"milestone":null,"author":"jane@example.com",` +
		`"created_at":"2026-09-04T12:00:00Z","updated_at":"2026-09-04T12:00:00Z",` +
		`"comment_count":0,"future_pr":"keep"}],` +
		`"closed_recent":[],"card_version":1,"pinned":[1],"future_flag":{"x":1}}`
	if _, err := store.PutBytes(ctx(), e.store, IndexKey("o", "r"), []byte(seeded),
		store.PutOptions{ContentType: "application/json"}); err != nil {
		t.Fatalf("seed shared index: %v", err)
	}
	if _, err := store.PutBytes(ctx(), e.store, CounterKey("o", "r"), []byte(`{"next":3}`),
		store.PutOptions{ContentType: "application/json"}); err != nil {
		t.Fatalf("seed counter: %v", err)
	}
}

// TestSharedIndexRoundTripsUnknownFields is the structural guarantee: an
// unknown envelope key and unknown keys on both an issue card and a PR card
// survive a PR mutation (a full read-modify-write of the shared document).
func TestSharedIndexRoundTripsUnknownFields(t *testing.T) {
	e := newTestEnv()
	seedIndex(t, e)

	th, _ := openBasic(t, e, "o", "r")
	if th.Num != 3 {
		t.Fatalf("PR num = %d, want 3 (the shared counter)", th.Num)
	}

	envelope, rows := reindex(t, e)
	if got := string(envelope["future_flag"]); got != `{"x":1}` {
		t.Fatalf("unknown envelope key after a PR write = %s, want {\"x\":1}", got)
	}
	if got := string(cardRow(t, rows, 1)["future_issue"]); got != `{"keep":true}` {
		t.Fatalf("unknown issue-card key after a PR write = %s", got)
	}
	if got := string(cardRow(t, rows, 2)["future_pr"]); got != `"keep"` {
		t.Fatalf("unknown PR-card key after a PR write = %s", got)
	}
	// The field-wise fix's fields stay too.
	if got := string(envelope["card_version"]); got != "1" {
		t.Fatalf("card_version after a PR write = %s, want 1", got)
	}
	if got := string(envelope["pinned"]); got != "[1]" {
		t.Fatalf("pinned after a PR write = %s, want [1]", got)
	}
	if len(rows) != 3 {
		t.Fatalf("cards = %d, want the issue, the seeded PR and the new PR", len(rows))
	}
	// No bookkeeping key leaks into the wire document.
	for _, leaked := range []string{"unknown", "extras"} {
		if _, ok := envelope[leaked]; ok {
			t.Fatalf("marshal leaked the %q bookkeeping key: %v", leaked, envelope)
		}
	}
}

// TestPRCardUnknownFieldsSurviveItsOwnWrite is the card half of the guarantee:
// a PR mutation REPLACES that card (prCardOf), and the replacement still
// carries the unknown keys the previous row had.
func TestPRCardUnknownFieldsSurviveItsOwnWrite(t *testing.T) {
	e := newTestEnv()
	const seeded = `{"version":1,"compacted_through":"","open":[` +
		`{"num":1,"kind":"pr","title":"before","state":"open","state_reason":null,` +
		`"labels":[],"assignees":[],"milestone":null,"author":"jane@example.com",` +
		`"created_at":"2026-09-04T12:00:00Z","updated_at":"2026-09-04T12:00:00Z",` +
		`"comment_count":0,"future_pr":{"kept":1}}],` +
		`"closed_recent":[],"future_flag":true}`
	if _, err := store.PutBytes(ctx(), e.store, IndexKey("o", "r"), []byte(seeded),
		store.PutOptions{ContentType: "application/json"}); err != nil {
		t.Fatalf("seed shared index: %v", err)
	}

	// The same write path a PR state change takes: read, upsert the
	// re-projected card, marshal, CAS-write.
	e.svc.updateIndex(ctx(), "o", "r", Card{
		Num: 1, Kind: "pr", Title: "after", State: "open",
		Labels: []string{}, Assignees: []string{}, Author: "jane@example.com",
		CreatedAt: "2026-09-04T12:00:00Z", UpdatedAt: "2026-09-04T13:00:00Z",
	})

	envelope, rows := reindex(t, e)
	if got := string(cardRow(t, rows, 1)["future_pr"]); got != `{"kept":1}` {
		t.Fatalf("unknown PR-card key after its own write = %s, want {\"kept\":1}", got)
	}
	if got := string(cardRow(t, rows, 1)["title"]); got != `"after"` {
		t.Fatalf("known field after the write = %s, want \"after\"", got)
	}
	if got := string(envelope["future_flag"]); got != "true" {
		t.Fatalf("unknown envelope key after a card write = %s, want true", got)
	}
}

// TestNoExtrasMarshalIsUnchanged pins the byte shape for documents without
// extras: the custom marshaler must not reorder known fields, add a
// bookkeeping key, or serialize a nil extras map.
func TestNoExtrasMarshalIsUnchanged(t *testing.T) {
	card := Card{
		Num: 1, Kind: "issue", Title: "first", State: "open",
		Labels: []string{}, Assignees: []string{},
		Author:    "jane@example.com",
		CreatedAt: "2026-09-04T12:00:00Z", UpdatedAt: "2026-09-04T12:00:00Z",
	}
	got, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	const wantCard = `{"num":1,"kind":"issue","title":"first","state":"open",` +
		`"state_reason":null,"labels":[],"assignees":[],"milestone":null,` +
		`"author":"jane@example.com","created_at":"2026-09-04T12:00:00Z",` +
		`"updated_at":"2026-09-04T12:00:00Z","comment_count":0}`
	if string(got) != wantCard {
		t.Fatalf("card without extras marshaled to\n%s\nwant\n%s", got, wantCard)
	}

	ix := Index{Version: 2, Open: []Card{}, ClosedRecent: []Card{}}
	got, err = json.Marshal(ix)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	const wantIndex = `{"version":2,"compacted_through":"","open":[],"closed_recent":[]}`
	if string(got) != wantIndex {
		t.Fatalf("index without extras marshaled to\n%s\nwant\n%s", got, wantIndex)
	}
}

// TestLosslessDecodeRejectsMalformed keeps the error path honest: the custom
// decoders must fail like encoding/json did, so parseIndex still maps a broken
// document to ErrCorrupt instead of serving zero-valued cards.
func TestLosslessDecodeRejectsMalformed(t *testing.T) {
	var c Card
	if err := json.Unmarshal([]byte(`{"num":`), &c); err == nil {
		t.Fatal("Card.UnmarshalJSON accepted malformed JSON")
	}
	var ix Index
	if err := json.Unmarshal([]byte(`{"version":`), &ix); err == nil {
		t.Fatal("Index.UnmarshalJSON accepted malformed JSON")
	}
	if _, err := parseIndex([]byte(`{"open":[`)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("parseIndex(broken) = %v, want ErrCorrupt", err)
	}
}
