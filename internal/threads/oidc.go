package threads

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/policy"
	"github.com/crueber/spindle/internal/store"
)

// Job OIDC tokens (docs/PLAN-THREADS-OIDC.md, docs/threads.md §5.5).
//
// A trusted job that declares `permissions: id-token: write` receives an
// opaque, single-job request token in its environment. It POSTs that token
// (plus an audience) to the exchange endpoint and receives a short-lived RS256
// JWT a cloud provider can verify against this instance's JWKS.
//
// The signing key is generated once into the bucket and CAS-created, so two
// instances cannot mint competing trust roots:
//
//	threads/oidc/signing-key.json                                  the key set (private material)
//	repos/<o>/<r>/threads/runs/<run>/jobs/<jid>/id-token/<attempt>.json   request-token record (hash only)
//
// The bucket is a trust root: whoever can write the key object can mint tokens
// this instance will sign with. That is the same trust the repository itself
// already places in the store, and it is stated in the plan and in
// docs/features/15_threads.md.
//
// ### Concurrency
//
// Hazard: two instances generating a signing key at first use (or rotating at
// the same moment). Avoidance: the key set is Create-only on first use and
// CAS-updated on rotation; the loser of either race re-reads the winner's
// object and signs with it, so the bucket holds exactly one active key and
// every instance converges on it. No in-process lock is taken (the CAS is the
// lock, 13_concurrency.md §3) and no lock is held across a store call. Hazard:
// two exchanges for one job racing the rate-limit counter. Avoidance: the
// counter is a CAS on the request-token record; a lost CAS re-reads and
// re-applies, and a racing pair can overshoot by at most the number of
// concurrent writers (documented, best effort — the point is to stop a loop,
// not to meter precisely). Hazard: reading the key on every exchange. The key
// object is one exact-key GET (no LIST) on a path that is already rare (one
// exchange per job per minute at most); a cache would hide another instance's
// rotation for its TTL, so the read is deliberately uncached.

const (
	// oidcKeyKeyPath is the instance signing key set (private material).
	oidcKeyKeyPath = "threads/oidc/signing-key.json"
	// oidcKeyBits is the RSA modulus size. 2048 is the RS256 floor every
	// provider accepts; larger keys slow every exchange for no security gain
	// against the attack this endpoint faces (rate-limited, short-lived).
	oidcKeyBits = 2048
	// oidcRotationAge rotates the active key automatically. A key older than
	// this is retired (its public half stays in the JWKS) and a new one signs.
	oidcRotationAge = 90 * 24 * time.Hour
	// oidcRetiredRetention keeps a retired public key in the JWKS at least as
	// long as any token it may still have signed: the 5-minute token TTL plus
	// clock skew, rounded up to an hour.
	oidcRetiredRetention = time.Hour
	// oidcTokenTTL is the lifetime of one issued ID token (≤ 10 minutes; this
	// matches providers' expectations and limits replay).
	oidcTokenTTL = 5 * time.Minute
	// oidcKeyCacheTTL bounds how long a warm instance may miss a PEER
	// instance's rotation. A local rotation is visible immediately (the loader
	// is exclusive and stores the rotated set before releasing).
	oidcKeyCacheTTL = 60 * time.Second
	// oidcRequestTokenTTL caps a job's request token at the attempt's own
	// lifetime (job timeout + grace, ≤ 6 hours) so a token cannot outlive the
	// job it belongs to.
	oidcRequestTokenTTL = 6 * time.Hour
	// idTokenExchangeLimit is the per-job-attempt budget in one window.
	idTokenExchangeLimit = 10
	// idTokenExchangeWindow is the rate-limit window.
	idTokenExchangeWindow = time.Minute
	// idTokenRequestPrefix marks the opaque request token.
	idTokenRequestPrefix = "wot_"
)

// oidcKeyDoc is the bucket object holding the key set. Private material lives
// only here (and in memory while signing); it is never in a page, an API
// answer, a job payload or a log line.
type oidcKeyDoc struct {
	Version   int       `json:"version"`
	ActiveKID string    `json:"active_kid"`
	Keys      []oidcKey `json:"keys"`
}

// oidcKey is one RSA key in the set.
type oidcKey struct {
	KID string `json:"kid"`
	// Alg is always RS256 today; carried so an old object states it.
	Alg string `json:"alg"`
	// PrivatePEM is PKCS#1 PEM ("RSA PRIVATE KEY").
	PrivatePEM string `json:"private_key_pem"`
	CreatedAt  string `json:"created_at"`
	// RetiredAt is when the key stopped signing; nil while active. A retired
	// key stays in the JWKS until oidcRetiredRetention has passed.
	RetiredAt *string `json:"retired_at,omitempty"`
}

// oidcSigningKeySet is a parsed key set: the active private key plus every
// public key the JWKS advertises.
type oidcSigningKeySet struct {
	doc       *oidcKeyDoc
	active    *rsa.PrivateKey
	activeKID string
}

// oidcJWK is one public JWKS entry.
type oidcJWK struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	KID string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// oidcDiscovery is the discovery document (hand-rolled JSON; the fields are
// the subset a provider needs and nothing else).
type oidcDiscovery struct {
	Issuer                           string   `json:"issuer"`
	JWKSURI                          string   `json:"jwks_uri"`
	SubjectTypesSupported            []string `json:"subject_types_supported"`
	IDTokenSigningAlgValuesSupported []string `json:"id_token_signing_alg_values_supported"`
	ResponseTypesSupported           []string `json:"response_types_supported"`
	ScopesSupported                  []string `json:"scopes_supported"`
	ClaimsSupported                  []string `json:"claims_supported"`
}

// idTokenClaims is the exact claim set (PLAN-THREADS-OIDC §4, PAR-120). A
// struct, not a map, so the wire shape is pinned and cannot grow an
// accidentally-exported field — no claim is ever copied from user data except
// `aud` (validated printable text) and `environment` (a parser-pinned name).
type idTokenClaims struct {
	Iss             string `json:"iss"`
	Sub             string `json:"sub"`
	Aud             string `json:"aud"`
	Exp             int64  `json:"exp"`
	Iat             int64  `json:"iat"`
	Nbf             int64  `json:"nbf"`
	JTI             string `json:"jti"`
	Repository      string `json:"repository"`
	RepositoryOwner string `json:"repository_owner"`
	Ref             string `json:"ref"`
	RefType         string `json:"ref_type"`
	RefProtected    bool   `json:"ref_protected"`
	SHA             string `json:"sha"`
	Workflow        string `json:"workflow"`
	JobWorkflowRef  string `json:"job_workflow_ref"`
	Job             string `json:"job"`
	RunID           string `json:"run_id"`
	RunAttempt      int    `json:"run_attempt"`
	EventName       string `json:"event_name"`
	// RunnerEnvironment is always "self-hosted" (Threads has no hosted fleet;
	// the claim exists so a provider policy written for GitHub matches).
	RunnerEnvironment string `json:"runner_environment"`
	// Environment appears ONLY when the job names one, and then the subject is
	// the environment subject instead of the ref subject (PAR-120).
	Environment string `json:"environment,omitempty"`
}

// oidcJWTHeader is the RS256 JOSE header.
type oidcJWTHeader struct {
	Alg string `json:"alg"`
	KID string `json:"kid"`
	Typ string `json:"typ"`
}

// idTokenRequestDoc is the hash-only request-token record. One per job
// attempt; the key is derivable from (repo, run, job, attempt), so the
// exchange needs one exact-key GET and never a LIST.
type idTokenRequestDoc struct {
	Version    int    `json:"version"`
	Repo       string `json:"repo"`
	Run        string `json:"run"`
	Job        string `json:"job"`
	Attempt    int    `json:"attempt"`
	TokenHash  string `json:"token_hash"`
	CreatedAt  string `json:"created_at"`
	ExpiresAt  string `json:"expires_at"`
	WindowFrom string `json:"window_from,omitempty"`
	WindowN    int    `json:"window_count,omitempty"`
}

// IDTokenRequest is the request-token material delivered to one job attempt.
// RequestToken is the only plaintext copy; the bucket holds its hash.
type IDTokenRequest struct {
	RequestURL   string `json:"request_url"`
	RequestToken string `json:"request_token"`
	ExpiresAt    string `json:"expires_at"`
}

// IDTokenRequestKey returns the request-token record for one job attempt.
func IDTokenRequestKey(owner, repo, runID, jobID string, attempt int) string {
	return IDTokenRequestPrefix(owner, repo, runID, jobID) + padInt(attempt, 3) + ".json"
}

// IDTokenRequestPrefix returns the request-token family of one job.
func IDTokenRequestPrefix(owner, repo, runID, jobID string) string {
	return JobsPrefix(owner, repo, runID) + jobID + "/id-token/"
}

// OIDCSigningKeyPath is the instance key object (the storage family this
// feature adds; exposed so docs and tests can name it without copying it).
func OIDCSigningKeyPath() string { return oidcKeyKeyPath }

// --- signing key set -------------------------------------------------------

// oidcKeyCache is the in-process key cache (the account-gate LRU precedent:
// a plain mutex + pointer, never a store client held across a call). One entry
// holds the active private key AND the rendered public JWKS, so a warm JWKS
// read and a warm exchange do ZERO store reads. It is memory-only: a restart
// re-reads the bucket.
//
// TTL (oidcKeyCacheTTL) bounds how long this instance can miss a PEER
// instance's rotation. A local rotation is visible immediately: the loader is
// exclusive (loading blocks other loads), so the rotated set is the one stored
// before any other caller can load.
type oidcKeyCache struct {
	mu      sync.Mutex
	entry   *oidcKeyEntry
	loading bool
	wait    chan struct{}
}

// oidcKeyEntry is one cached key state. jwksErr is a deterministic derivation
// error (a corrupt retired key), not a store fault.
type oidcKeyEntry struct {
	set      *oidcSigningKeySet
	jwks     []oidcJWK
	jwksErr  error
	loadedAt time.Time
}

// oidcKeyCacheFor returns the service's cache. A Service built as a literal
// (tests only) gets a throwaway cache, so no field write can race; New always
// installs one.
func (s *Service) oidcKeyCacheFor() *oidcKeyCache {
	if s.oidcCache != nil {
		return s.oidcCache
	}
	return &oidcKeyCache{}
}

// oidcKeyEntry returns the cached entry, loading it under a single-flight on
// a cold cache or after the TTL. A store fault is returned and NOT cached: the
// next request re-reads.
func (s *Service) oidcKeyEntry(ctx context.Context) (*oidcKeyEntry, error) {
	if s == nil || s.Store == nil {
		return nil, fmt.Errorf("%w: threads store not wired", ErrUnavailable)
	}
	cache := s.oidcKeyCacheFor()
	for {
		now := s.nowUTC()
		cache.mu.Lock()
		if e := cache.entry; e != nil && now.Sub(e.loadedAt) < oidcKeyCacheTTL {
			cache.mu.Unlock()
			return e, nil
		}
		if cache.loading {
			wait := cache.wait
			cache.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		cache.loading = true
		cache.wait = make(chan struct{})
		cache.mu.Unlock()

		set, err := s.loadOIDCKeySet(ctx)
		var entry *oidcKeyEntry
		if err == nil {
			entry = &oidcKeyEntry{set: set}
			entry.jwks, entry.jwksErr = set.jwks()
			entry.loadedAt = s.nowUTC()
		}
		cache.mu.Lock()
		if err == nil {
			cache.entry = entry
		}
		cache.loading = false
		close(cache.wait)
		cache.wait = nil
		cache.mu.Unlock()
		return entry, err
	}
}

// oidcKeySet returns the active signing key (cached while warm).
func (s *Service) oidcKeySet(ctx context.Context) (*oidcSigningKeySet, error) {
	e, err := s.oidcKeyEntry(ctx)
	if err != nil {
		return nil, err
	}
	return e.set, nil
}

// oidcJWKS returns the public key set for the served JWKS document (cached
// alongside the private key, so the open endpoint costs nothing while warm).
func (s *Service) oidcJWKS(ctx context.Context) ([]oidcJWK, error) {
	e, err := s.oidcKeyEntry(ctx)
	if err != nil {
		return nil, err
	}
	if e.jwksErr != nil {
		return nil, e.jwksErr
	}
	return e.jwks, nil
}

// loadOIDCKeySet reads (and, on first use or after oidcRotationAge, generates
// or rotates) the signing key set. The CAS/create race is the only writer.
func (s *Service) loadOIDCKeySet(ctx context.Context) (*oidcSigningKeySet, error) {
	if s == nil || s.Store == nil {
		return nil, fmt.Errorf("%w: threads store not wired", ErrUnavailable)
	}
	raw, _, err := s.getJSON(ctx, oidcKeyKeyPath)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		doc, cerr := s.createOIDCKeyDoc(ctx)
		if cerr != nil {
			return nil, cerr
		}
		return parseOIDCKeySet(doc)
	}
	doc, derr := decodeOIDCKeyDoc(raw)
	if derr != nil {
		return nil, derr
	}
	rotated, rerr := s.maybeRotateOIDCKey(ctx, doc)
	if rerr != nil {
		return nil, rerr
	}
	return parseOIDCKeySet(rotated)
}

// createOIDCKeyDoc generates the first key and Create-writes the object. A
// 412 means another instance won the race: its object is read back and used
// (never overwritten — two trust roots would be worse than a slow boot).
func (s *Service) createOIDCKeyDoc(ctx context.Context) (*oidcKeyDoc, error) {
	now := s.nowUTC()
	doc, err := newOIDCKeyDoc(now)
	if err != nil {
		return nil, err
	}
	perr := s.putCreate(ctx, oidcKeyKeyPath, mustJSON(doc))
	if perr == nil {
		return doc, nil
	}
	if !store.IsPreconditionFailed(perr) {
		return nil, perr
	}
	raw, _, gerr := s.getJSON(ctx, oidcKeyKeyPath)
	if gerr != nil {
		return nil, gerr
	}
	if raw == nil {
		// Created then removed between the two reads: fail closed rather than
		// mint a second trust root out of band.
		return nil, fmt.Errorf("%w: OIDC signing key disappeared during creation", ErrConflict)
	}
	return decodeOIDCKeyDoc(raw)
}

// maybeRotateOIDCKey rotates the active key when it is at least
// oidcRotationAge old. Rotation keeps every old public key in the JWKS (so
// tokens signed moments before keep verifying) and prunes keys retired longer
// than oidcRetiredRetention.
func (s *Service) maybeRotateOIDCKey(ctx context.Context, doc *oidcKeyDoc) (*oidcKeyDoc, error) {
	active := doc.activeKey()
	if active == nil {
		return nil, fmt.Errorf("%w: OIDC key set has no active key", ErrCorrupt)
	}
	created, cerr := time.Parse(dateTimeFmt, active.CreatedAt)
	if cerr != nil {
		return nil, fmt.Errorf("%w: OIDC key %s created_at: %v", ErrCorrupt, active.KID, cerr)
	}
	now := s.nowUTC()
	if now.Sub(created) < oidcRotationAge {
		return doc, nil
	}
	var out *oidcKeyDoc
	_, rerr := s.casUpdate(ctx, oidcKeyKeyPath, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: OIDC signing key vanished", ErrCorrupt)
		}
		curDoc, derr := decodeOIDCKeyDoc(cur)
		if derr != nil {
			return nil, false, derr
		}
		curActive := curDoc.activeKey()
		if curActive == nil {
			return nil, false, fmt.Errorf("%w: OIDC key set has no active key", ErrCorrupt)
		}
		curCreated, terr := time.Parse(dateTimeFmt, curActive.CreatedAt)
		if terr != nil {
			return nil, false, fmt.Errorf("%w: OIDC key %s created_at: %v", ErrCorrupt, curActive.KID, terr)
		}
		if now.Sub(curCreated) < oidcRotationAge {
			out = curDoc // another instance rotated while we were deciding
			return nil, false, nil
		}
		next, nerr := rotateOIDCKeyDoc(curDoc, now)
		if nerr != nil {
			return nil, false, nerr
		}
		out = next
		return mustJSON(next), true, nil
	})
	if rerr != nil {
		return nil, rerr
	}
	if out == nil {
		return nil, fmt.Errorf("%w: OIDC key rotation did not converge", ErrConflict)
	}
	return out, nil
}

// newOIDCKeyDoc mints the first key set.
func newOIDCKeyDoc(now time.Time) (*oidcKeyDoc, error) {
	key, err := newOIDCKey(now)
	if err != nil {
		return nil, err
	}
	return &oidcKeyDoc{Version: 1, ActiveKID: key.KID, Keys: []oidcKey{*key}}, nil
}

// rotateOIDCKeyDoc appends a new active key and retires the old one. The input
// is not mutated (a CAS loser re-reads its own copy).
func rotateOIDCKeyDoc(doc *oidcKeyDoc, now time.Time) (*oidcKeyDoc, error) {
	key, err := newOIDCKey(now)
	if err != nil {
		return nil, err
	}
	stamp := now.Format(dateTimeFmt)
	next := &oidcKeyDoc{Version: doc.Version + 1, ActiveKID: key.KID}
	for _, old := range doc.Keys {
		if old.KID == doc.ActiveKID && old.RetiredAt == nil {
			old.RetiredAt = &stamp
		}
		if old.RetiredAt != nil {
			if retired, perr := time.Parse(dateTimeFmt, *old.RetiredAt); perr == nil &&
				now.Sub(retired) > oidcRetiredRetention {
				continue // past every token it could have signed
			}
		}
		next.Keys = append(next.Keys, old)
	}
	next.Keys = append(next.Keys, *key)
	return next, nil
}

// newOIDCKey generates one RSA key and its kid (a public-key thumbprint, so a
// key identifies itself everywhere it is copied).
func newOIDCKey(now time.Time) (*oidcKey, error) {
	priv, err := rsa.GenerateKey(rand.Reader, oidcKeyBits)
	if err != nil {
		return nil, fmt.Errorf("%w: generate OIDC signing key: %v", ErrUnavailable, err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	if pemBytes == nil {
		return nil, fmt.Errorf("%w: encode OIDC signing key", ErrUnavailable)
	}
	return &oidcKey{
		KID:        oidcThumbprint(&priv.PublicKey),
		Alg:        "RS256",
		PrivatePEM: string(pemBytes),
		CreatedAt:  now.Format(dateTimeFmt),
	}, nil
}

// oidcThumbprint is base64url(sha256(n || e))[:16]: stable for a key, unique
// in practice, and never derived from private material.
func oidcThumbprint(pub *rsa.PublicKey) string {
	sum := sha256.New()
	sum.Write(pub.N.Bytes())
	sum.Write(big.NewInt(int64(pub.E)).Bytes())
	return base64.RawURLEncoding.EncodeToString(sum.Sum(nil))[:16]
}

// decodeOIDCKeyDoc parses one key object.
func decodeOIDCKeyDoc(raw []byte) (*oidcKeyDoc, error) {
	var doc oidcKeyDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%w: OIDC signing key: %v", ErrCorrupt, err)
	}
	if doc.ActiveKID == "" || len(doc.Keys) == 0 {
		return nil, fmt.Errorf("%w: OIDC signing key set is empty", ErrCorrupt)
	}
	return &doc, nil
}

// activeKey returns the active key entry (nil when the set is inconsistent).
func (d *oidcKeyDoc) activeKey() *oidcKey {
	for i := range d.Keys {
		if d.Keys[i].KID == d.ActiveKID {
			return &d.Keys[i]
		}
	}
	return nil
}

// parseOIDCKeySet decodes the private keys and picks the active one.
func parseOIDCKeySet(doc *oidcKeyDoc) (*oidcSigningKeySet, error) {
	active := doc.activeKey()
	if active == nil {
		return nil, fmt.Errorf("%w: OIDC key set names an unknown active kid", ErrCorrupt)
	}
	priv, err := parseOIDCPrivateKey(*active)
	if err != nil {
		return nil, err
	}
	return &oidcSigningKeySet{doc: doc, active: priv, activeKID: active.KID}, nil
}

// parseOIDCPrivateKey decodes one PKCS#1 PEM key and re-checks its size.
func parseOIDCPrivateKey(k oidcKey) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(k.PrivatePEM))
	if block == nil || block.Type != "RSA PRIVATE KEY" {
		return nil, fmt.Errorf("%w: OIDC key %s is not a PKCS#1 RSA private key", ErrCorrupt, k.KID)
	}
	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: OIDC key %s: %v", ErrCorrupt, k.KID, err)
	}
	if priv.N.BitLen() < oidcKeyBits {
		return nil, fmt.Errorf("%w: OIDC key %s is %d bits, want at least %d", ErrCorrupt, k.KID, priv.N.BitLen(), oidcKeyBits)
	}
	if err := priv.Validate(); err != nil {
		return nil, fmt.Errorf("%w: OIDC key %s: %v", ErrCorrupt, k.KID, err)
	}
	return priv, nil
}

// jwks renders every public key in the set (active and not-yet-pruned retired
// keys), so a token signed before a rotation still verifies until it expires.
func (set *oidcSigningKeySet) jwks() ([]oidcJWK, error) {
	out := make([]oidcJWK, 0, len(set.doc.Keys))
	for _, k := range set.doc.Keys {
		priv, err := parseOIDCPrivateKey(k)
		if err != nil {
			return nil, err
		}
		out = append(out, oidcJWK{
			Kty: "RSA", Use: "sig", Alg: "RS256", KID: k.KID,
			N: base64.RawURLEncoding.EncodeToString(priv.N.Bytes()),
			E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.E)).Bytes()),
		})
	}
	return out, nil
}

// --- JWT -------------------------------------------------------------------

// signOIDCJWT signs one RS256 JWT with the set's active key.
func (set *oidcSigningKeySet) sign(claims idTokenClaims) (string, error) {
	header, err := json.Marshal(oidcJWTHeader{Alg: "RS256", KID: set.activeKID, Typ: "JWT"})
	if err != nil {
		return "", fmt.Errorf("%w: encode OIDC header: %v", ErrUnavailable, err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("%w: encode OIDC claims: %v", ErrUnavailable, err)
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(input))
	sig, serr := rsa.SignPKCS1v15(rand.Reader, set.active, crypto.SHA256, digest[:])
	if serr != nil {
		return "", fmt.Errorf("%w: sign OIDC token: %v", ErrUnavailable, serr)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// --- request token ---------------------------------------------------------

// mintIDTokenRequest mints and stores one job attempt's request token. The
// record holds only sha256(token); the plaintext is returned to the caller (the
// fetch payload) and exists nowhere else.
func (s *Service) mintIDTokenRequest(ctx context.Context, owner, repo, runID, jobID string, attempt int, ttl time.Duration, baseURL string) (*IDTokenRequest, error) {
	if attempt < 1 || runID == "" || jobID == "" {
		return nil, fmt.Errorf("%w: request token needs a job and attempt", ErrInvalid)
	}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("%w: no public origin for the ID-token request URL", ErrUnavailable)
	}
	if ttl <= 0 || ttl > oidcRequestTokenTTL {
		ttl = oidcRequestTokenTTL
	}
	secretBytes, err := randomBytes(32)
	if err != nil {
		return nil, fmt.Errorf("%w: mint request token: %v", ErrUnavailable, err)
	}
	token := idTokenRequestPrefix + hex.EncodeToString(secretBytes)
	now := s.nowUTC()
	doc := idTokenRequestDoc{
		Version: 1, Repo: repoID(owner, repo), Run: runID, Job: jobID, Attempt: attempt,
		TokenHash: hashSecret(token), CreatedAt: now.Format(dateTimeFmt),
		ExpiresAt: now.Add(ttl).Format(dateTimeFmt),
	}
	if _, perr := store.PutBytes(ctx, s.Store, IDTokenRequestKey(owner, repo, runID, jobID, attempt),
		mustJSON(&doc), store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/json"}); perr != nil {
		return nil, perr
	}
	return &IDTokenRequest{
		RequestURL:   base + "/api/v1/threads/jobs/" + JobRef(repoID(owner, repo), runID, jobID) + "/attempt/" + fmt.Sprint(attempt) + "/id-token",
		RequestToken: token,
		ExpiresAt:    doc.ExpiresAt,
	}, nil
}

// ExchangeIDToken verifies one request token, enforces the permission gate and
// returns a signed ID token. The request token is the ONLY authority: the path
// supplies the job identity, and the stored record must name the same attempt
// and hash, so one job's token can never fetch another job's identity.
func (s *Service) ExchangeIDToken(ctx context.Context, ref string, attempt int, requestToken, audience, issuer string) (string, error) {
	aud := strings.TrimSpace(audience)
	if aud == "" {
		return "", fmt.Errorf("%w: audience is required", ErrInvalid)
	}
	if len(aud) > 512 || hasControl(aud) {
		return "", fmt.Errorf("%w: audience is invalid (1–512 printable characters)", ErrInvalid)
	}
	owner, repo, runID, jobID, perr := parseJobRef(ref)
	if perr != nil {
		return "", fmt.Errorf("%w: unknown job reference", ErrNotFound)
	}
	if attempt < 1 {
		return "", fmt.Errorf("%w: attempt must be a positive integer", ErrInvalid)
	}
	tok := strings.TrimSpace(requestToken)
	if !strings.HasPrefix(tok, idTokenRequestPrefix) {
		return "", fmt.Errorf("%w: request token required", ErrUnauthorized)
	}
	// Verify the token AND account the exchange in one CAS: the callback sees
	// the freshest record, so a concurrent exchange cannot bypass the window.
	now := s.nowUTC()
	var doc idTokenRequestDoc
	if _, cerr := s.casUpdate(ctx, IDTokenRequestKey(owner, repo, runID, jobID, attempt), 5,
		func(cur []byte, _ store.Version) ([]byte, bool, error) {
			if cur == nil {
				return nil, false, fmt.Errorf("%w: unknown or expired request token", ErrUnauthorized)
			}
			if jerr := json.Unmarshal(cur, &doc); jerr != nil {
				return nil, false, fmt.Errorf("%w: request token record: %v", ErrCorrupt, jerr)
			}
			if doc.Run != runID || doc.Job != jobID || doc.Attempt != attempt ||
				!verifySecret(tok, doc.TokenHash) {
				return nil, false, fmt.Errorf("%w: request token is scoped to another job", ErrUnauthorized)
			}
			exp, terr := time.Parse(dateTimeFmt, doc.ExpiresAt)
			if terr != nil {
				return nil, false, fmt.Errorf("%w: request token expiry: %v", ErrCorrupt, terr)
			}
			if !now.Before(exp) {
				return nil, false, fmt.Errorf("%w: request token expired", ErrUnauthorized)
			}
			from := doc.WindowFrom
			if from == "" {
				doc.WindowFrom = now.Format(dateTimeFmt)
			} else if parsed, perr := time.Parse(dateTimeFmt, from); perr == nil &&
				now.Sub(parsed) >= idTokenExchangeWindow {
				doc.WindowFrom = now.Format(dateTimeFmt)
				doc.WindowN = 0
			}
			if doc.WindowN >= idTokenExchangeLimit {
				return nil, false, fmt.Errorf("%w: too many ID-token requests for this job; retry after %s", ErrRateLimited, idTokenExchangeWindow)
			}
			doc.WindowN++
			return mustJSON(&doc), true, nil
		}); cerr != nil {
		return "", cerr
	}
	job, run, jerr := s.loadJobAndRun(ctx, owner, repo, runID, jobID)
	if jerr != nil {
		return "", jerr
	}
	if job == nil {
		return "", fmt.Errorf("%w: unknown job %q", ErrNotFound, jobID)
	}
	if job.Attempt != attempt {
		return "", fmt.Errorf("%w: request token is scoped to another attempt", ErrForbidden)
	}
	if terminal(job.State) {
		return "", fmt.Errorf("%w: the job is finished; its request token is revoked", ErrForbidden)
	}
	trusted := job.Trusted && (run == nil || run.Trusted)
	if !trusted {
		return "", fmt.Errorf("%w: fork runs never receive an ID token", ErrForbidden)
	}
	if !s.IDTokenAllowed(job.Spec, trusted) {
		return "", fmt.Errorf("%w: the job does not declare id-token: write (or enable-openid-connect)", ErrForbidden)
	}
	if run == nil {
		return "", fmt.Errorf("%w: unknown run %q", ErrNotFound, runID)
	}
	refValue := strings.TrimSpace(run.Ref)
	// A pull request's identity is its BASE branch, matching GITHUB_REF
	// (docs/threads.md §3.5); the PR number in refs/pull/<n>/head is not the
	// ref a provider should trust.
	if run.Event == EventPullReq || refValue == "" {
		if branch := strings.TrimSpace(run.Branch); branch != "" {
			refValue = "refs/heads/" + branch
		}
	}
	if refValue == "" {
		return "", fmt.Errorf("%w: the run has no ref to claim", ErrUnprocessable)
	}
	set, kerr := s.oidcKeySet(ctx)
	if kerr != nil {
		return "", kerr
	}
	iat := now.Unix()
	jti, jerr := newJTI()
	if jerr != nil {
		return "", jerr
	}
	// Subject template (PAR-120, docs/threads.md §5.5):
	//
	//	environment job   repo:<o>/<r>:environment:<name>
	//	pull_request      repo:<o>/<r>:pull_request
	//	push/tag/dispatch repo:<o>/<r>:ref:<full ref>
	//
	// An environment subject outranks the ref so an environment-scoped cloud
	// role cannot be assumed from a branch. The environment name is
	// parser-pinned to [A-Za-z0-9][A-Za-z0-9._-]{0,63}, so it cannot inject a
	// separator into the subject.
	environment := ""
	if job.Spec != nil && job.Spec.Environment != nil {
		environment = strings.TrimSpace(job.Spec.Environment.Name)
	}
	sub := "repo:" + repoID(owner, repo) + ":ref:" + refValue
	switch {
	case environment != "":
		sub = "repo:" + repoID(owner, repo) + ":environment:" + environment
	case run.Event == EventPullReq:
		sub = "repo:" + repoID(owner, repo) + ":pull_request"
	}
	claims := idTokenClaims{
		Iss: issuer, Aud: aud, Iat: iat, Nbf: iat, Exp: iat + int64(oidcTokenTTL/time.Second),
		Sub:             sub,
		JTI:             jti,
		Repository:      repoID(owner, repo),
		RepositoryOwner: owner,
		Ref:             refValue,
		RefType:         oidcRefType(refValue),
		RefProtected:    s.refProtected(ctx, owner, repo, refValue),
		SHA:             run.Commit,
		Workflow:        run.Workflow,
		JobWorkflowRef:  oidcJobWorkflowRef(repoID(owner, repo), run.WorkflowPath, refValue),
		Job:             job.Key,
		RunID:           run.ID,
		RunAttempt:      run.Attempt,
		EventName:       run.Event,

		RunnerEnvironment: "self-hosted",
		Environment:       environment,
	}
	return set.sign(claims)
}

// oidcRefType is the `ref_type` claim: branch or tag (never a PR ref).
func oidcRefType(ref string) string {
	if strings.HasPrefix(ref, "refs/tags/") {
		return "tag"
	}
	return "branch"
}

// oidcJobWorkflowRef renders `job_workflow_ref` in GitHub's documented form:
// `<owner>/<repo>/<workflow path>@<ref>`. Empty when the run did not record a
// path (an older record), so the claim is omitted rather than guessed.
func oidcJobWorkflowRef(repo, workflowPath, ref string) string {
	path := strings.TrimPrefix(strings.TrimSpace(workflowPath), "/")
	if path == "" {
		return ""
	}
	return repo + "/" + path + "@" + ref
}

// refProtected reports whether a `protect` rule covers the ref for a blanket
// pusher (S9: a provider may require a protected ref). The authority is the
// repository's own policy.json, read once per exchange (one exact-key GET on a
// rare path, never cached — a protection change applies to the next exchange).
// A missing or unparseable policy reports false: the claim grants less trust,
// so an error fails closed for the provider's decision.
func (s *Service) refProtected(ctx context.Context, owner, repo, ref string) bool {
	if s == nil || s.Store == nil || ref == "" {
		return false
	}
	raw, _, err := s.getJSON(ctx, "repos/"+owner+"/"+repo+"/policy.json")
	if err != nil || raw == nil {
		return false
	}
	doc, perr := policy.Parse(raw)
	if perr != nil {
		return false
	}
	for _, rule := range policy.MatchingRules(doc, policy.Request{Ref: ref, Op: policy.OpUpdate}) {
		if pe := rule.Protect(); pe != nil && len(pe.RestrictOps) > 0 {
			return true
		}
	}
	return false
}

// newJTI mints a random replay identifier (hex, 16 bytes).
func newJTI() (string, error) {
	b, err := randomBytes(16)
	if err != nil {
		return "", fmt.Errorf("%w: mint ID-token jti: %v", ErrUnavailable, err)
	}
	return hex.EncodeToString(b), nil
}

// hasControl reports whether s carries a control character (an env/JSON
// sabotage attempt in an audience).
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
