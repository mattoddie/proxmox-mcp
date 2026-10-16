package tools

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/mattoddie/proxmox-mcp/internal/js"
)

// Schema describes a tool argument, mirroring the zod schemas of the original TypeScript server:
// it renders the same JSON Schema and validates arguments with the same rules and messages.
type Schema struct {
	kind     string // string, number, integer, boolean, enum, array, record, object, union
	desc     string
	optional bool

	enum []string

	minimum, maximum, exclusiveMin *float64

	minLen, maxLen     *int
	pattern            *regexp.Regexp
	patternSrc, patMsg string

	minItems, maxItems *int
	items              *Schema

	keys   *Schema // record key schema (a string, maybe with a pattern)
	values *Schema // record value schema

	props []Prop // object properties

	anyOf []*Schema
}

// Prop is a named property of an object schema.
type Prop struct {
	Name   string
	Schema *Schema
}

const maxSafeInt = 9007199254740991

func f64(f float64) *float64 { return &f }
func intp(n int) *int        { return &n }

// Str is z.string().
func Str() *Schema { return &Schema{kind: "string"} }

// Num is z.number().
func Num() *Schema { return &Schema{kind: "number"} }

// Int is z.number().int().
func Int() *Schema {
	return &Schema{kind: "integer", minimum: f64(-maxSafeInt), maximum: f64(maxSafeInt)}
}

// Bool is z.boolean().
func Bool() *Schema { return &Schema{kind: "boolean"} }

// Enum is z.enum([...]). zod keeps the values as the keys of an object, so integer-like values
// ("3") come first, in numeric order, as JavaScript orders object keys.
func Enum(values ...string) *Schema {
	o := js.NewObj()
	for _, v := range values {
		o.Set(v, true)
	}
	return &Schema{kind: "enum", enum: o.Keys()}
}

// Arr is z.array(items).
func Arr(items *Schema) *Schema { return &Schema{kind: "array", items: items} }

// Rec is z.record(z.string(), values).
func Rec(values *Schema) *Schema { return &Schema{kind: "record", keys: Str(), values: values} }

// RecKeys is z.record(keys, values) with a key schema such as Str().Pattern(...).
func RecKeys(keys, values *Schema) *Schema {
	return &Schema{kind: "record", keys: keys, values: values}
}

// Object is z.object({...}).
func Object(props ...Prop) *Schema { return &Schema{kind: "object", props: props} }

// P is a property for Object.
func P(name string, s *Schema) Prop { return Prop{name, s} }

// Union is z.union([...]).
func Union(options ...*Schema) *Schema { return &Schema{kind: "union", anyOf: options} }

func (s *Schema) clone() *Schema { c := *s; return &c }

// D sets the description (.describe()).
func (s *Schema) D(desc string) *Schema { c := s.clone(); c.desc = desc; return c }

// Opt makes the argument optional (.optional()).
func (s *Schema) Opt() *Schema { c := s.clone(); c.optional = true; return c }

// Min is .min(n): a minimum value for numbers, length for strings, items for arrays.
func (s *Schema) Min(n float64) *Schema {
	c := s.clone()
	switch c.kind {
	case "string":
		c.minLen = intp(int(n))
	case "array":
		c.minItems = intp(int(n))
	default:
		c.minimum, c.exclusiveMin = f64(n), nil
	}
	return c
}

// Max is .max(n).
func (s *Schema) Max(n float64) *Schema {
	c := s.clone()
	switch c.kind {
	case "string":
		c.maxLen = intp(int(n))
	case "array":
		c.maxItems = intp(int(n))
	default:
		c.maximum = f64(n)
	}
	return c
}

// Pos is .positive().
func (s *Schema) Pos() *Schema { c := s.clone(); c.exclusiveMin, c.minimum = f64(0), nil; return c }

// Pattern is .regex(re[, message]).
func (s *Schema) Pattern(re string, message ...string) *Schema {
	c := s.clone()
	c.pattern = regexp.MustCompile(re)
	c.patternSrc = re
	if len(message) > 0 {
		c.patMsg = message[0]
	}
	return c
}

// ---------------------------------------------------------------- JSON Schema

// rendering builds the schema as an ordered object, like zod's toJSONSchema.
func (s *Schema) render() *js.Obj {
	o := js.NewObj()
	if s.desc != "" {
		o.Set("description", s.desc)
	}
	switch s.kind {
	case "string":
		o.Set("type", "string")
		if s.minLen != nil {
			o.Set("minLength", *s.minLen)
		}
		if s.maxLen != nil {
			o.Set("maxLength", *s.maxLen)
		}
		if s.pattern != nil {
			o.Set("pattern", s.patternSrc)
		}
	case "number", "integer":
		o.Set("type", s.kind)
		if s.exclusiveMin != nil {
			o.Set("exclusiveMinimum", *s.exclusiveMin)
		}
		if s.minimum != nil {
			o.Set("minimum", *s.minimum)
		}
		if s.maximum != nil {
			o.Set("maximum", *s.maximum)
		}
	case "boolean":
		o.Set("type", "boolean")
	case "enum":
		o.Set("type", "string")
		o.Set("enum", js.Array(s.enum))
	case "array":
		o.Set("type", "array")
		if s.minItems != nil {
			o.Set("minItems", *s.minItems)
		}
		if s.maxItems != nil {
			o.Set("maxItems", *s.maxItems)
		}
		o.Set("items", s.items.render())
	case "record":
		o.Set("type", "object")
		o.Set("propertyNames", s.keys.render())
		o.Set("additionalProperties", s.values.render())
	case "object":
		o.Set("type", "object")
		props := js.NewObj()
		var required []any
		for _, p := range s.props {
			props.Set(p.Name, p.Schema.render())
			if !p.Schema.optional {
				required = append(required, p.Name)
			}
		}
		o.Set("properties", props)
		if len(required) > 0 {
			o.Set("required", required)
		}
	case "union":
		// Unions of plain primitive types collapse into a type list, as zod renders them.
		var types []any
		for _, opt := range s.anyOf {
			r := opt.render()
			if r.Len() != 1 || !r.Has("type") {
				types = nil
				break
			}
			t, ok := r.Get("type").(string)
			if !ok {
				types = nil
				break
			}
			types = append(types, t)
		}
		if types != nil {
			o.Set("type", types)
		} else {
			options := make([]any, len(s.anyOf))
			for i, opt := range s.anyOf {
				options[i] = opt.render()
			}
			o.Set("anyOf", options)
		}
	}
	return o
}

// InputSchema renders the top-level schema of a tool's arguments.
func InputSchema(props []Prop) *js.Obj {
	o := Object(props...).render()
	o.Set("$schema", "http://json-schema.org/draft-07/schema#")
	return o
}

// ---------------------------------------------------------------- validation

// issue is a validation problem at a path, worded like zod's messages.
type issue struct {
	path []string
	msg  string
}

func typeName(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case js.Undefined:
		return "undefined"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		if math.IsNaN(x) {
			return "NaN"
		}
		return "number"
	case []any:
		return "array"
	case *js.Obj:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func fmtNum(f float64) string { return js.NumStr(f) }

// parse validates v and returns the parsed value (objects keep only their declared properties,
// in declaration order), or the issues found.
func (s *Schema) parse(v any, path []string) (any, []issue) {
	at := func(msg string) []issue { return []issue{{append([]string(nil), path...), msg}} }
	if js.IsUndef(v) {
		if s.optional {
			return js.Undef, nil
		}
	}
	switch s.kind {
	case "string":
		str, ok := v.(string)
		if !ok {
			return nil, at("Invalid input: expected string, received " + typeName(v))
		}
		var issues []issue
		n := js.Len16(str)
		if s.minLen != nil && n < *s.minLen {
			issues = append(issues, at(fmt.Sprintf("Too small: expected string to have >=%d characters", *s.minLen))...)
		}
		if s.maxLen != nil && n > *s.maxLen {
			issues = append(issues, at(fmt.Sprintf("Too big: expected string to have <=%d characters", *s.maxLen))...)
		}
		if s.pattern != nil && !s.pattern.MatchString(str) {
			msg := s.patMsg
			if msg == "" {
				msg = "Invalid string: must match pattern /" + s.patternSrc + "/"
			}
			issues = append(issues, at(msg)...)
		}
		return str, issues
	case "number", "integer":
		f, ok := v.(float64)
		if !ok || math.IsNaN(f) {
			return nil, at("Invalid input: expected number, received " + typeName(v))
		}
		if s.kind == "integer" && (!js.IsInteger(f) || math.Abs(f) > maxSafeInt) {
			return nil, at("Invalid input: expected int, received number")
		}
		var issues []issue
		if s.exclusiveMin != nil && !(f > *s.exclusiveMin) {
			issues = append(issues, at("Too small: expected number to be >"+fmtNum(*s.exclusiveMin))...)
		}
		if s.minimum != nil && f < *s.minimum {
			issues = append(issues, at("Too small: expected number to be >="+fmtNum(*s.minimum))...)
		}
		if s.maximum != nil && f > *s.maximum {
			issues = append(issues, at("Too big: expected number to be <="+fmtNum(*s.maximum))...)
		}
		return f, issues
	case "boolean":
		b, ok := v.(bool)
		if !ok {
			return nil, at("Invalid input: expected boolean, received " + typeName(v))
		}
		return b, nil
	case "enum":
		str, ok := v.(string)
		if ok {
			for _, e := range s.enum {
				if e == str {
					return str, nil
				}
			}
		}
		quoted := make([]string, len(s.enum))
		for i, e := range s.enum {
			quoted[i] = js.Quote(e)
		}
		return nil, at("Invalid option: expected one of " + strings.Join(quoted, "|"))
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return nil, at("Invalid input: expected array, received " + typeName(v))
		}
		var issues []issue
		out := make([]any, len(arr))
		for i, e := range arr {
			p, is := s.items.parse(e, append(path, fmt.Sprint(i)))
			out[i] = p
			issues = append(issues, is...)
		}
		if s.minItems != nil && len(arr) < *s.minItems {
			issues = append(issues, at(fmt.Sprintf("Too small: expected array to have >=%d items", *s.minItems))...)
		}
		if s.maxItems != nil && len(arr) > *s.maxItems {
			issues = append(issues, at(fmt.Sprintf("Too big: expected array to have <=%d items", *s.maxItems))...)
		}
		return out, issues
	case "record":
		obj, ok := v.(*js.Obj)
		if !ok {
			return nil, at("Invalid input: expected record, received " + typeName(v))
		}
		var issues []issue
		out := js.NewObj()
		for _, k := range obj.Keys() {
			if _, kis := s.keys.parse(k, append(path, k)); len(kis) > 0 {
				issues = append(issues, issue{append(append([]string(nil), path...), k), "Invalid key in record"})
				continue
			}
			p, is := s.values.parse(obj.Get(k), append(path, k))
			issues = append(issues, is...)
			out.Set(k, p)
		}
		return out, issues
	case "object":
		obj, ok := v.(*js.Obj)
		if !ok {
			return nil, at("Invalid input: expected object, received " + typeName(v))
		}
		var issues []issue
		out := js.NewObj()
		for _, p := range s.props {
			val := obj.Get(p.Name)
			parsed, is := p.Schema.parse(val, append(path, p.Name))
			issues = append(issues, is...)
			if obj.Has(p.Name) || !js.IsUndef(parsed) {
				out.Set(p.Name, parsed)
			}
		}
		return out, issues
	case "union":
		var all []issue
		for _, opt := range s.anyOf {
			o := opt.clone()
			o.optional = false
			p, is := o.parse(v, path)
			if len(is) == 0 {
				return p, nil
			}
			all = append(all, is...)
		}
		return nil, at("Invalid input")
	}
	return v, nil
}

// formatIssues joins issues as "message at path" lines, like the original server's errors.
func formatIssues(issues []issue) string {
	lines := make([]string, len(issues))
	for i, is := range issues {
		lines[i] = is.msg
		if len(is.path) > 0 {
			lines[i] += " at " + strings.Join(is.path, ".")
		}
	}
	return strings.Join(lines, "\n")
}

// ParseArgs validates a tool's arguments against its properties. Missing optional arguments are
// left out; unknown arguments are dropped.
func ParseArgs(props []Prop, args *js.Obj) (*js.Obj, error) {
	if args == nil {
		args = js.NewObj()
	}
	v, issues := Object(props...).parse(args, nil)
	if len(issues) > 0 {
		return nil, fmt.Errorf("%s", formatIssues(issues))
	}
	return v.(*js.Obj), nil
}
