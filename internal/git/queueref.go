package git

import "strings"

// MergeQueueRefPrefix is the documented merge-queue ref namespace (PAR-016,
// docs/threads.md §6.10):
//
//	refs/heads/gh-readonly-queue/<base-branch>/pr-<number>-<sha>
//
// The merge queue creates these refs transiently and deletes them when an
// entry lands, is ejected or is cancelled. They live under refs/heads/ (the
// documented shape), but they are NOT branches: a plain clone must not
// receive them as remote-tracking branches, the branch list must not show
// them, and the branch count must not include them. An explicit fetch of the
// full ref name still works (a client that reads the queue ref on purpose is
// the one consumer the contract names).
const MergeQueueRefPrefix = "refs/heads/gh-readonly-queue/"

// IsMergeQueueRef reports whether name is a merge-queue entry ref. It is a
// prefix test, deliberately: even a malformed queue ref stays out of the
// branch surfaces (the push hook names the malformed one, never turns it into
// a branch build).
func IsMergeQueueRef(name string) bool {
	return strings.HasPrefix(strings.TrimSpace(name), MergeQueueRefPrefix)
}
