package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FirstRunDefaults returns the zero-config boot configuration (divergence D5,
// 11_config_cli.md §2.3, as amended by PLAN-OIDC-ONLY): a fresh machine gets
// the first-run store/listen shape and NO authentication — which fails
// validation, so the boot is restricted until OIDC is configured.
// Every unmarked key keeps its compiled-in §2 table value.
func FirstRunDefaults(dataDir string) *Config {
	c := Defaults()
	c.DataDir = dataDir
	c.Server.Listen = "0.0.0.0:8080"
	c.Server.AutoCreateOnPush = true
	c.Store.Backend = "filesystem"
	c.Store.Root = filepath.Join(dataDir, "store")
	c.Cache.Dir = filepath.Join(dataDir, "cache")
	return c
}

// LoadSetupBase returns the file-visible base config that setup edits on top
// of (§3.4): compiled-in defaults ⊕ the first existing candidate file — no env
// overlay, no PORT lockstep (those stay env-side and are never baked into the
// file). Candidates are checked in priority order (an explicit --config path
// first, then the data-dir files, mirroring the §3.1 ladder). With no
// candidate present the zero-config first-run shape is the base, so a first
// save cannot silently swap the filesystem store for a bucket backend or move
// the listener off 0.0.0.0. An unparseable file returns the error; the caller
// falls back to FirstRunDefaults (the file's own errors surface separately
// through the boot state).
func LoadSetupBase(dataDir string, candidates []string) (*Config, error) {
	c := Defaults()
	loaded := false
	for _, path := range candidates {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if err := decodeFile(path, c); err != nil {
			return nil, err
		}
		loaded = true
		break
	}
	if !loaded {
		c = FirstRunDefaults(dataDir)
	}
	c.DataDir = dataDir
	return c, nil
}

// ResolveDataDir resolves the data dir (11_config_cli.md §3.1.1): the
// SPINDLE_DATA_DIR env var, else /var/lib/spindle in a container context
// (/.dockerenv present or KUBERNETES_SERVICE_HOST set), else the XDG default
// ~/.local/share/spindle.
func ResolveDataDir(getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if d := getenv("SPINDLE_DATA_DIR"); d != "" {
		return d
	}
	if IsContainer() {
		return "/var/lib/spindle"
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "/var/lib/spindle"
	}
	return filepath.Join(home, ".local", "share", "spindle")
}

// IsContainer reports whether the process appears to run in a container.
func IsContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return os.Getenv("KUBERNETES_SERVICE_HOST") != ""
}

// EnsureDataDir creates the data dir (mode 0700) if missing — §3.1.1.
func EnsureDataDir(dataDir string) error {
	return os.MkdirAll(dataDir, 0o700)
}

// --- Setup save semantics (§3.7, divergence D6) ---

// readLiveKeys are picked up on the next tick/emit without a restart; every
// other key is consumed once at subsystem construction and needs a restart.
var readLiveKeys = []string{
	"telemetry.log_format",
	"telemetry.log_filter",
	"maintenance.interval",
	"maintenance.follow_interval",
	"wal.freshness_ttl",
}

// RestartRequired reports whether a key takes effect only after a restart
// (the default) or is read live from the effective config.
func RestartRequired(key string) bool {
	for _, k := range readLiveKeys {
		if k == strings.ToLower(key) {
			return false
		}
	}
	return true
}

// ValidateForSave runs the full §5 fail-closed validation; invalid input
// writes nothing.
func ValidateForSave(c *Config) error {
	_, errs := Validate(c)
	if len(errs) == 0 {
		return nil
	}
	msgs := make([]string, len(errs))
	for i, err := range errs {
		msgs[i] = err.Error()
	}
	return errors.New(strings.Join(msgs, "; "))
}

// SaveSetup validates c (§5), then atomically serializes it to
// <data-dir>/spindle.toml: write spindle.toml.tmp (mode 0600), fsync, rename.
// Readers see either the old or the new file, never a partial one.
//
// The setup editor writes the FULL canonical document (Toml can be decoded but
// its comments are not preserved by the codec), so the first rewrite of an
// existing file keeps the operator's text: the previous bytes are copied to
// spindle.toml.bak-<utc> beside the config before the rewrite. The backup is
// made ONCE — a later save sees it already present — and an unchanged save
// writes nothing at all (byte-identical), so comments an operator keeps in a
// canonical file only go on the first canonicalising save. The returned string
// is that backup's file name ("" when none was needed).
func SaveSetup(c *Config, dataDir string) (string, error) {
	return SaveSetupValidated(c, c, dataDir)
}

// SaveSetupValidated writes write but validates validate. The admin page uses
// it when a value is EFFECTIVE but not in the file (environment or flags): the
// document on disk keeps the file's bytes (the env secret is not silently
// persisted), while the check is run on the configuration that will actually
// take effect. A file that relies on the environment stays exactly that; an
// invalid effective config is still refused.
func SaveSetupValidated(write, validate *Config, dataDir string) (string, error) {
	if err := ValidateForSave(validate); err != nil {
		return "", err
	}
	c := write
	if err := EnsureDataDir(dataDir); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	body, err := MarshalTOML(c)
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	path := filepath.Join(dataDir, ConfigFileName)
	backup, rewrite, err := backupBeforeRewrite(path, body)
	if err != nil {
		return "", err
	}
	if !rewrite {
		return "", nil // byte-identical: nothing to write, nothing to keep
	}
	tmpPath := filepath.Join(dataDir, "spindle.toml.tmp")
	if err := writeFileSync(tmpPath, body); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("rename config into place: %w", err)
	}
	return backup, nil
}

// backupBeforeRewrite copies an existing config's bytes to
// spindle.toml.bak-<utc> when the document about to be written differs and no
// backup exists yet. rewrite is false when the file already equals body (a
// no-op save must not touch the file at all). It returns the backup's base
// name, "" when there is nothing to back up (no file, an empty file, or a
// backup already exists).
func backupBeforeRewrite(path string, body []byte) (name string, rewrite bool, err error) {
	cur, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", true, nil
		}
		// A directory where the config belongs holds no operator text; let the
		// write/rename below report the real failure. Any other read error
		// fails closed: rewriting a file we could not read would destroy it
		// without a backup.
		if fi, serr := os.Stat(path); serr == nil && fi.IsDir() {
			return "", true, nil
		}
		return "", false, fmt.Errorf("read config: %w", err)
	}
	if bytes.Equal(cur, body) {
		return "", false, nil
	}
	if len(cur) == 0 {
		return "", true, nil
	}
	if prior, _ := filepath.Glob(path + ".bak-*"); len(prior) > 0 {
		return "", true, nil // the operator's original text is already preserved
	}
	name = ConfigFileName + ".bak-" + time.Now().UTC().Format("20060102T150405Z")
	backupPath := filepath.Join(filepath.Dir(path), name)
	if err := writeFileSync(backupPath, cur); err != nil {
		return "", false, fmt.Errorf("write config backup: %w", err)
	}
	return name, true, nil
}

// writeFileSync writes body with mode 0600 and fsyncs before returning.
func writeFileSync(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(body)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
