package threads

// Run notifications (PAR-197).
//
// Threads notifications come in two documented channels:
//
//   - the in-app inbox: a signed-in user subscribes to a repository's runs
//     (all workflows or a named set) and gets ONE durable notification per
//     failed run, with the run link. The record is keyed by the run id, so a
//     re-fold or a retry can never notify twice;
//   - a repository webhook: `notify_webhook` on the Threads settings record, a
//     single POST with the run summary and link. It is off by default.
//
// Email is deliberately not implemented: this instance runs no mailer, so a
// "subscribe by email" control would promise a delivery that cannot happen.
// The named gap is recorded in docs/threads.md §9 rather than an inert form.
//
// Delivery is detached from the fold (an unavailable webhook must never delay a
// run completing) and best effort: a failed notification is logged, not
// retried into a loop. The subscriber's record is the durable fact; the webhook
// is the outbound hint.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/egress"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// NotificationPrefix is the per-user inbox family.
func NotificationPrefix(user string) string {
	return "threads/notifications/" + strings.ToLower(strings.TrimSpace(user)) + "/"
}

// NotificationKey returns the user's record for one run.
func NotificationKey(user, runID string) string {
	return NotificationPrefix(user) + runID + ".json"
}

// Notification is one durable inbox entry.
type Notification struct {
	ID       string `json:"id"`
	User     string `json:"user"`
	Repo     string `json:"repo"`
	RunID    string `json:"run_id"`
	Workflow string `json:"workflow,omitempty"`
	State    string `json:"state"`
	Title    string `json:"title"`
	// Link is the run page path (relative), so a rendered inbox never has to
	// reconstruct the repository route.
	Link      string `json:"link"`
	CreatedAt string `json:"created_at"`
	Read      bool   `json:"read,omitempty"`
}

// SubscriptionsKey returns repos/<o>/<r>/threads/subscriptions.json.
func SubscriptionsKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/threads/subscriptions.json"
}

// Subscription is one user's watched workflow set.
type Subscription struct {
	User string `json:"user"`
	// Workflows are workflow names or paths; "*" watches every workflow. An
	// empty list watches every workflow (the same as "*") — the toggle is the
	// common case, and a user who names workflows opted into more detail.
	Workflows []string `json:"workflows,omitempty"`
	CreatedAt string   `json:"created_at"`
}

// SubscriptionsDoc is the repository's subscriber set.
type SubscriptionsDoc struct {
	Subscribers map[string]Subscription `json:"subscribers"`
	UpdatedAt   string                  `json:"updated_at"`
	Version     int                     `json:"version"`
}

// SubscriptionView is the API/page shape.
type SubscriptionView struct {
	Repo       string   `json:"repo"`
	Subscribed bool     `json:"subscribed"`
	Workflows  []string `json:"workflows,omitempty"`
}

// loadSubscriptions reads the repository's subscriber set.
func (s *Service) loadSubscriptions(ctx context.Context, owner, repo string) (*SubscriptionsDoc, error) {
	raw, _, err := s.getJSON(ctx, SubscriptionsKey(owner, repo))
	if err != nil || raw == nil {
		return &SubscriptionsDoc{Subscribers: map[string]Subscription{}}, err
	}
	doc := &SubscriptionsDoc{Subscribers: map[string]Subscription{}}
	if jerr := json.Unmarshal(raw, doc); jerr != nil {
		return nil, fmt.Errorf("%w: subscriptions: %v", ErrCorrupt, jerr)
	}
	if doc.Subscribers == nil {
		doc.Subscribers = map[string]Subscription{}
	}
	return doc, nil
}

// GetSubscription reports whether the caller watches this repository.
func (s *Service) GetSubscription(ctx context.Context, repo string, p auth.Principal) (*SubscriptionView, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, name, p); err != nil {
		return nil, err
	}
	user := strings.ToLower(strings.TrimSpace(p.Name))
	view := &SubscriptionView{Repo: repoID(owner, name)}
	if user == "" {
		return view, nil
	}
	doc, derr := s.loadSubscriptions(ctx, owner, name)
	if derr != nil {
		return nil, derr
	}
	if sub, ok := doc.Subscribers[user]; ok {
		view.Subscribed, view.Workflows = true, nonNilStr(sub.Workflows)
	}
	return view, nil
}

// SetSubscription subscribes (or unsubscribes) the caller. Any repository
// reader may do this: it is a personal preference, not repository policy, and
// it grants no authority. A subscription to nothing is an unsubscribe.
func (s *Service) SetSubscription(ctx context.Context, repo string, subscribed bool, workflows []string, p auth.Principal) (*SubscriptionView, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, name, p); err != nil {
		return nil, err
	}
	user := strings.ToLower(strings.TrimSpace(p.Name))
	if p.Anonymous || user == "" {
		return nil, fmt.Errorf("%w: sign in to subscribe", ErrUnauthorized)
	}
	now := s.nowUTC().Format(dateTimeFmt)
	view := &SubscriptionView{Repo: repoID(owner, name), Subscribed: subscribed}
	wf := normalizeWorkflows(workflows)
	if subscribed && len(wf) == 0 {
		wf = []string{"*"}
	}
	view.Workflows = wf
	_, err = s.casUpdate(ctx, SubscriptionsKey(owner, name), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		doc := &SubscriptionsDoc{Subscribers: map[string]Subscription{}}
		if cur != nil {
			if jerr := json.Unmarshal(cur, doc); jerr != nil {
				return nil, false, fmt.Errorf("%w: subscriptions: %v", ErrCorrupt, jerr)
			}
		}
		if doc.Subscribers == nil {
			doc.Subscribers = map[string]Subscription{}
		}
		if !subscribed {
			if _, ok := doc.Subscribers[user]; !ok {
				return nil, false, nil
			}
			delete(doc.Subscribers, user)
		} else {
			if prev, ok := doc.Subscribers[user]; ok && sameRepos(prev.Workflows, wf) {
				return nil, false, nil
			}
			doc.Subscribers[user] = Subscription{User: user, Workflows: wf, CreatedAt: now}
		}
		doc.UpdatedAt, doc.Version = now, doc.Version+1
		return mustJSON(doc), true, nil
	})
	if err != nil {
		return nil, err
	}
	if !subscribed {
		view.Workflows = nil
	}
	return view, nil
}

// normalizeWorkflows lowercases/trims a watched set (a workflow name or path).
func normalizeWorkflows(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		w := strings.ToLower(strings.TrimSpace(raw))
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

// watches reports whether a subscription matches a run's workflow.
func (sub Subscription) watches(run *Run) bool {
	if run == nil {
		return false
	}
	if len(sub.Workflows) == 0 {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(run.Workflow))
	path := strings.ToLower(strings.TrimSpace(run.WorkflowPath))
	for _, w := range sub.Workflows {
		if w == "*" || w == name || w == path {
			return true
		}
	}
	return false
}

// validateNotifyWebhook accepts an absolute http(s) URL with no userinfo, or
// the empty string (off). A malformed endpoint is a save error, not a silent
// no-op that never delivers (PAR-197).
func validateNotifyWebhook(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("%w: notify_webhook must be an absolute http(s) URL", ErrInvalid)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: notify_webhook must not carry credentials in the URL", ErrInvalid)
	}
	return u.String(), nil
}

// notifyRunFailure writes one notification per matching subscriber and posts the
// repository webhook. It is idempotent per (user, run): the deterministic key
// makes a repeated call a no-op.
func (s *Service) notifyRunFailure(ctx context.Context, owner, repo, runID string) {
	run, err := s.loadRun(ctx, owner, repo, runID)
	if err != nil || run == nil || run.State != RunFailure {
		return
	}
	subs, serr := s.loadSubscriptions(ctx, owner, repo)
	if serr != nil {
		slog.Warn("threads: notification subscriptions", "repo", repoID(owner, repo), "run", runID, "err", serr)
	} else {
		users := make([]string, 0, len(subs.Subscribers))
		for user := range subs.Subscribers {
			users = append(users, user)
		}
		sort.Strings(users)
		for _, user := range users {
			if !subs.Subscribers[user].watches(run) {
				continue
			}
			note := &Notification{
				ID: runID, User: user, Repo: repoID(owner, repo), RunID: runID,
				Workflow: run.Workflow, State: run.State,
				Title:     fmt.Sprintf("%s failed on %s", run.Workflow, run.Branch),
				Link:      fmt.Sprintf("/%s/%s/threads/%s", owner, repo, runID),
				CreatedAt: s.nowUTC().Format(dateTimeFmt),
			}
			if derr := s.putCreate(ctx, NotificationKey(user, runID), mustJSON(note)); derr != nil && !store.IsPreconditionFailed(derr) {
				slog.Warn("threads: notification write", "user", user, "run", runID, "err", derr)
			}
		}
	}
	// The repository webhook is one best-effort POST, off the fold.
	if settings, derr := s.loadSettings(ctx, owner, repo); derr == nil && settings != nil && strings.TrimSpace(settings.NotifyWebhook) != "" {
		s.postRunWebhook(ctx, settings.NotifyWebhook, owner, repo, run)
	}
}

// postRunWebhook delivers the failure summary. It never follows redirects and
// has a short timeout; a failure is logged and forgotten.
func (s *Service) postRunWebhook(ctx context.Context, endpoint, owner, repo string, run *Run) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		slog.Warn("threads: notify_webhook is not an http(s) URL", "repo", repoID(owner, repo))
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"event": "run_failed", "repo": repoID(owner, repo), "run_id": run.ID,
		"workflow": run.Workflow, "branch": run.Branch, "commit": run.Commit,
		"state": run.State, "link": fmt.Sprintf("/%s/%s/threads/%s", owner, repo, run.ID),
	})
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, rerr := http.NewRequestWithContext(reqCtx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if rerr != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "spindle-threads/1")
	// The shared webhook egress policy: no redirects, and a delivery-time IP
	// screen that refuses private/link-local targets (loopback stays allowed for
	// dev/CI). A tenant-configured URL must not be steerable onto the host's own
	// network or a cloud metadata endpoint.
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: egress.Transport(false),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return egress.RefuseRedirect(nil, nil)
		},
	}
	resp, derr := client.Do(req)
	if derr != nil {
		slog.Warn("threads: notify_webhook delivery", "repo", repoID(owner, repo), "err", derr)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		slog.Warn("threads: notify_webhook answer", "repo", repoID(owner, repo), "status", resp.StatusCode)
	}
}

// ListNotifications reads the caller's inbox newest first (one bounded LIST over
// the user's own prefix; never another user's records).
func (s *Service) ListNotifications(ctx context.Context, p auth.Principal, limit int) ([]Notification, error) {
	user := strings.ToLower(strings.TrimSpace(p.Name))
	if user == "" || p.Anonymous {
		return nil, fmt.Errorf("%w: sign in to read notifications", ErrUnauthorized)
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var keys []string
	if err := s.Store.List(ctx, NotificationPrefix(user), "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, ".json") && !strings.Contains(strings.TrimPrefix(m.Key, NotificationPrefix(user)), "/") {
			keys = append(keys, m.Key)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	out := make([]Notification, 0, limit)
	for _, key := range keys {
		if len(out) >= limit {
			break
		}
		raw, _, err := s.getJSON(ctx, key)
		if err != nil || raw == nil {
			continue
		}
		var n Notification
		if jerr := json.Unmarshal(raw, &n); jerr != nil {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// MarkNotificationsRead flips the caller's unread records (the whole inbox is
// the documented surface; a per-item verb is not).
func (s *Service) MarkNotificationsRead(ctx context.Context, p auth.Principal) error {
	user := strings.ToLower(strings.TrimSpace(p.Name))
	if user == "" || p.Anonymous {
		return fmt.Errorf("%w: sign in to read notifications", ErrUnauthorized)
	}
	var keys []string
	if err := s.Store.List(ctx, NotificationPrefix(user), "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, ".json") && !strings.Contains(strings.TrimPrefix(m.Key, NotificationPrefix(user)), "/") {
			keys = append(keys, m.Key)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, key := range keys {
		_, _ = s.casUpdate(ctx, key, 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
			if cur == nil {
				return nil, false, nil
			}
			var n Notification
			if jerr := json.Unmarshal(cur, &n); jerr != nil {
				return nil, false, nil
			}
			if n.Read {
				return nil, false, nil
			}
			n.Read = true
			return mustJSON(&n), true, nil
		})
	}
	return nil
}
