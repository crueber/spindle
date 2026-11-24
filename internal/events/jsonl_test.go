// jsonl_test.go — the `jsonl` sink (14 §14.6 as built): NDJSON frames for the
// same events the webhook sink arrays, size-capped rotation, and the registry
// that pins the documented sink kinds against the doc.
package events

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSuffix(string(raw), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func TestJSONLSink_WritesNDJSONFrames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit", "events.jsonl")
	sink := &JSONLSink{Path: path}
	batch := twoEvents()
	if err := sink.Deliver(context.Background(), "o/r", batch); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	for i, line := range lines {
		var got RefEvent
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d is not one JSON object: %v", i, err)
		}
		if got.RefName != batch[i].RefName || got.Walgit.Seq != batch[i].Walgit.Seq {
			t.Fatalf("line %d = %+v, want %+v", i, got, batch[i])
		}
	}
	// Append, not replace: a second batch continues the same file.
	if err := sink.Deliver(context.Background(), "o/r", batch[:1]); err != nil {
		t.Fatalf("Deliver 2: %v", err)
	}
	if got := len(readLines(t, path)); got != 3 {
		t.Fatalf("lines after second batch = %d, want 3", got)
	}
}

func TestJSONLSink_EmptyBatchNoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	sink := &JSONLSink{Path: path}
	if err := sink.Deliver(context.Background(), "o/r", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("empty batch created a file: %v", err)
	}
}

func TestJSONLSink_RotatesBySizeAndKeeps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	// Each frame is ~180 bytes; a 300-byte cap forces one rotation per batch.
	sink := &JSONLSink{Path: path, MaxBytes: 300, Keep: 2}
	for i := 0; i < 4; i++ {
		if err := sink.Deliver(context.Background(), "o/r", twoEvents()); err != nil {
			t.Fatalf("Deliver %d: %v", i, err)
		}
	}
	// keep=2 → live file + .1 + .2, and .3 was dropped.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("live file: %v", err)
	}
	for _, name := range []string{path + ".1", path + ".2"} {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("rotated file %s: %v", name, err)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("rotation kept more than Keep files: %v", err)
	}
}

func TestJSONLSink_OversizedBatchStillWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	sink := &JSONLSink{Path: path, MaxBytes: 10, Keep: 1}
	if err := sink.Deliver(context.Background(), "o/r", twoEvents()); err != nil {
		t.Fatalf("oversized batch must not be dropped: %v", err)
	}
	if got := len(readLines(t, path)); got != 2 {
		t.Fatalf("lines = %d, want 2", got)
	}
	// The next batch rotates (the live file was already over the cap).
	if err := sink.Deliver(context.Background(), "o/r", twoEvents()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected a rotation: %v", err)
	}
}

func TestJSONLSink_CanceledContextAndBadPath(t *testing.T) {
	sink := &JSONLSink{Path: filepath.Join(t.TempDir(), "events.jsonl")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sink.Deliver(ctx, "o/r", twoEvents()); err == nil {
		t.Fatal("canceled context must fail the delivery")
	}
	// A directory in place of the file is a delivery error (fail closed).
	dir := t.TempDir()
	if err := (&JSONLSink{Path: dir}).Deliver(context.Background(), "o/r", twoEvents()); err == nil {
		t.Fatal("directory path must fail")
	}
	if err := (&JSONLSink{}).Deliver(context.Background(), "o/r", twoEvents()); err == nil {
		t.Fatal("empty path must fail")
	}
}

func TestJSONLSink_Name(t *testing.T) {
	if got := (&JSONLSink{}).Name(); got != KindJSONL {
		t.Fatalf("default name = %q", got)
	}
	if got := (&JSONLSink{Label: "audit"}).Name(); got != "audit" {
		t.Fatalf("label name = %q", got)
	}
	if got := (*JSONLSink)(nil).Name(); got != KindJSONL {
		t.Fatalf("nil name = %q", got)
	}
}

func TestSinkRegistry_ConstructsEveryBuiltin(t *testing.T) {
	if got := SinkKinds(); len(got) != 2 || got[0] != KindWebhook || got[1] != KindJSONL {
		t.Fatalf("SinkKinds() = %v", got)
	}
	wh, err := NewSink(SinkSpec{Kind: KindWebhook, URL: "https://example.com/hook", Secret: "s", Name: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	if wh.Name() != "ci" {
		t.Fatalf("webhook label = %q", wh.Name())
	}
	jl, err := NewSink(SinkSpec{Kind: KindJSONL, Path: filepath.Join(t.TempDir(), "a.jsonl"), MaxBytes: 1024, Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	if jl.Name() != KindJSONL {
		t.Fatalf("jsonl name = %q", jl.Name())
	}
	if _, err := NewSink(SinkSpec{Kind: "carrier-pigeon"}); err == nil {
		t.Fatal("unknown kind must fail")
	}
	if _, err := NewSink(SinkSpec{Kind: KindWebhook}); err == nil {
		t.Fatal("webhook without url must fail")
	}
	if _, err := NewSink(SinkSpec{Kind: KindJSONL}); err == nil {
		t.Fatal("jsonl without path must fail")
	}
}

// TestDocumentedSinkKindsAreConstructible pins the doc and the registry
// together: every `kind = "..."` in the §14.6 config example must be a kind
// NewSink builds, and every built kind must appear in the doc.
func TestDocumentedSinkKindsAreConstructible(t *testing.T) {
	raw, err := os.ReadFile("../../docs/go/14_extensibility.md")
	if err != nil {
		t.Fatalf("read the sink doc: %v", err)
	}
	// Scope to §14.6 (seam 4, event sinks): "kind =" elsewhere in the doc
	// names other registries (task kinds, storage backends) and is not a sink.
	section := string(raw)
	if i := strings.Index(section, "## 14.6"); i >= 0 {
		section = section[i:]
		if j := strings.Index(section, "## 14.7"); j >= 0 {
			section = section[:j]
		}
	}
	re := regexp.MustCompile(`kind\s*=\s*"([a-z0-9_-]+)"`)
	documented := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(section, -1) {
		documented[m[1]] = true
	}
	if len(documented) == 0 {
		t.Fatal("doc pins no sink kinds")
	}
	for kind := range documented {
		if _, err := NewSink(SinkSpec{Kind: kind, URL: "https://example.com/h", Path: filepath.Join(t.TempDir(), "x.jsonl")}); err != nil {
			t.Errorf("documented kind %q is not constructible: %v", kind, err)
		}
	}
	for _, kind := range SinkKinds() {
		if !documented[kind] {
			t.Errorf("built-in kind %q is not documented in 14_extensibility.md", kind)
		}
	}
}
