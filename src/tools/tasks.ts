import { z } from "zod";
import { parseUpid } from "../proxmox/client.js";
import { compact, isoTime } from "../proxmox/format.js";
import { defined, defineTool, enc, filterAndLimit, limitArg, searchArg, type ToolContext } from "./util.js";

type Obj = Record<string, any>;

const upidArg = z.string().describe("Task ID (UPID), e.g. UPID:pve1:000A1B2C:0123ABCD:6701F3A2:qmstart:100:root@pam:");

function summarizeTask(t: Obj): Obj {
  const start = Number(t.starttime);
  const end = Number(t.endtime);
  return compact({
    upid: t.upid,
    node: t.node,
    type: t.type,
    id: t.id || undefined,
    user: t.user,
    status: t.status ?? (t.endtime ? undefined : "running"),
    starttime: isoTime(start),
    endtime: isoTime(end),
    duration_s: Number.isFinite(end) && end >= start ? end - start : undefined,
  });
}

export function registerTasksTools(ctx: ToolContext): void {
  const { client } = ctx;

  defineTool(ctx, "proxmox_list_tasks", {
    toolset: "tasks",
    title: "List tasks",
    description:
      "List Proxmox tasks (backups, migrations, starts, clones...). Without `node`, lists recent tasks across the whole cluster; with `node`, reads that node's task history with server-side filters. Use errors=true to find failures.",
    input: {
      node: z.string().optional().describe("Only this node, with full history and server-side filters (default: recent tasks cluster-wide)"),
      vmid: z.number().int().optional().describe("Only tasks for this VM/container"),
      type: z.string().optional().describe("Only this task type, e.g. vzdump, qmstart, qmigrate, vzcreate, qmclone"),
      user: z.string().optional().describe("Only tasks started by this user, e.g. root@pam"),
      errors: z.boolean().optional().describe("Only failed tasks"),
      running: z.boolean().optional().describe("Only tasks that are still running"),
      since: z.number().int().optional().describe("Only tasks started after this UNIX time (node only)"),
      search: searchArg,
      limit: limitArg(50),
    },
    handler: async (a) => {
      let tasks: Obj[];
      if (a.node) {
        tasks = await client.get<Obj[]>(
          `/nodes/${enc(a.node)}/tasks`,
          defined({
            vmid: a.vmid,
            typefilter: a.type,
            userfilter: a.user,
            errors: a.errors,
            since: a.since,
            source: a.running ? "active" : "all",
            limit: a.limit ?? 50,
          }),
        );
      } else {
        tasks = await client.get<Obj[]>("/cluster/tasks");
        if (a.vmid !== undefined) tasks = tasks.filter((t) => String(t.id) === String(a.vmid));
        if (a.type) tasks = tasks.filter((t) => t.type === a.type);
        if (a.user) tasks = tasks.filter((t) => t.user === a.user);
        if (a.errors) tasks = tasks.filter((t) => t.endtime && t.status && t.status !== "OK" && !/^WARNINGS/.test(t.status));
      }
      if (a.running) tasks = tasks.filter((t) => !t.endtime);
      tasks.sort((x, y) => Number(y.starttime) - Number(x.starttime));
      const res = filterAndLimit(tasks, { search: a.search, limit: a.limit ?? 50 }, (t) => [t.type, t.id, t.user, t.status, t.node]);
      return { ...res, items: res.items.map(summarizeTask) };
    },
  });

  defineTool(ctx, "proxmox_get_task", {
    toolset: "tasks",
    title: "Get task status",
    description: "Get a task's status (running/stopped), exit status and the last lines of its log.",
    input: {
      upid: upidArg,
      log_lines: z.number().int().min(0).max(1000).optional().describe("How many trailing log lines to include (default 30)"),
    },
    handler: async (a) => client.getTask(a.upid, a.log_lines ?? 30),
  });

  defineTool(ctx, "proxmox_get_task_log", {
    toolset: "tasks",
    title: "Read task log",
    description: "Read a task's log, paging from the start (or the end with tail=true).",
    input: {
      upid: upidArg,
      start: z.number().int().min(0).optional().describe("First line (0-based, default 0)"),
      limit: z.number().int().positive().max(5000).optional().describe("Number of lines (default 500)"),
      tail: z.boolean().optional().describe("Return the last `limit` lines instead"),
    },
    handler: async (a) => {
      const { node } = parseUpid(a.upid);
      const path = `/nodes/${enc(node)}/tasks/${enc(a.upid)}/log`;
      const limit = a.limit ?? 500;
      let start = a.start ?? 0;
      if (a.tail) {
        const first = await client.get<Obj[]>(path, { start: 0, limit: 1 });
        const total = Number((first as any).total ?? 0);
        start = Math.max(0, total - limit);
      }
      const lines = await client.get<Obj[]>(path, { start, limit });
      return { start, total: (lines as any).total, lines: lines.map((l) => l.t) };
    },
  });

  defineTool(ctx, "proxmox_wait_for_task", {
    toolset: "tasks",
    title: "Wait for a task",
    description: "Wait until a running task finishes (or the timeout passes) and return its outcome and log tail.",
    input: {
      upid: upidArg,
      timeout_s: z.number().int().positive().max(3600).optional().describe("How long to wait in seconds (default PROXMOX_TASK_TIMEOUT_MS)"),
    },
    handler: async (a) => client.waitForTask(a.upid, { timeoutMs: a.timeout_s ? a.timeout_s * 1000 : undefined }),
  });

  defineTool(ctx, "proxmox_stop_task", {
    toolset: "tasks",
    title: "Stop a task",
    description: "Abort a running task (e.g. a stuck backup or migration).",
    write: true,
    destructive: true,
    input: { upid: upidArg },
    handler: async (a) => {
      const { node } = parseUpid(a.upid);
      await client.delete(`/nodes/${enc(node)}/tasks/${enc(a.upid)}`);
      return { stopped: a.upid };
    },
  });
}
