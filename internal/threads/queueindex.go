package threads

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/store"
)

// The global queue index (docs/PLAN-INSTANCE-RUNNERS.md §3):
//
//	threads/queue/index.json   {version, queued:[{repo,run,job,labels,created_at}], updated_at}
//
// It is a HINT for the global fetch: it names the repositories that currently
// have queued work so one probe can offer an instance runner the oldest
// matching job. It is never the claim authority — the per-repository queue
// entry CAS decides, exactly as before. A stale row is skipped and swept by
// `threads-queue-reconcile`, and a lost append is repaired from the
// authoritative per-repository entries.
//
// ### Concurrency
//
// Hazard: concurrent enqueues overwrite each other's row. Avoidance: every
// mutation is a CAS on the single index object with bounded retries (the
// casUpdate 5-attempt loop); the loser re-reads. Hazard: the index is a single
// contention point under a burst of pushes. Avoidance: the write is off the
// client's push path (run creation is detached), a failed append is logged and
// leaves the job queued for the reconciler, and claims never depend on the row
// being present. Hazard: a corrupt document breaks every claim. Avoidance: a
// read error surfaces on fetch (never a silent 204), and the reconciler rewrites
// the document from the per-repository entries. No lock is held across a store
// call.

// queueIndexMax bounds the hint. A larger instance queue is still served by the
// per-repository probes (the reconciler keeps the oldest rows); the bound only
// keeps one object from growing without limit.
const queueIndexMax = 10000

// queueGlobalProbeMax bounds the distinct repositories one global fetch probes.
// Entries are ordered by enqueue time, so the first matching rows are the
// oldest; the bound keeps one poll's work proportional to a small constant.
const queueGlobalProbeMax = 16

// QueueIndexEntry is one queued job row (a hint, not a record).
type QueueIndexEntry struct {
	Repo      string   `json:"repo"` // owner/name
	RunID     string   `json:"run"`
	JobID     string   `json:"job"`
	Labels    []string `json:"labels"`
	CreatedAt string   `json:"created_at"`
	// Fork marks a fork pull-request job: the global claim skips it for a
	// runner that did not opt in to fork runs, so the hint never offers what
	// the per-repository claim would refuse.
	Fork bool `json:"fork,omitempty"`
	// Priority is the run's claim priority (PAR-155). claimGlobal orders rows
	// by priority descending, then enqueue time — the documented queue order.
	Priority int `json:"priority,omitempty"`
}

// QueueIndex is threads/queue/index.json.
type QueueIndex struct {
	Version   int               `json:"version"`
	Queued    []QueueIndexEntry `json:"queued"`
	UpdatedAt string            `json:"updated_at"`
}

// loadQueueIndex reads the global hint; (nil, nil) when it does not exist yet.
func (s *Service) loadQueueIndex(ctx context.Context) (*QueueIndex, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, QueueIndexKey())
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return nil, "", nil
	}
	var idx QueueIndex
	if jerr := json.Unmarshal(raw, &idx); jerr != nil {
		return nil, "", fmt.Errorf("%w: queue index: %v", ErrCorrupt, jerr)
	}
	return &idx, ver, nil
}

// appendQueueIndex adds one queued job to the hint. A full index refuses the
// append (the caller logs it; the job stays queued and the reconciler repairs
// the row). Idempotent per (repo, run, job).
func (s *Service) appendQueueIndex(ctx context.Context, repo, runID, jobID string, labels []string, fork bool, priority int, at string) error {
	if repo == "" || runID == "" || jobID == "" {
		return nil
	}
	_, err := s.casUpdate(ctx, QueueIndexKey(), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		idx := &QueueIndex{}
		if cur != nil {
			if jerr := json.Unmarshal(cur, idx); jerr != nil {
				return nil, false, fmt.Errorf("%w: queue index: %v", ErrCorrupt, jerr)
			}
		}
		for _, row := range idx.Queued {
			if row.Repo == repo && row.RunID == runID && row.JobID == jobID {
				return nil, false, nil // already present
			}
		}
		if len(idx.Queued) >= queueIndexMax {
			return nil, false, fmt.Errorf("%w: queue index is full (%d rows)", ErrUnavailable, queueIndexMax)
		}
		idx.Queued = append(nonNilQueueRows(idx.Queued), QueueIndexEntry{
			Repo: repo, RunID: runID, JobID: jobID, Labels: normalizeLabels(labels), CreatedAt: at, Fork: fork,
			Priority: priority,
		})
		idx.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		idx.Version++
		return mustJSON(idx), true, nil
	})
	return err
}

// removeQueueIndexJob drops one job's row (best effort: a lost CAS is harmless,
// the reconciler sweeps stale rows).
func (s *Service) removeQueueIndexJob(ctx context.Context, repo, runID, jobID string) {
	s.removeQueueIndexRows(ctx, func(row QueueIndexEntry) bool {
		return row.Repo == repo && row.RunID == runID && row.JobID == jobID
	})
}

// removeQueueIndexRun drops every row of one run (a deleted run's entries).
func (s *Service) removeQueueIndexRun(ctx context.Context, repo, runID string) {
	s.removeQueueIndexRows(ctx, func(row QueueIndexEntry) bool {
		return row.Repo == repo && row.RunID == runID
	})
}

// removeQueueIndexRows applies one predicate in a single CAS.
func (s *Service) removeQueueIndexRows(ctx context.Context, drop func(QueueIndexEntry) bool) {
	if drop == nil {
		return
	}
	_, _ = s.casUpdate(ctx, QueueIndexKey(), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var idx QueueIndex
		if jerr := json.Unmarshal(cur, &idx); jerr != nil {
			return nil, false, fmt.Errorf("%w: queue index: %v", ErrCorrupt, jerr)
		}
		kept := make([]QueueIndexEntry, 0, len(idx.Queued))
		changed := false
		for _, row := range idx.Queued {
			if drop(row) {
				changed = true
				continue
			}
			kept = append(kept, row)
		}
		if !changed {
			return nil, false, nil
		}
		idx.Queued = kept
		idx.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		idx.Version++
		return mustJSON(&idx), true, nil
	})
}

// queueRows is the defensive non-nil form (arrays never null on the wire, and
// an append must not inherit a nil slice).
func nonNilQueueRows(in []QueueIndexEntry) []QueueIndexEntry {
	if in == nil {
		return []QueueIndexEntry{}
	}
	return in
}

// claimGlobal walks the index oldest-first and returns the first job this
// runner may execute. The runner's repository policy filters the rows before
// anything is probed, and the per-repository claim loop decides; a repository
// whose rows are stale is simply passed over. skip carries the entries this poll
// already refused (a busy concurrency group) keyed repo|seq.
func (s *Service) claimGlobal(ctx context.Context, runner *RunnerDoc, labels []string, skip map[string]bool) (*Claim, string, string, error) {
	idx, _, err := s.loadQueueIndex(ctx)
	if err != nil {
		return nil, "", "", err
	}
	if idx == nil {
		return nil, "", "", nil
	}
	seen := map[string]bool{}
	probed := 0
	// Claim order (PAR-155): priority descending, then enqueue time ascending
	// (the index is appended in enqueue order, so CreatedAt is the FIFO key).
	// The order is the documented one; the per-repository entry CAS is still
	// the only authority.
	rows := append([]QueueIndexEntry{}, idx.Queued...)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Priority != rows[j].Priority {
			return rows[i].Priority > rows[j].Priority
		}
		return rows[i].CreatedAt < rows[j].CreatedAt
	})
	for _, row := range rows {
		if probed >= queueGlobalProbeMax {
			break
		}
		if seen[row.Repo] || !runner.CanServeRepo(row.Repo) || runner.IsDraining() || !labelsMatch(runner.EffectiveLabels(), row.Labels) {
			continue
		}
		if row.Fork && !runner.AllowForkRuns {
			continue // a fork job is never offered to a runner that did not opt in
		}
		owner, name, rerr := splitRepo(row.Repo)
		if rerr != nil {
			continue // a malformed hint row is never a reason to fail a claim
		}
		seen[row.Repo] = true
		probed++
		claim, cerr := s.claimLoopSkip(ctx, owner, name, runner, labels, skip)
		if cerr != nil {
			return nil, "", "", cerr
		}
		if claim != nil {
			return claim, owner, name, nil
		}
	}
	return nil, "", "", nil
}

// ReconcileQueueIndex rebuilds one repository's rows in the global hint from
// its authoritative queue entries. It is the maintainer's
// `threads-queue-reconcile` body: the ONLY place a global-index repair LISTs
// (never a request path). It returns the number of rows added plus removed.
//
// The merge is one CAS: the repository's existing rows are replaced by the
// fresh set (queued entries only), other repositories' rows are untouched. A
// full index keeps the oldest rows and reports the overflow, never drops a
// queued job (the job still lives in its repository queue).
func (s *Service) ReconcileQueueIndex(ctx context.Context, owner, repo string) (int, error) {
	if s == nil || s.Store == nil {
		return 0, nil
	}
	repoID := owner + "/" + repo
	fresh := map[string]QueueIndexEntry{}
	var keys []string
	if err := s.Store.List(ctx, QueuePrefix(owner, repo), "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, ".json") && !strings.HasSuffix(m.Key, "/head.json") {
			keys = append(keys, m.Key)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	sort.Strings(keys)
	for _, key := range keys {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		raw, _, err := s.getJSON(ctx, key)
		if err != nil || raw == nil {
			continue
		}
		var entry QueueEntry
		if jerr := json.Unmarshal(raw, &entry); jerr != nil || entry.State != QueueQueued {
			continue
		}
		fresh[entry.RunID+"\x00"+entry.JobID] = QueueIndexEntry{
			Repo: repoID, RunID: entry.RunID, JobID: entry.JobID,
			Labels: normalizeLabels(entry.Labels), CreatedAt: entry.EnqueuedAt, Fork: entry.Fork,
			Priority: entry.Priority,
		}
	}
	changed := 0
	_, err := s.casUpdate(ctx, QueueIndexKey(), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		idx := &QueueIndex{}
		if cur != nil {
			if jerr := json.Unmarshal(cur, idx); jerr != nil {
				return nil, false, fmt.Errorf("%w: queue index: %v", ErrCorrupt, jerr)
			}
		}
		existing := map[string]bool{}
		kept := make([]QueueIndexEntry, 0, len(idx.Queued)+len(fresh))
		for _, row := range idx.Queued {
			if row.Repo != repoID {
				kept = append(kept, row)
				continue
			}
			key := row.RunID + "\x00" + row.JobID
			if freshRow, ok := fresh[key]; ok {
				existing[key] = true
				kept = append(kept, freshRow) // keep the authoritative row
				continue
			}
			changed++ // a stale row
		}
		added := make([]QueueIndexEntry, 0, len(fresh))
		for key, row := range fresh {
			if existing[key] {
				continue
			}
			added = append(added, row)
		}
		sort.Slice(added, func(i, j int) bool { return added[i].CreatedAt < added[j].CreatedAt })
		changed += len(added)
		kept = append(kept, added...)
		if len(kept) > queueIndexMax {
			slog.Warn("threads: queue index overflow during reconcile; keeping the oldest rows",
				"repo", repoID, "rows", len(kept), "max", queueIndexMax)
			kept = kept[:queueIndexMax]
		}
		if changed == 0 {
			return nil, false, nil
		}
		idx.Queued = kept
		idx.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		idx.Version++
		return mustJSON(idx), true, nil
	})
	if err != nil {
		return 0, err
	}
	return changed, nil
}
