// mirror_roundtrip_test.go — the counter and thread-header mirrors are
// read-modify-written by this package on every ordinary write: CreateIssue and
// CreateMilestone rewrite meta/next_num and meta/milestones/index.json, and
// appendEvent (comment, reference, reaction, state change) rewrites
// issues/<num>/thread.json. A whitelist struct drops the NEXT additive field on
// the first such write, so the fix is structural — capture every unnamed key on
// decode and re-emit it on encode — not a list of known fields (the P6 index
// lesson; law 5: additive JSON fields round-trip unchanged).
//
// These tests drive the REAL write paths (CreateIssue, CreateMilestone,
// AddComment → appendEvent) and assert on the stored bytes, never on the Go
// structs.
package issues

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

// mirrorExtraShapes is the key-value matrix every mirror must survive: a nested
// object, an array, a string, a number, a bool and JSON null (a null value is
// still a present key and must not be dropped). Values are written compact so
// the seeded bytes and the re-read bytes are directly comparable.
var mirrorExtraShapes = []struct {
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

// seedCounter writes a {"next": N} allocator object with the given unknown keys,
// exactly as a future writer would before an allocation rewrites it.
func seedCounter(t *testing.T, s *Service, key string, next int, extras map[string]string) {
	t.Helper()
	doc := map[string]json.RawMessage{"next": json.RawMessage(strconv.Itoa(next))}
	for k, v := range extras {
		doc[k] = json.RawMessage(v)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode seeded counter %s: %v", key, err)
	}
	if _, err := store.PutBytes(reqCtx(), s.Store, key, raw,
		store.PutOptions{ContentType: "application/json"}); err != nil {
		t.Fatalf("seed counter %s: %v", key, err)
	}
}

// seedThreadExtras adds unknown top-level keys to an existing thread header,
// exactly as a future 03/04 or 02 writer would.
func seedThreadExtras(t *testing.T, s *Service, owner, repo string, num int, extras map[string]string) {
	t.Helper()
	key := ThreadKey(owner, repo, num)
	raw, ver, err := s.getJSON(reqCtx(), key)
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
	if _, err := store.PutBytes(reqCtx(), s.Store, key, out,
		store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
}

// readStoredObject re-reads one stored JSON object as raw keys.
func readStoredObject(t *testing.T, s *Service, key string) map[string]json.RawMessage {
	t.Helper()
	raw, _, err := store.GetBytes(reqCtx(), s.Store, key, store.GetOptions{})
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode %s: %v\n%s", key, err, raw)
	}
	return doc
}

// TestIssueCreateKeepsCounterExtras is the meta/next_num half: an unknown key
// on the shared counter must survive CreateIssue's allocation, and the known
// field must still increment.
func TestIssueCreateKeepsCounterExtras(t *testing.T) {
	for _, shape := range mirrorExtraShapes {
		t.Run(shape.key, func(t *testing.T) {
			s := testService(newFakeRoles())
			seedCounter(t, s, CounterKey("o", "r"), 5, map[string]string{shape.key: shape.val})

			th := mustCreate(t, s, "o", "r", writerP, "first", "")
			if th.Num != 5 {
				t.Fatalf("issue num = %d, want 5 (the seeded counter)", th.Num)
			}

			doc := readStoredObject(t, s, CounterKey("o", "r"))
			if got := string(doc["next"]); got != "6" {
				t.Fatalf("counter next after allocation = %s, want 6", got)
			}
			if got := string(doc[shape.key]); got != shape.val {
				t.Fatalf("unknown counter key %s after CreateIssue = %s, want %s", shape.key, got, shape.val)
			}
			if _, leaked := doc["unknown"]; leaked {
				t.Fatalf("marshal leaked the bookkeeping key: %v", doc)
			}
		})
	}
}

// TestMilestoneAllocKeepsCounterExtras is the milestone allocator half: the
// second {"next": N} read-modify-write has the same guarantee.
func TestMilestoneAllocKeepsCounterExtras(t *testing.T) {
	for _, shape := range mirrorExtraShapes {
		t.Run(shape.key, func(t *testing.T) {
			roles := newFakeRoles()
			grantTriage(roles, "o", "r")
			s := testService(roles)
			key := MilestoneCounterKey("o", "r")
			seedCounter(t, s, key, 5, map[string]string{shape.key: shape.val})

			m, err := s.CreateMilestone(reqCtx(), "o", "r", aliceP, "v1", "", nil)
			if err != nil {
				t.Fatalf("CreateMilestone: %v", err)
			}
			if m.ID != "000005" {
				t.Fatalf("milestone id = %q, want 000005 (the seeded counter)", m.ID)
			}

			doc := readStoredObject(t, s, key)
			if got := string(doc["next"]); got != "6" {
				t.Fatalf("milestone counter next after allocation = %s, want 6", got)
			}
			if got := string(doc[shape.key]); got != shape.val {
				t.Fatalf("unknown milestone counter key %s after CreateMilestone = %s, want %s", shape.key, got, shape.val)
			}
			if _, leaked := doc["unknown"]; leaked {
				t.Fatalf("marshal leaked the bookkeeping key: %v", doc)
			}
		})
	}
}

// TestCounterExtrasSurviveRepeatedAllocations proves the capture is not a
// one-shot: every subsequent allocation decodes and re-emits the same extras.
func TestCounterExtrasSurviveRepeatedAllocations(t *testing.T) {
	s := testService(newFakeRoles())
	seedCounter(t, s, CounterKey("o", "r"), 5, map[string]string{"future_object": `{"a":1}`})

	for want := 5; want <= 7; want++ {
		th := mustCreate(t, s, "o", "r", writerP, "next", "")
		if th.Num != want {
			t.Fatalf("issue num = %d, want %d", th.Num, want)
		}
	}
	doc := readStoredObject(t, s, CounterKey("o", "r"))
	if got := string(doc["next"]); got != "8" {
		t.Fatalf("counter next after three allocations = %s, want 8", got)
	}
	if got := string(doc["future_object"]); got != `{"a":1}` {
		t.Fatalf("unknown counter key after three allocations = %s, want {\"a\":1}", got)
	}
}

// TestThreadWriterKeepsUnknownFields is the thread half of the guarantee: an
// unknown header key survives appendEvent (the one writer of a live header),
// and the header write still does its own job.
func TestThreadWriterKeepsUnknownFields(t *testing.T) {
	for _, shape := range mirrorExtraShapes {
		t.Run(shape.key, func(t *testing.T) {
			s := testService(newFakeRoles())
			th := mustCreate(t, s, "o", "r", writerP, "first", "")
			seedThreadExtras(t, s, "o", "r", th.Num, map[string]string{shape.key: shape.val})

			if _, err := s.AddComment(reqCtx(), "o", "r", th.Num, writerP, "hello"); err != nil {
				t.Fatalf("AddComment: %v", err)
			}

			doc := readStoredObject(t, s, ThreadKey("o", "r", th.Num))
			if got := string(doc[shape.key]); got != shape.val {
				t.Fatalf("unknown thread key %s after appendEvent = %s, want %s", shape.key, got, shape.val)
			}
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

// TestThreadWriterKeepsReviewOwnedFields names the live defect this fix closes:
// 04 keeps next_review_seq, next_thread_num and review_summary on the shared PR
// header, and an issue event that lands on a PR thread (a #N reference) used to
// re-marshal the header from 02's whitelist and delete them.
func TestThreadWriterKeepsReviewOwnedFields(t *testing.T) {
	s := testService(newFakeRoles())
	th := mustCreate(t, s, "o", "r", writerP, "first", "")
	src := mustCreate(t, s, "o", "r", writerP, "second", "")
	review := map[string]string{
		"next_review_seq": `3`,
		"next_thread_num": `2`,
		"review_summary":  `{"latest":{"alice@example.com":{"seq":1,"state":"approved"}},"requested":["bob@example.com"],"threads_total":1,"threads_unresolved":0}`,
	}
	seedThreadExtras(t, s, "o", "r", th.Num, review)

	// A comment that references the PR thread lands a durable event on it
	// through appendEvent — the exact path that rewrote the header.
	if _, err := s.AddComment(reqCtx(), "o", "r", src.Num, writerP, "see #1"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}

	doc := readStoredObject(t, s, ThreadKey("o", "r", th.Num))
	for k, want := range review {
		if got := string(doc[k]); got != want {
			t.Fatalf("review-owned header key %s after appendEvent = %s, want %s", k, got, want)
		}
	}
	if got := string(doc["next_event_seq"]); got != "2" {
		t.Fatalf("next_event_seq after the reference event = %s, want 2", got)
	}
}

// TestMirrorNoExtrasMarshalIsUnchanged pins the byte shape for documents without
// extras: the custom marshalers must not reorder known fields, add a bookkeeping
// key, or serialize a nil extras map.
func TestMirrorNoExtrasMarshalIsUnchanged(t *testing.T) {
	got, err := json.Marshal(Counter{Next: 3})
	if err != nil {
		t.Fatalf("marshal counter: %v", err)
	}
	if string(got) != `{"next":3}` {
		t.Fatalf("counter without extras marshaled to %s, want {\"next\":3}", got)
	}

	s := testService(newFakeRoles())
	mustCreate(t, s, "o", "r", writerP, "first", "")

	raw, _, err := s.getJSON(reqCtx(), CounterKey("o", "r"))
	if err != nil || raw == nil {
		t.Fatalf("read counter: raw=%v err=%v", raw != nil, err)
	}
	if string(raw) != `{"next":2}` {
		t.Fatalf("allocated counter = %s, want {\"next\":2}", raw)
	}

	traw, _, err := s.getJSON(reqCtx(), ThreadKey("o", "r", 1))
	if err != nil || traw == nil {
		t.Fatalf("read thread: raw=%v err=%v", traw != nil, err)
	}
	th, err := parseThread(traw)
	if err != nil {
		t.Fatalf("parseThread: %v", err)
	}
	if again := encodeThread(th); string(again) != string(traw) {
		t.Fatalf("thread without extras re-encoded to\n%s\nwant\n%s", again, traw)
	}
}

// TestMirrorDecodeRejectsMalformed keeps the error paths honest: the custom
// decoders fail like encoding/json did, so parseThread still maps a broken
// document to ErrCorrupt instead of serving a zero-valued header. The cases are
// WELL-FORMED JSON with a wrong value type on purpose — encoding/json validates
// syntax before it calls UnmarshalJSON, so a truncated document never reaches
// the method and would prove nothing about it.
func TestMirrorDecodeRejectsMalformed(t *testing.T) {
	var (
		th      Thread
		counter Counter
	)
	for _, tc := range []struct {
		name string
		data string
		into any
	}{
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
