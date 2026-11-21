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
	"strings"

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

// sealSecret encrypts one value with AES-GCM, binding it to (repo, name).
func (s *Service) sealSecret(repo, name, value string) (string, error) {
	if !s.keys.SecretsEnabled() {
		return "", fmt.Errorf("%w: repository secrets need server.secrets_key or server.auth.session_secret", ErrUnavailable)
	}
	gcm := gcmFor(s.keys.Seal)
	nonce, err := randomBytes(gcm.NonceSize())
	if err != nil {
		return "", fmt.Errorf("%w: nonce: %v", ErrUnavailable, err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(value), secretAAD(repo, name))
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// openSecret decrypts one stored value (trusted runs + the log masker only).
func (s *Service) openSecret(repo, name, sealed string) (string, error) {
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
	plain, oerr := gcm.Open(nil, nonce, body, secretAAD(repo, name))
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

// secretValues decrypts every secret of one repository, for a trusted job
// payload and the log masker. A single unreadable envelope fails the fetch
// (fail closed: a job that silently lost its secrets is worse than a clear
// error an operator can fix).
func (s *Service) secretValues(ctx context.Context, owner, repo string) (map[string]string, error) {
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

// maskSecrets replaces every occurrence of every secret value in one log frame
// (best effort, documented: exact substring matches only, longest first so a
// value that contains another is fully replaced).
func maskSecrets(text string, values []string) string {
	if text == "" || len(values) == 0 {
		return text
	}
	sorted := append([]string(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, v := range sorted {
		if len(v) < 4 {
			continue // a 1–3 byte value would shred the log
		}
		text = strings.ReplaceAll(text, v, "***")
	}
	return text
}
