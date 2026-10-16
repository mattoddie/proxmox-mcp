package js

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Stringify is JSON.stringify(v, null, indent): object keys in JavaScript order, undefined
// properties dropped (undefined array elements become null), NaN and Infinity become null, and
// strings escaped the way JavaScript does. A top-level undefined gives "undefined".
func Stringify(v any, indent string) string {
	var b strings.Builder
	if !writeValue(&b, normalize(v), indent, "") {
		return "undefined"
	}
	return b.String()
}

func writeValue(b *strings.Builder, v any, indent, cur string) bool {
	switch x := v.(type) {
	case Undefined:
		return false
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			b.WriteString("null")
		} else {
			b.WriteString(NumStr(x))
		}
	case string:
		writeString(b, x)
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return true
		}
		inner := cur + indent
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if indent != "" {
				b.WriteString("\n" + inner)
			}
			if !writeValue(b, normalize(e), indent, inner) {
				b.WriteString("null")
			}
		}
		if indent != "" {
			b.WriteString("\n" + cur)
		}
		b.WriteByte(']')
	case *Obj:
		if x == nil {
			b.WriteString("null")
			return true
		}
		inner := cur + indent
		wrote := false
		b.WriteByte('{')
		for _, k := range x.Keys() {
			val := x.vals[k]
			if IsUndef(val) {
				continue
			}
			if wrote {
				b.WriteByte(',')
			}
			if indent != "" {
				b.WriteString("\n" + inner)
			}
			writeString(b, k)
			b.WriteByte(':')
			if indent != "" {
				b.WriteByte(' ')
			}
			writeValue(b, val, indent, inner)
			wrote = true
		}
		if wrote && indent != "" {
			b.WriteString("\n" + cur)
		}
		b.WriteByte('}')
	default:
		n := normalize(v)
		if _, same := n.(Undefined); !same {
			switch n.(type) {
			case nil, bool, float64, string, []any, *Obj:
				return writeValue(b, n, indent, cur)
			}
		}
		// Anything else (a Go struct): via encoding/json, then re-parsed for ordering and format.
		raw, err := json.Marshal(v)
		if err != nil {
			b.WriteString("null")
			return true
		}
		p, err := Parse(raw)
		if err != nil {
			b.WriteString("null")
			return true
		}
		return writeValue(b, p, indent, cur)
	}
	return true
}

const hexDigits = "0123456789abcdef"

func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				b.WriteString(`\"`)
			case '\\':
				b.WriteString(`\\`)
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				if c < 0x20 {
					b.WriteString(`\u00`)
					b.WriteByte(hexDigits[c>>4])
					b.WriteByte(hexDigits[c&0xf])
				} else {
					b.WriteByte(c)
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// Invalid UTF-8 can't come from a JavaScript string; show it as U+FFFD.
			b.WriteString("�")
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
}

// Quote is JSON.stringify of a string.
func Quote(s string) string {
	var b strings.Builder
	writeString(&b, s)
	return b.String()
}

// FormatInt formats an integer-valued float without exponent notation, for building paths.
func FormatInt(f float64) string {
	if IsInteger(f) && math.Abs(f) < 1e21 {
		return strconv.FormatFloat(f, 'f', 0, 64)
	}
	return NumStr(f)
}
