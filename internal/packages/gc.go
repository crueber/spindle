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
	var res GCResult
	if err := validateRepo(owner, repo); err != nil {
		return res, err
	}
	now := s.now()
	s.pruneRecent(now)

	// --- mark: every manifest under the prefix, whatever references it ---
	manifests, err := s.listMeta(ctx, manifestsPrefix(owner, repo))
	if err != nil {
		return res, mapStoreErr(err)
	}
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
	for i, m := range blobs {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res.Blobs++
		hexDigest := strings.TrimPrefix(m.Key, blobsPrefix(owner, repo))
		if referenced[hexDigest] || s.blobFresh(owner, repo, hexDigest, now) {
			res.Kept++
			progress(log, "blobs", uint64(i+1), uint64(len(blobs)), "blob")
			continue
		}
		if derr := s.Store.Delete(ctx, m.Key, ""); derr != nil {
			if !store.IsNotFound(derr) {
				return res, mapStoreErr(derr)
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
		if derr := s.Store.Delete(ctx, m.Key, ""); derr != nil && !store.IsNotFound(derr) {
			return res, mapStoreErr(derr)
		}
		switch {
		case leaf == "aborted":
			res.Tombstones++
		default: // "data" and a compose part
			res.Uploads++
		}
	}

	s.markSwept(owner, repo, now)
	if log != nil {
		log.Notice("packages-gc " + owner + "/" + repo + ": " + res.summary())
	}
	return res, nil
}

func (r GCResult) summary() string {
	return "scanned " + strconv.Itoa(r.Manifests) + " manifests, deleted " +
		strconv.Itoa(r.Deleted) + " unreferenced blob(s) (" + strconv.FormatInt(r.BytesFreed, 10) +
		" bytes), swept " + strconv.Itoa(r.Uploads) + " upload(s) and " +
		strconv.Itoa(r.Tombstones) + " tombstone(s)"
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
