import { createServer, type IncomingMessage, type Server } from "node:http";
import type { AddressInfo } from "node:net";

export interface RecordedRequest {
  method: string;
  /** Path relative to /api2/json, without the query string, e.g. /nodes/pve1/qemu/100/status/start. */
  path: string;
  query: Record<string, string | string[]>;
  /** Form-encoded body as an object; repeated keys become arrays. */
  body: Record<string, string | string[]>;
  headers: IncomingMessage["headers"];
}

/** Thrown (or returned via mock.error) by handlers to send an HTTP error. */
export class MockHttpError {
  constructor(
    readonly status: number,
    readonly reason: string,
    readonly errors?: Record<string, string>,
  ) {}
}

/**
 * Handler for a custom route. Receives the merged query+body params, the regex
 * match and the request; returns the `data` payload (wrapped as {"data": ...})
 * or a MockHttpError.
 */
export type MockHandler = (params: Record<string, any>, match: RegExpMatchArray, req: RecordedRequest) => unknown;

export interface MockTask {
  status: "running" | "stopped";
  exitstatus?: string;
  log?: string[];
}

export interface MockOptions {
  /** "token" requires the test API token; "password" requires a ticket from /access/ticket. */
  auth: "token" | "password";
}

export const TEST_TOKEN_ID = "root@pam!test";
export const TEST_TOKEN_SECRET = "00000000-1111-2222-3333-444444444444";

function formToObject(params: URLSearchParams): Record<string, string | string[]> {
  const out: Record<string, string | string[]> = {};
  for (const [k, v] of params) {
    const cur = out[k];
    if (cur === undefined) out[k] = v;
    else out[k] = Array.isArray(cur) ? [...cur, v] : [cur, v];
  }
  return out;
}

/**
 * A small fake Proxmox VE API. It serves a fixed two-node cluster (see the
 * fixtures below), records every request, answers task status/log lookups
 * for any UPID, and lets tests add routes with on(). Unmatched POST/PUT/DELETE
 * requests succeed with `null` data so simple write tools can be tested by
 * inspecting mock.requests.
 */
export class MockProxmox {
  requests: RecordedRequest[] = [];
  /** Task states by UPID; unknown UPIDs report stopped/OK. */
  tasks = new Map<string, MockTask>();
  /** Tickets the server accepts; clear it to simulate expiry. */
  tickets = new Set<string>();
  loginCount = 0;
  url = "";
  private handlers: { method: string; pattern: RegExp; fn: MockHandler }[] = [];
  private server!: Server;
  private upidCounter = 0;

  constructor(private readonly opts: MockOptions = { auth: "token" }) {}

  async start(): Promise<void> {
    this.server = createServer((req, res) => {
      const chunks: Buffer[] = [];
      req.on("data", (c) => chunks.push(c));
      req.on("end", () => {
        const text = Buffer.concat(chunks).toString();
        const url = new URL(req.url ?? "/", "http://localhost");
        const path = url.pathname.replace(/^\/api2\/json/, "");
        const recorded: RecordedRequest = {
          method: req.method ?? "GET",
          path,
          query: formToObject(url.searchParams),
          body: text ? formToObject(new URLSearchParams(text)) : {},
          headers: req.headers,
        };
        if (!url.pathname.startsWith("/api2/json/")) {
          res.writeHead(404, "Not Found").end();
          return;
        }
        this.requests.push(recorded);
        let status = 200;
        let reason = "OK";
        let payload: unknown;
        try {
          const out = this.route(recorded);
          if (out instanceof MockHttpError) throw out;
          payload = out instanceof WithTotal ? { data: out.data, total: out.total } : { data: out ?? null };
        } catch (err) {
          if (!(err instanceof MockHttpError)) throw err;
          status = err.status;
          reason = err.reason;
          payload = { data: null, ...(err.errors ? { errors: err.errors } : {}) };
        }
        res.writeHead(status, reason, { "Content-Type": "application/json" });
        res.end(JSON.stringify(payload));
      });
    });
    await new Promise<void>((r) => this.server.listen(0, "127.0.0.1", r));
    this.url = `http://127.0.0.1:${(this.server.address() as AddressInfo).port}`;
  }

  async stop(): Promise<void> {
    await new Promise((r) => this.server.close(r));
  }

  /**
   * Register a route. `pattern` is matched against the path relative to
   * /api2/json, e.g. /^\/nodes\/([^/]+)\/qemu\/(\d+)\/config$/. Later
   * registrations take precedence.
   */
  on(method: string, pattern: RegExp, fn: MockHandler): this {
    this.handlers.unshift({ method, pattern, fn });
    return this;
  }

  /** Remove all custom routes (fixtures stay). */
  reset(): void {
    this.handlers = [];
    this.requests = [];
    this.tasks.clear();
  }

  error(status: number, reason: string, errors?: Record<string, string>): MockHttpError {
    return new MockHttpError(status, reason, errors);
  }

  /** Make a UPID for a task on `node` and register its final state (default stopped/OK). */
  upid(node = "pve1", type = "qmstart", id = "100", task: MockTask = { status: "stopped", exitstatus: "OK", log: ["TASK OK"] }): string {
    const start = (0x65000000 + ++this.upidCounter).toString(16).toUpperCase();
    const upid = `UPID:${node}:0000ABCD:0001ABCD:${start}:${type}:${id}:root@pam:`;
    this.tasks.set(upid, task);
    return upid;
  }

  /** Requests whose method matches and whose path starts with `prefix` (or matches a regex). */
  requestsTo(prefix: string | RegExp, method?: string): RecordedRequest[] {
    return this.requests.filter(
      (r) => (!method || r.method === method) && (typeof prefix === "string" ? r.path.startsWith(prefix) : prefix.test(r.path)),
    );
  }

  /** The last recorded request that isn't a GET (i.e. the change a write tool made). */
  lastWrite(): RecordedRequest | undefined {
    return this.requests.filter((r) => r.method !== "GET").at(-1);
  }

  private authorized(req: RecordedRequest): boolean {
    if (this.opts.auth === "token") return req.headers.authorization === `PVEAPIToken=${TEST_TOKEN_ID}=${TEST_TOKEN_SECRET}`;
    const ticket = decodeURIComponent((req.headers.cookie ?? "").match(/PVEAuthCookie=([^;]+)/)?.[1] ?? "");
    if (!this.tickets.has(ticket)) return false;
    if (req.method !== "GET" && req.headers.csrfpreventiontoken !== `csrf-${ticket}`) return false;
    return true;
  }

  private route(req: RecordedRequest): unknown {
    const params = { ...req.query, ...req.body };

    if (req.method === "POST" && req.path === "/access/ticket") {
      if (params.username !== "root@pam" || params.password !== "secret") throw this.error(401, "authentication failure");
      const ticket = `PVE:root@pam:${++this.loginCount}`;
      this.tickets.add(ticket);
      return { ticket, CSRFPreventionToken: `csrf-${ticket}`, username: "root@pam", cap: {} };
    }
    if (!this.authorized(req)) throw this.error(401, "No ticket");

    for (const h of this.handlers) {
      const m = req.path.match(h.pattern);
      if (m && h.method === req.method) return h.fn(params, m, req);
    }

    const task = req.path.match(/^\/nodes\/([^/]+)\/tasks\/([^/]+)\/(status|log)$/);
    if (task && req.method === "GET") {
      const upid = decodeURIComponent(task[2]);
      const t = this.tasks.get(upid) ?? { status: "stopped", exitstatus: "OK", log: ["TASK OK"] };
      if (task[3] === "status") {
        const [, node, , , start, type, id, user] = upid.split(":");
        return {
          upid,
          node,
          type,
          id,
          user,
          status: t.status,
          exitstatus: t.status === "stopped" ? t.exitstatus : undefined,
          starttime: Number.parseInt(start, 16),
          endtime: t.status === "stopped" ? Number.parseInt(start, 16) + 3 : undefined,
          pid: 1,
        };
      }
      const lines = (t.log ?? []).map((l, i) => ({ n: i + 1, t: l }));
      const start = Number(params.start ?? 0);
      const limit = Number(params.limit ?? 50);
      return new WithTotal(lines.slice(start, start + limit), lines.length);
    }

    const fixture = FIXTURES[`${req.method} ${req.path}`];
    if (fixture !== undefined) return typeof fixture === "function" ? (fixture as (p: any) => unknown)(params) : structuredClone(fixture);

    if (req.method === "GET") throw this.error(404, `no mock for GET ${req.path}`);
    return null;
  }
}

/** Return value for routes that send a top-level `total` next to `data`, like the task log endpoint. */
export class WithTotal {
  constructor(
    readonly data: unknown[],
    readonly total: number,
  ) {}
}

// ---------------------------------------------------------------- fixtures

export const NODES = [
  { node: "pve1", status: "online", type: "node", id: "node/pve1", cpu: 0.12, maxcpu: 16, mem: 17179869184, maxmem: 68719476736, uptime: 864000, level: "", ssl_fingerprint: "AA:BB" },
  { node: "pve2", status: "online", type: "node", id: "node/pve2", cpu: 0.05, maxcpu: 8, mem: 8589934592, maxmem: 34359738368, uptime: 432000, level: "" },
];

export const GUESTS = [
  { id: "qemu/100", vmid: 100, type: "qemu", node: "pve1", name: "web", status: "running", cpu: 0.031, maxcpu: 4, mem: 2147483648, maxmem: 4294967296, disk: 0, maxdisk: 34359738368, uptime: 3600, netin: 1000, netout: 2000, tags: "prod;web", template: 0, pool: "production" },
  { id: "qemu/101", vmid: 101, type: "qemu", node: "pve2", name: "db", status: "stopped", cpu: 0, maxcpu: 2, mem: 0, maxmem: 8589934592, disk: 0, maxdisk: 107374182400, uptime: 0, template: 0 },
  { id: "lxc/200", vmid: 200, type: "lxc", node: "pve1", name: "dns", status: "running", cpu: 0.002, maxcpu: 1, mem: 134217728, maxmem: 536870912, disk: 1073741824, maxdisk: 8589934592, uptime: 7200, tags: "infra", template: 0 },
  { id: "qemu/9000", vmid: 9000, type: "qemu", node: "pve1", name: "tpl-debian", status: "stopped", maxcpu: 2, maxmem: 2147483648, maxdisk: 3221225472, template: 1 },
];

export const STORAGES = [
  { id: "storage/pve1/local", storage: "local", type: "storage", node: "pve1", status: "available", plugintype: "dir", content: "iso,vztmpl,backup", disk: 10737418240, maxdisk: 107374182400, shared: 0 },
  { id: "storage/pve1/local-lvm", storage: "local-lvm", type: "storage", node: "pve1", status: "available", plugintype: "lvmthin", content: "images,rootdir", disk: 53687091200, maxdisk: 429496729600, shared: 0 },
  { id: "storage/pve2/local", storage: "local", type: "storage", node: "pve2", status: "available", plugintype: "dir", content: "iso,vztmpl,backup", disk: 5368709120, maxdisk: 107374182400, shared: 0 },
];

const RESOURCES = [...NODES, ...GUESTS, ...STORAGES, { id: "pool/production", type: "pool", pool: "production" }];

export const VM_100_CONFIG = {
  name: "web",
  cores: 4,
  sockets: 1,
  memory: "4096",
  ostype: "l26",
  boot: "order=scsi0;net0",
  scsihw: "virtio-scsi-single",
  scsi0: "local-lvm:vm-100-disk-0,iothread=1,size=32G",
  ide2: "local:iso/debian-12.iso,media=cdrom",
  net0: "virtio=BC:24:11:00:00:01,bridge=vmbr0,firewall=1,tag=10",
  agent: "1",
  cipassword: "hunter2",
  tags: "prod;web",
  digest: "abc123",
};

export const CT_200_CONFIG = {
  hostname: "dns",
  arch: "amd64",
  cores: 1,
  memory: 512,
  swap: 512,
  ostype: "debian",
  rootfs: "local-lvm:vm-200-disk-0,size=8G",
  net0: "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:02,ip=dhcp,type=veth",
  unprivileged: 1,
  features: "nesting=1",
  tags: "infra",
  digest: "def456",
};

const FIXTURES: Record<string, unknown> = {
  "GET /version": { version: "9.0.10", release: "9.0", repoid: "deadbeef" },
  "GET /nodes": NODES,
  "GET /cluster/resources": (p: { type?: string }) =>
    structuredClone(
      p.type ? RESOURCES.filter((r) => (p.type === "vm" ? r.type === "qemu" || r.type === "lxc" : r.type === p.type)) : RESOURCES,
    ),
  "GET /cluster/status": [
    { type: "cluster", id: "cluster", name: "homelab", nodes: 2, quorate: 1, version: 4 },
    { type: "node", id: "node/pve1", name: "pve1", nodeid: 1, online: 1, local: 1, ip: "10.0.0.11", level: "" },
    { type: "node", id: "node/pve2", name: "pve2", nodeid: 2, online: 1, local: 0, ip: "10.0.0.12", level: "" },
  ],
  "GET /nodes/pve1/qemu/100/config": VM_100_CONFIG,
  "GET /nodes/pve1/lxc/200/config": CT_200_CONFIG,
};
