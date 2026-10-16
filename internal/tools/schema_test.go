package tools

import (
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
)

func TestSchemaValidation(t *testing.T) {
	props := []Prop{
		P("vmid", vmidArg),
		P("name", Str().Min(2).Max(4).Opt()),
		P("key", Str().Pattern(`^net\d+$`, "a NIC key").Opt()),
		P("size", Num().Pos().Opt()),
		P("mode", Enum("b", "10", "a", "2").Opt()),
		P("tags", Arr(Str()).Max(2).Opt()),
		P("ip", RecKeys(Str().Pattern(`^\d+$`), Str()).Opt()),
		P("agent", Union(Bool(), Str()).Opt()),
		P("disks", Arr(Object(P("bus", Enum("scsi", "ide")), P("size", Int().Min(1).Opt()))).Opt()),
		P("extra", extraArg),
	}
	for _, tc := range []struct{ args, want string }{
		{`{}`, "Invalid input: expected number, received undefined at vmid"},
		{`{"vmid":99}`, "Too small: expected number to be >=100 at vmid"},
		{`{"vmid":100.5}`, "Invalid input: expected int, received number at vmid"},
		{`{"vmid":100,"name":"x"}`, "Too small: expected string to have >=2 characters at name"},
		{`{"vmid":100,"name":"xxxxx"}`, "Too big: expected string to have <=4 characters at name"},
		{`{"vmid":100,"key":"eth0"}`, "a NIC key at key"},
		{`{"vmid":100,"size":0}`, "Too small: expected number to be >0 at size"},
		{`{"vmid":100,"mode":"c"}`, `Invalid option: expected one of "2"|"10"|"b"|"a" at mode`},
		{`{"vmid":100,"tags":["a","b","c"]}`, "Too big: expected array to have <=2 items at tags"},
		{`{"vmid":100,"tags":[1]}`, "Invalid input: expected string, received number at tags.0"},
		{`{"vmid":100,"ip":{"x":"y"}}`, "Invalid key in record at ip.x"},
		{`{"vmid":100,"agent":1}`, "Invalid input at agent"},
		{`{"vmid":100,"disks":[{"size":0}]}`, "Invalid option: expected one of \"scsi\"|\"ide\" at disks.0.bus\nToo small: expected number to be >=1 at disks.0.size"},
		{`{"vmid":100,"extra":{"a":null}}`, "Invalid input at extra.a"},
		{`{"vmid":null}`, "Invalid input: expected number, received null at vmid"},
	} {
		_, err := ParseArgs(props, js.MustObj(tc.args))
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: got %v, want %q", tc.args, err, tc.want)
		}
	}

	a, err := ParseArgs(props, js.MustObj(`{"unknown":1,"agent":"enabled=1","vmid":100,"ip":{"1":"b","0":"a"},"disks":[{"bus":"ide","x":1}]}`))
	if err != nil {
		t.Fatal(err)
	}
	// Declared order, unknown arguments dropped (also inside objects), omitted ones left out.
	eqStr(t, js.Stringify(a, ""), `{"vmid":100,"ip":{"0":"a","1":"b"},"agent":"enabled=1","disks":[{"bus":"ide"}]}`)
	if a.Has("name") || a.Get("name") != js.Undef {
		t.Fatal("omitted arguments are undefined")
	}
}

func TestSchemaRendering(t *testing.T) {
	eqStr(t, js.Stringify(InputSchema(nil), ""), `{"type":"object","properties":{},"$schema":"http://json-schema.org/draft-07/schema#"}`)
	eqStr(t, js.Stringify(Union(Str(), Num(), Bool()).render(), ""), `{"type":["string","number","boolean"]}`)
	eqStr(t, js.Stringify(Union(Str(), Arr(Str())).render(), ""), `{"anyOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}]}`)
	eqStr(t, js.Stringify(Int().Pos().Max(5).D("n").render(), ""), `{"description":"n","type":"integer","exclusiveMinimum":0,"maximum":5}`)
	eqStr(t, js.Stringify(Enum("default", "3", "2.0").render(), ""), `{"type":"string","enum":["3","default","2.0"]}`)
}
