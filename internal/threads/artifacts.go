package threads

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Run artifacts (15 §2.5, docs/threads.md §3.4): the files a job publishes with
// `uses: actions/upload-artifact`, downloaded by a later job with
// `uses: actions/download-artifact`.
//
// ## Storage
//
//	repos/<o>/<r>/threads/runs/<run>/artifacts/<name>/manifest.json
//	repos/<o>/<r>/threads/runs/<run>/artifacts/<name>/blobs/<attempt:03d>-<index:06d>
//	repos/<o>/<r>/threads/artifact-expiry/<expires-unix>-<run>-<name>.json
//
// The manifest is the artifact: it is Create-only and is the commit point. A
// blob is written first (immutable, content-verified by the server while it
// receives it) and becomes visible only when the manifest lands, so an
// interrupted upload leaves blobs the run's retention deletes and never a
// half-listed artifact. Artifact NAMES are unique per run: a second upload of
// one name is ErrConflict, never a silent replace (a replace would let a later
// job rewrite what an earlier job published).
//
// The expiry index exists only for a per-artifact `retention-days` that
// SHORTENS the run's window: expiry is always min(run expiry, created + days),
// so a retention-days value can never outlive the run. Retain reads the index
// (one prefix LIST per repository per pass) and deletes expired artifacts
// early; everything else ages out with its run.
//
// ## Caps
//
// Per-file and per-run byte caps plus a file-count cap (15 §8). The server is
// authoritative (a blob PUT enforces the per-file cap, finalize the run total);
// the runner also learns the caps from its attempt's start answer so it stops
// before streaming gigabytes the server must refuse.
//
// ## Masking
//
// Secret masking applies to a step's LOG LINES, including the artifact name
// echoed there (the runner writes it through its masking streamer). Artifact
// CONTENTS are the user's data and are never rewritten: a job that writes a
// secret into a file publishes that secret to every repository reader.

// Artifact caps (15 §8). A configuration value of zero selects the default.
const (
	// DefaultMaxArtifactBytesPerRun bounds one run's published artifacts.
	DefaultMaxArtifactBytesPerRun int64 = 2 << 30 // 2 GiB
	// DefaultMaxArtifactBytesPerFile bounds one uploaded file.
	DefaultMaxArtifactBytesPerFile int64 = 256 << 20 // 256 MiB
	// MaxArtifactFiles bounds the files one artifact may hold.
	MaxArtifactFiles = 1000
	// MaxArtifactNameLen bounds an artifact name (it is a key segment).
	MaxArtifactNameLen = 128
	// MaxArtifactPathLen bounds one manifest path.
	MaxArtifactPathLen = 1024
	// MaxArtifactRetentionDays is GitHub's ceiling for `retention-days`; a
	// larger value is a named step error, not a silent clamp.
	MaxArtifactRetentionDays = 90
)

// validArtifactName pins an artifact name: one key segment, no slashes, no
// leading dot or dash (a dot could climb; a dash is option-shaped), no
// whitespace.
func validArtifactName(name string) bool {
	if name == "" || len(name) > MaxArtifactNameLen {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
		if i == 0 && (r == '.' || r == '-') {
			return false
		}
	}
	return true
}

// ArtifactFile is one file inside an artifact. Index names its blob object;
// SHA256 is what the server computed while receiving it (the server never
// trusts a client-supplied digest).
type ArtifactFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Index  int    `json:"index"`
}

// Artifact is the stored manifest, repos/<o>/<r>/threads/runs/<run>/artifacts/<name>/manifest.json.
type Artifact struct {
	Name    string         `json:"name"`
	Run     string         `json:"run"`
	Job     string         `json:"job"`
	JobRef  string         `json:"job_ref,omitempty"`
	Attempt int            `json:"attempt"`
	Files   []ArtifactFile `json:"files"`
	// TotalSize is the sum of Files' sizes (stored, so a list never reads a
	// blob).
	TotalSize int64 `json:"total_size"`
	// RetentionDays is the per-artifact window when one was declared; 0 means
	// "the run's window". It only ever SHORTENS that window.
	RetentionDays int    `json:"retention_days,omitempty"`
	CreatedAt     string `json:"created_at"`
	// Trusted mirrors the run's trust class at upload time (documentation for
	// readers; it changes no access rule — the run's own class is the gate).
	Trusted bool `json:"trusted"`
}

// ArtifactView is the read model: the manifest plus derived lifetime fields.
// ExpiresAt is min(run expiry, createdAt + retention_days) — never later than
// the run's own retention.
type ArtifactView struct {
	Artifact
	ExpiresAt string `json:"expires_at"`
}

// ArtifactBlobView is the blob-upload answer.
type ArtifactBlobView struct {
	Index  int    `json:"index"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ArtifactFinalizeInput is the upload's finalize body: the complete file list.
// A runner reports paths and sizes; the digests it sends are informational
// (the server's stored digests are authoritative and win).
type ArtifactFinalizeInput struct {
	Name          string         `json:"name"`
	Files         []ArtifactFile `json:"files"`
	RetentionDays int            `json:"retention_days,omitempty"`
}

// --- keys ------------------------------------------------------------------

// ArtifactPrefix returns repos/<o>/<r>/threads/runs/<run>/artifacts/.
func ArtifactPrefix(owner, repo, runID string) string {
	return RunsPrefix(owner, repo) + runID + "/artifacts/"
}

// ArtifactDir returns repos/<o>/<r>/threads/runs/<run>/artifacts/<name>/.
func ArtifactDir(owner, repo, runID, name string) string {
	return ArtifactPrefix(owner, repo, runID) + name + "/"
}

// ArtifactManifestKey returns the artifact's manifest key (the commit point).
func ArtifactManifestKey(owner, repo, runID, name string) string {
	return ArtifactDir(owner, repo, runID, name) + "manifest.json"
}

// ArtifactBlobsPrefix returns the blob prefix of one artifact ATTEMPT. The
// attempt is part of the key so a retried job can never overwrite the bytes a
// previous attempt published under the same index.
func ArtifactBlobsPrefix(owner, repo, runID, name string, attempt int) string {
	return ArtifactDir(owner, repo, runID, name) + "blobs/" + padInt(attempt, 3) + "-"
}

// ArtifactBlobKey returns one blob key.
func ArtifactBlobKey(owner, repo, runID, name string, attempt, index int) string {
	return ArtifactBlobsPrefix(owner, repo, runID, name, attempt) + padInt(index, 6)
}

// ArtifactExpiryPrefix returns repos/<o>/<r>/threads/artifact-expiry/.
func ArtifactExpiryPrefix(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/threads/artifact-expiry/"
}

// ArtifactExpiryKey returns one expiry-index key, ordered by expiry first.
func ArtifactExpiryKey(owner, repo, runID, name string, at time.Time) string {
	return ArtifactExpiryPrefix(owner, repo) + padInt64(at.Unix()) + "-" + runID + "-" + name + ".json"
}

// padInt64 renders a non-negative int64 zero-padded to 19 digits.
func padInt64(v int64) string {
	return fmt.Sprintf("%019d", v)
}

// artifactExpiryIndex is the tiny index record body.
type artifactExpiryIndex struct {
	Run     string `json:"run"`
	Name    string `json:"name"`
	Expires string `json:"expires_at"`
}

// --- caps ------------------------------------------------------------------

// artifactCaps returns the effective (per-run, per-file) caps.
func (s *Service) artifactCaps() (perRun, perFile int64) {
	perRun, perFile = s.MaxArtifactBytesPerRun, s.MaxArtifactBytesPerFile
	if perRun <= 0 {
		perRun = DefaultMaxArtifactBytesPerRun
	}
	if perFile <= 0 {
		perFile = DefaultMaxArtifactBytesPerFile
	}
	if perFile > perRun {
		perFile = perRun
	}
	return perRun, perFile
}

// AgentCaps is the runner-facing cap advertisement (the start answer carries
// it so a runner refuses over-cap input before streaming it).
type ArtifactCaps struct {
	MaxBytesPerRun  int64 `json:"max_bytes_per_run"`
	MaxBytesPerFile int64 `json:"max_bytes_per_file"`
	MaxFiles        int   `json:"max_files"`
}

// ArtifactCaps returns the caps a runner must honour for this service.
func (s *Service) ArtifactCaps() ArtifactCaps {
	perRun, perFile := s.artifactCaps()
	return ArtifactCaps{MaxBytesPerRun: perRun, MaxBytesPerFile: perFile, MaxFiles: MaxArtifactFiles}
}

// --- path validation -------------------------------------------------------

// cleanArtifactPath validates one manifest path: relative, slash-separated, no
// dot segments, no control characters, bounded length. It is the zip-slip and
// tar-slip boundary: a name that survives here can be extracted under the
// workspace without escaping it.
func cleanArtifactPath(p string) (string, error) {
	raw := strings.TrimSpace(p)
	if raw == "" {
		return "", fmt.Errorf("%w: artifact file path is empty", ErrInvalid)
	}
	if strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("%w: artifact path %q must be relative to the workspace", ErrInvalid, p)
	}
	if len(raw) > MaxArtifactPathLen {
		return "", fmt.Errorf("%w: artifact path is longer than %d bytes", ErrInvalid, MaxArtifactPathLen)
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "", fmt.Errorf("%w: artifact path %q contains a control character or backslash", ErrInvalid, p)
		}
	}
	clean := path.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: artifact path %q escapes the workspace", ErrInvalid, p)
	}
	return clean, nil
}

// --- upload (attempt-scoped) ----------------------------------------------

// AddArtifactBlob receives one file of an in-flight artifact upload. The
// attempt token is the authority; the server computes the digest while
// receiving (a client digest is never trusted) and enforces the per-file cap.
func (s *Service) AddArtifactBlob(ctx context.Context, claims AttemptClaims, name string, index int, filePath string, size int64, body io.Reader) (*ArtifactBlobView, error) {
	if _, err := s.verifyAttempt(ctx, claims, ScopeArtifact); err != nil {
		return nil, err
	}
	if !validArtifactName(name) {
		return nil, fmt.Errorf("%w: invalid artifact name %q (letters, digits, dot, dash, underscore; at most %d bytes)", ErrInvalid, name, MaxArtifactNameLen)
	}
	clean, perr := cleanArtifactPath(filePath)
	if perr != nil {
		return nil, perr
	}
	if index < 0 || index >= MaxArtifactFiles {
		return nil, fmt.Errorf("%w: artifact file index %d out of range (0..%d)", ErrInvalid, index, MaxArtifactFiles-1)
	}
	_, perFile := s.artifactCaps()
	if size < 0 {
		return nil, fmt.Errorf("%w: artifact file %q has no size", ErrInvalid, clean)
	}
	if size > perFile {
		return nil, fmt.Errorf("%w: artifact %q: file %q is %s, over the per-file cap of %s (threads.max_artifact_bytes_per_file)",
			ErrTooLarge, name, clean, humanBytes(size), humanBytes(perFile))
	}
	owner, repo, err := splitRepo(claims.Repo)
	if err != nil {
		return nil, err
	}
	// The artifact must fit the run's remaining budget before we spend the
	// write: existing artifacts' totals are read from their manifests (one
	// prefix LIST on an upload path, never a read path).
	used, uerr := s.artifactBytesInRun(ctx, owner, repo, claims.Run)
	if uerr != nil {
		return nil, uerr
	}
	perRun, _ := s.artifactCaps()
	if used+size > perRun {
		return nil, fmt.Errorf("%w: artifact %q would take run %s to %s, over the per-run cap of %s (threads.max_artifact_bytes_per_run)",
			ErrTooLarge, name, claims.Run, humanBytes(used+size), humanBytes(perRun))
	}
	key := ArtifactBlobKey(owner, repo, claims.Run, name, claims.Attempt, index)
	h := sha256.New()
	limited := &countingReader{r: io.LimitReader(body, size), h: h}
	if _, perr := s.Store.Put(ctx, key,
		store.PutBody{Stream: limited, StreamLen: size},
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream", Immutable: true}); perr != nil {
		if store.IsPreconditionFailed(perr) {
			// An idempotent retry of the same (attempt, index) write: the FIRST
			// write's bytes are the artifact's bytes, so report the stored
			// digest rather than trusting the retried body.
			stored, serr := s.Store.Get(ctx, key, store.GetOptions{})
			if serr != nil {
				return nil, serr
			}
			obj, ok := stored.(store.Object)
			if !ok || obj.Meta.Size != size {
				return nil, fmt.Errorf("%w: artifact %q: blob %d already exists with a different size", ErrConflict, name, index)
			}
			sum := sha256.New()
			if _, cerr := io.Copy(sum, obj.Body); cerr != nil {
				_ = obj.Body.Close()
				return nil, cerr
			}
			_ = obj.Body.Close()
			return &ArtifactBlobView{Index: index, Size: size, SHA256: hex.EncodeToString(sum.Sum(nil))}, nil
		}
		return nil, perr
	}
	if limited.n != size {
		_ = s.Store.Delete(ctx, key, "")
		return nil, fmt.Errorf("%w: artifact %q: file %q ended after %d of %d bytes", ErrInvalid, name, clean, limited.n, size)
	}
	return &ArtifactBlobView{Index: index, Size: size, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// countingReader hashes and counts what it passes through, stopping at want.
type countingReader struct {
	r io.Reader
	h io.Writer
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n += int64(n)
		_, _ = c.h.Write(p[:n])
	}
	return n, err
}

// FinalizeArtifact commits one artifact. The manifest is Create-only, so a
// second upload of the same name in the same run is ErrConflict. Before the
// commit, the blobs of this attempt are verified with one prefix LIST: a
// missing or short blob is a named failure, never a manifest pointing at
// nothing.
func (s *Service) FinalizeArtifact(ctx context.Context, claims AttemptClaims, in ArtifactFinalizeInput) (*ArtifactView, error) {
	job, err := s.verifyAttempt(ctx, claims, ScopeArtifact)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	if !validArtifactName(name) {
		return nil, fmt.Errorf("%w: invalid artifact name %q", ErrInvalid, in.Name)
	}
	if len(in.Files) == 0 {
		return nil, fmt.Errorf("%w: artifact %q has no files", ErrInvalid, name)
	}
	if len(in.Files) > MaxArtifactFiles {
		return nil, fmt.Errorf("%w: artifact %q has %d files, over the cap of %d (threads.max_artifact_files)",
			ErrInvalid, name, len(in.Files), MaxArtifactFiles)
	}
	perRun, perFile := s.artifactCaps()
	files := make([]ArtifactFile, 0, len(in.Files))
	seenPath := map[string]bool{}
	seenIndex := map[int]bool{}
	var total int64
	for _, f := range in.Files {
		clean, perr := cleanArtifactPath(f.Path)
		if perr != nil {
			return nil, perr
		}
		if seenPath[clean] {
			return nil, fmt.Errorf("%w: artifact %q lists %q twice", ErrInvalid, name, clean)
		}
		seenPath[clean] = true
		if f.Index < 0 || f.Index >= MaxArtifactFiles || seenIndex[f.Index] {
			return nil, fmt.Errorf("%w: artifact %q has an invalid or duplicate file index %d", ErrInvalid, name, f.Index)
		}
		seenIndex[f.Index] = true
		if f.Size < 0 || f.Size > perFile {
			return nil, fmt.Errorf("%w: artifact %q: file %q is over the per-file cap of %s (threads.max_artifact_bytes_per_file)",
				ErrTooLarge, name, clean, humanBytes(perFile))
		}
		total += f.Size
		files = append(files, ArtifactFile{Path: clean, Size: f.Size, Index: f.Index})
	}
	if total > perRun {
		return nil, fmt.Errorf("%w: artifact %q is %s, over the per-run cap of %s (threads.max_artifact_bytes_per_run)",
			ErrTooLarge, name, humanBytes(total), humanBytes(perRun))
	}
	owner, repo, err := splitRepo(claims.Repo)
	if err != nil {
		return nil, err
	}
	existing, ierr := s.loadArtifacts(ctx, owner, repo, claims.Run)
	if ierr != nil {
		return nil, ierr
	}
	var used int64
	for _, a := range existing {
		if a.Name == name {
			return nil, fmt.Errorf("%w: artifact %q already exists in run %s (artifact names are unique per run; a second upload fails)", ErrConflict, name, claims.Run)
		}
		used += a.TotalSize
	}
	if used+total > perRun {
		return nil, fmt.Errorf("%w: artifact %q would take run %s to %s, over the per-run cap of %s (threads.max_artifact_bytes_per_run)",
			ErrTooLarge, name, claims.Run, humanBytes(used+total), humanBytes(perRun))
	}
	// Verify the uploaded blobs against the manifest list with ONE prefix LIST.
	stored := map[int]int64{}
	if err := s.Store.List(ctx, ArtifactBlobsPrefix(owner, repo, claims.Run, name, claims.Attempt), "", func(m store.ObjectMeta) error {
		idx, ok := parseArtifactBlobIndex(m.Key)
		if !ok {
			return nil
		}
		stored[idx] = m.Size
		return nil
	}); err != nil {
		return nil, err
	}
	for _, f := range files {
		got, ok := stored[f.Index]
		if !ok {
			return nil, fmt.Errorf("%w: artifact %q: blob %d (%s) was never uploaded", ErrInvalid, name, f.Index, f.Path)
		}
		if got != f.Size {
			return nil, fmt.Errorf("%w: artifact %q: blob %d (%s) is %s, the manifest says %s",
				ErrInvalid, name, f.Index, f.Path, humanBytes(got), humanBytes(f.Size))
		}
	}
	days := in.RetentionDays
	if days < 0 || days > MaxArtifactRetentionDays {
		return nil, fmt.Errorf("%w: retention-days must be 0 (the run's window) or 1..%d", ErrInvalid, MaxArtifactRetentionDays)
	}
	now := s.nowUTC()
	art := &Artifact{
		Name: name, Run: claims.Run, Job: claims.Job, Attempt: claims.Attempt,
		Files: files, TotalSize: total, RetentionDays: days,
		CreatedAt: now.Format(dateTimeFmt), Trusted: job.Trusted,
	}
	raw, merr := json.Marshal(art)
	if merr != nil {
		return nil, merr
	}
	if perr := s.putCreate(ctx, ArtifactManifestKey(owner, repo, claims.Run, name), raw); perr != nil {
		if store.IsPreconditionFailed(perr) {
			return nil, fmt.Errorf("%w: artifact %q already exists in run %s (artifact names are unique per run; a second upload fails)", ErrConflict, name, claims.Run)
		}
		return nil, perr
	}
	// Every artifact gets an expiry-index row carrying its own lifetime
	// (PAR-158): the artifact window is the per-artifact `retention-days` when
	// one was declared, else threads.artifact_retention (defaulting to the run
	// window). A longer window than the run's means the artifact OUTLIVES the
	// run: deleting the run leaves the bytes, and this index row is how Retain
	// later reclaims them (its reclaimed bytes are reported under the artifact
	// family). A `keep_forever` instance writes no row and reclaims nothing.
	if !s.KeepForever {
		run, rerr := s.loadRun(ctx, owner, repo, claims.Run)
		if rerr == nil && run != nil {
			expiry := artifactExpiresAt(run, art, s.retention(), s.artifactRetention(), now)
			body, _ := json.Marshal(artifactExpiryIndex{Run: claims.Run, Name: name, Expires: expiry.Format(dateTimeFmt)})
			_ = s.putCreate(ctx, ArtifactExpiryKey(owner, repo, claims.Run, name, expiry), body)
		}
	}
	if run, rerr := s.loadRun(ctx, owner, repo, claims.Run); rerr == nil && run != nil {
		return s.artifactView(run, art, now), nil
	}
	return s.artifactView(&Run{ID: claims.Run, CreatedAt: art.CreatedAt}, art, now), nil
}

// parseArtifactBlobIndex extracts <index> from …/blobs/<attempt>-<index>.
func parseArtifactBlobIndex(key string) (int, bool) {
	i := strings.LastIndex(key, "/")
	if i < 0 {
		return 0, false
	}
	name := key[i+1:]
	dash := strings.IndexByte(name, '-')
	if dash <= 0 || len(name)-dash-1 != 6 {
		return 0, false
	}
	idx := 0
	for _, r := range name[dash+1:] {
		if r < '0' || r > '9' {
			return 0, false
		}
		idx = idx*10 + int(r-'0')
	}
	return idx, true
}

// artifactBytesInRun sums the committed artifacts' totals.
func (s *Service) artifactBytesInRun(ctx context.Context, owner, repo, runID string) (int64, error) {
	arts, err := s.loadArtifacts(ctx, owner, repo, runID)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, a := range arts {
		total += a.TotalSize
	}
	return total, nil
}

// --- reads -----------------------------------------------------------------

// loadArtifacts reads one run's manifest records (name-sorted). Only manifests
// are listed: blobs are read by the index their manifest names.
func (s *Service) loadArtifacts(ctx context.Context, owner, repo, runID string) ([]*Artifact, error) {
	prefix := ArtifactPrefix(owner, repo, runID)
	var keys []string
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		if strings.HasSuffix(m.Key, "/manifest.json") {
			keys = append(keys, m.Key)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(keys)
	out := make([]*Artifact, 0, len(keys))
	for _, key := range keys {
		raw, _, err := s.getJSON(ctx, key)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		var a Artifact
		if jerr := json.Unmarshal(raw, &a); jerr != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, key, jerr)
		}
		out = append(out, &a)
	}
	return out, nil
}

// loadArtifact reads one manifest; (nil, nil) when absent.
func (s *Service) loadArtifact(ctx context.Context, owner, repo, runID, name string) (*Artifact, error) {
	if !validArtifactName(name) {
		return nil, fmt.Errorf("%w: unknown artifact %q", ErrNotFound, name)
	}
	raw, _, err := s.getJSON(ctx, ArtifactManifestKey(owner, repo, runID, name))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	var a Artifact
	if jerr := json.Unmarshal(raw, &a); jerr != nil {
		return nil, fmt.Errorf("%w: artifact %s: %v", ErrCorrupt, name, jerr)
	}
	return &a, nil
}

// ListArtifacts lists a run's live artifacts (repository read gate).
func (s *Service) ListArtifacts(ctx context.Context, repo, runID string, p auth.Principal) ([]*ArtifactView, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, name, p); err != nil {
		return nil, err
	}
	if !runIDRe.MatchString(strings.TrimSpace(runID)) {
		return nil, fmt.Errorf("%w: unknown run %q", ErrNotFound, runID)
	}
	run, err := s.loadRun(ctx, owner, name, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("%w: unknown run %q", ErrNotFound, runID)
	}
	return s.runArtifactViews(ctx, owner, name, run)
}

// runArtifactViews loads one run's artifacts and drops the expired ones (a
// retention-days window that closed is a 404 to a reader even before GC frees
// the bytes — fail closed on the read too).
func (s *Service) runArtifactViews(ctx context.Context, owner, repo string, run *Run) ([]*ArtifactView, error) {
	arts, err := s.loadArtifacts(ctx, owner, repo, run.ID)
	if err != nil {
		return nil, err
	}
	now := s.nowUTC()
	views := make([]*ArtifactView, 0, len(arts))
	for _, a := range arts {
		if artifactExpired(a, now) {
			continue
		}
		views = append(views, s.artifactView(run, a, now))
	}
	return views, nil
}

// artifactView fills the derived expiry.
func (s *Service) artifactView(run *Run, a *Artifact, now time.Time) *ArtifactView {
	return &ArtifactView{Artifact: *a, ExpiresAt: artifactExpiresAt(run, a, s.retention(), s.artifactRetention(), now).Format(dateTimeFmt)}
}

// runExpiry is when the run's whole record ages out: CompletedAt (or
// CreatedAt while incomplete) plus the configured retention.
func runExpiry(run *Run, retention time.Duration) time.Time {
	stamp := run.CreatedAt
	if run.CompletedAt != nil && strings.TrimSpace(*run.CompletedAt) != "" {
		stamp = *run.CompletedAt
	}
	at, err := time.Parse(dateTimeFmt, strings.TrimSpace(stamp))
	if err != nil {
		at = time.Now().UTC()
	}
	return at.Add(retention)
}

// artifactRetention is the artifact window (threads.artifact_retention),
// defaulting to the run window. A longer value is the documented way an
// artifact outlives its run (PAR-158).
func (s *Service) artifactRetention() time.Duration {
	if s != nil && s.ArtifactRetention > 0 {
		return s.ArtifactRetention
	}
	return s.retention()
}

// artifactExpiresAt is the artifact's OWN lifetime: created plus its
// `retention-days` when one was declared, else created plus the artifact window
// (threads.artifact_retention). It is deliberately not bounded by the run's
// expiry: the two windows are separate, and `deleteRun` only reclaims an
// artifact whose own window has closed.
func artifactExpiresAt(run *Run, a *Artifact, retention, artifactRetention time.Duration, now time.Time) time.Time {
	created, err := time.Parse(dateTimeFmt, strings.TrimSpace(a.CreatedAt))
	if err != nil {
		created = now
	}
	_ = run // retained for the signature: the run no longer bounds the artifact
	if a.RetentionDays > 0 {
		return created.Add(time.Duration(a.RetentionDays) * 24 * time.Hour)
	}
	if artifactRetention <= 0 {
		artifactRetention = retention
	}
	if artifactRetention <= 0 {
		artifactRetention = DefaultRetention
	}
	return created.Add(artifactRetention)
}

// artifactExpired reports a per-artifact window that has closed. A zero
// retention-days never expires early: the run's own deletion is the expiry.
func artifactExpired(a *Artifact, now time.Time) bool {
	if a.RetentionDays <= 0 {
		return false
	}
	created, err := time.Parse(dateTimeFmt, strings.TrimSpace(a.CreatedAt))
	if err != nil {
		return false
	}
	return !created.Add(time.Duration(a.RetentionDays) * 24 * time.Hour).After(now)
}

// --- download ---------------------------------------------------------------

// ArtifactDownload is an opened artifact: the manifest plus the blob reader.
// Every Open is scoped to the commit that produced this value, so a caller
// cannot read a blob the artifact does not list.
type ArtifactDownload struct {
	Artifact *Artifact
	// TotalSize is the manifest's total.
	TotalSize int64
	store     store.ObjectStore
	owner     string
	repo      string
	runID     string
	name      string
	attempt   int
}

// Files returns the manifest's files in index order.
func (d *ArtifactDownload) Files() []ArtifactFile {
	out := append([]ArtifactFile{}, d.Artifact.Files...)
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// Open returns one blob's bytes and recorded size.
func (d *ArtifactDownload) Open(ctx context.Context, f ArtifactFile) (io.ReadCloser, int64, error) {
	key := ArtifactBlobKey(d.owner, d.repo, d.runID, d.name, d.attempt, f.Index)
	res, err := d.store.Get(ctx, key, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, 0, fmt.Errorf("%w: artifact %q: file %q is missing from the bucket", ErrCorrupt, d.name, f.Path)
		}
		return nil, 0, err
	}
	obj, ok := res.(store.Object)
	if !ok {
		return nil, 0, fmt.Errorf("%w: artifact %q: unexpected object result", ErrCorrupt, d.name)
	}
	return obj.Body, obj.Meta.Size, nil
}

// OpenArtifact opens one artifact for a repository reader. The reader gate is
// checked against the repository in the URL, and the manifest must live under
// that repository's run prefix — the path is the authority, so a download can
// never widen into another repository.
func (s *Service) OpenArtifact(ctx context.Context, repo, runID, name string, p auth.Principal) (*ArtifactDownload, error) {
	owner, rname, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, rname, p); err != nil {
		return nil, err
	}
	return s.openArtifact(ctx, owner, rname, runID, name)
}

// openArtifact opens one artifact without the reader gate (an attempt token
// already scoped to the run uses it).
func (s *Service) openArtifact(ctx context.Context, owner, repo, runID, name string) (*ArtifactDownload, error) {
	if !runIDRe.MatchString(strings.TrimSpace(runID)) {
		return nil, fmt.Errorf("%w: unknown run %q", ErrNotFound, runID)
	}
	run, err := s.loadRun(ctx, owner, repo, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("%w: unknown run %q", ErrNotFound, runID)
	}
	art, err := s.loadArtifact(ctx, owner, repo, runID, name)
	if err != nil {
		return nil, err
	}
	if art == nil {
		return nil, fmt.Errorf("%w: unknown artifact %q", ErrNotFound, name)
	}
	if artifactExpired(art, s.nowUTC()) {
		return nil, fmt.Errorf("%w: artifact %q expired (retention-days %d)", ErrNotFound, name, art.RetentionDays)
	}
	// A download cap: an artifact over the run total cannot be assembled.
	perRun, _ := s.artifactCaps()
	if art.TotalSize > perRun {
		return nil, fmt.Errorf("%w: artifact %q is %s, over the download cap of %s",
			ErrTooLarge, name, humanBytes(art.TotalSize), humanBytes(perRun))
	}
	return &ArtifactDownload{
		Artifact: art, TotalSize: art.TotalSize, store: s.Store,
		owner: owner, repo: repo, runID: runID, name: name, attempt: art.Attempt,
	}, nil
}

// ArtifactManifestForJob serves the runner's download step: the attempt token
// is scoped to (repo, run, job), and the artifact must belong to that run.
func (s *Service) ArtifactManifestForJob(ctx context.Context, claims AttemptClaims, name string) (*Artifact, error) {
	if _, err := s.verifyAttempt(ctx, claims, ScopeArtifact); err != nil {
		return nil, err
	}
	owner, repo, err := splitRepo(claims.Repo)
	if err != nil {
		return nil, err
	}
	art, err := s.loadArtifact(ctx, owner, repo, claims.Run, name)
	if err != nil {
		return nil, err
	}
	if art == nil {
		return nil, fmt.Errorf("%w: unknown artifact %q in run %s", ErrNotFound, name, claims.Run)
	}
	return art, nil
}

// ArtifactBlobForJob serves one blob of the CURRENT run to an attempt token.
// A blob index outside the manifest is refused, so the token cannot read an
// arbitrary bucket key.
func (s *Service) ArtifactBlobForJob(ctx context.Context, claims AttemptClaims, name string, index int) (io.ReadCloser, *ArtifactFile, error) {
	if _, err := s.verifyAttempt(ctx, claims, ScopeArtifact); err != nil {
		return nil, nil, err
	}
	owner, repo, err := splitRepo(claims.Repo)
	if err != nil {
		return nil, nil, err
	}
	dl, err := s.openArtifact(ctx, owner, repo, claims.Run, name)
	if err != nil {
		return nil, nil, err
	}
	for _, f := range dl.Artifact.Files {
		if f.Index == index {
			body, size, oerr := dl.Open(ctx, f)
			if oerr != nil {
				return nil, nil, oerr
			}
			if size != f.Size {
				_ = body.Close()
				return nil, nil, fmt.Errorf("%w: artifact %q: blob %d is %s, the manifest says %s",
					ErrCorrupt, name, index, humanBytes(size), humanBytes(f.Size))
			}
			file := f
			return body, &file, nil
		}
	}
	return nil, nil, fmt.Errorf("%w: artifact %q has no file %d", ErrNotFound, name, index)
}

// WriteArtifactZip streams the artifact as a zip (the browser download). Each
// entry's recorded size is revalidated against the bucket while copying: a
// truncated or replaced blob aborts the response with a named error instead of
// shipping bad bytes.
func WriteArtifactZip(ctx context.Context, w io.Writer, dl *ArtifactDownload) error {
	zw := zip.NewWriter(w)
	for _, f := range dl.Files() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		body, size, err := dl.Open(ctx, f)
		if err != nil {
			return err
		}
		if size != f.Size {
			_ = body.Close()
			return fmt.Errorf("%w: artifact %q: file %q is %s in the bucket, the manifest says %s",
				ErrCorrupt, dl.name, f.Path, humanBytes(size), humanBytes(f.Size))
		}
		entry, zerr := zw.CreateHeader(&zip.FileHeader{Name: f.Path, Method: zip.Deflate, Modified: time.Now()})
		if zerr != nil {
			_ = body.Close()
			return zerr
		}
		n, cerr := io.Copy(entry, body)
		_ = body.Close()
		if cerr != nil {
			return cerr
		}
		if n != f.Size {
			return fmt.Errorf("%w: artifact %q: file %q ended after %d of %d bytes",
				ErrCorrupt, dl.name, f.Path, n, f.Size)
		}
	}
	return zw.Close()
}

// --- expiry sweeps ---------------------------------------------------------

// expireArtifacts reclaims artifacts whose per-artifact window closed. It reads
// the repo's expiry index (ONE prefix LIST, maintainer only) and deletes the
// manifest, its blobs and the index key. An index entry whose run or artifact
// is already gone is dropped; a second pass deletes nothing.
// expireArtifacts runs one expiry pass in APPLY mode (the historical entry
// point used by tests); retain uses expireArtifactsMode so a dry run can report
// the same numbers without deleting.
func (s *Service) expireArtifacts(ctx context.Context, owner, repo string, now time.Time, res *GCResult) error {
	return s.expireArtifactsMode(ctx, owner, repo, now, true, nil, res)
}

// expireArtifactsMode reclaims the artifacts whose own window closed. One
// prefix LIST per pass over the expiry index (PAR-158/159): a due row deletes
// the manifest, its blobs and the row, reporting the artifact family's bytes.
// A dry run reports the same counts/bytes and deletes nothing.
func (s *Service) expireArtifactsMode(ctx context.Context, owner, repo string, now time.Time, apply bool, skipRuns map[string]bool, res *GCResult) error {
	type entry struct {
		key string
		idx artifactExpiryIndex
	}
	var due []entry
	if err := s.Store.List(ctx, ArtifactExpiryPrefix(owner, repo), "", func(m store.ObjectMeta) error {
		if !apply && len(due) >= retentionBatch {
			return nil
		}
		raw, _, err := s.getJSON(ctx, m.Key)
		if err != nil || raw == nil {
			return nil // an unreadable index row never fails the sweep
		}
		var idx artifactExpiryIndex
		if jerr := json.Unmarshal(raw, &idx); jerr != nil {
			return nil
		}
		// An artifact of a run this same pass already reclaimed (or kept for its
		// own window) was handled by the run walk; counting it here would report
		// it twice in a dry run, where the run's expiry rows still exist.
		if skipRuns[idx.Run] {
			return nil
		}
		at, perr := time.Parse(dateTimeFmt, strings.TrimSpace(idx.Expires))
		if perr != nil {
			return nil
		}
		if !at.After(now) {
			due = append(due, entry{key: m.Key, idx: idx})
		}
		return nil
	}); err != nil {
		return err
	}
	for _, e := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !validArtifactName(e.idx.Name) || !runIDRe.MatchString(e.idx.Run) {
			if apply {
				_ = s.Store.Delete(ctx, e.key, "")
			}
			continue
		}
		if !apply {
			// Size the artifact without deleting: manifest + blobs, exactly what
			// an apply pass would free.
			var freed int64
			_ = s.Store.List(ctx, ArtifactDir(owner, repo, e.idx.Run, e.idx.Name), "", func(m store.ObjectMeta) error {
				freed += m.Size
				return nil
			})
			res.ArtifactsDeleted++
			res.ArtifactBytes += freed
			res.BytesFreed += freed
			continue
		}
		n, derr := s.deleteArtifact(ctx, owner, repo, e.idx.Run, e.idx.Name)
		if derr != nil {
			return derr
		}
		res.ArtifactsDeleted++
		res.ArtifactBytes += n
		res.BytesFreed += n
		if derr := s.Store.Delete(ctx, e.key, ""); derr != nil && !store.IsNotFound(derr) {
			return derr
		}
	}
	return nil
}

// deleteArtifact removes one artifact: blobs first, then the manifest (the
// commit point), and any expiry-index rows the caller did not name.
func (s *Service) deleteArtifact(ctx context.Context, owner, repo, runID, name string) (int64, error) {
	prefix := ArtifactDir(owner, repo, runID, name)
	var keys []string
	var bytes int64
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		keys = append(keys, m.Key)
		bytes += m.Size
		return nil
	}); err != nil {
		return 0, err
	}
	manifest := ArtifactManifestKey(owner, repo, runID, name)
	sort.Strings(keys)
	// Manifests sort after "blobs/", so deleting in order already leaves the
	// manifest last; be explicit anyway.
	for _, key := range keys {
		if key == manifest {
			continue
		}
		if err := s.Store.Delete(ctx, key, ""); err != nil && !store.IsNotFound(err) {
			return bytes, err
		}
	}
	if err := s.Store.Delete(ctx, manifest, ""); err != nil && !store.IsNotFound(err) {
		return bytes, err
	}
	return bytes, nil
}

// humanBytes renders a size for an error message.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GiB", n>>30)
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
