package threads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/store"
)

// Concurrency groups (15 §1/§4, pass B2): one in-flight run per group per
// repository.
//
//	repos/<o>/<r>/threads/concurrency/<slug>-<hash8>.json   holder index
//
// The record names the run holding the group. Admission happens on the CLAIM
// path (after the queue entry CAS, before the job is marked running), so no
// extra state is written for a job that never runs and two runners can never
// admit two runs into one group: the CAS on the holder record is the lock. A
// job whose group is held by ANOTHER run gives its queue entry back and waits
// for the next probe (the release path clears the index when the holder run
// folds terminal). A job of the HOLDING run passes: a workflow-level group
// belongs to the run, so its sibling jobs must not serialize each other.
//
// `cancel-in-progress` is applied at admission: a job that wants a group held
// by a live run cancels that run through the same terminal path the operator's
// cancel uses, then takes the group.

// ConcurrencyPrefix is the holder-index family of one repository.
func ConcurrencyPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/threads/concurrency/"
}

// ConcurrencyKey is one group's holder record.
func ConcurrencyKey(owner, repo, group string) string {
	return ConcurrencyPrefix(owner, repo) + concurrencySlug(group) + ".json"
}

// concurrencySlug maps a group name onto a key segment: the checks charset
// (lowercased, invalid runs → "-", capped) plus a hash suffix, so two groups
// whose slugs collide never share an index entry.
func concurrencySlug(group string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(group)))
	return slugSegment(group) + "-" + hex.EncodeToString(sum[:4])
}

// ConcurrencyDoc is one group's holder record.
type ConcurrencyDoc struct {
	Group string `json:"group"`
	// RunID is the run currently allowed to execute in this group ("" = free).
	RunID   string `json:"run_id"`
	Since   string `json:"since"`
	Version int    `json:"version"`
}

// admitConcurrency reports whether this job may run now, taking the group when
// it is free (or held by the same run). spec is the claiming job's spec.
func (s *Service) admitConcurrency(ctx context.Context, owner, repo, runID string, spec *Spec) (bool, error) {
	if spec == nil || spec.Concurrency == nil {
		return true, nil
	}
	group := strings.TrimSpace(spec.Concurrency.Group)
	if group == "" {
		return true, nil
	}
	for attempt := 0; attempt < 4; attempt++ {
		ok, holder, err := s.tryAcquireGroup(ctx, owner, repo, group, runID)
		if err != nil || ok {
			return ok, err
		}
		if !spec.Concurrency.CancelInProgress {
			return false, nil
		}
		// Cancel the holder run through the operator's terminal path, which
		// folds it and releases this group; the next iteration takes it.
		if _, cerr := s.cancelRunSystem(ctx, owner, repo, holder); cerr != nil {
			return false, cerr
		}
	}
	return false, nil
}

// tryAcquireGroup CASes one group's holder to runID. It returns (false, holder,
// nil) when a different live run holds it.
func (s *Service) tryAcquireGroup(ctx context.Context, owner, repo, group, runID string) (bool, string, error) {
	key := ConcurrencyKey(owner, repo, group)
	for attempt := 0; attempt < 4; attempt++ {
		raw, ver, err := s.getJSON(ctx, key)
		if err != nil {
			return false, "", err
		}
		doc := &ConcurrencyDoc{Group: group}
		if raw != nil {
			if jerr := json.Unmarshal(raw, doc); jerr != nil {
				return false, "", fmt.Errorf("%w: concurrency %s: %v", ErrCorrupt, group, jerr)
			}
		}
		if doc.RunID != "" && doc.RunID != runID {
			holder, rerr := s.loadRun(ctx, owner, repo, doc.RunID)
			if rerr != nil {
				return false, "", rerr
			}
			if holder != nil && !terminal(holder.State) {
				return false, doc.RunID, nil // taken by a live run
			}
			// The holder folded without releasing (a crashed cancel, a swept
			// run): steal it.
		}
		if doc.RunID == runID {
			return true, runID, nil
		}
		doc.RunID = runID
		doc.Since = s.nowUTC().Format(dateTimeFmt)
		doc.Version++
		body, _ := json.Marshal(doc) // cannot fail: the record is pure JSON types
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		if ver == "" {
			opts.Mode = store.PutCreate
		}
		if _, perr := store.PutBytes(ctx, s.Store, key, body, opts); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue // another admission won; re-read
			}
			return false, "", perr
		}
		return true, runID, nil
	}
	return false, "", nil
}

// releaseConcurrency frees every group the run held (called when its state
// folds terminal). A lost CAS is harmless: the next user of the group steals a
// holder whose run is terminal.
func (s *Service) releaseConcurrency(ctx context.Context, owner, repo, runID string, groups []string) {
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		key := ConcurrencyKey(owner, repo, group)
		_, _ = s.casUpdate(ctx, key, 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
			if cur == nil {
				return nil, false, nil
			}
			var doc ConcurrencyDoc
			if jerr := json.Unmarshal(cur, &doc); jerr != nil {
				return nil, false, nil // corrupt: leave it to a steal
			}
			if doc.RunID != runID {
				return nil, false, nil
			}
			doc.RunID = ""
			doc.Since = s.nowUTC().Format(dateTimeFmt)
			doc.Version++
			return mustJSON(&doc), true, nil
		})
	}
}

// runGroups returns every distinct concurrency group a run's jobs declare.
func runGroups(jobs []*Job) []string {
	seen := map[string]bool{}
	for _, j := range jobs {
		// A job the `if:`/`needs:` decision terminalised holds nothing: its
		// group must not block another run while this one is still live.
		if j.State != JobQueued && j.State != JobRunning {
			continue
		}
		if j.Spec == nil || j.Spec.Concurrency == nil {
			continue
		}
		if g := strings.TrimSpace(j.Spec.Concurrency.Group); g != "" {
			seen[g] = true
		}
	}
	return sortedKeys(seen)
}

// sortedKeys renders a set as a sorted slice (nil for an empty set).
func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
