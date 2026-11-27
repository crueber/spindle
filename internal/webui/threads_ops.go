package webui

// Wave-4 Threads page verbs (PAR-154/PAR-155/PAR-157/PAR-196/PAR-197): the
// schedule switches, the pause toggle, the watch toggle, the inbox and the
// validate editor. Each handler calls the same seam the JSON API uses, so the
// page is an affordance and the service is the gate.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/crueber/spindle/internal/threads"
)

// threadsSchedule answers POST …/threads/schedule: enable, disable or run-now
// one cron entry. The service is maintain+/write+ and audited.
func (s *Server) threadsSchedule(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.collabError(w, r, http.StatusBadRequest, "invalid form")
		return
	}
	action := strings.TrimSpace(r.PostForm.Get("action"))
	id := threads.ScheduleEntryID{
		Path: strings.TrimSpace(r.PostForm.Get("path")), Cron: strings.TrimSpace(r.PostForm.Get("cron")),
		Timezone: strings.TrimSpace(r.PostForm.Get("timezone")),
	}
	var err error
	notice := ""
	switch action {
	case "enable", "disable":
		if threadsDeps.SetSchedule == nil {
			s.collabError(w, r, http.StatusServiceUnavailable, "threads service not wired")
			return
		}
		_, err = threadsDeps.SetSchedule(r.Context(), rc.Full, id, action == "enable", p)
		if err == nil {
			notice = "schedule " + action + "d: " + id.Path + " (" + id.Cron + ")"
		}
	case "run-now":
		if threadsDeps.RunScheduleNow == nil {
			s.collabError(w, r, http.StatusServiceUnavailable, "threads service not wired")
			return
		}
		var runs []*threads.Run
		runs, err = threadsDeps.RunScheduleNow(r.Context(), rc.Full, id, p)
		if err == nil && len(runs) > 0 {
			http.Redirect(w, r, "/"+rc.Full+"/threads/"+url.PathEscape(runs[0].ID)+"?notice="+url.QueryEscape("schedule run started"), http.StatusSeeOther)
			return
		}
		if err == nil {
			err = fmt.Errorf("schedule run created no run")
		}
	default:
		s.collabError(w, r, http.StatusBadRequest, "unknown schedule action")
		return
	}
	if err != nil {
		http.Redirect(w, r, "/"+rc.Full+"/threads?schedule_error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/"+rc.Full+"/threads?schedule_notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

// threadsPause answers POST …/threads/pause (PAR-157).
func (s *Server) threadsPause(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.collabError(w, r, http.StatusBadRequest, "invalid form")
		return
	}
	if threadsDeps.SetPaused == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "threads service not wired")
		return
	}
	paused := r.PostForm.Get("paused") == "1" || r.PostForm.Get("paused") == "true"
	if _, err := threadsDeps.SetPaused(r.Context(), rc.Full, paused, p); err != nil {
		http.Redirect(w, r, "/"+rc.Full+"/threads?notice="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	msg := "CI resumed"
	if paused {
		msg = "CI paused: in-flight jobs finish, nothing new is claimed"
	}
	http.Redirect(w, r, "/"+rc.Full+"/threads?notice="+url.QueryEscape(msg), http.StatusSeeOther)
}

// threadsSubscribe answers POST …/threads/subscribe (PAR-197): the caller
// watches or stops watching this repository's failed runs.
func (s *Server) threadsSubscribe(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.collabError(w, r, http.StatusBadRequest, "invalid form")
		return
	}
	if threadsDeps.SetSubscription == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "threads service not wired")
		return
	}
	subscribed := r.PostForm.Get("subscribed") == "1"
	if _, err := threadsDeps.SetSubscription(r.Context(), rc.Full, subscribed, nil, p); err != nil {
		http.Redirect(w, r, "/"+rc.Full+"/threads?notice="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	msg := "unsubscribed from run notifications"
	if subscribed {
		msg = "subscribed: a failed run notifies you once, with its link"
	}
	http.Redirect(w, r, "/"+rc.Full+"/threads?notice="+url.QueryEscape(msg), http.StatusSeeOther)
}

// threadsNotificationsRead marks the caller's inbox read.
func (s *Server) threadsNotificationsRead(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	if threadsDeps.MarkRead != nil {
		_ = threadsDeps.MarkRead(r.Context(), p)
	}
	http.Redirect(w, r, "/"+rc.Full+"/threads", http.StatusSeeOther)
}

// threadsValidate answers POST …/threads/validate (PAR-196): the editor pane's
// validate button. The result travels back as a compact JSON query value so the
// page renders the key-level errors beside the editor.
func (s *Server) threadsValidate(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.collabError(w, r, http.StatusBadRequest, "invalid form")
		return
	}
	if threadsDeps.Validate == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "threads service not wired")
		return
	}
	file := strings.TrimSpace(r.PostForm.Get("file"))
	src := r.PostForm.Get("source")
	res, err := threadsDeps.Validate(r.Context(), rc.Full, p, file, []byte(src))
	if err != nil {
		http.Redirect(w, r, "/"+rc.Full+"/threads?notice="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	blob, _ := json.Marshal(res)
	http.Redirect(w, r, "/"+rc.Full+"/threads?validate="+url.QueryEscape(string(blob))+"#threads-editor", http.StatusSeeOther)
}
