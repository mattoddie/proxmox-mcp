package tools

import (
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

const (
	haTestCurrent = `[
  {"id":"quorum","type":"quorum","node":"pve1","status":"OK","quorate":1},
  {"id":"master","type":"master","node":"pve1","status":"pve1 (active, Sat Oct 11 10:00:00 2026)","timestamp":1791700000},
  {"id":"lrm:pve1","type":"lrm","node":"pve1","status":"pve1 (active, Sat Oct 11 10:00:00 2026)","timestamp":1791700000},
  {"id":"lrm:pve2","type":"lrm","node":"pve2","status":"pve2 (idle, Sat Oct 11 10:00:00 2026)","timestamp":1791700000},
  {"id":"service:vm:100","type":"service","sid":"vm:100","node":"pve1","state":"started","crm_state":"started","request_state":"started","status":"vm:100 (pve1, started)"},
  {"id":"fencing","type":"fencing","node":"pve1","status":"armed","armed-state":"armed"}
]`
	haTestManager = `{
  "manager_status":{"master_node":"pve1","node_status":{"pve1":"online","pve2":"online"},"service_status":{"vm:100":{"node":"pve1","state":"started"}}},
  "lrm_status":{"pve1":{"mode":"active","state":"active","timestamp":1791700000},"pve2":{"mode":"active","state":"wait_for_agent_lock","timestamp":1791700000}},
  "quorum":{"node":"pve1","quorate":"1"}
}`
	haTestResources = `[
  {"sid":"vm:100","type":"vm","state":"started","max_restart":2,"max_relocate":1,"failback":1,"comment":"web","digest":"r"},
  {"sid":"ct:200","type":"ct","state":"stopped","group":"pref1","digest":"r"}
]`
	haTestRules = `[
  {"rule":"web-on-pve1","type":"node-affinity","resources":"vm:100","nodes":"pve1:2,pve2","strict":1,"digest":"x"},
  {"rule":"apart","type":"resource-affinity","resources":"vm:100,vm:101","affinity":"negative","disable":1,"comment":"spread"}
]`
)

func TestHaTools(t *testing.T) {
	env := startEnv(t)
	run := func(name string, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			env.mock.Reset()
			f(t)
		})
	}
	routes := func() {
		env.mock.Reply("GET", `^/cluster/ha/status/current$`, haTestCurrent)
		env.mock.Reply("GET", `^/cluster/ha/status/manager_status$`, haTestManager)
		env.mock.On("GET", `^/cluster/ha/resources$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			all := js.MustParse(haTestResources)
			if !p.Truthy("type") {
				return all
			}
			return js.Array(filterObjs(js.Objs(all), func(r *Obj) bool { return r.Get("type") == p.Get("type") }))
		})
	}
	lastUpdate := proxmox.ISO(1791700000 * 1000)

	run("summarizes HA status", func(t *testing.T) {
		routes()
		out := env.call(t, "proxmox_get_ha_status", nil)
		eqAt(t, out, "quorum", `{"quorate":true,"node":"pve1","status":"OK"}`)
		eqAt(t, out, "master.node", "pve1")
		eqStr(t, at(out, "master.last_update"), lastUpdate)
		eqAt(t, out, "fencing", `{"armed_state":"armed","status":"armed"}`)
		eqAt(t, out, "nodes.length", 2)
		eqAt(t, out, "nodes.1", js.O("node", "pve2", "crm_node_state", "online", "lrm_state", "wait_for_agent_lock", "lrm_mode", "active", "status", "pve2 (idle, Sat Oct 11 10:00:00 2026)", "last_update", lastUpdate))
		eqAt(t, out, "services", `[{"sid":"vm:100","node":"pve1","state":"started","crm_state":"started","request_state":"started","status":"vm:100 (pve1, started)"}]`)
		raw := env.call(t, "proxmox_get_ha_status", `{"raw":true}`)
		eqAt(t, raw, "manager_status.manager_status.master_node", "pve1")
	})

	run("notes when nothing is HA-managed and works without manager_status", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/ha/status/current$`, `[{"id":"quorum","type":"quorum","node":"pve1","status":"OK","quorate":1}]`)
		out := env.call(t, "proxmox_get_ha_status", nil)
		match(t, at(out, "note"), `No HA-managed resources`)
		eqAt(t, out, "quorum.quorate", true)
	})

	run("lists resources merged with status and guest names", func(t *testing.T) {
		routes()
		out := env.call(t, "proxmox_list_ha_resources", nil)
		eqAt(t, out, "total", 2)
		eqAt(t, out, "items.0", `{"sid":"vm:100","name":"web","type":"vm","state":"started","max_restart":2,"max_relocate":1,"failback":true,"comment":"web",
			"current":{"node":"pve1","state":"started","crm_state":"started","request_state":"started","status":"vm:100 (pve1, started)"}}`)
		eqAt(t, out, "items.1", `{"sid":"ct:200","name":"dns","type":"ct","state":"stopped","group":"pref1"}`)
		cts := env.call(t, "proxmox_list_ha_resources", `{"type":"ct"}`)
		eqAt(t, cts, "total", 1)
		reqs := env.mock.RequestsTo("/cluster/ha/resources", "")
		eqStr(t, reqs[len(reqs)-1].Query.Get("type"), "ct")
		search := env.call(t, "proxmox_list_ha_resources", `{"search":"dns"}`)
		eqAt(t, search, "items.0.sid", "ct:200")
	})

	run("gets a resource by VMID with its rules", func(t *testing.T) {
		routes()
		r := js.Objs(js.MustParse(haTestResources))[1]
		r.Delete("sid")
		env.mock.Reply("GET", `^/cluster/ha/resources/ct%3A200$`, r)
		env.mock.Reply("GET", `^/cluster/ha/rules$`, `[{"rule":"keep-dns","type":"node-affinity","resources":"ct:200","nodes":"pve1:2,pve2","strict":0}]`)
		out := env.call(t, "proxmox_get_ha_resource", `{"sid":200}`)
		eqAt(t, out, "sid", "ct:200")
		eqAt(t, out, "state", "stopped")
		eqAt(t, out, "digest", "r")
		eqAt(t, out, "rules", `[{"rule":"keep-dns","type":"node-affinity","resources":["ct:200"],"nodes":["pve1:2","pve2"],"strict":false}]`)
		eqStr(t, env.mock.RequestsTo("/cluster/ha/rules", "")[0].Query.Get("resource"), "ct:200")
	})

	run("rejects unknown VMIDs", func(t *testing.T) {
		match(t, env.errorText(t, "proxmox_get_ha_resource", `{"sid":555}`), `No VM or container with ID 555`)
	})

	run("creates a new HA resource", func(t *testing.T) {
		routes()
		env.mock.Reply("GET", `^/cluster/ha/resources/vm%3A101$`, `{"type":"vm","state":"started","max_restart":3}`)
		out := env.call(t, "proxmox_save_ha_resource", `{"sid":101,"max_restart":3,"failback":false,"auto_rebalance":false,"comment":"db"}`)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["POST","/cluster/ha/resources"]`)
		eq(t, w.Body, `{"sid":"vm:101","max_restart":"3","failback":"0","auto-rebalance":"0","comment":"db"}`)
		eqAt(t, out, "created", "vm:101")
		eqAt(t, out, "resource.max_restart", 3)
	})

	run("updates an existing HA resource", func(t *testing.T) {
		routes()
		env.mock.Reply("GET", `^/cluster/ha/resources/vm%3A100$`, js.Objs(js.MustParse(haTestResources))[0])
		out := env.call(t, "proxmox_save_ha_resource", `{"sid":"vm:100","state":"disabled","delete":["comment"],"digest":"r"}`)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["PUT","/cluster/ha/resources/vm%3A100"]`)
		eq(t, w.Body, `{"state":"disabled","delete":"comment","digest":"r"}`)
		eqAt(t, out, "updated", "vm:100")
		match(t, env.errorText(t, "proxmox_save_ha_resource", `{"sid":"vm:101","delete":["comment"]}`), `delete only applies`)
	})

	run("deletes an HA resource", func(t *testing.T) {
		env.call(t, "proxmox_delete_ha_resource", `{"sid":"ct:200","purge":false}`)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path, w.Query.Get("purge")}, `["DELETE","/cluster/ha/resources/ct%3A200","0"]`)
	})

	run("migrates and relocates", func(t *testing.T) {
		env.mock.Reply("POST", `^/cluster/ha/resources/vm%3A100/migrate$`, `{"sid":"vm:100","requested-node":"pve2","comigrated-resources":["vm:101"],"blocking-resources":[]}`)
		out := env.call(t, "proxmox_ha_migrate", `{"sid":100,"action":"migrate","node":"pve2"}`)
		eq(t, env.mock.LastWrite().Body, `{"node":"pve2"}`)
		eqAt(t, out, "comigrated_resources", `["vm:101"]`)
		eqAt(t, out, "blocking_resources", js.Undef)
		eqAt(t, out, "requested", true)

		// PVE 8 returns null
		rel := env.call(t, "proxmox_ha_migrate", `{"sid":"ct:200","action":"relocate","node":"pve2"}`)
		eqStr(t, env.mock.LastWrite().Path, "/cluster/ha/resources/ct%3A200/relocate")
		eqAt(t, rel, "action", "relocate")
	})

	// ---------------------------------------------------------------- groups

	groups := func() {
		env.mock.Reply("GET", `^/cluster/ha/groups$`, `[{"group":"pref1","nodes":"pve1:2,pve2:1","restricted":0,"nofailback":1,"type":"group","digest":"g"}]`)
	}

	run("groups/lists and gets groups", func(t *testing.T) {
		groups()
		env.mock.Reply("GET", `^/cluster/ha/groups/pref1$`, `{"nodes":"pve1:2,pve2:1","restricted":1,"digest":"g"}`)
		eq(t, env.call(t, "proxmox_list_ha_groups", nil), `{"total":1,"returned":1,"items":[{"group":"pref1","nodes":["pve1:2","pve2:1"],"restricted":false,"nofailback":true}]}`)
		eq(t, env.call(t, "proxmox_get_ha_group", `{"group":"pref1"}`), `{"group":"pref1","nodes":["pve1:2","pve2:1"],"restricted":true,"nofailback":false,"digest":"g"}`)
	})

	run("groups/creates and updates groups", func(t *testing.T) {
		groups()
		env.call(t, "proxmox_save_ha_group", `{"group":"pref2","nodes":["pve2:2","pve1"],"restricted":true}`)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["POST","/cluster/ha/groups"]`)
		eq(t, w.Body, `{"group":"pref2","nodes":"pve2:2,pve1","restricted":"1"}`)
		match(t, env.errorText(t, "proxmox_save_ha_group", `{"group":"pref3"}`), `nodes is required`)

		out := env.call(t, "proxmox_save_ha_group", `{"group":"pref1","nofailback":false,"delete":["comment"]}`)
		w = env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["PUT","/cluster/ha/groups/pref1"]`)
		eq(t, w.Body, `{"nofailback":"0","delete":"comment"}`)
		eqAt(t, out, "updated", "pref1")
	})

	run("groups/deletes a group", func(t *testing.T) {
		env.call(t, "proxmox_delete_ha_group", `{"group":"pref1"}`)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["DELETE","/cluster/ha/groups/pref1"]`)
	})

	// ---------------------------------------------------------------- rules

	rules := func() { env.mock.Reply("GET", `^/cluster/ha/rules$`, haTestRules) }

	run("rules/lists rules with filters", func(t *testing.T) {
		rules()
		out := env.call(t, "proxmox_list_ha_rules", `{"type":"resource-affinity","resource":100}`)
		eqAt(t, out, "total", 2)
		eqAt(t, out, "items.1", `{"rule":"apart","type":"resource-affinity","resources":["vm:100","vm:101"],"affinity":"negative","disabled":true,"comment":"spread"}`)
		eq(t, env.mock.RequestsTo("/cluster/ha/rules", "")[0].Query, `{"type":"resource-affinity","resource":"vm:100"}`)
	})

	run("rules/gets a rule", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/ha/rules/web-on-pve1$`, js.Objs(js.MustParse(haTestRules))[0])
		eq(t, env.call(t, "proxmox_get_ha_rule", `{"rule":"web-on-pve1"}`), `{"rule":"web-on-pve1","type":"node-affinity","resources":["vm:100"],"nodes":["pve1:2","pve2"],"strict":true,"digest":"x"}`)
	})

	run("rules/creates node- and resource-affinity rules", func(t *testing.T) {
		rules()
		env.mock.Reply("GET", `^/cluster/ha/rules/dns-pref$`, `{"type":"node-affinity","resources":"ct:200","nodes":"pve1"}`)
		out := env.call(t, "proxmox_save_ha_rule", `{"rule":"dns-pref","type":"node-affinity","resources":[200],"nodes":["pve1"],"strict":true}`)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["POST","/cluster/ha/rules"]`)
		eq(t, w.Body, `{"rule":"dns-pref","type":"node-affinity","resources":"ct:200","nodes":"pve1","strict":"1"}`)
		eqAt(t, out, "created", "dns-pref")
		eqAt(t, out, "rule.resources", `["ct:200"]`)

		env.call(t, "proxmox_save_ha_rule", `{"rule":"together","type":"resource-affinity","resources":["vm:100",101],"affinity":"positive"}`)
		eq(t, env.mock.LastWrite().Body, `{"rule":"together","type":"resource-affinity","resources":"vm:100,vm:101","affinity":"positive"}`)
	})

	run("rules/validates new rules", func(t *testing.T) {
		rules()
		match(t, env.errorText(t, "proxmox_save_ha_rule", `{"rule":"n","resources":["vm:100"]}`), `type is required`)
		match(t, env.errorText(t, "proxmox_save_ha_rule", `{"rule":"n","type":"node-affinity"}`), `resources is required`)
		match(t, env.errorText(t, "proxmox_save_ha_rule", `{"rule":"n","type":"node-affinity","resources":["vm:100"]}`), `nodes is required`)
		match(t, env.errorText(t, "proxmox_save_ha_rule", `{"rule":"n","type":"resource-affinity","resources":["vm:100"]}`), `affinity is required`)
		match(t, env.errorText(t, "proxmox_save_ha_rule", `{"rule":"apart","type":"node-affinity"}`), `type can't be changed`)
		eqStr(t, env.mock.LastWrite().Method, "")
	})

	run("rules/updates a rule, sending its existing type", func(t *testing.T) {
		rules()
		env.call(t, "proxmox_save_ha_rule", `{"rule":"apart","disable":false,"delete":["comment"],"digest":"x"}`)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["PUT","/cluster/ha/rules/apart"]`)
		eq(t, w.Body, `{"type":"resource-affinity","disable":"0","delete":"comment","digest":"x"}`)
	})

	run("rules/deletes a rule", func(t *testing.T) {
		env.call(t, "proxmox_delete_ha_rule", `{"rule":"apart"}`)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["DELETE","/cluster/ha/rules/apart"]`)
	})

	run("arms and disarms HA", func(t *testing.T) {
		match(t, env.errorText(t, "proxmox_set_ha_armed", `{"action":"disarm"}`), `resource_mode`)
		env.call(t, "proxmox_set_ha_armed", `{"action":"disarm","resource_mode":"freeze"}`)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["POST","/cluster/ha/status/disarm-ha"]`)
		eq(t, w.Body, `{"resource-mode":"freeze"}`)
		out := env.call(t, "proxmox_set_ha_armed", `{"action":"arm"}`)
		eqStr(t, env.mock.LastWrite().Path, "/cluster/ha/status/arm-ha")
		eqAt(t, out, "requested", "arm")
	})
}
