import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";
import { connect, errorText, startEnv, toolOptions, type TestEnv } from "./helpers.js";

describe("raw API tool", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv();
  });
  after(() => env.stop());

  it("blocks guest file reads (GET) and encoded path tricks without exec access", async () => {
    const noExec = await connect(env.client, toolOptions("delete"));
    const call = (path: string, method = "GET") => noExec.callTool({ name: "proxmox_api_request", arguments: { method, path, params: { file: "/etc/shadow" } } });
    try {
      assert.match(errorText(await call("/nodes/pve1/qemu/100/agent/file-read")), /PROXMOX_ALLOW_EXEC/);
      assert.match(errorText(await call("/nodes/pve1/qemu/100/agent/exec-status")), /PROXMOX_ALLOW_EXEC/);
      assert.match(errorText(await call("/nodes/pve1/qemu/100/agent/file%2Dread")), /PROXMOX_ALLOW_EXEC/);
      assert.match(errorText(await call("/nodes/pve1/qemu/100/agent//exec", "POST")), /PROXMOX_ALLOW_EXEC/);
      assert.match(errorText(await call("/nodes/pve1/qemu/100/agent/x/%2e%2e/file-read")), /must not contain/);
      assert.match(errorText(await call("/nodes/pve1/qemu/100/agent/./file-read")), /must not contain/);
      assert.match(errorText(await call("/nodes/pve1/qemu/100/agent\\file-read")), /must not contain/);
      assert.match(errorText(await call("/nodes/%zz")), /percent-encoded/);
    } finally {
      await noExec.close();
    }
  });

  it("blocks control characters that URL parsing would strip, also in read-only mode", async () => {
    env.mock.reset();
    for (const level of ["read", "delete"] as const) {
      const mcp = await connect(env.client, toolOptions(level));
      const call = (path: string) => mcp.callTool({ name: "proxmox_api_request", arguments: { path, params: { file: "/etc/shadow" } } });
      try {
        // WHATWG URL parsing drops tab/CR/LF, so these would be sent as .../agent/file-read.
        assert.match(errorText(await call("/nodes/pve1/qemu/100/agent/file-re\tad")), /control characters/);
        assert.match(errorText(await call("/nodes/pve1/qemu/100/agent/file-re\nad")), /control characters/);
        assert.match(errorText(await call("/nodes/pve1/qemu/100/agent/file-re\r\nad")), /control characters/);
        assert.match(errorText(await call("/nodes/pve1/qemu/100/agent/file-re%09ad")), /control characters/);
        assert.match(errorText(await call("/nodes/pve1/qemu/100/x/.\t./agent/file-read")), /control characters/);
        assert.match(errorText(await call("/nodes/pve1/qemu/100/agent/file-read%00")), /control characters/);
        assert.match(errorText(await call("/nodes/pve1/status\x7f")), /control characters/);
      } finally {
        await mcp.close();
      }
    }
    assert.equal(env.mock.requests.length, 0);
  });

  it("blocks console, sendkey and every guest agent endpoint without exec access", async () => {
    env.mock.reset();
    const noExec = await connect(env.client, toolOptions("delete"));
    const call = (path: string, method = "POST") => noExec.callTool({ name: "proxmox_api_request", arguments: { method, path } });
    try {
      for (const path of [
        "/nodes/pve1/qemu/100/sendkey",
        "/nodes/pve1/qemu/100/vncwebsocket",
        "/nodes/pve1/qemu/100/vncproxy",
        "/nodes/pve1/qemu/100/termproxy",
        "/nodes/pve1/qemu/100/spiceproxy",
        "/nodes/pve1/qemu/100/monitor",
        "/nodes/pve1/qemu/100/mtunnel",
        "/nodes/pve1/qemu/100/mtunnelwebsocket",
        "/nodes/pve1/lxc/200/termproxy",
        "/nodes/pve1/termproxy",
        "/nodes/pve1/vncshell",
        "/nodes/pve1/spiceshell",
        "/nodes/pve1/vncwebsocket",
        "/nodes/pve1/execute",
        "/nodes/pve1/qemu/100/agent",
        "/nodes/pve1/qemu/100/agent/get-osinfo",
        "/nodes/pve1/qemu/100/agent/fsfreeze-freeze",
        "/nodes/pve1/qemu/100/Agent/File-Read",
        "/api2/json/nodes/pve1/qemu/100/sendkey",
      ]) {
        assert.match(errorText(await call(path)), /PROXMOX_ALLOW_EXEC/, path);
      }
      assert.equal(env.mock.requests.length, 0);
    } finally {
      await noExec.close();
    }
  });

  it("checks the normalised path that is actually sent", async () => {
    env.mock.reset();
    env.mock.on("GET", /^\/nodes\/pve1\/status$/, () => ({ ok: true }));
    const res = await env.call("proxmox_api_request", { path: "/api2/json/nodes/pve1/status" });
    assert.equal(res.ok, true);
    assert.equal(env.mock.requests.at(-1)!.path, "/nodes/pve1/status");
    assert.match(errorText(await env.callRaw("proxmox_api_request", { path: "nodes/pve1/status" })), /must be absolute/);
  });

  it("redacts ACME DNS plugin credentials", async () => {
    // The API returns plugin data as base64 of KEY=value lines.
    const data = Buffer.from("CF_Token=not-a-real-token\nCF_Account_ID=abc123\n").toString("base64");
    env.mock.on("GET", /^\/cluster\/acme\/plugins$/, () => [
      { plugin: "cf", type: "dns", api: "cf", data, digest: "d1" },
      { plugin: "standalone", type: "standalone" },
    ]);
    env.mock.on("GET", /^\/cluster\/acme\/plugins\/cf$/, () => ({ data, digest: "d1" }));
    const list = await env.call("proxmox_api_request", { path: "/cluster/acme/plugins" });
    assert.equal(list[0].data, "CF_Token=<redacted>\nCF_Account_ID=<redacted>");
    assert.equal(list[0].api, "cf");
    assert.deepEqual(list[1], { plugin: "standalone", type: "standalone" });
    // A single plugin's config doesn't repeat type/api, so the path decides.
    const one = await env.call("proxmox_api_request", { path: "/cluster/acme/plugins/cf" });
    assert.equal(one.data, "CF_Token=<redacted>\nCF_Account_ID=<redacted>");
    assert.ok(!JSON.stringify([list, one]).includes(data));
    // Unrelated `data` fields are left alone.
    env.mock.on("GET", /^\/nodes\/pve1\/rrddata$/, () => [{ data: "x=1" }]);
    assert.deepEqual(await env.call("proxmox_api_request", { path: "/nodes/pve1/rrddata" }), [{ data: "x=1" }]);
    // redact=false returns it as is.
    assert.equal((await env.call("proxmox_api_request", { path: "/cluster/acme/plugins/cf", redact: false })).data, data);
  });

  it("allows guest file reads with exec access", async () => {
    env.mock.on("GET", /\/agent\/file-read$/, () => ({ content: "x", truncated: 0 }));
    const res = await env.call("proxmox_api_request", { path: "/nodes/pve1/qemu/100/agent/file-read", params: { file: "/etc/hostname" } });
    assert.equal(res.content, "x");
  });
});
