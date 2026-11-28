package pulls

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crueber/spindle/internal/server/auth"
)

// The merge queue's pull-side seam (the Merge queue card; docs/features/22
// merge queue §4). The queue package owns ordering, claim/lease, run polling
// and the audit trail; this file owns exactly the three git/merge operations
// the queue must not reimplement:
//
//   - QueueCandidate reads the PR facts an enqueue validates against.
//   - PrepareQueueMerge builds the queue entry's temporary merge commit with
//     the SAME trial merge-tree + commit-tree path the merge task uses and
//     publishes the documented ref (refs/heads/gh-readonly-queue/...), whose
//     push starts the merge_group run (PAR-016).
//   - LandQueuedMerge lands a validated entry through runMerge — the one
//     merge path, with every gate re-checked under the task, no bypass.
//   - DeleteQueueRef removes the temporary ref on every exit path.
//
// QueueMerge always uses the `merge` strategy: the temporary ref must name a
// merge commit (a squash/rebase tip is not the tree the base branch receives),
// and the landing strategy is the same one. A base ref whose policy forbids
// `merge` refuses the entry rather than silently testing a different shape.

// QueueCandidate is the PR projection an enqueue validates.
type QueueCandidate struct {
	Num      int
	Title    string
	Author   string
	State    string
	BaseRef  string
	BaseRepo string
	HeadRef  string
	HeadRepo string
	HeadSHA  string
	BaseSHA  string
	Merged   bool
	Draft    bool
}

// QueueCandidate reads the live PR facts for enqueue. Auth: read (the queue
// service re-checks the maintain floor on the write verbs). It does not
// compute mergeability or touch git: enqueue is a queue record, and the
// worker's PrepareQueueMerge is where conflicts and policy are named.
func (s *Service) QueueCandidate(ctx context.Context, owner, repo string, num int, actor auth.Principal) (*QueueCandidate, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	if err := s.requireNotBlocked(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	th, _, err := s.loadThread(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	if th == nil || th.Kind != "pr" {
		return nil, fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	pr, _, err := s.loadPR(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	return &QueueCandidate{
		Num: num, Title: th.Title, Author: normPrincipal(th.Author), State: th.State,
		BaseRef: pr.Base.Ref, BaseRepo: pr.Base.Repo,
		HeadRef: pr.Head.Ref, HeadRepo: pr.Head.Repo, HeadSHA: pr.Head.SHA, BaseSHA: pr.Base.SHA,
		Merged: pr.Merged, Draft: pr.Draft,
	}, nil
}

// QueueMerge is one prepared queue entry: the documented ref, its tip (the
// computed merge commit) and the two parent shas the entry was built from.
// The landing call requires both parents to still be live — a moved base or
// head ejects the entry instead of landing a tree the run never checked.
type QueueMerge struct {
	Ref      string
	MergeSHA string
	BaseSHA  string
	HeadSHA  string
	Strategy string
}

// QueueRefName renders the documented merge-queue ref (PAR-016):
//
//	refs/heads/gh-readonly-queue/<base>/pr-<number>-<sha>
//
// `<base>` is the base branch's plain name (slashes allowed), `<sha>` the PR
// head commit the entry validates.
func QueueRefName(baseRef string, num int, headSHA string) string {
	return "refs/heads/gh-readonly-queue/" + strings.TrimPrefix(baseRef, "refs/heads/") +
		fmt.Sprintf("/pr-%d-%s", num, strings.ToLower(headSHA))
}

// PrepareQueueMerge computes and publishes the temporary merge commit for one
// queue entry. Auth: maintain (the landing floor; the queue re-checks it on
// every attempt). It is idempotent in effect: a leftover ref from a crashed
// previous attempt is deleted first, then the fresh commit is published with
// its objects atomically (the run's clone must find them).
//
// No lock is held across the git subprocesses or the WAL publish (13 §2.4);
// arbitration is the entry's doc CAS in the queue package plus the WAL
// manifest CAS on the ref create.
func (s *Service) PrepareQueueMerge(ctx context.Context, owner, repo string, num int, actor auth.Principal) (*QueueMerge, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, repo, actor, "maintain"); err != nil {
		return nil, err
	}
	if err := s.requireNotBlocked(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	if s.Git == nil || s.Dirs == nil || s.Refs == nil {
		return nil, fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	who := normPrincipal(actor.Name)
	th, _, err := s.loadThread(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	if th == nil || th.Kind != "pr" {
		return nil, fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	pr, _, err := s.loadPR(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	if pr.Merged {
		return nil, fmt.Errorf("%w: pull request #%d already merged as %s", ErrConflict, num, strOrEmpty(pr.MergeCommitSHA))
	}
	if th.State == StateClosed {
		return nil, fmt.Errorf("%w: pull request #%d is closed", ErrConflict, num)
	}
	if pr.Draft {
		return nil, fmt.Errorf("%w: pull request #%d is a draft", ErrConflict, num)
	}
	baseDir, err := s.Dirs.Dir(ctx, pr.Base.Repo)
	if err != nil {
		return nil, fmt.Errorf("%w: base repo unavailable: %v", ErrUnavailable, err)
	}
	headDir, err := s.Dirs.Dir(ctx, pr.Head.Repo)
	if err != nil {
		return nil, fmt.Errorf("%w: head repo unavailable: %v", ErrUnavailable, err)
	}
	baseLive, err := s.Git.ResolveRef(ctx, baseDir, pr.Base.Ref)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown base revision %q", ErrUnprocessable, pr.Base.Ref)
	}
	headLive, err := s.Git.ResolveRef(ctx, headDir, pr.Head.Ref)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown head revision %q", ErrUnprocessable, pr.Head.Ref)
	}
	if pr.Head.Repo != pr.Base.Repo {
		if ferr := s.Git.FetchInto(ctx, baseDir, headDir, headLive); ferr != nil {
			return nil, fmt.Errorf("%w: fork object bridge: %v", ErrUnavailable, ferr)
		}
	}
	m, err := s.ComputeMergeable(ctx, owner, repo, num)
	if err != nil {
		return nil, err
	}
	switch m.State {
	case MergeableDirty:
		return nil, fmt.Errorf("%w: pull request #%d has conflicts in %s", ErrConflict, num, strings.Join(m.Conflicts, ", "))
	case MergeableUpToDate:
		return nil, fmt.Errorf("%w: pull request #%d is already merged (up to date)", ErrConflict, num)
	}
	gate, gerr := s.loadMergeGate(ctx, owner, repo, pr.Base.Ref, who)
	if gerr != nil {
		return nil, fmt.Errorf("%w: %v", ErrConflict, gerr)
	}
	if merr := checkMergeMethod(gate, StrategyMerge, pr.Base.Ref); merr != nil {
		return nil, merr
	}
	if gate.RequireUpToDate {
		contained, cerr := s.Git.IsAncestor(ctx, baseDir, baseLive, headLive)
		if cerr != nil {
			return nil, cerr
		}
		if uerr := checkMergeUpToDate(gate, contained, pr.Base.Ref); uerr != nil {
			return nil, uerr
		}
	}
	tree, conflicts, terr := s.Git.TrialMerge(ctx, baseDir, baseLive, headLive)
	if terr != nil {
		if errors.Is(terr, errDirty) {
			return nil, fmt.Errorf("%w: pull request #%d has conflicts in %s", ErrConflict, num, strings.Join(conflicts, ", "))
		}
		return nil, terr
	}
	subject, _ := s.Git.Subject(ctx, headDir, headLive)
	title, body := mergeMessage(StrategyMerge, num, pr.Head.Ref, subject, "", "", "")
	cName, cEmail := s.committer()
	mergeSHA, err := s.Git.CommitTree(ctx, baseDir, tree, []string{baseLive, headLive},
		fullMessage(title, body), who, who, cName, cEmail, s.nowUTC())
	if err != nil {
		return nil, err
	}
	ref := QueueRefName(pr.Base.Ref, num, headLive)
	// A previous attempt (a crash between publish and the queue's doc CAS)
	// may have left the ref behind; delete it so the create is deterministic.
	// A refusal here is not fatal: the create below names the real CAS state.
	_ = s.Refs.DeleteRef(ctx, pr.Base.Repo, ref, map[string]string{"principal": who, "agent": "merge-queue"})
	rec := &TaskRecord{}
	meta := map[string]string{"principal": who, "agent": "merge-queue"}
	zero := strings.Repeat("0", len(baseLive))
	if perr := s.publishWithObjects(ctx, pr.Base.Repo, ref, zero, mergeSHA, baseDir, meta, rec, "merge-queue ref"); perr != nil {
		return nil, fmt.Errorf("%w: publish queue ref: %v", ErrUnavailable, perr)
	}
	return &QueueMerge{Ref: ref, MergeSHA: mergeSHA, BaseSHA: baseLive, HeadSHA: headLive, Strategy: StrategyMerge}, nil
}

// DeleteQueueRef deletes a merge-queue ref. It is idempotent: a missing ref
// is success (the queue calls it on every exit path and on recovery). Only
// refs under the documented prefix are accepted, so a caller cannot use this
// seam to delete a real branch.
func (s *Service) DeleteQueueRef(ctx context.Context, owner, repo, ref string) error {
	if !strings.HasPrefix(ref, "refs/heads/gh-readonly-queue/") {
		return fmt.Errorf("%w: %q is not a merge-queue ref", ErrForbidden, ref)
	}
	if s.Refs == nil {
		return fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	if err := s.Refs.DeleteRef(ctx, repoName(owner, repo), ref, map[string]string{"principal": "spindle", "agent": "merge-queue"}); err != nil {
		if isPrecondition(err) {
			return nil
		}
		return err
	}
	return nil
}

// LandQueuedMerge lands a validated queue entry through the one merge path.
// The queue has already waited for the merge_group run and the required
// checks on the queue head; this call re-runs every gate under the task
// (role, policy, merge method, up-to-date, required reviews, required checks
// on the QUEUE head, the protected-ref CAS) and NEVER bypasses one.
//
// ExpectedBaseSHA/ExpectedHeadSHA make the landing exactly the tree the queue
// checked: if either parent moved meanwhile, the merge refuses and the queue
// ejects with that reason instead of landing an untested merge.
//
// The (repo, pull-merge) task single-flight still serializes with a human
// merge: if one is running, this call waits for it and then takes its turn.
func (s *Service) LandQueuedMerge(ctx context.Context, owner, repo string, num int, actor auth.Principal, q *QueueMerge) (*TaskRecord, error) {
	if q == nil {
		return nil, fmt.Errorf("%w: no prepared merge", ErrInvalid)
	}
	if err := s.requireNotBlocked(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	repoID := repoName(owner, repo)
	for attempt := 0; attempt < 3; attempt++ {
		entry, joined := s.tasks.begin(repoID, TaskKindMerge)
		if joined {
			select {
			case <-ctx.Done():
				return entry.rec.snapshot(), ctx.Err()
			default:
			}
			// Wait for the in-flight merge (a human's, or another entry's)
			// without holding any lock; then re-check whether ours landed.
			entry.wg.Wait()
			if pr, _, perr := s.loadPR(ctx, owner, repo, num); perr == nil && pr != nil && pr.Merged {
				return entry.rec.snapshot(), nil
			}
			continue
		}
		return s.runQueuedMerge(ctx, entry, repoID, owner, repo, num, actor, q)
	}
	return nil, fmt.Errorf("%w: another merge kept the task slot; retry", ErrConflict)
}

// runQueuedMerge runs the leader's merge task for one queue entry and always
// completes the task-table entry (the joiners wait on it).
func (s *Service) runQueuedMerge(ctx context.Context, entry *taskEntry, repoID, owner, repo string, num int, actor auth.Principal, q *QueueMerge) (rec *TaskRecord, rerr error) {
	defer s.tasks.end(repoID, TaskKindMerge)
	entry.rec.initMerge(num, q.Strategy)
	entry.rec.notice("merge queue landing %s (queue head %s)", threadTitleOf(owner, repo, num), shortSHA(q.MergeSHA))
	bctx := context.WithoutCancel(ctx)
	outcome, err := s.runMerge(bctx, owner, repo, num, actor, MergeInput{
		Strategy:        q.Strategy,
		ExpectedBaseSHA: q.BaseSHA,
		ExpectedHeadSHA: q.HeadSHA,
		QueueHeadSHA:    q.MergeSHA,
	}, "merge-queue", entry.rec)
	if err != nil {
		entry.rec.setState(TaskError, "", err.Error(), nil)
		entry.rec.notice("merge queue landing failed: %s", err.Error())
		return entry.rec.snapshot(), err
	}
	entry.rec.setState(TaskOK, fmt.Sprintf("merged %s as %s (merge queue)", threadTitleOf(owner, repo, num), outcome["sha"]), "", outcome)
	return entry.rec.snapshot(), nil
}
