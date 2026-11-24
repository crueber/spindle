package setup

// roundtrip_test.go — G1's acceptance gate: a whole-schema render → coerce →
// decode → re-encode round trip must not change any key's value when nothing
// is edited.
//
// The renderer is setup.FormValues (the exact map POST /setup hands to
// ParseValues, prefix stripped) so struct-slice rows, unsigned counters and
// empty lists are exercised for real — the three classes the audit measured
// as 422s or silent `[""]` writes.

import (
	"bytes"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/crueber/spindle/internal/config"
)

const tokenLiteral = "literal-token-value-do-not-print"

// roundTripConfig is the file-visible base the test edits: every class the
// audit flagged carries a live value (struct slices with secrets, unsigned
// counters) and every empty-list key stays empty so the `""` → `[""]` bug has
// nowhere to hide.
func roundTripConfig(t *testing.T) *config.Config {
	t.Helper()
	c := config.Defaults()
	c.DataDir = t.TempDir()
	c.Server.Listen = "0.0.0.0:18080"
	c.Server.PublicURL = "https://spindle.example.com"
	c.Server.Roles = []string{"serve", "maintain"}
	c.Server.CorsOrigins = nil // empty list must stay empty
	c.Server.MaxPushBytes = 64 << 30
	c.Server.RequestTimeout = 90 * 60e9
	c.Server.Auth.Issuer = "https://id.example.com"
	c.Server.Auth.AnonymousRead = true
	c.Server.Auth.SessionSecret = strings.Repeat("s", 32)
	c.Server.Auth.OAuthClientID = "client-id"
	c.Server.Auth.OAuthClientSecret = "oauth-client-secret-value"
	c.Server.Auth.AllowedDomains = []string{"example.com"}
	c.Server.Auth.Tokens = []config.StaticToken{
		{Principal: "alice@example.com", Token: tokenLiteral, Write: true},
		{Principal: "bob@example.com", TokenEnv: "BOB_TOKEN", Write: true},
	}
	c.Server.Auth.AllowedEmails = nil
	c.Server.Auth.AdminGroups = []string{"spindle-admin"}
	c.Server.Auth.Audiences = nil
	c.Server.Auth.TrustedForwarders = nil
	c.WAL.SnapshotEveryEntries = 512 // uint64
	c.WAL.CASMaxRetries = 7          // uint32
	c.WAL.PushBrokerToken = "broker-token-value"
	c.Cache.MaxBytes = 20 << 30
	c.Cache.DiskHighWatermark = 0.75
	c.Cache.Prewarm = nil
	c.Placement.ServeExclude = nil
	c.Placement.MaintainExclude = nil
	c.Upstream.Git = "https://upstream.example.com/repo.git"
	c.Upstream.Follow = []string{"main"}
	c.Import.URLAllowlist = nil
	c.Bundles.Strategy = []config.BundleStrategy{
		{Name: "weekly", Kind: "full", Schedule: "0 0 23 * * 0", Keep: 2, BackfillMax: 1, Filter: "blob:none"},
		{Name: "daily", Kind: "incremental", Base: "weekly", Schedule: "0 0 23 * * *", BackfillMax: 0, Chain: true, Filter: "blob:none"},
		{Name: "hourly", Kind: "incremental", Base: "daily", Schedule: "0 0 * * * *", BackfillMax: 48, Filter: "blob:none", Refs: []string{"refs/heads/main"}},
	}
	c.Bundles.ExtraRefs = nil
	c.Bundles.SignedURLFor = nil
	c.Bundles.Require = nil
	if err := config.ValidateForSave(c); err != nil {
		t.Fatalf("fixture config must be saveable: %v", err)
	}
	return c
}

// TestFormValuesWholeSchemaRoundTrip is G1's acceptance gate: no key changes
// value when nothing is edited, and a second round trip is a no-op.
func TestFormValuesWholeSchemaRoundTrip(t *testing.T) {
	base := roundTripConfig(t)
	groups := BuildSchema(base, config.Defaults())

	// The three broken classes are actually in the schema.
	for _, key := range []string{
		"server.auth.tokens", "bundles.strategy", // struct slices
		"wal.snapshot_every_entries", "wal.cas_max_retries", // unsigned ints
		"server.auth.allowed_emails", "cache.prewarm", "upstream.follow", "import.url_allowlist", // empty lists
	} {
		if _, err := findField(groups, key); err != nil {
			t.Fatalf("%s missing from the schema: %v", key, err)
		}
	}

	values := FormValues(groups)
	// The form is the page's: struct slices are rows, secrets are the mask.
	if got := values.Get("server.auth.tokens#rows"); got != "" {
		t.Fatalf("an unrendered row marker was posted: %q", got)
	}
	if got := values.Get("server.auth.tokens#0.token"); got != SecretMask {
		t.Fatalf("token row secret rendered %q, want the mask", got)
	}
	if got := values.Get("server.auth.tokens#1.token_env"); got != "BOB_TOKEN" {
		t.Fatalf("token_env row = %q", got)
	}
	if got := values.Get("bundles.strategy#2.schedule"); got != "0 0 * * * *" {
		t.Fatalf("strategy row schedule = %q", got)
	}
	if got := values.Get("wal.snapshot_every_entries"); got != "512" {
		t.Fatalf("unsigned counter = %q, want 512", got)
	}
	if got := values.Get("wal.cas_max_retries"); got != "7" {
		t.Fatalf("unsigned counter = %q, want 7", got)
	}
	if got := values.Get("cache.prewarm"); got != "" {
		t.Fatalf("empty list rendered %q, want empty", got)
	}

	got, err := ParseValues(values, cloneConfig(t, base))
	if err != nil {
		t.Fatalf("coercing the untouched form: %v", err)
	}
	// 1. Byte-identical TOML: the file an untouched Save writes.
	want, err := config.MarshalTOML(base)
	if err != nil {
		t.Fatal(err)
	}
	have, err := config.MarshalTOML(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, have) {
		t.Fatalf("round trip changed the config\n--- want ---\n%s\n--- got ---\n%s", want, have)
	}
	// 2. And no schema key changed value (names the key on failure).
	for _, g := range groups {
		for _, k := range g.Keys {
			theirs := anyToString(EffectiveValue(base, k.Key))
			ours := anyToString(EffectiveValue(got, k.Key))
			if theirs != ours {
				t.Fatalf("%s changed: %q → %q", k.Key, theirs, ours)
			}
		}
	}
	// 3. The empty lists stayed empty, never [""].
	if len(got.Cache.Prewarm) != 0 || len(got.Server.Auth.AllowedEmails) != 0 ||
		len(got.Import.URLAllowlist) != 0 || len(got.Placement.ServeExclude) != 0 {
		t.Fatalf("an empty list became a bogus entry: prewarm=%q emails=%q allowlist=%q exclude=%q",
			got.Cache.Prewarm, got.Server.Auth.AllowedEmails, got.Import.URLAllowlist, got.Placement.ServeExclude)
	}
	// 4. Struct slices survived whole, secrets included.
	if !reflect.DeepEqual(got.Server.Auth.Tokens, base.Server.Auth.Tokens) {
		t.Fatalf("tokens = %+v, want %+v", got.Server.Auth.Tokens, base.Server.Auth.Tokens)
	}
	if !reflect.DeepEqual(got.Bundles.Strategy, base.Bundles.Strategy) {
		t.Fatalf("strategies = %+v, want %+v", got.Bundles.Strategy, base.Bundles.Strategy)
	}

	// 5. A second Save is a no-op (nothing drifts on repeated rounds).
	again, err := ParseValues(FormValues(BuildSchema(got, config.Defaults())), cloneConfig(t, got))
	if err != nil {
		t.Fatalf("second round trip: %v", err)
	}
	againTOML, err := config.MarshalTOML(again)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, againTOML) {
		t.Fatalf("second round trip drifted\n--- want ---\n%s\n--- got ---\n%s", want, againTOML)
	}
}

func TestValidateNamesOffendingKey(t *testing.T) {
	c := config.Defaults()
	c.Server.Roles = []string{"serve", "bogus"}
	c.Bundles.Strategy = []config.BundleStrategy{{Name: "", Kind: "full", Schedule: "not a cron"}}
	errs := Validate(c)
	if len(errs) == 0 {
		t.Fatal("expected validation errors")
	}
	byKey := map[string]bool{}
	for _, e := range errs {
		if e.Key == "" {
			t.Errorf("keyless error: %q", e.Message)
		}
		byKey[e.Key] = true
	}
	for _, key := range []string{"server.roles", "bundles.strategy"} {
		if !byKey[key] {
			t.Fatalf("no error names %s: %+v", key, errs)
		}
	}
}

func TestKeyInMessageWholeKey(t *testing.T) {
	keys := []string{"server.listen", "server.listener_x", "bundles.strategy"}
	cases := []struct{ msg, want string }{
		{"server.listen is required (host:port)", "server.listen"},
		{"bundles.strategy[2].schedule \"x\" is not a valid cron", "bundles.strategy"},
		{"no key here at all", ""},
		{"server.listener_x: nope", "server.listener_x"},
	}
	for _, tc := range cases {
		if got := KeyInMessage(tc.msg, keys); got != tc.want {
			t.Errorf("KeyInMessage(%q) = %q, want %q", tc.msg, got, tc.want)
		}
	}
}

func TestStructSliceSecretMasking(t *testing.T) {
	c := config.Defaults()
	c.Server.Auth.Tokens = []config.StaticToken{{Principal: "alice@example.com", Token: tokenLiteral, Write: true}}
	groups := BuildSchema(c, config.Defaults())
	f, err := findField(groups, "server.auth.tokens")
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != "list" || f.DisplayType != "rows" {
		t.Fatalf("tokens type = %q/%q, want list/rows", f.Type, f.DisplayType)
	}
	// The schema VALUE (GET /api/v1/setup) must not carry the secret either.
	if s := fmt.Sprint(f.Value); strings.Contains(s, tokenLiteral) {
		t.Fatalf("schema value leaks the token: %s", s)
	}
	rows := RenderRows(f)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want one element + the add row", len(rows))
	}
	if rows[0].Caption != "alice@example.com" {
		t.Fatalf("row caption = %q", rows[0].Caption)
	}
	tokenCell := rowCell(t, rows[0], "token")
	if tokenCell.Value != SecretMask || !tokenCell.Secret {
		t.Fatalf("token cell = %+v, want the mask", tokenCell)
	}
	if rows[0].Base != 0 || rows[1].Base != -1 {
		t.Fatalf("row base indices = %d/%d", rows[0].Base, rows[1].Base)
	}
	if !RowsDiffer(f) {
		t.Fatal("a non-default token list must read as 'from file'")
	}
}

// TestRowEditorAddRotateRevoke covers the editor's four verbs against the
// SAME Coerce path a scalar key uses.
func TestRowEditorAddRotateRevoke(t *testing.T) {
	base := roundTripConfig(t)
	groups := BuildSchema(base, config.Defaults())

	t.Run("rotate keeps the other rows", func(t *testing.T) {
		v := FormValues(groups)
		v.Set("server.auth.tokens#0.token", "rotated-token-value")
		c, err := ParseValues(v, cloneConfig(t, base))
		if err != nil {
			t.Fatal(err)
		}
		if c.Server.Auth.Tokens[0].Token != "rotated-token-value" {
			t.Fatalf("row 0 token = %q", c.Server.Auth.Tokens[0].Token)
		}
		if c.Server.Auth.Tokens[1] != base.Server.Auth.Tokens[1] {
			t.Fatalf("row 1 changed: %+v", c.Server.Auth.Tokens[1])
		}
		// A rotated secret must still be reported as restart-required, even
		// though both sides render the same mask in the schema.
		if keys := RestartKeys(c, base); !contains(keys, "server.auth.tokens") {
			t.Fatalf("RestartKeys = %v, want server.auth.tokens", keys)
		}
	})

	t.Run("revoke row 0 does not move row 0's secret to row 1", func(t *testing.T) {
		v := FormValues(groups)
		v.Set("server.auth.tokens#0.__remove", "true")
		c, err := ParseValues(v, cloneConfig(t, base))
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Server.Auth.Tokens) != 1 {
			t.Fatalf("tokens = %+v", c.Server.Auth.Tokens)
		}
		if c.Server.Auth.Tokens[0].Principal != "bob@example.com" || c.Server.Auth.Tokens[0].Token != "" || c.Server.Auth.Tokens[0].TokenEnv != "BOB_TOKEN" {
			t.Fatalf("surviving token = %+v (row identity followed position, not __base)", c.Server.Auth.Tokens[0])
		}
	})

	t.Run("revoke every row is the empty slice", func(t *testing.T) {
		v := FormValues(groups)
		for i := range base.Server.Auth.Tokens {
			v.Set(fmt.Sprintf("server.auth.tokens#%d.__remove", i), "true")
		}
		c, err := ParseValues(v, cloneConfig(t, base))
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Server.Auth.Tokens) != 0 {
			t.Fatalf("tokens = %+v, want none", c.Server.Auth.Tokens)
		}
	})

	t.Run("add a strategy from the blank row", func(t *testing.T) {
		v := FormValues(groups)
		i := len(base.Bundles.Strategy) // the add row
		for name, raw := range map[string]string{
			"name": "tenmin", "kind": "incremental", "base": "hourly",
			"schedule": "0 */10 * * * *", "chain": "true", "filter": "blob:none",
		} {
			v.Set(RowCellKey("bundles.strategy", i, name), raw)
		}
		c, err := ParseValues(v, cloneConfig(t, base))
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Bundles.Strategy) != 4 {
			t.Fatalf("strategies = %+v", c.Bundles.Strategy)
		}
		last := c.Bundles.Strategy[3]
		if last.Name != "tenmin" || !last.Chain || last.Schedule != "0 */10 * * * *" {
			t.Fatalf("added strategy = %+v", last)
		}
		if errs := Validate(c); len(errs) != 0 {
			t.Fatalf("validated added strategy: %+v", errs)
		}
	})

	t.Run("a bad cell names its key", func(t *testing.T) {
		v := FormValues(groups)
		v.Set("wal.cas_max_retries", "not-a-number")
		_, err := ParseValues(v, cloneConfig(t, base))
		if err == nil || !strings.Contains(err.Error(), "wal.cas_max_retries") {
			t.Fatalf("error = %v, want the offending key", err)
		}
		// The validator names the row it rejects.
		v = FormValues(groups)
		v.Set("bundles.strategy#0.keep", "3") // keep fails on incremental rows only; row 0 is full
		v.Set("bundles.strategy#1.keep", "3") // incremental + keep = invalid
		c, err := ParseValues(v, cloneConfig(t, base))
		if err != nil {
			t.Fatal(err)
		}
		errs := Validate(c)
		if len(errs) == 0 || errs[0].Key != "bundles.strategy" {
			t.Fatalf("errors = %+v, want bundles.strategy", errs)
		}
	})
}

// TestEmptyListIsEmpty pins the `[""]` regression directly: posting a blank
// field for every empty-list key yields zero-length slices.
func TestEmptyListIsEmpty(t *testing.T) {
	base := config.Defaults()
	keys := []string{
		"server.roles", "server.cors_origins", "server.auth.allowed_domains",
		"server.auth.allowed_emails", "server.auth.write_domains",
		"server.auth.audiences", "server.auth.trusted_forwarders",
		"cache.prewarm", "placement.serve_exclude", "placement.maintain_exclude",
		"bundles.extra_refs", "bundles.signed_url_for", "bundles.require",
		"upstream.follow", "import.url_allowlist",
	}
	v := url.Values{}
	for _, k := range keys {
		v.Set(k, "")
	}
	c, err := ParseValues(v, base)
	if err != nil {
		t.Fatal(err)
	}
	for _, val := range [][]string{
		c.Server.Roles, c.Server.CorsOrigins, c.Server.Auth.AllowedDomains,
		c.Server.Auth.AllowedEmails, c.Server.Auth.WriteDomains,
		c.Server.Auth.Audiences, c.Server.Auth.TrustedForwarders,
		c.Cache.Prewarm, c.Placement.ServeExclude, c.Placement.MaintainExclude,
		c.Bundles.ExtraRefs, c.Bundles.SignedURLFor, c.Bundles.Require,
		c.Upstream.Follow, c.Import.URLAllowlist,
	} {
		if len(val) != 0 {
			t.Fatalf("a blank submit wrote %q", val)
		}
	}
}

func findField(groups []Group, key string) (Field, error) {
	for _, g := range groups {
		for _, k := range g.Keys {
			if k.Key == key {
				return k, nil
			}
		}
	}
	return Field{}, fmt.Errorf("key %q not in schema", key)
}

func rowCell(t *testing.T, row Row, name string) RowCell {
	t.Helper()
	for _, c := range row.Cells {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("row has no cell %q", name)
	return RowCell{}
}

// cloneConfig deep-copies a config so a parse never mutates the fixture.
func cloneConfig(t *testing.T, c *config.Config) *config.Config {
	t.Helper()
	body, err := config.MarshalTOML(c)
	if err != nil {
		t.Fatal(err)
	}
	out := config.Defaults()
	if _, err := toml.Decode(string(body), out); err != nil {
		t.Fatalf("re-decoding the fixture: %v", err)
	}
	out.DataDir = c.DataDir
	back, err := config.MarshalTOML(out)
	if err != nil || !bytes.Equal(body, back) {
		t.Fatalf("fixture clone is not identical (err=%v)\n%s\n---\n%s", err, body, back)
	}
	return out
}
