package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
)

type fakeRoles struct {
	role string
	aerr *auth.AuthError
}

func (f *fakeRoles) CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError {
	if f.aerr != nil {
		return f.aerr
	}
	if f.role == "" && p.Anonymous {
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "login required"}
	}
	return nil
}

func (f *fakeRoles) Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc) {
	return identity.Role(f.role), nil
}

type fixedDirs struct{ dir string }

func (d fixedDirs) Dir(ctx context.Context, repo string) (string, error) { return d.dir, nil }

func newService(t *testing.T) (*Service, string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()
	work := filepath.Join(base, "work")
	bare := filepath.Join(base, "serving.git")
	env := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE=2026-01-02T03:04:05Z",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_COMMITTER_DATE=2026-01-02T03:04:05Z",
	}
	run := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(base, "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "docs", "a.txt"), []byte("nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", ".")
	run(work, "commit", "-q", "-m", "one")
	sha := run(work, "rev-parse", "HEAD")
	run(work, "tag", "-a", "v1.0.0", "-m", "release one")
	run(base, "clone", "-q", "--bare", work, bare)
	return &Service{Roles: &fakeRoles{role: string(identity.RoleRead)}, Dirs: fixedDirs{bare},
		Git: NewSubprocessGit("")}, sha, bare
}

func do(handler *Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	sub := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[2:]
	handler.HandleRepo(w, r, git.RepoId{Owner: "acme", Name: "widget"}, sub)
	return w
}

func TestArchiveTarMatchesGitArchive(t *testing.T) {
	svc, _, bare := newService(t)
	h := &Handler{Svc: svc}
	w := do(h, "GET", "/acme/widget/archive/v1.0.0.tar.gz", nil)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	cmd := exec.Command("git", "archive", "--format=tar", "--prefix=widget/", "v1.0.0^{tree}")
	cmd.Dir = bare
	cmd.Env = append(os.Environ(), "GIT_DIR="+bare, "GIT_TERMINAL_PROMPT=0")
	want, err := cmd.Output()
	if err != nil {
		t.Fatalf("local git archive: %v", err)
	}
	// The body is a REAL gzip stream (audit FAIL 1: the old lane labelled a
	// plain tar as application/gzip).
	body := w.Body.Bytes()
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Fatalf("body does not start with the gzip magic 1f 8b: % x", body[:min(4, len(body))])
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if err := zr.Close(); err != nil {
		t.Fatalf("gunzip close: %v", err)
	}
	// Compare the ARCHIVE, not the byte stream. A tar carries producer-specific
	// header fields (PAX vs GNU extensions, device numbers, padding), and two
	// equally valid writers encode them differently — CI caught this as
	// "got 10240 want 10240". Entry name, mode, mtime, type and content are
	// what the archive promises.
	assertSameTar(t, got, want)
	// A tar reader sees exactly the same entries as the local git archive
	// (the listing is what users open).
	gotNames, wantNames := tarNames(t, got), tarNames(t, want)
	if strings.Join(gotNames, "\n") != strings.Join(wantNames, "\n") {
		t.Fatalf("tar listing differs:\n got %v\nwant %v", gotNames, wantNames)
	}
	if got := w.Header().Get("Content-Type"); got != "application/gzip" {
		t.Fatalf("content-type %q", got)
	}
	if w.Header().Get("ETag") == "" {
		t.Fatal("missing ETag")
	}
}

// tarNames lists the entry names of a tar stream in order.
func tarNames(t *testing.T, b []byte) []string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(b))
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		names = append(names, hdr.Name)
	}
}

func TestArchiveZipMatchesGitArchiveWithStoreFlag(t *testing.T) {
	svc, _, bare := newService(t)
	h := &Handler{Svc: svc}
	w := do(h, "GET", "/acme/widget/archive/main.zip", nil)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	cmd := exec.Command("git", "archive", "--format=zip", "-0", "--prefix=widget/", "main^{tree}")
	cmd.Dir = bare
	cmd.Env = append(os.Environ(), "GIT_DIR="+bare, "GIT_TERMINAL_PROMPT=0")
	want, err := cmd.Output()
	if err != nil {
		t.Fatalf("local git archive: %v", err)
	}
	// Compare the ARCHIVE, not the container bytes. A zip stream carries
	// producer-specific fields (version-made-by, extra-field order, the UT
	// timestamp encoding) that differ between Go's writer and any given git
	// build, so byte equality is a toolchain assertion that flakes the moment
	// the two producers encode equally-valid zips differently (CI run 1091:
	// "got 515 want 515"). Entry names, modes, mtimes and contents are what
	// the archive actually promises.
	zipEntriesEqual(t, w.Body.Bytes(), want)
}

// zipEntriesEqual fails unless both zip streams carry the same entries: name,
// mode, modification time and decompressed content, in order.
func zipEntriesEqual(t *testing.T, got, want []byte) {
	t.Helper()
	gz, err := zip.NewReader(bytes.NewReader(got), int64(len(got)))
	if err != nil {
		t.Fatalf("our zip: %v", err)
	}
	wz, err := zip.NewReader(bytes.NewReader(want), int64(len(want)))
	if err != nil {
		t.Fatalf("git zip: %v", err)
	}
	if len(gz.File) != len(wz.File) {
		t.Fatalf("entry count: got %d want %d", len(gz.File), len(wz.File))
	}
	for i := range gz.File {
		g, w := gz.File[i], wz.File[i]
		if g.Name != w.Name {
			t.Fatalf("entry %d name: got %q want %q", i, g.Name, w.Name)
		}
		if g.Mode() != w.Mode() {
			t.Fatalf("entry %q mode: got %v want %v", g.Name, g.Mode(), w.Mode())
		}
		// Same rule as the tar lane: a directory entry's mtime is archiver
		// state; a file's is the commit time.
		if !strings.HasSuffix(g.Name, "/") && !g.Modified.Equal(w.Modified) {
			t.Fatalf("entry %q mtime: got %v want %v", g.Name, g.Modified, w.Modified)
		}
		gb, err := readZipEntry(g)
		if err != nil {
			t.Fatalf("entry %q: %v", g.Name, err)
		}
		wb, err := readZipEntry(w)
		if err != nil {
			t.Fatalf("entry %q: %v", w.Name, err)
		}
		if !bytes.Equal(gb, wb) {
			t.Fatalf("entry %q content differs (%d vs %d bytes)", g.Name, len(gb), len(wb))
		}
	}
}

func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func TestArchiveETag304AndRefEquivalence(t *testing.T) {
	svc, sha, _ := newService(t)
	h := &Handler{Svc: svc}
	w := do(h, "GET", "/acme/widget/archive/v1.0.0.tar.gz", nil)
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	w304 := do(h, "GET", "/acme/widget/archive/v1.0.0.tar.gz", map[string]string{"If-None-Match": etag})
	if w304.Code != http.StatusNotModified || w304.Body.Len() != 0 {
		t.Fatalf("304: %d body=%d", w304.Code, w304.Body.Len())
	}
	for _, ref := range []string{"main", sha} {
		got := do(h, "GET", "/acme/widget/archive/"+ref+".tar.gz", nil)
		if got.Code != 200 || got.Header().Get("ETag") != etag {
			t.Fatalf("ref %s: status %d etag %q want %q", ref, got.Code, got.Header().Get("ETag"), etag)
		}
	}
}

func TestArchiveHEADHasNoBody(t *testing.T) {
	svc, _, _ := newService(t)
	h := &Handler{Svc: svc}
	w := do(h, "HEAD", "/acme/widget/archive/v1.0.0.tar.gz", nil)
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD: %d body=%d", w.Code, w.Body.Len())
	}
}

func TestArchiveBadRefs(t *testing.T) {
	svc, _, _ := newService(t)
	h := &Handler{Svc: svc}
	for _, ref := range []string{"..", "-main", "bad%20ref", "a%5Eb"} {
		w := do(h, "GET", "/acme/widget/archive/"+ref+".tar.gz", nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("ref %q: status %d body=%s", ref, w.Code, w.Body.String())
		}
	}
	if w := do(h, "GET", "/acme/widget/archive/nope.tar.gz", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown: %d", w.Code)
	}
	if w := do(h, "POST", "/acme/widget/archive/v1.0.0.tar.gz", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method: %d", w.Code)
	}
}

func TestArchiveGate(t *testing.T) {
	svc, _, _ := newService(t)
	svc.Roles = &fakeRoles{}
	h := &Handler{Svc: svc}
	if w := do(h, "GET", "/acme/widget/archive/main.tar.gz", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", w.Code)
	}
	svc.Roles = &fakeRoles{aerr: &auth.AuthError{Kind: auth.ErrForbidden, Why: "no read"}}
	if w := do(h, "GET", "/acme/widget/archive/main.tar.gz", nil); w.Code != http.StatusForbidden {
		t.Fatalf("forbidden: %d", w.Code)
	}
}

func TestArchiveInvalidShapeNotClaimed(t *testing.T) {
	h := &Handler{Svc: &Service{}}
	r := httptest.NewRequest("GET", "/acme/widget/releases/x", nil)
	if h.HandleRepo(httptest.NewRecorder(), r, git.RepoId{Owner: "acme", Name: "widget"}, []string{"releases", "x"}) {
		t.Fatal("claimed a non-archive path")
	}
	r = httptest.NewRequest("GET", "/acme/widget/archive/plain", nil)
	if h.HandleRepo(httptest.NewRecorder(), r, git.RepoId{Owner: "acme", Name: "widget"}, []string{"archive", "plain"}) {
		t.Fatal("claimed an unsuffixed archive path")
	}
}

func TestArchiveGitFailures(t *testing.T) {
	svc, _, _ := newService(t)
	svc.Dirs = nil
	h := &Handler{Svc: svc}
	if w := do(h, "GET", "/acme/widget/archive/main.tar.gz", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired: %d", w.Code)
	}
	svc, _, _ = newService(t)
	svc.Git = &failingGit{}
	h = &Handler{Svc: svc}
	w := do(h, "GET", "/acme/widget/archive/main.tar.gz", nil)
	if w.Code != 200 {
		t.Fatalf("stream failure headers: %d", w.Code)
	}
}

type failingGit struct{}

func (f *failingGit) Tree(ctx context.Context, dir, ref string) (string, error) {
	return "0123456789abcdef0123456789abcdef01234567", nil
}
func (f *failingGit) Stream(ctx context.Context, dir string, argv []string, w io.Writer) error {
	return errors.New("boom")
}

func TestValidateRefAcceptsGitRefs(t *testing.T) {
	for _, ok := range []string{"main", "v1.0.0", "release/v1", "HEAD", strings.Repeat("a", 40)} {
		if err := validateRef(ok); err != nil {
			t.Fatalf("ref %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/x", "x/", "a..b", "a//b", "a.", "a b"} {
		if err := validateRef(bad); err == nil {
			t.Fatalf("ref %q accepted", bad)
		}
	}
}

func TestArchiveDispositionAndPrefix(t *testing.T) {
	if got := archiveDisposition("wid/get", "v1/2", "tar.gz"); got != `attachment; filename="widget-v1-2.tar.gz"` {
		t.Fatalf("disposition %q", got)
	}
	if got := ArchiveArgv("v1", "zip", "widget"); got[2] != "-0" {
		t.Fatalf("zip argv %v", got)
	}
	if got := ArchiveArgv("v1", "tar.gz", "widget"); got[2] != "--prefix=widget/" {
		t.Fatalf("tar argv %v", got)
	}
}

// assertSameTar compares two tar streams entry by entry: name, mode, mtime,
// type flag and decompressed content, in order.
func assertSameTar(t *testing.T, got, want []byte) {
	t.Helper()
	ge := readTarEntries(t, got)
	we := readTarEntries(t, want)
	if len(ge) != len(we) {
		t.Fatalf("tar entry count: got %d want %d", len(ge), len(we))
	}
	for i := range ge {
		g, w := ge[i], we[i]
		if g.name != w.name {
			t.Fatalf("entry %d name: got %q want %q", i, g.name, w.name)
		}
		if g.mode != w.mode {
			t.Fatalf("entry %q mode: got %o want %o", g.name, g.mode, w.mode)
		}
		// A DIRECTORY entry's mtime is the archiver's creation time, not data
		// from the commit: two valid `git archive` runs a second apart differ,
		// which flaked this test at 14:43:00 vs 14:43:01. Files carry the commit
		// time and must match exactly.
		if g.typeflag != tar.TypeDir && !g.mtime.Equal(w.mtime) {
			t.Fatalf("entry %q mtime: got %v want %v", g.name, g.mtime, w.mtime)
		}
		if g.typeflag != w.typeflag {
			t.Fatalf("entry %q type: got %c want %c", g.name, g.typeflag, w.typeflag)
		}
		if !bytes.Equal(g.body, w.body) {
			t.Fatalf("entry %q content differs (%d vs %d bytes)", g.name, len(g.body), len(w.body))
		}
	}
}

type tarEntry struct {
	name     string
	mode     int64
	mtime    time.Time
	typeflag byte
	body     []byte
}

func readTarEntries(t *testing.T, raw []byte) []tarEntry {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(raw))
	var out []tarEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar body: %v", err)
		}
		out = append(out, tarEntry{name: hdr.Name, mode: hdr.Mode, mtime: hdr.ModTime, typeflag: hdr.Typeflag, body: body})
	}
	return out
}
