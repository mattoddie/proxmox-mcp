import { z } from "zod";
import { redactSecrets } from "../proxmox/format.js";
import { defineTool, type ToolContext } from "./util.js";

/**
 * API paths that run commands or read/write files in guests or on nodes; gated by PROXMOX_ALLOW_EXEC.
 * Checked for every method, since agent file-read and exec-status are GETs.
 */
const EXEC_PATH_RE = /\/agent\/(exec|exec-status|file-read|file-write|set-user-password)\b|\/(termproxy|vncshell|spiceshell|vncproxy|spiceproxy|mtunnel|monitor)\b|\/nodes\/[^/]+\/execute\b/;

export function registerRawTools(ctx: ToolContext): void {
  const { client } = ctx;

  defineTool(ctx, "proxmox_api_request", {
    toolset: "raw",
    title: "Raw Proxmox API request",
    description:
      "Call any Proxmox VE API endpoint directly, for anything the other tools don't cover. `path` is relative to /api2/json, e.g. /nodes/pve1/qemu/100/firewall/options. Parameters go in the query string for GET/DELETE and in the form body for POST/PUT. " +
      (!ctx.allowWrites
        ? "Only GET is allowed because write access is disabled (PROXMOX_ALLOW_WRITES=false)."
        : ctx.allowDeletes
          ? "All HTTP methods are allowed."
          : "DELETE is not allowed because delete access is disabled (PROXMOX_ALLOW_DELETES=false).") +
      (ctx.allowExec ? "" : " Command execution and guest file endpoints (guest agent exec/file-read/file-write, shells, monitor) are blocked (PROXMOX_ALLOW_EXEC=false)."),
    readOnly: !ctx.allowWrites,
    destructive: ctx.allowWrites,
    input: {
      method: z.enum(["GET", "POST", "PUT", "DELETE"]).optional().describe("HTTP method (default GET)"),
      path: z.string().describe("Path relative to /api2/json, starting with /"),
      params: z
        .record(z.string(), z.union([z.string(), z.number(), z.boolean(), z.array(z.union([z.string(), z.number()]))]))
        .optional()
        .describe("API parameters (booleans are sent as 1/0, arrays as repeated keys)"),
      redact: z.boolean().optional().describe("Redact password/secret/key fields in the response (default true)"),
    },
    handler: async (a) => {
      const method = a.method ?? "GET";
      if (method !== "GET" && !ctx.allowWrites) {
        throw new Error("Write access is disabled. Set PROXMOX_ALLOW_WRITES=true to allow non-GET requests.");
      }
      if (method === "DELETE" && !ctx.allowDeletes) {
        throw new Error("Delete access is disabled. Set PROXMOX_ALLOW_DELETES=true to allow DELETE requests.");
      }
      // Check the decoded path: URL parsing turns %2e%2e into "..", drops "./" segments and treats "\" as "/",
      // which could otherwise sneak past the checks below.
      let decoded: string;
      try {
        decoded = decodeURIComponent(a.path);
      } catch {
        throw new Error("path is not validly percent-encoded");
      }
      if (!decoded.startsWith("/") || decoded.includes("..") || /[?#\\]/.test(decoded) || /\/\.(\/|$)/.test(decoded)) {
        throw new Error("path must be absolute, must not contain '..' or './' segments and must not include a query string (use params)");
      }
      if (EXEC_PATH_RE.test(decoded.replace(/\/{2,}/g, "/")) && !ctx.allowExec) {
        throw new Error("This endpoint runs commands or reads/writes files in a guest or on a node. Set PROXMOX_ALLOW_EXEC=true to allow it.");
      }
      const data = await client.request(method, a.path.replace(/^\/api2\/json/, ""), a.params);
      return a.redact === false ? data : redactSecrets(data);
    },
  });
}
