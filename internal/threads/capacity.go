package threads

// Per-repository concurrency caps (PAR-153).
//
// A repository's `concurrency_cap` bounds how many of its runs may be live
// (queued or running) at once. A run created above the cap is written with all
// its jobs (so the page, checks mirror and PR panel stay coherent) but none of
// them are enqueued, and the run header names the reason (`HoldReason`). When a
// live run ends, `releaseHeldRuns` admits held runs oldest-first until the cap
// is full again.
//
// Why a hold rather than a queue entry: a runner claim is the only way a job
// starts, and a held run has no entry, so nothing can claim it. The release is
// driven by the run fold (one LIST per terminal fold, only when the repository
// configured a cap); a lost release is repaired by the next terminal fold, not
// by a timer that could drift.
//
// ### Concurrency
//
// Hazard: two creations both count `cap-1` and both start, exceeding the cap.
// Avoidance: the cap is an admission heuristic, not a lock — a burst can
// overshoot by the number of simultaneous creations. The documented guarantee
// is "a run created in steady state waits", and the reason is visible, never a
// silent spinner (law 7). Hazard: two folds release the same held run.
// Avoidance: the clear is a CAS; only the winner enqueues, and enqueue is
// idempotent per job. No lock is held across a store call.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/store"
)

// capacityHoldReason names the cap on the run header and in the API/UI.
func capacityHoldReason(limit int) string {
	return fmt.Sprintf("repository concurrency cap %d reached", limit)
}

// applyCapacityGate decides whether a newly written run must wait for a slot.
// It returns true when the run is held (and has stamped the reason on its
// header). The caller then skips the enqueue.
func (s *Service) applyCapacityGate(ctx context.Context, owner, repo, runID string, limit int, now string) (bool, error) {
	live, err := s.countLiveRuns(ctx, owner, repo, runID)
	if err != nil {
		return false, err
	}
	if live < limit {
		return false, nil
	}
	reason := capacityHoldReason(limit)
	_, err = s.casUpdate(ctx, RunKey(owner, repo, runID), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown run %q", ErrNotFound, runID)
		}
		var r Run
		if jerr := json.Unmarshal(cur, &r); jerr != nil {
			return nil, false, fmt.Errorf("%w: run %s: %v", ErrCorrupt, runID, jerr)
		}
		if terminal(r.State) || r.State == RunPendingApproval || r.HoldReason != "" {
			return nil, false, nil
		}
		r.HoldReason = reason
		r.UpdatedAt = now
		r.Version++
		return mustJSON(&r), true, nil
	})
	return err == nil, err
}

// countLiveRuns counts the repository's live runs other than exclude: queued or
// running, not held by the cap, not awaiting approval (a held deployment is not
// consuming a runner either).
func (s *Service) countLiveRuns(ctx context.Context, owner, repo, exclude string) (int, error) {
	runs, err := s.listRunsForCap(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	live := 0
	for _, r := range runs {
		if r == nil || r.ID == exclude || r.HoldReason != "" {
			continue
		}
		if r.State == RunQueued || r.State == RunRunning {
			live++
		}
	}
	return live, nil
}

// listRunsForCap reads every run header of one repository (LIST + GETs). It is
// only called when a repository configured a cap or is releasing a held run, so
// the ordinary path never pays it.
func (s *Service) listRunsForCap(ctx context.Context, owner, repo string) ([]*Run, error) {
	var keys []string
	prefix := RunsPrefix(owner, repo)
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		rest := strings.TrimPrefix(m.Key, prefix)
		// Run headers are direct children (`<run_id>.json`); job records and log
		// frames live under `<run_id>/…` and must never be decoded as runs.
		if strings.HasSuffix(rest, ".json") && !strings.Contains(rest, "/") {
			keys = append(keys, m.Key)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(keys)
	out := make([]*Run, 0, len(keys))
	for _, key := range keys {
		raw, _, gerr := s.getJSON(ctx, key)
		if gerr != nil {
			return nil, gerr
		}
		if raw == nil {
			continue
		}
		var r Run
		if jerr := json.Unmarshal(raw, &r); jerr != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, key, jerr)
		}
		out = append(out, &r)
	}
	return out, nil
}

// releaseHeldRuns admits capacity-held runs, oldest first, while the repository
// is under its cap. It is called after a terminal fold (the slot that freed)
// and is a no-op when no cap is configured.
func (s *Service) releaseHeldRuns(ctx context.Context, owner, repo string) {
	policy := s.loadRepoPolicy(ctx, owner, repo)
	if policy.cap <= 0 {
		return
	}
	runs, err := s.listRunsForCap(ctx, owner, repo)
	if err != nil {
		return
	}
	live := 0
	held := make([]*Run, 0)
	for _, r := range runs {
		if r == nil {
			continue
		}
		switch {
		case r.HoldReason != "":
			held = append(held, r)
		case r.State == RunQueued || r.State == RunRunning:
			live++
		}
	}
	sort.Slice(held, func(i, j int) bool {
		if held[i].CreatedAt != held[j].CreatedAt {
			return held[i].CreatedAt < held[j].CreatedAt
		}
		return held[i].ID < held[j].ID
	})
	for _, r := range held {
		if live >= policy.cap {
			return
		}
		if !s.clearCapacityHold(ctx, owner, repo, r.ID) {
			continue
		}
		jobs, jerr := s.loadJobs(ctx, owner, repo, r.ID)
		if jerr != nil {
			continue
		}
		idx := needsIndex(jobs)
		for _, job := range jobs {
			if job.State != JobQueued {
				continue
			}
			if _, eerr := s.enqueueJob(ctx, owner, repo, job, entryNeedMap(idx, job)); eerr != nil {
				break
			}
		}
		fetchHub.wake(repoID(owner, repo))
		live++
	}
}

// clearCapacityHold clears one run's hold; it reports whether THIS caller won
// the CAS (only the winner enqueues).
func (s *Service) clearCapacityHold(ctx context.Context, owner, repo, runID string) bool {
	won := false
	_, err := s.casUpdate(ctx, RunKey(owner, repo, runID), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var r Run
		if jerr := json.Unmarshal(cur, &r); jerr != nil {
			return nil, false, fmt.Errorf("%w: run %s: %v", ErrCorrupt, runID, jerr)
		}
		if r.HoldReason == "" || terminal(r.State) || r.State != RunQueued {
			return nil, false, nil
		}
		r.HoldReason = ""
		r.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		r.Version++
		won = true
		return mustJSON(&r), true, nil
	})
	return err == nil && won
}
