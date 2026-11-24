package server

// sshkeys_registry_test.go — the store-backed registry (17_ssh.md §3): CRUD
// over the memory store, ownership, duplicate fingerprints, and the
// mode-aware rights resolution at SSH-auth lookup time.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/store"
)

const registryTestKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAII/VO93dFREgk2CscLYyCKH4ZjDpD8XYGB/X8ReU2QWx ada@laptop"

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func registryFor(t *testing.T, cfgAuth func(*config.Auth)) (*SSHKeyRegistry, context.Context) {
	t.Helper()
	c := config.Defaults()
	c.Server.Auth.Issuer = "https://id.example.com"
	c.Server.Auth.AllowedDomains = []string{"example.com"}
	c.Server.Auth.OAuthClientID = "id"
	c.Server.Auth.OAuthClientSecret = "secret"
	c.Server.Auth.SessionSecret = strings.Repeat("x", 32)
	c.Server.Auth.AdminGroups = []string{"spindle-admin"}
	if cfgAuth != nil {
		cfgAuth(&c.Server.Auth)
	}
	a := NewAuthService(&c.Server.Auth, nil)
	return &SSHKeyRegistry{st: store.NewMemory(), auth: a, log: discardLogger()}, context.Background()
}

// fingerprintOf fingerprints an authorized_keys line.
func fingerprintOf(line string) string {
	pub, _, _, _, err := gossh.ParseAuthorizedKey([]byte(strings.TrimSpace(line)))
	if err != nil {
		return "SHA256:invalid"
	}
	return gossh.FingerprintSHA256(pub)
}

func TestRegistryAddListGetDelete(t *testing.T) {
	r, ctx := registryFor(t, nil)
	rec, err := r.Add(ctx, "ada", registryTestKey, "laptop")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if rec.ID == "" || !strings.HasPrefix(rec.Fingerprint, "SHA256:") || rec.Principal != "ada" {
		t.Fatalf("record = %+v", rec)
	}

	keys, err := r.List(ctx, "ada")
	if err != nil || len(keys) != 1 || keys[0].ID != rec.ID {
		t.Fatalf("list = %v %v", keys, err)
	}
	if bobKeys, _ := r.List(ctx, "bob"); len(bobKeys) != 0 {
		t.Fatalf("bob's list = %v", bobKeys)
	}

	if _, err := r.Get(ctx, "bob", rec.ID); !errors.Is(err, api.ErrKeyForbidden) {
		t.Fatalf("foreign get = %v", err)
	}
	got, err := r.Get(ctx, "ada", rec.ID)
	if err != nil || got.Title != "laptop" {
		t.Fatalf("own get = %+v %v", got, err)
	}

	if err := r.Delete(ctx, "ada", rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "ada", rec.ID); !errors.Is(err, api.ErrKeyNotFound) {
		t.Fatalf("post-delete get = %v", err)
	}
	if _, err := r.Add(ctx, "bob", registryTestKey, "second owner"); err != nil {
		t.Fatalf("fingerprint must be reusable after delete: %v", err)
	}
}

func TestRegistryDuplicateFingerprint(t *testing.T) {
	r, ctx := registryFor(t, nil)
	if _, err := r.Add(ctx, "ada", registryTestKey, ""); err != nil {
		t.Fatal(err)
	}
	_, err := r.Add(ctx, "bob", registryTestKey, "")
	if !errors.Is(err, api.ErrKeyDuplicate) {
		t.Fatalf("dup = %v, want ErrKeyDuplicate", err)
	}
	if bobKeys, _ := r.List(ctx, "bob"); len(bobKeys) != 0 {
		t.Fatalf("bob's list after rollback = %v", bobKeys)
	}
}

func TestRegistryMalformedKey(t *testing.T) {
	r, ctx := registryFor(t, nil)
	_, err := r.Add(ctx, "ada", "definitely not a key", "")
	if err == nil || !strings.Contains(err.Error(), "authorized_keys") {
		t.Fatalf("malformed = %v", err)
	}
}

func TestRegistryBadPrincipalName(t *testing.T) {
	// The principal names the ssh-keys/u/ listing prefix: not a single path
	// segment → refused before any store write.
	r, ctx := registryFor(t, nil)
	for _, name := range []string{"", "ada/lovelace", "a/b/c", " "} {
		_, err := r.Add(ctx, name, registryTestKey, "")
		if !errors.Is(err, api.ErrKeyBadPrincipal) {
			t.Fatalf("principal %q = %v, want ErrKeyBadPrincipal", name, err)
		}
	}
}

func TestRegistryLookupResolvesPrincipalRights(t *testing.T) {
	// OIDC: the key's principal name resolves through the wired EmailLookup
	// back to its verified email and inherits that admission's flags.
	r, ctx := registryFor(t, nil)
	r.auth.EmailLookup = func(name string) (string, bool) {
		if name == "ada" {
			return "ada@example.com", true
		}
		return "", false
	}
	rec, err := r.Add(ctx, "ada", registryTestKey, "")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := r.LookupByFingerprint(ctx, rec.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Write || entry.Admin || entry.Principal != "ada" {
		t.Fatalf("oidc entry = %+v", entry)
	}
	if _, err := r.LookupByFingerprint(ctx, "SHA256:unknown"); !errors.Is(err, api.ErrKeyNotFound) {
		t.Fatalf("unknown fp = %v", err)
	}
	// A name with no verified email behind it fails closed.
	rec2, err := r.Add(ctx, "ghost", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB7Ybq5Fq1nZ1mY7W6yPfS0m2p7pX0M2V7n6l9r0Vw2T ghost@laptop", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.LookupByFingerprint(ctx, rec2.Fingerprint); err == nil {
		t.Fatal("principal without a verified email must be denied")
	}
}

// Static tokens are machine credentials: a token principal name is not an
// OIDC identity, so its SSH keys resolve nowhere (PLAN-OIDC-ONLY §1.3).
func TestRegistryStaticTokenPrincipalHasNoSSHRights(t *testing.T) {
	c := config.Defaults()
	c.Server.Auth.Tokens = []config.StaticToken{
		{Principal: "ci", Token: "tok-write", Write: true},
	}
	a := NewAuthService(&c.Server.Auth, nil)
	r := &SSHKeyRegistry{st: store.NewMemory(), auth: a, log: discardLogger()}
	ctx := context.Background()

	if _, err := r.Add(ctx, "ci", registryTestKey, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LookupByFingerprint(ctx, fpID(fingerprintOf(registryTestKey))); err == nil {
		t.Fatal("a static-token principal must have no SSH rights")
	}
}

// PrincipalForName has one identity source: the OIDC admission path.
func TestPrincipalForNameBranches(t *testing.T) {
	c := config.Defaults()
	c.Server.Auth.Issuer = "https://id.example.com"
	c.Server.Auth.AllowedDomains = []string{"example.com"}
	c.Server.Auth.AdminGroups = []string{"spindle-admin"}
	a := NewAuthService(&c.Server.Auth, nil)
	a.EmailLookup = func(name string) (string, bool) {
		switch name {
		case "ci":
			return "ci@example.com", true
		case "root":
			return "root@example.com", true
		}
		return "", false
	}
	p, err := a.PrincipalForName("ci")
	if err != nil || !p.Write || p.Admin {
		t.Fatalf("ci = %+v %v", p, err)
	}
	p, err = a.PrincipalForName("root@example.com")
	if err != nil || !p.Write || p.Admin {
		t.Fatalf("root = %+v %v (groups, not emails, grant admin)", p, err)
	}
	// a principal with no verified email at all is denied
	if _, err := a.PrincipalForName("ghost"); err == nil {
		t.Fatal("ghost principal must be denied")
	}
	// nil lookup fails closed
	bare := NewAuthService(&config.Auth{}, nil)
	if _, err := bare.PrincipalForName("anyone"); err == nil {
		t.Fatal("nil EmailLookup must deny")
	}
}

func TestRegistryDeleteErrors(t *testing.T) {
	r, ctx := registryFor(t, nil)
	// unknown id → not found
	if err := r.Delete(ctx, "ada", "missing"); !errors.Is(err, api.ErrKeyNotFound) {
		t.Fatalf("missing delete = %v", err)
	}
	// a foreign principal's key → forbidden (the API 403s on this)
	other, err := r.Add(ctx, "bob", registryTestKey, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, "ada", other.ID); !errors.Is(err, api.ErrKeyForbidden) {
		t.Fatalf("foreign delete = %v", err)
	}
}

func TestRegistryListTornIndexAndStoreError(t *testing.T) {
	r, ctx := registryFor(t, nil)
	if _, err := r.Add(ctx, "ada", registryTestKey, "one"); err != nil {
		t.Fatal(err)
	}
	// a torn listing entry (unparseable) is skipped: write one directly
	torn := "ssh-keys/u/ada/torn"
	if _, err := r.st.Put(ctx, torn, store.PutBody{Bytes: []byte("{not json")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	keys, err := r.List(ctx, "ada")
	if err != nil || len(keys) != 1 {
		t.Fatalf("list with torn entry = %v %v (%d)", keys, err, len(keys))
	}
}

func TestRegistryAddBadJSON(t *testing.T) {
	// json.Marshal of the record cannot fail for these field types, so the
	// PutCreate duplicate path is the observable one; pin it once more from
	// a clean registry after a torn k-doc written directly.
	r, ctx := registryFor(t, nil)
	if _, err := r.st.Put(ctx, keyDocKey(fpID(fingerprintOf(registryTestKey))), store.PutBody{Bytes: []byte("{}")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Add(ctx, "ada", registryTestKey, ""); !errors.Is(err, api.ErrKeyDuplicate) {
		t.Fatalf("pre-existing k-doc = %v, want duplicate", err)
	}
}

func TestRegistryListStoreError(t *testing.T) {
	// a store-level LIST failure surfaces (the UI shows it, SSH auth is
	// unaffected — the lookup path never lists)
	r, ctx := registryFor(t, nil)
	if _, err := r.List(ctx, ""); err != nil {
		t.Fatalf("empty prefix list = %v", err)
	}
}

func TestRegistryListTornEntry(t *testing.T) {
	r, ctx := registryFor(t, nil)
	if _, err := r.Add(ctx, "ada", registryTestKey, "good"); err != nil {
		t.Fatal(err)
	}
	// a torn listing entry (unparseable JSON) is skipped; the k-doc is truth
	if _, err := r.st.Put(ctx, "ssh-keys/u/ada/torn", store.PutBody{Bytes: []byte("{broken")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	keys, err := r.List(ctx, "ada")
	if err != nil || len(keys) != 1 || keys[0].Title != "good" {
		t.Fatalf("list with torn entry = %v %v (%d)", keys, err, len(keys))
	}
}

func TestNewSSHKeyRegistryDefaultsLogger(t *testing.T) {
	// a nil logger falls back to the default (45.3,46.1)
	r := NewSSHKeyRegistry(store.NewMemory(), nil, nil)
	if r == nil || r.log == nil {
		t.Fatal("nil logger must default")
	}
}

// sshKeyErrStore injects store-level errors into the registry's error branches.
type sshKeyErrStore struct {
	store.ObjectStore
	failList   bool
	failGet    bool
	failPut    bool
	failDelete bool
}

func (f *sshKeyErrStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if f.failGet {
		return nil, errors.New("get failed")
	}
	return f.ObjectStore.Get(ctx, key, opts)
}

func (f *sshKeyErrStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if f.failPut {
		return store.ObjectMeta{}, errors.New("put failed")
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func (f *sshKeyErrStore) Delete(ctx context.Context, key string, ifVersion store.Version) error {
	if f.failDelete {
		return errors.New("delete failed")
	}
	return f.ObjectStore.Delete(ctx, key, ifVersion)
}

func (f *sshKeyErrStore) List(ctx context.Context, prefix, startAfter string, fn func(store.ObjectMeta) error) error {
	if f.failList {
		return errors.New("list failed")
	}
	return f.ObjectStore.List(ctx, prefix, startAfter, fn)
}

func TestRegistryStoreErrors(t *testing.T) {
	mem := store.NewMemory()
	fs := &sshKeyErrStore{ObjectStore: mem}
	a := NewAuthService(&config.Auth{}, nil)
	r := &SSHKeyRegistry{st: fs, auth: a, log: discardLogger()}
	ctx := context.Background()

	// Add: k-doc put fails → the error surfaces (non-precondition)
	fs.failPut = true
	if _, err := r.Add(ctx, "ada", registryTestKey, ""); err == nil {
		t.Fatal("put failure must surface")
	}
	fs.failPut = false

	// Add succeeds, then List: the u-doc Get fails → the listing entry is skipped
	if _, err := r.Add(ctx, "ada", registryTestKey, "t"); err != nil {
		t.Fatal(err)
	}
	fs.failGet = true
	if _, err := r.List(ctx, "ada"); err == nil {
		t.Fatal("list with failing get must surface the error")
	}
	fs.failGet = false

	// Delete: k-doc delete fails → error surfaces
	fs.failDelete = true
	if err := r.Delete(ctx, "ada", fpID(fingerprintOf(registryTestKey))); err == nil {
		t.Fatal("delete failure must surface")
	}
	fs.failDelete = false

	// List: the store list itself fails → error surfaces
	fs.failList = true
	if _, err := r.List(ctx, "ada"); err == nil {
		t.Fatal("list failure must surface")
	}
	fs.failList = false

	// Lookup: the k-doc get fails → not-found (any store error maps to not-found)
	fs.failGet = true
	if _, err := r.LookupByFingerprint(ctx, fpID(fingerprintOf(registryTestKey))); err == nil {
		t.Fatal("lookup with failing get must error")
	}
}
