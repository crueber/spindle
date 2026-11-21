// threads.go — docs/features/15_threads.md (milestone T1) composition: the
// server-side Threads surface (workflow discovery from git, run/job records,
// the checks mirror) over the shared store/registry/config. One block for the
// package (09 §4 touch point 3), exactly like checks.go/pushmirror.go.
//
// Seams: Seam 1 is the server.ExtraRoutes chain (Handler.Handle, both lanes);
// Seam 5 is the threads-gc kind, registered once here (duplicate
// registration panics). The checks mirror binds *checks.Service.RecordStatus
// (the trusted in-process reporter) — this package never imports
// internal/checks, and the merge gate sees a Threads job exactly like an
// external CI report. The git reader is the stock-git subprocess
// (internal/threads/git.go; argv in docs/go/04_git.md §14) over the
// serve-synced repo dirs, so no worktree or checkout is ever needed.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/checks"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads"
	"github.com/crueber/spindle/internal/wal"
)

// threadsPushTimeout bounds one run-creation pass triggered by a push. The
// hook runs off the request path; the deadline keeps a hung git/store call
// from leaking a goroutine forever.
const threadsPushTimeout = 60 * time.Second

// newThreadsService builds the Threads surface over st/ident/checksSvc and
// registers its discovery templates + task kind (called once per process from
// buildCollab). A nil checks service leaves the mirror unwired; a nil registry
// leaves the git reader unable to resolve a repo (tests only).
func newThreadsService(st store.ObjectStore, ident *identity.Service, checksSvc *checks.Service, reg *wal.Registry, gitBinary string) (*threads.Service, *threads.Handler) {
	api.RegisterExposed(threads.ExposedTemplates...)
	threads.RegisterKind(threads.KindThreadsGC)
	svc := threads.New(st, ident)
	g := threads.NewSubprocessGit(gitBinary)
	if reg != nil {
		g.SetDirs(&pullsDirs{reg: reg})
	}
	svc.Git = g
	if checksSvc != nil {
		svc.Checks = threadsCheckSink{svc: checksSvc}
	}
	return svc, &threads.Handler{Svc: svc}
}

// threadsCheckSink adapts the Threads CheckSink seam onto checks.RecordStatus.
// The creator is "threads" (the stored record names the producer; the merge
// gate only reads context/state).
type threadsCheckSink struct{ svc *checks.Service }

// Report writes one job's checks record. repo is "owner/name".
func (c threadsCheckSink) Report(ctx context.Context, repo, sha, checkContext, state, description string, startedAt, completedAt *string) error {
	id, err := git.ParseRepoId(repo)
	if err != nil {
		return err
	}
	_, rerr := c.svc.RecordStatus(ctx, id.Owner, id.Name, sha, "threads", checks.ReportInput{
		Context:     checkContext,
		State:       state,
		Description: description,
		StartedAt:   startedAt,
		CompletedAt: completedAt,
	})
	return rerr
}

// chainThreads fronts the core mux with the threads JSON surface (Seam 1);
// authentication resolves through the server chain (Seam 2, including the
// §8.6 broker-forwarding rule).
func chainThreads(srv *server.Server, h *threads.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().AuthenticateForwarded(r, srv.Config())
	}
	srv.ChainExtra(h)
}

// threadsOnPushOf builds the on-push run-creation hook (15 §2): each landed
// branch ref creates the runs its workflow files trigger on. Deletes and
// non-branch refs are skipped. Nil service → nil hook.
func threadsOnPushOf(c *collabWiring) func(id git.RepoId, refs []server.PushedRef) {
	if c == nil || c.threadsSvc == nil {
		return nil
	}
	svc := c.threadsSvc
	return func(id git.RepoId, refs []server.PushedRef) {
		for _, ref := range refs {
			branch, ok := threadsRefTarget(ref)
			if !ok {
				continue // tag/other ref or a delete: no run
			}
			r, b := ref, branch
			// The server hook is already off the request path; one
			// goroutine per ref keeps a slow workflow read for one branch
			// from delaying another.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), threadsPushTimeout)
				defer cancel()
				if _, err := svc.CreateRun(ctx, id.String(), r.New, threads.EventPush, threads.CreateOpts{
					Branch: b, Ref: r.Ref, Actor: "push",
				}); err != nil {
					slog.Default().Warn("threads: create run", "repo", id.String(), "ref", r.Ref, "err", err)
				}
			}()
		}
	}
}

// onPushHookOf combines the post-push fan-outs (Forgejo #623 push mirrors and
// Threads runs). Nil when neither service exists (setup-only, bare tests).
func onPushHookOf(c *collabWiring) func(id git.RepoId, refs []server.PushedRef) {
	pm := pushMirrorHookOf(c)
	th := threadsOnPushOf(c)
	switch {
	case pm == nil:
		return th
	case th == nil:
		return pm
	}
	return func(id git.RepoId, refs []server.PushedRef) {
		pm(id, refs)
		th(id, refs)
	}
}

// threadsRefTarget reports whether a landed ref starts a Threads run: only
// refs/heads/ creates/updates do (tags, notes and deletes do not).
func threadsRefTarget(ref server.PushedRef) (string, bool) {
	branch, ok := strings.CutPrefix(ref.Ref, "refs/heads/")
	if !ok || branch == "" || isZeroOID(ref.New) {
		return "", false
	}
	return branch, true
}

// isZeroOID reports a delete command's all-zero new oid (40 or 64 hex zeros).
func isZeroOID(oid string) bool {
	if oid == "" {
		return true
	}
	return strings.Trim(oid, "0") == ""
}

// compile-time seam assertions: composition consumes exactly the narrow
// interfaces threads defines (core never imports threads).
var (
	_ threads.RoleService = (*identity.Service)(nil)
	_ threads.GitReader   = (*threads.SubprocessGit)(nil)
	_ threads.CheckSink   = threadsCheckSink{}
)
