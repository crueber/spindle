package threads

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Runner registry and registration (15 §3):
//
//	repos/<o>/<r>/threads/runners/<id>.json       registry: labels, state, last seen
//	users/<principal>/tokens/threads/<id>.json    credential: hash only, revocable
//
// Runners are repository-scoped: the admin of a repository mints the
// registration token in Settings → Runners, and the runner only ever sees that
// repository's queue. (An instance-wide runner would need a global dispatch
// index; the queue deliberately stays per-repository so the claim path is one
// probe of one head — 15 §8.)

// Runner states.
const (
	RunnerPending = "pending"
	RunnerActive  = "active"
	RunnerRevoked = "revoked"
)

// Credential kinds.
const (
	credRegistration = "registration"
	credRunner       = "runner"
)

// RegistrationTTL bounds an unused registration token (15 minutes: long enough
// for an operator to paste it into a runner, short enough to be useless when
// leaked).
const RegistrationTTL = 15 * time.Minute

// RunnerDoc is one runner's registry record (labels + liveness; never a secret).
type RunnerDoc struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Labels []string `json:"labels"`
	State  string   `json:"state"`
	// DeclaredVersion is the runner's self-reported version (declare).
	DeclaredVersion string  `json:"declared_version,omitempty"`
	RegisteredBy    string  `json:"registered_by,omitempty"`
	CreatedAt       string  `json:"created_at"`
	RegisteredAt    *string `json:"registered_at,omitempty"`
	LastSeenAt      *string `json:"last_seen_at,omitempty"`
	RevokedAt       *string `json:"revoked_at,omitempty"`
	Version         int     `json:"version"`
}

// RunnerCredential is one users/<principal>/tokens/threads/<id>.json record:
// hash only, revocable. A registration credential becomes the runner
// credential on first use (one token, one consumption).
type RunnerCredential struct {
	ID         string  `json:"id"`
	Principal  string  `json:"principal"`
	Repo       string  `json:"repo"`
	Kind       string  `json:"kind"`
	TokenHash  string  `json:"token_hash"`
	CreatedAt  string  `json:"created_at"`
	ExpiresAt  *string `json:"expires_at,omitempty"`
	ConsumedAt *string `json:"consumed_at,omitempty"`
	RevokedAt  *string `json:"revoked_at,omitempty"`
	Version    int     `json:"version"`
}

// --- admin surface -----------------------------------------------------------

// RegistrationToken is the mint answer: the secret travels here exactly once.
type RegistrationToken struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// MintRegistrationToken mints a one-time registration token (repo admin). The
// registry record and the credential record are both created here, so the
// runner appears in Settings → Runners as "pending" until it registers.
func (s *Service) MintRegistrationToken(ctx context.Context, repo string, p auth.Principal) (*RegistrationToken, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, name, p, "admin"); err != nil {
		return nil, err
	}
	// A credential under "anonymous"/"anon" would be unattributable; admins
	// mint under their own principal.
	principal := strings.TrimSpace(p.Name)
	if p.Anonymous || principal == "" {
		return nil, fmt.Errorf("%w: a registration token needs an authenticated principal", ErrForbidden)
	}
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
	doc := &RunnerDoc{
		ID: id, State: RunnerPending, Labels: []string{},
		RegisteredBy: strings.ToLower(principal), CreatedAt: now.Format(dateTimeFmt), Version: 1,
	}
	cred := &RunnerCredential{
		ID: id, Principal: strings.ToLower(principal), Repo: repoID(owner, name),
		Kind: credRegistration, TokenHash: hash, CreatedAt: now.Format(dateTimeFmt),
		ExpiresAt: ptr(expires.Format(dateTimeFmt)), Version: 1,
	}
	// Credential first: a registry entry without a credential is unusable and
	// would block the id (the run-header ordering rule, inverted).
	if perr := s.putCreate(ctx, RunnerTokenKey(cred.Principal, id), mustJSON(cred)); perr != nil {
		return nil, mintConflict(perr)
	}
	if perr := s.putCreate(ctx, RunnerKey(owner, name, id), mustJSON(doc)); perr != nil {
		_ = s.Store.Delete(ctx, RunnerTokenKey(cred.Principal, id), "")
		return nil, mintConflict(perr)
	}
	return &RegistrationToken{ID: id, Token: token, ExpiresAt: *cred.ExpiresAt}, nil
}

// mintConflict maps a duplicate id onto a retryable conflict.
func mintConflict(err error) error {
	if store.IsPreconditionFailed(err) {
		return fmt.Errorf("%w: runner id collided; retry", ErrUnavailable)
	}
	return err
}

// RunnerView is one row of GET …/api/threads/runners (never a secret).
type RunnerView struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Labels          []string `json:"labels"`
	State           string   `json:"state"`
	DeclaredVersion string   `json:"declared_version,omitempty"`
	CreatedAt       string   `json:"created_at"`
	RegisteredAt    *string  `json:"registered_at,omitempty"`
	LastSeenAt      *string  `json:"last_seen_at,omitempty"`
	RevokedAt       *string  `json:"revoked_at,omitempty"`
}

// ListRunners lists a repository's runners (admin; one bounded prefix LIST over
// a human-scale family — never a hot path).
func (s *Service) ListRunners(ctx context.Context, repo string, p auth.Principal) ([]RunnerView, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, name, p, "admin"); err != nil {
		return nil, err
	}
	var keys []string
	if lerr := s.Store.List(ctx, RunnersPrefix(owner, name), "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, ".json") {
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
			ID: doc.ID, Name: doc.Name, Labels: nonNilStr(doc.Labels), State: doc.State,
			DeclaredVersion: doc.DeclaredVersion, CreatedAt: doc.CreatedAt,
			RegisteredAt: doc.RegisteredAt, LastSeenAt: doc.LastSeenAt, RevokedAt: doc.RevokedAt,
		})
	}
	return out, nil
}

// RevokeRunner revokes one runner (admin): the registry state flips and the
// credential record is stamped revoked, so the next bearer is a real 401.
func (s *Service) RevokeRunner(ctx context.Context, repo, id string, p auth.Principal) error {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	if err := s.requireRole(ctx, owner, name, p, "admin"); err != nil {
		return err
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if !tokenIDRe.MatchString(id) {
		return fmt.Errorf("%w: unknown runner %q", ErrNotFound, id)
	}
	key := RunnerKey(owner, name, id)
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
	_, err = s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
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
		d.Version++
		return mustJSON(&d), true, nil
	})
	if err != nil {
		return err
	}
	// The credential lives under the minting principal; its address is the one
	// the registry record does not carry, so revoke every credential bearing
	// this id by probing the record's owner. The runner record's
	// RegisteredBy is that principal.
	if doc.RegisteredBy != "" {
		_ = s.revokeCredential(ctx, doc.RegisteredBy, id, now)
	}
	return nil
}

// revokeCredential CAS-stamps one credential record revoked (best effort: the
// registry state already denies the bearer, since every protocol call loads it).
func (s *Service) revokeCredential(ctx context.Context, principal, id, now string) error {
	key := RunnerTokenKey(principal, id)
	_, err := s.casUpdate(ctx, key, 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
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

// --- protocol: register / declare / fetch ------------------------------------

// RegisterInput is POST …/runners/register.
type RegisterInput struct {
	Token  string   `json:"token"`
	Name   string   `json:"name"`
	Labels []string `json:"labels"`
}

// RegisterResult is the register answer: the long-lived runner token, shown
// once (the server stores only its hash).
type RegisterResult struct {
	RunnerID string   `json:"runner_id"`
	Repo     string   `json:"repo"`
	Token    string   `json:"token"`
	Labels   []string `json:"labels"`
}

// Register consumes a registration token exactly once.
func (s *Service) Register(ctx context.Context, in RegisterInput) (*RegisterResult, error) {
	principal, id, secret, ok := tokenParts(in.Token, regTokenPrefix)
	if !ok {
		return nil, fmt.Errorf("%w: not a registration token", ErrUnauthorized)
	}
	cred, _, err := s.loadCredential(ctx, principal, id)
	if err != nil {
		return nil, err
	}
	if cred == nil || cred.Kind != credRegistration {
		return nil, fmt.Errorf("%w: unknown registration token", ErrUnauthorized)
	}
	if cred.RevokedAt != nil || cred.ConsumedAt != nil {
		return nil, fmt.Errorf("%w: registration token already used or revoked", ErrUnauthorized)
	}
	if cred.ExpiresAt != nil {
		if exp, perr := time.Parse(dateTimeFmt, *cred.ExpiresAt); perr != nil || s.nowUTC().After(exp) {
			return nil, fmt.Errorf("%w: registration token expired", ErrUnauthorized)
		}
	}
	if !verifySecret(secret, cred.TokenHash) {
		return nil, fmt.Errorf("%w: bad registration token", ErrUnauthorized)
	}
	owner, name, rerr := splitRepo(cred.Repo)
	if rerr != nil {
		return nil, rerr
	}
	runner, _, gerr := s.loadRunner(ctx, owner, name, id)
	if gerr != nil {
		return nil, gerr
	}
	if runner == nil || runner.State == RunnerRevoked {
		return nil, fmt.Errorf("%w: runner is revoked", ErrUnauthorized)
	}
	labels := normalizeLabels(in.Labels)
	if len(labels) == 0 {
		labels = []string{"self-hosted"}
	}
	runnerToken, runnerHash, merr := mintScopedToken(runnerTokenPrefix, cred.Principal, id)
	if merr != nil {
		return nil, fmt.Errorf("%w: mint: %v", ErrUnavailable, merr)
	}
	now := s.nowUTC().Format(dateTimeFmt)
	// Credential CAS first (single use), then the registry.
	_, cerr := s.casUpdate(ctx, RunnerTokenKey(cred.Principal, id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown registration token", ErrUnauthorized)
		}
		var c RunnerCredential
		if jerr := json.Unmarshal(cur, &c); jerr != nil {
			return nil, false, fmt.Errorf("%w: credential %s: %v", ErrCorrupt, id, jerr)
		}
		if c.Kind != credRegistration || c.ConsumedAt != nil || c.RevokedAt != nil {
			return nil, false, fmt.Errorf("%w: registration token already used or revoked", ErrUnauthorized)
		}
		c.Kind = credRunner
		c.TokenHash = runnerHash
		c.ConsumedAt = &now
		c.ExpiresAt = nil
		c.Version++
		return mustJSON(&c), true, nil
	})
	if cerr != nil {
		return nil, cerr
	}
	_, derr := s.casUpdate(ctx, RunnerKey(owner, name, id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
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
		d.RegisteredAt = &now
		d.LastSeenAt = &now
		d.Version++
		return mustJSON(&d), true, nil
	})
	if derr != nil {
		return nil, derr
	}
	return &RegisterResult{RunnerID: id, Repo: cred.Repo, Token: runnerToken, Labels: labels}, nil
}

// DeclareInput is POST …/runners/declare.
type DeclareInput struct {
	Version string   `json:"version"`
	Labels  []string `json:"labels"`
}

// DeclareView is the declare answer.
type DeclareView struct {
	RunnerID string   `json:"runner_id"`
	Repo     string   `json:"repo"`
	State    string   `json:"state"`
	Labels   []string `json:"labels"`
	// LeaseSeconds is the claim lease the server will grant.
	LeaseSeconds int `json:"lease_seconds"`
}

// Declare records a runner's version and label set (it is also its liveness
// record: fetching is the heartbeat, declare states what the runner is).
func (s *Service) Declare(ctx context.Context, runner *runnerAuth, in DeclareInput) (*DeclareView, error) {
	labels := normalizeLabels(in.Labels)
	now := s.nowUTC().Format(dateTimeFmt)
	key := RunnerKey(runner.Owner, runner.Repo, runner.Doc.ID)
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
		}
		if v := truncate(strings.TrimSpace(in.Version), 64); v != "" {
			d.DeclaredVersion = v
		}
		d.LastSeenAt = &now
		d.Version++
		runner.Doc = &d
		return mustJSON(&d), true, nil
	})
	if err != nil {
		return nil, err
	}
	return &DeclareView{
		RunnerID: runner.Doc.ID, Repo: repoID(runner.Owner, runner.Repo),
		State: runner.Doc.State, Labels: nonNilStr(runner.Doc.Labels),
		LeaseSeconds: int((15 * time.Minute).Seconds()),
	}, nil
}

// FetchResult is the fetch answer: a self-contained job payload (the runner
// needs no further reads to start — 12_runner D2).
type FetchResult struct {
	Repo     string `json:"repo"`
	RunID    string `json:"run_id"`
	JobID    string `json:"job_id"`
	JobRef   string `json:"job_ref"`
	Attempt  int    `json:"attempt"`
	Commit   string `json:"commit"`
	Event    string `json:"event"`
	Branch   string `json:"branch"`
	Workflow string `json:"workflow"`
	Trusted  bool   `json:"trusted"`
	Context  string `json:"context"`
	Spec     *Spec  `json:"spec"`
	// Checkout tells the runner where to clone from. URL is the runner-facing
	// repository URL (derived from the fetch request's host, so a reverse proxy
	// works); the credential is the attempt token (read scope, ci:<runner>
	// principal), which is why the checkout is always read-only — fork runs
	// get exactly that, never a push credential (15 §5.2).
	Checkout *CheckoutInfo `json:"checkout,omitempty"`
	// Secrets are decrypted ONLY for a trusted run (15 §5); a fork run carries
	// an empty map and SecretsDenied=true.
	Secrets       map[string]string `json:"secrets"`
	Mask          []string          `json:"mask"`
	SecretsDenied bool              `json:"secrets_denied"`
	LeaseUntil    string            `json:"lease_until"`
	// Permissions is the effective job-token permission map (workflow
	// `permissions:` over the instance default; a fork is always contents:read)
	// and Scopes the git scopes the attempt token will carry. The runner needs
	// neither to execute a job — the payload states them so a log line, a UI and
	// an operator can see exactly what authority the job received.
	Permissions map[string]string `json:"permissions"`
	Scopes      []string          `json:"scopes"`
}

// CheckoutInfo is the clone target one job container should use (pass B2).
type CheckoutInfo struct {
	URL      string `json:"url"`
	ReadOnly bool   `json:"read_only"`
}

// FetchJob long-polls for one job matching the runner's labels. wait is capped
// at 30 s; (nil, nil) means the poll expired with nothing to do (204).
func (s *Service) FetchJob(ctx context.Context, runner *runnerAuth, wait time.Duration) (*FetchResult, error) {
	return s.FetchJobFrom(ctx, runner, wait, "")
}

// FetchJobFrom is FetchJob with the runner-facing checkout base (scheme://host)
// the HTTP handler derives from the request, so a proxied deployment hands the
// container a URL it can actually reach (pass B2 checkout).
func (s *Service) FetchJobFrom(ctx context.Context, runner *runnerAuth, wait time.Duration, checkoutBase string) (*FetchResult, error) {
	if wait <= 0 {
		wait = 20 * time.Second
	}
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	deadline := s.nowUTC().Add(wait)
	repo := repoID(runner.Owner, runner.Repo)
	ch, cancel := fetchHub.subscribe(repo)
	defer cancel()
	// blocked holds the sequences this poll refused for concurrency (their
	// group is held by another live run); it is cleared on each 2 s re-probe.
	blocked := map[string]bool{}
	for {
		claim, err := s.claimLoopSkip(ctx, runner.Owner, runner.Repo, runner.Doc, runner.Doc.Labels, blocked)
		if err != nil {
			return nil, err
		}
		if claim != nil {
			// Concurrency admission (15 §4): the group CAS is the lock, so two
			// runners can never run two jobs of one group. A refusal gives the
			// claim straight back and scans the next entry.
			ok, aerr := s.admitConcurrency(ctx, runner.Owner, runner.Repo, claim.Entry.RunID, claim.Job.Spec)
			if aerr != nil {
				s.releaseClaim(ctx, runner.Owner, runner.Repo, claim)
				return nil, aerr
			}
			if !ok {
				blocked[claim.Entry.Seq] = true
				s.releaseClaim(ctx, runner.Owner, runner.Repo, claim)
				continue
			}
			// The claim is the point the check goes in progress (deliverable:
			// claim → in_progress; the checks enum spells it "pending" with a
			// started_at, 05 §2).
			job, uerr := s.UpdateJob(ctx, repo, claim.Entry.RunID, claim.Entry.JobID, JobUpdate{
				State: JobRunning, QueueSeq: claim.Entry.Seq, Attempt: claim.Entry.Attempt,
				LeaseUntil: claim.Entry.LeaseUntil,
				Runner:     runner.Doc.ID, RunnerName: runnerName(runner.Doc),
			})
			if uerr != nil {
				// Give the claim back rather than holding a job nobody runs.
				s.releaseClaim(ctx, runner.Owner, runner.Repo, claim)
				return nil, uerr
			}
			return s.fetchPayload(ctx, runner, claim, job, checkoutBase)
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
func (s *Service) fetchPayload(ctx context.Context, runner *runnerAuth, claim *Claim, job *Job, checkoutBase string) (*FetchResult, error) {
	repo := repoID(runner.Owner, runner.Repo)
	res := &FetchResult{
		Repo: repo, RunID: claim.Entry.RunID, JobID: claim.Entry.JobID,
		JobRef:  JobRef(repo, claim.Entry.RunID, claim.Entry.JobID),
		Attempt: job.Attempt, Commit: claim.Run.Commit, Event: claim.Run.Event, Branch: claim.Run.Branch,
		Workflow: claim.Run.Workflow, Trusted: claim.Run.Trusted, Context: job.Context, Spec: job.Spec,
		Secrets: map[string]string{}, Mask: []string{},
	}
	trusted := claim.Run.Trusted && job.Trusted
	perms := s.EffectivePermissions(job.Spec, trusted)
	res.Permissions = perms
	res.Scopes = attemptScopes(PermissionScopes(perms))
	// The checkout is read-only exactly when the token has no push scope: a
	// `contents: write` job may push, a fork (or a read-only workflow) may not.
	res.Checkout = &CheckoutInfo{ReadOnly: !containsScope(res.Scopes, ScopePush)}
	if base := strings.TrimSpace(checkoutBase); base != "" {
		res.Checkout.URL = strings.TrimRight(base, "/") + "/" + repo + ".git"
	} else {
		res.Checkout = &CheckoutInfo{ReadOnly: !containsScope(res.Scopes, ScopePush)}
	}
	if claim.Entry.LeaseUntil != nil {
		res.LeaseUntil = *claim.Entry.LeaseUntil
	}
	if !trusted {
		res.SecretsDenied = true
		return res, nil
	}
	values, err := s.secretValues(ctx, runner.Owner, runner.Repo)
	if err != nil {
		return nil, err
	}
	for name, value := range values {
		res.Secrets[name] = value
		res.Mask = append(res.Mask, value)
	}
	sort.Strings(res.Mask)
	return res, nil
}

// --- credential/runner resolution -------------------------------------------

// runnerAuth is one authenticated protocol request's context.
type runnerAuth struct {
	Owner string
	Repo  string
	Doc   *RunnerDoc
	Cred  *RunnerCredential
}

// authorizeRunner resolves a wrt_ bearer to its credential and registry record
// (one exact-key GET each, no LIST) and rejects revoked/unknown runners.
func (s *Service) authorizeRunner(ctx context.Context, bearer string) (*runnerAuth, error) {
	principal, id, secret, ok := tokenParts(bearer, runnerTokenPrefix)
	if !ok {
		return nil, fmt.Errorf("%w: runner token required", ErrUnauthorized)
	}
	cred, _, err := s.loadCredential(ctx, principal, id)
	if err != nil {
		return nil, err
	}
	if cred == nil || cred.Kind != credRunner {
		return nil, fmt.Errorf("%w: unknown runner token", ErrUnauthorized)
	}
	if cred.RevokedAt != nil {
		return nil, fmt.Errorf("%w: runner token revoked", ErrUnauthorized)
	}
	if !verifySecret(secret, cred.TokenHash) {
		return nil, fmt.Errorf("%w: bad runner token", ErrUnauthorized)
	}
	owner, repo, rerr := splitRepo(cred.Repo)
	if rerr != nil {
		return nil, rerr
	}
	doc, _, derr := s.loadRunner(ctx, owner, repo, id)
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
		_, _ = s.casUpdate(ctx, RunnerKey(owner, repo, id), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
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
	return &runnerAuth{Owner: owner, Repo: repo, Doc: doc, Cred: cred}, nil
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
func (s *Service) loadRunner(ctx context.Context, owner, repo, id string) (*RunnerDoc, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, RunnerKey(owner, repo, id))
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

// loadCredential reads one credential record; (nil, "", nil) when absent.
func (s *Service) loadCredential(ctx context.Context, principal, id string) (*RunnerCredential, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, RunnerTokenKey(principal, id))
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
