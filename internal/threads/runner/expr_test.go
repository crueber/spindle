package runner

import (
	"strings"
	"testing"
)

// The runner reads a concurrency group already expanded in the job spec, but
// the rule is the server's (threads.ExpandGroup), aliased here so the two ends
// cannot drift: a reference that resolves to nothing is an error, never a
// literal.
func TestExpandGroupMatchesTheServerRule(t *testing.T) {
	main, err := ExpandGroup("ci-${{ github.ref }}", map[string]string{"ref": "refs/heads/main"}, nil, nil)
	if err != nil || main != "ci-refs/heads/main" {
		t.Fatalf("main = %q, %v", main, err)
	}
	feature, err := ExpandGroup("ci-${{ github.ref }}", map[string]string{"ref": "refs/heads/fix/x"}, nil, nil)
	if err != nil || feature != "ci-refs/heads/fix/x" {
		t.Fatalf("feature = %q, %v", feature, err)
	}
	if main == feature {
		t.Fatal("two branches share one group key")
	}
	cell, err := ExpandGroup("build-${{ matrix.go }}", nil, map[string]string{"go": "1.27"}, nil)
	if err != nil || cell != "build-1.27" {
		t.Fatalf("cell = %q, %v", cell, err)
	}
	if _, err := ExpandGroup("ci-${{ matrix.os }}", nil, map[string]string{"go": "1.27"}, nil); err == nil {
		t.Fatal("an unresolved axis became a literal")
	}
	if _, err := ExpandGroup("ci-${{ inputs.mode }}", nil, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "inputs.mode") {
		t.Fatalf("unresolved input error = %v", err)
	}
}
