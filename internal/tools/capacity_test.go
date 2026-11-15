package tools

import (
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

func TestCapacityReport(t *testing.T) {
	env := startEnv(t)

	t.Run("compares allocation with physical capacity", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("GET", `^/nodes/pve1/storage/local-lvm/content$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			if p.Get("content") == "images" {
				// 300 GiB and 200 GiB of thin volumes on a 400 GiB pool.
				return []any{
					map[string]any{"volid": "local-lvm:vm-100-disk-0", "size": 322122547200.0},
					map[string]any{"volid": "local-lvm:vm-101-disk-0", "size": 214748364800.0},
				}
			}
			return []any{map[string]any{"volid": "local-lvm:vm-200-disk-0", "size": 8589934592.0}}
		})
		res := env.call(t, "proxmox_get_capacity_report", `{}`)
		// pve1: VM 100 (4 vCPU, 4 GiB, running) and CT 200 (1 vCPU, 512 MiB, running); template 9000 left out.
		eqAt(t, res, "nodes.0.node", "pve1")
		eqAt(t, res, "nodes.0.guests.total", 2)
		eqAt(t, res, "nodes.0.cpu.threads", 16)
		eqAt(t, res, "nodes.0.cpu.vcpus_allocated", 5)
		eqAt(t, res, "nodes.0.cpu.ratio_allocated", 0.31)
		eqAt(t, res, "nodes.0.memory.allocated", "4.50 GiB")
		// pve2: VM 101 is stopped, so only counts as allocated.
		eqAt(t, res, "nodes.1.cpu.vcpus_running", 0)
		eqAt(t, res, "nodes.1.cpu.vcpus_allocated", 2)
		eqAt(t, res, "nodes.1.memory.ratio_allocated", 0.25)
		eqAt(t, res, "totals.cpu.threads", 24)
		eqAt(t, res, "totals.guests.running", 2)
		// Only local-lvm holds guest disks.
		eqAt(t, res, "storage.0.storage", "local-lvm")
		eqAt(t, res, "storage.0.volumes", 3)
		eqAt(t, res, "storage.0.ratio_allocated", 1.27)
		eqAt(t, res, "storage.0.overcommitted", true)
		eq(t, len(at(res, "storage").([]any)), 1)
	})

	t.Run("filters by node and skips storage", func(t *testing.T) {
		env.mock.Reset()
		res := env.call(t, "proxmox_get_capacity_report", `{"node":"pve2","storage":false}`)
		eq(t, len(at(res, "nodes").([]any)), 1)
		eqAt(t, res, "nodes.0.node", "pve2")
		eqAt(t, res, "totals.cpu.threads", 8)
		falsy(t, at(res, "storage"), "storage")
		eq(t, len(env.mock.RequestsTo("/nodes/", "GET")), 0)
		match(t, env.errorText(t, "proxmox_get_capacity_report", `{"node":"nope"}`), "not found")
	})

	t.Run("reports a storage that can't be listed", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `/storage/local-lvm/content$`, proxmoxtest.Error(403, "Permission check failed"))
		res := env.call(t, "proxmox_get_capacity_report", `{}`)
		match(t, at(res, "storage.0.error"), "Permission")
	})
}
