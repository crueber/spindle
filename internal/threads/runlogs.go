package threads

// Whole-run log download (PAR-190): the run page's link and the API twin serve
// one archive carrying EVERY job's log, not only the ones a reader opened. The
// archive is assembled from the same stored frames the live panel reads, so a
// marker, a timestamp and a sequence gap look the same in both places.

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
)

// MaxRunLogJobBytes bounds one job's entry in a whole-run download. A job log
// over the bound is cut at a frame boundary and says so in the entry (a visible
// cap, never a silent truncation).
const MaxRunLogJobBytes = 16 << 20

// JobLog is one job's assembled log inside a whole-run download.
type JobLog struct {
	JobID   string     `json:"job_id"`
	Name    string     `json:"name"`
	Context string     `json:"context"`
	State   string     `json:"state"`
	Attempt int        `json:"attempt"`
	Frames  []LogFrame `json:"frames"`
	// Truncated names the cap when the entry stopped early (empty otherwise).
	Truncated string `json:"truncated,omitempty"`
}

// RunLogBundle is the run header plus every job's current-attempt log.
type RunLogBundle struct {
	Run  *Run
	Jobs []JobLog
}

// RunLogs assembles the whole-run download under the repository read gate. Jobs
// are in workflow order; every job has an entry, including one that produced no
// output (its entry says so — never an omitted file).
func (s *Service) RunLogs(ctx context.Context, repo, runID string, p auth.Principal) (*RunLogBundle, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, name, p); err != nil {
		return nil, err
	}
	run, err := s.loadRun(ctx, owner, name, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("%w: unknown run %q", ErrNotFound, runID)
	}
	jobs, err := s.loadJobs(ctx, owner, name, runID)
	if err != nil {
		return nil, err
	}
	bundle := &RunLogBundle{Run: run, Jobs: make([]JobLog, 0, len(jobs))}
	for _, job := range jobs {
		entry := JobLog{
			JobID: job.ID, Name: job.Name, Context: job.Context, State: job.State,
			Attempt: job.Attempt,
		}
		bytes := 0
		after := 0
		for {
			page, perr := s.LogFrames(ctx, repo, runID, job.ID, job.Attempt, after, 1000, p)
			if perr != nil {
				return nil, perr
			}
			stop := false
			for _, frame := range page.Frames {
				if bytes+len(frame.Text) > MaxRunLogJobBytes {
					entry.Truncated = fmt.Sprintf("download cap: this entry stops at %s; open the job's live log for the rest", humanBytes(MaxRunLogJobBytes))
					stop = true
					break
				}
				entry.Frames = append(entry.Frames, frame)
				bytes += len(frame.Text)
			}
			if stop || !page.More {
				break
			}
			after = page.Frames[len(page.Frames)-1].Seq
		}
		bundle.Jobs = append(bundle.Jobs, entry)
	}
	return bundle, nil
}

// RenderJobLog renders one job's frames as the archive's text: markers become
// the lines a text reader expects, frames keep their captured order, and a
// sequence gap is named rather than closed up.
func RenderJobLog(entry JobLog) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== job %s (%s) · %s ===\n", entry.JobID, displayName(entry.Name), entry.State)
	if entry.Attempt > 0 {
		fmt.Fprintf(&b, "attempt %d", entry.Attempt)
		if entry.Context != "" {
			fmt.Fprintf(&b, " · %s", entry.Context)
		}
		b.WriteString("\n")
	}
	if len(entry.Frames) == 0 {
		b.WriteString("(no log output)\n")
	}
	last := 0
	for _, frame := range entry.Frames {
		if last > 0 && frame.Seq > last+1 {
			fmt.Fprintf(&b, "[spindle: log frames %d..%d missing (%d frame(s)) — the runner could not write them]\n",
				last+1, frame.Seq-1, frame.Seq-last-1)
		}
		switch frame.Kind {
		case LogKindStepStart:
			fmt.Fprintf(&b, "\n▸ step %d: %s\n", frame.Step, displayName(frame.Name))
		case LogKindStepEnd:
			fmt.Fprintf(&b, "◂ step %d: %s%s in %s\n", frame.Step, frame.Conclusion,
				exitSuffix(frame.ExitCode), milliseconds(frame.DurationMS))
		case LogKindGroup:
			fmt.Fprintf(&b, "\n▸ group: %s\n", displayName(frame.Name))
		case LogKindGroupEnd:
			b.WriteString("◂ group end\n")
		case LogKindTruncated:
			b.WriteString(frame.Text)
			b.WriteString("\n")
		default:
			b.WriteString(frame.Text)
			if frame.Stream == "system" && !strings.HasSuffix(frame.Text, "\n") {
				b.WriteString("\n")
			}
		}
		last = frame.Seq
	}
	if entry.Truncated != "" {
		b.WriteString("\n[spindle: " + entry.Truncated + "]\n")
	}
	return b.String()
}

// WriteRunLogsText writes the whole run's logs as one text file.
func WriteRunLogsText(w io.Writer, bundle *RunLogBundle) error {
	if bundle == nil || bundle.Run == nil {
		return fmt.Errorf("%w: no run", ErrNotFound)
	}
	if _, err := fmt.Fprintf(w, "run %s · %s · %s · %s\n\n", bundle.Run.ID, bundle.Run.Workflow, bundle.Run.Commit, bundle.Run.State); err != nil {
		return err
	}
	for _, entry := range bundle.Jobs {
		if _, err := io.WriteString(w, RenderJobLog(entry)); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}
	return nil
}

// WriteRunLogsZip streams the whole run's logs as a zip: one entry per job plus
// a manifest naming what is inside. Entry order is workflow order.
func WriteRunLogsZip(w io.Writer, bundle *RunLogBundle) error {
	if bundle == nil || bundle.Run == nil {
		return fmt.Errorf("%w: no run", ErrNotFound)
	}
	zw := zip.NewWriter(w)
	now := time.Now()
	manifest := &strings.Builder{}
	fmt.Fprintf(manifest, "run %s\nworkflow %s\ncommit %s\nstate %s\n\n", bundle.Run.ID, bundle.Run.Workflow, bundle.Run.Commit, bundle.Run.State)
	for _, entry := range bundle.Jobs {
		name := logEntryName(entry)
		fmt.Fprintf(manifest, "%s\t%s\t%s\n", name, entry.JobID, entry.State)
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: now}
		entryWriter, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(entryWriter, RenderJobLog(entry)); err != nil {
			return err
		}
	}
	hdr := &zip.FileHeader{Name: "manifest.txt", Method: zip.Deflate, Modified: now}
	mw, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(mw, manifest.String()); err != nil {
		return err
	}
	return zw.Close()
}

// logEntryName is one job's archive path: filesystem-safe, unique per job, and
// readable without opening it.
func logEntryName(entry JobLog) string {
	return fmt.Sprintf("logs/%s-%s.txt", entry.JobID, slugSegment(entry.Name))
}

// displayName is a marker's name with a blank fallback (a marker with no name
// still renders as a step/group, never as an empty heading).
func displayName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "(unnamed)"
	}
	return name
}

// exitSuffix renders a non-zero exit code beside a step marker.
func exitSuffix(code int) string {
	if code == 0 {
		return ""
	}
	return " (exit " + strconv.Itoa(code) + ")"
}

// milliseconds renders a marker duration with the page's spelling.
func milliseconds(ms int64) string {
	return (time.Duration(ms) * time.Millisecond).Round(time.Millisecond).String()
}
