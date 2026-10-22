package tools

import (
	"encoding/base64"
	"sort"
	"strings"
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

const (
	backupTestVMA   = "local:backup/vzdump-qemu-100-2025_01_02-02_00_00.vma.zst"
	backupTestTAR   = "local:backup/vzdump-lxc-200-2025_01_01-02_00_00.tar.zst"
	backupTestPBSVM = "pbs:backup/vm/101/2025-01-03T02:00:00Z"
	backupTestJob   = `{"id":"backup-daily","type":"vzdump","schedule":"02:00","enabled":1,"storage":"local","mode":"snapshot","vmid":"100,101,200","compress":"zstd",
		"prune-backups":{"keep-last":"3","keep-daily":"7"},"notes-template":"{{guestname}}","next-run":1735783200,"fleecing":{"enabled":1,"storage":"local-lvm"}}`
)

func backupTestNoWrites(t *testing.T, env *testEnv) {
	t.Helper()
	if w := env.mock.LastWrite(); w.Method != "" {
		t.Fatalf("unexpected write %s %s", w.Method, w.Path)
	}
}

func backupTestUpidRoute(env *testEnv) proxmoxtest.Handler {
	return func(_ *Obj, m []string, _ proxmoxtest.Request) any { return env.mock.Upid(m[1], "vzdump", "") }
}

func TestBackupTools(t *testing.T) {
	env := startEnv(t)

	t.Run("detects guest types from backup volumes", func(t *testing.T) {
		eqStr(t, backupGuestType(backupTestVMA), "qemu")
		eqStr(t, backupGuestType(backupTestTAR), "lxc")
		eqStr(t, backupGuestType(backupTestPBSVM), "qemu")
		eqStr(t, backupGuestType("pbs:backup/ct/200/2025-01-03T02:00:00Z"), "lxc")
		eqStr(t, backupGuestType(js.O("volid", "x:y", "subtype", "lxc")), "lxc")
		eqStr(t, backupGuestType("nfs:backup/something.tar"), "")
	})

	// ---------------------------------------------------------------- jobs

	t.Run("lists backup jobs", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/backup$`, `[`+backupTestJob+`,
			{"id":"weekly-all","schedule":"sun 01:00","all":1,"exclude":"9000","storage":"pbs","enabled":0,"prune-backups":"keep-weekly=4"},
			{"id":"pool-job","schedule":"daily","pool":"production","storage":"pbs","mailto":"a@b.c"}]`)
		r := env.call(t, "proxmox_list_backup_jobs", nil)
		eqAt(t, r, "total", 3)
		eqAt(t, r, "items.0", `{"id":"backup-daily","enabled":true,"schedule":"02:00","next_run":"2025-01-02T02:00:00.000Z","storage":"local","mode":"snapshot",
			"selection":{"vmids":[100,101,200]},"compress":"zstd","retention":{"keep_last":3,"keep_daily":7},"notes_template":"{{guestname}}"}`)
		eqAt(t, r, "items.2.selection", `{"all":true,"exclude":[9000]}`)
		eqAt(t, r, "items.2.enabled", false)
		eqAt(t, r, "items.2.retention", `{"keep_weekly":4}`)
		eqAt(t, r, "items.1.selection", `{"pool":"production"}`)
		eqAt(t, env.call(t, "proxmox_list_backup_jobs", `{"search":"pbs"}`), "total", 2)
	})

	t.Run("gets a job with included volumes", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("GET", `^/cluster/backup/backup-daily$`, func(*Obj, []string, proxmoxtest.Request) any {
			j := js.MustObj(backupTestJob)
			j.Delete("id")
			return j
		})
		env.mock.Reply("GET", `^/cluster/backup/backup-daily/included_volumes$`, `{"children":[{"id":100,"name":"web","type":"qemu","children":[
			{"id":"scsi0","name":"local-lvm:vm-100-disk-0","included":1,"reason":"default"},
			{"id":"scsi1","name":"local-lvm:vm-100-disk-1","included":0,"reason":"disabled"}]}]}`)
		r := env.call(t, "proxmox_get_backup_job", `{"id":"backup-daily"}`)
		eqAt(t, r, "id", "backup-daily")
		eqAt(t, r, "storage", "local")
		eqAt(t, r, "guests", `[{"vmid":100,"name":"web","type":"vm","volumes":[
			{"id":"scsi0","name":"local-lvm:vm-100-disk-0","included":true,"reason":"default"},
			{"id":"scsi1","name":"local-lvm:vm-100-disk-1","included":false,"reason":"disabled"}]}]`)
		raw := env.call(t, "proxmox_get_backup_job", `{"id":"backup-daily","raw":true,"include_volumes":false}`)
		eqAt(t, raw, "job.compress", "zstd")
		eqAt(t, raw, "guests", js.Undef)
	})

	t.Run("creates a backup job", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/backup$`, `[]`)
		r := env.call(t, "proxmox_save_backup_job", `{"id":"nightly","schedule":"daily","storage":"local","vmids":[100,200],"mode":"snapshot","compress":"zstd",
			"retention":{"keep_last":3,"keep_weekly":2,"keep_all":false},"notes_template":"{{guestname}}","remove_old":true,"repeat_missed":true,
			"extra":{"pbs-change-detection-mode":"metadata"}}`)
		eq(t, r, `{"id":"nightly","status":"created"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, "/cluster/backup")
		eq(t, w.Body, `{"id":"nightly","schedule":"daily","storage":"local","vmid":"100,200","mode":"snapshot","compress":"zstd",
			"prune-backups":"keep-all=0,keep-last=3,keep-weekly=2","remove":"1","notes-template":"{{guestname}}","repeat-missed":"1",
			"pbs-change-detection-mode":"metadata"}`)
	})

	t.Run("requires a selection for new jobs", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/backup$`, `[]`)
		match(t, env.errorText(t, "proxmox_save_backup_job", `{"schedule":"daily"}`), `needs a selection`)
	})

	t.Run("updates a job and swaps its selection", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/backup$`, `[`+backupTestJob+`]`)
		r := env.call(t, "proxmox_save_backup_job", `{"id":"backup-daily","all":true,"exclude":[9000],"enabled":false,"delete":["comment"]}`)
		eq(t, r, `{"id":"backup-daily","status":"updated"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, "/cluster/backup/backup-daily")
		eqStr(t, at(w.Body, "all"), "1")
		eqStr(t, at(w.Body, "exclude"), "9000")
		eqStr(t, at(w.Body, "enabled"), "0")
		del := strings.Split(js.Str(w.Body.Get("delete")), ",")
		sort.Strings(del)
		eq(t, js.V(del), `["comment","pool","vmid"]`)

		env.call(t, "proxmox_save_backup_job", `{"id":"backup-daily","schedule":"03:00"}`)
		eq(t, env.mock.LastWrite().Body, `{"schedule":"03:00"}`)
	})

	t.Run("deletes a job", func(t *testing.T) {
		env.mock.Reset()
		env.call(t, "proxmox_delete_backup_job", `{"id":"backup-daily"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eqStr(t, w.Path, "/cluster/backup/backup-daily")
	})

	t.Run("runs a VMID-list job per node with the job's settings", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("GET", `^/cluster/backup/backup-daily$`, func(*Obj, []string, proxmoxtest.Request) any {
			return js.MustObj(backupTestJob).Set("vmid", "100,101,200,555")
		})
		env.mock.On("POST", `^/nodes/([^/]+)/vzdump$`, backupTestUpidRoute(env))
		r := env.call(t, "proxmox_run_backup_job", `{"id":"backup-daily"}`)
		eqAt(t, r, "tasks.length", 2)
		for _, s := range pluck(at(r, "tasks"), "status") {
			eqStr(t, s, "ok")
		}
		byNode := map[string]*Obj{}
		for _, p := range env.mock.RequestsTo(`^.*/vzdump$`, "POST") {
			byNode[strings.Split(p.Path, "/")[2]] = p.Body
		}
		eqAt(t, byNode["pve1"], "vmid", "100,200")
		eqStr(t, at(byNode["pve2"], "vmid"), "101")
		eqAt(t, byNode["pve1"], "storage", "local")
		eqAt(t, byNode["pve1"], "prune-backups", "keep-last=3,keep-daily=7")
		eqAt(t, byNode["pve1"], "fleecing", "enabled=1,storage=local-lvm")
		for _, k := range []string{"id", "schedule", "enabled", "next-run", "type"} {
			eqAt(t, byNode["pve1"], k, js.Undef, k)
		}
	})

	t.Run("runs an all-guests job on each node with guests", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/backup/weekly$`, `{"id":"weekly","all":1,"exclude":"101","storage":"local","schedule":"sun 01:00"}`)
		env.mock.On("POST", `^/nodes/([^/]+)/vzdump$`, backupTestUpidRoute(env))
		r := env.call(t, "proxmox_run_backup_job", `{"id":"weekly","wait":false}`)
		// pve2 only has 101, which is excluded.
		eqAt(t, r, "tasks.length", 1)
		eqAt(t, r, "tasks.0.node", "pve1")
		eqAt(t, r, "tasks.0.status", "started")
		w := env.mock.LastWrite()
		eqStr(t, at(w.Body, "all"), "1")
		eqStr(t, at(w.Body, "exclude"), "101")
	})

	t.Run("lists guests without a backup job", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/backup-info/not-backed-up$`, `[{"vmid":200,"type":"lxc","name":"dns"},{"vmid":101,"type":"qemu"}]`)
		eq(t, env.call(t, "proxmox_list_unbacked_guests", nil), `{"total":2,"returned":2,"items":[
			{"vmid":101,"name":"db","type":"vm","node":"pve2","status":"stopped"},
			{"vmid":200,"name":"dns","type":"container","node":"pve1","status":"running"}]}`)
	})

	// ---------------------------------------------------------------- ad-hoc backups

	t.Run("backs up guests grouped by node", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("POST", `^/nodes/([^/]+)/vzdump$`, func(_ *Obj, m []string, _ proxmoxtest.Request) any {
			if m[1] == "pve2" {
				return env.mock.Upid(m[1], "vzdump", "", proxmoxtest.Task{Status: "stopped", ExitStatus: "job errors", Log: []string{"ERROR: Backup of VM 101 failed"}})
			}
			return env.mock.Upid(m[1], "vzdump", "")
		})
		r := env.call(t, "proxmox_backup_guests", `{"vmids":[100,200,101],"storage":"local","mode":"stop","compress":"zstd","notes_template":"pre-upgrade","protected":true,"remove_old":false}`)
		var pve1 *Obj
		for _, p := range env.mock.RequestsTo(`^.*/vzdump$`, "POST") {
			if p.Path == "/nodes/pve1/vzdump" {
				pve1 = p.Body
			}
		}
		eq(t, pve1, `{"storage":"local","mode":"stop","compress":"zstd","notes-template":"pre-upgrade","protected":"1","remove":"0","vmid":"100,200"}`)
		eqAt(t, r, "status", "partially failed")
		var failed any
		for _, task := range at(r, "tasks").([]any) {
			if at(task, "node") == "pve2" {
				failed = task
			}
		}
		eqAt(t, failed, "status", "failed")
		match(t, at(failed, "error"), `job errors`)
	})

	t.Run("rejects unknown VMIDs before starting backups", func(t *testing.T) {
		env.mock.Reset()
		match(t, env.errorText(t, "proxmox_backup_guests", `{"vmids":[100,555]}`), `No VM or container with ID 555`)
		if n := len(env.mock.RequestsTo(`^.*/vzdump$`, "POST")); n != 0 {
			t.Fatalf("%d vzdump requests", n)
		}
	})

	// ---------------------------------------------------------------- volumes

	mockContent := func(t *testing.T) {
		env.mock.On("GET", `^/nodes/(pve\d)/storage/local/content$`, func(p *Obj, m []string, _ proxmoxtest.Request) any {
			if p.Get("content") != "backup" {
				t.Errorf("content=%s", js.Str(p.Get("content")))
			}
			if m[1] == "pve2" {
				return []any{}
			}
			return js.MustParse(`[{"volid":"` + backupTestTAR + `","vmid":200,"format":"tar.zst","size":104857600,"ctime":1735696800,"subtype":"lxc","content":"backup"},
				{"volid":"` + backupTestVMA + `","vmid":100,"format":"vma.zst","size":2147483648,"ctime":1735783200,"notes":"nightly","protected":1,"content":"backup"}]`)
		})
	}

	t.Run("lists backups across storages, newest first", func(t *testing.T) {
		env.mock.Reset()
		mockContent(t)
		r := env.call(t, "proxmox_list_backups", nil)
		eqAt(t, r, "total", 2)
		eqAt(t, r, "total_size", "2.10 GiB")
		eqAt(t, r, "items.0", js.O("volid", backupTestVMA, "storage", "local", "node", "pve1", "vmid", 100, "type", "vm", "time", "2025-01-02T02:00:00.000Z",
			"size", "2 GiB", "size_bytes", 2147483648, "format", "vma.zst", "notes", "nightly", "protected", true))
		eqAt(t, r, "items.1.type", "container")
		// Only storages with backup content were queried (not local-lvm).
		if n := len(env.mock.RequestsTo(`^.*local-lvm`, "")); n != 0 {
			t.Fatalf("%d local-lvm requests", n)
		}
		if n := len(env.mock.RequestsTo(`^.*/storage/local/content$`, "")); n != 2 {
			t.Fatalf("%d content requests", n)
		}

		eq(t, pluck(at(env.call(t, "proxmox_list_backups", `{"type":"container"}`), "items"), "vmid"), `[200]`)
		eqAt(t, env.call(t, "proxmox_list_backups", `{"older_than_days":1}`), "total", 2)
		eqAt(t, env.call(t, "proxmox_list_backups", `{"newer_than_days":1}`), "total", 0)
		eqAt(t, env.call(t, "proxmox_list_backups", `{"vmid":100,"node":"pve1"}`), "total", 1)
		reqs := env.mock.RequestsTo("/nodes/pve1/storage/local/content", "")
		eqStr(t, at(reqs[len(reqs)-1].Query, "vmid"), "100")
	})

	t.Run("reports storages that fail to list instead of failing", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("GET", `^/nodes/pve2/storage/local/content$`, func(*Obj, []string, proxmoxtest.Request) any {
			return proxmoxtest.Error(500, "storage offline")
		})
		env.mock.Reply("GET", `^/nodes/pve1/storage/local/content$`, `[]`)
		r := env.call(t, "proxmox_list_backups", nil)
		eqAt(t, r, "total", 0)
		eqAt(t, r, "errors.length", 1)
		eqAt(t, r, "errors.0.node", "pve2")
	})

	t.Run("extracts the config from a backup with secrets redacted", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("GET", `^/nodes/pve1/vzdump/extractconfig$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			if p.Get("volume") != backupTestVMA {
				t.Errorf("volume=%s", js.Str(p.Get("volume")))
			}
			return "#my%20web%20server\n#100% done\n#qmdump#map:scsi0:drive-scsi0:local-lvm:raw:\ncores: 4\nmemory: 4096\ncipassword: hunter2\nscsi0: local-lvm:vm-100-disk-0,size=32G\n\n[PENDING]\ncores: 8\n\n[pre-upgrade]\ncores: 2\n\n[special:cloudinit]\nname: web\n"
		})
		r := env.call(t, "proxmox_get_backup_config", js.O("volid", backupTestVMA, "node", "pve1"))
		eqAt(t, r, "type", "vm")
		eqStr(t, at(r, "config.cores"), "4")
		eqAt(t, r, "config.description", "my web server\n100% done")
		eqAt(t, r, "config.cipassword", "<redacted>")
		eqAt(t, r, "snapshots", `["pre-upgrade"]`)
		raw := env.call(t, "proxmox_get_backup_config", js.O("volid", backupTestVMA, "node", "pve1", "raw", true))
		match(t, at(raw, "config"), `cipassword: <redacted>`)
		noMatch(t, at(raw, "config"), `hunter2`)
	})

	t.Run("asks for a node when a local storage exists on several nodes", func(t *testing.T) {
		env.mock.Reset()
		match(t, env.errorText(t, "proxmox_get_backup_config", js.O("volid", backupTestVMA)), `local to several nodes \(pve1, pve2\)`)
	})

	t.Run("restores a VM backup to a new VMID", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("POST", `^/nodes/pve1/qemu$`, func(*Obj, []string, proxmoxtest.Request) any { return env.mock.Upid("pve1", "qmrestore", "150") })
		r := env.call(t, "proxmox_restore_backup", js.O("volid", backupTestVMA, "vmid", 150, "node", "pve1", "storage", "local-lvm", "unique", true, "start", true))
		eqAt(t, r, "status", "ok")
		eqAt(t, r, "type", "vm")
		w := env.mock.LastWrite()
		eqStr(t, w.Path, "/nodes/pve1/qemu")
		eq(t, w.Body, js.O("vmid", "150", "storage", "local-lvm", "unique", "1", "start", "1", "archive", backupTestVMA))
	})

	t.Run("restores a container backup", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("POST", `^/nodes/pve1/lxc$`, func(*Obj, []string, proxmoxtest.Request) any { return env.mock.Upid("pve1", "vzrestore", "250") })
		env.call(t, "proxmox_restore_backup", js.O("volid", backupTestTAR, "vmid", 250, "node", "pve1", "unprivileged", true))
		w := env.mock.LastWrite()
		eqStr(t, w.Path, "/nodes/pve1/lxc")
		eq(t, w.Body, js.O("vmid", "250", "ostemplate", backupTestTAR, "restore", "1", "unprivileged", "1"))
	})

	t.Run("guards restores over existing guests", func(t *testing.T) {
		env.mock.Reset()
		match(t, env.errorText(t, "proxmox_restore_backup", js.O("volid", backupTestVMA, "vmid", 100, "node", "pve1")), `in use by VM "web".*force=true`)
		match(t, env.errorText(t, "proxmox_restore_backup", js.O("volid", backupTestVMA, "vmid", 100, "force", true)), `is running; stop it`)
		match(t, env.errorText(t, "proxmox_restore_backup", js.O("volid", backupTestTAR, "vmid", 101, "force", true)), `is a VM but the backup is a container`)
		match(t, env.errorText(t, "proxmox_restore_backup", js.O("volid", backupTestTAR, "vmid", 250, "live_restore", true, "node", "pve1")), `only applies to VM`)
		backupTestNoWrites(t, env)

		// Overwriting a stopped VM goes to the node it lives on.
		env.mock.On("POST", `^/nodes/pve2/qemu$`, func(*Obj, []string, proxmoxtest.Request) any { return env.mock.Upid("pve2", "qmrestore", "101") })
		env.call(t, "proxmox_restore_backup", js.O("volid", backupTestPBSVM, "vmid", 101, "force", true))
		w := env.mock.LastWrite()
		eqStr(t, w.Path, "/nodes/pve2/qemu")
		eqStr(t, at(w.Body, "force"), "1")
	})

	t.Run("refuses to overwrite without delete permission", func(t *testing.T) {
		env.mock.Reset()
		s := connect(t, env.client, toolOptions("write"))
		match(t, errorTextOf(t, callOn(t, s, "proxmox_restore_backup", js.O("volid", backupTestPBSVM, "vmid", 101, "force", true))), `needs delete permission`)
	})

	t.Run("doesn't let extra swap the target VMID or add force past the overwrite checks", func(t *testing.T) {
		env.mock.Reset()
		s := connect(t, env.client, toolOptions("write"))
		call := func(args *Obj) string {
			return errorTextOf(t, callOn(t, s, "proxmox_restore_backup", args))
		}
		// VMID 150 is free, so without the check this would restore over the stopped VM 101 with force.
		match(t, call(js.O("volid", backupTestPBSVM, "vmid", 150, "node", "pve2", "extra", js.O("vmid", 101, "force", 1))), "extra must not set `vmid`, `force`")
		match(t, call(js.O("volid", backupTestTAR, "vmid", 250, "node", "pve1", "extra", js.O("restore", 0))), "extra must not set `restore`")
		match(t, call(js.O("volid", backupTestVMA, "vmid", 150, "node", "pve1", "extra", js.O("archive", backupTestPBSVM))), "extra must not set `archive`")
		// Reserved even where the guest type doesn't use them as typed arguments.
		match(t, call(js.O("volid", backupTestTAR, "vmid", 250, "node", "pve1", "extra", js.O("archive", backupTestVMA))), "extra must not set `archive`")
		match(t, call(js.O("volid", backupTestVMA, "vmid", 150, "node", "pve1", "extra", js.O("unique", 1, "storage", "local-lvm"))), "`unique`, `storage`")
		match(t, call(js.O("volid", backupTestVMA, "vmid", 150, "extra", js.O("force", 1))), "extra must not set `force`")
		backupTestNoWrites(t, env)
	})

	t.Run("doesn't let extra change the guest selection of a backup", func(t *testing.T) {
		env.mock.Reset()
		match(t, env.errorText(t, "proxmox_backup_guests", `{"vmids":[100],"extra":{"all":1}}`), "extra must not set `all`")
		match(t, env.errorText(t, "proxmox_backup_guests", `{"vmids":[100],"extra":{"vmid":"101"}}`), "extra must not set `vmid`")
		env.mock.Reply("GET", `^/cluster/backup$`, `[]`)
		match(t, env.errorText(t, "proxmox_save_backup_job", `{"id":"x","extra":{"vmid":"100"}}`), `needs a selection`)
		match(t, env.errorText(t, "proxmox_save_backup_job", `{"id":"x","all":true,"extra":{"vmid":"100"}}`), "extra must not set `vmid`")
		backupTestNoWrites(t, env)
	})

	t.Run("needs exec permission for hook scripts", func(t *testing.T) {
		env.mock.Reset()
		s := connect(t, env.client, toolOptions("write"))
		call := func(name string, args any) string { return errorTextOf(t, callOn(t, s, name, args)) }
		match(t, call("proxmox_backup_guests", `{"vmids":[100],"extra":{"script":"/root/hook.sh"}}`), `exec permission`)
		match(t, call("proxmox_save_backup_job", `{"id":"x","all":true,"extra":{"script":"/root/hook.sh"}}`), `exec permission`)
		match(t, call("proxmox_restore_backup", js.O("volid", backupTestVMA, "vmid", 150, "extra", js.O("hookscript", "local:snippets/h.sh"))), `exec permission`)
		backupTestNoWrites(t, env)
	})

	t.Run("updates notes and protection", func(t *testing.T) {
		env.mock.Reset()
		env.call(t, "proxmox_update_backup", js.O("volid", backupTestVMA, "node", "pve1", "notes", "keep me", "protected", true))
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, "/nodes/pve1/storage/local/content/"+js.EncodeURIComponent(backupTestVMA))
		eq(t, w.Body, `{"notes":"keep me","protected":"1"}`)
		match(t, env.errorText(t, "proxmox_update_backup", js.O("volid", backupTestVMA, "node", "pve1")), `Nothing to change`)
	})

	t.Run("deletes a backup", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("DELETE", `/content/`, func(*Obj, []string, proxmoxtest.Request) any { return env.mock.Upid("pve1", "imgdel", "") })
		r := env.call(t, "proxmox_delete_backup", js.O("volid", backupTestVMA, "node", "pve1"))
		eqAt(t, r, "status", "ok")
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eqStr(t, w.Path, "/nodes/pve1/storage/local/content/"+js.EncodeURIComponent(backupTestVMA))
		// Volume IDs without a storage are rejected.
		match(t, env.errorText(t, "proxmox_delete_backup", `{"volid":"nope","node":"pve1"}`), `Not a volume ID`)
	})

	t.Run("previews and runs pruning", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/nodes/pve1/storage/local/prunebackups$`, `[
			{"volid":"`+backupTestTAR+`","vmid":200,"type":"lxc","ctime":1735696800,"mark":"remove"},
			{"volid":"`+backupTestVMA+`","vmid":100,"type":"qemu","ctime":1735783200,"mark":"protected"}]`)
		p := env.call(t, "proxmox_preview_backup_prune", `{"storage":"local","node":"pve1","retention":{"keep_last":1},"type":"container"}`)
		eqAt(t, p, "remove", 1)
		eqAt(t, p, "protected", 1)
		eqAt(t, p, "would_remove", []any{backupTestTAR})
		eqAt(t, p, "items.0.volid", backupTestVMA)
		eq(t, env.mock.RequestsTo("/nodes/pve1/storage/local/prunebackups", "GET")[0].Query, `{"prune-backups":"keep-last=1","type":"lxc"}`)

		env.mock.On("DELETE", `prunebackups$`, func(*Obj, []string, proxmoxtest.Request) any { return env.mock.Upid("pve1", "prunebackups", "local") })
		r := env.call(t, "proxmox_prune_backups", `{"storage":"local","node":"pve1","vmid":200}`)
		eqAt(t, r, "status", "ok")
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eq(t, w.Query, `{"vmid":"200"}`)
	})

	t.Run("reads vzdump defaults", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("GET", `^/nodes/pve1/vzdump/defaults$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			return js.O("storage", or(p.Get("storage"), "local"), "mode", "snapshot", "prune-backups", "keep-last=5", "mailto", "root")
		})
		r := env.call(t, "proxmox_get_vzdump_defaults", `{"node":"pve1","storage":"pbs"}`)
		eqAt(t, r, "node", "pve1")
		eqAt(t, r, "storage", "pbs")
		eqAt(t, r, "retention", `{"keep_last":5}`)
	})

	t.Run("browses files in a PBS backup", func(t *testing.T) {
		env.mock.Reset()
		b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
		env.mock.On("GET", `^/cluster/resources$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			if p.Get("type") == "storage" {
				return js.MustParse(`[{"type":"storage","storage":"pbs","node":"pve1","shared":1,"status":"available","content":"backup"},
					{"type":"storage","storage":"pbs","node":"pve2","shared":1,"status":"available","content":"backup"}]`)
			}
			return []any{}
		})
		env.mock.On("GET", `^/nodes/pve1/storage/pbs/file-restore/list$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			if p.Get("volume") != backupTestPBSVM {
				t.Errorf("volume=%s", js.Str(p.Get("volume")))
			}
			if p.Get("filepath") == "/" {
				return []any{js.O("text", "drive-scsi0.img.fidx", "filepath", b64("/drive-scsi0.img.fidx"), "type", "v", "leaf", false)}
			}
			if got := string(backupDecodeBase64(js.Str(p.Get("filepath")))); got != "/drive-scsi0.img.fidx/part/1" {
				t.Errorf("filepath decodes to %q", got)
			}
			return []any{js.O("text", "hosts", "filepath", b64("/drive-scsi0.img.fidx/part/1/etc/hosts"), "type", "f", "leaf", true, "size", 200, "mtime", 1735696800)}
		})
		root := env.call(t, "proxmox_list_backup_files", js.O("storage", "pbs", "volid", backupTestPBSVM))
		eqAt(t, root, "items", `[{"name":"drive-scsi0.img.fidx","path":"/drive-scsi0.img.fidx","type":"v","directory":true}]`)
		sub := env.call(t, "proxmox_list_backup_files", js.O("storage", "pbs", "volid", backupTestPBSVM, "path", "/drive-scsi0.img.fidx/part/1"))
		eqAt(t, sub, "items.0", `{"name":"hosts","path":"/drive-scsi0.img.fidx/part/1/etc/hosts","type":"f","size":"200 B","mtime":"2025-01-01T02:00:00.000Z"}`)
	})
}
