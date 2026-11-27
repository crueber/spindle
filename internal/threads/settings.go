package threads

// Per-repository Threads settings (PAR-015, PAR-118, PAR-153).
//
// These are the repository-scoped switches that change what a trigger MAY do,
// as distinct from instance policy (config.Threads) which every repository
// inherits. They live in the bucket beside the runs — never on local disk (law
// 4) — and every flip is maintain+ and audited:
//
//	repos/<o>/<r>/threads/settings.json
//
// D-2 (`pull_request_target`): the trigger is off by default. Enabling it is a
// maintainer's written decision; enabling head checkout inside it is a SECOND,
// separately audited decision, and the run still never gets secrets on a head
// checkout (the plan-time refusal in service.go is the enforcement).
//
// D-13 (`send_write_tokens`): a fork run is read-only unless a maintainer turns
// this on. The decision is recorded on each run it affects (Run.WriteTokens) so
// the run page can say what token the run got.
//
// PAR-153 (`concurrency_cap`): 0 = unlimited; 1…maxRepoConcurrencyCap queues a
// run that would exceed the cap, naming the reason on the run.
//
// PAR-157 (`ci_paused`): a paused repository claims nothing. In-flight jobs run
// to completion (a pause is not a cancel), and newly created runs queue with
// the named reason "CI is paused for this repository" until a maintainer
// resumes. The pause marker is mirrored onto the queue head so the claim path
// stays one GET (queue.go), and the switch itself is audited.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Audit action verbs for the repository settings surface (host audit family,
// the runner/environment convention).
const (
	AuditRepoSendWriteTokens = "repo_send_write_tokens"
	AuditRepoPullTarget      = "repo_pull_request_target"
	AuditRepoPRTargetHead    = "repo_pr_target_head_checkout"
	AuditRepoConcurrencyCap  = "repo_concurrency_cap"
	AuditRepoCIPause         = "repo_ci_pause"
	AuditRepoNotifyWebhook   = "repo_notify_webhook"
)

// maxRepoConcurrencyCap bounds the per-repository cap (GitHub's ceiling for
// concurrency limits is 500; the same number here).
const maxRepoConcurrencyCap = 500

// SettingsKey returns repos/<o>/<r>/threads/settings.json.
func SettingsKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/threads/settings.json"
}

// SettingsDoc is the stored record. Every field is a deliberate opt-in: the
// zero value is the safe default (all off, no cap).
type SettingsDoc struct {
	// SendWriteTokens lets a FORK run's job token carry the write scopes its
	// workflow asks for (D-13). Off by default; still never an id-token and
	// still no repository/environment secrets.
	SendWriteTokens bool `json:"send_write_tokens,omitempty"`
	// PullRequestTarget enables `on.pull_request_target` for this repository
	// (D-2). Off by default.
	PullRequestTarget bool `json:"pull_request_target,omitempty"`
	// PRTargetAllowHeadCheckout allows a pull_request_target job's
	// actions/checkout to name the PR head (D-2's second switch). It is only
	// meaningful with PullRequestTarget on, and never applies to secrets: the
	// head checkout is refused at plan time unless this is on.
	PRTargetAllowHeadCheckout bool `json:"pr_target_allow_head_checkout,omitempty"`
	// ConcurrencyCap is the maximum number of live runs in this repository
	// (0 = unlimited). A run created above the cap is held with a named
	// reason and released when a slot frees (PAR-153).
	ConcurrencyCap int `json:"concurrency_cap,omitempty"`
	// CIPaused stops the repository claiming new jobs (PAR-157). In-flight
	// jobs keep running; queued jobs wait under a named reason. Off by default.
	CIPaused bool `json:"ci_paused,omitempty"`
	// NotifyWebhook is the failure-notification endpoint (PAR-197): one POST
	// per failed run, with the run link. Empty = no outbound notification.
	NotifyWebhook string `json:"notify_webhook,omitempty"`

	UpdatedBy string `json:"updated_by,omitempty"`
	UpdatedAt string `json:"updated_at"`
	Version   int    `json:"version"`
}

// SettingsView is the wire shape (GET/PUT …/api/threads/settings).
type SettingsView struct {
	SendWriteTokens           bool   `json:"send_write_tokens"`
	PullRequestTarget         bool   `json:"pull_request_target"`
	PRTargetAllowHeadCheckout bool   `json:"pr_target_allow_head_checkout"`
	ConcurrencyCap            int    `json:"concurrency_cap"`
	CIPaused                  bool   `json:"ci_paused"`
	NotifyWebhook             string `json:"notify_webhook,omitempty"`
	UpdatedBy                 string `json:"updated_by,omitempty"`
	UpdatedAt                 string `json:"updated_at,omitempty"`
	Version                   int    `json:"version,omitempty"`
}

// SettingsInput is the PUT body: a full replacement of the editable fields.
type SettingsInput struct {
	SendWriteTokens           bool   `json:"send_write_tokens"`
	PullRequestTarget         bool   `json:"pull_request_target"`
	PRTargetAllowHeadCheckout bool   `json:"pr_target_allow_head_checkout"`
	ConcurrencyCap            int    `json:"concurrency_cap"`
	CIPaused                  bool   `json:"ci_paused"`
	NotifyWebhook             string `json:"notify_webhook,omitempty"`
}

// view projects the record for the API/page.
func (d *SettingsDoc) view() SettingsView {
	if d == nil {
		return SettingsView{}
	}
	return SettingsView{
		SendWriteTokens: d.SendWriteTokens, PullRequestTarget: d.PullRequestTarget,
		PRTargetAllowHeadCheckout: d.PRTargetAllowHeadCheckout,
		ConcurrencyCap:            d.ConcurrencyCap, CIPaused: d.CIPaused, NotifyWebhook: d.NotifyWebhook,
		UpdatedBy: d.UpdatedBy, UpdatedAt: d.UpdatedAt, Version: d.Version,
	}
}

// loadSettings reads the repository record; (nil, "") when none exists (the
// safe defaults). One exact-key GET — callers that do not need a switch pass
// through this only when the event is fork/pull-target/cap relevant.
func (s *Service) loadSettings(ctx context.Context, owner, repo string) (*SettingsDoc, error) {
	raw, _, err := s.getJSON(ctx, SettingsKey(owner, repo))
	if err != nil || raw == nil {
		return nil, err
	}
	var doc SettingsDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		return nil, fmt.Errorf("%w: threads settings: %v", ErrCorrupt, jerr)
	}
	return &doc, nil
}

// repoPolicy is the decision set CreateRun needs, with the safe defaults for a
// repository that has no record. It is one GET per event, not per job.
type repoPolicy struct {
	sendWriteTokens bool
	pullTarget      bool
	prTargetHead    bool
	cap             int
	ciPaused        bool
}

// loadRepoPolicy reads the settings and folds them into the booleans the
// creation path consults. An unreadable record fails CLOSED (all switches off,
// no cap): a store hiccup must never enable a dangerous trigger.
func (s *Service) loadRepoPolicy(ctx context.Context, owner, repo string) repoPolicy {
	doc, err := s.loadSettings(ctx, owner, repo)
	if err != nil || doc == nil {
		return repoPolicy{}
	}
	return repoPolicy{
		sendWriteTokens: doc.SendWriteTokens,
		pullTarget:      doc.PullRequestTarget,
		prTargetHead:    doc.PRTargetAllowHeadCheckout && doc.PullRequestTarget,
		cap:             doc.ConcurrencyCap,
		ciPaused:        doc.CIPaused,
	}
}

// GetSettings reads the settings record (read gate).
func (s *Service) GetSettings(ctx context.Context, repo string, p auth.Principal) (*SettingsView, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, name, p); err != nil {
		return nil, err
	}
	doc, derr := s.loadSettings(ctx, owner, name)
	if derr != nil {
		return nil, derr
	}
	v := doc.view()
	return &v, nil
}

// UpdateSettings replaces the editable switches. It is maintain+ (D-2/D-13: a
// maintainer writes the decision down) and audits every value that changes, so
// the audit trail names the switch and the new value, not just "settings
// saved". Validation is fail closed: the second head-checkout switch requires
// the first, and the cap is bounded.
func (s *Service) UpdateSettings(ctx context.Context, repo string, in SettingsInput, p auth.Principal) (*SettingsView, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, name, p, "maintain"); err != nil {
		return nil, err
	}
	if in.PRTargetAllowHeadCheckout && !in.PullRequestTarget {
		return nil, fmt.Errorf("%w: pr_target_allow_head_checkout requires pull_request_target to be enabled", ErrInvalid)
	}
	if in.ConcurrencyCap < 0 || in.ConcurrencyCap > maxRepoConcurrencyCap {
		return nil, fmt.Errorf("%w: concurrency_cap must be 0..%d", ErrInvalid, maxRepoConcurrencyCap)
	}
	webhook, werr := validateNotifyWebhook(in.NotifyWebhook)
	if werr != nil {
		return nil, werr
	}
	now := s.nowUTC().Format(dateTimeFmt)
	key := SettingsKey(owner, name)
	var before SettingsDoc
	var written SettingsDoc
	_, err = s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		before = SettingsDoc{}
		if cur != nil {
			if jerr := json.Unmarshal(cur, &before); jerr != nil {
				return nil, false, fmt.Errorf("%w: threads settings: %v", ErrCorrupt, jerr)
			}
		}
		next := SettingsDoc{
			SendWriteTokens: in.SendWriteTokens, PullRequestTarget: in.PullRequestTarget,
			PRTargetAllowHeadCheckout: in.PRTargetAllowHeadCheckout, ConcurrencyCap: in.ConcurrencyCap,
			CIPaused:      in.CIPaused,
			NotifyWebhook: webhook,
			UpdatedBy:     strings.TrimSpace(p.Name), UpdatedAt: now,
		}
		if cur == nil && !in.SendWriteTokens && !in.PullRequestTarget && !in.PRTargetAllowHeadCheckout && in.ConcurrencyCap == 0 && !in.CIPaused && webhook == "" {
			return nil, false, nil // nothing set and nothing stored: no record needed
		}
		next.Version = before.Version + 1
		written = next
		return mustJSON(&next), true, nil
	})
	if err != nil {
		return nil, err
	}
	// Audit each switch that changed (never a value that did not move), so the
	// trail answers "who enabled pull_request_target" directly.
	audits := []struct {
		action   string
		was, now bool
		detail   string
	}{
		{AuditRepoSendWriteTokens, before.SendWriteTokens, in.SendWriteTokens, fmt.Sprintf("send_write_tokens=%t", in.SendWriteTokens)},
		{AuditRepoPullTarget, before.PullRequestTarget, in.PullRequestTarget, fmt.Sprintf("pull_request_target=%t", in.PullRequestTarget)},
		{AuditRepoPRTargetHead, before.PRTargetAllowHeadCheckout, in.PRTargetAllowHeadCheckout, fmt.Sprintf("pr_target_allow_head_checkout=%t", in.PRTargetAllowHeadCheckout)},
	}
	for _, a := range audits {
		if a.was != a.now {
			s.auditAdmin(ctx, p.Name, a.action, "repo:"+repoID(owner, name), a.detail)
		}
	}
	if before.ConcurrencyCap != in.ConcurrencyCap {
		s.auditAdmin(ctx, p.Name, AuditRepoConcurrencyCap, "repo:"+repoID(owner, name),
			fmt.Sprintf("concurrency_cap=%d", in.ConcurrencyCap))
	}
	if before.CIPaused != in.CIPaused {
		s.auditAdmin(ctx, p.Name, AuditRepoCIPause, "repo:"+repoID(owner, name),
			fmt.Sprintf("ci_paused=%t", in.CIPaused))
	}
	if before.NotifyWebhook != webhook {
		s.auditAdmin(ctx, p.Name, AuditRepoNotifyWebhook, "repo:"+repoID(owner, name),
			fmt.Sprintf("notify_webhook_set=%t", webhook != ""))
	}
	// Mirror the pause marker onto the queue head so a claim probe refuses the
	// repository without a settings read (PAR-157, the one-GET claim path).
	if before.CIPaused != in.CIPaused {
		if herr := s.setHeadPaused(ctx, owner, name, in.CIPaused); herr != nil {
			slog.Warn("threads: pause marker", "repo", repoID(owner, name), "paused", in.CIPaused, "err", herr)
		}
	}
	v := written.view()
	return &v, nil
}

// SetCIPaused flips only the repository pause switch (PAR-157), leaving every
// other setting untouched. It is the CLI/API affordance beside the full
// settings form: a caller that pauses CI must not have to resend the other
// switches (and risk clearing one). Maintain+ and audited.
func (s *Service) SetCIPaused(ctx context.Context, repo string, paused bool, p auth.Principal) (*SettingsView, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, name, p, "maintain"); err != nil {
		return nil, err
	}
	now := s.nowUTC().Format(dateTimeFmt)
	var before bool
	var written SettingsDoc
	_, err = s.casUpdate(ctx, SettingsKey(owner, name), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur != nil {
			if jerr := json.Unmarshal(cur, &written); jerr != nil {
				return nil, false, fmt.Errorf("%w: threads settings: %v", ErrCorrupt, jerr)
			}
		}
		before = written.CIPaused
		if before == paused {
			return nil, false, nil
		}
		written.CIPaused = paused
		written.UpdatedBy, written.UpdatedAt = strings.TrimSpace(p.Name), now
		written.Version++
		return mustJSON(&written), true, nil
	})
	if err != nil {
		return nil, err
	}
	if before != paused {
		s.auditAdmin(ctx, p.Name, AuditRepoCIPause, "repo:"+repoID(owner, name), fmt.Sprintf("ci_paused=%t", paused))
		if herr := s.setHeadPaused(ctx, owner, name, paused); herr != nil {
			slog.Warn("threads: pause marker", "repo", repoID(owner, name), "paused", paused, "err", herr)
		}
	}
	v := written.view()
	return &v, nil
}
