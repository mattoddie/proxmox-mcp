import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { backupGuestType } from "../tools/backup.js";
import { connect, errorText, startEnv, toolOptions, type TestEnv } from "./helpers.js";

const VMA = "local:backup/vzdump-qemu-100-2025_01_02-02_00_00.vma.zst";
const TAR = "local:backup/vzdump-lxc-200-2025_01_01-02_00_00.tar.zst";
const PBS_VM = "pbs:backup/vm/101/2025-01-03T02:00:00Z";

const JOB = {
  id: "backup-daily",
  type: "vzdump",
  schedule: "02:00",
  enabled: 1,
  storage: "local",
  mode: "snapshot",
  vmid: "100,101,200",
  compress: "zstd",
  "prune-backups": { "keep-last": "3", "keep-daily": "7" },
  "notes-template": "{{guestname}}",
  "next-run": 1735783200,
  fleecing: { enabled: 1, storage: "local-lvm" },
};

describe("backup tools", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv();
  });
  after(() => env.stop());
  beforeEach(() => env.mock.reset());

  it("detects guest types from backup volumes", () => {
    assert.equal(backupGuestType(VMA), "qemu");
    assert.equal(backupGuestType(TAR), "lxc");
    assert.equal(backupGuestType(PBS_VM), "qemu");
    assert.equal(backupGuestType("pbs:backup/ct/200/2025-01-03T02:00:00Z"), "lxc");
    assert.equal(backupGuestType({ volid: "x:y", subtype: "lxc" }), "lxc");
    assert.equal(backupGuestType("nfs:backup/something.tar"), undefined);
  });

  // ---------------------------------------------------------------- jobs

  it("lists backup jobs", async () => {
    env.mock.on("GET", /^\/cluster\/backup$/, () => [
      JOB,
      { id: "weekly-all", schedule: "sun 01:00", all: 1, exclude: "9000", storage: "pbs", enabled: 0, "prune-backups": "keep-weekly=4" },
      { id: "pool-job", schedule: "daily", pool: "production", storage: "pbs", mailto: "a@b.c" },
    ]);
    const r = await env.call("proxmox_list_backup_jobs");
    assert.equal(r.total, 3);
    const [daily, pool, weekly] = r.items;
    assert.deepEqual(daily, {
      id: "backup-daily",
      enabled: true,
      schedule: "02:00",
      next_run: "2025-01-02T02:00:00.000Z",
      storage: "local",
      mode: "snapshot",
      selection: { vmids: [100, 101, 200] },
      compress: "zstd",
      retention: { keep_last: 3, keep_daily: 7 },
      notes_template: "{{guestname}}",
    });
    assert.deepEqual(weekly.selection, { all: true, exclude: [9000] });
    assert.equal(weekly.enabled, false);
    assert.deepEqual(weekly.retention, { keep_weekly: 4 });
    assert.deepEqual(pool.selection, { pool: "production" });
    const s = await env.call("proxmox_list_backup_jobs", { search: "pbs" });
    assert.equal(s.total, 2);
  });

  it("gets a job with included volumes", async () => {
    env.mock
      .on("GET", /^\/cluster\/backup\/backup-daily$/, () => {
        const { id: _id, ...rest } = JOB;
        return rest;
      })
      .on("GET", /^\/cluster\/backup\/backup-daily\/included_volumes$/, () => ({
        children: [
          {
            id: 100,
            name: "web",
            type: "qemu",
            children: [
              { id: "scsi0", name: "local-lvm:vm-100-disk-0", included: 1, reason: "default" },
              { id: "scsi1", name: "local-lvm:vm-100-disk-1", included: 0, reason: "disabled" },
            ],
          },
        ],
      }));
    const r = await env.call("proxmox_get_backup_job", { id: "backup-daily" });
    assert.equal(r.id, "backup-daily");
    assert.equal(r.storage, "local");
    assert.deepEqual(r.guests, [
      {
        vmid: 100,
        name: "web",
        type: "vm",
        volumes: [
          { id: "scsi0", name: "local-lvm:vm-100-disk-0", included: true, reason: "default" },
          { id: "scsi1", name: "local-lvm:vm-100-disk-1", included: false, reason: "disabled" },
        ],
      },
    ]);
    const raw = await env.call("proxmox_get_backup_job", { id: "backup-daily", raw: true, include_volumes: false });
    assert.equal(raw.job.compress, "zstd");
    assert.equal(raw.guests, undefined);
  });

  it("creates a backup job", async () => {
    env.mock.on("GET", /^\/cluster\/backup$/, () => []);
    const r = await env.call("proxmox_save_backup_job", {
      id: "nightly",
      schedule: "daily",
      storage: "local",
      vmids: [100, 200],
      mode: "snapshot",
      compress: "zstd",
      retention: { keep_last: 3, keep_weekly: 2, keep_all: false },
      notes_template: "{{guestname}}",
      remove_old: true,
      extra: { "repeat-missed": 1 },
    });
    assert.deepEqual(r, { id: "nightly", status: "created" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.equal(w.path, "/cluster/backup");
    assert.deepEqual(w.body, {
      id: "nightly",
      schedule: "daily",
      storage: "local",
      vmid: "100,200",
      mode: "snapshot",
      compress: "zstd",
      "prune-backups": "keep-all=0,keep-last=3,keep-weekly=2",
      remove: "1",
      "notes-template": "{{guestname}}",
      "repeat-missed": "1",
    });
  });

  it("requires a selection for new jobs", async () => {
    env.mock.on("GET", /^\/cluster\/backup$/, () => []);
    assert.match(errorText(await env.callRaw("proxmox_save_backup_job", { schedule: "daily" })), /needs a selection/);
  });

  it("updates a job and swaps its selection", async () => {
    env.mock.on("GET", /^\/cluster\/backup$/, () => [JOB]);
    const r = await env.call("proxmox_save_backup_job", { id: "backup-daily", all: true, exclude: [9000], enabled: false, delete: ["comment"] });
    assert.deepEqual(r, { id: "backup-daily", status: "updated" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, "/cluster/backup/backup-daily");
    assert.equal(w.body.all, "1");
    assert.equal(w.body.exclude, "9000");
    assert.equal(w.body.enabled, "0");
    assert.deepEqual(String(w.body.delete).split(",").sort(), ["comment", "pool", "vmid"]);

    await env.call("proxmox_save_backup_job", { id: "backup-daily", schedule: "03:00" });
    const w2 = env.mock.lastWrite()!;
    assert.deepEqual(w2.body, { schedule: "03:00" });
  });

  it("deletes a job", async () => {
    await env.call("proxmox_delete_backup_job", { id: "backup-daily" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "DELETE");
    assert.equal(w.path, "/cluster/backup/backup-daily");
  });

  it("runs a VMID-list job per node with the job's settings", async () => {
    env.mock.on("GET", /^\/cluster\/backup\/backup-daily$/, () => ({ ...JOB, vmid: "100,101,200,555" }));
    env.mock.on("POST", /^\/nodes\/([^/]+)\/vzdump$/, (_p, m) => env.mock.upid(m[1], "vzdump", ""));
    const r = await env.call("proxmox_run_backup_job", { id: "backup-daily" });
    assert.equal(r.tasks.length, 2);
    assert.ok(r.tasks.every((t: any) => t.status === "ok"));
    const posts = env.mock.requestsTo(/\/vzdump$/, "POST");
    const byNode = Object.fromEntries(posts.map((p) => [p.path.split("/")[2], p.body]));
    assert.equal(byNode.pve1.vmid, "100,200");
    assert.equal(byNode.pve2.vmid, "101");
    assert.equal(byNode.pve1.storage, "local");
    assert.equal(byNode.pve1["prune-backups"], "keep-last=3,keep-daily=7");
    assert.equal(byNode.pve1.fleecing, "enabled=1,storage=local-lvm");
    for (const k of ["id", "schedule", "enabled", "next-run", "type"]) assert.equal(byNode.pve1[k], undefined, k);
  });

  it("runs an all-guests job on each node with guests", async () => {
    env.mock.on("GET", /^\/cluster\/backup\/weekly$/, () => ({ id: "weekly", all: 1, exclude: "101", storage: "local", schedule: "sun 01:00" }));
    env.mock.on("POST", /^\/nodes\/([^/]+)\/vzdump$/, (_p, m) => env.mock.upid(m[1], "vzdump", ""));
    const r = await env.call("proxmox_run_backup_job", { id: "weekly", wait: false });
    // pve2 only has 101, which is excluded.
    assert.equal(r.tasks.length, 1);
    assert.equal(r.tasks[0].node, "pve1");
    assert.equal(r.tasks[0].status, "started");
    const w = env.mock.lastWrite()!;
    assert.equal(w.body.all, "1");
    assert.equal(w.body.exclude, "101");
  });

  it("lists guests without a backup job", async () => {
    env.mock.on("GET", /^\/cluster\/backup-info\/not-backed-up$/, () => [
      { vmid: 200, type: "lxc", name: "dns" },
      { vmid: 101, type: "qemu" },
    ]);
    const r = await env.call("proxmox_list_unbacked_guests");
    assert.deepEqual(r, {
      total: 2,
      items: [
        { vmid: 101, name: "db", type: "vm", node: "pve2", status: "stopped" },
        { vmid: 200, name: "dns", type: "container", node: "pve1", status: "running" },
      ],
    });
  });

  // ---------------------------------------------------------------- ad-hoc backups

  it("backs up guests grouped by node", async () => {
    env.mock.on("POST", /^\/nodes\/([^/]+)\/vzdump$/, (p, m) =>
      env.mock.upid(m[1], "vzdump", "", m[1] === "pve2" ? { status: "stopped", exitstatus: "job errors", log: ["ERROR: Backup of VM 101 failed"] } : undefined),
    );
    const r = await env.call("proxmox_backup_guests", {
      vmids: [100, 200, 101],
      storage: "local",
      mode: "stop",
      compress: "zstd",
      notes_template: "pre-upgrade",
      protected: true,
      remove_old: false,
    });
    const posts = env.mock.requestsTo(/\/vzdump$/, "POST");
    const pve1 = posts.find((p) => p.path === "/nodes/pve1/vzdump")!.body;
    assert.deepEqual(pve1, { storage: "local", mode: "stop", compress: "zstd", "notes-template": "pre-upgrade", protected: "1", remove: "0", vmid: "100,200" });
    assert.equal(r.status, "partially failed");
    const failed = r.tasks.find((t: any) => t.node === "pve2");
    assert.equal(failed.status, "failed");
    assert.match(failed.error, /job errors/);
  });

  it("rejects unknown VMIDs before starting backups", async () => {
    assert.match(errorText(await env.callRaw("proxmox_backup_guests", { vmids: [100, 555] })), /No VM or container with ID 555/);
    assert.equal(env.mock.requestsTo(/\/vzdump$/, "POST").length, 0);
  });

  // ---------------------------------------------------------------- volumes

  function mockContent() {
    env.mock.on("GET", /^\/nodes\/(pve\d)\/storage\/local\/content$/, (p, m) => {
      assert.equal(p.content, "backup");
      if (m[1] === "pve2") return [];
      return [
        { volid: TAR, vmid: 200, format: "tar.zst", size: 104857600, ctime: 1735696800, subtype: "lxc", content: "backup" },
        { volid: VMA, vmid: 100, format: "vma.zst", size: 2147483648, ctime: 1735783200, notes: "nightly", protected: 1, content: "backup" },
      ];
    });
  }

  it("lists backups across storages, newest first", async () => {
    mockContent();
    const r = await env.call("proxmox_list_backups");
    assert.equal(r.total, 2);
    assert.equal(r.total_size, "2.10 GiB");
    assert.deepEqual(r.items[0], {
      volid: VMA,
      storage: "local",
      node: "pve1",
      vmid: 100,
      type: "vm",
      time: "2025-01-02T02:00:00.000Z",
      size: "2 GiB",
      size_bytes: 2147483648,
      format: "vma.zst",
      notes: "nightly",
      protected: true,
    });
    assert.equal(r.items[1].type, "container");
    // Only storages with backup content were queried (not local-lvm).
    assert.equal(env.mock.requestsTo(/local-lvm/).length, 0);
    assert.equal(env.mock.requestsTo(/\/storage\/local\/content$/).length, 2);

    const cts = await env.call("proxmox_list_backups", { type: "container" });
    assert.deepEqual(cts.items.map((b: any) => b.vmid), [200]);
    const old = await env.call("proxmox_list_backups", { older_than_days: 1 });
    assert.equal(old.total, 2);
    const recent = await env.call("proxmox_list_backups", { newer_than_days: 1 });
    assert.equal(recent.total, 0);
    const one = await env.call("proxmox_list_backups", { vmid: 100, node: "pve1" });
    assert.equal(one.total, 1);
    assert.equal(env.mock.requestsTo("/nodes/pve1/storage/local/content").at(-1)!.query.vmid, "100");
  });

  it("reports storages that fail to list instead of failing", async () => {
    env.mock.on("GET", /^\/nodes\/pve2\/storage\/local\/content$/, () => env.mock.error(500, "storage offline"));
    env.mock.on("GET", /^\/nodes\/pve1\/storage\/local\/content$/, () => []);
    const r = await env.call("proxmox_list_backups");
    assert.equal(r.total, 0);
    assert.equal(r.errors.length, 1);
    assert.equal(r.errors[0].node, "pve2");
  });

  it("extracts the config from a backup with secrets redacted", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/vzdump\/extractconfig$/, (p) => {
      assert.equal(p.volume, VMA);
      return "#my%20web%20server\n#100% done\n#qmdump#map:scsi0:drive-scsi0:local-lvm:raw:\ncores: 4\nmemory: 4096\ncipassword: hunter2\nscsi0: local-lvm:vm-100-disk-0,size=32G\n\n[PENDING]\ncores: 8\n\n[pre-upgrade]\ncores: 2\n\n[special:cloudinit]\nname: web\n";
    });
    const r = await env.call("proxmox_get_backup_config", { volid: VMA, node: "pve1" });
    assert.equal(r.type, "vm");
    assert.equal(r.config.cores, "4");
    assert.equal(r.config.description, "my web server\n100% done");
    assert.equal(r.config.cipassword, "<redacted>");
    assert.deepEqual(r.snapshots, ["pre-upgrade"]);
    const raw = await env.call("proxmox_get_backup_config", { volid: VMA, node: "pve1", raw: true });
    assert.match(raw.config, /cipassword: <redacted>/);
    assert.ok(!raw.config.includes("hunter2"));
  });

  it("asks for a node when a local storage exists on several nodes", async () => {
    assert.match(errorText(await env.callRaw("proxmox_get_backup_config", { volid: VMA })), /local to several nodes \(pve1, pve2\)/);
  });

  it("restores a VM backup to a new VMID", async () => {
    env.mock.on("POST", /^\/nodes\/pve1\/qemu$/, () => env.mock.upid("pve1", "qmrestore", "150"));
    const r = await env.call("proxmox_restore_backup", { volid: VMA, vmid: 150, node: "pve1", storage: "local-lvm", unique: true, start: true });
    assert.equal(r.status, "ok");
    assert.equal(r.type, "vm");
    const w = env.mock.lastWrite()!;
    assert.equal(w.path, "/nodes/pve1/qemu");
    assert.deepEqual(w.body, { vmid: "150", storage: "local-lvm", unique: "1", start: "1", archive: VMA });
  });

  it("restores a container backup", async () => {
    env.mock.on("POST", /^\/nodes\/pve1\/lxc$/, () => env.mock.upid("pve1", "vzrestore", "250"));
    await env.call("proxmox_restore_backup", { volid: TAR, vmid: 250, node: "pve1", unprivileged: true });
    const w = env.mock.lastWrite()!;
    assert.equal(w.path, "/nodes/pve1/lxc");
    assert.deepEqual(w.body, { vmid: "250", ostemplate: TAR, restore: "1", unprivileged: "1" });
  });

  it("guards restores over existing guests", async () => {
    assert.match(errorText(await env.callRaw("proxmox_restore_backup", { volid: VMA, vmid: 100, node: "pve1" })), /in use by VM "web".*force=true/);
    assert.match(errorText(await env.callRaw("proxmox_restore_backup", { volid: VMA, vmid: 100, force: true })), /is running; stop it/);
    assert.match(errorText(await env.callRaw("proxmox_restore_backup", { volid: TAR, vmid: 101, force: true })), /is a VM but the backup is a container/);
    assert.match(errorText(await env.callRaw("proxmox_restore_backup", { volid: TAR, vmid: 250, live_restore: true, node: "pve1" })), /only applies to VM/);
    assert.equal(env.mock.lastWrite(), undefined);

    // Overwriting a stopped VM goes to the node it lives on.
    env.mock.on("POST", /^\/nodes\/pve2\/qemu$/, () => env.mock.upid("pve2", "qmrestore", "101"));
    await env.call("proxmox_restore_backup", { volid: PBS_VM, vmid: 101, force: true });
    const w = env.mock.lastWrite()!;
    assert.equal(w.path, "/nodes/pve2/qemu");
    assert.equal(w.body.force, "1");
  });

  it("refuses to overwrite without delete permission", async () => {
    const mcp = await connect(env.client, toolOptions("write"));
    try {
      const res: any = await mcp.callTool({ name: "proxmox_restore_backup", arguments: { volid: PBS_VM, vmid: 101, force: true } });
      assert.match(errorText(res), /needs delete permission/);
    } finally {
      await mcp.close();
    }
  });

  it("needs exec permission for hook scripts", async () => {
    const mcp = await connect(env.client, toolOptions("write"));
    try {
      const call = (name: string, args: Record<string, unknown>) => mcp.callTool({ name, arguments: args });
      assert.match(errorText(await call("proxmox_backup_guests", { vmids: [100], extra: { script: "/root/hook.sh" } })), /exec permission/);
      assert.match(errorText(await call("proxmox_save_backup_job", { id: "x", all: true, extra: { script: "/root/hook.sh" } })), /exec permission/);
      assert.match(errorText(await call("proxmox_restore_backup", { volid: VMA, vmid: 150, extra: { hookscript: "local:snippets/h.sh" } })), /exec permission/);
      assert.equal(env.mock.lastWrite(), undefined);
    } finally {
      await mcp.close();
    }
  });

  it("updates notes and protection", async () => {
    await env.call("proxmox_update_backup", { volid: VMA, node: "pve1", notes: "keep me", protected: true });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, `/nodes/pve1/storage/local/content/${encodeURIComponent(VMA)}`);
    assert.deepEqual(w.body, { notes: "keep me", protected: "1" });
    assert.match(errorText(await env.callRaw("proxmox_update_backup", { volid: VMA, node: "pve1" })), /Nothing to change/);
  });

  it("deletes a backup", async () => {
    env.mock.on("DELETE", /\/content\//, () => env.mock.upid("pve1", "imgdel", ""));
    const r = await env.call("proxmox_delete_backup", { volid: VMA, node: "pve1" });
    assert.equal(r.status, "ok");
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "DELETE");
    assert.equal(w.path, `/nodes/pve1/storage/local/content/${encodeURIComponent(VMA)}`);
    // Volume IDs without a storage are rejected.
    assert.match(errorText(await env.callRaw("proxmox_delete_backup", { volid: "nope", node: "pve1" })), /Not a volume ID/);
  });

  it("previews and runs pruning", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/storage\/local\/prunebackups$/, () => [
      { volid: TAR, vmid: 200, type: "lxc", ctime: 1735696800, mark: "remove" },
      { volid: VMA, vmid: 100, type: "qemu", ctime: 1735783200, mark: "protected" },
    ]);
    const p = await env.call("proxmox_preview_backup_prune", { storage: "local", node: "pve1", retention: { keep_last: 1 }, type: "container" });
    assert.equal(p.remove, 1);
    assert.equal(p.protected, 1);
    assert.deepEqual(p.would_remove, [TAR]);
    assert.equal(p.items[0].volid, VMA);
    const q = env.mock.requestsTo("/nodes/pve1/storage/local/prunebackups", "GET")[0].query;
    assert.deepEqual(q, { "prune-backups": "keep-last=1", type: "lxc" });

    env.mock.on("DELETE", /prunebackups$/, () => env.mock.upid("pve1", "prunebackups", "local"));
    const r = await env.call("proxmox_prune_backups", { storage: "local", node: "pve1", vmid: 200 });
    assert.equal(r.status, "ok");
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "DELETE");
    assert.deepEqual(w.query, { vmid: "200" });
  });

  it("reads vzdump defaults", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/vzdump\/defaults$/, (p) => ({ storage: p.storage ?? "local", mode: "snapshot", "prune-backups": "keep-last=5", mailto: "root" }));
    const r = await env.call("proxmox_get_vzdump_defaults", { node: "pve1", storage: "pbs" });
    assert.equal(r.node, "pve1");
    assert.equal(r.storage, "pbs");
    assert.deepEqual(r.retention, { keep_last: 5 });
  });

  it("browses files in a PBS backup", async () => {
    env.mock.on("GET", /^\/cluster\/resources$/, (p) =>
      p.type === "storage" ? [{ type: "storage", storage: "pbs", node: "pve1", shared: 1, status: "available", content: "backup" }, { type: "storage", storage: "pbs", node: "pve2", shared: 1, status: "available", content: "backup" }] : [],
    );
    env.mock.on("GET", /^\/nodes\/pve1\/storage\/pbs\/file-restore\/list$/, (p) => {
      assert.equal(p.volume, PBS_VM);
      if (p.filepath === "/") return [{ text: "drive-scsi0.img.fidx", filepath: Buffer.from("/drive-scsi0.img.fidx").toString("base64"), type: "v", leaf: false }];
      assert.equal(Buffer.from(p.filepath, "base64").toString(), "/drive-scsi0.img.fidx/part/1");
      return [{ text: "hosts", filepath: Buffer.from("/drive-scsi0.img.fidx/part/1/etc/hosts").toString("base64"), type: "f", leaf: true, size: 200, mtime: 1735696800 }];
    });
    const root = await env.call("proxmox_list_backup_files", { storage: "pbs", volid: PBS_VM });
    assert.deepEqual(root.entries, [{ name: "drive-scsi0.img.fidx", path: "/drive-scsi0.img.fidx", type: "v", directory: true }]);
    const sub = await env.call("proxmox_list_backup_files", { storage: "pbs", volid: PBS_VM, path: "/drive-scsi0.img.fidx/part/1" });
    assert.deepEqual(sub.entries[0], { name: "hosts", path: "/drive-scsi0.img.fidx/part/1/etc/hosts", type: "f", size: "200 B", mtime: "2025-01-01T02:00:00.000Z" });
  });
});
