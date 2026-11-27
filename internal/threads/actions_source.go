package threads

// Wiring between the Threads service's git reader and the action resolver
// (internal/actions). The actions package is a leaf: it knows only read-only
// repository calls, so an instance that cannot list a tree (a test fake, or a
// reader without the capability) gets a named refusal instead of a partial
// fetch.

import (
	"context"
	"fmt"

	"github.com/crueber/spindle/internal/actions"
)

// actionTreeLister is the optional GitReader extension remote actions need to
// enumerate an action directory. SubprocessGit implements it (git ls-tree).
type actionTreeLister interface {
	ListTree(ctx context.Context, repo, commit, dir string) ([]actions.TreeEntry, error)
}

// actionSizedReader is the optional GitReader extension that reads a blob with
// an action-sized cap (the workflow reader's 1 MiB cap is too small for a
// bundled JavaScript action).
type actionSizedReader interface {
	ReadFileUpTo(ctx context.Context, repo, commit, path string, max int64) ([]byte, error)
}

// actionGitSource adapts GitReader onto actions.GitSource.
type actionGitSource struct {
	git GitReader
	max int64
}

func (s actionGitSource) ResolveRef(ctx context.Context, repo, ref string) (string, error) {
	return s.git.ResolveRef(ctx, repo, ref)
}

func (s actionGitSource) ReadFile(ctx context.Context, repo, commit, path string) ([]byte, error) {
	if sized, ok := s.git.(actionSizedReader); ok {
		return sized.ReadFileUpTo(ctx, repo, commit, path, s.max)
	}
	return s.git.ReadFile(ctx, repo, commit, path)
}

func (s actionGitSource) ListTree(ctx context.Context, repo, commit, dir string) ([]actions.TreeEntry, error) {
	if lister, ok := s.git.(actionTreeLister); ok {
		return lister.ListTree(ctx, repo, commit, dir)
	}
	return nil, fmt.Errorf("this instance's git reader cannot list an action directory (remote actions need tree listing)")
}

// newRunActionResolver builds the resolver for one run: local actions read from
// the run commit, remote actions from the configured source (this instance by
// default) under the instance policy.
func (s *Service) newRunActionResolver(repo, commit string) *actionResolver {
	policy := normalizeActionPolicy(s.ActionPolicy)
	res := newActionResolver(s.Git, repo, commit).withRemote(nil, policy)
	if s.Git == nil {
		return res
	}
	limits := actions.DefaultLimits()
	if s.ActionMaxBytes != 0 {
		limits.MaxBytes = s.ActionMaxBytes
	}
	local := &actions.LocalSource{Git: actionGitSource{git: s.Git, max: limits.MaxFileBytes}, Limits: limits}
	var remote *actions.HTTPSource
	if s.ActionSource != "" {
		if src, err := actions.NewHTTPSource(s.ActionSource, limits); err == nil {
			remote = src
		}
	}
	// The guard is created once by New (an instance-wide, read-only pointer);
	// a Service built by hand without one simply has no cross-run check.
	res.withRemote(actions.NewResolver(policy, local, remote, s.ActionDigests, limits), policy)
	return res
}

// normalizeActionPolicy returns the configured policy, or D-5's default (an
// allowlist with SHA required outside it) when the service has none.
func normalizeActionPolicy(p actions.Policy) actions.Policy {
	if p.Mode == "" && len(p.Allow) == 0 && !p.RequireSHA {
		return actions.DefaultPolicy()
	}
	return p.Normalize()
}
