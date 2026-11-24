// push.go — push/ref topics on REPO webhooks (06 §5.3 push amendment).
//
// The events bridge (internal/events) already classifies WAL ref events and
// owns the only producer of them; repo hooks deliver from the collaboration
// activity log. This file is the seam between the two: a bridge sink hands the
// batch over, and each ref event is appended to the repo's activity log as one
// immutable event whose action IS the push topic. From there the existing
// per-hook cursor, one-POST-per-event delivery, HMAC signature, deliveries ring
// and redeliver work exactly as they do for collaboration events — nothing is
// duplicated.
//
// What is NOT here, deliberately:
//   - a wake path: the server already wakes the events bridge after a landed
//     push (bind_ssh.go, the CI wake), and the bridge sink call wakes the
//     webhooks task through the normal s.wakeRepo;
//   - org hooks: an org has no repository to push to, so the org surface
//     neither offers nor accepts these topics (validateOrgHookEvents stays
//     activity-only) and deliverOrgRepo skips push actions it may see in a
//     member repo's log;
//   - commit enumeration (that needs the git layer, which notify does not
//     import): the composition fills PushEvent.Commits.
package notify

import (
	"context"
	"fmt"
	"strconv"

	"github.com/crueber/spindle/internal/store"
)

// Push topics exposed to repo hooks. The vocabulary mirrors exactly what the
// events bridge can emit (one event per ref update: branch|tag ×
// create|update|delete); "push" is the umbrella that selects every one of
// them, and it is also the action of an event whose ref is neither a branch
// nor a tag (e.g. refs/notes/*) — a topic the filter can still select.
const (
	TopicPush          = "push"
	TopicBranchCreated = "branch.created"
	TopicBranchUpdated = "branch.updated"
	TopicBranchDeleted = "branch.deleted"
	TopicTagCreated    = "tag.created"
	TopicTagUpdated    = "tag.updated"
	TopicTagDeleted    = "tag.deleted"
)

// pushTopics is the closed set (also the repo-hook filter's allowed values
// beyond activityActions).
var pushTopics = map[string]bool{
	TopicPush:          true,
	TopicBranchCreated: true,
	TopicBranchUpdated: true,
	TopicBranchDeleted: true,
	TopicTagCreated:    true,
	TopicTagUpdated:    true,
	TopicTagDeleted:    true,
}

// PushTopics returns the documented push topic vocabulary, stable order. The
// settings panel renders exactly this list; nothing else may invent topics.
func PushTopics() []string {
	return []string{
		TopicPush,
		TopicBranchCreated, TopicBranchUpdated, TopicBranchDeleted,
		TopicTagCreated, TopicTagUpdated, TopicTagDeleted,
	}
}

// RepoHookTopics is the full filter vocabulary a REPO hook may use: the
// collaboration actions plus the push family (what the settings panel offers
// as suggestions). Stable order: collaboration actions sorted, then push.
func RepoHookTopics() []string {
	out := make([]string, 0, len(activityActions)+len(pushTopics))
	for a := range activityActions {
		out = append(out, a)
	}
	sortStrings(out)
	return append(out, PushTopics()...)
}

// IsPushTopic reports whether action is a push topic (the umbrella included).
func IsPushTopic(action string) bool { return pushTopics[action] }

// HookWantsPush reports whether a hook's filter selects the push family: the
// empty filter ("all"), the "*" wildcard, the "push" umbrella, or any one of
// the concrete topics. The bridge sink uses it to skip the store work (and the
// activity-log writes) when no consumer exists.
func HookWantsPush(h *Hook) bool {
	if h == nil || !h.Active {
		return false
	}
	if len(h.Events) == 0 {
		return true
	}
	for _, e := range h.Events {
		if e == "*" || pushTopics[e] {
			return true
		}
	}
	return false
}

// WantsPush reports whether the repo has at least one ACTIVE hook subscribed to
// the push family. The events bridge sink calls it before recording anything:
// push history is materialized only for consumers, so a repo with no push
// hooks pays only this hook scan. A store error is returned (fail closed — the
// bridge must not advance past a push it could not record).
func (s *Service) WantsPush(ctx context.Context, owner, repo string) (bool, error) {
	hooks, err := s.ListHooks(ctx, owner, repo)
	if err != nil {
		return false, err
	}
	for _, h := range hooks {
		if HookWantsPush(h) {
			return true, nil
		}
	}
	return false, nil
}

// pushTopic maps a WAL ref event (action create|update|delete, ref_type
// branch|tag|"") onto its activity action. A ref that is neither a branch nor
// a tag keeps the umbrella topic.
func pushTopic(action, refType string) string {
	suffix, ok := map[string]string{
		ActionCreateWire: "created",
		ActionUpdateWire: "updated",
		ActionDeleteWire: "deleted",
	}[action]
	if !ok {
		return TopicPush
	}
	switch refType {
	case "branch", "tag":
		return refType + "." + suffix
	}
	return TopicPush
}

// WAL action spellings as the bridge reports them (09 §3 wire actions). The
// local constants keep notify from importing internal/events.
const (
	ActionCreateWire = "create"
	ActionUpdateWire = "update"
	ActionDeleteWire = "delete"
)

// PushCommit is one commit summary in a push payload (bounded: the bridge
// lists at most MaxPushCommits, newest first).
type PushCommit struct {
	ID      string `json:"id"`
	Author  string `json:"author"`
	Email   string `json:"email,omitempty"`
	Message string `json:"message"`
	At      string `json:"at"`
}

// MaxPushCommits bounds the commits list in one push payload (the delivery
// body cap is MaxDeliveryBody; a longer range is truncated newest-first and
// says so). Kept in notify because it is part of the documented payload.
const MaxPushCommits = 50

// PushEvent is one ref event as the events bridge hands it over (a narrow
// shape so notify never imports internal/events). Seq is the WAL sequence —
// the documented dedup key is (repo, Seq, RefName), so a bridge replay that
// re-appends the same event stays identifiable even though it gets a fresh
// activity seq.
type PushEvent struct {
	Seq       uint64
	Action    string // create | update | delete
	RefType   string // branch | tag | ""
	RefName   string // full ref, e.g. refs/heads/main
	Before    string // hex OID (full zero OID on create)
	After     string // hex OID (full zero OID on delete)
	Pusher    string // log entry meta "principal"; "" when absent
	EntryKind string // push | ref_update
	RequestID string
	Commits   []PushCommit
}

// PushWalgit is the "_walgit" block of the push payload: the WAL identity the
// event was classified from (14 §14.6 dedup key).
type PushWalgit struct {
	Seq       string `json:"seq"` // STRING decimal, the uint64-as-string rule
	EntryKind string `json:"entry_kind"`
	RequestID string `json:"request_id"`
}

// PushDetail is the payload.detail document of a push activity event: the
// real push shape a receiver needs (ref, before/after, repository, pusher,
// commits) plus the WAL identity for dedup.
type PushDetail struct {
	Ref        string       `json:"ref"`
	RefType    string       `json:"ref_type"`
	Before     string       `json:"before"`
	After      string       `json:"after"`
	Repository string       `json:"repository"` // "owner/name"
	Pusher     string       `json:"pusher"`
	Commits    []PushCommit `json:"commits"` // [] rather than null when unknown
	Walgit     PushWalgit   `json:"_walgit"`
}

// PushSink appends one activity event per ref event (action = the push topic)
// and wakes the webhooks task. It returns an error when an append fails so the
// events bridge leaves its cursor untouched (fail closed: the replay is
// at-least-once, and the (repo, _walgit.seq, ref) dedup key is in the body).
//
// Callers (the bridge sink adapter) are expected to have checked that at least
// one active hook subscribes to the push family: this method records the push
// history only for consumers that exist, so a quiet repo with no push hooks
// pays nothing.
func (s *Service) PushSink(ctx context.Context, repo string, events []PushEvent) error {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return fmt.Errorf("%w: bad repo %q", ErrInvalid, repo)
	}
	if len(events) == 0 {
		return nil
	}
	// One CAS for the whole batch: the activity seqs are contiguous, the
	// Creates below are independent and idempotent on a 412.
	first, err := s.reserveSeqs(ctx, owner, name, len(events))
	if err != nil {
		return err
	}
	at := s.nowUTC().Format(dateTimeFmt)
	appended := 0
	for i := range events {
		pe := &events[i]
		action := pushTopic(pe.Action, pe.RefType)
		commits := pe.Commits
		if commits == nil {
			commits = []PushCommit{}
		}
		payload, perr := encode(activityPayload{
			Class: "push", Recipients: []activityRecipient{},
			Detail: map[string]any{
				"ref": pe.RefName, "ref_type": pe.RefType, "before": pe.Before, "after": pe.After,
				"repository": repo, "pusher": normPrincipal(pe.Pusher), "commits": commits,
				"_walgit": map[string]string{
					"seq": strconv.FormatUint(pe.Seq, 10), "entry_kind": pe.EntryKind, "request_id": pe.RequestID,
				},
			},
		})
		if perr != nil {
			return perr
		}
		ev := ActivityEvent{
			Seq: first + i, Repo: repo, Action: action, Kind: "repo",
			Actor: normPrincipal(pe.Pusher), At: at, Payload: payload,
		}
		raw, eerr := encode(ev)
		if eerr != nil {
			return eerr
		}
		if err := s.putCreate(ctx, ActivityKey(owner, name, first+i), raw); err != nil && !store.IsPreconditionFailed(err) {
			return err
		}
		appended++
	}
	if appended > 0 {
		s.wakeRepo(repo)
	}
	return nil
}
