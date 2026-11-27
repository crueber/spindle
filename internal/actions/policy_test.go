package actions

import (
	"strings"
	"testing"
)

// TestPolicyDecisions pins D-5: allowlist mode denies a mutable ref by default,
// allows an approved repository's tag, and always allows a full commit SHA.
func TestPolicyDecisions(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name   string
		policy Policy
		uses   string
		allow  bool
		pinned bool
		entry  string
		reason string
	}{
		{"default denies a mutable ref", DefaultPolicy(), "acme/tools@v1", false, false, "", "not in threads.actions.allow"},
		{"default allows a pinned sha", DefaultPolicy(), "acme/tools@" + sha, true, true, "", ""},
		{"allowlist entry allows a tag", Policy{Mode: ModeAllowlist, Allow: []string{"acme/tools"}, RequireSHA: true}, "acme/tools@v1", true, false, "acme/tools", ""},
		{"owner wildcard", Policy{Mode: ModeAllowlist, Allow: []string{"acme/*"}, RequireSHA: true}, "acme/other/sub@main", true, false, "acme/*", ""},
		{"star allows everything", Policy{Mode: ModeAllowlist, Allow: []string{"*"}, RequireSHA: true}, "x/y@main", true, false, "*", ""},
		{"another owner is refused", Policy{Mode: ModeAllowlist, Allow: []string{"acme/*"}, RequireSHA: true}, "evil/tools@main", false, false, "", "not in threads.actions.allow"},
		{"require_sha off weakens to open", Policy{Mode: ModeAllowlist, RequireSHA: false}, "acme/tools@main", true, false, "", ""},
		{"open mode allows a tag", Policy{Mode: ModeOpen, RequireSHA: true}, "acme/tools@main", true, false, "", ""},
		{"deny refuses even a pinned sha", Policy{Mode: ModeDeny}, "acme/tools@" + sha, false, true, "", "disabled"},
		{"a full-URL source matches its host identity", Policy{Mode: ModeAllowlist, Allow: []string{"code.forgejo.org/actions/setup-go"}, RequireSHA: true}, "https://code.forgejo.org/actions/setup-go@v5", true, false, "code.forgejo.org/actions/setup-go", ""},
	} {
		ref, err := ParseRef(tc.uses)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		d := tc.policy.Decide(ref)
		if d.Allowed != tc.allow || d.Pinned != tc.pinned || d.AllowEntry != tc.entry {
			t.Fatalf("%s: decision = %+v", tc.name, d)
		}
		if tc.reason != "" && (!strings.Contains(d.Reason, tc.reason) || d.Reason == "") {
			t.Fatalf("%s: reason = %q, want it to contain %q", tc.name, d.Reason, tc.reason)
		}
	}
}

func TestPolicyValidation(t *testing.T) {
	for i, tc := range []struct {
		policy Policy
		ok     bool
	}{
		{Policy{Mode: ModeAllowlist}, true},
		{Policy{Mode: ""}, true},
		{Policy{Mode: "bogus"}, false},
		{Policy{Mode: ModeAllowlist, Allow: []string{""}}, false},
		{Policy{Mode: ModeAllowlist, Allow: []string{"acme"}}, false},
		{Policy{Mode: ModeAllowlist, Allow: []string{"acme/tools/"}}, false},
		{Policy{Mode: ModeAllowlist, Allow: []string{"acme/*", "host/acme/repo"}}, true},
		{Policy{Mode: ModeAllowlist, RequireSHA: false}, true},
		{Policy{Mode: ModeAllowlist, Allow: []string{"https://acme/tools"}}, false},
		{Policy{Mode: ModeAllowlist, Allow: []string{"acme/t?ols"}}, false},
		{Policy{Mode: ModeAllowlist, Allow: []string{" acme/tools"}}, false},
		{Policy{Mode: ModeAllowlist, Allow: []string{"-acme/tools"}}, false},
		{Policy{Mode: ModeAllowlist, Allow: []string{"a/b/c/d"}}, false},
	} {
		if err := tc.policy.Validate(); (err == nil) != tc.ok {
			t.Fatalf("%d %+v: err = %v, ok = %v", i, tc.policy, err, tc.ok)
		}
	}
}

// TestInvalidPolicyFailsClosed: a typo in the operator's configuration refuses
// remote actions rather than guessing what they meant.
func TestInvalidPolicyFailsClosed(t *testing.T) {
	ref, _ := ParseRef("acme/tools@v1")
	d := Policy{Mode: "allowlsit", Allow: []string{"acme/tools"}}.Decide(ref)
	if d.Allowed || !strings.Contains(d.Reason, "policy is invalid") {
		t.Fatalf("decision = %+v", d)
	}
}

// TestMutableRefRefusalIsActionable names both remedies.
func TestMutableRefRefusalIsActionable(t *testing.T) {
	ref, _ := ParseRef("acme/tools/sub@main")
	d := DefaultPolicy().Decide(ref)
	for _, want := range []string{"acme/tools/sub@main", "40-character", "acme/tools"} {
		if !strings.Contains(d.Reason, want) {
			t.Fatalf("reason %q missing %q", d.Reason, want)
		}
	}
}
