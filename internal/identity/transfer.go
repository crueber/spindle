// transfer.go — repository ownership transfer (Forgejo #358, PLAN-V2 §3 T.1).
//
// A transfer moves every object under repos/<srcOwner>/<srcRepo>/ to
// repos/<dstOwner>/<dstRepo>/ and then rewrites the families that name a repo
// OUTSIDE that prefix: the bucket-root catalog row, both directions of the fork
// pointers, the packages sweep bookkeeping, and the serving copies. It runs as
// the `repo-transfer` task (single-flight (repo,kind), narration packets, an
// attachable stream, a terminal record — law 7), not on the request goroutine:
// a transfer of a large repository takes minutes and a client disconnect must
// neither cancel it nor hide it.
//
// Gates (unchanged from v1, applied by the handler before the task starts):
// repo admin on the source (identity.CheckRole admin) plus CheckCreateOwner
// admission on the destination. The task body is principal-free (like
// DeleteOrg); the pre-flight re-probe only answers what the bucket says.
//
// # Ordering rule (the one this milestone commits to)
//
// One writer per prefix, one authority per repository:
//
//   - Every destination write is Create-only (PutCreate): a prefix key exists
//     because exactly one attempt created it, and a concurrent transfer loses
//     the CAS instead of interleaving writes. No destination key is ever
//     rewritten by the move.
//   - The destination manifest is copied LAST, so the destination becomes
//     openable only after every object its manifest references is there. The
//     source manifest is snapshotted first and re-read before that copy; a
//     byte difference means a push landed mid-move and the task aborts before
//     creating the destination manifest (a broken destination is worse than a
//     failed move).
//   - The source manifest is deleted FIRST in the delete pass: the moment the
//     destination manifest ACKs, authority transfers, and the source stops
//     being a repository before any object it referenced goes away.
//
// So the two paths may briefly serve the same immutable bytes (the shadow
// window), but no prefix ever has two writers, the destination is never
// observable half-formed, and the source's disappearance is monotonic. A push
// racing the delete pass is reported, never silently stranded: the post-delete
// LIST names the residue (the destination is complete; the operator deletes
// the residue and re-runs, which converges — see the resume path).
//
// # Re-runnable after a crash
//
// The bucket — not the task table — is the state machine, so a restart loses
// nothing:
//
//   - source manifest present + destination absent: a fresh move;
//   - both present: a move that crashed somewhere in the copy/delete window.
//     Each destination key that already exists is ADOPTED only when its bytes
//     equal the source's (or, for the two rewritten families, the rewritten
//     bytes): identical means a prior attempt's copy, different means a
//     foreign repository and the task aborts 409 with the source untouched;
//   - source absent + destination present: the move committed and something
//     after it failed. Only residue cleanup (each key verified as reproduced at
//     the destination) and the cross-reference families remain.
//
// Every cross-reference step is idempotent by construction: a catalog row
// rename, a fork row rename and a fork parent/root rewrite all only touch a
// value that still names the OLD id, so a retry converges instead of
// duplicating.
//
// # Concurrency
//
// Hazard: two writers racing one transfer (a concurrent push landing new
// objects mid-move, or two transfers of the same repo). Avoidance: store CAS is
// the lock (13 §3) — every destination write is PutCreate; the second attempt
// loses a key CAS, compares bytes, and aborts 409 with best-effort cleanup of
// the keys it created (the source untouched). A push racing the delete pass
// leaves a named residue and a 409, never silent loss. The serving handle is
// torn down before the copy pass (requests drain against bucket truth) and
// again after the move; no lock is held across any store call.
package identity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/sizecatalog"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/store/proto"
	"github.com/crueber/spindle/internal/wal"
)

// TransferKind is the Seam 5 task kind for a repository move.
const TransferKind = "repo-transfer"

// manifestName is the repo-existence signal (the same probe the invite paths
// gate on): a repo exists iff its manifest object exists.
const manifestName = "manifest.pb"

// --- kind registration (Seam 5) ----------------------------------------------

var (
	transferKindsMu sync.Mutex
	transferKinds   = map[string]bool{}
)

// RegisterKind records the transfer task kind; a duplicate panics (the
// maintain.RegisterKind contract — composition registers exactly once).
func RegisterKind(name string) {
	transferKindsMu.Lock()
	defer transferKindsMu.Unlock()
	if transferKinds[name] {
		panic(fmt.Sprintf("identity: duplicate task kind %q", name))
	}
	transferKinds[name] = true
}

// ResetKindsForTest clears the kind registry (test-only; the registry is
// process-global and write-only in production, so a -count=N re-run of a
// registering test would otherwise trip the duplicate panic).
func ResetKindsForTest() {
	transferKindsMu.Lock()
	defer transferKindsMu.Unlock()
	transferKinds = map[string]bool{}
}

// --- seams --------------------------------------------------------------------

// TaskRunner is the Seam 5 task table this package drives (satisfied by
// *wal.TaskTable at composition; nil = no task surface, BeginTransfer 503s).
type TaskRunner interface {
	Run(ctx context.Context, repo, kind string, params map[string]string, fn func(ctx context.Context, task *wal.Task) error) (*wal.TaskRecord, error)
	Running(repo, kind string) *wal.TaskRecord
}

// Serving is the serving-copy surface a move must quiesce and then verify
// (satisfied by *wal.Registry at composition; nil = skip those steps, which is
// only legitimate for a store-only test).
type Serving interface {
	// Invalidate tears the in-memory handle + local serving copy down without
	// touching bucket objects (the non-destructive twin of Delete).
	Invalidate(id string) error
	// VerifyRepo proves the repo at id is readable from the bucket: manifest
	// decodes and names id, refs phase replays.
	VerifyRepo(ctx context.Context, id string) error
}

// ForkMover rewrites the fork pointer families outside the moved prefix
// (satisfied by *pulls.Service at composition: identity must not import pulls —
// pulls imports identity for the P6 gates — so the fork JSON shapes stay with
// their owner; nil = no fork surface, the steps are skipped).
type ForkMover interface {
	ParentOf(ctx context.Context, owner, repo string) (string, bool, error)
	ChildMoved(ctx context.Context, parentOwner, parentRepo, oldChild, newChild string) (bool, error)
	ParentMoved(ctx context.Context, oldID, newID string) (int, error)
	VerifyMoved(ctx context.Context, oldID, newID string) error
}

// PackagesMover drops the OCI registry's per-repo sweep bookkeeping for a move
// (satisfied by *packages.Service; the registry prefix itself travels with the
// repo copy, so invalidation is the whole cost — nil = no registry surface).
type PackagesMover interface {
	RepoMoved(oldOwner, oldRepo, newOwner, newRepo string)
}

// Narrator is the narration sink for a move (satisfied by *wal.Task; nil is a
// no-op for the synchronous TransferRepo path).
type Narrator interface {
	Notice(text string)
	Progress(label string, done, total uint64, unit string)
}

type nopNarrator struct{}

func (nopNarrator) Notice(string)                           {}
func (nopNarrator) Progress(string, uint64, uint64, string) {}

// --- request ------------------------------------------------------------------

// TransferRequest is one validated move.
type TransferRequest struct {
	SrcOwner string
	SrcRepo  string
	DstOwner string
	DstRepo  string
}

// SourceID is "<srcOwner>/<srcRepo>" (the task's repo key).
func (r TransferRequest) SourceID() string { return r.SrcOwner + "/" + r.SrcRepo }

// DestinationID is "<dstOwner>/<dstRepo>".
func (r TransferRequest) DestinationID() string { return r.DstOwner + "/" + r.DstRepo }

// normalize trims and validates the request (ErrInvalid on any illegal id).
func (r TransferRequest) normalize() (TransferRequest, error) {
	r.SrcOwner = strings.TrimSpace(r.SrcOwner)
	r.SrcRepo = strings.TrimSpace(r.SrcRepo)
	r.DstOwner = strings.TrimSpace(r.DstOwner)
	r.DstRepo = strings.TrimSpace(r.DstRepo)
	if !validRepoID(r.SrcOwner, r.SrcRepo) {
		return r, fmt.Errorf("%w: invalid source repo %q", ErrInvalid, r.SrcOwner+"/"+r.SrcRepo)
	}
	if !validRepoID(r.DstOwner, r.DstRepo) {
		return r, fmt.Errorf("%w: invalid destination repo %q", ErrInvalid, r.DstOwner+"/"+r.DstRepo)
	}
	if r.SrcOwner == r.DstOwner && r.SrcRepo == r.DstRepo {
		return r, fmt.Errorf("%w: source and destination are identical", ErrInvalid)
	}
	return r, nil
}

// scrubbedMap is the TaskRecord.Params content (nothing secret, nothing that
// changes per caller).
func (r TransferRequest) scrubbedMap() map[string]string {
	return map[string]string{"from": r.SourceID(), "to": r.DestinationID()}
}

// --- start / join -------------------------------------------------------------

// transferSlot is one in-flight move (the service-level join hint; the task
// table's own (repo,kind) join is the backstop, never the decision).
type transferSlot struct {
	ready chan struct{}
	rec   *wal.TaskRecord
}

// BeginTransfer validates the request, answers the pre-flight verdict the API
// documents (404 unknown source, 409 a genuinely occupied destination), then
// starts — or joins — the `repo-transfer` task and returns its live record.
// joined reports a second start that reused the running task instead of
// starting another.
func (s *Service) BeginTransfer(ctx context.Context, req TransferRequest) (rec *wal.TaskRecord, joined bool, err error) {
	req, err = req.normalize()
	if err != nil {
		return nil, false, err
	}
	if s.Tasks == nil {
		return nil, false, fmt.Errorf("%w: task registry not configured", ErrUnavailable)
	}
	srcPrefix := store.RepoPrefix(req.SrcOwner, req.SrcRepo)
	dstPrefix := store.RepoPrefix(req.DstOwner, req.DstRepo)

	srcManifest, srcExists, err := s.readOptional(ctx, srcPrefix+manifestName)
	if err != nil {
		return nil, false, err
	}
	dstManifest, dstExists, err := s.readOptional(ctx, dstPrefix+manifestName)
	if err != nil {
		return nil, false, err
	}
	if !srcExists && !dstExists {
		return nil, false, fmt.Errorf("%w: unknown repo %q", ErrNotFound, req.SourceID())
	}
	if srcExists && dstExists {
		// Both prefixes exist. A resumed attempt carries byte-identical or
		// rewritten-identical content (the source manifest with Repo retargeted);
		// anything else is a foreign repository occupying the destination.
		want := s.rewriteTransferManifest(srcManifest, req.DestinationID())
		if want == nil {
			want = srcManifest
		}
		if !bytes.Equal(want, dstManifest) {
			return nil, false, fmt.Errorf("%w: repo already exists at %q", ErrConflict, req.DestinationID())
		}
	}
	key := req.SourceID()
	s.transferMu.Lock()
	if slot := s.transfers[key]; slot != nil {
		s.transferMu.Unlock()
		return s.awaitTransferSlot(ctx, slot)
	}
	slot := &transferSlot{ready: make(chan struct{})}
	if s.transfers == nil {
		s.transfers = map[string]*transferSlot{}
	}
	s.transfers[key] = slot
	s.transferMu.Unlock()

	go s.driveTransfer(ctx, key, slot, req)
	rec, _, err = s.awaitTransferSlot(ctx, slot)
	return rec, false, err
}

// awaitTransferSlot waits for the leader's record. The ready channel is
// preferred deterministically: a published record and a dead caller ctx are
// both ready after a disconnect, and Go would pick the arm at random (the
// coin-flip hazard 13_concurrency.md names) — a join that has a record must
// answer it.
func (s *Service) awaitTransferSlot(ctx context.Context, slot *transferSlot) (*wal.TaskRecord, bool, error) {
	select {
	case <-slot.ready:
		return slot.rec, true, nil
	default:
	}
	select {
	case <-slot.ready:
		return slot.rec, true, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

// driveTransfer runs the move on the core task table and publishes the leader's
// record into the slot. The body never sees the request ctx (a client
// disconnect cannot cancel a move), so the wait is structurally detached.
func (s *Service) driveTransfer(ctx context.Context, key string, slot *transferSlot, req TransferRequest) {
	var runErr error
	rec, _ := s.Tasks.Run(context.WithoutCancel(ctx), key, TransferKind, req.scrubbedMap(),
		func(tctx context.Context, task *wal.Task) error {
			s.setSlotRecord(slot, task.Record())
			runErr = s.transferRun(tctx, task, req)
			if runErr != nil && tctx.Err() != nil {
				// A drained registry cancels the body ctx; narrate the honest
				// retryable terminal instead of a bare context.Canceled (law 7).
				return errTransferDrained
			}
			return runErr
		})
	if rec == nil {
		// Run refused (drain) before a record existed: surface the refusal to
		// the leader's waiter with a synthesized terminal record.
		rec = &wal.TaskRecord{Kind: TransferKind, Repo: key, Params: req.scrubbedMap(),
			Started:  time.Now().UTC().Format(time.RFC3339Nano),
			Finished: time.Now().UTC().Format(time.RFC3339Nano),
			Summary:  errText(runErr, "transfer did not start (instance draining); safe to retry")}
		ok := false
		rec.OK = &ok
		s.setSlotRecord(slot, *rec)
	}
	s.transferMu.Lock()
	if s.transfers[key] == slot {
		delete(s.transfers, key)
	}
	s.transferMu.Unlock()
}

// setSlotRecord publishes the leader's record exactly once (the waiter may be
// racing the body's first narration).
func (s *Service) setSlotRecord(slot *transferSlot, rec wal.TaskRecord) {
	cp := rec
	if slot.rec != nil {
		return
	}
	slot.rec = &cp
	close(slot.ready)
}

func errText(err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	return fallback
}

// TransferRepo runs the complete move synchronously (the CLI/test shape; the
// HTTP lane starts the task through BeginTransfer). Same body, same ordering,
// same verification — only the narration sink differs.
func (s *Service) TransferRepo(ctx context.Context, srcOwner, srcRepo, dstOwner, dstRepo string) error {
	req, err := TransferRequest{SrcOwner: srcOwner, SrcRepo: srcRepo, DstOwner: dstOwner, DstRepo: dstRepo}.normalize()
	if err != nil {
		return err
	}
	return s.transferRun(ctx, nopNarrator{}, req)
}

// --- the task body -------------------------------------------------------------

// transferState carries the facts one attempt learned (and its cleanup set).
type transferState struct {
	req       TransferRequest
	srcPrefix string
	dstPrefix string

	// srcManifest is the byte snapshot the commit point is built from (nil on
	// the resume path, where the source no longer has one).
	srcManifest []byte
	// resume reports that the move already committed (source manifest gone,
	// destination manifest present): only residue + cross-references remain.
	resume bool

	// catalogHad is what the pre-flight aggregate said about the source row —
	// the verification only demands a destination row when one existed.
	catalogHadEntry bool
	catalogHadName  bool

	// packageKeys are the source registry keys (prefix-relative) that must
	// reappear at the destination.
	packageKeys []string
	// srcHadAccess records whether the source carried access.json (when it did
	// not, a user destination gets the synthesized owner binding instead).
	srcHadAccess bool

	committed bool     // the destination manifest ACKed
	created   []string // destination keys created by THIS attempt (cleanup set)
}

func (st *transferState) srcKey(rel string) string { return st.srcPrefix + rel }
func (st *transferState) dstKey(rel string) string { return st.dstPrefix + rel }

// transferRun executes the phases in order and returns the first failure with
// its specific missing piece.
func (s *Service) transferRun(ctx context.Context, n Narrator, req TransferRequest) error {
	st := &transferState{
		req:       req,
		srcPrefix: store.RepoPrefix(req.SrcOwner, req.SrcRepo),
		dstPrefix: store.RepoPrefix(req.DstOwner, req.DstRepo),
	}
	n.Notice("transfer " + req.SourceID() + " -> " + req.DestinationID() + " started")
	manifest, srcExists, err := s.readOptional(ctx, st.srcKey(manifestName))
	if err != nil {
		return err
	}
	_, dstExists, derr := s.readOptional(ctx, st.dstKey(manifestName))
	if derr != nil {
		return derr
	}
	switch {
	case !srcExists && !dstExists:
		return fmt.Errorf("%w: unknown repo %q", ErrNotFound, req.SourceID())
	case !srcExists && dstExists:
		st.resume = true
		n.Notice("move already committed at " + req.DestinationID() + "; completing residue and cross-references")
	default:
		st.srcManifest = manifest
	}
	if err := s.transferSnapshotCatalog(ctx, st); err != nil {
		return err
	}
	if !st.resume {
		if err := s.transferSnapshotPackages(ctx, st); err != nil {
			return err
		}
		if ok, aerr := store.Exists(ctx, s.Store, st.srcKey("access.json")); aerr != nil {
			return aerr
		} else {
			st.srcHadAccess = ok
		}
	} else {
		// Resume: the source's access.json travelled with the prefix, so the
		// destination's own object is the evidence of what the source had. (A
		// fresh probe of the source would answer "absent" and make the
		// synthesized default collide with the moved document.)
		if ok, aerr := store.Exists(ctx, s.Store, st.dstKey("access.json")); aerr != nil {
			return aerr
		} else {
			st.srcHadAccess = ok
		}
	}

	// Phase 1: quiesce the source serving copy. Requests already in flight
	// finish against bucket truth; nothing new is served from a stale local dir.
	if !st.resume {
		s.quiesce(ctx, req.SrcOwner, req.SrcRepo, n)
	}

	// Phase 2+3: copy (Create-only, manifest last) then delete (manifest first).
	if st.resume {
		err = s.transferResidue(ctx, n, st)
	} else {
		err = s.transferMovePass(ctx, n, st)
	}
	if err != nil {
		s.transferCleanup(ctx, st)
		return err
	}
	// Phase 3b: a missing source access.json materializes the destination's
	// synthesized default (user destinations only — see ensureTransferAccess).
	if aerr := s.ensureTransferAccess(ctx, st); aerr != nil {
		s.transferCleanup(ctx, st)
		return aerr
	}

	// Phase 4: the families outside the prefix.
	if err := s.transferCrossReferences(ctx, n, st); err != nil {
		return err
	}

	// Phase 5: prove it, or fail with the piece that is missing.
	if err := s.transferVerify(ctx, n, st); err != nil {
		return err
	}

	// Phase 6: flush both serving copies and the access cache; the next open of
	// either id reads bucket truth.
	s.access.invalidate(req.SrcOwner, req.SrcRepo)
	s.access.invalidate(req.DstOwner, req.DstRepo)
	s.quiesce(ctx, req.DstOwner, req.DstRepo, n)
	if s.Serving != nil {
		// Re-open the destination so the verified state is the live handle.
		if verr := s.Serving.VerifyRepo(ctx, req.DestinationID()); verr != nil {
			return fmt.Errorf("%w: destination unreadable after flush: %v", ErrConflict, verr)
		}
	}
	n.Notice("transfer complete: " + req.SourceID() + " -> " + req.DestinationID())
	// The ownership record (PLAN-V2 §T.4): written after verification proved
	// the move, so the entry never describes a half-moved repository. A failed
	// audit write does not un-move anything (audit-after-commit).
	detail := "from=" + req.SourceID() + ";to=" + req.DestinationID() + ";resumed=" + boolText(st.resume)
	s.auditAdmin(ctx, AuditTransfer, "repo:"+req.DestinationID(), detail)
	return nil
}

// transferSnapshotCatalog records whether the aggregate names the row this
// attempt will have to move — the source on a fresh move, the destination on a
// resume (where the source object is already gone) — so verification can tell
// "the sweep never folded this repo" (no row is fine) from "the rewrite missed"
// (a row went missing). The source row must be absent either way.
func (s *Service) transferSnapshotCatalog(ctx context.Context, st *transferState) error {
	cat, err := sizecatalog.ReadCatalog(ctx, s.Store)
	if err != nil {
		return err
	}
	probe := st.req.SourceID()
	if st.resume {
		probe = st.req.DestinationID()
	}
	for _, r := range cat.Repos {
		if r == probe {
			st.catalogHadName = true
		}
	}
	for _, e := range cat.Entries {
		if e != nil && e.Repo == probe {
			st.catalogHadEntry = true
		}
	}
	return nil
}

// transferSnapshotPackages remembers the source registry keys so verification
// can prove the OCI namespace followed the prefix (a package tag that silently
// vanished is a broken pull path, not a cosmetic miss).
func (s *Service) transferSnapshotPackages(ctx context.Context, st *transferState) error {
	prefix := st.srcKey("packages/registry/")
	var keys []string
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		keys = append(keys, strings.TrimPrefix(m.Key, st.srcPrefix))
		return nil
	}); err != nil {
		return err
	}
	st.packageKeys = keys
	return nil
}

// quiesce tears down the serving copy for one side (nil seam = nothing to do).
func (s *Service) quiesce(ctx context.Context, owner, repo string, n Narrator) {
	if s.Serving == nil {
		return
	}
	if err := s.Serving.Invalidate(owner + "/" + repo); err != nil {
		n.Notice("serving copy flush for " + owner + "/" + repo + " failed: " + err.Error())
	}
}

// transferMovePass is the shipped copy-then-delete, made re-runnable:
//
//	LIST → copy every non-manifest key (adopt byte-identical leftovers) →
//	re-read the source manifest and abort on a mid-move push → copy the
//	manifest (the commit point) → delete the source manifest first, then the
//	rest → LIST the source again and report any residue.
func (s *Service) transferMovePass(ctx context.Context, n Narrator, st *transferState) error {
	keys, err := s.listPrefix(ctx, st.srcPrefix)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return fmt.Errorf("%w: unknown repo %q (manifest vanished)", ErrNotFound, st.req.SourceID())
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	srcManifestKey := st.srcKey(manifestName)
	var body []string
	manifestSeen := false
	for _, k := range keys {
		if k == srcManifestKey {
			manifestSeen = true
			continue
		}
		body = append(body, k)
	}
	if !manifestSeen {
		return fmt.Errorf("%w: unknown repo %q (manifest vanished)", ErrNotFound, st.req.SourceID())
	}
	// Rewritten families are buffered so an adopted copy can be compared; every
	// other object streams (never whole packs in memory).
	for i, key := range body {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := strings.TrimPrefix(key, st.srcPrefix)
		var want []byte
		switch rel {
		case "access.json":
			raw, _, gerr := store.GetBytes(ctx, s.Store, key, store.GetOptions{})
			if gerr != nil {
				return gerr
			}
			want = s.rewriteTransferAccess(ctx, raw, st.req.SrcOwner, st.req.DstOwner, s.nowUTC().Format(time.RFC3339))
			if want == nil {
				want = raw // corrupt access.json moves byte-identical
			}
		}
		if err := s.putTransferObject(ctx, key, st.dstKey(rel), want, st); err != nil {
			return err
		}
		n.Progress("objects", uint64(i+1), uint64(len(body)), "objects")
	}
	// The manifest barrier: a push that landed after our snapshot would make
	// the destination's manifest describe objects we never copied.
	cur, _, err := store.GetBytes(ctx, s.Store, srcManifestKey, store.GetOptions{})
	if err != nil {
		return err
	}
	if !bytes.Equal(cur, st.srcManifest) {
		return fmt.Errorf("%w: repo changed during transfer; retry once the push settles", ErrConflict)
	}
	want := s.rewriteTransferManifest(st.srcManifest, st.req.DestinationID())
	if want == nil {
		want = st.srcManifest
	}
	if err := s.putTransferObject(ctx, srcManifestKey, st.dstKey(manifestName), want, st); err != nil {
		return err
	}
	st.committed = true
	n.Notice("move committed at " + st.req.DestinationID() + " (" + fmt.Sprint(len(body)) + " objects)")

	// Delete pass: manifest first (authority transfers), then the objects.
	if err := s.Store.Delete(ctx, srcManifestKey, ""); err != nil && !store.IsNotFound(err) {
		return err
	}
	for i, key := range body {
		if derr := s.Store.Delete(ctx, key, ""); derr != nil && !store.IsNotFound(derr) {
			return derr
		}
		n.Progress("cleanup", uint64(i+1), uint64(len(body)), "objects")
	}
	return s.transferResidueCheck(ctx, st)
}

// transferResidue is the resume path's cleanup: every key still under the
// source prefix must be reproduced at the destination before it is deleted (a
// push that raced a finished move is reported, never silently dropped).
func (s *Service) transferResidue(ctx context.Context, n Narrator, st *transferState) error {
	keys, err := s.listPrefix(ctx, st.srcPrefix)
	if err != nil {
		return err
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, key := range keys {
		rel := strings.TrimPrefix(key, st.srcPrefix)
		equal, cerr := s.objectsEqual(ctx, key, st.dstKey(rel))
		if cerr != nil {
			return cerr
		}
		if !equal {
			return fmt.Errorf("%w: %s was written after the move committed and is not reproduced at %q; nothing was deleted",
				ErrConflict, key, st.req.DestinationID())
		}
	}
	st.committed = true
	if len(keys) > 0 {
		n.Notice("deleting " + fmt.Sprint(len(keys)) + " reproduced source residue object(s)")
	}
	for i, key := range keys {
		if derr := s.Store.Delete(ctx, key, ""); derr != nil && !store.IsNotFound(derr) {
			return derr
		}
		n.Progress("cleanup", uint64(i+1), uint64(len(keys)), "objects")
	}
	return s.transferResidueCheck(ctx, st)
}

// transferResidueCheck is the honest leftover report: after the delete pass a
// key that is still there was written concurrently (the destination is already
// complete). A retry converges once the residue is removed.
func (s *Service) transferResidueCheck(ctx context.Context, st *transferState) error {
	var leftover int
	if err := s.Store.List(ctx, st.srcPrefix, "", func(m store.ObjectMeta) error {
		leftover++
		return nil
	}); err != nil {
		return err
	}
	if leftover > 0 {
		return fmt.Errorf("%w: repo changed during transfer; %d object(s) remain at %q (destination complete)",
			ErrConflict, leftover, st.req.SourceID())
	}
	return nil
}

// putTransferObject writes one destination object Create-only, adopting a
// byte-identical leftover from an interrupted attempt. want != nil is the exact
// rewritten body (access.json, manifest.pb); want == nil streams the source.
func (s *Service) putTransferObject(ctx context.Context, srcKey, dstKey string, want []byte, st *transferState) error {
	if want != nil {
		_, err := store.PutBytes(ctx, s.Store, dstKey, want,
			store.PutOptions{Mode: store.PutCreate, ContentType: transferContentType(dstKey)})
		if err == nil {
			st.created = append(st.created, dstKey)
			return nil
		}
		if !store.IsPreconditionFailed(err) {
			return err
		}
		existing, _, gerr := store.GetBytes(ctx, s.Store, dstKey, store.GetOptions{})
		if gerr != nil {
			return gerr
		}
		if bytes.Equal(existing, want) {
			return nil // a prior attempt's copy: adopt it
		}
		return fmt.Errorf("%w: repo already exists at %q (%s differs)", ErrConflict, dstKey, dstKey)
	}
	err := s.copyObject(ctx, srcKey, dstKey)
	if err == nil {
		st.created = append(st.created, dstKey)
		return nil
	}
	if !store.IsPreconditionFailed(err) {
		return err
	}
	equal, cerr := s.objectsEqual(ctx, srcKey, dstKey)
	if cerr != nil {
		return cerr
	}
	if equal {
		return nil // a prior attempt's copy: adopt it
	}
	return fmt.Errorf("%w: repo already exists at %q", ErrConflict, dstKey)
}

// transferCleanup deletes the destination keys THIS attempt created when the
// move failed before its commit point (never after: a completed destination is
// not rolled back).
func (s *Service) transferCleanup(ctx context.Context, st *transferState) {
	if st.committed {
		return
	}
	for _, key := range st.created {
		_ = s.Store.Delete(ctx, key, "")
	}
}

// ensureTransferAccess materializes the destination's synthesized admin
// binding when the source had no access.json, so a user new-owner holds exactly
// what creation would have written. Org destinations get none: org-owner
// resolution covers them, and a user:<orgslug> binding would be a latent grant.
// Deterministic bytes, so a retry adopts instead of overwriting.
func (s *Service) ensureTransferAccess(ctx context.Context, st *transferState) error {
	if st.srcHadAccess || !s.isUserNamespace(ctx, st.req.DstOwner) {
		return nil
	}
	def := s.SynthesizeOwner(ctx, st.req.DstOwner)
	def.Version = 1
	return s.putTransferObject(ctx, "", st.dstKey("access.json"), encodeAccess(def), st)
}

// transferCrossReferences fixes every family outside the moved prefix, in the
// order the plan names: catalog, fork child row, fork parent pointers, packages
// bookkeeping.
func (s *Service) transferCrossReferences(ctx context.Context, n Narrator, st *transferState) error {
	src, dst := st.req.SourceID(), st.req.DestinationID()

	// Catalog row: CAS, carries size/activity verbatim. A miss is bounded
	// staleness the sweep heals, but T1 treats it as a task failure: a terminal
	// "ok" that still publishes the old row is a worse lie than a failed task
	// that a retry or the sweep repairs.
	moved, err := sizecatalog.RenameRow(ctx, s.Store, src, dst)
	if err != nil {
		return fmt.Errorf("%w: catalog row for %s: %v", ErrConflict, src, err)
	}
	if moved {
		n.Notice("catalog row moved " + src + " -> " + dst)
	}

	if s.Forks != nil {
		parent, ok, perr := s.Forks.ParentOf(ctx, st.req.DstOwner, st.req.DstRepo)
		if perr != nil {
			return fmt.Errorf("%w: fork provenance of %s: %v", ErrConflict, dst, perr)
		}
		if ok && parent != dst {
			po, pr, split := splitTransferID(parent)
			if !split {
				return fmt.Errorf("%w: fork parent %q of %s is not an owner/name id", ErrConflict, parent, dst)
			}
			changed, cerr := s.Forks.ChildMoved(ctx, po, pr, src, dst)
			if cerr != nil {
				return fmt.Errorf("%w: fork index of %s: %v", ErrConflict, parent, cerr)
			}
			if changed {
				n.Notice("fork row moved in " + parent + "'s index")
			}
		}
		children, merr := s.Forks.ParentMoved(ctx, src, dst)
		if merr != nil {
			return fmt.Errorf("%w: fork pointers of %s's children: %v", ErrConflict, src, merr)
		}
		if children > 0 {
			n.Notice("rewrote " + fmt.Sprint(children) + " fork child pointer(s)")
		}
	}

	if s.Packages != nil {
		s.Packages.RepoMoved(st.req.SrcOwner, st.req.SrcRepo, st.req.DstOwner, st.req.DstRepo)
		n.Notice("OCI registry bookkeeping invalidated for both names (image digests survive; the namespace is the new name)")
	}
	return nil
}

// transferVerify is the loud door: the destination must be openable, the source
// gone, and every rewritten family consistent — each failure names its piece.
func (s *Service) transferVerify(ctx context.Context, n Narrator, st *transferState) error {
	src, dst := st.req.SourceID(), st.req.DestinationID()
	raw, found, err := s.readOptional(ctx, st.dstKey(manifestName))
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: destination manifest missing at %q", ErrConflict, dst)
	}
	m, merr := proto.UnmarshalManifest(raw)
	if merr != nil {
		return fmt.Errorf("%w: destination manifest at %q does not decode: %v", ErrConflict, dst, merr)
	}
	if m.Repo != dst {
		return fmt.Errorf("%w: destination manifest at %q names repo %q", ErrConflict, dst, m.Repo)
	}
	if left, lerr := store.Exists(ctx, s.Store, st.srcKey(manifestName)); lerr != nil {
		return lerr
	} else if left {
		return fmt.Errorf("%w: source manifest still present at %q", ErrConflict, src)
	}
	if s.Serving != nil {
		if verr := s.Serving.VerifyRepo(ctx, dst); verr != nil {
			return fmt.Errorf("%w: destination refs unreadable at %q: %v", ErrConflict, dst, verr)
		}
	}
	cat, cerr := sizecatalog.ReadCatalog(ctx, s.Store)
	if cerr != nil {
		return cerr
	}
	hasNew := false
	for _, e := range cat.Entries {
		if e == nil {
			continue
		}
		if e.Repo == src {
			return fmt.Errorf("%w: catalog still names %q", ErrConflict, src)
		}
		if e.Repo == dst {
			hasNew = true
		}
	}
	if st.catalogHadName || st.catalogHadEntry {
		for _, r := range cat.Repos {
			if r == dst {
				hasNew = true
			}
		}
		if !hasNew {
			return fmt.Errorf("%w: catalog has no row for %q (the sweep will heal a miss later, but the move is not verified)", ErrConflict, dst)
		}
	}
	for _, rel := range st.packageKeys {
		if ok, perr := store.Exists(ctx, s.Store, st.dstKey(rel)); perr != nil {
			return perr
		} else if !ok {
			return fmt.Errorf("%w: package object %q did not follow the move to %q", ErrConflict, rel, dst)
		}
	}
	if s.Forks != nil {
		if ferr := s.Forks.VerifyMoved(ctx, src, dst); ferr != nil {
			return fmt.Errorf("%w: fork pointers inconsistent after the move: %v", ErrConflict, ferr)
		}
	}
	n.Notice("verified: manifest, refs, catalog, packages and fork pointers at " + dst)
	return nil
}

// --- small helpers -------------------------------------------------------------

// listPrefix lists every key under prefix (a human-rate admin path — never a
// hot path).
func (s *Service) listPrefix(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		keys = append(keys, m.Key)
		return nil
	}); err != nil {
		return nil, err
	}
	return keys, nil
}

// readOptional GETs a key and reports absence without erroring.
func (s *Service) readOptional(ctx context.Context, key string) ([]byte, bool, error) {
	body, _, err := store.GetBytes(ctx, s.Store, key, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if body == nil {
		return nil, false, nil
	}
	return body, true, nil
}

// objectsEqual streams two objects and compares them byte-for-byte (the adopt
// test on the 409/412 path — never buffers a pack).
func (s *Service) objectsEqual(ctx context.Context, a, b string) (bool, error) {
	ra, err := s.Store.Get(ctx, a, store.GetOptions{})
	if err != nil {
		return false, err
	}
	oa, ok := ra.(store.Object)
	if !ok {
		return false, fmt.Errorf("identity: unexpected GetResult for %s", a)
	}
	defer oa.Body.Close()
	rb, err := s.Store.Get(ctx, b, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	ob, ok := rb.(store.Object)
	if !ok {
		return false, fmt.Errorf("identity: unexpected GetResult for %s", b)
	}
	defer ob.Body.Close()
	if oa.Meta.Size != ob.Meta.Size {
		return false, nil
	}
	bufa := make([]byte, 32<<10)
	bufb := make([]byte, 32<<10)
	for {
		na, ea := io.ReadFull(oa.Body, bufa)
		nb, eb := io.ReadFull(ob.Body, bufb)
		if na != nb || !bytes.Equal(bufa[:na], bufb[:nb]) {
			return false, nil
		}
		aDone := ea == io.EOF || ea == io.ErrUnexpectedEOF
		bDone := eb == io.EOF || eb == io.ErrUnexpectedEOF
		if aDone || bDone {
			return aDone && bDone, nil
		}
		if ea != nil {
			return false, ea
		}
		if eb != nil {
			return false, eb
		}
	}
}

// splitTransferID splits "owner/name" strictly.
func splitTransferID(id string) (owner, repo string, ok bool) {
	i := strings.IndexByte(id, '/')
	if i <= 0 || i == len(id)-1 {
		return "", "", false
	}
	return id[:i], id[i+1:], true
}

// rewriteTransferManifest retargets the moved manifest's Repo field at the
// destination. It is load-bearing, not cosmetic: wal.openOrInitLocal derives
// the serving-copy directory from Manifest.Repo, so an unrewritten manifest
// materializes the OLD owner's local dir for the new path. Everything else
// (revision, head_seq, packs, settings) is a move, not an edit, and travels
// untouched. Returns nil when the bytes do not decode (the caller carries them
// opaquely and the verification step fails the task later).
func (s *Service) rewriteTransferManifest(raw []byte, dstID string) []byte {
	if len(raw) == 0 {
		return nil
	}
	m, err := proto.UnmarshalManifest(raw)
	if err != nil {
		return nil
	}
	if m.Repo == dstID {
		return append([]byte(nil), raw...)
	}
	m.Repo = dstID
	return m.Marshal()
}

// --- shipped primitives (unchanged semantics) ---------------------------------

// copyObject streams one object to a destination key that must not exist
// (PutCreate). The content type restores the writer convention readers never
// consult (sidecars like the release asset entries carry serving MIME; object
// metadata is write-only) — .json/.pb keep their types, all other bytes travel
// as octet-stream.
func (s *Service) copyObject(ctx context.Context, srcKey, dstKey string) error {
	res, err := s.Store.Get(ctx, srcKey, store.GetOptions{})
	if err != nil {
		return err
	}
	obj, ok := res.(store.Object)
	if !ok {
		return fmt.Errorf("identity: unexpected GetResult for %s", srcKey)
	}
	defer obj.Body.Close()
	_, err = s.Store.Put(ctx, dstKey,
		store.PutBody{Stream: obj.Body, StreamLen: obj.Meta.Size},
		store.PutOptions{Mode: store.PutCreate, ContentType: transferContentType(dstKey)})
	return err
}

// validRepoID reports whether owner/repo is a bucket-legal repo id: the
// git charset (ParseRepoId), or an email owner for user namespaces (@ is
// outside the git charset but bucket-legal — user repos live under
// repos/<email>/<repo>/) with a git-legal repo name.
func validRepoID(owner, repo string) bool {
	if owner == "" || repo == "" {
		return false
	}
	if _, err := git.ParseRepoId(owner + "/" + repo); err == nil {
		return true
	}
	if !ValidPrincipal(owner) {
		return false
	}
	_, err := git.ParseRepoId("owner/" + repo)
	return err == nil
}

// transferContentType restores the writer content-type convention for a
// moved key (see copyObject).
func transferContentType(key string) string {
	switch {
	case strings.HasSuffix(key, ".json"):
		return "application/json"
	case strings.HasSuffix(key, ".pb"):
		return "application/x-protobuf"
	default:
		return "application/octet-stream"
	}
}

// rewriteTransferAccess moves an access.json across owners: visibility and
// every binding survive untouched, except the owner-subject —
// user:<srcOwner> bindings are dropped (the seller keeps no admin), and a
// user:<dstOwner> admin binding is added when the destination is a user
// namespace without one (org destinations need none: org-owner resolution
// covers them, and a user:<orgslug> binding would be a latent grant to
// whoever later claims that username). team: subjects name teams that
// still exist and move untouched. Returns nil when raw does not parse
// (the caller carries the bytes opaquely); the moved doc keeps its
// version (a move, not an edit) with a fresh timestamp.
func (s *Service) rewriteTransferAccess(ctx context.Context, raw []byte, srcOwner, dstOwner, now string) []byte {
	doc, err := parseAccess(raw)
	if err != nil {
		return nil
	}
	var kept []AccessBinding
	if ValidPrincipal(srcOwner) && !s.orgExists(ctx, srcOwner) {
		want := "user:" + normPrincipal(srcOwner)
		for _, b := range doc.RoleBindings {
			if strings.ToLower(b.Subject) == want {
				continue
			}
			kept = append(kept, b)
		}
	} else {
		kept = doc.RoleBindings
	}
	if s.isUserNamespace(ctx, dstOwner) {
		want := "user:" + normPrincipal(dstOwner)
		found := false
		for _, b := range kept {
			if strings.ToLower(b.Subject) == want {
				found = true
				break
			}
		}
		if !found {
			kept = append(kept, AccessBinding{Subject: want, Role: RoleAdmin})
		}
	}
	doc.RoleBindings = kept
	doc.UpdatedAt = now
	return encodeAccess(doc)
}

// errTransferDrained is the phase-1 drain terminal for a move (law 7: honest,
// retryable, never a bare context.Canceled).
var errTransferDrained = errors.New("transfer interrupted: instance is draining; safe to retry")
