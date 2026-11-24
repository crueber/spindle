package identity

import (
	"net/http"
	"strings"

	"github.com/crueber/spindle/internal/wal"
)

// transferBody is the POST /{owner}/{repo}/api/transfer response: the started
// (or joined) `repo-transfer` task. `task` is the live record; `task_id` is the
// key for the attach stream (GET …/api/tasks/{id} with
// Accept: text/event-stream) and for polling. `joined` reports that a transfer
// of the same source was already running and this request reused it.
type transferBody struct {
	Owner  string          `json:"owner"`
	Repo   string          `json:"repo"`
	TaskID string          `json:"task_id"`
	Joined bool            `json:"joined"`
	Task   *wal.TaskRecord `json:"task"`
}

// routeTransfer answers POST /{owner}/{repo}/api/transfer (both lanes): a
// repository move, started as the `repo-transfer` task (PLAN-V2 §T.1). The
// caller must hold repo admin on the source (CheckRole admin: binding,
// source-org ownership, or host admin) and pass CheckCreateOwner admission on
// the destination (the #346 rule, verbatim). Success is 202 with the task
// record — the move is narrated and attachable, and never reported done before
// the bucket has it committed (the task's terminal record is the only "done").
//
// Anonymous callers get a real 401 (CheckRole maps anonymous-denied to
// 401 — law 9); unknown or foreign destination orgs 403 via CheckCreateOwner
// (only the caller's own namespace and host admins bypass); an occupied
// destination 409; an unknown source 404.
func (h *Handler) routeTransfer(w http.ResponseWriter, r *http.Request, owner, repo string) bool {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return true
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return true
	}
	if cerr := h.Svc.CheckRole(r.Context(), owner, repo, p, RoleAdmin); cerr != nil {
		writeErr(w, cerr)
		return true
	}
	var body struct {
		Owner string `json:"owner"`
		Repo  string `json:"repo"`
	}
	if !readBodyJSON(w, r, 64<<10, &body) {
		return true
	}
	dstOwner := strings.TrimSpace(body.Owner)
	dstRepo := strings.TrimSpace(body.Repo)
	if dstOwner == "" {
		writePlain(w, http.StatusBadRequest, "destination owner required")
		return true
	}
	if dstRepo == "" {
		dstRepo = repo
	}
	if cerr := h.Svc.CheckCreateOwner(r.Context(), dstOwner, p); cerr != nil {
		writeErr(w, cerr)
		return true
	}
	rec, joined, err := h.Svc.BeginTransfer(WithActor(r.Context(), p.Name), TransferRequest{
		SrcOwner: owner, SrcRepo: repo, DstOwner: dstOwner, DstRepo: dstRepo,
	})
	if err != nil {
		writeErr(w, err)
		return true
	}
	writeCached(w, r, ccNoStore, "", http.StatusAccepted, transferBody{
		Owner: dstOwner, Repo: dstRepo, TaskID: rec.ID, Joined: joined, Task: rec,
	})
	return true
}
