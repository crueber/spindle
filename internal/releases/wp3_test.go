package releases

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// doByte fires the asset byte route through the repo-subpath seam.
func doByte(x *harness, method, tag, name string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/o/r/releases/"+tag+"/assets/"+name, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	x.handler.HandleRepo(rec, req, git.RepoId{Owner: "o", Name: "r"}, []string{"releases", tag, "assets", name})
	return rec
}

func seedReleaseWithAsset(t *testing.T, x *harness) {
	t.Helper()
	x.git.tags["v1"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mustPut(t, x, writer(), "v1", ReleaseInput{Draft: boolptr(false)})
	body := []byte("asset bytes")
	if _, err := x.svc.UploadAsset(ctx(), "o", "r", writer(), "v1", "tool.bin", strings.NewReader(string(body)), int64(len(body)), shaOf(body), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
}

func TestAssetDownloadCountIncrementsAndSurvivesRestart(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	seedReleaseWithAsset(t, x)
	for i := 0; i < 2; i++ {
		w := doByte(x, "GET", "v1", "tool.bin", asReader("jane"))
		if w.Code != 200 {
			t.Fatalf("download %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if got := x.svc.AssetDownloads(ctx(), "o", "r", "v1")["tool.bin"]; got != 2 {
		t.Fatalf("count %d, want 2", got)
	}
	// HEAD and a 304 are not downloads.
	if w := doByte(x, "HEAD", "v1", "tool.bin", asReader("jane")); w.Code != 200 {
		t.Fatalf("head %d", w.Code)
	}
	etag := doByte(x, "GET", "v1", "tool.bin", asReader("jane")).Header().Get("ETag")
	if w := doByte(x, "GET", "v1", "tool.bin", map[string]string{"If-None-Match": etag, "X-Test-Principal": "jane"}); w.Code != http.StatusNotModified {
		t.Fatalf("304: %d", w.Code)
	}
	if got := x.svc.AssetDownloads(ctx(), "o", "r", "v1")["tool.bin"]; got != 3 {
		// the etag-fetch above counted; HEAD/304 did not.
		t.Fatalf("count after head+304 %d, want 3", got)
	}
	// "Restart": a fresh Service over the same bucket reads the same count.
	fresh := New(x.svc.Store, x.roles)
	if got := fresh.AssetDownloads(ctx(), "o", "r", "v1")["tool.bin"]; got != 3 {
		t.Fatalf("count after restart %d, want 3", got)
	}
	// API shows it.
	w := do(t, x, "GET", "/o/r/api/releases/v1", nil, asReader("jane"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"downloads":3`) {
		t.Fatalf("api: %d %s", w.Code, w.Body.String())
	}
}

func TestDraftWithNewTagCreatesTagOnPublish(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["main"] = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	var created []string
	x.svc.CreateTag = func(_ context.Context, owner, repo, name, sha string, p auth.Principal) error {
		created = append(created, name+"@"+sha)
		return nil
	}
	target := "main"
	rel, isNew := mustPut(t, x, writer(), "v9.9.9", ReleaseInput{Draft: boolptr(true), Target: &target})
	if !isNew || !rel.PendingTag {
		t.Fatalf("draft: %+v", rel)
	}
	if len(created) != 0 {
		t.Fatalf("tag created before publish: %v", created)
	}
	if rel.TagSHA != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("tag sha %q", rel.TagSHA)
	}
	pub := false
	rel, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v9.9.9", ReleaseInput{Draft: &pub}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 || !strings.HasPrefix(created[0], "v9.9.9@") {
		t.Fatalf("created %v", created)
	}
	if rel.PendingTag || rel.Draft || rel.PublishedAt == nil {
		t.Fatalf("published: %+v", rel)
	}
	// Re-publishing is idempotent: no second create.
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v9.9.9", ReleaseInput{Draft: &pub}, ""); err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 {
		t.Fatalf("tag created twice: %v", created)
	}
}

func TestDraftTargetValidation(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["main"] = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	// A published release cannot declare a new tag.
	target := "main"
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v1", ReleaseInput{Target: &target}, ""); err == nil || !strings.Contains(err.Error(), "draft") {
		t.Fatalf("published target: %v", err)
	}
	// A draft with no target and no existing tag is the historical 404.
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v2", ReleaseInput{Draft: boolptr(true)}, ""); err == nil || !isErr(err, ErrNotFound) {
		t.Fatalf("missing target: %v", err)
	}
	// An unknown target is a 404.
	bad := "nope"
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v3", ReleaseInput{Draft: boolptr(true), Target: &bad}, ""); err == nil || !isErr(err, ErrNotFound) {
		t.Fatalf("bad target: %v", err)
	}
	// Target on an existing release is immutable-after-creation.
	x.git.tags["v4"] = "cccccccccccccccccccccccccccccccccccccccc"
	mustPut(t, x, writer(), "v4", ReleaseInput{Draft: boolptr(true)})
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v4", ReleaseInput{Target: &target}, ""); err == nil || !strings.Contains(err.Error(), "only valid") {
		t.Fatalf("target update: %v", err)
	}
}

func TestPendingTagPublishNeedsCreator(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["main"] = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	target := "main"
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v1", ReleaseInput{Draft: boolptr(true), Target: &target}, ""); err != nil {
		t.Fatal(err)
	}
	// No CreateTag seam wired: publishing refuses rather than silently
	// dropping the tag.
	x.svc.CreateTag = nil
	pub := false
	if _, _, err := x.svc.PutRelease(ctx(), "o", "r", writer(), "v1", ReleaseInput{Draft: &pub}, ""); err == nil || !isErr(err, ErrUnavailable) {
		t.Fatalf("unwired publish: %v", err)
	}
}

func TestDownloadCountsCorruptReadsZero(t *testing.T) {
	x := newHarness(t)
	if _, err := store.PutBytes(ctx(), x.svc.Store, DownloadCountsKey("o", "r", "v1"), []byte("{bad"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if got := x.svc.AssetDownloads(ctx(), "o", "r", "v1")["any"]; got != 0 {
		t.Fatalf("corrupt count %d", got)
	}
}
