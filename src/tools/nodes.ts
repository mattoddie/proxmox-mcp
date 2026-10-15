import { z } from "zod";
import type { ProxmoxClient } from "../proxmox/client.js";
import { compact, formatBytes, formatUptime, fractionPct, isoTime, parsePropertyString, redactPluginData, redactSecrets, usage } from "../proxmox/format.js";
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
  nodeArg,
  nodeRequiredArg,
  rawArg,
  runTask,
  searchArg,
  waitArg,
  withExtra,
  type ToolContext,
} from "./util.js";

type Obj = Record<string, any>;

const nodePath = (node: string) => `/nodes/${enc(node)}`;

/** Cuts long text (reports, changelogs) to `max` characters, saying so. */
function truncate(text: unknown, max: number): Obj {
  const s = typeof text === "string" ? text : text === undefined || text === null ? "" : JSON.stringify(text);
  if (s.length <= max) return { text: s };
  return { text: s.slice(0, max), truncated: true, total_chars: s.length, note: `Truncated to ${max} of ${s.length} characters; raise max_chars to see more` };
}

/** Accepts epoch seconds (number or numeric string) or an ISO / "YYYY-MM-DD HH:MM:SS" date and returns epoch seconds. */
function toEpoch(value: string | number | undefined): number | undefined {
  if (value === undefined) return undefined;
  if (typeof value === "number") return Math.floor(value);
  if (/^\d+$/.test(value.trim())) return Number(value.trim());
  const ms = Date.parse(value.includes("T") || /[zZ]|[+-]\d\d:?\d\d$/.test(value) ? value : value.replace(" ", "T"));
  if (!Number.isFinite(ms)) throw new Error(`Not a valid time: "${value}" (use epoch seconds or e.g. 2026-01-31T12:00:00Z)`);
  return Math.floor(ms / 1000);
}

/** Interface lists (bridge ports, bond slaves) are space-separated in /etc/network/interfaces. */
const ifaceList = (v?: string[]) => (v ? v.join(" ") : undefined);

const summarizeSubscription = (s: Obj | undefined): Obj | undefined =>
  s
    ? compact({
        status: s.status,
        level: s.level,
        product: s.productname,
        next_due_date: s.nextduedate,
        registered: s.regdate,
        sockets: s.sockets,
        message: s.message,
        last_checked: isoTime(s.checktime),
        // The key itself is a credential; only say whether one is set.
        key_set: s.key ? true : undefined,
      })
    : undefined;

/** Certificate details without the PEM body (public, but large and noisy). */
function summarizeCert(c: Obj): Obj {
  const notafter = Number(c.notafter);
  return compact({
    filename: c.filename,
    subject: c.subject,
    issuer: c.issuer,
    san: c.san,
    not_before: isoTime(c.notbefore),
    not_after: isoTime(c.notafter),
    days_left: Number.isFinite(notafter) && notafter > 0 ? Math.floor((notafter * 1000 - Date.now()) / 86400000) : undefined,
    fingerprint: c.fingerprint,
    public_key: c["public-key-type"] ? `${c["public-key-type"]}${c["public-key-bits"] ? ` ${c["public-key-bits"]} bits` : ""}` : undefined,
  });
}

function summarizeIface(i: Obj): Obj {
  return compact({
    iface: i.iface,
    type: i.type,
    active: i.active !== undefined ? Boolean(i.active) : false,
    autostart: i.autostart !== undefined ? Boolean(i.autostart) : undefined,
    exists: i.exists !== undefined ? Boolean(i.exists) : undefined,
    method: i.method,
    cidr: i.cidr ?? (i.address ? `${i.address}${i.netmask ? `/${i.netmask}` : ""}` : undefined),
    gateway: i.gateway,
    method6: i.method6,
    cidr6: i.cidr6 ?? (i.address6 ? `${i.address6}${i.netmask6 ? `/${i.netmask6}` : ""}` : undefined),
    gateway6: i.gateway6,
    mtu: i.mtu,
    bridge_ports: i.bridge_ports,
    bridge_vlan_aware: i.bridge_vlan_aware ? true : undefined,
    bridge_vids: i.bridge_vids,
    slaves: i.slaves,
    bond_mode: i.bond_mode,
    bond_primary: i["bond-primary"],
    bond_xmit_hash_policy: i.bond_xmit_hash_policy,
    vlan_raw_device: i["vlan-raw-device"],
    vlan_id: i["vlan-id"],
    ovs_bridge: i.ovs_bridge,
    ovs_ports: i.ovs_ports,
    ovs_bonds: i.ovs_bonds,
    ovs_tag: i.ovs_tag,
    comments: typeof i.comments === "string" ? i.comments.trim() : i.comments,
  });
}

// ---------------------------------------------------------------- RRD statistics

/** Byte-valued RRD metrics (net* are bytes per second). */
const BYTE_METRIC_RE = /^(mem|swap|root|arc|net)/;
/** Metrics Proxmox reports as 0..1 fractions. */
const FRACTION_METRICS = new Set(["cpu", "iowait"]);
const SERIES_METRICS = ["cpu", "iowait", "loadavg", "memused", "netin", "netout"];

function formatMetric(name: string, v: number): string | number {
  if (FRACTION_METRICS.has(name)) return fractionPct(v) as number;
  if (BYTE_METRIC_RE.test(name)) return `${formatBytes(Math.round(v))}${name.startsWith("net") ? "/s" : ""}`;
  return Math.round(v * 100) / 100;
}

const metricLabel = (name: string) => (FRACTION_METRICS.has(name) ? `${name}_pct` : name);

export function summarizeRrd(rows: Obj[], opts: { metrics?: string[]; points: number }): Obj {
  const valid = rows.filter((r) => r && Number.isFinite(Number(r.time)));
  const names = new Set<string>();
  for (const r of valid) for (const [k, v] of Object.entries(r)) if (k !== "time" && Number.isFinite(Number(v)) && v !== null) names.add(k);
  const wanted = opts.metrics?.length ? [...names].filter((n) => opts.metrics!.includes(n)) : [...names].sort();
  const metrics: Obj = {};
  for (const name of wanted) {
    const values = valid.map((r) => r[name]).filter((v) => v !== undefined && v !== null && Number.isFinite(Number(v))).map(Number);
    if (!values.length) continue;
    const avg = values.reduce((a, b) => a + b, 0) / values.length;
    metrics[metricLabel(name)] = {
      min: formatMetric(name, Math.min(...values)),
      avg: formatMetric(name, avg),
      max: formatMetric(name, Math.max(...values)),
      last: formatMetric(name, values.at(-1)!),
    };
  }
  // Downsample to `points` buckets by averaging so the model sees the shape without hundreds of rows.
  const seriesNames = (opts.metrics?.length ? wanted : SERIES_METRICS).filter((n) => names.has(n));
  const series: Obj[] = [];
  const buckets = Math.min(opts.points, valid.length);
  for (let b = 0; b < buckets; b++) {
    const slice = valid.slice(Math.floor((b * valid.length) / buckets), Math.floor(((b + 1) * valid.length) / buckets));
    const point: Obj = { time: isoTime(slice.at(-1)!.time) };
    for (const name of seriesNames) {
      const vals = slice.map((r) => r[name]).filter((v) => v !== undefined && v !== null && Number.isFinite(Number(v))).map(Number);
      if (vals.length) point[metricLabel(name)] = formatMetric(name, vals.reduce((x, y) => x + y, 0) / vals.length);
    }
    series.push(point);
  }
  return compact({
    samples: valid.length,
    from: isoTime(valid[0]?.time),
    to: isoTime(valid.at(-1)?.time),
    metrics,
    series,
  });
}

// ---------------------------------------------------------------- APT

function summarizeRepositories(r: Obj): Obj {
  const infos: Obj[] = r.infos ?? [];
  const repos: Obj[] = [];
  for (const file of r.files ?? []) {
    (file.repositories ?? []).forEach((repo: Obj, index: number) => {
      const notes = infos
        .filter((i) => i.path === file.path && Number(i.index) === index)
        .map((i) => `${i.kind}: ${i.message}${i.property ? ` (${i.property})` : ""}`);
      repos.push(
        compact({
          path: file.path,
          index,
          enabled: Boolean(repo.Enabled),
          types: repo.Types?.join(" "),
          uris: repo.URIs?.join(" "),
          suites: repo.Suites?.join(" "),
          components: repo.Components?.join(" "),
          comment: typeof repo.Comment === "string" ? repo.Comment.trim() : undefined,
          notes,
        }),
      );
    });
  }
  return compact({
    digest: r.digest,
    repositories: repos,
    errors: (r.errors ?? []).map((e: Obj) => `${e.path}: ${e.error}`),
    warnings: infos.filter((i) => i.kind === "warning").map((i) => `${i.path}#${i.index}: ${i.message}`),
    standard_repos: (r["standard-repos"] ?? []).map((s: Obj) => ({
      handle: s.handle,
      name: s.name,
      status: s.status === undefined || s.status === null ? "not configured" : s.status ? "enabled" : "disabled",
    })),
  });
}

// ---------------------------------------------------------------- ACME

function summarizePlugin(p: Obj): Obj {
  return compact({
    ...redactSecrets({ ...p, data: undefined }),
    data: undefined,
    data_keys: redactPluginData(p.data),
  });
}

/** Encodes plugin credentials the way the web UI does: KEY=value lines, base64. */
const encodePluginData = (data: Record<string, string>) =>
  Buffer.from(
    Object.entries(data)
      .map(([k, v]) => `${k}=${v}`)
      .join("\n"),
  ).toString("base64");

async function getNodeConfig(client: ProxmoxClient, node: string): Promise<Obj> {
  const cfg = await client.get<Obj>(`${nodePath(node)}/config`);
  const out: Obj = { ...cfg };
  for (const [k, v] of Object.entries(cfg)) {
    if (k === "acme" || /^acmedomain\d+$/.test(k)) out[k] = parsePropertyString(v, k === "acme" ? "account" : "domain");
    if (k === "wakeonlan") out[k] = parsePropertyString(v, "mac");
    if (k === "location") out[k] = parsePropertyString(v);
  }
  return out;
}

const SERVICE_ACTIONS = ["start", "stop", "restart", "reload"] as const;
const IFACE_TYPES = ["bridge", "bond", "eth", "alias", "vlan", "fabric", "OVSBridge", "OVSBond", "OVSPort", "OVSIntPort", "vnet", "unknown"] as const;
const BOND_MODES = ["balance-rr", "active-backup", "balance-xor", "broadcast", "802.3ad", "balance-tlb", "balance-alb", "balance-slb", "lacp-balance-slb", "lacp-balance-tcp"] as const;

const maxCharsArg = (def: number) =>
  z.number().int().positive().max(5_000_000).optional().describe(`Truncate the text to this many characters (default ${def})`);
const timeArg = (what: string) =>
  z.union([z.string(), z.number()]).optional().describe(`${what}: epoch seconds or a date like "2026-01-31 12:00:00" / ISO 8601`);

export function registerNodesTools(ctx: ToolContext): void {
  const { client } = ctx;

  // ================================================================ status & statistics

  defineTool(ctx, "proxmox_get_node_status", {
    toolset: "nodes",
    title: "Get node status",
    description:
      "Detailed status of one node: CPU model/sockets/cores and usage, IO wait, load average, memory/swap/root filesystem usage, kernel, Proxmox VE version, boot mode (EFI/BIOS, secure boot), uptime, KSM sharing and the subscription status. Use proxmox_list_nodes for all nodes at a glance and proxmox_get_node_stats for history.",
    input: { node: nodeArg, raw: rawArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const [st, version, sub] = await Promise.all([
        client.get<Obj>(`${nodePath(node)}/status`),
        client.get<Obj>(`${nodePath(node)}/version`).catch(() => undefined),
        client.get<Obj>(`${nodePath(node)}/subscription`).catch(() => undefined),
      ]);
      if (a.raw) return redactSecrets({ status: st, version, subscription: summarizeSubscription(sub) });
      const ci = st.cpuinfo ?? {};
      const kernel = st["current-kernel"];
      return compact({
        node,
        uptime: formatUptime(st.uptime),
        cpu_pct: fractionPct(st.cpu),
        iowait_pct: fractionPct(st.wait),
        loadavg: st.loadavg,
        cpu: compact({ model: ci.model, sockets: ci.sockets, cores: ci.cores, threads: ci.cpus, mhz: ci.mhz ? Number(ci.mhz) : undefined }),
        memory: usage(st.memory?.used, st.memory?.total),
        memory_available: formatBytes(st.memory?.available),
        swap: usage(st.swap?.used, st.swap?.total),
        root_fs: usage(st.rootfs?.used, st.rootfs?.total),
        ksm_shared: st.ksm?.shared ? formatBytes(st.ksm.shared) : undefined,
        kernel: kernel ? `${kernel.sysname ?? ""} ${kernel.release ?? ""} (${kernel.machine ?? "?"})`.trim() : st.kversion,
        pve_version: st.pveversion,
        version: version ? `${version.version} (release ${version.release}, ${version.repoid})` : undefined,
        boot: st["boot-info"] ? compact({ mode: st["boot-info"].mode, secure_boot: st["boot-info"].secureboot }) : undefined,
        subscription: summarizeSubscription(sub),
      });
    },
  });

  defineTool(ctx, "proxmox_get_node_stats", {
    toolset: "nodes",
    title: "Get node performance history",
    description:
      "Node performance history from the RRD database (CPU, IO wait, load, memory, swap, root disk, network, ARC...) over a timeframe, summarized as min/avg/max/last per metric plus a short downsampled series. Network values are bytes per second.",
    input: {
      node: nodeArg,
      timeframe: z.enum(["hour", "day", "week", "month", "year", "decade"]).optional().describe("Time window (default hour)"),
      cf: z.enum(["AVERAGE", "MAX"]).optional().describe("RRD consolidation function (default AVERAGE)"),
      metrics: z.array(z.string()).optional().describe("Only these metrics, e.g. [\"cpu\", \"memused\", \"netin\"] (default all)"),
      points: z.number().int().min(1).max(500).optional().describe("Number of points in the downsampled series (default 12)"),
      raw: rawArg,
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const timeframe = a.timeframe ?? "hour";
      const rows = await client.get<Obj[]>(`${nodePath(node)}/rrddata`, defined({ timeframe, cf: a.cf }));
      if (a.raw) return rows;
      return { node, timeframe, cf: a.cf ?? "AVERAGE", ...summarizeRrd(rows, { metrics: a.metrics, points: a.points ?? 12 }) };
    },
  });

  defineTool(ctx, "proxmox_get_node_netstat", {
    toolset: "nodes",
    title: "Get guest network counters",
    description: "Traffic counters of the guests' tap/veth network devices on a node (bytes in/out per device and VMID), as seen from the host.",
    input: { node: nodeArg, vmid: z.number().int().optional().describe("Only this guest") },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      let rows = await client.get<Obj[]>(`${nodePath(node)}/netstat`);
      if (a.vmid !== undefined) rows = rows.filter((r) => Number(r.vmid) === a.vmid);
      return rows.map((r) => compact({ vmid: r.vmid !== undefined ? Number(r.vmid) : undefined, dev: r.dev, in: formatBytes(r.in), out: formatBytes(r.out) }));
    },
  });

  defineTool(ctx, "proxmox_get_node_report", {
    toolset: "nodes",
    title: "Get node system report",
    description:
      "The node's pvereport: a long text dump of versions, hardware, storage, network, cluster and guest configuration, as used for support requests. Large; truncated to max_chars. Can take a while to generate.",
    input: { node: nodeArg, max_chars: maxCharsArg(50000) },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      return { node, ...truncate(await client.get(`${nodePath(node)}/report`), a.max_chars ?? 50000) };
    },
  });

  defineTool(ctx, "proxmox_query_url_metadata", {
    toolset: "nodes",
    title: "Query URL metadata",
    description:
      "Ask a node to look up a URL's file name, size and MIME type (HEAD request from the node). Useful before proxmox_download_to_storage to check the URL and pick the file name.",
    input: {
      node: nodeArg,
      url: z.string().describe("The URL to inspect, e.g. https://cdimage.debian.org/.../debian-13.iso"),
      verify_certificates: z.boolean().optional().describe("Verify TLS certificates (default true)"),
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const m = await client.get<Obj>(`${nodePath(node)}/query-url-metadata`, defined({ url: a.url, "verify-certificates": a.verify_certificates }));
      return compact({ url: a.url, filename: m?.filename, mimetype: m?.mimetype, size: formatBytes(m?.size), size_bytes: m?.size });
    },
  });

  // ================================================================ services

  defineTool(ctx, "proxmox_list_node_services", {
    toolset: "nodes",
    title: "List node services",
    description:
      "List the Proxmox-related system services on a node (pveproxy, pvedaemon, corosync, pve-cluster, chrony, sshd, postfix...) with their running state and whether they are enabled. Failed services are listed first.",
    input: { node: nodeArg, search: searchArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const services = await client.get<Obj[]>(`${nodePath(node)}/services`);
      const rank = (s: Obj) => (s["active-state"] === "failed" ? 0 : 1);
      services.sort((x, y) => rank(x) - rank(y) || String(x.service).localeCompare(String(y.service)));
      const res = filterAndLimit(services, { search: a.search }, (s) => [s.service, s.name, s.desc]);
      return {
        node,
        failed: services.filter((s) => s["active-state"] === "failed").map((s) => s.service),
        ...res,
        items: res.items.map((s) =>
          compact({ service: s.service, description: s.desc, active: s["active-state"], state: s.state, unit: s["unit-state"] }),
        ),
      };
    },
  });

  defineTool(ctx, "proxmox_manage_node_service", {
    toolset: "nodes",
    title: "Start, stop, restart or reload a node service",
    description:
      "Start, stop, restart or reload a system service on a node, e.g. restart pveproxy after a certificate change. Stopping pve-cluster, corosync, pveproxy or pvedaemon can cut off API access or cluster communication; prefer reload where possible.",
    write: true,
    destructive: true,
    input: {
      node: nodeArg,
      service: z.string().describe("Service ID, e.g. pveproxy, pvedaemon, pvestatd, corosync, pve-cluster, chrony, sshd, postfix (see proxmox_list_node_services)"),
      action: z.enum(SERVICE_ACTIONS).describe("What to do; reload falls back to restart if the service can't reload"),
      wait: waitArg,
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const base = `${nodePath(node)}/services/${enc(a.service)}`;
      const upid = await client.post(`${base}/${a.action}`);
      const res = await runTask(client, upid, a.wait, { node, service: a.service, action: a.action });
      if (a.wait === false) return res;
      const state = await client.get<Obj>(`${base}/state`).catch(() => undefined);
      return compact({ ...res, state: state ? compact({ active: state["active-state"], state: state.state, unit: state["unit-state"] }) : undefined });
    },
  });

  // ================================================================ network

  defineTool(ctx, "proxmox_list_node_network", {
    toolset: "nodes",
    title: "List node network interfaces",
    description:
      "List a node's network configuration: physical NICs, Linux bridges, bonds, VLANs and OVS objects with addresses, gateways, ports and comments. Interfaces that are configured but not active are flagged; that usually means pending (unapplied) changes from proxmox_save_node_network_interface, or a down link. Apply with proxmox_apply_node_network or discard with proxmox_revert_node_network.",
    input: {
      node: nodeArg,
      type: z
        .enum(["bridge", "bond", "eth", "alias", "vlan", "fabric", "OVSBridge", "OVSBond", "OVSPort", "OVSIntPort", "vnet", "any_bridge", "any_local_bridge", "include_sdn"])
        .optional()
        .describe("Only this interface type (any_bridge = Linux and OVS bridges)"),
      search: searchArg,
      raw: rawArg,
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const ifaces = await client.get<Obj[]>(`${nodePath(node)}/network`, defined({ type: a.type }));
      // Proxmox returns the pending diff as a top-level `changes` attribute; the client exposes it if it keeps it.
      const changes: unknown = (ifaces as any).changes;
      ifaces.sort((x, y) => String(x.iface).localeCompare(String(y.iface), undefined, { numeric: true }));
      const res = filterAndLimit(ifaces, { search: a.search }, (i) => [i.iface, i.type, i.cidr, i.address, i.comments, i.bridge_ports, i.slaves]);
      const items = a.raw ? res.items : res.items.map(summarizeIface);
      const inactive = res.items.filter((i) => !i.active && i.type !== "unknown").map((i) => i.iface);
      return compact({
        node,
        total: res.total,
        inactive_or_pending: inactive,
        pending_changes: typeof changes === "string" && changes ? changes : undefined,
        note: inactive.length
          ? "Interfaces listed in inactive_or_pending are not up: either the link is down or the change is pending until proxmox_apply_node_network."
          : undefined,
        items,
      });
    },
  });

  defineTool(ctx, "proxmox_get_node_network_interface", {
    toolset: "nodes",
    title: "Get a node network interface",
    description: "Get the configuration of one network interface on a node (as stored in /etc/network/interfaces, including pending changes).",
    input: { node: nodeArg, iface: z.string().describe("Interface name, e.g. vmbr0, bond0, eno1, vmbr0.20"), raw: rawArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const i = await client.get<Obj>(`${nodePath(node)}/network/${enc(a.iface)}`);
      return a.raw ? i : summarizeIface({ iface: a.iface, ...i });
    },
  });

  defineTool(ctx, "proxmox_save_node_network_interface", {
    toolset: "nodes",
    title: "Create or update a node network interface",
    description:
      "Create (create=true) or update a network interface on a node: Linux bridge, bond, VLAN, NIC address etc. Changes are only written to /etc/network/interfaces.new and stay pending until proxmox_apply_node_network (or are discarded with proxmox_revert_node_network). On update, only the given fields change; remove settings with `delete`. Lists of ports/slaves are given as arrays.",
    write: true,
    destructive: false,
    input: {
      node: nodeArg,
      iface: z.string().describe("Interface name, e.g. vmbr1, bond0, vmbr0.20, vlan20"),
      create: z.boolean().optional().describe("Create a new interface (default false = update an existing one)"),
      type: z.enum(IFACE_TYPES).optional().describe("Interface type (required when creating; looked up when updating)"),
      cidr: z.string().optional().describe("IPv4 address with prefix, e.g. 192.168.1.10/24"),
      gateway: z.string().optional().describe("IPv4 default gateway"),
      cidr6: z.string().optional().describe("IPv6 address with prefix"),
      gateway6: z.string().optional().describe("IPv6 default gateway"),
      bridge_ports: z.array(z.string()).optional().describe("Bridge ports, e.g. [\"eno1\"] (bridge); [] for none"),
      bridge_vlan_aware: z.boolean().optional().describe("Make the bridge VLAN aware (bridge)"),
      bridge_vids: z.string().optional().describe("Allowed VLANs on a VLAN-aware bridge, e.g. '2-4094' or '10 20 100-200'"),
      slaves: z.array(z.string()).optional().describe("Bond member interfaces, e.g. [\"eno1\", \"eno2\"] (bond)"),
      bond_mode: z.enum(BOND_MODES).optional().describe("Bonding mode (bond)"),
      bond_primary: z.string().optional().describe("Primary interface for active-backup bonds"),
      bond_xmit_hash_policy: z.enum(["layer2", "layer2+3", "layer3+4"]).optional().describe("Transmit hash policy for balance-xor/802.3ad bonds"),
      vlan_raw_device: z.string().optional().describe("Parent interface of a VLAN, e.g. vmbr0 (vlan)"),
      vlan_id: z.number().int().min(1).max(4094).optional().describe("VLAN tag for a custom-named VLAN interface (vlan)"),
      mtu: z.number().int().min(1280).max(65520).optional().describe("MTU"),
      autostart: z.boolean().optional().describe("Bring the interface up at boot"),
      comments: z.string().optional().describe("Comment"),
      delete: deleteKeysArg,
      extra: extraArg,
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      let type: string | undefined = a.type;
      if (!type) {
        if (a.create) throw new Error("type is required when creating an interface");
        // The update endpoint requires `type` too, so keep the current one.
        type = (await client.get<Obj>(`${nodePath(node)}/network/${enc(a.iface)}`)).type;
      }
      const params = defined(withExtra({
        type,
        cidr: a.cidr,
        gateway: a.gateway,
        cidr6: a.cidr6,
        gateway6: a.gateway6,
        bridge_ports: ifaceList(a.bridge_ports),
        bridge_vlan_aware: a.bridge_vlan_aware,
        bridge_vids: a.bridge_vids,
        slaves: ifaceList(a.slaves),
        bond_mode: a.bond_mode,
        "bond-primary": a.bond_primary,
        bond_xmit_hash_policy: a.bond_xmit_hash_policy,
        "vlan-raw-device": a.vlan_raw_device,
        "vlan-id": a.vlan_id,
        mtu: a.mtu,
        autostart: a.autostart,
        comments: a.comments,
      }, a.extra));
      if (a.create) {
        await client.post(`${nodePath(node)}/network`, { iface: a.iface, ...params });
      } else {
        await client.put(`${nodePath(node)}/network/${enc(a.iface)}`, defined({ ...params, delete: deleteParam(a.delete) }));
      }
      return {
        node,
        iface: a.iface,
        [a.create ? "created" : "updated"]: true,
        pending: true,
        note: "Pending until applied: review with proxmox_list_node_network, then proxmox_apply_node_network (or proxmox_revert_node_network to discard).",
      };
    },
  });

  defineTool(ctx, "proxmox_delete_node_network_interface", {
    toolset: "nodes",
    title: "Delete a node network interface",
    description:
      "Remove a network interface (bridge, bond, VLAN...) from a node's configuration. Like other network edits it stays pending until proxmox_apply_node_network. Guests attached to a removed bridge lose connectivity once applied.",
    delete: true,
    input: { node: nodeArg, iface: z.string().describe("Interface name, e.g. vmbr1") },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      await client.delete(`${nodePath(node)}/network/${enc(a.iface)}`);
      return { node, deleted: a.iface, pending: true, note: "Apply with proxmox_apply_node_network or discard with proxmox_revert_node_network." };
    },
  });

  defineTool(ctx, "proxmox_apply_node_network", {
    toolset: "nodes",
    title: "Apply pending network changes",
    description:
      "Apply a node's pending network changes (ifreload). Can interrupt connectivity to the node and its guests, and a wrong bridge/gateway can lock you out of the node; review pending changes with proxmox_list_node_network first.",
    write: true,
    destructive: true,
    input: {
      node: nodeArg,
      regenerate_frr: z.boolean().optional().describe("Also regenerate the FRR (SDN fabric routing) configuration"),
      wait: waitArg,
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const upid = await client.put(`${nodePath(node)}/network`, defined({ "regenerate-frr": a.regenerate_frr }));
      return runTask(client, upid, a.wait, { node, applied: true });
    },
  });

  defineTool(ctx, "proxmox_revert_node_network", {
    toolset: "nodes",
    title: "Discard pending network changes",
    description: "Discard all pending (unapplied) network changes on a node, keeping the running configuration.",
    write: true,
    destructive: false,
    idempotent: true,
    input: { node: nodeArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      await client.delete(`${nodePath(node)}/network`);
      return { node, reverted: true };
    },
  });

  // ================================================================ DNS, hosts, time

  defineTool(ctx, "proxmox_get_node_dns", {
    toolset: "nodes",
    title: "Get node DNS settings",
    description: "Get a node's DNS resolver settings: search domain and up to three name servers.",
    input: { node: nodeArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      return { node, ...(await client.get<Obj>(`${nodePath(node)}/dns`)) };
    },
  });

  defineTool(ctx, "proxmox_set_node_dns", {
    toolset: "nodes",
    title: "Set node DNS settings",
    description:
      "Set a node's DNS search domain and name servers (/etc/resolv.conf). Fields left out keep their current value; pass an empty string to clear dns2/dns3.",
    write: true,
    destructive: true,
    idempotent: true,
    input: {
      node: nodeArg,
      search: z.string().optional().describe("Search domain, e.g. example.lan"),
      dns1: z.string().optional().describe("First name server IP"),
      dns2: z.string().optional().describe("Second name server IP"),
      dns3: z.string().optional().describe("Third name server IP"),
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const cur = await client.get<Obj>(`${nodePath(node)}/dns`);
      // The API replaces the whole resolver config, so start from the current values.
      const merged: Obj = { search: cur.search, dns1: cur.dns1, dns2: cur.dns2, dns3: cur.dns3, ...defined({ search: a.search, dns1: a.dns1, dns2: a.dns2, dns3: a.dns3 }) };
      if (!merged.search) throw new Error("search (domain) is required");
      await client.put(`${nodePath(node)}/dns`, compact(merged));
      return { node, updated: true, dns: await client.get(`${nodePath(node)}/dns`) };
    },
  });

  defineTool(ctx, "proxmox_get_node_hosts", {
    toolset: "nodes",
    title: "Get node /etc/hosts",
    description: "Read a node's /etc/hosts file and its digest (pass the digest to proxmox_set_node_hosts to avoid overwriting concurrent edits).",
    input: { node: nodeArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const h = await client.get<Obj>(`${nodePath(node)}/hosts`);
      return { node, digest: h.digest, content: h.data };
    },
  });

  defineTool(ctx, "proxmox_set_node_hosts", {
    toolset: "nodes",
    title: "Write node /etc/hosts",
    description:
      "Replace a node's /etc/hosts with the given content. Read it first with proxmox_get_node_hosts and pass its digest. The node's own name must keep resolving to its cluster IP or pve-cluster can break.",
    write: true,
    destructive: true,
    idempotent: true,
    input: {
      node: nodeArg,
      content: z.string().describe("The complete new content of /etc/hosts"),
      digest: digestArg,
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      await client.post(`${nodePath(node)}/hosts`, defined({ data: a.content, digest: a.digest }));
      return { node, updated: true };
    },
  });

  defineTool(ctx, "proxmox_get_node_time", {
    toolset: "nodes",
    title: "Get node time and time zone",
    description: "Get a node's current time (UTC), local time and configured time zone. Useful for spotting clock drift between nodes.",
    input: { node: nodeArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const t = await client.get<Obj>(`${nodePath(node)}/time`);
      const offset = Number(t.localtime) - Number(t.time);
      return compact({
        node,
        timezone: t.timezone,
        utc: isoTime(t.time),
        utc_offset_hours: Number.isFinite(offset) ? Math.round((offset / 3600) * 100) / 100 : undefined,
        drift_s: Number.isFinite(Number(t.time)) ? Math.round(Number(t.time) - Date.now() / 1000) : undefined,
      });
    },
  });

  defineTool(ctx, "proxmox_set_node_timezone", {
    toolset: "nodes",
    title: "Set node time zone",
    description: "Set a node's time zone, e.g. Europe/London or UTC.",
    write: true,
    destructive: false,
    idempotent: true,
    input: { node: nodeArg, timezone: z.string().describe("Time zone name from zone.tab, e.g. Europe/London, America/New_York, UTC") },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      await client.put(`${nodePath(node)}/time`, { timezone: a.timezone });
      return { node, timezone: a.timezone, updated: true };
    },
  });

  // ================================================================ node config

  defineTool(ctx, "proxmox_get_node_config", {
    toolset: "nodes",
    title: "Get node options",
    description:
      "Get a node's own options (/etc/pve/nodes/<node>/config): notes/description, wake-on-LAN MAC, ACME account and domains, start-on-boot delay, ballooning target and location, plus the digest for safe updates.",
    input: { node: nodeArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      return { node, ...(await getNodeConfig(client, node)) };
    },
  });

  defineTool(ctx, "proxmox_update_node_config", {
    toolset: "nodes",
    title: "Update node options",
    description:
      "Change a node's options: notes/description, wake-on-LAN MAC, ACME account and certificate domains (used by proxmox_node_acme_certificate), start-on-boot delay, ballooning target. Only the given fields change; remove with `delete`.",
    write: true,
    destructive: false,
    input: {
      node: nodeArg,
      description: z.string().optional().describe("Node notes (Markdown), shown in the web UI"),
      wakeonlan: z.string().optional().describe("Wake-on-LAN MAC address, optionally with options, e.g. 'aa:bb:cc:dd:ee:ff,bind-interface=vmbr0'"),
      startall_onboot_delay: z.number().int().min(0).max(300).optional().describe("Seconds to wait before starting on-boot guests"),
      ballooning_target: z.number().int().min(0).max(100).optional().describe("RAM usage target for ballooning, in percent"),
      acme_account: z.string().optional().describe("ACME account name for this node's certificate (see proxmox_list_acme_accounts)"),
      acme_domains: z
        .array(
          z.object({
            domain: z.string().describe("Domain name, e.g. pve1.example.com"),
            plugin: z.string().optional().describe("ACME plugin ID (default standalone HTTP validation)"),
            alias: z.string().optional().describe("DNS alias domain for challenge delegation"),
          }),
        )
        .max(6)
        .optional()
        .describe("Certificate domains (acmedomain0..5); replaces the existing list"),
      delete: deleteKeysArg,
      digest: digestArg,
      extra: extraArg,
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const params: Obj = defined({
        description: a.description,
        wakeonlan: a.wakeonlan,
        "startall-onboot-delay": a.startall_onboot_delay,
        "ballooning-target": a.ballooning_target,
        acme: a.acme_account !== undefined ? `account=${a.acme_account}` : undefined,
        digest: a.digest,
      });
      const del = [...(a.delete ?? [])];
      if (a.acme_domains) {
        a.acme_domains.forEach((d, i) => {
          params[`acmedomain${i}`] = [d.domain, d.plugin && `plugin=${d.plugin}`, d.alias && `alias=${d.alias}`].filter(Boolean).join(",");
        });
        for (let i = a.acme_domains.length; i < 6; i++) del.push(`acmedomain${i}`);
      }
      // Clearing acmedomainN keys that were never set is harmless, but only send them if they exist to keep the request clean.
      let deleteKeys = del;
      if (a.acme_domains && a.acme_domains.length < 6) {
        const cur = await client.get<Obj>(`${nodePath(node)}/config`);
        deleteKeys = del.filter((k) => !/^acmedomain\d+$/.test(k) || k in cur);
      }
      await client.put(`${nodePath(node)}/config`, defined(withExtra({ ...params, delete: deleteParam(deleteKeys) }, a.extra)));
      return { node, updated: true, config: await getNodeConfig(client, node) };
    },
  });

  // ================================================================ power & bulk actions

  defineTool(ctx, "proxmox_node_power", {
    toolset: "nodes",
    title: "Reboot or shut down a node",
    description:
      "Reboot or shut down a Proxmox node. Running guests are shut down or stopped by the node first (HA guests may be migrated per the HA shutdown policy). Consider proxmox_node_bulk_action migrate first to move guests away. A shut-down node can only be brought back by hand, IPMI or proxmox_wake_node.",
    write: true,
    destructive: true,
    input: { node: nodeRequiredArg, command: z.enum(["reboot", "shutdown"]).describe("reboot or shutdown") },
    handler: async (a) => {
      await client.post(`${nodePath(a.node)}/status`, { command: a.command });
      return { node: a.node, command: a.command, status: "requested" };
    },
  });

  defineTool(ctx, "proxmox_wake_node", {
    toolset: "nodes",
    title: "Wake a node with wake-on-LAN",
    description:
      "Send a wake-on-LAN packet to a powered-off node (sent by another cluster node). The node needs its wake-on-LAN MAC set in its options (proxmox_update_node_config wakeonlan).",
    write: true,
    destructive: false,
    idempotent: true,
    input: { node: nodeRequiredArg },
    handler: async (a) => {
      const mac = await client.post(`${nodePath(a.node)}/wakeonlan`);
      return compact({ node: a.node, sent: true, mac: typeof mac === "string" ? mac : undefined });
    },
  });

  defineTool(ctx, "proxmox_node_bulk_action", {
    toolset: "nodes",
    title: "Start, stop, suspend or migrate all guests on a node",
    description:
      "Run a bulk action over the guests on one node: start (by default only guests with onboot=1; force=true for all), stop (shut down, hard-stopping after a timeout), suspend (VMs only) or migrate (all guests to `target`, e.g. before node maintenance). Restrict with `vmids`. Runs as one task. For a selection across nodes use proxmox_bulk_guest_action.",
    write: true,
    destructive: true,
    input: {
      node: nodeArg,
      action: z.enum(["start", "stop", "suspend", "migrate"]).describe("What to do with the node's guests"),
      vmids: z.array(z.number().int().min(100)).optional().describe("Only these guests (default all on the node)"),
      target: z.string().optional().describe("Target node (migrate only, required)"),
      force: z.boolean().optional().describe("start: also start guests without onboot=1"),
      force_stop: z.boolean().optional().describe("stop: hard-stop guests that don't shut down within the timeout (default true)"),
      timeout: z.number().int().positive().optional().describe("stop: per-guest shutdown timeout in seconds (default 180)"),
      with_local_disks: z.boolean().optional().describe("migrate: live-migrate local disks too"),
      max_workers: z.number().int().positive().optional().describe("Parallel jobs (default from datacenter max_workers; migrate needs one of the two)"),
      wait: waitArg,
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      if (a.action === "migrate" && !a.target) throw new Error("target is required for migrate");
      const endpoint = { start: "startall", stop: "stopall", suspend: "suspendall", migrate: "migrateall" }[a.action];
      const params = defined({
        vms: a.vmids?.length ? a.vmids.join(",") : undefined,
        "max-workers": a.max_workers,
        force: a.action === "start" ? a.force : undefined,
        "force-stop": a.action === "stop" ? a.force_stop : undefined,
        timeout: a.action === "stop" ? a.timeout : undefined,
        target: a.action === "migrate" ? a.target : undefined,
        "with-local-disks": a.action === "migrate" ? a.with_local_disks : undefined,
      });
      const upid = await client.post(`${nodePath(node)}/${endpoint}`, params);
      return runTask(client, upid, a.wait, compact({ node, action: a.action, target: a.target, vmids: a.vmids }));
    },
  });

  // ================================================================ logs

  defineTool(ctx, "proxmox_get_node_syslog", {
    toolset: "nodes",
    title: "Read node syslog",
    description:
      "Read a node's system log (journal rendered as syslog lines), optionally for one service and a time range. Page with start/limit. For the raw systemd journal with 'last N lines' semantics use proxmox_get_node_journal.",
    input: {
      node: nodeArg,
      service: z.string().optional().describe("Only this service/unit, e.g. pveproxy, pvedaemon, corosync, pve-cluster"),
      since: z.string().optional().describe("Start time, e.g. '2026-01-31 08:00:00' or 'yesterday'"),
      until: z.string().optional().describe("End time, same format as since"),
      start: z.number().int().min(0).optional().describe("Line offset to start from (default 0)"),
      limit: limitArg(200),
      search: searchArg,
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const lines = await client.get<Obj[]>(
        `${nodePath(node)}/syslog`,
        defined({ service: a.service, since: a.since, until: a.until, start: a.start, limit: a.limit ?? 200 }),
      );
      const text = lines.map((l) => String(l.t ?? "")).filter((t) => t !== "no content");
      const res = filterAndLimit(text, { search: a.search }, (t) => [t]);
      return { node, total: (lines as any).total ?? lines.length, returned: res.returned, lines: res.items };
    },
  });

  defineTool(ctx, "proxmox_get_node_journal", {
    toolset: "nodes",
    title: "Read node systemd journal",
    description:
      "Read a node's systemd journal: the last N entries (default 100) or a time range, optionally only one unit, a syslog identifier, a priority level or kernel messages.",
    input: {
      node: nodeArg,
      lastentries: z.number().int().positive().max(10000).optional().describe("Only the last N entries (default 100 when no range is given)"),
      since: timeArg("Start of the range"),
      until: timeArg("End of the range"),
      unit: z.string().optional().describe("Only this systemd unit, e.g. pveproxy (.service implied)"),
      identifier: z.string().optional().describe("Only messages whose syslog identifier matches this glob, e.g. 'pve*'"),
      priority: z.string().optional().describe("Only this priority or more severe: 0 (emerg) .. 7 (debug), or a range 'LOW..HIGH'; e.g. 3 for errors"),
      kernel: z.boolean().optional().describe("Only kernel messages"),
      search: searchArg,
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const since = toEpoch(a.since);
      const until = toEpoch(a.until);
      const lastentries = a.lastentries ?? (since === undefined && until === undefined ? 100 : undefined);
      const lines = await client.get<unknown[]>(
        `${nodePath(node)}/journal`,
        defined({ lastentries, since, until, unit: a.unit, service: a.identifier, priority: a.priority, kernel: a.kernel }),
      );
      const text = lines.map((l) => (typeof l === "string" ? l : JSON.stringify(l)));
      const res = filterAndLimit(text, { search: a.search }, (t) => [t]);
      return { node, returned: res.returned, lines: res.items };
    },
  });

  // ================================================================ updates & repositories

  defineTool(ctx, "proxmox_list_node_updates", {
    toolset: "nodes",
    title: "List available package updates",
    description:
      "List the package updates available on a node (from the last package index refresh) with installed and new versions, origin and priority, plus counts. Run proxmox_refresh_node_updates first for fresh results. Installing updates is not available through the API.",
    input: { node: nodeArg, search: searchArg, limit: limitArg(300) },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const pkgs = await client.get<Obj[]>(`${nodePath(node)}/apt/update`);
      pkgs.sort((x, y) => String(x.Package).localeCompare(String(y.Package)));
      const byOrigin: Record<string, number> = {};
      for (const p of pkgs) byOrigin[p.Origin ?? "unknown"] = (byOrigin[p.Origin ?? "unknown"] ?? 0) + 1;
      const res = filterAndLimit(pkgs, { search: a.search, limit: a.limit ?? 300 }, (p) => [p.Package, p.Title, p.Origin, p.Section]);
      const kernel = pkgs.some((p) => /^(proxmox-kernel|pve-kernel|proxmox-default-kernel)/.test(p.Package));
      return compact({
        node,
        by_origin: byOrigin,
        kernel_update: kernel || undefined,
        note: kernel ? "Includes a kernel update; a reboot is needed to run it after upgrading." : undefined,
        ...res,
        items: res.items.map((p) => compact({ package: p.Package, installed: p.OldVersion, new: p.Version, origin: p.Origin, priority: p.Priority, title: p.Title })),
      });
    },
  });

  defineTool(ctx, "proxmox_refresh_node_updates", {
    toolset: "nodes",
    title: "Refresh package index",
    description: "Refresh a node's package index (apt-get update) so proxmox_list_node_updates shows current updates. Runs as a task.",
    write: true,
    destructive: false,
    idempotent: true,
    input: { node: nodeArg, notify: z.boolean().optional().describe("Send a notification about new packages"), wait: waitArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const upid = await client.post(`${nodePath(node)}/apt/update`, defined({ notify: a.notify }));
      return runTask(client, upid, a.wait, { node });
    },
  });

  defineTool(ctx, "proxmox_get_package_changelog", {
    toolset: "nodes",
    title: "Get package changelog",
    description: "Get the changelog of a package available on a node, e.g. to see what an update to pve-manager or proxmox-kernel changes. Truncated to max_chars.",
    input: {
      node: nodeArg,
      package: z.string().describe("Package name, e.g. pve-manager"),
      version: z.string().optional().describe("Package version (default the candidate version)"),
      max_chars: maxCharsArg(20000),
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const text = await client.get(`${nodePath(node)}/apt/changelog`, defined({ name: a.package, version: a.version }));
      return { node, package: a.package, ...truncate(text, a.max_chars ?? 20000) };
    },
  });

  defineTool(ctx, "proxmox_get_node_package_versions", {
    toolset: "nodes",
    title: "Get Proxmox package versions",
    description: "List the installed versions of the important Proxmox packages on a node (like `pveversion -v`), including the running kernel.",
    input: { node: nodeArg, search: searchArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const pkgs = await client.get<Obj[]>(`${nodePath(node)}/apt/versions`);
      const res = filterAndLimit(pkgs, { search: a.search }, (p) => [p.Package, p.Title]);
      const pve = pkgs.find((p) => p.Package === "proxmox-ve");
      return compact({
        node,
        running_kernel: pve?.RunningKernel,
        manager_version: pkgs.find((p) => p.ManagerVersion)?.ManagerVersion,
        ...res,
        items: res.items.map((p) =>
          compact({ package: p.Package, version: p.CurrentState === "Installed" || !p.CurrentState ? p.Version : undefined, state: p.CurrentState === "Installed" ? undefined : p.CurrentState }),
        ),
      });
    },
  });

  defineTool(ctx, "proxmox_list_node_repositories", {
    toolset: "nodes",
    title: "List APT repositories",
    description:
      "List a node's APT repositories by file with enabled/disabled state, URIs, suites and components, any parse errors and warnings (e.g. enterprise repo without subscription, mixed suites), and which standard Proxmox repositories are configured. Use path/index and the digest with proxmox_set_node_repository.",
    input: { node: nodeArg, raw: rawArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const r = await client.get<Obj>(`${nodePath(node)}/apt/repositories`);
      return a.raw ? r : { node, ...summarizeRepositories(r) };
    },
  });

  defineTool(ctx, "proxmox_set_node_repository", {
    toolset: "nodes",
    title: "Enable, disable or add an APT repository",
    description:
      "Enable or disable an existing APT repository (give path and index from proxmox_list_node_repositories plus enabled), or add a standard Proxmox repository by handle (e.g. no-subscription, enterprise, test, ceph-squid-no-subscription). Refresh with proxmox_refresh_node_updates afterwards.",
    write: true,
    destructive: false,
    idempotent: true,
    input: {
      node: nodeArg,
      handle: z.string().optional().describe("Add this standard repository, e.g. 'no-subscription' or 'enterprise' (see standard_repos)"),
      path: z.string().optional().describe("File of the repository to change, e.g. /etc/apt/sources.list.d/pve-enterprise.sources"),
      index: z.number().int().min(0).optional().describe("Index of the repository within the file"),
      enabled: z.boolean().optional().describe("Enable (true) or disable (false) the repository at path/index"),
      digest: digestArg,
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const base = `${nodePath(node)}/apt/repositories`;
      if (a.handle) {
        if (a.path !== undefined || a.index !== undefined) throw new Error("Give either handle (add a standard repo) or path+index (change one), not both");
        await client.put(base, defined({ handle: a.handle, digest: a.digest }));
      } else {
        if (a.path === undefined || a.index === undefined || a.enabled === undefined) {
          throw new Error("To change a repository give path, index and enabled; to add a standard repository give handle");
        }
        await client.post(base, defined({ path: a.path, index: a.index, enabled: a.enabled, digest: a.digest }));
      }
      return { node, updated: true, ...summarizeRepositories(await client.get<Obj>(base)) };
    },
  });

  // ================================================================ subscription

  defineTool(ctx, "proxmox_get_node_subscription", {
    toolset: "nodes",
    title: "Get node subscription",
    description: "Get a node's subscription status, level, product, sockets and next due date. The key itself is never shown.",
    input: { node: nodeArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      return { node, ...summarizeSubscription(await client.get<Obj>(`${nodePath(node)}/subscription`)) };
    },
  });

  defineTool(ctx, "proxmox_set_node_subscription", {
    toolset: "nodes",
    title: "Set or refresh node subscription",
    description:
      "Set a node's subscription key (key given), or re-check the existing subscription with the Proxmox shop server (no key). Returns the resulting status; the key is never echoed back.",
    write: true,
    destructive: false,
    idempotent: true,
    input: {
      node: nodeArg,
      key: z.string().optional().describe("Subscription key, e.g. pve2c-0123456789 (omit to just refresh the status)"),
      force: z.boolean().optional().describe("Refresh: contact the server even if the cached status is still valid"),
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const path = `${nodePath(node)}/subscription`;
      if (a.key) await client.put(path, { key: a.key });
      else await client.post(path, defined({ force: a.force }));
      return { node, [a.key ? "key_set" : "refreshed"]: true, ...summarizeSubscription(await client.get<Obj>(path)) };
    },
  });

  defineTool(ctx, "proxmox_delete_node_subscription", {
    toolset: "nodes",
    title: "Remove node subscription key",
    description: "Remove the subscription key from a node (e.g. before moving it to other hardware). The enterprise repository stops working without it.",
    delete: true,
    input: { node: nodeArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      await client.delete(`${nodePath(node)}/subscription`);
      return { node, deleted: true };
    },
  });

  // ================================================================ certificates

  defineTool(ctx, "proxmox_get_node_certificates", {
    toolset: "nodes",
    title: "Get node certificates",
    description:
      "List the TLS certificates of a node's web interface/API (pve-root-ca, pve-ssl and any custom or ACME pveproxy-ssl certificate) with subject, issuer, SANs, validity, days left and fingerprint. Private keys are never returned.",
    input: { node: nodeArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const certs = await client.get<Obj[]>(`${nodePath(node)}/certificates/info`);
      return { node, certificates: certs.map(summarizeCert) };
    },
  });

  defineTool(ctx, "proxmox_upload_node_certificate", {
    toolset: "nodes",
    title: "Upload a custom node certificate",
    description:
      "Install a custom TLS certificate (PEM chain) and private key for a node's web interface/API (pveproxy-ssl). Replaces an existing custom/ACME certificate only with force=true; restart=true restarts pveproxy to use it. The key is sent to Proxmox only and never returned.",
    write: true,
    destructive: true,
    input: {
      node: nodeArg,
      certificates: z.string().describe("PEM certificate chain (server certificate first)"),
      key: z.string().optional().describe("PEM private key (required unless a key is already installed)"),
      force: z.boolean().optional().describe("Overwrite an existing custom or ACME certificate"),
      restart: z.boolean().optional().describe("Restart pveproxy to load the new certificate"),
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const res = await client.post<Obj>(
        `${nodePath(node)}/certificates/custom`,
        defined({ certificates: a.certificates, key: a.key, force: a.force, restart: a.restart }),
      );
      return compact({ node, uploaded: true, certificate: res && typeof res === "object" ? summarizeCert(res) : undefined });
    },
  });

  defineTool(ctx, "proxmox_delete_node_certificate", {
    toolset: "nodes",
    title: "Delete the custom node certificate",
    description: "Remove a node's custom (or ACME) web certificate and key, falling back to the self-signed pve-ssl certificate. restart=true restarts pveproxy.",
    delete: true,
    input: { node: nodeArg, restart: z.boolean().optional().describe("Restart pveproxy afterwards") },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      await client.delete(`${nodePath(node)}/certificates/custom`, defined({ restart: a.restart }));
      return { node, deleted: true };
    },
  });

  defineTool(ctx, "proxmox_node_acme_certificate", {
    toolset: "nodes",
    title: "Order, renew or revoke an ACME certificate",
    description:
      "Order a new ACME (e.g. Let's Encrypt) certificate for a node, renew it, or revoke it at the CA. The node's ACME account and domains must be set first (proxmox_update_node_config acme_account/acme_domains; accounts via proxmox_save_acme_account, DNS plugins via proxmox_save_acme_plugin). Runs as a task; pveproxy reloads the new certificate. revoke needs delete access (PROXMOX_ALLOW_DELETES).",
    write: true,
    destructive: true,
    input: {
      node: nodeArg,
      action: z.enum(["order", "renew", "revoke"]).describe("order a new certificate, renew the existing one, or revoke it"),
      force: z.boolean().optional().describe("order: overwrite an existing custom certificate; renew: renew even if more than 30 days remain"),
      wait: waitArg,
    },
    handler: async (a) => {
      // Revoking destroys the certificate at the CA, so it needs delete access like the other delete tools.
      if (a.action === "revoke" && !ctx.allowDeletes) throw new Error("Revoking needs delete access (PROXMOX_ALLOW_DELETES=true)");
      const node = await client.resolveNode(a.node);
      const path = `${nodePath(node)}/certificates/acme/certificate`;
      const params = defined({ force: a.action === "revoke" ? undefined : a.force });
      const upid =
        a.action === "order" ? await client.post(path, params) : a.action === "renew" ? await client.put(path, params) : await client.delete(path);
      return runTask(client, upid, a.wait, { node, action: a.action });
    },
  });

  // ================================================================ cluster ACME accounts & plugins

  defineTool(ctx, "proxmox_list_acme_accounts", {
    toolset: "nodes",
    title: "List ACME accounts",
    description: "List the cluster's registered ACME accounts (used to order node certificates), with their CA directory and contacts.",
    input: {},
    handler: async () => {
      const accounts = await client.get<Obj[]>("/cluster/acme/account");
      return Promise.all(
        accounts.map(async (acc) => {
          const d = await client.get<Obj>(`/cluster/acme/account/${enc(acc.name)}`).catch(() => undefined);
          return compact({ name: acc.name, directory: d?.directory, status: d?.account?.status, contact: d?.account?.contact });
        }),
      );
    },
  });

  defineTool(ctx, "proxmox_get_acme_account", {
    toolset: "nodes",
    title: "Get ACME account",
    description: "Get an ACME account's details: CA directory, account URL (location), status, contacts and accepted terms of service. Keys are redacted.",
    input: { name: z.string().optional().describe("Account name (default 'default')") },
    handler: async (a) => {
      const name = a.name ?? "default";
      return { name, ...redactSecrets(await client.get<Obj>(`/cluster/acme/account/${enc(name)}`)) };
    },
  });

  defineTool(ctx, "proxmox_save_acme_account", {
    toolset: "nodes",
    title: "Register or update an ACME account",
    description:
      "Register a new ACME account with a CA (create=true; Let's Encrypt by default, accepting its terms of service with tos_url from proxmox_list_acme_directories), or update an existing account's contact email (or refresh it when no contact is given). Runs as a task.",
    write: true,
    destructive: false,
    input: {
      name: z.string().optional().describe("Account name (default 'default')"),
      create: z.boolean().optional().describe("Register a new account (default false = update)"),
      contact: z.string().optional().describe("Contact email address(es), comma-separated (required when creating)"),
      directory: z.string().optional().describe("ACME directory URL (create only; default Let's Encrypt production)"),
      tos_url: z.string().optional().describe("Terms of service URL being agreed to (create only; required by most CAs)"),
      eab_kid: z.string().optional().describe("External account binding key ID (create only, for CAs that require EAB)"),
      eab_hmac_key: z.string().optional().describe("External account binding HMAC key (create only)"),
      wait: waitArg,
    },
    handler: async (a) => {
      const name = a.name ?? "default";
      let upid: unknown;
      if (a.create) {
        if (!a.contact) throw new Error("contact is required when registering an ACME account");
        upid = await client.post(
          "/cluster/acme/account",
          defined({ name, contact: a.contact, directory: a.directory, tos_url: a.tos_url, "eab-kid": a.eab_kid, "eab-hmac-key": a.eab_hmac_key }),
        );
      } else {
        upid = await client.put(`/cluster/acme/account/${enc(name)}`, defined({ contact: a.contact }));
      }
      return runTask(client, upid, a.wait, { name, [a.create ? "registered" : "updated"]: true });
    },
  });

  defineTool(ctx, "proxmox_delete_acme_account", {
    toolset: "nodes",
    title: "Deactivate an ACME account",
    description: "Deactivate an ACME account at the CA and remove it from the cluster. Nodes using it can no longer order or renew certificates. Runs as a task.",
    delete: true,
    input: { name: z.string().describe("Account name"), wait: waitArg },
    handler: async (a) => {
      const upid = await client.delete(`/cluster/acme/account/${enc(a.name)}`);
      return runTask(client, upid, a.wait, { name: a.name, deleted: true });
    },
  });

  defineTool(ctx, "proxmox_list_acme_directories", {
    toolset: "nodes",
    title: "List ACME directories",
    description:
      "List the known ACME CA directories (Let's Encrypt production/staging...). With `directory`, also fetch that CA's metadata: terms-of-service URL (needed to register an account) and whether external account binding is required.",
    input: { directory: z.string().optional().describe("Also fetch metadata for this directory URL") },
    handler: async (a) => {
      const directories = await client.get<Obj[]>("/cluster/acme/directories");
      if (!a.directory) return { directories };
      const meta = await client.get<Obj>("/cluster/acme/meta", { directory: a.directory }).catch(async () => {
        // Older versions only have /tos.
        const tos = await client.get("/cluster/acme/tos", { directory: a.directory });
        return { termsOfService: tos };
      });
      return { directories, meta: compact({ directory: a.directory, ...meta }) };
    },
  });

  defineTool(ctx, "proxmox_list_acme_plugins", {
    toolset: "nodes",
    title: "List ACME challenge plugins",
    description: "List the cluster's ACME challenge plugins (standalone HTTP and DNS API plugins). Credentials in plugin data are redacted; only their keys are shown.",
    input: { type: z.enum(["dns", "standalone"]).optional().describe("Only plugins of this type") },
    handler: async (a) => {
      const plugins = await client.get<Obj[]>("/cluster/acme/plugins", defined({ type: a.type }));
      return plugins.map(summarizePlugin);
    },
  });

  defineTool(ctx, "proxmox_get_acme_plugin", {
    toolset: "nodes",
    title: "Get ACME plugin",
    description: "Get one ACME challenge plugin's configuration (DNS API, nodes, validation delay). Credential values are redacted.",
    input: { id: z.string().describe("Plugin ID") },
    handler: async (a) => summarizePlugin(await client.get<Obj>(`/cluster/acme/plugins/${enc(a.id)}`)),
  });

  defineTool(ctx, "proxmox_get_acme_challenge_schema", {
    toolset: "nodes",
    title: "Get ACME DNS plugin schema",
    description: "Show which credential fields each ACME DNS API plugin needs (e.g. CF_Token for Cloudflare), to fill `data` in proxmox_save_acme_plugin. Filter by api name.",
    input: { api: z.string().optional().describe("Only this DNS API, e.g. cf, ovh, hetzner, route53 is 'aws'") },
    handler: async (a) => {
      let schema = await client.get<Obj[]>("/cluster/acme/challenge-schema");
      if (a.api) schema = schema.filter((s) => s.id === a.api);
      return schema.map((s) => compact({ api: s.id, name: s.name, type: s.type, fields: s.schema?.fields ?? s.schema }));
    },
  });

  defineTool(ctx, "proxmox_save_acme_plugin", {
    toolset: "nodes",
    title: "Create or update an ACME plugin",
    description:
      "Create (create=true) or update an ACME challenge plugin. For DNS validation give api (e.g. cf) and data with the API credentials as key/value pairs (see proxmox_get_acme_challenge_schema); they are base64-encoded for Proxmox and never echoed back. On update, data replaces all existing credentials.",
    write: true,
    destructive: false,
    input: {
      id: z.string().describe("Plugin ID, e.g. cloudflare"),
      create: z.boolean().optional().describe("Create a new plugin (default false = update)"),
      type: z.enum(["dns", "standalone"]).optional().describe("Challenge type (create only; default dns)"),
      api: z.string().optional().describe("DNS API plugin name, e.g. cf, ovh, hetzner, aws, gcloud, desec"),
      data: z.record(z.string(), z.string()).optional().describe("DNS API credentials, e.g. {\"CF_Token\": \"...\", \"CF_Account_ID\": \"...\"}"),
      nodes: z.array(z.string()).optional().describe("Restrict the plugin to these nodes (default all)"),
      validation_delay: z.number().int().min(0).max(172800).optional().describe("Seconds to wait before validation, for slow DNS propagation (default 30)"),
      disable: z.boolean().optional().describe("Disable the plugin"),
      delete: deleteKeysArg,
      digest: digestArg,
    },
    handler: async (a) => {
      const params = defined({
        api: a.api,
        data: a.data ? encodePluginData(a.data) : undefined,
        nodes: a.nodes?.join(","),
        "validation-delay": a.validation_delay,
        disable: a.disable,
      });
      if (a.create) {
        await client.post("/cluster/acme/plugins", { id: a.id, type: a.type ?? "dns", ...params });
      } else {
        await client.put(`/cluster/acme/plugins/${enc(a.id)}`, defined({ ...params, delete: deleteParam(a.delete), digest: a.digest }));
      }
      return { saved: a.id, plugin: summarizePlugin(await client.get<Obj>(`/cluster/acme/plugins/${enc(a.id)}`)) };
    },
  });

  defineTool(ctx, "proxmox_delete_acme_plugin", {
    toolset: "nodes",
    title: "Delete an ACME plugin",
    description: "Delete an ACME challenge plugin. Node certificate domains that use it can no longer be validated.",
    delete: true,
    input: { id: z.string().describe("Plugin ID") },
    handler: async (a) => {
      await client.delete(`/cluster/acme/plugins/${enc(a.id)}`);
      return { deleted: a.id };
    },
  });
}
