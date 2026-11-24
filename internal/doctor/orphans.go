// orphans.go — the highest-risk referential checks. This is deliberately NOT
// a general bucket graph walk: each check names one pointer whose target a
// real incident lost, and each is bounded by the same sample limit as the
// family scan. Everything here is read-only.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crueber/spindle/internal/store"
)

// readObject GETs one key for a check. (nil, false, nil) means absent or the
// budget is exhausted; a non-nil error is an I/O failure the caller returns.
func (s *scanner) readObject(ctx context.Context, key string) ([]byte, bool, error) {
	if s.decoded >= s.opts.Budget {
		s.budgetExhausted = true
		return nil, false, nil
	}
	raw, _, err := store.GetBytes(ctx, s.st, key, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("doctor: get %s: %w", key, err)
	}
	s.decoded++
	return raw, true, nil
}

// exists is a HEAD probe (not a decode; it does not consume the decode budget).
func (s *scanner) exists(ctx context.Context, key string) (bool, error) {
	m, err := s.st.Head(ctx, key)
	if err != nil {
		return false, fmt.Errorf("doctor: head %s: %w", key, err)
	}
	return m != nil, nil
}

// checkOrphans runs the four documented orphan checks.
func (s *scanner) checkOrphans(ctx context.Context) error {
	if err := s.checkArtifactOrphans(ctx); err != nil {
		return err
	}
	if err := s.checkQueueOrphans(ctx); err != nil {
		return err
	}
	if err := s.checkReportOrphans(ctx); err != nil {
		return err
	}
	return s.checkForkOrphans(ctx)
}

// checkArtifactOrphans: a Threads artifact manifest whose run record is gone.
// Artifacts live under the run prefix, so a deleted run usually takes them
// with it; a leftover means a failed delete or an out-of-band removal.
func (s *scanner) checkArtifactOrphans(ctx context.Context) error {
	checked := 0
	for _, repo := range s.repos {
		if checked >= s.opts.Limit || s.budgetExhausted {
			break
		}
		prefix := "repos/" + repo + "/threads/runs/"
		err := s.st.List(ctx, prefix, "", func(m store.ObjectMeta) error {
			if !strings.Contains(m.Key, "/artifacts/") || !strings.HasSuffix(m.Key, "/manifest.json") {
				return nil
			}
			if checked >= s.opts.Limit {
				return errStop
			}
			checked++
			artifactRepo, run := artifactRun(m.Key)
			if run == "" {
				return nil
			}
			runKey := "repos/" + artifactRepo + "/threads/runs/" + run + ".json"
			ok, err := s.exists(ctx, runKey)
			if err != nil {
				return err
			}
			if !ok {
				s.finding(nil, CheckArtifact, m.Key, "artifact's run %s has no run record at %s", run, runKey)
			}
			return nil
		})
		if err != nil && !errors.Is(err, errStop) {
			return fmt.Errorf("doctor: list artifacts under %s: %w", prefix, err)
		}
	}
	return nil
}

// artifactRun extracts (repo, run) from an artifact manifest key.
func artifactRun(key string) (repo, run string) {
	parts := strings.Split(key, "/")
	// repos / <owner> / <name> / threads / runs / <run> / artifacts / <name> / manifest.json
	if len(parts) < 8 || parts[0] != "repos" || parts[3] != "threads" || parts[4] != "runs" || parts[6] != "artifacts" {
		return "", ""
	}
	return parts[1] + "/" + parts[2], parts[5]
}

// checkQueueOrphans: global queue index rows naming a missing per-repo job.
func (s *scanner) checkQueueOrphans(ctx context.Context) error {
	raw, ok, err := s.readObject(ctx, "threads/queue/index.json")
	if err != nil || !ok {
		return err
	}
	var idx struct {
		Queued []struct {
			Repo  string `json:"repo"`
			RunID string `json:"run"`
			JobID string `json:"job"`
		} `json:"queued"`
	}
	if json.Unmarshal(raw, &idx) != nil {
		return nil // the family decode already reported it
	}
	checked := 0
	for _, row := range idx.Queued {
		if checked >= s.opts.Limit || s.budgetExhausted {
			break
		}
		checked++
		if row.Repo == "" || row.RunID == "" || row.JobID == "" {
			continue
		}
		jobKey := "repos/" + row.Repo + "/threads/runs/" + row.RunID + "/jobs/" + row.JobID + ".json"
		ok, err := s.exists(ctx, jobKey)
		if err != nil {
			return err
		}
		if !ok {
			s.finding(nil, CheckQueue, jobKey,
				"global queue index row names a missing job record (row %s %s/%s)", row.Repo, row.RunID, row.JobID)
		}
	}
	return nil
}

// checkReportOrphans: report index pointers (global and per-repo) whose
// immutable record is missing. The record is the durable truth; a card without
// one makes the moderation queue render a phantom row.
func (s *scanner) checkReportOrphans(ctx context.Context) error {
	checked := 0
	check := func(cards []reportCardRef, indexKey string) error {
		for _, c := range cards {
			if checked >= s.opts.Limit || s.budgetExhausted {
				return nil
			}
			checked++
			repo := c.Repo
			if repo == "" {
				continue
			}
			recKey := fmt.Sprintf("repos/%s/meta/moderation/reports/%012x.json", repo, c.ID)
			ok, err := s.exists(ctx, recKey)
			if err != nil {
				return err
			}
			if !ok {
				s.finding(nil, CheckReport, recKey,
					"report index %s points at id %d with no record", indexKey, c.ID)
			}
		}
		return nil
	}
	// Global queue.
	raw, ok, err := s.readObject(ctx, "moderation/reports/index.json")
	if err != nil {
		return err
	}
	if ok {
		var idx struct {
			Open     []reportCardRef `json:"open"`
			Resolved []reportCardRef `json:"resolved_recent"`
		}
		if json.Unmarshal(raw, &idx) == nil {
			cards := append(idx.Open, idx.Resolved...)
			if err := check(cards, "moderation/reports/index.json"); err != nil {
				return err
			}
		}
	}
	// Per-repo hot windows on the sampled repositories.
	for _, repo := range s.repos {
		if checked >= s.opts.Limit || s.budgetExhausted {
			break
		}
		key := "repos/" + repo + "/meta/moderation/reports/index.json"
		raw, ok, err := s.readObject(ctx, key)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		var idx struct {
			Open     []reportCardRef `json:"open"`
			Resolved []reportCardRef `json:"resolved_recent"`
		}
		if json.Unmarshal(raw, &idx) != nil {
			continue
		}
		cards := append(idx.Open, idx.Resolved...)
		for i := range cards {
			cards[i].Repo = repo
		}
		if err := check(cards, key); err != nil {
			return err
		}
	}
	return nil
}

// reportCardRef is the subset of a report card the pointer check needs.
type reportCardRef struct {
	ID   int    `json:"id"`
	Repo string `json:"repo"`
}

// checkForkOrphans: child/parent fork pointers that disagree. Both directions
// are checked for the sampled repositories only: a fork.json whose parent is
// missing or whose parent index omits it, and a parent index row whose child
// has no fork.json or names a different parent.
func (s *scanner) checkForkOrphans(ctx context.Context) error {
	checked := 0
	for _, repo := range s.repos {
		if checked >= s.opts.Limit || s.budgetExhausted {
			break
		}
		forkKey := "repos/" + repo + "/fork.json"
		raw, ok, err := s.readObject(ctx, forkKey)
		if err != nil {
			return err
		}
		if ok {
			checked++
			var doc struct {
				Parent string `json:"parent"`
			}
			if json.Unmarshal(raw, &doc) == nil {
				parent := strings.TrimSpace(doc.Parent)
				if parent == "" {
					s.finding(nil, CheckFork, forkKey, "fork.json has no parent")
				} else {
					ok, err := s.exists(ctx, "repos/"+parent+"/manifest.pb")
					if err != nil {
						return err
					}
					if !ok {
						s.finding(nil, CheckFork, forkKey, "fork names parent %s, which has no manifest", parent)
					}
					idxKey := "repos/" + parent + "/meta/forks.json"
					raw, ok, err := s.readObject(ctx, idxKey)
					if err != nil {
						return err
					}
					if ok {
						var idx struct {
							Forks []struct {
								Repo string `json:"repo"`
							} `json:"forks"`
						}
						if json.Unmarshal(raw, &idx) == nil && !forkIndexHas(idx.Forks, repo) {
							s.finding(nil, CheckFork, forkKey,
								"parent %s's fork index (%s) does not list this child", parent, idxKey)
						}
					}
				}
			}
		}
		// Parent direction: this repo's index rows.
		idxKey := "repos/" + repo + "/meta/forks.json"
		raw, ok, err = s.readObject(ctx, idxKey)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		var idx struct {
			Forks []struct {
				Repo string `json:"repo"`
			} `json:"forks"`
		}
		if json.Unmarshal(raw, &idx) != nil {
			continue
		}
		for _, row := range idx.Forks {
			if checked >= s.opts.Limit || s.budgetExhausted {
				break
			}
			child := strings.TrimSpace(row.Repo)
			if child == "" {
				continue
			}
			checked++
			childKey := "repos/" + child + "/fork.json"
			raw, ok, err := s.readObject(ctx, childKey)
			if err != nil {
				return err
			}
			if !ok {
				s.finding(nil, CheckFork, childKey,
					"fork index %s lists %s, but the child has no fork.json", idxKey, child)
				continue
			}
			var doc struct {
				Parent string `json:"parent"`
			}
			if json.Unmarshal(raw, &doc) == nil && strings.TrimSpace(doc.Parent) != repo {
				s.finding(nil, CheckFork, childKey,
					"fork index %s lists this child, but fork.json names parent %q", idxKey, doc.Parent)
			}
		}
	}
	return nil
}

func forkIndexHas(rows []struct {
	Repo string `json:"repo"`
}, child string) bool {
	for _, row := range rows {
		if row.Repo == child {
			return true
		}
	}
	return false
}

// checkManifests: a repository prefix that has WAL history (checkpoints/ or
// log/) but no manifest.pb is a repository that cannot open. Stale prefixes
// with only sidecars (meta/stats.json, a fork-provisioned unborn prefix) are
// deliberately NOT reported: that is expected state, not corruption.
func (s *scanner) checkManifests(ctx context.Context) error {
	for _, repo := range s.repos {
		key := "repos/" + repo + "/manifest.pb"
		ok, err := s.exists(ctx, key)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		hasRefs := false
		prefix := "repos/" + repo + "/checkpoints/"
		err = s.st.ListPrefixes(ctx, prefix, func(string) error {
			hasRefs = true
			return errStop
		})
		if err != nil && !errors.Is(err, errStop) {
			return fmt.Errorf("doctor: list prefixes under %s: %w", prefix, err)
		}
		if !hasRefs {
			logPrefix := "repos/" + repo + "/log/"
			err = s.st.List(ctx, logPrefix, "", func(store.ObjectMeta) error {
				hasRefs = true
				return errStop
			})
			if err != nil && !errors.Is(err, errStop) {
				return fmt.Errorf("doctor: list log under %s: %w", logPrefix, err)
			}
		}
		if hasRefs {
			s.finding(nil, CheckManifest, key,
				"repository prefix has WAL history (checkpoints/ or log/) but no manifest; it cannot open")
		}
	}
	return nil
}
