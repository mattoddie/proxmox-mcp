package tools

import (
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

const containersCT = "/nodes/pve1/lxc/200"

// containersUpid registers a route that starts a task.
func containersUpid(env *testEnv, method, pattern, node, typ, id string, task ...proxmoxtest.Task) {
	env.mock.On(method, pattern, func(*Obj, []string, proxmoxtest.Request) any { return env.mock.Upid(node, typ, id, task...) })
}

func TestContainersTools(t *testing.T) {
	env := startEnv(t)

	t.Run("lists only containers", func(t *testing.T) {
		env.mock.Reset()
		res := env.call(t, "proxmox_list_containers", nil)
		eqAt(t, res, "total", 1)
		eqAt(t, res, "items.0.vmid", 200)
		eqAt(t, res, "items.0.type", "container")
		eqAt(t, env.call(t, "proxmox_list_containers", `{"status":"stopped"}`), "total", 0)
	})

	t.Run("gets a container with status, config summary, live IPs and pending changes", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/nodes/pve1/lxc/200/status/current$`, `{"status":"running","name":"dns","vmid":200,"cpus":1,"cpu":0.05,"mem":134217728,"maxmem":536870912,
			"swap":0,"maxswap":536870912,"disk":1073741824,"maxdisk":8589934592,"uptime":7200,"pid":1234,"ha":{"managed":0}}`)
		env.mock.Reply("GET", `/lxc/200/pending$`, `[{"key":"memory","value":512,"pending":1024},{"key":"hostname","value":"dns"},
			{"key":"mp0","value":"local-lvm:vm-200-disk-1,mp=/data","delete":1}]`)
		env.mock.Reply("GET", `/lxc/200/interfaces$`, `[{"name":"lo","hwaddr":"00:00:00:00:00:00","inet":"127.0.0.1/8"},
			{"name":"eth0","hwaddr":"BC:24:11:00:00:02","inet":"192.168.1.53/24","inet6":"fe80::1/64"}]`)
		res := env.call(t, "proxmox_get_container", `{"vmid":200}`)
		eqAt(t, res, "vmid", 200)
		eqAt(t, res, "node", "pve1")
		eqAt(t, res, "status", "running")
		eqAt(t, res, "cpu_pct", 5)
		match(t, at(res, "memory"), `128 MiB of 512 MiB`)
		eqAt(t, res, "ha", js.Undef)
		eqAt(t, res, "ip_addresses", `[{"name":"eth0","hwaddr":"BC:24:11:00:00:02","addresses":["192.168.1.53/24","fe80::1/64"]}]`)
		eqAt(t, res, "config.hostname", "dns")
		eqAt(t, res, "config.unprivileged", true)
		eqAt(t, res, "config.memory", "512 MiB")
		eqAt(t, res, "config.features", `{"nesting":true}`)
		eqAt(t, res, "config.volumes.rootfs.volume", "local-lvm:vm-200-disk-0")
		eqAt(t, res, "config.nets.net0.ip", "dhcp")
		eqAt(t, res, "config.tags", `["infra"]`)
		eqAt(t, res, "pending_changes", `[{"key":"memory","current":512,"pending":1024},{"key":"mp0","current":"local-lvm:vm-200-disk-1,mp=/data","delete":true}]`)
	})

	t.Run("refuses VMs in container tools", func(t *testing.T) {
		env.mock.Reset()
		match(t, env.errorText(t, "proxmox_get_container", `{"vmid":100}`), `is a VM, not a container`)
	})

	t.Run("gets raw config with options, and the pending listing", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("GET", `/lxc/200/config$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			return js.O("hostname", "dns", "password", "x", "snapshot", p.Get("snapshot"), "current", p.Get("current"))
		})
		res := env.call(t, "proxmox_get_container_config", `{"vmid":200,"snapshot":"pre","current":true}`)
		eqAt(t, res, "config.snapshot", "pre")
		eqStr(t, at(res, "config.current"), "1")
		eqAt(t, res, "config.password", "<redacted>")
		env.mock.Reply("GET", `/lxc/200/pending$`, `[{"key":"cores","value":1,"pending":2},{"key":"hostname","value":"dns"},{"key":"password","pending":"hunter2"}]`)
		pend := env.call(t, "proxmox_get_container_config", `{"vmid":200,"pending":true}`)
		eqAt(t, pend, "pending_changes", `[{"key":"cores","current":1,"pending":2},{"key":"password","pending":"<redacted>"}]`)
		eqAt(t, pend, "config.1", `{"key":"hostname","value":"dns"}`)
	})

	t.Run("reads live interfaces and refuses stopped containers", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `/lxc/200/interfaces$`, `[{"name":"eth0","hardware-address":"aa","ip-addresses":[{"ip-address":"10.0.0.5","prefix":24,"ip-address-type":"inet"}]}]`)
		res := env.call(t, "proxmox_get_container_interfaces", `{"vmid":200}`)
		eqAt(t, res, "interfaces", `[{"name":"eth0","hwaddr":"aa","addresses":["10.0.0.5/24"]}]`)
		env.mock.Reply("GET", `^/cluster/resources$`, `[{"vmid":201,"type":"lxc","node":"pve2","status":"stopped","name":"off"}]`)
		match(t, env.errorText(t, "proxmox_get_container_interfaces", `{"vmid":201}`), `stopped; it must be running`)
	})

	t.Run("summarizes RRD stats", func(t *testing.T) {
		env.mock.Reset()
		var timeframe any
		env.mock.On("GET", `/lxc/200/rrddata$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			timeframe = p.Get("timeframe")
			return js.MustParse(`[{"time":1700000000,"cpu":0.1,"mem":1048576,"maxmem":4194304,"netin":1024,"netout":2048,"maxcpu":2},
				{"time":1700000060,"cpu":0.3,"mem":3145728,"maxmem":4194304,"netin":3072,"netout":2048,"maxcpu":2},{"time":1700000120}]`)
		})
		res := env.call(t, "proxmox_get_container_stats", `{"vmid":200,"timeframe":"day","points":true}`)
		eq(t, timeframe, "day")
		eqAt(t, res, "samples", 2)
		eqAt(t, res, "cpu_pct", `{"avg":20,"max":30,"latest":30}`)
		eqAt(t, res, "memory", `{"avg":"2 MiB","max":"3 MiB","latest":"3 MiB"}`)
		eqAt(t, res, "memory_total", "4 MiB")
		eqAt(t, res, "net_in.max", "3 KiB/s")
		eqAt(t, res, "cpus", 2)
		eqAt(t, res, "points.length", 2)
	})

	t.Run("checks features and migration preconditions", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("GET", `/lxc/200/feature$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			return js.O("hasFeature", pick(p.Get("feature") == "snapshot", 1, 0), "nodes", []any{"pve1"})
		})
		eqAt(t, env.call(t, "proxmox_check_container_feature", `{"vmid":200,"feature":"snapshot"}`), "available", true)
		eqAt(t, env.call(t, "proxmox_check_container_feature", `{"vmid":200,"feature":"clone"}`), "available", false)
		env.mock.On("GET", `/lxc/200/migrate$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			return js.O("running", 1, "allowed-nodes", []any{p.Get("target")}, "local-disks", []any{})
		})
		mig := env.call(t, "proxmox_check_container_migration", `{"vmid":200,"target":"pve2"}`)
		eqAt(t, mig, "allowed-nodes", `["pve2"]`)
	})

	t.Run("runs power actions as tasks with the right parameters", func(t *testing.T) {
		env.mock.Reset()
		containersUpid(env, "POST", `/lxc/200/status/shutdown$`, "pve1", "vzshutdown", "200")
		res := env.call(t, "proxmox_container_power", `{"vmid":200,"action":"shutdown","timeout":30,"force_stop":true}`)
		eqAt(t, res, "status", "ok")
		eqAt(t, res, "action", "shutdown")
		w := env.mock.LastWrite()
		eqStr(t, w.Path, containersCT+"/status/shutdown")
		eq(t, w.Body, `{"timeout":"30","forceStop":"1"}`)

		containersUpid(env, "POST", `/lxc/200/status/stop$`, "pve1", "vzstop", "200")
		started := env.call(t, "proxmox_container_power", `{"vmid":200,"action":"stop","overrule_shutdown":true,"wait":false}`)
		eqAt(t, started, "status", "started")
		match(t, at(started, "upid"), `^UPID:pve1:`)
		eq(t, env.mock.LastWrite().Body, `{"overrule-shutdown":"1"}`)
	})

	t.Run("reports failed tasks with the log", func(t *testing.T) {
		env.mock.Reset()
		containersUpid(env, "POST", `/lxc/200/status/reboot$`, "pve1", "vzreboot", "200", proxmoxtest.Task{Status: "stopped", ExitStatus: "boom", Log: []string{"something broke"}})
		text := env.errorText(t, "proxmox_container_power", `{"vmid":200,"action":"reboot"}`)
		match(t, text, `failed: boom`)
		match(t, text, `something broke`)
	})

	t.Run("creates a container with nextid, rootfs, mount points, nets and features", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/nextid$`, `"205"`)
		containersUpid(env, "POST", `^/nodes/pve1/lxc$`, "pve1", "vzcreate", "205")
		res := env.call(t, "proxmox_create_container", `{
			"node":"pve1","ostemplate":"local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst","hostname":"app","password":"pw",
			"ssh_public_keys":"ssh-ed25519 AAAA test","rootfs_storage":"local-lvm","rootfs_size_gb":16,
			"mount_points":[{"path":"/data","storage":"local-lvm","size_gb":50,"backup":true},{"path":"/media","volume":"/mnt/tank/media","ro":true}],
			"nets":[{"ip":"192.168.1.60/24","gw":"192.168.1.1","vlan":20,"firewall":true},{"bridge":"vmbr1","ip6":"auto"}],
			"cores":2,"memory":1024,"unprivileged":true,"features":{"nesting":true,"keyctl":false},"onboot":true,"start":true,
			"tags":["app","prod"],"extra":{"cpulimit":1}}`)
		eqAt(t, res, "vmid", 205)
		eqAt(t, res, "status", "ok")
		eqAt(t, res, "password", js.Undef)
		b := env.mock.LastWrite().Body
		eqStr(t, at(b, "vmid"), "205")
		eqAt(t, b, "rootfs", "local-lvm:16")
		eqAt(t, b, "mp0", "local-lvm:50,mp=/data,backup=1")
		eqAt(t, b, "mp1", "/mnt/tank/media,mp=/media,ro=1")
		eqAt(t, b, "net0", "name=eth0,bridge=vmbr0,ip=192.168.1.60/24,gw=192.168.1.1,firewall=1,tag=20")
		eqAt(t, b, "net1", "name=eth1,bridge=vmbr1,ip6=auto")
		eqAt(t, b, "features", "nesting=1,keyctl=0")
		eqAt(t, b, "ssh-public-keys", "ssh-ed25519 AAAA test")
		eqAt(t, b, "tags", "app;prod")
		eqStr(t, at(b, "unprivileged"), "1")
		eqStr(t, at(b, "start"), "1")
		eqStr(t, at(b, "cpulimit"), "1")
	})

	t.Run("adds a default DHCP interface and validates mount points on create", func(t *testing.T) {
		env.mock.Reset()
		containersUpid(env, "POST", `^/nodes/pve1/lxc$`, "pve1", "vzcreate", "300")
		env.call(t, "proxmox_create_container", `{"node":"pve1","vmid":300,"ostemplate":"local:vztmpl/alpine.tar.xz"}`)
		b := env.mock.LastWrite().Body
		eqAt(t, b, "net0", "name=eth0,bridge=vmbr0,ip=dhcp")
		eqAt(t, b, "rootfs", "local:8")

		env.call(t, "proxmox_create_container", `{"node":"pve1","vmid":301,"ostemplate":"local:vztmpl/alpine.tar.xz","nets":[]}`)
		eqAt(t, env.mock.LastWrite().Body, "net0", js.Undef)

		text := env.errorText(t, "proxmox_create_container", `{"node":"pve1","vmid":302,"ostemplate":"x","mount_points":[{"path":"/d","storage":"local-lvm"}]}`)
		match(t, text, `mp0: give either volume, or storage with size_gb`)
	})

	t.Run("clones a container", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/nextid$`, `"210"`)
		containersUpid(env, "POST", `/lxc/200/clone$`, "pve1", "vzclone", "200")
		res := env.call(t, "proxmox_clone_container", `{"vmid":200,"hostname":"dns2","full":true,"storage":"local-lvm","snapname":"base"}`)
		eqAt(t, res, "newid", 210)
		w := env.mock.LastWrite()
		eqStr(t, w.Path, containersCT+"/clone")
		eq(t, w.Body, `{"newid":"210","hostname":"dns2","full":"1","storage":"local-lvm","snapname":"base"}`)
		env.call(t, "proxmox_clone_container", `{"vmid":200,"newid":211,"storage":"local-lvm"}`)
		eq(t, env.mock.LastWrite().Body, `{"newid":"211","full":"1","storage":"local-lvm"}`)
	})

	t.Run("updates config, merging nets, mount points and features", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `/lxc/200/config$`, `{"net0":"name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:02,ip=dhcp,type=veth","mp0":"local-lvm:vm-200-disk-1,mp=/data,size=10G","features":"nesting=1"}`)
		env.mock.Reply("GET", `/lxc/200/pending$`, `[{"key":"swap","value":512,"pending":1024}]`)
		res := env.call(t, "proxmox_update_container_config", `{
			"vmid":200,"memory":2048,"swap":1024,
			"nets":{"net0":{"ip":"10.0.0.5/24","gw":"10.0.0.1"},"net1":{"bridge":"vmbr1"}},
			"mount_points":{"mp0":{"backup":false},"mp1":{"path":"/logs","storage":"local-lvm","size_gb":4}},
			"features":{"keyctl":true},"tags":["infra","dns"],"delete":["description"],"revert":["cores"],"digest":"def456"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, containersCT+"/config")
		eqAt(t, w.Body, "net0", "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:02,ip=10.0.0.5/24,type=veth,gw=10.0.0.1")
		eqAt(t, w.Body, "net1", "name=eth1,bridge=vmbr1,ip=dhcp")
		eqAt(t, w.Body, "mp0", "local-lvm:vm-200-disk-1,mp=/data,size=10G,backup=0")
		eqAt(t, w.Body, "mp1", "local-lvm:4,mp=/logs")
		eqAt(t, w.Body, "features", "nesting=1,keyctl=1")
		eqStr(t, at(w.Body, "memory"), "2048")
		eqAt(t, w.Body, "tags", "infra;dns")
		eqAt(t, w.Body, "delete", "description")
		eqAt(t, w.Body, "revert", "cores")
		eqAt(t, w.Body, "digest", "def456")
		eqAt(t, res, "pending_changes", `[{"key":"swap","current":512,"pending":1024}]`)
		match(t, at(res, "updated"), `"net0"`)
	})

	t.Run("rejects an empty update", func(t *testing.T) {
		env.mock.Reset()
		match(t, env.errorText(t, "proxmox_update_container_config", `{"vmid":200}`), `Nothing to change`)
	})

	t.Run("resizes a disk", func(t *testing.T) {
		env.mock.Reset()
		containersUpid(env, "PUT", `/lxc/200/resize$`, "pve1", "resize", "200")
		res := env.call(t, "proxmox_resize_container_disk", `{"vmid":200,"disk":"rootfs","size":"+4G"}`)
		eqAt(t, res, "status", "ok")
		eq(t, env.mock.LastWrite().Body, `{"disk":"rootfs","size":"+4G"}`)
		match(t, env.errorText(t, "proxmox_resize_container_disk", `{"vmid":200,"disk":"scsi0","size":"4G"}`), `must be rootfs or mpN at disk`)
	})

	t.Run("moves a volume", func(t *testing.T) {
		env.mock.Reset()
		containersUpid(env, "POST", `/lxc/200/move_volume$`, "pve1", "move_volume", "200")
		env.call(t, "proxmox_move_container_volume", `{"vmid":200,"volume":"mp0","storage":"ceph","delete_source":true}`)
		eq(t, env.mock.LastWrite().Body, `{"volume":"mp0","storage":"ceph","delete":"1"}`)
		env.call(t, "proxmox_move_container_volume", `{"vmid":200,"volume":"mp0","target_vmid":201,"target_volume":"mp3"}`)
		eq(t, env.mock.LastWrite().Body, `{"volume":"mp0","target-vmid":"201","target-volume":"mp3"}`)
		match(t, env.errorText(t, "proxmox_move_container_volume", `{"vmid":200,"volume":"mp0"}`), `Give storage`)
	})

	t.Run("migrates with restart and refuses running containers without it", func(t *testing.T) {
		env.mock.Reset()
		match(t, env.errorText(t, "proxmox_migrate_container", `{"vmid":200,"target":"pve2"}`), `restart=true`)
		match(t, env.errorText(t, "proxmox_migrate_container", `{"vmid":200,"target":"pve1","restart":true}`), `already on pve1`)
		containersUpid(env, "POST", `/lxc/200/migrate$`, "pve1", "vzmigrate", "200")
		res := env.call(t, "proxmox_migrate_container", `{"vmid":200,"target":"pve2","restart":true,"timeout":60}`)
		eqAt(t, res, "to", "pve2")
		eq(t, env.mock.LastWrite().Body, `{"target":"pve2","restart":"1","timeout":"60"}`)
	})

	t.Run("converts only stopped containers to templates", func(t *testing.T) {
		env.mock.Reset()
		match(t, env.errorText(t, "proxmox_convert_container_to_template", `{"vmid":200}`), `running; shut it down`)
		env.mock.Reply("GET", `^/cluster/resources$`, `[{"vmid":201,"type":"lxc","node":"pve2","status":"stopped","name":"off"}]`)
		res := env.call(t, "proxmox_convert_container_to_template", `{"vmid":201}`)
		eqAt(t, res, "template", true)
		eqStr(t, env.mock.LastWrite().Path, "/nodes/pve2/lxc/201/template")
	})

	t.Run("deletes a container, requiring force when running", func(t *testing.T) {
		env.mock.Reset()
		match(t, env.errorText(t, "proxmox_delete_container", `{"vmid":200}`), `force=true`)
		containersUpid(env, "DELETE", `^/nodes/pve1/lxc/200$`, "pve1", "vzdestroy", "200")
		res := env.call(t, "proxmox_delete_container", `{"vmid":200,"force":true,"purge":true,"destroy_unreferenced_disks":true}`)
		eqAt(t, res, "deleted", true)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eq(t, w.Query, `{"purge":"1","destroy-unreferenced-disks":"1","force":"1"}`)
	})
}

func TestContainersPermissionGates(t *testing.T) {
	env := startEnv(t)

	t.Run("needs delete permission to delete unusedN volumes and exec permission for hookscript", func(t *testing.T) {
		s := connect(t, env.client, toolOptions("write"))
		update := func(args string) string {
			return errorTextOf(t, callOn(t, s, "proxmox_update_container_config", `{"vmid":200,`+args+`}`))
		}
		match(t, update(`"delete":["unused0"]`), `delete permission`)
		for _, sep := range []string{`\u000b`, `\u0000`, `|`} {
			match(t, update(`"delete":["mp0`+sep+`unused0"]`), `delete permission`, sep)
		}
		match(t, update(`"extra":{"hookscript":"local:snippets/x.sh"}`), `exec permission`)
		match(t, errorTextOf(t, callOn(t, s, "proxmox_create_container", `{"node":"pve1","vmid":330,"ostemplate":"x","extra":{"hookscript":"y"}}`)), `exec permission`)
		// A create with restore+force restores a backup over the existing container; extra can't turn it into one.
		create := func(extra string) string {
			return errorTextOf(t, callOn(t, s, "proxmox_create_container", `{"node":"pve1","vmid":330,"ostemplate":"local:backup/vzdump-lxc-200.tar.zst","extra":`+extra+`}`))
		}
		match(t, create(`{"restore":1,"force":1}`), "extra must not set `restore`, `force`")
		match(t, create(`{"vmid":200}`), "extra must not set `vmid`")
		match(t, create(`{"ostemplate":"y","unique":1}`), "`ostemplate`, `unique`")
		// Typed fields that weren't given still count, e.g. delete on an update.
		match(t, update(`"extra":{"delete":"unused0"}`), "extra must not set `delete`")
		if w := env.mock.LastWrite(); w.Method != "" {
			t.Fatalf("unexpected write %s %s", w.Method, w.Path)
		}
	})
}
