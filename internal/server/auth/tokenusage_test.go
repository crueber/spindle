package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
)

func TestTokenUsageDocRoundTrip(t *testing.T) {
	d := NewTokenUsageDoc()
	if d.Version != 1 || d.Tokens == nil {
		t.Fatalf("new doc = %+v", d)
	}
	d.Set("ci-bot", TokenUsageEntry{Name: "ci-bot", Principal: "ci-bot",
		LastUsed: "2026-11-23T10:00:00Z", Source: "203.0.113.7", UserAgent: "git/2.55.0"})
	d.Set("ci-bot", TokenUsageEntry{Name: "ci-bot", Principal: "ci-bot",
		LastUsed: "2026-11-23T10:01:00Z", Source: "203.0.113.8", UserAgent: "git/2.56.0"})
	e, ok := d.Entry("ci-bot")
	if !ok {
		t.Fatal("entry missing")
	}
	if e.LastUsed != "2026-11-23T10:01:00Z" || e.Count != 2 || e.Source != "203.0.113.8" {
		t.Fatalf("entry = %+v, want the newer observation counted twice", e)
	}
	// A late (older) observation never rewinds LastUsed.
	d.Set("ci-bot", TokenUsageEntry{Principal: "ci-bot", LastUsed: "2026-11-23T09:00:00Z"})
	if e, _ := d.Entry("ci-bot"); e.LastUsed != "2026-11-23T10:01:00Z" {
		t.Fatalf("LastUsed rewound to %q", e.LastUsed)
	}
	raw := d.Encode()
	got, err := DecodeTokenUsage(raw)
	if err != nil {
		t.Fatal(err)
	}
	e2, ok := got.Entry("ci-bot")
	if !ok || e2.LastUsed != "2026-11-23T10:01:00Z" || e2.Principal != "ci-bot" || e2.Count != 3 {
		t.Fatalf("decoded entry = %+v", e2)
	}
	if _, ok := got.Entry("absent"); ok {
		t.Fatal("absent key resolved")
	}
	if _, ok := (&TokenUsageDoc{}).Entry("x"); ok {
		t.Fatal("nil map resolved an entry")
	}
	var nilDoc *TokenUsageDoc
	if _, ok := nilDoc.Entry("x"); ok {
		t.Fatal("nil doc resolved an entry")
	}
}

func TestDecodeTokenUsageRejectsUnknownVersion(t *testing.T) {
	for _, raw := range []string{
		`{"version":9,"tokens":{}}`,
		`not json`,
	} {
		if _, err := DecodeTokenUsage([]byte(raw)); err == nil {
			t.Fatalf("%q decoded", raw)
		}
	}
	// Version 0 is an older/absent-version spelling: accepted as v1.
	d, err := DecodeTokenUsage([]byte(`{"tokens":{}}`))
	if err != nil || d.Version != 1 {
		t.Fatalf("v0 doc = %+v, %v", d, err)
	}
	// Null tokens map is normalized.
	d, err = DecodeTokenUsage([]byte(`{"version":1}`))
	if err != nil || d.Tokens == nil {
		t.Fatalf("nil map not normalized: %+v, %v", d, err)
	}
}

func TestLoadTokenUsage(t *testing.T) {
	ctx := context.Background()
	if d, err := LoadTokenUsage(ctx, nil); d != nil || err != nil {
		t.Fatalf("nil store = %+v, %v", d, err)
	}
	st := store.NewMemory()
	if d, err := LoadTokenUsage(ctx, st); d != nil || err != nil {
		t.Fatalf("absent object = %+v, %v", d, err)
	}
	doc := NewTokenUsageDoc()
	doc.Set("ci", TokenUsageEntry{Principal: "ci", LastUsed: "2026-11-23T10:00:00Z"})
	raw := doc.Encode()
	if _, err := store.PutBytes(ctx, st, TokenUsageKey, raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadTokenUsage(ctx, st)
	if err != nil || got == nil {
		t.Fatalf("load = %+v, %v", got, err)
	}
	if e, ok := got.Entry("ci"); !ok || e.Principal != "ci" {
		t.Fatalf("entry = %+v", e)
	}
}

func TestTokenUsageDocZeroValue(t *testing.T) {
	var d TokenUsageDoc
	d.Set("ci", TokenUsageEntry{Principal: "ci", LastUsed: "2026-11-23T10:00:00Z"})
	if _, ok := d.Entry("ci"); !ok {
		t.Fatal("zero-value doc did not accept an entry")
	}
	if d.Version != 0 {
		t.Fatalf("version bumped by Set: %d", d.Version)
	}
	raw := d.Encode()
	if d.Version != 1 {
		t.Fatalf("Encode did not stamp the version: %d", d.Version)
	}
	if !strings.Contains(string(raw), `"version":1`) {
		t.Fatalf("encoded doc = %s", raw)
	}
}

// failingStore fails every Get with a retryable error (not NotFound).
type failingStore struct{ store.ObjectStore }

func (f failingStore) Get(context.Context, string, store.GetOptions) (store.GetResult, error) {
	return nil, store.NewRetryable("auth/token-usage.json", errors.New("read down"))
}

func TestLoadTokenUsageSurfacesReadFailure(t *testing.T) {
	_, err := LoadTokenUsage(context.Background(), failingStore{ObjectStore: store.NewMemory()})
	if err == nil {
		t.Fatal("read failure not surfaced")
	}
}

func TestTokenUsageIdentity(t *testing.T) {
	if got := TokenUsageIdentity("  ci-bot ", "secret"); got != "ci-bot" {
		t.Fatalf("named identity = %q", got)
	}
	if got := TokenUsageIdentity("", ""); got != "" {
		t.Fatalf("empty identity = %q", got)
	}
	got := TokenUsageIdentity("", "secret")
	if !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+12 {
		t.Fatalf("hash identity = %q", got)
	}
	if got != TokenUsageIdentity(" ", "secret") {
		t.Fatal("whitespace-only name must behave like unnamed")
	}
	if strings.Contains(got, "secret") {
		t.Fatalf("identity leaks the secret: %q", got)
	}
	if a, b := TokenHashPrefix("a"), TokenHashPrefix("b"); a == b {
		t.Fatal("distinct secrets share a prefix")
	}
	if got := NowRFC3339(time.Date(2026, 11, 23, 10, 0, 0, 0, time.FixedZone("x", 3600))); got != "2026-11-23T09:00:00Z" {
		t.Fatalf("NowRFC3339 = %q", got)
	}
}
