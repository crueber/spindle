// Package archive serves the repository source-archive route
// (docs/PLAN-V1.md WP3.1):
//
//	GET|HEAD /{owner}/{repo}/archive/{ref}.tar.gz
//	GET|HEAD /{owner}/{repo}/archive/{ref}.zip
//
// The bytes are produced by the `git archive` subprocess (exact argv in
// docs/go/04_git.md §11.3; law 2 — never a Go archive writer), streamed
// straight to the response, and stamped with an ETag derived from the
// resolved TREE id (so two refs naming the same tree revalidate against each
// other for free). Refs may be a branch, a tag, or a full commit sha; a ref
// containing `/` arrives as several URL path segments and is re-joined
// before resolution.
//
// The route lives OUTSIDE the api lanes in the `/{o}/{r}/<sub>` family and is
// registered through server.RepoRoutes (14_extensibility.md §14.3): bytes are
// never compressed (no compress group wraps repoDispatch), and the handler
// authenticates itself with the same read gate the pages use.
//
// Bucket layout: none. Archives are derived on read; nothing is cached in the
// store (law 4 — the repo is the bucket, a derived byte stream is not state).
//
// ### Concurrency
//
// Hazard: an archive spawn holds a git pool slot for as long as the client
// streams (a slow or hostile reader could pin capacity). Avoidance: one
// dedicated bounded pool per Service (never the control-plane pool), the
// subprocess runs under the request context so a client disconnect SIGKILLs
// it, and the slot is released when Wait returns. No lock is held across the
// stream; the handler keeps no state.
package archive

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
)

// Sentinel errors (mapped to plain-text statuses in the handler).
var (
	ErrNotFound     = errors.New("not found")
	ErrInvalid      = errors.New("invalid ref")
	ErrUnauthorized = errors.New("authentication required")
	ErrForbidden    = errors.New("forbidden")
	ErrUnavailable  = errors.New("temporarily unavailable")
)

// RoleService is the narrow P6 read gate (satisfied by *identity.Service).
type RoleService interface {
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// RepoDirs resolves a repo to its synced local git dir.
type RepoDirs interface {
	Dir(ctx context.Context, repo string) (string, error)
}

// GitRunner shells out to git (tests substitute a real-repo runner or a
// fake). Tree resolves a ref to its tree id; Stream writes the archive.
type GitRunner interface {
	Tree(ctx context.Context, dir, ref string) (string, error)
	Stream(ctx context.Context, dir string, argv []string, w io.Writer) error
}

// Service serves the archive route. Archives are derived on read, so there is
// no store field and no bucket key (law 4).
type Service struct {
	Roles RoleService
	Dirs  RepoDirs
	Git   GitRunner
}

// New builds a Service.
func New(roles RoleService) *Service { return &Service{Roles: roles} }

// Handler is the server.RepoRoutes surface.
type Handler struct {
	Svc  *Service
	Auth func(r *http.Request) (auth.Principal, *auth.AuthError)
}

// principal resolves the request principal (nil Auth = anonymous).
func (h *Handler) principal(r *http.Request) (auth.Principal, *auth.AuthError) {
	if h.Auth != nil {
		return h.Auth(r)
	}
	return auth.Anonymous(), nil
}

// HandleRepo answers GET|HEAD /{o}/{r}/archive/{ref}.{tar.gz,zip}; false when
// sub is not an archive path (the server falls through to its 404).
func (h *Handler) HandleRepo(w http.ResponseWriter, r *http.Request, id git.RepoId, sub []string) bool {
	if len(sub) < 2 || sub[0] != "archive" {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writePlain(w, http.StatusMethodNotAllowed, "method not allowed")
		return true
	}
	ref := strings.Join(sub[1:], "/")
	var format string
	switch {
	case strings.HasSuffix(ref, ".tar.gz"):
		format, ref = "tar.gz", strings.TrimSuffix(ref, ".tar.gz")
	case strings.HasSuffix(ref, ".zip"):
		format, ref = "zip", strings.TrimSuffix(ref, ".zip")
	default:
		return false // not an archive shape: let the next surface answer
	}
	p, aerr := h.principal(r)
	if aerr != nil {
		writeErr(w, aerr)
		return true
	}
	if h.Svc == nil {
		writePlain(w, http.StatusServiceUnavailable, "archive service not wired")
		return true
	}
	h.Svc.serve(w, r, id.Owner, id.Name, p, ref, format)
	return true
}

// ServeHTTP answers 404: the archive route is a repo-subpath family
// (HandleRepo), never a lane surface.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	writePlain(w, http.StatusNotFound, "not found")
}

// validateRef rejects refs git could interpret as an option or that contain
// bytes outside the refname grammar. Full 40/64-hex object ids are accepted
// (a commit archive).
func validateRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("%w: ref must not be empty", ErrInvalid)
	}
	if len(ref) > 500 {
		return fmt.Errorf("%w: ref too long", ErrInvalid)
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("%w: ref must not start with '-'", ErrInvalid)
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if c < 0x20 || c == 0x7f || c == ' ' || c == '~' || c == '^' || c == ':' ||
			c == '?' || c == '*' || c == '[' || c == '\\' {
			return fmt.Errorf("%w: bad character in ref", ErrInvalid)
		}
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, "@{") ||
		strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") ||
		strings.HasSuffix(ref, ".") || strings.Contains(ref, "//") {
		return fmt.Errorf("%w: malformed ref", ErrInvalid)
	}
	return nil
}

// serve resolves the ref, stamps the tree-id ETag (304 on If-None-Match), and
// streams the archive subprocess output.
func (s *Service) serve(w http.ResponseWriter, r *http.Request, owner, repo string, p auth.Principal, ref, format string) {
	if err := s.requireRead(r.Context(), owner, repo, p); err != nil {
		writeErr(w, err)
		return
	}
	if err := validateRef(ref); err != nil {
		writeErr(w, err)
		return
	}
	if s.Git == nil || s.Dirs == nil {
		writePlain(w, http.StatusServiceUnavailable, "archive git runner not wired")
		return
	}
	dir, err := s.Dirs.Dir(r.Context(), owner+"/"+repo)
	if err != nil {
		writePlain(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	tree, err := s.Git.Tree(r.Context(), dir, ref)
	if err != nil {
		writeErr(w, err)
		return
	}
	etag := `"` + tree + `-` + format + `"`
	hdr := w.Header()
	hdr.Set("ETag", etag)
	hdr.Set("Cache-Control", "public, max-age=300")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Content-Disposition", archiveDisposition(repo, ref, format))
	if format == "zip" {
		hdr.Set("Content-Type", "application/zip")
	} else {
		hdr.Set("Content-Type", "application/gzip")
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && matchETag(inm, strings.Trim(etag, `"`)) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	// The .tar.gz lane is `git archive --format=tar` piped through a
	// streaming gzip.Writer (docs/go/04_git.md §11.3): git produces the
	// reproducible tar stream, Go compresses it as it arrives — no whole
	// archive is buffered, and the tar bytes after decompression are
	// byte-identical to a local `git archive` run. The .zip lane gets the
	// archive bytes straight from git.
	var dst io.Writer = w
	var gz *gzip.Writer
	if format == "tar.gz" {
		gz = gzip.NewWriter(w)
		dst = gz
	}
	streamErr := s.Git.Stream(r.Context(), dir, ArchiveArgv(ref, format, repo), dst)
	if gz != nil {
		if cerr := gz.Close(); streamErr == nil {
			streamErr = cerr
		}
	}
	if streamErr != nil {
		// Headers are gone; the only honest move is to stop writing.
		return
	}
}

// requireRead is the page/API read gate (anonymous-denied is a real 401,
// authenticated-but-insufficient a 403).
func (s *Service) requireRead(ctx context.Context, owner, repo string, p auth.Principal) error {
	if p.Admin || p.Write {
		return nil
	}
	if s.Roles == nil {
		if p.Anonymous {
			return ErrUnauthorized
		}
		return nil
	}
	if aerr := s.Roles.CheckRead(ctx, owner, repo, p); aerr != nil {
		switch aerr.Kind {
		case auth.ErrForbidden:
			return fmt.Errorf("%w: %s", ErrForbidden, aerr.Why)
		case auth.ErrUnavailable:
			return fmt.Errorf("%w: %s", ErrUnavailable, aerr.Why)
		default:
			return fmt.Errorf("%w: %s", ErrUnauthorized, aerr.Why)
		}
	}
	return nil
}

// ArchiveArgv is the exact `git archive` argv (docs/go/04_git.md §11.3).
// The tar lane produces a plain tar that serve() compresses on the fly (the
// gzip stage is Go's, not git's); the zip lane passes -0 (store, no deflate)
// so the bytes are reproducible for a given commit and cost no compression
// CPU.
func ArchiveArgv(ref, format, repo string) []string {
	prefix := sanitizePrefix(repo) + "/"
	if format == "zip" {
		return []string{"archive", "--format=zip", "-0", "--prefix=" + prefix, ref + "^{tree}"}
	}
	return []string{"archive", "--format=tar", "--prefix=" + prefix, ref + "^{tree}"}
}

// sanitizePrefix keeps a repo name inert in the archive entry prefix.
func sanitizePrefix(repo string) string {
	var b strings.Builder
	for _, c := range repo {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-' {
			b.WriteRune(c)
		}
	}
	if b.Len() == 0 {
		return "repo"
	}
	return b.String()
}

// archiveDisposition builds an RFC 6266 attachment disposition.
func archiveDisposition(repo, ref, format string) string {
	name := sanitizePrefix(repo) + "-" + sanitizeName(ref) + "." + format
	return `attachment; filename="` + name + `"`
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-' {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "source"
	}
	return b.String()
}

// matchETag matches an If-None-Match header against one entity tag.
func matchETag(header, etag string) bool {
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, part := range strings.Split(header, ",") {
		p := strings.TrimSpace(part)
		p = strings.TrimPrefix(p, "W/")
		p = strings.Trim(p, `"`)
		if p == etag {
			return true
		}
	}
	return false
}

func writePlain(w http.ResponseWriter, status int, msg string) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Del("ETag")
	if status == http.StatusUnauthorized {
		hdr.Set("WWW-Authenticate", `Bearer realm="walgit"`)
	}
	if status == http.StatusServiceUnavailable {
		hdr.Set("Retry-After", "15")
	}
	hdr.Set("Content-Length", strconv.Itoa(len(msg)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}

func writeErr(w http.ResponseWriter, err error) {
	var aerr *auth.AuthError
	if errors.As(err, &aerr) {
		switch aerr.Kind {
		case auth.ErrForbidden:
			writePlain(w, http.StatusForbidden, aerr.Why)
		case auth.ErrUnavailable:
			writePlain(w, http.StatusServiceUnavailable, aerr.Why)
		default:
			writePlain(w, http.StatusUnauthorized, aerr.Why)
		}
		return
	}
	writePlain(w, statusFor(err), err.Error())
}

// statusFor maps a service error onto its HTTP status.
func statusFor(err error) int {
	switch {
	case err == nil:
		return 200
	case errors.Is(err, ErrNotFound):
		return 404
	case errors.Is(err, ErrInvalid):
		return 400
	case errors.Is(err, ErrUnauthorized):
		return 401
	case errors.Is(err, ErrForbidden):
		return 403
	case errors.Is(err, ErrUnavailable):
		return 503
	default:
		return 500
	}
}

// --- production git runner ---------------------------------------------------

// gitPool is the bounded semaphore of concurrent archive processes.
type gitPool struct{ sem chan struct{} }

func newGitPool(capacity int) *gitPool {
	if capacity <= 0 {
		capacity = 4 * runtime.GOMAXPROCS(0)
	}
	return &gitPool{sem: make(chan struct{}, capacity)}
}

// run executes fn while holding a pool slot; ctx cancellation while waiting
// returns ctx.Err() without running fn.
func (p *gitPool) run(ctx context.Context, fn func() error) error {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
		return fn()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SubprocessGit is the production GitRunner.
type SubprocessGit struct {
	Binary  string
	Pool    *gitPool
	Timeout time.Duration
}

// NewSubprocessGit builds the production runner.
func NewSubprocessGit(binary string) *SubprocessGit {
	if binary == "" {
		binary = "git"
	}
	return &SubprocessGit{Binary: binary, Pool: newGitPool(0), Timeout: 120 * time.Second}
}

func (g *SubprocessGit) timeout() time.Duration {
	if g.Timeout <= 0 {
		return 120 * time.Second
	}
	return g.Timeout
}

func (g *SubprocessGit) bin() string {
	if g.Binary == "" {
		return "git"
	}
	return g.Binary
}

func (g *SubprocessGit) pool() *gitPool {
	if g.Pool == nil {
		g.Pool = newGitPool(0)
	}
	return g.Pool
}

// Tree resolves <ref>^{tree} (peeled), mapping a git-refusal to ErrNotFound.
func (g *SubprocessGit) Tree(ctx context.Context, dir, ref string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()
	var out, errText string
	perr := g.pool().run(cctx, func() error {
		cmd := exec.CommandContext(cctx, g.bin(),
			"rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{tree}")
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0", "GIT_DIR=" + dir}
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		runErr := cmd.Run()
		out, errText = strings.TrimSpace(stdout.String()), stderr.String()
		return runErr
	})
	if perr != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(perr, &exitErr) {
			return "", fmt.Errorf("%w: unknown revision %q", ErrNotFound, ref)
		}
		return "", fmt.Errorf("%w: %s", ErrUnavailable, errText)
	}
	if out == "" {
		return "", fmt.Errorf("%w: unknown revision %q", ErrNotFound, ref)
	}
	return out, nil
}

// Stream runs argv with stdout piped to w, holding a pool slot until the
// process exits. A copy failure kills the child (client gone).
func (g *SubprocessGit) Stream(ctx context.Context, dir string, argv []string, w io.Writer) error {
	cctx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()
	return g.pool().run(cctx, func() error {
		cmd := exec.CommandContext(cctx, g.bin(), argv...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0", "GIT_DIR=" + dir}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		_, copyErr := io.Copy(w, stdout)
		if copyErr != nil {
			_ = cmd.Process.Kill()
		}
		waitErr := cmd.Wait()
		if copyErr != nil {
			return copyErr
		}
		if waitErr != nil {
			return fmt.Errorf("%w: git archive: %v (%s)", ErrUnavailable, waitErr, stderr.String())
		}
		return nil
	})
}

// compile-time seam assertion: composition wires *identity.Service.
var _ RoleService = (*identity.Service)(nil)
