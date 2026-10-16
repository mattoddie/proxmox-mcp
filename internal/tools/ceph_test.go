package tools

import (
	"strings"
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

const cephTestStatus = `{
  "fsid": "f-1",
  "health": {
    "status": "HEALTH_WARN",
    "checks": {
      "OSD_DOWN": {"severity": "HEALTH_WARN", "summary": {"message": "1 osds down", "count": 1}, "detail": [{"message": "osd.2 is down"}]},
      "POOL_NO_REDUNDANCY": {"severity": "HEALTH_WARN", "summary": {"message": "1 pool(s) have no replicas configured"}, "muted": true}
    }
  },
  "quorum_names": ["pve1", "pve2"],
  "monmap": {"mons": [{"name": "pve1"}, {"name": "pve2"}, {"name": "pve3"}]},
  "mgrmap": {"available": true, "active_name": "pve1", "standbys": [{"name": "pve2"}]},
  "osdmap": {"num_osds": 3, "num_up_osds": 2, "num_in_osds": 3, "num_remapped_pgs": 0},
  "pgmap": {
    "num_pgs": 129,
    "pgs_by_state": [{"state_name": "active+clean", "count": 100}, {"state_name": "active+undersized+degraded", "count": 29}],
    "bytes_used": 1099511627776, "bytes_total": 4398046511104, "bytes_avail": 3298534883328, "data_bytes": 366503875925,
    "degraded_ratio": 0.123, "read_bytes_sec": 1048576, "write_op_per_sec": 12, "num_pools": 2
  }
}`

const cephTestOsdTree = `{
  "flags": "noout",
  "root": {"leaf": 0, "children": [{"id": -1, "name": "default", "type": "root", "children": [
    {"id": -3, "name": "pve1", "type": "host", "children": [
      {"id": 0, "name": "osd.0", "type": "osd", "status": "up", "in": 1, "device_class": "ssd", "bytes_used": 107374182400, "total_space": 1099511627776, "percent_used": 9.77, "crush_weight": 1, "osdtype": "bluestore", "version": "19.2.1", "apply_latency_ms": 1, "commit_latency_ms": 2},
      {"id": 1, "name": "osd.1", "type": "osd", "status": "up", "in": 1, "device_class": "hdd", "crush_weight": 4}]},
    {"id": -5, "name": "pve2", "type": "host", "children": [{"id": 2, "name": "osd.2", "type": "osd", "status": "down", "in": 0, "device_class": "hdd", "host": "pve2"}]}
  ]}]}
}`

func TestCephTools(t *testing.T) {
	env := startEnv(t)
	reset := func() {
		env.mock.Reset()
		env.mock.Reply("GET", `^/nodes/[^/]+/ceph/osd$`, cephTestOsdTree)
	}

	t.Run("summarizes Ceph status", func(t *testing.T) {
		reset()
		env.mock.Reply("GET", `^/cluster/ceph/status$`, cephTestStatus)
		env.mock.Reply("GET", `^/cluster/ceph/health-mute$`, `[{"code":"POOL_NO_REDUNDANCY","sticky":false}]`)
		s := env.call(t, "proxmox_get_ceph_status", nil)
		eqAt(t, s, "health", "HEALTH_WARN")
		eqAt(t, s, "checks.0", `{"code":"OSD_DOWN","severity":"HEALTH_WARN","summary":"1 osds down","count":1,"detail":["osd.2 is down"]}`)
		eqAt(t, s, "checks.1.muted", true)
		eqAt(t, s, "monitors", `{"total":3,"in_quorum":["pve1","pve2"],"out_of_quorum":["pve3"]}`)
		eqAt(t, s, "managers", `{"active":"pve1","available":true,"standbys":["pve2"]}`)
		eqAt(t, s, "osds", `{"total":3,"up":2,"in":3,"down":1}`)
		eqAt(t, s, "pgs.total", 129)
		eq(t, at(s, "pgs.states").(*Obj).Get("active+clean"), 100)
		eqAt(t, s, "pgs.degraded_pct", 12.3)
		eqAt(t, s, "usage.raw", "1 TiB of 4 TiB (25%)")
		eqAt(t, s, "io.read", "1 MiB/s")
		eqAt(t, s, "muted_checks", `[{"code":"POOL_NO_REDUNDANCY"}]`)

		raw := env.call(t, "proxmox_get_ceph_status", `{"raw":true}`)
		eqAt(t, raw, "fsid", "f-1")
	})

	t.Run("handles the pre-Quincy nested osdmap and falls back to the node status", func(t *testing.T) {
		reset()
		env.mock.On("GET", `^/cluster/ceph/status$`, func(*Obj, []string, proxmoxtest.Request) any { return proxmoxtest.Error(501, "not implemented") })
		env.mock.Reply("GET", `^/nodes/pve1/ceph/status$`, `{"health":{"status":"HEALTH_OK"},"osdmap":{"osdmap":{"num_osds":4,"num_up_osds":4,"num_in_osds":4}}}`)
		s := env.call(t, "proxmox_get_ceph_status", nil)
		eqAt(t, s, "health", "HEALTH_OK")
		eqAt(t, s, "osds", `{"total":4,"up":4,"in":4,"down":0}`)
	})

	t.Run("lists OSDs with host, state and usage, with filters", func(t *testing.T) {
		reset()
		res := env.call(t, "proxmox_list_ceph_osds", nil)
		if n := len(env.mock.RequestsTo("/nodes/pve1/ceph/osd", "GET")); n != 1 {
			t.Fatalf("queries the first online node: %d requests", n)
		}
		eqAt(t, res, "flags", "noout")
		eqAt(t, res, "summary", `{"total":3,"up":2,"in":2,"hosts":["pve1","pve2"]}`)
		osd0 := at(res, "items.0")
		eqAt(t, osd0, "host", "pve1")
		eqAt(t, osd0, "in", true)
		eqAt(t, osd0, "usage", "100 GiB of 1 TiB (9.8%)")
		eqAt(t, osd0, "used_pct", 9.8)
		eqAt(t, osd0, "latency_ms", `{"apply":1,"commit":2}`)
		eqAt(t, res, "items.2.host", "pve2")

		down := env.call(t, "proxmox_list_ceph_osds", `{"status":"down"}`)
		eq(t, pluck(at(down, "items"), "id"), `[2]`)
		out := env.call(t, "proxmox_list_ceph_osds", `{"status":"out"}`)
		eq(t, pluck(at(out, "items"), "id"), `[2]`)
		hdd := env.call(t, "proxmox_list_ceph_osds", `{"device_class":"hdd","host":"pve1"}`)
		eq(t, pluck(at(hdd, "items"), "name"), `["osd.1"]`)
	})

	t.Run("gets OSD details from the hosting node", func(t *testing.T) {
		reset()
		env.mock.Reply("GET", `^/nodes/pve2/ceph/osd/2/metadata$`, `{"osd":{"hostname":"pve2","osd_objectstore":"bluestore"}}`)
		env.mock.Reply("GET", `^/nodes/pve2/ceph/osd/2/lv-info$`, `{"lv_name":"osd-block-x","lv_size":100}`)
		res := env.call(t, "proxmox_get_ceph_osd", `{"osdid":2}`)
		eqAt(t, res, "node", "pve2")
		eqAt(t, res, "metadata.osd.osd_objectstore", "bluestore")
		eqAt(t, res, "lv_info.lv_name", "osd-block-x")
	})

	t.Run("fails clearly for unknown OSDs", func(t *testing.T) {
		reset()
		match(t, env.errorText(t, "proxmox_control_ceph_osd", `{"osdid":42,"action":"out"}`), `OSD 42 not found`)
	})

	t.Run("lists pools with replication, PGs and usage", func(t *testing.T) {
		reset()
		env.mock.Reply("GET", `^/nodes/pve1/ceph/pool$`, `[
			{"pool_name":"vm","pool":2,"type":"replicated","size":3,"min_size":2,"pg_num":64,"pg_num_final":128,"pg_autoscale_mode":"on","application_metadata":{"rbd":{}},"crush_rule":0,"crush_rule_name":"replicated_rule","bytes_used":1073741824,"percent_used":0.0512},
			{"pool_name":".mgr","pool":1,"type":"replicated","size":3,"min_size":2,"pg_num":1,"crush_rule":0,"application_metadata":{"mgr":{}}}]`)
		res := env.call(t, "proxmox_list_ceph_pools", nil)
		eq(t, pluck(at(res, "items"), "name"), `[".mgr","vm"]`)
		vm := at(res, "items.1")
		eqAt(t, vm, "application", "rbd")
		eqAt(t, vm, "crush_rule", "replicated_rule")
		eqAt(t, vm, "used", "1 GiB")
		eqAt(t, vm, "used_pct", 5.1)
		eqAt(t, vm, "pg_num_optimal", 128)
		eqAt(t, vm, "autoscale", "on")
	})

	t.Run("gets a pool with statistics", func(t *testing.T) {
		reset()
		env.mock.On("GET", `^/nodes/pve1/ceph/pool/vm/status$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			return js.O("name", "vm", "size", 3, "verbose", p.Get("verbose"))
		})
		eqStr(t, at(env.call(t, "proxmox_get_ceph_pool", `{"name":"vm"}`), "verbose"), "1")
	})

	t.Run("lists monitors, managers and MDS", func(t *testing.T) {
		reset()
		env.mock.Reply("GET", `^/nodes/pve1/ceph/mon$`, `[{"name":"pve2","host":"pve2","quorum":1,"rank":1,"ceph_version_short":"19.2.1","service":1},{"name":"pve1","quorum":0,"state":"stopped"}]`)
		env.mock.Reply("GET", `^/nodes/pve1/ceph/mgr$`, `[{"name":"pve1","state":"active"}]`)
		env.mock.On("GET", `^/nodes/pve1/ceph/mds$`, func(*Obj, []string, proxmoxtest.Request) any { return proxmoxtest.Error(500, "no mds") })
		res := env.call(t, "proxmox_list_ceph_services", nil)
		eqAt(t, res, "mon.0", `{"name":"pve1","state":"stopped","quorum":false}`)
		eqAt(t, res, "mon.1", `{"name":"pve2","host":"pve2","quorum":true,"rank":1,"version":"19.2.1","service_enabled":true}`)
		eqAt(t, res, "mgr.0.state", "active")
		match(t, at(res, "mds.error"), `no mds`)
		mgrOnly := env.call(t, "proxmox_list_ceph_services", `{"type":"mgr","node":"pve2"}`)
		eq(t, mgrOnly.(*Obj).Keys(), `["mgr"]`)
	})

	t.Run("lists CephFS", func(t *testing.T) {
		reset()
		env.mock.Reply("GET", `^/nodes/pve1/ceph/fs$`, `[{"name":"cephfs","metadata_pool":"cephfs_metadata","data_pool":"cephfs_data"}]`)
		eq(t, env.call(t, "proxmox_list_ceph_fs", nil), `[{"name":"cephfs","metadata_pool":"cephfs_metadata","data_pools":["cephfs_data"]}]`)
	})

	t.Run("gets flags", func(t *testing.T) {
		reset()
		env.mock.Reply("GET", `^/cluster/ceph/flags$`, `[
			{"name":"noout","value":true,"description":"OSDs will not automatically be marked out"},
			{"name":"noscrub","value":false,"description":"Scrubbing is disabled."}]`)
		res := env.call(t, "proxmox_get_ceph_flags", nil)
		eqAt(t, res, "set", `["noout"]`)
		eqAt(t, res, "flags.1.set", false)
	})

	t.Run("sets flags as a task and maps flag names", func(t *testing.T) {
		reset()
		upid := env.mock.Upid("pve1", "cephsetflags", "")
		env.mock.Reply("PUT", `^/cluster/ceph/flags$`, upid)
		env.mock.Reply("GET", `^/cluster/ceph/flags$`, `[{"name":"noout","value":true}]`)
		res := env.call(t, "proxmox_set_ceph_flags", `{"noout":true,"nodeep_scrub":false}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Path, "/cluster/ceph/flags")
		eq(t, w.Body, `{"noout":"1","nodeep-scrub":"0"}`)
		eqAt(t, res, "status", "ok")
		eqAt(t, res, "now_set", `["noout"]`)
		match(t, env.errorText(t, "proxmox_set_ceph_flags", nil), `at least one flag`)
	})

	t.Run("reads the CRUSH map and rules", func(t *testing.T) {
		reset()
		env.mock.Reply("GET", `^/nodes/pve1/ceph/rules$`, `[{"name":"replicated_rule"},{"name":"ssd_only"}]`)
		env.mock.Reply("GET", `^/nodes/pve1/ceph/crush$`, "# begin crush map\nrule replicated_rule {}")
		res := env.call(t, "proxmox_get_ceph_crush", nil)
		eqAt(t, res, "rules", `["replicated_rule","ssd_only"]`)
		match(t, at(res, "crush_map"), `begin crush map`)
		rules := env.call(t, "proxmox_get_ceph_crush", `{"rules_only":true}`)
		eqAt(t, rules, "crush_map", js.Undef)
	})

	t.Run("reads the Ceph config DB and file", func(t *testing.T) {
		reset()
		env.mock.Reply("GET", `^/nodes/pve1/ceph/cfg/db$`, `[
			{"section":"global","name":"osd_pool_default_size","value":"3","level":"basic","can_update_at_runtime":true},
			{"section":"mon","name":"auth_allow_insecure_global_id_reclaim","value":"false"},
			{"section":"mgr","name":"mgr/dashboard/password","value":"dashpw"},
			{"section":"client.rgw","name":"rgw_keystone_admin_password","value":"kspw"}]`)
		env.mock.Reply("GET", `^/nodes/pve1/ceph/cfg/raw$`, js.Quote("[global]\n\tfsid = f-1\n[client.x]\n\tkey = AQBsecret==\n"))
		res := env.call(t, "proxmox_get_ceph_config", `{"search":"pool"}`)
		eqAt(t, res, "db.length", 1)
		eqAt(t, res, "db.0.runtime", true)
		match(t, at(res, "file"), `fsid = f-1`)
		noMatch(t, at(res, "file"), `AQBsecret`)
		dbOnly := env.call(t, "proxmox_get_ceph_config", `{"source":"db"}`)
		eqAt(t, dbOnly, "file", js.Undef)
		eqAt(t, dbOnly, "db.length", 4)
		if s := js.Stringify(dbOnly, ""); strings.Contains(s, "dashpw") || strings.Contains(s, "kspw") {
			t.Fatalf("secrets leaked: %s", s)
		}
		eqStr(t, at(dbOnly, "db.0.value"), "3")
	})

	t.Run("reads the Ceph log", func(t *testing.T) {
		reset()
		env.mock.On("GET", `^/nodes/pve2/ceph/log$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			return []any{
				js.O("n", 1, "t", "limit="+js.Str(p.Get("limit"))+" start="+js.Str(p.Get("start"))),
				js.O("n", 2, "t", "osd.2 marked down"),
			}
		})
		res := env.call(t, "proxmox_get_ceph_log", `{"node":"pve2","start":10,"limit":2}`)
		eqAt(t, res, "items", `["limit=2 start=10","osd.2 marked down"]`)
		found := env.call(t, "proxmox_get_ceph_log", `{"node":"pve2","search":"down"}`)
		eqAt(t, found, "items", `["osd.2 marked down"]`)
	})

	t.Run("summarizes daemon metadata", func(t *testing.T) {
		reset()
		env.mock.Reply("GET", `^/cluster/ceph/metadata$`, `{
			"mon": {"pve1": {"hostname":"pve1","ceph_version_short":"19.2.1","ceph_release":"squid","mem_total_kb":1024}},
			"osd": {"0": {"hostname":"pve1","ceph_version_short":"18.2.4"}}}`)
		res := env.call(t, "proxmox_get_ceph_metadata", nil)
		eqAt(t, res, "mon", `[{"id":"pve1","host":"pve1","version":"19.2.1","release":"squid","memory":"1 MiB"}]`)
		eqAt(t, res, "osd.0.version", "18.2.4")
	})

	t.Run("checks command safety", func(t *testing.T) {
		reset()
		env.mock.On("GET", `^/nodes/pve1/ceph/cmd-safety$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			return js.O("safe", false, "status", js.Str(p.Get("service"))+"."+js.Str(p.Get("id"))+" "+js.Str(p.Get("action"))+": 12 pgs would be inactive")
		})
		res := env.call(t, "proxmox_check_ceph_safety", `{"service":"osd","id":"1","action":"destroy"}`)
		eqAt(t, res, "safe", false)
		match(t, at(res, "status"), `osd.1 destroy`)
	})

	t.Run("marks OSDs in/out and scrubs them on their host", func(t *testing.T) {
		reset()
		env.call(t, "proxmox_control_ceph_osd", `{"osdid":2,"action":"out"}`)
		eqStr(t, env.mock.LastWrite().Path, "/nodes/pve2/ceph/osd/2/out")
		env.call(t, "proxmox_control_ceph_osd", `{"osdid":0,"action":"in","node":"pve1"}`)
		eqStr(t, env.mock.LastWrite().Path, "/nodes/pve1/ceph/osd/0/in")
		env.call(t, "proxmox_control_ceph_osd", `{"osdid":0,"action":"deep-scrub"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Path, "/nodes/pve1/ceph/osd/0/scrub")
		eq(t, w.Body, `{"deep":"1"}`)
	})

	t.Run("creates and destroys OSDs as tasks", func(t *testing.T) {
		reset()
		upid := env.mock.Upid("pve1", "cephcreateosd", "sdb")
		env.mock.Reply("POST", `^/nodes/pve1/ceph/osd$`, upid)
		res := env.call(t, "proxmox_create_ceph_osd", `{"node":"pve1","dev":"/dev/sdb","device_class":"ssd","db_dev":"/dev/nvme0n1","db_dev_size":60,"encrypted":true}`)
		eq(t, env.mock.LastWrite().Body, `{"dev":"/dev/sdb","crush-device-class":"ssd","db_dev":"/dev/nvme0n1","db_dev_size":"60","encrypted":"1"}`)
		eqAt(t, res, "status", "ok")

		del := env.mock.Upid("pve2", "cephdestroyosd", "2")
		env.mock.Reply("DELETE", `^/nodes/pve2/ceph/osd/2$`, del)
		d := env.call(t, "proxmox_delete_ceph_osd", `{"osdid":2,"cleanup":true,"wait":false}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eqStr(t, at(w.Query, "cleanup"), "1")
		eqAt(t, d, "status", "started")
		eqAt(t, d, "node", "pve2")
	})

	t.Run("surfaces a failed OSD creation", func(t *testing.T) {
		reset()
		upid := env.mock.Upid("pve1", "cephcreateosd", "sdb", proxmoxtest.Task{Status: "stopped", ExitStatus: "device is in use", Log: []string{"sdb has partitions"}})
		env.mock.Reply("POST", `^/nodes/pve1/ceph/osd$`, upid)
		match(t, env.errorText(t, "proxmox_create_ceph_osd", `{"node":"pve1","dev":"/dev/sdb"}`), `device is in use`)
	})

	t.Run("creates, updates and deletes pools", func(t *testing.T) {
		reset()
		upid := env.mock.Upid("pve1", "cephcreatepool", "vm2")
		env.mock.Reply("POST", `^/nodes/pve1/ceph/pool$`, upid)
		env.call(t, "proxmox_create_ceph_pool", `{"name":"vm2","size":2,"min_size":1,"pg_autoscale_mode":"on","add_storages":true,"erasure_coding":"k=2,m=1"}`)
		eq(t, env.mock.LastWrite().Body, `{"name":"vm2","size":"2","min_size":"1","pg_autoscale_mode":"on","add_storages":"1","erasure-coding":"k=2,m=1"}`)

		put := env.mock.Upid("pve1", "cephsetpool", "vm2")
		env.mock.Reply("PUT", `^/nodes/pve1/ceph/pool/vm2$`, put)
		u := env.call(t, "proxmox_update_ceph_pool", `{"name":"vm2","pg_num":128,"crush_rule":"ssd_only"}`)
		eq(t, env.mock.LastWrite().Body, `{"pg_num":"128","crush_rule":"ssd_only"}`)
		eqAt(t, u, "changed", `["pg_num","crush_rule"]`)
		match(t, env.errorText(t, "proxmox_update_ceph_pool", `{"name":"vm2"}`), `Nothing to change`)

		env.call(t, "proxmox_delete_ceph_pool", `{"name":"vm2","remove_storages":true}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eqStr(t, w.Path, "/nodes/pve1/ceph/pool/vm2")
		eq(t, w.Query, `{"remove_storages":"1"}`)
	})

	t.Run("creates and destroys monitors, managers and MDS", func(t *testing.T) {
		reset()
		env.call(t, "proxmox_create_ceph_service", `{"type":"mon","node":"pve2","mon_address":["10.0.0.12"],"hotstandby":true}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Path, "/nodes/pve2/ceph/mon/pve2")
		eq(t, w.Body, `{"mon-address":"10.0.0.12"}`)
		env.call(t, "proxmox_create_ceph_service", `{"type":"mds","node":"pve1","id":"pve1-b","hotstandby":true}`)
		w = env.mock.LastWrite()
		eqStr(t, w.Path, "/nodes/pve1/ceph/mds/pve1-b")
		eq(t, w.Body, `{"hotstandby":"1"}`)
		env.call(t, "proxmox_delete_ceph_service", `{"type":"mgr","node":"pve2","id":"pve2"}`)
		w = env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eqStr(t, w.Path, "/nodes/pve2/ceph/mgr/pve2")
	})

	t.Run("creates and destroys CephFS", func(t *testing.T) {
		reset()
		res := env.call(t, "proxmox_create_ceph_fs", `{"node":"pve1","add_storage":true}`)
		eqStr(t, env.mock.LastWrite().Path, "/nodes/pve1/ceph/fs/cephfs")
		eq(t, env.mock.LastWrite().Body, `{"add-storage":"1"}`)
		eqAt(t, res, "fs", "cephfs")
		env.call(t, "proxmox_delete_ceph_fs", `{"node":"pve1","name":"cephfs","remove_pools":true}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eq(t, w.Query, `{"remove-pools":"1"}`)
	})

	t.Run("mutes and unmutes health checks", func(t *testing.T) {
		reset()
		env.call(t, "proxmox_mute_ceph_health_check", `{"code":"OSD_NEARFULL","ttl":"2h"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Path, "/cluster/ceph/health-mute/OSD_NEARFULL")
		eq(t, w.Body, `{"value":"1","ttl":"2h"}`)
		env.call(t, "proxmox_mute_ceph_health_check", `{"code":"OSD_NEARFULL","mute":false,"ttl":"2h"}`)
		eq(t, env.mock.LastWrite().Body, `{"value":"0"}`)
	})

	t.Run("starts, stops and restarts services as tasks", func(t *testing.T) {
		reset()
		upid := env.mock.Upid("pve1", "srvrestart", "osd.0")
		env.mock.Reply("POST", `^/nodes/pve1/ceph/restart$`, upid)
		res := env.call(t, "proxmox_control_ceph_service", `{"action":"restart","node":"pve1","service":"osd.0"}`)
		eq(t, env.mock.LastWrite().Body, `{"service":"osd.0"}`)
		eqAt(t, res, "status", "ok")
		// systemd unit names are translated to the <type>.<id> form the API accepts
		env.call(t, "proxmox_control_ceph_service", `{"action":"restart","node":"pve1","service":"ceph-mon@pve1.service"}`)
		eq(t, env.mock.LastWrite().Body, `{"service":"mon.pve1"}`)
		stop := env.call(t, "proxmox_control_ceph_service", `{"action":"stop","node":"pve2"}`)
		eqStr(t, env.mock.LastWrite().Path, "/nodes/pve2/ceph/stop")
		eqAt(t, stop, "service", "ceph.target")
	})

	t.Run("initialises the Ceph config", func(t *testing.T) {
		reset()
		env.call(t, "proxmox_init_ceph", `{"node":"pve1","network":"10.10.0.0/24","cluster_network":"10.20.0.0/24"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Path, "/nodes/pve1/ceph/init")
		eq(t, w.Body, `{"network":"10.10.0.0/24","cluster-network":"10.20.0.0/24"}`)
	})

	t.Run("uses PROXMOX_DEFAULT_NODE for cluster-wide reads", func(t *testing.T) {
		env2 := startEnv(t, map[string]string{"PROXMOX_DEFAULT_NODE": "pve2"})
		env2.mock.Reply("GET", `^/nodes/[^/]+/ceph/osd$`, cephTestOsdTree)
		env2.call(t, "proxmox_list_ceph_osds", nil)
		if n := len(env2.mock.RequestsTo("/nodes/pve2/ceph/osd", "GET")); n != 1 {
			t.Fatalf("%d requests to pve2", n)
		}
	})
}
