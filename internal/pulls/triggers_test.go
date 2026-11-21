package pulls

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Threads pass A (docs/features/15_threads.md §1): the pulls service reports
// the pull_request lifecycle edges a CI trigger consumes — opened, synchronize
// (a new head commit on an open PR) and reopened — and never a close. The merge
// task reports its server-side base-ref publish through BranchPublished.

func recordPullEvents(e *testEnv) (*[]PullEvent, *sync.Mutex) {
	var mu sync.Mutex
	var got []PullEvent
	e.svc.PullEvents = func(_ context.Context, ev PullEvent) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev)
	}
	return &got, &mu
}

func pullEvents(got *[]PullEvent, mu *sync.Mutex) []PullEvent {
	mu.Lock()
	defer mu.Unlock()
	return append([]PullEvent(nil), (*got)...)
}

func TestPullEventsOpenedSynchronizeReopened(t *testing.T) {
	e := newTestEnv()
	got, mu := recordPullEvents(e)
	th, pr := openBasic(t, e, "o", "r")

	events := pullEvents(got, mu)
	if len(events) != 1 {
		t.Fatalf("opened events = %+v", events)
	}
	ev := events[0]
	if ev.Action != "opened" || ev.Repo != "o/r" || ev.Num != th.Num ||
		ev.BaseRef != "refs/heads/main" || ev.HeadRef != "refs/heads/topic" ||
		ev.HeadSHA != pr.Head.SHA || ev.HeadRepo != "o/r" || ev.Fork {
		t.Fatalf("opened event = %+v", ev)
	}

	// A new head commit on the open PR is the synchronize edge.
	newHead := hexSHA(3)
	e.seedRefs("o/r", map[string]string{"refs/heads/topic": newHead})
	e.svc.HandleRefEvent(ctx(), "o", "r", RefEvent{
		Repo: "o/r", RefName: "refs/heads/topic", Old: pr.Head.SHA, New: newHead,
	})
	events = pullEvents(got, mu)
	if len(events) != 2 || events[1].Action != "synchronize" || events[1].HeadSHA != newHead {
		t.Fatalf("synchronize events = %+v", events)
	}

	// Closing must never fire a CI trigger.
	closed := StateClosed
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", th.Num, writer(), PRPatch{State: &closed}); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := pullEvents(got, mu); len(got) != 2 {
		t.Fatalf("close fired a trigger: %+v", got)
	}

	// Reopening is the third edge.
	open := StateOpen
	if _, _, err := e.svc.UpdatePR(ctx(), "o", "r", th.Num, writer(), PRPatch{State: &open}); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	events = pullEvents(got, mu)
	if len(events) != 3 || events[2].Action != "reopened" || events[2].HeadSHA != newHead {
		t.Fatalf("reopened events = %+v", events)
	}
}

func TestPullEventsForkOpened(t *testing.T) {
	e := newTestEnv()
	got, mu := recordPullEvents(e)
	e.roles.Roles["jane@example.com"] = "write"
	e.seedRefs("o/r", map[string]string{"refs/heads/main": hexSHA(1)})
	e.seedRefs("f/r", map[string]string{"refs/heads/feat": hexSHA(9)})
	e.git.ReachableMap = map[string]bool{hexSHA(9): false}
	if _, _, err := e.svc.OpenPR(ctx(), "o", "r", writer(), OpenInput{
		Title: "fork work", BaseRef: "refs/heads/main", HeadRef: "refs/heads/feat", Fork: &ForkInfo{Repo: "f/r"},
	}, ""); err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	events := pullEvents(got, mu)
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	if !events[0].Fork || events[0].HeadRepo != "f/r" || events[0].BaseRef != "refs/heads/main" {
		t.Fatalf("fork event = %+v", events[0])
	}
}

func TestMergePublishesBranchToSink(t *testing.T) {
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))

	type published struct{ repo, ref, old, new string }
	var mu sync.Mutex
	var got []published
	e.svc.BranchPublished = func(_ context.Context, repo, ref, old, newSHA string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, published{repo, ref, old, newSHA})
	}
	if _, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, ""); err != nil {
		t.Fatalf("StartMerge: %v", err)
	}
	done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
	if done == nil || done.State != TaskOK {
		t.Fatalf("merge task = %+v", done)
	}
	sha, _ := done.Result["sha"].(string)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("published events = %+v", got)
	}
	if got[0].repo != "o/r" || got[0].ref != "refs/heads/main" || got[0].old != hexSHA(1) || got[0].new != sha {
		t.Fatalf("published = %+v (merge sha %s)", got[0], sha)
	}
}

// TestPullEventsUnwiredIsNoop pins the nil-safe seams (a Service built without
// composition must not panic on any lifecycle edge).
func TestPullEventsUnwiredIsNoop(t *testing.T) {
	e := newTestEnv()
	e.svc.PullEvents = nil
	e.svc.BranchPublished = nil
	openBasic(t, e, "o", "r")
	e.svc.pullEvent(ctx(), PullEvent{Repo: "o/r", Action: "opened"})
	e.svc.branchPublished(ctx(), "o/r", "refs/heads/main", "a", "b")
}

// TestPullEventsOutOfOrderHeadDelivery is the round-2 regression: a WAL ref
// event delivered late (or replayed) must never move a PR head backwards and
// must never create a second `pull_request` run for a sha the PR has left.
// Ordering comes from the event itself (its WAL sequence + its pre-image), not
// from wall-clock time.
func TestPullEventsOutOfOrderHeadDelivery(t *testing.T) {
	t.Run("newer event first, older replay second", func(t *testing.T) {
		e := newTestEnv()
		got, mu := recordPullEvents(e)
		openBasic(t, e, "o", "r") // head = 2
		e.git.Ancestors[hexSHA(2)+"\x00"+hexSHA(3)] = true
		e.git.Ancestors[hexSHA(2)+"\x00"+hexSHA(4)] = true
		e.git.Ancestors[hexSHA(3)+"\x00"+hexSHA(4)] = true

		// Push B (2 → 4 at seq 11) is delivered first; push A (2 → 3 at seq
		// 10) only afterwards. A is older, so it is stale whatever its sha.
		e.seedRefs("o/r", map[string]string{"refs/heads/topic": hexSHA(4)})
		e.svc.HandleRefEvent(ctx(), "o", "r", RefEvent{
			Repo: "o/r", RefName: "refs/heads/topic", Old: hexSHA(2), New: hexSHA(4), Seq: 11,
		})
		e.svc.HandleRefEvent(ctx(), "o", "r", RefEvent{
			Repo: "o/r", RefName: "refs/heads/topic", Old: hexSHA(2), New: hexSHA(3), Seq: 10,
		})

		pr, _, err := e.svc.loadPR(ctx(), "o", "r", 1)
		if err != nil {
			t.Fatalf("loadPR: %v", err)
		}
		if pr.Head.SHA != hexSHA(4) {
			t.Fatalf("head rewound to %s, want %s", pr.Head.SHA, hexSHA(4))
		}
		if pr.HeadForcePushedAt != nil {
			t.Fatalf("a stale replay stamped a spurious force-push: %v", *pr.HeadForcePushedAt)
		}
		if pr.HeadSeq != 11 {
			t.Fatalf("head watermark = %d, want 11", pr.HeadSeq)
		}
		sync := 0
		for _, ev := range pullEvents(got, mu) {
			if ev.Action == "synchronize" {
				sync++
				if ev.HeadSHA != hexSHA(4) {
					t.Fatalf("synchronize fired for the stale sha %s", ev.HeadSHA)
				}
			}
		}
		if sync != 1 {
			t.Fatalf("synchronize events = %d, want exactly 1 (the applied move)", sync)
		}
	})

	t.Run("live read ahead of a late older event", func(t *testing.T) {
		// The audited shape: the head was already 4 (recorded by a live read),
		// and a replayed event for 3 arrives with an older pre-image. It must
		// be ignored — no head move, no force-push stamp, no trigger.
		e := newTestEnv()
		got, mu := recordPullEvents(e)
		openBasic(t, e, "o", "r")
		e.git.Ancestors[hexSHA(2)+"\x00"+hexSHA(4)] = true
		e.seedRefs("o/r", map[string]string{"refs/heads/topic": hexSHA(4)})
		if _, err := e.svc.GetPR(ctx(), "o", "r", 1, writer(), 0, 0); err != nil {
			t.Fatalf("GetPR: %v", err)
		}
		before := len(pullEvents(got, mu))
		e.seedRefs("o/r", map[string]string{"refs/heads/topic": hexSHA(3)})
		e.svc.HandleRefEvent(ctx(), "o", "r", RefEvent{
			Repo: "o/r", RefName: "refs/heads/topic", Old: hexSHA(2), New: hexSHA(3), Seq: 4,
		})
		pr, _, err := e.svc.loadPR(ctx(), "o", "r", 1)
		if err != nil {
			t.Fatalf("loadPR: %v", err)
		}
		if pr.Head.SHA != hexSHA(4) || pr.HeadForcePushedAt != nil {
			t.Fatalf("late event rewound the head: %+v", pr)
		}
		if after := len(pullEvents(got, mu)); after != before {
			t.Fatalf("a stale event fired %d triggers", after-before)
		}
	})

	t.Run("a genuine ordered force-push still applies", func(t *testing.T) {
		e := newTestEnv()
		openBasic(t, e, "o", "r")
		e.git.Ancestors[hexSHA(2)+"\x00"+hexSHA(3)] = true
		e.seedRefs("o/r", map[string]string{"refs/heads/topic": hexSHA(3)})
		e.svc.HandleRefEvent(ctx(), "o", "r", RefEvent{
			Repo: "o/r", RefName: "refs/heads/topic", Old: hexSHA(2), New: hexSHA(3), Seq: 5,
		})
		// The ref is force-pushed BACK to 2: the event's pre-image is the head
		// we hold, so this is the ref's next transition, not a replay.
		e.seedRefs("o/r", map[string]string{"refs/heads/topic": hexSHA(2)})
		moved := e.svc.applyHeadEvent(ctx(), "o", "r", mustLoadPR(t, e), mustThread(t, e),
			HeadMove{New: hexSHA(2), Old: hexSHA(3), Seq: 6, Event: true}, writer())
		if !moved {
			t.Fatal("a genuine force-push back did not apply")
		}
		pr, _, _ := e.svc.loadPR(ctx(), "o", "r", 1)
		if pr.Head.SHA != hexSHA(2) || pr.HeadForcePushedAt == nil {
			t.Fatalf("force-push back = %+v", pr)
		}
	})
}

// TestJobTokenRefEventNeverSynchronizes pins round 3's suppression on the
// event-replay path too: a head move recorded under a Threads job credential
// still refreshes the PR head (the ref moved), but creates no `synchronize`
// trigger — the periodic events sweep cannot resurrect the run the push path
// suppressed.
func TestJobTokenRefEventNeverSynchronizes(t *testing.T) {
	e := newTestEnv()
	got, mu := recordPullEvents(e)
	_, pr := openBasic(t, e, "o", "r")
	newHead := hexSHA(5)
	e.seedRefs("o/r", map[string]string{"refs/heads/topic": newHead})
	e.svc.HandleRefEvent(ctx(), "o", "r", RefEvent{
		Repo: "o/r", RefName: "refs/heads/topic", Old: pr.Head.SHA, New: newHead,
		ByJob: true,
	})
	for _, ev := range pullEvents(got, mu) {
		if ev.Action == "synchronize" {
			t.Fatalf("a job-token ref event fired synchronize: %+v", ev)
		}
	}
	// The head still moved: the trigger is suppressed, not the refresh.
	fresh, err := e.svc.GetPR(ctx(), "o", "r", pr.Num, writer(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.PR == nil || fresh.PR.Head.SHA != newHead {
		t.Fatalf("PR head did not move to %s: %+v", newHead, fresh.PR)
	}
	// A human (unmarked) event on the next head does synchronize.
	next := hexSHA(6)
	e.seedRefs("o/r", map[string]string{"refs/heads/topic": next})
	e.svc.HandleRefEvent(ctx(), "o", "r", RefEvent{
		Repo: "o/r", RefName: "refs/heads/topic", Old: newHead, New: next,
	})
	syncs := 0
	for _, ev := range pullEvents(got, mu) {
		if ev.Action == "synchronize" {
			syncs++
		}
	}
	if syncs != 1 {
		t.Fatalf("synchronize events = %d, want 1", syncs)
	}
}
