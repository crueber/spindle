// Package auth: shared identity types (06_server_http.md §8.8). The server owner
// implements the full auth chain in sibling files; server/api handlers call these helpers.
package auth

// Principal is the authenticated identity. Admin (repo delete, PUT/DELETE settings+policy)
// is INDEPENDENT of write (push + create).
type Principal struct {
	Name string
	// Email is the verified OIDC email behind the username Name
	// (Forgejo #370): empty for static-token, anonymous, and
	// auth-none principals. Carried so alias matching (stored email
	// spellings) and token/session mints (which keep the email on
	// the wire) work without another lookup. Never rendered to
	// other users — Name is the only user-facing identity.
	Email string
	// Groups are the verified OIDC group claim values behind the
	// principal (WP13). They are captured from the signature-verified ID
	// token and ride walgit-issued sessions/tokens so the per-request
	// admin derivation can re-apply the configured admin_groups policy.
	// Empty for static-token, anonymous, and auth-none principals.
	Groups    []string
	Write     bool
	Admin     bool
	Anonymous bool
	// JobRepo marks a repository-scoped job token (15 §5, Threads): the
	// credential may READ exactly this repository ("owner/name") and, when
	// JobPush is set, push to it — nothing else, in no other repository. It is
	// set only by the wjt_ attempt-token resolver, which derives JobPush from
	// the workflow's `permissions: contents: write`.
	JobRepo string
	JobPush bool
}

// Anonymous is the unauthenticated principal (name "anonymous", no write, no admin).
func Anonymous() Principal { return Principal{Name: "anonymous", Anonymous: true} }

// None is the auth-mode-none principal: everyone is anon with write+admin.
func None() Principal { return Principal{Name: "anon", Write: true, Admin: true} }

// AuthError maps to HTTP: Invalid/Unauthorized → 401 (+WWW-Authenticate Bearer),
// Forbidden → 403, Unavailable → 503 (+Retry-After: 15).
type AuthError struct {
	Kind AuthErrorKind
	Why  string
}

type AuthErrorKind int

const (
	ErrUnauthorized AuthErrorKind = iota
	ErrInvalid
	ErrForbidden
	ErrUnavailable
)

func (e *AuthError) Error() string { return e.Why }

// Require helpers (doc 06 §8.8): implemented by the server owner; contract shapes here.
type Chain struct{}

// RequireRead: anonymous && !anonymous_read → ErrUnauthorized.
func (c *Chain) RequireRead(p Principal) *AuthError { panic("unimplemented") }

// RequireWrite: !p.Write → ErrForbidden.
func (c *Chain) RequireWrite(p Principal) *AuthError { panic("unimplemented") }

// RequireAdmin: !p.Admin → ErrForbidden.
func (c *Chain) RequireAdmin(p Principal) *AuthError { panic("unimplemented") }
