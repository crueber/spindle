package threads

import (
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

// The deepest failure arms of the cache/artifact surface: a corrupt manifest, a
// revoked approval mid-download, a store outage at every step, and the handler
// equivalents.

func TestCrossRunGateMatrix(t *testing.T) {
	job := func(steps ...StepSpec) *Job {
		return &Job{Spec: &Spec{Version: SpecVersion, Steps: steps}}
	}
	// uses with a ref suffix and a case difference still counts.
	if !jobWantsCrossRun(job(StepSpec{Uses: "actions/download-artifact@v4", With: map[string]string{"run-id": "1"}})) {
		t.Fatal("@v4 download was not detected")
	}
	if jobWantsCrossRun(job(StepSpec{Uses: "actions/upload-artifact", With: map[string]string{"run-id": "1"}})) {
		t.Fatal("an upload with a run-id is not a cross-run consumer")
	}
	if jobWantsCrossRun(job(StepSpec{Uses: "actions/download-artifact", With: map[string]string{"run-id": "  "}})) {
		t.Fatal("an empty run-id is not a cross-run consumer")
	}
	if jobWantsCrossRun(nil) || jobWantsCrossRun(&Job{}) {
		t.Fatal("a job with no spec wants a cross-run read")
	}
	// Secrets in every authored site.
	sites := []*Spec{
		{Version: SpecVersion, If: `${{ secrets.A != '' }}`},
		{Version: SpecVersion, Env: map[string]string{"X": "${{ secrets.B }}"}},
		{Version: SpecVersion, Steps: []StepSpec{{If: `${{ secrets.C != '' }}`}}},
		{Version: SpecVersion, Steps: []StepSpec{{Name: "${{ secrets.D }}"}}},
		{Version: SpecVersion, Steps: []StepSpec{{WorkingDirectory: "${{ secrets.E }}"}}},
		{Version: SpecVersion, Steps: []StepSpec{{With: map[string]string{"x": "${{ secrets.F }}"}}}},
		{Version: SpecVersion, Steps: []StepSpec{{Env: map[string]string{"x": "${{ secrets.G }}"}}}},
		{Version: SpecVersion, Actions: map[string]ActionSpec{"a": {Steps: []StepSpec{{Run: "${{ secrets.H }}"}}}}},
		{Version: SpecVersion, Actions: map[string]ActionSpec{"a": {Env: map[string]string{"x": "${{ secrets.I }}"}}}},
	}
	for i, spec := range sites {
		if !specHoldsRepoSecret(spec) {
			t.Fatalf("site %d did not detect a repository secret", i)
		}
	}
	if specHoldsRepoSecret(nil) {
		t.Fatal("a nil spec holds secrets")
	}
	if specHoldsRepoSecret(&Spec{Version: SpecVersion, Steps: []StepSpec{{Run: "no refs"}}}) {
		t.Fatal("a plain step holds secrets")
	}
}

func TestArtifactUploadFaults(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	// A second write of the same (attempt, index) with a different size is a
	// conflict.
	e.uploadFile(t, claims, "build", 0, "a.txt", "alpha")
	if _, err := e.svc.AddArtifactBlob(ctx(), claims, "build", 0, "a.txt", 99, strings.NewReader(strings.Repeat("x", 99))); !errors.Is(err, ErrConflict) {
		t.Fatalf("size conflict = %v", err)
	}
	// A body shorter than its advertised size never stores a blob (the store's
	// known-length write refuses it, and the engine's own check is the backstop).
	if _, err := e.svc.AddArtifactBlob(ctx(), claims, "short", 0, "a.txt", 10, strings.NewReader("x")); err == nil {
		t.Fatal("a short body was accepted")
	}
	if meta, _ := e.store.Head(ctx(), ArtifactBlobKey("o", "r", run.ID, "short", claims.Attempt, 0)); meta != nil {
		t.Fatal("a short body left a blob behind")
	}
	// The 412 dedupe path re-reads the stored blob; a result that is not an
	// Object cannot satisfy the size check and is a conflict.
	fault := &artifactFaultStore{ObjectStore: e.svc.Store, notObject: true, getErrKey: "/build/"}
	e.svc.Store = fault
	if _, err := e.svc.AddArtifactBlob(ctx(), claims, "build", 0, "a.txt", 5, strings.NewReader("alpha")); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-object 412 = %v", err)
	}
	e.svc.Store = fault.ObjectStore

	// Commit the artifact so prepare has something to replace.
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{
		Name: "build", Files: []ArtifactFile{{Path: "a.txt", Size: 5, Index: 0}},
	}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	// Prepare: a bad name, a store read failure and a delete failure.
	if _, err := e.svc.PrepareArtifactUpload(ctx(), claims, "..", true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("prepare bad name = %v", err)
	}
	good := e.svc.Store
	e.svc.Store = &artifactFaultStore{ObjectStore: good, getErr: errors.New("down"), getErrKey: "manifest"}
	if _, err := e.svc.PrepareArtifactUpload(ctx(), claims, "build", true); err == nil {
		t.Fatal("prepare swallowed a read failure")
	}
	e.svc.Store = &artifactFaultStore{ObjectStore: good, deleteErrKey: "/build/"}
	if _, err := e.svc.PrepareArtifactUpload(ctx(), claims, "build", true); err == nil {
		t.Fatal("prepare swallowed a delete failure")
	}
	e.svc.Store = good
	// A failed attempt token.
	bad := claims
	bad.Scopes = []string{ScopeLog}
	if _, err := e.svc.PrepareArtifactUpload(ctx(), bad, "x", true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("prepare without scope = %v", err)
	}
}

func TestArtifactFinalizeAndReadFaults(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)

	// Retention out of range on the finalize body.
	e.uploadFile(t, claims, "days", 0, "a.txt", "x")
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{
		Name: "days", Files: []ArtifactFile{{Path: "a.txt", Size: 1, Index: 0}}, RetentionDays: 99,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("retention 99 = %v", err)
	}
	// A corrupt manifest is a named error on every read.
	if _, err := store.PutBytes(ctx(), e.store, ArtifactManifestKey("o", "r", run.ID, "corrupt"), []byte("{"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.ArtifactManifestForJob(ctx(), claims, "corrupt"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt manifest = %v", err)
	}
	if _, err := e.svc.GetArtifact(ctx(), "o/r", run.ID, "corrupt", e.reader()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt manifest read = %v", err)
	}
	// A blob read whose stored size disagrees with the manifest.
	art := &Artifact{
		ID: artifactID(run.ID, "sized"), Name: "sized", Run: run.ID, Attempt: claims.Attempt, TotalSize: 3,
		Files:     []ArtifactFile{{Path: "s.txt", Size: 3, Index: 0}},
		CreatedAt: e.now.Format(dateTimeFmt),
	}
	raw, _ := json.Marshal(art)
	if err := e.svc.putCreate(ctx(), ArtifactManifestKey("o", "r", run.ID, "sized"), raw); err != nil {
		t.Fatalf("seed sized: %v", err)
	}
	if _, err := store.PutBytes(ctx(), e.store, ArtifactBlobKey("o", "r", run.ID, "sized", claims.Attempt, 0), []byte("toolong"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	if _, _, err := e.svc.ArtifactBlobForJob(ctx(), claims, "sized", 0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("blob size mismatch = %v", err)
	}
	// The zip writer refuses the same mismatch instead of shipping bad bytes.
	dl, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "sized", e.reader())
	if err != nil {
		t.Fatalf("open sized: %v", err)
	}
	if err := WriteArtifactZip(ctx(), io.Discard, dl); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("zip size mismatch = %v", err)
	}
	// Download caps: a manifest over the per-run cap is refused.
	e.svc.MaxArtifactBytesPerRun = 1
	if _, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "build", e.reader()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("download cap = %v", err)
	}
	e.svc.MaxArtifactBytesPerRun = 0

	// A store outage at each read step is surfaced.
	good := e.svc.Store
	e.svc.Store = &artifactFaultStore{ObjectStore: good, getErr: errors.New("down"), getErrKey: "manifest"}
	if _, err := e.svc.ListArtifacts(ctx(), "o/r", run.ID, e.reader()); err == nil {
		t.Fatal("ListArtifacts swallowed a store failure")
	}
	if _, _, err := e.svc.ArtifactBlobForJob(ctx(), claims, "build", 0); err == nil {
		t.Fatal("ArtifactBlobForJob swallowed a store failure")
	}
	e.svc.Store = &artifactFaultStore{ObjectStore: good, getErr: errors.New("down"), getErrKey: "blobs/"}
	if _, _, err := e.svc.ArtifactBlobForJob(ctx(), claims, "build", 0); err == nil {
		t.Fatal("a blob read failure was swallowed")
	}
	e.svc.Store = good
}

func TestCrossRunBlobGateRevokedAndFaults(t *testing.T) {
	e := newTestEnv(t)
	e.roles.roles["maint"] = "maintain"
	e.git.add(testSHA, ".spindle/workflows/v4.yml", v4Workflow)
	upstream := e.mustCreate(t)[0]
	upClaims := e.artifactClaims(t, upstream)
	e.finalizeBuild(t, upClaims, 0)

	second, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "writer"})
	if err != nil || len(second) == 0 {
		t.Fatalf("consumer run: %v", err)
	}
	consumer := second[0]
	claims := e.cacheClaims(t, consumer)
	// Approve the consumer.
	if _, cerr := e.svc.casUpdate(ctx(), RunKey("o", "r", consumer.ID), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var r Run
		_ = json.Unmarshal(cur, &r)
		now := e.now.Format(dateTimeFmt)
		r.ApprovalKind = ApprovalCrossRun
		r.ApprovedAt = &now
		r.Version++
		return mustJSON(&r), true, nil
	}); cerr != nil {
		t.Fatalf("approve: %v", cerr)
	}
	// A successful blob read.
	if _, _, err := e.svc.ArtifactBlobForJobFromRun(ctx(), claims, upstream.ID, "build", 0); err != nil {
		t.Fatalf("blob read: %v", err)
	}
	// Revoke the approval between the manifest and the blob: the blob refuses.
	if _, cerr := e.svc.casUpdate(ctx(), RunKey("o", "r", consumer.ID), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var r Run
		_ = json.Unmarshal(cur, &r)
		r.ApprovalKind = ""
		r.ApprovedAt = nil
		r.Version++
		return mustJSON(&r), true, nil
	}); cerr != nil {
		t.Fatalf("revoke: %v", cerr)
	}
	if _, _, err := e.svc.ArtifactBlobForJobFromRun(ctx(), claims, upstream.ID, "build", 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked blob read = %v", err)
	}
	// Re-approve for the fault cases.
	_, _ = e.svc.casUpdate(ctx(), RunKey("o", "r", consumer.ID), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var r Run
		_ = json.Unmarshal(cur, &r)
		now := e.now.Format(dateTimeFmt)
		r.ApprovalKind = ApprovalCrossRun
		r.ApprovedAt = &now
		r.Version++
		return mustJSON(&r), true, nil
	})
	good := e.svc.Store
	e.svc.Store = &artifactFaultStore{ObjectStore: good, notObject: true, getErrKey: "blobs/"}
	if _, _, err := e.svc.ArtifactBlobForJobFromRun(ctx(), claims, upstream.ID, "build", 0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("non-object blob = %v", err)
	}
	e.svc.Store = &artifactFaultStore{ObjectStore: good, getErr: errors.New("down"), getErrKey: "blobs/"}
	if _, _, err := e.svc.ArtifactBlobForJobFromRun(ctx(), claims, upstream.ID, "build", 0); err == nil {
		t.Fatal("a cross-run blob read failure was swallowed")
	}
	e.svc.Store = good
	// A bad repo on the consumer.
	badRepo := claims
	badRepo.Repo = "not a repo"
	if _, _, err := e.svc.ArtifactBlobForJobFromRun(ctx(), badRepo, upstream.ID, "build", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad repo = %v", err)
	}
	// An expired source artifact is a 404 on the cross-run read too.
	expiring := &Artifact{
		ID: artifactID(upstream.ID, "expired"), Name: "expired", Run: upstream.ID, Attempt: 1, TotalSize: 1,
		Files:         []ArtifactFile{{Path: "e.txt", Size: 1, Index: 0}},
		RetentionDays: 1, CreatedAt: e.now.Add(-72 * 3600e9).Format(dateTimeFmt),
	}
	raw, _ := json.Marshal(expiring)
	if err := e.svc.putCreate(ctx(), ArtifactManifestKey("o", "r", upstream.ID, "expired"), raw); err != nil {
		t.Fatalf("seed expired: %v", err)
	}
	if _, err := e.svc.ArtifactManifestForJobFromRun(ctx(), claims, upstream.ID, "expired"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired cross-run read = %v", err)
	}
	// An attempt token without the artifact scope.
	noScope := claims
	noScope.Scopes = []string{ScopeLog}
	if _, err := e.svc.ArtifactManifestForJobFromRun(ctx(), noScope, upstream.ID, "build"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("manifest without scope = %v", err)
	}
	if _, err := e.svc.ArtifactListForJob(ctx(), noScope, upstream.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("list without scope = %v", err)
	}
	if _, _, err := e.svc.ArtifactBlobForJobFromRun(ctx(), noScope, upstream.ID, "build", 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("blob without scope = %v", err)
	}
}

func TestCacheServiceFaults(t *testing.T) {
	e := newTestEnv(t)
	e.git.branch = "main"
	run := e.cachePush(t, "main")
	view := e.saveCache(t, run, "k", "v1", "bytes")
	claims := e.cacheClaims(t, run)

	// A pull_request run with no number is refused by the lookup path too.
	broken := *run
	broken.Event = EventPullReq
	broken.EventPayload = nil
	e.storeRunForTest(t, &broken)
	if _, err := e.svc.CacheLookupForJob(ctx(), claims, CacheLookupInput{Key: "k", Version: "v1"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("PR without number lookup = %v", err)
	}
	e.storeRunForTest(t, run)

	// A missing payload referenced by the index is corruption.
	if err := e.store.Delete(ctx(), actionscache.BlobKey("o", "r", view.SHA256), ""); err != nil {
		t.Fatalf("drop blob: %v", err)
	}
	if _, _, err := e.svc.CacheBlobForJob(ctx(), claims, view.SHA256); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("missing payload = %v", err)
	}
	// A digest the run cannot see.
	if _, _, err := e.svc.CacheBlobForJob(ctx(), claims, cacheSHA("unknown")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invisible digest = %v", err)
	}
	// A store outage on the blob path.
	good := e.svc.Store
	e.svc.Store = &errStore{Memory: e.store, getErr: errors.New("down")}
	if _, _, err := e.svc.CacheBlobForJob(ctx(), claims, view.SHA256); err == nil {
		t.Fatal("blob read swallowed a store failure")
	}
	e.svc.Store = good
	// A tag run has no write scope for either the blob or the save.
	tagRun := *run
	tagRun.Branch = ""
	e.storeRunForTest(t, &tagRun)
	if err := e.svc.PutCacheBlobForJob(ctx(), claims, view.SHA256, 5, strings.NewReader("bytes")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tag blob write = %v", err)
	}
	if _, err := e.svc.SaveCacheForJob(ctx(), claims, SaveCacheInput{Key: "k", Version: "v1", SHA256: view.SHA256, Size: 5}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tag save = %v", err)
	}
	// A bad repo on the delete path.
	if _, err := e.svc.DeleteCache(ctx(), "not a repo", "id", e.writer()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("delete bad repo = %v", err)
	}
	// An empty cache list serialises as [] not null.
	if entries, err := e.svc.ListCaches(ctx(), "o/r", e.reader()); err != nil || entries == nil {
		t.Fatalf("list = %v %v", entries, err)
	}
}

// storeRunForTest rewrites one run header (the fixture's clock and version are
// preserved by the caller's copy).
func (e *testEnv) storeRunForTest(t *testing.T, run *Run) {
	t.Helper()
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	if _, err := store.PutBytes(ctx(), e.store, RunKey("o", "r", run.ID), raw, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("store run: %v", err)
	}
}

func TestCacheHTTPFaults(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "writer", Admin: true}
	e.git.branch = "main"
	run := e.cachePush(t, "main")
	claims := e.cacheClaims(t, run)
	tok := attemptTokenFor(t, e, run, claims)
	base := "/api/v1/threads/jobs/" + JobRef("o/r", claims.Run, claims.Job) + "/attempt/" + itoa(claims.Attempt) + "/cache"

	// A lookup whose key is refused by the engine.
	if rec := doAuth(h, http.MethodPost, base+"/lookup", `{"key":"bad\u0001","version":"v1"}`, tok); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad key = %d", rec.Code)
	}
	// A save whose payload is absent.
	sha := cacheSHA("never")
	if rec := doAuth(h, http.MethodPost, base+"/save", `{"key":"k","version":"v1","sha256":"`+sha+`","size":5}`, tok); rec.Code != http.StatusBadRequest {
		t.Fatalf("save without payload = %d", rec.Code)
	}
	// A blob GET whose digest is unknown.
	if rec := doAuth(h, http.MethodGet, base+"/blobs/"+sha, "", tok); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown blob = %d", rec.Code)
	}
	// A repository cache list without a session is a 401 on a private repo.
	e.roles.public = false
	*cur = auth.Anonymous()
	if rec := doAuth(h, http.MethodGet, "/o/r/api/threads/caches", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("private list = %d", rec.Code)
	}
	e.roles.public = true
	*cur = auth.Principal{Name: "writer", Admin: true}
}

func TestArtifactHTTPFaults(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "writer", Admin: true}
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	tok := attemptTokenFor(t, e, run, claims)
	base := "/api/v1/threads/jobs/" + JobRef("o/r", claims.Run, claims.Job) + "/attempt/" + itoa(claims.Attempt) + "/artifacts"

	// No artifacts: the list answers [] and the manifest 404s.
	rec := doAuth(h, http.MethodGet, base, "", tok)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"artifacts":[]`) {
		t.Fatalf("empty list = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doAuth(h, http.MethodGet, base+"/nope", "", tok); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown manifest = %d", rec.Code)
	}
	// A cross-run list without the D-3 approval is refused before the source id
	// is even resolved (fail closed).
	if rec := doAuth(h, http.MethodGet, base+"?run_id=bad", "", tok); rec.Code != http.StatusForbidden {
		t.Fatalf("unapproved cross-run list = %d", rec.Code)
	}
	// A prepare for an invalid name.
	if rec := doAuth(h, http.MethodPost, base+"/..%2Fescape/prepare", `{"overwrite":true}`, tok); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad prepare name = %d: %s", rec.Code, rec.Body.String())
	}
	// A blob index that is not a number.
	if rec := doAuth(h, http.MethodPut, base+"/build/blobs/x?path=a&size=1", "x", tok); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad index = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodGet, base+"/build/blobs/x", "", tok); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad index get = %d", rec.Code)
	}
	// A blob PUT whose body does not match ?size= (Content-Length set by the
	// client) is refused.
	if rec := doAuth(h, http.MethodPut, base+"/build/blobs/0?path=a.txt&size=4", "ten", tok); rec.Code != http.StatusBadRequest {
		t.Fatalf("length mismatch = %d", rec.Code)
	}
	// Deep unknown subroute.
	if rec := doAuth(h, http.MethodGet, base+"/build/blobs/0/extra", "", tok); rec.Code != http.StatusNotFound {
		t.Fatalf("deep unknown = %d", rec.Code)
	}
	// The repository lane without a session (private) is a 401.
	e.roles.public = false
	*cur = auth.Anonymous()
	if rec := doAuth(h, http.MethodGet, "/o/r/api/threads/"+run.ID+"/artifacts", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("private artifact list = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodGet, "/o/r/api/threads/"+run.ID+"/artifacts/build", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("private manifest = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodDelete, "/o/r/api/threads/"+run.ID+"/artifacts/build", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("private delete = %d", rec.Code)
	}
	e.roles.public = true
	*cur = auth.Principal{Name: "writer", Admin: true}
}

func TestCacheAndArtifactStoreOutageArms(t *testing.T) {
	e := newTestEnv(t)
	e.git.branch = "main"
	run := e.cachePush(t, "main")
	view := e.saveCache(t, run, "k", "v1", "bytes")
	claims := e.cacheClaims(t, run)

	good := e.svc.Store
	e.svc.Store = &errStore{Memory: e.store, getErr: errors.New("down")}
	// Every service entry point maps the outage, including the run read.
	if _, err := e.svc.CacheLookupForJob(ctx(), claims, CacheLookupInput{Key: "k", Version: "v1"}); err == nil {
		t.Fatal("lookup swallowed the outage")
	}
	if _, _, err := e.svc.CacheBlobForJob(ctx(), claims, view.SHA256); err == nil {
		t.Fatal("blob read swallowed the outage")
	}
	if err := e.svc.PutCacheBlobForJob(ctx(), claims, view.SHA256, 5, strings.NewReader("bytes")); err == nil {
		t.Fatal("blob put swallowed the outage")
	}
	if _, err := e.svc.SaveCacheForJob(ctx(), claims, SaveCacheInput{Key: "k", Version: "v1", SHA256: view.SHA256, Size: 5}); err == nil {
		t.Fatal("save swallowed the outage")
	}
	e.svc.Store = good

	// A run with no branch: a PR whose payload names no base and a tag run.
	empty := *run
	empty.Event = EventPullReq
	empty.Branch = ""
	empty.EventPayload = map[string]any{"pull_request": map[string]any{"number": 3}}
	set, err := e.svc.cacheScopesForRun(ctx(), "o", "r", &empty)
	if err != nil || len(set.Read) != 1 || set.Write == nil || set.Write.String() != "pr:3" {
		t.Fatalf("PR without a base = %+v %v", set, err)
	}

	// Prepare with an unparseable repo claim.
	badRepo := claims
	badRepo.Repo = "not a repo"
	if _, err := e.svc.PrepareArtifactUpload(ctx(), badRepo, "x", true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("prepare bad repo = %v", err)
	}

	// An empty repository's cache list is [].
	emptyEnv := newTestEnv(t)
	if entries, err := emptyEnv.svc.ListCaches(ctx(), "o/r", emptyEnv.reader()); err != nil || entries == nil || len(entries) != 0 {
		t.Fatalf("empty list = %v %v", entries, err)
	}
}

func TestArtifactDryRunReclaimAndPhantomManifest(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	// A phantom manifest key (reported by List, absent in the store) is skipped.
	e.svc.Store = &errStore{Memory: e.store, phantom: []string{ArtifactManifestKey("o", "r", run.ID, "ghost")}}
	if arts, err := e.svc.loadArtifacts(ctx(), "o", "r", run.ID); err != nil || len(arts) != 0 {
		t.Fatalf("phantom manifest = %v %v", arts, err)
	}
	e.svc.Store = e.store
	// A short-retention artifact, then a dry-run retention pass that reports the
	// same numbers it would reclaim.
	e.uploadFile(t, claims, "short", 0, "s.txt", "abcdef")
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{
		Name: "short", Files: []ArtifactFile{{Path: "s.txt", Size: 6, Index: 0}}, RetentionDays: 1,
	}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	e.advance(48 * time.Hour)
	plan, err := e.svc.PlanRetention(ctx(), "o", "r", nil)
	if err != nil {
		t.Fatalf("PlanRetention: %v", err)
	}
	if plan.ArtifactsDeleted != 1 || plan.ArtifactBytes == 0 || plan.BytesFreed == 0 {
		t.Fatalf("dry-run artifact reclaim = %+v", plan)
	}
	// The dry run deleted nothing; the apply pass reports the same bytes.
	applied, err := e.svc.Retain(ctx(), "o", "r", nil)
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if applied.ArtifactsDeleted != 1 || applied.ArtifactBytes != plan.ArtifactBytes {
		t.Fatalf("apply vs plan: %+v vs %+v", applied, plan)
	}
}
