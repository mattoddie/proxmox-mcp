package tools

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

// hardwarePciClasses are the common PCI base classes (first two hex digits of the class code),
// for filtering by name.
var hardwarePciClasses = js.O(
	"01", "storage",
	"02", "network",
	"03", "display",
	"04", "multimedia",
	"05", "memory",
	"06", "bridge",
	"07", "communication",
	"08", "system",
	"09", "input",
	"0c", "serial-bus",
	"0d", "wireless",
	"12", "accelerator",
)

var hardwareClassNames = []string{"storage", "network", "display", "multimedia", "bridge", "serial-bus", "wireless", "accelerator"}

var (
	hardwareHexPrefixRe  = regexp.MustCompile(`(?i)^0x`)
	hardwareHexPrefixCs  = regexp.MustCompile(`^0x`)
	hardwareMappingKinds = []string{"pci", "usb", "dir"}
)

func hardwarePciClassCode(cls any) string {
	s := hardwareHexPrefixRe.ReplaceAllString(js.Str(or(cls, "")), "")
	return strings.ToLower(js.Slice16(s, 2))
}

func hardwareStrip0x(v any) string { return hardwareHexPrefixCs.ReplaceAllString(js.Str(v), "") }

func hardwareSummarizePci(d *Obj, groupPeers map[float64][]any) *Obj {
	group := d.Num("iommugroup")
	var ids, subsystem, subsystemID, peers any = js.Undef, js.Undef, js.Undef, js.Undef
	if d.Truthy("vendor") && d.Truthy("device") {
		ids = hardwareStrip0x(d.Get("vendor")) + ":" + hardwareStrip0x(d.Get("device"))
	}
	if d.Truthy("subsystem_vendor_name") || d.Truthy("subsystem_device_name") {
		subsystem = strings.TrimSpace(js.Str(or(d.Get("subsystem_vendor_name"), "")) + " " + js.Str(or(d.Get("subsystem_device_name"), "")))
	}
	if d.Truthy("subsystem_vendor") && d.Truthy("subsystem_device") {
		subsystemID = hardwareStrip0x(d.Get("subsystem_vendor")) + ":" + hardwareStrip0x(d.Get("subsystem_device"))
	}
	if group >= 0 {
		if list, ok := groupPeers[group]; ok {
			out := []any{}
			for _, id := range list {
				if !hardwareStrictEqual(id, d.Get("id")) {
					out = append(out, id)
				}
			}
			peers = out
		}
	}
	return proxmox.C(
		"id", d.Get("id"),
		"class", d.Get("class"),
		"class_name", hardwarePciClasses.Get(hardwarePciClassCode(d.Get("class"))),
		"vendor", d.Get("vendor_name"),
		"device", d.Get("device_name"),
		"ids", ids,
		"subsystem", subsystem,
		"subsystem_id", subsystemID,
		"iommu_group", ifThen(js.IsFinite(group), group),
		"iommu_group_peers", peers,
		"mdev", ifThen(d.Truthy("mdev"), true),
	)
}

// hardwareStrictEqual is === for JSON scalars.
func hardwareStrictEqual(a, b any) bool {
	switch x := a.(type) {
	case *Obj, []any:
		return false
	default:
		return x == b
	}
}

var (
	hardwareMappingKindArg = Enum(hardwareMappingKinds...).D("Mapping kind: pci (passthrough devices), usb, or dir (virtiofs directories)")

	hardwareMapEntryArg = Union(
		Str().D("Raw property string, e.g. 'node=pve1,path=0000:01:00.0,id=10de:2204,iommugroup=12'"),
		Object(
			P("node", Str().D("Cluster node")),
			P("path", Str().Opt().D("pci: device address(es) e.g. 0000:01:00.0 (';'-separated for alternatives); usb: port path e.g. 1-2; dir: absolute directory")),
			P("id", Str().Opt().D("pci/usb: vendor:device ID, e.g. 10de:2204 (required for pci and usb)")),
			P("iommugroup", Int().Opt().D("pci: expected IOMMU group")),
			P("subsystem_id", Str().Opt().D("pci: expected subsystem vendor:device ID")),
			P("description", Str().Opt().D("Per-node description")),
		),
	)
)

func hardwareMapEntryToString(e any) string {
	o, ok := e.(*Obj)
	if !ok {
		return js.Str(e)
	}
	parts := []string{"node=" + js.Str(o.Get("node"))}
	for _, f := range [][2]string{{"path", "path"}, {"id", "id"}, {"iommugroup", "iommugroup"}, {"subsystem_id", "subsystem-id"}, {"description", "description"}} {
		if o.Defined(f[0]) {
			parts = append(parts, f[1]+"="+js.Str(o.Get(f[0])))
		}
	}
	return strings.Join(parts, ",")
}

// hardwareMapList is Array.isArray(m.map) ? m.map : m.map ? [m.map] : fallback.
func hardwareMapList(m *Obj, fallback any) any {
	v := m.Get("map")
	if arr, ok := v.([]any); ok {
		return arr
	}
	if js.Truthy(v) {
		return []any{v}
	}
	return fallback
}

func hardwareSummarizeMapping(m *Obj) *Obj {
	list, _ := hardwareMapList(m, []any{}).([]any)
	maps := make([]any, len(list))
	for i, e := range list {
		maps[i] = proxmox.ParsePropertyString(e, "")
	}
	return proxmox.C(
		"id", m.Get("id"),
		"description", m.Get("description"),
		"mdev", ifThen(m.Truthy("mdev"), true),
		"live_migration_capable", ifThen(m.Truthy("live-migration-capable"), true),
		"map", maps,
		"checks", or(m.Get("checks"), m.Get("error")),
	)
}

var hardwareCPUModelFields = []Prop{
	P("reported_model", Str().Opt().D("Base QEMU CPU model reported to the guest, e.g. host, kvm64, x86-64-v2-AES, EPYC-Rome, Skylake-Server (required when creating)")),
	P("flags", Arr(Str()).Opt().D("CPU flags to add (+flag) or remove (-flag), e.g. [\"+aes\", \"-pcid\"]")),
	P("hidden", Bool().Opt().D("Hide that the guest runs under KVM")),
	P("hv_vendor_id", Str().Opt().D("Hyper-V vendor ID reported to Windows guests")),
	P("phys_bits", Str().Opt().D("Physical address bits reported to the guest (8-64 or 'host')")),
	P("guest_phys_bits", Int().Min(32).Max(64).Opt().D("Physical address bits usable by the guest")),
	P("level", Int().Min(0).Opt().D("Maximum basic CPUID leaf")),
}

func registerHardware(c *Context) {
	client := c.Client

	// ---------------------------------------------------------------- node hardware

	c.Define("proxmox_list_node_pci_devices", Def{
		Toolset:     "hardware",
		Title:       "List PCI devices",
		Description: "List a node's PCI devices for passthrough planning: address, class, vendor/device names and IDs, IOMMU group (with the other devices in the same group, which must be passed through together) and whether it supports mediated devices (vGPU). Memory controllers, bridges and processors are hidden unless all=true. IOMMU group -1 means IOMMU is off.",
		Input: []Prop{
			P("node", nodeArg),
			P("class", Union(Enum(hardwareClassNames...), Str().Pattern(`^(0x)?[0-9a-fA-F]{2,6}$`)).Opt().D("Only this class: a name (display, network, storage, multimedia, serial-bus, accelerator...) or hex class code prefix, e.g. 03 or 0300")),
			P("mdev_only", Bool().Opt().D("Only devices that support mediated devices (vGPU)")),
			P("all", Bool().Opt().D("Include memory controllers, bridges and processors (hidden by default)")),
			P("search", searchArg),
			P("limit", limitArg(500)),
			P("raw", rawArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			var q *Obj
			if a.Truthy("all") {
				q = js.O("pci-class-blacklist", "")
			}
			devices, err := client.GetList(ctx, "/nodes/"+enc(node)+"/hardware/pci", q)
			if err != nil {
				return nil, err
			}
			groupPeers := map[float64][]any{}
			for _, d := range devices {
				if g := d.Num("iommugroup"); g >= 0 {
					groupPeers[g] = append(groupPeers[g], d.Get("id"))
				}
			}
			list := devices
			if a.Truthy("class") {
				var byName any = js.Undef
				for _, k := range hardwarePciClasses.Keys() {
					if hardwarePciClasses.Get(k) == a.Get("class") {
						byName = k
						break
					}
				}
				prefixAny := byName
				if js.Nullish(prefixAny) {
					prefixAny = hardwareHexPrefixRe.ReplaceAllString(a.Str("class"), "")
				}
				prefix := strings.ToLower(js.Str(prefixAny))
				list = filterObjs(list, func(d *Obj) bool {
					return strings.HasPrefix(strings.ToLower(hardwareHexPrefixRe.ReplaceAllString(js.Str(or(d.Get("class"), "")), "")), prefix)
				})
			}
			if a.Truthy("mdev_only") {
				list = filterObjs(list, func(d *Obj) bool { return d.Truthy("mdev") })
			}
			sortObjs(list, func(x, y *Obj) float64 { return compareStr(x.Get("id"), y.Get("id")) })
			res := filterAndLimit(list, a.Get("search"), or(a.Get("limit"), 500), func(d *Obj) []any {
				return []any{d.Get("id"), d.Get("vendor_name"), d.Get("device_name"), d.Get("subsystem_vendor_name"), d.Get("subsystem_device_name"), d.Get("vendor"), d.Get("device")}
			})
			iommu := false
			for _, d := range devices {
				if d.Num("iommugroup") >= 0 {
					iommu = true
				}
			}
			var items any
			if a.Truthy("raw") {
				items = js.Array(res.Items)
			} else {
				items = mapObjs(res.Items, func(d *Obj) any { return hardwareSummarizePci(d, groupPeers) })
			}
			return js.Spread(js.O("node", node, "iommu_enabled", iommu), res.Obj(items)), nil
		},
	})

	c.Define("proxmox_list_node_mdev_types", Def{
		Toolset:     "hardware",
		Title:       "List mediated device types",
		Description: "List the mediated device (mdev, e.g. NVIDIA vGPU / Intel GVT-g) types a PCI device offers, with how many instances are still available. Use a device with mdev support from proxmox_list_node_pci_devices.",
		Input: []Prop{
			P("node", nodeArg),
			P("pci_id", Str().D("PCI address (e.g. 0000:01:00.0) or the ID of a PCI resource mapping")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			types, err := client.GetList(ctx, "/nodes/"+enc(node)+"/hardware/pci/"+enc(a.Get("pci_id"))+"/mdev", nil)
			if err != nil {
				return nil, err
			}
			return js.O("node", node, "pci_id", a.Get("pci_id"), "types", mapObjs(types, func(t *Obj) any {
				return proxmox.C("type", t.Get("type"), "name", t.Get("name"), "available", t.Get("available"), "description", t.Get("description"))
			})), nil
		},
	})

	c.Define("proxmox_list_node_usb_devices", Def{
		Toolset:     "hardware",
		Title:       "List USB devices",
		Description: "List a node's USB devices for passthrough: vendor:product ID, manufacturer and product names, bus/port path, speed and serial. Pass a device to a VM by ID (host=vendor:product) or port (host=bus-port), or via a USB resource mapping.",
		Input:       []Prop{P("node", nodeArg), P("search", searchArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			devices, err := client.GetList(ctx, "/nodes/"+enc(node)+"/hardware/usb", nil)
			if err != nil {
				return nil, err
			}
			res := filterAndLimit(devices, a.Get("search"), js.Undef, func(d *Obj) []any {
				return []any{d.Get("vendid"), d.Get("prodid"), d.Get("manufacturer"), d.Get("product"), d.Get("serial"), d.Get("usbpath")}
			})
			items := mapObjs(res.Items, func(d *Obj) any {
				port := d.Get("usbpath")
				if js.Nullish(port) {
					port = ifThen(d.Defined("busnum"), js.Str(d.Get("busnum"))+"-"+js.Str(d.Get("port")))
				}
				return proxmox.C(
					"id", ifThen(d.Truthy("vendid") && d.Truthy("prodid"), js.Str(d.Get("vendid"))+":"+js.Str(d.Get("prodid"))),
					"manufacturer", d.Get("manufacturer"),
					"product", d.Get("product"),
					"port", port,
					"bus", d.Get("busnum"),
					"device", d.Get("devnum"),
					"speed", d.Get("speed"),
					"class", d.Get("class"),
					"serial", d.Get("serial"),
					"hub", ifThen(d.Num("class") == 9, true),
				)
			})
			return js.Spread(js.O("node", node), res.Obj(items)), nil
		},
	})

	c.Define("proxmox_get_node_capabilities", Def{
		Toolset:     "hardware",
		Title:       "Get node QEMU capabilities",
		Description: "Get what a node's QEMU supports for VM configuration: CPU models (built-in and custom), machine types/versions (q35, i440fx) and the VM-specific CPU flags that can be toggled. Use it to pick valid values for a VM's cpu/machine options.",
		Input: []Prop{
			P("node", nodeArg),
			P("sections", Arr(Enum("cpu_models", "machines", "cpu_flags")).Opt().D("Which parts to return (default all)")),
			P("arch", Enum("x86_64", "aarch64").Opt().D("Guest architecture (default the host's)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			base := "/nodes/" + enc(node) + "/capabilities/qemu"
			want := map[string]bool{}
			if s := a.Strs("sections"); len(s) > 0 {
				for _, x := range s {
					want[x] = true
				}
			} else {
				want = map[string]bool{"cpu_models": true, "machines": true, "cpu_flags": true}
			}
			params := defined(js.O("arch", a.Get("arch")))
			out := js.O("node", node)
			if want["cpu_models"] {
				if models, err := client.GetList(ctx, base+"/cpu", params); err != nil {
					out.Set("cpu_models", js.O("error", errMessage(err)))
				} else {
					custom := []any{}
					builtIn := js.NewObj()
					for _, m := range models {
						if m.Truthy("custom") {
							custom = append(custom, m.Get("name"))
						}
					}
					for _, m := range models {
						if m.Truthy("custom") {
							continue
						}
						k := "generic"
						if !m.Truthy("abstract") {
							k = js.Str(or(orUndef(m.Get("vendor")), "other"))
						}
						list, _ := builtIn.Get(k).([]any)
						builtIn.Set(k, append(list, m.Get("name")))
					}
					out.Set("cpu_models", proxmox.Compact(js.Spread(builtIn, js.O("custom", custom))))
				}
			}
			if want["machines"] {
				if machines, err := client.GetList(ctx, base+"/machines", params); err != nil {
					out.Set("machines", js.O("error", errMessage(err)))
				} else {
					byType := js.NewObj()
					for _, m := range machines {
						k := js.Str(or(m.Get("type"), "other"))
						list, _ := byType.Get(k).([]any)
						byType.Set(k, append(list, m.Get("id")))
					}
					out.Set("machines", byType)
				}
			}
			if want["cpu_flags"] {
				if flags, err := client.GetList(ctx, base+"/cpu-flags", params); err != nil {
					out.Set("cpu_flags", js.O("error", errMessage(err)))
				} else {
					out.Set("cpu_flags", mapObjs(flags, func(f *Obj) any { return proxmox.C("name", f.Get("name"), "description", f.Get("description")) }))
				}
			}
			return out, nil
		},
	})

	// ---------------------------------------------------------------- resource mappings

	c.Define("proxmox_list_resource_mappings", Def{
		Toolset:     "hardware",
		Title:       "List resource mappings",
		Description: "List cluster resource mappings of one kind: PCI or USB devices, or directories (for virtiofs), each mapping a logical name to the device/path on every node. VMs reference them as e.g. hostpci0: mapping=<id>, which allows migration. With check_node, Proxmox validates the entries for that node.",
		Input: []Prop{
			P("kind", hardwareMappingKindArg),
			P("check_node", Str().Opt().D("Validate the mappings against this node's hardware and report problems")),
			P("search", searchArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			list, err := client.GetList(ctx, "/cluster/mapping/"+a.Str("kind"), defined(js.O("check-node", a.Get("check_node"))))
			if err != nil {
				return nil, err
			}
			sortObjs(list, func(x, y *Obj) float64 { return compareStr(x.Get("id"), y.Get("id")) })
			res := filterAndLimit(list, a.Get("search"), js.Undef, func(m *Obj) []any {
				return append([]any{m.Get("id"), m.Get("description")}, m.Arr("map")...)
			})
			return js.Spread(js.O("kind", a.Get("kind")), res.Map(func(m *Obj) any { return hardwareSummarizeMapping(m) })), nil
		},
	})

	c.Define("proxmox_get_resource_mapping", Def{
		Toolset:     "hardware",
		Title:       "Get a resource mapping",
		Description: "Get one PCI, USB or directory resource mapping with its per-node entries and digest.",
		Input:       []Prop{P("kind", hardwareMappingKindArg), P("id", Str().D("Mapping ID"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			mAny, err := client.Get(ctx, "/cluster/mapping/"+a.Str("kind")+"/"+enc(a.Get("id")), nil)
			if err != nil {
				return nil, err
			}
			m, _ := mAny.(*Obj)
			return proxmox.Compact(js.Spread(hardwareSummarizeMapping(js.Spread(js.O("id", a.Get("id")), m)), js.O("digest", m.Get("digest")))), nil
		},
	})

	c.Define("proxmox_save_resource_mapping", Def{
		Toolset:     "hardware",
		Title:       "Create or update a resource mapping",
		Description: "Create (create=true) or update a cluster resource mapping for PCI or USB passthrough or a virtiofs directory. `map` lists one entry per node (it replaces the whole list when given). Get device paths and IDs from proxmox_list_node_pci_devices / proxmox_list_node_usb_devices.",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("kind", hardwareMappingKindArg),
			P("id", Str().D("Mapping ID (a name such as 'gpu0')")),
			P("create", Bool().Opt().D("Create a new mapping (default false = update an existing one)")),
			P("map", Arr(hardwareMapEntryArg).Opt().D("Per-node entries (required when creating)")),
			P("description", Str().Opt().D("Description of the mapping")),
			P("mdev", Bool().Opt().D("pci only: devices provide mediated devices (vGPU)")),
			P("live_migration_capable", Bool().Opt().D("pci only: devices support live migration (experimental)")),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			kind := a.Str("kind")
			if kind != "pci" && (a.Defined("mdev") || a.Defined("live_migration_capable")) {
				return nil, errors.New("mdev and live_migration_capable only apply to PCI mappings")
			}
			var mapList any = js.Undef
			if a.Defined("map") {
				entries := a.Arr("map")
				out := make([]any, len(entries))
				for i, e := range entries {
					out[i] = hardwareMapEntryToString(e)
				}
				mapList = out
			}
			path := "/cluster/mapping/" + kind
			params := defined(js.O("description", a.Get("description"), "mdev", a.Get("mdev"), "live-migration-capable", a.Get("live_migration_capable")))
			create := a.Truthy("create")
			if create {
				if arr, _ := mapList.([]any); len(arr) == 0 {
					return nil, errors.New("`map` is required when creating a mapping")
				}
				body, err := definedExtra(js.Spread(js.O("id", a.Get("id"), "map", mapList), params), a.Get("extra"))
				if err != nil {
					return nil, err
				}
				if _, err := client.Post(ctx, path, body); err != nil {
					return nil, err
				}
			} else {
				// The USB update endpoint requires `map`; resend the current entries when it isn't changing.
				if js.IsUndef(mapList) && kind == "usb" {
					curAny, err := client.Get(ctx, path+"/"+enc(a.Get("id")), nil)
					if err != nil {
						return nil, err
					}
					cur, _ := curAny.(*Obj)
					mapList = hardwareMapList(cur, js.Undef)
				}
				body, err := definedExtra(js.Spread(js.O("map", mapList), params, js.O("delete", deleteParam(a.Get("delete")), "digest", a.Get("digest"))), a.Get("extra"))
				if err != nil {
					return nil, err
				}
				if _, err := client.Put(ctx, path+"/"+enc(a.Get("id")), body); err != nil {
					return nil, err
				}
			}
			var mapping any = js.Undef
			if saved, err := client.Get(ctx, path+"/"+enc(a.Get("id")), nil); err == nil {
				if o, ok := saved.(*Obj); ok && o != nil {
					mapping = hardwareSummarizeMapping(js.Spread(js.O("id", a.Get("id")), o))
				}
			}
			return proxmox.C("saved", a.Get("id"), "kind", kind, "action", pick(create, "created", "updated"), "mapping", mapping), nil
		},
	})

	c.Define("proxmox_delete_resource_mapping", Def{
		Toolset:     "hardware",
		Title:       "Delete a resource mapping",
		Description: "Delete a PCI, USB or directory resource mapping. VMs still referencing it will fail to start.",
		Delete:      true,
		Input:       []Prop{P("kind", hardwareMappingKindArg), P("id", Str().D("Mapping ID"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/cluster/mapping/"+a.Str("kind")+"/"+enc(a.Get("id")), nil); err != nil {
				return nil, err
			}
			return js.O("deleted", a.Get("id"), "kind", a.Get("kind")), nil
		},
	})

	// ---------------------------------------------------------------- custom CPU models

	c.Define("proxmox_list_custom_cpu_models", Def{
		Toolset:     "hardware",
		Title:       "List custom CPU models",
		Description: "List cluster-wide custom CPU models (base model plus flag changes). VMs use them as cpu: custom-<name>. Related: proxmox_get_node_capabilities for built-in models and flags.",
		Input:       []Prop{P("raw", rawArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			raw, err := client.Get(ctx, "/cluster/qemu/custom-cpu-models", nil)
			if err != nil {
				return nil, err
			}
			if a.Truthy("raw") {
				return proxmox.RedactSecrets(raw), nil
			}
			return mapObjs(js.Objs(raw), func(m *Obj) any {
				var flags any = js.Undef
				if m.Truthy("flags") {
					flags = js.Array(strings.Split(js.Str(m.Get("flags")), ";"))
				}
				return proxmox.C(
					"name", m.Get("cputype"),
					"reported_model", m.Get("reported-model"),
					"flags", flags,
					"hidden", ifThen(m.Truthy("hidden"), true),
					"hv_vendor_id", m.Get("hv-vendor-id"),
					"phys_bits", m.Get("phys-bits"),
					"guest_phys_bits", m.Get("guest-phys-bits"),
					"level", m.Get("level"),
				)
			}), nil
		},
	})

	c.Define("proxmox_get_custom_cpu_model", Def{
		Toolset:     "hardware",
		Title:       "Get a custom CPU model",
		Description: "Get one custom CPU model definition, including its digest.",
		Input:       []Prop{P("name", Str().D("Model name, with or without the 'custom-' prefix"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			return client.Get(ctx, "/cluster/qemu/custom-cpu-models/"+enc(a.Get("name")), nil)
		},
	})

	c.Define("proxmox_save_custom_cpu_model", Def{
		Toolset:     "hardware",
		Title:       "Create or update a custom CPU model",
		Description: "Create (create=true) or update a custom CPU model, e.g. a migration-safe baseline with selected flags. Only given fields change on update. Running VMs pick up changes on their next start.",
		Write:       true,
		Destructive: False,
		Input: append(append([]Prop{
			P("name", Str().D("Model name (the 'custom-' prefix is optional)")),
			P("create", Bool().Opt().D("Create a new model (default false = update an existing one)")),
		}, hardwareCPUModelFields...),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("extra", extraArg),
		),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			create := a.Truthy("create")
			if create && !a.Truthy("reported_model") {
				return nil, errors.New("`reported_model` is required when creating a custom CPU model")
			}
			var flags any = js.Undef
			if f := a.Strs("flags"); len(f) > 0 {
				flags = strings.Join(f, ";")
			}
			params := defined(js.O(
				"reported-model", a.Get("reported_model"),
				"flags", flags,
				"hidden", a.Get("hidden"),
				"hv-vendor-id", a.Get("hv_vendor_id"),
				"phys-bits", a.Get("phys_bits"),
				"guest-phys-bits", a.Get("guest_phys_bits"),
				"level", a.Get("level"),
			))
			base := "/cluster/qemu/custom-cpu-models"
			var err error
			if create {
				var body *Obj
				if body, err = definedExtra(js.Spread(js.O("cputype", a.Get("name")), params), a.Get("extra")); err == nil {
					_, err = client.Post(ctx, base, body)
				}
			} else {
				var body *Obj
				if body, err = definedExtra(js.Spread(params, js.O("delete", deleteParam(a.Get("delete")), "digest", a.Get("digest"))), a.Get("extra")); err == nil {
					_, err = client.Put(ctx, base+"/"+enc(a.Get("name")), body)
				}
			}
			if err != nil {
				return nil, err
			}
			saved, err := client.Get(ctx, base+"/"+enc(a.Get("name")), nil)
			if err != nil {
				saved = js.Undef
			}
			return proxmox.C("saved", a.Get("name"), "action", pick(create, "created", "updated"), "model", saved), nil
		},
	})

	c.Define("proxmox_delete_custom_cpu_model", Def{
		Toolset:     "hardware",
		Title:       "Delete a custom CPU model",
		Description: "Delete a custom CPU model. VMs still using it (cpu: custom-<name>) will fail to start.",
		Delete:      true,
		Input:       []Prop{P("name", Str().D("Model name, with or without the 'custom-' prefix"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/cluster/qemu/custom-cpu-models/"+enc(a.Get("name")), nil); err != nil {
				return nil, err
			}
			return js.O("deleted", a.Get("name")), nil
		},
	})
}
