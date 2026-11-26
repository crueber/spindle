package runner

import (
	"context"
	"strings"
	"testing"
)

// Branch coverage for the command files: an absent file is normal, an unreadable
// or oversized one is named, a cancelled job reads nothing, and the PATH list is
// de-duplicated.

func TestReadCommandFileBranches(t *testing.T) {
	p := &Plan{Names: Names{Container: "sp-j-test"}}
	exec := executorWith(&fakeDocker{})

	// Absent (the shell's exit 3): normal, not an error.
	exec.Docker = &fakeDocker{runOutputs: []runOutput{{match: "spindle-command", code: 3}}}
	data, present, err := exec.readCommandFile(context.Background(), p, "/tmp/spindle-command-1-env", 10)
	if err != nil || present || data != nil {
		t.Fatalf("absent = %v %v %v", data, present, err)
	}

	// Present and empty.
	exec.Docker = &fakeDocker{}
	data, present, err = exec.readCommandFile(context.Background(), p, "/tmp/spindle-command-1-env", 10)
	if err != nil || !present || len(data) != 0 {
		t.Fatalf("empty = %v %v %v", data, present, err)
	}

	// docker's failure is named with its own message.
	exec.Docker = &fakeDocker{runOutputs: []runOutput{{match: "spindle-command", errOut: "no such container", code: 1}}}
	if _, _, err := exec.readCommandFile(context.Background(), p, "/tmp/spindle-command-1-env", 10); err == nil ||
		!strings.Contains(err.Error(), "no such container") {
		t.Fatalf("failure = %v", err)
	}

	// Over the cap is refused before parsing.
	exec.Docker = &fakeDocker{runOutputs: []runOutput{{match: "spindle-command", out: strings.Repeat("x", 11)}}}
	if _, _, err := exec.readCommandFile(context.Background(), p, "/tmp/spindle-command-1-env", 10); err == nil ||
		!strings.Contains(err.Error(), "over the cap") {
		t.Fatalf("oversize = %v", err)
	}
}

// TestCollectCommandFilesSkipsACancelledJob pins that a cancelled job (whose
// container was killed) never turns a missing command file into a step error.
func TestCollectCommandFilesSkipsACancelledJob(t *testing.T) {
	exec := executorWith(&fakeDocker{})
	docker := exec.Docker.(*fakeDocker)
	p := &Plan{Names: Names{Container: "sp-j-test"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmds := newCommandFiles()
	step := StepPlan{Kind: "run", EnvFile: "/tmp/spindle-command-1-env", PathFile: "/tmp/spindle-command-1-path"}
	if err := exec.collectCommandFiles(ctx, p, step, cmds, quietStream(t)); err != nil {
		t.Fatalf("cancelled collection = %v", err)
	}
	if len(docker.snapshot()) != 0 {
		t.Fatalf("a cancelled job still read a container: %v", docker.snapshot())
	}
	// A parse error in the PATH file is named with the variable and the path.
	exec.Docker = &fakeDocker{runOutputs: []runOutput{{match: "spindle-command-1-path", out: strings.Repeat("p", MaxCommandPathEntryLen+1)}}}
	step.PathFile = "/tmp/spindle-command-1-path"
	step.EnvFile = ""
	if err := exec.collectCommandFiles(context.Background(), p, step, cmds, quietStream(t)); err == nil ||
		!strings.Contains(err.Error(), "GITHUB_PATH") {
		t.Fatalf("path parse error = %v", err)
	}
}

// TestCommandFileHelpers pins the remaining small branches: a de-duplicated
// entry, an over-long name, the reserved prefixes and a name that is allowed.
func TestCommandFileHelpers(t *testing.T) {
	cmds := newCommandFiles()
	if err := cmds.addPath([]string{"/a", "/a", "/b"}); err != nil {
		t.Fatalf("addPath: %v", err)
	}
	if strings.Join(cmds.path, ",") != "/a,/b" {
		t.Fatalf("path = %v", cmds.path)
	}
	if _, err := parseEnvFile([]byte(strings.Repeat("A", MaxCommandEnvNameLen+1) + "=x\n")); err == nil ||
		!strings.Contains(err.Error(), "longer than") {
		t.Fatalf("over-long name = %v", err)
	}
	for _, name := range []string{"FOO", "foo_bar", "_X", "A1"} {
		if reason := protectedEnvName(name); reason != "" {
			t.Errorf("protectedEnvName(%q) = %q, want allowed", name, reason)
		}
	}
	for name, want := range map[string]string{
		"FORGEJO_ENV":  "FORGEJO_*",
		"ACTIONS_STEP": "ACTIONS_*",
		"RUNNER_OS":    "RUNNER_*",
		"CI":           "CI",
		"BASH_ENV":     "process-startup",
		"PATH":         "GITHUB_PATH",
	} {
		if reason := protectedEnvName(name); !strings.Contains(reason, want) {
			t.Errorf("protectedEnvName(%q) = %q, want %q", name, reason, want)
		}
	}
}
