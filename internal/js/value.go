// Package js models JSON values the way the JavaScript runtime the server was first written for
// does: objects keep their key order (with integer-like keys first, ascending), numbers are
// float64 and print like JavaScript's String(n), and undefined is distinct from null. The tools
// reshape loosely typed Proxmox API data, so keeping these semantics keeps their output identical.
package js

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Undefined is JavaScript's undefined: a key holding it is present (for "in" checks) but is
// dropped when the object is serialised, and it is skipped when encoding request parameters.
type Undefined struct{}

// Undef is the undefined value.
var Undef = Undefined{}

// Obj is an ordered JSON object.
type Obj struct {
	keys []string
	vals map[string]any
}

// NewObj returns an empty object.
func NewObj() *Obj { return &Obj{vals: map[string]any{}} }

// O builds an object from alternating keys and values. Undefined values are kept (as present
// keys) so that helpers like withExtra see every key the tool sets; serialisation drops them.
func O(kv ...any) *Obj {
	o := &Obj{vals: make(map[string]any, len(kv)/2)}
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// Get returns the value of k, or Undef when k is absent. A nil *Obj behaves as an empty object.
func (o *Obj) Get(k string) any {
	if o == nil {
		return Undef
	}
	v, ok := o.vals[k]
	if !ok {
		return Undef
	}
	return v
}

// Has reports whether k is an own key (even one holding undefined), like Object.hasOwn.
func (o *Obj) Has(k string) bool {
	if o == nil {
		return false
	}
	_, ok := o.vals[k]
	return ok
}

// Set sets k, keeping its position when it already exists.
func (o *Obj) Set(k string, v any) *Obj {
	if o.vals == nil {
		o.vals = map[string]any{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = normalize(v)
	return o
}

// Delete removes k.
func (o *Obj) Delete(k string) {
	if o == nil {
		return
	}
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, x := range o.keys {
		if x == k {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			break
		}
	}
}

// Len is the number of own keys, including ones holding undefined (like Object.keys().length).
func (o *Obj) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// arrayIndex reports whether k is a canonical array index ("0", "17"; not "01" or "-1").
func arrayIndex(k string) (uint64, bool) {
	if k == "" || len(k) > 10 || (len(k) > 1 && k[0] == '0') {
		return 0, false
	}
	n, err := strconv.ParseUint(k, 10, 64)
	if err != nil || n >= math.MaxUint32 {
		return 0, false
	}
	return n, true
}

// Keys returns the keys in JavaScript property order: array indices ascending, then the rest in
// insertion order.
func (o *Obj) Keys() []string {
	if o == nil {
		return nil
	}
	var idx []string
	var rest []string
	for _, k := range o.keys {
		if _, ok := arrayIndex(k); ok {
			idx = append(idx, k)
		} else {
			rest = append(rest, k)
		}
	}
	if len(idx) == 0 {
		return append([]string(nil), o.keys...)
	}
	sort.Slice(idx, func(i, j int) bool {
		a, _ := arrayIndex(idx[i])
		b, _ := arrayIndex(idx[j])
		return a < b
	})
	return append(idx, rest...)
}

// Clone returns a shallow copy.
func (o *Obj) Clone() *Obj {
	c := &Obj{keys: append([]string(nil), o.Keys()...), vals: make(map[string]any, o.Len())}
	for _, k := range c.keys {
		c.vals[k] = o.vals[k]
	}
	return c
}

// Assign copies src's keys into o (like Object.assign / spread), returning o.
func (o *Obj) Assign(src *Obj) *Obj {
	for _, k := range src.Keys() {
		o.Set(k, src.vals[k])
	}
	return o
}

// Spread returns a new object with the keys of each object in turn, like {...a, ...b}. Nil and
// non-object values are skipped, as spread does.
func Spread(parts ...any) *Obj {
	out := NewObj()
	for _, p := range parts {
		if o, ok := p.(*Obj); ok && o != nil {
			out.Assign(o)
		}
	}
	return out
}

// Str returns the value of k as a string when it is one, else "".
func (o *Obj) Str(k string) string {
	s, _ := o.Get(k).(string)
	return s
}

// Num returns Number(o[k]).
func (o *Obj) Num(k string) float64 { return Num(o.Get(k)) }

// Bool returns the value of k when it is a boolean, else false.
func (o *Obj) Bool(k string) bool {
	b, _ := o.Get(k).(bool)
	return b
}

// Truthy reports whether o[k] is truthy.
func (o *Obj) Truthy(k string) bool { return Truthy(o.Get(k)) }

// Defined reports whether o[k] is neither undefined nor missing.
func (o *Obj) Defined(k string) bool { return !IsUndef(o.Get(k)) }

// Present reports whether o[k] is neither undefined nor null (the opposite of "?? fallback").
func (o *Obj) Present(k string) bool { return !Nullish(o.Get(k)) }

// Or returns o[k], or fallback when it is undefined or null (o[k] ?? fallback).
func (o *Obj) Or(k string, fallback any) any {
	v := o.Get(k)
	if Nullish(v) {
		return fallback
	}
	return v
}

// ObjAt returns o[k] when it is an object, else nil.
func (o *Obj) ObjAt(k string) *Obj {
	v, _ := o.Get(k).(*Obj)
	return v
}

// Arr returns o[k] when it is an array, else nil.
func (o *Obj) Arr(k string) []any {
	v, _ := o.Get(k).([]any)
	return v
}

// Objs returns the object elements of o[k] when it is an array.
func (o *Obj) Objs(k string) []*Obj { return Objs(o.Get(k)) }

// Strs returns the string elements of o[k] when it is an array.
func (o *Obj) Strs(k string) []string {
	var out []string
	for _, v := range o.Arr(k) {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Objs returns the object elements of v when it is an array (others become empty objects, so
// that property access on them behaves like on a JavaScript primitive: everything undefined).
func Objs(v any) []*Obj {
	arr, _ := v.([]any)
	out := make([]*Obj, 0, len(arr))
	for _, x := range arr {
		if o, ok := x.(*Obj); ok {
			out = append(out, o)
		} else {
			out = append(out, NewObj())
		}
	}
	return out
}

// Array converts objects to a generic array.
func Array[T any](items []T) []any {
	out := make([]any, len(items))
	for i, x := range items {
		out[i] = x
	}
	return out
}

// normalize turns Go values into the dynamic representation: numbers become float64, typed
// slices []any, and maps (with sorted keys) *Obj.
func normalize(v any) any {
	switch x := v.(type) {
	case nil, Undefined, string, bool, float64, *Obj, []any:
		return v
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	case uint64:
		return float64(x)
	case float32:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	case []string:
		return Array(x)
	case []*Obj:
		return Array(x)
	case []float64:
		return Array(x)
	case []int:
		out := make([]any, len(x))
		for i, n := range x {
			out[i] = float64(n)
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		o := NewObj()
		for _, k := range keys {
			o.Set(k, x[k])
		}
		return o
	case map[string]string:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		o := NewObj()
		for _, k := range keys {
			o.Set(k, x[k])
		}
		return o
	}
	return v
}

// V normalises a Go value (int, []string, map...) into the dynamic representation.
func V(v any) any { return normalize(v) }

// ---------------------------------------------------------------- parsing

// MaxDepth is how deeply objects and arrays may nest in Parse. Real Proxmox responses nest a
// few levels; the limit stops a hostile body from exhausting the stack.
const MaxDepth = 256

// Parse decodes JSON into *Obj, []any, float64, string, bool and nil.
func Parse(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := parseValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("invalid JSON: trailing data")
	}
	return v, nil
}

func parseValue(dec *json.Decoder, depth int) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if depth >= MaxDepth {
			return nil, fmt.Errorf("invalid JSON: nested more than %d levels deep", MaxDepth)
		}
		switch t {
		case '{':
			o := NewObj()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := parseValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				o.Set(k, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := parseValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("invalid JSON delimiter %v", t)
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil {
			// Out of range: JavaScript gives ±Infinity.
			if strings.HasPrefix(string(t), "-") {
				return math.Inf(-1), nil
			}
			return math.Inf(1), nil
		}
		return f, nil
	default:
		return t, nil // string, bool or nil
	}
}

// MustParse parses JSON or panics; for tests and fixtures.
func MustParse(s string) any {
	v, err := Parse([]byte(s))
	if err != nil {
		panic(err)
	}
	return v
}

// MustObj parses a JSON object or panics.
func MustObj(s string) *Obj { return MustParse(s).(*Obj) }

// MarshalJSON writes the object compactly, keys in JavaScript order.
func (o *Obj) MarshalJSON() ([]byte, error) {
	return []byte(Stringify(o, "")), nil
}

// UnmarshalJSON decodes an object preserving key order.
func (o *Obj) UnmarshalJSON(b []byte) error {
	v, err := Parse(b)
	if err != nil {
		return err
	}
	src, ok := v.(*Obj)
	if !ok {
		return fmt.Errorf("not a JSON object")
	}
	*o = *src
	return nil
}
