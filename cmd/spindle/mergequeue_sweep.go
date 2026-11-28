// mergequeue_sweep.go — the merge queue's restart/lease recovery pass
// (docs/features/22_merge_queue.md §6). It runs per repository on the
// maintainer's cadence, through the (repo,kind) task path, and is idempotent:
// a restart repeats the pass rather than skipping it.
//
// The pass does three things, in this order, all under the queue document's
// CAS: recover a stale validating/landing claim (delete its temporary ref,
// reset the entry to pending, so the next claim re-prepares and re-checks),
// eject an entry that exhausted its recovery budget, and delete any temporary
// ref an earlier process left on a terminal row. Then it wakes the queue
// driver if a runnable head exists.
package main

import (
	"context"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/maintain"
	"github.com/crueber/spindle/internal/mergequeue"
)

const (
	// mergeQueueRecoverCadence is how often one repository's recovery pass
	// runs. It is short enough that a restart resumes quickly; the driver
	// itself polls during normal operation, so this cadence only matters
	// after a crash or for an orphaned ref.
	mergeQueueRecoverCadence = 30 * time.Second
	// mergeQueueSweepKind is the task-table kind (registered once at boot).
	mergeQueueSweepKind = "merge-queue-recover"
)

// mergeQueueSweeper is the due-gated per-repo recovery pass.
type mergeQueueSweeper struct {
	svc *mergequeue.Service
	now func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
	// booted records the repositories whose first (post-start) pass has run.
	// The first pass force-recovers every active claim (ReconcileRepoForce):
	// a restarted process cannot tell a dead peer's claim from a live one by
	// time alone, and repeating work is the safe side of that choice.
	booted map[string]bool
}

func newMergeQueueSweeper(svc *mergequeue.Service) *mergeQueueSweeper {
	return &mergeQueueSweeper{svc: svc, last: map[string]time.Time{}, booted: map[string]bool{}}
}

func (w *mergeQueueSweeper) Kind() string { return mergeQueueSweepKind }

// Due follows the per-repository gate rule (the threads sweepers' precedent):
// one process-wide slot would spend the interval on the first repo only.
func (w *mergeQueueSweeper) Due(repo string) bool {
	now := time.Now()
	if w.now != nil {
		now = w.now()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if at, ok := w.last[repo]; ok && now.Sub(at) < mergeQueueRecoverCadence {
		return false
	}
	w.last[repo] = now
	return true
}

func (w *mergeQueueSweeper) Sweep(ctx context.Context, repo string, t maintain.TaskLogger) error {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return nil
	}
	w.mu.Lock()
	first := !w.booted[repo]
	w.booted[repo] = true
	w.mu.Unlock()
	if first {
		return w.svc.ReconcileRepoForce(ctx, owner, name)
	}
	return w.svc.ReconcileRepo(ctx, owner, name)
}
