package tools

import (
	"fmt"
	"testing"
	"time"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

func TestSnapshotsTools(t *testing.T) {
	env := startEnv(t)
	now := time.Now().Unix()
	const day = 86400
	vmSnaps := fmt.Sprintf(`[
		{"name":"base","description":"fresh install\n","snaptime":%d},
		{"name":"pre_upgrade","parent":"base","description":"before upgrade","snaptime":%d,"vmstate":1},
		{"name":"experiment","parent":"base","snaptime":%d},
		{"name":"current","parent":"pre_upgrade","description":"You are here!","running":1}]`, now-40*day, now-10*day, now-5*day)
	upid := func(method, pattern, node, typ, id string, task ...proxmoxtest.Task) {
		env.mock.On(method, pattern, func(*Obj, []string, proxmoxtest.Request) any { return env.mock.Upid(node, typ, id, task...) })
	}

	t.Run("lists a VM's snapshots as a tree with the current marker", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/nodes/pve1/qemu/100/snapshot$`, vmSnaps)
		res := env.call(t, "proxmox_list_snapshots", `{"vmid":100}`)
		eqAt(t, res, "type", "vm")
		eqAt(t, res, "total", 3)
		eqAt(t, res, "current_snapshot", "pre_upgrade")
		eq(t, pluck(at(res, "items"), "name"), `["base","pre_upgrade","experiment"]`)
		pre := at(res, "items.1")
		eqAt(t, pre, "current", true)
		eqAt(t, pre, "vmstate", true)
		eqAt(t, pre, "parent", "base")
		eqAt(t, pre, "age_days", 10)
		eqAt(t, res, "items.0.description", "fresh install")
		eqAt(t, res, "tree.length", 3)
		match(t, at(res, "tree.0"), `^base \[`)
		match(t, at(res, "tree.1"), `^ {2}pre_upgrade \[.*\] \(current\)$`)
		match(t, at(res, "tree.2"), `^ {2}experiment`)
	})

	t.Run("lists a container's snapshots using the lxc path", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/nodes/pve1/lxc/200/snapshot$`, `[{"name":"current","description":"You are here!"}]`)
		res := env.call(t, "proxmox_list_snapshots", `{"vmid":200}`)
		eqAt(t, res, "type", "container")
		eqAt(t, res, "count", 0)
		eqAt(t, res, "current_snapshot", js.Undef)
	})

	t.Run("gets a snapshot's config", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/nodes/pve1/qemu/100/snapshot/pre_upgrade/config$`, fmt.Sprintf(`{"description":"before upgrade","snaptime":%d,"parent":"base",
			"vmstate":"local-lvm:vm-100-state-pre_upgrade","memory":4096,"scsi0":"local-lvm:vm-100-disk-0,size=32G",
			"net0":"virtio=BC:24:11:00:00:01,bridge=vmbr0","cipassword":"secret"}`, now-day))
		res := env.call(t, "proxmox_get_snapshot", `{"vmid":100,"snapname":"pre_upgrade"}`)
		eqAt(t, res, "snapname", "pre_upgrade")
		eqAt(t, res, "vmstate", true)
		eqAt(t, res, "parent", "base")
		eqAt(t, res, "disks.scsi0.size", "32G")
		eqAt(t, res, "nets.net0.macaddr", "BC:24:11:00:00:01")
		eqAt(t, res, "config.cipassword", "<redacted>")
		eqAt(t, res, "config.description", js.Undef)
	})

	t.Run("lists snapshots across guests, filtered by age, with per-guest errors", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/nodes/pve1/qemu/100/snapshot$`, vmSnaps)
		env.mock.Reply("GET", `^/nodes/pve1/lxc/200/snapshot$`, fmt.Sprintf(`[{"name":"old_ct","snaptime":%d,"description":"ancient"},{"name":"current","parent":"old_ct"}]`, now-100*day))
		env.mock.On("GET", `^/nodes/pve2/qemu/101/snapshot$`, func(*Obj, []string, proxmoxtest.Request) any { return proxmoxtest.Error(595, "no route to host") })
		env.mock.Reply("GET", `^/nodes/pve1/qemu/9000/snapshot$`, `[{"name":"current"}]`)
		res := env.call(t, "proxmox_list_all_snapshots", `{"older_than_days":7}`)
		eqAt(t, res, "guests_checked", 4)
		items, _ := at(res, "items").([]any)
		var ids []any
		for _, s := range items {
			ids = append(ids, js.Str(at(s, "vmid"))+"/"+js.Str(at(s, "name")))
		}
		eq(t, js.V(ids), `["200/old_ct","100/base","100/pre_upgrade"]`)
		eqAt(t, res, "items.0.type", "container")
		eqAt(t, res, "items.0.guest", "dns")
		eqAt(t, res, "items.0.current", true)
		eqAt(t, res, "items.0.snaptime", js.Undef)
		eqAt(t, res, "errors.length", 1)
		eqAt(t, res, "errors.0.vmid", 101)

		onlyCt := env.call(t, "proxmox_list_all_snapshots", `{"type":"container"}`)
		eqAt(t, onlyCt, "guests_checked", 1)
		eqAt(t, onlyCt, "total", 1)
		search := env.call(t, "proxmox_list_all_snapshots", `{"vmids":[100],"search":"upgrade"}`)
		eqAt(t, search, "total", 1)
	})

	t.Run("creates a snapshot with vmstate for VMs only", func(t *testing.T) {
		env.mock.Reset()
		upid("POST", `^/nodes/pve1/qemu/100/snapshot$`, "pve1", "qmsnapshot", "100")
		res := env.call(t, "proxmox_create_snapshot", `{"vmid":100,"snapname":"pre_upgrade","description":"before","vmstate":true}`)
		eqAt(t, res, "status", "ok")
		eq(t, env.mock.LastWrite().Body, `{"snapname":"pre_upgrade","description":"before","vmstate":"1"}`)

		match(t, env.errorText(t, "proxmox_create_snapshot", `{"vmid":200,"snapname":"x1","vmstate":true}`), `only supported for VMs`)
		match(t, env.errorText(t, "proxmox_create_snapshot", `{"vmid":9000,"snapname":"x1"}`), `template`)
		truthy(t, env.errorText(t, "proxmox_create_snapshot", `{"vmid":100,"snapname":"1bad name"}`))

		upid("POST", `^/nodes/pve1/lxc/200/snapshot$`, "pve1", "vzsnapshot", "200")
		env.call(t, "proxmox_create_snapshot", `{"vmid":200,"snapname":"ct_snap"}`)
		eqStr(t, env.mock.LastWrite().Path, "/nodes/pve1/lxc/200/snapshot")
	})

	t.Run("rolls back with start", func(t *testing.T) {
		env.mock.Reset()
		upid("POST", `/lxc/200/snapshot/old_ct/rollback$`, "pve1", "vzrollback", "200")
		res := env.call(t, "proxmox_rollback_snapshot", `{"vmid":200,"snapname":"old_ct","start":true}`)
		eqAt(t, res, "rolled_back", true)
		eq(t, env.mock.LastWrite().Body, `{"start":"1"}`)
	})

	t.Run("updates a snapshot description", func(t *testing.T) {
		env.mock.Reset()
		env.call(t, "proxmox_update_snapshot", `{"vmid":101,"snapname":"base","description":""}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, "/nodes/pve2/qemu/101/snapshot/base/config")
		eq(t, w.Body, `{"description":""}`)
	})

	t.Run("deletes a snapshot, reporting task failures", func(t *testing.T) {
		env.mock.Reset()
		upid("DELETE", `/qemu/100/snapshot/base$`, "pve1", "qmdelsnapshot", "100")
		res := env.call(t, "proxmox_delete_snapshot", `{"vmid":100,"snapname":"base","force":true}`)
		eqAt(t, res, "deleted", true)
		eq(t, env.mock.LastWrite().Query, `{"force":"1"}`)

		upid("DELETE", `/qemu/100/snapshot/bad$`, "pve1", "qmdelsnapshot", "100", proxmoxtest.Task{Status: "stopped", ExitStatus: "zfs error", Log: []string{"could not destroy"}})
		match(t, env.errorText(t, "proxmox_delete_snapshot", `{"vmid":100,"snapname":"bad"}`), `zfs error`)
		started := env.call(t, "proxmox_delete_snapshot", `{"vmid":100,"snapname":"base","wait":false}`)
		eqAt(t, started, "status", "started")
	})

	t.Run("errors for unknown guests", func(t *testing.T) {
		env.mock.Reset()
		match(t, env.errorText(t, "proxmox_list_snapshots", `{"vmid":4242}`), `No VM or container with ID 4242`)
	})
}
