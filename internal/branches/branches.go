// Package branches implements the write half of the branches page: create a
// branch from a ref and delete a branch (docs/go/12_web_ui.md; argv pinned in
// docs/go/04_git.md §16.2).
//
// It is a thin policy/gate layer over two injected seams — the WAL ref
// publisher (composition owns it, exactly like tags/webedit) and a git
// resolver for the starting commit — so this package never writes a ref
// directly (law 4) and never reimplements the push gate or the policy
// evaluation. Deletion consults THE stored policy document (the same
// api.PolicyRaw object the Settings → Branch protection tab renders) through
// policy.EvaluateProtect, the server-side publisher's own evaluation law; there
// is no second protection rule here.
package branches

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/policy"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Sentinel families the page maps onto status codes (the tags/webedit shape).
var (
	ErrInvalid      = errors.New("invalid branch request")
	ErrNotFound     = errors.New("branch not found")
	ErrConflict     = errors.New("branch changed concurrently")
	ErrUnauthorized = errors.New("authentication required")
	ErrForbidden    = errors.New("forbidden")
	ErrUnavailable  = errors.New("branches unavailable")
)

// Gate is the repo-scoped push rule shared with receive-pack (identity.Service
// implements it): CheckRead is the visibility gate, CheckPush is the ref-write
// floor — the same gate webedit uses, never a page-local role check.
type Gate interface {
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
	CheckPush(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// RepoDirs resolves a repo to its synced bare git dir.
type RepoDirs interface {
	Dir(ctx context.Context, repo string) (string, error)
}

// GitRunner resolves a revision to a commit sha.
type GitRunner interface {
	ResolveCommit(ctx context.Context, dir, rev string) (string, error)
}

// RefPublisher lands a branch ref through the WAL publish funnel. Implementations
// use the CAS txn shape (old=zero for a create, old=<sha> for a delete) and
// return ErrConflict when the manifest CAS loses.
type RefPublisher interface {
	CreateBranch(ctx context.Context, repo, name, sha string, meta map[string]string) error
	DeleteBranch(ctx context.Context, repo, name, old string, meta map[string]string) error
}

// Service is the page's write surface. DefaultBranch reports the repo's
// default branch SHORT name ("" for an unborn repo).
type Service struct {
	Store         store.ObjectStore
	Gate          Gate
	Dirs          RepoDirs
	Git           GitRunner
	Refs          RefPublisher
	DefaultBranch func(ctx context.Context, owner, repo string) (string, error)
}

// PolicyKey is the existing per-repo policy document (no new key).
func PolicyKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/policy.json"
}

// ValidateBranchName validates a SHORT branch name as refs/heads/<name> grade
// and refuses anything that could read as an option. This is git's refname
// rule (git.ValidateRefName) applied once — the browser editor uses the same
// rule for the same job.
func ValidateBranchName(name string) (string, error) {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "refs/heads/")
	if name == "" || name == "HEAD" {
		return "", fmt.Errorf("%w: branch name must not be empty", ErrInvalid)
	}
	if strings.HasPrefix(name, "-") {
		return "", fmt.Errorf("%w: branch name must not start with '-'", ErrInvalid)
	}
	if err := git.ValidateRefName("refs/heads/" + name); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return name, nil
}

// Create makes refs/heads/<name> at the commit `from` resolves to.
func (s *Service) Create(ctx context.Context, owner, repo string, p auth.Principal, name, from string) error {
	if err := s.gatePush(ctx, owner, repo, p); err != nil {
		return err
	}
	name, err := ValidateBranchName(name)
	if err != nil {
		return err
	}
	if from = strings.TrimSpace(from); from == "" {
		def, derr := s.defaultBranch(ctx, owner, repo)
		if derr != nil {
			return derr
		}
		if def == "" {
			return fmt.Errorf("%w: choose a starting ref", ErrInvalid)
		}
		from = def
	}
	if strings.HasPrefix(from, "-") {
		return fmt.Errorf("%w: starting ref must not start with '-'", ErrInvalid)
	}
	dir, err := s.dir(ctx, owner, repo)
	if err != nil {
		return err
	}
	sha, err := s.Git.ResolveCommit(ctx, dir, from)
	if err != nil {
		return fmt.Errorf("%w: %q — %v", ErrNotFound, from, err)
	}
	if err := s.checkPolicy(ctx, owner, repo, p, "refs/heads/"+name, policy.OpCreate); err != nil {
		return err
	}
	if s.Refs == nil {
		return fmt.Errorf("%w: ref publishing is not wired", ErrUnavailable)
	}
	return s.Refs.CreateBranch(ctx, repoName(owner, repo), name, sha, map[string]string{"principal": p.Name, "agent": "branches"})
}

// Delete removes refs/heads/<name>. The default branch and any ref the stored
// policy protects against deletes are refused before any write.
func (s *Service) Delete(ctx context.Context, owner, repo string, p auth.Principal, name string) error {
	if err := s.gatePush(ctx, owner, repo, p); err != nil {
		return err
	}
	name, err := ValidateBranchName(name)
	if err != nil {
		return err
	}
	def, err := s.defaultBranch(ctx, owner, repo)
	if err != nil {
		return err
	}
	if def != "" && name == def {
		return fmt.Errorf("%w: the default branch %q cannot be deleted", ErrForbidden, def)
	}
	if err := s.checkPolicy(ctx, owner, repo, p, "refs/heads/"+name, policy.OpDelete); err != nil {
		return err
	}
	dir, err := s.dir(ctx, owner, repo)
	if err != nil {
		return err
	}
	sha, err := s.Git.ResolveCommit(ctx, dir, "refs/heads/"+name)
	if err != nil {
		return fmt.Errorf("%w: branch %q", ErrNotFound, name)
	}
	if s.Refs == nil {
		return fmt.Errorf("%w: ref publishing is not wired", ErrUnavailable)
	}
	return s.Refs.DeleteBranch(ctx, repoName(owner, repo), name, sha, map[string]string{"principal": p.Name, "agent": "branches"})
}

// defaultBranch resolves the default branch through the injected seam and
// fails closed when the seam is missing or errors: an unverifiable default
// must never authorize deleting it.
func (s *Service) defaultBranch(ctx context.Context, owner, repo string) (string, error) {
	if s.DefaultBranch == nil {
		return "", fmt.Errorf("%w: default branch is not available", ErrUnavailable)
	}
	def, err := s.DefaultBranch(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return def, nil
}

func (s *Service) dir(ctx context.Context, owner, repo string) (string, error) {
	if s.Dirs == nil {
		return "", fmt.Errorf("%w: repository dirs are not wired", ErrUnavailable)
	}
	dir, err := s.Dirs.Dir(ctx, repoName(owner, repo))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return dir, nil
}

// gatePush mirrors the browser editor's F.5 gate: anonymous is refused before
// any store/git work, then visibility, then the push floor (a host-wide write
// flag alone must not authorize someone else's repository).
func (s *Service) gatePush(ctx context.Context, owner, repo string, p auth.Principal) error {
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	if s.Gate == nil {
		if !p.Write && !p.Admin {
			return fmt.Errorf("%w: write access required", ErrForbidden)
		}
		return nil
	}
	if aerr := s.Gate.CheckRead(ctx, owner, repo, p); aerr != nil {
		return mapAuthErr(aerr)
	}
	if aerr := s.Gate.CheckPush(ctx, owner, repo, p); aerr != nil {
		return mapAuthErr(aerr)
	}
	return nil
}

func mapAuthErr(aerr *auth.AuthError) error {
	if aerr == nil {
		return nil
	}
	switch aerr.Kind {
	case auth.ErrUnauthorized:
		return fmt.Errorf("%w: %s", ErrUnauthorized, aerr.Why)
	case auth.ErrForbidden:
		return fmt.Errorf("%w: %s", ErrForbidden, aerr.Why)
	case auth.ErrInvalid:
		return fmt.Errorf("%w: %s", ErrInvalid, aerr.Why)
	default:
		return fmt.Errorf("%w: %s", ErrUnavailable, aerr.Why)
	}
}

// checkPolicy evaluates the stored policy document — the SAME object the
// Branch protection settings tab reads — with the server-side protection law
// (policy.EvaluateProtect). Missing policy.json is allow-all; an unparseable
// document fails closed.
func (s *Service) checkPolicy(ctx context.Context, owner, repo string, p auth.Principal, ref, op string) error {
	if s.Store == nil {
		return fmt.Errorf("%w: policy store is not wired", ErrUnavailable)
	}
	raw, _, err := store.GetBytes(ctx, s.Store, PolicyKey(owner, repo), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("%w: policy read: %v", ErrUnavailable, err)
	}
	doc, perr := policy.Parse(raw)
	if perr != nil {
		return fmt.Errorf("%w: policy.json unparseable: %v", ErrForbidden, perr)
	}
	v := policy.EvaluateProtect(ctx, doc, policy.Request{Principal: p.Name, Ref: ref, Op: op})
	if !v.Allow {
		return fmt.Errorf("%w: rejected by rule '%s'", ErrForbidden, v.Rule)
	}
	return nil
}

func repoName(owner, repo string) string { return owner + "/" + repo }

// ResolveTimeout bounds one rev-parse spawn.
const ResolveTimeout = 30 * time.Second
