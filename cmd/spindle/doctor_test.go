// doctor_test.go — the command contract: exit codes, the stable --json shape,
// the config preflight (a removed key is a finding, not a crash) and a
// filesystem-store run that proves doctor works against a real backend.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/doctor"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/store/proto"
)

func validOIDCConfig(storeRoot, cacheDir string) string {
	return `
[server]
listen = "127.0.0.1:18080"

[server.auth]
issuer = "https://id.example.com"
allowed_domains = ["example.com"]
groups_claim = "groups"
admin_groups = ["spindle-admin"]
session_secret = "` + strings.Repeat("s", 32) + `"
oauth_client_id = "spindle"
oauth_client_secret = "secret"

[store]
backend = "filesystem"
root = "` + storeRoot + `"

[cache]
dir = "` + cacheDir + `"
`
}

// doctorEnv isolates the ladder from the developer's environment.
func doctorEnv(t *testing.T, dir string) *cli {
	t.Helper()
	for _, k := range []string{
		"SPINDLE_CONFIG", "WALGIT_CONFIG", "PORT",
		"SPINDLE__STORE__ROOT", "SPINDLE__STORE__BACKEND", "SPINDLE__CACHE__DIR",
	} {
		clearEnv(t, k)
	}
	t.Setenv("SPINDLE_DATA_DIR", dir)
	return &cli{dataDir: dir, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
}

func seedFilesystemStore(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.NewFilesystemRoot(root, 4)
	if err != nil {
		t.Fatal(err)
	}
	m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: "acme/app", ObjectFormat: "sha1", Revision: 1}
	if _, err := store.PutBytes(context.Background(), st, "repos/acme/app/manifest.pb", m.Marshal(), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorExitCodes(t *testing.T) {
	if got := doctorExitCode(doctor.NewReport(doctor.Options{})); got != exitOK {
		t.Fatalf("clean exit = %d, want %d", got, exitOK)
	}
	rep := doctor.NewReport(doctor.Options{})
	rep.Add(doctor.Finding{Check: doctor.CheckDecode, Key: "repos/a/b/x.json", Reason: "invalid"})
	if got := doctorExitCode(rep); got != exitErr {
		t.Fatalf("finding exit = %d, want %d", got, exitErr)
	}
	if exitOK == exitErr || exitOK == exitArg || exitErr == exitArg {
		t.Fatal("exit codes 0/1/2 must be distinct")
	}
}

func TestDoctorCleanFilesystemStore(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "store")
	seedFilesystemStore(t, root)
	cache := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "spindle.toml")
	if err := os.WriteFile(cfgPath, []byte(validOIDCConfig(root, cache)), 0o600); err != nil {
		t.Fatal(err)
	}
	c := doctorEnv(t, dir)
	c.configPath = cfgPath
	if code := runDoctor(context.Background(), c, []string{"--json"}); code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, c.errOut.(*bytes.Buffer).String())
	}
	var rep struct {
		Version  int              `json:"version"`
		Findings []doctor.Finding `json:"findings"`
		Summary  doctor.Summary   `json:"summary"`
	}
	if err := json.Unmarshal(c.out.(*bytes.Buffer).Bytes(), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, c.out.(*bytes.Buffer).String())
	}
	if len(rep.Findings) != 0 || !rep.Summary.Clean {
		t.Fatalf("findings = %+v summary = %+v, want clean", rep.Findings, rep.Summary)
	}
	if rep.Summary.ReposSampled != 1 {
		t.Fatalf("repos sampled = %d, want 1", rep.Summary.ReposSampled)
	}
}

func TestDoctorConfigRemovedKeyIsAFindingNotACrash(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "store")
	seedFilesystemStore(t, root)
	cache := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	// The 2026-11-22 shape: the old binary tolerated server.auth.mode, the
	// new one refuses it before it can boot.
	cfgPath := filepath.Join(dir, "spindle.toml")
	src := strings.Replace(validOIDCConfig(root, cache), "[server.auth]", "[server.auth]\nmode = \"none\"", 1)
	if err := os.WriteFile(cfgPath, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	c := doctorEnv(t, dir)
	c.configPath = cfgPath
	code := runDoctor(context.Background(), c, nil)
	if code != exitErr {
		t.Fatalf("exit = %d, want 1 (stderr=%s)", code, c.errOut.(*bytes.Buffer).String())
	}
	out := c.out.(*bytes.Buffer).String()
	if !strings.Contains(out, "server.auth.mode") {
		t.Fatalf("output does not name server.auth.mode:\n%s", out)
	}
	if !strings.Contains(out, doctor.CheckConfigKey) {
		t.Fatalf("output does not carry the %s check:\n%s", doctor.CheckConfigKey, out)
	}
}

func TestDoctorMissingExplicitConfigIsExit2(t *testing.T) {
	dir := t.TempDir()
	c := doctorEnv(t, dir)
	c.configPath = filepath.Join(dir, "nope.toml")
	if code := runDoctor(context.Background(), c, nil); code != exitArg {
		t.Fatalf("exit = %d, want %d (usage/IO)", code, exitArg)
	}
}

func TestDoctorUnreadableConfigIsExit2(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions (the CI gate runs unprivileged)")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "spindle.toml")
	if err := os.WriteFile(path, []byte("[server]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	c := doctorEnv(t, dir)
	c.configPath = path
	if code := runDoctor(context.Background(), c, nil); code != exitArg {
		t.Fatalf("exit = %d, want %d (IO failure)", code, exitArg)
	}
}

func TestDoctorZeroConfigReportsAuthFindings(t *testing.T) {
	for _, k := range []string{
		"SPINDLE__SERVER__AUTH__ISSUER", "SPINDLE__SERVER__AUTH__ALLOWED_DOMAINS",
		"SPINDLE__SERVER__AUTH__OAUTH_CLIENT_ID", "SPINDLE__SERVER__AUTH__OAUTH_CLIENT_SECRET",
		"SPINDLE__SERVER__AUTH__SESSION_SECRET",
	} {
		clearEnv(t, k)
	}
	dir := t.TempDir()
	c := doctorEnv(t, dir)
	code := runDoctor(context.Background(), c, nil)
	if code != exitErr {
		t.Fatalf("exit = %d, want 1 (a zero-config host is restricted); stderr=%s", code, c.errOut.(*bytes.Buffer).String())
	}
	out := c.out.(*bytes.Buffer).String()
	if !strings.Contains(out, "server.auth.issuer") {
		t.Fatalf("output does not name server.auth.issuer:\n%s", out)
	}
}

func TestDoctorMissingStoreRootIsExit2(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "absent-store") // deliberately not created
	cache := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "spindle.toml")
	if err := os.WriteFile(cfgPath, []byte(validOIDCConfig(root, cache)), 0o600); err != nil {
		t.Fatal(err)
	}
	c := doctorEnv(t, dir)
	c.configPath = cfgPath
	if code := runDoctor(context.Background(), c, nil); code != exitArg {
		t.Fatalf("exit = %d, want %d (IO failure); stderr=%s", code, exitArg, c.errOut.(*bytes.Buffer).String())
	}
}
