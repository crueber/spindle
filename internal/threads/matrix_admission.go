package threads

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// Matrix admission (PAR-052, docs/threads.md §3.3).
//
// `strategy.max-parallel: N` bounds how many cells of ONE matrix run at the
// same time. The bounds are a per-run record beside the job records:
//
//	repos/<o>/<r>/threads/runs/<run>/matrix.json
//
// It is keyed by the YAML job key and holds the slot each claimed cell took. A
// claim of a bounded cell CASes a slot before the queue-entry CAS, so two
// probes cannot both admit a third cell when the bound is two. A job that
// finishes (or whose attempt is requeued) releases its slot. Only workflows
// that declare max-parallel pay the read/write; an ordinary job never touches
// the record.
//
// ### Concurrency
//
// Hazard: two runners admit the (N+1)th cell. Avoidance: the record is a CAS —
// the count and the add are one compare-and-swap. Hazard: a dead runner's slot
// outlives its claim. Avoidance: each slot carries a deadline; the next
// admission re-reads the cell's queue entry once the deadline passes and keeps
// the slot only if the claim's lease is still live. A granted slot the claim
// CAS then loses is released by the caller.

// matrixSlot is one running cell's admission.
type matrixSlot struct {
	// Seq is the queue entry the slot was granted for, so a lapsed deadline can
	// re-check the real claim.
	Seq string `json:"seq"`
	// Until is when the slot must be re-validated (the claim lease at grant
	// time, extended by a re-check).
	Until string `json:"until"`
}

type matrixGroupAdmission struct {
	Max     int                   `json:"max"`
	Running map[string]matrixSlot `json:"running,omitempty"`
}

type matrixAdmissionDoc struct {
	RunID   string                           `json:"run_id"`
	Groups  map[string]*matrixGroupAdmission `json:"groups"`
	Version int                              `json:"version"`
}

// matrixGroupOf is the job's matrix group ("" for a plain job).
func matrixGroupOf(job *Job) string {
	if job == nil || job.Spec == nil {
		return ""
	}
	return job.Spec.MatrixGroup
}

// maxParallelOf is the job's per-matrix admission bound (0 = none).
func maxParallelOf(job *Job) int {
	if job == nil || job.Spec == nil || job.Spec.Strategy == nil {
		return 0
	}
	return job.Spec.Strategy.MaxParallel
}

// matrixAdmissionLease is how long a granted slot is trusted before it is
// re-validated against the claim's own lease. It is the queue's lease ceiling
// (queue.go leaseFor caps at 30 minutes), so a live attempt's own renewal always
// wins the re-check.
const matrixAdmissionLease = 30 * time.Minute

// admitMatrixCell grants one queue entry a slot in its matrix group. It returns
// false when the group is full (the cell stays queued, and the probe moves on).
func (s *Service) admitMatrixCell(ctx context.Context, owner, repo string, entry *QueueEntry) (bool, error) {
	if entry == nil || entry.MaxParallel <= 0 || entry.MatrixGroup == "" {
		return true, nil
	}
	key := MatrixAdmissionKey(owner, repo, entry.RunID)
	now := s.nowUTC()
	admitted := false
	_, err := s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		// The closure runs again after a lost CAS; a grant from the losing
		// attempt must not leak into the retry.
		admitted = false
		doc := &matrixAdmissionDoc{RunID: entry.RunID}
		if cur != nil {
			if jerr := json.Unmarshal(cur, doc); jerr != nil {
				return nil, false, fmt.Errorf("%w: matrix admission: %v", ErrCorrupt, jerr)
			}
		}
		if doc.Groups == nil {
			doc.Groups = map[string]*matrixGroupAdmission{}
		}
		g := doc.Groups[entry.MatrixGroup]
		if g == nil {
			g = &matrixGroupAdmission{Running: map[string]matrixSlot{}}
			doc.Groups[entry.MatrixGroup] = g
		}
		if g.Running == nil {
			g.Running = map[string]matrixSlot{}
		}
		// The declared bound is authoritative on every pass (a re-run of a
		// changed file must not keep a stale, larger bound).
		g.Max = entry.MaxParallel
		s.pruneMatrixSlots(ctx, owner, repo, g, now)
		if _, ok := g.Running[entry.JobID]; ok {
			// Another probe already admitted this cell. This caller must not
			// claim it again — and must not release the winner's slot when its
			// own claim CAS loses; the winner holds it until it finishes.
			return nil, false, nil
		}
		if len(g.Running) >= g.Max {
			return nil, false, nil
		}
		g.Running[entry.JobID] = matrixSlot{Seq: entry.Seq, Until: now.Add(matrixAdmissionLease).Format(dateTimeFmt)}
		admitted = true
		doc.Version++
		return mustJSON(doc), true, nil
	})
	if err != nil {
		return false, err
	}
	return admitted, nil
}

// pruneMatrixSlots drops cells whose claim is gone. A slot past its deadline is
// re-checked against its queue entry: a live claim refreshes the deadline (the
// attempt renewed its lease), a done/requeued entry frees the slot.
func (s *Service) pruneMatrixSlots(ctx context.Context, owner, repo string, g *matrixGroupAdmission, now time.Time) {
	for jobID, slot := range g.Running {
		until, err := time.Parse(dateTimeFmt, slot.Until)
		if err == nil && until.After(now) {
			continue
		}
		entry := s.loadQueueEntry(ctx, owner, repo, slot.Seq)
		if entry == nil || entry.State != QueueClaimed || entry.LeaseUntil == nil {
			delete(g.Running, jobID)
			continue
		}
		lease, lerr := time.Parse(dateTimeFmt, *entry.LeaseUntil)
		if lerr != nil || !lease.After(now) {
			delete(g.Running, jobID)
			continue
		}
		slot.Until = lease.Format(dateTimeFmt)
		g.Running[jobID] = slot
	}
}

// loadQueueEntry reads one queue entry; nil when absent.
func (s *Service) loadQueueEntry(ctx context.Context, owner, repo, seq string) *QueueEntry {
	if seq == "" {
		return nil
	}
	raw, _, err := s.getJSON(ctx, QueueEntryKey(owner, repo, seq))
	if err != nil || raw == nil {
		return nil
	}
	var entry QueueEntry
	if jerr := json.Unmarshal(raw, &entry); jerr != nil {
		return nil
	}
	return &entry
}

// releaseMatrixCell frees one cell's slot after it finished, was requeued, or
// lost the claim race. A missing record or slot is a no-op.
func (s *Service) releaseMatrixCell(ctx context.Context, owner, repo, runID, group, jobID string) {
	if runID == "" || group == "" || jobID == "" {
		return
	}
	key := MatrixAdmissionKey(owner, repo, runID)
	_, _ = s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var doc matrixAdmissionDoc
		if jerr := json.Unmarshal(cur, &doc); jerr != nil {
			return nil, false, fmt.Errorf("%w: matrix admission: %v", ErrCorrupt, jerr)
		}
		g := doc.Groups[group]
		if g == nil {
			return nil, false, nil
		}
		if _, ok := g.Running[jobID]; !ok {
			return nil, false, nil
		}
		delete(g.Running, jobID)
		if len(g.Running) == 0 {
			delete(doc.Groups, group)
		}
		doc.Version++
		return mustJSON(&doc), true, nil
	})
}
