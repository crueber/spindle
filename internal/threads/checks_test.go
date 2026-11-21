package threads

import (
	"context"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/checks"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// This file is the test-only bridge to the REAL checks service: it proves the
// 15 §2 integration (a Threads job's record is what the require_checks merge
// gate reads), not a stub of it. internal/threads never imports
// internal/checks in production (CheckSink is the seam).

// realCheckSink adapts the Threads CheckSink onto checks.RecordStatus.
type realCheckSink struct{ svc *checks.Service }

func (r realCheckSink) Report(ctx context.Context, repo, sha, checkContext, state, description string, startedAt, completedAt *string) error {
	id, err := git.ParseRepoId(repo)
	if err != nil {
		return err
	}
	_, rerr := r.svc.RecordStatus(ctx, id.Owner, id.Name, sha, "threads", checks.ReportInput{
		Context:     checkContext,
		State:       state,
		Description: description,
		StartedAt:   startedAt,
		CompletedAt: completedAt,
	})
	return rerr
}

// knownCommits is a CommitChecker that accepts one sha.
type knownCommits struct{ sha string }

func (k knownCommits) ResolveCommit(_ context.Context, _, sha string) (string, error) {
	if sha == k.sha {
		return k.sha, nil
	}
	return "", checks.ErrNotFound
}

// checksTestService builds a real checks service over mem.
func checksTestService(t *testing.T, mem *store.Memory, sha string) *checks.Service {
	t.Helper()
	svc := checks.New(mem, nil)
	svc.Commits = knownCommits{sha: sha}
	svc.Now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	return svc
}

// putPolicy writes repos/o/r/policy.json.
func putPolicy(mem *store.Memory, doc string) error {
	_, err := store.PutBytes(context.Background(), mem, "repos/o/r/policy.json", []byte(doc),
		store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/json"})
	return err
}

// authAnonymous is the anonymous principal for service-level tests.
func authAnonymous() auth.Principal { return auth.Anonymous() }
