import { z } from "zod";
import type { ProxmoxClient } from "../proxmox/client.js";
import { compact, flag, parsePropertyString } from "../proxmox/format.js";
import {
  defined,
  defineTool,
  deleteKeysArg,
  deleteParam,
  digestArg,
  enc,
  extraArg,
  filterAndLimit,
  limitArg,
  rawArg,
  searchArg,
  vmidArg,
  type ToolContext,
} from "./util.js";

type Obj = Record<string, any>;

// ---------------------------------------------------------------- scopes

type Scope = "cluster" | "node" | "guest" | "group";

const SCOPE_TEXT: Record<Scope, string> = {
  cluster: "cluster = datacenter-wide",
  node: "node = a host's own firewall",
  guest: "guest = a VM or container (give vmid)",
  group: "group = a security group's rules (give group)",
};

/** The scope argument trio/quartet, restricted to the levels where an endpoint exists. */
function scopeArgs<const S extends readonly Scope[]>(scopes: S) {
  return {
    scope: z
      .enum(scopes as unknown as [S[number], ...S[number][]])
      .optional()
      .describe(
        `Firewall level: ${scopes.map((s) => SCOPE_TEXT[s]).join("; ")}. ` +
          `Inferred when omitted: ${scopes.includes("guest" as never) ? "guest if vmid is given, " : ""}${scopes.includes("group" as never) ? "group if group is given, " : ""}${scopes.includes("node" as never) ? "node if node is given, " : ""}otherwise ${scopes.includes("cluster" as never) ? "cluster" : scopes[0]}.`,
      ),
    node: z
      .string()
      .optional()
      .describe("Node name: the host for scope node (defaults to PROXMOX_DEFAULT_NODE or the only node), or the guest's node for scope guest (looked up from vmid when omitted)"),
    ...(scopes.includes("guest" as never) ? { vmid: vmidArg.optional().describe("VM or container ID, for scope guest") } : {}),
    ...(scopes.includes("group" as never) ? { group: z.string().optional().describe("Security group name, for scope group (see proxmox_list_firewall_groups)") } : {}),
  };
}

interface ScopeRef {
  scope: Scope;
  /** Firewall base path, e.g. /cluster/firewall or /nodes/pve1/qemu/100/firewall. */
  fw: string;
  /** Rules collection path (for groups, the group itself). */
  rules: string;
  /** What the scope points at, echoed in results. */
  target: Obj;
}

async function resolveScope(client: ProxmoxClient, a: Obj, allowed: readonly Scope[]): Promise<ScopeRef> {
  const fallback: Scope = allowed.includes("cluster") ? "cluster" : allowed[0];
  const inferred: Scope =
    a.vmid !== undefined && allowed.includes("guest") ? "guest" : a.group && allowed.includes("group") ? "group" : a.node && allowed.includes("node") ? "node" : fallback;
  const scope: Scope = a.scope ?? inferred;
  if (!allowed.includes(scope)) throw new Error(`scope "${scope}" is not supported here; use one of: ${allowed.join(", ")}`);
  if (scope !== "guest" && a.vmid !== undefined) throw new Error(`vmid only applies to scope "guest" (scope is "${scope}")`);
  if (scope !== "group" && a.group) throw new Error(`group only applies to scope "group" (scope is "${scope}")`);
  switch (scope) {
    case "cluster":
      if (a.node) throw new Error('node does not apply to scope "cluster"; use scope "node" for a host firewall');
      return { scope, fw: "/cluster/firewall", rules: "/cluster/firewall/rules", target: { scope } };
    case "node": {
      const node = await client.resolveNode(a.node);
      const fw = `/nodes/${enc(node)}/firewall`;
      return { scope, fw, rules: `${fw}/rules`, target: { scope, node } };
    }
    case "guest": {
      if (a.vmid === undefined) throw new Error('vmid is required for scope "guest"');
      const g = await client.resolveGuest(a.vmid, { node: a.node });
      const fw = `${client.guestPath(g)}/firewall`;
      return { scope, fw, rules: `${fw}/rules`, target: compact({ scope, vmid: g.vmid, node: g.node, type: g.type === "qemu" ? "vm" : "container", name: g.name }) };
    }
    case "group":
      if (!a.group) throw new Error('group is required for scope "group"');
      return { scope, fw: "/cluster/firewall", rules: `/cluster/firewall/groups/${enc(a.group)}`, target: { scope, group: a.group } };
  }
}

// ---------------------------------------------------------------- rules

const LOG_LEVELS = ["emerg", "alert", "crit", "err", "warning", "notice", "info", "debug", "nolog"] as const;
const logLevelArg = (what: string) => z.enum(LOG_LEVELS).optional().describe(`Log level for ${what} (nolog = off)`);

/** One-line rendering of a rule, close to the firewall config file syntax. */
export function ruleLine(r: Obj): string {
  const parts: string[] = [];
  if (r.enable !== undefined && !flag(r.enable)) parts.push("[disabled]");
  if (r.type === "group") parts.push(`GROUP ${r.action}`);
  else parts.push(String(r.type ?? "?").toUpperCase(), r.macro ? `${r.macro}(${r.action})` : String(r.action ?? "?"));
  const opt = (name: string, v: unknown) => {
    if (v !== undefined && v !== null && v !== "") parts.push(`-${name} ${v}`);
  };
  opt("i", r.iface);
  opt("source", r.source);
  opt("dest", r.dest);
  opt("p", r.proto);
  opt("sport", r.sport);
  opt("dport", r.dport);
  opt("icmp-type", r["icmp-type"]);
  if (r.log && r.log !== "nolog") opt("log", r.log);
  let line = parts.join(" ");
  if (r.comment) line += ` # ${r.comment}`;
  return line;
}

function summarizeRule(r: Obj): Obj {
  return compact({
    pos: r.pos !== undefined ? Number(r.pos) : undefined,
    rule: ruleLine(r),
    type: r.type,
    action: r.action,
    macro: r.macro,
    proto: r.proto,
    source: r.source,
    dest: r.dest,
    sport: r.sport,
    dport: r.dport,
    icmp_type: r["icmp-type"],
    iface: r.iface,
    enable: r.enable !== undefined ? Boolean(flag(r.enable)) : undefined,
    log: r.log,
    comment: r.comment,
  });
}

const ruleFields = {
  type: z.enum(["in", "out", "forward", "group"]).optional().describe("Direction: in, out, forward (PVE 8.2+, nftables) or group (insert a security group; action = group name). Required when creating"),
  action: z.string().optional().describe("ACCEPT, DROP or REJECT, or the security group name when type is group. Required when creating"),
  macro: z.string().optional().describe("Predefined service macro, e.g. SSH, HTTP, HTTPS, DNS, Ping (see proxmox_list_firewall_macros). Replaces proto/dport"),
  proto: z.string().optional().describe("IP protocol, e.g. tcp, udp, icmp, icmpv6, or a number"),
  source: z.string().optional().describe("Source address: IP, CIDR, range, comma list, IP set (+name, +dc/name, +guest/name) or alias name (see proxmox_list_firewall_refs)"),
  dest: z.string().optional().describe("Destination address, same syntax as source"),
  sport: z.string().optional().describe("Source port(s): number, service name, range 1000:2000 or comma list"),
  dport: z.string().optional().describe("Destination port(s): number, service name, range 1000:2000 or comma list"),
  icmp_type: z.string().optional().describe("ICMP type (only with proto icmp/icmpv6), e.g. echo-request"),
  iface: z.string().optional().describe("Interface: net0, net1... for guests; any interface name for hosts"),
  enable: z.boolean().optional().describe("Whether the rule is active (new rules are disabled unless enable=true)"),
  log: logLevelArg("packets matching this rule"),
  comment: z.string().optional().describe("Comment"),
};

function ruleParams(a: Obj): Obj {
  return defined({
    type: a.type,
    action: a.action,
    macro: a.macro,
    proto: a.proto,
    source: a.source,
    dest: a.dest,
    sport: a.sport,
    dport: a.dport,
    "icmp-type": a.icmp_type,
    iface: a.iface,
    enable: a.enable,
    log: a.log,
    comment: a.comment,
  });
}

// ---------------------------------------------------------------- options

const OPTION_KEYS: Record<"cluster" | "node" | "guest", string[]> = {
  cluster: ["enable", "policy_in", "policy_out", "policy_forward", "ebtables", "log_ratelimit"],
  node: [
    "enable",
    "log_level_in",
    "log_level_out",
    "log_level_forward",
    "log_nf_conntrack",
    "ndp",
    "nf_conntrack_allow_invalid",
    "nf_conntrack_helpers",
    "nf_conntrack_max",
    "nf_conntrack_tcp_timeout_established",
    "nf_conntrack_tcp_timeout_syn_recv",
    "nftables",
    "nosmurfs",
    "protection_synflood",
    "protection_synflood_burst",
    "protection_synflood_rate",
    "smurf_log_level",
    "tcp_flags_log_level",
    "tcpflags",
  ],
  guest: ["enable", "policy_in", "policy_out", "dhcp", "ipfilter", "log_level_in", "log_level_out", "macfilter", "ndp", "radv"],
};

/** Proxmox's documented defaults, used to show the effective value of unset options. */
const OPTION_DEFAULTS: Record<"cluster" | "node" | "guest", Obj> = {
  cluster: { enable: false, policy_in: "DROP", policy_out: "ACCEPT", policy_forward: "ACCEPT", ebtables: true },
  node: { enable: true, nftables: false, ndp: true, tcpflags: false, nosmurfs: true, log_level_in: "nolog", log_level_out: "nolog" },
  guest: { enable: false, policy_in: "DROP", policy_out: "ACCEPT", dhcp: false, macfilter: true, ipfilter: false, ndp: true, radv: false },
};

const BOOL_OPTIONS = new Set([
  "enable",
  "ebtables",
  "log_nf_conntrack",
  "ndp",
  "nf_conntrack_allow_invalid",
  "nftables",
  "nosmurfs",
  "protection_synflood",
  "tcpflags",
  "dhcp",
  "ipfilter",
  "macfilter",
  "radv",
]);

function effectiveOptions(scope: "cluster" | "node" | "guest", opts: Obj): Obj {
  const out: Obj = { ...OPTION_DEFAULTS[scope] };
  for (const [k, v] of Object.entries(opts)) {
    if (k === "digest") continue;
    out[k] = BOOL_OPTIONS.has(k) ? Boolean(flag(v)) : v;
  }
  return out;
}

// ---------------------------------------------------------------- helpers

function summarizeIpsetEntry(e: Obj): Obj {
  return compact({ cidr: e.cidr, nomatch: flag(e.nomatch) ? true : undefined, comment: e.comment });
}

function nicFirewall(config: Obj): Obj[] {
  return Object.keys(config)
    .filter((k) => /^net\d+$/.test(k))
    .sort((x, y) => Number(x.slice(3)) - Number(y.slice(3)))
    .map((k) => {
      const p = parsePropertyString(config[k]);
      return compact({ nic: k, bridge: p.bridge, tag: p.tag, firewall: Boolean(flag(p.firewall)), ip: p.ip, ip6: p.ip6 });
    });
}

export function registerFirewallTools(ctx: ToolContext): void {
  const { client } = ctx;
  const ALL: Scope[] = ["cluster", "node", "guest", "group"];
  const OPT_SCOPES = ["cluster", "node", "guest"] as const;
  const CG = ["cluster", "guest"] as const;

  // ---------------------------------------------------------------- rules

  defineTool(ctx, "proxmox_list_firewall_rules", {
    toolset: "firewall",
    title: "List firewall rules",
    description:
      "List the firewall rules of the datacenter (cluster), a node, a VM/container (guest) or a security group, in evaluation order (pos 0 first). Each rule has a readable one-line form like 'IN SSH(ACCEPT) -i net0 -source +mgmt # admins'. Rules of type group pull in a security group's rules. For 'why can't I reach VM X' start with proxmox_get_guest_firewall_status.",
    input: { ...scopeArgs(ALL), raw: rawArg },
    handler: async (a) => {
      const s = await resolveScope(client, a, ALL);
      const rules = await client.get<Obj[]>(s.rules);
      rules.sort((x, y) => Number(x.pos) - Number(y.pos));
      return compact({
        ...s.target,
        digest: rules[0]?.digest,
        count: rules.length,
        enabled: rules.filter((r) => flag(r.enable)).length,
        rules: a.raw ? rules : rules.map(summarizeRule),
      });
    },
  });

  defineTool(ctx, "proxmox_get_firewall_rule", {
    toolset: "firewall",
    title: "Get a firewall rule",
    description: "Get one firewall rule by position at any level (cluster, node, guest or security group), with its digest for safe updates.",
    input: { ...scopeArgs(ALL), pos: z.number().int().min(0).describe("Rule position (from proxmox_list_firewall_rules)") },
    handler: async (a) => {
      const s = await resolveScope(client, a, ALL);
      const r = await client.get<Obj>(`${s.rules}/${a.pos}`);
      return compact({ ...s.target, ...summarizeRule({ pos: a.pos, ...r }), digest: r.digest });
    },
  });

  defineTool(ctx, "proxmox_save_firewall_rule", {
    toolset: "firewall",
    title: "Create, update or move a firewall rule",
    description:
      "Create a firewall rule (no `pos`; type and action required) or update the rule at `pos` (only the given fields change; `delete` clears fields). Works at cluster, node, guest and security-group level. " +
      "New rules are inserted at the top (position 0) unless `insert_at` is given; rules are evaluated top-down and the first match wins. Use `moveto` with `pos` to reorder. " +
      "Rules only take effect where the firewall is enabled (see proxmox_get_guest_firewall_status / proxmox_update_firewall_options), and new rules are disabled unless enable=true.",
    write: true,
    destructive: true,
    input: {
      ...scopeArgs(ALL),
      pos: z.number().int().min(0).optional().describe("Position of the rule to update; omit to create a new rule"),
      insert_at: z.number().int().min(0).optional().describe("When creating: position to insert the new rule at (default 0 = top)"),
      moveto: z.number().int().min(0).optional().describe("With pos: move the rule to this position (applied after any field changes)"),
      ...ruleFields,
      delete: deleteKeysArg,
      digest: digestArg,
      extra: extraArg,
    },
    handler: async (a) => {
      const s = await resolveScope(client, a, ALL);
      const fields = { ...ruleParams(a), delete: deleteParam(a.delete), ...a.extra };
      const changes = Object.values(fields).some((v) => v !== undefined);
      if (a.pos === undefined) {
        if (a.moveto !== undefined) throw new Error("moveto needs pos (the rule to move); use insert_at to place a new rule");
        if (!a.type || !a.action) throw new Error("type and action are required when creating a rule");
        if (a.delete?.length) throw new Error("delete only applies when updating a rule (give pos)");
        await client.post(s.rules, defined({ ...fields, pos: a.insert_at, digest: a.digest }));
        const pos = a.insert_at ?? 0;
        const created = await client.get<Obj>(`${s.rules}/${pos}`).catch(() => undefined);
        return compact({ ...s.target, created: true, pos, rule: created ? summarizeRule({ pos, ...created }) : undefined });
      }
      if (a.insert_at !== undefined) throw new Error("insert_at only applies when creating; use moveto to move an existing rule");
      if (!changes && a.moveto === undefined) throw new Error("Nothing to change: give rule fields, delete or moveto");
      if (changes) await client.put(`${s.rules}/${a.pos}`, defined({ ...fields, digest: a.digest }));
      // Proxmox ignores all other fields when moveto is present, so the move is a separate call.
      if (a.moveto !== undefined) await client.put(`${s.rules}/${a.pos}`, defined({ moveto: a.moveto, digest: changes ? undefined : a.digest }));
      const pos = a.moveto ?? a.pos;
      const r = await client.get<Obj>(`${s.rules}/${pos}`).catch(() => undefined);
      return compact({ ...s.target, updated: true, moved: a.moveto !== undefined ? { from: a.pos, to: a.moveto } : undefined, pos, rule: r ? summarizeRule({ pos, ...r }) : undefined });
    },
  });

  defineTool(ctx, "proxmox_delete_firewall_rule", {
    toolset: "firewall",
    title: "Delete a firewall rule",
    description:
      "Delete the firewall rule at `pos` (cluster, node, guest or security group). Later rules move up one position, so re-list before deleting several; pass `digest` to guard against concurrent edits.",
    delete: true,
    destructive: true,
    input: { ...scopeArgs(ALL), pos: z.number().int().min(0).describe("Position of the rule to delete"), digest: digestArg },
    handler: async (a) => {
      const s = await resolveScope(client, a, ALL);
      const before = await client.get<Obj>(`${s.rules}/${a.pos}`).catch(() => undefined);
      await client.delete(`${s.rules}/${a.pos}`, defined({ digest: a.digest }));
      return compact({ ...s.target, deleted: a.pos, rule: before ? ruleLine(before) : undefined });
    },
  });

  // ---------------------------------------------------------------- options

  defineTool(ctx, "proxmox_get_firewall_options", {
    toolset: "firewall",
    title: "Get firewall options",
    description:
      "Get firewall options for the datacenter (enable, default policies, ebtables, log rate limit), a node (enable, nftables, conntrack, synflood/smurf/tcpflags protection, log levels) or a guest (enable, policies, MAC/IP filter, DHCP, NDP, RA, log levels). `effective` fills in Proxmox's defaults for unset options.",
    input: { ...scopeArgs(OPT_SCOPES) },
    handler: async (a) => {
      const s = await resolveScope(client, a, OPT_SCOPES);
      const opts = await client.get<Obj>(`${s.fw}/options`);
      return compact({ ...s.target, options: compact({ ...opts, digest: undefined }), effective: effectiveOptions(s.scope as "cluster", opts), digest: opts.digest });
    },
  });

  defineTool(ctx, "proxmox_update_firewall_options", {
    toolset: "firewall",
    title: "Update firewall options",
    description:
      "Change firewall options at datacenter (cluster), node or guest level; only the given fields change. Options differ per level: " +
      `cluster: ${OPTION_KEYS.cluster.join(", ")}; node: ${OPTION_KEYS.node.join(", ")}; guest: ${OPTION_KEYS.guest.join(", ")}. ` +
      "Careful: enabling the datacenter firewall with policy_in DROP blocks everything not allowed by rules, including the web UI/SSH from networks outside the management IP set.",
    write: true,
    destructive: true,
    input: {
      ...scopeArgs(OPT_SCOPES),
      enable: z.boolean().optional().describe("Enable the firewall at this level (datacenter: the master switch for everything)"),
      policy_in: z.enum(["ACCEPT", "REJECT", "DROP"]).optional().describe("Default inbound policy (cluster, guest)"),
      policy_out: z.enum(["ACCEPT", "REJECT", "DROP"]).optional().describe("Default outbound policy (cluster, guest)"),
      policy_forward: z.enum(["ACCEPT", "DROP"]).optional().describe("Default forward policy (cluster; nftables)"),
      ebtables: z.boolean().optional().describe("Enable ebtables rules cluster wide (cluster)"),
      log_ratelimit: z.string().optional().describe("Log rate limit, e.g. 'enable=1,rate=1/second,burst=5' (cluster)"),
      log_level_in: logLevelArg("incoming traffic (node, guest)"),
      log_level_out: logLevelArg("outgoing traffic (node, guest)"),
      log_level_forward: logLevelArg("forwarded traffic (node)"),
      nftables: z.boolean().optional().describe("Use the nftables-based proxmox-firewall (node)"),
      ndp: z.boolean().optional().describe("Allow NDP / IPv6 neighbor discovery (node, guest)"),
      tcpflags: z.boolean().optional().describe("Filter illegal TCP flag combinations (node)"),
      tcp_flags_log_level: logLevelArg("the illegal TCP flags filter (node)"),
      nosmurfs: z.boolean().optional().describe("Enable the SMURFS filter (node)"),
      smurf_log_level: logLevelArg("the SMURFS filter (node)"),
      log_nf_conntrack: z.boolean().optional().describe("Log conntrack information (node)"),
      nf_conntrack_allow_invalid: z.boolean().optional().describe("Allow invalid packets on connection tracking (node)"),
      nf_conntrack_helpers: z.string().optional().describe("Conntrack helpers, e.g. 'ftp,tftp' (node)"),
      nf_conntrack_max: z.number().int().optional().describe("Maximum tracked connections (node)"),
      nf_conntrack_tcp_timeout_established: z.number().int().optional().describe("Conntrack established timeout in seconds (node)"),
      nf_conntrack_tcp_timeout_syn_recv: z.number().int().optional().describe("Conntrack SYN-RECV timeout in seconds (node)"),
      protection_synflood: z.boolean().optional().describe("Enable SYN flood protection (node)"),
      protection_synflood_rate: z.number().int().optional().describe("SYN flood rate limit, SYN/s per source IP (node)"),
      protection_synflood_burst: z.number().int().optional().describe("SYN flood burst per source IP (node)"),
      dhcp: z.boolean().optional().describe("Allow DHCP (guest)"),
      macfilter: z.boolean().optional().describe("MAC address filter: drop traffic from MACs other than the NIC's (guest)"),
      ipfilter: z.boolean().optional().describe("IP filter: only allow the guest's own IPs (ipfilter-netX IP sets / link-local) (guest)"),
      radv: z.boolean().optional().describe("Allow the guest to send IPv6 router advertisements (guest)"),
      delete: deleteKeysArg,
      digest: digestArg,
      extra: extraArg,
    },
    handler: async (a) => {
      const s = await resolveScope(client, a, OPT_SCOPES);
      const scope = s.scope as "cluster" | "node" | "guest";
      const all = new Set(Object.values(OPTION_KEYS).flat());
      const given = Object.keys(a).filter((k) => all.has(k) && (a as Obj)[k] !== undefined);
      const bad = given.filter((k) => !OPTION_KEYS[scope].includes(k));
      if (bad.length) throw new Error(`Not available at ${scope} level: ${bad.join(", ")} (allowed: ${OPTION_KEYS[scope].join(", ")})`);
      const params: Obj = defined({
        ...Object.fromEntries(given.map((k) => [k, (a as Obj)[k]])),
        delete: deleteParam(a.delete),
        digest: a.digest,
        ...a.extra,
      });
      if (!Object.keys(params).some((k) => k !== "digest")) throw new Error("Nothing to change");
      await client.put(`${s.fw}/options`, params);
      const opts = await client.get<Obj>(`${s.fw}/options`);
      return compact({ ...s.target, updated: Object.keys(params).filter((k) => k !== "digest"), effective: effectiveOptions(scope, opts) });
    },
  });

  // ---------------------------------------------------------------- aliases

  defineTool(ctx, "proxmox_list_firewall_aliases", {
    toolset: "firewall",
    title: "List firewall aliases",
    description: "List IP/network aliases defined at datacenter (cluster) or guest level. Aliases are named addresses usable in rule source/dest.",
    input: { ...scopeArgs(CG), search: searchArg },
    handler: async (a) => {
      const s = await resolveScope(client, a, CG);
      const items = await client.get<Obj[]>(`${s.fw}/aliases`);
      const res = filterAndLimit(items, { search: a.search }, (x) => [x.name, x.cidr, x.comment]);
      return { ...s.target, ...res, items: res.items.map((x) => compact({ name: x.name, cidr: x.cidr, comment: x.comment })) };
    },
  });

  defineTool(ctx, "proxmox_save_firewall_alias", {
    toolset: "firewall",
    title: "Create or update a firewall alias",
    description:
      "Create an IP/network alias, or update it if it already exists (cidr/comment; `new_name` renames it). Datacenter (cluster) or guest level. Rules referencing a renamed alias are not rewritten.",
    write: true,
    destructive: false,
    idempotent: true,
    input: {
      ...scopeArgs(CG),
      name: z.string().describe("Alias name"),
      cidr: z.string().optional().describe("IP address or network in CIDR form, e.g. 10.0.0.5 or 10.0.0.0/24 (required when creating)"),
      comment: z.string().optional().describe("Comment"),
      new_name: z.string().optional().describe("Rename the existing alias to this"),
      digest: digestArg,
    },
    handler: async (a) => {
      const s = await resolveScope(client, a, CG);
      const existing = (await client.get<Obj[]>(`${s.fw}/aliases`)).find((x) => x.name === a.name);
      if (!existing) {
        if (!a.cidr) throw new Error(`Alias ${a.name} does not exist; cidr is required to create it`);
        if (a.new_name) throw new Error(`Alias ${a.name} does not exist, so it can't be renamed`);
        await client.post(`${s.fw}/aliases`, defined({ name: a.name, cidr: a.cidr, comment: a.comment }));
        return { ...s.target, created: a.name, cidr: a.cidr };
      }
      // PUT requires cidr even when only the comment or name changes.
      await client.put(
        `${s.fw}/aliases/${enc(a.name)}`,
        defined({ cidr: a.cidr ?? existing.cidr, comment: a.comment, rename: a.new_name, digest: a.digest }),
      );
      return compact({ ...s.target, updated: a.new_name ?? a.name, renamed_from: a.new_name ? a.name : undefined, cidr: a.cidr ?? existing.cidr });
    },
  });

  defineTool(ctx, "proxmox_delete_firewall_alias", {
    toolset: "firewall",
    title: "Delete a firewall alias",
    description: "Delete an IP/network alias at datacenter (cluster) or guest level. Fails while rules still reference it.",
    delete: true,
    input: { ...scopeArgs(CG), name: z.string().describe("Alias name"), digest: digestArg },
    handler: async (a) => {
      const s = await resolveScope(client, a, CG);
      await client.delete(`${s.fw}/aliases/${enc(a.name)}`, defined({ digest: a.digest }));
      return { ...s.target, deleted: a.name };
    },
  });

  // ---------------------------------------------------------------- IP sets

  defineTool(ctx, "proxmox_list_firewall_ipsets", {
    toolset: "firewall",
    title: "List firewall IP sets",
    description:
      "List IP sets at datacenter (cluster) or guest level with their entries (CIDRs, nomatch exclusions, comments). Reference them in rules as +name (or +dc/name, +guest/name). Special sets: 'management' (hosts allowed to reach the web UI/SSH when the firewall is on), 'blacklist', and per-NIC 'ipfilter-netX' on guests.",
    input: { ...scopeArgs(CG), name: z.string().optional().describe("Only this IP set"), search: searchArg },
    handler: async (a) => {
      const s = await resolveScope(client, a, CG);
      let sets = await client.get<Obj[]>(`${s.fw}/ipset`);
      if (a.name) {
        sets = sets.filter((x) => x.name === a.name);
        if (!sets.length) throw new Error(`No IP set named ${a.name}`);
      }
      const res = filterAndLimit(sets, { search: a.search }, (x) => [x.name, x.comment]);
      const items = await Promise.all(
        res.items.map(async (set) => {
          const entries = await client.get<Obj[]>(`${s.fw}/ipset/${enc(set.name)}`);
          return compact({ name: set.name, comment: set.comment, count: entries.length, entries: entries.map(summarizeIpsetEntry), digest: entries[0]?.digest });
        }),
      );
      return { ...s.target, ...res, items };
    },
  });

  defineTool(ctx, "proxmox_save_firewall_ipset", {
    toolset: "firewall",
    title: "Create or update a firewall IP set",
    description:
      "Create an IP set, or update an existing one's comment / rename it (`new_name`). Datacenter (cluster) or guest level. Add entries with proxmox_save_ipset_entry.",
    write: true,
    destructive: false,
    idempotent: true,
    input: {
      ...scopeArgs(CG),
      name: z.string().describe("IP set name"),
      comment: z.string().optional().describe("Comment"),
      new_name: z.string().optional().describe("Rename the existing IP set to this"),
      digest: digestArg,
    },
    handler: async (a) => {
      const s = await resolveScope(client, a, CG);
      const exists = (await client.get<Obj[]>(`${s.fw}/ipset`)).some((x) => x.name === a.name);
      if (!exists) {
        if (a.new_name) throw new Error(`IP set ${a.name} does not exist, so it can't be renamed`);
        await client.post(`${s.fw}/ipset`, defined({ name: a.name, comment: a.comment }));
        return { ...s.target, created: a.name };
      }
      // Proxmox's convention: `rename` is the existing name and `name` the new one.
      await client.post(`${s.fw}/ipset`, defined({ name: a.new_name ?? a.name, rename: a.name, comment: a.comment, digest: a.digest }));
      return compact({ ...s.target, updated: a.new_name ?? a.name, renamed_from: a.new_name ? a.name : undefined });
    },
  });

  defineTool(ctx, "proxmox_delete_firewall_ipset", {
    toolset: "firewall",
    title: "Delete a firewall IP set",
    description: "Delete an IP set at datacenter (cluster) or guest level. Fails if it still has entries unless force=true.",
    delete: true,
    input: { ...scopeArgs(CG), name: z.string().describe("IP set name"), force: z.boolean().optional().describe("Also delete all its entries") },
    handler: async (a) => {
      const s = await resolveScope(client, a, CG);
      await client.delete(`${s.fw}/ipset/${enc(a.name)}`, defined({ force: a.force }));
      return { ...s.target, deleted: a.name };
    },
  });

  defineTool(ctx, "proxmox_save_ipset_entry", {
    toolset: "firewall",
    title: "Add or update an IP set entry",
    description:
      "Add an IP/network to an IP set, or update an existing entry's comment/nomatch flag. nomatch=true makes the entry an exclusion. Datacenter (cluster) or guest level.",
    write: true,
    destructive: false,
    idempotent: true,
    input: {
      ...scopeArgs(CG),
      ipset: z.string().describe("IP set name"),
      cidr: z.string().describe("IP, network in CIDR form, or alias name, e.g. 10.0.0.0/24"),
      comment: z.string().optional().describe("Comment"),
      nomatch: z.boolean().optional().describe("Exclude this address from the set instead of including it"),
      digest: digestArg,
    },
    handler: async (a) => {
      const s = await resolveScope(client, a, CG);
      const path = `${s.fw}/ipset/${enc(a.ipset)}`;
      const entries = await client.get<Obj[]>(path);
      const exists = entries.some((e) => e.cidr === a.cidr);
      if (exists) await client.put(`${path}/${enc(a.cidr)}`, defined({ comment: a.comment, nomatch: a.nomatch, digest: a.digest }));
      else await client.post(path, defined({ cidr: a.cidr, comment: a.comment, nomatch: a.nomatch }));
      return { ...s.target, ipset: a.ipset, [exists ? "updated" : "added"]: a.cidr };
    },
  });

  defineTool(ctx, "proxmox_delete_ipset_entry", {
    toolset: "firewall",
    title: "Remove an IP set entry",
    description: "Remove an IP/network from an IP set at datacenter (cluster) or guest level.",
    delete: true,
    input: {
      ...scopeArgs(CG),
      ipset: z.string().describe("IP set name"),
      cidr: z.string().describe("The entry to remove, exactly as listed, e.g. 10.0.0.0/24"),
      digest: digestArg,
    },
    handler: async (a) => {
      const s = await resolveScope(client, a, CG);
      await client.delete(`${s.fw}/ipset/${enc(a.ipset)}/${enc(a.cidr)}`, defined({ digest: a.digest }));
      return { ...s.target, ipset: a.ipset, removed: a.cidr };
    },
  });

  // ---------------------------------------------------------------- security groups

  defineTool(ctx, "proxmox_list_firewall_groups", {
    toolset: "firewall",
    title: "List firewall security groups",
    description:
      "List the datacenter's security groups (reusable rule sets) with their comment and rule count. See a group's rules with proxmox_list_firewall_rules scope=group; apply a group to a guest/node with a rule of type group.",
    input: { search: searchArg },
    handler: async (a) => {
      const groups = await client.get<Obj[]>("/cluster/firewall/groups");
      const res = filterAndLimit(groups, { search: a.search }, (g) => [g.group, g.comment]);
      const items = await Promise.all(
        res.items.map(async (g) => {
          const rules = await client.get<Obj[]>(`/cluster/firewall/groups/${enc(g.group)}`).catch(() => undefined);
          return compact({ group: g.group, comment: g.comment, rules: rules?.length, enabled_rules: rules?.filter((r) => flag(r.enable)).length });
        }),
      );
      return { ...res, items };
    },
  });

  defineTool(ctx, "proxmox_save_firewall_group", {
    toolset: "firewall",
    title: "Create or update a security group",
    description:
      "Create a datacenter security group, or update an existing one's comment / rename it (`new_name`). Add rules to it with proxmox_save_firewall_rule scope=group.",
    write: true,
    destructive: false,
    idempotent: true,
    input: {
      group: z.string().describe("Security group name (2-18 chars, letters, digits, - and _)"),
      comment: z.string().optional().describe("Comment"),
      new_name: z.string().optional().describe("Rename the existing group to this"),
      digest: digestArg,
    },
    handler: async (a) => {
      const exists = (await client.get<Obj[]>("/cluster/firewall/groups")).some((g) => g.group === a.group);
      if (!exists) {
        if (a.new_name) throw new Error(`Security group ${a.group} does not exist, so it can't be renamed`);
        await client.post("/cluster/firewall/groups", defined({ group: a.group, comment: a.comment }));
        return { created: a.group };
      }
      // As for IP sets, `rename` is the current name and `group` the new one.
      await client.post("/cluster/firewall/groups", defined({ group: a.new_name ?? a.group, rename: a.group, comment: a.comment, digest: a.digest }));
      return compact({ updated: a.new_name ?? a.group, renamed_from: a.new_name ? a.group : undefined });
    },
  });

  defineTool(ctx, "proxmox_delete_firewall_group", {
    toolset: "firewall",
    title: "Delete a security group",
    description: "Delete a datacenter security group. Proxmox refuses while the group still has rules or is referenced by rules; remove those first.",
    delete: true,
    input: { group: z.string().describe("Security group name") },
    handler: async (a) => {
      await client.delete(`/cluster/firewall/groups/${enc(a.group)}`);
      return { deleted: a.group };
    },
  });

  // ---------------------------------------------------------------- log, refs, macros

  defineTool(ctx, "proxmox_get_firewall_log", {
    toolset: "firewall",
    title: "Read the firewall log",
    description:
      "Read the firewall log of a node (all its traffic, including guests on it) or of one guest. Only packets matching rules/policies with a log level other than nolog are logged; set log_level_in on the options or `log` on a rule to see drops.",
    input: {
      ...scopeArgs(["node", "guest"] as const),
      limit: limitArg(100),
      start: z.number().int().min(0).optional().describe("Skip this many lines (paging)"),
      since: z.number().int().optional().describe("Only entries since this UNIX time"),
      until: z.number().int().optional().describe("Only entries until this UNIX time"),
      search: searchArg,
    },
    handler: async (a) => {
      const s = await resolveScope(client, a, ["node", "guest"]);
      const lines = await client.get<Obj[]>(`${s.fw}/log`, defined({ limit: a.limit ?? 100, start: a.start, since: a.since, until: a.until }));
      const res = filterAndLimit(lines, { search: a.search }, (l) => [l.t]);
      return { ...s.target, ...res, items: res.items.map((l) => l.t) };
    },
  });

  defineTool(ctx, "proxmox_list_firewall_refs", {
    toolset: "firewall",
    title: "List firewall alias/IP set references",
    description:
      "List the aliases and IP sets that can be used in rule source/dest at datacenter (cluster) or guest level, with the exact reference string to use (e.g. +dc/management, dc/myalias).",
    input: { ...scopeArgs(CG), type: z.enum(["alias", "ipset"]).optional().describe("Only aliases or only IP sets") },
    handler: async (a) => {
      const s = await resolveScope(client, a, CG);
      const refs = await client.get<Obj[]>(`${s.fw}/refs`, defined({ type: a.type }));
      return { ...s.target, items: refs.map((r) => compact({ ref: r.ref, type: r.type, name: r.name, scope: r.scope, comment: r.comment })) };
    },
  });

  defineTool(ctx, "proxmox_list_firewall_macros", {
    toolset: "firewall",
    title: "List firewall macros",
    description: "List the predefined firewall macros (SSH, HTTP, DNS, Ceph, ...) usable as a rule's `macro` instead of proto/port.",
    input: { search: searchArg },
    handler: async (a) => {
      const macros = await client.get<Obj[]>("/cluster/firewall/macros");
      const res = filterAndLimit(macros, { search: a.search }, (m) => [m.macro, m.descr]);
      return { ...res, items: res.items.map((m) => compact({ macro: m.macro, description: m.descr })) };
    },
  });

  // ---------------------------------------------------------------- whole picture

  defineTool(ctx, "proxmox_get_guest_firewall_status", {
    toolset: "firewall",
    title: "Get a guest's firewall status",
    description:
      "Whole-picture firewall view for one VM/container, for questions like 'why can't I reach VM X': whether the firewall is enabled at datacenter, node and guest level and on each NIC (firewall=1), the effective policies, the guest's rules (one line each), IP/MAC filters, and plain-language findings about what is blocking or not filtering traffic.",
    input: { vmid: vmidArg, node: z.string().optional().describe("Node the guest is on. Looked up automatically from the VMID when omitted.") },
    handler: async (a) => {
      const g = await client.resolveGuest(a.vmid, { node: a.node });
      const base = client.guestPath(g);
      const soft = <T>(p: Promise<T>) => p.catch((e: Error) => ({ error: e.message }) as unknown as T);
      const [dc, host, guest, config, rules] = await Promise.all([
        soft(client.get<Obj>("/cluster/firewall/options")),
        soft(client.get<Obj>(`/nodes/${enc(g.node)}/firewall/options`)),
        soft(client.get<Obj>(`${base}/firewall/options`)),
        client.get<Obj>(`${base}/config`),
        soft(client.get<Obj[]>(`${base}/firewall/rules`)),
      ]);
      const ok = (o: Obj) => !o?.error;
      const dcEff = ok(dc) ? effectiveOptions("cluster", dc) : undefined;
      const hostEff = ok(host) ? effectiveOptions("node", host) : undefined;
      const guestEff = ok(guest) ? effectiveOptions("guest", guest) : undefined;
      const nics = nicFirewall(config);
      const ruleList = Array.isArray(rules) ? [...rules].sort((x, y) => Number(x.pos) - Number(y.pos)) : [];
      const active = ruleList.filter((r) => flag(r.enable));

      const findings: string[] = [];
      if (dcEff && !dcEff.enable) findings.push("Datacenter firewall is disabled: no firewall rules are applied anywhere, so the Proxmox firewall is not blocking this guest.");
      if (guestEff && !guestEff.enable) findings.push("Guest firewall is disabled: the guest's rules and policies are not applied.");
      const unfiltered = nics.filter((n) => !n.firewall).map((n) => n.nic);
      if (unfiltered.length) findings.push(`NICs without firewall=1 bypass the guest firewall: ${unfiltered.join(", ")}.`);
      const filtering = Boolean(dcEff?.enable && guestEff?.enable && nics.some((n) => n.firewall));
      if (filtering && guestEff) {
        const inAccept = active.filter((r) => r.type === "in" && r.action === "ACCEPT").length;
        const groups = active.filter((r) => r.type === "group").length;
        if (guestEff.policy_in !== "ACCEPT" && inAccept === 0 && groups === 0) {
          findings.push(`Inbound policy is ${guestEff.policy_in} and no enabled inbound ACCEPT rules or security groups exist: all incoming traffic is blocked.`);
        } else if (guestEff.policy_in !== "ACCEPT") {
          findings.push(`Inbound policy is ${guestEff.policy_in}: only traffic matching an enabled ACCEPT rule (or security group) gets in.`);
        }
        if (guestEff.policy_out !== "ACCEPT") findings.push(`Outbound policy is ${guestEff.policy_out}: outgoing traffic needs explicit out rules.`);
        if (guestEff.ipfilter) findings.push("IP filter is on: traffic from addresses other than the guest's configured IPs (ipfilter-netX IP sets) is dropped.");
        const disabled = ruleList.length - active.length;
        if (disabled) findings.push(`${disabled} rule(s) are disabled.`);
        if (guestEff.log_level_in === "nolog" || !guestEff.log_level_in) findings.push("Inbound drops are not logged; set log_level_in (proxmox_update_firewall_options) and read proxmox_get_firewall_log to see them.");
      }
      if (hostEff && !hostEff.enable) findings.push("The node's host firewall is disabled; this affects only traffic to the host itself, not guest rules.");

      return compact({
        vmid: g.vmid,
        name: g.name,
        node: g.node,
        type: g.type === "qemu" ? "vm" : "container",
        filtering_active: filtering,
        datacenter: dcEff
          ? compact({ enabled: dcEff.enable, policy_in: dcEff.policy_in, policy_out: dcEff.policy_out, policy_forward: dcEff.policy_forward })
          : { error: dc.error },
        node_firewall: hostEff ? compact({ enabled: hostEff.enable, nftables: hostEff.nftables }) : { error: host.error },
        guest_firewall: guestEff
          ? compact({
              enabled: guestEff.enable,
              policy_in: guestEff.policy_in,
              policy_out: guestEff.policy_out,
              macfilter: guestEff.macfilter,
              ipfilter: guestEff.ipfilter,
              dhcp: guestEff.dhcp,
              ndp: guestEff.ndp,
              radv: guestEff.radv,
              log_level_in: guestEff.log_level_in,
              log_level_out: guestEff.log_level_out,
            })
          : { error: guest.error },
        nics,
        rules: Array.isArray(rules)
          ? { count: ruleList.length, enabled: active.length, list: ruleList.map((r) => `${r.pos}: ${ruleLine(r)}`) }
          : { error: (rules as Obj).error },
        findings,
      });
    },
  });
}
