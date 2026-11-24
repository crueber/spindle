package policy

import (
	"os"
	"strings"
	"testing"
)

// merge_methods / require_up_to_date (PLAN-V2 §2.2): strict parse tables
// plus the combination law the merge task and the PR page share.

func parseProtect(t *testing.T, body string) (*ProtectEffect, error) {
	t.Helper()
	doc, err := Parse([]byte(`{"version":1,"rules":[{"name":"r","match":{"refs":["refs/heads/main"]},"effect":{"protect":` + body + `}}]}`))
	if err != nil {
		return nil, err
	}
	return doc.Rules[0].Protect(), nil
}

func TestProtectMergeFieldsParse(t *testing.T) {
	t.Run("absent defaults off and unrestricted", func(t *testing.T) {
		pe, err := parseProtect(t, `{"restricts":["update"]}`)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if pe.RequireUpToDate || len(pe.MergeMethods) != 0 {
			t.Fatalf("pe=%+v", pe)
		}
	})
	t.Run("both set", func(t *testing.T) {
		pe, err := parseProtect(t, `{"require_up_to_date":true,"merge_methods":["squash","merge"]}`)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if !pe.RequireUpToDate {
			t.Fatal("require_up_to_date lost")
		}
		if strings.Join(pe.MergeMethods, ",") != "squash,merge" {
			t.Fatalf("methods = %v", pe.MergeMethods)
		}
	})
	t.Run("explicit false is off", func(t *testing.T) {
		pe, err := parseProtect(t, `{"require_up_to_date":false}`)
		if err != nil || pe.RequireUpToDate {
			t.Fatalf("pe=%+v err=%v", pe, err)
		}
	})
	for _, tc := range []struct {
		name string
		body string
	}{
		{"up_to_date null", `{"require_up_to_date":null}`},
		{"up_to_date string", `{"require_up_to_date":"yes"}`},
		{"up_to_date number", `{"require_up_to_date":1}`},
		{"methods null", `{"merge_methods":null}`},
		{"methods empty", `{"merge_methods":[]}`},
		{"methods not array", `{"merge_methods":"merge"}`},
		{"methods unknown", `{"merge_methods":["merge","fast-forward"]}`},
		{"methods duplicate", `{"merge_methods":["merge","merge"]}`},
		{"methods non-string", `{"merge_methods":[42]}`},
		{"unknown key still refused", `{"merge_method":["merge"]}`},
	} {
		t.Run("reject "+tc.name, func(t *testing.T) {
			if _, err := parseProtect(t, tc.body); err == nil {
				t.Fatalf("accepted %s", tc.body)
			}
		})
	}
}

// doc builds a document from rule JSON fragments so each combination case
// reads as the policy it encodes.
func mergeDoc(t *testing.T, rules ...string) *Document {
	t.Helper()
	doc, err := Parse([]byte(`{"version":1,"rules":[` + strings.Join(rules, ",") + `]}`))
	if err != nil {
		t.Fatalf("parse doc: %v", err)
	}
	return doc
}

func TestMergeGateFor(t *testing.T) {
	update := Request{Ref: "refs/heads/main", Op: OpUpdate}

	t.Run("no document allows everything", func(t *testing.T) {
		g := MergeGateFor(nil, update)
		if g.RequireUpToDate {
			t.Fatal("nil doc must not require up-to-date")
		}
		for _, m := range MergeMethodsAll() {
			if !g.Allows(m) {
				t.Fatalf("nil doc must allow %s", m)
			}
		}
		if v := capsLen(g.MethodsOrAll()); v != 3 {
			t.Fatalf("MethodsOrAll = %v", g.MethodsOrAll())
		}
	})
	t.Run("rules not matching the ref are ignored", func(t *testing.T) {
		doc := mergeDoc(t,
			`{"name":"a","match":{"refs":["refs/heads/release/*"]},"effect":{"protect":{"require_up_to_date":true,"merge_methods":["squash"]}}}`)
		g := MergeGateFor(doc, update)
		if g.RequireUpToDate || len(g.Methods) != 0 || len(g.Rules) != 0 {
			t.Fatalf("gate = %+v", g)
		}
	})
	t.Run("all-of up-to-date across matching rules", func(t *testing.T) {
		doc := mergeDoc(t,
			`{"name":"a","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"require_up_to_date":true}}}`,
			`{"name":"b","match":{},"effect":{"protect":{"require_checks":["ci"]}}}`,
		)
		g := MergeGateFor(doc, update)
		if !g.RequireUpToDate {
			t.Fatalf("gate = %+v", g)
		}
		if strings.Join(g.Rules, ",") != "a" {
			t.Fatalf("contributing rules = %v", g.Rules)
		}
	})
	t.Run("bypass exempts that rule's up-to-date demand only", func(t *testing.T) {
		doc := mergeDoc(t,
			`{"name":"a","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"require_up_to_date":true,"bypass":["role:o/r:admin"]}}}`,
			`{"name":"b","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"require_up_to_date":true}}}`,
		)
		bypassed := Request{Principal: "jane", Ref: "refs/heads/main", Op: OpUpdate}
		if g := MergeGateFor(doc, bypassed); !g.RequireUpToDate {
			t.Fatalf("rule b still demands up-to-date: %+v", g)
		}
		// A document where the only demanding rule is bypassed: the gate
		// admits the actor (the per-rule law of the push half).
		only := mergeDoc(t,
			`{"name":"a","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"require_up_to_date":true,"bypass":["jane"]}}}`)
		if g := MergeGateFor(only, bypassed); g.RequireUpToDate {
			t.Fatalf("bypassed rule must not demand up-to-date: %+v", g)
		}
	})
	t.Run("methods intersect across matching rules", func(t *testing.T) {
		doc := mergeDoc(t,
			`{"name":"a","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"merge_methods":["squash","merge"]}}}`,
			`{"name":"b","match":{"refs":["refs/heads/*"]},"effect":{"protect":{"merge_methods":["rebase","merge"]}}}`,
			`{"name":"c","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"require_checks":["ci"]}}}`,
		)
		g := MergeGateFor(doc, update)
		if strings.Join(g.Methods, ",") != "merge" {
			t.Fatalf("methods = %v", g.Methods)
		}
		if g.Allows("squash") || g.Allows("rebase") || !g.Allows("merge") {
			t.Fatalf("allows = %+v", g)
		}
		if strings.Join(g.Rules, ",") != "a,b" {
			t.Fatalf("contributing rules = %v", g.Rules)
		}
	})
	t.Run("an absent list never narrows the intersection", func(t *testing.T) {
		doc := mergeDoc(t,
			`{"name":"a","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"merge_methods":["rebase"]}}}`,
			`{"name":"b","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"]}}}`,
		)
		g := MergeGateFor(doc, update)
		if strings.Join(g.MethodsOrAll(), ",") != "rebase" {
			t.Fatalf("methods = %v", g.Methods)
		}
	})
	t.Run("principal-scoped rules only apply to that principal", func(t *testing.T) {
		doc := mergeDoc(t,
			`{"name":"bots","match":{"refs":["refs/heads/main"],"principals":["bot"]},"effect":{"protect":{"merge_methods":["squash"]}}}`)
		if g := MergeGateFor(doc, Request{Principal: "jane", Ref: "refs/heads/main", Op: OpUpdate}); len(g.Methods) != 0 {
			t.Fatalf("jane: %+v", g)
		}
		if g := MergeGateFor(doc, Request{Principal: "bot", Ref: "refs/heads/main", Op: OpUpdate}); strings.Join(g.Methods, ",") != "squash" {
			t.Fatalf("bot: %+v", g)
		}
	})
	t.Run("Allows rejects unknown methods even when unrestricted", func(t *testing.T) {
		var g MergeGate
		if g.Allows("octopus") {
			t.Fatal("octopus is not a merge method")
		}
	})
	t.Run("non-protect effects contribute nothing", func(t *testing.T) {
		doc := mergeDoc(t,
			`{"name":"rev","match":{"refs":["refs/heads/main"]},"effect":{"required-reviews":{"min_approvals":2}}}`)
		if g := MergeGateFor(doc, update); g.RequireUpToDate || len(g.Methods) != 0 {
			t.Fatalf("gate = %+v", g)
		}
	})
}

func TestAppliesToRefAndScoping(t *testing.T) {
	doc := mergeDoc(t,
		`{"name":"main","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"]}}}`,
		`{"name":"bots","match":{"refs":["refs/heads/*"],"principals":["bot"]},"effect":{"protect":{"restricts":["update"]}}}`,
		`{"name":"all","effect":{"protect":{"restricts":["update"]}}}`,
	)
	cases := map[string][]string{
		"refs/heads/main":  {"main", "bots", "all"},
		"refs/heads/topic": {"bots", "all"},
		"refs/tags/v1":     {"all"},
	}
	for ref, want := range cases {
		var got []string
		for _, r := range doc.Rules {
			if AppliesToRef(r, ref) {
				got = append(got, r.Name)
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: got %v want %v", ref, got, want)
		}
	}
	if PrincipalScoped(doc.Rules[0]) || !PrincipalScoped(doc.Rules[1]) {
		t.Fatalf("scoping bits wrong")
	}
	if AppliesToRef(nil, "refs/heads/main") || PrincipalScoped(nil) {
		t.Fatal("nil rule must be inert")
	}
}

func capsLen(v []string) int { return len(v) }

// The rule-name grammar admits ':' after the first character so rulesets can
// be named protection:<slug>-push (PLAN-V2 §2.4). Everything that was legal
// stays legal; the leading character, the charset and the length are pinned.
func TestRuleNameGrammar(t *testing.T) {
	valid := []string{"a", "protection:main-push", "protection:default-reviews", "main-2", "a" + strings.Repeat("b", 62)}
	for _, name := range valid {
		body := `{"version":1,"rules":[{"name":"` + name + `","effect":{"protect":{}}}]}`
		if _, err := Parse([]byte(body)); err != nil {
			t.Fatalf("name %q refused: %v", name, err)
		}
	}
	invalid := []string{":x", "1x", "Main", "a b", "a.b", "a/b", "a" + strings.Repeat("b", 63)}
	for _, name := range invalid {
		body := `{"version":1,"rules":[{"name":"` + name + `","effect":{"protect":{}}}]}`
		if _, err := Parse([]byte(body)); err == nil {
			t.Fatalf("name %q accepted", name)
		}
	}
}

// TestMergeFieldsGoldenFixture pins the bucket shape the settings rulesets
// write (L5: a shared Rust-era file; the new fields are additive optional
// keys). The fixture exercises every new field, the ruleset name spelling and
// a rule that omits both fields.
func TestMergeFieldsGoldenFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/policy-merge-fields.json")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	doc, perr := Parse(raw)
	if perr != nil {
		t.Fatalf("fixture unparseable: %v", perr)
	}
	if len(doc.Rules) != 3 {
		t.Fatalf("rules = %d", len(doc.Rules))
	}
	main := doc.RuleByName("protection:main-push")
	if main == nil || main.Protect() == nil {
		t.Fatalf("ruleset push rule missing: %+v", doc.Rules)
	}
	pe := main.Protect()
	if !pe.RequireUpToDate || strings.Join(pe.MergeMethods, ",") != "merge,squash" {
		t.Fatalf("merge fields = %+v", pe)
	}
	// The rule that omits both fields keeps the defaults.
	lock := doc.RuleByName("release-lock").Protect()
	if lock.RequireUpToDate || lock.MergeMethods != nil {
		t.Fatalf("absent fields must default off: %+v", lock)
	}
	// And the resolvers agree for the fixture's ref.
	gate := MergeGateFor(doc, Request{Principal: "jane", Ref: "refs/heads/main", Op: OpUpdate})
	if !gate.RequireUpToDate || gate.Allows("rebase") || !gate.Allows("squash") {
		t.Fatalf("gate = %+v", gate)
	}
}
