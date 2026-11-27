package threads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// This file owns the bucket queue: enqueue, the dispatch head, the CAS claim,
// lease expiry and the per-process wake-up hub. There is no queue server and no
// LIST on the claim path (15 §3): the head object names the pending sequences
// and each entry is claimed by a CAS on its own object.
//
// ### Concurrency
//
// Hazard: two runners claim one job. Avoidance: the *queue entry* CAS is the
// lock — a claim is a PutUpdate carrying the version the prober read, so
// exactly one winner exists per (entry, version) and the loser re-probes.
// Hazard: a claimed entry leaks when a runner dies. Avoidance: the claim
// carries a lease; the probe requeues an expired entry (and bumps the job's
// attempt) before offering it again. A live runner's log frames renew the lease.
// Hazard: the head's pending list diverging from the entry objects. Avoidance:
// the entry is the truth and the head is a bounded projection — a stale seq
// fails its CAS and is trimmed by the next probe, and threads-gc rebuilds the
// head from the queue prefix (maintenance role, never the claim path).

// Queue entry states.
const (
	QueueQueued  = "queued"
	QueueClaimed = "claimed"
	QueueDone    = "done"
)

// queueSeqLayout is the append-only, time-ordered sequence: 19-digit unix
// nanos, a dash, six lowercase hex chars (the run-id shape, so lexical order is
// enqueue order).
const queueSeqLayout = runIDLayout

// queueWindowMax bounds the head's pending window. A repo with more than 512
// pending jobs is pathological; the entries still exist and threads-gc
// rebuilds the window (law 7: the overflow is a repair, not a silent drop).
const queueWindowMax = 512

// queueScanMax bounds one claim probe's claim ATTEMPTS (entries that matched
// labels and dependencies); entries refused by a blocked/lapsed state do not
// consume it, because their dependency's own entry must still be reachable in
// the same probe.
const queueScanMax = 32

// QueueEntry is repos/<o>/<r>/threads/queue/<seq>.json: one pending job. The
// entry is Create-only until claimed; the claim is a CAS (version) that records
// the runner, the attempt and the lease.
type QueueEntry struct {
	Seq     string   `json:"seq"`
	RunID   string   `json:"run_id"`
	JobID   string   `json:"job_id"`
	Labels  []string `json:"labels"`
	Attempt int      `json:"attempt"`
	State   string   `json:"state"`
	// Needs lists the job ids this job waits on (`needs:`), so the claim path
	// can refuse an ineligible job with one bounded GET per dependency and
	// never LISTs (15 §4.1, round 2).
	Needs []string `json:"needs,omitempty"`
	// NeedKeys maps each `needs:` YAML key to the job ids it expanded to, so a
	// job-level `if:` can read `needs.<key>.result` from the dependency records
	// the gate already reads (a matrix dependency's key covers every cell).
	NeedKeys map[string][]string `json:"need_keys,omitempty"`
	// Runner is the claiming runner id ("" while queued).
	Runner string `json:"runner,omitempty"`
	// CancelRequested is the operator's cancel signal the runner acks.
	CancelRequested bool `json:"cancel_requested,omitempty"`
	// Fork marks a fork pull-request job (the run's trust class). It is offered
	// only to a runner whose registry record opted in to fork runs; the field
	// is a hint copied from the job at enqueue, and the job record re-checked
	// under the claim CAS stays the authority (docs/threads.md §8.7).
	Fork bool `json:"fork,omitempty"`
	// Priority is the run's claim priority (PAR-155), copied from the job at
	// enqueue. Higher claims first; the head's ordering map is the queue's
	// projection of it, and the job record stays the authority at claim time.
	Priority int `json:"priority,omitempty"`
	// MatrixGroup is the YAML job key a matrix cell belongs to, and
	// MaxParallel its `strategy.max-parallel` bound (PAR-052). Both are copied
	// from the job at enqueue; 0/"" means "not a bounded matrix cell", which
	// pays no admission read.
	MatrixGroup string `json:"matrix_group,omitempty"`
	MaxParallel int    `json:"max_parallel,omitempty"`

	EnqueuedAt string  `json:"enqueued_at"`
	ClaimedAt  *string `json:"claimed_at,omitempty"`
	LeaseUntil *string `json:"lease_until,omitempty"`
	FinishedAt *string `json:"finished_at,omitempty"`
	Version    int     `json:"version"`
}

// QueueHead is repos/<o>/<r>/threads/queue/head.json: the dispatch head. It is
// a CAS'd projection over the entry objects — one GET is the whole claim probe.
type QueueHead struct {
	// Seq is the last allocated sequence.
	Seq string `json:"seq"`
	// Pending lists live sequence numbers, ordered by claim priority (PAR-155)
	// and then by sequence (FIFO). It is bounded to queueWindowMax.
	Pending []string `json:"pending"`
	// Priority maps a pending sequence to its run's priority so the head can
	// order claims without reading every entry (the claim path stays one GET).
	// A sequence absent from the map is priority 0.
	Priority map[string]int `json:"priority,omitempty"`
	// Paused mirrors the repository's `ci_paused` switch (PAR-157): the claim
	// probe refuses the repository from this one object, without a settings
	// read on the hot path. The settings record remains the authority; the
	// marker is written by UpdateSettings and repaired by a pause/resume.
	Paused    bool   `json:"paused,omitempty"`
	UpdatedAt string `json:"updated_at"`
	Version   int    `json:"version"`
}

// newQueueSeq mints one queue sequence.
func newQueueSeq(now time.Time) (string, error) {
	id, err := newRunID(now)
	if err != nil {
		return "", err
	}
	return id, nil
}

// enqueueJob appends one job to the repo queue. needs is the job's `needs:`
// resolved to key → job ids (entryNeedMap); the entry carries both the flat id
// list (the claim gate) and the key mapping (a job-level `if:` reading
// needs.<key>.result). The entry is the truth; the head append is a bounded
// projection, so a head failure is logged by the caller and repaired by
// threads-gc — it never fails run creation.
func (s *Service) enqueueJob(ctx context.Context, owner, repo string, job *Job, needs map[string][]string) (*QueueEntry, error) {
	seq, err := newQueueSeq(s.nowUTC())
	if err != nil {
		return nil, fmt.Errorf("%w: mint queue seq: %v", ErrUnavailable, err)
	}
	now := s.nowUTC().Format(dateTimeFmt)
	entry := &QueueEntry{
		Seq: seq, RunID: job.RunID, JobID: job.ID,
		Labels: jobLabels(job), Needs: needIDs(needs), NeedKeys: needs, Attempt: job.Attempt, State: QueueQueued,
		Fork:        !job.Trusted,
		Priority:    job.Priority,
		MatrixGroup: matrixGroupOf(job), MaxParallel: maxParallelOf(job),
		EnqueuedAt: now, Version: 1,
	}
	body, _ := json.Marshal(entry) // cannot fail: the record is pure JSON types
	if perr := s.putCreate(ctx, QueueEntryKey(owner, repo, seq), body); perr != nil {
		if store.IsPreconditionFailed(perr) {
			return nil, fmt.Errorf("%w: queue entry %s exists", ErrConflict, seq)
		}
		return nil, perr
	}
	if herr := s.appendHead(ctx, owner, repo, seq, entry.Priority); herr != nil {
		return entry, herr
	}
	// The global hint is what lets an instance runner find this repository. A
	// failed append is logged by the caller and repaired by
	// threads-queue-reconcile; the job remains queued either way.
	if ierr := s.appendQueueIndex(ctx, repoID(owner, repo), job.RunID, job.ID, entry.Labels, entry.Fork, entry.Priority, now); ierr != nil {
		return entry, ierr
	}
	return entry, nil
}

// jobLabels is the normalized label set the queue matches on. A well-known
// hosted label (`ubuntu-latest` and friends) is expanded onto the spindle label
// set that provides the same contract before matching (labels.go, PAR-170): the
// job still requires every label of that set, so an alias never bypasses a
// runner's declared capabilities.
func jobLabels(job *Job) []string {
	var out []string
	if job.Spec != nil {
		out = append(out, job.Spec.RunsOn...)
	}
	if len(out) == 0 && job.RunsOn != "" {
		out = strings.Split(job.RunsOn, ",")
	}
	return ExpandRunsOn(out)
}

// normalizeLabels lowercases, trims and de-duplicates a label set, sorted.
func normalizeLabels(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, l := range in {
		l = strings.ToLower(strings.TrimSpace(l))
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// labelsMatch reports runner ⊇ job (an empty job label set matches any runner —
// a workflow without runs-on still runs, which is the workflow parser's rule).
func labelsMatch(runner, job []string) bool {
	if len(job) == 0 {
		return true
	}
	have := map[string]bool{}
	for _, l := range runner {
		have[l] = true
	}
	for _, l := range job {
		if !have[l] {
			return false
		}
	}
	return true
}

// appendHead CAS-appends one sequence to the head's pending window, recording
// its claim priority. The window is kept in claim order: priority descending,
// then sequence ascending (FIFO within one priority).
func (s *Service) appendHead(ctx context.Context, owner, repo, seq string, priority int) error {
	key := QueueHeadKey(owner, repo)
	_, err := s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		head := &QueueHead{}
		if cur != nil {
			if jerr := json.Unmarshal(cur, head); jerr != nil {
				return nil, false, fmt.Errorf("%w: queue head: %v", ErrCorrupt, jerr)
			}
		}
		if head.Seq == "" || seq > head.Seq {
			head.Seq = seq
		}
		for _, p := range head.Pending {
			if p == seq {
				return nil, false, nil // already appended
			}
		}
		if len(head.Pending) >= queueWindowMax {
			return nil, false, fmt.Errorf("%w: queue head window is full", ErrUnavailable)
		}
		if head.Priority == nil {
			head.Priority = map[string]int{}
		}
		if priority != 0 {
			head.Priority[seq] = priority
		}
		head.Pending = append(head.Pending, seq)
		sortQueuePending(head)
		head.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		head.Version++
		body, _ := json.Marshal(head) // cannot fail: the record is pure JSON types
		return body, true, nil
	})
	return err
}

// sortQueuePending orders the head's window for claiming: highest priority
// first, then the oldest sequence. The queue sequence sorts by enqueue time, so
// the tie-break is FIFO.
func sortQueuePending(head *QueueHead) {
	if head == nil {
		return
	}
	sort.SliceStable(head.Pending, func(i, j int) bool {
		pi, pj := head.Priority[head.Pending[i]], head.Priority[head.Pending[j]]
		if pi != pj {
			return pi > pj
		}
		return head.Pending[i] < head.Pending[j]
	})
}

// setHeadPaused mirrors the repository's pause switch onto the queue head. It
// creates the head when absent so a pause before the first enqueue is still
// enforced on the next claim. A CAS loss is harmless: UpdateSettings logs it
// and the settings record remains the authority for the read model.
func (s *Service) setHeadPaused(ctx context.Context, owner, repo string, paused bool) error {
	key := QueueHeadKey(owner, repo)
	_, err := s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		head := &QueueHead{}
		if cur != nil {
			if jerr := json.Unmarshal(cur, head); jerr != nil {
				return nil, false, fmt.Errorf("%w: queue head: %v", ErrCorrupt, jerr)
			}
		}
		if head.Paused == paused {
			return nil, false, nil
		}
		head.Paused = paused
		head.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		head.Version++
		body, _ := json.Marshal(head) // cannot fail: the record is pure JSON types
		return body, true, nil
	})
	return err
}

// loadHead reads the dispatch head; (nil, nil) when the repo has no queue.
func (s *Service) loadHead(ctx context.Context, owner, repo string) (*QueueHead, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, QueueHeadKey(owner, repo))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return nil, "", nil
	}
	var head QueueHead
	if jerr := json.Unmarshal(raw, &head); jerr != nil {
		return nil, "", fmt.Errorf("%w: queue head: %v", ErrCorrupt, jerr)
	}
	sortQueuePending(&head)
	return &head, ver, nil
}

// trimHead drops sequences from the pending window (best effort: a lost CAS
// just means the next probe retries; the entry CAS remains the lock).
func (s *Service) trimHead(ctx context.Context, owner, repo string, drop map[string]bool) {
	if len(drop) == 0 {
		return
	}
	key := QueueHeadKey(owner, repo)
	_, _ = s.casUpdate(ctx, key, 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var head QueueHead
		if jerr := json.Unmarshal(cur, &head); jerr != nil {
			return nil, false, fmt.Errorf("%w: queue head: %v", ErrCorrupt, jerr)
		}
		kept := head.Pending[:0]
		for _, p := range head.Pending {
			if !drop[p] {
				kept = append(kept, p)
			}
		}
		if len(kept) == len(head.Pending) {
			return nil, false, nil
		}
		for seq := range drop {
			delete(head.Priority, seq)
		}
		head.Pending = append([]string{}, kept...)
		head.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		head.Version++
		body, _ := json.Marshal(&head) // cannot fail: the record is pure JSON types
		return body, true, nil
	})
}

// loadEntry reads one queue entry; (nil, "", nil) when absent.
func (s *Service) loadEntry(ctx context.Context, owner, repo, seq string) (*QueueEntry, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, QueueEntryKey(owner, repo, seq))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return nil, "", nil
	}
	var e QueueEntry
	if jerr := json.Unmarshal(raw, &e); jerr != nil {
		return nil, "", fmt.Errorf("%w: queue entry %s: %v", ErrCorrupt, seq, jerr)
	}
	return &e, ver, nil
}

// Claim is one successful queue claim.
type Claim struct {
	Entry *QueueEntry
	// EntryVersion is the version the claim wrote (the rollback CAS floor).
	EntryVersion store.Version
	Job          *Job
	Run          *Run
}

// claimLoop is the fetch probe with no skip set (tests and simple callers).
func (s *Service) claimLoop(ctx context.Context, owner, repo string, runner *RunnerDoc, labels []string) (*Claim, error) {
	return s.claimLoopSkip(ctx, owner, repo, runner, labels, nil)
}

// claimLoopSkip is the fetch probe: read the head once, walk the pending window,
// and return the first entry this runner may execute. It returns (nil, nil)
// when nothing matches. Expired claims are requeued in place before being
// offered again. skip holds sequences this poll already refused (a busy
// concurrency group), so one probe does not re-claim them in a loop.
func (s *Service) claimLoopSkip(ctx context.Context, owner, repo string, runner *RunnerDoc, labels []string, skip map[string]bool) (*Claim, error) {
	// Repository tenancy is the first filter: the admin's allowlist decides
	// whether this runner may see this repository at all. An empty policy
	// (a record written before tenancy) allows nothing.
	if !runner.AllowsRepo(repoID(owner, repo)) {
		return nil, nil
	}
	head, _, err := s.loadHead(ctx, owner, repo)
	if err != nil || head == nil {
		return nil, err
	}
	// PAR-157: a paused repository claims nothing. The pause marker rides the
	// head (one GET, the same object the probe already reads), so the settings
	// record is not on the hot path; in-flight jobs are untouched.
	if head.Paused {
		return nil, nil
	}
	// A draining runner finishes its job and claims nothing new (PAR-181).
	if runner.IsDraining() {
		return nil, nil
	}
	matchLabels := labels
	if groupLabels := runner.EffectiveLabels(); len(groupLabels) > 0 {
		matchLabels = normalizeLabels(append(append([]string{}, labels...), groupLabels...))
	}
	drop := map[string]bool{}
	indexDrop := map[string]QueueIndexEntry{}
	jobs := jobCache{}
	runs := runCache{}
	var claimed *Claim
	attempts := 0
	for _, seq := range head.Pending {
		if attempts >= queueScanMax || claimed != nil {
			break
		}
		if skip[repoID(owner, repo)+"|"+seq] {
			continue
		}
		entry, ver, eerr := s.loadEntry(ctx, owner, repo, seq)
		if eerr != nil {
			return nil, eerr
		}
		if entry == nil || entry.State == QueueDone {
			drop[seq] = true
			if entry != nil {
				indexDrop[seq] = QueueIndexEntry{Repo: repoID(owner, repo), RunID: entry.RunID, JobID: entry.JobID}
			}
			continue
		}
		if entry.State == QueueClaimed {
			if !s.claimExpired(entry) {
				continue
			}
			// Lease lapsed: requeue as a NEW attempt before offering it, so a
			// straggler from the old attempt is rejected by identity.
			if rerr := s.requeueEntry(ctx, owner, repo, entry, ver); rerr != nil {
				return nil, rerr
			}
			continue // offered on the next probe (the head still lists it)
		}
		if !labelsMatch(matchLabels, entry.Labels) {
			continue
		}
		// Fork runs need an opted-in runner. The entry is NOT retired: the job
		// stays queued for a runner that accepts fork code (or for the operator
		// to start one), exactly like a label mismatch.
		if entry.Fork && !runner.AllowForkRuns {
			continue
		}
		// `needs:` eligibility (15 §4.1). A job is offered only when every
		// dependency is terminal; then its job-level `if:` decides (the default
		// is "all dependencies succeeded"). A dependency that failed skips the
		// job — unless the job's own `if:` says otherwise — and the decision
		// terminalises it here rather than leaving it queued forever. Waiting
		// entries do not consume the scan budget: the dependency's own entry
		// must still be reachable in this probe.
		if len(entry.Needs) > 0 {
			gate, gerr := s.needsGate(ctx, owner, repo, entry, jobs)
			if gerr != nil {
				return nil, gerr
			}
			switch gate {
			case needsBlocked:
				if _, serr := s.settleQueuedJob(ctx, owner, repo, entry, JobSkipped,
					"needs: a dependency record could not be read"); serr != nil {
					return nil, serr
				}
				drop[seq] = true
				indexDrop[seq] = QueueIndexEntry{Repo: repoID(owner, repo), RunID: entry.RunID, JobID: entry.JobID}
				continue
			case needsWaiting:
				continue
			}
			verdict, reason, derr := s.dependentVerdict(ctx, owner, repo, entry, jobs, runs)
			if derr != nil {
				return nil, derr
			}
			switch verdict {
			case jobIfSkip:
				if _, serr := s.settleQueuedJob(ctx, owner, repo, entry, JobSkipped, reason); serr != nil {
					return nil, serr
				}
				drop[seq] = true
				indexDrop[seq] = QueueIndexEntry{Repo: repoID(owner, repo), RunID: entry.RunID, JobID: entry.JobID}
				continue
			case jobIfError:
				if _, serr := s.settleQueuedJob(ctx, owner, repo, entry, JobFailure, reason); serr != nil {
					return nil, serr
				}
				drop[seq] = true
				indexDrop[seq] = QueueIndexEntry{Repo: repoID(owner, repo), RunID: entry.RunID, JobID: entry.JobID}
				continue
			}
		}
		// Matrix admission (PAR-052): a bounded cell is offered only while its
		// group has a free slot. A refused cell stays queued and does not consume
		// the probe's claim budget; a slot taken here is released if the claim
		// CAS loses the race below.
		admitted := false
		if entry.MaxParallel > 0 && entry.MatrixGroup != "" {
			ok, aerr := s.admitMatrixCell(ctx, owner, repo, entry)
			if aerr != nil {
				return nil, aerr
			}
			if !ok {
				continue
			}
			admitted = true
		}
		attempts++
		gained, cerr := s.claimEntry(ctx, owner, repo, entry, ver, runner)
		if cerr != nil {
			if admitted {
				s.releaseMatrixCell(ctx, owner, repo, entry.RunID, entry.MatrixGroup, entry.JobID)
			}
			return nil, cerr
		}
		if gained == nil && admitted {
			s.releaseMatrixCell(ctx, owner, repo, entry.RunID, entry.MatrixGroup, entry.JobID)
		}
		if gained != nil {
			// A claimed entry stays in the window until it finishes: the head
			// is also how a lapsed lease is noticed (15 §3 requeue).
			claimed = gained
		}
	}
	s.trimHead(ctx, owner, repo, drop)
	for _, row := range indexDrop {
		s.removeQueueIndexJob(ctx, row.Repo, row.RunID, row.JobID)
	}
	return claimed, nil
}

// claimExpired reports a claim whose lease has lapsed.
func (s *Service) claimExpired(e *QueueEntry) bool {
	if e.LeaseUntil == nil {
		return true
	}
	until, err := time.Parse(dateTimeFmt, *e.LeaseUntil)
	if err != nil {
		return true
	}
	return s.nowUTC().After(until)
}

// claimEntry CASes one queued entry to this runner. It loads the job/run first
// (the claim response is self-contained), then writes the entry with the
// version the prober read — the CAS is the lock.
func (s *Service) claimEntry(ctx context.Context, owner, repo string, entry *QueueEntry, ver store.Version, runner *RunnerDoc) (*Claim, error) {
	job, run, err := s.loadJobAndRun(ctx, owner, repo, entry.RunID, entry.JobID)
	if err != nil || job == nil || run == nil {
		return nil, err
	}
	// A deployment wait timer (PAR-131) holds the job without consuming an
	// attempt and without a claim write: the entry stays queued, the run page
	// names the deadline, and the next probe after it offers the job. Checked
	// before the CAS so a waiting job is not churned every poll.
	if waiting, _ := deploymentWaitRemaining(job, s.nowUTC()); waiting {
		return nil, nil
	}
	// A deployment queue position (PAR-136) holds the job without consuming an
	// attempt: the environment serializes its deployments, so a waiter becomes
	// claimable only when the release path promotes it (the queue document is
	// the authority, this snapshot is its record).
	if job.Deployment != nil && job.Deployment.QueuePosition > 0 {
		return nil, nil
	}
	// Tenancy under the claim CAS (the authority, like the fork gate below): a
	// stale hint row or a hostile entry can never hand a repository's code to a
	// runner the admin did not allow. The job stays queued for an eligible
	// runner.
	if !runner.AllowsRepo(repoID(owner, repo)) {
		return nil, nil
	}
	// The group's visibility narrows the runner's policy (PAR-175); a group can
	// never widen it.
	if !runner.CanServeRepo(repoID(owner, repo)) {
		return nil, nil
	}
	// A draining runner claims nothing (PAR-181), and a runner that declares a
	// ceiling is not handed a job larger than it offers (PAR-176). Both leave
	// the job queued for another runner.
	if runner.IsDraining() || !RunnerOffers(runner, JobSizeOf(job)) {
		return nil, nil
	}
	// Fork gating under the claim CAS (the authority): a hostile or stale entry
	// cannot hand fork code to a runner that never opted in. The job stays
	// queued (returning no claim is not a refusal of the job).
	if !(job.Trusted && run.Trusted) && !runner.AllowForkRuns {
		return nil, nil
	}
	if job.Attempt != entry.Attempt || job.State != JobQueued {
		// The job moved on (cancelled, rerun, already claimed): retire the
		// entry so the head stops offering it, and drop its global hint row.
		entry.State = QueueDone
		entry.FinishedAt = ptr(s.nowUTC().Format(dateTimeFmt))
		entry.Version++
		body, _ := json.Marshal(entry) // cannot fail: the record is pure JSON types
		_, _ = store.PutBytes(ctx, s.Store, QueueEntryKey(owner, repo, entry.Seq), body,
			store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"})
		s.removeQueueIndexJob(ctx, repoID(owner, repo), entry.RunID, entry.JobID)
		return nil, nil
	}
	now := s.nowUTC()
	lease := now.Add(s.leaseFor(job))
	next := *entry
	next.State = QueueClaimed
	next.Runner = runner.ID
	next.ClaimedAt = ptr(now.Format(dateTimeFmt))
	next.LeaseUntil = ptr(lease.Format(dateTimeFmt))
	next.Version++
	body, _ := json.Marshal(&next) // cannot fail: the record is pure JSON types
	m, perr := store.PutBytes(ctx, s.Store, QueueEntryKey(owner, repo, entry.Seq), body,
		store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"})
	if perr != nil {
		if store.IsPreconditionFailed(perr) {
			return nil, nil // lost the race: the caller tries the next entry
		}
		return nil, perr
	}
	return &Claim{Entry: &next, EntryVersion: m.Version, Job: job, Run: run}, nil
}

// releaseClaim gives one claim back after the post-claim job write failed (the
// runner does not get a job the server could not mark running).
func (s *Service) releaseClaim(ctx context.Context, owner, repo string, claim *Claim) {
	if claim == nil || claim.Entry == nil {
		return
	}
	next := *claim.Entry
	next.State = QueueQueued
	next.Runner = ""
	next.ClaimedAt = nil
	next.LeaseUntil = nil
	next.Version++
	_, _ = store.PutBytes(ctx, s.Store, QueueEntryKey(owner, repo, claim.Entry.Seq),
		mustJSON(&next), store.PutOptions{Mode: store.PutUpdate, IfVersion: claim.EntryVersion, ContentType: "application/json"})
}

// leaseFor is the claim lease: the job's timeout plus a grace window, floored
// at the configured base lease (threads.job_lease, default 15 minutes) and
// capped at 30 minutes (log frames renew it).
func (s *Service) leaseFor(job *Job) time.Duration {
	lease := s.jobLease()
	if job.Spec != nil && job.Spec.TimeoutMinutes > 0 {
		lease = time.Duration(job.Spec.TimeoutMinutes)*time.Minute + 5*time.Minute
	}
	if lease > 30*time.Minute {
		lease = 30 * time.Minute
	}
	return lease
}

// jobLease is the configured base lease (threads.job_lease), default 15m.
func (s *Service) jobLease() time.Duration {
	if s != nil && s.JobLease > 0 {
		return s.JobLease
	}
	return 15 * time.Minute
}

// requeueEntry moves one expired claim to a new attempt: the job record first
// (identity authority), then the entry. A crash between the two leaves the
// entry claimed-but-expired, which the next probe advances again.
func (s *Service) requeueEntry(ctx context.Context, owner, repo string, entry *QueueEntry, ver store.Version) error {
	attempt := entry.Attempt + 1
	if _, err := s.UpdateJob(ctx, repoID(owner, repo), entry.RunID, entry.JobID, JobUpdate{
		State: JobQueued, Attempt: attempt, ClearTimings: true,
	}); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if entry.MaxParallel > 0 && entry.MatrixGroup != "" {
		s.releaseMatrixCell(ctx, owner, repo, entry.RunID, entry.MatrixGroup, entry.JobID)
	}
	next := *entry
	next.State = QueueQueued
	next.Attempt = attempt
	next.Runner = ""
	next.ClaimedAt = nil
	next.LeaseUntil = nil
	next.Version++
	body, _ := json.Marshal(&next) // cannot fail: the record is pure JSON types
	_, perr := store.PutBytes(ctx, s.Store, QueueEntryKey(owner, repo, entry.Seq), body,
		store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"})
	if perr != nil && !store.IsPreconditionFailed(perr) {
		return perr
	}
	// The old lease is gone: drop it from the registry's display state (best
	// effort; the runner may already be dead).
	s.clearRunnerCurrentJob(ctx, entry.Runner, repoID(owner, repo), entry.RunID, entry.JobID)
	return nil
}

// renewLease extends the claim lease after accepted attempt traffic (best
// effort, coalesced by the caller: one write per minute at most).
func (s *Service) renewLease(ctx context.Context, owner, repo string, entry *QueueEntry, job *Job) *string {
	now := s.nowUTC()
	until := now.Add(s.leaseFor(job)).Format(dateTimeFmt)
	key := QueueEntryKey(owner, repo, entry.Seq)
	wrote := false
	_, err := s.casUpdate(ctx, key, 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var e QueueEntry
		if jerr := json.Unmarshal(cur, &e); jerr != nil {
			return nil, false, fmt.Errorf("%w: queue entry: %v", ErrCorrupt, jerr)
		}
		if e.LeaseUntil != nil {
			if prev, perr := time.Parse(dateTimeFmt, *e.LeaseUntil); perr == nil && prev.After(now.Add(s.leaseFor(job)/2)) {
				return nil, false, nil // recently renewed; coalesce
			}
		}
		e.LeaseUntil = &until
		e.Version++
		wrote = true
		body, _ := json.Marshal(&e) // cannot fail: the record is pure JSON types
		return body, true, nil
	})
	if err != nil || !wrote {
		return nil
	}
	return &until
}

// finishEntry retires a queue entry after a terminal attempt.
func (s *Service) finishEntry(ctx context.Context, owner, repo, seq string) {
	if seq == "" {
		return
	}
	key := QueueEntryKey(owner, repo, seq)
	now := s.nowUTC().Format(dateTimeFmt)
	var runID, jobID, matrixGroup string
	var maxParallel int
	_, _ = s.casUpdate(ctx, key, 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var e QueueEntry
		if jerr := json.Unmarshal(cur, &e); jerr != nil {
			return nil, false, fmt.Errorf("%w: queue entry: %v", ErrCorrupt, jerr)
		}
		runID, jobID, matrixGroup, maxParallel = e.RunID, e.JobID, e.MatrixGroup, e.MaxParallel
		if e.State == QueueDone {
			return nil, false, nil
		}
		e.State = QueueDone
		e.FinishedAt = &now
		e.LeaseUntil = nil
		e.Version++
		body, _ := json.Marshal(&e) // cannot fail: the record is pure JSON types
		return body, true, nil
	})
	s.trimHead(ctx, owner, repo, map[string]bool{seq: true})
	if runID != "" {
		s.removeQueueIndexJob(ctx, repoID(owner, repo), runID, jobID)
	}
	if maxParallel > 0 && matrixGroup != "" {
		s.releaseMatrixCell(ctx, owner, repo, runID, matrixGroup, jobID)
	}
}

// setEntryCancel CASes the cancel signal onto one queue entry (an unclaimed
// pending job is marked done at the same time: nobody is running it).
func (s *Service) setEntryCancel(ctx context.Context, owner, repo, seq string, running bool) {
	if seq == "" {
		return
	}
	key := QueueEntryKey(owner, repo, seq)
	now := s.nowUTC().Format(dateTimeFmt)
	var runID, jobID string
	retired := false
	_, _ = s.casUpdate(ctx, key, 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var e QueueEntry
		if jerr := json.Unmarshal(cur, &e); jerr != nil {
			return nil, false, fmt.Errorf("%w: queue entry: %v", ErrCorrupt, jerr)
		}
		runID, jobID = e.RunID, e.JobID
		e.CancelRequested = true
		if !running {
			e.State = QueueDone
			e.FinishedAt = &now
			retired = true
		}
		e.Version++
		body, _ := json.Marshal(&e) // cannot fail: the record is pure JSON types
		return body, true, nil
	})
	if retired {
		s.removeQueueIndexJob(ctx, repoID(owner, repo), runID, jobID)
	}
}

// --- wake-up hub -------------------------------------------------------------

// globalWakeKey is the hub key every instance runner fetch waits on. The
// per-repo keys still exist for the per-repository internals/tests, but a
// global fetch must hear about an enqueue in ANY repository.
const globalWakeKey = "*"

// queueHub wakes long-polling fetches in this process when an enqueue lands.
// It is an optimisation, never the correctness mechanism: a fetch also re-probes
// on a short timer, so an enqueue on another instance is still seen.
type queueHub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]bool
}

var fetchHub = &queueHub{subs: map[string]map[chan struct{}]bool{}}

// subscribe registers a wake channel for one repo; the returned cancel closes
// it (sender owns the channel, 13 channel rule).
func (h *queueHub) subscribe(repo string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[repo] == nil {
		h.subs[repo] = map[chan struct{}]bool{}
	}
	h.subs[repo][ch] = true
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			if set, ok := h.subs[repo]; ok {
				delete(set, ch)
				if len(set) == 0 {
					delete(h.subs, repo)
				}
			}
			h.mu.Unlock()
			close(ch)
		})
	}
}

// subscribeGlobal registers a channel woken by an enqueue in any repository.
func (h *queueHub) subscribeGlobal() (<-chan struct{}, func()) {
	return h.subscribe(globalWakeKey)
}

// wake nudges every fetch waiting on one repo, and every global fetch: an
// instance runner does not know which repository an enqueue belongs to.
func (h *queueHub) wake(repo string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, key := range []string{repo, globalWakeKey} {
		for ch := range h.subs[key] {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

// ptr is a convenience for *string fields.
func ptr(s string) *string { return &s }

// QueueRow is one pending job as the queue API/page renders it, in CLAIM order
// (PAR-155): highest priority first, then oldest sequence.
type QueueRow struct {
	Seq        string   `json:"seq"`
	RunID      string   `json:"run_id"`
	JobID      string   `json:"job_id"`
	Priority   int      `json:"priority"`
	Labels     []string `json:"labels"`
	State      string   `json:"state"`
	Fork       bool     `json:"fork,omitempty"`
	Attempt    int      `json:"attempt"`
	EnqueuedAt string   `json:"enqueued_at"`
}

// QueueSnapshot is the repository's pending window in claim order, plus the
// pause marker. It is a read (repository read role); the head is bounded, so
// the cost is one head GET plus one GET per pending entry.
type QueueSnapshot struct {
	Repo   string     `json:"repo"`
	Paused bool       `json:"paused"`
	Rows   []QueueRow `json:"rows"`
}
