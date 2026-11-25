// save_target_test.go — the save target is "the file the process would load, or
// the file it was told to load" (config.SaveTargetPath / SaveSetupTo), not an
// unconditional <data-dir>/spindle.toml. The probe from the field: with
// --config /etc/spindle.toml the save wrote <data-dir>/spindle.toml and the
// restart reloaded the old values.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSaveTargetPathRule pins the truth table: an existing candidate wins; else
// a NAMED (explicit) file is created where the operator said; else the data-dir
// first-run default. `/dev/null` is never a target.
func TestSaveTargetPathRule(t *testing.T) {
	dataDir := "/data"
	missing := filepath.Join(t.TempDir(), "a.toml")
	existing := filepath.Join(t.TempDir(), "b.toml")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingExplicit := filepath.Join(t.TempDir(), "etc", "spindle.toml")
	missingAlias := filepath.Join(t.TempDir(), "walgit.toml")
	cases := []struct {
		name       string
		candidates []string
		explicit   bool
		want       string
	}{
		{"no candidates", nil, false, "/data/spindle.toml"},
		{"empty entries skipped", []string{"", ""}, false, "/data/spindle.toml"},
		// The data-dir ladder is a SEARCH: nothing found means the first run,
		// whose save belongs in the data dir.
		{"default ladder, none exist", []string{missing, existing + ".nope"}, false, "/data/spindle.toml"},
		// An explicit --config/pointer file is not a search: with no file on
		// disk it is still the file the process was told to load, so the save
		// CREATES it (the fixed gap: this used to write the data-dir shadow).
		{"explicit missing path is created", []string{missingExplicit}, true, missingExplicit},
		{"explicit missing alias is created", []string{missingAlias}, true, missingAlias},
		{"explicit picks the first missing candidate", []string{missingExplicit, missingAlias}, true, missingExplicit},
		{"explicit with no candidates falls back", nil, true, "/data/spindle.toml"},
		{"explicit with only empty entries falls back", []string{"", ""}, true, "/data/spindle.toml"},
		// An existing candidate always wins, explicit or not.
		{"first existing wins", []string{missing, existing}, false, existing},
		{"alias existing", []string{missing, existing}, false, existing},
		{"explicit existing wins", []string{existing}, true, existing},
		// `--config /dev/null` is defaults+env, never a device to write.
		{"dev null is never the target", []string{DevNullConfigPath}, true, "/data/spindle.toml"},
		{"dev null falls through to the existing file", []string{DevNullConfigPath, existing}, true, existing},
		{"dev null with data-dir ladder", []string{DevNullConfigPath, missing}, false, "/data/spindle.toml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SaveTargetPath(dataDir, tc.candidates, tc.explicit); got != tc.want {
				t.Fatalf("SaveTargetPath = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSaveSetupToWritesTheGivenPath: the atomic write, the backup and the
// byte-identical no-op all follow the TARGET, not a data dir.
func TestSaveSetupToWritesTheGivenPath(t *testing.T) {
	dataDir := tmpDataDir(t)
	explicitDir := t.TempDir()
	explicit := filepath.Join(explicitDir, "operator.toml")
	if err := os.WriteFile(explicit, []byte("# operator note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := validOIDC()
	c.DataDir = dataDir

	backup, err := SaveSetupTo(explicit, c, c)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if backup == "" || !strings.HasPrefix(backup, "operator.toml.bak-") {
		t.Fatalf("backup = %q, want operator.toml.bak-<utc>", backup)
	}
	if _, err := os.Stat(filepath.Join(explicitDir, backup)); err != nil {
		t.Fatalf("backup not beside the target: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, ConfigFileName)); !os.IsNotExist(err) {
		t.Fatal("a data-dir copy was written")
	}
	if _, err := os.Stat(explicit + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp file left behind: %v", err)
	}
	body, err := os.ReadFile(explicit)
	if err != nil || strings.Contains(string(body), "operator note") {
		t.Fatalf("the canonical rewrite is wrong (err=%v):\n%s", err, body)
	}

	// A second save of the same values writes nothing and keeps the one backup.
	again, err := SaveSetupTo(explicit, c, c)
	if err != nil || again != "" {
		t.Fatalf("no-op save = %q %v", again, err)
	}
	backups, _ := filepath.Glob(explicit + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one", backups)
	}

	// The base the page edits reads the target back.
	reloaded, lerr := LoadSetupBase(dataDir, []string{explicit})
	if lerr != nil {
		t.Fatalf("reload: %v", lerr)
	}
	if reloaded.Server.Auth.Issuer != c.Server.Auth.Issuer {
		t.Fatalf("reload = %+v", reloaded.Server.Auth)
	}
}

// TestSaveCreatesExplicitMissingFile: the create path of the rule actually
// works — the write makes the file and its directory, the loader reads it, and
// no data-dir shadow appears.
func TestSaveCreatesExplicitMissingFile(t *testing.T) {
	dataDir := tmpDataDir(t)
	explicit := filepath.Join(t.TempDir(), "etc", "spindle", "spindle.toml")
	c := validOIDC()
	c.DataDir = dataDir

	target := SaveTargetPath(dataDir, []string{explicit}, true)
	if target != explicit {
		t.Fatalf("target = %q, want the named file", target)
	}
	if _, err := SaveSetupTo(target, c, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	reloaded, _, err := LoadExplicit(explicit, func(string) string { return "" })
	if err != nil {
		t.Fatalf("the created file must load: %v", err)
	}
	if reloaded.Server.Auth.Issuer != c.Server.Auth.Issuer {
		t.Fatalf("reloaded = %+v", reloaded.Server.Auth)
	}
	if _, err := os.Stat(filepath.Join(dataDir, ConfigFileName)); !os.IsNotExist(err) {
		t.Fatal("a data-dir shadow file was created")
	}
}
