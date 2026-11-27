package workflow

import (
	"strings"
	"testing"
)

// PAR-004 (D-1): `spindle.*` is the documented name of the run context, and
// `github.*` / `forgejo.*` / `forge.*` are value-identical aliases. An imported
// workflow must not change meaning, so every alias resolves to the same value in
// a substitution site, in an `if:` and in a concurrency group.

func TestSpindleContextAliasesAreValueIdentical(t *testing.T) {
	values := TemplateValues{GitHub: map[string]string{
		"ref": "refs/heads/main", "event_name": "push", "sha": "abc",
		"ref_name": "main", "actor": "ada", "job": "build",
	}}
	contexts := []string{"spindle", "github", "forgejo", "forge"}
	cases := []struct {
		field string
		want  string
	}{
		{"ref", "refs/heads/main"},
		{"event_name", "push"},
		{"sha", "abc"},
		{"ref_name", "main"},
		{"actor", "ada"},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			var first string
			for i, context := range contexts {
				got, err := Substitute("${{ "+context+"."+tc.field+" }}", values)
				if err != nil {
					t.Fatalf("%s.%s: %v", context, tc.field, err)
				}
				if got != tc.want {
					t.Fatalf("%s.%s = %q, want %q", context, tc.field, got, tc.want)
				}
				if i == 0 {
					first = got
				} else if got != first {
					t.Fatalf("%s.%s = %q differs from spindle.%s = %q", context, tc.field, got, tc.field, first)
				}
				ev, eerr := EvalString(context+"."+tc.field, EvalScope{GitHub: values.GitHub, Site: SiteStepIf})
				if eerr != nil || ev != tc.want {
					t.Fatalf("if: %s.%s = %q, %v", context, tc.field, ev, eerr)
				}
			}
			// An `if:` comparing the canonical name with an alias is true: the
			// values are identical, not merely both supported.
			if ok, err := EvalIf("spindle."+tc.field+" == github."+tc.field+" && github."+tc.field+" == forgejo."+tc.field,
				EvalScope{GitHub: values.GitHub, Site: SiteStepIf}); err != nil || !ok {
				t.Fatalf("alias equality = %v, %v", ok, err)
			}
		})
	}
	// The concurrency group accepts every spelling and expands them the same.
	for _, context := range contexts {
		group, err := ExpandGroup("ci-${{ "+context+".ref_name }}", values.GitHub, nil, nil)
		if err != nil || group != "ci-main" {
			t.Fatalf("group %s = %q, %v", context, group, err)
		}
	}
	// A reference outside the closed set is still a named parse error for the
	// canonical spelling (the alias must not widen the vocabulary).
	_, err := Substitute("${{ spindle.actor_id }}", values)
	if err == nil || !strings.Contains(err.Error(), "no value in this run") {
		t.Fatalf("unavailable canonical field = %v, want the named error", err)
	}
	if err := ValidateTemplate("${{ spindle.token }}", TemplateScope{}); err == nil || !strings.Contains(err.Error(), "not a supported context field") {
		t.Fatalf("unknown canonical field = %v, want the closed-set error", err)
	}
}

// TestSpindleAliasIsDocumentedInParserErrors keeps the alias contract visible
// where an author will read it: the closed-set error names the canonical
// spelling and the aliases.
func TestSpindleAliasIsDocumentedInParserErrors(t *testing.T) {
	err := ValidateTemplate("${{ spindle.nope }}", TemplateScope{})
	if err == nil {
		t.Fatal("unknown field accepted")
	}
	msg := err.Error()
	for _, want := range []string{"spindle.* is canonical", "github.*", "forgejo.*", "forge.*", "value-identical aliases"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not contain %q", msg, want)
		}
	}
}
