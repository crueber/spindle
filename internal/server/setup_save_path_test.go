// setup_save_path_test.go — PUT /api/v1/setup writes the file the process
// LOADED. With an explicit config path (--config / SPINDLE_CONFIG /
// WALGIT_CONFIG) the old code wrote <data-dir>/spindle.toml, a shadow file the
// loader never reads: the save looked successful and the next restart showed
// the old values.
package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
)

func TestSetupPutWritesLoadedExplicitFile(t *testing.T) {
	dataDir := t.TempDir()
	// An operator-managed file outside the data dir, the --config shape.
	explicitDir := filepath.Join(t.TempDir(), "etc")
	if err := os.MkdirAll(explicitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(explicitDir, "spindle.toml")
	seeded := config.FirstRunDefaults(dataDir) // valid document (OIDC included)
	seeded.Server.Auth.Issuer = "https://id.example.com"
	seeded.Server.Auth.AllowedDomains = []string{"example.com"}
	seeded.Server.Auth.OAuthClientID = "spindle"
	seeded.Server.Auth.OAuthClientSecret = "s3cret"
	seeded.Server.Auth.SessionSecret = "0123456789abcdef0123456789abcdef"
	seeded.Server.Auth.AdminGroups = []string{"spindle-admin"}
	body, merr := config.MarshalTOML(seeded)
	if merr != nil {
		t.Fatal(merr)
	}
	if err := os.WriteFile(explicit, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, lerr := config.Load([]string{explicit}, func(string) string { return "" })
	if lerr != nil {
		t.Fatalf("load explicit: %v", lerr)
	}

	live := config.NewLiveImport(cfg)
	s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		DataDir: dataDir, LiveImport: live,
		Boot: BootState{Mode: "normal", FileState: "valid", ConfigPaths: []string{explicit}}})
	installTestAdminHook(s)

	req := httptest.NewRequest("PUT", "http://x/api/v1/setup", strings.NewReader(
		`{"overrides": {"import.allow_private_networks": "true", "import.url_allowlist": "github.com"}}`))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.setupPut(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"path":"`+explicit+`"`) {
		t.Fatalf("response does not name the written file: %s", rec.Body.String())
	}
	// The import policy is live and the restart list is empty for it.
	if !strings.Contains(rec.Body.String(), `"requires_restart":[]`) {
		t.Fatalf("requires_restart = %s, want the read-live policy omitted", rec.Body.String())
	}
	if got, _ := live.Get(); !got.AllowPrivateNetworks {
		t.Fatalf("live policy = %+v, want the saved section", got)
	}

	saved, err := os.ReadFile(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "allow_private_networks = true") {
		t.Fatalf("the loaded file does not carry the save:\n%s", saved)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, config.ConfigFileName)); !os.IsNotExist(statErr) {
		t.Fatal("the API save created a data-dir shadow file")
	}
}
