// Package threads implements the server half of docs/features/15_threads.md:
// workflow discovery from the repository at a trust-selected commit,
// bucket-native run/job records carrying a versioned execution Spec, the checks
// mirror the existing merge gate consumes, and pass B1's runner protocol — a
// bucket queue with CAS claims and leases, per-attempt HMAC tokens, idempotent
// log frames with an SSE attach, write-only repository secrets sealed with
// AES-256-GCM, and cancel/rerun. Container execution (the runner's own job) is
// pass B2 and is not here.
//
// Triggers: push (client and the merge task's server-side publish),
// pull_request (opened/synchronize/reopened; never close) and
// workflow_dispatch. The pulls service emits the PR edges through its
// PullEvents seam; composition binds every trigger to one CreateRun path.
// Parsing is per file, so a broken workflow disables only itself and its error
// is visible in the UI/API (15 §2.1).
//
// Seams (docs/go/14_extensibility.md):
//   - Seam 1 (routes): Handler.Handle fronts the core mux on both lanes for the
//     repo surface, and claims the top-level /api/v1/threads paths for the
//     runner protocol (runners are infrastructure, not repo-scoped users).
//   - Seam 4 (event sinks): the pulls service's PullEvents/BranchPublished
//     hooks feed run creation from the PR lifecycle and the merge publish.
//   - Seam 5 (task kinds): KindThreadsGC is registered by composition; its
//     body (log/record retention) is T3 because log frames do not exist yet.
//
// Bucket layout (15 §2/§2.2; no state outside the object store):
//
//	repos/<o>/<r>/threads/runs/<run_id>.json                  run header (commit, event, workflow, state)
//	repos/<o>/<r>/threads/runs/<run_id>/jobs/<job_id>.json    job record (labels, spec, attempt, state)
//	repos/<o>/<r>/threads/runs/<r>/jobs/<j>/log/<a>-<seq>     immutable log frames
//	repos/<o>/<r>/threads/queue/<seq>.json                    queue entry (CAS-claimed)
//	repos/<o>/<r>/threads/queue/head.json                     dispatch head (bounded pending window)
//	repos/<o>/<r>/threads/runners/<id>.json                   runner registry (labels, last seen)
//	repos/<o>/<r>/threads/secrets/<name>.json                 sealed secret envelopes
//	users/<principal>/tokens/threads/<id>.json                runner credentials (hash only)
//
// Bucket compatibility with the Rust implementation is not involved (this
// family is new); the key layout is the spec's.
//
// ### Concurrency
//
// Hazard: two run-creation paths (the push hook and a manual dispatch) racing
// on a run record. Avoidance: run ids are minted per creation (nanos + random)
// and every record is Create-only; a lost Create is retried with a fresh id.
// Job state transitions are the canonical CAS retry loop (13_concurrency.md
// §3) — the CAS is the lock, no repo lock is taken. Hazard: two writers
// updating the same run's fold. Avoidance: the fold itself is a CAS over the
// run header; a loser re-reads the jobs and re-applies, and the last writer
// converges to the same worst-of state. Hazard: a push hook blocking the push
// response. Avoidance: CreateRun runs in the hook's own goroutine (composition
// spawns it), never on the request path; nothing here takes syncMu/packMu/rw
// (13_concurrency.md §2 rule 4). Reads are conditional GETs; the run list is
// one prefix LIST behind a page request, never a hot path (a run get is by id).
package threads

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// dateTimeFmt is the RFC 3339 UTC wire timestamp (07 §2).
const dateTimeFmt = time.RFC3339

// ListLimits bound the run page (P5: paginated, never a full window).
const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// RoleService is the narrow P6 surface this package consumes (the same shape
// internal/checks uses; *identity.Service satisfies it in composition).
type RoleService interface {
	Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc)
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// CheckSink mirrors one job state into the checks surface (05 §2/§6). The
// production adapter is composition's binding of *checks.Service.RecordStatus
// — this package never imports internal/checks (law 8), and the merge gate
// sees the record exactly like an external CI report.
type CheckSink interface {
	// Report records one status for (repo, sha, context). state is the
	// checks enum (pending|success|failure). startedAt/completedAt are
	// RFC 3339 UTC or nil.
	Report(ctx context.Context, repo, sha, checkContext, state, description string, startedAt, completedAt *string) error
}

// Service is the Threads store client: discovery, run creation, run/job reads
// and the job→check mirror. Construct with New; Git may be nil in pure-record
// tests (discovery then fails closed).
type Service struct {
	Store store.ObjectStore
	Git   GitReader
	Roles RoleService
	// Checks receives every job-state mirror; nil = no checks integration
	// (record tests).
	Checks CheckSink
	// Now is the clock (tests pin it).
	Now func() time.Time
	// keys is the protocol keyring (attempt-token HMAC + secret sealing).
	keys *Keyring
	// Approvals is the first-time-contributor evidence source (pulls'
	// merged-author projection, one exact-key GET). Nil holds every gated run
	// (fail closed) rather than letting an unknown contributor execute.
	Approvals ApprovalPolicy
	// ApproveFirstTimeForks enables the gate (threads.approve_first_time_forks;
	// default true, set by composition from config).
	ApproveFirstTimeForks bool
	// DefaultPermissions is the job-token default when a workflow declares no
	// `permissions:` (threads.default_permissions; "" = read).
	DefaultPermissions string
	// Retention is the terminal-run window (threads.retention; default 30d).
	// The maintainer's threads-gc retention half deletes runs past it (gc.go).
	Retention time.Duration
}

// New builds a Service over st. The protocol keys default to a process-local
// keyring (attempt tokens live exactly as long as the jobs they authorise);
// composition replaces it with the configured one via SetKeyring.
func New(st store.ObjectStore, roles RoleService) *Service {
	s := &Service{Store: st, Roles: roles, Now: time.Now}
	if k, err := NewKeyring("", ""); err == nil {
		s.keys = k
	}
	return s
}

// SetKeyring installs the configured protocol keys and returns the keyring (so
// composition can log the ephemeral warning once).
func (s *Service) SetKeyring(k *Keyring) *Keyring {
	if k != nil {
		s.keys = k
	}
	return s.keys
}

// Keyring returns the installed keyring (nil before New).
func (s *Service) Keyring() *Keyring { return s.keys }

// nowUTC is the clock, UTC.
func (s *Service) nowUTC() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

// --- key helpers (bucket-relative; 15 §2) ---------------------------------

// RunsPrefix returns repos/<o>/<r>/threads/runs/ (the run LIST root).
func RunsPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/threads/runs/"
}

// RunKey returns repos/<o>/<r>/threads/runs/<id>.json.
func RunKey(owner, repo, runID string) string {
	return RunsPrefix(owner, repo) + runID + ".json"
}

// JobsPrefix returns repos/<o>/<r>/threads/runs/<id>/jobs/.
func JobsPrefix(owner, repo, runID string) string {
	return RunsPrefix(owner, repo) + runID + "/jobs/"
}

// JobKey returns repos/<o>/<r>/threads/runs/<id>/jobs/<jid>.json.
func JobKey(owner, repo, runID, jobID string) string {
	return JobsPrefix(owner, repo, runID) + jobID + ".json"
}

// splitRepo parses "owner/name" (the push hook's input shape).
func splitRepo(repo string) (owner, name string, err error) {
	id, perr := git.ParseRepoId(strings.TrimSpace(repo))
	if perr != nil {
		return "", "", fmt.Errorf("%w: %v", ErrInvalid, perr)
	}
	return id.Owner, id.Name, nil
}

// repoID renders "owner/name".
func repoID(owner, repo string) string { return owner + "/" + repo }

// --- roles (P6, the sibling feature packages' ladder) ---------------------

// roleRank orders role names read < triage < write < maintain < admin.
func roleRank(role string) int {
	switch identity.Role(strings.ToLower(role)) {
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

// roleOf resolves the actor's repo role ("" when none).
func (s *Service) roleOf(ctx context.Context, owner, repo string, p auth.Principal) string {
	if s.Roles == nil {
		if p.Admin {
			return string(identity.RoleAdmin)
		}
		if p.Write {
			return string(identity.RoleWrite)
		}
		if p.Anonymous {
			return ""
		}
		return string(identity.RoleRead)
	}
	role, _ := s.Roles.Resolve(ctx, owner, repo, p)
	return string(role)
}

// requireRole enforces a minimum repo role (anonymous failures 401,
// authenticated-but-insufficient 403).
func (s *Service) requireRole(ctx context.Context, owner, repo string, p auth.Principal, want string) error {
	if p.Admin {
		return nil
	}
	if roleRank(s.roleOf(ctx, owner, repo, p)) >= roleRank(want) {
		return nil
	}
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	return fmt.Errorf("%w: need %s", ErrForbidden, want)
}

// requireRead enforces the read gate (identity require_read when wired:
// public visibility or role ≥ read).
func (s *Service) requireRead(ctx context.Context, owner, repo string, p auth.Principal) error {
	if p.Admin || p.Write {
		return nil
	}
	if s.Roles == nil {
		if p.Anonymous {
			return fmt.Errorf("%w", ErrUnauthorized)
		}
		return nil
	}
	if aerr := s.Roles.CheckRead(ctx, owner, repo, p); aerr != nil {
		switch aerr.Kind {
		case auth.ErrForbidden:
			return fmt.Errorf("%w: %s", ErrForbidden, aerr.Why)
		case auth.ErrUnavailable:
			return fmt.Errorf("identity unavailable: %s", aerr.Why)
		default:
			return fmt.Errorf("%w: %s", ErrUnauthorized, aerr.Why)
		}
	}
	return nil
}

// --- CAS helpers (13 §3 canonical loops; the checks/releases shape) -------

// casUpdate is the canonical CAS loop: read, apply, Update(version), retry on
// 412 with a re-read, bounded attempts, then ErrConflict.
func (s *Service) casUpdate(ctx context.Context, key string, attempts int, f func(cur []byte, ver store.Version) ([]byte, bool, error)) (store.ObjectMeta, error) {
	if attempts <= 0 {
		attempts = 5
	}
	for i := 0; i < attempts; i++ {
		cur, meta, err := store.GetBytes(ctx, s.Store, key, store.GetOptions{})
		if err != nil {
			if store.IsNotFound(err) {
				cur, meta = nil, store.ObjectMeta{}
			} else {
				return store.ObjectMeta{}, err
			}
		}
		next, write, ferr := f(cur, meta.Version)
		if ferr != nil {
			return store.ObjectMeta{}, ferr
		}
		if !write {
			return meta, nil
		}
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/json"}
		if meta.Version == "" {
			opts.Mode = store.PutCreate
		}
		m, perr := store.PutBytes(ctx, s.Store, key, next, opts)
		if perr == nil {
			return m, nil
		}
		if !store.IsPreconditionFailed(perr) {
			return store.ObjectMeta{}, perr
		}
	}
	return store.ObjectMeta{}, fmt.Errorf("%w: %s changed concurrently; reload and retry", ErrConflict, key)
}

// getJSON reads one object; (nil, "", nil) when absent.
func (s *Service) getJSON(ctx context.Context, key string) ([]byte, store.Version, error) {
	raw, meta, err := store.GetBytes(ctx, s.Store, key, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	return raw, meta.Version, nil
}

// putCreate writes an immutable object; a 412 means it already exists.
func (s *Service) putCreate(ctx context.Context, key string, body []byte) error {
	_, err := store.PutBytes(ctx, s.Store, key, body, store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	return err
}

// statusFor maps a service error onto its HTTP status (07 §2).
func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrConflict), errors.Is(err, errSuperseded):
		return http.StatusConflict
	case errors.Is(err, ErrUnprocessable):
		return http.StatusUnprocessableEntity
	case errors.Is(err, ErrUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
