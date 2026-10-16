package tools

import (
	"net/url"
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

const storageTestConfig = `[
  {"storage":"local","type":"dir","path":"/var/lib/vz","content":"iso,vztmpl,backup","prune-backups":"keep-last=3,keep-daily=7","digest":"d1"},
  {"storage":"local-lvm","type":"lvmthin","vgname":"pve","thinpool":"data","content":"images,rootdir","digest":"d1"},
  {"storage":"pbs","type":"pbs","server":"10.0.0.50","datastore":"main","username":"backup@pbs","password":"s3cret","encryption-key":"{\"kdf\":null}","content":"backup","shared":1,"nodes":"pve1","digest":"d1"}
]`

func storageFind(items any, field string, value any) any {
	arr, _ := items.([]any)
	for _, x := range arr {
		if at(x, field) == value {
			return x
		}
	}
	return js.Undef
}

func TestStorageTools(t *testing.T) {
	env := startEnv(t, map[string]string{"PROXMOX_DEFAULT_NODE": "pve1"})
	reset := func() {
		env.mock.Reset()
		env.mock.On("GET", `^/storage$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			all := js.Objs(js.MustParse(storageTestConfig))
			if p.Truthy("type") {
				all = filterObjs(all, func(s *Obj) bool { return s.Get("type") == p.Get("type") })
			}
			return js.Array(all)
		})
	}
	run := func(name string, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			reset()
			f(t)
		})
	}

	// ---------------------------------------------------------------- definitions

	run("lists storage merged with per-node status", func(t *testing.T) {
		res := env.call(t, "proxmox_list_storage", nil)
		eqAt(t, res, "total", 3)
		local := storageFind(at(res, "items"), "storage", "local")
		eqAt(t, local, "content", `["iso","vztmpl","backup"]`)
		eqAt(t, local, "prune_backups", `{"keep-last":"3","keep-daily":"7"}`)
		eq(t, pluck(at(local, "status"), "node"), `["pve1","pve2"]`)
		eqAt(t, local, "status.0.used_pct", 10)
		match(t, at(local, "status.0.usage"), `10 GiB of 100 GiB`)
		pbs := storageFind(at(res, "items"), "storage", "pbs")
		eqAt(t, pbs, "shared", true)
		eqAt(t, pbs, "nodes", `["pve1"]`)
		eqAt(t, pbs, "password", js.Undef)
	})

	run("filters storage by type, content and node, and redacts raw output", func(t *testing.T) {
		byType := env.call(t, "proxmox_list_storage", `{"type":"lvmthin"}`)
		eq(t, pluck(at(byType, "items"), "storage"), `["local-lvm"]`)
		eqAt(t, env.mock.RequestsTo("/storage", "GET")[0].Query, "type", "lvmthin")
		byContent := env.call(t, "proxmox_list_storage", `{"content":"backup"}`)
		eq(t, pluck(at(byContent, "items"), "storage"), `["local","pbs"]`)
		byNode := env.call(t, "proxmox_list_storage", `{"node":"pve2"}`)
		eq(t, pluck(at(byNode, "items"), "storage"), `["local"]`)
		raw := env.call(t, "proxmox_list_storage", `{"raw":true,"search":"pbs"}`)
		eqAt(t, raw, "items.0.password", "<redacted>")
		eqAt(t, raw, "items.0.encryption-key", "<redacted>")
	})

	run("gets one storage with config, status and node detail", func(t *testing.T) {
		env.mock.On("GET", `^/storage/pbs$`, func(*Obj, []string, proxmoxtest.Request) any { return js.MustParse(storageTestConfig).([]any)[2] })
		env.mock.Reply("GET", `^/nodes/pve1/storage/pbs/status$`, `{"type":"pbs","active":1,"enabled":1,"shared":1,"content":"backup","used":50,"total":100,"avail":50}`)
		res := env.call(t, "proxmox_get_storage", `{"storage":"pbs","node":"pve1"}`)
		eqAt(t, res, "summary.datastore", "main")
		eqAt(t, res, "config.password", "<redacted>")
		eqAt(t, res, "config.encryption-key", "<redacted>")
		eqAt(t, res, "node_status.active", true)
		eqAt(t, res, "node_status.available", "50 B")
	})

	run("creates storage with typed fields mapped to API names", func(t *testing.T) {
		env.mock.On("POST", `^/storage$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			return js.O("storage", p.Get("storage"), "type", p.Get("type"), "config", js.O("encryption-key", "secretkey"))
		})
		res := env.call(t, "proxmox_save_storage", `{"storage":"nas","type":"pbs","server":"10.0.0.60","datastore":"store","username":"u@pbs","password":"pw",
			"content":["backup"],"nodes":["pve1","pve2"],"encryption_key":"autogen","prune_backups":{"keep_last":3,"keep_daily":7,"keep_all":false},
			"max_protected_backups":-1,"port":8008}`)
		eqAt(t, res, "action", "created")
		eqAt(t, res, "result.config.encryption-key", "<redacted>")
		match(t, at(res, "note"), `/etc/pve/priv/storage/nas\.enc`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, "/storage")
		eqStr(t, w.Body.Get("storage"), "nas")
		eqStr(t, w.Body.Get("content"), "backup")
		eqStr(t, w.Body.Get("nodes"), "pve1,pve2")
		eqStr(t, w.Body.Get("encryption-key"), "autogen")
		eqStr(t, w.Body.Get("prune-backups"), "keep-all=0,keep-last=3,keep-daily=7")
		eqStr(t, w.Body.Get("max-protected-backups"), "-1")
		eqStr(t, w.Body.Get("port"), "8008")
	})

	run("updates existing storage and rejects create-only fields", func(t *testing.T) {
		res := env.call(t, "proxmox_save_storage", `{"storage":"local","content":["iso","backup"],"disable":true,"delete":["prune-backups"],"digest":"d1"}`)
		eqAt(t, res, "action", "updated")
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, "/storage/local")
		eq(t, w.Body, `{"content":"iso,backup","disable":"1","delete":"prune-backups","digest":"d1"}`)

		match(t, env.errorText(t, "proxmox_save_storage", `{"storage":"local","path":"/srv","type":"dir"}`), `type, path can't be changed`)
		match(t, env.errorText(t, "proxmox_save_storage", `{"storage":"new","path":"/srv"}`), "pass `type`")
	})

	run("deletes storage definitions", func(t *testing.T) {
		eqAt(t, env.call(t, "proxmox_delete_storage", `{"storage":"pbs"}`), "deleted", true)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["DELETE","/storage/pbs"]`)
	})

	// ---------------------------------------------------------------- content

	run("lists content with filters and summaries", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/storage/local/content$`, `[
			{"volid":"local:iso/debian.iso","content":"iso","format":"iso","size":658505728,"ctime":1700000000},
			{"volid":"local:backup/vzdump-qemu-100-2024.vma.zst","content":"backup","format":"vma.zst","size":1073741824,"vmid":100,"ctime":1700000000,"notes":"nightly","protected":1,"verification":{"state":"ok"}}]`)
		res := env.call(t, "proxmox_list_storage_content", `{"storage":"local","content":"iso","vmid":100}`)
		req := env.mock.RequestsTo("/nodes/pve1/storage/local/content", "GET")[0]
		eq(t, req.Query, js.O("content", "iso", "vmid", "100"))
		eqAt(t, res, "node", "pve1")
		backup := storageFind(at(res, "items"), "content", "backup")
		eq(t, backup, `{"volid":"local:backup/vzdump-qemu-100-2024.vma.zst","content":"backup","format":"vma.zst","size":"1 GiB","vmid":100,
			"ctime":"2023-11-14T22:13:20.000Z","notes":"nightly","protected":true,"verification":"ok"}`)
		eqAt(t, env.call(t, "proxmox_list_storage_content", `{"storage":"local","search":"debian"}`), "total", 1)
	})

	run("gets and updates volume attributes, deriving the storage from the volume ID", func(t *testing.T) {
		env.mock.On("GET", `^/nodes/pve1/storage/local/content/(.+)$`, func(_ *Obj, m []string, _ proxmoxtest.Request) any {
			p, _ := url.PathUnescape(m[1])
			return js.O("path", "/var/lib/vz/"+p, "format", "iso", "size", 1024, "used", 1024, "protected", 0)
		})
		v := env.call(t, "proxmox_get_volume", `{"volume":"local:iso/debian.iso"}`)
		eqAt(t, v, "size", "1 KiB")
		eqAt(t, v, "protected", false)
		eqStr(t, env.mock.RequestsTo(`^.*content/`, "GET")[0].Path, "/nodes/pve1/storage/local/content/local%3Aiso%2Fdebian.iso")

		env.call(t, "proxmox_update_volume", `{"volume":"local:backup/x.vma.zst","notes":"keep","protected":true}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eq(t, w.Body, `{"notes":"keep","protected":"1"}`)
		match(t, env.errorText(t, "proxmox_update_volume", `{"volume":"local:iso/x.iso"}`), `Nothing to change`)
		match(t, env.errorText(t, "proxmox_get_volume", `{"volume":"noprefix"}`), "Pass `storage`")
	})

	run("deletes volumes as a task", func(t *testing.T) {
		upid := env.mock.Upid("pve1", "imgdel", "local-lvm:vm-100-disk-1")
		env.mock.Reply("DELETE", `^/nodes/pve1/storage/local-lvm/content/`, upid)
		res := env.call(t, "proxmox_delete_volume", `{"volume":"local-lvm:vm-100-disk-1"}`)
		eqAt(t, res, "status", "ok")
		eqAt(t, res, "task.upid", upid)
		eqAt(t, env.call(t, "proxmox_delete_volume", `{"volume":"local-lvm:vm-100-disk-1","wait":false}`), "status", "started")
	})

	run("allocates disks", func(t *testing.T) {
		env.mock.On("POST", `^/nodes/pve2/storage/local-lvm/content$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			return "local-lvm:" + p.Str("filename")
		})
		res := env.call(t, "proxmox_allocate_disk", `{"node":"pve2","storage":"local-lvm","vmid":101,"filename":"vm-101-disk-2","size":"16G","format":"raw"}`)
		eqAt(t, res, "volid", "local-lvm:vm-101-disk-2")
		eq(t, env.mock.LastWrite().Body, js.O("vmid", "101", "filename", "vm-101-disk-2", "size", "16G", "format", "raw"))
	})

	// ---------------------------------------------------------------- downloads

	run("downloads from a URL with checksum options", func(t *testing.T) {
		upid := env.mock.Upid("pve1", "download", "local")
		env.mock.Reply("POST", `download-url$`, upid)
		res := env.call(t, "proxmox_download_to_storage", `{"storage":"local","url":"https://example.com/d.iso","content":"iso","filename":"d.iso","checksum":"abc","checksum_algorithm":"sha256","verify_certificates":false}`)
		eqAt(t, res, "status", "ok")
		eqAt(t, res, "volid", "local:iso/d.iso")
		w := env.mock.LastWrite()
		eqStr(t, w.Path, "/nodes/pve1/storage/local/download-url")
		eqStr(t, w.Body.Get("checksum-algorithm"), "sha256")
		eqStr(t, w.Body.Get("verify-certificates"), "0")
		match(t, env.errorText(t, "proxmox_download_to_storage", `{"storage":"local","url":"u","content":"iso","filename":"f","checksum":"x"}`), `checksum_algorithm`)
	})

	run("reports failed download tasks", func(t *testing.T) {
		upid := env.mock.Upid("pve1", "download", "local", proxmoxtest.Task{Status: "stopped", ExitStatus: "checksum mismatch", Log: []string{"downloading", "checksum mismatch"}})
		env.mock.Reply("POST", `download-url$`, upid)
		match(t, env.errorText(t, "proxmox_download_to_storage", `{"storage":"local","url":"u","content":"vztmpl","filename":"f.tar.zst"}`), `failed: checksum mismatch`)
	})

	run("lists and downloads appliance templates", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/aplinfo$`, `[
			{"template":"debian-12-standard_12.7-1_amd64.tar.zst","section":"system","os":"debian-12","version":"12.7-1","headline":"Debian 12","type":"lxc","package":"debian-12-standard"},
			{"template":"debian-12-turnkey-nextcloud_18.0-1_amd64.tar.gz","section":"turnkeylinux","os":"debian-12","headline":"TurnKey Nextcloud","type":"lxc"},
			{"template":"alpine-3.20-default_20240908_amd64.tar.xz","section":"system","os":"alpine","headline":"Alpine","type":"lxc"}]`)
		sys := env.call(t, "proxmox_list_appliance_templates", `{"section":"system"}`)
		eq(t, pluck(at(sys, "items"), "template"), `["alpine-3.20-default_20240908_amd64.tar.xz","debian-12-standard_12.7-1_amd64.tar.zst"]`)
		eqAt(t, env.call(t, "proxmox_list_appliance_templates", `{"search":"nextcloud"}`), "total", 1)

		upid := env.mock.Upid("pve1", "download", "local")
		env.mock.Reply("POST", `^/nodes/pve1/aplinfo$`, upid)
		res := env.call(t, "proxmox_download_appliance_template", `{"storage":"local","template":"alpine-3.20-default_20240908_amd64.tar.xz"}`)
		eqAt(t, res, "status", "ok")
		eq(t, env.mock.LastWrite().Body, `{"storage":"local","template":"alpine-3.20-default_20240908_amd64.tar.xz"}`)
	})

	// ---------------------------------------------------------------- stats and scans

	run("summarizes storage RRD data", func(t *testing.T) {
		env.mock.Reply("GET", `rrddata$`, `[{"time":1700000000,"used":100,"total":1000},{"time":1700000060},{"time":1700000120,"used":300,"total":1000},{"time":1700000180,"used":200,"total":1000}]`)
		res := env.call(t, "proxmox_get_storage_stats", `{"storage":"local","timeframe":"week","samples":2}`)
		eqAt(t, res, "points", 3)
		eqAt(t, res, "min_used", "100 B")
		eqAt(t, res, "max_used", "300 B")
		eqAt(t, res, "growth", "+100 B")
		eqAt(t, res, "samples.length", 2)
		eqAt(t, res, "current", "200 B of 1000 B (20%)")
		eqAt(t, env.mock.RequestsTo(`^.*rrddata`, "")[0].Query, "timeframe", "week")

		env.mock.Reply("GET", `rrddata$`, `[]`)
		eqAt(t, env.call(t, "proxmox_get_storage_stats", `{"storage":"local"}`), "points", 0)
	})

	run("scans for storage with type-specific parameters", func(t *testing.T) {
		env.mock.On("GET", `^/nodes/pve1/scan/(\w+)$`, func(_ *Obj, m []string, _ proxmoxtest.Request) any { return []any{js.O("kind", m[1])} })
		eqAt(t, env.call(t, "proxmox_scan_storage", `{"type":"nfs","server":"nas"}`), "count", 1)
		eq(t, env.mock.Last().Query, `{"server":"nas"}`)
		env.call(t, "proxmox_scan_storage", `{"type":"pbs","server":"pbs","username":"u@pbs","password":"p","port":8007}`)
		eqStr(t, env.mock.Last().Path, "/nodes/pve1/scan/pbs")
		eqStr(t, env.mock.Last().Query.Get("port"), "8007")
		env.call(t, "proxmox_scan_storage", `{"type":"lvmthin","vg":"pve"}`)
		eq(t, env.mock.Last().Query, `{"vg":"pve"}`)
		env.call(t, "proxmox_scan_storage", `{"type":"zfs"}`)
		eq(t, env.mock.Last().Query, `{}`)
		match(t, env.errorText(t, "proxmox_scan_storage", `{"type":"pbs","server":"x"}`), "requires `username`")
		match(t, env.errorText(t, "proxmox_scan_storage", `{"type":"iscsi"}`), "requires `portal`")
	})

	run("only allows local storage scans without write access", func(t *testing.T) {
		env.mock.On("GET", `^/nodes/pve1/scan/(\w+)$`, func(_ *Obj, m []string, _ proxmoxtest.Request) any { return []any{js.O("kind", m[1])} })
		ro := connect(t, env.client, toolOptions("read"))
		before := len(env.mock.Requests())
		for _, args := range []string{
			`{"type":"nfs","server":"attacker.example"}`,
			`{"type":"cifs","server":"10.0.0.9","username":"u","password":"p"}`,
			`{"type":"pbs","server":"pbs","username":"u@pbs","password":"p"}`,
			`{"type":"iscsi","portal":"10.0.0.9"}`,
		} {
			match(t, errorTextOf(t, callOn(t, ro, "proxmox_scan_storage", args)), `needs write access \(PROXMOX_ALLOW_WRITES=true\)`, args)
		}
		if n := len(env.mock.Requests()); n != before {
			t.Fatalf("%d requests reached the API", n-before)
		}
		if res := callOn(t, ro, "proxmox_scan_storage", `{"type":"zfs"}`); res.IsError {
			t.Fatalf("zfs scan failed: %s", resultText(res))
		}
		for _, tool := range listAll(t, ro) {
			if tool.Name == "proxmox_scan_storage" {
				match(t, tool.Description, `need write access`)
			}
		}
	})

	run("registers proxmox_init_disk_gpt only with delete access, like proxmox_wipe_disk", func(t *testing.T) {
		for _, tc := range []struct {
			level string
			want  bool
		}{{"write", false}, {"delete", true}} {
			names := toolNames(t, connect(t, env.client, toolOptions(tc.level)))
			if names["proxmox_init_disk_gpt"] != tc.want || names["proxmox_wipe_disk"] != tc.want {
				t.Fatalf("%s: init_disk_gpt %v, wipe_disk %v, want %v", tc.level, names["proxmox_init_disk_gpt"], names["proxmox_wipe_disk"], tc.want)
			}
		}
	})

	// ---------------------------------------------------------------- disks

	run("lists disks with health and wearout", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/disks/list$`, `[
			{"devpath":"/dev/sdb","type":"hdd","model":"WDC","serial":"S2","size":4000787030016,"rpm":7200,"health":"PASSED","wearout":"N/A","used":"ZFS","gpt":1,"osdid":-1,"vendor":"ATA     "},
			{"devpath":"/dev/nvme0n1","type":"nvme","model":"Samsung 980","serial":"S1","size":1000204886016,"rpm":0,"health":"PASSED","wearout":97,"used":"LVM","gpt":1,"osdid":-1}]`)
		res := env.call(t, "proxmox_list_disks", `{"type":"unused","include_partitions":true,"skip_smart":true}`)
		eq(t, env.mock.RequestsTo(`^.*disks/list`, "")[0].Query, `{"type":"unused","include-partitions":"1","skipsmart":"1"}`)
		eqAt(t, res, "count", 2)
		nvme, hdd := at(res, "disks.0"), at(res, "disks.1")
		eqAt(t, nvme, "devpath", "/dev/nvme0n1")
		eqAt(t, nvme, "wearout_pct", 3)
		eqAt(t, nvme, "rpm", js.Undef)
		eqAt(t, nvme, "used_by", "LVM")
		eqAt(t, hdd, "wearout_pct", js.Undef)
		eqAt(t, hdd, "vendor", "ATA")
		eqAt(t, hdd, "size", "3.64 TiB")
		eqAt(t, hdd, "osdid", js.Undef)
	})

	run("gets SMART data", func(t *testing.T) {
		env.mock.Reply("GET", `disks/smart$`, `{"health":"PASSED","type":"ata","attributes":[{"id":"  5","name":"Reallocated_Sector_Ct","value":100,"worst":100,"threshold":10,"raw":"0","fail":"-","flags":"PO--CK"}]}`)
		res := env.call(t, "proxmox_get_disk_smart", `{"disk":"/dev/sdb"}`)
		eqAt(t, res, "health", "PASSED")
		eqAt(t, res, "attributes.0", `{"id":"  5","name":"Reallocated_Sector_Ct","value":100,"worst":100,"threshold":10,"raw":"0"}`)
		eqStr(t, env.mock.RequestsTo(`^.*smart`, "")[0].Query.Get("disk"), "/dev/sdb")
	})

	run("lists and inspects ZFS pools", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/disks/zfs$`, `[{"name":"rpool","size":1000,"alloc":250,"free":750,"frag":5,"dedup":1,"health":"ONLINE"}]`)
		env.mock.Reply("GET", `^/nodes/pve1/disks/zfs/rpool$`, `{"name":"rpool","state":"DEGRADED","status":"One or more devices has been removed","action":"Online the device",
			"scan":"scrub repaired 0B in 00:10:00 with 0 errors","errors":"No known data errors",
			"children":[{"name":"rpool","state":"DEGRADED","read":0,"write":0,"cksum":0,"children":[{"name":"mirror-0","state":"DEGRADED","children":[
				{"name":"sda3","state":"ONLINE","read":0,"write":0,"cksum":0},{"name":"sdb3","state":"REMOVED","msg":"was /dev/sdb3"}]}]}]}`)
		list := env.call(t, "proxmox_list_zfs_pools", nil)
		eqAt(t, list, "pools.0", `{"name":"rpool","health":"ONLINE","usage":"250 B of 1000 B (25%)","free":"750 B","frag_pct":5,"dedup":1}`)
		pool := env.call(t, "proxmox_get_zfs_pool", `{"name":"rpool"}`)
		eqAt(t, pool, "state", "DEGRADED")
		match(t, at(pool, "scan"), `scrub`)
		eqAt(t, pool, "vdevs.0.children.0.children.1.state", "REMOVED")
		eqAt(t, pool, "vdevs.0.children.0.children.1.msg", "was /dev/sdb3")
	})

	run("lists LVM VGs, thin pools and directory mounts", func(t *testing.T) {
		env.mock.Reply("GET", `disks/lvm$`, `{"leaf":0,"children":[{"name":"pve","size":1000,"free":100,"leaf":0,"children":[{"name":"/dev/sda3","size":1000,"free":100,"leaf":1}]}]}`)
		env.mock.Reply("GET", `disks/lvmthin$`, `[{"lv":"data","vg":"pve","lv_size":800,"used":400,"metadata_size":10,"metadata_used":9}]`)
		env.mock.Reply("GET", `disks/directory$`, `[{"path":"/mnt/pve/backup","device":"/dev/sdc1","type":"ext4","options":"defaults","unitfile":"/etc/systemd/system/mnt-pve-backup.mount"}]`)
		lvm := env.call(t, "proxmox_list_lvm", nil)
		eqAt(t, lvm, "volume_groups.0.usage", "900 B of 1000 B (90%)")
		eqAt(t, lvm, "volume_groups.0.physical_volumes.0.name", "/dev/sda3")
		thin := env.call(t, "proxmox_list_lvm_thin", nil)
		eqAt(t, thin, "thin_pools.0", `{"name":"data","volume_group":"pve","usage":"400 B of 800 B (50%)","metadata_usage":"9 B of 10 B (90%)"}`)
		dirs := env.call(t, "proxmox_list_directory_mounts", nil)
		eqAt(t, dirs, "directories.0.filesystem", "ext4")
		eqAt(t, dirs, "directories.0.unit_file", "/etc/systemd/system/mnt-pve-backup.mount")
	})

	run("creates ZFS, LVM, thin and directory storage on disks", func(t *testing.T) {
		env.mock.On("POST", `^/nodes/pve1/disks/(\w+)$`, func(_ *Obj, m []string, _ proxmoxtest.Request) any { return env.mock.Upid("pve1", m[1]+"create", "x") })
		zfs := env.call(t, "proxmox_create_disk_storage", `{"kind":"zfs","name":"tank","devices":["/dev/sdb","/dev/sdc"],"raidlevel":"mirror","ashift":12,"compression":"lz4","add_storage":true}`)
		eqAt(t, zfs, "status", "ok")
		w := env.mock.LastWrite()
		eqStr(t, w.Path, "/nodes/pve1/disks/zfs")
		eq(t, w.Body, js.O("name", "tank", "devices", "/dev/sdb,/dev/sdc", "raidlevel", "mirror", "ashift", "12", "compression", "lz4", "add_storage", "1"))

		env.call(t, "proxmox_create_disk_storage", `{"kind":"zfs","name":"single","devices":["/dev/sdd"]}`)
		eqStr(t, env.mock.LastWrite().Body.Get("raidlevel"), "single")

		env.call(t, "proxmox_create_disk_storage", `{"kind":"directory","name":"backup","devices":["/dev/sde"],"filesystem":"xfs"}`)
		w = env.mock.LastWrite()
		eqStr(t, w.Path, "/nodes/pve1/disks/directory")
		eq(t, w.Body, `{"name":"backup","device":"/dev/sde","filesystem":"xfs"}`)

		env.call(t, "proxmox_create_disk_storage", `{"kind":"lvmthin","name":"thin","devices":["/dev/sdf"],"filesystem":"xfs"}`)
		eq(t, env.mock.LastWrite().Body, `{"name":"thin","device":"/dev/sdf"}`)

		match(t, env.errorText(t, "proxmox_create_disk_storage", `{"kind":"zfs","name":"x","devices":["/dev/a","/dev/b"]}`), `raidlevel`)
		match(t, env.errorText(t, "proxmox_create_disk_storage", `{"kind":"lvm","name":"x","devices":["/dev/a","/dev/b"]}`), `exactly one device`)
	})

	run("destroys disk storage with cleanup options", func(t *testing.T) {
		env.mock.On("DELETE", `^/nodes/pve1/disks/`, func(*Obj, []string, proxmoxtest.Request) any { return env.mock.Upid("pve1", "zfsremove", "tank") })
		eqAt(t, env.call(t, "proxmox_delete_disk_storage", `{"kind":"zfs","name":"tank","cleanup_config":true,"cleanup_disks":true}`), "status", "ok")
		w := env.mock.LastWrite()
		eqStr(t, w.Path, "/nodes/pve1/disks/zfs/tank")
		eq(t, w.Query, `{"cleanup-config":"1","cleanup-disks":"1"}`)
		env.call(t, "proxmox_delete_disk_storage", `{"kind":"lvmthin","name":"data","volume_group":"pve"}`)
		eq(t, env.mock.LastWrite().Query, `{"volume-group":"pve"}`)
		match(t, env.errorText(t, "proxmox_delete_disk_storage", `{"kind":"lvmthin","name":"data"}`), `volume_group`)
	})

	run("initializes GPT and wipes disks", func(t *testing.T) {
		env.mock.On("POST", `disks/initgpt$`, func(*Obj, []string, proxmoxtest.Request) any { return env.mock.Upid("pve1", "diskinit", "sdb") })
		env.mock.On("PUT", `disks/wipedisk$`, func(*Obj, []string, proxmoxtest.Request) any { return env.mock.Upid("pve1", "wipedisk", "sdb") })
		eqAt(t, env.call(t, "proxmox_init_disk_gpt", `{"disk":"/dev/sdb"}`), "status", "ok")
		eq(t, env.mock.LastWrite().Body, `{"disk":"/dev/sdb"}`)
		eqAt(t, env.call(t, "proxmox_wipe_disk", `{"disk":"/dev/sdb","wait":false}`), "status", "started")
		eqStr(t, env.mock.LastWrite().Method, "PUT")
	})

	run("requires a node on multi-node clusters without a default", func(t *testing.T) {
		other := startEnv(t)
		match(t, other.errorText(t, "proxmox_list_disks", nil), "pass `node`")
	})
}
