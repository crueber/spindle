package server

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/pages"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/wal"
)

// fakeEngine implements Engine for tests.
type fakeEngine struct {
	placement     Placement // zero value lacks Serve; newTestServer enables it
	bundles       BundleList
	synced        []wal.SyncLevel
	published     int
	exists        bool
	noCreate      bool // AutoCreate returns false when set
	pubErr        error
	lastPrincipal string
	pubPerRef     []wal.RefResult
	syncErr       error
	repoErr       error
	placementErr  error
	repoCreate    func(root string, id git.RepoId, format git.ObjectFormat) (*git.LocalRepo, error)
}

func (f *fakeEngine) Sync(ctx context.Context, id git.RepoId, level wal.SyncLevel) error {
	f.synced = append(f.synced, level)
	if f.syncErr != nil {
		return f.syncErr
	}
	if !f.exists {
		return wal.ErrNotFound(id.String())
	}
	return nil
}

func (f *fakeEngine) Repo(ctx context.Context, id git.RepoId, create bool, format git.ObjectFormat) (*git.LocalRepo, error) {
	if f.repoErr != nil {
		return nil, f.repoErr
	}
	if !f.exists && !create {
		return nil, wal.ErrNotFound(id.String())
	}
	root := ctx.Value(repoRootKey{}).(string)
	return git.InitLocalRepo(root, id, format)
}

type repoRootKey struct{}

func (f *fakeEngine) Publish(ctx context.Context, id git.RepoId, req *git.PushRequest, principal string, access wal.ObjectAccess) (wal.PublishResult, error) {
	f.published++
	f.lastPrincipal = principal
	if f.pubErr != nil {
		return wal.PublishResult{}, f.pubErr
	}
	if f.pubPerRef != nil {
		return wal.PublishResult{PerRef: f.pubPerRef}, nil
	}
	return wal.PublishResult{PerRef: []wal.RefResult{{Name: "refs/heads/main"}}}, nil
}

func (f *fakeEngine) Placement(ctx context.Context, id git.RepoId) (Placement, error) {
	if f.placementErr != nil {
		return Placement{}, f.placementErr
	}
	return f.placement, nil
}

func (f *fakeEngine) Bundles(ctx context.Context, id git.RepoId, filter string) (BundleList, error) {
	if !f.exists {
		return BundleList{}, wal.ErrNotFound(id.String())
	}
	return f.bundles, nil
}

func (f *fakeEngine) AutoCreate(ctx context.Context, id git.RepoId) bool { return !f.noCreate }

// fakeStore wraps the real memory store (02_storage_protobuf.md contract).
func newFakeStore() store.ObjectStore { return store.NewMemory() }

// fakeUI is a pages.Provider stub for the server's page seam: it answers the
// paths it is given (a marker page) and reports false for everything else,
// exactly like the real provider's sentinel. It records what it was asked, so
// tests can assert the router reached the page layer.
type fakeUI struct {
	paths map[string]bool
	seen  []string
	auth  pages.Auth
}

func (f *fakeUI) BindAuth(a pages.Auth) { f.auth = a }

func (f *fakeUI) Serve(w http.ResponseWriter, r *http.Request) bool {
	f.seen = append(f.seen, r.URL.Path)
	if !f.paths[r.URL.Path] || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "<!doctype html><html><body><div id=\"page\">rendered page</div></body></html>")
	return true
}

var _ = time.Now
var _ = auth.Anonymous

func TestFakesCompile(t *testing.T) {
	var _ Engine = &fakeEngine{}
	var _ store.ObjectStore = newFakeStore()
}
