import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { errorText, startEnv, type TestEnv } from "./helpers.js";

const STORAGE_CONFIG = [
  { storage: "local", type: "dir", path: "/var/lib/vz", content: "iso,vztmpl,backup", "prune-backups": "keep-last=3,keep-daily=7", digest: "d1" },
  { storage: "local-lvm", type: "lvmthin", vgname: "pve", thinpool: "data", content: "images,rootdir", digest: "d1" },
  { storage: "pbs", type: "pbs", server: "10.0.0.50", datastore: "main", username: "backup@pbs", password: "s3cret", "encryption-key": "{\"kdf\":null}", content: "backup", shared: 1, nodes: "pve1", digest: "d1" },
];

describe("storage tools", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv({ PROXMOX_DEFAULT_NODE: "pve1" });
  });
  after(() => env.stop());
  beforeEach(() => {
    env.mock.reset();
    env.mock.on("GET", /^\/storage$/, (p) => structuredClone(p.type ? STORAGE_CONFIG.filter((s) => s.type === p.type) : STORAGE_CONFIG));
  });

  // ---------------------------------------------------------------- definitions

  it("lists storage merged with per-node status", async () => {
    const res = await env.call("proxmox_list_storage");
    assert.equal(res.total, 3);
    const local = res.items.find((s: any) => s.storage === "local");
    assert.deepEqual(local.content, ["iso", "vztmpl", "backup"]);
    assert.deepEqual(local.prune_backups, { "keep-last": "3", "keep-daily": "7" });
    assert.deepEqual(local.status.map((s: any) => s.node), ["pve1", "pve2"]);
    assert.equal(local.status[0].used_pct, 10);
    assert.match(local.status[0].usage, /10 GiB of 100 GiB/);
    const pbs = res.items.find((s: any) => s.storage === "pbs");
    assert.equal(pbs.shared, true);
    assert.deepEqual(pbs.nodes, ["pve1"]);
    assert.equal(pbs.password, undefined);
  });

  it("filters storage by type, content and node, and redacts raw output", async () => {
    const byType = await env.call("proxmox_list_storage", { type: "lvmthin" });
    assert.deepEqual(byType.items.map((s: any) => s.storage), ["local-lvm"]);
    assert.equal(env.mock.requestsTo("/storage", "GET")[0].query.type, "lvmthin");
    const byContent = await env.call("proxmox_list_storage", { content: "backup" });
    assert.deepEqual(byContent.items.map((s: any) => s.storage), ["local", "pbs"]);
    const byNode = await env.call("proxmox_list_storage", { node: "pve2" });
    assert.deepEqual(byNode.items.map((s: any) => s.storage), ["local"]);
    const raw = await env.call("proxmox_list_storage", { raw: true, search: "pbs" });
    assert.equal(raw.items[0].password, "<redacted>");
    assert.equal(raw.items[0]["encryption-key"], "<redacted>");
  });

  it("gets one storage with config, status and node detail", async () => {
    env.mock.on("GET", /^\/storage\/pbs$/, () => structuredClone(STORAGE_CONFIG[2]));
    env.mock.on("GET", /^\/nodes\/pve1\/storage\/pbs\/status$/, () => ({ type: "pbs", active: 1, enabled: 1, shared: 1, content: "backup", used: 50, total: 100, avail: 50 }));
    const res = await env.call("proxmox_get_storage", { storage: "pbs", node: "pve1" });
    assert.equal(res.summary.datastore, "main");
    assert.equal(res.config.password, "<redacted>");
    assert.equal(res.config["encryption-key"], "<redacted>");
    assert.equal(res.node_status.active, true);
    assert.equal(res.node_status.available, "50 B");
  });

  it("creates storage with typed fields mapped to API names", async () => {
    env.mock.on("POST", /^\/storage$/, (p) => ({ storage: p.storage, type: p.type, config: { "encryption-key": "secretkey" } }));
    const res = await env.call("proxmox_save_storage", {
      storage: "nas",
      type: "pbs",
      server: "10.0.0.60",
      datastore: "store",
      username: "u@pbs",
      password: "pw",
      content: ["backup"],
      nodes: ["pve1", "pve2"],
      encryption_key: "autogen",
      prune_backups: { keep_last: 3, keep_daily: 7, keep_all: false },
      max_protected_backups: -1,
      port: 8008,
    });
    assert.equal(res.action, "created");
    assert.equal(res.result.config["encryption-key"], "<redacted>");
    assert.match(res.note, /\/etc\/pve\/priv\/storage\/nas\.enc/);
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.equal(w.path, "/storage");
    assert.equal(w.body.storage, "nas");
    assert.equal(w.body.content, "backup");
    assert.equal(w.body.nodes, "pve1,pve2");
    assert.equal(w.body["encryption-key"], "autogen");
    assert.equal(w.body["prune-backups"], "keep-all=0,keep-last=3,keep-daily=7");
    assert.equal(w.body["max-protected-backups"], "-1");
    assert.equal(w.body.port, "8008");
  });

  it("updates existing storage and rejects create-only fields", async () => {
    const res = await env.call("proxmox_save_storage", { storage: "local", content: ["iso", "backup"], disable: true, delete: ["prune-backups"], digest: "d1" });
    assert.equal(res.action, "updated");
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, "/storage/local");
    assert.deepEqual(w.body, { content: "iso,backup", disable: "1", delete: "prune-backups", digest: "d1" });

    const err = errorText(await env.callRaw("proxmox_save_storage", { storage: "local", path: "/srv", type: "dir" }));
    assert.match(err, /type, path can't be changed/);
    const noType = errorText(await env.callRaw("proxmox_save_storage", { storage: "new", path: "/srv" }));
    assert.match(noType, /pass `type`/);
  });

  it("deletes storage definitions", async () => {
    const res = await env.call("proxmox_delete_storage", { storage: "pbs" });
    assert.equal(res.deleted, true);
    assert.deepEqual([env.mock.lastWrite()!.method, env.mock.lastWrite()!.path], ["DELETE", "/storage/pbs"]);
  });

  // ---------------------------------------------------------------- content

  it("lists content with filters and summaries", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/storage\/local\/content$/, () => [
      { volid: "local:iso/debian.iso", content: "iso", format: "iso", size: 658505728, ctime: 1700000000 },
      { volid: "local:backup/vzdump-qemu-100-2024.vma.zst", content: "backup", format: "vma.zst", size: 1073741824, vmid: 100, ctime: 1700000000, notes: "nightly", protected: 1, verification: { state: "ok" } },
    ]);
    const res = await env.call("proxmox_list_storage_content", { storage: "local", content: "iso", vmid: 100 });
    const req = env.mock.requestsTo("/nodes/pve1/storage/local/content", "GET")[0];
    assert.deepEqual(req.query, { content: "iso", vmid: "100" });
    assert.equal(res.node, "pve1");
    const backup = res.items.find((v: any) => v.content === "backup");
    assert.deepEqual(backup, {
      volid: "local:backup/vzdump-qemu-100-2024.vma.zst",
      content: "backup",
      format: "vma.zst",
      size: "1 GiB",
      vmid: 100,
      ctime: "2023-11-14T22:13:20.000Z",
      notes: "nightly",
      protected: true,
      verification: "ok",
    });
    const searched = await env.call("proxmox_list_storage_content", { storage: "local", search: "debian" });
    assert.equal(searched.total, 1);
  });

  it("gets and updates volume attributes, deriving the storage from the volume ID", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/storage\/local\/content\/(.+)$/, (_p, m) => ({
      path: `/var/lib/vz/${decodeURIComponent(m[1])}`,
      format: "iso",
      size: 1024,
      used: 1024,
      protected: 0,
    }));
    const v = await env.call("proxmox_get_volume", { volume: "local:iso/debian.iso" });
    assert.equal(v.size, "1 KiB");
    assert.equal(v.protected, false);
    assert.equal(env.mock.requestsTo(/content\//, "GET")[0].path, "/nodes/pve1/storage/local/content/local%3Aiso%2Fdebian.iso");

    await env.call("proxmox_update_volume", { volume: "local:backup/x.vma.zst", notes: "keep", protected: true });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.deepEqual(w.body, { notes: "keep", protected: "1" });
    assert.match(errorText(await env.callRaw("proxmox_update_volume", { volume: "local:iso/x.iso" })), /Nothing to change/);
    assert.match(errorText(await env.callRaw("proxmox_get_volume", { volume: "noprefix" })), /Pass `storage`/);
  });

  it("deletes volumes as a task", async () => {
    const upid = env.mock.upid("pve1", "imgdel", "local-lvm:vm-100-disk-1");
    env.mock.on("DELETE", /^\/nodes\/pve1\/storage\/local-lvm\/content\//, () => upid);
    const res = await env.call("proxmox_delete_volume", { volume: "local-lvm:vm-100-disk-1" });
    assert.equal(res.status, "ok");
    assert.equal(res.task.upid, upid);
    const noWait = await env.call("proxmox_delete_volume", { volume: "local-lvm:vm-100-disk-1", wait: false });
    assert.equal(noWait.status, "started");
  });

  it("allocates disks", async () => {
    env.mock.on("POST", /^\/nodes\/pve2\/storage\/local-lvm\/content$/, (p) => `local-lvm:${p.filename}`);
    const res = await env.call("proxmox_allocate_disk", { node: "pve2", storage: "local-lvm", vmid: 101, filename: "vm-101-disk-2", size: "16G", format: "raw" });
    assert.equal(res.volid, "local-lvm:vm-101-disk-2");
    assert.deepEqual(env.mock.lastWrite()!.body, { vmid: "101", filename: "vm-101-disk-2", size: "16G", format: "raw" });
  });

  // ---------------------------------------------------------------- downloads

  it("downloads from a URL with checksum options", async () => {
    const upid = env.mock.upid("pve1", "download", "local");
    env.mock.on("POST", /download-url$/, () => upid);
    const res = await env.call("proxmox_download_to_storage", {
      storage: "local",
      url: "https://example.com/d.iso",
      content: "iso",
      filename: "d.iso",
      checksum: "abc",
      checksum_algorithm: "sha256",
      verify_certificates: false,
    });
    assert.equal(res.status, "ok");
    assert.equal(res.volid, "local:iso/d.iso");
    const w = env.mock.lastWrite()!;
    assert.equal(w.path, "/nodes/pve1/storage/local/download-url");
    assert.equal(w.body["checksum-algorithm"], "sha256");
    assert.equal(w.body["verify-certificates"], "0");
    const err = errorText(await env.callRaw("proxmox_download_to_storage", { storage: "local", url: "u", content: "iso", filename: "f", checksum: "x" }));
    assert.match(err, /checksum_algorithm/);
  });

  it("reports failed download tasks", async () => {
    const upid = env.mock.upid("pve1", "download", "local", { status: "stopped", exitstatus: "checksum mismatch", log: ["downloading", "checksum mismatch"] });
    env.mock.on("POST", /download-url$/, () => upid);
    const err = errorText(await env.callRaw("proxmox_download_to_storage", { storage: "local", url: "u", content: "vztmpl", filename: "f.tar.zst" }));
    assert.match(err, /failed: checksum mismatch/);
  });

  it("lists and downloads appliance templates", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/aplinfo$/, () => [
      { template: "debian-12-standard_12.7-1_amd64.tar.zst", section: "system", os: "debian-12", version: "12.7-1", headline: "Debian 12", type: "lxc", package: "debian-12-standard" },
      { template: "debian-12-turnkey-nextcloud_18.0-1_amd64.tar.gz", section: "turnkeylinux", os: "debian-12", headline: "TurnKey Nextcloud", type: "lxc" },
      { template: "alpine-3.20-default_20240908_amd64.tar.xz", section: "system", os: "alpine", headline: "Alpine", type: "lxc" },
    ]);
    const sys = await env.call("proxmox_list_appliance_templates", { section: "system" });
    assert.deepEqual(sys.items.map((t: any) => t.template), ["alpine-3.20-default_20240908_amd64.tar.xz", "debian-12-standard_12.7-1_amd64.tar.zst"]);
    const search = await env.call("proxmox_list_appliance_templates", { search: "nextcloud" });
    assert.equal(search.total, 1);

    const upid = env.mock.upid("pve1", "download", "local");
    env.mock.on("POST", /^\/nodes\/pve1\/aplinfo$/, () => upid);
    const res = await env.call("proxmox_download_appliance_template", { storage: "local", template: "alpine-3.20-default_20240908_amd64.tar.xz" });
    assert.equal(res.status, "ok");
    assert.deepEqual(env.mock.lastWrite()!.body, { storage: "local", template: "alpine-3.20-default_20240908_amd64.tar.xz" });
  });

  // ---------------------------------------------------------------- stats and scans

  it("summarizes storage RRD data", async () => {
    env.mock.on("GET", /rrddata$/, () => [
      { time: 1700000000, used: 100, total: 1000 },
      { time: 1700000060 },
      { time: 1700000120, used: 300, total: 1000 },
      { time: 1700000180, used: 200, total: 1000 },
    ]);
    const res = await env.call("proxmox_get_storage_stats", { storage: "local", timeframe: "week", samples: 2 });
    assert.equal(res.points, 3);
    assert.equal(res.min_used, "100 B");
    assert.equal(res.max_used, "300 B");
    assert.equal(res.growth, "+100 B");
    assert.equal(res.samples.length, 2);
    assert.equal(res.current, "200 B of 1000 B (20%)");
    assert.equal(env.mock.requestsTo(/rrddata/)[0].query.timeframe, "week");

    env.mock.on("GET", /rrddata$/, () => []);
    const empty = await env.call("proxmox_get_storage_stats", { storage: "local" });
    assert.equal(empty.points, 0);
  });

  it("scans for storage with type-specific parameters", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/scan\/(\w+)$/, (_p, m) => [{ kind: m[1] }]);
    const nfs = await env.call("proxmox_scan_storage", { type: "nfs", server: "nas" });
    assert.equal(nfs.count, 1);
    assert.deepEqual(env.mock.requests.at(-1)!.query, { server: "nas" });
    await env.call("proxmox_scan_storage", { type: "pbs", server: "pbs", username: "u@pbs", password: "p", port: 8007 });
    assert.equal(env.mock.requests.at(-1)!.path, "/nodes/pve1/scan/pbs");
    assert.equal(env.mock.requests.at(-1)!.query.port, "8007");
    await env.call("proxmox_scan_storage", { type: "lvmthin", vg: "pve" });
    assert.deepEqual(env.mock.requests.at(-1)!.query, { vg: "pve" });
    await env.call("proxmox_scan_storage", { type: "zfs" });
    assert.deepEqual(env.mock.requests.at(-1)!.query, {});
    assert.match(errorText(await env.callRaw("proxmox_scan_storage", { type: "pbs", server: "x" })), /requires `username`/);
    assert.match(errorText(await env.callRaw("proxmox_scan_storage", { type: "iscsi" })), /requires `portal`/);
  });

  // ---------------------------------------------------------------- disks

  it("lists disks with health and wearout", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/disks\/list$/, () => [
      { devpath: "/dev/sdb", type: "hdd", model: "WDC", serial: "S2", size: 4000787030016, rpm: 7200, health: "PASSED", wearout: "N/A", used: "ZFS", gpt: 1, osdid: -1, vendor: "ATA     " },
      { devpath: "/dev/nvme0n1", type: "nvme", model: "Samsung 980", serial: "S1", size: 1000204886016, rpm: 0, health: "PASSED", wearout: 97, used: "LVM", gpt: 1, osdid: -1 },
    ]);
    const res = await env.call("proxmox_list_disks", { type: "unused", include_partitions: true, skip_smart: true });
    assert.deepEqual(env.mock.requestsTo(/disks\/list/)[0].query, { type: "unused", "include-partitions": "1", skipsmart: "1" });
    assert.equal(res.count, 2);
    const [nvme, hdd] = res.disks;
    assert.equal(nvme.devpath, "/dev/nvme0n1");
    assert.equal(nvme.wearout_pct, 3);
    assert.equal(nvme.rpm, undefined);
    assert.equal(nvme.used_by, "LVM");
    assert.equal(hdd.wearout_pct, undefined);
    assert.equal(hdd.vendor, "ATA");
    assert.equal(hdd.size, "3.64 TiB");
    assert.equal(hdd.osdid, undefined);
  });

  it("gets SMART data", async () => {
    env.mock.on("GET", /disks\/smart$/, () => ({
      health: "PASSED",
      type: "ata",
      attributes: [{ id: "  5", name: "Reallocated_Sector_Ct", value: 100, worst: 100, threshold: 10, raw: "0", fail: "-", flags: "PO--CK" }],
    }));
    const res = await env.call("proxmox_get_disk_smart", { disk: "/dev/sdb" });
    assert.equal(res.health, "PASSED");
    assert.deepEqual(res.attributes[0], { id: "  5", name: "Reallocated_Sector_Ct", value: 100, worst: 100, threshold: 10, raw: "0" });
    assert.equal(env.mock.requestsTo(/smart/)[0].query.disk, "/dev/sdb");
  });

  it("lists and inspects ZFS pools", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/disks\/zfs$/, () => [{ name: "rpool", size: 1000, alloc: 250, free: 750, frag: 5, dedup: 1, health: "ONLINE" }]);
    env.mock.on("GET", /^\/nodes\/pve1\/disks\/zfs\/rpool$/, () => ({
      name: "rpool",
      state: "DEGRADED",
      status: "One or more devices has been removed",
      action: "Online the device",
      scan: "scrub repaired 0B in 00:10:00 with 0 errors",
      errors: "No known data errors",
      children: [{ name: "rpool", state: "DEGRADED", read: 0, write: 0, cksum: 0, children: [{ name: "mirror-0", state: "DEGRADED", children: [{ name: "sda3", state: "ONLINE", read: 0, write: 0, cksum: 0 }, { name: "sdb3", state: "REMOVED", msg: "was /dev/sdb3" }] }] }],
    }));
    const list = await env.call("proxmox_list_zfs_pools");
    assert.deepEqual(list.pools[0], { name: "rpool", health: "ONLINE", usage: "250 B of 1000 B (25%)", free: "750 B", frag_pct: 5, dedup: 1 });
    const pool = await env.call("proxmox_get_zfs_pool", { name: "rpool" });
    assert.equal(pool.state, "DEGRADED");
    assert.match(pool.scan, /scrub/);
    assert.equal(pool.vdevs[0].children[0].children[1].state, "REMOVED");
    assert.equal(pool.vdevs[0].children[0].children[1].msg, "was /dev/sdb3");
  });

  it("lists LVM VGs, thin pools and directory mounts", async () => {
    env.mock.on("GET", /disks\/lvm$/, () => ({ leaf: 0, children: [{ name: "pve", size: 1000, free: 100, leaf: 0, children: [{ name: "/dev/sda3", size: 1000, free: 100, leaf: 1 }] }] }));
    env.mock.on("GET", /disks\/lvmthin$/, () => [{ lv: "data", vg: "pve", lv_size: 800, used: 400, metadata_size: 10, metadata_used: 9 }]);
    env.mock.on("GET", /disks\/directory$/, () => [{ path: "/mnt/pve/backup", device: "/dev/sdc1", type: "ext4", options: "defaults", unitfile: "/etc/systemd/system/mnt-pve-backup.mount" }]);
    const lvm = await env.call("proxmox_list_lvm");
    assert.equal(lvm.volume_groups[0].usage, "900 B of 1000 B (90%)");
    assert.equal(lvm.volume_groups[0].physical_volumes[0].name, "/dev/sda3");
    const thin = await env.call("proxmox_list_lvm_thin");
    assert.deepEqual(thin.thin_pools[0], { name: "data", volume_group: "pve", usage: "400 B of 800 B (50%)", metadata_usage: "9 B of 10 B (90%)" });
    const dirs = await env.call("proxmox_list_directory_mounts");
    assert.equal(dirs.directories[0].filesystem, "ext4");
    assert.equal(dirs.directories[0].unit_file, "/etc/systemd/system/mnt-pve-backup.mount");
  });

  it("creates ZFS, LVM, thin and directory storage on disks", async () => {
    env.mock.on("POST", /^\/nodes\/pve1\/disks\/(\w+)$/, (_p, m) => env.mock.upid("pve1", `${m[1]}create`, "x"));
    const zfs = await env.call("proxmox_create_disk_storage", { kind: "zfs", name: "tank", devices: ["/dev/sdb", "/dev/sdc"], raidlevel: "mirror", ashift: 12, compression: "lz4", add_storage: true });
    assert.equal(zfs.status, "ok");
    let w = env.mock.lastWrite()!;
    assert.equal(w.path, "/nodes/pve1/disks/zfs");
    assert.deepEqual(w.body, { name: "tank", devices: "/dev/sdb,/dev/sdc", raidlevel: "mirror", ashift: "12", compression: "lz4", add_storage: "1" });

    await env.call("proxmox_create_disk_storage", { kind: "zfs", name: "single", devices: ["/dev/sdd"] });
    assert.equal(env.mock.lastWrite()!.body.raidlevel, "single");

    await env.call("proxmox_create_disk_storage", { kind: "directory", name: "backup", devices: ["/dev/sde"], filesystem: "xfs" });
    w = env.mock.lastWrite()!;
    assert.equal(w.path, "/nodes/pve1/disks/directory");
    assert.deepEqual(w.body, { name: "backup", device: "/dev/sde", filesystem: "xfs" });

    await env.call("proxmox_create_disk_storage", { kind: "lvmthin", name: "thin", devices: ["/dev/sdf"], filesystem: "xfs" });
    assert.deepEqual(env.mock.lastWrite()!.body, { name: "thin", device: "/dev/sdf" });

    assert.match(errorText(await env.callRaw("proxmox_create_disk_storage", { kind: "zfs", name: "x", devices: ["/dev/a", "/dev/b"] })), /raidlevel/);
    assert.match(errorText(await env.callRaw("proxmox_create_disk_storage", { kind: "lvm", name: "x", devices: ["/dev/a", "/dev/b"] })), /exactly one device/);
  });

  it("destroys disk storage with cleanup options", async () => {
    env.mock.on("DELETE", /^\/nodes\/pve1\/disks\//, () => env.mock.upid("pve1", "zfsremove", "tank"));
    const res = await env.call("proxmox_delete_disk_storage", { kind: "zfs", name: "tank", cleanup_config: true, cleanup_disks: true });
    assert.equal(res.status, "ok");
    let w = env.mock.lastWrite()!;
    assert.equal(w.path, "/nodes/pve1/disks/zfs/tank");
    assert.deepEqual(w.query, { "cleanup-config": "1", "cleanup-disks": "1" });
    await env.call("proxmox_delete_disk_storage", { kind: "lvmthin", name: "data", volume_group: "pve" });
    w = env.mock.lastWrite()!;
    assert.deepEqual(w.query, { "volume-group": "pve" });
    assert.match(errorText(await env.callRaw("proxmox_delete_disk_storage", { kind: "lvmthin", name: "data" })), /volume_group/);
  });

  it("initializes GPT and wipes disks", async () => {
    env.mock.on("POST", /disks\/initgpt$/, () => env.mock.upid("pve1", "diskinit", "sdb"));
    env.mock.on("PUT", /disks\/wipedisk$/, () => env.mock.upid("pve1", "wipedisk", "sdb"));
    const init = await env.call("proxmox_init_disk_gpt", { disk: "/dev/sdb" });
    assert.equal(init.status, "ok");
    assert.deepEqual(env.mock.lastWrite()!.body, { disk: "/dev/sdb" });
    const wipe = await env.call("proxmox_wipe_disk", { disk: "/dev/sdb", wait: false });
    assert.equal(wipe.status, "started");
    assert.equal(env.mock.lastWrite()!.method, "PUT");
  });

  it("requires a node on multi-node clusters without a default", async () => {
    const other = await startEnv();
    try {
      const err = errorText(await other.callRaw("proxmox_list_disks"));
      assert.match(err, /pass `node`/);
    } finally {
      await other.stop();
    }
  });
});
