package pulls

import (
	"context"
	"errors"
	"fmt"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// This file owns §4 (mergeability: the stamped derived cache, the `pulls`
// event sink, the `pull-mergeable` task) plus the diff/commits read paths.

// loadMergeable reads a mergeable.json cache; (nil, "", nil) when absent.
func (s *Service) loadMergeable(ctx context.Context, owner, repo string, num int) (*MergeableDoc, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, MergeableKey(owner, repo, num))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return nil, "", nil
	}
	m, perr := parseMergeable(raw)
	if perr != nil {
		return nil, "", perr
	}
	return m, ver, nil
}

// unknownMergeable renders the stale-cache answer (§4: stamp mismatch ⇒
// state:"unknown" + enqueued recompute; neither blocks a read on git).
func unknownMergeable(pr *PRDoc, at string) *MergeableDoc {
	return &MergeableDoc{
		BaseRef: pr.Base.Ref, BaseSHA: pr.Base.SHA, HeadSHA: pr.Head.SHA,
		MergeBase: "", State: MergeableUnknown,
		Conflicts: []string{}, Rebaseable: true, ComputedAt: at,
	}
}

// saveMergeable CAS-writes a mergeable.json cache (derived state — losing
// the race only costs a recompute: a 412 loser re-runs after the winner and
// converges). Bounded at 5 attempts.
func (s *Service) saveMergeable(ctx context.Context, owner, repo string, num int, m *MergeableDoc) error {
	_, err := s.casUpdate(ctx, MergeableKey(owner, repo, num), 5, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		return encodeMergeable(m), true, nil
	})
	return err
}

// ComputeMergeable recomputes the mergeability stamp (§4) with the doc-04
// machinery: ancestry (`merge-base --is-ancestor` both directions), the
// trial merge (`merge-tree --write-tree --name-only`), and behind
// (`rev-list --count head..base`). All git runs go through the bounded
// per-repo pool (never bare on request goroutines). Recompute goes through
// the in-process single-flight keyed "mergeable:"+repo+"/"+num — joiners get
// the same result (bounded join). The result is CAS-written with the fresh
// stamp; no lock is held across any store call or git subprocess.
//
// Cost model (EVIDENCE.md E4): 0 LIST, 2 ref resolves (pool-gated) + 1
// merge-base + 1 merge-tree + 1 rev-list --count, 2 bucket GETs + 1 CAS PUT.
func (s *Service) ComputeMergeable(ctx context.Context, owner, repo string, num int) (*MergeableDoc, error) {
	if s.Git == nil || s.Dirs == nil {
		return nil, fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	v, err := s.flight.Do(ctx, "mergeable:"+repoName(owner, repo)+"/"+itoa(num), func() (any, error) {
		return s.computeMergeable(ctx, owner, repo, num)
	})
	if err != nil {
		return nil, err
	}
	m, ok := v.(*MergeableDoc)
	if !ok || m == nil {
		return nil, fmt.Errorf("%w: mergeable recompute produced no result", ErrCorrupt)
	}
	return m, nil
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// computeMergeable is the single-flight leader body.
func (s *Service) computeMergeable(ctx context.Context, owner, repo string, num int) (*MergeableDoc, error) {
	th, _, err := s.loadThread(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	if th == nil || th.Kind != "pr" {
		return nil, fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	pr, _, err := s.loadPR(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	if pr.Merged || th.State == StateClosed {
		m := &MergeableDoc{
			BaseRef: pr.Base.Ref, BaseSHA: pr.Base.SHA, HeadSHA: pr.Head.SHA,
			MergeBase: "", State: MergeableUpToDate,
			Conflicts: []string{}, Rebaseable: true, ComputedAt: s.nowUTC().Format(dateTimeFmt),
		}
		_ = s.saveMergeable(ctx, owner, repo, num, m)
		return m, nil
	}
	baseDir, err := s.Dirs.Dir(ctx, pr.Base.Repo)
	if err != nil {
		return nil, fmt.Errorf("%w: base repo unavailable: %v", ErrUnavailable, err)
	}
	headDir, err := s.Dirs.Dir(ctx, pr.Head.Repo)
	if err != nil {
		return nil, fmt.Errorf("%w: head repo unavailable: %v", ErrUnavailable, err)
	}
	// Live shas: the stamp triple (base_ref, base_sha, head_sha) IS the
	// invalidation key (§2.2).
	baseLive, err := s.Git.ResolveRef(ctx, baseDir, pr.Base.Ref)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown base revision %q", ErrUnprocessable, pr.Base.Ref)
	}
	headLive, err := s.Git.ResolveRef(ctx, headDir, pr.Head.Ref)
	if err != nil {
		m := unknownMergeable(pr, s.nowUTC().Format(dateTimeFmt))
		m.BaseSHA, m.HeadSHA = baseLive, pr.Head.SHA
		return m, nil
	}
	if headLive != pr.Head.SHA {
		s.reconcileHead(ctx, owner, repo, pr, th, headLive)
		if npr, _, nerr := s.loadPR(ctx, owner, repo, num); nerr == nil && npr != nil {
			pr = npr
		}
	}
	// Fork→base object bridge (issue #456): merge-base and the trial merge
	// run in baseDir, so fork-unique head objects are fetched from the
	// fork serving copy first (local disk-to-disk, zero bucket trips).
	// Same-repo heads need nothing (one object set). A bridge failure
	// fails the recompute loud — mergeability without objects is a
	// verdict no code path may guess.
	if pr.Head.Repo != pr.Base.Repo {
		if ferr := s.Git.FetchInto(ctx, baseDir, headDir, headLive); ferr != nil {
			return nil, fmt.Errorf("%w: fork object bridge: %v", ErrUnavailable, ferr)
		}
	}
	// Ancestry: head fully merged into base (head is an ancestor of base)
	// ⇒ up_to_date, nothing to merge. (The base-contained-in-head
	// direction is the NORMAL PR shape — head branched off base — and
	// proceeds to the trial merge below; see the §4 deviation note in
	// docs/features/03: the first ancestry parenthetical there would refuse
	// every ordinary PR, so the implementation follows the second clause,
	// which names the genuinely terminal condition.)
	merged, err := s.Git.IsAncestor(ctx, baseDir, headLive, baseLive)
	if err != nil {
		return nil, err
	}
	now := s.nowUTC().Format(dateTimeFmt)
	if merged {
		m := &MergeableDoc{BaseRef: pr.Base.Ref, BaseSHA: baseLive, HeadSHA: headLive,
			MergeBase: headLive, State: MergeableUpToDate,
			Conflicts: []string{}, Rebaseable: true, ComputedAt: now}
		_ = s.saveMergeable(ctx, owner, repo, num, m)
		return m, nil
	}
	mb, err := s.Git.MergeBase(ctx, baseDir, baseLive, headLive)
	if err != nil {
		return nil, err
	}
	tree, conflicts, terr := s.Git.TrialMerge(ctx, baseDir, baseLive, headLive)
	_ = tree
	m := &MergeableDoc{BaseRef: pr.Base.Ref, BaseSHA: baseLive, HeadSHA: headLive,
		MergeBase: mb, Conflicts: []string{}, Rebaseable: true, ComputedAt: now}
	if terr == nil {
		behind, berr := s.Git.BehindCount(ctx, baseDir, baseLive, headLive)
		if berr != nil {
			return nil, berr
		}
		if behind > 0 {
			m.State = MergeableBehind
		} else {
			m.State = MergeableClean
		}
	} else if errors.Is(terr, errDirty) {
		m.State = MergeableDirty
		m.Conflicts = nonNilStr(conflicts)
		m.Rebaseable = len(conflicts) == 0
	} else {
		return nil, terr
	}
	_ = s.saveMergeable(ctx, owner, repo, num, m)
	return m, nil
}

// HeadMove is one authoritative head transition (§2.3): the sha a path wants
// to record, and — for a WAL ref event — the event's own ordering evidence.
// Seq is the log sequence of the event that produced the move; Old is the
// event's pre-image (the ref value it moved away from); Event marks an ordered
// delivery (only an event may apply a non-fast-forward rewind). A move with
// neither ordering signal (a live ref read, or a publish this process just
// landed) is applied forward-only.
type HeadMove struct {
	New   string
	Old   string
	Seq   uint64
	Event bool
}

// refreshHead records head-sha drift from a publish this process just landed
// or a live read. It carries no ordering evidence, so it is forward-only.
func (s *Service) refreshHead(ctx context.Context, owner, repo string, pr *PRDoc, th *Thread, headLive string, actor auth.Principal) bool {
	return s.refreshHeadFrom(ctx, owner, repo, pr, th, HeadMove{New: headLive}, actor)
}

// reconcileHead aligns pr.json with a *live ref read* taken during a
// mergeability recompute. That read is a cache read (a serving copy may lag
// the WAL event that recorded the current head), so it is applied
// forward-only: the recorded head is replaced only when the live sha
// descends from it. A lagging read that points back at an ancestor — or
// diverges — is ignored; the ordered event path (applyHeadEvent) stays
// authoritative. Without this, a recompute that read the pre-push sha after
// the sink recorded the push wrote the head back and every later read served
// the stale sha (TestSink `head push refreshes sha`).
func (s *Service) reconcileHead(ctx context.Context, owner, repo string, pr *PRDoc, th *Thread, headLive string) bool {
	return s.refreshHeadFrom(ctx, owner, repo, pr, th, HeadMove{New: headLive}, auth.Principal{})
}

// applyHeadEvent applies ONE WAL ref event's head move. The event carries its
// own ordering, which is what makes a late delivery harmless: a duplicate or
// an older event (seq at or below the applied watermark) never moves the head,
// and a move that is not a fast-forward is applied only when the event's
// pre-image IS the head we hold (a real force-push/rewind — never a replay of
// an older state). Returns true only when the head actually moved, which is
// also the gate for the pull_request "synchronize" trigger.
func (s *Service) applyHeadEvent(ctx context.Context, owner, repo string, pr *PRDoc, th *Thread, mv HeadMove, actor auth.Principal) bool {
	return s.refreshHeadFrom(ctx, owner, repo, pr, th, mv, actor)
}

// refreshHeadFrom is the shared owned-delta writer for Head.SHA +
// HeadForcePushedAt + HeadSeq (§2.3). It applies the delta through updatePR, so
// the freshest doc is re-loaded and only these fields are set on every
// attempt: a concurrent merge/body write is never reverted, and two racing
// head refreshes converge instead of the loser restoring the old sha.
//
// Ordering rule (never wall clock): a ref-event move is rejected when its log
// sequence is not newer than HeadSeq (the watermark of the last applied
// event); a non-fast-forward move is rejected unless the event's own old sha
// is the head we hold. A move with neither ordering signal (a live read or a
// local publish) is accepted only when it descends from the recorded head.
func (s *Service) refreshHeadFrom(ctx context.Context, owner, repo string, pr *PRDoc, th *Thread, mv HeadMove, actor auth.Principal) bool {
	headLive := mv.New
	if headLive == "" {
		return false
	}
	now := s.nowUTC().Format(dateTimeFmt)
	var (
		old    string
		forced bool
		wrote  bool
	)
	updated, err := s.updatePR(ctx, owner, repo, pr.Num, func(cur *PRDoc) (bool, error) {
		wrote, forced = false, true
		if headLive == cur.Head.SHA {
			// Same head. A newer event still advances the watermark (so an
			// older delivery can never rewind it later); nothing is narrated.
			if mv.Seq > cur.HeadSeq {
				cur.HeadSeq = mv.Seq
				return true, nil
			}
			return false, nil // another writer already recorded it
		}
		if mv.Seq != 0 && cur.HeadSeq != 0 && mv.Seq <= cur.HeadSeq {
			return false, nil // a delivery older than the head's applied event
		}
		if cur.Head.SHA != "" && s.Git != nil && s.Dirs != nil {
			forward := false
			if dir, derr := s.Dirs.Dir(ctx, cur.Head.Repo); derr == nil {
				if fwd, ferr := s.Git.IsAncestor(ctx, dir, cur.Head.SHA, headLive); ferr == nil && fwd {
					forward = true
				}
			}
			if !forward {
				// A rewind/divergence. Only a genuine ordered event may apply
				// it, and only when its pre-image IS the head we hold (the ref
				// moved FROM here — the event's own ordering, never a clock),
				// with a sequence that is not stale (checked above).
				if !mv.Event || mv.Old == "" || mv.Old != cur.Head.SHA {
					return false, nil
				}
			}
		}
		wrote = true
		old = cur.Head.SHA
		if s.Git != nil && s.Dirs != nil && old != "" {
			if dir, derr := s.Dirs.Dir(ctx, cur.Head.Repo); derr == nil {
				if ff, ferr := s.Git.IsAncestor(ctx, dir, old, headLive); ferr == nil && ff {
					forced = false
				}
			}
		}
		cur.Head.SHA = headLive
		if mv.Seq > cur.HeadSeq {
			cur.HeadSeq = mv.Seq
		}
		if forced {
			cur.HeadForcePushedAt = &now
		}
		return true, nil
	})
	if err != nil || updated == nil {
		return false
	}
	// Keep the caller's snapshot in step with what was committed (callers
	// reload or read head fields from it after this returns).
	pr.Head.SHA = updated.Head.SHA
	pr.HeadForcePushedAt = updated.HeadForcePushedAt
	pr.HeadSeq = updated.HeadSeq
	if !wrote {
		return false // converged elsewhere: no delta, nothing to narrate
	}
	if !forced {
		// Fast-forward: no force-push evidence/event, but the recompute is
		// still owed (the head moved).
		s.updateIndex(ctx, owner, repo, prCardOf(th))
		s.enqueueMergeable(ctx, owner, repo, pr.Num)
		return true
	}
	who := normPrincipal(actor.Name)
	if who == "" {
		who = "pulls-sink"
	}
	_, _, _ = s.appendEvent(ctx, owner, repo, pr.Num, func(t *Thread, seq int) (*Event, error) {
		t.NextEventSeq = seq + 1
		t.UpdatedAt = now
		t.Participants = uniqSorted(append(t.Participants, who))
		t.Version++
		return &Event{Seq: seq, Type: EventHeadForcePushed, Actor: who, At: now, From: strPtr(old), To: strPtr(headLive)}, nil
	})
	if nth, _, nerr := s.loadThread(ctx, owner, repo, pr.Num); nerr == nil && nth != nil {
		*th = *nth
	}
	s.stream(ctx, StreamEvent{Name: "pull", Repo: repoName(owner, repo), Action: "head_force_pushed", Num: pr.Num, Title: th.Title, State: th.State, Author: th.Author, BaseRef: pr.Base.Ref, HeadRef: pr.Head.Ref, HeadSHA: headLive})
	s.updateIndex(ctx, owner, repo, prCardOf(th))
	s.enqueueMergeable(ctx, owner, repo, pr.Num)
	return true
}

// enqueueMergeable schedules a `pull-mergeable` recompute for one PR without
// blocking the reader: a detached background pass under the (repo, kind)
// task join (a second enqueue joins the running pass and reuses its
// outcome). Both paths converge; neither blocks a read on git.
func (s *Service) enqueueMergeable(ctx context.Context, owner, repo string, num int) {
	repoID := repoName(owner, repo)
	entry, joined := s.tasks.begin(repoID, TaskKindMergeable)
	entry.attach(num)
	if joined {
		return
	}
	go func() {
		defer s.tasks.end(repoID, TaskKindMergeable)
		bctx := context.WithoutCancel(ctx)
		for _, n := range entry.drain() {
			_, _ = s.ComputeMergeable(bctx, owner, repo, n)
		}
	}()
}

// --- the `pulls` event sink (§4 invalidation seam) -----------------------------

// RefEvent is one WAL ref event as the sink consumes it (Seam 4 shape,
// reduced to the fields the invalidation needs). Seq is the WAL log sequence
// (`_walgit.seq`) — the event's own ordering, which is what lets a late or
// replayed delivery be recognised as stale instead of rewinding a PR head.
type RefEvent struct {
	Repo    string // "owner/name"
	RefName string
	Old     string
	New     string
	Seq     uint64
	// ByJob marks a ref moved by a Threads job token (a `contents: write`
	// job's own push): the PR head still refreshes, but no `synchronize`
	// trigger follows — GitHub's GITHUB_TOKEN rule (15 §5). The marker is
	// derived by composition from the WAL event's pusher ("ci:<runner>"), so
	// this package never learns the Threads naming convention.
	ByJob bool
}

// HandleRefEvent consumes one WAL ref event (Seam 4 `pulls` sink, per-sink
// cursor repos/<o>/<r>/events/cursors/pulls.json owned by internal/events).
// For each event whose ref_name matches an open PR's base.ref or head.ref
// (looked up from the shared issues/index.json filtered to kind:"pr" — the
// index is authoritative for open items per P4), the PR num is enqueued onto
// the recompute batch (task kind pull-mergeable, (repo, kind) single-flight
// batches all dirty PRs of the repo into one pass). Head drift also runs
// the §6 refresh (force-push evidence + sha snapshot), forward-only by the
// event's own sequence, and only an APPLIED move fires the CI `synchronize`
// trigger — a duplicate or stale delivery creates no second run.
//
// Cost: 1 index GET + 1 GET per open PR sidecar (index-authoritative, no
// LIST); git only on the recompute path (pool-gated).
func (s *Service) HandleRefEvent(ctx context.Context, owner, repo string, ev RefEvent) {
	ix, _, err := s.loadIndex(ctx, owner, repo)
	if err != nil {
		return
	}
	var dirty []int
	for _, c := range ix.Open {
		if c.Kind != "pr" {
			continue
		}
		pr, _, perr := s.loadPR(ctx, owner, repo, c.Num)
		if perr != nil || pr == nil || pr.Merged {
			continue
		}
		matched := false
		if pr.Base.Repo == ev.Repo && pr.Base.Ref == ev.RefName {
			matched = true
		}
		if pr.Head.Repo == ev.Repo && pr.Head.Ref == ev.RefName {
			matched = true
			if ev.New != "" {
				th, _, terr := s.loadThread(ctx, owner, repo, pr.Num)
				if terr == nil && th != nil {
					moved := s.applyHeadEvent(ctx, owner, repo, pr, th,
						HeadMove{New: ev.New, Old: ev.Old, Seq: ev.Seq, Event: true}, auth.Principal{})
					// CI trigger (Threads pass A): an APPLIED new head
					// commit on an open PR is a pull_request
					// "synchronize". A duplicate delivery (same head) or
					// a stale one (older seq, or a non-fast-forward whose
					// pre-image is not our head) moved nothing and must
					// not start a run for a sha the PR has left behind.
					// A job-token push (ByJob) never synchronizes.
					if moved && !ev.ByJob {
						// The applied head is the event's new sha; the helper's
						// pr snapshot may still name the pre-image, so deliver
						// the event's sha explicitly (the same sha the run builds).
						pr.Head.SHA = ev.New
						s.emitPullLifecycle(ctx, repoName(owner, repo), th, pr, "synchronize", "push")
					}
				}
			}
		}
		if matched {
			dirty = append(dirty, pr.Num)
		}
	}
	if len(dirty) == 0 {
		return
	}
	repoID := repoName(owner, repo)
	entry, joined := s.tasks.begin(repoID, TaskKindMergeable)
	for _, n := range dirty {
		entry.attach(n)
	}
	if joined {
		return
	}
	go func() {
		defer s.tasks.end(repoID, TaskKindMergeable)
		bctx := context.WithoutCancel(ctx)
		for _, n := range entry.drain() {
			_, _ = s.ComputeMergeable(bctx, owner, repo, n)
		}
	}()
}

// --- diff / commits ------------------------------------------------------------

// diffDir selects the git dir for base...head reads: the base repo dir when
// the head sha is reachable there, else the fork dir (shared packs make it
// local, §7). A cross-fork head missing from the base serving copy is
// bridged first (issue #456: fetch fork objects into the base copy, then
// re-probe) — only a still-missing head falls back to the fork dir. One
// pool-gated reachability probe on the happy path, no LIST.
func (s *Service) diffDir(ctx context.Context, pr *PRDoc) (string, error) {
	if s.Git == nil || s.Dirs == nil {
		return "", fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	baseDir, err := s.Dirs.Dir(ctx, pr.Base.Repo)
	if err != nil {
		return "", fmt.Errorf("%w: base repo unavailable: %v", ErrUnavailable, err)
	}
	if pr.Head.Repo == pr.Base.Repo {
		return baseDir, nil
	}
	headDir, err := s.Dirs.Dir(ctx, pr.Head.Repo)
	if err != nil {
		return "", fmt.Errorf("%w: head repo unavailable: %v", ErrUnavailable, err)
	}
	// Bridge fork-unique objects into the base copy (issue #456), then
	// re-probe: a still-missing head falls back to the fork dir (the
	// bridge succeeded, so the miss is object state, not backend — the
	// fork-local read is the best remaining chance). Any bridge FAILURE
	// is 503 (fail closed — a diff without its objects is a verdict no
	// fallback may guess).
	_, post, berr := s.bridgeForkHead(ctx, pr.Base.Repo, pr.Head.Repo, baseDir, headDir, pr.Head.SHA)
	if berr != nil {
		return "", fmt.Errorf("%w: reachability check: %v", ErrUnavailable, berr)
	}
	if post {
		return baseDir, nil
	}
	return headDir, nil
}

// Diff returns the text/plain unified diff base…head (§9.5). Auth: read.
func (s *Service) Diff(ctx context.Context, owner, repo string, num int, p auth.Principal) (string, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return "", err
	}
	pr, _, err := s.loadPR(ctx, owner, repo, num)
	if err != nil {
		return "", err
	}
	if pr == nil {
		if th, _, terr := s.loadThread(ctx, owner, repo, num); terr != nil || th == nil || th.Kind != "pr" {
			return "", fmt.Errorf("%w: %d", ErrNotFound, num)
		}
		return "", fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	dir, err := s.diffDir(ctx, pr)
	if err != nil {
		return "", err
	}
	return s.Git.Diff(ctx, dir, pr.Base.SHA, pr.Head.SHA)
}

// Commits returns {commits, more} of base..head (doc 07 Commit shape,
// skip/n pagination). Auth: read.
func (s *Service) Commits(ctx context.Context, owner, repo string, num int, p auth.Principal, skip, n int) ([]CommitEntry, bool, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, false, err
	}
	pr, _, err := s.loadPR(ctx, owner, repo, num)
	if err != nil {
		return nil, false, err
	}
	if pr == nil {
		return nil, false, fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	dir, err := s.diffDir(ctx, pr)
	if err != nil {
		return nil, false, err
	}
	if n <= 0 {
		n = 50
	}
	if n > 200 {
		n = 200
	}
	if skip < 0 {
		skip = 0
	}
	rows, err := s.Git.LogRange(ctx, dir, pr.Base.SHA, pr.Head.SHA, skip, n+1)
	if err != nil {
		return nil, false, err
	}
	more := len(rows) > n
	if more {
		rows = rows[:n]
	}
	return rows, more, nil
}

// threadTitleOf renders "owner/repo#num" for narration.
func threadTitleOf(owner, repo string, num int) string {
	return repoName(owner, repo) + "#" + itoa(num)
}
