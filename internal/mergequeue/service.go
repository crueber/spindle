package mergequeue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Error classes the HTTP handler maps onto status codes (the pulls spelling:
// plain text, 401/403/404/409/422/503).
var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrForbidden    = errors.New("forbidden")
	ErrUnauthorized = errors.New("unauthorized")
	ErrInvalid      = errors.New("invalid")
	ErrUnavailable  = errors.New("unavailable")
)

// RunStatus is the queue's view of one merge_group run (the threads seam).
type RunStatus struct {
	ID    string
	State string
}

// CheckProgress is the required-checks read model (the checks seam). It is
// the same gate the merge task applies, with the pending/decided split the
// queue needs to wait rather than eject.
type CheckProgress struct {
	Required  []string
	Offenders []string
	OK        bool
	Pending   bool
}

// RoleService is the identity seam (pulls' P6 verbatim).
type RoleService interface {
	Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc)
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// PullsSeam is the pull side of the queue (internal/pulls/queue.go): the only
// code that computes the temporary merge commit, publishes/deletes the ref,
// and lands through the one merge task.
type PullsSeam interface {
	QueueCandidate(ctx context.Context, owner, repo string, num int, actor auth.Principal) (*pulls.QueueCandidate, error)
	PrepareQueueMerge(ctx context.Context, owner, repo string, num int, actor auth.Principal) (*pulls.QueueMerge, error)
	LandQueuedMerge(ctx context.Context, owner, repo string, num int, actor auth.Principal, q *pulls.QueueMerge) (*pulls.TaskRecord, error)
	DeleteQueueRef(ctx context.Context, owner, repo, ref string) error
}

// RunReader reads one commit's merge_group runs created after a watermark
// (threads.Service.MergeGroupRunsAfter). The queue passes the instant its ref
// was published, so the poll never lists the repository's full run history.
type RunReader interface {
	MergeGroupRuns(ctx context.Context, repo, sha string, since time.Time) ([]RunStatus, error)
}

// ChecksReader reads the require_checks progress for a commit
// (checks.Service.RequiredChecksProgress).
type ChecksReader interface {
	RequiredChecksProgress(ctx context.Context, owner, repo, sha, baseRef, principal string) (CheckProgress, error)
}

// Notification is the queue's fan-out event (the notify seam); the
// composition adapter maps it onto notify.Service.EmitPull.
type Notification struct {
	Repo       string
	Class      string
	Actor      string
	PullNum    int
	Recipients []string
	At         string
}

// Service is the merge queue. Store/Roles/Pulls/Runs/Checks are wired by
// composition; a nil seam fails the operation it powers (no silent pass).
type Service struct {
	Store  store.ObjectStore
	Roles  RoleService
	Pulls  PullsSeam
	Runs   RunReader
	Checks ChecksReader
	Notify func(ctx context.Context, ev Notification)
	// RefPublished is the push-hook seam (the Threads composition hook): the
	// queue publishes the documented temporary ref itself (through pulls), so
	// the client push pipeline never sees it. This is called after a
	// successful publish with the full ref and its tip, exactly the shape the
	// push hook consumes — the one run-creation path (PAR-016), never a second
	// implementation. Nil ⇒ the queue ejects (fail closed) rather than
	// publishing a ref no run will ever check.
	RefPublished func(ctx context.Context, repo, ref, newSHA string)

	// Now is the clock (tests pin it; nil = time.Now UTC).
	Now func() time.Time

	// Poll is the run/checks poll interval (default 10s).
	Poll time.Duration
	// Timeout bounds one entry from claim to landing/ejection (default 1h).
	Timeout time.Duration
	// RunGrace is how long an entry may wait for a merge_group run to appear
	// before it is ejected (default 5m).
	RunGrace time.Duration
	// ClaimLease is how stale a validating/landing claim must be before a
	// reconcile pass recovers it (default 15m).
	ClaimLease time.Duration
	// MaxAttempts bounds recovery replays of one entry (default 3).
	MaxAttempts int

	// instance identifies this process's claims. A boot recovery may only
	// reset claims another instance/process wrote; the instance that owns a
	// claim is never stolen by its own first sweep.
	instance string

	mu      sync.Mutex
	workers map[string]bool
	// kicks records a wake that arrived while a worker held the slot, so the
	// worker re-reads the document instead of exiting and losing the wake
	// (enqueue/resume racing a worker that is just about to return).
	kicks map[string]bool
}

// New builds a queue service with the documented defaults.
func New(st store.ObjectStore) *Service {
	return &Service{
		Store:       st,
		instance:    fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()),
		Poll:        10 * time.Second,
		Timeout:     time.Hour,
		RunGrace:    5 * time.Minute,
		ClaimLease:  15 * time.Minute,
		MaxAttempts: 3,
		workers:     map[string]bool{},
		kicks:       map[string]bool{},
	}
}

func (s *Service) nowUTC() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) poll() time.Duration {
	if s.Poll > 0 {
		return s.Poll
	}
	return 10 * time.Second
}

func (s *Service) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return time.Hour
}

func (s *Service) runGrace() time.Duration {
	if s.RunGrace > 0 {
		return s.RunGrace
	}
	return 5 * time.Minute
}

func (s *Service) claimLease() time.Duration {
	if s.ClaimLease > 0 {
		return s.ClaimLease
	}
	return 15 * time.Minute
}

func (s *Service) maxAttempts() int {
	if s.MaxAttempts > 0 {
		return s.MaxAttempts
	}
	return 3
}

func repoName(owner, repo string) string { return owner + "/" + repo }

// baseLabel trims a ref to its plain branch name.
func baseLabel(base string) string {
	if b, ok := strings.CutPrefix(strings.TrimSpace(base), "refs/heads/"); ok {
		return b
	}
	return strings.TrimSpace(base)
}

// --- auth ---------------------------------------------------------------------

// roleRank mirrors pulls' P6 order (read<triage<write<maintain<admin).
func roleRank(role identity.Role) int {
	switch role {
	case identity.RoleRead:
		return 1
	case identity.RoleTriage:
		return 2
	case identity.RoleWrite:
		return 3
	case identity.RoleMaintain:
		return 4
	case identity.RoleAdmin:
		return 5
	}
	return 0
}

func (s *Service) roleOf(ctx context.Context, owner, repo string, p auth.Principal) identity.Role {
	if s.Roles == nil {
		if p.Admin {
			return identity.RoleAdmin
		}
		if p.Write {
			return identity.RoleWrite
		}
		if p.Anonymous {
			return ""
		}
		return identity.RoleRead
	}
	r, _ := s.Roles.Resolve(ctx, owner, repo, p)
	return r
}

// requireRead is the pulls read gate verbatim (visibility-first when the
// identity hook is wired; anonymous is a real 401).
func (s *Service) requireRead(ctx context.Context, owner, repo string, p auth.Principal) error {
	if p.Admin || p.Write {
		return nil
	}
	if s.Roles == nil {
		if p.Anonymous {
			return fmt.Errorf("%w: sign in to read", ErrUnauthorized)
		}
		return nil
	}
	if aerr := s.Roles.CheckRead(ctx, owner, repo, p); aerr != nil {
		switch aerr.Kind {
		case auth.ErrForbidden:
			return fmt.Errorf("%w: %s", ErrForbidden, aerr.Why)
		case auth.ErrUnavailable:
			return fmt.Errorf("%w: identity unavailable: %s", ErrUnavailable, aerr.Why)
		default:
			return fmt.Errorf("%w: %s", ErrUnauthorized, aerr.Why)
		}
	}
	return nil
}

// requireRole is the maintain floor the merge action uses.
func (s *Service) requireRole(ctx context.Context, owner, repo string, p auth.Principal, want identity.Role) error {
	if p.Admin {
		return nil
	}
	if roleRank(s.roleOf(ctx, owner, repo, p)) >= roleRank(want) {
		return nil
	}
	if p.Anonymous {
		return fmt.Errorf("%w: sign in to manage the merge queue", ErrUnauthorized)
	}
	return fmt.Errorf("%w: need %s to manage the merge queue", ErrForbidden, want)
}

// --- store --------------------------------------------------------------------

// loadDoc reads repos/<o>/<r>/merge_queue.json; (empty, "", nil) when absent.
func (s *Service) loadDoc(ctx context.Context, owner, repo string) (*Doc, store.Version, error) {
	raw, meta, err := store.GetBytes(ctx, s.Store, DocKey(owner, repo), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return &Doc{Version: 1, Repo: repoName(owner, repo), Queues: []*Queue{}}, "", nil
		}
		return nil, "", err
	}
	var doc Doc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		return nil, "", fmt.Errorf("%w: merge_queue.json: %v", ErrUnavailable, jerr)
	}
	if doc.Queues == nil {
		doc.Queues = []*Queue{}
	}
	if doc.Version < 1 {
		doc.Version = 1
	}
	return &doc, meta.Version, nil
}

// updateDoc applies one writer's delta to the freshest document and
// CAS-writes it, reloading and re-applying on every precondition failure
// (docs/features/03 §2.3 discipline). The CAS is the commit point; no lock is
// held across it. `mutate` returns changed=false for a no-op.
func (s *Service) updateDoc(ctx context.Context, owner, repo string, mutate func(*Doc) (bool, error)) (*Doc, error) {
	const attempts = 8
	for i := 0; i < attempts; i++ {
		doc, ver, err := s.loadDoc(ctx, owner, repo)
		if err != nil {
			return nil, err
		}
		changed, merr := mutate(doc)
		if merr != nil {
			return nil, merr
		}
		if !changed {
			return doc, nil
		}
		doc.Version++
		doc.UpdatedAt = nowRFC(s.nowUTC())
		raw, jerr := json.Marshal(doc)
		if jerr != nil {
			return nil, jerr
		}
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		if ver == "" {
			opts.Mode = store.PutCreate
		}
		if _, perr := store.PutBytes(ctx, s.Store, DocKey(owner, repo), raw, opts); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return nil, perr
		}
		return doc, nil
	}
	return nil, fmt.Errorf("%w: merge queue changed concurrently; reload and retry", ErrConflict)
}

// queue returns the named base queue, creating it when create is set.
func (d *Doc) queue(base string, create bool) *Queue {
	for _, q := range d.Queues {
		if q.Base == base {
			return q
		}
	}
	if !create {
		return nil
	}
	q := &Queue{Base: base, NextSeq: 1, Entries: []*Entry{}}
	d.Queues = append(d.Queues, q)
	return q
}

// remove deletes an entry from the queue's slice.
func (q *Queue) remove(num int) *Entry {
	for i, e := range q.Entries {
		if e.Number == num {
			q.Entries = append(q.Entries[:i], q.Entries[i+1:]...)
			return e
		}
	}
	return nil
}

// find returns the entry for num (nil when absent).
func (q *Queue) find(num int) *Entry {
	for _, e := range q.Entries {
		if e.Number == num {
			return e
		}
	}
	return nil
}

// head returns the entry that may run now (effective order, first active).
func (q *Queue) head() *Entry {
	for _, e := range ordered(q.Entries) {
		if active(e.State) {
			return e
		}
	}
	return nil
}

// audit appends one bounded audit row.
func (d *Doc) audit(row AuditEntry) {
	if row.At == "" {
		row.At = nowRFC(time.Now().UTC())
	}
	d.Audit = append(d.Audit, row)
	const maxAudit = 200
	if len(d.Audit) > maxAudit {
		d.Audit = append([]AuditEntry(nil), d.Audit[len(d.Audit)-maxAudit:]...)
	}
}

// remember puts a terminal entry into the bounded recent-outcome list,
// replacing any earlier outcome for the same pull request (a PR has exactly
// one base branch, so the number is unambiguous).
func (d *Doc) remember(e *Entry) {
	out := make([]*Entry, 0, len(d.Recent)+1)
	out = append(out, e)
	for _, old := range d.Recent {
		if old.Number == e.Number {
			continue
		}
		out = append(out, old)
	}
	const maxRecent = 50
	if len(out) > maxRecent {
		out = out[:maxRecent]
	}
	d.Recent = out
}

// --- reads --------------------------------------------------------------------

// List returns the repo queue view. Auth: read.
func (s *Service) List(ctx context.Context, owner, repo string, actor auth.Principal) (*View, error) {
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	doc, _, err := s.loadDoc(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return viewOf(doc), nil
}

// ListBase returns one base queue's view. Auth: read.
func (s *Service) ListBase(ctx context.Context, owner, repo, base string, actor auth.Principal) (*QueueView, error) {
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	doc, _, err := s.loadDoc(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	q := doc.queue(baseLabel(base), false)
	if q == nil {
		return &QueueView{Base: baseLabel(base), Entries: []*Entry{}}, nil
	}
	return queueViewOf(q), nil
}

// PRView is the PR panel's queue projection: the PR's active entry (or most
// recent terminal outcome), the base queue's pause state and the base name.
type PRView struct {
	Base   string `json:"base"`
	Paused bool   `json:"paused"`
	Entry  *Entry `json:"entry,omitempty"`
}

// QueueForPR returns the PR's queue projection for the PR panel. Auth: read.
func (s *Service) QueueForPR(ctx context.Context, owner, repo string, num int, actor auth.Principal) (*PRView, error) {
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	doc, _, err := s.loadDoc(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	for _, q := range doc.Queues {
		if e := q.find(num); e != nil {
			cp := *e
			cp.Position = PositionOf(q, num)
			return &PRView{Base: q.Base, Paused: q.Paused, Entry: &cp}, nil
		}
	}
	for _, e := range doc.Recent {
		if e.Number == num {
			cp := *e
			return &PRView{Base: cp.Base, Entry: &cp}, nil
		}
	}
	return &PRView{}, nil
}

// EntryForPR returns the PR's queue entry: an active entry (with its live
// position) or the most recent terminal outcome. (nil, nil) when the PR is not
// in the queue. Auth: read.
func (s *Service) EntryForPR(ctx context.Context, owner, repo string, num int, actor auth.Principal) (*Entry, error) {
	v, err := s.QueueForPR(ctx, owner, repo, num, actor)
	if err != nil {
		return nil, err
	}
	return v.Entry, nil
}

func viewOf(doc *Doc) *View {
	v := &View{Repo: doc.Repo, UpdatedAt: doc.UpdatedAt, Audit: doc.Audit}
	for _, q := range doc.Queues {
		qv := queueViewOf(q)
		v.Queues = append(v.Queues, qv)
		if qv.Paused {
			v.PausedAny = true
		}
	}
	if v.Queues == nil {
		v.Queues = []*QueueView{}
	}
	v.Recent = doc.Recent
	return v
}

func queueViewOf(q *Queue) *QueueView {
	qv := &QueueView{Base: q.Base, Paused: q.Paused, PausedBy: q.PausedBy, PausedReason: q.PausedReason, PausedAt: q.PausedAt}
	for i, e := range ordered(q.Entries) {
		cp := *e
		cp.Position = i + 1
		qv.Entries = append(qv.Entries, &cp)
	}
	if qv.Entries == nil {
		qv.Entries = []*Entry{}
	}
	return qv
}

// --- enqueue / dequeue / pause / resume ---------------------------------------

// Enqueue adds a PR to its base branch's queue. Auth: maintain (the merge
// action's floor). The PR must be open, not a draft, not merged, and must
// target the named base branch. An existing active entry is a 409.
func (s *Service) Enqueue(ctx context.Context, owner, repo string, actor auth.Principal, in EnqueueInput) (*Entry, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, repo, actor, identity.RoleMaintain); err != nil {
		return nil, err
	}
	base := baseLabel(strings.TrimSpace(in.Base))
	if base == "" || strings.Contains(base, "..") || strings.HasPrefix(base, "/") || strings.HasSuffix(base, "/") {
		return nil, fmt.Errorf("%w: base branch is required", ErrInvalid)
	}
	if in.Number <= 0 {
		return nil, fmt.Errorf("%w: number is required", ErrInvalid)
	}
	if in.Priority < 0 || in.Priority > MaxPriority {
		return nil, fmt.Errorf("%w: priority must be 0…%d", ErrInvalid, MaxPriority)
	}
	if s.Pulls == nil {
		return nil, fmt.Errorf("%w: pulls service not wired", ErrUnavailable)
	}
	cand, err := s.Pulls.QueueCandidate(ctx, owner, repo, in.Number, actor)
	if err != nil {
		return nil, err
	}
	if cand.Merged {
		return nil, fmt.Errorf("%w: pull request #%d is already merged", ErrConflict, in.Number)
	}
	if cand.State == "closed" {
		return nil, fmt.Errorf("%w: pull request #%d is closed", ErrConflict, in.Number)
	}
	if cand.Draft {
		return nil, fmt.Errorf("%w: pull request #%d is a draft", ErrConflict, in.Number)
	}
	if cand.BaseRef != "refs/heads/"+base {
		return nil, fmt.Errorf("%w: pull request #%d targets %s, not refs/heads/%s", ErrInvalid, in.Number, cand.BaseRef, base)
	}
	who := norm(actor.Name)
	var made *Entry
	doc, err := s.updateDoc(ctx, owner, repo, func(doc *Doc) (bool, error) {
		q := doc.queue(base, true)
		if cur := q.find(in.Number); cur != nil {
			if active(cur.State) {
				return false, fmt.Errorf("%w: pull request #%d is already at position %d", ErrConflict, in.Number, PositionOf(q, in.Number))
			}
			q.remove(in.Number) // a terminal row: re-enqueue replaces it
		}
		entry := &Entry{
			Number: in.Number, Base: base, Title: cand.Title, Author: norm(cand.Author), Enqueuer: who,
			Priority: in.Priority, Seq: q.NextSeq,
			State: StatePending, EnqueuedAt: nowRFC(s.nowUTC()),
		}
		if doc.Credentials == nil {
			doc.Credentials = map[string]authPrincipal{}
		}
		doc.Credentials[credKey(base, in.Number)] = toAuthPrincipal(actor)
		q.NextSeq++
		q.Entries = append(q.Entries, entry)
		made = entry
		doc.audit(AuditEntry{At: entry.EnqueuedAt, Actor: who, Action: "enqueued", Base: base,
			Number: in.Number, Detail: fmt.Sprintf("priority %d, position %d", entry.Priority, PositionOf(q, in.Number))})
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if made != nil && doc != nil {
		made.Position = PositionOf(doc.queue(base, false), in.Number)
	}
	s.notify(ctx, owner, repo, "merge_queue_enqueued", who, in.Number, []string{norm(cand.Author), who})
	s.startWorker(ctx, owner, repo)
	return made, nil
}

// Dequeue removes a PR from the queue. Auth: maintain, or the PR's author /
// the maintainer who enqueued it (the contract's "by author/maintainer").
// An entry already landing cannot be dequeued (the merge is committed); every
// other active state is cancelled and its temporary ref deleted.
func (s *Service) Dequeue(ctx context.Context, owner, repo, base string, num int, actor auth.Principal) (*Entry, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	base = baseLabel(strings.TrimSpace(base))
	who := norm(actor.Name)
	var out *Entry
	var refLeft string
	doc, _, derr := s.loadDoc(ctx, owner, repo)
	if derr != nil {
		return nil, derr
	}
	q := doc.queue(base, false)
	if q == nil {
		return nil, fmt.Errorf("%w: no queue for %s", ErrNotFound, base)
	}
	cur := q.find(num)
	if cur == nil {
		return nil, fmt.Errorf("%w: pull request #%d is not queued on %s", ErrNotFound, num, base)
	}
	isMaintainer := actor.Admin || roleRank(s.roleOf(ctx, owner, repo, actor)) >= roleRank(identity.RoleMaintain)
	if !isMaintainer && who != cur.Author && who != cur.Enqueuer {
		return nil, fmt.Errorf("%w: only a maintainer or the pull request's author may dequeue it", ErrForbidden)
	}
	if cur.State == StateLanding {
		return nil, fmt.Errorf("%w: pull request #%d is already landing", ErrConflict, num)
	}
	_, err := s.updateDoc(ctx, owner, repo, func(doc *Doc) (bool, error) {
		q := doc.queue(base, false)
		if q == nil {
			return false, fmt.Errorf("%w: no queue for %s", ErrNotFound, base)
		}
		e := q.find(num)
		if e == nil || !active(e.State) {
			return false, fmt.Errorf("%w: pull request #%d is not queued any more", ErrConflict, num)
		}
		if e.State == StateLanding {
			return false, fmt.Errorf("%w: pull request #%d is already landing", ErrConflict, num)
		}
		now := nowRFC(s.nowUTC())
		q.remove(num)
		delete(doc.Credentials, credKey(base, num))
		e.State = StateCancelled
		e.FinishedAt = now
		e.Reason = "dequeued by " + who
		e.ClaimID, e.ClaimedAt = "", ""
		refLeft = e.Ref
		doc.remember(e)
		doc.audit(AuditEntry{At: now, Actor: who, Action: "dequeued", Base: base, Number: num, Detail: e.Reason})
		out = e
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	// Delete the temporary ref now (idempotent). A failure leaves Ref set;
	// the reconcile pass retries it, so a cancelled entry can never leave a
	// queue ref advertised forever.
	if refLeft != "" {
		s.cleanupRef(ctx, owner, repo, base, num, refLeft)
	}
	s.notify(ctx, owner, repo, "merge_queue_dequeued", who, num, []string{out.Author, out.Enqueuer})
	s.startWorker(ctx, owner, repo)
	return out, nil
}

// Pause stops the named base queue from starting its next entry (an entry
// already validating/landing continues). An empty base pauses every existing
// base queue. Auth: maintain.
func (s *Service) Pause(ctx context.Context, owner, repo, base, reason string, actor auth.Principal) error {
	if err := requireAuthenticated(actor); err != nil {
		return err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return err
	}
	if err := s.requireRole(ctx, owner, repo, actor, identity.RoleMaintain); err != nil {
		return err
	}
	base = baseLabel(strings.TrimSpace(base))
	who := norm(actor.Name)
	_, err := s.updateDoc(ctx, owner, repo, func(doc *Doc) (bool, error) {
		now := nowRFC(s.nowUTC())
		targets := doc.Queues
		if base != "" {
			targets = []*Queue{doc.queue(base, true)}
		}
		for _, q := range targets {
			if q.Paused {
				continue
			}
			q.Paused = true
			q.PausedBy, q.PausedReason, q.PausedAt = who, strings.TrimSpace(reason), now
			doc.audit(AuditEntry{At: now, Actor: who, Action: "paused", Base: q.Base, Detail: q.PausedReason})
		}
		return true, nil
	})
	return err
}

// Resume re-enables starts for the named base queue (empty = every queue)
// and wakes the worker. Auth: maintain.
func (s *Service) Resume(ctx context.Context, owner, repo, base string, actor auth.Principal) error {
	if err := requireAuthenticated(actor); err != nil {
		return err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return err
	}
	if err := s.requireRole(ctx, owner, repo, actor, identity.RoleMaintain); err != nil {
		return err
	}
	base = baseLabel(strings.TrimSpace(base))
	who := norm(actor.Name)
	_, err := s.updateDoc(ctx, owner, repo, func(doc *Doc) (bool, error) {
		now := nowRFC(s.nowUTC())
		targets := doc.Queues
		if base != "" {
			targets = []*Queue{doc.queue(base, true)}
		}
		for _, q := range targets {
			if !q.Paused {
				continue
			}
			q.Paused = false
			q.PausedBy, q.PausedReason, q.PausedAt = "", "", ""
			doc.audit(AuditEntry{At: now, Actor: who, Action: "resumed", Base: q.Base})
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	s.startWorker(ctx, owner, repo)
	return nil
}

// --- helpers ------------------------------------------------------------------

func requireAuthenticated(p auth.Principal) error {
	if p.Anonymous || norm(p.Name) == "" {
		return fmt.Errorf("%w: sign in to use the merge queue", ErrUnauthorized)
	}
	return nil
}

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func toAuthPrincipal(p auth.Principal) authPrincipal {
	return authPrincipal{Name: p.Name, Email: p.Email, Groups: append([]string(nil), p.Groups...), Admin: p.Admin, Write: p.Write}
}

func (a authPrincipal) principal() auth.Principal {
	return auth.Principal{Name: a.Name, Email: a.Email, Groups: append([]string(nil), a.Groups...), Admin: a.Admin, Write: a.Write}
}

func (s *Service) notify(ctx context.Context, owner, repo, class, actor string, num int, recipients []string) {
	if s.Notify == nil {
		return
	}
	s.Notify(ctx, Notification{
		Repo: repoName(owner, repo), Class: class, Actor: actor, PullNum: num,
		Recipients: recipients, At: nowRFC(s.nowUTC()),
	})
}

// cleanupRef deletes one temporary ref and clears it from the document. A
// failed delete leaves Ref on the row for the reconcile pass to retry.
func (s *Service) cleanupRef(ctx context.Context, owner, repo, base string, num int, ref string) {
	if ref == "" || s.Pulls == nil {
		return
	}
	if err := s.Pulls.DeleteQueueRef(ctx, owner, repo, ref); err != nil {
		return
	}
	_, _ = s.updateDoc(ctx, owner, repo, func(doc *Doc) (bool, error) {
		if q := doc.queue(base, false); q != nil {
			if e := q.find(num); e != nil && e.Ref == ref {
				e.Ref = ""
				return true, nil
			}
		}
		for _, e := range doc.Recent {
			if e.Ref == ref {
				e.Ref = ""
				return true, nil
			}
		}
		return false, nil
	})
}

// startWorker launches (or joins) the repository's queue driver. One driver
// per repository serialises the heads of all its base queues; the entry's doc
// CAS is the cross-instance arbitration, so a second process can never take a
// claim that is still fresh (see ReconcileRepo).
func (s *Service) startWorker(ctx context.Context, owner, repo string) {
	key := repoName(owner, repo)
	s.mu.Lock()
	if s.workers[key] {
		s.kicks[key] = true
		s.mu.Unlock()
		return
	}
	s.workers[key] = true
	s.mu.Unlock()
	go s.runWorker(context.WithoutCancel(ctx), owner, repo)
}

// releaseWorker clears the worker slot after a drain. When a kick arrived
// while the slot was held, the worker keeps the slot and drains once more
// (no lost wake-up between enqueue/resume and the worker's exit).
func (s *Service) releaseWorker(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kicks[key] {
		delete(s.kicks, key)
		return true
	}
	delete(s.workers, key)
	return false
}
