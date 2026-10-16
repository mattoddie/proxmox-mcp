package tools

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mattoddie/proxmox-mcp/internal/config"
	"github.com/mattoddie/proxmox-mcp/internal/js"
)

func listAll(t *testing.T, s *mcp.ClientSession) []*mcp.Tool {
	t.Helper()
	var out []*mcp.Tool
	for tool, err := range s.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tool)
	}
	return out
}

func TestToolGating(t *testing.T) {
	client := testClient(t)
	names := func(level string, toolsets ...string) map[string]bool {
		opts := toolOptions(level)
		if len(toolsets) > 0 {
			opts.Toolsets = map[string]bool{}
			for _, ts := range toolsets {
				opts.Toolsets[ts] = true
			}
		}
		return toolNames(t, connect(t, client, opts))
	}

	t.Run("registers only read tools by default and annotates them", func(t *testing.T) {
		tools := listAll(t, connect(t, client, toolOptions("read")))
		if len(tools) == 0 {
			t.Fatal("no tools")
		}
		for _, tool := range tools {
			match(t, tool.Name, `^proxmox_[a-z0-9_]+$`)
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Fatalf("%s should be read-only", tool.Name)
			}
			if len(tool.Description) <= 20 {
				t.Fatalf("%s needs a description", tool.Name)
			}
		}
	})

	t.Run("adds write, delete and exec tools only when enabled", func(t *testing.T) {
		read, write, del, exec, all := names("read"), names("write"), names("delete"), names("exec"), names("all")
		if read["proxmox_stop_task"] || !write["proxmox_stop_task"] {
			t.Fatal("proxmox_stop_task needs writes")
		}
		if write["proxmox_delete_metric_server"] || !del["proxmox_delete_metric_server"] {
			t.Fatal("proxmox_delete_metric_server needs deletes")
		}
		if len(all) < len(del) || len(all) < len(exec) {
			t.Fatal("all should include everything")
		}
		for n := range read {
			if !write[n] {
				t.Fatalf("%s is read but not in write", n)
			}
		}
		for n := range write {
			if !del[n] || !exec[n] || !all[n] {
				t.Fatalf("%s is a write tool missing from a higher level", n)
			}
		}
	})

	t.Run("limits tools to the enabled toolsets", func(t *testing.T) {
		only := names("all", "tasks")
		if len(only) == 0 {
			t.Fatal("no task tools")
		}
		for n := range only {
			match(t, n, `task`)
		}
		if len(names("all", "raw")) != 1 {
			t.Fatal("raw has one tool")
		}
	})

	t.Run("uses unique tool names with titles across all toolsets", func(t *testing.T) {
		seen := map[string]bool{}
		for _, tool := range listAll(t, connect(t, client, toolOptions("all"))) {
			if seen[tool.Name] {
				t.Fatalf("duplicate %s", tool.Name)
			}
			seen[tool.Name] = true
			if tool.Title == "" {
				t.Fatalf("%s needs a title", tool.Name)
			}
		}
	})

	t.Run("only mentions registered tools in prompts and tool descriptions", func(t *testing.T) {
		s := connect(t, client, toolOptions("all"))
		tools := listAll(t, s)
		known := map[string]bool{}
		var texts [][2]string
		for _, tool := range tools {
			known[tool.Name] = true
			texts = append(texts, [2]string{tool.Name, tool.Description})
		}
		for _, p := range Prompts {
			args := map[string]string{}
			for _, a := range p.Args {
				args[a.Name] = "100"
				if a.Name == "timeframe" {
					args[a.Name] = "week"
				}
			}
			res, err := s.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: p.Name, Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			texts = append(texts, [2]string{"prompt " + p.Name, res.Messages[0].Content.(*mcp.TextContent).Text})
		}
		ref := regexp.MustCompile(`proxmox_[a-z0-9_]+`)
		for _, tx := range texts {
			for _, name := range ref.FindAllString(tx[1], -1) {
				if strings.HasSuffix(name, "_") {
					continue // prefixes such as proxmox_*_vm
				}
				if !known[name] {
					t.Errorf("%s mentions unknown tool %s", tx[0], name)
				}
			}
		}
	})

	t.Run("marks write tools destructive unless they only add or change", func(t *testing.T) {
		registered := map[string]Info{}
		NewServer(client, config.AllTools(), registered)
		for _, tool := range listAll(t, connect(t, client, config.AllTools())) {
			info := registered[tool.Name]
			a := tool.Annotations
			if info.Gate == Read && tool.Name != "proxmox_api_request" && (a.DestructiveHint != nil || !a.ReadOnlyHint) {
				t.Fatalf("%s: read tools carry no destructive hint", tool.Name)
			}
			if (info.Gate == Delete || info.Gate == Exec) && (a.DestructiveHint == nil || !*a.DestructiveHint) {
				t.Fatalf("%s: delete and exec tools are destructive", tool.Name)
			}
			if *a.OpenWorldHint {
				t.Fatalf("%s: openWorldHint should be false", tool.Name)
			}
		}
	})

	t.Run("states the access level in the instructions", func(t *testing.T) {
		match(t, Instructions(toolOptions("read")), `The server is read-only`)
		match(t, Instructions(toolOptions("write")), `Write tools are enabled`)
		noMatch(t, Instructions(toolOptions("write")), `Command execution`)
		match(t, Instructions(toolOptions("all")), `Command execution in guests and on nodes is enabled`)
	})

	t.Run("returns JSON null for handlers without a result", func(t *testing.T) {
		eqStr(t, ok(js.Undef).Content[0].(*mcp.TextContent).Text, "null")
		eqStr(t, ok(js.O("a", 1, "b", js.Undef)).Content[0].(*mcp.TextContent).Text, "{\n  \"a\": 1\n}")
	})
}
