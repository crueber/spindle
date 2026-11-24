package webui

// blame_test.go — the Blame page's httptest contract: the same read gate as
// the blob page, the binary/too-large refusals in identical language, block
// grouping with commit links, and the honest unwired/empty states.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/git"
)

func (v *repoFakeView) Blame(_ context.Context, _ git.RepoId, _, _ string) (api.BlameResult, error) {
	v.blameN++
	if v.blameErr != nil {
		return api.BlameResult{}, v.blameErr
	}
	return v.blame, nil
}

func blameFixture(t *testing.T) *repoFixture {
	t.Helper()
	f := newRepoFixture(t, true)
	f.view.blame = api.BlameResult{
		Size: 24,
		Lines: []api.BlameLine{
			{SHA: strings.Repeat("a", 40), Author: "Ada", AuthorTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), LineNo: 1, Text: "alpha"},
			{SHA: strings.Repeat("a", 40), Author: "Ada", AuthorTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), LineNo: 2, Text: "bravo"},
			{SHA: strings.Repeat("b", 40), Author: "Bob", AuthorTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), LineNo: 3, Text: "<tail>"},
		},
	}
	return f
}

func TestRepoBlameRendersBlocks(t *testing.T) {
	t.Parallel()
	f := blameFixture(t)
	body := f.mustGet("/demo/spindle/blame/main/a.txt", http.StatusOK)
	if n := strings.Count(body, `<a class="link mono" href="/demo/spindle/commit/`); n != 2 {
		t.Fatalf("commit links = %d, want 2 blocks\n%s", n, body)
	}
	if !strings.Contains(body, "Ada") || !strings.Contains(body, "Bob") {
		t.Fatal("authors missing")
	}
	if strings.Count(body, `<tr id="L`) != 3 {
		t.Fatal("per-line rows missing")
	}
	// Text is escaped, never trusted as markup.
	if !strings.Contains(body, "&lt;tail&gt;") || strings.Contains(body, "<tail>") {
		t.Fatal("line text was not escaped")
	}
	// The block rowspan covers both Ada lines.
	if !strings.Contains(body, `rowspan="2"`) {
		t.Fatal("blame block did not rowspan its lines")
	}
	// Tab strip stays on Code, and the blob-page link pair is present.
	if !strings.Contains(body, `aria-current="page">Blame`) || !strings.Contains(body, `/demo/spindle/blob/main/a.txt`) {
		t.Fatal("code/blame switch missing")
	}
}

func TestRepoBlameRefusals(t *testing.T) {
	t.Parallel()
	f := blameFixture(t)
	f.view.blame = api.BlameResult{Size: 3 << 20, TooLarge: true}
	body := f.mustGet("/demo/spindle/blame/main/big.dat", http.StatusOK)
	if !strings.Contains(body, "too large to render") || !strings.Contains(body, "2 MiB") {
		t.Fatal("too-large language changed")
	}
	if strings.Contains(body, "rowspan=") {
		t.Fatal("too-large page must not render lines")
	}

	f.view.blame = api.BlameResult{Size: 9, Binary: true}
	body = f.mustGet("/demo/spindle/blame/main/bin.dat", http.StatusOK)
	if !strings.Contains(body, "binary file, 9b") {
		t.Fatalf("binary language changed: %s", body)
	}

	f.view.blame = api.BlameResult{Size: 24, Truncated: true, Lines: []api.BlameLine{
		{SHA: strings.Repeat("a", 40), Author: "Ada", AuthorTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), LineNo: 1, Text: "x"},
	}}
	body = f.mustGet("/demo/spindle/blame/main/a.txt", http.StatusOK)
	if !strings.Contains(body, "Blame covers the first") {
		t.Fatal("truncation notice missing")
	}
}

func TestRepoBlameGates(t *testing.T) {
	t.Parallel()
	// Invisible repo: 404, never existence leakage.
	f := blameFixture(t)
	f.reg.exists = map[string]bool{}
	if code, _ := f.get("/demo/spindle/blame/main/a.txt"); code != http.StatusNotFound {
		t.Fatalf("invisible repo = %d", code)
	}

	// anonymous_read=false with no identity surface: 401 for an anonymous
	// viewer (the blob page's rule).
	g := newRepoFixture(t, false)
	g.view.blame = api.BlameResult{Lines: []api.BlameLine{}}
	if code, _ := g.get("/demo/spindle/blame/main/a.txt"); code != http.StatusUnauthorized {
		t.Fatalf("anonymous read off = %d", code)
	}

	// Missing path / no resolved file: 404.
	h := blameFixture(t)
	if code, _ := h.get("/demo/spindle/blame/main"); code != http.StatusNotFound {
		t.Fatalf("blameless path = %d", code)
	}
	h.view.resolution = api.Resolution{Ref: "refs/heads/main", SHA: strings.Repeat("a", 40)}
	if code, _ := h.get("/demo/spindle/blame/main"); code != http.StatusNotFound {
		t.Fatalf("directory blob = %d", code)
	}

	// Empty repository: the guided guide, no blame read.
	e := blameFixture(t)
	e.view.summary = api.SummaryData{Health: api.RepoHealthEmpty}
	if body := e.mustGet("/demo/spindle/blame/main/a.txt", http.StatusOK); !strings.Contains(body, "empty") {
		t.Fatal("empty guide missing")
	}
	if e.view.blameN != 0 {
		t.Fatal("empty repo must not read blame")
	}

	// Unwired browsing provider: an honest 503 (the page never fabricates).
	u := blameFixture(t)
	u.s.env.API.Repo = struct{ api.RepoView }{u.view}
	if code, _ := u.get("/demo/spindle/blame/main/a.txt"); code != http.StatusServiceUnavailable {
		t.Fatalf("unwired browse = %d", code)
	}

	// Blame read failure maps like every object read.
	r := blameFixture(t)
	r.view.blameErr = api.ErrNotFound
	if code, _ := r.get("/demo/spindle/blame/main/a.txt"); code != http.StatusNotFound {
		t.Fatalf("blame 404 = %d", code)
	}
	r.view.blameErr = errors.New("boom")
	if code, _ := r.get("/demo/spindle/blame/main/a.txt"); code != http.StatusServiceUnavailable {
		t.Fatalf("blame 503 = %d", code)
	}
}

func TestBuildBlameBlocks(t *testing.T) {
	t.Parallel()
	lines := []api.BlameLine{
		{SHA: strings.Repeat("a", 40), Author: "Ada", LineNo: 1, Text: "one"},
		{SHA: strings.Repeat("b", 40), Author: "Bob", LineNo: 2, Text: ""},
		{SHA: strings.Repeat("a", 40), Author: "Ada", LineNo: 3, Text: "three"},
	}
	blocks := buildBlameBlocks("demo/spindle", lines)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3 (same sha must not merge across a gap)", len(blocks))
	}
	if !blocks[1].Lines[0].Blank || blocks[1].Lines[0].Code != "" {
		t.Fatal("blank line shape")
	}
	if blocks[0].Href != "/demo/spindle/commit/"+lines[0].SHA || blocks[0].Short != shortSHA(lines[0].SHA) {
		t.Fatalf("block link = %+v", blocks[0])
	}
}

func TestRepoBlameEdgeBranches(t *testing.T) {
	t.Parallel()
	f := blameFixture(t)
	// Empty rest is a 404, not the repo root.
	if code, _ := f.get("/demo/spindle/blame/"); code != http.StatusNotFound {
		t.Fatalf("empty rest = %d", code)
	}
	// Empty-repo marker from Resolve (the api's guide signal) renders the guide.
	g := blameFixture(t)
	g.view.resolveErr = fmt.Errorf("%w: empty repository: unborn HEAD", api.ErrNotFound)
	body := g.mustGet("/demo/spindle/blame/main/a.txt", http.StatusOK)
	if !strings.Contains(body, "empty") || g.view.blameN != 0 {
		t.Fatal("unborn blame must guide")
	}
	// A resolution without a ref falls back to the addressed sha for links.
	h := blameFixture(t)
	h.view.resolution = api.Resolution{SHA: strings.Repeat("a", 40)}
	body = h.mustGet("/demo/spindle/blame/main/a.txt", http.StatusOK)
	if !strings.Contains(body, "/api/blob/"+strings.Repeat("a", 40)+"/a.txt?raw") {
		t.Fatal("sha-addressed blame has no raw link")
	}
	if baseNameOf("a.txt") != "a.txt" || baseNameOf("d/a.txt") != "a.txt" {
		t.Fatal("baseNameOf broken")
	}
}
