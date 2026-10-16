// Command gen-tool-docs writes docs/tools.md from the registered tools and prompts, so the
// reference can't drift from the code. With -check it fails when the committed file is out of date.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mattoddie/proxmox-mcp/internal/config"
	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
	"github.com/mattoddie/proxmox-mcp/internal/tools"
)

var toolsetInfo = map[string]string{
	"cluster":       "Cluster overview, nodes and guests at a glance, datacenter options, corosync config, metric servers and bulk guest actions.",
	"nodes":         "Node status and statistics, services, network interfaces, DNS, time, updates and repositories, subscriptions, certificates and ACME, logs and node power.",
	"vms":           "QEMU virtual machines: status, configuration, power, create, clone, disks, migration, templates, cloud-init and statistics.",
	"containers":    "LXC containers: status, configuration, power, create, clone, volumes, migration, templates, interfaces and statistics.",
	"agent":         "The QEMU guest agent: OS, network and filesystem information from inside VMs, filesystem freeze/trim, and (with exec enabled) running commands and reading/writing files.",
	"snapshots":     "Snapshots of VMs and containers: list, create, roll back, describe and delete, plus a cluster-wide view of stale snapshots.",
	"storage":       "Storage definitions and usage, volumes and content, ISO/template downloads, storage discovery, physical disks, SMART, ZFS and LVM.",
	"backup":        "vzdump backup jobs, ad-hoc backups, backup listing and verification state, restores, pruning and guests without backups.",
	"tasks":         "Proxmox tasks (UPIDs): list, inspect, read logs, wait for and stop them.",
	"access":        "Users, groups, roles, ACLs, API tokens, authentication realms, effective permissions and two-factor authentication.",
	"firewall":      "Firewall rules, options, aliases, IP sets and security groups at datacenter, node and guest level, plus the firewall log.",
	"ha":            "High availability: status, HA resources, groups and rules, and HA-managed migration.",
	"sdn":           "Software-defined networking: zones, VNets, subnets, controllers, IPAM and DNS, and applying pending SDN changes.",
	"ceph":          "Ceph: health, OSDs, pools, monitors, managers, metadata servers, CephFS, flags and configuration.",
	"pools":         "Resource pools and their members.",
	"replication":   "Storage replication jobs, their status and logs.",
	"notifications": "Notification targets (sendmail, SMTP, Gotify, webhooks) and matchers.",
	"hardware":      "PCI and USB devices, mediated devices, resource mappings, CPU models and QEMU capabilities.",
	"raw":           "A raw API escape hatch for anything the other tools don't cover. It is limited to GET requests unless writes are enabled.",
}

var badge = map[tools.Gate]string{tools.Read: "🟢 read", tools.Write: "🟠 write", tools.Delete: "🔴 delete", tools.Exec: "🟣 exec"}

// typeOf renders a JSON Schema type the way the reference shows it: `"a"` | `"b"` for enums,
// T[] for arrays, unions joined with |.
func typeOf(s *js.Obj) string {
	if s.Defined("const") {
		return "`" + js.Stringify(s.Get("const"), "") + "`"
	}
	if e := s.Arr("enum"); e != nil {
		parts := make([]string, len(e))
		for i, v := range e {
			parts[i] = "`" + js.Stringify(v, "") + "`"
		}
		return strings.Join(parts, " | ")
	}
	if any := s.Objs("anyOf"); s.Defined("anyOf") {
		parts := make([]string, len(any))
		for i, o := range any {
			parts[i] = typeOf(o)
		}
		return strings.Join(parts, " | ")
	}
	switch t := s.Get("type").(type) {
	case string:
		if t == "array" {
			item := "any"
			if items := s.ObjAt("items"); items != nil {
				item = typeOf(items)
			}
			if strings.Contains(item, " | ") {
				return "(" + item + ")[]"
			}
			return item + "[]"
		}
		return t
	case []any:
		return strings.Join(strs(t), " | ")
	}
	return "any"
}

func strs(v []any) []string {
	out := make([]string, len(v))
	for i, x := range v {
		out[i] = js.Str(x)
	}
	return out
}

var spaces = regexp.MustCompile(`\s*\n\s*`)

func cell(s string) string { return spaces.ReplaceAllString(strings.ReplaceAll(s, "|", "\\|"), " ") }

// Generate renders docs/tools.md.
func Generate() (string, error) {
	// A client that is never used: listing tools doesn't touch Proxmox.
	client, err := proxmox.New(config.Proxmox{URL: "https://pve.invalid:8006", TokenID: "unused@pam!unused", TokenSecret: "unused", TimeoutMs: 1000, TaskTimeoutMs: 1000})
	if err != nil {
		return "", err
	}
	registered := map[string]tools.Info{}
	server := tools.NewServer(client, config.AllTools(), registered)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		return "", err
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "gen-tool-docs", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		return "", err
	}
	defer session.Close()

	var all []*mcp.Tool
	for t, err := range session.Tools(ctx, nil) {
		if err != nil {
			return "", err
		}
		all = append(all, t)
	}
	// The SDK lists tools by name; the reference lists them in registration order.
	sort.SliceStable(all, func(i, j int) bool { return registered[all[i].Name].Order < registered[all[j].Name].Order })
	listed := map[string]*mcp.Prompt{}
	for p, err := range session.Prompts(ctx, nil) {
		if err != nil {
			return "", err
		}
		listed[p.Name] = p
	}
	counts := map[tools.Gate]int{}
	for _, t := range all {
		counts[registered[t.Name].Gate]++
	}

	var out []string
	w := func(lines ...string) { out = append(out, lines...) }
	w("# Tool reference", "",
		"<!-- Generated by `make docs` (go run ./cmd/gen-tool-docs) from the tool definitions. Do not edit by hand. -->", "",
		fmt.Sprintf("proxmox-mcp has **%d tools** in **%d toolsets**: %d read, %d write, %d delete and %d exec. It also has **%d prompts**.",
			len(all), len(config.Toolsets), counts[tools.Read], counts[tools.Write], counts[tools.Delete], counts[tools.Exec], len(listed)), "",
		"Each tool is labelled with the setting it needs:", "",
		"| Label | Registered when |", "|---|---|",
		"| 🟢 read | Always |",
		"| 🟠 write | `PROXMOX_ALLOW_WRITES=true` |",
		"| 🔴 delete | `PROXMOX_ALLOW_WRITES=true` and `PROXMOX_ALLOW_DELETES=true` |",
		"| 🟣 exec | `PROXMOX_ALLOW_WRITES=true` and `PROXMOX_ALLOW_EXEC=true` |", "",
		"Every tool also needs its toolset to be enabled in `PROXMOX_TOOLSETS`, which enables all toolsets by default. Tools that act on a VM or container take its `vmid` and find the node automatically; node-scoped tools default to `PROXMOX_DEFAULT_NODE` or the only node. Tools that start a Proxmox task wait for it by default (`wait`), up to `PROXMOX_TASK_TIMEOUT_MS`.", "",
		"## Contents", "")
	for _, ts := range config.Toolsets {
		w(fmt.Sprintf("- [`%s`](#%s): %s", ts, ts, toolsetInfo[ts]))
	}
	w("- [Prompts](#prompts)", "")

	for _, ts := range config.Toolsets {
		w("## "+ts, "", toolsetInfo[ts], "")
		var inSet []*mcp.Tool
		for _, t := range all {
			if registered[t.Name].Toolset == ts {
				inSet = append(inSet, t)
			}
		}
		w("| Tool | Access | Summary |", "|---|---|---|")
		for _, t := range inSet {
			title := t.Title
			if title == "" && t.Annotations != nil {
				title = t.Annotations.Title
			}
			w(fmt.Sprintf("| [`%s`](#%s) | %s | %s |", t.Name, t.Name, badge[registered[t.Name].Gate], cell(title)))
		}
		w("")
		for _, t := range inSet {
			label := badge[registered[t.Name].Gate]
			if a := t.Annotations; a != nil && a.DestructiveHint != nil && *a.DestructiveHint {
				label += " · destructive"
			}
			w("### "+t.Name, "", label, "", t.Description, "")
			// The schema as the server sends it (the SDK client decodes it into a map, losing the property order).
			schema := tools.InputSchema(registered[t.Name].Props)
			props := schema.ObjAt("properties")
			if props.Len() == 0 {
				w("_No arguments._", "")
				continue
			}
			required := map[string]bool{}
			for _, r := range schema.Strs("required") {
				required[r] = true
			}
			w("| Argument | Type | Required | Description |", "|---|---|---|---|")
			for _, name := range props.Keys() {
				p := props.ObjAt(name)
				desc := js.Str(or(p.Get("description"), ""))
				if p.Defined("default") {
					if desc != "" {
						desc += " "
					}
					desc += "Default: `" + js.Stringify(p.Get("default"), "") + "`."
				}
				req := ""
				if required[name] {
					req = "yes"
				}
				w(fmt.Sprintf("| `%s` | %s | %s | %s |", name, cell(typeOf(p)), req, cell(desc)))
			}
			w("")
		}
	}

	w("## Prompts", "",
		"Prompts are ready-made requests that tell the model which tools to call and what to report. In Claude Code they appear as slash commands, such as `/mcp__proxmox__cluster_health_check`.", "")
	for _, def := range tools.Prompts {
		p := listed[def.Name]
		if p == nil {
			continue
		}
		w("### "+p.Name, "", p.Description, "")
		if len(p.Arguments) > 0 {
			w("| Argument | Required | Description |", "|---|---|---|")
			for _, a := range p.Arguments {
				req := ""
				if a.Required {
					req = "yes"
				}
				w(fmt.Sprintf("| `%s` | %s | %s |", a.Name, req, cell(a.Description)))
			}
			w("")
		}
	}
	return regexp.MustCompile(`\n{3,}`).ReplaceAllString(strings.Join(out, "\n"), "\n\n"), nil
}

func or(v, fallback any) any {
	if js.Nullish(v) {
		return fallback
	}
	return v
}

func main() {
	check := flag.Bool("check", false, "fail if docs/tools.md is out of date instead of writing it")
	target := flag.String("o", "docs/tools.md", "output file")
	flag.Parse()
	text, err := Generate()
	if err != nil {
		log.Fatal(err)
	}
	if *check {
		current, _ := os.ReadFile(*target)
		if string(current) != text {
			log.Fatalf("%s is out of date. Run `make docs` and commit the result.", *target)
		}
		fmt.Printf("%s is up to date.\n", *target)
		return
	}
	if err := os.WriteFile(*target, []byte(text), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Wrote %s (%d tools, %d prompts).\n", *target, strings.Count(text, "\n### proxmox_"), len(tools.Prompts))
}
