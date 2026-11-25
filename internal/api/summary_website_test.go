// summary_website_test.go — the repo website: the summary wire field, the
// ETag suffix for a settings-only change, and the walView fold from the
// manifest-inline settings TOML (the summary235_test.go precedent).
package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/store/proto"
	"github.com/crueber/spindle/internal/wal"
)

func TestSummaryWebsiteWire(t *testing.T) {
	f := newFixture(t)
	seedSummary(f)
	f.view.summaries["demo/walgit"] = SummaryData{
		Head:     &Ref{Name: "refs/heads/main", SHA: fakeSHA},
		Branches: 2,
		Tags:     1,
		Website:  "https://example.com/docs?ref=spindle",
	}
	w := f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Website string `json:"website"`
	}
	decodeJSON(t, w, &body)
	if body.Website != "https://example.com/docs?ref=spindle" {
		t.Fatalf("website = %q", body.Website)
	}
	// The ETag covers the website (same head sha, new field).
	etag := w.Header().Get("ETag")
	if !strings.Contains(etag, "~w") {
		t.Fatalf("etag = %q, want a ~w suffix", etag)
	}

	// Unset renders as "" (the field is always present) and the suffix drops,
	// which busts the cache too.
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 2, Tags: 1,
	}
	w = f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"website":""`) {
		t.Fatalf("unset website must serialize as empty string: %s", w.Body.String())
	}
	if etag := w.Header().Get("ETag"); strings.Contains(etag, "~w") {
		t.Fatalf("unset website etag = %q, must carry no ~w suffix", etag)
	}
}

func TestSummaryWebsite304(t *testing.T) {
	f := newFixture(t)
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
		Website: "https://example.com/v1",
	}
	w := f.req("GET", "/demo/walgit/api")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	current := w.Header().Get("ETag")

	// A website-only change must NOT 304 against the previous etag.
	f.view.summaries["demo/walgit"] = SummaryData{
		Head: &Ref{Name: "refs/heads/main", SHA: fakeSHA}, Branches: 1,
		Website: "https://example.com/v2",
	}
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": current}, readP())
	if w.Code != http.StatusOK {
		t.Fatalf("stale website etag must revalidate to 200, got %d", w.Code)
	}
	// The new etag 304s while the value is current.
	w = f.do("GET", "/demo/walgit/api", nil, map[string]string{"If-None-Match": w.Header().Get("ETag")}, readP())
	if w.Code != http.StatusNotModified {
		t.Fatalf("current website etag must 304, got %d", w.Code)
	}
}

// --- walView fold: manifest-inline settings TOML → SummaryData.Website ---

func TestWalSummaryWebsite(t *testing.T) {
	ctx := context.Background()
	id := git.RepoId{Owner: "demo", Name: "walgit"}
	fix := newGitFix(t)
	man := &proto.Manifest{HeadSeq: 3,
		Settings: &proto.RepoSettings{Toml: "description = \"docs\"\nwebsite = \"https://example.com/wal\"\n"}}
	eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 1, man: man}
	v := newEngineFixture(t, eng).view()
	s, err := v.Summary(ctx, id)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if s.Website != "https://example.com/wal" {
		t.Fatalf("website = %q", s.Website)
	}
	// No settings on the manifest → unset, never an error.
	eng.man = &proto.Manifest{HeadSeq: 3}
	s, err = v.Summary(ctx, id)
	if err != nil || s.Website != "" {
		t.Fatalf("no-settings summary = %+v, %v", s, err)
	}
	// Corrupt settings TOML fails open to "" (display metadata).
	eng.man = &proto.Manifest{HeadSeq: 3,
		Settings: &proto.RepoSettings{Toml: "[broken\n"}}
	s, err = v.Summary(ctx, id)
	if err != nil || s.Website != "" {
		t.Fatalf("corrupt-settings summary = %+v, %v", s, err)
	}
	// An invalid URL cannot be published, but a hand-written doc that carries
	// one still fails open rather than failing the summary.
	eng.man = &proto.Manifest{HeadSeq: 3,
		Settings: &proto.RepoSettings{Toml: "website = \"javascript:alert(1)\"\n"}}
	s, err = v.Summary(ctx, id)
	if err != nil || s.Website != "" {
		t.Fatalf("invalid-website summary = %+v, %v", s, err)
	}
	// Nil engine fails closed to "".
	if got := (&walView{}).repoWebsite(ctx, id); got != "" {
		t.Fatalf("nil engine = %q", got)
	}
}
