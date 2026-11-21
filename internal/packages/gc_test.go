package packages

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// imageWith builds an OCI image manifest naming the given blob digests as
// config + layers.
func imageWith(config string, layers ...string) []byte {
	var b strings.Builder
	b.WriteString(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"`)
	b.WriteString(config)
	b.WriteString(`","size":2},"layers":[`)
	for i, l := range layers {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"`)
		b.WriteString(l)
		b.WriteString(`","size":3}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// blobExists reports whether the registry still stores a blob.
func blobExists(t *testing.T, f *fixture, digest string) bool {
	t.Helper()
	meta, err := f.st.Head(context.Background(), blobKey("acme", "demo", DigestHex(digest)))
	if err != nil {
		t.Fatalf("head blob: %v", err)
	}
	return meta != nil
}

type gcLogger struct {
	notices  []string
	progress int
}

func (l *gcLogger) Notice(text string)                      { l.notices = append(l.notices, text) }
func (l *gcLogger) Progress(string, uint64, uint64, string) { l.progress++ }

// TestGCReclaimsDeletedImage is the audit's storage finding: once every tag and
// the manifest digest are deleted, the sweep removes the orphaned blobs and a
// second sweep is a no-op.
func TestGCReclaimsDeletedImage(t *testing.T) {
	f := newFixture(t)
	f.svc.BlobGrace = time.Minute
	config := f.pushBlob(t, "writer", []byte("config"))
	layer := f.pushBlob(t, "writer", []byte("layer!"))
	manifest := imageWith(config, layer)
	digest := digestOf(manifest)
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/app", manifest); w.Code != 201 {
		t.Fatalf("put = %d %s", w.Code, w.Body.String())
	}
	f.now = f.now.Add(time.Hour) // past the blob grace window

	// While the manifest exists, nothing is reclaimable.
	log := &gcLogger{}
	res, err := f.svc.GC(context.Background(), "acme", "demo", log)
	if err != nil {
		t.Fatalf("gc = %v", err)
	}
	if res.Manifests != 1 || res.Deleted != 0 || res.Kept != 2 {
		t.Fatalf("live gc = %+v", res)
	}
	if len(log.notices) == 0 || log.progress == 0 {
		t.Fatalf("gc narrated nothing: %+v", log)
	}

	// Delete the tag and the manifest, then sweep: both blobs are garbage.
	if w := f.as("writer", "DELETE", "/v2/acme/demo/manifests/app", nil); w.Code != http.StatusAccepted {
		t.Fatalf("delete tag = %d", w.Code)
	}
	if w := f.as("writer", "DELETE", "/v2/acme/demo/manifests/"+digest, nil); w.Code != http.StatusAccepted {
		t.Fatalf("delete digest = %d", w.Code)
	}
	res, err = f.svc.GC(context.Background(), "acme", "demo", nil)
	if err != nil {
		t.Fatalf("gc = %v", err)
	}
	if res.Manifests != 0 || res.Deleted != 2 || res.Kept != 0 || res.BytesFreed == 0 {
		t.Fatalf("sweep = %+v", res)
	}
	if blobExists(t, f, config) || blobExists(t, f, layer) {
		t.Fatal("orphaned blobs survived the sweep")
	}

	// Idempotent: a second pass deletes nothing.
	res, err = f.svc.GC(context.Background(), "acme", "demo", nil)
	if err != nil {
		t.Fatalf("gc2 = %v", err)
	}
	if res.Deleted != 0 || res.Blobs != 0 {
		t.Fatalf("second sweep not a no-op: %+v", res)
	}
}

// TestGCSharedBlobSurvives pins the mark phase: a blob named by two manifests
// stays while one of them is deleted.
func TestGCSharedBlobSurvives(t *testing.T) {
	f := newFixture(t)
	f.svc.BlobGrace = time.Minute
	config := f.pushBlob(t, "writer", []byte("config"))
	shared := f.pushBlob(t, "writer", []byte("shared-layer"))
	one := imageWith(config, shared)
	two := imageWith(config, shared, shared)
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/one", one); w.Code != 201 {
		t.Fatalf("put one = %d", w.Code)
	}
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/two", two); w.Code != 201 {
		t.Fatalf("put two = %d", w.Code)
	}
	f.now = f.now.Add(time.Hour)

	// Delete the first version only.
	if w := f.as("writer", "DELETE", "/v2/acme/demo/manifests/"+digestOf(one), nil); w.Code != 202 {
		t.Fatalf("delete one = %d", w.Code)
	}
	res, err := f.svc.GC(context.Background(), "acme", "demo", nil)
	if err != nil {
		t.Fatalf("gc = %v", err)
	}
	if res.Deleted != 0 || res.Kept != 2 || !blobExists(t, f, shared) {
		t.Fatalf("shared blob swept: %+v", res)
	}
	// Deleting the second version frees both.
	if w := f.as("writer", "DELETE", "/v2/acme/demo/manifests/"+digestOf(two), nil); w.Code != 202 {
		t.Fatalf("delete two = %d", w.Code)
	}
	res, err = f.svc.GC(context.Background(), "acme", "demo", nil)
	if err != nil {
		t.Fatalf("gc2 = %v", err)
	}
	if res.Deleted != 2 || blobExists(t, f, shared) {
		t.Fatalf("after second delete = %+v", res)
	}
}

// TestGCUploadsAndTombstones covers the staging sweep: expired uploads and
// tombstones go, fresh ones stay, and an unparseable id is garbage.
func TestGCUploadsAndTombstones(t *testing.T) {
	f := newFixture(t)
	f.svc.BlobGrace = time.Minute
	start := func() string {
		w := f.as("writer", "POST", "/v2/acme/demo/blobs/uploads/", nil)
		if w.Code != http.StatusAccepted {
			t.Fatalf("start = %d", w.Code)
		}
		return w.Header().Get("Location")[strings.LastIndex(w.Header().Get("Location"), "/")+1:]
	}
	old := start()
	if w := f.as("writer", "PATCH", "/v2/acme/demo/blobs/uploads/"+old, []byte("stale")); w.Code != 202 {
		t.Fatalf("patch = %d", w.Code)
	}
	// An aborted upload leaves a tombstone.
	gone := start()
	if w := f.as("writer", "DELETE", "/v2/acme/demo/blobs/uploads/"+gone, nil); w.Code != 204 {
		t.Fatalf("abort = %d", w.Code)
	}
	// An upload id that cannot even be parsed is garbage.
	if _, err := f.st.Put(context.Background(), uploadKey("acme", "demo", "bogus"), store.PutBody{Bytes: []byte("x")}, store.PutOptions{}); err != nil {
		t.Fatalf("seed bogus: %v", err)
	}

	f.now = f.now.Add(48 * time.Hour)
	// A fresh upload created after the clock move must survive.
	fresh := start()
	if w := f.as("writer", "PATCH", "/v2/acme/demo/blobs/uploads/"+fresh, []byte("live")); w.Code != 202 {
		t.Fatalf("patch fresh = %d", w.Code)
	}

	res, err := f.svc.GC(context.Background(), "acme", "demo", nil)
	if err != nil {
		t.Fatalf("gc = %v", err)
	}
	if res.Uploads != 2 || res.Tombstones != 1 {
		t.Fatalf("staging sweep = %+v", res)
	}
	for _, key := range []string{uploadKey("acme", "demo", old), abortedKey("acme", "demo", gone), uploadKey("acme", "demo", "bogus")} {
		meta, herr := f.st.Head(context.Background(), key)
		if herr != nil || meta != nil {
			t.Fatalf("%s survived the sweep (%v)", key, herr)
		}
	}
	if meta, herr := f.st.Head(context.Background(), uploadKey("acme", "demo", fresh)); herr != nil || meta == nil {
		t.Fatalf("fresh upload swept (%v)", herr)
	}
}

// TestGCBlobGraceKeepsFreshBlobs: a blob finalized since the last pass is not
// garbage yet — its manifest PUT is still in flight.
func TestGCBlobGraceKeepsFreshBlobs(t *testing.T) {
	f := newFixture(t)
	f.svc.BlobGrace = time.Hour
	fresh := f.pushBlob(t, "writer", []byte("in-flight"))
	res, err := f.svc.GC(context.Background(), "acme", "demo", nil)
	if err != nil {
		t.Fatalf("gc = %v", err)
	}
	if res.Deleted != 0 || !blobExists(t, f, fresh) {
		t.Fatalf("fresh blob swept: %+v", res)
	}
	f.now = f.now.Add(2 * time.Hour)
	res, err = f.svc.GC(context.Background(), "acme", "demo", nil)
	if err != nil {
		t.Fatalf("gc2 = %v", err)
	}
	if res.Deleted != 1 || blobExists(t, f, fresh) {
		t.Fatalf("stale blob survived: %+v", res)
	}
}

// TestGCIndexSubjectAndMalformed covers the remaining mark inputs: an index
// child is not a blob, a subject/blob descriptor is, and an unparseable body
// marks nothing.
func TestGCIndexSubjectAndMalformed(t *testing.T) {
	f := newFixture(t)
	f.svc.BlobGrace = time.Minute
	subject := f.pushBlob(t, "writer", []byte("subject"))
	artifact := f.pushBlob(t, "writer", []byte("artifact"))
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.artifact.manifest.v1+json","blobs":[{"digest":"` + artifact + `","size":8}],"subject":{"digest":"` + subject + `","size":7}}`)
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/art", body); w.Code != 201 {
		t.Fatalf("put artifact = %d", w.Code)
	}
	if _, err := f.st.Put(context.Background(), manifestKey("acme", "demo", DigestHex(digestOf([]byte("junk")))), store.PutBody{Bytes: []byte("junk")}, store.PutOptions{}); err != nil {
		t.Fatalf("seed junk: %v", err)
	}
	f.now = f.now.Add(time.Hour)
	res, err := f.svc.GC(context.Background(), "acme", "demo", nil)
	if err != nil {
		t.Fatalf("gc = %v", err)
	}
	if res.Manifests != 2 || res.Deleted != 0 {
		t.Fatalf("subject/blob descriptors not marked: %+v", res)
	}
}

// TestGCSweepDueAndErrors pins the maintainer due gate and the failure paths.
func TestGCSweepDueAndErrors(t *testing.T) {
	f := newFixture(t)
	f.svc.GCInterval = 10 * time.Minute
	if !f.svc.SweepDue("acme", "demo") {
		t.Fatal("a never-swept repo must be due")
	}
	if _, err := f.svc.GC(context.Background(), "acme", "demo", nil); err != nil {
		t.Fatalf("gc = %v", err)
	}
	if f.svc.SweepDue("acme", "demo") {
		t.Fatal("repo immediately due again")
	}
	f.now = f.now.Add(11 * time.Minute)
	if !f.svc.SweepDue("acme", "demo") {
		t.Fatal("repo not due after the interval")
	}

	f.pushBlob(t, "writer", []byte("x"))
	boom := errors.New("boom")
	f.st.listFn = func(error) error { return boom }
	if _, err := f.svc.GC(context.Background(), "acme", "demo", nil); err == nil {
		t.Fatal("list failure swallowed")
	}
	f.st.listFn = nil
	// A manifest exists in the mark phase only if we seed one.
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/m", []byte(imageManifest)); w.Code != 201 {
		t.Fatalf("put = %d", w.Code)
	}
	f.st.getFn = func(store.GetResult, error) (store.GetResult, error) { return nil, boom }
	if _, err := f.svc.GC(context.Background(), "acme", "demo", nil); err == nil {
		t.Fatal("manifest read failure swallowed")
	}
	f.st.getFn = nil
	f.now = f.now.Add(time.Hour) // past the grace window: the orphan is a candidate
	f.st.deleteFn = func(error) error { return boom }
	if _, err := f.svc.GC(context.Background(), "acme", "demo", nil); err == nil {
		t.Fatal("blob delete failure swallowed")
	}
	f.st.deleteFn = nil
	if _, err := f.svc.GC(context.Background(), "bad owner", "demo", nil); err == nil {
		t.Fatal("invalid repo accepted")
	}
}

// TestGCDeterministicSummary keeps the notice text stable (operators grep it).
func TestGCDeterministicSummary(t *testing.T) {
	r := GCResult{Manifests: 2, Deleted: 3, BytesFreed: 42, Uploads: 1, Tombstones: 4}
	want := "scanned 2 manifests, deleted 3 unreferenced blob(s) (42 bytes), swept 1 upload(s) and 4 tombstone(s)"
	if got := r.summary(); got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

// TestDeleteIndexVersionOwnsItsChildren: deleting an index by digest takes its
// platform manifests with it (GHCR's version semantics), so the blobs become
// reclaimable; a child another index still references survives.
func TestDeleteIndexVersionOwnsItsChildren(t *testing.T) {
	f := newFixture(t)
	f.svc.BlobGrace = time.Minute
	config := f.pushBlob(t, "writer", []byte("cfg"))
	layer := f.pushBlob(t, "writer", []byte("layer"))
	child := imageWith(config, layer)
	childDigest := DigestBytes(child)
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/"+childDigest, child); w.Code != 201 {
		t.Fatalf("put child = %d %s", w.Code, w.Body.String())
	}
	index := func(media string) []byte {
		return []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"` + media + `","digest":"` + childDigest + `","size":` + "1" + `}]}`)
	}
	one := index("application/vnd.oci.image.manifest.v1+json")
	two := index("application/octet-stream")
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/multi", one); w.Code != 201 {
		t.Fatalf("put index = %d", w.Code)
	}
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/other", two); w.Code != 201 {
		t.Fatalf("put other = %d", w.Code)
	}

	// Delete the first version: the shared child and its blobs survive.
	if w := f.as("writer", "DELETE", "/v2/acme/demo/manifests/"+DigestBytes(one), nil); w.Code != 202 {
		t.Fatalf("delete index = %d", w.Code)
	}
	if w := f.as("reader", "GET", "/v2/acme/demo/manifests/"+childDigest, nil); w.Code != 200 {
		t.Fatalf("shared child deleted = %d", w.Code)
	}
	f.now = f.now.Add(time.Hour)
	res, err := f.svc.GC(context.Background(), "acme", "demo", nil)
	if err != nil {
		t.Fatalf("gc = %v", err)
	}
	if res.Deleted != 0 {
		t.Fatalf("shared blobs swept: %+v", res)
	}

	// Delete the last version: the child goes, and the sweep reclaims it all.
	if w := f.as("writer", "DELETE", "/v2/acme/demo/manifests/"+DigestBytes(two), nil); w.Code != 202 {
		t.Fatalf("delete other = %d", w.Code)
	}
	if w := f.as("reader", "GET", "/v2/acme/demo/manifests/"+childDigest, nil); w.Code != 404 {
		t.Fatalf("orphaned child survived = %d", w.Code)
	}
	res, err = f.svc.GC(context.Background(), "acme", "demo", nil)
	if err != nil {
		t.Fatalf("gc2 = %v", err)
	}
	if res.Deleted != 2 || blobExists(t, f, config) || blobExists(t, f, layer) {
		t.Fatalf("version blobs not reclaimed: %+v", res)
	}
	w := f.as("reader", "GET", "/v2/acme/demo/tags/list", nil)
	if !strings.Contains(w.Body.String(), `"tags":[]`) {
		t.Fatalf("dead tag pointers remain: %s", w.Body.String())
	}
}
