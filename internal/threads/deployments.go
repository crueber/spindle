package threads

// Deployment records and the deployments API (PAR-132/PAR-133/PAR-136).
//
// A deployment is one attempt to put a revision into an environment. Two
// producers write the same record family:
//
//   - a workflow job that names `environment:` (the protection gate in
//     environments.go) — its record id is derived from (run, job), so a rerun
//     updates the same history instead of minting a second deployment; and
//   - the deployments API (`POST …/threads/deployments`), which a workflow or
//     an external CI calls with a `deployments: write` job token.
//
// Storage (bucket-only, CAS — law 4):
//
//	repos/<o>/<r>/threads/deployments/<id>.json            the record + status history
//	repos/<o>/<r>/threads/environments/<env>/queue.json   the serialization slot
//
// The queue document is one exact-key CAS per acquire/release, never a LIST:
// a deployment holds the environment, and later deployments wait in trigger
// order. The first waiter is promoted when the holder reaches a terminal
// status (success/failure/error/inactive), so "two concurrent deploys to one
// environment serialize instead of failing" is one bucket contract.
//
// Every record is bounded: at most MaxDeploymentStatuses status entries are
// kept (oldest dropped first), and the list page is cursor-paged by id. The
// maintainer's threads-gc retention pass deletes terminal deployments past the
// configured run retention.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Deployment states accepted by the status API (GitHub's deployment_status
// vocabulary). `queued` is the visible serialization state of a deployment
// that waits behind another one in the same environment.
const (
	DeploymentStateQueued     = "queued"
	DeploymentStatePending    = "pending"
	DeploymentStateInProgress = "in_progress"
	DeploymentStateSuccess    = "success"
	DeploymentStateFailure    = "failure"
	DeploymentStateError      = "error"
	DeploymentStateInactive   = "inactive"
)

// Bounds. The queue is small because an environment is serialized: 100 waiting
// deployments is already an operator problem, and failing closed with a named
// reason beats an unbounded bucket list.
const (
	MaxEnvironmentQueue      = 100
	MaxDeploymentStatuses    = 100
	MaxDeploymentDescription = 1024
	MaxDeploymentTask        = 256
	MaxDeploymentURL         = 2048
)

// DeploymentStatus is one entry of a deployment's state history.
type DeploymentStatus struct {
	State          string `json:"state"`
	Description    string `json:"description,omitempty"`
	EnvironmentURL string `json:"environment_url,omitempty"`
	LogURL         string `json:"log_url,omitempty"`
	Creator        string `json:"creator,omitempty"`
	CreatedAt      string `json:"created_at"`
}

// DeploymentDoc is repos/<o>/<r>/threads/deployments/<id>.json. It carries the
// creation facts, the latest state, and the bounded history the environment
// page and the API render.
type DeploymentDoc struct {
	ID          string `json:"id"`
	Repo        string `json:"repo"`
	Environment string `json:"environment"`
	Ref         string `json:"ref"`
	SHA         string `json:"sha,omitempty"`
	Task        string `json:"task,omitempty"`
	Description string `json:"description,omitempty"`
	// URL is the environment URL the deployment declared (the run page links
	// it; a status may replace it).
	URL string `json:"url,omitempty"`
	// LogURL is where an operator reads the deployment log.
	LogURL string `json:"log_url,omitempty"`
	// Transient/Production mirror GitHub's deployment flags.
	Transient  bool   `json:"transient_environment,omitempty"`
	Production bool   `json:"production_environment,omitempty"`
	Creator    string `json:"creator,omitempty"`
	// RunID/JobID bind a workflow-produced deployment to its run; empty for an
	// API-created deployment.
	RunID string `json:"run_id,omitempty"`
	JobID string `json:"job_id,omitempty"`
	State string `json:"state"`
	// QueuePosition is the deployment's place in its environment queue: 0
	// means it holds the environment (or is terminal), 1..N names the wait.
	QueuePosition int                `json:"queue_position,omitempty"`
	QueueReason   string             `json:"queue_reason,omitempty"`
	Statuses      []DeploymentStatus `json:"statuses"`
	CreatedAt     string             `json:"created_at"`
	UpdatedAt     string             `json:"updated_at"`
	Version       int                `json:"version"`
}

// DeploymentPage is GET …/threads/deployments: newest first, cursor-paged by
// deployment id.
type DeploymentPage struct {
	Deployments []*DeploymentDoc `json:"deployments"`
	More        bool             `json:"more"`
}

// DeploymentInput is the create body. `ref` and `environment` are required;
// every other field is optional metadata. `sha` lets a caller pin the revision
// when the server has no git backend (tests, an external deployment).
type DeploymentInput struct {
	Ref                   string `json:"ref"`
	Environment           string `json:"environment"`
	Description           string `json:"description"`
	URL                   string `json:"url"`
	LogURL                string `json:"log_url"`
	SHA                   string `json:"sha"`
	Task                  string `json:"task"`
	TransientEnvironment  bool   `json:"transient_environment"`
	ProductionEnvironment bool   `json:"production_environment"`
}

// DeploymentStatusInput is the status body.
type DeploymentStatusInput struct {
	State          string `json:"state"`
	Description    string `json:"description"`
	EnvironmentURL string `json:"environment_url"`
	LogURL         string `json:"log_url"`
}

// DeploymentsPrefix returns repos/<o>/<r>/threads/deployments/.
func DeploymentsPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/threads/deployments/"
}

// DeploymentKey returns repos/<o>/<r>/threads/deployments/<id>.json.
func DeploymentKey(owner, repo, id string) string {
	return DeploymentsPrefix(owner, repo) + id + ".json"
}

// EnvironmentQueueKey returns the environment's serialization document.
func EnvironmentQueueKey(owner, repo, env string) string {
	return EnvironmentsPrefix(owner, repo) + env + "/queue.json"
}

// ValidDeploymentState reports whether a state is one a status may carry.
func ValidDeploymentState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case DeploymentStateQueued, DeploymentStatePending, DeploymentStateInProgress,
		DeploymentStateSuccess, DeploymentStateFailure, DeploymentStateError, DeploymentStateInactive:
		return true
	}
	return false
}

// terminalDeploymentState reports a state that no longer holds the
// environment's serialization slot.
func terminalDeploymentState(state string) bool {
	switch state {
	case DeploymentStateSuccess, DeploymentStateFailure, DeploymentStateError, DeploymentStateInactive:
		return true
	}
	return false
}

// jobDeploymentID is the deterministic record id for a workflow job's
// deployment. The run id is time-ordered, so the pair sorts stably.
func jobDeploymentID(runID, jobID string) string { return "job-" + runID + "-" + jobID }

// deploymentRecordState maps a job deployment decision onto the record state.
func deploymentRecordState(job *Job, run *Run) string {
	if job == nil || job.Deployment == nil {
		return ""
	}
	dep := job.Deployment
	switch {
	case dep.QueuePosition > 0:
		return DeploymentStateQueued
	case dep.State == DeploymentDenied || dep.State == DeploymentFailed || job.State == JobFailure:
		return DeploymentStateFailure
	case job.State == JobCancelled || job.State == JobSkipped:
		return DeploymentStateInactive
	case job.State == JobSuccess:
		return DeploymentStateSuccess
	case job.State == JobRunning || dep.State == DeploymentApproved:
		return DeploymentStateInProgress
	default:
		return DeploymentStatePending
	}
}

// --- environment serialization (PAR-136) ------------------------------------

type deploymentQueueEntry struct {
	Token string `json:"token"`
	Label string `json:"label"`
	RunID string `json:"run_id,omitempty"`
	JobID string `json:"job_id,omitempty"`
	At    string `json:"at"`
}

type deploymentQueueDoc struct {
	Holder      string                 `json:"holder,omitempty"`
	HolderLabel string                 `json:"holder_label,omitempty"`
	Entries     []deploymentQueueEntry `json:"entries,omitempty"`
	UpdatedAt   string                 `json:"updated_at"`
	Version     int                    `json:"version"`
}

// loadEnvironmentQueue reads one environment's queue (nil = free).
func (s *Service) loadEnvironmentQueue(ctx context.Context, owner, repo, env string) (*deploymentQueueDoc, error) {
	raw, _, err := s.getJSON(ctx, EnvironmentQueueKey(owner, repo, env))
	if err != nil || raw == nil {
		return nil, err
	}
	var doc deploymentQueueDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		return nil, fmt.Errorf("%w: environment queue %s: %v", ErrCorrupt, env, jerr)
	}
	return &doc, nil
}

// acquireEnvironmentSlot places token in the environment's serialization
// queue. It returns the position (0 = it now holds the environment) and, when
// it waits, the visible reason. A token already queued is idempotent.
func (s *Service) acquireEnvironmentSlot(ctx context.Context, owner, repo, env, token, label, runID, jobID string) (int, string, error) {
	now := s.nowUTC().Format(dateTimeFmt)
	position := 0
	reason := ""
	_, err := s.casUpdate(ctx, EnvironmentQueueKey(owner, repo, env), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		doc := &deploymentQueueDoc{}
		if cur != nil {
			if jerr := json.Unmarshal(cur, doc); jerr != nil {
				return nil, false, fmt.Errorf("%w: environment queue %s: %v", ErrCorrupt, env, jerr)
			}
		}
		if doc.Holder == token {
			position, reason = 0, ""
			return nil, false, nil
		}
		for i, entry := range doc.Entries {
			if entry.Token == token {
				position = i + 1
				reason = queueBusyReason(env, doc)
				return nil, false, nil
			}
		}
		if doc.Holder == "" {
			doc.Holder, doc.HolderLabel = token, label
			position, reason = 0, ""
		} else {
			if len(doc.Entries) >= MaxEnvironmentQueue {
				return nil, false, fmt.Errorf("%w: environment %q queue is full (%d waiting deployments)",
					ErrConflict, env, MaxEnvironmentQueue)
			}
			doc.Entries = append(doc.Entries, deploymentQueueEntry{Token: token, Label: label, RunID: runID, JobID: jobID, At: now})
			position = len(doc.Entries)
			reason = queueBusyReason(env, doc)
		}
		doc.UpdatedAt = now
		doc.Version++
		return mustJSON(doc), true, nil
	})
	return position, reason, err
}

// queueBusyReason is the one visible sentence a queued deployment carries.
func queueBusyReason(env string, doc *deploymentQueueDoc) string {
	holder := strings.TrimSpace(doc.HolderLabel)
	if holder == "" {
		holder = strings.TrimSpace(doc.Holder)
	}
	if holder == "" {
		holder = "another deployment"
	}
	return fmt.Sprintf("environment %q is busy: %s is still deploying", env, holder)
}

// releaseEnvironmentSlot releases token's hold (or removes it from the wait
// list when it never held the slot). It returns the promoted waiter, if any.
func (s *Service) releaseEnvironmentSlot(ctx context.Context, owner, repo, env, token string) (*deploymentQueueEntry, error) {
	now := s.nowUTC().Format(dateTimeFmt)
	var promoted *deploymentQueueEntry
	_, err := s.casUpdate(ctx, EnvironmentQueueKey(owner, repo, env), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, nil
		}
		var doc deploymentQueueDoc
		if jerr := json.Unmarshal(cur, &doc); jerr != nil {
			return nil, false, fmt.Errorf("%w: environment queue %s: %v", ErrCorrupt, env, jerr)
		}
		if doc.Holder != token {
			kept := doc.Entries[:0]
			removed := false
			for _, entry := range doc.Entries {
				if entry.Token == token {
					removed = true
					continue
				}
				kept = append(kept, entry)
			}
			if !removed {
				return nil, false, nil
			}
			doc.Entries = kept
		} else if len(doc.Entries) > 0 {
			next := doc.Entries[0]
			doc.Entries = doc.Entries[1:]
			doc.Holder, doc.HolderLabel = next.Token, next.Label
			promoted = &next
		} else {
			doc.Holder, doc.HolderLabel = "", ""
		}
		doc.UpdatedAt = now
		doc.Version++
		return mustJSON(&doc), true, nil
	})
	if err != nil {
		return nil, err
	}
	if promoted != nil {
		s.promoteDeployment(ctx, owner, repo, env, promoted)
	}
	return promoted, nil
}

// promoteDeployment makes a promoted waiter visible: a job's deployment loses
// its queue position (the claim path can now offer it), and an API deployment
// moves from `queued` to `pending`. Best effort: the queue document is the
// authority, and a stale record is repaired on the next read.
func (s *Service) promoteDeployment(ctx context.Context, owner, repo, env string, entry *deploymentQueueEntry) {
	if entry == nil {
		return
	}
	if entry.RunID != "" && entry.JobID != "" {
		job, _, err := s.loadJobAndRun(ctx, owner, repo, entry.RunID, entry.JobID)
		if err != nil || job == nil || job.Deployment == nil || job.Deployment.QueuePosition == 0 {
			return
		}
		dep := *job.Deployment
		dep.QueuePosition, dep.QueueReason = 0, ""
		_, _ = s.UpdateJob(ctx, repoID(owner, repo), entry.RunID, entry.JobID, JobUpdate{Deployment: &dep})
		return
	}
	if id, ok := strings.CutPrefix(entry.Token, "dep:"); ok {
		doc, err := s.loadDeployment(ctx, owner, repo, id)
		if err != nil || doc == nil || doc.QueuePosition == 0 {
			return
		}
		if doc.State == DeploymentStateQueued {
			doc.State = DeploymentStatePending
			doc.Statuses = appendDeploymentStatus(doc.Statuses, DeploymentStatus{
				State: DeploymentStatePending, Creator: "threads", CreatedAt: s.nowUTC().Format(dateTimeFmt),
			})
		}
		doc.QueuePosition, doc.QueueReason = 0, ""
		doc.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		doc.Version++
		_, _ = s.casUpdate(ctx, DeploymentKey(owner, repo, id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
			if cur == nil {
				return nil, false, nil
			}
			var live DeploymentDoc
			if jerr := json.Unmarshal(cur, &live); jerr != nil {
				return nil, false, nil
			}
			live.State = doc.State
			live.Statuses = doc.Statuses
			live.QueuePosition, live.QueueReason = 0, ""
			live.UpdatedAt = doc.UpdatedAt
			live.Version++
			return mustJSON(&live), true, nil
		})
	}
}

// --- records ----------------------------------------------------------------

// loadDeployment reads one record exactly (nil = absent).
func (s *Service) loadDeployment(ctx context.Context, owner, repo, id string) (*DeploymentDoc, error) {
	raw, _, err := s.getJSON(ctx, DeploymentKey(owner, repo, id))
	if err != nil || raw == nil {
		return nil, err
	}
	var doc DeploymentDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		return nil, fmt.Errorf("%w: deployment %s: %v", ErrCorrupt, id, jerr)
	}
	return &doc, nil
}

// appendDeploymentStatus appends one history entry, keeping the newest
// MaxDeploymentStatuses. The newest entry is never dropped.
func appendDeploymentStatus(in []DeploymentStatus, st DeploymentStatus) []DeploymentStatus {
	in = append(in, st)
	if len(in) > MaxDeploymentStatuses {
		in = in[len(in)-MaxDeploymentStatuses:]
	}
	return in
}

// deploymentExpired is the retention rule: a terminal deployment older than
// the run window is gone (unless keep_forever).
func (s *Service) deploymentExpired(doc *DeploymentDoc, now time.Time) bool {
	if doc == nil || s.KeepForever || !terminalDeploymentState(doc.State) {
		return false
	}
	at, err := time.Parse(dateTimeFmt, doc.CreatedAt)
	if err != nil {
		return false
	}
	window := s.retention()
	return !at.Add(window).After(now)
}

// requireDeploymentRole authorizes the deployment surface. A Threads job token
// is scoped to its own repository and the `deployments` scope — never admin and
// never another repository (PAR-133/S10). Every other caller uses the normal
// repository ladder.
func (s *Service) requireDeploymentRole(ctx context.Context, owner, repo string, p auth.Principal, write bool) error {
	if p.JobRepo != "" {
		if !strings.EqualFold(strings.TrimSpace(p.JobRepo), repoID(owner, repo)) {
			return fmt.Errorf("%w: the job token is scoped to %s", ErrForbidden, p.JobRepo)
		}
		level := strings.ToLower(strings.TrimSpace(p.JobPerms["deployments"]))
		if write {
			if level == PermWrite {
				return nil
			}
			return fmt.Errorf("%w: the job token lacks deployments: write", ErrForbidden)
		}
		if level == PermRead || level == PermWrite {
			return nil
		}
		return fmt.Errorf("%w: the job token lacks deployments: read", ErrForbidden)
	}
	if write {
		return s.requireRole(ctx, owner, repo, p, "write")
	}
	return s.requireRead(ctx, owner, repo, p)
}

// CreateDeployment records a deployment through the API (PAR-133). The
// environment must exist (fail closed, exactly like a job that names an
// undefined one), the ref must pass the deployment branch policy, and the
// environment's serialization slot decides whether the record starts `pending`
// or visibly `queued`.
func (s *Service) CreateDeployment(ctx context.Context, repo string, in DeploymentInput, p auth.Principal) (*DeploymentDoc, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireDeploymentRole(ctx, owner, name, p, true); err != nil {
		return nil, err
	}
	env, err := normalizeEnvironmentName(in.Environment)
	if err != nil {
		return nil, err
	}
	doc, err := s.loadEnvironment(ctx, owner, name, env)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("%w: environment %q is not defined in this repository", ErrNotFound, env)
	}
	ref := strings.TrimSpace(in.Ref)
	if ref == "" {
		return nil, fmt.Errorf("%w: ref is required", ErrInvalid)
	}
	if len(ref) > 512 || strings.ContainsAny(ref, "\x00\r\n") {
		return nil, fmt.Errorf("%w: invalid ref", ErrInvalid)
	}
	run := &Run{Ref: ref}
	if allowed, reason := deploymentRefAllowed(doc.DeploymentBranches, run, env); !allowed {
		return nil, fmt.Errorf("%w: %s", ErrForbidden, reason)
	}
	sha := strings.TrimSpace(strings.ToLower(in.SHA))
	if sha != "" {
		if _, nerr := normalizeSHA(sha); nerr != nil {
			return nil, nerr
		}
	} else if s.Git != nil {
		if resolved, rerr := s.Git.ResolveRef(ctx, repo, ref); rerr == nil {
			sha = resolved
		}
	}
	if len(in.Description) > MaxDeploymentDescription {
		return nil, fmt.Errorf("%w: description must be ≤ %d bytes", ErrInvalid, MaxDeploymentDescription)
	}
	if len(in.Task) > MaxDeploymentTask {
		return nil, fmt.Errorf("%w: task must be ≤ %d bytes", ErrInvalid, MaxDeploymentTask)
	}
	if err := validDeploymentURL(in.URL); err != nil {
		return nil, err
	}
	if err := validDeploymentURL(in.LogURL); err != nil {
		return nil, err
	}
	now := s.nowUTC()
	id, ierr := newRunID(now)
	if ierr != nil {
		return nil, fmt.Errorf("%w: deployment id: %v", ErrUnavailable, ierr)
	}
	id = "d-" + strings.TrimPrefix(id, "run-")
	token := "dep:" + id
	label := fmt.Sprintf("deployment %s (ref %s)", id, ref)
	position, reason, qerr := s.acquireEnvironmentSlot(ctx, owner, name, env, token, label, "", "")
	if qerr != nil {
		return nil, qerr
	}
	state := DeploymentStatePending
	if position > 0 {
		state = DeploymentStateQueued
	}
	creator := strings.ToLower(strings.TrimSpace(p.Name))
	record := &DeploymentDoc{
		ID: id, Repo: repoID(owner, name), Environment: env, Ref: ref, SHA: sha,
		Task: in.Task, Description: in.Description, URL: in.URL, LogURL: in.LogURL,
		Transient: in.TransientEnvironment, Production: in.ProductionEnvironment,
		Creator: creator, State: state, QueuePosition: position, QueueReason: reason,
		CreatedAt: now.Format(dateTimeFmt), UpdatedAt: now.Format(dateTimeFmt), Version: 1,
	}
	record.Statuses = []DeploymentStatus{{
		State: state, Description: in.Description, EnvironmentURL: in.URL,
		LogURL: in.LogURL, Creator: creator, CreatedAt: record.CreatedAt,
	}}
	record.Statuses = record.Statuses[:1]
	if err := s.putCreate(ctx, DeploymentKey(owner, name, id), mustJSON(record)); err != nil {
		return nil, err
	}
	return record, nil
}

// UpdateDeploymentStatus appends one status transition (PAR-133). A queued
// deployment cannot leave `queued` before its slot is promoted, and a terminal
// deployment releases the environment so the next waiter starts.
func (s *Service) UpdateDeploymentStatus(ctx context.Context, repo, id string, in DeploymentStatusInput, p auth.Principal) (*DeploymentDoc, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireDeploymentRole(ctx, owner, name, p, true); err != nil {
		return nil, err
	}
	state := strings.ToLower(strings.TrimSpace(in.State))
	if !ValidDeploymentState(state) {
		return nil, fmt.Errorf("%w: state must be queued|pending|in_progress|success|failure|error|inactive", ErrInvalid)
	}
	if len(in.Description) > MaxDeploymentDescription {
		return nil, fmt.Errorf("%w: description must be ≤ %d bytes", ErrInvalid, MaxDeploymentDescription)
	}
	if err := validDeploymentURL(in.EnvironmentURL); err != nil {
		return nil, err
	}
	if err := validDeploymentURL(in.LogURL); err != nil {
		return nil, err
	}
	now := s.nowUTC().Format(dateTimeFmt)
	actor := strings.ToLower(strings.TrimSpace(p.Name))
	var out *DeploymentDoc
	var release bool
	env := ""
	_, err = s.casUpdate(ctx, DeploymentKey(owner, name, id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown deployment %q", ErrNotFound, id)
		}
		var doc DeploymentDoc
		if jerr := json.Unmarshal(cur, &doc); jerr != nil {
			return nil, false, fmt.Errorf("%w: deployment %s: %v", ErrCorrupt, id, jerr)
		}
		env = doc.Environment
		if doc.QueuePosition > 0 && state != DeploymentStateQueued {
			return nil, false, fmt.Errorf("%w: deployment %s is still queued (position %d) in environment %q",
				ErrConflict, id, doc.QueuePosition, doc.Environment)
		}
		if in.EnvironmentURL != "" {
			doc.URL = in.EnvironmentURL
		}
		if in.LogURL != "" {
			doc.LogURL = in.LogURL
		}
		doc.State = state
		doc.Statuses = appendDeploymentStatus(doc.Statuses, DeploymentStatus{
			State: state, Description: in.Description, EnvironmentURL: doc.URL,
			LogURL: doc.LogURL, Creator: actor, CreatedAt: now,
		})
		doc.UpdatedAt = now
		doc.Version++
		out = &doc
		release = terminalDeploymentState(state) && doc.QueuePosition == 0
		return mustJSON(&doc), true, nil
	})
	if err != nil {
		return nil, err
	}
	if release && env != "" {
		_, _ = s.releaseEnvironmentSlot(ctx, owner, name, env, "dep:"+id)
	}
	return out, nil
}

// ListDeployments pages the repository's deployment records newest first,
// optionally for one environment (PAR-132). One bounded LIST behind a page
// request; the per-environment settings read is not a job hot path.
func (s *Service) ListDeployments(ctx context.Context, repo, environment string, p auth.Principal, after string, n int) (*DeploymentPage, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireDeploymentRole(ctx, owner, name, p, false); err != nil {
		return nil, err
	}
	environment = strings.TrimSpace(environment)
	if environment != "" {
		environment, err = normalizeEnvironmentName(environment)
		if err != nil {
			return nil, err
		}
	}
	if n <= 0 {
		n = defaultPageSize
	}
	if n > maxPageSize {
		n = maxPageSize
	}
	after = strings.TrimSpace(after)
	prefix := DeploymentsPrefix(owner, name)
	var ids []string
	if lerr := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		rest := strings.TrimPrefix(m.Key, prefix)
		if strings.Contains(rest, "/") || !strings.HasSuffix(rest, ".json") {
			return nil
		}
		ids = append(ids, strings.TrimSuffix(rest, ".json"))
		return nil
	}); lerr != nil {
		return nil, lerr
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	page := &DeploymentPage{Deployments: []*DeploymentDoc{}}
	now := s.nowUTC()
	scanned := 0
	for _, id := range ids {
		if after != "" && id >= after {
			continue
		}
		if len(page.Deployments) >= n {
			page.More = true
			break
		}
		if scanned >= filterScanMax {
			page.More = true
			break
		}
		scanned++
		doc, derr := s.loadDeployment(ctx, owner, name, id)
		if derr != nil {
			return nil, derr
		}
		if doc == nil || (environment != "" && doc.Environment != environment) {
			continue
		}
		if s.deploymentExpired(doc, now) {
			continue
		}
		page.Deployments = append(page.Deployments, doc)
	}
	return page, nil
}

// GetDeployment reads one record (read gate; retention-aware 404).
func (s *Service) GetDeployment(ctx context.Context, repo, id string, p auth.Principal) (*DeploymentDoc, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireDeploymentRole(ctx, owner, name, p, false); err != nil {
		return nil, err
	}
	doc, err := s.loadDeployment(ctx, owner, name, id)
	if err != nil {
		return nil, err
	}
	if doc == nil || s.deploymentExpired(doc, s.nowUTC()) {
		return nil, fmt.Errorf("%w: unknown deployment %q", ErrNotFound, id)
	}
	return doc, nil
}

// syncJobDeployment writes the job's deployment into the shared record family
// (PAR-132). It is best effort: the job record is the execution authority, and
// a lost history write is repaired by the next transition or the GC pass.
func (s *Service) syncJobDeployment(ctx context.Context, owner, repo string, job *Job, run *Run) {
	if s == nil || job == nil || job.Deployment == nil || run == nil {
		return
	}
	id := job.Deployment.DeploymentID
	if id == "" {
		id = jobDeploymentID(job.RunID, job.ID)
	}
	state := deploymentRecordState(job, run)
	now := s.nowUTC().Format(dateTimeFmt)
	creator := strings.ToLower(strings.TrimSpace(run.Actor))
	actor := creator
	if job.Deployment.ApprovedBy != "" {
		actor = job.Deployment.ApprovedBy
	}
	if job.Deployment.DeniedBy != "" {
		actor = job.Deployment.DeniedBy
	}
	_, err := s.casUpdate(ctx, DeploymentKey(owner, repo, id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		doc := &DeploymentDoc{}
		if cur != nil {
			if jerr := json.Unmarshal(cur, doc); jerr != nil {
				return nil, false, nil // an unreadable record must not fail the job path
			}
		} else {
			doc.ID, doc.Repo, doc.Environment = id, repoID(owner, repo), job.Deployment.Environment
			doc.Ref, doc.SHA = run.Ref, run.Commit
			doc.Creator, doc.RunID, doc.JobID = creator, run.ID, job.ID
			doc.CreatedAt = now
			doc.State = state
			doc.Statuses = []DeploymentStatus{{State: state, Creator: actor, CreatedAt: now}}
		}
		doc.URL = job.Deployment.URL
		doc.QueuePosition = job.Deployment.QueuePosition
		doc.QueueReason = job.Deployment.QueueReason
		if doc.State != state {
			doc.State = state
			doc.Statuses = appendDeploymentStatus(doc.Statuses, DeploymentStatus{
				State: state, Description: job.Reason, EnvironmentURL: doc.URL, Creator: actor, CreatedAt: now,
			})
		}
		doc.UpdatedAt = now
		doc.Version++
		return mustJSON(doc), true, nil
	})
	_ = err
}

// pruneDeployments is the retention half of PAR-132: a terminal deployment
// older than the run window is deleted (never a live or queued one). It runs
// from the maintainer's threads-gc pass, never the request path.
func (s *Service) pruneDeployments(ctx context.Context, owner, repo string) (int, error) {
	if s == nil || s.Store == nil || s.KeepForever {
		return 0, nil
	}
	prefix := DeploymentsPrefix(owner, repo)
	var keys []string
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, ".json") {
			keys = append(keys, m.Key)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	now := s.nowUTC()
	removed := 0
	for _, key := range keys {
		raw, _, err := s.getJSON(ctx, key)
		if err != nil || raw == nil {
			continue
		}
		var doc DeploymentDoc
		if jerr := json.Unmarshal(raw, &doc); jerr != nil || !s.deploymentExpired(&doc, now) {
			continue
		}
		if derr := s.Store.Delete(ctx, key, ""); derr != nil {
			return removed, derr
		}
		removed++
	}
	return removed, nil
}

// validDeploymentURL accepts an empty URL or an absolute http(s) one.
func validDeploymentURL(u string) error {
	u = strings.TrimSpace(u)
	if u == "" {
		return nil
	}
	if len(u) > MaxDeploymentURL {
		return fmt.Errorf("%w: URL must be ≤ %d bytes", ErrInvalid, MaxDeploymentURL)
	}
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return fmt.Errorf("%w: URL must be absolute http(s)", ErrInvalid)
	}
	return nil
}
