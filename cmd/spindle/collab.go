// collab.go — Feature 09 integration (docs/features/09_rollout.md §4): the
// ONE place that assembles the collaboration layer (Features 01–08) over
// the frozen seams. serveHTTP calls buildCollab + chainCollab;
// integration tests reuse buildCollab so the measured composition is the
// shipped composition. No new product surface: pure wiring, one block per
// package (09 §4 touch point 3).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/archive"
	"github.com/crueber/spindle/internal/branches"
	"github.com/crueber/spindle/internal/checks"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/forksync"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/insights"
	"github.com/crueber/spindle/internal/issues"
	"github.com/crueber/spindle/internal/mirror"
	"github.com/crueber/spindle/internal/moderation"
	"github.com/crueber/spindle/internal/notify"
	"github.com/crueber/spindle/internal/packages"
	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/pushmirror"
	"github.com/crueber/spindle/internal/releases"
	"github.com/crueber/spindle/internal/repoimport"
	"github.com/crueber/spindle/internal/review"
	"github.com/crueber/spindle/internal/search"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/social"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/tags"
	"github.com/crueber/spindle/internal/threads"
	"github.com/crueber/spindle/internal/wal"
	"github.com/crueber/spindle/internal/webedit"
)

// collabWiring holds every collaboration service + handler (never built in
// setup-only mode, where there is no store).
type collabWiring struct {
	ident        *identity.Service
	identHandler *identity.Handler
	// Feature M1 (docs/features/16_moderation.md): the block list, its
	// both-lane top-level API, and the audit family skeleton. Its gates are
	// consumer-owned, so it is wired onto each consumer below.
	modSvc        *moderation.Service
	modHandler    *moderation.Handler
	createHandler *api.CreateHandler // #210 POST /api/v1/repos twin (Seam 1)
	issuesSvc     *issues.Service
	issuesHandler *issues.Handler
	pullsSvc      *pulls.Service
	pullsHandler  *pulls.Handler
	// pullsPub is the WAL ref publisher pulls writes through; serve attaches
	// the events-bridge wake once the bridge (created later) exists.
	pullsPub *pullsPublisher
	// Feature F (file edit): the service, its both-lane handler, and the WAL
	// publisher whose events-bridge wake serve attaches alongside pullsPub.
	webeditSvc        *webedit.Service
	webeditHandler    *webedit.Handler
	webeditPub        *webeditPublisher
	reviewSvc         *review.Service
	reviewHandler     *review.Handler
	checksSvc         *checks.Service
	checksHandler     *checks.Handler
	threadsSvc        *threads.Service
	threadsHandler    *threads.Handler
	releasesSvc       *releases.Service
	releasesHandler   *releases.Handler
	tagsSvc           *tags.Service
	tagsHandler       *tags.Handler
	branchesSvc       *branches.Service
	socialSvc         *social.Service
	socialHandler     *social.Handler
	notifySvc         *notify.Service
	notifyHandler     *notify.Handler
	importSvc         *repoimport.Service
	importHandler     *repoimport.Handler
	mirrorSvc         *mirror.Service
	mirrorHandler     *mirror.Handler
	pushMirrorSvc     *pushmirror.Service
	pushMirrorHandler *pushmirror.Handler
	packagesSvc       *packages.Service
	packagesHandler   *packages.Handler
	// Insights (docs/features/18): the cached repository pulse projection and
	// the on-demand refresh starter (the maintainer sweeper calls the service
	// directly).
	insightsSvc   *insights.Service
	insightsStart func(ctx context.Context, owner, repo string) error
	// searchSvc is the code-search service (docs/features/20_search.md): the
	// scopes, query grammar, permission-aware fan-out and completeness. P3
	// binds its API routes through api.Env.Search; P5 owns the pages.
	searchSvc       *search.Service
	archiveHandler  *archive.Handler
	forksyncSvc     *forksync.Service
	forksyncHandler *forksync.Handler
}

// newIdentityService builds the Wave A identity surface (docs/features/01:
// the access.json/org/team surface, Seam 1, both lanes) over st/cfg and
// registers its discovery templates (Forgejo #272 — template + handler
// land in the same change, law 12; the Feature 10 precedent).
func newIdentityService(st store.ObjectStore, cfg *config.Config) (*identity.Service, *identity.Handler) {
	api.RegisterExposed(identity.ExposedTemplates...)
	svc := identity.New(st, cfg)
	return svc, &identity.Handler{Svc: svc}
}

// newIssuesService builds the Wave B issues surface (docs/features/02:
// the thread/event/label/milestone surface, Seam 1, both lanes) over
// st/ident and registers its discovery templates (Forgejo #272 — template
// + handler land in the same change, law 12; the Feature 10 precedent).
func newIssuesService(st store.ObjectStore, ident *identity.Service, cfg *config.Config) (*issues.Service, *issues.Handler) {
	api.RegisterExposed(issues.ExposedTemplates...)
	svc := issues.New(st, ident)
	// Forgejo #564: dropped best-effort writes (index cards, milestone
	// counters) log instead of vanishing — same precedent as
	// wireNotifyFanout's svc.Logger = slog.Default() (notify.go).
	svc.Log = slog.Default()
	svc.MaxImageBytes = int64(cfg.Attachments.MaxImageBytes)
	if svc.MaxImageBytes <= 0 {
		svc.MaxImageBytes = issues.DefaultMaxImageBytes
	}
	if cfg.Cache.Dir != "" {
		svc.SpoolDir = filepath.Join(cfg.Cache.Dir, "attachments-spool")
	}
	return svc, &issues.Handler{Svc: svc}
}

// buildCollab assembles every collaboration service + handler over the
// shared store/registry/config and binds the nil-safe cross-package seams
// (fan-out, counters, frames, access bootstrap). apiEnv may be nil in
// tests that only need the push path (back-bindings are skipped then).
func buildCollab(st store.ObjectStore, cfg *config.Config, reg *wal.Registry, apiEnv *api.Env, live *config.LiveImport) *collabWiring {
	c := &collabWiring{}
	// Wave A identity (docs/features/01): the access.json/org/team
	// surface (Seam 1, both lanes), the require_read gate (Seam 3
	// expansion wired into dry-run via GroupExpander), and the
	// access-bootstrap op (Seam 5).
	c.ident, c.identHandler = newIdentityService(st, cfg)
	// Feature M1 moderation: blocks + audit over identity's user namespace
	// probe. Built here because its gate is needed by every collaboration
	// service below; wired onto them once they exist.
	c.modSvc, c.modHandler = newModerationService(st, c.ident, reg)
	var _ api.CreateOwnerGate = c.ident
	var _ api.AccessBootstrap = c.ident
	var _ api.OwnerEditor = c.ident
	var _ api.OrgLister = c.ident
	var _ api.UserAvatar = c.ident
	if apiEnv != nil {
		apiEnv.Access = c.ident
		apiEnv.GroupExpander = c.ident.PolicyExpander()
		apiEnv.CreateOwnerGate = c.ident
		apiEnv.AccessBoot = c.ident
		apiEnv.OwnerEdit = c.ident // Forgejo #234: org-owner profile edits
		apiEnv.Orgs = c.ident      // Forgejo #348: owners/detailed is_org marker
		apiEnv.Avatars = c.ident   // Forgejo #376: me() avatar_url projection
		// Forgejo #345: the summary/listing visibility projection behind
		// the Env hook (api renders the spelling without importing the
		// feature, law 8 — the MirrorSummary/CollabCounts shape). One
		// LRU-backed conditional access.json GET per projection (usually
		// a version hit, no body); missing access.json resolves public
		// (the §10 legacy default), so pre-existing repos badge public.
		apiEnv.RepoVisibility = func(ctx context.Context, owner, repo string) (string, bool) {
			vis, ok := c.ident.RepoVisibility(ctx, owner, repo)
			return string(vis), ok
		}
		// Explicit create-repo placeholder (Forgejo #210, R1 B2): the
		// POST /api/v1/repos twin (+ /api-browser/v1 twin) via the
		// server.ExtraRoutes chain + api.RegisterExposed discovery
		// (Feature 10 precedent). No core-table edit (law 8).
		api.RegisterExposed(api.ExposedTemplatesCreate...)
		c.createHandler = &api.CreateHandler{Env: apiEnv}
		if ot, ok := apiEnv.Tasks.(*opsTasks); ok {
			ot.ident = c.ident
		}
	}
	// Wave B issues (docs/features/02): the thread/event/label/
	// milestone surface (Seam 1, both lanes) over the P6 roles owned
	// by identity. Notifications emit through the 02 §10 seam —
	// nil until wireNotifyFanout binds the real emitter below
	// (best-effort synchronous fan-out contract, P8).
	c.issuesSvc, c.issuesHandler = newIssuesService(st, c.ident, cfg)
	// 02 §15: the template discovery reader is the same stock-git subprocess
	// over the serve-synced repo dirs the Threads reader uses; the post-push
	// hook (cmd/spindle/issue_templates.go) is its only caller.
	if reg != nil {
		c.issuesSvc.TemplateGit = issues.NewSubprocessTemplateGit(cfg.Git.Binary, &pullsDirs{reg: reg})
	}
	// Forgejo #319: the summary open-count projection behind the Env hook
	// (api renders CollabCounts without importing the feature, law 8 —
	// the MirrorSummary shape). One shared-index read per summary serves
	// both numerators (issues + pulls read the same index.json, so the
	// hook fans out nowhere); absent index → ok=false → zero counts with
	// the byte-identical ETag. Store errors fail open to absent (display
	// metadata must never fail the summary).
	if apiEnv != nil {
		apiEnv.CollabCounts = func(ctx context.Context, owner, repo string) (api.CollabCounts, bool) {
			oi, op, ver, ok, err := c.issuesSvc.OpenCounts(ctx, owner, repo)
			if err != nil || !ok {
				return api.CollabCounts{}, false
			}
			return api.CollabCounts{OpenIssues: oi, OpenPulls: op, Version: ver}, true
		}
	}
	// Issue #424: the summary fork projection behind the Env hook (api
	// renders ForkSummary without importing the feature, law 8 — the
	// CollabCounts shape). Two exact-key probes (fork.json parent +
	// meta/forks.json count/version, never a LIST); both absent →
	// ok=false → fields omitted with the byte-identical ETag. Store
	// errors fail open to absent (display metadata must never fail the
	// summary — the CollabCounts precedent).
	if apiEnv != nil {
		apiEnv.ForkInfo = func(ctx context.Context, owner, repo string) (api.ForkSummary, bool) {
			return forkSummaryOf(ctx, st, owner, repo)
		}
	}
	// Wave C1 pulls (docs/features/03): PR threads over the shared
	// numbering/thread/index family, pr.json sidecars, the stamped
	// mergeable.json cache, the pull-merge/pull-mergeable/pull-fork
	// tasks, and the pulls event sink. Ref publishes funnel through
	// the WAL publish path (never force); the merge task calls 02's
	// ApplyClosingReferences seam via issuesSvc.
	c.pullsSvc, c.pullsHandler, c.pullsPub = newPullsService(st, c.ident, c.issuesSvc, reg, cfg.Git.Binary)
	// Feature F (docs/features/17_file_edit.md): the browser file editor —
	// one commit through the same WAL funnel a push uses, optional new
	// branch + immediate PR (the shipped pulls opener, reached through the
	// consumer-owned PROpener seam). No new bucket keys, no new task kind.
	c.webeditSvc, c.webeditHandler, c.webeditPub = newWebeditService(st, c.ident, reg, cfg.Git.Binary, c.pullsSvc, cfg)
	// Wave C2 review (docs/features/04): immutable reviews, CAS'd
	// line-anchored threads, the review-requests index, the
	// review_summary render cache, and the required-reviews
	// merge-time half (consulted by the pull-merge task through
	// pulls' ReviewGate seam — the merge logic is NOT forked).
	// Suggest's commit authors ride pulls' HeadAuthors; the
	// push-time half (policy.RequiredReviewsEffect) enforces at
	// receive-pack with no wiring. Registers NO task kinds.
	c.reviewSvc, c.reviewHandler = newReviewService(st, c.ident, c.pullsSvc, reg)
	// Wave 05 checks (docs/features/05): commit statuses (Create-
	// then-CAS per (sha, context)), the CAS'd checks/index.json
	// projection with inline compaction, wct_ CI tokens (Seam 2
	// shape → unprivileged ci:<id> principal, capability checked
	// handler-side), the combined worst-of view, and the
	// require_checks merge-time half (consulted by the pull-merge
	// task through pulls' ChecksGate seam — the merge logic is NOT
	// forked). The push-time half needs no wiring: protect ignores
	// require_checks on the push path by construction.
	c.checksSvc, c.checksHandler = newChecksService(st, c.ident, c.pullsSvc, reg, cfg.Git.Binary)
	// Threads (docs/features/15): workflow discovery from the
	// repository at the trust-selected commit, bucket-native run/job
	// records, the checks mirror the merge gate already consumes, and
	// pass B1's runner protocol (queue + CAS claims, attempt tokens,
	// log frames, secrets, cancel/rerun). The on-push hook is wired in
	// serve.go (server.OnPush); the manual dispatch and the runner
	// protocol ride the Seam 1 chain below.
	c.threadsSvc, c.threadsHandler = newThreadsService(st, c.ident, c.checksSvc, reg, cfg, cfg.Git.Binary)
	// The first-time-contributor gate's evidence (docs/features/15 §5.2): one
	// exact-key read of pulls' merged-author projection. Bound here because
	// pulls exists by now and threads must not import it (law 8).
	if c.threadsSvc != nil && c.pullsSvc != nil {
		c.threadsSvc.Approvals = threadsApprovals{svc: c.pullsSvc, ident: c.ident, cfg: cfg}
	}
	// Runner administration is a host-admin mutation and is audited in the
	// host audit family (docs/PLAN-INSTANCE-RUNNERS.md §4). A nil moderation
	// service (tests) records nothing.
	if c.threadsSvc != nil && c.modSvc != nil {
		mod := c.modSvc
		c.threadsSvc.Audit = func(ctx context.Context, actor, action, target, detail string) error {
			return mod.RecordAdmin(ctx, actor, action, target, detail)
		}
	}
	// Threads triggers (pass A, docs/features/15 §1): the PR lifecycle edges
	// (opened/synchronize/reopened) and the merge task's server-side base
	// publish feed the SAME run-creation path as a client push. Composition
	// owns both sides (law 8); the hooks detach their work, so no PR or merge
	// request waits on a workflow read.
	if c.pullsSvc != nil {
		c.pullsSvc.PullEvents = threadsPullEventOf(c)
		c.pullsSvc.BranchPublished = threadsBranchPublishedOf(c)
	}
	// Issue #505: the summary checks-existence projection behind the Env
	// hook (api renders ChecksSummary without importing the feature, law
	// 8 — the CollabCounts shape). One exact-key probe on the CAS'd
	// hot-window index (absent → ok=false → has_checks:false; the
	// summary always carries a ~k suffix (#513: ~k0 when absent, so a
	// pre-#505 cached summary can never 304-match). Store errors fail open to absent (display
	// metadata must never fail the summary — the CollabCounts
	// precedent).
	if apiEnv != nil {
		apiEnv.ChecksSummary = func(ctx context.Context, owner, repo string) (api.ChecksSummary, bool) {
			if c.checksSvc == nil {
				return api.ChecksSummary{}, false
			}
			has, ver, ok, err := c.checksSvc.HasChecks(ctx, owner, repo)
			if err != nil || !ok {
				return api.ChecksSummary{}, false
			}
			return api.ChecksSummary{HasChecks: has, Version: ver}, true
		}
	}
	// Feature 07 releases (docs/features/07 §§1–3): release headers,
	// asset bytes (two-step upload, static serving), the monotonic
	// latest pointer, and changelog autodraft. Publish fan-out rides
	// internal/notify through the nil-safe seams bound below (P8).
	c.releasesSvc, c.releasesHandler = newReleasesService(st, c.ident, reg, cfg.Git.Binary, cfg.Cache.Dir, int64(cfg.Releases.MaxAssetBytes))
	// Feature 14 packages (docs/features/14): the OCI Distribution
	// registry at /v2/* (a top-level namespace, not a repo lane), over
	// the same store and P6 roles. The bucket prefix is spindle-only.
	c.packagesSvc, c.packagesHandler = newPackagesService(st, c.ident, cfg, apiEnv, cfg.Cache.Dir)
	if c.packagesSvc != nil {
		// The `packages-gc` op (docs/features/14 §6) rides the same task
		// table as the maintainer's sweep, so a manual start joins the
		// running pass instead of racing it.
		if ot, ok := apiEnv.Tasks.(*opsTasks); ok {
			ot.packages = c.packagesSvc
		}
	}
	// Insights (docs/features/18_repo_insights.md): the cached pulse
	// projection over the store + the pinned git scan. The seam is optional
	// (a test build without a registry leaves it nil; the page then says
	// "not available"), and the op rides the shared task table.
	c.insightsSvc = newInsightsService(st, reg)
	if c.insightsSvc != nil {
		if apiEnv != nil {
			if ot, ok := apiEnv.Tasks.(*opsTasks); ok {
				ot.insights = c.insightsSvc
			}
		}
		c.insightsStart = func(ctx context.Context, owner, repo string) error {
			return startInsightsRefresh(reg, c.insightsSvc, owner, repo)
		}
	}
	// Feature T / PLAN-V2 §3 T.1: the repository move runs as the
	// `repo-transfer` task on the core table, and its cross-reference steps
	// reach the families that live in other packages through identity's
	// consumer-owned seams (law 8 — identity must not import pulls/packages;
	// composition owns both sides, exactly like the fork-delete sweep below).
	// The serving-copy seam is the WAL registry itself: Invalidate is the
	// non-destructive twin of the delete path's teardown, VerifyRepo proves the
	// destination's manifest + refs replay before the task says done.
	if c.ident != nil && reg != nil {
		identity.RegisterKind(identity.TransferKind)
		c.ident.Tasks = reg.Tasks()
		c.ident.Serving = reg
		c.ident.Forks = c.pullsSvc
		c.ident.Packages = c.packagesSvc
	}
	// Forgejo #253 tags (create lightweight tag at a commit, server-side
	// via the WAL ref-update path): the P6 write gate + policy create
	// check live in the package; ref creates funnel through the WAL
	// publish path (never force); events ride the existing WAL bridge.
	c.tagsSvc, c.tagsHandler = newTagsService(st, c.ident, reg, cfg.Git.Binary)
	// Repo browsing M2: branch create/delete on the same P6 gate + WAL ref
	// funnel (no new writer). The page reads live in api.RepoBrowse.
	c.branchesSvc = newBranchesService(st, c.ident, reg, cfg.Git.Binary, apiEnvRepo(apiEnv))
	if c.ident != nil {
		_, c.archiveHandler = newArchiveService(c.ident, reg, cfg.Git.Binary)
	}
	// WP5: sync fork — the Seam 5 task kind, the lane surface, and the
	// local pack-sharing fetch (registered once per process).
	c.forksyncSvc, c.forksyncHandler = newForksyncService(st, c.ident, reg, cfg, live)
	// WP3.3/WP4.2 wiring between the two features (composition owns both
	// sides; neither package imports the other, law 8): a pending-tag draft
	// publishes its ref through tags' own create path (policy + WAL CAS), and
	// tag deletion refuses when a release backs the tag.
	if c.releasesSvc != nil && c.tagsSvc != nil {
		c.releasesSvc.CreateTag = func(ctx context.Context, owner, repo, name, sha string, p auth.Principal) error {
			_, err := c.tagsSvc.CreateTag(ctx, owner, repo, p, tags.CreateInput{Name: name, SHA: sha})
			return err
		}
		c.tagsSvc.SetReleaseExists(func(ctx context.Context, owner, repo, tag string) (bool, error) {
			_, _, err := c.releasesSvc.GetRelease(ctx, owner, repo, auth.Principal{Name: "system", Admin: true}, tag)
			if err != nil {
				if errors.Is(err, releases.ErrNotFound) {
					return false, nil
				}
				return false, err
			}
			return true, nil
		})
	}
	// Feature 07 social (docs/features/07 §§4–6): stars, watcher
	// reads, counters, starred lists. Watch mutation stays in
	// internal/notify (06 §6); the fork counter binds onto pulls
	// below (07 §6).
	c.socialSvc, c.socialHandler = newSocialService(st, c.ident)
	// Feature 06 notifications (docs/features/06): the fan-out
	// layer — notification objects, per-user indexes, the activity
	// log, per-user SSE, repo webhooks, retention. The REAL
	// Emitter/Streamer implementations bind onto the nil-safe
	// seams here, so issues/pulls/review/checks mutations fan out
	// synchronously (P8) from this boot forward.
	c.notifySvc, c.notifyHandler = newNotifyService(st, c.ident)
	wireNotifyFanout(c.notifySvc, c.issuesSvc, c.pullsSvc, c.reviewSvc, c.checksSvc)
	wireReleasesFanout(c.releasesSvc, c.notifySvc)
	wireSocialForks(c.socialSvc, c.pullsSvc)
	// Feature M1: one moderation service satisfies every consumer-owned
	// block gate (issues/pulls/review writes, identity invitations, notify's
	// mention filter). Nil-safe per consumer.
	wireModeration(c.modSvc, c.ident, c.issuesSvc, c.pullsSvc, c.reviewSvc, c.notifySvc)
	// M2: the moderation SSE frames ride the one repo collab stream once the
	// bus exists (the service is built before notify).
	wireModerationStream(c.modSvc, c.notifySvc)
	// M3: report_filed notifies the repo admins (reason moderation) through
	// the same notify service; the callback carries no reported content.
	wireModerationReports(c.modSvc, c.ident, c.notifySvc)
	// Forgejo #522: the per-repo feature-flag write guards — Star
	// refuses new stars, SetWatch refuses new watches, and StartFork
	// refuses new forks when the repo's flag is off (DELETE/unstar/
	// unwatch always work). One shared reader over the WAL settings
	// doc (manifest-inline — the same read the summary fold uses, so
	// the guards and the tab gating can never disagree); unreadable
	// settings fail open to all-enabled at each guard (display
	// metadata must never break a write on a transient read — the
	// CollabCounts precedent). apiEnv.Repo is read lazily per call so
	// startup ordering (view bound after buildCollab) is safe; a nil
	// view (tests without the surface) fails open too.
	if apiEnv != nil {
		featuresOf := func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool) {
			if apiEnv.Repo == nil {
				return config.AllFeatures(), false
			}
			doc, err := apiEnv.Repo.Settings(ctx, git.RepoId{Owner: owner, Name: repo})
			if err != nil {
				return config.AllFeatures(), false
			}
			return config.FeaturesOf([]byte(doc.TOML)), true
		}
		c.socialSvc.Features = featuresOf
		c.notifySvc.Features = featuresOf
		c.pullsSvc.Features = featuresOf
	}
	// Issue #457: the child-delete sweep — deleting a fork child unlists
	// it from the parent-side fork index and decrements the parent's
	// social counter. Wired here (composition owns both sides; core never
	// imports upward, law 8): the serving RepoRegistry captures the fork
	// parent pre-delete and calls back post-delete (serve.go Delete).
	if apiEnv != nil {
		if rr, ok := apiEnv.Repos.(*repoRegistry); ok {
			rr.onChildDelete = forkDeleteSweep(c.pullsSvc)
		}
	}
	// Feature 10 repository import (docs/features/10): the URL-import
	// surface (Seam 1, both lanes, top-level twins) over the P6 roles
	// owned by identity; the repo-import task runs on the core wal
	// task table (Seam 5) via the Service (B6 opsTasks-style wiring).
	c.importSvc, c.importHandler = newImportService(st, c.ident, reg, cfg, live)
	// Feature 11 mirrors (docs/features/11_mirror.md): the pull-only
	// sidecar + scheduled sync surface (Seam 1, repo lanes + the
	// create-from-URL top-level twin) over the same store/registry;
	// the mirror-sync task runs on the core wal task table (Seam 5).
	c.mirrorSvc, c.mirrorHandler = newMirrorService(st, reg, cfg, apiEnv, live)
	// Forgejo #623 push mirrors (docs/features/13_push_mirror.md): the
	// config + secret sidecars + on-push/scheduled sync surface (Seam 1,
	// repo lanes only — post-hoc config, never at create) over the same
	// store/registry; the mirror-push-sync task runs on the core wal
	// task table (Seam 5), fully independent of pull mirror-sync.
	c.pushMirrorSvc, c.pushMirrorHandler = newPushMirrorService(st, reg, cfg, apiEnv, live)
	// Forgejo #346: the mirror create-from-URL twin enforces the same
	// creation owner-admission rule (self, member org, or host admin)
	// before creating anything.
	if c.mirrorHandler != nil && c.ident != nil {
		c.mirrorHandler.CheckCreateOwner = c.ident.CheckCreateOwner
	}
	// Feature 08 §4: access.json CAS commits publish the "access"
	// collab frame (nil-safe seam on the identity service; the doc
	// stays the backfill truth).
	if c.ident != nil {
		c.ident.Stream = func(ctx context.Context, repo string) {
			c.notifySvc.PublishFrame(notify.RepoFrame{Name: "access", Repo: repo})
		}
		// Forgejo #363: org membership/team/invite transitions append
		// to the notify org-hook log (nil-safe seam on the identity
		// service; the members/teams/invitation docs stay the backfill
		// truth). Synchronous post-commit emission per P8; EmitOrgEvent
		// reserves, appends, and wakes — it never blocks on delivery.
		c.ident.OrgEvent = func(ctx context.Context, org, action, actor, title string) {
			c.notifySvc.EmitOrgEvent(ctx, org, action, actor, title)
		}
		// PLAN-ADMIN §8.6: identity's administrative writes (access grants,
		// org/team membership, ownership transfer) record into the ONE host
		// audit family. Identity cannot import moderation (moderation imports
		// identity for the role gates), so the writer is this seam.
		if c.modSvc != nil {
			c.ident.Audit = func(ctx context.Context, actor, action, target, detail string) error {
				return c.modSvc.RecordAdmin(ctx, actor, action, target, detail)
			}
		}
		// Forgejo #364: repo births land in the same org log (the
		// "repo-create" leg of the org activity surface). The wal
		// registry is the single birth choke (API creates, push
		// auto-creates, imports all commit the manifest there), and it
		// cannot import notify (law 8 — core never imports upward), so
		// the observer is injected here (orgBirthObserver, tested
		// without buildCollab — task-kind registrations panic on a
		// second buildCollab in one test binary).
		reg.OnCreate = orgBirthObserver(c.ident, c.notifySvc)
	}
	// P2 of docs/features/20_search.md: the code-search service over the
	// grep primitive (git.Layer's ScanPool), the WAL serving copies and
	// identity's read gate. P3 binds the routes; nil-safe without reg/ident
	// (a test composition answers 503, never an ungated search).
	c.searchSvc = newSearchService(cfg, reg, apiEnv, c.ident, c.issuesSvc)
	if apiEnv != nil {
		// P3: the frozen API lanes (07_api.md §9.13) read the same service.
		// The value is a *search.Service (never a typed nil): the routes
		// answer its classified failures, and an unwired composition answers
		// the documented 503 rather than searching with no gate.
		apiEnv.Search = c.searchSvc
	}
	return c
}

// orgBirthObserver returns the wal registry birth observer: org-owned
// births emit repo_created, user-owned births stay silent. One GetOrg
// probe per birth decides (cold path, once per repo lifetime); a
// missing/unreadable org reads as "not an org" — a birth is never failed
// by its audit event. The spelling pre-check is free (local — an org
// name can never carry uppercase or dots, unlike user principals), so
// non-org spellings skip the probe entirely; warm pushes never birth.
func orgBirthObserver(ident *identity.Service, notifier *notify.Service) func(ctx context.Context, id string) {
	return func(ctx context.Context, id string) {
		rid, err := git.ParseRepoId(id)
		if err != nil {
			return
		}
		owner := strings.ToLower(rid.Owner)
		if !identity.ValidOrg(owner) {
			return
		}
		got, err := ident.GetOrg(ctx, owner)
		if err != nil || got == nil {
			return
		}
		notifier.EmitOrgEvent(ctx, owner, notify.OrgActionRepoCreated, "", "repo "+id+" created")
	}
}

// chainCollab fronts the server mux with every collab surface (Seam 1,
// one block per package); authentication resolves through the server
// chain (Seam 2). Nil handlers are skipped (tests may wire a subset).
func chainCollab(srv *server.Server, c *collabWiring) {
	if c == nil {
		return
	}
	if c.identHandler != nil {
		// Chain the identity surface in front of the core api mux (Seam 1);
		// authentication resolves through the server chain (Seam 2),
		// including the §8.6 broker-forwarding rule — the forwarded
		// principal replaces the broker's, never the broker itself.
		c.identHandler.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
			return srv.Auth().AuthenticateForwarded(r, srv.Config())
		}
		srv.ChainExtra(c.identHandler)
	}
	if c.modHandler != nil {
		// The block twins (/api/v1/users/{principal}/blocks) are top-level
		// and independent of every repo lane; chain them beside identity.
		chainModeration(srv, c.modHandler)
	}
	if c.issuesHandler != nil {
		// Chain the issues surface in front of the core api mux (Seam 1);
		// authentication resolves through the server chain (Seam 2),
		// including the §8.6 broker-forwarding rule — the forwarded
		// principal replaces the broker's, never the broker itself.
		c.issuesHandler.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
			return srv.Auth().AuthenticateForwarded(r, srv.Config())
		}
		srv.ChainExtra(c.issuesHandler)
		// The §12 byte route lives outside the api lanes (repo_extra.go
		// seam, same as release asset bytes); authentication resolves
		// in-handler from the same chain.
		srv.ChainRepo(c.issuesHandler)
	}
	if c.pullsHandler != nil {
		chainPulls(srv, c.pullsHandler)
	}
	if c.webeditHandler != nil {
		chainWebedit(srv, c.webeditHandler)
	}
	if c.reviewHandler != nil {
		chainReview(srv, c.reviewHandler)
	}
	if c.checksHandler != nil {
		chainChecks(srv, c.checksHandler)
	}
	if c.threadsHandler != nil {
		chainThreads(srv, c.threadsHandler)
	}
	if c.releasesHandler != nil {
		chainReleases(srv, c.releasesHandler)
	}
	if c.archiveHandler != nil {
		chainArchive(srv, c.archiveHandler)
	}
	if c.forksyncHandler != nil {
		chainForksync(srv, c.forksyncHandler)
	}
	if c.tagsHandler != nil {
		chainTags(srv, c.tagsHandler)
	}
	if c.socialHandler != nil {
		chainSocial(srv, c.socialHandler)
	}
	if c.notifyHandler != nil {
		chainNotify(srv, c.notifyHandler)
	}
	if c.importHandler != nil {
		chainImport(srv, c.importHandler)
	}
	if c.mirrorHandler != nil {
		chainMirror(srv, c.mirrorHandler)
	}
	if c.pushMirrorHandler != nil {
		chainPushMirror(srv, c.pushMirrorHandler)
	}
	if c.packagesHandler != nil {
		chainPackages(srv, c.packagesHandler)
	}
	if c.createHandler != nil {
		// #210 create twin: authentication resolves through the server
		// chain (Seam 2) — the handler gates via Env.gate on the
		// injected principal, same as the PUT lane root.
		srv.ChainExtra(c.createHandler)
	}
}
