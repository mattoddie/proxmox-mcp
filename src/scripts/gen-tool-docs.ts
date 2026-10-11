/**
 * Generates docs/tools.md from the registered tools and prompts, so the
 * reference can't drift from the code. Run with `npm run docs:tools`;
 * `npm run docs:check` fails when the committed file is out of date.
 */
import { readFileSync, writeFileSync } from "node:fs";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { TOOLSETS, type ToolOptions, type Toolset } from "../config.js";
import { ProxmoxClient } from "../proxmox/client.js";
import { createServer } from "../server.js";

const TOOLSET_INFO: Record<Toolset, string> = {
  cluster: "Cluster overview, nodes and guests at a glance, datacenter options, corosync config, metric servers and bulk guest actions.",
  nodes: "Node status and statistics, services, network interfaces, DNS, time, updates and repositories, subscriptions, certificates and ACME, logs and node power.",
  vms: "QEMU virtual machines: status, configuration, power, create, clone, disks, migration, templates, cloud-init and statistics.",
  containers: "LXC containers: status, configuration, power, create, clone, volumes, migration, templates, interfaces and statistics.",
  agent: "The QEMU guest agent: OS, network and filesystem information from inside VMs, filesystem freeze/trim, and (with exec enabled) running commands and reading/writing files.",
  snapshots: "Snapshots of VMs and containers: list, create, roll back, describe and delete, plus a cluster-wide view of stale snapshots.",
  storage: "Storage definitions and usage, volumes and content, ISO/template downloads, storage discovery, physical disks, SMART, ZFS and LVM.",
  backup: "vzdump backup jobs, ad-hoc backups, backup listing and verification state, restores, pruning and guests without backups.",
  tasks: "Proxmox tasks (UPIDs): list, inspect, read logs, wait for and stop them.",
  access: "Users, groups, roles, ACLs, API tokens, authentication realms, effective permissions and two-factor authentication.",
  firewall: "Firewall rules, options, aliases, IP sets and security groups at datacenter, node and guest level, plus the firewall log.",
  ha: "High availability: status, HA resources, groups and rules, and HA-managed migration.",
  sdn: "Software-defined networking: zones, VNets, subnets, controllers, IPAM and DNS, and applying pending SDN changes.",
  ceph: "Ceph: health, OSDs, pools, monitors, managers, metadata servers, CephFS, flags and configuration.",
  pools: "Resource pools and their members.",
  replication: "Storage replication jobs, their status and logs.",
  notifications: "Notification targets (sendmail, SMTP, Gotify, webhooks) and matchers.",
  hardware: "PCI and USB devices, mediated devices, resource mappings, CPU models and QEMU capabilities.",
  raw: "A raw API escape hatch for anything the other tools don't cover. It is limited to GET requests unless writes are enabled.",
};

type Gate = "read" | "write" | "delete" | "exec";

interface JsonSchema {
  type?: string | string[];
  description?: string;
  enum?: unknown[];
  const?: unknown;
  items?: JsonSchema;
  anyOf?: JsonSchema[];
  properties?: Record<string, JsonSchema>;
  required?: string[];
  default?: unknown;
}

function typeOf(s: JsonSchema): string {
  if (s.const !== undefined) return `\`${JSON.stringify(s.const)}\``;
  if (s.enum) return s.enum.map((v) => `\`${JSON.stringify(v)}\``).join(" | ");
  if (s.anyOf) return s.anyOf.map(typeOf).join(" | ");
  if (s.type === "array") {
    const item = s.items ? typeOf(s.items) : "any";
    return item.includes(" | ") ? `(${item})[]` : `${item}[]`;
  }
  if (Array.isArray(s.type)) return s.type.join(" | ");
  return s.type ?? "any";
}

function cell(text: string): string {
  return text.replace(/\|/g, "\\|").replace(/\s*\n\s*/g, " ");
}

async function main(): Promise<void> {
  // A client that is never used: listing tools doesn't touch Proxmox.
  const proxmox = new ProxmoxClient({
    url: "https://pve.invalid:8006",
    tokenId: "unused@pam!unused",
    tokenSecret: "unused",
    verifySsl: false,
    timeoutMs: 1000,
    taskTimeoutMs: 1000,
  });
  const all: ToolOptions = { allowWrites: true, allowDeletes: true, allowExec: true, toolsets: new Set(TOOLSETS) };

  async function list(options: ToolOptions) {
    const server = createServer(proxmox, options);
    const [a, b] = InMemoryTransport.createLinkedPair();
    await server.connect(a);
    const mcp = new Client({ name: "gen-docs", version: "0" });
    await mcp.connect(b);
    // A server with no tools (or prompts) registered doesn't offer the list method at all.
    const result = {
      tools: (await mcp.listTools().catch(() => ({ tools: [] }))).tools,
      prompts: (await mcp.listPrompts().catch(() => ({ prompts: [] }))).prompts,
    };
    await mcp.close();
    return result;
  }

  const toolsetOf = new Map<string, Toolset>();
  for (const ts of TOOLSETS) {
    for (const t of (await list({ ...all, toolsets: new Set([ts]) })).tools) toolsetOf.set(t.name, ts);
  }
  const names = async (o: Partial<ToolOptions>) => new Set((await list({ ...all, ...o })).tools.map((t) => t.name));
  const reads = await names({ allowWrites: false, allowDeletes: false, allowExec: false });
  const writes = await names({ allowDeletes: false, allowExec: false });
  const withDeletes = await names({ allowExec: false });
  const { tools, prompts } = await list(all);

  const gate = (name: string): Gate => (reads.has(name) ? "read" : writes.has(name) ? "write" : withDeletes.has(name) ? "delete" : "exec");
  const count = (g: Gate) => tools.filter((t) => gate(t.name) === g).length;
  const badge: Record<Gate, string> = { read: "🟢 read", write: "🟠 write", delete: "🔴 delete", exec: "🟣 exec" };

  const out: string[] = [];
  out.push(
    "# Tool reference",
    "",
    "<!-- Generated by `npm run docs:tools` from the tool definitions. Do not edit by hand. -->",
    "",
    `proxmox-mcp has **${tools.length} tools** in **${TOOLSETS.length} toolsets**: ${count("read")} read, ${count("write")} write, ${count("delete")} delete and ${count("exec")} exec. It also has **${prompts.length} prompts**.`,
    "",
    "Each tool is labelled with the setting it needs:",
    "",
    "| Label | Registered when |",
    "|---|---|",
    "| 🟢 read | Always |",
    "| 🟠 write | `PROXMOX_ALLOW_WRITES=true` |",
    "| 🔴 delete | `PROXMOX_ALLOW_WRITES=true` and `PROXMOX_ALLOW_DELETES=true` |",
    "| 🟣 exec | `PROXMOX_ALLOW_WRITES=true` and `PROXMOX_ALLOW_EXEC=true` |",
    "",
    "Every tool also needs its toolset to be enabled in `PROXMOX_TOOLSETS`, which enables all toolsets by default. Tools that act on a VM or container take its `vmid` and find the node automatically; node-scoped tools default to `PROXMOX_DEFAULT_NODE` or the only node. Tools that start a Proxmox task wait for it by default (`wait`), up to `PROXMOX_TASK_TIMEOUT_MS`.",
    "",
    "## Contents",
    "",
    ...TOOLSETS.map((ts) => `- [\`${ts}\`](#${ts}): ${TOOLSET_INFO[ts]}`),
    "- [Prompts](#prompts)",
    "",
  );

  for (const ts of TOOLSETS) {
    out.push(`## ${ts}`, "", TOOLSET_INFO[ts], "");
    const inSet = tools.filter((t) => toolsetOf.get(t.name) === ts);
    out.push("| Tool | Access | Summary |", "|---|---|---|");
    for (const t of inSet) out.push(`| [\`${t.name}\`](#${t.name}) | ${badge[gate(t.name)]} | ${cell(t.title ?? t.annotations?.title ?? "")} |`);
    out.push("");
    for (const t of inSet) {
      out.push(`### ${t.name}`, "", `${badge[gate(t.name)]}${t.annotations?.destructiveHint ? " · destructive" : ""}`, "", t.description ?? "", "");
      const schema = t.inputSchema as JsonSchema;
      const props = Object.entries(schema.properties ?? {});
      if (props.length === 0) {
        out.push("_No arguments._", "");
        continue;
      }
      const required = new Set(schema.required ?? []);
      out.push("| Argument | Type | Required | Description |", "|---|---|---|---|");
      for (const [name, p] of props) {
        let desc = p.description ?? "";
        if (p.default !== undefined) desc += `${desc ? " " : ""}Default: \`${JSON.stringify(p.default)}\`.`;
        out.push(`| \`${name}\` | ${cell(typeOf(p))} | ${required.has(name) ? "yes" : ""} | ${cell(desc)} |`);
      }
      out.push("");
    }
  }

  out.push(
    "## Prompts",
    "",
    "Prompts are ready-made requests that tell the model which tools to call and what to report. In Claude Code they appear as slash commands, such as `/mcp__proxmox__cluster_health_check`.",
    "",
  );
  for (const p of prompts) {
    out.push(`### ${p.name}`, "", p.description ?? "", "");
    if (p.arguments?.length) {
      out.push("| Argument | Required | Description |", "|---|---|---|");
      for (const a of p.arguments) out.push(`| \`${a.name}\` | ${a.required ? "yes" : ""} | ${cell(a.description ?? "")} |`);
      out.push("");
    }
  }

  const text = out.join("\n").replace(/\n{3,}/g, "\n\n");
  const target = "docs/tools.md";
  if (process.argv.includes("--check")) {
    let current = "";
    try {
      current = readFileSync(target, "utf8");
    } catch {}
    if (current !== text) {
      console.error(`${target} is out of date. Run \`npm run docs:tools\` and commit the result.`);
      process.exit(1);
    }
    console.log(`${target} is up to date.`);
  } else {
    writeFileSync(target, text);
    console.log(`Wrote ${target} (${tools.length} tools, ${prompts.length} prompts).`);
  }
  await proxmox.close();
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
