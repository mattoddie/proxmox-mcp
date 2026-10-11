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

  it("allows guest file reads with exec access", async () => {
    env.mock.on("GET", /\/agent\/file-read$/, () => ({ content: "x", truncated: 0 }));
    const res = await env.call("proxmox_api_request", { path: "/nodes/pve1/qemu/100/agent/file-read", params: { file: "/etc/hostname" } });
    assert.equal(res.content, "x");
  });
});
