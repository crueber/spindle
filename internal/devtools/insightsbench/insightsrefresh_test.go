//go:build evidence

// insightsrefresh_test.go — the E19 harness for docs/EVIDENCE.md: measure the
// repository Insights refresh on a 1k-commit and a 10k-commit fixture.
//
//	cd /var/tmp && TMPDIR=/var/tmp/sp-tmp go test -tags evidence \
//	  ./internal/devtools/insightsbench/ -run TestInsightsRefreshScale -v -timeout 30m
//
// The build tag keeps the fixture generation out of CI and `go vet`. The
// measured path is the real one: internal/git's pinned `git log` argv through
// *git.Layer, then insights.Service.Refresh over the real memory store.
package insightsbench

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/insights"
	"github.com/crueber/spindle/internal/store"
)

const scanCap = 20000

// buildFixture creates a bare repository with n commits spread over 365 days,
// each touching one of 20 files, and returns its path and head.
func buildFixture(t *testing.T, n int) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", "-q", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	now := time.Now().UTC()
	span := int64(364 * 24 * 3600)
	base := now.Unix() - span
	step := span / int64(n)
	var stream bytes.Buffer
	for i := 0; i < n; i++ {
		ts := base + int64(i)*step
		msg := fmt.Sprintf("commit %d\n", i)
		fmt.Fprintf(&stream, "commit refs/heads/main\n")
		fmt.Fprintf(&stream, "committer Insight Bench <bench@example.com> %d +0000\n", ts)
		fmt.Fprintf(&stream, "data %d\n%s", len(msg), msg)
		body := fmt.Sprintf("line %d\n", i)
		fmt.Fprintf(&stream, "M 100644 inline f%02d.txt\n", i%20)
		fmt.Fprintf(&stream, "data %d\n%s", len(body), body)
	}
	cmd := exec.Command("git", "fast-import", "--quiet")
	cmd.Dir = dir
	cmd.Stdin = &stream
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git fast-import: %v\n%s", err, out)
	}
	out, err := exec.Command("git", "--git-dir="+dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return dir, strings.TrimSpace(string(out))
}

// seedFacts gives the refresh a realistic amount of non-git work: 100 shared
// index cards (50 closed PRs with sidecars) plus social/release/package/run
// facts. This is deliberately the worst sidecar case at the default cap.
func seedFacts(t *testing.T, st *store.Memory, owner, repo, now string) {
	t.Helper()
	ctx := context.Background()
	put := func(key, body string) {
		if _, err := st.Put(ctx, key, store.PutBody{Bytes: []byte(body)}, store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	var open, closed strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&open, `{"num":%d,"kind":"pr","state":"open","created_at":%q,"updated_at":%q},`, i, now, now)
		fmt.Fprintf(&closed, `{"num":%d,"kind":"pr","state":"closed","created_at":%q,"updated_at":%q},`, 100+i, now, now)
		put(fmt.Sprintf("repos/%s/%s/pulls/%06x/pr.json", owner, repo, 100+i),
			fmt.Sprintf(`{"num":%d,"merged":true,"merged_at":%q}`, 100+i, now))
	}
	put(fmt.Sprintf("repos/%s/%s/issues/index.json", owner, repo),
		fmt.Sprintf(`{"version":1,"open":[%s],"closed_recent":[%s]}`, strings.TrimSuffix(open.String(), ","), strings.TrimSuffix(closed.String(), ",")))
	put(fmt.Sprintf("repos/%s/%s/meta/social.json", owner, repo), `{"stars":5,"watchers":2,"forks":1}`)
	for i := 0; i < 25; i++ {
		put(fmt.Sprintf("repos/%s/%s/releases/v%d.json", owner, repo, i), `{"tag":"x"}`)
		put(fmt.Sprintf("repos/%s/%s/packages/registry/tags/t%d", owner, repo, i), `{}`)
		put(fmt.Sprintf("repos/%s/%s/threads/runs/r%d.json", owner, repo, i), `{}`)
	}
}

type countingScanner struct {
	layer *git.Layer
	calls int
}

func (c *countingScanner) InsightsCommits(ctx context.Context, repo *git.LocalRepo, rev string, since time.Time, capN int) ([]git.InsightsCommit, bool, error) {
	c.calls++
	return c.layer.InsightsCommits(ctx, repo, rev, since, capN)
}

func median(ds []time.Duration) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

func TestInsightsRefreshScale(t *testing.T) {
	ctx := context.Background()
	for _, n := range []int{1000, 10000} {
		n := n
		t.Run(fmt.Sprintf("%d_commits", n), func(t *testing.T) {
			dir, head := buildFixture(t, n)
			layer := git.NewLayer()
			local := &git.LocalRepo{Path: dir}
			since := time.Now().UTC().Add(-365 * 24 * time.Hour)

			// 1) The one git invocation alone (the dominant cost).
			var scanTimes []time.Duration
			var commits int
			for i := 0; i < 3; i++ {
				start := time.Now()
				got, _, err := layer.InsightsCommits(ctx, local, head, since, scanCap)
				scanTimes = append(scanTimes, time.Since(start))
				if err != nil {
					t.Fatalf("scan: %v", err)
				}
				commits = len(got)
			}

			// 2) The full refresh (git + index/sidecar reads + 1 CAS PUT).
			st := store.NewMemory()
			seedFacts(t, st, "acme", "api", time.Now().UTC().Format(time.RFC3339))
			scanner := &countingScanner{layer: layer}
			svc := insights.New(insights.Deps{
				Store: st,
				Git:   scanner,
				Head:  func(context.Context, string, string) (string, error) { return head, nil },
				Serve: func(context.Context, string, string) (*git.LocalRepo, string, error) {
					return local, head, nil
				},
			})
			var refreshTimes []time.Duration
			var ops int64
			for i := 0; i < 3; i++ {
				before := st.OpCounter().Load()
				start := time.Now()
				if err := svc.Refresh(ctx, "acme", "api", nil); err != nil {
					t.Fatalf("refresh: %v", err)
				}
				refreshTimes = append(refreshTimes, time.Since(start))
				ops = st.OpCounter().Load() - before
			}
			if scanner.calls != 3 {
				t.Fatalf("expected exactly one git scan per refresh, got %d", scanner.calls)
			}
			t.Logf("RESULT n=%d commits=%d raw_scan_median=%s refresh_median=%s store_ops_per_refresh=%d git_calls_per_refresh=1",
				n, commits, median(scanTimes), median(refreshTimes), ops)
		})
	}
}
