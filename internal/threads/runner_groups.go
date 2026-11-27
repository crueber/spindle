package threads

// Runner groups (PAR-175).
//
// A group is an administrator-owned bundle of two things:
//
//   - INHERITED LABELS: every member runner advertises the group's labels as
//     well as its own, so one group label can select a whole fleet section
//     without editing each runner.
//   - VISIBILITY: an owner/name / owner/* allowlist (or all repositories). It
//     is INTERSECTED with the runner's own admin-set policy, so a group can
//     only narrow what a runner may be offered — it never widens tenancy. That
//     is the fail-closed reading of GitHub's "group with repository access":
//     a mis-assigned group cannot hand one team's repository to another's host.
//
// The group is the source of truth; its labels and visibility are MATERIALIZED
// onto each member runner's registry record when the group is created, updated,
// deleted or assigned, so the claim path still reads one record and pays no
// second GET (the queue's round-trip budget, AGENTS law 6). The group record is
// what the admin page edits; the runner record is the derived cache.
//
// Every mutation is host-administrator only and audited (PAR-181's rule).

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// RunnerGroupPrefix is the group record family.
func RunnerGroupPrefix() string { return "threads/runner_groups/" }

// RunnerGroupKey returns threads/runner_groups/<id>.json.
func RunnerGroupKey(id string) string { return RunnerGroupPrefix() + id + ".json" }

// MaxRunnerGroups bounds the family (groups are a human-scale fleet concept).
const MaxRunnerGroups = 100

// runnerGroupIDRe matches a group id (the URL segment, lowercased).
var runnerGroupIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// RunnerGroup is one stored group (never a secret).
type RunnerGroup struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Labels []string `json:"labels"`
	// AllowAll / Repos are the group's repository visibility. A group with no
	// visibility allows nothing: assigning it refuses every repository, which is
	// the fail-closed default rather than "all".
	AllowAll  bool     `json:"allow_all,omitempty"`
	Repos     []string `json:"repos,omitempty"`
	CreatedBy string   `json:"created_by,omitempty"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	Version   int      `json:"version"`
}

// RunnerGroupInput is the create/update body.
type RunnerGroupInput struct {
	Name   string   `json:"name"`
	ID     string   `json:"id,omitempty"`
	Labels []string `json:"labels"`
	RunnerPolicyInput
}

// RunnerGroupView is the wire shape (with the member count the page renders).
type RunnerGroupView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Labels      []string `json:"labels"`
	AllowAll    bool     `json:"allow_all"`
	Repos       []string `json:"repos,omitempty"`
	Scope       string   `json:"scope"`
	ScopeDetail string   `json:"scope_detail"`
	MemberCount int      `json:"member_count"`
	CreatedBy   string   `json:"created_by,omitempty"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

func (g *RunnerGroup) view(members int) RunnerGroupView {
	if g == nil {
		return RunnerGroupView{}
	}
	pol := RunnerPolicy{AllowAll: g.AllowAll, Repos: g.Repos}
	return RunnerGroupView{
		ID: g.ID, Name: g.Name, Labels: nonNilStr(g.Labels),
		AllowAll: g.AllowAll, Repos: nonNilStr(g.Repos),
		Scope: pol.Scope(), ScopeDetail: pol.DescribeScope(), MemberCount: members,
		CreatedBy: g.CreatedBy, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt,
	}
}

// loadRunnerGroup reads one group; (nil, nil) when absent.
func (s *Service) loadRunnerGroup(ctx context.Context, id string) (*RunnerGroup, error) {
	raw, _, err := s.getJSON(ctx, RunnerGroupKey(id))
	if err != nil || raw == nil {
		return nil, err
	}
	var g RunnerGroup
	if jerr := json.Unmarshal(raw, &g); jerr != nil {
		return nil, fmt.Errorf("%w: runner group %s: %v", ErrCorrupt, id, jerr)
	}
	return &g, nil
}

// listRunnerGroups reads the family (host-admin page; a bounded LIST over a
// human-scale prefix, never a request hot path).
func (s *Service) listRunnerGroups(ctx context.Context) ([]*RunnerGroup, error) {
	var keys []string
	if err := s.Store.List(ctx, RunnerGroupPrefix(), "", func(m store.ObjectMeta) error {
		rest := strings.TrimPrefix(m.Key, RunnerGroupPrefix())
		if strings.HasSuffix(rest, ".json") && !strings.Contains(rest, "/") {
			keys = append(keys, m.Key)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(keys)
	out := make([]*RunnerGroup, 0, len(keys))
	for _, key := range keys {
		raw, _, err := s.getJSON(ctx, key)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		var g RunnerGroup
		if jerr := json.Unmarshal(raw, &g); jerr != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, key, jerr)
		}
		out = append(out, &g)
	}
	return out, nil
}

// normaliseGroupID derives the id from the input (or the name).
func normaliseGroupID(in RunnerGroupInput) string {
	id := strings.ToLower(strings.TrimSpace(in.ID))
	if id == "" {
		id = strings.ToLower(strings.TrimSpace(in.Name))
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	id = strings.Trim(b.String(), ".-_")
	return id
}

// ValidateRunnerGroupInput normalises the editable fields. A group with no
// labels is allowed (it is a pure visibility partition); a group with no
// visibility allows no repository by construction, and is refused at creation
// so the mistake is visible at write time.
func ValidateRunnerGroupInput(in RunnerGroupInput) (*RunnerGroup, error) {
	pol, err := ValidateRunnerPolicy(in.RunnerPolicyInput)
	if err != nil {
		return nil, err
	}
	name := truncate(strings.TrimSpace(in.Name), 100)
	if name == "" {
		return nil, fmt.Errorf("%w: a runner group needs a name", ErrInvalid)
	}
	id := normaliseGroupID(in)
	if !runnerGroupIDRe.MatchString(id) {
		return nil, fmt.Errorf("%w: runner group name %q does not produce a valid id", ErrInvalid, in.Name)
	}
	return &RunnerGroup{ID: id, Name: name, Labels: normalizeLabels(in.Labels), AllowAll: pol.AllowAll, Repos: pol.Repos}, nil
}

// CreateRunnerGroup creates a group (host admin, audited).
func (s *Service) CreateRunnerGroup(ctx context.Context, p auth.Principal, in RunnerGroupInput) (*RunnerGroupView, error) {
	if err := requireHostAdmin(p); err != nil {
		return nil, err
	}
	g, verr := ValidateRunnerGroupInput(in)
	if verr != nil {
		return nil, verr
	}
	existing, err := s.listRunnerGroups(ctx)
	if err != nil {
		return nil, err
	}
	if len(existing) >= MaxRunnerGroups {
		return nil, fmt.Errorf("%w: at most %d runner groups", ErrUnprocessable, MaxRunnerGroups)
	}
	now := s.nowUTC().Format(dateTimeFmt)
	g.CreatedBy = strings.TrimSpace(p.Name)
	g.CreatedAt, g.UpdatedAt, g.Version = now, now, 1
	if perr := s.putCreate(ctx, RunnerGroupKey(g.ID), mustJSON(g)); perr != nil {
		if store.IsPreconditionFailed(perr) {
			return nil, fmt.Errorf("%w: runner group %q already exists", ErrConflict, g.ID)
		}
		return nil, perr
	}
	s.auditAdmin(ctx, p.Name, AuditRunnerGroupCreate, "runner-group:"+g.ID,
		fmt.Sprintf("name=%s;labels=%s;visibility=%s", g.Name, strings.Join(g.Labels, ","), RunnerPolicy{AllowAll: g.AllowAll, Repos: g.Repos}.Describe()))
	v := g.view(0)
	return &v, nil
}

// UpdateRunnerGroup replaces a group's name/labels/visibility and re-applies it
// to every member runner (host admin, audited).
func (s *Service) UpdateRunnerGroup(ctx context.Context, p auth.Principal, id string, in RunnerGroupInput) (*RunnerGroupView, error) {
	if err := requireHostAdmin(p); err != nil {
		return nil, err
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if !runnerGroupIDRe.MatchString(id) {
		return nil, fmt.Errorf("%w: unknown runner group %q", ErrNotFound, id)
	}
	if existing, lerr := s.loadRunnerGroup(ctx, id); lerr != nil {
		return nil, lerr
	} else if existing == nil {
		return nil, fmt.Errorf("%w: unknown runner group %q", ErrNotFound, id)
	}
	g, verr := ValidateRunnerGroupInput(in)
	if verr != nil {
		return nil, verr
	}
	if g.ID != id {
		return nil, fmt.Errorf("%w: a group's id is fixed (%s)", ErrInvalid, id)
	}
	key := RunnerGroupKey(id)
	var written *RunnerGroup
	_, err := s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown runner group %q", ErrNotFound, id)
		}
		var old RunnerGroup
		if jerr := json.Unmarshal(cur, &old); jerr != nil {
			return nil, false, fmt.Errorf("%w: runner group %s: %v", ErrCorrupt, id, jerr)
		}
		next := *g
		next.CreatedBy, next.CreatedAt, next.UpdatedAt = old.CreatedBy, old.CreatedAt, s.nowUTC().Format(dateTimeFmt)
		next.Version = old.Version + 1
		written = &next
		return mustJSON(&next), true, nil
	})
	if err != nil {
		return nil, err
	}
	if aerr := s.reapplyRunnerGroup(ctx, written); aerr != nil {
		return nil, aerr
	}
	s.auditAdmin(ctx, p.Name, AuditRunnerGroupUpdate, "runner-group:"+id,
		fmt.Sprintf("name=%s;labels=%s;visibility=%s", written.Name, strings.Join(written.Labels, ","),
			RunnerPolicy{AllowAll: written.AllowAll, Repos: written.Repos}.Describe()))
	v := written.view(s.runnerGroupMembers(ctx, id))
	return &v, nil
}

// DeleteRunnerGroup removes a group and clears its materialized labels and
// visibility from every member runner (host admin, audited). The runners
// themselves stay registered with their own policy and labels.
func (s *Service) DeleteRunnerGroup(ctx context.Context, p auth.Principal, id string) error {
	if err := requireHostAdmin(p); err != nil {
		return err
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if !runnerGroupIDRe.MatchString(id) {
		return fmt.Errorf("%w: unknown runner group %q", ErrNotFound, id)
	}
	if cerr := s.clearRunnerGroupMembers(ctx, id); cerr != nil {
		return cerr
	}
	if derr := s.Store.Delete(ctx, RunnerGroupKey(id), ""); derr != nil && !store.IsPreconditionFailed(derr) {
		return derr
	}
	s.auditAdmin(ctx, p.Name, AuditRunnerGroupDelete, "runner-group:"+id, "deleted")
	return nil
}

// SetRunnerGroup assigns (or clears, with an empty group) one runner's group
// (host admin, audited). The group's labels and visibility are materialized on
// the runner record immediately, so the next fetch honours the change.
func (s *Service) SetRunnerGroup(ctx context.Context, id, groupID string, p auth.Principal) (*RunnerView, error) {
	if err := requireHostAdmin(p); err != nil {
		return nil, err
	}
	id = strings.ToLower(strings.TrimSpace(id))
	groupID = strings.ToLower(strings.TrimSpace(groupID))
	if !tokenIDRe.MatchString(id) {
		return nil, fmt.Errorf("%w: unknown runner %q", ErrNotFound, id)
	}
	var group *RunnerGroup
	if groupID != "" {
		if !runnerGroupIDRe.MatchString(groupID) {
			return nil, fmt.Errorf("%w: unknown runner group %q", ErrNotFound, groupID)
		}
		g, gerr := s.loadRunnerGroup(ctx, groupID)
		if gerr != nil {
			return nil, gerr
		}
		if g == nil {
			return nil, fmt.Errorf("%w: unknown runner group %q", ErrNotFound, groupID)
		}
		group = g
	}
	var written *RunnerDoc
	_, err := s.casUpdate(ctx, RunnerKey(id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown runner %q", ErrNotFound, id)
		}
		var doc RunnerDoc
		if jerr := json.Unmarshal(cur, &doc); jerr != nil {
			return nil, false, fmt.Errorf("%w: runner %s: %v", ErrCorrupt, id, jerr)
		}
		if doc.State == RunnerRevoked || doc.RevokedAt != nil {
			return nil, false, fmt.Errorf("%w: runner %s is revoked", ErrConflict, id)
		}
		if doc.Group == groupID {
			return nil, false, nil
		}
		if group == nil {
			doc.Group = ""
			doc.GroupLabels, doc.GroupAllowAll, doc.GroupRepos = nil, false, nil
		} else {
			applyRunnerGroup(&doc, group)
		}
		doc.Version++
		written = &doc
		return mustJSON(&doc), true, nil
	})
	if err != nil {
		return nil, err
	}
	if written == nil {
		doc, _, lerr := s.loadRunner(ctx, id)
		if lerr != nil {
			return nil, lerr
		}
		written = doc
	}
	s.auditAdmin(ctx, p.Name, AuditRunnerGroupAssign, "runner:"+id, "group="+groupID)
	return s.runnerView(ctx, written), nil
}

// applyRunnerGroup materializes a group onto a runner record (the derived cache
// the claim path reads). The group id is always recorded, so membership is
// visible on the fleet row.
func applyRunnerGroup(doc *RunnerDoc, g *RunnerGroup) {
	if doc == nil || g == nil {
		return
	}
	doc.Group = g.ID
	doc.GroupLabels = append([]string{}, g.Labels...)
	doc.GroupAllowAll = g.AllowAll
	doc.GroupRepos = append([]string{}, g.Repos...)
}

// reapplyRunnerGroup rewrites every member runner's materialized copy after a
// group edit.
func (s *Service) reapplyRunnerGroup(ctx context.Context, g *RunnerGroup) error {
	if g == nil {
		return nil
	}
	runners, err := s.loadRunnerDocsForReasons(ctx)
	if err != nil {
		return err
	}
	for _, doc := range runners {
		if doc.Group != g.ID {
			continue
		}
		id := doc.ID
		_, cerr := s.casUpdate(ctx, RunnerKey(id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
			if cur == nil {
				return nil, false, nil
			}
			var d RunnerDoc
			if jerr := json.Unmarshal(cur, &d); jerr != nil {
				return nil, false, fmt.Errorf("%w: runner %s: %v", ErrCorrupt, id, jerr)
			}
			if d.Group != g.ID {
				return nil, false, nil
			}
			applyRunnerGroup(&d, g)
			d.Version++
			return mustJSON(&d), true, nil
		})
		if cerr != nil {
			return cerr
		}
	}
	return nil
}

// clearRunnerGroupMembers drops a deleted group from every member runner.
func (s *Service) clearRunnerGroupMembers(ctx context.Context, groupID string) error {
	runners, err := s.loadRunnerDocsForReasons(ctx)
	if err != nil {
		return err
	}
	for _, doc := range runners {
		if doc.Group != groupID {
			continue
		}
		id := doc.ID
		_, cerr := s.casUpdate(ctx, RunnerKey(id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
			if cur == nil {
				return nil, false, nil
			}
			var d RunnerDoc
			if jerr := json.Unmarshal(cur, &d); jerr != nil {
				return nil, false, fmt.Errorf("%w: runner %s: %v", ErrCorrupt, id, jerr)
			}
			if d.Group != groupID {
				return nil, false, nil
			}
			d.Group = ""
			d.GroupLabels, d.GroupAllowAll, d.GroupRepos = nil, false, nil
			d.Version++
			return mustJSON(&d), true, nil
		})
		if cerr != nil {
			return cerr
		}
	}
	return nil
}

// runnerGroupMembers counts a group's runners (admin page; one bounded LIST).
func (s *Service) runnerGroupMembers(ctx context.Context, groupID string) int {
	runners, err := s.loadRunnerDocsForReasons(ctx)
	if err != nil {
		return 0
	}
	n := 0
	for _, doc := range runners {
		if doc.Group == groupID {
			n++
		}
	}
	return n
}

// ListRunnerGroups lists groups with their member counts (host admin).
func (s *Service) ListRunnerGroups(ctx context.Context, p auth.Principal) ([]RunnerGroupView, error) {
	if err := requireHostAdmin(p); err != nil {
		return nil, err
	}
	groups, err := s.listRunnerGroups(ctx)
	if err != nil {
		return nil, err
	}
	runners, _ := s.loadRunnerDocsForReasons(ctx)
	counts := map[string]int{}
	for _, doc := range runners {
		if doc.Group != "" {
			counts[doc.Group]++
		}
	}
	out := make([]RunnerGroupView, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.view(counts[g.ID]))
	}
	return out, nil
}

// runnerView projects one RunnerDoc (the SetRunnerGroup answer).
func (s *Service) runnerView(ctx context.Context, doc *RunnerDoc) *RunnerView {
	if doc == nil {
		return &RunnerView{}
	}
	return &RunnerView{
		ID: doc.ID, Name: doc.Name, Labels: nonNilStr(doc.Labels), LabelImages: doc.LabelImages, Capacity: doc.Capacity,
		State: doc.State, DeclaredVersion: doc.DeclaredVersion, RegisteredBy: doc.RegisteredBy,
		CreatedAt: doc.CreatedAt, RegisteredAt: doc.RegisteredAt, LastSeenAt: doc.LastSeenAt,
		RevokedAt: doc.RevokedAt, CurrentJob: doc.CurrentJob, AllowForkRuns: doc.AllowForkRuns,
		AllowAll: doc.AllowAll, Repos: nonNilStr(doc.Repos),
		Scope: doc.policy().Scope(), ScopeDetail: doc.policy().DescribeScope(),
		Ephemeral: doc.Ephemeral, Group: doc.Group, GroupLabels: nonNilStr(doc.GroupLabels),
		MaxCPUs: doc.MaxCPUs, MaxMemoryBytes: doc.MaxMemoryBytes,
	}
}
