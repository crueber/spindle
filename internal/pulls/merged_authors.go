package pulls

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crueber/spindle/internal/store"
)

// Merged-author projection (docs/features/15_threads.md §5.2, round-2 pass 2).
//
// The first-time-contributor approval gate needs one cheap question: "has this
// account ever had a pull request merged in this repository?" Walking every
// pr.json on every fork PR would be a LIST on a hot path (law 4), so the answer
// is a small CAS'd projection written when a merge lands:
//
//	repos/<o>/<r>/meta/merged_authors.json   {"authors": [...], "version": n}
//
// One exact-key GET answers the gate. Authors are principal names, lowercased
// and de-duplicated, capped at maxMergedAuthors (beyond the cap a lookup reports
// "unknown" and the gate fails closed — a maintainer approves once).
//
// Migration: the projection did not exist before this pass, so a read that finds
// no object (or misses an author) falls back to ONE bounded scan of the shared PR
// index (open + closed_recent, the pool ListPRs already reads) and, on a hit,
// writes the projection for every merged author it saw. That self-heals an
// existing repository on the first fork PR after upgrade; a merged PR that has
// aged out of the index before the projection existed is the one case the scan
// cannot see, and it is documented as "the gate may fire once more".

// MergedAuthorsKey returns repos/<o>/<r>/meta/merged_authors.json.
func MergedAuthorsKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/meta/merged_authors.json"
}

// maxMergedAuthors caps the projection (a repository with more distinct merged
// authors than this reports "unknown" past the cap instead of growing forever).
const maxMergedAuthors = 4096

// MergedAuthorsDoc is the projection's record.
type MergedAuthorsDoc struct {
	Authors []string `json:"authors"`
	Version int      `json:"version"`
}

// has reports whether name is in the set (case-insensitive).
func (d *MergedAuthorsDoc) has(name string) bool {
	name = normalizeAuthor(name)
	for _, a := range d.Authors {
		if a == name {
			return true
		}
	}
	return false
}

// normalizeAuthor lowercases and trims a principal name.
func normalizeAuthor(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// noteMergedAuthor records one author in the projection (called after a merge
// lands; a failure is narrated by the caller, never a reason to fail the merge).
func (s *Service) noteMergedAuthor(ctx context.Context, owner, repo, author string) {
	author = normalizeAuthor(author)
	if author == "" {
		return
	}
	_, _ = s.casUpdate(ctx, MergedAuthorsKey(owner, repo), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		doc := &MergedAuthorsDoc{}
		if cur != nil {
			if jerr := json.Unmarshal(cur, doc); jerr != nil {
				return nil, false, fmt.Errorf("merged-authors: %v", jerr)
			}
		}
		if doc.has(author) {
			return nil, false, nil
		}
		if len(doc.Authors) >= maxMergedAuthors {
			return nil, false, nil // full: fail closed at the cap
		}
		doc.Authors = append(doc.Authors, author)
		doc.Version++
		body, merr := json.Marshal(doc)
		if merr != nil {
			return nil, false, merr
		}
		return body, true, nil
	})
}

// MergedAuthor reports whether author has a merged pull request in this
// repository. It is read path only (one exact-key GET, plus one bounded index
// scan on a miss that also self-heals the projection).
func (s *Service) MergedAuthor(ctx context.Context, owner, repo, author string) (bool, error) {
	author = normalizeAuthor(author)
	if author == "" {
		return false, nil
	}
	doc, _, err := s.loadMergedAuthors(ctx, owner, repo)
	if err != nil {
		return false, err
	}
	if doc.has(author) {
		return true, nil
	}
	// Miss (or no projection yet): one bounded scan over the index pool, and a
	// hit heals the projection so the next read is a single GET.
	if found := s.scanMergedAuthors(ctx, owner, repo, author); found {
		s.noteMergedAuthor(ctx, owner, repo, author)
		return true, nil
	}
	return false, nil
}

// loadMergedAuthors reads the projection; (empty, "", nil) when absent.
func (s *Service) loadMergedAuthors(ctx context.Context, owner, repo string) (*MergedAuthorsDoc, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, MergedAuthorsKey(owner, repo))
	if err != nil {
		return nil, "", err
	}
	doc := &MergedAuthorsDoc{}
	if raw == nil {
		return doc, "", nil
	}
	if jerr := json.Unmarshal(raw, doc); jerr != nil {
		return nil, "", fmt.Errorf("merged-authors: %v", jerr)
	}
	return doc, ver, nil
}

// scanMergedAuthors loads the PR index's pool and probes each PR by the author
// in question (bounded by the pool size, which the index already bounds).
func (s *Service) scanMergedAuthors(ctx context.Context, owner, repo, author string) bool {
	ix, _, err := s.loadIndex(ctx, owner, repo)
	if err != nil {
		return false
	}
	pool := append(append([]Card{}, ix.Open...), ix.ClosedRecent...)
	for _, c := range pool {
		if c.Kind != "pr" || normalizeAuthor(c.Author) != author || c.State != StateClosed {
			continue
		}
		pr, _, perr := s.loadPR(ctx, owner, repo, c.Num)
		if perr != nil || pr == nil {
			continue
		}
		if pr.Merged {
			return true
		}
	}
	return false
}
