package config

// docs/features/19_static_sites.md §5: the host section [static_sites] (the
// dedicated static listener) and the per-repo [static_site] publication
// passenger. The global rules are fail-closed but gated on `enabled`; the
// per-repo section is validated at publish time and never merges.

import (
	"slices"
	"strings"
	"testing"
)

func TestStaticSitesDefaults(t *testing.T) {
	s := Defaults().StaticSites
	if s.Enabled {
		t.Fatal("static sites must be off by default: the listener is not started when false")
	}
	if s.Address != "127.0.0.1:8081" {
		t.Fatalf("address = %q, want 127.0.0.1:8081", s.Address)
	}
	if s.Host != "static.localhost" {
		t.Fatalf("host = %q, want static.localhost", s.Host)
	}
	if s.MaxFileBytes != 8<<20 {
		t.Fatalf("max_file_bytes = %d, want 8MiB", s.MaxFileBytes)
	}
	if len(s.IndexFiles) != 2 || s.IndexFiles[0] != "index.html" || s.IndexFiles[1] != "index.htm" {
		t.Fatalf("index_files = %v, want [index.html index.htm]", s.IndexFiles)
	}
}

func TestStaticSitesDisabledAcceptsAnything(t *testing.T) {
	c := testBaseConfig()
	c.StaticSites = StaticSites{
		Enabled:      false,
		Address:      "not-an-address",
		Host:         "",
		MaxFileBytes: 0,
		IndexFiles:   []string{"", "../escape", "a/b"},
	}
	_, errs := Validate(c)
	for _, err := range errs {
		if strings.Contains(err.Error(), "static_sites") {
			t.Fatalf("disabled section must not be validated, got %v", err)
		}
	}
}

func TestStaticSitesValidate(t *testing.T) {
	good := func() *Config {
		c := testBaseConfig()
		c.StaticSites = Defaults().StaticSites
		c.StaticSites.Enabled = true
		return c
	}
	// The default enabled shape is valid.
	if _, errs := Validate(good()); len(errs) != 0 {
		t.Fatalf("enabled defaults must validate: %v", errs)
	}

	t.Run("host required", func(t *testing.T) {
		for _, host := range []string{"", "   "} {
			c := good()
			c.StaticSites.Host = host
			_, errs := Validate(c)
			errsContain(t, errs, "static_sites.host is required")
		}
		c := good()
		c.StaticSites.Host = " static.localhost "
		_, errs := Validate(c)
		errsContain(t, errs, "static_sites.host")
	})

	t.Run("address required and parseable", func(t *testing.T) {
		c := good()
		c.StaticSites.Address = ""
		_, errs := Validate(c)
		errsContain(t, errs, "static_sites.address is required")

		// SplitHostPort shape only, exactly like server.listen and
		// server.ssh.listen: a non-numeric port fails at net.Listen with the
		// service-name error, not here.
		for _, bad := range []string{"8081", "127.0.0.1", ":8081:extra"} {
			c := good()
			c.StaticSites.Address = bad
			_, errs := Validate(c)
			errsContain(t, errs, "static_sites.address")
		}
	})

	t.Run("address must differ from server.listen", func(t *testing.T) {
		c := good()
		c.StaticSites.Address = c.Server.Listen
		_, errs := Validate(c)
		errsContain(t, errs, "is server.listen")

		// A different port on the same host is fine.
		c = good()
		c.Server.Listen = "127.0.0.1:8080"
		c.StaticSites.Address = "127.0.0.1:8081"
		if _, errs := Validate(c); len(errs) != 0 {
			t.Fatalf("distinct listener addresses must validate: %v", errs)
		}
	})

	t.Run("max_file_bytes must be positive", func(t *testing.T) {
		c := good()
		c.StaticSites.MaxFileBytes = 0
		_, errs := Validate(c)
		errsContain(t, errs, "static_sites.max_file_bytes must be positive")

		c = good()
		c.StaticSites.MaxFileBytes = -1
		_, errs = Validate(c)
		errsContain(t, errs, "static_sites.max_file_bytes")
	})

	t.Run("max_file_bytes has a ceiling", func(t *testing.T) {
		// The read path buffers one file per request; the ceiling is what the
		// listener's pool is sized for, so 4GiB must not boot.
		c := good()
		c.StaticSites.MaxFileBytes = 65 << 20
		_, errs := Validate(c)
		errsContain(t, errs, "static_sites.max_file_bytes")
		errsContain(t, errs, "64MiB")

		c = good()
		c.StaticSites.MaxFileBytes = 64 << 20
		if _, errs := Validate(c); len(errs) != 0 {
			t.Fatalf("exactly 64MiB must validate: %v", errs)
		}
		// The default stays 8MiB.
		c = good()
		c.StaticSites.MaxFileBytes = 8 << 20
		if _, errs := Validate(c); len(errs) != 0 {
			t.Fatalf("8MiB must validate: %v", errs)
		}
	})

	t.Run("host must not be the application origin", func(t *testing.T) {
		// public_url names the app's origin; a static host equal to it would
		// serve user HTML/JS on the session origin (docs/features/19 §2).
		c := good()
		for _, host := range []string{"git.example.com", "Git.Example.COM", "git.example.com:8443"} {
			c = good()
			c.Server.PublicURL = "https://git.example.com"
			c.StaticSites.Host = host
			_, errs := Validate(c)
			errsContain(t, errs, "static_sites.host")
			errsContain(t, errs, "server.public_url")
		}
		// The public_url port must not matter either.
		c = good()
		c.Server.PublicURL = "https://git.example.com:8443"
		c.StaticSites.Host = "git.example.com"
		_, errs := Validate(c)
		errsContain(t, errs, "static_sites.host")

		// A distinct static host is the whole point and stays valid.
		c = good()
		c.Server.PublicURL = "https://git.example.com"
		c.StaticSites.Host = "static.example.com"
		if _, errs := Validate(c); len(errs) != 0 {
			t.Fatalf("a distinct static host must validate: %v", errs)
		}

		// With no public_url the fallback is server.listen's hostname, but only
		// when it names a real host — a wildcard bind names no origin.
		c = good()
		c.Server.PublicURL = ""
		c.Server.Listen = "git.example.com:8080"
		c.StaticSites.Host = "git.example.com"
		_, errs = Validate(c)
		errsContain(t, errs, "server.listen")

		for _, tc := range []struct{ listen, host string }{
			{"0.0.0.0:8080", "0.0.0.0"},
			{"[::]:8080", "::"},
			{"[::]:8080", "[::1]"},
		} {
			c = good()
			c.Server.PublicURL = ""
			c.Server.Listen = tc.listen
			c.StaticSites.Host = tc.host
			if _, errs := Validate(c); len(errs) != 0 {
				t.Fatalf("wildcard listen %q with host %q names no origin and must not collide: %v", tc.listen, tc.host, errs)
			}
		}

		// public_url wins over listen when both are set.
		c = good()
		c.Server.PublicURL = "https://app.example.com"
		c.Server.Listen = "127.0.0.1:8080"
		c.StaticSites.Host = "127.0.0.1"
		if _, errs := Validate(c); len(errs) != 0 {
			t.Fatalf("listen host is not the app origin when public_url is set: %v", errs)
		}
	})

	t.Run("address must not be server.listen's IPv6 twin", func(t *testing.T) {
		c := good()
		c.Server.Listen = "127.0.0.1:8080"
		c.StaticSites.Address = "[::1]:8080"
		_, errs := Validate(c)
		errsContain(t, errs, "static_sites.address")
		errsContain(t, errs, "IPv6 twin")

		// The twin of a wildcard or hostname bind does not exist.
		for _, listen := range []string{"0.0.0.0:8080", "[::]:8080", "localhost:8080"} {
			c = good()
			c.Server.Listen = listen
			c.StaticSites.Address = "[::1]:8080"
			if _, errs := Validate(c); len(errs) != 0 {
				t.Fatalf("listen %q has no loopback twin: %v", listen, errs)
			}
		}
		// The same port on the main family is fine (a different socket).
		c = good()
		c.Server.Listen = "127.0.0.1:8080"
		c.StaticSites.Address = "127.0.0.1:8081"
		if _, errs := Validate(c); len(errs) != 0 {
			t.Fatalf("distinct twin-family addresses must validate: %v", errs)
		}
	})

	t.Run("index files are plain names", func(t *testing.T) {
		for _, bad := range []string{"", " ", " docs/index.html", "docs/index.html", `docs\index.html`, "../index.html", "index..html", "."} {
			c := good()
			c.StaticSites.IndexFiles = []string{bad}
			_, errs := Validate(c)
			errsContain(t, errs, "static_sites.index_files[0]")
		}
		c := good()
		c.StaticSites.IndexFiles = []string{"index.html", "default.htm"}
		if _, errs := Validate(c); len(errs) != 0 {
			t.Fatalf("plain index names must validate: %v", errs)
		}
	})
}

// TestStaticSitesEnvOverlay pins the exact dotted path: a missing segment is
// silently recorded as an unknown key, which is how env overlays have gone
// wrong before (AGENTS.md field lessons).
func TestStaticSitesEnvOverlay(t *testing.T) {
	env := map[string]string{
		"SPINDLE__STATIC_SITES__ENABLED":        "true",
		"SPINDLE__STATIC_SITES__ADDRESS":        "0.0.0.0:9001",
		"SPINDLE__STATIC_SITES__HOST":           "pages.example.com",
		"SPINDLE__STATIC_SITES__MAX_FILE_BYTES": "2MiB",
		"SPINDLE__STATIC_SITES__INDEX_FILES":    `["index.html"]`,
	}
	c := Defaults()
	overrides, err := applyEnv(c, func(k string) string { return env[k] }, envList(env))
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if len(overrides) != 0 {
		t.Fatalf("ignored overrides = %+v, want none", overrides)
	}
	if !c.StaticSites.Enabled || c.StaticSites.Address != "0.0.0.0:9001" ||
		c.StaticSites.Host != "pages.example.com" ||
		c.StaticSites.MaxFileBytes != 2<<20 ||
		len(c.StaticSites.IndexFiles) != 1 || c.StaticSites.IndexFiles[0] != "index.html" {
		t.Fatalf("env overlay missed the section: %+v", c.StaticSites)
	}
}

func TestParseRepoStaticSite(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		check   func(t *testing.T, rs *RepoSettings)
	}{
		{
			name:    "absent section stays nil and unpublished",
			payload: "[bundles]\nmin_commits = 5\n",
			check: func(t *testing.T, rs *RepoSettings) {
				t.Helper()
				if rs.StaticSite != nil {
					t.Fatalf("static_site = %+v, want nil", rs.StaticSite)
				}
				if got := rs.StaticSite.Resolve(); got.Enabled {
					t.Fatalf("nil section resolved %+v, want unpublished", got)
				}
			},
		},
		{
			name:    "empty section stays unpublished",
			payload: "[static_site]\n",
			check: func(t *testing.T, rs *RepoSettings) {
				t.Helper()
				if rs.StaticSite == nil {
					t.Fatal("static_site section must decode non-nil")
				}
				if rs.StaticSite.Enabled != nil {
					t.Fatalf("enabled = %v, want nil (unset)", *rs.StaticSite.Enabled)
				}
				if got := rs.StaticSite.Resolve(); got.Enabled {
					t.Fatalf("empty section resolved %+v, want unpublished", got)
				}
			},
		},
		{
			name:    "explicit false is distinguishable but unpublished",
			payload: "[static_site]\nenabled = false\n",
			check: func(t *testing.T, rs *RepoSettings) {
				t.Helper()
				if rs.StaticSite == nil || rs.StaticSite.Enabled == nil {
					t.Fatal("explicit false must decode to a non-nil pointer")
				}
				if *rs.StaticSite.Enabled {
					t.Fatal("explicit false decoded as true")
				}
				if got := rs.StaticSite.Resolve(); got.Enabled {
					t.Fatalf("explicit false resolved %+v, want unpublished", got)
				}
			},
		},
		{
			name:    "enabled with ref and root",
			payload: "description = \"site\"\n[static_site]\nenabled = true\nref = \"gh-pages\"\nroot = \"public/docs\"\n",
			check: func(t *testing.T, rs *RepoSettings) {
				t.Helper()
				got := rs.StaticSite.Resolve()
				want := ResolvedStaticSite{Enabled: true, Ref: "gh-pages", Root: "public/docs"}
				if got != want {
					t.Fatalf("resolved = %+v, want %+v", got, want)
				}
				if rs.Description != "site" {
					t.Fatalf("description = %q, want coexisting key intact", rs.Description)
				}
			},
		},
		{
			name:    "full ref names and tag refs are accepted",
			payload: "[static_site]\nenabled = true\nref = \"refs/heads/gh-pages\"\n",
			check: func(t *testing.T, rs *RepoSettings) {
				t.Helper()
				if got := rs.StaticSite.Resolve(); got.Ref != "refs/heads/gh-pages" {
					t.Fatalf("ref = %q", got.Ref)
				}
			},
		},
		{
			name:    "empty root is the repository root",
			payload: "[static_site]\nenabled = true\nroot = \"\"\n",
			check: func(t *testing.T, rs *RepoSettings) {
				t.Helper()
				if got := rs.StaticSite.Resolve(); got.Root != "" {
					t.Fatalf("root = %q", got.Root)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs, err := ParseRepoSettings([]byte(tt.payload))
			if err != nil {
				t.Fatalf("ParseRepoSettings: %v", err)
			}
			tt.check(t, rs)
		})
	}
}

func TestParseRepoStaticSiteRejects(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr string
	}{
		{name: "ref with dotdot", payload: "[static_site]\nenabled = true\nref = \"main..old\"\n", wantErr: "static_site.ref"},
		{name: "ref with space", payload: "[static_site]\nenabled = true\nref = \"my branch\"\n", wantErr: "static_site.ref"},
		{name: "ref with tilde", payload: "[static_site]\nenabled = true\nref = \"main~1\"\n", wantErr: "static_site.ref"},
		{name: "ref with colon", payload: "[static_site]\nenabled = true\nref = \"main:old\"\n", wantErr: "static_site.ref"},
		{name: "ref with backslash", payload: "[static_site]\nenabled = true\nref = \"main\\\\old\"\n", wantErr: "static_site.ref"},
		{name: "ref with lock suffix", payload: "[static_site]\nenabled = true\nref = \"main.lock\"\n", wantErr: "static_site.ref"},
		{name: "ref with leading dash", payload: "[static_site]\nenabled = true\nref = \"-main\"\n", wantErr: "static_site.ref"},
		{name: "ref with at-brace", payload: "[static_site]\nenabled = true\nref = \"main@{1}\"\n", wantErr: "static_site.ref"},
		{name: "ref with empty component", payload: "[static_site]\nenabled = true\nref = \"refs//heads/main\"\n", wantErr: "static_site.ref"},
		{name: "ref with dot component", payload: "[static_site]\nenabled = true\nref = \"refs/.hidden/main\"\n", wantErr: "static_site.ref"},
		{name: "root with dotdot", payload: "[static_site]\nenabled = true\nroot = \"../outside\"\n", wantErr: "static_site.root"},
		{name: "root with inner dotdot", payload: "[static_site]\nenabled = true\nroot = \"docs/../../outside\"\n", wantErr: "static_site.root"},
		{name: "root absolute", payload: "[static_site]\nenabled = true\nroot = \"/docs\"\n", wantErr: "static_site.root"},
		{name: "root not clean", payload: "[static_site]\nenabled = true\nroot = \"docs/./site\"\n", wantErr: "names the directory itself"},
		{name: "root trailing slash", payload: "[static_site]\nenabled = true\nroot = \"docs/\"\n", wantErr: "must not end with '/'"},
		{name: "root empty segment", payload: "[static_site]\nenabled = true\nroot = \"docs//site\"\n", wantErr: "empty path segment"},
		{name: "root with backslash", payload: "[static_site]\nenabled = true\nroot = \"docs\\\\site\"\n", wantErr: "backslash"},
		// The handler's cleanRoot refuses these three, so accepting them here
		// would publish a site that is a permanent 404 (m9: the two rules are
		// one contract).
		{name: "root dot", payload: "[static_site]\nenabled = true\nroot = \".\"\n", wantErr: "static_site.root"},
		{name: "root dot directory", payload: "[static_site]\nenabled = true\nroot = \"docs/.hidden\"\n", wantErr: "dot-directory"},
		{name: "root leading dot directory", payload: "[static_site]\nenabled = true\nroot = \".config/site\"\n", wantErr: "dot-directory"},
		{name: "root control byte", payload: "[static_site]\nenabled = true\nroot = \"docs/\\u0001site\"\n", wantErr: "control bytes"},
		{name: "root DEL byte", payload: "[static_site]\nenabled = true\nroot = \"docs/\\u007fsite\"\n", wantErr: "control bytes"},
		{name: "root too long", payload: "[static_site]\nenabled = true\nroot = \"" + strings.Repeat("a", 513) + "\"\n", wantErr: "limit is 512"},
		{name: "unknown key in static_site", payload: "[static_site]\nteleport = true\n", wantErr: "unknown key"},
		{name: "wrong type", payload: "[static_site]\nenabled = \"yes\"\n", wantErr: "repo settings"},
		{name: "bare static_site key", payload: "static_site = true\n", wantErr: "repo settings"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRepoSettings([]byte(tt.payload))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestRepoStaticSiteRootParity pins the publish-time root rule to the handler's
// cleanRoot (internal/staticpages/path.go, §5). The two are one contract: a
// root the handler refuses would publish a permanent-404 site, and config could
// not import staticpages (it would close an import cycle), so the shared shape
// is asserted here as a table instead.
func TestRepoStaticSiteRootParity(t *testing.T) {
	accepted := []string{"", "public", "public/docs", "docs.v2", "a-b_c", strings.Repeat("a", 512)}
	for _, root := range accepted {
		if err := ValidateRepoStaticSite(&RepoStaticSite{Root: root}); err != nil {
			t.Errorf("root %q rejected, want accepted: %v", root, err)
		}
	}
	rejected := []string{
		".",               // "." names the directory itself
		"docs/.hidden",    // dot-directory
		".config/site",    // leading dot-directory
		"docs/../outside", // escape
		"../outside",      // escape
		"/docs",           // absolute
		"docs/",           // trailing slash
		"docs//site",      // empty segment
		"docs\\site",      // backslash
		"docs/\x00site",   // NUL / control byte
		"docs/\x01site",   // control byte
		"docs/\x7fsite",   // DEL
		strings.Repeat("a", 513),
	}
	for _, root := range rejected {
		err := ValidateRepoStaticSite(&RepoStaticSite{Root: root})
		if err == nil {
			t.Errorf("root %q accepted, want rejected", root)
			continue
		}
		if !strings.Contains(err.Error(), "static_site.root") {
			t.Errorf("root %q error %q does not name static_site.root", root, err)
		}
	}
}

func TestValidGitRefName(t *testing.T) {
	valid := []string{
		"main",
		"gh-pages",
		"refs/heads/gh-pages",
		"refs/tags/v1.2.3",
		"release/v1.2",
		"v1.0",
		"a.b",
	}
	for _, name := range valid {
		if !validGitRefName(name) {
			t.Errorf("validGitRefName(%q) = false, want true", name)
		}
	}
	invalid := []string{
		"",                     // empty
		"@",                    // reserved single character
		"-main",                // option confusion
		"/main",                // leading slash
		"main/",                // trailing slash
		"main.",                // trailing dot
		"main..old",            // ".."
		"main@{1}",             // "@{"
		"refs//heads/main",     // empty component
		"refs/.hidden/main",    // dot component
		"refs/heads/main.lock", // .lock component
		"a b",
		"a\tb",
		"a~b",
		"a^b",
		"a:b",
		"a?b",
		"a*b",
		"a[b",
		`a\b`,
		"a\x01b", // control byte
		"a\x7fb", // DEL
	}
	for _, name := range invalid {
		if validGitRefName(name) {
			t.Errorf("validGitRefName(%q) = true, want false", name)
		}
	}
}

func TestRepoSettingsStaticSiteMergeIgnored(t *testing.T) {
	// The section rides the document (persistence, revisioning, admin writes)
	// but never merges into the host config — the Description/Features/Review
	// precedent: Merge and ValidateAgainst both succeed, the merged host
	// section is untouched, and the merged sections still apply.
	rs, err := ParseRepoSettings([]byte("[static_site]\nenabled = true\nref = \"gh-pages\"\nroot = \"public\"\n[bundles]\nmin_commits = 5\n"))
	if err != nil {
		t.Fatalf("ParseRepoSettings: %v", err)
	}
	base := testBaseConfig()
	base.StaticSites = Defaults().StaticSites
	merged, err := rs.Merge(base)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if merged.StaticSites.Enabled != base.StaticSites.Enabled ||
		merged.StaticSites.Address != base.StaticSites.Address ||
		merged.StaticSites.Host != base.StaticSites.Host ||
		merged.StaticSites.MaxFileBytes != base.StaticSites.MaxFileBytes ||
		!slices.Equal(merged.StaticSites.IndexFiles, base.StaticSites.IndexFiles) {
		t.Fatalf("merged static_sites = %+v, want host section untouched", merged.StaticSites)
	}
	if merged.Bundles.MinCommits != 5 {
		t.Fatalf("merged bundles = %+v, want settings value", merged.Bundles)
	}
	if err := rs.ValidateAgainst(base); err != nil {
		t.Fatalf("ValidateAgainst with static_site: %v", err)
	}
}

func TestStaticSiteOf(t *testing.T) {
	tests := []struct {
		name string
		body string
		want ResolvedStaticSite
	}{
		{name: "empty body is unpublished", body: "", want: ResolvedStaticSite{}},
		{name: "whitespace body is unpublished", body: "  \n", want: ResolvedStaticSite{}},
		{name: "no section is unpublished", body: "description = \"hi\"\n", want: ResolvedStaticSite{}},
		{name: "corrupt body is unpublished", body: "[broken\n", want: ResolvedStaticSite{}},
		{name: "unknown key is unpublished", body: "[static_site]\nenabled = true\nteleport = true\n", want: ResolvedStaticSite{}},
		{name: "invalid ref is unpublished", body: "[static_site]\nenabled = true\nref = \"a..b\"\n", want: ResolvedStaticSite{}},
		{name: "explicit false", body: "[static_site]\nenabled = false\n", want: ResolvedStaticSite{}},
		{
			name: "enabled",
			body: "[features]\nissues = false\n[static_site]\nenabled = true\nref = \"gh-pages\"\nroot = \"public\"\n",
			want: ResolvedStaticSite{Enabled: true, Ref: "gh-pages", Root: "public"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StaticSiteOf([]byte(tt.body)); got != tt.want {
				t.Fatalf("StaticSiteOf = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// envList renders a map as the KEY=VALUE environ slice applyEnv consumes.
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
