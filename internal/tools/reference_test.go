package tools

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mattoddie/proxmox-mcp/internal/config"
	"github.com/mattoddie/proxmox-mcp/internal/js"
)

// testdata/typescript-reference.json is what the TypeScript server (v0.2.0) listed: every tool's
// name, title, description, input schema and annotations, the tools registered at each access
// level with the server instructions, and the prompts and their text. These tests hold the Go
// server to it, so it stays a drop-in replacement.

type reference struct {
	all      *Obj
	toolsets map[string]string
}

func loadReference(t *testing.T) reference {
	t.Helper()
	raw, err := os.ReadFile("testdata/typescript-reference.json")
	if err != nil {
		t.Fatal(err)
	}
	all, err := js.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Toolset of each tool, from the reference docs.
	docs, err := os.ReadFile("../../docs/tools.md")
	if err != nil {
		t.Fatal(err)
	}
	toolsets := map[string]string{}
	current := ""
	for _, line := range strings.Split(string(docs), "\n") {
		if ts, ok := strings.CutPrefix(line, "## "); ok {
			current = ts
		} else if name, ok := strings.CutPrefix(line, "### "); ok && strings.HasPrefix(name, "proxmox_") {
			toolsets[name] = current
		}
	}
	ref := all.(*Obj)
	applyDeliberateChanges(ref)
	return reference{all: ref, toolsets: toolsets}
}

// applyDeliberateChanges edits the reference for the changes made on purpose since the
// TypeScript server (each is in the CHANGELOG), so everything else is still held to it.
func applyDeliberateChanges(ref *Obj) {
	for _, tool := range ref.Objs("tools") {
		switch tool.Get("name") {
		case "proxmox_query_url_metadata":
			// A write tool now: the node fetches any URL it is given.
			tool.ObjAt("annotations").Set("readOnlyHint", false).Set("destructiveHint", false)
		case "proxmox_list_acme_directories":
			tool.Set("description", js.Str(tool.Get("description"))+" Fetching `directory` makes the node connect to that URL, so it needs write access.")
		}
	}
	read := ref.ObjAt("variants").ObjAt("read")
	var tools []any
	for _, n := range read.Strs("tools") {
		if n != "proxmox_query_url_metadata" {
			tools = append(tools, n)
		}
	}
	read.Set("tools", tools)
}

// listTools returns the tools a server with opts lists, by name, with their schemas decoded
// preserving order (the SDK client would decode them into maps).
func listTools(t *testing.T, opts config.Tools) (map[string]*mcp.Tool, map[string]Info, string) {
	t.Helper()
	registered := map[string]Info{}
	server := NewServer(testClient(t), opts, registered)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close(); _ = ss.Close() }()
	out := map[string]*mcp.Tool{}
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		out[tool.Name] = tool
	}
	return out, registered, cs.InitializeResult().Instructions
}

// annotations normalises tool annotations: the Go SDK leaves out false readOnlyHint and idempotentHint.
func annotations(v any) *Obj {
	o := js.Spread(js.V(js.MustParse(js.Stringify(v, ""))))
	for _, k := range []string{"readOnlyHint", "idempotentHint"} {
		if !o.Defined(k) {
			o.Set(k, false)
		}
	}
	return o
}

func TestReferenceTools(t *testing.T) {
	ref := loadReference(t)
	got, registered, _ := listTools(t, config.AllTools())
	for _, rt := range ref.all.Objs("tools") {
		name := js.Str(rt.Get("name"))
		t.Run(ref.toolsets[name]+"/"+name, func(t *testing.T) {
			tool, ok := got[name]
			if !ok {
				t.Fatalf("%s is not registered", name)
			}
			eqStr(t, tool.Title, js.Str(rt.Get("title")), "title")
			eqStr(t, tool.Description, js.Str(rt.Get("description")), "description")
			eq(t, annotations(tool.Annotations), annotations(rt.Get("annotations")), "annotations")
			schema := InputSchema(registered[name].Props)
			eq(t, schema, rt.Get("inputSchema"), "inputSchema")
			// Argument order matters for the docs.
			eq(t, schema.ObjAt("properties").Keys(), rt.ObjAt("inputSchema").ObjAt("properties").Keys(), "argument order")
			if ts := ref.toolsets[name]; registered[name].Toolset != ts {
				t.Fatalf("toolset %s, want %s", registered[name].Toolset, ts)
			}
		})
	}
	t.Run("no extra tools", func(t *testing.T) {
		known := map[string]bool{}
		for _, rt := range ref.all.Objs("tools") {
			known[js.Str(rt.Get("name"))] = true
		}
		for name := range got {
			if !known[name] {
				t.Errorf("%s is not in the reference", name)
			}
		}
	})
}

func TestReferenceAccessLevels(t *testing.T) {
	ref := loadReference(t)
	levels := map[string]func(*config.Tools){
		"all":         func(*config.Tools) {},
		"read":        func(o *config.Tools) { o.AllowWrites, o.AllowDeletes, o.AllowExec = false, false, false },
		"write":       func(o *config.Tools) { o.AllowDeletes, o.AllowExec = false, false },
		"writeDelete": func(o *config.Tools) { o.AllowExec = false },
		"writeExec":   func(o *config.Tools) { o.AllowDeletes = false },
	}
	variants := ref.all.ObjAt("variants")
	for _, level := range variants.Keys() {
		t.Run(level, func(t *testing.T) {
			v := variants.ObjAt(level)
			opts := config.AllTools()
			levels[level](&opts)
			got, _, instructions := listTools(t, opts)
			eqStr(t, instructions, js.Str(v.Get("instructions")), "instructions")
			var missing, extra []string
			wantNames := map[string]bool{}
			for _, n := range v.Strs("tools") {
				wantNames[n] = true
				if got[n] == nil {
					missing = append(missing, n)
				}
			}
			for n := range got {
				if !wantNames[n] {
					extra = append(extra, n)
				}
			}
			if len(missing) > 0 || len(extra) > 0 {
				t.Fatalf("missing %v, extra %v", missing, extra)
			}
			raw := v.ObjAt("raw")
			// The raw tool's description only gains sentences after the reference's (about the
			// checks added since), so the reference must still be its start.
			if desc, want := got["proxmox_api_request"].Description, js.Str(raw.Get("description")); !strings.HasPrefix(desc, want) {
				t.Fatalf("raw tool description:\n got %q\nwant %q...", desc, want)
			}
			eq(t, annotations(got["proxmox_api_request"].Annotations), annotations(raw.Get("annotations")), "raw tool annotations")
		})
	}
}

func TestReferencePrompts(t *testing.T) {
	ref := loadReference(t)
	cs := connect(t, testClient(t), config.AllTools())
	ctx := context.Background()
	listed := map[string]*mcp.Prompt{}
	for p, err := range cs.Prompts(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		listed[p.Name] = p
	}
	refPrompts := ref.all.Objs("prompts")
	if len(listed) != len(refPrompts) || len(Prompts) != len(refPrompts) {
		t.Fatalf("%d prompts, want %d", len(listed), len(refPrompts))
	}
	for i, rp := range refPrompts {
		name := js.Str(rp.Get("name"))
		if Prompts[i].Name != name {
			t.Fatalf("prompt %d is %s, want %s", i, Prompts[i].Name, name)
		}
		p := listed[name]
		eqStr(t, p.Title, js.Str(rp.Get("title")), name)
		eqStr(t, p.Description, js.Str(rp.Get("description")), name)
		var args []any
		for _, a := range p.Arguments {
			args = append(args, js.O("name", a.Name, "description", a.Description, "required", a.Required))
		}
		var wantArgs []any
		for _, a := range rp.Objs("arguments") {
			wantArgs = append(wantArgs, js.O("name", a.Get("name"), "description", a.Get("description"), "required", a.Truthy("required")))
		}
		eq(t, js.V(args), js.V(wantArgs), name+" arguments")
	}
	for _, pt := range ref.all.Objs("promptTexts") {
		name := js.Str(pt.Get("name"))
		args := map[string]string{}
		for _, a := range listed[name].Arguments {
			if pt.Get("args") == "min" && !a.Required {
				continue
			}
			switch a.Name {
			case "timeframe":
				args[a.Name] = "month"
			case "older_than_days":
				args[a.Name] = "30"
			default:
				args[a.Name] = "ARG_" + a.Name
			}
		}
		res, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		eqStr(t, res.Messages[0].Content.(*mcp.TextContent).Text, js.Str(pt.Get("text")), name+" "+js.Str(pt.Get("args")))
		if res.Messages[0].Role != "user" {
			t.Fatalf("%s: role %s", name, res.Messages[0].Role)
		}
	}
	// Invalid arguments are refused like the original server's.
	_, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "capacity_planning", Arguments: map[string]string{"timeframe": "x"}})
	match(t, errString(err), `Invalid arguments for prompt capacity_planning: Invalid option: expected one of "day"\|"week"\|"month"\|"year" at timeframe`)
	_, err = cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "troubleshoot_guest", Arguments: map[string]string{}})
	match(t, errString(err), `Invalid arguments for prompt troubleshoot_guest: Invalid input: expected string, received undefined at guest`)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
