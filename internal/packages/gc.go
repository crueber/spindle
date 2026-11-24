// gc.go — the maintainer sweep (docs/features/14 §6): mark every blob a
// stored manifest references, delete the rest, and drop expired upload
// staging plus abort tombstones.
//
// This is the ONLY place the registry enumerates its own bucket, and it runs
// on the maintainer, never on a request path (law 4). The pass is idempotent:
// a second run over an unchanged registry deletes nothing, and a delete of an
// absent object is a no-op on every backend.
package packages

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// TaskKindGC is the Seam 5 task kind (docs/features/14 §6): the maintainer
// runs it through the same (repo,kind) single-flight task path as every other
// narrated unit, so a manual start joins the pass instead of racing it.
const TaskKindGC = "packages-gc"

// TaskLogger narrates one sweep (the maintain/op TaskLogger shape; satisfied by
// wal.Task and the maintain adapter). Nil is safe — the sweep is silent then.
type TaskLogger interface {
	Notice(text string)
	Progress(label string, done, total uint64, unit string)
}

// GCResult is what one sweep did (also the summary notice).
type GCResult struct {
	Manifests  int   `json:"manifests"`   // manifest objects scanned (mark phase)
	Blobs      int   `json:"blobs"`       // blob objects seen (sweep phase)
	Deleted    int   `json:"deleted"`     // unreferenced blobs deleted
	Kept       int   `json:"kept"`        // referenced blobs retained
	BytesFreed int64 `json:"bytes_freed"` // deleted blob bytes
	Uploads    int   `json:"uploads"`     // expired staging objects deleted
	Tombstones int   `json:"tombstones"`  // expired abort tombstones deleted
	// Retention is the tag-retention sub-report (docs/features/14 §5): the
	// pruned tags and the manifest versions they took with them. Nil when the
	// repository has no retention policy.
	Retention *RetentionResult `json:"retention,omitempty"`
	// UsageBytes is the registry's stored footprint after the pass (blob
	// bytes + manifest bytes), i.e. what the usage counter was reconciled to;
	// UsageDrift is the counter's error before reconciliation (positive =
	// the counter under-reported). Both are the reading at this instant.
	UsageBytes int64 `json:"usage_bytes"`
	UsageDrift int64 `json:"usage_drift"`
}

func (s *Service) blobGrace() time.Duration {
	if s.BlobGrace > 0 {
		return s.BlobGrace
	}
	return DefaultBlobGrace
}

func (s *Service) gcInterval() time.Duration {
	if s.GCInterval > 0 {
		return s.GCInterval
	}
	return DefaultGCInterval
}

// noteBlob records a finalize time so the sweep keeps the blob for the grace
// window (the manifest that references it is still in flight). In-memory by
// design: after a restart the window restarts, which is the safe direction.
func (s *Service) noteBlob(owner, repo, hexDigest string) {
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	if s.recent == nil {
		s.recent = map[string]time.Time{}
	}
	s.recent[owner+"/"+repo+"@"+hexDigest] = s.now()
}

// blobFresh reports whether the blob was finalized on this instance inside the
// grace window. The caller holds no lock across a store call.
func (s *Service) blobFresh(owner, repo, hexDigest string, now time.Time) bool {
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	at, ok := s.recent[owner+"/"+repo+"@"+hexDigest]
	return ok && now.Sub(at) < s.blobGrace()
}

// SweepDue reports whether the maintainer should sweep repo this pass. The
// gate is a per-instance cache: a restart repeats a pass, never skips one.
func (s *Service) SweepDue(owner, repo string) bool {
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	at, ok := s.sweptAt[owner+"/"+repo]
	return !ok || s.now().Sub(at) >= s.gcInterval()
}

func (s *Service) markSwept(owner, repo string, now time.Time) {
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	if s.sweptAt == nil {
		s.sweptAt = map[string]time.Time{}
	}
	s.sweptAt[owner+"/"+repo] = now
}

// pruneRecent drops finalize records past the grace window (bounded memory on
// a long-lived instance).
func (s *Service) pruneRecent(now time.Time) {
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	for k, at := range s.recent {
		if now.Sub(at) >= s.blobGrace() {
			delete(s.recent, k)
		}
	}
}

// manifestBlobs returns the blob digests a manifest body references: the image
// config, layers, artifact blobs and subject. Index child manifests live under
// manifests/ (not blobs/) so they are not marked here; they are scanned on
// their own. An unparseable body marks nothing — which is safe, because a
// manifest is only ever created from bytes the registry itself parsed.
func manifestBlobs(raw []byte) []string {
	var doc struct {
		Config *struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
		Blobs []struct {
			Digest string `json:"digest"`
		} `json:"blobs"`
		Subject *struct {
			Digest string `json:"digest"`
		} `json:"subject"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	out := make([]string, 0, 2+len(doc.Layers)+len(doc.Blobs))
	add := func(d string) {
		if IsDigest(d) {
			out = append(out, DigestHex(d))
		}
	}
	if doc.Config != nil {
		add(doc.Config.Digest)
	}
	for _, l := range doc.Layers {
		add(l.Digest)
	}
	for _, b := range doc.Blobs {
		add(b.Digest)
	}
	if doc.Subject != nil {
		add(doc.Subject.Digest)
	}
	return out
}

// listMeta enumerates one prefix (the maintainer's LIST; never a request
// path). The callback receives a copy so no lock is held across the walk.
func (s *Service) listMeta(ctx context.Context, prefix string) ([]store.ObjectMeta, error) {
	var out []store.ObjectMeta
	err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		out = append(out, m)
		return nil
	})
	return out, err
}

// GC runs one mark/sweep pass for repo. It is single-flight through the task
// path at the caller (maintainer or op) and idempotent on its own.
func (s *Service) GC(ctx context.Context, owner, repo string, log TaskLogger) (GCResult, error) {
	res, err := s.sweepRepo(ctx, owner, repo, true, log)
	if err != nil {
		return res, err
	}
	s.markSwept(owner, repo, s.now())
	return res, nil
}

// PlanGC is the read-only twin of GC: it runs the same mark/sweep walk and
// COUNTS what an applying pass would reclaim, deleting nothing and moving no
// due-gate clock. It exists so an operator can see the number before pulling
// the trigger; the count is a reading of the registry at this instant, not a
// promise (a push racing the pass changes it).
func (s *Service) PlanGC(ctx context.Context, owner, repo string, log TaskLogger) (GCResult, error) {
	return s.sweepRepo(ctx, owner, repo, false, log)
}

// sweepRepo is the one mark/sweep body: apply=true deletes, apply=false counts
// (both report the same shape, so a dry run and a live pass cannot disagree
// about what "unreferenced" means).
func (s *Service) sweepRepo(ctx context.Context, owner, repo string, apply bool, log TaskLogger) (GCResult, error) {
	var res GCResult
	if err := validateRepo(owner, repo); err != nil {
		return res, err
	}
	now := s.now()
	s.pruneRecent(now)

	// The counter as the pass found it — the drift this pass reports.
	counter, _, cerr := s.readUsage(ctx, owner, repo)
	if cerr != nil {
		return res, mapStoreErr(cerr)
	}

	// --- retention: apply the repo's tag policy before the mark phase, so
	// the bytes a pruned version owned are swept by this same pass ---
	pol := s.policyFor(ctx, owner, repo)
	if len(pol.Retention) > 0 {
		rr, rerr := s.applyRetention(ctx, owner, repo, pol.Retention, apply)
		if rerr != nil {
			return res, rerr
		}
		res.Retention = &rr
		if apply && rr.PrunedTags > 0 && log != nil {
			log.Notice("packages retention " + owner + "/" + repo + ": pruned " +
				strconv.Itoa(rr.PrunedTags) + " tag(s), " + strconv.Itoa(rr.Manifests) +
				" manifest version(s) (" + strconv.FormatInt(rr.Bytes, 10) + " bytes)")
		}
	}

	// --- mark: every manifest under the prefix, whatever references it ---
	manifests, err := s.listMeta(ctx, manifestsPrefix(owner, repo))
	if err != nil {
		return res, mapStoreErr(err)
	}
	manifestBytes := int64(0)
	referenced := map[string]bool{}
	for i, m := range manifests {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		raw, _, gerr := store.GetBytes(ctx, s.Store, m.Key, store.GetOptions{})
		if gerr != nil {
			if store.IsNotFound(gerr) {
				continue // deleted under us; its blobs are the next pass's problem
			}
			return res, mapStoreErr(gerr)
		}
		res.Manifests++
		manifestBytes += m.Size
		for _, hexDigest := range manifestBlobs(raw) {
			referenced[hexDigest] = true
		}
		progress(log, "manifests", uint64(i+1), uint64(len(manifests)), "manifest")
	}

	// --- sweep: blobs no stored manifest names ---
	blobs, err := s.listMeta(ctx, blobsPrefix(owner, repo))
	if err != nil {
		return res, mapStoreErr(err)
	}
	keptBlobBytes := int64(0)
	allBlobBytes := int64(0)
	for i, m := range blobs {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res.Blobs++
		allBlobBytes += m.Size
		hexDigest := strings.TrimPrefix(m.Key, blobsPrefix(owner, repo))
		if referenced[hexDigest] || s.blobFresh(owner, repo, hexDigest, now) {
			res.Kept++
			keptBlobBytes += m.Size
			progress(log, "blobs", uint64(i+1), uint64(len(blobs)), "blob")
			continue
		}
		if apply {
			if derr := s.Store.Delete(ctx, m.Key, ""); derr != nil {
				if !store.IsNotFound(derr) {
					return res, mapStoreErr(derr)
				}
			}
		}
		res.Deleted++
		res.BytesFreed += m.Size
		progress(log, "blobs", uint64(i+1), uint64(len(blobs)), "blob")
	}

	// --- staging: expired uploads and abort tombstones ---
	uploads, err := s.listMeta(ctx, uploadsPrefix(owner, repo))
	if err != nil {
		return res, mapStoreErr(err)
	}
	for _, m := range uploads {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		rest := strings.TrimPrefix(m.Key, uploadsPrefix(owner, repo))
		id, leaf := splitUploadKey(rest)
		if id == "" || !s.uploadExpired(id, now) {
			continue
		}
		if apply {
			if derr := s.Store.Delete(ctx, m.Key, ""); derr != nil && !store.IsNotFound(derr) {
				return res, mapStoreErr(derr)
			}
		}
		switch {
		case leaf == "aborted":
			res.Tombstones++
		default: // "data" and a compose part
			res.Uploads++
		}
	}

	// --- usage: reconcile the counter to this walk's reading ---
	// The pass just enumerated both families, so the true footprint is the
	// surviving manifests plus the surviving blobs. A dry run reports the
	// drift against the storage as it stands (nothing was deleted); an
	// applying pass writes the post-pass number and records the error it
	// repaired. This is the reconciliation task (docs/features/14 §5) — no
	// LIST happens on any request path.
	if apply {
		res.UsageBytes = manifestBytes + keptBlobBytes
		res.UsageDrift = res.UsageBytes - counter.Bytes
		if uerr := s.setUsage(ctx, owner, repo, Usage{
			Bytes: res.UsageBytes, BlobBytes: keptBlobBytes, ManifestBytes: manifestBytes,
			Blobs: int64(res.Kept), Manifests: int64(res.Manifests),
		}); uerr != nil {
			return res, mapStoreErr(uerr)
		}
		s.noteInstanceUsage(ctx, owner, repo, res.UsageBytes)
	} else {
		res.UsageBytes = manifestBytes + allBlobBytes
		res.UsageDrift = res.UsageBytes - counter.Bytes
	}

	if apply {
		s.markSwept(owner, repo, now)
	}
	if log != nil {
		log.Notice("packages-gc " + owner + "/" + repo + ": " + res.summaryMode(apply))
	}
	return res, nil
}

func (r GCResult) summary() string { return r.summaryMode(true) }

// summaryMode states what the pass did, or what it would do (dry run), so the
// narration can never report a count as a deletion.
func (r GCResult) summaryMode(apply bool) string {
	verb, unit := "deleted", "unreferenced blob(s)"
	uploads := "swept"
	if !apply {
		verb, unit = "would delete", "unreferenced blob(s)"
		uploads = "would sweep"
	}
	s := "scanned " + strconv.Itoa(r.Manifests) + " manifests, " + verb + " " +
		strconv.Itoa(r.Deleted) + " " + unit + " (" + strconv.FormatInt(r.BytesFreed, 10) +
		" bytes), " + uploads + " " + strconv.Itoa(r.Uploads) + " upload(s) and " +
		strconv.Itoa(r.Tombstones) + " tombstone(s)"
	if r.Retention != nil && r.Retention.PrunedTags > 0 {
		retVerb := "retention pruned"
		if !apply {
			retVerb = "retention would prune"
		}
		s += "; " + retVerb + " " + strconv.Itoa(r.Retention.PrunedTags) + " tag(s) and " +
			strconv.Itoa(r.Retention.Manifests) + " manifest version(s)"
	}
	if r.UsageDrift != 0 {
		s += "; usage counter drifted " + strconv.FormatInt(r.UsageDrift, 10) + " bytes"
		if apply {
			s += " (reconciled to " + strconv.FormatInt(r.UsageBytes, 10) + ")"
		}
	}
	return s
}

func progress(log TaskLogger, label string, done, total uint64, unit string) {
	if log != nil {
		log.Progress(label, done, total, unit)
	}
}

// splitUploadKey splits "<id>/<leaf>" (leaf: data | aborted | data.part-*).
func splitUploadKey(rest string) (id, leaf string) {
	i := strings.IndexByte(rest, '/')
	if i <= 0 || i == len(rest)-1 {
		return "", ""
	}
	return rest[:i], rest[i+1:]
}

// uploadCreated recovers the creation time embedded in an upload id
// (<unix-hex>-<nonce>-<hmac>). ok=false for a malformed id, which the sweep
// treats as expired (no request can ever present it: parseUploadID rejects it).
func uploadCreated(id string) (time.Time, bool) {
	parts := strings.Split(id, "-")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	ts, err := strconv.ParseInt(parts[0], 16, 64)
	if err != nil || ts <= 0 {
		return time.Time{}, false
	}
	return time.Unix(ts, 0).UTC(), true
}

// uploadExpired reports whether one staging id is past its TTL.
func (s *Service) uploadExpired(id string, now time.Time) bool {
	created, ok := uploadCreated(id)
	if !ok {
		return true
	}
	return now.Sub(created) > s.uploadTTL()
}

// RepoMoved drops this instance's sweep bookkeeping for a repository that
// moved to another owner/repo (PLAN-V2 §T.1, docs/features/14 §6). The
// registry prefix itself is inside repos/<o>/<r>/packages/registry and travels
// with the repo copy, so invalidation is the whole cross-reference cost: both
// in-memory maps are pure warmth (a wiped instance repeats a pass, never skips
// one), and a stale entry under the OLD id would otherwise protect a blob of
// a name that may be re-created by someone else. Both ids are dropped —
// old (the path is gone) and new (its clocks belonged to another path; the
// fresh grace/due window is the safe direction).
func (s *Service) RepoMoved(oldOwner, oldRepo, newOwner, newRepo string) {
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	for _, id := range []string{oldOwner + "/" + oldRepo, newOwner + "/" + newRepo} {
		delete(s.sweptAt, id)
		for k := range s.recent {
			if strings.HasPrefix(k, id+"@") {
				delete(s.recent, k)
			}
		}
	}
}
