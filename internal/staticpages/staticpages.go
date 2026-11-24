// Package staticpages serves a repository's committed bytes as a static site on
// a dedicated host (docs/features/19_static_sites.md).
//
// The listener answers one hostname only (port-insensitive, case-insensitive),
// never reads or sets cookies, and serves public repositories only: a repo that
// is not anonymous-readable is a plain 404. Bytes come from the synced serving
// copy in at most three git subprocesses on the happy path — resolve the ref,
// look the path up, read the blob — with the blob bounded by
// Options.MaxFileBytes.
//
// The package imports the standard library only; composition supplies every
// seam (RepoDirs, Settings, Visible) so it never reaches the store, the API or
// the page layer (13 §8 / law 8).
//
// ### Concurrency
//
// Hazard: a slow client plus a spawned `cat-file` pins a goroutine and one
// subprocess slot; a pool shared with git-over-HTTP would starve control-plane
// traffic (13 §7). Avoidance: the package owns its own bounded pool
// (4 × GOMAXPROCS, git.go), every command is bound to the request context with
// a per-command timeout, the handler holds no lock across a subprocess or a
// stream, and blob bodies are buffered lazily under MaxFileBytes — a request
// waiting for a pool slot holds no buffer, and an active read cannot outgrow the
// cap.
package staticpages

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
)

// Sentinel errors. The handler maps them onto plain statuses; nothing outside
// this package should need to inspect a git failure in detail.
var (
	// ErrNotFound reports a missing ref, path or repository.
	ErrNotFound = errors.New("not found")
	// ErrInvalid reports a malformed request path, ref or object id.
	ErrInvalid = errors.New("invalid request")
	// ErrUnavailable reports a store/git failure that is not the client's fault.
	ErrUnavailable = errors.New("temporarily unavailable")
	// ErrTooLarge reports content larger than Options.MaxFileBytes.
	ErrTooLarge = errors.New("file exceeds the static-site size cap")
)

const (
	// DefaultMaxFileBytes is the per-file cap when Options leaves it zero
	// (docs/features/19_static_sites.md §5: 8MiB).
	DefaultMaxFileBytes int64 = 8 << 20
	// CacheControl is the only cache class this listener emits: 200s carry it,
	// 404s and 301s carry no cache headers at all (§4).
	CacheControl = "public, max-age=600"
)

// DefaultIndexFiles returns the index names tried inside a directory, in order
// (§3/§5). It is a function rather than a package variable so no mutable global
// state exists in this package.
func DefaultIndexFiles() []string { return []string{"index.html", "index.htm"} }

// RepoDirs resolves a repository to its synced serving copy, with the sync
// read guard already released (composition owns Sync(LevelServe)).
type RepoDirs interface {
	Dir(ctx context.Context, repo string) (string, error)
}

// Entry is one `git ls-tree` record.
type Entry struct {
	Mode string // "100644", "100755", "120000", "040000", "160000"
	Type string // "blob", "tree", "commit"
	OID  string
	Size int64 // -1 for trees (`-` in ls-tree -l output)
}

// regularFile reports whether the entry is a plain file blob. Symlinks
// (120000) and gitlinks (160000) are never served (§3).
func (e Entry) regularFile() bool {
	return e.Type == "blob" && (e.Mode == "100644" || e.Mode == "100755")
}

// GitRunner is the subprocess seam (implemented by SubprocessGit in git.go).
// ref "" means HEAD. treeish is always a commit oid returned by Resolve.
type GitRunner interface {
	Resolve(ctx context.Context, dir, ref string) (commitOID string, err error)
	Lookup(ctx context.Context, dir, treeish, path string) (Entry, bool, error)
	ReadBlob(ctx context.Context, dir, oid string, w io.Writer) error
}

// SiteSettings is the per-repo [static_site] document.
type SiteSettings struct {
	Enabled bool
	Ref     string // "" = the repository's default branch tip
	Root    string // "" = repository root
}

// Settings reads the per-repo site document. Fail open: an error means "not
// published" (the handler answers 404, existence is not leaked).
type Settings interface {
	Site(ctx context.Context, owner, repo string) (SiteSettings, error)
}

// Visible is the anonymous read gate. false (or an error) means 404.
type Visible interface {
	CanRead(ctx context.Context, owner, repo string) (bool, error)
}

// Options configures a Handler. Host is the only Host the listener answers for;
// empty refuses every request. Logger receives one record per infrastructure
// failure (the 503s); a nil Logger means slog.Default(). There is no clock seam:
// v1 deliberately passes a zero modtime to http.ServeContent (no Last-Modified,
// §4).
type Options struct {
	Host         string
	MaxFileBytes int64
	IndexFiles   []string
	Dirs         RepoDirs
	Git          GitRunner
	Settings     Settings
	Visible      Visible
	Logger       *slog.Logger
}

// Handler answers every request on the static listener. All fields are fixed at
// New, so one Handler is safe for concurrent use.
type Handler struct {
	host         string // normalised (lowercase, port stripped); "" refuses all
	maxFileBytes int64
	indexFiles   []string
	dirs         RepoDirs
	git          GitRunner
	settings     Settings
	visible      Visible
	logger       *slog.Logger
}

// New builds a Handler with the documented defaults: 8MiB per-file cap,
// index.html/index.htm, the production subprocess runner. An empty Host is kept
// empty — that refuses every request rather than guessing a host.
func New(opts Options) *Handler {
	maxBytes := opts.MaxFileBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFileBytes
	}
	index := cleanIndexFiles(opts.IndexFiles)
	runner := opts.Git
	if runner == nil {
		runner = NewSubprocessGit("")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		host:         normalizeHost(opts.Host),
		maxFileBytes: maxBytes,
		indexFiles:   index,
		dirs:         opts.Dirs,
		git:          runner,
		settings:     opts.Settings,
		visible:      opts.Visible,
		logger:       logger,
	}
}

// normalizeHost lowercases a host and drops its port (RFC 6265/RFC 9110 Host
// comparison; docs §2). A bracketed IPv6 literal keeps its address without the
// brackets so "host" and "[host]:port" spellings converge. Nothing else is
// normalised: a trailing-dot FQDN ("static.localhost.") is a different string
// and is refused by design — the listener matches the configured host exactly
// rather than guessing a DNS canonical form.
func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if strings.HasPrefix(host, "[") {
		if end := strings.IndexByte(host, ']'); end > 0 {
			return strings.ToLower(host[1:end])
		}
		return strings.ToLower(host)
	}
	// A single colon separates host from port; several colons mean a bare IPv6
	// literal, which has no port to strip.
	if strings.Count(host, ":") == 1 {
		host = host[:strings.LastIndexByte(host, ':')]
	}
	return strings.ToLower(host)
}
