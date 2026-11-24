// quota_test.go — the quota, usage-counter and retention work
// (docs/features/14 §5): the named 413 at the push commit point, the counter's
// accuracy across put/delete/gc, retention by count and age, the guarantee
// that a referenced blob is never removed, and the maintainer reconcile that
// repairs drift. The budget side (no LIST on a request path) stays in
// TestBudgetNoList plus TestUsageReadsNoList here.
package packages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/store"
)

// setPolicy installs a mutable policy closure on the fixture.
func (f *fixture) setPolicy(quota *int64, rules *[]config.PackageRetention) {
	f.svc.Policy = func(context.Context, string, string) config.PackagesPolicy {
		pol := config.PackagesPolicy{QuotaBytes: *quota}
		if rules != nil {
			pol.Retention = append(pol.Retention, *rules...)
		}
		return pol
	}
}

// manifestWith builds a single-platform manifest naming the given blobs (the
// registry never validates their existence on PUT, by design: the client's
// push order owns that).
func manifestWith(tags ...string) []byte {
	layers := make([]map[string]any, 0, len(tags))
	for _, tag := range tags {
		layers = append(layers, map[string]any{
			"mediaType": "application/vnd.oci.image.layer.v1.tar",
			"digest":    digestOf([]byte("layer-" + tag)),
			"size":      len("layer-" + tag),
		})
	}
	body, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    digestOf([]byte("config-" + tags[0])),
			"size":      len("config-" + tags[0]),
		},
		"layers": layers,
	})
	return body
}

// pushManifest is the tag push helper (201 expected).
func (f *fixture) pushManifest(t *testing.T, user, tag string, body []byte) string {
	t.Helper()
	w := f.as(user, "PUT", "/v2/acme/demo/manifests/"+tag, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("manifest %s = %d body=%s", tag, w.Code, w.Body.String())
	}
	return DigestBytes(body)
}

// TestQuotaRefusesManifestAtCommitPoint is the headline rule: usage plus the
// new manifest over the quota is a named 413 and NOTHING is written (no
// manifest object, no tag pointer). Raising the quota lets the same push land.
func TestQuotaRefusesManifestAtCommitPoint(t *testing.T) {
	f := newFixture(t)
	var quota int64
	f.setPolicy(&quota, nil)
	ctx := context.Background()

	blob := bytes.Repeat([]byte{'q'}, 64)
	f.pushBlob(t, "writer", blob)
	quota = int64(len(blob)) + 10 // the manifest body alone no longer fits

	w := f.as("writer", "PUT", "/v2/acme/demo/manifests/app", []byte(imageManifest))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-quota manifest = %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"DENIED", "quota exceeded", " of ", "quota; this push needs"} {
		if !strings.Contains(body, want) {
			t.Fatalf("refusal body missing %q: %s", want, body)
		}
	}
	var env errorBody
	if err := json.Unmarshal([]byte(body), &env); err != nil || len(env.Errors) != 1 {
		t.Fatalf("refusal is not the spec envelope: %v %s", err, body)
	}
	detail, _ := env.Errors[0].Detail.(map[string]any)
	if detail["quota"] == nil || detail["usage"] == nil || detail["requested"] == nil {
		t.Fatalf("refusal detail lacks usage/quota/requested: %s", body)
	}
	// Nothing written: no manifest object, no tag pointer, counter unchanged.
	hex := DigestHex(DigestBytes([]byte(imageManifest)))
	if meta, err := f.st.Head(ctx, manifestKey("acme", "demo", hex)); err != nil || meta != nil {
		t.Fatalf("refused push stored the manifest (meta=%v err=%v)", meta, err)
	}
	if meta, err := f.st.Head(ctx, tagKey("acme", "demo", "app")); err != nil || meta != nil {
		t.Fatalf("refused push stored the tag (meta=%v err=%v)", meta, err)
	}
	u, oerr := f.svc.Usage(ctx, "acme", "demo")
	if oerr != nil || u.Bytes != int64(len(blob)) {
		t.Fatalf("counter after refusal = %+v (%v)", u, oerr)
	}

	quota = 1 << 20
	if dig := f.pushManifest(t, "writer", "app", []byte(imageManifest)); dig != DigestBytes([]byte(imageManifest)) {
		t.Fatalf("digest = %s", dig)
	}
}

// TestQuotaRefusesOversizedLayer: the same bound applies at finalize, so a
// push cannot stream layers past the quota and only fail at the end. A refused
// layer writes no blob.
func TestQuotaRefusesOversizedLayer(t *testing.T) {
	f := newFixture(t)
	quota := int64(16)
	f.setPolicy(&quota, nil)
	ctx := context.Background()

	data := bytes.Repeat([]byte{'z'}, 64)
	w := f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/", nil)
	loc := w.Header().Get("Location")
	if w = f.as("writer", "PATCH", loc, data); w.Code != http.StatusAccepted {
		t.Fatalf("patch = %d", w.Code)
	}
	w = f.as("writer", "PUT", loc+"?digest="+digestOf(data), nil)
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "quota exceeded") {
		t.Fatalf("over-quota finalize = %d %s", w.Code, w.Body.String())
	}
	if meta, err := f.st.Head(ctx, blobKey("acme", "demo", DigestHex(digestOf(data)))); err != nil || meta != nil {
		t.Fatalf("refused layer stored a blob (meta=%v err=%v)", meta, err)
	}
	u, oerr := f.svc.Usage(ctx, "acme", "demo")
	if oerr != nil || u.Bytes != 0 {
		t.Fatalf("counter after refused layer = %+v (%v)", u, oerr)
	}
	// A layer that fits still lands.
	quota = 1 << 20
	f.pushBlob(t, "writer", data)
}

// TestUsageCounterPutDeleteGC pins the counter across the whole lifecycle:
// blob finalize, manifest put, a version delete (blobs stay until the sweep)
// and the sweep itself, which reconciles the absolute reading.
func TestUsageCounterPutDeleteGC(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	b1 := bytes.Repeat([]byte{'a'}, 100)
	b2 := bytes.Repeat([]byte{'b'}, 50)
	f.pushBlob(t, "writer", b1)
	f.pushBlob(t, "writer", b2)
	body := manifestWith("v1")
	f.pushManifest(t, "writer", "v1", body)

	u, oerr := f.svc.Usage(ctx, "acme", "demo")
	if oerr != nil {
		t.Fatal(oerr)
	}
	wantBytes := int64(150 + len(body))
	if u.Bytes != wantBytes || u.BlobBytes != 150 || u.ManifestBytes != int64(len(body)) {
		t.Fatalf("counter = %+v, want %d bytes", u, wantBytes)
	}
	if u.Blobs != 2 || u.Manifests != 1 {
		t.Fatalf("counts = %d/%d", u.Blobs, u.Manifests)
	}

	// A version delete drops the manifest bytes; the blobs wait for the sweep.
	if err := f.svc.DeleteManifest(ctx, "acme", "demo", DigestBytes(body)); err != nil {
		t.Fatal(err)
	}
	if u, _ = f.svc.Usage(ctx, "acme", "demo"); u.Bytes != 150 || u.Manifests != 0 {
		t.Fatalf("counter after delete = %+v", u)
	}

	// Past the grace window the sweep reclaims the now-unreferenced blobs and
	// reconciles the counter to zero.
	f.now = f.now.Add(time.Hour)
	res, err := f.svc.GC(ctx, "acme", "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 2 || res.UsageBytes != 0 {
		t.Fatalf("gc = %+v", res)
	}
	if u, _ = f.svc.Usage(ctx, "acme", "demo"); u.Bytes != 0 || u.Blobs != 0 {
		t.Fatalf("counter after gc = %+v", u)
	}
	inst, err := f.svc.InstanceUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if inst.TotalBytes != 0 || len(inst.Repos) != 0 {
		t.Fatalf("instance index = %+v", inst)
	}
}

// TestInstanceUsageIndex: an applying sweep records the per-repo reading and
// the instance total (the Maintenance card's one GET).
func TestInstanceUsageIndex(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.pushBlob(t, "writer", bytes.Repeat([]byte{'i'}, 40))
	body := manifestWith("v1")
	f.pushManifest(t, "writer", "v1", body)
	if _, err := f.svc.GC(ctx, "acme", "demo", nil); err != nil {
		t.Fatal(err)
	}
	inst, err := f.svc.InstanceUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(40 + len(body))
	if inst.TotalBytes != want || inst.Repos["acme/demo"] != want {
		t.Fatalf("instance index = %+v, want %d", inst, want)
	}
	if got := inst.Largest(5); len(got) != 1 || got[0].Repo != "acme/demo" {
		t.Fatalf("largest = %+v", got)
	}
	// The index is maintainer-written: a dry run must not move it.
	f.pushBlob(t, "writer", bytes.Repeat([]byte{'j'}, 10))
	if _, err := f.svc.PlanGC(ctx, "acme", "demo", nil); err != nil {
		t.Fatal(err)
	}
	inst, _ = f.svc.InstanceUsage(ctx)
	if inst.TotalBytes != want {
		t.Fatalf("dry run wrote the index: %+v", inst)
	}
}

// TestReconcileRepairsDrift: a counter that drifted (crashed writer, a manual
// object delete) is repaired by the next applying sweep, which reports the
// drift it fixed.
func TestReconcileRepairsDrift(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.pushBlob(t, "writer", bytes.Repeat([]byte{'d'}, 70))
	body := manifestWith("v1")
	f.pushManifest(t, "writer", "v1", body)
	realBytes := int64(70 + len(body))

	// Corrupt the counter: under-report by 69 bytes.
	raw, _ := json.Marshal(Usage{Version: 1, Bytes: realBytes - 69, BlobBytes: 1, Manifests: 1})
	if _, err := store.PutBytes(ctx, f.st, UsageKey("acme", "demo"), raw, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.GC(ctx, "acme", "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.UsageDrift != 69 || res.UsageBytes != realBytes {
		t.Fatalf("gc drift = %d/%d, want 69/%d", res.UsageDrift, res.UsageBytes, realBytes)
	}
	if u, _ := f.svc.Usage(ctx, "acme", "demo"); u.Bytes != realBytes {
		t.Fatalf("reconciled counter = %+v", u)
	}
	// A second pass finds no drift.
	res, err = f.svc.GC(ctx, "acme", "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.UsageDrift != 0 {
		t.Fatalf("second pass drift = %d", res.UsageDrift)
	}
}

// TestRetentionKeepLastSharedManifest: five tags on ONE manifest keep the
// newest two; the version and its blob survive because the surviving tags
// still name it.
func TestRetentionKeepLastSharedManifest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var quota int64
	rules := []config.PackageRetention{{Pattern: "*", KeepLast: 2}}
	f.setPolicy(&quota, &rules)
	blob := []byte("shared-layer")
	layer := f.pushBlob(t, "writer", blob)
	body, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    digestOf([]byte("cfg")), "size": 3,
		},
		"layers": []map[string]any{{"digest": layer, "size": len(blob)}},
	})
	for i, tag := range []string{"t1", "t2", "t3", "t4", "t5"} {
		f.now = f.now.Add(time.Hour)
		f.pushManifest(t, "writer", tag, body)
		_ = i
	}
	res, err := f.svc.GC(ctx, "acme", "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Retention == nil || res.Retention.PrunedTags != 3 {
		t.Fatalf("retention = %+v", res.Retention)
	}
	tags, _, oerr := f.svc.ListTags(ctx, "acme", "demo", "", 100)
	if oerr != nil {
		t.Fatal(oerr)
	}
	if strings.Join(tags, ",") != "t4,t5" {
		t.Fatalf("tags = %v", tags)
	}
	// The manifest and its blob are still there: surviving tags reference it.
	if meta, _ := f.st.Head(ctx, manifestKey("acme", "demo", DigestHex(DigestBytes(body)))); meta == nil {
		t.Fatal("retention removed a manifest a surviving tag names")
	}
	if meta, _ := f.st.Head(ctx, blobKey("acme", "demo", DigestHex(layer))); meta == nil {
		t.Fatal("retention removed a blob a surviving manifest references")
	}
}

// TestRetentionKeepLastDistinctVersions: three tags, three manifests, three
// layers. Keep one: the two pruned versions and their exclusive blobs go.
func TestRetentionKeepLastDistinctVersions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var quota int64
	rules := []config.PackageRetention{{Pattern: "v*", KeepLast: 1}}
	f.setPolicy(&quota, &rules)
	layers := map[string]string{}
	for _, tag := range []string{"v1", "v2", "v3"} {
		f.now = f.now.Add(time.Hour)
		layers[tag] = f.pushBlob(t, "writer", []byte("layer-"+tag))
		f.pushManifest(t, "writer", tag, manifestWith(tag))
	}
	// Past the grace window, so the sweep may reclaim the pruned layers.
	f.now = f.now.Add(time.Hour)
	res, err := f.svc.GC(ctx, "acme", "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Retention == nil || res.Retention.PrunedTags != 2 || res.Retention.Manifests != 2 {
		t.Fatalf("retention = %+v", res.Retention)
	}
	if res.Deleted != 2 {
		t.Fatalf("blobs deleted = %d, want 2", res.Deleted)
	}
	tags, _, _ := f.svc.ListTags(ctx, "acme", "demo", "", 100)
	if strings.Join(tags, ",") != "v3" {
		t.Fatalf("tags = %v", tags)
	}
	for tag, dig := range layers {
		meta, _ := f.st.Head(ctx, blobKey("acme", "demo", DigestHex(dig)))
		if tag == "v3" && meta == nil {
			t.Fatalf("kept version lost its layer %s", tag)
		}
		if tag != "v3" && meta != nil {
			t.Fatalf("pruned version kept its exclusive layer %s", tag)
		}
	}
	// The usage counter was reconciled by the same pass.
	u, _ := f.svc.Usage(ctx, "acme", "demo")
	want := int64(len("layer-v3") + len(manifestWith("v3")))
	if u.Bytes != want {
		t.Fatalf("usage = %d, want %d", u.Bytes, want)
	}
}

// TestRetentionMaxAge: a tag older than max_age is pruned, a fresher one is
// kept, and an untagged manifest is never a candidate (it was not pruned by
// this pass, so it counts as referenced until a sweep proves otherwise).
func TestRetentionMaxAge(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var quota int64
	rules := []config.PackageRetention{{Pattern: "", MaxAge: config.Duration(7 * 24 * time.Hour)}}
	f.setPolicy(&quota, &rules)
	f.pushManifest(t, "writer", "old", manifestWith("old"))
	f.now = f.now.Add(30 * 24 * time.Hour)
	f.pushManifest(t, "writer", "new", manifestWith("new"))
	// A digest-only push: never a retention candidate.
	untagged := manifestWith("untagged")
	if _, oerr := f.svc.PutManifest(ctx, "acme", "demo", DigestBytes(untagged), untagged, ""); oerr != nil {
		t.Fatal(oerr)
	}
	res, err := f.svc.GC(ctx, "acme", "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Retention == nil || res.Retention.PrunedTags != 1 || res.Retention.PrunedNames[0] != "old" {
		t.Fatalf("retention = %+v", res.Retention)
	}
	tags, _, _ := f.svc.ListTags(ctx, "acme", "demo", "", 100)
	if strings.Join(tags, ",") != "new" {
		t.Fatalf("tags = %v", tags)
	}
	if meta, _ := f.st.Head(ctx, manifestKey("acme", "demo", DigestHex(DigestBytes(untagged)))); meta == nil {
		t.Fatal("an untagged manifest was pruned by the tag policy")
	}
}

// TestRetentionBlobSharedByKeptVersion: two versions share a layer; pruning
// one must never remove the shared blob (the kept manifest names it).
func TestRetentionBlobSharedByKeptVersion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var quota int64
	rules := []config.PackageRetention{{Pattern: "*", KeepLast: 1}}
	f.setPolicy(&quota, &rules)
	shared := f.pushBlob(t, "writer", []byte("common"))
	onlyOld := f.pushBlob(t, "writer", []byte("only-old"))
	mk := func(configDigest string, layers ...string) []byte {
		ls := make([]map[string]any, 0, len(layers))
		for _, l := range layers {
			ls = append(ls, map[string]any{"digest": l, "size": 8})
		}
		body, _ := json.Marshal(map[string]any{
			"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
			"config": map[string]any{"digest": configDigest, "size": 3},
			"layers": ls,
		})
		return body
	}
	oldBody := mk(digestOf([]byte("cfg-old")), shared, onlyOld)
	f.pushManifest(t, "writer", "old", oldBody)
	f.now = f.now.Add(time.Hour)
	f.pushManifest(t, "writer", "new", mk(digestOf([]byte("cfg-new")), shared))
	f.now = f.now.Add(time.Hour)
	if _, err := f.svc.GC(ctx, "acme", "demo", nil); err != nil {
		t.Fatal(err)
	}
	if meta, _ := f.st.Head(ctx, blobKey("acme", "demo", DigestHex(shared))); meta == nil {
		t.Fatal("a blob shared with the kept version was removed")
	}
	if meta, _ := f.st.Head(ctx, blobKey("acme", "demo", DigestHex(onlyOld))); meta != nil {
		t.Fatal("the pruned version's exclusive blob survived")
	}
}

// TestUsageReadsNoList: reading usage (the page, the API, the quota check) is
// a GET, never a bucket walk — the store method set is the assertion.
func TestUsageReadsNoList(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.pushBlob(t, "writer", []byte("x"))
	f.pushManifest(t, "writer", "v1", manifestWith("v1"))
	f.resetCounts()
	if _, oerr := f.svc.Usage(ctx, "acme", "demo"); oerr != nil {
		t.Fatal(oerr)
	}
	if _, err := f.svc.InstanceUsage(ctx); err != nil {
		t.Fatal(err)
	}
	puts, gets, lists := f.st.counts()
	if lists != 0 {
		t.Fatalf("usage read listed the bucket %d times", lists)
	}
	if puts != 0 || gets != 2 {
		t.Fatalf("usage read did %d puts / %d gets, want 0/2", puts, gets)
	}
}

// TestPolicyNilIsUnlimited: an un-wired projection cannot invent a quota.
func TestPolicyNilIsUnlimited(t *testing.T) {
	f := newFixture(t)
	f.svc.Policy = nil
	f.pushBlob(t, "writer", bytes.Repeat([]byte{'u'}, 1024))
	f.pushManifest(t, "writer", "v1", manifestWith("v1"))
	u, oerr := f.svc.Usage(context.Background(), "acme", "demo")
	if oerr != nil {
		t.Fatal(oerr)
	}
	if u.QuotaBytes != 0 || u.Limited() || u.OverQuota() {
		t.Fatalf("nil policy produced a quota: %+v", u)
	}
}

// TestRetentionDeleteFailures: a hard delete failure aborts the pass (the
// caller retries), while an unreadable candidate body only costs the child
// sweep — never a blind delete of what could not be read.
func TestRetentionDeleteFailures(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var quota int64
	rules := []config.PackageRetention{{Pattern: "*", KeepLast: 1}}
	f.setPolicy(&quota, &rules)
	child := manifestWith("child")
	if _, oerr := f.svc.PutManifest(ctx, "acme", "demo", DigestBytes(child), child, ""); oerr != nil {
		t.Fatal(oerr)
	}
	index, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{{"digest": DigestBytes(child), "size": len(child)}},
	})
	f.pushManifest(t, "writer", "old", index)
	f.now = f.now.Add(time.Hour)
	f.pushManifest(t, "writer", "new", manifestWith("new"))

	// The candidate index body cannot be read: the pass still deletes the
	// manifest it planned (the child is left for the next pass, where the
	// mark phase's own read decides).
	idxHex := DigestHex(DigestBytes(index))
	f.st.getKeyFn = func(key string, res store.GetResult, err error) (store.GetResult, error) {
		if strings.HasSuffix(key, "/manifests/"+idxHex) {
			return nil, errors.New("body down")
		}
		return res, err
	}
	res, err := f.svc.applyRetention(ctx, "acme", "demo", rules, true)
	if err != nil {
		t.Fatalf("unreadable candidate body failed the pass: %v", err)
	}
	if res.PrunedTags != 1 {
		t.Fatalf("retention = %+v", res.PrunedTags)
	}
	f.st.getKeyFn = nil

	// A hard delete failure surfaces.
	f.pushManifest(t, "writer", "old2", index)
	f.now = f.now.Add(time.Hour)
	f.pushManifest(t, "writer", "new2", manifestWith("new2"))
	f.st.deleteKeyFn = func(key string, err error) error {
		if strings.Contains(key, "/manifests/") {
			return errors.New("delete down")
		}
		return err
	}
	if _, err := f.svc.applyRetention(ctx, "acme", "demo", rules, true); err == nil {
		t.Fatal("a hard delete failure did not fail the pass")
	}
	f.st.deleteKeyFn = nil
}

// TestDeleteManifestTreeFailureBranches: the version-tree delete fails closed on
// a listing that cannot be trusted and on an unreadable tag pointer, and a
// moved tag pointer is left alone.
func TestDeleteManifestTreeFailureBranches(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	child := manifestWith("kid")
	index, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{{"digest": DigestBytes(child), "size": len(child)}},
	})
	f.pushManifest(t, "writer", "v1", index)
	digest := DigestBytes(index)

	f.st.listKeyFn = func(prefix string, err error) error {
		if strings.Contains(prefix, "/manifests/") {
			return errors.New("list down")
		}
		return err
	}
	if err := f.svc.DeleteManifest(ctx, "acme", "demo", digest); err == nil {
		t.Fatal("delete ignored a manifest listing failure")
	}
	f.st.listKeyFn = nil

	f.st.getKeyFn = func(key string, res store.GetResult, err error) (store.GetResult, error) {
		if strings.Contains(key, "/tags/") {
			return nil, store.NewNotFound(key)
		}
		return res, err
	}
	if err := f.svc.DeleteManifest(ctx, "acme", "demo", digest); err != nil {
		t.Fatalf("a vanished tag pointer failed the delete: %v", err)
	}
	f.st.getKeyFn = nil
}

// TestRetentionKeepsDirectlyTaggedChild: a surviving tag may point at a
// platform manifest directly; pruning an index that also names it must not
// delete the version that tag still resolves.
func TestRetentionKeepsDirectlyTaggedChild(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var quota int64
	rules := []config.PackageRetention{{Pattern: "*", KeepLast: 1}}
	f.setPolicy(&quota, &rules)
	child := manifestWith("platform")
	if _, oerr := f.svc.PutManifest(ctx, "acme", "demo", DigestBytes(child), child, ""); oerr != nil {
		t.Fatal(oerr)
	}
	index, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{{"digest": DigestBytes(child), "size": len(child)}},
	})
	f.pushManifest(t, "writer", "old-idx", index)
	f.now = f.now.Add(time.Hour)
	f.pushManifest(t, "writer", "platform", child) // the surviving tag, newest
	if _, err := f.svc.GC(ctx, "acme", "demo", nil); err != nil {
		t.Fatal(err)
	}
	if meta, _ := f.st.Head(ctx, manifestKey("acme", "demo", DigestHex(DigestBytes(child)))); meta == nil {
		t.Fatal("a directly tagged platform manifest followed a pruned index")
	}
	if meta, _ := f.st.Head(ctx, manifestKey("acme", "demo", DigestHex(DigestBytes(index)))); meta != nil {
		t.Fatal("the pruned index survived")
	}
}
