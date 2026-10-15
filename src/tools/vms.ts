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

// ---------------------------------------------------------------- shared arguments

const DISK_RE = /^(ide[0-3]|sata[0-5]|scsi([0-9]|[12][0-9]|30)|virtio([0-9]|1[0-5])|efidisk0|tpmstate0)$/;
const diskKeyArg = z.string().regex(DISK_RE, "a disk key such as scsi0, virtio1, sata0, ide0, efidisk0 or tpmstate0").describe("Disk key, e.g. scsi0, virtio1, sata0");
const skiplockArg = z.boolean().optional().describe("Ignore a config lock (root@pam only; use with care)");
const OSTYPES = ["other", "wxp", "w2k", "w2k3", "w2k8", "wvista", "win7", "win8", "win10", "win11", "l24", "l26", "solaris"] as const;
const SCSIHW = ["lsi", "lsi53c810", "virtio-scsi-pci", "virtio-scsi-single", "megasas", "pvscsi"] as const;
const NIC_MODELS = ["virtio", "e1000", "e1000e", "rtl8139", "vmxnet3"] as const;
const optionsArg = z
  .record(z.string(), z.union([z.string(), z.number(), z.boolean()]))
  .optional()
  .describe("Extra property-string options, e.g. {discard: \"on\", ssd: true, iothread: true}");

/** Config fields that create and update share; mapped to API params by commonParams(). */
const commonFields = {
  name: z.string().optional().describe("VM name (DNS-style: letters, digits, '-' and '.')"),
  memory_mb: z.number().int().positive().optional().describe("Memory in MiB, e.g. 4096"),
  balloon_mb: z.number().int().min(0).optional().describe("Minimum memory for the balloon driver in MiB (0 disables ballooning)"),
  cores: z.number().int().positive().optional().describe("CPU cores per socket"),
  sockets: z.number().int().positive().optional().describe("CPU sockets"),
  cpu: z.string().optional().describe("CPU type, e.g. host, x86-64-v2-AES, kvm64 (or a full cpu property string)"),
  numa: z.boolean().optional().describe("Enable NUMA"),
  ostype: z.enum(OSTYPES).optional().describe("Guest OS type: l26 = Linux 2.6+, win11/win10..., other"),
  scsihw: z.enum(SCSIHW).optional().describe("SCSI controller model (virtio-scsi-single recommended)"),
  bios: z.enum(["seabios", "ovmf"]).optional().describe("Firmware: seabios (legacy BIOS) or ovmf (UEFI; needs an EFI disk)"),
  machine: z.string().optional().describe("QEMU machine type, e.g. q35, pc, pc-q35-9.0"),
  vga: z.string().optional().describe("Display, e.g. std, virtio, qxl, serial0, none"),
  agent: z
    .union([z.boolean(), z.string()])
    .optional()
    .describe("QEMU guest agent: true/false, or a property string like 'enabled=1,fstrim_cloned_disks=1'"),
  onboot: z.boolean().optional().describe("Start the VM when the node boots"),
  startup: z.string().optional().describe("Startup/shutdown order and delays, e.g. 'order=2,up=30,down=60'"),
  protection: z.boolean().optional().describe("Protect the VM and its disks from removal"),
  tags: z.array(z.string()).optional().describe("Tags (replaces all existing tags)"),
  description: z.string().optional().describe("Notes shown in the VM summary (Markdown)"),
  boot_order: z.array(z.string()).optional().describe("Boot device order, e.g. [\"scsi0\", \"ide2\", \"net0\"]"),
  hotplug: z.string().optional().describe("Hotplug features, e.g. 'network,disk,usb' or '0' to disable"),
  ciuser: z.string().optional().describe("cloud-init: user to create/configure"),
  cipassword: z.string().optional().describe("cloud-init: password for the user (prefer sshkeys)"),
  sshkeys: z
    .union([z.string(), z.array(z.string())])
    .optional()
    .describe("cloud-init: public SSH keys (OpenSSH format, one per entry or newline-separated). URL-encoded for Proxmox automatically."),
  ipconfig: z
    .record(z.string().regex(/^\d+$/), z.string())
    .optional()
    .describe("cloud-init IP config per NIC index, e.g. {\"0\": \"ip=dhcp\", \"1\": \"ip=10.0.0.5/24,gw=10.0.0.1\"} (ip6=auto/dhcp also allowed)"),
  nameserver: z.string().optional().describe("cloud-init: DNS servers (space-separated)"),
  searchdomain: z.string().optional().describe("cloud-init: DNS search domains"),
  citype: z.enum(["configdrive2", "nocloud", "opennebula"]).optional().describe("cloud-init data format (default depends on ostype)"),
  ciupgrade: z.boolean().optional().describe("cloud-init: upgrade packages on first boot (default true)"),
  cicustom: z.string().optional().describe("cloud-init: custom snippet files, e.g. 'user=local:snippets/user.yaml'"),
};

type CommonArgs = { [K in keyof typeof commonFields]?: z.infer<(typeof commonFields)[K]> };

function commonParams(a: CommonArgs): Obj {
  const ipconfig: Obj = {};
  for (const [i, v] of Object.entries(a.ipconfig ?? {})) ipconfig[`ipconfig${i}`] = v;
  return defined({
    name: a.name,
    memory: a.memory_mb,
    balloon: a.balloon_mb,
    cores: a.cores,
    sockets: a.sockets,
    cpu: a.cpu,
    numa: a.numa,
    ostype: a.ostype,
    scsihw: a.scsihw,
    bios: a.bios,
    machine: a.machine,
    vga: a.vga,
    agent: typeof a.agent === "boolean" ? (a.agent ? "1" : "0") : a.agent,
    onboot: a.onboot,
    startup: a.startup,
    protection: a.protection,
    tags: a.tags ? a.tags.join(";") : undefined,
    description: a.description,
    boot: a.boot_order ? `order=${a.boot_order.join(";")}` : undefined,
    hotplug: a.hotplug,
    ciuser: a.ciuser,
    cipassword: a.cipassword,
    sshkeys: encodeSshKeys(a.sshkeys),
    nameserver: a.nameserver,
    searchdomain: a.searchdomain,
    citype: a.citype,
    ciupgrade: a.ciupgrade,
    cicustom: a.cicustom,
    ...ipconfig,
  });
}

/** Proxmox wants sshkeys URL-encoded (and rejects '+' for spaces), one key per line. */
function encodeSshKeys(keys: string | string[] | undefined): string | undefined {
  if (keys === undefined) return undefined;
  const text = (Array.isArray(keys) ? keys.join("\n") : keys).trim();
  return encodeURIComponent(text);
}

/**
 * Guards for options that reach beyond a plain config write: deleting an unusedN key
 * destroys that volume, and a hookscript runs as root on the node at every lifecycle event.
 */
function checkGatedParams(ctx: ToolContext, params: Obj): void {
  if (!ctx.allowDeletes && /(^|[,;\s])unused\d+/.test(String(params.delete ?? ""))) {
    throw new Error("Deleting an unusedN entry permanently destroys its volume and needs delete permission (PROXMOX_ALLOW_DELETES); see proxmox_unlink_vm_disk");
  }
  if (!ctx.allowExec && params.hookscript !== undefined) {
    throw new Error("hookscript runs a script as root on the node, so setting it needs exec permission (PROXMOX_ALLOW_EXEC)");
  }
}

// ---------------------------------------------------------------- summaries

/** Redacts a single config value by its key (e.g. cipassword). */
function redactValue(key: string, value: unknown): unknown {
  if (value === undefined) return undefined;
  return redactSecrets({ [key]: value })[key];
}

function splitTags(tags: unknown): string[] | undefined {
  return tags ? String(tags).split(/[;,\s]+/).filter(Boolean) : undefined;
}

/** Key type and comment of each SSH key, so the model sees which keys are set without the key material. */
function describeSshKeys(value: unknown): string[] | undefined {
  if (typeof value !== "string" || !value) return undefined;
  let text = value;
  try {
    text = decodeURIComponent(value);
  } catch {
    /* keep as-is */
  }
  return text
    .split(/\r?\n/)
    .map((l) => l.trim())
    .filter(Boolean)
    .map((l) => {
      const parts = l.split(/\s+/);
      return [parts[0], parts.slice(2).join(" ")].filter(Boolean).join(" ");
    });
}

const DEVICE_KEY_RE = /^(hostpci|usb|serial|parallel|audio|rng|virtiofs|watchdog|ivshmem|numa)\d*$/;

export function summarizeVmConfig(c: Obj): Obj {
  const mem = parsePropertyString(c.memory, "current");
  const ipconfig: Obj = {};
  for (const [k, v] of Object.entries(c)) if (/^ipconfig\d+$/.test(k)) ipconfig[k] = v;
  const devices: Obj = {};
  for (const [k, v] of Object.entries(c)) if (DEVICE_KEY_RE.test(k)) devices[k] = v;
  const disks = summarizeDisks(c);
  const hasCloudInit = Object.values(disks).some((d) => /cloudinit/.test(d.file ?? ""));
  const boot = typeof c.boot === "string" ? parsePropertyString(c.boot, "legacy") : {};
  return compact({
    name: c.name,
    template: flag(c.template) ? true : undefined,
    lock: c.lock,
    ostype: c.ostype,
    cpu: compact({
      sockets: Number(c.sockets ?? 1),
      cores: Number(c.cores ?? 1),
      vcpus: c.vcpus,
      type: c.cpu,
      limit: c.cpulimit,
      units: c.cpuunits,
      affinity: c.affinity,
      numa: flag(c.numa) || undefined,
    }),
    memory: compact({
      size: mem.current ? formatBytes(Number(mem.current) * 1024 * 1024) : undefined,
      balloon_min: c.balloon !== undefined ? (Number(c.balloon) === 0 ? "disabled" : formatBytes(Number(c.balloon) * 1024 * 1024)) : undefined,
      hugepages: c.hugepages,
      shares: c.shares,
    }),
    bios: c.bios,
    machine: c.machine,
    scsihw: c.scsihw,
    vga: c.vga,
    boot_order: boot.order ? boot.order.split(";") : c.bootdisk ? [c.bootdisk] : undefined,
    disks,
    nets: summarizeNets(c),
    devices,
    agent: c.agent !== undefined ? c.agent : undefined,
    onboot: flag(c.onboot),
    startup: c.startup,
    protection: flag(c.protection) || undefined,
    hotplug: c.hotplug,
    cloud_init:
      hasCloudInit || c.ciuser || c.cipassword || c.sshkeys || c.cicustom || Object.keys(ipconfig).length
        ? compact({
            drive: hasCloudInit ? true : undefined,
            type: c.citype,
            user: c.ciuser,
            password_set: c.cipassword ? true : undefined,
            ssh_keys: describeSshKeys(c.sshkeys),
            ipconfig,
            nameserver: c.nameserver,
            searchdomain: c.searchdomain,
            upgrade: flag(c.ciupgrade),
            custom: c.cicustom,
          })
        : undefined,
    tags: splitTags(c.tags),
    description: c.description,
    digest: c.digest,
  });
}

export function summarizeVmStatus(s: Obj): Obj {
  const running = s.status === "running";
  const pressure = compact({
    cpu_some: s.pressurecpusome || undefined,
    cpu_full: s.pressurecpufull || undefined,
    io_some: s.pressureiosome || undefined,
    io_full: s.pressureiofull || undefined,
    memory_some: s.pressurememorysome || undefined,
    memory_full: s.pressurememoryfull || undefined,
  });
  return compact({
    status: s.status,
    qmpstatus: s.qmpstatus && s.qmpstatus !== s.status ? s.qmpstatus : undefined,
    lock: s.lock,
    template: flag(s.template) || undefined,
    uptime: running ? formatUptime(s.uptime) : undefined,
    cpus: s.cpus,
    cpu_pct: running ? fractionPct(s.cpu) : undefined,
    memory: running ? usage(s.mem, s.maxmem) : formatBytes(s.maxmem),
    host_memory: running ? formatBytes(s.memhost) : undefined,
    balloon_target: running && s.balloon && Number(s.balloon) !== Number(s.maxmem) ? formatBytes(s.balloon) : undefined,
    disk_read: running ? formatBytes(s.diskread) : undefined,
    disk_write: running ? formatBytes(s.diskwrite) : undefined,
    net_in: running ? formatBytes(s.netin) : undefined,
    net_out: running ? formatBytes(s.netout) : undefined,
    pressure_pct: running ? pressure : undefined,
    pid: s.pid,
    running_qemu: s["running-qemu"],
    running_machine: s["running-machine"],
    agent_enabled: flag(s.agent),
    ha: s.ha?.managed ? compact({ state: s.ha.state, group: s.ha.group }) : undefined,
    tags: splitTags(s.tags),
  });
}

/** Entries of /pending (or /cloudinit) that differ from the running config, with secrets redacted. */
function pendingChanges(entries: Obj[]): Obj[] {
  return entries
    .filter((e) => e.key !== "digest" && (e.pending !== undefined || e.delete))
    .map((e) =>
      compact({
        key: e.key,
        current: redactValue(e.key, e.value),
        pending: redactValue(e.key, e.pending),
        delete: e.delete ? (Number(e.delete) === 2 ? "force" : true) : undefined,
      }),
    );
}

// ---------------------------------------------------------------- RRD statistics

interface Metric {
  get: (p: Obj) => number | undefined;
  fmt: (n: number) => unknown;
}

const num = (v: unknown): number | undefined => {
  const n = Number(v);
  return v === undefined || v === null || v === "" || !Number.isFinite(n) ? undefined : n;
};
const rate = (n: number) => `${formatBytes(n)}/s`;
const round1 = (n: number) => Math.round(n * 10) / 10;

const METRICS: Record<string, Metric> = {
  cpu_pct: { get: (p) => (num(p.cpu) === undefined ? undefined : num(p.cpu)! * 100), fmt: round1 },
  memory: { get: (p) => num(p.mem), fmt: (n) => formatBytes(Math.round(n)) },
  host_memory: { get: (p) => num(p.memhost), fmt: (n) => formatBytes(Math.round(n)) },
  net_in: { get: (p) => num(p.netin), fmt: rate },
  net_out: { get: (p) => num(p.netout), fmt: rate },
  disk_read: { get: (p) => num(p.diskread), fmt: rate },
  disk_write: { get: (p) => num(p.diskwrite), fmt: rate },
  pressure_cpu_some_pct: { get: (p) => num(p.pressurecpusome), fmt: round1 },
  pressure_io_some_pct: { get: (p) => num(p.pressureiosome), fmt: round1 },
  pressure_memory_some_pct: { get: (p) => num(p.pressurememorysome), fmt: round1 },
};

export function summarizeRrd(points: Obj[], seriesPoints: number): Obj {
  const pts = points.filter((p) => num(p.time) !== undefined).sort((x, y) => Number(x.time) - Number(y.time));
  const summary: Obj = {};
  for (const [name, m] of Object.entries(METRICS)) {
    const vals = pts.map(m.get).filter((v): v is number => v !== undefined);
    if (!vals.length) continue;
    const sum = vals.reduce((s, v) => s + v, 0);
    summary[name] = { min: m.fmt(Math.min(...vals)), avg: m.fmt(sum / vals.length), max: m.fmt(Math.max(...vals)) };
  }
  const maxmem = pts.map((p) => num(p.maxmem)).filter((v) => v !== undefined).at(-1);
  if (summary.memory && maxmem) summary.memory.of = formatBytes(maxmem);
  const series: Obj[] = [];
  if (seriesPoints > 0 && pts.length) {
    const size = Math.ceil(pts.length / seriesPoints);
    for (let i = 0; i < pts.length; i += size) {
      const bucket = pts.slice(i, i + size);
      const row: Obj = { time: isoTime(bucket[0].time) };
      for (const [name, m] of Object.entries(METRICS)) {
        if (!summary[name]) continue;
        const vals = bucket.map(m.get).filter((v): v is number => v !== undefined);
        if (vals.length) row[name] = m.fmt(vals.reduce((s, v) => s + v, 0) / vals.length);
      }
      series.push(row);
    }
  }
  return compact({
    from: isoTime(pts[0]?.time),
    to: isoTime(pts.at(-1)?.time),
    samples: pts.length,
    summary,
    series,
  });
}

// ---------------------------------------------------------------- sendkey text conversion

const SHIFTED: Record<string, string> = {
  "!": "1", "@": "2", "#": "3", $: "4", "%": "5", "^": "6", "&": "7", "*": "8", "(": "9", ")": "0",
  _: "minus", "+": "equal", "{": "bracket_left", "}": "bracket_right", "|": "backslash", ":": "semicolon",
  '"': "apostrophe", "<": "comma", ">": "dot", "?": "slash", "~": "grave_accent",
};
const PLAIN: Record<string, string> = {
  " ": "spc", "-": "minus", "=": "equal", "[": "bracket_left", "]": "bracket_right", "\\": "backslash", ";": "semicolon",
  "'": "apostrophe", ",": "comma", ".": "dot", "/": "slash", "`": "grave_accent", "\n": "ret", "\t": "tab",
};

/** Turns text into QEMU sendkey names for a US keyboard layout. */
export function textToKeys(text: string): string[] {
  const keys: string[] = [];
  for (const ch of text) {
    if (/^[a-z0-9]$/.test(ch)) keys.push(ch);
    else if (/^[A-Z]$/.test(ch)) keys.push(`shift-${ch.toLowerCase()}`);
    else if (PLAIN[ch]) keys.push(PLAIN[ch]);
    else if (SHIFTED[ch]) keys.push(`shift-${SHIFTED[ch]}`);
    else if (ch === "\r") continue;
    else throw new Error(`Cannot type character ${JSON.stringify(ch)} with sendkey (US layout, ASCII only)`);
  }
  return keys;
}

// ---------------------------------------------------------------- disk spec building for create

const diskSpec = z.object({
  bus: z.enum(["scsi", "virtio", "sata", "ide"]).optional().describe("Controller bus (default scsi)"),
  index: z.number().int().min(0).max(30).optional().describe("Slot number on the bus (default: next free)"),
  storage: z.string().optional().describe("Storage to allocate on (default: the top-level `storage`)"),
  size_gb: z.number().positive().optional().describe("Size in GiB (required unless import_from or spec is given)"),
  import_from: z.string().optional().describe("Import an existing image as this disk, e.g. 'local:import/debian-12.qcow2' or a volume ID"),
  format: z.enum(["raw", "qcow2", "vmdk"]).optional().describe("Image format on file-based storage"),
  options: optionsArg,
  spec: z.string().optional().describe("Raw Proxmox disk property string (overrides storage/size/options), e.g. 'local-lvm:32,ssd=1'"),
});

const netSpec = z.object({
  model: z.enum(NIC_MODELS).optional().describe("NIC model (default virtio)"),
  bridge: z.string().optional().describe("Bridge (default vmbr0)"),
  vlan: z.number().int().min(1).max(4094).optional().describe("VLAN tag"),
  firewall: z.boolean().optional().describe("Enable the Proxmox firewall on this NIC"),
  macaddr: z.string().optional().describe("MAC address (default: generated)"),
  options: optionsArg,
  spec: z.string().optional().describe("Raw Proxmox net property string (overrides the other fields), e.g. 'virtio,bridge=vmbr1,tag=20'"),
});

const BUS_MAX: Record<string, number> = { scsi: 30, virtio: 15, sata: 5, ide: 3 };

class SlotAllocator {
  private used = new Set<string>();
  take(key: string): string {
    if (this.used.has(key)) throw new Error(`Disk slot ${key} is used twice`);
    if (!DISK_RE.test(key)) throw new Error(`Invalid disk slot ${key}`);
    this.used.add(key);
    return key;
  }
  next(bus: string, prefer: number[] = []): string {
    for (const i of [...prefer, ...Array.from({ length: BUS_MAX[bus] + 1 }, (_, n) => n)]) {
      if (!this.used.has(`${bus}${i}`)) return this.take(`${bus}${i}`);
    }
    throw new Error(`No free ${bus} slot left`);
  }
}

function buildDisk(d: z.infer<typeof diskSpec>, defaultStorage: string | undefined): string {
  if (d.spec) return d.spec;
  const storage = d.storage ?? defaultStorage;
  if (!storage) throw new Error("Each disk needs a storage (or set the top-level `storage`), or a raw `spec`");
  if (!d.import_from && d.size_gb === undefined) throw new Error("Each disk needs size_gb (or import_from / spec)");
  return toPropertyString(
    { _default: `${storage}:${d.import_from ? 0 : d.size_gb}`, "import-from": d.import_from, format: d.format, ...d.options },
    "_default",
  );
}

function buildNet(n: z.infer<typeof netSpec>): string {
  if (n.spec) return n.spec;
  return toPropertyString(
    {
      _default: n.macaddr ? `${n.model ?? "virtio"}=${n.macaddr}` : (n.model ?? "virtio"),
      bridge: n.bridge ?? "vmbr0",
      tag: n.vlan,
      firewall: n.firewall,
      ...n.options,
    },
    "_default",
  );
}

// ---------------------------------------------------------------- tools

export function registerVmsTools(ctx: ToolContext): void {
  const { client } = ctx;
  const vm = (a: { vmid: number; node?: string }) => client.resolveGuest(a.vmid, { node: a.node, type: "qemu" });
  const nextId = async () => Number(await client.get("/cluster/nextid"));
  const ref = (g: GuestRef) => compact({ vmid: g.vmid, name: g.name, node: g.node });

  // ---------------------------------------------------------------- read

  defineTool(ctx, "proxmox_list_vms", {
    toolset: "vms",
    title: "List VMs with live details",
    description:
      "List QEMU VMs straight from the nodes (GET /nodes/{node}/qemu?full=1), with details proxmox_list_guests lacks: disk I/O counters, host memory, PSI pressure stalls and the running QEMU version/machine. Without `node`, queries every online node. For a quick cluster-wide list with filters by tag/pool use proxmox_list_guests instead.",
    input: {
      node: z.string().optional().describe("Only this node (default: all online nodes)"),
      status: z.enum(["running", "stopped"]).optional().describe("Only VMs in this state"),
      search: searchArg,
      limit: limitArg(500),
      raw: rawArg,
    },
    handler: async (a) => {
      const nodes = a.node ? [a.node] : (await client.listNodes()).filter((n) => n.status === "online").map((n) => n.node);
      const errors: Obj[] = [];
      const lists = await Promise.all(
        nodes.map((node) =>
          client
            .get<Obj[]>(`/nodes/${encodeURIComponent(node)}/qemu`, { full: 1 })
            .then((vms) => vms.map((v): Obj => ({ ...v, node })))
            .catch((e: Error) => {
              errors.push({ node, error: e.message });
              return [] as Obj[];
            }),
        ),
      );
      let vms = lists.flat();
      if (a.status) vms = vms.filter((v) => v.status === a.status);
      vms.sort((x, y) => Number(x.vmid) - Number(y.vmid));
      const res = filterAndLimit(vms, { search: a.search, limit: a.limit ?? 500 }, (v) => [v.vmid, v.name, v.tags, v.node]);
      const items = a.raw
        ? redactSecrets(res.items)
        : res.items.map((v) => compact({ vmid: Number(v.vmid), name: v.name, node: v.node, ...summarizeVmStatus(v) }));
      return compact({ ...res, items, errors });
    },
  });

  defineTool(ctx, "proxmox_get_vm", {
    toolset: "vms",
    title: "Get VM details",
    description:
      "Get a VM's live status (run state, CPU, memory, disk/network I/O, uptime, HA, lock, guest agent, QEMU version) together with a summary of its configuration (CPU, memory, disks, NICs, boot order, firmware, cloud-init, tags, notes) and any pending changes that need a restart. Use proxmox_get_vm_config for the raw config.",
    input: { vmid: vmidArg, node: guestNodeArg, raw: rawArg },
    handler: async (a) => {
      const g = await vm(a);
      const base = client.guestPath(g);
      const [status, config, pending] = await Promise.all([
        client.get<Obj>(`${base}/status/current`),
        client.get<Obj>(`${base}/config`, { current: 1 }),
        client.get<Obj[]>(`${base}/pending`).catch(() => [] as Obj[]),
      ]);
      if (a.raw) return redactSecrets({ vmid: g.vmid, node: g.node, status, config, pending });
      const changes = pendingChanges(pending);
      return compact({
        vmid: g.vmid,
        node: g.node,
        ...summarizeVmStatus(status),
        config: summarizeVmConfig(config),
        pending_changes: changes,
        note: changes.length ? "Pending changes apply at the next VM restart (proxmox_vm_power action=reboot) or can be reverted with proxmox_update_vm_config revert=[...]" : undefined,
      });
    },
  });

  defineTool(ctx, "proxmox_get_vm_config", {
    toolset: "vms",
    title: "Get VM configuration",
    description:
      "Get a VM's configuration as Proxmox stores it (key → value, secrets redacted). By default pending changes are shown applied; current=true shows the running values instead, pending=true lists every key with its current and pending value, and snapshot reads the config saved in a snapshot. The `digest` can be passed to proxmox_update_vm_config for safe concurrent edits.",
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      current: z.boolean().optional().describe("Show the current (running) values instead of values with pending changes applied"),
      pending: z.boolean().optional().describe("List each key with its current value, pending value and pending deletion"),
      snapshot: z.string().optional().describe("Read the configuration stored in this snapshot"),
    },
    handler: async (a) => {
      const g = await vm(a);
      const base = client.guestPath(g);
      if (a.pending) {
        const entries = await client.get<Obj[]>(`${base}/pending`);
        return {
          vmid: g.vmid,
          node: g.node,
          entries: entries.map((e) =>
            compact({
              key: e.key,
              value: redactValue(e.key, e.value),
              pending: redactValue(e.key, e.pending),
              delete: e.delete ? (Number(e.delete) === 2 ? "force" : true) : undefined,
            }),
          ),
        };
      }
      const config = await client.get<Obj>(`${base}/config`, defined({ current: a.current, snapshot: a.snapshot }));
      const sorted = Object.fromEntries(Object.entries(config).sort(([x], [y]) => x.localeCompare(y, "en", { numeric: true })));
      return { vmid: g.vmid, node: g.node, config: redactSecrets(sorted) };
    },
  });

  defineTool(ctx, "proxmox_get_vm_stats", {
    toolset: "vms",
    title: "Get VM performance history",
    description:
      "Get a VM's performance history from Proxmox's RRD database: min/avg/max of CPU %, memory, network and disk throughput and pressure stalls over the last hour/day/week/month/year, plus a downsampled time series. Use for 'was it busy?', trend and capacity questions.",
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      timeframe: z.enum(["hour", "day", "week", "month", "year"]).optional().describe("Time window (default hour)"),
      cf: z.enum(["AVERAGE", "MAX"]).optional().describe("Consolidation function for each data point (default AVERAGE)"),
      points: z.number().int().min(0).max(200).optional().describe("Number of points in the returned series (default 12; 0 = summary only)"),
      raw: rawArg,
    },
    handler: async (a) => {
      const g = await vm(a);
      const timeframe = a.timeframe ?? "hour";
      const cf = a.cf ?? "AVERAGE";
      const data = await client.get<Obj[]>(`${client.guestPath(g)}/rrddata`, { timeframe, cf });
      if (a.raw) return { vmid: g.vmid, node: g.node, timeframe, cf, data };
      return { vmid: g.vmid, node: g.node, timeframe, cf, ...summarizeRrd(data, a.points ?? 12) };
    },
  });

  defineTool(ctx, "proxmox_get_vm_cloudinit", {
    toolset: "vms",
    title: "Get VM cloud-init data",
    description:
      "Show a VM's cloud-init settings with current vs pending values (pending ones need proxmox_regenerate_vm_cloudinit or a restart to reach the guest), plus the generated user-data, network-config and meta-data as the guest will see them. Passwords are redacted.",
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      dump: z
        .array(z.enum(["user", "network", "meta"]))
        .optional()
        .describe("Which generated documents to include (default all three; [] for none)"),
    },
    handler: async (a) => {
      const g = await vm(a);
      const base = client.guestPath(g);
      const types = a.dump ?? ["user", "network", "meta"];
      const [entries, ...docs] = await Promise.all([
        client.get<Obj[]>(`${base}/cloudinit`),
        ...types.map((type) =>
          client
            .get<string>(`${base}/cloudinit/dump`, { type })
            .then((text) => redactCloudInitDoc(String(text ?? "")))
            .catch((e: Error) => `Error: ${e.message}`),
        ),
      ]);
      const settings = entries
        .filter((e) => e.key !== "digest")
        .map((e) =>
          compact({
            key: e.key,
            value: e.key === "sshkeys" ? describeSshKeys(e.value) : redactValue(e.key, e.value),
            pending: e.key === "sshkeys" ? describeSshKeys(e.pending) : redactValue(e.key, e.pending),
            delete: e.delete ? true : undefined,
          }),
        );
      const pending = settings.filter((s) => s.pending !== undefined || s.delete).map((s) => s.key);
      return compact({
        vmid: g.vmid,
        node: g.node,
        settings,
        pending_keys: pending,
        note: pending.length ? "Some values are pending: run proxmox_regenerate_vm_cloudinit (or restart the VM) to rebuild the cloud-init drive" : undefined,
        generated: Object.fromEntries(types.map((t, i) => [t, docs[i]])),
      });
    },
  });

  defineTool(ctx, "proxmox_check_vm_migration", {
    toolset: "vms",
    title: "Check VM migration preconditions",
    description:
      "Check whether and where a VM can be migrated before calling proxmox_migrate_vm: allowed and blocked target nodes (with unavailable storages or blocking HA rules), local disks that would need with_local_disks/targetstorage, local devices (PCI/USB passthrough) that block migration, and mapped resources.",
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      target: z.string().optional().describe("Target node to check specifically"),
    },
    handler: async (a) => {
      const g = await vm(a);
      const r = await client.get<Obj>(`${client.guestPath(g)}/migrate`, defined({ target: a.target }));
      return compact({
        ...ref(g),
        running: flag(r.running),
        allowed_nodes: r.allowed_nodes,
        not_allowed_nodes: r.not_allowed_nodes,
        local_disks: (r.local_disks ?? []).map((d: Obj) =>
          compact({ volid: d.volid, size: formatBytes(d.size), cdrom: flag(d.cdrom) || undefined, unused: flag(d.is_unused) || undefined, drive: d.drivename }),
        ),
        local_resources: r.local_resources,
        mapped_resources: r["mapped-resource-info"] ?? r["mapped-resources"],
        dependent_ha_resources: r["dependent-ha-resources"],
        can_migrate_conntrack: r["has-dbus-vmstate"] === undefined ? undefined : flag(r["has-dbus-vmstate"]),
      });
    },
  });

  defineTool(ctx, "proxmox_check_vm_feature", {
    toolset: "vms",
    title: "Check VM feature support",
    description:
      "Check whether a VM's storage supports snapshots, linked clones ('clone') or full copies ('copy'), optionally for a given snapshot, and on which nodes. Useful before proxmox_clone_vm or taking snapshots.",
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      feature: z.enum(["snapshot", "clone", "copy"]).describe("Feature to check"),
      snapname: z.string().optional().describe("Check for this snapshot"),
    },
    handler: async (a) => {
      const g = await vm(a);
      const r = await client.get<Obj>(`${client.guestPath(g)}/feature`, defined({ feature: a.feature, snapname: a.snapname }));
      return compact({ ...ref(g), feature: a.feature, available: flag(r.hasFeature) ?? false, nodes: r.nodes });
    },
  });

  // ---------------------------------------------------------------- write

  defineTool(ctx, "proxmox_vm_power", {
    toolset: "vms",
    title: "Start, stop or reboot a VM",
    description:
      "Change a VM's power state. start; shutdown (clean ACPI shutdown, optionally force_stop after timeout); stop (hard power-off, may lose data); reboot (clean, applies pending config); reset (hard reset); suspend (pause in RAM); resume; hibernate (save RAM to disk and stop; resumes on next start). Reports without acting when the VM is already in the requested state.",
    write: true,
    destructive: true,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      action: z.enum(["start", "stop", "shutdown", "reboot", "reset", "suspend", "resume", "hibernate"]).describe("Power action"),
      timeout: z.number().int().positive().optional().describe("Seconds to wait for start/stop/shutdown/reboot to complete"),
      force_stop: z.boolean().optional().describe("shutdown: hard-stop the VM if it hasn't shut down within the timeout"),
      keep_active: z.boolean().optional().describe("stop/shutdown: don't deactivate storage volumes"),
      overrule_shutdown: z.boolean().optional().describe("stop: abort running shutdown tasks first"),
      state_storage: z.string().optional().describe("hibernate: storage for the saved RAM state"),
      machine: z.string().optional().describe("start: override the QEMU machine type for this boot"),
      skiplock: skiplockArg,
      wait: waitArg,
    },
    handler: async (a) => {
      const g = await vm(a);
      const only = (field: keyof typeof a, actions: string[]) => {
        if (a[field] !== undefined && !actions.includes(a.action)) throw new Error(`${field} only applies to: ${actions.join(", ")}`);
      };
      only("timeout", ["start", "stop", "shutdown", "reboot"]);
      only("force_stop", ["shutdown"]);
      only("keep_active", ["stop", "shutdown"]);
      only("overrule_shutdown", ["stop"]);
      only("state_storage", ["hibernate"]);
      only("machine", ["start"]);
      only("skiplock", ["start", "stop", "shutdown", "reset", "suspend", "resume", "hibernate"]);
      if (g.template) throw new Error(`VM ${g.vmid} is a template and cannot be started; clone it with proxmox_clone_vm`);
      if (a.action === "start" && g.status === "running") return { ...ref(g), action: a.action, status: "already running" };
      if ((a.action === "stop" || a.action === "shutdown") && g.status === "stopped") return { ...ref(g), action: a.action, status: "already stopped" };
      const endpoint = a.action === "hibernate" ? "suspend" : a.action;
      const upid = await client.post(
        `${client.guestPath(g)}/status/${endpoint}`,
        defined({
          timeout: a.timeout,
          forceStop: a.force_stop,
          keepActive: a.keep_active,
          "overrule-shutdown": a.overrule_shutdown,
          todisk: a.action === "hibernate" ? true : undefined,
          statestorage: a.state_storage,
          machine: a.machine,
          skiplock: a.skiplock,
        }),
      );
      return runTask(client, upid, a.wait, { ...ref(g), action: a.action });
    },
  });

  defineTool(ctx, "proxmox_create_vm", {
    toolset: "vms",
    title: "Create a VM",
    description:
      "Create a QEMU VM. Give disks as typed objects (bus, storage, size_gb, options, or import_from to import a cloud image) or raw specs, an ISO to attach as CD-ROM, NICs (default one virtio NIC on vmbr0), UEFI (bios=ovmf adds an EFI disk) and TPM, and cloud-init settings (setting cloudinit_storage adds the cloud-init drive). VMID defaults to the next free one. Anything else can go in `extra` with Proxmox's parameter names (e.g. hostpci0, serial0, args). To copy an existing VM or template use proxmox_clone_vm; to restore a backup use the backup tools.",
    write: true,
    destructive: false,
    input: {
      node: nodeArg,
      vmid: z.number().int().min(100).max(999999999).optional().describe("VMID for the new VM (default: next free ID)"),
      ...commonFields,
      storage: z.string().optional().describe("Default storage for disks, EFI/TPM state and the cloud-init drive when they don't name one, e.g. local-lvm"),
      disks: z.array(diskSpec).optional().describe("Disks, e.g. [{bus: \"scsi\", storage: \"local-lvm\", size_gb: 32, options: {discard: \"on\", ssd: true, iothread: true}}]"),
      iso: z.string().optional().describe("ISO volume to attach as CD-ROM on ide2, e.g. 'local:iso/debian-12.iso'"),
      nets: z.array(netSpec).optional().describe("Network interfaces net0, net1... (default one virtio NIC on vmbr0; [] for none)"),
      efi_storage: z.string().optional().describe("Storage for the EFI vars disk (used with bios=ovmf; default `storage` / first disk's storage)"),
      efi_pre_enrolled_keys: z.boolean().optional().describe("Pre-enroll Secure Boot keys in the EFI disk (needed for Windows 11 Secure Boot)"),
      tpm_storage: z.string().optional().describe("Add a TPM 2.0 state disk on this storage (needed for Windows 11)"),
      cloudinit_storage: z.string().optional().describe("Add a cloud-init drive on this storage (ide2, or ide0 when an ISO uses ide2)"),
      start: z.boolean().optional().describe("Start the VM after creating it"),
      pool: z.string().optional().describe("Add the VM to this resource pool"),
      extra: extraArg,
      wait: waitArg,
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const vmid = a.vmid ?? (await nextId());
      const slots = new SlotAllocator();
      const params: Obj = {};
      if (a.iso) params[slots.take("ide2")] = `${a.iso},media=cdrom`;
      for (const d of a.disks ?? []) {
        const bus = d.bus ?? "scsi";
        const key = d.index !== undefined ? slots.take(`${bus}${d.index}`) : slots.next(bus);
        params[key] = buildDisk(d, a.storage);
      }
      const firstStorage = a.storage ?? a.disks?.find((d) => d.storage)?.storage;
      if (a.cloudinit_storage) params[slots.next("ide", [2, 0, 1, 3])] = `${a.cloudinit_storage}:cloudinit`;
      if (a.bios === "ovmf") {
        const efi = a.efi_storage ?? firstStorage;
        if (!efi) throw new Error("bios=ovmf needs an EFI disk: set efi_storage (or storage)");
        params.efidisk0 = toPropertyString({ _default: `${efi}:1`, efitype: "4m", "pre-enrolled-keys": a.efi_pre_enrolled_keys }, "_default");
      } else if (a.efi_storage) {
        throw new Error("efi_storage only applies with bios=ovmf");
      }
      if (a.tpm_storage) params.tpmstate0 = `${a.tpm_storage}:1,version=v2.0`;
      (a.nets ?? [{}]).forEach((n, i) => {
        params[`net${i}`] = buildNet(n);
      });
      const boot = a.boot_order ?? (a.disks?.length || a.iso ? Object.keys(params).filter((k) => DISK_RE.test(k) && !/^(efidisk|tpmstate)/.test(k) && !/cloudinit/.test(params[k])).sort(bootSort) : undefined);
      const body = withExtra(
        {
          vmid,
          ...commonParams(a),
          scsihw: a.scsihw ?? "virtio-scsi-single",
          ostype: a.ostype ?? "l26",
          ...params,
          boot: boot?.length ? `order=${boot.join(";")}` : undefined,
          start: a.start,
          pool: a.pool,
        },
        a.extra,
        // These turn a create into a backup restore, which with force overwrites an existing VM; use proxmox_restore_backup.
        ["archive", "force", "unique", "live-restore"],
      );
      checkGatedParams(ctx, body);
      const upid = await client.post(`/nodes/${encodeURIComponent(node)}/qemu`, defined(body));
      return runTask(client, upid, a.wait, { vmid, node, name: a.name });
    },
  });

  defineTool(ctx, "proxmox_clone_vm", {
    toolset: "vms",
    title: "Clone a VM or template",
    description:
      "Copy a VM or template to a new VM. Templates get a fast linked clone by default (full=true for an independent copy); normal VMs are always copied in full. newid defaults to the next free VMID. target (another node) only works when the source disks are on shared storage. Clone from a snapshot with snapname. Use proxmox_check_vm_feature feature=clone to see if linked clones are possible.",
    write: true,
    destructive: false,
    input: {
      vmid: vmidArg.describe("Source VM or template ID"),
      node: guestNodeArg,
      newid: z.number().int().min(100).max(999999999).optional().describe("VMID for the clone (default: next free ID)"),
      name: z.string().optional().describe("Name for the clone"),
      full: z.boolean().optional().describe("Full copy instead of a linked clone (templates only; normal VMs are always full; implied by storage/format)"),
      target: z.string().optional().describe("Node to create the clone on (source must be on shared storage)"),
      storage: z.string().optional().describe("Target storage for a full clone"),
      format: z.enum(["raw", "qcow2", "vmdk"]).optional().describe("Disk format for a full clone"),
      pool: z.string().optional().describe("Add the clone to this pool"),
      description: z.string().optional().describe("Notes for the clone"),
      snapname: z.string().optional().describe("Clone from this snapshot"),
      bwlimit: z.number().int().positive().optional().describe("I/O bandwidth limit in KiB/s"),
      wait: waitArg,
    },
    handler: async (a) => {
      const g = await vm(a);
      if ((a.storage || a.format) && a.full === false) throw new Error("storage and format only apply to full clones");
      // Proxmox defaults templates to linked clones and then rejects storage/format, so ask for a full clone explicitly.
      const full = a.full ?? (a.storage || a.format ? true : undefined);
      const newid = a.newid ?? (await nextId());
      const upid = await client.post(
        `${client.guestPath(g)}/clone`,
        defined({
          newid,
          name: a.name,
          full,
          target: a.target,
          storage: a.storage,
          format: a.format,
          pool: a.pool,
          description: a.description,
          snapname: a.snapname,
          bwlimit: a.bwlimit,
        }),
      );
      return runTask(client, upid, a.wait, { source: g.vmid, newid, name: a.name, node: a.target ?? g.node });
    },
  });

  defineTool(ctx, "proxmox_update_vm_config", {
    toolset: "vms",
    title: "Update VM configuration",
    description:
      "Change a VM's settings; only the given fields change. Common fields are typed; for disks, NICs and devices use `extra` with Proxmox keys, e.g. {scsi1: \"local-lvm:20,ssd=1\"} (adds a new 20 GiB disk), {net1: \"virtio,bridge=vmbr1\"}, {hostpci0: \"0000:01:00.0\"}. On a running VM, changes that can't be hot-plugged (e.g. CPU type, memory without hotplug) become pending until the next reboot; the result lists them. `delete` removes keys (deleting a disk key only detaches it to unusedN, but deleting an unusedN key destroys that volume and needs delete permission; proxmox_unlink_vm_disk is the explicit way to destroy disks). `revert` drops pending changes. Use proxmox_resize_vm_disk to grow disks.",
    write: true,
    destructive: true,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      ...commonFields,
      vcpus: z.number().int().positive().optional().describe("Number of hot-plugged vCPUs (≤ sockets×cores)"),
      cpulimit: z.number().min(0).optional().describe("CPU usage limit in cores (0 = unlimited)"),
      cpuunits: z.number().int().min(1).max(262144).optional().describe("CPU scheduler weight"),
      delete: deleteKeysArg,
      revert: z.array(z.string()).optional().describe("Pending changes to revert, by key"),
      digest: digestArg,
      skiplock: skiplockArg,
      extra: extraArg,
      wait: waitArg,
    },
    handler: async (a) => {
      if (a.extra && "force" in a.extra) throw new Error("`force` (physically deleting disks) is not allowed here; use proxmox_unlink_vm_disk");
      const g = await vm(a);
      const params = defined(withExtra({
        ...commonParams(a),
        vcpus: a.vcpus,
        cpulimit: a.cpulimit,
        cpuunits: a.cpuunits,
        delete: deleteParam(a.delete),
        revert: deleteParam(a.revert),
        digest: a.digest,
        skiplock: a.skiplock,
      }, a.extra));
      if (!Object.keys(params).some((k) => k !== "digest" && k !== "skiplock")) throw new Error("Nothing to change: give at least one setting, delete or revert");
      checkGatedParams(ctx, params);
      const upid = await client.post(`${client.guestPath(g)}/config`, params);
      const result = await runTask(client, upid, a.wait, { ...ref(g), updated: Object.keys(params).filter((k) => k !== "digest" && k !== "skiplock") });
      if (result.upid) return result;
      const pending = pendingChanges(await client.get<Obj[]>(`${client.guestPath(g)}/pending`).catch(() => [] as Obj[]));
      return compact({
        ...result,
        pending_changes: pending,
        note: pending.length ? "Some changes are pending and take effect after a reboot (proxmox_vm_power action=reboot)" : undefined,
      });
    },
  });

  defineTool(ctx, "proxmox_resize_vm_disk", {
    toolset: "vms",
    title: "Grow a VM disk",
    description:
      "Grow a VM disk, e.g. size='+10G' to add 10 GiB or '64G' for an absolute size. Disks can't be shrunk. Works on running VMs; the partition and filesystem inside the guest still need extending afterwards.",
    write: true,
    destructive: false,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      disk: diskKeyArg,
      size: z
        .string()
        .regex(/^\+?\d+(\.\d+)?[KMGT]?$/, "a size like +10G or 64G")
        .describe("New size ('64G') or increment ('+10G'); units K, M, G, T"),
      digest: digestArg,
      skiplock: skiplockArg,
      wait: waitArg,
    },
    handler: async (a) => {
      const g = await vm(a);
      const upid = await client.put(`${client.guestPath(g)}/resize`, defined({ disk: a.disk, size: a.size, digest: a.digest, skiplock: a.skiplock }));
      return runTask(client, upid, a.wait, { ...ref(g), disk: a.disk, size: a.size });
    },
  });

  defineTool(ctx, "proxmox_move_vm_disk", {
    toolset: "vms",
    title: "Move a VM disk",
    description:
      "Move a VM disk to another storage (works live on running VMs) or reassign it to another VM (target_vmid, optionally target_disk). After a storage move the old copy is kept as an unused disk unless delete_source=true.",
    write: true,
    destructive: true,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      disk: diskKeyArg,
      storage: z.string().optional().describe("Target storage"),
      format: z.enum(["raw", "qcow2", "vmdk"]).optional().describe("Target format (storage moves only)"),
      delete_source: z.boolean().optional().describe("Delete the original disk after a successful copy (default keep it as unusedN)"),
      target_vmid: z.number().int().min(100).optional().describe("Reassign the disk to this VM instead (same node)"),
      target_disk: diskKeyArg.optional().describe("Disk key on the target VM (default: same key)"),
      bwlimit: z.number().int().positive().optional().describe("I/O bandwidth limit in KiB/s"),
      digest: digestArg,
      target_digest: z.string().optional().describe("Only proceed if the target VM's config digest matches"),
      wait: waitArg,
    },
    handler: async (a) => {
      if (!a.storage && a.target_vmid === undefined) throw new Error("Give a target storage or a target_vmid");
      if (a.storage && a.target_vmid !== undefined) throw new Error("Give either storage or target_vmid, not both");
      const g = await vm(a);
      const upid = await client.post(
        `${client.guestPath(g)}/move_disk`,
        defined({
          disk: a.disk,
          storage: a.storage,
          format: a.format,
          delete: a.delete_source,
          "target-vmid": a.target_vmid,
          "target-disk": a.target_disk,
          bwlimit: a.bwlimit,
          digest: a.digest,
          "target-digest": a.target_digest,
        }),
      );
      return runTask(client, upid, a.wait, compact({ ...ref(g), disk: a.disk, storage: a.storage, target_vmid: a.target_vmid }));
    },
  });

  defineTool(ctx, "proxmox_unlink_vm_disk", {
    toolset: "vms",
    title: "Detach or delete VM disks",
    description:
      "Remove disks from a VM. Without destroy, disks are detached and kept as unusedN entries (data intact, re-attachable via proxmox_update_vm_config). With destroy=true the volumes are permanently deleted. Also deletes unusedN entries' volumes when given with destroy=true.",
    delete: true,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      disks: z
        .array(z.string().regex(/^(ide|sata|scsi|virtio|unused|efidisk|tpmstate)\d+$/, "a disk key like scsi1 or unused0"))
        .min(1)
        .describe("Disk keys to remove, e.g. [\"scsi1\", \"unused0\"]"),
      destroy: z.boolean().optional().describe("Permanently delete the disk images (default false: detach to unusedN)"),
    },
    handler: async (a) => {
      const g = await vm(a);
      await client.put(`${client.guestPath(g)}/unlink`, defined({ idlist: a.disks.join(","), force: a.destroy }));
      return { ...ref(g), disks: a.disks, result: a.destroy ? "destroyed" : "detached (now unused disks)" };
    },
  });

  defineTool(ctx, "proxmox_migrate_vm", {
    toolset: "vms",
    title: "Migrate a VM",
    description:
      "Move a VM to another node in the cluster. Running VMs are live-migrated (online defaults to true when running). VMs with local disks need with_local_disks=true (optionally targetstorage). Check first with proxmox_check_vm_migration.",
    write: true,
    destructive: true,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      target: z.string().describe("Target node"),
      online: z.boolean().optional().describe("Live-migrate a running VM (default true when running)"),
      with_local_disks: z.boolean().optional().describe("Also migrate local disks (live storage migration)"),
      targetstorage: z.string().optional().describe("Target storage, or a mapping 'src:dst,src2:dst2' (with local disks)"),
      migration_type: z.enum(["secure", "insecure"]).optional().describe("insecure skips the SSH tunnel (trusted networks only)"),
      migration_network: z.string().optional().describe("CIDR of the network to migrate over"),
      bwlimit: z.number().int().positive().optional().describe("Bandwidth limit in KiB/s"),
      with_conntrack_state: z.boolean().optional().describe("Also migrate firewall conntrack entries (running VMs)"),
      force: z.boolean().optional().describe("Allow migrating VMs with local devices (root only)"),
      wait: waitArg,
    },
    handler: async (a) => {
      const g = await vm(a);
      if (a.target === g.node) throw new Error(`VM ${g.vmid} is already on ${g.node}`);
      const online = a.online ?? (g.status === "running" ? true : undefined);
      const upid = await client.post(
        `${client.guestPath(g)}/migrate`,
        defined({
          target: a.target,
          online,
          "with-local-disks": a.with_local_disks,
          targetstorage: a.targetstorage,
          migration_type: a.migration_type,
          migration_network: a.migration_network,
          bwlimit: a.bwlimit,
          "with-conntrack-state": a.with_conntrack_state,
          force: a.force,
        }),
      );
      return runTask(client, upid, a.wait, { ...ref(g), from: g.node, target: a.target, online: online ?? false });
    },
  });

  defineTool(ctx, "proxmox_convert_vm_to_template", {
    toolset: "vms",
    title: "Convert a VM to a template",
    description:
      "Turn a stopped VM into a template (irreversible: templates can't be started, only cloned with proxmox_clone_vm). Its disks become read-only base images.",
    write: true,
    destructive: true,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      disk: diskKeyArg.optional().describe("Convert only this disk to a base image"),
      wait: waitArg,
    },
    handler: async (a) => {
      const g = await vm(a);
      if (g.template) return { ...ref(g), status: "already a template" };
      if (g.status === "running") throw new Error(`VM ${g.vmid} is running; shut it down first (proxmox_vm_power action=shutdown)`);
      const upid = await client.post(`${client.guestPath(g)}/template`, defined({ disk: a.disk }));
      return runTask(client, upid, a.wait, { ...ref(g), template: true });
    },
  });

  defineTool(ctx, "proxmox_delete_vm", {
    toolset: "vms",
    title: "Delete a VM",
    description:
      "Permanently destroy a VM and all its disks. Refuses a running VM unless stop=true (hard-stops it first). purge=true also removes it from backup/replication jobs and HA; destroy_unreferenced_disks=true also deletes orphaned disks carrying its VMID.",
    delete: true,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      stop: z.boolean().optional().describe("Hard-stop the VM first if it is running"),
      purge: z.boolean().optional().describe("Also remove the VMID from backup jobs, replication jobs and HA"),
      destroy_unreferenced_disks: z.boolean().optional().describe("Also destroy disks with this VMID not referenced in the config"),
      skiplock: skiplockArg,
      wait: waitArg,
    },
    handler: async (a) => {
      const g = await vm(a);
      const base = client.guestPath(g);
      if (g.status !== "stopped" && !g.template) {
        if (!a.stop) throw new Error(`VM ${g.vmid} (${g.name ?? "unnamed"}) is ${g.status}; stop it first or pass stop=true`);
        const stopped = await runTask(client, await client.post(`${base}/status/stop`, defined({ skiplock: a.skiplock })), true);
        if (stopped.status !== "ok") throw new Error(`Could not confirm VM ${g.vmid} stopped: ${JSON.stringify(stopped)}`);
      }
      const upid = await client.delete(
        base,
        defined({ purge: a.purge, "destroy-unreferenced-disks": a.destroy_unreferenced_disks, skiplock: a.skiplock }),
      );
      return runTask(client, upid, a.wait, { ...ref(g), deleted: true });
    },
  });

  defineTool(ctx, "proxmox_regenerate_vm_cloudinit", {
    toolset: "vms",
    title: "Regenerate VM cloud-init drive",
    description:
      "Rebuild a VM's cloud-init drive from its current settings so pending cloud-init changes (user, keys, IPs, DNS) reach the guest. The guest applies them on its next boot (cloud-init normally runs per instance).",
    write: true,
    destructive: false,
    idempotent: true,
    input: { vmid: vmidArg, node: guestNodeArg },
    handler: async (a) => {
      const g = await vm(a);
      await client.put(`${client.guestPath(g)}/cloudinit`);
      return { ...ref(g), regenerated: true };
    },
  });

  defineTool(ctx, "proxmox_send_vm_key", {
    toolset: "vms",
    title: "Send keystrokes to a VM",
    description:
      "Press keys on a VM's virtual keyboard, as on its console: special keys/combos with `keys` (QEMU names, e.g. 'ctrl-alt-delete', 'ret', 'esc', 'tab', 'f2', 'up', 'alt-f4') and/or type ASCII `text` (US layout, '\\n' presses Enter). Useful for boot menus, installers or a hung login prompt. Keys are sent one at a time; nothing is read back.",
    exec: true,
    destructive: true,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      keys: z.array(z.string().regex(/^[a-z0-9_]+(-[a-z0-9_]+)*$/, "a QEMU key name like ret or ctrl-alt-delete")).optional().describe("Key names sent before `text`"),
      text: z.string().max(2000).optional().describe("Text to type (ASCII, US keyboard layout)"),
      skiplock: skiplockArg,
    },
    handler: async (a) => {
      const keys = [...(a.keys ?? []), ...(a.text ? textToKeys(a.text) : [])];
      if (!keys.length) throw new Error("Give keys and/or text");
      const g = await vm(a);
      if (g.status !== "running") throw new Error(`VM ${g.vmid} is ${g.status ?? "not running"}`);
      for (const key of keys) await client.put(`${client.guestPath(g)}/sendkey`, defined({ key, skiplock: a.skiplock }));
      return { ...ref(g), sent: keys.length, keys: keys.length <= 50 ? keys : undefined };
    },
  });

  defineTool(ctx, "proxmox_vm_monitor_command", {
    toolset: "vms",
    title: "Run a QEMU monitor command",
    description:
      "Run a QEMU human monitor (HMP) command on a running VM and return its output, e.g. 'info status', 'info block', 'info network', 'info cpus', 'info balloon', 'info migrate'. Non-info commands (e.g. 'balloon 2048', 'device_del') change the live VM; prefer the dedicated tools where they exist.",
    exec: true,
    destructive: true,
    input: { vmid: vmidArg, node: guestNodeArg, command: z.string().min(1).describe("Monitor command, e.g. 'info block'") },
    handler: async (a) => {
      const g = await vm(a);
      const output = await client.post<string>(`${client.guestPath(g)}/monitor`, { command: a.command });
      return { ...ref(g), command: a.command, output: typeof output === "string" ? output.trimEnd() : output };
    },
  });
}

/** Boot order: disks in bus order (scsi, virtio, sata, ide) then index, CD-ROM (ide2) last among disks. */
function bootSort(x: string, y: string): number {
  const rank = (k: string) => {
    const m = k.match(/^([a-z]+)(\d+)$/)!;
    const bus = ["scsi", "virtio", "sata", "ide"].indexOf(m[1]);
    return k === "ide2" ? 1000 : bus * 100 + Number(m[2]);
  };
  return rank(x) - rank(y);
}

/** Hides password hashes and similar secrets in generated cloud-init YAML. */
function redactCloudInitDoc(text: string): string {
  return text.replace(/^(\s*(?:password|passwd|hashed_passwd|plain_text_passwd)\s*:\s*).+$/gim, "$1<redacted>");
}
