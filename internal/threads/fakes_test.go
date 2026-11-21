package threads

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// --- fake GitReader --------------------------------------------------------

// fakeGit is a map-backed repository: commit → path → bytes, plus refs. It is
// the discovery seam's scripted stand-in (the real reader is pinned against a
// real git dir in git_test.go).
type fakeGit struct {
	files   map[string]map[string][]byte
	refs    map[string]string
	list    []string // overrides the derived list when non-nil
	listErr error
	readErr error
	refErr  error
	lists   int
	reads   int
}

func newFakeGit() *fakeGit {
	return &fakeGit{files: map[string]map[string][]byte{}, refs: map[string]string{}}
}

func (f *fakeGit) add(commit, path string, body string) {
	if f.files[commit] == nil {
		f.files[commit] = map[string][]byte{}
	}
	f.files[commit][path] = []byte(body)
}

func (f *fakeGit) ListWorkflowFiles(_ context.Context, _ string, commit string) ([]string, error) {
	f.lists++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.list != nil {
		return append([]string(nil), f.list...), nil
	}
	var out []string
	for p := range f.files[commit] {
		if workflowPath(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeGit) ReadFile(_ context.Context, _ string, commit, path string) ([]byte, error) {
	f.reads++
	if f.readErr != nil {
		return nil, f.readErr
	}
	body, ok := f.files[commit][path]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	return body, nil
}

func (f *fakeGit) ResolveRef(_ context.Context, _ string, ref string) (string, error) {
	if f.refErr != nil {
		return "", f.refErr
	}
	sha, ok := f.refs[ref]
	if !ok {
		return "", fmt.Errorf("%w: unknown ref %q", ErrNotFound, ref)
	}
	return sha, nil
}

// --- fake roles ------------------------------------------------------------

type fakeRoles struct {
	roles  map[string]string
	public bool
}

func (f *fakeRoles) Resolve(_ context.Context, _, _ string, p auth.Principal) (identity.Role, *identity.AccessDoc) {
	if p.Admin {
		return identity.RoleAdmin, nil
	}
	if r := f.roles[strings.ToLower(p.Name)]; r != "" {
		return identity.Role(r), nil
	}
	if p.Anonymous {
		return "", nil
	}
	return identity.RoleRead, nil
}

func (f *fakeRoles) CheckRead(_ context.Context, _, _ string, p auth.Principal) *auth.AuthError {
	if p.Admin || p.Write {
		return nil
	}
	if p.Anonymous {
		if f.public {
			return nil
		}
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	}
	if _, ok := f.roles[strings.ToLower(p.Name)]; !ok && !f.public {
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "private repository"}
	}
	return nil
}

// --- capture CheckSink -----------------------------------------------------

type checkReport struct {
	Repo, SHA, Context, State, Description string
	StartedAt, CompletedAt                 *string
}

type fakeChecks struct {
	reports []checkReport
	err     error
}

func (f *fakeChecks) Report(_ context.Context, repo, sha, checkContext, state, description string, startedAt, completedAt *string) error {
	if f.err != nil {
		return f.err
	}
	f.reports = append(f.reports, checkReport{
		Repo: repo, SHA: sha, Context: checkContext, State: state,
		Description: description, StartedAt: startedAt, CompletedAt: completedAt,
	})
	return nil
}

// --- env -------------------------------------------------------------------

const testSHA = "1111111111111111111111111111111111111111"
const baseSHA = "2222222222222222222222222222222222222222"

const workflowYAML = `name: CI
on:
  push:
    branches: [main]
  pull_request:
  workflow_dispatch:
  schedule:
    - cron: "0 3 * * *"
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo build
  test:
    runs-on: ubuntu-latest
    needs: [build]
    steps:
      - run: echo test
`

type testEnv struct {
	store  *store.Memory
	git    *fakeGit
	roles  *fakeRoles
	checks *fakeChecks
	svc    *Service
	now    time.Time
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{
		store:  store.NewMemory(),
		git:    newFakeGit(),
		roles:  &fakeRoles{public: true, roles: map[string]string{"writer": "write", "reader": "read"}},
		checks: &fakeChecks{},
		now:    time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
	}
	e.svc = New(e.store, e.roles)
	e.svc.Git = e.git
	e.svc.Checks = e.checks
	e.svc.Now = func() time.Time { e.now = e.now.Add(time.Second); return e.now }
	e.git.add(testSHA, ".spindle/workflows/ci.yml", workflowYAML)
	e.git.refs["refs/heads/main"] = testSHA
	return e
}

func (e *testEnv) public() auth.Principal { return auth.Anonymous() }
func (e *testEnv) writer() auth.Principal { return auth.Principal{Name: "writer"} }
func (e *testEnv) reader() auth.Principal { return auth.Principal{Name: "reader"} }

func ctx() context.Context { return context.Background() }

// mustCreate creates the push run for the fake HEAD and returns it.
func (e *testEnv) mustCreate(t *testing.T) []*Run {
	t.Helper()
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "writer"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return runs
}
