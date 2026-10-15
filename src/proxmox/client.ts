import { rootCertificates, type ConnectionOptions, type TLSSocket } from "node:tls";
import { Agent, buildConnector, fetch, type Response } from "undici";
import type { ProxmoxConfig } from "../config.js";

export class ProxmoxError extends Error {
  constructor(
    message: string,
    readonly status?: number,
  ) {
    super(message);
    this.name = "ProxmoxError";
  }
}

export type HttpMethod = "GET" | "POST" | "PUT" | "DELETE";

/** Request parameters. undefined values are dropped, booleans become 1/0 and arrays repeat the key. */
export type Params = Record<string, unknown>;

export type GuestType = "qemu" | "lxc";

export interface GuestRef {
  vmid: number;
  node: string;
  type: GuestType;
  name?: string;
  status?: string;
  template?: boolean;
}

export interface TaskResult {
  upid: string;
  node: string;
  type?: string;
  id?: string;
  user?: string;
  /** "running" or "stopped". */
  status: string;
  /** "OK", "WARNINGS: n" or an error message once stopped. */
  exitstatus?: string;
  ok: boolean;
  /** True when the task was still running when we stopped waiting. */
  timedOut?: boolean;
  starttime?: string;
  endtime?: string;
  duration_s?: number;
  /** Last lines of the task log. */
  log?: string[];
}

/** Ticket lifetime is 2h; renew well before that. */
const TICKET_MAX_AGE_MS = 90 * 60 * 1000;

const UPID_RE = /^UPID:([^:]+):[0-9A-Fa-f]+:[0-9A-Fa-f]+:([0-9A-Fa-f]+):([^:]*):([^:]*):([^:]*):/;

export function isUpid(value: unknown): value is string {
  return typeof value === "string" && UPID_RE.test(value);
}

/** Splits a UPID into its parts: node, start time, task type, object id and user. */
export function parseUpid(upid: string): { node: string; starttime: number; type: string; id: string; user: string } {
  const m = upid.match(UPID_RE);
  if (!m) throw new ProxmoxError(`Not a task id (UPID): ${upid}`);
  return { node: m[1], starttime: Number.parseInt(m[2], 16), type: m[3], id: m[4], user: m[5] };
}

export function encodeParams(params: Params | undefined): URLSearchParams {
  const out = new URLSearchParams();
  for (const [key, value] of Object.entries(params ?? {})) {
    if (value === undefined || value === null) continue;
    const values = Array.isArray(value) ? value : [value];
    for (const v of values) {
      if (v === undefined || v === null) continue;
      if (typeof v === "boolean") out.append(key, v ? "1" : "0");
      else if (typeof v === "object") out.append(key, JSON.stringify(v));
      else out.append(key, String(v));
    }
  }
  return out;
}

/**
 * Client for the Proxmox VE REST API (/api2/json).
 *
 * Authenticates with an API token (recommended) or with a username/password
 * ticket, which is renewed automatically. Parameters are sent the way the
 * Proxmox web UI sends them: in the query string for GET/DELETE and
 * form-encoded for POST/PUT.
 */
export class ProxmoxClient {
  private readonly dispatcher: Agent;
  private ticket?: { value: string; csrf: string; at: number };
  private loginPromise?: Promise<void>;

  constructor(readonly cfg: ProxmoxConfig) {
    // A custom CA turns verification on; it is trusted alongside the system CAs (Node's `ca` replaces them otherwise).
    const tls: ConnectionOptions = cfg.caCert ? { rejectUnauthorized: true, ca: [...rootCertificates, cfg.caCert] } : { rejectUnauthorized: cfg.verifySsl };
    this.dispatcher = new Agent({
      connect: cfg.tlsFingerprints ? pinnedConnector(tls, cfg.tlsFingerprints) : tls,
      headersTimeout: cfg.timeoutMs,
      bodyTimeout: cfg.timeoutMs,
    });
  }

  get defaultNode(): string | undefined {
    return this.cfg.defaultNode;
  }

  get taskTimeoutMs(): number {
    return this.cfg.taskTimeoutMs;
  }

  async close(): Promise<void> {
    await this.dispatcher.close();
  }

  get<T = any>(path: string, params?: Params): Promise<T> {
    return this.request<T>("GET", path, params);
  }

  post<T = any>(path: string, params?: Params): Promise<T> {
    return this.request<T>("POST", path, params);
  }

  put<T = any>(path: string, params?: Params): Promise<T> {
    return this.request<T>("PUT", path, params);
  }

  delete<T = any>(path: string, params?: Params): Promise<T> {
    return this.request<T>("DELETE", path, params);
  }

  /**
   * Low-level request. `path` is relative to /api2/json, e.g. /cluster/resources.
   * Returns the `data` member of the response.
   */
  async request<T = any>(method: HttpMethod, path: string, params?: Params): Promise<T> {
    if (!path.startsWith("/")) path = `/${path}`;
    if (!this.cfg.tokenId) await this.ensureTicket();

    let res = await this.send(method, path, params);
    if (res.status === 401 && !this.cfg.tokenId) {
      // Ticket expired or revoked: log in again and retry once.
      await res.body?.cancel();
      this.ticket = undefined;
      await this.login();
      res = await this.send(method, path, params);
    }
    return this.parse<T>(res, method, path);
  }

  // ---------------------------------------------------------------- helpers used by many tools

  /** Node names in the cluster, with their online status. */
  async listNodes(): Promise<{ node: string; status: string }[]> {
    return this.get("/nodes");
  }

  /**
   * Picks the node for node-scoped calls: the one given, else PROXMOX_DEFAULT_NODE,
   * else the only node of a single-node install.
   */
  async resolveNode(node?: string): Promise<string> {
    if (node) return node;
    if (this.cfg.defaultNode) return this.cfg.defaultNode;
    const nodes = await this.listNodes();
    if (nodes.length === 1) return nodes[0].node;
    throw new ProxmoxError(
      `This cluster has ${nodes.length} nodes; pass \`node\` (one of: ${nodes.map((n) => n.node).sort().join(", ")}) or set PROXMOX_DEFAULT_NODE`,
    );
  }

  /** Finds which node a VM or container lives on and whether it is a VM (qemu) or container (lxc). */
  async resolveGuest(vmid: number, opts: { node?: string; type?: GuestType } = {}): Promise<GuestRef> {
    const resources: any[] = await this.get("/cluster/resources", { type: "vm" });
    const match = resources.find((r) => Number(r.vmid) === Number(vmid));
    if (!match) {
      if (opts.node && opts.type) return { vmid, node: opts.node, type: opts.type };
      throw new ProxmoxError(`No VM or container with ID ${vmid} (see proxmox_list_guests)`);
    }
    if (opts.type && match.type !== opts.type) {
      const want = opts.type === "qemu" ? "a VM (qemu)" : "a container (lxc)";
      const is = match.type === "qemu" ? "a VM" : "a container";
      throw new ProxmoxError(`Guest ${vmid} is ${is}, not ${want}; use the ${match.type === "qemu" ? "proxmox_*_vm" : "proxmox_*_container"} tools`);
    }
    return {
      vmid: Number(match.vmid),
      node: opts.node ?? match.node,
      type: match.type,
      name: match.name,
      status: match.status,
      template: Boolean(match.template),
    };
  }

  /**
   * The URL a request for `path` (relative to /api2/json) is sent to. URL parsing normalises the path
   * (resolves dot segments, strips tabs and newlines), so callers that vet paths should check this.
   */
  apiUrl(path: string): URL {
    return new URL(`${this.cfg.url}/api2/json${path.startsWith("/") ? path : `/${path}`}`);
  }

  /** API path prefix of a guest, e.g. /nodes/pve1/qemu/100. */
  guestPath(g: GuestRef): string {
    return `/nodes/${encodeURIComponent(g.node)}/${g.type}/${g.vmid}`;
  }

  /** Current status of a task, plus the tail of its log. */
  async getTask(upid: string, logLines = 30): Promise<TaskResult> {
    const { node } = parseUpid(upid);
    const base = `/nodes/${encodeURIComponent(node)}/tasks/${encodeURIComponent(upid)}`;
    const st: any = await this.get(`${base}/status`);
    let log: string[] | undefined;
    if (logLines > 0) {
      // The log endpoint pages from the start; ask for the total first so we can show the end.
      const first: any[] = await this.get(`${base}/log`, { start: 0, limit: 1 }).catch(() => []);
      const total = Number((first as any).total ?? 0);
      const start = Math.max(0, (Number.isFinite(total) && total > 0 ? total : 0) - logLines);
      const lines: any[] = await this.get(`${base}/log`, { start, limit: logLines }).catch(() => []);
      log = lines.map((l) => String(l.t ?? "")).filter((t) => t !== "no content");
    }
    return taskResult(upid, st, log);
  }

  /**
   * Waits for a task to finish, polling its status, up to `timeoutMs`
   * (default PROXMOX_TASK_TIMEOUT_MS). Returns early with timedOut=true
   * rather than failing when the task is still running.
   */
  async waitForTask(upid: string, opts: { timeoutMs?: number; pollMs?: number; logLines?: number } = {}): Promise<TaskResult> {
    const { node } = parseUpid(upid);
    const timeoutMs = opts.timeoutMs ?? this.cfg.taskTimeoutMs;
    const deadline = Date.now() + timeoutMs;
    let delay = opts.pollMs ?? 500;
    for (;;) {
      const st: any = await this.get(`/nodes/${encodeURIComponent(node)}/tasks/${encodeURIComponent(upid)}/status`);
      if (st.status !== "running") return this.getTask(upid, opts.logLines ?? 30);
      if (Date.now() + delay > deadline) {
        const res = await this.getTask(upid, opts.logLines ?? 10);
        return { ...res, timedOut: true };
      }
      await new Promise((r) => setTimeout(r, delay));
      delay = Math.min(delay * 1.5, 3000);
    }
  }

  // ---------------------------------------------------------------- transport

  private async ensureTicket(): Promise<void> {
    if (!this.ticket || Date.now() - this.ticket.at > TICKET_MAX_AGE_MS) await this.login();
  }

  private login(): Promise<void> {
    // Collapse concurrent logins into one.
    this.loginPromise ??= this.doLogin().finally(() => {
      this.loginPromise = undefined;
    });
    return this.loginPromise;
  }

  private async doLogin(): Promise<void> {
    const res = await this.raw("POST", "/access/ticket", { username: this.cfg.username, password: this.cfg.password }, false);
    const text = await res.text();
    if (!res.ok) {
      throw new ProxmoxError(
        `Login to Proxmox failed (HTTP ${res.status} ${res.statusText}); check PROXMOX_USERNAME (user@realm) and PROXMOX_PASSWORD`,
        res.status,
      );
    }
    let data: any;
    try {
      data = JSON.parse(text)?.data;
    } catch {
      /* handled below */
    }
    if (data?.NeedTFA) {
      throw new ProxmoxError("This account requires two-factor authentication; use an API token instead (PROXMOX_TOKEN_ID/PROXMOX_TOKEN_SECRET)");
    }
    if (!data?.ticket) throw new ProxmoxError("Login to Proxmox failed: no ticket in the response");
    this.ticket = { value: data.ticket, csrf: data.CSRFPreventionToken, at: Date.now() };
  }

  private send(method: HttpMethod, path: string, params?: Params): Promise<Response> {
    return this.raw(method, path, params, true);
  }

  private async raw(method: HttpMethod, path: string, params: Params | undefined, auth: boolean): Promise<Response> {
    const url = this.apiUrl(path);
    const encoded = encodeParams(params);
    let body: string | undefined;
    const headers: Record<string, string> = { Accept: "application/json" };
    if (method === "GET" || method === "DELETE") {
      for (const [k, v] of encoded) url.searchParams.append(k, v);
    } else {
      body = encoded.toString();
      headers["Content-Type"] = "application/x-www-form-urlencoded";
    }
    if (auth) {
      if (this.cfg.tokenId) {
        headers.Authorization = `PVEAPIToken=${this.cfg.tokenId}=${this.cfg.tokenSecret}`;
      } else if (this.ticket) {
        headers.Cookie = `PVEAuthCookie=${encodeURIComponent(this.ticket.value)}`;
        if (method !== "GET") headers.CSRFPreventionToken = this.ticket.csrf;
      }
    }
    try {
      return await fetch(url, {
        method,
        headers,
        body,
        redirect: "manual",
        dispatcher: this.dispatcher,
        signal: AbortSignal.timeout(this.cfg.timeoutMs),
      });
    } catch (err) {
      throw this.networkError(err);
    }
  }

  private async parse<T>(res: Response, method: string, path: string): Promise<T> {
    const text = await res.text();
    let json: any;
    if (text) {
      try {
        json = JSON.parse(text);
      } catch {
        json = undefined;
      }
    }
    if (!res.ok) {
      // Proxmox puts the message in the HTTP reason phrase and per-parameter errors in `errors`.
      const reason = res.statusText || json?.message || text.slice(0, 300);
      const paramErrors =
        json?.errors && typeof json.errors === "object"
          ? Object.entries(json.errors)
              .map(([k, v]) => `${k}: ${String(v).trim()}`)
              .join("; ")
          : "";
      const hint =
        res.status === 401
          ? " (check the API token or credentials)"
          : res.status === 403
            ? " (the token or user lacks the privilege for this; for API tokens check privilege separation and the token's own ACLs)"
            : res.status === 501
              ? " (not implemented on this Proxmox VE version)"
              : "";
      throw new ProxmoxError(
        `${method} ${path} failed with HTTP ${res.status}: ${String(reason).trim()}${paramErrors ? ` (${paramErrors})` : ""}${hint}`,
        res.status,
      );
    }
    if (json === undefined) {
      if (!text) return undefined as T;
      throw new ProxmoxError(`${method} ${path} returned a non-JSON response: ${text.slice(0, 200)}`, res.status);
    }
    const data = json && typeof json === "object" && "data" in json ? json.data : json;
    // Keep top-level metadata (task logs report `total`, network config reports pending `changes`)
    // reachable without changing the shape of the data.
    if (data && typeof data === "object") {
      for (const key of ["total", "changes"]) {
        if (json[key] !== undefined && !(key in data)) Object.defineProperty(data, key, { value: json[key], enumerable: false });
      }
    }
    return data as T;
  }

  private networkError(err: unknown): ProxmoxError {
    const e = err as Error & { cause?: { code?: string; message?: string } };
    if (e.name === "TimeoutError" || e.name === "AbortError") {
      return new ProxmoxError(`Timed out after ${this.cfg.timeoutMs}ms talking to ${this.cfg.url}`);
    }
    const cause = e.cause?.code ?? e.cause?.message ?? e.message;
    const tlsHint = /CERT|SELF_SIGNED|UNABLE_TO_VERIFY/i.test(String(cause))
      ? " (Proxmox uses a self-signed certificate by default; pin it with PROXMOX_TLS_FINGERPRINT, trust its CA with PROXMOX_CA_FILE, or set PROXMOX_VERIFY_SSL=false)"
      : "";
    return new ProxmoxError(`Could not reach Proxmox at ${this.cfg.url}: ${cause}${tlsHint}`);
  }
}

/**
 * A connector that, on top of the usual TLS options, only accepts a server certificate whose SHA-256 fingerprint is
 * pinned, the way Proxmox's own clients (e.g. the PBS storage plugin, pvesh) trust a self-signed certificate.
 * The check runs after the handshake because Node skips checkServerIdentity when chain verification fails or is off.
 */
function pinnedConnector(tls: ConnectionOptions, fingerprints: string[]): buildConnector.connector {
  // No TLS session reuse, so every connection presents (and is checked against) the certificate.
  const connect = buildConnector({ ...tls, maxCachedSessions: 0 });
  return (opts, callback) => {
    connect(opts, (err, socket) => {
      if (err || !socket) return callback(err ?? new Error("TLS connection failed"), null);
      const presented = (socket as TLSSocket).getPeerCertificate?.()?.fingerprint256;
      if (!presented || !fingerprints.includes(presented.toUpperCase())) {
        socket.destroy();
        return callback(
          new Error(`TLS certificate fingerprint mismatch: the server presented SHA-256 ${presented ?? "no certificate"}, which is not in PROXMOX_TLS_FINGERPRINT`),
          null,
        );
      }
      callback(null, socket);
    });
  };
}

function iso(epochSeconds: unknown): string | undefined {
  const n = Number(epochSeconds);
  return Number.isFinite(n) && n > 0 ? new Date(n * 1000).toISOString() : undefined;
}

function taskResult(upid: string, st: any, log?: string[]): TaskResult {
  const parts = parseUpid(upid);
  const stopped = st.status === "stopped";
  const start = Number(st.starttime ?? parts.starttime);
  const end = Number(st.endtime);
  return {
    upid,
    node: st.node ?? parts.node,
    type: st.type ?? parts.type,
    id: (st.id ?? parts.id) || undefined,
    user: st.user ?? parts.user,
    status: st.status ?? "unknown",
    exitstatus: st.exitstatus,
    ok: stopped && (st.exitstatus === "OK" || /^WARNINGS/.test(String(st.exitstatus ?? ""))),
    starttime: iso(start),
    endtime: stopped ? iso(end) : undefined,
    duration_s: stopped && Number.isFinite(end) && end >= start ? end - start : undefined,
    log,
  };
}
