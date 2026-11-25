// admin_import_live_test.go — the /admin/config save path (adminSaveFn, the
// same function the page's POST and the API's PUT bind) publishes the saved
// [import] SSRF policy to config.LiveImport, so the import/mirror gates read it
// on the very next request. It also pins the two properties the operator sees:
// the policy changed immediately, and no import.* key appears in the "Restart
// required" list.
package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/setup"
)

// liveSaveFixture writes a valid spindle.toml, boots a config from it, and
// returns the running config, the boot state and a live policy holder wired
// the way serveHTTP wires it.
func liveSaveFixture(t *testing.T) (*config.Config, server.BootState, *config.LiveImport) {
	t.Helper()
	dir := t.TempDir()
	base := config.FirstRunDefaults(dir)
	base.Server.Auth.Issuer = "https://id.example.com"
	base.Server.Auth.AllowedDomains = []string{"example.com"}
	base.Server.Auth.OAuthClientID = "spindle"
	base.Server.Auth.OAuthClientSecret = "client-secret"
	base.Server.Auth.SessionSecret = strings.Repeat("s", 64)
	base.Server.Auth.AdminGroups = []string{"spindle-admin"}
	path := filepath.Join(dir, config.ConfigFileName)
	if _, _, err := setup.Save(path, base, base); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	cfg, _, err := config.Load([]string{path}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Import.AllowPrivateNetworks || cfg.Import.AllowFileURLs {
		t.Fatalf("fixture must start with the fail-closed policy: %+v", cfg.Import)
	}
	boot := server.BootState{Mode: "normal", FileState: "valid", ConfigPaths: []string{path}}
	return cfg, boot, config.NewLiveImport(cfg)
}

// TestAdminSavePublishesLiveImportPolicy is the unit acceptance for the defect:
// saving the policy on /admin/config changes the live policy at once (no
// restart) and reports NO restart-required import key.
func TestAdminSavePublishesLiveImportPolicy(t *testing.T) {
	cfg, boot, live := liveSaveFixture(t)
	save := adminSaveFn(cfg, boot, live)

	res, errs, err := save(context.Background(), map[string]string{
		"import.allow_private_networks": "true",
		"import.url_allowlist":          "github.com",
		"import.allow_file_urls":        "true",
	})
	if err != nil || len(errs) != 0 {
		t.Fatalf("save = %v %v", errs, err)
	}
	if !res.Saved {
		t.Fatal("save did not report Saved")
	}

	got, ok := live.Get()
	if !ok {
		t.Fatal("live holder unwired")
	}
	if !got.AllowPrivateNetworks || !got.AllowFileURLs {
		t.Fatalf("live policy = %+v, want the saved section", got)
	}
	if len(got.URLAllowlist) != 1 || got.URLAllowlist[0] != "github.com" {
		t.Fatalf("live allowlist = %v, want [github.com]", got.URLAllowlist)
	}
	// The running config itself is untouched (it is the boot snapshot by
	// contract, 11_config_cli.md §3.6).
	if cfg.Import.AllowPrivateNetworks {
		t.Fatal("the boot config must not be mutated")
	}

	if len(res.RequiresRestart) != 0 {
		t.Fatalf("RequiresRestart = %v, want none: the import policy is read live", res.RequiresRestart)
	}
	for _, key := range res.RequiresRestart {
		if strings.HasPrefix(key, "import.") {
			t.Fatalf("restart list names an import key: %v", res.RequiresRestart)
		}
	}
}

// TestAdminSaveLiveImportRespectsEnvironment: the env overlay wins at boot, so
// a file save must not publish a value the environment overrides — and the
// restart list stays empty for it (env wins; nothing to restart into).
func TestAdminSaveLiveImportRespectsEnvironment(t *testing.T) {
	t.Setenv("SPINDLE__IMPORT__ALLOW_PRIVATE_NETWORKS", "false")
	cfg, boot, live := liveSaveFixture(t)
	save := adminSaveFn(cfg, boot, live)

	res, errs, err := save(context.Background(), map[string]string{
		"import.allow_private_networks": "true",
		"import.allow_file_urls":        "true",
	})
	if err != nil || len(errs) != 0 {
		t.Fatalf("save = %v %v", errs, err)
	}
	got, _ := live.Get()
	if got.AllowPrivateNetworks {
		t.Fatal("the file save overrode an environment-supplied false")
	}
	if !got.AllowFileURLs {
		t.Fatalf("live policy = %+v, want the un-overridden key published", got)
	}
	// The file DOES carry what the operator typed (env is never baked in)…
	// checked through setup.BaseConfig, the file-visible base, because a
	// config.Load here would re-apply the very overlay under test.
	onDisk := setup.BaseConfig(cfg.DataDir, boot.ConfigPaths)
	if !onDisk.Import.AllowPrivateNetworks {
		t.Fatal("the saved file should carry the submitted true")
	}
	// …and the operator is not told to restart for a key the environment owns.
	for _, key := range res.RequiresRestart {
		if strings.HasPrefix(key, "import.") {
			t.Fatalf("restart list names %q, want none for the read-live policy", key)
		}
	}
}

// A save that changes a construction-snapshot key still reports it, so the
// filter is the read-live list and not "ignore import.*".
func TestAdminSaveRestartListKeepsSnapshotKeys(t *testing.T) {
	cfg, boot, live := liveSaveFixture(t)
	save := adminSaveFn(cfg, boot, live)
	res, errs, err := save(context.Background(), map[string]string{
		"import.allow_private_networks": "true",
		"import.max_concurrent":         "4",
	})
	if err != nil || len(errs) != 0 {
		t.Fatalf("save = %v %v", errs, err)
	}
	joined := strings.Join(res.RequiresRestart, ",")
	if !strings.Contains(joined, "import.max_concurrent") {
		t.Fatalf("restart list = %q, want import.max_concurrent", joined)
	}
	if strings.Contains(joined, "import.allow_private_networks") {
		t.Fatalf("restart list = %q, must not name the live key", joined)
	}
}

// An unwired holder must not panic on the save path (setup-only/test boots).
func TestAdminSaveUnwiredLiveImport(t *testing.T) {
	cfg, boot, _ := liveSaveFixture(t)
	save := adminSaveFn(cfg, boot, nil)
	if _, errs, err := save(context.Background(), map[string]string{"import.allow_private_networks": "true"}); err != nil || len(errs) != 0 {
		t.Fatalf("save with an unwired holder = %v %v", errs, err)
	}
}
