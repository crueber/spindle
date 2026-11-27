package threads

// Organization secrets (PAR-115, docs/threads.md §5.1b).
//
// Scope model and precedence — the documented order is
// ENVIRONMENT > REPOSITORY > ORGANIZATION (the same rule GitHub states, and the
// one docs/threads.md §5.1/§5.1b pins):
//
//	environment secret  wins over a repository secret of the same name
//	repository secret   wins over an organization secret of the same name
//	organization secret resolves in the repositories its policy selects
//
// The scope a value came from is recorded on the run's job (Job.SecretScopes,
// name → scope) so the page can say WHERE a secret came from — never WHAT it
// was. Values are sealed with the same keyring and AAD discipline as the
// repository scope, and they enter the same mask list, so an org secret that
// reaches a job is masked in the log stream exactly like a repository secret.
//
// A USER scope is deliberately NOT modelled. A Threads run executes as the
// REPOSITORY, not as a person: `github.actor` is the triggering identity, but
// the credential is the repository's, and a per-user secret would let one
// contributor's personal credential be read by a workflow they did not write.
// This is the same decision variables took (vars.go), and it is recorded in
// docs/threads.md §5.1b and the parity note for PAR-115.
//
// Storage: one document, `orgs/<org>/threads/secrets.json`, never one object per
// name. Resolution on the claim path is then EXACTLY ONE conditional-free GET
// (never a LIST over the org prefix, and never one GET per secret), which is
// what the read-budget rule requires. The document is bounded (MaxOrgSecrets)
// so one organization cannot make every claim in it large.
//
// Repository policy: an entry with Visibility "all" resolves in every
// repository the organization owns; "selected" resolves only in the listed
// repositories. An unselected repository gets NOTHING — not an empty value, not
// the name.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Org secret visibility values (the API/UI spellings).
const (
	OrgSecretAll      = "all"
	OrgSecretSelected = "selected"
)

// MaxOrgSecrets bounds one organization's secret document. The document is read
// once per trusted claim, so its size is a claim-path cost.
const MaxOrgSecrets = 100

// repoNameRe is one repository-name part (the git id rule: ASCII letters,
// digits, '.', '_' and '-', 1..100 chars, no leading dot).
var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,99}$`)

// MaxOrgSecretRepos bounds the selection list of one "selected" secret.
const MaxOrgSecretRepos = 100

// Audit action verbs for the organization secret surface.
const (
	AuditOrgSecretSet    = "org_secret_set"
	AuditOrgSecretDelete = "org_secret_delete"
)

// OrgSecretDoc is orgs/<org>/threads/secrets.json.
type OrgSecretDoc struct {
	Values    map[string]OrgSecretValue `json:"values"`
	UpdatedAt string                    `json:"updated_at"`
	Version   int                       `json:"version"`
}

// OrgSecretValue is one sealed organization secret plus its repository policy.
type OrgSecretValue struct {
	Name string `json:"name"`
	// Sealed is base64(nonce || ciphertext) — never the value.
	Sealed    string `json:"sealed"`
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// Visibility is OrgSecretAll or OrgSecretSelected. Empty reads as "all"
	// (the safe-for-existing-records interpretation, and the API default).
	Visibility string `json:"visibility,omitempty"`
	// Repositories are the repository names (relative to the organization) a
	// "selected" secret is scoped to. Ignored for "all".
	Repositories []string `json:"repositories,omitempty"`
}

// OrgSecretView is the wire/page shape: name, policy and timestamps, never a
// value.
type OrgSecretView struct {
	Name         string   `json:"name"`
	Visibility   string   `json:"visibility"`
	Repositories []string `json:"repositories"`
	CreatedBy    string   `json:"created_by,omitempty"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
}

// OrgSecretScope is the policy half of a write: who may read the secret.
type OrgSecretScope struct {
	// Visibility is "all" (default) or "selected".
	Visibility string
	// Repositories are the selected repositories ("name" or "owner/name").
	Repositories []string
}

// OrgSecretsKey returns orgs/<org>/threads/secrets.json.
func OrgSecretsKey(org string) string { return "orgs/" + org + "/threads/secrets.json" }

// normalizeOrgSecretScope validates and normalizes a policy. An empty
// visibility is "all"; "selected" requires at least one repository and refuses
// anything that is not an identifier or `owner/name`.
func normalizeOrgSecretScope(org string, in OrgSecretScope) (string, []string, error) {
	visibility := strings.ToLower(strings.TrimSpace(in.Visibility))
	if visibility == "" {
		visibility = OrgSecretAll
	}
	switch visibility {
	case OrgSecretAll:
		return visibility, nil, nil
	case OrgSecretSelected:
	default:
		return "", nil, fmt.Errorf("%w: organization secret visibility must be all|selected, got %q", ErrInvalid, in.Visibility)
	}
	if len(in.Repositories) == 0 {
		return "", nil, fmt.Errorf("%w: a selected organization secret needs at least one repository", ErrInvalid)
	}
	if len(in.Repositories) > MaxOrgSecretRepos {
		return "", nil, fmt.Errorf("%w: at most %d repositories per secret", ErrInvalid, MaxOrgSecretRepos)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in.Repositories))
	for _, raw := range in.Repositories {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		owner, name := org, entry
		if i := strings.IndexByte(entry, '/'); i >= 0 {
			owner, name = entry[:i], entry[i+1:]
		}
		if !strings.EqualFold(owner, org) {
			return "", nil, fmt.Errorf("%w: repository %q is not owned by organization %q", ErrInvalid, entry, org)
		}
		if !repoNameRe.MatchString(name) {
			return "", nil, fmt.Errorf("%w: %q is not a repository name", ErrInvalid, entry)
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return "", nil, fmt.Errorf("%w: a selected organization secret needs at least one repository", ErrInvalid)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return visibility, out, nil
}

// selectsRepo reports whether a policy covers one repository of its org.
func selectsRepo(visibility string, repositories []string, repo string) bool {
	switch strings.ToLower(strings.TrimSpace(visibility)) {
	case "", OrgSecretAll:
		return true
	case OrgSecretSelected:
		for _, candidate := range repositories {
			if strings.EqualFold(strings.TrimSpace(candidate), repo) {
				return true
			}
		}
		return false
	default:
		// An unknown policy value fails closed: a record this build cannot
		// interpret must never widen a secret's reach.
		return false
	}
}

// SetOrgSecret stores (or replaces) one organization secret (org owner or host
// admin, write-only). The audit entry names the secret and its policy, never a
// value.
func (s *Service) SetOrgSecret(ctx context.Context, org, name, value string, in OrgSecretScope, p auth.Principal) error {
	org = strings.TrimSpace(org)
	if org == "" {
		return fmt.Errorf("%w: organization is required", ErrInvalid)
	}
	if err := s.requireOrgAdmin(ctx, org, p); err != nil {
		return err
	}
	name, err := normalizeSecretName(name)
	if err != nil {
		return err
	}
	if !s.SecretsEnabled() {
		return fmt.Errorf("%w: organization secrets need server.secrets_key or server.auth.session_secret", ErrUnavailable)
	}
	if len(value) == 0 || len(value) > 64<<10 {
		return fmt.Errorf("%w: secret value must be 1 byte–64 KiB", ErrInvalid)
	}
	visibility, repos, serr := normalizeOrgSecretScope(org, in)
	if serr != nil {
		return serr
	}
	sealed, eerr := s.sealSecret("org:"+org, name, value)
	if eerr != nil {
		return eerr
	}
	now := s.nowUTC().Format(dateTimeFmt)
	_, err = s.casUpdate(ctx, OrgSecretsKey(org), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		doc := &OrgSecretDoc{Values: map[string]OrgSecretValue{}}
		if cur != nil {
			if jerr := json.Unmarshal(cur, doc); jerr != nil {
				return nil, false, fmt.Errorf("%w: organization secrets: %v", ErrCorrupt, jerr)
			}
		}
		if doc.Values == nil {
			doc.Values = map[string]OrgSecretValue{}
		}
		entry, ok := doc.Values[name]
		if !ok {
			if len(doc.Values) >= MaxOrgSecrets {
				return nil, false, fmt.Errorf("%w: at most %d organization secrets", ErrInvalid, MaxOrgSecrets)
			}
			entry = OrgSecretValue{Name: name, CreatedAt: now}
		}
		entry.Sealed = sealed
		entry.CreatedBy = truncate(strings.ToLower(strings.TrimSpace(p.Name)), 100)
		entry.UpdatedAt = now
		entry.Visibility = visibility
		entry.Repositories = repos
		doc.Values[name] = entry
		doc.UpdatedAt = now
		doc.Version++
		return mustJSON(doc), true, nil
	})
	if err == nil {
		s.auditAdmin(ctx, p.Name, AuditOrgSecretSet, "org:"+org,
			"secret="+name+" visibility="+visibility+" repos="+fmt.Sprint(len(repos)))
	}
	return err
}

// ListOrgSecrets lists an organization's secret names and policies (org owner
// only; values are never returned).
func (s *Service) ListOrgSecrets(ctx context.Context, org string, p auth.Principal) ([]OrgSecretView, error) {
	org = strings.TrimSpace(org)
	if err := s.requireOrgAdmin(ctx, org, p); err != nil {
		return nil, err
	}
	doc, err := s.loadOrgSecrets(ctx, org)
	if err != nil {
		return nil, err
	}
	out := []OrgSecretView{}
	if doc == nil {
		return out, nil
	}
	names := make([]string, 0, len(doc.Values))
	for name := range doc.Values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := doc.Values[name]
		visibility := strings.ToLower(strings.TrimSpace(entry.Visibility))
		if visibility == "" {
			visibility = OrgSecretAll
		}
		repos := []string{}
		if visibility == OrgSecretSelected {
			repos = append(repos, entry.Repositories...)
		}
		out = append(out, OrgSecretView{
			Name: entry.Name, Visibility: visibility, Repositories: repos,
			CreatedBy: entry.CreatedBy, CreatedAt: entry.CreatedAt, UpdatedAt: entry.UpdatedAt,
		})
	}
	return out, nil
}

// DeleteOrgSecret removes one organization secret (absent is not an error).
func (s *Service) DeleteOrgSecret(ctx context.Context, org, name string, p auth.Principal) error {
	org = strings.TrimSpace(org)
	if err := s.requireOrgAdmin(ctx, org, p); err != nil {
		return err
	}
	name, err := normalizeSecretName(name)
	if err != nil {
		return err
	}
	_, err = s.casUpdate(ctx, OrgSecretsKey(org), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		doc := &OrgSecretDoc{Values: map[string]OrgSecretValue{}}
		if jerr := json.Unmarshal(cur, doc); jerr != nil {
			return nil, false, fmt.Errorf("%w: organization secrets: %v", ErrCorrupt, jerr)
		}
		if _, ok := doc.Values[name]; !ok {
			return nil, false, nil
		}
		delete(doc.Values, name)
		doc.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		doc.Version++
		return mustJSON(doc), true, nil
	})
	if err == nil {
		s.auditAdmin(ctx, p.Name, AuditOrgSecretDelete, "org:"+org, "secret="+name)
	}
	return err
}

// loadOrgSecrets reads one organization's secret document (one exact-key GET;
// nil when none exists).
func (s *Service) loadOrgSecrets(ctx context.Context, org string) (*OrgSecretDoc, error) {
	raw, _, err := s.getJSON(ctx, OrgSecretsKey(org))
	if err != nil || raw == nil {
		return nil, err
	}
	var doc OrgSecretDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		return nil, fmt.Errorf("%w: organization secrets: %v", ErrCorrupt, jerr)
	}
	return &doc, nil
}

// orgSecretValues decrypts the organization secrets this repository's policy
// admits (one GET, no LIST). A single unreadable envelope fails the claim
// closed: a job that silently lost a credential is worse than a named error.
func (s *Service) orgSecretValues(ctx context.Context, owner, repo string) (map[string]string, error) {
	out := map[string]string{}
	if !s.SecretsEnabled() {
		return out, nil
	}
	doc, err := s.loadOrgSecrets(ctx, owner)
	if err != nil || doc == nil {
		return out, err
	}
	for _, entry := range doc.Values {
		if !selectsRepo(entry.Visibility, entry.Repositories, repo) {
			continue
		}
		value, oerr := s.openSecret("org:"+owner, entry.Name, entry.Sealed)
		if oerr != nil {
			return nil, oerr
		}
		out[entry.Name] = value
	}
	return out, nil
}
