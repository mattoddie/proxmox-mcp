import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { summarizeRrd } from "../tools/nodes.js";
import { connect, errorText, startEnv, toolOptions, type TestEnv } from "./helpers.js";

let env: TestEnv;
before(async () => {
  env = await startEnv();
});
after(() => env.stop());
beforeEach(() => env.mock.reset());

const N = "/nodes/pve1";

describe("node status and statistics", () => {
  it("summarizes node status with version and subscription", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/status$/, () => ({
      cpu: 0.25,
      wait: 0.01,
      loadavg: ["0.50", "0.40", "0.30"],
      uptime: 90061,
      cpuinfo: { model: "AMD EPYC", sockets: 1, cores: 8, cpus: 16, mhz: "3000.000", flags: "fpu vme ..." },
      memory: { total: 68719476736, used: 17179869184, free: 1, available: 51539607552 },
      swap: { total: 8589934592, used: 0, free: 8589934592 },
      rootfs: { total: 107374182400, used: 10737418240, avail: 1 },
      ksm: { shared: 1048576 },
      "current-kernel": { sysname: "Linux", release: "6.14.8-2-pve", machine: "x86_64", version: "#1 SMP" },
      pveversion: "pve-manager/9.0.10/deadbeef",
      "boot-info": { mode: "efi", secureboot: 1 },
    }));
    env.mock.on("GET", /^\/nodes\/pve1\/version$/, () => ({ version: "9.0.10", release: "9.0", repoid: "deadbeef" }));
    env.mock.on("GET", /^\/nodes\/pve1\/subscription$/, () => ({ status: "active", level: "c", key: "pve1c-secretkey", nextduedate: "2027-01-01", serverid: "ABC" }));
    const s = await env.call("proxmox_get_node_status", { node: "pve1" });
    assert.equal(s.cpu_pct, 25);
    assert.deepEqual(s.cpu, { model: "AMD EPYC", sockets: 1, cores: 8, threads: 16, mhz: 3000 });
    assert.equal(s.memory, "16 GiB of 64 GiB (25%)");
    assert.equal(s.root_fs, "10 GiB of 100 GiB (10%)");
    assert.equal(s.kernel, "Linux 6.14.8-2-pve (x86_64)");
    assert.equal(s.uptime, "1d 1h 1m");
    assert.equal(s.ksm_shared, "1 MiB");
    assert.deepEqual(s.boot, { mode: "efi", secure_boot: 1 });
    assert.equal(s.subscription.status, "active");
    assert.equal(s.subscription.key_set, true);
    assert.ok(!JSON.stringify(s).includes("secretkey"));
  });

  it("requires a node on multi-node clusters", async () => {
    assert.match(errorText(await env.callRaw("proxmox_get_node_status")), /pass `node`/);
  });

  it("summarizes RRD data", async () => {
    const rows = Array.from({ length: 60 }, (_, i) => ({ time: 1700000000 + i * 60, cpu: i < 30 ? 0.1 : 0.3, memused: 1073741824, netin: 1024, loadavg: 1 }));
    env.mock.on("GET", /^\/nodes\/pve1\/rrddata$/, () => rows);
    const s = await env.call("proxmox_get_node_stats", { node: "pve1", timeframe: "day", points: 4 });
    assert.equal(env.mock.requestsTo(`${N}/rrddata`)[0].query.timeframe, "day");
    assert.equal(s.samples, 60);
    assert.deepEqual(s.metrics.cpu_pct, { min: 10, avg: 20, max: 30, last: 30 });
    assert.equal(s.metrics.memused.max, "1 GiB");
    assert.equal(s.metrics.netin.avg, "1 KiB/s");
    assert.equal(s.series.length, 4);
    assert.equal(s.series[0].cpu_pct, 10);
    assert.equal(s.series[3].cpu_pct, 30);
  });

  it("summarizeRrd restricts metrics and handles empty data", () => {
    const out = summarizeRrd([{ time: 1, cpu: 0.5, memused: 10 }], { metrics: ["memused"], points: 5 });
    assert.deepEqual(Object.keys(out.metrics), ["memused"]);
    assert.deepEqual(summarizeRrd([], { points: 5 }), { samples: 0 });
  });

  it("reads netstat and the report", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/netstat$/, () => [
      { dev: "tap100i0", vmid: "100", in: "2048", out: "1024" },
      { dev: "veth200i0", vmid: "200", in: "10", out: "20" },
    ]);
    assert.deepEqual(await env.call("proxmox_get_node_netstat", { node: "pve1", vmid: 100 }), [{ vmid: 100, dev: "tap100i0", in: "2 KiB", out: "1 KiB" }]);

    env.mock.on("GET", /^\/nodes\/pve1\/report$/, () => "x".repeat(100));
    const r = await env.call("proxmox_get_node_report", { node: "pve1", max_chars: 10 });
    assert.equal(r.text.length, 10);
    assert.equal(r.truncated, true);
    assert.equal(r.total_chars, 100);

    env.mock.on("GET", /^\/nodes\/pve1\/query-url-metadata$/, () => ({ filename: "debian.iso", mimetype: "application/octet-stream", size: 1073741824 }));
    const m = await env.call("proxmox_query_url_metadata", { node: "pve1", url: "https://x/debian.iso", verify_certificates: false });
    assert.equal(m.size, "1 GiB");
    assert.equal(m.filename, "debian.iso");
    assert.deepEqual(env.mock.requestsTo(`${N}/query-url-metadata`)[0].query, { url: "https://x/debian.iso", "verify-certificates": "0" });
  });
});

describe("node services", () => {
  it("lists services with failed ones first", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/services$/, () => [
      { service: "chrony", name: "chrony", desc: "time", "active-state": "active", state: "running", "unit-state": "enabled" },
      { service: "pveproxy", name: "pveproxy", desc: "API", "active-state": "failed", state: "failed", "unit-state": "enabled" },
    ]);
    const s = await env.call("proxmox_list_node_services", { node: "pve1" });
    assert.deepEqual(s.failed, ["pveproxy"]);
    assert.equal(s.items[0].service, "pveproxy");
    assert.equal(s.items[1].active, "active");
  });

  it("restarts a service and waits for the task", async () => {
    const upid = env.mock.upid("pve1", "srvrestart", "pveproxy");
    env.mock.on("POST", /^\/nodes\/pve1\/services\/pveproxy\/restart$/, () => upid);
    env.mock.on("GET", /^\/nodes\/pve1\/services\/pveproxy\/state$/, () => ({ "active-state": "active", state: "running", "unit-state": "enabled" }));
    const r = await env.call("proxmox_manage_node_service", { node: "pve1", service: "pveproxy", action: "restart" });
    assert.equal(r.status, "ok");
    assert.equal(r.state.active, "active");
    assert.equal(env.mock.lastWrite()!.path, `${N}/services/pveproxy/restart`);
  });

  it("returns the UPID when not waiting", async () => {
    const upid = env.mock.upid("pve1", "srvstop", "sshd");
    env.mock.on("POST", /^\/nodes\/pve1\/services\/sshd\/stop$/, () => upid);
    const r = await env.call("proxmox_manage_node_service", { node: "pve1", service: "sshd", action: "stop", wait: false });
    assert.equal(r.upid, upid);
    assert.equal(r.status, "started");
  });
});

describe("node network", () => {
  it("lists interfaces and flags inactive ones", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/network$/, (p) => {
      assert.equal(p.type, "any_bridge");
      return [
        { iface: "vmbr1", type: "bridge", autostart: 1, bridge_ports: "eno2", cidr: "10.1.0.1/24", comments: "lab\n" },
        { iface: "vmbr0", type: "bridge", active: 1, autostart: 1, bridge_ports: "eno1", address: "10.0.0.11", netmask: "24", gateway: "10.0.0.1", bridge_vlan_aware: 1 },
      ];
    });
    const r = await env.call("proxmox_list_node_network", { node: "pve1", type: "any_bridge" });
    assert.deepEqual(r.inactive_or_pending, ["vmbr1"]);
    assert.equal(r.items[0].iface, "vmbr0");
    assert.equal(r.items[0].cidr, "10.0.0.11/24");
    assert.equal(r.items[0].bridge_vlan_aware, true);
    assert.equal(r.items[1].comments, "lab");
    assert.equal(r.items[1].active, false);
    assert.match(r.note, /pending/);
  });

  it("gets one interface", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/network\/bond0$/, () => ({ type: "bond", slaves: "eno1 eno2", bond_mode: "802.3ad", active: 1 }));
    const r = await env.call("proxmox_get_node_network_interface", { node: "pve1", iface: "bond0" });
    assert.deepEqual(r, { iface: "bond0", type: "bond", active: true, slaves: "eno1 eno2", bond_mode: "802.3ad" });
  });

  it("creates an interface with mapped parameter names", async () => {
    const r = await env.call("proxmox_save_node_network_interface", {
      node: "pve1",
      iface: "vmbr2",
      create: true,
      type: "bridge",
      cidr: "10.2.0.1/24",
      bridge_ports: ["eno3", "eno4"],
      bridge_vlan_aware: true,
      mtu: 9000,
      autostart: true,
      bridge_vids: "2-100",
    });
    assert.equal(r.created, true);
    assert.equal(r.pending, true);
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.equal(w.path, `${N}/network`);
    assert.deepEqual(w.body, {
      iface: "vmbr2",
      type: "bridge",
      cidr: "10.2.0.1/24",
      bridge_ports: "eno3 eno4",
      bridge_vlan_aware: "1",
      mtu: "9000",
      autostart: "1",
      bridge_vids: "2-100",
    });
  });

  it("updates an interface, keeping its type and sending deletes", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/network\/vmbr0\.20$/, () => ({ type: "vlan" }));
    await env.call("proxmox_save_node_network_interface", { node: "pve1", iface: "vmbr0.20", vlan_raw_device: "vmbr0", vlan_id: 20, delete: ["gateway", "mtu"] });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, `${N}/network/vmbr0.20`);
    assert.deepEqual(w.body, { type: "vlan", "vlan-raw-device": "vmbr0", "vlan-id": "20", delete: "gateway,mtu" });
  });

  it("requires a type when creating", async () => {
    assert.match(errorText(await env.callRaw("proxmox_save_node_network_interface", { node: "pve1", iface: "x", create: true })), /type is required/);
  });

  it("deletes, applies and reverts", async () => {
    await env.call("proxmox_delete_node_network_interface", { node: "pve1", iface: "vmbr2" });
    assert.deepEqual([env.mock.lastWrite()!.method, env.mock.lastWrite()!.path], ["DELETE", `${N}/network/vmbr2`]);

    const upid = env.mock.upid("pve1", "srvreload", "networking");
    env.mock.on("PUT", /^\/nodes\/pve1\/network$/, () => upid);
    const r = await env.call("proxmox_apply_node_network", { node: "pve1" });
    assert.equal(r.status, "ok");
    assert.equal(env.mock.lastWrite()!.method, "PUT");

    await env.call("proxmox_revert_node_network", { node: "pve1" });
    assert.deepEqual([env.mock.lastWrite()!.method, env.mock.lastWrite()!.path], ["DELETE", `${N}/network`]);
  });

  it("reports a failed apply with the task log", async () => {
    const upid = env.mock.upid("pve1", "srvreload", "networking", { status: "stopped", exitstatus: "command failed", log: ["error: vmbr9: bad"] });
    env.mock.on("PUT", /^\/nodes\/pve1\/network$/, () => upid);
    assert.match(errorText(await env.callRaw("proxmox_apply_node_network", { node: "pve1" })), /vmbr9: bad/);
  });
});

describe("DNS, hosts and time", () => {
  it("gets and merges DNS settings", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/dns$/, () => ({ search: "lan", dns1: "1.1.1.1", dns2: "8.8.8.8" }));
    assert.equal((await env.call("proxmox_get_node_dns", { node: "pve1" })).dns1, "1.1.1.1");
    await env.call("proxmox_set_node_dns", { node: "pve1", dns1: "9.9.9.9" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.deepEqual(w.body, { search: "lan", dns1: "9.9.9.9", dns2: "8.8.8.8" });
  });

  it("reads and writes /etc/hosts", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/hosts$/, () => ({ data: "127.0.0.1 localhost\n", digest: "d1" }));
    const h = await env.call("proxmox_get_node_hosts", { node: "pve1" });
    assert.deepEqual(h, { node: "pve1", digest: "d1", content: "127.0.0.1 localhost\n" });
    await env.call("proxmox_set_node_hosts", { node: "pve1", content: "x", digest: "d1" });
    assert.deepEqual(env.mock.lastWrite()!.body, { data: "x", digest: "d1" });
    assert.equal(env.mock.lastWrite()!.method, "POST");
  });

  it("reads time and sets the time zone", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/time$/, () => ({ time: 1700000000, localtime: 1700003600, timezone: "Europe/Paris" }));
    const t = await env.call("proxmox_get_node_time", { node: "pve1" });
    assert.equal(t.timezone, "Europe/Paris");
    assert.equal(t.utc_offset_hours, 1);
    assert.equal(t.utc, "2023-11-14T22:13:20.000Z");
    await env.call("proxmox_set_node_timezone", { node: "pve1", timezone: "UTC" });
    assert.deepEqual(env.mock.lastWrite()!.body, { timezone: "UTC" });
  });
});

describe("node config and power", () => {
  it("parses node config", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/config$/, () => ({
      description: "rack 1",
      wakeonlan: "aa:bb:cc:dd:ee:ff",
      acme: "account=default",
      acmedomain0: "pve1.example.com,plugin=cf",
      digest: "abc",
    }));
    const c = await env.call("proxmox_get_node_config", { node: "pve1" });
    assert.deepEqual(c.acmedomain0, { domain: "pve1.example.com", plugin: "cf" });
    assert.deepEqual(c.wakeonlan, { mac: "aa:bb:cc:dd:ee:ff" });
    assert.deepEqual(c.acme, { account: "default" });
  });

  it("updates node config, replacing ACME domains", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/config$/, () => ({ acmedomain0: "old.example.com", acmedomain1: "old2.example.com", digest: "abc" }));
    await env.call("proxmox_update_node_config", {
      node: "pve1",
      description: "notes",
      startall_onboot_delay: 30,
      acme_account: "le",
      acme_domains: [{ domain: "pve1.example.com", plugin: "cf" }],
      digest: "abc",
    });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, `${N}/config`);
    assert.deepEqual(w.body, {
      description: "notes",
      "startall-onboot-delay": "30",
      acme: "account=le",
      digest: "abc",
      acmedomain0: "pve1.example.com,plugin=cf",
      delete: "acmedomain1",
    });
  });

  it("reboots, wakes and runs bulk actions", async () => {
    await env.call("proxmox_node_power", { node: "pve2", command: "reboot" });
    assert.deepEqual([env.mock.lastWrite()!.path, env.mock.lastWrite()!.body], ["/nodes/pve2/status", { command: "reboot" }]);

    env.mock.on("POST", /^\/nodes\/pve2\/wakeonlan$/, () => "aa:bb:cc:dd:ee:ff");
    assert.equal((await env.call("proxmox_wake_node", { node: "pve2" })).mac, "aa:bb:cc:dd:ee:ff");

    const upid = env.mock.upid("pve1", "migrateall", "");
    env.mock.on("POST", /^\/nodes\/pve1\/migrateall$/, () => upid);
    const r = await env.call("proxmox_node_bulk_action", { node: "pve1", action: "migrate", target: "pve2", vmids: [100, 200], max_workers: 2, with_local_disks: true, force: true });
    assert.equal(r.status, "ok");
    assert.deepEqual(env.mock.lastWrite()!.body, { vms: "100,200", "max-workers": "2", target: "pve2", "with-local-disks": "1" });

    await env.call("proxmox_node_bulk_action", { node: "pve1", action: "stop", force_stop: false, timeout: 60, wait: false });
    assert.deepEqual([env.mock.lastWrite()!.path, env.mock.lastWrite()!.body], [`${N}/stopall`, { "force-stop": "0", timeout: "60" }]);

    assert.match(errorText(await env.callRaw("proxmox_node_bulk_action", { node: "pve1", action: "migrate" })), /target is required/);
  });
});

describe("node logs", () => {
  it("reads syslog with filters", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/syslog$/, () => [
      { n: 1, t: "Jan 1 pveproxy[1]: started" },
      { n: 2, t: "Jan 1 pveproxy[1]: error x" },
    ]);
    const r = await env.call("proxmox_get_node_syslog", { node: "pve1", service: "pveproxy", since: "2026-01-01 00:00:00", limit: 10, search: "error" });
    assert.deepEqual(r.lines, ["Jan 1 pveproxy[1]: error x"]);
    assert.deepEqual(env.mock.requestsTo(`${N}/syslog`)[0].query, { service: "pveproxy", since: "2026-01-01 00:00:00", limit: "10" });
  });

  it("reads the journal with a default of the last 100 entries", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/journal$/, () => ["line 1", "line 2"]);
    const r = await env.call("proxmox_get_node_journal", { node: "pve1", unit: "pveproxy" });
    assert.deepEqual(r.lines, ["line 1", "line 2"]);
    assert.deepEqual(env.mock.requestsTo(`${N}/journal`)[0].query, { lastentries: "100", unit: "pveproxy" });
  });

  it("converts journal times to epoch and rejects bad ones", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/journal$/, () => []);
    await env.call("proxmox_get_node_journal", { node: "pve1", since: "2023-11-14T22:13:20Z", until: 1700000100, priority: "3" });
    assert.deepEqual(env.mock.requestsTo(`${N}/journal`)[0].query, { since: "1700000000", until: "1700000100", priority: "3" });
    assert.match(errorText(await env.callRaw("proxmox_get_node_journal", { node: "pve1", since: "not a date" })), /Not a valid time/);
  });
});

describe("updates and repositories", () => {
  it("lists updates with counts", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/apt\/update$/, () => [
      { Package: "pve-manager", OldVersion: "9.0.9", Version: "9.0.10", Origin: "Proxmox", Priority: "optional", Title: "PVE manager" },
      { Package: "proxmox-kernel-6.14", OldVersion: "1", Version: "2", Origin: "Proxmox", Priority: "optional" },
      { Package: "libc6", OldVersion: "2.41-1", Version: "2.41-2", Origin: "Debian", Priority: "required" },
    ]);
    const r = await env.call("proxmox_list_node_updates", { node: "pve1" });
    assert.equal(r.total, 3);
    assert.deepEqual(r.by_origin, { Proxmox: 2, Debian: 1 });
    assert.equal(r.kernel_update, true);
    assert.deepEqual(r.items[0], { package: "libc6", installed: "2.41-1", new: "2.41-2", origin: "Debian", priority: "required" });
  });

  it("refreshes the package index as a task", async () => {
    const upid = env.mock.upid("pve1", "aptupdate", "");
    env.mock.on("POST", /^\/nodes\/pve1\/apt\/update$/, () => upid);
    const r = await env.call("proxmox_refresh_node_updates", { node: "pve1" });
    assert.equal(r.status, "ok");
    assert.equal(r.task.type, "aptupdate");
  });

  it("gets a changelog and package versions", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/apt\/changelog$/, () => "pve-manager (9.0.10) ...");
    const c = await env.call("proxmox_get_package_changelog", { node: "pve1", package: "pve-manager" });
    assert.equal(c.text, "pve-manager (9.0.10) ...");
    assert.deepEqual(env.mock.requestsTo(`${N}/apt/changelog`)[0].query, { name: "pve-manager" });

    env.mock.on("GET", /^\/nodes\/pve1\/apt\/versions$/, () => [
      { Package: "proxmox-ve", Version: "9.0.0", CurrentState: "Installed", RunningKernel: "6.14.8-2-pve", ManagerVersion: "9.0.10" },
      { Package: "zfsutils-linux", Version: "2.3", CurrentState: "NotInstalled" },
    ]);
    const v = await env.call("proxmox_get_node_package_versions", { node: "pve1" });
    assert.equal(v.running_kernel, "6.14.8-2-pve");
    assert.equal(v.manager_version, "9.0.10");
    assert.deepEqual(v.items, [{ package: "proxmox-ve", version: "9.0.0" }, { package: "zfsutils-linux", state: "NotInstalled" }]);
  });

  const REPOS = {
    digest: "dg",
    files: [
      {
        path: "/etc/apt/sources.list.d/pve-enterprise.sources",
        "file-type": "sources",
        repositories: [{ Types: ["deb"], URIs: ["https://enterprise.proxmox.com/debian/pve"], Suites: ["trixie"], Components: ["pve-enterprise"], Enabled: true }],
      },
    ],
    errors: [{ path: "/etc/apt/broken.list", error: "parse error" }],
    infos: [{ path: "/etc/apt/sources.list.d/pve-enterprise.sources", index: "0", kind: "warning", message: "no subscription" }],
    "standard-repos": [
      { handle: "enterprise", name: "Enterprise", status: true },
      { handle: "no-subscription", name: "No-Subscription" },
    ],
  };

  it("summarizes repositories", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/apt\/repositories$/, () => REPOS);
    const r = await env.call("proxmox_list_node_repositories", { node: "pve1" });
    assert.equal(r.repositories[0].enabled, true);
    assert.equal(r.repositories[0].components, "pve-enterprise");
    assert.deepEqual(r.repositories[0].notes, ["warning: no subscription"]);
    assert.deepEqual(r.errors, ["/etc/apt/broken.list: parse error"]);
    assert.deepEqual(r.standard_repos[1], { handle: "no-subscription", name: "No-Subscription", status: "not configured" });
  });

  it("enables/disables or adds repositories", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/apt\/repositories$/, () => REPOS);
    await env.call("proxmox_set_node_repository", { node: "pve1", path: "/etc/apt/x.sources", index: 0, enabled: false, digest: "dg" });
    assert.deepEqual([env.mock.lastWrite()!.method, env.mock.lastWrite()!.body], ["POST", { path: "/etc/apt/x.sources", index: "0", enabled: "0", digest: "dg" }]);
    await env.call("proxmox_set_node_repository", { node: "pve1", handle: "no-subscription" });
    assert.deepEqual([env.mock.lastWrite()!.method, env.mock.lastWrite()!.body], ["PUT", { handle: "no-subscription" }]);
    assert.match(errorText(await env.callRaw("proxmox_set_node_repository", { node: "pve1", path: "/x" })), /give path, index and enabled/);
  });
});

describe("subscription", () => {
  it("gets, sets, refreshes and deletes without leaking the key", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/subscription$/, () => ({ status: "active", level: "b", key: "pve1b-topsecret", productname: "Basic", nextduedate: "2027-01-01", serverid: "X" }));
    const s = await env.call("proxmox_get_node_subscription", { node: "pve1" });
    assert.equal(s.level, "b");
    assert.equal(s.next_due_date, "2027-01-01");
    assert.ok(!JSON.stringify(s).includes("topsecret"));

    const set = await env.call("proxmox_set_node_subscription", { node: "pve1", key: "pve1b-newsecret" });
    assert.deepEqual([env.mock.lastWrite()!.method, env.mock.lastWrite()!.body], ["PUT", { key: "pve1b-newsecret" }]);
    assert.ok(!JSON.stringify(set).includes("secret"));

    await env.call("proxmox_set_node_subscription", { node: "pve1", force: true });
    assert.deepEqual([env.mock.lastWrite()!.method, env.mock.lastWrite()!.body], ["POST", { force: "1" }]);

    await env.call("proxmox_delete_node_subscription", { node: "pve1" });
    assert.deepEqual([env.mock.lastWrite()!.method, env.mock.lastWrite()!.path], ["DELETE", `${N}/subscription`]);
  });
});

describe("certificates", () => {
  const PEM = "-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----";
  const KEY = "-----BEGIN PRIVATE KEY-----\nSECRETKEYDATA\n-----END PRIVATE KEY-----";

  it("lists certificate info without PEM bodies", async () => {
    const notafter = Math.floor(Date.now() / 1000) + 10 * 86400 + 60;
    env.mock.on("GET", /^\/nodes\/pve1\/certificates\/info$/, () => [
      { filename: "pveproxy-ssl.pem", subject: "/CN=pve1", issuer: "/CN=R3", san: ["pve1.example.com"], notbefore: 1700000000, notafter, fingerprint: "AB:CD", "public-key-type": "rsaEncryption", "public-key-bits": 2048, pem: PEM },
    ]);
    const r = await env.call("proxmox_get_node_certificates", { node: "pve1" });
    const c = r.certificates[0];
    assert.equal(c.days_left, 10);
    assert.equal(c.public_key, "rsaEncryption 2048 bits");
    assert.deepEqual(c.san, ["pve1.example.com"]);
    assert.ok(!JSON.stringify(r).includes("BEGIN"));
  });

  it("uploads a custom certificate without echoing the key", async () => {
    env.mock.on("POST", /^\/nodes\/pve1\/certificates\/custom$/, () => ({ filename: "pveproxy-ssl.pem", subject: "/CN=pve1", pem: PEM }));
    const r = await env.call("proxmox_upload_node_certificate", { node: "pve1", certificates: PEM, key: KEY, force: true, restart: true });
    assert.equal(r.uploaded, true);
    assert.equal(r.certificate.subject, "/CN=pve1");
    assert.ok(!JSON.stringify(r).includes("SECRETKEYDATA"));
    assert.ok(!JSON.stringify(r).includes("BEGIN"));
    assert.deepEqual(env.mock.lastWrite()!.body, { certificates: PEM, key: KEY, force: "1", restart: "1" });
  });

  it("deletes the custom certificate", async () => {
    await env.call("proxmox_delete_node_certificate", { node: "pve1", restart: true });
    const w = env.mock.lastWrite()!;
    assert.deepEqual([w.method, w.path, w.query], ["DELETE", `${N}/certificates/custom`, { restart: "1" }]);
  });

  it("orders, renews and revokes ACME certificates", async () => {
    const path = /^\/nodes\/pve1\/certificates\/acme\/certificate$/;
    for (const [action, method] of [["order", "POST"], ["renew", "PUT"], ["revoke", "DELETE"]] as const) {
      const upid = env.mock.upid("pve1", `acme${action}`, "");
      env.mock.on(method, path, () => upid);
      const r = await env.call("proxmox_node_acme_certificate", { node: "pve1", action, force: true });
      assert.equal(r.status, "ok");
      assert.equal(env.mock.lastWrite()!.method, method);
    }
    assert.deepEqual(env.mock.requestsTo(`${N}/certificates/acme/certificate`, "POST")[0].body, { force: "1" });
    assert.deepEqual(env.mock.requestsTo(`${N}/certificates/acme/certificate`, "DELETE")[0].query, {});

    const writeOnly = await connect(env.client, toolOptions("write"));
    try {
      const denied = await writeOnly.callTool({ name: "proxmox_node_acme_certificate", arguments: { node: "pve1", action: "revoke" } });
      assert.match(errorText(denied), /PROXMOX_ALLOW_DELETES/);
    } finally {
      await writeOnly.close();
    }
  });
});

describe("cluster ACME", () => {
  it("lists and gets accounts", async () => {
    env.mock.on("GET", /^\/cluster\/acme\/account$/, () => [{ name: "default" }]);
    env.mock.on("GET", /^\/cluster\/acme\/account\/default$/, () => ({
      directory: "https://acme-v02.api.letsencrypt.org/directory",
      location: "https://acme/acct/1",
      account: { status: "valid", contact: ["mailto:a@b.c"], key: { kty: "RSA", n: "pub" } },
    }));
    const list = await env.call("proxmox_list_acme_accounts");
    assert.deepEqual(list, [{ name: "default", directory: "https://acme-v02.api.letsencrypt.org/directory", status: "valid", contact: ["mailto:a@b.c"] }]);
    const acc = await env.call("proxmox_get_acme_account", {});
    assert.equal(acc.account.status, "valid");
  });

  it("registers, updates and deactivates accounts", async () => {
    const upid = env.mock.upid("pve1", "acmeregister", "default");
    env.mock.on("POST", /^\/cluster\/acme\/account$/, () => upid);
    const r = await env.call("proxmox_save_acme_account", { create: true, contact: "a@b.c", tos_url: "https://tos", eab_kid: "kid", eab_hmac_key: "hmac" });
    assert.equal(r.status, "ok");
    assert.equal(r.registered, true);
    assert.ok(!JSON.stringify(r).includes("hmac\""));
    assert.deepEqual(env.mock.lastWrite()!.body, { name: "default", contact: "a@b.c", tos_url: "https://tos", "eab-kid": "kid", "eab-hmac-key": "hmac" });
    assert.match(errorText(await env.callRaw("proxmox_save_acme_account", { create: true })), /contact is required/);

    env.mock.on("PUT", /^\/cluster\/acme\/account\/le$/, () => env.mock.upid("pve1", "acmeupdate", "le"));
    await env.call("proxmox_save_acme_account", { name: "le", contact: "x@y.z" });
    assert.deepEqual([env.mock.lastWrite()!.method, env.mock.lastWrite()!.body], ["PUT", { contact: "x@y.z" }]);

    env.mock.on("DELETE", /^\/cluster\/acme\/account\/le$/, () => env.mock.upid("pve1", "acmedeactivate", "le"));
    const d = await env.call("proxmox_delete_acme_account", { name: "le" });
    assert.equal(d.deleted, true);
  });

  it("lists directories with CA metadata", async () => {
    env.mock.on("GET", /^\/cluster\/acme\/directories$/, () => [{ name: "Let's Encrypt V2", url: "https://le/dir" }]);
    env.mock.on("GET", /^\/cluster\/acme\/meta$/, () => ({ termsOfService: "https://le/tos", externalAccountRequired: false }));
    const r = await env.call("proxmox_list_acme_directories", { directory: "https://le/dir" });
    assert.equal(r.directories.length, 1);
    assert.equal(r.meta.termsOfService, "https://le/tos");
    assert.deepEqual(env.mock.requestsTo("/cluster/acme/meta")[0].query, { directory: "https://le/dir" });
  });

  it("lists, gets and saves plugins without exposing credentials", async () => {
    const plugin = { plugin: "cf", type: "dns", api: "cf", data: "CF_Token=supersecret\nCF_Account_ID=acct", digest: "pd" };
    env.mock.on("GET", /^\/cluster\/acme\/plugins$/, () => [plugin, { plugin: "standalone", type: "standalone" }]);
    env.mock.on("GET", /^\/cluster\/acme\/plugins\/cf$/, () => plugin);
    const list = await env.call("proxmox_list_acme_plugins", {});
    assert.deepEqual(list[0].data_keys, ["CF_Token=<redacted>", "CF_Account_ID=<redacted>"]);
    assert.ok(!JSON.stringify(list).includes("supersecret"));
    const one = await env.call("proxmox_get_acme_plugin", { id: "cf" });
    assert.equal(one.api, "cf");
    assert.equal(one.data, undefined);

    const saved = await env.call("proxmox_save_acme_plugin", { id: "cf", create: true, api: "cf", data: { CF_Token: "supersecret" }, nodes: ["pve1", "pve2"], validation_delay: 60 });
    assert.ok(!JSON.stringify(saved).includes("supersecret"));
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.deepEqual(w.body, { id: "cf", type: "dns", api: "cf", data: Buffer.from("CF_Token=supersecret").toString("base64"), nodes: "pve1,pve2", "validation-delay": "60" });

    await env.call("proxmox_save_acme_plugin", { id: "cf", disable: true, delete: ["nodes"] });
    assert.deepEqual([env.mock.lastWrite()!.method, env.mock.lastWrite()!.path, env.mock.lastWrite()!.body], ["PUT", "/cluster/acme/plugins/cf", { disable: "1", delete: "nodes" }]);

    await env.call("proxmox_delete_acme_plugin", { id: "cf" });
    assert.deepEqual([env.mock.lastWrite()!.method, env.mock.lastWrite()!.path], ["DELETE", "/cluster/acme/plugins/cf"]);
  });

  it("redacts base64-encoded plugin data (as the API returns it)", async () => {
    // "CF_Token=s3cret!!" encodes with "=" padding; splitting the base64 at "=" would leak it.
    const data = Buffer.from("CF_Token=s3cret!!\nCF_Account_ID=acct1").toString("base64");
    assert.ok(data.endsWith("="));
    env.mock.on("GET", /^\/cluster\/acme\/plugins\/b64$/, () => ({ plugin: "b64", type: "dns", api: "cf", data }));
    const one = await env.call("proxmox_get_acme_plugin", { id: "b64" });
    assert.deepEqual(one.data_keys, ["CF_Token=<redacted>", "CF_Account_ID=<redacted>"]);
    assert.ok(!JSON.stringify(one).includes(data.slice(0, 10)));
  });

  it("filters the challenge schema", async () => {
    env.mock.on("GET", /^\/cluster\/acme\/challenge-schema$/, () => [
      { id: "cf", name: "Cloudflare", type: "dns", schema: { fields: { CF_Token: { type: "string" } } } },
      { id: "ovh", name: "OVH", type: "dns", schema: { fields: {} } },
    ]);
    const r = await env.call("proxmox_get_acme_challenge_schema", { api: "cf" });
    assert.deepEqual(r, [{ api: "cf", name: "Cloudflare", type: "dns", fields: { CF_Token: { type: "string" } } }]);
  });
});
