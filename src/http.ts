import { timingSafeEqual } from "node:crypto";
import { createServer as createHttpServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { AUTH_TOKEN_REQUIRED, type Config } from "./config.js";

const MAX_BODY_BYTES = 4 * 1024 * 1024;

function sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  res.writeHead(status, { "Content-Type": "application/json", ...headers }).end(JSON.stringify(body));
}

function rpcError(res: ServerResponse, status: number, message: string, headers?: Record<string, string>): void {
  sendJson(res, status, { jsonrpc: "2.0", error: { code: -32000, message }, id: null }, headers);
}

function readBody(req: IncomingMessage): Promise<unknown> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    req.on("data", (chunk: Buffer) => {
      size += chunk.length;
      if (size > MAX_BODY_BYTES) {
        reject(new Error("Request body too large"));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on("end", () => {
      const text = Buffer.concat(chunks).toString("utf8");
      if (!text) return resolve(undefined);
      try {
        resolve(JSON.parse(text));
      } catch {
        reject(new Error("Invalid JSON body"));
      }
    });
    req.on("error", reject);
  });
}

/** True when `host` (a Host header value or an Origin's host, lower-cased) or its hostname is allowed. */
function hostAllowed(allowedHosts: string[], host: string): boolean {
  return allowedHosts.includes(host) || allowedHosts.includes(host.replace(/:\d+$/, ""));
}

/** An Origin header's host[:port], lower-cased, or undefined for "null" or an unparsable origin. */
function originHost(origin: string): string | undefined {
  try {
    return new URL(origin).host.toLowerCase() || undefined;
  } catch {
    return undefined;
  }
}

function tokenMatches(header: string | undefined, token: string): boolean {
  const presented = header?.match(/^Bearer\s+(.+)$/i)?.[1] ?? "";
  const a = Buffer.from(presented);
  const b = Buffer.from(token);
  return a.length === b.length && timingSafeEqual(a, b);
}

/**
 * Stateless Streamable HTTP transport: every POST to /mcp gets a fresh
 * server + transport, so the container can be restarted or scaled freely.
 */
export function startHttpServer(cfg: Config, makeServer: () => McpServer): Promise<Server> {
  const { host, port, authToken, allowUnauthenticated, allowedHosts } = cfg.http;
  if (!authToken && !allowUnauthenticated) return Promise.reject(new Error(AUTH_TOKEN_REQUIRED));

  const httpServer = createHttpServer(async (req, res) => {
    const url = new URL(req.url ?? "/", "http://localhost");

    if (url.pathname === "/health" || url.pathname === "/healthz") {
      return sendJson(res, 200, { status: "ok" });
    }
    if (url.pathname !== "/mcp") {
      return sendJson(res, 404, { error: "Not found. The MCP endpoint is /mcp" });
    }

    if (allowedHosts) {
      const hostHeader = (req.headers.host ?? "").toLowerCase();
      if (!hostAllowed(allowedHosts, hostHeader)) {
        return rpcError(res, 403, `Host ${hostHeader} is not allowed`);
      }
      // Browsers send Origin on cross-site requests; refuse pages served from other hosts (DNS rebinding, CSRF).
      const origin = req.headers.origin;
      if (origin !== undefined) {
        const oh = originHost(origin);
        if (!oh || !hostAllowed(allowedHosts, oh)) return rpcError(res, 403, `Origin ${origin} is not allowed`);
      }
    }
    if (authToken && !tokenMatches(req.headers.authorization, authToken)) {
      return rpcError(res, 401, "Unauthorized", { "WWW-Authenticate": "Bearer" });
    }

    if (req.method !== "POST") {
      // Stateless mode: no server-initiated SSE stream and no sessions to delete.
      return rpcError(res, 405, "Method not allowed", { Allow: "POST" });
    }

    let body: unknown;
    try {
      body = await readBody(req);
    } catch (err) {
      return rpcError(res, 400, (err as Error).message);
    }

    const server = makeServer();
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
    res.on("close", () => {
      void transport.close();
      void server.close();
    });
    try {
      await server.connect(transport);
      await transport.handleRequest(req, res, body);
    } catch (err) {
      console.error("Error handling MCP request:", err);
      if (!res.headersSent) rpcError(res, 500, "Internal server error");
    }
  });

  return new Promise((resolve, reject) => {
    httpServer.once("error", reject);
    httpServer.listen(port, host, () => {
      console.error(`proxmox-mcp listening on http://${host}:${port}/mcp${authToken ? " (bearer auth enabled)" : ""}`);
      if (!authToken) {
        console.error(
          "WARNING: MCP_ALLOW_UNAUTHENTICATED=true: anyone who can reach this port can use every enabled tool. Only run it like this behind something else that authenticates.",
        );
      }
      resolve(httpServer);
    });
  });
}
