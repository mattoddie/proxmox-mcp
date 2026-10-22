package tools

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

const nodesN = "/nodes/pve1"

// nodesRun runs a subtest against a freshly reset mock.
func nodesRun(t *testing.T, env *testEnv, name string, f func(t *testing.T)) {
	t.Run(name, func(t *testing.T) {
		env.mock.Reset()
		f(t)
	})
}

func nodesNotLeaked(t *testing.T, v any, secret string) {
	t.Helper()
	if strings.Contains(js.Stringify(v, ""), secret) {
		t.Fatalf("%q leaked in %s", secret, js.Stringify(v, ""))
	}
}

func TestNodesStatusAndStatistics(t *testing.T) {
	env := startEnv(t)

	nodesRun(t, env, "summarizes node status with version and subscription", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/status$`, `{
			"cpu":0.25,"wait":0.01,"loadavg":["0.50","0.40","0.30"],"uptime":90061,
			"cpuinfo":{"model":"AMD EPYC","sockets":1,"cores":8,"cpus":16,"mhz":"3000.000","flags":"fpu vme ..."},
			"memory":{"total":68719476736,"used":17179869184,"free":1,"available":51539607552},
			"swap":{"total":8589934592,"used":0,"free":8589934592},
			"rootfs":{"total":107374182400,"used":10737418240,"avail":1},
			"ksm":{"shared":1048576},
			"current-kernel":{"sysname":"Linux","release":"6.14.8-2-pve","machine":"x86_64","version":"#1 SMP"},
			"pveversion":"pve-manager/9.0.10/deadbeef",
			"boot-info":{"mode":"efi","secureboot":1}}`)
		env.mock.Reply("GET", `^/nodes/pve1/version$`, `{"version":"9.0.10","release":"9.0","repoid":"deadbeef"}`)
		env.mock.Reply("GET", `^/nodes/pve1/subscription$`, `{"status":"active","level":"c","key":"pve1c-secretkey","nextduedate":"2027-01-01","serverid":"ABC"}`)
		s := env.call(t, "proxmox_get_node_status", `{"node":"pve1"}`)
		eqAt(t, s, "cpu_pct", 25)
		eqAt(t, s, "cpu", `{"model":"AMD EPYC","sockets":1,"cores":8,"threads":16,"mhz":3000}`)
		eqAt(t, s, "memory", "16 GiB of 64 GiB (25%)")
		eqAt(t, s, "root_fs", "10 GiB of 100 GiB (10%)")
		eqAt(t, s, "kernel", "Linux 6.14.8-2-pve (x86_64)")
		eqAt(t, s, "uptime", "1d 1h 1m")
		eqAt(t, s, "ksm_shared", "1 MiB")
		eqAt(t, s, "boot", `{"mode":"efi","secure_boot":1}`)
		eqAt(t, s, "version", "9.0.10 (release 9.0, deadbeef)")
		eqAt(t, s, "subscription.status", "active")
		eqAt(t, s, "subscription.key_set", true)
		nodesNotLeaked(t, s, "secretkey")
	})

	nodesRun(t, env, "requires a node on multi-node clusters", func(t *testing.T) {
		match(t, env.errorText(t, "proxmox_get_node_status", nil), "pass `node`")
	})

	nodesRun(t, env, "summarizes RRD data", func(t *testing.T) {
		var rows []any
		for i := 0; i < 60; i++ {
			cpu := 0.3
			if i < 30 {
				cpu = 0.1
			}
			rows = append(rows, js.O("time", 1700000000+i*60, "cpu", cpu, "memused", 1073741824, "netin", 1024, "loadavg", 1))
		}
		env.mock.Reply("GET", `^/nodes/pve1/rrddata$`, rows)
		s := env.call(t, "proxmox_get_node_stats", `{"node":"pve1","timeframe":"day","points":4}`)
		eqStr(t, env.mock.RequestsTo(nodesN+"/rrddata", "")[0].Query.Get("timeframe"), "day")
		eqAt(t, s, "samples", 60)
		eqAt(t, s, "metrics.cpu_pct", `{"min":10,"avg":20,"max":30,"last":30}`)
		eqAt(t, s, "metrics.memused.max", "1 GiB")
		eqAt(t, s, "metrics.netin.avg", "1 KiB/s")
		eqAt(t, s, "series.length", 4)
		eqAt(t, s, "series.0.cpu_pct", 10)
		eqAt(t, s, "series.3.cpu_pct", 30)
		eqAt(t, s, "cf", "AVERAGE")
		eqAt(t, s, "from", "2023-11-14T22:13:20.000Z")
	})

	t.Run("summarizeRrd restricts metrics and handles empty data", func(t *testing.T) {
		out := nodesSummarizeRrd([]any{js.O("time", 1, "cpu", 0.5, "memused", 10)}, []string{"memused"}, 5)
		eq(t, out.ObjAt("metrics").Keys(), `["memused"]`)
		eq(t, nodesSummarizeRrd([]any{}, nil, 5), `{"samples":0}`)
	})

	nodesRun(t, env, "reads netstat and the report", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/netstat$`, `[{"dev":"tap100i0","vmid":"100","in":"2048","out":"1024"},{"dev":"veth200i0","vmid":"200","in":"10","out":"20"}]`)
		eq(t, env.call(t, "proxmox_get_node_netstat", `{"node":"pve1","vmid":100}`), `[{"vmid":100,"dev":"tap100i0","in":"2 KiB","out":"1 KiB"}]`)

		env.mock.Reply("GET", `^/nodes/pve1/report$`, js.V(strings.Repeat("x", 100)))
		r := env.call(t, "proxmox_get_node_report", `{"node":"pve1","max_chars":10}`)
		eqAt(t, r, "text.length", 10)
		eqAt(t, r, "truncated", true)
		eqAt(t, r, "total_chars", 100)

		env.mock.Reply("GET", `^/nodes/pve1/query-url-metadata$`, `{"filename":"debian.iso","mimetype":"application/octet-stream","size":1073741824}`)
		m := env.call(t, "proxmox_query_url_metadata", `{"node":"pve1","url":"https://x/debian.iso","verify_certificates":false}`)
		eqAt(t, m, "size", "1 GiB")
		eqAt(t, m, "filename", "debian.iso")
		eq(t, env.mock.RequestsTo(nodesN+"/query-url-metadata", "")[0].Query, `{"url":"https://x/debian.iso","verify-certificates":"0"}`)
		// The node fetches whatever URL it is given, so the tool needs writes.
		if toolNames(t, connect(t, env.client, toolOptions("read")))["proxmox_query_url_metadata"] {
			t.Fatal("proxmox_query_url_metadata is registered read-only")
		}
	})
}

func TestNodesServices(t *testing.T) {
	env := startEnv(t)

	nodesRun(t, env, "lists services with failed ones first", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/services$`, `[
			{"service":"chrony","name":"chrony","desc":"time","active-state":"active","state":"running","unit-state":"enabled"},
			{"service":"pveproxy","name":"pveproxy","desc":"API","active-state":"failed","state":"failed","unit-state":"enabled"}]`)
		s := env.call(t, "proxmox_list_node_services", `{"node":"pve1"}`)
		eqAt(t, s, "failed", `["pveproxy"]`)
		eqAt(t, s, "items.0.service", "pveproxy")
		eqAt(t, s, "items.1.active", "active")
		eqAt(t, s, "total", 2)
	})

	nodesRun(t, env, "restarts a service and waits for the task", func(t *testing.T) {
		upid := env.mock.Upid("pve1", "srvrestart", "pveproxy")
		env.mock.Reply("POST", `^/nodes/pve1/services/pveproxy/restart$`, upid)
		env.mock.Reply("GET", `^/nodes/pve1/services/pveproxy/state$`, `{"active-state":"active","state":"running","unit-state":"enabled"}`)
		r := env.call(t, "proxmox_manage_node_service", `{"node":"pve1","service":"pveproxy","action":"restart"}`)
		eqAt(t, r, "status", "ok")
		eqAt(t, r, "state.active", "active")
		eqStr(t, env.mock.LastWrite().Path, nodesN+"/services/pveproxy/restart")
	})

	nodesRun(t, env, "returns the UPID when not waiting", func(t *testing.T) {
		upid := env.mock.Upid("pve1", "srvstop", "sshd")
		env.mock.Reply("POST", `^/nodes/pve1/services/sshd/stop$`, upid)
		r := env.call(t, "proxmox_manage_node_service", `{"node":"pve1","service":"sshd","action":"stop","wait":false}`)
		eqAt(t, r, "upid", upid)
		eqAt(t, r, "status", "started")
	})
}

func TestNodesNetwork(t *testing.T) {
	env := startEnv(t)

	nodesRun(t, env, "lists interfaces and flags inactive ones", func(t *testing.T) {
		env.mock.On("GET", `^/nodes/pve1/network$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			if p.Get("type") != "any_bridge" {
				return proxmoxtest.Error(400, "wrong type")
			}
			return js.MustParse(`[
				{"iface":"vmbr1","type":"bridge","autostart":1,"bridge_ports":"eno2","cidr":"10.1.0.1/24","comments":"lab\n"},
				{"iface":"vmbr0","type":"bridge","active":1,"autostart":1,"bridge_ports":"eno1","address":"10.0.0.11","netmask":"24","gateway":"10.0.0.1","bridge_vlan_aware":1}]`)
		})
		r := env.call(t, "proxmox_list_node_network", `{"node":"pve1","type":"any_bridge"}`)
		eqAt(t, r, "inactive_or_pending", `["vmbr1"]`)
		eqAt(t, r, "items.0.iface", "vmbr0")
		eqAt(t, r, "items.0.cidr", "10.0.0.11/24")
		eqAt(t, r, "items.0.bridge_vlan_aware", true)
		eqAt(t, r, "items.1.comments", "lab")
		eqAt(t, r, "items.1.active", false)
		match(t, at(r, "note"), `pending`)
	})

	t.Run("sorts interface names numerically", func(t *testing.T) {
		names := []*Obj{js.O("iface", "vmbr10"), js.O("iface", "vmbr2"), js.O("iface", "eno1"), js.O("iface", "vmbr0.20"), js.O("iface", "vmbr0")}
		sortObjs(names, func(x, y *Obj) float64 { return nodesNaturalCompare(js.Str(x.Get("iface")), js.Str(y.Get("iface"))) })
		eq(t, pluck(js.Array(names), "iface"), `["eno1","vmbr0","vmbr0.20","vmbr2","vmbr10"]`)
	})

	nodesRun(t, env, "reports pending changes", func(t *testing.T) {
		env.mock.On("GET", `^/nodes/pve1/network$`, func(*Obj, []string, proxmoxtest.Request) any {
			return proxmoxtest.WithTotal{Data: []any{js.O("iface", "vmbr0", "type", "bridge", "active", 1)}, Total: 1}
		})
		r := env.call(t, "proxmox_list_node_network", `{"node":"pve1"}`)
		eqAt(t, r, "total", 1)
		eqAt(t, r, "pending_changes", js.Undef)
		eqAt(t, r, "note", js.Undef)
	})

	nodesRun(t, env, "gets one interface", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/network/bond0$`, `{"type":"bond","slaves":"eno1 eno2","bond_mode":"802.3ad","active":1}`)
		eq(t, env.call(t, "proxmox_get_node_network_interface", `{"node":"pve1","iface":"bond0"}`), `{"iface":"bond0","type":"bond","active":true,"slaves":"eno1 eno2","bond_mode":"802.3ad"}`)
	})

	nodesRun(t, env, "creates an interface with mapped parameter names", func(t *testing.T) {
		r := env.call(t, "proxmox_save_node_network_interface", `{"node":"pve1","iface":"vmbr2","create":true,"type":"bridge","cidr":"10.2.0.1/24","bridge_ports":["eno3","eno4"],"bridge_vlan_aware":true,"mtu":9000,"autostart":true,"bridge_vids":"2-100"}`)
		eqAt(t, r, "created", true)
		eqAt(t, r, "pending", true)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, nodesN+"/network")
		eq(t, w.Body, `{"iface":"vmbr2","type":"bridge","cidr":"10.2.0.1/24","bridge_ports":"eno3 eno4","bridge_vlan_aware":"1","mtu":"9000","autostart":"1","bridge_vids":"2-100"}`)
	})

	nodesRun(t, env, "updates an interface, keeping its type and sending deletes", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/network/vmbr0\.20$`, `{"type":"vlan"}`)
		r := env.call(t, "proxmox_save_node_network_interface", `{"node":"pve1","iface":"vmbr0.20","vlan_raw_device":"vmbr0","vlan_id":20,"delete":["gateway","mtu"]}`)
		eqAt(t, r, "updated", true)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, nodesN+"/network/vmbr0.20")
		eq(t, w.Body, `{"type":"vlan","vlan-raw-device":"vmbr0","vlan-id":"20","delete":"gateway,mtu"}`)
	})

	nodesRun(t, env, "requires a type when creating", func(t *testing.T) {
		match(t, env.errorText(t, "proxmox_save_node_network_interface", `{"node":"pve1","iface":"x","create":true}`), `type is required`)
	})

	nodesRun(t, env, "refuses extra keys that the typed arguments set", func(t *testing.T) {
		match(t, env.errorText(t, "proxmox_save_node_network_interface", `{"node":"pve1","iface":"x","create":true,"type":"bridge","extra":{"type":"bond"}}`), "extra must not set `type`")
		match(t, env.errorText(t, "proxmox_update_node_config", `{"node":"pve1","extra":{"delete":"acme"}}`), "extra must not set `delete`")
		if n := len(env.mock.Writes()); n != 0 {
			t.Fatalf("%d writes reached the API", n)
		}
	})

	nodesRun(t, env, "deletes, applies and reverts", func(t *testing.T) {
		env.call(t, "proxmox_delete_node_network_interface", `{"node":"pve1","iface":"vmbr2"}`)
		eq(t, []any{env.mock.LastWrite().Method, env.mock.LastWrite().Path}, []any{"DELETE", nodesN + "/network/vmbr2"})

		upid := env.mock.Upid("pve1", "srvreload", "networking")
		env.mock.Reply("PUT", `^/nodes/pve1/network$`, upid)
		r := env.call(t, "proxmox_apply_node_network", `{"node":"pve1"}`)
		eqAt(t, r, "status", "ok")
		eqStr(t, env.mock.LastWrite().Method, "PUT")

		env.call(t, "proxmox_revert_node_network", `{"node":"pve1"}`)
		eq(t, []any{env.mock.LastWrite().Method, env.mock.LastWrite().Path}, []any{"DELETE", nodesN + "/network"})
	})

	nodesRun(t, env, "reports a failed apply with the task log", func(t *testing.T) {
		upid := env.mock.Upid("pve1", "srvreload", "networking", proxmoxtest.Task{Status: "stopped", ExitStatus: "command failed", Log: []string{"error: vmbr9: bad"}})
		env.mock.Reply("PUT", `^/nodes/pve1/network$`, upid)
		match(t, env.errorText(t, "proxmox_apply_node_network", `{"node":"pve1"}`), `vmbr9: bad`)
	})
}

func TestNodesDNSHostsTime(t *testing.T) {
	env := startEnv(t)

	nodesRun(t, env, "gets and merges DNS settings", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/dns$`, `{"search":"lan","dns1":"1.1.1.1","dns2":"8.8.8.8"}`)
		eqAt(t, env.call(t, "proxmox_get_node_dns", `{"node":"pve1"}`), "dns1", "1.1.1.1")
		env.call(t, "proxmox_set_node_dns", `{"node":"pve1","dns1":"9.9.9.9"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eq(t, w.Body, `{"search":"lan","dns1":"9.9.9.9","dns2":"8.8.8.8"}`)
	})

	nodesRun(t, env, "reads and writes /etc/hosts", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/hosts$`, `{"data":"127.0.0.1 localhost\n","digest":"d1"}`)
		eq(t, env.call(t, "proxmox_get_node_hosts", `{"node":"pve1"}`), `{"node":"pve1","digest":"d1","content":"127.0.0.1 localhost\n"}`)
		env.call(t, "proxmox_set_node_hosts", `{"node":"pve1","content":"x","digest":"d1"}`)
		eq(t, env.mock.LastWrite().Body, `{"data":"x","digest":"d1"}`)
		eqStr(t, env.mock.LastWrite().Method, "POST")
	})

	nodesRun(t, env, "reads time and sets the time zone", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/time$`, `{"time":1700000000,"localtime":1700003600,"timezone":"Europe/Paris"}`)
		tm := env.call(t, "proxmox_get_node_time", `{"node":"pve1"}`)
		eqAt(t, tm, "timezone", "Europe/Paris")
		eqAt(t, tm, "utc_offset_hours", 1)
		eqAt(t, tm, "utc", "2023-11-14T22:13:20.000Z")
		env.call(t, "proxmox_set_node_timezone", `{"node":"pve1","timezone":"UTC"}`)
		eq(t, env.mock.LastWrite().Body, `{"timezone":"UTC"}`)
	})
}

func TestNodesConfigAndPower(t *testing.T) {
	env := startEnv(t)

	nodesRun(t, env, "parses node config", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/config$`, `{"description":"rack 1","wakeonlan":"aa:bb:cc:dd:ee:ff","acme":"account=default","acmedomain0":"pve1.example.com,plugin=cf","digest":"abc"}`)
		cfg := env.call(t, "proxmox_get_node_config", `{"node":"pve1"}`)
		eqAt(t, cfg, "acmedomain0", `{"domain":"pve1.example.com","plugin":"cf"}`)
		eqAt(t, cfg, "wakeonlan", `{"mac":"aa:bb:cc:dd:ee:ff"}`)
		eqAt(t, cfg, "acme", `{"account":"default"}`)
	})

	nodesRun(t, env, "updates node config, replacing ACME domains", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/config$`, `{"acmedomain0":"old.example.com","acmedomain1":"old2.example.com","digest":"abc"}`)
		env.call(t, "proxmox_update_node_config", `{"node":"pve1","description":"notes","startall_onboot_delay":30,"acme_account":"le","acme_domains":[{"domain":"pve1.example.com","plugin":"cf"}],"digest":"abc"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, nodesN+"/config")
		eq(t, w.Body, `{"description":"notes","startall-onboot-delay":"30","acme":"account=le","digest":"abc","acmedomain0":"pve1.example.com,plugin=cf","delete":"acmedomain1"}`)
	})

	nodesRun(t, env, "reboots, wakes and runs bulk actions", func(t *testing.T) {
		env.call(t, "proxmox_node_power", `{"node":"pve2","command":"reboot"}`)
		eqStr(t, env.mock.LastWrite().Path, "/nodes/pve2/status")
		eq(t, env.mock.LastWrite().Body, `{"command":"reboot"}`)

		env.mock.Reply("POST", `^/nodes/pve2/wakeonlan$`, "aa:bb:cc:dd:ee:ff")
		eqAt(t, env.call(t, "proxmox_wake_node", `{"node":"pve2"}`), "mac", "aa:bb:cc:dd:ee:ff")

		upid := env.mock.Upid("pve1", "migrateall", "")
		env.mock.Reply("POST", `^/nodes/pve1/migrateall$`, upid)
		r := env.call(t, "proxmox_node_bulk_action", `{"node":"pve1","action":"migrate","target":"pve2","vmids":[100,200],"max_workers":2,"with_local_disks":true,"force":true}`)
		eqAt(t, r, "status", "ok")
		eq(t, env.mock.LastWrite().Body, `{"vms":"100,200","max-workers":"2","target":"pve2","with-local-disks":"1"}`)

		env.call(t, "proxmox_node_bulk_action", `{"node":"pve1","action":"stop","force_stop":false,"timeout":60,"wait":false}`)
		eqStr(t, env.mock.LastWrite().Path, nodesN+"/stopall")
		eq(t, env.mock.LastWrite().Body, `{"force-stop":"0","timeout":"60"}`)

		match(t, env.errorText(t, "proxmox_node_bulk_action", `{"node":"pve1","action":"migrate"}`), `target is required`)
	})
}

func TestNodesLogs(t *testing.T) {
	env := startEnv(t)

	nodesRun(t, env, "reads syslog with filters", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/syslog$`, `[{"n":1,"t":"Jan 1 pveproxy[1]: started"},{"n":2,"t":"Jan 1 pveproxy[1]: error x"}]`)
		r := env.call(t, "proxmox_get_node_syslog", `{"node":"pve1","service":"pveproxy","since":"2026-01-01 00:00:00","limit":10,"search":"error"}`)
		eqAt(t, r, "lines", `["Jan 1 pveproxy[1]: error x"]`)
		eqAt(t, r, "total", 2)
		eq(t, env.mock.RequestsTo(nodesN+"/syslog", "")[0].Query, `{"service":"pveproxy","since":"2026-01-01 00:00:00","limit":"10"}`)
	})

	nodesRun(t, env, "reads the journal with a default of the last 100 entries", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/journal$`, `["line 1","line 2"]`)
		r := env.call(t, "proxmox_get_node_journal", `{"node":"pve1","unit":"pveproxy"}`)
		eqAt(t, r, "lines", `["line 1","line 2"]`)
		eq(t, env.mock.RequestsTo(nodesN+"/journal", "")[0].Query, `{"lastentries":"100","unit":"pveproxy"}`)
	})

	nodesRun(t, env, "converts journal times to epoch and rejects bad ones", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/journal$`, `[]`)
		env.call(t, "proxmox_get_node_journal", `{"node":"pve1","since":"2023-11-14T22:13:20Z","until":1700000100,"priority":"3"}`)
		eq(t, env.mock.RequestsTo(nodesN+"/journal", "")[0].Query, `{"since":"1700000000","until":"1700000100","priority":"3"}`)
		match(t, env.errorText(t, "proxmox_get_node_journal", `{"node":"pve1","since":"not a date"}`), `Not a valid time`)
	})

	t.Run("parses the date formats Date.parse accepts", func(t *testing.T) {
		for in, want := range map[string]float64{
			"1700000000": 1700000000, "2023-11-14T22:13:20Z": 1700000000, "2023-11-14T23:13:20+01:00": 1700000000,
			"2023-11-14": 1699920000, "2023-11-14T22:13:20.999Z": 1700000000,
		} {
			got, err := nodesToEpoch(in)
			if err != nil || got != want {
				t.Fatalf("%s: got %v, %v; want %v", in, got, err, want)
			}
		}
		local := time.Date(2023, 11, 14, 22, 13, 20, 0, time.Local).Unix()
		if got, _ := nodesToEpoch("2023-11-14 22:13:20"); got != float64(local) {
			t.Fatalf("local time: got %v, want %v", got, local)
		}
		if got, _ := nodesToEpoch(1700000000.7); got != 1700000000.0 {
			t.Fatalf("number: got %v", got)
		}
	})
}

func TestNodesUpdatesAndRepositories(t *testing.T) {
	env := startEnv(t)

	nodesRun(t, env, "lists updates with counts", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/apt/update$`, `[
			{"Package":"pve-manager","OldVersion":"9.0.9","Version":"9.0.10","Origin":"Proxmox","Priority":"optional","Title":"PVE manager"},
			{"Package":"proxmox-kernel-6.14","OldVersion":"1","Version":"2","Origin":"Proxmox","Priority":"optional"},
			{"Package":"libc6","OldVersion":"2.41-1","Version":"2.41-2","Origin":"Debian","Priority":"required"}]`)
		r := env.call(t, "proxmox_list_node_updates", `{"node":"pve1"}`)
		eqAt(t, r, "total", 3)
		eqAt(t, r, "by_origin", `{"Proxmox":2,"Debian":1}`)
		eqAt(t, r, "kernel_update", true)
		eqAt(t, r, "items.0", `{"package":"libc6","installed":"2.41-1","new":"2.41-2","origin":"Debian","priority":"required"}`)
	})

	nodesRun(t, env, "refreshes the package index as a task", func(t *testing.T) {
		upid := env.mock.Upid("pve1", "aptupdate", "")
		env.mock.Reply("POST", `^/nodes/pve1/apt/update$`, upid)
		r := env.call(t, "proxmox_refresh_node_updates", `{"node":"pve1"}`)
		eqAt(t, r, "status", "ok")
		eqAt(t, r, "task.type", "aptupdate")
	})

	nodesRun(t, env, "gets a changelog and package versions", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/apt/changelog$`, "pve-manager (9.0.10) ...")
		cl := env.call(t, "proxmox_get_package_changelog", `{"node":"pve1","package":"pve-manager"}`)
		eqAt(t, cl, "text", "pve-manager (9.0.10) ...")
		eq(t, env.mock.RequestsTo(nodesN+"/apt/changelog", "")[0].Query, `{"name":"pve-manager"}`)

		env.mock.Reply("GET", `^/nodes/pve1/apt/versions$`, `[
			{"Package":"proxmox-ve","Version":"9.0.0","CurrentState":"Installed","RunningKernel":"6.14.8-2-pve","ManagerVersion":"9.0.10"},
			{"Package":"zfsutils-linux","Version":"2.3","CurrentState":"NotInstalled"}]`)
		v := env.call(t, "proxmox_get_node_package_versions", `{"node":"pve1"}`)
		eqAt(t, v, "running_kernel", "6.14.8-2-pve")
		eqAt(t, v, "manager_version", "9.0.10")
		eqAt(t, v, "items", `[{"package":"proxmox-ve","version":"9.0.0"},{"package":"zfsutils-linux","state":"NotInstalled"}]`)
	})

	repos := `{"digest":"dg",
		"files":[{"path":"/etc/apt/sources.list.d/pve-enterprise.sources","file-type":"sources",
			"repositories":[{"Types":["deb"],"URIs":["https://enterprise.proxmox.com/debian/pve"],"Suites":["trixie"],"Components":["pve-enterprise"],"Enabled":true}]}],
		"errors":[{"path":"/etc/apt/broken.list","error":"parse error"}],
		"infos":[{"path":"/etc/apt/sources.list.d/pve-enterprise.sources","index":"0","kind":"warning","message":"no subscription"}],
		"standard-repos":[{"handle":"enterprise","name":"Enterprise","status":true},{"handle":"no-subscription","name":"No-Subscription"}]}`

	nodesRun(t, env, "summarizes repositories", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/apt/repositories$`, repos)
		r := env.call(t, "proxmox_list_node_repositories", `{"node":"pve1"}`)
		eqAt(t, r, "repositories.0.enabled", true)
		eqAt(t, r, "repositories.0.components", "pve-enterprise")
		eqAt(t, r, "repositories.0.notes", `["warning: no subscription"]`)
		eqAt(t, r, "errors", `["/etc/apt/broken.list: parse error"]`)
		eqAt(t, r, "warnings", `["/etc/apt/sources.list.d/pve-enterprise.sources#0: no subscription"]`)
		eqAt(t, r, "standard_repos.0.status", "enabled")
		eqAt(t, r, "standard_repos.1", `{"handle":"no-subscription","name":"No-Subscription","status":"not configured"}`)
	})

	nodesRun(t, env, "enables/disables or adds repositories", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/apt/repositories$`, repos)
		env.call(t, "proxmox_set_node_repository", `{"node":"pve1","path":"/etc/apt/x.sources","index":0,"enabled":false,"digest":"dg"}`)
		eqStr(t, env.mock.LastWrite().Method, "POST")
		eq(t, env.mock.LastWrite().Body, `{"path":"/etc/apt/x.sources","index":"0","enabled":"0","digest":"dg"}`)
		env.call(t, "proxmox_set_node_repository", `{"node":"pve1","handle":"no-subscription"}`)
		eqStr(t, env.mock.LastWrite().Method, "PUT")
		eq(t, env.mock.LastWrite().Body, `{"handle":"no-subscription"}`)
		match(t, env.errorText(t, "proxmox_set_node_repository", `{"node":"pve1","path":"/x"}`), `give path, index and enabled`)
		match(t, env.errorText(t, "proxmox_set_node_repository", `{"node":"pve1","handle":"test","index":0}`), `not both`)
	})
}

func TestNodesSubscription(t *testing.T) {
	env := startEnv(t)

	nodesRun(t, env, "gets, sets, refreshes and deletes without leaking the key", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/subscription$`, `{"status":"active","level":"b","key":"pve1b-topsecret","productname":"Basic","nextduedate":"2027-01-01","serverid":"X"}`)
		s := env.call(t, "proxmox_get_node_subscription", `{"node":"pve1"}`)
		eqAt(t, s, "level", "b")
		eqAt(t, s, "next_due_date", "2027-01-01")
		nodesNotLeaked(t, s, "topsecret")

		set := env.call(t, "proxmox_set_node_subscription", `{"node":"pve1","key":"pve1b-newsecret"}`)
		eqStr(t, env.mock.LastWrite().Method, "PUT")
		eq(t, env.mock.LastWrite().Body, `{"key":"pve1b-newsecret"}`)
		nodesNotLeaked(t, set, "secret")

		env.call(t, "proxmox_set_node_subscription", `{"node":"pve1","force":true}`)
		eqStr(t, env.mock.LastWrite().Method, "POST")
		eq(t, env.mock.LastWrite().Body, `{"force":"1"}`)

		env.call(t, "proxmox_delete_node_subscription", `{"node":"pve1"}`)
		eq(t, []any{env.mock.LastWrite().Method, env.mock.LastWrite().Path}, []any{"DELETE", nodesN + "/subscription"})
	})
}

func TestNodesCertificates(t *testing.T) {
	env := startEnv(t)
	const pem = "-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----"
	const key = "-----BEGIN PRIVATE KEY-----\nSECRETKEYDATA\n-----END PRIVATE KEY-----"

	nodesRun(t, env, "lists certificate info without PEM bodies", func(t *testing.T) {
		notafter := time.Now().Unix() + 10*86400 + 60
		env.mock.Reply("GET", `^/nodes/pve1/certificates/info$`, js.Array([]*Obj{js.O(
			"filename", "pveproxy-ssl.pem", "subject", "/CN=pve1", "issuer", "/CN=R3", "san", []any{"pve1.example.com"},
			"notbefore", 1700000000, "notafter", float64(notafter), "fingerprint", "AB:CD", "public-key-type", "rsaEncryption", "public-key-bits", 2048, "pem", pem,
		)}))
		r := env.call(t, "proxmox_get_node_certificates", `{"node":"pve1"}`)
		cert := at(r, "certificates.0")
		eqAt(t, cert, "days_left", 10)
		eqAt(t, cert, "public_key", "rsaEncryption 2048 bits")
		eqAt(t, cert, "san", `["pve1.example.com"]`)
		nodesNotLeaked(t, r, "BEGIN")
	})

	nodesRun(t, env, "uploads a custom certificate without echoing the key", func(t *testing.T) {
		env.mock.Reply("POST", `^/nodes/pve1/certificates/custom$`, js.O("filename", "pveproxy-ssl.pem", "subject", "/CN=pve1", "pem", pem))
		r := env.call(t, "proxmox_upload_node_certificate", js.O("node", "pve1", "certificates", pem, "key", key, "force", true, "restart", true))
		eqAt(t, r, "uploaded", true)
		eqAt(t, r, "certificate.subject", "/CN=pve1")
		nodesNotLeaked(t, r, "SECRETKEYDATA")
		nodesNotLeaked(t, r, "BEGIN")
		eq(t, env.mock.LastWrite().Body, js.O("certificates", pem, "key", key, "force", "1", "restart", "1"))
	})

	nodesRun(t, env, "deletes the custom certificate", func(t *testing.T) {
		env.call(t, "proxmox_delete_node_certificate", `{"node":"pve1","restart":true}`)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path, w.Query}, []any{"DELETE", nodesN + "/certificates/custom", js.O("restart", "1")})
	})

	nodesRun(t, env, "orders, renews and revokes ACME certificates", func(t *testing.T) {
		for _, am := range [][2]string{{"order", "POST"}, {"renew", "PUT"}, {"revoke", "DELETE"}} {
			upid := env.mock.Upid("pve1", "acme"+am[0], "")
			env.mock.Reply(am[1], `^/nodes/pve1/certificates/acme/certificate$`, upid)
			r := env.call(t, "proxmox_node_acme_certificate", js.O("node", "pve1", "action", am[0], "force", true))
			eqAt(t, r, "status", "ok")
			eqStr(t, env.mock.LastWrite().Method, am[1])
		}
		eq(t, env.mock.RequestsTo(nodesN+"/certificates/acme/certificate", "POST")[0].Body, `{"force":"1"}`)
		eq(t, env.mock.RequestsTo(nodesN+"/certificates/acme/certificate", "DELETE")[0].Query, `{}`)

		writeOnly := connect(t, env.client, toolOptions("write"))
		match(t, errorTextOf(t, callOn(t, writeOnly, "proxmox_node_acme_certificate", `{"node":"pve1","action":"revoke"}`)), `PROXMOX_ALLOW_DELETES`)
	})
}

func TestNodesClusterACME(t *testing.T) {
	env := startEnv(t)

	nodesRun(t, env, "lists and gets accounts", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/acme/account$`, `[{"name":"default"}]`)
		env.mock.Reply("GET", `^/cluster/acme/account/default$`, `{"directory":"https://acme-v02.api.letsencrypt.org/directory","location":"https://acme/acct/1",
			"account":{"status":"valid","contact":["mailto:a@b.c"],"key":{"kty":"RSA","n":"pub"}}}`)
		eq(t, env.call(t, "proxmox_list_acme_accounts", nil), `{"total":1,"returned":1,"items":[{"name":"default","directory":"https://acme-v02.api.letsencrypt.org/directory","status":"valid","contact":["mailto:a@b.c"]}]}`)
		acc := env.call(t, "proxmox_get_acme_account", `{}`)
		eqAt(t, acc, "account.status", "valid")
		eqAt(t, acc, "name", "default")
	})

	nodesRun(t, env, "registers, updates and deactivates accounts", func(t *testing.T) {
		upid := env.mock.Upid("pve1", "acmeregister", "default")
		env.mock.Reply("POST", `^/cluster/acme/account$`, upid)
		r := env.call(t, "proxmox_save_acme_account", `{"create":true,"contact":"a@b.c","tos_url":"https://tos","eab_kid":"kid","eab_hmac_key":"hmac"}`)
		eqAt(t, r, "status", "ok")
		eqAt(t, r, "registered", true)
		nodesNotLeaked(t, r, `hmac"`)
		eq(t, env.mock.LastWrite().Body, `{"name":"default","contact":"a@b.c","tos_url":"https://tos","eab-kid":"kid","eab-hmac-key":"hmac"}`)
		match(t, env.errorText(t, "proxmox_save_acme_account", `{"create":true}`), `contact is required`)

		env.mock.On("PUT", `^/cluster/acme/account/le$`, func(*Obj, []string, proxmoxtest.Request) any { return env.mock.Upid("pve1", "acmeupdate", "le") })
		env.call(t, "proxmox_save_acme_account", `{"name":"le","contact":"x@y.z"}`)
		eqStr(t, env.mock.LastWrite().Method, "PUT")
		eq(t, env.mock.LastWrite().Body, `{"contact":"x@y.z"}`)

		env.mock.On("DELETE", `^/cluster/acme/account/le$`, func(*Obj, []string, proxmoxtest.Request) any { return env.mock.Upid("pve1", "acmedeactivate", "le") })
		eqAt(t, env.call(t, "proxmox_delete_acme_account", `{"name":"le"}`), "deleted", true)
	})

	nodesRun(t, env, "lists directories with CA metadata", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/acme/directories$`, `[{"name":"Let's Encrypt V2","url":"https://le/dir"}]`)
		env.mock.Reply("GET", `^/cluster/acme/meta$`, `{"termsOfService":"https://le/tos","externalAccountRequired":false}`)
		r := env.call(t, "proxmox_list_acme_directories", `{"directory":"https://le/dir"}`)
		eqAt(t, r, "total", 1)
		eqAt(t, r, "meta.termsOfService", "https://le/tos")
		eqAt(t, r, "meta.externalAccountRequired", false)
		eq(t, env.mock.RequestsTo("/cluster/acme/meta", "")[0].Query, `{"directory":"https://le/dir"}`)

		// Read-only, the node must not be sent to fetch a URL of the caller's choosing.
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/acme/directories$`, `[]`)
		ro := connect(t, env.client, toolOptions("read"))
		match(t, errorTextOf(t, callOn(t, ro, "proxmox_list_acme_directories", `{"directory":"http://192.0.2.10/"}`)), `needs write access`)
		if len(env.mock.RequestsTo("/cluster/acme/meta", "")) != 0 || len(env.mock.RequestsTo("/cluster/acme/tos", "")) != 0 {
			t.Fatal("the directory was fetched read-only")
		}
		eq(t, parseResult(t, callOn(t, ro, "proxmox_list_acme_directories", `{}`)), `{"total":0,"returned":0,"items":[]}`)
	})

	nodesRun(t, env, "falls back to the terms of service on older versions", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/acme/directories$`, `[]`)
		env.mock.Reply("GET", `^/cluster/acme/tos$`, "https://le/tos")
		eq(t, env.call(t, "proxmox_list_acme_directories", `{"directory":"https://le/dir"}`), `{"total":0,"returned":0,"items":[],"meta":{"directory":"https://le/dir","termsOfService":"https://le/tos"}}`)
	})

	nodesRun(t, env, "lists, gets and saves plugins without exposing credentials", func(t *testing.T) {
		plugin := `{"plugin":"cf","type":"dns","api":"cf","data":"CF_Token=supersecret\nCF_Account_ID=acct","digest":"pd"}`
		env.mock.Reply("GET", `^/cluster/acme/plugins$`, `[`+plugin+`,{"plugin":"standalone","type":"standalone"}]`)
		env.mock.Reply("GET", `^/cluster/acme/plugins/cf$`, plugin)
		list := env.call(t, "proxmox_list_acme_plugins", `{}`)
		eqAt(t, list, "items.0.data_keys", `["CF_Token=<redacted>","CF_Account_ID=<redacted>"]`)
		nodesNotLeaked(t, list, "supersecret")
		one := env.call(t, "proxmox_get_acme_plugin", `{"id":"cf"}`)
		eqAt(t, one, "api", "cf")
		eqAt(t, one, "data", js.Undef)

		saved := env.call(t, "proxmox_save_acme_plugin", `{"id":"cf","create":true,"api":"cf","data":{"CF_Token":"supersecret"},"nodes":["pve1","pve2"],"validation_delay":60}`)
		nodesNotLeaked(t, saved, "supersecret")
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eq(t, w.Body, js.O("id", "cf", "type", "dns", "api", "cf", "data", base64.StdEncoding.EncodeToString([]byte("CF_Token=supersecret")), "nodes", "pve1,pve2", "validation-delay", "60"))

		env.call(t, "proxmox_save_acme_plugin", `{"id":"cf","disable":true,"delete":["nodes"]}`)
		w = env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path, w.Body}, []any{"PUT", "/cluster/acme/plugins/cf", js.O("disable", "1", "delete", "nodes")})

		env.call(t, "proxmox_delete_acme_plugin", `{"id":"cf"}`)
		eq(t, []any{env.mock.LastWrite().Method, env.mock.LastWrite().Path}, []any{"DELETE", "/cluster/acme/plugins/cf"})
	})

	nodesRun(t, env, "redacts base64-encoded plugin data (as the API returns it)", func(t *testing.T) {
		// "CF_Token=s3cret!!" encodes with "=" padding; splitting the base64 at "=" would leak it.
		data := base64.StdEncoding.EncodeToString([]byte("CF_Token=s3cret!!\nCF_Account_ID=acct1"))
		if !strings.HasSuffix(data, "=") {
			t.Fatal("test data should end with padding")
		}
		env.mock.Reply("GET", `^/cluster/acme/plugins/b64$`, js.O("plugin", "b64", "type", "dns", "api", "cf", "data", data))
		one := env.call(t, "proxmox_get_acme_plugin", `{"id":"b64"}`)
		eqAt(t, one, "data_keys", `["CF_Token=<redacted>","CF_Account_ID=<redacted>"]`)
		nodesNotLeaked(t, one, data[:10])
	})

	nodesRun(t, env, "filters the challenge schema", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/acme/challenge-schema$`, `[
			{"id":"cf","name":"Cloudflare","type":"dns","schema":{"fields":{"CF_Token":{"type":"string"}}}},
			{"id":"ovh","name":"OVH","type":"dns","schema":{"fields":{}}}]`)
		eq(t, env.call(t, "proxmox_get_acme_challenge_schema", `{"api":"cf"}`), `[{"api":"cf","name":"Cloudflare","type":"dns","fields":{"CF_Token":{"type":"string"}}}]`)
	})
}
