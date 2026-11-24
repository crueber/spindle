// usage_edge_test.go — the failure and dry-run branches of the quota/usage
// work: the projection's arithmetic, a corrupt/absent counter, CAS contention,
// read/write failures that must never fail a committed object, and retention in
// plan mode plus the index-child rules.
package packages

import (
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

// TestUsageInfoArithmetic pins the projection's numbers (text always carries
// the state; Percent is only a meter width).
func TestUsageInfoArithmetic(t *testing.T) {
	var nilInfo *UsageInfo
	if nilInfo.Limited() || nilInfo.OverQuota() || nilInfo.Percent() != 0 || nilInfo.Remaining() != 0 {
		t.Fatal("nil UsageInfo must read as unlimited/zero")
	}
	unlimited := &UsageInfo{Bytes: 4096}
	if unlimited.Limited() || unlimited.OverQuota() || unlimited.Percent() != 0 || unlimited.Remaining() != 0 {
		t.Fatalf("unlimited reading = %+v", unlimited)
	}
	part := &UsageInfo{Bytes: 100, QuotaBytes: 400}
	if !part.Limited() || part.OverQuota() || part.Percent() != 25 || part.Remaining() != 300 {
		t.Fatalf("partial reading = %+v", part)
	}
	over := &UsageInfo{Bytes: 500, QuotaBytes: 400}
	if !over.OverQuota() || over.Percent() != 100 || over.Remaining() != 0 {
		t.Fatalf("over reading = %+v", over)
	}
	full := &UsageInfo{Bytes: 400, QuotaBytes: 400}
	if full.OverQuota() || full.Percent() != 100 || full.Remaining() != 0 {
		t.Fatalf("full reading = %+v", full)
	}
	empty := &UsageInfo{QuotaBytes: 400}
	if empty.Percent() != 0 || empty.Remaining() != 400 {
		t.Fatalf("empty reading = %+v", empty)
	}
}

// TestUsageCounterCorruptReadsAsZero: a corrupt counter never bricks the
// registry; the sweep reconciles it.
func TestUsageCounterCorruptReadsAsZero(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := store.PutBytes(ctx, f.st, UsageKey("acme", "demo"), []byte("{not json"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	u, oerr := f.svc.Usage(ctx, "acme", "demo")
	if oerr != nil || u.Bytes != 0 {
		t.Fatalf("corrupt counter = %+v (%v)", u, oerr)
	}
	// A push still counts from zero.
	f.pushBlob(t, "writer", []byte("abc"))
	if u, _ = f.svc.Usage(ctx, "acme", "demo"); u.Bytes != 3 || u.Blobs != 1 {
		t.Fatalf("counter after corrupt read = %+v", u)
	}
}

// TestUsageReadFailureFailsClosed: when the counter cannot be read the manifest
// commit point answers 500 instead of pushing unbounded.
func TestUsageReadFailureFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.st.getKeyFn = func(key string, res store.GetResult, err error) (store.GetResult, error) {
		if strings.HasSuffix(key, "/usage.json") {
			return nil, errors.New("counter unreadable")
		}
		return res, err
	}
	w := f.as("writer", "PUT", "/v2/acme/demo/manifests/app", []byte(imageManifest))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("unreadable counter = %d %s", w.Code, w.Body.String())
	}
	f.st.getKeyFn = nil
	// Usage() surfaces the failure as a 500 too (never a fabricated zero).
	f.st.getKeyFn = func(key string, res store.GetResult, err error) (store.GetResult, error) {
		if strings.HasSuffix(key, "/usage.json") {
			return nil, errors.New("boom")
		}
		return res, err
	}
	if _, oerr := f.svc.Usage(context.Background(), "acme", "demo"); oerr == nil || oerr.Status != http.StatusInternalServerError {
		t.Fatalf("Usage error = %v", oerr)
	}
}

// TestUsageCASContention: a lost version race is retried (both the fast path
// and the re-read path), and a persistent write failure only logs — the object
// that committed stays committed.
func TestUsageCASContention(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.pushBlob(t, "writer", []byte("base"))

	// The fast path loses one CAS.
	calls := 0
	f.st.putPreFn = func(key string) error {
		if strings.HasSuffix(key, "/usage.json") && calls == 0 {
			calls++
			return store.NewPrecondition(key, "v0")
		}
		return nil
	}
	f.pushBlob(t, "writer", []byte("layer-b"))
	f.st.putPreFn = nil
	u, _ := f.svc.Usage(ctx, "acme", "demo")
	if u.Bytes != 11 { // "base" (4) + "layer-b" (7)
		t.Fatalf("counter after one lost CAS = %+v", u)
	}

	// The re-read path also loses: adjustUsage gives up and logs, the blob is
	// still stored (reconcile repairs the number later).
	alwaysFail := func(key string) error {
		if strings.HasSuffix(key, "/usage.json") {
			return store.NewPrecondition(key, "vX")
		}
		return nil
	}
	f.st.putPreFn = alwaysFail
	f.pushBlob(t, "writer", []byte("layer-c"))
	f.st.putPreFn = nil
	if meta, err := f.st.Head(ctx, blobKey("acme", "demo", DigestHex(digestOf([]byte("layer-c"))))); err != nil || meta == nil {
		t.Fatalf("a usage CAS failure cost the blob: meta=%v err=%v", meta, err)
	}
	// A non-CAS write failure is the same story.
	f.st.putPreFn = func(key string) error {
		if strings.HasSuffix(key, "/usage.json") {
			return errors.New("disk on fire")
		}
		return nil
	}
	f.pushBlob(t, "writer", []byte("layer-d"))
	f.st.putPreFn = nil
	res, err := f.svc.GC(ctx, "acme", "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.UsageDrift == 0 {
		t.Fatal("the reconcile did not report the drifted counter")
	}
	if u, _ = f.svc.Usage(ctx, "acme", "demo"); u.Bytes != res.UsageBytes {
		t.Fatalf("counter = %d, reconcile said %d", u.Bytes, res.UsageBytes)
	}
}

// TestInstanceUsageEdges: corrupt index → explicit error; read failure →
// explicit error; a lost CAS retries; an unreadable index is rebuilt; zero
// bytes drops the repo's row.
func TestInstanceUsageEdges(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if u, err := f.svc.InstanceUsage(ctx); err != nil || u.TotalBytes != 0 || u.Repos == nil {
		t.Fatalf("absent index = %+v (%v)", u, err)
	}
	// Corrupt.
	if _, err := store.PutBytes(ctx, f.st, InstanceUsageKey(), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.InstanceUsage(ctx); err == nil {
		t.Fatal("corrupt index read as valid")
	}
	// noteInstanceUsage rebuilds it (corrupt → fresh map, one true row).
	f.svc.noteInstanceUsage(ctx, "acme", "demo", 100)
	u, err := f.svc.InstanceUsage(ctx)
	if err != nil || u.TotalBytes != 100 || u.Repos["acme/demo"] != 100 {
		t.Fatalf("rebuilt index = %+v (%v)", u, err)
	}
	// A second repo adds; the largest list caps and orders.
	f.svc.noteInstanceUsage(ctx, "bob", "tools", 300)
	f.svc.noteInstanceUsage(ctx, "zoe", "tiny", 50)
	u, _ = f.svc.InstanceUsage(ctx)
	if u.TotalBytes != 450 {
		t.Fatalf("total = %d", u.TotalBytes)
	}
	if got := u.Largest(2); len(got) != 2 || got[0].Repo != "bob/tools" || got[1].Repo != "acme/demo" {
		t.Fatalf("largest = %+v", got)
	}
	if got := u.Largest(0); len(got) != 3 {
		t.Fatalf("Largest(0) = %+v", got)
	}
	// Zero drops the row.
	f.svc.noteInstanceUsage(ctx, "zoe", "tiny", 0)
	if u, _ = f.svc.InstanceUsage(ctx); u.TotalBytes != 400 {
		t.Fatalf("total after drop = %d", u.TotalBytes)
	}
	if _, ok := u.Repos["zoe/tiny"]; ok {
		t.Fatal("zero-byte repo left a row")
	}
	// Contention: the first write loses its CAS and is retried.
	loser := true
	f.st.putPreFn = func(key string) error {
		if key == InstanceUsageKey() && loser {
			loser = false
			return store.NewPrecondition(key, "v0")
		}
		return nil
	}
	f.svc.noteInstanceUsage(ctx, "amy", "new", 10)
	f.st.putPreFn = nil
	if u, _ = f.svc.InstanceUsage(ctx); u.Repos["amy/new"] != 10 {
		t.Fatalf("lost CAS dropped the row: %+v", u)
	}
	// Persistent read/write failures only log.
	f.st.getKeyFn = func(key string, res store.GetResult, err error) (store.GetResult, error) {
		if key == InstanceUsageKey() {
			return nil, errors.New("index down")
		}
		return res, err
	}
	f.svc.noteInstanceUsage(ctx, "amy", "new", 20)
	f.st.getKeyFn = nil
	f.st.putPreFn = func(key string) error {
		if key == InstanceUsageKey() {
			return errors.New("write down")
		}
		return nil
	}
	f.svc.noteInstanceUsage(ctx, "amy", "new", 30)
	f.st.putPreFn = nil
	if u, _ = f.svc.InstanceUsage(ctx); u.Repos["amy/new"] != 10 {
		t.Fatalf("failed writes changed the index: %+v", u)
	}
}

// TestRetentionPlanMode: a dry run reports what retention would prune, deletes
// nothing, and does not fail on a pointer that vanished under it.
func TestRetentionPlanMode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var quota int64
	rules := []config.PackageRetention{{Pattern: "*", KeepLast: 1}}
	f.setPolicy(&quota, &rules)
	for _, tag := range []string{"a", "b"} {
		f.now = f.now.Add(time.Hour)
		f.pushManifest(t, "writer", tag, manifestWith(tag))
	}
	res, err := f.svc.PlanGC(ctx, "acme", "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Retention == nil || res.Retention.PrunedTags != 1 {
		t.Fatalf("plan retention = %+v", res.Retention)
	}
	if strings.Join(res.Retention.PrunedNames, ",") != "a" {
		t.Fatalf("plan names = %v", res.Retention.PrunedNames)
	}
	// Nothing deleted, the counter untouched.
	tags, _, _ := f.svc.ListTags(ctx, "acme", "demo", "", 100)
	if strings.Join(tags, ",") != "a,b" {
		t.Fatalf("plan deleted tags: %v", tags)
	}
	if res.Retention.Manifests != 1 {
		t.Fatalf("plan should count the version it would delete: %+v", res.Retention)
	}
	if meta, _ := f.st.Head(ctx, manifestKey("acme", "demo", DigestHex(DigestBytes(manifestWith("a"))))); meta == nil {
		t.Fatal("plan deleted a manifest")
	}
}

// TestRetentionIndexChildren: pruning a tagged index takes its platform
// manifests with it, unless a surviving manifest still names them.
func TestRetentionIndexChildren(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var quota int64
	rules := []config.PackageRetention{{Pattern: "idx-*", MaxAge: config.Duration(24 * time.Hour)}}
	f.setPolicy(&quota, &rules)
	shared := manifestWith("shared")
	onlyOld := manifestWith("only-old")
	// Two untagged platform manifests...
	for _, body := range [][]byte{shared, onlyOld} {
		if _, oerr := f.svc.PutManifest(ctx, "acme", "demo", DigestBytes(body), body, ""); oerr != nil {
			t.Fatal(oerr)
		}
	}
	index := func(kids ...[]byte) []byte {
		ms := make([]map[string]any, 0, len(kids))
		for _, k := range kids {
			ms = append(ms, map[string]any{"digest": DigestBytes(k), "size": len(k)})
		}
		body, _ := json.Marshal(map[string]any{
			"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": ms,
		})
		return body
	}
	oldIndex := index(shared, onlyOld)
	keepIndex := index(shared)
	f.pushManifest(t, "writer", "idx-old", oldIndex)
	f.now = f.now.Add(48 * time.Hour)
	f.pushManifest(t, "writer", "keep", keepIndex)
	f.pushManifest(t, "writer", "idx-new", index(onlyOld))
	res, err := f.svc.GC(ctx, "acme", "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Only the aged tag went; idx-new still names onlyOld.
	if res.Retention == nil || res.Retention.PrunedTags != 1 {
		t.Fatalf("retention = %+v", res.Retention)
	}
	if meta, _ := f.st.Head(ctx, manifestKey("acme", "demo", DigestHex(DigestBytes(shared)))); meta == nil {
		t.Fatal("a manifest a surviving index names was deleted")
	}
	if meta, _ := f.st.Head(ctx, manifestKey("acme", "demo", DigestHex(DigestBytes(oldIndex)))); meta != nil {
		t.Fatal("the pruned index survived")
	}
}

// TestRetentionIndexChildFollowsPrunedIndex: a child only the pruned index
// named is deleted with it.
func TestRetentionIndexChildFollowsPrunedIndex(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var quota int64
	rules := []config.PackageRetention{{Pattern: "*", KeepLast: 1}}
	f.setPolicy(&quota, &rules)
	child := manifestWith("child")
	if _, oerr := f.svc.PutManifest(ctx, "acme", "demo", DigestBytes(child), child, ""); oerr != nil {
		t.Fatal(oerr)
	}
	index := func(kid []byte) []byte {
		body, _ := json.Marshal(map[string]any{
			"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
			"manifests": []map[string]any{{"digest": DigestBytes(kid), "size": len(kid)}},
		})
		return body
	}
	f.pushManifest(t, "writer", "old", index(child))
	f.now = f.now.Add(time.Hour)
	f.pushManifest(t, "writer", "new", manifestWith("new"))
	if _, err := f.svc.GC(ctx, "acme", "demo", nil); err != nil {
		t.Fatal(err)
	}
	if meta, _ := f.st.Head(ctx, manifestKey("acme", "demo", DigestHex(DigestBytes(child)))); meta != nil {
		t.Fatal("an index child only the pruned index named survived")
	}
}

// TestRetentionSkipsUndatablePointers and the listing failures: a pointer with
// no parsable updated_at is never pruned (retention does not guess), and a
// listing error fails the pass instead of pruning blind.
func TestRetentionSkipsUndatablePointers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rules := []config.PackageRetention{{Pattern: "*", KeepLast: 1}}
	// Two pointers: a datable one and a malformed one the policy cannot date.
	doc, _ := json.Marshal(tagDoc{Digest: DigestBytes(manifestWith("a")), UpdatedAt: f.now.Add(-time.Hour).Format(time.RFC3339)})
	if _, err := store.PutBytes(ctx, f.st, tagKey("acme", "demo", "a"), doc, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	bad := []byte(`{"digest":"` + DigestBytes(manifestWith("b")) + `"}`)
	if _, err := store.PutBytes(ctx, f.st, tagKey("acme", "demo", "b"), bad, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	// The store body for the manifests must exist for the delete path to look
	// sane, but retention only needs the pointers.
	res, err := f.svc.applyRetention(ctx, "acme", "demo", rules, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tags != 1 || res.PrunedTags != 0 {
		t.Fatalf("retention = %+v (the undatable pointer must be skipped)", res)
	}
	if meta, _ := f.st.Head(ctx, tagKey("acme", "demo", "b")); meta == nil {
		t.Fatal("an undatable pointer was pruned")
	}

	// A LIST error fails the pass.
	f.st.listKeyFn = func(prefix string, err error) error {
		if strings.HasSuffix(prefix, "/tags/") {
			return errors.New("list down")
		}
		return err
	}
	if _, err := f.svc.applyRetention(ctx, "acme", "demo", rules, true); err == nil {
		t.Fatal("retention ignored a listing failure")
	}
	f.st.listKeyFn = nil
}

// TestRetentionPointerVanished: a pointer deleted by another writer between the
// read and the delete is skipped, not fatal.
func TestRetentionPointerVanished(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var quota int64
	rules := []config.PackageRetention{{Pattern: "*", KeepLast: 1}}
	f.setPolicy(&quota, &rules)
	f.pushManifest(t, "writer", "old", manifestWith("old"))
	f.now = f.now.Add(time.Hour)
	f.pushManifest(t, "writer", "new", manifestWith("new"))
	f.st.deleteKeyFn = func(key string, err error) error {
		if strings.HasSuffix(key, "/tags/old") {
			return store.NewNotFound(key)
		}
		return err
	}
	if _, err := f.svc.GC(ctx, "acme", "demo", nil); err != nil {
		t.Fatalf("a vanished pointer failed the pass: %v", err)
	}
	f.st.deleteKeyFn = nil
}

// TestGCResultSummary: the narration names retention and the reconciled drift,
// and a clean pass says neither.
func TestGCResultSummary(t *testing.T) {
	clean := GCResult{Manifests: 1, Kept: 1}
	if s := clean.summary(); strings.Contains(s, "retention") || strings.Contains(s, "drift") {
		t.Fatalf("clean summary = %q", s)
	}
	dirty := GCResult{
		Manifests: 3, Deleted: 1, BytesFreed: 10,
		Retention:  &RetentionResult{PrunedTags: 2, Manifests: 1},
		UsageDrift: 5, UsageBytes: 100,
	}
	s := dirty.summary()
	for _, want := range []string{"retention pruned 2 tag(s)", "usage counter drifted 5 bytes", "reconciled to 100"} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary %q missing %q", s, want)
		}
	}
	plan := GCResult{Retention: &RetentionResult{PrunedTags: 1}}
	if s := plan.summaryMode(false); !strings.Contains(s, "retention would prune 1 tag(s)") {
		t.Fatalf("plan summary = %q", s)
	}
}

// TestUsageAndRetentionFailureBranches pins the remaining fail-safe edges:
// every one of them either fails the pass explicitly (a listing that cannot be
// trusted) or logs and lets committed data stand (a counter that cannot be
// written). None may panic, and none may fabricate a number.
func TestUsageAndRetentionFailureBranches(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Usage on an invalid repository name is the registry's NAME_UNKNOWN.
	if _, oerr := f.svc.Usage(ctx, "bad owner", "demo"); oerr == nil || oerr.Code != CodeNameUnknown {
		t.Fatalf("invalid repo = %v", oerr)
	}
	// The clamp is a display guard for a delta that raced a reconcile.
	u := Usage{Bytes: -1, BlobBytes: -2, ManifestBytes: -3, Blobs: -4, Manifests: -5}
	clampNonNegative(&u)
	if u.Bytes != 0 || u.BlobBytes != 0 || u.ManifestBytes != 0 || u.Blobs != 0 || u.Manifests != 0 {
		t.Fatalf("clamp = %+v", u)
	}
	// The refusal message clamps a negative overshoot (a caller can only reach
	// it with a policy that shrank under a concurrent read).
	if e := errQuotaExceeded(10, 100, 10); !strings.Contains(e.Message, "0 B over") {
		t.Fatalf("negative overshoot message = %q", e.Message)
	}
	// Largest on nil/empty maps is empty, never a nil dereference.
	var empty InstanceUsage
	if got := empty.Largest(5); len(got) != 0 {
		t.Fatalf("empty largest = %+v", got)
	}
	var nilUsage *InstanceUsage
	if got := nilUsage.Largest(5); len(got) != 0 {
		t.Fatalf("nil largest = %+v", got)
	}

	// A version-less index object reads as an empty index (a lost object is
	// the absent case the caller already renders as "no pass recorded").
	if _, err := store.PutBytes(ctx, f.st, InstanceUsageKey(), []byte("{}"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if u, err := f.svc.InstanceUsage(ctx); err != nil || u.Repos == nil {
		t.Fatalf("empty index = %+v (%v)", u, err)
	}
	if _, err := store.PutBytes(ctx, f.st, InstanceUsageKey(), []byte(`{"total_bytes":0}`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if u, err := f.svc.InstanceUsage(ctx); err != nil || u.Version != 1 || u.Repos == nil {
		t.Fatalf("version-less index = %+v (%v)", u, err)
	}
	// A read failure is an error (the card renders it), never a zero.
	f.st.getKeyFn = func(key string, res store.GetResult, err error) (store.GetResult, error) {
		if key == InstanceUsageKey() {
			return nil, errors.New("index down")
		}
		return res, err
	}
	if _, err := f.svc.InstanceUsage(ctx); err == nil {
		t.Fatal("unreadable index read as empty")
	}
	f.st.getKeyFn = nil

	// adjustUsage's own read failure logs and leaves the counter alone (the
	// request paths fail closed before reaching it; this is the post-commit
	// retry and the sweep repairs the number).
	f.st.getKeyFn = func(key string, res store.GetResult, err error) (store.GetResult, error) {
		if strings.HasSuffix(key, "/usage.json") {
			return nil, errors.New("counter down")
		}
		return res, err
	}
	f.svc.adjustUsage(ctx, "acme", "demo", Usage{Bytes: 5, Blobs: 1})
	f.st.getKeyFn = nil
	if u, _ := f.svc.Usage(ctx, "acme", "demo"); u.Bytes != 0 {
		t.Fatalf("a failed counter read wrote a delta: %+v", u)
	}

	// setUsage cannot win its CAS: the sweep still finishes (the number is a
	// reading, not a commit point).
	var quota int64
	rules := []config.PackageRetention{{Pattern: "*", KeepLast: 1}}
	f.setPolicy(&quota, &rules)
	f.pushManifest(t, "writer", "v1", manifestWith("v1"))
	f.st.putPreFn = func(key string) error {
		if strings.HasSuffix(key, "/usage.json") {
			return store.NewPrecondition(key, "vX")
		}
		return nil
	}
	if _, err := f.svc.GC(ctx, "acme", "demo", nil); err != nil {
		t.Fatalf("persistent usage CAS failure failed the sweep: %v", err)
	}
	f.st.putPreFn = nil

	// Retention's pointer read fails (not a LIST, a GET) and a pointer that
	// vanished between list and read is skipped.
	f.pushManifest(t, "writer", "v2", manifestWith("v2"))
	f.st.getKeyFn = func(key string, res store.GetResult, err error) (store.GetResult, error) {
		if strings.Contains(key, "/tags/v1") {
			return nil, errors.New("pointer down")
		}
		if strings.Contains(key, "/tags/v2") {
			return nil, store.NewNotFound(key)
		}
		return res, err
	}
	if _, err := f.svc.applyRetention(ctx, "acme", "demo", rules, true); err == nil {
		t.Fatal("retention ignored an unreadable pointer")
	}
	f.st.getKeyFn = nil

	// setUsage surfaces a counter read failure to its caller (the sweep).
	f.st.getKeyFn = func(key string, res store.GetResult, err error) (store.GetResult, error) {
		if strings.HasSuffix(key, "/usage.json") {
			return nil, errors.New("counter down")
		}
		return res, err
	}
	if err := f.svc.setUsage(ctx, "acme", "demo", Usage{Bytes: 1}); err == nil {
		t.Fatal("setUsage ignored a read failure")
	}
	f.st.getKeyFn = nil

	// A version-less counter document reads as version 1 (append-only
	// tolerance for an older writer's shape).
	if _, err := store.PutBytes(ctx, f.st, UsageKey("acme", "demo"), []byte(`{"bytes":7}`), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if u, _ := f.svc.Usage(ctx, "acme", "demo"); u.Bytes != 7 {
		t.Fatalf("version-less counter = %+v", u)
	}

	// adjustUsage and setUsage surface a non-CAS write failure to nobody (they
	// log) and to the sweep (an error) respectively.
	f.st.putPreFn = func(key string) error {
		if strings.HasSuffix(key, "/usage.json") {
			return errors.New("disk on fire")
		}
		return nil
	}
	f.svc.adjustUsage(ctx, "acme", "demo", Usage{Bytes: 1})
	if err := f.svc.setUsage(ctx, "acme", "demo", Usage{Bytes: 7}); err == nil {
		t.Fatal("setUsage ignored a hard write failure")
	}
	f.st.putPreFn = nil

	// A pointer delete that fails hard fails the retention pass.
	f.pushManifest(t, "writer", "v3", manifestWith("v3"))
	f.st.deleteKeyFn = func(key string, err error) error {
		if strings.Contains(key, "/tags/") {
			return errors.New("delete down")
		}
		return err
	}
	if _, err := f.svc.applyRetention(ctx, "acme", "demo", rules, true); err == nil {
		t.Fatal("retention ignored a pointer delete failure")
	}
	f.st.deleteKeyFn = nil

	// The reference scan cannot read a surviving manifest: retention fails
	// instead of deleting a version it cannot prove is unreferenced.
	f.st.getKeyFn = func(key string, res store.GetResult, err error) (store.GetResult, error) {
		if strings.Contains(key, "/manifests/") && !strings.Contains(key, DigestHex(DigestBytes(manifestWith("v3")))) {
			return nil, errors.New("manifest down")
		}
		return res, err
	}
	f.now = f.now.Add(2 * time.Hour)
	if _, err := f.svc.applyRetention(ctx, "acme", "demo", rules, true); err == nil {
		t.Fatal("retention ignored an unreadable manifest during the reference scan")
	}
	f.st.getKeyFn = nil

	// Percent on a corrupt (negative) counter reads as 0, never a negative
	// meter width.
	neg := &UsageInfo{Bytes: -5, QuotaBytes: 100}
	if neg.Percent() != 0 {
		t.Fatalf("negative percent = %d", neg.Percent())
	}

	// The mark phase cannot read a manifest: the pass fails rather than
	// treating its blobs as unreferenced.
	f.st.getKeyFn = func(key string, res store.GetResult, err error) (store.GetResult, error) {
		if strings.Contains(key, "/manifests/") {
			return nil, errors.New("manifest down")
		}
		return res, err
	}
	if _, err := f.svc.GC(ctx, "acme", "demo", nil); err == nil {
		t.Fatal("an unreadable manifest did not fail the sweep")
	}
	f.st.getKeyFn = nil
}

// TestPruneInstanceUsageEdges: the instance pass's stale-row cleanup is
// best-effort bookkeeping — every failure logs and leaves the index alone, and
// a lost CAS is retried.
func TestPruneInstanceUsageEdges(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// An absent index is nothing to prune.
	f.svc.pruneInstanceUsage(ctx, map[string]bool{})

	seed := func() {
		raw, _ := json.Marshal(InstanceUsage{Version: 1, Repos: map[string]int64{"gone/repo": 10}})
		if _, err := store.PutBytes(ctx, f.st, InstanceUsageKey(), raw, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
			t.Fatal(err)
		}
	}
	none := map[string]bool{}

	// The first read fails: log, leave it.
	seed()
	f.st.getKeyFn = func(key string, res store.GetResult, err error) (store.GetResult, error) {
		if key == InstanceUsageKey() {
			return nil, errors.New("index down")
		}
		return res, err
	}
	f.svc.pruneInstanceUsage(ctx, none)
	f.st.getKeyFn = nil

	// The second read fails: log, leave it.
	calls := 0
	f.st.getKeyFn = func(key string, res store.GetResult, err error) (store.GetResult, error) {
		if key == InstanceUsageKey() {
			calls++
			if calls == 2 {
				return nil, errors.New("second read down")
			}
		}
		return res, err
	}
	f.svc.pruneInstanceUsage(ctx, none)
	f.st.getKeyFn = nil
	if u, _ := f.svc.InstanceUsage(ctx); len(u.Repos) != 1 {
		t.Fatalf("failed prune changed the index: %+v", u)
	}

	// A lost CAS is retried and the row goes.
	lost := false
	f.st.putPreFn = func(key string) error {
		if key == InstanceUsageKey() && !lost {
			lost = true
			return store.NewPrecondition(key, "v0")
		}
		return nil
	}
	f.svc.pruneInstanceUsage(ctx, none)
	f.st.putPreFn = nil
	if u, _ := f.svc.InstanceUsage(ctx); len(u.Repos) != 0 {
		t.Fatalf("retry did not prune: %+v", u)
	}

	// A hard write failure leaves the row for the next pass.
	seed()
	f.st.putPreFn = func(key string) error {
		if key == InstanceUsageKey() {
			return errors.New("write down")
		}
		return nil
	}
	f.svc.pruneInstanceUsage(ctx, none)
	f.st.putPreFn = nil
	if u, _ := f.svc.InstanceUsage(ctx); len(u.Repos) != 1 {
		t.Fatalf("hard failure lost the row? %+v", u)
	}
}
