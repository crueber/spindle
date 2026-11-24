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
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/checks"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/pulls"
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
func newThreadsService(st store.ObjectStore, ident *identity.Service, checksSvc *checks.Service, reg *wal.Registry, cfg *config.Config, gitBinary string) (*threads.Service, *threads.Handler) {
	api.RegisterExposed(threads.ExposedTemplates...)
	threads.RegisterKind(threads.KindThreadsGC)
	threads.RegisterKind(threads.KindThreadsQueueReconcile)
	threads.RegisterKind(threads.KindThreadsSchedule)
	svc := threads.New(st, ident)
	if cfg != nil {
		// Runtime policy (docs/features/15 §8): retention window, the
		// first-time-fork approval gate and the job-token default.
		svc.Retention = time.Duration(cfg.Threads.Retention)
		svc.ApproveFirstTimeForks = cfg.Threads.ApproveFirstTimeForks
		svc.DefaultPermissions = cfg.Threads.DefaultPermissions
		svc.MaxArtifactBytesPerRun = int64(cfg.Threads.MaxArtifactBytesPerRun)
		svc.MaxArtifactBytesPerFile = int64(cfg.Threads.MaxArtifactBytesPerFile)
		// The checkout URL prefers the configured public origin: it is the
		// address the operator declared reachable from job containers, where
		// the request origin may be a browser-only name (docs/threads.md §4.4).
		svc.CheckoutBaseURL = cfg.Server.PublicURL
		// The instance-wide image gate (docs/PLAN-ARTIFACT-KEEPER.md §3.3):
		// applied at run creation over container.image and services.*.image.
		svc.ImageAllowPrefixes = cfg.Threads.ImageAllowPrefixes
		svc.ImageRequireDigest = cfg.Threads.ImageRequireDigest
	}
	// Protocol keys (15 §5.3): server.secrets_key wins over the session secret;
	// without either, attempt tokens are process-local and repository secrets
	// are unavailable (a loud warning, never a silent fallback).
	if cfg != nil {
		keyring, kerr := threads.NewKeyring(cfg.Server.Auth.SessionSecret, cfg.Server.SecretsKey)
		if kerr != nil {
			slog.Default().Warn("threads: derive protocol keys", "err", kerr)
		} else {
			svc.SetKeyring(keyring)
			if keyring.Ephemeral {
				slog.Default().Warn("threads: no server.auth.session_secret or server.secrets_key — attempt tokens are process-local and repository secrets are disabled")
			}
		}
	}
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

// threadsApprovals adapts the Threads ApprovalPolicy seam onto pulls'
// merged-author projection (one exact-key GET) and the identity ladder (repo
// role or configured instance admin), docs/features/15 §5.2. The Threads
// package never imports pulls or identity (law 8): composition binds both.
type threadsApprovals struct {
	svc   *pulls.Service
	ident *identity.Service
	cfg   *config.Config
}

// MergedAuthor reports whether author has a merged PR in the repository.
func (a threadsApprovals) MergedAuthor(ctx context.Context, owner, repo, author string) (bool, error) {
	if a.svc == nil {
		return false, nil
	}
	return a.svc.MergedAuthor(ctx, owner, repo, author)
}

// Maintainer reports whether actor holds maintain/admin in the repository. The
// answer comes from the identity ladder only: the auth chain derives p.Admin
// per request, and the actor here is only a NAME (a PR event carries no
// principal). Static tokens can never administer (PLAN-OIDC-ONLY §1.3), so an
// OIDC admin known only by group is not resolvable from a name and may be
// gated once — they can approve it (the gate is not a lockout).
func (a threadsApprovals) Maintainer(ctx context.Context, owner, repo, actor string) (bool, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return false, nil
	}
	svc := a.ident
	if svc == nil {
		return false, nil
	}
	role, _ := svc.Resolve(ctx, owner, repo, auth.Principal{Name: actor})
	switch strings.ToLower(string(role)) {
	case "maintain", "admin":
		return true, nil
	}
	return false, nil
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
// §8.6 broker-forwarding rule). It also registers the job checkout credential
// (pass B2): a wjt_ attempt token resolves to an unprivileged ci:<runner>
// principal for git-over-HTTP, so a job container can clone read-only while
// the checks package's wct_ shape (wired earlier) keeps working.
func chainThreads(srv *server.Server, h *threads.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().AuthenticateForwarded(r, srv.Config())
	}
	if a := srv.Auth(); a != nil {
		prev := a.ExtraCredential
		a.ExtraCredential = func(token string) (auth.Principal, *auth.AuthError, bool) {
			if p, aerr, ok := h.Svc.ResolveCloneCredential(token); ok {
				return p, aerr, ok
			}
			if p, aerr, ok := threads.ClaimRunnerCredential(token); ok {
				return p, aerr, ok
			}
			if prev != nil {
				return prev(token)
			}
			return auth.Principal{}, nil, false
		}
	}
	srv.ChainExtra(h)
}

// threadsRun creates one run off the request path. It is the single
// run-creation entry point for every server-side trigger — the client push
// hook, the merge task's server-side base publish, and the pull_request
// lifecycle edges — so no trigger grows a second implementation. The deadline
// keeps a hung git/store call from leaking a goroutine forever.
func threadsRun(svc *threads.Service, repo, sha, event string, opts threads.CreateOpts) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), threadsPushTimeout)
		defer cancel()
		if _, err := svc.CreateRun(ctx, repo, sha, event, opts); err != nil {
			slog.Default().Warn("threads: create run", "repo", repo, "event", event, "sha", sha, "err", err)
		}
	}()
}

// threadsOnPushOf builds the on-push run-creation hook (15 §2): each landed
// branch ref creates the runs its workflow files trigger on. Deletes and
// non-branch refs are skipped, and a push authenticated by a Threads job token
// (byJob) creates nothing at all: GitHub's GITHUB_TOKEN rule — a job's own
// `git push` must never retrigger workflows, or a `contents: write` workflow
// rebuilds itself forever (the round-3 audit's loop). Nil service → nil hook.
func threadsOnPushOf(c *collabWiring) func(id git.RepoId, refs []server.PushedRef, byJob bool) {
	if c == nil || c.threadsSvc == nil {
		return nil
	}
	svc := c.threadsSvc
	return func(id git.RepoId, refs []server.PushedRef, byJob bool) {
		if byJob {
			return // a job token's push never starts a workflow run
		}
		for _, ref := range refs {
			branch, ok := threadsRefTarget(ref)
			if !ok {
				continue // tag/other ref or a delete: no run
			}
			threadsRun(svc, id.String(), ref.New, threads.EventPush, threads.CreateOpts{
				Branch: branch, Ref: ref.Ref, Actor: "push",
			})
		}
	}
}

// threadsPullEventOf builds the pull_request trigger sink (15 §1): opened,
// synchronize and reopened each create runs from the head commit. A fork PR
// runs the BASE branch's workflow files untrusted — CreateRun owns that
// selection (BaseBranch/BaseCommit + Fork), so the trust rule is not re-derived
// here. The pulls service calls the sink synchronously; threadsRun detaches it.
func threadsPullEventOf(c *collabWiring) pulls.PullEventSink {
	if c == nil || c.threadsSvc == nil {
		return nil
	}
	svc := c.threadsSvc
	return func(_ context.Context, ev pulls.PullEvent) {
		// ev.BaseRef arrives as the PR's base ref ("refs/heads/main"); Branch is
		// the plain name every trigger filter and the base-branch resolution
		// expect (a fork run resolves refs/heads/<BaseBranch>). Action is the
		// lifecycle edge (opened/synchronize/reopened) the `types` filter
		// reads; pulls never emits close, so a `types: [closed]` workflow is a
		// parse error rather than a dead trigger.
		base := threadsBranchName(ev.BaseRef)
		threadsRun(svc, ev.Repo, ev.HeadSHA, threads.EventPullReq, threads.CreateOpts{
			Branch: base, Action: ev.Action, Ref: fmt.Sprintf("refs/pull/%d/head", ev.Num),
			BaseBranch: base, Fork: ev.Fork, Actor: ev.Actor,
		})
	}
}

// threadsBranchName strips a refs/heads/ prefix (the trigger filters and the
// fork base resolution speak plain branch names).
func threadsBranchName(ref string) string {
	if branch, ok := strings.CutPrefix(strings.TrimSpace(ref), "refs/heads/"); ok {
		return branch
	}
	return strings.TrimSpace(ref)
}

// threadsBranchPublishedOf builds the merge-publish hook (15 §2): the merge
// task moves the base ref server-side, bypassing the client push pipeline that
// fires onPush, so the SAME CreateRun path is invoked here for the merge
// commit. Deletes are skipped like any other push.
func threadsBranchPublishedOf(c *collabWiring) pulls.BranchPublishedSink {
	if c == nil || c.threadsSvc == nil {
		return nil
	}
	svc := c.threadsSvc
	return func(_ context.Context, repo, ref, _, newSHA string) {
		branch, ok := strings.CutPrefix(ref, "refs/heads/")
		if !ok || branch == "" || isZeroOID(newSHA) {
			return
		}
		threadsRun(svc, repo, newSHA, threads.EventPush, threads.CreateOpts{
			Branch: branch, Ref: ref, Actor: "merge",
		})
	}
}

// onPushHookOf combines the post-push fan-outs (Forgejo #623 push mirrors,
// Threads runs, and the 02 §15 issue-template refresh). Nil when no consumer
// exists (setup-only, bare tests).
func onPushHookOf(c *collabWiring) func(id git.RepoId, refs []server.PushedRef, byJob bool) {
	hooks := []func(id git.RepoId, refs []server.PushedRef, byJob bool){
		pushMirrorHookOf(c),
		threadsOnPushOf(c),
		issuesTemplatesOnPushOf(c),
	}
	live := make([]func(id git.RepoId, refs []server.PushedRef, byJob bool), 0, len(hooks))
	for _, h := range hooks {
		if h != nil {
			live = append(live, h)
		}
	}
	switch len(live) {
	case 0:
		return nil
	case 1:
		return live[0]
	}
	return func(id git.RepoId, refs []server.PushedRef, byJob bool) {
		for _, h := range live {
			h(id, refs, byJob)
		}
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
