import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { connect, errorText, startEnv, toolOptions, type TestEnv } from "./helpers.js";

const CT = "/nodes/pve1/lxc/200";

describe("containers tools", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv();
  });
  after(() => env.stop());
  beforeEach(() => env.mock.reset());

  it("lists only containers", async () => {
    const res = await env.call("proxmox_list_containers");
    assert.equal(res.total, 1);
    assert.equal(res.items[0].vmid, 200);
    assert.equal(res.items[0].type, "container");
    const none = await env.call("proxmox_list_containers", { status: "stopped" });
    assert.equal(none.total, 0);
  });

  it("gets a container with status, config summary, live IPs and pending changes", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/lxc\/200\/status\/current$/, () => ({
      status: "running",
      name: "dns",
      vmid: 200,
      cpus: 1,
      cpu: 0.05,
      mem: 134217728,
      maxmem: 536870912,
      swap: 0,
      maxswap: 536870912,
      disk: 1073741824,
      maxdisk: 8589934592,
      uptime: 7200,
      pid: 1234,
      ha: { managed: 0 },
    }));
    env.mock.on("GET", /\/lxc\/200\/pending$/, () => [
      { key: "memory", value: 512, pending: 1024 },
      { key: "hostname", value: "dns" },
      { key: "mp0", value: "local-lvm:vm-200-disk-1,mp=/data", delete: 1 },
    ]);
    env.mock.on("GET", /\/lxc\/200\/interfaces$/, () => [
      { name: "lo", hwaddr: "00:00:00:00:00:00", inet: "127.0.0.1/8" },
      { name: "eth0", hwaddr: "BC:24:11:00:00:02", inet: "192.168.1.53/24", inet6: "fe80::1/64" },
    ]);
    const res = await env.call("proxmox_get_container", { vmid: 200 });
    assert.equal(res.vmid, 200);
    assert.equal(res.node, "pve1");
    assert.equal(res.status, "running");
    assert.equal(res.cpu_pct, 5);
    assert.match(res.memory, /128 MiB of 512 MiB/);
    assert.equal(res.ha, undefined);
    assert.deepEqual(res.ip_addresses, [{ name: "eth0", hwaddr: "BC:24:11:00:00:02", addresses: ["192.168.1.53/24", "fe80::1/64"] }]);
    assert.equal(res.config.hostname, "dns");
    assert.equal(res.config.unprivileged, true);
    assert.equal(res.config.memory, "512 MiB");
    assert.deepEqual(res.config.features, { nesting: true });
    assert.equal(res.config.volumes.rootfs.volume, "local-lvm:vm-200-disk-0");
    assert.equal(res.config.nets.net0.ip, "dhcp");
    assert.deepEqual(res.config.tags, ["infra"]);
    assert.deepEqual(res.pending_changes, [
      { key: "memory", current: 512, pending: 1024 },
      { key: "mp0", current: "local-lvm:vm-200-disk-1,mp=/data", delete: true },
    ]);
  });

  it("refuses VMs in container tools", async () => {
    const text = errorText(await env.callRaw("proxmox_get_container", { vmid: 100 }));
    assert.match(text, /is a VM, not a container/);
  });

  it("gets raw config with options, and the pending listing", async () => {
    env.mock.on("GET", /\/lxc\/200\/config$/, (p) => ({ hostname: "dns", password: "x", snapshot: p.snapshot, current: p.current }));
    const res = await env.call("proxmox_get_container_config", { vmid: 200, snapshot: "pre", current: true });
    assert.equal(res.config.snapshot, "pre");
    assert.equal(res.config.current, "1");
    assert.equal(res.config.password, "<redacted>");
    env.mock.on("GET", /\/lxc\/200\/pending$/, () => [
      { key: "cores", value: 1, pending: 2 },
      { key: "hostname", value: "dns" },
      { key: "password", pending: "hunter2" },
    ]);
    const pend = await env.call("proxmox_get_container_config", { vmid: 200, pending: true });
    assert.deepEqual(pend.pending_changes, [
      { key: "cores", current: 1, pending: 2 },
      { key: "password", pending: "<redacted>" },
    ]);
    assert.deepEqual(pend.config[1], { key: "hostname", value: "dns" });
  });

  it("reads live interfaces and refuses stopped containers", async () => {
    env.mock.on("GET", /\/lxc\/200\/interfaces$/, () => [
      { name: "eth0", "hardware-address": "aa", "ip-addresses": [{ "ip-address": "10.0.0.5", prefix: 24, "ip-address-type": "inet" }] },
    ]);
    const res = await env.call("proxmox_get_container_interfaces", { vmid: 200 });
    assert.deepEqual(res.interfaces, [{ name: "eth0", hwaddr: "aa", addresses: ["10.0.0.5/24"] }]);

    env.mock.on("GET", /^\/cluster\/resources$/, () => [{ vmid: 201, type: "lxc", node: "pve2", status: "stopped", name: "off" }]);
    assert.match(errorText(await env.callRaw("proxmox_get_container_interfaces", { vmid: 201 })), /stopped; it must be running/);
  });

  it("summarizes RRD stats", async () => {
    env.mock.on("GET", /\/lxc\/200\/rrddata$/, (p) => {
      assert.equal(p.timeframe, "day");
      return [
        { time: 1700000000, cpu: 0.1, mem: 1048576, maxmem: 4194304, netin: 1024, netout: 2048, maxcpu: 2 },
        { time: 1700000060, cpu: 0.3, mem: 3145728, maxmem: 4194304, netin: 3072, netout: 2048, maxcpu: 2 },
        { time: 1700000120 },
      ];
    });
    const res = await env.call("proxmox_get_container_stats", { vmid: 200, timeframe: "day", points: true });
    assert.equal(res.samples, 2);
    assert.deepEqual(res.cpu_pct, { avg: 20, max: 30, latest: 30 });
    assert.deepEqual(res.memory, { avg: "2 MiB", max: "3 MiB", latest: "3 MiB" });
    assert.equal(res.memory_total, "4 MiB");
    assert.equal(res.net_in.max, "3 KiB/s");
    assert.equal(res.cpus, 2);
    assert.equal(res.points.length, 2);
  });

  it("checks features and migration preconditions", async () => {
    env.mock.on("GET", /\/lxc\/200\/feature$/, (p) => ({ hasFeature: p.feature === "snapshot" ? 1 : 0, nodes: ["pve1"] }));
    assert.equal((await env.call("proxmox_check_container_feature", { vmid: 200, feature: "snapshot" })).available, true);
    assert.equal((await env.call("proxmox_check_container_feature", { vmid: 200, feature: "clone" })).available, false);
    env.mock.on("GET", /\/lxc\/200\/migrate$/, (p) => ({ running: 1, "allowed-nodes": [p.target], "local-disks": [] }));
    const mig = await env.call("proxmox_check_container_migration", { vmid: 200, target: "pve2" });
    assert.deepEqual(mig["allowed-nodes"], ["pve2"]);
  });

  it("runs power actions as tasks with the right parameters", async () => {
    env.mock.on("POST", /\/lxc\/200\/status\/shutdown$/, () => env.mock.upid("pve1", "vzshutdown", "200"));
    const res = await env.call("proxmox_container_power", { vmid: 200, action: "shutdown", timeout: 30, force_stop: true });
    assert.equal(res.status, "ok");
    assert.equal(res.action, "shutdown");
    const w = env.mock.lastWrite()!;
    assert.equal(w.path, `${CT}/status/shutdown`);
    assert.deepEqual(w.body, { timeout: "30", forceStop: "1" });

    env.mock.on("POST", /\/lxc\/200\/status\/stop$/, () => env.mock.upid("pve1", "vzstop", "200"));
    const started = await env.call("proxmox_container_power", { vmid: 200, action: "stop", overrule_shutdown: true, wait: false });
    assert.equal(started.status, "started");
    assert.match(started.upid, /^UPID:pve1:/);
    assert.deepEqual(env.mock.lastWrite()!.body, { "overrule-shutdown": "1" });
  });

  it("reports failed tasks with the log", async () => {
    env.mock.on("POST", /\/lxc\/200\/status\/reboot$/, () =>
      env.mock.upid("pve1", "vzreboot", "200", { status: "stopped", exitstatus: "boom", log: ["something broke"] }),
    );
    const text = errorText(await env.callRaw("proxmox_container_power", { vmid: 200, action: "reboot" }));
    assert.match(text, /failed: boom/);
    assert.match(text, /something broke/);
  });

  it("creates a container with nextid, rootfs, mount points, nets and features", async () => {
    env.mock.on("GET", /^\/cluster\/nextid$/, () => "205");
    env.mock.on("POST", /^\/nodes\/pve1\/lxc$/, () => env.mock.upid("pve1", "vzcreate", "205"));
    const res = await env.call("proxmox_create_container", {
      node: "pve1",
      ostemplate: "local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst",
      hostname: "app",
      password: "pw",
      ssh_public_keys: "ssh-ed25519 AAAA test",
      rootfs_storage: "local-lvm",
      rootfs_size_gb: 16,
      mount_points: [
        { path: "/data", storage: "local-lvm", size_gb: 50, backup: true },
        { path: "/media", volume: "/mnt/tank/media", ro: true },
      ],
      nets: [{ ip: "192.168.1.60/24", gw: "192.168.1.1", vlan: 20, firewall: true }, { bridge: "vmbr1", ip6: "auto" }],
      cores: 2,
      memory: 1024,
      unprivileged: true,
      features: { nesting: true, keyctl: false },
      onboot: true,
      start: true,
      tags: ["app", "prod"],
      extra: { cpulimit: 1 },
    });
    assert.equal(res.vmid, 205);
    assert.equal(res.status, "ok");
    assert.equal(res.password, undefined);
    const b = env.mock.lastWrite()!.body;
    assert.equal(b.vmid, "205");
    assert.equal(b.rootfs, "local-lvm:16");
    assert.equal(b.mp0, "local-lvm:50,mp=/data,backup=1");
    assert.equal(b.mp1, "/mnt/tank/media,mp=/media,ro=1");
    assert.equal(b.net0, "name=eth0,bridge=vmbr0,ip=192.168.1.60/24,gw=192.168.1.1,firewall=1,tag=20");
    assert.equal(b.net1, "name=eth1,bridge=vmbr1,ip6=auto");
    assert.equal(b.features, "nesting=1,keyctl=0");
    assert.equal(b["ssh-public-keys"], "ssh-ed25519 AAAA test");
    assert.equal(b.tags, "app;prod");
    assert.equal(b.unprivileged, "1");
    assert.equal(b.start, "1");
    assert.equal(b.cpulimit, "1");
  });

  it("adds a default DHCP interface and validates mount points on create", async () => {
    env.mock.on("POST", /^\/nodes\/pve1\/lxc$/, () => env.mock.upid("pve1", "vzcreate", "300"));
    await env.call("proxmox_create_container", { node: "pve1", vmid: 300, ostemplate: "local:vztmpl/alpine.tar.xz" });
    const b = env.mock.lastWrite()!.body;
    assert.equal(b.net0, "name=eth0,bridge=vmbr0,ip=dhcp");
    assert.equal(b.rootfs, "local:8");

    await env.call("proxmox_create_container", { node: "pve1", vmid: 301, ostemplate: "local:vztmpl/alpine.tar.xz", nets: [] });
    assert.equal(env.mock.lastWrite()!.body.net0, undefined);

    const text = errorText(
      await env.callRaw("proxmox_create_container", { node: "pve1", vmid: 302, ostemplate: "x", mount_points: [{ path: "/d", storage: "local-lvm" }] }),
    );
    assert.match(text, /mp0: give either volume, or storage with size_gb/);
  });

  it("clones a container", async () => {
    env.mock.on("GET", /^\/cluster\/nextid$/, () => "210");
    env.mock.on("POST", /\/lxc\/200\/clone$/, () => env.mock.upid("pve1", "vzclone", "200"));
    const res = await env.call("proxmox_clone_container", { vmid: 200, hostname: "dns2", full: true, storage: "local-lvm", snapname: "base" });
    assert.equal(res.newid, 210);
    const w = env.mock.lastWrite()!;
    assert.equal(w.path, `${CT}/clone`);
    assert.deepEqual(w.body, { newid: "210", hostname: "dns2", full: "1", storage: "local-lvm", snapname: "base" });
    await env.call("proxmox_clone_container", { vmid: 200, newid: 211, storage: "local-lvm" });
    assert.deepEqual(env.mock.lastWrite()!.body, { newid: "211", full: "1", storage: "local-lvm" });
  });

  it("updates config, merging nets, mount points and features", async () => {
    env.mock.on("GET", /\/lxc\/200\/config$/, () => ({
      net0: "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:02,ip=dhcp,type=veth",
      mp0: "local-lvm:vm-200-disk-1,mp=/data,size=10G",
      features: "nesting=1",
    }));
    env.mock.on("GET", /\/lxc\/200\/pending$/, () => [{ key: "swap", value: 512, pending: 1024 }]);
    const res = await env.call("proxmox_update_container_config", {
      vmid: 200,
      memory: 2048,
      swap: 1024,
      nets: { net0: { ip: "10.0.0.5/24", gw: "10.0.0.1" }, net1: { bridge: "vmbr1" } },
      mount_points: { mp0: { backup: false }, mp1: { path: "/logs", storage: "local-lvm", size_gb: 4 } },
      features: { keyctl: true },
      tags: ["infra", "dns"],
      delete: ["description"],
      revert: ["cores"],
      digest: "def456",
    });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, `${CT}/config`);
    assert.equal(w.body.net0, "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:02,ip=10.0.0.5/24,type=veth,gw=10.0.0.1");
    assert.equal(w.body.net1, "name=eth1,bridge=vmbr1,ip=dhcp");
    assert.equal(w.body.mp0, "local-lvm:vm-200-disk-1,mp=/data,size=10G,backup=0");
    assert.equal(w.body.mp1, "local-lvm:4,mp=/logs");
    assert.equal(w.body.features, "nesting=1,keyctl=1");
    assert.equal(w.body.memory, "2048");
    assert.equal(w.body.tags, "infra;dns");
    assert.equal(w.body.delete, "description");
    assert.equal(w.body.revert, "cores");
    assert.equal(w.body.digest, "def456");
    assert.deepEqual(res.pending_changes, [{ key: "swap", current: 512, pending: 1024 }]);
    assert.ok(res.updated.includes("net0"));
  });

  it("rejects an empty update", async () => {
    assert.match(errorText(await env.callRaw("proxmox_update_container_config", { vmid: 200 })), /Nothing to change/);
  });

  it("resizes a disk", async () => {
    env.mock.on("PUT", /\/lxc\/200\/resize$/, () => env.mock.upid("pve1", "resize", "200"));
    const res = await env.call("proxmox_resize_container_disk", { vmid: 200, disk: "rootfs", size: "+4G" });
    assert.equal(res.status, "ok");
    assert.deepEqual(env.mock.lastWrite()!.body, { disk: "rootfs", size: "+4G" });
    assert.equal(errorText(await env.callRaw("proxmox_resize_container_disk", { vmid: 200, disk: "scsi0", size: "4G" })).length > 0, true);
  });

  it("moves a volume", async () => {
    env.mock.on("POST", /\/lxc\/200\/move_volume$/, () => env.mock.upid("pve1", "move_volume", "200"));
    await env.call("proxmox_move_container_volume", { vmid: 200, volume: "mp0", storage: "ceph", delete_source: true });
    assert.deepEqual(env.mock.lastWrite()!.body, { volume: "mp0", storage: "ceph", delete: "1" });
    await env.call("proxmox_move_container_volume", { vmid: 200, volume: "mp0", target_vmid: 201, target_volume: "mp3" });
    assert.deepEqual(env.mock.lastWrite()!.body, { volume: "mp0", "target-vmid": "201", "target-volume": "mp3" });
    assert.match(errorText(await env.callRaw("proxmox_move_container_volume", { vmid: 200, volume: "mp0" })), /Give storage/);
  });

  it("migrates with restart and refuses running containers without it", async () => {
    assert.match(errorText(await env.callRaw("proxmox_migrate_container", { vmid: 200, target: "pve2" })), /restart=true/);
    assert.match(errorText(await env.callRaw("proxmox_migrate_container", { vmid: 200, target: "pve1", restart: true })), /already on pve1/);
    env.mock.on("POST", /\/lxc\/200\/migrate$/, () => env.mock.upid("pve1", "vzmigrate", "200"));
    const res = await env.call("proxmox_migrate_container", { vmid: 200, target: "pve2", restart: true, timeout: 60 });
    assert.equal(res.to, "pve2");
    assert.deepEqual(env.mock.lastWrite()!.body, { target: "pve2", restart: "1", timeout: "60" });
  });

  it("converts only stopped containers to templates", async () => {
    assert.match(errorText(await env.callRaw("proxmox_convert_container_to_template", { vmid: 200 })), /running; shut it down/);
    env.mock.on("GET", /^\/cluster\/resources$/, () => [{ vmid: 201, type: "lxc", node: "pve2", status: "stopped", name: "off" }]);
    const res = await env.call("proxmox_convert_container_to_template", { vmid: 201 });
    assert.equal(res.template, true);
    assert.equal(env.mock.lastWrite()!.path, "/nodes/pve2/lxc/201/template");
  });

  it("deletes a container, requiring force when running", async () => {
    assert.match(errorText(await env.callRaw("proxmox_delete_container", { vmid: 200 })), /force=true/);
    env.mock.on("DELETE", /^\/nodes\/pve1\/lxc\/200$/, () => env.mock.upid("pve1", "vzdestroy", "200"));
    const res = await env.call("proxmox_delete_container", { vmid: 200, force: true, purge: true, destroy_unreferenced_disks: true });
    assert.equal(res.deleted, true);
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "DELETE");
    assert.deepEqual(w.query, { purge: "1", "destroy-unreferenced-disks": "1", force: "1" });
  });
});

describe("container config permission gates", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv();
  });
  after(() => env.stop());

  it("needs delete permission to delete unusedN volumes and exec permission for hookscript", async () => {
    const mcp = await connect(env.client, toolOptions("write"));
    try {
      const raw = (args: Record<string, unknown>) => mcp.callTool({ name: "proxmox_update_container_config", arguments: { vmid: 200, ...args } });
      assert.match(errorText(await raw({ delete: ["unused0"] })), /delete permission/);
      assert.match(errorText(await raw({ extra: { hookscript: "local:snippets/x.sh" } })), /exec permission/);
      const create = await mcp.callTool({ name: "proxmox_create_container", arguments: { node: "pve1", vmid: 330, ostemplate: "x", extra: { hookscript: "y" } } });
      assert.match(errorText(create), /exec permission/);
      // A create with restore+force restores a backup over the existing container; extra can't turn it into one.
      const createCt = (extra: Record<string, unknown>) =>
        mcp.callTool({ name: "proxmox_create_container", arguments: { node: "pve1", vmid: 330, ostemplate: "local:backup/vzdump-lxc-200.tar.zst", extra } });
      assert.match(errorText(await createCt({ restore: 1, force: 1 })), /extra must not set `restore`, `force`/);
      assert.match(errorText(await createCt({ vmid: 200 })), /extra must not set `vmid`/);
      assert.match(errorText(await createCt({ ostemplate: "y", unique: 1 })), /`ostemplate`, `unique`/);
      // Typed fields that weren't given still count, e.g. delete on an update.
      assert.match(errorText(await raw({ extra: { delete: "unused0" } })), /extra must not set `delete`/);
      assert.equal(env.mock.lastWrite(), undefined);
    } finally {
      await mcp.close();
    }
  });
});
