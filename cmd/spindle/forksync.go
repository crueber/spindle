// forksync.go — PLAN-V1 WP5 composition: the "Sync fork" task (Seam 5 kind
// registered once), the repo lane surface, and the local pack-sharing fetch
// that stands in for a network clone (a fork's parent is another repo on
// THIS instance; its pack bytes already live in the shared bucket, so the
// fetch is a manifest share — the fork's §7 creation shape — not a clone).
//
// SSRF still applies: the composition derives the upstream URL from
// server.public_url and screens it with the [import] gate (the mirror/import
// rules) before any work. The fetch itself never dials: it reads the parent's
// materialized packs through the registry.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/egress"
	"github.com/crueber/spindle/internal/forksync"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/store/proto"
	"github.com/crueber/spindle/internal/wal"
)

// newForksyncService builds the service + handler and registers the kind.
func newForksyncService(st store.ObjectStore, ident *identity.Service, reg *wal.Registry, cfg *config.Config) (*forksync.Service, *forksync.Handler) {
	forksync.RegisterKind(forksync.KindSyncFork)
	api.RegisterExposed(forksync.ExposedTemplates...)
	svc := &forksync.Service{
		Store:     st,
		Roles:     ident,
		Refs:      &forkRefs{reg: reg, gitBinary: cfg.Git.Binary},
		Fetcher:   &forkPackShare{reg: reg},
		Publisher: &forkRefPublisher{reg: reg},
		Tasks:     &forkTasks{tasks: reg.Tasks()},
		Guard: func(rawURL string) error {
			// The upstream is operator-derived (server.public_url + the
			// fork.json parent), never user input, and the fetch itself is
			// local pack sharing (no dial) — so the screen is the shared
			// egress table: refuse an upstream host that resolves into
			// metadata/RFC1918/etc. Loopback stays allowed (the parent is
			// this instance; the egress rule allows loopback for exactly
			// this reason). `import.allow_private_networks` lifts the screen
			// for an intentionally internal deployment, same knob as
			// import/mirror. Fail closed on parse/resolve errors.
			u, perr := url.Parse(rawURL)
			if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
				return fmt.Errorf("upstream URL %q is not an http(s) URL", rawURL)
			}
			if cfg.Import.AllowPrivateNetworks {
				return nil
			}
			ips, rerr := net.DefaultResolver.LookupNetIP(context.Background(), "ip", u.Hostname())
			if rerr != nil {
				return fmt.Errorf("cannot resolve upstream host %q: %v", u.Hostname(), rerr)
			}
			for _, ip := range ips {
				if egress.BlockedIP(ip) {
					return fmt.Errorf("upstream host %q resolves to a non-public address (refused)", u.Hostname())
				}
			}
			return nil
		},
		UpstreamURL: func(parent string) (string, error) {
			base := strings.TrimSuffix(cfg.Server.PublicURL, "/")
			if base == "" {
				return "", fmt.Errorf("server.public_url is required to resolve the upstream")
			}
			return base + "/" + parent + ".git", nil
		},
	}
	return svc, &forksync.Handler{Svc: svc}
}

// chainForksync fronts the core api mux (Seam 1) with the server's principal
// chain (Seam 2, including broker forwarding).
func chainForksync(srv *server.Server, h *forksync.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().AuthenticateForwarded(r, srv.Config())
	}
	srv.ChainExtra(h)
}

// --- refs ------------------------------------------------------------------

type forkRefs struct {
	reg       *wal.Registry
	gitBinary string
}

// DefaultBranch resolves the repo's HEAD branch (short name) and tip.
func (f *forkRefs) DefaultBranch(ctx context.Context, repo string) (string, string, error) {
	h, err := f.reg.Open(ctx, repo)
	if err != nil {
		return "", "", err
	}
	g, serr := h.Sync(ctx, wal.LevelRefs)
	if serr != nil {
		return "", "", serr
	}
	defer g.Release()
	snap, serr := h.Layer().Snapshot(h.Repo())
	if serr != nil {
		return "", "", serr
	}
	target := snap.HeadTarget
	if target == "" {
		target = "refs/heads/main"
	}
	for _, r := range snap.Refs {
		if r.Name == target {
			sha := string(r.Oid)
			if r.Peeled != "" {
				sha = string(r.Peeled)
			}
			return strings.TrimPrefix(target, "refs/heads/"), sha, nil
		}
	}
	// Unborn repo: HEAD names a branch with no tip.
	return strings.TrimPrefix(target, "refs/heads/"), strings.Repeat("0", 40), nil
}

// IsAncestor runs `git merge-base --is-ancestor` in the fork's materialized
// serving copy (the fetch step installed the parent's packs there).
func (f *forkRefs) IsAncestor(ctx context.Context, repo, ancestor, descendant string) (bool, error) {
	h, err := f.reg.Open(ctx, repo)
	if err != nil {
		return false, err
	}
	sg, serr := h.Sync(ctx, wal.LevelServe)
	if serr != nil {
		return false, serr
	}
	sg.Release()
	cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	bin := f.gitBinary
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(cctx, bin, "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = h.Dir()
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0", "GIT_DIR=" + h.Dir()}
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return true, nil
	}
	if ee, ok := runErr.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("merge-base --is-ancestor: %v (%s)", runErr, strings.TrimSpace(string(out)))
}

// --- pack sharing (the local "fetch") ---------------------------------------

type forkPackShare struct{ reg *wal.Registry }

// Fetch makes the upstream branch reachable from the fork: it materializes
// the parent, then AddPack's every parent pack the fork's manifest does not
// already reference (content-addressed bytes are ALREADY in the bucket — the
// fork-network sharing shape of docs/features/03 §7). No network, no clone.
func (f *forkPackShare) Fetch(ctx context.Context, repo, upstreamRepo, branch string, progress func(label string, done uint64)) (string, error) {
	ph, err := f.reg.Open(ctx, upstreamRepo)
	if err != nil {
		return "", err
	}
	pg, err := ph.Sync(ctx, wal.LevelServe)
	if err != nil {
		return "", err
	}
	pg.Release()
	pm, _ := ph.ManifestSnapshot()
	if pm == nil {
		return "", fmt.Errorf("upstream manifest unavailable")
	}
	ch, err := f.reg.Open(ctx, repo)
	if err != nil {
		return "", err
	}
	cg, err := ch.Sync(ctx, wal.LevelServe)
	if err != nil {
		return "", err
	}
	cg.Release()
	cm, _ := ch.ManifestSnapshot()
	have := map[string]bool{}
	if cm != nil {
		for _, p := range cm.Packs {
			have[p.Checksum] = true
		}
	}
	total := uint64(len(pm.Packs))
	for i, p := range pm.Packs {
		if have[p.Checksum] {
			continue
		}
		path := filepath.Join(ph.Repo().PackDir(), "pack-"+p.Checksum+".pack")
		if _, serr := os.Stat(path); serr != nil {
			return "", fmt.Errorf("upstream pack %s not materialized: %v", p.Checksum, serr)
		}
		progress("ingest", uint64(i+1))
		if _, aerr := ch.AddPack(ctx, path, p.Checksum, p.Tier, map[string]string{"kind": forksync.KindSyncFork}); aerr != nil {
			return "", fmt.Errorf("share pack %s: %v", p.Checksum, aerr)
		}
	}
	if total > 0 {
		progress("ingest", total)
	}
	return "", nil
}

// --- publish ---------------------------------------------------------------

type forkRefPublisher struct{ reg *wal.Registry }

// FastForward publishes refs/heads/<branch> old → new (CAS: a moved branch
// fails the old-oid check; never a force).
func (p *forkRefPublisher) FastForward(ctx context.Context, repo, branch, oldSHA, newSHA string) error {
	h, err := p.reg.Open(ctx, repo)
	if err != nil {
		return err
	}
	g, serr := h.Sync(ctx, wal.LevelServe)
	if serr != nil {
		return serr
	}
	g.Release()
	res, err := h.Publish(ctx, wal.PublishRequest{
		Txn: &proto.RefTransaction{Updates: []*proto.RefUpdate{{
			Name:   "refs/heads/" + branch,
			OldOid: oldSHA,
			NewOid: newSHA,
		}}},
		Meta: map[string]string{"kind": forksync.KindSyncFork},
	})
	if err != nil {
		return err
	}
	for _, rr := range res.PerRef {
		if rr.Err != nil {
			return fmt.Errorf("%s: %s", rr.Name, rr.Err.Detail)
		}
	}
	return nil
}

// --- tasks -----------------------------------------------------------------

type forkTasks struct{ tasks *wal.TaskTable }

// Start runs the task on its own goroutine and answers with its id as soon as
// the record exists (the 202 contract); a second start joins the running
// leader (single-flight) and reports its id.
func (t *forkTasks) Start(ctx context.Context, repo, kind string, params map[string]string, fn func(ctx context.Context, tl forksync.TaskLogger) error) (string, error) {
	if rec := t.tasks.Running(repo, kind); rec != nil {
		return rec.ID, nil
	}
	idCh := make(chan string, 1)
	go func() {
		_, _ = t.tasks.Run(context.WithoutCancel(ctx), repo, kind, params, func(tctx context.Context, task *wal.Task) error {
			select {
			case idCh <- task.Record().ID:
			default:
			}
			return fn(tctx, task)
		})
	}()
	select {
	case id := <-idCh:
		return id, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Get projects a task record for the JSON surface.
func (t *forkTasks) Get(id string) (forksync.TaskInfo, bool) {
	rec := t.tasks.Get(id)
	if rec == nil {
		return forksync.TaskInfo{}, false
	}
	info := forksync.TaskInfo{ID: rec.ID, Kind: rec.Kind, Repo: rec.Repo,
		Started: rec.Started, Finished: rec.Finished, OK: rec.OK, Summary: rec.Summary}
	if board, ok := rec.Params["result"]; ok {
		info.Result = board
	}
	return info, true
}
