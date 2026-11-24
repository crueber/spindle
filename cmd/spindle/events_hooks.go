// events_hooks.go — the repo-webhook sink on the events bridge.
//
// Push/ref topics on repo hooks (docs/features/06 §5.3 push amendment) ride
// the EXISTING bridge: the WAL emits the ref events, this sink appends them to
// the repo's collaboration activity log as activity events (action = push
// topic), and the existing per-hook delivery task POSTs them with the same
// cursor, signature, deliveries ring and redeliver as any collaboration event.
// There is no second wake path: the server already wakes the bridge after a
// landed push (the CI wake), and PushSink wakes the webhooks task through the
// notify service's own coalescing wake.
package main

import (
	"context"
	"strconv"
	"time"

	"github.com/crueber/spindle/internal/events"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/notify"
	"github.com/crueber/spindle/internal/server"
)

// pushCommitTimeout bounds the best-effort commit enumeration per catch-up
// batch: the listing materializes the serving copy on a cold instance, which
// must never hold the bridge's shared cursor indefinitely.
const pushCommitTimeout = 30 * time.Second

// repoHooksSink is the `repo-hooks` events sink: it forwards the bridge batch
// into notify only when at least one active repo hook subscribes to the push
// family.
type repoHooksSink struct {
	svc     *notify.Service
	commits pushCommitLister // nil → commits omitted ([] in the payload)
}

// pushCommitLister enumerates commits for one ref update, newest first. It is
// satisfied by pushCommitReader; tests substitute a fake.
type pushCommitLister interface {
	Commits(ctx context.Context, repo, old, new string) []notify.PushCommit
}

// Name implements events.Sink (metric label).
func (s *repoHooksSink) Name() string { return "repo-hooks" }

// Deliver implements events.Sink. A store failure is returned so the bridge
// leaves its cursor untouched (no silent loss; the replay is at-least-once and
// the PushDetail carries the (repo, _walgit.seq, ref) dedup key).
func (s *repoHooksSink) Deliver(ctx context.Context, repo string, batch []events.RefEvent) error {
	if s == nil || s.svc == nil || len(batch) == 0 {
		return nil
	}
	owner, name, ok := splitOwnerRepo(repo)
	if !ok {
		return nil
	}
	wanted, err := s.svc.WantsPush(ctx, owner, name)
	if err != nil {
		return err
	}
	if !wanted {
		return nil // no consumer: record no push history and pay nothing
	}
	out := make([]notify.PushEvent, 0, len(batch))
	for _, e := range batch {
		if e.RefName == "" {
			continue
		}
		seq, _ := strconv.ParseUint(e.Walgit.Seq, 10, 64)
		pe := notify.PushEvent{
			Seq: seq, Action: e.Action, RefType: e.RefType, RefName: e.RefName,
			Before: e.Old, After: e.New, Pusher: e.Pusher,
			EntryKind: e.Walgit.EntryKind, RequestID: e.Walgit.RequestID,
		}
		if s.commits != nil && (e.RefType == "branch" || e.RefType == "tag") && !zeroOID(e.New) {
			pe.Commits = s.commits.Commits(ctx, repo, e.Old, e.New)
		}
		out = append(out, pe)
	}
	if len(out) == 0 {
		return nil
	}
	return s.svc.PushSink(ctx, repo, out)
}

// pushCommitReader is the production pushCommitLister: it materializes the
// serving copy through the WAL engine and runs the documented bounded
// `git log` (04_git.md §9.11). Every failure degrades to "no commits" — an
// unreachable object store must not stop a signed delivery.
type pushCommitReader struct {
	eng   *server.WalEngine
	layer *git.Layer
}

// Commits implements pushCommitLister.
func (p *pushCommitReader) Commits(ctx context.Context, repo, old, new string) []notify.PushCommit {
	if p == nil || p.eng == nil || p.layer == nil {
		return nil
	}
	id, err := git.ParseRepoId(repo)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, pushCommitTimeout)
	defer cancel()
	local, err := p.eng.Repo(ctx, id, false, git.Sha1)
	if err != nil {
		return nil
	}
	listed, err := p.layer.LogRange(ctx, local, old, new, notify.MaxPushCommits)
	if err != nil {
		return nil
	}
	out := make([]notify.PushCommit, 0, len(listed))
	for _, c := range listed {
		out = append(out, notify.PushCommit{ID: c.ID, Author: c.Author, Email: c.Email, Message: c.Message, At: c.At})
	}
	return out
}

// zeroOID reports an all-zero (or empty) OID — a delete's `after` / a create's
// `before`. The bridge normalizes these to the full zero OID on the wire.
func zeroOID(s string) bool {
	if s == "" {
		return true
	}
	for i := range len(s) {
		if s[i] != '0' {
			return false
		}
	}
	return true
}
