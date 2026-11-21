package packages

import "strings"

// OCI Distribution Spec v1.1 error codes (docs/features/14 §3). Only the
// documented subset is used; a registry that invents codes breaks clients.
const (
	CodeUnauthorized      = "UNAUTHORIZED"
	CodeDenied            = "DENIED"
	CodeNameUnknown       = "NAME_UNKNOWN"
	CodeManifestUnknown   = "MANIFEST_UNKNOWN"
	CodeBlobUnknown       = "BLOB_UNKNOWN"
	CodeDigestInvalid     = "DIGEST_INVALID"
	CodeBlobUploadUnknown = "BLOB_UPLOAD_UNKNOWN"
	CodeSizeInvalid       = "SIZE_INVALID"
	CodeUnknown           = "UNKNOWN"
)

// errorBody is the spec error envelope: {"errors":[{"code","message","detail"}]}.
type errorBody struct {
	Errors []errorEntry `json:"errors"`
}

type errorEntry struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

func bodyOf(e *Error) errorBody {
	return errorBody{Errors: []errorEntry{{
		Code:    e.Code,
		Message: e.Message,
		Detail:  e.Detail,
	}}}
}

// scope is one Docker token scope (repository:<owner>/<repo>:<actions>).
type scope struct {
	Kind  string // "repository"
	Name  string // "owner/repo"
	acted map[string]bool
}

// parseScope parses "repository:o/r:pull,push". An unparseable scope is
// ignored by the token issuer (Docker tolerates a reduced grant).
func parseScope(s string) (scope, bool) {
	kind, rest, ok := strings.Cut(s, ":")
	if !ok || kind != "repository" {
		return scope{}, false
	}
	name, actions, ok := strings.Cut(rest, ":")
	if !ok || name == "" {
		return scope{}, false
	}
	sc := scope{Kind: kind, Name: name, acted: map[string]bool{}}
	for _, a := range strings.Split(actions, ",") {
		if a = strings.TrimSpace(a); a != "" {
			sc.acted[a] = true
		}
	}
	return sc, true
}

// String renders the canonical scope spelling used in challenges and tokens.
func (s scope) String() string {
	acts := make([]string, 0, 2)
	for _, a := range []string{"pull", "push"} {
		if s.acted[a] {
			acts = append(acts, a)
		}
	}
	return s.Kind + ":" + s.Name + ":" + strings.Join(acts, ",")
}

// grants reports whether the scope grants action on repo.
func (s scope) grants(repo, action string) bool {
	return s.Name == repo && s.acted[action]
}
