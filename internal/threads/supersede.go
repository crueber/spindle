package threads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/crueber/spindle/internal/store"
)

// Implicit supersede (PAR-150, docs/threads.md §3.1a).
//
// Forgejo's rule: a new push to a ref, and a PR `synchronize`, cancel the
// in-flight run of the commit it supersedes when the workflow declares no
// `concurrency:` block. Without it two rapid pushes race the same deploy; with
// it the older run reads `cancelled` and names the run that replaced it.
//
// The supersede head is one small record per (workflow file, ref):
//
//	repos/<o>/<r>/threads/supersede/<wf-slug>-<hash8>.<ref-slug>-<hash8>.json
//
// It holds the newest run id created for that pair, and it is CAS'd
// forward-only on the run id (ids are time-ordered, so "newer" is a string
// compare). That single rule gives the whole feature:
//
//   - the writer that advances the head supersedes the run it displaced;
//   - the writer that finds a NEWER head already there has itself been
//     superseded, and cancels itself;
//   - no read-then-cancel window can leave two group-free runs of one ref live,
//     because the loser always learns it lost.
//
// The head is adopted AFTER the run header is committed and the cancel is
// applied last, so a superseded run always has a visible header (the run
// header stays the run's commit point) and cancellation can never strand a run
// that was not created.
//
// ### Concurrency
//
// Hazard: two creations for one ref racing on the head record. Avoidance: the
// CAS on the head is the lock (the store's conditional PUT); a loser re-reads
// and learns it is superseded, so the convergence point is a single live id.
// Hazard: a supersede that stalls the creation it belongs to. Avoidance: the
// head CAS is one GET + one conditional PUT issued in its own goroutine
// concurrently with the checks mirror and the enqueue — it is never in series
// with the job writes — and its failure is logged, never fatal (the run exists
// and the shared, documented cost is one extra pass of two rapid pushes).
// Nothing here takes syncMu/packMu/rw, and the goroutine always exits through
// the context or the join.
//
// Request budget (law 6): a group-free push / PR-synchronize run issues
// exactly ONE GET and ONE conditional PUT of this record, overlapping the
// checks mirror writes that already follow the header; the common case is the
// only new cost. Every other event (workflow_dispatch, schedule, PR opened or
// reopened, and any run whose workflow declares a concurrency group) issues
// none — the workflow declares no group and the event does not supersede, so
// no head is adopted. Cancelling a displaced run is the pre-existing terminal
// path (load header, load jobs, one CAS per live job, fold), which only runs
// when there IS an older live run — exactly the work supersede exists to do.

// SupersedePrefix is the supersede-head family of one repository.
func SupersedePrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/threads/supersede/"
}

// SupersedeKey is the head record for one (workflow file, ref) pair. Both
// segments carry a hash suffix so two pairs whose slugs collide never share an
// entry.
func SupersedeKey(owner, repo, workflowPath, ref string) string {
	return SupersedePrefix(owner, repo) + headSegment(workflowPath) + "." + headSegment(ref) + ".json"
}

// headSegment maps a free-form key part onto one key segment.
func headSegment(s string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(s)))
	return slugSegment(s) + "-" + hex.EncodeToString(sum[:4])
}

// SupersedeDoc is one (workflow, ref) head record.
type SupersedeDoc struct {
	Workflow string `json:"workflow"`
	Ref      string `json:"ref"`
	// RunID is the newest run created for this pair ("" = none).
	RunID  string `json:"run_id"`
	Commit string `json:"commit"`
	Since  string `json:"since"`
	// Version is the CAS counter (diagnostics; the store's own version is what
	// the write compares).
	Version int `json:"version"`
}

// supersedePending is one in-flight head adoption: the channel its result
// arrives on, and whether this run also displaces the run it replaced.
type supersedePending struct {
	ch        chan supersedeResult
	displaces bool
}

// supersedeResult is the outcome of adopting the head for one new run.
type supersedeResult struct {
	// prev is the run the new one displaced ("" when the head was free).
	prev string
	// newer is the run that already owned the head and therefore supersedes
	// THIS one ("" in the normal case).
	newer string
	err   error
}

// supersedeAdopt reports whether a run created for this trigger registers the
// (workflow, ref) head at all. A push and every pull_request edge do — a PR's
// first run must be visible to the synchronize that replaces it — while a
// dispatch and a schedule are explicit acts outside the chain.
func supersedeAdopt(event string) bool {
	return event == EventPush || event == EventPullReq
}

// supersedeDisplaces reports whether this run cancels the run it displaced. A
// push always does; a pull_request only on `synchronize`, where the head moved
// and the previous head's work is stale. `opened` and `reopened` register the
// head (so a later synchronize can name them) but cancel nothing: reopening a
// PR is not a new commit.
func supersedeDisplaces(event, action string) bool {
	switch event {
	case EventPush:
		return true
	case EventPullReq:
		return strings.EqualFold(strings.TrimSpace(action), "synchronize")
	}
	return false
}

// startSupersede adopts the supersede head for a just-committed run, in its own
// goroutine, and returns the (buffered) channel its result arrives on, or nil
// when this run does not participate (see supersedeEvent and the group rule).
//
// A run whose workflow declares a concurrency group is skipped: its group owns
// serialisation (PAR-152), and silently cancelling it would make
// `cancel-in-progress: false` a lie. That is the reconciliation between the two
// rules — implicit supersede is the group-free default, the group is the
// explicit override.
func (s *Service) startSupersede(ctx context.Context, owner, repo string, run *Run, event, action string) *supersedePending {
	if run == nil || !supersedeAdopt(event) {
		return nil
	}
	if len(run.Concurrency) > 0 {
		return nil // the workflow's own group governs (PAR-152)
	}
	if strings.TrimSpace(run.Ref) == "" {
		return nil // no ref, no supersede key
	}
	ch := make(chan supersedeResult, 1)
	go func() {
		prev, newer, err := s.adoptRunHead(ctx, owner, repo, run.WorkflowPath, run.Ref, run.ID, run.Commit)
		ch <- supersedeResult{prev: prev, newer: newer, err: err}
	}()
	return &supersedePending{ch: ch, displaces: supersedeDisplaces(event, action)}
}

// adoptRunHead advances the (workflow, ref) head to runID and reports what it
// displaced. Forward-only: a head that already names a lexically greater run id
// (created later) is left alone and returned as `newer`.
func (s *Service) adoptRunHead(ctx context.Context, owner, repo, workflowPath, ref, runID, commit string) (prev, newer string, err error) {
	key := SupersedeKey(owner, repo, workflowPath, ref)
	for attempt := 0; attempt < 4; attempt++ {
		raw, ver, gerr := s.getJSON(ctx, key)
		if gerr != nil {
			return "", "", gerr
		}
		doc := &SupersedeDoc{Workflow: workflowPath, Ref: ref}
		if raw != nil {
			if jerr := json.Unmarshal(raw, doc); jerr != nil {
				return "", "", fmt.Errorf("%w: supersede %s: %v", ErrCorrupt, key, jerr)
			}
		}
		switch {
		case doc.RunID == runID:
			return "", "", nil // already ours (a retried creation)
		case doc.RunID > runID:
			return "", doc.RunID, nil // a newer run owns the head: we lost
		}
		displaced := doc.RunID
		doc.RunID = runID
		doc.Commit = commit
		doc.Since = s.nowUTC().Format(dateTimeFmt)
		doc.Version++
		body, _ := json.Marshal(doc) // cannot fail: the record is pure JSON types
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		if ver == "" {
			opts.Mode = store.PutCreate
		}
		if _, perr := store.PutBytes(ctx, s.Store, key, body, opts); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue // another creation won; re-read and settle
			}
			return "", "", perr
		}
		return displaced, "", nil
	}
	// Four lost CASes in a row: concede rather than spin. The next creation for
	// this ref settles the head; nothing is left inconsistent (a missed
	// supersede costs one racing run, never a corrupt record).
	return "", "", nil
}

// finishSupersede joins the adoption started by startSupersede and applies its
// verdict: cancel the run this one displaced, or cancel this one when a newer
// run already owns the ref. It returns the reloaded header when THIS run was
// cancelled, so the caller never returns a stale record.
//
// Every failure here is logged, never returned: the run exists and is the
// commit point; a supersede bookkeeping outage must not turn a created run into
// an error the trigger hook reports as "no run".
func (s *Service) finishSupersede(ctx context.Context, owner, repo string, run *Run, pending *supersedePending) *Run {
	if pending == nil || run == nil {
		return nil
	}
	res := <-pending.ch
	if res.err != nil {
		slog.Warn("threads: supersede head", "repo", repoID(owner, repo), "run", run.ID, "err", res.err)
		return nil
	}
	if res.newer != "" {
		updated, err := s.cancelRun(ctx, owner, repo, run.ID, cancelWhy{
			Reason:       CancelReasonSupersede,
			SupersededBy: res.newer,
		})
		if err != nil {
			slog.Warn("threads: cancel superseded run", "repo", repoID(owner, repo), "run", run.ID, "newer", res.newer, "err", err)
			return nil
		}
		return updated
	}
	if res.prev == "" || !pending.displaces {
		// A run that registers the head without displacing (a PR `opened` or
		// `reopened`) leaves its predecessor alone.
		return nil
	}
	prev, err := s.loadRun(ctx, owner, repo, res.prev)
	if err != nil {
		slog.Warn("threads: load superseded run", "repo", repoID(owner, repo), "run", res.prev, "err", err)
		return nil
	}
	switch {
	case prev == nil:
		return nil // created and already reclaimed: nothing to cancel
	case terminal(prev.State):
		return nil // it ended on its own; a terminal run is not "in flight"
	case len(prev.Concurrency) > 0:
		// The displaced run declares a group: its own admission rule decides
		// whether it queues or runs (PAR-152). Never cancel it implicitly.
		return nil
	case prev.WorkflowPath != run.WorkflowPath:
		return nil // defensive: the key already separates workflow files
	}
	if _, cerr := s.cancelRun(ctx, owner, repo, prev.ID, cancelWhy{
		Reason:       CancelReasonSupersede,
		SupersededBy: run.ID,
	}); cerr != nil {
		slog.Warn("threads: supersede run", "repo", repoID(owner, repo), "run", prev.ID, "by", run.ID, "err", cerr)
	}
	return nil
}
