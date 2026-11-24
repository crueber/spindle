package config

// seam_gate_test.go — PLAN-OIDC-ONLY §6.4: assert the configuration surface has
// no authentication bypass. The strongest claim available is that there is
// nothing to leak: this gate fails if a bypass-shaped key name ever appears in
// the config package's non-test sources.

import (
	"os"
	"strings"
	"testing"
)

func TestNoTestAuthSeamInConfig(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	bypass := []string{"testauth", "auth_bypass", "dev_auth", "insecure_auth", "no_auth", "test_auth"}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		lower := strings.ToLower(string(raw))
		for _, marker := range bypass {
			if strings.Contains(lower, marker) {
				t.Fatalf("%s contains a bypass-shaped auth key %q — the release binary has no unauthenticated seam", name, marker)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("seam gate scanned no sources (wrong working directory?)")
	}
}
