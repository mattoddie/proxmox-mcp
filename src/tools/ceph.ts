import { z } from "zod";
import type { ProxmoxClient } from "../proxmox/client.js";
import { compact, formatBytes, redactSecrets, usage } from "../proxmox/format.js";
import {
  defined,
  defineTool,
  enc,
  extraArg,
  filterAndLimit,
  limitArg,
  rawArg,
  runTask,
  searchArg,
  waitArg,
  withExtra,
  type ToolContext,
} from "./util.js";

type Obj = Record<string, any>;

const FLAGS = ["nobackfill", "nodeep-scrub", "nodown", "noin", "noout", "norebalance", "norecover", "noscrub", "notieragent", "noup", "pause"] as const;
const FLAG_HELP: Record<(typeof FLAGS)[number], string> = {
  nobackfill: "suspend PG backfilling",
  "nodeep-scrub": "disable deep scrubbing",
  nodown: "ignore OSD failure reports (OSDs won't be marked down)",
  noin: "don't mark restarted OSDs back in",
  noout: "don't mark down OSDs out (use during maintenance)",
  norebalance: "suspend PG rebalancing",
  norecover: "suspend PG recovery",
  noscrub: "disable scrubbing",
  notieragent: "suspend cache tiering",
  noup: "don't allow OSDs to start",
  pause: "pause ALL client reads and writes",
};
/** One optional boolean argument per flag (dashes become underscores). */
const FLAG_ARGS = Object.fromEntries(
  FLAGS.map((f) => [f.replace("-", "_"), z.boolean().optional().describe(`true sets, false clears: ${FLAG_HELP[f]}`)]),
) as Record<"nobackfill" | "nodeep_scrub" | "nodown" | "noin" | "noout" | "norebalance" | "norecover" | "noscrub" | "notieragent" | "noup" | "pause", z.ZodOptional<z.ZodBoolean>>;
const SERVICE_TYPES = ["mon", "mgr", "mds"] as const;

const cephNodeArg = z
  .string()
  .optional()
  .describe("Node to query through (any node with Ceph installed; default PROXMOX_DEFAULT_NODE or the first online node). Results are cluster-wide.");
const targetNodeArg = z.string().describe("Node to act on (where the daemon/disk is)");
const osdIdArg = z.number().int().min(0).describe("OSD ID number, e.g. 3 for osd.3");

/**
 * Cluster-wide Ceph reads work through any node; unlike resolveNode we don't
 * insist on a single-node cluster, we just pick the first online node.
 */
async function cephNode(client: ProxmoxClient, node?: string): Promise<string> {
  if (node) return node;
  if (client.defaultNode) return client.defaultNode;
  const nodes = await client.listNodes();
  const online = nodes.filter((n) => n.status === "online").map((n) => n.node).sort();
  if (!online.length) throw new Error("No online node found to query Ceph through; pass `node`");
  return online[0];
}

const cephBase = (node: string) => `/nodes/${enc(node)}/ceph`;

// ---------------------------------------------------------------- status

function summarizeStatus(s: Obj): Obj {
  const health = s.health ?? {};
  const checks = Object.entries((health.checks ?? {}) as Obj).map(([code, c]: [string, Obj]) =>
    compact({
      code,
      severity: c.severity,
      summary: c.summary?.message ?? c.summary,
      count: c.summary?.count,
      muted: c.muted ? true : undefined,
      detail: Array.isArray(c.detail) ? c.detail.slice(0, 5).map((d: Obj) => d.message ?? d) : undefined,
    }),
  );
  // The osdmap was nested one level deeper (osdmap.osdmap) before Ceph Quincy.
  const osd = s.osdmap?.osdmap ?? s.osdmap ?? {};
  const pg = s.pgmap ?? {};
  const mons: Obj[] = s.monmap?.mons ?? [];
  const mgr = s.mgrmap ?? {};
  const standbys = Array.isArray(mgr.standbys) ? mgr.standbys.map((x: Obj) => x.name) : undefined;
  const fs = s.fsmap ?? {};
  return compact({
    health: health.status,
    checks,
    fsid: s.fsid,
    monitors: compact({
      total: mons.length || s.monmap?.num_mons,
      in_quorum: s.quorum_names,
      out_of_quorum: mons.map((m) => m.name).filter((n: string) => s.quorum_names && !s.quorum_names.includes(n)),
    }),
    managers: compact({
      active: mgr.active_name,
      available: mgr.available === undefined ? undefined : Boolean(mgr.available),
      standbys: standbys ?? mgr.num_standbys,
    }),
    osds: compact({
      total: osd.num_osds,
      up: osd.num_up_osds,
      in: osd.num_in_osds,
      down: osd.num_osds !== undefined && osd.num_up_osds !== undefined ? osd.num_osds - osd.num_up_osds : undefined,
      remapped_pgs: osd.num_remapped_pgs || undefined,
    }),
    pgs: compact({
      total: pg.num_pgs,
      states: Array.isArray(pg.pgs_by_state) ? Object.fromEntries(pg.pgs_by_state.map((p: Obj) => [p.state_name, p.count])) : undefined,
      degraded_pct: pg.degraded_ratio !== undefined ? Math.round(pg.degraded_ratio * 1000) / 10 : undefined,
      misplaced_pct: pg.misplaced_ratio !== undefined ? Math.round(pg.misplaced_ratio * 1000) / 10 : undefined,
      recovering_per_sec: pg.recovering_bytes_per_sec ? `${formatBytes(pg.recovering_bytes_per_sec)}/s` : undefined,
    }),
    usage: compact({
      raw: usage(pg.bytes_used, pg.bytes_total),
      available: formatBytes(pg.bytes_avail),
      data_stored: formatBytes(pg.data_bytes),
      pools: pg.num_pools,
      objects: pg.num_objects,
    }),
    io: compact({
      read: pg.read_bytes_sec ? `${formatBytes(pg.read_bytes_sec)}/s` : undefined,
      write: pg.write_bytes_sec ? `${formatBytes(pg.write_bytes_sec)}/s` : undefined,
      read_ops: pg.read_op_per_sec,
      write_ops: pg.write_op_per_sec,
    }),
    cephfs: fs.by_rank?.length ? compact({ epoch: fs.epoch, mds: fs.by_rank.map((r: Obj) => `${r.name}: ${r.status}`), standby: fs["up:standby"] }) : undefined,
  });
}

// ---------------------------------------------------------------- OSD tree

interface OsdLeaf extends Obj {
  host?: string;
}

/** Flattens the OSD tree into OSD leaves (with their host) and host buckets. */
function walkTree(root: Obj): { osds: OsdLeaf[]; hosts: Obj[] } {
  const osds: OsdLeaf[] = [];
  const hosts: Obj[] = [];
  const visit = (n: Obj, host?: string) => {
    if (!n || typeof n !== "object") return;
    if (n.type === "osd") {
      osds.push({ ...n, host: n.host ?? host });
      return;
    }
    const h = n.type === "host" ? n.name : host;
    if (n.type === "host") hosts.push(n);
    for (const c of n.children ?? []) visit(c, h);
  };
  visit(root);
  return { osds, hosts };
}

function summarizeOsd(o: OsdLeaf): Obj {
  return compact({
    id: o.id,
    name: o.name,
    host: o.host,
    status: o.status,
    in: o.in === undefined ? undefined : Boolean(Number(o.in)),
    device_class: o.device_class,
    usage: usage(o.bytes_used, o.total_space),
    used_pct: o.percent_used !== undefined ? Math.round(Number(o.percent_used) * 10) / 10 : undefined,
    pgs: o.pgs,
    crush_weight: o.crush_weight,
    reweight: o.reweight,
    latency_ms: o.apply_latency_ms !== undefined || o.commit_latency_ms !== undefined ? { apply: o.apply_latency_ms, commit: o.commit_latency_ms } : undefined,
    type: o.osdtype,
    version: o.version,
    db_dev: o.dbdev ? true : undefined,
    wal_dev: o.waldev ? true : undefined,
  });
}

async function osdTree(client: ProxmoxClient, node: string): Promise<{ flags?: string; osds: OsdLeaf[]; hosts: Obj[] }> {
  const tree = await client.get<Obj>(`${cephBase(node)}/osd`);
  return { flags: tree?.flags, ...walkTree(tree?.root ?? {}) };
}

/** OSD actions must go to the node hosting the OSD; find it from the tree when not given. */
async function osdHost(client: ProxmoxClient, osdid: number, node?: string): Promise<string> {
  if (node) return node;
  const { osds } = await osdTree(client, await cephNode(client));
  const osd = osds.find((o) => Number(o.id) === osdid);
  if (!osd) throw new Error(`OSD ${osdid} not found in the Ceph OSD tree (see proxmox_list_ceph_osds)`);
  if (!osd.host) throw new Error(`Could not determine which node hosts osd.${osdid}; pass \`node\``);
  return osd.host;
}

// ---------------------------------------------------------------- pools

function summarizePool(p: Obj): Obj {
  return compact({
    name: p.pool_name,
    id: p.pool,
    type: p.type,
    size: p.size,
    min_size: p.min_size,
    pg_num: p.pg_num,
    pg_num_optimal: p.pg_num_final,
    pg_num_min: p.pg_num_min,
    autoscale: p.pg_autoscale_mode,
    application: p.application_metadata ? Object.keys(p.application_metadata).join(",") : undefined,
    crush_rule: p.crush_rule_name ?? p.crush_rule,
    used: formatBytes(p.bytes_used),
    used_pct: p.percent_used !== undefined ? Math.round(Number(p.percent_used) * 1000) / 10 : undefined,
    target_size: p.target_size ? formatBytes(p.target_size) : undefined,
    target_size_ratio: p.target_size_ratio,
  });
}

function summarizeDaemon(d: Obj): Obj {
  return compact({
    name: d.name,
    host: d.host,
    state: d.state,
    quorum: d.quorum === undefined ? undefined : Boolean(d.quorum),
    rank: d.rank,
    fs: d.fs_name,
    standby_replay: d.standby_replay ? true : undefined,
    addr: d.addr,
    version: d.ceph_version_short,
    service_enabled: d.service === undefined ? undefined : Boolean(d.service),
  });
}

export function registerCephTools(ctx: ToolContext): void {
  const { client } = ctx;

  // ---------------------------------------------------------------- read

  defineTool(ctx, "proxmox_get_ceph_status", {
    toolset: "ceph",
    title: "Get Ceph status",
    description:
      "Summarized Ceph cluster health: HEALTH_OK/WARN/ERR with each health check, monitor quorum, active/standby managers, OSD counts (up/in/down), PG states, raw usage and client I/O. Start here for any Ceph question; drill down with proxmox_list_ceph_osds, proxmox_list_ceph_pools or proxmox_list_ceph_services.",
    input: { node: cephNodeArg, raw: rawArg },
    handler: async (a) => {
      // /cluster/ceph/status needs no node; fall back to the node alias if it fails (e.g. older versions).
      const status = await client
        .get<Obj>("/cluster/ceph/status")
        .catch(async () => client.get<Obj>(`${cephBase(await cephNode(client, a.node))}/status`));
      const muted = await client.get<Obj[]>("/cluster/ceph/health-mute").catch(() => undefined);
      if (a.raw) return redactSecrets(status);
      return compact({ ...summarizeStatus(status), muted_checks: muted?.map((m) => compact({ code: m.code, ttl: m.ttl, sticky: m.sticky || undefined })) });
    },
  });

  defineTool(ctx, "proxmox_list_ceph_osds", {
    toolset: "ceph",
    title: "List Ceph OSDs",
    description:
      "List Ceph OSDs from the OSD tree: host, up/down, in/out, device class, usage, PGs, CRUSH weight, latency and version, plus the per-host grouping and cluster-wide OSD flags (noout etc.). Filter by host, status or device class.",
    input: {
      node: cephNodeArg,
      host: z.string().optional().describe("Only OSDs on this host"),
      status: z.enum(["up", "down", "in", "out"]).optional().describe("Only OSDs in this state"),
      device_class: z.string().optional().describe("Only this device class, e.g. hdd, ssd, nvme"),
      search: searchArg,
      limit: limitArg(1000),
      raw: rawArg,
    },
    handler: async (a) => {
      const { flags, osds, hosts } = await osdTree(client, await cephNode(client, a.node));
      let list = osds;
      if (a.host) list = list.filter((o) => o.host === a.host);
      if (a.status === "up" || a.status === "down") list = list.filter((o) => o.status === a.status);
      if (a.status === "in") list = list.filter((o) => Number(o.in) === 1);
      if (a.status === "out") list = list.filter((o) => Number(o.in) !== 1);
      if (a.device_class) list = list.filter((o) => o.device_class === a.device_class);
      list.sort((x, y) => Number(x.id) - Number(y.id));
      const res = filterAndLimit(list, { search: a.search, limit: a.limit ?? 1000 }, (o) => [o.name, o.host, o.device_class, o.status]);
      return compact({
        flags,
        summary: {
          total: osds.length,
          up: osds.filter((o) => o.status === "up").length,
          in: osds.filter((o) => Number(o.in) === 1).length,
          hosts: hosts.map((h) => h.name).sort(),
        },
        ...res,
        items: a.raw ? res.items : res.items.map(summarizeOsd),
      });
    },
  });

  defineTool(ctx, "proxmox_get_ceph_osd", {
    toolset: "ceph",
    title: "Get Ceph OSD details",
    description: "Get one OSD's details: daemon metadata (devices, BlueStore, memory, version) and its logical volume info (block/DB/WAL devices).",
    input: { osdid: osdIdArg, node: z.string().optional().describe("Node hosting the OSD (looked up from the OSD tree when omitted)") },
    handler: async (a) => {
      const node = await osdHost(client, a.osdid, a.node);
      const base = `${cephBase(node)}/osd/${a.osdid}`;
      const [metadata, lv] = await Promise.all([
        client.get<Obj>(`${base}/metadata`),
        client.get<Obj>(`${base}/lv-info`).catch((e: Error) => ({ error: e.message })),
      ]);
      return compact({ osd: a.osdid, node, metadata: redactSecrets(metadata), lv_info: lv });
    },
  });

  defineTool(ctx, "proxmox_list_ceph_pools", {
    toolset: "ceph",
    title: "List Ceph pools",
    description:
      "List Ceph pools with replication (size/min_size), PG count and autoscale mode (with the autoscaler's optimal PG count), application, CRUSH rule and usage %. Related: proxmox_get_ceph_pool, proxmox_update_ceph_pool.",
    input: { node: cephNodeArg, search: searchArg, raw: rawArg },
    handler: async (a) => {
      const pools = await client.get<Obj[]>(`${cephBase(await cephNode(client, a.node))}/pool`);
      pools.sort((x, y) => String(x.pool_name).localeCompare(String(y.pool_name)));
      const res = filterAndLimit(pools, { search: a.search }, (p) => [p.pool_name, p.crush_rule_name, p.type]);
      return { ...res, items: a.raw ? res.items : res.items.map(summarizePool) };
    },
  });

  defineTool(ctx, "proxmox_get_ceph_pool", {
    toolset: "ceph",
    title: "Get Ceph pool",
    description: "Get one Ceph pool's settings (size, min_size, PGs, autoscaler, CRUSH rule, application, protection flags) and, with statistics=true, its usage and I/O statistics.",
    input: {
      name: z.string().describe("Pool name"),
      statistics: z.boolean().optional().describe("Include usage and I/O statistics (default true)"),
      node: cephNodeArg,
    },
    handler: async (a) => {
      const node = await cephNode(client, a.node);
      return client.get<Obj>(`${cephBase(node)}/pool/${enc(a.name)}/status`, { verbose: a.statistics ?? true });
    },
  });

  defineTool(ctx, "proxmox_list_ceph_services", {
    toolset: "ceph",
    title: "List Ceph monitors, managers and metadata servers",
    description:
      "List Ceph daemons of one type or all: monitors (quorum, rank), managers (active/standby) and metadata servers (MDS state, CephFS, rank), with host, address and version.",
    input: {
      type: z.enum([...SERVICE_TYPES, "all"]).optional().describe("Daemon type (default all)"),
      node: cephNodeArg,
    },
    handler: async (a) => {
      const node = await cephNode(client, a.node);
      const types = !a.type || a.type === "all" ? SERVICE_TYPES : [a.type];
      const out: Obj = {};
      await Promise.all(
        types.map(async (t) => {
          try {
            const list = await client.get<Obj[]>(`${cephBase(node)}/${t}`);
            out[t] = list.sort((x, y) => String(x.name).localeCompare(String(y.name))).map(summarizeDaemon);
          } catch (err) {
            out[t] = { error: (err as Error).message };
          }
        }),
      );
      return out;
    },
  });

  defineTool(ctx, "proxmox_list_ceph_fs", {
    toolset: "ceph",
    title: "List CephFS file systems",
    description: "List CephFS file systems with their metadata and data pools. MDS daemons serving them are listed by proxmox_list_ceph_services.",
    input: { node: cephNodeArg },
    handler: async (a) => {
      const fs = await client.get<Obj[]>(`${cephBase(await cephNode(client, a.node))}/fs`);
      return fs.map((f) => compact({ name: f.name, metadata_pool: f.metadata_pool, data_pools: f.data_pools ?? [f.data_pool] }));
    },
  });

  defineTool(ctx, "proxmox_get_ceph_flags", {
    toolset: "ceph",
    title: "Get Ceph OSD flags",
    description: "Show which cluster-wide Ceph OSD flags are set (noout, noscrub, nodeep-scrub, norebalance, pause...). Change them with proxmox_set_ceph_flags.",
    input: {},
    handler: async () => {
      const flags = await client.get<Obj[]>("/cluster/ceph/flags");
      return {
        set: flags.filter((f) => f.value).map((f) => f.name),
        flags: flags.map((f) => ({ name: f.name, set: Boolean(f.value), description: f.description })),
      };
    },
  });

  defineTool(ctx, "proxmox_get_ceph_crush", {
    toolset: "ceph",
    title: "Get Ceph CRUSH map and rules",
    description: "Get the decompiled CRUSH map (devices, buckets, rules) and the list of CRUSH rule names usable as a pool's crush_rule.",
    input: { node: cephNodeArg, rules_only: z.boolean().optional().describe("Only list the rule names (much shorter)") },
    handler: async (a) => {
      const node = await cephNode(client, a.node);
      const rules = await client.get<Obj[]>(`${cephBase(node)}/rules`);
      if (a.rules_only) return { rules: rules.map((r) => r.name) };
      const crush = await client.get<string>(`${cephBase(node)}/crush`);
      return { rules: rules.map((r) => r.name), crush_map: crush };
    },
  });

  defineTool(ctx, "proxmox_get_ceph_config", {
    toolset: "ceph",
    title: "Get Ceph configuration",
    description:
      "Read Ceph configuration: the configuration database (`ceph config dump`: section, name, value) and/or the ceph.conf file. Secrets such as keys are redacted.",
    input: {
      source: z.enum(["db", "file", "both"]).optional().describe("Configuration database, ceph.conf file, or both (default both)"),
      search: searchArg,
      node: cephNodeArg,
    },
    handler: async (a) => {
      const node = await cephNode(client, a.node);
      const src = a.source ?? "both";
      const out: Obj = {};
      if (src !== "file") {
        const db = await client.get<Obj[]>(`${cephBase(node)}/cfg/db`);
        const res = filterAndLimit(db, { search: a.search }, (e) => [e.section, e.name, e.value, e.mask]);
        // The values sit under a generic "value" key, so redact by option name (e.g. mgr/dashboard passwords, rgw keystone secrets).
        const secretName = /pass(word|wd)?\b|secret|token|(^|[/_-])key$/i;
        out.db = res.items.map((e) =>
          compact({
            section: e.section,
            name: e.name,
            value: secretName.test(String(e.name ?? "")) && e.value ? "<redacted>" : e.value,
            mask: e.mask,
            level: e.level,
            runtime: e.can_update_at_runtime,
          }),
        );
      }
      if (src !== "db") {
        const raw = await client.get<string>(`${cephBase(node)}/cfg/raw`);
        // ceph.conf rarely holds secrets, but keyring-style "key = ..." lines can appear.
        out.file = String(raw ?? "").replace(/^(\s*key\s*=\s*).+$/gim, "$1<redacted>");
      }
      return out;
    },
  });

  defineTool(ctx, "proxmox_get_ceph_log", {
    toolset: "ceph",
    title: "Read Ceph log",
    description: "Read lines of the Ceph cluster log (ceph.log) from a node: health changes, OSD up/down events, slow ops and scrub errors.",
    input: {
      node: cephNodeArg,
      limit: z.number().int().positive().max(5000).optional().describe("Number of lines (default 50)"),
      start: z.number().int().min(0).optional().describe("Offset of the first line (0-based)"),
      search: searchArg,
    },
    handler: async (a) => {
      const node = await cephNode(client, a.node);
      const lines = await client.get<Obj[]>(`${cephBase(node)}/log`, defined({ limit: a.limit ?? 50, start: a.start }));
      const res = filterAndLimit(lines, { search: a.search }, (l) => [l.t]);
      return { node, ...res, items: res.items.map((l) => l.t) };
    },
  });

  defineTool(ctx, "proxmox_get_ceph_metadata", {
    toolset: "ceph",
    title: "Get Ceph daemon metadata",
    description:
      "Get metadata of every Ceph daemon (mon, mgr, mds, osd) and node: host, Ceph version and release, memory. Useful to spot daemons running an outdated version after an upgrade.",
    input: { raw: rawArg },
    handler: async (a) => {
      const meta = await client.get<Obj>("/cluster/ceph/metadata");
      if (a.raw) return redactSecrets(meta);
      const out: Obj = {};
      for (const [type, entries] of Object.entries(meta ?? {})) {
        if (!entries || typeof entries !== "object") continue;
        out[type] = Object.entries(entries as Obj).map(([id, e]: [string, Obj]) =>
          compact({ id, host: e.hostname, version: e.ceph_version_short ?? e.version?.str ?? e.version, release: e.ceph_release, memory: e.mem_total_kb ? formatBytes(e.mem_total_kb * 1024) : undefined }),
        );
      }
      return out;
    },
  });

  defineTool(ctx, "proxmox_check_ceph_safety", {
    toolset: "ceph",
    title: "Check if stopping/destroying a Ceph daemon is safe",
    description:
      "Ask Ceph whether stopping or destroying an OSD, monitor or MDS is safe right now (e.g. no PGs would become unavailable). Call before proxmox_delete_ceph_osd, proxmox_delete_ceph_service or stopping services.",
    input: {
      service: z.enum(["osd", "mon", "mds"]).describe("Daemon type"),
      id: z.string().describe("Daemon ID, e.g. '3' for osd.3 or the monitor name"),
      action: z.enum(["stop", "destroy"]).describe("Intended action"),
      node: cephNodeArg,
    },
    handler: async (a) => {
      const node = await cephNode(client, a.node);
      const r = await client.get<Obj>(`${cephBase(node)}/cmd-safety`, { service: a.service, id: a.id, action: a.action });
      return compact({ service: a.service, id: a.id, action: a.action, safe: Boolean(r?.safe), status: r?.status });
    },
  });

  // ---------------------------------------------------------------- OSD write

  defineTool(ctx, "proxmox_control_ceph_osd", {
    toolset: "ceph",
    title: "Mark OSD in/out or scrub it",
    description:
      "Mark an OSD in or out of data placement, or start a (deep) scrub. Marking out starts moving its data elsewhere (rebalancing); mark in to bring it back. Set the noout flag instead for short maintenance.",
    write: true,
    destructive: true,
    input: {
      osdid: osdIdArg,
      action: z.enum(["in", "out", "scrub", "deep-scrub"]).describe("What to do"),
      node: z.string().optional().describe("Node hosting the OSD (looked up when omitted)"),
    },
    handler: async (a) => {
      const node = await osdHost(client, a.osdid, a.node);
      const base = `${cephBase(node)}/osd/${a.osdid}`;
      if (a.action === "scrub" || a.action === "deep-scrub") await client.post(`${base}/scrub`, { deep: a.action === "deep-scrub" });
      else await client.post(`${base}/${a.action}`);
      return { osd: a.osdid, node, action: a.action, done: true };
    },
  });

  defineTool(ctx, "proxmox_create_ceph_osd", {
    toolset: "ceph",
    title: "Create a Ceph OSD",
    description:
      "Create a Ceph OSD on an unused disk of a node. DESTROYS ALL DATA on the disk (and on db_dev/wal_dev partitions it uses). List candidate disks with the node disk tools first.",
    write: true,
    destructive: true,
    input: {
      node: targetNodeArg,
      dev: z.string().describe("Block device, e.g. /dev/sdb"),
      device_class: z.string().optional().describe("CRUSH device class, e.g. hdd, ssd, nvme (default auto-detected)"),
      db_dev: z.string().optional().describe("Separate device for the BlueStore DB"),
      db_dev_size: z.number().positive().optional().describe("DB size in GiB"),
      wal_dev: z.string().optional().describe("Separate device for the BlueStore WAL"),
      wal_dev_size: z.number().positive().optional().describe("WAL size in GiB"),
      encrypted: z.boolean().optional().describe("Encrypt the OSD"),
      osds_per_device: z.number().int().positive().optional().describe("OSDs per device (fast NVMe only; excludes db_dev/wal_dev)"),
      wait: waitArg,
      extra: extraArg,
    },
    handler: async (a) => {
      const upid = await client.post(
        `${cephBase(a.node)}/osd`,
        defined(withExtra({
          dev: a.dev,
          "crush-device-class": a.device_class,
          db_dev: a.db_dev,
          db_dev_size: a.db_dev_size,
          wal_dev: a.wal_dev,
          wal_dev_size: a.wal_dev_size,
          encrypted: a.encrypted,
          "osds-per-device": a.osds_per_device,
        }, a.extra)),
      );
      return runTask(client, upid, a.wait, { node: a.node, dev: a.dev });
    },
  });

  defineTool(ctx, "proxmox_delete_ceph_osd", {
    toolset: "ceph",
    title: "Destroy a Ceph OSD",
    description:
      "Destroy an OSD and remove it from the cluster. The OSD must be out and stopped first (proxmox_control_ceph_osd action=out, then proxmox_control_ceph_service action=stop service=osd.<id>) and data rebalanced; check with proxmox_check_ceph_safety. cleanup=true also wipes the disk.",
    delete: true,
    input: {
      osdid: osdIdArg,
      node: z.string().optional().describe("Node hosting the OSD (looked up when omitted)"),
      cleanup: z.boolean().optional().describe("Also zap the logical volumes and wipe the disk"),
      wait: waitArg,
    },
    handler: async (a) => {
      const node = await osdHost(client, a.osdid, a.node);
      const upid = await client.delete(`${cephBase(node)}/osd/${a.osdid}`, defined({ cleanup: a.cleanup }));
      return runTask(client, upid, a.wait, { osd: a.osdid, node });
    },
  });

  // ---------------------------------------------------------------- pools write

  const poolSettings = {
    size: z.number().int().min(1).max(7).optional().describe("Number of replicas (default 3)"),
    min_size: z.number().int().min(1).max(7).optional().describe("Minimum replicas needed to serve I/O (default 2)"),
    pg_num: z.number().int().min(1).max(32768).optional().describe("Number of placement groups"),
    pg_num_min: z.number().int().max(32768).optional().describe("Minimum PG count for the autoscaler"),
    pg_autoscale_mode: z.enum(["on", "off", "warn"]).optional().describe("PG autoscaler mode"),
    application: z.enum(["rbd", "cephfs", "rgw"]).optional().describe("Pool application (default rbd)"),
    crush_rule: z.string().optional().describe("CRUSH rule name (see proxmox_get_ceph_crush)"),
    target_size: z.string().optional().describe("Expected pool size for the autoscaler, e.g. 500G"),
    target_size_ratio: z.number().optional().describe("Expected share of the cluster capacity for the autoscaler"),
  };

  defineTool(ctx, "proxmox_create_ceph_pool", {
    toolset: "ceph",
    title: "Create a Ceph pool",
    description:
      "Create a Ceph pool, optionally erasure-coded and optionally added as Proxmox storage (add_storages). Defaults: size 3, min_size 2, application rbd.",
    write: true,
    destructive: false,
    input: {
      name: z.string().describe("Pool name"),
      ...poolSettings,
      add_storages: z.boolean().optional().describe("Also create Proxmox storage entries for the pool"),
      erasure_coding: z.string().optional().describe("Create an erasure-coded pool, e.g. 'k=2,m=1' (property string; see the Proxmox docs)"),
      node: cephNodeArg,
      wait: waitArg,
      extra: extraArg,
    },
    handler: async (a) => {
      const node = await cephNode(client, a.node);
      const upid = await client.post(
        `${cephBase(node)}/pool`,
        defined(withExtra({
          name: a.name,
          size: a.size,
          min_size: a.min_size,
          pg_num: a.pg_num,
          pg_num_min: a.pg_num_min,
          pg_autoscale_mode: a.pg_autoscale_mode,
          application: a.application,
          crush_rule: a.crush_rule,
          target_size: a.target_size,
          target_size_ratio: a.target_size_ratio,
          add_storages: a.add_storages,
          "erasure-coding": a.erasure_coding,
        }, a.extra)),
      );
      return runTask(client, upid, a.wait, { pool: a.name });
    },
  });

  defineTool(ctx, "proxmox_update_ceph_pool", {
    toolset: "ceph",
    title: "Change Ceph pool settings",
    description:
      "Change a pool's size/min_size, PG count, autoscaler, application or CRUSH rule. Only given fields change. Changing size, pg_num or crush_rule moves data and can load the cluster for a long time.",
    write: true,
    destructive: true,
    input: { name: z.string().describe("Pool name"), ...poolSettings, node: cephNodeArg, wait: waitArg, extra: extraArg },
    handler: async (a) => {
      const node = await cephNode(client, a.node);
      const params = defined(withExtra({
        size: a.size,
        min_size: a.min_size,
        pg_num: a.pg_num,
        pg_num_min: a.pg_num_min,
        pg_autoscale_mode: a.pg_autoscale_mode,
        application: a.application,
        crush_rule: a.crush_rule,
        target_size: a.target_size,
        target_size_ratio: a.target_size_ratio,
      }, a.extra));
      if (!Object.keys(params).length) throw new Error("Nothing to change: give at least one setting");
      const upid = await client.put(`${cephBase(node)}/pool/${enc(a.name)}`, params);
      return runTask(client, upid, a.wait, { pool: a.name, changed: Object.keys(params) });
    },
  });

  defineTool(ctx, "proxmox_delete_ceph_pool", {
    toolset: "ceph",
    title: "Destroy a Ceph pool",
    description: "Destroy a Ceph pool and ALL data in it. Refused while in use unless force=true. Optionally removes the Proxmox storage entries for it.",
    delete: true,
    input: {
      name: z.string().describe("Pool name"),
      force: z.boolean().optional().describe("Destroy even if guest disks still use it"),
      remove_storages: z.boolean().optional().describe("Also remove Proxmox storage entries for this pool"),
      remove_ecprofile: z.boolean().optional().describe("Remove the erasure-code profile (default true where applicable)"),
      node: cephNodeArg,
      wait: waitArg,
    },
    handler: async (a) => {
      const node = await cephNode(client, a.node);
      const upid = await client.delete(
        `${cephBase(node)}/pool/${enc(a.name)}`,
        defined({ force: a.force, remove_storages: a.remove_storages, remove_ecprofile: a.remove_ecprofile }),
      );
      return runTask(client, upid, a.wait, { pool: a.name });
    },
  });

  // ---------------------------------------------------------------- mon / mgr / mds

  defineTool(ctx, "proxmox_create_ceph_service", {
    toolset: "ceph",
    title: "Create a Ceph monitor, manager or MDS",
    description:
      "Create a Ceph monitor (mon), manager (mgr) or metadata server (mds) on a node. The daemon ID defaults to the node name. The first monitor also gets a manager.",
    write: true,
    destructive: false,
    input: {
      type: z.enum(SERVICE_TYPES).describe("Daemon type"),
      node: targetNodeArg,
      id: z.string().optional().describe("Daemon ID (default the node name)"),
      mon_address: z.array(z.string()).optional().describe("mon only: IP address(es) in the Ceph public network (default auto-detected)"),
      hotstandby: z.boolean().optional().describe("mds only: poll the active MDS's log for faster failover"),
      wait: waitArg,
    },
    handler: async (a) => {
      const id = a.id ?? a.node;
      const params = defined({
        "mon-address": a.type === "mon" ? a.mon_address?.join(",") : undefined,
        hotstandby: a.type === "mds" ? a.hotstandby : undefined,
      });
      const upid = await client.post(`${cephBase(a.node)}/${a.type}/${enc(id)}`, params);
      return runTask(client, upid, a.wait, { type: a.type, id, node: a.node });
    },
  });

  defineTool(ctx, "proxmox_delete_ceph_service", {
    toolset: "ceph",
    title: "Destroy a Ceph monitor, manager or MDS",
    description:
      "Destroy a Ceph monitor, manager or metadata server. The last monitor can't be removed; keep enough monitors for quorum. Check with proxmox_check_ceph_safety first.",
    delete: true,
    input: {
      type: z.enum(SERVICE_TYPES).describe("Daemon type"),
      node: targetNodeArg,
      id: z.string().describe("Daemon ID (see proxmox_list_ceph_services)"),
      wait: waitArg,
    },
    handler: async (a) => {
      const upid = await client.delete(`${cephBase(a.node)}/${a.type}/${enc(a.id)}`);
      return runTask(client, upid, a.wait, { type: a.type, id: a.id, node: a.node });
    },
  });

  defineTool(ctx, "proxmox_create_ceph_fs", {
    toolset: "ceph",
    title: "Create a CephFS",
    description: "Create a CephFS file system (with its data and metadata pools). Needs at least one MDS (proxmox_create_ceph_service type=mds). add_storage=true also adds it as Proxmox storage.",
    write: true,
    destructive: false,
    input: {
      name: z.string().optional().describe("File system name (default cephfs)"),
      node: targetNodeArg,
      pg_num: z.number().int().positive().optional().describe("PGs of the data pool (default 128; metadata pool gets a quarter)"),
      add_storage: z.boolean().optional().describe("Add it as Proxmox storage"),
      wait: waitArg,
    },
    handler: async (a) => {
      const name = a.name ?? "cephfs";
      const upid = await client.post(`${cephBase(a.node)}/fs/${enc(name)}`, defined({ pg_num: a.pg_num, "add-storage": a.add_storage }));
      return runTask(client, upid, a.wait, { fs: name });
    },
  });

  defineTool(ctx, "proxmox_delete_ceph_fs", {
    toolset: "ceph",
    title: "Destroy a CephFS",
    description: "Destroy a CephFS file system. Refused while an enabled Proxmox cephfs storage uses it. remove_pools=true also deletes its pools and ALL data.",
    delete: true,
    input: {
      name: z.string().describe("File system name"),
      node: targetNodeArg,
      remove_pools: z.boolean().optional().describe("Also remove the data and metadata pools"),
      remove_storages: z.boolean().optional().describe("Also remove the Proxmox storage entries for it"),
      wait: waitArg,
    },
    handler: async (a) => {
      const upid = await client.delete(`${cephBase(a.node)}/fs/${enc(a.name)}`, defined({ "remove-pools": a.remove_pools, "remove-storages": a.remove_storages }));
      return runTask(client, upid, a.wait, { fs: a.name });
    },
  });

  // ---------------------------------------------------------------- flags / health

  defineTool(ctx, "proxmox_set_ceph_flags", {
    toolset: "ceph",
    title: "Set or clear Ceph OSD flags",
    description:
      "Set (true) or clear (false) cluster-wide Ceph OSD flags; omitted flags are left alone (nodeep-scrub is nodeep_scrub here). Typical: noout=true before node maintenance, then noout=false afterwards. pause=true stops ALL client I/O.",
    write: true,
    destructive: true,
    input: {
      ...FLAG_ARGS,
      wait: waitArg,
    },
    handler: async (a) => {
      const flags = defined(Object.fromEntries(FLAGS.map((f) => [f, (a as Obj)[f.replace("-", "_")]])));
      if (!Object.keys(flags).length) throw new Error("Give at least one flag to set or clear");
      const upid = await client.put("/cluster/ceph/flags", flags);
      const result = await runTask(client, upid, a.wait, { changed: flags });
      const current = await client.get<Obj[]>("/cluster/ceph/flags").catch(() => undefined);
      return compact({ ...result, now_set: current?.filter((f) => f.value).map((f) => f.name) });
    },
  });

  defineTool(ctx, "proxmox_mute_ceph_health_check", {
    toolset: "ceph",
    title: "Mute or unmute a Ceph health check",
    description:
      "Mute (or unmute) a Ceph health check such as AUTH_INSECURE_CLIENT_KEY_TYPE so it no longer degrades the health status; it stays visible in proxmox_get_ceph_status. Optionally for a limited time (ttl).",
    write: true,
    destructive: false,
    idempotent: true,
    input: {
      code: z.string().describe("Health check code from proxmox_get_ceph_status, e.g. OSD_NEARFULL"),
      mute: z.boolean().optional().describe("true to mute (default), false to unmute"),
      ttl: z.string().optional().describe("Mute duration, e.g. '2h', '3d', '1w' (default no expiry)"),
      sticky: z.boolean().optional().describe("Keep the mute even if the check gets worse"),
    },
    handler: async (a) => {
      const value = a.mute ?? true;
      await client.put(`/cluster/ceph/health-mute/${enc(a.code)}`, defined({ value, ttl: value ? a.ttl : undefined, sticky: value ? a.sticky : undefined }));
      return { code: a.code, muted: value };
    },
  });

  // ---------------------------------------------------------------- node services

  defineTool(ctx, "proxmox_control_ceph_service", {
    toolset: "ceph",
    title: "Start, stop or restart Ceph services",
    description:
      "Start, stop or restart Ceph services on a node: all of them (default ceph.target), all of one type ('osd', 'mon', 'mgr', 'mds') or one daemon as <type>.<id>, e.g. 'osd.3', 'mon.pve1', 'mgr.pve1', 'mds.pve1'. Stopping can make data unavailable; check proxmox_check_ceph_safety and consider the noout flag.",
    write: true,
    destructive: true,
    input: {
      action: z.enum(["start", "stop", "restart"]).describe("What to do"),
      node: targetNodeArg,
      service: z.string().optional().describe("Service as <type>[.<id>], e.g. osd.3, mon.pve1 or osd (all OSDs); default ceph.target = all Ceph services on the node"),
      wait: waitArg,
    },
    handler: async (a) => {
      // Proxmox wants <type>.<id>; accept the systemd unit names (ceph-osd@3) models tend to use too.
      const service = a.service?.replace(/^ceph-(mon|mgr|mds|osd)@(.+?)(\.service)?$/, "$1.$2");
      const upid = await client.post(`${cephBase(a.node)}/${a.action}`, defined({ service }));
      return runTask(client, upid, a.wait, { node: a.node, action: a.action, service: service ?? "ceph.target" });
    },
  });

  defineTool(ctx, "proxmox_init_ceph", {
    toolset: "ceph",
    title: "Initialise Ceph configuration",
    description:
      "Create the initial Ceph configuration (ceph.conf) for the cluster with the public and optional cluster network. Run once on the first node after installing Ceph, then create monitors. Existing settings are preserved on re-run.",
    write: true,
    destructive: false,
    idempotent: true,
    input: {
      node: targetNodeArg,
      network: z.string().optional().describe("Public network for Ceph traffic in CIDR, e.g. 10.10.10.0/24"),
      cluster_network: z.string().optional().describe("Separate network for OSD replication/heartbeat traffic (CIDR)"),
      size: z.number().int().min(1).max(7).optional().describe("Default pool replicas (default 3)"),
      min_size: z.number().int().min(1).max(7).optional().describe("Default pool min_size (default 2)"),
      extra: extraArg,
    },
    handler: async (a) => {
      await client.post(
        `${cephBase(a.node)}/init`,
        defined(withExtra({ network: a.network, "cluster-network": a.cluster_network, size: a.size, min_size: a.min_size }, a.extra)),
      );
      return { initialised: true, node: a.node };
    },
  });

}
