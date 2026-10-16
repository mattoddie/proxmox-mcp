package js

import (
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf16"
)

// IsUndef reports whether v is undefined.
func IsUndef(v any) bool {
	_, ok := v.(Undefined)
	return ok
}

// Nullish reports whether v is undefined or null.
func Nullish(v any) bool { return v == nil || IsUndef(v) }

// Truthy is JavaScript truthiness.
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil, Undefined:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0 && !math.IsNaN(x)
	case int:
		return x != 0
	case int64:
		return x != 0
	}
	return true
}

// Num is JavaScript's Number(v).
func Num(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case Undefined:
		return math.NaN()
	case bool:
		if x {
			return 1
		}
		return 0
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case string:
		return StrToNum(x)
	case []any:
		switch len(x) {
		case 0:
			return 0
		case 1:
			return Num(Str(x[0]))
		}
		return math.NaN()
	}
	return math.NaN()
}

// StrToNum converts a string like Number("...") does.
func StrToNum(s string) float64 {
	s = strings.TrimFunc(s, isJSSpace)
	if s == "" {
		return 0
	}
	switch s {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	if len(s) > 2 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			n, ok := new(big.Int).SetString(s[2:], base)
			if !ok {
				return math.NaN()
			}
			f, _ := new(big.Float).SetInt(n).Float64()
			return f
		}
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-') {
			return math.NaN()
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f
		}
		return math.NaN()
	}
	return f
}

func isJSSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0xa0, 0xfeff, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// ParseInt is Number.parseInt(s, 10): leading whitespace, an optional sign and the leading digits.
func ParseInt(s string) float64 {
	s = strings.TrimLeftFunc(s, isJSSpace)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return math.NaN()
	}
	f, _ := strconv.ParseFloat(s[:end], 64)
	if neg {
		f = -f
	}
	return f
}

// ParseFloat is Number.parseFloat(s).
func ParseFloat(s string) float64 {
	s = strings.TrimLeftFunc(s, isJSSpace)
	if strings.HasPrefix(s, "Infinity") || strings.HasPrefix(s, "+Infinity") {
		return math.Inf(1)
	}
	if strings.HasPrefix(s, "-Infinity") {
		return math.Inf(-1)
	}
	// Longest prefix that parses as a decimal literal.
	end, i := 0, 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := false
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits = true
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			digits = true
		}
	}
	if !digits {
		return math.NaN()
	}
	end = i
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		k := j
		for k < len(s) && s[k] >= '0' && s[k] <= '9' {
			k++
		}
		if k > j {
			end = k
		}
	}
	f, _ := strconv.ParseFloat(strings.TrimSuffix(s[:end], "."), 64)
	return f
}

// IsFinite is Number.isFinite for a number.
func IsFinite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// IsInteger is Number.isInteger.
func IsInteger(f float64) bool { return IsFinite(f) && f == math.Trunc(f) }

// Str is JavaScript's String(v).
func Str(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case Undefined:
		return "undefined"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return NumStr(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if !Nullish(e) {
				parts[i] = Str(e)
			}
		}
		return strings.Join(parts, ",")
	case *Obj:
		return "[object Object]"
	}
	return Str(normalize(v))
}

// NumStr formats a number like JavaScript's Number.prototype.toString().
func NumStr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	neg := f < 0
	if neg {
		f = -f
	}
	// Shortest round-tripping digits and exponent.
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expStr, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expStr)
	k := len(digits)
	n := exp + 1
	var s string
	switch {
	case k <= n && n <= 21:
		s = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		s = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		s = "0." + strings.Repeat("0", -n) + digits
	default:
		sign := "+"
		if n-1 < 0 {
			sign = "-"
		}
		ex := n - 1
		if ex < 0 {
			ex = -ex
		}
		if k == 1 {
			s = digits + "e" + sign + strconv.Itoa(ex)
		} else {
			s = digits[:1] + "." + digits[1:] + "e" + sign + strconv.Itoa(ex)
		}
	}
	if neg {
		return "-" + s
	}
	return s
}

// ToFixed is Number.prototype.toFixed(digits): exact decimal rounding, ties away from zero.
func ToFixed(f float64, digits int) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.Abs(f) >= 1e21 || math.IsInf(f, 0) {
		return NumStr(f)
	}
	neg := f < 0
	if neg {
		f = -f
	}
	exact := new(big.Float).SetPrec(2000).SetFloat64(f).Text('f', 1100)
	intPart, frac, _ := strings.Cut(exact, ".")
	frac += strings.Repeat("0", digits+1)
	keep := intPart + frac[:digits]
	roundUp := frac[digits] >= '5'
	n, _ := new(big.Int).SetString(keep, 10)
	if roundUp {
		n.Add(n, big.NewInt(1))
	}
	s := n.String()
	if digits > 0 {
		if len(s) <= digits {
			s = strings.Repeat("0", digits-len(s)+1) + s
		}
		s = s[:len(s)-digits] + "." + s[len(s)-digits:]
	}
	if neg && strings.Trim(s, "0.") != "" {
		s = "-" + s
	}
	return s
}

// Round is Math.round: to the nearest integer, ties towards +Infinity.
func Round(f float64) float64 {
	if !IsFinite(f) {
		return f
	}
	r := math.Floor(f)
	if f-r >= 0.5 {
		r++
	}
	return r
}

// Slice16 is String.prototype.slice(0, n) counted in UTF-16 code units (n >= 0).
func Slice16(s string, n int) string {
	units := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}

// Len16 is a string's length in UTF-16 code units.
func Len16(s string) int { return len(utf16.Encode([]rune(s))) }

// EncodeURIComponent is JavaScript's encodeURIComponent.
func EncodeURIComponent(s string) string {
	const unreserved = "-_.!~*'()"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%" + strings.ToUpper(strconv.FormatInt(int64(c)|0x100, 16)[1:]))
	}
	return b.String()
}

// LocaleCompare approximates String.prototype.localeCompare under the default (ICU root)
// collation for the ASCII names Proxmox uses: case-insensitive first, lower case before upper
// case on ties, then code point order.
func LocaleCompare(a, b string) int {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	if la != lb {
		if c := collateKey(la, lb); c != 0 {
			return c
		}
	}
	// Tertiary: lower case sorts before upper case.
	ra, rb := []rune(a), []rune(b)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		if ra[i] != rb[i] {
			lowA := ra[i] >= 'a' && ra[i] <= 'z'
			lowB := rb[i] >= 'a' && rb[i] <= 'z'
			if lowA && !lowB {
				return -1
			}
			if lowB && !lowA {
				return 1
			}
			if ra[i] < rb[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(ra) < len(rb):
		return -1
	case len(ra) > len(rb):
		return 1
	}
	return 0
}

// collateKey compares lower-cased strings with ICU's primary ordering for ASCII: punctuation
// and symbols, then digits, then letters.
func collateKey(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		if ra[i] == rb[i] {
			continue
		}
		wa, wb := weight(ra[i]), weight(rb[i])
		if wa != wb {
			if wa < wb {
				return -1
			}
			return 1
		}
		if ra[i] < rb[i] {
			return -1
		}
		return 1
	}
	switch {
	case len(ra) < len(rb):
		return -1
	case len(ra) > len(rb):
		return 1
	}
	return 0
}

func weight(r rune) int {
	switch {
	case r >= 'a' && r <= 'z':
		return 3
	case r >= '0' && r <= '9':
		return 2
	case r < 0x80:
		return 1
	}
	return 4
}
