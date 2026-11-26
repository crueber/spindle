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
	"github.com/crueber/spindle/internal/issues"
	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/releases"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads"
	"github.com/crueber/spindle/internal/threads/workflow"
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
		// Operator knobs (PAR-179): the artifact window (separate from run
		// retention), keep-forever, the requeue ceiling and the base job lease.
		svc.ArtifactRetention = time.Duration(cfg.Threads.ArtifactRetention)
		svc.KeepForever = cfg.Threads.KeepForever
		svc.MaxQueueAttempts = cfg.Threads.MaxQueueAttempts
		svc.JobLease = time.Duration(cfg.Threads.JobLease)
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

// threadsTeamRoster adapts the Threads TeamRoster seam onto identity's team
// records (PAR-131 environment team reviewers). A missing team resolves to no
// members, which the Threads validator refuses by name (fail closed) rather
// than holding a deployment nobody can release.
type threadsTeamRoster struct {
	ident *identity.Service
}

// TeamMembers returns one team's member principals.
func (t threadsTeamRoster) TeamMembers(ctx context.Context, org, team string) ([]string, error) {
	if t.ident == nil {
		return nil, fmt.Errorf("threads: team directory not wired")
	}
	tm, _, err := t.ident.GetTeam(ctx, org, team)
	if err != nil {
		return nil, err
	}
	if tm == nil {
		return nil, nil
	}
	return append([]string{}, tm.Members...), nil
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

// threadsOnPushOf builds the on-push run-creation hook (15 §2, §6.1): each
// landed branch or tag ref creates the runs its workflow files trigger on — a
// branch push matches `on.push.branches` by the branch name, a tag push matches
// `on.push.tags` by the tag name and carries no branch. Deletes, notes and
// other refs are skipped, and a push authenticated by a Threads job token
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
			name, kind, ok := threadsRefTarget(ref)
			if !ok {
				continue // notes, pull refs or a delete: no run
			}
			opts := threads.CreateOpts{
				RefName: name, RefKind: kind, Ref: ref.Ref, Actor: "push",
				// The pre-image is what a `paths:` filter diffs against; a new
				// ref leaves it empty (the pushed commit's own change set).
				BaseSHA:      ref.Old,
				EventPayload: threads.PushEventPayload(id.String(), ref.Ref, ref.Old, ref.New, "push"),
			}
			if kind == workflow.RefBranch {
				opts.Branch = name // a tag run has no branch; its tag is the RefName
			}
			threadsRun(svc, id.String(), ref.New, threads.EventPush, opts)
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
		// lifecycle edge the `types` filter reads; BaseSHA is the merge-base
		// side of the changed-path filter.
		base := threadsBranchName(ev.BaseRef)
		threadsRun(svc, ev.Repo, ev.HeadSHA, threads.EventPullReq, threads.CreateOpts{
			Branch: base, Action: ev.Action, Ref: fmt.Sprintf("refs/pull/%d/head", ev.Num),
			BaseBranch: base, BaseSHA: ev.BaseSHA, Fork: ev.Fork, Actor: ev.Actor,
			EventPayload: threads.PullEventPayload(ev.Repo, ev.Action, ev.Num, ev.Title, ev.State,
				authorOfPull(ev), ev.BaseRef, ev.BaseSHA, ev.HeadRef, ev.HeadSHA, ev.HeadRepo,
				ev.Draft, ev.Merged, ev.Fork, ev.Actor),
		})
	}
}

// authorOfPull is the sender on a lifecycle event (the PR author is in the
// payload's pull_request.user in the feature package; the projection here uses
// the actor for sender, which is what github.event.sender means).
func authorOfPull(ev pulls.PullEvent) string { return ev.Actor }

// threadsIssueEventOf routes the issues service's trigger edges (PAR-020): an
// `issues` action, an `issue_comment` created edge, and the pull_request
// metadata half (labeled/assigned/milestoned) for a PR thread. Every run reads
// the DEFAULT branch's workflow files (CreateRun owns that rule) and carries
// the documented payload.
func threadsIssueEventOf(c *collabWiring) func(ctx context.Context, ev issues.ThreadEvent) {
	if c == nil || c.threadsSvc == nil {
		return nil
	}
	svc := c.threadsSvc
	return func(ctx context.Context, ev issues.ThreadEvent) {
		repo := ev.Repo
		if ev.Kind == "pr" {
			info, ok := pullsTriggerInfo(ctx, c, repo, ev.Num)
			if !ok {
				return // the PR was deleted between the write and the trigger
			}
			base := threadsBranchName(info.BaseRef)
			threadsRun(svc, repo, info.HeadSHA, threads.EventPullReq, threads.CreateOpts{
				Action: ev.Action, Branch: base, BaseBranch: base, BaseSHA: info.BaseSHA,
				Fork: info.Fork, Ref: fmt.Sprintf("refs/pull/%d/head", info.Num), Actor: ev.Actor,
				EventPayload: threads.PullEventPayload(repo, ev.Action, info.Num, info.Title, info.State,
					info.Author, info.BaseRef, info.BaseSHA, info.HeadRef, info.HeadSHA, info.HeadRepo,
					info.Draft, info.Merged, info.Fork, ev.Actor),
			})
			return
		}
		event := threads.EventIssues
		payload := threads.IssueEventPayload(repo, ev.Action, ev.Actor, ev.Num, ev.Title, ev.State, ev.Labels)
		if ev.Kind == "issue_comment" {
			event = threads.EventIssueComment
			payload = threads.IssueCommentEventPayload(repo, ev.Action, ev.Actor, ev.CommentBody,
				ev.Num, ev.Title, ev.State, ev.Labels, ev.CommentActor)
		}
		// GitHub's repository-level events read the workflow from the default
		// branch; CreateRun enforces that for the file, and the run reports the
		// default branch tip as its commit.
		commit, branch := defaultBranchTarget(ctx, c, repo)
		if commit == "" {
			return
		}
		opts := threads.CreateOpts{Actor: ev.Actor, Action: ev.Action, EventPayload: payload}
		if branch != "" {
			opts.Branch = branch
			opts.Ref = "refs/heads/" + branch
		}
		threadsRun(svc, repo, commit, event, opts)
	}
}

// pullsTriggerInfo reads the PR identifiers for a metadata trigger (two
// exact-key GETs). A missing PR drops the event (it was deleted mid-write).
func pullsTriggerInfo(ctx context.Context, c *collabWiring, repo string, num int) (pulls.TriggerInfo, bool) {
	if c == nil || c.pullsSvc == nil {
		return pulls.TriggerInfo{}, false
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return pulls.TriggerInfo{}, false
	}
	return c.pullsSvc.PRTriggerInfo(ctx, owner, name, num)
}

// threadsReleaseEventOf routes the releases service's edges (PAR-017): the
// workflow comes from the default branch, the run reports the release tag's
// commit, and the payload carries the tag and asset list.
func threadsReleaseEventOf(c *collabWiring) releases.ReleaseEventSink {
	if c == nil || c.threadsSvc == nil {
		return nil
	}
	svc := c.threadsSvc
	return func(ctx context.Context, ev releases.ReleaseEvent) {
		assets := make([]threads.ReleaseAsset, 0, len(ev.Assets))
		for _, a := range ev.Assets {
			assets = append(assets, threads.ReleaseAsset{Name: a.Name, Size: a.Size})
		}
		commit := ev.TagSHA
		if commit == "" {
			// A deleted release has no tag object any more; the default branch
			// is the only commit that still exists, and CreateRun reads the
			// workflow there regardless.
			commit = defaultBranchSHA(ctx, c, ev.Repo)
		}
		if commit == "" {
			return
		}
		threadsRun(svc, ev.Repo, commit, threads.EventRelease, threads.CreateOpts{
			Actor: ev.Actor, Action: ev.Action, Ref: "refs/tags/" + ev.Tag, RefName: ev.Tag, Branch: "",
			EventPayload: threads.ReleaseEventPayload(ev.Repo, ev.Action, ev.Tag, ev.Name,
				ev.TagSHA, ev.Actor, ev.Draft, ev.Prerelease, assets),
		})
	}
}

// defaultBranchTarget resolves the default branch tip and its short name for a
// repository-level event.
func defaultBranchTarget(ctx context.Context, c *collabWiring, repo string) (sha, branch string) {
	sha = defaultBranchSHA(ctx, c, repo)
	if sha == "" {
		return "", ""
	}
	if br, ok := c.threadsSvc.Git.(threads.DefaultBranchReader); ok {
		if name, err := br.DefaultBranch(ctx, repo); err == nil {
			branch = threadsBranchName(name)
		}
	}
	return sha, branch
}

// defaultBranchSHA resolves the default branch tip for a release-deleted edge.
func defaultBranchSHA(ctx context.Context, c *collabWiring, repo string) string {
	if c == nil || c.threadsSvc == nil || c.threadsSvc.Git == nil {
		return ""
	}
	sha, err := c.threadsSvc.Git.ResolveRef(ctx, repo, "HEAD")
	if err != nil {
		return ""
	}
	return sha
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
	return func(_ context.Context, repo, ref, oldSHA, newSHA string) {
		branch, ok := strings.CutPrefix(ref, "refs/heads/")
		if !ok || branch == "" || isZeroOID(newSHA) {
			return
		}
		threadsRun(svc, repo, newSHA, threads.EventPush, threads.CreateOpts{
			Branch: branch, Ref: ref, Actor: "merge", BaseSHA: oldSHA,
			EventPayload: threads.PushEventPayload(repo, ref, oldSHA, newSHA, "merge"),
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

// threadsRefTarget reports whether a landed ref starts a Threads run, and if
// so, the ref's short name and kind (15 §6.1): a `refs/heads/…` create/update
// is a branch push and a `refs/tags/…` create/update is a tag push. Notes,
// pull refs and deletes (all-zero new oid) start nothing.
func threadsRefTarget(ref server.PushedRef) (string, workflow.RefKind, bool) {
	if isZeroOID(ref.New) {
		return "", workflow.RefBranch, false
	}
	if branch, ok := strings.CutPrefix(ref.Ref, "refs/heads/"); ok && branch != "" {
		return branch, workflow.RefBranch, true
	}
	if tag, ok := strings.CutPrefix(ref.Ref, "refs/tags/"); ok && tag != "" {
		return tag, workflow.RefTag, true
	}
	return "", workflow.RefBranch, false
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
