package issues

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// parity.go owns the issue-parity curation verbs that live on the existing
// CAS'd objects (docs/features/02_issues.md §13/§14):
//
//   - pinning, stored as Index.Pinned in issues/index.json — no new family;
//   - dependencies (blocks/blocked_by), stored as one repo-scoped CAS'd
//     document meta/issue_deps.json — one object, so one add/remove is ONE
//     atomic CAS and two directions can never disagree.
//
// Both are repo curation, not conversation activity: the write floor is
// triage (the label/milestone floor), host admins pass, anonymous is a real
// 401. Neither appends a thread event — the issue timeline is the
// conversation record, and a pin is display state while a dependency is repo
// metadata (the labels.json precedent). Both are moderation-exempt by the
// repo-configuration rule (16_moderation §3.3).

// --- pinning -----------------------------------------------------------------

// pinSet folds numbers into a sorted unique set.
func pinSet(nums []int) []int {
	seen := make(map[int]bool, len(nums))
	out := make([]int, 0, len(nums))
	for _, n := range nums {
		if n < 1 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out
}

// isPinned reports membership in a pin set.
func isPinned(pins []int, num int) bool {
	for _, p := range pins {
		if p == num {
			return true
		}
	}
	return false
}

// sortPinnedFirst orders a filtered pool for display: pinned issues first
// (newest-number-first within the pinned block), then everything else
// newest-number-first. The pinned block never reorders the rest — this is
// §13's only ordering change over §2's number-descending render.
func sortPinnedFirst(cards []Card, pins []int) {
	if len(pins) == 0 {
		sortCardsByNum(cards)
		return
	}
	pin := make(map[int]bool, len(pins))
	for _, p := range pins {
		pin[p] = true
	}
	sort.Slice(cards, func(i, j int) bool {
		pi, pj := pin[cards[i].Num], pin[cards[j].Num]
		if pi != pj {
			return pi
		}
		return cards[i].Num > cards[j].Num
	})
}

// PinnedIssues returns the repo pin set (newest-number-first). One exact-key
// GET; an absent index is the empty set, never an error. No read gate: the
// caller already passed one and the set carries numbers only.
func (s *Service) PinnedIssues(ctx context.Context, owner, repo string) ([]int, error) {
	ix, _, err := s.loadIndex(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return pinSet(ix.Pinned), nil
}

// PinIssue pins or unpins one issue in the repo pin list (§13). The floor is
// triage; the issue must exist and be kind "issue" (404 otherwise). Both
// directions are idempotent: a repeated pin/unpin writes nothing and reports
// changed=false with the current set. The write is ONE bounded CAS on
// issues/index.json — the set rides the object the lists already read, so no
// extra read is added to any list path.
//
// ### Concurrency: the index CAS loop only (13 §3/§5) — no lock, and a lost
// update is the same performance gap the rest of the index tolerates (the
// next mutation or RepairIndex repairs the cards; the pin set itself is
// rewritten on the next successful pin/unpin).
func (s *Service) PinIssue(ctx context.Context, owner, repo string, num int, actor auth.Principal, pinned bool) ([]int, bool, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, false, err
	}
	if err := s.requireRole(ctx, owner, repo, actor, string(identity.RoleTriage)); err != nil {
		return nil, false, err
	}
	th, _, err := s.loadThread(ctx, owner, repo, num)
	if err != nil {
		return nil, false, err
	}
	if th == nil || th.Kind != "issue" {
		return nil, false, fmt.Errorf("%w: unknown issue", ErrNotFound)
	}
	var out []int
	changed := false
	_, err = s.casUpdate(ctx, IndexKey(owner, repo), 10, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		ix := &Index{Open: []Card{}, ClosedRecent: []Card{}}
		if cur != nil {
			parsed, perr := parseIndex(cur)
			if perr != nil {
				return nil, false, perr
			}
			ix = parsed
		}
		cur1 := pinSet(ix.Pinned)
		out = cur1
		if isPinned(cur1, num) == pinned {
			return nil, false, nil // already in the requested state
		}
		if pinned {
			if len(cur1) >= MaxPinned {
				return nil, false, fmt.Errorf("%w: at most %d pinned issues", ErrConflict, MaxPinned)
			}
			cur1 = pinSet(append(cur1, num))
		} else {
			kept := make([]int, 0, len(cur1))
			for _, p := range cur1 {
				if p != num {
					kept = append(kept, p)
				}
			}
			cur1 = pinSet(kept)
		}
		ix.Pinned = cur1
		ix.Version++
		out, changed = cur1, true
		raw, _ := json.Marshal(ix)
		return raw, true, nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, changed, nil
}
