package threads

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The runner protocol (15 §3): JSON over HTTPS, authenticated by the protocol's
// own tokens (never a session cookie), versioned by X-Spindle-Runner: 1 on every
// response. Paths are top-level because a runner is infrastructure, not a user:
//
//	POST /api/v1/threads/runners/register         registration token → runner token
//	POST /api/v1/threads/runners/declare          version + labels (liveness)
//	POST /api/v1/threads/runners/fetch            long-poll (≤ 30 s), 204 when idle
//	POST /api/v1/threads/jobs/{ref}/attempt/{n}/start|log|complete|cancel
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
		}
		writePlain(w, http.StatusNotFound, "unknown runner endpoint")
		return true
	case len(rest) == 5 && rest[0] == "jobs" && rest[2] == "attempt":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.attemptOp(w, r, rest[1], rest[3], rest[4])
		return true
	}
	return false
}

// runnerRegister consumes a registration token once.
func (h *Handler) runnerRegister(w http.ResponseWriter, r *http.Request) {
	var in RegisterInput
	if !decodeStrict(w, r, maxProtocolBody, map[string]bool{"token": true, "name": true, "labels": true}, &in) {
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
	if !decodeStrict(w, r, 16<<10, map[string]bool{"version": true, "labels": true}, &in) {
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
	job, ferr := h.Svc.FetchJobFrom(r.Context(), auth, wait, checkoutBase(r))
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

// attemptOp dispatches one attempt-scoped operation.
func (h *Handler) attemptOp(w http.ResponseWriter, r *http.Request, ref, nRaw, op string) {
	attempt, nerr := attemptPathNumber(nRaw)
	if nerr != nil {
		writeErr(w, nerr)
		return
	}
	switch op {
	case "start":
		h.attemptStart(w, r, ref, attempt)
	case "log":
		h.attemptLog(w, r, ref, attempt)
	case "complete":
		h.attemptComplete(w, r, ref, attempt)
	case "cancel":
		h.attemptCancel(w, r, ref, attempt)
	default:
		writePlain(w, http.StatusNotFound, "unknown attempt operation")
	}
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
	if !decodeStrict(w, r, maxProtocolBody, map[string]bool{"conclusion": true, "steps": true}, &in) {
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

// --- repo-scoped admin surface ----------------------------------------------

// runners / runner tokens / secrets are repo admin operations. They live on the
// repo lanes (a page and its API twin share the gate: admin, both lanes).

func (h *Handler) listRunners(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	runners, err := h.Svc.ListRunners(r.Context(), repo, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runners": runners})
}

func (h *Handler) mintRunnerToken(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	tok, err := h.Svc.MintRegistrationToken(r.Context(), repo, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, tok)
}

func (h *Handler) revokeRunner(w http.ResponseWriter, r *http.Request, repo, id string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.Svc.RevokeRunner(r.Context(), repo, id, p); err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
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
