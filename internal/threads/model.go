package threads

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/threads/workflow"
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
	// ErrTooLarge marks a request over a configured size cap (artifact
	// uploads/downloads; → 413), never a generic 400: the named message says
	// which cap and which config key.
	ErrTooLarge = errors.New("too large")
	// ErrRateLimited marks an over-budget request (the OIDC exchange's
	// per-job window; → 429 with Retry-After).
	ErrRateLimited = errors.New("rate limited")
	// ErrUnavailable marks a down dependency (git, store; → 503).
	ErrUnavailable = errors.New("temporarily unavailable")
	// ErrCorrupt marks an unreadable bucket object (→ 500).
	ErrCorrupt = errors.New("corrupt object")
)

// Events v1 fires on (15 §1). `schedule` is created only by the maintainer's
// cron evaluation (SweepSchedule): a push/dispatch path never fabricates one.
const (
	EventPush       = "push"
	EventPullReq    = "pull_request"
	EventDispatch   = "workflow_dispatch"
	EventSchedule   = "schedule" // created by SweepSchedule, never by Discover
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
	// RunSkipped is a job (or a run folded from jobs) that `needs:` made
	// impossible: a dependency failed or was cancelled, so this job never ran
	// (round 3 — GitHub reads such dependents as `skipped`, not `cancelled`).
	// It is terminal and never green, but it is distinct from an operator's
	// cancel: a genuinely cancelled run still reads `cancelled`.
	RunSkipped = "skipped"
	// RunPendingApproval is the first-time-contributor gate (15 §5.2): the run
	// exists with its jobs written but NOT enqueued until a maintainer approves.
	RunPendingApproval = "pending_approval"
)

// Job states (the same vocabulary as a run; a job is the unit the checks
// surface mirrors).
const (
	JobQueued    = RunQueued
	JobRunning   = RunRunning
	JobSuccess   = RunSuccess
	JobFailure   = RunFailure
	JobCancelled = RunCancelled
	JobSkipped   = RunSkipped
)

// KindThreadsGC is the Seam 5 task kind reserved for run/log retention
// (15 §2). T1 registers the name; the body is the T3 retention pass (see
// Service.GC, the documented stub — logs and their frames do not exist yet).
const KindThreadsGC = "threads-gc"

// KindThreadsQueueReconcile is the Seam 5 task kind for the global queue-index
// repair (docs/PLAN-INSTANCE-RUNNERS.md §3). It is the only place a global
// index rebuild LISTs, and it runs per repository on the maintainer.
const KindThreadsQueueReconcile = "threads-queue-reconcile"

// RunIDLayout is the lexical run-id shape: 19-digit unix-nanos, a dash, and 6
// lowercase hex chars. Lexical order == creation order, which is what the
// cursor-paged run list relies on.
const runIDLayout = "%019d-%s"

// runIDRe pins the run-id shape (it is a bucket key segment and a URL path
// segment).
var runIDRe = regexp.MustCompile(`^[0-9]{19}-[a-z0-9]{6}$`)

// jobIDRe pins the job-id shape.
var jobIDRe = regexp.MustCompile(`^j-[a-z0-9][a-z0-9._-]*$`)

// tokenIDRe pins a runner/credential id (the mintID shape).
var tokenIDRe = regexp.MustCompile(`^[a-z0-9]{8,24}$`)

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
	// Inputs are the workflow_dispatch input values this run was dispatched
	// with (nil for push/PR runs); the job specs carry the same map.
	Inputs map[string]string `json:"inputs,omitempty"`
	// Concurrency lists the distinct concurrency groups this run holds while
	// in flight (15 §4, pass B2). It is written at creation and read by the
	// claim path (one in-flight run per group) and by the terminal fold, which
	// releases exactly these keys.
	Concurrency []string `json:"concurrency,omitempty"`

	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	StartedAt   *string `json:"started_at,omitempty"`
	CompletedAt *string `json:"completed_at,omitempty"`
	Version     int     `json:"version"`

	// ApprovedBy/ApprovedAt and DeniedBy/DeniedAt record the decided hold's
	// decision: the first-time-contributor gate (15 §5.2) or an environment's
	// required reviewers (docs/PLAN-THREADS-ENVIRONMENTS.md). ApprovalKind
	// names which gate held it ("" = not held) and Environments the gated
	// environment names, so the page, the check description and the API name
	// the environment instead of a generic "pending approval".
	ApprovalKind string   `json:"approval_kind,omitempty"`
	Environments []string `json:"environments,omitempty"`
	ApprovedBy   string   `json:"approved_by,omitempty"`
	ApprovedAt   *string  `json:"approved_at,omitempty"`
	DeniedBy     string   `json:"denied_by,omitempty"`
	DeniedAt     *string  `json:"denied_at,omitempty"`

	// CancelReason names why the run left the live states, and CancelledBy who
	// (or what) did it: CancelReasonOperator for a maintainer's cancel,
	// CancelReasonSupersede for a newer push / PR synchronize (SupersededBy
	// names that run, PAR-150), CancelReasonConcurrency for a group's
	// `cancel-in-progress`. Both are empty on a run that ended on its own, and
	// the FIRST cancel wins: a later cancel of an already-terminal run never
	// rewrites them (cancel is idempotent).
	CancelReason string `json:"cancel_reason,omitempty"`
	CancelledBy  string `json:"cancelled_by,omitempty"`
	SupersededBy string `json:"superseded_by,omitempty"`

	// DurationSeconds is a DERIVED read-model field (never stored, never CAS'd):
	// the wall time from StartedAt to CompletedAt, or to now while the run is
	// live. The run page and the API both read it.
	DurationSeconds int64 `json:"duration_seconds,omitempty"`
}

// Step is one workflow step copied into the job record for display and for the
// checks mirror's convenience fields (15 §2: a runner must never need the
// workflow file). The authoritative, complete step is Spec.Steps.
type Step struct {
	Name string `json:"name,omitempty"`
	Run  string `json:"run,omitempty"`
	Uses string `json:"uses,omitempty"`
	// Parent is the display name of the composite action step this step was
	// expanded from ("" for a top-level step). It carries no execution
	// meaning: Spec.Steps is the payload (docs/threads.md §3.4a).
	Parent string `json:"parent,omitempty"`
}

// SpecVersion is the current Job.Spec schema version. A runner refuses a spec
// it does not know; bump it when a field's meaning changes, never to add an
// optional field (additions stay compatible within a version).
const SpecVersion = 1

// Spec is everything a runner needs to start one job, captured in the record at
// creation time (15 §2: the runner never reads the workflow file). It is the
// authoritative execution payload; the Job convenience fields (RunsOn,
// Container, Steps) are a projection kept for the existing page/mirror code.
type Spec struct {
	Version int `json:"version"`
	// Workflow is the workflow's display name, carried so the checks mirror
	// and log labels derive from the job record alone.
	Workflow string `json:"workflow"`
	// RunsOn is the label set (one entry for the scalar spelling).
	RunsOn    []string               `json:"runs_on"`
	Container *ContainerSpec         `json:"container,omitempty"`
	Services  map[string]ServiceSpec `json:"services,omitempty"`
	// Env is the merged workflow+job environment (job wins).
	Env             map[string]string `json:"env,omitempty"`
	If              string            `json:"if,omitempty"`
	TimeoutMinutes  int               `json:"timeout_minutes,omitempty"`
	ContinueOnError bool              `json:"continue_on_error,omitempty"`
	// Permissions is carried for the future job token; NOT enforced in pass A.
	Permissions map[string]string `json:"permissions,omitempty"`
	// Strategy is the matrix product + fail-fast flag. The scheduler expands
	// it at run creation (pass B2): every expanded job carries one cell in
	// Matrix and the fail-fast flag rides the whole product.
	Strategy *StrategySpec `json:"strategy,omitempty"`
	// Matrix is one expanded cell (axis → value) for a matrix job; nil for a
	// plain job. The runner substitutes ${{ matrix.<axis> }} in step fields and
	// exposes the values through the `matrix` context of `if`.
	Matrix map[string]string `json:"matrix,omitempty"`
	// MatrixGroup is the YAML job key a matrix cell belongs to ("" for a plain
	// job). fail-fast cancels exactly the sibling cells of the same group.
	MatrixGroup string `json:"matrix_group,omitempty"`
	// Concurrency is the job's group (job-level wins over the workflow's,
	// pass A recording); the claim path enforces one in-flight run per group.
	Concurrency *ConcurrencySpec `json:"concurrency,omitempty"`
	// Uses/With name a reusable workflow instead of runnable steps. Accepted
	// and carried; execution is pass B/C.
	Uses string            `json:"uses,omitempty"`
	With map[string]string `json:"with,omitempty"`
	// Inputs are the resolved workflow_dispatch input values for this run
	// (declared defaults merged with what the dispatcher supplied). The runner
	// substitutes ${{ inputs.<name> }} and exposes them to `if` as
	// inputs.<name>; a non-dispatch run has none.
	Inputs map[string]string `json:"inputs,omitempty"`
	// Environment is the deployment environment the job targets (nil when the
	// job names none), from `jobs.<key>.environment:` — a name and an optional
	// URL. Environment secrets are resolved only for this name.
	Environment *EnvironmentSpec `json:"environment,omitempty"`
	// Outputs are the job's declared output templates (docs/threads.md §3.9),
	// evaluated by the runner once its steps finish. A template may read
	// `${{ steps.<id>.outputs.<name> }}`, `${{ matrix.<axis> }}` and
	// `${{ inputs.<name> }}`; the parser validates the references.
	Outputs map[string]string `json:"outputs,omitempty"`
	Steps   []StepSpec        `json:"steps"`
	// Actions are the repository-local action definitions this job's steps use
	// (directly or nested), resolved from the run commit and embedded so the
	// runner needs no repository access (docs/threads.md §3.4a). Keyed by
	// repository-relative directory.
	Actions map[string]ActionSpec `json:"actions,omitempty"`
}

// ContainerSpec is one job container (image plus the mapping fields).
type ContainerSpec struct {
	Image      string            `json:"image"`
	Env        map[string]string `json:"env,omitempty"`
	Ports      []string          `json:"ports,omitempty"`
	Command    []string          `json:"command,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Options    string            `json:"options,omitempty"`
	Volumes    []string          `json:"volumes,omitempty"`
}

// ServiceSpec is one sidecar container.
type ServiceSpec struct {
	Image      string            `json:"image"`
	Env        map[string]string `json:"env,omitempty"`
	Ports      []string          `json:"ports,omitempty"`
	Command    []string          `json:"command,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Options    string            `json:"options,omitempty"`
	Volumes    []string          `json:"volumes,omitempty"`
}

// StrategySpec is the job's matrix declaration and fail-fast flag. Matrix holds
// the axes; Include/Exclude carry GitHub's matrix lists so the server expands
// exactly the cells the parser accepted (docs/WORKFLOWS.md §5.2).
type StrategySpec struct {
	Matrix   map[string][]string `json:"matrix,omitempty"`
	Include  []map[string]string `json:"include,omitempty"`
	Exclude  []map[string]string `json:"exclude,omitempty"`
	FailFast *bool               `json:"fail_fast,omitempty"`
}

// ConcurrencySpec is one concurrency group.
type ConcurrencySpec struct {
	Group            string `json:"group"`
	CancelInProgress bool   `json:"cancel_in_progress,omitempty"`
}

// StepSpec is one complete workflow step.
type StepSpec struct {
	// ID is the step's `id:` (GitHub's `steps.<id>` key); empty for an
	// anonymous step. The runner records outcome/conclusion under it.
	ID               string            `json:"id,omitempty"`
	Name             string            `json:"name,omitempty"`
	Run              string            `json:"run,omitempty"`
	Uses             string            `json:"uses,omitempty"`
	With             map[string]string `json:"with,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	If               string            `json:"if,omitempty"`
	Shell            string            `json:"shell,omitempty"`
	WorkingDirectory string            `json:"working_directory,omitempty"`
	// ContinueOnError keeps the job going after this step fails (the job's
	// conclusion ignores it, the step result records the failure).
	ContinueOnError bool `json:"continue_on_error,omitempty"`
	// TimeoutMinutes bounds one step; zero means the remaining job time.
	TimeoutMinutes int `json:"timeout_minutes,omitempty"`
}

// ActionSpec is one repository-local action definition, resolved from the run
// commit and embedded in the job payload (docs/threads.md §3.4a). The runner
// never reads the repository: the definition it executes is exactly the one the
// run was created with, so a later commit (or an earlier step editing the file)
// cannot change it.
type ActionSpec struct {
	// Path is the repository-relative directory (".spindle/actions/setup").
	Path string `json:"path"`
	// Using is "composite" or "docker".
	Using string `json:"using"`
	// Inputs are the declared inputs; the runner turns them into INPUT_* env.
	Inputs map[string]ActionInputSpec `json:"inputs,omitempty"`
	// Outputs maps an output name to the template evaluated over the action's
	// own step outputs (composite actions only).
	Outputs map[string]string `json:"outputs,omitempty"`
	// Steps are the composite body (composite actions only).
	Steps []StepSpec `json:"steps,omitempty"`
	// Image is the docker image with the docker:// prefix stripped (docker
	// actions only); Entrypoint/Args/Env come from the action metadata.
	Image      string            `json:"image,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// ActionInputSpec is one declared action input (the default is the YAML scalar
// text, already a string in the action's environment).
type ActionInputSpec struct {
	Default    string `json:"default,omitempty"`
	HasDefault bool   `json:"has_default,omitempty"`
	Required   bool   `json:"required,omitempty"`
}

// EnvironmentSpec is a job's deployment environment (docs/threads.md §3.3):
// the name of a repository environment record and the URL the run page links.
type EnvironmentSpec struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

// Deployment is one job's deployment record
// (docs/PLAN-THREADS-ENVIRONMENTS.md §7): the environment it targets, the URL
// it declared, and — when the environment requires reviewers — who decided it
// and when. It lives on the job record (no separate family), so a run page and
// an API reader see the same fact and the retention pass deletes it with the
// job.
type Deployment struct {
	Environment string  `json:"environment"`
	URL         string  `json:"url,omitempty"`
	State       string  `json:"state"`
	ApprovedBy  string  `json:"approved_by,omitempty"`
	ApprovedAt  *string `json:"approved_at,omitempty"`
	DeniedBy    string  `json:"denied_by,omitempty"`
	DeniedAt    *string `json:"denied_at,omitempty"`
}

// Deployment states. `not_required` is an environment with no required
// reviewers: the job runs and may resolve that environment's secrets, recorded
// so the page can tell "no approval needed" from "not yet decided".
const (
	DeploymentNotRequired = "not_required"
	DeploymentPending     = "pending"
	DeploymentApproved    = "approved"
	DeploymentDenied      = "denied"
	DeploymentFailed      = "failed"
)

// Approval kinds (Run.ApprovalKind): which hold is on the run.
const (
	ApprovalFork        = "fork"
	ApprovalEnvironment = "environment"
)

// Cancel reasons (Run.CancelReason): who ended a live run, so a superseded run
// says WHY instead of looking like an operator's decision. These are record
// values, not display strings; the page/API renders them.
const (
	// CancelReasonOperator is a maintainer's cancel (Run.CancelledBy is the
	// principal).
	CancelReasonOperator = "operator"
	// CancelReasonSupersede is implicit supersede (PAR-150): a newer push, or a
	// PR synchronize, cancelled the run of an older commit of the same
	// workflow+ref. Run.SupersededBy names the newer run.
	CancelReasonSupersede = "supersede"
	// CancelReasonConcurrency is a concurrency group's `cancel-in-progress`
	// taking the group from the holder run (docs/threads.md §3.1).
	CancelReasonConcurrency = "concurrency"
)

// specWorkflow returns the spec-carried workflow name, falling back to the run
// header for a record written before Spec existed.
func (j *Job) specWorkflow(fallback string) string {
	if j.Spec != nil && strings.TrimSpace(j.Spec.Workflow) != "" {
		return j.Spec.Workflow
	}
	return fallback
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
	Context string `json:"context"`
	// RunsOn render the spec's label set (", "-joined); Container is the
	// image. Both are display conveniences over Spec.
	RunsOn    string   `json:"runs_on"`
	Container string   `json:"container,omitempty"`
	Needs     []string `json:"needs"`
	Steps     []Step   `json:"steps"`
	// Spec is the versioned execution payload (15 §2); nil on a record written
	// before pass A.
	Spec    *Spec  `json:"spec,omitempty"`
	Attempt int    `json:"attempt"`
	Trusted bool   `json:"trusted"`
	State   string `json:"state"`

	// Runner is the id of the runner that claimed the current attempt and
	// RunnerName its display name (round 2: attribution in the job view). Both
	// are empty until a claim.
	Runner     string `json:"runner,omitempty"`
	RunnerName string `json:"runner_name,omitempty"`
	// QueueSeq is the queue entry the current attempt was claimed from ("" for
	// a job that was never claimed). It is how attempt traffic finds the entry
	// without a queue probe.
	QueueSeq string `json:"queue_seq,omitempty"`
	// CancelRequested carries the operator's cancel signal until the attempt
	// acknowledges it (15 §3: the runner is busy, so its writes are the
	// control channel).
	CancelRequested bool `json:"cancel_requested,omitempty"`
	// LeaseUntil mirrors the claim lease for the UI.
	LeaseUntil *string `json:"lease_until,omitempty"`
	// StepResults are the runner-reported step outcomes from complete.
	StepResults []StepResult `json:"step_results,omitempty"`
	// Outputs are the job outputs the runner reported on complete, after
	// evaluating the spec's `outputs:` templates against the step outputs
	// (docs/threads.md §3.9). A dependent job reads them as
	// `needs.<job>.outputs.<name>`; a job that did not succeed exposes none.
	Outputs map[string]string `json:"outputs,omitempty"`
	// Reason is why the job did not execute its steps, or why it is not green
	// when no runner reported: a false job-level `if:`, a blocked `needs:`, or
	// an `if:` the subset cannot evaluate. Written by the server only; empty on
	// a job that ran.
	Reason string `json:"reason,omitempty"`
	// Tolerated records a FAILED job whose spec declares
	// `continue-on-error: true`: the record still reads `failure` (nothing is
	// rewritten) but the run fold treats it as neutral, so a reader can tell
	// "failed but tolerated" from "succeeded".
	Tolerated bool `json:"tolerated,omitempty"`
	// Deployment is the job's deployment record when its spec names an
	// environment (nil for an ordinary job): the environment, the declared
	// URL, and the required-reviewer decision.
	Deployment *Deployment `json:"deployment,omitempty"`

	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	StartedAt   *string `json:"started_at,omitempty"`
	CompletedAt *string `json:"completed_at,omitempty"`
	Version     int     `json:"version"`

	// DurationSeconds is a DERIVED read-model field (never stored, never CAS'd):
	// the wall time from StartedAt to CompletedAt, or to now while the job is
	// live. The run page and the API both read it.
	DurationSeconds int64 `json:"duration_seconds,omitempty"`
}

// RunView is GET …/api/threads/{run}: the header, its jobs (name-sorted), and
// (15 §2.5) the artifacts a job published. Approval is the principal-aware
// approval projection (pending/approved/denied, the environments a hold names,
// the reviewers awaited and whether THIS caller may decide) — nil only on a
// view a test built directly.
type RunView struct {
	Run       *Run            `json:"run"`
	Jobs      []*Job          `json:"jobs"`
	Artifacts []*ArtifactView `json:"artifacts"`
	Approval  *ApprovalView   `json:"approval,omitempty"`
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
	// filters. A tag push has no branch: the caller leaves it empty and
	// names the tag in RefName.
	Branch string
	// RefName is the triggering ref's SHORT name — the branch name, the tag
	// name for a tag push, or the PR target branch — and is what the
	// `on.push.branches`/`on.push.tags` globs match (after RefKind picks the
	// filter). Empty falls back to Branch, so every caller that predates tag
	// runs keeps its meaning.
	RefName string
	// RefKind names the kind of ref the event was delivered for: a branch
	// (the zero value) or a tag. Only a push distinguishes them; a tag
	// selects `on.push.tags` and is otherwise treated as a branch so a
	// non-push event can never state a ref kind it does not have.
	RefKind workflow.RefKind
	// Action is the pull_request lifecycle action being delivered: opened,
	// synchronize or reopened (workflow.PullRequestTypes). It is required for
	// a pull_request and selects the `on.pull_request.types` filter; the
	// other events ignore it. An action Threads cannot emit is ErrInvalid, so
	// a caller cannot quietly create a run for an unknown edge.
	Action string
	// Ref is the triggering ref ("refs/heads/main", "refs/tags/v1.2.3");
	// optional. It is what `github.ref` (and the job OIDC token's ref) report,
	// so a tag run keeps its refs/tags/ prefix here and nowhere in Branch.
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
	// Inputs are the workflow_dispatch input values the dispatcher supplied
	// (already validated against the workflow's declared schema). They are
	// recorded on the run header and merged into every job's spec.
	Inputs map[string]string
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
	case EventPush, EventPullReq, EventDispatch, EventSchedule:
		return true
	}
	return false
}

// validRunState reports the run state enum.
func validRunState(state string) bool {
	switch state {
	case RunQueued, RunRunning, RunSuccess, RunFailure, RunCancelled, RunSkipped, RunPendingApproval:
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
	return state == RunSuccess || state == RunFailure || state == RunCancelled || state == RunSkipped
}

// checkState maps a job state onto the checks status enum (05 §2:
// pending|in_progress|success|failure|cancelled|skipped|error). A queued job is
// `pending` and a claimed one is `in_progress` (a distinct value, so a consumer
// can tell "nobody has it" from "a runner is executing it"); a cancelled job
// maps to `cancelled` and a `needs:`-blocked one to `skipped` — both distinct,
// both non-green conclusions that never satisfy a required check and neither of
// which claims the run failed.
func checkState(jobState string) string {
	switch jobState {
	case JobSuccess:
		return "success"
	case JobFailure:
		return "failure"
	case JobCancelled:
		return "cancelled"
	case JobSkipped:
		return "skipped"
	case JobRunning:
		return "in_progress"
	default:
		return "pending"
	}
}

// foldRunState computes a run's state from its jobs: any live job keeps the run
// running (or queued when nothing started); a failed job makes the run a
// failure; otherwise a genuinely cancelled job makes it cancelled (an operator
// cancel must not read as a failure); skipped jobs are neutral (GitHub folds
// them into a green run — the failure that skipped them is already the run's
// failure); all green is success.
//
// A failed job whose `continue-on-error` is true is neutral for the same
// reason: the job record keeps `failure` (and Job.Tolerated marks it), but the
// run conclusion follows the OTHER jobs — that is exactly what
// continue-on-error is for.
func foldRunState(jobs []*Job) string {
	if len(jobs) == 0 {
		return RunQueued
	}
	allQueued, anyFailed, anyCancelled, allTerminal := true, false, false, true
	for _, j := range jobs {
		if j.State != JobQueued {
			allQueued = false
		}
		if j.State == JobFailure && !j.Tolerated {
			anyFailed = true
		}
		if j.State == JobCancelled {
			anyCancelled = true
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
	case anyCancelled:
		return RunCancelled
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
	if _, err := randRead(b[:]); err != nil {
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
