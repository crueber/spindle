package tags

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	walgit "github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/policy"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// This file owns mutation policy: the P6 write gate, tag-name validation,
// the annotated path, the policy.json create check, sha resolution, and the
// CAS create publish. Wire mapping lives in http.go.

// RoleService is the narrow P6 surface this package consumes (same shape as
// internal/releases and internal/pulls: satisfied by *identity.Service;
// tests substitute a fake).
type RoleService interface {
	// Resolve returns the max repo role for p (P6 verbatim).
	Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc)
	// CheckRead is the require_read gate (unused here; create is write-only).
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// GitRunner verifies a sha names a commit and mints annotated tag objects
// (exact argv in git.go; never a Go git library). Tests substitute a fake.
type GitRunner interface {
	// CommitExists resolves sha to its commit id (peeled `^{commit}`).
	// Unresolvable ⇒ ErrNotFound-class error (→ 404 unknown revision).
	CommitExists(ctx context.Context, dir, sha string) (string, error)
	// CreateTagObject mints the annotated tag object via `git mktag`
	// (strict fsck; rejection ⇒ ErrInvalid-class → 400, nothing stored).
	CreateTagObject(ctx context.Context, dir string, body []byte) (string, error)
	// PackObject packs one object via `git pack-objects --stdout`
	// (failure ⇒ ErrUnavailable-class → 5xx, nothing published).
	PackObject(ctx context.Context, dir, oid string) ([]byte, error)
	// ListTagRefs lists tag rows newest-creator-date-first (name, tag
	// object, peeled commit, creator date, creator name):
	// `git for-each-ref --sort=-creatordate --format=… refs/tags`
	// (04_git.md §11.4).
	ListTagRefs(ctx context.Context, dir string) ([]TagRef, error)
}

// TagRef is one for-each-ref row (the tags page's data source).
type TagRef struct {
	Name      string
	Object    string // tag object oid for annotated tags, commit oid otherwise
	Commit    string // peeled commit oid
	CreatedAt string // %(creatordate:iso-strict)
	Author    string // %(authorname): tagger for annotated, committer otherwise
	Annotated bool
}

// RepoDirs resolves a repo to its synced local git dir (production syncs
// the WAL handle to serve level and returns the bare repo path).
type RepoDirs interface {
	Dir(ctx context.Context, repo string) (string, error)
}

// RefPublisher publishes tag creates through the normal WAL publish path
// (doc 05 CAS ladder). The production adapter funnels through
// RepoHandle.Publish: lightweight tags as a single-update REF_UPDATE txn,
// annotated tags (#263) as a PUSH entry (single-object pack + the same txn
// shape with NewPeeled set). Old-oid zero = create in both shapes; a present
// tag fails in verify and surfaces as an ErrConflict-class error (→ 409),
// never a force.
type RefPublisher interface {
	// CreateTag creates refs/tags/<name> → sha (CAS create).
	CreateTag(ctx context.Context, repo, name, sha string, meta map[string]string) error
	// CreateAnnotatedTag publishes the pre-packed tag object: refs/tags/<name>
	// → tagOid with NewPeeled = the peeled commit sha (CAS create). The pack
	// carries the tag object to every replica (a REF_UPDATE-only publish
	// would advertise a ref whose object replicas lack).
	CreateAnnotatedTag(ctx context.Context, repo, name, tagOid, peeled string, pack []byte, meta map[string]string) error
	// DeleteTag publishes refs/tags/<name> old → zero (CAS delete: a moved
	// tag fails the old-oid check, never a blind delete of someone else's
	// move).
	DeleteTag(ctx context.Context, repo, name, oldOid string, meta map[string]string) error
}

// Service is the tags store client: validation, policy, and the publish
// funnel. Construct with New; Git/Dirs/Refs may be nil in tests that
// exercise pure-validation paths (nil Git/Dirs makes sha-touching ops 503;
// nil Refs makes publishing 503; nil Roles falls back to principal flags).
// Now renders server wall-clock for annotated tagger lines (tests pin it;
// nil means time.Now).
type Service struct {
	Store store.ObjectStore
	Roles RoleService
	Git   GitRunner
	Dirs  RepoDirs
	Refs  RefPublisher
	Now   func() time.Time
	// ReleaseExists gates DeleteTag on the release-backed refusal (WP4.2;
	// nil skips the check — composition always wires it).
	ReleaseExists ReleaseExists
}

// now renders the clock (pinneable in tests).
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// New builds a Service over st.
func New(st store.ObjectStore, roles RoleService) *Service {
	return &Service{Store: st, Roles: roles}
}

// Tag is the create response: Name/Ref name the tag; SHA is the ref target —
// the commit for lightweight tags, the tag OBJECT oid for annotated ones.
type Tag struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
	Ref  string `json:"ref"`
}

// CreateInput is the POST body: name + sha required; a non-empty message
// requests an annotated tag (#263: mktag + single-object pack + PUSH
// publish with NewPeeled). Empty/whitespace message is the lightweight path.
type CreateInput struct {
	Name    string `json:"name"`
	SHA     string `json:"sha"`
	Message string `json:"message"`
}

// PolicyKey renders repos/<o>/<r>/policy.json (owned by internal/policy;
// this package reads it for the create check only).
func PolicyKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/policy.json"
}

// CreateTag validates and publishes refs/tags/<name>. An empty/whitespace
// message takes the lightweight path (one WAL REF_UPDATE create at the
// commit); a non-empty message takes the annotated path (#263: tag object
// via mktag, single-object pack, PUSH publish with NewPeeled). A non-empty
// message is never silently downgraded to lightweight. Error classes: 401
// anonymous, 403 non-writer or policy-denied, 400 bad name/sha/message or
// mktag rejection, 404 unknown sha, 409 tag exists, 503/5xx git/publish
// unwired or pack/ingest failure.
func (s *Service) CreateTag(ctx context.Context, owner, repo string, p auth.Principal, in CreateInput) (*Tag, error) {
	if err := s.requireRole(ctx, owner, repo, p, string(identity.RoleWrite)); err != nil {
		return nil, err
	}
	name, err := validateTagName(in.Name)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Message) != "" {
		return s.createAnnotated(ctx, owner, repo, p, name, in.SHA, in.Message)
	}
	sha := strings.TrimSpace(in.SHA)
	if sha == "" {
		return nil, fmt.Errorf("%w: sha must not be empty", ErrInvalid)
	}
	resolved, err := s.resolveCommit(ctx, owner, repo, sha)
	if err != nil {
		return nil, err
	}
	if err := s.checkPolicy(ctx, owner, repo, p, "refs/tags/"+name); err != nil {
		return nil, err
	}
	meta := map[string]string{"principal": p.Name}
	if s.Refs == nil {
		return nil, fmt.Errorf("%w: tag publish unavailable", ErrUnavailable)
	}
	if perr := s.Refs.CreateTag(ctx, repoName(owner, repo), name, resolved, meta); perr != nil {
		if isErr(perr, errConflict) {
			return nil, fmt.Errorf("%w: tag %q already exists", ErrConflict, name)
		}
		return nil, perr
	}
	return &Tag{Name: name, SHA: resolved, Ref: "refs/tags/" + name}, nil
}

// createAnnotated publishes an annotated tag (Forgejo #263, ruling (b)):
// the tag object is constructed server-side with `git mktag` (strict fsck),
// packed as a single object (`git pack-objects --stdout`), ingested through
// the existing Layer.Ingest path, and published as a PUSH entry whose txn
// creates refs/tags/<name> at the tag oid with NewPeeled = the commit sha.
// No new WAL kind, no proto change. Failure semantics: message/mktag
// problems → 400 with nothing published; pack/publish problems → 5xx with
// nothing published; an existing tag → 409 via the verify step.
func (s *Service) createAnnotated(ctx context.Context, owner, repo string, p auth.Principal, name, sha, message string) (*Tag, error) {
	msg, err := normalizeTagMessage(message)
	if err != nil {
		return nil, err
	}
	target := strings.TrimSpace(sha)
	if target == "" {
		return nil, fmt.Errorf("%w: sha must not be empty", ErrInvalid)
	}
	resolved, err := s.resolveCommit(ctx, owner, repo, target)
	if err != nil {
		return nil, err
	}
	if err := s.checkPolicy(ctx, owner, repo, p, "refs/tags/"+name); err != nil {
		return nil, err
	}
	tagger, err := renderTagger(p, s.now())
	if err != nil {
		return nil, err
	}
	body := renderTagBody(name, resolved, tagger, msg)
	// Git/Dirs are non-nil here: resolveCommit above already refused the
	// unwired case (503), and nothing mutates the service mid-request.
	dir, err := s.Dirs.Dir(ctx, repoName(owner, repo))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	tagOid, err := s.Git.CreateTagObject(ctx, dir, body)
	if err != nil {
		return nil, err
	}
	pack, err := s.Git.PackObject(ctx, dir, tagOid)
	if err != nil {
		return nil, err
	}
	if s.Refs == nil {
		return nil, fmt.Errorf("%w: tag publish unavailable", ErrUnavailable)
	}
	meta := map[string]string{"principal": p.Name}
	if perr := s.Refs.CreateAnnotatedTag(ctx, repoName(owner, repo), name, tagOid, resolved, pack, meta); perr != nil {
		if isErr(perr, errConflict) {
			return nil, fmt.Errorf("%w: tag %q already exists", ErrConflict, name)
		}
		return nil, perr
	}
	return &Tag{Name: name, SHA: tagOid, Ref: "refs/tags/" + name}, nil
}

// normalizeTagMessage validates an annotated-tag message: non-UTF8 and NUL
// bytes are rejected (git fsck would refuse them later), the message is
// capped at MaxTagMessageLen, and the body is normalized to a single
// trailing newline.
func normalizeTagMessage(message string) (string, error) {
	if len(message) > MaxTagMessageLen {
		return "", fmt.Errorf("%w: message exceeds %d bytes", ErrInvalid, MaxTagMessageLen)
	}
	if strings.IndexByte(message, 0) >= 0 {
		return "", fmt.Errorf("%w: message must not contain NUL bytes", ErrInvalid)
	}
	if !utf8.ValidString(message) {
		return "", fmt.Errorf("%w: message must be UTF-8", ErrInvalid)
	}
	return strings.TrimRight(message, "\n") + "\n", nil
}

// renderTagger synthesizes the tagger line from the authed principal (the
// Principal carries only Name, no email): `<sanitized-name>
// <<sanitized-name>@spindle.local>` with server wall-clock time + local tz
// offset. The fixed domain marks the tag server-minted rather than claiming
// a verified external email. Sanitization strips `<`, `>`, `\n`, `\r`;
// empty-after-sanitize is a 400.
func renderTagger(p auth.Principal, now time.Time) (string, error) {
	name := strings.ReplaceAll(p.Name, "<", "")
	name = strings.ReplaceAll(name, ">", "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.ReplaceAll(name, "\r", "")
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("%w: principal name is empty after sanitization", ErrInvalid)
	}
	_, offset := now.Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	tz := fmt.Sprintf("%s%02d%02d", sign, offset/3600, (offset%3600)/60)
	return fmt.Sprintf("%s <%s@spindle.local> %d %s", name, name, now.Unix(), tz), nil
}

// renderTagBody renders the canonical mktag stdin bytes (LF, trailing
// newline): object/type/tag/tagger headers, a blank line, then the message.
func renderTagBody(name, commitSHA, tagger, message string) []byte {
	var b strings.Builder
	b.WriteString("object " + commitSHA + "\n")
	b.WriteString("type commit\n")
	b.WriteString("tag " + name + "\n")
	b.WriteString("tagger " + tagger + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

// validateTagName enforces the git refname rules pushes enforce, applied to
// the full refs/tags/<name> form: no ~^:?*[ no whitespace/control bytes,
// no "..", no "@{", no leading/trailing slashes. Slashes inside the name
// are legal (refs/tags/a/b); the rule names itself in the error.
func validateTagName(name string) (string, error) {
	t := strings.TrimSpace(name)
	if t == "" {
		return "", fmt.Errorf("%w: tag must not be empty", ErrInvalid)
	}
	if len(t) > MaxTagLen {
		return "", fmt.Errorf("%w: tag exceeds %d bytes", ErrInvalid, MaxTagLen)
	}
	if err := walgit.ValidateRefName("refs/tags/" + t); err != nil {
		return "", fmt.Errorf("%w: invalid ref name %q: %v", ErrInvalid, t, err)
	}
	return t, nil
}

// resolveCommit verifies sha names a commit in the repo. Unwired git ⇒ 503;
// unresolvable ⇒ 404 unknown revision.
func (s *Service) resolveCommit(ctx context.Context, owner, repo, sha string) (string, error) {
	if s.Git == nil || s.Dirs == nil {
		return "", fmt.Errorf("%w: commit resolution unavailable", ErrUnavailable)
	}
	dir, err := s.Dirs.Dir(ctx, repoName(owner, repo))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	resolved, err := s.Git.CommitExists(ctx, dir, sha)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// checkPolicy evaluates policy.json for (principal, ref, create) because the
// tag publish is server-side, NOT a receive-pack push (the merge-task
// precedent): protect rules deny the create exactly as they would deny a
// push (`rejected by rule '<name>'`). Missing policy.json is allow-all; an
// unparseable file fails closed (refused, never "skip policy").
func (s *Service) checkPolicy(ctx context.Context, owner, repo string, p auth.Principal, ref string) error {
	raw, _, err := store.GetBytes(ctx, s.Store, PolicyKey(owner, repo), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			raw = nil
		} else {
			return err
		}
	}
	var doc *policy.Document
	if raw == nil {
		doc, err = policy.Parse([]byte(`{"version":1,"rules":[]}`))
	} else {
		doc, err = policy.Parse(raw)
	}
	if err != nil {
		return fmt.Errorf("%w: policy.json unparseable: %v", ErrCorrupt, err)
	}
	v := policy.EvaluateProtect(ctx, doc, policy.Request{Principal: p.Name, Ref: ref, Op: policy.OpCreate})
	if !v.Allow {
		return fmt.Errorf("%w: rejected by rule '%s'", ErrForbidden, v.Rule)
	}
	return nil
}

// repoName renders "owner/repo".
func repoName(owner, repo string) string { return owner + "/" + repo }

// --- role helpers (P6; same shape as internal/releases) ----------------------

// roleRank orders role names on the P6 ladder read < triage < write <
// maintain < admin.
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

// roleOf resolves the actor's repo role ("" when none). Host admin/write
// flags short-circuit through identity's own resolution.
func (s *Service) roleOf(ctx context.Context, owner, repo string, p auth.Principal) string {
	if s.Roles == nil {
		if p.Admin {
			return string(identity.RoleAdmin)
		}
		if p.Write {
			return string(identity.RoleWrite)
		}
		if p.Anonymous {
			return ""
		}
		return string(identity.RoleRead)
	}
	role, _ := s.Roles.Resolve(ctx, owner, repo, p)
	return string(role)
}

// requireRole enforces a minimum repo role: host admin always passes;
// anonymous failures are 401, authenticated-but-insufficient are 403.
func (s *Service) requireRole(ctx context.Context, owner, repo string, p auth.Principal, want string) error {
	if p.Admin {
		return nil
	}
	got := s.roleOf(ctx, owner, repo, p)
	if roleRank(got) >= roleRank(want) {
		return nil
	}
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	return fmt.Errorf("%w: need %s", ErrForbidden, want)
}

// --- tags page + delete (PLAN-V1 WP4) ---------------------------------------

// TagEntry is one /tags page row: the tag, its resolved commit, the creator
// stamp git records, and whether a release backs it (a release-backed tag is
// refused by DeleteTag — deleting it would strand the release's snapshot).
type TagEntry struct {
	Name      string `json:"name"`
	Commit    string `json:"commit"`
	Object    string `json:"object,omitempty"`
	Date      string `json:"date"`
	Author    string `json:"author"`
	Annotated bool   `json:"annotated"`
	Release   bool   `json:"release"`
}

// ListTagEntries lists refs/tags for the tags page (read gate). A repo with no
// tags is an empty list, not an error. Git unwired is 503.
func (s *Service) ListTagEntries(ctx context.Context, owner, repo string, p auth.Principal) ([]TagEntry, error) {
	if err := s.requireRole(ctx, owner, repo, p, string(identity.RoleRead)); err != nil {
		return nil, err
	}
	if s.Git == nil || s.Dirs == nil {
		return nil, fmt.Errorf("%w: tag listing unavailable", ErrUnavailable)
	}
	dir, err := s.Dirs.Dir(ctx, repoName(owner, repo))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	refs, err := s.Git.ListTagRefs(ctx, dir)
	if err != nil {
		return nil, err
	}
	out := make([]TagEntry, 0, len(refs))
	for _, r := range refs {
		entry := TagEntry{Name: r.Name, Commit: r.Commit, Object: r.Object,
			Date: r.CreatedAt, Author: r.Author, Annotated: r.Annotated}
		if s.ReleaseExists != nil {
			if ok, rerr := s.ReleaseExists(ctx, owner, repo, r.Name); rerr == nil {
				entry.Release = ok
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// ReleaseExists reports whether owner/repo has a release header for tag. It is
// a seam (composition binds internal/releases GetRelease) so this package
// never imports the releases feature; nil skips the release-backed refusal
// (tests that only exercise policy/CAS).
type ReleaseExists func(ctx context.Context, owner, repo, tag string) (bool, error)

// SetReleaseExists installs the release-backed check.
func (s *Service) SetReleaseExists(fn ReleaseExists) { s.ReleaseExists = fn }

// DeleteTag deletes refs/tags/<name> (write role + the protect policy effect
// for op=delete). A tag that backs a release is refused with a clear 409: the
// release snapshots tag_sha and keeps serving, so the tag is the release's
// public address — re-pointing users at nothing is not a delete we perform.
func (s *Service) DeleteTag(ctx context.Context, owner, repo string, p auth.Principal, name string) error {
	if err := s.requireRole(ctx, owner, repo, p, string(identity.RoleWrite)); err != nil {
		return err
	}
	name, err := validateTagName(name)
	if err != nil {
		return err
	}
	if s.ReleaseExists != nil {
		ok, rerr := s.ReleaseExists(ctx, owner, repo, name)
		if rerr != nil {
			return fmt.Errorf("%w: release check: %v", ErrUnavailable, rerr)
		}
		if ok {
			return fmt.Errorf("%w: tag %q backs a release; delete the release first", ErrConflict, name)
		}
	}
	if err := s.checkPolicyOp(ctx, owner, repo, p, "refs/tags/"+name, policy.OpDelete); err != nil {
		return err
	}
	sha, err := s.resolveCommit(ctx, owner, repo, "refs/tags/"+name)
	if err != nil {
		return err
	}
	if s.Refs == nil {
		return fmt.Errorf("%w: tag publish unavailable", ErrUnavailable)
	}
	if perr := s.Refs.DeleteTag(ctx, repoName(owner, repo), name, sha, map[string]string{"principal": p.Name}); perr != nil {
		if isErr(perr, errConflict) {
			return fmt.Errorf("%w: tag %q moved concurrently; reload and retry", ErrConflict, name)
		}
		return perr
	}
	return nil
}

// checkPolicyOp evaluates policy.json for (principal, ref, op). Missing
// policy.json is allow-all; unparseable fails closed.
func (s *Service) checkPolicyOp(ctx context.Context, owner, repo string, p auth.Principal, ref, op string) error {
	raw, _, err := store.GetBytes(ctx, s.Store, PolicyKey(owner, repo), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			raw = nil
		} else {
			return err
		}
	}
	var doc *policy.Document
	if raw == nil {
		doc, err = policy.Parse([]byte(`{"version":1,"rules":[]}`))
	} else {
		doc, err = policy.Parse(raw)
	}
	if err != nil {
		return fmt.Errorf("%w: policy.json unparseable: %v", ErrCorrupt, err)
	}
	v := policy.EvaluateProtect(ctx, doc, policy.Request{Principal: p.Name, Ref: ref, Op: op})
	if !v.Allow {
		return fmt.Errorf("%w: rejected by rule '%s'", ErrForbidden, v.Rule)
	}
	return nil
}
