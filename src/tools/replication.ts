import { z } from "zod";
import type { ProxmoxClient } from "../proxmox/client.js";
import { compact, flag, isoTime } from "../proxmox/format.js";
import { defined, defineTool, deleteKeysArg, deleteParam, digestArg, enc, extraArg, rawArg, runTask, waitArg, type ToolContext } from "./util.js";

type Obj = Record<string, any>;

const JOB_ID_RE = /^[1-9][0-9]{2,8}-\d{1,9}$/;

const jobIdArg = z
  .string()
  .regex(JOB_ID_RE, "Job IDs look like <VMID>-<number>, e.g. 100-0")
  .describe("Replication job ID: <guest VMID>-<job number>, e.g. \"100-0\" (see proxmox_list_replication_jobs)");

const jobNodeArg = z
  .string()
  .optional()
  .describe("Node the guest currently runs on (the replication source). Looked up from the guest when omitted.");

/** Status fields from /nodes/{node}/replication, summarized. */
function summarizeStatus(s: Obj | undefined): Obj | undefined {
  if (!s) return undefined;
  return compact({
    source: s.source,
    state: s.pid ? "running" : s.error ? "error" : Number(s.fail_count) > 0 ? "failing" : s.last_sync ? "ok" : "pending",
    last_sync: isoTime(s.last_sync),
    last_try: isoTime(s.last_try),
    next_sync: isoTime(s.next_sync),
    duration_s: s.duration !== undefined ? Math.round(Number(s.duration) * 10) / 10 : undefined,
    fail_count: Number(s.fail_count) || undefined,
    error: s.error,
  });
}

function summarizeJob(j: Obj, status?: Obj): Obj {
  return compact({
    id: j.id,
    guest: j.guest !== undefined ? Number(j.guest) : Number(String(j.id).split("-")[0]),
    target: j.target,
    schedule: j.schedule ?? "*/15",
    rate_mbps: j.rate !== undefined ? Number(j.rate) : undefined,
    disabled: flag(j.disable) || undefined,
    remove_job: j.remove_job,
    comment: j.comment,
    status: summarizeStatus(status),
  });
}

/** Map of VMID → node from /cluster/resources. */
async function guestNodes(client: ProxmoxClient): Promise<Map<number, string>> {
  const res = await client.get<Obj[]>("/cluster/resources", { type: "vm" }).catch(() => [] as Obj[]);
  return new Map(res.map((r) => [Number(r.vmid), String(r.node)]));
}

async function sourceNode(client: ProxmoxClient, id: string, node?: string): Promise<string> {
  if (node) return node;
  const vmid = Number(id.split("-")[0]);
  const g = await client.resolveGuest(vmid);
  return g.node;
}

export function registerReplicationTools(ctx: ToolContext): void {
  const { client } = ctx;

  defineTool(ctx, "proxmox_list_replication_jobs", {
    toolset: "replication",
    title: "List replication jobs",
    description:
      "List storage replication jobs (ZFS-based guest replication to another node) with guest, target, schedule, rate limit and live status from the source node: last/next sync, duration, failure count and last error.",
    input: {
      guest: z.number().int().optional().describe("Only jobs for this guest VMID"),
      target: z.string().optional().describe("Only jobs replicating to this node"),
      raw: rawArg,
    },
    handler: async (a) => {
      let jobs = await client.get<Obj[]>("/cluster/replication");
      if (a.guest !== undefined) jobs = jobs.filter((j) => Number(j.guest ?? String(j.id).split("-")[0]) === a.guest);
      if (a.target) jobs = jobs.filter((j) => j.target === a.target);
      jobs.sort((x, y) => String(x.id).localeCompare(String(y.id), undefined, { numeric: true }));
      if (!jobs.length) return { total: 0, items: [] };

      // Status lives on the node the guest currently runs on.
      const nodes = await guestNodes(client);
      const sourceOf = (j: Obj) => nodes.get(Number(j.guest ?? String(j.id).split("-")[0])) ?? j.source;
      const unique = [...new Set(jobs.map(sourceOf).filter(Boolean))] as string[];
      const statusLists = await Promise.all(
        unique.map((n) => client.get<Obj[]>(`/nodes/${enc(n)}/replication`).catch(() => [] as Obj[])),
      );
      const status = new Map<string, Obj>();
      unique.forEach((n, i) => statusLists[i].forEach((s) => status.set(String(s.id), { source: n, ...s })));
      return {
        total: jobs.length,
        items: jobs.map((j) => (a.raw ? { ...j, status: status.get(j.id) } : summarizeJob(j, status.get(j.id)))),
      };
    },
  });

  defineTool(ctx, "proxmox_get_replication_job", {
    toolset: "replication",
    title: "Get replication job",
    description: "Get one replication job's configuration (with digest for safe updates) and its current status on the source node.",
    input: { id: jobIdArg, node: jobNodeArg },
    handler: async (a) => {
      const job = await client.get<Obj>(`/cluster/replication/${enc(a.id)}`);
      let status: Obj | undefined;
      let statusError: string | undefined;
      try {
        const node = await sourceNode(client, a.id, a.node);
        status = { source: node, ...(await client.get<Obj>(`/nodes/${enc(node)}/replication/${enc(a.id)}/status`)) };
      } catch (err) {
        statusError = (err as Error).message;
      }
      return compact({ ...summarizeJob(job, status), digest: job.digest, status_error: statusError });
    },
  });

  defineTool(ctx, "proxmox_save_replication_job", {
    toolset: "replication",
    title: "Create or update replication job",
    description:
      "Create a replication job for a guest (pass `guest` and `target`; the job number is picked automatically) or update an existing one (pass `id`). Replication needs the guest's disks on ZFS storage that exists on both nodes. Schedules use systemd calendar syntax, e.g. \"*/15\" (every 15 min, default), \"*/5\", \"hourly\", \"22:30\".",
    input: {
      id: jobIdArg.optional().describe("Job ID to update, or to create with a specific job number, e.g. \"100-0\""),
      guest: z.number().int().min(100).optional().describe("Guest VMID (to create a job without picking the job number)"),
      target: z.string().optional().describe("Target node (required when creating; can't be changed afterwards)"),
      schedule: z.string().optional().describe("Schedule in systemd calendar-event format (default \"*/15\")"),
      rate: z.number().positive().optional().describe("Bandwidth limit in MB/s"),
      comment: z.string().optional().describe("Description"),
      disable: z.boolean().optional().describe("Disable (true) or enable (false) the job"),
      delete: deleteKeysArg,
      digest: digestArg,
      extra: extraArg,
    },
    write: true,
    destructive: false,
    idempotent: true,
    handler: async (a) => {
      const jobs = await client.get<Obj[]>("/cluster/replication");
      const params = defined({ schedule: a.schedule, rate: a.rate, comment: a.comment, disable: a.disable, ...a.extra });
      const existing = a.id ? jobs.find((j) => j.id === a.id) : undefined;
      if (existing) {
        if (a.target && a.target !== existing.target) throw new Error(`Job ${a.id} replicates to ${existing.target}; the target can't be changed (create a new job)`);
        await client.put(`/cluster/replication/${enc(a.id!)}`, { ...params, delete: deleteParam(a.delete), digest: a.digest });
        return { id: a.id, action: "updated" };
      }
      if (!a.target) throw new Error(a.id ? `No job ${a.id}; pass \`target\` to create it` : "Pass `target` (and `guest`) to create a job");
      let id = a.id;
      if (!id) {
        if (a.guest === undefined) throw new Error("Pass `guest` (or a full `id`) to create a job");
        const used = jobs.filter((j) => String(j.id).startsWith(`${a.guest}-`)).map((j) => Number(String(j.id).split("-")[1]));
        id = `${a.guest}-${used.length ? Math.max(...used) + 1 : 0}`;
      } else if (a.guest !== undefined && !id.startsWith(`${a.guest}-`)) {
        throw new Error(`Job ID ${id} doesn't belong to guest ${a.guest}`);
      }
      await client.post("/cluster/replication", { id, type: "local", target: a.target, ...params });
      return { id, action: "created", target: a.target };
    },
  });

  defineTool(ctx, "proxmox_delete_replication_job", {
    toolset: "replication",
    title: "Delete replication job",
    description:
      "Mark a replication job for removal. The job then removes its replication snapshots (and, unless keep is set, the replicated volumes on the target) and deletes itself on its next run. force removes only the config entry without cleanup.",
    input: {
      id: jobIdArg,
      keep: z.boolean().optional().describe("Keep the replicated data on the target node"),
      force: z.boolean().optional().describe("Remove the job config immediately without any cleanup"),
    },
    delete: true,
    handler: async (a) => {
      await client.delete(`/cluster/replication/${enc(a.id)}`, defined({ keep: a.keep, force: a.force }));
      return compact({ id: a.id, status: a.force ? "removed" : "marked for removal", keep: a.keep });
    },
  });

  defineTool(ctx, "proxmox_run_replication_now", {
    toolset: "replication",
    title: "Run replication now",
    description: "Schedule a replication job to run as soon as possible instead of waiting for its schedule. Check the result with proxmox_get_replication_job or proxmox_get_replication_log.",
    input: { id: jobIdArg, node: jobNodeArg, wait: waitArg },
    write: true,
    destructive: false,
    handler: async (a) => {
      const node = await sourceNode(client, a.id, a.node);
      const res = await client.post(`/nodes/${enc(node)}/replication/${enc(a.id)}/schedule_now`);
      return runTask(client, res, a.wait, { id: a.id, node, status: "scheduled" });
    },
  });

  defineTool(ctx, "proxmox_get_replication_log", {
    toolset: "replication",
    title: "Get replication log",
    description: "Read the log of a replication job's last run from the source node, e.g. to diagnose a failing job.",
    input: {
      id: jobIdArg,
      node: jobNodeArg,
      start: z.number().int().min(0).optional().describe("First line to return (default 0)"),
      limit: z.number().int().positive().max(5000).optional().describe("Maximum lines (default 500)"),
    },
    handler: async (a) => {
      const node = await sourceNode(client, a.id, a.node);
      const lines = await client.get<Obj[]>(`/nodes/${enc(node)}/replication/${enc(a.id)}/log`, { start: a.start ?? 0, limit: a.limit ?? 500 });
      const total = (lines as any).total;
      return compact({ id: a.id, node, total_lines: total, lines: lines.map((l) => String(l.t ?? "")).filter((t) => t !== "no content") });
    },
  });
}
