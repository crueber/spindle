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
	"errors"
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
	// Fields is the element schema of a struct slice (server.auth.tokens,
	// bundles.strategy): one Field per toml field of the element, Key = the
	// element's field name. It is what turns the row into an editor instead of
	// an opaque value (rows.go). Absent for every scalar/list key.
	Fields []Field `json:"fields,omitempty"`
	// DisplayType is for non-HTTP consumers (the setup page): it may refine
	// Type without changing the frozen /api/v1/setup JSON (json:"-").
	DisplayType string `json:"-"`
	// Enum is the closed value set of an enum-valued key. The setup page
	// renders a <select> over exactly these choices instead of free text;
	// the wire Type stays "string" so the frozen /api/v1/setup JSON is
	// unchanged (Enum is json:"-" like DisplayType).
	Enum []string `json:"-"`
}

// EnumValues returns the closed value set for an enum-valued config key (nil
// for a free-form key). The sets are the ones the loader and validator accept:
// store.backend is validated in config.Validate, the rest are the values the
// subsystems switch on (bundle.ServeSignedURL, maintain's cache modes, the
// telemetry slog handler, the upload-pack engine aliasing in 04_git.md §8.1).
// This table is the schema authority for the page's <select> choices; webui
// must not carry a second copy.
func EnumValues(key string) []string { return schemaEnums[key] }

var schemaEnums = map[string][]string{
	"server.auth.mode":       {"none", "token", "oidc"},
	"store.backend":          {"s3", "memory", "filesystem"},
	"cache.mode":             {"budget", "disk", "auto"},
	"bundles.serve_via":      {"proxy", "signed_url"},
	"compaction.engine":      {"git"},
	"git.upload_pack_engine": {"auto", "git", "gix"},
	"telemetry.log_format":   {"pretty", "json"},
}

// Group is one schema card: a config section plus its editable keys.
type Group struct {
	Section string  `json:"section"`
	Keys    []Field `json:"keys"`
}

// Error is one validation error as the setup API reports it
// ({key, message}); Key is empty for errors that name no single key.
//
// Coerce failures are reported through this same type (a parse error IS a
// validation error for one key): the page marks that row and opens its
// Advanced group instead of printing one keyless banner line. Error therefore
// implements error, so a coercion failure can be carried up the stack without
// losing the key.
type Error struct {
	Key     string `json:"key"`
	Message string `json:"message"`
}

// Error lets a keyed setup failure travel as an error value; callers lift it
// back with KeyOf or errors.As.
func (e Error) Error() string { return e.Message }

// KeyOf returns the key carried by err when err (or a wrapped error) is a
// keyed setup Error, and "" otherwise.
func KeyOf(err error) string {
	if err == nil {
		return ""
	}
	var e Error
	if errors.As(err, &e) {
		return e.Key
	}
	return ""
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
		section := tomlName(f.Tag.Get("toml"))
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
		name := tomlName(sf.Tag.Get("toml"))
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
		case reflect.Uint, reflect.Uint64, reflect.Uint32, reflect.Uint16, reflect.Uint8:
			// Unsigned counters (wal.snapshot_every_entries uint64,
			// wal.cas_max_retries uint32): same operator spelling as ints, and
			// Coerce's Uint arm bounds-checks them.
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
		var fields []Field
		if sf.Type.Kind() == reflect.Slice && sf.Type.Elem().Kind() == reflect.Struct {
			// A struct slice is edited as repeating rows (G1/G3/G4); the
			// element schema is carried so the page never invents a column.
			display = "rows"
			fields = elementFields(sf.Type.Elem())
		}
		enums := schemaEnums[key]
		if len(enums) > 0 && display == "" {
			// Enum-valued keys render as a <select> on the page (G18). The
			// wire Type stays "string": only DisplayType is page-facing.
			display = "enum"
		}
		g.Keys = append(g.Keys, Field{
			Key:         key,
			Value:       fmtAny(sv.Field(j)),
			Default:     fmtAny(dv.Field(j)),
			Type:        typ,
			DisplayType: display,
			Enum:        enums,
			Secret:      secretKey(name),
			Doc:         sf.Name,
			Fields:      fields,
		})
	}
}

// elementFields is the schema of one struct-slice element: the columns the row
// editor renders and ParseValues coerces back, one per toml field of the type.
func elementFields(t reflect.Type) []Field {
	out := []Field{}
	for j := 0; j < t.NumField(); j++ {
		sf := t.Field(j)
		name := tomlName(sf.Tag.Get("toml"))
		if name == "" || name == "-" {
			continue
		}
		typ := "string"
		switch sf.Type.Kind() {
		case reflect.Bool:
			typ = "bool"
		case reflect.Int, reflect.Int64, reflect.Int32:
			typ = "int"
		case reflect.Uint, reflect.Uint64, reflect.Uint32, reflect.Uint16, reflect.Uint8:
			typ = "int"
		case reflect.Slice:
			typ = "list"
		}
		if sf.Type == reflect.TypeOf(config.Duration(0)) {
			typ = "duration"
		}
		display := ""
		if sf.Type == reflect.TypeOf(config.ByteSize(0)) {
			display = "size"
		}
		out = append(out, Field{
			Key:         name,
			Type:        typ,
			DisplayType: display,
			Secret:      secretKey(name),
			Doc:         sf.Name,
		})
	}
	return out
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
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Struct {
			return structSliceMaps(v)
		}
		return x
	}
}

// structSliceMaps renders a struct-slice value as the row map the page and the
// test read: toml names, display-ready scalars, and every secret field masked.
// The schema is also served by GET /api/v1/setup, so masking here is what
// keeps a static token's value out of the JSON API as well as the page.
func structSliceMaps(v reflect.Value) []map[string]any {
	out := make([]map[string]any, 0, v.Len())
	t := v.Type().Elem()
	for i := 0; i < v.Len(); i++ {
		row := map[string]any{}
		elem := v.Index(i)
		for j := 0; j < t.NumField(); j++ {
			sf := t.Field(j)
			name := tomlName(sf.Tag.Get("toml"))
			if name == "" || name == "-" {
				continue
			}
			val := fmtAny(elem.Field(j))
			if secretKey(name) && val != nil && fmt.Sprint(val) != "" {
				val = SecretMask
			}
			row[name] = val
		}
		out = append(out, row)
	}
	return out
}

// tomlName strips a tag's options: BundleStrategy fields carry
// `toml:"keep,omitempty"`, and the last path segment / element column is the
// KEY, never the option list.
func tomlName(tag string) string {
	if i := strings.IndexByte(tag, ','); i >= 0 {
		return tag[:i]
	}
	return tag
}

// secretKey reports whether a config field NAME holds a secret VALUE. Suffix
// matching, not substring: `access_token_ttl` is a duration, `token_env` and
// `secret_key_env` name where a secret lives, and `host_key` is a path — none
// of them are values to mask or to treat as overwrite-only.
func secretKey(name string) bool {
	l := strings.ToLower(name)
	switch l {
	case "secret", "password", "token", "secrets_key":
		return true
	}
	return strings.HasSuffix(l, "_secret") || strings.HasSuffix(l, "_password") ||
		strings.HasSuffix(l, "_token")
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

// SecretMask is the placeholder /setup renders in place of a stored secret.
// Submitting it back (or an empty value) leaves the stored secret unchanged:
// the page never receives the real value, and there is no way to echo it back.
const SecretMask = "•••••• (set)"

// IsSecretKey reports whether key (a dotted schema key) names a secret VALUE.
// The field name carries the schema's own rule (internal/setup secretKey): a
// session secret, an OAuth client secret, a webhook secret, or a token.
func IsSecretKey(key string) bool {
	i := strings.LastIndexByte(key, '.')
	name := key[i+1:]
	// `*_env` / `*_file` keys name where a secret lives, not the secret.
	if strings.HasSuffix(name, "_env") || strings.HasSuffix(name, "_file") {
		return false
	}
	return secretKey(name)
}

// ParseValues applies the settings page's form values (key → raw string, the
// same spellings the TOML file accepts) on top of base. Keys are sorted so a
// multi-key rejection is deterministic; a repeated key keeps its last value,
// matching net/http form semantics.
//
// Struct-slice rows (rows.go's "K#<i>.<field>" namespace) are rebuilt first,
// then scalars; an empty submitted value for a list is the empty list, never
// [""] — a save can no longer invent a bogus list entry for a key the
// operator never touched.
func ParseValues(v url.Values, base *config.Config) (*config.Config, error) {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	c := base
	// Flatten first (rows and scalars share the last-value rule).
	flat := make(map[string]string, len(v))
	for _, k := range keys {
		vals := v[k]
		if len(vals) == 0 {
			continue
		}
		flat[k] = vals[len(vals)-1]
	}
	rowFields := []string{}
	seen := map[string]bool{}
	for _, k := range keys {
		i := strings.IndexByte(k, '#')
		if i <= 0 {
			continue
		}
		if fk := k[:i]; !seen[fk] {
			seen[fk] = true
			rowFields = append(rowFields, fk)
		}
	}
	sort.Strings(rowFields)
	for _, fk := range rowFields {
		if err := setRows(c, fk, flat); err != nil {
			return nil, err
		}
	}
	for _, k := range keys {
		if strings.IndexByte(k, '#') > 0 {
			continue
		}
		vals := v[k]
		if len(vals) == 0 {
			continue
		}
		raw := vals[len(vals)-1]
		// Secrets are overwrite-only: the page renders the mask (never the
		// stored value), so a round-tripped mask or an empty field means
		// "keep the current value".
		if IsSecretKey(k) && (raw == "" || raw == SecretMask) {
			continue
		}
		if err := setDottedKey(c, k, raw); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// setRows rebuilds one struct-slice field from its posted rows (rows.go).
func setRows(c *config.Config, key string, flat map[string]string) error {
	fv, path, err := fieldByKey(reflect.ValueOf(c).Elem(), strings.Split(key, "."), "")
	if err != nil {
		return err
	}
	if fv.Kind() != reflect.Slice || fv.Type().Elem().Kind() != reflect.Struct {
		return keyedError(path, "%s: not a struct list", path)
	}
	edits := ParseRowEdits(path, flat)
	if len(edits) == 0 {
		// Nothing posted for this field: leave the stored slice untouched, so
		// an untouched (nil) slice re-encodes to nothing.
		return nil
	}
	elemType := fv.Type().Elem()
	wasNil := fv.IsNil()
	out := reflect.MakeSlice(fv.Type(), 0, len(edits))
	for _, edit := range edits {
		if edit.Remove || rowIsBlank(elemType, edit.Cells) {
			continue
		}
		elem := reflect.New(elemType).Elem()
		// Start from the row this one was rendered from, not from the posted
		// position: the masked cell of a kept secret must follow the row it
		// belongs to (deleting row 0 must not hand row 1 row 0's token).
		if edit.Base >= 0 && edit.Base < fv.Len() {
			elem.Set(fv.Index(edit.Base))
		}
		for j := 0; j < elemType.NumField(); j++ {
			sf := elemType.Field(j)
			name := tomlName(sf.Tag.Get("toml"))
			if name == "" || name == "-" {
				continue
			}
			raw, ok := edit.Cells[name]
			if !ok {
				continue
			}
			sub := elem.Field(j)
			// A masked (or empty) secret cell keeps the row's stored value;
			// a new value rotates it.
			if secretKey(name) && (raw == "" || raw == SecretMask) {
				continue
			}
			// A blank numeric/size/duration cell keeps the row's value (type
			// 0 to clear it) — the blank "add" row's unset counters must not
			// fail as "not an int". Strings do clear, and booleans always post
			// explicit true/false.
			if strings.TrimSpace(raw) == "" && sub.Kind() != reflect.String {
				continue
			}
			if err := Coerce(sub, raw, fmt.Sprintf("%s[%d].%s", path, edit.Index, name)); err != nil {
				return err
			}
		}
		out = reflect.Append(out, elem)
	}
	if out.Len() == 0 && wasNil {
		return nil // unset stays unset (byte-identical re-encode)
	}
	fv.Set(out)
	return nil
}

// setDottedKey splits "server.auth.mode" and walks the toml-tagged structs.
func setDottedKey(c *config.Config, key, raw string) error {
	parts := strings.Split(key, ".")
	if len(parts) < 2 {
		return keyedError(key, "key %q must be section.field", key)
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
// toml tags, walking nested section structs.
func setPath(sv reflect.Value, parts []string, raw, path string) error {
	fv, next, err := fieldByKey(sv, parts, path)
	if err != nil {
		return err
	}
	if fv.Kind() == reflect.Struct {
		return keyedError(next, "%s: is a section, not a value", next)
	}
	return Coerce(fv, raw, next)
}

// fieldByKey resolves a dotted path over the toml tags and returns the target
// field plus its canonical key. path is the prefix already consumed.
func fieldByKey(sv reflect.Value, parts []string, path string) (reflect.Value, string, error) {
	st := sv.Type()
	for j := 0; j < st.NumField(); j++ {
		name := tomlName(st.Field(j).Tag.Get("toml"))
		if name == "" || name == "-" || name != parts[0] {
			continue
		}
		next := name
		if path != "" {
			next = path + "." + name
		}
		fv := sv.Field(j)
		if len(parts) == 1 {
			return fv, next, nil
		}
		if fv.Kind() != reflect.Struct {
			return reflect.Value{}, next, keyedError(next, "%s: is not a section", next)
		}
		return fieldByKey(fv, parts[1:], next)
	}
	unknown := strings.Join(parts, ".")
	if path != "" {
		unknown = path + "." + unknown
	}
	// The submitted key rides along even when the schema has no such row: the
	// errors table names it instead of printing a keyless line, and an unknown
	// key can never pass silently.
	return reflect.Value{}, "", keyedError(unknown, "unknown key %q", unknown)
}

// keyedError builds one keyed coercion failure: the message keeps the
// "<key>: …" spelling operators already see, and Key lets the page attach it
// to the offending row (and open a collapsed Advanced group).
func keyedError(key, format string, args ...any) Error {
	return Error{Key: key, Message: fmt.Sprintf(format, args...)}
}

// Coerce parses one raw operator value into the field it belongs to. Named
// types first, via the shared spec parsers: the setup UI sends the same
// spellings the TOML file accepts ("64MiB", "7d") — Sscanf("%d") or a bare
// time.ParseDuration would reject them ("7d" is not a Go duration).
//
// Every failure is a keyed Error: a value the coercion layer rejects must mark
// its row exactly like a config.Validate rejection does.
func Coerce(field reflect.Value, raw, path string) error {
	raw = strings.TrimSpace(raw)
	switch field.Type() {
	case reflect.TypeOf(config.Duration(0)):
		d, err := config.ParseDuration(raw)
		if err != nil {
			return keyedError(path, "%s: %q is not a duration (accepted ms|s|m|h|d|w, max %s)", path, raw, maxDurationSpelling)
		}
		field.SetInt(int64(d))
		return nil
	case reflect.TypeOf(config.ByteSize(0)):
		if n, err := config.ParseByteSize(raw); err == nil && n >= 0 {
			field.SetInt(int64(n))
			return nil
		}
		n, err := parseSettingInt(raw) // bare bytes / scientific notation
		if err != nil || n < 0 {
			return keyedError(path, "%s: %q is not a size (accepted 0..%d bytes, or a unit such as 64MiB)", path, raw, int64(math.MaxInt64))
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
			return keyedError(path, "%s: not a bool", path)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		bits := field.Type().Bits()
		n, err := parseSettingInt(raw)
		if err != nil {
			return keyedError(path, "%s: %q is not an int (accepted range %s)", path, raw, intRange(bits))
		}
		if field.OverflowInt(n) {
			return keyedError(path, "%s: %q is out of range (accepted range %s)", path, raw, intRange(bits))
		}
		field.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		bits := field.Type().Bits()
		n, err := parseSettingUint(raw)
		if err != nil {
			return keyedError(path, "%s: %q is not an int (accepted range %s)", path, raw, uintRange(bits))
		}
		if field.OverflowUint(n) {
			return keyedError(path, "%s: %q is out of range (accepted range %s)", path, raw, uintRange(bits))
		}
		field.SetUint(n)
	case reflect.Float32, reflect.Float64:
		bits := field.Type().Bits()
		f, err := strconv.ParseFloat(raw, bits)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return keyedError(path, "%s: %q is not a number (accepted a finite float%d)", path, raw, bits)
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
				return keyedError(path, "%s: not a TOML fragment: %v", path, err)
			}
			p, ok := prim[name]
			if !ok {
				return keyedError(path, `%s: fragment must define [[%s]]`, path, name)
			}
			out := reflect.New(field.Type())
			if err := toml.PrimitiveDecode(p, out.Interface()); err != nil {
				return keyedError(path, "%s: %v", path, err)
			}
			field.Set(out.Elem())
			return nil
		}
		items := strings.Split(raw, ",")
		wasNil := field.IsNil()
		out := reflect.MakeSlice(field.Type(), 0, len(items))
		for i := range items {
			item := strings.TrimSpace(items[i])
			// An empty field is the empty list, not [""]: the page renders a
			// key's current value, so a blank list row must stay blank rather
			// than inventing an entry the operator never typed.
			if item == "" {
				continue
			}
			if field.Type().Elem().Kind() == reflect.String {
				out = reflect.Append(out, reflect.ValueOf(item).Convert(field.Type().Elem()))
			} else {
				return keyedError(path, "%s: unsupported list element", path)
			}
		}
		if out.Len() == 0 && wasNil {
			// Keep "unset" unset: a nil slice encodes to nothing, so an
			// untouched field saves back byte-identically. Clearing a slice
			// that HAD entries still writes the explicit empty list.
			field.SetZero()
			return nil
		}
		field.Set(out)
	default:
		return keyedError(path, "%s: unsupported type", path)
	}
	return nil
}

// parseSettingInt accepts plain decimals and the scientific notation that
// JSON numbers produce when formatted with %v ("1.073741824e+09"). It never
// saturates: ParseInt covers the whole int64 range exactly, and the float
// fallback only accepts values it can represent exactly (|f| < 2^53). Before
// that bound existed, "99999999999999999999" fell through ParseInt, parsed as
// 1e20 and converted with int64(f) — an out-of-range float conversion that
// yields MinInt64 on amd64, so an out-of-range entry persisted as
// -9223372036854775808 with a success response (closing-audit Observation 3).
func parseSettingInt(raw string) (int64, error) {
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return n, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, fmt.Errorf("not an integer: %q", raw)
	}
	// 2^53 is the first integer float64 cannot represent exactly; 2^63 is one
	// past MaxInt64. Both bounds are exact float64 values.
	const maxExactInt = 9007199254740992.0 // 2^53
	if f < -maxExactInt || f >= maxExactInt {
		return 0, fmt.Errorf("not an exactly representable integer: %q", raw)
	}
	return int64(f), nil
}

// parseSettingUint is parseSettingInt for unsigned fields: it accepts the full
// uint64 range in plain decimal and refuses a negative, fractional, unexact or
// out-of-range value instead of wrapping it.
func parseSettingUint(raw string) (uint64, error) {
	if n, err := strconv.ParseUint(raw, 10, 64); err == nil {
		return n, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f < 0 {
		return 0, fmt.Errorf("not an unsigned integer: %q", raw)
	}
	const maxExactInt = 9007199254740992.0 // 2^53
	if f >= maxExactInt {
		return 0, fmt.Errorf("not an exactly representable integer: %q", raw)
	}
	return uint64(f), nil
}

// intRange renders the accepted range of a signed field width for a keyed
// coercion error ("-2147483648..2147483647").
func intRange(bits int) string {
	if bits >= 64 {
		return "-9223372036854775808..9223372036854775807"
	}
	lo := int64(-1) << (bits - 1)
	hi := int64(1)<<(bits-1) - 1
	return strconv.FormatInt(lo, 10) + ".." + strconv.FormatInt(hi, 10)
}

// uintRange renders the accepted range of an unsigned field width
// ("0..4294967295").
func uintRange(bits int) string {
	if bits >= 64 {
		return "0..18446744073709551615"
	}
	return "0.." + strconv.FormatUint(uint64(1)<<bits-1, 10)
}

// maxDurationSpelling is math.MaxInt64 nanoseconds in Go's duration spelling —
// the largest value config.ParseDuration can represent.
const maxDurationSpelling = "2562047h47m16.854775807s"

// Validate runs the full §5 validator WITHOUT saving and returns every error
// in the shape the setup API reports (empty = saveable). It returns the list,
// not one joined error, because §3.4 requires the 422 body to name each
// rejection separately. Each error also carries the schema Key its message
// names (G1: "a validation failure names the offending key", so the page can
// mark the row instead of printing one keyless line on all 145 rows).
func Validate(c *config.Config) []Error {
	_, errs := config.Validate(c)
	keys := schemaKeys(c)
	out := make([]Error, 0, len(errs))
	for _, e := range errs {
		msg := e.Error()
		out = append(out, Error{Key: KeyInMessage(msg, keys), Message: msg})
	}
	return out
}

// schemaKeys is every key GET /api/v1/setup advertises for c.
func schemaKeys(c *config.Config) []string {
	var keys []string
	for _, g := range BuildSchema(c, c) {
		for _, k := range g.Keys {
			keys = append(keys, k.Key)
		}
	}
	return keys
}

// KeyInMessage returns the schema key a validator message names, or "" when it
// names none. Matching is whole-key: the characters around the hit must not be
// key characters, so "bundles.strategy[0].name" matches bundles.strategy while
// "server.listener" matches nothing.
func KeyInMessage(msg string, keys []string) string {
	best := ""
	for _, key := range keys {
		if len(key) <= len(best) || !MessageNamesKey(msg, key) {
			continue
		}
		best = key
	}
	return best
}

// MessageNamesKey reports whether msg names key as a whole key (a character
// before or after the hit cannot continue the identifier).
func MessageNamesKey(msg, key string) bool {
	from := 0
	for {
		i := strings.Index(msg[from:], key)
		if i < 0 {
			return false
		}
		i += from
		beforeOK := i == 0 || !isKeyByte(msg[i-1])
		after := i + len(key)
		afterOK := after == len(msg) || !isKeyByte(msg[after])
		if beforeOK && afterOK {
			return true
		}
		from = i + 1
	}
}

func isKeyByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '.'
}

// Save validates (config.SaveSetup fails closed), then atomically writes
// <data-dir>/spindle.toml, serialized by the process-wide mutex (§3.4), and
// diffs the candidate against the still-running effective config. The
// returned keys are the ones whose effective value differs from the file just
// saved — the process does NOT hot-reload. An empty (non-nil) slice means
// nothing changed. backup names the spindle.toml.bak-<utc> file the first
// canonicalising rewrite of an existing config kept ("" when none was made).
func Save(dataDir string, c *config.Config, effective *config.Config) (restart []string, backup string, err error) {
	saveMu.Lock()
	backup, err = config.SaveSetup(c, dataDir)
	saveMu.Unlock()
	if err != nil {
		return nil, "", err
	}
	return RestartKeys(effective, c), backup, nil
}

// RestartKeys diffs the effective config against the candidate. It compares
// RAW field values (not the schema's masked row maps), so rotating a static
// token is reported even though both sides render the same SecretMask.
func RestartKeys(effective, candidate *config.Config) []string {
	groups := BuildSchema(candidate, effective)
	out := []string{}
	for _, g := range groups {
		for _, k := range g.Keys {
			a, aok := rawKeyValue(effective, k.Key)
			b, bok := rawKeyValue(candidate, k.Key)
			if !aok || !bok || fmt.Sprint(a) != fmt.Sprint(b) {
				out = append(out, k.Key)
			}
		}
	}
	return out
}

// rawKeyValue resolves one dotted schema key to its stored field value,
// unmasked (RestartKeys' comparison; nothing renders it).
func rawKeyValue(c *config.Config, key string) (any, bool) {
	if c == nil {
		return nil, false
	}
	fv, _, err := fieldByKey(reflect.ValueOf(c).Elem(), strings.Split(key, "."), "")
	if err != nil || !fv.CanInterface() {
		return nil, false
	}
	return fv.Interface(), true
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
