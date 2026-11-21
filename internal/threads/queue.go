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
	// Runner is the claiming runner id ("" while queued).
	Runner string `json:"runner,omitempty"`
	// CancelRequested is the operator's cancel signal the runner acks.
	CancelRequested bool `json:"cancel_requested,omitempty"`

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
	// Pending lists live sequence numbers, oldest first, bounded to
	// queueWindowMax.
	Pending   []string `json:"pending"`
	UpdatedAt string   `json:"updated_at"`
	Version   int      `json:"version"`
}

// newQueueSeq mints one queue sequence.
func newQueueSeq(now time.Time) (string, error) {
	id, err := newRunID(now)
	if err != nil {
		return "", err
	}
	return id, nil
}

// enqueueJob appends one job to the repo queue. needs are the job ids this job
// waits on (resolved by the caller from the run's job set, 15 §4.1). The entry
// is the truth; the head append is a bounded projection, so a head failure is
// logged by the caller and repaired by threads-gc — it never fails run
// creation.
func (s *Service) enqueueJob(ctx context.Context, owner, repo string, job *Job, needs []string) (*QueueEntry, error) {
	seq, err := newQueueSeq(s.nowUTC())
	if err != nil {
		return nil, fmt.Errorf("%w: mint queue seq: %v", ErrUnavailable, err)
	}
	now := s.nowUTC().Format(dateTimeFmt)
	entry := &QueueEntry{
		Seq: seq, RunID: job.RunID, JobID: job.ID,
		Labels: jobLabels(job), Needs: needs, Attempt: job.Attempt, State: QueueQueued,
		EnqueuedAt: now, Version: 1,
	}
	body, _ := json.Marshal(entry) // cannot fail: the record is pure JSON types
	if perr := s.putCreate(ctx, QueueEntryKey(owner, repo, seq), body); perr != nil {
		if store.IsPreconditionFailed(perr) {
			return nil, fmt.Errorf("%w: queue entry %s exists", ErrConflict, seq)
		}
		return nil, perr
	}
	if herr := s.appendHead(ctx, owner, repo, seq); herr != nil {
		return entry, herr
	}
	return entry, nil
}

// jobLabels is the normalized label set the queue matches on.
func jobLabels(job *Job) []string {
	var out []string
	if job.Spec != nil {
		out = append(out, job.Spec.RunsOn...)
	}
	if len(out) == 0 && job.RunsOn != "" {
		out = strings.Split(job.RunsOn, ",")
	}
	return normalizeLabels(out)
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

// appendHead CAS-appends one sequence to the head's pending window.
func (s *Service) appendHead(ctx context.Context, owner, repo, seq string) error {
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
		head.Pending = append(head.Pending, seq)
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
	sort.Strings(head.Pending)
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
	head, _, err := s.loadHead(ctx, owner, repo)
	if err != nil || head == nil {
		return nil, err
	}
	drop := map[string]bool{}
	jobs := jobCache{}
	var claimed *Claim
	attempts := 0
	for _, seq := range head.Pending {
		if attempts >= queueScanMax || claimed != nil {
			break
		}
		if skip[seq] {
			continue
		}
		entry, ver, eerr := s.loadEntry(ctx, owner, repo, seq)
		if eerr != nil {
			return nil, eerr
		}
		if entry == nil || entry.State == QueueDone {
			drop[seq] = true
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
		if !labelsMatch(labels, entry.Labels) {
			continue
		}
		// `needs:` eligibility (15 §4.1). A job is offered only when every
		// dependency concluded successfully; a job whose dependency failed or
		// was cancelled can never run, so it is skipped (terminalised
		// cancelled) here rather than left queued forever. Waiting entries do
		// not consume the scan budget: the dependency's own entry must still
		// be reachable in this probe.
		if len(entry.Needs) > 0 {
			gate, gerr := s.needsGate(ctx, owner, repo, entry, jobs)
			if gerr != nil {
				return nil, gerr
			}
			switch gate {
			case needsBlocked:
				if _, serr := s.skipJob(ctx, owner, repo, entry); serr != nil {
					return nil, serr
				}
				drop[seq] = true
				continue
			case needsWaiting:
				continue
			}
		}
		attempts++
		gained, cerr := s.claimEntry(ctx, owner, repo, entry, ver, runner)
		if cerr != nil {
			return nil, cerr
		}
		if gained != nil {
			// A claimed entry stays in the window until it finishes: the head
			// is also how a lapsed lease is noticed (15 §3 requeue).
			claimed = gained
		}
	}
	s.trimHead(ctx, owner, repo, drop)
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
	if job.Attempt != entry.Attempt || job.State != JobQueued {
		// The job moved on (cancelled, rerun, already claimed): retire the
		// entry so the head stops offering it.
		entry.State = QueueDone
		entry.FinishedAt = ptr(s.nowUTC().Format(dateTimeFmt))
		entry.Version++
		body, _ := json.Marshal(entry) // cannot fail: the record is pure JSON types
		_, _ = store.PutBytes(ctx, s.Store, QueueEntryKey(owner, repo, entry.Seq), body,
			store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"})
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
// at 15 minutes and capped at 30 minutes (log frames renew it).
func (s *Service) leaseFor(job *Job) time.Duration {
	lease := 15 * time.Minute
	if job.Spec != nil && job.Spec.TimeoutMinutes > 0 {
		lease = time.Duration(job.Spec.TimeoutMinutes)*time.Minute + 5*time.Minute
	}
	if lease > 30*time.Minute {
		lease = 30 * time.Minute
	}
	return lease
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
	_, _ = s.casUpdate(ctx, key, 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var e QueueEntry
		if jerr := json.Unmarshal(cur, &e); jerr != nil {
			return nil, false, fmt.Errorf("%w: queue entry: %v", ErrCorrupt, jerr)
		}
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
}

// setEntryCancel CASes the cancel signal onto one queue entry (an unclaimed
// pending job is marked done at the same time: nobody is running it).
func (s *Service) setEntryCancel(ctx context.Context, owner, repo, seq string, running bool) {
	if seq == "" {
		return
	}
	key := QueueEntryKey(owner, repo, seq)
	now := s.nowUTC().Format(dateTimeFmt)
	_, _ = s.casUpdate(ctx, key, 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var e QueueEntry
		if jerr := json.Unmarshal(cur, &e); jerr != nil {
			return nil, false, fmt.Errorf("%w: queue entry: %v", ErrCorrupt, jerr)
		}
		e.CancelRequested = true
		if !running {
			e.State = QueueDone
			e.FinishedAt = &now
		}
		e.Version++
		body, _ := json.Marshal(&e) // cannot fail: the record is pure JSON types
		return body, true, nil
	})
}

// --- wake-up hub -------------------------------------------------------------

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

// wake nudges every fetch waiting on one repo (non-blocking sends).
func (h *queueHub) wake(repo string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[repo] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// ptr is a convenience for *string fields.
func ptr(s string) *string { return &s }
