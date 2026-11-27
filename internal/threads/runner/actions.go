package runner

// Remote action execution (PAR-071/072/074): materialize the fetched, digest-
// pinned file set into the job container, probe the JavaScript runtime the
// action declares, and build a Dockerfile action on the runner host.
//
// The content a runner executes is exactly the bytes the server resolved and
// hashed; nothing here reads the repository or the network. Materialization
// targets a runner-owned directory in the container's own rootfs (not the
// workspace volume), so a checked-out repository cannot pre-create the path or
// turn an ancestor into a symlink.

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/threads"
)

// materializeAction extracts one remote action's files into dir inside the job
// container. It is idempotent per job: the digest directory is immutable, so
// the first step that needs it pays for the extraction and later steps (a
// post hook, another invocation) reuse it.
func (e *Executor) materializeAction(ctx context.Context, plan *Plan, dir, label string, files []threads.ActionFile) error {
	if len(files) == 0 {
		return nil
	}
	if plan.markMaterialized(dir) {
		return nil
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	total := 0
	for _, f := range files {
		name := strings.TrimPrefix(f.Path, "./")
		if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) ||
			name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") {
			return fmt.Errorf("action %s: file %q escapes the action directory", label, f.Path)
		}
		mode := int64(0o644)
		if f.Mode&0o111 != 0 {
			mode = 0o755
		}
		hdr := &tar.Header{Name: name, Mode: mode, Size: int64(len(f.Data)), Typeflag: tar.TypeReg}
		// Writes into a bytes.Buffer cannot fail; the only failure mode left is
		// the container-side extract below.
		_ = tw.WriteHeader(hdr)
		_, _ = tw.Write(f.Data)
		total += len(f.Data)
	}
	_ = tw.Close()
	argv := []string{"exec", "-i", plan.Names.Container, "sh", "-c",
		`rm -rf "$1" && mkdir -p "$1" && tar -xf - -C "$1"`, "sh", dir}
	code, err := e.Docker.StreamIn(ctx, nil, &buf, io.Discard, argv...)
	if err != nil || code != 0 {
		return fmt.Errorf("action %s: extract %d bytes into %s: exit %d: %v", label, total, dir, code, err)
	}
	return nil
}

// ensureNodeRuntime probes the job image's node and enforces the action's
// declared minimum. A missing or too-old runtime is a NAMED failure: a
// JavaScript action is never silently skipped.
func (e *Executor) ensureNodeRuntime(ctx context.Context, plan *Plan, actionUsing, label string) error {
	if plan.exec != nil {
		plan.exec.mu.Lock()
		if plan.exec.probedNode {
			version, perr := plan.exec.nodeVersion, plan.exec.nodeErr
			plan.exec.mu.Unlock()
			return nodeRuntimeVerdict(actionUsing, label, version, perr)
		}
		plan.exec.mu.Unlock()
	}
	out, stderr, code, err := e.Docker.Run(ctx, nil, "exec", plan.Names.Container, "sh", "-c", `node --version`)
	version := strings.TrimSpace(out)
	if version == "" {
		version = strings.TrimSpace(stderr)
	}
	var perr error
	if err != nil || code != 0 {
		perr = fmt.Errorf("no node runtime in the job image (node --version: exit %d)", code)
		version = ""
	}
	if plan.exec != nil {
		plan.exec.mu.Lock()
		plan.exec.probedNode, plan.exec.nodeVersion, plan.exec.nodeErr = true, version, perr
		plan.exec.mu.Unlock()
	}
	return nodeRuntimeVerdict(actionUsing, label, version, perr)
}

// nodeRuntimeVerdict renders the named refusal for one declared runtime.
func nodeRuntimeVerdict(actionUsing, label, version string, probeErr error) error {
	want := 20
	if actionUsing == "node24" {
		want = 24
	}
	if probeErr != nil {
		return fmt.Errorf("action %s declares using: %s, which needs a Node %d runtime in the job image, but %v; use an image with node (for example node:22) or a docker action — the step is refused, never skipped",
			label, actionUsing, want, probeErr)
	}
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	majorPart := v
	if i := strings.IndexByte(v, '.'); i > 0 {
		majorPart = v[:i]
	}
	major, err := strconv.Atoi(majorPart)
	if err != nil {
		return fmt.Errorf("action %s declares using: %s, but the job image's node reported %q (unparseable version); use an image with Node %d or a docker action",
			label, actionUsing, version, want)
	}
	if major < want {
		return fmt.Errorf("action %s declares using: %s, but the job image's node is %s (needs Node >= %d); use a newer image or a docker action",
			label, actionUsing, version, want)
	}
	return nil
}

// buildDockerfileAction builds one Dockerfile action on the runner host and
// returns the local image tag. The build context is exactly the fetched action
// files (written to a temporary host directory), never a checkout, and the
// resulting image is local-only: cleanup removes it after the job. No job
// container ever receives a docker socket (D-8).
func (e *Executor) buildDockerfileAction(ctx context.Context, plan *Plan, step StepPlan, stream StepStream) (string, error) {
	d := step.Docker
	if d == nil || d.Dockerfile == "" {
		return "", fmt.Errorf("internal: no Dockerfile action")
	}
	dir, err := os.MkdirTemp("", "spindle-action-")
	if err != nil {
		return "", fmt.Errorf("create the action build context: %v", err)
	}
	defer os.RemoveAll(dir)
	for _, f := range d.Files {
		rel := strings.TrimPrefix(f.Path, "./")
		if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, `\`) ||
			rel == ".." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
			return "", fmt.Errorf("action file %q escapes the action directory", f.Path)
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", fmt.Errorf("create the action build context: %v", err)
		}
		mode := os.FileMode(0o644)
		if f.Mode&0o111 != 0 {
			mode = 0o755
		}
		if err := os.WriteFile(target, f.Data, mode); err != nil {
			return "", fmt.Errorf("write the action build context: %v", err)
		}
	}
	name := "sp-action-" + plan.Names.Container + "-" + strconv.Itoa(step.OutputIndex+1)
	dockerfile := filepath.Join(dir, filepath.FromSlash(d.Dockerfile))
	argv := []string{"build", "--tag", name, "--file", dockerfile, dir}
	stream.Systemf("spindle: building docker action %s from %s", step.Name, d.Dockerfile)
	code, berr := e.Docker.Stream(ctx, nil, stream, argv...)
	if berr != nil || code != 0 {
		return "", fmt.Errorf("build the docker action image: exit %d: %v", code, berr)
	}
	plan.addBuiltImage(name)
	return name, nil
}

// runPostSteps runs the registered action post hooks in REVERSE order after the
// last main step (GitHub's ordering, PAR-071). A hook runs only when its main
// step actually started (a skipped step's action never initialized), and its
// post-if is evaluated against the job's status at this point — so a post hook
// runs after a FAILED main. A cancelled job skips hooks with a stated line
// (there is no reliable way to run more container work after cancellation).
func (e *Executor) runPostSteps(ctx context.Context, plan *Plan, stream *Streamer, steps *stepOutcomes,
	cmds *commandFiles, jobCmds *jobCommands, summary *jobSummary, res *Result) {
	if plan == nil || len(plan.Posts) == 0 {
		return
	}
	failed := res.Conclusion == threads.JobFailure
	cancelled := ctx.Err() != nil || stream.Cancelled()
	for i := len(plan.Posts) - 1; i >= 0; i-- {
		post := plan.Posts[i]
		if !plan.wasRan(post.Gate) {
			continue
		}
		if cancelled {
			stream.Systemf("spindle: post hook %q skipped (the job was cancelled)", post.Step.Name)
			continue
		}
		cond, err := EvalIf(post.Step.If, e.exprContext(plan, failed, stream, steps, nil))
		if err != nil {
			res.Conclusion = threads.JobFailure
			res.Error = joinErrors(res.Error, fmt.Sprintf("post hook %q: %v", post.Step.Name, err))
			stream.Systemf("spindle: post hook %q: %v", post.Step.Name, err)
			continue
		}
		if !cond {
			stream.Systemf("spindle: post hook %q skipped (if: %s)", post.Step.Name, post.Step.If)
			continue
		}
		outcome, _ := e.runStep(ctx, plan, post.Step, stream, nil, steps, nil, failed, cmds, jobCmds, summary)
		res.Steps = append(res.Steps, outcome)
		if outcome.Conclusion == threads.JobFailure {
			failed = true
			if res.Conclusion != threads.JobCancelled {
				res.Conclusion = threads.JobFailure
			}
			res.Error = joinErrors(res.Error, fmt.Sprintf("post hook %q: %s", post.Step.Name, outcome.Error))
		}
	}
}

// logAction states one action's resolution record in the job log, once per
// job (the same action is often used by several steps).
func (p *Plan) logAction(stream StepStream, o *threads.ActionOrigin) {
	if p == nil || o == nil || p.exec == nil {
		return
	}
	key := o.Repository + "/" + o.Path + "@" + o.Commit
	p.exec.mu.Lock()
	if p.exec.loggedActions == nil {
		p.exec.loggedActions = map[string]bool{}
	}
	seen := p.exec.loggedActions[key]
	p.exec.loggedActions[key] = true
	p.exec.mu.Unlock()
	if seen {
		return
	}
	stream.Systemf("spindle: action %s/%s@%s resolved to commit %s (content %s, policy %s)",
		o.Source, o.Repository, o.Ref, shortSha(o.Commit), o.Digest, o.Policy)
}

// shortSha bounds a commit in a log line (the full sha is on the run header).
func shortSha(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
