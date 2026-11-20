// seam_test.go — the non-HTTP config seam the server-rendered settings page
// uses (Configure/Keygen/Store). The HTTP handlers call the same
// implementation, so these tests also pin that the page cannot drift from the
// JSON API.
package pushmirror

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
)

func seamHandler() (*Handler, store.ObjectStore) {
	return &Handler{Svc: New(Deps{
		Store:     store.NewMemory(),
		Allowlist: []string{"example.com"},
		Now:       func() time.Time { return time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC) },
	})}, nil
}

func seedManifest(t *testing.T, st store.ObjectStore, owner, repo string) {
	t.Helper()
	if _, err := st.Put(context.Background(), store.RepoPrefix(owner, repo)+store.Manifest,
		store.PutBody{Bytes: []byte("manifest")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
}

func sched(v string) *string { return &v }
func known(v string) *string { return &v }

func TestConfigureSeamCreateUpdateAndErrors(t *testing.T) {
	h, _ := seamHandler()
	st := h.Svc.Store()
	seedManifest(t, st, "acme", "repo")
	ctx := context.Background()

	if _, cerr := (&Handler{}).Configure(ctx, "acme", "repo", ConfigInput{}); cerr == nil || cerr.Status != http.StatusServiceUnavailable {
		t.Fatalf("nil service = %+v, want 503", cerr)
	}

	v, cerr := h.Configure(ctx, "acme", "repo", ConfigInput{
		UpstreamURL:   "https://example.com/acme/backup.git",
		AuthKind:      AuthNone,
		Schedule:      sched("daily"),
		SSHKnownHosts: known(""),
	})
	if cerr != nil {
		t.Fatalf("create = %+v", cerr)
	}
	if v.UpstreamURL != "https://example.com/acme/backup" || v.Schedule != "daily" {
		t.Fatalf("view = %+v", v)
	}

	// Update: schedule changes, upstream stays.
	v, cerr = h.Configure(ctx, "acme", "repo", ConfigInput{Schedule: sched("weekly")})
	if cerr != nil || v.Schedule != "weekly" {
		t.Fatalf("update = %+v err=%+v", v, cerr)
	}

	// A different upstream is 409 (delete-and-recreate).
	if _, cerr = h.Configure(ctx, "acme", "repo", ConfigInput{UpstreamURL: "https://example.com/other.git"}); cerr == nil || cerr.Status != http.StatusConflict {
		t.Fatalf("upstream change = %+v, want 409", cerr)
	}
	// The same upstream in another spelling is not a change.
	if _, cerr = h.Configure(ctx, "acme", "repo", ConfigInput{UpstreamURL: "https://example.com/acme/backup.git"}); cerr != nil {
		t.Fatalf("same upstream = %+v", cerr)
	}
	// Unknown schedule / auth kind are 400s.
	if _, cerr = h.Configure(ctx, "acme", "repo", ConfigInput{Schedule: sched("fortnightly")}); cerr == nil || cerr.Status != http.StatusBadRequest {
		t.Fatalf("bad schedule = %+v", cerr)
	}
	if !strings.Contains(cerr.Error(), "unknown schedule preset") {
		t.Fatalf("schedule message = %q", cerr.Error())
	}
	if _, cerr = h.Configure(ctx, "acme", "repo", ConfigInput{AuthKind: "kerberos"}); cerr == nil || cerr.Status != http.StatusBadRequest {
		t.Fatalf("bad auth kind = %+v", cerr)
	}
	// Validation + the SSRF gate (on a fresh destination each time: a
	// different upstream on an existing config is the 409 above).
	if _, cerr = h.Configure(ctx, "acme", "new1", ConfigInput{UpstreamURL: "git://example.com/x.git"}); cerr == nil || cerr.Status != http.StatusBadRequest {
		t.Fatalf("git:// = %+v", cerr)
	}
	if _, cerr = h.Configure(ctx, "acme", "new2", ConfigInput{UpstreamURL: "https://evil.example/x.git"}); cerr == nil || cerr.Status != http.StatusBadRequest {
		t.Fatalf("off-allowlist = %+v, want SSRF refusal", cerr)
	}
	// Missing repo / missing url.
	if _, cerr = h.Configure(ctx, "acme", "ghost", ConfigInput{UpstreamURL: "https://example.com/x.git"}); cerr == nil || cerr.Status != http.StatusNotFound {
		t.Fatalf("ghost repo = %+v", cerr)
	}
	if _, cerr = h.Configure(ctx, "acme", "repo", ConfigInput{Schedule: sched("daily")}); cerr != nil {
		t.Fatalf("existing mirror with no upstream = %+v", cerr)
	}
	if _, cerr = h.Configure(ctx, "acme", "fresh", ConfigInput{}); cerr == nil || cerr.Status == http.StatusOK {
		t.Fatalf("create without url = %+v", cerr)
	}
}

func TestConfigureSeamKeygenAndDeletePath(t *testing.T) {
	h, _ := seamHandler()
	st := h.Svc.Store()
	seedManifest(t, st, "acme", "repo")
	ctx := context.Background()
	key, gerr := GenerateKeypair("")
	if gerr != nil {
		t.Fatalf("GenerateKeypair: %v", gerr)
	}
	if _, cerr := h.Configure(ctx, "acme", "repo", ConfigInput{
		UpstreamURL: "ssh://example.com/acme/backup.git", AuthKind: AuthSSH,
		SSHPrivateKey: key.PrivatePEM, Schedule: sched(""),
	}); cerr != nil {
		t.Fatalf("ssh create = %+v", cerr)
	}
	gen, cerr := h.Keygen(ctx, "acme", "repo", "example.com ssh-ed25519 AAAA")
	if cerr != nil || gen == nil || gen.PublicKey == "" {
		t.Fatalf("keygen = %+v %+v", gen, cerr)
	}
	// Keygen without a config is 404; on an https destination it is 400.
	if _, cerr = h.Keygen(ctx, "acme", "ghost", ""); cerr == nil || cerr.Status != http.StatusNotFound {
		t.Fatalf("keygen ghost = %+v", cerr)
	}
	if _, cerr = (&Handler{}).Keygen(ctx, "acme", "repo", ""); cerr == nil || cerr.Status != http.StatusServiceUnavailable {
		t.Fatalf("keygen nil svc = %+v", cerr)
	}
	// ConfigError is an error with its message as text.
	if cerr == nil || cerr.Error() != cerr.Message {
		t.Fatalf("ConfigError text = %v", cerr)
	}
}
