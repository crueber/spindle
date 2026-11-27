package workflow

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Permission scopes (PAR-112, decision D-11).
//
// `permissions:` is a closed vocabulary, not an open map. A scope we cannot
// enforce is refused **by name at parse time** — an accepted-but-inert key is
// the bug this type exists to prevent (an author would believe the job token
// gained an authority it does not have). The supported set is exactly the
// authority the job token actually carries:
//
//	contents       read|write   git read/push of the run's repository
//	issues         read|write   the repository's issue surface (write = triage)
//	pull-requests  read|write   the pull-request surface (write = write role)
//	id-token       write|none   exchange for a short-lived OIDC ID token
//
// `none` is accepted for every scope (an explicit denial is never an error).
// `read-all`/`write-all` are accepted as the scalar shorthand and as the
// `all` key; the baseline they set never includes `id-token` (docs/threads.md
// §5.3, S10).
const (
	ScopeContents    = "contents"
	ScopeIssues      = "issues"
	ScopePullRequest = "pull-requests"
	ScopeIDToken     = "id-token"
	// ScopeDeployments is the deployment API (PAR-133): `write` creates a
	// deployment and its statuses for the run's repository; `read` lists them.
	// It never grants the Threads operator surface (dispatch/cancel/approve).
	ScopeDeployments = "deployments"
	// KeyAll is the shorthand baseline key produced by `read-all`/`write-all`.
	KeyAll = "all"
)

// permissionScope is one supported scope and its accepted levels.
type permissionScope struct {
	// levels is the closed level set for the scope, canonicalized.
	levels []string
	// surface names the enforced capability in one phrase (docs + errors).
	surface string
}

var permissionScopes = map[string]permissionScope{
	ScopeContents:    {levels: []string{"read", "write"}, surface: "git read/push of the run's repository"},
	ScopeIssues:      {levels: []string{"read", "write"}, surface: "the repository's issue surface (write = triage)"},
	ScopePullRequest: {levels: []string{"read", "write"}, surface: "the pull-request surface (write = write role)"},
	ScopeDeployments: {levels: []string{"read", "write"}, surface: "the deployment API (write = create deployments and statuses)"},
	ScopeIDToken:     {levels: []string{"write"}, surface: "OIDC ID-token exchange (trusted runs only)"},
}

// permissionAliases accepts the underscore spelling some CI files use.
var permissionAliases = map[string]string{
	"pull_requests": ScopePullRequest,
	"pull-request":  ScopePullRequest,
}

// refusedScopes are keys a CI author may reasonably write that this server has
// no enforced surface for. They are refused by name with the reason, so the
// failure mode is an error that says what is missing — never a silent no-op.
var refusedScopes = map[string]string{
	"actions":             "the run cancel/rerun API is not a job-token capability",
	"attestations":        "artifact attestations are a non-goal (D-15)",
	"checks":              "the check-run API accepts a dedicated CI token, not the job token",
	"discussions":         "there is no discussion surface",
	"models":              "there is no model-registry surface",
	"packages":            "the OCI registry authenticates a registry token, not the job token",
	"pages":               "there is no Pages deployment surface",
	"repository-projects": "there is no project-board surface",
	"security-events":     "there is no code-scanning ingestion surface",
	"statuses":            "commit statuses are reported by the dedicated CI token",
	"wiki":                "there is no wiki surface",
	"metadata":            "metadata is always readable and cannot be requested",
}

// SupportedPermissionScopes returns the canonical supported scope names, sorted
// (docs and error messages share this one list).
func SupportedPermissionScopes() []string {
	out := make([]string, 0, len(permissionScopes))
	for k := range permissionScopes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CanonicalPermissionScope maps a written key onto its canonical scope name.
// ok=false means the key is not a supported scope; why explains the refusal.
func CanonicalPermissionScope(key string) (canonical string, ok bool, why string) {
	k := strings.ToLower(strings.TrimSpace(key))
	if alias, found := permissionAliases[k]; found {
		return alias, true, ""
	}
	if _, found := permissionScopes[k]; found {
		return k, true, ""
	}
	if reason, found := refusedScopes[k]; found {
		return "", false, fmt.Sprintf("scope %q is not implemented by this server (%s); a scope with no capability is refused by name", k, reason)
	}
	return "", false, fmt.Sprintf("unknown scope %q", k)
}

// ValidatePermissionLevel checks one written level against a canonical scope.
func ValidatePermissionLevel(scope, level string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(level))
	if v == "none" {
		return v, nil
	}
	spec, ok := permissionScopes[scope]
	if !ok {
		return "", fmt.Errorf("unknown scope %q", scope)
	}
	for _, accepted := range spec.levels {
		if v == accepted {
			return v, nil
		}
	}
	if v == "read-all" || v == "write-all" {
		return "", fmt.Errorf("level %q is only valid as the whole `permissions:` value, not per scope", v)
	}
	return "", fmt.Errorf("level %q is not valid for %q (%s)", v, scope, strings.Join(append(append([]string{}, spec.levels...), "none"), ", "))
}

// Permissions is the GITHUB_TOKEN permission map. The scalar shorthand
// (`read-all` / `write-all`) is normalized to a single "all" entry; keys and
// levels are validated against the supported scope table above.
type Permissions map[string]string

// UnmarshalYAML accepts the scalar shorthand and the mapping form, refusing an
// unsupported scope or level by name.
func (p *Permissions) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if isNull(node) {
			*p = nil
			return nil
		}
		v := strings.ToLower(strings.TrimSpace(node.Value))
		switch v {
		case "read-all", "write-all":
			*p = Permissions{KeyAll: v}
			return nil
		}
		return fmt.Errorf("permissions: %q is not a permission shorthand (use read-all or write-all, or a mapping of scopes)", node.Value)
	case yaml.MappingNode:
		out := make(Permissions, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			val := node.Content[i+1]
			if val.Kind != yaml.ScalarNode || isNull(val) {
				return fmt.Errorf("permissions.%s must be a level (read, write, none)", key)
			}
			if key == KeyAll {
				v := strings.ToLower(strings.TrimSpace(val.Value))
				if v != "read-all" && v != "write-all" {
					return fmt.Errorf("permissions.all: %q is not a permission shorthand (use read-all or write-all)", val.Value)
				}
				out[KeyAll] = v
				continue
			}
			canonical, ok, why := CanonicalPermissionScope(key)
			if !ok {
				return fmt.Errorf("permissions.%s: %s (supported: %s)", key, why,
					strings.Join(SupportedPermissionScopes(), ", "))
			}
			level, lerr := ValidatePermissionLevel(canonical, val.Value)
			if lerr != nil {
				return fmt.Errorf("permissions.%s: %v", key, lerr)
			}
			out[canonical] = level
		}
		*p = out
		return nil
	default:
		return fmt.Errorf("permissions: expected a mapping or read-all/write-all")
	}
}

// IsZeroPermissionKey reports whether a permission-map key is the shorthand
// baseline rather than a scope (the importer and the effective-permission
// resolver both need this distinction).
func IsZeroPermissionKey(key string) bool { return key == KeyAll }
