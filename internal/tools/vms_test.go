package tools

import (
	"strings"
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

const vmsVM100 = "/nodes/pve1/qemu/100"

func vmsNextID(env *testEnv, id string) {
	env.mock.Reply("GET", `^/cluster/nextid$`, js.Quote(id))
}

// vmsTaskRoute answers method on pattern with a fresh task ID.
func vmsTaskRoute(env *testEnv, method, pattern, typ, node string) {
	env.mock.On(method, pattern, func(*Obj, []string, proxmoxtest.Request) any { return env.mock.Upid(node, typ, "100") })
}

func vmsNoWrites(t *testing.T, env *testEnv) {
	t.Helper()
	if w := env.mock.Writes(); len(w) != 0 {
		t.Fatalf("unexpected writes: %v %v", w[0].Method, w[0].Path)
	}
}

func TestVmsRead(t *testing.T) {
	env := startEnv(t)

	t.Run("lists VMs from every online node with full details", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/nodes/pve1/qemu$`, `[
			{"vmid":100,"name":"web","status":"running","cpu":0.5,"cpus":4,"mem":1073741824,"maxmem":4294967296,"uptime":120,"diskread":2048,"diskwrite":4096,"running-qemu":"9.2.0","pressurecpusome":1.5},
			{"vmid":9000,"name":"tpl-debian","status":"stopped","maxmem":2147483648,"template":1}]`)
		env.mock.Reply("GET", `^/nodes/pve2/qemu$`, `[{"vmid":101,"name":"db","status":"stopped","maxmem":8589934592}]`)
		res := env.call(t, "proxmox_list_vms", nil)
		eqAt(t, res, "total", 3)
		eq(t, pluck(at(res, "items"), "vmid"), "[100,101,9000]")
		web := at(res, "items.0")
		eqAt(t, web, "node", "pve1")
		eqAt(t, web, "cpu_pct", 50)
		eqAt(t, web, "disk_write", "4 KiB")
		eqAt(t, web, "running_qemu", "9.2.0")
		eqAt(t, web, "pressure_pct", `{"cpu_some":1.5}`)
		eqAt(t, res, "items.2.template", true)
		eqStr(t, at(env.mock.RequestsTo("/nodes/pve1/qemu", "GET")[0].Query, "full"), "1")

		filtered := env.call(t, "proxmox_list_vms", `{"node":"pve2","search":"db"}`)
		eqAt(t, filtered, "total", 1)
		eqAt(t, filtered, "items.0.memory", "8 GiB")
	})

	t.Run("reports nodes that fail instead of failing the whole list", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/nodes/pve1/qemu$`, `[{"vmid":100,"name":"web","status":"running"}]`)
		env.mock.On("GET", `^/nodes/pve2/qemu$`, func(*Obj, []string, proxmoxtest.Request) any { return proxmoxtest.Error(500, "node down") })
		res := env.call(t, "proxmox_list_vms", `{"status":"running"}`)
		eqAt(t, res, "total", 1)
		eqAt(t, res, "errors.0.node", "pve2")
		match(t, at(res, "errors.0.error"), `HTTP 500: node down`)
	})

	t.Run("gets status, summarized config and pending changes", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `/qemu/100/status/current$`, `{"status":"running","qmpstatus":"running","cpu":0.031,"cpus":4,"mem":2147483648,"maxmem":4294967296,
			"uptime":3600,"netin":1000,"netout":2000,"diskread":1048576,"diskwrite":0,"pid":1234,"agent":1,"running-qemu":"9.2.0",
			"running-machine":"pc-q35-9.2+pve1","ha":{"managed":1,"state":"started","group":"g1"}}`)
		env.mock.Reply("GET", `/qemu/100/pending$`, `[
			{"key":"cores","value":4,"pending":8},
			{"key":"name","value":"web"},
			{"key":"cipassword","value":"hunter2","pending":"newpass"},
			{"key":"net1","value":"virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr1","delete":1}]`)
		res := env.call(t, "proxmox_get_vm", `{"vmid":100}`)
		eqAt(t, res, "node", "pve1")
		eqAt(t, res, "status", "running")
		eqAt(t, res, "qmpstatus", js.Undef)
		eqAt(t, res, "uptime", "1h 0m")
		eqAt(t, res, "memory", "2 GiB of 4 GiB (50%)")
		eqAt(t, res, "disk_read", "1 MiB")
		eqAt(t, res, "ha", `{"state":"started","group":"g1"}`)
		eqAt(t, res, "running_machine", "pc-q35-9.2+pve1")
		c := at(res, "config")
		eqAt(t, c, "cpu", `{"sockets":1,"cores":4}`)
		eqAt(t, c, "memory.size", "4 GiB")
		eqAt(t, c, "boot_order", `["scsi0","net0"]`)
		eqAt(t, c, "disks.scsi0.file", "local-lvm:vm-100-disk-0")
		eqAt(t, c, "disks.scsi0.size", "32G")
		eqAt(t, c, "nets.net0.model", "virtio")
		eqStr(t, at(c, "nets.net0.tag"), "10")
		eqAt(t, c, "cloud_init.password_set", true)
		eqAt(t, c, "tags", `["prod","web"]`)
		eqAt(t, res, "pending_changes.length", 3)
		eqAt(t, res, "pending_changes.0", `{"key":"cores","current":4,"pending":8}`)
		eqAt(t, res, "pending_changes.2.delete", true)
		match(t, at(res, "note"), `reboot`)
		text := js.Stringify(res, "")
		if strings.Contains(text, "hunter2") || strings.Contains(text, "newpass") {
			t.Fatal("secrets must be redacted")
		}
		eqStr(t, at(env.mock.RequestsTo(vmsVM100+"/config", "GET")[0].Query, "current"), "1")
	})

	t.Run("returns redacted raw objects and tolerates a missing pending endpoint", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `/qemu/100/status/current$`, `{"status":"stopped","maxmem":4294967296}`)
		res := env.call(t, "proxmox_get_vm", `{"vmid":100,"raw":true}`)
		eqAt(t, res, "config.cipassword", "<redacted>")
		eqAt(t, res, "pending", `[]`)
		summary := env.call(t, "proxmox_get_vm", `{"vmid":100}`)
		eqAt(t, summary, "memory", "4 GiB")
		eqAt(t, summary, "pending_changes", js.Undef)
	})

	t.Run("refuses containers", func(t *testing.T) {
		env.mock.Reset()
		match(t, env.errorText(t, "proxmox_get_vm", `{"vmid":200}`), `container`)
	})

	t.Run("gets the config, current, from a snapshot or with pending values", func(t *testing.T) {
		env.mock.Reset()
		res := env.call(t, "proxmox_get_vm_config", `{"vmid":100,"snapshot":"before-upgrade","current":true}`)
		eqAt(t, res, "config.cipassword", "<redacted>")
		eqAt(t, res, "config.digest", "abc123")
		eq(t, js.Array(at(res, "config").(*Obj).Keys()[:3]), `["agent","boot","cipassword"]`)
		q := env.mock.RequestsTo(vmsVM100+"/config", "GET")[0].Query
		eqAt(t, q, "snapshot", "before-upgrade")
		eqStr(t, at(q, "current"), "1")

		env.mock.Reply("GET", `/qemu/100/pending$`, `[
			{"key":"memory","value":"4096","pending":"8192"},
			{"key":"cipassword","value":"x"},
			{"key":"tags","value":"a","delete":2}]`)
		p := env.call(t, "proxmox_get_vm_config", `{"vmid":100,"pending":true}`)
		eqAt(t, p, "entries.0", `{"key":"memory","value":"4096","pending":"8192"}`)
		eqAt(t, p, "entries.1.value", "<redacted>")
		eqAt(t, p, "entries.2.delete", "force")
	})

	t.Run("sorts config keys numerically", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `/qemu/100/config$`, `{"net10":"a","net2":"b","Name":"x","agent":"1"}`)
		res := env.call(t, "proxmox_get_vm_config", `{"vmid":100}`)
		eq(t, js.Array(at(res, "config").(*Obj).Keys()), `["agent","Name","net2","net10"]`)
	})

	t.Run("summarizes RRD statistics", func(t *testing.T) {
		env.mock.Reset()
		start := 1700000000.0
		var data []any
		for i := 0; i < 60; i++ {
			netout := 2048
			if i == 59 {
				netout = 4096
			}
			data = append(data, js.O("time", start+float64(i*60), "cpu", float64(i)/100, "maxcpu", 4, "mem", 1073741824+i*1048576, "maxmem", 4294967296.0,
				"netin", 1024, "netout", netout, "diskread", 0, "diskwrite", 512))
		}
		data = append(data, js.O("time", start+3600))
		env.mock.Reply("GET", `/qemu/100/rrddata$`, js.Stringify(data, ""))
		res := env.call(t, "proxmox_get_vm_stats", `{"vmid":100,"timeframe":"day","cf":"MAX"}`)
		q := env.mock.RequestsTo(vmsVM100+"/rrddata", "")[0].Query
		eqAt(t, q, "timeframe", "day")
		eqAt(t, q, "cf", "MAX")
		eqAt(t, res, "samples", 61)
		eqAt(t, res, "summary.cpu_pct.min", 0)
		eqAt(t, res, "summary.cpu_pct.max", 59)
		eqAt(t, res, "summary.memory.min", "1 GiB")
		eqAt(t, res, "summary.memory.of", "4 GiB")
		eqAt(t, res, "summary.net_out.max", "4 KiB/s")
		eqAt(t, res, "summary.net_in.avg", "1 KiB/s")
		if n := js.Num(at(res, "series.length")); n > 12 || n < 10 {
			t.Fatalf("series length %v", n)
		}
		eqAt(t, res, "series.0.time", proxmox.ISO(start*1000))
		eqAt(t, res, "from", proxmox.ISO(start*1000))

		summaryOnly := env.call(t, "proxmox_get_vm_stats", `{"vmid":100,"points":0}`)
		eqAt(t, summaryOnly, "series", js.Undef)
		eqAt(t, summaryOnly, "timeframe", "hour")
		raw := env.call(t, "proxmox_get_vm_stats", `{"vmid":100,"raw":true}`)
		eqAt(t, raw, "data.length", 61)
	})

	t.Run("handles empty RRD data", func(t *testing.T) {
		eq(t, vmsSummarizeRrd(nil, 12), `{"samples":0}`)
	})

	t.Run("shows cloud-init settings and generated documents", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `/qemu/100/cloudinit$`, js.Stringify(js.V([]any{
			js.O("key", "ciuser", "value", "debian"),
			js.O("key", "cipassword", "value", "$5$hash", "pending", "$5$newhash"),
			js.O("key", "sshkeys", "value", js.EncodeURIComponent("ssh-ed25519 AAAAC3Nza me@laptop")),
			js.O("key", "ipconfig0", "value", "ip=dhcp", "pending", "ip=10.0.0.5/24,gw=10.0.0.1"),
		}), ""))
		env.mock.On("GET", `/qemu/100/cloudinit/dump$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			switch p.Get("type") {
			case "user":
				return "#cloud-config\nuser: debian\npassword: $5$hash\nchpasswd:\n  expire: False\n"
			case "network":
				return "version: 1\n"
			}
			return proxmoxtest.Error(500, "boom")
		})
		res := env.call(t, "proxmox_get_vm_cloudinit", `{"vmid":100}`)
		eqAt(t, res, "pending_keys", `["cipassword","ipconfig0"]`)
		var ssh any
		for _, s := range js.Objs(at(res, "settings")) {
			if s.Get("key") == "sshkeys" {
				ssh = s.Get("value")
			}
		}
		eq(t, ssh, `["ssh-ed25519 me@laptop"]`)
		match(t, at(res, "generated.user"), `password: <redacted>`)
		eqAt(t, res, "generated.network", "version: 1\n")
		match(t, at(res, "generated.meta"), `^Error:`)
		match(t, at(res, "note"), `regenerate`)
		if strings.Contains(js.Stringify(res, ""), "$5$") {
			t.Fatal("password hashes must be redacted")
		}
		none := env.call(t, "proxmox_get_vm_cloudinit", `{"vmid":100,"dump":[]}`)
		eqAt(t, none, "generated", js.Undef)
	})

	t.Run("checks migration preconditions", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `/qemu/100/migrate$`, `{"running":1,"allowed_nodes":[],"not_allowed_nodes":{"pve2":{"unavailable_storages":["local-lvm"]}},
			"local_disks":[{"volid":"local-lvm:vm-100-disk-0","size":34359738368,"drivename":"scsi0"},{"volid":"local:iso/debian-12.iso","cdrom":1,"size":1}],
			"local_resources":["hostpci0"],"mapped-resources":[],"has-dbus-vmstate":1}`)
		res := env.call(t, "proxmox_check_vm_migration", `{"vmid":100,"target":"pve2"}`)
		eqAt(t, env.mock.RequestsTo(vmsVM100+"/migrate", "")[0].Query, "target", "pve2")
		eqAt(t, res, "running", true)
		eqAt(t, res, "not_allowed_nodes.pve2.unavailable_storages", `["local-lvm"]`)
		eqAt(t, res, "local_disks.0", `{"volid":"local-lvm:vm-100-disk-0","size":"32 GiB","drive":"scsi0"}`)
		eqAt(t, res, "local_disks.1.cdrom", true)
		eqAt(t, res, "local_resources", `["hostpci0"]`)
		eqAt(t, res, "can_migrate_conntrack", true)
	})

	t.Run("checks storage features", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("GET", `/qemu/9000/feature$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			return js.O("hasFeature", pick(p.Get("feature") == "clone", 1, 0), "nodes", []any{"pve1"})
		})
		res := env.call(t, "proxmox_check_vm_feature", `{"vmid":9000,"feature":"clone"}`)
		eq(t, res, `{"vmid":9000,"name":"tpl-debian","node":"pve1","feature":"clone","available":true,"nodes":["pve1"]}`)
		snap := env.call(t, "proxmox_check_vm_feature", `{"vmid":9000,"feature":"snapshot","snapname":"s1"}`)
		eqAt(t, snap, "available", false)
		reqs := env.mock.RequestsTo("/nodes/pve1/qemu/9000/feature", "")
		eqAt(t, reqs[len(reqs)-1].Query, "snapname", "s1")
	})
}

func TestVmsPower(t *testing.T) {
	env := startEnv(t)

	t.Run("shuts down with options and waits for the task", func(t *testing.T) {
		env.mock.Reset()
		vmsTaskRoute(env, "POST", `/qemu/100/status/shutdown$`, "qmshutdown", "pve1")
		res := env.call(t, "proxmox_vm_power", `{"vmid":100,"action":"shutdown","timeout":60,"force_stop":true}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Path, vmsVM100+"/status/shutdown")
		eq(t, w.Body, `{"timeout":"60","forceStop":"1"}`)
		eqAt(t, res, "status", "ok")
		eqAt(t, res, "action", "shutdown")
		eqAt(t, res, "task.type", "qmshutdown")
	})

	t.Run("hibernates via suspend to disk and can return without waiting", func(t *testing.T) {
		env.mock.Reset()
		vmsTaskRoute(env, "POST", `/qemu/100/status/suspend$`, "qmsuspend", "pve1")
		res := env.call(t, "proxmox_vm_power", `{"vmid":100,"action":"hibernate","state_storage":"local-lvm","wait":false}`)
		eq(t, env.mock.LastWrite().Body, `{"todisk":"1","statestorage":"local-lvm"}`)
		eqAt(t, res, "status", "started")
		match(t, at(res, "upid"), `^UPID:pve1:`)
	})

	t.Run("starts stopped VMs on their own node and skips no-op actions", func(t *testing.T) {
		env.mock.Reset()
		vmsTaskRoute(env, "POST", `/qemu/101/status/start$`, "qmstart", "pve2")
		env.call(t, "proxmox_vm_power", `{"vmid":101,"action":"start","machine":"q35"}`)
		eqStr(t, env.mock.LastWrite().Path, "/nodes/pve2/qemu/101/status/start")
		eqAt(t, env.mock.LastWrite().Body, "machine", "q35")

		env.mock.Reset()
		eqAt(t, env.call(t, "proxmox_vm_power", `{"vmid":100,"action":"start"}`), "status", "already running")
		eqAt(t, env.call(t, "proxmox_vm_power", `{"vmid":101,"action":"stop"}`), "status", "already stopped")
		vmsNoWrites(t, env)
	})

	t.Run("validates options per action and refuses templates", func(t *testing.T) {
		env.mock.Reset()
		match(t, env.errorText(t, "proxmox_vm_power", `{"vmid":100,"action":"reset","force_stop":true}`), `force_stop only applies to: shutdown`)
		match(t, env.errorText(t, "proxmox_vm_power", `{"vmid":9000,"action":"start"}`), `template`)
		vmsNoWrites(t, env)
	})

	t.Run("reports failed tasks with the log tail", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("POST", `/qemu/100/status/reboot$`, func(*Obj, []string, proxmoxtest.Request) any {
			return env.mock.Upid("pve1", "qmreboot", "100", proxmoxtest.Task{Status: "stopped", ExitStatus: "VM quit/powerdown failed", Log: []string{"trying to shut down", "TASK ERROR: VM quit/powerdown failed"}})
		})
		text := env.errorText(t, "proxmox_vm_power", `{"vmid":100,"action":"reboot"}`)
		match(t, text, `failed: VM quit/powerdown failed`)
		match(t, text, `TASK ERROR`)
	})
}

func TestVmsCreateCloneConfig(t *testing.T) {
	env := startEnv(t)

	t.Run("creates a VM from typed arguments", func(t *testing.T) {
		env.mock.Reset()
		vmsNextID(env, "150")
		vmsTaskRoute(env, "POST", `^/nodes/pve1/qemu$`, "qmcreate", "pve1")
		res := env.call(t, "proxmox_create_vm", `{
			"node":"pve1","name":"app1","memory_mb":4096,"cores":2,"cpu":"host","bios":"ovmf","machine":"q35","storage":"local-lvm",
			"disks":[
				{"size_gb":32,"options":{"discard":"on","ssd":true,"iothread":true}},
				{"bus":"virtio","storage":"fast","import_from":"local:import/debian.qcow2"},
				{"bus":"scsi","index":5,"spec":"local-lvm:10,backup=0"}],
			"iso":"local:iso/debian-12.iso",
			"nets":[{"vlan":10,"firewall":true},{"model":"e1000","bridge":"vmbr1","macaddr":"BC:24:11:00:00:09"}],
			"tpm_storage":"local-lvm","cloudinit_storage":"local-lvm","ciuser":"debian",
			"sshkeys":["ssh-ed25519 AAAA me@laptop","ssh-rsa BBBB+/= other"],
			"ipconfig":{"0":"ip=dhcp"},"agent":true,"tags":["prod","app"],"onboot":true,"start":true,"pool":"production",
			"extra":{"serial0":"socket"}}`)
		eqAt(t, res, "vmid", 150)
		eqAt(t, res, "status", "ok")
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, "/nodes/pve1/qemu")
		b := w.Body
		for _, kv := range [][2]string{
			{"vmid", "150"},
			{"scsi0", "local-lvm:32,discard=on,ssd=1,iothread=1"},
			{"virtio0", "fast:0,import-from=local:import/debian.qcow2"},
			{"scsi5", "local-lvm:10,backup=0"},
			{"ide2", "local:iso/debian-12.iso,media=cdrom"},
			{"ide0", "local-lvm:cloudinit"},
			{"efidisk0", "local-lvm:1,efitype=4m"},
			{"tpmstate0", "local-lvm:1,version=v2.0"},
			{"net0", "virtio,bridge=vmbr0,tag=10,firewall=1"},
			{"net1", "e1000=BC:24:11:00:00:09,bridge=vmbr1"},
			{"boot", "order=scsi0;scsi5;virtio0;ide2"},
			{"sshkeys", js.EncodeURIComponent("ssh-ed25519 AAAA me@laptop\nssh-rsa BBBB+/= other")},
			{"ipconfig0", "ip=dhcp"},
			{"agent", "1"},
			{"tags", "prod;app"},
			{"memory", "4096"},
			{"scsihw", "virtio-scsi-single"},
			{"ostype", "l26"},
			{"start", "1"},
			{"pool", "production"},
			{"serial0", "socket"},
		} {
			eqStr(t, b.Get(kv[0]), kv[1], kv[0])
		}
		eqAt(t, b, "storage", js.Undef)
	})

	t.Run("creates a minimal VM with a default NIC and cloud-init on ide2", func(t *testing.T) {
		env.mock.Reset()
		vmsTaskRoute(env, "POST", `^/nodes/pve2/qemu$`, "qmcreate", "pve2")
		env.call(t, "proxmox_create_vm", `{"node":"pve2","vmid":300,"cloudinit_storage":"local"}`)
		b := env.mock.LastWrite().Body
		eqStr(t, at(b, "vmid"), "300")
		eqAt(t, b, "ide2", "local:cloudinit")
		eqAt(t, b, "net0", "virtio,bridge=vmbr0")
		eqAt(t, b, "boot", js.Undef)
		if n := len(env.mock.RequestsTo("/cluster/nextid", "")); n != 0 {
			t.Fatalf("asked for the next id %d times", n)
		}
	})

	t.Run("validates create arguments", func(t *testing.T) {
		env.mock.Reset()
		run := func(extra string) string {
			return env.errorText(t, "proxmox_create_vm", `{"node":"pve1","vmid":300,`+extra+`}`)
		}
		match(t, run(`"disks":[{"size_gb":10}]`), `needs a storage`)
		match(t, run(`"disks":[{"storage":"local-lvm"}]`), `needs size_gb`)
		match(t, run(`"bios":"ovmf"`), `efi_storage`)
		match(t, run(`"iso":"local:iso/x.iso","disks":[{"bus":"ide","index":2,"storage":"s","size_gb":1}]`), `ide2 is used twice`)
		match(t, run(`"disks":[{"bus":"ide","index":9,"storage":"s","size_gb":1}]`), `Invalid disk slot`)
		vmsNoWrites(t, env)
	})

	t.Run("clones a template to the next free id", func(t *testing.T) {
		env.mock.Reset()
		vmsNextID(env, "160")
		vmsTaskRoute(env, "POST", `/qemu/9000/clone$`, "qmclone", "pve1")
		res := env.call(t, "proxmox_clone_vm", `{"vmid":9000,"name":"web2","full":true,"storage":"local-lvm","format":"raw","pool":"production"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Path, "/nodes/pve1/qemu/9000/clone")
		eq(t, w.Body, `{"newid":"160","name":"web2","full":"1","storage":"local-lvm","format":"raw","pool":"production"}`)
		eqAt(t, res, "newid", 160)
		eqAt(t, res, "source", 9000)
		match(t, env.errorText(t, "proxmox_clone_vm", `{"vmid":9000,"newid":161,"full":false,"storage":"x"}`), `full clones`)
		// A target storage on a template implies a full clone (Proxmox rejects storage for linked clones).
		env.call(t, "proxmox_clone_vm", `{"vmid":9000,"newid":162,"storage":"local-lvm"}`)
		eq(t, env.mock.LastWrite().Body, `{"newid":"162","full":"1","storage":"local-lvm"}`)
	})

	t.Run("updates config asynchronously and reports pending changes", func(t *testing.T) {
		env.mock.Reset()
		vmsTaskRoute(env, "POST", `/qemu/100/config$`, "qmconfig", "pve1")
		env.mock.Reply("GET", `/qemu/100/pending$`, `[{"key":"cpu","value":"kvm64","pending":"host"},{"key":"name","value":"web"}]`)
		res := env.call(t, "proxmox_update_vm_config", `{"vmid":100,"cpu":"host","memory_mb":8192,"boot_order":["scsi0","net0"],
			"sshkeys":"ssh-ed25519 AAAA a b","delete":["ide2","description"],"revert":["cores"],"digest":"abc123","extra":{"scsi1":"local-lvm:20,ssd=1"}}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, vmsVM100+"/config")
		eq(t, w.Body, `{"memory":"8192","cpu":"host","boot":"order=scsi0;net0","sshkeys":"ssh-ed25519%20AAAA%20a%20b",
			"delete":"ide2,description","revert":"cores","digest":"abc123","scsi1":"local-lvm:20,ssd=1"}`)
		eqAt(t, res, "status", "ok")
		eqAt(t, res, "pending_changes", `[{"key":"cpu","current":"kvm64","pending":"host"}]`)
		match(t, at(res, "note"), `reboot`)
	})

	t.Run("handles a synchronous config result and rejects unsafe or empty updates", func(t *testing.T) {
		env.mock.Reset()
		res := env.call(t, "proxmox_update_vm_config", `{"vmid":100,"onboot":false,"protection":true}`)
		eq(t, env.mock.LastWrite().Body, `{"onboot":"0","protection":"1"}`)
		eqAt(t, res, "updated", `["onboot","protection"]`)
		eqAt(t, res, "pending_changes", js.Undef)

		env.mock.Reset()
		match(t, env.errorText(t, "proxmox_update_vm_config", `{"vmid":100,"delete":["scsi0"],"extra":{"force":1}}`), `proxmox_unlink_vm_disk`)
		match(t, env.errorText(t, "proxmox_update_vm_config", `{"vmid":100,"digest":"x"}`), `Nothing to change`)
		vmsNoWrites(t, env)
	})
}

func TestVmsDisks(t *testing.T) {
	env := startEnv(t)

	t.Run("resizes a disk", func(t *testing.T) {
		env.mock.Reset()
		vmsTaskRoute(env, "PUT", `/qemu/100/resize$`, "resize", "pve1")
		res := env.call(t, "proxmox_resize_vm_disk", `{"vmid":100,"disk":"scsi0","size":"+10G"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eq(t, w.Body, `{"disk":"scsi0","size":"+10G"}`)
		eqAt(t, res, "status", "ok")
		match(t, env.errorText(t, "proxmox_resize_vm_disk", `{"vmid":100,"disk":"scsi0","size":"-5G"}`), `a size like \+10G or 64G at size`)
		match(t, env.errorText(t, "proxmox_resize_vm_disk", `{"vmid":100,"disk":"net0","size":"5G"}`), `a disk key such as scsi0`)
	})

	t.Run("moves a disk to storage or another VM", func(t *testing.T) {
		env.mock.Reset()
		vmsTaskRoute(env, "POST", `/qemu/100/move_disk$`, "qmmove", "pve1")
		env.call(t, "proxmox_move_vm_disk", `{"vmid":100,"disk":"scsi0","storage":"ceph","format":"raw","delete_source":true}`)
		eq(t, env.mock.LastWrite().Body, `{"disk":"scsi0","storage":"ceph","format":"raw","delete":"1"}`)
		env.call(t, "proxmox_move_vm_disk", `{"vmid":100,"disk":"scsi0","target_vmid":9000,"target_disk":"scsi3"}`)
		eq(t, env.mock.LastWrite().Body, `{"disk":"scsi0","target-vmid":"9000","target-disk":"scsi3"}`)
		match(t, env.errorText(t, "proxmox_move_vm_disk", `{"vmid":100,"disk":"scsi0"}`), `target storage or a target_vmid`)
		match(t, env.errorText(t, "proxmox_move_vm_disk", `{"vmid":100,"disk":"scsi0","storage":"a","target_vmid":101}`), `not both`)
	})

	t.Run("detaches or destroys disks", func(t *testing.T) {
		env.mock.Reset()
		res := env.call(t, "proxmox_unlink_vm_disk", `{"vmid":100,"disks":["scsi1","unused0"]}`)
		eq(t, env.mock.LastWrite().Body, `{"idlist":"scsi1,unused0"}`)
		match(t, at(res, "result"), `detached`)
		destroyed := env.call(t, "proxmox_unlink_vm_disk", `{"vmid":100,"disks":["unused0"],"destroy":true}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, vmsVM100+"/unlink")
		eq(t, w.Body, `{"idlist":"unused0","force":"1"}`)
		eqAt(t, destroyed, "result", "destroyed")
	})
}

func TestVmsLifecycle(t *testing.T) {
	env := startEnv(t)

	t.Run("live-migrates running VMs by default", func(t *testing.T) {
		env.mock.Reset()
		vmsTaskRoute(env, "POST", `/qemu/100/migrate$`, "qmigrate", "pve1")
		res := env.call(t, "proxmox_migrate_vm", `{"vmid":100,"target":"pve2","with_local_disks":true,"targetstorage":"local"}`)
		eq(t, env.mock.LastWrite().Body, `{"target":"pve2","online":"1","with-local-disks":"1","targetstorage":"local"}`)
		eqAt(t, res, "online", true)
		eqAt(t, res, "from", "pve1")

		vmsTaskRoute(env, "POST", `/qemu/101/migrate$`, "qmigrate", "pve2")
		env.call(t, "proxmox_migrate_vm", `{"vmid":101,"target":"pve1"}`)
		eq(t, env.mock.LastWrite().Body, `{"target":"pve1"}`)
		match(t, env.errorText(t, "proxmox_migrate_vm", `{"vmid":100,"target":"pve1"}`), `already on pve1`)
	})

	t.Run("converts stopped VMs to templates", func(t *testing.T) {
		env.mock.Reset()
		vmsTaskRoute(env, "POST", `/qemu/101/template$`, "qmtemplate", "pve2")
		res := env.call(t, "proxmox_convert_vm_to_template", `{"vmid":101}`)
		eqStr(t, env.mock.LastWrite().Path, "/nodes/pve2/qemu/101/template")
		eqAt(t, res, "status", "ok")
		match(t, env.errorText(t, "proxmox_convert_vm_to_template", `{"vmid":100}`), `running`)
		eqAt(t, env.call(t, "proxmox_convert_vm_to_template", `{"vmid":9000}`), "status", "already a template")
	})

	t.Run("deletes stopped VMs and refuses running ones unless asked to stop", func(t *testing.T) {
		env.mock.Reset()
		vmsTaskRoute(env, "DELETE", `^/nodes/pve2/qemu/101$`, "qmdestroy", "pve2")
		res := env.call(t, "proxmox_delete_vm", `{"vmid":101,"purge":true,"destroy_unreferenced_disks":true}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eqStr(t, w.Path, "/nodes/pve2/qemu/101")
		eq(t, w.Query, `{"purge":"1","destroy-unreferenced-disks":"1"}`)
		eqAt(t, res, "deleted", true)

		env.mock.Reset()
		match(t, env.errorText(t, "proxmox_delete_vm", `{"vmid":100}`), `is running; stop it first`)
		vmsNoWrites(t, env)

		vmsTaskRoute(env, "POST", `/qemu/100/status/stop$`, "qmstop", "pve1")
		vmsTaskRoute(env, "DELETE", `^/nodes/pve1/qemu/100$`, "qmdestroy", "pve1")
		env.call(t, "proxmox_delete_vm", `{"vmid":100,"stop":true}`)
		var writes []any
		for _, r := range env.mock.Writes() {
			writes = append(writes, r.Method+" "+r.Path)
		}
		eq(t, writes, []any{"POST " + vmsVM100 + "/status/stop", "DELETE " + vmsVM100})
	})
}

func TestVmsCloudInitKeysMonitor(t *testing.T) {
	env := startEnv(t)

	t.Run("regenerates the cloud-init drive", func(t *testing.T) {
		env.mock.Reset()
		res := env.call(t, "proxmox_regenerate_vm_cloudinit", `{"vmid":100}`)
		eqStr(t, env.mock.LastWrite().Method, "PUT")
		eqStr(t, env.mock.LastWrite().Path, vmsVM100+"/cloudinit")
		eqAt(t, res, "regenerated", true)
	})

	t.Run("converts text to QEMU key names", func(t *testing.T) {
		keys, err := vmsTextToKeys("Hi 2!\n")
		if err != nil {
			t.Fatal(err)
		}
		eq(t, js.Array(keys), `["shift-h","i","spc","2","shift-1","ret"]`)
		keys, _ = vmsTextToKeys("a_b:/")
		eq(t, js.Array(keys), `["a","shift-minus","b","shift-semicolon","slash"]`)
		_, err = vmsTextToKeys("é")
		match(t, errString(err), `Cannot type character "é"`)
	})

	t.Run("sends keys and text to a running VM", func(t *testing.T) {
		env.mock.Reset()
		res := env.call(t, "proxmox_send_vm_key", `{"vmid":100,"keys":["ctrl-alt-delete"],"text":"ls\n"}`)
		var sent []any
		for _, r := range env.mock.RequestsTo(vmsVM100+"/sendkey", "PUT") {
			sent = append(sent, r.Body.Get("key"))
		}
		eq(t, sent, `["ctrl-alt-delete","l","s","ret"]`)
		eqAt(t, res, "sent", 4)
		match(t, env.errorText(t, "proxmox_send_vm_key", `{"vmid":101,"keys":["ret"]}`), `stopped`)
		match(t, env.errorText(t, "proxmox_send_vm_key", `{"vmid":100}`), `keys and/or text`)
	})

	t.Run("runs monitor commands", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("POST", `/qemu/100/monitor$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			return "VM status: running (" + js.Str(p.Get("command")) + ")\n"
		})
		res := env.call(t, "proxmox_vm_monitor_command", `{"vmid":100,"command":"info status"}`)
		eqAt(t, res, "output", "VM status: running (info status)")
		eq(t, env.mock.LastWrite().Body, `{"command":"info status"}`)
	})
}

func TestVmsPermissionGates(t *testing.T) {
	env := startEnv(t)

	t.Run("needs delete permission to delete unusedN volumes and exec permission for hookscript", func(t *testing.T) {
		env.mock.Reset()
		s := connect(t, env.client, toolOptions("write"))
		raw := func(args string) string {
			return errorTextOf(t, callOn(t, s, "proxmox_update_vm_config", `{"vmid":100,`+args+`}`))
		}
		match(t, raw(`"delete":["unused0"]`), `delete permission`)
		match(t, raw(`"extra":{"delete":"net1,unused1"}`), "extra must not set `delete`")
		match(t, raw(`"extra":{"hookscript":"local:snippets/x.sh"}`), `exec permission`)
		createVM := func(extra string) string {
			return errorTextOf(t, callOn(t, s, "proxmox_create_vm", `{"node":"pve1","vmid":170,"extra":`+extra+`}`))
		}
		match(t, createVM(`{"hookscript":"local:snippets/x.sh"}`), `exec permission`)
		// A create with archive+force is a restore that overwrites the existing VM; extra can't turn it into one.
		match(t, createVM(`{"archive":"local:backup/vzdump-qemu-100.vma.zst","force":1}`), "extra must not set `archive`, `force`")
		match(t, createVM(`{"vmid":100}`), "extra must not set `vmid`")
		match(t, createVM(`{"live-restore":1,"unique":1}`), "`live-restore`, `unique`")
		match(t, raw(`"extra":{"force":1}`), "`force`")
		vmsNoWrites(t, env)

		// With every permission (the default env) the same delete goes through.
		vmsTaskRoute(env, "POST", `/qemu/100/config$`, "qmconfig", "pve1")
		env.mock.Reply("GET", `/qemu/100/pending$`, `[]`)
		env.call(t, "proxmox_update_vm_config", `{"vmid":100,"delete":["unused0"]}`)
		eqAt(t, env.mock.LastWrite().Body, "delete", "unused0")
	})

	t.Run("hides exec tools without exec permission", func(t *testing.T) {
		names := toolNames(t, connect(t, env.client, toolOptions("delete")))
		if names["proxmox_send_vm_key"] || names["proxmox_vm_monitor_command"] || !names["proxmox_unlink_vm_disk"] {
			t.Fatal("exec tools need PROXMOX_ALLOW_EXEC")
		}
		names = toolNames(t, connect(t, env.client, toolOptions("write")))
		if names["proxmox_unlink_vm_disk"] || names["proxmox_delete_vm"] || !names["proxmox_create_vm"] {
			t.Fatal("delete tools need PROXMOX_ALLOW_DELETES")
		}
	})
}
