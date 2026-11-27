package threads

import (
	"encoding/json"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"
)

// The runner protocol (docs/PLAN-INSTANCE-RUNNERS.md §4): JSON over HTTPS,
// authenticated by the protocol's own tokens (never a session cookie), versioned
// by X-Spindle-Runner: 1 on every response. Paths are top-level because a runner
// is instance infrastructure, not a repository member:
//
//	POST /api/v1/threads/runners/register         registration token → runner credential
//	POST /api/v1/threads/runners/declare          version + labels + capacity (liveness)
//	POST /api/v1/threads/runners/fetch            global long-poll (≤ 30 s), 204 when idle
//	POST /api/v1/threads/jobs/{ref}/attempt/{n}/start|log|complete|cancel
//
// The host-admin fleet surface lives beside it (the service is the gate):
//
//	POST   /api/v1/admin/runners/tokens           mint a one-time registration token
//	GET    /api/v1/admin/runners                  the instance fleet
//	DELETE /api/v1/admin/runners/{id}             revoke
//	POST   /api/v1/admin/runners/{id}/policy      change one runner's repository policy
//
// {ref} is an opaque job reference (jr_…, see JobRef): the protocol table's
// {id} cannot be a bare job id, because two runs may both own "j-build".

// runnerVersionHeader is the protocol version every runner response carries.
const runnerVersionHeader = "X-Spindle-Runner"

// runnerVersion is the protocol version this server speaks.
const runnerVersion = "1"

// maxProtocolBody bounds a protocol request body (a 256 KiB log frame plus JSON
// escaping headroom).
const maxProtocolBody = 2 * MaxLogFrame

// maxCompleteBody bounds the complete payload: a job may report up to
// MaxJobSummaryBytes of step summary plus its annotations and step results, so
// the generic protocol bound (a log frame pair) is too small for this one call.
const maxCompleteBody = MaxJobSummaryBytes + 2*MaxLogFrame

// handleTop answers /api/v1/threads/… (the runner protocol). It reports false
// for anything else, so the core api mux answers.
func (h *Handler) handleTop(w http.ResponseWriter, r *http.Request, segs []string) bool {
	// segs = ["api"|"api-browser", "v1", "threads", …]
	if len(segs) < 4 || segs[2] != "threads" {
		return false
	}
	// The protocol is machine-only: /api/v1 only (no browser twin).
	if segs[0] != "api" {
		return false
	}
	w.Header().Set(runnerVersionHeader, runnerVersion)
	rest := segs[3:]
	switch {
	case len(rest) == 2 && rest[0] == "runners":
		switch rest[1] {
		case "register":
			if r.Method != http.MethodPost {
				methodNotAllowed(w, "POST")
				return true
			}
			h.runnerRegister(w, r)
			return true
		case "declare":
			if r.Method != http.MethodPost {
				methodNotAllowed(w, "POST")
				return true
			}
			h.runnerDeclare(w, r)
			return true
		case "fetch":
			if r.Method != http.MethodPost {
				methodNotAllowed(w, "POST")
				return true
			}
			h.runnerFetch(w, r)
			return true
		case "drain", "resume", "rotate", "deregister":
			h.runnerSelf(w, r, rest[1:])
			return true
		}
		writePlain(w, http.StatusNotFound, "unknown runner endpoint")
		return true
	case len(rest) >= 5 && rest[0] == "jobs" && rest[2] == "attempt":
		h.attemptOp(w, r, rest[1], rest[3], rest[4:])
		return true
	}
	return false
}

// handleAdminRunners answers /api/v1/admin/runners[/…] (the instance fleet).
// It is a host-admin surface on both JSON lanes: the service's requireHostAdmin
// decides (401 anonymous, 403 authenticated non-admin).
func (h *Handler) handleAdminRunners(w http.ResponseWriter, r *http.Request, rest []string) bool {
	if h.Svc == nil || h.Svc.Store == nil {
		writePlain(w, http.StatusServiceUnavailable, "threads service not wired")
		return true
	}
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		h.listRunners(w, r)
	case len(rest) == 1 && rest[0] == "tokens" && r.Method == http.MethodPost:
		h.mintRunnerToken(w, r)
	case len(rest) == 2 && rest[1] == "policy" && r.Method == http.MethodPost:
		h.setRunnerPolicy(w, r, rest[0])
	case len(rest) == 2 && (rest[1] == "drain" || rest[1] == "resume" || rest[1] == "rotate-token" || rest[1] == "deregister" || rest[1] == "group"):
		h.runnerFleetVerb(w, r, rest[0], rest[1:])
	case len(rest) == 1 && r.Method == http.MethodDelete:
		h.revokeRunner(w, r, rest[0])
	case len(rest) == 0:
		methodNotAllowed(w, "GET")
	case len(rest) == 1 && rest[0] == "tokens":
		methodNotAllowed(w, "POST")
	case len(rest) == 2 && rest[1] == "policy":
		methodNotAllowed(w, "POST")
	case len(rest) == 2:
		writePlain(w, http.StatusNotFound, "unknown runner operation")
	default:
		methodNotAllowed(w, "DELETE")
	}
	return true
}

// runnerRegister consumes a registration token once.
func (h *Handler) runnerRegister(w http.ResponseWriter, r *http.Request) {
	var in RegisterInput
	if !decodeStrict(w, r, maxProtocolBody, map[string]bool{
		"token": true, "name": true, "labels": true, "capacity": true,
		"allow_fork_runs": true, "ephemeral": true,
		"max_cpus": true, "max_memory_bytes": true,
	}, &in) {
		return
	}
	if h.Svc == nil || h.Svc.Store == nil {
		writePlain(w, http.StatusServiceUnavailable, "threads service not wired")
		return
	}
	res, err := h.Svc.Register(r.Context(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// runnerDeclare records a runner's version and labels.
func (h *Handler) runnerDeclare(w http.ResponseWriter, r *http.Request) {
	auth, err := h.Svc.authorizeRunner(r.Context(), bearerToken(r.Header.Get("Authorization")))
	if err != nil {
		writeErr(w, err)
		return
	}
	var in DeclareInput
	if !decodeStrict(w, r, 16<<10, map[string]bool{
		"version": true, "labels": true, "capacity": true, "allow_fork_runs": true,
		"ephemeral": true, "max_cpus": true, "max_memory_bytes": true,
	}, &in) {
		return
	}
	view, derr := h.Svc.Declare(r.Context(), auth, in)
	if derr != nil {
		writeErr(w, derr)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// runnerFetch long-polls for one job matching the runner's labels.
func (h *Handler) runnerFetch(w http.ResponseWriter, r *http.Request) {
	auth, err := h.Svc.authorizeRunner(r.Context(), bearerToken(r.Header.Get("Authorization")))
	if err != nil {
		writeErr(w, err)
		return
	}
	wait := 20 * time.Second
	if v := r.URL.Query().Get("timeout"); v != "" {
		secs, cerr := strconv.Atoi(v)
		if cerr != nil || secs < 0 || secs > 30 {
			writePlain(w, http.StatusBadRequest, "invalid timeout: 0–30 seconds")
			return
		}
		wait = time.Duration(secs) * time.Second
	}
	job, ferr := h.Svc.FetchJobFrom(r.Context(), auth, wait, h.checkoutBase(r))
	if ferr != nil {
		writeErr(w, ferr)
		return
	}
	if job == nil {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// attemptOp dispatches one attempt-scoped operation. op is the remainder of the
// path, so an artifact transfer's nested route (artifacts/{name}/blobs/{index})
// stays under the same authenticated job channel.
func (h *Handler) attemptOp(w http.ResponseWriter, r *http.Request, ref, nRaw string, op []string) {
	attempt, nerr := attemptPathNumber(nRaw)
	if nerr != nil {
		writeErr(w, nerr)
		return
	}
	if len(op) == 1 {
		switch op[0] {
		case "start":
			if r.Method != http.MethodPost {
				methodNotAllowed(w, "POST")
				return
			}
			h.attemptStart(w, r, ref, attempt)
			return
		case "log":
			if r.Method != http.MethodPost {
				methodNotAllowed(w, "POST")
				return
			}
			h.attemptLog(w, r, ref, attempt)
			return
		case "complete":
			if r.Method != http.MethodPost {
				methodNotAllowed(w, "POST")
				return
			}
			h.attemptComplete(w, r, ref, attempt)
			return
		case "cancel":
			if r.Method != http.MethodPost {
				methodNotAllowed(w, "POST")
				return
			}
			h.attemptCancel(w, r, ref, attempt)
			return
		case "artifacts":
			h.attemptArtifacts(w, r, ref, attempt, nil)
			return
		case "cache":
			h.attemptCache(w, r, ref, attempt, nil)
			return
		case "id-token":
			if r.Method != http.MethodPost {
				methodNotAllowed(w, "POST")
				return
			}
			h.exchangeIDToken(w, r, ref, attempt)
			return
		}
	}
	if len(op) >= 1 && op[0] == "artifacts" {
		h.attemptArtifacts(w, r, ref, attempt, op[1:])
		return
	}
	if len(op) >= 1 && op[0] == "cache" {
		h.attemptCache(w, r, ref, attempt, op[1:])
		return
	}
	writePlain(w, http.StatusNotFound, "unknown attempt operation")
}

// attemptStart issues the attempt token (authenticated by the runner token: it
// is the call that CREATES the attempt credential).
func (h *Handler) attemptStart(w http.ResponseWriter, r *http.Request, ref string, attempt int) {
	// Reject an attempt token here with a clear answer instead of a confusing
	// "unknown runner token".
	if strings.HasPrefix(bearerToken(r.Header.Get("Authorization")), attemptTokenPrefix) {
		writePlain(w, http.StatusUnauthorized, "start takes the runner token; the attempt token it returns authorises log/complete/cancel")
		return
	}
	auth, err := h.Svc.authorizeRunner(r.Context(), bearerToken(r.Header.Get("Authorization")))
	if err != nil {
		writeErr(w, err)
		return
	}
	view, serr := h.Svc.Start(r.Context(), auth, ref, attempt)
	if serr != nil {
		writeErr(w, serr)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// attemptClaims authenticates one attempt-scoped write and checks the URL's job
// reference and attempt number against the token (defence in depth: the token
// alone is authoritative, but a mismatch means a bugging runner).
func (h *Handler) attemptClaims(w http.ResponseWriter, r *http.Request, ref string, attempt int) (AttemptClaims, bool) {
	claims, err := h.Svc.parseAttemptToken(bearerToken(r.Header.Get("Authorization")), h.Svc.nowUTC())
	if err != nil {
		writeErr(w, err)
		return AttemptClaims{}, false
	}
	owner, repo, runID, jobID, perr := parseJobRef(ref)
	if perr != nil {
		writePlain(w, http.StatusNotFound, "unknown job reference")
		return AttemptClaims{}, false
	}
	if claims.Repo != repoID(owner, repo) || claims.Run != runID || claims.Job != jobID {
		writePlain(w, http.StatusForbidden, "attempt token is scoped to another job")
		return AttemptClaims{}, false
	}
	if claims.Attempt != attempt {
		writePlain(w, http.StatusForbidden, "attempt token is scoped to another attempt")
		return AttemptClaims{}, false
	}
	return claims, true
}

// attemptLog appends one idempotent log frame.
func (h *Handler) attemptLog(w http.ResponseWriter, r *http.Request, ref string, attempt int) {
	claims, ok := h.attemptClaims(w, r, ref, attempt)
	if !ok {
		return
	}
	var in struct {
		Seq  int    `json:"seq"`
		Text string `json:"text"`
	}
	if !decodeStrict(w, r, maxProtocolBody, map[string]bool{"seq": true, "text": true}, &in) {
		return
	}
	view, err := h.Svc.AppendLog(r.Context(), claims, in.Seq, in.Text)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// attemptComplete records the conclusion and step results.
func (h *Handler) attemptComplete(w http.ResponseWriter, r *http.Request, ref string, attempt int) {
	claims, ok := h.attemptClaims(w, r, ref, attempt)
	if !ok {
		return
	}
	var in CompleteInput
	if !decodeStrict(w, r, maxCompleteBody, map[string]bool{"conclusion": true, "steps": true, "outputs": true, "annotations": true, "summary": true}, &in) {
		return
	}
	view, err := h.Svc.Complete(r.Context(), claims, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// attemptCancel acknowledges a cancellation (no body).
func (h *Handler) attemptCancel(w http.ResponseWriter, r *http.Request, ref string, attempt int) {
	claims, ok := h.attemptClaims(w, r, ref, attempt)
	if !ok {
		return
	}
	view, err := h.Svc.AckCancel(r.Context(), claims)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// --- host-admin fleet surface ------------------------------------------------

// runners / runner tokens are HOST-ADMIN operations (never a repository role:
// an instance runner is infrastructure and may be offered any repository's
// jobs). The service re-checks the gate, so the handler and the CLI share it.

func (h *Handler) listRunners(w http.ResponseWriter, r *http.Request) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	runners, err := h.Svc.ListRunners(r.Context(), p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runners": runners})
}

func (h *Handler) mintRunnerToken(w http.ResponseWriter, r *http.Request) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	// Gate before decoding: an anonymous caller must get a real 401, not a
	// body-validation 400 (the credential boundary runs first).
	if err := requireHostAdmin(p); err != nil {
		writeErr(w, err)
		return
	}
	// The repository policy is the ADMIN's, set here exactly as the page's mint
	// form sets it: {name?, allow_all, repos}. Neither field is accepted on the
	// runner protocol, so a runner cannot widen its own tenancy.
	var in MintInput
	if !decodeStrict(w, r, 16<<10, map[string]bool{"name": true, "allow_all": true, "repos": true, "group": true}, &in) {
		return
	}
	tok, err := h.Svc.MintRegistrationToken(r.Context(), p, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, tok)
}

func (h *Handler) revokeRunner(w http.ResponseWriter, r *http.Request, id string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.Svc.RevokeRunner(r.Context(), id, p); err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// setRunnerPolicy answers POST /api/v1/admin/runners/{id}/policy: the admin
// JSON lane for the fleet's one non-mint tenancy write. Body shape is the
// mint's ({allow_all, repos}); the service validates, CASes only the policy and
// audits it.
func (h *Handler) setRunnerPolicy(w http.ResponseWriter, r *http.Request, id string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	// Gate before decoding: an anonymous caller gets a real 401, never a body
	// 400 (the credential boundary runs first).
	if err := requireHostAdmin(p); err != nil {
		writeErr(w, err)
		return
	}
	var in RunnerPolicyInput
	if !decodeStrict(w, r, 16<<10, map[string]bool{"allow_all": true, "repos": true}, &in) {
		return
	}
	pol, err := h.Svc.SetRunnerPolicy(r.Context(), id, RunnerPolicy{AllowAll: in.AllowAll, Repos: in.Repos}, p.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": strings.ToLower(strings.TrimSpace(id)), "policy": pol})
}

func (h *Handler) listSecrets(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	secrets, err := h.Svc.ListSecrets(r.Context(), repo, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"secrets": secrets,
		"enabled": h.Svc.SecretsEnabled(),
	})
}

// putSecret takes {value} and never echoes it (write-only surface).
func (h *Handler) putSecret(w http.ResponseWriter, r *http.Request, repo, name string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in struct {
		Value string `json:"value"`
	}
	if !decodeStrict(w, r, 128<<10, map[string]bool{"value": true}, &in) {
		return
	}
	if err := h.Svc.SetSecret(r.Context(), repo, name, in.Value, p); err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteSecret(w http.ResponseWriter, r *http.Request, repo, name string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.Svc.DeleteSecret(r.Context(), repo, name, p); err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// variables is the repository-variable surface (PAR-035): list names, put a
// value (write-only), delete. Values are never echoed.
func (h *Handler) variables(w http.ResponseWriter, r *http.Request, repo string, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		h.listVariables(w, r, repo)
	case len(rest) == 1 && r.Method == http.MethodPut:
		h.putVariable(w, r, repo, rest[0])
	case len(rest) == 1 && r.Method == http.MethodDelete:
		h.deleteVariable(w, r, repo, rest[0])
	default:
		methodNotAllowed(w, "GET", "PUT", "DELETE")
	}
}

func (h *Handler) listVariables(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	vars, err := h.Svc.ListVariables(r.Context(), repo, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"variables": vars, "enabled": h.Svc.SecretsEnabled()})
}

func (h *Handler) putVariable(w http.ResponseWriter, r *http.Request, repo, name string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in struct {
		Value string `json:"value"`
	}
	if !decodeStrict(w, r, 128<<10, map[string]bool{"value": true}, &in) {
		return
	}
	if err := h.Svc.SetVariable(r.Context(), repo, name, in.Value, p); err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteVariable(w http.ResponseWriter, r *http.Request, repo, name string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.Svc.DeleteVariable(r.Context(), repo, name, p); err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// handleOrgVariables is the organization-variable twin (org owner only).
func (h *Handler) handleOrgVariables(w http.ResponseWriter, r *http.Request, org string, rest []string) bool {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		p, aerr := h.principal(r)
		if aerr != nil {
			writeErr(w, aerr)
			return true
		}
		vars, err := h.Svc.ListOrgVariables(r.Context(), org, p)
		if err != nil {
			writeErr(w, err)
			return true
		}
		writeJSON(w, http.StatusOK, map[string]any{"variables": vars, "enabled": h.Svc.SecretsEnabled()})
	case len(rest) == 1 && r.Method == http.MethodPut:
		p, aerr := h.principal(r)
		if aerr != nil {
			writeErr(w, aerr)
			return true
		}
		var in struct {
			Value string `json:"value"`
		}
		if !decodeStrict(w, r, 128<<10, map[string]bool{"value": true}, &in) {
			return true
		}
		if err := h.Svc.SetOrgVariable(r.Context(), org, rest[0], in.Value, p); err != nil {
			writeErr(w, err)
			return true
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	case len(rest) == 1 && r.Method == http.MethodDelete:
		p, aerr := h.principal(r)
		if aerr != nil {
			writeErr(w, aerr)
			return true
		}
		if err := h.Svc.DeleteOrgVariable(r.Context(), org, rest[0], p); err != nil {
			writeErr(w, err)
			return true
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, "GET", "PUT", "DELETE")
	}
	return true
}

// handleOrgSecrets is the organization-secret surface (PAR-115, org owner
// only): list names and policies, put a value (write-only), delete. A value is
// never echoed, and the repository policy travels with the write.
func (h *Handler) handleOrgSecrets(w http.ResponseWriter, r *http.Request, org string, rest []string) bool {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		p, aerr := h.principal(r)
		if aerr != nil {
			writeErr(w, aerr)
			return true
		}
		secrets, err := h.Svc.ListOrgSecrets(r.Context(), org, p)
		if err != nil {
			writeErr(w, err)
			return true
		}
		writeJSON(w, http.StatusOK, map[string]any{"secrets": secrets, "enabled": h.Svc.SecretsEnabled()})
	case len(rest) == 1 && r.Method == http.MethodPut:
		p, aerr := h.principal(r)
		if aerr != nil {
			writeErr(w, aerr)
			return true
		}
		var in struct {
			Value        string   `json:"value"`
			Visibility   string   `json:"visibility"`
			Repositories []string `json:"repositories"`
		}
		if !decodeStrict(w, r, 128<<10, map[string]bool{"value": true, "visibility": true, "repositories": true}, &in) {
			return true
		}
		scope := OrgSecretScope{Visibility: in.Visibility, Repositories: in.Repositories}
		if err := h.Svc.SetOrgSecret(r.Context(), org, rest[0], in.Value, scope, p); err != nil {
			writeErr(w, err)
			return true
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	case len(rest) == 1 && r.Method == http.MethodDelete:
		p, aerr := h.principal(r)
		if aerr != nil {
			writeErr(w, aerr)
			return true
		}
		if err := h.Svc.DeleteOrgSecret(r.Context(), org, rest[0], p); err != nil {
			writeErr(w, err)
			return true
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, "GET", "PUT", "DELETE")
	}
	return true
}

// cancelRun / rerunRun are the operator verbs (repo write role).
func (h *Handler) cancelRun(w http.ResponseWriter, r *http.Request, repo, runID string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	run, err := h.Svc.CancelRun(r.Context(), repo, runID, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

// cancelJob is POST …/threads/{run}/jobs/{job}/cancel (PAR-195). The optional
// body {"force":true} makes the job terminal at once; without it a running
// attempt receives the normal cancel signal.
func (h *Handler) cancelJob(w http.ResponseWriter, r *http.Request, repo, runID, jobID string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in struct {
		Force bool `json:"force"`
	}
	if r.ContentLength != 0 {
		if !decodeStrict(w, r, 4<<10, map[string]bool{"force": true}, &in) {
			return
		}
	}
	job, err := h.Svc.CancelJob(r.Context(), repo, runID, jobID, in.Force, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

// rerunRun and rerunFailedRun are the operator verbs (repo write role):
// `rerun` restarts every job, `rerun-failed` only the failed jobs and their
// dependents (PAR-059, docs/threads.md §9.1).
func (h *Handler) rerunRun(w http.ResponseWriter, r *http.Request, repo, runID string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	run, err := h.Svc.RerunRun(r.Context(), repo, runID, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"run": run})
}

func (h *Handler) rerunFailedRun(w http.ResponseWriter, r *http.Request, repo, runID string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	run, err := h.Svc.RerunFailedRun(r.Context(), repo, runID, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"run": run})
}

// approvalRun is POST …/threads/{run}/approve|deny: the first-time-contributor
// gate's decision (15 §5.2). Maintain-role only; the service is the gate.
func (h *Handler) approvalRun(w http.ResponseWriter, r *http.Request, repo, runID string, approve bool) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var (
		run *Run
		err error
	)
	if approve {
		run, err = h.Svc.ApproveRun(r.Context(), repo, runID, p)
	} else {
		run, err = h.Svc.DenyRun(r.Context(), repo, runID, p)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "approval": Approval(run)})
}

// marshalName is a tiny helper for the SSE frames (the writer marshals too).
func marshalName(v any) string {
	// The SSE payloads are small JSON maps of strings/ints; encoding cannot
	// fail, and a nil body simply carries no data.
	b, _ := json.Marshal(v)
	return string(b)
}

// checkoutBase derives the runner-facing origin: the configured
// server.public_url when set (the operator's declaration of an address job
// containers can reach), otherwise the protocol request's origin. See
// checkoutBase(r) for the request-derived rule. A public value that is not an
// absolute http(s) URL is ignored — a malformed setting must not poison the
// checkout.
func (h *Handler) checkoutBase(r *http.Request) string {
	if h != nil && h.Svc != nil {
		if base := strings.TrimSpace(h.Svc.CheckoutBaseURL); base != "" {
			if u, err := neturl.Parse(base); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
				return strings.TrimRight(base, "/")
			}
		}
	}
	return checkoutBase(r)
}

// checkoutBase derives the runner-facing origin from the protocol request, so
// the job container clones from the address the runner itself used (a reverse
// proxy's X-Forwarded-Proto wins; the host keeps its port). The runner rewrites
// a loopback host to host.docker.internal inside a container.
func checkoutBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); proto != "" {
		if i := strings.IndexByte(proto, ','); i >= 0 {
			proto = strings.TrimSpace(proto[:i])
		}
		proto = strings.ToLower(proto)
		if proto == "http" || proto == "https" {
			scheme = proto
		}
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}
