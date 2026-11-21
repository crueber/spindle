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
	DefaultMaxBlobBytes     int64 = 2 << 30
	DefaultMaxManifestBytes int64 = 4 << 20
	DefaultUploadTTL              = 24 * time.Hour
	DefaultTokenTTL               = 5 * time.Minute
)

// RegistryPrefix is the bucket prefix this feature owns (§2). It is
// spindle-only and never collides with a Rust-era key.
const registrySuffix = "/packages/registry"

// RoleService is the narrow P6 surface this package consumes (the same shape
// internal/releases and internal/checks use: satisfied by *identity.Service,
// tests substitute a fake). Core never imports this package.
type RoleService interface {
	Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc)
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
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
	// Exists reports whether the repository exists (nil → assume it does:
	// unit tests without a registry). A missing repo answers NAME_UNKNOWN.
	Exists func(ctx context.Context, owner, repo string) (bool, error)

	MaxBlobBytes     int64
	MaxManifestBytes int64
	UploadTTL        time.Duration
	TokenTTL         time.Duration
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

	Now func() time.Time

	secretOnce sync.Once
	secret     []byte
	// TokenSecret is the HMAC key for registry bearer tokens. Empty → a
	// per-process random key (tokens survive only this process; Docker
	// re-mints on every operation, so the practical effect is re-login
	// after a restart — documented in 14_packages.md §4).
	TokenSecret []byte
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
// and a flag-only fallback keeps instances without identity working.
func (s *Service) CanPull(ctx context.Context, owner, repo string, p auth.Principal) *Error {
	if p.Admin || p.Write {
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

// CanPush enforces write on the repository (scope push ⇒ role write).
func (s *Service) CanPush(ctx context.Context, owner, repo string, p auth.Principal) *Error {
	if p.Admin {
		return nil
	}
	if p.Anonymous {
		return errUnauthorized()
	}
	if s.Roles == nil {
		if !p.Write {
			return errDenied("write access required")
		}
		return nil
	}
	role, _ := s.Roles.Resolve(ctx, owner, repo, p)
	if roleRank(string(role)) >= roleRank(string(identity.RoleWrite)) {
		return nil
	}
	return errDenied("write access required")
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
	// The manifest object is idempotent content-addressed bytes (S3 keeps
	// the same version for identical bytes — never assume a counter).
	if _, err := s.Store.Put(ctx, manifestKey(owner, repo, hexDigest), store.PutBody{Bytes: body}, store.PutOptions{
		Mode: store.PutOverwrite, ContentType: mt, Immutable: true,
	}); err != nil {
		return "", mapStoreErr(err)
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

// DeleteManifest removes a tag pointer (tag ref) or a manifest object
// (digest ref). Blobs stay until the maintainer sweep (§6). Deletion is
// idempotent: an absent object is still 202.
func (s *Service) DeleteManifest(ctx context.Context, owner, repo, ref string) *Error {
	if err := validateRepo(owner, repo); err != nil {
		return err
	}
	switch {
	case IsTag(ref):
		if err := s.Store.Delete(ctx, tagKey(owner, repo, ref), ""); err != nil {
			return mapStoreErr(err)
		}
		return nil
	case IsDigest(ref):
		if err := s.Store.Delete(ctx, manifestKey(owner, repo, DigestHex(ref)), ""); err != nil {
			return mapStoreErr(err)
		}
		return nil
	default:
		return errManifestUnknown(ref)
	}
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
	if _, err := s.Store.Put(ctx, blobKey(owner, repo, DigestHex(digest)), store.PutBody{File: tmp}, store.PutOptions{
		Mode: store.PutOverwrite, Immutable: true,
	}); err != nil {
		return 0, mapStoreErr(err)
	}
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
