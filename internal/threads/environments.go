package threads

// Deployment environments (docs/PLAN-THREADS-ENVIRONMENTS.md).
//
// An environment is a repository-scoped record that (a) names the principals
// who must approve a deployment to it and (b) carries the secrets scoped to it.
// The record is the authority: a job that names an environment the record does
// not define FAILS (fail closed — a typo must never silently deploy), a job
// naming one with required reviewers is created pending approval, and its
// environment secrets are resolved only after that approval, only for a trusted
// run.
//
// Storage (bucket-only, CAS — law 4):
//
//	repos/<o>/<r>/threads/environments/<name>.json            the record
//	repos/<o>/<r>/threads/environments/<name>/secrets/<S>.json sealed value
//
// The record's `secrets` field is the allowlist of names that exist for the
// environment, so resolving a job payload does one exact-key GET for the record
// and one per listed secret — never a LIST on the request path (law 4/6). The
// sealed value reuses the repository-secret envelope (AES-256-GCM), with an AAD
// that binds it to (repo, environment, name) so a ciphertext cannot be replayed
// as a repository secret or under another environment.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// EnvironmentDoc is repos/<o>/<r>/threads/environments/<name>.json.
type EnvironmentDoc struct {
	Name string `json:"name"`
	// RequiredReviewers are principals (lowercased) who must approve a
	// deployment. Empty (with no team reviewers and no wait timer) means no
	// approval is required (the job runs like any other, still with no secrets
	// for a fork run).
	RequiredReviewers []string `json:"required_reviewers,omitempty"`
	// TeamReviewers are organization teams ("org/team") whose members must
	// approve; the members are resolved at save and at each decision (PAR-131).
	TeamReviewers []string `json:"team_reviewers,omitempty"`
	// WaitTimerSeconds delays every deployment's start after creation
	// (0…21600 = GitHub's 6-hour ceiling).
	WaitTimerSeconds int `json:"wait_timer_seconds,omitempty"`
	// PreventSelfReview refuses the run's own actor as an approver.
	PreventSelfReview bool `json:"prevent_self_review,omitempty"`
	// DeploymentBranches is the deployment branch/tag policy: empty = every
	// ref; otherwise the run's ref must match one pattern (exact names and
	// `*`, matched against the short branch/tag name and the full ref).
	DeploymentBranches []string `json:"deployment_branches,omitempty"`
	// Secrets is the allowlist of environment secret names. Values live sealed
	// under the environment's secret prefix; this list is what the resolver
	// reads (no LIST).
	Secrets []string `json:"secrets,omitempty"`
	// Vars is the allowlist of non-secret environment variable names
	// (PAR-117). Values are sealed like repository variables and override
	// repository variables of the same name at claim time.
	Vars      []string `json:"vars,omitempty"`
	CreatedBy string   `json:"created_by,omitempty"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	Version   int      `json:"version"`
}

// EnvironmentView is the wire/API shape: names and timestamps, never a value.
type EnvironmentView struct {
	Name               string   `json:"name"`
	RequiredReviewers  []string `json:"required_reviewers"`
	TeamReviewers      []string `json:"team_reviewers,omitempty"`
	WaitTimerSeconds   int      `json:"wait_timer_seconds,omitempty"`
	PreventSelfReview  bool     `json:"prevent_self_review,omitempty"`
	DeploymentBranches []string `json:"deployment_branches,omitempty"`
	Secrets            []string `json:"secrets"`
	Vars               []string `json:"vars"`
	CreatedBy          string   `json:"created_by,omitempty"`
	CreatedAt          string   `json:"created_at"`
	UpdatedAt          string   `json:"updated_at"`
	Version            int      `json:"version"`
}

// EnvironmentInput is the create/update body. Only the protection rules are
// editable here; secret names change through the secret endpoints so the
// record's allowlist and the stored ciphertexts cannot drift.
type EnvironmentInput struct {
	RequiredReviewers  []string `json:"required_reviewers"`
	TeamReviewers      []string `json:"team_reviewers,omitempty"`
	WaitTimerSeconds   int      `json:"wait_timer_seconds,omitempty"`
	PreventSelfReview  bool     `json:"prevent_self_review,omitempty"`
	DeploymentBranches []string `json:"deployment_branches,omitempty"`
}

// EnvironmentKey returns repos/<o>/<r>/threads/environments/<name>.json.
func EnvironmentKey(owner, repo, name string) string {
	return "repos/" + owner + "/" + repo + "/threads/environments/" + name + ".json"
}

// EnvironmentsPrefix returns repos/<o>/<r>/threads/environments/.
func EnvironmentsPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/threads/environments/"
}

// EnvironmentSecretKey returns repos/<o>/<r>/threads/environments/<name>/secrets/<secret>.json.
func EnvironmentSecretKey(owner, repo, env, secret string) string {
	return EnvironmentsPrefix(owner, repo) + env + "/secrets/" + secret + ".json"
}

// EnvironmentVariableKey returns repos/<o>/<r>/threads/environments/<name>/vars/<var>.json.
func EnvironmentVariableKey(owner, repo, env, variable string) string {
	return EnvironmentsPrefix(owner, repo) + env + "/vars/" + variable + ".json"
}

// normalizeEnvironmentName validates an environment name (the same charset the
// workflow parser pins; it is a bucket key segment).
func normalizeEnvironmentName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !workflow.EnvironmentNameRe.MatchString(name) {
		return "", fmt.Errorf("%w: environment name must match [A-Za-z0-9][A-Za-z0-9._-]{0,63}", ErrInvalid)
	}
	return name, nil
}

// normalizeReviewers validates the reviewer list: principals with at least
// write access, deduped, lowercased, at most MaxEnvironmentReviewers entries.
// Validation happens at SAVE time (the task's rule): a name nobody can resolve
// to write is ErrInvalid, never a silently ignored reviewer a deployment would
// then wait for forever.
func (s *Service) normalizeReviewers(ctx context.Context, owner, repo string, in []string) ([]string, error) {
	if len(in) > MaxEnvironmentReviewers {
		return nil, fmt.Errorf("%w: at most %d required reviewers", ErrInvalid, MaxEnvironmentReviewers)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" || seen[name] {
			continue
		}
		if len(name) > 100 {
			return nil, fmt.Errorf("%w: reviewer name is too long", ErrInvalid)
		}
		role := s.roleOf(ctx, owner, repo, auth.Principal{Name: name})
		if roleRank(role) < roleRank("write") {
			return nil, fmt.Errorf("%w: reviewer %q needs at least write access to this repository", ErrInvalid, name)
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// maxEnvironmentReviewers is kept as the internal alias of the exported limit.
const maxEnvironmentReviewers = MaxEnvironmentReviewers

// PutEnvironment creates or replaces one environment record (maintain+, audited).
func (s *Service) PutEnvironment(ctx context.Context, repo, name string, in EnvironmentInput, p auth.Principal) (*EnvironmentView, error) {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, rname, p, "maintain"); err != nil {
		return nil, err
	}
	name, err = normalizeEnvironmentName(name)
	if err != nil {
		return nil, err
	}
	reviewers, err := s.normalizeReviewers(ctx, owner, rname, in.RequiredReviewers)
	if err != nil {
		return nil, err
	}
	teams, err := s.normalizeTeamReviewers(ctx, in.TeamReviewers)
	if err != nil {
		return nil, err
	}
	// GitHub's ceiling is six reviewers OR teams in total: a seventh is a named
	// error, not a silently truncated gate.
	if len(reviewers)+len(teams) > MaxEnvironmentReviewers {
		return nil, fmt.Errorf("%w: at most %d required reviewers and teams together (got %d)",
			ErrInvalid, MaxEnvironmentReviewers, len(reviewers)+len(teams))
	}
	if in.WaitTimerSeconds < 0 || in.WaitTimerSeconds > MaxEnvironmentWaitSeconds {
		return nil, fmt.Errorf("%w: wait timer must be 0…%d seconds (6 hours)", ErrInvalid, MaxEnvironmentWaitSeconds)
	}
	branches, err := normalizeDeploymentBranches(in.DeploymentBranches)
	if err != nil {
		return nil, err
	}
	now := s.nowUTC().Format(dateTimeFmt)
	key := EnvironmentKey(owner, rname, name)
	created := false
	var out *EnvironmentDoc
	_, err = s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		doc := &EnvironmentDoc{Name: name}
		if cur != nil {
			if jerr := json.Unmarshal(cur, doc); jerr != nil {
				return nil, false, fmt.Errorf("%w: environment %s: %v", ErrCorrupt, name, jerr)
			}
		} else {
			created = true
			doc.CreatedAt = now
			doc.CreatedBy = truncate(strings.ToLower(strings.TrimSpace(p.Name)), 100)
		}
		doc.RequiredReviewers = reviewers
		doc.TeamReviewers = teams
		doc.WaitTimerSeconds = in.WaitTimerSeconds
		doc.PreventSelfReview = in.PreventSelfReview
		doc.DeploymentBranches = branches
		doc.UpdatedAt = now
		doc.Version++
		out = doc
		return mustJSON(doc), true, nil
	})
	if err != nil {
		return nil, err
	}
	action := AuditEnvironmentUpdate
	if created {
		action = AuditEnvironmentCreate
	}
	s.auditAdmin(ctx, p.Name, action, "environment:"+name,
		fmt.Sprintf("reviewers=%d teams=%d wait=%ds branches=%d", len(reviewers), len(teams), in.WaitTimerSeconds, len(branches)))
	return environmentView(out), nil
}

// DeleteEnvironment removes one environment record (maintain+, audited) and its
// sealed secrets. A run already held for it fails closed at approval time.
func (s *Service) DeleteEnvironment(ctx context.Context, repo, name string, p auth.Principal) error {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return err
	}
	if err := s.requireRole(ctx, owner, rname, p, "maintain"); err != nil {
		return err
	}
	name, err = normalizeEnvironmentName(name)
	if err != nil {
		return err
	}
	// A LIST here is fine: it is an administrative delete, not a request path,
	// and the record's allowlist may be stale. Secrets are removed before the
	// record so a crash leaves a record that still names them, never orphaned
	// ciphertext nobody can find.
	var keys []string
	if lerr := s.Store.List(ctx, EnvironmentsPrefix(owner, rname)+name+"/", "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, ".json") && (strings.Contains(m.Key, "/secrets/") || strings.Contains(m.Key, "/vars/")) {
			keys = append(keys, m.Key)
		}
		return nil
	}); lerr != nil {
		return lerr
	}
	for _, key := range keys {
		if derr := s.Store.Delete(ctx, key, ""); derr != nil {
			return derr
		}
	}
	if derr := s.Store.Delete(ctx, EnvironmentKey(owner, rname, name), ""); derr != nil {
		return derr
	}
	// PAR-136: a deleted environment must not leave deployments queued behind a
	// slot that no longer exists. Every waiting deployment fails closed with a
	// named reason, then the queue object is removed.
	s.failEnvironmentQueue(ctx, owner, rname, name)
	s.auditAdmin(ctx, p.Name, AuditEnvironmentDelete, "environment:"+name, "secrets="+fmt.Sprint(len(keys)))
	return nil
}

// failEnvironmentQueue terminalises every deployment (job or API) that was
// waiting on a deleted environment and removes the queue object. Best effort:
// the record deletion is the commit point, and a missed transition is repaired
// when the job's claim fails closed on the missing environment.
func (s *Service) failEnvironmentQueue(ctx context.Context, owner, repo, env string) {
	q, err := s.loadEnvironmentQueue(ctx, owner, repo, env)
	if err == nil && q != nil {
		tokens := make([]string, 0, len(q.Entries)+1)
		if q.Holder != "" {
			tokens = append(tokens, q.Holder)
		}
		for _, entry := range q.Entries {
			tokens = append(tokens, entry.Token)
		}
		now := s.nowUTC().Format(dateTimeFmt)
		for _, token := range tokens {
			if rest, ok := strings.CutPrefix(token, "job:"); ok {
				runID, jobID, ok := strings.Cut(rest, ":")
				if !ok {
					continue
				}
				job, run, lerr := s.loadJobAndRun(ctx, owner, repo, runID, jobID)
				if lerr != nil || job == nil || terminal(job.State) {
					continue
				}
				// A pending_approval run keeps its existing contract: deletion
				// fails the deployment closed at the decision/claim, not here (a
				// reviewer may still deny, and the page explains the state).
				if run != nil && run.State == RunPendingApproval {
					continue
				}
				// Only a queued waiter is failed here; a running attempt already
				// carries its payload and ends through its own path.
				if job.State != JobQueued {
					continue
				}
				dep := Deployment{Environment: env, State: DeploymentFailed}
				if job.Deployment != nil {
					dep = *job.Deployment
					dep.State, dep.QueuePosition, dep.QueueReason = DeploymentFailed, 0, ""
				}
				_, _ = s.UpdateJob(ctx, repoID(owner, repo), runID, jobID, JobUpdate{
					State: JobFailure, Deployment: &dep,
					Reason: fmt.Sprintf("environment %q was deleted while the deployment was queued", env),
				})
				continue
			}
			id, ok := strings.CutPrefix(token, "dep:")
			if !ok {
				continue
			}
			doc, derr := s.loadDeployment(ctx, owner, repo, id)
			if derr != nil || doc == nil || terminalDeploymentState(doc.State) {
				continue
			}
			doc.State = DeploymentStateFailure
			doc.QueuePosition, doc.QueueReason = 0, ""
			doc.Statuses = appendDeploymentStatus(doc.Statuses, DeploymentStatus{
				State: DeploymentStateFailure, Creator: "threads", CreatedAt: now,
				Description: fmt.Sprintf("environment %q was deleted while the deployment was queued", env),
			})
			doc.UpdatedAt = now
			doc.Version++
			_, _ = s.casUpdate(ctx, DeploymentKey(owner, repo, id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
				if cur == nil {
					return nil, false, nil
				}
				var live DeploymentDoc
				if jerr := json.Unmarshal(cur, &live); jerr != nil {
					return nil, false, nil
				}
				live.State, live.QueuePosition, live.QueueReason = doc.State, 0, ""
				live.Statuses, live.UpdatedAt = doc.Statuses, doc.UpdatedAt
				live.Version++
				return mustJSON(&live), true, nil
			})
		}
	}
	_ = s.Store.Delete(ctx, EnvironmentQueueKey(owner, repo, env), "")
}

// GetEnvironment reads one record (maintain+ — it names secret names).
func (s *Service) GetEnvironment(ctx context.Context, repo, name string, p auth.Principal) (*EnvironmentView, error) {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, rname, p, "maintain"); err != nil {
		return nil, err
	}
	name, err = normalizeEnvironmentName(name)
	if err != nil {
		return nil, err
	}
	doc, err := s.loadEnvironment(ctx, owner, rname, name)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("%w: unknown environment %q", ErrNotFound, name)
	}
	return environmentView(doc), nil
}

// ListEnvironments lists the repository's environments (maintain+), sorted by
// name. It is a settings read, never a job-path read.
func (s *Service) ListEnvironments(ctx context.Context, repo string, p auth.Principal) ([]EnvironmentView, error) {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, rname, p, "maintain"); err != nil {
		return nil, err
	}
	prefix := EnvironmentsPrefix(owner, rname)
	var keys []string
	if lerr := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		rest := strings.TrimPrefix(m.Key, prefix)
		// Direct children only: `<name>.json`, never a secret doc under
		// `<name>/secrets/`.
		if strings.HasSuffix(rest, ".json") && !strings.Contains(rest, "/") {
			keys = append(keys, m.Key)
		}
		return nil
	}); lerr != nil {
		return nil, lerr
	}
	sort.Strings(keys)
	out := []EnvironmentView{}
	for _, key := range keys {
		raw, _, gerr := s.getJSON(ctx, key)
		if gerr != nil {
			return nil, gerr
		}
		if raw == nil {
			continue
		}
		var doc EnvironmentDoc
		if jerr := json.Unmarshal(raw, &doc); jerr != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, key, jerr)
		}
		out = append(out, *environmentView(&doc))
	}
	return out, nil
}

// environmentNameList resolves the repository's environment names without the
// settings role gate: the dispatch form is visible to write-role users and an
// environment NAME is not a secret. It is one bounded LIST, and only on a
// dispatch that declares an `environment` input.
func (s *Service) environmentNameList(ctx context.Context, owner, repo string) ([]string, error) {
	prefix := EnvironmentsPrefix(owner, repo)
	names := []string{}
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		rest := strings.TrimPrefix(m.Key, prefix)
		if strings.HasSuffix(rest, ".json") && !strings.Contains(rest, "/") {
			names = append(names, strings.TrimSuffix(rest, ".json"))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// EnvironmentNames is the dispatch picker's read (PAR-023): names only, behind
// the repository READ gate (a name is not a secret, and a write-role
// dispatcher may not hold `maintain`).
func (s *Service) EnvironmentNames(ctx context.Context, repo string, p auth.Principal) ([]string, error) {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, rname, p); err != nil {
		return nil, err
	}
	return s.environmentNameList(ctx, owner, rname)
}

// loadEnvironment reads one record exactly (no LIST). A nil doc, nil error means
// "no such environment".
func (s *Service) loadEnvironment(ctx context.Context, owner, repo, name string) (*EnvironmentDoc, error) {
	raw, _, err := s.getJSON(ctx, EnvironmentKey(owner, repo, name))
	if err != nil || raw == nil {
		return nil, err
	}
	var doc EnvironmentDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		return nil, fmt.Errorf("%w: environment %s: %v", ErrCorrupt, name, jerr)
	}
	return &doc, nil
}

// environmentView projects one record onto its wire shape.
func environmentView(doc *EnvironmentDoc) *EnvironmentView {
	if doc == nil {
		return nil
	}
	return &EnvironmentView{
		Name: doc.Name, RequiredReviewers: nonNilStr(doc.RequiredReviewers), Secrets: nonNilStr(doc.Secrets),
		Vars:          nonNilStr(doc.Vars),
		TeamReviewers: nonNilStr(doc.TeamReviewers), WaitTimerSeconds: doc.WaitTimerSeconds,
		PreventSelfReview: doc.PreventSelfReview, DeploymentBranches: nonNilStr(doc.DeploymentBranches),
		CreatedBy: doc.CreatedBy, CreatedAt: doc.CreatedAt, UpdatedAt: doc.UpdatedAt, Version: doc.Version,
	}
}

// --- secrets -----------------------------------------------------------------

// envSecretAAD binds one environment ciphertext to (repo, environment, name).
func envSecretAAD(repo, env, name string) []byte {
	return []byte("spindle/threads/environment-secret/v1\x00" + repo + "\x00" + env + "\x00" + name)
}

// sealEnvironmentSecret seals one value (the repository-secret envelope, a
// distinct AAD).
func (s *Service) sealEnvironmentSecret(repo, env, name, value string) (string, error) {
	if !s.keys.SecretsEnabled() {
		return "", fmt.Errorf("%w: secrets need server.secrets_key or server.auth.session_secret", ErrUnavailable)
	}
	gcm := gcmFor(s.keys.Seal)
	nonce, err := randomBytes(gcm.NonceSize())
	if err != nil {
		return "", fmt.Errorf("%w: nonce: %v", ErrUnavailable, err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(value), envSecretAAD(repo, env, name))
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// openEnvironmentSecret opens one stored value (a trusted job payload only).
func (s *Service) openEnvironmentSecret(repo, env, name, sealed string) (string, error) {
	if !s.keys.SecretsEnabled() {
		return "", fmt.Errorf("%w: secrets need server.secrets_key or server.auth.session_secret", ErrUnavailable)
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("%w: environment secret %s/%s: bad envelope", ErrCorrupt, env, name)
	}
	gcm := gcmFor(s.keys.Seal)
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("%w: environment secret %s/%s: short envelope", ErrCorrupt, env, name)
	}
	nonce, body := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, oerr := gcm.Open(nil, nonce, body, envSecretAAD(repo, env, name))
	if oerr != nil {
		return "", fmt.Errorf("%w: environment secret %s/%s: cannot be opened (wrong key or corrupt)", ErrCorrupt, env, name)
	}
	return string(plain), nil
}

// ListEnvironmentSecrets lists one environment's secret names (maintain+;
// values never returned).
func (s *Service) ListEnvironmentSecrets(ctx context.Context, repo, env string, p auth.Principal) ([]SecretView, error) {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, rname, p, "maintain"); err != nil {
		return nil, err
	}
	env, err = normalizeEnvironmentName(env)
	if err != nil {
		return nil, err
	}
	doc, err := s.loadEnvironment(ctx, owner, rname, env)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("%w: unknown environment %q", ErrNotFound, env)
	}
	out := []SecretView{}
	for _, name := range doc.Secrets {
		raw, _, gerr := s.getJSON(ctx, EnvironmentSecretKey(owner, rname, env, name))
		if gerr != nil {
			return nil, gerr
		}
		if raw == nil {
			continue
		}
		var secret SecretDoc
		if jerr := json.Unmarshal(raw, &secret); jerr != nil {
			return nil, fmt.Errorf("%w: environment secret %s/%s: %v", ErrCorrupt, env, name, jerr)
		}
		out = append(out, SecretView{Name: secret.Name, CreatedAt: secret.CreatedAt, UpdatedAt: secret.UpdatedAt})
	}
	return out, nil
}

// SetEnvironmentSecret stores (or replaces) one environment secret and adds its
// name to the record's allowlist (maintain+, write-only, audited). The record is
// updated FIRST: a ciphertext no record names is unreachable, while a name whose
// value failed to store reads as "listed, absent" and is skipped (never a silent
// empty secret injected into a job).
func (s *Service) SetEnvironmentSecret(ctx context.Context, repo, env, name, value string, p auth.Principal) error {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return err
	}
	if err := s.requireRole(ctx, owner, rname, p, "maintain"); err != nil {
		return err
	}
	env, err = normalizeEnvironmentName(env)
	if err != nil {
		return err
	}
	name, err = normalizeSecretName(name)
	if err != nil {
		return err
	}
	if !s.SecretsEnabled() {
		return fmt.Errorf("%w: secrets need server.secrets_key or server.auth.session_secret", ErrUnavailable)
	}
	if len(value) == 0 || len(value) > 64<<10 {
		return fmt.Errorf("%w: secret value must be 1 byte–64 KiB", ErrInvalid)
	}
	sealed, serr := s.sealEnvironmentSecret(repo, env, name, value)
	if serr != nil {
		return serr
	}
	now := s.nowUTC().Format(dateTimeFmt)
	doc, err := s.addEnvironmentSecretName(ctx, owner, rname, env, name, now)
	if err != nil {
		return err
	}
	_ = doc
	key := EnvironmentSecretKey(owner, rname, env, name)
	_, err = s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		secret := &SecretDoc{Name: name, CreatedAt: now}
		if cur != nil {
			if jerr := json.Unmarshal(cur, secret); jerr != nil {
				return nil, false, fmt.Errorf("%w: environment secret %s/%s: %v", ErrCorrupt, env, name, jerr)
			}
		}
		secret.Sealed = sealed
		secret.UpdatedAt = now
		secret.CreatedBy = truncate(strings.ToLower(strings.TrimSpace(p.Name)), 100)
		secret.Version++
		return mustJSON(secret), true, nil
	})
	if err != nil {
		return err
	}
	s.auditAdmin(ctx, p.Name, AuditEnvironmentSecretSet, "environment:"+env, "secret="+name)
	return nil
}

// addEnvironmentSecretName adds name to the record's allowlist (CAS, idempotent).
func (s *Service) addEnvironmentSecretName(ctx context.Context, owner, repo, env, name, now string) (*EnvironmentDoc, error) {
	var out *EnvironmentDoc
	_, err := s.casUpdate(ctx, EnvironmentKey(owner, repo, env), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown environment %q", ErrNotFound, env)
		}
		var doc EnvironmentDoc
		if jerr := json.Unmarshal(cur, &doc); jerr != nil {
			return nil, false, fmt.Errorf("%w: environment %s: %v", ErrCorrupt, env, jerr)
		}
		for _, existing := range doc.Secrets {
			if existing == name {
				out = &doc
				return nil, false, nil // already listed: no write
			}
		}
		doc.Secrets = append(doc.Secrets, name)
		sort.Strings(doc.Secrets)
		doc.UpdatedAt = now
		doc.Version++
		out = &doc
		return mustJSON(&doc), true, nil
	})
	return out, err
}

// DeleteEnvironmentSecret removes one secret and drops its name from the
// allowlist (maintain+, audited; absent is not an error).
func (s *Service) DeleteEnvironmentSecret(ctx context.Context, repo, env, name string, p auth.Principal) error {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return err
	}
	if err := s.requireRole(ctx, owner, rname, p, "maintain"); err != nil {
		return err
	}
	env, err = normalizeEnvironmentName(env)
	if err != nil {
		return err
	}
	name, err = normalizeSecretName(name)
	if err != nil {
		return err
	}
	if derr := s.Store.Delete(ctx, EnvironmentSecretKey(owner, rname, env, name), ""); derr != nil {
		return derr
	}
	now := s.nowUTC().Format(dateTimeFmt)
	_, err = s.casUpdate(ctx, EnvironmentKey(owner, rname, env), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var doc EnvironmentDoc
		if jerr := json.Unmarshal(cur, &doc); jerr != nil {
			return nil, false, fmt.Errorf("%w: environment %s: %v", ErrCorrupt, env, jerr)
		}
		kept := doc.Secrets[:0]
		for _, existing := range doc.Secrets {
			if existing != name {
				kept = append(kept, existing)
			}
		}
		doc.Secrets = kept
		doc.UpdatedAt = now
		doc.Version++
		return mustJSON(&doc), true, nil
	})
	if err != nil {
		return err
	}
	s.auditAdmin(ctx, p.Name, AuditEnvironmentSecretDelete, "environment:"+env, "secret="+name)
	return nil
}

// --- environment variables (PAR-117) ----------------------------------------

// envVariableAAD binds one environment variable ciphertext to (repo,
// environment, name), so it cannot be replayed as a repository variable or
// under another environment.
func envVariableAAD(repo, env, name string) []byte {
	return []byte("spindle/threads/environment-variable/v1\x00" + repo + "\x00" + env + "\x00" + name)
}

// sealEnvironmentVariable seals one value with the variable envelope.
func (s *Service) sealEnvironmentVariable(repo, env, name, value string) (string, error) {
	if !s.keys.SecretsEnabled() {
		return "", fmt.Errorf("%w: variables need server.secrets_key or server.auth.session_secret", ErrUnavailable)
	}
	gcm := gcmFor(s.keys.Seal)
	nonce, err := randomBytes(gcm.NonceSize())
	if err != nil {
		return "", fmt.Errorf("%w: nonce: %v", ErrUnavailable, err)
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(value), envVariableAAD(repo, env, name))), nil
}

// openEnvironmentVariable opens one stored value.
func (s *Service) openEnvironmentVariable(repo, env, name, sealed string) (string, error) {
	if !s.keys.SecretsEnabled() {
		return "", fmt.Errorf("%w: variables need server.secrets_key or server.auth.session_secret", ErrUnavailable)
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("%w: environment variable %s/%s: bad envelope", ErrCorrupt, env, name)
	}
	gcm := gcmFor(s.keys.Seal)
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("%w: environment variable %s/%s: short envelope", ErrCorrupt, env, name)
	}
	nonce, body := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, oerr := gcm.Open(nil, nonce, body, envVariableAAD(repo, env, name))
	if oerr != nil {
		return "", fmt.Errorf("%w: environment variable %s/%s: cannot be opened (wrong key or corrupt)", ErrCorrupt, env, name)
	}
	return string(plain), nil
}

// ListEnvironmentVariables lists one environment's variable names (maintain+;
// values never returned).
func (s *Service) ListEnvironmentVariables(ctx context.Context, repo, env string, p auth.Principal) ([]VariableView, error) {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, rname, p, "maintain"); err != nil {
		return nil, err
	}
	env, err = normalizeEnvironmentName(env)
	if err != nil {
		return nil, err
	}
	doc, err := s.loadEnvironment(ctx, owner, rname, env)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("%w: unknown environment %q", ErrNotFound, env)
	}
	out := []VariableView{}
	for _, name := range doc.Vars {
		raw, _, gerr := s.getJSON(ctx, EnvironmentVariableKey(owner, rname, env, name))
		if gerr != nil {
			return nil, gerr
		}
		if raw == nil {
			continue
		}
		var entry VariableValue
		if jerr := json.Unmarshal(raw, &entry); jerr != nil {
			return nil, fmt.Errorf("%w: environment variable %s/%s: %v", ErrCorrupt, env, name, jerr)
		}
		out = append(out, VariableView{Name: entry.Name, CreatedBy: entry.CreatedBy, CreatedAt: entry.CreatedAt, UpdatedAt: entry.UpdatedAt})
	}
	return out, nil
}

// SetEnvironmentVariable stores (or replaces) one environment variable and
// adds its name to the record's allowlist (maintain+, audited). The record is
// updated first, exactly like a secret: a ciphertext no record names is
// unreachable, while a listed name with no value is skipped.
func (s *Service) SetEnvironmentVariable(ctx context.Context, repo, env, name, value string, p auth.Principal) error {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return err
	}
	if err := s.requireRole(ctx, owner, rname, p, "maintain"); err != nil {
		return err
	}
	env, err = normalizeEnvironmentName(env)
	if err != nil {
		return err
	}
	name, err = normalizeVariableName(name)
	if err != nil {
		return err
	}
	if !s.SecretsEnabled() {
		return fmt.Errorf("%w: variables need server.secrets_key or server.auth.session_secret", ErrUnavailable)
	}
	if len(value) > 64<<10 {
		return fmt.Errorf("%w: variable value must be ≤ 64 KiB", ErrInvalid)
	}
	sealed, serr := s.sealEnvironmentVariable(repoID(owner, rname), env, name, value)
	if serr != nil {
		return serr
	}
	now := s.nowUTC().Format(dateTimeFmt)
	doc, err := s.loadEnvironment(ctx, owner, rname, env)
	if err != nil {
		return err
	}
	if doc == nil {
		return fmt.Errorf("%w: unknown environment %q", ErrNotFound, env)
	}
	if derr := s.addEnvironmentVariableName(ctx, owner, rname, env, name, now); derr != nil {
		return derr
	}
	key := EnvironmentVariableKey(owner, rname, env, name)
	_, err = s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		entry := &VariableValue{Name: name, CreatedAt: now}
		if cur != nil {
			if jerr := json.Unmarshal(cur, entry); jerr != nil {
				return nil, false, fmt.Errorf("%w: environment variable %s/%s: %v", ErrCorrupt, env, name, jerr)
			}
		}
		entry.Sealed = sealed
		entry.UpdatedAt = now
		entry.CreatedBy = truncate(strings.ToLower(strings.TrimSpace(p.Name)), 100)
		return mustJSON(entry), true, nil
	})
	if err != nil {
		return err
	}
	s.auditAdmin(ctx, p.Name, AuditEnvironmentVariableSet, "environment:"+env, "variable="+name)
	return nil
}

// addEnvironmentVariableName adds name to the record's variable allowlist
// (CAS, idempotent).
func (s *Service) addEnvironmentVariableName(ctx context.Context, owner, repo, env, name, now string) error {
	_, err := s.casUpdate(ctx, EnvironmentKey(owner, repo, env), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown environment %q", ErrNotFound, env)
		}
		var doc EnvironmentDoc
		if jerr := json.Unmarshal(cur, &doc); jerr != nil {
			return nil, false, fmt.Errorf("%w: environment %s: %v", ErrCorrupt, env, jerr)
		}
		for _, existing := range doc.Vars {
			if existing == name {
				return nil, false, nil
			}
		}
		doc.Vars = append(doc.Vars, name)
		sort.Strings(doc.Vars)
		doc.UpdatedAt = now
		doc.Version++
		return mustJSON(&doc), true, nil
	})
	return err
}

// DeleteEnvironmentVariable removes one variable and drops its name from the
// allowlist (maintain+, audited; absent is not an error).
func (s *Service) DeleteEnvironmentVariable(ctx context.Context, repo, env, name string, p auth.Principal) error {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return err
	}
	if err := s.requireRole(ctx, owner, rname, p, "maintain"); err != nil {
		return err
	}
	env, err = normalizeEnvironmentName(env)
	if err != nil {
		return err
	}
	name, err = normalizeVariableName(name)
	if err != nil {
		return err
	}
	if derr := s.Store.Delete(ctx, EnvironmentVariableKey(owner, rname, env, name), ""); derr != nil {
		return derr
	}
	now := s.nowUTC().Format(dateTimeFmt)
	_, err = s.casUpdate(ctx, EnvironmentKey(owner, rname, env), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var doc EnvironmentDoc
		if jerr := json.Unmarshal(cur, &doc); jerr != nil {
			return nil, false, fmt.Errorf("%w: environment %s: %v", ErrCorrupt, env, jerr)
		}
		kept := doc.Vars[:0]
		for _, existing := range doc.Vars {
			if existing != name {
				kept = append(kept, existing)
			}
		}
		doc.Vars = kept
		doc.UpdatedAt = now
		doc.Version++
		return mustJSON(&doc), true, nil
	})
	if err != nil {
		return err
	}
	s.auditAdmin(ctx, p.Name, AuditEnvironmentVariableDelete, "environment:"+env, "variable="+name)
	return nil
}

// environmentVariableValues resolves one environment's non-secret variables
// for a trusted job payload (PAR-117). They override repository/organization
// variables of the same name at the claim merge, exactly like GitHub.
func (s *Service) environmentVariableValues(ctx context.Context, owner, repo, env string) (map[string]string, error) {
	out := map[string]string{}
	if !s.SecretsEnabled() {
		return out, nil
	}
	doc, err := s.loadEnvironment(ctx, owner, repo, env)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("%w: environment %q no longer exists", ErrNotFound, env)
	}
	for _, name := range doc.Vars {
		raw, _, gerr := s.getJSON(ctx, EnvironmentVariableKey(owner, repo, env, name))
		if gerr != nil {
			return nil, gerr
		}
		if raw == nil {
			continue
		}
		var entry VariableValue
		if jerr := json.Unmarshal(raw, &entry); jerr != nil {
			return nil, fmt.Errorf("%w: environment variable %s/%s: %v", ErrCorrupt, env, name, jerr)
		}
		value, oerr := s.openEnvironmentVariable(repoID(owner, repo), env, entry.Name, entry.Sealed)
		if oerr != nil {
			return nil, oerr
		}
		out[entry.Name] = value
	}
	return out, nil
}

// environmentSecretValues decrypts one environment's secrets for a trusted job
// payload. A missing environment or an unreadable envelope fails the fetch
// (fail closed: a job that silently lost its secrets is worse than a clear
// error an operator can fix).
func (s *Service) environmentSecretValues(ctx context.Context, owner, repo, env string) (map[string]string, error) {
	out := map[string]string{}
	if !s.SecretsEnabled() {
		return out, nil
	}
	doc, err := s.loadEnvironment(ctx, owner, repo, env)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		// The record vanished between approval and the claim: never inject a
		// partial set.
		return nil, fmt.Errorf("%w: environment %q no longer exists", ErrNotFound, env)
	}
	for _, name := range doc.Secrets {
		raw, _, gerr := s.getJSON(ctx, EnvironmentSecretKey(owner, repo, env, name))
		if gerr != nil {
			return nil, gerr
		}
		if raw == nil {
			continue // listed but absent: skip, never inject an empty value
		}
		var secret SecretDoc
		if jerr := json.Unmarshal(raw, &secret); jerr != nil {
			return nil, fmt.Errorf("%w: environment secret %s/%s: %v", ErrCorrupt, env, name, jerr)
		}
		value, oerr := s.openEnvironmentSecret(repoID(owner, repo), env, secret.Name, secret.Sealed)
		if oerr != nil {
			return nil, oerr
		}
		out[secret.Name] = value
	}
	return out, nil
}

// --- the gate ----------------------------------------------------------------

// pendingEnvironments lists the gated environment names of a freshly decided
// job set (unique, sorted): the run's hold names them.
func pendingEnvironments(jobs []*Job) []string {
	var gated []string
	seen := map[string]bool{}
	for _, job := range jobs {
		if job.State != JobQueued || job.Deployment == nil || job.Deployment.State != DeploymentPending {
			continue
		}
		if seen[job.Deployment.Environment] {
			continue
		}
		seen[job.Deployment.Environment] = true
		gated = append(gated, job.Deployment.Environment)
	}
	sort.Strings(gated)
	return gated
}

// uniqueEnvNames sorts and dedupes a gated environment list (matrix cells share
// one environment name).
func uniqueEnvNames(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// applyEnvironmentDecision resolves one job's environment at creation and
// reports whether that job requires an approval hold. An undefined environment
// fails the job (fail closed); a defined one whose branch/tag policy refuses the
// run's ref fails the job with a named reason; a defined one carries the
// deployment record with its resolved approver set and wait-timer deadline.
func (s *Service) applyEnvironmentDecision(ctx context.Context, owner, repo string, job *Job, run *Run, now time.Time) (bool, error) {
	if job.Spec == nil || job.Spec.Environment == nil {
		return false, nil
	}
	env := strings.TrimSpace(job.Spec.Environment.Name)
	doc, err := s.loadEnvironment(ctx, owner, repo, env)
	if err != nil {
		return false, err
	}
	nowStr := now.Format(dateTimeFmt)
	if doc == nil {
		if job.State == JobQueued {
			job.State = JobFailure
			job.CompletedAt = &nowStr
			job.Reason = fmt.Sprintf("environment %q is not defined in this repository (create it in Settings → Environments)", env)
		}
		return false, nil
	}
	// Deployment branch/tag policy (PAR-131): a ref the environment does not
	// allow fails the job with a named reason — the same fail-closed shape as an
	// undefined environment, so nothing ever deploys from a forbidden ref.
	if allowed, reason := deploymentRefAllowed(doc.DeploymentBranches, run, env); !allowed {
		job.Deployment = &Deployment{Environment: env, URL: job.Spec.Environment.URL, State: DeploymentFailed}
		if job.State == JobQueued {
			job.State = JobFailure
			job.CompletedAt = &nowStr
			job.Reason = reason
		}
		return false, nil
	}
	approvers, aerr := s.deploymentApprovers(ctx, doc)
	if aerr != nil {
		// An unresolvable team requirement fails the deployment closed: a hold
		// nobody can satisfy would show a spinner forever (law 7).
		job.Deployment = &Deployment{Environment: env, URL: job.Spec.Environment.URL, State: DeploymentFailed}
		if job.State == JobQueued {
			job.State = JobFailure
			job.CompletedAt = &nowStr
			job.Reason = aerr.Error()
		}
		return false, nil
	}
	state := DeploymentNotRequired
	if len(approvers) > 0 {
		state = DeploymentPending
	}
	job.Deployment = &Deployment{
		Environment: env, URL: job.Spec.Environment.URL, State: state,
		RequiredApprovers: approvers, WaitUntil: waitUntilFor(doc, now),
		PreventSelfReview: doc.PreventSelfReview, TriggeredBy: strings.ToLower(strings.TrimSpace(run.Actor)),
		DeploymentID: jobDeploymentID(job.RunID, job.ID),
	}
	// PAR-136: every environment deployment takes the environment's
	// serialization slot in trigger order. A second deployment waits (position
	// 1..N) instead of running beside the first; the claim path refuses a
	// queued position and the visible reason names the holder. A full queue is
	// a named refusal on the job — never a silent overwrite or a parallel run.
	token := "job:" + job.RunID + ":" + job.ID
	position, reason, qerr := s.acquireEnvironmentSlot(ctx, owner, repo, env, token, jobDeploymentLabel(run, job, env), job.RunID, job.ID)
	if qerr != nil {
		job.Deployment.State = DeploymentFailed
		if job.State == JobQueued {
			job.State = JobFailure
			job.CompletedAt = &nowStr
			job.Reason = qerr.Error()
		}
		return false, nil
	}
	job.Deployment.QueuePosition = position
	if position > 0 {
		job.Deployment.QueueReason = reason
	}
	s.syncJobDeployment(ctx, owner, repo, job, run)
	// PAR-021: a deployment record transition is a `deployment` +
	// `deployment_status` event. It is emitted off the creation path (detach)
	// and the follow-up chain cap bounds a deployment_status workflow that
	// itself deploys.
	s.emitDeploymentEvents(run, job, deploymentStatusState(state), "")
	return state == DeploymentPending && job.State == JobQueued, nil
}

// jobDeploymentLabel is the visible queue holder label ("run … / job …").
func jobDeploymentLabel(run *Run, job *Job, env string) string {
	runID := ""
	if run != nil {
		runID = run.ID
	}
	name := ""
	if job != nil {
		name = job.Name
		if name == "" {
			name = job.Key
		}
	}
	return fmt.Sprintf("deployment for job %q in environment %q (run %s)", name, env, runID)
}

// guardDeploymentSecrets is the payload-side half of the gate: a job whose
// environment requires reviewers must carry an APPROVED deployment record
// before its secrets are resolved, and no deployment may resolve them before
// its wait timer elapses. Unreachable through the queue (an approval enqueues,
// the claim path honours the timer), but a claim racing a deletion or a stale
// queue entry must fail closed rather than run a deployment nobody approved.
func (s *Service) guardDeploymentSecrets(ctx context.Context, owner, repo string, job *Job) error {
	if job == nil || job.Spec == nil || job.Spec.Environment == nil {
		return nil
	}
	env := strings.TrimSpace(job.Spec.Environment.Name)
	doc, err := s.loadEnvironment(ctx, owner, repo, env)
	if err != nil {
		return err
	}
	if doc == nil {
		return fmt.Errorf("%w: environment %q no longer exists", ErrNotFound, env)
	}
	approvers, aerr := s.deploymentApprovers(ctx, doc)
	if aerr != nil {
		return aerr
	}
	if len(approvers) > 0 {
		if job.Deployment == nil || job.Deployment.State != DeploymentApproved {
			return fmt.Errorf("%w: deployment to environment %q is not approved", ErrForbidden, env)
		}
	}
	if waiting, until := deploymentWaitRemaining(job, s.nowUTC()); waiting {
		return fmt.Errorf("%w: deployment to environment %q waits until %s (wait timer)", ErrConflict, env, until)
	}
	return nil
}

// holdsForEnvironment reports whether a principal may DECIDE a run held by an
// environment: a repo admin always may; otherwise the principal must be in the
// environment's CURRENT required-approver set (its direct reviewers plus the
// members of its team reviewers), and hold at least write (a read/anonymous
// principal can never decide, whatever a stale list says). The live record is
// the authority, exactly like GitHub's protection rules: a roster edit applies
// to the next decision.
func (s *Service) holdsForEnvironment(ctx context.Context, owner, repo string, p auth.Principal, run *Run) error {
	role := "admin"
	if !p.Admin {
		role = s.roleOf(ctx, owner, repo, p)
	}
	if roleRank(role) < roleRank("write") {
		return fmt.Errorf("%w: approving a deployment requires at least write access", ErrForbidden)
	}
	if roleRank(role) >= roleRank("admin") {
		return nil
	}
	who := strings.ToLower(strings.TrimSpace(p.Name))
	if who == "" {
		return fmt.Errorf("%w: approving a deployment requires a signed-in reviewer", ErrForbidden)
	}
	for _, env := range run.Environments {
		doc, err := s.loadEnvironment(ctx, owner, repo, env)
		if err != nil {
			return err
		}
		if doc == nil {
			// A deleted environment has no reviewers left; only an admin can
			// release it (and approval then fails the job — see ApproveRun).
			return fmt.Errorf("%w: environment %q no longer exists; an admin must decide this run", ErrForbidden, env)
		}
		approvers, aerr := s.deploymentApprovers(ctx, doc)
		if aerr != nil {
			return fmt.Errorf("%w: %v", ErrForbidden, aerr)
		}
		listed := false
		for _, reviewer := range approvers {
			if reviewer == who {
				listed = true
				break
			}
		}
		if !listed {
			return fmt.Errorf("%w: %q is not a required reviewer of environment %q", ErrForbidden, p.Name, env)
		}
	}
	return nil
}
