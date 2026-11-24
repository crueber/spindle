package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/policy"
)

// ---------------------------------------------------------------------------
// Branch protection — named rulesets (PLAN-V2 §2.4, milestones P1/P2)
//
// A ruleset is a NAME plus the pair of policy rules that carry its effects:
//
//	protection:<slug>-push     effect.protect          (push half + merge half)
//	protection:<slug>-reviews  effect.required-reviews (approvals)
//
// The ruleset is not a new stored object — policy.json stays the record and
// the API stays the validator. This file owns three things:
//
//  1. the read mapping (rules → rulesets), including the pre-ruleset names
//     `protection-push`/`protection-reviews`, which present as the ruleset
//     "default" and are rewritten to the new names on the first save. The
//     mapping IS the read: no code path special-cases the old spelling
//     afterwards (the no-shims law);
//  2. a lossless write (a ruleset save replaces only that pair's two rules,
//     preserving every other rule's bytes and every group);
//  3. the effective-rules view — what actually applies to one ref, computed
//     by the same policy functions the server enforces with.
//
// A rule the form cannot express is never guessed at: the pair is marked
// read-only with a reason, the form refuses to save over it, and its bytes
// survive every other ruleset's save untouched.

const (
	settingsProtectionPrefix        = "protection:"
	settingsProtectionPushSuffix    = "-push"
	settingsProtectionReviewsSuffix = "-reviews"
	// settingsProtectionLegacyPush/Reviews are the pre-ruleset managed names
	// (the single-ruleset tab). They read as the ruleset "default".
	settingsProtectionLegacyPush    = "protection-push"
	settingsProtectionLegacyReviews = "protection-reviews"
	settingsProtectionDefaultSlug   = "default"
	settingsProtectionNewSlug       = "new"
)

// settingsRulesetSlugRe is the org-slug grammar (PLAN-V2 §2.4): it is a URL
// path segment and a rule-name infix at the same time.
var settingsRulesetSlugRe = regexp.MustCompile(`^[a-z0-9-]{1,39}$`)

// settingsProtectionRoles is the "who may push" role set (the repo role
// ladder, 01 §4).
var settingsProtectionRoles = []SettingsOption{
	{"read", "read"}, {"triage", "triage"}, {"write", "write"},
	{"maintain", "maintain"}, {"admin", "admin"},
}

// settingsMergeMethodOptions is the closed merge-method universe in
// presentation order (policy.MergeMethodsAll is the authority).
var settingsMergeMethodOptions = []SettingsOption{
	{policy.MethodMerge, "merge commit"},
	{policy.MethodSquash, "squash"},
	{policy.MethodRebase, "rebase"},
}

// settingsRulesetRuleNames maps a slug to its pair of rule names.
func settingsRulesetRuleNames(slug string) (push, reviews string) {
	return settingsProtectionPrefix + slug + settingsProtectionPushSuffix,
		settingsProtectionPrefix + slug + settingsProtectionReviewsSuffix
}

// settingsRulesetSlugFromRule classifies a rule name. ok=false means the
// form does not own the name (it stays a single, untouched rule).
func settingsRulesetSlugFromRule(name string) (slug, kind string, ok bool) {
	switch name {
	case settingsProtectionLegacyPush:
		return settingsProtectionDefaultSlug, "push", true
	case settingsProtectionLegacyReviews:
		return settingsProtectionDefaultSlug, "reviews", true
	}
	rest, has := strings.CutPrefix(name, settingsProtectionPrefix)
	if !has {
		return "", "", false
	}
	if s, hit := strings.CutSuffix(rest, settingsProtectionPushSuffix); hit && s != "" {
		return s, "push", true
	}
	if s, hit := strings.CutSuffix(rest, settingsProtectionReviewsSuffix); hit && s != "" {
		return s, "reviews", true
	}
	return "", "", false
}

// settingsRulesetPair is one slug's rules as found in the document.
type settingsRulesetPair struct {
	slug    string
	push    *policy.Rule
	reviews *policy.Rule
}

// settingsRulesetPairs groups the document's rules by ruleset slug, in
// document order (the order is the priority the list shows and moves).
func settingsRulesetPairs(doc *policy.Document) []settingsRulesetPair {
	if doc == nil {
		return nil
	}
	var out []settingsRulesetPair
	index := map[string]int{}
	for _, r := range doc.Rules {
		slug, kind, ok := settingsRulesetSlugFromRule(r.Name)
		if !ok {
			continue
		}
		i, seen := index[slug]
		if !seen {
			index[slug] = len(out)
			out = append(out, settingsRulesetPair{slug: slug})
			i = len(out) - 1
		}
		switch kind {
		case "push":
			out[i].push = r
		case "reviews":
			out[i].reviews = r
		}
	}
	return out
}

// settingsBypassRole extracts the role from a `role:<owner>/<repo>:<role>`
// bypass spelling, for THIS repo and a role the picker knows. Anything else
// (a principal, a tag, a group, another repo's role, an unknown role) is a
// spelling the form does not own.
func settingsBypassRole(owner, repo, spelling string) string {
	ref, ok := strings.CutPrefix(spelling, "role:")
	if !ok {
		return ""
	}
	prefix := owner + "/" + repo + ":"
	if !strings.HasPrefix(ref, prefix) {
		return ""
	}
	role := strings.TrimPrefix(ref, prefix)
	if !settingsRoleKnown(role) {
		return ""
	}
	return role
}

// settingsRulesetReadOnlyReason explains why the form cannot express a
// ruleset's pair, or "" when it can. Read-only is a promise about bytes: the
// save path never rewrites a pair it cannot express.
func settingsRulesetReadOnlyReason(owner, repo string, pair settingsRulesetPair) string {
	switch {
	case pair.push == nil:
		return "the review rule has no matching push rule — edit it in the raw policy"
	}
	push := pair.push
	pe := push.Protect()
	switch {
	case pe == nil:
		return "the push rule does not carry a protect effect"
	case push.Mode != "":
		return "the push rule carries a mode"
	case len(push.Match.Principals) > 0 || len(push.Match.Paths) > 0:
		return "the push rule narrows on principals or paths"
	case !containsStr(pe.RestrictOps, policy.OpCreate) || !containsStr(pe.RestrictOps, policy.OpUpdate):
		return "the push rule does not restrict create and update"
	}
	for _, b := range pe.Bypass {
		if settingsBypassRole(owner, repo, b) == "" {
			return "the push rule bypasses an actor this form does not own (" + b + ") — edit it in the raw policy"
		}
	}
	if pair.reviews == nil {
		return ""
	}
	rev := pair.reviews
	re := rev.RequiredReviews()
	switch {
	case re == nil:
		return "the review rule does not carry a required-reviews effect"
	case rev.Mode != "":
		return "the review rule carries a mode"
	case len(rev.Match.Principals) > 0 || len(rev.Match.Paths) > 0:
		return "the review rule narrows on principals or paths"
	case len(re.Bypass) > 0:
		return "the review rule carries a bypass list"
	case strings.Join(rev.Match.Refs, "\x00") != strings.Join(push.Match.Refs, "\x00"):
		return "the two rules match different refs"
	}
	return ""
}

// settingsRulesetFromPair projects a stored pair onto the form model.
func settingsRulesetFromPair(owner, repo string, pair settingsRulesetPair, order int) SettingsRuleset {
	rs := SettingsRuleset{
		Slug: pair.slug, Enabled: true, Editable: true, Order: order,
		BlockForce: true, BlockDelete: true,
	}
	if pair.push != nil {
		rs.PushRule = pair.push.Name
		rs.RefPatterns = append([]string{}, pair.push.Match.Refs...)
		rs.Refs = strings.Join(rs.RefPatterns, ", ")
		if pe := pair.push.Protect(); pe != nil {
			rs.BlockForce = containsStr(pe.RestrictOps, policy.OpForcePush)
			rs.BlockDelete = containsStr(pe.RestrictOps, policy.OpDelete)
			rs.Checks = append([]string{}, pe.RequireChecks...)
			rs.ChecksText = strings.Join(rs.Checks, ", ")
			rs.RequireUpToDate = pe.RequireUpToDate
			rs.MergeMethods = append([]string{}, pe.MergeMethods...)
			rs.MethodsText = strings.Join(pe.MergeMethods, ", ")
			for _, b := range pe.Bypass {
				if role := settingsBypassRole(owner, repo, b); role != "" {
					rs.PushRoles = append(rs.PushRoles, role)
				}
			}
		}
	}
	if pair.reviews != nil {
		rs.RevRule = pair.reviews.Name
		if re := pair.reviews.RequiredReviews(); re != nil {
			rs.Approvals = re.MinApprovals
			rs.DismissStale = re.DismissStale
		}
		if rs.Refs == "" {
			rs.RefPatterns = append([]string{}, pair.reviews.Match.Refs...)
			rs.Refs = strings.Join(rs.RefPatterns, ", ")
		}
	}
	rs.ReadOnly = settingsRulesetReadOnlyReason(owner, repo, pair)
	rs.Editable = rs.ReadOnly == ""
	rs.Summary = settingsRulesetSummary(rs)
	return rs
}

// settingsRulesetSummary is the row's chip list: what the ruleset does, in
// the words the effective view uses.
func settingsRulesetSummary(rs SettingsRuleset) []string {
	var out []string
	ops := []string{policy.OpCreate, policy.OpUpdate}
	if rs.BlockForce {
		ops = append(ops, policy.OpForcePush)
	}
	if rs.BlockDelete {
		ops = append(ops, policy.OpDelete)
	}
	out = append(out, "protects "+strings.Join(ops, ", "))
	if len(rs.PushRoles) > 0 {
		out = append(out, "push bypass: "+strings.Join(rs.PushRoles, ", "))
	}
	if len(rs.Checks) > 0 {
		out = append(out, "checks: "+strings.Join(rs.Checks, ", "))
	}
	if rs.Approvals > 0 {
		label := strconv.Itoa(rs.Approvals) + " approval"
		if rs.Approvals > 1 {
			label += "s"
		}
		if rs.DismissStale {
			label += " (stale dismissed)"
		}
		out = append(out, label)
	}
	if rs.RequireUpToDate {
		out = append(out, "up-to-date head required")
	}
	if len(rs.MergeMethods) > 0 {
		out = append(out, "merge methods: "+strings.Join(rs.MergeMethods, ", "))
	}
	return out
}

// settingsPolicyRuleRow renders one rule for the read-only tables.
func settingsPolicyRuleRow(r *policy.Rule) SettingsPolicyRule {
	row := SettingsPolicyRule{Name: r.Name, Refs: strings.Join(r.Match.Refs, ", "), Scoped: policy.PrincipalScoped(r)}
	if row.Refs == "" {
		row.Refs = "all refs"
	}
	if _, _, ok := settingsRulesetSlugFromRule(r.Name); ok {
		row.Managed = true
	}
	switch e := r.Effect.(type) {
	case *policy.ProtectEffect:
		parts := []string{"restricts " + strings.Join(e.RestrictOps, ", ")}
		if len(e.Bypass) > 0 {
			parts = append(parts, "bypass "+strings.Join(e.Bypass, ", "))
		}
		if len(e.RequireChecks) > 0 {
			parts = append(parts, "requires checks "+strings.Join(e.RequireChecks, ", "))
		}
		if e.RequireUpToDate {
			parts = append(parts, "requires an up-to-date head")
		}
		if len(e.MergeMethods) > 0 {
			parts = append(parts, "merge methods "+strings.Join(e.MergeMethods, ", "))
		}
		row.Summary = strings.Join(parts, "; ")
	case *policy.RequiredReviewsEffect:
		row.Summary = "requires " + strconv.Itoa(e.MinApprovals) + " approvals"
		if e.DismissStale {
			row.Summary += ", stale approvals dismissed"
		}
		if len(e.Bypass) > 0 {
			row.Summary += ", bypass " + strings.Join(e.Bypass, ", ")
		}
	default:
		row.Summary = r.Effect.Kind()
	}
	return row
}

// settingsProtectionView parses the stored document into the tab's model:
// every ruleset (document order), the form vocabulary, and the read-only
// rule table. A parse error keeps the raw bytes visible and disables the
// form (fail closed: an unreadable policy is never edited through a form
// that would rewrite it from a guess).
func settingsProtectionView(raw []byte, owner, repo string) *SettingsProtection {
	out := &SettingsProtection{
		Owner: owner, Repo: repo,
		Roles:   settingsProtectionRoles,
		Methods: settingsMergeMethodOptions,
		Raw:     string(raw),
	}
	doc, err := policy.Parse(raw)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.doc = doc
	for i, pair := range settingsRulesetPairs(doc) {
		out.Rulesets = append(out.Rulesets, settingsRulesetFromPair(owner, repo, pair, i))
	}
	out.HasManaged = len(out.Rulesets) > 0
	for i := range out.Rulesets {
		out.Rulesets[i].CanMoveUp = i > 0
		out.Rulesets[i].CanMoveDown = i < len(out.Rulesets)-1
	}
	for _, r := range doc.Rules {
		row := settingsPolicyRuleRow(r)
		row.ReadOnly = settingsRuleReadOnly(out.Rulesets, r.Name)
		out.Rules = append(out.Rules, row)
	}
	return out
}

// settingsRuleReadOnly reports whether a rule belongs to a ruleset the form
// cannot express (or to no ruleset pair at all while carrying a
// protection-shaped name). The table marks those rows so an operator knows
// why editing happens in the raw policy.
func settingsRuleReadOnly(rulesets []SettingsRuleset, name string) bool {
	slug, _, ok := settingsRulesetSlugFromRule(name)
	if !ok {
		if strings.HasPrefix(name, settingsProtectionPrefix) {
			return true // a protection-shaped name that is not a pair
		}
		return false
	}
	for i := range rulesets {
		if rulesets[i].Slug == slug {
			return !rulesets[i].Editable
		}
	}
	return true
}

// settingsEffectiveRef picks the ref the effective view answers for: the
// explicit ?ref= argument, else the first stored pattern, else main.
func settingsEffectiveRef(p *SettingsProtection, ref string) string {
	if ref = strings.TrimSpace(ref); ref != "" {
		return ref
	}
	for _, rs := range p.Rulesets {
		if len(rs.RefPatterns) > 0 {
			return rs.RefPatterns[0]
		}
	}
	return "refs/heads/main"
}

// settingsEffectiveView computes what the stored document actually does to
// ref for principal. Push verdicts come from policy.EvaluateProtect (the
// server's own publisher check), merge demands from policy.MergeGateFor
// (the merge task's resolver) plus the reviews/checks unions — never from a
// second interpretation of the JSON.
func settingsEffectiveView(ctx context.Context, doc *policy.Document, principal, ref string) SettingsEffective {
	eff := SettingsEffective{Ref: ref, Principal: principal}
	if doc == nil {
		return eff
	}
	eff.HasRules = len(doc.Rules) > 0
	for _, op := range []string{policy.OpCreate, policy.OpUpdate, policy.OpDelete, policy.OpForcePush} {
		v := policy.EvaluateProtect(ctx, doc, policy.Request{Principal: principal, Ref: ref, Op: op})
		row := SettingsEffectiveOp{Op: op, Allowed: v.Allow, Verdict: "allowed"}
		if !v.Allow {
			row.Rule = v.Rule
			row.Verdict = "protected by '" + v.Rule + "'"
		}
		eff.Ops = append(eff.Ops, row)
	}
	for _, r := range doc.Rules {
		if !policy.AppliesToRef(r, ref) {
			continue
		}
		eff.Rules = append(eff.Rules, settingsPolicyRuleRow(r))
		re := r.RequiredReviews()
		if re == nil {
			continue
		}
		if policy.Bypassed(re.Bypass, principal, nil, doc.Roster()) {
			eff.ReviewsBypassed = true
			continue
		}
		if re.MinApprovals > eff.Approvals {
			eff.Approvals = re.MinApprovals
			eff.ReviewsRule = r.Name
		}
		eff.DismissStale = eff.DismissStale || re.DismissStale
	}
	eff.Checks, _ = requiredChecks(doc, ref)
	gate := policy.MergeGateFor(doc, policy.Request{Principal: principal, Ref: ref, Op: policy.OpUpdate})
	eff.UpToDate = gate.RequireUpToDate
	eff.UpToDateRules = gate.Rules
	eff.MethodsNarrowed = len(gate.Methods) > 0
	eff.Methods = gate.MethodsOrAll()
	return eff
}

// Select renders one state of the tab: slug "" is the list plus the
// effective view; "new" is the empty form; anything else is that ruleset's
// form. ref scopes the effective view.
func (p *SettingsProtection) Select(slug, ref, principal string) {
	if p == nil {
		return
	}
	if p.doc == nil {
		return // unreadable policy: raw editor only
	}
	p.Editing, p.New = nil, false
	if slug == "" {
		p.RefArg = settingsEffectiveRef(p, ref)
		p.Effective = settingsEffectiveView(context.Background(), p.doc, principal, p.RefArg)
		return
	}
	if slug == settingsProtectionNewSlug {
		p.New = true
		p.Editing = &SettingsRuleset{Enabled: true, Editable: true, BlockForce: true, BlockDelete: true}
		return
	}
	for i := range p.Rulesets {
		if p.Rulesets[i].Slug == slug {
			p.Editing = &p.Rulesets[i]
			return
		}
	}
	p.Error = "no ruleset named " + strconv.Quote(slug)
}

// --- form input ---------------------------------------------------------------

// settingsRulesetInput is the validated form payload. Action is one of
// save|delete|up|down ("" = save).
type settingsRulesetInput struct {
	// Owner/Repo scope the bypass role spellings this repo owns.
	Owner           string
	Repo            string
	Action          string
	Slug            string // identity: the path's ruleset ("" or "new" = create)
	NewSlug         string // the name field (rename, or the new ruleset's name)
	Enabled         bool
	Refs            []string
	Approvals       int
	DismissStale    bool
	Checks          []string
	PushRoles       []string
	BlockForce      bool
	BlockDelete     bool
	RequireUpToDate bool
	MergeMethods    []string
}

// settingsRulesetInputFrom reads the submitted form.
func settingsRulesetInputFrom(w SettingsRepoWrite) settingsRulesetInput {
	in := settingsRulesetInput{
		Action:          strings.TrimSpace(w.Get("ruleset_action")),
		Slug:            strings.TrimSpace(w.Get("id")),
		NewSlug:         strings.TrimSpace(w.Get("ruleset_slug")),
		Enabled:         w.Get("enabled") != "",
		Refs:            settingsSplitList(w.Get("refs")),
		Approvals:       settingsAtoiOr(w.Get("approvals"), 0),
		DismissStale:    w.Get("dismiss_stale") != "",
		Checks:          settingsSplitList(w.Get("checks")),
		PushRoles:       append([]string{}, w.Multi["push_roles"]...),
		BlockForce:      w.Get("block_force") != "",
		BlockDelete:     w.Get("block_delete") != "",
		RequireUpToDate: w.Get("require_up_to_date") != "",
		MergeMethods:    append([]string{}, w.Multi["merge_methods"]...),
	}
	if in.Slug == settingsProtectionNewSlug {
		in.Slug = ""
	}
	switch in.Action {
	case "", "save", "delete", "up", "down":
	default:
		in.Action = "save" // unknown verbs are a save; validation owns the rest
	}
	if in.Action == "" {
		in.Action = "save"
	}
	return in
}

// settingsRulesetValidate checks the form payload. Errors are the operator's
// words, not the parser's: the same document is re-parsed by PutPolicy.
func settingsRulesetValidate(in settingsRulesetInput) error {
	slug := in.NewSlug
	if slug == "" {
		slug = in.Slug
	}
	switch {
	case slug == "":
		return errors.New("a ruleset needs a name (1–39 characters: a-z, 0-9, -)")
	case !settingsRulesetSlugRe.MatchString(slug):
		return errors.New("ruleset names are 1–39 characters of a-z, 0-9 and - (for example: main-protection)")
	}
	if !in.Enabled {
		return nil // removal needs no further validation
	}
	if len(in.Refs) == 0 {
		return errors.New("a ruleset needs at least one ref pattern (for example refs/heads/main)")
	}
	if in.Approvals < 0 || in.Approvals > 50 {
		return errors.New("required approvals must be between 0 and 50")
	}
	if len(in.Checks) > 32 {
		return errors.New("at most 32 required status checks")
	}
	seen := map[string]bool{}
	for _, c := range in.Checks {
		if err := policy.ValidCheckContext(c); err != nil {
			return err
		}
		if seen[c] {
			return errors.New("duplicate status check " + strconv.Quote(c))
		}
		seen[c] = true
	}
	for _, r := range in.PushRoles {
		if !settingsRoleKnown(r) {
			return errors.New("unknown push role " + strconv.Quote(r))
		}
	}
	if len(in.MergeMethods) == 0 {
		return errors.New("select at least one merge method (merge, squash or rebase)")
	}
	seen = map[string]bool{}
	for _, m := range in.MergeMethods {
		if !settingsMergeMethodKnown(m) {
			return errors.New("unknown merge method " + strconv.Quote(m))
		}
		if seen[m] {
			return errors.New("duplicate merge method " + strconv.Quote(m))
		}
		seen[m] = true
	}
	return nil
}

func settingsMergeMethodKnown(method string) bool {
	for _, m := range policy.MergeMethodsAll() {
		if m == method {
			return true
		}
	}
	return false
}

// settingsRulesetRules builds the pair's two rule objects (push always,
// reviews when approvals > 0). An all-methods selection stores no
// merge_methods key (absent is the universe, so the document stays minimal
// and older readers keep the union semantics).
func settingsRulesetRules(in settingsRulesetInput) ([]map[string]any, error) {
	slug := in.NewSlug
	if slug == "" {
		slug = in.Slug
	}
	pushName, reviewsName := settingsRulesetRuleNames(slug)
	restricts := []string{policy.OpCreate, policy.OpUpdate}
	if in.BlockForce {
		restricts = append(restricts, policy.OpForcePush)
	}
	if in.BlockDelete {
		restricts = append(restricts, policy.OpDelete)
	}
	bypass := make([]string, 0, len(in.PushRoles))
	for _, role := range in.PushRoles {
		bypass = append(bypass, "role:"+in.Owner+"/"+in.Repo+":"+role)
	}
	protect := map[string]any{"restricts": restricts, "bypass": bypass}
	if len(in.Checks) > 0 {
		protect["require_checks"] = in.Checks
	}
	if in.RequireUpToDate {
		protect["require_up_to_date"] = true
	}
	methods := settingsNormalizedMethods(in.MergeMethods)
	if len(methods) > 0 {
		protect["merge_methods"] = methods
	}
	rules := []map[string]any{{
		"name":   pushName,
		"match":  map[string]any{"refs": in.Refs},
		"effect": map[string]any{"protect": protect},
	}}
	if in.Approvals > 0 {
		rules = append(rules, map[string]any{
			"name":  reviewsName,
			"match": map[string]any{"refs": in.Refs},
			"effect": map[string]any{"required-reviews": map[string]any{
				"min_approvals": in.Approvals,
				"dismiss_stale": in.DismissStale,
			}},
		})
	}
	return rules, nil
}

// settingsNormalizedMethods returns the selected methods in the universe's
// order, or nil when every method is selected (absent = universe).
func settingsNormalizedMethods(selected []string) []string {
	var out []string
	for _, m := range policy.MergeMethodsAll() {
		if containsStr(selected, m) {
			out = append(out, m)
		}
	}
	if len(out) == len(policy.MergeMethodsAll()) {
		return nil
	}
	return out
}

// --- document apply --------------------------------------------------------

// settingsProtectionItem is one rule block of the rewritten document: a
// ruleset pair (two raws, emitted adjacent) or a single rule the form does
// not own (bytes preserved verbatim).
type settingsProtectionItem struct {
	slug string
	raws []json.RawMessage
}

// settingsProtectionItems splits the stored rules into movable ruleset
// blocks and untouched single rules, in document order.
func settingsProtectionItems(raws []json.RawMessage) ([]settingsProtectionItem, error) {
	var items []settingsProtectionItem
	index := map[string]int{}
	for _, raw := range raws {
		var head struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, errors.New("the stored policy has an unreadable rule — fix it in the advanced editor first")
		}
		slug, _, ok := settingsRulesetSlugFromRule(head.Name)
		if !ok {
			items = append(items, settingsProtectionItem{raws: []json.RawMessage{raw}})
			continue
		}
		if i, seen := index[slug]; seen {
			items[i].raws = append(items[i].raws, raw)
			continue
		}
		index[slug] = len(items)
		items = append(items, settingsProtectionItem{slug: slug, raws: []json.RawMessage{raw}})
	}
	return items, nil
}

func settingsItemIndex(items []settingsProtectionItem, slug string) int {
	for i := range items {
		if items[i].slug == slug {
			return i
		}
	}
	return -1
}

// settingsMoveRuleset swaps a ruleset block with the nearest neighbouring
// ruleset block (single rules stay put — their order carries no priority the
// form knows). Reports whether anything moved.
func settingsMoveRuleset(items []settingsProtectionItem, slug string, up bool) bool {
	i := settingsItemIndex(items, slug)
	if i < 0 {
		return false
	}
	j := -1
	if up {
		for k := i - 1; k >= 0; k-- {
			if items[k].slug != "" {
				j = k
				break
			}
		}
	} else {
		for k := i + 1; k < len(items); k++ {
			if items[k].slug != "" {
				j = k
				break
			}
		}
	}
	if j < 0 {
		return false
	}
	items[i], items[j] = items[j], items[i]
	return true
}

// settingsProtectionApply rewrites the stored document for one ruleset
// operation. It preserves the version, the groups, every unowned rule's
// bytes, and any envelope keys this form does not model are re-emitted from
// the raw envelope. The result is validated by policy.Parse before it is
// returned, so a form bug can never store an invalid document.
func settingsProtectionApply(current []byte, owner, repo string, in settingsRulesetInput) ([]byte, []string, error) {
	type envelope struct {
		Version int               `json:"version"`
		Groups  json.RawMessage   `json:"groups,omitempty"`
		Rules   []json.RawMessage `json:"rules"`
	}
	env := envelope{Version: 1}
	if len(bytes.TrimSpace(current)) > 0 {
		if err := json.Unmarshal(current, &env); err != nil {
			return nil, nil, errors.New("the stored policy is not valid JSON — fix it in the advanced editor first")
		}
		if env.Version == 0 {
			env.Version = 1
		}
	}
	// The current document must parse: every read-only decision below is
	// taken from the parsed form, never from the raw bytes.
	doc, perr := policy.Parse(current)
	if perr != nil && len(bytes.TrimSpace(current)) > 0 {
		return nil, nil, fmt.Errorf("the stored policy does not parse (%v) — fix it in the advanced editor first", perr)
	}
	if doc == nil {
		doc = &policy.Document{Version: 1}
	}
	items, err := settingsProtectionItems(env.Rules)
	if err != nil {
		return nil, nil, err
	}
	var warnings []string

	switch in.Action {
	case "up", "down":
		if !settingsMoveRuleset(items, in.Slug, in.Action == "up") {
			return nil, nil, fmt.Errorf("ruleset %s cannot move %s (it is first or last)", strconv.Quote(in.Slug), in.Action)
		}
	default:
		idx := -1
		if in.Slug != "" {
			idx = settingsItemIndex(items, in.Slug)
		}
		if idx >= 0 {
			if reason := settingsPairReadOnly(doc, owner, repo, in.Slug); reason != "" {
				return nil, nil, fmt.Errorf("ruleset %s is read-only: %s", strconv.Quote(in.Slug), reason)
			}
		}
		if in.Action == "delete" || !in.Enabled {
			// Deleting, and unchecking "enabled" on the form, both remove the
			// pair: a ruleset is enforced by being in the document (the
			// pre-ruleset tab's semantics). Removal is the one operation a
			// read-only pair also refuses above — its bytes are not ours to
			// drop from a form that cannot reproduce them.
			if idx < 0 {
				return nil, nil, fmt.Errorf("no ruleset named %s", strconv.Quote(in.NewSlug))
			}
			items = append(items[:idx], items[idx+1:]...)
			break
		}
		if err := settingsRulesetValidate(in); err != nil {
			return nil, nil, err
		}
		rules, rerr := settingsRulesetRules(in)
		if rerr != nil {
			return nil, nil, rerr
		}
		raws := make([]json.RawMessage, 0, len(rules))
		for _, rule := range rules {
			raw, merr := json.Marshal(rule)
			if merr != nil {
				return nil, nil, merr
			}
			raws = append(raws, raw)
		}
		slug := in.NewSlug
		if slug == "" {
			slug = in.Slug
		}
		if idx < 0 {
			// A new ruleset: the name must be free.
			if settingsItemIndex(items, slug) >= 0 {
				return nil, nil, fmt.Errorf("a ruleset named %s already exists", strconv.Quote(slug))
			}
			items = append(items, settingsProtectionItem{slug: slug, raws: raws})
		} else {
			// A save (possibly a rename) keeps the ruleset's position.
			if slug != in.Slug && settingsItemIndex(items, slug) >= 0 {
				return nil, nil, fmt.Errorf("a ruleset named %s already exists", strconv.Quote(slug))
			}
			if in.Slug == settingsProtectionDefaultSlug {
				warnings = append(warnings, "the default ruleset's rules were renamed to protection:default-push / protection:default-reviews")
			}
			items[idx] = settingsProtectionItem{slug: slug, raws: raws}
		}
	}

	env.Rules = env.Rules[:0]
	for _, item := range items {
		env.Rules = append(env.Rules, item.raws...)
	}
	if env.Rules == nil {
		env.Rules = []json.RawMessage{}
	}
	out, merr := json.Marshal(env)
	if merr != nil {
		return nil, nil, merr
	}
	if _, perr := policy.Parse(out); perr != nil {
		return nil, nil, perr
	}
	return out, warnings, nil
}

// settingsPairReadOnly recomputes a stored pair's read-only reason (the save
// path's guard, independent of the view the page rendered).
func settingsPairReadOnly(doc *policy.Document, owner, repo, slug string) string {
	for _, pair := range settingsRulesetPairs(doc) {
		if pair.slug == slug {
			return settingsRulesetReadOnlyReason(owner, repo, pair)
		}
	}
	return ""
}

// settingsProtectionNotice is the post-write notice for one ruleset verb.
func settingsProtectionNotice(in settingsRulesetInput) string {
	slug := in.NewSlug
	if slug == "" {
		slug = in.Slug
	}
	switch {
	case in.Action == "up" || in.Action == "down":
		return "ruleset order saved"
	case in.Action == "delete":
		return "ruleset " + strconv.Quote(slug) + " removed"
	case !in.Enabled:
		return "ruleset " + strconv.Quote(slug) + " disabled — its rules were removed"
	default:
		return "ruleset " + strconv.Quote(slug) + " saved"
	}
}
