package webui

// packages_test.go — the Packages page (docs/features/14 §7): the tag table,
// the copyable pull command, the delete affordances behind CanPush (the git
// push rule), the Packages tab, and the feature toggle's fail-closed 404.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/packages"
	"github.com/crueber/spindle/internal/server/auth"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func bindPackagesDeps(t *testing.T, canPush bool, deleted *[]string) {
	t.Helper()
	old := packagesDeps
	t.Cleanup(func() { packagesDeps = old })
	BindPackages(PackagesDeps{
		List: func(context.Context, string, string) (*packages.RepoPackages, error) {
			return &packages.RepoPackages{
				Repository: "acme/widgets",
				Packages: []packages.TagInfo{{
					Tag:       "v1.0.0",
					Digest:    testDigest,
					MediaType: "application/vnd.oci.image.index.v1+json",
					Size:      4096,
					UpdatedAt: "2026-11-21T12:00:00Z",
					Platforms: []string{"linux/amd64", "linux/arm64"},
				}},
				Manifests: []packages.ManifestView{{
					Digest:    testDigest,
					MediaType: "application/vnd.oci.image.index.v1+json",
					Size:      4096,
					Tags:      []string{"v1.0.0"},
					Platforms: []string{"linux/amd64", "linux/arm64"},
				}},
				TotalSize: 4096,
			}, nil
		},
		DeleteTag: func(_ context.Context, _, _, tag string) error {
			*deleted = append(*deleted, "tag:"+tag)
			return nil
		},
		DeleteManifest: func(_ context.Context, _, _, digest string) error {
			*deleted = append(*deleted, "digest:"+digest)
			return nil
		},
		CanPush: func(context.Context, string, string, auth.Principal) bool { return canPush },
	})
}

func packagesFixture(t *testing.T, canPush bool) (*collabFixture, *[]string) {
	t.Helper()
	bindChromeProjections(t)
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	// A writer principal so the write preflight passes; CanPush is the gate
	// under test (the production wiring is packages.Service.CanPush).
	f.principal = auth.Principal{Name: "writer", Write: true}
	deleted := &[]string{}
	bindPackagesDeps(t, canPush, deleted)
	return f, deleted
}

// TestPackagesPageRenders: the read page carries the chrome, the Packages tab,
// the tag row with digest/size/platforms, the pull command and the delete
// affordances.
func TestPackagesPageRenders(t *testing.T) {
	f, _ := packagesFixture(t, true)
	rec := f.get("/acme/widgets/packages")
	wantStatus(t, rec, http.StatusOK)
	body := bodyText(rec)
	wantChrome(t, body, "acme/widgets", "packages")
	for _, want := range []string{
		"v1.0.0",
		"0123456789ab", // the short digest
		"4.0 KiB",
		"linux/amd64",
		"docker pull ",
		`data-copy="#cmd-pull"`,
		`/acme/widgets/packages/tags/v1.0.0/delete`,
		`/acme/widgets/packages/` + url.PathEscape(testDigest) + `/delete`,
		`data-danger-confirm="v1.0.0"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("packages page missing %q", want)
		}
	}
}

// TestPackagesPageFeatureOff: features.packages=false answers 404 and the tab
// disappears from the strip (the same fail-closed rule as every /v2 route).
func TestPackagesPageFeatureOff(t *testing.T) {
	f, _ := packagesFixture(t, true)
	off := config.AllFeatures()
	off.Packages = false
	f.view.summary.Features = &off
	rec := f.get("/acme/widgets/packages")
	wantStatus(t, rec, http.StatusNotFound)
	if tabs := tabsOf(t, rec.Body.String()); len(tabs) > 0 {
		if active := activeTabOf(t, rec.Body.String()); active == "packages" {
			t.Fatal("feature-off page still highlights the Packages tab")
		}
	}
	// The repo page's strip has no Packages entry either.
	rec = f.get("/acme/widgets")
	if strings.Contains(bodyText(rec), `data-tab="packages"`) {
		t.Fatal("Packages tab rendered while the feature is off")
	}
}

// TestPackagesDeleteWrites: the POST deletes through the seam and redirects
// with a flash; no push right ⇒ 403 and nothing deleted.
func TestPackagesDeleteWrites(t *testing.T) {
	f, deleted := packagesFixture(t, true)
	rr := f.post("/acme/widgets/packages/tags/v1.0.0/delete", url.Values{})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete tag = %d: %s", rr.Code, rr.Body.String())
	}
	if len(*deleted) != 1 || (*deleted)[0] != "tag:v1.0.0" {
		t.Fatalf("deleted = %v", *deleted)
	}
	rr = f.post("/acme/widgets/packages/"+url.PathEscape(testDigest)+"/delete", url.Values{})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete digest = %d", rr.Code)
	}
	if len(*deleted) != 2 || !strings.HasPrefix((*deleted)[1], "digest:sha256:") {
		t.Fatalf("deleted = %v", *deleted)
	}

	// A viewer without the push rule cannot delete (the API's own gate).
	f2, deleted2 := packagesFixture(t, false)
	rr = f2.post("/acme/widgets/packages/tags/v1.0.0/delete", url.Values{})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("denied delete = %d", rr.Code)
	}
	if len(*deleted2) != 0 {
		t.Fatalf("denied delete reached the service: %v", *deleted2)
	}
}

// TestPackagesDeleteTypedNilSucceeds is the regression for the page 500 that
// followed a SUCCESSFUL delete: the page seam speaks `error`, the service
// speaks *packages.Error, and returning the service result straight through
// hands success a TYPED-NIL (non-nil interface, nil pointer) that the handler
// reads as a failure — and packageErrStatus then dereferenced it.
func TestPackagesDeleteTypedNilSucceeds(t *testing.T) {
	f, deleted := packagesFixture(t, true)
	old := packagesDeps
	t.Cleanup(func() { packagesDeps = old })
	typedNil := func() error {
		var pe *packages.Error // nil pointer, non-nil interface once returned
		return pe
	}
	packagesDeps.DeleteTag = func(_ context.Context, _, _, tag string) error {
		*deleted = append(*deleted, "tag:"+tag)
		return typedNil()
	}
	packagesDeps.DeleteManifest = func(_ context.Context, _, _, digest string) error {
		*deleted = append(*deleted, "digest:"+digest)
		return typedNil()
	}
	rr := f.post("/acme/widgets/packages/tags/v1.0.0/delete", url.Values{})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("typed-nil tag delete = %d (want %d): %s", rr.Code, http.StatusSeeOther, rr.Body.String())
	}
	rr = f.post("/acme/widgets/packages/"+url.PathEscape(testDigest)+"/delete", url.Values{})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("typed-nil digest delete = %d: %s", rr.Code, rr.Body.String())
	}
	if len(*deleted) != 2 {
		t.Fatalf("deleted = %v", *deleted)
	}
	// And the mapper itself must not dereference a typed-nil.
	var pe *packages.Error
	if got := packageErrStatus(pe); got != http.StatusServiceUnavailable {
		t.Fatalf("packageErrStatus(typed-nil) = %d", got)
	}
}

// TestPackagesDeleteNotFoundAndUnknownRepo pins the error mapping.
func TestPackagesDeleteNotFoundAndUnknownRepo(t *testing.T) {
	f, _ := packagesFixture(t, true)
	old := packagesDeps
	t.Cleanup(func() { packagesDeps = old })
	packagesDeps.DeleteTag = func(context.Context, string, string, string) error {
		return &packages.Error{Status: http.StatusNotFound, Code: "MANIFEST_UNKNOWN", Message: "manifest unknown"}
	}
	rr := f.post("/acme/widgets/packages/tags/nope/delete", url.Values{})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown tag delete = %d", rr.Code)
	}
	packagesDeps.DeleteTag = func(context.Context, string, string, string) error {
		return errors.New("boom")
	}
	rr = f.post("/acme/widgets/packages/tags/x/delete", url.Values{})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("service failure = %d", rr.Code)
	}
	// Unknown repo: the read gate 404s before any delete.
	delete(f.reg.exists, "acme/widgets")
	rr = f.get("/acme/widgets/packages")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown repo page = %d", rr.Code)
	}
}

// TestPackagesPageUnwired: an unbound seam renders the honest card instead of
// a dead button.
func TestPackagesPageUnwired(t *testing.T) {
	bindChromeProjections(t)
	f := newCollabFixture(t)
	f.view.summary = busySummary()
	old := packagesDeps
	t.Cleanup(func() { packagesDeps = old })
	BindPackages(PackagesDeps{})
	rec := f.get("/acme/widgets/packages")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	if !strings.Contains(bodyText(rec), "not wired") {
		t.Fatal("unwired page must say so")
	}
}

// shortDigest/shortMediaType/humanBytes are the page's display helpers.
func TestPackagesDisplayHelpers(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"sha256:0123456789abcdef": "0123456789ab",
		"weird":                   "weird",
	}
	for in, want := range cases {
		if got := shortDigest(in); got != want {
			t.Errorf("shortDigest(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"application/vnd.oci.image.index.v1+json":                   "OCI index",
		"application/vnd.docker.distribution.manifest.list.v2+json": "manifest list",
		"application/vnd.oci.image.manifest.v1+json":                "OCI image",
		"application/vnd.oci.artifact.manifest.v1+json":             "artifact",
		"":                 "manifest",
		"application/json": "application/json",
	} {
		if got := shortMediaType(in); got != want {
			t.Errorf("shortMediaType(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[int64]string{
		12:              "12 B",
		4096:            "4.0 KiB",
		3 * 1024 * 1024: "3.0 MiB",
	} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestPackagesUsageStates is the rendered usage/quota contract (docs/features/14
// §5): the numbers, the words that state the condition (never colour alone),
// the meter's accessible reading only when a quota exists, and the unbounded
// copy when none is set.
func TestPackagesUsageStates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		usage   *packages.UsageInfo
		want    []string
		notWant []string
	}{
		{
			name:  "within quota",
			usage: &packages.UsageInfo{Bytes: 2048, QuotaBytes: 4096, Blobs: 3, Manifests: 2},
			want: []string{
				"Storage", "2.0 KiB of 4.0 KiB quota", "within quota", "2.0 KiB remaining",
				"3 blob(s) · 2 manifest(s)", `role="progressbar"`, `aria-valuenow="50"`,
				`aria-valuetext="2.0 KiB of 4.0 KiB quota used (50%); 2.0 KiB free"`,
				"[packages] quota_bytes", "packages-gc",
			},
			notWant: []string{"over quota", "no quota is set"},
		},
		{
			name:    "over quota",
			usage:   &packages.UsageInfo{Bytes: 8192, QuotaBytes: 4096, Blobs: 9, Manifests: 4},
			want:    []string{"8.0 KiB of 4.0 KiB quota", "over quota", `aria-valuenow="100"`, "0 B remaining"},
			notWant: []string{"within quota"},
		},
		{
			name:    "unlimited",
			usage:   &packages.UsageInfo{Bytes: 8192, Blobs: 9, Manifests: 4},
			want:    []string{"Storage", "8.0 KiB", "no quota", "storage is unbounded"},
			notWant: []string{"role=\"progressbar\"", "of 4.0 KiB quota"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := packagesFixture(t, true)
			usage := tc.usage
			packagesDeps.List = func(context.Context, string, string) (*packages.RepoPackages, error) {
				return &packages.RepoPackages{
					Repository: "acme/widgets",
					Packages:   []packages.TagInfo{{Tag: "v1.0.0", Digest: testDigest, Size: 4096}},
					Manifests:  []packages.ManifestView{{Digest: testDigest, Size: 4096, Tags: []string{"v1.0.0"}}},
					TotalSize:  4096,
					Usage:      usage,
				}, nil
			}
			body := bodyText(f.get("/acme/widgets/packages"))
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("page missing %q", want)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(body, bad) {
					t.Errorf("page unexpectedly contains %q", bad)
				}
			}
		})
	}
}
