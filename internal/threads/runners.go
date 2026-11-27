package threads

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Instance runner registry and the runner protocol (docs/PLAN-INSTANCE-RUNNERS.md,
// 15 §3):
//
//	threads/runners/<id>.json              registry: labels, capacity, state, last seen, current job
//	threads/runners/tokens/<id>.json       single-use registration token (hash only, 15-minute TTL)
//	threads/runners/credentials/<id>.json  long-lived runner credential (hash only, revocable)
//	threads/queue/index.json               global dispatch hint (queueindex.go)
//
// A runner is INSTANCE-SCOPED infrastructure: only a host administrator mints a
// registration token, and the resulting credential names the runner id, never a
// repository. What the runner may be offered is the ADMIN-SET tenancy policy the
// mint carries (RunnerPolicy): "all repositories", or a bounded list of
// owner/name / owner/* patterns. The policy is copied to the registry record at
// registration and can never be declared by the runner itself — register and
// declare take labels, capacity and `allow_fork_runs` only, and both reject an
// unknown field. A record with no policy (one written before tenancy existed)
// allows NO repository until an administrator edits it: SetRunnerPolicy is the
// audited write path for a registered runner, so a fleet runner is not thrown
// away (and its current job not killed) just to widen or narrow tenancy.
//
// Tenancy is not isolation: a runner with a docker socket is root-equivalent on
// its host, so the allowlist narrows which repositories' code reaches a host —
// it does not contain that code (docs/threads.md §8.5, §8.7).

// Runner states.
const (
	RunnerPending = "pending"
	RunnerActive  = "active"
	RunnerRevoked = "revoked"
)

// RegistrationTTL bounds an unused registration token (15 minutes: long enough
// for an operator to paste it into a runner, short enough to be useless when
// leaked).
const RegistrationTTL = 15 * time.Minute

// Runner capacity bounds (declare). Capacity 1 is the isolation-preserving
// default; a larger value is an explicit operator choice on ONE daemon.
const (
	runnerCapacityMin = 1
	// MaxRunnerCapacity is the declared-capacity ceiling. It is exported so the
	// runner process bounds its own worker count by the same number the server
	// records (a runner asking for 999 must not start 999 fetch loops).
	MaxRunnerCapacity = 64
	runnerCapacityMax = MaxRunnerCapacity
)

// Audit action verbs the instance runner admin surface records in the host
// audit family (never invented for a read).
const (
	AuditRunnerTokenMint = "runner_token_mint"
	AuditRunnerRevoke    = "runner_revoke"
	AuditRunnerPolicy    = "runner_policy"
)

// MaxRunnerPolicyRepos bounds one runner's repository allowlist. A larger list
// is a mistake (a fleet of a few dozen runners covers far more than fifty
// repositories); the bound keeps the registry record and the claim filter small.
const MaxRunnerPolicyRepos = 50

// runnerPolicySegment is one owner or repository-name segment of a policy
// pattern: the repository naming rule, lowercased, bounded.
var runnerPolicySegment = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)

// RunnerPolicyInput is the ADMIN-set tenancy policy of one runner, as the mint
// (and the JSON API's mint body) carries it:
//
//	{"allow_all": true}                      every repository
//	{"repos": ["acme/mono", "acme/*"]}       an explicit allowlist
//
// It is never part of the runner protocol: RegisterInput and DeclareInput do not
// carry it, so a runner cannot widen its own tenancy.
type RunnerPolicyInput struct {
	AllowAll bool     `json:"allow_all"`
	Repos    []string `json:"repos,omitempty"`
}

// RunnerPolicy is the normalised policy recorded on the token and registry
// records (lowercased, trimmed, de-duplicated, sorted). An empty value allows
// NOTHING: that is the fail-closed default for any record written before
// tenancy existed (docs/threads.md §8.7).
type RunnerPolicy struct {
	AllowAll bool     `json:"allow_all,omitempty"`
	Repos    []string `json:"repos,omitempty"`
}

// ValidateRunnerPolicy normalises and validates an admin-supplied policy. A
// policy that names nothing is an error: a token that can never be offered a job
// is an operator mistake, not a quiet no-op.
func ValidateRunnerPolicy(in RunnerPolicyInput) (RunnerPolicy, error) {
	pol := RunnerPolicy{AllowAll: in.AllowAll}
	seen := map[string]bool{}
	for _, raw := range in.Repos {
		p := strings.ToLower(strings.TrimSpace(raw))
		if p == "" {
			continue
		}
		owner, name, ok := strings.Cut(p, "/")
		if !ok || !runnerPolicySegment.MatchString(owner) {
			return RunnerPolicy{}, fmt.Errorf("%w: repository pattern %q must be owner/name or owner/*", ErrInvalid, raw)
		}
		if name != "*" && !runnerPolicySegment.MatchString(name) {
			return RunnerPolicy{}, fmt.Errorf("%w: repository pattern %q must be owner/name or owner/*", ErrInvalid, raw)
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		pol.Repos = append(pol.Repos, p)
	}
	sort.Strings(pol.Repos)
	if len(pol.Repos) > MaxRunnerPolicyRepos {
		return RunnerPolicy{}, fmt.Errorf("%w: at most %d repository patterns (got %d)", ErrInvalid, MaxRunnerPolicyRepos, len(pol.Repos))
	}
	if !pol.AllowAll && len(pol.Repos) == 0 {
		return RunnerPolicy{}, fmt.Errorf("%w: choose all repositories or at least one owner/name pattern — a runner with no repository would never be offered a job", ErrInvalid)
	}
	return pol, nil
}

// Describe is the human and audit reading of a policy: "all repositories", the
// pattern list, or "none" for a record that predates tenancy.
func (p RunnerPolicy) Describe() string {
	switch {
	case p.AllowAll:
		return "all repositories"
	case len(p.Repos) == 0:
		return "none"
	default:
		return strings.Join(p.Repos, ",")
	}
}

// AllowsRepo reports whether the policy permits one repository ("owner/name",
// matched case-insensitively): allow_all permits everything, an exact pattern
// matches one repository, and `owner/*` matches every repository of that owner.
// Anything else — including an empty policy — permits nothing.
func (p RunnerPolicy) AllowsRepo(repo string) bool {
	if p.AllowAll {
		return true
	}
	repo = strings.ToLower(strings.TrimSpace(repo))
	owner, _, ok := strings.Cut(repo, "/")
	if !ok || owner == "" {
		return false
	}
	for _, pat := range p.Repos {
		pat = strings.ToLower(strings.TrimSpace(pat))
		if pat == repo {
			return true
		}
		if strings.TrimSuffix(pat, "/*") != pat && strings.TrimSuffix(pat, "/*") == owner {
			return true
		}
	}
	return false
}

// policy is the registry record's own policy value.
func (d *RunnerDoc) policy() RunnerPolicy {
	if d == nil {
		return RunnerPolicy{}
	}
	return RunnerPolicy{AllowAll: d.AllowAll, Repos: d.Repos}
}

// AllowsRepo is the claim filter's question: may this runner be offered one
// repository? It reads the admin-set policy only; the runner has no say.
func (d *RunnerDoc) AllowsRepo(repo string) bool {
	return d.policy().AllowsRepo(repo)
}

// Audit action verbs for deployment environments (docs/PLAN-THREADS-ENVIRONMENTS.md
// §8): every environment mutation and every deployment decision is recorded in
// the host audit family. The target names the environment; no secret value ever
// reaches the detail.
const (
	AuditEnvironmentCreate       = "environment_create"
	AuditEnvironmentUpdate       = "environment_update"
	AuditEnvironmentDelete       = "environment_delete"
	AuditEnvironmentSecretSet    = "environment_secret_set"
	AuditEnvironmentSecretDelete = "environment_secret_delete"
	AuditEnvironmentApprove      = "environment_approve"
	AuditEnvironmentDeny         = "environment_deny"
)

// RunnerJobRef names the job a runner currently leases (display state).
type RunnerJobRef struct {
	Repo  string `json:"repo"`
	RunID string `json:"run_id"`
	JobID string `json:"job_id"`
}

// RunnerDoc is one instance runner's registry record (labels + capacity +
// liveness; never a secret).
type RunnerDoc struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Labels   []string `json:"labels"`
	Capacity int      `json:"capacity"`
	State    string   `json:"state"`
	// LabelImages is the Forgejo-style label→image mapping the runner declared
	// ("ubuntu-latest:docker://node:lts", PAR-171). A job whose runs-on names a
	// mapped label and whose workflow named no container.image runs in that
	// image; the declared labels remain the match set.
	LabelImages     map[string]string `json:"label_images,omitempty"`
	DeclaredVersion string            `json:"declared_version,omitempty"`
	RegisteredBy    string            `json:"registered_by,omitempty"`
	CreatedAt       string            `json:"created_at"`
	RegisteredAt    *string           `json:"registered_at,omitempty"`
	LastSeenAt      *string           `json:"last_seen_at,omitempty"`
	RevokedAt       *string           `json:"revoked_at,omitempty"`
	CurrentJob      *RunnerJobRef     `json:"current_job,omitempty"`
	LeaseUntil      *string           `json:"lease_until,omitempty"`
	// AllowAll / Repos are the ADMIN-SET repository tenancy policy, copied from
	// the registration token at mint and re-applied at registration. The claim
	// filter reads them and nothing on the runner side can write them
	// (docs/threads.md §8.7). Both absent means "no repository": a record
	// written before tenancy existed is offered nothing rather than everything.
	AllowAll bool     `json:"allow_all,omitempty"`
	Repos    []string `json:"repos,omitempty"`
	// AllowForkRuns is the operator's opt-in to executing FORK pull-request
	// code on this runner (default false). A fork run is offered only to a
	// runner that declares it; the per-repository claim CAS remains the
	// authority, and a run with no eligible runner simply stays queued
	// (docs/threads.md §8.7).
	AllowForkRuns bool `json:"allow_fork_runs,omitempty"`
	Version       int  `json:"version"`
}

// runnerTokenDoc is threads/runners/tokens/<id>.json: a single-use registration
// token. Only its hash is stored; the admin's policy travels with it, so the
// registration copies the policy the mint authorized rather than trusting the
// registry record alone.
type runnerTokenDoc struct {
	ID         string   `json:"id"`
	Name       string   `json:"name,omitempty"`
	TokenHash  string   `json:"token_hash"`
	CreatedBy  string   `json:"created_by"`
	CreatedAt  string   `json:"created_at"`
	ExpiresAt  string   `json:"expires_at"`
	ConsumedAt *string  `json:"consumed_at,omitempty"`
	RevokedAt  *string  `json:"revoked_at,omitempty"`
	AllowAll   bool     `json:"allow_all,omitempty"`
	Repos      []string `json:"repos,omitempty"`
	Version    int      `json:"version"`
}

// RunnerCredential is threads/runners/credentials/<id>.json: hash only,
// revocable, and it names a runner id — never a repository.
type RunnerCredential struct {
	ID        string  `json:"id"`
	TokenHash string  `json:"token_hash"`
	CreatedAt string  `json:"created_at"`
	RevokedAt *string `json:"revoked_at,omitempty"`
	Version   int     `json:"version"`
}

// --- host-admin surface ------------------------------------------------------

// RegistrationToken is the mint answer: the secret travels here exactly once.
type RegistrationToken struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	// Policy echoes the tenancy the admin set, so the mint answer states what
	// the token will grant (no secret, no repository data beyond the patterns).
	Policy RunnerPolicy `json:"policy"`
}

// requireHostAdmin is the instance runner gate: administration is exactly
// "signed in ∧ p.Admin" (AGENTS law 9). A repo admin is NOT a host admin and
// gets 403; an anonymous caller gets 401.
func requireHostAdmin(p auth.Principal) error {
	if p.Anonymous || strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("%w: sign in as a host administrator", ErrUnauthorized)
	}
	if !p.Admin {
		return fmt.Errorf("%w: runner registration is host-administrator only", ErrForbidden)
	}
	return nil
}

// MintInput is the admin mint request: a display name and the repository
// tenancy policy. BOTH are the administrator's decision; the runner part is
// only the resulting token.
type MintInput struct {
	Name string `json:"name"`
	RunnerPolicyInput
}

// MintRegistrationToken mints a single-use registration token (host admin). The
// registry record is created here in state "pending" (with the admin's policy),
// so the runner appears in /admin/runners before it registers; the token record
// travels once, stores only its hash, and carries the same policy so
// registration applies what the mint authorized.
func (s *Service) MintRegistrationToken(ctx context.Context, p auth.Principal, in MintInput) (*RegistrationToken, error) {
	if err := requireHostAdmin(p); err != nil {
		return nil, err
	}
	// Validate before writing anything: a malformed policy must not leave a
	// half-minted token, and a policy that allows nothing is refused (it would
	// be a runner that can never be offered a job).
	pol, perr := ValidateRunnerPolicy(in.RunnerPolicyInput)
	if perr != nil {
		return nil, perr
	}
	principal := strings.ToLower(strings.TrimSpace(p.Name))
	now := s.nowUTC()
	expires := now.Add(RegistrationTTL)
	id, err := mintID()
	if err != nil {
		return nil, fmt.Errorf("%w: mint: %v", ErrUnavailable, err)
	}
	token, hash, err := mintScopedToken(regTokenPrefix, principal, id)
	if err != nil {
		return nil, fmt.Errorf("%w: mint: %v", ErrUnavailable, err)
	}
	display := truncate(strings.TrimSpace(in.Name), 100)
	doc := &RunnerDoc{
		ID: id, Name: display, State: RunnerPending, Labels: []string{}, Capacity: runnerCapacityMin,
		AllowAll: pol.AllowAll, Repos: pol.Repos,
		RegisteredBy: principal, CreatedAt: now.Format(dateTimeFmt), Version: 1,
	}
	tok := &runnerTokenDoc{
		ID: id, Name: display, TokenHash: hash, CreatedBy: principal,
		AllowAll: pol.AllowAll, Repos: pol.Repos,
		CreatedAt: now.Format(dateTimeFmt), ExpiresAt: expires.Format(dateTimeFmt), Version: 1,
	}
	// Token first: a registry entry without a token is unusable and would block
	// the id; the reverse leaves an orphan token the sweep removes.
	if perr := s.putCreate(ctx, RunnerTokenKey(id), mustJSON(tok)); perr != nil {
		return nil, mintConflict(perr)
	}
	if perr := s.putCreate(ctx, RunnerKey(id), mustJSON(doc)); perr != nil {
		_ = s.Store.Delete(ctx, RunnerTokenKey(id), "")
		return nil, mintConflict(perr)
	}
	s.auditAdmin(ctx, p.Name, AuditRunnerTokenMint, "runner:"+id,
		fmt.Sprintf("name=%s;expires=%s;policy=%s", display, tok.ExpiresAt, pol.Describe()))
	return &RegistrationToken{ID: id, Token: token, ExpiresAt: tok.ExpiresAt, Policy: pol}, nil
}

// mintConflict maps a duplicate id onto a retryable conflict.
func mintConflict(err error) error {
	if store.IsPreconditionFailed(err) {
		return fmt.Errorf("%w: runner id collided; retry", ErrUnavailable)
	}
	return err
}

// RunnerView is one row of GET /api/v1/admin/runners (never a secret).
type RunnerView struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Labels          []string          `json:"labels"`
	LabelImages     map[string]string `json:"label_images,omitempty"`
	Capacity        int               `json:"capacity"`
	State           string            `json:"state"`
	DeclaredVersion string            `json:"declared_version,omitempty"`
	RegisteredBy    string            `json:"registered_by,omitempty"`
	CreatedAt       string            `json:"created_at"`
	RegisteredAt    *string           `json:"registered_at,omitempty"`
	LastSeenAt      *string           `json:"last_seen_at,omitempty"`
	RevokedAt       *string           `json:"revoked_at,omitempty"`
	CurrentJob      *RunnerJobRef     `json:"current_job,omitempty"`
	AllowForkRuns   bool              `json:"allow_fork_runs"`
	// AllowAll / Repos are the admin-set tenancy policy. Both absent is the
	// honest reading "no repository" (a record from before tenancy), never
	// "all repositories".
	AllowAll bool     `json:"allow_all"`
	Repos    []string `json:"repos,omitempty"`
}

// ListRunners lists the instance fleet (host admin; one bounded prefix LIST
// over a human-scale family — never a request hot path).
func (s *Service) ListRunners(ctx context.Context, p auth.Principal) ([]RunnerView, error) {
	if err := requireHostAdmin(p); err != nil {
		return nil, err
	}
	var keys []string
	if lerr := s.Store.List(ctx, RunnersPrefix(), "", func(m store.ObjectMeta) error {
		rest := strings.TrimPrefix(m.Key, RunnersPrefix())
		if strings.HasSuffix(rest, ".json") && !strings.Contains(rest, "/") {
			keys = append(keys, m.Key)
		}
		return nil
	}); lerr != nil {
		return nil, lerr
	}
	sort.Strings(keys)
	out := []RunnerView{}
	for _, key := range keys {
		raw, _, gerr := s.getJSON(ctx, key)
		if gerr != nil {
			return nil, gerr
		}
		if raw == nil {
			continue
		}
		var doc RunnerDoc
		if jerr := json.Unmarshal(raw, &doc); jerr != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, key, jerr)
		}
		out = append(out, RunnerView{
			ID: doc.ID, Name: doc.Name, Labels: nonNilStr(doc.Labels), LabelImages: doc.LabelImages, Capacity: doc.Capacity,
			State: doc.State, DeclaredVersion: doc.DeclaredVersion, RegisteredBy: doc.RegisteredBy,
			CreatedAt: doc.CreatedAt, RegisteredAt: doc.RegisteredAt, LastSeenAt: doc.LastSeenAt,
			RevokedAt: doc.RevokedAt, CurrentJob: doc.CurrentJob, AllowForkRuns: doc.AllowForkRuns,
			AllowAll: doc.AllowAll, Repos: nonNilStr(doc.Repos),
		})
	}
	return out, nil
}

// RevokeRunner revokes one instance runner (host admin): the registry state
// flips, the credential and any unused registration token are stamped revoked,
// so the next bearer is a real 401 and the runner is never offered a job again.
func (s *Service) RevokeRunner(ctx context.Context, id string, p auth.Principal) error {
	if err := requireHostAdmin(p); err != nil {
		return err
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if !tokenIDRe.MatchString(id) {
		return fmt.Errorf("%w: unknown runner %q", ErrNotFound, id)
	}
	key := RunnerKey(id)
	raw, _, gerr := s.getJSON(ctx, key)
	if gerr != nil {
		return gerr
	}
	if raw == nil {
		return fmt.Errorf("%w: unknown runner %q", ErrNotFound, id)
	}
	var doc RunnerDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		return fmt.Errorf("%w: runner %s: %v", ErrCorrupt, id, jerr)
	}
	now := s.nowUTC().Format(dateTimeFmt)
	_, err := s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown runner %q", ErrNotFound, id)
		}
		var d RunnerDoc
		if jerr := json.Unmarshal(cur, &d); jerr != nil {
			return nil, false, fmt.Errorf("%w: runner %s: %v", ErrCorrupt, id, jerr)
		}
		if d.RevokedAt != nil {
			return nil, false, nil // idempotent
		}
		d.State = RunnerRevoked
		d.RevokedAt = &now
		d.CurrentJob = nil
		d.LeaseUntil = nil
		d.Version++
		return mustJSON(&d), true, nil
	})
	if err != nil {
		return err
	}
	_ = s.revokeRunnerCredential(ctx, id, now)
	_ = s.revokeRunnerTokenDoc(ctx, id, now)
	s.auditAdmin(ctx, p.Name, AuditRunnerRevoke, "runner:"+id, "state=revoked")
	return nil
}

// SetRunnerPolicy replaces one registered runner's ADMIN-set repository policy
// (host-admin write; the page and the JSON lane share this call). It is the
// supported way to change tenancy for a runner that is already in the fleet —
// including a record written before tenancy existed, which is offered nothing —
// without revoking the credential and minting a new registration token (the
// hostile shape for a working fleet, and one that cannot be done while the
// runner is mid-job).
//
// The write is one CAS over the registry record that rewrites ONLY
// allow_all/repos (every other field, the current job and its lease included, is
// preserved) and is a no-op when the policy is unchanged. The policy is
// validated by the same ValidateRunnerPolicy the mint uses, so the page and the
// JSON lane share one rule and one normalisation. A revoked runner is refused
// (its credential is dead; re-arming it would resurrect a bearer that was
// explicitly killed).
//
// The claim filter reads the policy from the registry record at claim time
// (authorizeRunner loads the record on every protocol request, and claimGlobal
// consults the record it was handed), so the NEXT fetch honours the edit; a job
// already running is untouched (its attempt token and lease are unchanged). A
// fetch already in flight keeps the record it loaded for at most its 30-second
// long poll — the same staleness window revoke has.
func (s *Service) SetRunnerPolicy(ctx context.Context, id string, in RunnerPolicy, actor string) (RunnerPolicy, error) {
	// The caller is the host administrator the route already authenticated; an
	// empty actor means the gate was skipped, so fail closed rather than write
	// an unattributable authorization change.
	if strings.TrimSpace(actor) == "" {
		return RunnerPolicy{}, fmt.Errorf("%w: sign in as a host administrator", ErrUnauthorized)
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if !tokenIDRe.MatchString(id) {
		return RunnerPolicy{}, fmt.Errorf("%w: unknown runner %q", ErrNotFound, id)
	}
	pol, perr := ValidateRunnerPolicy(RunnerPolicyInput{AllowAll: in.AllowAll, Repos: in.Repos})
	if perr != nil {
		return RunnerPolicy{}, perr
	}
	key := RunnerKey(id)
	applied := pol
	_, err := s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown runner %q", ErrNotFound, id)
		}
		var d RunnerDoc
		if jerr := json.Unmarshal(cur, &d); jerr != nil {
			return nil, false, fmt.Errorf("%w: runner %s: %v", ErrCorrupt, id, jerr)
		}
		if d.State == RunnerRevoked || d.RevokedAt != nil {
			return nil, false, fmt.Errorf("%w: runner %s is revoked; its credential is dead and its policy cannot be changed", ErrConflict, id)
		}
		// A re-submission of the recorded policy writes nothing (no version
		// bump, no lease/current-job churn); the audit entry below still records
		// that the administrator declared it.
		if d.AllowAll == pol.AllowAll && sameRepos(d.Repos, pol.Repos) {
			return nil, false, nil
		}
		d.AllowAll = pol.AllowAll
		d.Repos = pol.Repos
		d.Version++
		applied = d.policy()
		return mustJSON(&d), true, nil
	})
	if err != nil {
		return RunnerPolicy{}, err
	}
	s.auditAdmin(ctx, strings.TrimSpace(actor), AuditRunnerPolicy, "runner:"+id, "policy="+applied.Describe())
	return applied, nil
}

// sameRepos reports whether two normalised pattern lists are equal (both are
// trimmed/sorted by ValidateRunnerPolicy, so a byte comparison is a set
// comparison here).
func sameRepos(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// revokeRunnerCredential CAS-stamps one credential record revoked (best effort:
// the registry state already denies the bearer, since every protocol call loads
// it).
func (s *Service) revokeRunnerCredential(ctx context.Context, id, now string) error {
	_, err := s.casUpdate(ctx, RunnerCredentialKey(id), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var cred RunnerCredential
		if jerr := json.Unmarshal(cur, &cred); jerr != nil {
			return nil, false, fmt.Errorf("%w: credential %s: %v", ErrCorrupt, id, jerr)
		}
		if cred.RevokedAt != nil {
			return nil, false, nil
		}
		cred.RevokedAt = &now
		cred.Version++
		return mustJSON(&cred), true, nil
	})
	return err
}

// revokeRunnerTokenDoc stamps an unused registration token revoked.
func (s *Service) revokeRunnerTokenDoc(ctx context.Context, id, now string) error {
	_, err := s.casUpdate(ctx, RunnerTokenKey(id), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var tok runnerTokenDoc
		if jerr := json.Unmarshal(cur, &tok); jerr != nil {
			return nil, false, fmt.Errorf("%w: registration token %s: %v", ErrCorrupt, id, jerr)
		}
		if tok.RevokedAt != nil || tok.ConsumedAt != nil {
			return nil, false, nil
		}
		tok.RevokedAt = &now
		tok.Version++
		return mustJSON(&tok), true, nil
	})
	return err
}

// auditAdmin records one host-administrative runner mutation. A failure is
// logged and never reverts the committed change (the audit-after-commit rule).
func (s *Service) auditAdmin(ctx context.Context, actor, action, target, detail string) {
	if s.Audit == nil {
		return
	}
	if err := s.Audit(ctx, actor, action, target, detail); err != nil {
		slog.Warn("threads: runner audit write failed", "actor", actor, "action", action, "target", target, "err", err)
	}
}

// --- protocol: register / declare / fetch ------------------------------------

// RegisterInput is POST /api/v1/threads/runners/register. It cannot carry the
// repository policy: `allow_all`/`repos` are the admin's, and the strict decoder
// rejects them here (a runner cannot widen its own tenancy).
type RegisterInput struct {
	Token    string   `json:"token"`
	Name     string   `json:"name"`
	Labels   []string `json:"labels"`
	Capacity int      `json:"capacity"`
	// AllowForkRuns records the runner's opt-in to executing fork pull-request
	// code (default false: a fork run is never offered to it).
	AllowForkRuns bool `json:"allow_fork_runs"`
}

// RegisterResult is the register answer: the long-lived runner token, shown
// once (the server stores only its hash). It carries no repository: a runner is
// instance-scoped.
type RegisterResult struct {
	RunnerID string   `json:"runner_id"`
	Token    string   `json:"token"`
	Labels   []string `json:"labels"`
	// LabelImages echoes the parsed label→image map (PAR-171): the runner states
	// which image each mapped label serves, and the server honours it.
	LabelImages map[string]string `json:"label_images,omitempty"`
	// AllowForkRuns echoes the recorded opt-in so the runner can state its
	// effective policy in startup output.
	AllowForkRuns bool `json:"allow_fork_runs"`
	// Policy echoes the admin-set tenancy it must operate under (read-only:
	// the runner states it, it cannot change it).
	Policy RunnerPolicy `json:"policy"`
}

// Register consumes a registration token exactly once.
func (s *Service) Register(ctx context.Context, in RegisterInput) (*RegisterResult, error) {
	_, id, secret, ok := tokenParts(in.Token, regTokenPrefix)
	if !ok {
		return nil, fmt.Errorf("%w: not a registration token", ErrUnauthorized)
	}
	tok, _, err := s.loadRunnerToken(ctx, id)
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, fmt.Errorf("%w: unknown registration token", ErrUnauthorized)
	}
	if tok.RevokedAt != nil || tok.ConsumedAt != nil {
		return nil, fmt.Errorf("%w: registration token already used or revoked", ErrUnauthorized)
	}
	if exp, perr := time.Parse(dateTimeFmt, tok.ExpiresAt); perr != nil || s.nowUTC().After(exp) {
		return nil, fmt.Errorf("%w: registration token expired", ErrUnauthorized)
	}
	if !verifySecret(secret, tok.TokenHash) {
		return nil, fmt.Errorf("%w: bad registration token", ErrUnauthorized)
	}
	runner, _, gerr := s.loadRunner(ctx, id)
	if gerr != nil {
		return nil, gerr
	}
	if runner == nil || runner.State == RunnerRevoked {
		return nil, fmt.Errorf("%w: runner is revoked", ErrUnauthorized)
	}
	parsed, lerr := ParseRunnerLabels(in.Labels)
	if lerr != nil {
		return nil, lerr
	}
	labels := parsed.Labels
	principal := strings.TrimSpace(runner.RegisteredBy)
	if principal == "" {
		principal = "runner"
	}
	runnerToken, runnerHash, merr := mintScopedToken(runnerTokenPrefix, principal, id)
	if merr != nil {
		return nil, fmt.Errorf("%w: mint: %v", ErrUnavailable, merr)
	}
	now := s.nowUTC().Format(dateTimeFmt)
	// Credential first: the token's single consumption is the CAS below, and a
	// lost credential create (another registration won the id) is a 401.
	cred := &RunnerCredential{ID: id, TokenHash: runnerHash, CreatedAt: now, Version: 1}
	if perr := s.putCreate(ctx, RunnerCredentialKey(id), mustJSON(cred)); perr != nil {
		if store.IsPreconditionFailed(perr) {
			return nil, fmt.Errorf("%w: registration token already used", ErrUnauthorized)
		}
		return nil, perr
	}
	_, cerr := s.casUpdate(ctx, RunnerTokenKey(id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown registration token", ErrUnauthorized)
		}
		var t runnerTokenDoc
		if jerr := json.Unmarshal(cur, &t); jerr != nil {
			return nil, false, fmt.Errorf("%w: registration token %s: %v", ErrCorrupt, id, jerr)
		}
		if t.ConsumedAt != nil || t.RevokedAt != nil {
			return nil, false, fmt.Errorf("%w: registration token already used or revoked", ErrUnauthorized)
		}
		t.ConsumedAt = &now
		t.Version++
		return mustJSON(&t), true, nil
	})
	if cerr != nil {
		_ = s.Store.Delete(ctx, RunnerCredentialKey(id), "")
		return nil, cerr
	}
	_, derr := s.casUpdate(ctx, RunnerKey(id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: runner vanished", ErrNotFound)
		}
		var d RunnerDoc
		if jerr := json.Unmarshal(cur, &d); jerr != nil {
			return nil, false, fmt.Errorf("%w: runner %s: %v", ErrCorrupt, id, jerr)
		}
		if d.State == RunnerRevoked {
			return nil, false, fmt.Errorf("%w: runner is revoked", ErrUnauthorized)
		}
		d.State = RunnerActive
		if n := strings.TrimSpace(in.Name); n != "" {
			d.Name = truncate(n, 100)
		}
		d.Labels = labels
		d.LabelImages = parsed.Images
		if c := clampCapacity(in.Capacity); c > 0 {
			d.Capacity = c
		}
		d.AllowForkRuns = in.AllowForkRuns
		// The policy is the admin mint's, re-applied from the token record so a
		// drift between the pending registry entry and the token cannot widen
		// tenancy. Nothing the runner sent is consulted.
		d.AllowAll = tok.AllowAll
		d.Repos = tok.Repos
		d.RegisteredAt = &now
		d.LastSeenAt = &now
		d.Version++
		return mustJSON(&d), true, nil
	})
	if derr != nil {
		return nil, derr
	}
	pol := RunnerPolicy{AllowAll: tok.AllowAll, Repos: tok.Repos}
	return &RegisterResult{
		RunnerID: id, Token: runnerToken, Labels: labels, LabelImages: parsed.Images,
		AllowForkRuns: in.AllowForkRuns, Policy: pol,
	}, nil
}

// clampCapacity bounds a declared capacity; 0 means "not supplied".
func clampCapacity(n int) int {
	if n <= 0 {
		return 0
	}
	if n < runnerCapacityMin {
		return runnerCapacityMin
	}
	if n > runnerCapacityMax {
		return runnerCapacityMax
	}
	return n
}

// DeclareInput is POST /api/v1/threads/runners/declare. Like register it cannot
// carry the repository policy: declare states what the runner is, never what it
// may be offered.
type DeclareInput struct {
	Version  string   `json:"version"`
	Labels   []string `json:"labels"`
	Capacity int      `json:"capacity"`
	// AllowForkRuns is the runner's current opt-in; declare is idempotent, so a
	// restart without the flag turns it off (fail closed).
	AllowForkRuns bool `json:"allow_fork_runs"`
}

// DeclareView is the declare answer.
type DeclareView struct {
	RunnerID    string            `json:"runner_id"`
	State       string            `json:"state"`
	Labels      []string          `json:"labels"`
	LabelImages map[string]string `json:"label_images,omitempty"`
	Capacity    int               `json:"capacity"`
	// LeaseSeconds is the claim lease the server will grant.
	LeaseSeconds int `json:"lease_seconds"`
	// AllowForkRuns echoes what the registry now records.
	AllowForkRuns bool `json:"allow_fork_runs"`
	// Policy echoes the admin-set repository tenancy the claim filter enforces.
	// Declare never changes it (an administrator edits it at /admin/runners).
	Policy RunnerPolicy `json:"policy"`
}

// Declare records a runner's version, label set and capacity (it is also its
// liveness record: fetching is the heartbeat, declare states what the runner
// is).
func (s *Service) Declare(ctx context.Context, runner *runnerAuth, in DeclareInput) (*DeclareView, error) {
	parsed, lerr := ParseRunnerLabels(in.Labels)
	if lerr != nil {
		return nil, lerr
	}
	labels := parsed.Labels
	now := s.nowUTC().Format(dateTimeFmt)
	key := RunnerKey(runner.Doc.ID)
	_, err := s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown runner", ErrNotFound)
		}
		var d RunnerDoc
		if jerr := json.Unmarshal(cur, &d); jerr != nil {
			return nil, false, fmt.Errorf("%w: runner %s: %v", ErrCorrupt, runner.Doc.ID, jerr)
		}
		if d.State == RunnerRevoked || d.RevokedAt != nil {
			return nil, false, fmt.Errorf("%w: runner revoked", ErrUnauthorized)
		}
		if len(labels) > 0 {
			d.Labels = labels
			d.LabelImages = parsed.Images
		}
		if v := truncate(strings.TrimSpace(in.Version), 64); v != "" {
			d.DeclaredVersion = v
		}
		if c := clampCapacity(in.Capacity); c > 0 {
			d.Capacity = c
		}
		d.AllowForkRuns = in.AllowForkRuns
		d.LastSeenAt = &now
		d.Version++
		runner.Doc = &d
		return mustJSON(&d), true, nil
	})
	if err != nil {
		return nil, err
	}
	return &DeclareView{
		RunnerID: runner.Doc.ID, State: runner.Doc.State, Labels: nonNilStr(runner.Doc.Labels),
		LabelImages: runner.Doc.LabelImages,
		Capacity:    runner.Doc.Capacity, LeaseSeconds: int(s.jobLease().Seconds()),
		AllowForkRuns: runner.Doc.AllowForkRuns, Policy: runner.Doc.policy(),
	}, nil
}

// FetchResult is the fetch answer: a self-contained job payload (the runner
// needs no further reads to start — 12_runner D2).
type FetchResult struct {
	Repo    string `json:"repo"`
	RunID   string `json:"run_id"`
	JobID   string `json:"job_id"`
	JobRef  string `json:"job_ref"`
	Attempt int    `json:"attempt"`
	Commit  string `json:"commit"`
	Event   string `json:"event"`
	Branch  string `json:"branch"`
	// Ref is the run's full triggering ref ("refs/heads/main",
	// "refs/tags/v1.2.3"); Branch is empty for a tag run, so this is what the
	// runner's GITHUB_REF/GITHUB_REF_NAME are derived from (docs/threads.md
	// §3.5). A pull request reports its base branch regardless (the PR head ref
	// is not a job's identity).
	Ref      string `json:"ref,omitempty"`
	Workflow string `json:"workflow"`
	Trusted  bool   `json:"trusted"`
	Context  string `json:"context"`
	// Actor is the principal that triggered the run (the run's `actor`): the
	// runner exposes it as GITHUB_ACTOR and as `${{ github.actor }}`
	// (docs/threads.md §3.5/§3.6), the same value a job-level `if:` reads on
	// the server side.
	Actor string `json:"actor,omitempty"`
	// EventPayload is the bounded, documented event body (PAR-033): the runner
	// writes it to $GITHUB_EVENT_PATH and projects its fields into
	// `github.event.*`. Nil for a run with no payload (a synthetic dispatch).
	EventPayload map[string]any `json:"event_payload,omitempty"`
	// Vars are the resolved repository and organization variables (PAR-035),
	// with the documented precedence repository > organization. They are
	// resolved server-side and masked like a secret value; the workflow reads
	// them as `${{ vars.<name> }}`.
	Vars map[string]string `json:"vars,omitempty"`
	Spec *Spec             `json:"spec"`
	// Checkout tells the runner where to clone from. URL is the runner-facing
	// repository URL (derived from the fetch request's host, so a reverse proxy
	// works); the credential is the attempt token (read scope, ci:<runner>
	// principal), which is why the checkout is always read-only — fork runs
	// get exactly that, never a push credential (15 §5.2).
	Checkout *CheckoutInfo `json:"checkout,omitempty"`
	// ServerURL is the canonical spindle origin (no trailing slash) the job
	// exposes as GITHUB_SERVER_URL/FORGEJO_SERVER_URL, with GITHUB_API_URL
	// derived as <origin>/api/v1. It is the same base the checkout URL uses
	// (server.public_url when set, otherwise the runner-facing request origin),
	// so a proxied deployment hands the job an address it can actually reach
	// (PAR-111).
	ServerURL string `json:"server_url,omitempty"`
	// Secrets are decrypted ONLY for a trusted run (15 §5); a fork run carries
	// an empty map and SecretsDenied=true.
	Secrets       map[string]string `json:"secrets"`
	Mask          []string          `json:"mask"`
	SecretsDenied bool              `json:"secrets_denied"`
	LeaseUntil    string            `json:"lease_until"`
	// LabelImages is the runner's declared label→image map (PAR-171), echoed so
	// the runner can state it in its log. The SELECTED image is already applied
	// to Spec.Container.Image below; this map is the declaration, not a second
	// instruction.
	LabelImages map[string]string `json:"label_images,omitempty"`
	// Permissions is the effective job-token permission map (workflow
	// `permissions:` over the instance default; a fork is always contents:read)
	// and Scopes the git scopes the attempt token will carry. The runner needs
	// neither to execute a job — the payload states them so a log line, a UI and
	// an operator can see exactly what authority the job received.
	Permissions map[string]string `json:"permissions"`
	Scopes      []string          `json:"scopes"`
	// NeedsOutputs are the outputs of the job's `needs:` dependencies, folded
	// when the job is claimed (only successful dependencies contribute; a
	// matrix dependency's cells are unioned, last cell in job-id order winning
	// a name). The runner substitutes them as
	// `${{ needs.<job>.outputs.<name> }}` (docs/threads.md §3.9).
	NeedsOutputs map[string]map[string]string `json:"needs_outputs,omitempty"`
	// IDToken is the job's OIDC request token (docs/threads.md §5.5), present
	// ONLY for a trusted job whose effective permissions say `id-token: write`.
	// The plaintext token exists solely here and in the job's environment; the
	// bucket holds its hash.
	IDToken *IDTokenRequest `json:"id_token,omitempty"`
}

// CheckoutInfo is the clone target one job container should use (pass B2).
type CheckoutInfo struct {
	URL      string `json:"url"`
	ReadOnly bool   `json:"read_only"`
}

// FetchJob long-polls for one job matching the runner's labels and repository
// policy. wait is capped at 30 s; (nil, nil) means the poll expired with nothing
// to do (204).
func (s *Service) FetchJob(ctx context.Context, runner *runnerAuth, wait time.Duration) (*FetchResult, error) {
	return s.FetchJobFrom(ctx, runner, wait, "")
}

// FetchJobFrom is FetchJob with the runner-facing checkout base (scheme://host)
// the HTTP handler derives from the request, so a proxied deployment hands the
// container a URL it can actually reach (pass B2 checkout).
//
// The claim is GLOBAL: the queue index names candidate repositories, the
// runner's tenancy policy filters them, and the per-repository claim loop is the
// only authority (queueindex.go).
func (s *Service) FetchJobFrom(ctx context.Context, runner *runnerAuth, wait time.Duration, checkoutBase string) (*FetchResult, error) {
	if wait <= 0 {
		wait = 20 * time.Second
	}
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	deadline := s.nowUTC().Add(wait)
	ch, cancel := fetchHub.subscribeGlobal()
	defer cancel()
	// blocked holds the entries this poll refused for concurrency (their group
	// is held by another live run); it is cleared on each 2 s re-probe.
	blocked := map[string]bool{}
	for {
		claim, owner, repo, err := s.claimGlobal(ctx, runner.Doc, runner.Doc.Labels, blocked)
		if err != nil {
			return nil, err
		}
		if claim != nil {
			// Concurrency admission (15 §4): the group CAS is the lock, so two
			// runners can never run two jobs of one group. A refusal gives the
			// claim straight back and scans the next entry.
			ok, aerr := s.admitConcurrency(ctx, owner, repo, claim.Entry.RunID, claim.Job.Spec)
			if aerr != nil {
				s.releaseClaim(ctx, owner, repo, claim)
				return nil, aerr
			}
			if !ok {
				blocked[repoID(owner, repo)+"|"+claim.Entry.Seq] = true
				s.releaseClaim(ctx, owner, repo, claim)
				continue
			}
			// The claim is the point the check goes in progress (deliverable:
			// claim → in_progress; the checks enum spells it "pending" with a
			// started_at, 05 §2).
			job, uerr := s.UpdateJob(ctx, repoID(owner, repo), claim.Entry.RunID, claim.Entry.JobID, JobUpdate{
				State: JobRunning, QueueSeq: claim.Entry.Seq, Attempt: claim.Entry.Attempt,
				LeaseUntil: claim.Entry.LeaseUntil,
				Runner:     runner.Doc.ID, RunnerName: runnerName(runner.Doc),
			})
			if uerr != nil {
				// Give the claim back rather than holding a job nobody runs.
				s.releaseClaim(ctx, owner, repo, claim)
				return nil, uerr
			}
			s.noteRunnerJob(ctx, runner.Doc.ID, repoID(owner, repo), claim)
			s.removeQueueIndexJob(ctx, repoID(owner, repo), claim.Entry.RunID, claim.Entry.JobID)
			return s.fetchPayload(ctx, owner, repo, runner, claim, job, checkoutBase)
		}
		if !s.nowUTC().Before(deadline) {
			return nil, nil
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-ch:
			timer.Stop()
		case <-timer.C:
		}
		blocked = map[string]bool{}
	}
}

// noteRunnerJob records the current lease on the registry (best effort: the
// job record is the authority; this is the admin page's display state).
func (s *Service) noteRunnerJob(ctx context.Context, runnerID, repo string, claim *Claim) {
	if runnerID == "" || claim == nil || claim.Entry == nil {
		return
	}
	ref := &RunnerJobRef{Repo: repo, RunID: claim.Entry.RunID, JobID: claim.Entry.JobID}
	lease := claim.Entry.LeaseUntil
	_, _ = s.casUpdate(ctx, RunnerKey(runnerID), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var d RunnerDoc
		if jerr := json.Unmarshal(cur, &d); jerr != nil {
			return nil, false, fmt.Errorf("%w: runner %s: %v", ErrCorrupt, runnerID, jerr)
		}
		if d.State == RunnerRevoked {
			return nil, false, nil
		}
		d.CurrentJob = ref
		d.LeaseUntil = lease
		d.LastSeenAt = ptr(s.nowUTC().Format(dateTimeFmt))
		d.Version++
		return mustJSON(&d), true, nil
	})
}

// clearRunnerCurrentJob drops one terminal (or requeued) job from the
// registry's display state. Best effort: a stale value is cosmetic and the next
// claim overwrites it.
func (s *Service) clearRunnerCurrentJob(ctx context.Context, runnerID, repo, runID, jobID string) {
	if runnerID == "" {
		return
	}
	_, _ = s.casUpdate(ctx, RunnerKey(runnerID), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var d RunnerDoc
		if jerr := json.Unmarshal(cur, &d); jerr != nil {
			return nil, false, fmt.Errorf("%w: runner %s: %v", ErrCorrupt, runnerID, jerr)
		}
		if d.CurrentJob == nil {
			return nil, false, nil
		}
		if d.CurrentJob.Repo != repo || d.CurrentJob.RunID != runID || d.CurrentJob.JobID != jobID {
			return nil, false, nil
		}
		d.CurrentJob = nil
		d.LeaseUntil = nil
		d.Version++
		return mustJSON(&d), true, nil
	})
}

// runnerName is a runner's display name, falling back to its id.
func runnerName(doc *RunnerDoc) string {
	if doc == nil {
		return ""
	}
	if name := strings.TrimSpace(doc.Name); name != "" {
		return name
	}
	return doc.ID
}

// fetchPayload builds the claim answer (secrets only for a trusted run).
func (s *Service) fetchPayload(ctx context.Context, owner, repo string, runner *runnerAuth, claim *Claim, job *Job, checkoutBase string) (*FetchResult, error) {
	id := repoID(owner, repo)
	res := &FetchResult{
		Repo: id, RunID: claim.Entry.RunID, JobID: claim.Entry.JobID,
		JobRef:  JobRef(id, claim.Entry.RunID, claim.Entry.JobID),
		Attempt: job.Attempt, Commit: claim.Run.Commit, Event: claim.Run.Event, Branch: claim.Run.Branch,
		Ref:      claim.Run.Ref,
		Workflow: claim.Run.Workflow, Trusted: claim.Run.Trusted, Context: job.Context, Spec: job.Spec,
		Actor:        claim.Run.Actor,
		EventPayload: claim.Run.EventPayload,
		Secrets:      map[string]string{}, Mask: []string{},
	}
	// Label→image mapping (PAR-171): when the runner declared
	// `ubuntu-latest:docker://node:lts` and this job's authored runs-on names
	// that label, the job runs in that image — but only when the workflow named
	// no container.image of its own (the workflow's explicit choice always
	// wins). The spec is copied before the injection so the stored job record
	// is untouched.
	if image := runnerImageFor(runner.Doc.LabelImages, jobRunsOn(job)); image != "" && job.Spec != nil {
		spec := *job.Spec
		if spec.Container == nil {
			spec.Container = &ContainerSpec{Image: image}
		} else {
			container := *spec.Container
			container.Image = image
			spec.Container = &container
		}
		res.Spec = &spec
		res.LabelImages = runner.Doc.LabelImages
	}
	trusted := claim.Run.Trusted && job.Trusted
	writeTokens := claim.Run.WriteTokens && !trusted
	perms := s.EffectivePermissionsFor(job.Spec, trusted, writeTokens)
	res.Permissions = perms
	res.Scopes = attemptScopes(PermissionScopes(perms))
	// The job OIDC request token (docs/PLAN-THREADS-OIDC.md): only a trusted
	// job that declares `id-token: write` or `enable-openid-connect: true`
	// gets one, and only when the instance has an origin the job container can
	// reach (server.public_url, or the request-derived base). A fork run is
	// untrusted and never reaches here.
	if s.IDTokenAllowed(job.Spec, trusted) {
		base := strings.TrimSpace(checkoutBase)
		if base == "" {
			base = strings.TrimSpace(s.CheckoutBaseURL)
		}
		if base != "" {
			req, ierr := s.mintIDTokenRequest(ctx, owner, repo, claim.Entry.RunID, claim.Entry.JobID,
				job.Attempt, attemptTokenTTL(job.Spec), base)
			if ierr != nil {
				return nil, ierr
			}
			res.IDToken = req
		}
	}
	// The checkout is read-only exactly when the token has no push scope: a
	// `contents: write` job may push, a fork (or a read-only workflow) may not.
	res.Checkout = &CheckoutInfo{ReadOnly: !containsScope(res.Scopes, ScopePush)}
	if base := strings.TrimSpace(checkoutBase); base != "" {
		res.Checkout.URL = strings.TrimRight(base, "/") + "/" + id + ".git"
		res.ServerURL = strings.TrimRight(base, "/")
	} else {
		res.Checkout = &CheckoutInfo{ReadOnly: !containsScope(res.Scopes, ScopePush)}
	}
	if claim.Entry.LeaseUntil != nil {
		res.LeaseUntil = *claim.Entry.LeaseUntil
	}
	// A dependent job's `needs.<job>.outputs.<name>` values are materialized at
	// claim time: the spec is written at run creation, before a dependency can
	// have finished, so the payload is the only honest place for them.
	if needs, nerr := s.claimNeedsOutputs(ctx, owner, repo, claim); nerr != nil {
		return nil, nerr
	} else if len(needs) > 0 {
		res.NeedsOutputs = needs
	}
	if !trusted {
		res.SecretsDenied = true
		return res, nil
	}
	// Variables (PAR-035) resolve for a trusted job exactly like secrets (same
	// sealing, same masking); an untrusted run gets none.
	vars, verr := s.variableValues(ctx, owner, repo)
	if verr != nil {
		return nil, verr
	}
	if len(vars) > 0 {
		res.Vars = vars
		for _, value := range vars {
			if value != "" {
				res.Mask = append(res.Mask, value)
			}
		}
		sort.Strings(res.Mask)
	}
	values, err := s.secretValues(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	// PAR-075/D-6: an expanded callee receives exactly the secrets its call
	// passes (the same-repository implicit set or `inherit`, or the explicit
	// name map). A cross-repository call that lists nothing gets nothing, and a
	// secret the callee does not declare never reaches it. An ordinary job has
	// no reusable secret set and keeps the repository's secrets.
	values = reusableSecretValues(job.Spec, values)
	// Environment secrets override repository secrets by name (GitHub's rule,
	// docs/PLAN-THREADS-ENVIRONMENTS.md §4). They are resolved only for a job
	// that names an environment, only for a TRUSTED run, and only when the
	// deployment is approved (or required no approval) — guardDeploymentSecrets
	// fails the fetch closed otherwise, so a deployment nobody approved can
	// never run with its credentials.
	if job.Spec != nil && job.Spec.Environment != nil {
		if gerr := s.guardDeploymentSecrets(ctx, owner, repo, job); gerr != nil {
			return nil, gerr
		}
		envVals, eerr := s.environmentSecretValues(ctx, owner, repo, strings.TrimSpace(job.Spec.Environment.Name))
		if eerr != nil {
			return nil, eerr
		}
		for name, value := range envVals {
			values[name] = value
		}
	}
	for name, value := range values {
		res.Secrets[name] = value
		res.Mask = append(res.Mask, value)
	}
	sort.Strings(res.Mask)
	return res, nil
}

// claimNeedsOutputs folds the current dependency records into the outputs map a
// dependent job's payload carries. Only a successful dependency contributes (a
// failed/cancelled/skipped one exposes none), and a matrix dependency is the
// union of its cells with the LAST cell in job-id order winning a collision —
// the same rule the server's job-`if:` fold applies.
func (s *Service) claimNeedsOutputs(ctx context.Context, owner, repo string, claim *Claim) (map[string]map[string]string, error) {
	if claim == nil || len(claim.Entry.NeedKeys) == 0 {
		return nil, nil
	}
	byID := make(map[string]*Job, len(claim.Entry.Needs))
	for _, ids := range claim.Entry.NeedKeys {
		for _, id := range ids {
			dep, err := s.loadJobCached(ctx, owner, repo, claim.Entry.RunID, id, nil)
			if err != nil {
				return nil, err
			}
			if dep != nil {
				byID[id] = dep
			}
		}
	}
	facts := foldNeeds(claim.Entry.NeedKeys, byID)
	return facts.Outputs, nil
}

// --- credential/runner resolution -------------------------------------------

// runnerAuth is one authenticated protocol request's context. It names no
// repository: what that runner may be offered is its admin-set RunnerPolicy,
// enforced by the claim filter.
type runnerAuth struct {
	Doc  *RunnerDoc
	Cred *RunnerCredential
}

// authorizeRunner resolves a wrt_ bearer to its credential and registry record
// (one exact-key GET each, no LIST) and rejects revoked/unknown runners.
func (s *Service) authorizeRunner(ctx context.Context, bearer string) (*runnerAuth, error) {
	_, id, secret, ok := tokenParts(bearer, runnerTokenPrefix)
	if !ok {
		return nil, fmt.Errorf("%w: runner token required", ErrUnauthorized)
	}
	cred, _, err := s.loadRunnerCredential(ctx, id)
	if err != nil {
		return nil, err
	}
	if cred == nil {
		return nil, fmt.Errorf("%w: unknown runner token", ErrUnauthorized)
	}
	if cred.RevokedAt != nil {
		return nil, fmt.Errorf("%w: runner token revoked", ErrUnauthorized)
	}
	if !verifySecret(secret, cred.TokenHash) {
		return nil, fmt.Errorf("%w: bad runner token", ErrUnauthorized)
	}
	doc, _, derr := s.loadRunner(ctx, id)
	if derr != nil {
		return nil, derr
	}
	if doc == nil {
		return nil, fmt.Errorf("%w: unknown runner", ErrUnauthorized)
	}
	if doc.State == RunnerRevoked || doc.RevokedAt != nil {
		return nil, fmt.Errorf("%w: runner revoked", ErrUnauthorized)
	}
	// Piggyback liveness: fetching is the heartbeat, so a last_seen older than
	// a minute is refreshed here (best effort, bounded to one write/minute;
	// a lost CAS is harmless — the next call retries).
	if doc.LastSeenAt == nil || s.olderThan(*doc.LastSeenAt, time.Minute) {
		now := s.nowUTC().Format(dateTimeFmt)
		_, _ = s.casUpdate(ctx, RunnerKey(id), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
			if cur == nil {
				return nil, false, nil
			}
			var d RunnerDoc
			if jerr := json.Unmarshal(cur, &d); jerr != nil {
				return nil, false, fmt.Errorf("%w: runner %s: %v", ErrCorrupt, id, jerr)
			}
			if d.State == RunnerRevoked {
				return nil, false, nil
			}
			d.LastSeenAt = &now
			d.Version++
			return mustJSON(&d), true, nil
		})
	}
	return &runnerAuth{Doc: doc, Cred: cred}, nil
}

// olderThan reports whether an RFC 3339 stamp is older than d.
func (s *Service) olderThan(stamp string, d time.Duration) bool {
	t, err := time.Parse(dateTimeFmt, stamp)
	if err != nil {
		return true
	}
	return s.nowUTC().Sub(t) > d
}

// loadRunner reads one registry record; (nil, "", nil) when absent.
func (s *Service) loadRunner(ctx context.Context, id string) (*RunnerDoc, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, RunnerKey(id))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return nil, "", nil
	}
	var doc RunnerDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		return nil, "", fmt.Errorf("%w: runner %s: %v", ErrCorrupt, id, jerr)
	}
	return &doc, ver, nil
}

// loadRunnerCredential reads one credential record; (nil, "", nil) when absent.
func (s *Service) loadRunnerCredential(ctx context.Context, id string) (*RunnerCredential, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, RunnerCredentialKey(id))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return nil, "", nil
	}
	var cred RunnerCredential
	if jerr := json.Unmarshal(raw, &cred); jerr != nil {
		return nil, "", fmt.Errorf("%w: credential %s: %v", ErrCorrupt, id, jerr)
	}
	return &cred, ver, nil
}

// loadRunnerToken reads one registration token record; (nil, "", nil) when
// absent.
func (s *Service) loadRunnerToken(ctx context.Context, id string) (*runnerTokenDoc, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, RunnerTokenKey(id))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return nil, "", nil
	}
	var tok runnerTokenDoc
	if jerr := json.Unmarshal(raw, &tok); jerr != nil {
		return nil, "", fmt.Errorf("%w: registration token %s: %v", ErrCorrupt, id, jerr)
	}
	return &tok, ver, nil
}

// mustJSON marshals a record (the records are pure JSON types, so this cannot
// fail; a failure would be a programming error and panics rather than writing a
// half record).
func mustJSON(v any) []byte {
	// The records are pure JSON types (strings, ints, slices, maps), so
	// encoding cannot fail; a nil body would fail every consumer shut.
	b, _ := json.Marshal(v)
	return b
}

// truncate bounds a display string (rune-safe).
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
