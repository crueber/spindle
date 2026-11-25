// livepolicy_test.go — the import gate reads the LIVE [import] policy, not the
// boot snapshot (config.LiveImport): the admin save publishes, the next
// ParseRequest sees it, and an unwired holder keeps the startup copy.
package repoimport

import (
	"context"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/wal"
)

// ParseRequest is the API/page path: same body, same startup config, only the
// live holder changes between the two calls.
func TestParseRequestReadsLivePolicy(t *testing.T) {
	cfg := testConfig(t)
	cfg.Import.AllowFileURLs = false
	cfg.Import.AllowPrivateNetworks = false
	cfg.Import.URLAllowlist = nil
	live := config.NewLiveImport(cfg)

	body := []byte(`{"source_url":"file:///srv/src.git","owner":"acme","name":"live","dangerous":true}`)
	if _, _, err := ParseRequest(body, cfg, live); err == nil || !strings.Contains(err.Error(), "import.allow_file_urls=false") {
		t.Fatalf("pre-save gate = %v, want the file:// refusal", err)
	}

	// The save publishes a policy with file:// allowed — cfg is untouched.
	imp := config.ImportPolicy(live, nil)
	imp.AllowFileURLs = true
	live.Set(imp)

	if _, _, err := ParseRequest(body, cfg, live); err != nil {
		t.Fatalf("post-save gate refused: %v", err)
	}
	// The startup snapshot still refuses (proof the gate read the holder).
	if _, _, err := ParseRequest(body, cfg, nil); err == nil {
		t.Fatal("the unwired holder must keep the startup policy")
	}
}

// The reported defect verbatim: a private/loopback source with the policy
// saved live. No DNS stub is needed — 127.0.0.1 resolves locally and is
// rejected before any dial (the gate's own predicate).
func TestParseRequestLiveAllowPrivate(t *testing.T) {
	cfg := testConfig(t)
	cfg.Import.AllowPrivateNetworks = false
	cfg.Import.URLAllowlist = nil
	live := config.NewLiveImport(cfg)

	body := []byte(`{"source_url":"http://127.0.0.1/src.git","owner":"acme","name":"priv","dangerous":true}`)
	_, _, err := ParseRequest(body, cfg, live)
	if err == nil || !strings.Contains(err.Error(), `source host "127.0.0.1" resolves to a private address (import.allow_private_networks=false)`) {
		t.Fatalf("pre-save gate = %v, want the reported sentence", err)
	}

	imp := config.ImportPolicy(live, nil)
	imp.AllowPrivateNetworks = true
	live.Set(imp)
	if _, _, err := ParseRequest(body, cfg, live); err != nil {
		t.Fatalf("post-save gate refused: %v", err)
	}
}

// An allowlist saved live is the same one policy: the host is refused before,
// accepted after, and the dangerous flag still governs an empty allowlist.
func TestParseRequestLiveAllowlist(t *testing.T) {
	cfg := testConfig(t)
	cfg.Import.AllowPrivateNetworks = true
	cfg.Import.URLAllowlist = []string{"other.example"}
	live := config.NewLiveImport(cfg)

	body := []byte(`{"source_url":"https://github.com/crueber/spindle.git","owner":"acme","name":"al"}`)
	if _, _, err := ParseRequest(body, cfg, live); err == nil || !strings.Contains(err.Error(), "not in import.url_allowlist") {
		t.Fatalf("pre-save gate = %v, want the allowlist refusal", err)
	}
	imp := config.ImportPolicy(live, nil)
	imp.URLAllowlist = []string{"github.com"}
	live.Set(imp)
	if _, _, err := ParseRequest(body, cfg, live); err != nil {
		t.Fatalf("post-save gate refused: %v", err)
	}
}

// SSRFPolicy is the one mapping from the section to the gate; it must cover
// every policy key (a new key that skips it would silently stay enforced).
func TestSSRFPolicyMapsEveryKey(t *testing.T) {
	imp := config.Import{
		AllowPrivateNetworks: true,
		URLAllowlist:         []string{"a.example"},
		AllowFileURLs:        true,
	}
	got := SSRFPolicy(imp, true)
	if !got.AllowPrivate || !got.AllowFile || !got.Dangerous ||
		len(got.Allowlist) != 1 || got.Allowlist[0] != "a.example" {
		t.Fatalf("SSRFPolicy = %+v", got)
	}
}

// The JSON API handler is the second read point: it gates through the service's
// live holder, so the same POST is refused before the save and accepted after.
func TestHandlerGateReadsLivePolicy(t *testing.T) {
	cfg := testConfig(t)
	cfg.Import.AllowFileURLs = false
	live := config.NewLiveImport(cfg)
	st := store.NewMemory()
	reg := wal.NewRegistry(context.Background(), st, cfg)
	t.Cleanup(reg.Close)
	svc := New(Deps{Store: st, Reg: reg, Roles: &FakeRoles{}, Cfg: cfg, Hostname: "testhost", LiveImport: live})
	h := testHandler(svc, adminPrincipal())
	body := `{"source_url":"file:///nonexistent/import-live.git","owner":"acme","name":"live"}`

	w := doPost(t, h, "/api/v1/repos/imports", body, "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "import.allow_file_urls=false") {
		t.Fatalf("pre-save = %d %q, want the file:// refusal", w.Code, w.Body.String())
	}

	imp := config.ImportPolicy(live, nil)
	imp.AllowFileURLs = true
	live.Set(imp)
	w = doPost(t, h, "/api/v1/repos/imports", body, "")
	if w.Code != 202 {
		t.Fatalf("post-save = %d %q, want the gate to accept it", w.Code, w.Body.String())
	}
	// The clone of the missing source fails; let the task leave the running
	// map rather than racing the test's TempDirs.
	awaitStarted237(t, svc, w.Body.Bytes())
}
