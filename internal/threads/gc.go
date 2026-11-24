package threads

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// Retention (15 §2/§8, round-2 pass 2): terminal runs, their job records, log
// frames and queue entries do not accumulate forever, and neither do runner
// records whose credential is dead.
//
// ## What is kept, and why deleting a run is safe for a live PR
//
// A run's *checks* are a separate family (`checks/<sha>/<context>.json`, owned
// by internal/checks, 05 §2). The merge gate, the checks page and the PR checks
// panel read that family, never the Threads run record, so deleting an old run
// neither un-greens nor un-blocks a merge — the required check is still there
// and still authoritative. The only thing a deleted run loses is the run page's
// log/step history (the check description names the run id, which then 404s).
// Threads therefore never deletes `checks/`: 05 owns that retention.
//
// ## What is deleted
//
//   - terminal runs whose CompletedAt (or CreatedAt, for a run that never
//     started) is older than Service.Retention;
//   - their job records, their `log/<attempt>-<seq>` frames, and the exact
//     queue entries their job records name (`Job.QueueSeq` — an exact-key
//     delete, never a LIST on the claim path);
//   - revoked runners past the window, and `pending` runners whose registration
//     token expired without ever being used (both the registry record and the
//     credential record).
//
// A running/queued/pending-approval run is NEVER touched, whatever its age: a
// live run's jobs are still claimable.
//
// ## Idempotence and cost
//
// Every step is a delete of an object that may already be gone (a no-op on every
// backend), and a second sweep over an unchanged repository deletes nothing. The
// pass is bounded: at most `retentionBatch` runs and `retentionRunners` runner
// records per pass, so one pass can never run past the unit cap; the rest is
// the next pass's work. The run LIST is the only prefix enumeration, and it runs
// on the maintainer, never on a request path (law 4).
//
// ## Cadence lives in the scheduler, per repository
//
// Retain has no clock of its own. The maintainer's `threads-gc` sweeper owns the
// cadence with a gate keyed by repository (cmd/spindle/threads_sweep.go): the
// queue repair runs every pass and retention runs at most once an hour FOR EACH
// repository. A single process-wide slot (the round-3 defect) let only the first
// repository of each hour run Retain, so every other repository's terminal runs
// and logs lived forever; a per-repo gate means one pass over the repositories
// covers all of them. The gate is in memory, so a restart repeats a pass for
// every repository — the safe direction (an extra pass, never a starved one) —
// and two instances may run the same repository concurrently: every delete is
// an already-gone-tolerant exact-key delete, so the loser frees nothing.

// DefaultRetention is the terminal-run window when the config says nothing
// (threads.retention, 11_config_cli.md).
const DefaultRetention = 30 * 24 * time.Hour

const (
	// retentionBatch bounds the runs one sweep deletes.
	retentionBatch = 200
	// retentionRunners bounds the runner records one sweep deletes.
	retentionRunners = 200
)

// GCResult is what one threads-gc pass did (also the summary notice).
type GCResult struct {
	// Requeued is the lapsed claims the queue pass re-queued (Service.GC).
	Requeued int `json:"requeued"`
	// RunsScanned / RunsDeleted count terminal runs examined and removed.
	RunsScanned int `json:"runs_scanned"`
	RunsDeleted int `json:"runs_deleted"`
	JobsDeleted int `json:"jobs_deleted"`
	LogsDeleted int `json:"logs_deleted"`
	// ArtifactsDeleted / ArtifactBytes count published artifacts reclaimed
	// (with their run, or early through a per-artifact retention-days window).
	ArtifactsDeleted int   `json:"artifacts_deleted"`
	ArtifactBytes    int64 `json:"artifact_bytes"`
	// QueueDeleted counts completed queue entries removed with their run.
	QueueDeleted int `json:"queue_deleted"`
	// RunnersDeleted / CredentialsDeleted count registry rows and their
	// credential records.
	RunnersDeleted     int `json:"runners_deleted"`
	CredentialsDeleted int `json:"credentials_deleted"`
	// BytesFreed is the sum of the deleted objects' recorded sizes.
	BytesFreed int64 `json:"bytes_freed"`
}

// String renders the one-line notice the task log carries.
func (r GCResult) String() string {
	return fmt.Sprintf("requeued=%d runs=%d jobs=%d logs=%d artifacts=%d artifact_bytes=%d queue=%d runners=%d credentials=%d freed=%dB",
		r.Requeued, r.RunsDeleted, r.JobsDeleted, r.LogsDeleted, r.ArtifactsDeleted, r.ArtifactBytes, r.QueueDeleted,
		r.RunnersDeleted, r.CredentialsDeleted, r.BytesFreed)
}

// retention returns the effective window (DefaultRetention when unset).
func (s *Service) retention() time.Duration {
	if s.Retention > 0 {
		return s.Retention
	}
	return DefaultRetention
}

// Retain runs one retention pass over one repository (the maintainer's
// `threads-gc` retention half, on its per-repository hourly cadence; exposed for
// tests and for an operator's manual start). Idempotent: a second pass over an
// unchanged repository deletes nothing.
func (s *Service) Retain(ctx context.Context, owner, repo string, log TaskLogger) (*GCResult, error) {
	res := &GCResult{}
	if s == nil || s.Store == nil {
		return res, nil
	}
	now := s.nowUTC()
	cutoff := now.Add(-s.retention())
	ids, err := s.runIDs(ctx, owner, repo)
	if err != nil {
		return res, err
	}
	deleted := 0
	gone := map[string]bool{} // run ids this pass deletes (their queue entries go too)
	for i, id := range ids {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if deleted >= retentionBatch {
			break // the rest is the next pass's work
		}
		r, lerr := s.loadRun(ctx, owner, repo, id)
		if lerr != nil {
			return res, lerr
		}
		if r == nil {
			continue // raced a concurrent delete
		}
		res.RunsScanned++
		if log != nil && i%20 == 0 {
			log.Progress("runs", uint64(i), uint64(len(ids)), "run")
		}
		if !terminal(r.State) || !beforeCutoff(r, cutoff) {
			continue
		}
		n, ferr := s.deleteRun(ctx, owner, repo, id)
		if ferr != nil {
			return res, ferr
		}
		gone[id] = true
		res.RunsDeleted++
		deleted++
		res.JobsDeleted += n.jobs
		res.LogsDeleted += n.logs
		res.QueueDeleted += n.queue
		res.ArtifactsDeleted += n.artifacts
		res.ArtifactBytes += n.artifactsBytes
		res.BytesFreed += n.bytes
	}
	if len(gone) > 0 {
		// An unclaimed job's queue entry carries no Job.QueueSeq (only a claim
		// records one), so the entries of a deleted run are found with ONE LIST
		// over the queue family — on the maintainer, never a claim path — and
		// removed. The dispatch head drops the dangling seqs on its next visit.
		s.dropQueueEntries(ctx, owner, repo, gone, res)
	}
	// Shortened per-artifact windows (retention-days) are reclaimed through the
	// expiry index: ONE prefix LIST per pass, never a per-run enumeration.
	if eerr := s.expireArtifacts(ctx, owner, repo, now, res); eerr != nil {
		return res, eerr
	}
	s.deleteDeadRunners(ctx, cutoff, res)

	if log != nil {
		log.Notice(fmt.Sprintf("threads-gc %s/%s: %s", owner, repo, res.String()))
	}
	return res, nil
}

// dropQueueEntries deletes every queue entry belonging to one of the runs this
// pass deleted (one bounded LIST, exact-key deletes).
func (s *Service) dropQueueEntries(ctx context.Context, owner, repo string, gone map[string]bool, res *GCResult) {
	var keys []string
	if err := s.Store.List(ctx, QueuePrefix(owner, repo), "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, ".json") && !strings.HasSuffix(m.Key, "/head.json") {
			keys = append(keys, m.Key)
		}
		return nil
	}); err != nil {
		return // a listing failure never fails the sweep
	}
	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}
		raw, _, err := s.getJSON(ctx, key)
		if err != nil || raw == nil {
			continue
		}
		var entry QueueEntry
		if jerr := json.Unmarshal(raw, &entry); jerr != nil {
			continue
		}
		if !gone[entry.RunID] {
			continue
		}
		if derr := s.Store.Delete(ctx, key, ""); derr != nil && !store.IsNotFound(derr) {
			continue
		}
		res.QueueDeleted++
	}
	for runID := range gone {
		s.removeQueueIndexRun(ctx, repoID(owner, repo), runID)
	}
}

// runCounts is what deleting one run freed.
type runCounts struct {
	jobs, logs, queue int
	artifacts         int
	artifactsBytes    int64
	bytes             int64
}

// beforeCutoff reports whether a terminal run is older than the cutoff. A run
// with no completion stamp falls back to its creation time (a run that never
// started still ages out).
func beforeCutoff(r *Run, cutoff time.Time) bool {
	stamp := r.CompletedAt
	if stamp == nil || strings.TrimSpace(*stamp) == "" {
		stamp = &r.CreatedAt
	}
	at, err := time.Parse(dateTimeFmt, strings.TrimSpace(*stamp))
	if err != nil {
		return false // unparseable age is never a reason to delete
	}
	return at.Before(cutoff)
}

// deleteRun removes one terminal run: its jobs, their log frames, the queue
// entries those jobs name, and the header last (the header is the run's commit
// point, so an interrupted delete leaves an invisible-but-sweepable remainder
// rather than a run page missing its jobs).
func (s *Service) deleteRun(ctx context.Context, owner, repo, runID string) (runCounts, error) {
	var out runCounts
	jobs, err := s.loadJobs(ctx, owner, repo, runID)
	if err != nil {
		return out, err
	}
	for _, job := range jobs {
		n, derr := s.deleteJob(ctx, owner, repo, runID, job)
		if derr != nil {
			return out, derr
		}
		out.jobs++
		out.logs += n.logs
		out.bytes += n.bytes
		if job.QueueSeq != "" {
			if derr := s.Store.Delete(ctx, QueueEntryKey(owner, repo, job.QueueSeq), ""); derr != nil && !store.IsNotFound(derr) {
				return out, derr
			}
			out.queue++
		}
	}
	// Artifacts are deleted with the run: their blobs, their manifests, and any
	// short-expiry index rows (an index row whose artifact is gone is dropped
	// by the next expiry pass).
	arts, aerr := s.loadArtifacts(ctx, owner, repo, runID)
	if aerr != nil {
		return out, aerr
	}
	for _, art := range arts {
		freed, derr := s.deleteArtifact(ctx, owner, repo, runID, art.Name)
		if derr != nil {
			return out, derr
		}
		out.artifacts++
		out.artifactsBytes += freed
	}
	if derr := s.Store.Delete(ctx, RunKey(owner, repo, runID), ""); derr != nil && !store.IsNotFound(derr) {
		return out, derr
	}
	return out, nil
}

// deleteJob removes one job record and every log frame of every attempt.
func (s *Service) deleteJob(ctx context.Context, owner, repo, runID string, job *Job) (runCounts, error) {
	var out runCounts
	prefix := LogPrefix(owner, repo, runID, job.ID)
	var keys []string
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		keys = append(keys, m.Key)
		out.bytes += m.Size
		return nil
	}); err != nil {
		return out, err
	}
	for _, k := range keys {
		if err := s.Store.Delete(ctx, k, ""); err != nil && !store.IsNotFound(err) {
			return out, err
		}
		out.logs++
	}
	if err := s.Store.Delete(ctx, JobKey(owner, repo, runID, job.ID), ""); err != nil && !store.IsNotFound(err) {
		return out, err
	}
	// OIDC request-token records are per attempt and carry no index: the
	// retention pass lists the (small) family under this job and drops it with
	// the job. It holds only hashes.
	tokPrefix := IDTokenRequestPrefix(owner, repo, runID, job.ID)
	var tokKeys []string
	if err := s.Store.List(ctx, tokPrefix, "", func(m store.ObjectMeta) error {
		tokKeys = append(tokKeys, m.Key)
		return nil
	}); err != nil {
		return out, err
	}
	for _, k := range tokKeys {
		if err := s.Store.Delete(ctx, k, ""); err != nil && !store.IsNotFound(err) {
			return out, err
		}
	}
	return out, nil
}

// deleteDeadRunners removes instance runner records whose credential is dead:
// a revoked runner past the window, or a pending registration that expired
// unused. A live runner is never touched. The family is instance-wide, so a
// process-local hourly gate keeps the per-repository retention pass from
// LISTing it once per repository (a restart repeats the sweep — safe).
func (s *Service) deleteDeadRunners(ctx context.Context, cutoff time.Time, res *GCResult) {
	if !s.runnerSweepDue() {
		return
	}
	if res.RunnersDeleted >= retentionRunners {
		return
	}
	var keys []string
	if err := s.Store.List(ctx, RunnersPrefix(), "", func(m store.ObjectMeta) error {
		rest := strings.TrimPrefix(m.Key, RunnersPrefix())
		if strings.HasSuffix(rest, ".json") && !strings.Contains(rest, "/") {
			keys = append(keys, m.Key)
		}
		return nil
	}); err != nil {
		return // a listing failure is not a reason to fail the sweep
	}
	for _, key := range keys {
		if ctx.Err() != nil || res.RunnersDeleted >= retentionRunners {
			return
		}
		raw, _, err := s.getJSON(ctx, key)
		if err != nil || raw == nil {
			continue
		}
		var doc RunnerDoc
		if jerr := json.Unmarshal(raw, &doc); jerr != nil {
			continue
		}
		if !deadRunner(&doc, cutoff, s.nowUTC()) {
			continue
		}
		if derr := s.Store.Delete(ctx, key, ""); derr != nil && !store.IsNotFound(derr) {
			continue
		}
		res.RunnersDeleted++
		if derr := s.Store.Delete(ctx, RunnerCredentialKey(doc.ID), ""); derr != nil && !store.IsNotFound(derr) {
			continue
		}
		res.CredentialsDeleted++
		if derr := s.Store.Delete(ctx, RunnerTokenKey(doc.ID), ""); derr != nil && !store.IsNotFound(derr) {
			continue
		}
	}
}

// runnerSweepGate is the process-wide hourly gate for the instance runner
// family's retention pass.
type runnerSweepGate struct {
	mu sync.Mutex
	at time.Time
}

// runnerGateAlloc guards the lazy gate allocation (a Service built by hand in a
// test has no New() to initialise it).
var runnerGateAlloc sync.Mutex

func (s *Service) sweepGate() *runnerSweepGate {
	runnerGateAlloc.Lock()
	defer runnerGateAlloc.Unlock()
	if s.runnerSweep == nil {
		s.runnerSweep = &runnerSweepGate{}
	}
	return s.runnerSweep
}

// runnerSweepDue applies the process-wide hourly gate (see sweepGate).
func (s *Service) runnerSweepDue() bool {
	g := s.sweepGate()
	g.mu.Lock()
	defer g.mu.Unlock()
	now := s.nowUTC()
	if !g.at.IsZero() && now.Sub(g.at) < time.Hour {
		return false
	}
	g.at = now
	return true
}

// deadRunner reports a runner record past retention that can never authenticate
// again: a revoked credential past the window, or a `pending` registration whose
// token expired without ever being used.
func deadRunner(doc *RunnerDoc, cutoff, now time.Time) bool {
	switch doc.State {
	case RunnerRevoked:
		stamp := doc.RevokedAt
		if stamp == nil {
			stamp = &doc.CreatedAt
		}
		at, err := time.Parse(dateTimeFmt, strings.TrimSpace(*stamp))
		return err == nil && at.Before(cutoff)
	case RunnerPending:
		at, err := time.Parse(dateTimeFmt, strings.TrimSpace(doc.CreatedAt))
		return err == nil && at.Add(RegistrationTTL).Before(now)
	default:
		return false
	}
}
