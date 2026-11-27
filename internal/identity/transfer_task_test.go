// transfer_task_test.go — the `repo-transfer` task (PLAN-V2 §T.1): joining a
// running move, crash/resume convergence, the cross-reference families and the
// loud verification step. The per-family rewrite arithmetic lives with its
// owner (pulls/forkmove_test.go, sizecatalog, packages/gc_test.go); these tests
// pin the orchestration, the ordering and the retry.
package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/crueber/spindle/internal/sizecatalog"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/store/proto"
	"github.com/crueber/spindle/internal/wal"
)

// --- fakes ------------------------------------------------------------------

// fakeServing records the serving-copy calls (the real one is *wal.Registry).
type fakeServing struct {
	mu          sync.Mutex
	invalid     []string
	verified    []string
	failVerify  error
	failInvalid error
}

func (f *fakeServing) Invalidate(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalid = append(f.invalid, id)
	return f.failInvalid
}

func (f *fakeServing) VerifyRepo(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verified = append(f.verified, id)
	return f.failVerify
}

// manifestAwareServing records, per id, how many Invalidate calls saw the
// repo's manifest already absent from the bucket — the evidence that the
// handle was flushed at/after the move's commit point, not only before the
// copy pass.
type manifestAwareServing struct {
	fakeServing
	st               store.ObjectStore
	goneAtInvalidate map[string]int
}

func (m *manifestAwareServing) Invalidate(id string) error {
	owner, repo, ok := strings.Cut(id, "/")
	if ok {
		present, err := store.Exists(context.Background(), m.st, store.RepoPrefix(owner, repo)+manifestName)
		if err == nil && !present {
			m.goneAtInvalidate[id]++
		}
	}
	return m.fakeServing.Invalidate(id)
}

// fakeForks is a faithful enough ForkMover: it keeps the parent index and the
// child pointers in memory and applies the same old→new rewrites the real
// implementation applies, so a retried task converges exactly as it would.
type fakeForks struct {
	mu     sync.Mutex
	parent map[string]string    // repo id -> parent id
	index  map[string][]string  // parent id -> child ids
	points map[string][2]string // child id -> {parent, root}
	calls  []string

	failParentOf    error
	failChildMoved  error
	failParentMoved error
	failVerify      error
}

func newFakeForks() *fakeForks {
	return &fakeForks{
		parent: map[string]string{},
		index:  map[string][]string{},
		points: map[string][2]string{},
	}
}

func (f *fakeForks) ParentOf(ctx context.Context, owner, repo string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "parentOf:"+owner+"/"+repo)
	if f.failParentOf != nil {
		return "", false, f.failParentOf
	}
	p, ok := f.parent[owner+"/"+repo]
	return p, ok && p != "", nil
}

func (f *fakeForks) ChildMoved(ctx context.Context, parentOwner, parentRepo, oldChild, newChild string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := parentOwner + "/" + parentRepo
	f.calls = append(f.calls, "childMoved:"+key+":"+oldChild+"->"+newChild)
	if f.failChildMoved != nil {
		return false, f.failChildMoved
	}
	rows := f.index[key]
	changed := false
	for i, r := range rows {
		if r == oldChild {
			rows[i] = newChild
			changed = true
		}
	}
	if changed {
		f.index[key] = rows
	}
	return changed, nil
}

func (f *fakeForks) ParentMoved(ctx context.Context, oldID, newID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "parentMoved:"+oldID+"->"+newID)
	if f.failParentMoved != nil {
		return 0, f.failParentMoved
	}
	changed := 0
	for _, child := range f.index[newID] {
		pt := f.points[child]
		moved := false
		if pt[0] == oldID {
			pt[0] = newID
			moved = true
		}
		if pt[1] == oldID {
			pt[1] = newID
			moved = true
		}
		if moved {
			f.points[child] = pt
			changed++
		}
	}
	return changed, nil
}

func (f *fakeForks) VerifyMoved(ctx context.Context, oldID, newID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "verify:"+oldID+"->"+newID)
	if f.failVerify != nil {
		return f.failVerify
	}
	if p, ok := f.parent[newID]; ok && p != "" {
		for _, row := range f.index[p] {
			if row == oldID {
				return fmt.Errorf("%s still lists %s", p, oldID)
			}
		}
	}
	for _, child := range f.index[newID] {
		if pt, ok := f.points[child]; ok && (pt[0] == oldID || pt[1] == oldID) {
			return fmt.Errorf("%s still names %s", child, oldID)
		}
	}
	return nil
}

func (f *fakeForks) hasCall(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// fakePackages records the registry invalidation.
type fakePackages struct {
	mu    sync.Mutex
	moves [][4]string
}

func (f *fakePackages) RepoMoved(oldOwner, oldRepo, newOwner, newRepo string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moves = append(f.moves, [4]string{oldOwner, oldRepo, newOwner, newRepo})
}

// blockingStore blocks the first Put after it is armed (the copy pass), so a
// second start can be observed while the first move is in flight.
type blockingStore struct {
	store.ObjectStore
	armed   bool
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if b.armed {
		b.once.Do(func() {
			close(b.started)
			<-b.release
		})
	}
	return b.ObjectStore.Put(ctx, key, body, opts)
}

// hidingStore makes one exact HEAD look absent (the verification probe test).
type hidingStore struct {
	store.ObjectStore
	hide string
}

func (h *hidingStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	if key == h.hide {
		return nil, nil
	}
	return h.ObjectStore.Head(ctx, key)
}

// --- fixtures ---------------------------------------------------------------

// seedCatalogRow puts a catalog row for id into the bucket-root aggregate.
func seedCatalogRow(t *testing.T, svc *Service, id string, size uint64) {
	t.Helper()
	if _, err := sizecatalog.WriteCatalog(context.Background(), svc.Store, func(cur *proto.RepoCatalog) (*proto.RepoCatalog, error) {
		if cur == nil {
			cur = &proto.RepoCatalog{}
		}
		cur.Repos = append(cur.Repos, id)
		cur.Entries = append(cur.Entries, &proto.RepoCatalogEntry{Repo: id, SizeBytes: size, ObjectCount: 3, HeadSeq: 9,
			LastCommitSHA: "beef"})
		return cur, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// seedPackages writes two registry objects under the repo prefix (a tag and a
// blob) so the move must carry the OCI namespace.
func seedPackages(t *testing.T, svc *Service, owner, repo string) {
	t.Helper()
	ctx := context.Background()
	for rel, body := range map[string]string{
		"packages/registry/tags/latest":           `{"digest":"sha256:aa"}`,
		"packages/registry/blobs/sha256/aa":       "blob-bytes",
		"packages/registry/manifests/sha256/aaaa": `{"schemaVersion":2}`,
	} {
		if _, err := store.PutBytes(ctx, svc.Store, store.RepoPrefix(owner, repo)+rel, []byte(body),
			store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream"}); err != nil {
			t.Fatal(err)
		}
	}
}

func catalogHas(t *testing.T, svc *Service, id string) bool {
	t.Helper()
	cat, err := sizecatalog.ReadCatalog(context.Background(), svc.Store)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range cat.Entries {
		if e != nil && e.Repo == id {
			return true
		}
	}
	return false
}

func catalogRow(t *testing.T, svc *Service, id string) *proto.RepoCatalogEntry {
	t.Helper()
	cat, err := sizecatalog.ReadCatalog(context.Background(), svc.Store)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range cat.Entries {
		if e != nil && e.Repo == id {
			return e
		}
	}
	return nil
}

// --- tests ------------------------------------------------------------------

// deleteHookStore runs hook before the underlying delete, so a test can act
// inside a specific delete window (the transfer's manifest delete).
type deleteHookStore struct {
	store.ObjectStore
	hook func(key string)
}

func (d *deleteHookStore) Delete(ctx context.Context, key string, token store.Version) error {
	if d.hook != nil {
		d.hook(key)
	}
	return d.ObjectStore.Delete(ctx, key, token)
}

// TestTransferOldPathIs404EvenIfAHandleRacesTheDelete reproduces the e2e
// race deterministically: a read opens the SOURCE serving handle in the
// window between the pre-copy flush and the manifest delete (the maintainer
// sweep or a client request does this in production). Before the fix that
// handle survived the task, and the old path answered 503 — the freshness
// conditional GET on an absent manifest surfaced as a store fault — while the
// destination served. The contract: once the move commits, the old id is not
// a repository, whatever raced it.
func TestTransferOldPathIs404EvenIfAHandleRacesTheDelete(t *testing.T) {
	f := newTransferFix(t)
	reg := f.reg
	f.svc.Serving = reg
	ctx := f.ctx

	raced := false
	f.svc.Store = &deleteHookStore{ObjectStore: f.svc.Store, hook: func(key string) {
		if key != store.RepoPrefix("acme", "r1")+manifestName {
			return
		}
		// The racing read: the manifest is still there this instant, so the
		// registry caches a handle for the source that outlives it.
		if _, err := reg.Open(ctx, "acme/r1"); err == nil {
			raced = true
		}
	}}

	if err := f.svc.TransferRepo(ctx, "acme", "r1", "beta", "r1"); err != nil {
		t.Fatalf("TransferRepo: %v", err)
	}
	if !raced {
		t.Fatal("fixture never opened the source handle in the delete window")
	}
	// The destination is served; the old path is gone — not-erroring is not
	// enough, it must be the NOT-FOUND class (the HTTP layer's 404).
	syncRepo := func(id string) error {
		h, err := reg.Open(ctx, id)
		if err != nil {
			return err
		}
		g, err := h.Sync(ctx, wal.LevelRefs)
		if err != nil {
			return err
		}
		g.Release()
		return nil
	}
	if err := syncRepo("beta/r1"); err != nil {
		t.Fatalf("destination sync after the move: %v", err)
	}
	// isNotFound is the HTTP mapping's own predicate (WalEngine.open adopts a
	// store not-found as the engine's): anything else is a 503-class fault.
	isNotFound := func(err error) bool {
		var we *wal.WalError
		return store.IsNotFound(err) || (errors.As(err, &we) && we.Kind == wal.WalErrNotFound)
	}
	err := syncRepo("acme/r1")
	if err == nil {
		t.Fatal("old path still syncs after the move")
	}
	if !isNotFound(err) {
		t.Fatalf("old path sync = %v, want not-found (404), never a store fault (503)", err)
	}
	if _, err := reg.Open(ctx, "acme/r1"); err == nil {
		t.Fatal("registry still hands out a handle for the moved-away id")
	} else if !isNotFound(err) {
		t.Fatalf("old path open = %v, want not-found", err)
	}
}

// TestTransferSourceHandleFlushedAfterCommit pins the ordering the old path
// depends on: the source serving handle is dropped once the move commits, at
// or after the point where the source manifest is gone — not only in the
// pre-copy flush. A handle re-opened between that flush and the manifest
// delete used to survive the task, and the old path then answered 503 (a
// store fault from the freshness conditional GET on an absent manifest)
// instead of 404. The engine half of the fix (a vanished manifest under a
// live handle is NOT-FOUND) lives in internal/wal's sync tests; this test
// proves the move stops handing the stale handle out in the first place.
func TestTransferSourceHandleFlushedAfterCommit(t *testing.T) {
	f := newTransferFix(t)
	sv := &manifestAwareServing{st: f.svc.Store, goneAtInvalidate: map[string]int{}}
	f.svc.Serving = sv
	if err := f.svc.TransferRepo(f.ctx, "acme", "r1", "beta", "r1"); err != nil {
		t.Fatalf("TransferRepo: %v", err)
	}
	if sv.goneAtInvalidate["acme/r1"] == 0 {
		t.Fatalf("source handle never flushed after the move committed (invalidations: %v); "+
			"a handle re-opened during the copy window stays cached and the old path answers 503, want 404", sv.invalid)
	}
}

// TestTransferTaskFamilies is the completeness proof: catalog row, both fork
// directions, packages and serving copies all follow the move, and the old
// prefix is gone.
func TestTransferTaskFamilies(t *testing.T) {
	f := newTransferFix(t)
	svc := f.svc
	ctx := f.ctx

	seedTransferRepo(t, svc, "acme", "up") // the parent (unmoved)
	seedCatalogRow(t, svc, "acme/r1", 4096)
	seedPackages(t, svc, "acme", "r1")

	fk := newFakeForks()
	// r1 is a fork of acme/up AND the parent of carol/kid.
	fk.parent["acme/r1"] = "acme/up"
	fk.parent["beta/r1"] = "acme/up" // the moved fork.json travels with the prefix
	fk.index["acme/up"] = []string{"acme/r1"}
	fk.index["acme/r1"] = []string{"carol/kid"}
	fk.index["beta/r1"] = []string{"carol/kid"}
	fk.points["carol/kid"] = [2]string{"acme/r1", "acme/r1"}
	svc.Forks = fk
	pk := &fakePackages{}
	svc.Packages = pk
	sv := &fakeServing{}
	svc.Serving = sv

	if err := svc.TransferRepo(ctx, "acme", "r1", "beta", "r1"); err != nil {
		t.Fatalf("TransferRepo: %v", err)
	}

	// The old prefix is gone (manifest + the package namespace).
	if keys := listKeys(t, svc, "repos/acme/r1/"); len(keys) != 0 {
		t.Fatalf("old prefix residue: %v", keys)
	}
	// Catalog: the row moved with its fields, the old name is gone.
	if catalogHas(t, svc, "acme/r1") {
		t.Fatal("catalog still names the old path")
	}
	row := catalogRow(t, svc, "beta/r1")
	if row == nil || row.SizeBytes != 4096 || row.ObjectCount != 3 || row.LastCommitSHA != "beef" {
		t.Fatalf("catalog row did not follow: %+v", row)
	}
	// Packages: every registry object followed the prefix.
	for rel := range map[string]bool{
		"packages/registry/tags/latest":           true,
		"packages/registry/blobs/sha256/aa":       true,
		"packages/registry/manifests/sha256/aaaa": true,
	} {
		if ok, err := store.Exists(ctx, svc.Store, store.RepoPrefix("beta", "r1")+rel); err != nil || !ok {
			t.Fatalf("package object %s did not follow: %v %v", rel, ok, err)
		}
	}
	if len(pk.moves) != 1 || pk.moves[0] != [4]string{"acme", "r1", "beta", "r1"} {
		t.Fatalf("packages invalidation = %v", pk.moves)
	}
	// Forks: the parent's row was rewritten, the child's pointers followed.
	if got := fk.index["acme/up"]; len(got) != 1 || got[0] != "beta/r1" {
		t.Fatalf("parent index row = %v", got)
	}
	if pt := fk.points["carol/kid"]; pt != [2]string{"beta/r1", "beta/r1"} {
		t.Fatalf("child pointers = %v", pt)
	}
	if !fk.hasCall("parentMoved:acme/r1->beta/r1") || !fk.hasCall("verify:acme/r1->beta/r1") {
		t.Fatalf("fork steps not exercised: %v", fk.calls)
	}
	// Serving copies: quiesced on both sides, verified at the destination.
	if len(sv.verified) == 0 || sv.verified[len(sv.verified)-1] != "beta/r1" {
		t.Fatalf("destination never verified: %v", sv.verified)
	}
	found := false
	for _, id := range sv.invalid {
		if id == "acme/r1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("source serving copy never invalidated: %v", sv.invalid)
	}
}

// TestTransferTaskJoin: a second start while the move runs joins (same task id,
// joined=true, one record) instead of duplicating the work.
func TestTransferTaskJoin(t *testing.T) {
	f := newTransferFix(t)
	bs := &blockingStore{ObjectStore: f.svc.Store, armed: true,
		started: make(chan struct{}), release: make(chan struct{})}
	f.svc.Store = bs
	h := testHandler(f.svc, alice)

	code, out, id := startTransfer(t, f, h, "/acme/r1/api/transfer", `{"owner":"beta"}`)
	if code != 202 || id == "" {
		t.Fatalf("first start = %d (%v)", code, out)
	}
	<-bs.started // the copy pass is now blocked mid-Put
	code2, out2, id2 := startTransfer(t, f, h, "/acme/r1/api/transfer", `{"owner":"beta"}`)
	if code2 != 202 {
		t.Fatalf("second start = %d, want 202", code2)
	}
	if out2["joined"] != true {
		t.Fatalf("second start must report joined: %v", out2)
	}
	if id2 != id {
		t.Fatalf("second start task id = %q, want the running %q", id2, id)
	}
	if rec := f.reg.Tasks().Running("acme/r1", TransferKind); rec == nil || rec.ID != id {
		t.Fatalf("running hint = %+v, want %s", rec, id)
	}
	close(bs.release)
	if rec := waitTask(t, f, id); rec.OK == nil || !*rec.OK {
		t.Fatalf("joined transfer failed: %+v", rec)
	}
	if n := len(f.reg.Tasks().List("acme/r1")); n != 1 {
		t.Fatalf("task records = %d, want exactly 1 (no duplicate)", n)
	}
}

// TestTransferTaskCrashBetweenFamilies: the move commits, a cross-reference
// family fails, the task reports it loudly, and the retry completes on the
// resume path (the source is already gone).
func TestTransferTaskCrashBetweenFamilies(t *testing.T) {
	f := newTransferFix(t)
	svc := f.svc
	ctx := f.ctx
	seedCatalogRow(t, svc, "acme/r1", 100)
	seedPackages(t, svc, "acme", "r1")
	fk := newFakeForks()
	// The index object lives inside the moved prefix: it was at acme/r1 before
	// the move and is read at beta/r1 after it (the target attempts below hit
	// both, exactly like the bucket copy).
	fk.index["acme/r1"] = []string{"carol/kid"}
	fk.index["beta/r1"] = []string{"carol/kid"}
	fk.points["carol/kid"] = [2]string{"acme/r1", "acme/r1"}
	fk.failParentMoved = errors.New("fork index store down")
	svc.Forks = fk

	err := svc.TransferRepo(ctx, "acme", "r1", "beta", "r1")
	if err == nil {
		t.Fatal("a failed fork rewrite must fail the move")
	}
	if !strings.Contains(err.Error(), "fork pointers") {
		t.Fatalf("failure must name the family: %v", err)
	}
	// The move itself committed: destination complete, source manifest gone.
	if ok, _ := store.Exists(ctx, svc.Store, "repos/beta/r1/"+manifestName); !ok {
		t.Fatal("destination must be complete after the family failure")
	}
	if ok, _ := store.Exists(ctx, svc.Store, "repos/acme/r1/"+manifestName); ok {
		t.Fatal("source manifest must be gone after commit")
	}

	// The retry converges: resume path, then the families.
	fk.failParentMoved = nil
	if err := svc.TransferRepo(ctx, "acme", "r1", "beta", "r1"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if pt := fk.points["carol/kid"]; pt != [2]string{"beta/r1", "beta/r1"} {
		t.Fatalf("retry must complete the fork rewrite: %v", pt)
	}
	if !catalogHas(t, svc, "beta/r1") || catalogHas(t, svc, "acme/r1") {
		t.Fatal("retry must complete the catalog row move")
	}
}

// TestTransferTaskCrashMidMove: a copy crash (delete refused) leaves both
// prefixes; the retry adopts the byte-identical destination keys, commits, and
// deletes the source.
func TestTransferTaskCrashMidMove(t *testing.T) {
	f := newTransferFix(t)
	base := f.svc.Store
	// Fail the source-manifest delete on the first attempt (a crash after the
	// destination commit point but before the delete pass finished).
	flaky := &moveDeleteFailStore{ObjectStore: base, key: "repos/acme/r1/" + manifestName}
	f.svc.Store = flaky
	err := f.svc.TransferRepo(f.ctx, "acme", "r1", "beta", "r1")
	if !errors.Is(err, errBoom) {
		t.Fatalf("delete failure must surface: %v", err)
	}
	if ok, _ := store.Exists(f.ctx, base, "repos/beta/r1/"+manifestName); !ok {
		t.Fatal("destination must be complete on a delete failure")
	}
	// Retry with a healthy store: adopt the destination (byte-identical,
	// including the retargeted manifest) and finish.
	f.svc.Store = base
	if err := f.svc.TransferRepo(f.ctx, "acme", "r1", "beta", "r1"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if keys := listKeys(t, f.svc, "repos/acme/r1/"); len(keys) != 0 {
		t.Fatalf("retry left residue: %v", keys)
	}
	if doc, _, err := f.svc.GetAccess(f.ctx, "beta", "r1"); err != nil || doc == nil {
		t.Fatalf("destination access unreadable: %v", err)
	}
}

// moveDeleteFailStore fails Delete of one exact key once (a crash after the
// destination commit point but before the delete pass finished).
type moveDeleteFailStore struct {
	store.ObjectStore
	key  string
	done bool
	mu   sync.Mutex
}

func (d *moveDeleteFailStore) Delete(ctx context.Context, key string, v store.Version) error {
	d.mu.Lock()
	if key == d.key && !d.done {
		d.done = true
		d.mu.Unlock()
		return errBoom
	}
	d.mu.Unlock()
	return d.ObjectStore.Delete(ctx, key, v)
}

// TestTransferTaskVerificationFailsLoudly: each verification miss names the
// piece instead of reporting success.
func TestTransferTaskVerificationFailsLoudly(t *testing.T) {
	t.Run("refs unreadable", func(t *testing.T) {
		f := newTransferFix(t)
		f.svc.Serving = &fakeServing{failVerify: errors.New("git fsck says no")}
		err := f.svc.TransferRepo(f.ctx, "acme", "r1", "beta", "r1")
		if err == nil || !strings.Contains(err.Error(), "destination refs unreadable") {
			t.Fatalf("want a named refs failure, got %v", err)
		}
	})
	t.Run("package object missing", func(t *testing.T) {
		f := newTransferFix(t)
		seedPackages(t, f.svc, "acme", "r1")
		f.svc.Store = &hidingStore{ObjectStore: f.svc.Store,
			hide: store.RepoPrefix("beta", "r1") + "packages/registry/tags/latest"}
		err := f.svc.TransferRepo(f.ctx, "acme", "r1", "beta", "r1")
		if err == nil || !strings.Contains(err.Error(), "did not follow the move") {
			t.Fatalf("want a named package failure, got %v", err)
		}
	})
	t.Run("fork pointers inconsistent", func(t *testing.T) {
		f := newTransferFix(t)
		fk := newFakeForks()
		fk.failVerify = errors.New("stale row")
		f.svc.Forks = fk
		err := f.svc.TransferRepo(f.ctx, "acme", "r1", "beta", "r1")
		if err == nil || !strings.Contains(err.Error(), "fork pointers inconsistent") {
			t.Fatalf("want a named fork failure, got %v", err)
		}
	})
	t.Run("manifest misdirected", func(t *testing.T) {
		f := newTransferFix(t)
		// A destination whose manifest names a third repo: the move must refuse
		// to call it complete.
		bad := &proto.Manifest{FormatVersion: 1, Repo: "gamma/r1", ObjectFormat: "sha1", Revision: 3}
		if _, err := store.PutBytes(f.ctx, f.svc.Store, "repos/beta/r1/"+manifestName, bad.Marshal(),
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		if err := f.svc.TransferRepo(f.ctx, "acme", "r1", "beta", "r1"); !errors.Is(err, ErrConflict) {
			t.Fatalf("occupied destination must conflict: %v", err)
		}
	})
}

// TestTransferAdoptsPartialAttempt: a destination pre-populated with the bytes
// a crashed attempt would have written is adopted, not fought.
func TestTransferAdoptsPartialAttempt(t *testing.T) {
	f := newTransferFix(t)
	svc := f.svc
	ctx := f.ctx
	// Simulate the crash: the pack copy landed, the manifest never did.
	if _, err := store.PutBytes(ctx, svc.Store, "repos/beta/r1/packs/pack-1.pack", []byte("pack-bytes"),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.TransferRepo(ctx, "acme", "r1", "beta", "r1"); err != nil {
		t.Fatalf("adopt path: %v", err)
	}
	raw, _, err := store.GetBytes(ctx, svc.Store, "repos/beta/r1/packs/pack-1.pack", store.GetOptions{})
	if err != nil || string(raw) != "pack-bytes" {
		t.Fatalf("adopted bytes = %q, %v", raw, err)
	}
	if keys := listKeys(t, svc, "repos/acme/r1/"); len(keys) != 0 {
		t.Fatalf("source residue after adopt: %v", keys)
	}
}

// TestBeginTransferPreflight covers the request-path verdicts without starting
// a task: unknown source, occupied destination, and a byte-identical resumable
// destination.
func TestBeginTransferPreflight(t *testing.T) {
	f := newTransferFix(t)
	svc := f.svc
	ctx := f.ctx
	if _, _, err := svc.BeginTransfer(ctx, TransferRequest{SrcOwner: "acme", SrcRepo: "ghost", DstOwner: "beta", DstRepo: "ghost"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown source = %v, want ErrNotFound", err)
	}
	if _, _, err := svc.BeginTransfer(ctx, TransferRequest{SrcOwner: "acme", SrcRepo: "r1", DstOwner: "beta", DstRepo: "bad name"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad destination = %v, want ErrInvalid", err)
	}
	seedRepo(t, svc, "beta", "r1")
	if _, _, err := svc.BeginTransfer(ctx, TransferRequest{SrcOwner: "acme", SrcRepo: "r1", DstOwner: "beta", DstRepo: "r1"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("occupied = %v, want ErrConflict", err)
	}
	// Identical (retargeted) manifests mean a resumable attempt: allowed.
	srcRaw, _, _ := store.GetBytes(ctx, svc.Store, "repos/acme/r1/"+manifestName, store.GetOptions{})
	same := svc.rewriteTransferManifest(srcRaw, "beta/r1")
	if _, _, err := store.GetBytes(ctx, svc.Store, "repos/beta/r1/"+manifestName, store.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutBytes(ctx, svc.Store, "repos/beta/r1/"+manifestName, same,
		store.PutOptions{Mode: store.PutUpdate, IfVersion: "10", ContentType: "application/x-protobuf"}); err != nil {
		// Version tokens are backend-defined; the memory store's is "10" here,
		// but never hard-code it — retry with the read version instead.
		_, meta, _ := store.GetBytes(ctx, svc.Store, "repos/beta/r1/"+manifestName, store.GetOptions{})
		if _, perr := store.PutBytes(ctx, svc.Store, "repos/beta/r1/"+manifestName, same,
			store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/x-protobuf"}); perr != nil {
			t.Fatal(perr)
		}
	}
	rec, joined, err := svc.BeginTransfer(ctx, TransferRequest{SrcOwner: "acme", SrcRepo: "r1", DstOwner: "beta", DstRepo: "r1"})
	if err != nil || rec == nil || joined {
		t.Fatalf("resumable start = %+v, %v, %v", rec, joined, err)
	}
	waitTask(t, f, rec.ID)
	// No task surface = 503-shaped refusal, never a silent success.
	svc.Tasks = nil
	if _, _, err := svc.BeginTransfer(ctx, TransferRequest{SrcOwner: "acme", SrcRepo: "x", DstOwner: "beta", DstRepo: "x"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no registry = %v, want ErrUnavailable", err)
	}
}

// TestTransferRegistryKind pins the Seam 5 registration contract.
func TestTransferRegistryKind(t *testing.T) {
	ResetKindsForTest()
	t.Cleanup(ResetKindsForTest)
	RegisterKind(TransferKind)
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate kind registration must panic")
		}
	}()
	RegisterKind(TransferKind)
}

// TestTransferGatedTaskSurface guards the handler's 503 when no registry is
// wired (a build without the task table must not pretend to move anything).
func TestTransferGatedTaskSurface(t *testing.T) {
	s := testService()
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "acme", "A", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	seedRepo(t, s, "acme", "r1")
	if _, err := s.PutAccess(ctx, "acme", "r1", "", VisibilityPrivate,
		[]AccessBinding{{Subject: "user:alice@example.com", Role: RoleAdmin}}); err != nil {
		t.Fatal(err)
	}
	w := doReq(testHandler(s, alice), "POST", "/acme/r1/api/transfer", `{"owner":"alice@example.com"}`)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "task registry") {
		t.Fatalf("no registry = %d (%s), want 503 naming the registry", w.Code, w.Body.String())
	}
}

// TestTransferResidueAfterMove: a source key that the destination does not
// reproduce is reported, never deleted (the concurrent-push class).
func TestTransferResidueAfterMove(t *testing.T) {
	f := newTransferFix(t)
	svc := f.svc
	ctx := f.ctx
	// Do a completed move, then plant a source key with no destination twin.
	if err := svc.TransferRepo(ctx, "acme", "r1", "beta", "r1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutBytes(ctx, svc.Store, "repos/acme/r1/wal/newpack.pack", []byte("late"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	err := svc.TransferRepo(ctx, "acme", "r1", "beta", "r1")
	if err == nil || !strings.Contains(err.Error(), "not reproduced") {
		t.Fatalf("residue = %v, want a named non-reproduction failure", err)
	}
	if ok, _ := store.Exists(ctx, svc.Store, "repos/acme/r1/wal/newpack.pack"); !ok {
		t.Fatal("unreproduced residue must never be deleted")
	}
}

// TestTransferManifestRewrite pins the load-bearing field: a moved manifest
// names the destination (wal derives the serving copy from it) and keeps every
// other field.
func TestTransferManifestRewrite(t *testing.T) {
	s := testService()
	raw := (&proto.Manifest{FormatVersion: 1, Repo: "acme/app", ObjectFormat: "sha1",
		HeadSeq: 4, Revision: 2, Writer: "w1"}).Marshal()
	out := s.rewriteTransferManifest(raw, "beta/app")
	m, err := proto.UnmarshalManifest(out)
	if err != nil {
		t.Fatal(err)
	}
	if m.Repo != "beta/app" || m.HeadSeq != 4 || m.Revision != 2 || m.Writer != "w1" || m.ObjectFormat != "sha1" {
		t.Fatalf("rewritten manifest = %+v", m)
	}
	if s.rewriteTransferManifest([]byte("{not-a-manifest"), "beta/app") != nil {
		t.Fatal("undecodable manifest must travel opaquely (nil rewrite)")
	}
	if got := s.rewriteTransferManifest(raw, "acme/app"); string(got) != string(raw) {
		t.Fatal("an already-correct manifest must pass through unchanged")
	}
}

// TestTransferResumeUserDestination: a crash after the move leaves the moved
// access.json at a USER destination; the retry must adopt it instead of writing
// the synthesized default over it (the resume path has no source to probe).
func TestTransferResumeUserDestination(t *testing.T) {
	f := newTransferFix(t)
	svc := f.svc
	ctx := f.ctx
	fk := newFakeForks()
	fk.index["beta/r1"] = []string{"carol/kid"}
	fk.points["carol/kid"] = [2]string{"acme/r1", "acme/r1"}
	fk.failParentMoved = errors.New("fork index store down")
	svc.Forks = fk
	// A user destination: alice's namespace.
	if err := svc.TransferRepo(ctx, "acme", "r1", "alice@example.com", "r1"); err == nil {
		t.Fatal("the injected family failure must fail the move")
	}
	doc, _, err := svc.GetAccess(ctx, "alice@example.com", "r1")
	if err != nil || doc == nil {
		t.Fatalf("moved access unreadable: %v", err)
	}
	wantVersion := doc.Version
	fk.failParentMoved = nil
	if err := svc.TransferRepo(ctx, "acme", "r1", "alice@example.com", "r1"); err != nil {
		t.Fatalf("resume at a user destination: %v", err)
	}
	doc, _, err = svc.GetAccess(ctx, "alice@example.com", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Version != wantVersion {
		t.Fatalf("resume rewrote the moved access.json (version %d -> %d)", wantVersion, doc.Version)
	}
	role, _ := svc.Resolve(ctx, "alice@example.com", "r1", authPrincipal("alice@example.com"))
	if role != RoleAdmin {
		t.Fatalf("destination owner must hold admin, got %q", role)
	}
}
