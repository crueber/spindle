// Package packages implements the OCI Distribution registry served per
// repository (docs/features/14_packages.md). The package is the repository:
// <host>/<owner>/<repo> addresses repos/<owner>/<repo>/packages/registry and
// permissions are that repository's roles. Storage is bucket-native and
// append-only — blobs are content-addressed store objects, manifests are
// blobs with a media type, tags are CAS JSON pointers, and chunked uploads
// stage under uploads/<id>/data with a TTL.
//
// Nothing here enumerates the bucket on a hot path (law 4): tag resolution is
// one exact GET, a manifest read is tag-then-manifest, a blob read is one
// HEAD/GET, and the only LIST is the user-triggered tags/list page. There is
// no in-process lock across a store call (law 3): parallel layer uploads are
// independent objects.
package packages

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Limits (docs/features/14 §5). Zero Service fields resolve to these.
const (
	// DefaultMaxBlobBytes is GHCR's per-layer ceiling (10 GB) rather than a
	// smaller spindle-specific cap: a registry that refuses a layer the
	// reference implementation accepts is a parity bug, not prudence.
	DefaultMaxBlobBytes     int64 = 10 << 30
	DefaultMaxManifestBytes int64 = 4 << 20
	DefaultUploadTTL              = 24 * time.Hour
	DefaultTokenTTL               = 5 * time.Minute
	// DefaultBlobGrace keeps a just-finalized blob out of the sweep: a
	// manifest PUT follows its layer uploads, and a pass landing in that
	// window would otherwise delete bytes the push is about to reference.
	DefaultBlobGrace = 10 * time.Minute
	// DefaultGCInterval is the minimum spacing between maintainer sweeps of
	// one repo (the in-memory due gate; a wiped instance only repeats work).
	DefaultGCInterval = 15 * time.Minute
)

// RegistryPrefix is the bucket prefix this feature owns (§2). It is
// spindle-only and never collides with a Rust-era key.
const registrySuffix = "/packages/registry"

// RoleService is the narrow P6 surface this package consumes (the same shape
// internal/releases and internal/checks use: satisfied by *identity.Service,
// tests substitute a fake). Core never imports this package.
//
// CheckPush is the git receive-pack gate VERBATIM (identity.CheckPush, the
// Forgejo #347 rule): push authorisation is not reimplemented here, it is the
// same function the git transport enforces — self-namespace owner, org roster,
// explicit binding, host admin; the host-wide write flag grants nothing. A
// registry that resolved roles on its own drifted from git once (an owner with
// no access.json could git-push but not docker-push, docs/AUDIT-V1.md); reusing
// the gate makes that class of divergence impossible.
type RoleService interface {
	Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc)
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
	CheckPush(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// Authenticator resolves the request principal through Seam 2 (the server's
// AuthService, injected by composition). Nil falls back to anonymous.
type Authenticator func(r *http.Request) (auth.Principal, *auth.AuthError)

// Service is the registry store client. Store and Roles are the only
// required fields; the rest are optional wiring (nil = the documented
// fallback).
type Service struct {
	Store store.ObjectStore
	Roles RoleService
	// Features is the per-repo [features] projection (Forgejo #522): off ⇒
	// routes 404. Nil or ok=false fails open to enabled (the display
	// metadata rule: an unreadable settings doc must never break the
	// registry).
	Features func(ctx context.Context, owner, repo string) (config.ResolvedFeatures, bool)
	// Policy resolves the effective per-repo registry policy (docs/features/14
	// §5): the settings [packages] section resolved over the instance default
	// (`packages.quota_bytes`). The projection owns the resolution, so it
	// always answers a complete policy; nil is unlimited with no retention —
	// the compatible behavior for tests and un-wired builds (an unreadable
	// settings doc must never invent a quota nobody set).
	Policy func(ctx context.Context, owner, repo string) config.PackagesPolicy
	// Exists reports whether the repository exists (nil → assume it does:
	// unit tests without a registry). A missing repo answers NAME_UNKNOWN.
	Exists func(ctx context.Context, owner, repo string) (bool, error)

	MaxBlobBytes     int64
	MaxManifestBytes int64
	UploadTTL        time.Duration
	TokenTTL         time.Duration
	// BlobGrace protects a just-finalized blob from the maintainer sweep
	// while its manifest is still in flight (0 → DefaultBlobGrace).
	BlobGrace time.Duration
	// GCInterval is the maintainer sweep's per-repo due spacing (0 →
	// DefaultGCInterval). In-memory only: a restart merely repeats a pass.
	GCInterval time.Duration
	// AnonymousRead is the auth.anonymous_read fallback when Roles is nil
	// (tests). Production always wires Roles, whose CheckRead is the
	// authority (the Forgejo #345 visibility rule).
	AnonymousRead bool
	// SpoolDir stages request bodies and upload finalization (the LFS
	// §6.2 pattern: never buffer a blob in memory). Empty = the OS temp dir.
	SpoolDir string
	// PublicURL is the externally reachable base (server.public_url) used
	// in the Bearer realm; empty derives it from the request.
	PublicURL string
	// ServiceName is the token `service` value (default: the request host).
	ServiceName string

	// Repos enumerates this instance's [owner, repo] pairs for the
	// instance-scope GC (nil → a delimiter listing over repos/, the
	// maintainer-pass LIST; never a request path). Composition may inject
	// identity's registry walk instead.
	Repos func(ctx context.Context) ([][2]string, error)
	// Audit records one administrative mutation in the host audit family
	// (nil = no audit family on this build; the GC trigger then records no
	// entry rather than failing — docs/features/14_packages.md §6).
	Audit AuditFunc
	// Logf is the package's log sink (nil → log.Printf): a failed summary or
	// audit write is reported, never silently swallowed.
	Logf func(format string, args ...any)

	Now func() time.Time

	secretOnce sync.Once
	secret     []byte
	// TokenSecret is the HMAC key for registry bearer tokens. Empty → a
	// per-process random key (tokens survive only this process; Docker
	// re-mints on every operation, so the practical effect is re-login
	// after a restart — documented in 14_packages.md §4).
	TokenSecret []byte

	// gcMu guards the sweep bookkeeping: recently finalized blobs (the
	// grace guard, §6) and the per-repo last-sweep clock. Both are caches —
	// no lock is held across a store call.
	gcMu    sync.Mutex
	recent  map[string]time.Time
	sweptAt map[string]time.Time
}

// New builds a Service over st and the role resolver.
func New(st store.ObjectStore, roles RoleService) *Service {
	return &Service{Store: st, Roles: roles, Now: time.Now}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) maxBlob() int64 {
	if s.MaxBlobBytes > 0 {
		return s.MaxBlobBytes
	}
	return DefaultMaxBlobBytes
}

func (s *Service) maxManifest() int64 {
	if s.MaxManifestBytes > 0 {
		return s.MaxManifestBytes
	}
	return DefaultMaxManifestBytes
}

func (s *Service) uploadTTL() time.Duration {
	if s.UploadTTL > 0 {
		return s.UploadTTL
	}
	return DefaultUploadTTL
}

func (s *Service) tokenTTL() time.Duration {
	if s.TokenTTL > 0 {
		return s.TokenTTL
	}
	return DefaultTokenTTL
}

// key returns the HMAC secret, generating a per-process key when none is
// configured (sync.Once: the only mutable state in the service, never held
// across a store call).
func (s *Service) key() []byte {
	s.secretOnce.Do(func() {
		if len(s.TokenSecret) > 0 {
			s.secret = s.TokenSecret
			return
		}
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			k = []byte(fmt.Sprintf("packages-ephemeral-%d", time.Now().UnixNano()))
		}
		s.secret = k
	})
	return s.secret
}

// --- keys (§2) ---------------------------------------------------------------

// RepoBase is the repository's registry prefix.
func RepoBase(owner, repo string) string {
	return "repos/" + owner + "/" + repo + registrySuffix
}

func blobKey(owner, repo, hexDigest string) string {
	return RepoBase(owner, repo) + "/blobs/sha256/" + hexDigest
}

func manifestKey(owner, repo, hexDigest string) string {
	return RepoBase(owner, repo) + "/manifests/sha256/" + hexDigest
}

func tagKey(owner, repo, tag string) string {
	return RepoBase(owner, repo) + "/tags/" + tag
}

func uploadKey(owner, repo, id string) string {
	return RepoBase(owner, repo) + "/uploads/" + id + "/data"
}

// abortedKey is the tombstone an aborted upload leaves behind (empty object):
// without a metadata sidecar, a deleted staging object would otherwise let the
// same id resume as a fresh upload (§3 DELETE …/uploads/{id} aborts it).
func abortedKey(owner, repo, id string) string {
	return RepoBase(owner, repo) + "/uploads/" + id + "/aborted"
}

func tagsPrefix(owner, repo string) string {
	return RepoBase(owner, repo) + "/tags/"
}

func blobsPrefix(owner, repo string) string {
	return RepoBase(owner, repo) + "/blobs/sha256/"
}

func manifestsPrefix(owner, repo string) string {
	return RepoBase(owner, repo) + "/manifests/sha256/"
}

func uploadsPrefix(owner, repo string) string {
	return RepoBase(owner, repo) + "/uploads/"
}

// validateRepo checks the owner/repo shape (path safety) before it becomes a
// store key. Callers get ErrNameUnknown rather than a traversal.
func validateRepo(owner, repo string) *Error {
	if _, err := git.ParseRepoId(owner + "/" + repo); err != nil {
		return errNameUnknown(owner + "/" + repo)
	}
	return nil
}

// --- digests and tags ---------------------------------------------------------

var (
	digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	tagRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// IsDigest reports whether ref is a well-formed sha256 digest.
func IsDigest(ref string) bool { return digestRe.MatchString(ref) }

// DigestHex returns the hex part of a sha256 digest.
func DigestHex(ref string) string { return strings.TrimPrefix(ref, "sha256:") }

// IsTag reports whether ref is a valid OCI tag ([A-Za-z0-9_][A-Za-z0-9._-]{0,127}).
func IsTag(ref string) bool { return tagRe.MatchString(ref) && ref != "." && ref != ".." }

// DigestBytes is the canonical sha256 digest of b.
func DigestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// --- errors -------------------------------------------------------------------

// Error is one OCI error entry; Status is the HTTP status the handler uses.
type Error struct {
	Status  int
	Code    string
	Message string
	Detail  any
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func ociErr(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

func errNameUnknown(name string) *Error {
	e := ociErr(404, CodeNameUnknown, "repository name not known to registry")
	e.Detail = map[string]any{"name": name}
	return e
}

func errManifestUnknown(ref string) *Error {
	e := ociErr(404, CodeManifestUnknown, "manifest unknown")
	e.Detail = map[string]any{"reference": ref}
	return e
}

func errBlobUnknown(ref string) *Error {
	e := ociErr(404, CodeBlobUnknown, "blob unknown to registry")
	e.Detail = map[string]any{"digest": ref}
	return e
}

func errDigestInvalid(msg string) *Error {
	return ociErr(400, CodeDigestInvalid, msg)
}

func errUploadUnknown(id string) *Error {
	e := ociErr(404, CodeBlobUploadUnknown, "blob upload unknown to registry")
	e.Detail = map[string]any{"id": id}
	return e
}

func errSizeInvalid(msg string) *Error {
	return ociErr(413, CodeSizeInvalid, msg)
}

// errQuotaExceeded is the named 413 the push commit point returns when a
// repository's stored bytes plus what is being written would cross its quota
// (docs/features/14 §5). The spec has no QUOTA code, so DENIED carries the
// name in the message and the numbers in detail — a client prints the message,
// and an operator reading a CI log sees usage against quota, not "413".
func errQuotaExceeded(usage, quota, requested int64) *Error {
	over := usage + requested - quota
	if over < 0 {
		over = 0
	}
	e := ociErr(http.StatusRequestEntityTooLarge, CodeDenied,
		"packages quota exceeded: repo holds "+humanBytes(usage)+" of "+humanBytes(quota)+
			" quota; this push needs "+humanBytes(requested)+" more ("+humanBytes(over)+" over)")
	e.Detail = map[string]any{"usage": usage, "quota": quota, "requested": requested}
	return e
}

func errDenied(msg string) *Error {
	return ociErr(403, CodeDenied, msg)
}

func errConflict(msg string) *Error {
	e := ociErr(409, CodeDenied, msg)
	return e
}

func errInternal(err error) *Error {
	return &Error{Status: 500, Code: CodeUnknown, Message: err.Error()}
}

func errUnauthorized() *Error {
	return ociErr(401, CodeUnauthorized, "authentication required")
}

// mapStoreErr maps a store failure onto the OCI error vocabulary.
func mapStoreErr(err error) *Error {
	if err == nil {
		return nil
	}
	var oe *Error
	if errors.As(err, &oe) {
		return oe
	}
	if store.IsNotFound(err) {
		return ociErr(404, CodeBlobUnknown, "not found")
	}
	if store.IsInvalidArgument(err) {
		return errDigestInvalid(err.Error())
	}
	return errInternal(err)
}

// --- authorization -------------------------------------------------------------

// FeaturesOn reports whether the repo's packages flag is enabled (fail-open
// when the settings projection is absent or unreadable).
func (s *Service) FeaturesOn(ctx context.Context, owner, repo string) bool {
	if s.Features == nil {
		return true
	}
	f, ok := s.Features(ctx, owner, repo)
	if !ok {
		return true
	}
	return f.Packages
}

// RepoExists reports whether the repository is registered (nil hook →
// true: unit tests and un-wired builds).
func (s *Service) RepoExists(ctx context.Context, owner, repo string) (bool, error) {
	if s.Exists == nil {
		return true, nil
	}
	return s.Exists(ctx, owner, repo)
}

// roleRank orders the P6 ladder read < triage < write < maintain < admin.
func roleRank(role string) int {
	switch identity.Role(strings.ToLower(role)) {
	case identity.RoleRead:
		return 1
	case identity.RoleTriage:
		return 2
	case identity.RoleWrite:
		return 3
	case identity.RoleMaintain:
		return 4
	case identity.RoleAdmin:
		return 5
	}
	return 0
}

// CanPull enforces read (pull) on the repository: the identity require_read
// gate is the authority when wired, so anonymous callers reach public repos
// and a flag-only fallback keeps instances without identity working. There is
// no host-write shortcut: the git read gate grants a host-wide write flag
// nothing (Forgejo #374), and the registry has no privilege of its own.
func (s *Service) CanPull(ctx context.Context, owner, repo string, p auth.Principal) *Error {
	if p.Admin {
		return nil
	}
	if s.Roles == nil {
		if p.Anonymous && !s.AnonymousRead {
			return errUnauthorized()
		}
		return nil
	}
	if aerr := s.Roles.CheckRead(ctx, owner, repo, p); aerr != nil {
		switch aerr.Kind {
		case auth.ErrForbidden:
			return errDenied(aerr.Why)
		case auth.ErrUnavailable:
			return errInternal(errors.New(aerr.Why))
		default:
			return errUnauthorized()
		}
	}
	return nil
}

// CanPush enforces write on the repository (scope push ⇒ the git push rule).
// It calls the SAME gate the receive-pack path enforces — identity.CheckPush —
// so the owner-of-a-repo-without-access.json case, org rosters, explicit
// bindings and the host-admin bypass behave identically on both transports
// (docs/AUDIT-V1.md finding 1).
func (s *Service) CanPush(ctx context.Context, owner, repo string, p auth.Principal) *Error {
	if s.Roles == nil {
		if p.Admin {
			return nil
		}
		if p.Anonymous {
			return errUnauthorized()
		}
		if !p.Write {
			return errDenied("write access required")
		}
		return nil
	}
	if aerr := s.Roles.CheckPush(ctx, owner, repo, p); aerr != nil {
		switch aerr.Kind {
		case auth.ErrForbidden:
			return errDenied(aerr.Why)
		case auth.ErrUnavailable:
			return errInternal(errors.New(aerr.Why))
		default:
			return errUnauthorized()
		}
	}
	return nil
}

// --- manifests ----------------------------------------------------------------

// tagDoc is the CAS pointer stored at tags/<tag> (§2).
type tagDoc struct {
	Digest    string `json:"digest"`
	MediaType string `json:"media_type"`
	UpdatedAt string `json:"updated_at"`
}

// Manifest is a resolved manifest read.
type Manifest struct {
	Bytes     []byte
	Digest    string // "sha256:<hex>"
	MediaType string
	Version   store.Version
}

// GetManifest resolves ref (tag or digest) and returns the manifest bytes.
// Tag lookups are 1 tag read + 1 manifest read; digest lookups skip the
// pointer (the §5 pull budget).
func (s *Service) GetManifest(ctx context.Context, owner, repo, ref string) (*Manifest, *Error) {
	if err := validateRepo(owner, repo); err != nil {
		return nil, err
	}
	var hexDigest string
	switch {
	case IsDigest(ref):
		hexDigest = DigestHex(ref)
	case IsTag(ref):
		raw, _, err := store.GetBytes(ctx, s.Store, tagKey(owner, repo, ref), store.GetOptions{})
		if err != nil {
			if store.IsNotFound(err) {
				return nil, errManifestUnknown(ref)
			}
			return nil, mapStoreErr(err)
		}
		var doc tagDoc
		if err := json.Unmarshal(raw, &doc); err != nil || !IsDigest(doc.Digest) {
			return nil, errManifestUnknown(ref)
		}
		hexDigest = DigestHex(doc.Digest)
	default:
		return nil, errManifestUnknown(ref)
	}
	raw, meta, err := store.GetBytes(ctx, s.Store, manifestKey(owner, repo, hexDigest), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, errManifestUnknown(ref)
		}
		return nil, mapStoreErr(err)
	}
	return &Manifest{
		Bytes:     raw,
		Digest:    "sha256:" + hexDigest,
		MediaType: manifestMediaType(raw),
		Version:   meta.Version,
	}, nil
}

// manifestMediaType recovers the media type from the manifest body (the
// spec requires the mediaType member; §2 has no sidecar).
func manifestMediaType(raw []byte) string {
	var probe struct {
		MediaType string `json:"mediaType"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil && probe.MediaType != "" {
		return probe.MediaType
	}
	return "application/vnd.oci.image.manifest.v1+json"
}

// PutManifest stores the manifest bytes (a content-addressed blob) and, for a
// tag ref, CASes the pointer. A digest ref must match the computed digest.
// Returns the digest.
//
// This is the push COMMIT POINT and therefore where the repository quota is
// enforced (docs/features/14 §5): the counter is read once, the projected
// footprint (stored bytes + this manifest when it is new) is compared with the
// quota, and an over-quota push is refused with a named 413 BEFORE anything is
// written — no manifest object, no tag pointer. Layer bytes are already in the
// counter (finalize accounts them), so "usage plus the new blobs" is exactly
// the counter plus this body; `FinalizeUpload` refuses an over-quota layer for
// the same reason, so a push cannot stream past the bound and only fail here.
func (s *Service) PutManifest(ctx context.Context, owner, repo, ref string, body []byte, contentType string) (string, *Error) {
	if err := validateRepo(owner, repo); err != nil {
		return "", err
	}
	if int64(len(body)) > s.maxManifest() {
		return "", errSizeInvalid("manifest exceeds packages.max_manifest_bytes")
	}
	if !IsDigest(ref) && !IsTag(ref) {
		return "", errManifestUnknown(ref)
	}
	digest := DigestBytes(body)
	if IsDigest(ref) && ref != digest {
		return "", errDigestInvalid("provided digest does not match the manifest bytes")
	}
	hexDigest := DigestHex(digest)
	mt := manifestMediaType(body)
	if contentType != "" {
		mt = contentType
	}
	// The counter drives both the quota and the page. A failure to read it is
	// a 500 rather than an unenforced push: the quota is a bound, and a bound
	// that silently disappears on a store hiccup is not one.
	usage, _, uerr := s.readUsage(ctx, owner, repo)
	if uerr != nil {
		return "", errInternal(uerr)
	}
	pol := s.policyFor(ctx, owner, repo)
	// Is this manifest object already stored? A re-push of identical bytes is
	// idempotent and must not be counted twice (S3 ETags are content hashes,
	// never counters).
	existing, herr := s.Store.Head(ctx, manifestKey(owner, repo, hexDigest))
	if herr != nil {
		return "", mapStoreErr(herr)
	}
	isNew := existing == nil
	if pol.QuotaBytes > 0 && isNew {
		if usage.Bytes+int64(len(body)) > pol.QuotaBytes {
			return "", errQuotaExceeded(usage.Bytes, pol.QuotaBytes, int64(len(body)))
		}
	}
	// The manifest object is idempotent content-addressed bytes (S3 keeps
	// the same version for identical bytes — never assume a counter).
	if _, err := s.Store.Put(ctx, manifestKey(owner, repo, hexDigest), store.PutBody{Bytes: body}, store.PutOptions{
		Mode: store.PutOverwrite, ContentType: mt, Immutable: true,
	}); err != nil {
		return "", mapStoreErr(err)
	}
	if isNew {
		// A lost increment is repaired by the next sweep's reconcile; it must
		// never fail a manifest that is already committed.
		s.adjustUsage(ctx, owner, repo, Usage{
			Bytes: int64(len(body)), ManifestBytes: int64(len(body)), Manifests: 1,
		})
	}
	if IsTag(ref) {
		doc := tagDoc{Digest: digest, MediaType: mt, UpdatedAt: s.now().Format(time.RFC3339)}
		raw, _ := json.Marshal(doc)
		if err := s.putTag(ctx, owner, repo, ref, raw); err != nil {
			return "", err
		}
	}
	return digest, nil
}

// putTag CASes the tag pointer: one pointer read for the version, then
// create-if-absent or update-if-known, one retry against the version that beat
// us, then 409 (the §2 race rule). The initial read is what keeps the happy
// path at one CAS attempt — a blind create-then-update would spend its retry
// on every existing tag.
func (s *Service) putTag(ctx context.Context, owner, repo, tag string, raw []byte) *Error {
	key := tagKey(owner, repo, tag)
	_, meta, err := store.GetBytes(ctx, s.Store, key, store.GetOptions{})
	if err != nil && !store.IsNotFound(err) {
		return mapStoreErr(err)
	}
	known := store.Version("")
	if err == nil {
		known = meta.Version
	}
	for attempt := 0; attempt < 2; attempt++ {
		opts := store.PutOptions{Mode: store.PutCreate}
		if known != "" {
			opts = store.PutOptions{Mode: store.PutUpdate, IfVersion: known}
		}
		_, perr := s.Store.Put(ctx, key, store.PutBody{Bytes: raw}, opts)
		if perr == nil {
			return nil
		}
		if !store.IsPreconditionFailed(perr) {
			return mapStoreErr(perr)
		}
		cur, ok := store.PreconditionCurrent(perr)
		if !ok || cur == "" {
			// Re-read the pointer for the retry token (the CAS loss told us
			// nothing parseable).
			_, meta, gerr := store.GetBytes(ctx, s.Store, key, store.GetOptions{})
			if gerr != nil {
				return mapStoreErr(gerr)
			}
			cur = meta.Version
		}
		known = cur
	}
	return errConflict("tag pointer changed concurrently; retry the push")
}

// DeleteManifest removes a tag pointer (tag ref) or a manifest VERSION (digest
// ref: the manifest, the direct platform manifests an index owns, and every
// tag pointer aimed at any of them — GHCR drops the version's tags with the
// version). Blobs stay until the maintainer sweep (§6).
//
// An unknown tag or digest is a real 404 (MANIFEST_UNKNOWN), never a silent
// 202: a delete that hides a typo is worse than one that says not-found.
func (s *Service) DeleteManifest(ctx context.Context, owner, repo, ref string) *Error {
	if err := validateRepo(owner, repo); err != nil {
		return err
	}
	switch {
	case IsTag(ref):
		key := tagKey(owner, repo, ref)
		meta, err := s.Store.Head(ctx, key)
		if err != nil {
			return mapStoreErr(err)
		}
		if meta == nil {
			return errManifestUnknown(ref)
		}
		if err := s.Store.Delete(ctx, key, meta.Version); err != nil {
			if store.IsNotFound(err) {
				return errManifestUnknown(ref) // lost the race to another delete
			}
			return mapStoreErr(err)
		}
		return nil
	case IsDigest(ref):
		return s.deleteManifestTree(ctx, owner, repo, DigestHex(ref))
	default:
		return errManifestUnknown(ref)
	}
}

// deleteManifestTree deletes a manifest digest and the platform manifests an
// index owns (GHCR's "delete the version"): the index bytes name their
// children, so the version is the whole tree. A child that another stored
// manifest also references is kept (manifests are content-addressed and may be
// shared). Deeper nesting is not produced by any client, so only the direct
// children are considered — documented in §6.
//
// Deleting the root also drops every tag aiming at it (and at any child it
// took with it), so `tags/list` never advertises a dead pointer.
func (s *Service) deleteManifestTree(ctx context.Context, owner, repo, rootHex string) *Error {
	rootKey := manifestKey(owner, repo, rootHex)
	raw, _, err := store.GetBytes(ctx, s.Store, rootKey, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return errManifestUnknown("sha256:" + rootHex)
		}
		return mapStoreErr(err)
	}
	children := indexChildren(raw)

	// References from every OTHER stored manifest keep a shared child alive.
	keep := map[string]bool{}
	if len(children) > 0 {
		prefix := manifestsPrefix(owner, repo)
		var keys []string
		if lerr := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
			keys = append(keys, m.Key)
			return nil
		}); lerr != nil {
			return mapStoreErr(lerr)
		}
		for _, key := range keys {
			hex := strings.TrimPrefix(key, prefix)
			if hex == rootHex {
				continue
			}
			body, _, gerr := store.GetBytes(ctx, s.Store, key, store.GetOptions{})
			if gerr != nil {
				if store.IsNotFound(gerr) {
					continue
				}
				return mapStoreErr(gerr)
			}
			for _, child := range indexChildren(body) {
				keep[child] = true
			}
		}
	}

	deleted := map[string]bool{"sha256:" + rootHex: true}
	// The counter drops with the version tree; the blobs stay until the sweep
	// (which reconciles the absolute number anyway).
	freed := int64(len(raw))
	freedCount := int64(1)
	if derr := s.Store.Delete(ctx, rootKey, ""); derr != nil {
		return mapStoreErr(derr)
	}
	for _, child := range children {
		if child == rootHex || keep[child] {
			continue
		}
		if meta, herr := s.Store.Head(ctx, manifestKey(owner, repo, child)); herr == nil && meta != nil {
			freed += meta.Size
			freedCount++
		}
		if derr := s.Store.Delete(ctx, manifestKey(owner, repo, child), ""); derr != nil && !store.IsNotFound(derr) {
			return mapStoreErr(derr)
		}
		deleted["sha256:"+child] = true
	}
	s.adjustUsage(ctx, owner, repo, Usage{Bytes: -freed, ManifestBytes: -freed, Manifests: -freedCount})
	return s.dropTagPointersFor(ctx, owner, repo, deleted)
}

// indexChildren returns the manifest digests (hex) an index body references.
func indexChildren(raw []byte) []string {
	var doc struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	out := make([]string, 0, len(doc.Manifests))
	for _, m := range doc.Manifests {
		if IsDigest(m.Digest) {
			out = append(out, DigestHex(m.Digest))
		}
	}
	return out
}

// dropTagPointersFor deletes every tag pointer whose digest is in the set (a
// deleted version tree), matching the full "sha256:" spelling each pointer
// stores. One prefix LIST plus a pointer read per tag — a delete-time cost,
// never a request hot path (§6). A pointer whose version moved is left alone
// (the tag now names a different manifest); a malformed pointer is deleted,
// since it can never resolve.
func (s *Service) dropTagPointersFor(ctx context.Context, owner, repo string, digests map[string]bool) *Error {
	prefix := tagsPrefix(owner, repo)
	var keys []string
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		keys = append(keys, m.Key)
		return nil
	}); err != nil {
		return mapStoreErr(err)
	}
	for _, key := range keys {
		raw, meta, err := store.GetBytes(ctx, s.Store, key, store.GetOptions{})
		if err != nil {
			if store.IsNotFound(err) {
				continue
			}
			return mapStoreErr(err)
		}
		var doc tagDoc
		malformed := json.Unmarshal(raw, &doc) != nil
		if !malformed && !digests[doc.Digest] {
			continue
		}
		if err := s.Store.Delete(ctx, key, meta.Version); err != nil && !store.IsNotFound(err) && !store.IsPreconditionFailed(err) {
			return mapStoreErr(err)
		}
	}
	return nil
}

// --- blobs --------------------------------------------------------------------

// BlobMeta is a blob HEAD/read header set.
type BlobMeta struct {
	Digest string
	Size   int64
}

// HeadBlob reports whether the blob exists.
func (s *Service) HeadBlob(ctx context.Context, owner, repo, digest string) (*BlobMeta, *Error) {
	if err := validateRepo(owner, repo); err != nil {
		return nil, err
	}
	if !IsDigest(digest) {
		return nil, errDigestInvalid("invalid blob digest")
	}
	meta, err := s.Store.Head(ctx, blobKey(owner, repo, DigestHex(digest)))
	if err != nil {
		return nil, mapStoreErr(err)
	}
	if meta == nil {
		return nil, errBlobUnknown(digest)
	}
	return &BlobMeta{Digest: digest, Size: meta.Size}, nil
}

// GetBlob opens a blob (optionally ranged). The caller closes the body.
func (s *Service) GetBlob(ctx context.Context, owner, repo, digest string, rng *[2]int64) (*store.Object, *Error) {
	if err := validateRepo(owner, repo); err != nil {
		return nil, err
	}
	if !IsDigest(digest) {
		return nil, errDigestInvalid("invalid blob digest")
	}
	opts := store.GetOptions{}
	if rng != nil {
		opts.Range = rng
	}
	res, err := s.Store.Get(ctx, blobKey(owner, repo, DigestHex(digest)), opts)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, errBlobUnknown(digest)
		}
		if rng != nil && store.IsPreconditionFailed(err) {
			// A range starting past EOF: the store reports the 416 analog.
			return nil, ociErr(http.StatusRequestedRangeNotSatisfiable, CodeSizeInvalid, "requested range not satisfiable")
		}
		return nil, mapStoreErr(err)
	}
	obj, ok := res.(store.Object)
	if !ok {
		return nil, errInternal(errors.New("unexpected store result"))
	}
	return &obj, nil
}

// --- uploads (§5) -------------------------------------------------------------

// uploadID embeds its creation time and an HMAC so TTL is enforceable and
// uploads cannot be forged by guessing a random name — no metadata object,
// so a POST costs zero writes (the 2N+1 push budget).
func (s *Service) newUploadID() string {
	ts := strconv.FormatInt(s.now().Unix(), 16)
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		nonce = []byte(fmt.Sprintf("%x", s.now().UnixNano()))
	}
	body := ts + "-" + hex.EncodeToString(nonce)
	return body + "-" + s.signUpload(body)
}

func (s *Service) signUpload(body string) string {
	mac := hmac.New(sha256.New, s.key())
	mac.Write([]byte("upload:" + body))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

// parseUploadID validates the signature and TTL, returning the staging key.
func (s *Service) parseUploadID(owner, repo, id string) (string, *Error) {
	parts := strings.Split(id, "-")
	if len(parts) != 3 {
		return "", errUploadUnknown(id)
	}
	body := parts[0] + "-" + parts[1]
	if !hmac.Equal([]byte(parts[2]), []byte(s.signUpload(body))) {
		return "", errUploadUnknown(id)
	}
	ts, err := strconv.ParseInt(parts[0], 16, 64)
	if err != nil {
		return "", errUploadUnknown(id)
	}
	created := time.Unix(ts, 0)
	if s.now().Sub(created) > s.uploadTTL() {
		// Expired: leave the bytes for the maintainer sweep (§6), but the
		// upload is gone as far as clients are concerned.
		return "", errUploadUnknown(id)
	}
	return uploadKey(owner, repo, id), nil
}

// StartUpload allocates an upload id. No store write (POST is free).
func (s *Service) StartUpload(ctx context.Context, owner, repo string) (string, *Error) {
	if err := validateRepo(owner, repo); err != nil {
		return "", err
	}
	return s.newUploadID(), nil
}

// UploadOffset is the resumed offset (the staged object's size).
func (s *Service) UploadOffset(ctx context.Context, owner, repo, id string) (int64, *Error) {
	if err := validateRepo(owner, repo); err != nil {
		return 0, err
	}
	key, oerr := s.parseUploadID(owner, repo, id)
	if oerr != nil {
		return 0, oerr
	}
	meta, err := s.Store.Head(ctx, key)
	if err != nil {
		return 0, mapStoreErr(err)
	}
	if meta == nil {
		if gone, gerr := s.aborted(ctx, owner, repo, id); gerr != nil {
			return 0, mapStoreErr(gerr)
		} else if gone {
			return 0, errUploadUnknown(id)
		}
		return 0, nil
	}
	return meta.Size, nil
}

// aborted reports whether the upload was explicitly aborted.
func (s *Service) aborted(ctx context.Context, owner, repo, id string) (bool, error) {
	m, err := s.Store.Head(ctx, abortedKey(owner, repo, id))
	if err != nil {
		return false, err
	}
	return m != nil, nil
}

// AppendUpload appends one chunk to the staging object. The body is spooled
// locally (known-length Put; no unbounded memory), then either written as the
// first chunk or composed after the existing bytes. No lock is held across a
// store call: parallel layer uploads are independent keys.
func (s *Service) AppendUpload(ctx context.Context, owner, repo, id string, body io.Reader) (int64, *Error) {
	if err := validateRepo(owner, repo); err != nil {
		return 0, err
	}
	key, oerr := s.parseUploadID(owner, repo, id)
	if oerr != nil {
		return 0, oerr
	}
	meta, err := s.Store.Head(ctx, key)
	if err != nil {
		return 0, mapStoreErr(err)
	}
	var cur int64
	if meta != nil {
		cur = meta.Size
	} else {
		gone, gerr := s.aborted(ctx, owner, repo, id)
		if gerr != nil {
			return 0, mapStoreErr(gerr)
		}
		if gone {
			return 0, errUploadUnknown(id)
		}
	}
	tmp, size, serr := s.spool(body, s.maxBlob()-cur)
	if serr != nil {
		return 0, serr
	}
	defer os.Remove(tmp)
	if cur == 0 {
		if _, err := s.Store.Put(ctx, key, store.PutBody{File: tmp}, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
			return 0, mapStoreErr(err)
		}
		return size, nil
	}
	part := key + ".part-" + strconv.FormatInt(s.now().UnixNano(), 36)
	if _, err := s.Store.Put(ctx, part, store.PutBody{File: tmp}, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		return 0, mapStoreErr(err)
	}
	defer func() { _ = s.Store.Delete(context.WithoutCancel(ctx), part, "") }()
	if _, err := s.Store.Compose(ctx, key, []string{key, part}, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		return 0, mapStoreErr(err)
	}
	return cur + size, nil
}

// FinalizeUpload verifies the digest while streaming the staged bytes into a
// local spool, then writes the content-addressed blob and drops the staging
// object. A mismatch never stores the blob (§5: mismatch → 400, never
// stored). Returns the blob size.
func (s *Service) FinalizeUpload(ctx context.Context, owner, repo, id, digest string) (int64, *Error) {
	if err := validateRepo(owner, repo); err != nil {
		return 0, err
	}
	key, oerr := s.parseUploadID(owner, repo, id)
	if oerr != nil {
		return 0, oerr
	}
	if !IsDigest(digest) {
		return 0, errDigestInvalid("invalid digest")
	}
	res, err := s.Store.Get(ctx, key, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return 0, errUploadUnknown(id)
		}
		return 0, mapStoreErr(err)
	}
	obj, ok := res.(store.Object)
	if !ok {
		return 0, errInternal(errors.New("unexpected store result"))
	}
	defer obj.Body.Close()
	tmp, size, hashed, serr := s.spoolHashed(obj.Body, s.maxBlob())
	if serr != nil {
		return 0, serr
	}
	defer os.Remove(tmp)
	got := "sha256:" + hex.EncodeToString(hashed)
	if got != digest {
		return 0, errDigestInvalid("provided digest does not match the uploaded bytes")
	}
	// Quota (docs/features/14 §5): the same bound the manifest commit point
	// enforces, applied here so a push cannot stream an unbounded team of
	// layers past it and only fail at the end. Layer bytes are counted as they
	// land, so the counter is the repo's stored footprint. A refusal writes no
	// blob (the staging object stays for the TTL sweep's cleanup).
	usage, uv, uerr := s.readUsage(ctx, owner, repo)
	if uerr != nil {
		return 0, errInternal(uerr)
	}
	pol := s.policyFor(ctx, owner, repo)
	if pol.QuotaBytes > 0 && usage.Bytes+size > pol.QuotaBytes {
		return 0, errQuotaExceeded(usage.Bytes, pol.QuotaBytes, size)
	}
	if _, err := s.Store.Put(ctx, blobKey(owner, repo, DigestHex(digest)), store.PutBody{File: tmp}, store.PutOptions{
		Mode: store.PutOverwrite, Immutable: true,
	}); err != nil {
		return 0, mapStoreErr(err)
	}
	s.adjustUsageFrom(ctx, owner, repo, usage, uv, Usage{Bytes: size, BlobBytes: size, Blobs: 1})
	// Grace guard (§6): the manifest PUT follows this finalize, so a sweep
	// landing in between must not treat the blob as garbage yet.
	s.noteBlob(owner, repo, DigestHex(digest))
	if err := s.Store.Delete(ctx, key, ""); err != nil {
		return 0, mapStoreErr(err)
	}
	return size, nil
}

// AbortUpload drops the staging object and leaves a tombstone so the same id
// can never resume (idempotent; a repeated abort rewrites the marker).
func (s *Service) AbortUpload(ctx context.Context, owner, repo, id string) *Error {
	if err := validateRepo(owner, repo); err != nil {
		return err
	}
	key, oerr := s.parseUploadID(owner, repo, id)
	if oerr != nil {
		return oerr
	}
	if _, err := s.Store.Put(ctx, abortedKey(owner, repo, id), store.PutBody{Bytes: []byte{}}, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		return mapStoreErr(err)
	}
	if err := s.Store.Delete(ctx, key, ""); err != nil {
		return mapStoreErr(err)
	}
	return nil
}

// spool copies an at-most-cap body to a local temp file, returning its path
// and size. Over-cap bodies are rejected as SIZE_INVALID without buffering.
func (s *Service) spool(body io.Reader, cap int64) (string, int64, *Error) {
	f, err := s.tempFile()
	if err != nil {
		return "", 0, errInternal(err)
	}
	path := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(path)
		}
	}()
	n, cerr := io.Copy(f, io.LimitReader(body, cap+1))
	if cerr != nil {
		return "", 0, errInternal(cerr)
	}
	if n > cap {
		return "", 0, errSizeInvalid("blob exceeds packages.max_blob_bytes")
	}
	if err := f.Close(); err != nil {
		return "", 0, errInternal(err)
	}
	ok = true
	return path, n, nil
}

// spoolHashed is spool with a running sha256 (the finalize verification).
func (s *Service) spoolHashed(body io.Reader, cap int64) (string, int64, []byte, *Error) {
	f, err := s.tempFile()
	if err != nil {
		return "", 0, nil, errInternal(err)
	}
	path := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(path)
		}
	}()
	h := sha256.New()
	n, cerr := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, cap+1))
	if cerr != nil {
		return "", 0, nil, errInternal(cerr)
	}
	if n > cap {
		return "", 0, nil, errSizeInvalid("blob exceeds packages.max_blob_bytes")
	}
	if err := f.Close(); err != nil {
		return "", 0, nil, errInternal(err)
	}
	ok = true
	return path, n, h.Sum(nil), nil
}

func (s *Service) tempFile() (*os.File, error) {
	if s.SpoolDir != "" {
		if err := os.MkdirAll(s.SpoolDir, 0o755); err == nil {
			return os.CreateTemp(s.SpoolDir, "pkg-upload-*")
		}
	}
	return os.CreateTemp(filepath.Clean(os.TempDir()), "pkg-upload-*")
}

// --- tags list (§3; the one user-triggered LIST) -------------------------------

// ListTags pages tag names strictly after `last`, at most n, reporting
// whether another page exists. Exactly one LIST call.
func (s *Service) ListTags(ctx context.Context, owner, repo, last string, n int) ([]string, bool, *Error) {
	if err := validateRepo(owner, repo); err != nil {
		return nil, false, err
	}
	if n <= 0 {
		n = 100
	}
	startAfter := ""
	if last != "" {
		startAfter = tagsPrefix(owner, repo) + last
	}
	tags := make([]string, 0, n+1)
	err := s.Store.List(ctx, tagsPrefix(owner, repo), startAfter, func(m store.ObjectMeta) error {
		name := strings.TrimPrefix(m.Key, tagsPrefix(owner, repo))
		if name == "" {
			return nil
		}
		tags = append(tags, name)
		if len(tags) > n {
			return errStopList
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopList) {
		return nil, false, mapStoreErr(err)
	}
	more := len(tags) > n
	if more {
		tags = tags[:n]
	}
	return tags, more, nil
}

var errStopList = errors.New("stop list")
