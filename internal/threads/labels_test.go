package threads

import (
	"strings"
	"testing"
)

// Hosted runner labels (docs/features/21_parity_master.md PAR-170;
// docs/threads.md §8.2). A `ubuntu-latest` job must be schedulable on a stock
// spindle runner without editing the workflow, and the alias must never let a
// job run where its platform is not declared.

// TestHostedLabelExpansion pins the table and the pass-through rule.
func TestHostedLabelExpansion(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"ubuntu-latest"}, "linux,self-hosted"},
		{[]string{"Ubuntu-Latest"}, "linux,self-hosted"}, // case-insensitive
		{[]string{"ubuntu-24.04"}, "linux,self-hosted"},  // dated variants
		{[]string{"ubuntu-22.04"}, "linux,self-hosted"},
		{[]string{"ubuntu-20.04"}, "linux,self-hosted"},
		{[]string{"self-hosted", "ubuntu-latest"}, "linux,self-hosted"}, // de-duplicated
		{[]string{"linux"}, "linux"},                                    // a plain label is untouched
		{[]string{"docker"}, "docker"},
		// Withheld on purpose (D-15): never mapped onto a Linux runner.
		{[]string{"windows-latest"}, "windows-latest"},
		{[]string{"macos-14"}, "macos-14"},
		{[]string{"ubuntu-24.04-arm"}, "ubuntu-24.04-arm"},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := strings.Join(ExpandRunsOn(tc.in), ","); got != tc.want {
			t.Errorf("ExpandRunsOn(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Every mapped target is part of the default runner label set the docs
	// describe, so "the alias works out of the box" is a property of the table,
	// not of a test fixture.
	defaults := map[string]bool{"self-hosted": true, "linux": true, "docker": true}
	for _, name := range HostedAliasNames() {
		for _, label := range HostedLabelAliases[name] {
			if !defaults[label] {
				t.Errorf("alias %s maps to %q, which is not in the default label set", name, label)
			}
		}
	}
}

// TestHostedLabelScheduling pins the queue rule: a `ubuntu-latest` job matches
// the stock runner, an unmapped hosted label matches nothing, and a runner that
// lacks one mapped label still refuses the job (no capability is bypassed).
func TestHostedLabelScheduling(t *testing.T) {
	job := &Job{Spec: &Spec{RunsOn: []string{"ubuntu-latest"}}}
	defaultRunner := normalizeLabels([]string{"self-hosted", "linux", "docker"})
	if !labelsMatch(defaultRunner, jobLabels(job)) {
		t.Fatalf("a ubuntu-latest job is not schedulable on the default runner: %v", jobLabels(job))
	}
	// A linux-only runner does not declare self-hosted: the mapped set still
	// requires it, so the alias cannot widen a runner's capabilities.
	linuxOnly := normalizeLabels([]string{"linux"})
	if labelsMatch(linuxOnly, jobLabels(job)) {
		t.Fatalf("ubuntu-latest matched a runner without self-hosted: %v", jobLabels(job))
	}
	// A hosted label with no spindle contract stays unmatched (queued) rather
	// than landing on an unrelated Linux runner.
	job.Spec.RunsOn = []string{"windows-latest"}
	if labelsMatch(defaultRunner, jobLabels(job)) {
		t.Fatalf("windows-latest matched a Linux runner: %v", jobLabels(job))
	}
	// The unmapped label is preserved in the queue entry, which is what the run
	// page and the queue-reason surface (PAR-156) read.
	if got := jobLabels(job); len(got) != 1 || got[0] != "windows-latest" {
		t.Fatalf("jobLabels(windows-latest) = %v", got)
	}
}
