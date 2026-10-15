import { z } from "zod";
import { redactSecrets } from "../proxmox/format.js";
import { defineTool, type ToolContext } from "./util.js";

/**
 * API paths that run commands, open consoles, send input or read/write files in guests or on nodes; gated by
 * PROXMOX_ALLOW_EXEC. Checked for every method, since agent file-read and exec-status are GETs. This covers every
 * guest agent endpoint, consoles and shells (termproxy, vncshell, spiceshell, vncproxy, spiceproxy, vncwebsocket),
 * migration tunnels, the QEMU monitor, sendkey (keystrokes into a console) and the node's bulk `execute`.
 */
const EXEC_PATH_RE =
  /\/agent\b|\/(termproxy|vncshell|spiceshell|vncproxy|spiceproxy|vncwebsocket|mtunnel|mtunnelwebsocket|monitor|sendkey)\b|\/nodes\/[^/]+\/execute\b/i;

/** ASCII control characters: URL parsing silently strips tab, CR and LF, so they could hide a blocked path. */
const CONTROL_CHAR_RE = /[\x00-\x1f\x7f]/;

/** Throws unless `p` (a decoded path relative to /api2/json) is a plain absolute path the exec check can trust. */
function checkPath(p: string): void {
  if (CONTROL_CHAR_RE.test(p)) throw new Error("path must not contain control characters (tab, newline, NUL...)");
  if (!p.startsWith("/") || p.includes("..") || /[?#\\]/.test(p) || /\/\.(\/|$)/.test(p)) {
    throw new Error("path must be absolute, must not contain '..' or './' segments and must not include a query string (use params)");
  }
}

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
      (ctx.allowExec ? "" : " Command execution, console and guest file endpoints (all guest agent endpoints, shells and consoles, monitor, sendkey) are blocked (PROXMOX_ALLOW_EXEC=false)."),
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
      // Check the decoded path: URL parsing turns %2e%2e into "..", drops "./" segments, treats "\" as "/" and strips
      // tabs and newlines, which could otherwise sneak past the checks below.
      if (CONTROL_CHAR_RE.test(a.path)) throw new Error("path must not contain control characters (tab, newline, NUL...)");
      const path = a.path.replace(/^\/api2\/json(?=\/)/, "");
      let decoded: string;
      try {
        decoded = decodeURIComponent(path);
      } catch {
        throw new Error("path is not validly percent-encoded");
      }
      checkPath(decoded);
      // Then check what will actually be sent: the path as URL parsing normalises it, decoded again.
      const base = client.apiUrl("/").pathname;
      let sent: string;
      try {
        sent = decodeURIComponent(client.apiUrl(path).pathname);
      } catch {
        throw new Error("path is not validly percent-encoded");
      }
      if (!sent.startsWith(base)) throw new Error("path must stay under /api2/json");
      sent = sent.slice(base.length - 1);
      checkPath(sent);
      const blocked = [decoded, sent].some((p) => EXEC_PATH_RE.test(p.replace(/\/{2,}/g, "/")));
      if (blocked && !ctx.allowExec) {
        throw new Error(
          "This endpoint runs commands, opens a console, sends input or reads/writes files in a guest or on a node. Set PROXMOX_ALLOW_EXEC=true to allow it.",
        );
      }
      const data = await client.request(method, path, a.params);
      // ACME plugin `data` is a KEY=value blob of DNS API credentials, which the field-name check can't see.
      return a.redact === false ? data : redactSecrets(data, { acmePluginData: /^\/cluster\/acme\/plugins\b/i.test(sent) });
    },
  });
}
