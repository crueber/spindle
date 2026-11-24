// retention.go — tag retention (docs/features/14 §5). A repository may declare
// keep-last-N and/or maximum-age rules per tag pattern; the maintainer's
// `packages-gc` pass applies them before its mark/sweep phase, so the bytes a
// pruned tag owned are reclaimed by the same pass that decided to drop it.
//
// Two inviolable rules:
//
//   - A blob a stored manifest references is NEVER removed here. Retention
//     deletes tag pointers and the manifest versions whose last tag it pruned;
//     the mark phase then keeps every blob any surviving manifest names (a
//     layer shared with a kept version survives because the kept manifest
//     still names it).
//   - An untagged manifest is referenced until this pass proves otherwise: a
//     manifest that had no tag before the pass is never a deletion candidate.
//     Only a version whose last tag this pass removed can go, and only when no
//     surviving manifest names it as an index child.
//
// All listing happens here and in the sweep — the maintainer's LISTs, never a
// request path (law 4).
package packages

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/store"
)

// retentionReportCap bounds the pruned tag names a narration repeats: the
// count is always the truth, the names are a sample.
const retentionReportCap = 20

// RetentionResult is one retention pass (the GCResult sub-report).
type RetentionResult struct {
	Tags        int      `json:"tags"`         // tag pointers scanned
	PrunedTags  int      `json:"pruned_tags"`  // tag pointers the policy removed
	PrunedNames []string `json:"pruned_names"` // a sample of them (cap 20)
	Manifests   int      `json:"manifests"`    // manifest versions deleted because their last tag went
	Bytes       int64    `json:"bytes"`        // their manifest bytes
}

// tagPointer is one tag pointer read (the retention input).
type tagPointer struct {
	name      string
	hexDigest string
	updated   time.Time
	version   store.Version
}

// planRetention evaluates the rules against the pointers and returns the names
// the policy prunes (first matching rule wins; a tag no rule matches is kept).
func planRetention(rules []config.PackageRetention, tags []tagPointer, now time.Time) map[string]bool {
	pruned := map[string]bool{}
	// Bucket by the first matching rule.
	buckets := make(map[int][]tagPointer, len(rules))
	for _, t := range tags {
		for i, r := range rules {
			if r.Match(t.name) {
				buckets[i] = append(buckets[i], t)
				break
			}
		}
	}
	for i, group := range buckets {
		r := rules[i]
		sort.Slice(group, func(a, b int) bool {
			if !group[a].updated.Equal(group[b].updated) {
				return group[a].updated.After(group[b].updated)
			}
			return group[a].name > group[b].name
		})
		maxAge := time.Duration(r.MaxAge)
		for idx, t := range group {
			if r.KeepLast > 0 && idx >= r.KeepLast {
				pruned[t.name] = true
				continue
			}
			if maxAge > 0 && now.Sub(t.updated) >= maxAge {
				pruned[t.name] = true
			}
		}
	}
	return pruned
}

// retentionReport is the flattened pointer list, newest first, for the page
// (not used here yet) — kept unexported.
func readTagPointers(ctx context.Context, st store.ObjectStore, owner, repo string) ([]tagPointer, error) {
	prefix := tagsPrefix(owner, repo)
	var keys []string
	if err := st.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		keys = append(keys, m.Key)
		return nil
	}); err != nil {
		return nil, err
	}
	out := make([]tagPointer, 0, len(keys))
	for _, key := range keys {
		raw, meta, err := store.GetBytes(ctx, st, key, store.GetOptions{})
		if err != nil {
			if store.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		var doc tagDoc
		if json.Unmarshal(raw, &doc) != nil || !IsDigest(doc.Digest) {
			// A malformed pointer can never resolve; the tag delete path
			// already treats it as deletable, but retention never prunes what
			// it cannot date.
			continue
		}
		at, perr := time.Parse(time.RFC3339, doc.UpdatedAt)
		if perr != nil {
			continue
		}
		out = append(out, tagPointer{
			name:      strings.TrimPrefix(key, prefix),
			hexDigest: DigestHex(doc.Digest),
			updated:   at.UTC(),
			version:   meta.Version,
		})
	}
	return out, nil
}

// applyRetention runs one retention pass. apply=false plans (counts and names,
// deletes nothing).
func (s *Service) applyRetention(ctx context.Context, owner, repo string, rules []config.PackageRetention, apply bool) (RetentionResult, error) {
	var res RetentionResult
	if len(rules) == 0 {
		return res, nil
	}
	now := s.now()
	tags, err := readTagPointers(ctx, s.Store, owner, repo)
	if err != nil {
		return res, mapStoreErr(err)
	}
	res.Tags = len(tags)
	pruned := planRetention(rules, tags, now)
	if len(pruned) == 0 {
		return res, nil
	}
	// Delete the pointers (apply) and collect the digests whose tags went.
	removed := map[string]bool{}
	for _, t := range tags {
		if !pruned[t.name] {
			continue
		}
		res.PrunedTags++
		removed[t.hexDigest] = true
		if len(res.PrunedNames) < retentionReportCap {
			res.PrunedNames = append(res.PrunedNames, t.name)
		}
		if !apply {
			continue
		}
		if derr := s.Store.Delete(ctx, tagKey(owner, repo, t.name), t.version); derr != nil {
			if store.IsNotFound(derr) || store.IsPreconditionFailed(derr) {
				continue // the pointer moved or vanished under us: keep it
			}
			return res, mapStoreErr(derr)
		}
	}
	// Surviving references: in apply mode re-read the pointers (a concurrent
	// push may have re-tagged a digest we are about to consider); in plan mode
	// the snapshot minus the plan is the answer.
	survivors := map[string]bool{}
	if apply {
		remaining, rerr := readTagPointers(ctx, s.Store, owner, repo)
		if rerr != nil {
			return res, mapStoreErr(rerr)
		}
		for _, t := range remaining {
			survivors[t.hexDigest] = true
		}
	} else {
		for _, t := range tags {
			if !pruned[t.name] {
				survivors[t.hexDigest] = true
			}
		}
	}
	candidates := map[string]bool{}
	for hex := range removed {
		if !survivors[hex] {
			candidates[hex] = true
		}
	}
	if len(candidates) == 0 {
		return res, nil
	}
	// Read the candidates' own child lists BEFORE anything is deleted, then
	// the whole manifest set once to learn which candidates survive through
	// another manifest (a shared child, or a manifest another tag still names).
	childrenOf := s.childrenOfCandidates(ctx, owner, repo, candidates)
	protected, sizes, err := s.indexReferences(ctx, owner, repo, candidates)
	if err != nil {
		return res, mapStoreErr(err)
	}
	// A surviving tag is a reference in its own right: a tag may point at a
	// platform manifest directly, and that digest must not follow a pruned
	// index that also named it.
	for hex := range survivors {
		protected[hex] = true
	}
	for root := range candidates {
		if protected[root] {
			continue // a surviving manifest still names it (an index child)
		}
		if size, ok := sizes[root]; ok {
			res.Manifests++
			res.Bytes += size
		} else if meta, herr := s.Store.Head(ctx, manifestKey(owner, repo, root)); herr == nil && meta != nil {
			res.Manifests++
			res.Bytes += meta.Size
		}
		if apply {
			if derr := s.Store.Delete(ctx, manifestKey(owner, repo, root), ""); derr != nil && !store.IsNotFound(derr) {
				return res, mapStoreErr(derr)
			}
		}
		// The platform manifests this index owned follow it, unless another
		// surviving manifest (or another candidate, counted on its own) names
		// them.
		for _, kid := range childrenOf[root] {
			if protected[kid] || candidates[kid] {
				continue
			}
			if meta, herr := s.Store.Head(ctx, manifestKey(owner, repo, kid)); herr == nil && meta != nil {
				res.Manifests++
				res.Bytes += meta.Size
			}
			if apply {
				if derr := s.Store.Delete(ctx, manifestKey(owner, repo, kid), ""); derr != nil && !store.IsNotFound(derr) {
					return res, mapStoreErr(derr)
				}
			}
		}
	}
	return res, nil
}

// indexReferences reads every stored manifest once and returns which of the
// candidates another manifest references as an index child (those must stay)
// plus the candidate sizes. One LIST + one GET per manifest: the maintainer
// pass, bounded by the repo's manifest count.
func (s *Service) indexReferences(ctx context.Context, owner, repo string, candidates map[string]bool) (map[string]bool, map[string]int64, error) {
	protected := map[string]bool{}
	sizes := map[string]int64{}
	metas, err := s.listMeta(ctx, manifestsPrefix(owner, repo))
	if err != nil {
		return nil, nil, err
	}
	for _, m := range metas {
		hex := strings.TrimPrefix(m.Key, manifestsPrefix(owner, repo))
		if candidates[hex] {
			sizes[hex] = m.Size
			continue
		}
		raw, _, gerr := store.GetBytes(ctx, s.Store, m.Key, store.GetOptions{})
		if gerr != nil {
			if store.IsNotFound(gerr) {
				continue
			}
			return nil, nil, gerr
		}
		for _, child := range indexChildren(raw) {
			protected[child] = true
		}
	}
	return protected, sizes, nil
}

// childrenOfCandidates returns each candidate index manifest's direct child
// digests, read before the deletion phase so a pruned index takes its platform
// manifests with it.
func (s *Service) childrenOfCandidates(ctx context.Context, owner, repo string, candidates map[string]bool) map[string][]string {
	out := map[string][]string{}
	for hex := range candidates {
		raw, _, err := store.GetBytes(ctx, s.Store, manifestKey(owner, repo, hex), store.GetOptions{})
		if err != nil {
			continue
		}
		if kids := indexChildren(raw); len(kids) > 0 {
			out[hex] = kids
		}
	}
	return out
}
