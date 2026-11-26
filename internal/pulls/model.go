package pulls

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Sentinel errors (07 §2: mapped to plain-text statuses in http.go).
var (
	// ErrNotFound marks an unknown PR/thread/ref (→ 404).
	ErrNotFound = errors.New("unknown pull request")
	// ErrInvalid marks a bad request body/field (→ 400).
	ErrInvalid = errors.New("invalid pull request")
	// ErrUnauthorized marks anonymous-denied access (→ 401 + Bearer).
	ErrUnauthorized = errors.New("authentication required")
	// ErrForbidden marks authenticated-but-insufficient access (→ 403).
	ErrForbidden = errors.New("forbidden")
	// ErrConflict marks state conflicts: duplicate open PR, stale CAS,
	// milestone-style 409s (→ 409).
	ErrConflict = errors.New("conflict")
	// ErrLocked marks conversation writes on a merged or closed PR
	// (→ 409). The lock keys on current thread state (Forgejo #594):
	// reopening a closed-but-unmerged PR restores commenting; merged is
	// terminal. No maintainer/admin override in v1 (named decision,
	// 03_pull_requests.md).
	ErrLocked = errors.New("pull request is closed or merged: commenting is locked")
	// ErrUnprocessable marks resolvable-but-unusable refs: unknown revision,
	// unreachable head (→ 422).
	ErrUnprocessable = errors.New("unprocessable")
	// ErrUnavailable marks a down dependency (git pool, identity state)
	// (→ 503 + Retry-After: 15).
	ErrUnavailable = errors.New("temporarily unavailable")
	// ErrCorrupt marks an unreadable bucket object (→ 500-class).
	ErrCorrupt = errors.New("corrupt object")
	// ErrMethodNotAllowed marks a merge strategy the base ref's policy
	// forbids (PLAN-V2 §2.2 merge_methods; → 409, naming the allowed set).
	// A named error, not a bare string: callers and tests match it and the
	// message keeps the rule that narrowed the set.
	ErrMethodNotAllowed = errors.New("merge method not allowed")
	// ErrHeadBehindBase marks a merge refused by require_up_to_date
	// (PLAN-V2 §2.2): the head does not contain the base tip, so the merge
	// would land on a stale tree. Also → 409; the PR page's update-branch
	// control is the fix (the refusal message names it).
	ErrHeadBehindBase = errors.New("head is behind base — update the branch first")
)

// statusFor maps a service error onto its HTTP status.
func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return 404
	case errors.Is(err, ErrInvalid):
		return 400
	case errors.Is(err, ErrUnauthorized):
		return 401
	case errors.Is(err, ErrForbidden):
		return 403
	case errors.Is(err, ErrConflict):
		return 409
	case errors.Is(err, ErrLocked):
		return 409
	case errors.Is(err, ErrUnprocessable):
		return 422
	case errors.Is(err, ErrUnavailable):
		return 503
	default:
		return 500
	}
}

// Thread states.
const (
	StateOpen   = "open"
	StateClosed = "closed"
)

// Merge strategies (§5, exact set).
const (
	StrategyMerge  = "merge"
	StrategySquash = "squash"
	StrategyRebase = "rebase"
)

// Mergeable states (§2.2, exact set).
const (
	MergeableClean    = "clean"
	MergeableDirty    = "dirty"
	MergeableBehind   = "behind"
	MergeableUpToDate = "up_to_date"
	MergeableUnknown  = "unknown"
)

// Endpoint describes one PR ref endpoint: the repo it lives in, the ref,
// and the resolved sha.
type Endpoint struct {
	Repo string `json:"repo"` // "owner/name"
	Ref  string `json:"ref"`  // full ref name
	SHA  string `json:"sha"`  // full hex at open (or at last refresh)
}

// ForkInfo marks a cross-fork head: the fork repo holding the head ref.
type ForkInfo struct {
	Repo string `json:"repo"` // "owner/name" of the fork
}

// PRDoc is pulls/<num>/pr.json (§2.1, CAS'd): the PR-sidecar snapshot (refs
// + shas at open, merge outcome). thread.json stays the P3 header exactly
// (02 owns its shape); PR-specific fields MUST NOT leak into it. Body is an
// additive optional description field (14 §14.12 field rule: the opened
// event carries the original body; Body is the editable view).
type PRDoc struct {
	Num               int       `json:"num"`
	Kind              string    `json:"kind"` // always "pr"
	Base              Endpoint  `json:"base"`
	Head              Endpoint  `json:"head"`
	Fork              *ForkInfo `json:"fork"`
	Body              string    `json:"body,omitempty"`
	Merged            bool      `json:"merged"`
	MergedAt          *string   `json:"merged_at"`
	MergedBy          *string   `json:"merged_by"`
	MergeCommitSHA    *string   `json:"merge_commit_sha"`
	MergeStrategy     *string   `json:"merge_strategy"`
	HeadForcePushedAt *string   `json:"head_force_pushed_at"`
	// HeadSeq is the WAL log sequence of the last ref event applied to
	// Head.SHA (the event-ordering watermark, 15 §2.3): a later delivery with
	// a lower sequence is stale and must never move the head backwards.
	// Zero means no event has been applied yet (the head came from the open
	// snapshot or a live ref read).
	HeadSeq       uint64 `json:"head_seq,omitempty"`
	HeadPublished bool   `json:"head_published"`
	Draft         bool   `json:"draft"`
	Version       int    `json:"version"`
}

// MergeableDoc is pulls/<num>/mergeable.json (§2.2, derived
// Create-then-CAS overwrite): the stamp triple (base_ref, base_sha,
// head_sha) IS the invalidation key — a reader compares it against the live
// ref shas; any mismatch ⇒ the cache is stale. conflicts[] is populated
// only when state is dirty.
type MergeableDoc struct {
	BaseRef    string   `json:"base_ref"`
	BaseSHA    string   `json:"base_sha"`
	HeadSHA    string   `json:"head_sha"`
	MergeBase  string   `json:"merge_base"`
	State      string   `json:"state"`
	Conflicts  []string `json:"conflicts"`
	Rebaseable bool     `json:"rebaseable"`
	ComputedAt string   `json:"computed_at"`
}

// ForkEntry is one child row of the parent-side fork index.
type ForkEntry struct {
	Repo     string `json:"repo"` // "owner/name" of the fork
	ForkedAt string `json:"forked_at"`
}

// ForksIndex is repos/<o>/<r>/meta/forks.json (CAS'd): the parent-side fork
// index listing children. The maintain unit consults children's manifests
// before deleting superseded packs (GC rule, §7); grand-children are
// discovered transitively in the same pass (bounded by the probe cap in
// internal/maintain/forknet.go; exhaustion defers the sweep fail-closed).
type ForksIndex struct {
	Version int         `json:"version"`
	Forks   []ForkEntry `json:"forks"`
}

// ForkDoc is repos/<o2>/<r2>/fork.json (Create once, then CAS'd for
// Root backfill and merged_upstream_at): fork-side provenance. Root is the network root
// ancestor (issue #424 amendment: the network is a tree — Root lets any
// node answer the whole network; a direct fork of a root has Root ==
// Parent; additive per 14 §14.12, old readers ignore it).
type ForkDoc struct {
	Parent           string  `json:"parent"` // "owner/name" of the parent
	Root             string  `json:"root,omitempty"`
	ForkedAt         string  `json:"forked_at"`
	MergedUpstreamAt *string `json:"merged_upstream_at"`
	Version          int     `json:"version"`
}

// Thread is the shared P3 header as pulls reads it (02 owns the shape, §1.1;
// PR threads carry no PR fields by contract, §2.1). Only the fields pulls
// needs are modeled, and every key it does not name is captured on decode and
// re-emitted on encode: appendEvent rewrites thread.json on every PR write, so
// a whitelist mirror would strip the NEXT additive field 02 or 04 adds — the
// same defect class the index mirror fixed in P6. Law 5: additive JSON fields
// round-trip unchanged. The three review-owned fields (04 §7) are preserved
// opaquely: pulls round-trips them verbatim on every header write but never
// interprets them (04 owns the semantics; the merge-time gate reads the
// events, never this cache).
type Thread struct {
	Num          int      `json:"num"`
	Kind         string   `json:"kind"`
	Title        string   `json:"title"`
	State        string   `json:"state"`
	StateReason  *string  `json:"state_reason"`
	Author       string   `json:"author"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	Labels       []string `json:"labels"`
	Assignees    []string `json:"assignees"`
	Milestone    *string  `json:"milestone"`
	Participants []string `json:"participants"`
	NextEventSeq int      `json:"next_event_seq"`
	CommentCount int      `json:"comment_count"`
	// ReactionSummary is 02's per-event reaction cache; pulls carries it
	// through header writes verbatim (02 owns the shape, 03 never edits it).
	ReactionSummary map[string]map[string]int `json:"reaction_summary"`
	Version         int                       `json:"version"`
	// NextReviewSeq reserves review seqs (04 §3); NextThreadNum reserves
	// tids (04 §4); ReviewSummary is 04's denormalized render cache (§6).
	NextReviewSeq int             `json:"next_review_seq,omitempty"`
	NextThreadNum int             `json:"next_thread_num,omitempty"`
	ReviewSummary json.RawMessage `json:"review_summary,omitempty"`

	unknown map[string]json.RawMessage
}

// Card is the shared P4 index projection as pulls reads it (02 owns the
// shape). PR rows are enriched from pr.json sidecars at list time.
//
// unknown holds every JSON key the struct does not name. The card rides in the
// shared issues/index.json, which pulls rewrites on every PR mutation, so a
// whitelist struct would strip the NEXT additive field a future 02 writer adds
// — the same defect class as the missing card_version. Law 5: additive JSON
// fields round-trip unchanged; UnmarshalJSON/MarshalJSON below keep them.
type Card struct {
	Num          int      `json:"num"`
	Kind         string   `json:"kind"`
	Title        string   `json:"title"`
	State        string   `json:"state"`
	StateReason  *string  `json:"state_reason"`
	Labels       []string `json:"labels"`
	Assignees    []string `json:"assignees"`
	Milestone    *string  `json:"milestone"`
	Author       string   `json:"author"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	CommentCount int      `json:"comment_count"`

	unknown map[string]json.RawMessage
}

// Index is the shared P4 list object as pulls reads it (02 owns the shape).
//
// Every PR mutation REWRITES this document (updateIndex), so the mirror must
// be lossless rather than a whitelist: 02's additive fields (card_version, the
// pin set, and whatever a future writer adds) pass through a read-modify-write
// unchanged. A lossy mirror once stripped `card_version`, making the issue
// search projection report an incomplete index after the first PR write
// (docs/features/20_search.md §6.3), and would have dropped every pin.
// Law 5: additive JSON fields round-trip unchanged.
type Index struct {
	Version          int    `json:"version"`
	CompactedThrough string `json:"compacted_through"`
	Open             []Card `json:"open"`
	ClosedRecent     []Card `json:"closed_recent"`
	// CardVersion is 02's Card-projection stamp (Forgejo #564).
	CardVersion int `json:"card_version,omitempty"`
	// Pinned is 02's repo pin set (§13).
	Pinned []int `json:"pinned,omitempty"`

	unknown map[string]json.RawMessage
}

// ---- lossless JSON mirror (law 5) -------------------------------------------
//
// pulls is a second writer of three shared 02 documents: issues/index.json,
// issues/<num>/thread.json and meta/next_num (02 owns all three shapes). Every
// field it does not name is captured on decode and re-emitted on encode, so an
// additive field added by a later writer survives the PR path. The known fields
// keep their struct order and exact output; unknown keys are appended in sorted
// order, and a document with no unknown keys marshals to the same bytes as the
// plain struct (no bookkeeping field leaks into the wire).

// cardJSONKeys / indexJSONKeys / threadJSONKeys / counterJSONKeys are the JSON
// names the structs above own. A key that matches one of them (encoding/json
// matches field names case-insensitively too) is never treated as an unknown,
// so a re-emit can never duplicate it.
var (
	cardJSONKeys = []string{
		"num", "kind", "title", "state", "state_reason", "labels", "assignees",
		"milestone", "author", "created_at", "updated_at", "comment_count",
	}
	indexJSONKeys = []string{
		"version", "compacted_through", "open", "closed_recent",
		"card_version", "pinned",
	}
	threadJSONKeys = []string{
		"num", "kind", "title", "state", "state_reason", "author", "created_at",
		"updated_at", "labels", "assignees", "milestone", "participants",
		"next_event_seq", "comment_count", "reaction_summary", "version",
		"next_review_seq", "next_thread_num", "review_summary",
	}
	counterJSONKeys = []string{"next"}
)

// UnmarshalJSON decodes the known card fields and keeps every other key.
func (c *Card) UnmarshalJSON(data []byte) error {
	type cardJSON Card // no methods: the alias avoids recursing into this one
	var known cardJSON
	if err := json.Unmarshal(data, &known); err != nil {
		return err
	}
	unknown, err := captureUnknownJSON(data, cardJSONKeys)
	if err != nil {
		return err
	}
	*c = Card(known)
	c.unknown = unknown
	return nil
}

// MarshalJSON re-emits the known card fields plus every captured unknown key.
func (c Card) MarshalJSON() ([]byte, error) {
	type cardJSON Card
	base, err := json.Marshal(cardJSON(c))
	if err != nil {
		return nil, err
	}
	return appendUnknownJSON(base, c.unknown, cardJSONKeys)
}

// UnmarshalJSON decodes the known envelope fields and keeps every other key.
func (ix *Index) UnmarshalJSON(data []byte) error {
	type indexJSON Index
	var known indexJSON
	if err := json.Unmarshal(data, &known); err != nil {
		return err
	}
	unknown, err := captureUnknownJSON(data, indexJSONKeys)
	if err != nil {
		return err
	}
	*ix = Index(known)
	ix.unknown = unknown
	return nil
}

// MarshalJSON re-emits the known envelope fields plus every captured unknown key.
func (ix Index) MarshalJSON() ([]byte, error) {
	type indexJSON Index
	base, err := json.Marshal(indexJSON(ix))
	if err != nil {
		return nil, err
	}
	return appendUnknownJSON(base, ix.unknown, indexJSONKeys)
}

// UnmarshalJSON decodes the known thread fields and keeps every other key.
func (t *Thread) UnmarshalJSON(data []byte) error {
	type threadJSON Thread
	var known threadJSON
	if err := json.Unmarshal(data, &known); err != nil {
		return err
	}
	unknown, err := captureUnknownJSON(data, threadJSONKeys)
	if err != nil {
		return err
	}
	*t = Thread(known)
	t.unknown = unknown
	return nil
}

// MarshalJSON re-emits the known thread fields plus every captured unknown key.
func (t Thread) MarshalJSON() ([]byte, error) {
	type threadJSON Thread
	base, err := json.Marshal(threadJSON(t))
	if err != nil {
		return nil, err
	}
	return appendUnknownJSON(base, t.unknown, threadJSONKeys)
}

// UnmarshalJSON decodes the known counter fields and keeps every other key, so
// the allocator's read-modify-write cannot strip an additive key.
func (c *Counter) UnmarshalJSON(data []byte) error {
	type counterJSON Counter
	var known counterJSON
	if err := json.Unmarshal(data, &known); err != nil {
		return err
	}
	unknown, err := captureUnknownJSON(data, counterJSONKeys)
	if err != nil {
		return err
	}
	*c = Counter(known)
	c.unknown = unknown
	return nil
}

// MarshalJSON re-emits `next` plus every captured unknown key.
func (c Counter) MarshalJSON() ([]byte, error) {
	type counterJSON Counter
	base, err := json.Marshal(counterJSON(c))
	if err != nil {
		return nil, err
	}
	return appendUnknownJSON(base, c.unknown, counterJSONKeys)
}

// inheritUnknown carries old's captured keys onto c (a re-projection of the
// same card must not drop a field a future writer added). c's own keys win.
func (c *Card) inheritUnknown(old Card) {
	if len(old.unknown) == 0 {
		return
	}
	if c.unknown == nil {
		c.unknown = make(map[string]json.RawMessage, len(old.unknown))
	}
	for k, v := range old.unknown {
		if _, ok := c.unknown[k]; !ok {
			c.unknown[k] = v
		}
	}
}

// captureUnknownJSON returns the object keys that known does not own. A null or
// empty object yields nil (nothing to re-emit).
func captureUnknownJSON(data []byte, known []string) (map[string]json.RawMessage, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	for k := range all {
		if ownsJSONKey(k, known) {
			delete(all, k)
		}
	}
	if len(all) == 0 {
		return nil, nil
	}
	return all, nil
}

// appendUnknownJSON inserts the unknown keys into the JSON object base without
// touching a known key. It returns base unchanged when there is nothing extra,
// so a document without extras is byte-identical to the plain struct's output.
func appendUnknownJSON(base []byte, unknown map[string]json.RawMessage, known []string) ([]byte, error) {
	keys := make([]string, 0, len(unknown))
	for k := range unknown {
		if !ownsJSONKey(k, known) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return base, nil
	}
	if len(base) == 0 || base[len(base)-1] != '}' {
		return nil, fmt.Errorf("pulls: marshal object: base is not a JSON object")
	}
	sort.Strings(keys) // stable output; JSON object order is not semantic
	out := make([]byte, 0, len(base)+len(keys)*16)
	out = append(out, base[:len(base)-1]...)
	for _, k := range keys {
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		out = append(out, ',')
		out = append(out, kb...)
		out = append(out, ':')
		out = append(out, unknown[k]...)
	}
	return append(out, '}'), nil
}

// ownsJSONKey reports whether key names one of the struct's JSON fields. The
// comparison folds case because encoding/json does.
func ownsJSONKey(key string, known []string) bool {
	for _, k := range known {
		if strings.EqualFold(key, k) {
			return true
		}
	}
	return false
}

// Counter is the shared {"next": N} allocator shape (P2).
//
// allocNum increments it on every PR open, and that read-modify-write rewrites
// the whole object: unknown keys are captured on decode and re-emitted, so an
// additive field a future 02 writer adds survives the allocation rather than
// vanishing on the next PR. Law 5: additive JSON fields round-trip unchanged.
type Counter struct {
	Next int `json:"next"`

	unknown map[string]json.RawMessage
}

// Event is one shared P3 immutable event as pulls writes it (opened,
// commented, title/state edits, merged, head_force_pushed, referenced).
type Event struct {
	Seq     int            `json:"seq"`
	Type    string         `json:"type"`
	Actor   string         `json:"actor"`
	At      string         `json:"at"`
	Body    *string        `json:"body,omitempty"`
	From    *string        `json:"from,omitempty"`
	To      *string        `json:"to,omitempty"`
	Reason  *string        `json:"reason,omitempty"`
	Added   []string       `json:"added,omitempty"`
	Removed []string       `json:"removed,omitempty"`
	Source  map[string]any `json:"source,omitempty"`
	PRNum   *int           `json:"pr_num,omitempty"`
	Keyword *string        `json:"keyword,omitempty"`
	// Hidden is the M2 moderation projection hint (never persisted): set by
	// the read verbs when a moderator hid this body. Body is omitted for
	// every non-moderator.
	Hidden *HiddenInfo `json:"hidden,omitempty"`
	// Merge carries the merge outcome on "merged" events.
	MergeCommitSHA *string `json:"merge_commit_sha,omitempty"`
	Strategy       *string `json:"strategy,omitempty"`
}

// Event types pulls writes (02's set plus the PR-side trigger). The
// metadata events are written by 02's shared thread patch (PR labels,
// assignees and milestone reuse that one implementation).
const (
	EventOpened           = "opened"
	EventCommented        = "commented"
	EventTitleChanged     = "title_changed"
	EventStateChanged     = "state_changed"
	EventLabelsChanged    = "labels_changed"
	EventAssigneesChanged = "assignees_changed"
	EventMilestoneChanged = "milestone_changed"
	EventDraftChanged     = "draft_changed"
	EventReferenced       = "referenced"
	EventMerged           = "merged"
	EventHeadForcePushed  = "head_force_pushed"
)

// Limits (§§2-3,7).
const (
	MaxTitleLen  = 256
	MaxBodyBytes = 64 << 10
)

// --- validation -----------------------------------------------------------

// validateTitle trims and bounds a PR title (1–256 chars).
func validateTitle(title string) (string, error) {
	t := strings.TrimSpace(title)
	if t == "" {
		return "", fmt.Errorf("%w: title must not be empty", ErrInvalid)
	}
	if len([]rune(t)) > MaxTitleLen {
		return "", fmt.Errorf("%w: title exceeds %d characters", ErrInvalid, MaxTitleLen)
	}
	return t, nil
}

// validateBody bounds an optional PR body (≤ 64 KiB raw text).
func validateBody(body string) error {
	if len(body) > MaxBodyBytes {
		return fmt.Errorf("%w: body exceeds %d bytes", ErrInvalid, MaxBodyBytes)
	}
	return nil
}

// validateRefName checks a full ref name (refs/… shape; the git layer's
// ValidateRefName grammar reduced to the wire check pulls needs).
func validateRefName(ref string) error {
	if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") ||
		strings.Contains(ref, "//") || strings.HasSuffix(ref, "/") ||
		strings.HasSuffix(ref, ".lock") || strings.ContainsAny(ref, " \n\r~^:?*[`@{") {
		return fmt.Errorf("%w: invalid ref %q", ErrInvalid, ref)
	}
	return nil
}

// validateSHA checks a full 40/64-hex sha.
func validateSHA(sha string) error {
	if len(sha) != 40 && len(sha) != 64 {
		return fmt.Errorf("%w: sha must be 40/64-hex, got %q", ErrInvalid, sha)
	}
	for i := 0; i < len(sha); i++ {
		c := sha[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%w: sha must be lowercase hex, got %q", ErrInvalid, sha)
		}
	}
	return nil
}

// validateStrategy checks the merge strategy (exact §5 set).
func validateStrategy(s string) error {
	switch s {
	case StrategyMerge, StrategySquash, StrategyRebase:
		return nil
	}
	return fmt.Errorf("%w: strategy must be merge|squash|rebase, got %q", ErrInvalid, s)
}

// --- codec -----------------------------------------------------------------

func parseThread(raw []byte) (*Thread, error) {
	var t Thread
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("%w: thread.json: %v", ErrCorrupt, err)
	}
	if t.Labels == nil {
		t.Labels = []string{}
	}
	if t.Assignees == nil {
		t.Assignees = []string{}
	}
	if t.Participants == nil {
		t.Participants = []string{}
	}
	return &t, nil
}

func encodeThread(t *Thread) []byte {
	if t.Labels == nil {
		t.Labels = []string{}
	}
	if t.Assignees == nil {
		t.Assignees = []string{}
	}
	if t.Participants == nil {
		t.Participants = []string{}
	}
	raw, _ := json.Marshal(t)
	return raw
}

func parseEvent(raw []byte) (*Event, error) {
	var e Event
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("%w: event: %v", ErrCorrupt, err)
	}
	return &e, nil
}

func encodeEvent(e *Event) []byte {
	raw, _ := json.Marshal(e)
	return raw
}

// parsePR decodes a pr.json sidecar.
func parsePR(raw []byte) (*PRDoc, error) {
	var p PRDoc
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("%w: pr.json: %v", ErrCorrupt, err)
	}
	return &p, nil
}

func encodePR(p *PRDoc) []byte {
	raw, _ := json.Marshal(p)
	return raw
}

// parseMergeable decodes a mergeable.json cache (nil-tolerant: conflicts
// normalize to []).
func parseMergeable(raw []byte) (*MergeableDoc, error) {
	var m MergeableDoc
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: mergeable.json: %v", ErrCorrupt, err)
	}
	if m.Conflicts == nil {
		m.Conflicts = []string{}
	}
	return &m, nil
}

func encodeMergeable(m *MergeableDoc) []byte {
	if m.Conflicts == nil {
		m.Conflicts = []string{}
	}
	raw, _ := json.Marshal(m)
	return raw
}

// parseIndex decodes the shared list index.
func parseIndex(raw []byte) (*Index, error) {
	var ix Index
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&ix); err != nil {
		return nil, fmt.Errorf("%w: index.json: %v", ErrCorrupt, err)
	}
	if ix.Open == nil {
		ix.Open = []Card{}
	}
	if ix.ClosedRecent == nil {
		ix.ClosedRecent = []Card{}
	}
	for i := range ix.Open {
		ix.Open[i].Labels = nonNilStr(ix.Open[i].Labels)
		ix.Open[i].Assignees = nonNilStr(ix.Open[i].Assignees)
	}
	for i := range ix.ClosedRecent {
		ix.ClosedRecent[i].Labels = nonNilStr(ix.ClosedRecent[i].Labels)
		ix.ClosedRecent[i].Assignees = nonNilStr(ix.ClosedRecent[i].Assignees)
	}
	return &ix, nil
}

// parseForks decodes meta/forks.json.
func parseForks(raw []byte) (*ForksIndex, error) {
	var f ForksIndex
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%w: forks.json: %v", ErrCorrupt, err)
	}
	if f.Forks == nil {
		f.Forks = []ForkEntry{}
	}
	return &f, nil
}

// --- small helpers ----------------------------------------------------------

func nonNilStr(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// uniqSorted sorts and dedups name lists (stored sorted, unique).
func uniqSorted(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	cp := append([]string(nil), in...)
	sort.Strings(cp)
	out := cp[:1]
	for _, v := range cp[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

func strPtr(s string) *string { return &s }
