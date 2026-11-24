// Package moderation implements docs/features/16_moderation.md milestone M1
// (PLAN-V2 §1): the per-user block list, the consumer-owned block gate that
// every collaboration write verb consults, the host audit family, and the
// profile-page control. Hide/lock (M2), reports/queues (M3) and rate
// limits/spam signals (M4) are deliberately absent — their object families
// are specified in the feature doc and land with their milestones.
//
// Bucket layout (all keys bucket-relative; the store prefix is applied by
// the store layer):
//
//	users/<principal>/blocks.json        CAS'd block list (identity scope, P1)
//	moderation/audit_state.json         CAS'd seq allocator {"next_seq": N}
//	moderation/audit/<seq:012x>.json    Create-only immutable audit record
//
// The block list lives in the blocker's own identity namespace, exactly as
// the plan pins it: one whole-document object per user, normalized principal
// NAMES, sorted, unique, ≤ BlockLimit entries. There are no per-block
// objects, so adding or removing a block is one CAS (the members.json
// reasoning, docs/features/01 §3) and no LIST ever runs: every read is an
// exact-key conditional GET.
//
// Blocks are NOT access control. A block never changes access.json, rosters
// or policy; reads, stars, watches and forks are unaffected (visibility
// governs reads — PLAN-V2 §M.5). It is an interaction gate for repositories
// owned by the blocker's USER namespace; org-owned repos are the org's
// moderation surface and are never gated by a member's personal block list.
//
// ### Concurrency
//
// Hazard: two tabs of the same user blocking different principals losing an
// update on a blind write. Avoidance: one CAS loop per mutation (≤5
// attempts, then ErrConflict), the 13_concurrency.md §3 discipline — CAS is
// the lock, no lock object, no .lock sidecar, no in-process mutex around a
// store call. The only in-process lock here guards the version-stamped
// block-list cache and is released before every store call.
//
// Hazard: a read-path block cache serving a stale list after an unblock on
// another instance. Avoidance: every gate read revalidates with a
// conditional GET stamped by the CAS version (the access.json LRU pattern),
// so a cached entry is only ever a parse-avoidance, never a freshness
// shortcut; the store stays the truth.
package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// BlockLimit caps one user's block list. A block is a person-level
// relationship at human rate; the cap keeps the whole-document CAS bounded
// (the members.json bound, docs/features/01 §3).
const BlockLimit = 500

// blocksVersion is the on-bucket document version. A reader refuses a
// version it does not know (14 §14.12 version rule); M1 knows exactly one.
const blocksVersion = 1

// dateTimeFmt is the RFC 3339 UTC wire timestamp (07 §2).
const dateTimeFmt = time.RFC3339

// Sentinel errors mapped to HTTP statuses (plain-text bodies, 07 §2).
var (
	// ErrConflict maps to 409: CAS exhaustion after the bounded retry loop,
	// or a block that is already present (the plan's "409 already blocked").
	ErrConflict = errors.New("conflict")
	// ErrInvalid maps to 400: bad principal, self-block, unknown subject,
	// or a list over BlockLimit.
	ErrInvalid = errors.New("invalid")
	// ErrNotFound maps to 404: unknown principal, or an unblock of a
	// principal that is not on the list.
	ErrNotFound = errors.New("not found")
	// ErrForbidden maps to 403: the caller may not read or edit this list
	// (not the principal, and not a host admin).
	ErrForbidden = errors.New("forbidden")
	// ErrUnauthorized maps to 401 + WWW-Authenticate: Bearer.
	ErrUnauthorized = errors.New("authentication required")
)

// Namespaces is the identity seam the moderation service consumes: user
// existence (the profile-existence probe the block API mirrors) and org
// detection (a personal block never applies to an org-owned repo). It is
// satisfied by *identity.Service; tests substitute a fake. Nil means "no
// probe": subjects are not validated and every owner is a user namespace —
// the composition always wires it, and the fail-closed default (apply the
// gate) is the safe side of the ambiguity.
type Namespaces interface {
	UserExists(ctx context.Context, username string) (bool, error)
	IsOrg(ctx context.Context, owner string) (bool, error)
}

// Roles is the P6 seam the M2 floors consume (Resolve for the role,
// CheckRead for the read-gated moderation GET). Satisfied by
// *identity.Service; nil falls back to the principal flags.
type Roles interface {
	Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc)
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// Stream publishes moderation frames onto the one repo collaboration stream
// (composition binds notify.Service.PublishFrame; nil = no live updates).
type Stream func(ctx context.Context, ev StreamEvent)

// BlocksDoc is the stored users/<principal>/blocks.json shape.
type BlocksDoc struct {
	Version   int      `json:"version"`
	Blocked   []string `json:"blocked"`
	UpdatedAt string   `json:"updated_at"`
}

// BlocksKey is the block list key for one principal.
func BlocksKey(principal string) string {
	return "users/" + normPrincipal(principal) + "/blocks.json"
}

type cachedBlocks struct {
	version store.Version
	doc     BlocksDoc
}

// Service owns the block list, the repo moderation document, and the audit
// family over one store.
type Service struct {
	Store store.ObjectStore
	Now   func() time.Time
	// Namespaces validates subjects and distinguishes user from org owners
	// (nil = no probe; see the interface docs).
	Namespaces Namespaces
	// Roles resolves repo roles for the M2 hide/lock floors and the
	// read-gated moderation GET (nil = principal-flag fallback).
	Roles Roles
	// Targets probes hide targets in their owning family (nil = grammar
	// only; composition always wires it).
	Targets TargetProbe
	// Stream publishes moderation SSE frames (nil = no live updates).
	Stream Stream
	// Tasks is the Seam 5 task table the global report rebuild runs on
	// (nil = inline trimming only; the next wired pass rebuilds).
	Tasks TaskRunner
	// ReportNotify publishes the report_filed notification (nil = none). The
	// callback carries the conversation pointer, never the report content.
	ReportNotify ReportNotifier
	// Logger receives best-effort audit-write failures. Nil discards.
	Logger logger

	mu       sync.Mutex
	cache    map[string]cachedBlocks
	docCache map[string]cachedDoc

	// spam is the advisory spam scorer's state (spam.go): one ring per
	// principal, its own mutex, never a store call under it.
	spam scorer
}

// logger is the tiny slog surface this package uses (kept as an interface
// so tests can assert drops without importing log/slog).
type logger interface {
	WarnContext(ctx context.Context, msg string, args ...any)
}

// New builds a Service over st.
func New(st store.ObjectStore) *Service {
	return &Service{
		Store: st, Now: time.Now,
		cache:    map[string]cachedBlocks{},
		docCache: map[string]cachedDoc{},
		spam:     scorer{ring: map[string]*spamRing{}},
	}
}

func (s *Service) nowUTC() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

// normPrincipal lowercases and trims a principal spelling (the identity key,
// Forgejo #370).
func normPrincipal(p string) string { return strings.ToLower(strings.TrimSpace(p)) }

// Blocks returns one principal's block list (a nil Store is an empty list).
// Absent reads as the zero document, never an error.
func (s *Service) Blocks(ctx context.Context, principal string) (BlocksDoc, error) {
	doc, _, err := s.loadBlocks(ctx, normPrincipal(principal))
	return doc, err
}

// Count returns the size of one principal's block list.
func (s *Service) Count(ctx context.Context, principal string) (int, error) {
	doc, err := s.Blocks(ctx, principal)
	return len(doc.Blocked), err
}

// loadBlocks reads the list with the version-stamped cache: the conditional
// GET revalidates every time, and an unchanged version skips only the parse
// (no stale list can be served).
func (s *Service) loadBlocks(ctx context.Context, principal string) (BlocksDoc, store.Version, error) {
	key := BlocksKey(principal)
	s.mu.Lock()
	cached, have := s.cache[key]
	s.mu.Unlock()

	opts := store.GetOptions{}
	if have {
		opts.IfNoneMatch = cached.version
	}
	raw, meta, err := store.GetBytes(ctx, s.Store, key, opts)
	if err != nil {
		if store.IsNotFound(err) {
			s.mu.Lock()
			delete(s.cache, key)
			s.mu.Unlock()
			return BlocksDoc{Version: blocksVersion, Blocked: []string{}}, "", nil
		}
		return BlocksDoc{}, "", err
	}
	if raw == nil {
		// Conditional GET: the known version is still current (the only way
		// GetBytes answers a nil body without an error).
		return cached.doc, cached.version, nil
	}
	var doc BlocksDoc
	if uerr := json.Unmarshal(raw, &doc); uerr != nil {
		// A corrupt object is a SERVER fault (503-class), never a client 400:
		// the gate fails closed rather than reading it as an empty list.
		return BlocksDoc{}, "", fmt.Errorf("corrupt block list %s: %v", key, uerr)
	}
	if doc.Version > blocksVersion {
		return BlocksDoc{}, "", fmt.Errorf("unsupported block list version %d at %s", doc.Version, key)
	}
	doc.Blocked = canonicalList(doc.Blocked)
	s.mu.Lock()
	s.cache[key] = cachedBlocks{version: meta.Version, doc: doc}
	s.mu.Unlock()
	return doc, meta.Version, nil
}

// BlockedBy reports whether blocker has blocked subject. Missing documents,
// blank spellings and a self-pair are all "not blocked" — the gate's cheap
// negative path.
func (s *Service) BlockedBy(ctx context.Context, blocker, subject string) (bool, error) {
	blocker, subject = normPrincipal(blocker), normPrincipal(subject)
	if blocker == "" || subject == "" || blocker == subject {
		return false, nil
	}
	doc, err := s.Blocks(ctx, blocker)
	if err != nil {
		return false, err
	}
	i := sort.SearchStrings(doc.Blocked, subject)
	return i < len(doc.Blocked) && doc.Blocked[i] == subject, nil
}

// BlockedFromRepo is the consumer-facing gate (PLAN-V2 §M.5): true when the
// actor is blocked by the repository owner's USER namespace. Org-owned repos
// are never gated by a member's personal block list, and the owner can never
// be blocked out of their own namespace. The host-admin exemption is the
// consumer's job (it holds the Principal flags).
func (s *Service) BlockedFromRepo(ctx context.Context, owner, repo, actor string) (bool, string, error) {
	owner, actor = normPrincipal(owner), normPrincipal(actor)
	if owner == "" || actor == "" || owner == actor {
		return false, "", nil
	}
	if s.Namespaces != nil {
		org, err := s.Namespaces.IsOrg(ctx, owner)
		if err != nil {
			return false, "", err
		}
		if org {
			return false, "", nil
		}
	}
	blocked, err := s.BlockedBy(ctx, owner, actor)
	if err != nil || !blocked {
		return false, "", err
	}
	// The reason is deliberately generic: the blocked principal is never told
	// a block exists (PLAN-V2 §M.12 decision (b)). Consumers render the
	// shared refusal text, not this string.
	return true, "interaction not permitted", nil
}

// Block adds subject to blocker's list. changed=false means the subject was
// already blocked: the state is idempotent, no write and no audit record is
// produced, and the HTTP layer answers the plan's 409.
func (s *Service) Block(ctx context.Context, blocker, subject string) (int, bool, error) {
	blocker, subject = normPrincipal(blocker), normPrincipal(subject)
	if err := s.validatePair(blocker, subject); err != nil {
		return 0, false, err
	}
	if err := s.requireKnownSubject(ctx, subject); err != nil {
		return 0, false, err
	}
	doc, changed, err := s.mutate(ctx, blocker, func(doc *BlocksDoc) (bool, error) {
		i := sort.SearchStrings(doc.Blocked, subject)
		if i < len(doc.Blocked) && doc.Blocked[i] == subject {
			return false, nil
		}
		if len(doc.Blocked) >= BlockLimit {
			return false, fmt.Errorf("%w: block list is full (max %d)", ErrInvalid, BlockLimit)
		}
		doc.Blocked = append(doc.Blocked, subject)
		sort.Strings(doc.Blocked)
		return true, nil
	})
	if err != nil {
		return 0, false, err
	}
	if !changed {
		return doc.Version, false, nil
	}
	// Audit after the commit: the block IS the state change, and a failed
	// audit write must not un-block (the notify activity-log precedent:
	// honest gap, logged, never a phantom revert).
	if aerr := s.appendAudit(ctx, blocker, ActionBlock, "user:"+subject, "", ""); aerr != nil {
		s.warn(ctx, "moderation: block audit write failed", "actor", blocker, "subject", subject, "err", aerr)
	}
	return doc.Version, true, nil
}

// Unblock removes subject from blocker's list. found=false means the subject
// was not blocked (no write, no audit; the HTTP layer answers 404).
func (s *Service) Unblock(ctx context.Context, blocker, subject string) (int, bool, error) {
	blocker, subject = normPrincipal(blocker), normPrincipal(subject)
	if err := s.validatePair(blocker, subject); err != nil {
		return 0, false, err
	}
	doc, changed, err := s.mutate(ctx, blocker, func(doc *BlocksDoc) (bool, error) {
		i := sort.SearchStrings(doc.Blocked, subject)
		if i >= len(doc.Blocked) || doc.Blocked[i] != subject {
			return false, nil
		}
		doc.Blocked = append(doc.Blocked[:i], doc.Blocked[i+1:]...)
		return true, nil
	})
	if err != nil {
		return 0, false, err
	}
	if !changed {
		return doc.Version, false, nil
	}
	if aerr := s.appendAudit(ctx, blocker, ActionUnblock, "user:"+subject, "", ""); aerr != nil {
		s.warn(ctx, "moderation: unblock audit write failed", "actor", blocker, "subject", subject, "err", aerr)
	}
	return doc.Version, true, nil
}

// validatePair rejects malformed and self-referential pairs.
func (s *Service) validatePair(blocker, subject string) error {
	if !identity.ValidPrincipal(blocker) {
		return fmt.Errorf("%w: invalid principal %q", ErrInvalid, blocker)
	}
	if !identity.ValidPrincipal(subject) {
		return fmt.Errorf("%w: invalid subject %q", ErrInvalid, subject)
	}
	if blocker == subject {
		return fmt.Errorf("%w: you cannot block yourself", ErrInvalid)
	}
	return nil
}

// requireKnownSubject is the profile-existence probe the plan's 400 requires.
func (s *Service) requireKnownSubject(ctx context.Context, subject string) error {
	if s.Namespaces == nil {
		return nil
	}
	exists, err := s.Namespaces.UserExists(ctx, subject)
	if err != nil {
		return fmt.Errorf("identity unavailable: %v", err)
	}
	if !exists {
		return fmt.Errorf("%w: unknown subject %q", ErrInvalid, subject)
	}
	return nil
}

// mutate runs the bounded whole-document CAS loop. The mutator returns
// changed=false to stop without a write (idempotent duplicate).
func (s *Service) mutate(ctx context.Context, principal string, f func(doc *BlocksDoc) (bool, error)) (BlocksDoc, bool, error) {
	const attempts = 5
	key := BlocksKey(principal)
	for i := 0; i < attempts; i++ {
		doc, ver, err := s.loadBlocks(ctx, principal)
		if err != nil {
			return BlocksDoc{}, false, err
		}
		changed, ferr := f(&doc)
		if ferr != nil {
			return BlocksDoc{}, false, ferr
		}
		if !changed {
			return doc, false, nil
		}
		doc.Version = blocksVersion
		doc.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		doc.Blocked = canonicalList(doc.Blocked)
		body, merr := json.Marshal(doc)
		if merr != nil {
			return BlocksDoc{}, false, merr
		}
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		if ver == "" {
			opts.Mode = store.PutCreate
		}
		meta, perr := store.PutBytes(ctx, s.Store, key, body, opts)
		if perr == nil {
			s.mu.Lock()
			s.cache[key] = cachedBlocks{version: meta.Version, doc: doc}
			s.mu.Unlock()
			return doc, true, nil
		}
		if !store.IsPreconditionFailed(perr) {
			return BlocksDoc{}, false, perr
		}
		// A concurrent writer won; drop the cached version and retry.
		s.mu.Lock()
		delete(s.cache, key)
		s.mu.Unlock()
	}
	return BlocksDoc{}, false, fmt.Errorf("%w: %s changed concurrently; reload and retry", ErrConflict, key)
}

// canonicalList normalizes, dedupes, sorts and bounds a list for storage.
func canonicalList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, p := range in {
		n := normPrincipal(p)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (s *Service) warn(ctx context.Context, msg string, args ...any) {
	if s.Logger != nil {
		s.Logger.WarnContext(ctx, msg, args...)
	}
}
