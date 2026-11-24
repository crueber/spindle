// cover3_test.go — the determinism pass behind the ≥95% per-package gate.
//
// Two mechanisms used to make this package's coverage vary run to run:
//
//  1. a select whose two arms are BOTH ready (a free fan-out slot and an
//     already-canceled context) picks at random, so "canceled" tests
//     covered one arm or the other per run. acquireFanoutSlot now takes a
//     ready slot first and every caller re-checks ctx, making both arms
//     reachable by construction (below: the canceled pass, then the
//     saturated pass that can only observe cancellation while blocked);
//  2. TestEmitConcurrentSameTripleDedups reaches the dedup-lost index CAS
//     only when a loser happens to race past the hasUnread fast path. The
//     forced-interleaving test below pins that branch deterministically;
//     the race test stays as the concurrency proof.
//
// Every test here asserts owned state (returned values, task table rows,
// store contents, the package's keepalive counter) — none samples timing.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// --- doubles ------------------------------------------------------------------

// indexBlindStore hides one key from its FIRST Get only: createOne's
// hasUnread pre-check misses the live row, then the index CAS (the second
// read) sees it. That is the exact interleaving the #91 dedup race allows,
// forced here instead of sampled.
type indexBlindStore struct {
	store.ObjectStore
	key   string
	first sync.Once
}

func (s *indexBlindStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if key == s.key {
		hidden := false
		s.first.Do(func() { hidden = true })
		if hidden {
			return nil, store.NewNotFound(key)
		}
	}
	return s.ObjectStore.Get(ctx, key, opts)
}

// keyGateStore parks every Get whose key matches on the gate (worker
// entry), letting a test observe exactly which fan-out workers launched.
type keyGateStore struct {
	store.ObjectStore
	g     *arrivalGate
	match func(key string) bool
}

func (s *keyGateStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if s.match(key) {
		s.g.arrive()
	}
	return s.ObjectStore.Get(ctx, key, opts)
}

// firstCreate412Store refuses the FIRST Create whose key matches and
// delegates afterwards (a ULID collision on the hook config write).
type firstCreate412Store struct {
	store.ObjectStore
	mu      sync.Mutex
	armed   bool
	match   func(key string) bool
	refused []string
}

func (s *firstCreate412Store) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if opts.Mode == store.PutCreate && s.match(key) {
		s.mu.Lock()
		refuse := s.armed
		if refuse {
			s.armed = false
			s.refused = append(s.refused, key)
		}
		s.mu.Unlock()
		if refuse {
			return store.ObjectMeta{}, store.NewPrecondition(key, "v0")
		}
	}
	return s.ObjectStore.Put(ctx, key, body, opts)
}

// plainWriter is an http.ResponseWriter with no Flush (SSE must 406).
type plainWriter struct {
	hdr  http.Header
	code int
	body strings.Builder
}

func (w *plainWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}

func (w *plainWriter) Write(p []byte) (int, error) { return w.body.Write(p) }

func (w *plainWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

// brokenWriter is a Flusher whose Write always fails: the stream opened
// and every packet/keepalive write is a transport error.
type brokenWriter struct {
	hdr    http.Header
	writes int
}

func (w *brokenWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}

func (w *brokenWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, errors.New("write boom")
}

func (w *brokenWriter) WriteHeader(int) {}
func (w *brokenWriter) Flush()          {}

// runWithDeadline waits for fn to return and fails the test instead of
// hanging the package (a blocked handler is a bug, not a slow test).
func runWithDeadline(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	waitDone(t, done, what)
}

// --- 1. the dedup-lost index CAS (emit.go createOne) ---------------------------

// TestCreateOneDedupLostRaceIsSkipped forces the operation order a lost
// dedup race produces: the pre-check misses the live row, this emission's
// object is written, and the index CAS then refuses it because a live row
// for the same (user, thread, reason) committed first. A fresh Create is
// an orphan (deleted); a retried one (412, fresh=false) is not.
func TestCreateOneDedupLostRaceIsSkipped(t *testing.T) {
	const (
		who = "carol@example.com"
		at  = "2026-09-04T12:00:00Z"
	)
	live := IndexEntry{ID: "live-row@other", Repo: "acme/repo", Num: 7,
		Kind: "issue", Reason: ReasonSubscribed, State: StateUnread, At: at}
	cases := []struct {
		name       string
		retry      bool // seed this emission's own object → putCreate 412s
		wantObject bool // the orphan delete only fires when the Create was fresh
	}{
		{"fresh create loses the CAS and deletes its orphan", false, false},
		{"retried create (412) leaves the object it found", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := store.NewMemory()
			writeRaw(t, st, NotifIndexKey(who), mustEncode(t, IndexDoc{
				Version: 1, UnreadCount: 1, Entries: []IndexEntry{live},
			}))
			const seq = 100
			id := NotificationID(who, "acme/repo", 7, ReasonSubscribed, seq)
			if tc.retry {
				writeRaw(t, st, NotifKey(who, id), mustEncode(t, Notification{
					ID: id, Repo: "acme/repo", Num: 7, Kind: "issue",
					Reason: ReasonSubscribed, State: StateRead, CreatedAt: at,
				}))
			}
			svc := New(&indexBlindStore{ObjectStore: st, key: NotifIndexKey(who)}, nil)

			_, status := svc.createOne(ctx(),
				Emission{Repo: "acme/repo", Num: 7, Kind: "issue"},
				"Bug title", "bob@example.com", at, seq,
				target{principal: who, reason: ReasonSubscribed})
			if status != createSkipped {
				t.Fatalf("status = %d, want createSkipped (the live row covers the triple)", status)
			}

			raw, _, err := store.GetBytes(ctx(), st, NotifIndexKey(who), store.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var ix IndexDoc
			if err := json.Unmarshal(raw, &ix); err != nil {
				t.Fatal(err)
			}
			if len(ix.Entries) != 1 || ix.Entries[0].ID != live.ID || ix.UnreadCount != 1 {
				t.Fatalf("index after lost race = %+v, want only the live row (dedup count 1)", ix)
			}

			_, _, err = store.GetBytes(ctx(), st, NotifKey(who, id), store.GetOptions{})
			switch {
			case tc.wantObject && err != nil:
				t.Fatalf("retry object gone (%v), want it retained (nothing was created by this call)", err)
			case !tc.wantObject && !store.IsNotFound(err):
				t.Fatalf("orphan object %s still present (err=%v), want it deleted", NotifKey(who, id), err)
			}
		})
	}
}

// --- 2. fan-out cancellation: every arm reachable deterministically ------------

// TestCreateAllCancelWithFreeSlots pins the canceled-context branch that a
// free slot must take: the slot is granted, the worker observes the dead
// context and counts the target failed WITHOUT touching the store.
func TestCreateAllCancelWithFreeSlots(t *testing.T) {
	x := newHarness(t)
	targets := []target{
		{principal: "a@example.com", reason: ReasonSubscribed},
		{principal: "b@example.com", reason: ReasonSubscribed},
	}
	cctx, cancel := context.WithCancel(ctx())
	cancel()
	done, failed := x.svc.createAll(cctx, "acme", "repo",
		Emission{Repo: "acme/repo", Num: 7, Kind: "issue"}, "T", "actor",
		x.now.Format(dateTimeFmt), 1, targets)
	if !failed || len(done) != 0 {
		t.Fatalf("canceled createAll = %v/%d, want failed/0", failed, len(done))
	}
	for _, tg := range targets {
		if n := countNotifs(t, x, tg.principal); n != 0 {
			t.Fatalf("%s notifications = %d, want 0 (no store work under a dead context)", tg.principal, n)
		}
	}
}

// TestCreateAllCancelWhileSaturated pins the other arm: cancellation
// arriving while every slot is busy. FanoutParallel workers park inside
// their Create, the loop blocks on target FanoutParallel+1, and the cancel
// can only be observed on the slot wait — never by racing a spawn.
func TestCreateAllCancelWhileSaturated(t *testing.T) {
	x := newHarness(t)
	g := newArrivalGate()
	defer g.open()
	x.svc.Store = &gatePutStore{ObjectStore: x.svc.Store, g: g}

	targets := make([]target, 0, FanoutParallel+2)
	for i := range FanoutParallel + 2 {
		targets = append(targets, target{principal: principalN(i), reason: ReasonSubscribed})
	}
	cctx, cancel := context.WithCancel(ctx())
	var created []completed
	var failed bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		created, failed = x.svc.createAll(cctx, "acme", "repo",
			Emission{Repo: "acme/repo", Num: 7, Kind: "issue"}, "T", "actor",
			x.now.Format(dateTimeFmt), 1, targets)
	}()
	g.waitFor(t, FanoutParallel) // every slot busy ⇒ the loop is on the slot wait
	cancel()
	g.open()
	waitDone(t, done, "saturated createAll")

	if !failed {
		t.Fatal("targets dropped by cancellation must count failed")
	}
	if len(created) != FanoutParallel {
		t.Fatalf("created = %d, want the %d in-flight workers only", len(created), FanoutParallel)
	}
}

// TestFanoutOneCancelWhileSaturated is the drain-path twin: the budget
// expires while all slots are parked, so the remaining recipients are
// counted failed and the seq is reported incomplete (the redrain re-drives
// it) instead of silently dropped.
func TestFanoutOneCancelWhileSaturated(t *testing.T) {
	at := "2026-09-04T12:00:00Z"
	recips := make([]activityRecipient, 0, FanoutParallel+2)
	for i := range FanoutParallel + 2 {
		recips = append(recips, activityRecipient{Principal: principalN(i), Reason: ReasonSubscribed})
	}
	mem := store.NewMemory()
	writeRaw(t, mem, CollabStateKey("acme", "repo"), mustEncode(t, CollabState{NextSeq: 2}))
	seedActivity(t, mem, at, "acme", "repo", 1, true, recips...)

	g := newArrivalGate()
	defer g.open()
	svc := New(&gatePutStore{ObjectStore: mem, g: g}, nil)
	svc.Now = func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }

	cctx, cancel := context.WithCancel(ctx())
	type outcome struct{ existed, complete bool }
	res := make(chan outcome, 1)
	go func() {
		e, c := svc.fanoutOne(cctx, "acme", "repo", "acme/repo", 1)
		res <- outcome{e, c}
	}()
	g.waitFor(t, FanoutParallel)
	cancel()
	g.open()

	var out outcome
	select {
	case out = <-res:
	case <-time.After(30 * time.Second):
		t.Fatal("fanoutOne did not return after the gate opened")
	}
	if !out.existed || out.complete {
		t.Fatalf("budget-canceled fanoutOne = (%v, %v), want (true, false)", out.existed, out.complete)
	}
	for i, r := range recips {
		_, _, err := store.GetBytes(ctx(), mem, NotifKey(r.Principal, NotificationID(r.Principal, "acme/repo", 7, r.Reason, 1)), store.GetOptions{})
		if i < FanoutParallel && err != nil {
			t.Fatalf("in-flight recipient %s lost its notification: %v", r.Principal, err)
		}
		if i >= FanoutParallel && !store.IsNotFound(err) {
			t.Fatalf("stranded recipient %s wrote a notification (err=%v), want none", r.Principal, err)
		}
	}
}

// TestHookPassesCancelMidLaunch covers the repo and org delivery passes:
// with more hooks than slots, the cancel is observed on the slot wait, the
// launch loop stops, and the already-launched hooks still finish (their
// failed POSTs keep cursors for the next pass). The hook that never
// launched has no deliveries ring at all.
func TestHookPassesCancelMidLaunch(t *testing.T) {
	cases := []struct {
		name    string
		create  func(t *testing.T, x *harness, url string) string
		deliver func(x *harness, ctx context.Context)
		ring    func(x *harness, id string) *DeliveriesDoc
	}{
		{
			name: "repo pass",
			create: func(t *testing.T, x *harness, url string) string {
				t.Helper()
				seedRepo(t, x, "acme", "repo")
				hk, err := x.svc.CreateHook(ctx(), "acme", "repo", "amy@example.com", HookSpec{URL: strPtr(url)})
				if err != nil {
					t.Fatal(err)
				}
				return hk.ID
			},
			deliver: func(x *harness, c context.Context) { x.svc.DeliverRepo(c, "acme", "repo") },
			ring:    func(x *harness, id string) *DeliveriesDoc { return x.svc.ReadDeliveries(ctx(), "acme", "repo", id) },
		},
		{
			name: "org pass",
			create: func(t *testing.T, x *harness, url string) string {
				t.Helper()
				hk, err := x.svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{URL: strPtr(url)})
				if err != nil {
					t.Fatal(err)
				}
				return hk.ID
			},
			deliver: func(x *harness, c context.Context) { x.svc.DeliverOrg(c, "acme") },
			ring:    func(x *harness, id string) *DeliveriesDoc { return x.svc.ReadOrgDeliveries(ctx(), "acme", id) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := newHarness(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
			defer srv.Close()

			ids := make([]string, 0, FanoutParallel+1)
			for range FanoutParallel + 1 {
				ids = append(ids, tc.create(t, x, srv.URL))
			}
			if tc.name == "repo pass" {
				writeRaw(t, x.svc.Store, CollabStateKey("acme", "repo"), mustEncode(t, CollabState{NextSeq: 2}))
				writeRaw(t, x.svc.Store, ActivityKey("acme", "repo", 1),
					mustEncode(t, ActivityEvent{Seq: 1, Repo: "acme/repo", Action: "commented", Kind: "issue",
						Title: "T", Actor: "bob@example.com", At: "2026-09-04T12:00:00Z"}))
			} else {
				x.svc.EmitOrgEvent(ctx(), "acme", OrgActionMemberAdded, "bob@example.com", "")
			}

			g := newArrivalGate()
			defer g.open()
			x.svc.Store = &keyGateStore{ObjectStore: x.svc.Store, g: g,
				match: func(key string) bool { return strings.Contains(key, "/cursors/") }}

			cctx, cancel := context.WithCancel(ctx())
			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.deliver(x, cctx)
			}()
			g.waitFor(t, FanoutParallel) // all slots busy ⇒ the loop is on the slot wait
			cancel()
			g.open()
			waitDone(t, done, "delivery pass")

			// The launch loop spawns in hook-key order, and ULID suffixes
			// sort arbitrarily within one millisecond, so count rows
			// instead of indexing by creation order: exactly the launched
			// hooks recorded an attempt, and one hook was never launched.
			launched, idle := 0, 0
			for _, id := range ids {
				ring := tc.ring(x, id)
				if ring != nil && len(ring.Entries) > 0 {
					launched++
					continue
				}
				idle++
			}
			if launched != FanoutParallel || idle != 1 {
				t.Fatalf("launched=%d idle=%d, want %d/%d (one hook must never launch)", launched, idle, FanoutParallel, 1)
			}
		})
	}
}

// TestDrainFanoutCancelsMidBacklog pins the phase-1 drain contract
// (issue #154): a cancel observed between seqs re-attaches the remainder
// (durable for the redrain sweep) and the loop force-ends the task as
// "draining" instead of running more store work. Deterministic: seq 1's
// fan-out is parked inside its Create, so the cancel can only be observed
// on the next seq's check.
func TestDrainFanoutCancelsMidBacklog(t *testing.T) {
	at := "2026-09-04T12:00:00Z"
	mem := store.NewMemory()
	writeRaw(t, mem, CollabStateKey("acme", "repo"), mustEncode(t, CollabState{NextSeq: 3}))
	for seq, who := range map[int]string{1: "amy@example.com", 2: "bob@example.com"} {
		seedActivity(t, mem, at, "acme", "repo", seq, true,
			activityRecipient{Principal: who, Reason: ReasonSubscribed})
	}
	g := newArrivalGate()
	defer g.open()
	svc := New(&gatePutStore{ObjectStore: mem, g: g}, nil)
	svc.Now = func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }

	e, joined := svc.tasks.begin("acme/repo", TaskKindFanout, svc.nowUTC())
	if joined {
		t.Fatal("fan-out task already running")
	}
	e.attach(1)
	e.attach(2)

	cctx, cancel := context.WithCancel(ctx())
	done := make(chan struct{})
	go func() { defer close(done); svc.drainFanout(cctx, "acme/repo", e) }()
	g.waitFor(t, 1) // seq 1 is in flight; seq 2 has not been probed
	cancel()
	g.open()
	waitDone(t, done, "drainFanout")

	rec := svc.TaskStatus("acme/repo", TaskKindFanout)
	if rec == nil || rec.Note != "draining" {
		t.Fatalf("drained record = %+v, want note %q (force-end, not quiescent end)", rec, "draining")
	}
	e.mu.Lock()
	tail := append([]int(nil), e.seqs...)
	e.mu.Unlock()
	if len(tail) != 1 || tail[0] != 2 {
		t.Fatalf("re-attached tail = %v, want [2] (the redrain re-drives it)", tail)
	}
	if n := countPrincipalNotifs(t, mem, "bob@example.com"); n != 0 {
		t.Fatalf("undrained seq delivered %d notifications, want 0", n)
	}
}

// principalN names the nth synthetic recipient.
func principalN(i int) string { return fmt.Sprintf("user%04d@example.com", i) }

// --- 3. the keepalive goroutine's write-failure exit ----------------------------

// TestSSEWriterKeepaliveExitsOnWriteFailure pins the second keepalive exit
// (#13): a failed keepalive comment stops the writer, so a dead client
// cannot pin the goroutine until the 10s ticker fires again. The interval
// is shortened via Ticker.Reset (safe with a concurrent receive) and the
// assertion polls the package's own counter.
func TestSSEWriterKeepaliveExitsOnWriteFailure(t *testing.T) {
	w := &brokenWriter{}
	s, ok := newSSEWriter(w, httptest.NewRequest("GET", "/api/v1/notifications/stream", nil))
	if !ok {
		t.Fatal("brokenWriter must flush")
	}
	awaitKeepalives(t, 1, "keepalive goroutine after attach")
	s.ka.Reset(time.Millisecond)
	awaitKeepalives(t, 0, "keepalive goroutine after its write failed")

	s.mu.Lock()
	ended := s.ended
	s.mu.Unlock()
	if !ended {
		t.Fatal("a failed keepalive must stop the writer, so packets cannot resume")
	}
}

// --- 4. stream guards ----------------------------------------------------------

// TestCollabStreamEdges covers the stream's drop rules without timing: a
// writer that cannot flush is refused (406), recent frames for another repo
// or of an unknown kind are skipped, and a failed packet write ends the
// handler instead of blocking on the live channel.
func TestCollabStreamEdges(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "acme", "repo")
	p := auth.Principal{Name: "amy@example.com"}

	t.Run("no flusher", func(t *testing.T) {
		w := &plainWriter{}
		r := httptest.NewRequest("GET", "/acme/repo/api/collab/stream", nil)
		x.handler.collabStream(w, r, "acme", "repo", p)
		if w.code != http.StatusNotAcceptable {
			t.Fatalf("status = %d, want 406 (streaming unsupported)", w.code)
		}
	})

	t.Run("recent ring filters", func(t *testing.T) {
		// Seed the replay ring out of order on purpose: another repo's
		// frame, an unknown kind, then a valid frame whose write fails.
		x.svc.rbus.mu.Lock()
		x.svc.rbus.ring["acme/repo"] = []RepoFrame{
			{Name: "issue", Repo: "other/repo", Num: 1},
			{Name: "not_a_collab_kind", Repo: "acme/repo", Num: 2},
			{Name: "issue", Repo: "acme/repo", Num: 3},
		}
		x.svc.rbus.mu.Unlock()

		w := &brokenWriter{}
		r := httptest.NewRequest("GET", "/acme/repo/api/collab/stream", nil)
		runWithDeadline(t, "collabStream", func() {
			x.handler.collabStream(w, r, "acme", "repo", p)
		})
		if w.writes == 0 {
			t.Fatal("handler returned without attempting the opener + frame writes")
		}
	})
}

// TestOrgActivityStreamEdges is the org twin: no flusher → 406, and a
// failed packet write ends the handler.
func TestOrgActivityStreamEdges(t *testing.T) {
	x := newHarness(t)
	p := auth.Principal{Name: "amy@example.com"}

	t.Run("no flusher", func(t *testing.T) {
		w := &plainWriter{}
		r := httptest.NewRequest("GET", "/api/v1/orgs/acme/activity/stream", nil)
		x.handler.orgActivityStream(w, r, "acme", p)
		if w.code != http.StatusNotAcceptable {
			t.Fatalf("status = %d, want 406 (streaming unsupported)", w.code)
		}
	})

	t.Run("failed packet write", func(t *testing.T) {
		x.svc.rbus.mu.Lock()
		x.svc.rbus.ring["acme"] = []RepoFrame{
			{Name: OrgActivityFrameKind, Repo: "acme", Seq: 1, Action: OrgActionMemberAdded},
		}
		x.svc.rbus.mu.Unlock()

		w := &brokenWriter{}
		r := httptest.NewRequest("GET", "/api/v1/orgs/acme/activity/stream", nil)
		runWithDeadline(t, "orgActivityStream", func() {
			x.handler.orgActivityStream(w, r, "acme", p)
		})
		if w.writes == 0 {
			t.Fatal("handler returned without attempting a write")
		}
	})
}

// --- 5. hook writes, lookups, and sweep pruning --------------------------------

// TestHookCreateRetriesULIDCollision: a 412 on the first config Create is
// randomness failure, not a conflict — both scopes retry once with fresh
// entropy and store the second id.
func TestHookCreateRetriesULIDCollision(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		key    func(id string) string
		create func(t *testing.T, st store.ObjectStore, svc *Service) (*Hook, error)
	}{
		{
			name:   "repo hook",
			prefix: WebhooksPrefix("acme", "repo"),
			key:    func(id string) string { return HookKey("acme", "repo", id) },
			create: func(t *testing.T, st store.ObjectStore, svc *Service) (*Hook, error) {
				t.Helper()
				writeRaw(t, st, manifestKey("acme", "repo"), []byte("manifest"))
				return svc.CreateHook(ctx(), "acme", "repo", "amy@example.com",
					HookSpec{URL: strPtr("https://hooks.example/x")})
			},
		},
		{
			name:   "org hook",
			prefix: OrgWebhooksPrefix("acme"),
			key:    func(id string) string { return OrgHookKey("acme", id) },
			create: func(t *testing.T, st store.ObjectStore, svc *Service) (*Hook, error) {
				t.Helper()
				return svc.CreateOrgHook(ctx(), "acme", "amy@example.com",
					HookSpec{URL: strPtr("https://hooks.example/x")})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := store.NewMemory()
			wrapper := &firstCreate412Store{ObjectStore: mem, armed: true,
				match: func(key string) bool {
					// Config writes only: cursors/ and deliveries/ live below.
					name := strings.TrimPrefix(key, tc.prefix)
					return name != key && name != "" && !strings.Contains(name, "/")
				}}
			svc := New(wrapper, nil)

			hk, err := tc.create(t, mem, svc)
			if err != nil {
				t.Fatalf("create after a collided id: %v", err)
			}
			if len(wrapper.refused) != 1 {
				t.Fatalf("refused writes = %d, want exactly the collided attempt", len(wrapper.refused))
			}
			if wrapper.refused[0] == tc.key(hk.ID) {
				t.Fatalf("retry reused the collided key %s", tc.key(hk.ID))
			}
			raw, _, err := store.GetBytes(ctx(), mem, tc.key(hk.ID), store.GetOptions{})
			if err != nil || len(raw) == 0 {
				t.Fatalf("retried config not stored at %s: %v", tc.key(hk.ID), err)
			}
		})
	}
}

// TestHookPassesSurfaceLookupErrors: a store failure in the hook LIST is
// reported by the create (never a silent no-hook config), and the delivery
// and sweep passes fail closed (no pass, no task) instead of delivering a
// partial window.
func TestHookPassesSurfaceLookupErrors(t *testing.T) {
	t.Run("create hook reports the list error", func(t *testing.T) {
		svc := New(failListStore{store.NewMemory()}, nil)
		if _, err := svc.CreateHook(ctx(), "acme", "repo", "amy@example.com",
			HookSpec{URL: strPtr("https://hooks.example/x")}); err == nil {
			t.Fatal("CreateHook hid the hook-list failure")
		}
	})
	t.Run("create org hook reports the list error", func(t *testing.T) {
		svc := New(failListStore{store.NewMemory()}, nil)
		if _, err := svc.CreateOrgHook(ctx(), "acme", "amy@example.com",
			HookSpec{URL: strPtr("https://hooks.example/x")}); err == nil {
			t.Fatal("CreateOrgHook hid the hook-list failure")
		}
	})
	t.Run("deliver and sweep passes fail closed", func(t *testing.T) {
		svc := New(failListStore{store.NewMemory()}, nil)
		svc.DeliverOrg(ctx(), "acme")
		if got := svc.TaskStatus(orgTaskRepo("acme"), TaskKindOrgWebhooks); got != nil {
			t.Fatalf("failed DeliverOrg registered a task: %+v", got)
		}
		svc.sweepOrg(ctx(), "acme")
		if got := svc.TaskStatus(orgTaskRepo("acme"), TaskKindOrgWebhooks); got != nil {
			t.Fatalf("failed sweepOrg registered a task: %+v", got)
		}
	})
	t.Run("delete reports the store error", func(t *testing.T) {
		mem := store.NewMemory()
		svc := New(mem, nil)
		hk, err := svc.CreateOrgHook(ctx(), "acme", "amy@example.com", HookSpec{URL: strPtr("https://hooks.example/x")})
		if err != nil {
			t.Fatal(err)
		}
		broken := New(failDeleteStore{mem}, nil)
		if err := broken.DeleteOrgHook(ctx(), "acme", hk.ID); err == nil {
			t.Fatal("DeleteOrgHook hid the delete failure")
		}
	})
}

// TestSweepsPruneWatermarksForVanishedScopes: the sweeps drop watermarks
// for repos/orgs the enumeration no longer sees (a deleted repo or org must
// not pin a map entry forever) while keeping the entries they did observe.
func TestSweepsPruneWatermarksForVanishedScopes(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "acme", "live")

	x.svc.hookMu.Lock()
	x.svc.hookSeen["gone/repo"] = 9
	x.svc.hookPending["gone/repo"] = true
	x.svc.hookSeen["acme/live"] = 1
	x.svc.orgHookSeen["gone"] = 3
	x.svc.orgRepoSeen["gone\x00repo"] = 2
	x.svc.hookMu.Unlock()
	x.svc.fanoutMu.Lock()
	x.svc.fanoutSeen["gone/repo"] = 4
	x.svc.fanoutSeen["acme/live"] = 0
	x.svc.fanoutMu.Unlock()

	x.svc.sweepWebhooks(ctx())
	x.svc.sweepFanout(ctx())
	x.svc.sweepOrgHooks(ctx())

	x.svc.hookMu.Lock()
	_, seenGone := x.svc.hookSeen["gone/repo"]
	_, pendGone := x.svc.hookPending["gone/repo"]
	_, liveRepo := x.svc.hookSeen["acme/live"]
	_, orgGone := x.svc.orgHookSeen["gone"]
	_, orgRepoGone := x.svc.orgRepoSeen["gone\x00repo"]
	x.svc.hookMu.Unlock()
	x.svc.fanoutMu.Lock()
	_, fanoutGone := x.svc.fanoutSeen["gone/repo"]
	_, fanoutLive := x.svc.fanoutSeen["acme/live"]
	x.svc.fanoutMu.Unlock()

	if seenGone || pendGone || orgGone || orgRepoGone || fanoutGone {
		t.Fatalf("stale watermarks survived: hookSeen=%v hookPending=%v orgSeen=%v orgRepo=%v fanout=%v",
			seenGone, pendGone, orgGone, orgRepoGone, fanoutGone)
	}
	if !liveRepo || !fanoutLive {
		t.Fatalf("observed repo watermarks dropped: hookSeen=%v fanoutSeen=%v", liveRepo, fanoutLive)
	}
}

// TestEnumerationsSkipMalformedPrefixes: a bucket key that yields an empty
// owner/org (a stray "repos//…" or "orgs//…") is skipped instead of being
// visited as a scope named "".
func TestEnumerationsSkipMalformedPrefixes(t *testing.T) {
	x := newHarness(t)
	writeRaw(t, x.svc.Store, "repos//junk/x.json", []byte("{}"))
	writeRaw(t, x.svc.Store, "repos/acme/one/x.json", []byte("{}"))
	writeRaw(t, x.svc.Store, "orgs//junk/x.json", []byte("{}"))
	writeRaw(t, x.svc.Store, "orgs/acme/x.json", []byte("{}"))

	var repos, orgs []string
	x.svc.eachRepo(ctx(), func(owner, repo string) { repos = append(repos, owner+"/"+repo) })
	x.svc.eachOrg(ctx(), func(org string) { orgs = append(orgs, org) })

	if len(repos) != 1 || repos[0] != "acme/one" {
		t.Fatalf("eachRepo visited %v, want [acme/one]", repos)
	}
	if len(orgs) != 1 || orgs[0] != "acme" {
		t.Fatalf("eachOrg visited %v, want [acme]", orgs)
	}
}

// TestRunServesOrgWakeThenExits pins the Run loop's org wake arm and, more
// importantly, that Run returns on cancel (the sweep goroutine owns that
// wait; a test that only cancels cannot see it finish).
func TestRunServesOrgWakeThenExits(t *testing.T) {
	x := newHarness(t)
	rctx, cancel := context.WithCancel(ctx())
	done := make(chan struct{})
	go func() { defer close(done); x.svc.Run(rctx) }()

	x.svc.wakeOrgCh <- "acme"
	deadline := time.Now().Add(15 * time.Second)
	for x.svc.TaskStatus(orgTaskRepo("acme"), TaskKindOrgWebhooks) == nil {
		if time.Now().After(deadline) {
			t.Fatal("Run never served the org wake-up")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestRetentionCompactsOrgEvents covers the retention pass's org
// enumeration and the org-log floor scan: a gap is skipped (never delete
// what cannot be seen), an old event is deleted, and a fresh event stops
// the scan because seq order approximates time order.
func TestRetentionCompactsOrgEvents(t *testing.T) {
	cases := []struct {
		name string
		seed map[int]string // seq → At
		want []int          // surviving seqs
	}{
		{"gap then old event deleted", map[int]string{2: "2020-01-01T00:00:00Z"}, nil},
		{"fresh event breaks the scan", map[int]string{1: "2020-01-01T00:00:00Z", 2: "2026-09-04T12:00:00Z"}, []int{2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := newHarness(t)
			writeRaw(t, x.svc.Store, OrgHookStateKey("acme"), mustEncode(t, OrgHookState{NextSeq: 3}))
			for seq, at := range tc.seed {
				writeRaw(t, x.svc.Store, OrgEventKey("acme", seq),
					mustEncode(t, ActivityEvent{Seq: seq, Repo: "acme", Kind: OrgHookKind,
						Action: OrgActionMemberAdded, At: at}))
			}
			x.svc.RunRetention(ctx())
			for seq := 1; seq <= 2; seq++ {
				keep := false
				for _, w := range tc.want {
					keep = keep || w == seq
				}
				_, _, err := store.GetBytes(ctx(), x.svc.Store, OrgEventKey("acme", seq), store.GetOptions{})
				if keep && err != nil {
					t.Fatalf("seq %d deleted, want retained: %v", seq, err)
				}
				if !keep && !store.IsNotFound(err) {
					t.Fatalf("seq %d kept (err=%v), want deleted", seq, err)
				}
			}
		})
	}
}

// TestSweepFanoutSkipsDrainedSeq: a pending event with a completion record
// is not re-enqueued (only undrained seqs are), so a resweep cannot start
// an empty task for work that already landed.
func TestSweepFanoutSkipsDrainedSeq(t *testing.T) {
	at := "2026-09-04T12:00:00Z"
	st := store.NewMemory()
	writeRaw(t, st, CollabStateKey("acme", "repo"), mustEncode(t, CollabState{NextSeq: 2}))
	seedActivity(t, st, at, "acme", "repo", 1, true,
		activityRecipient{Principal: "amy@example.com", Reason: ReasonSubscribed})
	writeRaw(t, st, FanoutDoneKey("acme", "repo", 1), mustEncode(t, FanoutDoneDoc{Seq: 1, At: at}))
	svc := New(st, nil)

	svc.sweepFanoutRepo(ctx(), "acme", "repo", "acme/repo")
	if got := svc.TaskStatus("acme/repo", TaskKindFanout); got != nil {
		t.Fatalf("drained seq re-enqueued a fan-out task: %+v", got)
	}
	if n := countPrincipalNotifs(t, st, "amy@example.com"); n != 0 {
		t.Fatalf("drained seq delivered %d notifications, want 0", n)
	}
}

// TestReadAllSkipsAlreadyRead pins the bulk mark-read loop: only unread
// entries flip, so the returned count is the number actually changed.
func TestReadAllSkipsAlreadyRead(t *testing.T) {
	x := newHarness(t)
	const who = "amy@example.com"
	at := x.now.Format(dateTimeFmt)
	readID, unreadID := "read-row", "unread-row"
	for _, id := range []string{readID, unreadID} {
		state := StateUnread
		if id == readID {
			state = StateRead
		}
		writeRaw(t, x.svc.Store, NotifKey(who, id), mustEncode(t, Notification{
			ID: id, Repo: "acme/repo", Num: 1, Kind: "issue",
			Reason: ReasonSubscribed, State: state, CreatedAt: at,
		}))
	}
	writeRaw(t, x.svc.Store, NotifIndexKey(who), mustEncode(t, IndexDoc{
		Version: 1, UnreadCount: 1, Entries: []IndexEntry{
			{ID: unreadID, Repo: "acme/repo", Num: 1, Kind: "issue", Reason: ReasonSubscribed, State: StateUnread, At: at},
			{ID: readID, Repo: "acme/repo", Num: 1, Kind: "issue", Reason: ReasonSubscribed, State: StateRead, At: at},
		},
	}))
	if got := x.svc.ReadAll(ctx(), who); got != 1 {
		t.Fatalf("ReadAll = %d, want 1 (the read row must not be recounted)", got)
	}
}
