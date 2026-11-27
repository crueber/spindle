package threads

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server/auth"
)

// Wire conventions (07 §2, the sibling feature packages' shape): JSON success,
// plain-text errors, arrays [] never null, RFC 3339 UTC, per-segment decoding,
// no-store on every GET (run state mutates in place), both lanes everywhere.
// Anonymous-denied reads get a real 401 with WWW-Authenticate: Bearer.

// ExposedTemplates lists the discovery endpoints[] entries this surface serves
// (14 §14.12 lane rule; registered from composition via api.RegisterExposed —
// the checks/import precedent, law 12). One entry per distinct path shape.
var ExposedTemplates = []string{
	"/{owner}/{repo}/api/threads",
	"/{owner}/{repo}/api/threads/workflows",
	"/{owner}/{repo}/api/threads/dispatches",
	"/{owner}/{repo}/api/threads/settings",
	"/{owner}/{repo}/api/threads/schedules",
	"/{owner}/{repo}/api/threads/schedules/enable",
	"/{owner}/{repo}/api/threads/schedules/run-now",
	"/{owner}/{repo}/api/threads/pause",
	"/{owner}/{repo}/api/threads/queue",
	"/{owner}/{repo}/api/threads/notifications",
	"/{owner}/{repo}/api/threads/notifications/read",
	"/{owner}/{repo}/api/threads/subscription",
	"/{owner}/{repo}/api/threads/validate",
	"/{owner}/{repo}/api/threads/{run}",
	"/{owner}/{repo}/api/threads/{run}/cancel",
	"/{owner}/{repo}/api/threads/{run}/rerun",
	"/{owner}/{repo}/api/threads/{run}/rerun-failed",
	"/{owner}/{repo}/api/threads/{run}/approve",
	"/{owner}/{repo}/api/threads/{run}/deny",
	"/{owner}/{repo}/api/threads/{run}/jobs/{job}/log",
	"/{owner}/{repo}/api/threads/{run}/jobs/{job}/cancel",
	"/{owner}/{repo}/api/threads/{run}/artifacts",
	"/{owner}/{repo}/api/threads/{run}/artifacts/{name}",
	"/{owner}/{repo}/api/threads/caches",
	"/{owner}/{repo}/api/threads/caches/{id}",
	"/{owner}/{repo}/api/threads/secrets",
	"/{owner}/{repo}/api/threads/secrets/{name}",
	"/{owner}/{repo}/api/threads/variables",
	"/{owner}/{repo}/api/threads/variables/{name}",
	"/api/v1/orgs/{org}/threads/variables",
	"/api/v1/orgs/{org}/threads/variables/{name}",
	"/{owner}/{repo}/api/threads/environments",
	"/{owner}/{repo}/api/threads/environments/{name}",
	"/{owner}/{repo}/api/threads/environments/{name}/secrets",
	"/{owner}/{repo}/api/threads/environments/{name}/secrets/{secret}",
	"/{owner}/{repo}/api/threads/environments/{name}/variables",
	"/{owner}/{repo}/api/threads/environments/{name}/variables/{variable}",
	"/{owner}/{repo}/api/threads/deployments",
	"/{owner}/{repo}/api/threads/deployments/{id}",
	"/{owner}/{repo}/api/threads/deployments/{id}/statuses",
	"/api/v1/admin/runners",
	"/api/v1/admin/runners/tokens",
	"/api/v1/admin/runners/{id}",
	"/api/v1/admin/runners/{id}/policy",
	"/api/v1/admin/runner-groups",
	"/api/v1/admin/runner-groups/{id}",
	"/api/v1/threads/runners/register",
	"/api/v1/threads/runners/declare",
	"/api/v1/threads/runners/fetch",
	"/api/v1/threads/runners/drain",
	"/api/v1/threads/runners/resume",
	"/api/v1/threads/runners/rotate",
	"/api/v1/threads/runners/deregister",
	"/api/v1/threads/jobs/{ref}/attempt/{n}/start",
	"/api/v1/threads/jobs/{ref}/attempt/{n}/log",
	"/api/v1/threads/jobs/{ref}/attempt/{n}/complete",
	"/api/v1/threads/jobs/{ref}/attempt/{n}/cancel",
	"/api/v1/threads/jobs/{ref}/attempt/{n}/id-token",
	"/api/v1/threads/jobs/{ref}/attempt/{n}/artifacts",
	"/api/v1/threads/jobs/{ref}/attempt/{n}/artifacts/{name}",
	"/api/v1/threads/jobs/{ref}/attempt/{n}/artifacts/{name}/blobs/{index}",
	"/api/v1/threads/jobs/{ref}/attempt/{n}/artifacts/{name}/prepare",
	"/api/v1/threads/jobs/{ref}/attempt/{n}/cache/lookup",
	"/api/v1/threads/jobs/{ref}/attempt/{n}/cache/save",
	"/api/v1/threads/jobs/{ref}/attempt/{n}/cache/blobs/{sha}",
}

// Authenticator resolves the request principal through Seam 2 (the server
// chain, injected by composition). Nil falls back to anonymous.
type Authenticator func(r *http.Request) (auth.Principal, *auth.AuthError)

// Handler is the Seam 1 surface: the threads JSON API on both lanes.
// Composition chains it in front of the core mux; Handle reports false for
// non-threads paths so the core mux answers.
type Handler struct {
	Svc  *Service
	Auth Authenticator
}

func (h *Handler) principal(r *http.Request) (auth.Principal, *auth.AuthError) {
	if h.Auth != nil {
		return h.Auth(r)
	}
	return auth.Anonymous(), nil
}

// Handle answers one request; false when the path is not a threads route.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) bool {
	segs := splitPath(r)
	if len(segs) == 0 {
		return false
	}
	// The OIDC discovery/JWKS documents are top-level and public: a provider
	// fetches them before it ever sees a job token. Unknown /.well-known paths
	// fall through to the core mux's 404.
	if segs[0] == ".well-known" {
		return h.handleWellKnown(w, r, segs)
	}
	// The runner protocol is top-level (/api/v1/threads/…): runners are
	// infrastructure, not users, so their paths are not repo-scoped.
	if segs[0] == "api" || segs[0] == "api-browser" {
		// Organization variables (PAR-035) are owner-scoped, not repo-scoped:
		// /api[-browser]/v1/orgs/{org}/threads/variables[/{name}].
		if len(segs) >= 6 && segs[1] == "v1" && segs[2] == "orgs" && segs[4] == "threads" && segs[5] == "variables" {
			return h.handleOrgVariables(w, r, segs[3], segs[6:])
		}
		if len(segs) >= 4 && segs[1] == "v1" && segs[2] == "admin" && segs[3] == "runner-groups" {
			return h.runnerGroups(w, r, segs[4:])
		}
		if len(segs) >= 4 && segs[1] == "v1" && segs[2] == "admin" && segs[3] == "runners" {
			// The instance runner fleet is a HOST-ADMIN surface, not a repo
			// lane: /api[-browser]/v1/admin/runners[/*] (both lanes, like every
			// other JSON twin). The runner PROTOCOL stays machine-only: only
			// /api/v1 handles threads/runners/{register,declare,fetch}.
			return h.handleAdminRunners(w, r, segs[4:])
		}
		return h.handleTop(w, r, segs)
	}
	if len(segs) >= 4 && (segs[2] == "api" || segs[2] == "api-browser") {
		owner, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
		if _, err := git.ParseRepoId(owner + "/" + repo); err != nil {
			return false
		}
		return h.handleRepo(w, r, owner+"/"+repo, segs[3:])
	}
	return false
}

// ServeHTTP answers threads routes and 404s otherwise (httptest surface).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.Handle(w, r) {
		writePlain(w, http.StatusNotFound, "not found")
	}
}

// splitPath splits the escaped path and decodes each segment separately.
func splitPath(r *http.Request) []string {
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		out = append(out, decodeSegment(s))
	}
	return out
}

// decodeSegment decodes one path segment; an undecodable segment survives
// verbatim (fail closed downstream).
func decodeSegment(s string) string {
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	return s
}

// --- writers ---------------------------------------------------------------

func writePlain(w http.ResponseWriter, status int, msg string) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Del("ETag")
	if status == http.StatusUnauthorized {
		hdr.Set("WWW-Authenticate", `Bearer realm="walgit"`)
	}
	if status == http.StatusServiceUnavailable {
		hdr.Set("Retry-After", "15")
	}
	if status == http.StatusTooManyRequests {
		hdr.Set("Retry-After", "60")
	}
	hdr.Set("Content-Length", strconv.Itoa(len(msg)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}

func writeErr(w http.ResponseWriter, err error) {
	if aerr, ok := err.(*auth.AuthError); ok {
		switch aerr.Kind {
		case auth.ErrForbidden:
			writePlain(w, http.StatusForbidden, aerr.Why)
		case auth.ErrUnavailable:
			writePlain(w, http.StatusServiceUnavailable, aerr.Why)
		default:
			writePlain(w, http.StatusUnauthorized, aerr.Why)
		}
		return
	}
	writePlain(w, statusFor(err), err.Error())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	writeJSONCached(w, status, v, "no-store")
}

// writeJSONCached is writeJSON with an explicit Cache-Control class (the OIDC
// discovery/JWKS documents are the only public, cacheable JSON this package
// serves; everything else mutates in place and stays no-store).
func writeJSONCached(w http.ResponseWriter, status int, v any, cacheControl string) {
	b, err := json.Marshal(v)
	if err != nil {
		writePlain(w, http.StatusInternalServerError, "threads: encode: "+err.Error())
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Cache-Control", cacheControl)
	hdr.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// decodeStrict unmarshals a body after rejecting unknown top-level keys (fail
// closed on writes).
func decodeStrict(w http.ResponseWriter, r *http.Request, limit int64, allowed map[string]bool, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		writePlain(w, http.StatusBadRequest, "unreadable body")
		return false
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	if keys == nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: expected an object")
		return false
	}
	for k := range keys {
		if !allowed[k] {
			writePlain(w, http.StatusBadRequest, "unknown field "+strconv.Quote(k))
			return false
		}
	}
	if err := json.Unmarshal(body, v); err != nil {
		writePlain(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func methodNotAllowed(w http.ResponseWriter, allow ...string) {
	w.Header().Set("Allow", strings.Join(allow, ", "))
	writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
}

// --- routing ---------------------------------------------------------------

func (h *Handler) handleRepo(w http.ResponseWriter, r *http.Request, repo string, rest []string) bool {
	if len(rest) == 0 || rest[0] != "threads" {
		return false
	}
	rest = rest[1:]
	switch {
	case len(rest) == 0:
		switch r.Method {
		case http.MethodGet:
			h.listRuns(w, r, repo)
			return true
		case http.MethodPost:
			h.dispatch(w, r, repo)
			return true
		}
		methodNotAllowed(w, "GET", "POST")
		return true
	case len(rest) == 1 && rest[0] == "workflows":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		h.listWorkflows(w, r, repo)
		return true
	case rest[0] == "caches":
		switch {
		case len(rest) == 1 && r.Method == http.MethodGet:
			h.listCaches(w, r, repo)
		case len(rest) == 2 && r.Method == http.MethodDelete:
			h.deleteCache(w, r, repo, rest[1])
		case len(rest) == 1:
			methodNotAllowed(w, "GET")
		default:
			methodNotAllowed(w, "DELETE")
		}
		return true
	case len(rest) == 1 && rest[0] == "dispatches":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.repositoryDispatch(w, r, repo)
		return true
	case len(rest) == 1 && rest[0] == "settings":
		switch r.Method {
		case http.MethodGet:
			h.getSettings(w, r, repo)
		case http.MethodPut:
			h.putSettings(w, r, repo)
		default:
			methodNotAllowed(w, "GET", "PUT")
		}
		return true
	case len(rest) == 1 && rest[0] == "schedules":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		h.listSchedules(w, r, repo)
		return true
	case len(rest) == 2 && rest[0] == "schedules" && rest[1] == "enable":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.scheduleEnable(w, r, repo)
		return true
	case len(rest) == 2 && rest[0] == "schedules" && rest[1] == "run-now":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.scheduleRunNow(w, r, repo)
		return true
	case len(rest) == 1 && rest[0] == "pause":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.pauseRepo(w, r, repo)
		return true
	case len(rest) == 1 && rest[0] == "queue":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		h.queueSnapshot(w, r, repo)
		return true
	case len(rest) == 1 && rest[0] == "notifications":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		h.listNotifications(w, r)
		return true
	case len(rest) == 2 && rest[0] == "notifications" && rest[1] == "read":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.markNotificationsRead(w, r)
		return true
	case len(rest) == 1 && rest[0] == "subscription":
		switch r.Method {
		case http.MethodGet:
			h.getSubscription(w, r, repo)
		case http.MethodPut:
			h.putSubscription(w, r, repo)
		default:
			methodNotAllowed(w, "GET", "PUT")
		}
		return true
	case len(rest) == 1 && rest[0] == "validate":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.validateWorkflow(w, r, repo)
		return true
	case rest[0] == "secrets":
		switch {
		case len(rest) == 1 && r.Method == http.MethodGet:
			h.listSecrets(w, r, repo)
		case len(rest) == 2 && r.Method == http.MethodPut:
			h.putSecret(w, r, repo, rest[1])
		case len(rest) == 2 && r.Method == http.MethodDelete:
			h.deleteSecret(w, r, repo, rest[1])
		case len(rest) == 1:
			methodNotAllowed(w, "GET")
		default:
			methodNotAllowed(w, "PUT", "DELETE")
		}
		return true
	case rest[0] == "variables":
		switch {
		case len(rest) == 1 && r.Method == http.MethodGet:
			h.listVariables(w, r, repo)
		case len(rest) == 2 && r.Method == http.MethodPut:
			h.putVariable(w, r, repo, rest[1])
		case len(rest) == 2 && r.Method == http.MethodDelete:
			h.deleteVariable(w, r, repo, rest[1])
		case len(rest) == 1:
			methodNotAllowed(w, "GET")
		default:
			methodNotAllowed(w, "PUT", "DELETE")
		}
		return true
	case rest[0] == "environments":
		h.environments(w, r, repo, rest[1:])
		return true
	case rest[0] == "deployments":
		h.deployments(w, r, repo, rest[1:])
		return true
	case len(rest) == 2 && rest[1] == "cancel":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.cancelRun(w, r, repo, rest[0])
		return true
	case len(rest) == 2 && rest[1] == "rerun":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.rerunRun(w, r, repo, rest[0])
		return true
	case len(rest) == 2 && rest[1] == "rerun-failed":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.rerunFailedRun(w, r, repo, rest[0])
		return true
	case len(rest) == 2 && (rest[1] == "approve" || rest[1] == "deny"):
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.approvalRun(w, r, repo, rest[0], rest[1] == "approve")
		return true
	case len(rest) == 4 && rest[1] == "jobs" && rest[3] == "log":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		h.streamLog(w, r, repo, rest[0], rest[2])
		return true
	case len(rest) == 4 && rest[1] == "jobs" && rest[3] == "cancel":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return true
		}
		h.cancelJob(w, r, repo, rest[0], rest[2])
		return true
	case len(rest) == 2 && rest[1] == "artifacts":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		h.listArtifacts(w, r, repo, rest[0])
		return true
	case len(rest) == 3 && rest[1] == "artifacts":
		h.artifactRead(w, r, repo, rest[0], rest[2])
		return true
	case len(rest) == 1:
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return true
		}
		h.getRun(w, r, repo, rest[0])
		return true
	}
	return false
}

// --- handlers --------------------------------------------------------------

func (h *Handler) listRuns(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	q := r.URL.Query()
	n := defaultPageSize
	if v := q.Get("n"); v != "" {
		parsed, cerr := strconv.Atoi(v)
		if cerr != nil || parsed < 1 {
			writePlain(w, http.StatusBadRequest, "invalid n: must be a positive integer")
			return
		}
		if parsed > maxPageSize {
			writePlain(w, http.StatusBadRequest, "invalid n: max 200")
			return
		}
		n = parsed
	}
	state := strings.TrimSpace(q.Get("state"))
	if state == "" {
		state = strings.TrimSpace(q.Get("status")) // documented alias
	}
	f := RunFilter{
		Workflow: strings.TrimSpace(q.Get("workflow")),
		State:    state,
		Actor:    strings.TrimSpace(q.Get("actor")),
		Branch:   strings.TrimSpace(q.Get("branch")),
		Event:    strings.TrimSpace(q.Get("event")),
	}
	page, err := h.Svc.ListRunsFiltered(r.Context(), repo, p, q.Get("after"), n, f)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// listWorkflows answers the per-file parse state at a commit (default: the
// default branch tip) — the API twin of the Threads page's workflow list.
func (h *Handler) listWorkflows(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	list, err := h.Svc.ListWorkflows(r.Context(), repo, r.URL.Query().Get("commit"), p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) getRun(w http.ResponseWriter, r *http.Request, repo, runID string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	view, err := h.Svc.GetRun(r.Context(), repo, runID, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *Handler) dispatch(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Workflow string         `json:"workflow"`
		Ref      string         `json:"ref"`
		Inputs   map[string]any `json:"inputs"`
		Priority int            `json:"priority"`
	}
	if !decodeStrict(w, r, 1<<20, map[string]bool{"workflow": true, "ref": true, "inputs": true, "priority": true}, &body) {
		return
	}
	inputs, ierr := coerceInputs(body.Inputs)
	if ierr != nil {
		writeErr(w, ierr)
		return
	}
	runs, err := h.Svc.DispatchRunWithPriority(r.Context(), repo, body.Ref, body.Workflow, inputs, body.Priority, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"runs": runs})
}

// repositoryDispatch answers POST …/api/threads/dispatches (PAR-018): GitHub's
// `POST /repos/{owner}/{repo}/dispatches` shape, on the spaced Threads lane.
// The write role is the gate (GitHub's contents: write); a job token is refused
// by the service. The answer is 202 with the runs it started, because the
// trigger is asynchronous and "accepted" is the honest status.
func (h *Handler) repositoryDispatch(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		EventType     string         `json:"event_type"`
		ClientPayload map[string]any `json:"client_payload"`
		Ref           string         `json:"ref"`
	}
	if !decodeStrict(w, r, 1<<20, map[string]bool{"event_type": true, "client_payload": true, "ref": true}, &body) {
		return
	}
	runs, err := h.Svc.RepositoryDispatch(r.Context(), repo, body.EventType, body.ClientPayload, body.Ref, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"runs": runs})
}

// getSettings answers GET …/api/threads/settings (read gate).
func (h *Handler) getSettings(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	view, err := h.Svc.GetSettings(r.Context(), repo, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// putSettings answers PUT …/api/threads/settings (maintain+; the service
// re-checks and audits every changed switch).
func (h *Handler) putSettings(w http.ResponseWriter, r *http.Request, repo string) {
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in SettingsInput
	if !decodeStrict(w, r, 64<<10, map[string]bool{
		"send_write_tokens": true, "pull_request_target": true,
		"pr_target_allow_head_checkout": true, "concurrency_cap": true,
	}, &in) {
		return
	}
	view, err := h.Svc.UpdateSettings(r.Context(), repo, in, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// coerceInputs turns the JSON input map into the string map the workflow
// schema is typed over. String, boolean and integral-number values are
// accepted (a JSON-only client should not have to stringify a boolean); any
// other shape is a 400 naming the input.
func coerceInputs(in map[string]any) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(in))
	for name, v := range in {
		switch val := v.(type) {
		case nil:
			// An explicit null is an omitted value (defaults/required apply).
		case string:
			out[name] = val
		case bool:
			out[name] = strconv.FormatBool(val)
		case float64:
			if val == math.Trunc(val) && math.Abs(val) < 1e15 {
				out[name] = strconv.FormatInt(int64(val), 10)
			} else {
				out[name] = strconv.FormatFloat(val, 'f', -1, 64)
			}
		default:
			return nil, fmt.Errorf("%w: input %q must be a string, boolean or number", ErrInvalid, name)
		}
	}
	return out, nil
}
