package threads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/store"
)

// Concurrency groups (15 §1/§4, pass B2; PAR-152 parity in wave 2): one
// in-flight run per group per repository, plus — GitHub's rule — at most ONE
// pending run behind it.
//
//	repos/<o>/<r>/threads/concurrency/<slug>-<hash8>.json   holder + pending
//
// The record names the run holding the group and (optionally) the single run
// waiting for it. Admission happens on the CLAIM path (after the queue entry
// CAS, before the job is marked running), so no extra state is written for a
// job that never runs and two runners can never admit two runs into one group:
// the CAS on the holder record is the lock. A job whose group is held by
// ANOTHER run gives its queue entry back and waits for the next probe (the
// release path clears the index when the holder run folds terminal). A job of
// the HOLDING run passes: a workflow-level group belongs to the run, so its
// sibling jobs must not serialize each other.
//
// ### Pending semantics (PAR-152)
//
// GitHub allows one running and one pending run per group. When a newer run
// wants a held group it becomes the pending run, and the pending run that was
// already there is CANCELLED — "the pending job or workflow in the same
// concurrency group will be cancelled". That is implemented by the `pending`
// field:
//
//   - run creation registers the run as pending when its group is held (one
//     group read per declared group; a group-free workflow pays nothing). The
//     newer run cancels the older pending run and takes the slot;
//   - `cancel-in-progress: true` cancels the HOLDER (and any pending run) at
//     creation, so the new run does not wait for a claim to start replacing it;
//   - admission refuses a run while the pending slot names a live run, so a
//     later-created run cannot jump ahead of the run that was already waiting;
//     the pending run itself takes the holder once the previous holder folds;
//   - a terminal run's fold clears both the holder and (when it names that run)
//     the pending slot, so a cancelled pending run never wedges the group.
//
// Sequencing with `cancel-in-progress: false` is therefore trigger order:
// holder → the pending run → the next run created after that.
const (
	// concurrencyPendingSuffix is a diagnostic marker only; the pending run is
	// a field on the same holder record.
	concurrencyPendingSuffix = "-pending"
)

// ConcurrencyPrefix is the holder-index family of one repository.
func ConcurrencyPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/threads/concurrency/"
}

// ConcurrencyKey is one group's holder record.
func ConcurrencyKey(owner, repo, group string) string {
	return ConcurrencyPrefix(owner, repo) + concurrencySlug(group) + ".json"
}

// concurrencySlug maps a group name onto a key segment: the checks charset
// (lowercased, invalid runs → "-", capped) plus a hash suffix, so two groups
// whose slugs collide never share an index entry.
func concurrencySlug(group string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(group)))
	return slugSegment(group) + "-" + hex.EncodeToString(sum[:4])
}

// ConcurrencyDoc is one group's holder record.
type ConcurrencyDoc struct {
	Group string `json:"group"`
	// RunID is the run currently allowed to execute in this group ("" = free).
	RunID string `json:"run_id"`
	// Pending is the single run waiting behind RunID ("" = none). GitHub allows
	// one pending run per group; a newer pending run cancels the older one
	// (PAR-152).
	Pending string `json:"pending,omitempty"`
	Since   string `json:"since"`
	Version int    `json:"version"`
}

// groupPolicy is one concurrency group a run's jobs declare and whether ANY
// job naming it asked to cancel the holder in progress.
type groupPolicy struct {
	Group            string
	CancelInProgress bool
}

// runGroupPolicies is the per-run view of its concurrency groups (distinct
// names, sorted; the strictest cancel-in-progress wins for a shared group).
func runGroupPolicies(jobs []*Job) []groupPolicy {
	byGroup := map[string]bool{}
	for _, j := range jobs {
		// A job the `if:`/`needs:` decision terminalised holds nothing: its
		// group must not block another run while this one is still live.
		if j.State != JobQueued && j.State != JobRunning {
			continue
		}
		if j.Spec == nil || j.Spec.Concurrency == nil {
			continue
		}
		g := strings.TrimSpace(j.Spec.Concurrency.Group)
		if g == "" {
			continue
		}
		byGroup[g] = byGroup[g] || j.Spec.Concurrency.CancelInProgress
	}
	groups := make([]string, 0, len(byGroup))
	for g := range byGroup {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	out := make([]groupPolicy, 0, len(groups))
	for _, g := range groups {
		out = append(out, groupPolicy{Group: g, CancelInProgress: byGroup[g]})
	}
	return out
}

// prepareRunGroups is the creation-time half of the group semantics (PAR-152):
// for every group the run declares, it either cancels the holder (when the
// workflow says cancel-in-progress) or registers this run as the single pending
// run — cancelling the pending run it replaces. It returns the first group this
// run waits for (the header's PendingGroup, shown on the run page) or "" when
// nothing is in the way.
//
// It runs AFTER the run header committed (service.createOne): the cancellations
// it performs must never destroy a live run on behalf of a run that then failed
// to be created, so its errors are logged and the claim path (which re-applies
// the same rule and fails closed) is the backstop.
//
// Cost: one GET per declared group, and a CAS only when the group is held. A
// workflow with no `concurrency:` block reaches none of this (startSupersede
// already skips it), so the group-free push budget is unchanged.
func (s *Service) prepareRunGroups(ctx context.Context, owner, repo string, run *Run, jobs []*Job) (string, error) {
	if run == nil || len(run.Concurrency) == 0 {
		return "", nil
	}
	pendingOn := ""
	for _, pol := range runGroupPolicies(jobs) {
		waiting, err := s.prepareGroup(ctx, owner, repo, pol, run.ID)
		if err != nil {
			return pendingOn, err
		}
		if waiting != "" && pendingOn == "" {
			pendingOn = waiting
		}
	}
	return pendingOn, nil
}

// prepareGroup applies one group's creation-time rule. It returns the group
// name when this run is now its pending run.
func (s *Service) prepareGroup(ctx context.Context, owner, repo string, pol groupPolicy, runID string) (string, error) {
	if pol.Group == "" {
		return "", nil
	}
	key := ConcurrencyKey(owner, repo, pol.Group)
	for attempt := 0; attempt < 4; attempt++ {
		raw, ver, err := s.getJSON(ctx, key)
		if err != nil {
			return "", err
		}
		doc := &ConcurrencyDoc{Group: pol.Group}
		if raw != nil {
			if jerr := json.Unmarshal(raw, doc); jerr != nil {
				return "", fmt.Errorf("%w: concurrency %s: %v", ErrCorrupt, pol.Group, jerr)
			}
		}
		if doc.RunID == runID || (doc.RunID == "" && doc.Pending == "") {
			return "", nil // free or already ours: nothing to register
		}
		holderLive := doc.RunID != "" && doc.RunID != runID && s.runLive(ctx, owner, repo, doc.RunID)
		pendingLive := doc.Pending != "" && doc.Pending != runID && s.runLive(ctx, owner, repo, doc.Pending)

		if pol.CancelInProgress {
			// Cancel the holder (and the pending run it replaced) now, not at
			// claim time: the documented behaviour is that the new run starts
			// cancelling the moment it is queued.
			if holderLive {
				if _, cerr := s.cancelRun(ctx, owner, repo, doc.RunID, cancelWhy{Reason: CancelReasonConcurrency}); cerr != nil {
					return "", cerr
				}
			}
			if pendingLive {
				if _, cerr := s.cancelRun(ctx, owner, repo, doc.Pending, cancelWhy{Reason: CancelReasonConcurrency}); cerr != nil {
					return "", cerr
				}
			}
			// The fold released the slots; a retry observes the free record.
			continue
		}
		if !holderLive {
			return "", nil // the holder folded without releasing: the claim steals it
		}
		if pendingLive && doc.Pending > runID {
			// A newer run already owns the pending slot (a retried creation):
			// leave it; this run is superseded as pending.
			return pol.Group, nil
		}
		displaced := doc.Pending
		doc.Pending = runID
		doc.Since = s.nowUTC().Format(dateTimeFmt)
		doc.Version++
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		if ver == "" {
			opts.Mode = store.PutCreate
		}
		if _, perr := store.PutBytes(ctx, s.Store, key, mustJSON(doc), opts); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue // another creation won; re-read
			}
			return "", perr
		}
		if displaced != "" && displaced != runID {
			// Pending-cancel: the newer run replaces the run that was waiting.
			if _, cerr := s.cancelRun(ctx, owner, repo, displaced, cancelWhy{Reason: CancelReasonConcurrency}); cerr != nil {
				return pol.Group, cerr
			}
		}
		return pol.Group, nil
	}
	return "", nil
}

// runLive reports whether a run id names a non-terminal run the store still
// has. An unreadable/absent run is not live (the slot can be stolen).
func (s *Service) runLive(ctx context.Context, owner, repo, runID string) bool {
	if strings.TrimSpace(runID) == "" {
		return false
	}
	r, err := s.loadRun(ctx, owner, repo, runID)
	return err == nil && r != nil && !terminal(r.State)
}

// admitConcurrency reports whether this job may run now, taking the group when
// it is free (or held by the same run). spec is the claiming job's spec.
func (s *Service) admitConcurrency(ctx context.Context, owner, repo, runID string, spec *Spec) (bool, error) {
	if spec == nil || spec.Concurrency == nil {
		return true, nil
	}
	group := strings.TrimSpace(spec.Concurrency.Group)
	if group == "" {
		return true, nil
	}
	for attempt := 0; attempt < 4; attempt++ {
		ok, holder, err := s.tryAcquireGroup(ctx, owner, repo, group, runID)
		if err != nil || ok {
			return ok, err
		}
		if holder == "" {
			return false, nil
		}
		if !spec.Concurrency.CancelInProgress {
			// Wait in the single pending slot (cancelling the run this one
			// replaces). A failure here must not fail the claim: the job simply
			// stays queued and the next probe tries again.
			_, _ = s.notePendingGroup(ctx, owner, repo, group, runID)
			return false, nil
		}
		// Cancel the holder (or the pending run ahead of us) through the shared
		// terminal path, which folds it and releases/advances this group; the
		// next iteration takes it.
		if _, cerr := s.cancelRun(ctx, owner, repo, holder, cancelWhy{Reason: CancelReasonConcurrency}); cerr != nil {
			return false, cerr
		}
	}
	return false, nil
}

// notePendingGroup registers runID as the group's single pending run, cancelling
// the older pending run it replaces (GitHub's rule). It is the claim-path entry
// point; creation uses prepareGroup, which knows the cancel-in-progress flag.
func (s *Service) notePendingGroup(ctx context.Context, owner, repo, group, runID string) (bool, error) {
	key := ConcurrencyKey(owner, repo, group)
	for attempt := 0; attempt < 4; attempt++ {
		raw, ver, err := s.getJSON(ctx, key)
		if err != nil || raw == nil {
			return false, err // no record: the claim will create it
		}
		doc := &ConcurrencyDoc{Group: group}
		if jerr := json.Unmarshal(raw, doc); jerr != nil {
			return false, fmt.Errorf("%w: concurrency %s: %v", ErrCorrupt, group, jerr)
		}
		if doc.RunID == runID {
			return false, nil // we hold it: nothing to wait for
		}
		if doc.Pending == runID {
			return true, nil
		}
		if doc.Pending != "" && s.runLive(ctx, owner, repo, doc.Pending) && doc.Pending > runID {
			return true, nil // a newer run owns the slot; this one is superseded
		}
		displaced := doc.Pending
		doc.Pending = runID
		doc.Since = s.nowUTC().Format(dateTimeFmt)
		doc.Version++
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		if ver == "" {
			opts.Mode = store.PutCreate
		}
		if _, perr := store.PutBytes(ctx, s.Store, key, mustJSON(doc), opts); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return false, perr
		}
		if displaced != "" && displaced != runID && s.runLive(ctx, owner, repo, displaced) {
			_, _ = s.cancelRun(ctx, owner, repo, displaced, cancelWhy{Reason: CancelReasonConcurrency})
		}
		return true, nil
	}
	return false, nil
}

// tryAcquireGroup CASes one group's holder to runID. It returns (false, holder,
// nil) when a different live run holds it OR when a live run owns the pending
// slot ahead of runID (the "holder" is then that pending run, so
// cancel-in-progress can supersede it too).
func (s *Service) tryAcquireGroup(ctx context.Context, owner, repo, group, runID string) (bool, string, error) {
	key := ConcurrencyKey(owner, repo, group)
	for attempt := 0; attempt < 4; attempt++ {
		raw, ver, err := s.getJSON(ctx, key)
		if err != nil {
			return false, "", err
		}
		doc := &ConcurrencyDoc{Group: group}
		if raw != nil {
			if jerr := json.Unmarshal(raw, doc); jerr != nil {
				return false, "", fmt.Errorf("%w: concurrency %s: %v", ErrCorrupt, group, jerr)
			}
		}
		clearPending := false
		if doc.RunID != "" && doc.RunID != runID {
			holder, rerr := s.loadRun(ctx, owner, repo, doc.RunID)
			if rerr != nil {
				return false, "", rerr
			}
			if holder != nil && !terminal(holder.State) {
				return false, doc.RunID, nil // taken by a live run
			}
			// The holder folded without releasing (a crashed cancel, a swept
			// run): steal it.
		}
		if doc.Pending != "" && doc.Pending != runID {
			pending, perr := s.loadRun(ctx, owner, repo, doc.Pending)
			if perr != nil {
				return false, "", perr
			}
			if pending != nil && !terminal(pending.State) {
				// A run is already waiting: it gets the group next (one running,
				// one pending — GitHub's rule). The caller treats this like a
				// live holder for cancel-in-progress.
				return false, doc.Pending, nil
			}
			clearPending = true // stale slot
		}
		if doc.RunID == runID && doc.Pending == "" {
			return true, runID, nil
		}
		doc.RunID = runID
		if doc.Pending == runID || clearPending {
			doc.Pending = ""
		}
		doc.Since = s.nowUTC().Format(dateTimeFmt)
		doc.Version++
		body, _ := json.Marshal(doc) // cannot fail: the record is pure JSON types
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		if ver == "" {
			opts.Mode = store.PutCreate
		}
		if _, perr := store.PutBytes(ctx, s.Store, key, body, opts); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue // another admission won; re-read
			}
			return false, "", perr
		}
		return true, runID, nil
	}
	return false, "", nil
}

// releaseConcurrency frees every group the run held (called when its state
// folds terminal). A lost CAS is harmless: the next user of the group steals a
// holder whose run is terminal. It also clears a pending slot naming this run,
// so a cancelled pending run never wedges the group behind a dead waiter.
func (s *Service) releaseConcurrency(ctx context.Context, owner, repo, runID string, groups []string) {
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		key := ConcurrencyKey(owner, repo, group)
		_, _ = s.casUpdate(ctx, key, 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
			if cur == nil {
				return nil, false, nil
			}
			var doc ConcurrencyDoc
			if jerr := json.Unmarshal(cur, &doc); jerr != nil {
				return nil, false, nil // corrupt: leave it to a steal
			}
			changed := false
			if doc.RunID == runID {
				doc.RunID = ""
				changed = true
			}
			if doc.Pending == runID {
				doc.Pending = ""
				changed = true
			}
			if !changed {
				return nil, false, nil
			}
			doc.Since = s.nowUTC().Format(dateTimeFmt)
			doc.Version++
			return mustJSON(&doc), true, nil
		})
		// Wake the runners so a promoted pending run is offered on the next
		// probe instead of waiting for its 2 s re-probe.
		fetchHub.wake(repoID(owner, repo))
	}
}

// runGroups returns every distinct concurrency group a run's jobs declare.
func runGroups(jobs []*Job) []string {
	policies := runGroupPolicies(jobs)
	if len(policies) == 0 {
		return nil
	}
	out := make([]string, 0, len(policies))
	for _, p := range policies {
		out = append(out, p.Group)
	}
	return out
}
