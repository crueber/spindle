package threads

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// Token shapes (15 §3, the 12_runner D4 families re-expressed for Threads).
//
//	wrg_<principal>.<id>.<secret>   registration token (one-time, admin-minted)
//	wrt_<principal>.<id>.<secret>   runner token (long-lived, revocable)
//	wjt_<payload>.<sig>             attempt token (short-lived, HMAC, verified locally)
//
// The principal segment is base64url (no padding) so an email principal is one
// token segment; the id segment indexes the credential record directly — a
// bearer can be resolved with ONE exact-key GET and no LIST.
const (
	regTokenPrefix     = "wrg_"
	runnerTokenPrefix  = "wrt_"
	attemptTokenPrefix = "wjt_"
)

// Attempt scopes embedded in a job token. The token is the *only* authority a
// runner has on the job endpoints; each handler checks the scope it needs.
const (
	ScopeLog      = "log"
	ScopeComplete = "complete"
	ScopeCancel   = "cancel"
	// ScopeRead authorises the git checkout (clone/fetch) of the job token.
	ScopeRead = "read"
	// ScopePush authorises git pushes to the run's repository (workflow
	// `permissions: contents: write`, 15 §5). A fork run never gets it.
	ScopePush = "push"
	// ScopeArtifact authorises the job channel's artifact operations: upload a
	// blob / finalize an artifact, and download an artifact of the SAME run
	// (15 §2.5). It is not a repository read and never reaches another run.
	ScopeArtifact = "artifact"
	// ScopeCache authorises the job channel's cache operations (PAR-095). The
	// scope a job may read or write is derived from its run, never from the
	// request, so the token carries no scope choice of its own.
	ScopeCache = "cache"
)

// attemptScopes is the control scope set every attempt token carries (log,
// complete, cancel, artifact, cache) plus the git scopes derived from the job's
// effective permissions. `push` is present only when `contents` resolves to
// write.
func attemptScopes(scopes []string) []string {
	out := []string{ScopeLog, ScopeComplete, ScopeCancel, ScopeRead, ScopeArtifact, ScopeCache}
	for _, sc := range scopes {
		if sc == ScopePush {
			out = append(out, ScopePush)
		}
	}
	return out
}

// --- job permissions → token scopes (15 §5, round-2 pass 2) ----------------

// PermissionValues are the permission levels a job may declare.
const (
	PermNone  = "none"
	PermRead  = "read"
	PermWrite = "write"
)

// DefaultJobPermissions is the instance default when a workflow declares no
// `permissions:` at all (threads.default_permissions; "read" unless configured).
const DefaultJobPermissions = PermRead

// baselineScopes are the scopes `read-all`/`write-all` set a baseline for.
// `id-token` is deliberately absent: write-all must never mint cloud identity
// (S10) — a job asks for it by name. The feature scopes (`deployments`,
// `packages`, `checks`, `statuses`) are absent too: write-all grants no image
// push and no check-run write, so a job that wants either names it.
var baselineScopes = []string{
	workflow.ScopeContents,
	workflow.ScopeIssues,
	workflow.ScopePullRequest,
}

// normalizePerm maps the accepted spellings onto none|read|write.
func normalizePerm(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "write", "write-all":
		return PermWrite
	case "read", "read-all":
		return PermRead
	case "none", "": // an explicit `none` grants nothing
		return PermNone
	}
	return "" // unknown: ignored (fail closed means the default still applies)
}

// EffectivePermissions resolves the permissions a job token is scoped to: the
// job spec's merged map over the instance default. `all`/`read-all`/`write-all`
// set the baseline for every baseline scope (never `id-token`); a specific key
// overrides it. A fork run is ALWAYS `contents: read` — the trust class wins
// over anything the workflow asked for (15 §2/§5: the fork's workflow runs from
// the base branch, untrusted).
//
// The scope vocabulary is closed at parse time (workflow.Permissions); this
// resolver is deliberately defensive anyway, because a spec can be read back
// from the bucket and an unknown level must never widen access.
func (s *Service) EffectivePermissions(spec *Spec, trusted bool) map[string]string {
	return s.EffectivePermissionsFor(spec, trusted, false)
}

// EffectivePermissionsFor is EffectivePermissions with the fork-run write-token
// decision (PAR-118, D-13). writeTokens is only consulted for an UNTRUSTED run:
// turning the repository switch on lets a fork run's token carry the scopes its
// workflow asks for (never id-token — IDTokenAllowed still refuses), while a
// trusted run is governed by `permissions:` alone exactly as before. With the
// switch off (the default) an untrusted run is `contents: read`.
func (s *Service) EffectivePermissionsFor(spec *Spec, trusted, writeTokens bool) map[string]string {
	if !trusted && !writeTokens {
		return map[string]string{workflow.ScopeContents: PermRead}
	}
	def := PermRead
	if s.DefaultPermissions != "" {
		if n := normalizePerm(s.DefaultPermissions); n != "" {
			def = n
		}
	}
	out := map[string]string{workflow.ScopeContents: def}
	if spec == nil {
		return out
	}
	if all, ok := spec.Permissions[workflow.KeyAll]; ok {
		if n := normalizePerm(all); n != "" {
			for _, scope := range baselineScopes {
				out[scope] = n
			}
		}
	}
	for k, v := range spec.Permissions {
		if k == workflow.KeyAll {
			continue
		}
		canonical, ok, _ := workflow.CanonicalPermissionScope(k)
		if !ok {
			continue // a scope the parser refuses never reaches a token
		}
		n := normalizePerm(v)
		if n == "" {
			continue // an unrecognised level never widens access
		}
		out[canonical] = n
	}
	return out
}

// IDTokenAllowed reports whether a job may exchange an OIDC ID token. Two
// spellings mean the same thing (PAR-119): `permissions: id-token: write` and
// the Forgejo key `enable-openid-connect: true`. An explicit `id-token: none`
// denies regardless of the boolean, an untrusted run never qualifies, and a
// `write-all` baseline never implies it (S10).
func (s *Service) IDTokenAllowed(spec *Spec, trusted bool) bool {
	if !trusted {
		return false
	}
	perms := s.EffectivePermissions(spec, trusted)
	if v, ok := perms[workflow.ScopeIDToken]; ok {
		return v == PermWrite
	}
	return spec != nil && spec.EnableOpenIDConnect
}

// permissionRank orders the three levels for a downgrade (PAR-075).
func permissionRank(level string) int {
	switch level {
	case PermWrite:
		return 2
	case PermRead:
		return 1
	}
	return 0 // none or absent: not granted
}

// downgradePermissions caps a reusable workflow's declared scopes at its
// caller's effective scopes: the callee may narrow, never widen. A scope the
// caller does not grant is `none` for the callee, whatever the callee asks for
// (a caller with no `permissions:` block grants the instance default baseline).
// Scope spellings are canonicalized exactly as EffectivePermissions does it, so
// "what the token gets" and "what the caller may grant" cannot disagree.
func downgradePermissions(cap, declared map[string]string) map[string]string {
	if len(cap) == 0 && len(declared) == 0 {
		return nil
	}
	out := make(map[string]string, len(declared)+1)
	if all, ok := declared[workflow.KeyAll]; ok {
		if n := normalizePerm(all); n != "" {
			for _, scope := range baselineScopes {
				limit := normalizePerm(cap[scope])
				if permissionRank(n) > permissionRank(limit) {
					out[scope] = limit
				} else {
					out[scope] = n
				}
			}
		}
	}
	for k, v := range declared {
		if k == workflow.KeyAll {
			continue
		}
		canonical, ok, _ := workflow.CanonicalPermissionScope(k)
		if !ok {
			continue
		}
		callee := normalizePerm(v)
		if callee == "" {
			callee = PermNone
		}
		limit := normalizePerm(cap[canonical])
		if permissionRank(callee) > permissionRank(limit) {
			out[canonical] = limit
		} else {
			out[canonical] = callee
		}
	}
	if _, ok := out[workflow.ScopeContents]; !ok {
		out[workflow.ScopeContents] = cap[workflow.ScopeContents]
	}
	if out[workflow.ScopeContents] == "" {
		out[workflow.ScopeContents] = PermRead
	}
	return out
}

// containsScope reports whether a scope set carries one scope.
func containsScope(scopes []string, want string) bool {
	for _, sc := range scopes {
		if sc == want {
			return true
		}
	}
	return false
}

// PermissionScopes maps an effective permission map onto git scopes: `push`
// only when `contents: write`. The platform-API half of the authority is the
// repository role the same map implies (internal/identity jobTokenRole),
// computed at the credential boundary.
func PermissionScopes(perms map[string]string) []string {
	out := []string{}
	if perms[workflow.ScopeContents] == PermWrite {
		out = append(out, ScopePush)
	}
	return out
}

// Keyring holds the derived symmetric keys the protocol needs.
//
// Seal derives from the operator's configured secret (server.secrets_key, or
// server.auth.session_secret) and is nil when neither is set — secrets at rest
// then fail closed (503) because an ephemeral key would make stored
// ciphertexts unreadable after a restart.
//
// Sign always exists. Without a configured secret it is process-local: attempt
// tokens stay valid for this instance's lifetime only, which is exactly the
// lifetime of the jobs they authorise (documented in 15 §5).
type Keyring struct {
	Seal []byte
	Sign []byte
	// Ephemeral marks a process-local Sign key (zero-config boots get loud
	// warnings: a restart invalidates every in-flight attempt token).
	Ephemeral bool
}

// NewKeyring derives the protocol keys. secretsKey wins over sessionSecret;
// both empty yields an ephemeral signing key and no sealing key.
func NewKeyring(sessionSecret, secretsKey string) (*Keyring, error) {
	base := strings.TrimSpace(secretsKey)
	if base == "" {
		base = strings.TrimSpace(sessionSecret)
	}
	if base == "" {
		k, err := randomBytes(32)
		if err != nil {
			return nil, err
		}
		return &Keyring{Sign: k, Ephemeral: true}, nil
	}
	return &Keyring{
		Seal: derive(base, "spindle/threads/secrets/aes-gcm/v1"),
		Sign: derive(base, "spindle/threads/attempt-token/hmac/v1"),
	}, nil
}

// derive is HKDF-SHA256 with the purpose as info and an empty salt (the base
// secret is the only entropy, and each purpose yields an independent key).
// The length is a compile-time constant, so hkdf cannot fail here.
func derive(secret, purpose string) []byte {
	// hkdf.Key's only failure is an out-of-range length, which is a constant
	// here; the error is deliberately ignored (a nil key would fail shut
	// everywhere it is used).
	out, _ := hkdf.Key(sha256.New, []byte(secret), nil, purpose, 32)
	return out
}

// SecretsEnabled reports whether at-rest encryption is configured.
func (k *Keyring) SecretsEnabled() bool { return k != nil && len(k.Seal) == 32 }

// randRead is crypto/rand.Read, swappable so the tests can prove every mint
// path fails closed on an entropy failure.
var randRead = rand.Read

// randomBytes reads n cryptographically random bytes.
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := randRead(b); err != nil {
		return nil, err
	}
	return b, nil
}

// mintSecret mints a 32-byte hex secret (shown once).
func mintSecret() (string, error) {
	b, err := randomBytes(32)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// mintID mints a 12-char [a-z0-9] id (runner ids, credential ids).
func mintID() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 12)
	if _, err := randRead(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}

// hashSecret hex-encodes sha256(secret) — the only stored form of a token.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// verifySecret compares a candidate secret against a stored hash in constant
// time (the checks/metadata token rule).
func verifySecret(candidate, storedHash string) bool {
	cand := hashSecret(candidate)
	if len(cand) != len(storedHash) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cand), []byte(storedHash)) == 1
}

// tokenParts splits "<prefix><principal>.<id>.<secret>".
func tokenParts(token, prefix string) (principal, id, secret string, ok bool) {
	body, found := strings.CutPrefix(strings.TrimSpace(token), prefix)
	if !found {
		return "", "", "", false
	}
	parts := strings.Split(body, ".")
	if len(parts) != 3 {
		return "", "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(raw) == 0 {
		return "", "", "", false
	}
	if !tokenIDRe.MatchString(parts[1]) || len(parts[2]) < 16 {
		return "", "", "", false
	}
	return string(raw), parts[1], parts[2], true
}

// mintScopedToken mints a principal/id/secret token with the given prefix.
func mintScopedToken(prefix, principal string, id string) (string, string, error) {
	secret, err := mintSecret()
	if err != nil {
		return "", "", err
	}
	enc := base64.RawURLEncoding.EncodeToString([]byte(strings.ToLower(strings.TrimSpace(principal))))
	return prefix + enc + "." + id + "." + secret, hashSecret(secret), nil
}

// --- attempt tokens --------------------------------------------------------

// AttemptClaims is the signed payload of a wjt_ token: identity, not a
// timestamp comparison, is what rejects a superseded attempt.
type AttemptClaims struct {
	Repo    string   `json:"repo"`
	Run     string   `json:"run"`
	Job     string   `json:"job"`
	Attempt int      `json:"attempt"`
	Runner  string   `json:"runner"`
	Scopes  []string `json:"scopes"`
	// Exp is the unix-second expiry.
	Exp int64 `json:"exp"`
}

// Allows reports whether the claim carries one scope.
func (c AttemptClaims) Allows(scope string) bool {
	for _, sc := range c.Scopes {
		if sc == scope {
			return true
		}
	}
	return false
}

// mintAttemptToken signs one attempt token.
func (s *Service) mintAttemptToken(c AttemptClaims) (string, time.Time, error) {
	if s.keys == nil || len(s.keys.Sign) == 0 {
		return "", time.Time{}, fmt.Errorf("%w: runner protocol not configured", ErrUnavailable)
	}
	body := base64.RawURLEncoding.EncodeToString(mustJSON(c))
	mac := hmac.New(sha256.New, s.keys.Sign)
	mac.Write([]byte(attemptTokenPrefix + body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return attemptTokenPrefix + body + "." + sig, time.Unix(c.Exp, 0).UTC(), nil
}

// parseAttemptToken verifies the signature and expiry and returns the claims.
func (s *Service) parseAttemptToken(token string, now time.Time) (AttemptClaims, error) {
	if s.keys == nil || len(s.keys.Sign) == 0 {
		return AttemptClaims{}, fmt.Errorf("%w: runner protocol not configured", ErrUnavailable)
	}
	body, ok := strings.CutPrefix(strings.TrimSpace(token), attemptTokenPrefix)
	if !ok {
		return AttemptClaims{}, fmt.Errorf("%w: not an attempt token", ErrUnauthorized)
	}
	parts := strings.Split(body, ".")
	if len(parts) != 2 {
		return AttemptClaims{}, fmt.Errorf("%w: malformed attempt token", ErrUnauthorized)
	}
	mac := hmac.New(sha256.New, s.keys.Sign)
	mac.Write([]byte(attemptTokenPrefix + parts[0]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(parts[1])) != 1 {
		return AttemptClaims{}, fmt.Errorf("%w: bad attempt token signature", ErrUnauthorized)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return AttemptClaims{}, fmt.Errorf("%w: malformed attempt token", ErrUnauthorized)
	}
	var c AttemptClaims
	if jerr := json.Unmarshal(raw, &c); jerr != nil {
		return AttemptClaims{}, fmt.Errorf("%w: malformed attempt token", ErrUnauthorized)
	}
	if c.Exp <= now.Unix() {
		return AttemptClaims{}, fmt.Errorf("%w: attempt token expired", ErrUnauthorized)
	}
	return c, nil
}

// attemptTokenTTL bounds one attempt token: the job's own timeout plus a grace
// window, never longer than six hours.
func attemptTokenTTL(spec *Spec) time.Duration {
	ttl := 2 * time.Hour
	if spec != nil && spec.TimeoutMinutes > 0 {
		ttl = time.Duration(spec.TimeoutMinutes)*time.Minute + 10*time.Minute
	}
	if ttl > 6*time.Hour {
		ttl = 6 * time.Hour
	}
	return ttl
}

// --- request helpers -------------------------------------------------------

// bearerToken reads an Authorization: Bearer credential.
func bearerToken(header string) string {
	h := strings.TrimSpace(header)
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

// attemptPathNumber parses the {n} path segment of the attempt endpoints.
func attemptPathNumber(raw string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 || n > 1_000_000 {
		return 0, fmt.Errorf("%w: attempt must be a positive integer", ErrInvalid)
	}
	return n, nil
}

// errSuperseded marks a write carrying an attempt that is no longer current.
var errSuperseded = errors.New("superseded attempt")
