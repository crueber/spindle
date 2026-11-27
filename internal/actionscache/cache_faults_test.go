package actionscache

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// The failure branches: a store that fails, a CAS that never wins, a payload
// whose address disagrees with its bytes. Every one must be a named error, never
// a silent success.

// faultStore wraps a real store with injectable failures.
type faultStore struct {
	store.ObjectStore
	getErr    error
	putErr    error
	headErr   error
	listErr   error
	deleteErr error
	// alwaysStale makes every update look like a lost CAS.
	alwaysStale bool
}

func (f *faultStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.ObjectStore.Get(ctx, key, opts)
}

func (f *faultStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if f.putErr != nil {
		return store.ObjectMeta{}, f.putErr
	}
	if f.alwaysStale {
		return store.ObjectMeta{}, &store.StoreError{Kind: store.ErrKindPreconditionFailed, Key: key}
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func (f *faultStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	if f.headErr != nil {
		return nil, f.headErr
	}
	return f.ObjectStore.Head(ctx, key)
}

func (f *faultStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	if f.listErr != nil {
		return f.listErr
	}
	return f.ObjectStore.List(ctx, prefix, startAfter, fn)
}

func (f *faultStore) Delete(ctx context.Context, key string, ifVersion store.Version) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.ObjectStore.Delete(ctx, key, ifVersion)
}

func faultEnv(t *testing.T) (*env, *faultStore) {
	e := newEnv(t)
	f := &faultStore{ObjectStore: e.store}
	e.eng.Store = f
	return e, f
}

var errBoom = errors.New("boom")

func TestStoreFailuresAreNamed(t *testing.T) {
	e, f := faultEnv(t)
	ctx := context.Background()
	scope := Scope{Kind: KindDefault}

	if err := e.eng.PutBlob(ctx, "o", "r", shaOf("x"), 1, strings.NewReader("x")); err != nil {
		t.Fatalf("seed payload: %v", err)
	}
	// Every read path surfaces the store error.
	f.getErr = errBoom
	if _, err := e.eng.Lookup(ctx, "o", "r", []Scope{scope}, "k", "", nil); !errors.Is(err, errBoom) {
		t.Fatalf("Lookup get error = %v", err)
	}
	if _, err := e.eng.List(ctx, "o", "r"); !errors.Is(err, errBoom) {
		t.Fatalf("List get error = %v", err)
	}
	if _, err := e.eng.Delete(ctx, "o", "r", "id"); !errors.Is(err, errBoom) {
		t.Fatalf("Delete get error = %v", err)
	}
	if _, err := e.eng.DiscardScope(ctx, "o", "r", scope); !errors.Is(err, errBoom) {
		t.Fatalf("DiscardScope get error = %v", err)
	}
	if _, err := e.eng.Save(ctx, "o", "r", scope, SaveInput{Key: "k", Version: "v", SHA256: shaOf("x"), Size: 1}); !errors.Is(err, errBoom) {
		t.Fatalf("Save get error = %v", err)
	}
	if _, err := e.eng.Reclaim(ctx, "o", "r", true); !errors.Is(err, errBoom) {
		t.Fatalf("Reclaim get error = %v", err)
	}
	if _, _, err := e.eng.OpenBlob(ctx, "o", "r", shaOf("x")); !errors.Is(err, errBoom) {
		t.Fatalf("OpenBlob get error = %v", err)
	}
	f.getErr = nil

	// HEAD failure.
	f.headErr = errBoom
	if _, err := e.eng.HasBlob(ctx, "o", "r", shaOf("x"), 1); !errors.Is(err, errBoom) {
		t.Fatalf("HasBlob head error = %v", err)
	}
	f.headErr = nil

	// Put failure (the payload).
	f.putErr = errBoom
	if err := e.eng.PutBlob(ctx, "o", "r", shaOf("x"), 1, strings.NewReader("x")); !errors.Is(err, errBoom) {
		t.Fatalf("PutBlob put error = %v", err)
	}
}

func TestIndexWriteFailureIsNamed(t *testing.T) {
	e, f := faultEnv(t)
	ctx := context.Background()
	scope := Scope{Kind: KindDefault}
	sha, size := e.put("seed")
	f.putErr = errBoom
	if _, err := e.eng.Save(ctx, "o", "r", scope, SaveInput{Key: "k", Version: "v", SHA256: sha, Size: size}); !errors.Is(err, errBoom) {
		t.Fatalf("index write failure = %v", err)
	}
	f.putErr = nil
	// A committed entry, then a failing index write on delete/discard.
	if _, err := e.eng.Save(ctx, "o", "r", scope, SaveInput{Key: "k", Version: "v", SHA256: sha, Size: size}); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	entries, _ := e.eng.List(ctx, "o", "r")
	f.putErr = errBoom
	if _, err := e.eng.Delete(ctx, "o", "r", entries[0].ID); !errors.Is(err, errBoom) {
		t.Fatalf("delete index write failure = %v", err)
	}
	if _, err := e.eng.DiscardScope(ctx, "o", "r", scope); !errors.Is(err, errBoom) {
		t.Fatalf("discard index write failure = %v", err)
	}
	f.putErr = nil
}

func TestCASStarvationAndStaleWrites(t *testing.T) {
	e, f := faultEnv(t)
	ctx := context.Background()
	scope := Scope{Kind: KindDefault}
	sha, size := e.put("body")
	save := SaveInput{Key: "k", Version: "v", SHA256: sha, Size: size}

	f.alwaysStale = true
	if _, err := e.eng.Save(ctx, "o", "r", scope, save); !errors.Is(err, ErrInvalid) {
		t.Fatalf("starved save = %v", err)
	}
	if _, err := e.eng.Delete(ctx, "o", "r", "id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete starved read = %v", err)
	}
	if freed, err := e.eng.DiscardScope(ctx, "o", "r", scope); err != nil || freed != 0 {
		t.Fatalf("discard of an empty scope = %d %v", freed, err)
	}
	f.alwaysStale = false

	// A committed entry then a starving store: delete/discard take the CAS
	// path and report the retry exhaustion instead of claiming success.
	if _, err := e.eng.Save(ctx, "o", "r", scope, save); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	entries, _ := e.eng.List(ctx, "o", "r")
	f.alwaysStale = true
	if _, err := e.eng.Delete(ctx, "o", "r", entries[0].ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("starved delete = %v", err)
	}
	if _, err := e.eng.DiscardScope(ctx, "o", "r", scope); !errors.Is(err, ErrInvalid) {
		t.Fatalf("starved discard = %v", err)
	}
	f.alwaysStale = false

	// Marking the index as absent switches the CAS writes to Create.
	if err := e.store.Delete(ctx, IndexKey("o", "r"), ""); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if _, err := e.eng.Save(ctx, "o", "r", scope, save); err != nil {
		t.Fatalf("create-path save: %v", err)
	}
}

func TestReclaimFaultsAndDryBudget(t *testing.T) {
	e, f := faultEnv(t)
	ctx := context.Background()
	e.eng.Retention = 1
	e.save(Scope{Kind: KindDefault}, "a", "v", "aaa")
	e.now = e.now.Add(48 * time.Hour)

	// A failing payload LIST fails the sweep (never a silent partial success).
	f.listErr = errBoom
	if _, err := e.eng.Reclaim(ctx, "o", "r", true); !errors.Is(err, errBoom) {
		t.Fatalf("reclaim list error = %v", err)
	}
	f.listErr = nil
	// A payload delete that fails is best effort (the index is already the
	// truth; the orphan is retried by the next sweep), so the pass still
	// reports its accounting.
	f.deleteErr = errBoom
	if res, err := e.eng.Reclaim(ctx, "o", "r", false); err != nil || res.EntriesDeleted != 1 {
		t.Fatalf("dry reclaim with a failing delete = %+v %v", res, err)
	}
	f.deleteErr = nil
	if _, err := e.eng.Reclaim(ctx, "o", "r", true); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	// Everything is gone; the second pass has no work.
	if res, err := e.eng.Reclaim(ctx, "o", "r", true); err != nil || res.EntriesDeleted != 0 {
		t.Fatalf("idle reclaim = %+v %v", res, err)
	}
}

func TestPutBlobSizeAndDedupeFaults(t *testing.T) {
	e, f := faultEnv(t)
	ctx := context.Background()
	if err := e.eng.PutBlob(ctx, "o", "r", shaOf("x"), -1, strings.NewReader("")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative size = %v", err)
	}
	// An existing address with a different stored size is corruption, not a
	// dedupe.
	sha := shaOf("abc")
	if err := e.eng.PutBlob(ctx, "o", "r", sha, 3, strings.NewReader("abc")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := e.eng.PutBlob(ctx, "o", "r", sha, 4, strings.NewReader("abcd")); err == nil {
		t.Fatal("a size disagreement at one address must be refused")
	}
	// A 412 whose HEAD cannot be read is that error.
	f.headErr = errBoom
	if err := e.eng.PutBlob(ctx, "o", "r", sha, 3, strings.NewReader("abc")); !errors.Is(err, errBoom) {
		t.Fatalf("412 head error = %v", err)
	}
	f.headErr = nil
	// A 412 whose HEAD disagrees is ErrCorrupt.
	if err := e.eng.PutBlob(ctx, "o", "r", sha, 4, strings.NewReader("abcd")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("412 size disagreement = %v", err)
	}
	// The engine's cap is a named error even before the store is touched.
	e.eng.MaxEntryBytes = 2
	if err := e.eng.PutBlob(ctx, "o", "r", shaOf("long"), 8, strings.NewReader("longlong")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over-cap put = %v", err)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		1 << 30:   "1 GiB",
		512 << 20: "512.0 MiB",
		2 << 20:   "2.0 MiB",
		1024:      "1.0 KiB",
		3:         "3 B",
		3 << 30:   "3 GiB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
