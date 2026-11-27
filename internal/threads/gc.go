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

// Retention (15 §2/§8, round-2 pass 2; PAR-158/159 in wave 2): terminal runs,
// their job records, log frames and queue entries do not accumulate forever,
// and neither do runner records whose credential is dead.
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
// ## Two windows, not one (PAR-158)
//
//   - `threads.retention` (default 30d) is the RUN window: the header, job
//     records, log frames and queue entries;
//   - `threads.artifact_retention` (default: the run window) is the ARTIFACT
//     window, and a per-artifact `retention-days` overrides it for that
//     artifact. The two are independent: an artifact whose own window is longer
//     OUTLIVES its run (the artifact family is reclaimed by its expiry-index row
//     afterwards, reported separately);
//   - `threads.keep_forever = true` reclaims neither (the runs/artifacts age
//     check is skipped entirely; dead runner records are still pruned).
//
// ## What is deleted, when
//
//   - terminal runs whose CompletedAt (or CreatedAt, for a run that never
//     started) is older than `threads.retention`;
//   - their job records, their `log/<attempt>-<seq>` frames, and the exact
//     queue entries their job records name (`Job.QueueSeq` — an exact-key
//     delete, never a LIST on the claim path);
//   - the artifacts whose OWN window has closed (with their blobs, manifests
//     and expiry rows); an artifact still inside its window is left in place;
//   - revoked runners past the window, and `pending` runners whose registration
//     token expired without ever being used.
//
// ## The safety rule: never delete a run that is live or attached
//
// A running/queued/pending-approval run is NEVER touched, whatever its age: a
// live run's jobs are still claimable. "Attached" is checked explicitly — any
// job whose lease is still in the future (a claimed job, even if a stale fold
// wrote a terminal header) pins the run, so a retention pass racing an active
// runner cannot delete the log a job is writing. The guard is
// `runDeletable`, and the tests pin both halves (live state and live lease).
//
// ## Dry run reports the same numbers without deleting (PAR-159)
//
// `PlanRetention` walks exactly the same objects with apply=false: every count
// and every per-family byte figure is identical, nothing is deleted, and the
// plan is what the admin page shows before an operator runs the real pass.
//
// ## Idempotence and cost
//
// Every step is a delete of an object that may already be gone (a no-op on every
// backend), and a second sweep over an unchanged repository deletes nothing. The
// pass is bounded: at most `retentionBatch` runs and `retentionRunners` runner
// records per pass, so one pass can never run past the unit cap; the rest is
// the next pass's work. The run LIST is the only prefix enumeration on the run
// path, and it runs on the maintainer, never on a request path (law 4). Each
// deleted run is enumerated ONCE (its whole prefix with sizes), which is how the
// per-family byte report is produced without a second round trip.
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

// Retention bounds (PAR-179). The run/log window may be as long as a year (a
// 90-day setting is squarely inside it); `keep_forever` is the unbounded
// spelling. Values are validated at config load with these names in the error.
const (
	MinRetention = 24 * time.Hour
	MaxRetention = 365 * 24 * time.Hour
)

const (
	// retentionBatch bounds the runs one sweep deletes.
	retentionBatch = 200
	// retentionRunners bounds the runner records one sweep deletes.
	retentionRunners = 200
)

// GCResult is what one threads-gc pass did (also the summary notice). The
// per-family byte fields are the PAR-159 report: bytes reclaimed, never a
// guess — they come from the object metadata of the keys actually deleted (or,
// in a dry run, of the keys that WOULD be deleted).
type GCResult struct {
	// Requeued is the lapsed claims the queue pass re-queued (Service.GC).
	Requeued int `json:"requeued"`
	// RunsScanned / RunsDeleted count terminal runs examined and removed.
	RunsScanned int `json:"runs_scanned"`
	RunsDeleted int `json:"runs_deleted"`
	JobsDeleted int `json:"jobs_deleted"`
	LogsDeleted int `json:"logs_deleted"`
	// ArtifactsDeleted / ArtifactBytes count published artifacts reclaimed
	// (with their run, or through their own expiry window).
	ArtifactsDeleted int   `json:"artifacts_deleted"`
	ArtifactBytes    int64 `json:"artifact_bytes"`
	// CachesDeleted / CacheBytes count cache entries and payload bytes
	// reclaimed (expiry, the repo budget, and orphans from a lost index CAS).
	CachesDeleted int   `json:"caches_deleted"`
	CacheBytes    int64 `json:"cache_bytes"`
	// QueueDeleted counts completed queue entries removed with their run.
	QueueDeleted int `json:"queue_deleted"`
	// RunnersDeleted / CredentialsDeleted count registry rows and their
	// credential records.
	RunnersDeleted     int `json:"runners_deleted"`
	CredentialsDeleted int `json:"credentials_deleted"`

	// Per-family reclaimed bytes (PAR-159). They sum to BytesFreed.
	RunBytes      int64 `json:"run_bytes"`
	JobBytes      int64 `json:"job_bytes"`
	LogBytes      int64 `json:"log_bytes"`
	QueueBytes    int64 `json:"queue_bytes"`
	RunnerBytes   int64 `json:"runner_bytes"`
	BytesFreed    int64 `json:"bytes_freed"`
	DryRun        bool  `json:"dry_run,omitempty"`
	KeepForever   bool  `json:"keep_forever,omitempty"`
	RetentionDays int   `json:"retention_days,omitempty"`
	ArtifactDays  int   `json:"artifact_retention_days,omitempty"`
}

// String renders the one-line notice the task log carries.
func (r GCResult) String() string {
	mode := ""
	if r.DryRun {
		mode = " (dry run)"
	}
	if r.KeepForever {
		mode = " (keep forever)"
	}
	return fmt.Sprintf("requeued=%d runs=%d jobs=%d logs=%d artifacts=%d caches=%d queue=%d runners=%d credentials=%d freed=%dB (run=%dB job=%dB log=%dB artifact=%dB cache=%dB queue=%dB runner=%dB) windows=%dd/%dd%s",
		r.Requeued, r.RunsDeleted, r.JobsDeleted, r.LogsDeleted, r.ArtifactsDeleted, r.CachesDeleted, r.QueueDeleted,
		r.RunnersDeleted, r.CredentialsDeleted, r.BytesFreed,
		r.RunBytes, r.JobBytes, r.LogBytes, r.ArtifactBytes, r.CacheBytes, r.QueueBytes, r.RunnerBytes,
		r.RetentionDays, r.ArtifactDays, mode)
}

// retention returns the effective run/log window (DefaultRetention when unset).
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
	return s.retain(ctx, owner, repo, true, log)
}

// PlanRetention is the dry run: exactly the same walk and the same numbers as
// Retain, with nothing deleted (PAR-159, the admin page's preview).
func (s *Service) PlanRetention(ctx context.Context, owner, repo string, log TaskLogger) (*GCResult, error) {
	return s.retain(ctx, owner, repo, false, log)
}

func (s *Service) retain(ctx context.Context, owner, repo string, apply bool, log TaskLogger) (*GCResult, error) {
	res := &GCResult{DryRun: !apply}
	if s == nil || s.Store == nil {
		return res, nil
	}
	res.KeepForever = s.KeepForever
	if s.KeepForever {
		// Nothing ages out. Dead runner records are still pruned: they are
		// infrastructure, not history, and a revoked credential must not linger.
		now := s.nowUTC()
		res.RetentionDays = int(s.retention() / (24 * time.Hour))
		res.ArtifactDays = int(s.artifactRetention() / (24 * time.Hour))
		s.deleteDeadRunners(ctx, now.Add(-s.retention()), apply, res)
		return res, nil
	}
	now := s.nowUTC()
	cutoff := now.Add(-s.retention())
	res.RetentionDays = int(s.retention() / (24 * time.Hour))
	res.ArtifactDays = int(s.artifactRetention() / (24 * time.Hour))
	ids, err := s.runIDs(ctx, owner, repo)
	if err != nil {
		return res, err
	}
	deleted := 0
	gone := map[string]bool{} // run ids this pass reclaims (their queue entries go too)
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
		jobs, lerr := s.loadJobs(ctx, owner, repo, id)
		if lerr != nil {
			return res, lerr
		}
		if !runDeletable(r, jobs, now, cutoff) {
			continue
		}
		n, ferr := s.deleteRunWith(ctx, owner, repo, id, now, apply)
		if ferr != nil {
			return res, ferr
		}
		gone[id] = true
		res.RunsDeleted++
		deleted++
		res.JobsDeleted += n.jobs
		res.LogsDeleted += n.logs
		res.ArtifactsDeleted += n.artifacts
		res.ArtifactBytes += n.artifactsBytes
		res.RunBytes += n.runBytes
		res.JobBytes += n.jobBytes
		res.LogBytes += n.logBytes
		res.BytesFreed += n.bytes
	}
	if len(gone) > 0 {
		// An unclaimed job's queue entry carries no Job.QueueSeq (only a claim
		// records one), so the entries of a reclaimed run are found with ONE LIST
		// over the queue family — on the maintainer, never a claim path. A dry
		// run counts them; an apply run removes them. The dispatch head drops the
		// dangling seqs on its next visit.
		s.dropQueueEntries(ctx, owner, repo, gone, apply, res)
	}
	// Artifacts whose own window closed (one expiry-index LIST per pass, never a
	// per-run enumeration).
	if eerr := s.expireArtifactsMode(ctx, owner, repo, now, apply, gone, res); eerr != nil {
		return res, eerr
	}
	// The cache family (PAR-095): expiry, the repo budget and orphan payloads.
	// Its own index is the read, so a repository with no caches pays one probe.
	if cerr := s.reclaimCaches(ctx, owner, repo, apply, res); cerr != nil {
		return res, cerr
	}
	s.deleteDeadRunners(ctx, cutoff, apply, res)

	if log != nil {
		log.Notice(fmt.Sprintf("threads-gc %s/%s%s: %s", owner, repo, map[bool]string{true: " (dry run)"}[!apply], res.String()))
	}
	return res, nil
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

// runDeletable is the safety rule (PAR-159): a run may be reclaimed only when
// it is terminal, older than the cutoff, and NOT attached — no job of it holds a
// live lease. A stale terminal fold over a claimed job therefore pins the run
// instead of deleting the log a runner is still writing.
func runDeletable(r *Run, jobs []*Job, now, cutoff time.Time) bool {
	if r == nil || !terminal(r.State) || !beforeCutoff(r, cutoff) {
		return false
	}
	for _, job := range jobs {
		if job == nil || job.LeaseUntil == nil {
			continue
		}
		until, err := time.Parse(dateTimeFmt, strings.TrimSpace(*job.LeaseUntil))
		if err == nil && until.After(now) {
			return false // a runner is attached to this job
		}
	}
	return true
}

// dropQueueEntries reclaims every queue entry belonging to one of the runs this
// pass deletes (one bounded LIST, exact-key deletes). A dry run counts and sizes
// the same entries without deleting, so the plan and the apply pass agree.
func (s *Service) dropQueueEntries(ctx context.Context, owner, repo string, gone map[string]bool, apply bool, res *GCResult) {
	var keys []string
	sizes := map[string]int64{}
	if err := s.Store.List(ctx, QueuePrefix(owner, repo), "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, ".json") && !strings.HasSuffix(m.Key, "/head.json") {
			keys = append(keys, m.Key)
			sizes[m.Key] = m.Size
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
		if apply {
			if derr := s.Store.Delete(ctx, key, ""); derr != nil && !store.IsNotFound(derr) {
				continue
			}
		}
		res.QueueDeleted++
		res.QueueBytes += sizes[key]
		res.BytesFreed += sizes[key]
	}
	if apply {
		for runID := range gone {
			s.removeQueueIndexRun(ctx, repoID(owner, repo), runID)
		}
	}
}

// runCounts is what deleting one run freed, split by family (PAR-159). Queue
// entries are counted by dropQueueEntries instead (one LIST for the whole pass).
type runCounts struct {
	jobs, logs     int
	artifacts      int
	artifactsBytes int64
	runBytes       int64
	jobBytes       int64
	logBytes       int64
	bytes          int64
}

// deleteRun removes one terminal run (apply) exactly as the retention pass
// would; exposed for tests and for the run-deletion path.
func (s *Service) deleteRun(ctx context.Context, owner, repo, runID string) (runCounts, error) {
	return s.deleteRunWith(ctx, owner, repo, runID, s.nowUTC(), true)
}

// deleteRunWith reclaims one terminal run's records. In apply mode it deletes
// the run's objects under ONE prefix LIST (with sizes, so no extra round trip is
// needed for the byte report); in a dry run it reports exactly the same numbers
// and deletes nothing. Artifacts whose own window has NOT closed are kept (their
// expiry row reclaims them later), and the header is deleted last because it is
// the run's commit point: an interrupted delete leaves an invisible-but-sweepable
// remainder rather than a run page missing its jobs.
//
// Queue entries are reclaimed by the caller (dropQueueEntries, one LIST per
// pass), so a queue entry is counted exactly once whether or not a claim wrote
// Job.QueueSeq.
func (s *Service) deleteRunWith(ctx context.Context, owner, repo, runID string, now time.Time, apply bool) (runCounts, error) {
	var out runCounts
	prefix := RunsPrefix(owner, repo) + runID + "/"
	header := RunKey(owner, repo, runID)
	var keys []string
	sizes := map[string]int64{}
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		keys = append(keys, m.Key)
		sizes[m.Key] = m.Size
		return nil
	}); err != nil {
		return out, err
	}
	if res, gerr := s.Store.Get(ctx, header, store.GetOptions{}); gerr == nil {
		if obj, ok := res.(store.Object); ok {
			out.runBytes = obj.Meta.Size
			_ = obj.Body.Close()
		}
	} else if !store.IsNotFound(gerr) {
		return out, gerr
	}
	// Which artifacts are due: the expiry index is authoritative for a live
	// artifact; an artifact with no row (written before two windows existed) is
	// due with its run, which is the only reclaim path an old artifact has.
	dueArtifacts := map[string]bool{}
	var expiryKeys []string
	if lerr := s.Store.List(ctx, ArtifactExpiryPrefix(owner, repo), "", func(m store.ObjectMeta) error {
		raw, _, err := s.getJSON(ctx, m.Key)
		if err != nil || raw == nil {
			return nil
		}
		var idx artifactExpiryIndex
		if jerr := json.Unmarshal(raw, &idx); jerr != nil {
			return nil
		}
		if idx.Run != runID {
			return nil
		}
		at, perr := time.Parse(dateTimeFmt, strings.TrimSpace(idx.Expires))
		if perr != nil {
			return nil
		}
		if !at.After(now) {
			dueArtifacts[idx.Name] = true
			expiryKeys = append(expiryKeys, m.Key)
		}
		return nil
	}); lerr != nil {
		return out, lerr
	}
	keepArtifactPrefixes := map[string]bool{}
	arts, aerr := s.loadArtifacts(ctx, owner, repo, runID)
	if aerr != nil {
		return out, aerr
	}
	for _, art := range arts {
		if !dueArtifacts[art.Name] && !artifactExpired(art, now) {
			// Its own window is still open (threads.artifact_retention or a
			// longer retention-days): keep manifest + blobs, reclaim later.
			keepArtifactPrefixes[ArtifactDir(owner, repo, runID, art.Name)] = true
			continue
		}
		out.artifacts++
	}
	for _, key := range keys {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if key == header {
			continue // the run header is the commit point: deleted last
		}
		if keptArtifactPrefix(key, keepArtifactPrefixes) {
			continue
		}
		size := sizes[key]
		switch {
		case strings.Contains(key, "/artifacts/"):
			// Artifact bytes are reported per manifest below (a dry run must
			// report the same figure, and an orphan blob belongs to none).
			out.artifactsBytes += size
		case strings.Contains(key, "/log/"):
			out.logs++
			out.logBytes += size
			out.bytes += size
		default:
			// A job record (jobs/<jid>.json), its id-token records and any other
			// per-job object.
			if strings.HasSuffix(key, ".json") && strings.Contains(key, "/jobs/") && !strings.Contains(key, "/id-token/") {
				out.jobs++
			}
			out.jobBytes += size
			out.bytes += size
		}
		if apply {
			if derr := s.Store.Delete(ctx, key, ""); derr != nil && !store.IsNotFound(derr) {
				return out, derr
			}
		}
	}
	out.bytes += out.artifactsBytes + out.runBytes
	if apply {
		for _, key := range expiryKeys {
			if derr := s.Store.Delete(ctx, key, ""); derr != nil && !store.IsNotFound(derr) {
				return out, derr
			}
		}
		if derr := s.Store.Delete(ctx, header, ""); derr != nil && !store.IsNotFound(derr) {
			return out, derr
		}
	}
	return out, nil
}

// keptArtifactPrefix reports whether a key belongs to an artifact kept for its
// own window.
func keptArtifactPrefix(key string, kept map[string]bool) bool {
	if len(kept) == 0 {
		return false
	}
	for prefix := range kept {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// deleteDeadRunners removes instance runner records whose credential is dead:
// a revoked runner past the window, or a pending registration that expired
// unused. A live runner is never touched. The family is instance-wide, so a
// process-local hourly gate keeps the per-repository retention pass from
// LISTing it once per repository (a restart repeats the sweep — safe). In a dry
// run the same records are counted and nothing is deleted.
func (s *Service) deleteDeadRunners(ctx context.Context, cutoff time.Time, apply bool, res *GCResult) {
	if !s.runnerSweepDue() {
		return
	}
	if res.RunnersDeleted >= retentionRunners {
		return
	}
	var keys []string
	sizes := map[string]int64{}
	if err := s.Store.List(ctx, RunnersPrefix(), "", func(m store.ObjectMeta) error {
		rest := strings.TrimPrefix(m.Key, RunnersPrefix())
		if strings.HasSuffix(rest, ".json") && !strings.Contains(rest, "/") {
			keys = append(keys, m.Key)
			sizes[m.Key] = m.Size
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
		if !apply {
			res.RunnersDeleted++
			res.RunnerBytes += sizes[key]
			res.BytesFreed += sizes[key]
			continue
		}
		if derr := s.Store.Delete(ctx, key, ""); derr != nil && !store.IsNotFound(derr) {
			continue
		}
		res.RunnersDeleted++
		res.RunnerBytes += sizes[key]
		res.BytesFreed += sizes[key]
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
