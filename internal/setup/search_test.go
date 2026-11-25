package setup

// search_test.go — the Configuration-page round trip for the [search] section
// (docs/features/20_search.md §5.2, docs/go/11_config_cli.md). The enabled knob
// is a *bool: the page renders the effective value as a switch, an untouched
// save is a byte-identical no-op (the pointer compare is by value, not
// address), and an explicit false is written as false.

import (
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
)

func TestSearchSchemaRoundTrip(t *testing.T) {
	base := roundTripConfig(t)
	base.Search = config.Search{Enabled: config.Bool(true), MaxConcurrent: 4}
	groups := BuildSchema(base, config.Defaults())
	for _, key := range []string{"search.enabled", "search.max_concurrent"} {
		f, err := findField(groups, key)
		if err != nil {
			t.Fatalf("%s missing from the schema: %v", key, err)
		}
		if key == "search.enabled" && f.Type != "bool" {
			t.Fatalf("search.enabled type = %q, want bool (the pointer is a rendering detail)", f.Type)
		}
	}
	values := FormValues(groups)
	if got := values.Get("search.enabled"); got != "true" {
		t.Fatalf("rendered search.enabled = %q, want true", got)
	}
	if got := values.Get("search.max_concurrent"); got != "4" {
		t.Fatalf("rendered search.max_concurrent = %q, want 4", got)
	}

	// Untouched: no change, no restart keys (the *bool compares by value).
	cand, err := ParseValues(values, cloneConfig(t, base))
	if err != nil {
		t.Fatalf("ParseValues: %v", err)
	}
	if !cand.Search.IsEnabled() || cand.Search.MaxConcurrent != 4 {
		t.Fatalf("round trip changed the section: %+v", cand.Search)
	}
	if keys := RestartKeys(base, cand); len(keys) != 0 {
		t.Fatalf("an untouched save reported restart keys %v", keys)
	}

	// The switch off is an explicit false, saved as one, and restart-required
	// (the service is built at boot with the request pool sized).
	edited := url.Values{}
	for k, v := range values {
		edited[k] = v
	}
	edited.Set("search.enabled", "false")
	edited.Set("search.max_concurrent", "1")
	cand, err = ParseValues(edited, cloneConfig(t, base))
	if err != nil {
		t.Fatalf("ParseValues edited: %v", err)
	}
	if cand.Search.IsEnabled() {
		t.Fatal("an explicit false stayed enabled")
	}
	if cand.Search.MaxConcurrent != 1 {
		t.Fatalf("max_concurrent = %d, want 1", cand.Search.MaxConcurrent)
	}
	keys := strings.Join(RestartKeys(base, cand), ",")
	for _, want := range []string{"search.enabled", "search.max_concurrent"} {
		if !strings.Contains(keys, want) {
			t.Fatalf("restart keys %q missing %s", keys, want)
		}
	}
}

// TestSearchDefaultsRenderEnabled pins the nil-means-enabled rule at the page:
// the compiled default is a set pointer, so the switch renders on rather than
// unchecked while search is actually available.
func TestSearchDefaultsRenderEnabled(t *testing.T) {
	groups := BuildSchema(config.Defaults(), config.Defaults())
	values := FormValues(groups)
	if got := values.Get("search.enabled"); got != "true" {
		t.Fatalf("compiled default renders search.enabled = %q, want true", got)
	}
	if got := values.Get("search.max_concurrent"); got != "2" {
		t.Fatalf("compiled default renders search.max_concurrent = %q, want 2", got)
	}
	// A hand-built config with the pointer nil is still enabled, and the page
	// shows the empty value (there is nothing to display) rather than a panic.
	if !(config.Search{}).IsEnabled() {
		t.Fatal("nil enabled must mean enabled")
	}
}

// TestSearchNilPointerSchema pins N3: a nil *bool is schema-visible (Value nil,
// type bool, compiled default true), renders as the blank form value, and then
// round-trips through ParseValues as still nil — the "blank cell means unset"
// rule of Coerce (N1). This also covers fmtAny's and rawKeyValue's nil-pointer
// branches, which no Defaults()-built config reaches.
func TestSearchNilPointerSchema(t *testing.T) {
	base := roundTripConfig(t)
	base.Search = config.Search{} // Enabled nil (= enabled), MaxConcurrent 0
	groups := BuildSchema(base, config.Defaults())

	f, err := findField(groups, "search.enabled")
	if err != nil {
		t.Fatalf("search.enabled missing from the schema: %v", err)
	}
	if f.Value != nil {
		t.Fatalf("schema value for a nil *bool = %#v, want nil", f.Value)
	}
	if f.Type != "bool" {
		t.Fatalf("type = %q, want bool", f.Type)
	}
	if d, ok := f.Default.(bool); !ok || !d {
		t.Fatalf("default = %#v, want true (the compiled default)", f.Default)
	}

	values := FormValues(groups)
	if got := values.Get("search.enabled"); got != "" {
		t.Fatalf("rendered search.enabled = %q, want the blank (unset) spelling", got)
	}
	if got := values.Get("search.max_concurrent"); got != "0" {
		t.Fatalf("rendered search.max_concurrent = %q, want 0", got)
	}

	// cloneConfig marshals/re-decodes into Defaults(), so it cannot represent a
	// nil *bool (the absent key decodes as the compiled &true). Take the
	// independent copy from a default-shaped fixture and re-apply the nil
	// section afterwards.
	work := cloneConfig(t, roundTripConfig(t))
	work.Search = config.Search{}
	cand, err := ParseValues(values, work)
	if err != nil {
		t.Fatalf("ParseValues over a nil *bool: %v", err)
	}
	if cand.Search.Enabled != nil {
		t.Fatalf("a blank cell materialized the pointer: %v", *cand.Search.Enabled)
	}
	if cand.Search.MaxConcurrent != 0 {
		t.Fatalf("max_concurrent = %d, want 0", cand.Search.MaxConcurrent)
	}
	if keys := RestartKeys(base, cand); len(keys) != 0 {
		t.Fatalf("an untouched nil-pointer save reported restart keys %v", keys)
	}
}

// TestCoercePointerBool drives Coerce's optional-scalar arm directly (N1): a
// blank cell leaves the field as it is (nil stays nil, an explicit value is not
// cleared), true/false materialize the pointer, a bad token names the key, and
// a pointer to a non-bool type is still refused.
func TestCoercePointerBool(t *testing.T) {
	var s config.Search
	fv := reflect.ValueOf(&s).Elem().FieldByName("Enabled")

	if err := Coerce(fv, "", "search.enabled"); err != nil {
		t.Fatalf("blank: %v", err)
	}
	if s.Enabled != nil {
		t.Fatal("a blank cell must leave a nil pointer nil")
	}
	if err := Coerce(fv, "true", "search.enabled"); err != nil {
		t.Fatalf("true: %v", err)
	}
	if s.Enabled == nil || !*s.Enabled {
		t.Fatal("true did not materialize the pointer")
	}
	if err := Coerce(fv, "false", "search.enabled"); err != nil {
		t.Fatalf("false: %v", err)
	}
	if s.Enabled == nil || *s.Enabled {
		t.Fatal("false did not materialize the pointer")
	}
	if err := Coerce(fv, "  ", "search.enabled"); err != nil {
		t.Fatalf("whitespace: %v", err)
	}
	if s.Enabled == nil || *s.Enabled {
		t.Fatal("a blank cell cleared an explicit value (it must keep it)")
	}

	var keyed Error
	if err := Coerce(fv, "yes", "search.enabled"); !errors.As(err, &keyed) || keyed.Key != "search.enabled" {
		t.Fatalf("bad token err = %v, want a keyed failure", err)
	}

	// No config field is a pointer to a non-bool today; the arm still refuses
	// one rather than panicking.
	var list *[]string
	lv := reflect.ValueOf(&list).Elem()
	if err := Coerce(lv, "x", "x.list"); !errors.As(err, &keyed) || keyed.Key != "x.list" {
		t.Fatalf("non-bool pointer err = %v, want a keyed unsupported-type failure", err)
	}
}

// TestSearchMaxConcurrentNamesItsKey drives the page's failure path: an
// out-of-range ceiling is a validation error carrying the row's key.
func TestSearchMaxConcurrentNamesItsKey(t *testing.T) {
	base := roundTripConfig(t)
	values := FormValues(BuildSchema(base, config.Defaults()))
	values.Set("search.max_concurrent", "99")
	cand, err := ParseValues(values, cloneConfig(t, base))
	if err != nil {
		t.Fatalf("ParseValues rejected the value before Validate: %v", err)
	}
	found := false
	for _, e := range Validate(cand) {
		if e.Key == "search.max_concurrent" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Validate did not report search.max_concurrent (got %v)", Validate(cand))
	}
}
