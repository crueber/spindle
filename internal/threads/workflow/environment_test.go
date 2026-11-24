package workflow

// environment_test.go — `jobs.<key>.environment:` (string or {name,url}),
// docs/PLAN-THREADS-ENVIRONMENTS.md §2. The parser validates the name (it is a
// bucket key and a URL segment) and the URL scheme (a page renders it as a
// link), and a job without one is byte-for-byte unchanged.

import (
	"errors"
	"strings"
	"testing"
)

const environmentWorkflow = `name: Deploy
on: [push]
jobs:
  ship:
    runs-on: linux
    environment: production
    steps:
      - run: ./deploy.sh
  ship-url:
    runs-on: linux
    environment:
      name: staging
      url: https://staging.example.test/
    steps:
      - run: ./deploy.sh
  plain:
    runs-on: linux
    steps:
      - run: echo hi
`

func TestParseEnvironmentShapes(t *testing.T) {
	w, err := Parse([]byte(environmentWorkflow))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ship := w.Jobs["ship"]
	if ship.Environment == nil || ship.Environment.Name != "production" || ship.Environment.URL != "" {
		t.Fatalf("string form = %+v", ship.Environment)
	}
	withURL := w.Jobs["ship-url"]
	if withURL.Environment == nil || withURL.Environment.Name != "staging" ||
		withURL.Environment.URL != "https://staging.example.test/" {
		t.Fatalf("mapping form = %+v", withURL.Environment)
	}
	if plain := w.Jobs["plain"]; plain.Environment != nil {
		t.Fatalf("a job without `environment:` gained one: %+v", plain.Environment)
	}
}

func TestParseEnvironmentRejections(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want error
	}{
		{"empty mapping", "environment: {name: \"\"}", ErrBadEnvironment},
		{"leading dot", "environment: .hidden", ErrBadEnvironment},
		{"slash", "environment: prod/us", ErrBadEnvironment},
		{"too long", "environment: " + strings.Repeat("a", 65), ErrBadEnvironment},
		{"space", "environment: \"prod east\"", ErrBadEnvironment},
		{"relative url", "environment: {name: prod, url: /deploy}", ErrBadEnvironment},
		{"javascript url", "environment: {name: prod, url: \"javascript:alert(1)\"}", ErrBadEnvironment},
		{"unknown key", "environment: {name: prod, url: https://x.test, secret: s}", nil},
		{"sequence", "environment: [prod]", nil},
	}
	for _, tc := range cases {
		src := "name: x\non: [push]\njobs:\n  j:\n    runs-on: linux\n    steps:\n      - run: echo\n    " + tc.env + "\n"
		_, err := Parse([]byte(src))
		if tc.want == nil {
			if err == nil {
				t.Fatalf("%s: parsed, want an error", tc.name)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// TestEnvironmentValidateNil pins that a nil declaration validates (the
// ordinary job).
func TestEnvironmentValidateNil(t *testing.T) {
	var e *Environment
	if err := e.Validate("j"); err != nil {
		t.Fatalf("nil environment: %v", err)
	}
	if err := (&Environment{Name: " prod ", URL: "http://x.test"}).Validate("j"); err != nil {
		t.Fatalf("http url / padded name: %v", err)
	}
}
