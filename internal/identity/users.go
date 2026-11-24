// users.go — the username registry (Forgejo #370).
//
// Every OIDC principal owns an immutable username, derived at first
// login from the verified email's local part and uniquified on
// collision (crueber, crueber2, …). The mapping lives on the bucket
// (law 4 — memory is a cache, the bucket is truth):
//
//	users/<username>/user.json              the binding {username, email}
//	users/by-email/<encoded-email>/ref.json alias {username, email} (fast path)
//
// The alias makes repeat logins cost one exact-key GET (law 6 — probe,
// don't list); the user.json CAS is the only commit point for
// collision-uniqueness (law 4: two first-logins racing the same base
// resolve by PutCreate — the loser reads the winner and moves to the
// next suffix). Both keys are additive (law 5): no existing key shape
// changes, and every pre-#370 spelling (email subjects, rosters,
// profiles) keeps validating — matchPrincipal is the read-side alias.
//
// ### Concurrency
// Hazard: two logins racing ResolveUsername for distinct emails sharing
// one base. Avoidance: PutCreate per candidate; 412 means someone won
// the name — read the winner (same email → adopt + repair alias;
// foreign email → next suffix). No lock is held across any store call.
package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// UserDoc is users/<username>/user.json: the immutable username ↔
// verified-email binding. Version is the CAS token.
//
// The disablement fields are the Administration area's directory decision
// (docs/PLAN-ADMIN.md §3.7): the flag lives HERE, on the credential
// binding, not on profile.json — a profile is public-facing display state
// (display name, bio, avatar) and a credential fact must not ride a
// document the profile page reads. Every field is append-only (law 5):
// old readers ignore them, old writers never emit them, and a cleared
// disablement writes zero values back so an active account's bytes match
// its pre-field shape.
type UserDoc struct {
	Version  int    `json:"version"`
	Username string `json:"username"`
	Email    string `json:"email"`
	// CreatedAt is RFC3339.
	CreatedAt string `json:"created_at"`

	// Disabled is the deactivate flag enforced at the credential boundary
	// (internal/server/auth.go accountGate): a disabled account cannot
	// authenticate with any OIDC-derived credential.
	Disabled bool `json:"disabled,omitempty"`
	// DisabledBy / DisabledAt / Reason record the transition (the audit
	// family carries the durable record; this is the account's own state,
	// so the directory can explain a row after an audit page is gone).
	DisabledBy string `json:"disabled_by,omitempty"`
	DisabledAt string `json:"disabled_at,omitempty"`
	Reason     string `json:"reason,omitempty"`
	// TokensRevokedAt is the RFC3339 watermark deactivation set: every
	// wgt_ token AND browser session minted at or before it is refused,
	// permanently. Reactivation clears Disabled and keeps this — revoked
	// credentials are never restored (the user signs in again).
	TokensRevokedAt string `json:"tokens_revoked_at,omitempty"`
}

// AliasDoc is users/by-email/<encoded-email>/ref.json: the login fast
// path from verified email to username. Same content as the user doc's
// key pair; repaired lazily when a user.json exists without one.
type AliasDoc struct {
	Version  int    `json:"version"`
	Username string `json:"username"`
	Email    string `json:"email"`
}

// UserKey returns users/<username>/user.json. Usernames are key-safe
// (ValidUsername charset), so no encoding is needed.
func UserKey(username string) string {
	return "users/" + strings.ToLower(strings.TrimSpace(username)) + "/user.json"
}

// EmailAliasKey returns users/by-email/<encoded-email>/ref.json, reusing
// the principal segment encoding (@ → %40, one segment).
func EmailAliasKey(email string) string {
	return "users/by-email/" + encodePrincipal(email) + "/ref.json"
}

func encodeUserDoc(d *UserDoc) []byte {
	raw, _ := json.Marshal(d)
	return raw
}

func parseUserDoc(raw []byte) (*UserDoc, error) {
	var d UserDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("%w: user.json: %v", ErrInvalid, err)
	}
	return &d, nil
}

func encodeAliasDoc(d *AliasDoc) []byte {
	raw, _ := json.Marshal(d)
	return raw
}

func parseAliasDoc(raw []byte) (*AliasDoc, error) {
	var d AliasDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("%w: ref.json: %v", ErrInvalid, err)
	}
	return &d, nil
}

// matchPrincipal reports whether a stored principal spelling refers to
// p: the stored value matches either the username or the verified
// email. This is the read-side migration alias — pre-#370 state keyed
// by email (rosters, access subjects, invite subjects, team members)
// keeps resolving for username principals, and new username-keyed
// state resolves for email-carrying credentials, with no extra store
// round trip (pure comparison, safe on push hot paths).
func matchPrincipal(stored string, p auth.Principal) bool {
	s := normPrincipal(stored)
	if s == "" {
		return false
	}
	if s == normPrincipal(p.Name) {
		return true
	}
	return p.Email != "" && s == normPrincipal(p.Email)
}

// ResolveUsername maps a verified email to its immutable username,
// creating the binding on first login. Repeat logins cost one alias
// GET; first login costs one alias GET plus one GET + one PutCreate per
// collision candidate. A store outage fails closed with the error (the
// caller falls back to the pure derivation base, which still satisfies
// "no @ in the principal name").
func (s *Service) ResolveUsername(ctx context.Context, email string) (string, error) {
	email = normPrincipal(email)
	if email == "" {
		return "", fmt.Errorf("%w: empty email", ErrInvalid)
	}
	if raw, _, err := store.GetBytes(ctx, s.Store, EmailAliasKey(email), store.GetOptions{}); err == nil {
		if a, perr := parseAliasDoc(raw); perr == nil && normPrincipal(a.Email) == email && auth.ValidUsername(a.Username) {
			return normPrincipal(a.Username), nil
		}
	} else if !store.IsNotFound(err) {
		return "", err
	}
	base := auth.DeriveUsername(email)
	now := s.nowUTC().Format(time.RFC3339)
	if now == "" {
		now = time.Now().UTC().Format(time.RFC3339)
	}
	for i := 0; i < 100; i++ {
		cand := auth.WithCollisionSuffix(base, i)
		raw, _, gerr := store.GetBytes(ctx, s.Store, UserKey(cand), store.GetOptions{})
		if gerr != nil && !store.IsNotFound(gerr) {
			return "", gerr
		}
		if gerr == nil {
			// Name taken: same email → adopt (repair the alias);
			// foreign email → next suffix.
			if u, perr := parseUserDoc(raw); perr == nil && normPrincipal(u.Email) == email {
				s.ensureAlias(ctx, email, cand)
				return normPrincipal(u.Username), nil
			}
			continue
		}
		u := &UserDoc{Version: 1, Username: cand, Email: email, CreatedAt: now}
		if _, perr := store.PutBytes(ctx, s.Store, UserKey(cand), encodeUserDoc(u),
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); perr != nil {
			if !store.IsPreconditionFailed(perr) {
				return "", perr
			}
			// Lost the race for cand: re-read the winner IN THIS
			// PASS (not the next suffix — the winner holds THIS
			// name). Same email → adopt it (repair the alias);
			// foreign email → next suffix. Without the re-read,
			// two concurrent first-logins for one email would
			// claim two usernames (split identity).
			raw, _, rerr := store.GetBytes(ctx, s.Store, UserKey(cand), store.GetOptions{})
			if rerr != nil {
				if store.IsNotFound(rerr) {
					continue
				}
				return "", rerr
			}
			if w, werr := parseUserDoc(raw); werr == nil && normPrincipal(w.Email) == email {
				s.ensureAlias(ctx, email, cand)
				return normPrincipal(w.Username), nil
			}
			continue
		}
		s.ensureAlias(ctx, email, cand)
		return cand, nil
	}
	return "", fmt.Errorf("%w: username space exhausted for %q", ErrConflict, email)
}

// ensureAlias best-effort writes the email → username alias (PutCreate;
// 412 means someone already wrote it — adopt, never overwrite).
func (s *Service) ensureAlias(ctx context.Context, email, username string) {
	a := &AliasDoc{Version: 1, Username: username, Email: email}
	_, _ = store.PutBytes(ctx, s.Store, EmailAliasKey(email), encodeAliasDoc(a),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
}

// userExists probes users/<username>/user.json (one exact-key GET): the
// "is this a real user" verdict. A username with no binding is
// unclaimed (or a foreign namespace) — synthesis and self-grants must
// not manufacture authority for it. Absent or error reads as non-user
// (fail closed). Head's absent contract is (nil, nil), like orgExists.
func (s *Service) userExists(ctx context.Context, username string) bool {
	ok, err := s.accountTrace(ctx, username)
	return err == nil && ok
}

// accountTrace is the ONE claimed-user predicate: a users/<u>/user.json (the
// OIDC username registry) or a users/<u>/profile.json (created lazily on first
// authenticated access or first role grant) is an account trace. Token-mode
// principals have only the profile, so probing user.json alone made their own
// repositories unadministrable (the audit's HIGH bug) — and a rebase dropped
// this helper once, which the owner-predicate test now pins. Unknown and
// invalid namespaces are (false, nil); a store error surfaces so callers can
// fail closed.
func (s *Service) accountTrace(ctx context.Context, username string) (bool, error) {
	if s == nil || s.Store == nil {
		return false, nil
	}
	username = normPrincipal(username)
	if !auth.ValidUsername(username) {
		return false, nil
	}
	for _, key := range []string{UserKey(username), ProfileKey(username)} {
		meta, err := s.Store.Head(ctx, key)
		if err != nil {
			return false, err
		}
		if meta != nil {
			return true, nil
		}
	}
	return false, nil
}

// UserExists reports whether username is a claimed identity: a users/<u>/
// user.json binding (the OIDC username registry) OR a users/<u>/profile.json
// (created lazily on first authenticated access / first role grant). Both
// are account traces; a namespace with neither is unknown, not empty. It is
// the exported page-side twin of the fail-closed userExists probe: unlike
// that probe it returns the store error, so an owner-scoped read page can
// answer 503 for a bucket fault instead of pretending the user does not
// exist. An invalid/normalising-empty username is (false, nil) — a
// syntactically impossible identity, not a fault.
func (s *Service) UserExists(ctx context.Context, username string) (bool, error) {
	if s == nil || s.Store == nil {
		return false, nil
	}
	username = normPrincipal(username)
	if !auth.ValidUsername(username) {
		return false, nil
	}
	return s.accountTrace(ctx, username)
}

// EmailForUsername returns the verified email bound to username, or ""
// when no binding exists. Powers the "own email on own profile only"
// surface (the GET /users/{u} self view) without ever exposing other
// users' addresses.
func (s *Service) EmailForUsername(ctx context.Context, username string) (string, error) {
	username = normPrincipal(username)
	raw, _, err := store.GetBytes(ctx, s.Store, UserKey(username), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	u, perr := parseUserDoc(raw)
	if perr != nil {
		return "", perr
	}
	if normPrincipal(u.Username) != username {
		return "", nil
	}
	return normPrincipal(u.Email), nil
}
