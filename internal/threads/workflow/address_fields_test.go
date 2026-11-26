package workflow

import (
	"strings"
	"testing"
)

// The two server-address fields (PAR-111, docs/threads.md §3.5) are part of the
// closed `github.*` vocabulary so a validated workflow can read them. The
// runner substitutes them from its environment; a job-level `if:` does not
// resolve them yet. Everything else about the vocabulary is unchanged: an
// unknown field is still a parse error naming the set.

func TestServerAddressFieldsAreInTheClosedSet(t *testing.T) {
	for _, field := range []string{"server_url", "api_url"} {
		if !ValidGitHubField(field) {
			t.Fatalf("github.%s is not in the closed field set: %v", field, GitHubFields)
		}
		if !ValidGitHubField("SERVER_URL") {
			t.Fatal("field matching is case-insensitive")
		}
	}
	// The parser accepts each alias spelling in a substituted step field…
	for _, ref := range []string{
		"${{ github.server_url }}", "${{ forgejo.server_url }}", "${{ forge.api_url }}",
		"${{github.api_url}}",
	} {
		if err := ValidateTemplate("curl "+ref, TemplateScope{}); err != nil {
			t.Errorf("ValidateTemplate(%q) = %v", ref, err)
		}
	}
	// …and still refuses an unknown field, naming the set.
	err := ValidateTemplate("${{ github.server_urlx }}", TemplateScope{})
	if err == nil {
		t.Fatal("an unknown field was accepted")
	}
	if want := "not a supported context field"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %q", err, want)
	}
	// A concurrency group still refuses them: the group is expanded before the
	// runner resolves the addresses (server-side context), so it fails closed.
	if err := ValidateGroupTemplate("g-${{ github.server_url }}", nil, nil, true); err == nil {
		t.Fatal("a group accepted github.server_url")
	}
}
