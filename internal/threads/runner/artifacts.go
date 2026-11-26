package runner

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// Built-in artifact steps (15 §2.5, docs/threads.md §3.4):
//
//	uses: actions/upload-artifact
//	with: {name, path, if-no-files-found, retention-days}
//
//	uses: actions/download-artifact
//	with: {name, path}
//
// Both run in the JOB CONTAINER and travel over the existing authenticated job
// channel: the container is listed with `find`, its files are streamed out as a
// tar through `docker exec`, and the runner PUTs each one with the attempt
// token. Download is the reverse: the runner GETs each blob and pipes a tar into
// `tar -xf -` inside the container. The job container never needs network
// access to the object store, and the bucket is never the container's peer.
//
// Supported `path` patterns (exact, documented in docs/threads.md §3.4):
//
//   - an exact relative path to a file;
//   - a relative directory: every regular file beneath it, recursively;
//   - a glob with `*`, `?`, `[abc]` (one path segment) and `**` (any number of
//     segments, including none), e.g. `dist/**/*.js`;
//   - several patterns, one per line (or comma-separated: a `path` value may
//     use either separator).
//
// Patterns are matched against workspace-relative, slash-separated paths and
// are case-sensitive. Absolute paths, `..` segments and backslashes are named
// errors. A glob that matches a DIRECTORY includes the files beneath it (the
// GitHub rule). Only regular files are uploaded: symlinks are neither followed
// nor archived, so a pattern can never publish bytes from outside the
// workspace. The job image must provide `find` and `tar` (busybox does; the
// default alpine/git image has both) — a missing tool is a named step error.

// ArtifactStep is one planned artifact action.
type ArtifactStep struct {
	Name string
	// Patterns are the resolved upload patterns (upload only).
	Patterns []string
	// IfNoFilesFound is warn | error | ignore.
	IfNoFilesFound string
	RetentionDays  int
	// Destination is the download's container path (absolute, resolved under
	// the workspace when relative).
	Destination string
}

// Artifact action defaults (the GitHub spellings).
const defaultArtifactName = "artifact"

// parseUploadArtifact validates `with:` for upload-artifact. Every key outside
// the supported set is a NAMED error (never silently ignored): a typo such as
// `retention_day` must not look like a working workflow.
func parseUploadArtifact(p *Plan, with map[string]string) (*ArtifactStep, error) {
	step := &ArtifactStep{Name: defaultArtifactName, IfNoFilesFound: "warn"}
	for key, val := range with {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "name":
			step.Name = strings.TrimSpace(val)
		case "path":
			patterns, err := splitArtifactPatterns(val)
			if err != nil {
				return nil, err
			}
			step.Patterns = patterns
		case "if-no-files-found":
			switch strings.ToLower(strings.TrimSpace(val)) {
			case "warn", "error", "ignore":
				step.IfNoFilesFound = strings.ToLower(strings.TrimSpace(val))
			default:
				return nil, fmt.Errorf("actions/upload-artifact: if-no-files-found must be warn, error or ignore (got %q)", val)
			}
		case "retention-days":
			days, err := parseRetentionDays(val)
			if err != nil {
				return nil, err
			}
			step.RetentionDays = days
		default:
			return nil, fmt.Errorf("actions/upload-artifact: unsupported with key %q (supported: name, path, if-no-files-found, retention-days)", key)
		}
	}
	if len(step.Patterns) == 0 {
		return nil, fmt.Errorf("actions/upload-artifact: path is required (a file, a directory, or a glob)")
	}
	if err := validateArtifactName(step.Name); err != nil {
		return nil, err
	}
	return step, nil
}

// parseDownloadArtifact validates `with:` for download-artifact.
func parseDownloadArtifact(p *Plan, with map[string]string) (*ArtifactStep, error) {
	step := &ArtifactStep{Name: defaultArtifactName, Destination: p.Workspace}
	for key, val := range with {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "name":
			step.Name = strings.TrimSpace(val)
		case "path":
			dir := strings.TrimSpace(val)
			if dir == "" {
				dir = "."
			}
			dest, err := artifactDestination(p.Workspace, dir)
			if err != nil {
				return nil, err
			}
			step.Destination = dest
		default:
			return nil, fmt.Errorf("actions/download-artifact: unsupported with key %q (supported: name, path)", key)
		}
	}
	if err := validateArtifactName(step.Name); err != nil {
		return nil, err
	}
	return step, nil
}

// validateArtifactName mirrors the server's rule, so a bad name fails the step
// before an upload or a lookup.
func validateArtifactName(name string) error {
	if name == "" {
		return fmt.Errorf("artifact name is empty")
	}
	if len(name) > threads.MaxArtifactNameLen {
		return fmt.Errorf("artifact name %q is longer than %d bytes", name, threads.MaxArtifactNameLen)
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return fmt.Errorf("artifact name %q must use letters, digits, dot, dash or underscore", name)
		}
		if i == 0 && (r == '.' || r == '-') {
			return fmt.Errorf("artifact name %q must not start with %q", name, string(r))
		}
	}
	return nil
}

// parseRetentionDays accepts 0 (the run's window) or 1..90.
func parseRetentionDays(val string) (int, error) {
	var days int
	if _, err := fmt.Sscanf(strings.TrimSpace(val), "%d", &days); err != nil {
		return 0, fmt.Errorf("actions/upload-artifact: retention-days must be an integer (got %q)", val)
	}
	if days < 0 || days > threads.MaxArtifactRetentionDays {
		return 0, fmt.Errorf("actions/upload-artifact: retention-days must be 0 (the run's window) or 1..%d (got %d)",
			threads.MaxArtifactRetentionDays, days)
	}
	return days, nil
}

// splitArtifactPatterns splits a `path` value on newlines and commas (a
// comma-separated list is the shape older workflows use; a newline is the
// modern YAML block).
func splitArtifactPatterns(raw string) ([]string, error) {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		for _, part := range strings.Split(line, ",") {
			pat := strings.TrimSpace(part)
			if pat == "" {
				continue
			}
			if strings.HasPrefix(pat, "/") {
				return nil, fmt.Errorf("actions/upload-artifact: path %q must be relative to the workspace", pat)
			}
			if strings.HasPrefix(pat, "~") {
				return nil, fmt.Errorf("actions/upload-artifact: path %q must not use ~", pat)
			}
			clean := path.Clean(pat)
			if clean == ".." || strings.HasPrefix(clean, "../") {
				return nil, fmt.Errorf("actions/upload-artifact: path %q escapes the workspace", pat)
			}
			out = append(out, strings.TrimPrefix(pat, "./"))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("actions/upload-artifact: path is required")
	}
	return out, nil
}

// artifactDestination resolves the download destination inside the workspace.
func artifactDestination(workspace, dir string) (string, error) {
	clean := path.Clean(strings.TrimSpace(dir))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("actions/download-artifact: path %q escapes the workspace", dir)
	}
	if strings.HasPrefix(clean, "/") {
		// An absolute container path is inside the container by definition; it
		// cannot escape the workspace mount because the extraction is the
		// container's own tar.
		return clean, nil
	}
	if clean == "." {
		return workspace, nil
	}
	return path.Join(workspace, clean), nil
}

// --- path matching ---------------------------------------------------------

// matchArtifactPaths expands the patterns against the workspace's file list and
// returns the workspace-relative paths to upload, sorted and deduplicated.
func matchArtifactPaths(files, patterns []string) ([]string, error) {
	selected := map[string]bool{}
	for _, pattern := range patterns {
		pat := path.Clean(strings.TrimSpace(pattern))
		if pat == "." {
			for _, f := range files {
				selected[f] = true
			}
			continue
		}
		pat = strings.TrimPrefix(pat, "./")
		if !hasGlobMeta(pat) {
			for _, f := range files {
				if f == pat {
					selected[f] = true
				} else if strings.HasPrefix(f, pat+"/") {
					// A directory: every file beneath it.
					selected[f] = true
				}
			}
			continue
		}
		re, err := globRegexp(pat)
		if err != nil {
			return nil, fmt.Errorf("actions/upload-artifact: invalid pattern %q: %v", pattern, err)
		}
		for _, f := range files {
			for _, candidate := range pathCandidates(f) {
				if re.MatchString(candidate) {
					selected[f] = true
					break
				}
			}
		}
	}
	out := make([]string, 0, len(selected))
	for f := range selected {
		out = append(out, f)
	}
	sort.Strings(out)
	return out, nil
}

// pathCandidates lists a file path and each of its ancestor directories most
// specific first, so a pattern matching a directory ("dist/*" matching "dist")
// selects the files beneath it.
func pathCandidates(file string) []string {
	out := []string{file}
	for {
		dir := path.Dir(out[len(out)-1])
		if dir == "." || dir == "/" || dir == out[len(out)-1] {
			return out
		}
		out = append(out, dir)
	}
}

// hasGlobMeta reports whether a pattern carries glob syntax.
func hasGlobMeta(pat string) bool {
	return strings.ContainsAny(pat, "*?[")
}

// globRegexp compiles an artifact glob: `**` spans segments (including none),
// `*` and `?` stay inside one segment, `[...]` is a character class.
func globRegexp(pat string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		switch c {
		case '*':
			if i+1 < len(pat) && pat[i+1] == '*' {
				i++
				if i+1 < len(pat) && pat[i+1] == '/' {
					i++
					b.WriteString("(?:[^/]+/)*")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(pat[i+1:], ']')
			if end < 0 {
				return nil, fmt.Errorf("unterminated character class")
			}
			class := pat[i+1 : i+1+end]
			class = strings.ReplaceAll(class, `\`, "")
			b.WriteString("[" + class + "]")
			i += end + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// --- executor --------------------------------------------------------------

// ArtifactClient is the runner's artifact transport (the Client implements it).
// It is an interface so the executor's unit tests need no HTTP server.
type ArtifactClient interface {
	PutArtifactBlob(ctx context.Context, token, jobRef string, attempt int, name string, index int, filePath string, size int64, body io.Reader) (*threads.ArtifactBlobView, error)
	FinalizeArtifact(ctx context.Context, token, jobRef string, attempt int, in threads.ArtifactFinalizeInput) (*threads.ArtifactView, error)
	ArtifactManifest(ctx context.Context, token, jobRef string, attempt int, name string) (*threads.Artifact, error)
	GetArtifactBlob(ctx context.Context, token, jobRef string, attempt int, name string, index int) (io.ReadCloser, int64, error)
}

// maxWorkspaceListing bounds the `find` listing (a runaway workspace must not
// exhaust the runner's memory before a cap could refuse it).
const maxWorkspaceListing = 8 << 20

// uploadArtifact runs actions/upload-artifact.
func (e *Executor) uploadArtifact(ctx context.Context, plan *Plan, step StepPlan, stream *Streamer) error {
	art := step.Artifact
	if art == nil {
		return fmt.Errorf("actions/upload-artifact: no plan (runner bug)")
	}
	if e.Artifacts == nil {
		return fmt.Errorf("actions/upload-artifact: the runner has no artifact transport configured")
	}
	files, err := e.listWorkspace(ctx, plan)
	if err != nil {
		return err
	}
	matches, err := matchArtifactPaths(files, art.Patterns)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		msg := fmt.Sprintf("spindle: no files matched %q for artifact %q", strings.Join(art.Patterns, ", "), art.Name)
		switch art.IfNoFilesFound {
		case "error":
			return fmt.Errorf("actions/upload-artifact: no files were found with the provided path: %s (if-no-files-found: error)",
				strings.Join(art.Patterns, ", "))
		case "ignore":
			stream.Systemf("%s (if-no-files-found: ignore)", msg)
			return nil
		default:
			stream.Systemf("%s (if-no-files-found: warn)", msg)
			return nil
		}
	}
	caps := artifactCaps(plan.ArtifactCaps)
	if len(matches) > caps.MaxFiles {
		return fmt.Errorf("actions/upload-artifact: %d files matched, over the cap of %d", len(matches), caps.MaxFiles)
	}
	stream.Systemf("spindle: uploading artifact %q (%d file(s))", stream.Mask(art.Name), len(matches))

	// One `tar` stream carries every file out of the container. Docker's exec
	// writes the archive to the pipe; this goroutine reads entries and uploads
	// each one, so no whole-artifact copy ever sits on the runner's disk.
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	argv := append([]string{"exec", "-i", plan.Names.Container, "tar", "-cf", "-", "-C", plan.Workspace, "--"}, matches...)
	go func() {
		code, derr := e.Docker.StreamIn(ctx, nil, nil, pw, argv...)
		if derr != nil && code != 0 {
			derr = fmt.Errorf("tar in the job container: exit %d: %v", code, derr)
		}
		pw.CloseWithError(derr)
		done <- derr
	}()

	tr := tar.NewReader(pr)
	uploaded := make([]threads.ArtifactFile, 0, len(matches))
	var total int64
	index := 0
	var uploadErr error
	for {
		hdr, terr := tr.Next()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			uploadErr = fmt.Errorf("read the artifact archive: %w", terr)
			break
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		rel, perr := cleanArtifactPathFromTar(hdr.Name)
		if perr != nil {
			uploadErr = perr
			break
		}
		if hdr.Size > caps.MaxBytesPerFile {
			uploadErr = fmt.Errorf("actions/upload-artifact: %q is %s, over the per-file cap of %s (threads.max_artifact_bytes_per_file)",
				rel, humanSize(hdr.Size), humanSize(caps.MaxBytesPerFile))
			break
		}
		if total+hdr.Size > caps.MaxBytesPerRun {
			uploadErr = fmt.Errorf("actions/upload-artifact: the artifact would exceed the per-run cap of %s (threads.max_artifact_bytes_per_run)",
				humanSize(caps.MaxBytesPerRun))
			break
		}
		view, uerr := e.Artifacts.PutArtifactBlob(ctx, plan.JobToken, plan.JobRef, plan.Attempt,
			art.Name, index, rel, hdr.Size, io.LimitReader(tr, hdr.Size))
		if uerr != nil {
			uploadErr = fmt.Errorf("upload %q: %w", rel, uerr)
			break
		}
		total += hdr.Size
		uploaded = append(uploaded, threads.ArtifactFile{Path: rel, Size: hdr.Size, SHA256: view.SHA256, Index: index})
		index++
	}
	// Stop the container's tar promptly on a refusal: closing the reader makes
	// the next write fail, so a capped upload never streams the rest.
	if uploadErr != nil {
		_ = pr.CloseWithError(uploadErr)
	}
	if derr := <-done; derr != nil && uploadErr == nil {
		uploadErr = derr
	}
	if uploadErr != nil {
		return uploadErr
	}
	if len(uploaded) == 0 {
		return fmt.Errorf("actions/upload-artifact: the archive carried no regular files")
	}
	if _, err := e.Artifacts.FinalizeArtifact(ctx, plan.JobToken, plan.JobRef, plan.Attempt, threads.ArtifactFinalizeInput{
		Name: art.Name, Files: uploaded, RetentionDays: art.RetentionDays,
	}); err != nil {
		return fmt.Errorf("finalize artifact %q: %w", art.Name, err)
	}
	stream.Systemf("spindle: uploaded artifact %q: %d file(s), %s", stream.Mask(art.Name), len(uploaded), humanSize(total))
	return nil
}

// listWorkspace lists the job container's regular files (workspace-relative).
func (e *Executor) listWorkspace(ctx context.Context, plan *Plan) ([]string, error) {
	out, stderr, code, err := e.Docker.Run(ctx, nil,
		"exec", "--workdir", plan.Workspace, plan.Names.Container, "find", ".", "-type", "f")
	if err != nil || code != 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = strings.TrimSpace(out)
		}
		return nil, fmt.Errorf("actions/upload-artifact: list the workspace (the job image needs `find`): exit %d: %s", code, msg)
	}
	if len(out) > maxWorkspaceListing {
		return nil, fmt.Errorf("actions/upload-artifact: the workspace listing exceeds %s; select a narrower path",
			humanSize(maxWorkspaceListing))
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "./")
		if line == "" || strings.HasPrefix(line, "../") {
			continue
		}
		files = append(files, line)
	}
	return files, nil
}

// cleanArtifactPathFromTar validates a tar entry name as a workspace-relative
// path (the tar stream is produced by the container's own tar, so it is still
// untrusted input).
func cleanArtifactPathFromTar(name string) (string, error) {
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("actions/upload-artifact: the archive names an unsafe path %q", name)
	}
	return clean, nil
}

// downloadArtifact runs actions/download-artifact: fetch the manifest, stream
// each blob into one tar, and extract it under the destination directory.
func (e *Executor) downloadArtifact(ctx context.Context, plan *Plan, step StepPlan, stream *Streamer) error {
	art := step.Artifact
	if art == nil {
		return fmt.Errorf("actions/download-artifact: no plan (runner bug)")
	}
	if e.Artifacts == nil {
		return fmt.Errorf("actions/download-artifact: the runner has no artifact transport configured")
	}
	manifest, err := e.Artifacts.ArtifactManifest(ctx, plan.JobToken, plan.JobRef, plan.Attempt, art.Name)
	if err != nil {
		return fmt.Errorf("actions/download-artifact: artifact %q: %w", art.Name, err)
	}
	caps := artifactCaps(plan.ArtifactCaps)
	if manifest.TotalSize > caps.MaxBytesPerRun {
		return fmt.Errorf("actions/download-artifact: artifact %q is %s, over the download cap of %s",
			art.Name, humanSize(manifest.TotalSize), humanSize(caps.MaxBytesPerRun))
	}
	if _, _, code, derr := e.Docker.Run(ctx, nil, "exec", plan.Names.Container, "mkdir", "-p", art.Destination); derr != nil || code != 0 {
		return fmt.Errorf("actions/download-artifact: create %s in the job container: %v", art.Destination, derr)
	}
	stream.Systemf("spindle: downloading artifact %q (%d file(s)) to %s", stream.Mask(art.Name), len(manifest.Files), art.Destination)

	pr, pw := io.Pipe()
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- writeArtifactTar(ctx, pw, e.Artifacts, plan, art.Name, manifest)
	}()
	code, derr := e.Docker.StreamIn(ctx, nil, pr, stream, "exec", "-i", plan.Names.Container, "tar", "-xf", "-", "-C", art.Destination)
	_ = pr.CloseWithError(context.Canceled)
	werr := <-writeDone
	if derr != nil || code != 0 {
		// The container's tar refused the archive (a missing `tar`, a
		// permission failure): its exit is the message, and the writer's broken
		// pipe is a consequence.
		return fmt.Errorf("actions/download-artifact: extract in the job container (the job image needs `tar`): exit %d: %v", code, derr)
	}
	if werr != nil {
		return werr
	}
	stream.Systemf("spindle: downloaded artifact %q", stream.Mask(art.Name))
	return nil
}

// writeArtifactTar streams the artifact's blobs as a tar archive.
func writeArtifactTar(ctx context.Context, w *io.PipeWriter, client ArtifactClient, plan *Plan, name string, manifest *threads.Artifact) error {
	tw := tar.NewWriter(w)
	abort := func(err error) error {
		_ = tw.Close()
		_ = w.CloseWithError(err)
		return err
	}
	for _, f := range manifest.Files {
		if ctx.Err() != nil {
			return abort(ctx.Err())
		}
		body, size, err := client.GetArtifactBlob(ctx, plan.JobToken, plan.JobRef, plan.Attempt, name, f.Index)
		if err != nil {
			return abort(fmt.Errorf("download %q: %w", f.Path, err))
		}
		if size != f.Size {
			_ = body.Close()
			return abort(fmt.Errorf("download %q: %s arrived, the manifest says %s", f.Path, humanSize(size), humanSize(f.Size)))
		}
		hdr := &tar.Header{Name: f.Path, Mode: 0o644, Size: f.Size, Typeflag: tar.TypeReg, ModTime: time.Now()}
		if herr := tw.WriteHeader(hdr); herr != nil {
			_ = body.Close()
			return abort(herr)
		}
		n, cerr := io.Copy(tw, io.LimitReader(body, f.Size))
		_ = body.Close()
		if cerr != nil {
			return abort(cerr)
		}
		if n != f.Size {
			return abort(fmt.Errorf("download %q: ended after %d of %d bytes", f.Path, n, f.Size))
		}
	}
	if err := tw.Close(); err != nil {
		_ = w.CloseWithError(err)
		return err
	}
	return w.Close()
}

// artifactCaps resolves zero-valued caps to the service defaults.
func artifactCaps(c threads.ArtifactCaps) threads.ArtifactCaps {
	if c.MaxBytesPerRun <= 0 {
		c.MaxBytesPerRun = threads.DefaultMaxArtifactBytesPerRun
	}
	if c.MaxBytesPerFile <= 0 {
		c.MaxBytesPerFile = threads.DefaultMaxArtifactBytesPerFile
	}
	if c.MaxFiles <= 0 {
		c.MaxFiles = threads.MaxArtifactFiles
	}
	if c.MaxBytesPerFile > c.MaxBytesPerRun {
		c.MaxBytesPerFile = c.MaxBytesPerRun
	}
	return c
}

// humanSize renders a byte count for a step message.
func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
