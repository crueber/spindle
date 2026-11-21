package threads

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

// Failure-side coverage for the `needs:` gate and cascade (15 §4.1): every
// store read/write on the eligibility path fails closed, and a missing
// dependency record is skipped rather than blocking a run forever.

func TestNeedsGateFaultsAndMissingDependency(t *testing.T) {
	e, fs, _, _ := faultEnv(t)
	run := e.mustCreate(t)[0]
	head, _, err := e.svc.loadHead(ctx(), "o", "r")
	if err != nil || head == nil {
		t.Fatalf("head = %+v, %v", head, err)
	}
	var buildEntry *QueueEntry
	for _, seq := range head.Pending {
		entry, _, eerr := e.svc.loadEntry(ctx(), "o", "r", seq)
		if eerr != nil {
			t.Fatalf("entry: %v", eerr)
		}
		if entry.JobID == "j-test" {
			buildEntry = entry
		}
	}
	if buildEntry == nil || len(buildEntry.Needs) != 1 {
		t.Fatalf("test entry = %+v", buildEntry)
	}

	// A missing dependency record can never succeed: the dependent is blocked
	// (never offered), not left waiting forever.
	cache := jobCache{}
	if derr := e.store.Delete(ctx(), JobKey("o", "r", run.ID, "j-build"), ""); derr != nil {
		t.Fatalf("delete lint job: %v", derr)
	}
	gate, gerr := e.svc.needsGate(ctx(), "o", "r", buildEntry, cache)
	if gerr != nil || gate != needsBlocked {
		t.Fatalf("missing dependency = %v, %v (want blocked)", gate, gerr)
	}

	// A corrupt dependency record is an error, never a silent "ready".
	if _, err := store.PutBytes(ctx(), e.store, JobKey("o", "r", run.ID, "j-build"),
		[]byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed corrupt dep: %v", err)
	}
	if _, gerr := e.svc.needsGate(ctx(), "o", "r", buildEntry, jobCache{}); !errors.Is(gerr, ErrCorrupt) {
		t.Fatalf("corrupt dependency = %v", gerr)
	}

	// A failing GET propagates.
	fs.fail("get", 0)
	if _, gerr := e.svc.needsGate(ctx(), "o", "r", buildEntry, jobCache{}); gerr == nil {
		t.Fatal("needsGate ignored a store failure")
	}
	fs.clear()

	// The per-probe cache is used (a second call issues no read).
	fs.fail("get", 0)
	defer fs.clear()
	if _, gerr := e.svc.needsGate(ctx(), "o", "r", buildEntry, cache); gerr != nil {
		t.Fatalf("cached gate: %v", gerr)
	}
}

func TestNeedsCascadeFaults(t *testing.T) {
	e, fs, _, _ := faultEnv(t)
	run := e.mustCreate(t)[0]
	// A failing job-family read fails the cascade.
	fs.fail("list", 0)
	if err := e.svc.skipBlockedDependents(ctx(), "o", "r", run.ID, nil); err == nil {
		t.Fatal("cascade ignored a list failure")
	}
	fs.clear()

	// Unknown run: the cascade is a no-op (loadRun returns nil).
	if err := e.svc.skipBlockedDependents(ctx(), "o", "r", "0000000000000000001-abcdef", nil); err != nil {
		t.Fatalf("unknown run cascade = %v", err)
	}

	// skipQueuedJob: unknown job, corrupt record, already-terminal no-op.
	loaded, _ := e.svc.loadRun(ctx(), "o", "r", run.ID)
	if _, err := e.svc.skipQueuedJob(ctx(), "o", "r", loaded, run.ID, "j-ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown job cancel = %v", err)
	}
	jobs, _ := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	key := JobKey("o", "r", run.ID, jobs[0].ID)
	if _, err := store.PutBytes(ctx(), e.store, key, []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed corrupt job: %v", err)
	}
	if _, err := e.svc.skipQueuedJob(ctx(), "o", "r", loaded, run.ID, jobs[0].ID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt job cancel = %v", err)
	}
	// Restore a real record, drive it terminal, then cancel again: no write.
	body, _ := json.Marshal(jobs[0])
	if _, err := store.PutBytes(ctx(), e.store, key, body, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("restore job: %v", err)
	}
	e.finish(t, run.ID, jobs[0].ID, JobSuccess)
	if updated, err := e.svc.skipQueuedJob(ctx(), "o", "r", loaded, run.ID, jobs[0].ID); err != nil || updated != nil {
		t.Fatalf("terminal job cancel = %+v, %v", updated, err)
	}

	// A mirror failure after a successful skip CAS is reported (the CAS is the
	// commit point; the next transition re-mirrors), never swallowed.
	if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, jobs[0].ID, JobUpdate{State: JobQueued}); err != nil {
		t.Fatalf("requeue job: %v", err)
	}
	e.checks.err = errors.New("checks down")
	defer func() { e.checks.err = nil }()
	if _, err := e.svc.skipQueuedJob(ctx(), "o", "r", loaded, run.ID, jobs[0].ID); err == nil {
		t.Fatal("a mirror failure was swallowed")
	}
}
