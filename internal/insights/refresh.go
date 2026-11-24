// refresh.go — the projection's read/write paths. Load is the page's ONE
// exact-key GET; Refresh is the task body (one bounded git scan + bounded
// reads of the existing families) and the only writer.
package insights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/store"
)

// Scanner is the one git capability insights needs: a bounded history scan
// with per-commit line totals. *git.Layer implements it; tests substitute a
// pure fake.
type Scanner interface {
	InsightsCommits(ctx context.Context, repo *git.LocalRepo, rev string, since time.Time, cap int) ([]git.InsightsCommit, bool, error)
}

// Deps is the narrow composition seam: the store, the git scanner, and the two
// serving-copy resolvers. Head is cheap (refs level); Serve materializes the
// packs the scan needs. Both are injected so this package never imports the
// WAL engine (law 8).
type Deps struct {
	Store store.ObjectStore
	Git   Scanner

	// Head returns the default-branch tip ("" for an unborn repository).
	Head func(ctx context.Context, owner, repo string) (string, error)
	// Serve materializes the serving copy and returns it plus the head it
	// serves. It is only called when Head reported a non-empty tip.
	Serve func(ctx context.Context, owner, repo string) (*git.LocalRepo, string, error)

	Now          func() time.Time
	MaxCommits   int
	TTL          time.Duration
	MergeScanCap int
	FactCap      int
}

// Service computes and reads projections.
type Service struct{ d Deps }

// New builds the service. Zero optional fields resolve to their defaults.
func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.MaxCommits <= 0 {
		d.MaxCommits = DefaultMaxCommits
	}
	if d.TTL <= 0 {
		d.TTL = DefaultTTL
	}
	if d.MergeScanCap <= 0 {
		d.MergeScanCap = DefaultMergeScanCap
	}
	if d.FactCap <= 0 {
		d.FactCap = DefaultFactCap
	}
	return &Service{d: d}
}

// Load reads the projection. (nil, false, nil) means never computed (or a
// version this build refuses, which is the same "not available" answer).
func (s *Service) Load(ctx context.Context, owner, repo string) (*Projection, bool, error) {
	if s == nil || s.d.Store == nil {
		return nil, false, nil
	}
	body, _, err := store.GetBytes(ctx, s.d.Store, Key(owner, repo), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if body == nil {
		return nil, false, nil
	}
	var p Projection
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, false, fmt.Errorf("insights: decode projection: %w", err)
	}
	if p.Version != Version {
		return nil, false, nil
	}
	return &p, true, nil
}

// Stale reports whether the projection needs a recompute for head.
func (s *Service) Stale(p *Projection, head string) bool {
	return Stale(p, head, s.d.Now(), s.d.TTL)
}

// Refresh runs one refresh regardless of staleness. refreshed=false means the
// repository was empty (an empty projection is still written: "verified no
// commits" is a fact, not an error).
func (s *Service) Refresh(ctx context.Context, owner, repo string, log Logger) error {
	_, err := s.refresh(ctx, owner, repo, log, true)
	return err
}

// RefreshIfStale refreshes only when the head moved or the TTL expired. It is
// the maintainer sweep's body: an idle repository costs one refs-level head
// read plus one exact-key GET (law 4 — no LIST, no git).
func (s *Service) RefreshIfStale(ctx context.Context, owner, repo string, log Logger) (bool, error) {
	return s.refresh(ctx, owner, repo, log, false)
}

func (s *Service) refresh(ctx context.Context, owner, repo string, log Logger, force bool) (bool, error) {
	if s == nil || s.d.Store == nil || s.d.Git == nil || s.d.Head == nil || s.d.Serve == nil {
		return false, errors.New("insights: service is not wired")
	}
	if log == nil {
		log = nopLogger{}
	}
	now := s.d.Now().UTC()
	log.Notice("insights: reading " + owner + "/" + repo + " head")
	head, err := s.d.Head(ctx, owner, repo)
	if err != nil {
		return false, fmt.Errorf("insights: head: %w", err)
	}
	if !force {
		cur, ok, lerr := s.Load(ctx, owner, repo)
		if lerr != nil {
			return false, lerr
		}
		if ok && !Stale(cur, head, now, s.d.TTL) {
			return false, nil
		}
	}

	var commits []git.InsightsCommit
	truncated := false
	if head != "" {
		local, servedHead, serr := s.d.Serve(ctx, owner, repo)
		if serr != nil {
			return false, fmt.Errorf("insights: serve: %w", serr)
		}
		// Stamp the head the scan actually covers: if another push landed
		// while materializing, the next refresh sees the mismatch.
		if servedHead != "" {
			head = servedHead
		}
		if local == nil || local.Path == "" {
			return false, errors.New("insights: serving copy unavailable")
		}
		log.Notice("insights: scanning commits since " + scanFloor(now).Format("2006-01-02"))
		commits, truncated, err = s.d.Git.InsightsCommits(ctx, local, head, scanFloor(now), s.d.MaxCommits)
		if err != nil {
			return false, err
		}
	}

	ct := s.readCollab(ctx, owner, repo, now)
	facts := s.readFacts(ctx, owner, repo)

	p := &Projection{
		Version:    Version,
		ComputedAt: rfc3339(now),
		HeadSHA:    head,
		Windows:    buildWindows(commits, now, ct),
		Scan: ScanInfo{
			Commits:     len(commits),
			MaxCommits:  s.d.MaxCommits,
			Truncated:   truncated,
			FutureDated: countFuture(commits, now),
			Since:       rfc3339(scanFloor(now)),
		},
		Collab: CollabInfo{
			Known:           ct.known,
			IndexBounded:    ct.indexBounded,
			MergeScanCapped: ct.mergeScanCapped,
			PRSidecarReads:  ct.sidecarReads,
		},
		Facts: facts,
	}
	body, merr := json.Marshal(p)
	if merr != nil {
		return false, fmt.Errorf("insights: encode projection: %w", merr)
	}
	log.Notice(fmt.Sprintf("insights: writing %d commits / %d windows", len(commits), len(p.Windows)))
	if perr := s.put(ctx, Key(owner, repo), body); perr != nil {
		return false, perr
	}
	return true, nil
}

// put writes the projection with a bounded CAS loop: create-if-absent, then
// update-if-version. A 412 means a concurrent refresh won; re-read and retry
// (the projection is derived, so either answer is valid, but a torn read is
// never possible).
func (s *Service) put(ctx context.Context, key string, body []byte) error {
	for attempt := 0; attempt < 4; attempt++ {
		meta, err := s.d.Store.Head(ctx, key)
		if err != nil {
			return fmt.Errorf("insights: projection probe: %w", err)
		}
		opts := store.PutOptions{ContentType: "application/json"}
		if meta == nil {
			opts.Mode = store.PutCreate
		} else {
			opts.Mode = store.PutUpdate
			opts.IfVersion = meta.Version
		}
		_, perr := s.d.Store.Put(ctx, key, store.PutBody{Bytes: body}, opts)
		if perr == nil {
			return nil
		}
		if !store.IsPreconditionFailed(perr) {
			return fmt.Errorf("insights: projection write: %w", perr)
		}
	}
	return errors.New("insights: projection write kept losing its CAS")
}

// --- collaboration counts (existing index/card families) --------------------

// sharedCard is the P4 list projection as insights reads it (02 owns the
// shape; the fields used here are frozen).
type sharedCard struct {
	Num       int    `json:"num"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// sharedIndex is repos/<o>/<r>/issues/index.json (one exact-key GET).
type sharedIndex struct {
	CompactedThrough string       `json:"compacted_through"`
	Open             []sharedCard `json:"open"`
	ClosedRecent     []sharedCard `json:"closed_recent"`
}

// prMergeDoc is the pulls sidecar's merge half.
type prMergeDoc struct {
	Merged   bool    `json:"merged"`
	MergedAt *string `json:"merged_at"`
}

// readCollab counts opened/closed threads from the shared index and classifies
// closed PRs (merged vs closed-unmerged) from at most MergeScanCap pr.json
// sidecars. Failure to read the index is "unknown", never zero.
func (s *Service) readCollab(ctx context.Context, owner, repo string, now time.Time) collabTimes {
	ct := collabTimes{known: true}
	key := "repos/" + owner + "/" + repo + "/issues/index.json"
	body, _, err := store.GetBytes(ctx, s.d.Store, key, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return ct // verified: no issue or PR thread ever existed
		}
		ct.known = false
		return ct
	}
	if body == nil {
		return ct // verified: no issue or PR thread ever existed
	}
	var ix sharedIndex
	if json.Unmarshal(body, &ix) != nil {
		ct.known = false
		return ct
	}
	ct.indexBounded = ix.CompactedThrough != ""

	type mergedCandidate struct {
		num int
		at  time.Time
	}
	var candidates []mergedCandidate
	seen := map[int]bool{}
	consider := func(c sharedCard) {
		if c.Num <= 0 || seen[c.Num] {
			return
		}
		seen[c.Num] = true
		switch c.Kind {
		case "pr":
			if t, ok := parseStamp(c.CreatedAt); ok {
				ct.prsOpened = append(ct.prsOpened, t)
			}
			if c.State == "closed" {
				if t, ok := parseStamp(c.UpdatedAt); ok {
					// Only a closed PR in the widest window can be merged in
					// one; older cards need no sidecar read.
					if !t.Before(windowStart(now, Windows[len(Windows)-1])) {
						candidates = append(candidates, mergedCandidate{num: c.Num, at: t})
					}
				}
			}
		case "issue":
			if t, ok := parseStamp(c.CreatedAt); ok {
				ct.issuesOpened = append(ct.issuesOpened, t)
			}
			if c.State == "closed" {
				// Closed threads refuse conversation writes (02 §6.1), so the
				// card's last-activity stamp IS the close stamp; a reopen +
				// re-close moves it to the later close, which is the honest
				// "closed in this window" answer.
				if t, ok := parseStamp(c.UpdatedAt); ok {
					ct.issuesClosed = append(ct.issuesClosed, t)
				}
			}
		}
	}
	for _, c := range ix.Open {
		consider(c)
	}
	for _, c := range ix.ClosedRecent {
		consider(c)
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].at.After(candidates[j].at) })
	if len(candidates) > s.d.MergeScanCap {
		candidates = candidates[:s.d.MergeScanCap]
		ct.mergeScanCapped = true
	}
	for _, cand := range candidates {
		if err := ctx.Err(); err != nil {
			ct.known = false
			return ct
		}
		ct.sidecarReads++
		raw, _, gerr := store.GetBytes(ctx, s.d.Store, fmt.Sprintf("repos/%s/%s/pulls/%06x/pr.json", owner, repo, cand.num), store.GetOptions{})
		if gerr != nil || raw == nil {
			continue // a missing sidecar cannot prove a merge
		}
		var doc prMergeDoc
		if json.Unmarshal(raw, &doc) != nil || !doc.Merged {
			continue
		}
		if doc.MergedAt != nil {
			if t, ok := parseStamp(*doc.MergedAt); ok {
				ct.prsMerged = append(ct.prsMerged, t)
				continue
			}
		}
		ct.prsMerged = append(ct.prsMerged, cand.at)
	}
	return ct
}

// --- facts (the families that actually exist) --------------------------------

// errStopList bounds a fact LIST once the cap is known to be exceeded.
var errStopList = errors.New("insights: fact cap reached")

// readFacts reads the bucket's O(existing) facts, each bounded: social.json is
// one GET; releases, package tags and Threads runs are capped prefix LISTs
// (task path only — never a request).
func (s *Service) readFacts(ctx context.Context, owner, repo string) Facts {
	var f Facts
	// social.json: stars/watchers/forks counters (07 §4).
	if raw, _, err := store.GetBytes(ctx, s.d.Store, "repos/"+owner+"/"+repo+"/meta/social.json", store.GetOptions{}); err == nil || store.IsNotFound(err) {
		f.Social = true
		if raw != nil {
			var doc struct {
				Stars    int `json:"stars"`
				Watchers int `json:"watchers"`
				Forks    int `json:"forks"`
			}
			if json.Unmarshal(raw, &doc) == nil {
				f.Stars, f.Watchers, f.Forks = doc.Stars, doc.Watchers, doc.Forks
			}
		}
	}

	f.Releases, f.ReleasesKnown, f.ReleasesCapped = s.countPrefix(ctx,
		"repos/"+owner+"/"+repo+"/releases/", func(rest string) bool {
			// Header keys are `releases/<tag>.json`; latest.json is a pointer,
			// assets/ and counts/ are subdirectories.
			return rest != "latest.json" && strings.HasSuffix(rest, ".json") && !strings.Contains(rest, "/")
		})
	f.Packages, f.PackagesKnown, f.PackagesCapped = s.countPrefix(ctx,
		"repos/"+owner+"/"+repo+"/packages/registry/tags/", func(rest string) bool {
			return rest != "" && !strings.Contains(rest, "/")
		})
	f.ThreadsRuns, f.ThreadsRunsKnown, f.ThreadsRunsCapped = s.countPrefix(ctx,
		"repos/"+owner+"/"+repo+"/threads/runs/", func(rest string) bool {
			return strings.HasSuffix(rest, ".json") && !strings.Contains(rest, "/")
		})
	return f
}

// countPrefix counts matching leaf keys under prefix, stopping after cap+1 so
// "more than the cap" is observable without a second LIST. A LIST failure is
// unknown (known=false), never a fabricated zero.
func (s *Service) countPrefix(ctx context.Context, prefix string, match func(rest string) bool) (n int, known, capped bool) {
	capN := s.d.FactCap
	count := 0
	err := s.d.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		rest := strings.TrimPrefix(m.Key, prefix)
		if !match(rest) {
			return nil
		}
		count++
		if count > capN {
			return errStopList
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopList) {
		return 0, false, false
	}
	if count > capN {
		return capN, true, true
	}
	return count, true, false
}

// nopLogger is the silent default.
type nopLogger struct{}

func (nopLogger) Notice(string)                           {}
func (nopLogger) Progress(string, uint64, uint64, string) {}
