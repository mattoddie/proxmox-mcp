import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { errorText, startEnv, type TestEnv } from "./helpers.js";

const now = Math.floor(Date.now() / 1000);
const DAY = 86400;

const VM_SNAPS = [
  { name: "base", description: "fresh install\n", snaptime: now - 40 * DAY },
  { name: "pre_upgrade", parent: "base", description: "before upgrade", snaptime: now - 10 * DAY, vmstate: 1 },
  { name: "experiment", parent: "base", snaptime: now - 5 * DAY },
  { name: "current", parent: "pre_upgrade", description: "You are here!", running: 1 },
];

describe("snapshots tools", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv();
  });
  after(() => env.stop());
  beforeEach(() => env.mock.reset());

  it("lists a VM's snapshots as a tree with the current marker", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/qemu\/100\/snapshot$/, () => VM_SNAPS);
    const res = await env.call("proxmox_list_snapshots", { vmid: 100 });
    assert.equal(res.type, "vm");
    assert.equal(res.count, 3);
    assert.equal(res.current_snapshot, "pre_upgrade");
    assert.deepEqual(
      res.snapshots.map((s: any) => s.name),
      ["base", "pre_upgrade", "experiment"],
    );
    const pre = res.snapshots[1];
    assert.equal(pre.current, true);
    assert.equal(pre.vmstate, true);
    assert.equal(pre.parent, "base");
    assert.equal(pre.age_days, 10);
    assert.equal(res.snapshots[0].description, "fresh install");
    assert.equal(res.tree.length, 3);
    assert.match(res.tree[0], /^base \[/);
    assert.match(res.tree[1], /^ {2}pre_upgrade \[.*\] \(current\)$/);
    assert.match(res.tree[2], /^ {2}experiment/);
  });

  it("lists a container's snapshots using the lxc path", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/lxc\/200\/snapshot$/, () => [{ name: "current", description: "You are here!" }]);
    const res = await env.call("proxmox_list_snapshots", { vmid: 200 });
    assert.equal(res.type, "container");
    assert.equal(res.count, 0);
    assert.equal(res.current_snapshot, undefined);
  });

  it("gets a snapshot's config", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/qemu\/100\/snapshot\/pre_upgrade\/config$/, () => ({
      description: "before upgrade",
      snaptime: now - DAY,
      parent: "base",
      vmstate: "local-lvm:vm-100-state-pre_upgrade",
      memory: 4096,
      scsi0: "local-lvm:vm-100-disk-0,size=32G",
      net0: "virtio=BC:24:11:00:00:01,bridge=vmbr0",
      cipassword: "secret",
    }));
    const res = await env.call("proxmox_get_snapshot", { vmid: 100, snapname: "pre_upgrade" });
    assert.equal(res.snapname, "pre_upgrade");
    assert.equal(res.vmstate, true);
    assert.equal(res.parent, "base");
    assert.equal(res.disks.scsi0.size, "32G");
    assert.equal(res.nets.net0.macaddr, "BC:24:11:00:00:01");
    assert.equal(res.config.cipassword, "<redacted>");
    assert.equal(res.config.description, undefined);
  });

  it("lists snapshots across guests, filtered by age, with per-guest errors", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/qemu\/100\/snapshot$/, () => VM_SNAPS);
    env.mock.on("GET", /^\/nodes\/pve1\/lxc\/200\/snapshot$/, () => [
      { name: "old_ct", snaptime: now - 100 * DAY, description: "ancient" },
      { name: "current", parent: "old_ct" },
    ]);
    env.mock.on("GET", /^\/nodes\/pve2\/qemu\/101\/snapshot$/, () => env.mock.error(595, "no route to host"));
    env.mock.on("GET", /^\/nodes\/pve1\/qemu\/9000\/snapshot$/, () => [{ name: "current" }]);
    const res = await env.call("proxmox_list_all_snapshots", { older_than_days: 7 });
    assert.equal(res.guests_checked, 4);
    assert.deepEqual(
      res.items.map((s: any) => `${s.vmid}/${s.name}`),
      ["200/old_ct", "100/base", "100/pre_upgrade"],
    );
    assert.equal(res.items[0].type, "container");
    assert.equal(res.items[0].guest, "dns");
    assert.equal(res.items[0].current, true);
    assert.equal(res.items[0].snaptime, undefined);
    assert.equal(res.errors.length, 1);
    assert.equal(res.errors[0].vmid, 101);

    const onlyCt = await env.call("proxmox_list_all_snapshots", { type: "container" });
    assert.equal(onlyCt.guests_checked, 1);
    assert.equal(onlyCt.total, 1);
    const search = await env.call("proxmox_list_all_snapshots", { vmids: [100], search: "upgrade" });
    assert.equal(search.total, 1);
  });

  it("creates a snapshot with vmstate for VMs only", async () => {
    env.mock.on("POST", /^\/nodes\/pve1\/qemu\/100\/snapshot$/, () => env.mock.upid("pve1", "qmsnapshot", "100"));
    const res = await env.call("proxmox_create_snapshot", { vmid: 100, snapname: "pre_upgrade", description: "before", vmstate: true });
    assert.equal(res.status, "ok");
    assert.deepEqual(env.mock.lastWrite()!.body, { snapname: "pre_upgrade", description: "before", vmstate: "1" });

    assert.match(errorText(await env.callRaw("proxmox_create_snapshot", { vmid: 200, snapname: "x1", vmstate: true })), /only supported for VMs/);
    assert.match(errorText(await env.callRaw("proxmox_create_snapshot", { vmid: 9000, snapname: "x1" })), /template/);
    assert.ok(errorText(await env.callRaw("proxmox_create_snapshot", { vmid: 100, snapname: "1bad name" })));

    env.mock.on("POST", /^\/nodes\/pve1\/lxc\/200\/snapshot$/, () => env.mock.upid("pve1", "vzsnapshot", "200"));
    await env.call("proxmox_create_snapshot", { vmid: 200, snapname: "ct_snap" });
    assert.equal(env.mock.lastWrite()!.path, "/nodes/pve1/lxc/200/snapshot");
  });

  it("rolls back with start", async () => {
    env.mock.on("POST", /\/lxc\/200\/snapshot\/old_ct\/rollback$/, () => env.mock.upid("pve1", "vzrollback", "200"));
    const res = await env.call("proxmox_rollback_snapshot", { vmid: 200, snapname: "old_ct", start: true });
    assert.equal(res.rolled_back, true);
    assert.deepEqual(env.mock.lastWrite()!.body, { start: "1" });
  });

  it("updates a snapshot description", async () => {
    await env.call("proxmox_update_snapshot", { vmid: 101, snapname: "base", description: "" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, "/nodes/pve2/qemu/101/snapshot/base/config");
    assert.deepEqual(w.body, { description: "" });
  });

  it("deletes a snapshot, reporting task failures", async () => {
    env.mock.on("DELETE", /\/qemu\/100\/snapshot\/base$/, () => env.mock.upid("pve1", "qmdelsnapshot", "100"));
    const res = await env.call("proxmox_delete_snapshot", { vmid: 100, snapname: "base", force: true });
    assert.equal(res.deleted, true);
    assert.deepEqual(env.mock.lastWrite()!.query, { force: "1" });

    env.mock.on("DELETE", /\/qemu\/100\/snapshot\/bad$/, () =>
      env.mock.upid("pve1", "qmdelsnapshot", "100", { status: "stopped", exitstatus: "zfs error", log: ["could not destroy"] }),
    );
    assert.match(errorText(await env.callRaw("proxmox_delete_snapshot", { vmid: 100, snapname: "bad" })), /zfs error/);
    const started = await env.call("proxmox_delete_snapshot", { vmid: 100, snapname: "base", wait: false });
    assert.equal(started.status, "started");
  });

  it("errors for unknown guests", async () => {
    assert.match(errorText(await env.callRaw("proxmox_list_snapshots", { vmid: 4242 })), /No VM or container with ID 4242/);
  });
});
