package setup

// Table-driven pins for the setup surface's behavior: the schema the JSON API
// reports, every coercion spelling the operator may type, the parse channels
// (form values, {"overrides": …} JSON, raw TOML), the validator's error list,
// the atomic save + requires_restart diff, and the OIDC discovery probe.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/config"
)

func schemaKey(t *testing.T, groups []Group, key string) Field {
	t.Helper()
	for _, g := range groups {
		for _, k := range g.Keys {
			if k.Key == key {
				return k
			}
		}
	}
	t.Fatalf("schema key %q not found", key)
	return Field{}
}

// --- schema ---------------------------------------------------------------

func TestBuildSchemaGroupsAndTypes(t *testing.T) {
	c := config.Defaults()
	groups := BuildSchema(c, c)
	if len(groups) < 3 || groups[0].Section != "server" || groups[1].Section != "auth" || groups[2].Section != "store" {
		sections := make([]string, len(groups))
		for i, g := range groups {
			sections[i] = g.Section
		}
		t.Fatalf("group sections = %v, want [server auth store ...]", sections)
	}
	for _, k := range groups[0].Keys {
		if strings.HasPrefix(k.Key, "server.auth.") {
			t.Fatalf("server group still carries auth key %q", k.Key)
		}
	}
	for _, k := range groups[1].Keys {
		if !strings.HasPrefix(k.Key, "server.auth.") {
			t.Fatalf("auth group carries non-auth key %q", k.Key)
		}
	}

	cases := []struct {
		key   string
		typ   string
		value any
	}{
		{"server.listen", "string", c.Server.Listen},
		{"server.request_timeout", "duration", c.Server.RequestTimeout.String()},
		{"server.max_push_bytes", "int", int64(c.Server.MaxPushBytes)},
		{"server.auto_create_on_push", "bool", c.Server.AutoCreateOnPush},
		{"server.cors_origins", "list", c.Server.CorsOrigins},
		{"server.auth.mode", "string", c.Server.Auth.Mode},
		{"server.auth.admin_groups", "list", c.Server.Auth.AdminGroups},
		{"server.auth.groups_claim", "string", c.Server.Auth.GroupsClaim},
		{"server.auth.tokens", "list", c.Server.Auth.Tokens},
		{"server.auth.session_ttl", "duration", c.Server.Auth.SessionTTL.String()},
	}
	for _, tc := range cases {
		k := schemaKey(t, groups, tc.key)
		if k.Type != tc.typ {
			t.Errorf("%s type = %q, want %q", tc.key, k.Type, tc.typ)
		}
		if !reflect.DeepEqual(k.Value, tc.value) {
			t.Errorf("%s value = %#v, want %#v", tc.key, k.Value, tc.value)
		}
		if k.Doc == "" {
			t.Errorf("%s has no doc name", tc.key)
		}
	}
	// Defaults are reported alongside the effective values, and the schema for
	// a candidate differs from the running config only in Value.
	cand := config.Defaults()
	cand.Server.Listen = "10.0.0.1:1234"
	diff := BuildSchema(cand, c)
	if got := schemaKey(t, diff, "server.listen"); got.Value != "10.0.0.1:1234" || got.Default != c.Server.Listen {
		t.Fatalf("listen value/default = %#v/%#v", got.Value, got.Default)
	}
}

func TestBuildSchemaSecretFlagsAndFieldJSON(t *testing.T) {
	c := config.Defaults()
	groups := BuildSchema(c, c)
	for key, want := range map[string]bool{
		"server.auth.tokens":              false, // a list of token tables, not one value
		"server.auth.oauth_client_secret": true,
		"server.auth.session_secret":      true,
		"server.auth.access_token_ttl":    false, // a duration
		"server.listen":                   false,
		"server.ssh.host_key":             false, // a path
		"store.s3.secret_key_env":         false, // an env var NAME
		"store.backend":                   false,
		"cache.disk_high_watermark":       false,
	} {
		if got := schemaKey(t, groups, key).Secret; got != want {
			t.Errorf("%s secret = %v, want %v", key, got, want)
		}
	}
	// The wire shape of one row is frozen: key, value, default, type, secret, doc.
	b, err := json.Marshal(Field{Key: "server.listen"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"key":"server.listen","value":null,"default":null,"type":"","secret":false,"doc":""}`; string(b) != want {
		t.Fatalf("field JSON = %s, want %s", b, want)
	}
	b, err = json.Marshal(Group{Section: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"section":"s","keys":null}`; string(b) != want {
		t.Fatalf("group JSON = %s, want %s", b, want)
	}
}

func TestSecretKeyTruthTable(t *testing.T) {
	for name, want := range map[string]bool{
		"key":            false, // a bare path flag (host_key), never a value
		"my_password":    true,
		"session_secret": true,
		"admin_token":    true,
		"token":          true,
		"token_env":      false,
		"listen":         false,
		"public_url":     false,
	} {
		if got := secretKey(name); got != want {
			t.Errorf("secretKey(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestFmtAnyAndSchemaSkip(t *testing.T) {
	// Invalid reflection → nil (never a panic on schema edges).
	if got := fmtAny(reflect.Value{}); got != nil {
		t.Fatalf("fmtAny(invalid) = %v, want nil", got)
	}
	if got := fmtAny(reflect.ValueOf(config.Duration(90 * time.Second))); got != "1m30s" {
		t.Fatalf("fmtAny(duration) = %v", got)
	}
	if got := fmtAny(reflect.ValueOf(config.ByteSize(1<<20 + 3))); got != int64(1<<20+3) {
		t.Fatalf("fmtAny(bytesize) = %#v", got)
	}
	// The default branch passes plain scalars through untouched.
	if got := fmtAny(reflect.ValueOf(struct{ V float64 }{V: 3.5}).Field(0)); got != 3.5 {
		t.Fatalf("fmtAny(float) = %#v", got)
	}
	// Untagged struct fields never become schema keys.
	type tagged struct {
		SkipMe string
		Keep   string `toml:"keep"`
	}
	g := &Group{}
	appendSchemaKeys(g, "s.", reflect.ValueOf(tagged{}), reflect.ValueOf(tagged{}))
	if len(g.Keys) != 1 || g.Keys[0].Key != "s.keep" {
		t.Fatalf("schema keys = %+v, want [s.keep]", g.Keys)
	}
}

// --- coercion -------------------------------------------------------------

// TestCoerceSpellings pins the operator-facing spellings: the same ones the
// TOML file and SPINDLE__ env overlay accept (sizes with binary units,
// durations with d/w, floats, unsigned counters) plus struct-slice TOML
// fragments.
func TestCoerceSpellings(t *testing.T) {
	durationField := func() reflect.Value {
		c := config.Defaults()
		return reflect.ValueOf(&c.Server.RequestTimeout).Elem()
	}
	sizeField := func() reflect.Value {
		c := config.Defaults()
		return reflect.ValueOf(&c.Server.MaxPushBytes).Elem()
	}
	uint64Field := func() reflect.Value {
		c := config.Defaults()
		return reflect.ValueOf(&c.WAL.SnapshotEveryEntries).Elem()
	}
	strategyField := func() reflect.Value {
		c := config.Defaults()
		return reflect.ValueOf(&c.Bundles.Strategy).Elem()
	}

	cases := []struct {
		name string
		fv   func() reflect.Value
		raw  string
		path string
		err  string // "" = success
		// after, when set, is asserted against the coerced value.
		after func(t *testing.T, v reflect.Value)
	}{
		{
			name: "duration days", fv: durationField, raw: "7d", path: "server.request_timeout",
			after: func(t *testing.T, v reflect.Value) {
				if time.Duration(v.Int()) != 7*24*time.Hour {
					t.Fatalf("7d = %v", time.Duration(v.Int()))
				}
			},
		},
		{
			name: "duration weeks", fv: durationField, raw: "2w", path: "server.request_timeout",
			after: func(t *testing.T, v reflect.Value) {
				if time.Duration(v.Int()) != 14*24*time.Hour {
					t.Fatalf("2w = %v", time.Duration(v.Int()))
				}
			},
		},
		{
			name: "duration bare int seconds", fv: durationField, raw: "45", path: "server.request_timeout",
			after: func(t *testing.T, v reflect.Value) {
				if time.Duration(v.Int()) != 45*time.Second {
					t.Fatalf("45 = %v", time.Duration(v.Int()))
				}
			},
		},
		{name: "duration garbage", fv: durationField, raw: "not-a-time", path: "server.request_timeout", err: "not a duration"},
		{name: "duration compound", fv: durationField, raw: "1h30m", path: "server.request_timeout"},
		{
			name: "size MiB", fv: sizeField, raw: "64MiB", path: "server.max_push_bytes",
			after: func(t *testing.T, v reflect.Value) {
				if v.Int() != 64<<20 {
					t.Fatalf("64MiB = %d", v.Int())
				}
			},
		},
		{
			name: "size GiB", fv: sizeField, raw: "1GiB", path: "server.max_push_bytes",
			after: func(t *testing.T, v reflect.Value) {
				if v.Int() != 1<<30 {
					t.Fatalf("1GiB = %d", v.Int())
				}
			},
		},
		{
			name: "size scientific", fv: sizeField, raw: "1.073741824e+10", path: "server.max_push_bytes",
			after: func(t *testing.T, v reflect.Value) {
				if v.Int() != 10737418240 {
					t.Fatalf("scientific = %d", v.Int())
				}
			},
		},
		{name: "size bare bytes", fv: sizeField, raw: "1000", path: "server.max_push_bytes"},
		{name: "size fractional", fv: sizeField, raw: "1.5", path: "server.max_push_bytes", err: "not a size"},
		{name: "size garbage", fv: sizeField, raw: "zzz", path: "server.max_push_bytes", err: "not a size"},
		{
			name: "uint64 counter", fv: uint64Field, raw: "5", path: "wal.snapshot_every_entries",
			after: func(t *testing.T, v reflect.Value) {
				if v.Uint() != 5 {
					t.Fatalf("uint = %d", v.Uint())
				}
			},
		},
		{name: "uint negative", fv: uint64Field, raw: "-5", path: "wal.snapshot_every_entries", err: "not an int"},
		{name: "uint garbage", fv: uint64Field, raw: "abc", path: "wal.snapshot_every_entries", err: "not an int"},
		{
			name: "uint32 overflow", path: "wal.cas_max_retries", raw: "4294967296", err: "out of range",
			fv: func() reflect.Value {
				c := config.Defaults()
				return reflect.ValueOf(&c.WAL.CASMaxRetries).Elem()
			},
		},
		{
			name: "float", path: "cache.disk_high_watermark", raw: "0.85",
			fv: func() reflect.Value {
				c := config.Defaults()
				return reflect.ValueOf(&c.Cache.DiskHighWatermark).Elem()
			},
			after: func(t *testing.T, v reflect.Value) {
				if v.Float() != 0.85 {
					t.Fatalf("float = %v", v.Float())
				}
			},
		},
		{
			name: "float garbage", path: "cache.disk_high_watermark", raw: "abc", err: "not a number",
			fv: func() reflect.Value {
				c := config.Defaults()
				return reflect.ValueOf(&c.Cache.DiskHighWatermark).Elem()
			},
		},
		{
			name: "string trims", path: "server.listen", raw: "  127.0.0.1:9999  ",
			fv: func() reflect.Value {
				c := config.Defaults()
				return reflect.ValueOf(&c.Server.Listen).Elem()
			},
			after: func(t *testing.T, v reflect.Value) {
				if v.String() != "127.0.0.1:9999" {
					t.Fatalf("string = %q", v.String())
				}
			},
		},
		{
			name: "bool false", path: "server.auto_create_on_push", raw: "false",
			fv: func() reflect.Value {
				c := config.Defaults()
				c.Server.AutoCreateOnPush = true
				return reflect.ValueOf(&c.Server.AutoCreateOnPush).Elem()
			},
			after: func(t *testing.T, v reflect.Value) {
				if v.Bool() {
					t.Fatal("bool false did not stick")
				}
			},
		},
		{
			name: "bool garbage", path: "server.auto_create_on_push", raw: "maybe", err: "not a bool",
			fv: func() reflect.Value {
				c := config.Defaults()
				return reflect.ValueOf(&c.Server.AutoCreateOnPush).Elem()
			},
		},
		{
			name: "string list trims", path: "server.cors_origins", raw: "a.com, b.com",
			fv: func() reflect.Value {
				c := config.Defaults()
				return reflect.ValueOf(&c.Server.CorsOrigins).Elem()
			},
			after: func(t *testing.T, v reflect.Value) {
				if v.Len() != 2 || v.Index(1).String() != "b.com" {
					t.Fatalf("list = %+v", v.Interface())
				}
			},
		},
		{
			name: "strategy fragment", fv: strategyField, path: "bundles.strategy",
			raw: "[[strategy]]\nname = \"weekly\"\nkind = \"full\"\nschedule = \"0 0 * * * *\"\nkeep = 2",
			after: func(t *testing.T, v reflect.Value) {
				if v.Len() != 1 || v.Index(0).FieldByName("Keep").Int() != 2 {
					t.Fatalf("fragment decoded = %+v", v.Interface())
				}
			},
		},
		{name: "strategy wrong table", fv: strategyField, raw: "[[wrong]]\nname = \"x\"", path: "bundles.strategy", err: "must define [[strategy]]"},
		{name: "strategy bad field", fv: strategyField, raw: "[[strategy]]\nkeep = \"many\"", path: "bundles.strategy", err: "bundles.strategy"},
		{name: "strategy not a fragment", fv: strategyField, raw: "not = [valid", path: "bundles.strategy", err: "not a TOML fragment"},
		{
			name: "int32 overflow", path: "p.n", raw: "99999999999", err: "out of range",
			fv: func() reflect.Value { v := int32(0); return reflect.ValueOf(&v).Elem() },
		},
		{
			name: "non-string list", path: "p.l", raw: "a,b", err: "unsupported list element",
			fv: func() reflect.Value { v := []int{}; return reflect.ValueOf(&v).Elem() },
		},
		{
			name: "unsupported kind", path: "p.c", raw: "x", err: "unsupported type",
			fv: func() reflect.Value { v := make(chan int); return reflect.ValueOf(&v).Elem() },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fv := tc.fv()
			err := Coerce(fv, tc.raw, tc.path)
			if tc.err == "" {
				if err != nil {
					t.Fatalf("coerce %q = %v, want success", tc.raw, err)
				}
				if tc.after != nil {
					tc.after(t, fv)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("coerce %q = %v, want %q", tc.raw, err, tc.err)
			}
		})
	}
}

// TestSetPathWalksSections pins the path walker's guards (a section used as a
// value, a scalar used as a section, a single segment, an unknown key) and
// the parseSettingInt edge behind the numeric coercions.
func TestSetPathWalksSections(t *testing.T) {
	c := config.Defaults()
	rv := reflect.ValueOf(c).Elem()
	for _, tc := range []struct {
		name  string
		parts []string
		want  string
	}{
		{"section as value", []string{"server", "auth"}, "server.auth: is a section, not a value"},
		{"scalar as section", []string{"server", "http2", "deep"}, "server.http2: is not a section"},
		{"unknown section", []string{"nope", "key"}, `unknown key ".nope"`},
		{"unknown key", []string{"server", "nope"}, `unknown key "server.nope"`},
		{"single segment", []string{"server"}, `key "server" must be section.field`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if len(tc.parts) < 2 {
				err = setDottedKey(c, tc.parts[0], "x")
			} else {
				err = setPath(rv, tc.parts, "x", "")
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("setPath(%v) = %v, want %q", tc.parts, err, tc.want)
			}
		})
	}
	// The nested auth section is editable by its dotted key.
	if err := setDottedKey(c, "server.auth.mode", "token"); err != nil || c.Server.Auth.Mode != "token" {
		t.Fatalf("auth mode = %q err=%v", c.Server.Auth.Mode, err)
	}
	if _, err := parseSettingInt("1.5"); err == nil {
		t.Fatal("fractional int must fail")
	}
	if n, err := parseSettingInt("1.073741824e+09"); err != nil || n != 1073741824 {
		t.Fatalf("scientific int = %d err=%v", n, err)
	}
}

// --- parse channels -------------------------------------------------------

func TestParseValues(t *testing.T) {
	base := config.Defaults()
	base.Server.Listen = "127.0.0.1:8080"
	values := url.Values{}
	values.Set("server.request_timeout", "7d")
	values.Set("server.max_push_bytes", "64MiB")
	values.Set("wal.cas_max_retries", "3")
	values.Set("cache.disk_high_watermark", "0.75")
	values.Set("bundles.strategy", "[[strategy]]\nname = \"weekly\"\nkind = \"full\"\nschedule = \"0 0 * * * *\"")
	c, err := ParseValues(values, base)
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(c.Server.RequestTimeout) != 7*24*time.Hour {
		t.Fatalf("duration = %v", c.Server.RequestTimeout)
	}
	if int64(c.Server.MaxPushBytes) != 64<<20 {
		t.Fatalf("size = %d", c.Server.MaxPushBytes)
	}
	if c.WAL.CASMaxRetries != 3 {
		t.Fatalf("uint32 = %d", c.WAL.CASMaxRetries)
	}
	if c.Cache.DiskHighWatermark != 0.75 {
		t.Fatalf("float = %v", c.Cache.DiskHighWatermark)
	}
	if len(c.Bundles.Strategy) != 1 || c.Bundles.Strategy[0].Name != "weekly" {
		t.Fatalf("strategy = %+v", c.Bundles.Strategy)
	}
	// Untouched keys keep the base value.
	if c.Server.Listen != "127.0.0.1:8080" {
		t.Fatalf("untouched key changed: %q", c.Server.Listen)
	}
	// Repeated keys keep their last value (form semantics).
	rep := url.Values{"server.listen": {"a:1", "b:2"}}
	if c, err := ParseValues(rep, config.Defaults()); err != nil || c.Server.Listen != "b:2" {
		t.Fatalf("repeated key = %q err=%v", c.Server.Listen, err)
	}
	// Empty value lists are skipped.
	if c, err := ParseValues(url.Values{"server.listen": {}}, config.Defaults()); err != nil || c.Server.Listen != config.Defaults().Server.Listen {
		t.Fatalf("empty value list = %q err=%v", c.Server.Listen, err)
	}
}

func TestParseValuesErrors(t *testing.T) {
	cases := []struct {
		name   string
		values url.Values
		want   string
	}{
		{"bare key", url.Values{"listen": {"x"}}, "must be section.field"},
		{"unknown section", url.Values{"nope.key": {"1"}}, "unknown key"},
		{"unknown key", url.Values{"server.nope": {"1"}}, "unknown key"},
		{"section as value", url.Values{"server.auth": {"x"}}, "is a section, not a value"},
		{"bad duration", url.Values{"server.request_timeout": {"zzz"}}, "not a duration"},
		// Sorted keys make the reported failure deterministic.
		{"sorted first", url.Values{"server.request_timeout": {"zzz"}, "cache.max_bytes": {"zzz"}}, "cache.max_bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseValues(tc.values, config.Defaults()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseValues = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseRequestJSONAndTOML(t *testing.T) {
	parse := func(t *testing.T, body string) (*config.Config, error) {
		t.Helper()
		req := httptest.NewRequest("PUT", "/api/v1/setup", strings.NewReader(body))
		return ParseRequest(req, config.Defaults())
	}
	// Raw TOML scalar shapes.
	c, err := parse(t, "[server]\nlisten = \"127.0.0.1:9999\"\nauto_create_on_push = true\nmax_push_bytes = 123")
	if err != nil || c.Server.Listen != "127.0.0.1:9999" || !c.Server.AutoCreateOnPush || int64(c.Server.MaxPushBytes) != 123 {
		t.Fatalf("toml parse = %+v err=%v", c.Server, err)
	}
	// TOML floats ride the float64 branch (no exponents on the wire).
	c, err = parse(t, "[server]\nmax_push_bytes = 1.5e3")
	if err != nil || int64(c.Server.MaxPushBytes) != 1500 {
		t.Fatalf("toml float = %+v err=%v", c.Server.MaxPushBytes, err)
	}
	// TOML lists join on commas before the list coercion runs.
	c, err = parse(t, "[server]\ncors_origins = [\"a.test\", \"b.test\"]")
	if err != nil || len(c.Server.CorsOrigins) != 2 || c.Server.CorsOrigins[1] != "b.test" {
		t.Fatalf("toml list = %+v err=%v", c.Server.CorsOrigins, err)
	}
	// JSON overrides with non-string values.
	c, err = parse(t, `{"overrides": {"server.auto_create_on_push": true, "server.max_push_bytes": 1000, "server.cors_origins": "a.com,b.com"}}`)
	if err != nil || !c.Server.AutoCreateOnPush || int64(c.Server.MaxPushBytes) != 1000 || len(c.Server.CorsOrigins) != 2 {
		t.Fatalf("json overrides = %+v %+v err=%v", c.Server, c.Server.Auth, err)
	}
	// JSON list values join on commas.
	c, err = parse(t, `{"overrides": {"server.cors_origins": ["a.test", "b.test"]}}`)
	if err != nil || len(c.Server.CorsOrigins) != 2 {
		t.Fatalf("list override = %+v err=%v", c.Server.CorsOrigins, err)
	}
	// A nested object where a scalar belongs → the section guard fires.
	if _, err := parse(t, `{"overrides": {"server.auth": {"mode": "x"}}}`); err == nil ||
		!strings.Contains(err.Error(), "is a section, not a value") {
		t.Fatalf("nested object = %v", err)
	}
	// A key without a section.
	if _, err := parse(t, `{"overrides": {"listen": "x"}}`); err == nil ||
		!strings.Contains(err.Error(), "must be section.field") {
		t.Fatalf("bare key = %v", err)
	}
	// Unknown dotted key.
	if _, err := parse(t, `{"overrides": {"server.nope": "x"}}`); err == nil ||
		!strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("unknown dotted = %v", err)
	}
	// Malformed JSON and malformed TOML both surface an error.
	if _, err := parse(t, `{"overrides": `); err == nil {
		t.Fatal("bad json must fail")
	}
	if _, err := parse(t, "[server\nbroken"); err == nil {
		t.Fatal("bad toml must fail")
	}
	// A top-level non-table value is rejected.
	if _, err := parse(t, "server = 5\n"); err == nil || !strings.Contains(err.Error(), "must be a table") {
		t.Fatalf("non-table section = %v", err)
	}
	// Inline tables are not scalar spellings → typed error, not a panic.
	if _, err := parse(t, "[server]\nrequest_timeout = {a = 1}"); err == nil ||
		!strings.Contains(err.Error(), "not a duration") {
		t.Fatalf("inline table = %v", err)
	}
	// A torn body surfaces the read error (never a half-parsed config).
	req := &http.Request{Body: io.NopCloser(errBody{})}
	if _, err := ParseRequest(req, config.Defaults()); err == nil {
		t.Fatal("torn body must error")
	}
}

type errBody struct{}

func (errBody) Read([]byte) (int, error) { return 0, errors.New("torn body") }

// --- validate + save ------------------------------------------------------

func TestValidateReturnsEveryError(t *testing.T) {
	if errs := Validate(config.Defaults()); len(errs) != 0 {
		t.Fatalf("defaults must validate, got %v", errs)
	}
	c := config.Defaults()
	c.Store.Backend = "nope"
	c.Store.MultipartPartSize = 1 << 30
	c.Store.MultipartThreshold = 1 << 20
	errs := Validate(c)
	if len(errs) < 2 {
		t.Fatalf("errors = %v, want the full list", errs)
	}
	for _, e := range errs {
		if e.Message == "" {
			t.Fatalf("empty message in %v", errs)
		}
	}
	if !strings.Contains(errs[0].Message, "store.backend") && !strings.Contains(errs[1].Message, "store.backend") {
		t.Fatalf("backend error missing from %v", errs)
	}
}

func TestSaveWritesAtomicallyAndDiffsRestart(t *testing.T) {
	dataDir := t.TempDir()
	effective := config.FirstRunDefaults(dataDir)
	cand := config.FirstRunDefaults(dataDir)
	cand.Server.Listen = "127.0.0.1:9099"
	cand.Server.RequestTimeout = config.Duration(90 * time.Minute)

	restart, err := Save(dataDir, cand, effective)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"server.listen", "server.request_timeout"} {
		if !contains(restart, key) {
			t.Fatalf("requires_restart = %v, want %s", restart, key)
		}
	}
	path := filepath.Join(dataDir, config.ConfigFileName)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("spindle.toml missing: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp file left behind: %v", err)
	}
	saved, err := config.LoadSetupBase(dataDir, []string{path})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Server.Listen != "127.0.0.1:9099" || time.Duration(saved.Server.RequestTimeout) != 90*time.Minute {
		t.Fatalf("saved config = %+v", saved.Server)
	}

	// Saving the same values again reports no restart keys (non-nil slice: the
	// API marshals [] not null).
	restart, err = Save(dataDir, cand, cand)
	if err != nil {
		t.Fatal(err)
	}
	if restart == nil || len(restart) != 0 {
		t.Fatalf("unchanged save restart = %#v", restart)
	}

	// An invalid candidate writes nothing and reports the failure.
	bad := config.FirstRunDefaults(dataDir)
	bad.Store.Backend = "nope"
	if _, err := Save(dataDir, bad, effective); err == nil {
		t.Fatal("invalid save must fail")
	}
}

func TestSaveSerialized(t *testing.T) {
	dataDir := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := config.FirstRunDefaults(dataDir)
			c.Server.Listen = "127.0.0.1:" + strconv.Itoa(9000+i)
			if _, err := Save(dataDir, c, c); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent save = %v", err)
	}
	if _, err := config.LoadSetupBase(dataDir, []string{filepath.Join(dataDir, config.ConfigFileName)}); err != nil {
		t.Fatalf("saved file unreadable after concurrent saves: %v", err)
	}
}

func TestEffectiveValueAndRestartKeys(t *testing.T) {
	c := config.Defaults()
	if EffectiveValue(c, "server.listen") != c.Server.Listen {
		t.Fatalf("effective listen = %v", EffectiveValue(c, "server.listen"))
	}
	// The auth subsection is keyed by its own group, not a section prefix.
	if EffectiveValue(c, "server.auth.mode") != c.Server.Auth.Mode {
		t.Fatalf("effective auth mode = %v", EffectiveValue(c, "server.auth.mode"))
	}
	if EffectiveValue(c, "nodot") != nil || EffectiveValue(c, "bogus") != nil {
		t.Fatal("unknown key must be nil")
	}
	cand := config.Defaults()
	cand.Server.Listen = "1.2.3.4:1"
	keys := RestartKeys(c, cand)
	if len(keys) != 1 || keys[0] != "server.listen" {
		t.Fatalf("restart keys = %v", keys)
	}
	if keys := RestartKeys(c, c); keys == nil || len(keys) != 0 {
		t.Fatalf("unchanged restart keys = %#v", keys)
	}
	if anyToString(nil) != "<nil>" || anyToString(true) != "true" || anyToString(int64(7)) != "7" || anyToString(3.5) != "3.5" {
		t.Fatal("anyToString truth table broken")
	}
}

func TestBaseConfig(t *testing.T) {
	dataDir := t.TempDir()
	// No candidate: the zero-config first-run shape.
	c := BaseConfig(dataDir, []string{filepath.Join(dataDir, config.ConfigFileName)})
	if c.Server.Listen != "0.0.0.0:8080" || !c.Server.AutoCreateOnPush || c.Store.Backend != "filesystem" {
		t.Fatalf("first-run base = %+v %+v", c.Server, c.Store)
	}
	if c.Store.Root != filepath.Join(dataDir, "store") {
		t.Fatalf("store root = %q", c.Store.Root)
	}
	// An unparseable candidate also falls back to the first-run shape (its
	// errors are reported separately through the boot state).
	bad := filepath.Join(dataDir, "bad.toml")
	if err := os.WriteFile(bad, []byte("[server\nbroken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := BaseConfig(dataDir, []string{bad, dataDir}); c.Server.Listen != "0.0.0.0:8080" {
		t.Fatalf("fallback base = %+v", c.Server)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// --- OIDC discovery probe -------------------------------------------------

func TestProbeAuth(t *testing.T) {
	var served map[string]any
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(served)
	}))
	defer idp.Close()

	// ok: the document names its own issuer and carries every required endpoint.
	served = map[string]any{
		"issuer":                                idp.URL,
		"authorization_endpoint":                "https://id.example.com/authorize",
		"token_endpoint":                        "https://id.example.com/token",
		"jwks_uri":                              "https://id.example.com/jwks",
		"scopes_supported":                      []string{"openid", "email"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
	probe, errs := ProbeAuth(idp.URL+"/", " spindle-web ", " http://localhost:8080/_auth/callback ")
	if len(errs) != 0 {
		t.Fatalf("ok probe errors = %v", errs)
	}
	if probe.Issuer != idp.URL || probe.AuthorizationEndpoint != "https://id.example.com/authorize" ||
		probe.TokenEndpoint != "https://id.example.com/token" || probe.JWKSURI != "https://id.example.com/jwks" {
		t.Fatalf("probe = %+v", probe)
	}
	if len(probe.ScopesSupported) != 2 || len(probe.SigningAlgs) != 1 {
		t.Fatalf("probe lists = %+v", probe)
	}
	if !probe.ClientIDPresent || probe.RedirectURI != "http://localhost:8080/_auth/callback" {
		t.Fatalf("probe client/redirect = %+v", probe)
	}

	// Issuer mismatch: the document must name the exact configured issuer.
	served["issuer"] = "https://elsewhere.example.com"
	if _, errs := ProbeAuth(idp.URL, "", ""); len(errs) != 1 || !strings.Contains(errs[0].Message, "does not match") {
		t.Fatalf("mismatch errors = %v", errs)
	}

	// Missing a required endpoint is named in the error.
	served["issuer"] = idp.URL
	delete(served, "jwks_uri")
	if _, errs := ProbeAuth(idp.URL, "", ""); len(errs) != 1 || !strings.Contains(errs[0].Message, "jwks_uri") {
		t.Fatalf("missing endpoint errors = %v", errs)
	}

	// A non-200 discovery answer is reported with its status.
	served = nil
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer gone.Close()
	if _, errs := ProbeAuth(gone.URL, "", ""); len(errs) != 1 || !strings.Contains(errs[0].Message, "discovery returned 404") {
		t.Fatalf("404 errors = %v", errs)
	}

	// Unreachable issuer → a fetch failure message keyed to the issuer.
	if _, errs := ProbeAuth("http://127.0.0.1:1", "", ""); len(errs) != 1 ||
		errs[0].Key != "server.auth.issuer" || !strings.Contains(errs[0].Message, "discovery fetch failed") {
		t.Fatalf("unreachable errors = %v", errs)
	}

	// A body that is not an OIDC document.
	badDoc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer badDoc.Close()
	if _, errs := ProbeAuth(badDoc.URL, "", ""); len(errs) != 1 || !strings.Contains(errs[0].Message, "not a valid OIDC discovery document") {
		t.Fatalf("bad doc errors = %v", errs)
	}

	// Form-side guards run before discovery: a missing issuer and a
	// non-http(s) redirect are both named.
	probe, errs = ProbeAuth("", "", "ftp://x/_auth/callback")
	if len(errs) != 2 || errs[0].Key != "server.auth.issuer" || errs[1].Key != "redirect_uri" {
		t.Fatalf("guard errors = %+v", errs)
	}
	if probe.ClientIDPresent {
		t.Fatalf("empty client id reported present: %+v", probe)
	}
}
