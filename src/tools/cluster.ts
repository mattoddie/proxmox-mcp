import { z } from "zod";
import { compact, formatBytes, formatUptime, fractionPct, isoTime, percent, redactSecrets, usage } from "../proxmox/format.js";
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
  runTask,
  searchArg,
  waitArg,
  type ToolContext,
} from "./util.js";

type Obj = Record<string, any>;

/** Compact summary of a /cluster/resources guest entry (VM or container). */
export function summarizeGuest(r: Obj): Obj {
  return compact({
    vmid: Number(r.vmid),
    name: r.name,
    type: r.type === "qemu" ? "vm" : r.type === "lxc" ? "container" : r.type,
    node: r.node,
    status: r.status,
    template: r.template ? true : undefined,
    lock: r.lock,
    hastate: r.hastate,
    pool: r.pool,
    tags: r.tags ? String(r.tags).split(/[;,\s]+/).filter(Boolean) : undefined,
    cpus: r.maxcpu,
    cpu_pct: r.status === "running" ? fractionPct(r.cpu) : undefined,
    memory: r.status === "running" ? usage(r.mem, r.maxmem) : formatBytes(r.maxmem),
    disk: r.maxdisk ? (r.disk ? usage(r.disk, r.maxdisk) : formatBytes(r.maxdisk)) : undefined,
    uptime: r.status === "running" ? formatUptime(r.uptime) : undefined,
    net_in: r.status === "running" ? formatBytes(r.netin) : undefined,
    net_out: r.status === "running" ? formatBytes(r.netout) : undefined,
  });
}

/** Compact summary of a node entry from /nodes or /cluster/resources. */
export function summarizeNode(n: Obj): Obj {
  return compact({
    node: n.node,
    status: n.status,
    cpu_pct: n.status === "online" ? fractionPct(n.cpu) : undefined,
    cpus: n.maxcpu,
    memory: usage(n.mem, n.maxmem),
    root_disk: usage(n.disk, n.maxdisk),
    uptime: formatUptime(n.uptime),
    subscription_level: n.level || undefined,
    ssl_fingerprint: n.ssl_fingerprint,
  });
}

function summarizeStorage(s: Obj): Obj {
  return compact({
    storage: s.storage,
    node: s.node,
    type: s.plugintype,
    status: s.status,
    shared: s.shared ? true : undefined,
    content: s.content,
    usage: usage(s.disk, s.maxdisk),
    used_pct: percent(s.disk, s.maxdisk),
  });
}

const GUEST_SORTS = ["vmid", "name", "cpu", "memory", "disk", "uptime"] as const;

/**
 * redactSecrets() plus secrets it can't recognise by key name: credentials in
 * the http_proxy URL (user:password@host) and OpenTelemetry auth headers.
 */
function redactConfig<T>(value: T): T {
  const out: any = redactSecrets(value);
  const fix = (o: Obj) => {
    if (typeof o.http_proxy === "string") o.http_proxy = o.http_proxy.replace(/\/\/[^/@\s]+@/, "//<redacted>@");
    if (o["otel-headers"]) o["otel-headers"] = "<redacted>";
  };
  if (Array.isArray(out)) out.forEach((o) => o && typeof o === "object" && fix(o));
  else if (out && typeof out === "object") fix(out);
  return out;
}

export function registerClusterTools(ctx: ToolContext): void {
  const { client } = ctx;

  // ---------------------------------------------------------------- read

  defineTool(ctx, "proxmox_get_version", {
    toolset: "cluster",
    title: "Get Proxmox VE version",
    description: "Get the Proxmox VE version and release of the API endpoint this server talks to.",
    input: {},
    handler: async () => client.get("/version"),
  });

  defineTool(ctx, "proxmox_get_cluster_status", {
    toolset: "cluster",
    title: "Get cluster overview",
    description:
      "One-call overview of the whole cluster: name, quorum, Proxmox version, every node (online state, CPU, memory, uptime), guest counts by state, totals, storage usage and HA state. A good first call.",
    input: {},
    handler: async () => {
      const [status, resources, version] = await Promise.all([
        client.get<Obj[]>("/cluster/status"),
        client.get<Obj[]>("/cluster/resources"),
        client.get<Obj>("/version").catch(() => undefined),
      ]);
      const cluster = status.find((s) => s.type === "cluster");
      const nodeStatus = new Map(status.filter((s) => s.type === "node").map((s) => [s.name, s]));
      const nodes = resources.filter((r) => r.type === "node");
      const guests = resources.filter((r) => r.type === "qemu" || r.type === "lxc");
      const storages = resources.filter((r) => r.type === "storage");
      const count = (list: Obj[], pred: (g: Obj) => boolean) => list.filter(pred).length;
      const sum = (list: Obj[], k: string) => list.reduce((n, r) => n + (Number(r[k]) || 0), 0);
      const online = nodes.filter((n) => n.status === "online");
      return compact({
        cluster: cluster
          ? { name: cluster.name, quorate: Boolean(cluster.quorate), nodes: cluster.nodes, config_version: cluster.version }
          : { name: "(standalone node, no cluster)" },
        version: version ? `${version.version} (release ${version.release})` : undefined,
        nodes: nodes.map((n) =>
          compact({ ...summarizeNode(n), ip: nodeStatus.get(n.node)?.ip, local: nodeStatus.get(n.node)?.local ? true : undefined }),
        ),
        guests: {
          vms: {
            running: count(guests, (g) => g.type === "qemu" && g.status === "running" && !g.template),
            stopped: count(guests, (g) => g.type === "qemu" && g.status !== "running" && !g.template),
            templates: count(guests, (g) => g.type === "qemu" && Boolean(g.template)),
          },
          containers: {
            running: count(guests, (g) => g.type === "lxc" && g.status === "running"),
            stopped: count(guests, (g) => g.type === "lxc" && g.status !== "running" && !g.template),
            templates: count(guests, (g) => g.type === "lxc" && Boolean(g.template)),
          },
          locked: guests.filter((g) => g.lock).map((g) => ({ vmid: g.vmid, name: g.name, lock: g.lock })),
          ha_managed: count(guests, (g) => Boolean(g.hastate)),
        },
        totals: {
          cpus: sum(online, "maxcpu"),
          cpu_pct: online.length ? fractionPct(sum(online, "cpu") / online.length) : undefined,
          memory: usage(sum(online, "mem"), sum(online, "maxmem")),
        },
        storage: storages.map(summarizeStorage),
      });
    },
  });

  defineTool(ctx, "proxmox_list_nodes", {
    toolset: "cluster",
    title: "List nodes",
    description: "List the cluster's nodes with online state, CPU and memory usage, uptime and subscription level.",
    input: { raw: rawArg },
    handler: async (a) => {
      const nodes = await client.get<Obj[]>("/nodes");
      nodes.sort((x, y) => String(x.node).localeCompare(String(y.node)));
      return a.raw ? redactSecrets(nodes) : nodes.map(summarizeNode);
    },
  });

  defineTool(ctx, "proxmox_list_guests", {
    toolset: "cluster",
    title: "List VMs and containers",
    description:
      "List virtual machines and containers across the cluster with status, node, CPU, memory, disk, uptime, tags and pool. Filter by type, node, status, tag, pool or a search string, and sort by resource use.",
    input: {
      type: z.enum(["vm", "container"]).optional().describe("Only VMs (qemu) or only containers (lxc)"),
      node: z.string().optional().describe("Only guests on this node"),
      status: z.enum(["running", "stopped", "paused", "suspended"]).optional().describe("Only guests in this state"),
      tag: z.string().optional().describe("Only guests with this tag"),
      pool: z.string().optional().describe("Only guests in this resource pool"),
      templates: z.enum(["include", "exclude", "only"]).optional().describe("How to treat templates (default include)"),
      search: searchArg,
      sort: z.enum(GUEST_SORTS).optional().describe("Sort order (default vmid); cpu/memory/disk/uptime sort highest first"),
      limit: limitArg(500),
      raw: rawArg,
    },
    handler: async (a) => {
      let guests = await client.get<Obj[]>("/cluster/resources", { type: "vm" });
      if (a.type) guests = guests.filter((g) => g.type === (a.type === "vm" ? "qemu" : "lxc"));
      if (a.node) guests = guests.filter((g) => g.node === a.node);
      if (a.status) guests = guests.filter((g) => g.status === a.status);
      if (a.tag) {
        const tag = a.tag.toLowerCase();
        guests = guests.filter((g) => String(g.tags ?? "").toLowerCase().split(/[;,\s]+/).includes(tag));
      }
      if (a.pool) guests = guests.filter((g) => g.pool === a.pool);
      if (a.templates === "exclude") guests = guests.filter((g) => !g.template);
      if (a.templates === "only") guests = guests.filter((g) => g.template);
      const key: Record<string, (g: Obj) => number | string> = {
        vmid: (g) => Number(g.vmid),
        name: (g) => String(g.name ?? ""),
        cpu: (g) => -(Number(g.cpu) || 0),
        memory: (g) => -(Number(g.mem) || 0),
        disk: (g) => -(Number(g.disk) || Number(g.maxdisk) || 0),
        uptime: (g) => -(Number(g.uptime) || 0),
      };
      const k = key[a.sort ?? "vmid"];
      guests.sort((x, y) => {
        const [p, q] = [k(x), k(y)];
        return typeof p === "string" ? p.localeCompare(String(q)) : p - (q as number);
      });
      const res = filterAndLimit(guests, { search: a.search, limit: a.limit ?? 500 }, (g) => [g.vmid, g.name, g.tags, g.node, g.pool]);
      return { ...res, items: a.raw ? redactSecrets(res.items) : res.items.map(summarizeGuest) };
    },
  });

  defineTool(ctx, "proxmox_list_resources", {
    toolset: "cluster",
    title: "List cluster resources",
    description:
      "List cluster resources of any kind from /cluster/resources: nodes, guests, storage, pools and SDN zones. Use proxmox_list_guests for a friendlier guest list.",
    input: {
      type: z.enum(["node", "vm", "storage", "pool", "sdn"]).optional().describe("Resource type (default all)"),
      search: searchArg,
      limit: limitArg(1000),
    },
    handler: async (a) => {
      const items = await client.get<Obj[]>("/cluster/resources", defined({ type: a.type }));
      const res = filterAndLimit(items, { search: a.search, limit: a.limit ?? 1000 }, (r) => [r.id, r.name, r.node, r.storage, r.pool, r.tags]);
      return { ...res, items: res.items.map((r) => compact(r)) };
    },
  });

  defineTool(ctx, "proxmox_get_next_vmid", {
    toolset: "cluster",
    title: "Get next free VMID",
    description: "Get the next free VM/container ID, or check whether a specific ID is free.",
    input: { vmid: z.number().int().min(100).optional().describe("Check whether this ID is free instead") },
    handler: async (a) => {
      try {
        const id = await client.get("/cluster/nextid", defined({ vmid: a.vmid }));
        return { vmid: Number(id), free: true };
      } catch (err) {
        if (a.vmid !== undefined) return { vmid: a.vmid, free: false, reason: (err as Error).message };
        throw err;
      }
    },
  });

  defineTool(ctx, "proxmox_get_cluster_log", {
    toolset: "cluster",
    title: "Get cluster log",
    description: "Read recent entries of the cluster log (task starts/ends, logins and other cluster events from all nodes).",
    input: { max: z.number().int().positive().max(5000).optional().describe("Maximum number of entries (default 50)"), search: searchArg },
    handler: async (a) => {
      const entries = await client.get<Obj[]>("/cluster/log", { max: a.max ?? 50 });
      const res = filterAndLimit(entries, { search: a.search }, (e) => [e.msg, e.node, e.user, e.tag]);
      return {
        ...res,
        items: res.items.map((e) => compact({ time: isoTime(e.time), node: e.node, user: e.user, tag: e.tag, severity: e.pri, msg: e.msg })),
      };
    },
  });

  defineTool(ctx, "proxmox_get_cluster_options", {
    toolset: "cluster",
    title: "Get datacenter options",
    description: "Get datacenter-wide options (datacenter.cfg): keyboard, console, migration network/type, HA shutdown policy, bandwidth limits, MAC prefix, tag style, next-id range and more.",
    input: {},
    handler: async () => redactConfig(await client.get("/cluster/options")),
  });

  defineTool(ctx, "proxmox_get_cluster_config", {
    toolset: "cluster",
    title: "Get cluster (corosync) configuration",
    description: "Get the corosync cluster configuration: member nodes with their IDs, votes and link addresses, the totem settings, and the join information (fingerprint, addresses) used to add nodes.",
    input: {},
    handler: async () => {
      const [nodes, totem, join, qdevice] = await Promise.all([
        client.get<Obj[]>("/cluster/config/nodes").catch((e: Error) => ({ error: e.message })),
        client.get<Obj>("/cluster/config/totem").catch((e: Error) => ({ error: e.message })),
        client.get<Obj>("/cluster/config/join").catch((e: Error) => ({ error: e.message })),
        client.get<Obj>("/cluster/config/qdevice").catch(() => undefined),
      ]);
      return compact({ nodes, totem, join: redactSecrets(join), qdevice });
    },
  });

  defineTool(ctx, "proxmox_list_metric_servers", {
    toolset: "cluster",
    title: "List metric servers",
    description: "List external metric servers (InfluxDB, Graphite, OpenTelemetry) that Proxmox sends performance data to.",
    input: {},
    handler: async () => redactConfig(await client.get("/cluster/metrics/server")),
  });

  defineTool(ctx, "proxmox_simulate_schedule", {
    toolset: "cluster",
    title: "Simulate a job schedule",
    description:
      "Show the next run times of a Proxmox calendar-event schedule (as used by backup, replication and other jobs), e.g. 'mon..fri 02:00' or '*/15'. Useful to check a schedule before saving it.",
    input: {
      schedule: z.string().describe("Calendar event, e.g. 'daily', 'sat 03:30', '*/2:00'"),
      iterations: z.number().int().positive().max(100).optional().describe("How many run times to show (default 10)"),
      starttime: z.number().int().optional().describe("Start from this UNIX time instead of now"),
    },
    handler: async (a) => {
      const runs = await client.get<Obj[]>("/cluster/jobs/schedule-analyze", defined({ schedule: a.schedule, iterations: a.iterations ?? 10, starttime: a.starttime }));
      return runs.map((r) => isoTime(r.timestamp) ?? r.utc);
    },
  });

  // ---------------------------------------------------------------- write

  defineTool(ctx, "proxmox_update_cluster_options", {
    toolset: "cluster",
    title: "Update datacenter options",
    description:
      "Change datacenter-wide options (datacenter.cfg). Only the given fields change. Common ones are typed; anything else (bwlimit, ha, crs, migration, next-id, tag-style, u2f, webauthn, notify...) can be set via `extra` in Proxmox's property-string format.",
    write: true,
    destructive: true,
    input: {
      keyboard: z.string().optional().describe("Default VNC keyboard layout, e.g. en-gb"),
      language: z.string().optional().describe("Default GUI language, e.g. en"),
      console: z.enum(["applet", "vv", "html5", "xtermjs"]).optional().describe("Default console viewer"),
      email_from: z.string().optional().describe("Sender address for notification emails"),
      http_proxy: z.string().optional().describe("Proxy for downloads (e.g. http://proxy:3128)"),
      mac_prefix: z.string().optional().describe("Prefix for auto-generated MAC addresses"),
      max_workers: z.number().int().positive().optional().describe("Maximum parallel workers for bulk actions"),
      migration: z.string().optional().describe("Migration settings, e.g. 'type=insecure,network=10.0.0.0/24'"),
      ha: z.string().optional().describe("HA settings, e.g. 'shutdown_policy=migrate'"),
      description: z.string().optional().describe("Datacenter notes (Markdown)"),
      delete: deleteKeysArg,
      digest: digestArg,
      extra: extraArg,
    },
    handler: async (a) => {
      await client.put(
        "/cluster/options",
        defined({
          keyboard: a.keyboard,
          language: a.language,
          console: a.console,
          email_from: a.email_from,
          http_proxy: a.http_proxy,
          mac_prefix: a.mac_prefix,
          max_workers: a.max_workers,
          migration: a.migration,
          ha: a.ha,
          description: a.description,
          delete: deleteParam(a.delete),
          digest: a.digest,
          ...a.extra,
        }),
      );
      return { updated: true, options: redactConfig(await client.get("/cluster/options")) };
    },
  });

  defineTool(ctx, "proxmox_save_metric_server", {
    toolset: "cluster",
    title: "Create or update a metric server",
    description:
      "Create (create=true) or update an external metric server for InfluxDB, Graphite or OpenTelemetry. Only the given fields change on update.",
    write: true,
    input: {
      id: z.string().describe("Metric server ID"),
      create: z.boolean().optional().describe("Create a new server (default false = update an existing one)"),
      type: z.enum(["influxdb", "graphite", "opentelemetry"]).optional().describe("Server type (required when creating)"),
      server: z.string().optional().describe("Server address (required when creating)"),
      port: z.number().int().positive().optional().describe("Server port (required when creating)"),
      disable: z.boolean().optional().describe("Disable sending metrics"),
      extra: extraArg,
    },
    handler: async (a) => {
      const params = defined({ type: a.create ? a.type : undefined, server: a.server, port: a.port, disable: a.disable, ...a.extra });
      if (a.create) {
        if (!a.type || !a.server || !a.port) throw new Error("type, server and port are required when creating a metric server");
        await client.post(`/cluster/metrics/server/${enc(a.id)}`, params);
      } else {
        await client.put(`/cluster/metrics/server/${enc(a.id)}`, params);
      }
      return { saved: a.id, server: redactConfig(await client.get(`/cluster/metrics/server/${enc(a.id)}`)) };
    },
  });

  defineTool(ctx, "proxmox_delete_metric_server", {
    toolset: "cluster",
    title: "Delete a metric server",
    description: "Remove an external metric server configuration.",
    delete: true,
    input: { id: z.string().describe("Metric server ID") },
    handler: async (a) => {
      await client.delete(`/cluster/metrics/server/${enc(a.id)}`);
      return { deleted: a.id };
    },
  });

  defineTool(ctx, "proxmox_bulk_guest_action", {
    toolset: "cluster",
    title: "Bulk start, shut down, suspend or migrate guests",
    description:
      "Start, shut down, suspend or migrate many VMs and containers at once with Proxmox's bulk action (Proxmox VE 9+). Select guests with `vmids`; for migrate also give `target`. Runs as one task.",
    write: true,
    destructive: true,
    input: {
      action: z.enum(["start", "shutdown", "suspend", "migrate"]).describe("What to do"),
      vmids: z.array(z.number().int().min(100)).min(1).describe("Guest IDs to act on"),
      target: z.string().optional().describe("Target node (migrate only)"),
      online: z.boolean().optional().describe("Live-migrate running VMs and restart-migrate running containers (migrate only)"),
      timeout: z.number().int().positive().optional().describe("Shutdown timeout in seconds before giving up (shutdown only)"),
      force_stop: z.boolean().optional().describe("Hard-stop guests that don't shut down within the timeout (shutdown only)"),
      with_local_disks: z.boolean().optional().describe("Also migrate local disks of running VMs (migrate only)"),
      to_disk: z.boolean().optional().describe("Suspend to disk (hibernate) instead of to RAM (suspend only)"),
      max_workers: z.number().int().positive().optional().describe("How many guests to handle in parallel"),
      wait: waitArg,
      extra: extraArg,
    },
    handler: async (a) => {
      if (a.action === "migrate" && !a.target) throw new Error("target is required for migrate");
      const params = defined({
        vms: a.vmids,
        target: a.target,
        online: a.online,
        timeout: a.timeout,
        "force-stop": a.force_stop,
        "max-workers": a.max_workers,
        "with-local-disks": a.with_local_disks,
        "to-disk": a.to_disk,
        ...a.extra,
      });
      const upid = await client.post(`/cluster/bulk-action/guest/${a.action}`, params);
      return runTask(client, upid, a.wait, { action: a.action, vmids: a.vmids });
    },
  });
}
