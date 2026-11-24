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

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// EnvironmentDoc is repos/<o>/<r>/threads/environments/<name>.json.
type EnvironmentDoc struct {
	Name string `json:"name"`
	// RequiredReviewers are principals (lowercased) who must approve a
	// deployment. Empty means no approval is required (the job runs like any
	// other, still with no secrets for a fork run).
	RequiredReviewers []string `json:"required_reviewers,omitempty"`
	// Secrets is the allowlist of environment secret names. Values live sealed
	// under the environment's secret prefix; this list is what the resolver
	// reads (no LIST).
	Secrets   []string `json:"secrets,omitempty"`
	CreatedBy string   `json:"created_by,omitempty"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	Version   int      `json:"version"`
}

// EnvironmentView is the wire/API shape: names and timestamps, never a value.
type EnvironmentView struct {
	Name              string   `json:"name"`
	RequiredReviewers []string `json:"required_reviewers"`
	Secrets           []string `json:"secrets"`
	CreatedBy         string   `json:"created_by,omitempty"`
	CreatedAt         string   `json:"created_at"`
	UpdatedAt         string   `json:"updated_at"`
	Version           int      `json:"version"`
}

// EnvironmentInput is the create/update body. Only the reviewer set is
// editable; secret names change through the secret endpoints so the record's
// allowlist and the stored ciphertexts cannot drift.
type EnvironmentInput struct {
	RequiredReviewers []string `json:"required_reviewers"`
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
// write access, deduped, lowercased, at most maxEnvironmentReviewers entries.
// Validation happens at SAVE time (the task's rule): a name nobody can resolve
// to write is ErrInvalid, never a silently ignored reviewer a deployment would
// then wait for forever.
func (s *Service) normalizeReviewers(ctx context.Context, owner, repo string, in []string) ([]string, error) {
	if len(in) > maxEnvironmentReviewers {
		return nil, fmt.Errorf("%w: at most %d required reviewers", ErrInvalid, maxEnvironmentReviewers)
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

// maxEnvironmentReviewers bounds one environment's reviewer list.
const maxEnvironmentReviewers = 10

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
		fmt.Sprintf("reviewers=%d", len(reviewers)))
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
	if lerr := s.Store.List(ctx, EnvironmentsPrefix(owner, rname)+name+"/secrets/", "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, ".json") {
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
	s.auditAdmin(ctx, p.Name, AuditEnvironmentDelete, "environment:"+name, "secrets="+fmt.Sprint(len(keys)))
	return nil
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
// fails the job (fail closed); a defined one carries the deployment record.
func (s *Service) applyEnvironmentDecision(ctx context.Context, owner, repo string, job *Job, now string) (bool, error) {
	if job.Spec == nil || job.Spec.Environment == nil {
		return false, nil
	}
	env := strings.TrimSpace(job.Spec.Environment.Name)
	doc, err := s.loadEnvironment(ctx, owner, repo, env)
	if err != nil {
		return false, err
	}
	if doc == nil {
		if job.State == JobQueued {
			job.State = JobFailure
			job.CompletedAt = &now
			job.Reason = fmt.Sprintf("environment %q is not defined in this repository (create it in Settings → Environments)", env)
		}
		return false, nil
	}
	state := DeploymentNotRequired
	if len(doc.RequiredReviewers) > 0 {
		state = DeploymentPending
	}
	job.Deployment = &Deployment{Environment: env, URL: job.Spec.Environment.URL, State: state}
	return state == DeploymentPending && job.State == JobQueued, nil
}

// guardDeploymentSecrets is the payload-side half of the gate: a job whose
// environment requires reviewers must carry an APPROVED deployment record
// before its secrets are resolved. Unreachable through the queue (an approval
// enqueues), but a claim racing a deletion or a stale queue entry must fail
// closed rather than run a deployment nobody approved.
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
	if len(doc.RequiredReviewers) == 0 {
		return nil
	}
	if job.Deployment == nil || job.Deployment.State != DeploymentApproved {
		return fmt.Errorf("%w: deployment to environment %q is not approved", ErrForbidden, env)
	}
	return nil
}

// holdsForEnvironment reports whether a principal may DECIDE a run held by an
// environment: a repo admin always may; otherwise the principal must be listed
// as a required reviewer of EVERY gated environment, and hold at least write
// (a read/anonymous principal can never decide, whatever a stale list says).
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
		listed := false
		for _, reviewer := range doc.RequiredReviewers {
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
