package tools

import (
	"net/url"
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

// sdnTestLastQuery is the query of the last request to prefix with method.
func sdnTestLastQuery(t *testing.T, env *testEnv, prefix, method string) *Obj {
	t.Helper()
	reqs := env.mock.RequestsTo(prefix, method)
	if len(reqs) == 0 {
		t.Fatalf("no %s request to %s", method, prefix)
	}
	return reqs[len(reqs)-1].Query
}

func TestSdn(t *testing.T) {
	env := startEnv(t)
	run := func(name string, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			env.mock.Reset()
			f(t)
		})
	}

	run("lists zones with pending state and passes filters", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/sdn/zones$`, `[
			{"zone":"vxz","type":"vxlan","peers":"10.0.0.11,10.0.0.12","digest":"d1","state":"new","pending":{"mtu":1450}},
			{"zone":"lan","type":"vlan","bridge":"vmbr0","digest":"d2"}]`)
		res := env.call(t, "proxmox_list_sdn_objects", `{"kind":"zone","pending":true,"type":"vlan"}`)
		q := sdnTestLastQuery(t, env, "/cluster/sdn/zones", "GET")
		eqStr(t, q.Get("pending"), "1")
		eqStr(t, q.Get("type"), "vlan")
		eqAt(t, res, "total", 2)
		eq(t, pluck(at(res, "items"), "zone"), `["lan","vxz"]`)
		eqAt(t, res, "items.1.state", "new")
		eqAt(t, res, "items.1.pending", `{"mtu":1450}`)
		eqAt(t, res, "items.0.digest", js.Undef)
	})

	run("lists subnets of a vnet and requires the vnet", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/sdn/vnets/vnet1/subnets$`, `[{"subnet":"z1-10.0.0.0-24","cidr":"10.0.0.0/24","type":"subnet","gateway":"10.0.0.1"}]`)
		res := env.call(t, "proxmox_list_sdn_objects", `{"kind":"subnet","vnet":"vnet1"}`)
		eqStr(t, at(res, "items.0.gateway"), "10.0.0.1")
		match(t, env.errorText(t, "proxmox_list_sdn_objects", `{"kind":"subnet"}`), `vnet.*required`)
	})

	run("redacts IPAM tokens and ignores pending for ipams", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/sdn/ipams$`, `[{"ipam":"nb","type":"netbox","url":"https://nb","token":"s3cret"}]`)
		res := env.call(t, "proxmox_list_sdn_objects", `{"kind":"ipam","pending":true}`)
		eqAt(t, res, "items.0.token", "<redacted>")
		eqAt(t, sdnTestLastQuery(t, env, "/cluster/sdn/ipams", "GET"), "pending", js.Undef)
	})

	run("gets one object with pending and searches", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/sdn/vnets/vnet1$`, `{"vnet":"vnet1","zone":"lan","tag":20,"digest":"abc"}`)
		v := env.call(t, "proxmox_get_sdn_object", `{"kind":"vnet","id":"vnet1","pending":true}`)
		eqAt(t, v, "tag", 20)
		eqAt(t, v, "digest", "abc")
		eqStr(t, sdnTestLastQuery(t, env, "/cluster/sdn/vnets/vnet1", "GET").Get("pending"), "1")
	})

	run("redacts the DNS plugin key", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/sdn/dns/pdns$`, `{"dns":"pdns","type":"powerdns","key":"k3y","url":"http://pdns"}`)
		d := env.call(t, "proxmox_get_sdn_object", `{"kind":"dns","id":"pdns"}`)
		eqAt(t, d, "key", "<redacted>")
		eqAt(t, d, "url", "http://pdns")
	})

	run("translates subnet CIDRs to subnet IDs (<zone>-<ip>-<prefix>) in paths", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/sdn/vnets/vnet1$`, `{"vnet":"vnet1","zone":"z1"}`)
		env.mock.On("GET", `^/cluster/sdn/vnets/vnet1/subnets/(.+)$`, func(_ *Obj, m []string, _ proxmoxtest.Request) any {
			s, _ := url.PathUnescape(m[1])
			return js.O("subnet", s)
		})
		s := env.call(t, "proxmox_get_sdn_object", `{"kind":"subnet","vnet":"vnet1","id":"10.0.0.0/24"}`)
		eqAt(t, s, "subnet", "z1-10.0.0.0-24")
		if len(env.mock.RequestsTo("^/cluster/sdn/vnets/vnet1/subnets/z1-10.0.0.0-24$", "")) == 0 {
			t.Fatal("subnet path not translated")
		}
		env.call(t, "proxmox_delete_sdn_object", `{"kind":"subnet","vnet":"vnet1","id":"10.0.0.0/24"}`)
		eqStr(t, env.mock.LastWrite().Path, "/cluster/sdn/vnets/vnet1/subnets/z1-10.0.0.0-24")
	})

	run("reports zone and vnet status per node", func(t *testing.T) {
		env.mock.On("GET", `^/nodes/(pve\d)/sdn/zones$`, func(_ *Obj, m []string, _ proxmoxtest.Request) any {
			if m[1] == "pve1" {
				return js.MustParse(`[{"zone":"lan","status":"available"},{"zone":"vxz","status":"error"}]`)
			}
			return js.MustParse(`[{"zone":"lan","status":"available"}]`)
		})
		env.mock.On("GET", `^/nodes/(pve\d)/sdn/zones/(\w+)/content$`, func(_ *Obj, m []string, _ proxmoxtest.Request) any {
			return []any{js.O("vnet", m[2]+"v1", "status", "available")}
		})
		res := env.call(t, "proxmox_get_sdn_status", `{}`)
		eq(t, pluck(at(res, "nodes"), "node"), `["pve1","pve2"]`)
		pve1 := at(res, "nodes.0")
		eqAt(t, pve1, "not_available", `["vxz"]`)
		eqAt(t, pve1, "zones.0.vnets.0.vnet", "lanv1")

		one := env.call(t, "proxmox_get_sdn_status", `{"node":"pve2","zone":"lan","vnets":false}`)
		eqAt(t, one, "nodes", `[{"node":"pve2","zones":[{"zone":"lan","status":"available"}]}]`)
	})

	run("reports per-node errors in status without failing", func(t *testing.T) {
		env.mock.On("GET", `^/nodes/pve1/sdn/zones$`, func(*Obj, []string, proxmoxtest.Request) any { return proxmoxtest.Error(500, "boom") })
		res := env.call(t, "proxmox_get_sdn_status", `{"node":"pve1"}`)
		match(t, at(res, "nodes.0.error"), `boom`)
	})

	run("shows the dry-run diff", func(t *testing.T) {
		env.mock.On("GET", `^/cluster/sdn/dry-run$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			return js.O("interfaces-diff", "+auto vnet1 ("+js.Str(p.Get("node"))+")", "frr-diff", "")
		})
		res := env.call(t, "proxmox_get_sdn_pending_diff", `{"node":"pve1"}`)
		eqAt(t, res, "interfaces_diff", "+auto vnet1 (pve1)")
		eqAt(t, res, "frr_diff", js.Undef)
		env.mock.Reply("GET", `^/cluster/sdn/dry-run$`, `{}`)
		none := env.call(t, "proxmox_get_sdn_pending_diff", `{"node":"pve1"}`)
		match(t, at(none, "note"), `No differences`)
	})

	run("lists IPAM entries with filters", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/sdn/ipams/pve/status$`, `[
			{"zone":"z","vnet":"vnet1","subnet":"10.0.0.0/24","ip":"10.0.0.1","gateway":1},
			{"zone":"z","vnet":"vnet1","subnet":"10.0.0.0/24","ip":"10.0.0.5","mac":"BC:24:11:00:00:01","vmid":100,"hostname":"web"},
			{"zone":"z","vnet":"vnet2","subnet":"10.1.0.0/24","ip":"10.1.0.5","vmid":101}]`)
		all := env.call(t, "proxmox_get_sdn_ipam_status", `{}`)
		eqAt(t, all, "ipam", "pve")
		eqAt(t, all, "total", 3)
		eqAt(t, all, "items.0.gateway", true)
		vm := env.call(t, "proxmox_get_sdn_ipam_status", `{"vmid":100}`)
		eq(t, pluck(at(vm, "items"), "ip"), `["10.0.0.5"]`)
		vnet := env.call(t, "proxmox_get_sdn_ipam_status", `{"vnet":"vnet2"}`)
		eqAt(t, vnet, "total", 1)
	})

	run("lists fabrics and optional live node status", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/sdn/fabrics/all$`, `{"fabrics":[{"id":"f1","protocol":"openfabric"}],"nodes":[{"node_id":"pve1","fabric_id":"f1"}]}`)
		env.mock.On("GET", `^/nodes/pve1/sdn/fabrics/f1/(routes|neighbors|interfaces)$`, func(_ *Obj, m []string, _ proxmoxtest.Request) any {
			return []any{js.O("kind", m[1])}
		})
		res := env.call(t, "proxmox_list_sdn_fabrics", `{"node":"pve1","fabric":"f1"}`)
		eqAt(t, res, "fabrics.0.id", "f1")
		eqAt(t, res, "status.routes.0.kind", "routes")
		eqAt(t, res, "status.interfaces.0.kind", "interfaces")
		plain := env.call(t, "proxmox_list_sdn_fabrics", `{}`)
		eqAt(t, plain, "status", js.Undef)
	})

	run("lists prefix lists and groups route map entries", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/sdn/prefix-lists$`, `[{"id":"pl1","entries":["permit 10.0.0.0/8"],"digest":"x"}]`)
		env.mock.Reply("GET", `^/cluster/sdn/route-maps/entries$`, `[
			{"route-map-id":"rm1","order":20,"action":"deny"},
			{"route-map-id":"rm1","order":10,"action":"permit","match":["key=vni,value=100"]}]`)
		res := env.call(t, "proxmox_list_sdn_route_policies", `{}`)
		eqAt(t, res, "prefix_lists.0.id", "pl1")
		eqAt(t, res, "prefix_lists.0.digest", js.Undef)
		eqStr(t, sdnTestLastQuery(t, env, "/cluster/sdn/prefix-lists", "GET").Get("verbose"), "1")
		eq(t, pluck(at(res, "route_maps.rm1"), "order"), `[10,20]`)
	})

	run("degrades gracefully when route policies aren't supported", func(t *testing.T) {
		res := env.call(t, "proxmox_list_sdn_route_policies", `{}`)
		match(t, at(res, "prefix_lists.error"), `404`)
		match(t, at(res, "route_maps.error"), `404`)
	})

	run("creates a zone with mapped parameter names", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/sdn/zones/evpn1$`, `{"zone":"evpn1","type":"evpn","state":"new"}`)
		res := env.call(t, "proxmox_save_sdn_zone", `{"zone":"evpn1","create":true,"type":"evpn","controller":"ctl","vrf_vxlan":10000,"exitnodes":["pve1","pve2"],"advertise_subnets":true,"nodes":["pve1"],"extra":{"mac":"BC:24:11:00:00:AA"}}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, "/cluster/sdn/zones")
		eq(t, w.Body, `{"zone":"evpn1","type":"evpn","controller":"ctl","vrf-vxlan":"10000","exitnodes":"pve1,pve2","advertise-subnets":"1","nodes":"pve1","mac":"BC:24:11:00:00:AA"}`)
		eqAt(t, res, "action", "created")
		eqAt(t, res, "object.state", "new")
		match(t, at(res, "note"), `proxmox_apply_sdn`)
	})

	run("requires a type when creating a zone", func(t *testing.T) {
		match(t, env.errorText(t, "proxmox_save_sdn_zone", `{"zone":"z1","create":true}`), `type.*required`)
	})

	run("updates a zone without sending its type", func(t *testing.T) {
		env.call(t, "proxmox_save_sdn_zone", `{"zone":"lan","type":"vlan","mtu":9000,"delete":["dns"],"digest":"d","lock_token":"tok"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, "/cluster/sdn/zones/lan")
		eq(t, w.Body, `{"mtu":"9000","delete":"dns","digest":"d","lock-token":"tok"}`)
	})

	run("creates and updates vnets", func(t *testing.T) {
		match(t, env.errorText(t, "proxmox_save_sdn_vnet", `{"vnet":"v1","create":true}`), `zone.*required`)
		env.call(t, "proxmox_save_sdn_vnet", `{"vnet":"v1","create":true,"zone":"lan","tag":30,"vlanaware":false,"isolate_ports":true}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Path, "/cluster/sdn/vnets")
		eq(t, w.Body, `{"vnet":"v1","zone":"lan","tag":"30","vlanaware":"0","isolate-ports":"1"}`)
		env.call(t, "proxmox_save_sdn_vnet", `{"vnet":"v1","alias":"guests"}`)
		w = env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, "/cluster/sdn/vnets/v1")
		eq(t, w.Body, `{"alias":"guests"}`)
	})

	run("creates a subnet with DHCP ranges", func(t *testing.T) {
		env.call(t, "proxmox_save_sdn_subnet", `{"vnet":"v1","subnet":"10.0.0.0/24","create":true,"gateway":"10.0.0.1","snat":true,
			"dhcp_range":[{"start":"10.0.0.100","end":"10.0.0.150"},{"start":"10.0.0.200","end":"10.0.0.210"}]}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, "/cluster/sdn/vnets/v1/subnets")
		eq(t, w.Body, `{"subnet":"10.0.0.0/24","type":"subnet","gateway":"10.0.0.1","snat":"1","dhcp-range":["start-address=10.0.0.100,end-address=10.0.0.150","start-address=10.0.0.200,end-address=10.0.0.210"]}`)
	})

	run("updates a subnet without the create-only type", func(t *testing.T) {
		env.call(t, "proxmox_save_sdn_subnet", `{"vnet":"v1","subnet":"z-10.0.0.0-24","dhcp_dns_server":"10.0.0.53"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, "/cluster/sdn/vnets/v1/subnets/z-10.0.0.0-24")
		eq(t, w.Body, `{"dhcp-dns-server":"10.0.0.53"}`)
	})

	run("creates controllers, IPAMs and DNS plugins", func(t *testing.T) {
		env.call(t, "proxmox_save_sdn_controller", `{"controller":"evpnctl","create":true,"type":"evpn","asn":65000,"peers":["10.0.0.11","10.0.0.12"]}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Path, "/cluster/sdn/controllers")
		eq(t, w.Body, `{"controller":"evpnctl","type":"evpn","asn":"65000","peers":"10.0.0.11,10.0.0.12"}`)
		match(t, env.errorText(t, "proxmox_save_sdn_controller", `{"controller":"c","create":true}`), `type`)

		env.call(t, "proxmox_save_sdn_ipam", `{"ipam":"nb","create":true,"type":"netbox","url":"https://nb/api","token":"t0k"}`)
		w = env.mock.LastWrite()
		eqStr(t, w.Path, "/cluster/sdn/ipams")
		eq(t, w.Body, `{"ipam":"nb","type":"netbox","url":"https://nb/api","token":"t0k"}`)
		match(t, env.errorText(t, "proxmox_save_sdn_ipam", `{"ipam":"x","create":true}`), `type`)

		match(t, env.errorText(t, "proxmox_save_sdn_dns", `{"dns":"pdns","create":true,"type":"powerdns"}`), "url.*key")
		env.mock.Reply("GET", `^/cluster/sdn/dns/pdns$`, `{"dns":"pdns","type":"powerdns","url":"http://pdns","key":"k3y"}`)
		res := env.call(t, "proxmox_save_sdn_dns", `{"dns":"pdns","create":true,"type":"powerdns","url":"http://pdns","key":"k3y","ttl":300}`)
		w = env.mock.LastWrite()
		eqStr(t, w.Path, "/cluster/sdn/dns")
		eq(t, w.Body, `{"dns":"pdns","type":"powerdns","url":"http://pdns","key":"k3y","ttl":"300"}`)
		eqAt(t, res, "object.key", "<redacted>")
		// DNS objects don't support pending; the read-back mustn't ask for it.
		eqAt(t, sdnTestLastQuery(t, env, "/cluster/sdn/dns/pdns", "GET"), "pending", js.Undef)
	})

	run("updates an IPAM via PUT", func(t *testing.T) {
		env.call(t, "proxmox_save_sdn_ipam", `{"ipam":"nb","fingerprint":"AA:BB"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, "/cluster/sdn/ipams/nb")
		eq(t, w.Body, `{"fingerprint":"AA:BB"}`)
	})

	run("deletes SDN objects", func(t *testing.T) {
		res := env.call(t, "proxmox_delete_sdn_object", `{"kind":"vnet","id":"v1","lock_token":"tok"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eqStr(t, w.Path, "/cluster/sdn/vnets/v1")
		eqStr(t, w.Query.Get("lock-token"), "tok")
		eqAt(t, res, "deleted", "v1")
		env.call(t, "proxmox_delete_sdn_object", `{"kind":"controller","id":"c1"}`)
		eqStr(t, env.mock.LastWrite().Path, "/cluster/sdn/controllers/c1")
		env.call(t, "proxmox_delete_sdn_object", `{"kind":"subnet","id":"z-10.0.0.0-24","vnet":"v1"}`)
		eqStr(t, env.mock.LastWrite().Path, "/cluster/sdn/vnets/v1/subnets/z-10.0.0.0-24")
	})

	run("manages vnet IP mappings, looking up the zone", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/sdn/vnets/v1$`, `{"vnet":"v1","zone":"lan"}`)
		res := env.call(t, "proxmox_save_sdn_vnet_ip", `{"vnet":"v1","ip":"10.0.0.7","mac":"BC:24:11:00:00:07","create":true,"vmid":100}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, "/cluster/sdn/vnets/v1/ips")
		eq(t, w.Body, `{"zone":"lan","ip":"10.0.0.7","mac":"BC:24:11:00:00:07"}`)
		eqAt(t, res, "zone", "lan")

		env.call(t, "proxmox_save_sdn_vnet_ip", `{"vnet":"v1","zone":"other","ip":"10.0.0.7","vmid":100}`)
		w = env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eq(t, w.Body, `{"zone":"other","ip":"10.0.0.7","vmid":"100"}`)

		env.call(t, "proxmox_delete_sdn_vnet_ip", `{"vnet":"v1","ip":"10.0.0.7"}`)
		w = env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eq(t, w.Query, `{"zone":"lan","ip":"10.0.0.7"}`)
	})

	run("fails clearly when the vnet's zone can't be found", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/sdn/vnets/v9$`, `{"vnet":"v9"}`)
		match(t, env.errorText(t, "proxmox_delete_sdn_vnet_ip", `{"vnet":"v9","ip":"10.0.0.1"}`), "pass `zone`")
	})

	run("applies SDN as a task", func(t *testing.T) {
		upid := env.mock.Upid("pve1", "reloadnetworkall", "")
		env.mock.Reply("PUT", `^/cluster/sdn$`, upid)
		res := env.call(t, "proxmox_apply_sdn", `{}`)
		eqAt(t, res, "status", "ok")
		eqStr(t, at(res, "task.upid"), upid)
		eqStr(t, env.mock.LastWrite().Method, "PUT")

		started := env.call(t, "proxmox_apply_sdn", `{"wait":false,"lock_token":"tok","release_lock":false}`)
		eqAt(t, started, "status", "started")
		eq(t, env.mock.LastWrite().Body, `{"lock-token":"tok","release-lock":"0"}`)
	})

	run("reports a failed apply with the task log", func(t *testing.T) {
		upid := env.mock.Upid("pve1", "reloadnetworkall", "", proxmoxtest.Task{Status: "stopped", ExitStatus: "error during reload", Log: []string{"vnet1: failed"}})
		env.mock.Reply("PUT", `^/cluster/sdn$`, upid)
		text := env.errorText(t, "proxmox_apply_sdn", `{}`)
		match(t, text, `error during reload`)
		match(t, text, `vnet1: failed`)
	})

	run("rolls back pending changes", func(t *testing.T) {
		res := env.call(t, "proxmox_rollback_sdn", `{}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, "/cluster/sdn/rollback")
		eqAt(t, res, "rolled_back", true)
	})
}
