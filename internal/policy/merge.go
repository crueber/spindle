package policy

// This file owns the MERGE-METHOD half of the `protect` effect
// (docs/PLAN-V2.md §2.2/§2.5.2): require_up_to_date and merge_methods are
// parsed inside ProtectEffect but evaluated here, for one merging actor and
// one base ref, because receive-pack cannot decide them (a push has no PR
// head, and a base-ref push is not a merge — GitHub parity, L9 honesty
// note). One resolver serves BOTH the merge task's gate (internal/pulls)
// and the PR page's merge box (internal/webui), so what the page offers and
// what the gate accepts can never drift.

// The closed merge-method universe (03 §5 step 2's strategy set). Order is
// the UI's presentation order.
const (
	MethodMerge  = "merge"
	MethodSquash = "squash"
	MethodRebase = "rebase"
)

// MergeMethodsAll returns the method universe in presentation order.
func MergeMethodsAll() []string { return []string{MethodMerge, MethodSquash, MethodRebase} }

var mergeMethodEnum = map[string]bool{MethodMerge: true, MethodSquash: true, MethodRebase: true}

// MergeGate is the resolved merge-time view of every matching `protect`
// rule for one (ref, actor): the two effect fields after combination.
// Methods is nil when no matching rule restricts the set (every method).
type MergeGate struct {
	RequireUpToDate bool
	Methods         []string // nil = every method
	Rules           []string // contributing rule names, file order (narration)
}

// Allows reports whether method is permitted by the gate.
func (g MergeGate) Allows(method string) bool {
	if len(g.Methods) == 0 {
		return mergeMethodEnum[method]
	}
	for _, m := range g.Methods {
		if m == method {
			return true
		}
	}
	return false
}

// MethodsOrAll returns the permitted set, spelling out the universe when
// unrestricted (display helper; never a gate).
func (g MergeGate) MethodsOrAll() []string {
	if len(g.Methods) == 0 {
		return MergeMethodsAll()
	}
	return g.Methods
}

// MergeGateFor resolves the merge-method half for req (its Principal, Tags
// and Ref are consulted; Op is irrelevant — these fields are merge-only).
// Combination (declared at the effect, PLAN-V2 §2.2):
//   - require_up_to_date is all-of across matching rules the actor does NOT
//     bypass (a rule's bypass list exempts its own up-to-date demand, the
//     same per-rule law the push half uses);
//   - merge_methods is the intersection across matching rules; an absent
//     list is the universe, so a rule without the field never narrows. The
//     intersection is not bypass-sensitive: it is an offer, not a verdict,
//     and the plan declares it per matching rule.
//
// A nil document is "no gate" (allow every method, up-to-date not
// required). Pure and local.
func MergeGateFor(d *Document, req Request) MergeGate {
	var g MergeGate
	if d == nil {
		return g
	}
	methods := MergeMethodsAll()
	narrowed := false
	for _, r := range d.Rules {
		pe := r.Protect()
		if pe == nil || !ruleMatches(r, req, d.roster) {
			continue
		}
		contributed := false
		if pe.RequireUpToDate && !Bypassed(pe.Bypass, req.Principal, req.Tags, d.roster) {
			g.RequireUpToDate = true
			contributed = true
		}
		if len(pe.MergeMethods) > 0 {
			methods = intersectMethods(methods, pe.MergeMethods)
			narrowed = true
			contributed = true
		}
		if contributed {
			g.Rules = append(g.Rules, r.Name)
		}
	}
	if narrowed {
		g.Methods = methods
	}
	return g
}

// intersectMethods keeps the methods present in both lists, in the
// universe's presentation order.
func intersectMethods(a, b []string) []string {
	keep := make(map[string]bool, len(b))
	for _, m := range b {
		keep[m] = true
	}
	out := make([]string, 0, len(a))
	for _, m := range a {
		if keep[m] {
			out = append(out, m)
		}
	}
	return out
}

// AppliesToRef reports whether r's ref patterns select ref, ignoring the
// rule's other match keys (principals/paths). The effective-rules view needs
// "what can this ref's pushes and merges encounter" — a principal-scoped
// rule is shown, marked as scoped, rather than silently hidden. An absent
// refs list matches everything, exactly like ruleMatches.
func AppliesToRef(r *Rule, ref string) bool {
	if r == nil {
		return false
	}
	return matchGlobList(r.Match.Refs, ref)
}

// PrincipalScoped reports whether r narrows on principals (a display fact: a
// rule scoped to a team applies to that team's pushes only).
func PrincipalScoped(r *Rule) bool { return r != nil && len(r.Match.Principals) > 0 }
