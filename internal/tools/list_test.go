package tools

import (
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
)

func TestListing(t *testing.T) {
	env := startEnv(t)

	t.Run("projects fields on a list the tool narrows itself", func(t *testing.T) {
		res := env.call(t, "proxmox_list_guests", `{"fields":["vmid","name"],"limit":2}`)
		eqAt(t, res, "total", 4)
		eqAt(t, res, "returned", 2)
		eqAt(t, res, "items", `[{"vmid":100,"name":"web"},{"vmid":101,"name":"db"}]`)
	})

	t.Run("names the available fields when none match", func(t *testing.T) {
		res := env.call(t, "proxmox_list_guests", `{"fields":["nope"],"limit":1}`)
		eqAt(t, res, "items", `[{}]`)
		match(t, at(res, "hint"), `^None of the requested fields \(nope\) exist\. Available fields: vmid, name, type`)
	})

	t.Run("wraps a plain list and searches it", func(t *testing.T) {
		env.mock.Reply("GET", `^/access/domains$`, `[{"realm":"pam","type":"pam","comment":"Linux PAM"},{"realm":"corp","type":"ldap"}]`)
		eq(t, env.call(t, "proxmox_list_realms", nil), `{"total":2,"returned":2,"items":[{"realm":"pam","type":"pam","comment":"Linux PAM"},{"realm":"corp","type":"ldap"}]}`)
		eq(t, env.call(t, "proxmox_list_realms", `{"search":"LINUX","fields":["realm"]}`), `{"total":1,"returned":1,"items":[{"realm":"pam"}]}`)
	})

	t.Run("keeps a list's context and replaces its count", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/disks/zfs$`, `[{"name":"rpool","health":"ONLINE","alloc":250,"size":1000,"free":750},{"name":"tank","health":"DEGRADED","alloc":1,"size":2}]`)
		res := env.callObj(t, "proxmox_list_zfs_pools", `{"node":"pve1","search":"degraded","fields":["name"]}`)
		eq(t, js.V(res.Keys()), `["node","total","returned","items"]`)
		eqAt(t, res, "items", `[{"name":"tank"}]`)
	})

	t.Run("applies a default limit to long lists", func(t *testing.T) {
		var entries []any
		for i := 0; i < 600; i++ {
			entries = append(entries, js.O("text", "f", "type", "f", "leaf", true, "size", 1))
		}
		env.mock.Reply("GET", `^/cluster/resources$`, `[{"type":"storage","storage":"pbs","node":"pve1","shared":1,"status":"available","content":"backup"}]`)
		env.mock.Reply("GET", `file-restore/list$`, js.Array(entries))
		res := env.call(t, "proxmox_list_backup_files", js.O("storage", "pbs", "volid", backupTestPBSVM))
		eqAt(t, res, "total", 600)
		eqAt(t, res, "returned", 500)
		eqAt(t, env.call(t, "proxmox_list_backup_files", js.O("storage", "pbs", "volid", backupTestPBSVM, "limit", 3)), "returned", 3)
	})

	t.Run("walks arrays in dot paths", func(t *testing.T) {
		items, hint := selectFields([]any{js.MustParse(`{"name":"a","disks":[{"id":"d1","size":1},{"id":"d2"}],"cfg":{"x":1,"y":2}}`)}, []any{"disks.size", "cfg.x", " name "})
		eq(t, js.V(items), `[{"disks":[{"size":1}],"cfg":{"x":1},"name":"a"}]`)
		eqStr(t, hint, "")
	})
}
