package threads

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Run artifacts (15 §2.5): manifest records, the key layout, name uniqueness,
// retention clamps, caps, the permission matrix and the ZIP download.

// artifactClaims builds an attempt token for one job of a created run.
func (e *testEnv) artifactClaims(t *testing.T, run *Run) AttemptClaims {
	t.Helper()
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	if err != nil || len(jobs) == 0 {
		t.Fatalf("loadJobs: %v (%d jobs)", err, len(jobs))
	}
	return AttemptClaims{
		Repo: "o/r", Run: run.ID, Job: jobs[0].ID, Attempt: jobs[0].Attempt,
		Runner: "r1", Scopes: attemptScopes([]string{ScopeRead}),
		Exp: e.now.Add(time.Hour).Unix(),
	}
}

// uploadFile is one AddArtifactBlob call.
func (e *testEnv) uploadFile(t *testing.T, claims AttemptClaims, name string, index int, path, body string) {
	t.Helper()
	if _, err := e.svc.AddArtifactBlob(ctx(), claims, name, index, path, int64(len(body)), strings.NewReader(body)); err != nil {
		t.Fatalf("AddArtifactBlob(%s): %v", path, err)
	}
}

// finalizeArtifact commits a two-file artifact named "build".
func (e *testEnv) finalizeBuild(t *testing.T, claims AttemptClaims, days int) *ArtifactView {
	t.Helper()
	e.uploadFile(t, claims, "build", 0, "dist/a.txt", "alpha")
	e.uploadFile(t, claims, "build", 1, "dist/sub/b.txt", "beta")
	view, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{
		Name: "build",
		Files: []ArtifactFile{
			{Path: "dist/a.txt", Size: 5, Index: 0},
			{Path: "dist/sub/b.txt", Size: 4, Index: 1},
		},
		RetentionDays: days,
	})
	if err != nil {
		t.Fatalf("FinalizeArtifact: %v", err)
	}
	return view
}

func TestArtifactKeyLayout(t *testing.T) {
	if got := ArtifactManifestKey("o", "r", "run1", "build"); got != "repos/o/r/threads/runs/run1/artifacts/build/manifest.json" {
		t.Fatalf("manifest key = %q", got)
	}
	if got := ArtifactBlobKey("o", "r", "run1", "build", 2, 7); got != "repos/o/r/threads/runs/run1/artifacts/build/blobs/002-000007" {
		t.Fatalf("blob key = %q", got)
	}
	if got := ArtifactBlobKey("o", "r", "run1", "build", 10, 1); !strings.HasSuffix(got, "/blobs/010-000001") {
		t.Fatalf("blob key attempt padding = %q", got)
	}
	at := time.Unix(1_800_000_000, 0).UTC()
	if got := ArtifactExpiryKey("o", "r", "run1", "build", at); got != "repos/o/r/threads/artifact-expiry/0000000001800000000-run1-build.json" {
		t.Fatalf("expiry key = %q", got)
	}
	if idx, ok := parseArtifactBlobIndex("repos/o/r/threads/runs/x/artifacts/build/blobs/003-000042"); !ok || idx != 42 {
		t.Fatalf("blob index parse = %d %v", idx, ok)
	}
	if _, ok := parseArtifactBlobIndex("repos/o/r/threads/runs/x/artifacts/build/blobs/garbage"); ok {
		t.Fatal("garbage blob key parsed as an index")
	}
}

func TestArtifactNamesAreUniquePerRun(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)

	// A second finalize of the same name is a conflict, even with different
	// bytes: a replace would let a later job rewrite what an earlier one
	// published.
	e.uploadFile(t, claims, "build", 0, "dist/a.txt", "other")
	_, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{
		Name: "build", Files: []ArtifactFile{{Path: "dist/a.txt", Size: 5, Index: 0}},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second upload = %v, want ErrConflict", err)
	}
	// A different name is fine.
	e.uploadFile(t, claims, "logs", 0, "dist/a.txt", "alpha")
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{
		Name: "logs", Files: []ArtifactFile{{Path: "dist/a.txt", Size: 5, Index: 0}},
	}); err != nil {
		t.Fatalf("second artifact: %v", err)
	}
	arts, err := e.svc.ListArtifacts(ctx(), "o/r", run.ID, e.reader())
	if err != nil || len(arts) != 2 {
		t.Fatalf("artifacts = %v (%v)", arts, err)
	}
	if arts[0].Name != "build" || arts[1].Name != "logs" {
		t.Fatalf("artifacts not name-sorted: %v", arts)
	}
	if arts[0].TotalSize != 9 || len(arts[0].Files) != 2 {
		t.Fatalf("manifest = %+v", arts[0])
	}
	// The stored manifest is the record the list reads (one GET by known key).
	raw, _, err := e.svc.getJSON(ctx(), ArtifactManifestKey("o", "r", run.ID, "build"))
	if err != nil || raw == nil {
		t.Fatalf("manifest object: %v", err)
	}
	var stored Artifact
	if err := json.Unmarshal(raw, &stored); err != nil || stored.Run != run.ID || stored.Files[1].Path != "dist/sub/b.txt" {
		t.Fatalf("stored manifest = %+v (%v)", stored, err)
	}
	// GetRun carries the artifacts (the page and API read model).
	view, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.reader())
	if err != nil || len(view.Artifacts) != 2 {
		t.Fatalf("RunView artifacts = %v (%v)", view, err)
	}
}

func TestArtifactFinalizeRefusesIncompleteUploads(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.uploadFile(t, claims, "build", 0, "dist/a.txt", "alpha")
	// File 1 was never uploaded.
	_, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{
		Name: "build",
		Files: []ArtifactFile{
			{Path: "dist/a.txt", Size: 5, Index: 0},
			{Path: "dist/b.txt", Size: 4, Index: 1},
		},
	})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "never uploaded") {
		t.Fatalf("incomplete upload = %v", err)
	}
	// A size mismatch is refused too (truncated blob).
	_, err = e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{
		Name: "build", Files: []ArtifactFile{{Path: "dist/a.txt", Size: 99, Index: 0}},
	})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "the manifest says") {
		t.Fatalf("size mismatch = %v", err)
	}
	// No manifest was committed, so the run lists none.
	arts, _ := e.svc.ListArtifacts(ctx(), "o/r", run.ID, e.reader())
	if len(arts) != 0 {
		t.Fatalf("failed uploads became visible: %v", arts)
	}
}

func TestArtifactManifestRecordValidation(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.uploadFile(t, claims, "build", 0, "dist/a.txt", "alpha")
	cases := []struct {
		name string
		in   ArtifactFinalizeInput
		want error
	}{
		{"no files", ArtifactFinalizeInput{Name: "build"}, ErrInvalid},
		{"bad name", ArtifactFinalizeInput{Name: "../x", Files: []ArtifactFile{{Path: "a", Index: 0}}}, ErrInvalid},
		{"absolute path", ArtifactFinalizeInput{Name: "build", Files: []ArtifactFile{{Path: "/etc/passwd", Size: 1, Index: 0}}}, ErrInvalid},
		{"climbing path", ArtifactFinalizeInput{Name: "build", Files: []ArtifactFile{{Path: "../../x", Size: 1, Index: 0}}}, ErrInvalid},
		{"duplicate path", ArtifactFinalizeInput{Name: "build", Files: []ArtifactFile{
			{Path: "a", Size: 1, Index: 0}, {Path: "a", Size: 1, Index: 1}}}, ErrInvalid},
		{"duplicate index", ArtifactFinalizeInput{Name: "build", Files: []ArtifactFile{
			{Path: "a", Size: 1, Index: 0}, {Path: "b", Size: 1, Index: 0}}}, ErrInvalid},
		{"over per-file cap", ArtifactFinalizeInput{Name: "build", Files: []ArtifactFile{
			{Path: "a", Size: DefaultMaxArtifactBytesPerFile + 1, Index: 0}}}, ErrTooLarge},
		{"retention too long", ArtifactFinalizeInput{Name: "build", RetentionDays: MaxArtifactRetentionDays + 1,
			Files: []ArtifactFile{{Path: "a", Size: 1, Index: 0}}}, ErrInvalid},
	}
	for _, tc := range cases {
		if _, err := e.svc.FinalizeArtifact(ctx(), claims, tc.in); !errors.Is(err, tc.want) {
			t.Fatalf("%s = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestArtifactSizeCaps(t *testing.T) {
	e := newTestEnv(t)
	e.svc.MaxArtifactBytesPerRun = 10
	e.svc.MaxArtifactBytesPerFile = 6
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)

	// The per-file cap is enforced while receiving.
	_, err := e.svc.AddArtifactBlob(ctx(), claims, "build", 0, "big.bin", 7, strings.NewReader("1234567"))
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "max_artifact_bytes_per_file") {
		t.Fatalf("per-file cap = %v", err)
	}
	// The per-run cap is enforced before the write (the first artifact already
	// holds 6 bytes).
	e.uploadFile(t, claims, "first", 0, "a.txt", "012345")
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{
		Name: "first", Files: []ArtifactFile{{Path: "a.txt", Size: 6, Index: 0}},
	}); err != nil {
		t.Fatalf("first artifact: %v", err)
	}
	_, err = e.svc.AddArtifactBlob(ctx(), claims, "second", 0, "b.txt", 5, strings.NewReader("12345"))
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "max_artifact_bytes_per_run") {
		t.Fatalf("per-run cap = %v", err)
	}
	// The download cap uses the same per-run ceiling.
	dl, oerr := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "first", e.reader())
	if oerr != nil || dl.TotalSize != 6 {
		t.Fatalf("open = %v (%v)", dl, oerr)
	}
}

func TestArtifactRetentionShortensNeverExtends(t *testing.T) {
	e := newTestEnv(t)
	e.svc.Retention = 30 * 24 * time.Hour
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	view := e.finalizeBuild(t, claims, 2) // two days, shorter than 30
	created, _ := time.Parse(dateTimeFmt, view.CreatedAt)
	want := created.Add(2 * 24 * time.Hour)
	got, _ := time.Parse(dateTimeFmt, view.ExpiresAt)
	if !got.Equal(want) {
		t.Fatalf("expiry = %s, want %s", got, want)
	}
	// A retention-days longer than the run's window does NOT extend it.
	long := e.finalizeArtifactNamed(t, claims, "long", 90)
	gotLong, _ := time.Parse(dateTimeFmt, long.ExpiresAt)
	if gotLong.After(runExpiry(run, e.svc.retention())) {
		t.Fatalf("retention-days extended the run window: %s > %s", gotLong, runExpiry(run, e.svc.retention()))
	}
	// The expired short artifact is 404 to a reader and absent from the list,
	// even before GC frees its bytes.
	e.advance(3 * 24 * time.Hour)
	if _, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "build", e.reader()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired artifact open = %v, want ErrNotFound", err)
	}
	arts, err := e.svc.ListArtifacts(ctx(), "o/r", run.ID, e.reader())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(arts) != 1 || arts[0].Name != "long" {
		t.Fatalf("expired artifact still listed: %v", arts)
	}
}

// finalizeArtifactNamed commits one one-file artifact.
func (e *testEnv) finalizeArtifactNamed(t *testing.T, claims AttemptClaims, name string, days int) *ArtifactView {
	t.Helper()
	e.uploadFile(t, claims, name, 0, name+".txt", "x")
	view, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{
		Name: name, Files: []ArtifactFile{{Path: name + ".txt", Size: 1, Index: 0}}, RetentionDays: days,
	})
	if err != nil {
		t.Fatalf("finalize %s: %v", name, err)
	}
	return view
}

func TestArtifactPermissionMatrix(t *testing.T) {
	e := newTestEnv(t)
	e.roles.public = false
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)

	// A reader of the repository may list and download.
	if _, err := e.svc.ListArtifacts(ctx(), "o/r", run.ID, e.reader()); err != nil {
		t.Fatalf("reader list: %v", err)
	}
	if _, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "build", e.reader()); err != nil {
		t.Fatalf("reader open: %v", err)
	}
	// An anonymous caller is 401 and an unrelated principal is 403.
	if _, err := e.svc.ListArtifacts(ctx(), "o/r", run.ID, auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous list = %v", err)
	}
	if _, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "build", auth.Principal{Name: "stranger"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger open = %v", err)
	}
	// A run id from another repository is unknown here: the URL's repository is
	// the authority, never the caller's reach.
	others, err := e.svc.CreateRun(ctx(), "o/other", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "writer"})
	if err != nil || len(others) == 0 {
		t.Fatalf("second repo run: %v (%d)", err, len(others))
	}
	if _, err := e.svc.OpenArtifact(ctx(), "o/r", others[0].ID, "build", e.reader()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-repo open = %v", err)
	}
	// And the other repository's own run lists nothing, though it is readable.
	if arts, err := e.svc.ListArtifacts(ctx(), "o/other", others[0].ID, e.reader()); err != nil || len(arts) != 0 {
		t.Fatalf("other repo list = %v (%v)", arts, err)
	}
}

func TestArtifactBlobDigestsAndJobScopedReads(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	view, err := e.svc.AddArtifactBlob(ctx(), claims, "build", 0, "dist/a.txt", 5, strings.NewReader("alpha"))
	if err != nil {
		t.Fatalf("blob: %v", err)
	}
	// The server computes the digest (a client one is never trusted).
	if view.SHA256 != "8ed3f6ad685b959ead7022518e1af76cd816f8e8ec7ccdda1ed4018e8f2223f8" {
		t.Fatalf("digest = %s", view.SHA256)
	}
	e.finalizeBuild(t, claims, 0)
	// The runner's download step sees the same manifest and can read a listed
	// blob only.
	man, err := e.svc.ArtifactManifestForJob(ctx(), claims, "build")
	if err != nil || len(man.Files) != 2 {
		t.Fatalf("manifest for job = %v (%v)", man, err)
	}
	body, file, err := e.svc.ArtifactBlobForJob(ctx(), claims, "build", 1)
	if err != nil {
		t.Fatalf("blob for job: %v", err)
	}
	got, _ := io.ReadAll(body)
	_ = body.Close()
	if string(got) != "beta" || file.Path != "dist/sub/b.txt" {
		t.Fatalf("blob = %q (%s)", got, file.Path)
	}
	if _, _, err := e.svc.ArtifactBlobForJob(ctx(), claims, "build", 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unlisted blob index = %v", err)
	}
	// Another run's claims cannot read this artifact.
	other := e.artifactClaims(t, e.mustCreate(t)[0])
	if _, err := e.svc.ArtifactManifestForJob(ctx(), other, "build"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-run manifest = %v", err)
	}
}

func TestArtifactZipDownload(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)
	dl, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "build", e.reader())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var buf bytes.Buffer
	if err := WriteArtifactZip(ctx(), &buf, dl); err != nil {
		t.Fatalf("zip: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip reader: %v", err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, oerr := f.Open()
		if oerr != nil {
			t.Fatalf("zip entry %s: %v", f.Name, oerr)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		got[f.Name] = string(b)
	}
	if got["dist/a.txt"] != "alpha" || got["dist/sub/b.txt"] != "beta" || len(got) != 2 {
		t.Fatalf("zip entries = %v", got)
	}
}

func TestArtifactGCReclaimsBlobsAndList(t *testing.T) {
	e := newTestEnv(t)
	e.svc.Retention = 30 * 24 * time.Hour
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)
	short := e.finalizeArtifactNamed(t, claims, "short", 1)

	// Nothing expires yet.
	res := &GCResult{}
	if err := e.svc.expireArtifacts(ctx(), "o", "r", e.now, res); err != nil || res.ArtifactsDeleted != 0 {
		t.Fatalf("premature expiry = %+v (%v)", res, err)
	}

	// A short window really deletes the manifest and both blobs; the long one
	// stays.
	e.advance(2 * 24 * time.Hour)
	res = &GCResult{}
	if err := e.svc.expireArtifacts(ctx(), "o", "r", e.now, res); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if res.ArtifactsDeleted != 1 || res.ArtifactBytes == 0 {
		t.Fatalf("expired artifacts = %+v", res)
	}
	left, _ := e.svc.ListArtifacts(ctx(), "o/r", run.ID, e.reader())
	if len(left) != 1 || left[0].Name != "build" {
		t.Fatalf("artifacts after short expiry = %v", left)
	}

	// Deleting the run reclaims its remaining artifact: no manifest, no blob
	// under the run's artifact prefix.
	if _, err := e.svc.deleteRun(ctx(), "o", "r", run.ID); err != nil {
		t.Fatalf("deleteRun: %v", err)
	}
	var keys []string
	_ = e.svc.Store.List(ctx(), ArtifactPrefix("o", "r", run.ID), "", func(m store.ObjectMeta) error {
		keys = append(keys, m.Key)
		return nil
	})
	if len(keys) != 0 {
		t.Fatalf("blobs survived the run delete: %v", keys)
	}
	_ = short
	// And the expiry index drops stale rows on the next pass.
	res = &GCResult{}
	if err := e.svc.expireArtifacts(ctx(), "o", "r", e.now.Add(48*time.Hour), res); err != nil {
		t.Fatalf("second expire: %v", err)
	}
}

func TestArtifactPathAndNameValidation(t *testing.T) {
	bad := []string{"", "/etc/passwd", "../x", "a/../../x", "a\\b", "a\x00b", strings.Repeat("x", MaxArtifactPathLen+1)}
	for _, p := range bad {
		if _, err := cleanArtifactPath(p); !errors.Is(err, ErrInvalid) {
			t.Fatalf("cleanArtifactPath(%q) = %v", p, err)
		}
	}
	if got, err := cleanArtifactPath("dist/./a.txt"); err != nil || got != "dist/a.txt" {
		t.Fatalf("clean = %q (%v)", got, err)
	}
	for _, n := range []string{"", ".", "..", "-x", "a/b", "a b", strings.Repeat("x", MaxArtifactNameLen+1)} {
		if validArtifactName(n) {
			t.Fatalf("name %q accepted", n)
		}
	}
	for _, n := range []string{"build", "build-out", "Build_1.tar.gz"} {
		if !validArtifactName(n) {
			t.Fatalf("name %q rejected", n)
		}
	}
}

func TestArtifactExpiryKeysSortByTime(t *testing.T) {
	at := time.Unix(1_800_000_000, 0).UTC()
	later := at.Add(time.Hour)
	a := ArtifactExpiryKey("o", "r", "run", "a", at)
	b := ArtifactExpiryKey("o", "r", "run", "b", later)
	if !sort.StringsAreSorted([]string{a, b}) {
		t.Fatalf("expiry keys not time-ordered: %q, %q", a, b)
	}
}

func TestArtifactServiceErrorBranches(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)

	// Attempt identity gates every artifact write.
	stale := claims
	stale.Attempt = claims.Attempt + 1
	if _, err := e.svc.AddArtifactBlob(ctx(), stale, "build", 0, "a", 1, strings.NewReader("x")); !errors.Is(err, errSuperseded) {
		t.Fatalf("stale attempt blob = %v", err)
	}
	if _, err := e.svc.FinalizeArtifact(ctx(), stale, ArtifactFinalizeInput{Name: "build", Files: []ArtifactFile{{Path: "a", Size: 1, Index: 0}}}); !errors.Is(err, errSuperseded) {
		t.Fatalf("stale attempt finalize = %v", err)
	}
	// Input validation on the blob path.
	for _, tc := range []struct {
		name, artifact, path string
		index                int
		size                 int64
		want                 error
	}{
		{"bad name", "a/b", "a", 0, 1, ErrInvalid},
		{"bad index", "build", "a", -1, 1, ErrInvalid},
		{"index too large", "build", "a", MaxArtifactFiles, 1, ErrInvalid},
		{"negative size", "build", "a", 0, -1, ErrInvalid},
		{"bad path", "build", "../a", 0, 1, ErrInvalid},
	} {
		if _, err := e.svc.AddArtifactBlob(ctx(), claims, tc.artifact, tc.index, tc.path, tc.size, strings.NewReader("x")); !errors.Is(err, tc.want) {
			t.Fatalf("%s = %v, want %v", tc.name, err, tc.want)
		}
	}
	// The per-run budget is checked before the write.
	e.svc.MaxArtifactBytesPerRun = 3
	e.svc.MaxArtifactBytesPerFile = 3
	e.uploadFile(t, claims, "first", 0, "a", "abc")
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{Name: "first", Files: []ArtifactFile{{Path: "a", Size: 3, Index: 0}}}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := e.svc.AddArtifactBlob(ctx(), claims, "second", 0, "b", 1, strings.NewReader("x")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("run budget = %v", err)
	}
	// An idempotent retry of one blob key succeeds (Create + 412).
	e.svc.MaxArtifactBytesPerRun, e.svc.MaxArtifactBytesPerFile = 0, 0
	if _, err := e.svc.AddArtifactBlob(ctx(), claims, "retry", 0, "r", 2, strings.NewReader("ok")); err != nil {
		t.Fatalf("retry first = %v", err)
	}
	if _, err := e.svc.AddArtifactBlob(ctx(), claims, "retry", 0, "r", 2, strings.NewReader("ok")); err != nil {
		t.Fatalf("retry second = %v", err)
	}
	// A short body is refused by the store (a declared length the stream does
	// not satisfy) and never becomes a listed blob.
	short := &shortReader{r: strings.NewReader("ab"), remain: 5}
	if _, err := e.svc.AddArtifactBlob(ctx(), claims, "cut", 0, "c", 5, short); err == nil {
		t.Fatal("a short body was accepted")
	}
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{Name: "cut", Files: []ArtifactFile{{Path: "c", Size: 5, Index: 0}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("short-body finalize = %v", err)
	}
	// Finalize validation beyond the record table: too many files, a total over
	// the run budget, and an invalid retention value.
	many := make([]ArtifactFile, 0, MaxArtifactFiles+1)
	for i := 0; i <= MaxArtifactFiles; i++ {
		many = append(many, ArtifactFile{Path: fmt.Sprintf("f%d", i), Size: 0, Index: i})
	}
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{Name: "many", Files: many}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("many files = %v", err)
	}
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{Name: "huge", Files: []ArtifactFile{{Path: "a", Size: DefaultMaxArtifactBytesPerRun + 1, Index: 0}}}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("finalize total = %v", err)
	}
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{Name: "neg", RetentionDays: -1,
		Files: []ArtifactFile{{Path: "a", Size: 1, Index: 0}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative retention = %v", err)
	}
	// A run whose committed total fills the budget refuses a second artifact at
	// finalize even when its blobs fit individually (a fresh run, so the blob
	// pre-check sees only the fill artifact).
	e2 := newTestEnv(t)
	e2.svc.MaxArtifactBytesPerRun = 4
	e2.svc.MaxArtifactBytesPerFile = 4
	run2 := e2.mustCreate(t)[0]
	claims2 := e2.artifactClaims(t, run2)
	e2.uploadFile(t, claims2, "fill", 0, "a", "abcd")
	if _, err := e2.svc.FinalizeArtifact(ctx(), claims2, ArtifactFinalizeInput{Name: "fill", Files: []ArtifactFile{{Path: "a", Size: 4, Index: 0}}}); err != nil {
		t.Fatalf("fill: %v", err)
	}
	e2.svc.MaxArtifactBytesPerRun = 100
	e2.uploadFile(t, claims2, "more", 0, "b", "ab")
	e2.svc.MaxArtifactBytesPerRun = 4
	if _, err := e2.svc.FinalizeArtifact(ctx(), claims2, ArtifactFinalizeInput{Name: "more", Files: []ArtifactFile{{Path: "b", Size: 2, Index: 0}}}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("finalize run budget = %v", err)
	}
	// A blob object whose key does not parse is ignored by the verification LIST.
	if _, err := e.store.Put(ctx(), ArtifactBlobsPrefix("o", "r", run.ID, "odd", 0)+"garbage", store.PutBody{Bytes: []byte("x")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed junk blob: %v", err)
	}
	e.uploadFile(t, claims, "odd", 0, "a", "x")
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{Name: "odd", Files: []ArtifactFile{{Path: "a", Size: 1, Index: 0}}}); err != nil {
		t.Fatalf("junk blob key broke finalize: %v", err)
	}
	// Read-path guards.
	if _, err := e.svc.ListArtifacts(ctx(), "badrepo", run.ID, e.reader()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad repo = %v", err)
	}
	if _, err := e.svc.ListArtifacts(ctx(), "o/r", "nope", e.reader()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad run id = %v", err)
	}
	if _, err := e.svc.ListArtifacts(ctx(), "o/r", "0000000000000000001-abcdef", e.reader()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent run = %v", err)
	}
	// Unparseable timestamps never delete and never extend: the fallbacks are
	// exercised directly.
	if runExpiry(&Run{CreatedAt: "not a time"}, time.Hour).IsZero() {
		t.Fatal("runExpiry returned the zero time")
	}
	bad := &Artifact{CreatedAt: "not a time", RetentionDays: 1}
	if artifactExpired(bad, e.now) {
		t.Fatal("an unparseable created_at expired")
	}
	if artifactExpiresAt(run, bad, time.Hour, e.now).IsZero() {
		t.Fatal("artifactExpiresAt returned the zero time")
	}
	// humanBytes covers all four unit bands.
	for _, tc := range []struct {
		n    int64
		want string
	}{{0, "0 B"}, {2048, "2.0 KiB"}, {3 << 20, "3.0 MiB"}, {7 << 30, "7 GiB"}} {
		if got := humanBytes(tc.n); got != tc.want {
			t.Fatalf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// shortReader yields fewer bytes than its declared length.
type shortReader struct {
	r      io.Reader
	remain int64
}

func (s *shortReader) Read(p []byte) (int, error) {
	if s.remain <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > s.remain {
		p = p[:s.remain]
	}
	n, err := s.r.Read(p)
	s.remain -= int64(n)
	return n, err
}

func TestArtifactSmallBranchCoverage(t *testing.T) {
	// Caps: zero selects the defaults; a per-file value above the run value is
	// clamped down.
	e := newTestEnv(t)
	perRun, perFile := e.svc.artifactCaps()
	if perRun != DefaultMaxArtifactBytesPerRun || perFile != DefaultMaxArtifactBytesPerFile {
		t.Fatalf("default caps = %d/%d", perRun, perFile)
	}
	e.svc.MaxArtifactBytesPerRun, e.svc.MaxArtifactBytesPerFile = 10, 20
	if _, perFile := e.svc.artifactCaps(); perFile != 10 {
		t.Fatalf("per-file clamp = %d", perFile)
	}
	if caps := e.svc.ArtifactCaps(); caps.MaxBytesPerRun != 10 || caps.MaxBytesPerFile != 10 || caps.MaxFiles != MaxArtifactFiles {
		t.Fatalf("advertised caps = %+v", caps)
	}
	// Blob-index parsing rejects malformed tails.
	for _, key := range []string{"noslash", "x/y/abc", "x/000-12", "x/000-00001z"} {
		if _, ok := parseArtifactBlobIndex(key); ok {
			t.Fatalf("blob index parsed from %q", key)
		}
	}
	// A corrupt manifest surfaces as ErrCorrupt on a load and on the budget read.
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	if _, err := e.store.Put(ctx(), ArtifactManifestKey("o", "r", run.ID, "broken"), store.PutBody{Bytes: []byte("{")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.loadArtifact(ctx(), "o", "r", run.ID, "broken"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt manifest = %v", err)
	}
	if _, err := e.svc.AddArtifactBlob(ctx(), claims, "x", 0, "a", 1, strings.NewReader("x")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("budget read over a corrupt manifest = %v", err)
	}
	// A manifest run whose clock fields do not parse never expires and yields a
	// timestamp (the fail-safe direction: age is never a reason to delete).
	if got := runExpiry(&Run{CreatedAt: "2026-11-20T12:00:00Z", CompletedAt: ptrString("nope")}, time.Hour); got.IsZero() {
		t.Fatal("runExpiry fell back to zero")
	}
	if got := runExpiry(&Run{CreatedAt: "2026-11-20T12:00:00Z", CompletedAt: ptrString("2026-11-20T13:00:00Z")}, time.Hour); got.Hour() != 14 {
		t.Fatalf("runExpiry(completed) = %s", got)
	}
}

// ptrString is the test helper for an optional JSON timestamp.
func ptrString(s string) *string { return &s }

// artifactFaultStore injects the store failures the artifact paths must surface (never
// swallow): a Get error, a non-Object Get result, a Put error, a List error.
type artifactFaultStore struct {
	store.ObjectStore
	getErr       error
	getErrKey    string
	notObject    bool
	bodyErr      bool
	putErrKey    string
	listErr      error
	listErrKey   string
	deleteErrKey string
}

func (f *artifactFaultStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if f.getErr != nil && (f.getErrKey == "" || strings.Contains(key, f.getErrKey)) {
		return nil, f.getErr
	}
	if f.notObject && (f.getErrKey == "" || strings.Contains(key, f.getErrKey)) {
		return store.NotModified{}, nil
	}
	res, err := f.ObjectStore.Get(ctx, key, opts)
	if f.bodyErr && err == nil && (f.getErrKey == "" || strings.Contains(key, f.getErrKey)) {
		if obj, ok := res.(store.Object); ok {
			obj.Body = &errReader{inner: obj.Body}
			return obj, nil
		}
	}
	return res, err
}

func (f *artifactFaultStore) Delete(ctx context.Context, key string, ifVersion store.Version) error {
	if f.deleteErrKey != "" && strings.Contains(key, f.deleteErrKey) {
		return errors.New("store: delete failed")
	}
	return f.ObjectStore.Delete(ctx, key, ifVersion)
}

func (f *artifactFaultStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if f.putErrKey != "" && strings.Contains(key, f.putErrKey) {
		return store.ObjectMeta{}, errors.New("store: put failed")
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func (f *artifactFaultStore) List(ctx context.Context, prefix, after string, fn func(store.ObjectMeta) error) error {
	if f.listErr != nil && (f.listErrKey == "" || strings.Contains(prefix, f.listErrKey)) {
		return f.listErr
	}
	return f.ObjectStore.List(ctx, prefix, after, fn)
}

func TestArtifactStoreFailureBranches(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)
	dl, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "build", e.reader())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	base := e.store
	// A generic store failure while reading a blob is surfaced (not a 404).
	e.svc.Store = &artifactFaultStore{ObjectStore: base, getErrKey: "/blobs/", getErr: errors.New("store: get failed")}
	dl, err = e.svc.OpenArtifact(ctx(), "o/r", run.ID, "build", e.reader())
	if err != nil {
		t.Fatalf("reopen with fault store: %v", err)
	}
	if _, _, err := dl.Open(ctx(), dl.Artifact.Files[0]); err == nil || errors.Is(err, ErrCorrupt) {
		t.Fatalf("get failure = %v", err)
	}
	// A non-Object Get result is a corruption error.
	e.svc.Store = &artifactFaultStore{ObjectStore: base, getErrKey: "/blobs/", notObject: true}
	dl, _ = e.svc.OpenArtifact(ctx(), "o/r", run.ID, "build", e.reader())
	if dl == nil {
		t.Fatal("reopen with a non-object store")
	}
	if _, _, err := dl.Open(ctx(), dl.Artifact.Files[0]); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("non-object = %v", err)
	}
	// A manifest write failure is surfaced by finalize.
	e.svc.Store = &artifactFaultStore{ObjectStore: base, putErrKey: "manifest.json"}
	e.uploadFile(t, claims, "faulty", 0, "f.txt", "x")
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{Name: "faulty", Files: []ArtifactFile{{Path: "f.txt", Size: 1, Index: 0}}}); err == nil {
		t.Fatal("finalize swallowed a store failure")
	}
	// A LIST failure reaches the caller from both the artifact list and the
	// per-run delete (never a silent partial success).
	e.svc.Store = &artifactFaultStore{ObjectStore: base, listErr: errors.New("store: list failed")}
	if _, err := e.svc.ListArtifacts(ctx(), "o/r", run.ID, e.reader()); err == nil {
		t.Fatal("list swallowed a store failure")
	}
	if _, err := e.svc.deleteRun(ctx(), "o", "r", run.ID); err == nil {
		t.Fatal("delete swallowed a list failure")
	}
	e.svc.Store = base
}

func TestArtifactOpenGuards(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)
	// A malformed repository and a denied caller never reach a store read.
	if _, err := e.svc.OpenArtifact(ctx(), "badrepo", run.ID, "build", e.reader()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad repo = %v", err)
	}
	e.roles.public = false
	if _, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "build", auth.Anonymous()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anonymous open = %v", err)
	}
	// An unknown artifact name through the job channel is a named 404.
	if _, _, err := e.svc.ArtifactBlobForJob(ctx(), claims, "nope", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown artifact blob = %v", err)
	}
}

// errReader fails mid-body (the ZIP copy error path).
type errReader struct{ inner io.ReadCloser }

func (e *errReader) Read(p []byte) (int, error) { return 0, errors.New("network: reset") }
func (e *errReader) Close() error               { return e.inner.Close() }

func TestArtifactMoreErrorEdges(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)
	base := e.store
	// A run-header read failure is surfaced by the open path (never a 404).
	e.svc.Store = &artifactFaultStore{ObjectStore: base, getErrKey: "/runs/" + run.ID + ".json", getErr: errors.New("store: get failed")}
	if _, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "build", e.reader()); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("run read failure = %v", err)
	}
	e.svc.Store = base
	// A manifest read failure likewise.
	e.svc.Store = &artifactFaultStore{ObjectStore: base, getErrKey: "manifest.json", getErr: errors.New("store: get failed")}
	if _, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "build", e.reader()); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("manifest read failure = %v", err)
	}
	// The blob delete path surfaces a LIST failure.
	e.svc.Store = &artifactFaultStore{ObjectStore: base, listErr: errors.New("store: list failed")}
	if _, err := e.svc.deleteArtifact(ctx(), "o", "r", run.ID, "build"); err == nil {
		t.Fatal("delete swallowed a list failure")
	}
	e.svc.Store = base
	// A blob read failure through the job channel is surfaced.
	key := ArtifactBlobKey("o", "r", run.ID, "build", claims.Attempt, 0)
	if err := e.store.Delete(ctx(), key, ""); err != nil {
		t.Fatalf("delete blob: %v", err)
	}
	if _, _, err := e.svc.ArtifactBlobForJob(ctx(), claims, "build", 0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("missing blob via job = %v", err)
	}
	// A canceled expiry pass stops at the first due row.
	e.svc.Now = func() time.Time { return e.now.Add(48 * time.Hour) }
	claims2 := e.artifactClaims(t, e.mustCreate(t)[0])
	e.finalizeArtifactNamed(t, claims2, "short2", 1)
	canceled, cancel := contextWithCancel()
	cancel()
	_ = e.svc.expireArtifacts(canceled, "o", "r", e.now.Add(72*time.Hour), &GCResult{})
	// The expiry pass surfaces a LIST failure too.
	e.svc.Store = &artifactFaultStore{ObjectStore: base, listErr: errors.New("store: list failed")}
	if err := e.svc.expireArtifacts(ctx(), "o", "r", e.now, &GCResult{}); err == nil {
		t.Fatal("expiry swallowed a list failure")
	}
	// The reader list surfaces a run-header read failure.
	e.svc.Store = &artifactFaultStore{ObjectStore: base, getErrKey: "/runs/" + run.ID + ".json", getErr: errors.New("store: get failed")}
	if _, err := e.svc.ListArtifacts(ctx(), "o/r", run.ID, e.reader()); err == nil {
		t.Fatal("list swallowed a run read failure")
	}
	e.svc.Store = base
	// An unreadable expiry row is skipped, never a failed sweep.
	claims4 := e.artifactClaims(t, e.mustCreate(t)[0])
	e.finalizeArtifactNamed(t, claims4, "short4", 1)
	e.svc.Store = &artifactFaultStore{ObjectStore: base, getErrKey: "/artifact-expiry/", getErr: errors.New("store: get failed")}
	if err := e.svc.expireArtifacts(ctx(), "o", "r", e.now.Add(48*time.Hour), &GCResult{}); err != nil {
		t.Fatalf("unreadable expiry row failed the sweep: %v", err)
	}
	e.svc.Store = base
	// Restore the deleted blob, then break its body mid-stream.
	_ = restoreBlob(t, e, run.ID, 0, "alpha")
	// The ZIP copy fails when a blob body breaks mid-stream.
	e.svc.Store = &artifactFaultStore{ObjectStore: base, getErrKey: "/blobs/", bodyErr: true}
	dl, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "build", e.reader())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := WriteArtifactZip(ctx(), io.Discard, dl); err == nil {
		t.Fatal("zip swallowed a body failure")
	}
	e.svc.Store = base
	// A corrupt manifest makes finalize refuse (its budget read fails).
	run2 := e.mustCreate(t)[0]
	claims2 = e.artifactClaims(t, run2)
	if _, err = e.store.Put(ctx(), ArtifactManifestKey("o", "r", run2.ID, "broken"), store.PutBody{Bytes: []byte("{")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.FinalizeArtifact(ctx(), claims2, ArtifactFinalizeInput{Name: "x", Files: []ArtifactFile{{Path: "a", Size: 1, Index: 0}}}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("finalize over a corrupt manifest = %v", err)
	}
	// The finalize verification LIST failure is surfaced (a clean run).
	run3 := e.mustCreate(t)[0]
	claims3 := e.artifactClaims(t, run3)
	e.uploadFile(t, claims3, "x", 0, "a", "1")
	e.svc.Store = &artifactFaultStore{ObjectStore: base, listErr: errors.New("store: list failed")}
	if _, err := e.svc.FinalizeArtifact(ctx(), claims3, ArtifactFinalizeInput{Name: "x", Files: []ArtifactFile{{Path: "a", Size: 1, Index: 0}}}); err == nil {
		t.Fatal("finalize swallowed a list failure")
	}
	e.svc.Store = base
}

func TestArtifactDeleteAndTotalEdges(t *testing.T) {
	// A multi-file artifact whose SUM exceeds the per-run cap (each file fits).
	e := newTestEnv(t)
	e.svc.MaxArtifactBytesPerRun, e.svc.MaxArtifactBytesPerFile = 4, 4
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{Name: "sum", Files: []ArtifactFile{
		{Path: "a", Size: 3, Index: 0}, {Path: "b", Size: 3, Index: 1}}}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("sum cap = %v", err)
	}
	// Delete failures are surfaced from both the blob and manifest deletes.
	e2 := newTestEnv(t)
	run2 := e2.mustCreate(t)[0]
	claims2 := e2.artifactClaims(t, run2)
	e2.finalizeBuild(t, claims2, 0)
	base := e2.store
	e2.svc.Store = &artifactFaultStore{ObjectStore: base, deleteErrKey: "/blobs/"}
	if _, err := e2.svc.deleteArtifact(ctx(), "o", "r", run2.ID, "build"); err == nil {
		t.Fatal("blob delete failure swallowed")
	}
	e2.svc.Store = &artifactFaultStore{ObjectStore: base, deleteErrKey: "manifest.json"}
	if _, err := e2.svc.deleteArtifact(ctx(), "o", "r", run2.ID, "build"); err == nil {
		t.Fatal("manifest delete failure swallowed")
	}
	// An early-expiry reclaim whose blob deletion fails is surfaced.
	e2.svc.Store = base
	e2.svc.Now = func() time.Time { return e2.now.Add(48 * time.Hour) }
	e2.finalizeArtifactNamed(t, claims2, "short5", 1)
	e2.svc.Store = &artifactFaultStore{ObjectStore: base, listErr: errors.New("store: list failed"), listErrKey: "/artifacts/"}
	if err := e2.svc.expireArtifacts(ctx(), "o", "r", e2.now.Add(72*time.Hour), &GCResult{}); err == nil {
		t.Fatal("expiry swallowed a delete LIST failure")
	}
}

func TestHTTPArtifactListAuthError(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	h := errAuthHandler(e, auth.ErrUnauthorized)
	if rec := doAuth(h, http.MethodGet, "/o/r/api/threads/"+run.ID+"/artifacts", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("auth error list = %d", rec.Code)
	}
}
