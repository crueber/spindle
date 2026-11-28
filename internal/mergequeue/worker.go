package mergequeue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/server/auth"
)

// The queue driver. One goroutine per repository serialises the heads of all
// its base queues (one running entry per base is therefore stronger than the
// contract asks: one running entry per repository). Every state change is a
// CAS on the queue document that verifies the worker's claim, so a stale
// worker — a restart, a second instance, a dequeued entry — can never land or
// eject an entry it no longer owns.
//
// Restart recovery is ReconcileRepo (the maintainer sweep): a validating or
// landing entry whose claim lease has expired is reset to pending and its
// temporary ref deleted, so the entry is re-prepared and re-checked. Work is
// repeated, never skipped; an entry that exhausts MaxAttempts is ejected with
// a named reason rather than wedging the queue.

var claimSeq uint64

func (s *Service) newClaimID() string {
	return fmt.Sprintf("%s-%d-%d", s.instance, s.nowUTC().UnixNano(), atomic.AddUint64(&claimSeq, 1))
}

// ownClaim reports a claim this process wrote (its instance prefix).
func (s *Service) ownClaim(id string) bool {
	return s.instance != "" && strings.HasPrefix(id, s.instance+"-")
}

// runWorker drains the repository's runnable heads. It returns when there is
// nothing left to start (pending entries are always claimable unless a base
// is paused), so a later enqueue/resume/restart starts a fresh worker.
func (s *Service) runWorker(ctx context.Context, owner, repo string) {
	key := repoName(owner, repo)
	for ctx.Err() == nil {
		doc, _, err := s.loadDoc(ctx, owner, repo)
		if err != nil {
			s.releaseWorker(key)
			return
		}
		worked := false
		for _, q := range doc.Queues {
			if q.Paused {
				continue
			}
			h := q.head()
			if h == nil || h.State != StatePending {
				continue
			}
			e, cred, ok := s.claim(ctx, owner, repo, q.Base, h.Number)
			if !ok {
				continue
			}
			if cred.Name == "" {
				s.finish(ctx, owner, repo, q.Base, e.Number, StateEjected, "merge queue: the enqueue credential is missing (re-enqueue the entry)", "")
			} else {
				s.processEntry(ctx, owner, repo, q.Base, e, cred)
			}
			worked = true
			break
		}
		if !worked && !s.releaseWorker(key) {
			return
		}
	}
	s.releaseWorker(key)
}

// claim CASes the queue head from pending to validating and returns a copy of
// the entry plus its recorded credential. ok=false means another worker (or a
// dequeue) won the race: the caller simply re-reads.
func (s *Service) claim(ctx context.Context, owner, repo, base string, num int) (Entry, auth.Principal, bool) {
	var out Entry
	var cred authPrincipal
	claimed := false
	_, err := s.updateDoc(ctx, owner, repo, func(doc *Doc) (bool, error) {
		q := doc.queue(base, false)
		if q == nil || q.Paused {
			return false, nil
		}
		h := q.head()
		if h == nil || h.Number != num || h.State != StatePending {
			return false, nil
		}
		now := nowRFC(s.nowUTC())
		h.State = StateValidating
		h.ClaimID = s.newClaimID()
		h.ClaimedAt = now
		if h.StartedAt == "" {
			h.StartedAt = now
		}
		h.Attempts++
		out = *h
		cred = doc.Credentials[credKey(base, num)]
		claimed = true
		return true, nil
	})
	if err != nil || !claimed {
		return Entry{}, auth.Principal{}, false
	}
	return out, cred.principal(), true
}

// processEntry prepares, validates and lands (or ejects) one claimed entry.
func (s *Service) processEntry(ctx context.Context, owner, repo, base string, e Entry, actor auth.Principal) {
	if s.Pulls == nil {
		s.finish(ctx, owner, repo, base, e.Number, StateEjected, "merge queue: pulls service not wired", "")
		return
	}
	// A human merge may have landed the PR while it waited. The queue record
	// then follows the PR rather than landing (or ejecting) a merged case.
	if cand, err := s.Pulls.QueueCandidate(ctx, owner, repo, e.Number, actor); err == nil && cand != nil {
		if cand.Merged {
			s.finish(ctx, owner, repo, base, e.Number, StateLanded, "merged outside the queue", cand.HeadSHA)
			return
		}
		if cand.State == "closed" {
			s.finish(ctx, owner, repo, base, e.Number, StateEjected, "pull request was closed while queued", "")
			return
		}
		if cand.Draft {
			s.finish(ctx, owner, repo, base, e.Number, StateEjected, "pull request was converted to a draft while queued", "")
			return
		}
	}
	prepCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	qm, err := s.Pulls.PrepareQueueMerge(prepCtx, owner, repo, e.Number, actor)
	cancel()
	if err != nil {
		s.finish(ctx, owner, repo, base, e.Number, StateEjected, err.Error(), "")
		return
	}
	// Hand the ref to the one push hook (PAR-016) before recording: the hook
	// creates the merge_group run, and the entry is not "validating" in the
	// queue's own bookkeeping until the ref it names has a run to check it.
	if s.RefPublished == nil {
		s.finish(ctx, owner, repo, base, e.Number, StateEjected, "merge queue: the Threads push hook is not wired", qm.MergeSHA)
		return
	}
	s.RefPublished(ctx, repoName(owner, repo), qm.Ref, qm.MergeSHA)
	if !s.recordPrepared(ctx, owner, repo, base, e, qm) {
		// The claim was taken from us between the publish and the CAS. The new
		// owner deletes/replaces the ref in PrepareQueueMerge; deleting here
		// could pull it out from under that owner, so we leave it.
		return
	}
	verdict := s.waitForValidation(ctx, owner, repo, base, e, qm, actor.Name)
	switch verdict.kind {
	case verdictLand:
		s.land(ctx, owner, repo, base, e, qm, actor)
	case verdictEject:
		s.finish(ctx, owner, repo, base, e.Number, StateEjected, verdict.reason, qm.MergeSHA)
	case verdictLost:
		// Another writer owns (or removed) the entry; it owns the ref too.
	}
}

// recordPrepared writes the prepared ref/shas onto the entry while verifying
// the claim. false = the claim was lost (the caller must not touch the ref).
func (s *Service) recordPrepared(ctx context.Context, owner, repo, base string, e Entry, qm *pulls.QueueMerge) bool {
	ok := false
	_, err := s.updateDoc(ctx, owner, repo, func(doc *Doc) (bool, error) {
		q := doc.queue(base, false)
		if q == nil {
			return false, nil
		}
		cur := q.find(e.Number)
		if cur == nil || cur.State != StateValidating || cur.ClaimID != e.ClaimID {
			return false, nil
		}
		cur.Ref, cur.MergeSHA, cur.BaseSHA, cur.HeadSHA, cur.Strategy =
			qm.Ref, qm.MergeSHA, qm.BaseSHA, qm.HeadSHA, qm.Strategy
		ok = true
		return true, nil
	})
	return err == nil && ok
}

type verdictKind int

const (
	verdictWait verdictKind = iota
	verdictLand
	verdictEject
	verdictLost
)

type verdict struct {
	kind   verdictKind
	reason string
}

// waitForValidation polls the merge_group run and the required-checks gate
// until the entry lands, ejects, or its claim is taken away. It holds no lock
// across a poll or a store read.
func (s *Service) waitForValidation(ctx context.Context, owner, repo, base string, e Entry, qm *pulls.QueueMerge, principal string) verdict {
	started := s.nowUTC()
	if t, err := time.Parse(time.RFC3339, e.StartedAt); err == nil {
		started = t
	}
	baseRef := "refs/heads/" + base
	for {
		if ctx.Err() != nil {
			// Shutdown: leave the entry validating. ReconcileRepo recovers it
			// after the restart (the claim lease expires) rather than losing it.
			return verdict{kind: verdictLost}
		}
		if !s.stillOwned(ctx, owner, repo, base, e.Number, e.ClaimID, StateValidating) {
			return verdict{kind: verdictLost}
		}
		if s.Runs == nil {
			return verdict{kind: verdictEject, reason: "merge queue: no Threads run reader wired"}
		}
		runs, err := s.Runs.MergeGroupRuns(ctx, repoName(owner, repo), qm.MergeSHA, started.Add(-10*time.Second))
		if err != nil {
			if ctx.Err() != nil {
				return verdict{kind: verdictLost}
			}
			return verdict{kind: verdictEject, reason: "cannot read the merge_group run: " + err.Error()}
		}
		runSuccess := false
		for _, r := range runs {
			switch r.State {
			case "success":
				runSuccess = true
			case "failure", "cancelled", "skipped", "timed_out":
				return verdict{kind: verdictEject, reason: fmt.Sprintf("merge_group run %s ended %s", r.ID, r.State)}
			}
		}
		prog, perr := s.readChecks(ctx, owner, repo, qm.MergeSHA, baseRef, principal)
		if perr != nil {
			return verdict{kind: verdictEject, reason: "required checks unavailable: " + perr.Error()}
		}
		if prog.OK {
			// The required-checks union is the gate. With no required context
			// the merge_group run itself must finish green (a repository that
			// configured no protection still gets its CI run's verdict).
			if len(prog.Required) > 0 || runSuccess {
				return verdict{kind: verdictLand}
			}
		} else if !prog.Pending {
			return verdict{kind: verdictEject, reason: "required checks not green for " + qm.MergeSHA + ": " + strings.Join(prog.Offenders, ", ")}
		} else if runSuccess {
			return verdict{kind: verdictEject, reason: "the merge_group run ended green but required checks are not green for " + qm.MergeSHA + ": " + strings.Join(prog.Offenders, ", ")}
		}
		elapsed := s.nowUTC().Sub(started)
		if len(runs) == 0 && elapsed >= s.runGrace() {
			return verdict{kind: verdictEject, reason: fmt.Sprintf("no merge_group run started within %s (the base branch needs a workflow with on.merge_group)", s.runGrace())}
		}
		if elapsed >= s.timeout() {
			return verdict{kind: verdictEject, reason: fmt.Sprintf("timed out after %s waiting for the merge_group run and required checks", s.timeout())}
		}
		select {
		case <-ctx.Done():
			return verdict{kind: verdictLost}
		case <-time.After(s.poll()):
		}
	}
}

// readChecks consults the checks seam; a nil seam is fail-closed (never a
// silent pass) because the queue's whole purpose is the gate.
func (s *Service) readChecks(ctx context.Context, owner, repo, sha, baseRef, principal string) (CheckProgress, error) {
	if s.Checks == nil {
		return CheckProgress{}, errors.New("no checks backend wired")
	}
	return s.Checks.RequiredChecksProgress(ctx, owner, repo, sha, baseRef, principal)
}

// land moves the entry to landing under its claim and calls the one merge
// path. A landing refusal ejects with the merge task's own named reason.
func (s *Service) land(ctx context.Context, owner, repo, base string, e Entry, qm *pulls.QueueMerge, actor auth.Principal) {
	if !s.markLanding(ctx, owner, repo, base, e.Number, e.ClaimID) {
		return // claim lost; the new owner owns the ref and the entry
	}
	landCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Minute)
	defer cancel()
	rec, err := s.Pulls.LandQueuedMerge(landCtx, owner, repo, e.Number, actor, qm)
	if err != nil {
		s.finish(ctx, owner, repo, base, e.Number, StateEjected, err.Error(), qm.MergeSHA)
		return
	}
	detail := ""
	if rec != nil {
		detail = rec.Summary
	}
	s.finish(ctx, owner, repo, base, e.Number, StateLanded, detail, qm.MergeSHA)
}

func (s *Service) markLanding(ctx context.Context, owner, repo, base string, num int, claim string) bool {
	ok := false
	_, err := s.updateDoc(ctx, owner, repo, func(doc *Doc) (bool, error) {
		q := doc.queue(base, false)
		if q == nil {
			return false, nil
		}
		cur := q.find(num)
		if cur == nil || cur.ClaimID != claim || cur.State != StateValidating {
			return false, nil
		}
		cur.State = StateLanding
		ok = true
		return true, nil
	})
	return err == nil && ok
}

// finish writes a terminal outcome under the claim, records it in the recent
// list and the audit trail, notifies the participants and deletes the temp ref.
// A lost claim is a no-op (the new owner is responsible).
func (s *Service) finish(ctx context.Context, owner, repo, base string, num int, state, reason, mergeSHA string) {
	who := "spindle"
	var out *Entry
	_, err := s.updateDoc(ctx, owner, repo, func(doc *Doc) (bool, error) {
		q := doc.queue(base, false)
		if q == nil {
			return false, nil
		}
		e := q.find(num)
		if e == nil || !active(e.State) {
			return false, nil
		}
		now := nowRFC(s.nowUTC())
		q.remove(num)
		delete(doc.Credentials, credKey(base, num))
		e.State = state
		e.Reason = reason
		e.FinishedAt = now
		e.ClaimID, e.ClaimedAt = "", ""
		if mergeSHA != "" {
			e.MergeSHA = mergeSHA
		}
		doc.remember(e)
		doc.audit(AuditEntry{At: now, Actor: who, Action: state, Base: base, Number: num, Detail: reason})
		out = e
		return true, nil
	})
	if err != nil || out == nil {
		return
	}
	switch state {
	case StateLanded:
		s.notify(ctx, owner, repo, "merge_queue_landed", who, num, []string{out.Author, out.Enqueuer})
	default:
		s.notify(ctx, owner, repo, "merge_queue_ejected", who, num, []string{out.Author, out.Enqueuer})
	}
	if out.Ref != "" {
		s.cleanupRef(ctx, owner, repo, base, num, out.Ref)
	}
}

// stillOwned reports whether the entry is still active in state under claim.
func (s *Service) stillOwned(ctx context.Context, owner, repo, base string, num int, claim, state string) bool {
	doc, _, err := s.loadDoc(ctx, owner, repo)
	if err != nil {
		return false
	}
	q := doc.queue(base, false)
	if q == nil {
		return false
	}
	e := q.find(num)
	return e != nil && e.ClaimID == claim && e.State == state
}

// ReconcileRepo is the restart/lease recovery pass (the maintainer sweeper and
// the boot path): stale claims are reset to pending after their temp ref is
// deleted, an entry that exhausted MaxAttempts is ejected with a named reason,
// and orphaned refs left by an earlier process are deleted. It never wedges:
// every branch ends in a runnable pending head, a terminal row, or nothing.
func (s *Service) ReconcileRepo(ctx context.Context, owner, repo string) error {
	return s.reconcile(ctx, owner, repo, false)
}

// ReconcileRepoForce is the first pass after THIS process starts: it ignores
// the claim lease, because a claim older than the lease and a claim from a
// process that just died are indistinguishable by time. Repeating a live
// instance's validation is safe by construction — the doc CAS revokes the old
// worker's claim (it can no longer land), the temporary ref is rebuilt, and
// the merge task's PR-merged flag is write-once — so a boot recovery prefers
// duplicated work over a wedged queue (the contract's ordering). It runs once
// per repository per process (the sweeper's boot gate).
func (s *Service) ReconcileRepoForce(ctx context.Context, owner, repo string) error {
	return s.reconcile(ctx, owner, repo, true)
}

func (s *Service) reconcile(ctx context.Context, owner, repo string, force bool) error {
	doc, _, err := s.loadDoc(ctx, owner, repo)
	if err != nil {
		return err
	}
	if len(doc.Queues) == 0 && len(doc.Recent) == 0 {
		return nil
	}
	now := s.nowUTC()
	var refs []struct {
		base string
		num  int
		ref  string
	}
	run := false
	_, err = s.updateDoc(ctx, owner, repo, func(doc *Doc) (bool, error) {
		changed := false
		refs = refs[:0]
		for _, q := range doc.Queues {
			for _, e := range append([]*Entry(nil), q.Entries...) {
				if !active(e.State) {
					continue
				}
				if e.State == StatePending {
					run = run || !q.Paused
					continue
				}
				if force && s.ownClaim(e.ClaimID) {
					continue // this process owns it; a boot sweep never steals from itself
				}
				claimed, _ := time.Parse(time.RFC3339, e.ClaimedAt)
				if !force && e.ClaimID != "" && !claimed.IsZero() && now.Sub(claimed) < s.claimLease() {
					continue // a live worker owns it
				}
				if e.Ref != "" {
					refs = append(refs, struct {
						base string
						num  int
						ref  string
					}{q.Base, e.Number, e.Ref})
				}
				if e.Attempts >= s.maxAttempts() {
					q.remove(e.Number)
					e.State = StateEjected
					e.FinishedAt = nowRFC(now)
					e.Reason = fmt.Sprintf("recovered %d times without completing; ejected for a person to re-enqueue", e.Attempts)
					e.ClaimID, e.ClaimedAt, e.Ref = "", "", ""
					e.MergeSHA, e.BaseSHA, e.HeadSHA, e.RunID = "", "", "", ""
					delete(doc.Credentials, credKey(q.Base, e.Number))
					doc.remember(e)
					doc.audit(AuditEntry{At: e.FinishedAt, Actor: "spindle", Action: "ejected", Base: q.Base,
						Number: e.Number, Detail: e.Reason})
					changed = true
					continue
				}
				e.State = StatePending
				e.ClaimID, e.ClaimedAt, e.Ref = "", "", ""
				e.MergeSHA, e.BaseSHA, e.HeadSHA, e.RunID = "", "", "", ""
				doc.audit(AuditEntry{At: nowRFC(now), Actor: "spindle", Action: "recovered", Base: q.Base,
					Number: e.Number, Detail: fmt.Sprintf("attempt %d: stale claim recovered after restart", e.Attempts)})
				run = run || !q.Paused
				changed = true
			}
		}
		for _, e := range doc.Recent {
			if e.Ref != "" {
				refs = append(refs, struct {
					base string
					num  int
					ref  string
				}{"", e.Number, e.Ref})
			}
		}
		return changed, nil
	})
	if err != nil {
		return err
	}
	for _, r := range refs {
		s.cleanupRef(ctx, owner, repo, r.base, r.num, r.ref)
	}
	if run {
		s.startWorker(ctx, owner, repo)
	}
	return nil
}

// RecoverAll is the boot-callable form for a caller that knows no repository
// list (composition runs ReconcileRepo from the maintainer's per-repo pass;
// this is for tests and a future boot scan).
func (s *Service) RecoverAll(ctx context.Context, repos []string) error {
	for _, r := range repos {
		owner, repo, ok := strings.Cut(r, "/")
		if !ok {
			continue
		}
		if err := s.ReconcileRepo(ctx, owner, repo); err != nil {
			return err
		}
	}
	return nil
}
