package tools

import (
	"strings"
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

const firewallTestRules = `[
  {"pos":1,"type":"in","action":"DROP","enable":0,"source":"+dc/blacklist","digest":"d1"},
  {"pos":0,"type":"in","action":"ACCEPT","macro":"SSH","iface":"net0","source":"+dc/management","enable":1,"log":"info","comment":"admins","digest":"d1"},
  {"pos":2,"type":"group","action":"webservers","enable":1,"digest":"d1"}
]`

func firewallTestRule(i int) *Obj { return js.Objs(js.MustParse(firewallTestRules))[i] }

func TestFirewallRuleLine(t *testing.T) {
	t.Run("renders rules like the firewall config", func(t *testing.T) {
		eqStr(t, firewallRuleLine(firewallTestRule(1)), "IN SSH(ACCEPT) -i net0 -source +dc/management -log info # admins")
		eqStr(t, firewallRuleLine(firewallTestRule(0)), "[disabled] IN DROP -source +dc/blacklist")
		eqStr(t, firewallRuleLine(firewallTestRule(2)), "GROUP webservers")
		eqStr(t, firewallRuleLine(js.MustObj(`{"type":"out","action":"REJECT","proto":"tcp","dport":"25","dest":"10.0.0.0/8","log":"nolog"}`)), "OUT REJECT -dest 10.0.0.0/8 -p tcp -dport 25")
	})
}

func TestFirewallTools(t *testing.T) {
	env := startEnv(t)
	run := func(name string, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			env.mock.Reset()
			f(t)
		})
	}

	// ---------------------------------------------------------------- scopes

	run("lists cluster rules sorted, with summaries", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/firewall/rules$`, firewallTestRules)
		out := env.call(t, "proxmox_list_firewall_rules", `{"scope":"cluster"}`)
		eqAt(t, out, "scope", "cluster")
		eqAt(t, out, "total", 3)
		eqAt(t, out, "enabled", 2)
		eqAt(t, out, "digest", "d1")
		eq(t, pluck(at(out, "items"), "pos"), "[0,1,2]")
		eqAt(t, out, "items.0.rule", "IN SSH(ACCEPT) -i net0 -source +dc/management -log info # admins")
		eqAt(t, out, "items.0.enable", true)
		eqAt(t, out, "items.1.enable", false)
	})

	run("defaults to cluster scope and returns raw rules on request", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/firewall/rules$`, firewallTestRules)
		out := env.call(t, "proxmox_list_firewall_rules", `{"raw":true}`)
		eqAt(t, out, "scope", "cluster")
		eqAt(t, out, "items.0.digest", "d1")
	})

	run("infers node, guest and group scope from the arguments", func(t *testing.T) {
		env.mock.Reply("GET", `firewall/rules$|firewall/groups/[^/]+$`, `[]`)
		node := env.call(t, "proxmox_list_firewall_rules", `{"node":"pve2"}`)
		eq(t, []any{at(node, "scope"), at(node, "node")}, `["node","pve2"]`)
		vm := env.call(t, "proxmox_list_firewall_rules", `{"vmid":100}`)
		eq(t, []any{at(vm, "scope"), at(vm, "node"), at(vm, "type"), at(vm, "name")}, `["guest","pve1","vm","web"]`)
		ct := env.call(t, "proxmox_list_firewall_rules", `{"vmid":200}`)
		eqAt(t, ct, "type", "container")
		grp := env.call(t, "proxmox_list_firewall_rules", `{"group":"webservers"}`)
		eqAt(t, grp, "scope", "group")
		var paths []any
		for _, r := range env.mock.RequestsTo(`^.*firewall`, "GET") {
			paths = append(paths, r.Path)
		}
		eq(t, paths, `["/nodes/pve2/firewall/rules","/nodes/pve1/qemu/100/firewall/rules","/nodes/pve1/lxc/200/firewall/rules","/cluster/firewall/groups/webservers"]`)
	})

	run("rejects inconsistent or unsupported scopes", func(t *testing.T) {
		match(t, env.errorText(t, "proxmox_list_firewall_rules", `{"scope":"guest"}`), `vmid is required`)
		match(t, env.errorText(t, "proxmox_list_firewall_rules", `{"scope":"group"}`), `group is required`)
		match(t, env.errorText(t, "proxmox_list_firewall_rules", `{"scope":"cluster","vmid":100}`), `vmid only applies`)
		match(t, env.errorText(t, "proxmox_list_firewall_rules", `{"scope":"cluster","node":"pve1"}`), `node does not apply`)
		match(t, env.errorText(t, "proxmox_list_firewall_aliases", `{"scope":"node","node":"pve1"}`), `Invalid|scope`)
		// node scope without a node on a two-node cluster needs a node
		match(t, env.errorText(t, "proxmox_get_firewall_options", `{"scope":"node"}`), "pass `node`")
	})

	// ---------------------------------------------------------------- rules

	run("gets a single rule", func(t *testing.T) {
		r := firewallTestRule(1)
		r.Delete("pos")
		env.mock.Reply("GET", `^/nodes/pve1/qemu/100/firewall/rules/0$`, r)
		out := env.call(t, "proxmox_get_firewall_rule", `{"vmid":100,"pos":0}`)
		eqAt(t, out, "pos", 0)
		eqAt(t, out, "rule", "IN SSH(ACCEPT) -i net0 -source +dc/management -log info # admins")
		eqAt(t, out, "digest", "d1")
	})

	run("creates a rule with mapped field names", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/qemu/100/firewall/rules/3$`, `{"type":"in","action":"ACCEPT","proto":"icmp","icmp-type":"echo-request","enable":1}`)
		out := env.call(t, "proxmox_save_firewall_rule", `{"vmid":100,"type":"in","action":"ACCEPT","proto":"icmp","icmp_type":"echo-request","enable":true,"insert_at":3,"extra":{"ipversion":4}}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, "/nodes/pve1/qemu/100/firewall/rules")
		eq(t, w.Body, `{"type":"in","action":"ACCEPT","proto":"icmp","icmp-type":"echo-request","enable":"1","ipversion":"4","pos":"3"}`)
		eqAt(t, out, "created", true)
		eqAt(t, out, "pos", 3)
		eqAt(t, out, "rule.rule", "IN ACCEPT -p icmp -icmp-type echo-request")
	})

	run("requires type and action to create", func(t *testing.T) {
		match(t, env.errorText(t, "proxmox_save_firewall_rule", `{"action":"ACCEPT"}`), `type and action are required`)
		match(t, env.errorText(t, "proxmox_save_firewall_rule", `{"type":"in","action":"ACCEPT","moveto":2}`), `moveto needs pos`)
		eqStr(t, env.mock.LastWrite().Method, "")
	})

	run("updates fields and then moves the rule", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/firewall/groups/web/0$`, firewallTestRule(1))
		out := env.call(t, "proxmox_save_firewall_rule", `{"group":"web","pos":2,"enable":false,"delete":["log"],"moveto":0,"digest":"d1"}`)
		puts := env.mock.RequestsTo("/cluster/firewall/groups/web/2", "PUT")
		if len(puts) != 2 {
			t.Fatalf("%d PUTs", len(puts))
		}
		eq(t, puts[0].Body, `{"enable":"0","delete":"log","digest":"d1"}`)
		eq(t, puts[1].Body, `{"moveto":"0"}`)
		eqAt(t, out, "moved", `{"from":2,"to":0}`)
		eqAt(t, out, "pos", 0)
	})

	run("moves only when no fields are given", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/firewall/rules/1$`, firewallTestRule(0))
		env.call(t, "proxmox_save_firewall_rule", `{"node":"pve1","pos":4,"moveto":1}`)
		puts := env.mock.RequestsTo("/nodes/pve1/firewall/rules/4", "PUT")
		if len(puts) != 1 {
			t.Fatalf("%d PUTs", len(puts))
		}
		eq(t, puts[0].Body, `{"moveto":"1"}`)
		match(t, env.errorText(t, "proxmox_save_firewall_rule", `{"pos":1}`), `Nothing to change`)
	})

	run("deletes a rule", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/firewall/rules/1$`, firewallTestRule(0))
		out := env.call(t, "proxmox_delete_firewall_rule", `{"pos":1,"digest":"d1"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eqStr(t, w.Path, "/cluster/firewall/rules/1")
		eqAt(t, w.Query, "digest", "d1")
		eqAt(t, out, "rule", "[disabled] IN DROP -source +dc/blacklist")
	})

	// ---------------------------------------------------------------- options

	run("gets options with effective defaults", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/lxc/200/firewall/options$`, `{"enable":1,"policy_in":"ACCEPT","digest":"x"}`)
		out := env.call(t, "proxmox_get_firewall_options", `{"vmid":200}`)
		eqAt(t, out, "options", `{"enable":1,"policy_in":"ACCEPT"}`)
		eqAt(t, out, "effective.enable", true)
		eqAt(t, out, "effective.policy_in", "ACCEPT")
		eqAt(t, out, "effective.policy_out", "ACCEPT")
		eqAt(t, out, "effective.macfilter", true)
		eqAt(t, out, "digest", "x")
	})

	run("updates cluster options", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/firewall/options$`, `{"enable":1,"policy_in":"DROP"}`)
		out := env.call(t, "proxmox_update_firewall_options", `{"scope":"cluster","enable":true,"policy_forward":"DROP","log_ratelimit":"enable=1,rate=5/second"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, "/cluster/firewall/options")
		eq(t, w.Body, `{"enable":"1","policy_forward":"DROP","log_ratelimit":"enable=1,rate=5/second"}`)
		eqAt(t, out, "effective.enable", true)
		eqAt(t, out, "updated", `["enable","policy_forward","log_ratelimit"]`)
	})

	run("updates node options and rejects options from another level", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve2/firewall/options$`, `{}`)
		env.call(t, "proxmox_update_firewall_options", `{"node":"pve2","nftables":true,"protection_synflood":true,"nf_conntrack_max":524288,"delete":["log_level_in"]}`)
		eq(t, env.mock.LastWrite().Body, `{"nftables":"1","protection_synflood":"1","nf_conntrack_max":"524288","delete":"log_level_in"}`)
		match(t, env.errorText(t, "proxmox_update_firewall_options", `{"vmid":100,"nftables":true,"macfilter":false}`), `Not available at guest level: nftables`)
		match(t, env.errorText(t, "proxmox_update_firewall_options", `{"scope":"cluster"}`), `Nothing to change`)
	})

	// ---------------------------------------------------------------- aliases

	run("lists aliases", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/firewall/aliases$`, `[{"name":"nas","cidr":"10.0.0.5","digest":"a"},{"name":"lan","cidr":"10.0.0.0/24","comment":"home"}]`)
		out := env.call(t, "proxmox_list_firewall_aliases", `{"search":"home"}`)
		eqAt(t, out, "items", `[{"name":"lan","cidr":"10.0.0.0/24","comment":"home"}]`)
	})

	run("creates a missing alias and updates/renames an existing one", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/qemu/100/firewall/aliases$`, `[{"name":"nas","cidr":"10.0.0.5"}]`)
		env.call(t, "proxmox_save_firewall_alias", `{"vmid":100,"name":"db","cidr":"10.0.0.9","comment":"database"}`)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["POST","/nodes/pve1/qemu/100/firewall/aliases"]`)
		eq(t, w.Body, `{"name":"db","cidr":"10.0.0.9","comment":"database"}`)

		out := env.call(t, "proxmox_save_firewall_alias", `{"vmid":100,"name":"nas","new_name":"storage"}`)
		w = env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["PUT","/nodes/pve1/qemu/100/firewall/aliases/nas"]`)
		eq(t, w.Body, `{"cidr":"10.0.0.5","rename":"storage"}`)
		eqAt(t, out, "renamed_from", "nas")

		match(t, env.errorText(t, "proxmox_save_firewall_alias", `{"vmid":100,"name":"new"}`), `cidr is required`)
	})

	run("deletes an alias", func(t *testing.T) {
		env.call(t, "proxmox_delete_firewall_alias", `{"name":"nas"}`)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["DELETE","/cluster/firewall/aliases/nas"]`)
	})

	// ---------------------------------------------------------------- IP sets

	run("lists IP sets with their entries", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/firewall/ipset$`, `[{"name":"management","comment":"admins"},{"name":"blacklist"}]`)
		env.mock.Reply("GET", `^/cluster/firewall/ipset/management$`, `[{"cidr":"10.0.0.0/24","digest":"z"},{"cidr":"10.0.0.66","nomatch":1,"comment":"not him"}]`)
		env.mock.Reply("GET", `^/cluster/firewall/ipset/blacklist$`, `[]`)
		out := env.call(t, "proxmox_list_firewall_ipsets", `{}`)
		eqAt(t, out, "total", 2)
		eqAt(t, out, "items.0", `{"name":"management","comment":"admins","count":2,"entries":[{"cidr":"10.0.0.0/24"},{"cidr":"10.0.0.66","nomatch":true,"comment":"not him"}],"digest":"z"}`)
		eqAt(t, out, "items.1", `{"name":"blacklist","count":0}`)
		one := env.call(t, "proxmox_list_firewall_ipsets", `{"name":"blacklist"}`)
		eqAt(t, one, "items.length", 1)
		match(t, env.errorText(t, "proxmox_list_firewall_ipsets", `{"name":"nope"}`), `No IP set named nope`)
	})

	run("creates, updates and renames IP sets", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/firewall/ipset$`, `[{"name":"management"}]`)
		env.call(t, "proxmox_save_firewall_ipset", `{"name":"trusted","comment":"ok"}`)
		eq(t, env.mock.LastWrite().Body, `{"name":"trusted","comment":"ok"}`)
		env.call(t, "proxmox_save_firewall_ipset", `{"name":"management","comment":"new comment"}`)
		eq(t, env.mock.LastWrite().Body, `{"name":"management","rename":"management","comment":"new comment"}`)
		env.call(t, "proxmox_save_firewall_ipset", `{"name":"management","new_name":"mgmt"}`)
		eq(t, env.mock.LastWrite().Body, `{"name":"mgmt","rename":"management"}`)
		eqStr(t, env.mock.LastWrite().Path, "/cluster/firewall/ipset")
		match(t, env.errorText(t, "proxmox_save_firewall_ipset", `{"name":"x","new_name":"y"}`), `does not exist`)
	})

	run("deletes an IP set with force", func(t *testing.T) {
		env.call(t, "proxmox_delete_firewall_ipset", `{"vmid":200,"name":"ipfilter-net0","force":true}`)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path, w.Query.Get("force")}, `["DELETE","/nodes/pve1/lxc/200/firewall/ipset/ipfilter-net0","1"]`)
	})

	run("adds new entries and updates existing ones", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/firewall/ipset/management$`, `[{"cidr":"10.0.0.0/24"}]`)
		added := env.call(t, "proxmox_save_ipset_entry", `{"ipset":"management","cidr":"192.168.1.0/24","comment":"vpn"}`)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["POST","/cluster/firewall/ipset/management"]`)
		eq(t, w.Body, `{"cidr":"192.168.1.0/24","comment":"vpn"}`)
		eqAt(t, added, "added", "192.168.1.0/24")

		updated := env.call(t, "proxmox_save_ipset_entry", `{"ipset":"management","cidr":"10.0.0.0/24","nomatch":true}`)
		w = env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["PUT","/cluster/firewall/ipset/management/10.0.0.0%2F24"]`)
		eq(t, w.Body, `{"nomatch":"1"}`)
		eqAt(t, updated, "updated", "10.0.0.0/24")
	})

	run("removes an entry", func(t *testing.T) {
		env.call(t, "proxmox_delete_ipset_entry", `{"vmid":100,"ipset":"allowed","cidr":"10.1.0.0/16"}`)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["DELETE","/nodes/pve1/qemu/100/firewall/ipset/allowed/10.1.0.0%2F16"]`)
	})

	// ---------------------------------------------------------------- security groups

	run("lists groups with rule counts", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/firewall/groups$`, `[{"group":"webservers","comment":"http","digest":"g"},{"group":"empty"}]`)
		env.mock.Reply("GET", `^/cluster/firewall/groups/webservers$`, js.Array([]*Obj{firewallTestRule(0), firewallTestRule(1)}))
		env.mock.Reply("GET", `^/cluster/firewall/groups/empty$`, `[]`)
		out := env.call(t, "proxmox_list_firewall_groups", `{}`)
		eqAt(t, out, "items", `[{"group":"webservers","comment":"http","rules":2,"enabled_rules":1},{"group":"empty","rules":0,"enabled_rules":0}]`)
	})

	run("creates and renames groups", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/firewall/groups$`, `[{"group":"web"}]`)
		env.call(t, "proxmox_save_firewall_group", `{"group":"db","comment":"postgres"}`)
		eq(t, env.mock.LastWrite().Body, `{"group":"db","comment":"postgres"}`)
		out := env.call(t, "proxmox_save_firewall_group", `{"group":"web","new_name":"webservers"}`)
		eq(t, env.mock.LastWrite().Body, `{"group":"webservers","rename":"web"}`)
		eqAt(t, out, "renamed_from", "web")
	})

	run("adds rules to a group via the rule tool and deletes groups", func(t *testing.T) {
		env.call(t, "proxmox_save_firewall_rule", `{"group":"web","type":"in","action":"ACCEPT","macro":"HTTPS","enable":true}`)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["POST","/cluster/firewall/groups/web"]`)
		eqAt(t, w.Body, "macro", "HTTPS")
		env.call(t, "proxmox_delete_firewall_group", `{"group":"web"}`)
		w = env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["DELETE","/cluster/firewall/groups/web"]`)
	})

	// ---------------------------------------------------------------- log, refs and macros

	run("reads the guest firewall log with paging", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/qemu/100/firewall/log$`, `[
			{"n":1,"t":"100 5 tap100i0-IN 11/Oct/2026:10:00:00 +0000 policy DROP: IN=fwbr100i0 SRC=10.0.0.9 DST=10.0.0.10 PROTO=TCP DPT=22"},
			{"n":2,"t":"100 5 tap100i0-IN 11/Oct/2026:10:00:01 +0000 policy DROP: PROTO=UDP DPT=53"}]`)
		out := env.call(t, "proxmox_get_firewall_log", `{"vmid":100,"limit":20,"since":1700000000,"search":"DPT=22"}`)
		eqAt(t, out, "returned", 1)
		match(t, at(out, "items.0"), `DPT=22`)
		req := env.mock.RequestsTo("/nodes/pve1/qemu/100/firewall/log", "GET")[0]
		eq(t, req.Query, `{"limit":"20","since":"1700000000"}`)
	})

	run("reads the node firewall log and refuses cluster scope", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/firewall/log$`, `[]`)
		out := env.call(t, "proxmox_get_firewall_log", `{"node":"pve1"}`)
		eqAt(t, out, "scope", "node")
		eqStr(t, env.mock.RequestsTo("/nodes/pve1/firewall/log", "")[0].Query.Get("limit"), "100")
		match(t, env.errorText(t, "proxmox_get_firewall_log", `{}`), "pass `node`")
	})

	run("lists refs and macros", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/lxc/200/firewall/refs$`, `[{"type":"ipset","name":"management","ref":"+dc/management","scope":"dc"}]`)
		refs := env.call(t, "proxmox_list_firewall_refs", `{"vmid":200,"type":"ipset"}`)
		eqAt(t, refs, "items", `[{"ref":"+dc/management","type":"ipset","name":"management","scope":"dc"}]`)
		eqAt(t, env.mock.RequestsTo("/nodes/pve1/lxc/200/firewall/refs", "")[0].Query, "type", "ipset")

		env.mock.Reply("GET", `^/cluster/firewall/macros$`, `[{"macro":"SSH","descr":"Secure shell traffic"},{"macro":"HTTP","descr":"Hypertext"}]`)
		macros := env.call(t, "proxmox_list_firewall_macros", `{"search":"shell"}`)
		eqAt(t, macros, "items", `[{"macro":"SSH","description":"Secure shell traffic"}]`)
	})

	// ---------------------------------------------------------------- proxmox_get_guest_firewall_status

	run("reports everything enabled and the blocking policy", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/firewall/options$`, `{"enable":1}`)
		env.mock.Reply("GET", `^/nodes/pve1/firewall/options$`, `{}`)
		env.mock.Reply("GET", `^/nodes/pve1/qemu/100/firewall/options$`, `{"enable":1,"policy_in":"DROP"}`)
		env.mock.Reply("GET", `^/nodes/pve1/qemu/100/firewall/rules$`, `[{"pos":0,"type":"in","action":"DROP","enable":0}]`)
		out := env.call(t, "proxmox_get_guest_firewall_status", `{"vmid":100}`)
		eqAt(t, out, "filtering_active", true)
		eqAt(t, out, "datacenter", `{"enabled":true,"policy_in":"DROP","policy_out":"ACCEPT","policy_forward":"ACCEPT"}`)
		eqAt(t, out, "node_firewall.enabled", true)
		eqAt(t, out, "guest_firewall.enabled", true)
		eqAt(t, out, "guest_firewall.policy_in", "DROP")
		eqAt(t, out, "nics", `[{"nic":"net0","bridge":"vmbr0","tag":"10","firewall":true}]`)
		eqAt(t, out, "rules", `{"count":1,"enabled":0,"list":["0: [disabled] IN DROP"]}`)
		f := js.Str(at(out, "findings"))
		match(t, f, `all incoming traffic is blocked`)
		match(t, f, `1 rule\(s\) are disabled`)
	})

	run("explains why a container is not filtered", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/firewall/options$`, `{}`)
		env.mock.Reply("GET", `^/nodes/pve1/firewall/options$`, `{"enable":0}`)
		env.mock.Reply("GET", `^/nodes/pve1/lxc/200/firewall/options$`, `{}`)
		env.mock.Reply("GET", `^/nodes/pve1/lxc/200/firewall/rules$`, `[]`)
		out := env.call(t, "proxmox_get_guest_firewall_status", `{"vmid":200}`)
		eqAt(t, out, "filtering_active", false)
		eqAt(t, out, "type", "container")
		eqAt(t, out, "nics.0.firewall", false)
		eqAt(t, out, "nics.0.ip", "dhcp")
		var lines []string
		for _, l := range at(out, "findings").([]any) {
			lines = append(lines, js.Str(l))
		}
		f := strings.Join(lines, "\n")
		match(t, f, `Datacenter firewall is disabled`)
		match(t, f, `Guest firewall is disabled`)
		match(t, f, `without firewall=1.*net0`)
		match(t, f, `host firewall is disabled`)
	})

	run("tolerates unreadable parts", func(t *testing.T) {
		env.mock.On("GET", `^/cluster/firewall/options$`, func(*Obj, []string, proxmoxtest.Request) any {
			return proxmoxtest.Error(403, "Permission check failed")
		})
		env.mock.Reply("GET", `^/nodes/pve1/firewall/options$`, `{}`)
		env.mock.Reply("GET", `^/nodes/pve1/qemu/100/firewall/options$`, `{"enable":1}`)
		out := env.call(t, "proxmox_get_guest_firewall_status", `{"vmid":100}`)
		match(t, at(out, "datacenter.error"), `Permission check failed`)
		match(t, at(out, "rules.error"), `no mock`)
		eqAt(t, out, "filtering_active", false)
	})
}
