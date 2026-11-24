// account.go — the account-state seam of the credential boundary
// (docs/PLAN-ADMIN.md §3.7, the instance directory's deactivate decision).
//
// Deactivation is a CREDENTIAL fact, not a page behaviour: after OIDC
// verification and username resolution the gate below refuses a disabled
// account, and it refuses every credential minted at or before the
// account's revocation watermark. It lives in this package because every
// OIDC-derived credential passes through AuthService — the ID token, the
// wgt_ access token, the browser session cookie, a forwarded identity and
// SSH key auth — so one gate covers them all and no page can leak what the
// boundary protects.
//
// Machine credentials (server.auth.tokens and the ExtraCredential shapes)
// are configuration entries, not user-bound accounts: they never reach this
// gate and deactivation never touches them.
package auth

import "time"

// AccountState is one account's directory state, read from
// users/<username>/user.json by the identity service and memoized there
// (bounded LRU, 10 s TTL; see internal/identity/account_cache.go).
type AccountState struct {
	// Disabled refuses every OIDC-derived credential for this account.
	Disabled bool
	// DisabledBy/DisabledAt/Reason explain the transition to an
	// administrator; the refusal message itself never discloses them.
	DisabledBy string
	DisabledAt string
	Reason     string
	// TokensRevokedAt is the deactivation watermark: a session or wgt_
	// token minted at or before it is refused, permanently (reactivation
	// does not restore it). Zero means no watermark.
	TokensRevokedAt time.Time
}
