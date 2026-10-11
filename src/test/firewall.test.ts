import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { ruleLine } from "../tools/firewall.js";
import { errorText, startEnv, type TestEnv } from "./helpers.js";

const RULES = [
  { pos: 1, type: "in", action: "DROP", enable: 0, source: "+dc/blacklist", digest: "d1" },
  { pos: 0, type: "in", action: "ACCEPT", macro: "SSH", iface: "net0", source: "+dc/management", enable: 1, log: "info", comment: "admins", digest: "d1" },
  { pos: 2, type: "group", action: "webservers", enable: 1, digest: "d1" },
];

describe("firewall tools", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv();
  });
  after(async () => env.stop());
  beforeEach(() => env.mock.reset());

  describe("ruleLine", () => {
    it("renders rules like the firewall config", () => {
      assert.equal(ruleLine(RULES[1]), "IN SSH(ACCEPT) -i net0 -source +dc/management -log info # admins");
      assert.equal(ruleLine(RULES[0]), "[disabled] IN DROP -source +dc/blacklist");
      assert.equal(ruleLine(RULES[2]), "GROUP webservers");
      assert.equal(ruleLine({ type: "out", action: "REJECT", proto: "tcp", dport: "25", dest: "10.0.0.0/8", log: "nolog" }), "OUT REJECT -dest 10.0.0.0/8 -p tcp -dport 25");
    });
  });

  describe("scopes", () => {
    it("lists cluster rules sorted, with summaries", async () => {
      env.mock.on("GET", /^\/cluster\/firewall\/rules$/, () => RULES);
      const out = await env.call("proxmox_list_firewall_rules", { scope: "cluster" });
      assert.equal(out.scope, "cluster");
      assert.equal(out.count, 3);
      assert.equal(out.enabled, 2);
      assert.equal(out.digest, "d1");
      assert.deepEqual(out.rules.map((r: any) => r.pos), [0, 1, 2]);
      assert.equal(out.rules[0].rule, "IN SSH(ACCEPT) -i net0 -source +dc/management -log info # admins");
      assert.equal(out.rules[0].enable, true);
      assert.equal(out.rules[1].enable, false);
    });

    it("defaults to cluster scope and returns raw rules on request", async () => {
      env.mock.on("GET", /^\/cluster\/firewall\/rules$/, () => RULES);
      const out = await env.call("proxmox_list_firewall_rules", { raw: true });
      assert.equal(out.scope, "cluster");
      assert.equal(out.rules[0].digest, "d1");
    });

    it("infers node, guest and group scope from the arguments", async () => {
      env.mock.on("GET", /firewall\/rules$|firewall\/groups\/[^/]+$/, () => []);
      const node = await env.call("proxmox_list_firewall_rules", { node: "pve2" });
      assert.deepEqual([node.scope, node.node], ["node", "pve2"]);
      const vm = await env.call("proxmox_list_firewall_rules", { vmid: 100 });
      assert.deepEqual([vm.scope, vm.node, vm.type, vm.name], ["guest", "pve1", "vm", "web"]);
      const ct = await env.call("proxmox_list_firewall_rules", { vmid: 200 });
      assert.equal(ct.type, "container");
      const grp = await env.call("proxmox_list_firewall_rules", { group: "webservers" });
      assert.equal(grp.scope, "group");
      const paths = env.mock.requestsTo(/firewall/, "GET").map((r) => r.path);
      assert.deepEqual(paths, [
        "/nodes/pve2/firewall/rules",
        "/nodes/pve1/qemu/100/firewall/rules",
        "/nodes/pve1/lxc/200/firewall/rules",
        "/cluster/firewall/groups/webservers",
      ]);
    });

    it("rejects inconsistent or unsupported scopes", async () => {
      assert.match(errorText(await env.callRaw("proxmox_list_firewall_rules", { scope: "guest" })), /vmid is required/);
      assert.match(errorText(await env.callRaw("proxmox_list_firewall_rules", { scope: "group" })), /group is required/);
      assert.match(errorText(await env.callRaw("proxmox_list_firewall_rules", { scope: "cluster", vmid: 100 })), /vmid only applies/);
      assert.match(errorText(await env.callRaw("proxmox_list_firewall_rules", { scope: "cluster", node: "pve1" })), /node does not apply/);
      assert.match(errorText(await env.callRaw("proxmox_list_firewall_aliases", { scope: "node", node: "pve1" })), /Invalid|scope/);
      // node scope without a node on a two-node cluster needs a node
      assert.match(errorText(await env.callRaw("proxmox_get_firewall_options", { scope: "node" })), /pass `node`/);
    });
  });

  describe("rules", () => {
    it("gets a single rule", async () => {
      env.mock.on("GET", /^\/nodes\/pve1\/qemu\/100\/firewall\/rules\/0$/, () => ({ ...RULES[1], pos: undefined }));
      const out = await env.call("proxmox_get_firewall_rule", { vmid: 100, pos: 0 });
      assert.equal(out.pos, 0);
      assert.equal(out.rule, "IN SSH(ACCEPT) -i net0 -source +dc/management -log info # admins");
      assert.equal(out.digest, "d1");
    });

    it("creates a rule with mapped field names", async () => {
      env.mock.on("GET", /^\/nodes\/pve1\/qemu\/100\/firewall\/rules\/3$/, () => ({ type: "in", action: "ACCEPT", proto: "icmp", "icmp-type": "echo-request", enable: 1 }));
      const out = await env.call("proxmox_save_firewall_rule", {
        vmid: 100,
        type: "in",
        action: "ACCEPT",
        proto: "icmp",
        icmp_type: "echo-request",
        enable: true,
        insert_at: 3,
        extra: { ipversion: 4 },
      });
      const w = env.mock.lastWrite()!;
      assert.equal(w.method, "POST");
      assert.equal(w.path, "/nodes/pve1/qemu/100/firewall/rules");
      assert.deepEqual(w.body, { type: "in", action: "ACCEPT", proto: "icmp", "icmp-type": "echo-request", enable: "1", ipversion: "4", pos: "3" });
      assert.equal(out.created, true);
      assert.equal(out.pos, 3);
      assert.equal(out.rule.rule, "IN ACCEPT -p icmp -icmp-type echo-request");
    });

    it("requires type and action to create", async () => {
      assert.match(errorText(await env.callRaw("proxmox_save_firewall_rule", { action: "ACCEPT" })), /type and action are required/);
      assert.match(errorText(await env.callRaw("proxmox_save_firewall_rule", { type: "in", action: "ACCEPT", moveto: 2 })), /moveto needs pos/);
      assert.equal(env.mock.lastWrite(), undefined);
    });

    it("updates fields and then moves the rule", async () => {
      env.mock.on("GET", /^\/cluster\/firewall\/groups\/web\/0$/, () => RULES[1]);
      const out = await env.call("proxmox_save_firewall_rule", { group: "web", pos: 2, enable: false, delete: ["log"], moveto: 0, digest: "d1" });
      const puts = env.mock.requestsTo("/cluster/firewall/groups/web/2", "PUT");
      assert.equal(puts.length, 2);
      assert.deepEqual(puts[0].body, { enable: "0", delete: "log", digest: "d1" });
      assert.deepEqual(puts[1].body, { moveto: "0" });
      assert.deepEqual(out.moved, { from: 2, to: 0 });
      assert.equal(out.pos, 0);
    });

    it("moves only when no fields are given", async () => {
      env.mock.on("GET", /^\/nodes\/pve1\/firewall\/rules\/1$/, () => RULES[0]);
      await env.call("proxmox_save_firewall_rule", { node: "pve1", pos: 4, moveto: 1 });
      const puts = env.mock.requestsTo("/nodes/pve1/firewall/rules/4", "PUT");
      assert.equal(puts.length, 1);
      assert.deepEqual(puts[0].body, { moveto: "1" });
      assert.match(errorText(await env.callRaw("proxmox_save_firewall_rule", { pos: 1 })), /Nothing to change/);
    });

    it("deletes a rule", async () => {
      env.mock.on("GET", /^\/cluster\/firewall\/rules\/1$/, () => RULES[0]);
      const out = await env.call("proxmox_delete_firewall_rule", { pos: 1, digest: "d1" });
      const w = env.mock.lastWrite()!;
      assert.equal(w.method, "DELETE");
      assert.equal(w.path, "/cluster/firewall/rules/1");
      assert.equal(w.query.digest, "d1");
      assert.equal(out.rule, "[disabled] IN DROP -source +dc/blacklist");
    });
  });

  describe("options", () => {
    it("gets options with effective defaults", async () => {
      env.mock.on("GET", /^\/nodes\/pve1\/lxc\/200\/firewall\/options$/, () => ({ enable: 1, policy_in: "ACCEPT", digest: "x" }));
      const out = await env.call("proxmox_get_firewall_options", { vmid: 200 });
      assert.deepEqual(out.options, { enable: 1, policy_in: "ACCEPT" });
      assert.equal(out.effective.enable, true);
      assert.equal(out.effective.policy_in, "ACCEPT");
      assert.equal(out.effective.policy_out, "ACCEPT");
      assert.equal(out.effective.macfilter, true);
      assert.equal(out.digest, "x");
    });

    it("updates cluster options", async () => {
      env.mock.on("GET", /^\/cluster\/firewall\/options$/, () => ({ enable: 1, policy_in: "DROP" }));
      const out = await env.call("proxmox_update_firewall_options", { scope: "cluster", enable: true, policy_forward: "DROP", log_ratelimit: "enable=1,rate=5/second" });
      const w = env.mock.lastWrite()!;
      assert.equal(w.method, "PUT");
      assert.equal(w.path, "/cluster/firewall/options");
      assert.deepEqual(w.body, { enable: "1", policy_forward: "DROP", log_ratelimit: "enable=1,rate=5/second" });
      assert.equal(out.effective.enable, true);
      assert.deepEqual(out.updated.sort(), ["enable", "log_ratelimit", "policy_forward"]);
    });

    it("updates node options and rejects options from another level", async () => {
      env.mock.on("GET", /^\/nodes\/pve2\/firewall\/options$/, () => ({}));
      await env.call("proxmox_update_firewall_options", { node: "pve2", nftables: true, protection_synflood: true, nf_conntrack_max: 524288, delete: ["log_level_in"] });
      assert.deepEqual(env.mock.lastWrite()!.body, { nftables: "1", protection_synflood: "1", nf_conntrack_max: "524288", delete: "log_level_in" });
      const err = errorText(await env.callRaw("proxmox_update_firewall_options", { vmid: 100, nftables: true, macfilter: false }));
      assert.match(err, /Not available at guest level: nftables/);
      assert.match(errorText(await env.callRaw("proxmox_update_firewall_options", { scope: "cluster" })), /Nothing to change/);
    });
  });

  describe("aliases", () => {
    it("lists aliases", async () => {
      env.mock.on("GET", /^\/cluster\/firewall\/aliases$/, () => [{ name: "nas", cidr: "10.0.0.5", digest: "a" }, { name: "lan", cidr: "10.0.0.0/24", comment: "home" }]);
      const out = await env.call("proxmox_list_firewall_aliases", { search: "home" });
      assert.deepEqual(out.items, [{ name: "lan", cidr: "10.0.0.0/24", comment: "home" }]);
    });

    it("creates a missing alias and updates/renames an existing one", async () => {
      env.mock.on("GET", /^\/nodes\/pve1\/qemu\/100\/firewall\/aliases$/, () => [{ name: "nas", cidr: "10.0.0.5" }]);
      await env.call("proxmox_save_firewall_alias", { vmid: 100, name: "db", cidr: "10.0.0.9", comment: "database" });
      let w = env.mock.lastWrite()!;
      assert.deepEqual([w.method, w.path], ["POST", "/nodes/pve1/qemu/100/firewall/aliases"]);
      assert.deepEqual(w.body, { name: "db", cidr: "10.0.0.9", comment: "database" });

      const out = await env.call("proxmox_save_firewall_alias", { vmid: 100, name: "nas", new_name: "storage" });
      w = env.mock.lastWrite()!;
      assert.deepEqual([w.method, w.path], ["PUT", "/nodes/pve1/qemu/100/firewall/aliases/nas"]);
      assert.deepEqual(w.body, { cidr: "10.0.0.5", rename: "storage" });
      assert.equal(out.renamed_from, "nas");

      assert.match(errorText(await env.callRaw("proxmox_save_firewall_alias", { vmid: 100, name: "new" })), /cidr is required/);
    });

    it("deletes an alias", async () => {
      await env.call("proxmox_delete_firewall_alias", { name: "nas" });
      const w = env.mock.lastWrite()!;
      assert.deepEqual([w.method, w.path], ["DELETE", "/cluster/firewall/aliases/nas"]);
    });
  });

  describe("IP sets", () => {
    it("lists IP sets with their entries", async () => {
      env.mock.on("GET", /^\/cluster\/firewall\/ipset$/, () => [{ name: "management", comment: "admins" }, { name: "blacklist" }]);
      env.mock.on("GET", /^\/cluster\/firewall\/ipset\/management$/, () => [{ cidr: "10.0.0.0/24", digest: "z" }, { cidr: "10.0.0.66", nomatch: 1, comment: "not him" }]);
      env.mock.on("GET", /^\/cluster\/firewall\/ipset\/blacklist$/, () => []);
      const out = await env.call("proxmox_list_firewall_ipsets", {});
      assert.equal(out.total, 2);
      assert.deepEqual(out.items[0], {
        name: "management",
        comment: "admins",
        count: 2,
        entries: [{ cidr: "10.0.0.0/24" }, { cidr: "10.0.0.66", nomatch: true, comment: "not him" }],
        digest: "z",
      });
      assert.deepEqual(out.items[1], { name: "blacklist", count: 0 });
      const one = await env.call("proxmox_list_firewall_ipsets", { name: "blacklist" });
      assert.equal(one.items.length, 1);
      assert.match(errorText(await env.callRaw("proxmox_list_firewall_ipsets", { name: "nope" })), /No IP set named nope/);
    });

    it("creates, updates and renames IP sets", async () => {
      env.mock.on("GET", /^\/cluster\/firewall\/ipset$/, () => [{ name: "management" }]);
      await env.call("proxmox_save_firewall_ipset", { name: "trusted", comment: "ok" });
      assert.deepEqual(env.mock.lastWrite()!.body, { name: "trusted", comment: "ok" });
      await env.call("proxmox_save_firewall_ipset", { name: "management", comment: "new comment" });
      assert.deepEqual(env.mock.lastWrite()!.body, { name: "management", rename: "management", comment: "new comment" });
      await env.call("proxmox_save_firewall_ipset", { name: "management", new_name: "mgmt" });
      assert.deepEqual(env.mock.lastWrite()!.body, { name: "mgmt", rename: "management" });
      assert.equal(env.mock.lastWrite()!.path, "/cluster/firewall/ipset");
      assert.match(errorText(await env.callRaw("proxmox_save_firewall_ipset", { name: "x", new_name: "y" })), /does not exist/);
    });

    it("deletes an IP set with force", async () => {
      await env.call("proxmox_delete_firewall_ipset", { vmid: 200, name: "ipfilter-net0", force: true });
      const w = env.mock.lastWrite()!;
      assert.deepEqual([w.method, w.path, w.query.force], ["DELETE", "/nodes/pve1/lxc/200/firewall/ipset/ipfilter-net0", "1"]);
    });

    it("adds new entries and updates existing ones", async () => {
      env.mock.on("GET", /^\/cluster\/firewall\/ipset\/management$/, () => [{ cidr: "10.0.0.0/24" }]);
      const added = await env.call("proxmox_save_ipset_entry", { ipset: "management", cidr: "192.168.1.0/24", comment: "vpn" });
      let w = env.mock.lastWrite()!;
      assert.deepEqual([w.method, w.path], ["POST", "/cluster/firewall/ipset/management"]);
      assert.deepEqual(w.body, { cidr: "192.168.1.0/24", comment: "vpn" });
      assert.equal(added.added, "192.168.1.0/24");

      const updated = await env.call("proxmox_save_ipset_entry", { ipset: "management", cidr: "10.0.0.0/24", nomatch: true });
      w = env.mock.lastWrite()!;
      assert.deepEqual([w.method, w.path], ["PUT", "/cluster/firewall/ipset/management/10.0.0.0%2F24"]);
      assert.deepEqual(w.body, { nomatch: "1" });
      assert.equal(updated.updated, "10.0.0.0/24");
    });

    it("removes an entry", async () => {
      await env.call("proxmox_delete_ipset_entry", { vmid: 100, ipset: "allowed", cidr: "10.1.0.0/16" });
      const w = env.mock.lastWrite()!;
      assert.deepEqual([w.method, w.path], ["DELETE", "/nodes/pve1/qemu/100/firewall/ipset/allowed/10.1.0.0%2F16"]);
    });
  });

  describe("security groups", () => {
    it("lists groups with rule counts", async () => {
      env.mock.on("GET", /^\/cluster\/firewall\/groups$/, () => [{ group: "webservers", comment: "http", digest: "g" }, { group: "empty" }]);
      env.mock.on("GET", /^\/cluster\/firewall\/groups\/webservers$/, () => [RULES[0], RULES[1]]);
      env.mock.on("GET", /^\/cluster\/firewall\/groups\/empty$/, () => []);
      const out = await env.call("proxmox_list_firewall_groups", {});
      assert.deepEqual(out.items, [
        { group: "webservers", comment: "http", rules: 2, enabled_rules: 1 },
        { group: "empty", rules: 0, enabled_rules: 0 },
      ]);
    });

    it("creates and renames groups", async () => {
      env.mock.on("GET", /^\/cluster\/firewall\/groups$/, () => [{ group: "web" }]);
      await env.call("proxmox_save_firewall_group", { group: "db", comment: "postgres" });
      assert.deepEqual(env.mock.lastWrite()!.body, { group: "db", comment: "postgres" });
      const out = await env.call("proxmox_save_firewall_group", { group: "web", new_name: "webservers" });
      assert.deepEqual(env.mock.lastWrite()!.body, { group: "webservers", rename: "web" });
      assert.equal(out.renamed_from, "web");
    });

    it("adds rules to a group via the rule tool and deletes groups", async () => {
      await env.call("proxmox_save_firewall_rule", { group: "web", type: "in", action: "ACCEPT", macro: "HTTPS", enable: true });
      let w = env.mock.lastWrite()!;
      assert.deepEqual([w.method, w.path], ["POST", "/cluster/firewall/groups/web"]);
      assert.equal(w.body.macro, "HTTPS");
      await env.call("proxmox_delete_firewall_group", { group: "web" });
      w = env.mock.lastWrite()!;
      assert.deepEqual([w.method, w.path], ["DELETE", "/cluster/firewall/groups/web"]);
    });
  });

  describe("log, refs and macros", () => {
    it("reads the guest firewall log with paging", async () => {
      env.mock.on("GET", /^\/nodes\/pve1\/qemu\/100\/firewall\/log$/, () => [
        { n: 1, t: "100 5 tap100i0-IN 11/Oct/2026:10:00:00 +0000 policy DROP: IN=fwbr100i0 SRC=10.0.0.9 DST=10.0.0.10 PROTO=TCP DPT=22" },
        { n: 2, t: "100 5 tap100i0-IN 11/Oct/2026:10:00:01 +0000 policy DROP: PROTO=UDP DPT=53" },
      ]);
      const out = await env.call("proxmox_get_firewall_log", { vmid: 100, limit: 20, since: 1700000000, search: "DPT=22" });
      assert.equal(out.returned, 1);
      assert.match(out.items[0], /DPT=22/);
      const req = env.mock.requestsTo("/nodes/pve1/qemu/100/firewall/log", "GET")[0];
      assert.deepEqual(req.query, { limit: "20", since: "1700000000" });
    });

    it("reads the node firewall log and refuses cluster scope", async () => {
      env.mock.on("GET", /^\/nodes\/pve1\/firewall\/log$/, () => []);
      const out = await env.call("proxmox_get_firewall_log", { node: "pve1" });
      assert.equal(out.scope, "node");
      assert.equal(env.mock.requestsTo("/nodes/pve1/firewall/log")[0].query.limit, "100");
      assert.match(errorText(await env.callRaw("proxmox_get_firewall_log", {})), /pass `node`/);
    });

    it("lists refs and macros", async () => {
      env.mock.on("GET", /^\/nodes\/pve1\/lxc\/200\/firewall\/refs$/, () => [{ type: "ipset", name: "management", ref: "+dc/management", scope: "dc" }]);
      const refs = await env.call("proxmox_list_firewall_refs", { vmid: 200, type: "ipset" });
      assert.deepEqual(refs.items, [{ ref: "+dc/management", type: "ipset", name: "management", scope: "dc" }]);
      assert.equal(env.mock.requestsTo("/nodes/pve1/lxc/200/firewall/refs")[0].query.type, "ipset");

      env.mock.on("GET", /^\/cluster\/firewall\/macros$/, () => [{ macro: "SSH", descr: "Secure shell traffic" }, { macro: "HTTP", descr: "Hypertext" }]);
      const macros = await env.call("proxmox_list_firewall_macros", { search: "shell" });
      assert.deepEqual(macros.items, [{ macro: "SSH", description: "Secure shell traffic" }]);
    });
  });

  describe("proxmox_get_guest_firewall_status", () => {
    it("reports everything enabled and the blocking policy", async () => {
      env.mock.on("GET", /^\/cluster\/firewall\/options$/, () => ({ enable: 1 }));
      env.mock.on("GET", /^\/nodes\/pve1\/firewall\/options$/, () => ({}));
      env.mock.on("GET", /^\/nodes\/pve1\/qemu\/100\/firewall\/options$/, () => ({ enable: 1, policy_in: "DROP" }));
      env.mock.on("GET", /^\/nodes\/pve1\/qemu\/100\/firewall\/rules$/, () => [{ pos: 0, type: "in", action: "DROP", enable: 0 }]);
      const out = await env.call("proxmox_get_guest_firewall_status", { vmid: 100 });
      assert.equal(out.filtering_active, true);
      assert.deepEqual(out.datacenter, { enabled: true, policy_in: "DROP", policy_out: "ACCEPT", policy_forward: "ACCEPT" });
      assert.equal(out.node_firewall.enabled, true);
      assert.equal(out.guest_firewall.enabled, true);
      assert.equal(out.guest_firewall.policy_in, "DROP");
      assert.deepEqual(out.nics, [{ nic: "net0", bridge: "vmbr0", tag: "10", firewall: true }]);
      assert.deepEqual(out.rules, { count: 1, enabled: 0, list: ["0: [disabled] IN DROP"] });
      assert.ok(out.findings.some((f: string) => /all incoming traffic is blocked/.test(f)));
      assert.ok(out.findings.some((f: string) => /1 rule\(s\) are disabled/.test(f)));
    });

    it("explains why a container is not filtered", async () => {
      env.mock.on("GET", /^\/cluster\/firewall\/options$/, () => ({}));
      env.mock.on("GET", /^\/nodes\/pve1\/firewall\/options$/, () => ({ enable: 0 }));
      env.mock.on("GET", /^\/nodes\/pve1\/lxc\/200\/firewall\/options$/, () => ({}));
      env.mock.on("GET", /^\/nodes\/pve1\/lxc\/200\/firewall\/rules$/, () => []);
      const out = await env.call("proxmox_get_guest_firewall_status", { vmid: 200 });
      assert.equal(out.filtering_active, false);
      assert.equal(out.type, "container");
      assert.equal(out.nics[0].firewall, false);
      assert.equal(out.nics[0].ip, "dhcp");
      const f = out.findings.join("\n");
      assert.match(f, /Datacenter firewall is disabled/);
      assert.match(f, /Guest firewall is disabled/);
      assert.match(f, /without firewall=1.*net0/);
      assert.match(f, /host firewall is disabled/);
    });

    it("tolerates unreadable parts", async () => {
      env.mock.on("GET", /^\/cluster\/firewall\/options$/, () => env.mock.error(403, "Permission check failed"));
      env.mock.on("GET", /^\/nodes\/pve1\/firewall\/options$/, () => ({}));
      env.mock.on("GET", /^\/nodes\/pve1\/qemu\/100\/firewall\/options$/, () => ({ enable: 1 }));
      const out = await env.call("proxmox_get_guest_firewall_status", { vmid: 100 });
      assert.match(out.datacenter.error, /Permission check failed/);
      assert.match(out.rules.error, /no mock/);
      assert.equal(out.filtering_active, false);
    });
  });
});
