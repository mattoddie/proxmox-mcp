package js

import (
	"math"
	"strings"
	"testing"
)

func TestNumStr(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, "0"}, {1, "1"}, {-1.5, "-1.5"}, {0.30000000000000004, "0.30000000000000004"}, {1e21, "1e+21"}, {123456789012345680000, "123456789012345680000"},
		{1e-7, "1e-7"}, {0.000001, "0.000001"}, {17179869184, "17179869184"}, {42.3, "42.3"}, {1.5e300, "1.5e+300"},
		{math.NaN(), "NaN"}, {math.Inf(-1), "-Infinity"}, {9007199254740991, "9007199254740991"},
	} {
		if got := NumStr(tc.in); got != tc.want {
			t.Errorf("NumStr(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestToFixed(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		d    int
		want string
	}{
		{1.5, 2, "1.50"}, {1.125, 2, "1.13"}, {2.5, 0, "3"}, {1.005, 2, "1.00"}, {0, 1, "0.0"}, {99.95, 1, "100.0"}, {-1.5, 0, "-2"}, {123.456, 0, "123"},
	} {
		if got := ToFixed(tc.in, tc.d); got != tc.want {
			t.Errorf("ToFixed(%v, %d) = %q, want %q", tc.in, tc.d, got, tc.want)
		}
	}
}

func TestRoundAndParse(t *testing.T) {
	if Round(2.5) != 3 || Round(-2.5) != -2 || Round(0.49) != 0 {
		t.Error("Round")
	}
	if ParseInt(" 30s") != 30 || !math.IsNaN(ParseInt("x")) || ParseInt("-4") != -4 {
		t.Error("ParseInt")
	}
	if Num("") != 0 || Num(" 12 ") != 12 || !math.IsNaN(Num("12px")) || Num(nil) != 0 || !math.IsNaN(Num(Undef)) || Num("0x1f") != 31 || Num(true) != 1 {
		t.Error("Num")
	}
	if ParseFloat("1.5GB") != 1.5 || !math.IsNaN(ParseFloat("abc")) {
		t.Error("ParseFloat")
	}
}

func TestObjOrderAndStringify(t *testing.T) {
	o := MustObj(`{"b":1,"10":2,"a":{"x":[1,"s",null,true]},"2":3,"u":"é\u0001\"\\</"}`)
	o.Set("undef", Undef)
	if got, want := Stringify(o, ""), `{"2":3,"10":2,"b":1,"a":{"x":[1,"s",null,true]},"u":"é\u0001\"\\</"}`; got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	if got, want := Stringify(O("a", []any{}, "b", NewObj(), "c", []any{Undef}), "  "), "{\n  \"a\": [],\n  \"b\": {},\n  \"c\": [\n    null\n  ]\n}"; got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	if Stringify(Undef, "  ") != "undefined" || Stringify(math.NaN(), "") != "null" {
		t.Error("top-level undefined/NaN")
	}
	if !o.Has("undef") || o.Defined("undef") || o.Len() != 6 {
		t.Error("undefined keys count as own keys")
	}
}

func TestStrAndEncode(t *testing.T) {
	if Str([]any{1.0, "a", nil}) != "1,a," || Str(O()) != "[object Object]" || Str(Undef) != "undefined" {
		t.Error("Str")
	}
	if got := EncodeURIComponent("UPID:pve1:00/a b~*'()é"); got != "UPID%3Apve1%3A00%2Fa%20b~*'()%C3%A9" {
		t.Errorf("EncodeURIComponent = %q", got)
	}
	if LocaleCompare("pve10", "pve2") >= 0 || LocaleCompare("B", "a") <= 0 || LocaleCompare("a", "A") >= 0 || LocaleCompare("a-b", "ab") >= 0 {
		t.Error("LocaleCompare")
	}
}

func TestParseDepthLimit(t *testing.T) {
	nested := func(n int) []byte {
		return []byte(strings.Repeat(`[{"a":`, n/2) + "1" + strings.Repeat(`}]`, n/2))
	}
	if _, err := Parse(nested(MaxDepth)); err != nil {
		t.Fatalf("%d levels: %v", MaxDepth, err)
	}
	if _, err := Parse(nested(MaxDepth + 2)); err == nil || !strings.Contains(err.Error(), "nested more than 256 levels") {
		t.Fatal(err)
	}
	// A hostile body is refused quickly rather than recursing a million frames deep.
	if _, err := Parse([]byte(strings.Repeat("[", 1_000_000))); err == nil {
		t.Fatal("deep array accepted")
	}
}
