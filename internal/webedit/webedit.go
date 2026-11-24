// Package webedit implements Feature F of docs/PLAN-V2.md: the browser file
// editor. A signed-in principal with push rights can create, edit or delete
// one text file from the UI; the change lands as ONE commit authored by that
// principal and published through the same WAL funnel a push uses (pack →
// Layer.Ingest → RefPublisher.UpdateRefWithPack), so policy, events, Threads
// CI, checks and the events bridge observe it exactly like a pushed commit.
//
// There is NO data model (F.2): the commit is git state, the optional branch
// is a ref, and the optional PR is the shipped pulls state. Any bucket key
// this package touches is a bug — the one store read here is the repo's
// existing policy.json, which every server-side ref writer already reads.
//
// Commit construction is plumbing only, in the serving copy, with a
// request-scoped temporary index (no worktree, no branch movement):
//
//	git hash-object -w --stdin
//	git read-tree <base>
//	git update-index --add --cacheinfo <mode>,<blob>,<path>   (create/edit)
//	git update-index --force-remove -- <path>                 (delete)
//	git write-tree
//	git diff-tree --quiet <base> <tree>                       (no-op guard)
//	git commit-tree <tree> -p <base> -m <message>
//
// The exact argv are pinned in docs/go/04_git.md §15 (law L2).
//
// ### Concurrency
//
// Hazard: two editors racing one branch. Avoidance: this package holds no
// locks and spawns no goroutines beyond the bounded git pool. The publish
// CAS arbitrates — the second writer's old-oid check fails and maps to 409
// naming both shas (F.4). The temporary index is unique per request
// (pid+nanos, cleaned on every exit path), so concurrent edits never share
// scratch state. Git spawns run sequentially inside the request (dependent
// steps), each pool-bounded and request-context cancellable.
package webedit

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	walgit "github.com/crueber/spindle/internal/git"
)

// Constants — the bounds F.3/F.4 name.
const (
	// DefaultMaxBytes is webedit.max_bytes' compiled default (1 MiB): the
	// largest file the editor accepts. Binary upload is out of scope; this
	// keeps a commit's pack O(KiB) above the cap, trivially inside
	// server.max_push_bytes.
	DefaultMaxBytes = 1 << 20
	// MaxMessageBytes matches the tag-message cap (F.3).
	MaxMessageBytes = 64 * 1024
	// MaxPathBytes is the path length bound (F.3).
	MaxPathBytes = 512
	// binarySniffBytes is the NUL-sniff window (F.4: the blob page's rule).
	binarySniffBytes = 8000
	// lfsPointerPrefix identifies an LFS pointer blob (F.4).
	lfsPointerPrefix = "version https://git-lfs.github.com/spec/v1"
	// committerName/Email are the server identity for the committer field
	// (author = the principal; the merge convention).
	committerName  = "spindle"
	committerEmail = "spindle@localhost"
)

// Sentinel error family (errors.Is-compatible; http.go maps each to a status).
var (
	ErrInvalid      = errors.New("invalid request")
	ErrNotFound     = errors.New("not found")
	ErrUnauthorized = errors.New("authentication required")
	ErrForbidden    = errors.New("forbidden")
	ErrConflict     = errors.New("conflict")
	ErrTooLarge     = errors.New("file too large")
	ErrUnsupported  = errors.New("unsupported file")
	ErrNoChange     = errors.New("nothing to commit")
	ErrUnavailable  = errors.New("temporarily unavailable")
)

// ConflictError is the 409 shape of F.4: the expected sha vs what disk holds.
// The client is never allowed a blind overwrite; the page re-renders the
// editor from the new base.
type ConflictError struct {
	// What is "branch" or "blob".
	What     string
	Expected string
	Actual   string
	// Why carries a one-line human reason (e.g. "file exists").
	Why string
}

func (e *ConflictError) Error() string {
	if e.What == "blob" && e.Why != "" {
		return fmt.Sprintf("%s (disk: %s)", e.Why, e.Actual)
	}
	what := e.What
	if what == "" {
		what = "branch"
	}
	return fmt.Sprintf("%s moved under you (page: %s, disk: %s)", what, e.Expected, e.Actual)
}

// Is lets errors.Is(err, ErrConflict) match a ConflictError.
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// isErr is the sentinel-family matcher (errors.Is plus the wrapped text, the
// tags precedent: a service double may wrap by message).
func isErr(err error, target error) bool {
	return err != nil && (errors.Is(err, target) || strings.Contains(err.Error(), target.Error()))
}

// Mode is the write shape.
type Mode string

const (
	ModeCreate Mode = "create"
	ModeEdit   Mode = "edit"
	ModeDelete Mode = "delete"
)

// Input is the one write request shape (API body + page form both land here).
type Input struct {
	Mode Mode
	// Ref is the branch the edit applies to (short or full name).
	Ref  string
	Path string
	// Message is the commit subject; Description the optional body.
	Message     string
	Description string
	// Content is the exact bytes to write (create/edit). The JSON API
	// carries base64; the page form normalizes CRLF→LF (git convention).
	Content []byte
	// ExpectedBlobSHA is required when the path exists (edit/delete) and
	// forbidden on create.
	ExpectedBlobSHA string
	// ExpectedBranchSHA is the branch tip the editor rendered from.
	ExpectedBranchSHA string
	// NewBranch, when non-empty, publishes to a new branch created from
	// Ref instead of moving Ref.
	NewBranch string
	// OpenPR opens a PR (Ref → NewBranch) right after the branch lands.
	OpenPR  bool
	PRTitle string
	PRBody  string
}

// Result is the write response: the commit, the branch it landed on, and the
// optional PR number.
type Result struct {
	Commit string
	Branch string
	PRNum  int
}

// File is one blob read from a branch (the page's load side).
type File struct {
	Path    string
	SHA     string
	Mode    string // git mode ("100644", "100755", …)
	Size    int64
	Content []byte
	Binary  bool
	LFS     bool
}

// DecodeContent decodes a base64 body (the JSON API spelling) with a size
// bound applied by the caller.
func DecodeContent(s string) ([]byte, error) {
	// Accept both padded and unpadded base64 (the SDK's spelling is padded).
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
		if err != nil {
			return nil, fmt.Errorf("%w: content is not base64", ErrInvalid)
		}
	}
	return b, nil
}

// ValidatePath enforces the git-safe subset F.3 names: UTF-8, ≤ 512 bytes, no
// leading/trailing slash, no empty, "." or ".." segment, no ".git" segment,
// no NUL/control bytes and no backslash. A leading "-" is refused explicitly
// so no value can ever be parsed as a git option.
func ValidatePath(p string) error {
	if p == "" {
		return fmt.Errorf("%w: path must not be empty", ErrInvalid)
	}
	if len(p) > MaxPathBytes {
		return fmt.Errorf("%w: path exceeds %d bytes", ErrInvalid, MaxPathBytes)
	}
	if !utf8.ValidString(p) {
		return fmt.Errorf("%w: path is not valid UTF-8", ErrInvalid)
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "-") || strings.HasSuffix(p, "/") {
		return fmt.Errorf("%w: path must be relative and must not start with '-'", ErrInvalid)
	}
	if strings.Contains(p, "\\") {
		return fmt.Errorf("%w: path must use '/'", ErrInvalid)
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c < 0x20 || c == 0x7f {
			return fmt.Errorf("%w: path contains a control byte", ErrInvalid)
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" {
			return fmt.Errorf("%w: path contains an empty segment", ErrInvalid)
		}
		if seg == "." || seg == ".." {
			return fmt.Errorf("%w: path contains a relative segment", ErrInvalid)
		}
		if strings.EqualFold(seg, ".git") {
			return fmt.Errorf("%w: path contains a .git segment", ErrInvalid)
		}
	}
	return nil
}

// ValidateMessage trims and bounds the commit message (F.3: ≤ 64 KiB after
// trimming to non-empty).
func ValidateMessage(msg, desc string) (string, string, error) {
	msg = strings.TrimSpace(msg)
	desc = strings.TrimSpace(desc)
	if msg == "" {
		return "", "", fmt.Errorf("%w: commit message must not be empty", ErrInvalid)
	}
	total := len(msg) + len(desc) + 2
	if total > MaxMessageBytes {
		return "", "", fmt.Errorf("%w: commit message exceeds %d bytes", ErrInvalid, MaxMessageBytes)
	}
	return msg, desc, nil
}

// ValidateBranch validates a branch name as `refs/heads/<name>` grade
// (git.ValidateRefName) and refuses anything that could read as an option.
func ValidateBranch(name string) (string, error) {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "refs/heads/")
	if name == "" || name == "HEAD" {
		return "", fmt.Errorf("%w: branch must not be empty", ErrInvalid)
	}
	if strings.HasPrefix(name, "-") {
		return "", fmt.Errorf("%w: branch must not start with '-'", ErrInvalid)
	}
	if err := walgit.ValidateRefName("refs/heads/" + name); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return name, nil
}

// normalizeBranchRef renders the full ref for a validated short branch name.
func normalizeBranchRef(ref string) (string, string, error) {
	name, err := ValidateBranch(ref)
	if err != nil {
		return "", "", err
	}
	return "refs/heads/" + name, name, nil
}

// commitMessage joins subject and description (one trailing newline is added
// by commit-tree itself; the message is stored verbatim otherwise).
func commitMessage(msg, desc string) string {
	if desc == "" {
		return msg
	}
	return msg + "\n\n" + desc
}

// isBinary sniffs a NUL byte in the first 8 000 bytes (F.4, the blob page's
// rule).
func isBinary(b []byte) bool {
	if len(b) > binarySniffBytes {
		b = b[:binarySniffBytes]
	}
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}

// isLFSPointer reports whether the blob is a pointer body (F.4).
func isLFSPointer(b []byte) bool {
	return strings.HasPrefix(string(b), lfsPointerPrefix)
}

// IsBinary exposes the F.4 NUL sniff to page handlers (the same rule the API
// guard applies).
func IsBinary(b []byte) bool { return isBinary(b) }

// IsLFSPointer exposes the F.4 pointer sniff to page handlers.
func IsLFSPointer(b []byte) bool { return isLFSPointer(b) }

// now renders the service clock (pinnable in tests).
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
