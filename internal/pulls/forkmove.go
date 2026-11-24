package pulls

// forkmove.go — the fork pointer families follow a repository move
// (PLAN-V2 §T.1/T.4.3, docs/features/01 §5.4). The move itself is
// internal/identity's; this file is the seam it binds (identity must not
// import pulls — pulls imports identity for the P6 gates, law 8), so the fork
// JSON shapes stay in their owning package.
//
// Three families name a repo across prefixes:
//
//	parent meta/forks.json   child rows (a moved child leaves a stale row)
//	child  fork.json         parent/root pointers (a moved parent leaves both stale)
//
// Both directions are load-bearing: internal/wal/forkread.go resolves a
// fork's pack fallback from Parent+Root, and internal/maintain/forknet.go's
// GC walk follows the same pointers (a stale pointer breaks pack reads and GC
// liveness). They are also cheap: each rewrite is one exact-key GET + one CAS
// on an object the caller already names, never a LIST.
//
// Idempotency (a transfer task retries after a crash): every writer here
// rewrites only a row/pointer that still names the OLD id; a second pass
// finds the new value and writes nothing (or a missing row, which per the
// #457 ghost rule is a no-op — never a resurrect).

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crueber/spindle/internal/store"
)

// ParentOf returns the parent id ("owner/name") from a repo's fork.json.
// ok=false when the repo is not a fork (absent, unreadable, or corrupt doc —
// the fork chain's own fail-closed reading, wal.forkread's rule).
func (s *Service) ParentOf(ctx context.Context, owner, repo string) (string, bool, error) {
	raw, _, err := s.getJSON(ctx, ForkKey(owner, repo))
	if err != nil {
		return "", false, err
	}
	if raw == nil {
		return "", false, nil
	}
	var doc ForkDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil || doc.Parent == "" {
		return "", false, nil
	}
	return doc.Parent, true, nil
}

// ChildMoved rewrites a moved fork's row in its parent's meta/forks.json
// (oldChild → newChild). One CAS; an absent index or absent row writes
// nothing and reports false (the #457 ghost rule: a row for a repo that is
// not there is left for the delete sweep, never invented).
func (s *Service) ChildMoved(ctx context.Context, parentOwner, parentRepo, oldChild, newChild string) (bool, error) {
	renamed := false
	_, err := s.casUpdate(ctx, ForksKey(parentOwner, parentRepo), 10, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		renamed = false
		if cur == nil {
			return nil, false, nil
		}
		fx, perr := parseForks(cur)
		if perr != nil {
			return nil, false, perr
		}
		found := false
		for i := range fx.Forks {
			if fx.Forks[i].Repo == oldChild {
				fx.Forks[i].Repo = newChild
				found = true
			}
		}
		if !found {
			return nil, false, nil
		}
		fx.Version++
		out, merr := json.Marshal(fx)
		if merr != nil {
			return nil, false, merr
		}
		renamed = true
		return out, true, nil
	})
	if err != nil {
		return false, err
	}
	return renamed, nil
}

// ParentMoved rewrites the child-side fork.json pointers that name a moved
// parent: for every child listed in the MOVED parent's index (read at newID,
// where the prefix copy landed), Parent and Root are rewritten oldID →
// newID. Returns how many child docs changed. Bounded by the index's own row
// count (exact-key GETs, no LIST); a child whose provenance is already gone
// is skipped, a corrupt one fails the pass (fail closed — a stale pointer is
// worse than a loud failure).
func (s *Service) ParentMoved(ctx context.Context, oldID, newID string) (int, error) {
	owner, repo, ok := splitRepoID(newID)
	if !ok {
		return 0, fmt.Errorf("%w: bad repo id %q", ErrInvalid, newID)
	}
	raw, _, err := s.getJSON(ctx, ForksKey(owner, repo))
	if err != nil {
		return 0, err
	}
	if raw == nil {
		return 0, nil
	}
	fx, perr := parseForks(raw)
	if perr != nil {
		return 0, perr
	}
	changed := 0
	for _, child := range fx.Forks {
		co, cn, ok := splitRepoID(child.Repo)
		if !ok {
			continue
		}
		moved := false
		_, cerr := s.casUpdate(ctx, ForkKey(co, cn), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
			moved = false
			if cur == nil {
				return nil, false, nil
			}
			var doc ForkDoc
			if jerr := json.Unmarshal(cur, &doc); jerr != nil {
				return nil, false, fmt.Errorf("%w: %s/fork.json: %v", ErrCorrupt, child.Repo, jerr)
			}
			changedHere := false
			if doc.Parent == oldID {
				doc.Parent = newID
				changedHere = true
			}
			if doc.Root == oldID {
				// The moved repo was the network root: children that
				// short-circuit through Root must follow it too.
				doc.Root = newID
				changedHere = true
			}
			if !changedHere {
				return nil, false, nil
			}
			doc.Version++
			out, merr := json.Marshal(&doc)
			if merr != nil {
				return nil, false, merr
			}
			moved = true
			return out, true, nil
		})
		if cerr != nil {
			return changed, cerr
		}
		if moved {
			changed++
		}
	}
	return changed, nil
}

// VerifyMoved proves the fork pointer families are consistent after a move
// (the transfer task's verification step): the parent's index must not still
// name oldID, and every child listed in the moved repo's index must not still
// name oldID as parent or root. It reports the specific stale object, never a
// generic "verification failed".
func (s *Service) VerifyMoved(ctx context.Context, oldID, newID string) error {
	// The moved repo as a CHILD: its parent's index row must follow it.
	if raw, _, err := s.getJSON(ctx, ForkKey(mustSplit(newID))); err != nil {
		return err
	} else if raw != nil {
		var doc ForkDoc
		if jerr := json.Unmarshal(raw, &doc); jerr == nil && doc.Parent != "" {
			po, pr, ok := splitRepoID(doc.Parent)
			if ok {
				iraw, _, ierr := s.getJSON(ctx, ForksKey(po, pr))
				if ierr != nil {
					return ierr
				}
				if iraw != nil {
					fx, perr := parseForks(iraw)
					if perr != nil {
						return perr
					}
					for _, f := range fx.Forks {
						if f.Repo == oldID {
							return fmt.Errorf("%w: %s still lists fork %s (want %s)",
								ErrCorrupt, ForksKey(po, pr), oldID, newID)
						}
					}
				}
			}
		}
	}
	// The moved repo as a PARENT: its children's docs must follow it.
	no, nr, ok := splitRepoID(newID)
	if !ok {
		return fmt.Errorf("%w: bad repo id %q", ErrInvalid, newID)
	}
	raw, _, err := s.getJSON(ctx, ForksKey(no, nr))
	if err != nil || raw == nil {
		return err
	}
	fx, perr := parseForks(raw)
	if perr != nil {
		return perr
	}
	for _, child := range fx.Forks {
		co, cn, ok := splitRepoID(child.Repo)
		if !ok {
			continue
		}
		craw, _, cerr := s.getJSON(ctx, ForkKey(co, cn))
		if cerr != nil {
			return cerr
		}
		if craw == nil {
			continue // provenance swept with a deleted child: nothing stale
		}
		var doc ForkDoc
		if jerr := json.Unmarshal(craw, &doc); jerr != nil {
			continue // corrupt provenance is the forkread fail-closed case
		}
		if doc.Parent == oldID || doc.Root == oldID {
			return fmt.Errorf("%w: %s/fork.json still names %s (want %s)",
				ErrCorrupt, child.Repo, oldID, newID)
		}
	}
	return nil
}

// splitRepoID splits "owner/name" (strict: exactly one slash, both non-empty).
func splitRepoID(id string) (owner, repo string, ok bool) {
	for i := 0; i < len(id); i++ {
		if id[i] != '/' {
			continue
		}
		owner, repo = id[:i], id[i+1:]
		return owner, repo, owner != "" && repo != ""
	}
	return "", "", false
}

// mustSplit is splitRepoID for call sites that already validated the id.
func mustSplit(id string) (string, string) {
	o, r, _ := splitRepoID(id)
	return o, r
}
