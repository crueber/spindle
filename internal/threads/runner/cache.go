package runner

// Built-in cache steps (docs/threads.md §3.4c; PAR-095/PAR-096):
//
//	uses: actions/cache            restore now, save at the end of a green job
//	uses: actions/cache/restore    restore only
//	uses: actions/cache/save       save now
//
// The payload is a tar of the named workspace paths. Restore streams it out of
// the cache service into `tar -xf -` inside the job container; save tars the
// paths out of the container into one temp file, hashing it while writing, then
// streams it to the content-addressed store. Nothing is written outside the job
// container and the runner's own temp file, and the container never talks to
// the object store.
//
// ## Paths
//
// A cache path must be inside the workspace (relative, no `..`, no `~`, no
// backslashes). An absolute path such as `~/.npm` would be container-local
// state that the next job's container does not have: caching it would be a lie,
// so it is refused by name. Point the tool at a workspace directory instead
// (`npm_config_cache=.npm`) — the docs give the recipe per ecosystem.
//
// ## Version
//
// The cache's `version` is derived, not authored: sha256 of the sorted path
// list plus the archive format tag. A workflow that changes the cached paths
// therefore misses instead of restoring a tree that no longer matches.
//
// ## Timing and failure
//
// A restore miss is not a failure. `actions/cache` defers its save to the end of
// the job and saves only when every step succeeded (GitHub's post-step rule,
// minus its always()). A save failure is reported on the log and does not fail
// an otherwise-green job: the job's real work already succeeded, and failing it
// because a cache write failed would be worse than a cold next run.

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/threads"
)

// CacheStep is one planned cache action.
type CacheStep struct {
	// Action is restore | save.
	Action string
	Key    string
	// Version is the derived path fingerprint (see the file comment).
	Version     string
	RestoreKeys []string
	// Paths are the workspace-relative paths the archive holds.
	Paths []string
	// Deferred marks `actions/cache` (save at the end of the job).
	Deferred bool
	// FailOnMiss / LookupOnly are the restore inputs.
	FailOnMiss bool
	LookupOnly bool
	// Hit and Exact record the restore result for the `cache-hit` output and
	// the log (filled by the executor; the plan value is a template).
	Hit   bool
	Exact bool
	// Authority is "cache" (set by the plan) — nothing else.
	_ struct{}
}

// Built-in cache spellings (the `uses:` base names).
const (
	cacheAction        = "actions/cache"
	cacheRestoreAction = "actions/cache/restore"
	cacheSaveAction    = "actions/cache/save"
)

// maxCachePaths bounds one cache action's path list.
const maxCachePaths = 64

// cacheFingerprint derives the cache version from the sorted path list.
func cacheFingerprint(paths []string) string {
	sorted := append([]string{}, paths...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte("spindle-cache-tar-v1\n" + strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:16])
}

// parseCacheStep validates `with:` for one of the three cache actions. Every
// unknown key is a NAMED error (a typo must not look like a cached workflow).
func parseCacheStep(kind, uses string, with map[string]string) (*CacheStep, error) {
	step := &CacheStep{Action: "restore"}
	switch kind {
	case cacheSaveAction:
		step.Action = "save"
	case cacheAction:
		step.Deferred = true
	}
	for key, val := range with {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "path":
			paths, err := splitCachePaths(val)
			if err != nil {
				return nil, err
			}
			step.Paths = paths
		case "key":
			step.Key = strings.TrimSpace(val)
		case "restore-keys":
			step.RestoreKeys = splitCacheRestoreKeys(val)
		case "fail-on-cache-miss":
			on, err := parseCacheBool("fail-on-cache-miss", val)
			if err != nil {
				return nil, err
			}
			step.FailOnMiss = on
		case "lookup-only":
			on, err := parseCacheBool("lookup-only", val)
			if err != nil {
				return nil, err
			}
			step.LookupOnly = on
		case "enablecrossosarchive":
			return nil, fmt.Errorf("%s: enableCrossOsArchive is not supported: every Threads job runs Linux, and a cross-OS archive cannot be produced here", uses)
		default:
			return nil, fmt.Errorf("%s: unsupported with key %q (supported: path, key, restore-keys, fail-on-cache-miss, lookup-only)", uses, key)
		}
	}
	if step.Key == "" && step.Action != "save" {
		return nil, fmt.Errorf("%s: key is required", uses)
	}
	if step.Key == "" {
		return nil, fmt.Errorf("%s: key is required", uses)
	}
	if len(step.Key) > threads.MaxCacheKeyLen {
		return nil, fmt.Errorf("%s: key is longer than %d bytes", uses, threads.MaxCacheKeyLen)
	}
	if len(step.Paths) == 0 {
		return nil, fmt.Errorf("%s: path is required", uses)
	}
	if len(step.RestoreKeys) > threads.MaxCacheRestoreKeys {
		return nil, fmt.Errorf("%s: at most %d restore-keys are accepted", uses, threads.MaxCacheRestoreKeys)
	}
	step.Version = cacheFingerprint(step.Paths)
	return step, nil
}

// splitCachePaths splits a `path` value on newlines/commas and validates every
// entry as a workspace-relative path.
func splitCachePaths(raw string) ([]string, error) {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		for _, part := range strings.Split(line, ",") {
			pat := strings.TrimSpace(part)
			if pat == "" {
				continue
			}
			clean, err := cleanCachePath(pat)
			if err != nil {
				return nil, err
			}
			out = append(out, clean)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("cache: path is required")
	}
	if len(out) > maxCachePaths {
		return nil, fmt.Errorf("cache: %d paths, over the limit of %d", len(out), maxCachePaths)
	}
	return out, nil
}

// cleanCachePath validates one cache path (the tar-slip boundary).
func cleanCachePath(p string) (string, error) {
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("cache: path %q must be relative to the workspace (an absolute path is container-local state the next job does not have)", p)
	}
	if strings.HasPrefix(p, "~") {
		return "", fmt.Errorf("cache: path %q must not use ~ (point the tool at a workspace directory instead, e.g. npm_config_cache=.npm)", p)
	}
	if strings.Contains(p, `\`) {
		return "", fmt.Errorf("cache: path %q must use / separators", p)
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return "", fmt.Errorf("cache: path %q escapes the workspace", p)
	}
	return strings.TrimPrefix(clean, "./"), nil
}

// splitCacheRestoreKeys splits `restore-keys` one per line.
func splitCacheRestoreKeys(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// parseCacheBool accepts the YAML boolean spellings the plan already resolved.
func parseCacheBool(name, val string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no", "":
		return false, nil
	}
	return false, fmt.Errorf("cache: %s must be true or false (got %q)", name, val)
}

// CacheClient is the runner's cache transport (the Client implements it).
type CacheClient interface {
	CacheLookup(ctx context.Context, token, jobRef string, attempt int, in threads.CacheLookupInput) (*threads.CacheView, error)
	GetCacheBlob(ctx context.Context, token, jobRef string, attempt int, sha string) (io.ReadCloser, int64, error)
	PutCacheBlob(ctx context.Context, token, jobRef string, attempt int, sha string, size int64, body io.Reader) error
	SaveCache(ctx context.Context, token, jobRef string, attempt int, in threads.SaveCacheInput) (*threads.CacheView, error)
}

// cacheCaps resolves zero-valued caps to the service defaults.
func cacheCaps(c threads.CacheCaps) threads.CacheCaps {
	if c.MaxBytesPerEntry <= 0 {
		c.MaxBytesPerEntry = threads.DefaultMaxCacheEntryBytes
	}
	if c.MaxBytesPerRepo <= 0 {
		c.MaxBytesPerRepo = c.MaxBytesPerEntry
	}
	if c.MaxEntries <= 0 {
		c.MaxEntries = 1
	}
	return c
}

// restoreCache runs a restore (the `actions/cache` and `actions/cache/restore`
// step path). It returns nil on a miss: a cache-less job is a normal job.
func (e *Executor) restoreCache(ctx context.Context, plan *Plan, step StepPlan, stream StepStream) error {
	cs := step.Cache
	if cs == nil {
		return fmt.Errorf("actions/cache: no plan (runner bug)")
	}
	if e.Caches == nil {
		return fmt.Errorf("actions/cache: the runner has no cache transport configured")
	}
	view, err := e.Caches.CacheLookup(ctx, plan.JobToken, plan.JobRef, plan.Attempt, threads.CacheLookupInput{
		Key: cs.Key, Version: cs.Version, RestoreKeys: cs.RestoreKeys,
	})
	if err != nil {
		return fmt.Errorf("actions/cache: %w", err)
	}
	if !view.Hit {
		cs.Hit, cs.Exact = false, false
		stream.Systemf("spindle: cache miss for key %q (%s)", stream.Mask(cs.Key), view.Reason)
		if cs.FailOnMiss {
			return fmt.Errorf("actions/cache: no cache entry matched key %q (fail-on-cache-miss: true)", cs.Key)
		}
		return nil
	}
	cs.Hit, cs.Exact = true, view.Exact
	if err := e.extractCache(ctx, plan, cs, view, stream); err != nil {
		return err
	}
	hit := "false"
	if view.Exact {
		hit = "true"
	}
	stream.Systemf("spindle: restored cache %q (%s, %s, cache-hit=%s)",
		stream.Mask(view.Key), humanSize(view.Size), view.Scope, hit)
	return nil
}

// extractCache streams one payload into the job container and verifies its
// digest end to end.
func (e *Executor) extractCache(ctx context.Context, plan *Plan, cs *CacheStep, view *threads.CacheView, stream StepStream) error {
	body, size, err := e.Caches.GetCacheBlob(ctx, plan.JobToken, plan.JobRef, plan.Attempt, view.SHA256)
	if err != nil {
		return fmt.Errorf("actions/cache: download %q: %w", view.Key, err)
	}
	defer body.Close()
	if size != view.Size {
		return fmt.Errorf("actions/cache: payload for %q is %s, the index says %s", view.Key, humanSize(size), humanSize(view.Size))
	}
	pr, pw := io.Pipe()
	sum := sha256.New()
	copied := make(chan error, 1)
	go func() {
		n, cerr := io.Copy(io.MultiWriter(pw, sum), body)
		if cerr == nil && n != size {
			cerr = fmt.Errorf("ended after %d of %d bytes", n, size)
		}
		pw.CloseWithError(cerr)
		copied <- cerr
	}()
	code, derr := e.Docker.StreamIn(ctx, nil, pr, stream, "exec", "-i", plan.Names.Container, "tar", "-xf", "-", "-C", plan.Workspace)
	_ = pr.CloseWithError(context.Canceled)
	cerr := <-copied
	if derr != nil || code != 0 {
		return fmt.Errorf("actions/cache: extract in the job container (the job image needs `tar`): exit %d: %v", code, derr)
	}
	if cerr != nil {
		return fmt.Errorf("actions/cache: download %q: %v", view.Key, cerr)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != view.SHA256 {
		return fmt.Errorf("actions/cache: payload for %q hashed to %s, the index says %s (corrupt entry)", view.Key, got, view.SHA256)
	}
	return nil
}

// saveCache tars the planned paths out of the container and commits one entry.
func (e *Executor) saveCache(ctx context.Context, plan *Plan, step StepPlan, stream StepStream) error {
	cs := step.Cache
	if cs == nil {
		return fmt.Errorf("actions/cache: no plan (runner bug)")
	}
	if e.Caches == nil {
		return fmt.Errorf("actions/cache: the runner has no cache transport configured")
	}
	caps := cacheCaps(plan.CacheCaps)
	tmp, err := os.CreateTemp("", "spindle-cache-*.tar")
	if err != nil {
		return fmt.Errorf("actions/cache: create the archive: %w", err)
	}
	name := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}()
	sum := sha256.New()
	limit := &limitWriter{w: io.MultiWriter(tmp, sum), max: caps.MaxBytesPerEntry}
	// A missing path is tar's error, which names it; only an EXISTING path is
	// archived, so an empty directory is an empty archive (GitHub's rule).
	argv := append([]string{"exec", "-i", plan.Names.Container, "tar", "-cf", "-", "-C", plan.Workspace, "--"}, cs.Paths...)
	code, derr := e.Docker.StreamIn(ctx, nil, nil, limit, argv...)
	if limit.over {
		return fmt.Errorf("actions/cache: the archive is over the per-entry cap of %s (threads.cache_max_entry_bytes)", humanSize(caps.MaxBytesPerEntry))
	}
	if derr != nil || code != 0 {
		return fmt.Errorf("actions/cache: tar the paths in the job container (the job image needs `tar`, and every path must exist): exit %d: %v", code, derr)
	}
	size := limit.n
	sha := hex.EncodeToString(sum.Sum(nil))
	stream.Systemf("spindle: saving cache %q (%s)", stream.Mask(cs.Key), humanSize(size))
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("actions/cache: rewind the archive: %w", err)
	}
	if err := e.Caches.PutCacheBlob(ctx, plan.JobToken, plan.JobRef, plan.Attempt, sha, size, io.LimitReader(tmp, size)); err != nil {
		return fmt.Errorf("actions/cache: upload the archive: %w", err)
	}
	// The file count is informational; the tar entry count is cheap to keep.
	if _, err := e.Caches.SaveCache(ctx, plan.JobToken, plan.JobRef, plan.Attempt, threads.SaveCacheInput{
		Key: cs.Key, Version: cs.Version, SHA256: sha, Size: size, Files: countTarFiles(name),
	}); err != nil {
		return fmt.Errorf("actions/cache: commit the entry: %w", err)
	}
	stream.Systemf("spindle: saved cache %q (%s)", stream.Mask(cs.Key), humanSize(size))
	return nil
}

// limitWriter fails the stream once more than max bytes have been written, so a
// runaway cache never fills the runner's disk.
type limitWriter struct {
	w    io.Writer
	max  int64
	n    int64
	over bool
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n+int64(len(p)) > l.max {
		l.over = true
		return 0, fmt.Errorf("archive over the %d-byte cap", l.max)
	}
	n, err := l.w.Write(p)
	l.n += int64(n)
	return n, err
}

// countTarFiles counts the regular files in a tar archive (a diagnostic; an
// unreadable archive reports 0).
func countTarFiles(file string) int {
	f, err := os.Open(file)
	if err != nil {
		return 0
	}
	defer f.Close()
	tr := tar.NewReader(f)
	n := 0
	for {
		hdr, terr := tr.Next()
		if terr != nil {
			return n
		}
		if hdr.Typeflag == tar.TypeReg {
			n++
		}
	}
}

// pendingSaveStep marks one `actions/cache` save for the end of the job.
func (p *Plan) deferCacheSave(cs *CacheStep) {
	if p == nil || cs == nil {
		return
	}
	p.deferred = append(p.deferred, cs)
}

// runDeferredCaches performs the saves `actions/cache` deferred, after the last
// step. Only a green job saves; a failure logs and continues (see the file
// comment on why a cache write never fails a green job).
func (e *Executor) runDeferredCaches(ctx context.Context, plan *Plan, stream StepStream) {
	for _, cs := range plan.deferred {
		if ctx.Err() != nil {
			return
		}
		if e.Caches == nil {
			stream.Systemf("spindle: cache save for %q skipped: the runner has no cache transport", stream.Mask(cs.Key))
			return
		}
		step := StepPlan{Cache: cs, Kind: "cache-save"}
		if err := e.saveCache(ctx, plan, step, stream); err != nil {
			stream.Systemf("spindle: cache save for %q failed (the job stays green): %v", stream.Mask(cs.Key), err)
		}
	}
}

// cacheHitOutput publishes the step's `cache-hit` output (exact match only,
// GitHub's rule).
func cacheHitOutput(step StepPlan) map[string]string {
	if step.Cache == nil {
		return nil
	}
	hit := "false"
	if step.Cache.Exact {
		hit = "true"
	}
	return map[string]string{"cache-hit": hit}
}

// cacheStepKind reports whether a step is one of the built-in cache actions.
func cacheStepKind(kind string) bool {
	return kind == "cache" || kind == "cache-restore" || kind == "cache-save"
}
