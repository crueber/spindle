// WIRING — moderation M2 page actions (PLAN-V2 §M.2/M.4).
//
//	POST /{owner}/{repo}/issues/{num}/moderation/{action}   hide|unhide|lock|unlock
//	POST /{owner}/{repo}/pull/{num}/moderation/{action}     hide|unhide|lock|unlock
//
// The handlers are thin: they enforce the page's own floor (so a rejected
// affordance cannot be replayed), enforce same-origin through collabWrite,
// and call the ONE moderation verb — the service re-checks the floor, so the
// page and the JSON API can never disagree. Nothing here decides what
// "hidden" means; the projection rule lives in internal/moderation and is
// applied by the feature read verbs that feed the templates.
package webui

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
)

// threadHideTarget is the plan's target key for one issue/PR conversation
// event body (mirrors moderation.ThreadTarget; the page only formats it).
func threadHideTarget(num, seq int) string {
	return fmt.Sprintf("thread:%06x:event:%012x", num, seq)
}

// reviewHideTarget is the target key for one review body.
func reviewHideTarget(num, seq int) string {
	return fmt.Sprintf("review:%06x:%012x", num, seq)
}

// reviewThreadHideTarget is the target key for one review-thread comment.
func reviewThreadHideTarget(num int, tid string, seq int) string {
	return fmt.Sprintf("review_thread:%06x:%s:%012x", num, tid, seq)
}

// moderationFloor is the page-side mirror of the service floor: hide/unhide
// need triage, lock/unlock need maintain (PLAN-V2 §M.5).
func moderationFloor(action string) identity.Role {
	switch action {
	case "lock", "unlock":
		return identity.RoleMaintain
	default:
		return identity.RoleTriage
	}
}

// moderationLock composes the M2 moderator lock over the closed/merged
// conversation lock. The banner names the mode and the reason; existing
// content stays readable. `mayWrite` is the viewer's write+ standing: a
// collaborators-only lock still refuses them nothing, so the composer stays
// live and only the banner changes (mode "all" refuses everyone, unchanged).
func moderationLock(base lockState, locked bool, mode, reason string, mayWrite bool) lockState {
	if !locked {
		return base
	}
	if strings.TrimSpace(reason) == "" {
		reason = "no reason given"
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "collaborators" {
		return lockState{
			Locked:            !mayWrite,
			Reason:            "Locked to collaborators: " + reason,
			Moderated:         true,
			Mode:              "collaborators",
			CollaboratorsOnly: true,
		}
	}
	return lockState{Locked: true, Reason: "Locked by a moderator: " + reason, Moderated: true, Mode: "all"}
}

// moderate runs one moderation page action and re-renders the page with an
// inline error on failure (the collab write-back convention).
func (s *Server) moderate(w http.ResponseWriter, r *http.Request, kind, numStr, action string) {
	num, ok := issueNum(numStr)
	if !ok || num < 1 {
		s.collabError(w, r, http.StatusNotFound, "unknown conversation")
		return
	}
	switch action {
	case "hide", "unhide", "lock", "unlock":
	default:
		s.collabError(w, r, http.StatusNotFound, "unknown moderation action")
		return
	}
	rc, p, ok := s.collabWrite(w, r, moderationFloor(action), "Moderate a conversation")
	if !ok {
		return
	}
	target := strings.TrimSpace(r.PostFormValue("target"))
	reason := strings.TrimSpace(r.PostFormValue("reason"))

	var err error
	switch action {
	case "hide":
		if collabDeps.ModHide == nil {
			s.collabError(w, r, http.StatusServiceUnavailable, "moderation service not wired")
			return
		}
		_, _, err = collabDeps.ModHide(r.Context(), rc.Owner, rc.Name, p, target, reason)
	case "unhide":
		if collabDeps.ModUnhide == nil {
			s.collabError(w, r, http.StatusServiceUnavailable, "moderation service not wired")
			return
		}
		_, _, err = collabDeps.ModUnhide(r.Context(), rc.Owner, rc.Name, p, target)
	case "lock":
		if collabDeps.ModLock == nil {
			s.collabError(w, r, http.StatusServiceUnavailable, "moderation service not wired")
			return
		}
		// The mode select is part of the existing lock control; an absent
		// field (or an old client) means today's semantics: "all".
		mode := strings.TrimSpace(r.PostFormValue("mode"))
		if mode == "" {
			mode = "all"
		}
		_, _, err = collabDeps.ModLock(r.Context(), rc.Owner, rc.Name, num, p, reason, mode)
	case "unlock":
		if collabDeps.ModUnlock == nil {
			s.collabError(w, r, http.StatusServiceUnavailable, "moderation service not wired")
			return
		}
		_, _, err = collabDeps.ModUnlock(r.Context(), rc.Owner, rc.Name, num, p)
	}
	if err != nil {
		if kind == "pull" {
			s.pullWriteBack(w, r, rc, p, num, err)
			return
		}
		s.issueWriteBack(w, r, rc, p, num, err)
		return
	}
	anchor := ""
	if action == "lock" || action == "unlock" {
		anchor = "#conversation"
	}
	if kind == "pull" {
		redirectPage(w, r, "/"+rc.Full+"/pull/"+strconv.Itoa(num)+anchor)
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/issues/"+strconv.Itoa(num)+anchor)
}

// issueModerate serves one issue moderation action.
func (s *Server) issueModerate(w http.ResponseWriter, r *http.Request) {
	s.moderate(w, r, "issue", r.PathValue("num"), r.PathValue("action"))
}

// pullModerate serves one PR moderation action.
func (s *Server) pullModerate(w http.ResponseWriter, r *http.Request) {
	s.moderate(w, r, "pull", r.PathValue("num"), r.PathValue("action"))
}

// --- M3 reports (PLAN-V2 §M.3/M.4) ------------------------------------------

// ModerationReportInput is the page's view of one filing request. The page
// builds no target key: the service validates the structured fields (the
// one-grammar rule); Num comes from the conversation path.
type ModerationReportInput struct {
	Kind      string
	Num       int
	Seq       int
	TID       string
	Reason    string
	Note      string
	Principal string
}

// issueReport serves POST /{owner}/{repo}/issues/{num}/report.
func (s *Server) issueReport(w http.ResponseWriter, r *http.Request) {
	s.fileReport(w, r, "issue")
}

// pullReport serves POST /{owner}/{repo}/pull/{num}/report.
func (s *Server) pullReport(w http.ResponseWriter, r *http.Request) {
	s.fileReport(w, r, "pull")
}

// fileReport is the one page write for the report affordance. The floor is
// "authenticated with read": collabWrite already proved the viewer may see the
// repo, the service re-checks the read floor, and anonymous is gated to the
// login interstitial (never a silent no-op).
func (s *Server) fileReport(w http.ResponseWriter, r *http.Request, kind string) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown conversation")
		return
	}
	rc, p, ok := s.collabWrite(w, r, "", "File a report")
	if !ok {
		return
	}
	if collabDeps.ModReport == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "moderation service not wired")
		return
	}
	targetKind := strings.TrimSpace(r.PostFormValue("kind"))
	switch targetKind {
	case "thread", "review", "review_thread":
	default:
		s.reportWriteBack(w, r, rc, p, num, kind, fmt.Errorf("bad report target"))
		return
	}
	seq, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("seq")))
	in := ModerationReportInput{
		Kind:   targetKind,
		Num:    num,
		Seq:    seq,
		TID:    strings.TrimSpace(r.PostFormValue("tid")),
		Reason: strings.TrimSpace(r.PostFormValue("reason")),
		Note:   strings.TrimSpace(r.PostFormValue("note")),
	}
	id, err := collabDeps.ModReport(r.Context(), rc.Owner, rc.Name, p, in)
	if err != nil {
		s.reportWriteBack(w, r, rc, p, num, kind, err)
		return
	}
	anchor := "#event-" + strconv.Itoa(seq)
	if targetKind == "review" {
		anchor = "#reviews"
	}
	if kind == "pull" {
		redirectPage(w, r, "/"+rc.Full+"/pull/"+strconv.Itoa(num)+"?reported="+strconv.Itoa(id)+anchor)
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/issues/"+strconv.Itoa(num)+"?reported="+strconv.Itoa(id)+anchor)
}

// reportWriteBack re-renders the conversation with the service's refusal
// inline (the collab write-back convention; 401/403/404 stay the error page).
func (s *Server) reportWriteBack(w http.ResponseWriter, r *http.Request, rc collabRepo, p auth.Principal, num int, kind string, err error) {
	status := collabErrStatus(err)
	if status < 400 {
		status = http.StatusBadRequest
	}
	if status == http.StatusNotFound || status == http.StatusUnauthorized || status == http.StatusForbidden {
		s.collabError(w, r, status, err.Error())
		return
	}
	if kind == "pull" {
		s.renderPull(w, r, rc, p, num, status, err.Error(), "")
		return
	}
	s.renderIssue(w, r, rc, p, num, status, err.Error(), "")
}

// reportedNotice renders the filing confirmation (?reported=<id>) as the
// page's notice. It never echoes the report's content — just the id.
func reportedNotice(r *http.Request) string {
	id, err := strconv.Atoi(r.URL.Query().Get("reported"))
	if err != nil || id < 1 {
		return ""
	}
	return fmt.Sprintf("Report #%d filed — a repository admin will review it.", id)
}
