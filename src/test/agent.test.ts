import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { errorText, startEnv, type TestEnv } from "./helpers.js";

const AGENT = "/nodes/pve1/qemu/100/agent";

describe("agent tools", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv();
  });
  after(() => env.stop());
  beforeEach(() => env.mock.reset());

  it("combines guest info and tolerates individual failures", async () => {
    env.mock
      .on("POST", /\/agent\/ping$/, () => ({ result: {} }))
      .on("GET", /\/agent\/get-osinfo$/, () => ({
        result: { "pretty-name": "Debian GNU/Linux 12 (bookworm)", id: "debian", "version-id": "12", "kernel-release": "6.1.0-18-amd64", machine: "x86_64" },
      }))
      .on("GET", /\/agent\/get-host-name$/, () => ({ result: { "host-name": "web01" } }))
      .on("GET", /\/agent\/get-timezone$/, () => ({ result: { zone: "UTC", offset: 0 } }))
      .on("GET", /\/agent\/get-time$/, () => ({ result: Date.now() * 1e6 }))
      .on("GET", /\/agent\/get-vcpus$/, () => ({ result: [{ "logical-id": 0, online: true }, { "logical-id": 1, online: false }] }))
      .on("GET", /\/agent\/info$/, () => env.mock.error(500, "command disabled"));
    const r = await env.call("proxmox_get_guest_info", { vmid: 100 });
    assert.equal(r.node, "pve1");
    assert.equal(r.hostname, "web01");
    assert.equal(r.os.name, "Debian GNU/Linux 12 (bookworm)");
    assert.equal(r.os.kernel, "6.1.0-18-amd64");
    assert.deepEqual(r.vcpus, { total: 2, online: 1 });
    assert.equal(r.timezone.zone, "UTC");
    assert.ok(Math.abs(r.clock_skew_s) < 5);
    assert.match(r.errors.info, /command disabled/);
    assert.equal(r.agent_version, undefined);
  });

  it("explains when the agent is not running or not configured", async () => {
    env.mock.on("POST", /\/agent\/ping$/, () => env.mock.error(500, "QEMU guest agent is not running"));
    assert.match(errorText(await env.callRaw("proxmox_get_guest_info", { vmid: 100 })), /not responding.*qemu-guest-agent service/);
    env.mock.on("GET", /\/agent\/get-fsinfo$/, () => env.mock.error(500, "No QEMU guest agent configured"));
    assert.match(errorText(await env.callRaw("proxmox_get_guest_filesystems", { vmid: 100 })), /not enabled for VM 100.*agent=1/);
  });

  it("rejects containers", async () => {
    assert.match(errorText(await env.callRaw("proxmox_get_guest_network", { vmid: 200 })), /is a container, not a VM/);
  });

  it("summarises network interfaces and hides loopback", async () => {
    env.mock.on("GET", /\/agent\/network-get-interfaces$/, () => ({
      result: [
        { name: "lo", "hardware-address": "00:00:00:00:00:00", "ip-addresses": [{ "ip-address-type": "ipv4", "ip-address": "127.0.0.1", prefix: 8 }] },
        {
          name: "eth0",
          "hardware-address": "bc:24:11:00:00:01",
          "ip-addresses": [
            { "ip-address-type": "ipv4", "ip-address": "10.0.10.5", prefix: 24 },
            { "ip-address-type": "ipv6", "ip-address": "fe80::1", prefix: 64 },
          ],
          statistics: { "rx-bytes": 1048576, "tx-bytes": 2048, "rx-packets": 10, "tx-packets": 5, "rx-errs": 0, "rx-dropped": 3 },
        },
      ],
    }));
    const r = await env.call("proxmox_get_guest_network", { vmid: 100 });
    assert.deepEqual(r.ipv4, ["10.0.10.5"]);
    assert.equal(r.interfaces.length, 1);
    const eth = r.interfaces[0];
    assert.deepEqual(eth, {
      name: "eth0",
      mac: "bc:24:11:00:00:01",
      ipv4: ["10.0.10.5/24"],
      ipv6: ["fe80::1/64"],
      rx: "1 MiB",
      tx: "2 KiB",
      rx_packets: 10,
      tx_packets: 5,
      rx_dropped: 3,
    });
    const all = await env.call("proxmox_get_guest_network", { vmid: 100, include_loopback: true });
    assert.equal(all.interfaces.length, 2);
  });

  it("summarises filesystems", async () => {
    env.mock.on("GET", /\/agent\/get-fsinfo$/, () => ({
      result: [
        { name: "sda2", mountpoint: "/boot", type: "ext4", "used-bytes": 100 * 2 ** 20, "total-bytes": 1024 * 2 ** 20, disk: [{ dev: "/dev/sda2", serial: "drive-scsi0", "bus-type": "scsi" }] },
        { name: "sda1", mountpoint: "/", type: "ext4", "used-bytes": 8 * 2 ** 30, "total-bytes": 32 * 2 ** 30, disk: [] },
      ],
    }));
    const r = await env.call("proxmox_get_guest_filesystems", { vmid: 100 });
    assert.deepEqual(r.filesystems.map((f: any) => f.mountpoint), ["/", "/boot"]);
    assert.equal(r.filesystems[0].usage, "8 GiB of 32 GiB (25%)");
    assert.equal(r.filesystems[0].used_pct, 25);
    assert.deepEqual(r.filesystems[1].disks, [{ dev: "/dev/sda2", serial: "drive-scsi0", bus: "scsi" }]);
  });

  it("lists users and memory blocks", async () => {
    env.mock.on("GET", /\/agent\/get-users$/, () => ({ result: [{ user: "matt", "login-time": 1700000000.5 }] }));
    const u = await env.call("proxmox_get_guest_users", { vmid: 100 });
    assert.deepEqual(u.users, [{ user: "matt", login_time: "2023-11-14T22:13:20.500Z" }]);

    env.mock
      .on("GET", /\/agent\/get-memory-blocks$/, () => ({ result: [{ "phys-index": 0, online: true }, { "phys-index": 1, online: true }, { "phys-index": 2, online: false }] }))
      .on("GET", /\/agent\/get-memory-block-info$/, () => ({ result: { size: 134217728 } }));
    const m = await env.call("proxmox_get_guest_memory_blocks", { vmid: 100 });
    assert.equal(m.blocks, 3);
    assert.equal(m.online_blocks, 2);
    assert.equal(m.block_size, "128 MiB");
    assert.equal(m.online_memory, "256 MiB");
  });

  it("freezes, thaws and reports freeze state", async () => {
    env.mock.on("POST", /\/agent\/fsfreeze-(freeze|thaw|status)$/, (_p, m) => ({ result: m[1] === "status" ? "thawed" : 2 }));
    const f = await env.call("proxmox_guest_fs_freeze", { vmid: 100, action: "freeze" });
    assert.equal(env.mock.lastWrite()?.path, `${AGENT}/fsfreeze-freeze`);
    assert.equal(f.filesystems, 2);
    const s = await env.call("proxmox_guest_fs_freeze", { vmid: 100, action: "status" });
    assert.equal(s.status, "thawed");
  });

  it("runs fstrim and agent power actions", async () => {
    env.mock.on("POST", /\/agent\/fstrim$/, () => ({ result: { paths: [{ path: "/", trimmed: 1073741824 }, { path: "/boot", error: "not supported" }] } }));
    const t = await env.call("proxmox_guest_fstrim", { vmid: 100 });
    assert.deepEqual(t.paths, [{ path: "/", trimmed: "1 GiB" }, { path: "/boot", error: "not supported" }]);

    const p = await env.call("proxmox_guest_power", { vmid: 100, action: "suspend-disk" });
    assert.equal(p.status, "requested");
    assert.equal(env.mock.lastWrite()?.path, `${AGENT}/suspend-disk`);
  });

  it("runs a command and polls until it exits", async () => {
    env.mock.on("POST", /\/agent\/exec$/, () => ({ pid: 4242 }));
    let polls = 0;
    env.mock.on("GET", /\/agent\/exec-status$/, (p) => {
      assert.equal(p.pid, "4242");
      return ++polls < 2 ? { exited: 0 } : { exited: 1, exitcode: 0, "out-data": "hello\n", "err-data": "warn\n", "out-truncated": 1 };
    });
    const r = await env.call("proxmox_guest_exec", { vmid: 100, command: ["/bin/sh", "-c", "echo hello"], input_data: "stdin!" });
    const w = env.mock.requestsTo(`${AGENT}/exec`, "POST")[0];
    assert.deepEqual(w.body.command, ["/bin/sh", "-c", "echo hello"]);
    assert.equal(w.body["input-data"], "stdin!");
    assert.equal(r.pid, 4242);
    assert.equal(r.exited, true);
    assert.equal(r.exitcode, 0);
    assert.equal(r.stdout, "hello\n");
    assert.equal(r.stderr, "warn\n");
    assert.equal(r.stdout_truncated, true);
    assert.equal(polls, 2);
  });

  it("returns the pid when a command outlives the timeout, and checks it later", async () => {
    env.mock.on("POST", /\/agent\/exec$/, () => ({ pid: 7 }));
    env.mock.on("GET", /\/agent\/exec-status$/, () => ({ exited: 0 }));
    const r = await env.call("proxmox_guest_exec", { vmid: 100, command: ["sleep", "100"], timeout_s: 0 });
    assert.equal(r.exited, false);
    assert.match(r.note, /proxmox_get_guest_exec_status \(pid 7\)/);

    env.mock.on("GET", /\/agent\/exec-status$/, () => ({ exited: 1, exitcode: 1, signal: 9 }));
    const s = await env.call("proxmox_get_guest_exec_status", { vmid: 100, pid: 7 });
    assert.equal(s.exited, true);
    assert.equal(s.exitcode, 1);
    assert.equal(s.signal, 9);
  });

  it("reads files as text or base64", async () => {
    env.mock.on("GET", /\/agent\/file-read$/, (p) => (p.decode === "0" ? { content: "aGk=" } : { content: "PRETTY_NAME=Debian\n", truncated: 1 }));
    const t = await env.call("proxmox_guest_read_file", { vmid: 100, file: "/etc/os-release" });
    assert.equal(t.content, "PRETTY_NAME=Debian\n");
    assert.equal(t.truncated, true);
    assert.equal(t.bytes, 19);
    const q = env.mock.requestsTo(`${AGENT}/file-read`).at(-1)!.query;
    assert.equal(q.file, "/etc/os-release");
    assert.equal(q.count, "1048576");
    assert.equal(q.decode, undefined);

    const b = await env.call("proxmox_guest_read_file", { vmid: 100, file: "/bin/x", encoding: "base64", offset: 10, count: 2 });
    assert.equal(b.content, "aGk=");
    assert.equal(b.bytes, 2);
    assert.equal(b.truncated, false);
    const q2 = env.mock.requestsTo(`${AGENT}/file-read`).at(-1)!.query;
    assert.deepEqual([q2.decode, q2.offset, q2.count], ["0", "10", "2"]);
  });

  it("writes files", async () => {
    const r = await env.call("proxmox_guest_write_file", { vmid: 100, file: "/tmp/a.txt", content: "hello" });
    let w = env.mock.lastWrite()!;
    assert.equal(w.path, `${AGENT}/file-write`);
    assert.deepEqual(w.body, { file: "/tmp/a.txt", content: "hello" });
    assert.equal(r.bytes, 5);

    await env.call("proxmox_guest_write_file", { vmid: 100, file: "/tmp/b", content: "aGk=", encoding: "base64" });
    w = env.mock.lastWrite()!;
    assert.equal(w.body.encode, "0");
  });

  it("sets a user password without echoing it", async () => {
    const r = await env.call("proxmox_guest_set_password", { vmid: 100, username: "root", password: "s3cret!" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.path, `${AGENT}/set-user-password`);
    assert.deepEqual(w.body, { username: "root", password: "s3cret!" });
    assert.ok(!JSON.stringify(r).includes("s3cret!"));
  });
});
