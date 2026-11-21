package setup

// Struct-slice rows — the repeated-row editor shared by /setup, PUT
// /api/v1/setup and the whole-schema round-trip test.
//
// A struct slice (server.auth.tokens → []StaticToken, bundles.strategy →
// []BundleStrategy) is edited as repeating rows of the element's toml fields,
// never as raw TOML. The page posts, for a field key K:
//
//	K#<i>.<tomlname>   one cell of row i (what the operator typed);
//	K#<i>.__base       the stored index the row was rendered from ("" = new);
//	K#<i>.__remove     "true" drops row i.
//
// setup.ParseValues rebuilds the slice in posted order through Coerce — the
// same shared parsers every scalar key uses — so add, edit, rotate and revoke
// are one code path. A row whose non-boolean cells are all empty is the
// "add a row" placeholder and is ignored. When no row key is posted at all the
// stored slice is left untouched, so an untouched field re-encodes byte for
// byte (a nil slice keeps encoding to nothing).
//
// Secrets inside an element are masked exactly like scalar secrets: the
// rendered cell carries SecretMask, posting the mask (or an empty cell) keeps
// the stored value for that row — selected by __base, NOT by position, so
// deleting row 0 cannot hand row 1 the deleted row's credential.

import (
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"
)

// RowCell is one editable cell of a struct-slice row.
type RowCell struct {
	Name   string // element toml field name ("name", "token")
	Key    string // full form key, e.g. "bundles.strategy#0.name"
	Label  string // the cell caption ("Token env" from the Go field name)
	Type   string // string|int|bool|duration|size|list
	Value  string // display spelling; a secret cell is always SecretMask
	Secret bool
	Bool   bool
}

// Row is one rendered struct-slice element.
type Row struct {
	Index     int    // row index in the rendered form
	Base      int    // stored index it was rendered from; -1 for the add row
	BaseKey   string // K#<index>.__base
	Caption   string // the row's element name ("alice@…", "daily", "new row")
	Cells     []RowCell
	RemoveKey string // K#<index>.__remove
}

// RowCellKey is the form name of one cell.
func RowCellKey(key string, i int, name string) string {
	return fmt.Sprintf("%s#%d.%s", key, i, name)
}

// RowBaseKey is the form name carrying a row's stored index.
func RowBaseKey(key string, i int) string { return fmt.Sprintf("%s#%d.__base", key, i) }

// RowRemoveKey is the form name of a row's "remove" checkbox.
func RowRemoveKey(key string, i int) string { return fmt.Sprintf("%s#%d.__remove", key, i) }

// RenderRows renders one struct-slice field's current value as editable rows,
// plus one blank "add a row" row. Cells are derived from Field.Fields (the
// element schema appendSchemaKeys recorded), so the editor never invents a
// field the type does not define.
func RenderRows(f Field) []Row { return renderRows(f, f.Value) }

func renderRows(f Field, value any) []Row {
	items := structSliceValue(value)
	out := make([]Row, 0, len(items)+1)
	for i, m := range items {
		out = append(out, renderRow(f, i, i, m))
	}
	// The add row: same cells, empty, never an element of the stored slice.
	out = append(out, renderRow(f, len(items), -1, nil))
	return out
}

func renderRow(f Field, index, base int, values map[string]any) Row {
	row := Row{
		Index: index, Base: base,
		BaseKey: RowBaseKey(f.Key, index), RemoveKey: RowRemoveKey(f.Key, index),
	}
	for _, sub := range f.Fields {
		cell := RowCell{
			Name: sub.Key, Key: RowCellKey(f.Key, index, sub.Key),
			Label: humanFieldName(sub.Doc), Type: sub.Type, Secret: sub.Secret,
		}
		v := values[sub.Key]
		if sub.Secret {
			if DisplayValue(Field{Type: sub.Type, Value: v}) != "" {
				cell.Value = SecretMask
			}
		} else {
			cell.Value = DisplayValue(Field{Type: sub.Type, DisplayType: sub.DisplayType, Value: v})
		}
		cell.Bool = sub.Type == "bool" && strings.EqualFold(cell.Value, "true")
		row.Cells = append(row.Cells, cell)
		if row.Caption == "" && (sub.Key == "name" || sub.Key == "principal") {
			row.Caption = cell.Value
		}
	}
	if row.Caption == "" && base < 0 {
		row.Caption = "new row"
	}
	return row
}

// humanFieldName turns a Go field name into the row caption ("TokenEnv" →
// "Token env"), so the editor reads as a form without a second label table.
func humanFieldName(name string) string {
	var b strings.Builder
	for i, r := range name {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte(' ')
			b.WriteRune(r - 'A' + 'a')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// RowsDiffer reports whether the field's current value differs from its
// default (the page's file/default signal for a row editor).
func RowsDiffer(f Field) bool {
	return fmt.Sprint(renderRows(f, f.Value)) != fmt.Sprint(renderRows(f, f.Default))
}

// structSliceValue unwraps the masked []map[string]any fmtAny produces for a
// struct slice.
func structSliceValue(v any) []map[string]any {
	switch x := v.(type) {
	case []map[string]any:
		return x
	}
	return nil
}

// DisplayValue renders one schema value in a spelling Coerce accepts: the
// same string the settings page posts back. Durations stay in the API's
// Duration spelling ("1h0m0s") and sizes in bare bytes; the page layer then
// applies its spec spelling ("1h", "64GiB") for display, which Coerce also
// accepts.
func DisplayValue(f Field) string {
	switch x := f.Value.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case uint:
		return strconv.FormatUint(uint64(x), 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case uint32:
		return strconv.FormatUint(uint64(x), 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case []string:
		return strings.Join(x, ", ")
	default:
		return ""
	}
}

// RowEdit is one row exactly as the form posted it, independent of the
// stored value: the parser ParseValues and the rejected-form re-render share.
type RowEdit struct {
	Index  int
	Base   int // stored index from __base; -1 = new row
	Remove bool
	Cells  map[string]string // toml field name → typed value
}

// ParseRowEdits extracts the posted rows of field key from a flat form map
// (keys already stripped of the "cfg." prefix), ordered by row index.
func ParseRowEdits(key string, values map[string]string) []RowEdit {
	prefix := key + "#"
	byIndex := map[int]*RowEdit{}
	max := -1
	for k, raw := range values {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		dot := strings.IndexByte(rest, '.')
		if dot <= 0 {
			continue // K#rows or a malformed segment: not a cell
		}
		i, err := strconv.Atoi(rest[:dot])
		if err != nil || i < 0 {
			continue
		}
		name := rest[dot+1:]
		edit := byIndex[i]
		if edit == nil {
			edit = &RowEdit{Index: i, Base: -1, Cells: map[string]string{}}
			byIndex[i] = edit
		}
		switch name {
		case "__remove":
			if truthy(raw) {
				edit.Remove = true
			}
		case "__base":
			if n, cerr := strconv.Atoi(strings.TrimSpace(raw)); cerr == nil && n >= 0 {
				edit.Base = n
			}
		default:
			edit.Cells[name] = raw
		}
		if i > max {
			max = i
		}
	}
	out := make([]RowEdit, 0, max+1)
	for i := 0; i <= max; i++ {
		if edit := byIndex[i]; edit != nil {
			out = append(out, *edit)
		}
	}
	return out
}

func truthy(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "on", "yes":
		return true
	}
	return false
}

// rowIsBlank reports whether an edit is the untouched "add a row" placeholder:
// every non-boolean cell empty and every boolean cell false. A new row with
// only a boolean flipped is NOT blank — the validator gets to speak about it.
func rowIsBlank(elemType reflect.Type, cells map[string]string) bool {
	nonBlank := false
	for j := 0; j < elemType.NumField(); j++ {
		sf := elemType.Field(j)
		name := tomlName(sf.Tag.Get("toml"))
		if name == "" || name == "-" {
			continue
		}
		raw := strings.TrimSpace(cells[name])
		if raw == "" {
			continue
		}
		if sf.Type.Kind() == reflect.Bool && !truthy(raw) {
			continue
		}
		nonBlank = true
	}
	return !nonBlank
}

// FormValues renders a whole schema as the form map an untouched /setup page
// posts (the keys POST /setup hands to ParseValues, prefix already stripped).
// Secrets render as SecretMask and are kept by ParseValues, so the round-trip
// this feeds is the same one the page performs.
func FormValues(groups []Group) url.Values {
	v := url.Values{}
	for _, g := range groups {
		for _, f := range g.Keys {
			if len(f.Fields) > 0 {
				for _, row := range RenderRows(f) {
					v.Set(RowBaseKey(f.Key, row.Index), "")
					if row.Base >= 0 {
						v.Set(RowBaseKey(f.Key, row.Index), strconv.Itoa(row.Base))
					}
					for _, c := range row.Cells {
						v.Set(c.Key, c.Value)
					}
				}
				continue
			}
			if f.Secret && DisplayValue(f) != "" {
				v.Set(f.Key, SecretMask)
				continue
			}
			v.Set(f.Key, DisplayValue(f))
		}
	}
	return v
}
