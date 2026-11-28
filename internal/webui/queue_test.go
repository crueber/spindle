package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/mergequeue"
	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/server/auth"
)

// queueFixture binds the queue page seam over the shared collab fixture.
func queueFixture(t *testing.T, d QueueDeps) *collabFixture {
	t.Helper()
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Write: true}
	cd := pullDeps()
	cd.RoleOf = func(_ context.Context, _, _ string, p auth.Principal) identity.Role {
		if p.Anonymous {
			return ""
		}
		return identity.RoleMaintain
	}
	f.deps(cd)
	BindQueue(d)
	t.Cleanup(func() { queueDeps = QueueDeps{} })
	return f
}

func sampleQueueView() *mergequeue.View {
	return &mergequeue.View{
		Repo: "acme/widgets",
		Queues: []*mergequeue.QueueView{{
			Base: "main",
			Entries: []*mergequeue.Entry{
				{Number: 5, Base: "main", Title: "Add cache", Author: "alice", Enqueuer: "alice", State: mergequeue.StateValidating, Position: 1, MergeSHA: strings.Repeat("c", 40), EnqueuedAt: "2026-11-19T10:00:00Z"},
				{Number: 6, Base: "main", Title: "Fix bug", Author: "bob", Enqueuer: "alice", State: mergequeue.StatePending, Position: 2, EnqueuedAt: "2026-11-19T10:05:00Z"},
			},
		}},
		Recent: []*mergequeue.Entry{{
			Number: 4, Base: "main", Title: "Old change", Author: "bob", State: mergequeue.StateEjected,
			Reason: "required checks not green for abc: ci/test (failure)", FinishedAt: "2026-11-19T09:00:00Z",
		}},
		Audit: []mergequeue.AuditEntry{
			{At: "2026-11-19T10:00:00Z", Actor: "alice", Action: "enqueued", Base: "main", Number: 5},
			{At: "2026-11-19T09:00:00Z", Actor: "spindle", Action: "ejected", Base: "main", Number: 4, Detail: "ci/test (failure)"},
		},
		UpdatedAt: "2026-11-19T10:05:00Z",
	}
}

func TestQueuePageRenders(t *testing.T) {
	var listed, enqueued, dequeued, paused, resumed int
	f := queueFixture(t, QueueDeps{
		List: func(context.Context, string, string, auth.Principal) (*mergequeue.View, error) {
			listed++
			return sampleQueueView(), nil
		},
		Enqueue: func(_ context.Context, _, _ string, _ auth.Principal, in mergequeue.EnqueueInput) (*mergequeue.Entry, error) {
			enqueued++
			return &mergequeue.Entry{Number: in.Number, Base: in.Base, State: mergequeue.StatePending, Position: 3}, nil
		},
		Dequeue: func(_ context.Context, _, _, _ string, num int, _ auth.Principal) (*mergequeue.Entry, error) {
			dequeued++
			return &mergequeue.Entry{Number: num, State: mergequeue.StateCancelled}, nil
		},
		Pause: func(context.Context, string, string, string, string, auth.Principal) error {
			paused++
			return nil
		},
		Resume: func(context.Context, string, string, string, auth.Principal) error {
			resumed++
			return nil
		},
	})

	rec := f.get("/acme/widgets/queue")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec,
		"Merge queue",
		"base <span class=\"mono\">main</span>",
		"#1", "validating", "Add cache",
		"#2", "queued · position 2", "Fix bug",
		"required checks not green",
		"Recent outcomes",
		"Audit",
		"enqueued",
		"ejected",
		"/acme/widgets/queue/enqueue",
		"/acme/widgets/queue/pause",
		`aria-label="merge queue for main"`,
	)
	// A state chip carries its text label (state is never colour-only).
	if !strings.Contains(bodyText(rec), ">validating</span>") || !strings.Contains(bodyText(rec), ">queued · position 2</span>") {
		t.Fatal("state chips must carry their text")
	}
	// The filter links to all branches when narrowed.
	if rec := f.get("/acme/widgets/queue?base=main"); !strings.Contains(bodyText(rec), "all branches") {
		t.Fatal("filtered page must link back to all branches")
	}

	// POST enqueue / dequeue / pause / resume.
	rec = f.post("/acme/widgets/queue/enqueue", url.Values{"base": {"main"}, "number": {"7"}, "priority": {"2"}})
	wantStatus(t, rec, http.StatusSeeOther)
	if enqueued != 1 || rec.Header().Get("Location") != "/acme/widgets/queue?base=main" {
		t.Fatalf("enqueue = %d, location %q", enqueued, rec.Header().Get("Location"))
	}
	rec = f.post("/acme/widgets/queue/dequeue", url.Values{"base": {"main"}, "number": {"7"}})
	wantStatus(t, rec, http.StatusSeeOther)
	if dequeued != 1 {
		t.Fatalf("dequeue = %d", dequeued)
	}
	rec = f.post("/acme/widgets/queue/pause", url.Values{"base": {"main"}, "reason": {"freeze"}})
	wantStatus(t, rec, http.StatusSeeOther)
	rec = f.post("/acme/widgets/queue/resume", url.Values{"base": {"main"}})
	wantStatus(t, rec, http.StatusSeeOther)
	if paused != 1 || resumed != 1 {
		t.Fatalf("pause/resume = %d/%d", paused, resumed)
	}

	// Bad input renders an error with a 400.
	rec = f.get("/acme/widgets/queue")
	wantStatus(t, rec, http.StatusOK)
	rec = f.post("/acme/widgets/queue/enqueue", url.Values{"base": {"main"}, "number": {"0"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("zero number = %d", rec.Code)
	}
	rec = f.post("/acme/widgets/queue/enqueue", url.Values{"number": {"7"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no base = %d", rec.Code)
	}
	// Cross-origin is refused before anything runs.
	req := f.req("POST", "/acme/widgets/queue/enqueue", url.Values{"base": {"main"}, "number": {"7"}})
	req.Header.Set("Origin", "http://evil.example")
	out := httptest.NewRecorder()
	if !f.srv.Serve(out, req) {
		t.Fatal("cross-origin post not claimed")
	}
	wantStatus(t, out, http.StatusForbidden)
	if listed == 0 {
		t.Fatal("list not called")
	}
}

func TestQueuePageAnonymousAndNotWired(t *testing.T) {
	f := queueFixture(t, QueueDeps{
		List: func(context.Context, string, string, auth.Principal) (*mergequeue.View, error) {
			return sampleQueueView(), nil
		},
	})
	f.principal = auth.Anonymous()
	rec := f.get("/acme/widgets/queue")
	wantStatus(t, rec, http.StatusOK)
	// No maintainer affordances for an anonymous reader.
	wantNotContains(t, rec, "/acme/widgets/queue/enqueue", "/acme/widgets/queue/pause")

	// Not wired renders an unavailable page, not a panic.
	BindQueue(QueueDeps{})
	rec = f.get("/acme/widgets/queue")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	wantContains(t, rec, "merge queue service not wired")
}

func TestPullQueuePanel(t *testing.T) {
	entry := &mergequeue.Entry{
		Number: 5, Base: "main", Title: "Add cache", Author: "alice", Enqueuer: "alice",
		State: mergequeue.StatePending, Position: 1, MergeSHA: strings.Repeat("c", 40),
	}
	f := queueFixture(t, QueueDeps{
		ForPR: func(context.Context, string, string, int, auth.Principal) (*mergequeue.PRView, error) {
			return &mergequeue.PRView{Base: "main", Entry: entry}, nil
		},
		Dequeue: func(context.Context, string, string, string, int, auth.Principal) (*mergequeue.Entry, error) {
			return &mergequeue.Entry{Number: 5, State: mergequeue.StateCancelled}, nil
		},
	})
	rec := f.get("/acme/widgets/pull/5")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "Merge queue", "queued · position 1", "remove from queue", "/acme/widgets/pull/5/queue/dequeue", "open the queue for main")

	// The panel's dequeue posts to the PR route and returns to the PR.
	rec = f.post("/acme/widgets/pull/5/queue/dequeue", url.Values{"base": {"main"}})
	wantStatus(t, rec, http.StatusSeeOther)
	if rec.Header().Get("Location") != "/acme/widgets/pull/5" {
		t.Fatalf("location = %q", rec.Header().Get("Location"))
	}

	// Not queued: a maintainer gets the add control.
	f2 := queueFixture(t, QueueDeps{
		ForPR: func(context.Context, string, string, int, auth.Principal) (*mergequeue.PRView, error) {
			return &mergequeue.PRView{Base: "main"}, nil
		},
		Enqueue: func(context.Context, string, string, auth.Principal, mergequeue.EnqueueInput) (*mergequeue.Entry, error) {
			return entry, nil
		},
	})
	rec = f2.get("/acme/widgets/pull/5")
	wantContains(t, rec, "not in the merge queue", "add to merge queue", "/acme/widgets/pull/5/queue")
	rec = f2.post("/acme/widgets/pull/5/queue", url.Values{"base": {"main"}})
	wantStatus(t, rec, http.StatusSeeOther)
}

func TestQueueTabAndPanelAbsentWhenNotWired(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Write: true}
	f.deps(pullDeps())
	// The tab strip always offers the queue beside Pulls.
	rec := f.get("/acme/widgets/pulls")
	wantContains(t, rec, `data-tab="queue"`, `href="/acme/widgets/queue"`)
	// With no queue seam the PR page renders normally without the panel.
	rec = f.get("/acme/widgets/pull/5")
	wantStatus(t, rec, http.StatusOK)
	wantNotContains(t, rec, "Merge queue")
}

func TestQueuePageErrors(t *testing.T) {
	f := queueFixture(t, QueueDeps{
		List: func(context.Context, string, string, auth.Principal) (*mergequeue.View, error) {
			return nil, mergequeue.ErrNotFound
		},
	})
	wantStatus(t, f.get("/acme/widgets/queue"), http.StatusNotFound)

	f = queueFixture(t, QueueDeps{
		List: func(context.Context, string, string, auth.Principal) (*mergequeue.View, error) {
			return sampleQueueView(), nil
		},
		Enqueue: func(context.Context, string, string, auth.Principal, mergequeue.EnqueueInput) (*mergequeue.Entry, error) {
			return nil, mergequeue.ErrForbidden
		},
	})
	rec := f.post("/acme/widgets/queue/enqueue", url.Values{"base": {"main"}, "number": {"7"}})
	wantStatus(t, rec, http.StatusForbidden)
}

func TestQueueStateLabels(t *testing.T) {
	cases := []struct {
		state, label string
		chip         string
	}{
		{mergequeue.StatePending, "queued", "chip chip-neutral"},
		{mergequeue.StateValidating, "validating", "chip chip-draft"},
		{mergequeue.StateLanding, "landing", "chip chip-draft"},
		{mergequeue.StateLanded, "landed", "chip chip-merged"},
		{mergequeue.StateEjected, "ejected", "chip chip-closed"},
		{mergequeue.StateCancelled, "cancelled", "chip chip-neutral"},
		{"weird", "weird", "chip chip-neutral"},
	}
	for _, c := range cases {
		label, chip := queueState(c.state, 0)
		if label != c.label || chip.Cls != c.chip {
			t.Errorf("%s → %q/%q, want %q/%q", c.state, label, chip.Cls, c.label, c.chip)
		}
	}
	label, chip := queueState(mergequeue.StatePending, 3)
	if label != "queued" || chip.Text != "queued · position 3" {
		t.Fatalf("position chip = %+v", chip)
	}
}

func TestQueueWriteErrorArms(t *testing.T) {
	f := queueFixture(t, QueueDeps{
		List: func(context.Context, string, string, auth.Principal) (*mergequeue.View, error) {
			return sampleQueueView(), nil
		},
		Dequeue: func(context.Context, string, string, string, int, auth.Principal) (*mergequeue.Entry, error) {
			return nil, mergequeue.ErrConflict
		},
		Pause: func(context.Context, string, string, string, string, auth.Principal) error {
			return mergequeue.ErrForbidden
		},
		Resume: func(context.Context, string, string, string, auth.Principal) error {
			return mergequeue.ErrUnavailable
		},
	})
	// Missing base/number on dequeue renders a 400.
	if rec := f.post("/acme/widgets/queue/dequeue", url.Values{"base": {""}, "number": {"0"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("dequeue missing = %d", rec.Code)
	}
	// A service conflict maps to 409.
	if rec := f.post("/acme/widgets/queue/dequeue", url.Values{"base": {"main"}, "number": {"5"}}); rec.Code != http.StatusConflict {
		t.Fatalf("dequeue conflict = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/queue/pause", url.Values{"base": {"main"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("pause forbidden = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/queue/resume", url.Values{"base": {"main"}}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("resume unavailable = %d", rec.Code)
	}
	// A not-wired dequeue is unavailable.
	BindQueue(QueueDeps{List: func(context.Context, string, string, auth.Principal) (*mergequeue.View, error) {
		return sampleQueueView(), nil
	}})
	if rec := f.post("/acme/widgets/queue/dequeue", url.Values{"base": {"main"}, "number": {"5"}}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("dequeue not wired = %d", rec.Code)
	}
}

func TestQueuePanelStates(t *testing.T) {
	// A landed entry shows its outcome, not a dequeue control.
	f := queueFixture(t, QueueDeps{
		ForPR: func(context.Context, string, string, int, auth.Principal) (*mergequeue.PRView, error) {
			return &mergequeue.PRView{Base: "main", Entry: &mergequeue.Entry{
				Number: 5, Base: "main", Author: "alice", State: mergequeue.StateLanded,
			}}, nil
		},
	})
	rec := f.get("/acme/widgets/pull/5")
	wantContains(t, rec, "Merge queue", ">landed</span>", "add to merge queue")
	wantNotContains(t, rec, "remove from queue")

	// The author (read-only) may remove their own pending entry.
	f = queueFixture(t, QueueDeps{
		ForPR: func(context.Context, string, string, int, auth.Principal) (*mergequeue.PRView, error) {
			return &mergequeue.PRView{Base: "main", Entry: &mergequeue.Entry{
				Number: 5, Base: "main", Author: "alice", State: mergequeue.StatePending, Position: 2,
			}}, nil
		},
	})
	f.principal = auth.Principal{Name: "alice"} // write flag off; role below is read
	d := pullDeps()
	d.RoleOf = func(context.Context, string, string, auth.Principal) identity.Role { return identity.RoleRead }
	f.deps(d)
	rec = f.get("/acme/widgets/pull/5")
	wantContains(t, rec, "remove from queue")

	// A ForPR error hides the panel (the PR page still renders).
	f = queueFixture(t, QueueDeps{
		ForPR: func(context.Context, string, string, int, auth.Principal) (*mergequeue.PRView, error) {
			return nil, mergequeue.ErrUnavailable
		},
	})
	rec = f.get("/acme/widgets/pull/5")
	wantStatus(t, rec, http.StatusOK)
	wantNotContains(t, rec, "Merge queue")

	// A terminal PR hides the panel.
	f = queueFixture(t, QueueDeps{
		ForPR: func(context.Context, string, string, int, auth.Principal) (*mergequeue.PRView, error) {
			return &mergequeue.PRView{Base: "main", Entry: &mergequeue.Entry{Number: 5, State: mergequeue.StateLanded}}, nil
		},
	})
	d = pullDeps()
	orig := d.PullGet
	d.PullGet = func(ctx context.Context, owner, repo string, num int, p auth.Principal, a, n int) (*pulls.PullView, error) {
		v, err := orig(ctx, owner, repo, num, p, a, n)
		if v != nil && v.Thread != nil {
			v.Thread.State = "closed"
			if v.PR != nil {
				v.PR.Merged = true
			}
		}
		return v, err
	}
	f.deps(d)
	rec = f.get("/acme/widgets/pull/5")
	wantStatus(t, rec, http.StatusOK)
	wantNotContains(t, rec, "Merge queue")
}

func TestQueuePRDequeueErrorRendersPull(t *testing.T) {
	f := queueFixture(t, QueueDeps{
		Dequeue: func(context.Context, string, string, string, int, auth.Principal) (*mergequeue.Entry, error) {
			return nil, mergequeue.ErrConflict
		},
	})
	rec := f.post("/acme/widgets/pull/5/queue/dequeue", url.Values{"base": {"main"}})
	wantStatus(t, rec, http.StatusConflict)
	wantContains(t, rec, "conflict")
}

func TestQueueMoreErrorArms(t *testing.T) {
	f := queueFixture(t, QueueDeps{
		List: func(context.Context, string, string, auth.Principal) (*mergequeue.View, error) {
			return nil, mergequeue.ErrUnavailable
		},
	})
	if rec := f.get("/acme/widgets/queue"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("list unavailable = %d", rec.Code)
	}

	f = queueFixture(t, QueueDeps{
		List: func(context.Context, string, string, auth.Principal) (*mergequeue.View, error) {
			return sampleQueueView(), nil
		},
	})
	// Nil pause/resume seams report unavailable-ish errors, not panics.
	if rec := f.post("/acme/widgets/queue/pause", url.Values{"base": {"main"}}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil pause = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/queue/resume", url.Values{"base": {"main"}}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil resume = %d", rec.Code)
	}
	// An invalid PR path number with no form number is a 400.
	if rec := f.post("/acme/widgets/pull/abc/queue", url.Values{"base": {"main"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad path num = %d", rec.Code)
	}
	// ForPR returning (nil, nil) hides the panel.
	f = queueFixture(t, QueueDeps{
		ForPR: func(context.Context, string, string, int, auth.Principal) (*mergequeue.PRView, error) {
			return nil, nil
		},
	})
	if rec := f.get("/acme/widgets/pull/5"); rec.Code != http.StatusOK {
		t.Fatalf("nil PR view = %d", rec.Code)
	}
}

func TestQueueBaseFilterAndWiringArms(t *testing.T) {
	f := queueFixture(t, QueueDeps{
		List: func(context.Context, string, string, auth.Principal) (*mergequeue.View, error) {
			return sampleQueueView(), nil
		},
	})
	// A base with no queue still renders an empty section (a maintainer can
	// enqueue into it).
	rec := f.get("/acme/widgets/queue?base=release/1.0")
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "release/1.0", "Nothing queued on this branch")
	// Enqueue while the seam is unwired is unavailable.
	BindQueue(QueueDeps{List: func(context.Context, string, string, auth.Principal) (*mergequeue.View, error) {
		return sampleQueueView(), nil
	}})
	if rec := f.post("/acme/widgets/queue/enqueue", url.Values{"base": {"main"}, "number": {"7"}}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("enqueue not wired = %d", rec.Code)
	}
	// The panel derives its base from the PR when the entry view carries none.
	f = queueFixture(t, QueueDeps{
		ForPR: func(context.Context, string, string, int, auth.Principal) (*mergequeue.PRView, error) {
			return &mergequeue.PRView{}, nil
		},
	})
	rec = f.get("/acme/widgets/pull/5")
	wantContains(t, rec, "open the queue for main")
}
