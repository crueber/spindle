package threads

// Per-organization concurrency caps (PAR-153).
//
// A repository's cap (capacity.go) bounds that repository; the organization cap
// bounds live runs across EVERY repository the organization owns. The reason a
// single index document exists is cost: counting across an organization would
// otherwise be one LIST per repository on every creation. Instead:
//
//	orgs/<org>/threads/cap.json   {live:[…], held:[…], version}
//
// "live" names the admitted runs (queued/running) across the org; "held" names
// the runs created above the cap, oldest first. The same one-reason rule as the
// repository cap applies: a held run's header carries
// "organization concurrency cap N reached" and every queued job reports it
// through QueueReason.
//
// ### Concurrency
//
// Hazard: two creations both see live < cap and both admit, overshooting.
// Avoidance: the cap is an admission heuristic, not a lock — a burst can
// overshoot by the number of simultaneous creations, exactly like the
// repository cap, and the documented guarantee is steady state. The document
// CAS is still the arbitrator for the list itself (a loser re-reads).
// Hazard: a crash between run creation and the index write leaks a slot
// (`live` too small); a crash between a terminal fold and the release leaks a
// stale live entry (`live` too large, so runs stay held). Avoidance: on the
// admission path above the cap, up to orgCapPrunePerPass oldest entries are
// checked against their run headers and dead ones dropped; the release path
// also checks the held entries it pops. A lost write is repaired by the next
// admission or fold in any repository of the org, never by a timer. No lock is
// held across a store call.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/store"
)

const (
	// maxOrgHeld bounds the held list. A run created above the cap beyond this
	// bound is a named creation error, never an untracked hold that could never
	// be released (the alternative — an unbounded document — is worse).
	maxOrgHeld = 1000
	// orgCapPrunePerPass bounds the liveness checks one admission may perform
	// while the org is at its cap. Each check is one run-header GET.
	orgCapPrunePerPass = 16
)

// OrgCapKey returns orgs/<org>/threads/cap.json.
func OrgCapKey(org string) string { return "orgs/" + org + "/threads/cap.json" }

// OrgCapEntry is one run in the organization live/held index.
type OrgCapEntry struct {
	Repo      string `json:"repo"` // owner/name
	RunID     string `json:"run_id"`
	CreatedAt string `json:"created_at"`
}

// OrgCapDoc is the organization cap index.
type OrgCapDoc struct {
	Live      []OrgCapEntry `json:"live,omitempty"`
	Held      []OrgCapEntry `json:"held,omitempty"`
	UpdatedAt string        `json:"updated_at"`
	Version   int           `json:"version"`
}

// orgCapacityHoldReason names the organization cap on the run header and in
// the API/UI. It is distinct from capacityHoldReason so a reader can tell which
// cap is waiting.
func orgCapacityHoldReason(limit int) string {
	return fmt.Sprintf("organization concurrency cap %d reached", limit)
}

func orgCapEntryOf(repo, runID, now string) OrgCapEntry {
	return OrgCapEntry{Repo: repo, RunID: runID, CreatedAt: now}
}

func orgCapHas(list []OrgCapEntry, repo, runID string) bool {
	for _, e := range list {
		if e.Repo == repo && e.RunID == runID {
			return true
		}
	}
	return false
}

func orgCapRemove(list []OrgCapEntry, repo, runID string) []OrgCapEntry {
	out := list[:0]
	for _, e := range list {
		if e.Repo == repo && e.RunID == runID {
			continue
		}
		out = append(out, e)
	}
	return out
}

// loadOrgCapDoc reads the organization cap index (nil when absent). It is the
// admin-facing usage projection; admission uses the same read inline.
func (s *Service) loadOrgCapDoc(ctx context.Context, org string) (*OrgCapDoc, error) {
	raw, _, err := s.getJSON(ctx, OrgCapKey(org))
	if err != nil || raw == nil {
		return nil, err
	}
	var doc OrgCapDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		return nil, fmt.Errorf("%w: organization cap index: %v", ErrCorrupt, jerr)
	}
	return &doc, nil
}

// applyOrgCapGate decides whether a newly written run must wait for an
// organization slot. It returns true when the run is held (the caller stamps the
// reason and skips the enqueue).
func (s *Service) applyOrgCapGate(ctx context.Context, org, repo, runID string, limit int, now string) (bool, error) {
	for attempt := 0; attempt < 4; attempt++ {
		raw, ver, err := s.getJSON(ctx, OrgCapKey(org))
		if err != nil {
			return false, err
		}
		doc := &OrgCapDoc{}
		if raw != nil {
			if jerr := json.Unmarshal(raw, doc); jerr != nil {
				return false, fmt.Errorf("%w: organization cap index: %v", ErrCorrupt, jerr)
			}
		}
		if orgCapHas(doc.Live, repo, runID) {
			return false, nil
		}
		if orgCapHas(doc.Held, repo, runID) {
			return true, nil
		}
		// Fast path: under the cap, admit without any liveness checks (a stale
		// entry only makes admission conservative).
		if len(doc.Live) < limit {
			doc.Live = append(doc.Live, orgCapEntryOf(repo, runID, now))
			if err := s.putOrgCapDocAt(ctx, org, doc, ver); err != nil {
				if store.IsPreconditionFailed(err) {
					continue
				}
				return false, err
			}
			return false, nil
		}
		// At/over the cap: prune a bounded slice of the oldest entries so a
		// crash-leaked slot does not hold the organization forever.
		pruned := s.pruneOrgCapEntries(ctx, doc.Live, orgCapPrunePerPass)
		if len(pruned) != len(doc.Live) {
			doc.Live = pruned
			doc.UpdatedAt = now
			doc.Version++
			opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
			if _, perr := store.PutBytes(ctx, s.Store, OrgCapKey(org), mustJSON(doc), opts); perr != nil {
				if store.IsPreconditionFailed(perr) {
					continue
				}
				return false, perr
			}
			continue // re-read: a freed slot changes the decision
		}
		if len(doc.Held) >= maxOrgHeld {
			return false, fmt.Errorf("%w: organization cap %d reached and %d runs are already held; a maintainer must let the backlog drain", ErrUnavailable, limit, len(doc.Held))
		}
		doc.Held = append(doc.Held, orgCapEntryOf(repo, runID, now))
		if err := s.putOrgCapDocAt(ctx, org, doc, ver); err != nil {
			if store.IsPreconditionFailed(err) {
				continue
			}
			return false, err
		}
		return true, nil
	}
	return false, fmt.Errorf("%w: organization cap index changed concurrently; retry", ErrUnavailable)
}

// pruneOrgCapEntries returns the entries that are still live, checking at most
// the first n (oldest) entries and keeping the rest unchecked (a later pass
// re-checks them). An unreadable run header is treated as dead — the entry is a
// hint, and the run's own header is authoritative.
func (s *Service) pruneOrgCapEntries(ctx context.Context, entries []OrgCapEntry, n int) []OrgCapEntry {
	if n <= 0 || n > len(entries) {
		n = len(entries)
	}
	out := make([]OrgCapEntry, 0, len(entries))
	checked := 0
	for _, e := range entries {
		if checked < n {
			checked++
			if !s.orgCapEntryLive(ctx, e) {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

// orgCapEntryLive reports whether the entry names a live run.
func (s *Service) orgCapEntryLive(ctx context.Context, e OrgCapEntry) bool {
	owner, repo, ok := strings.Cut(e.Repo, "/")
	if !ok {
		return false
	}
	return s.runLive(ctx, owner, repo, e.RunID)
}

// releaseOrgHeldRuns runs after a terminal fold: it removes the folded run from
// the organization index and admits held runs, oldest first, while the org is
// under its cap. It is called only when the folded run recorded a cap, so an
// uncapped organization pays no reads.
func (s *Service) releaseOrgHeldRuns(ctx context.Context, org, repo, runID string) {
	for attempt := 0; attempt < 8; attempt++ {
		raw, ver, err := s.getJSON(ctx, OrgCapKey(org))
		if err != nil || raw == nil {
			return
		}
		doc := &OrgCapDoc{}
		if jerr := json.Unmarshal(raw, doc); jerr != nil {
			slog.Warn("threads: organization cap index corrupt; no release", "org", org, "err", jerr)
			return
		}
		live := orgCapRemove(doc.Live, repo, runID)
		held := orgCapRemove(doc.Held, repo, runID)
		limit := s.orgCap(ctx, org)
		if limit <= 0 {
			// The cap was removed: every held run is admitted now, whatever
			// folded last. Without this a cap removal could leave runs waiting
			// forever (nothing else triggers a release once the live set
			// drains).
			for _, h := range held {
				owner, name, ok := strings.Cut(h.Repo, "/")
				if !ok || !s.clearOrgHold(ctx, owner, name, h.RunID) {
					continue
				}
				s.enqueueHeldRun(ctx, owner, name, h.RunID)
				live = append(live, h)
			}
			doc.Live, doc.Held = live, nil
			if err := s.putOrgCapDocAt(ctx, org, doc, ver); err != nil && !store.IsPreconditionFailed(err) {
				slog.Warn("threads: organization cap release write", "org", org, "err", err)
			}
			return
		}
		if len(live) >= limit || len(held) == 0 {
			doc.Live, doc.Held = live, held
			if err := s.putOrgCapDocAt(ctx, org, doc, ver); err != nil && !store.IsPreconditionFailed(err) {
				slog.Warn("threads: organization cap release write", "org", org, "err", err)
			}
			return
		}
		// Take the oldest held entry whose run can actually be released.
		sort.SliceStable(held, func(i, j int) bool {
			if held[i].CreatedAt != held[j].CreatedAt {
				return held[i].CreatedAt < held[j].CreatedAt
			}
			if held[i].Repo != held[j].Repo {
				return held[i].Repo < held[j].Repo
			}
			return held[i].RunID < held[j].RunID
		})
		next := held[0]
		owner, name, ok := strings.Cut(next.Repo, "/")
		if !ok {
			doc.Live, doc.Held = live, held[1:]
			if werr := s.putOrgCapDocAt(ctx, org, doc, ver); werr != nil {
				if store.IsPreconditionFailed(werr) {
					continue
				}
				return
			}
			continue
		}
		if !s.clearOrgHold(ctx, owner, name, next.RunID) {
			// Terminal, deleted, or already released: drop the entry.
			doc.Live, doc.Held = live, held[1:]
			if werr := s.putOrgCapDocAt(ctx, org, doc, ver); werr != nil {
				if store.IsPreconditionFailed(werr) {
					continue
				}
				return
			}
			continue
		}
		s.enqueueHeldRun(ctx, owner, name, next.RunID)
		doc.Live, doc.Held = append(live, next), held[1:]
		if err := s.putOrgCapDocAt(ctx, org, doc, ver); err != nil {
			if store.IsPreconditionFailed(err) {
				continue
			}
			slog.Warn("threads: organization cap release write", "org", org, "err", err)
			return
		}
		return
	}
}

// putOrgCapDocAt writes the index at its org key.
func (s *Service) putOrgCapDocAt(ctx context.Context, org string, doc *OrgCapDoc, ver store.Version) error {
	doc.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
	doc.Version++
	opts := store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}
	if ver != "" {
		opts = store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
	}
	_, err := store.PutBytes(ctx, s.Store, OrgCapKey(org), mustJSON(doc), opts)
	return err
}

// enqueueHeldRun loads one released run's jobs and enqueues every still-queued
// one. A load failure is logged by the caller's absence of queue entries; the
// next fold retries (the run is now live).
func (s *Service) enqueueHeldRun(ctx context.Context, owner, repo, runID string) {
	jobs, err := s.loadJobs(ctx, owner, repo, runID)
	if err != nil {
		slog.Warn("threads: organization cap release load", "repo", repoID(owner, repo), "run", runID, "err", err)
		return
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
}

// clearOrgHold clears an organization-cap hold on one run; it reports whether
// THIS caller won the CAS (only the winner enqueues).
func (s *Service) clearOrgHold(ctx context.Context, owner, repo, runID string) bool {
	won := false
	_, err := s.casUpdate(ctx, RunKey(owner, repo, runID), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var r Run
		if jerr := json.Unmarshal(cur, &r); jerr != nil {
			return nil, false, fmt.Errorf("%w: run %s: %v", ErrCorrupt, runID, jerr)
		}
		if r.HoldReason == "" || !strings.HasPrefix(r.HoldReason, "organization concurrency cap") || r.State != RunQueued {
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
