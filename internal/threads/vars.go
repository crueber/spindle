package threads

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Variables (PAR-035). A variable is a non-secret configuration value with the
// same storage and redaction posture as a secret: the value is sealed with the
// instance keyring and is never returned by the API (a list reports names and
// timestamps), and the runner masks every value in the log stream. That is
// stricter than GitHub's variables (which are readable), and it is deliberate:
// this product's operator asked for the secret posture, so a variable cannot
// become a credential leak by being pasted into `${{ vars.TOKEN }}`.
//
// Scopes and precedence: repository variables override organization variables
// of the same name. There is no user scope (a workflow runs as the repository,
// not a person) and no environment scope yet; both would need a schema
// decision, and the two implemented scopes cover the documented acceptance.
//
// Storage (one exact-key GET per scope, never a LIST on the job path):
//
//	repos/<o>/<r>/threads/vars.json      repository variables
//	orgs/<org>/threads/vars.json        organization variables
//
// Resolution happens when a trusted job is claimed, exactly like secrets; an
// untrusted (fork) job gets none.

// VariableDoc is repos/<o>/<r>/threads/vars.json (or the org twin).
type VariableDoc struct {
	Values    map[string]VariableValue `json:"values"`
	UpdatedAt string                   `json:"updated_at"`
	Version   int                      `json:"version"`
}

// VariableValue is one sealed variable (name + envelope + timestamps).
type VariableValue struct {
	Name      string `json:"name"`
	Sealed    string `json:"sealed"`
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// VariableView is the wire shape: names and timestamps, never a value.
type VariableView struct {
	Name      string `json:"name"`
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// VarsKey is the repository variable document.
func VarsKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/threads/vars.json"
}

// OrgVarsKey is the organization variable document.
func OrgVarsKey(org string) string { return "orgs/" + org + "/threads/vars.json" }

// variableAAD binds one ciphertext to its scope and name.
func variableAAD(scope, name string) []byte {
	return []byte("spindle/threads/variable/v1\x00" + scope + "\x00" + name)
}

// normalizeVariableName validates a variable name. The reserved prefixes name
// the platform's own namespace: a variable that could shadow GITHUB_* or
// SPINDLE_* would make a workflow read an operator value where it expects the
// run's identity, so it is refused at save.
func normalizeVariableName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !secretNameRe.MatchString(name) {
		return "", fmt.Errorf("%w: variable name must match [A-Za-z_][A-Za-z0-9_]{0,63}", ErrInvalid)
	}
	upper := strings.ToUpper(name)
	for _, reserved := range []string{"GITHUB_", "SPINDLE_", "FORGEJO_", "RUNNER_"} {
		if strings.HasPrefix(upper, reserved) {
			return "", fmt.Errorf("%w: variable name %q uses the reserved prefix %s", ErrInvalid, name, reserved)
		}
	}
	return name, nil
}

// sealVariable encrypts one value, binding it to (scope, name).
func (s *Service) sealVariable(scope, name, value string) (string, error) {
	if !s.keys.SecretsEnabled() {
		return "", fmt.Errorf("%w: variables need server.secrets_key or server.auth.session_secret", ErrUnavailable)
	}
	gcm := gcmFor(s.keys.Seal)
	nonce, err := randomBytes(gcm.NonceSize())
	if err != nil {
		return "", fmt.Errorf("%w: nonce: %v", ErrUnavailable, err)
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(value), variableAAD(scope, name))), nil
}

// openVariable decrypts one stored value.
func (s *Service) openVariable(scope, name, sealed string) (string, error) {
	if !s.keys.SecretsEnabled() {
		return "", fmt.Errorf("%w: variables need server.secrets_key or server.auth.session_secret", ErrUnavailable)
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("%w: variable %s: bad envelope", ErrCorrupt, name)
	}
	gcm := gcmFor(s.keys.Seal)
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("%w: variable %s: short envelope", ErrCorrupt, name)
	}
	nonce, body := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, oerr := gcm.Open(nil, nonce, body, variableAAD(scope, name))
	if oerr != nil {
		return "", fmt.Errorf("%w: variable %s: cannot be opened (wrong key or corrupt)", ErrCorrupt, name)
	}
	return string(plain), nil
}

// SetVariable stores one repository variable (admin).
func (s *Service) SetVariable(ctx context.Context, repo, name, value string, p auth.Principal) error {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return err
	}
	if err := s.requireRole(ctx, owner, rname, p, "admin"); err != nil {
		return err
	}
	return s.setVariableAt(ctx, VarsKey(owner, rname), "repo:"+repo, name, value, p)
}

// SetOrgVariable stores one organization variable (org owner or host admin).
func (s *Service) SetOrgVariable(ctx context.Context, org, name, value string, p auth.Principal) error {
	if err := s.requireOrgAdmin(ctx, org, p); err != nil {
		return err
	}
	return s.setVariableAt(ctx, OrgVarsKey(org), "org:"+org, name, value, p)
}

func (s *Service) setVariableAt(ctx context.Context, key, scope, name, value string, p auth.Principal) error {
	name, err := normalizeVariableName(name)
	if err != nil {
		return err
	}
	if len(value) == 0 || len(value) > 64<<10 {
		return fmt.Errorf("%w: variable value must be 1 byte–64 KiB", ErrInvalid)
	}
	sealed, serr := s.sealVariable(scope, name, value)
	if serr != nil {
		return serr
	}
	now := s.nowUTC().Format(dateTimeFmt)
	_, err = s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		doc := &VariableDoc{Values: map[string]VariableValue{}}
		if cur != nil {
			if jerr := json.Unmarshal(cur, doc); jerr != nil {
				return nil, false, fmt.Errorf("%w: variables: %v", ErrCorrupt, jerr)
			}
		}
		if doc.Values == nil {
			doc.Values = map[string]VariableValue{}
		}
		entry, ok := doc.Values[name]
		if !ok {
			entry = VariableValue{Name: name, CreatedAt: now}
		}
		entry.Sealed = sealed
		entry.CreatedBy = truncate(strings.ToLower(strings.TrimSpace(p.Name)), 100)
		entry.UpdatedAt = now
		doc.Values[name] = entry
		doc.UpdatedAt = now
		doc.Version++
		return mustJSON(doc), true, nil
	})
	return err
}

// ListVariables lists repository variable names (admin; values never returned).
func (s *Service) ListVariables(ctx context.Context, repo string, p auth.Principal) ([]VariableView, error) {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, rname, p, "admin"); err != nil {
		return nil, err
	}
	return s.listVariablesAt(ctx, VarsKey(owner, rname))
}

// ListOrgVariables lists organization variable names.
func (s *Service) ListOrgVariables(ctx context.Context, org string, p auth.Principal) ([]VariableView, error) {
	if err := s.requireOrgAdmin(ctx, org, p); err != nil {
		return nil, err
	}
	return s.listVariablesAt(ctx, OrgVarsKey(org))
}

func (s *Service) listVariablesAt(ctx context.Context, key string) ([]VariableView, error) {
	out := []VariableView{}
	raw, _, err := s.getJSON(ctx, key)
	if err != nil || raw == nil {
		return out, err
	}
	var doc VariableDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, key, jerr)
	}
	names := make([]string, 0, len(doc.Values))
	for name := range doc.Values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := doc.Values[name]
		out = append(out, VariableView{Name: v.Name, CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt})
	}
	return out, nil
}

// DeleteVariable removes one repository variable (absent is not an error).
func (s *Service) DeleteVariable(ctx context.Context, repo, name string, p auth.Principal) error {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return err
	}
	if err := s.requireRole(ctx, owner, rname, p, "admin"); err != nil {
		return err
	}
	return s.deleteVariableAt(ctx, VarsKey(owner, rname), "repo:"+repo, name)
}

// DeleteOrgVariable removes one organization variable.
func (s *Service) DeleteOrgVariable(ctx context.Context, org, name string, p auth.Principal) error {
	if err := s.requireOrgAdmin(ctx, org, p); err != nil {
		return err
	}
	return s.deleteVariableAt(ctx, OrgVarsKey(org), "org:"+org, name)
}

func (s *Service) deleteVariableAt(ctx context.Context, key, scope, name string) error {
	name, err := normalizeVariableName(name)
	if err != nil {
		return err
	}
	_, err = s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		doc := &VariableDoc{Values: map[string]VariableValue{}}
		if jerr := json.Unmarshal(cur, doc); jerr != nil {
			return nil, false, fmt.Errorf("%w: variables: %v", ErrCorrupt, jerr)
		}
		if _, ok := doc.Values[name]; !ok {
			return nil, false, nil
		}
		delete(doc.Values, name)
		doc.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		doc.Version++
		return mustJSON(doc), true, nil
	})
	return err
}

// requireOrgAdmin enforces org ownership (host admin bypasses).
func (s *Service) requireOrgAdmin(ctx context.Context, org string, p auth.Principal) error {
	if p.Admin {
		return nil
	}
	if p.Anonymous {
		return fmt.Errorf("%w: authentication required", ErrUnauthorized)
	}
	if s.OrgAdmin == nil {
		return fmt.Errorf("%w: organization administration is not wired", ErrUnavailable)
	}
	if aerr := s.OrgAdmin(ctx, org, p); aerr != nil {
		switch aerr.Kind {
		case auth.ErrForbidden:
			return fmt.Errorf("%w: %s", ErrForbidden, aerr.Why)
		case auth.ErrUnavailable:
			return fmt.Errorf("%w: %s", ErrUnavailable, aerr.Why)
		default:
			return fmt.Errorf("%w: %s", ErrUnauthorized, aerr.Why)
		}
	}
	return nil
}

// variableValues resolves a trusted job's variables: organization first, then
// repository (repository wins). Two exact-key GETs on the claim path, no LIST.
func (s *Service) variableValues(ctx context.Context, owner, repo string) (map[string]string, error) {
	if !s.SecretsEnabled() {
		return map[string]string{}, nil
	}
	out := map[string]string{}
	for _, scope := range []struct {
		key, label string
	}{
		{OrgVarsKey(owner), "org:" + owner},
		{VarsKey(owner, repo), "repo:" + owner + "/" + repo},
	} {
		raw, _, err := s.getJSON(ctx, scope.key)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		var doc VariableDoc
		if jerr := json.Unmarshal(raw, &doc); jerr != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, scope.key, jerr)
		}
		for name, entry := range doc.Values {
			val, oerr := s.openVariable(scope.label, name, entry.Sealed)
			if oerr != nil {
				return nil, oerr
			}
			out[name] = val
		}
	}
	return out, nil
}
