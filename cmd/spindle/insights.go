// insights.go — composition for the repository Insights projection
// (docs/features/18_repo_insights.md).
//
// Three seams meet here and nowhere else:
//   - the service (internal/insights) over the store + the pinned git layer;
//   - the serving-copy resolvers (refs-level head / pack materialization),
//     backed by the registry the rest of composition already holds;
//   - the narrated refresh task (`insights-refresh`), run through the WAL task
//     table like every other piece of long work (law 7).
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/insights"
	"github.com/crueber/spindle/internal/maintain"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/wal"
)

// newInsightsService binds the projection to the store and the serving copies.
// Git work runs only from the refresh task; Head is refs-level (no pack
// download) so the maintainer's staleness probe stays cheap.
func newInsightsService(st store.ObjectStore, reg *wal.Registry) *insights.Service {
	if st == nil || reg == nil {
		return nil
	}
	return insights.New(insights.Deps{
		Store: st,
		Git:   reg.GitLayer(),
		Head: func(ctx context.Context, owner, repo string) (string, error) {
			return insightsHead(ctx, reg, owner, repo)
		},
		Serve: func(ctx context.Context, owner, repo string) (*git.LocalRepo, string, error) {
			return insightsServe(ctx, reg, owner, repo)
		},
	})
}

// insightsHead resolves the default-branch tip at the manifest's sequence. The
// refs view is authoritative; before the first checkpoint/HEAD move the WAL
// view carries no head target, so the serving copy's HEAD symref is the
// fallback (the same rule as the sizecatalog activity derivation).
func insightsHead(ctx context.Context, reg *wal.Registry, owner, repo string) (string, error) {
	h, err := reg.Open(ctx, owner+"/"+repo)
	if err != nil {
		return "", err
	}
	m, _ := h.ManifestSnapshot()
	var seq uint64
	if m != nil {
		seq = m.HeadSeq
	}
	rv, err := h.RefsAtSeq(ctx, seq)
	if err != nil {
		return "", err
	}
	if rv == nil {
		return "", nil
	}
	target := rv.HeadTarget
	if target == "" {
		target = readHeadSymref(h.Dir())
	}
	if target == "" {
		return "", nil
	}
	for _, e := range rv.Refs {
		if e.Name == target {
			return e.Oid, nil
		}
	}
	return "", nil
}

// insightsServe materializes the pack set and re-reads the head it serves, so
// the projection is stamped with the revision the scan actually covered.
func insightsServe(ctx context.Context, reg *wal.Registry, owner, repo string) (*git.LocalRepo, string, error) {
	h, err := reg.Open(ctx, owner+"/"+repo)
	if err != nil {
		return nil, "", err
	}
	if _, err := h.Sync(ctx, wal.LevelServe); err != nil {
		return nil, "", err
	}
	head, err := insightsHead(ctx, reg, owner, repo)
	if err != nil {
		return nil, "", err
	}
	return h.Repo(), head, nil
}

// readHeadSymref reads `ref: refs/heads/...` from a bare repo's HEAD file.
func readHeadSymref(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "HEAD"))
	if err != nil {
		return ""
	}
	t, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "ref: ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(t)
}

// startInsightsRefresh starts (or joins) the task and returns immediately. The
// page redirects; the task table narrates progress and the next page load reads
// the projection.
func startInsightsRefresh(reg *wal.Registry, svc *insights.Service, owner, repo string) error {
	if reg == nil || svc == nil {
		return nil
	}
	id := owner + "/" + repo
	base := context.WithoutCancel(context.Background())
	go func() {
		_, _ = reg.Tasks().Run(base, id, insights.TaskKind, nil, func(ctx context.Context, task *wal.Task) error {
			return svc.Refresh(ctx, owner, repo, task)
		})
	}()
	return nil
}

// insightsSweeper plugs the projection refresh into the maintainer's sweeper
// path (the packages-gc shape): an in-memory per-repo due gate keeps an idle
// repository off the task table, and the service's own head equality check
// keeps the git scan off a repository whose head did not move.
type insightsSweeper struct {
	svc  *insights.Service
	now  func() time.Time
	mu   sync.Mutex
	last map[string]time.Time
}

// insightsSweepInterval is the minimum spacing between refresh attempts for
// one repository. The projection's own 24 h TTL is the second gate.
const insightsSweepInterval = 30 * time.Minute

func (w *insightsSweeper) Kind() string { return insights.TaskKind }

var _ maintain.Sweeper = (*insightsSweeper)(nil)

func (w *insightsSweeper) Due(repo string) bool {
	if w.svc == nil {
		return false
	}
	now := time.Now().UTC()
	if w.now != nil {
		now = w.now().UTC()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if at, ok := w.last[repo]; ok && now.Sub(at) < insightsSweepInterval {
		return false
	}
	return true
}

func (w *insightsSweeper) Sweep(ctx context.Context, repo string, t maintain.TaskLogger) error {
	owner, name, ok := splitRepo(repo)
	if !ok || w.svc == nil {
		return nil
	}
	now := time.Now().UTC()
	if w.now != nil {
		now = w.now().UTC()
	}
	w.mu.Lock()
	if w.last == nil {
		w.last = map[string]time.Time{}
	}
	w.last[repo] = now
	w.mu.Unlock()
	_, err := w.svc.RefreshIfStale(ctx, owner, name, t)
	return err
}
