package actions

import (
	"strings"
	"testing"
)

// TestSetupCacheMappings pins PAR-096: every ecosystem the cache engine can
// express maps onto one built-in actions/cache pair; a manager the table does
// not know is refused BY NAME with the reason (never a silent no-cache).
func TestSetupCacheMappings(t *testing.T) {
	for _, tc := range []struct {
		base, cache string
		ok          bool
		want        string
	}{
		{"actions/setup-node", "npm", true, ""},
		{"actions/setup-node", "yarn", true, ""},
		{"actions/setup-node", "pnpm", true, ""},
		{"actions/setup-node", "", false, ""},
		{"actions/setup-node", "bundler", false, "not a manager this step maps"},
		{"actions/setup-python", "pip", true, ""},
		{"actions/setup-python", "poetry", false, "not a manager this step maps"},
		{"actions/setup-go", "true", true, ""},
		{"actions/setup-java", "maven", false, "not one of the mapped ecosystems"},
		{"actions/setup-ruby", "bundler", false, "not one of the mapped ecosystems"},
	} {
		_, ok, reason := SetupCacheInput(tc.base, tc.cache)
		if ok != tc.ok {
			t.Fatalf("%s cache=%q: ok = %v reason = %q", tc.base, tc.cache, ok, reason)
		}
		if tc.want != "" && !strings.Contains(reason, tc.want) {
			t.Fatalf("%s cache=%q: reason %q missing %q", tc.base, tc.cache, reason, tc.want)
		}
		if !ok && reason == "" {
			t.Fatalf("%s cache=%q: a refusal needs a reason", tc.base, tc.cache)
		}
	}
}

func TestSetupCacheShape(t *testing.T) {
	c, ok := SetupCacheFor("actions/setup-node")
	if !ok {
		t.Fatal("setup-node must map")
	}
	if c.Paths != ".npm" || c.Env["npm_config_cache"] != ".npm" {
		t.Fatalf("node mapping = %+v", c)
	}
	if c.RestoreKey != "npm-${{ runner.os }}-${{ github.ref_name }}-" {
		t.Fatalf("restore key = %q", c.RestoreKey)
	}
	goCache, _ := SetupCacheFor("actions/setup-go")
	if len(goCache.Env) != 2 || goCache.Env["GOMODCACHE"] != ".cache/gomod" {
		t.Fatalf("go mapping = %+v", goCache)
	}
}

// TestSetupRefusalQuotesTheRecipe: the refusal names the exact replacement so an
// author can act without guessing (the PAR-096 acceptance).
func TestSetupRefusalQuotesTheRecipe(t *testing.T) {
	msg := SetupRefusal("actions/setup-node", map[string]string{"cache": "npm", "node-version": "22"}, "the mutable ref is not allowed")
	for _, want := range []string{
		"unsupported action actions/setup-node", "the mutable ref is not allowed",
		"cache, node-version", "npm_config_cache: .npm",
		"uses: actions/cache", "path: '.npm'", "restore-keys: npm-${{ runner.os }}-${{ github.ref_name }}-",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal %q missing %q", msg, want)
		}
	}
	unknown := SetupRefusal("actions/setup-ruby", nil, "")
	if !strings.Contains(unknown, "not one of the mapped ecosystems") {
		t.Fatalf("unknown refusal = %q", unknown)
	}
}
