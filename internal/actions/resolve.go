package actions

// The resolver: policy → commit → bytes → digest → parsed metadata.
//
// Every refusal is named and belongs to the job that uses the action. The
// resolver never executes anything and never talks to a token endpoint; a
// remote action gets exactly the job environment the runner hands it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Resolver resolves remote actions for one run. It caches each reference's
// parsed definition for the jobs of that run (a workflow usually shares the
// same action), so one fetch serves every use.
type Resolver struct {
	Policy Policy
	// Local resolves from this instance's git. Required unless Remote is set.
	Local *LocalSource
	// Remote is the explicitly configured source; nil means "this instance".
	Remote *HTTPSource
	// Guard remembers commit → content digest across runs. Nil disables the
	// cross-run check (unit tests), never the within-run one.
	Guard *DigestGuard
	// Limits bound fetched content.
	Limits Limits

	mu       sync.Mutex
	byUses   map[string]*Action
	commitOf map[string]string
}

// NewResolver builds a resolver. A nil policy selects D-5's default.
func NewResolver(policy Policy, local *LocalSource, remote *HTTPSource, guard *DigestGuard, limits Limits) *Resolver {
	return &Resolver{
		Policy: policy.Normalize(), Local: local, Remote: remote, Guard: guard,
		Limits: limits.normalize(), byUses: map[string]*Action{},
	}
}

// Resolve parses, authorises and fetches one `uses:` value. The returned action
// is digest-pinned and safe to embed.
func (r *Resolver) Resolve(ctx context.Context, uses string) (*Action, error) {
	ref, err := ParseRef(uses)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if cached, ok := r.byUses[uses]; ok {
		r.mu.Unlock()
		return cached, nil
	}
	r.mu.Unlock()

	// A full-URL reference is checked against the configured source first: a
	// source that does not exist is a structural refusal, not a policy one.
	var src source
	if ref.Host != "" {
		src, err = r.sourceFor(ref)
		if err != nil {
			return nil, err
		}
	}
	decision := r.Policy.Decide(ref)
	if !decision.Allowed {
		return nil, fmt.Errorf("%s", decision.Reason)
	}
	if src == nil {
		src, err = r.sourceFor(ref)
		if err != nil {
			return nil, err
		}
	}
	commit, err := src.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !IsCommitSHA(strings.ToLower(commit)) {
		return nil, fmt.Errorf("action %s@%s resolved to %q, which is not a full commit SHA", ref.Repository(), ref.Ref, commit)
	}
	commit = strings.ToLower(commit)
	files, err := src.Fetch(ctx, ref, commit)
	if err != nil {
		return nil, err
	}
	digest, err := Digest(files)
	if err != nil {
		return nil, err
	}
	if r.Guard != nil {
		// The key includes the action SUBPATH: two actions in one repository
		// share a commit and legitimately have different content digests.
		if gerr := r.Guard.Check(ref.Identity()+"#"+ref.Path, commit, digest); gerr != nil {
			return nil, gerr
		}
	}
	def, err := parseFetched(files)
	if err != nil {
		return nil, fmt.Errorf("action %s at %s: %w", displayDir(ref), shortSHA(commit), err)
	}
	def.Ref, def.Commit, def.Digest = ref, commit, digest
	def.DirName = dirName(commit, digest)
	if err := validateContent(def); err != nil {
		return nil, fmt.Errorf("action %s at %s: %w", displayDir(ref), shortSHA(commit), err)
	}
	r.mu.Lock()
	r.byUses[uses] = def
	r.mu.Unlock()
	return def, nil
}

// sourceFor selects the declared source for one reference. A full URL must
// name the configured source's host; a bare reference names that source when
// one is configured, otherwise this instance.
func (r *Resolver) sourceFor(ref Ref) (source, error) {
	if ref.Host != "" {
		if r.Remote == nil {
			return nil, fmt.Errorf("uses: %s is a full-URL action reference, but this instance has no threads.actions.source configured; use owner/repo@ref to resolve from this instance, or ask the operator to configure a source", ref.Raw)
		}
		if ref.Scheme != "https" && !(ref.Scheme == "http" && loopbackHost(ref.Host)) {
			return nil, fmt.Errorf("uses: %s must use https", ref.Raw)
		}
		if strings.ToLower(ref.Host) != r.Remote.Host() {
			return nil, fmt.Errorf("uses: %s names host %s, which is not the configured action source %s", ref.Raw, ref.Host, r.Remote.Base)
		}
		return r.Remote, nil
	}
	if r.Remote != nil {
		return r.Remote, nil
	}
	if r.Local == nil {
		return nil, fmt.Errorf("uses: %s cannot be resolved: this instance has no action source configured", ref.Raw)
	}
	return r.Local, nil
}

// source is the common shape both sources implement.
type source interface {
	Resolve(ctx context.Context, ref Ref) (string, error)
	Fetch(ctx context.Context, ref Ref, commit string) ([]File, error)
}

// parseFetched finds and parses the action metadata in a fetched file set.
func parseFetched(files []File) (*Action, error) {
	var meta []byte
	for _, name := range []string{"action.yml", "action.yaml"} {
		for _, f := range files {
			if f.Path == name {
				meta = f.Data
				break
			}
		}
		if meta != nil {
			break
		}
	}
	if meta == nil {
		return nil, fmt.Errorf("no action metadata at the action directory (looked for action.yml and action.yaml)")
	}
	def, err := ParseAction(meta)
	if err != nil {
		return nil, err
	}
	def.Files = files
	return def, nil
}

// validateContent closes the metadata's file references against the fetched
// set: a `main:` that names a file the fetch did not carry would fail at run
// time in the runner, so it is refused here, where the reason names the job.
func validateContent(a *Action) error {
	have := make(map[string]bool, len(a.Files))
	for _, f := range a.Files {
		have[f.Path] = true
	}
	switch a.Using {
	case UsingDocker:
		if a.IsDockerfile() {
			if !have[a.Image] {
				return fmt.Errorf("runs.image names %q, which is not in the action directory", a.Image)
			}
		}
	case UsingNode20, UsingNode24:
		for field, p := range map[string]string{"main": a.Main, "pre": a.Pre, "post": a.Post} {
			if p == "" {
				continue
			}
			if !have[p] {
				return fmt.Errorf("runs.%s names %q, which is not in the action directory", field, p)
			}
		}
	}
	return nil
}

// Digest is the canonical content hash of one action: sha256 over the sorted
// (mode, path, bytes) tuple, so two fetches of the same commit agree and a
// changed byte changes the digest. It is the value recorded on the run.
func Digest(files []File) (string, error) {
	if len(files) == 0 {
		return "", fmt.Errorf("action has no files to hash")
	}
	sorted := append([]File{}, files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	h.Write([]byte("spindle-action-v1\x00"))
	for _, f := range sorted {
		if strings.ContainsAny(f.Path, "\x00\n") {
			return "", fmt.Errorf("action file path %q is not representable in the digest", f.Path)
		}
		fmt.Fprintf(h, "%o %s %d\n", f.Mode, f.Path, len(f.Data))
		h.Write(f.Data)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// dirName is the stable materialization directory for one action.
func dirName(commit, digest string) string {
	sum := sha256.Sum256([]byte(commit + "\x00" + digest))
	return hex.EncodeToString(sum[:])[:24]
}

// DigestGuard remembers the content digest seen for a (source, commit) pair,
// across runs, so a source that serves different bytes for the same commit is
// a named refusal instead of a cache hit. It is bounded (FIFO eviction); an
// evicted entry is re-learned on the next fetch.
type DigestGuard struct {
	mu    sync.Mutex
	max   int
	order []string
	seen  map[string]string
}

// NewDigestGuard builds a guard holding at most max entries (default 4096).
func NewDigestGuard(max int) *DigestGuard {
	if max <= 0 {
		max = 4096
	}
	return &DigestGuard{max: max, seen: map[string]string{}}
}

// Check records digest for key and refuses a changed value.
func (g *DigestGuard) Check(source, commit, digest string) error {
	key := strings.ToLower(source) + "@" + commit
	g.mu.Lock()
	defer g.mu.Unlock()
	if prev, ok := g.seen[key]; ok {
		if prev == digest {
			return nil
		}
		return fmt.Errorf("action supply chain: %s changed content under the same commit (recorded %s, fetched %s); refusing to run it — this is a source compromise or corruption, not a cache miss", key, prev, digest)
	}
	if len(g.order) >= g.max {
		oldest := g.order[0]
		g.order = g.order[1:]
		delete(g.seen, oldest)
	}
	g.seen[key] = digest
	g.order = append(g.order, key)
	return nil
}
