import { z } from "zod";
import { compact, parsePropertyString, redactSecrets } from "../proxmox/format.js";
import {
  defined,
  defineTool,
  deleteKeysArg,
  deleteParam,
  digestArg,
  enc,
  extraArg,
  filterAndLimit,
  limitArg,
  nodeArg,
  rawArg,
  searchArg,
  type ToolContext,
} from "./util.js";

type Obj = Record<string, any>;

/** Common PCI base classes (first two hex digits of the class code), for filtering by name. */
const PCI_CLASSES: Record<string, string> = {
  "01": "storage",
  "02": "network",
  "03": "display",
  "04": "multimedia",
  "05": "memory",
  "06": "bridge",
  "07": "communication",
  "08": "system",
  "09": "input",
  "0c": "serial-bus",
  "0d": "wireless",
  "12": "accelerator",
};
const CLASS_NAMES = ["storage", "network", "display", "multimedia", "bridge", "serial-bus", "wireless", "accelerator"] as const;

function pciClassCode(cls: unknown): string {
  return String(cls ?? "").replace(/^0x/i, "").slice(0, 2).toLowerCase();
}

function summarizePci(d: Obj, groupPeers: Map<number, string[]>): Obj {
  const group = Number(d.iommugroup);
  return compact({
    id: d.id,
    class: d.class,
    class_name: PCI_CLASSES[pciClassCode(d.class)],
    vendor: d.vendor_name,
    device: d.device_name,
    ids: d.vendor && d.device ? `${String(d.vendor).replace(/^0x/, "")}:${String(d.device).replace(/^0x/, "")}` : undefined,
    subsystem: d.subsystem_vendor_name || d.subsystem_device_name ? `${d.subsystem_vendor_name ?? ""} ${d.subsystem_device_name ?? ""}`.trim() : undefined,
    subsystem_id:
      d.subsystem_vendor && d.subsystem_device
        ? `${String(d.subsystem_vendor).replace(/^0x/, "")}:${String(d.subsystem_device).replace(/^0x/, "")}`
        : undefined,
    iommu_group: Number.isFinite(group) ? group : undefined,
    iommu_group_peers: group >= 0 ? groupPeers.get(group)?.filter((id) => id !== d.id) : undefined,
    mdev: d.mdev ? true : undefined,
  });
}

// ---------------------------------------------------------------- resource mappings

const MAPPING_KINDS = ["pci", "usb", "dir"] as const;
type MappingKind = (typeof MAPPING_KINDS)[number];

const mappingKindArg = z.enum(MAPPING_KINDS).describe("Mapping kind: pci (passthrough devices), usb, or dir (virtiofs directories)");

const mapEntryArg = z.union([
  z.string().describe("Raw property string, e.g. 'node=pve1,path=0000:01:00.0,id=10de:2204,iommugroup=12'"),
  z.object({
    node: z.string().describe("Cluster node"),
    path: z.string().optional().describe("pci: device address(es) e.g. 0000:01:00.0 (';'-separated for alternatives); usb: port path e.g. 1-2; dir: absolute directory"),
    id: z.string().optional().describe("pci/usb: vendor:device ID, e.g. 10de:2204 (required for pci and usb)"),
    iommugroup: z.number().int().optional().describe("pci: expected IOMMU group"),
    subsystem_id: z.string().optional().describe("pci: expected subsystem vendor:device ID"),
    description: z.string().optional().describe("Per-node description"),
  }),
]);

function mapEntryToString(e: z.infer<typeof mapEntryArg>): string {
  if (typeof e === "string") return e;
  const parts = [`node=${e.node}`];
  if (e.path !== undefined) parts.push(`path=${e.path}`);
  if (e.id !== undefined) parts.push(`id=${e.id}`);
  if (e.iommugroup !== undefined) parts.push(`iommugroup=${e.iommugroup}`);
  if (e.subsystem_id !== undefined) parts.push(`subsystem-id=${e.subsystem_id}`);
  if (e.description !== undefined) parts.push(`description=${e.description}`);
  return parts.join(",");
}

function summarizeMapping(m: Obj): Obj {
  const map: string[] = Array.isArray(m.map) ? m.map : m.map ? [m.map] : [];
  return compact({
    id: m.id,
    description: m.description,
    mdev: m.mdev ? true : undefined,
    live_migration_capable: m["live-migration-capable"] ? true : undefined,
    map: map.map((e) => parsePropertyString(e)),
    checks: m.checks ?? m.error,
  });
}

// ---------------------------------------------------------------- custom CPU models

const cpuModelFields = {
  reported_model: z.string().optional().describe("Base QEMU CPU model reported to the guest, e.g. host, kvm64, x86-64-v2-AES, EPYC-Rome, Skylake-Server (required when creating)"),
  flags: z.array(z.string()).optional().describe("CPU flags to add (+flag) or remove (-flag), e.g. [\"+aes\", \"-pcid\"]"),
  hidden: z.boolean().optional().describe("Hide that the guest runs under KVM"),
  hv_vendor_id: z.string().optional().describe("Hyper-V vendor ID reported to Windows guests"),
  phys_bits: z.string().optional().describe("Physical address bits reported to the guest (8-64 or 'host')"),
  guest_phys_bits: z.number().int().min(32).max(64).optional().describe("Physical address bits usable by the guest"),
  level: z.number().int().min(0).optional().describe("Maximum basic CPUID leaf"),
};

export function registerHardwareTools(ctx: ToolContext): void {
  const { client } = ctx;

  // ---------------------------------------------------------------- node hardware

  defineTool(ctx, "proxmox_list_node_pci_devices", {
    toolset: "hardware",
    title: "List PCI devices",
    description:
      "List a node's PCI devices for passthrough planning: address, class, vendor/device names and IDs, IOMMU group (with the other devices in the same group, which must be passed through together) and whether it supports mediated devices (vGPU). Memory controllers, bridges and processors are hidden unless all=true. IOMMU group -1 means IOMMU is off.",
    input: {
      node: nodeArg,
      class: z
        .union([z.enum(CLASS_NAMES), z.string().regex(/^(0x)?[0-9a-fA-F]{2,6}$/)])
        .optional()
        .describe("Only this class: a name (display, network, storage, multimedia, serial-bus, accelerator...) or hex class code prefix, e.g. 03 or 0300"),
      mdev_only: z.boolean().optional().describe("Only devices that support mediated devices (vGPU)"),
      all: z.boolean().optional().describe("Include memory controllers, bridges and processors (hidden by default)"),
      search: searchArg,
      limit: limitArg(500),
      raw: rawArg,
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const devices = await client.get<Obj[]>(`/nodes/${enc(node)}/hardware/pci`, a.all ? { "pci-class-blacklist": "" } : undefined);
      const groupPeers = new Map<number, string[]>();
      for (const d of devices) {
        const g = Number(d.iommugroup);
        if (g >= 0) groupPeers.set(g, [...(groupPeers.get(g) ?? []), d.id]);
      }
      let list = devices;
      if (a.class) {
        const byName = Object.entries(PCI_CLASSES).find(([, n]) => n === a.class)?.[0];
        const prefix = (byName ?? String(a.class).replace(/^0x/i, "")).toLowerCase();
        list = list.filter((d) => String(d.class ?? "").replace(/^0x/i, "").toLowerCase().startsWith(prefix));
      }
      if (a.mdev_only) list = list.filter((d) => d.mdev);
      list.sort((x, y) => String(x.id).localeCompare(String(y.id)));
      const res = filterAndLimit(list, { search: a.search, limit: a.limit ?? 500 }, (d) => [
        d.id,
        d.vendor_name,
        d.device_name,
        d.subsystem_vendor_name,
        d.subsystem_device_name,
        d.vendor,
        d.device,
      ]);
      return {
        node,
        iommu_enabled: devices.some((d) => Number(d.iommugroup) >= 0),
        ...res,
        items: a.raw ? res.items : res.items.map((d) => summarizePci(d, groupPeers)),
      };
    },
  });

  defineTool(ctx, "proxmox_list_node_mdev_types", {
    toolset: "hardware",
    title: "List mediated device types",
    description:
      "List the mediated device (mdev, e.g. NVIDIA vGPU / Intel GVT-g) types a PCI device offers, with how many instances are still available. Use a device with mdev support from proxmox_list_node_pci_devices.",
    input: {
      node: nodeArg,
      pci_id: z.string().describe("PCI address (e.g. 0000:01:00.0) or the ID of a PCI resource mapping"),
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const types = await client.get<Obj[]>(`/nodes/${enc(node)}/hardware/pci/${enc(a.pci_id)}/mdev`);
      return {
        node,
        pci_id: a.pci_id,
        types: types.map((t) => compact({ type: t.type, name: t.name, available: t.available, description: t.description })),
      };
    },
  });

  defineTool(ctx, "proxmox_list_node_usb_devices", {
    toolset: "hardware",
    title: "List USB devices",
    description:
      "List a node's USB devices for passthrough: vendor:product ID, manufacturer and product names, bus/port path, speed and serial. Pass a device to a VM by ID (host=vendor:product) or port (host=bus-port), or via a USB resource mapping.",
    input: { node: nodeArg, search: searchArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const devices = await client.get<Obj[]>(`/nodes/${enc(node)}/hardware/usb`);
      const res = filterAndLimit(devices, { search: a.search }, (d) => [d.vendid, d.prodid, d.manufacturer, d.product, d.serial, d.usbpath]);
      return {
        node,
        ...res,
        items: res.items.map((d) =>
          compact({
            id: d.vendid && d.prodid ? `${d.vendid}:${d.prodid}` : undefined,
            manufacturer: d.manufacturer,
            product: d.product,
            port: d.usbpath ?? (d.busnum !== undefined ? `${d.busnum}-${d.port}` : undefined),
            bus: d.busnum,
            device: d.devnum,
            speed: d.speed,
            class: d.class,
            serial: d.serial,
            hub: Number(d.class) === 9 ? true : undefined,
          }),
        ),
      };
    },
  });

  defineTool(ctx, "proxmox_get_node_capabilities", {
    toolset: "hardware",
    title: "Get node QEMU capabilities",
    description:
      "Get what a node's QEMU supports for VM configuration: CPU models (built-in and custom), machine types/versions (q35, i440fx) and the VM-specific CPU flags that can be toggled. Use it to pick valid values for a VM's cpu/machine options.",
    input: {
      node: nodeArg,
      sections: z
        .array(z.enum(["cpu_models", "machines", "cpu_flags"]))
        .optional()
        .describe("Which parts to return (default all)"),
      arch: z.enum(["x86_64", "aarch64"]).optional().describe("Guest architecture (default the host's)"),
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const base = `/nodes/${enc(node)}/capabilities/qemu`;
      const want = new Set(a.sections?.length ? a.sections : ["cpu_models", "machines", "cpu_flags"]);
      const params = defined({ arch: a.arch });
      const out: Obj = { node };
      await Promise.all([
        want.has("cpu_models") &&
          client.get<Obj[]>(`${base}/cpu`, params).then(
            (models) => {
              const custom = models.filter((m) => m.custom).map((m) => m.name);
              const builtIn: Record<string, string[]> = {};
              for (const m of models.filter((x) => !x.custom)) (builtIn[m.abstract ? "generic" : m.vendor || "other"] ??= []).push(m.name);
              out.cpu_models = compact({ ...builtIn, custom });
            },
            (e: Error) => (out.cpu_models = { error: e.message }),
          ),
        want.has("machines") &&
          client.get<Obj[]>(`${base}/machines`, params).then(
            (machines) => {
              const byType: Record<string, string[]> = {};
              for (const m of machines) (byType[m.type ?? "other"] ??= []).push(m.id);
              out.machines = byType;
            },
            (e: Error) => (out.machines = { error: e.message }),
          ),
        want.has("cpu_flags") &&
          client.get<Obj[]>(`${base}/cpu-flags`, params).then(
            (flags) => (out.cpu_flags = flags.map((f) => compact({ name: f.name, description: f.description }))),
            (e: Error) => (out.cpu_flags = { error: e.message }),
          ),
      ]);
      return out;
    },
  });

  // ---------------------------------------------------------------- resource mappings

  defineTool(ctx, "proxmox_list_resource_mappings", {
    toolset: "hardware",
    title: "List resource mappings",
    description:
      "List cluster resource mappings of one kind: PCI or USB devices, or directories (for virtiofs), each mapping a logical name to the device/path on every node. VMs reference them as e.g. hostpci0: mapping=<id>, which allows migration. With check_node, Proxmox validates the entries for that node.",
    input: {
      kind: mappingKindArg,
      check_node: z.string().optional().describe("Validate the mappings against this node's hardware and report problems"),
      search: searchArg,
    },
    handler: async (a) => {
      const list = await client.get<Obj[]>(`/cluster/mapping/${a.kind}`, defined({ "check-node": a.check_node }));
      list.sort((x, y) => String(x.id).localeCompare(String(y.id)));
      const res = filterAndLimit(list, { search: a.search }, (m) => [m.id, m.description, ...(Array.isArray(m.map) ? m.map : [])]);
      return { kind: a.kind, ...res, items: res.items.map(summarizeMapping) };
    },
  });

  defineTool(ctx, "proxmox_get_resource_mapping", {
    toolset: "hardware",
    title: "Get a resource mapping",
    description: "Get one PCI, USB or directory resource mapping with its per-node entries and digest.",
    input: { kind: mappingKindArg, id: z.string().describe("Mapping ID") },
    handler: async (a) => {
      const m = await client.get<Obj>(`/cluster/mapping/${a.kind}/${enc(a.id)}`);
      return compact({ ...summarizeMapping({ id: a.id, ...m }), digest: m?.digest });
    },
  });

  defineTool(ctx, "proxmox_save_resource_mapping", {
    toolset: "hardware",
    title: "Create or update a resource mapping",
    description:
      "Create (create=true) or update a cluster resource mapping for PCI or USB passthrough or a virtiofs directory. `map` lists one entry per node (it replaces the whole list when given). Get device paths and IDs from proxmox_list_node_pci_devices / proxmox_list_node_usb_devices.",
    write: true,
    destructive: false,
    input: {
      kind: mappingKindArg,
      id: z.string().describe("Mapping ID (a name such as 'gpu0')"),
      create: z.boolean().optional().describe("Create a new mapping (default false = update an existing one)"),
      map: z.array(mapEntryArg).optional().describe("Per-node entries (required when creating)"),
      description: z.string().optional().describe("Description of the mapping"),
      mdev: z.boolean().optional().describe("pci only: devices provide mediated devices (vGPU)"),
      live_migration_capable: z.boolean().optional().describe("pci only: devices support live migration (experimental)"),
      delete: deleteKeysArg,
      digest: digestArg,
      extra: extraArg,
    },
    handler: async (a) => {
      if (a.kind !== "pci" && (a.mdev !== undefined || a.live_migration_capable !== undefined)) {
        throw new Error("mdev and live_migration_capable only apply to PCI mappings");
      }
      let map = a.map?.map(mapEntryToString);
      const path = `/cluster/mapping/${a.kind}`;
      const params = defined({ description: a.description, mdev: a.mdev, "live-migration-capable": a.live_migration_capable });
      if (a.create) {
        if (!map?.length) throw new Error("`map` is required when creating a mapping");
        await client.post(path, defined({ id: a.id, map, ...params, ...a.extra }));
      } else {
        // The USB update endpoint requires `map`; resend the current entries when it isn't changing.
        if (!map && a.kind === "usb") {
          const cur = await client.get<Obj>(`${path}/${enc(a.id)}`);
          map = Array.isArray(cur?.map) ? cur.map : cur?.map ? [cur.map] : undefined;
        }
        await client.put(`${path}/${enc(a.id)}`, defined({ map, ...params, delete: deleteParam(a.delete), digest: a.digest, ...a.extra }));
      }
      const saved = await client.get<Obj>(`${path}/${enc(a.id)}`).catch(() => undefined);
      return compact({ saved: a.id, kind: a.kind, action: a.create ? "created" : "updated", mapping: saved ? summarizeMapping({ id: a.id, ...saved }) : undefined });
    },
  });

  defineTool(ctx, "proxmox_delete_resource_mapping", {
    toolset: "hardware",
    title: "Delete a resource mapping",
    description: "Delete a PCI, USB or directory resource mapping. VMs still referencing it will fail to start.",
    delete: true,
    input: { kind: mappingKindArg, id: z.string().describe("Mapping ID") },
    handler: async (a) => {
      await client.delete(`/cluster/mapping/${a.kind}/${enc(a.id)}`);
      return { deleted: a.id, kind: a.kind };
    },
  });

  // ---------------------------------------------------------------- custom CPU models

  defineTool(ctx, "proxmox_list_custom_cpu_models", {
    toolset: "hardware",
    title: "List custom CPU models",
    description:
      "List cluster-wide custom CPU models (base model plus flag changes). VMs use them as cpu: custom-<name>. Related: proxmox_get_node_capabilities for built-in models and flags.",
    input: { raw: rawArg },
    handler: async (a) => {
      const models = await client.get<Obj[]>("/cluster/qemu/custom-cpu-models");
      if (a.raw) return redactSecrets(models);
      return models.map((m) =>
        compact({
          name: m.cputype,
          reported_model: m["reported-model"],
          flags: m.flags ? String(m.flags).split(";") : undefined,
          hidden: m.hidden ? true : undefined,
          hv_vendor_id: m["hv-vendor-id"],
          phys_bits: m["phys-bits"],
          guest_phys_bits: m["guest-phys-bits"],
          level: m.level,
        }),
      );
    },
  });

  defineTool(ctx, "proxmox_get_custom_cpu_model", {
    toolset: "hardware",
    title: "Get a custom CPU model",
    description: "Get one custom CPU model definition, including its digest.",
    input: { name: z.string().describe("Model name, with or without the 'custom-' prefix") },
    handler: async (a) => client.get<Obj>(`/cluster/qemu/custom-cpu-models/${enc(a.name)}`),
  });

  defineTool(ctx, "proxmox_save_custom_cpu_model", {
    toolset: "hardware",
    title: "Create or update a custom CPU model",
    description:
      "Create (create=true) or update a custom CPU model, e.g. a migration-safe baseline with selected flags. Only given fields change on update. Running VMs pick up changes on their next start.",
    write: true,
    destructive: false,
    input: {
      name: z.string().describe("Model name (the 'custom-' prefix is optional)"),
      create: z.boolean().optional().describe("Create a new model (default false = update an existing one)"),
      ...cpuModelFields,
      delete: deleteKeysArg,
      digest: digestArg,
      extra: extraArg,
    },
    handler: async (a) => {
      if (a.create && !a.reported_model) throw new Error("`reported_model` is required when creating a custom CPU model");
      const params = defined({
        "reported-model": a.reported_model,
        flags: a.flags?.length ? a.flags.join(";") : undefined,
        hidden: a.hidden,
        "hv-vendor-id": a.hv_vendor_id,
        "phys-bits": a.phys_bits,
        "guest-phys-bits": a.guest_phys_bits,
        level: a.level,
      });
      const base = "/cluster/qemu/custom-cpu-models";
      if (a.create) await client.post(base, defined({ cputype: a.name, ...params, ...a.extra }));
      else await client.put(`${base}/${enc(a.name)}`, defined({ ...params, delete: deleteParam(a.delete), digest: a.digest, ...a.extra }));
      const saved = await client.get<Obj>(`${base}/${enc(a.name)}`).catch(() => undefined);
      return compact({ saved: a.name, action: a.create ? "created" : "updated", model: saved });
    },
  });

  defineTool(ctx, "proxmox_delete_custom_cpu_model", {
    toolset: "hardware",
    title: "Delete a custom CPU model",
    description: "Delete a custom CPU model. VMs still using it (cpu: custom-<name>) will fail to start.",
    delete: true,
    input: { name: z.string().describe("Model name, with or without the 'custom-' prefix") },
    handler: async (a) => {
      await client.delete(`/cluster/qemu/custom-cpu-models/${enc(a.name)}`);
      return { deleted: a.name };
    },
  });
}
