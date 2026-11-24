// branches.go — the branches page's write composition (repo-browsing parity):
// the branches service over the P6 role/push gate owned by identity, stock git
// through the bounded pool, repo dirs through the WAL registry, and ref
// creates/deletes through the WAL publish funnel. Nothing here is a second
// writer: one REF_UPDATE txn per call, CAS'd by the manifest verify step, so a
// concurrent push or a moved branch fails the oid check instead of clobbering.
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/branches"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/store/proto"
	"github.com/crueber/spindle/internal/wal"
)

// apiEnvRepo is the nil-safe view accessor for buildCollab (apiEnv may be nil
// in tests that only exercise the push path).
func apiEnvRepo(e *api.Env) api.RepoView {
	if e == nil {
		return nil
	}
	return e.Repo
}

// newBranchesService builds the branch write surface. view is the api.RepoView
// the default branch is read from (nil in tests that do not wire one; the
// service then fails closed on delete).
func newBranchesService(st store.ObjectStore, ident *identity.Service, reg *wal.Registry, gitBinary string, view api.RepoView) *branches.Service {
	var gate branches.Gate // a typed-nil *identity.Service is NOT a nil Gate
	if ident != nil {
		gate = ident
	}
	svc := &branches.Service{
		Store: st,
		Gate:  gate,
		Dirs:  &pullsDirs{reg: reg},
		Git:   branches.NewSubprocessGit(gitBinary),
		Refs:  &branchesPublisher{reg: reg},
	}
	svc.DefaultBranch = func(ctx context.Context, owner, repo string) (string, error) {
		if view == nil {
			return "", errors.New("repository view is not wired")
		}
		id, err := git.ParseRepoId(owner + "/" + repo)
		if err != nil {
			return "", err
		}
		ref, ok, err := view.Head(ctx, id)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", nil // unborn HEAD
		}
		return strings.TrimPrefix(ref.Name, "refs/heads/"), nil
	}
	return svc
}

// branchesPublisher publishes branch creates/deletes through the normal WAL
// publish funnel: one single-update REF_UPDATE txn (OldOid zero = create,
// NewOid zero = delete). The manifest CAS arbitrates concurrent pushes and
// concurrent page writes; a lost CAS is branches.ErrConflict (409), never a
// force.
type branchesPublisher struct {
	reg *wal.Registry
}

// zeroOid renders the all-zero absent marker for the repo's object format.
func (p *branchesPublisher) zeroOid(ctx context.Context, repo, likeSHA string) string {
	if len(likeSHA) == 64 {
		return strings.Repeat("0", 64)
	}
	h, err := p.reg.Open(ctx, repo)
	if err != nil {
		return strings.Repeat("0", 40)
	}
	m, _ := h.ManifestSnapshot()
	if m != nil && m.ObjectFormat == "sha256" {
		return strings.Repeat("0", 64)
	}
	return strings.Repeat("0", 40)
}

func (p *branchesPublisher) CreateBranch(ctx context.Context, repo, name, sha string, meta map[string]string) error {
	h, err := p.reg.Open(ctx, repo)
	if err != nil {
		return fmt.Errorf("%w: %v", branches.ErrUnavailable, err)
	}
	res, err := h.Publish(ctx, wal.PublishRequest{
		Txn: &proto.RefTransaction{Updates: []*proto.RefUpdate{{
			Name:   "refs/heads/" + name,
			OldOid: p.zeroOid(ctx, repo, sha),
			NewOid: sha,
		}}},
		Meta: copyMeta(meta),
	})
	if err != nil {
		return fmt.Errorf("%w: %v", branches.ErrUnavailable, err)
	}
	return branchesPublishVerdict(res)
}

func (p *branchesPublisher) DeleteBranch(ctx context.Context, repo, name, old string, meta map[string]string) error {
	h, err := p.reg.Open(ctx, repo)
	if err != nil {
		return fmt.Errorf("%w: %v", branches.ErrUnavailable, err)
	}
	res, err := h.Publish(ctx, wal.PublishRequest{
		Txn: &proto.RefTransaction{Updates: []*proto.RefUpdate{{
			Name:   "refs/heads/" + name,
			OldOid: old,
			NewOid: p.zeroOid(ctx, repo, old),
		}}},
		Meta: copyMeta(meta),
	})
	if err != nil {
		return fmt.Errorf("%w: %v", branches.ErrUnavailable, err)
	}
	return branchesPublishVerdict(res)
}

// branchesPublishVerdict maps per-ref WAL verdicts onto the service's
// sentinels: a lost CAS is a conflict, anything else is unavailable.
func branchesPublishVerdict(res wal.PublishResult) error {
	for _, rr := range res.PerRef {
		if rr.Err == nil {
			continue
		}
		if rr.Err.Kind == wal.RefErrConflict || rr.Err.Kind == wal.RefErrStale {
			return fmt.Errorf("%w: %s", branches.ErrConflict, rr.Err.Detail)
		}
		return fmt.Errorf("%w: publish %s: %s", branches.ErrUnavailable, rr.Name, rr.Err.Detail)
	}
	return nil
}

func copyMeta(meta map[string]string) map[string]string {
	out := make(map[string]string, len(meta))
	for k, v := range meta {
		out[k] = v
	}
	return out
}
