// threads_sweep.go — the maintainer half of Threads (docs/features/15 §8,
// round-2 pass 2): `threads-gc` (queue repair + retention) and
// `threads-schedule` (cron evaluation) run as out-of-band maintain sweeps,
// never on a request path and never as a §4 priority unit.
//
// Each sweeper carries an in-memory due gate KEYED BY REPOSITORY, so one pass
// over the repositories covers every one of them: a process-wide slot would let
// the first repository of each interval consume it for all the others (the
// round-3 retention defect). The gates are per instance; every pass is
// idempotent, and the schedule's persisted watermark CAS is what makes
// cross-instance/restart double-firing impossible. A restart forgets the map
// and repeats a pass — never skips one (an extra pass is harmless, a starved
// repository is not).
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/maintain"
	"github.com/crueber/spindle/internal/threads"
)

const (
	// threadsGCCadence is how often a repo's queue repair pass runs.
	threadsGCCadence = 5 * time.Minute
	// threadsRetentionCadence is how often a repo's retention pass runs (one
	// pass per repository per hour; 15 §2.3). It is a scheduler cadence, not a
	// service one: the gate is keyed by repo.
	threadsRetentionCadence = time.Hour
	// threadsScheduleCadence matches cron's minute granularity.
	threadsScheduleCadence = time.Minute
)

// threadsSweepers holds the Threads sweepers over one service. The queue repair
// and the retention pass share the `threads-gc` task kind (one maintenance
// subject) but carry separate cadences, so the queue pass can run every five
// minutes while retention runs hourly per repository.
func newThreadsSweepers(svc *threads.Service) []maintain.Sweeper {
	return []maintain.Sweeper{
		&threadsSweeper{svc: svc, kind: threads.KindThreadsGC, cadence: threadsGCCadence, run: queueSweep},
		&threadsSweeper{svc: svc, kind: threads.KindThreadsQueueReconcile, cadence: threadsGCCadence, run: queueIndexSweep},
		&threadsSweeper{svc: svc, kind: threads.KindThreadsGC, cadence: threadsRetentionCadence, run: retentionSweep},
		&threadsSweeper{svc: svc, kind: threads.KindThreadsSchedule, cadence: threadsScheduleCadence, aligned: true, run: scheduleSweep},
	}
}

// threadsSweeper is one due-gated Threads maintain task.
type threadsSweeper struct {
	svc     *threads.Service
	kind    string
	cadence time.Duration
	// aligned makes the gate follow the wall clock's minute boundary instead
	// of measuring a full cadence from the last pass: cron's finest granularity
	// is a minute, and a duration gate measured from a pass at 08:37:57 is "a
	// hair early" at 08:38:57, which used to lose tick 08:38 entirely. A
	// minute-aligned gate is due as soon as the clock enters a new minute.
	aligned bool
	run     func(ctx context.Context, svc *threads.Service, repo string, log maintain.TaskLogger) error
	// now is the gate's clock (tests pin it; nil = time.Now).
	now func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
}

// Kind is the task-table kind.
func (w *threadsSweeper) Kind() string { return w.kind }

// Due reports whether repo needs this pass (a restart repeats a pass, never
// skips one). The gate is per repository: a multi-repo instance sweeps every
// repository on the interval instead of spending one global slot on the first.
func (w *threadsSweeper) Due(repo string) bool {
	now := w.gateNow()
	w.mu.Lock()
	defer w.mu.Unlock()
	at, ok := w.last[repo]
	if !ok {
		w.mark(repo, now)
		return true
	}
	if w.aligned {
		// Compare minute boundaries, not the sampled instants: any pass in a
		// later minute is due, so sub-second drift can never skip a minute.
		if now.UTC().Truncate(time.Minute).After(at.UTC().Truncate(time.Minute)) {
			w.mark(repo, now)
			return true
		}
		return false
	}
	if now.Sub(at) >= w.cadence {
		w.mark(repo, now)
		return true
	}
	return false
}

// mark records one repo's last pass.
func (w *threadsSweeper) mark(repo string, at time.Time) {
	if w.last == nil {
		w.last = map[string]time.Time{}
	}
	w.last[repo] = at
}

// gateNow is the gate's clock.
func (w *threadsSweeper) gateNow() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// Sweep runs one pass through the (repo,kind) task path.
func (w *threadsSweeper) Sweep(ctx context.Context, repo string, log maintain.TaskLogger) error {
	owner, name, ok := splitRepo(repo)
	if !ok || w.svc == nil {
		return nil
	}
	return w.run(ctx, w.svc, owner+"/"+name, log)
}

// queueSweep is the threads-gc queue-repair body.
func queueSweep(ctx context.Context, svc *threads.Service, repo string, log maintain.TaskLogger) error {
	owner, name, _ := splitRepo(repo)
	_, err := svc.GC(ctx, owner, name)
	return err
}

// retentionSweep is the threads-gc retention body.
func retentionSweep(ctx context.Context, svc *threads.Service, repo string, log maintain.TaskLogger) error {
	owner, name, _ := splitRepo(repo)
	_, err := svc.Retain(ctx, owner, name, log)
	return err
}

// queueIndexSweep is the threads-queue-reconcile body: rebuild one repository's
// rows in the global dispatch hint from its authoritative queue entries (the
// only place the index is repaired, and the only LIST it performs).
func queueIndexSweep(ctx context.Context, svc *threads.Service, repo string, log maintain.TaskLogger) error {
	owner, name, _ := splitRepo(repo)
	n, err := svc.ReconcileQueueIndex(ctx, owner, name)
	if err == nil && n > 0 && log != nil {
		log.Notice(fmt.Sprintf("threads-queue-reconcile %s: %d rows", repo, n))
	}
	return err
}

// scheduleSweep is the threads-schedule body.
func scheduleSweep(ctx context.Context, svc *threads.Service, repo string, log maintain.TaskLogger) error {
	owner, name, _ := splitRepo(repo)
	_, err := svc.SweepSchedule(ctx, owner, name, log)
	return err
}
