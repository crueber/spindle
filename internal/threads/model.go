package threads

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Sentinel errors (the 07 §2 plain-text/status mapping, same class set the
// sibling feature packages use).
var (
	// ErrNotFound marks an unknown run/job/workflow (→ 404).
	ErrNotFound = errors.New("unknown thread")
	// ErrInvalid marks a bad request/record field (→ 400).
	ErrInvalid = errors.New("invalid threads request")
	// ErrUnauthorized marks anonymous-denied access (→ 401).
	ErrUnauthorized = errors.New("authentication required")
	// ErrForbidden marks authenticated-but-insufficient access (→ 403).
	ErrForbidden = errors.New("forbidden")
	// ErrConflict marks a CAS conflict or a duplicate record (→ 409).
	ErrConflict = errors.New("conflict")
	// ErrUnprocessable marks a well-formed but unusable request (a sha that
	// is not a commit, a workflow file that does not parse; → 422).
	ErrUnprocessable = errors.New("unprocessable")
	// ErrUnavailable marks a down dependency (git, store; → 503).
	ErrUnavailable = errors.New("temporarily unavailable")
	// ErrCorrupt marks an unreadable bucket object (→ 500).
	ErrCorrupt = errors.New("corrupt object")
)

// Events v1 fires on (15 §1). Schedule is deliberately absent: cron evaluation
// belongs to the maintainer role (T4), so Discover never fabricates a run for a
// cron entry.
const (
	EventPush       = "push"
	EventPullReq    = "pull_request"
	EventDispatch   = "workflow_dispatch"
	EventSchedule   = "schedule" // accepted by workflow.Fires as "never fires"
	defaultRunActor = "spindle"
)

// Run states. T1 creates every run in RunQueued; UpdateJob moves it (T2 will
// claim queue objects, T3 executes them).
const (
	RunQueued    = "queued"
	RunRunning   = "running"
	RunSuccess   = "success"
	RunFailure   = "failure"
	RunCancelled = "cancelled"
)

// Job states (the same vocabulary as a run; a job is the unit the checks
// surface mirrors).
const (
	JobQueued    = RunQueued
	JobRunning   = RunRunning
	JobSuccess   = RunSuccess
	JobFailure   = RunFailure
	JobCancelled = RunCancelled
)

// KindThreadsGC is the Seam 5 task kind reserved for run/log retention
// (15 §2). T1 registers the name; the body is the T3 retention pass (see
// Service.GC, the documented stub — logs and their frames do not exist yet).
const KindThreadsGC = "threads-gc"

// RunIDLayout is the lexical run-id shape: 19-digit unix-nanos, a dash, and 6
// lowercase hex chars. Lexical order == creation order, which is what the
// cursor-paged run list relies on.
const runIDLayout = "%019d-%s"

// runIDRe pins the run-id shape (it is a bucket key segment and a URL path
// segment).
var runIDRe = regexp.MustCompile(`^[0-9]{19}-[a-z0-9]{6}$`)

// jobIDRe pins the job-id shape.
var jobIDRe = regexp.MustCompile(`^j-[a-z0-9][a-z0-9._-]*$`)

// shaRe pins full commit shas (40/64 lowercase hex; input is lowercased).
var shaRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// slugInvalid collapses everything outside the checks-context charset to a
// dash.
var slugInvalid = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Run is repos/<o>/<r>/threads/runs/<run_id>.json (15 §2): the run header. It
// is Create-only in T1 (state transitions CAS the same object) and it is the
// run's commit point — the job records are written first, so an invisible
// orphan job is harmless and swept by threads-gc.
type Run struct {
	ID   string `json:"id"`
	Repo string `json:"repo"`
	// Commit is the sha the run reports checks for: the pushed head for a
	// same-repository push/PR, the head commit for a manual dispatch.
	Commit string `json:"commit"`
	Event  string `json:"event"`
	// Branch/Ref describe the triggering ref (no refs/… prefix problem for
	// Branch: it is the human branch name).
	Branch string `json:"branch"`
	Ref    string `json:"ref"`
	// Workflow is the display name (the YAML `name`, falling back to the
	// file basename); WorkflowPath is the repository-relative file path.
	Workflow string `json:"workflow"`
	// WorkflowPath is the file the run was created from, and WorkflowCommit
	// is the commit that file was read at. They differ for a fork pull
	// request: the run builds Commit but reads the file at the BASE branch's
	// tip (trust class, 15 §2/§5).
	WorkflowPath   string `json:"workflow_path"`
	WorkflowCommit string `json:"workflow_commit"`
	WorkflowDigest string `json:"workflow_digest"` // sha256 of the file bytes
	// Trusted is the trust class: true for same-repository pushes/PRs and
	// manual dispatches, false for fork PRs (no secrets later, read-only
	// token — T3 enforces; T1 records the decision).
	Trusted bool   `json:"trusted"`
	State   string `json:"state"`
	Actor   string `json:"actor"`
	Attempt int    `json:"attempt"`

	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	StartedAt   *string `json:"started_at,omitempty"`
	CompletedAt *string `json:"completed_at,omitempty"`
	Version     int     `json:"version"`
}

// Step is one workflow step copied into the job record (15 §2: the job record
// carries the steps so a runner never needs the workflow file to start).
type Step struct {
	Name string `json:"name,omitempty"`
	Run  string `json:"run,omitempty"`
	Uses string `json:"uses,omitempty"`
}

// Job is repos/<o>/<r>/threads/runs/<run_id>/jobs/<job_id>.json: one workflow
// job, attempt-scoped (T2 requeues by bumping Attempt).
type Job struct {
	ID    string `json:"id"`
	RunID string `json:"run_id"`
	Repo  string `json:"repo"`
	// Key is the YAML map key; Name is the display name (key when unnamed).
	Key  string `json:"key"`
	Name string `json:"name"`
	// Context is the checks context this job mirrors
	// (threads/<workflow>/<job>, lowercased and slugged to the checks
	// charset — see CheckContext).
	Context   string   `json:"context"`
	RunsOn    string   `json:"runs_on"`
	Container string   `json:"container,omitempty"`
	Needs     []string `json:"needs"`
	Steps     []Step   `json:"steps"`
	Attempt   int      `json:"attempt"`
	Trusted   bool     `json:"trusted"`
	State     string   `json:"state"`

	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	StartedAt   *string `json:"started_at,omitempty"`
	CompletedAt *string `json:"completed_at,omitempty"`
	Version     int     `json:"version"`
}

// RunView is GET …/api/threads/{run}: the header plus its jobs (name-sorted).
type RunView struct {
	Run  *Run   `json:"run"`
	Jobs []*Job `json:"jobs"`
}

// RunPage is GET …/api/threads: newest-first, cursor-paged by run id.
type RunPage struct {
	Runs []*Run `json:"runs"`
	More bool   `json:"more"`
}

// CreateOpts carries the event context into CreateRun. Server-side policy owns
// Trusted; there is no client-set trust field.
type CreateOpts struct {
	// Branch is the pushed branch (push) or the PR target branch
	// (pull_request) as a plain name ("main"). It selects push/PR trigger
	// filters.
	Branch string
	// Ref is the triggering ref ("refs/heads/main"); optional.
	Ref string
	// BaseBranch/BaseCommit identify the trusted revision for a fork PR
	// (the base branch tip). BaseCommit is preferred when set; otherwise
	// the service resolves refs/heads/<BaseBranch>.
	BaseBranch string
	BaseCommit string
	// Fork marks a pull_request from a forked repository: base-branch
	// workflow revision, Trusted=false (15 §2).
	Fork bool
	// Actor is the principal that triggered the run ("spindle" for the
	// server-side push hook).
	Actor string
	// Workflow restricts a workflow_dispatch to one file (basename such as
	// "ci.yml" or the repository-relative path). Empty = every file whose
	// workflow_dispatch trigger is present.
	Workflow string
	// Attempt starts at 1 (a requeue bumps it; T2 owns requeueing).
	Attempt int
}

// --- state helpers ---------------------------------------------------------

// normalizeSHA lowercases and validates a full commit sha.
func normalizeSHA(sha string) (string, error) {
	lower := strings.ToLower(strings.TrimSpace(sha))
	if !shaRe.MatchString(lower) {
		return "", fmt.Errorf("%w: sha must be a full 40/64-hex commit sha, got %q", ErrInvalid, sha)
	}
	return lower, nil
}

// validEvent reports the events CreateRun accepts.
func validEvent(event string) bool {
	switch event {
	case EventPush, EventPullReq, EventDispatch:
		return true
	}
	return false
}

// validRunState reports the run state enum.
func validRunState(state string) bool {
	switch state {
	case RunQueued, RunRunning, RunSuccess, RunFailure, RunCancelled:
		return true
	}
	return false
}

// validJobState reports the job state enum.
func validJobState(state string) bool {
	return validRunState(state)
}

// terminal reports a state no further transition leaves (T1's run fold).
func terminal(state string) bool {
	return state == RunSuccess || state == RunFailure || state == RunCancelled
}

// checkState maps a job state onto the checks status enum
// (05 §2: pending|success|failure|error). queued/running are "pending" — a job
// that has not concluded is in flight, which is exactly what the merge gate
// must treat as not-green; a cancelled job maps to failure (a cancelled
// required check must never unblock a merge).
func checkState(jobState string) string {
	switch jobState {
	case JobSuccess:
		return "success"
	case JobFailure, JobCancelled:
		return "failure"
	default:
		return "pending"
	}
}

// foldRunState computes a run's state from its jobs: any live job keeps the run
// running (or queued when nothing started), all terminal means worst-of.
func foldRunState(jobs []*Job) string {
	if len(jobs) == 0 {
		return RunQueued
	}
	allQueued, anyFailed, allTerminal := true, false, true
	for _, j := range jobs {
		if j.State != JobQueued {
			allQueued = false
		}
		if j.State == JobFailure || j.State == JobCancelled {
			anyFailed = true
		}
		if !terminal(j.State) {
			allTerminal = false
		}
	}
	switch {
	case allQueued:
		return RunQueued
	case !allTerminal:
		return RunRunning
	case anyFailed:
		return RunFailure
	default:
		return RunSuccess
	}
}

// CheckContext renders the checks context a job mirrors:
//
//	threads/<workflow>/<job>
//
// The checks grammar (policy.ValidCheckContext: [A-Za-z0-9._/-], 1–100 chars)
// admits no spaces, so the prose "threads / <workflow> / <job>" is written with
// slashes and both segments are lowercased and slugged (invalid runs → "-",
// each capped at 40 chars). Pinned by TestCheckContextNaming so a required
// check configured in policy.json cannot surprise an operator.
func CheckContext(workflowName, jobName string) string {
	return "threads/" + slugSegment(workflowName) + "/" + slugSegment(jobName)
}

// slugSegment lowercases and maps a free-form workflow/job label onto the
// checks context charset, capped at 40 chars.
func slugSegment(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugInvalid.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-._")
	if s == "" {
		return "unnamed"
	}
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-._")
		if s == "" {
			return "unnamed"
		}
	}
	return s
}

// jobIDFor builds a stable, key-safe job id from the YAML job key ("j-" +
// slug). Collisions (two keys slugging identically) get a numeric suffix;
// callers pass the already-used set.
func jobIDFor(key string, used map[string]bool) string {
	base := "j-" + slugSegment(key)
	id := base
	for n := 2; used[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	used[id] = true
	return id
}

// newRunID mints a time-ordered run id. The random suffix only separates runs
// created in the same nanosecond.
func newRunID(now time.Time) (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf(runIDLayout, now.UTC().UnixNano(), hex.EncodeToString(b[:])), nil
}

// nonNilStr normalizes a nil slice to [] (wire rule: [] never null).
func nonNilStr(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// nonNilSteps normalizes a nil slice to [].
func nonNilSteps(in []Step) []Step {
	if in == nil {
		return []Step{}
	}
	return in
}
