// packages.go — the Packages page (docs/features/14 §7): one package per
// repository, listing tags with their digest, size, platforms (an index) and
// update time, the copyable `docker pull` command, and delete actions behind
// the typed confirmation.
//
// Gate parity with the JSON twin (`internal/packages` api.go): the same
// feature toggle (off ⇒ 404), the same read gate, and for deletes the SAME
// push rule — `packages.Service.CanPush`, which is identity.CheckPush, the
// git receive-pack rule. The page has no privilege of its own (§2.4).
package webui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/packages"
	"github.com/crueber/spindle/internal/server/auth"
)

// PackagesDeps is the registry seam (bound at mount time by composition; nil
// functions render the honest "not wired" card and refuse writes).
type PackagesDeps struct {
	// List reads the tag/digest rows (the JSON collection's own service).
	List func(ctx context.Context, owner, repo string) (*packages.RepoPackages, error)
	// Get reads one manifest by digest (page view; the API twin serves it).
	Get func(ctx context.Context, owner, repo, digest string) (*packages.ManifestView, error)
	// DeleteTag/DeleteManifest are the registry deletes (404 on unknown).
	DeleteTag      func(ctx context.Context, owner, repo, tag string) error
	DeleteManifest func(ctx context.Context, owner, repo, digest string) error
	// CanPush is packages.Service.CanPush (the git push rule); nil = no
	// viewer may delete (the same default as an unwired service).
	CanPush func(ctx context.Context, owner, repo string, p auth.Principal) bool
}

var packagesDeps PackagesDeps

// BindPackages installs the registry seam (composition, at mount time).
func BindPackages(d PackagesDeps) { packagesDeps = d }

func init() { registerGroup((*Server).mountPackages) }

// mountPackages owns the Packages page and its two delete POSTs. The route
// shapes are static-before-dynamic safe: `packages/tags/{tag}/delete` and
// `packages/{digest}/delete` differ in segment count.
func (s *Server) mountPackages() {
	m := &s.mux
	m.HandleFunc("GET /{owner}/{repo}/packages", s.repoPackages)
	m.HandleFunc("POST /{owner}/{repo}/packages/tags/{tag}/delete", s.repoPackageDeleteTag)
	m.HandleFunc("POST /{owner}/{repo}/packages/{digest}/delete", s.repoPackageDeleteManifest)
	// Unknown deeper GETs answer the Packages 404 page; unknown POSTs fall
	// through to the server's repo dispatch (the releases precedent).
	m.HandleFunc("GET /{owner}/{repo}/packages/{rest...}", s.collabNotFound)
}

// packageRow is one tag row of the page table.
type packageRow struct {
	Tag       string
	Digest    string
	Short     string
	MediaType string
	ShortType string
	Size      string
	Updated   string
	Platforms []string
	Pull      string
	// DeleteTag deletes the pointer; DeleteManifest deletes the version and
	// every tag aiming at it (both behind the typed confirmation).
	DeleteTag      string
	DeleteManifest string
}

// manifestRow is one distinct manifest (its tags folded in).
type manifestRow struct {
	Digest    string
	Short     string
	Size      string
	MediaType string
	Tags      []string
	Platforms []string
	Modified  string
	Delete    string
}

type packagesPage struct {
	Repo      *RepoChrome
	CanWrite  bool
	Rows      []packageRow
	Manifest  []manifestRow
	Pull      string // docker pull <host>/<o>/<r>:<first tag> (or bare)
	Count     int
	TotalSize string
	Usage     *packageUsageView
	Error     string
	Flash     string
}

// packageUsageView is the page's usage/quota reading (docs/features/14 §5).
// The state is words first — a hue-blind reader must get the same fact — and
// the meter is decoration (aria-valuetext carries the numbers).
type packageUsageView struct {
	Used      string
	Quota     string
	Remaining string
	Percent   int
	Limited   bool
	Over      bool
	State     string // "within quota" | "over quota" | "no quota"
	Tone      string // chip tone: open | closed | neutral
	Counts    string // "3 blobs · 2 manifests"
}

func (s *Server) repoPackages(w http.ResponseWriter, r *http.Request) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	c, ok := s.repoOpen(w, r, rq, api.SyncRefs)
	if !ok {
		return
	}
	if !c.FeaturePackages {
		// Off ⇒ 404, exactly like every /v2 route and the JSON twin.
		s.errorPage(w, r, http.StatusNotFound, "not found")
		return
	}
	data := &packagesPage{Repo: c, Flash: formsFlash(r)}
	if packagesDeps.CanPush != nil {
		data.CanWrite = packagesDeps.CanPush(r.Context(), rq.id.Owner, rq.id.Name, rq.p)
	}
	if packagesDeps.List == nil {
		data.Error = "the packages registry is not wired on this build"
		s.pageRepo(w, r, rq.p, c, http.StatusServiceUnavailable, "packages/packages", "Packages · "+c.Full, data)
		return
	}
	res, err := packagesDeps.List(r.Context(), rq.id.Owner, rq.id.Name)
	if err != nil {
		data.Error = err.Error()
		s.pageRepo(w, r, rq.p, c, packageErrStatus(err), "packages/packages", "Packages · "+c.Full, data)
		return
	}
	data.Count = len(res.Packages)
	data.TotalSize = humanBytes(res.TotalSize)
	data.Usage = packageUsageOf(res.Usage)
	base := s.packagesPullBase(r) + "/" + c.Full
	data.Pull = "docker pull " + base
	if len(res.Packages) > 0 {
		data.Pull += ":" + res.Packages[0].Tag
	}
	del := func(path string) string {
		if !data.CanWrite {
			return ""
		}
		return path
	}
	for _, row := range res.Packages {
		data.Rows = append(data.Rows, packageRow{
			Tag:       row.Tag,
			Digest:    row.Digest,
			Short:     shortDigest(row.Digest),
			MediaType: row.MediaType,
			ShortType: shortMediaType(row.MediaType),
			Size:      humanBytes(row.Size),
			Updated:   row.UpdatedAt,
			Platforms: row.Platforms,
			Pull:      "docker pull " + base + ":" + row.Tag,
			DeleteTag: del("/" + c.Full + "/packages/tags/" + url.PathEscape(row.Tag) + "/delete"),
		})
	}
	for _, m := range res.Manifests {
		data.Manifest = append(data.Manifest, manifestRow{
			Digest:    m.Digest,
			Short:     shortDigest(m.Digest),
			Size:      humanBytes(m.Size),
			MediaType: shortMediaType(m.MediaType),
			Tags:      m.Tags,
			Platforms: m.Platforms,
			Modified:  m.UpdatedAt,
			Delete:    del("/" + c.Full + "/packages/" + url.PathEscape(m.Digest) + "/delete"),
		})
	}
	s.pageRepo(w, r, rq.p, c, http.StatusOK, "packages/packages", "Packages · "+c.Full, data)
}

// repoPackageDeleteTag serves POST …/packages/tags/{tag}/delete.
func (s *Server) repoPackageDeleteTag(w http.ResponseWriter, r *http.Request) {
	s.repoPackageDelete(w, r, "Delete a package tag", func(ctx context.Context, owner, repo string) error {
		if packagesDeps.DeleteTag == nil {
			return errPackagesUnwired
		}
		return packagesDeps.DeleteTag(ctx, owner, repo, r.PathValue("tag"))
	}, "Tag "+r.PathValue("tag")+" deleted")
}

// repoPackageDeleteManifest serves POST …/packages/{digest}/delete: the
// version AND every tag aiming at it (GHCR's delete-a-version semantics).
func (s *Server) repoPackageDeleteManifest(w http.ResponseWriter, r *http.Request) {
	digest := r.PathValue("digest")
	s.repoPackageDelete(w, r, "Delete a package version", func(ctx context.Context, owner, repo string) error {
		if packagesDeps.DeleteManifest == nil {
			return errPackagesUnwired
		}
		return packagesDeps.DeleteManifest(ctx, owner, repo, digest)
	}, "Version "+shortDigest(digest)+" deleted")
}

var errPackagesUnwired = fmt.Errorf("the packages registry is not wired on this build")

// repoPackageDelete is the shared write path: read gate, feature toggle,
// same-origin + anonymous preflight, then the SAME push rule the API uses.
func (s *Server) repoPackageDelete(w http.ResponseWriter, r *http.Request, action string, do func(context.Context, string, string) error, flash string) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	c, ok := s.repoOpen(w, r, rq, api.SyncRefs)
	if !ok {
		return
	}
	if !c.FeaturePackages {
		s.errorPage(w, r, http.StatusNotFound, "not found")
		return
	}
	if !s.parityWritePreflight(w, r, rq.p, action) {
		return
	}
	if packagesDeps.CanPush == nil || !packagesDeps.CanPush(r.Context(), rq.id.Owner, rq.id.Name, rq.p) {
		s.errorPage(w, r, http.StatusForbidden, "write access required")
		return
	}
	if err := normalizeErr(do(r.Context(), rq.id.Owner, rq.id.Name)); err != nil {
		status := packageErrStatus(err)
		if status == http.StatusNotFound {
			s.errorPage(w, r, status, "package not found")
			return
		}
		s.errorPage(w, r, status, err.Error())
		return
	}
	s.formsRedirectFlash(w, r, "/"+c.Full+"/packages", flash)
}

// normalizeErr collapses a TYPED-NIL error to a real nil. The page seams speak
// `error` while the services speak concrete pointers (`*packages.Error`), so a
// success path that returns the pointer straight through yields a non-nil
// interface holding a nil pointer: `err != nil` is true, and the error branch
// both misreports a success and panics on err.Error(). Treat it as what it is.
func normalizeErr(err error) error {
	if err == nil {
		return nil
	}
	if v := reflect.ValueOf(err); v.Kind() == reflect.Ptr && v.IsNil() {
		return nil
	}
	return err
}

// packageErrStatus maps a *packages.Error onto the page status.
func packageErrStatus(err error) int {
	var pe *packages.Error
	// errors.As matches a TYPED-NIL *packages.Error too, so the pointer must be
	// checked before its fields: a success path that leaked a typed-nil once
	// panicked here instead of rendering the page.
	if errors.As(err, &pe) && pe != nil && pe.Status >= 400 && pe.Status <= 599 {
		return pe.Status
	}
	return http.StatusServiceUnavailable
}

// packagesPullBase is the registry host for a pull command: the public URL's
// host when configured, else the request host.
func (s *Server) packagesPullBase(r *http.Request) string {
	base := s.repoBaseURL(r)
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}
	return strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
}

// packageUsageOf projects the registry's usage reading onto the page: the
// numbers, the words that state the quota condition, and the meter width. A
// nil reading (the counter could not be read) renders nothing rather than a
// fabricated zero.
func packageUsageOf(u *packages.UsageInfo) *packageUsageView {
	if u == nil {
		return nil
	}
	v := &packageUsageView{
		Used:    humanBytes(u.Bytes),
		Limited: u.Limited(),
		Over:    u.OverQuota(),
		Percent: u.Percent(),
		Counts:  strconv.FormatInt(u.Blobs, 10) + " blob(s) · " + strconv.FormatInt(u.Manifests, 10) + " manifest(s)",
	}
	switch {
	case !v.Limited:
		v.State, v.Tone = "no quota", "neutral"
	case v.Over:
		v.State, v.Tone = "over quota", "closed"
	default:
		v.State, v.Tone = "within quota", "open"
	}
	if v.Limited {
		v.Quota = humanBytes(u.QuotaBytes)
		v.Remaining = humanBytes(u.Remaining())
	}
	return v
}

// shortDigest renders sha256:abcdef… → abcdef… (12).
func shortDigest(d string) string {
	hex := strings.TrimPrefix(d, "sha256:")
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return hex
}

// shortMediaType maps the three verbose media types onto a chip label.
func shortMediaType(mt string) string {
	switch {
	case strings.Contains(mt, "image.index"):
		return "OCI index"
	case strings.Contains(mt, "manifest.list"):
		return "manifest list"
	case strings.Contains(mt, "image.manifest"):
		return "OCI image"
	case strings.Contains(mt, "artifact.manifest"):
		return "artifact"
	case mt == "":
		return "manifest"
	}
	return mt
}

// humanBytes renders a byte count for a table cell.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
