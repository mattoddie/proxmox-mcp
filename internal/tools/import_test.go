package tools

import (
	"testing"
)

const importESXiMetadata = `{
	"type":"vm","source":"esxi",
	"create-args":{"name":"web01","ostype":"l26","memory":4096,"cores":2,"bios":"ovmf","scsihw":"pvscsi"},
	"disks":{"scsi0":"esxi1:ha-datacenter/datastore1/web01/web01.vmdk","scsi1":"esxi1:ha-datacenter/datastore1/web01/web01_1.vmdk"},
	"net":{"net0":{"model":"vmxnet3","macaddr":"BC:24:11:00:00:01"},"net1":{"model":"e1000","bridge":"vmbr1"}},
	"warnings":[{"type":"efi-state-lost","key":"bios","value":"ovmf"}]
}`

func TestImport(t *testing.T) {
	env := startEnv(t)

	t.Run("reads import metadata", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/nodes/pve1/storage/esxi1/import-metadata$`, importESXiMetadata)
		res := env.call(t, "proxmox_get_import_metadata", `{"node":"pve1","volume":"esxi1:ha-datacenter/datastore1/web01/web01.vmx"}`)
		eqAt(t, res, "source", "esxi")
		eqAt(t, res, "create_args.name", "web01")
		eqAt(t, res, "disks.scsi1", "esxi1:ha-datacenter/datastore1/web01/web01_1.vmdk")
		eqAt(t, res, "warnings.0.type", "efi-state-lost")
		eqStr(t, env.mock.Last().Query.Get("volume"), "esxi1:ha-datacenter/datastore1/web01/web01.vmx")
	})

	t.Run("creates a VM from the metadata", func(t *testing.T) {
		env.mock.Reset()
		vmsNextID(env, "150")
		env.mock.Reply("GET", `^/nodes/pve1/storage/esxi1/import-metadata$`, importESXiMetadata)
		vmsTaskRoute(env, "POST", `^/nodes/pve1/qemu$`, "qmcreate", "pve1")
		res := env.call(t, "proxmox_import_vm", `{"node":"pve1","volume":"esxi1:ha-datacenter/datastore1/web01/web01.vmx","target_storage":"local-lvm","format":"raw","extra":{"cores":4}}`)
		eqAt(t, res, "vmid", 150)
		eqAt(t, res, "name", "web01")
		eqAt(t, res, "status", "ok")
		w := env.mock.LastWrite()
		eqStr(t, w.Path, "/nodes/pve1/qemu")
		for k, v := range map[string]string{
			"vmid":     "150",
			"name":     "web01",
			"memory":   "4096",
			"cores":    "4",
			"scsihw":   "pvscsi",
			"scsi0":    "local-lvm:0,import-from=esxi1:ha-datacenter/datastore1/web01/web01.vmdk,format=raw",
			"scsi1":    "local-lvm:0,import-from=esxi1:ha-datacenter/datastore1/web01/web01_1.vmdk,format=raw",
			"net0":     "vmxnet3,macaddr=BC:24:11:00:00:01,bridge=vmbr0",
			"net1":     "e1000,bridge=vmbr1",
			"efidisk0": "local-lvm:1,efitype=4m",
		} {
			eqStr(t, w.Body.Get(k), v, k)
		}
		falsy(t, w.Body.Has("start"), "start")
	})

	t.Run("extracts an OVA through a working storage", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/nodes/pve1/storage/local/import-metadata$`, `{"type":"vm","source":"esxi","create-args":{"name":"appliance"},"disks":{"sata0":"local:import/appliance.ova/disk1.vmdk"},"warnings":[{"type":"ova-needs-extracting"}]}`)
		vmsTaskRoute(env, "POST", `^/nodes/pve1/qemu$`, "qmcreate", "pve1")
		env.call(t, "proxmox_import_vm", `{"node":"pve1","volume":"local:import/appliance.ova","target_storage":"local-lvm","vmid":151,"name":"app2","working_storage":"scratch","bridge":"vmbr2"}`)
		b := env.mock.LastWrite().Body
		eqStr(t, b.Get("name"), "app2")
		eqStr(t, b.Get("sata0"), "local-lvm:0,import-from=local:import/appliance.ova/disk1.vmdk")
		eqStr(t, b.Get("import-working-storage"), "scratch")
		falsy(t, b.Has("efidisk0"), "efidisk0")
	})

	t.Run("refuses extra that would restore or override typed arguments", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `import-metadata$`, importESXiMetadata)
		match(t, env.errorText(t, "proxmox_import_vm", `{"node":"pve1","vmid":150,"volume":"esxi1:x.vmx","target_storage":"local-lvm","extra":{"archive":"x"}}`), "extra must not set `archive`")
		match(t, env.errorText(t, "proxmox_import_vm", `{"node":"pve1","vmid":150,"volume":"esxi1:x.vmx","target_storage":"local-lvm","extra":{"scsi0":"x"}}`), "extra must not set `scsi0`")
		vmsNoWrites(t, env)
	})

	t.Run("refuses metadata without disks or with odd keys", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `import-metadata$`, `{"type":"vm","source":"esxi","create-args":{}}`)
		match(t, env.errorText(t, "proxmox_import_vm", `{"node":"pve1","vmid":150,"volume":"esxi1:x.vmx","target_storage":"local-lvm"}`), "no disks")
		env.mock.Reply("GET", `import-metadata$`, `{"type":"vm","create-args":{},"disks":{"hookscript":"local:snippets/x"}}`)
		match(t, env.errorText(t, "proxmox_import_vm", `{"node":"pve1","vmid":150,"volume":"esxi1:x.vmx","target_storage":"local-lvm"}`), "Unexpected disk key")
		vmsNoWrites(t, env)
	})

	t.Run("is gated behind writes", func(t *testing.T) {
		names := toolNames(t, connect(t, env.client, toolOptions("read")))
		truthy(t, names["proxmox_get_import_metadata"])
		falsy(t, names["proxmox_import_vm"])
	})
}
