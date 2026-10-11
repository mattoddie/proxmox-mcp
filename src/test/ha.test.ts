import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { errorText, startEnv, type TestEnv } from "./helpers.js";

const CURRENT = [
  { id: "quorum", type: "quorum", node: "pve1", status: "OK", quorate: 1 },
  { id: "master", type: "master", node: "pve1", status: "pve1 (active, Sat Oct 11 10:00:00 2026)", timestamp: 1791700000 },
  { id: "lrm:pve1", type: "lrm", node: "pve1", status: "pve1 (active, Sat Oct 11 10:00:00 2026)", timestamp: 1791700000 },
  { id: "lrm:pve2", type: "lrm", node: "pve2", status: "pve2 (idle, Sat Oct 11 10:00:00 2026)", timestamp: 1791700000 },
  { id: "service:vm:100", type: "service", sid: "vm:100", node: "pve1", state: "started", crm_state: "started", request_state: "started", status: "vm:100 (pve1, started)" },
  { id: "fencing", type: "fencing", node: "pve1", status: "armed", "armed-state": "armed" },
];

const MANAGER = {
  manager_status: { master_node: "pve1", node_status: { pve1: "online", pve2: "online" }, service_status: { "vm:100": { node: "pve1", state: "started" } } },
  lrm_status: { pve1: { mode: "active", state: "active", timestamp: 1791700000 }, pve2: { mode: "active", state: "wait_for_agent_lock", timestamp: 1791700000 } },
  quorum: { node: "pve1", quorate: "1" },
};

const RESOURCES = [
  { sid: "vm:100", type: "vm", state: "started", max_restart: 2, max_relocate: 1, failback: 1, comment: "web", digest: "r" },
  { sid: "ct:200", type: "ct", state: "stopped", group: "pref1", digest: "r" },
];

describe("ha tools", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv();
  });
  after(async () => env.stop());
  beforeEach(() => env.mock.reset());

  const routes = () => {
    env.mock.on("GET", /^\/cluster\/ha\/status\/current$/, () => CURRENT);
    env.mock.on("GET", /^\/cluster\/ha\/status\/manager_status$/, () => MANAGER);
    env.mock.on("GET", /^\/cluster\/ha\/resources$/, (p) => (p.type ? RESOURCES.filter((r) => r.type === p.type) : RESOURCES));
  };

  it("summarizes HA status", async () => {
    routes();
    const out = await env.call("proxmox_get_ha_status");
    assert.deepEqual(out.quorum, { quorate: true, node: "pve1", status: "OK" });
    assert.equal(out.master.node, "pve1");
    assert.equal(out.master.last_update, new Date(1791700000 * 1000).toISOString());
    assert.deepEqual(out.fencing, { armed_state: "armed", status: "armed" });
    assert.equal(out.nodes.length, 2);
    assert.deepEqual(out.nodes[1], {
      node: "pve2",
      crm_node_state: "online",
      lrm_state: "wait_for_agent_lock",
      lrm_mode: "active",
      status: "pve2 (idle, Sat Oct 11 10:00:00 2026)",
      last_update: new Date(1791700000 * 1000).toISOString(),
    });
    assert.deepEqual(out.services, [{ sid: "vm:100", node: "pve1", state: "started", crm_state: "started", request_state: "started", status: "vm:100 (pve1, started)" }]);
    const raw = await env.call("proxmox_get_ha_status", { raw: true });
    assert.equal(raw.manager_status.manager_status.master_node, "pve1");
  });

  it("notes when nothing is HA-managed and works without manager_status", async () => {
    env.mock.on("GET", /^\/cluster\/ha\/status\/current$/, () => [{ id: "quorum", type: "quorum", node: "pve1", status: "OK", quorate: 1 }]);
    const out = await env.call("proxmox_get_ha_status");
    assert.match(out.note, /No HA-managed resources/);
    assert.equal(out.quorum.quorate, true);
  });

  it("lists resources merged with status and guest names", async () => {
    routes();
    const out = await env.call("proxmox_list_ha_resources");
    assert.equal(out.total, 2);
    assert.deepEqual(out.items[0], {
      sid: "vm:100",
      name: "web",
      type: "vm",
      state: "started",
      max_restart: 2,
      max_relocate: 1,
      failback: true,
      comment: "web",
      current: { node: "pve1", state: "started", crm_state: "started", request_state: "started", status: "vm:100 (pve1, started)" },
    });
    assert.deepEqual(out.items[1], { sid: "ct:200", name: "dns", type: "ct", state: "stopped", group: "pref1" });
    const cts = await env.call("proxmox_list_ha_resources", { type: "ct" });
    assert.equal(cts.total, 1);
    assert.equal(env.mock.requestsTo("/cluster/ha/resources").at(-1)!.query.type, "ct");
    const search = await env.call("proxmox_list_ha_resources", { search: "dns" });
    assert.equal(search.items[0].sid, "ct:200");
  });

  it("gets a resource by VMID with its rules", async () => {
    routes();
    env.mock.on("GET", /^\/cluster\/ha\/resources\/ct%3A200$/, () => ({ ...RESOURCES[1], sid: undefined }));
    env.mock.on("GET", /^\/cluster\/ha\/rules$/, () => [{ rule: "keep-dns", type: "node-affinity", resources: "ct:200", nodes: "pve1:2,pve2", strict: 0 }]);
    const out = await env.call("proxmox_get_ha_resource", { sid: 200 });
    assert.equal(out.sid, "ct:200");
    assert.equal(out.state, "stopped");
    assert.equal(out.digest, "r");
    assert.deepEqual(out.rules, [{ rule: "keep-dns", type: "node-affinity", resources: ["ct:200"], nodes: ["pve1:2", "pve2"], strict: false }]);
    assert.equal(env.mock.requestsTo("/cluster/ha/rules")[0].query.resource, "ct:200");
  });

  it("rejects unknown VMIDs", async () => {
    assert.match(errorText(await env.callRaw("proxmox_get_ha_resource", { sid: 555 })), /No VM or container with ID 555/);
  });

  it("creates a new HA resource", async () => {
    routes();
    env.mock.on("GET", /^\/cluster\/ha\/resources\/vm%3A101$/, () => ({ type: "vm", state: "started", max_restart: 3 }));
    const out = await env.call("proxmox_save_ha_resource", { sid: 101, max_restart: 3, failback: false, auto_rebalance: false, comment: "db" });
    const w = env.mock.lastWrite()!;
    assert.deepEqual([w.method, w.path], ["POST", "/cluster/ha/resources"]);
    assert.deepEqual(w.body, { sid: "vm:101", max_restart: "3", failback: "0", "auto-rebalance": "0", comment: "db" });
    assert.equal(out.created, "vm:101");
    assert.equal(out.resource.max_restart, 3);
  });

  it("updates an existing HA resource", async () => {
    routes();
    env.mock.on("GET", /^\/cluster\/ha\/resources\/vm%3A100$/, () => RESOURCES[0]);
    const out = await env.call("proxmox_save_ha_resource", { sid: "vm:100", state: "disabled", delete: ["comment"], digest: "r" });
    const w = env.mock.lastWrite()!;
    assert.deepEqual([w.method, w.path], ["PUT", "/cluster/ha/resources/vm%3A100"]);
    assert.deepEqual(w.body, { state: "disabled", delete: "comment", digest: "r" });
    assert.equal(out.updated, "vm:100");
    assert.match(errorText(await env.callRaw("proxmox_save_ha_resource", { sid: "vm:101", delete: ["comment"] })), /delete only applies/);
  });

  it("deletes an HA resource", async () => {
    await env.call("proxmox_delete_ha_resource", { sid: "ct:200", purge: false });
    const w = env.mock.lastWrite()!;
    assert.deepEqual([w.method, w.path, w.query.purge], ["DELETE", "/cluster/ha/resources/ct%3A200", "0"]);
  });

  it("migrates and relocates", async () => {
    env.mock.on("POST", /^\/cluster\/ha\/resources\/vm%3A100\/migrate$/, () => ({
      sid: "vm:100",
      "requested-node": "pve2",
      "comigrated-resources": ["vm:101"],
      "blocking-resources": [],
    }));
    const out = await env.call("proxmox_ha_migrate", { sid: 100, action: "migrate", node: "pve2" });
    assert.deepEqual(env.mock.lastWrite()!.body, { node: "pve2" });
    assert.deepEqual(out.comigrated_resources, ["vm:101"]);
    assert.equal(out.blocking_resources, undefined);
    assert.equal(out.requested, true);

    // PVE 8 returns null
    const rel = await env.call("proxmox_ha_migrate", { sid: "ct:200", action: "relocate", node: "pve2" });
    assert.equal(env.mock.lastWrite()!.path, "/cluster/ha/resources/ct%3A200/relocate");
    assert.equal(rel.action, "relocate");
  });

  describe("groups", () => {
    const groups = () =>
      env.mock.on("GET", /^\/cluster\/ha\/groups$/, () => [{ group: "pref1", nodes: "pve1:2,pve2:1", restricted: 0, nofailback: 1, type: "group", digest: "g" }]);

    it("lists and gets groups", async () => {
      groups();
      env.mock.on("GET", /^\/cluster\/ha\/groups\/pref1$/, () => ({ nodes: "pve1:2,pve2:1", restricted: 1, digest: "g" }));
      assert.deepEqual(await env.call("proxmox_list_ha_groups"), [{ group: "pref1", nodes: ["pve1:2", "pve2:1"], restricted: false, nofailback: true }]);
      const one = await env.call("proxmox_get_ha_group", { group: "pref1" });
      assert.deepEqual(one, { group: "pref1", nodes: ["pve1:2", "pve2:1"], restricted: true, nofailback: false, digest: "g" });
    });

    it("creates and updates groups", async () => {
      groups();
      await env.call("proxmox_save_ha_group", { group: "pref2", nodes: ["pve2:2", "pve1"], restricted: true });
      let w = env.mock.lastWrite()!;
      assert.deepEqual([w.method, w.path], ["POST", "/cluster/ha/groups"]);
      assert.deepEqual(w.body, { group: "pref2", nodes: "pve2:2,pve1", restricted: "1" });
      assert.match(errorText(await env.callRaw("proxmox_save_ha_group", { group: "pref3" })), /nodes is required/);

      const out = await env.call("proxmox_save_ha_group", { group: "pref1", nofailback: false, delete: ["comment"] });
      w = env.mock.lastWrite()!;
      assert.deepEqual([w.method, w.path], ["PUT", "/cluster/ha/groups/pref1"]);
      assert.deepEqual(w.body, { nofailback: "0", delete: "comment" });
      assert.equal(out.updated, "pref1");
    });

    it("deletes a group", async () => {
      await env.call("proxmox_delete_ha_group", { group: "pref1" });
      assert.deepEqual([env.mock.lastWrite()!.method, env.mock.lastWrite()!.path], ["DELETE", "/cluster/ha/groups/pref1"]);
    });
  });

  describe("rules", () => {
    const RULES = [
      { rule: "web-on-pve1", type: "node-affinity", resources: "vm:100", nodes: "pve1:2,pve2", strict: 1, digest: "x" },
      { rule: "apart", type: "resource-affinity", resources: "vm:100,vm:101", affinity: "negative", disable: 1, comment: "spread" },
    ];
    const rules = () => env.mock.on("GET", /^\/cluster\/ha\/rules$/, () => RULES);

    it("lists rules with filters", async () => {
      rules();
      const out = await env.call("proxmox_list_ha_rules", { type: "resource-affinity", resource: 100 });
      assert.equal(out.length, 2);
      assert.deepEqual(out[1], { rule: "apart", type: "resource-affinity", resources: ["vm:100", "vm:101"], affinity: "negative", disabled: true, comment: "spread" });
      assert.deepEqual(env.mock.requestsTo("/cluster/ha/rules")[0].query, { type: "resource-affinity", resource: "vm:100" });
    });

    it("gets a rule", async () => {
      env.mock.on("GET", /^\/cluster\/ha\/rules\/web-on-pve1$/, () => RULES[0]);
      const out = await env.call("proxmox_get_ha_rule", { rule: "web-on-pve1" });
      assert.deepEqual(out, { rule: "web-on-pve1", type: "node-affinity", resources: ["vm:100"], nodes: ["pve1:2", "pve2"], strict: true, digest: "x" });
    });

    it("creates node- and resource-affinity rules", async () => {
      rules();
      env.mock.on("GET", /^\/cluster\/ha\/rules\/dns-pref$/, () => ({ type: "node-affinity", resources: "ct:200", nodes: "pve1" }));
      const out = await env.call("proxmox_save_ha_rule", { rule: "dns-pref", type: "node-affinity", resources: [200], nodes: ["pve1"], strict: true });
      let w = env.mock.lastWrite()!;
      assert.deepEqual([w.method, w.path], ["POST", "/cluster/ha/rules"]);
      assert.deepEqual(w.body, { rule: "dns-pref", type: "node-affinity", resources: "ct:200", nodes: "pve1", strict: "1" });
      assert.equal(out.created, "dns-pref");
      assert.deepEqual(out.rule.resources, ["ct:200"]);

      await env.call("proxmox_save_ha_rule", { rule: "together", type: "resource-affinity", resources: ["vm:100", 101], affinity: "positive" });
      w = env.mock.lastWrite()!;
      assert.deepEqual(w.body, { rule: "together", type: "resource-affinity", resources: "vm:100,vm:101", affinity: "positive" });
    });

    it("validates new rules", async () => {
      rules();
      assert.match(errorText(await env.callRaw("proxmox_save_ha_rule", { rule: "n", resources: ["vm:100"] })), /type is required/);
      assert.match(errorText(await env.callRaw("proxmox_save_ha_rule", { rule: "n", type: "node-affinity" })), /resources is required/);
      assert.match(errorText(await env.callRaw("proxmox_save_ha_rule", { rule: "n", type: "node-affinity", resources: ["vm:100"] })), /nodes is required/);
      assert.match(errorText(await env.callRaw("proxmox_save_ha_rule", { rule: "n", type: "resource-affinity", resources: ["vm:100"] })), /affinity is required/);
      assert.match(errorText(await env.callRaw("proxmox_save_ha_rule", { rule: "apart", type: "node-affinity" })), /type can't be changed/);
      assert.equal(env.mock.lastWrite(), undefined);
    });

    it("updates a rule, sending its existing type", async () => {
      rules();
      await env.call("proxmox_save_ha_rule", { rule: "apart", disable: false, delete: ["comment"], digest: "x" });
      const w = env.mock.lastWrite()!;
      assert.deepEqual([w.method, w.path], ["PUT", "/cluster/ha/rules/apart"]);
      assert.deepEqual(w.body, { type: "resource-affinity", disable: "0", delete: "comment", digest: "x" });
    });

    it("deletes a rule", async () => {
      await env.call("proxmox_delete_ha_rule", { rule: "apart" });
      assert.deepEqual([env.mock.lastWrite()!.method, env.mock.lastWrite()!.path], ["DELETE", "/cluster/ha/rules/apart"]);
    });
  });

  it("arms and disarms HA", async () => {
    assert.match(errorText(await env.callRaw("proxmox_set_ha_armed", { action: "disarm" })), /resource_mode/);
    await env.call("proxmox_set_ha_armed", { action: "disarm", resource_mode: "freeze" });
    let w = env.mock.lastWrite()!;
    assert.deepEqual([w.method, w.path], ["POST", "/cluster/ha/status/disarm-ha"]);
    assert.deepEqual(w.body, { "resource-mode": "freeze" });
    const out = await env.call("proxmox_set_ha_armed", { action: "arm" });
    w = env.mock.lastWrite()!;
    assert.equal(w.path, "/cluster/ha/status/arm-ha");
    assert.equal(out.requested, "arm");
  });
});
