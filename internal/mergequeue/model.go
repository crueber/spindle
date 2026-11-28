// Package mergequeue implements the merge queue (the Merge queue card): a
// per-base-branch FIFO queue of pull requests that are validated by a
// merge_group run against a temporary merge commit and landed through the one
// merge path when the required checks are green.
//
// The queue is bucket state, never process state: one JSON document per
// repository (repos/<o>/<r>/merge_queue.json) holds every base branch's
// ordered entries, the pause flag, a bounded recent-outcome list and a bounded
// audit trail. Every mutation is a CAS on that document; a lost CAS reloads
// and re-applies (docs/features/22_merge_queue.md §3).
//
// The hand-off to CI is the documented temporary ref
// refs/heads/gh-readonly-queue/<base>/pr-<n>-<sha> (PAR-016). Pushing it
// starts exactly one merge_group run whose jobs mirror checks onto the ref
// tip; the queue polls for that run and the required-checks gate, then lands
// the PR with pulls.LandQueuedMerge (which re-checks every protection gate —
// the queue is never a bypass).
//
// Ordering is priority-then-FIFO: an entry enqueued with a higher priority
// runs first; entries of equal priority run in enqueue order (Seq). Enqueue
// itself is maintain-only, so priority is a maintainer's ordering tool, not a
// contributor's.
package mergequeue

import (
	"sort"
	"strconv"
	"time"
)

// DocKey is the queue document for one repository (the only queue key).
func DocKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/merge_queue.json"
}

// Entry states. A terminal state never transitions again; the PR stays open
// on every non-landed outcome (the queue never closes the PR).
const (
	StatePending    = "pending"    // waiting for its turn; Position is live
	StateValidating = "validating" // temp ref pushed, merge_group run in flight
	StateLanding    = "landing"    // checks green, the merge task is landing it
	StateLanded     = "landed"     // merged through the one merge path
	StateEjected    = "ejected"    // failed/conflicted/refused: reason recorded
	StateCancelled  = "cancelled"  // dequeued by a person
)

// DefaultPriority is the enqueue priority when the request omits one.
const DefaultPriority = 0

// MaxPriority bounds the ordering knob (higher = earlier). The queue is a
// human-rate surface; the bound keeps a typo from parking an entry forever.
const MaxPriority = 100

// Doc is repos/<o>/<r>/merge_queue.json: every base queue for one repo, the
// recent outcomes (one per PR+base) and the bounded audit trail.
type Doc struct {
	Version int      `json:"version"`
	Repo    string   `json:"repo"`
	Queues  []*Queue `json:"queues"`
	// Credentials is the recorded enqueue principal per active entry
	// (base#number). It is deliberately NOT part of Entry: entries are the
	// wire/read shape (rendered and returned by the API), and the recorded
	// credential must never be emitted — it carries the enqueuer's email and
	// verified groups. The landing task re-checks the role against it.
	Credentials map[string]authPrincipal `json:"credentials,omitempty"`
	Recent      []*Entry                 `json:"recent,omitempty"`
	Audit       []AuditEntry             `json:"audit,omitempty"`
	UpdatedAt   string                   `json:"updated_at"`
}

// Queue is one base branch's ordered entries plus its pause state.
type Queue struct {
	Base         string   `json:"base"` // plain branch name ("main", "release/1.0")
	Paused       bool     `json:"paused"`
	PausedBy     string   `json:"paused_by,omitempty"`
	PausedReason string   `json:"paused_reason,omitempty"`
	PausedAt     string   `json:"paused_at,omitempty"`
	NextSeq      int64    `json:"next_seq"` // monotonic enqueue order
	Entries      []*Entry `json:"entries"`
}

// Entry is one queued pull request.
type Entry struct {
	Number   int    `json:"number"`
	Base     string `json:"base"` // plain base branch name (the queue it belongs to)
	Title    string `json:"title,omitempty"`
	Author   string `json:"author"`   // pull request author (may dequeue it)
	Enqueuer string `json:"enqueuer"` // maintainer who enqueued it
	Priority int    `json:"priority"`
	Seq      int64  `json:"seq"` // enqueue order within the base queue

	State      string `json:"state"`
	EnqueuedAt string `json:"enqueued_at"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`

	// Ref/MergeSHA/BaseSHA/HeadSHA describe the prepared entry. HeadSHA is
	// the PR head the ref name encodes; MergeSHA is the merge commit the
	// merge_group run checks. BaseSHA is the base tip it was built on.
	Ref      string `json:"ref,omitempty"`
	MergeSHA string `json:"merge_sha,omitempty"`
	BaseSHA  string `json:"base_sha,omitempty"`
	HeadSHA  string `json:"head_sha,omitempty"`
	Strategy string `json:"strategy,omitempty"`

	// ClaimID/ClaimedAt are the lease a worker writes when it takes the head.
	// Every later write verifies its own claim, so a stale worker (a restart,
	// another instance) can never land or eject an entry it no longer owns.
	ClaimID   string `json:"claim_id,omitempty"`
	ClaimedAt string `json:"claimed_at,omitempty"`
	Attempts  int    `json:"attempts,omitempty"`
	RunID     string `json:"run_id,omitempty"`

	// Reason is the named outcome for a terminal entry: why it was ejected,
	// cancelled or recovered. It is the text the PR panel renders.
	Reason string `json:"reason,omitempty"`

	// Position is DERIVED on a read copy (1-based place in the base queue's
	// effective order; 0 for terminal rows). It is never stored: the stored
	// entry carries zero, and every view fill sets it from the effective
	// order at read time.
	Position int `json:"position,omitempty"`
}

// authPrincipal is the JSON projection of the recorded enqueue credential.
// The type mirrors auth.Principal's fields the queue needs; it is defined
// here (not as auth.Principal) so a document never silently grows a field a
// future credential adds.
type authPrincipal struct {
	Name   string   `json:"name"`
	Email  string   `json:"email,omitempty"`
	Groups []string `json:"groups,omitempty"`
	Admin  bool     `json:"admin,omitempty"`
	Write  bool     `json:"write,omitempty"`
}

// AuditEntry is one queue action, append-only within the document's bound.
type AuditEntry struct {
	At     string `json:"at"`
	Actor  string `json:"actor"`
	Action string `json:"action"` // enqueued|dequeued|paused|resumed|landed|ejected|recovered
	Base   string `json:"base"`
	Number int    `json:"number,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// PositionOf is the 1-based place of an entry in its base queue's effective
// order. Terminal entries have no position (0).
func PositionOf(q *Queue, num int) int {
	if q == nil {
		return 0
	}
	for i, cur := range ordered(q.Entries) {
		if cur.Number == num {
			if terminal(cur.State) {
				return 0
			}
			return i + 1
		}
	}
	return 0
}

// terminal reports a state no transition leaves.
func terminal(state string) bool {
	return state == StateLanded || state == StateEjected || state == StateCancelled
}

// active reports a state that occupies a queue slot.
func active(state string) bool {
	return state == StatePending || state == StateValidating || state == StateLanding
}

// ordered returns entries in effective run order: priority descending, then
// enqueue sequence ascending (stable FIFO within a priority). It returns the
// input slice sorted in place when it already owns it; callers that must not
// mutate pass a copy.
func ordered(entries []*Entry) []*Entry {
	out := append([]*Entry(nil), entries...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].Seq < out[j].Seq
	})
	return out
}

// QueueView is the read projection of one base queue: entries in effective
// order with their positions filled.
type QueueView struct {
	Base         string   `json:"base"`
	Paused       bool     `json:"paused"`
	PausedBy     string   `json:"paused_by,omitempty"`
	PausedReason string   `json:"paused_reason,omitempty"`
	PausedAt     string   `json:"paused_at,omitempty"`
	Entries      []*Entry `json:"entries"`
}

// View is the repo queue read (GET …/merge-queue): every base queue, recent
// outcomes and the audit tail.
type View struct {
	Repo      string       `json:"repo"`
	Queues    []*QueueView `json:"queues"`
	Recent    []*Entry     `json:"recent,omitempty"`
	Audit     []AuditEntry `json:"audit,omitempty"`
	UpdatedAt string       `json:"updated_at"`
	PausedAny bool         `json:"paused_any"`
}

// EnqueueInput shapes POST …/merge-queue/enqueue: the base branch, the PR
// number and an optional priority (higher runs first; equal priorities FIFO).
type EnqueueInput struct {
	Base     string `json:"base"`
	Number   int    `json:"number"`
	Priority int    `json:"priority"`
}

// credKey is the credential map key for one active entry.
func credKey(base string, num int) string { return base + "#" + strconv.Itoa(num) }

// nowRFC is the document's timestamp spelling (RFC 3339 UTC, seconds).
func nowRFC(t time.Time) string { return t.UTC().Format(time.RFC3339) }
