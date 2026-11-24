// tasks_test.go — the task system (05 §5.8): (repo,kind) single-flight join,
// lag-tolerant broadcast with replay, drain hooks, recent ring.
package wal

import (
	"context"
	"testing"
	"time"
)

func TestBroadcast_ReplayAndLagTolerance(t *testing.T) {
	var b Broadcast[int]
	b.Send(1)
	b.Send(2)
	id, ch, replay := b.Subscribe()
	if len(replay) != 2 || replay[0] != 1 || replay[1] != 2 {
		t.Fatalf("replay = %v, want [1 2]", replay)
	}
	// Non-blocking send: a full subscriber drops instead of blocking (§5.8).
	for i := 0; i < bcastSubCap+10; i++ {
		b.Send(100 + i)
	}
	dropped := 0
	deadline := time.After(time.Second)
	got := 0
	for got < bcastSubCap {
		select {
		case <-ch:
			got++
		case <-deadline:
			t.Fatalf("only %d of %d buffered packets arrived", got, bcastSubCap)
		}
	}
	_ = dropped
	select {
	case v := <-ch:
		t.Fatalf("buffer overflowed (got %d)", v)
	default:
	}
	// Unsubscribe closes the channel exactly once (owner: the broadcast).
	b.Unsubscribe(id)
	if _, ok := <-ch; ok {
		t.Fatal("channel open after Unsubscribe")
	}
	b.Unsubscribe(id) // idempotent
}

func TestTaskTable_JoinSemantics(t *testing.T) {
	tt := newTaskTable("test-host", context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	var runs int
	run := func(ctx context.Context, task *Task) error {
		runs++
		close(started)
		<-release
		task.Notice("done working")
		return nil
	}

	leaderDone := make(chan struct{})
	go func() {
		rec, err := tt.Run(context.Background(), "acme/api", "materialize", nil, run)
		if err != nil || rec == nil {
			t.Errorf("leader: %v", err)
		}
		close(leaderDone)
	}()
	<-started

	// A second start of the same (repo,kind) JOINS the running one.
	joinerDone := make(chan struct{})
	go func() {
		rec, err := tt.Run(context.Background(), "acme/api", "materialize", nil, run)
		if err != nil || rec == nil {
			t.Errorf("joiner: %v", err)
		}
		if rec != nil && rec.Summary != "" && rec.OK == nil {
			t.Error("joiner got a non-terminal record")
		}
		close(joinerDone)
	}()
	time.Sleep(20 * time.Millisecond) // the joiner must be waiting, not re-running
	if runs != 1 {
		t.Fatalf("runs = %d, want 1 (joiner must join, not re-run)", runs)
	}

	close(release)
	<-leaderDone
	<-joinerDone
	if runs != 1 {
		t.Fatalf("runs = %d after completion, want 1", runs)
	}
	recs := tt.List("acme/api")
	if len(recs) != 1 {
		t.Fatalf("recent = %d records, want 1", len(recs))
	}
	if recs[0].OK == nil || !*recs[0].OK {
		t.Fatalf("record not OK: %+v", recs[0])
	}
	if len(recs[0].LogTail) != 1 || recs[0].LogTail[0] != "done working" {
		t.Fatalf("log tail = %v", recs[0].LogTail)
	}
}

func TestTaskTable_DifferentKindsRunConcurrently(t *testing.T) {
	tt := newTaskTable("test-host", context.Background())
	blocked := make(chan struct{})
	release := make(chan struct{})
	block := func(ctx context.Context, task *Task) error {
		close(blocked)
		<-release
		return nil
	}
	go tt.Run(context.Background(), "acme/api", "materialize", nil, block)
	<-blocked
	// A different kind on the same repo must NOT join.
	done := make(chan struct{})
	go func() {
		defer close(done)
		tt.Run(context.Background(), "acme/api", "remote-index", nil, func(ctx context.Context, t *Task) error { return nil })
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("(repo, kind) collision: remote-index joined materialize")
	}
	close(release)
}

func TestTaskTable_DrainInterrupts(t *testing.T) {
	tt := newTaskTable("test-host", context.Background())
	blocked := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		rec, _ := tt.Run(context.Background(), "acme/api", "fsck", nil, func(tctx context.Context, t *Task) error {
			close(blocked)
			<-tctx.Done() // interrupted by drain
			return tctx.Err()
		})
		if rec != nil && rec.OK != nil && *rec.OK {
			t.Error("interrupted task recorded success")
		}
		close(drained)
	}()
	<-blocked
	tt.Drain()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("drain did not interrupt the running task")
	}
	// Further starts refuse while draining.
	if _, err := tt.Run(context.Background(), "acme/api", "fsck", nil, func(ctx context.Context, t *Task) error { return nil }); err == nil {
		t.Fatal("start during drain succeeded")
	}
}

// TestTaskTable_RunningJoinHint pins the HTTP 202 id hint: Running reports the
// live record for (repo, kind) while a task runs, nil once it finishes, and a
// copy (callers cannot mutate the table's record through it).
func TestTaskTable_RunningJoinHint(t *testing.T) {
	tt := newTaskTable("test-host", context.Background())
	if rec := tt.Running("o/r", "sync-fork"); rec != nil {
		t.Fatalf("idle table reported %+v", rec)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = tt.Run(context.Background(), "o/r", "sync-fork", nil, func(ctx context.Context, task *Task) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	rec := tt.Running("o/r", "sync-fork")
	if rec == nil || rec.ID == "" || rec.Kind != "sync-fork" || rec.Repo != "o/r" {
		t.Fatalf("running record %+v", rec)
	}
	rec.Kind = "mutated"
	if again := tt.Running("o/r", "sync-fork"); again.Kind != "sync-fork" {
		t.Fatal("Running returned the live record, not a copy")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for tt.Running("o/r", "sync-fork") != nil {
		if time.Now().After(deadline) {
			t.Fatal("running record never cleared")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBroadcast_CloseEndsSubscribers(t *testing.T) {
	var b Broadcast[int]
	b.Send(1)
	_, ch, _ := b.Subscribe()
	b.Close()
	if _, ok := <-ch; ok {
		t.Fatal("Close must close subscriber channels")
	}
	// Send after Close is a no-op (never a panic on a closed channel).
	b.Send(2)
	// Subscribing after Close hands back an already-closed channel (a reader
	// ends instead of blocking on a stream nobody will close).
	_, dead, replay := b.Subscribe()
	if replay != nil {
		t.Fatalf("closed broadcast replay = %v, want nil", replay)
	}
	if _, ok := <-dead; ok {
		t.Fatal("subscribe after Close must end")
	}
	b.Close() // idempotent
}

// TestTaskTable_SubscribeByID covers the handle-free attach path (PLAN-V2
// §T.1): attach to a task by id, get its replay + live packets, and end the
// stream when the record is evicted.
func TestTaskTable_SubscribeByID(t *testing.T) {
	tt := newTaskTable("test-host", context.Background())
	release := make(chan struct{})
	started := make(chan struct{})
	done := make(chan *TaskRecord, 1)
	go func() {
		rec, _ := tt.Run(context.Background(), "acme/api", "repo-transfer", nil,
			func(ctx context.Context, task *Task) error {
				// Signal only after BOTH narrations exist: the subscriber
				// below asserts its replay ring carries them, and closing
				// started first made that a race against this goroutine
				// (replay = [task, notice] with the bar still unwritten).
				task.Notice("phase one")
				task.Progress("objects", 1, 4, "objects")
				close(started)
				<-release
				return nil
			})
		done <- rec
	}()
	<-started
	rec := tt.Running("acme/api", "repo-transfer")
	if rec == nil {
		t.Fatal("no running record")
	}
	subID, ch, replay, ok := tt.Subscribe(rec.ID)
	if !ok {
		t.Fatal("Subscribe by id failed")
	}
	// The replay ring carries the narration the late attacher missed.
	var notices, bars int
	for _, p := range replay {
		switch p.Kind {
		case "notice":
			notices++
		case "progress":
			bars++
		}
	}
	if notices == 0 || bars == 0 {
		t.Fatalf("replay missing narration: %+v", replay)
	}
	close(release)
	final := <-done
	// Follow the live stream until the terminal task packet (the channel is
	// closed by Unsubscribe/the janitor, never by the reader).
	var terminal *TaskRecord
	deadline := time.After(5 * time.Second)
read:
	for {
		select {
		case p, ok := <-ch:
			if !ok {
				break read
			}
			if p.Kind == "task" && p.Task != nil && p.Task.Finished != "" {
				cp := *p.Task
				terminal = &cp
				break read
			}
		case <-deadline:
			t.Fatal("no terminal packet on the attach stream")
		}
	}
	tt.Unsubscribe(rec.ID, subID)
	if _, stillOpen := <-ch; stillOpen {
		t.Fatal("Unsubscribe must close the attach channel")
	}
	if terminal == nil || terminal.ID != final.ID {
		t.Fatalf("terminal packet = %+v, want record %s", terminal, final.ID)
	}
	if final.Summary != "" {
		t.Fatalf("clean task summary = %q", final.Summary)
	}
	// Unknown ids answer ok=false (404 at the handler).
	if _, _, _, ok := tt.Subscribe("nope"); ok {
		t.Fatal("unknown id must not subscribe")
	}
	tt.Unsubscribe("nope", 1) // idempotent on an unknown id
}

// TestTaskTableRunningForAndEviction covers the listing's running half and the
// janitor's eviction of a finished record with its packet drone (the attach
// stream must end, not leak).
func TestTaskTableRunningForAndEviction(t *testing.T) {
	tt := newTaskTable("test-host", context.Background())
	ctx := context.Background()
	releaseF, releaseB := make(chan struct{}), make(chan struct{})
	startedF, startedB := make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = tt.Run(ctx, "acme/api", "fsck", nil, func(ctx context.Context, task *Task) error {
			close(startedF)
			<-releaseF
			return nil
		})
	}()
	<-startedF
	go func() {
		_, _ = tt.Run(ctx, "acme/api", "bundle", nil, func(ctx context.Context, task *Task) error {
			close(startedB)
			<-releaseB
			return nil
		})
	}()
	<-startedB
	// A different repo's running task must not appear — and it stays RUNNING
	// through the first eviction assertion. A task that returns immediately is
	// already finished, so evictFinished(future) evicts it and the "did not
	// touch live records" assertion fails under load (the transfer T1 gate flake:
	// "evicted 1 live records").
	releaseO := make(chan struct{})
	go func() {
		_, _ = tt.Run(ctx, "acme/other", "fsck", nil, func(ctx context.Context, task *Task) error {
			<-releaseO
			return nil
		})
	}()
	live := tt.RunningFor("acme/api")
	if len(live) != 2 {
		t.Fatalf("RunningFor = %d entries, want 2", len(live))
	}
	if live[0].Kind != "fsck" || live[1].Kind != "bundle" {
		t.Fatalf("RunningFor order = %s,%s (want start order)", live[0].Kind, live[1].Kind)
	}
	if got := tt.RunningFor("acme/none"); len(got) != 0 {
		t.Fatalf("unknown repo running = %d", len(got))
	}
	rec := tt.Running("acme/api", "fsck")
	subID, _, _, ok := tt.Subscribe(rec.ID)
	if !ok {
		t.Fatal("subscribe failed")
	}
	// Eviction must not touch a live record.
	if n := tt.evictFinished(time.Now().Add(48 * time.Hour)); n != 0 {
		t.Fatalf("evicted %d live records", n)
	}
	close(releaseO)
	close(releaseF)
	close(releaseB)
	waitFinished := func(id string) {
		deadline := time.Now().Add(2 * time.Second)
		for {
			if cur := tt.Get(id); cur != nil && cur.Finished != "" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("task %s never finished", id)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	waitFinished(rec.ID)
	tt.Unsubscribe(rec.ID, subID)
	// Re-subscribe so the eviction's Close has a live subscriber to end.
	_, ch2, _, ok := tt.Subscribe(rec.ID)
	if !ok {
		t.Fatal("re-subscribe failed")
	}
	if n := tt.evictFinished(time.Now().Add(48 * time.Hour)); n < 2 {
		t.Fatalf("evicted %d records, want at least the two finished ones", n)
	}
	if _, open := <-ch2; open {
		t.Fatal("eviction must close the drone (attached reader ends)")
	}
	if tt.Get(rec.ID) != nil {
		t.Fatal("record survived eviction")
	}
	if _, _, _, ok := tt.Subscribe(rec.ID); ok {
		t.Fatal("evicted task must not be subscribable")
	}
}
