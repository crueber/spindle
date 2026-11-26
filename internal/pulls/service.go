package pulls

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// This file owns the shared-family helpers (P2 numbering, P3 two-step, P4
// index over the SHARED issues keys) plus the read/write PR surface: open,
// get, list, update, comment.

// --- shared P2 numbering -----------------------------------------------------

// allocNum allocates one number from the shared meta/next_num via a
// PutUpdate CAS loop (P2; human-rate ⇒ CAS contention is a non-issue).
// Bounded at 10 attempts, then ErrConflict.
func (s *Service) allocNum(ctx context.Context, owner, repo string) (int, error) {
	key := CounterKey(owner, repo)
	var num int
	_, err := s.casUpdate(ctx, key, 10, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		c := Counter{Next: 1}
		if cur != nil {
			if jerr := json.Unmarshal(cur, &c); jerr != nil || c.Next < 1 {
				return nil, false, fmt.Errorf("%w: meta/next_num: corrupt", ErrCorrupt)
			}
		}
		num = c.Next
		// Increment in place: the decode captured every key this struct does
		// not name, so the CAS write re-emits them instead of stripping an
		// additive counter field (law 5).
		c.Next++
		raw, _ := json.Marshal(c)
		return raw, true, nil
	})
	if err != nil {
		return 0, err
	}
	return num, nil
}

// --- shared P3 two-step ------------------------------------------------------

// loadThread reads a thread header; (nil, "", nil) when absent.
func (s *Service) loadThread(ctx context.Context, owner, repo string, num int) (*Thread, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, ThreadKey(owner, repo, num))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return nil, "", nil
	}
	t, perr := parseThread(raw)
	if perr != nil {
		return nil, "", perr
	}
	return t, ver, nil
}

// appendEvent runs the P3 two-step on one thread: CAS the header with mutate
// (which MUST increment NextEventSeq by exactly 1), then Create the event.
// Bounded at 5 attempts, then ErrConflict. A nil event from mutate is a
// no-op (nothing reserved, nothing written).
func (s *Service) appendEvent(ctx context.Context, owner, repo string, num int, mutate func(t *Thread, seq int) (*Event, error)) (*Thread, *Event, error) {
	key := ThreadKey(owner, repo, num)
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		raw, ver, err := s.getJSON(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		if raw == nil {
			return nil, nil, fmt.Errorf("%w: %d", ErrNotFound, num)
		}
		t, perr := parseThread(raw)
		if perr != nil {
			return nil, nil, perr
		}
		seq := t.NextEventSeq
		ev, merr := mutate(t, seq)
		if merr != nil {
			return nil, nil, merr
		}
		if ev == nil {
			return t, nil, nil
		}
		ev.Seq = seq
		if _, cerr := store.PutBytes(ctx, s.Store, key, encodeThread(t),
			store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}); cerr != nil {
			if store.IsPreconditionFailed(cerr) {
				continue
			}
			return nil, nil, cerr
		}
		if cerr := s.putCreate(ctx, EventKey(owner, repo, num, seq), encodeEvent(ev)); cerr != nil {
			if store.IsPreconditionFailed(cerr) {
				continue
			}
			return nil, nil, cerr
		}
		nt, _, _ := s.loadThread(ctx, owner, repo, num)
		if nt != nil {
			t = nt
		}
		return t, ev, nil
	}
	return nil, nil, fmt.Errorf("%w: pull request %d changed concurrently; reload and retry", ErrConflict, num)
}

// loadPR reads a pr.json sidecar; (nil, "", nil) when absent.
func (s *Service) loadPR(ctx context.Context, owner, repo string, num int) (*PRDoc, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, PRKey(owner, repo, num))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return nil, "", nil
	}
	p, perr := parsePR(raw)
	if perr != nil {
		return nil, "", perr
	}
	return p, ver, nil
}

// prWriteAttempts bounds the pr.json CAS retry loop (contention, not a hint).
const prWriteAttempts = 5

// updatePR applies one writer's owned delta to the freshest pr.json and
// CAS-writes it, re-loading and re-applying on every precondition failure
// (§2.3 write discipline). `mutate` runs against the freshly loaded doc on
// each attempt, so no field is ever taken from a stale copy: a writer can
// only change what it explicitly sets, and returns changed=false when the
// fresh doc already records the wanted state (no write, no version bump).
//
// Concurrency: read-modify-write on one key; CAS is the commit point, so a
// lost race retries against the winner's doc instead of clobbering it. The
// loop is bounded (prWriteAttempts) and returns ErrConflict on exhaustion.
func (s *Service) updatePR(ctx context.Context, owner, repo string, num int, mutate func(*PRDoc) (bool, error)) (*PRDoc, error) {
	for attempt := 0; attempt < prWriteAttempts; attempt++ {
		cur, ver, err := s.loadPR(ctx, owner, repo, num)
		if err != nil {
			return nil, err
		}
		if cur == nil {
			return nil, fmt.Errorf("%w: pull request %d not found", ErrNotFound, num)
		}
		changed, merr := mutate(cur)
		if merr != nil {
			return nil, merr
		}
		if !changed {
			return cur, nil
		}
		cur.Version++
		if perr := s.putPR(ctx, owner, repo, cur, ver); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return nil, perr
		}
		return cur, nil
	}
	return nil, fmt.Errorf("%w: pull request %d changed concurrently", ErrConflict, num)
}

// putPR writes one encoded pr.json with an explicit precondition ("" ⇒
// Create). It is the only pr.json PUT in this package.
func (s *Service) putPR(ctx context.Context, owner, repo string, p *PRDoc, ver store.Version) error {
	opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
	if ver == "" {
		opts.Mode = store.PutCreate
	}
	_, err := store.PutBytes(ctx, s.Store, PRKey(owner, repo, p.Num), encodePR(p), opts)
	return err
}

// savePR writes a WHOLE pr.json document as the caller holds it. It is the
// explicit whole-doc writer: the OpenPR create path and hermetic tests use
// it; production maintenance writers go through updatePR instead. A CAS
// loss is reported as ErrConflict, never retried — a stale whole doc has no
// owned delta to re-apply, and copying it onto the fresh doc is exactly the
// field-resurrection bug §2.3 forbids (head sha reverted to a stale value;
// see the Decisions entry for the TestSink flake).
func (s *Service) savePR(ctx context.Context, owner, repo string, p *PRDoc, ver store.Version) error {
	p.Version++
	err := s.putPR(ctx, owner, repo, p, ver)
	if store.IsPreconditionFailed(err) {
		return fmt.Errorf("%w: pull request %d changed concurrently", ErrConflict, p.Num)
	}
	return err
}

// --- shared P4 index ---------------------------------------------------------

// loadIndex reads the shared issues/index.json; (empty, "", nil) when absent.
func (s *Service) loadIndex(ctx context.Context, owner, repo string) (*Index, store.Version, error) {
	raw, ver, err := s.getJSON(ctx, IndexKey(owner, repo))
	if err != nil {
		return nil, "", err
	}
	if raw == nil {
		return &Index{Open: []Card{}, ClosedRecent: []Card{}}, "", nil
	}
	ix, perr := parseIndex(raw)
	if perr != nil {
		return nil, "", perr
	}
	return ix, ver, nil
}

// prCardOf projects a PR thread header onto its shared-index card
// (kind:"pr"; the header alone — 02's list rendering reads cards alone).
func prCardOf(t *Thread) Card {
	return Card{
		Num: t.Num, Kind: "pr", Title: t.Title, State: t.State,
		StateReason: t.StateReason,
		Labels:      nonNilStr(t.Labels), Assignees: nonNilStr(t.Assignees),
		Milestone: t.Milestone,
		Author:    t.Author, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		CommentCount: t.CommentCount,
	}
}

// updateIndex upserts one card by its own CAS loop (P4). Bounded at 10
// attempts, then it PROCEEDS WITHOUT the index update — LIST fallback
// covers reads, so staleness is a performance gap, never correctness.
func (s *Service) updateIndex(ctx context.Context, owner, repo string, card Card) {
	key := IndexKey(owner, repo)
	for attempt := 0; attempt < 10; attempt++ {
		raw, ver, err := s.getJSON(ctx, key)
		if err != nil {
			return
		}
		var ix *Index
		if raw == nil {
			ix = &Index{Open: []Card{}, ClosedRecent: []Card{}}
		} else if ix, err = parseIndex(raw); err != nil {
			return
		}
		upsertCard(ix, card)
		ix.Version++
		written, _ := json.Marshal(ix)
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		if ver == "" {
			opts.Mode = store.PutCreate
		}
		if _, perr := store.PutBytes(ctx, s.Store, key, written, opts); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return
		}
		break
	}
}

// upsertCard inserts or replaces a card, newest-activity-first in both
// pages. Cards of either kind are carried (one index, one numbering space).
//
// A replacement is a re-projection of the same row, so it inherits the old
// card's unknown JSON keys (law 5): the shared document may carry a field a
// future writer added, and a PR state change must not strip it.
func upsertCard(ix *Index, card Card) {
	if old, ok := findIndexCard(ix, card.Num); ok {
		card.inheritUnknown(old)
	}
	drop := func(cards []Card, num int) []Card {
		out := cards[:0]
		for _, c := range cards {
			if c.Num != num {
				out = append(out, c)
			}
		}
		if out == nil {
			return []Card{}
		}
		return out
	}
	ix.Open = drop(ix.Open, card.Num)
	ix.ClosedRecent = drop(ix.ClosedRecent, card.Num)
	if card.State == StateOpen {
		ix.Open = append(ix.Open, card)
		sortCards(ix.Open)
	} else {
		ix.ClosedRecent = append(ix.ClosedRecent, card)
		sortCards(ix.ClosedRecent)
	}
}

// findIndexCard returns the current row for num from either page.
func findIndexCard(ix *Index, num int) (Card, bool) {
	for _, cards := range [][]Card{ix.Open, ix.ClosedRecent} {
		for _, c := range cards {
			if c.Num == num {
				return c, true
			}
		}
	}
	return Card{}, false
}

// sortCards orders newest-activity-first (updated_at desc, num desc).
// This is the INDEX storage order (shared with 02 §2) — it is NOT the
// list render order (ListPRs renders number-desc via sortCardsByNum).
func sortCards(cards []Card) {
	sort.Slice(cards, func(i, j int) bool {
		if cards[i].UpdatedAt != cards[j].UpdatedAt {
			return cards[i].UpdatedAt > cards[j].UpdatedAt
		}
		return cards[i].Num > cards[j].Num
	})
}

// sortCardsByNum orders lists newest-first by PR number descending (num
// desc). ListPRs uses this AFTER merging the open + closed_recent pages,
// so the combined list is always #N…#1 regardless of the state filter —
// activity recency never reorders it (same rule as 02 issue #48).
func sortCardsByNum(cards []Card) {
	sort.Slice(cards, func(i, j int) bool { return cards[i].Num > cards[j].Num })
}

// --- open --------------------------------------------------------------------

// OpenInput shapes POST …/pulls (§8): {title, base_ref, head_ref, body?, fork?, draft?}.
type OpenInput struct {
	Title   string
	BaseRef string
	HeadRef string
	Body    string
	Fork    *ForkInfo // cross-fork head: fork repo holding head_ref
	Draft   bool      // open as a draft (default false — ready for review)
}

// OpenPR opens a PR (§3): resolve + verify, allocate the number (shared
// P2), create the thread (kind:"pr") + opened event (shared P3), Create
// pr.json, update the shared index (P4, card kind:"pr"), fan out (P8), then
// publish refs/pull/<num>/head server-side through the WAL publish path —
// ONLY when the head commit is already reachable (422 otherwise). Auth:
// write on the base repo (the server acts as the creator's principal for
// the ref create, so the creator needs only base-repo write).
func (s *Service) OpenPR(ctx context.Context, owner, repo string, actor auth.Principal, in OpenInput, correlationID string) (*Thread, *PRDoc, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, nil, err
	}
	if err := s.requireRole(ctx, owner, repo, actor, "write"); err != nil {
		return nil, nil, err
	}
	if err := s.requireNotBlocked(ctx, owner, repo, actor); err != nil {
		return nil, nil, err
	}
	title, err := validateTitle(in.Title)
	if err != nil {
		return nil, nil, err
	}
	if err := validateBody(in.Body); err != nil {
		return nil, nil, err
	}
	if err := validateRefName(in.BaseRef); err != nil {
		return nil, nil, err
	}
	if err := validateRefName(in.HeadRef); err != nil {
		return nil, nil, err
	}
	if s.Git == nil || s.Dirs == nil {
		return nil, nil, fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	baseRepo := repoName(owner, repo)
	headOwner, headRepo := owner, repo
	if in.Fork != nil {
		parts := strings.SplitN(in.Fork.Repo, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, nil, fmt.Errorf("%w: fork.repo must be owner/name", ErrInvalid)
		}
		headOwner, headRepo = parts[0], parts[1]
	}
	baseDir, err := s.Dirs.Dir(ctx, baseRepo)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: base repo unavailable: %v", ErrUnavailable, err)
	}
	headDir, err := s.Dirs.Dir(ctx, repoName(headOwner, headRepo))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: head repo unavailable: %v", ErrUnavailable, err)
	}
	baseSHA, err := s.Git.ResolveRef(ctx, baseDir, in.BaseRef)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: unknown base revision %q", ErrUnprocessable, in.BaseRef)
	}
	headSHA, err := s.Git.ResolveRef(ctx, headDir, in.HeadRef)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: unknown revision %q", ErrUnprocessable, in.HeadRef)
	}
	// Reachability in the BASE object set (§3: no PR ever publishes git
	// objects, only a ref to objects that already arrived). Fork-unique
	// heads miss the base serving copy by construction (issue #456) — the
	// §7 fork→base bridge fetches them from the fork serving copy on
	// demand (local disk-to-disk, zero bucket trips) and re-probes, so a
	// missing-object rev-list failure is bridged, not 503'd. pre is the
	// WITHOUT-bridge verdict: refs/pull/<num>/head is published only for
	// pre-reachable heads (bucket-backed rule — bridge.go); bridged-only
	// cross-fork heads record fork-local (§7) exactly as before.
	pre, reachable, err := s.bridgeForkHead(ctx, baseRepo, repoName(headOwner, headRepo), baseDir, headDir, headSHA)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: reachability check: %v", ErrUnavailable, err)
	}
	sameRepo := headOwner == owner && headRepo == repo
	if !reachable && sameRepo {
		return nil, nil, fmt.Errorf("%w: head commit not reachable — push first", ErrUnprocessable)
	}
	// 409 if an OPEN PR already pairs base+head (index hot window; open PRs
	// are human-scale — one bounded read set, no LIST).
	if dup, derr := s.findOpenPair(ctx, owner, repo, in.BaseRef, in.HeadRef, repoName(headOwner, headRepo)); derr != nil {
		return nil, nil, derr
	} else if dup > 0 {
		return nil, nil, fmt.Errorf("%w: open pull request #%d already pairs %s and %s", ErrConflict, dup, in.BaseRef, in.HeadRef)
	}
	num, err := s.allocNum(ctx, owner, repo)
	if err != nil {
		return nil, nil, err
	}
	now := s.nowUTC().Format(dateTimeFmt)
	who := normPrincipal(actor.Name)
	th := &Thread{
		Num: num, Kind: "pr", Title: title, State: StateOpen,
		Author: who, CreatedAt: now, UpdatedAt: now,
		Labels: []string{}, Assignees: []string{}, Participants: []string{who},
		NextEventSeq: 1, CommentCount: 0, Version: 1,
	}
	ev := &Event{Seq: 0, Type: EventOpened, Actor: who, At: now, Body: strPtr(in.Body)}
	if _, err := store.PutBytes(ctx, s.Store, ThreadKey(owner, repo, num), encodeThread(th),
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil {
		if store.IsPreconditionFailed(err) {
			return nil, nil, fmt.Errorf("%w: pull request %d already exists", ErrConflict, num)
		}
		return nil, nil, err
	}
	if err := s.putCreate(ctx, EventKey(owner, repo, num, 0), encodeEvent(ev)); err != nil {
		return nil, nil, err
	}
	pr := &PRDoc{
		Num: num, Kind: "pr",
		Base:  Endpoint{Repo: baseRepo, Ref: in.BaseRef, SHA: baseSHA},
		Head:  Endpoint{Repo: repoName(headOwner, headRepo), Ref: in.HeadRef, SHA: headSHA},
		Body:  in.Body,
		Draft: in.Draft, Version: 1,
	}
	if !sameRepo {
		pr.Fork = &ForkInfo{Repo: repoName(headOwner, headRepo)}
	}
	if err := s.putCreate(ctx, PRKey(owner, repo, num), encodePR(pr)); err != nil {
		if store.IsPreconditionFailed(err) {
			return nil, nil, fmt.Errorf("%w: pull request %d already exists", ErrConflict, num)
		}
		return nil, nil, err
	}
	s.updateIndex(ctx, owner, repo, prCardOf(th))
	s.emit(ctx, NotifyEvent{Repo: baseRepo, Class: "opened", Actor: who, PullNum: num, Recipients: []string{}})
	s.emitMentioned(ctx, owner, repo, num, who, in.Body)
	s.stream(ctx, StreamEvent{Name: "pull", Repo: baseRepo, Action: "opened", Num: num, Title: title, State: StateOpen, Author: who, BaseRef: in.BaseRef, HeadRef: in.HeadRef, HeadSHA: headSHA})
	// Server-side refs/pull/<num>/head publish (WAL publish path, §3):
	// only for pre-reachable heads (unreachable same-repo heads already
	// 422'd above; bridged-only cross-fork heads stay fork-local — their
	// objects are cache-resident, not bucket-backed, so no base-side ref
	// may name them).
	headPublished := false
	if pre && s.Refs != nil {
		meta := map[string]string{"principal": who, "agent": "pulls"}
		if correlationID != "" {
			meta["correlation_id"] = correlationID
		}
		if cerr := s.Refs.CreateRef(ctx, baseRepo, PullHeadRef(num), headSHA, meta); cerr != nil {
			// Recovery is a named repair (§3 Concurrency): the PR exists
			// with refs/pull/<num>/head absent; GET re-verifies reachability
			// and re-publishes idempotently. The open still 201s.
			s.stream(ctx, StreamEvent{Name: "pull", Repo: baseRepo, Action: "opened", Num: num, Title: title, State: StateOpen, Author: who, BaseRef: in.BaseRef, HeadRef: in.HeadRef, HeadSHA: headSHA})
		} else {
			headPublished = true
		}
	}
	pr.HeadPublished = headPublished
	nt, _, _ := s.loadThread(ctx, owner, repo, num)
	if nt != nil {
		th = nt
	}
	// CI trigger (Threads pass A): a new PR starts its pull_request runs.
	// Post-commit, on the same lifecycle edge the notify/SSE seams use.
	s.emitPullLifecycle(ctx, baseRepo, th, pr, "opened", who)
	return th, pr, nil
}

// findOpenPair returns the num of an OPEN PR pairing (baseRef, headRef,
// headRepo), or 0. Index hot window only (open PRs, human-scale).
func (s *Service) findOpenPair(ctx context.Context, owner, repo, baseRef, headRef, headRepo string) (int, error) {
	ix, _, err := s.loadIndex(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	for _, c := range ix.Open {
		if c.Kind != "pr" {
			continue
		}
		pr, _, perr := s.loadPR(ctx, owner, repo, c.Num)
		if perr != nil || pr == nil {
			continue
		}
		if pr.Base.Ref == baseRef && pr.Head.Ref == headRef && pr.Head.Repo == headRepo && !pr.Merged {
			return pr.Num, nil
		}
	}
	return 0, nil
}

// --- get ---------------------------------------------------------------------

// PullView is GET …/pulls/{num}: header + pr.json + live mergeable (§4:
// stamped; mismatch ⇒ unknown + enqueued recompute) + the last-K timeline
// events (newest-first; older on demand via after_seq, like 02 §7).
type PullView struct {
	Thread     *Thread       `json:"thread"`
	PR         *PRDoc        `json:"pr"`
	Mergeable  *MergeableDoc `json:"mergeable"`
	HeadLive   string        `json:"head_live_sha"`
	BaseLive   string        `json:"base_live_sha"`
	HeadRefOk  bool          `json:"head_ref_ok"`
	Events     []*Event      `json:"events"`
	EventsMore bool          `json:"events_more"`
	// Locked/LockMode/LockReason are the M2 moderator lock (additive to the
	// closed/merged conversation lock).
	Locked     bool   `json:"locked"`
	LockMode   string `json:"lock_mode,omitempty"`
	LockReason string `json:"lock_reason,omitempty"`
	// DescriptionHidden carries the moderation marker when the PR
	// description's opened-event target is hidden. PR.Body is already
	// omitted for non-moderators by the read verb.
	DescriptionHidden *HiddenInfo `json:"description_hidden,omitempty"`
	// ModerationRevision folds into the view's ETag (a hide must never 304
	// a stale body).
	ModerationRevision int `json:"-"`
}

// GetPR reads header + pr.json + live mergeable. Auth: read. Recovery (§3):
// re-verifies reachability and re-publishes refs/pull/<num>/head
// idempotently when absent; HeadRefOk reports the outcome (the UI shows
// head_ref pending until then). afterSeq/n window the timeline older on
// demand (afterSeq ≤ 0 = newest; n ≤ 0 defaults 50, capped at 200).
func (s *Service) GetPR(ctx context.Context, owner, repo string, num int, p auth.Principal, afterSeq, n int) (*PullView, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
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
	view := &PullView{Thread: th, PR: pr}
	events, more, eerr := s.eventWindow(ctx, owner, repo, num, afterSeq, n)
	if eerr != nil {
		return nil, eerr
	}
	view.Events, view.EventsMore = events, more
	mv, merr := s.moderationView(ctx, owner, repo, num, p)
	if merr != nil {
		return nil, merr
	}
	view.applyModeration(mv, num)
	if s.Git == nil || s.Dirs == nil {
		view.Mergeable = unknownMergeable(pr, s.nowUTC().Format(dateTimeFmt))
		return view, nil
	}
	// Live shas (2 git calls through the bounded pool — the §4
	// stamp comparison; neither blocks a read on git beyond the pool).
	baseDir, derr := s.Dirs.Dir(ctx, pr.Base.Repo)
	if derr != nil {
		view.Mergeable = unknownMergeable(pr, s.nowUTC().Format(dateTimeFmt))
		return view, nil
	}
	headDir, derr := s.Dirs.Dir(ctx, pr.Head.Repo)
	if derr != nil {
		view.Mergeable = unknownMergeable(pr, s.nowUTC().Format(dateTimeFmt))
		return view, nil
	}
	baseLive, berr := s.Git.ResolveRef(ctx, baseDir, pr.Base.Ref)
	headLive, herr := s.Git.ResolveRef(ctx, headDir, pr.Head.Ref)
	if berr != nil || herr != nil {
		// A deleted branch reports head.ref retained as snapshot (§3:
		// deleted branch ⇒ head sha retained; GET still serves).
		if berr == nil {
			view.BaseLive = baseLive
		} else {
			view.BaseLive = pr.Base.SHA
		}
		if herr == nil {
			view.HeadLive = headLive
		} else {
			view.HeadLive = pr.Head.SHA
		}
		view.Mergeable = unknownMergeable(pr, s.nowUTC().Format(dateTimeFmt))
		view.HeadRefOk = s.headRefMatches(ctx, pr, view.HeadLive)
		return view, nil
	}
	view.BaseLive = baseLive
	view.HeadLive = headLive
	// Head-sha drift (normal push or force-push, §6): refresh pr.json when
	// the live head moved, record head_force_pushed evidence, enqueue
	// mergeability. Best-effort on the read path (failures degrade to
	// unknown, never a 500 for the reader).
	if headLive != pr.Head.SHA {
		s.refreshHead(ctx, owner, repo, pr, th, headLive, p)
		if npr, _, nerr := s.loadPR(ctx, owner, repo, num); nerr == nil && npr != nil {
			pr = npr
			view.PR = pr
		}
		if nth, _, nerr := s.loadThread(ctx, owner, repo, num); nerr == nil && nth != nil {
			th = nth
			view.Thread = th
		}
	}
	// Recovery: re-publish refs/pull/<num>/head idempotently when absent.
	view.HeadRefOk = s.ensurePullHead(ctx, pr, headLive)
	// Stamped cache: match ⇒ serve; mismatch ⇒ unknown + enqueue.
	cached, _, _ := s.loadMergeable(ctx, owner, repo, num)
	if cached != nil && cached.BaseRef == pr.Base.Ref && cached.BaseSHA == baseLive && cached.HeadSHA == headLive {
		view.Mergeable = cached
		return view, nil
	}
	view.Mergeable = &MergeableDoc{
		BaseRef: pr.Base.Ref, BaseSHA: baseLive, HeadSHA: headLive,
		MergeBase: cachedMergeBase(cached), State: MergeableUnknown,
		Conflicts: []string{}, Rebaseable: true, ComputedAt: s.nowUTC().Format(dateTimeFmt),
	}
	s.enqueueMergeable(ctx, owner, repo, num)
	return view, nil
}

// eventWindow returns at most n events newest-first with seq < afterSeq
// (afterSeq ≤ 0 = newest), plus whether older remain. n ≤ 0 defaults 50,
// capped at 200 (P5 page sizes). One bounded prefix LIST per call —
// collaboration page, never the git hot path.
func (s *Service) eventWindow(ctx context.Context, owner, repo string, num, afterSeq, n int) ([]*Event, bool, error) {
	if n <= 0 {
		n = 50
	}
	if n > 200 {
		n = 200
	}
	events, err := s.scanEvents(ctx, owner, repo, num)
	if err != nil {
		return nil, false, err
	}
	var filtered []*Event
	for i := len(events) - 1; i >= 0; i-- {
		if afterSeq > 0 && events[i].Seq >= afterSeq {
			continue
		}
		filtered = append(filtered, events[i])
		if len(filtered) > n {
			break
		}
	}
	more := len(filtered) > n
	if more {
		filtered = filtered[:n]
	}
	if filtered == nil {
		filtered = []*Event{}
	}
	return filtered, more, nil
}

// scanEvents lists every event of one thread in seq order (P3: timeline
// reads by seq order, not density — gaps skipped).
func (s *Service) scanEvents(ctx context.Context, owner, repo string, num int) ([]*Event, error) {
	prefix := EventsPrefix(owner, repo, num)
	var keys []string
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		keys = append(keys, m.Key)
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(keys)
	out := make([]*Event, 0, len(keys))
	for _, k := range keys {
		raw, _, err := s.getJSON(ctx, k)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		ev, perr := parseEvent(raw)
		if perr != nil {
			return nil, perr
		}
		out = append(out, ev)
	}
	return out, nil
}

// cachedMergeBase keeps the last merge base across unknown stamps (display
// continuity; the stamp triple is what invalidates).
func cachedMergeBase(cached *MergeableDoc) string {
	if cached == nil {
		return ""
	}
	return cached.MergeBase
}

// headRefMatches checks refs/pull/<num>/head == sha without publishing.
func (s *Service) headRefMatches(ctx context.Context, pr *PRDoc, sha string) bool {
	if s.Git == nil || s.Dirs == nil || s.Refs == nil {
		return false
	}
	dir, err := s.Dirs.Dir(ctx, pr.Base.Repo)
	if err != nil {
		return false
	}
	got, err := s.Git.ResolveRef(ctx, dir, PullHeadRef(pr.Num))
	if err != nil {
		return false
	}
	return got == sha
}

// ensurePullHead publishes refs/pull/<num>/head idempotently (§3): a
// missing ref is created when the head is reachable; a DRIFTED server ref
// is CAS-forwarded to the live head (never forced) — the ref tracks the PR
// head for fetchers, like any topic ref. Unreachable cross-fork heads
// resolve through the fork (§7). Returns whether the ref matches now.
func (s *Service) ensurePullHead(ctx context.Context, pr *PRDoc, headLive string) bool {
	if s.Refs == nil || s.Git == nil || s.Dirs == nil {
		return pr.HeadPublished
	}
	dir, err := s.Dirs.Dir(ctx, pr.Base.Repo)
	if err != nil {
		return false
	}
	meta := map[string]string{"principal": pr.Head.Repo, "agent": "pulls"}
	if got, rerr := s.Git.ResolveRef(ctx, dir, PullHeadRef(pr.Num)); rerr == nil {
		if got == headLive {
			return true
		}
		reachable, rerr := s.Git.Reachable(ctx, dir, headLive)
		if rerr != nil || !reachable {
			return false
		}
		if uerr := s.Refs.UpdateRef(ctx, pr.Base.Repo, PullHeadRef(pr.Num), got, headLive, meta); uerr != nil {
			return false
		}
		return true
	}
	reachable, rerr := s.Git.Reachable(ctx, dir, headLive)
	if rerr != nil || !reachable {
		return false
	}
	if cerr := s.Refs.CreateRef(ctx, pr.Base.Repo, PullHeadRef(pr.Num), headLive, meta); cerr != nil {
		return false
	}
	return true
}

// --- list --------------------------------------------------------------------

// PROut is one row of GET …/pulls (§8): the index card enriched from pr.json.
// Merged is always present (never omitempty — the PROut/Draft discipline):
// additive per law 5, old clients ignore the unknown field.
type PROut struct {
	Num       int      `json:"num"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	Labels    []string `json:"labels"`
	Assignees []string `json:"assignees"`
	Author    string   `json:"author"`
	// CommentCount comes from the shared index card (P4), like the issue
	// list's card: the two list rows show the same anatomy.
	CommentCount int    `json:"comment_count"`
	BaseRef      string `json:"base_ref"`
	HeadRef      string `json:"head_ref"`
	HeadSHA      string `json:"head_sha"`
	Draft        bool   `json:"draft"`
	Merged       bool   `json:"merged"`
	UpdatedAt    string `json:"updated_at"`
}

// ListFilter shapes GET …/pulls (§8): state/labels/base/head/sort/n/after.
type ListFilter struct {
	State  string
	Labels []string // AND semantics (02 §7)
	Base   string
	Head   string
	Sort   string // updated | created — parsed + validated, but the render is always number-descending (issue #48)
	After  int
	N      int
}

// ListResult is the paged PR list answer.
type ListResult struct {
	Pulls []PROut `json:"pulls"`
	More  bool    `json:"more"`
}

// ListPRs serves the PR list index-first (P4): the shared index supplies
// nums in newest-activity order; each row is enriched from its pr.json
// sidecar (one GET per listed row — page-bounded, never a LIST). Base/head
// filters apply post-enrichment. Merged rides that same sidecar read (law 6:
// zero new round trips — no per-row probe beyond the enrichment GET the row
// already costs). Render order is ALWAYS number-descending
// (newest PR first), regardless of the state filter — the merged open +
// closed_recent pool is re-sorted by num before windowing (same rule as 02
// issue #48). Auth: read.
func (s *Service) ListPRs(ctx context.Context, owner, repo string, p auth.Principal, f ListFilter) (*ListResult, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	n := f.N
	if n <= 0 {
		n = 50
	}
	if n > 100 {
		n = 100
	}
	ix, _, err := s.loadIndex(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	pool := append(append([]Card{}, ix.Open...), ix.ClosedRecent...)
	sortCardsByNum(pool)
	var rows []PROut
	for _, c := range pool {
		if c.Kind != "pr" {
			continue
		}
		if f.State != "" && c.State != f.State {
			continue
		}
		if !hasAllLabels(c.Labels, f.Labels) {
			continue
		}
		pr, _, perr := s.loadPR(ctx, owner, repo, c.Num)
		if perr != nil || pr == nil {
			continue
		}
		if f.Base != "" && pr.Base.Ref != f.Base {
			continue
		}
		if f.Head != "" && pr.Head.Ref != f.Head {
			continue
		}
		rows = append(rows, PROut{
			Num: pr.Num, Title: c.Title, State: c.State,
			Labels: nonNilStr(c.Labels), Assignees: nonNilStr(c.Assignees), Author: c.Author,
			CommentCount: c.CommentCount,
			BaseRef:      pr.Base.Ref, HeadRef: pr.Head.Ref, HeadSHA: pr.Head.SHA,
			Draft: pr.Draft, Merged: pr.Merged, UpdatedAt: c.UpdatedAt,
		})
	}
	if rows == nil {
		rows = []PROut{}
	}
	page, more := windowRows(rows, f.After, n)
	return &ListResult{Pulls: page, More: more}, nil
}

// hasAllLabels is the shared AND label predicate (02 §7): a PR matches
// when every requested label is on its index card (case-insensitive).
func hasAllLabels(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if strings.EqualFold(h, w) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// windowRows slices the number-descending rows after the after-num cursor
// (unknown cursor ⇒ from the top). Callers always pass a non-nil pool
// (ListPRs normalizes), so the page is non-nil by construction.
func windowRows(pool []PROut, after, n int) ([]PROut, bool) {
	start := 0
	if after > 0 {
		start = len(pool)
		for i, r := range pool {
			if r.Num == after {
				start = i + 1
				break
			}
		}
	}
	end := start + n
	more := false
	if end < len(pool) {
		more = true
	} else {
		end = len(pool)
	}
	return pool[start:end], more
}

// threadLocked reports whether a PR thread is read-only for conversation
// writes (Forgejo #594): merged or plain-closed. Merge stamps StateClosed
// alongside Merged in the same header CAS (merge.go step 6), so the state
// check alone covers merged — the pr.Merged arm is belt-and-braces for a
// header whose merge stamp is still in flight. Keys on CURRENT state:
// reopening a closed-but-unmerged PR restores commenting; merged is
// terminal. Checked inside the CAS mutator (or against already-loaded
// docs), so the gate adds no store round trip (law 6).
func threadLocked(th *Thread, pr *PRDoc) bool {
	if th.State != StateOpen {
		return true
	}
	return pr != nil && pr.Merged
}

// --- update + comment ---------------------------------------------------------

// PRPatch carries the PUT fields (§8): title/body/state/draft plus the
// 02-owned thread metadata (labels/assignees/milestone) through Metadata.
// Unknown keys are rejected at the HTTP layer before this is built.
type PRPatch struct {
	Title    *string
	Body     *string
	State    *string
	Draft    *bool
	Metadata *ThreadMetaPatch
}

// UpdatePR applies title/body/state/draft edits (§8 PUT). Title/state/draft
// append events; body updates the pr.json description (additive optional field —
// P3 events are never rewritten). Close/reopen append state events; draft
// flips append draft_changed events. Auth: title/state/body/draft — author or
// triage (triage may close others'). Draft is orthogonal to open/closed (a
// closed-but-unmerged PR may flip either way); merged is terminal for draft
// and state alike.
func (s *Service) UpdatePR(ctx context.Context, owner, repo string, num int, actor auth.Principal, p PRPatch) (*Thread, *PRDoc, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, nil, err
	}
	if err := s.requireNotBlocked(ctx, owner, repo, actor); err != nil {
		return nil, nil, err
	}
	th, _, err := s.loadThread(ctx, owner, repo, num)
	if err != nil {
		return nil, nil, err
	}
	if th == nil || th.Kind != "pr" {
		return nil, nil, fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	pr, _, err := s.loadPR(ctx, owner, repo, num)
	if err != nil {
		return nil, nil, err
	}
	if pr == nil {
		return nil, nil, fmt.Errorf("%w: %d", ErrNotFound, num)
	}
	who := normPrincipal(actor.Name)
	isAuthor := normPrincipal(th.Author) == who
	triage := s.requireRole(ctx, owner, repo, actor, "triage") == nil
	canMod := isAuthor || triage

	var title *string
	if p.Title != nil {
		if !canMod {
			return nil, nil, fmt.Errorf("%w: title changes need author or triage", ErrForbidden)
		}
		t, verr := validateTitle(*p.Title)
		if verr != nil {
			return nil, nil, verr
		}
		if t != th.Title {
			title = &t
		}
	}
	var state *string
	if p.State != nil {
		if !canMod {
			return nil, nil, fmt.Errorf("%w: state changes need author or triage", ErrForbidden)
		}
		want := strings.ToLower(strings.TrimSpace(*p.State))
		if want != StateOpen && want != StateClosed {
			return nil, nil, fmt.Errorf("%w: state must be open|closed, got %q", ErrInvalid, *p.State)
		}
		if want != th.State {
			if want == StateClosed && pr.Merged {
				return nil, nil, fmt.Errorf("%w: pull request #%d is merged", ErrConflict, num)
			}
			if want == StateOpen && pr.Merged {
				return nil, nil, fmt.Errorf("%w: pull request #%d is merged", ErrConflict, num)
			}
			state = &want
		}
	}
	if p.Body != nil {
		if !canMod {
			return nil, nil, fmt.Errorf("%w: body changes need author or triage", ErrForbidden)
		}
		if err := validateBody(*p.Body); err != nil {
			return nil, nil, err
		}
	}
	var draft *bool
	if p.Draft != nil {
		if !canMod {
			return nil, nil, fmt.Errorf("%w: draft changes need author or triage", ErrForbidden)
		}
		if pr.Merged {
			return nil, nil, fmt.Errorf("%w: pull request #%d is merged", ErrConflict, num)
		}
		if *p.Draft != pr.Draft {
			draft = p.Draft
		}
	}
	now := s.nowUTC().Format(dateTimeFmt)
	if title != nil {
		from := th.Title
		to := *title
		nt, _, aerr := s.appendEvent(ctx, owner, repo, num, func(t *Thread, seq int) (*Event, error) {
			t.NextEventSeq = seq + 1
			t.UpdatedAt = now
			t.Title = to
			t.Participants = uniqSorted(append(t.Participants, who))
			t.Version++
			return &Event{Seq: seq, Type: EventTitleChanged, Actor: who, At: now, From: strPtr(from), To: strPtr(to)}, nil
		})
		if aerr != nil {
			return nil, nil, aerr
		}
		th = nt
	}
	if state != nil {
		from := th.State
		to := *state
		nt, _, aerr := s.appendEvent(ctx, owner, repo, num, func(t *Thread, seq int) (*Event, error) {
			t.NextEventSeq = seq + 1
			t.UpdatedAt = now
			t.State = to
			t.Participants = uniqSorted(append(t.Participants, who))
			t.Version++
			return &Event{Seq: seq, Type: EventStateChanged, Actor: who, At: now, From: strPtr(from), To: strPtr(to)}, nil
		})
		if aerr != nil {
			return nil, nil, aerr
		}
		th = nt
		action := "reopened"
		class := "reopened"
		if to == StateClosed {
			action, class = "closed", "closed"
		}
		s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: class, Actor: who, PullNum: num, Recipients: prParticipants(th, who)})
		s.stream(ctx, StreamEvent{Name: "pull", Repo: repoName(owner, repo), Action: action, Num: num, Title: th.Title, State: th.State, Author: th.Author, BaseRef: pr.Base.Ref, HeadRef: pr.Head.Ref, HeadSHA: pr.Head.SHA})
		// CI trigger: opening and closing are both lifecycle edges now
		// (PAR-014). A fork's close still runs the base file untrusted.
		if to == StateOpen {
			s.emitPullLifecycle(ctx, repoName(owner, repo), th, pr, "reopened", who)
		} else {
			s.emitPullLifecycle(ctx, repoName(owner, repo), th, pr, "closed", who)
		}
	}
	if p.Body != nil || draft != nil {
		// Owned-delta re-apply (§2.3: UpdatePR owns body + draft): the
		// updater re-loads the fresh doc and sets only these two fields, so a
		// concurrently-landed merge outcome or head refresh survives both a
		// CAS retry and the no-retry happy path.
		applied := draft != nil
		updated, uerr := s.updatePR(ctx, owner, repo, num, func(cur *PRDoc) (bool, error) {
			changed := false
			if draft != nil {
				// Merged is terminal for draft (mirrors the state flips): a
				// merge landing between the two reads still refuses here.
				// Body edits stay allowed on merged PRs, as before.
				if cur.Merged {
					return false, fmt.Errorf("%w: pull request #%d is merged", ErrConflict, num)
				}
				applied = *draft != cur.Draft // a concurrent flip already reached it
				if applied {
					cur.Draft = *draft
					changed = true
				}
			}
			if p.Body != nil && *p.Body != cur.Body {
				cur.Body = *p.Body
				changed = true
			}
			return changed, nil
		})
		if uerr != nil {
			return nil, nil, uerr
		}
		pr = updated
		if !applied {
			draft = nil // no draft_changed event for a no-op flip
		}
	}
	if draft != nil {
		// The pr.json flip above is the mutation (it precedes the event so
		// a CAS shortfall fails with no phantom event); the draft_changed
		// event below is the narration, mirroring the state flips.
		from, to := "ready", "draft"
		if !*draft {
			from, to = "draft", "ready"
		}
		nt, _, aerr := s.appendEvent(ctx, owner, repo, num, func(t *Thread, seq int) (*Event, error) {
			t.NextEventSeq = seq + 1
			t.UpdatedAt = now
			t.Participants = uniqSorted(append(t.Participants, who))
			t.Version++
			return &Event{Seq: seq, Type: EventDraftChanged, Actor: who, At: now, From: strPtr(from), To: strPtr(to)}, nil
		})
		if aerr != nil {
			return nil, nil, aerr
		}
		th = nt
		action, class := "converted_to_draft", "converted_to_draft"
		if !*draft {
			action, class = "ready_for_review", "ready_for_review"
		}
		s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: class, Actor: who, PullNum: num, Recipients: prParticipants(th, who)})
		s.stream(ctx, StreamEvent{Name: "pull", Repo: repoName(owner, repo), Action: action, Num: num, Title: th.Title, State: th.State, Author: th.Author, BaseRef: pr.Base.Ref, HeadRef: pr.Head.Ref, HeadSHA: pr.Head.SHA})
		pr.Draft = *draft
		s.emitPullLifecycle(ctx, repoName(owner, repo), th, pr, action, who)
	}
	// A title or body edit is the `edited` edge (a body edit reaches here with
	// p.Body, the pr.json update is the owned delta above).
	if title != nil || p.Body != nil {
		s.emitPullLifecycle(ctx, repoName(owner, repo), th, pr, "edited", who)
	}
	if p.Metadata != nil {
		// 02 owns the thread-metadata fields: this call is its only writer
		// (validation, events, milestone counters, index card and
		// notifications live there). The reload picks up the new header so
		// the index upsert below cannot write a stale card back.
		if s.Metadata == nil {
			return nil, nil, fmt.Errorf("%w: thread metadata is not wired", ErrUnavailable)
		}
		if merr := s.Metadata(ctx, owner, repo, num, actor, *p.Metadata); merr != nil {
			return nil, nil, merr
		}
		if nt, _, lerr := s.loadThread(ctx, owner, repo, num); lerr == nil && nt != nil {
			th = nt
		}
	}
	s.updateIndex(ctx, owner, repo, prCardOf(th))
	return th, pr, nil
}

// prParticipants fans subscribed notifications to participants minus actor.
func prParticipants(th *Thread, actor string) []string {
	var out []string
	for _, p := range th.Participants {
		if p != "" && p != actor {
			out = append(out, p)
		}
	}
	return out
}

// AddComment appends a commented event to a PR thread (the conversation
// box; same P3 two-step as issues). Auth: read (authenticated). Refused
// 409 on a merged or closed PR (ErrLocked); reopening a
// closed-but-unmerged PR restores commenting.
func (s *Service) AddComment(ctx context.Context, owner, repo string, num int, actor auth.Principal, body string) (*Event, error) {
	if err := requireAuthenticated(actor); err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	if err := s.requireNotBlocked(ctx, owner, repo, actor); err != nil {
		return nil, err
	}
	if err := s.requireUnlocked(ctx, owner, repo, num, actor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("%w: body must not be empty", ErrInvalid)
	}
	if err := validateBody(body); err != nil {
		return nil, err
	}
	who := normPrincipal(actor.Name)
	now := s.nowUTC().Format(dateTimeFmt)
	// The sidecar read is hoisted ahead of the CAS (it already happened
	// below, result discarded — same read count, law 6 neutral) so the
	// mutator can consult pr.Merged for a header whose merge stamp is
	// still in flight. A merge landing between the two reads is still
	// caught: its header CAS stamps StateClosed, which the mutator sees
	// on the fresh doc.
	pr, _, _ := s.loadPR(ctx, owner, repo, num)
	th, ev, err := s.appendEvent(ctx, owner, repo, num, func(t *Thread, seq int) (*Event, error) {
		if t.Kind != "pr" {
			return nil, fmt.Errorf("%w: %d", ErrNotFound, num)
		}
		if threadLocked(t, pr) {
			return nil, fmt.Errorf("%w: pull request #%d", ErrLocked, num)
		}
		t.NextEventSeq = seq + 1
		t.UpdatedAt = now
		t.CommentCount++
		t.Participants = uniqSorted(append(t.Participants, who))
		t.Version++
		return &Event{Seq: seq, Type: EventCommented, Actor: who, At: now, Body: strPtr(body)}, nil
	})
	if err != nil {
		return nil, err
	}
	s.updateIndex(ctx, owner, repo, prCardOf(th))
	s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: "subscribed", Actor: who, PullNum: num, Recipients: prParticipants(th, who)})
	s.emitMentioned(ctx, owner, repo, num, who, body)
	return ev, nil
}

// emitMentioned fans "mentioned" for @-parsed principals and @org/team
// spellings in a PR opened/commented body (06 §3; the consumer validates
// and expands). Bodies without tokens emit nothing.
func (s *Service) emitMentioned(ctx context.Context, owner, repo string, num int, actor, body string) {
	if body == "" {
		return
	}
	users, teams := identity.ParseMentions(body)
	var recips []string
	for _, m := range users {
		if m != actor && identity.ValidPrincipal(m) {
			recips = append(recips, m)
		}
	}
	for _, t := range teams {
		if t != actor {
			recips = append(recips, t)
		}
	}
	if len(recips) == 0 {
		return
	}
	s.emit(ctx, NotifyEvent{Repo: repoName(owner, repo), Class: "mentioned", Actor: actor, PullNum: num, Recipients: recips})
}

// TriggerInfo is the narrow projection the CI trigger adapter reads for a PR
// metadata edge (labeled/assigned/…): the identifiers a run needs, without the
// event window or the mergeability probe a full GetPR performs. ok=false means
// the PR vanished between the metadata write and the trigger.
type TriggerInfo struct {
	Num      int
	Title    string
	State    string
	Author   string
	BaseRef  string
	BaseSHA  string
	HeadRef  string
	HeadSHA  string
	HeadRepo string
	Draft    bool
	Merged   bool
	Fork     bool
}

// PRTriggerInfo reads the trigger projection (two exact-key GETs).
func (s *Service) PRTriggerInfo(ctx context.Context, owner, repo string, num int) (TriggerInfo, bool) {
	th, _, err := s.loadThread(ctx, owner, repo, num)
	if err != nil || th == nil || th.Kind != "pr" {
		return TriggerInfo{}, false
	}
	pr, _, err := s.loadPR(ctx, owner, repo, num)
	if err != nil || pr == nil {
		return TriggerInfo{}, false
	}
	return TriggerInfo{
		Num: th.Num, Title: th.Title, State: th.State, Author: th.Author,
		BaseRef: pr.Base.Ref, BaseSHA: pr.Base.SHA,
		HeadRef: pr.Head.Ref, HeadSHA: pr.Head.SHA, HeadRepo: pr.Head.Repo,
		Draft: pr.Draft, Merged: pr.Merged, Fork: pr.Fork != nil,
	}, true
}
