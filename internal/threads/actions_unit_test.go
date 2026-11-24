package threads

import (
	"context"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// Unit edges for the local-action plumbing: path validation, the resolver's
// failure modes, the display expansion bounds, and the image walk's defensive
// branches (a stored spec is data, not a promise).

func TestLocalActionDirSpellings(t *testing.T) {
	ok := map[string]string{
		"./x":          "x",
		"./a/b/c":      "a/b/c",
		"./a/../b":     "b",
		"./a/./b/":     "a/b",
		"./.spindle/x": ".spindle/x",
	}
	for in, want := range ok {
		got, err := LocalActionDir(in)
		if err != nil || got != want {
			t.Errorf("LocalActionDir(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := map[string]string{
		"./":               "needs a directory",
		"./.":              "needs a directory",
		".//":              "no leading /",
		".//etc/passwd":    "no leading /",
		"./../escape":      "escapes the repository",
		"./a/../../escape": "escapes the repository",
		"./a\\b":           "use / separators",
		"./a:b":            "must not contain ':'",
		"./a\x00b":         "NUL",
	}
	for in, want := range bad {
		if _, err := LocalActionDir(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("LocalActionDir(%q) = %v, want %q", in, err, want)
		}
	}
	// A bare (non-./) path is not a local action, whatever it looks like.
	if IsLocalAction(".spindle/actions/x") || IsLocalAction("owner/repo@v1") {
		t.Fatal("only ./<path> is a local action")
	}
}

func TestActionHelpers(t *testing.T) {
	if shortCommit("abc") != "abc" || shortCommit("1234567890abcdef") != "1234567890ab" {
		t.Fatal("shortCommit")
	}
	if copyStringMap(nil) != nil {
		t.Fatal("copyStringMap(nil) must be nil")
	}
	in := map[string]string{"a": "b"}
	out := copyStringMap(in)
	out["a"] = "changed"
	if in["a"] != "b" {
		t.Fatal("copyStringMap must clone")
	}
	if listOrNone(nil) != "none" || listOrNone([]string{"b", "a"}) != "a, b" {
		t.Fatal("listOrNone")
	}
	if DockerUsesImage("docker://alpine") != "alpine" || DockerUsesImage("./x") != "" || DockerUsesImage("DOCKER://alpine") != "alpine" {
		t.Fatal("DockerUsesImage")
	}
}

func TestActionResolverEdges(t *testing.T) {
	r := newActionResolver(nil, "o/r", testSHA)
	if err := r.fill(context.Background(), nil); err != nil {
		t.Fatalf("nil spec: %v", err)
	}
	if err := r.fill(context.Background(), &Spec{}); err != nil {
		t.Fatalf("empty spec: %v", err)
	}
	if _, err := r.load(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "git backend not wired") {
		t.Fatalf("nil git = %v", err)
	}
	// A read failure that is not "not found" is reported, not mistaken for a
	// missing action.
	g := newFakeGit()
	g.readErr = errReadFailure{}
	r = newActionResolver(g, "o/r", testSHA)
	if _, err := r.load(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "could not read") {
		t.Fatalf("read failure = %v", err)
	}
	// A long commit is shortened in the message.
	r = newActionResolver(newFakeGit(), "o/r", "1234567890abcdef1234567890abcdef12345678")
	if _, err := r.load(context.Background(), "x"); err == nil || strings.Contains(err.Error(), "1234567890abcdef") {
		t.Fatalf("long commit in message = %v", err)
	}
}

// errReadFailure is a non-ErrNotFound read error (an infrastructure failure).
type errReadFailure struct{}

func (errReadFailure) Error() string { return "backend down" }

func TestConvenienceStepsBounds(t *testing.T) {
	if convenienceSteps(nil) != nil {
		t.Fatal("nil spec")
	}
	if got := convenienceSteps(&Spec{}); len(got) != 0 {
		t.Fatalf("empty = %v", got)
	}
	// A hostile spec that loops through its own action map must terminate (the
	// display walk is depth-bounded exactly like the resolver).
	spec := &Spec{
		Steps: []StepSpec{{Uses: "./a"}},
		Actions: map[string]ActionSpec{
			"a": {Path: "a", Using: workflow.ActionUsingComposite, Steps: []StepSpec{{Uses: "./a"}}},
		},
	}
	if got := convenienceSteps(spec); len(got) > workflow.MaxActionDepth+2 {
		t.Fatalf("cyclic display expansion produced %d rows", len(got))
	}
	// A local docker action shows the image it runs (not just the path).
	spec = &Spec{
		Steps: []StepSpec{{Name: "lint", Uses: "./a"}},
		Actions: map[string]ActionSpec{
			"a": {Path: "a", Using: workflow.ActionUsingDocker, Image: "ghcr.io/acme/lint:1"},
		},
	}
	row := convenienceSteps(spec)[0]
	if row.Uses != "docker://ghcr.io/acme/lint:1" || row.Name != "lint" {
		t.Fatalf("docker display = %+v", row)
	}
}

// TestCheckStepImagesDefensiveWalk pins the image walk's branches that creation
// already prevents: a malformed path, a definition the payload does not carry,
// a cycle, and a nested composite's docker image.
func TestCheckStepImagesDefensiveWalk(t *testing.T) {
	policy := ImagePolicy{AllowPrefixes: []string{"docker.io/library"}}
	cases := []struct {
		name string
		spec *Spec
		want string
	}{
		{"inline docker", &Spec{Steps: []StepSpec{{Uses: "docker://evil.example/x"}}}, "evil.example/x"},
		{"malformed path", &Spec{Steps: []StepSpec{{Uses: "./../x"}}}, ""},
		{"missing definition", &Spec{Steps: []StepSpec{{Uses: "./gone"}}}, ""},
		{"cycle", &Spec{
			Steps:   []StepSpec{{Uses: "./a"}},
			Actions: map[string]ActionSpec{"a": {Path: "a", Using: "composite", Steps: []StepSpec{{Uses: "./a"}}}},
		}, ""},
		{"nested docker", &Spec{
			Steps: []StepSpec{{Uses: "./a"}},
			Actions: map[string]ActionSpec{
				"a": {Path: "a", Using: "composite", Steps: []StepSpec{{Uses: "./b"}}},
				"b": {Path: "b", Using: "composite", Steps: []StepSpec{{Uses: "docker://evil.example/y"}}},
			},
		}, "evil.example/y"},
		{"nested docker step source", &Spec{
			Steps: []StepSpec{{Uses: "./a"}},
			Actions: map[string]ActionSpec{
				"a": {Path: "a", Using: "composite", Steps: []StepSpec{{Uses: "docker://evil.example/z"}}},
			},
		}, "step 1 (uses: ./a) step 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refusal, err := policy.checkJobImages(tc.spec)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if tc.want == "" {
				if refusal != nil {
					t.Fatalf("unexpected refusal: %v", refusal)
				}
				return
			}
			if refusal == nil || !strings.Contains(refusal.Reason, tc.want) {
				t.Fatalf("refusal = %v, want %q", refusal, tc.want)
			}
		})
	}
}
