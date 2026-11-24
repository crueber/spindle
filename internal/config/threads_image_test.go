package config

// threads.image_allow_prefixes / threads.image_require_digest validation
// (docs/PLAN-ARTIFACT-KEEPER.md §5.2): the keys' defaults, the accepted
// spellings, and the named errors the Configuration page marks its row with.

import (
	"strings"
	"testing"
)

func TestThreadsImageDefaults(t *testing.T) {
	c := Defaults()
	if len(c.Threads.ImageAllowPrefixes) != 0 {
		t.Fatalf("default allowlist = %v, want empty (unrestricted)", c.Threads.ImageAllowPrefixes)
	}
	if c.Threads.ImageRequireDigest {
		t.Fatal("image_require_digest defaults on; it must be a deliberate rollout")
	}
	if _, errs := Validate(validOIDC()); len(errs) != 0 {
		t.Fatalf("defaults must validate: %v", errs)
	}
}

func TestValidateImageAllowPrefixes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		want    string // fragment the error must carry; "" = valid
	}{
		{"empty list", nil, ""},
		{"hub library", []string{"docker.io/library"}, ""},
		{"mirror prefix", []string{"ak.example/dockerhub"}, ""},
		{"host and port", []string{"localhost:5000/ci-images"}, ""},
		{"ipv4 and port", []string{"192.168.1.5:5000/a/b"}, ""},
		{"several", []string{"docker.io/library", "ghcr.io/acme", "ak.example/dockerhub"}, ""},
		{"duplicate is tolerated", []string{"docker.io/library", "docker.io/library"}, ""},
		{"empty entry", []string{""}, "threads.image_allow_prefixes[0]"},
		{"scheme", []string{"https://ak.example/dockerhub"}, "must not carry a scheme"},
		{"trailing slash", []string{"ak.example/dockerhub/"}, "must not end with '/'"},
		{"host only", []string{"docker.io"}, "host[:port]/path"},
		{"uppercase", []string{"AK.example/dockerhub"}, "lowercase"},
		{"tag", []string{"docker.io/library:latest"}, "must not name a tag"},
		{"digest", []string{"docker.io/library@sha256:" + strings.Repeat("a", 64)}, "not a digest"},
		{"second entry names its row", []string{"docker.io/library", "bad entry"}, "threads.image_allow_prefixes[1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validOIDC()
			c.Threads.ImageAllowPrefixes = tc.entries
			_, errs := Validate(c)
			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("entries %v must validate: %v", tc.entries, errs)
				}
				return
			}
			errsContain(t, errs, tc.want)
			// Every entry error names the KEY (the setup page marks the row).
			errsContain(t, errs, "threads.image_allow_prefixes")
		})
	}
}

// TestThreadsImageKeysLoadFromTOML pins the file spelling: the admin page's
// save path writes these names, so a tag drift would silently drop the policy.
func TestThreadsImageKeysLoadFromTOML(t *testing.T) {
	dir := t.TempDir()
	body := `
[server.auth]
issuer = "https://id.example.com"
allowed_domains = ["example.com"]
oauth_client_id = "id"
oauth_client_secret = "secret"
session_secret = "` + strings.Repeat("x", 32) + `"

[threads]
image_allow_prefixes = ["docker.io/library", "ak.example/dockerhub"]
image_require_digest = true
`
	path := writeConfig(t, dir, "spindle.toml", body)
	c, _, err := Load([]string{path}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Join(c.Threads.ImageAllowPrefixes, ","); got != "docker.io/library,ak.example/dockerhub" {
		t.Fatalf("image_allow_prefixes = %q", got)
	}
	if !c.Threads.ImageRequireDigest {
		t.Fatal("image_require_digest did not load")
	}
	if _, errs := Validate(c); len(errs) != 0 {
		t.Fatalf("loaded config must validate: %v", errs)
	}
}
