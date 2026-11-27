package webui

// Workflow status badge (PAR-193):
//
//	GET /{owner}/{repo}/threads/badge.svg?workflow=<name-or-path>&branch=<branch>
//
// The badge reflects the latest run of that workflow on that branch (the
// repository's default branch when `branch` is omitted). It is an SVG served
// to anonymous readers of a PUBLIC repository only — the same read gate the
// Threads page uses — and the state is written as text, never colour alone
// (a hue-blind reader sees "passing"/"failing"/"pending"/"no runs").

import (
	"html"
	"net/http"
	"strings"

	"github.com/crueber/spindle/internal/threads"
)

func (s *Server) threadBadge(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	workflow := strings.TrimSpace(r.URL.Query().Get("workflow"))
	if workflow == "" {
		s.collabError(w, r, http.StatusBadRequest, "badge: workflow is required")
		return
	}
	branch := strings.TrimSpace(r.URL.Query().Get("branch"))
	if branch == "" {
		branch = strings.TrimSpace(rc.HeadLabel)
		if looksLikeSHA(branch) {
			branch = ""
		}
	}
	state, label := "none", "no runs"
	if threadsDeps.List != nil {
		page, err := threadsDeps.List(r.Context(), rc.Owner, rc.Name, p, "", 1, threads.RunFilter{
			Workflow: workflow, Branch: branch,
		})
		if err != nil {
			s.collabError(w, r, collabErrStatus(err), err.Error())
			return
		}
		if len(page.Runs) > 0 {
			state, label = badgeState(page.Runs[0].State)
		}
	}
	fill := "#6b7280"
	switch state {
	case "success":
		fill = "#047857"
	case "failure":
		fill = "#b91c1c"
	case "pending":
		fill = "#b45309"
	}
	subject := "threads" + func() string {
		if branch != "" {
			return " · " + branch
		}
		return ""
	}()
	left := html.EscapeString(subject)
	right := html.EscapeString(label)
	svg := `<svg xmlns="http://www.w3.org/2000/svg" width="210" height="20" role="img" aria-label="` +
		left + ": " + right + `">` +
		`<title>` + left + ": " + right + `</title>` +
		`<rect width="110" height="20" fill="#3f3f46"/>` +
		`<rect x="110" width="100" height="20" fill="` + fill + `"/>` +
		`<g fill="#fff" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">` +
		`<text x="8" y="14">` + left + `</text>` +
		`<text x="118" y="14">` + right + `</text></g></svg>`
	hdr := w.Header()
	hdr.Set("Content-Type", "image/svg+xml; charset=utf-8")
	hdr.Set("Cache-Control", "public, max-age=60")
	hdr.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(svg))
}

// badgeState maps a run state onto the badge's word and colour bucket. The
// word is the state signal; colour is decoration.
func badgeState(state string) (string, string) {
	switch state {
	case threads.RunSuccess:
		return "success", "passing"
	case threads.RunFailure:
		return "failure", "failing"
	case threads.RunCancelled:
		return "failure", "cancelled"
	case threads.RunSkipped:
		return "none", "skipped"
	case threads.RunRunning, threads.RunPendingApproval:
		return "pending", "running"
	default:
		return "pending", "queued"
	}
}
