package setup

// overflow_test.go — closing-audit Observation 3: a numeric value outside its
// field's range must be a KEYED setup.Error naming the key and the accepted
// range, never a wrapped/saturated value persisted as if it were valid. The
// reproduced bug: "99999999999999999999" in bundles.strategy#0.keep fell
// through strconv.ParseInt, parsed as a float, and int64(f) saturated to
// MinInt64 on amd64 while the save reported success.

import (
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/config"
)

// TestNumericCoercionNeverSaturates walks every numeric width and spelling.
// A boundary value still saves; one past it (or an unrepresentable,
// fractional, or wrong-sign value) is keyed, names its accepted range, and
// leaves the target field untouched.
func TestNumericCoercionNeverSaturates(t *testing.T) {
	int64Field := func(v int64) reflect.Value { return reflect.ValueOf(&v).Elem() }
	int32Field := func(v int32) reflect.Value { return reflect.ValueOf(&v).Elem() }
	int8Field := func(v int8) reflect.Value { return reflect.ValueOf(&v).Elem() }
	uint64Field := func(v uint64) reflect.Value { return reflect.ValueOf(&v).Elem() }
	uint32Field := func(v uint32) reflect.Value { return reflect.ValueOf(&v).Elem() }
	uint8Field := func(v uint8) reflect.Value { return reflect.ValueOf(&v).Elem() }
	sizeField := func() reflect.Value {
		c := config.Defaults()
		return reflect.ValueOf(&c.Server.MaxPushBytes).Elem()
	}
	durationField := func() reflect.Value {
		c := config.Defaults()
		return reflect.ValueOf(&c.Server.RequestTimeout).Elem()
	}
	float64Field := func() reflect.Value {
		c := config.Defaults()
		return reflect.ValueOf(&c.Cache.DiskHighWatermark).Elem()
	}
	float32Field := func(v float32) reflect.Value { return reflect.ValueOf(&v).Elem() }

	cases := []struct {
		name  string
		fv    func() reflect.Value
		raw   string
		after func(t *testing.T, v reflect.Value) // nil = must reject
	}{
		{"int64 MinInt64 saves", func() reflect.Value { return int64Field(0) }, "-9223372036854775808",
			func(t *testing.T, v reflect.Value) {
				if v.Int() != -9223372036854775808 {
					t.Fatalf("MinInt64 = %d", v.Int())
				}
			}},
		{"int64 MaxInt64 saves", func() reflect.Value { return int64Field(0) }, "9223372036854775807",
			func(t *testing.T, v reflect.Value) {
				if v.Int() != 9223372036854775807 {
					t.Fatalf("MaxInt64 = %d", v.Int())
				}
			}},
		{"int64 MaxInt64+1", func() reflect.Value { return int64Field(0) }, "9223372036854775808", nil},
		{"int64 huge digits (the reported bug)", func() reflect.Value { return int64Field(0) }, "99999999999999999999", nil},
		{"int64 huge scientific", func() reflect.Value { return int64Field(0) }, "1e30", nil},
		{"int64 negative huge", func() reflect.Value { return int64Field(0) }, "-99999999999999999999", nil},
		{"int64 fractional", func() reflect.Value { return int64Field(0) }, "1.5", nil},
		{"int32 MaxInt32 saves", func() reflect.Value { return int32Field(0) }, "2147483647",
			func(t *testing.T, v reflect.Value) {
				if v.Int() != 2147483647 {
					t.Fatalf("MaxInt32 = %d", v.Int())
				}
			}},
		{"int32 MinInt32 saves", func() reflect.Value { return int32Field(0) }, "-2147483648",
			func(t *testing.T, v reflect.Value) {
				if v.Int() != -2147483648 {
					t.Fatalf("MinInt32 = %d", v.Int())
				}
			}},
		{"int32 MaxInt32+1", func() reflect.Value { return int32Field(0) }, "2147483648", nil},
		{"int32 MinInt32-1", func() reflect.Value { return int32Field(0) }, "-2147483649", nil},
		{"int8 MaxInt8 saves", func() reflect.Value { return int8Field(0) }, "127",
			func(t *testing.T, v reflect.Value) {
				if v.Int() != 127 {
					t.Fatalf("int8 = %d", v.Int())
				}
			}},
		{"int8 MaxInt8+1", func() reflect.Value { return int8Field(0) }, "128", nil},
		{"uint64 MaxUint64 saves", func() reflect.Value { return uint64Field(0) }, "18446744073709551615",
			func(t *testing.T, v reflect.Value) {
				if v.Uint() != 18446744073709551615 {
					t.Fatalf("MaxUint64 = %d", v.Uint())
				}
			}},
		{"uint64 MaxUint64+1", func() reflect.Value { return uint64Field(0) }, "18446744073709551616", nil},
		{"uint64 negative", func() reflect.Value { return uint64Field(0) }, "-1", nil},
		{"uint64 fractional", func() reflect.Value { return uint64Field(0) }, "1.5", nil},
		{"uint32 MaxUint32 saves", func() reflect.Value { return uint32Field(0) }, "4294967295",
			func(t *testing.T, v reflect.Value) {
				if v.Uint() != 4294967295 {
					t.Fatalf("MaxUint32 = %d", v.Uint())
				}
			}},
		{"uint32 MaxUint32+1", func() reflect.Value { return uint32Field(0) }, "4294967296", nil},
		{"uint8 MaxUint8 saves", func() reflect.Value { return uint8Field(0) }, "255",
			func(t *testing.T, v reflect.Value) {
				if v.Uint() != 255 {
					t.Fatalf("uint8 = %d", v.Uint())
				}
			}},
		{"uint8 MaxUint8+1", func() reflect.Value { return uint8Field(0) }, "256", nil},
		{"size MaxInt64 bytes saves", sizeField, "9223372036854775807",
			func(t *testing.T, v reflect.Value) {
				if v.Int() != 9223372036854775807 {
					t.Fatalf("size = %d", v.Int())
				}
			}},
		{"size 2^63 bytes", sizeField, "9223372036854775808", nil},
		{"size huge unit", sizeField, "99999999999999999999999B", nil},
		{"size negative bare", sizeField, "-5", nil},
		{"duration max saves", durationField, "2562047h",
			func(t *testing.T, v reflect.Value) {
				if time.Duration(v.Int()) != 2562047*time.Hour {
					t.Fatalf("2562047h = %v", time.Duration(v.Int()))
				}
			}},
		{"duration overflowing weeks", durationField, "99999999999999w", nil},
		{"duration overflowing digits", durationField, "99999999999999999999s", nil},
		{"float64 overflow", float64Field, "1e400", nil},
		{"float32 overflow", func() reflect.Value { return float32Field(0) }, "1e40", nil},
		{"float64 NaN", float64Field, "NaN", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fv := tc.fv()
			before := reflect.New(fv.Type()).Elem()
			before.Set(fv)
			err := Coerce(fv, tc.raw, "test.key")
			if tc.after == nil {
				if err == nil {
					t.Fatalf("%q was accepted as %v", tc.raw, fv.Interface())
				}
				if got := KeyOf(err); got != "test.key" {
					t.Fatalf("%q: key = %q, want test.key (%v)", tc.raw, got, err)
				}
				if !strings.Contains(err.Error(), "accepted") && !strings.Contains(err.Error(), "range") &&
					!strings.Contains(err.Error(), "not a duration") {
					t.Fatalf("%q: message names no accepted range: %v", tc.raw, err)
				}
				if !reflect.DeepEqual(fv.Interface(), before.Interface()) {
					t.Fatalf("%q changed the field from %v to %v", tc.raw, before.Interface(), fv.Interface())
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: unexpected error %v", tc.raw, err)
			}
			tc.after(t, fv)
		})
	}
}

// TestStructSliceCellOverflowIsKeyedAndChangesNothing is the reproduced route:
// the row editor's keep cell. The failure is keyed to the cell, the base
// config is untouched, and the config file on disk is byte-identical (the
// save never runs when ParseValues fails).
func TestStructSliceCellOverflowIsKeyedAndChangesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spindle.toml")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	base := config.FirstRunDefaults(dir)
	base.Server.Auth.Issuer = "https://id.example.com"
	base.Server.Auth.AllowedDomains = []string{"example.com"}
	base.Server.Auth.OAuthClientID = "id"
	base.Server.Auth.OAuthClientSecret = "secret"
	base.Server.Auth.SessionSecret = "0123456789abcdef0123456789abcdef"
	base.Bundles.Strategy = []config.BundleStrategy{{Name: "weekly", Kind: "full", Schedule: "0 0 23 * * 0", Keep: 2}}
	if _, err := config.SaveSetup(base, dir); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	form := FormValues(BuildSchema(base, config.Defaults()))
	form.Set("bundles.strategy#0.keep", "99999999999999999999")
	cand, err := ParseValues(form, setupBaseForTest(dir))
	if err == nil {
		t.Fatalf("out-of-range keep saved: keep = %d", cand.Bundles.Strategy[0].Keep)
	}
	if got := KeyOf(err); got != "bundles.strategy[0].keep" {
		t.Fatalf("KeyOf = %q, want bundles.strategy[0].keep (%v)", got, err)
	}
	if !strings.Contains(err.Error(), "accepted range") {
		t.Fatalf("message names no range: %v", err)
	}
	after, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(after) != string(before) {
		t.Fatalf("file changed on a rejected save:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
	// The legitimate boundary still round-trips through the same form path.
	form.Set("bundles.strategy#0.keep", "9223372036854775807")
	cand, err = ParseValues(form, setupBaseForTest(dir))
	if err != nil {
		t.Fatalf("MaxInt64 keep rejected: %v", err)
	}
	if cand.Bundles.Strategy[0].Keep != 9223372036854775807 {
		t.Fatalf("MaxInt64 keep = %d", cand.Bundles.Strategy[0].Keep)
	}
}

// setupBaseForTest resolves the file-visible base the setup surface edits, the
// same call the page and API make.
func setupBaseForTest(dir string) *config.Config {
	return BaseConfig(dir, []string{filepath.Join(dir, "spindle.toml")})
}

// TestNumericBoundaryMessagesNameTheRange pins the operator-facing spelling of
// a rejected value for the widths the schema actually carries.
func TestNumericBoundaryMessagesNameTheRange(t *testing.T) {
	for _, tc := range []struct {
		key  string
		raw  string
		want string
	}{
		{"bundles.strategy#0.keep", "99999999999999999999", "accepted range -9223372036854775808..9223372036854775807"},
		{"wal.cas_max_retries", "4294967296", "accepted range 0..4294967295"},
		{"wal.snapshot_every_entries", "-1", "accepted range 0..18446744073709551615"},
		{"server.max_push_bytes", "99999999999999999999999B", "accepted 0..9223372036854775807 bytes"},
		{"server.request_timeout", "99999999999999w", "max 2562047h47m16.854775807s"},
	} {
		base := config.Defaults()
		base.Bundles.Strategy = []config.BundleStrategy{{Name: "weekly", Kind: "full", Schedule: "0 0 23 * * 0"}}
		_, err := ParseValues(url.Values{tc.key: {tc.raw}}, base)
		if err == nil {
			t.Fatalf("%s=%q accepted", tc.key, tc.raw)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s=%q: %v (want %q)", tc.key, tc.raw, err, tc.want)
		}
	}
}
