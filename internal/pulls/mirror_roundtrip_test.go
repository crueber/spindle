// mirror_roundtrip_test.go — thread.json and meta/next_num are ONE document
// each with two writers: 02 (issues) owns both shapes, 03 (pulls) rewrites the
// thread header in appendEvent and the counter in allocNum. The index mirror's
// P6 lesson applies to them unchanged: a whitelist struct drops the NEXT
// additive field on the first read-modify-write, so the fix is structural —
// capture every unnamed key on decode and re-emit it on encode — not a list of
// known fields. Law 5: additive JSON fields round-trip unchanged.
//
// These tests drive the REAL write paths (AddComment → appendEvent, OpenPR →
// allocNum) and assert on the stored bytes, not on the Go structs.
package pulls

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

// extraShapes is the key-value matrix every mirror must survive: a nested
// object, an array, a string, a number, a bool and JSON null (a null value is
// still a present key and must not be dropped). Values are written compact so
// the seeded bytes and the re-read bytes are directly comparable.
var extraShapes = []struct {
	key string
	val string
}{
	{"future_object", `{"nested":{"deep":[1,2]},"flag":false}`},
	{"future_array", `[1,"two",null]`},
	{"future_string", `"keep"`},
	{"future_number", `42`},
	{"future_bool", `true`},
	{"future_null", `null`},
}

// seedThreadExtras adds unknown top-level keys to an existing thread header,
// exactly as a future 02/04 writer would.
func seedThreadExtras(t *testing.T, e *testEnv, owner, repo string, num int, extras map[string]string) {
	t.Helper()
	key := ThreadKey(owner, repo, num)
	raw, ver, err := e.svc.getJSON(ctx(), key)
	if err != nil || raw == nil {
		t.Fatalf("read thread to seed: raw=%v ver=%q err=%v", raw != nil, ver, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode thread: %v", err)
	}
	for k, v := range extras {
		doc[k] = json.RawMessage(v)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode seeded thread: %v", err)
	}
	if _, err := store.PutBytes(ctx(), e.store, key, out,
		store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
}

// jsonObject re-reads one stored JSON object as raw keys.
func jsonObject(t *testing.T, e *testEnv, key string) map[string]json.RawMessage {
	t.Helper()
	raw, _, err := store.GetBytes(ctx(), e.store, key, store.GetOptions{})
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode %s: %v\n%s", key, err, raw)
	}
	return doc
}

// TestThreadRoundTripsUnknownFields is the thread half of the guarantee: an
// unknown header key survives appendEvent (the one writer of a live header).
func TestThreadRoundTripsUnknownFields(t *testing.T) {
	for _, shape := range extraShapes {
		t.Run(shape.key, func(t *testing.T) {
			e := newTestEnv()
			openBasic(t, e, "o", "r") // seeds counter + thread #1
			seedThreadExtras(t, e, "o", "r", 1, map[string]string{shape.key: shape.val})

			if _, err := e.svc.AddComment(ctx(), "o", "r", 1, writer(), "hello"); err != nil {
				t.Fatalf("AddComment: %v", err)
			}

			doc := jsonObject(t, e, ThreadKey("o", "r", 1))
			if got := string(doc[shape.key]); got != shape.val {
				t.Fatalf("unknown thread key %s after appendEvent = %s, want %s", shape.key, got, shape.val)
			}
			// The header write must still do its own job (known fields move).
			if got := string(doc["comment_count"]); got != "1" {
				t.Fatalf("comment_count after AddComment = %s, want 1", got)
			}
			if got := string(doc["next_event_seq"]); got != "2" {
				t.Fatalf("next_event_seq after AddComment = %s, want 2", got)
			}
			if _, leaked := doc["unknown"]; leaked {
				t.Fatalf("marshal leaked the bookkeeping key: %v", doc)
			}
		})
	}
}

// TestCounterRoundTripsUnknownFields is the counter half: the extra key must
// survive allocNum's read-modify-write, including the increment itself.
func TestCounterRoundTripsUnknownFields(t *testing.T) {
	for _, shape := range extraShapes {
		t.Run(shape.key, func(t *testing.T) {
			e := newTestEnv()
			seed := `{"next":5,"` + shape.key + `":` + shape.val + `}`
			if _, err := store.PutBytes(ctx(), e.store, CounterKey("o", "r"), []byte(seed),
				store.PutOptions{ContentType: "application/json"}); err != nil {
				t.Fatalf("seed counter: %v", err)
			}

			th, _ := openBasic(t, e, "o", "r")
			if th.Num != 5 {
				t.Fatalf("PR num = %d, want 5 (the seeded counter)", th.Num)
			}

			doc := jsonObject(t, e, CounterKey("o", "r"))
			if got := string(doc["next"]); got != "6" {
				t.Fatalf("counter next after allocation = %s, want 6", got)
			}
			if got := string(doc[shape.key]); got != shape.val {
				t.Fatalf("unknown counter key %s after allocNum = %s, want %s", shape.key, got, shape.val)
			}
			if _, leaked := doc["unknown"]; leaked {
				t.Fatalf("marshal leaked the bookkeeping key: %v", doc)
			}
		})
	}
}

// TestMirrorNoExtrasMarshalIsUnchanged pins the byte shape for documents
// without extras: the custom marshalers must not reorder known fields, add a
// bookkeeping key, or serialize a nil extras map. A stored header parsed and
// re-encoded is byte-identical to what 02's writer left behind.
func TestMirrorNoExtrasMarshalIsUnchanged(t *testing.T) {
	got, err := json.Marshal(Counter{Next: 3})
	if err != nil {
		t.Fatalf("marshal counter: %v", err)
	}
	if string(got) != `{"next":3}` {
		t.Fatalf("counter without extras marshaled to %s, want {\"next\":3}", got)
	}

	e := newTestEnv()
	openBasic(t, e, "o", "r")
	raw, _, err := e.svc.getJSON(ctx(), ThreadKey("o", "r", 1))
	if err != nil || raw == nil {
		t.Fatalf("read thread: raw=%v err=%v", raw != nil, err)
	}
	th, err := parseThread(raw)
	if err != nil {
		t.Fatalf("parseThread: %v", err)
	}
	if again := encodeThread(th); string(again) != string(raw) {
		t.Fatalf("thread without extras re-encoded to\n%s\nwant\n%s", again, raw)
	}
}

// TestMirrorDecodeRejectsMalformed keeps the error path honest: the custom
// decoders fail like encoding/json did, so parseThread still maps a broken
// document to ErrCorrupt instead of serving a zero-valued header. The cases are
// WELL-FORMED JSON with a wrong value type on purpose — encoding/json validates
// syntax before it calls UnmarshalJSON, so a truncated document never reaches
// the method and would prove nothing about it.
func TestMirrorDecodeRejectsMalformed(t *testing.T) {
	var (
		card    Card
		ix      Index
		th      Thread
		counter Counter
	)
	for _, tc := range []struct {
		name string
		data string
		into any
	}{
		{"card", `{"num":"one"}`, &card},
		{"index", `{"version":"two"}`, &ix},
		{"thread", `{"num":"three"}`, &th},
		{"counter", `{"next":"four"}`, &counter},
	} {
		if err := json.Unmarshal([]byte(tc.data), tc.into); err == nil {
			t.Fatalf("%s UnmarshalJSON accepted a wrong-typed field: %s", tc.name, tc.data)
		}
	}
	if _, err := parseThread([]byte(`{"num":"one"}`)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("parseThread(bad shape) = %v, want ErrCorrupt", err)
	}
}

// TestThreadMarshalRejectsInvalidReviewSummary keeps the encode error path
// honest: 04's opaque cache is a RawMessage, so an invalid one must surface as
// an error rather than being silently rewritten.
func TestThreadMarshalRejectsInvalidReviewSummary(t *testing.T) {
	th := Thread{
		Num: 1, Kind: "pr", Labels: []string{}, Assignees: []string{},
		Participants: []string{}, ReviewSummary: json.RawMessage(`{bad`),
	}
	if _, err := json.Marshal(th); err == nil {
		t.Fatal("Thread.MarshalJSON accepted an invalid review_summary")
	}
}
