package threads

// PAR-190: the whole-run log download and the frame record. The download's
// subject is completeness and honesty — every job has an entry (even an empty
// one), markers keep their position, and a sequence gap is named rather than
// closed up.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

// putLogFrame writes one stored frame the way the runner's AppendLog does (a
// record body under the sequence key).
func putLogFrame(t *testing.T, e *testEnv, runID, jobID string, attempt, seq int, rec LogRecord) {
	t.Helper()
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	_, perr := store.PutBytes(context.Background(), e.store,
		LogKey("o", "r", runID, jobID, attempt, seq), raw,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"})
	if perr != nil {
		t.Fatalf("store frame %d: %v", seq, perr)
	}
}

func TestRunLogsDownloadsEveryJobIncludingEmpty(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs = %d, %v", len(jobs), err)
	}
	build, test := jobs[0], jobs[1]
	putLogFrame(t, e, run.ID, build.ID, 1, 1, LogRecord{Time: "2026-11-20T12:00:01.5Z", Stream: "output", Text: "building\n"})
	putLogFrame(t, e, run.ID, build.ID, 1, 2, LogRecord{Time: "2026-11-20T12:00:02Z", Kind: LogKindStepStart, Step: 1, Name: "Build"})
	putLogFrame(t, e, run.ID, build.ID, 1, 3, LogRecord{Kind: LogKindGroup, Name: "compile"})
	putLogFrame(t, e, run.ID, build.ID, 1, 4, LogRecord{Time: "2026-11-20T12:00:03Z", Stream: "output", Text: "ok\n"})
	putLogFrame(t, e, run.ID, build.ID, 1, 5, LogRecord{Kind: LogKindGroupEnd})
	putLogFrame(t, e, run.ID, build.ID, 1, 6, LogRecord{Kind: LogKindStepEnd, Step: 1, Name: "Build", Conclusion: JobSuccess, DurationMS: 1500})

	bundle, err := e.svc.RunLogs(ctx(), "o/r", run.ID, e.reader())
	if err != nil {
		t.Fatalf("RunLogs: %v", err)
	}
	if bundle.Run == nil || bundle.Run.ID != run.ID {
		t.Fatalf("bundle run = %+v", bundle.Run)
	}
	if len(bundle.Jobs) != 2 {
		t.Fatalf("bundle jobs = %d, want every job (2)", len(bundle.Jobs))
	}
	if bundle.Jobs[1].JobID != test.ID {
		t.Fatalf("second job = %s, want %s", bundle.Jobs[1].JobID, test.ID)
	}
	// A job that produced no output still has an entry that says so.
	text := RenderJobLog(bundle.Jobs[1])
	if !strings.Contains(text, "(no log output)") {
		t.Fatalf("an empty job's entry does not say so:\n%s", text)
	}
	// Markers and text keep their stored order and spelling.
	buildText := RenderJobLog(bundle.Jobs[0])
	for _, want := range []string{"building\n", "▸ step 1: Build", "▸ group: compile", "ok\n", "◂ group end", "◂ step 1: success in 1.5s"} {
		if !strings.Contains(buildText, want) {
			t.Fatalf("rendered build log misses %q:\n%s", want, buildText)
		}
	}
	if strings.Index(buildText, "▸ group: compile") > strings.Index(buildText, "ok\n") {
		t.Fatalf("group marker reordered:\n%s", buildText)
	}
}

func TestRenderJobLogNamesASequenceGap(t *testing.T) {
	entry := JobLog{
		JobID: "j-build", Name: "build", State: JobFailure, Attempt: 1,
		Frames: []LogFrame{
			{Seq: 1, Text: "a\n"},
			{Seq: 4, Text: "b\n"},
		},
	}
	text := RenderJobLog(entry)
	if !strings.Contains(text, "log frames 2..3 missing") {
		t.Fatalf("a dropped frame is not named:\n%s", text)
	}
}

func TestWriteRunLogsZipCarriesEveryJobAndAManifest(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	putLogFrame(t, e, run.ID, "j-build", 1, 1, LogRecord{Time: "2026-11-20T12:00:01Z", Stream: "output", Text: "hello\n"})
	bundle, err := e.svc.RunLogs(ctx(), "o/r", run.ID, e.reader())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteRunLogsZip(&buf, bundle); err != nil {
		t.Fatalf("WriteRunLogsZip: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["manifest.txt"] {
		t.Fatalf("zip has no manifest: %v", names)
	}
	for _, job := range bundle.Jobs {
		found := false
		for name := range names {
			if strings.Contains(name, job.JobID) {
				found = true
			}
		}
		if !found {
			t.Fatalf("zip misses job %s: %v", job.JobID, names)
		}
	}
}

func TestRunLogsRequiresTheReadGate(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	e.roles.public = false
	if _, err := e.svc.RunLogs(ctx(), "o/r", run.ID, e.public()); err == nil {
		t.Fatal("an anonymous read of a private repository's logs must be refused")
	}
}

func TestAppendLogRefusesAMalformedRecord(t *testing.T) {
	e := newTestEnv(t)
	for _, rec := range []LogRecord{
		{Kind: "nonsense", Text: "x"},
		{Stream: "stderr", Text: "x"},
		{Time: "yesterday", Text: "x"},
		{Text: strings.Repeat("x", MaxLogFrame+1)},
	} {
		if _, err := e.svc.AppendLog(ctx(), AttemptClaims{}, 1, rec); err == nil {
			t.Fatalf("AppendLog accepted %+v", rec)
		}
	}
}

func TestLogFramesSurfacesAnUnreadableFrame(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	jobs, _ := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	// A body that is not a record (a frame written by an older/corrupt producer)
	// is a visible, sequenced line — never a silent skip.
	_, perr := store.PutBytes(ctx(), e.store, LogKey("o", "r", run.ID, jobs[0].ID, 1, 1),
		[]byte("plain text, not a record"), store.PutOptions{Mode: store.PutCreate})
	if perr != nil {
		t.Fatal(perr)
	}
	page, err := e.svc.LogFrames(ctx(), "o/r", run.ID, jobs[0].ID, 1, 0, 10, e.reader())
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Frames) != 1 || !strings.Contains(page.Frames[0].Text, "unreadable") {
		t.Fatalf("frames = %+v", page.Frames)
	}
}

func TestWriteRunLogsTextNamesTheRun(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	bundle, _ := e.svc.RunLogs(ctx(), "o/r", run.ID, e.reader())
	var buf bytes.Buffer
	if err := WriteRunLogsText(&buf, bundle); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, run.ID) || !strings.Contains(out, "no log output") {
		t.Fatalf("text download = %q", out)
	}
}

func TestRenderJobLogCoversEveryMarker(t *testing.T) {
	entry := JobLog{
		JobID: "j-x", Name: "", State: JobFailure, Attempt: 2, Context: "threads/ci/x",
		Frames: []LogFrame{
			{Seq: 1, Kind: LogKindStepStart, Step: 1, Name: ""},
			{Seq: 2, Stream: "system", Text: "a system line"},
			{Seq: 3, Kind: LogKindGroup, Name: ""},
			{Seq: 4, Stream: "output", Text: "in group\n"},
			{Seq: 5, Kind: LogKindGroupEnd},
			{Seq: 6, Kind: LogKindStepEnd, Step: 1, Conclusion: JobFailure, ExitCode: 3, DurationMS: 250},
			{Seq: 7, Kind: LogKindTruncated, Text: "cut at the cap"},
		},
		Truncated: "entry cap reached",
	}
	out := RenderJobLog(entry)
	for _, want := range []string{
		"(unnamed)", "▸ step 1: (unnamed)", "a system line\n", "▸ group: (unnamed)",
		"in group", "◂ group end", "exit 3", "250ms", "cut at the cap", "entry cap reached",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered log misses %q:\n%s", want, out)
		}
	}
}

func TestLogEntryNameFallsBackForAnUnsluggableName(t *testing.T) {
	if got := logEntryName(JobLog{JobID: "j-x", Name: "!!!"}); got != "logs/j-x-unnamed.txt" {
		t.Fatalf("entry name = %q", got)
	}
}

func TestWriteRunLogsRefusesAnEmptyBundle(t *testing.T) {
	if err := WriteRunLogsText(&bytes.Buffer{}, nil); err == nil {
		t.Fatal("WriteRunLogsText accepted a nil bundle")
	}
	if err := WriteRunLogsZip(&bytes.Buffer{}, nil); err == nil {
		t.Fatal("WriteRunLogsZip accepted a nil bundle")
	}
}

func TestRunLogsNamesAnUnknownRun(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.svc.RunLogs(ctx(), "o/r", "1795176002000000000-nosuch", e.reader()); err == nil {
		t.Fatal("RunLogs accepted an unknown run")
	}
}

func TestRunLogsCapsAJobEntryVisibly(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	jobs, _ := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	// One oversized stored body (the read path does not re-check the write cap)
	// makes the download's cap trip on the FIRST frame, which is the branch that
	// must state itself rather than silently truncate.
	big := strings.Repeat("x", MaxRunLogJobBytes+1)
	putLogFrame(t, e, run.ID, jobs[0].ID, 1, 1, LogRecord{Text: big})
	bundle, err := e.svc.RunLogs(ctx(), "o/r", run.ID, e.reader())
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Jobs[0].Truncated == "" || len(bundle.Jobs[0].Frames) != 0 {
		t.Fatalf("oversized entry = %d frames, truncated=%q", len(bundle.Jobs[0].Frames), bundle.Jobs[0].Truncated)
	}
	if !strings.Contains(RenderJobLog(bundle.Jobs[0]), "download cap") {
		t.Fatal("the cap is not stated in the entry")
	}
}

// failingWriter is the download writer that always fails: the copy path reports
// its error instead of claiming a complete archive.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestWriteRunLogsReportsAWriterError(t *testing.T) {
	bundle := &RunLogBundle{Run: &Run{ID: "r1", State: RunSuccess}, Jobs: []JobLog{{JobID: "j-x", State: JobSuccess}}}
	if err := WriteRunLogsText(failingWriter{}, bundle); err == nil {
		t.Fatal("WriteRunLogsText swallowed a writer error")
	}
	if err := WriteRunLogsZip(failingWriter{}, bundle); err == nil {
		t.Fatal("WriteRunLogsZip swallowed a writer error")
	}
}

// TestRunLogsPagesBeyondOneWindow pins the no-loss half of the download: a job
// with more frames than one LogFrames window still contributes EVERY frame (the
// loop pages by sequence, not one bounded read).
func TestRunLogsPagesBeyondOneWindow(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	jobs, _ := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	const frames = 1005
	for i := 1; i <= frames; i++ {
		putLogFrame(t, e, run.ID, jobs[0].ID, 1, i, LogRecord{Stream: "output", Text: "x"})
	}
	bundle, err := e.svc.RunLogs(ctx(), "o/r", run.ID, e.reader())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(bundle.Jobs[0].Frames); got != frames {
		t.Fatalf("frames = %d, want every frame (%d)", got, frames)
	}
	if last := bundle.Jobs[0].Frames[frames-1].Seq; last != frames {
		t.Fatalf("last seq = %d, want %d", last, frames)
	}
}

// TestRunLogsReportsCorruptOrMalformedInput covers the read errors: a malformed
// repository id, a corrupt run record and a corrupt job record are named errors,
// never an empty archive that looks like a run with no output.
func TestRunLogsReportsCorruptOrMalformedInput(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.svc.RunLogs(ctx(), "noslash", "1795176002000000000-a", e.reader()); err == nil {
		t.Fatal("a malformed repository id was accepted")
	}

	corruptRun := newTestEnv(t)
	run := corruptRun.mustCreate(t)[0]
	if _, perr := store.PutBytes(ctx(), corruptRun.store, RunKey("o", "r", run.ID), []byte("{"), store.PutOptions{}); perr != nil {
		t.Fatal(perr)
	}
	if _, err := corruptRun.svc.RunLogs(ctx(), "o/r", run.ID, corruptRun.reader()); err == nil {
		t.Fatal("a corrupt run record produced an archive")
	}

	corruptJob := newTestEnv(t)
	run2 := corruptJob.mustCreate(t)[0]
	jobs, _ := corruptJob.svc.loadJobs(ctx(), "o", "r", run2.ID)
	if _, perr := store.PutBytes(ctx(), corruptJob.store, JobKey("o", "r", run2.ID, jobs[0].ID), []byte("{"), store.PutOptions{}); perr != nil {
		t.Fatal(perr)
	}
	if _, err := corruptJob.svc.RunLogs(ctx(), "o/r", run2.ID, corruptJob.reader()); err == nil {
		t.Fatal("a corrupt job record produced an archive")
	} else {
		t.Logf("corrupt job error: %v", err)
	}
}

// limitWriter accepts a byte budget and then fails, so the entry loop's error
// paths are reachable (a real disk fills mid-archive, and the caller must see
// the error rather than a short file that looks complete).
type limitWriter struct{ n int }

func (w *limitWriter) Write(p []byte) (int, error) {
	if len(p) <= w.n {
		w.n -= len(p)
		return len(p), nil
	}
	ok := w.n
	w.n = 0
	return ok, errors.New("quota exhausted")
}

func TestWriteRunLogsTextReportsAMidEntryFailure(t *testing.T) {
	entry := JobLog{JobID: "j-x", Name: "x", State: JobSuccess}
	bundle := &RunLogBundle{Run: &Run{ID: "r1", State: RunSuccess}, Jobs: []JobLog{entry}}
	header := len("run r1 ·  ·  · success\n\n")
	entryText := len(RenderJobLog(entry))
	if err := WriteRunLogsText(&limitWriter{n: header + 1}, bundle); err == nil {
		t.Fatal("a failing entry write was swallowed")
	}
	// The newline after a complete entry is also checked.
	if err := WriteRunLogsText(&limitWriter{n: header + entryText}, bundle); err == nil {
		t.Fatal("a failing separator write was swallowed")
	}
}

func TestWriteRunLogsZipReportsAFailureInsideAnEntry(t *testing.T) {
	entry := JobLog{JobID: "j-big", Name: "big", State: JobSuccess}
	// Enough incompressible text to make the deflate writer flush into the
	// failing sink mid-entry (a small entry only fails at Close).
	entry.Frames = []LogFrame{{Seq: 1, Stream: "output", Text: strings.Repeat("x", 200<<10)}}
	bundle := &RunLogBundle{Run: &Run{ID: "r1", State: RunSuccess}, Jobs: []JobLog{entry}}
	if err := WriteRunLogsZip(failingWriter{}, bundle); err == nil {
		t.Fatal("a zip write failure inside an entry was swallowed")
	}
}
