package threads

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Repository/workflow secrets (15 §5.3). The API is write-only: a value is
// accepted, sealed with AES-256-GCM and never returned — reads report the name
// and timestamps only. Values are decrypted for exactly one consumer: the job
// payload of a TRUSTED run (fork pull requests get none) and the log masker.
//
// Cryptography: AES-256-GCM with a key derived by HKDF-SHA256 from
// server.secrets_key (preferred) or server.auth.session_secret. The AAD binds
// the ciphertext to (repo, name), so a sealed value cannot be replayed under
// another repository's or another secret's key. Stdlib only (law 1).
//
// Masking is BEST EFFORT and documented as such: the server replaces every
// occurrence of every secret value in a log frame before storing it. A runner
// that transforms a value (encodes it, splits it across frames, prints a
// derived credential) defeats it; there is no in-band redaction guarantee.

// SecretDoc is repos/<o>/<r>/threads/secrets/<name>.json.
type SecretDoc struct {
	Name string `json:"name"`
	// Sealed is base64(nonce || ciphertext) — never the value.
	Sealed    string `json:"sealed"`
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Version   int    `json:"version"`
}

// SecretView is one row of GET …/api/threads/secrets (name + presence only).
type SecretView struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// secretNameRe pins a secret name: an environment-variable-ish identifier.
var secretNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// normalizeSecretName validates a secret name.
func normalizeSecretName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !secretNameRe.MatchString(name) {
		return "", fmt.Errorf("%w: secret name must match [A-Za-z_][A-Za-z0-9_]{0,63}", ErrInvalid)
	}
	return name, nil
}

// sealSecret encrypts one value with AES-GCM, binding it to (scope, name). The
// repository scope passes the repository id; an organization secret passes
// "org:<org>", so a ciphertext can never be replayed under another scope even
// when both hold a secret of the same name.
func (s *Service) sealSecret(scope, name, value string) (string, error) {
	if !s.keys.SecretsEnabled() {
		return "", fmt.Errorf("%w: repository secrets need server.secrets_key or server.auth.session_secret", ErrUnavailable)
	}
	gcm := gcmFor(s.keys.Seal)
	nonce, err := randomBytes(gcm.NonceSize())
	if err != nil {
		return "", fmt.Errorf("%w: nonce: %v", ErrUnavailable, err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(value), secretAAD(scope, name))
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// openSecret decrypts one stored value (trusted runs + the log masker only).
func (s *Service) openSecret(scope, name, sealed string) (string, error) {
	if !s.keys.SecretsEnabled() {
		return "", fmt.Errorf("%w: repository secrets need server.secrets_key or server.auth.session_secret", ErrUnavailable)
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("%w: secret %s: bad envelope", ErrCorrupt, name)
	}
	gcm := gcmFor(s.keys.Seal)
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("%w: secret %s: short envelope", ErrCorrupt, name)
	}
	nonce, body := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, oerr := gcm.Open(nil, nonce, body, secretAAD(scope, name))
	if oerr != nil {
		return "", fmt.Errorf("%w: secret %s: cannot be opened (wrong key or corrupt)", ErrCorrupt, name)
	}
	return string(plain), nil
}

// gcmFor builds AES-256-GCM over a 32-byte key. The key length is fixed by
// NewKeyring, so aes.NewCipher/cipher.NewGCM cannot fail; a failure would be a
// programming error, never a runtime condition.
func gcmFor(key []byte) cipher.AEAD {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic("threads: aes: " + err.Error())
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic("threads: gcm: " + err.Error())
	}
	return gcm
}

// secretAAD binds one ciphertext to its repository and name.
func secretAAD(repo, name string) []byte {
	return []byte("spindle/threads/secret/v1\x00" + repo + "\x00" + name)
}

// SecretsEnabled reports whether this instance can store secrets.
func (s *Service) SecretsEnabled() bool { return s.keys.SecretsEnabled() }

// SetSecret stores (or replaces) one repository secret (admin, write-only).
func (s *Service) SetSecret(ctx context.Context, repo, name, value string, p auth.Principal) error {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return err
	}
	if err := s.requireRole(ctx, owner, rname, p, "admin"); err != nil {
		return err
	}
	name, err = normalizeSecretName(name)
	if err != nil {
		return err
	}
	if !s.SecretsEnabled() {
		return fmt.Errorf("%w: repository secrets need server.secrets_key or server.auth.session_secret", ErrUnavailable)
	}
	if len(value) == 0 || len(value) > 64<<10 {
		return fmt.Errorf("%w: secret value must be 1 byte–64 KiB", ErrInvalid)
	}
	sealed, serr := s.sealSecret(repo, name, value)
	if serr != nil {
		return serr
	}
	now := s.nowUTC().Format(dateTimeFmt)
	key := SecretKey(owner, rname, name)
	_, err = s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		doc := &SecretDoc{Name: name, CreatedAt: now}
		if cur != nil {
			if jerr := json.Unmarshal(cur, doc); jerr != nil {
				return nil, false, fmt.Errorf("%w: secret %s: %v", ErrCorrupt, name, jerr)
			}
		}
		doc.Sealed = sealed
		doc.UpdatedAt = now
		doc.CreatedBy = truncate(strings.ToLower(strings.TrimSpace(p.Name)), 100)
		doc.Version++
		return mustJSON(doc), true, nil
	})
	return err
}

// ListSecrets lists a repository's secret names (admin; values never returned).
func (s *Service) ListSecrets(ctx context.Context, repo string, p auth.Principal) ([]SecretView, error) {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, rname, p, "admin"); err != nil {
		return nil, err
	}
	var keys []string
	if lerr := s.Store.List(ctx, SecretsPrefix(owner, rname), "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, ".json") {
			keys = append(keys, m.Key)
		}
		return nil
	}); lerr != nil {
		return nil, lerr
	}
	sort.Strings(keys)
	out := []SecretView{}
	for _, key := range keys {
		raw, _, gerr := s.getJSON(ctx, key)
		if gerr != nil {
			return nil, gerr
		}
		if raw == nil {
			continue
		}
		var doc SecretDoc
		if jerr := json.Unmarshal(raw, &doc); jerr != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, key, jerr)
		}
		out = append(out, SecretView{Name: doc.Name, CreatedAt: doc.CreatedAt, UpdatedAt: doc.UpdatedAt})
	}
	return out, nil
}

// DeleteSecret removes one secret (admin; absent is not an error).
func (s *Service) DeleteSecret(ctx context.Context, repo, name string, p auth.Principal) error {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return err
	}
	if err := s.requireRole(ctx, owner, rname, p, "admin"); err != nil {
		return err
	}
	name, err = normalizeSecretName(name)
	if err != nil {
		return err
	}
	return s.Store.Delete(ctx, SecretKey(owner, rname, name), "")
}

// Secret scope labels recorded on a job (Job.SecretScopes) and in the fetch
// payload: the scope a resolved value came from, never the value.
const (
	SecretScopeOrganization = "organization"
	SecretScopeRepository   = "repository"
	SecretScopeEnvironment  = "environment"
)

// secretValues decrypts every secret a trusted job of this repository reads,
// with the documented precedence: organization first, then repository (a
// repository secret of the same name wins). Environment secrets override both
// at claim time (fetchPayload), because only the job's spec names one. A single
// unreadable envelope fails the fetch (fail closed: a job that silently lost its
// secrets is worse than a clear error an operator can fix).
func (s *Service) secretValues(ctx context.Context, owner, repo string) (map[string]string, error) {
	values, _, err := s.resolvedSecrets(ctx, owner, repo)
	return values, err
}

// resolvedSecrets is secretValues plus the scope each value came from. Both
// scopes are resolved before the caller filters for a reusable callee, so a
// filtered-out secret loses its scope record too (fetchPayload reconciles).
func (s *Service) resolvedSecrets(ctx context.Context, owner, repo string) (map[string]string, map[string]string, error) {
	values := map[string]string{}
	scopes := map[string]string{}
	if !s.SecretsEnabled() {
		return values, scopes, nil
	}
	org, oerr := s.orgSecretValues(ctx, owner, repo)
	if oerr != nil {
		return nil, nil, oerr
	}
	for name, value := range org {
		values[name] = value
		scopes[name] = SecretScopeOrganization
	}
	repoVals, rerr := s.repoSecretValues(ctx, owner, repo)
	if rerr != nil {
		return nil, nil, rerr
	}
	for name, value := range repoVals {
		values[name] = value
		scopes[name] = SecretScopeRepository
	}
	return values, scopes, nil
}

// repoSecretValues decrypts one repository's own secrets (the historical
// per-name object layout; a LIST here is the repository prefix the caller
// already owns).
func (s *Service) repoSecretValues(ctx context.Context, owner, repo string) (map[string]string, error) {
	if !s.SecretsEnabled() {
		return map[string]string{}, nil
	}
	var keys []string
	if err := s.Store.List(ctx, SecretsPrefix(owner, repo), "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, ".json") {
			keys = append(keys, m.Key)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, key := range keys {
		raw, _, err := s.getJSON(ctx, key)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		var doc SecretDoc
		if jerr := json.Unmarshal(raw, &doc); jerr != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, key, jerr)
		}
		value, oerr := s.openSecret(repoID(owner, repo), doc.Name, doc.Sealed)
		if oerr != nil {
			return nil, oerr
		}
		out[doc.Name] = value
	}
	return out, nil
}

// maskSecretValues replaces every occurrence of every secret value in one log
// frame (best effort, documented: exact matches, plus the two refinements the
// PAR-116 evidence pins):
//   - a value of 4+ bytes is replaced wherever it appears, longest value first
//     so a value that contains another is fully replaced;
//   - a MULTI-LINE value is also masked line by line, because a tool that
//     prints it with its newlines and a streamer that splits a frame can hand
//     the masker one line at a time;
//   - a value SHORTER than 4 bytes is replaced only as a whole token (bounded
//     by non-identifier bytes), so a 1–3 byte secret is masked where it is
//     used without shredding every word that happens to contain it.
//
// MaskValues is the ONE masking rule, exported for the runner process: both
// the server (before a frame/annotation/summary is stored) and the runner
// (before a frame leaves the process) apply it, so the two layers cannot spell
// masking differently.
func MaskValues(text string, values []string) string { return maskSecretValues(text, values) }

func maskSecretValues(text string, values []string) string {
	if text == "" || len(values) == 0 {
		return text
	}
	for _, v := range maskPatterns(values) {
		if len(v) < 4 {
			text = replaceToken(text, v)
			continue
		}
		text = strings.ReplaceAll(text, v, "***")
	}
	return text
}

// maskPatterns expands the raw values into the patterns the masker applies,
// longest first: the value itself plus, for a multi-line value, each non-empty
// line long enough to be masked (both as printed and trimmed).
func maskPatterns(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	add := func(v string) {
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, v := range values {
		if v == "" {
			continue
		}
		add(v)
		if !strings.ContainsAny(v, "\r\n") {
			continue
		}
		for _, line := range strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == '\r' }) {
			if len(line) >= 4 {
				add(line)
			}
			if trimmed := strings.TrimSpace(line); trimmed != line && len(trimmed) >= 4 {
				add(trimmed)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// replaceToken masks a short value only where it stands alone: the byte before
// and after it is not an identifier character. Overlaps are handled by
// advancing one byte on a rejected match.
func replaceToken(text, v string) string {
	if text == "" || v == "" {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		j := strings.Index(text[i:], v)
		if j < 0 {
			b.WriteString(text[i:])
			break
		}
		j += i
		beforeOK := j == 0 || !identifierByte(text[j-1])
		after := j + len(v)
		afterOK := after == len(text) || !identifierByte(text[after])
		b.WriteString(text[i:j])
		if beforeOK && afterOK {
			b.WriteString("***")
			i = after
			continue
		}
		b.WriteByte(text[j])
		i = j + 1
	}
	return b.String()
}

// identifierByte reports whether c can be part of an identifier (the token
// boundary rule the short-value masker uses). '-', '.', and ':' count as
// intra-token bytes, so a short value inside a hyphenated or dotted word
// ("own-abc", "v1.abc") or in front of a separator ("s3://bucket",
// "key:value") is not a false positive; a value that stands alone between
// whitespace, quotes, '=' or brackets still is.
func identifierByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '-' || c == '.' || c == ':'
}

// --- the per-job mask set ----------------------------------------------------

// maskCache remembers one job attempt's mask list for the life of the job. A
// log frame, a step summary and an annotation set all need the same values, and
// re-resolving (one LIST plus every envelope, plus the environment and variable
// documents) on every 256 KiB frame would be a cost with no correctness gain:
// the job received its values at claim time, so a value written after the claim
// was never in that job's environment. The cache is memory only (law 4: warmth),
// bounded, and a pointer so Service stays copyable in tests.
type maskCache struct {
	mu      sync.Mutex
	entries map[string]maskCacheEntry
}

type maskCacheEntry struct {
	values []string
	at     time.Time
}

const (
	maskCacheTTL = 30 * time.Minute
	maskCacheMax = 512
	// maxMaskValues bounds one job's mask list (a mask is applied per frame, so
	// an unbounded list is a CPU knife — the runner applies the same bound).
	maxMaskValues = 256
)

func newMaskCache() *maskCache {
	return &maskCache{entries: map[string]maskCacheEntry{}}
}

func (c *maskCache) get(key string, now time.Time) ([]string, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || now.Sub(entry.at) > maskCacheTTL {
		delete(c.entries, key)
		return nil, false
	}
	return entry.values, true
}

func (c *maskCache) put(key string, values []string, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maskCacheMax {
		for k, entry := range c.entries {
			if now.Sub(entry.at) > maskCacheTTL {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= maskCacheMax {
			// Still full: drop an arbitrary entry. Every entry is a cache hit
			// that costs one resolution to rebuild, never a correctness loss.
			for k := range c.entries {
				delete(c.entries, k)
				break
			}
		}
	}
	c.entries[key] = maskCacheEntry{values: values, at: now}
}

// forget drops one attempt's cached mask (complete, cancel, rerun).
func (c *maskCache) forget(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// maskCacheKey is one attempt's cache identity.
func maskCacheKey(repo, runID, jobID string, attempt int) string {
	return repo + "|" + runID + "|" + jobID + "|" + strconv.Itoa(attempt)
}

// jobMaskValues is the complete value set a trusted job's log frames, step
// summary, annotations and step errors are masked with: every secret the job
// could read (organization, repository and the environment it names) plus every
// variable and environment variable, which carry the same posture. An untrusted
// job has no values at all. The result is cached per attempt.
func (s *Service) jobMaskValues(ctx context.Context, owner, repo string, job *Job) ([]string, error) {
	if job == nil {
		return nil, nil
	}
	key := maskCacheKey(repoID(owner, repo), job.RunID, job.ID, job.Attempt)
	if cached, ok := s.maskCache.get(key, s.nowUTC()); ok {
		return cached, nil
	}
	seen := map[string]bool{}
	values := make([]string, 0, 8)
	add := func(v string) {
		if v == "" || seen[v] || len(values) >= maxMaskValues {
			return
		}
		seen[v] = true
		values = append(values, v)
	}
	secrets, _, serr := s.resolvedSecrets(ctx, owner, repo)
	if serr != nil {
		return nil, serr
	}
	for _, v := range secrets {
		add(v)
	}
	vars, verr := s.variableValues(ctx, owner, repo)
	if verr != nil {
		return nil, verr
	}
	for _, v := range vars {
		add(v)
	}
	if job.Spec != nil && job.Spec.Environment != nil {
		envName := strings.TrimSpace(job.Spec.Environment.Name)
		if envName != "" {
			envSecrets, eerr := s.environmentSecretValues(ctx, owner, repo, envName)
			if eerr != nil {
				return nil, eerr
			}
			for _, v := range envSecrets {
				add(v)
			}
			envVars, eerr := s.environmentVariableValues(ctx, owner, repo, envName)
			if eerr != nil {
				return nil, eerr
			}
			for _, v := range envVars {
				add(v)
			}
		}
	}
	sort.Strings(values)
	s.maskCache.put(key, values, s.nowUTC())
	return values, nil
}

// forgetJobMask drops one attempt's cached mask.
func (s *Service) forgetJobMask(repo, runID, jobID string, attempt int) {
	s.maskCache.forget(maskCacheKey(repo, runID, jobID, attempt))
}
