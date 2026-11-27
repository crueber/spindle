package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestThreadsRunInputsAndPriority(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("POST", "/o/r/api/threads", 201, `{"runs":[{"id":"run1","workflow":"ci.yml","state":"queued"}]}`)
	code, _, errOut := runCmd(t, srv, "tok", runThreadsRun,
		"o/r", "ci.yml", "--ref", "main", "--input", "mode=fast", "--input", "level=2", "--priority", "3")
	if code != exitOK {
		t.Fatalf("run = %d (%s)", code, errOut)
	}
	decodeBody(t, f.last(), map[string]any{
		"ref": "main", "workflow": "ci.yml", "priority": float64(3),
		"inputs": map[string]any{"mode": "fast", "level": "2"},
	})
}

func TestThreadsWatchExitCodes(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/o/r/api/threads/run1", 200, `{"run":{"state":"success"},"jobs":[]}`)
	if code, out, _ := runCmd(t, srv, "tok", runThreadsWatch, "o/r", "run1"); code != exitOK || !strings.Contains(out, "succeeded") {
		t.Fatalf("success watch = %d %q", code, out)
	}
	f.on("GET", "/o/r/api/threads/run2", 200, `{"run":{"state":"failure"},"jobs":[]}`)
	if code, out, _ := runCmd(t, srv, "tok", runThreadsWatch, "o/r", "run2"); code != exitErr || !strings.Contains(out, "failed") {
		t.Fatalf("failure watch = %d %q", code, out)
	}
	f.on("GET", "/o/r/api/threads/run3", 200, `{"run":{"state":"cancelled"},"jobs":[]}`)
	if code, _, _ := runCmd(t, srv, "tok", runThreadsWatch, "o/r", "run3"); code != 130 {
		t.Fatalf("cancel watch = %d, want 130", code)
	}
}

func TestThreadsScheduleVerbs(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("POST", "/o/r/api/threads/schedules/enable", 200, `{"path":".spindle/workflows/ci.yml","cron":"0 3 * * *","disabled":true}`)
	code, out, errOut := runCmd(t, srv, "tok", runThreadsScheduleEnable(false),
		"o/r", "--path", ".spindle/workflows/ci.yml", "--cron", "0 3 * * *")
	if code != exitOK || !strings.Contains(out, "disabled") {
		t.Fatalf("disable = %d %q (%s)", code, out, errOut)
	}
	decodeBody(t, f.last(), map[string]any{
		"path": ".spindle/workflows/ci.yml", "cron": "0 3 * * *", "timezone": "", "enabled": false,
	})
	f.on("POST", "/o/r/api/threads/schedules/run-now", 201, `{"runs":[{"id":"run9","state":"queued"}]}`)
	if code, out, _ := runCmd(t, srv, "tok", runThreadsScheduleRun,
		"o/r", "--path", ".spindle/workflows/ci.yml", "--cron", "0 3 * * *"); code != exitOK || !strings.Contains(out, "run9") {
		t.Fatalf("run-now = %d %q", code, out)
	}
}

func TestThreadsExplainLocalFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ci.yml")
	body := "name: CI\non: [push]\njobs:\n  build:\n    runs-on: linux\n    steps:\n      - run: echo hi\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c := isolateCLI(t, "http://127.0.0.1:1")
	var out, errOut bytes.Buffer
	c.out, c.errOut = &out, &errOut
	code := runThreadsExplain(t.Context(), c, []string{path})
	if code != exitOK || !strings.Contains(out.String(), "build") || !strings.Contains(out.String(), "push") {
		t.Fatalf("explain = %d %q (%s)", code, out.String(), errOut.String())
	}
}
