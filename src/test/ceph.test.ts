import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { errorText, startEnv, type TestEnv } from "./helpers.js";

const STATUS = {
  fsid: "f-1",
  health: {
    status: "HEALTH_WARN",
    checks: {
      OSD_DOWN: { severity: "HEALTH_WARN", summary: { message: "1 osds down", count: 1 }, detail: [{ message: "osd.2 is down" }] },
      POOL_NO_REDUNDANCY: { severity: "HEALTH_WARN", summary: { message: "1 pool(s) have no replicas configured" }, muted: true },
    },
  },
  quorum_names: ["pve1", "pve2"],
  monmap: { mons: [{ name: "pve1" }, { name: "pve2" }, { name: "pve3" }] },
  mgrmap: { available: true, active_name: "pve1", standbys: [{ name: "pve2" }] },
  osdmap: { num_osds: 3, num_up_osds: 2, num_in_osds: 3, num_remapped_pgs: 0 },
  pgmap: {
    num_pgs: 129,
    pgs_by_state: [
      { state_name: "active+clean", count: 100 },
      { state_name: "active+undersized+degraded", count: 29 },
    ],
    bytes_used: 1099511627776,
    bytes_total: 4398046511104,
    bytes_avail: 3298534883328,
    data_bytes: 366503875925,
    degraded_ratio: 0.123,
    read_bytes_sec: 1048576,
    write_op_per_sec: 12,
    num_pools: 2,
  },
};

const OSD_TREE = {
  flags: "noout",
  root: {
    leaf: 0,
    children: [
      {
        id: -1,
        name: "default",
        type: "root",
        children: [
          {
            id: -3,
            name: "pve1",
            type: "host",
            children: [
              { id: 0, name: "osd.0", type: "osd", status: "up", in: 1, device_class: "ssd", bytes_used: 107374182400, total_space: 1099511627776, percent_used: 9.77, crush_weight: 1, osdtype: "bluestore", version: "19.2.1", apply_latency_ms: 1, commit_latency_ms: 2 },
              { id: 1, name: "osd.1", type: "osd", status: "up", in: 1, device_class: "hdd", crush_weight: 4 },
            ],
          },
          {
            id: -5,
            name: "pve2",
            type: "host",
            children: [{ id: 2, name: "osd.2", type: "osd", status: "down", in: 0, device_class: "hdd", host: "pve2" }],
          },
        ],
      },
    ],
  },
};

describe("ceph tools", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv();
  });
  after(async () => {
    await env.stop();
  });
  beforeEach(() => {
    env.mock.reset();
    env.mock.on("GET", /^\/nodes\/[^/]+\/ceph\/osd$/, () => structuredClone(OSD_TREE));
  });

  it("summarizes Ceph status", async () => {
    env.mock.on("GET", /^\/cluster\/ceph\/status$/, () => STATUS);
    env.mock.on("GET", /^\/cluster\/ceph\/health-mute$/, () => [{ code: "POOL_NO_REDUNDANCY", sticky: false }]);
    const s = await env.call("proxmox_get_ceph_status", {});
    assert.equal(s.health, "HEALTH_WARN");
    assert.deepEqual(s.checks[0], { code: "OSD_DOWN", severity: "HEALTH_WARN", summary: "1 osds down", count: 1, detail: ["osd.2 is down"] });
    assert.equal(s.checks[1].muted, true);
    assert.deepEqual(s.monitors, { total: 3, in_quorum: ["pve1", "pve2"], out_of_quorum: ["pve3"] });
    assert.deepEqual(s.managers, { active: "pve1", available: true, standbys: ["pve2"] });
    assert.deepEqual(s.osds, { total: 3, up: 2, in: 3, down: 1 });
    assert.equal(s.pgs.total, 129);
    assert.equal(s.pgs.states["active+clean"], 100);
    assert.equal(s.pgs.degraded_pct, 12.3);
    assert.equal(s.usage.raw, "1 TiB of 4 TiB (25%)");
    assert.equal(s.io.read, "1 MiB/s");
    assert.deepEqual(s.muted_checks, [{ code: "POOL_NO_REDUNDANCY" }]);

    const raw = await env.call("proxmox_get_ceph_status", { raw: true });
    assert.equal(raw.fsid, "f-1");
  });

  it("handles the pre-Quincy nested osdmap and falls back to the node status", async () => {
    env.mock.on("GET", /^\/cluster\/ceph\/status$/, () => env.mock.error(501, "not implemented"));
    env.mock.on("GET", /^\/nodes\/pve1\/ceph\/status$/, () => ({ health: { status: "HEALTH_OK" }, osdmap: { osdmap: { num_osds: 4, num_up_osds: 4, num_in_osds: 4 } } }));
    const s = await env.call("proxmox_get_ceph_status", {});
    assert.equal(s.health, "HEALTH_OK");
    assert.deepEqual(s.osds, { total: 4, up: 4, in: 4, down: 0 });
  });

  it("lists OSDs with host, state and usage, with filters", async () => {
    const res = await env.call("proxmox_list_ceph_osds", {});
    assert.equal(env.mock.requestsTo("/nodes/pve1/ceph/osd", "GET").length, 1, "queries the first online node");
    assert.equal(res.flags, "noout");
    assert.deepEqual(res.summary, { total: 3, up: 2, in: 2, hosts: ["pve1", "pve2"] });
    const osd0 = res.items[0];
    assert.equal(osd0.host, "pve1");
    assert.equal(osd0.in, true);
    assert.equal(osd0.usage, "100 GiB of 1 TiB (9.8%)");
    assert.equal(osd0.used_pct, 9.8);
    assert.deepEqual(osd0.latency_ms, { apply: 1, commit: 2 });
    assert.equal(res.items[2].host, "pve2");

    const down = await env.call("proxmox_list_ceph_osds", { status: "down" });
    assert.deepEqual(
      down.items.map((o: any) => o.id),
      [2],
    );
    const out = await env.call("proxmox_list_ceph_osds", { status: "out" });
    assert.deepEqual(
      out.items.map((o: any) => o.id),
      [2],
    );
    const hdd = await env.call("proxmox_list_ceph_osds", { device_class: "hdd", host: "pve1" });
    assert.deepEqual(
      hdd.items.map((o: any) => o.name),
      ["osd.1"],
    );
  });

  it("gets OSD details from the hosting node", async () => {
    env.mock.on("GET", /^\/nodes\/pve2\/ceph\/osd\/2\/metadata$/, () => ({ osd: { hostname: "pve2", osd_objectstore: "bluestore" } }));
    env.mock.on("GET", /^\/nodes\/pve2\/ceph\/osd\/2\/lv-info$/, () => ({ lv_name: "osd-block-x", lv_size: 100 }));
    const res = await env.call("proxmox_get_ceph_osd", { osdid: 2 });
    assert.equal(res.node, "pve2");
    assert.equal(res.metadata.osd.osd_objectstore, "bluestore");
    assert.equal(res.lv_info.lv_name, "osd-block-x");
  });

  it("fails clearly for unknown OSDs", async () => {
    const err = errorText(await env.callRaw("proxmox_control_ceph_osd", { osdid: 42, action: "out" }));
    assert.match(err, /OSD 42 not found/);
  });

  it("lists pools with replication, PGs and usage", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/ceph\/pool$/, () => [
      { pool_name: "vm", pool: 2, type: "replicated", size: 3, min_size: 2, pg_num: 64, pg_num_final: 128, pg_autoscale_mode: "on", application_metadata: { rbd: {} }, crush_rule: 0, crush_rule_name: "replicated_rule", bytes_used: 1073741824, percent_used: 0.0512 },
      { pool_name: ".mgr", pool: 1, type: "replicated", size: 3, min_size: 2, pg_num: 1, crush_rule: 0, application_metadata: { mgr: {} } },
    ]);
    const res = await env.call("proxmox_list_ceph_pools", {});
    assert.deepEqual(
      res.items.map((p: any) => p.name),
      [".mgr", "vm"],
    );
    const vm = res.items[1];
    assert.equal(vm.application, "rbd");
    assert.equal(vm.crush_rule, "replicated_rule");
    assert.equal(vm.used, "1 GiB");
    assert.equal(vm.used_pct, 5.1);
    assert.equal(vm.pg_num_optimal, 128);
    assert.equal(vm.autoscale, "on");
  });

  it("gets a pool with statistics", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/ceph\/pool\/vm\/status$/, (p) => ({ name: "vm", size: 3, verbose: p.verbose }));
    const p = await env.call("proxmox_get_ceph_pool", { name: "vm" });
    assert.equal(p.verbose, "1");
  });

  it("lists monitors, managers and MDS", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/ceph\/mon$/, () => [{ name: "pve2", host: "pve2", quorum: 1, rank: 1, ceph_version_short: "19.2.1", service: 1 }, { name: "pve1", quorum: 0, state: "stopped" }]);
    env.mock.on("GET", /^\/nodes\/pve1\/ceph\/mgr$/, () => [{ name: "pve1", state: "active" }]);
    env.mock.on("GET", /^\/nodes\/pve1\/ceph\/mds$/, () => env.mock.error(500, "no mds"));
    const res = await env.call("proxmox_list_ceph_services", {});
    assert.deepEqual(res.mon[0], { name: "pve1", state: "stopped", quorum: false });
    assert.deepEqual(res.mon[1], { name: "pve2", host: "pve2", quorum: true, rank: 1, version: "19.2.1", service_enabled: true });
    assert.equal(res.mgr[0].state, "active");
    assert.match(res.mds.error, /no mds/);
    const mgrOnly = await env.call("proxmox_list_ceph_services", { type: "mgr", node: "pve2" });
    assert.deepEqual(Object.keys(mgrOnly), ["mgr"]);
  });

  it("lists CephFS", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/ceph\/fs$/, () => [{ name: "cephfs", metadata_pool: "cephfs_metadata", data_pool: "cephfs_data" }]);
    const res = await env.call("proxmox_list_ceph_fs", {});
    assert.deepEqual(res, [{ name: "cephfs", metadata_pool: "cephfs_metadata", data_pools: ["cephfs_data"] }]);
  });

  it("gets flags", async () => {
    env.mock.on("GET", /^\/cluster\/ceph\/flags$/, () => [
      { name: "noout", value: true, description: "OSDs will not automatically be marked out" },
      { name: "noscrub", value: false, description: "Scrubbing is disabled." },
    ]);
    const res = await env.call("proxmox_get_ceph_flags", {});
    assert.deepEqual(res.set, ["noout"]);
    assert.equal(res.flags[1].set, false);
  });

  it("sets flags as a task and maps flag names", async () => {
    const upid = env.mock.upid("pve1", "cephsetflags", "");
    env.mock.on("PUT", /^\/cluster\/ceph\/flags$/, () => upid);
    env.mock.on("GET", /^\/cluster\/ceph\/flags$/, () => [{ name: "noout", value: true }]);
    const res = await env.call("proxmox_set_ceph_flags", { noout: true, nodeep_scrub: false });
    const w = env.mock.lastWrite()!;
    assert.equal(w.path, "/cluster/ceph/flags");
    assert.deepEqual(w.body, { noout: "1", "nodeep-scrub": "0" });
    assert.equal(res.status, "ok");
    assert.deepEqual(res.now_set, ["noout"]);
    assert.match(errorText(await env.callRaw("proxmox_set_ceph_flags", {})), /at least one flag/);
  });

  it("reads the CRUSH map and rules", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/ceph\/rules$/, () => [{ name: "replicated_rule" }, { name: "ssd_only" }]);
    env.mock.on("GET", /^\/nodes\/pve1\/ceph\/crush$/, () => "# begin crush map\nrule replicated_rule {}");
    const res = await env.call("proxmox_get_ceph_crush", {});
    assert.deepEqual(res.rules, ["replicated_rule", "ssd_only"]);
    assert.match(res.crush_map, /begin crush map/);
    const rules = await env.call("proxmox_get_ceph_crush", { rules_only: true });
    assert.equal(rules.crush_map, undefined);
  });

  it("reads the Ceph config DB and file", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/ceph\/cfg\/db$/, () => [
      { section: "global", name: "osd_pool_default_size", value: "3", level: "basic", can_update_at_runtime: true },
      { section: "mon", name: "auth_allow_insecure_global_id_reclaim", value: "false" },
      { section: "mgr", name: "mgr/dashboard/password", value: "dashpw" },
      { section: "client.rgw", name: "rgw_keystone_admin_password", value: "kspw" },
    ]);
    env.mock.on("GET", /^\/nodes\/pve1\/ceph\/cfg\/raw$/, () => "[global]\n\tfsid = f-1\n[client.x]\n\tkey = AQBsecret==\n");
    const res = await env.call("proxmox_get_ceph_config", { search: "pool" });
    assert.equal(res.db.length, 1);
    assert.equal(res.db[0].runtime, true);
    assert.match(res.file, /fsid = f-1/);
    assert.doesNotMatch(res.file, /AQBsecret/);
    const dbOnly = await env.call("proxmox_get_ceph_config", { source: "db" });
    assert.equal(dbOnly.file, undefined);
    assert.equal(dbOnly.db.length, 4);
    assert.ok(!JSON.stringify(dbOnly).includes("dashpw") && !JSON.stringify(dbOnly).includes("kspw"));
    assert.equal(dbOnly.db[0].value, "3");
  });

  it("reads the Ceph log", async () => {
    env.mock.on("GET", /^\/nodes\/pve2\/ceph\/log$/, (p) => [
      { n: 1, t: `limit=${p.limit} start=${p.start}` },
      { n: 2, t: "osd.2 marked down" },
    ]);
    const res = await env.call("proxmox_get_ceph_log", { node: "pve2", start: 10, limit: 2 });
    assert.deepEqual(res.items, ["limit=2 start=10", "osd.2 marked down"]);
    const found = await env.call("proxmox_get_ceph_log", { node: "pve2", search: "down" });
    assert.deepEqual(found.items, ["osd.2 marked down"]);
  });

  it("summarizes daemon metadata", async () => {
    env.mock.on("GET", /^\/cluster\/ceph\/metadata$/, () => ({
      mon: { "pve1": { hostname: "pve1", ceph_version_short: "19.2.1", ceph_release: "squid", mem_total_kb: 1024 } },
      osd: { "0": { hostname: "pve1", ceph_version_short: "18.2.4" } },
    }));
    const res = await env.call("proxmox_get_ceph_metadata", {});
    assert.deepEqual(res.mon, [{ id: "pve1", host: "pve1", version: "19.2.1", release: "squid", memory: "1 MiB" }]);
    assert.equal(res.osd[0].version, "18.2.4");
  });

  it("checks command safety", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/ceph\/cmd-safety$/, (p) => ({ safe: false, status: `${p.service}.${p.id} ${p.action}: 12 pgs would be inactive` }));
    const res = await env.call("proxmox_check_ceph_safety", { service: "osd", id: "1", action: "destroy" });
    assert.equal(res.safe, false);
    assert.match(res.status, /osd.1 destroy/);
  });

  it("marks OSDs in/out and scrubs them on their host", async () => {
    await env.call("proxmox_control_ceph_osd", { osdid: 2, action: "out" });
    assert.equal(env.mock.lastWrite()!.path, "/nodes/pve2/ceph/osd/2/out");
    await env.call("proxmox_control_ceph_osd", { osdid: 0, action: "in", node: "pve1" });
    assert.equal(env.mock.lastWrite()!.path, "/nodes/pve1/ceph/osd/0/in");
    await env.call("proxmox_control_ceph_osd", { osdid: 0, action: "deep-scrub" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.path, "/nodes/pve1/ceph/osd/0/scrub");
    assert.deepEqual(w.body, { deep: "1" });
  });

  it("creates and destroys OSDs as tasks", async () => {
    const upid = env.mock.upid("pve1", "cephcreateosd", "sdb");
    env.mock.on("POST", /^\/nodes\/pve1\/ceph\/osd$/, () => upid);
    const res = await env.call("proxmox_create_ceph_osd", { node: "pve1", dev: "/dev/sdb", device_class: "ssd", db_dev: "/dev/nvme0n1", db_dev_size: 60, encrypted: true });
    assert.deepEqual(env.mock.lastWrite()!.body, { dev: "/dev/sdb", "crush-device-class": "ssd", db_dev: "/dev/nvme0n1", db_dev_size: "60", encrypted: "1" });
    assert.equal(res.status, "ok");

    const del = env.mock.upid("pve2", "cephdestroyosd", "2");
    env.mock.on("DELETE", /^\/nodes\/pve2\/ceph\/osd\/2$/, () => del);
    const d = await env.call("proxmox_delete_ceph_osd", { osdid: 2, cleanup: true, wait: false });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "DELETE");
    assert.equal(w.query.cleanup, "1");
    assert.equal(d.status, "started");
    assert.equal(d.node, "pve2");
  });

  it("surfaces a failed OSD creation", async () => {
    const upid = env.mock.upid("pve1", "cephcreateosd", "sdb", { status: "stopped", exitstatus: "device is in use", log: ["sdb has partitions"] });
    env.mock.on("POST", /^\/nodes\/pve1\/ceph\/osd$/, () => upid);
    const err = errorText(await env.callRaw("proxmox_create_ceph_osd", { node: "pve1", dev: "/dev/sdb" }));
    assert.match(err, /device is in use/);
  });

  it("creates, updates and deletes pools", async () => {
    const upid = env.mock.upid("pve1", "cephcreatepool", "vm2");
    env.mock.on("POST", /^\/nodes\/pve1\/ceph\/pool$/, () => upid);
    await env.call("proxmox_create_ceph_pool", { name: "vm2", size: 2, min_size: 1, pg_autoscale_mode: "on", add_storages: true, erasure_coding: "k=2,m=1" });
    assert.deepEqual(env.mock.lastWrite()!.body, { name: "vm2", size: "2", min_size: "1", pg_autoscale_mode: "on", add_storages: "1", "erasure-coding": "k=2,m=1" });

    const put = env.mock.upid("pve1", "cephsetpool", "vm2");
    env.mock.on("PUT", /^\/nodes\/pve1\/ceph\/pool\/vm2$/, () => put);
    const u = await env.call("proxmox_update_ceph_pool", { name: "vm2", pg_num: 128, crush_rule: "ssd_only" });
    assert.deepEqual(env.mock.lastWrite()!.body, { pg_num: "128", crush_rule: "ssd_only" });
    assert.deepEqual(u.changed, ["pg_num", "crush_rule"]);
    assert.match(errorText(await env.callRaw("proxmox_update_ceph_pool", { name: "vm2" })), /Nothing to change/);

    await env.call("proxmox_delete_ceph_pool", { name: "vm2", remove_storages: true });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "DELETE");
    assert.equal(w.path, "/nodes/pve1/ceph/pool/vm2");
    assert.deepEqual(w.query, { remove_storages: "1" });
  });

  it("creates and destroys monitors, managers and MDS", async () => {
    await env.call("proxmox_create_ceph_service", { type: "mon", node: "pve2", mon_address: ["10.0.0.12"], hotstandby: true });
    let w = env.mock.lastWrite()!;
    assert.equal(w.path, "/nodes/pve2/ceph/mon/pve2");
    assert.deepEqual(w.body, { "mon-address": "10.0.0.12" });
    await env.call("proxmox_create_ceph_service", { type: "mds", node: "pve1", id: "pve1-b", hotstandby: true });
    w = env.mock.lastWrite()!;
    assert.equal(w.path, "/nodes/pve1/ceph/mds/pve1-b");
    assert.deepEqual(w.body, { hotstandby: "1" });
    await env.call("proxmox_delete_ceph_service", { type: "mgr", node: "pve2", id: "pve2" });
    w = env.mock.lastWrite()!;
    assert.equal(w.method, "DELETE");
    assert.equal(w.path, "/nodes/pve2/ceph/mgr/pve2");
  });

  it("creates and destroys CephFS", async () => {
    const res = await env.call("proxmox_create_ceph_fs", { node: "pve1", add_storage: true });
    assert.equal(env.mock.lastWrite()!.path, "/nodes/pve1/ceph/fs/cephfs");
    assert.deepEqual(env.mock.lastWrite()!.body, { "add-storage": "1" });
    assert.equal(res.fs, "cephfs");
    await env.call("proxmox_delete_ceph_fs", { node: "pve1", name: "cephfs", remove_pools: true });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "DELETE");
    assert.deepEqual(w.query, { "remove-pools": "1" });
  });

  it("mutes and unmutes health checks", async () => {
    await env.call("proxmox_mute_ceph_health_check", { code: "OSD_NEARFULL", ttl: "2h" });
    let w = env.mock.lastWrite()!;
    assert.equal(w.path, "/cluster/ceph/health-mute/OSD_NEARFULL");
    assert.deepEqual(w.body, { value: "1", ttl: "2h" });
    await env.call("proxmox_mute_ceph_health_check", { code: "OSD_NEARFULL", mute: false, ttl: "2h" });
    w = env.mock.lastWrite()!;
    assert.deepEqual(w.body, { value: "0" });
  });

  it("starts, stops and restarts services as tasks", async () => {
    const upid = env.mock.upid("pve1", "srvrestart", "osd.0");
    env.mock.on("POST", /^\/nodes\/pve1\/ceph\/restart$/, () => upid);
    const res = await env.call("proxmox_control_ceph_service", { action: "restart", node: "pve1", service: "osd.0" });
    assert.deepEqual(env.mock.lastWrite()!.body, { service: "osd.0" });
    assert.equal(res.status, "ok");
    // systemd unit names are translated to the <type>.<id> form the API accepts
    await env.call("proxmox_control_ceph_service", { action: "restart", node: "pve1", service: "ceph-mon@pve1.service" });
    assert.deepEqual(env.mock.lastWrite()!.body, { service: "mon.pve1" });
    const stop = await env.call("proxmox_control_ceph_service", { action: "stop", node: "pve2" });
    assert.equal(env.mock.lastWrite()!.path, "/nodes/pve2/ceph/stop");
    assert.equal(stop.service, "ceph.target");
  });

  it("initialises the Ceph config", async () => {
    await env.call("proxmox_init_ceph", { node: "pve1", network: "10.10.0.0/24", cluster_network: "10.20.0.0/24" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.path, "/nodes/pve1/ceph/init");
    assert.deepEqual(w.body, { network: "10.10.0.0/24", "cluster-network": "10.20.0.0/24" });
  });

  it("uses PROXMOX_DEFAULT_NODE for cluster-wide reads", async () => {
    const env2 = await startEnv({ PROXMOX_DEFAULT_NODE: "pve2" });
    try {
      env2.mock.on("GET", /^\/nodes\/[^/]+\/ceph\/osd$/, () => structuredClone(OSD_TREE));
      await env2.call("proxmox_list_ceph_osds", {});
      assert.equal(env2.mock.requestsTo("/nodes/pve2/ceph/osd", "GET").length, 1);
    } finally {
      await env2.stop();
    }
  });
});
