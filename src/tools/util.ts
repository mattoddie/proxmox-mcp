import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";
import type { ToolOptions, Toolset } from "../config.js";
import { isUpid, type ProxmoxClient } from "../proxmox/client.js";
import { compact } from "../proxmox/format.js";

export interface ToolContext extends ToolOptions {
  server: McpServer;
  client: ProxmoxClient;
}

type Obj = Record<string, any>;

// ---------------------------------------------------------------- common arguments

export const nodeArg = z
  .string()
  .optional()
  .describe("Node name. Defaults to PROXMOX_DEFAULT_NODE, or the only node on a single-node install. See proxmox_list_nodes.");

export const nodeRequiredArg = z.string().describe("Node name (see proxmox_list_nodes)");

/** For tools that act on a guest: the node is looked up from the VMID when omitted. */
export const guestNodeArg = z
  .string()
  .optional()
  .describe("Node the guest is on. Looked up automatically from the VMID when omitted.");

export const vmidArg = z.number().int().min(100).max(999999999).describe("VM or container ID (VMID), e.g. 100");

export const waitArg = z
  .boolean()
  .optional()
  .describe(
    "Wait for the Proxmox task to finish and return its result and log (default true, up to PROXMOX_TASK_TIMEOUT_MS). With false the task ID (UPID) is returned immediately; follow it with proxmox_get_task.",
  );

export const searchArg = z.string().optional().describe("Case-insensitive substring filter on names, IDs, tags and similar fields");

export const limitArg = (def: number) =>
  z.number().int().positive().max(10000).optional().describe(`Maximum number of results (default ${def})`);

export const rawArg = z.boolean().optional().describe("Return the full unprocessed objects from the API (larger)");

/**
 * Free-form extra parameters passed to the API call, for options the typed
 * arguments don't cover. Names are Proxmox's own (see the API viewer).
 */
export const extraArg = z
  .record(z.string(), z.union([z.string(), z.number(), z.boolean()]))
  .optional()
  .describe("Additional raw Proxmox API parameters (advanced; names as in the Proxmox API viewer). Merged last, so they override typed arguments.");

/** Config keys to remove, sent as Proxmox's `delete` parameter. */
export const deleteKeysArg = z
  .array(z.string())
  .optional()
  .describe("Config keys to remove/reset to default, e.g. [\"net1\", \"description\"] (sent as the API's `delete` parameter)");

export const digestArg = z
  .string()
  .optional()
  .describe("Only apply the change if the current config digest matches (optimistic locking; from the matching get tool)");

// ---------------------------------------------------------------- results

export function ok(data: unknown): CallToolResult {
  return { content: [{ type: "text", text: JSON.stringify(data, null, 2) ?? "null" }] };
}

export function fail(err: unknown): CallToolResult {
  const message = err instanceof Error ? err.message : String(err);
  return { content: [{ type: "text", text: `Error: ${message}` }], isError: true };
}

// ---------------------------------------------------------------- tool registration

type Shape = z.ZodRawShape;

interface ToolDef<S extends Shape> {
  /** Toolset the tool belongs to; only registered when the toolset is enabled (PROXMOX_TOOLSETS). */
  toolset: Toolset;
  title: string;
  description: string;
  input: S;
  /** true for tools that change state; only registered when writes are enabled. */
  write?: boolean;
  /** true for tools that delete guests or configuration; only registered when deletes are enabled too. */
  delete?: boolean;
  /** true for tools that run commands or write files in guests/nodes; only registered when exec is enabled too. */
  exec?: boolean;
  destructive?: boolean;
  /** true when repeating the call with the same arguments has no further effect. */
  idempotent?: boolean;
  /** Override the readOnlyHint annotation (defaults to !write). */
  readOnly?: boolean;
  handler: (args: z.infer<z.ZodObject<S>>) => Promise<unknown>;
}

export function defineTool<S extends Shape>(ctx: ToolContext, name: string, def: ToolDef<S>): void {
  if (!ctx.toolsets.has(def.toolset)) return;
  const write = def.write || def.delete || def.exec;
  if (write && !ctx.allowWrites) return;
  if (def.delete && !ctx.allowDeletes) return;
  if (def.exec && !ctx.allowExec) return;
  const readOnly = def.readOnly ?? !write;
  ctx.server.registerTool(
    name,
    {
      title: def.title,
      description: def.description,
      inputSchema: def.input,
      annotations: {
        title: def.title,
        readOnlyHint: readOnly,
        destructiveHint: readOnly ? undefined : (def.destructive ?? Boolean(def.delete || def.exec)),
        idempotentHint: def.idempotent,
        openWorldHint: false,
      },
    },
    (async (args: z.infer<z.ZodObject<S>>) => {
      try {
        return ok(await def.handler(args));
      } catch (err) {
        return fail(err);
      }
    }) as any,
  );
}

// ---------------------------------------------------------------- helpers

export function filterAndLimit<T>(items: T[], opts: { search?: string; limit?: number }, fields: (item: T) => unknown[]): {
  total: number;
  returned: number;
  items: T[];
} {
  let out = items;
  if (opts.search) {
    const q = opts.search.toLowerCase();
    out = out.filter((item) => fields(item).some((f) => f !== undefined && f !== null && String(f).toLowerCase().includes(q)));
  }
  const total = out.length;
  if (opts.limit !== undefined) out = out.slice(0, opts.limit);
  return { total, returned: out.length, items: out };
}

/** Copy only the keys whose value is not undefined (lets "" and false through, unlike compact()). */
export function defined(obj: Record<string, unknown>): Record<string, unknown> {
  return Object.fromEntries(Object.entries(obj).filter(([, v]) => v !== undefined));
}

/** Joins a list of config keys for Proxmox's comma-separated `delete` parameter. */
export function deleteParam(keys?: string[]): string | undefined {
  return keys?.length ? keys.join(",") : undefined;
}

export const enc = encodeURIComponent;

/**
 * Handles an API result that may be a task ID (UPID). By default waits for
 * the task, throwing with the log tail if it fails; with wait=false returns
 * the UPID straight away. Non-UPID results are passed through as `result`.
 */
export async function runTask(client: ProxmoxClient, result: unknown, wait: boolean | undefined, extra: Obj = {}): Promise<Obj> {
  if (!isUpid(result)) return compact({ ...extra, result });
  if (wait === false) return { ...extra, upid: result, status: "started", note: "Follow progress with proxmox_get_task" };
  const task = await client.waitForTask(result);
  if (task.timedOut) {
    return {
      ...extra,
      status: "running",
      note: `Still running after ${Math.round(client.taskTimeoutMs / 1000)}s; check with proxmox_get_task`,
      task: compact(task),
    };
  }
  if (!task.ok) {
    const tail = task.log?.slice(-15).join("\n");
    throw new Error(`Task ${task.type ?? ""} ${task.id ?? ""} failed: ${task.exitstatus ?? "unknown error"}${tail ? `\n--- task log (tail) ---\n${tail}` : ""}\n(UPID ${task.upid})`);
  }
  return { ...extra, status: "ok", task: compact(task) };
}
