package threads

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The merge_group trigger's ref contract (PAR-016).
//
// A merge queue hands its work to CI through a read-only git ref, documented by
// GitHub as
//
//	refs/heads/gh-readonly-queue/<base-branch>/pr-<number>-<sha>
//
// The push hook recognises exactly that shape and starts a `merge_group` run
// instead of a `push` run. The queue itself — deciding what to enqueue,
// batching entries, pushing these refs and landing the merge when the checks
// are green — is the separate Merge queue backlog card. This file is the
// boundary between the two: the queue pushes the ref, the trigger creates the
// run, and the run's mirrored checks are what the required-checks merge gate
// reads (§7 of docs/threads.md).
//
// Validation is strict and named: a ref that merely LOOKS like a queue ref but
// does not match the documented shape creates nothing at all (not a push run
// either), because a malformed queue entry must never silently become an
// ordinary branch build.

// MergeQueueRefPrefix is the ref-name prefix of a merge-queue entry.
const MergeQueueRefPrefix = "refs/heads/gh-readonly-queue/"

// mergeQueueRefRe is the documented shape. The base may contain slashes (a
// branch such as `release/1.0`), so it is matched greedily up to the final
// `/pr-`; the PR number has no leading zero; the sha is a lowercase full git
// object id (40 hex, or 64 for a sha256 repository).
var mergeQueueRefRe = regexp.MustCompile(`^refs/heads/gh-readonly-queue/(.+)/pr-([1-9][0-9]{0,8})-([0-9a-f]{40}|[0-9a-f]{64})$`)

// MergeQueueRef is one parsed merge-queue ref.
type MergeQueueRef struct {
	// Ref is the full ref name as pushed.
	Ref string
	// Name is the short ref name (github.ref_name for the run).
	Name string
	// BaseBranch is the branch the queue targets, as a plain name
	// ("main", "release/1.0").
	BaseBranch string
	// BaseRef is the full target ref ("refs/heads/<base>").
	BaseRef string
	// Number is the pull request number encoded in the ref.
	Number int
	// HeadSHA is the sha named in the ref name (the pull request head GitHub
	// encodes there). The run's github.sha is the ref TIP, the merge-group
	// commit, not this value.
	HeadSHA string
}

// ParseMergeQueueRef validates ref against the documented merge-queue shape.
// ok=false means the ref is not a merge-queue entry AT ALL; a caller that sees
// the prefix but gets ok=false must create nothing and say why.
func ParseMergeQueueRef(ref string) (MergeQueueRef, bool) {
	ref = strings.TrimSpace(ref)
	m := mergeQueueRefRe.FindStringSubmatch(ref)
	if m == nil {
		return MergeQueueRef{}, false
	}
	base, number, sha := m[1], m[2], m[3]
	if !validMergeQueueBase(base) {
		return MergeQueueRef{}, false
	}
	n, err := strconv.Atoi(number)
	if err != nil || n <= 0 {
		return MergeQueueRef{}, false
	}
	name := strings.TrimPrefix(ref, "refs/heads/")
	return MergeQueueRef{
		Ref: ref, Name: name, BaseBranch: base,
		BaseRef: "refs/heads/" + base, Number: n, HeadSHA: sha,
	}, true
}

// IsMergeQueueRef reports whether ref claims to be a merge-queue ref (even a
// malformed one). The push hook uses it to refuse a malformed queue ref rather
// than letting it fall through to the ordinary branch path.
func IsMergeQueueRef(ref string) bool {
	return strings.HasPrefix(strings.TrimSpace(ref), MergeQueueRefPrefix)
}

// validMergeQueueBase applies git's ref-name rules to the base branch segment.
// The point is not to reimplement `git check-ref-format` in full; it is to
// reject a shape that cannot name a branch (`..`, a control character, a
// `@{`, a `.lock` suffix, an empty or leading-dot component), so the ref's
// parsed base is always a name the git reader can resolve.
func validMergeQueueBase(base string) bool {
	if base == "" || len(base) > 255 {
		return false
	}
	if base == "@" || strings.HasPrefix(base, "/") || strings.HasSuffix(base, "/") ||
		strings.Contains(base, "//") || strings.Contains(base, "..") ||
		strings.Contains(base, "@{") || strings.HasSuffix(base, ".lock") {
		return false
	}
	for _, r := range base {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(` ~^:?*[\`, r) {
			return false
		}
	}
	for _, part := range strings.Split(base, "/") {
		if part == "" || strings.HasPrefix(part, ".") || part == "@" {
			return false
		}
	}
	return true
}

// MergeGroupEventPayload builds the documented `merge_group` payload. head_sha
// is the run's commit (the queue ref's tip, which the checks run against) and
// base_sha the target branch tip the group would merge into. `number` is the
// pull request the queue entry names — the ref carries it, and a workflow
// reading the entry should not have to parse the ref to find it.
func MergeGroupEventPayload(repo, ref, headSHA, baseRef, baseSHA string, number int, actor string) map[string]any {
	return map[string]any{
		"action": "checks_requested",
		"number": number,
		"merge_group": map[string]any{
			"head_ref": ref,
			"head_sha": headSHA,
			"base_ref": baseRef,
			"base_sha": baseSHA,
		},
		"repository": repositoryProjection(repo),
		"sender":     map[string]any{"login": actor},
	}
}

// fillMergeGroupPayload overwrites the payload's merge_group projection with
// the ref-derived facts CreateRun resolved, so a hand-built CreateOpts cannot
// claim a different base or head than the ref names. It also fills a missing
// payload entirely (the push hook always supplies one; a direct caller need
// not).
func fillMergeGroupPayload(payload map[string]any, repo string, mq MergeQueueRef, headSHA, baseSHA, actor string) map[string]any {
	if payload == nil {
		payload = MergeGroupEventPayload(repo, mq.Ref, headSHA, mq.BaseRef, baseSHA, mq.Number, actor)
	}
	payload["action"] = "checks_requested"
	payload["number"] = mq.Number
	group, _ := payload["merge_group"].(map[string]any)
	if group == nil {
		group = map[string]any{}
		payload["merge_group"] = group
	}
	group["head_ref"] = mq.Ref
	group["head_sha"] = headSHA
	group["base_ref"] = mq.BaseRef
	group["base_sha"] = baseSHA
	return payload
}

// MergeGroupRefusal is the named reason a merge-queue-shaped ref was refused.
// It is logged by the push hook and returned by CreateRun, so a malformed ref
// is diagnosed rather than silently dropped.
func MergeGroupRefusal(ref string) error {
	return fmt.Errorf("%w: merge queue ref %q is not the documented shape refs/heads/gh-readonly-queue/<base>/pr-<number>-<sha>", ErrInvalid, ref)
}
