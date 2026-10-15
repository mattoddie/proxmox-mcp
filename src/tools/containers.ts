import { z } from "zod";
import type { GuestRef } from "../proxmox/client.js";
import {
  compact,
  flag,
  formatBytes,
  formatUptime,
  fractionPct,
  isoTime,
  parsePropertyString,
  redactSecrets,
  summarizeDisks,
  summarizeNets,
  toPropertyString,
  usage,
} from "../proxmox/format.js";
import { summarizeGuest } from "./cluster.js";
import {
  defined,
  defineTool,
  deleteKeysArg,
  deleteParam,
  digestArg,
  extraArg,
  filterAndLimit,
  guestNodeArg,
  limitArg,
  nodeArg,
  rawArg,
  runTask,
  searchArg,
  vmidArg,
  waitArg,
  withExtra,
  type ToolContext,
} from "./util.js";

type Obj = Record<string, any>;

// ---------------------------------------------------------------- argument shapes

const volumeKeyArg = z
  .string()
  .regex(/^(rootfs|mp\d+)$/, "must be rootfs or mpN")
  .describe("Container volume: rootfs or a mount point key such as mp0");

const tagsArg = z.array(z.string()).optional().describe("Tags, e.g. [\"prod\", \"dns\"] (replaces existing tags)");

const netSpec = z.object({
  name: z.string().optional().describe("Interface name inside the container (default eth0, eth1... by position)"),
  bridge: z.string().optional().describe("Host bridge (default vmbr0)"),
  ip: z.string().optional().describe("IPv4: \"dhcp\", \"manual\" or CIDR such as \"192.168.1.50/24\""),
  gw: z.string().optional().describe("IPv4 gateway (with a static ip)"),
  ip6: z.string().optional().describe("IPv6: \"auto\" (SLAAC), \"dhcp\", \"manual\" or CIDR"),
  gw6: z.string().optional().describe("IPv6 gateway"),
  vlan: z.number().int().min(1).max(4094).optional().describe("VLAN tag"),
  firewall: z.boolean().optional().describe("Enable the Proxmox firewall on this interface"),
  hwaddr: z.string().optional().describe("MAC address (default: generated)"),
  mtu: z.number().int().min(64).max(65535).optional().describe("MTU"),
  rate: z.number().positive().optional().describe("Rate limit in MB/s"),
  link_down: z.boolean().optional().describe("Disconnect the interface"),
});
type NetSpec = z.infer<typeof netSpec>;

const mountSpec = z.object({
  path: z.string().optional().describe("Mount path inside the container, e.g. /data (required for new mount points)"),
  storage: z.string().optional().describe("Storage to allocate a new volume on (with size_gb)"),
  size_gb: z.number().positive().optional().describe("Size of the new volume in GiB (with storage)"),
  volume: z.string().optional().describe("Existing volume ID, or a host directory to bind-mount such as /mnt/tank/media (instead of storage+size_gb)"),
  backup: z.boolean().optional().describe("Include the volume in backups"),
  ro: z.boolean().optional().describe("Mount read-only"),
  acl: z.boolean().optional().describe("Enable/disable ACL support"),
  quota: z.boolean().optional().describe("Enable user quotas inside the container"),
  replicate: z.boolean().optional().describe("Include in storage replication jobs (default true)"),
  shared: z.boolean().optional().describe("Mark a non-volume mount as available on all nodes"),
  mountoptions: z.string().optional().describe("Extra mount options separated by ';', e.g. noatime;nodev"),
});
type MountSpec = z.infer<typeof mountSpec>;

const featuresSpec = z
  .object({
    nesting: z.boolean().optional().describe("Allow nested containers/namespaces (needed by systemd in modern distros and by Docker)"),
    keyctl: z.boolean().optional().describe("Allow the keyctl() syscall (unprivileged only; needed by Docker)"),
    fuse: z.boolean().optional().describe("Allow FUSE mounts"),
    mknod: z.boolean().optional().describe("Allow mknod for some device nodes (experimental)"),
    force_rw_sys: z.boolean().optional().describe("Mount /sys read-write in unprivileged containers"),
    mount: z.string().optional().describe("Allowed filesystem types to mount, separated by ';', e.g. nfs;cifs (privileged only)"),
  })
  .describe("Container features; booleans set to false are written as 0");
type Features = z.infer<typeof featuresSpec>;

// ---------------------------------------------------------------- helpers

/** Builds a netN property string, overlaying `spec` on an existing value (so the MAC and other options survive). */
function netString(spec: NetSpec, index: number, existing?: string): string {
  const base: Obj = existing ? parsePropertyString(existing) : { name: `eth${index}`, bridge: "vmbr0" };
  const { vlan, link_down, ...rest } = spec;
  const merged: Obj = { ...base, ...defined({ ...rest, tag: vlan, link_down }) };
  if (!existing && merged.ip === undefined && merged.ip6 === undefined) merged.ip = "dhcp";
  // The property string has no default key; keep name first for readability.
  const { name, ...others } = merged;
  return toPropertyString({ name, ...others });
}

function mountString(spec: MountSpec, key: string, existing?: string): string {
  const base = existing ? parsePropertyString(existing, "volume") : {};
  const volume = spec.volume ?? (spec.storage ? `${spec.storage}:${spec.size_gb ?? ""}` : base.volume);
  if (!volume || /:$/.test(volume)) throw new Error(`${key}: give either volume, or storage with size_gb`);
  const path = spec.path ?? base.mp;
  if (!path && key !== "rootfs") throw new Error(`${key}: path is required for a new mount point`);
  const { path: _p, storage: _s, size_gb: _z, volume: _v, ...opts } = spec;
  return toPropertyString({ ...base, volume, mp: key === "rootfs" ? undefined : path, ...defined(opts) }, "volume");
}

function featuresString(f: Features, existing?: string): string {
  return toPropertyString({ ...parsePropertyString(existing), ...defined(f) });
}

function parseFeatures(value: unknown): Obj | undefined {
  if (!value) return undefined;
  const p = parsePropertyString(value);
  return Object.fromEntries(Object.entries(p).map(([k, v]) => [k, k === "mount" ? v : flag(v)]));
}

function splitTags(tags: unknown): string[] | undefined {
  return tags ? String(tags).split(/[;,\s]+/).filter(Boolean) : undefined;
}

/** Pending changes from /pending: keys whose new value applies on the next restart, or that will be deleted. */
function pendingChanges(items: Obj[]): Obj[] {
  return items.filter((p) => p.pending !== undefined || p.delete).map((p) => pendingEntry(p, true));
}

/** One /pending entry, with secret-looking values (by config key name) redacted. */
function pendingEntry(p: Obj, changesOnly = false): Obj {
  const hide = (v: unknown) => (v === undefined ? undefined : redactSecrets({ [p.key]: v })[p.key]);
  return compact({
    key: p.key,
    [changesOnly ? "current" : "value"]: hide(p.value),
    pending: hide(p.pending),
    delete: p.delete ? true : undefined,
  });
}

/** Live addresses from /interfaces (format differs slightly between versions). */
function summarizeInterfaces(ifaces: Obj[]): Obj[] {
  return ifaces
    .filter((i) => i.name !== "lo")
    .map((i) => {
      const addrs: string[] = [];
      for (const a of i["ip-addresses"] ?? []) addrs.push(`${a["ip-address"]}${a.prefix !== undefined ? `/${a.prefix}` : ""}`);
      for (const k of ["inet", "inet6"]) {
        const v = i[k];
        if (Array.isArray(v)) addrs.push(...v.map(String));
        else if (v) addrs.push(String(v));
      }
      return compact({ name: i.name, hwaddr: i.hwaddr ?? i["hardware-address"], addresses: [...new Set(addrs)] });
    });
}

function summarizeConfig(c: Obj): Obj {
  return compact({
    hostname: c.hostname,
    description: c.description,
    arch: c.arch,
    ostype: c.ostype,
    unprivileged: flag(c.unprivileged) ?? false,
    template: flag(c.template) || undefined,
    protection: flag(c.protection) || undefined,
    onboot: flag(c.onboot) ?? false,
    startup: c.startup,
    lock: c.lock,
    cores: c.cores ?? "all host cores",
    cpulimit: c.cpulimit,
    cpuunits: c.cpuunits,
    memory: formatBytes(Number(c.memory ?? 512) * 1024 * 1024),
    swap: formatBytes(Number(c.swap ?? 512) * 1024 * 1024),
    features: parseFeatures(c.features),
    volumes: summarizeDisks(c),
    nets: summarizeNets(c),
    nameserver: c.nameserver,
    searchdomain: c.searchdomain,
    timezone: c.timezone,
    tags: splitTags(c.tags),
    hookscript: c.hookscript,
    devices: Object.fromEntries(Object.entries(c).filter(([k]) => /^dev\d+$/.test(k))),
    digest: c.digest,
  });
}

function summarizeStatus(s: Obj): Obj {
  const running = s.status === "running";
  return compact({
    status: s.status,
    lock: s.lock,
    ha: s.ha?.managed ? s.ha : undefined,
    pid: running ? s.pid : undefined,
    uptime: running ? formatUptime(s.uptime) : undefined,
    cpus: s.cpus,
    cpu_pct: running ? fractionPct(s.cpu) : undefined,
    memory: running ? usage(s.mem, s.maxmem) : formatBytes(s.maxmem),
    swap: running && s.maxswap ? usage(s.swap, s.maxswap) : undefined,
    rootfs: s.maxdisk ? (s.disk ? usage(s.disk, s.maxdisk) : formatBytes(s.maxdisk)) : undefined,
    net_in: running ? formatBytes(s.netin) : undefined,
    net_out: running ? formatBytes(s.netout) : undefined,
    disk_read: running ? formatBytes(s.diskread) : undefined,
    disk_write: running ? formatBytes(s.diskwrite) : undefined,
  });
}

type Fmt = (v: number) => unknown;
function seriesStats(points: Obj[], key: string, fmt: Fmt): Obj | undefined {
  const vals = points.map((p) => Number(p[key])).filter((v) => Number.isFinite(v));
  if (!vals.length) return undefined;
  const avg = vals.reduce((a, b) => a + b, 0) / vals.length;
  return { avg: fmt(avg), max: fmt(Math.max(...vals)), latest: fmt(vals[vals.length - 1]) };
}

const rate: Fmt = (v) => `${formatBytes(Math.round(v)) ?? "0 B"}/s`;
const bytes: Fmt = (v) => formatBytes(Math.round(v));

/**
 * Guards for options that reach beyond a plain config write: deleting an unusedN key
 * destroys that volume, and a hookscript runs as root on the node at every lifecycle event.
 */
function checkGatedParams(ctx: ToolContext, params: Obj): void {
  if (!ctx.allowDeletes && /(^|[,;\s])unused\d+/.test(String(params.delete ?? ""))) {
    throw new Error("Deleting an unusedN entry permanently destroys its volume and needs delete permission (PROXMOX_ALLOW_DELETES)");
  }
  if (!ctx.allowExec && params.hookscript !== undefined) {
    throw new Error("hookscript runs a script as root on the node, so setting it needs exec permission (PROXMOX_ALLOW_EXEC)");
  }
}

async function nextVmid(ctx: ToolContext): Promise<number> {
  return Number(await ctx.client.get("/cluster/nextid"));
}

const POWER_ACTIONS = ["start", "stop", "shutdown", "reboot", "suspend", "resume"] as const;

export function registerContainersTools(ctx: ToolContext): void {
  const { client } = ctx;
  const ct = (vmid: number, node?: string): Promise<GuestRef> => client.resolveGuest(vmid, { node, type: "lxc" });

  // ---------------------------------------------------------------- read

  defineTool(ctx, "proxmox_list_containers", {
    toolset: "containers",
    title: "List containers",
    description:
      "List LXC containers across the cluster with status, node, CPU, memory, disk, uptime, tags and pool. For VMs and containers together use proxmox_list_guests.",
    input: {
      node: z.string().optional().describe("Only containers on this node"),
      status: z.enum(["running", "stopped"]).optional().describe("Only containers in this state"),
      templates: z.enum(["include", "exclude", "only"]).optional().describe("How to treat templates (default include)"),
      search: searchArg,
      limit: limitArg(500),
      raw: rawArg,
    },
    handler: async (a) => {
      let items = (await client.get<Obj[]>("/cluster/resources", { type: "vm" })).filter((r) => r.type === "lxc");
      if (a.node) items = items.filter((r) => r.node === a.node);
      if (a.status) items = items.filter((r) => r.status === a.status);
      if (a.templates === "exclude") items = items.filter((r) => !r.template);
      if (a.templates === "only") items = items.filter((r) => r.template);
      items.sort((x, y) => Number(x.vmid) - Number(y.vmid));
      const res = filterAndLimit(items, { search: a.search, limit: a.limit ?? 500 }, (r) => [r.vmid, r.name, r.tags, r.node, r.pool]);
      return { ...res, items: a.raw ? redactSecrets(res.items) : res.items.map(summarizeGuest) };
    },
  });

  defineTool(ctx, "proxmox_get_container", {
    toolset: "containers",
    title: "Get container details",
    description:
      "Get an LXC container's live status (CPU, memory, swap, rootfs usage, uptime) together with a summary of its configuration: hostname, OS, cores, memory, swap, rootfs and mount points, network interfaces with configured IPs, live IP addresses (when running), features, unprivileged, onboot, tags and pending changes that apply on next restart. Use proxmox_get_container_config for the full raw config.",
    input: { vmid: vmidArg, node: guestNodeArg },
    handler: async (a) => {
      const g = await ct(a.vmid, a.node);
      const base = client.guestPath(g);
      const [status, config, pending] = await Promise.all([
        client.get<Obj>(`${base}/status/current`),
        client.get<Obj>(`${base}/config`),
        client.get<Obj[]>(`${base}/pending`).catch(() => [] as Obj[]),
      ]);
      const ifaces =
        status.status === "running" ? await client.get<Obj[]>(`${base}/interfaces`).catch(() => undefined) : undefined;
      return compact({
        vmid: g.vmid,
        name: status.name ?? config.hostname,
        node: g.node,
        type: "container",
        template: flag(status.template) || undefined,
        ...summarizeStatus(status),
        ip_addresses: ifaces ? summarizeInterfaces(ifaces) : undefined,
        config: summarizeConfig(config),
        pending_changes: pendingChanges(pending),
      });
    },
  });

  defineTool(ctx, "proxmox_get_container_config", {
    toolset: "containers",
    title: "Get container configuration",
    description:
      "Get an LXC container's full configuration as stored by Proxmox (secrets redacted). Options: current=true for the running values instead of pending ones, snapshot to read the config as saved in a snapshot, or pending=true to list each key with its current and pending value. The returned digest can be passed to proxmox_update_container_config for optimistic locking.",
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      current: z.boolean().optional().describe("Return current values instead of values including pending changes"),
      snapshot: z.string().optional().describe("Read the config saved in this snapshot"),
      pending: z.boolean().optional().describe("Return the key/current/pending listing from the pending endpoint instead"),
    },
    handler: async (a) => {
      const g = await ct(a.vmid, a.node);
      const base = client.guestPath(g);
      if (a.pending) {
        const items = await client.get<Obj[]>(`${base}/pending`);
        return {
          vmid: g.vmid,
          node: g.node,
          pending_changes: pendingChanges(items),
          config: items.map((p) => pendingEntry(p)),
        };
      }
      const config = await client.get<Obj>(`${base}/config`, defined({ current: a.current, snapshot: a.snapshot }));
      return { vmid: g.vmid, node: g.node, config: redactSecrets(config) };
    },
  });

  defineTool(ctx, "proxmox_get_container_interfaces", {
    toolset: "containers",
    title: "Get container IP addresses",
    description:
      "Get the live network interfaces and IP addresses (IPv4 and IPv6) inside a running LXC container, including addresses obtained via DHCP/SLAAC that the config doesn't show. The container must be running.",
    input: { vmid: vmidArg, node: guestNodeArg, raw: rawArg },
    handler: async (a) => {
      const g = await ct(a.vmid, a.node);
      if (g.status && g.status !== "running") throw new Error(`Container ${g.vmid} is ${g.status}; it must be running to read live interfaces`);
      const ifaces = await client.get<Obj[]>(`${client.guestPath(g)}/interfaces`);
      return { vmid: g.vmid, node: g.node, interfaces: a.raw ? ifaces : summarizeInterfaces(ifaces) };
    },
  });

  defineTool(ctx, "proxmox_get_container_stats", {
    toolset: "containers",
    title: "Get container performance history",
    description:
      "Summarize an LXC container's performance history from RRD data: average, maximum and latest CPU %, memory, swap, disk usage, network and disk I/O rates over the chosen timeframe. Set points=true to also get the individual data points.",
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      timeframe: z.enum(["hour", "day", "week", "month", "year"]).optional().describe("Time window (default hour)"),
      cf: z.enum(["AVERAGE", "MAX"]).optional().describe("RRD consolidation function (default AVERAGE)"),
      points: z.boolean().optional().describe("Also return the data points (compacted)"),
    },
    handler: async (a) => {
      const g = await ct(a.vmid, a.node);
      const timeframe = a.timeframe ?? "hour";
      const data = await client.get<Obj[]>(`${client.guestPath(g)}/rrddata`, defined({ timeframe, cf: a.cf }));
      const pts = data.filter((p) => p.time !== undefined && Object.keys(p).length > 1);
      const last = pts.at(-1) ?? {};
      return compact({
        vmid: g.vmid,
        node: g.node,
        timeframe,
        samples: pts.length,
        from: isoTime(pts[0]?.time),
        to: isoTime(last.time),
        cpus: last.maxcpu,
        cpu_pct: seriesStats(pts, "cpu", (v) => fractionPct(v)),
        memory: seriesStats(pts, "mem", bytes),
        memory_total: formatBytes(last.maxmem),
        swap: seriesStats(pts, "swap", bytes),
        disk: seriesStats(pts, "disk", bytes),
        disk_total: formatBytes(last.maxdisk),
        net_in: seriesStats(pts, "netin", rate),
        net_out: seriesStats(pts, "netout", rate),
        disk_read: seriesStats(pts, "diskread", rate),
        disk_write: seriesStats(pts, "diskwrite", rate),
        points: a.points
          ? pts.map((p) =>
              compact({
                time: isoTime(p.time),
                cpu_pct: fractionPct(p.cpu),
                mem: formatBytes(p.mem),
                netin: p.netin === undefined ? undefined : rate(Number(p.netin)),
                netout: p.netout === undefined ? undefined : rate(Number(p.netout)),
              }),
            )
          : undefined,
      });
    },
  });

  defineTool(ctx, "proxmox_check_container_feature", {
    toolset: "containers",
    title: "Check container feature support",
    description:
      "Check whether a container's storage supports snapshots, cloning (linked) or copying (full clone), optionally from a given snapshot, and on which nodes. Useful before proxmox_create_snapshot or proxmox_clone_container.",
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      feature: z.enum(["snapshot", "clone", "copy"]).describe("Feature to check"),
      snapname: z.string().optional().describe("Check from this snapshot"),
    },
    handler: async (a) => {
      const g = await ct(a.vmid, a.node);
      const res = await client.get<Obj>(`${client.guestPath(g)}/feature`, defined({ feature: a.feature, snapname: a.snapname }));
      return compact({ vmid: g.vmid, node: g.node, feature: a.feature, available: Boolean(res?.hasFeature), nodes: res?.nodes });
    },
  });

  defineTool(ctx, "proxmox_check_container_migration", {
    toolset: "containers",
    title: "Check container migration preconditions",
    description:
      "Check whether an LXC container can be migrated, and to which nodes: reports running state, local disks/mount points that block migration, and allowed/not-allowed target nodes. Call before proxmox_migrate_container.",
    input: { vmid: vmidArg, node: guestNodeArg, target: z.string().optional().describe("Target node to check") },
    handler: async (a) => {
      const g = await ct(a.vmid, a.node);
      const res = await client.get<Obj>(`${client.guestPath(g)}/migrate`, defined({ target: a.target }));
      return { vmid: g.vmid, node: g.node, ...res };
    },
  });

  // ---------------------------------------------------------------- write

  defineTool(ctx, "proxmox_container_power", {
    toolset: "containers",
    title: "Start, stop or reboot a container",
    description:
      "Change an LXC container's power state: start, stop (immediate, like pulling the plug), shutdown (clean, with optional timeout and force_stop), reboot (clean shutdown and start; applies pending changes), suspend or resume (experimental for containers). Returns once the task finishes unless wait=false.",
    write: true,
    destructive: true,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      action: z.enum(POWER_ACTIONS).describe("Power action"),
      timeout: z.number().int().positive().optional().describe("Seconds to wait for a clean shutdown (shutdown/reboot; Proxmox default 60)"),
      force_stop: z.boolean().optional().describe("Hard-stop if the clean shutdown doesn't finish within the timeout (shutdown only)"),
      overrule_shutdown: z.boolean().optional().describe("Abort a running shutdown task before stopping (stop only)"),
      skiplock: z.boolean().optional().describe("Ignore locks (start/stop; root only)"),
      wait: waitArg,
    },
    handler: async (a) => {
      const g = await ct(a.vmid, a.node);
      if (g.template && a.action === "start") throw new Error(`Container ${g.vmid} is a template and cannot be started; clone it first`);
      const params: Obj = {};
      if (a.action === "shutdown") Object.assign(params, defined({ timeout: a.timeout, forceStop: a.force_stop }));
      if (a.action === "reboot") Object.assign(params, defined({ timeout: a.timeout }));
      if (a.action === "stop") Object.assign(params, defined({ "overrule-shutdown": a.overrule_shutdown, skiplock: a.skiplock }));
      if (a.action === "start") Object.assign(params, defined({ skiplock: a.skiplock }));
      const upid = await client.post(`${client.guestPath(g)}/status/${a.action}`, params);
      return runTask(client, upid, a.wait, { vmid: g.vmid, node: g.node, action: a.action });
    },
  });

  defineTool(ctx, "proxmox_create_container", {
    toolset: "containers",
    title: "Create a container",
    description:
      "Create an LXC container from an OS template (a vztmpl volume such as local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst; list a storage's vztmpl content to find one). The VMID defaults to the next free ID. The root disk is allocated on rootfs_storage (default size 8 GiB). Without `nets` one interface eth0 on vmbr0 with DHCP is added; pass nets: [] for none. A new interface defaults to ip=dhcp unless ip or ip6 is given. Set password and/or ssh_public_keys for root access. Unprivileged containers are the Proxmox default for new containers; features.nesting is recommended for systemd-based distros. Anything else via `extra` (e.g. dev0, cpulimit; hookscript needs exec permission).",
    write: true,
    destructive: false,
    input: {
      node: nodeArg,
      vmid: z.number().int().min(100).optional().describe("VMID for the new container (default: next free ID)"),
      ostemplate: z.string().describe("OS template volume, e.g. local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst"),
      hostname: z.string().optional().describe("Hostname"),
      password: z.string().optional().describe("Root password inside the container"),
      ssh_public_keys: z.string().optional().describe("Root's authorized SSH public keys (OpenSSH format, one per line)"),
      rootfs_storage: z.string().optional().describe("Storage for the root disk, e.g. local-lvm (default: Proxmox's default storage `local`)"),
      rootfs_size_gb: z.number().positive().optional().describe("Root disk size in GiB (default 8)"),
      mount_points: z.array(mountSpec).optional().describe("Additional mount points, assigned to mp0, mp1... in order"),
      nets: z.array(netSpec).optional().describe("Network interfaces, assigned to net0, net1... in order"),
      cores: z.number().int().positive().optional().describe("CPU cores (default: all host cores)"),
      memory: z.number().int().positive().optional().describe("Memory in MiB (default 512)"),
      swap: z.number().int().min(0).optional().describe("Swap in MiB (default 512)"),
      unprivileged: z.boolean().optional().describe("Run as an unprivileged container (recommended; Proxmox default for new containers)"),
      features: featuresSpec.optional(),
      ostype: z.string().optional().describe("OS type (normally detected from the template), e.g. debian, ubuntu, alpine"),
      arch: z.enum(["amd64", "i386", "arm64", "armhf", "riscv32", "riscv64"]).optional().describe("Architecture (default amd64)"),
      nameserver: z.string().optional().describe("DNS server(s), space-separated (default: copied from the host)"),
      searchdomain: z.string().optional().describe("DNS search domain(s) (default: copied from the host)"),
      timezone: z.string().optional().describe("Time zone, e.g. Europe/London, or \"host\""),
      onboot: z.boolean().optional().describe("Start when the node boots"),
      start: z.boolean().optional().describe("Start the container once created"),
      protection: z.boolean().optional().describe("Protect against removal"),
      pool: z.string().optional().describe("Add to this resource pool"),
      tags: tagsArg,
      description: z.string().optional().describe("Notes shown in the container's summary"),
      wait: waitArg,
      extra: extraArg,
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const vmid = a.vmid ?? (await nextVmid(ctx));
      const params: Obj = {
        vmid,
        ostemplate: a.ostemplate,
        hostname: a.hostname,
        password: a.password,
        "ssh-public-keys": a.ssh_public_keys,
        // Always sent: without it Proxmox allocates 4 GiB on `local` rather than the documented 8 GiB.
        rootfs: `${a.rootfs_storage ?? "local"}:${a.rootfs_size_gb ?? 8}`,
        cores: a.cores,
        memory: a.memory,
        swap: a.swap,
        unprivileged: a.unprivileged,
        features: a.features ? featuresString(a.features) : undefined,
        ostype: a.ostype,
        arch: a.arch,
        nameserver: a.nameserver,
        searchdomain: a.searchdomain,
        timezone: a.timezone,
        onboot: a.onboot,
        start: a.start,
        protection: a.protection,
        pool: a.pool,
        tags: a.tags?.join(";"),
        description: a.description,
      };
      (a.mount_points ?? []).forEach((m, i) => (params[`mp${i}`] = mountString(m, `mp${i}`)));
      (a.nets ?? [{}]).forEach((n, i) => (params[`net${i}`] = netString(n, i)));
      // restore/force/unique turn a create into a backup restore, which with force overwrites an existing container;
      // use proxmox_restore_backup.
      const body = defined(withExtra(params, a.extra, ["restore", "force", "unique", "ignore-unpack-errors"]));
      checkGatedParams(ctx, body);
      const upid = await client.post(`/nodes/${encodeURIComponent(node)}/lxc`, body);
      return runTask(client, upid, a.wait, { vmid, node, hostname: a.hostname });
    },
  });

  defineTool(ctx, "proxmox_clone_container", {
    toolset: "containers",
    title: "Clone a container",
    description:
      "Clone an LXC container or container template to a new VMID (default: next free ID). Templates get a linked clone unless full=true; normal containers are always fully copied (stop the source, or clone from a snapshot with snapname, since a running container can't be copied directly). storage sets the target storage for full clones; target places the clone on another node (shared storage only).",
    write: true,
    destructive: false,
    input: {
      vmid: vmidArg.describe("Source container ID"),
      node: guestNodeArg,
      newid: z.number().int().min(100).optional().describe("VMID for the clone (default: next free ID)"),
      hostname: z.string().optional().describe("Hostname of the clone"),
      description: z.string().optional().describe("Notes for the clone"),
      full: z.boolean().optional().describe("Full copy instead of a linked clone (templates only; default linked unless storage is given)"),
      storage: z.string().optional().describe("Target storage for a full clone"),
      target: z.string().optional().describe("Target node (source must be on shared storage)"),
      pool: z.string().optional().describe("Add the clone to this pool"),
      snapname: z.string().optional().describe("Clone from this snapshot"),
      bwlimit: z.number().positive().optional().describe("I/O bandwidth limit in KiB/s"),
      wait: waitArg,
      extra: extraArg,
    },
    handler: async (a) => {
      const g = await ct(a.vmid, a.node);
      if (a.storage && a.full === false) throw new Error("storage only applies to full clones");
      const newid = a.newid ?? (await nextVmid(ctx));
      const upid = await client.post(
        `${client.guestPath(g)}/clone`,
        defined(withExtra({
          newid,
          hostname: a.hostname,
          description: a.description,
          // Templates default to linked clones, for which Proxmox rejects storage.
          full: a.full ?? (a.storage ? true : undefined),
          storage: a.storage,
          target: a.target,
          pool: a.pool,
          snapname: a.snapname,
          bwlimit: a.bwlimit,
        }, a.extra)),
      );
      return runTask(client, upid, a.wait, { source: g.vmid, newid, node: a.target ?? g.node });
    },
  });

  defineTool(ctx, "proxmox_update_container_config", {
    toolset: "containers",
    title: "Update container configuration",
    description:
      "Change an LXC container's configuration; only the given fields change. `nets` and `mount_points` are keyed by netN/mpN and merged with the existing entry (e.g. {\"net0\": {\"ip\": \"10.0.0.5/24\", \"gw\": \"10.0.0.1\"}} keeps the bridge and MAC); `features` is merged too. New mount points with storage+size_gb are allocated. Memory, swap, cores and network changes usually apply live; others become pending until a reboot (see pending_changes in the result). Use delete to remove keys (removing an mpN volume keeps it as unusedN; deleting an unusedN key destroys the volume and needs delete permission), revert to discard pending changes, extra for anything else. To grow a disk use proxmox_resize_container_disk.",
    write: true,
    destructive: true,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      hostname: z.string().optional().describe("Hostname"),
      cores: z.number().int().positive().optional().describe("CPU cores"),
      cpulimit: z.number().min(0).optional().describe("CPU time limit (0 = unlimited; 2 = two cores' worth)"),
      cpuunits: z.number().int().min(0).optional().describe("CPU weight relative to other guests"),
      memory: z.number().int().positive().optional().describe("Memory in MiB"),
      swap: z.number().int().min(0).optional().describe("Swap in MiB"),
      nets: z.record(z.string().regex(/^net\d+$/), netSpec).optional().describe("Network interfaces to add or change, keyed by net0, net1..."),
      mount_points: z.record(z.string().regex(/^mp\d+$/), mountSpec).optional().describe("Mount points to add or change, keyed by mp0, mp1..."),
      features: featuresSpec.optional(),
      nameserver: z.string().optional().describe("DNS server(s)"),
      searchdomain: z.string().optional().describe("DNS search domain(s)"),
      timezone: z.string().optional().describe("Time zone or \"host\""),
      onboot: z.boolean().optional().describe("Start when the node boots"),
      startup: z.string().optional().describe("Startup order/delays, e.g. \"order=2,up=30\""),
      protection: z.boolean().optional().describe("Protect against removal"),
      tags: tagsArg,
      description: z.string().optional().describe("Notes"),
      delete: deleteKeysArg,
      revert: z.array(z.string()).optional().describe("Pending changes to discard, by key"),
      digest: digestArg,
      extra: extraArg,
    },
    handler: async (a) => {
      const g = await ct(a.vmid, a.node);
      const base = client.guestPath(g);
      const needCurrent = a.nets || a.mount_points || a.features;
      const current: Obj = needCurrent ? await client.get<Obj>(`${base}/config`) : {};
      const typed: Obj = {
        hostname: a.hostname,
        cores: a.cores,
        cpulimit: a.cpulimit,
        cpuunits: a.cpuunits,
        memory: a.memory,
        swap: a.swap,
        features: a.features ? featuresString(a.features, current.features) : undefined,
        nameserver: a.nameserver,
        searchdomain: a.searchdomain,
        timezone: a.timezone,
        onboot: a.onboot,
        startup: a.startup,
        protection: a.protection,
        tags: a.tags?.join(";"),
        description: a.description,
        delete: deleteParam(a.delete),
        revert: deleteParam(a.revert),
        digest: a.digest,
      };
      for (const [key, spec] of Object.entries(a.nets ?? {})) typed[key] = netString(spec, Number(key.slice(3)), current[key]);
      for (const [key, spec] of Object.entries(a.mount_points ?? {})) typed[key] = mountString(spec, key, current[key]);
      const params = defined(withExtra(typed, a.extra));
      if (!Object.keys(params).filter((k) => k !== "digest").length) throw new Error("Nothing to change");
      checkGatedParams(ctx, params);
      await client.put(`${base}/config`, params);
      const pending = await client.get<Obj[]>(`${base}/pending`).catch(() => [] as Obj[]);
      return compact({ vmid: g.vmid, node: g.node, updated: Object.keys(params).filter((k) => k !== "digest"), pending_changes: pendingChanges(pending) });
    },
  });

  defineTool(ctx, "proxmox_resize_container_disk", {
    toolset: "containers",
    title: "Resize a container disk",
    description:
      "Grow a container's rootfs or mount point volume. size is absolute (\"20G\") or relative (\"+5G\"). Shrinking is not supported. The filesystem is grown automatically, also while running.",
    write: true,
    destructive: false,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      disk: volumeKeyArg,
      size: z.string().regex(/^\+?\d+(\.\d+)?[KMGT]?$/, "e.g. 20G or +5G").describe("New size, e.g. \"20G\", or an increase such as \"+5G\""),
      digest: digestArg,
      wait: waitArg,
    },
    handler: async (a) => {
      const g = await ct(a.vmid, a.node);
      const upid = await client.put(`${client.guestPath(g)}/resize`, defined({ disk: a.disk, size: a.size, digest: a.digest }));
      return runTask(client, upid, a.wait, { vmid: g.vmid, node: g.node, disk: a.disk, size: a.size });
    },
  });

  defineTool(ctx, "proxmox_move_container_volume", {
    toolset: "containers",
    title: "Move a container volume",
    description:
      "Move a container's rootfs or mount point to another storage (storage), or reassign a volume to another container (target_vmid, optional target_volume). The container usually must be stopped to move rootfs. By default the original is kept as an unusedN entry; delete_source=true removes it after a successful copy.",
    write: true,
    destructive: true,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      volume: z.string().regex(/^(rootfs|mp\d+|unused\d+)$/).describe("Volume to move: rootfs, mpN or unusedN"),
      storage: z.string().optional().describe("Target storage"),
      target_vmid: z.number().int().min(100).optional().describe("Reassign the volume to this container instead"),
      target_volume: z.string().optional().describe("Config key on the target container (default: same key)"),
      delete_source: z.boolean().optional().describe("Delete the original volume after copying (default false: kept as unused)"),
      bwlimit: z.number().positive().optional().describe("I/O bandwidth limit in KiB/s"),
      digest: digestArg,
      target_digest: z.string().optional().describe("Only proceed if the target container's config digest matches"),
      wait: waitArg,
    },
    handler: async (a) => {
      if (!a.storage && a.target_vmid === undefined) throw new Error("Give storage (move to storage) or target_vmid (reassign to container)");
      const g = await ct(a.vmid, a.node);
      const upid = await client.post(
        `${client.guestPath(g)}/move_volume`,
        defined({
          volume: a.volume,
          storage: a.storage,
          "target-vmid": a.target_vmid,
          "target-volume": a.target_volume,
          delete: a.delete_source,
          bwlimit: a.bwlimit,
          digest: a.digest,
          "target-digest": a.target_digest,
        }),
      );
      return runTask(client, upid, a.wait, compact({ vmid: g.vmid, node: g.node, volume: a.volume, storage: a.storage, target_vmid: a.target_vmid }));
    },
  });

  defineTool(ctx, "proxmox_migrate_container", {
    toolset: "containers",
    title: "Migrate a container",
    description:
      "Move an LXC container to another node. Containers can't be live-migrated: a running container needs restart=true (it is shut down, moved and started again on the target, with brief downtime). Stopped containers move offline. Check first with proxmox_check_container_migration. target_storage maps local volumes to storage on the target.",
    write: true,
    destructive: true,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      target: z.string().describe("Target node"),
      restart: z.boolean().optional().describe("Restart migration for a running container (required if running)"),
      timeout: z.number().int().positive().optional().describe("Shutdown timeout in seconds for restart migration (default 180)"),
      target_storage: z.string().optional().describe("Target storage, or a mapping like \"local-lvm:fast,local:local\""),
      bwlimit: z.number().positive().optional().describe("I/O bandwidth limit in KiB/s"),
      wait: waitArg,
    },
    handler: async (a) => {
      const g = await ct(a.vmid, a.node);
      if (g.node === a.target) throw new Error(`Container ${g.vmid} is already on ${a.target}`);
      if (g.status === "running" && !a.restart) {
        throw new Error(`Container ${g.vmid} is running; LXC can't live-migrate. Pass restart=true for a restart migration (brief downtime), or stop it first.`);
      }
      const upid = await client.post(
        `${client.guestPath(g)}/migrate`,
        defined({ target: a.target, restart: a.restart, timeout: a.timeout, "target-storage": a.target_storage, bwlimit: a.bwlimit }),
      );
      return runTask(client, upid, a.wait, { vmid: g.vmid, from: g.node, to: a.target });
    },
  });

  defineTool(ctx, "proxmox_convert_container_to_template", {
    toolset: "containers",
    title: "Convert a container to a template",
    description:
      "Turn a stopped LXC container into a template for proxmox_clone_container. This cannot be undone: templates can't be started or edited like normal containers.",
    write: true,
    destructive: true,
    input: { vmid: vmidArg, node: guestNodeArg },
    handler: async (a) => {
      const g = await ct(a.vmid, a.node);
      if (g.template) throw new Error(`Container ${g.vmid} is already a template`);
      if (g.status === "running") throw new Error(`Container ${g.vmid} is running; shut it down first (proxmox_container_power)`);
      const res = await client.post(`${client.guestPath(g)}/template`);
      return runTask(client, res, true, { vmid: g.vmid, node: g.node, template: true });
    },
  });

  defineTool(ctx, "proxmox_delete_container", {
    toolset: "containers",
    title: "Delete a container",
    description:
      "Permanently destroy an LXC container and all its volumes. It must be stopped unless force=true. purge also removes it from backup jobs, replication and HA. Cannot be undone; consider a backup first.",
    delete: true,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      purge: z.boolean().optional().describe("Also remove from backup jobs, replication jobs and HA"),
      destroy_unreferenced_disks: z.boolean().optional().describe("Also destroy disks with this VMID on any storage that aren't in the config"),
      force: z.boolean().optional().describe("Destroy even if running"),
      wait: waitArg,
    },
    handler: async (a) => {
      const g = await ct(a.vmid, a.node);
      if (g.status === "running" && !a.force) throw new Error(`Container ${g.vmid} is running; stop it first or pass force=true`);
      const upid = await client.delete(
        client.guestPath(g),
        defined({ purge: a.purge, "destroy-unreferenced-disks": a.destroy_unreferenced_disks, force: a.force }),
      );
      return runTask(client, upid, a.wait, { vmid: g.vmid, node: g.node, name: g.name, deleted: true });
    },
  });
}
