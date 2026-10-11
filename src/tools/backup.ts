import { z } from "zod";
import { ProxmoxError, type ProxmoxClient } from "../proxmox/client.js";
import { compact, flag, formatBytes, isoTime, parsePropertyString, redactSecrets, toPropertyString } from "../proxmox/format.js";
import {
  defined,
  defineTool,
  deleteKeysArg,
  deleteParam,
  enc,
  extraArg,
  filterAndLimit,
  limitArg,
  nodeArg,
  rawArg,
  runTask,
  searchArg,
  vmidArg,
  waitArg,
  type ToolContext,
} from "./util.js";

type Obj = Record<string, any>;

// ---------------------------------------------------------------- arguments

const modeArg = z
  .enum(["snapshot", "suspend", "stop"])
  .optional()
  .describe("Backup mode: snapshot (live, default), suspend (brief pause) or stop (shuts the guest down for a consistent backup, then restarts it)");

const compressArg = z
  .enum(["0", "1", "gzip", "lzo", "zstd"])
  .optional()
  .describe("Compression: zstd (recommended), gzip, lzo, or 0 for none. Ignored for Proxmox Backup Server storages");

const retentionArg = z
  .object({
    keep_all: z.boolean().optional().describe("Keep all backups (disables the other options)"),
    keep_last: z.number().int().min(0).optional().describe("Keep the last N backups"),
    keep_hourly: z.number().int().min(0).optional().describe("Keep backups for the last N hours (one per hour)"),
    keep_daily: z.number().int().min(0).optional().describe("Keep backups for the last N days (one per day)"),
    keep_weekly: z.number().int().min(0).optional().describe("Keep backups for the last N weeks"),
    keep_monthly: z.number().int().min(0).optional().describe("Keep backups for the last N months"),
    keep_yearly: z.number().int().min(0).optional().describe("Keep backups for the last N years"),
  })
  .optional()
  .describe("Retention (prune-backups) settings, e.g. {keep_last: 3, keep_daily: 7, keep_weekly: 4}");

const notesTemplateArg = z
  .string()
  .optional()
  .describe("Notes for the backup(s); may use {{cluster}}, {{guestname}}, {{node}} and {{vmid}}, e.g. \"{{guestname}} before upgrade\"");

const volidArg = z.string().min(3).describe("Backup volume ID as listed by proxmox_list_backups, e.g. local:backup/vzdump-qemu-100-2025_01_01-02_00_00.vma.zst or pbs:backup/vm/100/2025-01-01T02:00:00Z");

const backupNodeArg = z
  .string()
  .optional()
  .describe("Node to access the storage from. Needed for node-local storages present on several nodes (use the node shown by proxmox_list_backups); looked up automatically otherwise");

type Retention = z.infer<typeof retentionArg>;

/** {keep_last: 3} → "keep-last=3". */
function retentionString(r: Retention): string | undefined {
  if (!r) return undefined;
  const parts = Object.entries(r)
    .filter(([, v]) => v !== undefined)
    .map(([k, v]) => `${k.replace(/_/g, "-")}=${typeof v === "boolean" ? (v ? 1 : 0) : v}`);
  return parts.length ? parts.join(",") : undefined;
}

/** prune-backups arrives as a property string or (in job listings) an object; normalise to {keep_last: 3}. */
function parseRetention(value: unknown): Obj | undefined {
  if (value === undefined || value === null || value === "") return undefined;
  const obj: Obj = typeof value === "string" ? parsePropertyString(value) : (value as Obj);
  const out: Obj = {};
  for (const [k, v] of Object.entries(obj)) out[k.replace(/-/g, "_")] = k === "keep-all" ? flag(v) : Number(v);
  return out;
}

/** Job/defaults fields that may come back as objects but must be sent as property strings. */
function toParam(value: unknown): unknown {
  if (value && typeof value === "object" && !Array.isArray(value)) return toPropertyString(value as Obj);
  return value;
}

function vmidList(value: unknown): number[] {
  return String(value ?? "")
    .split(/[,\s]+/)
    .filter(Boolean)
    .map(Number)
    .filter(Number.isFinite);
}

// ---------------------------------------------------------------- summaries

function summarizeJob(j: Obj): Obj {
  const selection = j.all
    ? compact({ all: true, exclude: j.exclude ? vmidList(j.exclude) : undefined })
    : j.pool
      ? { pool: j.pool }
      : { vmids: vmidList(j.vmid) };
  return compact({
    id: j.id,
    enabled: j.enabled === undefined ? true : flag(j.enabled),
    schedule: j.schedule ?? (j.starttime ? `${j.dow ?? "daily"} ${j.starttime}` : undefined),
    next_run: isoTime(j["next-run"]),
    storage: j.storage,
    mode: j.mode ?? "snapshot",
    selection,
    node: j.node,
    compress: j.compress,
    retention: parseRetention(j["prune-backups"]),
    remove_old: j.remove === undefined ? undefined : flag(j.remove),
    protected: flag(j.protected) || undefined,
    notes_template: j["notes-template"],
    comment: j.comment,
    repeat_missed: flag(j["repeat-missed"]) || undefined,
    notification_mode: j["notification-mode"],
    mailto: j.mailto,
  });
}

/** Guest type from a backup listing entry or volid: "qemu" or "lxc". */
export function backupGuestType(b: Obj | string): "qemu" | "lxc" | undefined {
  const e: Obj = typeof b === "string" ? { volid: b } : b;
  if (e.subtype === "qemu" || e.subtype === "lxc") return e.subtype;
  if (e.format === "pbs-vm") return "qemu";
  if (e.format === "pbs-ct") return "lxc";
  const v = String(e.volid ?? "");
  if (/vzdump-qemu-|backup\/vm\/|\.vma(\.|$)/.test(v)) return "qemu";
  if (/vzdump-(lxc|openvz)-|backup\/ct\//.test(v)) return "lxc";
  return undefined;
}

function summarizeBackup(b: Obj): Obj {
  const type = backupGuestType(b);
  const size = b.size ?? b["approximate-size"];
  return compact({
    volid: b.volid,
    storage: b._storage,
    node: b._node,
    vmid: b.vmid !== undefined ? Number(b.vmid) : undefined,
    type: type === "qemu" ? "vm" : type === "lxc" ? "container" : undefined,
    time: isoTime(b.ctime),
    size: formatBytes(size),
    size_bytes: size !== undefined ? Number(size) : undefined,
    format: b.format,
    notes: b.notes,
    protected: flag(b.protected) || undefined,
    encrypted: b.encrypted ? true : undefined,
    verification: b.verification?.state,
  });
}

/** Storage IDs are the part of a volid before the first colon. */
function storageOf(volid: string): string {
  const i = volid.indexOf(":");
  if (i <= 0) throw new ProxmoxError(`Not a volume ID: ${volid} (expected storage:path, see proxmox_list_backups)`);
  return volid.slice(0, i);
}

/**
 * Picks a node that can reach `storage`: the given node, else any online node
 * for shared storage, else the single node that has it, else the default node.
 */
async function nodeForStorage(client: ProxmoxClient, storage: string, node?: string): Promise<string> {
  if (node) return node;
  const all = await client.get<Obj[]>("/cluster/resources", { type: "storage" });
  const entries = all.filter((s) => s.storage === storage && s.status !== "unknown");
  if (entries.length === 0) throw new ProxmoxError(`Storage ${storage} was not found on any online node`);
  if (entries.some((s) => flag(s.shared)) || entries.length === 1) {
    if (client.defaultNode && entries.some((s) => s.node === client.defaultNode)) return client.defaultNode;
    return entries[0].node;
  }
  if (client.defaultNode && entries.some((s) => s.node === client.defaultNode)) return client.defaultNode;
  throw new ProxmoxError(
    `Storage ${storage} is local to several nodes (${entries.map((s) => s.node).sort().join(", ")}); pass \`node\` (the node shown by proxmox_list_backups)`,
  );
}

/** Splits an `extractconfig` text into the main config and snapshot section names. */
function parseConfigText(text: string): { config: Obj; snapshots: string[] } {
  const config: Obj = {};
  const snapshots: string[] = [];
  let inMain = true;
  for (const line of text.split("\n")) {
    const section = line.match(/^\[([^\]]+)\]\s*$/);
    if (section) {
      inMain = false;
      // [PENDING] and [special:cloudinit] are not snapshots.
      if (section[1] !== "PENDING" && !section[1].startsWith("special:")) snapshots.push(section[1]);
      continue;
    }
    if (!inMain || !line.trim()) continue;
    if (line.startsWith("#")) {
      // vzdump adds "#qmdump#map:..." disk mapping lines to VMA configs; they aren't part of the notes.
      if (line.startsWith("#qmdump#")) continue;
      let text = line.slice(1);
      try {
        text = decodeURIComponent(text);
      } catch {
        /* not percent-encoded; keep as-is */
      }
      config.description = config.description !== undefined ? `${config.description}\n${text}` : text;
      continue;
    }
    const idx = line.indexOf(":");
    if (idx > 0) config[line.slice(0, idx).trim()] = line.slice(idx + 1).trim();
  }
  return { config, snapshots };
}

/** vzdump's `script` and a guest's `hookscript` run as root on the node, so they need exec permission too. */
function checkHookParams(ctx: ToolContext, extra: Obj | undefined): void {
  const key = ["script", "hookscript"].find((k) => extra?.[k] !== undefined);
  if (key && !ctx.allowExec) throw new ProxmoxError(`${key} runs a script as root on the node, so setting it needs exec permission (PROXMOX_ALLOW_EXEC)`);
}

// Job keys that describe the schedule/job rather than vzdump options.
const JOB_ONLY_KEYS = new Set(["id", "type", "schedule", "enabled", "comment", "repeat-missed", "next-run", "starttime", "dow", "node", "digest"]);

export function registerBackupTools(ctx: ToolContext): void {
  const { client } = ctx;

  /** Online nodes and where each guest lives. */
  async function clusterMap(): Promise<{ nodes: string[]; guests: Map<number, Obj> }> {
    const res = await client.get<Obj[]>("/cluster/resources");
    const nodes = res.filter((r) => r.type === "node" && r.status === "online").map((r) => String(r.node));
    const guests = new Map(res.filter((r) => r.type === "qemu" || r.type === "lxc").map((r) => [Number(r.vmid), r]));
    return { nodes, guests };
  }

  /** Starts one vzdump per node and handles the tasks (in parallel). */
  async function runVzdump(perNode: Map<string, Obj>, wait: boolean | undefined): Promise<Obj[]> {
    return Promise.all(
      [...perNode].map(async ([node, params]) => {
        const upid = await client.post(`/nodes/${enc(node)}/vzdump`, params);
        try {
          return await runTask(client, upid, wait, compact({ node, vmids: params.vmid ? vmidList(params.vmid) : undefined, all: params.all ? true : undefined, pool: params.pool }));
        } catch (err) {
          return { node, vmids: params.vmid ? vmidList(params.vmid) : undefined, status: "failed", error: err instanceof Error ? err.message : String(err) };
        }
      }),
    );
  }

  /** Groups VMIDs by the node they live on, failing on unknown ones. */
  function groupByNode(vmids: number[], guests: Map<number, Obj>): Map<string, number[]> {
    const missing = vmids.filter((v) => !guests.has(v));
    if (missing.length) throw new ProxmoxError(`No VM or container with ID ${missing.join(", ")} (see proxmox_list_guests)`);
    const out = new Map<string, number[]>();
    for (const v of vmids) {
      const node = String(guests.get(v)!.node);
      out.set(node, [...(out.get(node) ?? []), v]);
    }
    return out;
  }

  // ---------------------------------------------------------------- jobs

  defineTool(ctx, "proxmox_list_backup_jobs", {
    toolset: "backup",
    title: "List backup jobs",
    description:
      "List the cluster's scheduled backup (vzdump) jobs: schedule, next run, enabled, target storage, mode, which guests (all/VMID list/pool, exclusions), compression and retention. See proxmox_get_backup_job for one job with the guests and disks it covers, and proxmox_list_unbacked_guests for guests no job covers.",
    input: { search: searchArg, raw: rawArg },
    handler: async (a) => {
      const jobs = await client.get<Obj[]>("/cluster/backup");
      jobs.sort((x, y) => String(x.id).localeCompare(String(y.id)));
      const res = filterAndLimit(jobs, { search: a.search }, (j) => [j.id, j.storage, j.comment, j.vmid, j.pool, j.schedule]);
      return { ...res, items: a.raw ? redactSecrets(res.items) : res.items.map(summarizeJob) };
    },
  });

  defineTool(ctx, "proxmox_get_backup_job", {
    toolset: "backup",
    title: "Get backup job",
    description:
      "Get one backup job's settings plus the guests it currently includes and, per guest, which disks are backed up (and why others are excluded, e.g. backup=0).",
    input: {
      id: z.string().min(1).describe("Backup job ID (see proxmox_list_backup_jobs)"),
      include_volumes: z.boolean().optional().describe("Include the covered guests and their disks (default true)"),
      raw: rawArg,
    },
    handler: async (a) => {
      const path = `/cluster/backup/${enc(a.id)}`;
      const [job, vols] = await Promise.all([
        client.get<Obj>(path),
        a.include_volumes === false ? undefined : client.get<Obj>(`${path}/included_volumes`).catch(() => undefined),
      ]);
      const guests = (vols?.children as Obj[] | undefined)?.map((g) =>
        compact({
          vmid: Number(g.id),
          name: g.name,
          type: g.type === "qemu" ? "vm" : g.type === "lxc" ? "container" : g.type,
          volumes: ((g.children ?? []) as Obj[]).map((v) => compact({ id: v.id, name: v.name, included: Boolean(v.included), reason: v.reason })),
        }),
      );
      return compact({ ...(a.raw ? { job: redactSecrets(job) } : summarizeJob({ id: a.id, ...job })), guests });
    },
  });

  defineTool(ctx, "proxmox_save_backup_job", {
    toolset: "backup",
    title: "Create or update backup job",
    description:
      "Create a scheduled backup job, or update an existing one when `id` names one. Choose guests with exactly one of vmids, all (optionally with exclude) or pool; on update, giving a new selection replaces the old one. schedule uses systemd calendar syntax, e.g. \"daily\", \"sat 02:00\", \"mon..fri 21:30\" (check with proxmox_simulate_schedule). Only given fields change on update. Run a job immediately with proxmox_run_backup_job.",
    write: true,
    destructive: false,
    idempotent: true,
    input: {
      id: z.string().optional().describe("Job ID. Updates the job if it exists; otherwise creates it with this ID (auto-generated when omitted)"),
      schedule: z.string().optional().describe("When to run, systemd calendar event, e.g. \"daily\", \"02:30\", \"sun 01:00\""),
      enabled: z.boolean().optional().describe("Enable or disable the job"),
      storage: z.string().optional().describe("Target storage (must allow 'backup' content)"),
      vmids: z.array(z.number().int()).optional().describe("Back up these guests"),
      all: z.boolean().optional().describe("Back up all guests (optionally minus `exclude`)"),
      exclude: z.array(z.number().int()).optional().describe("With all=true: guests to skip"),
      pool: z.string().optional().describe("Back up all guests in this resource pool"),
      node: z.string().optional().describe("Only run on this node (default: every node backs up its own guests)"),
      mode: modeArg,
      compress: compressArg,
      retention: retentionArg,
      remove_old: z.boolean().optional().describe("Prune older backups after the run according to the retention (Proxmox default true)"),
      notes_template: notesTemplateArg,
      protected: z.boolean().optional().describe("Mark the resulting backups as protected (never pruned or deleted)"),
      comment: z.string().optional().describe("Job description"),
      repeat_missed: z.boolean().optional().describe("Run as soon as possible if a run was missed (e.g. the node was off)"),
      bwlimit: z.number().int().min(0).optional().describe("I/O bandwidth limit in KiB/s (0 = unlimited)"),
      delete: deleteKeysArg,
      extra: extraArg,
    },
    handler: async (a) => {
      checkHookParams(ctx, a.extra);
      const exists = a.id ? (await client.get<Obj[]>("/cluster/backup")).some((j) => j.id === a.id) : false;
      const selectionGiven = a.vmids !== undefined || a.all !== undefined || a.pool !== undefined;
      if (!exists && !selectionGiven && !a.extra?.vmid && !a.extra?.all && !a.extra?.pool) {
        throw new ProxmoxError("A new backup job needs a selection: pass vmids, all=true or pool");
      }
      const toDelete = [...(a.delete ?? [])];
      if (exists && selectionGiven) {
        // Swapping selection type: clear the old one, or Proxmox keeps both.
        if (a.vmids === undefined) toDelete.push("vmid");
        if (a.pool === undefined) toDelete.push("pool");
        if (!a.all) toDelete.push("all");
        if (!a.all && a.exclude === undefined) toDelete.push("exclude");
      }
      const params = defined({
        schedule: a.schedule,
        enabled: a.enabled,
        storage: a.storage,
        vmid: a.vmids?.join(","),
        all: a.all || undefined,
        exclude: a.exclude?.join(","),
        pool: a.pool,
        node: a.node,
        mode: a.mode,
        compress: a.compress,
        "prune-backups": retentionString(a.retention),
        remove: a.remove_old,
        "notes-template": a.notes_template,
        protected: a.protected,
        comment: a.comment,
        "repeat-missed": a.repeat_missed,
        bwlimit: a.bwlimit,
        ...a.extra,
      });
      if (exists) {
        await client.put(`/cluster/backup/${enc(a.id!)}`, { ...params, delete: deleteParam([...new Set(toDelete)]) });
        return { id: a.id, status: "updated" };
      }
      await client.post("/cluster/backup", { id: a.id, ...params });
      // The API doesn't return the generated ID; find the job we just made.
      if (!a.id) {
        const jobs = await client.get<Obj[]>("/cluster/backup").catch(() => [] as Obj[]);
        const match = jobs.filter((j) => j.storage === params.storage && j.schedule === params.schedule && j.comment === params.comment).at(-1);
        return compact({ id: match?.id, status: "created" });
      }
      return { id: a.id, status: "created" };
    },
  });

  defineTool(ctx, "proxmox_delete_backup_job", {
    toolset: "backup",
    title: "Delete backup job",
    description: "Delete a scheduled backup job. Existing backups are kept. To pause a job instead, use proxmox_save_backup_job with enabled=false.",
    delete: true,
    idempotent: true,
    input: { id: z.string().min(1).describe("Backup job ID (see proxmox_list_backup_jobs)") },
    handler: async (a) => {
      await client.delete(`/cluster/backup/${enc(a.id)}`);
      return { id: a.id, status: "deleted" };
    },
  });

  defineTool(ctx, "proxmox_run_backup_job", {
    toolset: "backup",
    title: "Run backup job now",
    description:
      "Run a scheduled backup job immediately with its own settings (storage, mode, compression, retention...), the way the scheduler would: one vzdump task per node for the guests on that node. By default waits for all tasks; with wait=false returns the task IDs. Note that with remove_old (the default) old backups are pruned per the job's retention.",
    write: true,
    destructive: true,
    input: { id: z.string().min(1).describe("Backup job ID (see proxmox_list_backup_jobs)"), wait: waitArg },
    handler: async (a) => {
      const job = await client.get<Obj>(`/cluster/backup/${enc(a.id)}`);
      const base: Obj = {};
      for (const [k, v] of Object.entries(job)) if (!JOB_ONLY_KEYS.has(k) && v !== undefined && v !== null) base[k] = toParam(v);
      const { nodes, guests } = await clusterMap();
      const perNode = new Map<string, Obj>();
      if (job.vmid && !job.all && !job.pool) {
        const vmids = vmidList(job.vmid);
        // Guests deleted since the job was made are skipped, like the scheduler does.
        const present = vmids.filter((v) => guests.has(v));
        if (!present.length) throw new ProxmoxError(`None of the job's guests (${vmids.join(", ")}) exist any more`);
        for (const [node, ids] of groupByNode(present, guests)) {
          if (job.node && node !== job.node) continue;
          perNode.set(node, { ...base, vmid: ids.join(",") });
        }
      } else {
        for (const node of job.node ? [job.node] : nodes) {
          // Skip nodes without any matching guest to avoid empty-run errors.
          const has = [...guests.values()].some(
            (g) => g.node === node && (job.pool ? g.pool === job.pool : !vmidList(job.exclude).includes(Number(g.vmid))),
          );
          if (has) perNode.set(node, { ...base });
        }
      }
      if (!perNode.size) throw new ProxmoxError(`Backup job ${a.id} matches no guests on any online node`);
      const tasks = await runVzdump(perNode, a.wait);
      return { id: a.id, tasks };
    },
  });

  defineTool(ctx, "proxmox_list_unbacked_guests", {
    toolset: "backup",
    title: "List guests without backup job",
    description:
      "List VMs and containers that no backup job covers, with node and status. Use it to audit backup coverage; add them with proxmox_save_backup_job or back them up once with proxmox_backup_guests.",
    input: {},
    handler: async () => {
      const [list, { guests }] = await Promise.all([client.get<Obj[]>("/cluster/backup-info/not-backed-up"), clusterMap()]);
      const items = list
        .map((g) => {
          const r = guests.get(Number(g.vmid));
          return compact({
            vmid: Number(g.vmid),
            name: g.name ?? r?.name,
            type: g.type === "qemu" ? "vm" : g.type === "lxc" ? "container" : g.type,
            node: r?.node,
            status: r?.status,
            template: r?.template ? true : undefined,
          });
        })
        .sort((x, y) => Number(x.vmid) - Number(y.vmid));
      return { total: items.length, items };
    },
  });

  // ---------------------------------------------------------------- ad-hoc backup

  defineTool(ctx, "proxmox_backup_guests", {
    toolset: "backup",
    title: "Back up guests now",
    description:
      "Back up one or more VMs/containers now (vzdump), e.g. before an upgrade. Guests are grouped by node and one task runs per node. Defaults come from the node's vzdump defaults and the storage. Use protected=true or notes_template to label a one-off backup, and remove_old=false to avoid pruning older backups as a side effect. Waits for completion by default (large guests can exceed the wait; follow with proxmox_get_task).",
    write: true,
    destructive: true,
    input: {
      vmids: z.array(z.number().int().min(100)).min(1).describe("Guests to back up, e.g. [100, 200]"),
      storage: z.string().optional().describe("Target storage (default: from vzdump defaults, usually 'local')"),
      mode: modeArg,
      compress: compressArg,
      notes_template: notesTemplateArg,
      protected: z.boolean().optional().describe("Protect the new backups from pruning and deletion"),
      remove_old: z.boolean().optional().describe("Prune older backups of these guests per the retention afterwards (Proxmox default true)"),
      retention: retentionArg,
      bwlimit: z.number().int().min(0).optional().describe("I/O bandwidth limit in KiB/s"),
      extra: extraArg,
      wait: waitArg,
    },
    handler: async (a) => {
      checkHookParams(ctx, a.extra);
      const { guests } = await clusterMap();
      const groups = groupByNode([...new Set(a.vmids)], guests);
      const common = defined({
        storage: a.storage,
        mode: a.mode,
        compress: a.compress,
        "notes-template": a.notes_template,
        protected: a.protected,
        remove: a.remove_old,
        "prune-backups": retentionString(a.retention),
        bwlimit: a.bwlimit,
        ...a.extra,
      });
      const perNode = new Map([...groups].map(([node, ids]) => [node, { ...common, vmid: ids.join(",") }]));
      const tasks = await runVzdump(perNode, a.wait);
      const failed = tasks.filter((t) => t.status === "failed").length;
      return compact({ status: failed ? (failed === tasks.length ? "failed" : "partially failed") : undefined, tasks });
    },
  });

  // ---------------------------------------------------------------- backup volumes

  defineTool(ctx, "proxmox_list_backups", {
    toolset: "backup",
    title: "List backups",
    description:
      "List backup archives across every storage that holds backups (local dirs, NFS/CIFS, Proxmox Backup Server...), newest first: volume ID, guest, time, size, format, notes, protection and PBS verification state. Filter by vmid, type, storage, node or age. Shared storages are listed once. Use the volid with proxmox_restore_backup, proxmox_get_backup_config, proxmox_update_backup or proxmox_delete_backup.",
    input: {
      vmid: z.number().int().optional().describe("Only backups of this guest"),
      type: z.enum(["vm", "container"]).optional().describe("Only VM or only container backups"),
      storage: z.string().optional().describe("Only this storage"),
      node: z.string().optional().describe("Only storages on this node"),
      older_than_days: z.number().min(0).optional().describe("Only backups older than this many days"),
      newer_than_days: z.number().min(0).optional().describe("Only backups newer than this many days"),
      search: searchArg,
      limit: limitArg(100),
      raw: rawArg,
    },
    handler: async (a) => {
      const storages = (await client.get<Obj[]>("/cluster/resources", { type: "storage" })).filter(
        (s) =>
          s.status === "available" &&
          String(s.content ?? "").split(",").includes("backup") &&
          (!a.storage || s.storage === a.storage) &&
          (!a.node || s.node === a.node),
      );
      if (a.storage && !storages.length) throw new ProxmoxError(`Storage ${a.storage} is not an available backup storage${a.node ? ` on ${a.node}` : ""}`);
      const seenShared = new Set<string>();
      const targets = storages.filter((s) => {
        if (!flag(s.shared)) return true;
        if (seenShared.has(s.storage)) return false;
        seenShared.add(s.storage);
        return true;
      });
      const errors: Obj[] = [];
      const lists = await Promise.all(
        targets.map(async (s): Promise<Obj[]> => {
          try {
            const items = await client.get<Obj[]>(`/nodes/${enc(s.node)}/storage/${enc(s.storage)}/content`, defined({ content: "backup", vmid: a.vmid }));
            return items.map((b) => ({ ...b, _storage: s.storage, _node: flag(s.shared) ? undefined : s.node }));
          } catch (err) {
            errors.push({ storage: s.storage, node: s.node, error: err instanceof Error ? err.message : String(err) });
            return [];
          }
        }),
      );
      let backups = lists.flat();
      if (a.vmid !== undefined) backups = backups.filter((b) => Number(b.vmid) === a.vmid);
      if (a.type) backups = backups.filter((b) => backupGuestType(b) === (a.type === "vm" ? "qemu" : "lxc"));
      const now = Date.now() / 1000;
      if (a.older_than_days !== undefined) backups = backups.filter((b) => Number(b.ctime) < now - a.older_than_days! * 86400);
      if (a.newer_than_days !== undefined) backups = backups.filter((b) => Number(b.ctime) >= now - a.newer_than_days! * 86400);
      backups.sort((x, y) => Number(y.ctime ?? 0) - Number(x.ctime ?? 0));
      const res = filterAndLimit(backups, { search: a.search, limit: a.limit ?? 100 }, (b) => [b.volid, b.vmid, b.notes, b._storage]);
      const totalBytes = backups.reduce((n, b) => n + (Number(b.size ?? b["approximate-size"]) || 0), 0);
      return compact({
        ...res,
        total_size: formatBytes(totalBytes),
        items: a.raw ? redactSecrets(res.items) : res.items.map(summarizeBackup),
        errors,
      });
    },
  });

  defineTool(ctx, "proxmox_get_backup_config", {
    toolset: "backup",
    title: "Show config inside a backup",
    description:
      "Show the guest configuration stored inside a backup archive (cores, memory, disks, network...) without restoring it, e.g. to check what a backup contains or which storage its disks need. Secrets are redacted.",
    input: { volid: volidArg, node: backupNodeArg, raw: z.boolean().optional().describe("Return the config file text as stored (secrets redacted)") },
    handler: async (a) => {
      const node = await nodeForStorage(client, storageOf(a.volid), a.node);
      const text = String((await client.get(`/nodes/${enc(node)}/vzdump/extractconfig`, { volume: a.volid })) ?? "");
      if (a.raw) {
        return {
          volid: a.volid,
          config: text
            .split("\n")
            .map((l) => {
              const m = l.match(/^([^:#[]+):\s*(.*)$/);
              return m && redactSecrets({ [m[1].trim()]: m[2] })[m[1].trim()] !== m[2] ? `${m[1]}: <redacted>` : l;
            })
            .join("\n"),
        };
      }
      const { config, snapshots } = parseConfigText(text);
      const type = backupGuestType(a.volid);
      return compact({ volid: a.volid, type: type === "qemu" ? "vm" : type === "lxc" ? "container" : undefined, config: redactSecrets(config), snapshots });
    },
  });

  defineTool(ctx, "proxmox_restore_backup", {
    toolset: "backup",
    title: "Restore a backup",
    description:
      "Restore a VM or container backup (type detected from the archive) to a new VMID, or over an existing guest with force=true. Overwriting destroys the existing guest's disks and config, needs delete permission, and the guest must be stopped. Use unique=true when restoring a copy alongside the original (new MAC addresses). The target node defaults to the node holding the backup's storage; restoring from local storage only works on that node. Find a free VMID with proxmox_get_next_vmid.",
    write: true,
    destructive: true,
    input: {
      volid: volidArg,
      vmid: vmidArg.describe("VMID to restore to: a free one for a new guest, or the existing one with force=true"),
      node: z.string().optional().describe("Node to restore on (default: the node holding the backup's storage)"),
      type: z.enum(["vm", "container"]).optional().describe("Override the detected guest type"),
      storage: z.string().optional().describe("Storage for the restored disks (default: as in the backup)"),
      force: z.boolean().optional().describe("Overwrite the existing guest with this VMID (destroys its current disks)"),
      unique: z.boolean().optional().describe("Assign new random MAC addresses (use for copies)"),
      start: z.boolean().optional().describe("Start the guest after restoring"),
      pool: z.string().optional().describe("Add the restored guest to this resource pool"),
      bwlimit: z.number().int().min(0).optional().describe("I/O bandwidth limit in KiB/s"),
      live_restore: z.boolean().optional().describe("VMs only: start the VM immediately and restore in the background (PBS backups)"),
      unprivileged: z.boolean().optional().describe("Containers only: restore as unprivileged (default: as in the backup)"),
      extra: extraArg,
      wait: waitArg,
    },
    handler: async (a) => {
      checkHookParams(ctx, a.extra);
      const detected = backupGuestType(a.volid);
      const type = a.type ? (a.type === "vm" ? "qemu" : "lxc") : detected;
      if (!type) throw new ProxmoxError(`Can't tell whether ${a.volid} is a VM or container backup; pass type`);
      if (a.live_restore && type !== "qemu") throw new ProxmoxError("live_restore only applies to VM backups");
      if (a.unprivileged !== undefined && type !== "lxc") throw new ProxmoxError("unprivileged only applies to container backups");
      const existing = (await client.get<Obj[]>("/cluster/resources", { type: "vm" })).find((r) => Number(r.vmid) === a.vmid);
      if (existing && !a.force) {
        throw new ProxmoxError(`VMID ${a.vmid} is in use by ${existing.type === "qemu" ? "VM" : "container"} "${existing.name ?? ""}" on ${existing.node}; pick a free VMID (proxmox_get_next_vmid) or pass force=true to overwrite it`);
      }
      if (a.force && existing) {
        if (!ctx.allowDeletes) throw new ProxmoxError("Overwriting an existing guest destroys it and needs delete permission (PROXMOX_ALLOW_DELETES); restore to a new VMID instead");
        if (existing.type !== type) throw new ProxmoxError(`VMID ${a.vmid} is a ${existing.type === "qemu" ? "VM" : "container"} but the backup is a ${type === "qemu" ? "VM" : "container"}`);
        if (existing.status === "running") throw new ProxmoxError(`Guest ${a.vmid} is running; stop it before restoring over it`);
      }
      const node = a.node ?? (a.force && existing ? existing.node : await nodeForStorage(client, storageOf(a.volid)));
      const common = { vmid: a.vmid, storage: a.storage, force: a.force, unique: a.unique, start: a.start, pool: a.pool, bwlimit: a.bwlimit };
      const params =
        type === "qemu"
          ? defined({ ...common, archive: a.volid, "live-restore": a.live_restore, ...a.extra })
          : defined({ ...common, ostemplate: a.volid, restore: true, unprivileged: a.unprivileged, ...a.extra });
      const upid = await client.post(`/nodes/${enc(node)}/${type}`, params);
      return runTask(client, upid, a.wait, { vmid: a.vmid, node, type: type === "qemu" ? "vm" : "container", volid: a.volid });
    },
  });

  defineTool(ctx, "proxmox_update_backup", {
    toolset: "backup",
    title: "Update backup notes/protection",
    description:
      "Change a backup's notes or protection flag. Protected backups are never pruned and can't be deleted until unprotected; use it to keep a known-good backup.",
    write: true,
    destructive: false,
    idempotent: true,
    input: {
      volid: volidArg,
      node: backupNodeArg,
      notes: z.string().optional().describe("New notes (replaces existing; \"\" clears them)"),
      protected: z.boolean().optional().describe("Protect (true) or unprotect (false) the backup"),
    },
    handler: async (a) => {
      if (a.notes === undefined && a.protected === undefined) throw new ProxmoxError("Nothing to change: pass notes and/or protected");
      const storage = storageOf(a.volid);
      const node = await nodeForStorage(client, storage, a.node);
      await client.put(`/nodes/${enc(node)}/storage/${enc(storage)}/content/${enc(a.volid)}`, defined({ notes: a.notes, protected: a.protected }));
      return compact({ volid: a.volid, notes: a.notes, protected: a.protected, status: "updated" });
    },
  });

  defineTool(ctx, "proxmox_delete_backup", {
    toolset: "backup",
    title: "Delete a backup",
    description: "Permanently delete a backup archive. Protected backups must be unprotected first (proxmox_update_backup). Cannot be undone.",
    delete: true,
    input: { volid: volidArg, node: backupNodeArg, wait: waitArg },
    handler: async (a) => {
      const storage = storageOf(a.volid);
      const node = await nodeForStorage(client, storage, a.node);
      const res = await client.delete(`/nodes/${enc(node)}/storage/${enc(storage)}/content/${enc(a.volid)}`);
      return runTask(client, res, a.wait, { volid: a.volid, node, status: "deleted" });
    },
  });

  const pruneInput = {
    storage: z.string().min(1).describe("Backup storage to prune"),
    node: z.string().optional().describe("Node to access the storage from (needed for node-local storages present on several nodes)"),
    vmid: z.number().int().optional().describe("Only this guest's backups"),
    type: z.enum(["vm", "container"]).optional().describe("Only VM or only container backups"),
    retention: retentionArg.describe("Retention to apply instead of the storage's configured prune-backups, e.g. {keep_last: 3}"),
  };
  const pruneParams = (a: { vmid?: number; type?: "vm" | "container"; retention?: Retention }) =>
    defined({ vmid: a.vmid, type: a.type ? (a.type === "vm" ? "qemu" : "lxc") : undefined, "prune-backups": retentionString(a.retention) });

  defineTool(ctx, "proxmox_preview_backup_prune", {
    toolset: "backup",
    title: "Preview backup pruning",
    description:
      "Dry run of pruning a backup storage: shows which backups the retention rules (the storage's own, or `retention`) would keep and which they would remove. Nothing is deleted; run proxmox_prune_backups with the same arguments to apply.",
    input: pruneInput,
    handler: async (a) => {
      const node = await nodeForStorage(client, a.storage, a.node);
      const list = await client.get<Obj[]>(`/nodes/${enc(node)}/storage/${enc(a.storage)}/prunebackups`, pruneParams(a));
      list.sort((x, y) => Number(y.ctime ?? 0) - Number(x.ctime ?? 0));
      const items = list.map((b) => compact({ volid: b.volid, vmid: b.vmid, type: b.type === "qemu" ? "vm" : b.type === "lxc" ? "container" : b.type, time: isoTime(b.ctime), mark: b.mark }));
      const count = (m: string) => items.filter((i) => i.mark === m).length;
      return {
        storage: a.storage,
        node,
        keep: count("keep"),
        remove: count("remove"),
        protected: count("protected"),
        renamed: count("renamed"),
        would_remove: items.filter((i) => i.mark === "remove").map((i) => i.volid),
        items,
      };
    },
  });

  defineTool(ctx, "proxmox_prune_backups", {
    toolset: "backup",
    title: "Prune backups",
    description:
      "Delete the backups on a storage that the retention rules don't keep (the storage's prune-backups, or `retention`). Permanent; check first with proxmox_preview_backup_prune using the same arguments. Protected backups and archives with non-standard names are never removed.",
    delete: true,
    input: { ...pruneInput, wait: waitArg },
    handler: async (a) => {
      const node = await nodeForStorage(client, a.storage, a.node);
      const res = await client.delete(`/nodes/${enc(node)}/storage/${enc(a.storage)}/prunebackups`, pruneParams(a));
      return runTask(client, res, a.wait, { storage: a.storage, node });
    },
  });

  defineTool(ctx, "proxmox_get_vzdump_defaults", {
    toolset: "backup",
    title: "Get backup defaults",
    description:
      "Show the vzdump defaults a node applies to backups (from /etc/vzdump.conf, merged with a storage's settings when `storage` is given): default storage, mode, compression, retention, bandwidth limits and so on.",
    input: { node: nodeArg, storage: z.string().optional().describe("Include this storage's settings (e.g. its retention)") },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const d = await client.get<Obj>(`/nodes/${enc(node)}/vzdump/defaults`, defined({ storage: a.storage }));
      const out: Obj = redactSecrets({ ...d });
      if (out["prune-backups"] !== undefined) out.retention = parseRetention(out["prune-backups"]);
      return { node, ...out };
    },
  });

  defineTool(ctx, "proxmox_list_backup_files", {
    toolset: "backup",
    title: "Browse files in a PBS backup",
    description:
      "Browse the files inside a Proxmox Backup Server backup (single-file restore): start at path \"/\" to see the archives (disk images/filesystem archives), then drill into directories. Only works for PBS storages. Opening VM disk images starts a temporary restore VM on the node and can take a while on first access.",
    input: {
      storage: z.string().min(1).describe("PBS storage ID"),
      volid: volidArg,
      path: z.string().optional().describe("Path to list, as shown in earlier results (default \"/\")"),
      node: z.string().optional().describe("Node to run the file restore on (default: any node with the storage)"),
    },
    handler: async (a) => {
      const node = await nodeForStorage(client, a.storage, a.node);
      const path = a.path && a.path !== "/" ? a.path : "/";
      const filepath = path === "/" ? "/" : Buffer.from(path).toString("base64");
      const entries = await client.get<Obj[]>(`/nodes/${enc(node)}/storage/${enc(a.storage)}/file-restore/list`, { volume: a.volid, filepath });
      return {
        volid: a.volid,
        path,
        entries: entries.map((e) =>
          compact({
            name: e.text,
            path: e.filepath ? Buffer.from(String(e.filepath), "base64").toString() : undefined,
            type: e.type,
            directory: e.leaf ? undefined : true,
            size: e.leaf ? formatBytes(e.size) : undefined,
            mtime: isoTime(e.mtime),
          }),
        ),
      };
    },
  });
}
