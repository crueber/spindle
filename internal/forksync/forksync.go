// Package forksync implements the "Sync fork" action (docs/PLAN-V1.md WP5):
// fetch the fork's upstream and fast-forward the fork's default branch when
// it is a strict ancestor; otherwise report divergence and link to a pull
// request. It is a Seam 5 task kind ((repo, kind=sync-fork) single-flight)
// with progress packets (law 7), a bucket-only outcome sidecar (law 4), and
// the shared SSRF gate (internal/egress rules) before any fetch.
//
// Bucket layout (one CAS'd sidecar, no local state):
//
//	repos/<o>/<r>/meta/sync-fork.json   last attempt/outcome + upstream sha
//
// The fork provenance (fork.json) is owned by internal/pulls §7; the refs,
// fetch, ancestry and publish mechanics are narrow seams so the service is
// testable without git or the network and composition wires the real ones
// (WAL handle + `git fetch`/publish). Core packages never import this one.
//
// ### Concurrency
//
// Hazard: two "Sync fork" clicks (or a click racing a scheduled pass) both
// fetch and publish. Avoidance: wal.TaskTable's (repo, kind) single-flight —
// a second start joins the running task and observes its outcome; the ref
// publish is a CAS old-oid → new-oid update, so even a cross-instance race
// cannot force-move a moved branch (the loser reports 409, never rewrites).
// The sidecar is a CAS loop; a failed write is narrated, never fatal. No
// lock is held across a fetch or a store call.
package forksync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// KindSyncFork is the Seam 5 task kind (registered once from composition).
const KindSyncFork = "sync-fork"

// Outcomes recorded in the sidecar and the task summary.
const (
	ResultCurrent     = "current"      // already at upstream
	ResultFastForward = "fast-forward" // moved the fork branch
	ResultDiverged    = "diverged"     // no safe move; open a PR
	ResultFailed      = "failed"       // fetch/guard/git failure
)

// Sentinel errors mapped to plain-text statuses.
var (
	ErrNotFound     = errors.New("not found")
	ErrInvalid      = errors.New("invalid request")
	ErrUnauthorized = errors.New("authentication required")
	ErrForbidden    = errors.New("forbidden")
	ErrConflict     = errors.New("conflict")
	ErrUnavailable  = errors.New("temporarily unavailable")
)

// RoleService is the narrow P6 surface (satisfied by *identity.Service).
type RoleService interface {
	Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc)
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// Refs resolves the default branch and ancestry (production: a WAL handle
// synced to serve level + merge-base).
type Refs interface {
	DefaultBranch(ctx context.Context, repo string) (branch, sha string, err error)
	IsAncestor(ctx context.Context, repo, ancestor, descendant string) (bool, error)
}

// Fetcher fetches upstream objects into the fork's serving copy and returns
// the fetched upstream tip (production: mirror-style clone/fetch + ingest).
type Fetcher interface {
	// Fetch makes the upstream branch's objects and tip available in the
	// fork (production: local pack-set sharing — the parent lives on this
	// instance — then ancestry runs in the fork's serving copy).
	Fetch(ctx context.Context, repo, upstreamRepo, branch string, progress func(label string, done uint64)) (string, error)
}

// Publisher publishes a fast-forward on the fork (CAS old → new).
type Publisher interface {
	FastForward(ctx context.Context, repo, branch, oldSHA, newSHA string) error
}

// ProgressFunc narrates a phase (done=0 means "started", otherwise units done).
type ProgressFunc func(label string, done uint64)

// TaskLogger is the narration surface the task body uses (wal.Task shape).
type TaskLogger interface {
	Notice(string)
	Progress(label string, done, total uint64, unit string)
}

// TaskInfo is the task record projection the HTTP surface returns.
type TaskInfo struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Repo     string `json:"repo"`
	Started  string `json:"started"`
	Finished string `json:"finished,omitempty"`
	OK       *bool  `json:"ok,omitempty"`
	Summary  string `json:"summary,omitempty"`
	Result   string `json:"result,omitempty"`
}

// Tasks is the (repo, kind) single-flight task runner seam.
type Tasks interface {
	Start(ctx context.Context, repo, kind string, params map[string]string, fn func(ctx context.Context, t TaskLogger) error) (string, error)
	Get(id string) (TaskInfo, bool)
}

// Service is the sync-fork surface.
type Service struct {
	Store     store.ObjectStore
	Roles     RoleService
	Refs      Refs
	Fetcher   Fetcher
	Publisher Publisher
	Tasks     Tasks
	// Guard screens the upstream URL (SSRF). Composition wires
	// internal/repoimport's gate (same [import] rules as mirror/import);
	// nil means deny-all (fail closed — a sync must never be the one path
	// without an SSRF check).
	Guard func(rawURL string) error
	// UpstreamURL renders the upstream clone URL for a parent "owner/name".
	// Composition builds it from server.public_url (loopback is allowed by
	// the egress rule: the parent is this instance).
	UpstreamURL func(parent string) (string, error)
	Now         func() time.Time
}

// Sidecar is repos/<o>/<r>/meta/sync-fork.json — the last outcome.
type Sidecar struct {
	Parent      string `json:"parent"`
	Branch      string `json:"branch"`
	UpstreamSHA string `json:"upstream_sha,omitempty"`
	ForkSHA     string `json:"fork_sha,omitempty"`
	Result      string `json:"result"`
	Message     string `json:"message,omitempty"`
	PRURL       string `json:"pr_url,omitempty"`
	AttemptedAt string `json:"attempted_at"`
	UpdatedAt   string `json:"updated_at"`
	Version     int    `json:"version"`
}

// SidecarKey returns the outcome key.
func SidecarKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/meta/sync-fork.json"
}

func (s *Service) nowUTC() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// --- gates ------------------------------------------------------------------

func (s *Service) requireWrite(ctx context.Context, owner, repo string, p auth.Principal) error {
	if p.Admin {
		return nil
	}
	if s.Roles == nil {
		if p.Anonymous {
			return ErrUnauthorized
		}
		if p.Write {
			return nil
		}
		return fmt.Errorf("%w: write access required", ErrForbidden)
	}
	role, _ := s.Roles.Resolve(ctx, owner, repo, p)
	if rank(role) >= rank(identity.RoleWrite) {
		return nil
	}
	if p.Anonymous {
		return ErrUnauthorized
	}
	return fmt.Errorf("%w: write access required", ErrForbidden)
}

func rank(r identity.Role) int {
	switch identity.Role(strings.ToLower(string(r))) {
	case identity.RoleRead:
		return 1
	case identity.RoleTriage:
		return 2
	case identity.RoleWrite:
		return 3
	case identity.RoleMaintain:
		return 4
	case identity.RoleAdmin:
		return 5
	}
	return 0
}

// --- fork provenance ---------------------------------------------------------

// ForkDoc is the minimal fork.json projection (§7: pulls owns the shape).
type ForkDoc struct {
	Parent string `json:"parent"`
}

// readFork returns the fork parent ("" when the repo is not a fork).
func (s *Service) readFork(ctx context.Context, owner, repo string) (string, error) {
	raw, _, err := store.GetBytes(ctx, s.Store, "repos/"+owner+"/"+repo+"/fork.json", store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	var doc ForkDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		return "", fmt.Errorf("%w: fork.json: %v", ErrInvalid, jerr)
	}
	return strings.TrimSpace(doc.Parent), nil
}

// Start validates and starts the sync task (or joins a running one),
// returning its id. The HTTP route answers 202 with this id.
func (s *Service) Start(ctx context.Context, owner, repo string, p auth.Principal) (string, error) {
	if err := s.requireWrite(ctx, owner, repo, p); err != nil {
		return "", err
	}
	parent, err := s.readFork(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	if parent == "" {
		return "", fmt.Errorf("%w: this repository is not a fork", ErrInvalid)
	}
	if s.Tasks == nil {
		return "", fmt.Errorf("%w: task runner not wired", ErrUnavailable)
	}
	repoName := owner + "/" + repo
	return s.Tasks.Start(ctx, repoName, KindSyncFork,
		map[string]string{"upstream": parent, "principal": p.Name},
		func(tctx context.Context, task TaskLogger) error {
			return s.Run(tctx, repoName, parent, p, task)
		})
}

// Run is the task body: read both tips, fetch, decide, publish, record. Every
// phase narrates a progress packet; divergence is a SUCCESSFUL run that
// publishes nothing (the UI links to a PR).
func (s *Service) Run(ctx context.Context, repo, parent string, p auth.Principal, task TaskLogger) error {
	fail := func(res Sidecar, err error) error {
		res.Result = ResultFailed
		res.Message = scrub(err.Error())
		res.Parent = parent
		s.record(ctx, repo, res, task)
		task.Notice("sync fork failed: " + res.Message)
		return err
	}
	if s.Refs == nil || s.Fetcher == nil || s.Publisher == nil {
		return fail(Sidecar{}, fmt.Errorf("%w: sync seams not wired", ErrUnavailable))
	}
	now := s.nowUTC().Format(time.RFC3339)
	res := Sidecar{Parent: parent, AttemptedAt: now}

	task.Progress("upstream refs", 0, 2, "steps")
	_, upSHA, err := s.Refs.DefaultBranch(ctx, parent)
	if err != nil {
		return fail(res, fmt.Errorf("%w: upstream default branch: %v", ErrUnavailable, err))
	}
	branch, forkSHA, err := s.Refs.DefaultBranch(ctx, repo)
	if err != nil {
		return fail(res, fmt.Errorf("%w: fork default branch: %v", ErrUnavailable, err))
	}
	res.Branch, res.UpstreamSHA, res.ForkSHA = branch, upSHA, forkSHA

	upURL, uerr := s.UpstreamURLFor(parent)
	if uerr != nil {
		return fail(res, uerr)
	}
	task.Progress("upstream refs", 1, 2, "steps")

	// SSRF gate: the same rules mirror/import apply, before any fetch.
	if s.Guard == nil {
		return fail(res, fmt.Errorf("%w: SSRF gate not wired", ErrUnavailable))
	}
	if gerr := s.Guard(upURL); gerr != nil {
		res.Message = gerr.Error()
		res.Result = ResultFailed
		s.record(ctx, repo, res, task)
		task.Notice("sync fork refused: " + gerr.Error())
		return fmt.Errorf("%w: upstream refused: %v", ErrForbidden, gerr)
	}

	// Tops equal: already current, and no fetch is needed (a poll must not
	// clone the upstream to learn nothing changed).
	if res.UpstreamSHA == res.ForkSHA {
		res.Result = ResultCurrent
		res.Message = "Already up to date with " + parent + "."
		s.record(ctx, repo, res, task)
		task.Progress("done", 1, 1, "steps")
		task.Notice(res.Message)
		return nil
	}

	// Fetch: the upstream tip may be newer than what the fork's serving copy
	// holds (a fork shares packs only up to its fork point).
	task.Progress("fetch", 0, 1, "objects")
	fetched, ferr := s.Fetcher.Fetch(ctx, repo, parent, branch, func(label string, done uint64) {
		task.Progress(label, done, 0, "")
	})
	if ferr != nil {
		return fail(res, fmt.Errorf("%w: fetch: %v", ErrUnavailable, scrub(ferr.Error())))
	}
	if fetched != "" {
		res.UpstreamSHA = fetched
	}
	task.Progress("fetch", 1, 1, "objects")

	if res.UpstreamSHA == res.ForkSHA {
		res.Result = ResultCurrent
		res.Message = "Already up to date with " + parent + "."
		s.record(ctx, repo, res, task)
		task.Progress("done", 1, 1, "steps")
		task.Notice(res.Message)
		return nil
	}

	task.Progress("ancestry", 0, 1, "checks")
	strictAncestor, aerr := s.Refs.IsAncestor(ctx, repo, res.ForkSHA, res.UpstreamSHA)
	if aerr != nil {
		return fail(res, fmt.Errorf("%w: ancestry: %v", ErrUnavailable, aerr))
	}
	task.Progress("ancestry", 1, 1, "checks")
	if !strictAncestor || res.ForkSHA == res.UpstreamSHA {
		// Same tip handled above; not-an-ancestor means the fork has commits
		// of its own on the branch: report divergence + the PR link. Nothing
		// is published (law: never rewrite a branch).
		res.Result = ResultDiverged
		res.Message = fmt.Sprintf("%s has commits that are not in %s; open a pull request to reconcile.", branch, parent)
		res.PRURL = prURL(parent, branch, repo, branch)
		s.record(ctx, repo, res, task)
		task.Progress("done", 1, 1, "steps")
		task.Notice(res.Message + " " + res.PRURL)
		return nil
	}

	task.Progress("publish", 0, 1, "refs")
	if perr := s.Publisher.FastForward(ctx, repo, branch, res.ForkSHA, res.UpstreamSHA); perr != nil {
		return fail(res, fmt.Errorf("%w: publish: %v", ErrConflict, perr))
	}
	task.Progress("publish", 1, 1, "refs")
	res.Result = ResultFastForward
	res.Message = fmt.Sprintf("Fast-forwarded %s to %s.", branch, short(res.UpstreamSHA))
	s.record(ctx, repo, res, task)
	task.Progress("done", 1, 1, "steps")
	task.Notice(res.Message)
	return nil
}

// UpstreamURLFor renders the upstream URL through the composition seam.
func (s *Service) UpstreamURLFor(parent string) (string, error) {
	if s.UpstreamURL == nil {
		return "", fmt.Errorf("%w: upstream resolver not wired", ErrUnavailable)
	}
	u, err := s.UpstreamURL(parent)
	if err != nil {
		return "", fmt.Errorf("%w: upstream URL: %v", ErrUnavailable, err)
	}
	parsed, perr := url.Parse(u)
	if perr != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("%w: upstream URL %q is not absolute", ErrUnavailable, u)
	}
	return u, nil
}

// record CAS-writes the outcome sidecar, best-effort (a lost sidecar write
// never fails the run — the task record already narrated it).
func (s *Service) record(ctx context.Context, repo string, res Sidecar, task TaskLogger) {
	if s.Store == nil {
		return
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return
	}
	key := SidecarKey(owner, name)
	res.UpdatedAt = s.nowUTC().Format(time.RFC3339)
	for attempt := 0; attempt < 4; attempt++ {
		cur, meta, err := store.GetBytes(ctx, s.Store, key, store.GetOptions{})
		if err != nil && !store.IsNotFound(err) {
			task.Notice("sync fork: outcome not recorded: " + scrub(err.Error()))
			return
		}
		res.Version = 1
		if cur != nil {
			var prev Sidecar
			if json.Unmarshal(cur, &prev) == nil {
				res.Version = prev.Version + 1
			}
		}
		body, _ := json.Marshal(res)
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/json"}
		if meta.Version == "" {
			opts.Mode = store.PutCreate
		}
		if _, perr := store.PutBytes(ctx, s.Store, key, body, opts); perr == nil {
			return
		} else if !store.IsPreconditionFailed(perr) {
			task.Notice("sync fork: outcome not recorded: " + scrub(perr.Error()))
			return
		}
	}
}

// LastOutcome reads the sidecar (nil when never run).
func (s *Service) LastOutcome(ctx context.Context, owner, repo string) (*Sidecar, error) {
	raw, _, err := store.GetBytes(ctx, s.Store, SidecarKey(owner, repo), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc Sidecar
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		return nil, fmt.Errorf("%w: sync-fork.json: %v", ErrCorrupt, jerr)
	}
	return &doc, nil
}

// ErrCorrupt marks an unreadable outcome document.
var ErrCorrupt = errors.New("corrupt object")

// prURL builds the "open a PR" link in the PARENT repo (fork PRs are opened
// against the base). Both branch names are query-escaped by the caller's
// template layer; here they are plain components.
func prURL(parent, baseBranch, headRepo, headBranch string) string {
	return "/" + parent + "/pulls/new?base=" + url.QueryEscape(baseBranch) +
		"&head=" + url.QueryEscape(headRepo+":"+headBranch)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// scrub bounds and strips control bytes from narrator text (fetch errors can
// carry URL fragments; never tokens — the fetch carries none).
func scrub(s string) string {
	if len(s) > 512 {
		s = s[:512]
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
}

// --- Seam 5 kind registry (the maintain.RegisterKind contract) ---------------

var (
	kindsMu sync.Mutex
	kinds   = map[string]bool{}
)

// RegisterKind records the task kind; a duplicate panics (one registration
// per process, from composition).
func RegisterKind(name string) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	if kinds[name] {
		panic(fmt.Sprintf("forksync: duplicate task kind %q", name))
	}
	kinds[name] = true
}

// ResetKindsForTest clears the registry (test-only, -count=N support).
func ResetKindsForTest() {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	kinds = map[string]bool{}
}
