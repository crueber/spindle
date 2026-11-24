package review

import (
	"strings"
	"testing"
)

// TestDocExamplesVerbatim executes the request bodies docs/features/04 shows,
// as a client would copy them: §4's normative anchor plus §7's three write
// shapes (open a thread, reply, submit a review with attached threads). The
// docs are the public contract, so a shape change here must be a doc change
// in the same commit (BUG-8).
func TestDocExamplesVerbatim(t *testing.T) {
	h, _ := testHandler(t)
	base := "/o/r/api/pulls/7"

	// docs/features/04 §4 — the anchor spec, every field present.
	anchor := `{"path":"src/main.go","side":"NEW","old_start":0,"old_lines":0,` +
		`"new_start":120,"new_lines":3,` +
		`"commit_sha":"` + testHead + `",` +
		`"context_sha":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`

	// §7: POST …/threads takes {anchor, body}.
	w := doReq(h, "POST", base+"/threads", "carol", `{"anchor":`+anchor+`,"body":"nit"}`)
	if w.Code != 201 {
		t.Fatalf("documented open-thread body: %d %s", w.Code, w.Body.String())
	}
	th := mustJSON(t, w)["thread"].(map[string]any)
	tid := th["tid"].(string)
	if th["anchor"].(map[string]any)["path"] != "src/main.go" {
		t.Fatalf("thread = %v", th)
	}

	// §7: POST …/threads/{tid}/comments takes {body}.
	w = doReq(h, "POST", base+"/threads/"+tid+"/comments", "bob", `{"body":"ack"}`)
	if w.Code != 201 {
		t.Fatalf("documented reply body: %d %s", w.Code, w.Body.String())
	}
	if mustJSON(t, w)["comment"].(map[string]any)["body"] != "ack" {
		t.Fatalf("comment = %s", w.Body.String())
	}

	// §7: POST …/reviews takes {state, body?, commit_sha, threads?: [{anchor,
	// body}]} — state in the wire spellings §3/§8 name.
	reviewBody := `{"state":"APPROVED","body":"lgtm","commit_sha":"` + testHead + `",` +
		`"threads":[{"anchor":` + anchor + `,"body":"nit"}]}`
	w = doReq(h, "POST", base+"/reviews", "carol", reviewBody)
	if w.Code != 201 {
		t.Fatalf("documented review body: %d %s", w.Code, w.Body.String())
	}
	if got := len(mustJSON(t, w)["threads"].([]any)); got != 1 {
		t.Fatalf("review threads = %d, want 1", got)
	}

	// The SPA-era flat thread entry ({path, side, line, body}) is not a
	// documented shape — no alias is accepted — but the one 400 names every
	// unknown key AND the accepted set, so the client never climbs a
	// key-per-retry ladder.
	w = doReq(h, "POST", base+"/threads", "carol", `{"path":"src/main.go","side":"NEW","line":120,"body":"nit"}`)
	if w.Code != 400 {
		t.Fatalf("flat thread entry = %d, want 400", w.Code)
	}
	for _, want := range []string{`"line"`, `"path"`, `"side"`, "accepted: anchor, body"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("flat-entry rejection missing %q: %s", want, w.Body.String())
		}
	}

	// A half-anchor reports every missing piece at once (commit_sha and
	// context_sha), not one per round trip.
	w = doReq(h, "POST", base+"/threads", "carol",
		`{"anchor":{"path":"src/main.go","side":"NEW","new_start":1,"new_lines":1},"body":"nit"}`)
	if w.Code != 400 {
		t.Fatalf("half anchor = %d, want 400", w.Code)
	}
	for _, want := range []string{"commit_sha", "context_sha"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("half-anchor rejection does not name %q: %s", want, w.Body.String())
		}
	}
}
