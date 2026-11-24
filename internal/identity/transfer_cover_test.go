// transfer_cover_test.go — the transfer's failure-path branches (PLAN-V2 §T.1):
// the adopt comparison, the store seams, the verification misses, the drained
// start, and the joiner's cancelled wait. The happy paths live in
// transfer_task_test.go.
package identity

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/sizecatalog"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/store/proto"
	"github.com/crueber/spindle/internal/wal"
)

// lyingCatalogStore accepts the catalog PUT but never persists it (the CAS
// "succeeded" on a store whose visibility lags — the verification must catch
// the missing row instead of trusting the write).
type lyingCatalogStore struct {
	store.ObjectStore
}

func (l *lyingCatalogStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if key == sizecatalog.CatalogKey {
		return store.ObjectMeta{Key: key, Version: "lying"}, nil
	}
	return l.ObjectStore.Put(ctx, key, body, opts)
}

// nbStore wraps Get so one exact key answers a non-Object GetResult.
type nbStore struct {
	store.ObjectStore
	key string
}

func (n *nbStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if key == n.key {
		return store.NotModified{Version: "v"}, nil
	}
	return n.ObjectStore.Get(ctx, key, opts)
}

// deadRunner is a TaskRunner whose Run refuses (the drained-table shape).
type deadRunner struct{}

func (deadRunner) Run(context.Context, string, string, map[string]string, func(context.Context, *wal.Task) error) (*wal.TaskRecord, error) {
	return nil, errors.New("503 interrupted: instance shut down")
}

func (deadRunner) Running(string, string) *wal.TaskRecord { return nil }

func TestTransferHelpers(t *testing.T) {
	s := testService()
	ctx := context.Background()
	// objectsEqual: absent right-hand side, size mismatch, and a non-Object
	// GetResult all answer (false, nil) or an error without panicking.
	put := func(key, body string) {
		if _, err := store.PutBytes(ctx, s.Store, key, []byte(body), store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
	}
	put("t/a", "hello")
	put("t/b", "hello")
	put("t/c", "hello!")
	put("t/d", "hell")
	if ok, err := s.objectsEqual(ctx, "t/a", "t/b"); err != nil || !ok {
		t.Fatalf("equal bodies = %v, %v", ok, err)
	}
	if ok, err := s.objectsEqual(ctx, "t/a", "t/missing"); err != nil || ok {
		t.Fatalf("absent = %v, %v", ok, err)
	}
	if ok, err := s.objectsEqual(ctx, "t/a", "t/c"); err != nil || ok {
		t.Fatalf("same size, different bytes = %v, %v", ok, err)
	}
	if ok, err := s.objectsEqual(ctx, "t/a", "t/d"); err != nil || ok {
		t.Fatalf("size mismatch = %v, %v", ok, err)
	}
	s.Store = &nbStore{ObjectStore: s.Store, key: "t/a"}
	if _, err := s.objectsEqual(ctx, "t/a", "t/b"); err == nil {
		t.Fatal("non-Object GetResult must error")
	}
	s.Store = &errStore{ObjectStore: testService().Store, getErr: errBoom}
	if _, err := s.objectsEqual(ctx, "x", "y"); !errors.Is(err, errBoom) {
		t.Fatalf("get failure = %v", err)
	}

	// copyObject: non-Object source and a Get failure.
	fresh := testService()
	if _, err := store.PutBytes(ctx, fresh.Store, "t/src", []byte("x"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	fresh.Store = &nbStore{ObjectStore: fresh.Store, key: "t/src"}
	if err := fresh.copyObject(ctx, "t/src", "t/dst"); err == nil {
		t.Fatal("copyObject must reject a non-Object GetResult")
	}

	// readOptional: absent is (nil,false,nil); a real error surfaces.
	plain := testService()
	if body, ok, err := plain.readOptional(ctx, "t/missing"); body != nil || ok || err != nil {
		t.Fatalf("absent readOptional = %v %v %v", body, ok, err)
	}
	s.Store = &errStore{ObjectStore: testService().Store, getErr: errBoom}
	if _, _, err := s.readOptional(ctx, "t/x"); !errors.Is(err, errBoom) {
		t.Fatalf("readOptional error = %v", err)
	}

	// listPrefix: a LIST failure surfaces.
	s = testService()
	s.Store = &errStore{ObjectStore: testService().Store, listErr: errBoom}
	if _, err := s.listPrefix(ctx, "repos/"); !errors.Is(err, errBoom) {
		t.Fatalf("listPrefix error = %v", err)
	}

	// splitTransferID: strict owner/name.
	for _, tc := range []struct {
		in string
		ok bool
	}{{"a/b", true}, {"a/", false}, {"/b", false}, {"a", false}, {"", false}} {
		if _, _, ok := splitTransferID(tc.in); ok != tc.ok {
			t.Errorf("splitTransferID(%q) ok = %v, want %v", tc.in, ok, tc.ok)
		}
	}

	// rewriteTransferManifest: empty input and an already-correct id.
	if got := s.rewriteTransferManifest(nil, "a/b"); got != nil {
		t.Fatalf("nil manifest rewrite = %v", got)
	}

	// quiesce tolerates a failing serving seam (narrated, never fatal).
	f := newTransferFix(t)
	f.svc.Serving = &fakeServing{failInvalid: errors.New("cache dir busy")}
	if err := f.svc.TransferRepo(f.ctx, "acme", "r1", "beta", "r1"); err != nil {
		t.Fatalf("a cache-dir flush failure must not fail the move: %v", err)
	}
}

func TestTransferVerificationMisses(t *testing.T) {
	t.Run("catalog row missing after a lying write", func(t *testing.T) {
		f := newTransferFix(t)
		seedCatalogRow(t, f.svc, "acme/r1", 7)
		f.svc.Store = &lyingCatalogStore{ObjectStore: f.svc.Store}
		err := f.svc.TransferRepo(f.ctx, "acme", "r1", "beta", "r1")
		if err == nil || !strings.Contains(err.Error(), "catalog still names") {
			t.Fatalf("want a named catalog failure, got %v", err)
		}
	})
	t.Run("source manifest left behind", func(t *testing.T) {
		f := newTransferFix(t)
		// A no-delete store leaves the source manifest: verification must name it.
		f.svc.Store = &noDeleteStore{ObjectStore: f.svc.Store}
		err := f.svc.TransferRepo(f.ctx, "acme", "r1", "beta", "r1")
		if err == nil || !strings.Contains(err.Error(), "changed during transfer") {
			t.Fatalf("want the leftover report, got %v", err)
		}
	})
	t.Run("destination manifest undecodable", func(t *testing.T) {
		f := newTransferFix(t)
		// A corrupt manifest at the destination is adopted only if equal; seed
		// one equal to the source's bytes but corrupt, so the copy adopts it.
		srcRaw, _, _ := store.GetBytes(f.ctx, f.svc.Store, "repos/acme/r1/"+manifestName, store.GetOptions{})
		bad := append([]byte(nil), srcRaw...)
		bad[len(bad)-1] ^= 0xff
		m, _ := proto.UnmarshalManifest(srcRaw)
		m.Repo = "beta/r1"
		// Rewrite-decodable source whose destination twin differs → conflict.
		if _, err := store.PutBytes(f.ctx, f.svc.Store, "repos/beta/r1/"+manifestName, bad,
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		if err := f.svc.TransferRepo(f.ctx, "acme", "r1", "beta", "r1"); err == nil {
			t.Fatal("a foreign destination manifest must conflict")
		}
	})
}

func TestTransferDrainedStart(t *testing.T) {
	f := newTransferFix(t)
	f.svc.Tasks = deadRunner{}
	rec, joined, err := f.svc.BeginTransfer(f.ctx, TransferRequest{SrcOwner: "acme", SrcRepo: "r1", DstOwner: "beta", DstRepo: "r1"})
	if err != nil {
		t.Fatalf("a drained start must answer a terminal record, not an error: %v", err)
	}
	if joined || rec == nil || rec.OK == nil || *rec.OK {
		t.Fatalf("drained record = %+v", rec)
	}
	if !strings.Contains(rec.Summary, "draining") {
		t.Fatalf("drained summary = %q", rec.Summary)
	}
}

func TestTransferJoinWaitCancelled(t *testing.T) {
	f := newTransferFix(t)
	bs := &blockingStore{ObjectStore: f.svc.Store, armed: true,
		started: make(chan struct{}), release: make(chan struct{})}
	f.svc.Store = bs
	if _, _, err := f.svc.BeginTransfer(f.ctx, TransferRequest{SrcOwner: "acme", SrcRepo: "r1", DstOwner: "beta", DstRepo: "r1"}); err != nil {
		t.Fatalf("first start: %v", err)
	}
	<-bs.started
	// A join whose caller ctx is already dead still answers the published
	// record (deterministic — a ready record always wins the two-arm select).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec, joined, err := f.svc.BeginTransfer(ctx, TransferRequest{SrcOwner: "acme", SrcRepo: "r1", DstOwner: "beta", DstRepo: "r1"})
	if err != nil || !joined || rec == nil {
		t.Fatalf("dead-ctx join = %+v, %v, %v; want the running record", rec, joined, err)
	}
	close(bs.release)
}

func TestTransferTaskLoadsRealRegistry(t *testing.T) {
	// The production seams wired together: the real task table, the real
	// serving-copy registry as the Serving seam (goroutine-free: the table is
	// driven directly, so no registry background work runs).
	f := newTransferFix(t)
	if err := f.svc.TransferRepo(f.ctx, "acme", "r1", "beta", "r1"); err != nil {
		t.Fatalf("move behind the real task table: %v", err)
	}
	if rec := f.reg.Tasks().List("acme/r1"); len(rec) != 0 {
		t.Fatalf("TransferRepo must not leave a table record (it is the synchronous shape): %+v", rec)
	}
}

// --- direct phase tests (each phase's verdict, without a whole move) ---------

func transferStateFor(owner, repo, dstOwner, dstRepo string) *transferState {
	return &transferState{
		req:       TransferRequest{SrcOwner: owner, SrcRepo: repo, DstOwner: dstOwner, DstRepo: dstRepo},
		srcPrefix: store.RepoPrefix(owner, repo),
		dstPrefix: store.RepoPrefix(dstOwner, dstRepo),
	}
}

// TestTransferVerifyDoor calls the verification step directly for each miss:
// every arm must name the piece it is unhappy about.
func TestTransferVerifyDoor(t *testing.T) {
	ctx := context.Background()
	newSvc := func(t *testing.T) *Service {
		s := testService()
		seedRepo(t, s, "acme", "r1")
		return s
	}
	t.Run("destination manifest missing", func(t *testing.T) {
		s := newSvc(t)
		err := s.transferVerify(ctx, nopNarrator{}, transferStateFor("acme", "r1", "beta", "r1"))
		if err == nil || !strings.Contains(err.Error(), "destination manifest missing") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("destination manifest undecodable", func(t *testing.T) {
		s := newSvc(t)
		if _, err := store.PutBytes(ctx, s.Store, "repos/beta/r1/"+manifestName, []byte("{not a manifest"),
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		err := s.transferVerify(ctx, nopNarrator{}, transferStateFor("acme", "r1", "beta", "r1"))
		if err == nil || !strings.Contains(err.Error(), "does not decode") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("destination names another repo", func(t *testing.T) {
		s := newSvc(t)
		m := &proto.Manifest{FormatVersion: 1, Repo: "gamma/r1", ObjectFormat: "sha1", Revision: 1}
		if _, err := store.PutBytes(ctx, s.Store, "repos/beta/r1/"+manifestName, m.Marshal(),
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		err := s.transferVerify(ctx, nopNarrator{}, transferStateFor("acme", "r1", "beta", "r1"))
		if err == nil || !strings.Contains(err.Error(), "names repo") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("source manifest still there", func(t *testing.T) {
		s := newSvc(t)
		m := &proto.Manifest{FormatVersion: 1, Repo: "beta/r1", ObjectFormat: "sha1", Revision: 2}
		if _, err := store.PutBytes(ctx, s.Store, "repos/beta/r1/"+manifestName, m.Marshal(),
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		err := s.transferVerify(ctx, nopNarrator{}, transferStateFor("acme", "r1", "beta", "r1"))
		if err == nil || !strings.Contains(err.Error(), "source manifest still present") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("catalog row missing", func(t *testing.T) {
		s := newSvc(t)
		seedRepo(t, s, "beta", "r1") // destination manifest, but its repo field names beta/r1
		if err := s.Store.Delete(ctx, "repos/acme/r1/"+manifestName, ""); err != nil {
			t.Fatal(err)
		}
		st := transferStateFor("acme", "r1", "beta", "r1")
		st.catalogHadEntry = true
		err := s.transferVerify(ctx, nopNarrator{}, st)
		if err == nil || !strings.Contains(err.Error(), "catalog has no row") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("package object missing", func(t *testing.T) {
		s := newSvc(t)
		seedRepo(t, s, "beta", "r1")
		if err := s.Store.Delete(ctx, "repos/acme/r1/"+manifestName, ""); err != nil {
			t.Fatal(err)
		}
		st := transferStateFor("acme", "r1", "beta", "r1")
		st.packageKeys = []string{"packages/registry/tags/v1"}
		err := s.transferVerify(ctx, nopNarrator{}, st)
		if err == nil || !strings.Contains(err.Error(), "did not follow the move") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("fork pointers inconsistent", func(t *testing.T) {
		s := newSvc(t)
		seedRepo(t, s, "beta", "r1")
		if err := s.Store.Delete(ctx, "repos/acme/r1/"+manifestName, ""); err != nil {
			t.Fatal(err)
		}
		s.Forks = &fakeForks{failVerify: errors.New("stale row")}
		err := s.transferVerify(ctx, nopNarrator{}, transferStateFor("acme", "r1", "beta", "r1"))
		if err == nil || !strings.Contains(err.Error(), "fork pointers inconsistent") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("serving probe failure", func(t *testing.T) {
		s := newSvc(t)
		seedRepo(t, s, "beta", "r1")
		if err := s.Store.Delete(ctx, "repos/acme/r1/"+manifestName, ""); err != nil {
			t.Fatal(err)
		}
		s.Serving = &fakeServing{failVerify: errors.New("refs down")}
		err := s.transferVerify(ctx, nopNarrator{}, transferStateFor("acme", "r1", "beta", "r1"))
		if err == nil || !strings.Contains(err.Error(), "refs unreadable") {
			t.Fatalf("got %v", err)
		}
	})
}

// TestTransferResidueDirect covers the resume cleanup: reproduced residue is
// deleted, unreproduced residue is refused untouched, and a delete failure
// surfaces.
func TestTransferResidueDirect(t *testing.T) {
	ctx := context.Background()
	t.Run("reproduced", func(t *testing.T) {
		s := testService()
		if _, err := store.PutBytes(ctx, s.Store, "repos/acme/r1/wal/x.pack", []byte("x"), store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PutBytes(ctx, s.Store, "repos/beta/r1/wal/x.pack", []byte("x"), store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		st := transferStateFor("acme", "r1", "beta", "r1")
		if err := s.transferResidue(ctx, nopNarrator{}, st); err != nil {
			t.Fatalf("residue: %v", err)
		}
		if ok, _ := store.Exists(ctx, s.Store, "repos/acme/r1/wal/x.pack"); ok {
			t.Fatal("reproduced residue must be deleted")
		}
	})
	t.Run("not reproduced", func(t *testing.T) {
		s := testService()
		if _, err := store.PutBytes(ctx, s.Store, "repos/acme/r1/wal/y.pack", []byte("y"), store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		st := transferStateFor("acme", "r1", "beta", "r1")
		err := s.transferResidue(ctx, nopNarrator{}, st)
		if err == nil || !strings.Contains(err.Error(), "not reproduced") {
			t.Fatalf("got %v", err)
		}
		if ok, _ := store.Exists(ctx, s.Store, "repos/acme/r1/wal/y.pack"); !ok {
			t.Fatal("unreproduced residue must survive")
		}
	})
	t.Run("delete failure", func(t *testing.T) {
		base := store.NewMemory()
		if _, err := store.PutBytes(ctx, base, "repos/acme/r1/wal/z.pack", []byte("z"), store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PutBytes(ctx, base, "repos/beta/r1/wal/z.pack", []byte("z"), store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
		s := New(&errStore{ObjectStore: base, delErr: errBoom}, config.Defaults())
		s.Now = testClock
		if err := s.transferResidue(ctx, nopNarrator{}, transferStateFor("acme", "r1", "beta", "r1")); !errors.Is(err, errBoom) {
			t.Fatalf("delete failure = %v", err)
		}
	})
	t.Run("listing failure", func(t *testing.T) {
		s := New(&errStore{ObjectStore: store.NewMemory(), listErr: errBoom}, config.Defaults())
		s.Now = testClock
		if err := s.transferResidue(ctx, nopNarrator{}, transferStateFor("acme", "r1", "beta", "r1")); !errors.Is(err, errBoom) {
			t.Fatalf("list failure = %v", err)
		}
	})
}

// TestTransferResidueCheckDirect covers the leftover report's both arms.
func TestTransferResidueCheckDirect(t *testing.T) {
	ctx := context.Background()
	s := testService()
	st := transferStateFor("acme", "r1", "beta", "r1")
	if err := s.transferResidueCheck(ctx, st); err != nil {
		t.Fatalf("clean prefix = %v", err)
	}
	if _, err := store.PutBytes(ctx, s.Store, "repos/acme/r1/left", []byte("l"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if err := s.transferResidueCheck(ctx, st); !errors.Is(err, ErrConflict) {
		t.Fatalf("leftover = %v, want ErrConflict", err)
	}
	s.Store = &errStore{ObjectStore: store.NewMemory(), listErr: errBoom}
	if err := s.transferResidueCheck(ctx, st); !errors.Is(err, errBoom) {
		t.Fatalf("list failure = %v", err)
	}
}

// TestTransferMovePassVanished covers the "manifest vanished between the probe
// and the copy" class (the repo was deleted mid-flight).
func TestTransferMovePassVanished(t *testing.T) {
	ctx := context.Background()
	s := testService()
	st := transferStateFor("acme", "r1", "beta", "r1")
	if err := s.transferMovePass(ctx, nopNarrator{}, st); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty prefix = %v, want ErrNotFound", err)
	}
	// Keys but no manifest (a prefix being swept).
	if _, err := store.PutBytes(ctx, s.Store, "repos/acme/r1/wal/a.pack", []byte("a"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if err := s.transferMovePass(ctx, nopNarrator{}, st); !errors.Is(err, ErrNotFound) {
		t.Fatalf("manifestless prefix = %v, want ErrNotFound", err)
	}
}

// TestTransferCrossReferenceFailures covers each seam's error arm.
func TestTransferCrossReferenceFailures(t *testing.T) {
	ctx := context.Background()
	s := testService()
	seedRepo(t, s, "beta", "r1")
	st := transferStateFor("acme", "r1", "beta", "r1")

	s.Forks = &fakeForks{failParentOf: errBoom}
	if err := s.transferCrossReferences(ctx, nopNarrator{}, st); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("parentOf failure = %v", err)
	}
	s.Forks = &fakeForks{parent: map[string]string{"beta/r1": "bad-id"}}
	if err := s.transferCrossReferences(ctx, nopNarrator{}, st); err == nil || !strings.Contains(err.Error(), "not an owner/name") {
		t.Fatalf("bad parent id = %v", err)
	}
	s.Forks = &fakeForks{
		parent:         map[string]string{"beta/r1": "acme/up"},
		index:          map[string][]string{"acme/up": {"acme/r1"}},
		failChildMoved: errBoom,
	}
	if err := s.transferCrossReferences(ctx, nopNarrator{}, st); err == nil || !strings.Contains(err.Error(), "fork index of acme/up") {
		t.Fatalf("childMoved failure = %v", err)
	}
	s.Forks = &fakeForks{index: map[string][]string{"beta/r1": {"x/y"}}, failParentMoved: errBoom}
	if err := s.transferCrossReferences(ctx, nopNarrator{}, st); err == nil || !strings.Contains(err.Error(), "fork pointers of acme/r1") {
		t.Fatalf("parentMoved failure = %v", err)
	}
	// No seams at all (a build without pulls/packages): the catalog alone runs.
	s.Forks = nil
	s.Packages = nil
	if err := s.transferCrossReferences(ctx, nopNarrator{}, st); err != nil {
		t.Fatalf("catalog-only cross-references: %v", err)
	}
}

// TestTransferSlotHelpers covers the join plumbing's guard arms directly.
func TestTransferSlotHelpers(t *testing.T) {
	s := testService()
	slot := &transferSlot{ready: make(chan struct{})}
	rec := &wal.TaskRecord{ID: "t1"}
	s.setSlotRecord(slot, *rec)
	s.setSlotRecord(slot, wal.TaskRecord{ID: "t2"}) // second publish is ignored
	if slot.rec == nil || slot.rec.ID != "t1" {
		t.Fatalf("slot rec = %+v", slot.rec)
	}
	// A slot that never publishes answers the caller's ctx error.
	dead := &transferSlot{ready: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.awaitTransferSlot(ctx, dead); !errors.Is(err, context.Canceled) {
		t.Fatalf("dead slot = %v", err)
	}
	if errText(nil, "fallback") != "fallback" {
		t.Fatal("errText fallback")
	}
	if errText(errBoom, "fallback") != errBoom.Error() {
		t.Fatal("errText error")
	}
}

// TestDriveTransferWithoutRecord covers the drain refusal that never reaches a
// table record (Run answers nil) and the synthesized terminal it publishes.
func TestDriveTransferWithoutRecord(t *testing.T) {
	s := testService()
	s.Tasks = deadRunner{}
	req, err := (TransferRequest{SrcOwner: "acme", SrcRepo: "r1", DstOwner: "beta", DstRepo: "r1"}).normalize()
	if err != nil {
		t.Fatal(err)
	}
	slot := &transferSlot{ready: make(chan struct{})}
	s.driveTransfer(context.Background(), req.SourceID(), slot, req)
	if slot.rec == nil || slot.rec.OK == nil || *slot.rec.OK {
		t.Fatalf("synthesized record = %+v", slot.rec)
	}
}

// TestTransferBeginPreflightFailures covers the request-path probe errors.
func TestTransferBeginPreflightFailures(t *testing.T) {
	f := newTransferFix(t)
	f.svc.Store = &errStore{ObjectStore: f.svc.Store, getErr: errBoom}
	if _, _, err := f.svc.BeginTransfer(f.ctx, TransferRequest{SrcOwner: "acme", SrcRepo: "r1", DstOwner: "beta", DstRepo: "r1"}); !errors.Is(err, errBoom) {
		t.Fatalf("preflight read failure = %v", err)
	}
	// An undecodable source manifest makes the pair comparison carry raw bytes.
	s := testService()
	if _, err := store.PutBytes(context.Background(), s.Store, "repos/acme/r1/"+manifestName, []byte("{bad"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutBytes(context.Background(), s.Store, "repos/beta/r1/"+manifestName, []byte("{other"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	reg := wal.NewRegistry(context.Background(), s.Store, config.Defaults())
	t.Cleanup(reg.Close)
	s.Tasks = reg.Tasks()
	if _, _, err := s.BeginTransfer(context.Background(), TransferRequest{SrcOwner: "acme", SrcRepo: "r1", DstOwner: "beta", DstRepo: "r1"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("opaque manifests that differ = %v, want ErrConflict", err)
	}
}

// --- one-shot fault injection per store call ---------------------------------

// oneShotStore fails the first GET/HEAD/LIST/PUT/DELETE of one exact key with
// err, then behaves normally (so a later retry converges).
type oneShotStore struct {
	store.ObjectStore
	mu    sync.Mutex
	op    string // "get" | "head" | "list" | "put" | "delete"
	key   string
	err   error
	fired bool
}

func (o *oneShotStore) fail(op, key string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.op == op && o.key == key && !o.fired {
		o.fired = true
		return true
	}
	return false
}

func (o *oneShotStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if o.fail("get", key) {
		return nil, o.err
	}
	return o.ObjectStore.Get(ctx, key, opts)
}

func (o *oneShotStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	if o.fail("head", key) {
		return nil, o.err
	}
	return o.ObjectStore.Head(ctx, key)
}

func (o *oneShotStore) List(ctx context.Context, prefix, after string, fn func(store.ObjectMeta) error) error {
	if o.fail("list", prefix) {
		return o.err
	}
	return o.ObjectStore.List(ctx, prefix, after, fn)
}

func (o *oneShotStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if o.fail("put", key) {
		return store.ObjectMeta{}, o.err
	}
	return o.ObjectStore.Put(ctx, key, body, opts)
}

func (o *oneShotStore) Delete(ctx context.Context, key string, v store.Version) error {
	if o.fail("delete", key) {
		return o.err
	}
	return o.ObjectStore.Delete(ctx, key, v)
}

// nilBodyStore answers a nil body with no error for one key (the NotModified
// shape GetBytes hands back without If-None-Match).
type nilBodyStore struct {
	store.ObjectStore
	key string
}

func (n *nilBodyStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if key == n.key {
		return store.NotModified{Version: "v"}, nil
	}
	return n.ObjectStore.Get(ctx, key, opts)
}

// TestTransferStoreFaults drives every one-shot store fault through the move so
// each seam's error arm is exercised (the phases must surface, not swallow).
func TestTransferStoreFaults(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		op   string
		key  string
	}{
		{"destination manifest probe (run)", "get", "repos/beta/r1/" + manifestName},
		{"catalog snapshot", "get", sizecatalog.CatalogKey},
		{"package snapshot", "list", "repos/acme/r1/packages/registry/"},
		{"access probe", "head", "repos/acme/r1/access.json"},
		{"move pass list", "list", "repos/acme/r1/"},
		{"object copy", "get", "repos/acme/r1/packs/pack-1.pack"},
		{"object put", "put", "repos/beta/r1/packs/pack-1.pack"},
		{"manifest barrier", "get", "repos/acme/r1/" + manifestName},
		{"catalog rewrite", "put", sizecatalog.CatalogKey},
		{"source delete", "delete", "repos/acme/r1/" + manifestName},
		{"destination manifest in verify", "get", "repos/beta/r1/" + manifestName},
		{"source manifest in verify", "head", "repos/acme/r1/" + manifestName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTransferFix(t)
			seedCatalogRow(t, f.svc, "acme/r1", 5)
			seedPackages(t, f.svc, "acme", "r1")
			f.svc.Store = &oneShotStore{ObjectStore: f.svc.Store, op: tc.op, key: tc.key, err: errBoom}
			// Some faults land in the request-path preflight (BeginTransfer) and
			// some in the body; the synchronous shape covers both in one call.
			err := f.svc.TransferRepo(f.ctx, "acme", "r1", "beta", "r1")
			if err == nil {
				t.Fatalf("fault %s/%s was swallowed", tc.op, tc.key)
			}
		})
	}
	// The same faults on the task start path answer an error, never a record.
	f := newTransferFix(t)
	f.svc.Store = &oneShotStore{ObjectStore: f.svc.Store, op: "get", key: "repos/acme/r1/" + manifestName, err: errBoom}
	if _, _, err := f.svc.BeginTransfer(f.ctx, TransferRequest{SrcOwner: "acme", SrcRepo: "r1", DstOwner: "beta", DstRepo: "r1"}); !errors.Is(err, errBoom) {
		t.Fatalf("begin fault = %v", err)
	}
	_ = ctx
}

// TestTransferNilBodyProbe covers GetBytes' NotModified shape in readOptional.
func TestTransferNilBodyProbe(t *testing.T) {
	s := testService()
	s.Store = &nilBodyStore{ObjectStore: s.Store, key: "k"}
	body, ok, err := s.readOptional(context.Background(), "k")
	if body != nil || ok || err != nil {
		t.Fatalf("nil-body readOptional = %v %v %v", body, ok, err)
	}
}

// brokenBody is an object body that fails after the first read.
type brokenBody struct{ n int }

func (b *brokenBody) Read(p []byte) (int, error) {
	if b.n == 0 {
		b.n++
		if len(p) > 0 {
			p[0] = 'x'
		}
		return 1, nil
	}
	return 0, errBoom
}

func (b *brokenBody) Close() error { return nil }

// brokenBodyStore serves one key with a failing body.
type brokenBodyStore struct {
	store.ObjectStore
	key  string
	key2 string
}

func (b *brokenBodyStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if key == b.key || (b.key2 != "" && key == b.key2) {
		res, err := b.ObjectStore.Get(ctx, key, opts)
		if err != nil {
			return res, err
		}
		obj := res.(store.Object)
		return store.Object{Meta: obj.Meta, Body: &brokenBody{}}, nil
	}
	return b.ObjectStore.Get(ctx, key, opts)
}

// countingServing fails VerifyRepo from the Nth call on (the move verifies in
// transferVerify, then again after the serving flush).
type countingServing struct {
	fakeServing
	calls  int
	failAt int
}

func (c *countingServing) VerifyRepo(ctx context.Context, id string) error {
	c.calls++
	if c.failAt > 0 && c.calls >= c.failAt {
		return errBoom
	}
	return c.fakeServing.VerifyRepo(ctx, id)
}

func TestTransferLateFaults(t *testing.T) {
	ctx := context.Background()
	t.Run("comparison body error", func(t *testing.T) {
		s := testService()
		for _, k := range []string{"t/a", "t/b"} {
			if _, err := store.PutBytes(ctx, s.Store, k, []byte("payload"), store.PutOptions{Mode: store.PutCreate}); err != nil {
				t.Fatal(err)
			}
		}
		// Both sides error mid-stream (equal byte counts) → the read error
		// surfaces instead of a false "different" verdict.
		s.Store = &brokenBodyStore{ObjectStore: s.Store, key: "t/a", key2: "t/b"}
		if _, err := s.objectsEqual(ctx, "t/a", "t/b"); !errors.Is(err, errBoom) {
			t.Fatalf("body error = %v", err)
		}
	})
	t.Run("post-flush verify", func(t *testing.T) {
		f := newTransferFix(t)
		f.svc.Serving = &countingServing{failAt: 2}
		err := f.svc.TransferRepo(f.ctx, "acme", "r1", "beta", "r1")
		if err == nil || !strings.Contains(err.Error(), "destination unreadable after flush") {
			t.Fatalf("post-flush verify = %v", err)
		}
	})
	t.Run("resume access probe", func(t *testing.T) {
		f := newTransferFix(t)
		if err := f.svc.TransferRepo(f.ctx, "acme", "r1", "alice@example.com", "r1"); err != nil {
			t.Fatal(err)
		}
		f.svc.Store = &oneShotStore{ObjectStore: f.svc.Store, op: "head",
			key: "repos/alice@example.com/r1/access.json", err: errBoom}
		if err := f.svc.TransferRepo(f.ctx, "acme", "r1", "alice@example.com", "r1"); !errors.Is(err, errBoom) {
			t.Fatalf("resume access probe = %v", err)
		}
	})
	t.Run("synthesized access write", func(t *testing.T) {
		f := newTransferFix(t)
		// No access.json at the source (acme/r2), a user destination, and a
		// failing write of the synthesized default.
		seedRepo(t, f.svc, "acme", "r2")
		f.svc.Store = &oneShotStore{ObjectStore: f.svc.Store, op: "put",
			key: "repos/alice@example.com/r2/access.json", err: errBoom}
		if err := f.svc.TransferRepo(f.ctx, "acme", "r2", "alice@example.com", "r2"); !errors.Is(err, errBoom) {
			t.Fatalf("synthesized access write = %v", err)
		}
	})
}
