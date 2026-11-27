package runner

// A runner that grants ANY container capability — privilege, a shared network,
// a workflow-volume allowlist or a docker endpoint — never executes fork code
// (D-8: a workflow can never widen its own isolation). A shared network reaches
// other containers and a volume allowlist reaches host paths, so the exclusion
// covers all four grants, not only privilege.

import (
	"strings"
	"testing"
)

func TestNewRefusesForkRunsWithAnyContainerGrant(t *testing.T) {
	base := Config{ServerURL: "http://127.0.0.1:1", Token: "wrt_x", AllowForkRuns: true, AllowHostDocker: true, Capacity: 1}
	for _, tc := range []struct {
		name  string
		apply func(*Config)
		want  string
	}{
		{"privilege", func(c *Config) { c.Policy.Privileged = true }, "--container-privileged"},
		{"docker endpoint", func(c *Config) { c.Policy.DockerHost = "tcp://127.0.0.1:2375" }, "--docker-host"},
		{"shared network", func(c *Config) { c.Policy.Network = "ci-shared" }, "--container-network"},
		{"volume allowlist", func(c *Config) { c.Policy.ValidVolumes = []string{"/srv/cache"} }, "--container-valid-volume"},
	} {
		cfg := base
		tc.apply(&cfg)
		_, err := New(cfg)
		if err == nil || !strings.Contains(err.Error(), "--allow-fork-runs") || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want the %s exclusion", tc.name, err, tc.want)
		}
	}
	// Without the fork opt-in the same grants are legitimate.
	cfg := base
	cfg.AllowForkRuns = false
	cfg.Policy.ValidVolumes = []string{"/srv/cache"}
	if _, err := New(cfg); err != nil {
		t.Fatalf("a granting runner that refuses fork code must start: %v", err)
	}
	// The policy itself is validated before the exclusion names it.
	cfg = base
	cfg.Policy.ValidVolumes = []string{"relative/path"}
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "absolute host path") {
		t.Fatalf("err = %v, want the policy validation", err)
	}
}
