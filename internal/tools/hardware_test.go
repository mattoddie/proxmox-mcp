package tools

import (
	"net/url"
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

const hardwareTestPci = `[
	{"id":"0000:01:00.0","class":"0x030000","vendor":"0x10de","device":"0x2204","vendor_name":"NVIDIA Corporation","device_name":"GA102 [GeForce RTX 3090]","subsystem_vendor":"0x1462","subsystem_device":"0x3880","subsystem_vendor_name":"MSI","iommugroup":12,"mdev":0},
	{"id":"0000:01:00.1","class":"0x040300","vendor":"0x10de","device":"0x1aef","vendor_name":"NVIDIA Corporation","device_name":"GA102 HD Audio","iommugroup":12},
	{"id":"0000:02:00.0","class":"0x020000","vendor":"0x8086","device":"0x1533","vendor_name":"Intel Corporation","device_name":"I210 Gigabit","iommugroup":13},
	{"id":"0000:03:00.0","class":"0x030000","vendor":"0x8086","device":"0x56a0","vendor_name":"Intel Corporation","device_name":"Arc A770","iommugroup":14,"mdev":1}]`

func TestHardware(t *testing.T) {
	env := startEnv(t, map[string]string{"PROXMOX_DEFAULT_NODE": "pve1"})
	run := func(name string, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			env.mock.Reset()
			env.mock.Reply("GET", `^/nodes/pve1/hardware/pci$`, hardwareTestPci)
			f(t)
		})
	}
	firstQuery := func(t *testing.T, prefix string) *Obj {
		t.Helper()
		reqs := env.mock.RequestsTo(prefix, "GET")
		if len(reqs) == 0 {
			t.Fatalf("no GET %s", prefix)
		}
		return reqs[0].Query
	}

	run("lists PCI devices with IOMMU groups and names", func(t *testing.T) {
		res := env.call(t, "proxmox_list_node_pci_devices", `{}`)
		eqAt(t, res, "node", "pve1")
		eqAt(t, res, "iommu_enabled", true)
		eqAt(t, res, "total", 4)
		eqAt(t, res, "items.0", `{"id":"0000:01:00.0","class":"0x030000","class_name":"display","vendor":"NVIDIA Corporation","device":"GA102 [GeForce RTX 3090]","ids":"10de:2204","subsystem":"MSI","subsystem_id":"1462:3880","iommu_group":12,"iommu_group_peers":["0000:01:00.1"]}`)
		eqAt(t, res, "items.3.mdev", true)
		eqAt(t, firstQuery(t, "/nodes/pve1/hardware/pci"), "pci-class-blacklist", js.Undef)
	})

	run("filters PCI devices by class name, class code, mdev and search", func(t *testing.T) {
		display := env.call(t, "proxmox_list_node_pci_devices", `{"class":"display"}`)
		eq(t, pluck(at(display, "items"), "id"), `["0000:01:00.0","0000:03:00.0"]`)
		net := env.call(t, "proxmox_list_node_pci_devices", `{"class":"0x02"}`)
		eq(t, pluck(at(net, "items"), "id"), `["0000:02:00.0"]`)
		mdev := env.call(t, "proxmox_list_node_pci_devices", `{"mdev_only":true}`)
		eq(t, pluck(at(mdev, "items"), "id"), `["0000:03:00.0"]`)
		search := env.call(t, "proxmox_list_node_pci_devices", `{"search":"rtx"}`)
		eqAt(t, search, "total", 1)
	})

	run("includes hidden classes with all=true and validates class codes", func(t *testing.T) {
		env.call(t, "proxmox_list_node_pci_devices", `{"all":true}`)
		eqStr(t, firstQuery(t, "/nodes/pve1/hardware/pci").Get("pci-class-blacklist"), "")
		if !env.callRaw(t, "proxmox_list_node_pci_devices", `{"class":"gpu!"}`).IsError {
			t.Fatal("expected a validation error")
		}
	})

	run("reports IOMMU off", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/hardware/pci$`, `[{"id":"0000:01:00.0","class":"0x030000","iommugroup":-1}]`)
		res := env.call(t, "proxmox_list_node_pci_devices", `{}`)
		eqAt(t, res, "iommu_enabled", false)
		eqAt(t, res, "items.0.iommu_group", -1)
		eqAt(t, res, "items.0.iommu_group_peers", js.Undef)
	})

	run("lists mdev types", func(t *testing.T) {
		env.mock.On("GET", `^/nodes/pve1/hardware/pci/([^/]+)/mdev$`, func(_ *Obj, m []string, _ proxmoxtest.Request) any {
			name, _ := url.PathUnescape(m[1])
			return []any{js.O("type", "i915-GVTg_V5_4", "name", name, "available", 2, "description", "low_gm_size: 128MB")}
		})
		res := env.call(t, "proxmox_list_node_mdev_types", `{"pci_id":"0000:03:00.0"}`)
		eqAt(t, res, "items.0.available", 2)
		eqStr(t, at(res, "items.0.name"), "0000:03:00.0")
	})

	run("lists USB devices", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/hardware/usb$`, `[
			{"busnum":1,"devnum":1,"port":0,"level":0,"class":9,"vendid":"1d6b","prodid":"0002","product":"xHCI Host Controller","speed":"480"},
			{"busnum":1,"devnum":3,"port":2,"level":1,"class":0,"vendid":"0658","prodid":"0200","manufacturer":"Sigma Designs","product":"Aeotec Z-Stick","usbpath":"1-2","speed":"12","serial":"123"}]`)
		res := env.call(t, "proxmox_list_node_usb_devices", `{}`)
		eqAt(t, res, "items.0.hub", true)
		eqAt(t, res, "items.1", `{"id":"0658:0200","manufacturer":"Sigma Designs","product":"Aeotec Z-Stick","port":"1-2","bus":1,"device":3,"speed":"12","class":0,"serial":"123"}`)
		z := env.call(t, "proxmox_list_node_usb_devices", `{"search":"z-stick"}`)
		eqAt(t, z, "total", 1)
	})

	run("gets node capabilities grouped for the model", func(t *testing.T) {
		env.mock.Reply("GET", `^/nodes/pve1/capabilities/qemu/cpu$`, `[
			{"name":"host","vendor":"default","custom":0},
			{"name":"x86-64-v2-AES","vendor":"default","custom":0,"abstract":1},
			{"name":"EPYC-Rome","vendor":"AuthenticAMD","custom":0},
			{"name":"custom-safe","vendor":"GenuineIntel","custom":1}]`)
		env.mock.Reply("GET", `^/nodes/pve1/capabilities/qemu/machines$`, `[
			{"id":"pc-q35-9.2+pve1","type":"q35","version":"9.2+pve1"},
			{"id":"pc-i440fx-9.2","type":"i440fx","version":"9.2"}]`)
		env.mock.Reply("GET", `^/nodes/pve1/capabilities/qemu/cpu-flags$`, `[{"name":"aes","description":"Activate AES instruction set for HW acceleration."}]`)
		res := env.call(t, "proxmox_get_node_capabilities", `{}`)
		eqAt(t, res, "cpu_models", `{"default":["host"],"generic":["x86-64-v2-AES"],"AuthenticAMD":["EPYC-Rome"],"custom":["custom-safe"]}`)
		eqAt(t, res, "machines", `{"q35":["pc-q35-9.2+pve1"],"i440fx":["pc-i440fx-9.2"]}`)
		eqAt(t, res, "cpu_flags.0.name", "aes")

		only := env.call(t, "proxmox_get_node_capabilities", `{"sections":["machines"],"arch":"aarch64"}`)
		eqAt(t, only, "cpu_models", js.Undef)
		reqs := env.mock.RequestsTo("/nodes/pve1/capabilities/qemu/machines", "GET")
		eqStr(t, reqs[len(reqs)-1].Query.Get("arch"), "aarch64")
	})

	run("reports per-section capability errors", func(t *testing.T) {
		res := env.call(t, "proxmox_get_node_capabilities", `{"sections":["cpu_flags"]}`)
		match(t, at(res, "cpu_flags.error"), `404`)
	})

	run("lists and gets resource mappings with parsed entries", func(t *testing.T) {
		env.mock.On("GET", `^/cluster/mapping/pci$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			var checks any = js.Undef
			if p.Truthy("check-node") {
				checks = []any{js.O("severity", "warning", "message", "iommu group changed")}
			}
			return []any{js.O("id", "gpu", "description", "RTX", "map", []any{"node=pve1,path=0000:01:00.0,id=10de:2204,iommugroup=12"}, "checks", checks)}
		})
		res := env.call(t, "proxmox_list_resource_mappings", `{"kind":"pci","check_node":"pve1"}`)
		eqAt(t, res, "items.0.map", `[{"node":"pve1","path":"0000:01:00.0","id":"10de:2204","iommugroup":"12"}]`)
		eqAt(t, res, "items.0.checks.0.severity", "warning")

		env.mock.Reply("GET", `^/cluster/mapping/dir/share$`, `{"description":"shared","map":["node=pve1,path=/srv/share"],"digest":"dg"}`)
		one := env.call(t, "proxmox_get_resource_mapping", `{"kind":"dir","id":"share"}`)
		eq(t, one, `{"id":"share","description":"shared","map":[{"node":"pve1","path":"/srv/share"}],"digest":"dg"}`)
	})

	run("creates a PCI mapping from structured entries", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/mapping/pci/gpu$`, `{"map":["node=pve1,path=0000:01:00.0,id=10de:2204"],"mdev":0}`)
		res := env.call(t, "proxmox_save_resource_mapping", `{"kind":"pci","id":"gpu","create":true,"description":"RTX",
			"map":[{"node":"pve1","path":"0000:01:00.0","id":"10de:2204","iommugroup":12,"subsystem_id":"1462:3880"},"node=pve2,path=0000:41:00.0,id=10de:2204"],
			"live_migration_capable":false}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, "/cluster/mapping/pci")
		eq(t, w.Body, `{"id":"gpu","map":["node=pve1,path=0000:01:00.0,id=10de:2204,iommugroup=12,subsystem-id=1462:3880","node=pve2,path=0000:41:00.0,id=10de:2204"],"description":"RTX","live-migration-capable":"0"}`)
		eqAt(t, res, "action", "created")
		eqAt(t, res, "mapping.map.0.node", "pve1")
	})

	run("validates mapping arguments", func(t *testing.T) {
		match(t, env.errorText(t, "proxmox_save_resource_mapping", `{"kind":"dir","id":"d","create":true}`), `map.*required`)
		match(t, env.errorText(t, "proxmox_save_resource_mapping", `{"kind":"usb","id":"u","mdev":true}`), `only apply to PCI`)
	})

	run("resends the current map when updating a USB mapping without one", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/mapping/usb/zstick$`, `{"map":["node=pve1,id=0658:0200"],"description":"old"}`)
		env.call(t, "proxmox_save_resource_mapping", `{"kind":"usb","id":"zstick","description":"Z-Wave stick"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, "/cluster/mapping/usb/zstick")
		eq(t, w.Body, `{"map":"node=pve1,id=0658:0200","description":"Z-Wave stick"}`)
	})

	run("updates a dir mapping without resending the map", func(t *testing.T) {
		env.call(t, "proxmox_save_resource_mapping", `{"kind":"dir","id":"share","delete":["description"],"digest":"dg"}`)
		eq(t, env.mock.LastWrite().Body, `{"delete":"description","digest":"dg"}`)
	})

	run("deletes a resource mapping", func(t *testing.T) {
		res := env.call(t, "proxmox_delete_resource_mapping", `{"kind":"usb","id":"zstick"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eqStr(t, w.Path, "/cluster/mapping/usb/zstick")
		eqAt(t, res, "deleted", "zstick")
	})

	run("lists and gets custom CPU models", func(t *testing.T) {
		env.mock.Reply("GET", `^/cluster/qemu/custom-cpu-models$`, `[{"cputype":"custom-safe","reported-model":"EPYC-Rome","flags":"+aes;-pcid","hidden":1,"digest":"x"}]`)
		eq(t, env.call(t, "proxmox_list_custom_cpu_models", `{}`), `{"total":1,"returned":1,"items":[{"name":"custom-safe","reported_model":"EPYC-Rome","flags":["+aes","-pcid"],"hidden":true}]}`)
		env.mock.Reply("GET", `^/cluster/qemu/custom-cpu-models/safe$`, `{"cputype":"custom-safe","digest":"x"}`)
		eqAt(t, env.call(t, "proxmox_get_custom_cpu_model", `{"name":"safe"}`), "digest", "x")
	})

	run("creates, updates and deletes custom CPU models", func(t *testing.T) {
		match(t, env.errorText(t, "proxmox_save_custom_cpu_model", `{"name":"safe","create":true}`), `reported_model`)
		env.call(t, "proxmox_save_custom_cpu_model", `{"name":"safe","create":true,"reported_model":"EPYC-Rome","flags":["+aes","-pcid"],"phys_bits":"host"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, "/cluster/qemu/custom-cpu-models")
		eq(t, w.Body, `{"cputype":"safe","reported-model":"EPYC-Rome","flags":"+aes;-pcid","phys-bits":"host"}`)

		env.call(t, "proxmox_save_custom_cpu_model", `{"name":"safe","hidden":true,"delete":["flags"]}`)
		w = env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, "/cluster/qemu/custom-cpu-models/safe")
		eq(t, w.Body, `{"hidden":"1","delete":"flags"}`)

		env.call(t, "proxmox_delete_custom_cpu_model", `{"name":"safe"}`)
		w = env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eqStr(t, w.Path, "/cluster/qemu/custom-cpu-models/safe")
	})
}
