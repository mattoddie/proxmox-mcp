import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { errorText, startEnv, type TestEnv } from "./helpers.js";

const PCI = [
  { id: "0000:01:00.0", class: "0x030000", vendor: "0x10de", device: "0x2204", vendor_name: "NVIDIA Corporation", device_name: "GA102 [GeForce RTX 3090]", subsystem_vendor: "0x1462", subsystem_device: "0x3880", subsystem_vendor_name: "MSI", iommugroup: 12, mdev: 0 },
  { id: "0000:01:00.1", class: "0x040300", vendor: "0x10de", device: "0x1aef", vendor_name: "NVIDIA Corporation", device_name: "GA102 HD Audio", iommugroup: 12 },
  { id: "0000:02:00.0", class: "0x020000", vendor: "0x8086", device: "0x1533", vendor_name: "Intel Corporation", device_name: "I210 Gigabit", iommugroup: 13 },
  { id: "0000:03:00.0", class: "0x030000", vendor: "0x8086", device: "0x56a0", vendor_name: "Intel Corporation", device_name: "Arc A770", iommugroup: 14, mdev: 1 },
];

describe("hardware tools", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv({ PROXMOX_DEFAULT_NODE: "pve1" });
  });
  after(async () => {
    await env.stop();
  });
  beforeEach(() => {
    env.mock.reset();
    env.mock.on("GET", /^\/nodes\/pve1\/hardware\/pci$/, () => structuredClone(PCI));
  });

  it("lists PCI devices with IOMMU groups and names", async () => {
    const res = await env.call("proxmox_list_node_pci_devices", {});
    assert.equal(res.node, "pve1");
    assert.equal(res.iommu_enabled, true);
    assert.equal(res.total, 4);
    const gpu = res.items[0];
    assert.deepEqual(gpu, {
      id: "0000:01:00.0",
      class: "0x030000",
      class_name: "display",
      vendor: "NVIDIA Corporation",
      device: "GA102 [GeForce RTX 3090]",
      ids: "10de:2204",
      subsystem: "MSI",
      subsystem_id: "1462:3880",
      iommu_group: 12,
      iommu_group_peers: ["0000:01:00.1"],
    });
    assert.equal(res.items[3].mdev, true);
    assert.equal(env.mock.requestsTo("/nodes/pve1/hardware/pci", "GET")[0].query["pci-class-blacklist"], undefined);
  });

  it("filters PCI devices by class name, class code, mdev and search", async () => {
    const display = await env.call("proxmox_list_node_pci_devices", { class: "display" });
    assert.deepEqual(
      display.items.map((d: any) => d.id),
      ["0000:01:00.0", "0000:03:00.0"],
    );
    const net = await env.call("proxmox_list_node_pci_devices", { class: "0x02" });
    assert.deepEqual(
      net.items.map((d: any) => d.id),
      ["0000:02:00.0"],
    );
    const mdev = await env.call("proxmox_list_node_pci_devices", { mdev_only: true });
    assert.deepEqual(
      mdev.items.map((d: any) => d.id),
      ["0000:03:00.0"],
    );
    const search = await env.call("proxmox_list_node_pci_devices", { search: "rtx" });
    assert.equal(search.total, 1);
  });

  it("includes hidden classes with all=true and validates class codes", async () => {
    await env.call("proxmox_list_node_pci_devices", { all: true });
    assert.equal(env.mock.requestsTo("/nodes/pve1/hardware/pci", "GET")[0].query["pci-class-blacklist"], "");
    const bad = await env.callRaw("proxmox_list_node_pci_devices", { class: "gpu!" });
    assert.equal(bad.isError, true);
  });

  it("reports IOMMU off", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/hardware\/pci$/, () => [{ id: "0000:01:00.0", class: "0x030000", iommugroup: -1 }]);
    const res = await env.call("proxmox_list_node_pci_devices", {});
    assert.equal(res.iommu_enabled, false);
    assert.equal(res.items[0].iommu_group, -1);
    assert.equal(res.items[0].iommu_group_peers, undefined);
  });

  it("lists mdev types", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/hardware\/pci\/([^/]+)\/mdev$/, (_p, m) => [
      { type: "i915-GVTg_V5_4", name: decodeURIComponent(m[1]), available: 2, description: "low_gm_size: 128MB" },
    ]);
    const res = await env.call("proxmox_list_node_mdev_types", { pci_id: "0000:03:00.0" });
    assert.equal(res.types[0].available, 2);
    assert.equal(res.types[0].name, "0000:03:00.0");
  });

  it("lists USB devices", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/hardware\/usb$/, () => [
      { busnum: 1, devnum: 1, port: 0, level: 0, class: 9, vendid: "1d6b", prodid: "0002", product: "xHCI Host Controller", speed: "480" },
      { busnum: 1, devnum: 3, port: 2, level: 1, class: 0, vendid: "0658", prodid: "0200", manufacturer: "Sigma Designs", product: "Aeotec Z-Stick", usbpath: "1-2", speed: "12", serial: "123" },
    ]);
    const res = await env.call("proxmox_list_node_usb_devices", {});
    assert.equal(res.items[0].hub, true);
    assert.deepEqual(res.items[1], { id: "0658:0200", manufacturer: "Sigma Designs", product: "Aeotec Z-Stick", port: "1-2", bus: 1, device: 3, speed: "12", class: 0, serial: "123" });
    const z = await env.call("proxmox_list_node_usb_devices", { search: "z-stick" });
    assert.equal(z.total, 1);
  });

  it("gets node capabilities grouped for the model", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/capabilities\/qemu\/cpu$/, () => [
      { name: "host", vendor: "default", custom: 0 },
      { name: "x86-64-v2-AES", vendor: "default", custom: 0, abstract: 1 },
      { name: "EPYC-Rome", vendor: "AuthenticAMD", custom: 0 },
      { name: "custom-safe", vendor: "GenuineIntel", custom: 1 },
    ]);
    env.mock.on("GET", /^\/nodes\/pve1\/capabilities\/qemu\/machines$/, () => [
      { id: "pc-q35-9.2+pve1", type: "q35", version: "9.2+pve1" },
      { id: "pc-i440fx-9.2", type: "i440fx", version: "9.2" },
    ]);
    env.mock.on("GET", /^\/nodes\/pve1\/capabilities\/qemu\/cpu-flags$/, () => [{ name: "aes", description: "Activate AES instruction set for HW acceleration." }]);
    const res = await env.call("proxmox_get_node_capabilities", {});
    assert.deepEqual(res.cpu_models, { default: ["host"], generic: ["x86-64-v2-AES"], AuthenticAMD: ["EPYC-Rome"], custom: ["custom-safe"] });
    assert.deepEqual(res.machines, { q35: ["pc-q35-9.2+pve1"], i440fx: ["pc-i440fx-9.2"] });
    assert.equal(res.cpu_flags[0].name, "aes");

    const only = await env.call("proxmox_get_node_capabilities", { sections: ["machines"], arch: "aarch64" });
    assert.equal(only.cpu_models, undefined);
    assert.equal(env.mock.requestsTo("/nodes/pve1/capabilities/qemu/machines", "GET").at(-1)!.query.arch, "aarch64");
  });

  it("reports per-section capability errors", async () => {
    const res = await env.call("proxmox_get_node_capabilities", { sections: ["cpu_flags"] });
    assert.match(res.cpu_flags.error, /404/);
  });

  it("lists and gets resource mappings with parsed entries", async () => {
    env.mock.on("GET", /^\/cluster\/mapping\/pci$/, (p) => [
      { id: "gpu", description: "RTX", map: ["node=pve1,path=0000:01:00.0,id=10de:2204,iommugroup=12"], checks: p["check-node"] ? [{ severity: "warning", message: "iommu group changed" }] : undefined },
    ]);
    const res = await env.call("proxmox_list_resource_mappings", { kind: "pci", check_node: "pve1" });
    assert.deepEqual(res.items[0].map, [{ node: "pve1", path: "0000:01:00.0", id: "10de:2204", iommugroup: "12" }]);
    assert.equal(res.items[0].checks[0].severity, "warning");

    env.mock.on("GET", /^\/cluster\/mapping\/dir\/share$/, () => ({ description: "shared", map: ["node=pve1,path=/srv/share"], digest: "dg" }));
    const one = await env.call("proxmox_get_resource_mapping", { kind: "dir", id: "share" });
    assert.deepEqual(one, { id: "share", description: "shared", map: [{ node: "pve1", path: "/srv/share" }], digest: "dg" });
  });

  it("creates a PCI mapping from structured entries", async () => {
    env.mock.on("GET", /^\/cluster\/mapping\/pci\/gpu$/, () => ({ map: ["node=pve1,path=0000:01:00.0,id=10de:2204"], mdev: 0 }));
    const res = await env.call("proxmox_save_resource_mapping", {
      kind: "pci",
      id: "gpu",
      create: true,
      description: "RTX",
      map: [
        { node: "pve1", path: "0000:01:00.0", id: "10de:2204", iommugroup: 12, subsystem_id: "1462:3880" },
        "node=pve2,path=0000:41:00.0,id=10de:2204",
      ],
      live_migration_capable: false,
    });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.equal(w.path, "/cluster/mapping/pci");
    assert.deepEqual(w.body, {
      id: "gpu",
      map: ["node=pve1,path=0000:01:00.0,id=10de:2204,iommugroup=12,subsystem-id=1462:3880", "node=pve2,path=0000:41:00.0,id=10de:2204"],
      description: "RTX",
      "live-migration-capable": "0",
    });
    assert.equal(res.action, "created");
    assert.equal(res.mapping.map[0].node, "pve1");
  });

  it("validates mapping arguments", async () => {
    assert.match(errorText(await env.callRaw("proxmox_save_resource_mapping", { kind: "dir", id: "d", create: true })), /map.*required/);
    assert.match(errorText(await env.callRaw("proxmox_save_resource_mapping", { kind: "usb", id: "u", mdev: true })), /only apply to PCI/);
  });

  it("resends the current map when updating a USB mapping without one", async () => {
    env.mock.on("GET", /^\/cluster\/mapping\/usb\/zstick$/, () => ({ map: ["node=pve1,id=0658:0200"], description: "old" }));
    await env.call("proxmox_save_resource_mapping", { kind: "usb", id: "zstick", description: "Z-Wave stick" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, "/cluster/mapping/usb/zstick");
    assert.deepEqual(w.body, { map: "node=pve1,id=0658:0200", description: "Z-Wave stick" });
  });

  it("updates a dir mapping without resending the map", async () => {
    await env.call("proxmox_save_resource_mapping", { kind: "dir", id: "share", delete: ["description"], digest: "dg" });
    const w = env.mock.lastWrite()!;
    assert.deepEqual(w.body, { delete: "description", digest: "dg" });
  });

  it("deletes a resource mapping", async () => {
    const res = await env.call("proxmox_delete_resource_mapping", { kind: "usb", id: "zstick" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "DELETE");
    assert.equal(w.path, "/cluster/mapping/usb/zstick");
    assert.equal(res.deleted, "zstick");
  });

  it("lists and gets custom CPU models", async () => {
    env.mock.on("GET", /^\/cluster\/qemu\/custom-cpu-models$/, () => [{ cputype: "custom-safe", "reported-model": "EPYC-Rome", flags: "+aes;-pcid", hidden: 1, digest: "x" }]);
    const res = await env.call("proxmox_list_custom_cpu_models", {});
    assert.deepEqual(res, [{ name: "custom-safe", reported_model: "EPYC-Rome", flags: ["+aes", "-pcid"], hidden: true }]);
    env.mock.on("GET", /^\/cluster\/qemu\/custom-cpu-models\/safe$/, () => ({ cputype: "custom-safe", digest: "x" }));
    const one = await env.call("proxmox_get_custom_cpu_model", { name: "safe" });
    assert.equal(one.digest, "x");
  });

  it("creates, updates and deletes custom CPU models", async () => {
    assert.match(errorText(await env.callRaw("proxmox_save_custom_cpu_model", { name: "safe", create: true })), /reported_model/);
    await env.call("proxmox_save_custom_cpu_model", { name: "safe", create: true, reported_model: "EPYC-Rome", flags: ["+aes", "-pcid"], phys_bits: "host" });
    let w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.equal(w.path, "/cluster/qemu/custom-cpu-models");
    assert.deepEqual(w.body, { cputype: "safe", "reported-model": "EPYC-Rome", flags: "+aes;-pcid", "phys-bits": "host" });

    await env.call("proxmox_save_custom_cpu_model", { name: "safe", hidden: true, delete: ["flags"] });
    w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, "/cluster/qemu/custom-cpu-models/safe");
    assert.deepEqual(w.body, { hidden: "1", delete: "flags" });

    await env.call("proxmox_delete_custom_cpu_model", { name: "safe" });
    w = env.mock.lastWrite()!;
    assert.equal(w.method, "DELETE");
    assert.equal(w.path, "/cluster/qemu/custom-cpu-models/safe");
  });
});
