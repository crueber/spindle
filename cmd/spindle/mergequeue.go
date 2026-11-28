// mergequeue.go — the merge queue's composition (the Merge queue card;
// docs/features/22_merge_queue.md): the queue service over the shared store,
// the identity roles, and the three narrow seams it consumes:
//
//   - pulls: prepare the temporary merge commit / publish the documented ref,
//     land through the one merge task, delete the ref.
//   - threads: read the merge_group runs reporting the queue head
//     (Service.MergeGroupRuns; PAR-016).
//   - checks: the require_checks progress for that head
//     (Service.RequiredChecksProgress — the same gate the merge task uses).
//
// Nothing here is a second merge path: the landing call is
// pulls.Service.LandQueuedMerge, which re-runs every protection gate under the
// merge task. The maintainer sweeper (mergequeue_sweep.go) runs the restart
// recovery pass per repository.
package main

import (
	"context"
	"net/http"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/checks"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/mergequeue"
	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads"
)

// newMergeQueueService builds the queue over st/ident and its seams, and
// registers the discovery templates (law 12).
func newMergeQueueService(st store.ObjectStore, ident *identity.Service, pullsSvc *pulls.Service, threadsSvc *threads.Service, checksSvc *checks.Service) (*mergequeue.Service, *mergequeue.Handler) {
	api.RegisterExposed(mergequeue.ExposedTemplates...)
	svc := mergequeue.New(st)
	svc.Roles = ident
	if pullsSvc != nil {
		svc.Pulls = pullsSvc
	}
	if threadsSvc != nil {
		svc.Runs = queueRuns{svc: threadsSvc}
	}
	if checksSvc != nil {
		svc.Checks = queueChecks{svc: checksSvc}
	}
	h := &mergequeue.Handler{Svc: svc}
	return svc, h
}

// chainMergeQueue fronts the core mux with the queue surface (Seam 1);
// authentication resolves through the server chain (Seam 2), including the
// broker-forwarding rule.
func chainMergeQueue(srv *server.Server, h *mergequeue.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().AuthenticateForwarded(r, srv.Config())
	}
	srv.ChainExtra(h)
}

// queueRuns adapts threads.Service.MergeGroupRuns onto the queue's narrow
// read (no principal: the driver is a background pass, not a request).
type queueRuns struct{ svc *threads.Service }

func (q queueRuns) MergeGroupRuns(ctx context.Context, repo, sha string, since time.Time) ([]mergequeue.RunStatus, error) {
	runs, err := q.svc.MergeGroupRunsAfter(ctx, repo, sha, since)
	if err != nil {
		return nil, err
	}
	out := make([]mergequeue.RunStatus, 0, len(runs))
	for _, r := range runs {
		out = append(out, mergequeue.RunStatus{ID: r.ID, State: r.State})
	}
	return out, nil
}

// queueChecks adapts checks.Service.RequiredChecksProgress.
type queueChecks struct{ svc *checks.Service }

func (q queueChecks) RequiredChecksProgress(ctx context.Context, owner, repo, sha, baseRef, principal string) (mergequeue.CheckProgress, error) {
	p, err := q.svc.RequiredChecksProgress(ctx, owner, repo, sha, baseRef, principal)
	if err != nil {
		return mergequeue.CheckProgress{}, err
	}
	return mergequeue.CheckProgress{
		Required: p.Required, Offenders: p.Offenders, OK: p.OK, Pending: p.Pending,
	}, nil
}

// compile-time seam assertions.
var (
	_ mergequeue.RoleService  = (*identity.Service)(nil)
	_ mergequeue.PullsSeam    = (*pulls.Service)(nil)
	_ mergequeue.RunReader    = queueRuns{}
	_ mergequeue.ChecksReader = queueChecks{}
	_ server.ExtraRoutes      = (*mergequeue.Handler)(nil)
)
