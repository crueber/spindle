package importer

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// TestTranslateWave2Triggers pins the importer's Wave-2 surface: paths and
// ignore filters are carried, the typed triggers keep their emitted types (and
// drop unemittable ones by name), and a schedule entry's timezone survives.
func TestTranslateWave2Triggers(t *testing.T) {
	src := `on:
  push:
    branches-ignore: [wip]
    paths: ['src/**', '!src/vendor/**']
  pull_request:
    paths-ignore: ['docs/**']
    types: [opened, synchronize, deleted]
  release:
    types: [published, locked]
  issues:
    types: [opened, closed]
  issue_comment:
    types: [created, edited]
  schedule:
    - cron: "@daily"
      timezone: Europe/Berlin
    - cron: "0 3 * * *"
      timezone: Mars/Olympus
jobs:
  a:
    runs-on: linux
    steps:
      - run: echo hi
`
	res, err := Translate(Forgejo, []byte(src))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if _, err := workflow.Parse(res.Output); err != nil {
		t.Fatalf("output does not parse: %v\n%s", err, res.Output)
	}
	var paths []string
	for _, item := range res.Untranslated {
		paths = append(paths, item.Path)
	}
	for _, want := range []string{
		"on.pull_request.types", "on.release.types", "on.issue_comment.types", "on.schedule[2]",
	} {
		found := false
		for _, p := range paths {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no refusal %q in %v", want, paths)
		}
	}
	out := string(res.Output)
	for _, want := range []string{"paths:", "branches-ignore:", "paths-ignore:", "opened", "timezone: Europe/Berlin"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lost %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"locked", "edited"} {
		// The unemittable type's spelling may survive only inside a comment.
		for _, line := range strings.Split(out, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "- "+bad {
				t.Errorf("unemittable type %q survived as a list entry:\n%s", bad, out)
			}
		}
	}
}

// TestTranslateTypedTriggerShapes covers the typed trigger's refusal arms.
func TestTranslateTypedTriggerShapes(t *testing.T) {
	for _, src := range []string{
		"on:\n  release: every\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: x\n",
		"on:\n  release:\n    branches: [x]\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: x\n",
		"on:\n  release:\n    types: published\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: x\n",
	} {
		res, err := Translate(Forgejo, []byte(src))
		if err != nil {
			t.Fatalf("Translate: %v", err)
		}
		if len(res.Untranslated) == 0 {
			t.Errorf("no refusal for:\n%s", src)
		}
	}
}

// TestTranslateScheduleTimezoneArms covers the invalid timezone and extra-key
// arms of the schedule entry.
func TestTranslateScheduleTimezoneArms(t *testing.T) {
	for _, src := range []string{
		"on:\n  schedule:\n    - cron: '0 3 * * *'\n      timezone: 7\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: x\n",
		"on:\n  schedule:\n    - cron: '0 3 * * *'\n      tz: UTC\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: x\n",
	} {
		res, err := Translate(Forgejo, []byte(src))
		if err != nil {
			t.Fatalf("Translate: %v", err)
		}
		if len(res.Untranslated) == 0 {
			t.Errorf("no refusal for:\n%s", src)
		}
	}
}

// TestTranslateServiceCmd pins the Forgejo `services.<id>.cmd` rename and the
// credentials refusal.
func TestTranslateServiceCmd(t *testing.T) {
	src := `on: push
jobs:
  a:
    runs-on: linux
    services:
      db:
        image: postgres:16
        cmd: ["postgres"]
      cache:
        image: redis:7
        credentials:
          username: u
          password: p
    steps:
      - run: echo hi
`
	res, err := Translate(Forgejo, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Output)
	if !strings.Contains(out, "command: [") || strings.Contains(out, "cmd:") {
		t.Fatalf("cmd not renamed:\n%s", out)
	}
	found := false
	for _, item := range res.Untranslated {
		if strings.Contains(item.Path, "credentials") {
			found = true
		}
	}
	if !found {
		t.Fatalf("credentials refusal missing: %+v", res.Untranslated)
	}
}

// TestTranslatePermissionsArms covers the permission-map refusal branches the
// merged track's parser behaviour relies on.
func TestTranslatePermissionsArms(t *testing.T) {
	src := `on: push
permissions:
  all: read
  bogus-scope: read
  contents: 3
jobs:
  a:
    runs-on: linux
    steps:
      - run: echo hi
`
	res, err := Translate(Forgejo, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Untranslated) < 3 {
		t.Fatalf("permission refusals = %+v", res.Untranslated)
	}
}
