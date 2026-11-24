package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/api"
	git "github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/policy"
)

// PLAN-V2 §2.4 (P1) and §2.2 (P2) page-layer contracts: the ruleset model,
// lossless round-trip, ordering, the effective-rules matrix, and the merge
// box's allowed methods / up-to-date affordance.

// rulesetDoc is the fixture document used across these tests: one editable
// ruleset, one read-only pair (a principal bypass the form does not own), a
// protection-shaped rule that is not a pair, and an unrelated rule.
const rulesetDoc = `{"version":1,"groups":[{"name":"sre","members":["ops@example.com","sam@example.com"]}],"rules":[
	{"name":"protection:main-push","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["create","update"],"bypass":["role:acme/monorepo:admin"],"require_checks":["ci/build"],"require_up_to_date":true,"merge_methods":["squash"]}}},
	{"name":"protection:main-reviews","match":{"refs":["refs/heads/main"]},"effect":{"required-reviews":{"min_approvals":2,"dismiss_stale":true}}},
	{"name":"protection:bot-push","match":{"refs":["refs/heads/bot/*"],"principals":["group:sre"]},"effect":{"protect":{"restricts":["update"],"bypass":["ops@example.com"]}}},
	{"name":"protection:odd","match":{"refs":["refs/heads/odd"]},"effect":{"protect":{"restricts":["delete"]}}},
	{"name":"release-tags","match":{"refs":["refs/tags/v*"]},"effect":{"protect":{"restricts":["delete"]}}}
]}`

func rulesetInput(owner, repo string) settingsRulesetInput {
	return settingsRulesetInput{
		Owner: owner, Repo: repo,
		Action: "save", Slug: "main", NewSlug: "main", Enabled: true,
		Refs: []string{"refs/heads/main"}, Approvals: 1,
		Checks: []string{"ci/build"}, PushRoles: []string{"maintain"},
		BlockForce: true, BlockDelete: true,
		MergeMethods: []string{"merge", "squash", "rebase"},
	}
}

func TestRulesetViewAndPairs(t *testing.T) {
	t.Parallel()
	v := settingsProtectionView([]byte(rulesetDoc), "acme", "monorepo")
	if v.Error != "" {
		t.Fatalf("view error: %s", v.Error)
	}
	var slugs []string
	for _, rs := range v.Rulesets {
		slugs = append(slugs, rs.Slug)
	}
	if strings.Join(slugs, ",") != "main,bot" {
		t.Fatalf("rulesets = %v", slugs)
	}
	main, bot := v.Rulesets[0], v.Rulesets[1]
	if !main.Editable || main.ReadOnly != "" {
		t.Fatalf("main must be editable: %+v", main)
	}
	if main.Approvals != 2 || !main.DismissStale || len(main.Checks) != 1 || !main.RequireUpToDate || len(main.MergeMethods) != 1 {
		t.Fatalf("main fields = %+v", main)
	}
	if strings.Join(main.PushRoles, ",") != "admin" {
		t.Fatalf("main bypass roles = %v", main.PushRoles)
	}
	if bot.Editable || !strings.Contains(bot.ReadOnly, "principals") {
		t.Fatalf("bot must be read-only: %+v", bot)
	}
	if !v.HasManaged || len(v.Rules) != 5 {
		t.Fatalf("hasManaged=%v rules=%d", v.HasManaged, len(v.Rules))
	}
	// The rule table marks the unowned rows; every rule is present.
	readonly := map[string]bool{}
	for _, row := range v.Rules {
		readonly[row.Name] = row.ReadOnly
	}
	if !readonly["protection:bot-push"] || !readonly["protection:odd"] || readonly["protection:main-push"] || readonly["release-tags"] {
		t.Fatalf("read-only marks = %v", readonly)
	}
	// Ordering bits.
	if main.CanMoveUp || !main.CanMoveDown || !bot.CanMoveUp || bot.CanMoveDown {
		t.Fatalf("move bits = %+v / %+v", main, bot)
	}
}

func TestRulesetSummaryChips(t *testing.T) {
	t.Parallel()
	rs := SettingsRuleset{
		BlockForce: true, BlockDelete: true, PushRoles: []string{"maintain"},
		Checks: []string{"ci/build"}, Approvals: 2, DismissStale: true,
		RequireUpToDate: true, MergeMethods: []string{"squash"},
	}
	got := strings.Join(settingsRulesetSummary(rs), " | ")
	for _, want := range []string{
		"protects create, update, force-push, delete",
		"push bypass: maintain",
		"checks: ci/build",
		"2 approvals (stale dismissed)",
		"up-to-date head required",
		"merge methods: squash",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary %q misses %q", got, want)
		}
	}
}

// TestRulesetRoundTripLossless pins the promise: saving one ruleset rewrites
// only its two rules; a pair the form cannot express keeps its exact bytes
// and position, and unrelated rules/groups survive.
func TestRulesetRoundTripLossless(t *testing.T) {
	t.Parallel()
	in := rulesetInput("acme", "monorepo")
	in.Approvals = 3
	in.Checks = []string{"ci/build", "lint"}
	in.RequireUpToDate = true
	in.MergeMethods = []string{"squash"}
	out, warnings, err := settingsProtectionApply([]byte(rulesetDoc), "acme", "monorepo", in)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	doc, perr := policy.Parse(out)
	if perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	push := doc.RuleByName("protection:main-push")
	if push == nil {
		t.Fatalf("main push rule missing: %s", out)
	}
	pe := push.Protect()
	if pe == nil || !pe.RequireUpToDate || strings.Join(pe.MergeMethods, ",") != "squash" ||
		len(pe.RequireChecks) != 2 {
		t.Fatalf("main protect = %+v", pe)
	}
	if rev := doc.RuleByName("protection:main-reviews"); rev == nil || rev.RequiredReviews().MinApprovals != 3 {
		t.Fatalf("main reviews = %+v", rev)
	}
	// The unexpressible pair is byte-identical and still in place.
	rawBot := rawRule(t, out, "protection:bot-push")
	wantBot := rawRule(t, []byte(rulesetDoc), "protection:bot-push")
	if rawBot != wantBot {
		t.Fatalf("read-only rule rewritten:\n got %s\nwant %s", rawBot, wantBot)
	}
	// Position: main pair, bot pair, odd, release-tags (document order kept).
	var names []string
	for _, r := range doc.Rules {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != "protection:main-push,protection:main-reviews,protection:bot-push,protection:odd,release-tags" {
		t.Fatalf("rule order = %v", names)
	}
	// The roster survives.
	if _, ok := doc.Roster().Lookup("sre"); !ok {
		t.Fatal("groups dropped")
	}
}

// rawRule returns a rule's exact JSON text from a document (compact form).
func rawRule(t *testing.T, doc []byte, name string) string {
	t.Helper()
	var env struct {
		Rules []json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal(doc, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, raw := range env.Rules {
		var head struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			t.Fatalf("rule head: %v", err)
		}
		if head.Name == name {
			return string(raw)
		}
	}
	return ""
}

// TestRulesetReadOnlyRefused pins fail-closed: the server refuses to write
// over (or delete) a pair the form cannot express, and the store is
// untouched.
func TestRulesetReadOnlyRefused(t *testing.T) {
	t.Parallel()
	in := rulesetInput("acme", "monorepo")
	in.Slug = "bot"
	in.NewSlug = "bot"
	for _, action := range []string{"save", "delete"} {
		in.Action = action
		out, _, err := settingsProtectionApply([]byte(rulesetDoc), "acme", "monorepo", in)
		if err == nil {
			t.Fatalf("%s on a read-only ruleset must be refused: %s", action, out)
		}
		if !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("%s error = %v", action, err)
		}
	}
	// Disabling through the enabled checkbox is the same refusal.
	in.Action, in.Enabled = "save", false
	if _, _, err := settingsProtectionApply([]byte(rulesetDoc), "acme", "monorepo", in); err == nil {
		t.Fatal("disabling a read-only ruleset must be refused")
	}
}

func TestRulesetOrdering(t *testing.T) {
	t.Parallel()
	in := settingsRulesetInput{Action: "down", Slug: "main"}
	out, _, err := settingsProtectionApply([]byte(rulesetDoc), "acme", "monorepo", in)
	if err != nil {
		t.Fatalf("down: %v", err)
	}
	doc, _ := policy.Parse(out)
	var names []string
	for _, r := range doc.Rules {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != "protection:bot-push,protection:main-push,protection:main-reviews,protection:odd,release-tags" {
		t.Fatalf("down order = %v", names)
	}
	// Up restores the original order.
	in = settingsRulesetInput{Action: "up", Slug: "main"}
	out, _, err = settingsProtectionApply(out, "acme", "monorepo", in)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	doc, _ = policy.Parse(out)
	names = nil
	for _, r := range doc.Rules {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != "protection:main-push,protection:main-reviews,protection:bot-push,protection:odd,release-tags" {
		t.Fatalf("up order = %v", names)
	}
	// The first ruleset cannot move up; the last cannot move down.
	if _, _, err := settingsProtectionApply([]byte(rulesetDoc), "acme", "monorepo", settingsRulesetInput{Action: "up", Slug: "main"}); err == nil {
		t.Fatal("first ruleset moved up")
	}
	if _, _, err := settingsProtectionApply([]byte(rulesetDoc), "acme", "monorepo", settingsRulesetInput{Action: "down", Slug: "bot"}); err == nil {
		t.Fatal("last ruleset moved down")
	}
}

func TestRulesetCreateRenameAndDuplicates(t *testing.T) {
	t.Parallel()
	// A new ruleset appends at the end.
	in := rulesetInput("acme", "monorepo")
	in.Slug, in.NewSlug = "", "release"
	in.Refs = []string{"refs/heads/release/*"}
	out, _, err := settingsProtectionApply([]byte(rulesetDoc), "acme", "monorepo", in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	doc, _ := policy.Parse(out)
	n := len(doc.Rules)
	if n < 2 || doc.Rules[n-2].Name != "protection:release-push" || doc.Rules[n-1].Name != "protection:release-reviews" {
		t.Fatalf("appended rules = %v / %v", doc.Rules[n-2].Name, doc.Rules[n-1].Name)
	}
	// A duplicate name is refused inline.
	dup := in
	if _, _, err := settingsProtectionApply(out, "acme", "monorepo", dup); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate error = %v", err)
	}
	// A rename keeps the ruleset's position.
	ren := in
	ren.Slug, ren.NewSlug = "release", "stable"
	ren.Refs = []string{"refs/heads/stable/*"}
	out2, _, err := settingsProtectionApply(out, "acme", "monorepo", ren)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	doc, _ = policy.Parse(out2)
	n = len(doc.Rules)
	if doc.Rules[n-2].Name != "protection:stable-push" || doc.Rules[n-1].Name != "protection:stable-reviews" {
		t.Fatalf("rename moved the ruleset: %v / %v", doc.Rules[n-2].Name, doc.Rules[n-1].Name)
	}
	if doc.RuleByName("protection:release-push") != nil {
		t.Fatal("renamed rules left behind")
	}
	// Renaming onto an existing slug is refused.
	ren.NewSlug = "main"
	if _, _, err := settingsProtectionApply(out2, "acme", "monorepo", ren); err == nil {
		t.Fatal("rename collision accepted")
	}
}

func TestProtectionApplyUnparseableDocument(t *testing.T) {
	if _, _, err := settingsProtectionApply([]byte(`{"version":1,"rules":[{"name":"x"`), "acme", "monorepo", rulesetInput("acme", "monorepo")); err == nil {
		t.Fatal("unparseable stored policy must refuse an edit")
	}
	if _, _, err := settingsProtectionApply([]byte(`[]`), "acme", "monorepo", rulesetInput("acme", "monorepo")); err == nil {
		t.Fatal("a non-object envelope must refuse an edit")
	}
}

// TestEffectiveViewMatrix is the effective-rules contract: for a matrix of
// refs, the push verdict per op and the merge demands are the engine's own
// answers.
func TestEffectiveViewMatrix(t *testing.T) {
	t.Parallel()
	doc, err := policy.Parse([]byte(rulesetDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ctx := context.Background()

	main := settingsEffectiveView(ctx, doc, "carol", "refs/heads/main")
	if !main.HasRules || len(main.Ops) != 4 {
		t.Fatalf("main view = %+v", main)
	}
	for _, op := range main.Ops {
		if op.Op == policy.OpCreate || op.Op == policy.OpUpdate {
			if op.Allowed || op.Rule != "protection:main-push" {
				t.Fatalf("%s verdict = %+v", op.Op, op)
			}
		} else if !op.Allowed {
			t.Fatalf("%s should be allowed: %+v", op.Op, op)
		}
	}
	if main.Approvals != 2 || !main.DismissStale || main.ReviewsRule != "protection:main-reviews" {
		t.Fatalf("reviews = %+v", main)
	}
	if strings.Join(main.Checks, ",") != "ci/build" {
		t.Fatalf("checks = %v", main.Checks)
	}
	if !main.UpToDate || !main.MethodsNarrowed || strings.Join(main.Methods, ",") != "squash" {
		t.Fatalf("merge demands = %+v", main)
	}

	// A group member who is NOT in the rule's bypass list is denied; the
	// bypassed member is admitted (the per-rule bypass law).
	sam := settingsEffectiveView(ctx, doc, "sam@example.com", "refs/heads/bot/x")
	if sam.Ops[1].Allowed || sam.Ops[1].Rule != "protection:bot-push" {
		t.Fatalf("unbypassed bot update = %+v", sam.Ops[1])
	}
	bot := settingsEffectiveView(ctx, doc, "ops@example.com", "refs/heads/bot/x")
	if !bot.Ops[1].Allowed {
		t.Fatalf("bypassed bot update = %+v", bot.Ops[1])
	}
	if bot.UpToDate || bot.MethodsNarrowed {
		t.Fatalf("bot must carry no merge demands: %+v", bot)
	}

	tags := settingsEffectiveView(ctx, doc, "carol", "refs/tags/v1")
	// release-tags restricts delete on refs/tags/v*; nothing else applies.
	if tags.Ops[2].Allowed || tags.Ops[2].Rule != "release-tags" {
		t.Fatalf("tag delete = %+v", tags.Ops[2])
	}
	if !tags.Ops[0].Allowed || !tags.Ops[1].Allowed || !tags.Ops[3].Allowed {
		t.Fatalf("tag ops = %+v", tags.Ops)
	}
	if len(tags.Rules) != 1 || tags.Rules[0].Name != "release-tags" {
		t.Fatalf("tag rules = %+v", tags.Rules)
	}
	if tags.UpToDate || tags.Approvals != 0 || len(tags.Checks) != 0 {
		t.Fatalf("tag demands = %+v", tags)
	}

	// A bypassed review rule is reported as bypassed, not as a demand.
	bypassDoc, _ := policy.Parse([]byte(`{"version":1,"rules":[
		{"name":"rev","match":{"refs":["refs/heads/main"]},"effect":{"required-reviews":{"min_approvals":3,"bypass":["carol"]}}}]}`))
	bypassed := settingsEffectiveView(ctx, bypassDoc, "carol", "refs/heads/main")
	if bypassed.Approvals != 0 || !bypassed.ReviewsBypassed {
		t.Fatalf("bypass view = %+v", bypassed)
	}

	// No document is allow-all and says so.
	empty := settingsEffectiveView(ctx, nil, "carol", "refs/heads/main")
	if empty.HasRules {
		t.Fatalf("empty view = %+v", empty)
	}
}

func TestProtectionSelect(t *testing.T) {
	t.Parallel()
	v := settingsProtectionView([]byte(rulesetDoc), "acme", "monorepo")
	v.Select("", "", "carol")
	if v.Editing != nil {
		t.Fatal("list view must not select a ruleset")
	}
	if v.Effective.Ref != "refs/heads/main" || v.Effective.Principal != "carol" {
		t.Fatalf("effective = %+v", v.Effective)
	}
	if v.RefArg != "refs/heads/main" {
		t.Fatalf("ref arg = %q", v.RefArg)
	}
	v.Select("", "refs/heads/bot/x", "carol")
	if v.Effective.Ref != "refs/heads/bot/x" {
		t.Fatalf("explicit ref ignored: %q", v.Effective.Ref)
	}
	v.Select("main", "", "carol")
	if v.Editing == nil || v.Editing.Slug != "main" {
		t.Fatalf("editing = %+v", v.Editing)
	}
	v.Select("new", "", "carol")
	if !v.New || v.Editing == nil || !v.Editing.Enabled || !v.Editing.BlockForce {
		t.Fatalf("new form = %+v", v.Editing)
	}
	v.Select("nope", "", "carol")
	if v.Editing != nil || !strings.Contains(v.Error, "no ruleset named") {
		t.Fatalf("unknown slug = %+v %q", v.Editing, v.Error)
	}
	// An unreadable document selects nothing (the raw editor is the path).
	bad := settingsProtectionView([]byte(`{"version":2}`), "acme", "monorepo")
	bad.Select("main", "", "carol")
	if bad.Editing != nil || bad.Error == "" {
		t.Fatalf("unreadable doc = %+v %q", bad.Editing, bad.Error)
	}
}

func TestProtectionHelpers(t *testing.T) {
	t.Parallel()
	rs := &SettingsRuleset{MergeMethods: []string{"squash"}}
	if !rs.MergeMethodChecked("squash") || rs.MergeMethodChecked("merge") {
		t.Fatal("MergeMethodChecked narrow set")
	}
	var empty *SettingsRuleset
	if empty.MergeMethodChecked("merge") {
		t.Fatal("nil receiver must not check")
	}
	all := &SettingsRuleset{}
	if !all.MergeMethodChecked("rebase") {
		t.Fatal("empty set means every method")
	}
	p := &SettingsProtection{Editing: &SettingsRuleset{PushRoles: []string{"write"}}}
	if !p.PushRoleChecked("write") || p.PushRoleChecked("admin") {
		t.Fatal("PushRoleChecked")
	}
	if (&SettingsProtection{}).PushRoleChecked("write") {
		t.Fatal("no editing ruleset must check nothing")
	}
	if settingsMergeMethodKnown("octopus") {
		t.Fatal("octopus is not a merge method")
	}
	if got := strings.Join(settingsNormalizedMethods([]string{"rebase", "merge", "squash"}), ","); got != "" {
		t.Fatalf("all methods must store absent, got %q", got)
	}
	if got := strings.Join(settingsNormalizedMethods([]string{"rebase", "merge"}), ","); got != "merge,rebase" {
		t.Fatalf("normalized = %q", got)
	}
}

// TestSettingProtectionRulesBuild pins the generated effect fields, including
// that selecting every method stores no merge_methods key.
func TestSettingProtectionRulesBuild(t *testing.T) {
	t.Parallel()
	in := rulesetInput("acme", "monorepo")
	in.Approvals = 0
	in.MergeMethods = []string{"merge", "squash", "rebase"}
	rules, err := settingsRulesetRules(in)
	if err != nil {
		t.Fatalf("rules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("approvals 0 must not emit a reviews rule: %v", rules)
	}
	raw, _ := json.Marshal(rules[0])
	doc, perr := policy.Parse([]byte(`{"version":1,"rules":[` + string(raw) + `]}`))
	if perr != nil {
		t.Fatalf("generated rule unparseable: %v (%s)", perr, raw)
	}
	pe := doc.Rules[0].Protect()
	if pe == nil || pe.RequireUpToDate || len(pe.MergeMethods) != 0 {
		t.Fatalf("generated protect = %+v", pe)
	}
	if strings.Join(pe.Bypass, ",") != "role:acme/monorepo:maintain" {
		t.Fatalf("bypass = %v", pe.Bypass)
	}
	if strings.Contains(string(raw), `"merge_methods"`) {
		t.Fatalf("all-methods selection must omit merge_methods: %s", raw)
	}
}

// TestRulesetPagesRender drives the two renderings of the tab: the list and
// one ruleset's form, including the read-only refusal state.
func TestRulesetPagesRender(t *testing.T) {
	f := settingsRepoFakes("admin")
	raw := []byte(rulesetDoc)
	f.deps.RepoProtection = func(context.Context, string, string) (*SettingsProtection, error) {
		return settingsProtectionView(raw, "acme", "monorepo"), nil
	}
	s := newSettingsServer(t, f)

	// The list: one row per ruleset, the primary action, the effective view.
	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/protection?ref=refs/heads/bot/x", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Protection rulesets", `href="/acme/monorepo/settings/protection/new"`,
		`href="/acme/monorepo/settings/protection/main"`, "protects create, update",
		"read-only", "What applies to a ref", "refs/heads/bot/x",
	} {
		settingsMustContain(t, body, want, "list page")
	}
	if strings.Count(body, "btn primary") != 1 {
		t.Fatalf("exactly one primary action on the list: %d", strings.Count(body, "btn primary"))
	}

	// A ruleset's form: every effect field, a save button, the typed delete.
	rec = settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/protection/main", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("form = %d", rec.Code)
	}
	body = rec.Body.String()
	for _, want := range []string{
		`name="ruleset_slug" value="main"`, `name="refs" value="refs/heads/main"`,
		`name="approvals" type="number"`, `name="checks"`, `name="push_roles" value="admin"`,
		`name="require_up_to_date"`, `value="squash" class="h-4 w-4 check" checked`,
		"Save ruleset", `data-danger-expected="protection:main"`, "Remove ruleset",
	} {
		settingsMustContain(t, body, want, "ruleset form")
	}
	if strings.Count(body, "btn primary") != 1 {
		t.Fatalf("exactly one primary action on the form: %d", strings.Count(body, "btn primary"))
	}

	// The new-ruleset form: empty name, enabled, both blocks on.
	rec = settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/protection/new", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("new form = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "New ruleset", "new form heading")
	settingsMustContain(t, rec.Body.String(), `value="merge" class="h-4 w-4 check" checked`, "all methods default")

	// A read-only ruleset renders the refusal instead of a form.
	rec = settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/protection/bot", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("read-only form = %d", rec.Code)
	}
	body = rec.Body.String()
	settingsMustContain(t, body, "This ruleset is read-only", "read-only notice")
	settingsMustNotContain(t, body, `name="ruleset_action" value="save"`, "no form for a read-only ruleset")

	// An unknown slug is an inline error, not a blank form.
	rec = settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/protection/ghost", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown slug = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "no ruleset named", "unknown slug error")
}

// TestProtectionWriteDispatchesInSeam pins the seam verbs the page uses
// (save/delete/up/down), the typed delete confirmation and the redirects.
func TestProtectionWriteDispatchesInSeam(t *testing.T) {
	h := newSeamsHarness(t)
	ctx := context.Background()
	id := git.RepoId{Owner: "acme", Name: "monorepo"}
	if werr := api.PutPolicy(ctx, h.apiE, id, []byte(rulesetDoc)); werr != nil {
		t.Fatalf("seed: %v", werr)
	}
	// Delete without the typed value is refused before the store.
	code, _, body := seamsPost(t, h, "/acme/monorepo/settings/protection/main", url.Values{
		"ruleset_action": {"delete"},
	})
	if code != http.StatusBadRequest || !strings.Contains(body, "type protection:main exactly to confirm removal") {
		t.Fatalf("unconfirmed delete = %d (%s)", code, body)
	}
	// Confirmed delete removes the pair only.
	code, loc, body := seamsPost(t, h, "/acme/monorepo/settings/protection/main", url.Values{
		"ruleset_action": {"delete"}, "confirm": {"protection:main"},
	})
	if code != http.StatusSeeOther || !strings.Contains(loc, "/settings/protection") {
		t.Fatalf("delete = %d %q (%s)", code, loc, body)
	}
	raw, _, _ := api.PolicyRaw(ctx, h.apiE, id)
	if strings.Contains(string(raw), "protection:main-push") || strings.Contains(string(raw), "protection:main-reviews") {
		t.Fatalf("delete left rules: %s", raw)
	}
	if !strings.Contains(string(raw), "protection:bot-push") {
		t.Fatalf("delete dropped a read-only pair: %s", raw)
	}
	// Reordering the remaining ruleset is a no-op refusal (it is alone).
	code, _, body = seamsPost(t, h, "/acme/monorepo/settings/protection/bot", url.Values{
		"ruleset_action": {"up"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("move = %d (%s)", code, body)
	}
}

// TestRulesetReadOnlyReasons covers every reason the form declines a pair:
// each one must be read-only (never silently rewritten) and name itself.
func TestRulesetReadOnlyReasons(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		rules string
		want  string
	}{
		{"review rule without a push rule", `[
			{"name":"protection:x-reviews","match":{"refs":["refs/heads/main"]},"effect":{"required-reviews":{"min_approvals":1}}}]`,
			"no matching push rule"},
		{"push rule is not a protect effect", `[
			{"name":"protection:x-push","match":{"refs":["refs/heads/main"]},"effect":{"required-reviews":{"min_approvals":1}}}]`,
			"does not carry a protect effect"},
		{"push rule carries a mode", `[
			{"name":"protection:x-push","match":{"refs":["refs/heads/main"]},"mode":"audit","effect":{"protect":{}}}]`,
			"carries a mode"},
		{"push rule narrows on principals", `[
			{"name":"protection:x-push","match":{"refs":["refs/heads/main"],"principals":["alice"]},"effect":{"protect":{}}}]`,
			"principals or paths"},
		{"push rule does not restrict create and update", `[
			{"name":"protection:x-push","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["delete"]}}}]`,
			"does not restrict create and update"},
		{"push rule bypasses a foreign spelling", `[
			{"name":"protection:x-push","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"bypass":["group:sre"]}}}]`,
			"does not own"},
		{"push rule bypasses another repo's role", `[
			{"name":"protection:x-push","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"bypass":["role:other/repo:admin"]}}}]`,
			"does not own"},
		{"review rule carries a bypass", `[
			{"name":"protection:x-push","match":{"refs":["refs/heads/main"]},"effect":{"protect":{}}},
			{"name":"protection:x-reviews","match":{"refs":["refs/heads/main"]},"effect":{"required-reviews":{"min_approvals":1,"bypass":["alice"]}}}]`,
			"carries a bypass list"},
		{"rules match different refs", `[
			{"name":"protection:x-push","match":{"refs":["refs/heads/main"]},"effect":{"protect":{}}},
			{"name":"protection:x-reviews","match":{"refs":["refs/heads/dev"]},"effect":{"required-reviews":{"min_approvals":1}}}]`,
			"match different refs"},
		{"review rule carries a mode", `[
			{"name":"protection:x-push","match":{"refs":["refs/heads/main"]},"effect":{"protect":{}}},
			{"name":"protection:x-reviews","match":{"refs":["refs/heads/main"]},"mode":"audit","effect":{"required-reviews":{"min_approvals":1}}}]`,
			"carries a mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := policy.Parse([]byte(`{"version":1,"rules":` + tc.rules + `}`))
			if err != nil {
				t.Fatalf("fixture: %v", err)
			}
			pairs := settingsRulesetPairs(doc)
			if len(pairs) != 1 {
				t.Fatalf("pairs = %+v", pairs)
			}
			reason := settingsRulesetReadOnlyReason("acme", "monorepo", pairs[0])
			if !strings.Contains(reason, tc.want) {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
			// A save over it is refused by the server guard, not just the view.
			in := rulesetInput("acme", "monorepo")
			in.Slug, in.NewSlug = "x", "x"
			if _, _, err := settingsProtectionApply([]byte(`{"version":1,"rules":`+tc.rules+`}`), "acme", "monorepo", in); err == nil {
				t.Fatal("read-only pair accepted a save")
			}
		})
	}
	// A pair the form can express reports no reason.
	doc, _ := policy.Parse([]byte(`{"version":1,"rules":[
		{"name":"protection:x-push","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"bypass":["role:acme/monorepo:write"]}}},
		{"name":"protection:x-reviews","match":{"refs":["refs/heads/main"]},"effect":{"required-reviews":{"min_approvals":1}}}]}`))
	if got := settingsRulesetReadOnlyReason("acme", "monorepo", settingsRulesetPairs(doc)[0]); got != "" {
		t.Fatalf("editable pair reported %q", got)
	}
	if got := settingsRulesetReadOnlyReason("acme", "monorepo", settingsRulesetPair{slug: "x"}); got == "" {
		t.Fatal("an empty pair must be read-only")
	}
}

// TestRulesetValidateRefusals covers the form's own guards (the document
// parser would refuse most of them too, but with worse words).
func TestRulesetValidateRefusals(t *testing.T) {
	t.Parallel()
	base := rulesetInput("acme", "monorepo")
	cases := []struct {
		name string
		mut  func(*settingsRulesetInput)
		want string
	}{
		{"empty name", func(in *settingsRulesetInput) { in.Slug, in.NewSlug = "", "" }, "needs a name"},
		{"no refs", func(in *settingsRulesetInput) { in.Refs = nil }, "at least one ref pattern"},
		{"too many approvals", func(in *settingsRulesetInput) { in.Approvals = 51 }, "between 0 and 50"},
		{"negative approvals", func(in *settingsRulesetInput) { in.Approvals = -1 }, "between 0 and 50"},
		{"too many checks", func(in *settingsRulesetInput) {
			in.Checks = make([]string, 33)
			for i := range in.Checks {
				in.Checks[i] = "ci/" + strconv.Itoa(i)
			}
		}, "at most 32"},
		{"duplicate check", func(in *settingsRulesetInput) { in.Checks = []string{"ci", "ci"} }, "duplicate status check"},
		{"bad check", func(in *settingsRulesetInput) { in.Checks = []string{"has space"} }, "invalid context"},
		{"unknown role", func(in *settingsRulesetInput) { in.PushRoles = []string{"owner"} }, "unknown push role"},
		{"no methods", func(in *settingsRulesetInput) { in.MergeMethods = nil }, "at least one merge method"},
		{"unknown method", func(in *settingsRulesetInput) { in.MergeMethods = []string{"octopus"} }, "unknown merge method"},
		{"duplicate method", func(in *settingsRulesetInput) { in.MergeMethods = []string{"merge", "merge"} }, "duplicate merge method"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mut(&in)
			err := settingsRulesetValidate(in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if err := settingsRulesetValidate(base); err != nil {
		t.Fatalf("valid form refused: %v", err)
	}
	// A disabled form needs no field validation (removal only needs a slug).
	off := base
	off.Enabled, off.Refs, off.MergeMethods = false, nil, nil
	if err := settingsRulesetValidate(off); err != nil {
		t.Fatalf("disabled form: %v", err)
	}
}

// TestRulesetApplyRefusals covers the apply-level guards.
func TestRulesetApplyRefusals(t *testing.T) {
	t.Parallel()
	in := rulesetInput("acme", "monorepo")
	// Deleting an unknown ruleset.
	del := in
	del.Action, del.Slug, del.NewSlug = "delete", "ghost", "ghost"
	if _, _, err := settingsProtectionApply([]byte(rulesetDoc), "acme", "monorepo", del); err == nil ||
		!strings.Contains(err.Error(), "no ruleset named") {
		t.Fatalf("delete unknown = %v", err)
	}
	// Disabling a ruleset that does not exist is the same refusal.
	offUnknown := in
	offUnknown.Slug, offUnknown.NewSlug, offUnknown.Enabled = "ghost", "ghost", false
	if _, _, err := settingsProtectionApply([]byte(rulesetDoc), "acme", "monorepo", offUnknown); err == nil {
		t.Fatal("disable unknown accepted")
	}
	// Moving an unknown ruleset.
	mv := in
	mv.Action, mv.Slug = "down", "ghost"
	if _, _, err := settingsProtectionApply([]byte(rulesetDoc), "acme", "monorepo", mv); err == nil {
		t.Fatal("move unknown accepted")
	}
	// A name that violates the grammar never builds a document.
	bad := in
	bad.Slug, bad.NewSlug = "new", "Bad Name"
	if _, _, err := settingsProtectionApply([]byte(rulesetDoc), "acme", "monorepo", bad); err == nil ||
		!strings.Contains(err.Error(), "ruleset names are") {
		t.Fatalf("bad name = %v", err)
	}
	// An empty stored document still accepts the first ruleset.
	out, warnings, err := settingsProtectionApply(nil, "acme", "monorepo", in)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("empty store = %v %v", err, warnings)
	}
	doc, perr := policy.Parse(out)
	if perr != nil || doc.RuleByName("protection:main-push") == nil {
		t.Fatalf("first ruleset = %v %s", perr, out)
	}
	// Saving the mapped "default" ruleset warns about the rename.
	def, _, err := settingsProtectionApply([]byte(`{"version":1,"rules":[
		{"name":"protection-push","match":{"refs":["refs/heads/main"]},"effect":{"protect":{}}}]}`), "acme", "monorepo",
		settingsRulesetInput{Action: "save", Slug: "default", NewSlug: "default", Enabled: true,
			Refs: []string{"refs/heads/main"}, MergeMethods: []string{"merge"}})
	if err != nil {
		t.Fatalf("default save: %v", err)
	}
	_ = def
	if _, warns, err := settingsProtectionApply([]byte(`{"version":1,"rules":[
		{"name":"protection-push","match":{"refs":["refs/heads/main"]},"effect":{"protect":{}}}]}`), "acme", "monorepo",
		settingsRulesetInput{Action: "save", Slug: "default", NewSlug: "default", Enabled: true,
			Refs: []string{"refs/heads/main"}, MergeMethods: []string{"merge"}}); err != nil || len(warns) != 1 {
		t.Fatalf("rename warning = %v %v", warns, err)
	}
	// Unreadable rule heads refuse the rewrite.
	if _, _, err := settingsProtectionApply([]byte(`{"version":1,"rules":[{"name":42}]}`), "acme", "monorepo", in); err == nil {
		t.Fatal("unreadable rule name accepted")
	}
}

// TestRulesetNotices pins the post-write copy: a reorder is not a removal
// (the move forms post no enabled field, which must not read as "disabled").
func TestRulesetNotices(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   settingsRulesetInput
		want string
	}{
		{settingsRulesetInput{Action: "up", Slug: "main"}, "ruleset order saved"},
		{settingsRulesetInput{Action: "down", Slug: "main"}, "ruleset order saved"},
		{settingsRulesetInput{Action: "delete", Slug: "main"}, `ruleset "main" removed`},
		{settingsRulesetInput{Slug: "main", Enabled: false}, `ruleset "main" disabled — its rules were removed`},
		{settingsRulesetInput{Slug: "main", Enabled: true, NewSlug: "main"}, `ruleset "main" saved`},
	}
	for _, tc := range cases {
		if got := settingsProtectionNotice(tc.in); got != tc.want {
			t.Fatalf("%+v: notice = %q, want %q", tc.in, got, tc.want)
		}
	}
}
