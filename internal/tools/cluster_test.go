package tools

import (
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

func TestClusterRedaction(t *testing.T) {
	env := startEnv(t)

	t.Run("hides proxy credentials in datacenter options", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/options$`, `{"keyboard":"en-gb","http_proxy":"http://bob:s3cret@proxy.lan:3128"}`)
		res := env.call(t, "proxmox_get_cluster_options", nil)
		eqAt(t, res, "http_proxy", "http://<redacted>@proxy.lan:3128")
		eqAt(t, res, "keyboard", "en-gb")
		env.mock.Reply("GET", `^/cluster/options$`, `{"http_proxy":"http://proxy.lan:3128"}`)
		eqAt(t, env.call(t, "proxmox_get_cluster_options", nil), "http_proxy", "http://proxy.lan:3128")
	})

	t.Run("hides metric server tokens and OpenTelemetry headers", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/metrics/server$`, `[
			{"id":"influx","type":"influxdb","server":"10.0.0.5","port":8086,"token":"abc"},
			{"id":"otel","type":"opentelemetry","server":"otel.lan","port":4318,"otel-headers":"eyJBdXRob3JpemF0aW9uIjoiQmVhcmVyIHgifQ=="}]`)
		res := env.call(t, "proxmox_list_metric_servers", nil)
		eqAt(t, res, "0.token", "<redacted>")
		eqAt(t, res, "1.otel-headers", "<redacted>")
		eqAt(t, res, "1.server", "otel.lan")
	})
}

func TestClusterTools(t *testing.T) {
	env := startEnv(t)

	t.Run("summarises the cluster", func(t *testing.T) {
		s := env.call(t, "proxmox_get_cluster_status", nil)
		eqAt(t, s, "cluster.name", "homelab")
		eqAt(t, s, "cluster.quorate", true)
		eqAt(t, s, "version", "9.0.10 (release 9.0)")
		eqAt(t, s, "nodes.length", 2)
		eqAt(t, s, "nodes.0.ip", "10.0.0.11")
		eqAt(t, s, "guests.vms", `{"running":1,"stopped":1,"templates":1}`)
		eqAt(t, s, "guests.containers", `{"running":1,"stopped":0,"templates":0}`)
		eqAt(t, s, "storage.length", 3)
	})

	t.Run("lists and filters guests", func(t *testing.T) {
		all := env.call(t, "proxmox_list_guests", nil)
		eqAt(t, all, "total", 4)
		eq(t, pluck(at(all, "items"), "vmid"), "[100,101,200,9000]")
		web := at(all, "items.0")
		eqAt(t, web, "type", "vm")
		eqAt(t, web, "tags", `["prod","web"]`)
		eqAt(t, web, "memory", "2 GiB of 4 GiB (50%)")
		cts := env.call(t, "proxmox_list_guests", `{"type":"container"}`)
		eq(t, pluck(at(cts, "items"), "name"), `["dns"]`)
		tagged := env.call(t, "proxmox_list_guests", `{"tag":"WEB","templates":"exclude"}`)
		eqAt(t, tagged, "total", 1)
		byMem := env.call(t, "proxmox_list_guests", `{"sort":"memory","status":"running"}`)
		eq(t, pluck(at(byMem, "items"), "vmid"), "[100,200]")
	})

	t.Run("checks free VMIDs", func(t *testing.T) {
		env.mock.On("GET", `^/cluster/nextid$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			if p.Get("vmid") == "100" {
				return proxmoxtest.Error(400, "VM 100 already exists")
			}
			return or(p.Get("vmid"), "102")
		})
		eq(t, env.call(t, "proxmox_get_next_vmid", nil), `{"vmid":102,"free":true}`)
		eqAt(t, env.call(t, "proxmox_get_next_vmid", `{"vmid":100}`), "free", false)
	})

	t.Run("runs bulk actions as a task", func(t *testing.T) {
		upid := env.mock.Upid("pve1", "startall", "")
		env.mock.Reply("POST", `^/cluster/bulk-action/guest/shutdown$`, upid)
		res := env.call(t, "proxmox_bulk_guest_action", `{"action":"shutdown","vmids":[100,200],"timeout":60}`)
		eqAt(t, res, "status", "ok")
		eq(t, env.mock.LastWrite().Body, `{"vms":["100","200"],"timeout":"60"}`)
	})

	t.Run("reports failed tasks as errors with the log tail", func(t *testing.T) {
		upid := env.mock.Upid("pve1", "startall", "", proxmoxtest.Task{Status: "stopped", ExitStatus: "command failed", Log: []string{"ERROR: no quorum"}})
		env.mock.Reply("POST", `^/cluster/bulk-action/guest/start$`, upid)
		text := env.errorText(t, "proxmox_bulk_guest_action", `{"action":"start","vmids":[101]}`)
		match(t, text, `failed: command failed`)
		match(t, text, `ERROR: no quorum`)
	})

	t.Run("returns the UPID immediately with wait=false", func(t *testing.T) {
		upid := env.mock.Upid("pve1", "startall", "", proxmoxtest.Task{Status: "running"})
		env.mock.Reply("POST", `^/cluster/bulk-action/guest/start$`, upid)
		res := env.call(t, "proxmox_bulk_guest_action", `{"action":"start","vmids":[101],"wait":false}`)
		eqAt(t, res, "upid", upid)
		eqAt(t, res, "status", "started")
	})

	t.Run("requires a target to migrate and refuses extra keys it sets itself", func(t *testing.T) {
		match(t, env.errorText(t, "proxmox_bulk_guest_action", `{"action":"migrate","vmids":[100]}`), `target is required for migrate`)
		match(t, env.errorText(t, "proxmox_bulk_guest_action", `{"action":"start","vmids":[100],"extra":{"vms":"100,101"}}`), "extra must not set `vms`")
		match(t, env.errorText(t, "proxmox_update_cluster_options", `{"keyboard":"de","extra":{"keyboard":"fr"}}`), "extra must not set `keyboard`")
	})

	t.Run("validates arguments like the original server", func(t *testing.T) {
		eqStr(t, env.errorText(t, "proxmox_list_guests", `{"type":"bogus","limit":1.5}`),
			"MCP error -32602: Input validation error: Invalid arguments for tool proxmox_list_guests: Invalid option: expected one of \"vm\"|\"container\" at type\nInvalid input: expected int, received number at limit")
		eqStr(t, env.errorText(t, "proxmox_bulk_guest_action", `{"action":"start","vmids":[]}`),
			"MCP error -32602: Input validation error: Invalid arguments for tool proxmox_bulk_guest_action: Too small: expected array to have >=1 items at vmids")
		eqStr(t, env.errorText(t, "proxmox_get_next_vmid", `{"vmid":"100"}`),
			"MCP error -32602: Input validation error: Invalid arguments for tool proxmox_get_next_vmid: Invalid input: expected number, received string at vmid")
	})

	t.Run("saves metric servers", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/metrics/server/influx$`, `{"id":"influx","type":"influxdb","token":"t"}`)
		match(t, env.errorText(t, "proxmox_save_metric_server", `{"id":"influx","create":true,"type":"influxdb"}`), `required when creating`)
		res := env.call(t, "proxmox_save_metric_server", `{"id":"influx","create":true,"type":"influxdb","server":"10.0.0.5","port":8086}`)
		eqAt(t, res, "server.token", "<redacted>")
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eq(t, w.Body, `{"type":"influxdb","server":"10.0.0.5","port":"8086"}`)
		env.call(t, "proxmox_save_metric_server", `{"id":"influx","type":"graphite","disable":true}`)
		w = env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eq(t, w.Body, `{"disable":"1"}`)
	})

	t.Run("reads the corosync config, reporting sections that fail", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/config/nodes$`, `[{"name":"pve1","nodeid":1}]`)
		env.mock.Reply("GET", `^/cluster/config/join$`, `{"nodelist":[],"totem":{},"config_digest":"x","preferred_node":"pve1"}`)
		res := env.call(t, "proxmox_get_cluster_config", nil)
		eqAt(t, res, "nodes.0.name", "pve1")
		match(t, at(res, "totem.error"), `HTTP 404`)
		eqAt(t, res, "qdevice", js.Undef)
	})
}

func TestTaskTools(t *testing.T) {
	env := startEnv(t)
	upid := env.mock.Upid("pve2", "vzdump", "101", proxmoxtest.Task{Status: "stopped", ExitStatus: "job errors", Log: []string{"a", "b", "c"}})
	env.mock.Reply("GET", `^/cluster/tasks$`, js.Array([]*Obj{
		js.O("upid", upid, "node", "pve2", "type", "vzdump", "id", "101", "user", "root@pam", "starttime", 1700000000, "endtime", 1700000100, "status", "job errors"),
		js.O("upid", "UPID:pve1:1:1:1:qmstart:100:root@pam:", "node", "pve1", "type", "qmstart", "id", "100", "user", "root@pam", "starttime", 1700000200, "endtime", 1700000201, "status", "OK"),
	}))

	t.Run("lists and inspects tasks", func(t *testing.T) {
		failed := env.call(t, "proxmox_list_tasks", `{"errors":true}`)
		eq(t, pluck(at(failed, "items"), "type"), `["vzdump"]`)
		eqAt(t, failed, "items.0.duration_s", 100)
		eqAt(t, failed, "items.0.starttime", "2023-11-14T22:13:20.000Z")
		all := env.call(t, "proxmox_list_tasks", nil)
		eq(t, pluck(at(all, "items"), "type"), `["qmstart","vzdump"]`)
		task := env.call(t, "proxmox_get_task", js.O("upid", upid, "log_lines", 2))
		eqAt(t, task, "ok", false)
		eqAt(t, task, "log", `["b","c"]`)
		log := env.call(t, "proxmox_get_task_log", js.O("upid", upid, "tail", true, "limit", 1))
		eqAt(t, log, "lines", `["c"]`)
		eqAt(t, log, "total", 3)
		eqAt(t, log, "start", 2)
	})

	t.Run("filters a node's history server-side", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/tasks$`, `[{"upid":"UPID:pve1:1:1:1:qmstart:100:root@pam:","type":"qmstart","starttime":1700000200}]`)
		res := env.call(t, "proxmox_list_tasks", `{"node":"pve1","vmid":100,"type":"qmstart","running":true}`)
		eqAt(t, res, "items.0.status", "running")
		eq(t, env.mock.Last().Query, `{"vmid":"100","typefilter":"qmstart","source":"active","limit":"50"}`)
	})

	t.Run("waits for and stops tasks", func(t *testing.T) {
		res := env.call(t, "proxmox_wait_for_task", js.O("upid", upid))
		eqAt(t, res, "status", "stopped")
		eqAt(t, res, "exitstatus", "job errors")
		eq(t, env.call(t, "proxmox_stop_task", js.O("upid", upid)), js.O("stopped", upid))
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eqStr(t, w.Path, "/nodes/pve2/tasks/"+js.EncodeURIComponent(upid))
		match(t, env.errorText(t, "proxmox_get_task", `{"upid":"nope"}`), `^Error: Not a task id \(UPID\): nope$`)
	})

	t.Run("reports tasks that are still running", func(t *testing.T) {
		running := env.mock.Upid("pve1", "vzdump", "100", proxmoxtest.Task{Status: "running", Log: []string{"INFO: starting"}})
		res := env.call(t, "proxmox_wait_for_task", js.O("upid", running, "timeout_s", 1))
		eqAt(t, res, "timedOut", true)
		eqAt(t, res, "ok", false)
		eqAt(t, res, "log", `["INFO: starting"]`)
	})
}
