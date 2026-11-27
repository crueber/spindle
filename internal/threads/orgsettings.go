package threads

// Per-organization Threads settings (PAR-153).
//
// The only organization-scoped Threads switch is the concurrency cap: the
// maximum number of live runs across every repository the organization owns.
// It lives beside the organization's variables and secrets:
//
//	orgs/<org>/threads/settings.json
//
// Scope: an org owner sets it (host admin bypasses), every flip is audited, and
// a repository owned by a user (not an organization) is unaffected — the
// service only reads the record when the owner's organization nature is
// confirmed by the narrow IsOrg seam, so a user-owned repository adds no store
// round trip for a setting that cannot apply to it.
//
// Enforcement is a hold, not a queue entry: a run created above the org cap is
// written with its jobs (the page, checks mirror and PR panel stay coherent)
// but not enqueued, and its header names the reason. The org cap document
// (`orgs/<org>/threads/cap.json`) tracks the admitted and held runs so a
// terminal fold in ANY of the organization's repositories can admit the oldest
// held one without a LIST over every repository (capacity_org.go).
//
// A corrupt or unreadable settings record reads as "no cap", matching the
// per-repository settings loader: display/limits must not wedge every run in
// the organization on a transient read. A corrupt cap index is a named error on
// admission (the cap cannot be enforced honestly), never a silent bypass.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// AuditOrgConcurrencyCap is the audit verb for an organization cap change.
const AuditOrgConcurrencyCap = "org_concurrency_cap"

// OrgSettingsKey returns orgs/<org>/threads/settings.json.
func OrgSettingsKey(org string) string { return "orgs/" + org + "/threads/settings.json" }

// OrgSettingsDoc is the stored organization settings record.
type OrgSettingsDoc struct {
	// ConcurrencyCap bounds live runs across every repository the
	// organization owns (0 = unlimited, 1…maxRepoConcurrencyCap).
	ConcurrencyCap int    `json:"concurrency_cap,omitempty"`
	UpdatedBy      string `json:"updated_by,omitempty"`
	UpdatedAt      string `json:"updated_at"`
	Version        int    `json:"version"`
}

// OrgSettingsView is the wire/page shape. Live/Held report the cap index's
// current usage (admitted runs across the organization and runs held above the
// cap), so an owner can see why a run waits without reading run headers.
type OrgSettingsView struct {
	ConcurrencyCap int    `json:"concurrency_cap"`
	Live           int    `json:"live"`
	Held           int    `json:"held"`
	UpdatedBy      string `json:"updated_by,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
	Version        int    `json:"version,omitempty"`
}

// OrgSettingsInput is the PUT body (a full replacement of the editable field).
type OrgSettingsInput struct {
	ConcurrencyCap int `json:"concurrency_cap"`
}

// orgSettingsView projects the record for the API/page.
func orgSettingsView(d *OrgSettingsDoc) OrgSettingsView {
	if d == nil {
		return OrgSettingsView{}
	}
	return OrgSettingsView{
		ConcurrencyCap: d.ConcurrencyCap, UpdatedBy: d.UpdatedBy,
		UpdatedAt: d.UpdatedAt, Version: d.Version,
	}
}

// loadOrgSettings reads one organization's settings record; (nil, "") when none
// exists (the safe default: no cap).
func (s *Service) loadOrgSettings(ctx context.Context, org string) (*OrgSettingsDoc, store.Version) {
	raw, ver, err := s.getJSON(ctx, OrgSettingsKey(org))
	if err != nil {
		slog.Warn("threads: organization settings unreadable; no cap applied", "org", org, "err", err)
		return nil, ""
	}
	if raw == nil {
		return nil, ""
	}
	var doc OrgSettingsDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		slog.Warn("threads: organization settings corrupt; no cap applied", "org", org, "err", jerr)
		return nil, ""
	}
	return &doc, ver
}

// orgCap returns the organization cap in force for owner (0 = none). A
// user-owned namespace pays no read at all: the IsOrg seam answers first.
func (s *Service) orgCap(ctx context.Context, owner string) int {
	if s.IsOrg == nil {
		return 0
	}
	isOrg, err := s.IsOrg(ctx, owner)
	if err != nil {
		slog.Warn("threads: organization lookup failed; no organization cap applied", "owner", owner, "err", err)
		return 0
	}
	if !isOrg {
		return 0
	}
	doc, _ := s.loadOrgSettings(ctx, owner)
	if doc == nil {
		return 0
	}
	return doc.ConcurrencyCap
}

// GetOrgSettings reads the organization settings (org owner or host admin).
func (s *Service) GetOrgSettings(ctx context.Context, org string, p auth.Principal) (*OrgSettingsView, error) {
	org = strings.TrimSpace(org)
	if err := s.requireOrgAdmin(ctx, org, p); err != nil {
		return nil, err
	}
	doc, _ := s.loadOrgSettings(ctx, org)
	view := orgSettingsView(doc)
	if cap, cerr := s.loadOrgCapDoc(ctx, org); cerr == nil && cap != nil {
		view.Live, view.Held = len(cap.Live), len(cap.Held)
	}
	return &view, nil
}

// SetOrgSettings replaces the editable organization settings (org owner or host
// admin, audited). The cap bound is the same 0…500 the repository cap uses.
func (s *Service) SetOrgSettings(ctx context.Context, org string, in OrgSettingsInput, p auth.Principal) (*OrgSettingsView, error) {
	org = strings.TrimSpace(org)
	if org == "" {
		return nil, fmt.Errorf("%w: organization is required", ErrInvalid)
	}
	if err := s.requireOrgAdmin(ctx, org, p); err != nil {
		return nil, err
	}
	if in.ConcurrencyCap < 0 || in.ConcurrencyCap > maxRepoConcurrencyCap {
		return nil, fmt.Errorf("%w: concurrency_cap must be 0..%d", ErrInvalid, maxRepoConcurrencyCap)
	}
	now := s.nowUTC().Format(dateTimeFmt)
	var before int
	_, err := s.casUpdate(ctx, OrgSettingsKey(org), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		doc := &OrgSettingsDoc{}
		if cur != nil {
			if jerr := json.Unmarshal(cur, doc); jerr != nil {
				return nil, false, fmt.Errorf("%w: organization settings: %v", ErrCorrupt, jerr)
			}
		}
		before = doc.ConcurrencyCap
		if cur != nil && doc.ConcurrencyCap == in.ConcurrencyCap {
			return nil, false, nil // no change: no version churn
		}
		doc.ConcurrencyCap = in.ConcurrencyCap
		doc.UpdatedBy = truncate(strings.ToLower(strings.TrimSpace(p.Name)), 100)
		doc.UpdatedAt = now
		doc.Version++
		return mustJSON(doc), true, nil
	})
	if err != nil {
		return nil, err
	}
	if before != in.ConcurrencyCap {
		s.auditAdmin(ctx, p.Name, AuditOrgConcurrencyCap, "org:"+org,
			fmt.Sprintf("concurrency_cap=%d", in.ConcurrencyCap))
	}
	// Removing the cap admits everything it held. Raising it is served by the
	// next terminal fold (each fold admits one held run), so no burst of
	// enqueues happens on an admin save.
	if before > 0 && in.ConcurrencyCap == 0 {
		s.releaseOrgHeldRuns(ctx, org, "", "")
	}
	doc, _ := s.loadOrgSettings(ctx, org)
	view := orgSettingsView(doc)
	return &view, nil
}
