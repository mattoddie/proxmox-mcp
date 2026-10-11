import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { errorText, startEnv, type TestEnv } from "./helpers.js";
import { GUESTS, STORAGES } from "./mock-proxmox.js";

const POOLS: Record<string, any> = {
  production: { poolid: "production", comment: "Prod", members: [GUESTS[0], GUESTS[2], STORAGES[1]] },
  empty: { poolid: "empty" },
};

describe("pools tools", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv();
  });
  after(() => env.stop());
  beforeEach(() => {
    env.mock.reset();
    env.mock.on("GET", /^\/pools$/, (p) => {
      if (!p.poolid) return Object.values(POOLS).map((x) => ({ poolid: x.poolid, comment: x.comment }));
      const pool = POOLS[p.poolid];
      if (!pool) throw env.mock.error(404, `pool '${p.poolid}' does not exist`);
      const members = (pool.members ?? []).filter((m: any) => !p.type || m.type === p.type);
      return [structuredClone({ ...pool, members })];
    });
  });

  it("lists pools with member counts", async () => {
    const res = await env.call("proxmox_list_pools");
    assert.equal(res.total, 2);
    assert.deepEqual(res.items[1], { poolid: "production", comment: "Prod", members: { vms: 1, containers: 1, storage: 1 } });
    assert.deepEqual(res.items[0].members, { vms: 0, containers: 0, storage: 0 });
    const searched = await env.call("proxmox_list_pools", { search: "prod" });
    assert.equal(searched.total, 1);
  });

  it("gets a pool with summarized members", async () => {
    const res = await env.call("proxmox_get_pool", { poolid: "production" });
    assert.equal(env.mock.requestsTo("/pools", "GET")[0].query.poolid, "production");
    assert.deepEqual(res.counts, { vms: 1, containers: 1, storage: 1 });
    assert.equal(res.guests[0].vmid, 100);
    assert.equal(res.guests[0].type, "vm");
    assert.equal(res.guests[1].type, "container");
    assert.equal(res.storage[0].storage, "local-lvm");
    assert.equal(res.storage[0].used_pct, 12.5);

    const onlyStorage = await env.call("proxmox_get_pool", { poolid: "production", type: "storage" });
    assert.equal(onlyStorage.guests, undefined);
    assert.equal(env.mock.requests.at(-1)!.query.type, "storage");

    const raw = await env.call("proxmox_get_pool", { poolid: "production", raw: true });
    assert.equal(raw.members.length, 3);

    assert.match(errorText(await env.callRaw("proxmox_get_pool", { poolid: "nope" })), /does not exist/);
  });

  it("creates pools", async () => {
    await env.call("proxmox_create_pool", { poolid: "lab", comment: "Testing" });
    const w = env.mock.lastWrite()!;
    assert.deepEqual([w.method, w.path, w.body], ["POST", "/pools", { poolid: "lab", comment: "Testing" }]);
  });

  it("adds and removes members in separate requests", async () => {
    const res = await env.call("proxmox_update_pool", {
      poolid: "production",
      comment: "Production",
      add_vms: [101],
      add_storage: ["local"],
      remove_vms: [200],
      allow_move: true,
    });
    const puts = env.mock.requestsTo("/pools", "PUT");
    assert.equal(puts.length, 2);
    assert.deepEqual(puts[0].body, { poolid: "production", comment: "Production", vms: "101", storage: "local", "allow-move": "1" });
    assert.deepEqual(puts[1].body, { poolid: "production", vms: "200", delete: "1" });
    assert.deepEqual(res.added, { vms: [101], storage: ["local"] });
    assert.deepEqual(res.removed, { vms: [200] });
  });

  it("only sends a removal request when only removing", async () => {
    await env.call("proxmox_update_pool", { poolid: "production", remove_storage: ["local-lvm"] });
    const puts = env.mock.requestsTo("/pools", "PUT");
    assert.equal(puts.length, 1);
    assert.deepEqual(puts[0].body, { poolid: "production", storage: "local-lvm", delete: "1" });
    assert.match(errorText(await env.callRaw("proxmox_update_pool", { poolid: "production" })), /Nothing to change/);
  });

  it("deletes pools via the poolid query parameter", async () => {
    await env.call("proxmox_delete_pool", { poolid: "parent/child" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "DELETE");
    assert.equal(w.path, "/pools");
    assert.deepEqual(w.query, { poolid: "parent/child" });
  });
});
