package issues

import (
	"context"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/store"
)

// This file is the read-only projection internal/search consumes for P4
// issue/PR search (docs/features/20_search.md §7, §12 decision 4/12). It adds
// no bucket family, no write path and no index: the metadata is the existing
// issues/index.json cards (with the existing header-scan fallback when the
// index is not provably complete), and the body/comment text is a bounded
// scan over the existing immutable events.
//
// Redaction happens here, before any text leaves this package: every scanned
// thread is folded through the same moderation document the page/JSON reads
// use (foldModeration), and a hidden target's body is omitted unconditionally.
// Search carries no principal (the frozen IssueSearcher seam has none), so it
// never applies the moderator's "show original" arm: a hidden body can never
// be matched, counted or excerpted by any caller (20_search.md §8 leak rules).

// SearchText is one searchable piece of a thread's text. Kind is "body" for
// the opened event (the issue description or PR description, seq 0) and
// "comment" for a commented event.
type SearchText struct {
	Seq  int
	Kind string
	Text string
}

// SearchThread is one thread selected for the bounded body/comment scan.
type SearchThread struct {
	Card  Card
	Texts []SearchText
}

// SearchProjection is the read-only answer SearchProjection returns.
//
// Cards is every issue/PR card the reader could prove (the index when it is
// complete, merged with a header scan otherwise), newest number first.
// Threads holds the bounded body scan (newest-number threads first). The
// truncated flags are the honest caps: they mean a thread or event the query
// might have matched was not read, so the caller must not report complete.
type SearchProjection struct {
	Cards            []Card
	IndexComplete    bool
	Threads          []SearchThread
	ThreadsScanned   int
	EventsScanned    int
	ThreadsTruncated bool
	EventsTruncated  bool
}

// SearchProjection reads one repository's issue/PR search projection.
//
// maxThreads and maxEvents are the caller's caps (20_search.md §5.2: 50
// threads / 500 events per repository in body mode). A zero maxThreads is the
// metadata-only path: no event is read and no moderation document is fetched.
//
// It deliberately performs no read gate: its only caller (internal/search) has
// already passed the single visibility primitive for the repository. Keeping
// the gate out of this read keeps one visibility authority (20_search.md §3).
func (s *Service) SearchProjection(ctx context.Context, owner, repo string, maxThreads, maxEvents int) (SearchProjection, error) {
	ix, _, err := s.loadIndex(ctx, owner, repo)
	if err != nil {
		return SearchProjection{}, err
	}
	next := s.loadCounter(ctx, owner, repo)
	indexOK := indexComplete(ix, next)

	byNum := make(map[int]Card, len(ix.Open)+len(ix.ClosedRecent))
	for _, c := range ix.Open {
		byNum[c.Num] = c
	}
	for _, c := range ix.ClosedRecent {
		byNum[c.Num] = c
	}
	if !indexOK {
		// The index is not provably complete (no counter, a lost card write,
		// pre-projection cards, compacted history): the header scan is the
		// same fallback ListIssues uses, and the header wins over a card.
		scanned, truncated, serr := s.scanSearchCards(ctx, owner, repo)
		if serr != nil {
			return SearchProjection{}, serr
		}
		for _, c := range scanned {
			byNum[c.Num] = c
		}
		if next <= 0 && len(byNum) == 0 && !truncated {
			// An empty repository with no counter and no headers has nothing
			// missing: "no issues" is a complete answer, not an incomplete
			// index (§6.3's index_complete is about a repo that HAS cards).
			indexOK = true
		}
	}

	cards := make([]Card, 0, len(byNum))
	for _, c := range byNum {
		if c.Kind == "issue" || c.Kind == "pr" {
			cards = append(cards, c)
		}
	}
	// Newest number first is deterministic and mirrors the list surface. The
	// body cap therefore keeps the threads a reader is most likely to mean.
	sort.Slice(cards, func(i, j int) bool { return cards[i].Num > cards[j].Num })

	proj := SearchProjection{
		Cards:         cards,
		IndexComplete: indexOK,
		Threads:       []SearchThread{},
	}
	if maxThreads <= 0 {
		return proj, nil
	}
	if len(cards) > maxThreads {
		cards = cards[:maxThreads]
		proj.ThreadsTruncated = true
	}

	budget := maxEvents
	for _, c := range cards {
		if budget <= 0 {
			// The event cap stopped before this thread: it is not body-scanned,
			// so the answer must not claim completeness.
			proj.ThreadsTruncated = true
			break
		}
		events, more, eerr := s.scanEventsBounded(ctx, owner, repo, c.Num, budget)
		if eerr != nil {
			return SearchProjection{}, eerr
		}
		budget -= len(events)
		proj.ThreadsScanned++
		proj.EventsScanned += len(events)
		if more {
			proj.EventsTruncated = true
		}

		texts := make([]SearchText, 0, 2)
		hasBody := false
		for _, ev := range events {
			if ev.Body == nil {
				continue
			}
			if ev.Type == EventOpened || ev.Type == EventCommented {
				hasBody = true
				break
			}
		}
		var hidden map[string]HiddenInfo
		if hasBody {
			// Fold the moderation document before matching. A read failure
			// fails the projection closed (16_moderation §10.3): never serve
			// text we cannot prove visible.
			hidden, eerr = s.searchModerationView(ctx, owner, repo, c.Num)
			if eerr != nil {
				return SearchProjection{}, eerr
			}
		}
		for _, ev := range events {
			if ev.Body == nil {
				continue
			}
			var kind string
			switch ev.Type {
			case EventOpened:
				kind = "body"
			case EventCommented:
				kind = "comment"
			default:
				continue
			}
			if _, isHidden := hidden[hiddenTarget(c.Num, ev.Seq)]; isHidden {
				continue // redacted: never matched, excerpted or returned
			}
			texts = append(texts, SearchText{Seq: ev.Seq, Kind: kind, Text: *ev.Body})
		}
		proj.Threads = append(proj.Threads, SearchThread{Card: c, Texts: texts})
	}
	if proj.EventsScanned >= maxEvents && len(proj.Threads) < len(cards) {
		proj.ThreadsTruncated = true
	}
	return proj, nil
}

// scanSearchCards scans …/<num>/thread.json headers for both kinds. It is the
// same LIST fallback as ListIssues' scanHeaders, but PR threads ride through
// (search covers both families) and a corrupt header fails the read instead of
// silently shrinking the corpus.
func (s *Service) scanSearchCards(ctx context.Context, owner, repo string) ([]Card, bool, error) {
	var keys []string
	if err := s.Store.List(ctx, IssuesPrefix(owner, repo), "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, "/thread.json") {
			keys = append(keys, m.Key)
		}
		return nil
	}); err != nil {
		return nil, false, err
	}
	sort.Strings(keys)
	out := make([]Card, 0, len(keys))
	for _, k := range keys {
		if len(out) >= headerScanCap {
			return out, true, nil
		}
		raw, _, err := s.getJSON(ctx, k)
		if err != nil {
			return nil, false, err
		}
		if raw == nil {
			continue
		}
		t, perr := parseThread(raw)
		if perr != nil {
			return nil, false, perr
		}
		if t.Kind != "issue" && t.Kind != "pr" {
			continue
		}
		out = append(out, cardOf(t))
	}
	return out, false, nil
}

// scanEventsBounded reads at most limit events of one thread, oldest first
// (seq order). The LIST is bounded to the same prefix the timeline uses; the
// reads are what the cap bounds, so a 10,000-comment thread costs at most
// limit event GETs. more reports that the thread has events beyond the read
// window.
func (s *Service) scanEventsBounded(ctx context.Context, owner, repo string, num, limit int) ([]*Event, bool, error) {
	if limit <= 0 {
		return nil, true, nil
	}
	keys := make([]string, 0, limit+1)
	if err := s.Store.List(ctx, EventsPrefix(owner, repo, num), "", func(m store.ObjectMeta) error {
		keys = append(keys, m.Key)
		return nil
	}); err != nil {
		return nil, false, err
	}
	sort.Strings(keys)
	more := len(keys) > limit
	if more {
		keys = keys[:limit]
	}
	out := make([]*Event, 0, len(keys))
	for _, k := range keys {
		raw, _, err := s.getJSON(ctx, k)
		if err != nil {
			return nil, false, err
		}
		if raw == nil {
			continue
		}
		ev, perr := parseEvent(raw)
		if perr != nil {
			return nil, false, perr
		}
		out = append(out, ev)
	}
	return out, more, nil
}

// searchModerationView folds the repo moderation document into a search read
// with reveal=false. It shares foldModeration with moderationView so both
// reads parse the same document the same way; only the moderator arm differs,
// and search never takes it (see the file comment).
func (s *Service) searchModerationView(ctx context.Context, owner, repo string, num int) (map[string]HiddenInfo, error) {
	mv := modView{hidden: map[string]HiddenInfo{}}
	if s.Moderation == nil {
		return mv.hidden, nil
	}
	if err := s.foldModeration(ctx, owner, repo, num, &mv); err != nil {
		return nil, err
	}
	return mv.hidden, nil
}
