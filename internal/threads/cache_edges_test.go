package threads

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/actionscache"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// The cache and artifact v4 failure branches: every store error is a named
// error, every gate fails closed, and the HTTP surface refuses malformed input.

func TestCacheErrorMappingAndHelpers(t *testing.T) {
	cases := []struct {
		in   error
		want error
	}{
		{nil, nil},
		{actionscache.ErrNotFound, ErrNotFound},
		{actionscache.ErrInvalid, ErrInvalid},
		{actionscache.ErrTooLarge, ErrTooLarge},
		{actionscache.ErrForbidden, ErrForbidden},
		{actionscache.ErrCorrupt, ErrCorrupt},
		{io.EOF, io.EOF},
	}
	for _, tc := range cases {
		got := cacheErr(tc.in)
		if tc.in == nil {
			if got != nil {
				t.Fatalf("cacheErr(nil) = %v", got)
			}
			continue
		}
		if tc.want == io.EOF {
			if !errors.Is(got, io.EOF) {
				t.Fatalf("passthrough = %v", got)
			}
			continue
		}
		if !errors.Is(got, tc.want) {
			t.Fatalf("cacheErr(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if got := cacheViewOf(nil); got.Hit {
		t.Fatalf("nil hit = %+v", got)
	}
	if got := cacheViewOf(&actionscache.Hit{Entry: actionscache.Entry{Key: "k"}, Exact: true}); !got.Hit || !got.Exact || got.Key != "k" {
		t.Fatalf("hit view = %+v", got)
	}
	// prNumberOf tolerates every payload shape a run may carry.
	cases2 := []struct {
		run  *Run
		want int
	}{
		{nil, 0},
		{&Run{Event: EventPush}, 0},
		{&Run{Event: EventPullReq}, 0},
		{&Run{Event: EventPullReq, EventPayload: map[string]any{}}, 0},
		{&Run{Event: EventPullReq, EventPayload: map[string]any{"pull_request": "x"}}, 0},
		{&Run{Event: EventPullReq, EventPayload: map[string]any{"pull_request": map[string]any{"number": 4}}}, 4},
		{&Run{Event: EventPullReq, EventPayload: map[string]any{"pull_request": map[string]any{"number": int64(5)}}}, 5},
		{&Run{Event: EventPullReq, EventPayload: map[string]any{"pull_request": map[string]any{"number": float64(6)}}}, 6},
		{&Run{Event: EventPullReq, EventPayload: map[string]any{"pull_request": map[string]any{"number": "7"}}}, 0},
	}
	for i, tc := range cases2 {
		if got := prNumberOf(tc.run); got != tc.want {
			t.Fatalf("case %d: prNumberOf = %d, want %d", i, got, tc.want)
		}
	}
	// baseScopeOf edge: an empty branch has no base scope.
	if baseScopeOf("", "main") != nil {
		t.Fatal("an empty branch produced a base scope")
	}
	if got := baseScopeOf("refs/heads/main", "main"); got == nil || got.Kind != actionscache.KindDefault || got.Writable() {
		t.Fatalf("default base = %+v", got)
	}
	if got := baseScopeOf("feat", "main"); got == nil || got.Branch != "feat" || got.Writable() {
		t.Fatalf("branch base = %+v", got)
	}
	// A service with no git reader has no default-branch fallback.
	svc := &Service{}
	if got := svc.cacheDefaultBranch(context.Background(), "o", "r"); got != "" {
		t.Fatalf("no-git default branch = %q", got)
	}
}

// errDefaultGit is a reader whose DefaultBranch fails.
type errDefaultGit struct{ *fakeGit }

func (e errDefaultGit) DefaultBranch(context.Context, string) (string, error) {
	return "", errors.New("no HEAD")
}

func TestCacheStoreFailuresAreNamed(t *testing.T) {
	e := newTestEnv(t)
	e.git.branch = "main"
	run := e.cachePush(t, "main")
	view := e.saveCache(t, run, "k", "v1", "bytes")
	claims := e.cacheClaims(t, run)
	boom := errors.New("down")

	// A git reader whose default branch fails simply loses the fallback.
	e.svc.Git = errDefaultGit{e.git}
	if _, err := e.svc.cacheScopesForRun(ctx(), "o", "r", run); err != nil {
		t.Fatalf("broken default branch = %v", err)
	}
	e.svc.Git = e.git

	// Every store error surfaces through the service.
	good := e.svc.Store
	e.svc.Store = &errStore{Memory: e.store, getErr: boom}
	if _, err := e.svc.CacheLookupForJob(ctx(), claims, CacheLookupInput{Key: "k", Version: "v1"}); !errors.Is(err, boom) {
		t.Fatalf("lookup get error = %v", err)
	}
	if _, err := e.svc.ListCaches(ctx(), "o/r", e.reader()); !errors.Is(err, boom) {
		t.Fatalf("list get error = %v", err)
	}
	if _, err := e.svc.DiscardPRCaches(ctx(), "o/r", 3); !errors.Is(err, boom) {
		t.Fatalf("discard get error = %v", err)
	}
	if err := e.svc.reclaimCaches(ctx(), "o", "r", true, &GCResult{}); !errors.Is(err, boom) {
		t.Fatalf("reclaim get error = %v", err)
	}
	e.svc.Store = good

	// An attempt token whose run is gone.
	gone := claims
	gone.Run = "0000000000000000001-abcdef"
	if _, err := e.svc.CacheLookupForJob(ctx(), gone, CacheLookupInput{Key: "k", Version: "v1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run = %v", err)
	}
	if _, _, err := e.svc.CacheBlobForJob(ctx(), gone, view.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run blob = %v", err)
	}
	if err := e.svc.PutCacheBlobForJob(ctx(), gone, view.SHA256, 5, strings.NewReader("bytes")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run put = %v", err)
	}
	if _, err := e.svc.SaveCacheForJob(ctx(), gone, SaveCacheInput{Key: "k", Version: "v1", SHA256: view.SHA256, Size: 5}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run save = %v", err)
	}
	// A repo id that does not parse.
	bad := claims
	bad.Repo = "not a repo"
	if _, err := e.svc.CacheLookupForJob(ctx(), bad, CacheLookupInput{Key: "k", Version: "v1"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad repo = %v", err)
	}
	if _, err := e.svc.ListCaches(ctx(), "not a repo", e.reader()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("list bad repo = %v", err)
	}
	if _, err := e.svc.DiscardPRCaches(ctx(), "not a repo", 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("discard bad repo = %v", err)
	}
	// The blob read where the stored size disagrees with the index.
	sha := view.SHA256
	key := actionscache.BlobKey("o", "r", sha)
	raw, _ := json.Marshal(actionscache.Entry{
		ID: actionscache.NewEntryID("default", "size-mismatch", "v1", sha), Scope: "default",
		Key: "size-mismatch", Version: "v1", SHA256: sha, Size: 99, CreatedAt: e.now.Format(dateTimeFmt),
	})
	if _, err := store.PutBytes(ctx(), e.store, actionscache.IndexKey("o", "r"), mustJSONMap(map[string]any{
		"schema":      1,
		"entries":     []any{json.RawMessage(raw)},
		"blobs":       map[string]any{sha: map[string]any{"size": 5, "refs": 1}},
		"total_bytes": 99,
	}), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed index: %v", err)
	}
	_ = key
	if _, _, err := e.svc.CacheBlobForJob(ctx(), claims, sha); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("size mismatch = %v", err)
	}
}

// mustJSONMap marshals a fixture map (test only; a marshal failure is a panic).
func mustJSONMap(v map[string]any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestCacheHTTPHandlers(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "writer", Admin: true}
	e.git.branch = "main"
	run := e.cachePush(t, "main")
	claims := e.cacheClaims(t, run)
	tok := attemptTokenFor(t, e, run, claims)

	sha := cacheSHA("http-bytes")
	base := "/api/v1/threads/jobs/" + JobRef("o/r", claims.Run, claims.Job) + "/attempt/" + itoa(claims.Attempt) + "/cache"
	_ = run

	rec := doAuth(h, http.MethodPut, base+"/blobs/"+sha+"?size=10", "http-bytes", tok)
	if rec.Code != http.StatusCreated {
		t.Fatalf("blob put = %d: %s", rec.Code, rec.Body.String())
	}
	// A wrong ?size= and a short body are refusals.
	if rec := doAuth(h, http.MethodPut, base+"/blobs/"+sha+"?size=4", "http-bytes", tok); rec.Code != http.StatusBadRequest {
		t.Fatalf("size mismatch = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPut, base+"/blobs/"+sha+"?size=nope", "x", tok); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad size = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPut, base+"/blobs/"+sha+"?size=2147483648", "x", tok); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over cap = %d", rec.Code)
	}
	// Save, then look up and stream back.
	body := `{"key":"k","version":"v1","sha256":"` + sha + `","size":10,"files":1}`
	if rec := doAuth(h, http.MethodPost, base+"/save", body, tok); rec.Code != http.StatusCreated {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doAuth(h, http.MethodPost, base+"/lookup", `{"key":"k","version":"v1"}`, tok)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"hit":true`) {
		t.Fatalf("lookup = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doAuth(h, http.MethodGet, base+"/blobs/"+sha, "", tok)
	if rec.Code != http.StatusOK || rec.Body.String() != "http-bytes" || rec.Header().Get("X-Spindle-Cache-SHA256") != sha {
		t.Fatalf("blob get = %d %q", rec.Code, rec.Body.String())
	}
	// Bodies are strict: an unknown key and a bad body are refused.
	if rec := doAuth(h, http.MethodPost, base+"/lookup", `{"key":"k","version":"v1","bogus":1}`, tok); rec.Code != http.StatusBadRequest {
		t.Fatalf("strict lookup = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPost, base+"/save", `{`, tok); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad save body = %d", rec.Code)
	}
	// Wrong methods and unknown routes.
	if rec := doAuth(h, http.MethodGet, base+"/lookup", "", tok); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("lookup get = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodGet, base+"/save", "", tok); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("save get = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPost, base+"/blobs/"+sha, "", tok); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("blob post = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodGet, base+"/other", "", tok); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown cache route = %d", rec.Code)
	}
	// The repository lane: list, delete, and the role/method rules.
	rec = doAuth(h, http.MethodGet, "/o/r/api/threads/caches", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"caps"`) {
		t.Fatalf("list caches = %d: %s", rec.Code, rec.Body.String())
	}
	entries, _ := e.svc.ListCaches(ctx(), "o/r", e.reader())
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	if rec := doAuth(h, http.MethodPost, "/o/r/api/threads/caches", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("list post = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodGet, "/o/r/api/threads/caches/"+entries[0].ID, "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("cache get = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodDelete, "/o/r/api/threads/caches/"+entries[0].ID, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("cache delete = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doAuth(h, http.MethodDelete, "/o/r/api/threads/caches/missing", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing delete = %d", rec.Code)
	}
}

// attemptTokenFor mints a real attempt token for the claim.
func attemptTokenFor(t *testing.T, e *testEnv, run *Run, claims AttemptClaims) string {
	t.Helper()
	tok, _, err := e.svc.mintAttemptToken(claims)
	if err != nil {
		t.Fatalf("attempt token: %v", err)
	}
	return tok
}

func TestArtifactV4HTTPHandlers(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "writer"}
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)
	tok := attemptTokenFor(t, e, run, claims)
	base := "/api/v1/threads/jobs/" + JobRef("o/r", claims.Run, claims.Job) + "/attempt/" + itoa(claims.Attempt) + "/artifacts"

	// The v4 list.
	rec := doAuth(h, http.MethodGet, base, "", tok)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"build"`) {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	// prepare: without overwrite it conflicts; with it, it replaces.
	if rec := doAuth(h, http.MethodPost, base+"/build/prepare", `{"overwrite":false}`, tok); rec.Code != http.StatusConflict {
		t.Fatalf("prepare no-overwrite = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doAuth(h, http.MethodPost, base+"/build/prepare", `{}`, tok); rec.Code != http.StatusConflict {
		t.Fatalf("prepare default = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPost, base+"/nope/prepare", `{"overwrite":true}`, tok); rec.Code != http.StatusOK {
		t.Fatalf("prepare fresh = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPost, base+"/build/prepare", `{"overwrite":true,"bogus":1}`, tok); rec.Code != http.StatusBadRequest {
		t.Fatalf("prepare strict = %d", rec.Code)
	}
	// A finalize that claims overwrite without the prepare step is refused by
	// name: a late delete would remove the bytes just uploaded.
	if rec := doAuth(h, http.MethodPost, base, `{"name":"build","files":[{"path":"dist/a.txt","size":5,"index":0}],"overwrite":true}`, tok); rec.Code != http.StatusConflict {
		t.Fatalf("overwrite without prepare = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doAuth(h, http.MethodPost, base+"/build/prepare", `{"overwrite":true}`, tok); rec.Code != http.StatusOK {
		t.Fatalf("prepare overwrite = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodGet, base+"/build/prepare", "", tok); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("prepare get = %d", rec.Code)
	}
	// A finalize with an explicit compression level and overwrite decodes.
	e.uploadFile(t, claims, "build", 0, "dist/a.txt", "alpha")
	body := `{"name":"build","files":[{"path":"dist/a.txt","size":5,"index":0}],"overwrite":true,"compression_level":3}`
	if rec := doAuth(h, http.MethodPost, base, body, tok); rec.Code != http.StatusCreated {
		t.Fatalf("overwrite finalize = %d: %s", rec.Code, rec.Body.String())
	}
	// A level outside 0..9 is a 400.
	e.uploadFile(t, claims, "lvl", 0, "a.txt", "x")
	if rec := doAuth(h, http.MethodPost, base, `{"name":"lvl","files":[{"path":"a.txt","size":1,"index":0}],"compression_level":10}`, tok); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad level = %d", rec.Code)
	}
	// The repository lane: the manifest read and the delete.
	repoBase := "/o/r/api/threads/" + run.ID + "/artifacts"
	rec = doAuth(h, http.MethodGet, repoBase+"/build", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id"`) {
		t.Fatalf("manifest read = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doAuth(h, http.MethodDelete, repoBase+"/build", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"bytes_freed"`) {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doAuth(h, http.MethodGet, repoBase+"/build", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("read after delete = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPatch, repoBase+"/build", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("patch manifest = %d", rec.Code)
	}
}

func TestArtifactReaderAndDeleteEdges(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)

	if _, err := e.svc.GetArtifact(ctx(), "not a repo", run.ID, "build", e.reader()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad repo = %v", err)
	}
	if _, err := e.svc.GetArtifact(ctx(), "o/r", "bad", "build", e.reader()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad run = %v", err)
	}
	if _, err := e.svc.GetArtifact(ctx(), "o/r", "0000000000000000001-abcdef", "build", e.reader()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run = %v", err)
	}
	if _, err := e.svc.GetArtifact(ctx(), "o/r", run.ID, "nope", e.reader()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown artifact = %v", err)
	}
	if _, err := e.svc.DeleteArtifact(ctx(), "not a repo", run.ID, "build", e.writer()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("delete bad repo = %v", err)
	}
	if _, err := e.svc.DeleteArtifact(ctx(), "o/r", "bad", "build", e.writer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete bad run = %v", err)
	}

	// A reader-denied service (no public repo, anonymous) refuses the read.
	e.roles.public = false
	if _, err := e.svc.GetArtifact(ctx(), "o/r", run.ID, "build", e.public()); err == nil {
		t.Fatal("an anonymous read of a private repository succeeded")
	}
	e.roles.public = true

	// A per-artifact retention window that closed is a 404 before GC runs.
	e.uploadFile(t, claims, "short", 0, "s.txt", "x")
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{
		Name: "short", Files: []ArtifactFile{{Path: "s.txt", Size: 1, Index: 0}}, RetentionDays: 1,
	}); err != nil {
		t.Fatalf("finalize short: %v", err)
	}
	e.advance(48 * time.Hour)
	if _, err := e.svc.GetArtifact(ctx(), "o/r", run.ID, "short", e.reader()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired artifact = %v", err)
	}
	// A delete of an expired artifact still succeeds: reclaiming bytes is never
	// gated on the read window.
	if _, err := e.svc.DeleteArtifact(ctx(), "o/r", run.ID, "short", e.writer()); err != nil {
		t.Fatalf("delete of an expired artifact = %v", err)
	}
	// The expired artifact is still deleteable by name through the expiry path
	// (the sweep, not the API): the fixture's second finalize is a conflict, so
	// nothing else needs removing here.
}

func TestCrossRunSourceEdges(t *testing.T) {
	e := newTestEnv(t)
	e.roles.roles["maint"] = "maintain"
	e.git.add(testSHA, ".spindle/workflows/v4.yml", v4Workflow)
	upstream := e.mustCreate(t)[0]
	// A second, distinct run is the consumer (the source run itself never needs
	// an approval).
	second, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "writer"})
	if err != nil || len(second) == 0 {
		t.Fatalf("consumer run: %v (%d)", err, len(second))
	}
	consumer := second[0]

	// An attempt token whose run does not exist.
	missing := AttemptClaims{Repo: "o/r", Run: "0000000000000000001-abcdef", Job: "j", Attempt: 1, Scopes: attemptScopes(nil)}
	if _, _, _, err := e.svc.crossRunSource(ctx(), missing, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown consumer = %v", err)
	}
	claims := e.cacheClaims(t, consumer)
	// A consumer whose repo id does not parse.
	bad := claims
	bad.Repo = "not a repo"
	if _, _, _, err := e.svc.crossRunSource(ctx(), bad, "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad consumer repo = %v", err)
	}
	// An empty source id names the invalid-input rule.
	if _, _, _, err := e.svc.crossRunSource(ctx(), claims, " "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty source = %v", err)
	}
	// The same-run shortcut needs no approval.
	if _, _, _, err := e.svc.crossRunSource(ctx(), claims, claims.Run); err != nil {
		t.Fatalf("same run = %v", err)
	}
	// A consumer without approval is refused, and the refusal names D-3.
	consumerRun, err := e.svc.loadRun(ctx(), "o", "r", claims.Run)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if consumerRun.ApprovalKind != "" {
		t.Fatalf("fixture run unexpectedly held: %q", consumerRun.ApprovalKind)
	}
	if _, _, _, err := e.svc.crossRunSource(ctx(), claims, upstream.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unapproved = %v", err)
	}

	// A consumer approved for cross-run: a malformed source id, then a missing
	// source run.
	if _, cerr := e.svc.casUpdate(ctx(), RunKey("o", "r", claims.Run), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var r Run
		_ = json.Unmarshal(cur, &r)
		now := e.now.Format(dateTimeFmt)
		r.ApprovalKind = ApprovalCrossRun
		r.ApprovedAt = &now
		r.ApprovedBy = "maint"
		r.Version++
		return mustJSON(&r), true, nil
	}); cerr != nil {
		t.Fatalf("approve fixture: %v", cerr)
	}
	if _, _, _, err := e.svc.crossRunSource(ctx(), claims, "bad id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad source id = %v", err)
	}
	if _, _, _, err := e.svc.crossRunSource(ctx(), claims, "0000000000000000001-abcdef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing source = %v", err)
	}
	// A list whose source run exists but has no artifacts.
	list, err := e.svc.ArtifactListForJob(ctx(), claims, upstream.ID)
	if err != nil || len(list) != 0 {
		t.Fatalf("empty cross-run list = %v %v", list, err)
	}
	// A manifest read of an absent artifact.
	if _, err := e.svc.ArtifactManifestForJobFromRun(ctx(), claims, upstream.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent cross-run artifact = %v", err)
	}
	// The cross-run list/source paths surface a store failure.
	good := e.svc.Store
	e.svc.Store = &errStore{Memory: e.store, listErr: errors.New("down")}
	if _, err := e.svc.ArtifactListForJob(ctx(), claims, upstream.ID); err == nil {
		t.Fatal("a failing list was swallowed")
	}
	e.svc.Store = good
}
