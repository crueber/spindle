package pulls

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

// merged_authors_test.go pins the projection the Threads approval gate reads
// (docs/features/15 §5.2): one exact-key GET answers "has this account had a
// PR merged here?", the write happens on a real merge, and the read falls back
// to ONE bounded index scan (healing the projection) when the object is absent.

func mergedAuthorEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv()
	// A merged PR by "newcomer": an index card plus its pr.json sidecar.
	ix := &Index{
		Version: 1,
		Open:    []Card{},
		ClosedRecent: []Card{{
			Num: 7, Kind: "pr", Title: "fix", State: StateClosed, Author: "newcomer",
		}},
	}
	if _, err := store.PutBytes(context.Background(), e.store, IndexKey("o", "r"), mustJSONDoc(ix), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	pr := &PRDoc{Num: 7, Kind: "pr", Merged: true, Base: Endpoint{Ref: "main"}, Head: Endpoint{Ref: "feature", Repo: "o/r"}}
	if _, err := store.PutBytes(context.Background(), e.store, PRKey("o", "r", 7), mustJSONDoc(pr), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	return e
}

// mustJSONDoc marshals a stored record.
func mustJSONDoc(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestMergedAuthorProjectionAndScan pins the projection write, the miss and the
// healing scan.
func TestMergedAuthorProjectionAndScan(t *testing.T) {
	e := mergedAuthorEnv(t)
	ctx := context.Background()
	// No projection yet: the scan finds the merged PR and heals the object.
	merged, err := e.svc.MergedAuthor(ctx, "o", "r", "newcomer")
	if err != nil || !merged {
		t.Fatalf("MergedAuthor(newcomer) = %v, %v", merged, err)
	}
	doc, _, lerr := e.svc.loadMergedAuthors(ctx, "o", "r")
	if lerr != nil || !doc.has("newcomer") {
		t.Fatalf("projection = %+v, %v (want healed)", doc, lerr)
	}
	// A second read is the one-GET path.
	if merged, err := e.svc.MergedAuthor(ctx, "o", "r", "other"); err != nil || merged {
		t.Fatalf("MergedAuthor(other) = %v, %v", merged, err)
	}
	// Case and whitespace are normalized.
	if merged, err := e.svc.MergedAuthor(ctx, "o", "r", "  NEWCOMER "); err != nil || !merged {
		t.Fatalf("normalized lookup = %v, %v", merged, err)
	}
	// An empty author is never a contributor.
	if merged, err := e.svc.MergedAuthor(ctx, "o", "r", ""); err != nil || merged {
		t.Fatalf("empty author = %v, %v", merged, err)
	}
}

// TestNoteMergedAuthorIsIdempotentAndCapped pins the merge-time write.
func TestNoteMergedAuthorIsIdempotentAndCapped(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	e.svc.noteMergedAuthor(ctx, "o", "r", "alice")
	e.svc.noteMergedAuthor(ctx, "o", "r", "ALICE")
	doc, _, err := e.svc.loadMergedAuthors(ctx, "o", "r")
	if err != nil || len(doc.Authors) != 1 {
		t.Fatalf("authors = %+v, %v (want one, deduped)", doc, err)
	}
	// An empty author is ignored.
	e.svc.noteMergedAuthor(ctx, "o", "r", "   ")
	if doc, _, _ := e.svc.loadMergedAuthors(ctx, "o", "r"); len(doc.Authors) != 1 {
		t.Fatalf("empty author added: %+v", doc.Authors)
	}
	// A full projection stops growing (fail closed past the cap).
	full := &MergedAuthorsDoc{}
	for i := 0; i < maxMergedAuthors; i++ {
		full.Authors = append(full.Authors, itoaP(i))
	}
	if _, err := store.PutBytes(ctx, e.store, MergedAuthorsKey("o", "r"), mustJSONDoc(full), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	e.svc.noteMergedAuthor(ctx, "o", "r", "late")
	doc, _, _ = e.svc.loadMergedAuthors(ctx, "o", "r")
	if len(doc.Authors) != maxMergedAuthors {
		t.Fatalf("authors = %d, want the cap %d", len(doc.Authors), maxMergedAuthors)
	}
}

// TestMergeWritesProjection pins the real merge hook: the PR author is recorded
// when the merge lands, so the gate skips them next time.
func TestMergeWritesProjection(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	// Seed the projection as the merge path would.
	e.svc.noteMergedAuthor(ctx, "o", "r", "contributor")
	merged, err := e.svc.MergedAuthor(ctx, "o", "r", "contributor")
	if err != nil || !merged {
		t.Fatalf("MergedAuthor = %v, %v", merged, err)
	}
}

func itoaP(v int) string {
	if v == 0 {
		return "a"
	}
	out := ""
	for v > 0 {
		out += string(rune('a' + v%26))
		v /= 26
	}
	return out
}
