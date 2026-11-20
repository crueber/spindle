// Package setup owns the machine-readable configuration surface behind
// /setup and /api/v1/setup (docs/go/06_server_http.md §3.4):
//
//   - BuildSchema describes every toml key the UI may edit — with the
//     effective and default values, types, secret flags and doc names —
//     exactly as GET /api/v1/setup reports them.
//   - ParseValues / ParseRequest apply raw operator input (the page's form
//     values, {"overrides": …} JSON, or a raw TOML body) ON TOP OF the
//     file-visible base config, coercing each key into the typed Config.
//   - Validate runs the same full §5 validator as boot; Save serializes the
//     candidate atomically to <data-dir>/spindle.toml (tmp + rename, one
//     process-wide mutex per §3.4 concurrency).
//   - RestartKeys / EffectiveValue power the requires_restart diff.
//   - ProbeAuth runs the OIDC discovery probe behind the auth card's Test
//     button.
//
// internal/server wires the HTTP surface and internal/webui the rendered page;
// both bind these same functions, so the JSON API and the settings page can
// never drift apart (no HTTP loopback, no duplicated parsers).
package setup

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/crueber/spindle/internal/config"
)

// saveMu serializes Saves (process-wide mutex, §3.4 concurrency note).
var saveMu sync.Mutex

// Field is one key row of a GET /api/v1/setup group. The JSON tags are the
// wire contract with the setup UI — never reorder or rename them.
type Field struct {
	Key     string `json:"key"`
	Value   any    `json:"value"`
	Default any    `json:"default"`
	Type    string `json:"type"`
	Secret  bool   `json:"secret"`
	Doc     string `json:"doc"`
	// DisplayType is for non-HTTP consumers (the setup page): it may refine
	// Type without changing the frozen /api/v1/setup JSON (json:"-").
	DisplayType string `json:"-"`
}

// Group is one schema card: a config section plus its editable keys.
type Group struct {
	Section string  `json:"section"`
	Keys    []Field `json:"keys"`
}

// Error is one validation error as the setup API reports it
// ({key, message}); Key is empty for errors that name no single key.
type Error struct {
	Key     string `json:"key"`
	Message string `json:"message"`
}

// BaseConfig resolves the file-visible base the setup surface edits on top
// of (§3.4): defaults ⊕ the first existing candidate config file; with no
// candidate the zero-config first-run shape; an unparseable file also falls
// back to the first-run shape (its errors are reported separately through the
// boot state). This is what makes setup EDIT the current configuration
// instead of replacing it with defaults plus a diff.
func BaseConfig(dataDir string, candidates []string) *config.Config {
	base, err := config.LoadSetupBase(dataDir, candidates)
	if err != nil {
		return config.FirstRunDefaults(dataDir)
	}
	return base
}

// BuildSchema walks the Config struct via its toml tags: section = top-level
// field, key = "<section>.<field>[.<subfield>…]". Nested section structs
// (server.auth.*) flatten into dotted keys — they are real editable settings,
// not one opaque row. Types: string|int|bool|duration|list.
// The auth subsection is split into its own card: the setup UI treats auth as
// a first-class section whose fields depend on server.auth.mode.
func BuildSchema(effective, defaults *config.Config) []Group {
	rv, dv := reflect.ValueOf(*effective), reflect.ValueOf(*defaults)
	rt := rv.Type()
	groups := []Group{}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		section := f.Tag.Get("toml")
		if section == "" || section == "-" || f.Type.Kind() != reflect.Struct {
			continue
		}
		g := Group{Section: section, Keys: []Field{}}
		appendSchemaKeys(&g, section+".", rv.Field(i), dv.Field(i))
		if len(g.Keys) == 0 {
			continue
		}
		if section == "server" {
			serverKeys := []Field{}
			authKeys := []Field{}
			for _, k := range g.Keys {
				if strings.HasPrefix(k.Key, "server.auth.") {
					authKeys = append(authKeys, k)
				} else {
					serverKeys = append(serverKeys, k)
				}
			}
			if len(serverKeys) > 0 {
				groups = append(groups, Group{Section: "server", Keys: serverKeys})
			}
			if len(authKeys) > 0 {
				groups = append(groups, Group{Section: "auth", Keys: authKeys})
			}
			continue
		}
		groups = append(groups, g)
	}
	return groups
}

func appendSchemaKeys(g *Group, prefix string, sv, dv reflect.Value) {
	st := sv.Type()
	for j := 0; j < st.NumField(); j++ {
		sf := st.Field(j)
		name := sf.Tag.Get("toml")
		if name == "" || name == "-" {
			continue
		}
		key := prefix + name
		if sf.Type.Kind() == reflect.Struct {
			appendSchemaKeys(g, key+".", sv.Field(j), dv.Field(j))
			continue
		}
		typ := "string"
		switch sf.Type.Kind() {
		case reflect.Bool:
			typ = "bool"
		case reflect.Int, reflect.Int64, reflect.Int32:
			typ = "int"
		case reflect.Slice:
			typ = "list"
		}
		if sf.Type == reflect.TypeOf(config.Duration(0)) {
			typ = "duration"
		}
		display := ""
		if sf.Type == reflect.TypeOf(config.ByteSize(0)) {
			// The wire contract reports sizes as int bytes; the page shows the
			// spec spelling (64GiB) and posts it back in the same spelling.
			display = "size"
		}
		g.Keys = append(g.Keys, Field{
			Key:         key,
			Value:       fmtAny(sv.Field(j)),
			Default:     fmtAny(dv.Field(j)),
			Type:        typ,
			DisplayType: display,
			Secret:      secretKey(name),
			Doc:         sf.Name,
		})
	}
}

func fmtAny(v reflect.Value) any {
	if !v.IsValid() || !v.CanInterface() {
		return nil
	}
	switch x := v.Interface().(type) {
	case config.Duration:
		return x.String()
	case config.ByteSize:
		return int64(x)
	default:
		return x
	}
}

func secretKey(name string) bool {
	l := strings.ToLower(name)
	return strings.Contains(l, "secret") || strings.Contains(l, "password") ||
		strings.Contains(l, "token") || l == "key"
}

// ParseRequest applies an HTTP setup body to base: raw TOML or
// {"overrides": {key: value}} (§3.4) — the file-visible config
// (BaseConfig), so untouched keys keep their current values. Never mutates
// base's callers: base is a freshly-loaded object per request.
func ParseRequest(r *http.Request, base *config.Config) (*config.Config, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	c := base
	text := strings.TrimSpace(string(body))
	if strings.HasPrefix(text, "{") {
		var req struct {
			Overrides map[string]any `json:"overrides"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		for k, v := range req.Overrides {
			if err := setDottedKey(c, k, jsonScalar(v)); err != nil {
				return nil, err
			}
		}
		return c, nil
	}
	// Raw TOML: merge over defaults via the toml decoder's primitive map walk.
	var overlay map[string]any
	if _, derr := toml.Decode(string(body), &overlay); derr != nil {
		return nil, derr
	}
	for section, fields := range overlay {
		m, ok := fields.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("section %q must be a table", section)
		}
		for name, v := range m {
			// Two explicit segments (not a dotted split): a quoted TOML key may
			// itself contain a dot, and the lookup is by toml tag either way.
			if err := setPath(reflect.ValueOf(c).Elem(), []string{section, name}, tomlScalar(v), ""); err != nil {
				return nil, err
			}
		}
	}
	return c, nil
}

// ParseValues applies the settings page's form values (key → raw string, the
// same spellings the TOML file accepts) on top of base. Keys are sorted so a
// multi-key rejection is deterministic; a repeated key keeps its last value,
// matching net/http form semantics.
func ParseValues(v url.Values, base *config.Config) (*config.Config, error) {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	c := base
	for _, k := range keys {
		vals := v[k]
		if len(vals) == 0 {
			continue
		}
		if err := setDottedKey(c, k, vals[len(vals)-1]); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// setDottedKey splits "server.auth.mode" and walks the toml-tagged structs.
func setDottedKey(c *config.Config, key, raw string) error {
	parts := strings.Split(key, ".")
	if len(parts) < 2 {
		return fmt.Errorf("key %q must be section.field", key)
	}
	return setPath(reflect.ValueOf(c).Elem(), parts, raw, "")
}

// jsonScalar renders one {"overrides": …} JSON value as the raw string the
// coercion layer accepts.
func jsonScalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return fmt.Sprintf("%t", x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64) // no exponent: 1073741824, not "1.07e+09"
	case []any:
		items := make([]string, len(x))
		for i := range x {
			items[i] = fmt.Sprintf("%v", x[i])
		}
		return strings.Join(items, ",")
	default:
		return fmt.Sprintf("%v", x)
	}
}

// tomlScalar renders one decoded raw-TOML value as the raw string the
// coercion layer accepts.
func tomlScalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return fmt.Sprintf("%t", x)
	case int64:
		return fmt.Sprintf("%d", x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		items := make([]string, len(x))
		for i := range x {
			items[i] = fmt.Sprintf("%v", x[i])
		}
		return strings.Join(items, ",")
	default:
		return fmt.Sprintf("%v", v)
	}
}

// setPath sets one dotted key ("server.auth.mode") by reflection over the
// toml tags, walking nested section structs. path is the prefix already
// consumed (for error messages).
func setPath(sv reflect.Value, parts []string, raw, path string) error {
	st := sv.Type()
	for j := 0; j < st.NumField(); j++ {
		name := st.Field(j).Tag.Get("toml")
		if name == "" || name == "-" || name != parts[0] {
			continue
		}
		next := path
		if next != "" {
			next += "."
		}
		next += name
		fv := sv.Field(j)
		if len(parts) == 1 {
			if fv.Kind() == reflect.Struct {
				return fmt.Errorf("%s: is a section, not a value", next)
			}
			return Coerce(fv, raw, next)
		}
		if fv.Kind() != reflect.Struct {
			return fmt.Errorf("%s: is not a section", next)
		}
		return setPath(fv, parts[1:], raw, next)
	}
	return fmt.Errorf("unknown key %q", path+"."+parts[0])
}

// Coerce parses one raw operator value into the field it belongs to. Named
// types first, via the shared spec parsers: the setup UI sends the same
// spellings the TOML file accepts ("64MiB", "7d") — Sscanf("%d") or a bare
// time.ParseDuration would reject them ("7d" is not a Go duration).
func Coerce(field reflect.Value, raw, path string) error {
	raw = strings.TrimSpace(raw)
	switch field.Type() {
	case reflect.TypeOf(config.Duration(0)):
		d, err := config.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("%s: not a duration", path)
		}
		field.SetInt(int64(d))
		return nil
	case reflect.TypeOf(config.ByteSize(0)):
		if n, err := config.ParseByteSize(raw); err == nil {
			field.SetInt(int64(n))
			return nil
		}
		n, err := parseSettingInt(raw) // bare bytes / scientific notation
		if err != nil {
			return fmt.Errorf("%s: not a size", path)
		}
		field.SetInt(n)
		return nil
	}
	switch field.Kind() {
	case reflect.String:
		field.SetString(raw)
	case reflect.Bool:
		if raw == "true" {
			field.SetBool(true)
		} else if raw == "false" {
			field.SetBool(false)
		} else {
			return fmt.Errorf("%s: not a bool", path)
		}
	case reflect.Int, reflect.Int64, reflect.Int32:
		n, err := parseSettingInt(raw)
		if err != nil {
			return fmt.Errorf("%s: not an int", path)
		}
		if field.OverflowInt(n) {
			return fmt.Errorf("%s: %d out of range", path, n)
		}
		field.SetInt(n)
	case reflect.Uint, reflect.Uint64, reflect.Uint32:
		n, err := parseSettingInt(raw)
		if err != nil || n < 0 {
			return fmt.Errorf("%s: not an int", path)
		}
		if field.OverflowUint(uint64(n)) {
			return fmt.Errorf("%s: %d out of range", path, n)
		}
		field.SetUint(uint64(n))
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("%s: not a number", path)
		}
		field.SetFloat(f)
	case reflect.Slice:
		// Struct slices (bundles.strategy → []BundleStrategy) arrive as a TOML
		// fragment naming the field: "[[strategy]]\n…". Decode it by the field's
		// toml name — the last path segment.
		if field.Type().Elem().Kind() == reflect.Struct {
			name := path[strings.LastIndex(path, ".")+1:]
			var prim map[string]toml.Primitive
			if _, err := toml.Decode(raw, &prim); err != nil {
				return fmt.Errorf("%s: not a TOML fragment: %v", path, err)
			}
			p, ok := prim[name]
			if !ok {
				return fmt.Errorf(`%s: fragment must define [[%s]]`, path, name)
			}
			out := reflect.New(field.Type())
			if err := toml.PrimitiveDecode(p, out.Interface()); err != nil {
				return fmt.Errorf("%s: %v", path, err)
			}
			field.Set(out.Elem())
			return nil
		}
		items := strings.Split(raw, ",")
		out := reflect.MakeSlice(field.Type(), len(items), len(items))
		for i := range items {
			if out.Index(i).Kind() == reflect.String {
				out.Index(i).SetString(strings.TrimSpace(items[i]))
			} else {
				return fmt.Errorf("%s: unsupported list element", path)
			}
		}
		field.Set(out)
	default:
		return fmt.Errorf("%s: unsupported type", path)
	}
	return nil
}

// parseSettingInt accepts plain decimals and the scientific notation that
// JSON numbers produce when formatted with %v ("1.073741824e+09").
func parseSettingInt(raw string) (int64, error) {
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return n, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f != math.Trunc(f) {
		return 0, fmt.Errorf("not an integer: %q", raw)
	}
	return int64(f), nil
}

// Validate runs the full §5 validator WITHOUT saving and returns every error
// in the shape the setup API reports (empty = saveable). It returns the list,
// not one joined error, because §3.4 requires the 422 body to name each
// rejection separately.
func Validate(c *config.Config) []Error {
	_, errs := config.Validate(c)
	out := make([]Error, 0, len(errs))
	for _, e := range errs {
		out = append(out, Error{Message: e.Error()})
	}
	return out
}

// Save validates (config.SaveSetup fails closed), then atomically writes
// <data-dir>/spindle.toml, serialized by the process-wide mutex (§3.4), and
// diffs the candidate against the still-running effective config. The
// returned keys are the ones whose effective value differs from the file just
// saved — the process does NOT hot-reload. An empty (non-nil) slice means
// nothing changed.
func Save(dataDir string, c *config.Config, effective *config.Config) ([]string, error) {
	saveMu.Lock()
	err := config.SaveSetup(c, dataDir)
	saveMu.Unlock()
	if err != nil {
		return nil, err
	}
	return RestartKeys(effective, c), nil
}

// RestartKeys diffs the effective config against the candidate.
func RestartKeys(effective, candidate *config.Config) []string {
	groups := BuildSchema(candidate, effective)
	out := []string{}
	for _, g := range groups {
		for _, k := range g.Keys {
			if anyToString(EffectiveValue(effective, k.Key)) != anyToString(k.Value) {
				out = append(out, k.Key)
			}
		}
	}
	return out
}

// EffectiveValue returns the value the schema reports for key in c, or nil
// when key is not a schema key. Keys are unique across the schema (the auth
// subsection lives in its own group, so a section-prefix lookup would miss
// server.auth.*).
func EffectiveValue(c *config.Config, key string) any {
	for _, g := range BuildSchema(c, c) {
		for _, k := range g.Keys {
			if k.Key == key {
				return k.Value
			}
		}
	}
	return nil
}

func anyToString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return fmt.Sprintf("%t", x)
	case int64:
		return fmt.Sprintf("%d", x)
	case nil:
		return "<nil>"
	default:
		return fmt.Sprintf("%v", x)
	}
}

// AuthProbe is a successful OIDC discovery probe (the auth card's Test
// button).
type AuthProbe struct {
	Issuer                string
	AuthorizationEndpoint string
	TokenEndpoint         string
	JWKSURI               string
	ScopesSupported       []string
	SigningAlgs           []string
	RedirectURI           string
	ClientIDPresent       bool
}

// ProbeAuth probes the OIDC discovery document of the issuer in the FORM (not
// the saved config) so the auth card's Test button validates what the
// operator typed. It fails with errors when discovery is unreachable,
// malformed, or disagrees with the configured issuer (an OIDC discovery
// document MUST name its own issuer).
func ProbeAuth(issuer, clientID, redirectURI string) (AuthProbe, []Error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	redirectURI = strings.TrimSpace(redirectURI)
	res := AuthProbe{RedirectURI: redirectURI, ClientIDPresent: strings.TrimSpace(clientID) != ""}
	errs := []Error{}
	if issuer == "" {
		errs = append(errs, Error{Key: "server.auth.issuer", Message: "issuer is required (e.g. https://id.example.com)"})
	}
	if redirectURI != "" && !strings.HasPrefix(redirectURI, "http://") && !strings.HasPrefix(redirectURI, "https://") {
		errs = append(errs, Error{Key: "redirect_uri", Message: fmt.Sprintf("redirect_uri %q must be an http(s) URL", redirectURI)})
	}
	if len(errs) > 0 {
		return res, errs
	}

	doc := struct {
		Issuer                 string   `json:"issuer"`
		AuthorizationEndpoint  string   `json:"authorization_endpoint"`
		TokenEndpoint          string   `json:"token_endpoint"`
		JWKSURI                string   `json:"jwks_uri"`
		ScopesSupported        []string `json:"scopes_supported"`
		IDTokenSigningAlgValue []string `json:"id_token_signing_alg_values_supported"`
	}{}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(issuer + "/.well-known/openid-configuration")
	if err != nil {
		return res, []Error{{Key: "server.auth.issuer", Message: fmt.Sprintf("discovery fetch failed: %v", err)}}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return res, []Error{{Key: "server.auth.issuer", Message: fmt.Sprintf("discovery returned %d from %s/.well-known/openid-configuration", resp.StatusCode, issuer)}}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return res, []Error{{Key: "server.auth.issuer", Message: fmt.Sprintf("discovery response is not a valid OIDC discovery document: %v", err)}}
	}
	res.Issuer = doc.Issuer
	res.AuthorizationEndpoint = doc.AuthorizationEndpoint
	res.TokenEndpoint = doc.TokenEndpoint
	res.JWKSURI = doc.JWKSURI
	res.ScopesSupported = doc.ScopesSupported
	res.SigningAlgs = doc.IDTokenSigningAlgValue
	if doc.Issuer != issuer {
		errs = append(errs, Error{Key: "server.auth.issuer", Message: fmt.Sprintf("discovery issuer %q does not match the configured issuer %q — use the exact value the document names", doc.Issuer, issuer)})
	}
	for _, m := range []struct {
		field, what string
	}{
		{doc.AuthorizationEndpoint, "authorization_endpoint"},
		{doc.TokenEndpoint, "token_endpoint"},
		{doc.JWKSURI, "jwks_uri"},
	} {
		if m.field == "" {
			errs = append(errs, Error{Key: "server.auth.issuer", Message: fmt.Sprintf("discovery document is missing %s (required by OIDC Discovery)", m.what)})
		}
	}
	if len(errs) > 0 {
		return res, errs
	}
	return res, []Error{}
}
