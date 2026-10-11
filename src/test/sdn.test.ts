import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { errorText, startEnv, type TestEnv } from "./helpers.js";

describe("sdn tools", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv();
  });
  after(async () => {
    await env.stop();
  });
  beforeEach(() => env.mock.reset());

  it("lists zones with pending state and passes filters", async () => {
    env.mock.on("GET", /^\/cluster\/sdn\/zones$/, () => [
      { zone: "vxz", type: "vxlan", peers: "10.0.0.11,10.0.0.12", digest: "d1", state: "new", pending: { mtu: 1450 } },
      { zone: "lan", type: "vlan", bridge: "vmbr0", digest: "d2" },
    ]);
    const res = await env.call("proxmox_list_sdn_objects", { kind: "zone", pending: true, type: "vlan" });
    const req = env.mock.requestsTo("/cluster/sdn/zones", "GET").at(-1)!;
    assert.equal(req.query.pending, "1");
    assert.equal(req.query.type, "vlan");
    assert.equal(res.total, 2);
    assert.deepEqual(
      res.items.map((z: any) => z.zone),
      ["lan", "vxz"],
    );
    assert.equal(res.items[1].state, "new");
    assert.deepEqual(res.items[1].pending, { mtu: 1450 });
    assert.equal(res.items[0].digest, undefined);
  });

  it("lists subnets of a vnet and requires the vnet", async () => {
    env.mock.on("GET", /^\/cluster\/sdn\/vnets\/vnet1\/subnets$/, () => [{ subnet: "z1-10.0.0.0-24", cidr: "10.0.0.0/24", type: "subnet", gateway: "10.0.0.1" }]);
    const res = await env.call("proxmox_list_sdn_objects", { kind: "subnet", vnet: "vnet1" });
    assert.equal(res.items[0].gateway, "10.0.0.1");
    const err = errorText(await env.callRaw("proxmox_list_sdn_objects", { kind: "subnet" }));
    assert.match(err, /vnet.*required/);
  });

  it("redacts IPAM tokens and ignores pending for ipams", async () => {
    env.mock.on("GET", /^\/cluster\/sdn\/ipams$/, () => [{ ipam: "nb", type: "netbox", url: "https://nb", token: "s3cret" }]);
    const res = await env.call("proxmox_list_sdn_objects", { kind: "ipam", pending: true });
    assert.equal(res.items[0].token, "<redacted>");
    assert.equal(env.mock.requestsTo("/cluster/sdn/ipams", "GET").at(-1)!.query.pending, undefined);
  });

  it("gets one object with pending and searches", async () => {
    env.mock.on("GET", /^\/cluster\/sdn\/vnets\/vnet1$/, () => ({ vnet: "vnet1", zone: "lan", tag: 20, digest: "abc" }));
    const v = await env.call("proxmox_get_sdn_object", { kind: "vnet", id: "vnet1", pending: true });
    assert.equal(v.tag, 20);
    assert.equal(v.digest, "abc");
    assert.equal(env.mock.requestsTo("/cluster/sdn/vnets/vnet1", "GET").at(-1)!.query.pending, "1");
  });

  it("redacts the DNS plugin key", async () => {
    env.mock.on("GET", /^\/cluster\/sdn\/dns\/pdns$/, () => ({ dns: "pdns", type: "powerdns", key: "k3y", url: "http://pdns" }));
    const d = await env.call("proxmox_get_sdn_object", { kind: "dns", id: "pdns" });
    assert.equal(d.key, "<redacted>");
    assert.equal(d.url, "http://pdns");
  });

  it("translates subnet CIDRs to subnet IDs (<zone>-<ip>-<prefix>) in paths", async () => {
    env.mock.on("GET", /^\/cluster\/sdn\/vnets\/vnet1$/, () => ({ vnet: "vnet1", zone: "z1" }));
    env.mock.on("GET", /^\/cluster\/sdn\/vnets\/vnet1\/subnets\/(.+)$/, (_p, m) => ({ subnet: decodeURIComponent(m[1]) }));
    const s = await env.call("proxmox_get_sdn_object", { kind: "subnet", vnet: "vnet1", id: "10.0.0.0/24" });
    assert.equal(s.subnet, "z1-10.0.0.0-24");
    assert.ok(env.mock.requests.some((r) => r.path === "/cluster/sdn/vnets/vnet1/subnets/z1-10.0.0.0-24"));
    await env.call("proxmox_delete_sdn_object", { kind: "subnet", vnet: "vnet1", id: "10.0.0.0/24" });
    assert.equal(env.mock.lastWrite()!.path, "/cluster/sdn/vnets/vnet1/subnets/z1-10.0.0.0-24");
  });

  it("reports zone and vnet status per node", async () => {
    env.mock.on("GET", /^\/nodes\/(pve\d)\/sdn\/zones$/, (_p, m) =>
      m[1] === "pve1" ? [{ zone: "lan", status: "available" }, { zone: "vxz", status: "error" }] : [{ zone: "lan", status: "available" }],
    );
    env.mock.on("GET", /^\/nodes\/(pve\d)\/sdn\/zones\/(\w+)\/content$/, (_p, m) => [{ vnet: `${m[2]}v1`, status: "available" }]);
    const res = await env.call("proxmox_get_sdn_status", {});
    assert.deepEqual(
      res.nodes.map((n: any) => n.node),
      ["pve1", "pve2"],
    );
    const pve1 = res.nodes[0];
    assert.deepEqual(pve1.not_available, ["vxz"]);
    assert.equal(pve1.zones[0].vnets[0].vnet, "lanv1");

    const one = await env.call("proxmox_get_sdn_status", { node: "pve2", zone: "lan", vnets: false });
    assert.deepEqual(one.nodes, [{ node: "pve2", zones: [{ zone: "lan", status: "available" }] }]);
  });

  it("reports per-node errors in status without failing", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/sdn\/zones$/, () => env.mock.error(500, "boom"));
    const res = await env.call("proxmox_get_sdn_status", { node: "pve1" });
    assert.match(res.nodes[0].error, /boom/);
  });

  it("shows the dry-run diff", async () => {
    env.mock.on("GET", /^\/cluster\/sdn\/dry-run$/, (p) => ({ "interfaces-diff": `+auto vnet1 (${p.node})`, "frr-diff": "" }));
    const res = await env.call("proxmox_get_sdn_pending_diff", { node: "pve1" });
    assert.equal(res.interfaces_diff, "+auto vnet1 (pve1)");
    assert.equal(res.frr_diff, undefined);
    env.mock.on("GET", /^\/cluster\/sdn\/dry-run$/, () => ({}));
    const none = await env.call("proxmox_get_sdn_pending_diff", { node: "pve1" });
    assert.match(none.note, /No differences/);
  });

  it("lists IPAM entries with filters", async () => {
    env.mock.on("GET", /^\/cluster\/sdn\/ipams\/pve\/status$/, () => [
      { zone: "z", vnet: "vnet1", subnet: "10.0.0.0/24", ip: "10.0.0.1", gateway: 1 },
      { zone: "z", vnet: "vnet1", subnet: "10.0.0.0/24", ip: "10.0.0.5", mac: "BC:24:11:00:00:01", vmid: 100, hostname: "web" },
      { zone: "z", vnet: "vnet2", subnet: "10.1.0.0/24", ip: "10.1.0.5", vmid: 101 },
    ]);
    const all = await env.call("proxmox_get_sdn_ipam_status", {});
    assert.equal(all.ipam, "pve");
    assert.equal(all.total, 3);
    assert.equal(all.items[0].gateway, true);
    const vm = await env.call("proxmox_get_sdn_ipam_status", { vmid: 100 });
    assert.deepEqual(
      vm.items.map((e: any) => e.ip),
      ["10.0.0.5"],
    );
    const vnet = await env.call("proxmox_get_sdn_ipam_status", { vnet: "vnet2" });
    assert.equal(vnet.total, 1);
  });

  it("lists fabrics and optional live node status", async () => {
    env.mock.on("GET", /^\/cluster\/sdn\/fabrics\/all$/, () => ({ fabrics: [{ id: "f1", protocol: "openfabric" }], nodes: [{ node_id: "pve1", fabric_id: "f1" }] }));
    env.mock.on("GET", /^\/nodes\/pve1\/sdn\/fabrics\/f1\/(routes|neighbors|interfaces)$/, (_p, m) => [{ kind: m[1] }]);
    const res = await env.call("proxmox_list_sdn_fabrics", { node: "pve1", fabric: "f1" });
    assert.equal(res.fabrics[0].id, "f1");
    assert.equal(res.status.routes[0].kind, "routes");
    assert.equal(res.status.interfaces[0].kind, "interfaces");
    const plain = await env.call("proxmox_list_sdn_fabrics", {});
    assert.equal(plain.status, undefined);
  });

  it("lists prefix lists and groups route map entries", async () => {
    env.mock.on("GET", /^\/cluster\/sdn\/prefix-lists$/, () => [{ id: "pl1", entries: ["permit 10.0.0.0/8"], digest: "x" }]);
    env.mock.on("GET", /^\/cluster\/sdn\/route-maps\/entries$/, () => [
      { "route-map-id": "rm1", order: 20, action: "deny" },
      { "route-map-id": "rm1", order: 10, action: "permit", match: ["key=vni,value=100"] },
    ]);
    const res = await env.call("proxmox_list_sdn_route_policies", {});
    assert.equal(res.prefix_lists[0].id, "pl1");
    assert.equal(res.prefix_lists[0].digest, undefined);
    assert.equal(env.mock.requestsTo("/cluster/sdn/prefix-lists", "GET").at(-1)!.query.verbose, "1");
    assert.deepEqual(
      res.route_maps.rm1.map((e: any) => e.order),
      [10, 20],
    );
  });

  it("degrades gracefully when route policies aren't supported", async () => {
    const res = await env.call("proxmox_list_sdn_route_policies", {});
    assert.match(res.prefix_lists.error, /404/);
    assert.match(res.route_maps.error, /404/);
  });

  it("creates a zone with mapped parameter names", async () => {
    env.mock.on("GET", /^\/cluster\/sdn\/zones\/evpn1$/, () => ({ zone: "evpn1", type: "evpn", state: "new" }));
    const res = await env.call("proxmox_save_sdn_zone", {
      zone: "evpn1",
      create: true,
      type: "evpn",
      controller: "ctl",
      vrf_vxlan: 10000,
      exitnodes: ["pve1", "pve2"],
      advertise_subnets: true,
      nodes: ["pve1"],
      extra: { mac: "BC:24:11:00:00:AA" },
    });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.equal(w.path, "/cluster/sdn/zones");
    assert.deepEqual(w.body, {
      zone: "evpn1",
      type: "evpn",
      controller: "ctl",
      "vrf-vxlan": "10000",
      exitnodes: "pve1,pve2",
      "advertise-subnets": "1",
      nodes: "pve1",
      mac: "BC:24:11:00:00:AA",
    });
    assert.equal(res.action, "created");
    assert.equal(res.object.state, "new");
    assert.match(res.note, /proxmox_apply_sdn/);
  });

  it("requires a type when creating a zone", async () => {
    const err = errorText(await env.callRaw("proxmox_save_sdn_zone", { zone: "z1", create: true }));
    assert.match(err, /type.*required/);
  });

  it("updates a zone without sending its type", async () => {
    await env.call("proxmox_save_sdn_zone", { zone: "lan", type: "vlan", mtu: 9000, delete: ["dns"], digest: "d", lock_token: "tok" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, "/cluster/sdn/zones/lan");
    assert.deepEqual(w.body, { mtu: "9000", delete: "dns", digest: "d", "lock-token": "tok" });
  });

  it("creates and updates vnets", async () => {
    const err = errorText(await env.callRaw("proxmox_save_sdn_vnet", { vnet: "v1", create: true }));
    assert.match(err, /zone.*required/);
    await env.call("proxmox_save_sdn_vnet", { vnet: "v1", create: true, zone: "lan", tag: 30, vlanaware: false, isolate_ports: true });
    let w = env.mock.lastWrite()!;
    assert.equal(w.path, "/cluster/sdn/vnets");
    assert.deepEqual(w.body, { vnet: "v1", zone: "lan", tag: "30", vlanaware: "0", "isolate-ports": "1" });
    await env.call("proxmox_save_sdn_vnet", { vnet: "v1", alias: "guests" });
    w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, "/cluster/sdn/vnets/v1");
    assert.deepEqual(w.body, { alias: "guests" });
  });

  it("creates a subnet with DHCP ranges", async () => {
    await env.call("proxmox_save_sdn_subnet", {
      vnet: "v1",
      subnet: "10.0.0.0/24",
      create: true,
      gateway: "10.0.0.1",
      snat: true,
      dhcp_range: [
        { start: "10.0.0.100", end: "10.0.0.150" },
        { start: "10.0.0.200", end: "10.0.0.210" },
      ],
    });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.equal(w.path, "/cluster/sdn/vnets/v1/subnets");
    assert.deepEqual(w.body, {
      subnet: "10.0.0.0/24",
      type: "subnet",
      gateway: "10.0.0.1",
      snat: "1",
      "dhcp-range": ["start-address=10.0.0.100,end-address=10.0.0.150", "start-address=10.0.0.200,end-address=10.0.0.210"],
    });
  });

  it("updates a subnet without the create-only type", async () => {
    await env.call("proxmox_save_sdn_subnet", { vnet: "v1", subnet: "z-10.0.0.0-24", dhcp_dns_server: "10.0.0.53" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, "/cluster/sdn/vnets/v1/subnets/z-10.0.0.0-24");
    assert.deepEqual(w.body, { "dhcp-dns-server": "10.0.0.53" });
  });

  it("creates controllers, IPAMs and DNS plugins", async () => {
    await env.call("proxmox_save_sdn_controller", { controller: "evpnctl", create: true, type: "evpn", asn: 65000, peers: ["10.0.0.11", "10.0.0.12"] });
    let w = env.mock.lastWrite()!;
    assert.equal(w.path, "/cluster/sdn/controllers");
    assert.deepEqual(w.body, { controller: "evpnctl", type: "evpn", asn: "65000", peers: "10.0.0.11,10.0.0.12" });
    assert.match(errorText(await env.callRaw("proxmox_save_sdn_controller", { controller: "c", create: true })), /type/);

    await env.call("proxmox_save_sdn_ipam", { ipam: "nb", create: true, type: "netbox", url: "https://nb/api", token: "t0k" });
    w = env.mock.lastWrite()!;
    assert.equal(w.path, "/cluster/sdn/ipams");
    assert.deepEqual(w.body, { ipam: "nb", type: "netbox", url: "https://nb/api", token: "t0k" });
    assert.match(errorText(await env.callRaw("proxmox_save_sdn_ipam", { ipam: "x", create: true })), /type/);

    assert.match(errorText(await env.callRaw("proxmox_save_sdn_dns", { dns: "pdns", create: true, type: "powerdns" })), /url.*key/);
    env.mock.on("GET", /^\/cluster\/sdn\/dns\/pdns$/, () => ({ dns: "pdns", type: "powerdns", url: "http://pdns", key: "k3y" }));
    const res = await env.call("proxmox_save_sdn_dns", { dns: "pdns", create: true, type: "powerdns", url: "http://pdns", key: "k3y", ttl: 300 });
    w = env.mock.lastWrite()!;
    assert.equal(w.path, "/cluster/sdn/dns");
    assert.deepEqual(w.body, { dns: "pdns", type: "powerdns", url: "http://pdns", key: "k3y", ttl: "300" });
    assert.equal(res.object.key, "<redacted>");
    // DNS objects don't support pending; the read-back mustn't ask for it.
    assert.equal(env.mock.requestsTo("/cluster/sdn/dns/pdns", "GET").at(-1)!.query.pending, undefined);
  });

  it("updates an IPAM via PUT", async () => {
    await env.call("proxmox_save_sdn_ipam", { ipam: "nb", fingerprint: "AA:BB" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, "/cluster/sdn/ipams/nb");
    assert.deepEqual(w.body, { fingerprint: "AA:BB" });
  });

  it("deletes SDN objects", async () => {
    const res = await env.call("proxmox_delete_sdn_object", { kind: "vnet", id: "v1", lock_token: "tok" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "DELETE");
    assert.equal(w.path, "/cluster/sdn/vnets/v1");
    assert.equal(w.query["lock-token"], "tok");
    assert.equal(res.deleted, "v1");
    await env.call("proxmox_delete_sdn_object", { kind: "controller", id: "c1" });
    assert.equal(env.mock.lastWrite()!.path, "/cluster/sdn/controllers/c1");
    await env.call("proxmox_delete_sdn_object", { kind: "subnet", id: "z-10.0.0.0-24", vnet: "v1" });
    assert.equal(env.mock.lastWrite()!.path, "/cluster/sdn/vnets/v1/subnets/z-10.0.0.0-24");
  });

  it("manages vnet IP mappings, looking up the zone", async () => {
    env.mock.on("GET", /^\/cluster\/sdn\/vnets\/v1$/, () => ({ vnet: "v1", zone: "lan" }));
    const res = await env.call("proxmox_save_sdn_vnet_ip", { vnet: "v1", ip: "10.0.0.7", mac: "BC:24:11:00:00:07", create: true, vmid: 100 });
    let w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.equal(w.path, "/cluster/sdn/vnets/v1/ips");
    assert.deepEqual(w.body, { zone: "lan", ip: "10.0.0.7", mac: "BC:24:11:00:00:07" });
    assert.equal(res.zone, "lan");

    await env.call("proxmox_save_sdn_vnet_ip", { vnet: "v1", zone: "other", ip: "10.0.0.7", vmid: 100 });
    w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.deepEqual(w.body, { zone: "other", ip: "10.0.0.7", vmid: "100" });

    await env.call("proxmox_delete_sdn_vnet_ip", { vnet: "v1", ip: "10.0.0.7" });
    w = env.mock.lastWrite()!;
    assert.equal(w.method, "DELETE");
    assert.deepEqual(w.query, { zone: "lan", ip: "10.0.0.7" });
  });

  it("fails clearly when the vnet's zone can't be found", async () => {
    env.mock.on("GET", /^\/cluster\/sdn\/vnets\/v9$/, () => ({ vnet: "v9" }));
    assert.match(errorText(await env.callRaw("proxmox_delete_sdn_vnet_ip", { vnet: "v9", ip: "10.0.0.1" })), /pass `zone`/);
  });

  it("applies SDN as a task", async () => {
    const upid = env.mock.upid("pve1", "reloadnetworkall", "");
    env.mock.on("PUT", /^\/cluster\/sdn$/, () => upid);
    const res = await env.call("proxmox_apply_sdn", {});
    assert.equal(res.status, "ok");
    assert.equal(res.task.upid, upid);
    assert.equal(env.mock.lastWrite()!.method, "PUT");

    const started = await env.call("proxmox_apply_sdn", { wait: false, lock_token: "tok", release_lock: false });
    assert.equal(started.status, "started");
    assert.deepEqual(env.mock.lastWrite()!.body, { "lock-token": "tok", "release-lock": "0" });
  });

  it("reports a failed apply with the task log", async () => {
    const upid = env.mock.upid("pve1", "reloadnetworkall", "", { status: "stopped", exitstatus: "error during reload", log: ["vnet1: failed"] });
    env.mock.on("PUT", /^\/cluster\/sdn$/, () => upid);
    const err = errorText(await env.callRaw("proxmox_apply_sdn", {}));
    assert.match(err, /error during reload/);
    assert.match(err, /vnet1: failed/);
  });

  it("rolls back pending changes", async () => {
    const res = await env.call("proxmox_rollback_sdn", {});
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.equal(w.path, "/cluster/sdn/rollback");
    assert.equal(res.rolled_back, true);
  });
});
