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

// deps.go owns issue dependencies (§14): parsed "blocks"/"blocked_by" links
// between issues of ONE repository.
//
// Storage decision: one repo-scoped CAS'd document (meta/issue_deps.json)
// holding every directed edge blocker → blocked. One add/remove is ONE atomic
// CAS, so the two sides of a link can never disagree with each other and no
// writer can lose the other's link; cycle detection reads the same object the
// write CASes. The alternative (a field on each thread header) needs two
// CASes per change on two different objects, which is exactly the lost-update
// shape 13_concurrency.md §5 tells us not to build.

// Dependency kinds on the wire (§14).
const (
	DepBlocks    = "blocks"
	DepBlockedBy = "blocked_by"
)

// MaxDependenciesPerIssue bounds one issue's link count on either side so a
// page render stays a bounded number of header probes.
const MaxDependenciesPerIssue = 25

// DependencyLink is one directed edge: Blocker blocks Blocked.
type DependencyLink struct {
	Blocker int `json:"blocker"`
	Blocked int `json:"blocked"`
}

// DependencyDoc is repos/<o>/<r>/meta/issue_deps.json (CAS'd). Links are
// sorted by (blocker, blocked) so the stored bytes are canonical.
type DependencyDoc struct {
	Version int              `json:"version"`
	Links   []DependencyLink `json:"links"`
}

// DependencyRef is one linked issue as the view renders it.
type DependencyRef struct {
	Num   int    `json:"num"`
	Title string `json:"title"`
	State string `json:"state"`
}

// DependencyView is GET …/api/issues/{num}/dependencies and the page's card:
// both directions, plus the derived unblocked signal.
//
//	Blocked   — at least one blocker is still open.
//	Unblocked — no open blockers (true with no blockers at all). Derived on
//	            read from the blocker headers, never stored (§14).
type DependencyView struct {
	Num       int             `json:"num"`
	Blocks    []DependencyRef `json:"blocks"`
	BlockedBy []DependencyRef `json:"blocked_by"`
	Blocked   bool            `json:"blocked"`
	Unblocked bool            `json:"unblocked"`
}

// DependencyKey is the repo link document key.
func DependencyKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/meta/issue_deps.json"
}

// loadDeps reads the link document; an absent object is the empty document.
func (s *Service) loadDeps(ctx context.Context, owner, repo string) (*DependencyDoc, error) {
	raw, _, err := s.getJSON(ctx, DependencyKey(owner, repo))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return &DependencyDoc{Links: []DependencyLink{}}, nil
	}
	var doc DependencyDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		return nil, fmt.Errorf("%w: issue_deps.json: %v", ErrCorrupt, jerr)
	}
	if doc.Links == nil {
		doc.Links = []DependencyLink{}
	}
	return &doc, nil
}

// depEdgeOf normalizes a wire (kind, num, target) pair onto the stored edge:
// "blocks" means num blocks target; "blocked_by" means target blocks num.
func depEdgeOf(kind string, num, target int) (DependencyLink, error) {
	switch kind {
	case DepBlocks:
		return DependencyLink{Blocker: num, Blocked: target}, nil
	case DepBlockedBy:
		return DependencyLink{Blocker: target, Blocked: num}, nil
	}
	return DependencyLink{}, fmt.Errorf("%w: kind must be blocks|blocked_by", ErrInvalid)
}

// depIndex folds the document into adjacency.
func depIndex(doc *DependencyDoc) map[int][]int {
	out := make(map[int][]int, len(doc.Links))
	for _, l := range doc.Links {
		out[l.Blocker] = append(out[l.Blocker], l.Blocked)
	}
	return out
}

// depReachable reports whether `to` is reachable from `from` by following
// blocks edges — i.e. adding from→to would close a cycle.
func depReachable(doc *DependencyDoc, from, to int) bool {
	adj := depIndex(doc)
	seen := map[int]bool{from: true}
	queue := []int{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range adj[cur] {
			if next == to {
				return true
			}
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

// hasLink reports membership.
func hasLink(doc *DependencyDoc, want DependencyLink) bool {
	for _, l := range doc.Links {
		if l == want {
			return true
		}
	}
	return false
}

// AddDependency adds one link (§14). Floor: triage. Self-links are refused
// (400), an unknown/PR target is 404, a link that would close a cycle is 409
// with the reason (cycles are refused, documented — the signal "all blockers
// closed" has no meaning on a cycle), and the per-issue cap is 409. A
// duplicate add is an idempotent no-op (changed=false).
func (s *Service) AddDependency(ctx context.Context, owner, repo string, num int, actor auth.Principal, kind string, target int) (bool, error) {
	if err := requireAuthenticated(actor); err != nil {
		return false, err
	}
	if err := s.requireRole(ctx, owner, repo, actor, string(identity.RoleTriage)); err != nil {
		return false, err
	}
	if num < 1 {
		return false, fmt.Errorf("%w: unknown issue", ErrNotFound)
	}
	edge, err := depEdgeOf(kind, num, target)
	if err != nil {
		return false, err
	}
	if edge.Blocker == edge.Blocked {
		return false, fmt.Errorf("%w: an issue cannot block itself", ErrInvalid)
	}
	for _, n := range []int{edge.Blocker, edge.Blocked} {
		th, _, lerr := s.loadThread(ctx, owner, repo, n)
		if lerr != nil {
			return false, lerr
		}
		if th == nil || th.Kind != "issue" {
			return false, fmt.Errorf("%w: unknown issue #%d", ErrNotFound, n)
		}
	}
	changed := false
	_, err = s.casUpdate(ctx, DependencyKey(owner, repo), 10, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		doc := &DependencyDoc{Links: []DependencyLink{}}
		if cur != nil {
			if jerr := json.Unmarshal(cur, &doc); jerr != nil {
				return nil, false, fmt.Errorf("%w: issue_deps.json: %v", ErrCorrupt, jerr)
			}
			if doc.Links == nil {
				doc.Links = []DependencyLink{}
			}
		}
		if hasLink(doc, edge) {
			return nil, false, nil // idempotent
		}
		if depReachable(doc, edge.Blocked, edge.Blocker) {
			return nil, false, fmt.Errorf("%w: #%d already blocks #%d; linking them both ways would create a dependency cycle", ErrConflict, edge.Blocked, edge.Blocker)
		}
		side := map[int]int{}
		for _, l := range doc.Links {
			side[l.Blocker]++
			side[l.Blocked]++
		}
		if side[edge.Blocker] >= MaxDependenciesPerIssue || side[edge.Blocked] >= MaxDependenciesPerIssue {
			return nil, false, fmt.Errorf("%w: at most %d dependencies per issue", ErrConflict, MaxDependenciesPerIssue)
		}
		if len(doc.Links) >= MaxDependencies {
			return nil, false, fmt.Errorf("%w: at most %d dependency links per repository", ErrConflict, MaxDependencies)
		}
		doc.Links = append(doc.Links, edge)
		doc.Version++
		changed = true
		raw, _ := json.Marshal(doc)
		return raw, true, nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// RemoveDependency removes one link. Floor: triage. An absent link is an
// idempotent no-op (changed=false).
func (s *Service) RemoveDependency(ctx context.Context, owner, repo string, num int, actor auth.Principal, kind string, target int) (bool, error) {
	if err := requireAuthenticated(actor); err != nil {
		return false, err
	}
	if err := s.requireRole(ctx, owner, repo, actor, string(identity.RoleTriage)); err != nil {
		return false, err
	}
	edge, err := depEdgeOf(kind, num, target)
	if err != nil {
		return false, err
	}
	if edge.Blocker == edge.Blocked {
		return false, fmt.Errorf("%w: an issue cannot block itself", ErrInvalid)
	}
	changed := false
	_, err = s.casUpdate(ctx, DependencyKey(owner, repo), 10, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var doc DependencyDoc
		if jerr := json.Unmarshal(cur, &doc); jerr != nil {
			return nil, false, fmt.Errorf("%w: issue_deps.json: %v", ErrCorrupt, jerr)
		}
		kept := make([]DependencyLink, 0, len(doc.Links))
		for _, l := range doc.Links {
			if l == edge {
				continue
			}
			kept = append(kept, l)
		}
		if len(kept) == len(doc.Links) {
			return nil, false, nil // nothing to remove
		}
		doc.Links = kept
		doc.Version++
		changed = true
		out, _ := json.Marshal(&doc)
		return out, true, nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// depsFor reads the link document once and projects one issue's view. Blocker
// headers are probed per side (bounded by MaxDependenciesPerIssue), so no
// LIST and no repo scan ever runs on the page path.
func (s *Service) depsFor(ctx context.Context, owner, repo string, num int) (*DependencyView, *DependencyDoc, error) {
	doc, err := s.loadDeps(ctx, owner, repo)
	if err != nil {
		return nil, nil, err
	}
	view := &DependencyView{Num: num, Blocks: []DependencyRef{}, BlockedBy: []DependencyRef{}, Unblocked: true}
	refOf := func(n int) (DependencyRef, bool) {
		th, _, lerr := s.loadThread(ctx, owner, repo, n)
		if lerr != nil || th == nil || th.Kind != "issue" {
			return DependencyRef{}, false
		}
		return DependencyRef{Num: th.Num, Title: th.Title, State: th.State}, true
	}
	for _, l := range doc.Links {
		switch {
		case l.Blocker == num:
			if r, ok := refOf(l.Blocked); ok {
				view.Blocks = append(view.Blocks, r)
			}
		case l.Blocked == num:
			if r, ok := refOf(l.Blocker); ok {
				view.BlockedBy = append(view.BlockedBy, r)
				if r.State != StateClosed {
					view.Blocked = true
				}
			}
		}
	}
	sort.Slice(view.Blocks, func(i, j int) bool { return view.Blocks[i].Num > view.Blocks[j].Num })
	sort.Slice(view.BlockedBy, func(i, j int) bool { return view.BlockedBy[i].Num > view.BlockedBy[j].Num })
	view.Unblocked = !view.Blocked
	return view, doc, nil
}

// DependenciesFor is the read verb behind GET …/dependencies and the page
// card. Read-gated; an unknown issue is 404 (the endpoint is per-issue).
func (s *Service) DependenciesFor(ctx context.Context, owner, repo string, num int, p auth.Principal) (*DependencyView, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	th, _, err := s.loadThread(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	if th == nil || th.Kind != "issue" {
		return nil, fmt.Errorf("%w: unknown issue", ErrNotFound)
	}
	view, _, err := s.depsFor(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	return view, nil
}
