import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { summarizeRrd, textToKeys } from "../tools/vms.js";
import { connect, errorText, startEnv, toolOptions, type TestEnv } from "./helpers.js";

let env: TestEnv;

before(async () => {
  env = await startEnv();
});
after(() => env.stop());
beforeEach(() => env.mock.reset());

const VM100 = "/nodes/pve1/qemu/100";

function nextId(id = 150) {
  env.mock.on("GET", /^\/cluster\/nextid$/, () => String(id));
}

/** Answer a POST/PUT/DELETE on `pattern` with a fresh task ID and return the request params via the mock. */
function taskRoute(method: string, pattern: RegExp, type = "qmtask", node = "pve1") {
  env.mock.on(method, pattern, () => env.mock.upid(node, type, "100"));
}

describe("vm read tools", () => {
  it("lists VMs from every online node with full details", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/qemu$/, () => [
      { vmid: 100, name: "web", status: "running", cpu: 0.5, cpus: 4, mem: 1073741824, maxmem: 4294967296, uptime: 120, diskread: 2048, diskwrite: 4096, "running-qemu": "9.2.0", pressurecpusome: 1.5 },
      { vmid: 9000, name: "tpl-debian", status: "stopped", maxmem: 2147483648, template: 1 },
    ]);
    env.mock.on("GET", /^\/nodes\/pve2\/qemu$/, () => [{ vmid: 101, name: "db", status: "stopped", maxmem: 8589934592 }]);
    const res = await env.call("proxmox_list_vms", {});
    assert.equal(res.total, 3);
    assert.deepEqual(res.items.map((v: any) => v.vmid), [100, 101, 9000]);
    const web = res.items[0];
    assert.equal(web.node, "pve1");
    assert.equal(web.cpu_pct, 50);
    assert.equal(web.disk_write, "4 KiB");
    assert.equal(web.running_qemu, "9.2.0");
    assert.deepEqual(web.pressure_pct, { cpu_some: 1.5 });
    assert.equal(res.items[2].template, true);
    assert.equal(env.mock.requestsTo("/nodes/pve1/qemu", "GET")[0].query.full, "1");

    const filtered = await env.call("proxmox_list_vms", { node: "pve2", search: "db" });
    assert.equal(filtered.total, 1);
    assert.equal(filtered.items[0].memory, "8 GiB");
  });

  it("reports nodes that fail instead of failing the whole list", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/qemu$/, () => [{ vmid: 100, name: "web", status: "running" }]);
    env.mock.on("GET", /^\/nodes\/pve2\/qemu$/, () => env.mock.error(500, "node down"));
    const res = await env.call("proxmox_list_vms", { status: "running" });
    assert.equal(res.total, 1);
    assert.equal(res.errors[0].node, "pve2");
  });

  it("gets status, summarized config and pending changes", async () => {
    env.mock.on("GET", /\/qemu\/100\/status\/current$/, () => ({
      status: "running",
      qmpstatus: "running",
      cpu: 0.031,
      cpus: 4,
      mem: 2147483648,
      maxmem: 4294967296,
      uptime: 3600,
      netin: 1000,
      netout: 2000,
      diskread: 1048576,
      diskwrite: 0,
      pid: 1234,
      agent: 1,
      "running-qemu": "9.2.0",
      "running-machine": "pc-q35-9.2+pve1",
      ha: { managed: 1, state: "started", group: "g1" },
    }));
    env.mock.on("GET", /\/qemu\/100\/pending$/, () => [
      { key: "cores", value: 4, pending: 8 },
      { key: "name", value: "web" },
      { key: "cipassword", value: "hunter2", pending: "newpass" },
      { key: "net1", value: "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr1", delete: 1 },
    ]);
    const res = await env.call("proxmox_get_vm", { vmid: 100 });
    assert.equal(res.node, "pve1");
    assert.equal(res.status, "running");
    assert.equal(res.qmpstatus, undefined);
    assert.equal(res.uptime, "1h 0m");
    assert.equal(res.memory, "2 GiB of 4 GiB (50%)");
    assert.equal(res.disk_read, "1 MiB");
    assert.deepEqual(res.ha, { state: "started", group: "g1" });
    assert.equal(res.running_machine, "pc-q35-9.2+pve1");
    const c = res.config;
    assert.deepEqual(c.cpu, { sockets: 1, cores: 4 });
    assert.equal(c.memory.size, "4 GiB");
    assert.deepEqual(c.boot_order, ["scsi0", "net0"]);
    assert.equal(c.disks.scsi0.file, "local-lvm:vm-100-disk-0");
    assert.equal(c.disks.scsi0.size, "32G");
    assert.equal(c.nets.net0.model, "virtio");
    assert.equal(c.nets.net0.tag, "10");
    assert.equal(c.cloud_init.password_set, true);
    assert.deepEqual(c.tags, ["prod", "web"]);
    assert.equal(res.pending_changes.length, 3);
    assert.deepEqual(res.pending_changes[0], { key: "cores", current: 4, pending: 8 });
    assert.equal(res.pending_changes[2].delete, true);
    assert.match(res.note, /reboot/);
    const text = JSON.stringify(res);
    assert.ok(!text.includes("hunter2") && !text.includes("newpass"), "secrets must be redacted");
    assert.equal(env.mock.requestsTo(`${VM100}/config`, "GET")[0].query.current, "1");
  });

  it("returns redacted raw objects and tolerates a missing pending endpoint", async () => {
    env.mock.on("GET", /\/qemu\/100\/status\/current$/, () => ({ status: "stopped", maxmem: 4294967296 }));
    const res = await env.call("proxmox_get_vm", { vmid: 100, raw: true });
    assert.equal(res.config.cipassword, "<redacted>");
    assert.deepEqual(res.pending, []);
    const summary = await env.call("proxmox_get_vm", { vmid: 100 });
    assert.equal(summary.memory, "4 GiB");
    assert.equal(summary.pending_changes, undefined);
  });

  it("refuses containers", async () => {
    const text = errorText(await env.callRaw("proxmox_get_vm", { vmid: 200 }));
    assert.match(text, /container/);
  });

  it("gets the config, current, from a snapshot or with pending values", async () => {
    const res = await env.call("proxmox_get_vm_config", { vmid: 100, snapshot: "before-upgrade", current: true });
    assert.equal(res.config.cipassword, "<redacted>");
    assert.equal(res.config.digest, "abc123");
    assert.deepEqual(Object.keys(res.config).slice(0, 3), ["agent", "boot", "cipassword"]);
    const q = env.mock.requestsTo(`${VM100}/config`, "GET")[0].query;
    assert.equal(q.snapshot, "before-upgrade");
    assert.equal(q.current, "1");

    env.mock.on("GET", /\/qemu\/100\/pending$/, () => [
      { key: "memory", value: "4096", pending: "8192" },
      { key: "cipassword", value: "x" },
      { key: "tags", value: "a", delete: 2 },
    ]);
    const p = await env.call("proxmox_get_vm_config", { vmid: 100, pending: true });
    assert.deepEqual(p.entries[0], { key: "memory", value: "4096", pending: "8192" });
    assert.equal(p.entries[1].value, "<redacted>");
    assert.equal(p.entries[2].delete, "force");
  });

  it("summarizes RRD statistics", async () => {
    const start = 1_700_000_000;
    env.mock.on("GET", /\/qemu\/100\/rrddata$/, () =>
      Array.from({ length: 60 }, (_, i) => ({
        time: start + i * 60,
        cpu: i / 100,
        maxcpu: 4,
        mem: 1073741824 + i * 1048576,
        maxmem: 4294967296,
        netin: 1024,
        netout: i === 59 ? 4096 : 2048,
        diskread: 0,
        diskwrite: 512,
      })).concat([{ time: start + 3600 } as any]),
    );
    const res = await env.call("proxmox_get_vm_stats", { vmid: 100, timeframe: "day", cf: "MAX" });
    const q = env.mock.requestsTo(`${VM100}/rrddata`)[0].query;
    assert.equal(q.timeframe, "day");
    assert.equal(q.cf, "MAX");
    assert.equal(res.samples, 61);
    assert.equal(res.summary.cpu_pct.min, 0);
    assert.equal(res.summary.cpu_pct.max, 59);
    assert.equal(res.summary.memory.min, "1 GiB");
    assert.equal(res.summary.memory.of, "4 GiB");
    assert.equal(res.summary.net_out.max, "4 KiB/s");
    assert.equal(res.summary.net_in.avg, "1 KiB/s");
    assert.ok(res.series.length <= 12 && res.series.length >= 10, `series length ${res.series.length}`);
    assert.equal(res.series[0].time, new Date(start * 1000).toISOString());
    assert.equal(res.from, new Date(start * 1000).toISOString());

    const summaryOnly = await env.call("proxmox_get_vm_stats", { vmid: 100, points: 0 });
    assert.equal(summaryOnly.series, undefined);
    assert.equal(summaryOnly.timeframe, "hour");
    const raw = await env.call("proxmox_get_vm_stats", { vmid: 100, raw: true });
    assert.equal(raw.data.length, 61);
  });

  it("handles empty RRD data", () => {
    assert.deepEqual(summarizeRrd([], 12), { samples: 0 });
  });

  it("shows cloud-init settings and generated documents", async () => {
    env.mock.on("GET", /\/qemu\/100\/cloudinit$/, () => [
      { key: "ciuser", value: "debian" },
      { key: "cipassword", value: "$5$hash", pending: "$5$newhash" },
      { key: "sshkeys", value: encodeURIComponent("ssh-ed25519 AAAAC3Nza me@laptop") },
      { key: "ipconfig0", value: "ip=dhcp", pending: "ip=10.0.0.5/24,gw=10.0.0.1" },
    ]);
    env.mock.on("GET", /\/qemu\/100\/cloudinit\/dump$/, (p) =>
      p.type === "user" ? "#cloud-config\nuser: debian\npassword: $5$hash\nchpasswd:\n  expire: False\n" : p.type === "network" ? "version: 1\n" : env.mock.error(500, "boom"),
    );
    const res = await env.call("proxmox_get_vm_cloudinit", { vmid: 100 });
    assert.deepEqual(res.pending_keys, ["cipassword", "ipconfig0"]);
    assert.deepEqual(res.settings.find((s: any) => s.key === "sshkeys").value, ["ssh-ed25519 me@laptop"]);
    assert.match(res.generated.user, /password: <redacted>/);
    assert.equal(res.generated.network, "version: 1\n");
    assert.match(res.generated.meta, /^Error:/);
    assert.match(res.note, /regenerate/);
    assert.ok(!JSON.stringify(res).includes("$5$"), "password hashes must be redacted");

    const none = await env.call("proxmox_get_vm_cloudinit", { vmid: 100, dump: [] });
    assert.equal(none.generated, undefined);
  });

  it("checks migration preconditions", async () => {
    env.mock.on("GET", /\/qemu\/100\/migrate$/, () => ({
      running: 1,
      allowed_nodes: [],
      not_allowed_nodes: { pve2: { unavailable_storages: ["local-lvm"] } },
      local_disks: [{ volid: "local-lvm:vm-100-disk-0", size: 34359738368, drivename: "scsi0" }, { volid: "local:iso/debian-12.iso", cdrom: 1, size: 1 }],
      local_resources: ["hostpci0"],
      "mapped-resources": [],
      "has-dbus-vmstate": 1,
    }));
    const res = await env.call("proxmox_check_vm_migration", { vmid: 100, target: "pve2" });
    assert.equal(env.mock.requestsTo(`${VM100}/migrate`)[0].query.target, "pve2");
    assert.equal(res.running, true);
    assert.deepEqual(res.not_allowed_nodes.pve2.unavailable_storages, ["local-lvm"]);
    assert.deepEqual(res.local_disks[0], { volid: "local-lvm:vm-100-disk-0", size: "32 GiB", drive: "scsi0" });
    assert.equal(res.local_disks[1].cdrom, true);
    assert.deepEqual(res.local_resources, ["hostpci0"]);
    assert.equal(res.can_migrate_conntrack, true);
  });

  it("checks storage features", async () => {
    env.mock.on("GET", /\/qemu\/9000\/feature$/, (p) => ({ hasFeature: p.feature === "clone" ? 1 : 0, nodes: ["pve1"] }));
    const res = await env.call("proxmox_check_vm_feature", { vmid: 9000, feature: "clone" });
    assert.deepEqual(res, { vmid: 9000, name: "tpl-debian", node: "pve1", feature: "clone", available: true, nodes: ["pve1"] });
    const snap = await env.call("proxmox_check_vm_feature", { vmid: 9000, feature: "snapshot", snapname: "s1" });
    assert.equal(snap.available, false);
    assert.equal(env.mock.requestsTo("/nodes/pve1/qemu/9000/feature").at(-1)!.query.snapname, "s1");
  });
});

describe("vm power", () => {
  it("shuts down with options and waits for the task", async () => {
    taskRoute("POST", /\/qemu\/100\/status\/shutdown$/, "qmshutdown");
    const res = await env.call("proxmox_vm_power", { vmid: 100, action: "shutdown", timeout: 60, force_stop: true });
    const w = env.mock.lastWrite()!;
    assert.equal(w.path, `${VM100}/status/shutdown`);
    assert.deepEqual(w.body, { timeout: "60", forceStop: "1" });
    assert.equal(res.status, "ok");
    assert.equal(res.action, "shutdown");
    assert.equal(res.task.type, "qmshutdown");
  });

  it("hibernates via suspend to disk and can return without waiting", async () => {
    taskRoute("POST", /\/qemu\/100\/status\/suspend$/, "qmsuspend");
    const res = await env.call("proxmox_vm_power", { vmid: 100, action: "hibernate", state_storage: "local-lvm", wait: false });
    assert.deepEqual(env.mock.lastWrite()!.body, { todisk: "1", statestorage: "local-lvm" });
    assert.equal(res.status, "started");
    assert.match(res.upid, /^UPID:pve1:/);
  });

  it("starts stopped VMs on their own node and skips no-op actions", async () => {
    taskRoute("POST", /\/qemu\/101\/status\/start$/, "qmstart", "pve2");
    await env.call("proxmox_vm_power", { vmid: 101, action: "start", machine: "q35" });
    assert.equal(env.mock.lastWrite()!.path, "/nodes/pve2/qemu/101/status/start");
    assert.equal(env.mock.lastWrite()!.body.machine, "q35");

    env.mock.reset();
    const already = await env.call("proxmox_vm_power", { vmid: 100, action: "start" });
    assert.equal(already.status, "already running");
    const stopped = await env.call("proxmox_vm_power", { vmid: 101, action: "stop" });
    assert.equal(stopped.status, "already stopped");
    assert.equal(env.mock.lastWrite(), undefined);
  });

  it("validates options per action and refuses templates", async () => {
    assert.match(errorText(await env.callRaw("proxmox_vm_power", { vmid: 100, action: "reset", force_stop: true })), /force_stop only applies to: shutdown/);
    assert.match(errorText(await env.callRaw("proxmox_vm_power", { vmid: 9000, action: "start" })), /template/);
    assert.equal(env.mock.lastWrite(), undefined);
  });

  it("reports failed tasks with the log tail", async () => {
    env.mock.on("POST", /\/qemu\/100\/status\/reboot$/, () =>
      env.mock.upid("pve1", "qmreboot", "100", { status: "stopped", exitstatus: "VM quit/powerdown failed", log: ["trying to shut down", "TASK ERROR: VM quit/powerdown failed"] }),
    );
    const text = errorText(await env.callRaw("proxmox_vm_power", { vmid: 100, action: "reboot" }));
    assert.match(text, /failed: VM quit\/powerdown failed/);
    assert.match(text, /TASK ERROR/);
  });
});

describe("vm create, clone and config", () => {
  it("creates a VM from typed arguments", async () => {
    nextId(150);
    taskRoute("POST", /^\/nodes\/pve1\/qemu$/, "qmcreate");
    const res = await env.call("proxmox_create_vm", {
      node: "pve1",
      name: "app1",
      memory_mb: 4096,
      cores: 2,
      cpu: "host",
      bios: "ovmf",
      machine: "q35",
      storage: "local-lvm",
      disks: [
        { size_gb: 32, options: { discard: "on", ssd: true, iothread: true } },
        { bus: "virtio", storage: "fast", import_from: "local:import/debian.qcow2" },
        { bus: "scsi", index: 5, spec: "local-lvm:10,backup=0" },
      ],
      iso: "local:iso/debian-12.iso",
      nets: [{ vlan: 10, firewall: true }, { model: "e1000", bridge: "vmbr1", macaddr: "BC:24:11:00:00:09" }],
      tpm_storage: "local-lvm",
      cloudinit_storage: "local-lvm",
      ciuser: "debian",
      sshkeys: ["ssh-ed25519 AAAA me@laptop", "ssh-rsa BBBB+/= other"],
      ipconfig: { "0": "ip=dhcp" },
      agent: true,
      tags: ["prod", "app"],
      onboot: true,
      start: true,
      pool: "production",
      extra: { serial0: "socket" },
    });
    assert.equal(res.vmid, 150);
    assert.equal(res.status, "ok");
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.equal(w.path, "/nodes/pve1/qemu");
    const b = w.body;
    assert.equal(b.vmid, "150");
    assert.equal(b.scsi0, "local-lvm:32,discard=on,ssd=1,iothread=1");
    assert.equal(b.virtio0, "fast:0,import-from=local:import/debian.qcow2");
    assert.equal(b.scsi5, "local-lvm:10,backup=0");
    assert.equal(b.ide2, "local:iso/debian-12.iso,media=cdrom");
    assert.equal(b.ide0, "local-lvm:cloudinit");
    assert.equal(b.efidisk0, "local-lvm:1,efitype=4m");
    assert.equal(b.tpmstate0, "local-lvm:1,version=v2.0");
    assert.equal(b.net0, "virtio,bridge=vmbr0,tag=10,firewall=1");
    assert.equal(b.net1, "e1000=BC:24:11:00:00:09,bridge=vmbr1");
    assert.equal(b.boot, "order=scsi0;scsi5;virtio0;ide2");
    assert.equal(b.sshkeys, encodeURIComponent("ssh-ed25519 AAAA me@laptop\nssh-rsa BBBB+/= other"));
    assert.equal(b.ipconfig0, "ip=dhcp");
    assert.equal(b.agent, "1");
    assert.equal(b.tags, "prod;app");
    assert.equal(b.memory, "4096");
    assert.equal(b.scsihw, "virtio-scsi-single");
    assert.equal(b.ostype, "l26");
    assert.equal(b.start, "1");
    assert.equal(b.pool, "production");
    assert.equal(b.serial0, "socket");
    assert.equal(b.storage, undefined);
  });

  it("creates a minimal VM with a default NIC and cloud-init on ide2", async () => {
    taskRoute("POST", /^\/nodes\/pve2\/qemu$/, "qmcreate", "pve2");
    await env.call("proxmox_create_vm", { node: "pve2", vmid: 300, cloudinit_storage: "local", nets: undefined });
    const b = env.mock.lastWrite()!.body;
    assert.equal(b.vmid, "300");
    assert.equal(b.ide2, "local:cloudinit");
    assert.equal(b.net0, "virtio,bridge=vmbr0");
    assert.equal(b.boot, undefined);
    assert.equal(env.mock.requestsTo("/cluster/nextid").length, 0);
  });

  it("validates create arguments", async () => {
    const run = (args: Record<string, unknown>) => env.callRaw("proxmox_create_vm", { node: "pve1", vmid: 300, ...args });
    assert.match(errorText(await run({ disks: [{ size_gb: 10 }] })), /needs a storage/);
    assert.match(errorText(await run({ disks: [{ storage: "local-lvm" }] })), /needs size_gb/);
    assert.match(errorText(await run({ bios: "ovmf" })), /efi_storage/);
    assert.match(errorText(await run({ iso: "local:iso/x.iso", disks: [{ bus: "ide", index: 2, storage: "s", size_gb: 1 }] })), /ide2 is used twice/);
    assert.match(errorText(await run({ disks: [{ bus: "ide", index: 9, storage: "s", size_gb: 1 }] })), /Invalid disk slot/);
    assert.equal(env.mock.lastWrite(), undefined);
  });

  it("clones a template to the next free id", async () => {
    nextId(160);
    taskRoute("POST", /\/qemu\/9000\/clone$/, "qmclone");
    const res = await env.call("proxmox_clone_vm", { vmid: 9000, name: "web2", full: true, storage: "local-lvm", format: "raw", pool: "production" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.path, "/nodes/pve1/qemu/9000/clone");
    assert.deepEqual(w.body, { newid: "160", name: "web2", full: "1", storage: "local-lvm", format: "raw", pool: "production" });
    assert.equal(res.newid, 160);
    assert.equal(res.source, 9000);
    assert.match(errorText(await env.callRaw("proxmox_clone_vm", { vmid: 9000, newid: 161, full: false, storage: "x" })), /full clones/);
    // A target storage on a template implies a full clone (Proxmox rejects storage for linked clones).
    await env.call("proxmox_clone_vm", { vmid: 9000, newid: 162, storage: "local-lvm" });
    assert.deepEqual(env.mock.lastWrite()!.body, { newid: "162", full: "1", storage: "local-lvm" });
  });

  it("updates config asynchronously and reports pending changes", async () => {
    taskRoute("POST", /\/qemu\/100\/config$/, "qmconfig");
    env.mock.on("GET", /\/qemu\/100\/pending$/, () => [{ key: "cpu", value: "kvm64", pending: "host" }, { key: "name", value: "web" }]);
    const res = await env.call("proxmox_update_vm_config", {
      vmid: 100,
      cpu: "host",
      memory_mb: 8192,
      boot_order: ["scsi0", "net0"],
      sshkeys: "ssh-ed25519 AAAA a b",
      delete: ["ide2", "description"],
      revert: ["cores"],
      digest: "abc123",
      extra: { scsi1: "local-lvm:20,ssd=1" },
    });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.equal(w.path, `${VM100}/config`);
    assert.deepEqual(w.body, {
      memory: "8192",
      cpu: "host",
      boot: "order=scsi0;net0",
      sshkeys: "ssh-ed25519%20AAAA%20a%20b",
      delete: "ide2,description",
      revert: "cores",
      digest: "abc123",
      scsi1: "local-lvm:20,ssd=1",
    });
    assert.equal(res.status, "ok");
    assert.deepEqual(res.pending_changes, [{ key: "cpu", current: "kvm64", pending: "host" }]);
    assert.match(res.note, /reboot/);
  });

  it("handles a synchronous config result and rejects unsafe or empty updates", async () => {
    const res = await env.call("proxmox_update_vm_config", { vmid: 100, onboot: false, protection: true });
    assert.deepEqual(env.mock.lastWrite()!.body, { onboot: "0", protection: "1" });
    assert.deepEqual(res.updated, ["onboot", "protection"]);
    assert.equal(res.pending_changes, undefined);

    env.mock.reset();
    assert.match(errorText(await env.callRaw("proxmox_update_vm_config", { vmid: 100, delete: ["scsi0"], extra: { force: 1 } })), /proxmox_unlink_vm_disk/);
    assert.match(errorText(await env.callRaw("proxmox_update_vm_config", { vmid: 100, digest: "x" })), /Nothing to change/);
    assert.equal(env.mock.lastWrite(), undefined);
  });
});

describe("vm disks", () => {
  it("resizes a disk", async () => {
    taskRoute("PUT", /\/qemu\/100\/resize$/, "resize");
    const res = await env.call("proxmox_resize_vm_disk", { vmid: 100, disk: "scsi0", size: "+10G" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.deepEqual(w.body, { disk: "scsi0", size: "+10G" });
    assert.equal(res.status, "ok");
    assert.ok((await env.callRaw("proxmox_resize_vm_disk", { vmid: 100, disk: "scsi0", size: "-5G" })).isError);
    assert.ok((await env.callRaw("proxmox_resize_vm_disk", { vmid: 100, disk: "net0", size: "5G" })).isError);
  });

  it("moves a disk to storage or another VM", async () => {
    taskRoute("POST", /\/qemu\/100\/move_disk$/, "qmmove");
    await env.call("proxmox_move_vm_disk", { vmid: 100, disk: "scsi0", storage: "ceph", format: "raw", delete_source: true });
    assert.deepEqual(env.mock.lastWrite()!.body, { disk: "scsi0", storage: "ceph", format: "raw", delete: "1" });
    await env.call("proxmox_move_vm_disk", { vmid: 100, disk: "scsi0", target_vmid: 9000, target_disk: "scsi3" });
    assert.deepEqual(env.mock.lastWrite()!.body, { disk: "scsi0", "target-vmid": "9000", "target-disk": "scsi3" });
    assert.match(errorText(await env.callRaw("proxmox_move_vm_disk", { vmid: 100, disk: "scsi0" })), /target storage or a target_vmid/);
    assert.match(errorText(await env.callRaw("proxmox_move_vm_disk", { vmid: 100, disk: "scsi0", storage: "a", target_vmid: 101 })), /not both/);
  });

  it("detaches or destroys disks", async () => {
    const res = await env.call("proxmox_unlink_vm_disk", { vmid: 100, disks: ["scsi1", "unused0"] });
    assert.deepEqual(env.mock.lastWrite()!.body, { idlist: "scsi1,unused0" });
    assert.match(res.result, /detached/);
    const destroyed = await env.call("proxmox_unlink_vm_disk", { vmid: 100, disks: ["unused0"], destroy: true });
    assert.equal(env.mock.lastWrite()!.method, "PUT");
    assert.equal(env.mock.lastWrite()!.path, `${VM100}/unlink`);
    assert.deepEqual(env.mock.lastWrite()!.body, { idlist: "unused0", force: "1" });
    assert.equal(destroyed.result, "destroyed");
  });
});

describe("vm lifecycle", () => {
  it("live-migrates running VMs by default", async () => {
    taskRoute("POST", /\/qemu\/100\/migrate$/, "qmigrate");
    const res = await env.call("proxmox_migrate_vm", { vmid: 100, target: "pve2", with_local_disks: true, targetstorage: "local" });
    assert.deepEqual(env.mock.lastWrite()!.body, { target: "pve2", online: "1", "with-local-disks": "1", targetstorage: "local" });
    assert.equal(res.online, true);
    assert.equal(res.from, "pve1");

    taskRoute("POST", /\/qemu\/101\/migrate$/, "qmigrate", "pve2");
    await env.call("proxmox_migrate_vm", { vmid: 101, target: "pve1" });
    assert.deepEqual(env.mock.lastWrite()!.body, { target: "pve1" });
    assert.match(errorText(await env.callRaw("proxmox_migrate_vm", { vmid: 100, target: "pve1" })), /already on pve1/);
  });

  it("converts stopped VMs to templates", async () => {
    taskRoute("POST", /\/qemu\/101\/template$/, "qmtemplate", "pve2");
    const res = await env.call("proxmox_convert_vm_to_template", { vmid: 101 });
    assert.equal(env.mock.lastWrite()!.path, "/nodes/pve2/qemu/101/template");
    assert.equal(res.status, "ok");
    assert.match(errorText(await env.callRaw("proxmox_convert_vm_to_template", { vmid: 100 })), /running/);
    assert.equal((await env.call("proxmox_convert_vm_to_template", { vmid: 9000 })).status, "already a template");
  });

  it("deletes stopped VMs and refuses running ones unless asked to stop", async () => {
    taskRoute("DELETE", /^\/nodes\/pve2\/qemu\/101$/, "qmdestroy", "pve2");
    const res = await env.call("proxmox_delete_vm", { vmid: 101, purge: true, destroy_unreferenced_disks: true });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "DELETE");
    assert.equal(w.path, "/nodes/pve2/qemu/101");
    assert.deepEqual(w.query, { purge: "1", "destroy-unreferenced-disks": "1" });
    assert.equal(res.deleted, true);

    env.mock.reset();
    assert.match(errorText(await env.callRaw("proxmox_delete_vm", { vmid: 100 })), /is running; stop it first/);
    assert.equal(env.mock.lastWrite(), undefined);

    taskRoute("POST", /\/qemu\/100\/status\/stop$/, "qmstop");
    taskRoute("DELETE", /^\/nodes\/pve1\/qemu\/100$/, "qmdestroy");
    await env.call("proxmox_delete_vm", { vmid: 100, stop: true });
    const writes = env.mock.requests.filter((r) => r.method !== "GET").map((r) => `${r.method} ${r.path}`);
    assert.deepEqual(writes, [`POST ${VM100}/status/stop`, `DELETE ${VM100}`]);
  });
});

describe("vm cloud-init, keys and monitor", () => {
  it("regenerates the cloud-init drive", async () => {
    const res = await env.call("proxmox_regenerate_vm_cloudinit", { vmid: 100 });
    assert.equal(env.mock.lastWrite()!.method, "PUT");
    assert.equal(env.mock.lastWrite()!.path, `${VM100}/cloudinit`);
    assert.equal(res.regenerated, true);
  });

  it("converts text to QEMU key names", () => {
    assert.deepEqual(textToKeys("Hi 2!\n"), ["shift-h", "i", "spc", "2", "shift-1", "ret"]);
    assert.deepEqual(textToKeys("a_b:/"), ["a", "shift-minus", "b", "shift-semicolon", "slash"]);
    assert.throws(() => textToKeys("é"), /Cannot type/);
  });

  it("sends keys and text to a running VM", async () => {
    const res = await env.call("proxmox_send_vm_key", { vmid: 100, keys: ["ctrl-alt-delete"], text: "ls\n" });
    const sent = env.mock.requestsTo(`${VM100}/sendkey`, "PUT").map((r) => r.body.key);
    assert.deepEqual(sent, ["ctrl-alt-delete", "l", "s", "ret"]);
    assert.equal(res.sent, 4);
    assert.match(errorText(await env.callRaw("proxmox_send_vm_key", { vmid: 101, keys: ["ret"] })), /stopped/);
    assert.match(errorText(await env.callRaw("proxmox_send_vm_key", { vmid: 100 })), /keys and\/or text/);
  });

  it("runs monitor commands", async () => {
    env.mock.on("POST", /\/qemu\/100\/monitor$/, (p) => `VM status: running (${p.command})\n`);
    const res = await env.call("proxmox_vm_monitor_command", { vmid: 100, command: "info status" });
    assert.equal(res.output, "VM status: running (info status)");
    assert.deepEqual(env.mock.lastWrite()!.body, { command: "info status" });
  });
});

describe("vm config permission gates", () => {
  it("needs delete permission to delete unusedN volumes and exec permission for hookscript", async () => {
    const mcp = await connect(env.client, toolOptions("write"));
    try {
      const raw = (args: Record<string, unknown>) => mcp.callTool({ name: "proxmox_update_vm_config", arguments: { vmid: 100, ...args } });
      assert.match(errorText(await raw({ delete: ["unused0"] })), /delete permission/);
      assert.match(errorText(await raw({ extra: { delete: "net1,unused1" } })), /delete permission/);
      assert.match(errorText(await raw({ extra: { hookscript: "local:snippets/x.sh" } })), /exec permission/);
      const create = await mcp.callTool({ name: "proxmox_create_vm", arguments: { node: "pve1", vmid: 170, extra: { hookscript: "local:snippets/x.sh" } } });
      assert.match(errorText(create), /exec permission/);
      assert.equal(env.mock.lastWrite(), undefined);
    } finally {
      await mcp.close();
    }
    // With every permission (the default env) the same delete goes through.
    taskRoute("POST", /\/qemu\/100\/config$/, "qmconfig");
    env.mock.on("GET", /\/qemu\/100\/pending$/, () => []);
    await env.call("proxmox_update_vm_config", { vmid: 100, delete: ["unused0"] });
    assert.equal(env.mock.lastWrite()!.body.delete, "unused0");
  });
});
