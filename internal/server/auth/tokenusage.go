// tokenusage.go — the shared shape of the static-machine-token last-used
// record (06_server_http.md §8.2.1). The server WRITES it (one CAS object,
// debounced, asynchronous); the Access page READS it once. Keeping the key and
// the JSON codec here means the writer and the page cannot drift.
//
// The object is `auth/token-usage.json`:
//
//	{"version":1,"tokens":{"ci-bot":{"name":"ci-bot","principal":"ci-bot",
//	  "last_used":"2026-11-23T10:00:00Z","source":"203.0.113.7",
//	  "user_agent":"git/2.55.0","count":12}}}
//
// A token is keyed by its configured `name`, or by "sha256:<12 hex>" when it
// has none. The secret never enters this object: the key is a truncated hash,
// which identifies a credential across rotations without being usable as one.
// It is a cache of observations, not authority: an absent/unreadable object
// only makes the Access page say "not recorded yet".
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// TokenUsageKey is the one object holding every static token's last use.
const TokenUsageKey = "auth/token-usage.json"

// tokenUsageVersion is the schema version written by this build. A version 1
// object is readable by any instance of the same release line; an unknown
// version is treated as unreadable (the page degrades, auth is unaffected).
const tokenUsageVersion = 1

// TokenUsageEntry is one token's last successful authentication.
type TokenUsageEntry struct {
	// Name is the configured stable label, empty for an unnamed token.
	Name string `json:"name,omitempty"`
	// Principal is the identity the credential authenticated as.
	Principal string `json:"principal"`
	// LastUsed is RFC 3339 UTC at the observation.
	LastUsed string `json:"last_used"`
	// Source is the remote address (host only, no port).
	Source string `json:"source,omitempty"`
	// UserAgent is the request's User-Agent, truncated.
	UserAgent string `json:"user_agent,omitempty"`
	// Count is the number of recorded observations (write debounce means it
	// counts update windows, not requests).
	Count uint64 `json:"count,omitempty"`
}

// TokenUsageDoc is the whole object: one entry per token identity.
type TokenUsageDoc struct {
	Version int                        `json:"version"`
	Tokens  map[string]TokenUsageEntry `json:"tokens"`
}

// NewTokenUsageDoc returns an empty version-1 document.
func NewTokenUsageDoc() *TokenUsageDoc {
	return &TokenUsageDoc{Version: tokenUsageVersion, Tokens: map[string]TokenUsageEntry{}}
}

// Entry returns the record for a usage key.
func (d *TokenUsageDoc) Entry(key string) (TokenUsageEntry, bool) {
	if d == nil || d.Tokens == nil {
		return TokenUsageEntry{}, false
	}
	e, ok := d.Tokens[key]
	return e, ok
}

// Set stores one observation, never moving LastUsed backwards (two instances
// rolling through the same object can race; the newer observation wins).
func (d *TokenUsageDoc) Set(key string, e TokenUsageEntry) {
	if d.Tokens == nil {
		d.Tokens = map[string]TokenUsageEntry{}
	}
	if old, ok := d.Tokens[key]; ok {
		if old.LastUsed > e.LastUsed && e.LastUsed != "" {
			e.LastUsed = old.LastUsed
		}
		if old.Count > 0 {
			e.Count = old.Count + 1
		}
	} else if e.Count == 0 {
		e.Count = 1
	}
	d.Tokens[key] = e
}

// Encode marshals the document; map keys are sorted, so equal documents encode
// to equal bytes (the store's version is a content hash). Marshalling a struct
// of strings and a string-keyed map cannot fail, so there is no error return.
func (d *TokenUsageDoc) Encode() []byte {
	if d.Version == 0 {
		d.Version = tokenUsageVersion
	}
	raw, _ := json.Marshal(d)
	return raw
}

// DecodeTokenUsage parses the object. An unknown version or malformed JSON is
// an error; callers degrade to "no usage recorded" rather than failing.
func DecodeTokenUsage(raw []byte) (*TokenUsageDoc, error) {
	var d TokenUsageDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("token usage: decode: %w", err)
	}
	switch d.Version {
	case 0:
		d.Version = tokenUsageVersion
	case tokenUsageVersion:
	default:
		return nil, fmt.Errorf("token usage: unsupported version %d", d.Version)
	}
	if d.Tokens == nil {
		d.Tokens = map[string]TokenUsageEntry{}
	}
	return &d, nil
}

// LoadTokenUsage reads the object once. Absent → (nil, nil): a host that never
// authenticated a machine token has no record, which is not an error.
func LoadTokenUsage(ctx context.Context, st store.ObjectStore) (*TokenUsageDoc, error) {
	if st == nil {
		return nil, nil
	}
	raw, _, err := store.GetBytes(ctx, st, TokenUsageKey, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return DecodeTokenUsage(raw)
}

// TokenHashPrefix is the 12-hex-character SHA-256 prefix that identifies a
// credential without revealing it (75 bits of a one-way digest).
func TokenHashPrefix(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:6])
}

// TokenUsageIdentity is the label a token row is known by: its configured name,
// else "sha256:<prefix>" from the resolved secret ("" when the row has no
// resolvable credential).
func TokenUsageIdentity(name, secret string) string {
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	if secret == "" {
		return ""
	}
	return "sha256:" + TokenHashPrefix(secret)
}

// NowRFC3339 is the stored spelling (second precision is plenty for a use
// record; the debounce window is minutes).
func NowRFC3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
