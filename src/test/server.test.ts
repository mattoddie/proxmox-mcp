import assert from "node:assert/strict";
import type { Server } from "node:http";
import type { AddressInfo } from "node:net";
import { after, before, describe, it } from "node:test";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { loadConfig, TOOLSETS, type Config } from "../config.js";
import { startHttpServer } from "../http.js";
import { encodeParams, parseUpid, ProxmoxClient, ProxmoxError } from "../proxmox/client.js";
import { formatBytes, parsePropertyString, parseSize, redactSecrets, summarizeNets, toPropertyString } from "../proxmox/format.js";
import { createServer } from "../server.js";
import { connect, errorText, startEnv, toolOptions, type TestEnv } from "./helpers.js";
import { MockProxmox, TEST_TOKEN_ID, TEST_TOKEN_SECRET } from "./mock-proxmox.js";

const TOKEN_ENV = { PROXMOX_TOKEN_ID: TEST_TOKEN_ID, PROXMOX_TOKEN_SECRET: TEST_TOKEN_SECRET };

describe("config", () => {
  it("requires a URL and credentials", () => {
    assert.throws(() => loadConfig({}), /PROXMOX_URL is required/);
    assert.throws(() => loadConfig({ PROXMOX_URL: "https://pve:8006" }), /API token .* or both PROXMOX_USERNAME/);
    assert.throws(() => loadConfig({ PROXMOX_URL: "ftp://pve" , ...TOKEN_ENV }), /must use https/);
  });

  it("applies defaults", () => {
    const cfg = loadConfig({ PROXMOX_URL: "https://pve:8006/api2/json/", ...TOKEN_ENV });
    assert.equal(cfg.proxmox.url, "https://pve:8006");
    assert.equal(cfg.proxmox.verifySsl, false);
    assert.equal(cfg.proxmox.timeoutMs, 30000);
    assert.equal(cfg.proxmox.taskTimeoutMs, 120000);
    assert.equal(cfg.tools.allowWrites, false);
    assert.equal(cfg.tools.allowDeletes, false);
    assert.equal(cfg.tools.allowExec, false);
    assert.equal(cfg.tools.toolsets.size, TOOLSETS.length);
    assert.equal(cfg.transport, "stdio");
    assert.equal(cfg.http.port, 8080);
  });

  it("parses the combined token form and validates token ids", () => {
    const cfg = loadConfig({ PROXMOX_URL: "https://pve", PROXMOX_API_TOKEN: "PVEAPIToken=mcp@pve!ro=abc-def" });
    assert.equal(cfg.proxmox.tokenId, "mcp@pve!ro");
    assert.equal(cfg.proxmox.tokenSecret, "abc-def");
    assert.throws(() => loadConfig({ PROXMOX_URL: "https://pve", PROXMOX_TOKEN_ID: "root", PROXMOX_TOKEN_SECRET: "x" }), /user@realm!tokenid/);
    assert.throws(() => loadConfig({ PROXMOX_URL: "https://pve", PROXMOX_TOKEN_ID: "root@pam!x" }), /Set both/);
  });

  it("adds the default realm to bare usernames", () => {
    const cfg = loadConfig({ PROXMOX_URL: "https://pve", PROXMOX_USERNAME: "admin", PROXMOX_PASSWORD: "pw" });
    assert.equal(cfg.proxmox.username, "admin@pam");
    const pve = loadConfig({ PROXMOX_URL: "https://pve", PROXMOX_USERNAME: "admin", PROXMOX_PASSWORD: "pw", PROXMOX_REALM: "pve" });
    assert.equal(pve.proxmox.username, "admin@pve");
  });

  it("parses permission gating and toolsets", () => {
    const base = { PROXMOX_URL: "https://pve", ...TOKEN_ENV };
    assert.throws(() => loadConfig({ ...base, PROXMOX_ALLOW_DELETES: "true" }), /also requires PROXMOX_ALLOW_WRITES/);
    assert.throws(() => loadConfig({ ...base, PROXMOX_ALLOW_EXEC: "true" }), /also requires PROXMOX_ALLOW_WRITES/);
    const cfg = loadConfig({ ...base, PROXMOX_ALLOW_WRITES: "1", PROXMOX_ALLOW_DELETES: "yes", PROXMOX_ALLOW_EXEC: "on", PROXMOX_TOOLSETS: "cluster, vms" });
    assert.equal(cfg.tools.allowDeletes, true);
    assert.equal(cfg.tools.allowExec, true);
    assert.deepEqual([...cfg.tools.toolsets].sort(), ["cluster", "vms"]);
    assert.throws(() => loadConfig({ ...base, PROXMOX_TOOLSETS: "cluster,bogus" }), /Unknown toolset "bogus"/);
  });
});

describe("format helpers", () => {
  it("formats and parses sizes", () => {
    assert.equal(formatBytes(1536), "1.50 KiB");
    assert.equal(formatBytes(34359738368), "32 GiB");
    assert.equal(parseSize("32G"), 34359738368);
    assert.equal(parseSize("512M"), 536870912);
  });

  it("round-trips property strings", () => {
    const p = parsePropertyString("local-lvm:vm-100-disk-0,size=32G,ssd=1", "file");
    assert.deepEqual(p, { file: "local-lvm:vm-100-disk-0", size: "32G", ssd: "1" });
    assert.equal(toPropertyString(p, "file"), "local-lvm:vm-100-disk-0,size=32G,ssd=1");
  });

  it("splits VM NIC model and MAC", () => {
    const nets = summarizeNets({ net0: "virtio=BC:24:11:00:00:01,bridge=vmbr0,tag=10" });
    assert.deepEqual(nets.net0, { model: "virtio", macaddr: "BC:24:11:00:00:01", bridge: "vmbr0", tag: "10" });
  });

  it("redacts secrets recursively", () => {
    const out = redactSecrets({ cipassword: "x", nested: [{ password: "y", name: "ok" }], token: "t", "smtp-password": "z" });
    assert.deepEqual(out, { cipassword: "<redacted>", nested: [{ password: "<redacted>", name: "ok" }], token: "<redacted>", "smtp-password": "<redacted>" });
  });

  it("redacts credentials inside URLs", () => {
    assert.deepEqual(redactSecrets({ http_proxy: "http://bob:pw@proxy:3128", email: "a@b.c" }), {
      http_proxy: "http://bob:<redacted>@proxy:3128",
      email: "a@b.c",
    });
  });

  it("encodes params like the web UI", () => {
    const p = encodeParams({ a: true, b: false, c: [1, 2], d: undefined, e: "x y" });
    assert.equal(p.toString(), "a=1&b=0&c=1&c=2&e=x+y");
  });

  it("parses UPIDs", () => {
    const u = parseUpid("UPID:pve1:0000ABCD:0001ABCD:65000001:qmstart:100:root@pam:");
    assert.equal(u.node, "pve1");
    assert.equal(u.type, "qmstart");
    assert.equal(u.id, "100");
    assert.equal(u.user, "root@pam");
    assert.throws(() => parseUpid("nope"), ProxmoxError);
  });
});

describe("ProxmoxClient with an API token", () => {
  const mock = new MockProxmox({ auth: "token" });
  let client: ProxmoxClient;
  before(async () => {
    await mock.start();
    client = new ProxmoxClient(loadConfig({ PROXMOX_URL: mock.url, ...TOKEN_ENV, PROXMOX_TASK_TIMEOUT_MS: "2000" }).proxmox);
  });
  after(async () => {
    await client.close();
    await mock.stop();
  });

  it("sends the PVEAPIToken header and unwraps data", async () => {
    const v = await client.get("/version");
    assert.equal(v.version, "9.0.10");
    assert.equal(mock.requests.at(-1)!.headers.authorization, `PVEAPIToken=${TEST_TOKEN_ID}=${TEST_TOKEN_SECRET}`);
  });

  it("sends GET params as query and POST params as a form body", async () => {
    await client.get("/cluster/resources", { type: "vm" });
    assert.deepEqual(mock.requests.at(-1)!.query, { type: "vm" });
    await client.post("/nodes/pve1/qemu/100/status/start", { skiplock: true, timeout: 30 });
    const req = mock.requests.at(-1)!;
    assert.equal(req.headers["content-type"], "application/x-www-form-urlencoded");
    assert.deepEqual(req.body, { skiplock: "1", timeout: "30" });
  });

  it("surfaces the reason phrase and parameter errors", async () => {
    mock.on("POST", /^\/nodes\/pve1\/qemu\/100\/config$/, () => mock.error(400, "Parameter verification failed.", { memory: "value must be at least 16" }));
    await assert.rejects(client.post("/nodes/pve1/qemu/100/config", { memory: 1 }), (err: ProxmoxError) => {
      assert.equal(err.status, 400);
      assert.match(err.message, /Parameter verification failed\. \(memory: value must be at least 16\)/);
      return true;
    });
    await assert.rejects(client.get("/nope"), /HTTP 404/);
  });

  it("explains permission errors", async () => {
    mock.on("GET", /^\/access\/users$/, () => mock.error(403, "Permission check failed (/access, User.Modify)"));
    await assert.rejects(client.get("/access/users"), /Permission check failed .*privilege separation/);
  });

  it("resolves nodes and guests", async () => {
    await assert.rejects(client.resolveNode(), /has 2 nodes; pass `node` \(one of: pve1, pve2\)/);
    assert.equal(await client.resolveNode("pve2"), "pve2");
    const g = await client.resolveGuest(101);
    assert.deepEqual({ node: g.node, type: g.type, name: g.name }, { node: "pve2", type: "qemu", name: "db" });
    await assert.rejects(client.resolveGuest(200, { type: "qemu" }), /is a container, not a VM/);
    await assert.rejects(client.resolveGuest(555), /No VM or container with ID 555/);
  });

  it("waits for tasks and reports their log", async () => {
    const upid = mock.upid("pve1", "qmstart", "100", { status: "stopped", exitstatus: "OK", log: ["starting", "TASK OK"] });
    const t = await client.waitForTask(upid);
    assert.equal(t.ok, true);
    assert.equal(t.type, "qmstart");
    assert.deepEqual(t.log, ["starting", "TASK OK"]);
    assert.equal(t.duration_s, 3);
  });

  it("returns timedOut for tasks that keep running", async () => {
    const upid = mock.upid("pve1", "vzdump", "100", { status: "running", log: ["INFO: starting"] });
    const t = await client.waitForTask(upid, { timeoutMs: 300, pollMs: 100 });
    assert.equal(t.timedOut, true);
    assert.equal(t.ok, false);
  });
});

describe("ProxmoxClient with a password", () => {
  const mock = new MockProxmox({ auth: "password" });
  let client: ProxmoxClient;
  before(async () => {
    await mock.start();
    client = new ProxmoxClient(loadConfig({ PROXMOX_URL: mock.url, PROXMOX_USERNAME: "root", PROXMOX_PASSWORD: "secret" }).proxmox);
  });
  after(async () => {
    await client.close();
    await mock.stop();
  });

  it("logs in, sends the ticket cookie and CSRF token on writes", async () => {
    await client.get("/version");
    assert.equal(mock.loginCount, 1);
    await client.post("/nodes/pve1/qemu/100/status/start");
    const req = mock.requests.at(-1)!;
    assert.equal(req.headers.csrfpreventiontoken, "csrf-PVE:root@pam:1");
  });

  it("logs in again once when the ticket expires", async () => {
    mock.tickets.clear();
    const nodes = await client.get("/nodes");
    assert.equal(nodes.length, 2);
    assert.equal(mock.loginCount, 2);
  });

  it("reports bad credentials clearly", async () => {
    const bad = new ProxmoxClient(loadConfig({ PROXMOX_URL: mock.url, PROXMOX_USERNAME: "root@pam", PROXMOX_PASSWORD: "wrong" }).proxmox);
    await assert.rejects(bad.get("/version"), /Login to Proxmox failed \(HTTP 401/);
    await bad.close();
  });

  it("reports unreachable hosts", async () => {
    const down = new ProxmoxClient(loadConfig({ PROXMOX_URL: "http://127.0.0.1:1", ...TOKEN_ENV }).proxmox);
    await assert.rejects(down.get("/version"), /Could not reach Proxmox at http:\/\/127\.0\.0\.1:1/);
    await down.close();
  });
});

describe("tool gating", () => {
  const dummy = new ProxmoxClient(loadConfig({ PROXMOX_URL: "https://pve.invalid", ...TOKEN_ENV }).proxmox);
  after(() => dummy.close());

  async function names(level: Parameters<typeof toolOptions>[0], toolsets?: string[]): Promise<Set<string>> {
    const opts = toolOptions(level);
    const mcp = await connect(dummy, toolsets ? { ...opts, toolsets: new Set(toolsets as any) } : opts);
    const { tools } = await mcp.listTools();
    await mcp.close();
    return new Set(tools.map((t) => t.name));
  }

  it("registers only read tools by default and annotates them", async () => {
    const mcp = await connect(dummy, toolOptions("read"));
    const { tools } = await mcp.listTools();
    await mcp.close();
    assert.ok(tools.length > 0);
    for (const t of tools) {
      assert.match(t.name, /^proxmox_[a-z0-9_]+$/, t.name);
      assert.equal(t.annotations?.readOnlyHint, true, `${t.name} should be read-only`);
      assert.ok(t.description && t.description.length > 20, `${t.name} needs a description`);
    }
  });

  it("adds write, delete and exec tools only when enabled", async () => {
    const read = await names("read");
    const write = await names("write");
    const del = await names("delete");
    const all = await names("all");
    assert.ok(!read.has("proxmox_stop_task") && write.has("proxmox_stop_task"));
    assert.ok(!write.has("proxmox_delete_metric_server") && del.has("proxmox_delete_metric_server"));
    assert.ok(all.size >= del.size);
    for (const n of read) assert.ok(write.has(n));
  });

  it("limits tools to the enabled toolsets", async () => {
    const only = await names("all", ["tasks"]);
    assert.ok(only.size > 0);
    for (const n of only) assert.match(n, /task/);
  });

  it("uses unique tool names with descriptions across all toolsets", async () => {
    const mcp = await connect(dummy, toolOptions("all"));
    const { tools } = await mcp.listTools();
    await mcp.close();
    assert.equal(new Set(tools.map((t) => t.name)).size, tools.length);
    for (const t of tools) assert.ok((t.title ?? "").length > 0, `${t.name} needs a title`);
  });

  it("only mentions registered tools in prompts and tool descriptions", async () => {
    const mcp = await connect(dummy, toolOptions("all"));
    const { tools } = await mcp.listTools();
    const { prompts } = await mcp.listPrompts();
    const known = new Set(tools.map((t) => t.name));
    const texts: [string, string][] = tools.map((t) => [t.name, t.description ?? ""]);
    for (const p of prompts) {
      const args = Object.fromEntries((p.arguments ?? []).map((a) => [a.name, a.name === "timeframe" ? "week" : "100"]));
      const res = await mcp.getPrompt({ name: p.name, arguments: args });
      texts.push([`prompt ${p.name}`, res.messages.map((m) => (m.content.type === "text" ? m.content.text : "")).join("\n")]);
    }
    await mcp.close();
    for (const [where, text] of texts) {
      for (const ref of text.match(/proxmox_[a-z0-9_]+/g) ?? []) {
        if (ref.endsWith("_")) continue; // prefixes such as proxmox_*_vm
        assert.ok(known.has(ref), `${where} mentions unknown tool ${ref}`);
      }
    }
  });
});

describe("cluster, tasks and raw tools", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv();
  });
  after(() => env.stop());

  it("summarises the cluster", async () => {
    const s = await env.call("proxmox_get_cluster_status");
    assert.equal(s.cluster.name, "homelab");
    assert.equal(s.cluster.quorate, true);
    assert.equal(s.version, "9.0.10 (release 9.0)");
    assert.equal(s.nodes.length, 2);
    assert.equal(s.nodes[0].ip, "10.0.0.11");
    assert.deepEqual(s.guests.vms, { running: 1, stopped: 1, templates: 1 });
    assert.deepEqual(s.guests.containers, { running: 1, stopped: 0, templates: 0 });
    assert.equal(s.storage.length, 3);
  });

  it("lists and filters guests", async () => {
    const all = await env.call("proxmox_list_guests");
    assert.equal(all.total, 4);
    assert.deepEqual(all.items.map((g: any) => g.vmid), [100, 101, 200, 9000]);
    const web = all.items[0];
    assert.equal(web.type, "vm");
    assert.deepEqual(web.tags, ["prod", "web"]);
    assert.equal(web.memory, "2 GiB of 4 GiB (50%)");
    const cts = await env.call("proxmox_list_guests", { type: "container" });
    assert.deepEqual(cts.items.map((g: any) => g.name), ["dns"]);
    const tagged = await env.call("proxmox_list_guests", { tag: "WEB", templates: "exclude" });
    assert.equal(tagged.total, 1);
    const byMem = await env.call("proxmox_list_guests", { sort: "memory", status: "running" });
    assert.deepEqual(byMem.items.map((g: any) => g.vmid), [100, 200]);
  });

  it("checks free VMIDs", async () => {
    env.mock.on("GET", /^\/cluster\/nextid$/, (p) => (p.vmid === "100" ? env.mock.error(400, "VM 100 already exists") : (p.vmid ?? "102")));
    assert.deepEqual(await env.call("proxmox_get_next_vmid"), { vmid: 102, free: true });
    const taken = await env.call("proxmox_get_next_vmid", { vmid: 100 });
    assert.equal(taken.free, false);
  });

  it("runs bulk actions as a task", async () => {
    const upid = env.mock.upid("pve1", "startall", "");
    env.mock.on("POST", /^\/cluster\/bulk-action\/guest\/shutdown$/, () => upid);
    const res = await env.call("proxmox_bulk_guest_action", { action: "shutdown", vmids: [100, 200], timeout: 60 });
    assert.equal(res.status, "ok");
    assert.deepEqual(env.mock.lastWrite()!.body, { vms: ["100", "200"], timeout: "60" });
  });

  it("reports failed tasks as errors with the log tail", async () => {
    const upid = env.mock.upid("pve1", "startall", "", { status: "stopped", exitstatus: "command failed", log: ["ERROR: no quorum"] });
    env.mock.on("POST", /^\/cluster\/bulk-action\/guest\/start$/, () => upid);
    const text = errorText(await env.callRaw("proxmox_bulk_guest_action", { action: "start", vmids: [101] }));
    assert.match(text, /failed: command failed/);
    assert.match(text, /ERROR: no quorum/);
  });

  it("returns the UPID immediately with wait=false", async () => {
    const upid = env.mock.upid("pve1", "startall", "", { status: "running" });
    env.mock.on("POST", /^\/cluster\/bulk-action\/guest\/start$/, () => upid);
    const res = await env.call("proxmox_bulk_guest_action", { action: "start", vmids: [101], wait: false });
    assert.equal(res.upid, upid);
    assert.equal(res.status, "started");
  });

  it("lists and inspects tasks", async () => {
    const upid = env.mock.upid("pve2", "vzdump", "101", { status: "stopped", exitstatus: "job errors", log: ["a", "b", "c"] });
    env.mock.on("GET", /^\/cluster\/tasks$/, () => [
      { upid, node: "pve2", type: "vzdump", id: "101", user: "root@pam", starttime: 1700000000, endtime: 1700000100, status: "job errors" },
      { upid: "UPID:pve1:1:1:1:qmstart:100:root@pam:", node: "pve1", type: "qmstart", id: "100", user: "root@pam", starttime: 1700000200, endtime: 1700000201, status: "OK" },
    ]);
    const failed = await env.call("proxmox_list_tasks", { errors: true });
    assert.deepEqual(failed.items.map((t: any) => t.type), ["vzdump"]);
    assert.equal(failed.items[0].duration_s, 100);
    const t = await env.call("proxmox_get_task", { upid, log_lines: 2 });
    assert.equal(t.ok, false);
    assert.deepEqual(t.log, ["b", "c"]);
    const log = await env.call("proxmox_get_task_log", { upid, tail: true, limit: 1 });
    assert.deepEqual(log.lines, ["c"]);
  });

  it("limits the raw tool by permission level", async () => {
    const v = await env.call("proxmox_api_request", { path: "/version" });
    assert.equal(v.version, "9.0.10");
    const res = await env.call("proxmox_api_request", { method: "POST", path: "/nodes/pve1/qemu/100/status/start", params: { skiplock: true } });
    assert.equal(res, null);
    assert.deepEqual(env.mock.lastWrite()!.body, { skiplock: "1" });
    assert.match(errorText(await env.callRaw("proxmox_api_request", { path: "/nodes/../x" })), /must not contain/);

    const readOnly = await connect(env.client, toolOptions("read"));
    const denied = await readOnly.callTool({ name: "proxmox_api_request", arguments: { method: "POST", path: "/nodes/pve1/qemu/100/status/start" } });
    assert.match(errorText(denied), /Write access is disabled/);
    await readOnly.close();

    const noExec = await connect(env.client, { ...toolOptions("delete") });
    const blocked = await noExec.callTool({ name: "proxmox_api_request", arguments: { method: "POST", path: "/nodes/pve1/qemu/100/agent/exec", params: { command: "id" } } });
    assert.match(errorText(blocked), /PROXMOX_ALLOW_EXEC/);
    await noExec.close();
  });
});

describe("Streamable HTTP transport", () => {
  let httpServer: Server;
  let base: string;
  const dummy = new ProxmoxClient(loadConfig({ PROXMOX_URL: "https://pve.invalid", ...TOKEN_ENV }).proxmox);

  before(async () => {
    const cfg: Config = loadConfig({
      PROXMOX_URL: "https://pve.invalid",
      ...TOKEN_ENV,
      MCP_TRANSPORT: "http",
      MCP_HTTP_HOST: "127.0.0.1",
      MCP_HTTP_PORT: "1",
      MCP_AUTH_TOKEN: "s3cret",
    });
    cfg.http.port = 0;
    httpServer = await startHttpServer(cfg, () => createServer(dummy, cfg.tools));
    base = `http://127.0.0.1:${(httpServer.address() as AddressInfo).port}`;
  });
  after(async () => {
    httpServer.close();
    await dummy.close();
  });

  it("serves /health without auth", async () => {
    const res = await fetch(`${base}/health`);
    assert.equal(res.status, 200);
  });

  it("rejects requests without the bearer token", async () => {
    const res = await fetch(`${base}/mcp`, { method: "POST", body: "{}", headers: { "Content-Type": "application/json" } });
    assert.equal(res.status, 401);
  });

  it("lists tools over HTTP with the token", async () => {
    const mcp = new Client({ name: "t", version: "0" });
    await mcp.connect(new StreamableHTTPClientTransport(new URL(`${base}/mcp`), { requestInit: { headers: { Authorization: "Bearer s3cret" } } }));
    const { tools } = await mcp.listTools();
    assert.ok(tools.some((t) => t.name === "proxmox_get_cluster_status"));
    await mcp.close();
  });
});


