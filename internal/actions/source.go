package actions

// Sources: where a remote action's bytes come from.
//
// The default is THIS instance's own git (D-5: "resolve from this instance, and
// only this instance unless a source is explicitly configured"). An external
// source is an operator declaration (threads.actions.source) naming one
// Forgejo/Gitea-compatible base URL; there is no fallback, no discovery, and no
// implicit credential. Both sources return the same bounded file set and the
// resolver hashes it the same way.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TreeEntry is one file in a repository tree (mode 0o100644 or 0o100755).
type TreeEntry struct {
	Path string
	Mode uint32
	Size int64
}

// GitSource is the this-instance source: the server's git reader, restricted to
// read-only calls. internal/threads adapts its GitReader onto it.
type GitSource interface {
	ResolveRef(ctx context.Context, repo, ref string) (string, error)
	ReadFile(ctx context.Context, repo, commit, path string) ([]byte, error)
	ListTree(ctx context.Context, repo, commit, dir string) ([]TreeEntry, error)
}

// ErrNotFound marks a missing repository, ref or path (a named refusal, not an
// infrastructure error).
var ErrNotFound = errors.New("action source: not found")

// ErrTooLarge marks an action over the configured bound.
var ErrTooLarge = errors.New("action source: over the size bound")

// ErrRefused marks content the resolver refuses to extract (a symlink, a
// submodule, a path escape).
var ErrRefused = errors.New("action source: refused")

// LocalSource resolves actions from this instance's git.
type LocalSource struct {
	Git GitSource
	// Limits bound one action's content (threads.actions.max_bytes and the
	// file-count cap).
	Limits Limits
}

// Limits bound a fetched action.
type Limits struct {
	// MaxBytes bounds the summed content of one action (default 16 MiB).
	MaxBytes int64
	// MaxFileBytes bounds one file (default 4 MiB).
	MaxFileBytes int64
	// MaxFiles bounds the file count (default 2048).
	MaxFiles int
}

// DefaultLimits is the documented bound: an action is code, not a data set.
func DefaultLimits() Limits {
	return Limits{MaxBytes: 16 << 20, MaxFileBytes: 4 << 20, MaxFiles: 2048}
}

func (l Limits) normalize() Limits {
	d := DefaultLimits()
	if l.MaxBytes <= 0 {
		l.MaxBytes = d.MaxBytes
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = d.MaxFileBytes
	}
	if l.MaxFiles <= 0 {
		l.MaxFiles = d.MaxFiles
	}
	// A file cap above the action cap is meaningless and would report the
	// wrong reason; clamp it.
	if l.MaxFileBytes > l.MaxBytes {
		l.MaxFileBytes = l.MaxBytes
	}
	return l
}

// Resolve maps a tag/branch/SHA to a commit in this instance's repository.
func (s *LocalSource) Resolve(ctx context.Context, ref Ref) (string, error) {
	if s == nil || s.Git == nil {
		return "", fmt.Errorf("%w: this instance has no git reader wired, so it cannot resolve remote actions", ErrNotFound)
	}
	commit, err := s.Git.ResolveRef(ctx, ref.Repository(), ref.Ref)
	if err != nil {
		return "", fmt.Errorf("%w: %s@%s was not found on this instance (%v); remote actions resolve from this instance unless threads.actions.source is configured",
			ErrNotFound, ref.Repository(), ref.Ref, err)
	}
	return strings.ToLower(strings.TrimSpace(commit)), nil
}

// Fetch reads the action directory at commit and returns its bounded file set.
func (s *LocalSource) Fetch(ctx context.Context, ref Ref, commit string) ([]File, error) {
	if s == nil || s.Git == nil {
		return nil, fmt.Errorf("%w: this instance has no git reader wired", ErrNotFound)
	}
	limits := s.Limits.normalize()
	entries, err := s.Git.ListTree(ctx, ref.Repository(), commit, ref.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: could not list %s at %s: %v", ErrNotFound, displayDir(ref), shortSHA(commit), err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: %s has no files at %s", ErrNotFound, displayDir(ref), shortSHA(commit))
	}
	if len(entries) > limits.MaxFiles {
		return nil, fmt.Errorf("%w: %s has %d files at %s, over the limit of %d (an action is code, not a tree dump)",
			ErrTooLarge, displayDir(ref), len(entries), shortSHA(commit), limits.MaxFiles)
	}
	files := make([]File, 0, len(entries))
	var total int64
	for _, entry := range entries {
		rel := entry.Path
		if ref.Path != "" {
			rel = strings.TrimPrefix(strings.TrimPrefix(entry.Path, ref.Path), "/")
		}
		if rel == "" {
			continue
		}
		fileMode, ok := actionMode(entry.Mode)
		if !ok {
			switch entry.Mode & 0o170000 {
			case 0o120000:
				return nil, fmt.Errorf("%w: %s contains the symlink %q; a symlink in action content could point outside the action directory", ErrRefused, displayDir(ref), rel)
			case 0o160000:
				return nil, fmt.Errorf("%w: %s contains the submodule %q; submodules are not fetched into an action", ErrRefused, displayDir(ref), rel)
			}
			return nil, fmt.Errorf("%w: %s contains %q with unsupported mode %o", ErrRefused, displayDir(ref), rel, entry.Mode)
		}
		if entry.Size > limits.MaxFileBytes {
			return nil, fmt.Errorf("%w: %s/%s is %s, over the per-file limit of %s (threads.actions.max_bytes)",
				ErrTooLarge, displayDir(ref), rel, humanBytes(entry.Size), humanBytes(limits.MaxFileBytes))
		}
		data, rerr := s.Git.ReadFile(ctx, ref.Repository(), commit, entry.Path)
		if rerr != nil {
			return nil, fmt.Errorf("%w: could not read %s/%s at %s: %v", ErrNotFound, displayDir(ref), rel, shortSHA(commit), rerr)
		}
		if int64(len(data)) > limits.MaxFileBytes {
			return nil, fmt.Errorf("%w: %s/%s is larger than the per-file limit of %s", ErrTooLarge, displayDir(ref), rel, humanBytes(limits.MaxFileBytes))
		}
		total += int64(len(data))
		if total > limits.MaxBytes {
			return nil, fmt.Errorf("%w: %s is over the total limit of %s (threads.actions.max_bytes)", ErrTooLarge, displayDir(ref), humanBytes(limits.MaxBytes))
		}
		files = append(files, File{Path: rel, Mode: fileMode, Data: data})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: %s has no files at %s", ErrNotFound, displayDir(ref), shortSHA(commit))
	}
	return files, nil
}

// HTTPSource resolves actions from ONE explicitly configured Forgejo/Gitea-style
// base URL. It exists so an operator can mirror an approved action set without
// the instance reading a foreign host implicitly.
type HTTPSource struct {
	// Base is the configured source (https://host[:port][/prefix]).
	Base *url.URL
	// Client is the operator's HTTP client; nil builds one with a 30s timeout
	// that refuses every redirect (a redirect to another host would bypass the
	// source declaration).
	Client *http.Client
	Limits Limits
}

// NewHTTPSource validates a configured base URL.
func NewHTTPSource(base string, limits Limits) (*HTTPSource, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("threads.actions.source must be an absolute URL (https://git.example.com), got %q", base)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopbackHost(u.Hostname())) {
		return nil, fmt.Errorf("threads.actions.source must be https (http is accepted only for a loopback test source), got %q", base)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("threads.actions.source must not carry a user, query or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return &HTTPSource{Base: u, Limits: limits}, nil
}

// Host reports the source host, for the policy's identity spelling.
func (s *HTTPSource) Host() string {
	if s == nil || s.Base == nil {
		return ""
	}
	return strings.ToLower(s.Base.Host)
}

func (s *HTTPSource) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("the configured action source refused a redirect")
		},
	}
}

// Resolve asks the source's Forgejo-compatible ref API for a commit. A full SHA
// is its own answer (the fetch still verifies the bytes).
func (s *HTTPSource) Resolve(ctx context.Context, ref Ref) (string, error) {
	if ref.CommitSHA() {
		return ref.Ref, nil
	}
	for _, spelling := range []string{"tags/" + ref.Ref, "heads/" + ref.Ref, ref.Ref} {
		body, status, err := s.get(ctx, "/api/v1/repos/"+ref.Repository()+"/git/refs/"+spelling, 1<<20)
		if err != nil {
			return "", err
		}
		if status == http.StatusNotFound {
			continue
		}
		if status != http.StatusOK {
			return "", fmt.Errorf("action source %s: ref %s@%s: HTTP %d", s.Base, ref.Repository(), ref.Ref, status)
		}
		sha := refSHA(body)
		if !IsCommitSHA(strings.ToLower(sha)) {
			return "", fmt.Errorf("action source %s: ref %s@%s answered no commit SHA", s.Base, ref.Repository(), ref.Ref)
		}
		return strings.ToLower(sha), nil
	}
	return "", fmt.Errorf("%w: %s@%s was not found on the configured action source %s", ErrNotFound, ref.Repository(), ref.Ref, s.Base)
}

// refSHA extracts object.sha from either the object or the array shape.
func refSHA(body []byte) string {
	var one struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if json.Unmarshal(body, &one) == nil && one.Object.SHA != "" {
		return one.Object.SHA
	}
	var many []struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if json.Unmarshal(body, &many) == nil && len(many) > 0 {
		return many[0].Object.SHA
	}
	return ""
}

// Fetch downloads the commit's archive and keeps the action directory.
func (s *HTTPSource) Fetch(ctx context.Context, ref Ref, commit string) ([]File, error) {
	limits := s.Limits.normalize()
	u := *s.Base
	u.Path = strings.TrimRight(u.Path, "/") + "/" + ref.Repository() + "/archive/" + commit + ".tar.gz"
	resp, err := s.getRaw(ctx, u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: action source %s: %s@%s archive: HTTP %d", ErrNotFound, s.Base, ref.Repository(), shortSHA(commit), resp.StatusCode)
	}
	// The download bound is the total limit plus gzip/slack: the reader stops
	// at the first byte over it.
	gz, err := gzip.NewReader(io.LimitReader(resp.Body, limits.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("action source %s: %s@%s: not a gzip archive: %v", s.Base, ref.Repository(), shortSHA(commit), err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := make([]File, 0, 32)
	var total int64
	prefix := ""
	for {
		hdr, herr := tr.Next()
		if herr == io.EOF {
			break
		}
		if herr != nil {
			return nil, fmt.Errorf("action source %s: %s@%s: %v", s.Base, ref.Repository(), shortSHA(commit), herr)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		root, rest, _ := strings.Cut(name, "/")
		if root == "" {
			continue
		}
		if prefix == "" {
			prefix = root
		}
		if root != prefix {
			return nil, fmt.Errorf("%w: archive has more than one root directory", ErrRefused)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("%w: archive entry %q is not a regular file (a symlink or device in action content could escape the action directory)", ErrRefused, rest)
		}
		rel := rest
		if ref.Path != "" {
			if !strings.HasPrefix(rel, ref.Path+"/") {
				continue
			}
			rel = strings.TrimPrefix(rel, ref.Path+"/")
		}
		if rel == "" || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
			continue
		}
		if len(files) >= limits.MaxFiles {
			return nil, fmt.Errorf("%w: %s has more than %d files", ErrTooLarge, displayDir(ref), limits.MaxFiles)
		}
		if hdr.Size > limits.MaxFileBytes {
			return nil, fmt.Errorf("%w: %s/%s is %s, over the per-file limit of %s", ErrTooLarge, displayDir(ref), rel, humanBytes(hdr.Size), humanBytes(limits.MaxFileBytes))
		}
		data, rerr := io.ReadAll(io.LimitReader(tr, limits.MaxFileBytes+1))
		if rerr != nil {
			return nil, fmt.Errorf("action source %s: %s@%s: read %s: %v", s.Base, ref.Repository(), shortSHA(commit), rel, rerr)
		}
		if int64(len(data)) > limits.MaxFileBytes {
			return nil, fmt.Errorf("%w: %s/%s is larger than the per-file limit of %s", ErrTooLarge, displayDir(ref), rel, humanBytes(limits.MaxFileBytes))
		}
		total += int64(len(data))
		if total > limits.MaxBytes {
			return nil, fmt.Errorf("%w: %s is over the total limit of %s", ErrTooLarge, displayDir(ref), humanBytes(limits.MaxBytes))
		}
		mode := uint32(0o644)
		if hdr.Mode&0o111 != 0 {
			mode = 0o755
		}
		files = append(files, File{Path: rel, Mode: mode, Data: data})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: %s has no files at %s", ErrNotFound, displayDir(ref), shortSHA(commit))
	}
	return files, nil
}

// get performs one bounded GET and returns its body and status.
func (s *HTTPSource) get(ctx context.Context, path string, limit int64) ([]byte, int, error) {
	u := *s.Base
	u.Path = strings.TrimRight(u.Path, "/") + path
	resp, err := s.getRaw(ctx, u.String())
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, limit))
	if rerr != nil {
		return nil, resp.StatusCode, fmt.Errorf("action source %s: %v", s.Base, rerr)
	}
	return body, resp.StatusCode, nil
}

func (s *HTTPSource) getRaw(ctx context.Context, rawurl string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, fmt.Errorf("action source %s: %v", s.Base, err)
	}
	req.Header.Set("Accept", "application/json, application/gzip")
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("action source %s: %v", s.Base, err)
	}
	return resp, nil
}

// actionMode maps a tree mode onto the two modes an action file may carry
// (git's 0o100644/0o100755 and the plain 0o644/0o755 a fake or a test source
// may use).
func actionMode(m uint32) (uint32, bool) {
	switch m {
	case 0o100644, 0o644:
		return 0o644, true
	case 0o100755, 0o755:
		return 0o755, true
	}
	return 0, false
}

// loopbackHost reports a hostname the operator may reach over http (a test or
// local mirror; an http external source is refused).
func loopbackHost(host string) bool {
	h := strings.ToLower(host)
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasSuffix(h, ".localhost")
}

func displayDir(ref Ref) string {
	if ref.Path == "" {
		return ref.Repository()
	}
	return ref.Repository() + "/" + ref.Path
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
