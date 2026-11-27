package threads

import (
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads/workflow"
)

func TestSizingRemainingBranches(t *testing.T) {
	if size := ParseJobSize("--cpus 2 --memory-swap 8GiB --memory 4GiB --init"); size.CPUs != 2 || size.MemoryBytes != 4<<30 {
		t.Fatalf("size = %+v", size)
	}
	if size := ParseJobSize("--cpus"); size.CPUs != 0 {
		t.Fatalf("bare option = %+v", size)
	}
	if size := ParseJobSize("--memory 2GiB --memory-swap 8GiB"); size.MemoryBytes != 2<<30 {
		t.Fatalf("swap counted = %+v", size)
	}
	if RunnerOffers(&RunnerDoc{MaxMemoryBytes: 1 << 30}, JobSize{MemoryBytes: 2 << 30}) {
		t.Fatal("memory ceiling not enforced")
	}
	if !RunnerOffers(nil, JobSize{}) || !RunnerOffers(&RunnerDoc{}, JobSize{CPUs: 4}) {
		t.Fatal("undeclared ceiling did not admit the job")
	}
	c, m := clampRunnerSizing(-1, -1)
	if c != 0 || m != 0 {
		t.Fatalf("clamp = %v/%d", c, m)
	}
	if _, ok := parseMemoryBytes(""); ok {
		t.Fatal("empty memory parsed")
	}
	if _, ok := parseMemoryBytes("-5m"); ok {
		t.Fatal("negative memory parsed")
	}
	if got := formatMemory(5 << 20); got != "5 MiB" {
		t.Fatalf("MiB = %q", got)
	}
	if err := ValidateRunnerSizing(0, 0); err != nil {
		t.Fatalf("zero sizing: %v", err)
	}
}

func TestAuthoringRemainingBranches(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.svc.ValidateWorkflowForRepo(ctx(), "badrepo", e.reader(), "x", []byte("on: [push]\n")); err == nil {
		t.Fatal("bad repository accepted")
	}
	e.roles.public = false
	if _, err := e.svc.ValidateWorkflowForRepo(ctx(), "o/r", auth.Anonymous(), "x", []byte("on: [push]\n")); err == nil {
		t.Fatal("anonymous validate on a private repository")
	}
	// Empty source still yields a validation answer (no node to walk).
	if res := ValidateWorkflowSource("empty.yml", nil); res.OK || len(res.Issues) == 0 {
		t.Fatalf("empty source = %+v", res)
	}
	if out := splitYAMLErrors(""); len(out) != 1 {
		t.Fatalf("empty split = %v", out)
	}
	if line, _ := errorLine("line 5"); line != 0 {
		t.Fatalf("line without colon = %d", line)
	}
	if key := keyFromMessage("bad key with spaces: x"); key != "" {
		t.Fatalf("prose key = %q", key)
	}
	if key := keyFromMessage("weird!key: x"); key != "" {
		t.Fatalf("punctuated key = %q", key)
	}
	if key := keyFromMessage("plain: x"); key != "" {
		t.Fatalf("dotless key = %q", key)
	}
}

func TestQueueViewRemainingBranches(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.svc.QueueSnapshotForRepo(ctx(), "badrepo", e.reader()); err == nil {
		t.Fatal("bad repo accepted")
	}
	e.roles.public = false
	if _, err := e.svc.QueueSnapshotForRepo(ctx(), "o/r", auth.Anonymous()); err == nil {
		t.Fatal("private queue read by anonymous")
	}
	e.roles.public = true
	// A corrupt pending entry fails the read.
	corruptKey(t, e, QueueEntryKey("o", "r", "seq"))
	if err := e.svc.setHeadPaused(ctx(), "o", "r", false); err != nil {
		t.Fatal(err)
	}
	// Create a head naming the corrupt entry.
	head := &QueueHead{Seq: "seq", Pending: []string{"seq"}}
	if _, err := store.PutBytes(ctx(), e.store, QueueHeadKey("o", "r"), mustJSON(head), store.PutOptions{Mode: store.PutCreate}); err != nil {
		// A head may already exist from the pause write; overwrite via update.
		if _, uerr := store.PutBytes(ctx(), e.store, QueueHeadKey("o", "r"), mustJSON(head), store.PutOptions{Mode: store.PutUpdate, IfVersion: ""}); uerr != nil {
			t.Skip("cannot rewrite the head in this store")
		}
	}
	if _, err := e.svc.QueueSnapshotForRepo(ctx(), "o/r", e.reader()); err == nil {
		t.Fatal("corrupt entry surfaced no error")
	}
}

func TestCIPauseSettingsBranches(t *testing.T) {
	e := newTestEnv(t)
	e.roles.roles["maint"] = "maintain"
	p := auth.Principal{Name: "maint"}
	// Toggling through the full settings form audits the pause and mirrors it.
	if _, err := e.svc.UpdateSettings(ctx(), "o/r", SettingsInput{CIPaused: true}, p); err != nil {
		t.Fatalf("pause via settings: %v", err)
	}
	head, _, _ := e.svc.loadHead(ctx(), "o", "r")
	if head == nil || !head.Paused {
		t.Fatalf("head = %+v", head)
	}
	// A corrupt settings record fails the dedicated pause write.
	corruptKey(t, e, SettingsKey("o", "r"))
	if _, err := e.svc.SetCIPaused(ctx(), "o/r", false, p); err == nil {
		t.Fatal("corrupt settings pause succeeded")
	}
	// A sound record with the same value is a no-op.
	e.store.Delete(ctx(), SettingsKey("o", "r"), "")
	if _, err := e.svc.SetCIPaused(ctx(), "o/r", false, p); err != nil {
		t.Fatalf("initial pause=false: %v", err)
	}
	if _, err := e.svc.SetCIPaused(ctx(), "o/r", false, p); err != nil {
		t.Fatalf("no-op pause: %v", err)
	}
	// A corrupt queue head only logs when mirroring the marker.
	corruptKey(t, e, QueueHeadKey("o", "r"))
	if _, err := e.svc.SetCIPaused(ctx(), "o/r", true, p); err != nil {
		t.Fatalf("pause with corrupt head: %v", err)
	}
	_ = workflow.Cron{}
	_ = time.Second
}
