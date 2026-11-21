package pulls

import (
	"errors"
	"sync"
	"testing"
)

// Regression tests for issue #64: pr.json CAS retries (and stale-struct
// saves) must never clobber a concurrently-landed merge outcome. The merge
// outcome fields (merged/merged_at/merged_by/merge_commit_sha/
// merge_strategy) are write-once/monotonic: once merged:true, no writer may
// unset them.
//
// Since the owned-delta re-apply audit (§2.3 write discipline, TestSink
// `head push refreshes sha` flake), production writers go through updatePR —
// the narrow updater that re-loads the freshest doc and re-applies its owned
// delta on every attempt. Whole-doc savePR is the explicit create/test
// writer and refuses a stale version with ErrConflict instead of merging a
// stale struct onto the live doc (the old reapplyPR wholesale copy, which
// resurrected a stale Head.SHA).

// landMerge records a merge outcome through the merge task's write path
// (the narrow updater owning the outcome fields).
func landMerge(t *testing.T, e *testEnv, sha string) {
	t.Helper()
	at, by, strategy := "2026-09-04T12:30:00Z", "merger@example.com", StrategyMerge
	if _, err := e.svc.updatePR(ctx(), "o", "r", 1, func(cur *PRDoc) (bool, error) {
		if cur.Merged {
			return false, nil
		}
		cur.Merged = true
		cur.MergedAt = &at
		cur.MergedBy = &by
		cur.MergeCommitSHA = &sha
		cur.MergeStrategy = &strategy
		return true, nil
	}); err != nil {
		t.Fatalf("landMerge updatePR: %v", err)
	}
}

func checkMerged(t *testing.T, e *testEnv, sha string) *PRDoc {
	t.Helper()
	got, _, err := e.svc.loadPR(ctx(), "o", "r", 1)
	if err != nil || got == nil {
		t.Fatalf("loadPR: %v %v", got, err)
	}
	if !got.Merged || got.MergeCommitSHA == nil || *got.MergeCommitSHA != sha ||
		got.MergedAt == nil || got.MergedBy == nil || got.MergeStrategy == nil {
		t.Fatalf("merge outcome clobbered: %+v", got)
	}
	return got
}

// A head-update writer that loaded the PR before a merge landed must
// preserve the landed outcome while still applying its head delta.
func TestSavePRRetryPreservesLandedMerge(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	stale, _, err := e.svc.loadPR(ctx(), "o", "r", 1)
	if err != nil {
		t.Fatalf("loadPR: %v", err)
	}
	if stale == nil {
		t.Fatal("no pr")
	}
	landMerge(t, e, hexSHA(9))
	// The head delta is a live-read move: it must descend from the recorded
	// head to apply (forward-only), which every read/publish path now enforces.
	e.git.Ancestors[hexSHA(2)+"\x00"+hexSHA(3)] = true
	// Force the retry path so the head delta is re-applied onto the fresh
	// (merged) doc rather than the writer's pre-merge snapshot.
	e.failPut(PRKey("o", "r", 1), 1)
	e.svc.refreshHead(ctx(), "o", "r", stale, mustThread(t, e), hexSHA(3), writer())
	got := checkMerged(t, e, hexSHA(9))
	if got.Head.SHA != hexSHA(3) {
		t.Fatalf("head delta lost: %s", got.Head.SHA)
	}
	if got.Body != "fixes #3" {
		t.Fatalf("body changed: %q", got.Body)
	}
}

// A merge-completion writer must preserve concurrently-landed editorial
// fields while recording the outcome (including across a CAS retry).
func TestSavePRMergeRetryPreservesFreshFields(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	// A body edit lands first through the narrow updater.
	body := "edited description"
	if _, err := e.svc.updatePR(ctx(), "o", "r", 1, func(cur *PRDoc) (bool, error) {
		cur.Body = body
		return true, nil
	}); err != nil {
		t.Fatalf("body updatePR: %v", err)
	}
	// The merge completion retries (forced 412) onto the edited doc.
	e.failPut(PRKey("o", "r", 1), 1)
	landMerge(t, e, hexSHA(9))
	got := checkMerged(t, e, hexSHA(9))
	if got.Body != body {
		t.Fatalf("body edit clobbered: %q", got.Body)
	}
}

// refreshHead saves through the narrow updater with a potentially STALE
// snapshot: the landed outcome must survive the direct write.
func TestRefreshHeadPreservesLandedMerge(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	stale, _, err := e.svc.loadPR(ctx(), "o", "r", 1)
	if err != nil {
		t.Fatalf("loadPR: %v", err)
	}
	landMerge(t, e, hexSHA(9))
	// The head delta is a live-read move, so it must descend from the recorded
	// head to apply (the same forward-only rule every read path uses).
	e.git.Ancestors[hexSHA(2)+"\x00"+hexSHA(5)] = true
	e.svc.refreshHead(ctx(), "o", "r", stale, mustThread(t, e), hexSHA(5), writer())
	got := checkMerged(t, e, hexSHA(9))
	if got.Head.SHA != hexSHA(5) {
		t.Fatalf("head delta lost: %s", got.Head.SHA)
	}
}

// The exact defect that flaked TestSink: writer A loads the PR, writer B
// records a new head through the narrow updater, then A writes its stale
// snapshot. B's head must survive and A's own owned field must still land.
func TestStaleWriterCannotResurrectHead(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	stale, staleVer, err := e.svc.loadPR(ctx(), "o", "r", 1)
	if err != nil || stale == nil {
		t.Fatalf("loadPR: %v %v", stale, err)
	}
	// Writer B: a concurrent head refresh.
	if _, err := e.svc.updatePR(ctx(), "o", "r", 1, func(cur *PRDoc) (bool, error) {
		if cur.Head.SHA == hexSHA(7) {
			return false, nil
		}
		cur.Head.SHA = hexSHA(7)
		return true, nil
	}); err != nil {
		t.Fatalf("B updatePR: %v", err)
	}
	// Writer A: whole-doc save of the stale snapshot is REFUSED (a stale
	// whole doc has no owned delta to re-apply), so it cannot write B's head
	// back to hexSHA(2).
	if err := e.svc.savePR(ctx(), "o", "r", stale, staleVer); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale whole-doc save = %v, want ErrConflict", err)
	}
	got, _, err := e.svc.loadPR(ctx(), "o", "r", 1)
	if err != nil || got == nil {
		t.Fatalf("loadPR: %v %v", got, err)
	}
	if got.Head.SHA != hexSHA(7) {
		t.Fatalf("stale writer resurrected the head: %s", got.Head.SHA)
	}
	// Writer A's owned delta still lands, without touching B's head.
	if _, err := e.svc.updatePR(ctx(), "o", "r", 1, func(cur *PRDoc) (bool, error) {
		cur.Body = "from a"
		return true, nil
	}); err != nil {
		t.Fatalf("A updatePR: %v", err)
	}
	got, _, err = e.svc.loadPR(ctx(), "o", "r", 1)
	if err != nil || got == nil {
		t.Fatalf("loadPR: %v %v", got, err)
	}
	if got.Head.SHA != hexSHA(7) || got.Body != "from a" {
		t.Fatalf("final = head %s body %q", got.Head.SHA, got.Body)
	}
}

// An update that hits repeated 412s re-applies onto the newest doc each
// attempt: two interleaved narrow updates of disjoint fields lose nothing.
// Ordering is explicit (the concurrent write happens inside the retried
// writer's mutate, before its PUT), so no sleeps and no timing assumptions.
func TestUpdatePRRetriesReapplyOntoNewest(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	attempts := 0
	got, err := e.svc.updatePR(ctx(), "o", "r", 1, func(cur *PRDoc) (bool, error) {
		attempts++
		var err error
		switch attempts {
		case 1: // a head refresh lands between our load and our PUT
			_, err = e.svc.updatePR(ctx(), "o", "r", 1, func(c *PRDoc) (bool, error) {
				c.Head.SHA = hexSHA(5)
				return true, nil
			})
		case 2: // a draft flip lands between our second load and PUT
			_, err = e.svc.updatePR(ctx(), "o", "r", 1, func(c *PRDoc) (bool, error) {
				c.Draft = true
				return true, nil
			})
		}
		if err != nil {
			return false, err
		}
		cur.Body = "retried body"
		return true, nil
	})
	if err != nil {
		t.Fatalf("updatePR: %v", err)
	}
	if attempts < 3 {
		t.Fatalf("expected >=3 attempts (two 412 retries), got %d", attempts)
	}
	// The returned doc and the stored doc must both carry every field.
	for _, doc := range []*PRDoc{got, mustLoadPR(t, e)} {
		if doc.Head.SHA != hexSHA(5) || !doc.Draft || doc.Body != "retried body" {
			t.Fatalf("lost field: head %s draft %v body %q", doc.Head.SHA, doc.Draft, doc.Body)
		}
	}
}

// Unmerged narrow updates of the same field stay last-writer-wins.
func TestSavePRRetryLastWriterWinsWhenUnmerged(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	if _, err := e.svc.updatePR(ctx(), "o", "r", 1, func(cur *PRDoc) (bool, error) {
		cur.Body = "from b"
		return true, nil
	}); err != nil {
		t.Fatalf("b updatePR: %v", err)
	}
	if _, err := e.svc.updatePR(ctx(), "o", "r", 1, func(cur *PRDoc) (bool, error) {
		cur.Body = "from a"
		return true, nil
	}); err != nil {
		t.Fatalf("a updatePR: %v", err)
	}
	got := mustLoadPR(t, e)
	if got.Body != "from a" || got.Merged {
		t.Fatalf("got = %+v", got)
	}
}

// Once merged, concurrent body/head churn must never unset the outcome.
func TestMergeOutcomeMonotonicUnderChurn(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	landMerge(t, e, hexSHA(9))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Bounded client-style retry on 412: contention may exhaust
			// one CAS window; the assertion is monotonicity of the
			// outcome, not first-try success.
			for attempt := 0; attempt < 20; attempt++ {
				body := "churn"
				_, _, err := e.svc.UpdatePR(ctx(), "o", "r", 1, writer(), PRPatch{Body: &body})
				if err == nil {
					break
				}
				if !errors.Is(err, ErrConflict) {
					t.Errorf("UpdatePR: %v", err)
					break
				}
			}
			if th, _, _ := e.svc.loadThread(ctx(), "o", "r", 1); th != nil {
				if live, _, lerr := e.svc.loadPR(ctx(), "o", "r", 1); lerr == nil && live != nil {
					e.svc.refreshHead(ctx(), "o", "r", live, th, hexSHA(20+i), writer())
				}
			}
		}(i)
	}
	wg.Wait()
	checkMerged(t, e, hexSHA(9))
}

// A live-ref read taken during a mergeability recompute is forward-only: a
// serving copy that lags the WAL event which recorded the current head must
// not move Head.SHA back (the second arm of the TestSink flake — the
// recompute re-resolved the pre-push sha after the sink had recorded the
// push). A genuine advance still lands.
func TestReconcileHeadForwardOnly(t *testing.T) {
	e := newTestEnv()
	openBasic(t, e, "o", "r")
	th := mustThread(t, e)
	pr := mustLoadPR(t, e)
	// The sink records a fast-forward move 2 → 3 (event-authoritative).
	e.git.Ancestors[hexSHA(2)+"\x00"+hexSHA(3)] = true
	e.git.Ancestors[hexSHA(3)+"\x00"+hexSHA(4)] = true
	e.svc.refreshHead(ctx(), "o", "r", pr, th, hexSHA(3), writer())
	if got := mustLoadPR(t, e); got.Head.SHA != hexSHA(3) {
		t.Fatalf("sink head = %s", got.Head.SHA)
	}
	// A lagging live read still says 2: it is behind the recorded head.
	e.svc.reconcileHead(ctx(), "o", "r", mustLoadPR(t, e), th, hexSHA(2))
	if got := mustLoadPR(t, e); got.Head.SHA != hexSHA(3) {
		t.Fatalf("lagging read rewrote the head: %s", got.Head.SHA)
	}
	// A diverged read (not a descendant) is equally refused.
	e.svc.reconcileHead(ctx(), "o", "r", mustLoadPR(t, e), th, hexSHA(6))
	if got := mustLoadPR(t, e); got.Head.SHA != hexSHA(3) {
		t.Fatalf("diverged read rewrote the head: %s", got.Head.SHA)
	}
	// A real advance (3 → 4) lands, without force-push evidence.
	e.svc.reconcileHead(ctx(), "o", "r", mustLoadPR(t, e), th, hexSHA(4))
	got := mustLoadPR(t, e)
	if got.Head.SHA != hexSHA(4) {
		t.Fatalf("advance lost: %s", got.Head.SHA)
	}
	if got.HeadForcePushedAt != nil {
		t.Fatalf("fast-forward stamped as forced: %v", *got.HeadForcePushedAt)
	}
}

func mustLoadPR(t *testing.T, e *testEnv) *PRDoc {
	t.Helper()
	pr, _, err := e.svc.loadPR(ctx(), "o", "r", 1)
	if err != nil || pr == nil {
		t.Fatalf("loadPR: %v %v", pr, err)
	}
	return pr
}

func mustThread(t *testing.T, e *testEnv) *Thread {
	t.Helper()
	th, _, err := e.svc.loadThread(ctx(), "o", "r", 1)
	if err != nil || th == nil {
		t.Fatalf("loadThread: %v %v", th, err)
	}
	return th
}
