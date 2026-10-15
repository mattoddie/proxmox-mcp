import { z } from "zod";
import type { ProxmoxClient } from "../proxmox/client.js";
import { compact, flag, formatBytes, isoTime, parsePropertyString, percent, redactSecrets, usage } from "../proxmox/format.js";
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
  runTask,
  searchArg,
  waitArg,
  withExtra,
  type ToolContext,
} from "./util.js";

type Obj = Record<string, any>;

const STORAGE_TYPES = [
  "btrfs",
  "cephfs",
  "cifs",
  "dir",
  "esxi",
  "iscsi",
  "iscsidirect",
  "lvm",
  "lvmthin",
  "nfs",
  "pbs",
  "rbd",
  "zfs",
  "zfspool",
] as const;

const CONTENT_TYPES = ["images", "rootdir", "iso", "vztmpl", "backup", "snippets", "import"] as const;

/** Options POST /storage accepts but PUT /storage/{storage} does not (fixed once the storage exists). */
const CREATE_ONLY = ["type", "path", "export", "share", "portal", "target", "vgname", "thinpool", "datastore", "base", "authsupported", "iscsiprovider"];

// redactSecrets() doesn't know these storage-specific secret names.
const STORAGE_SECRET_RE = /^(encryption-key|keyring|password)$/;

function redactStorage<T>(value: T): T {
  const out: any = redactSecrets(value);
  const walk = (v: any): any => {
    if (Array.isArray(v)) return v.map(walk);
    if (v && typeof v === "object") {
      return Object.fromEntries(
        Object.entries(v).map(([k, x]) => [k, STORAGE_SECRET_RE.test(k) && typeof x === "string" && x !== "" ? "<redacted>" : walk(x)]),
      );
    }
    return v;
  };
  return walk(out);
}

const splitList = (v: unknown): string[] | undefined =>
  typeof v === "string" && v !== "" ? v.split(/[,;\s]+/).filter(Boolean) : undefined;

/** Storage definition from /storage, summarized. */
function summarizeStorageConfig(s: Obj): Obj {
  return compact({
    storage: s.storage,
    type: s.type,
    content: splitList(s.content),
    shared: flag(s.shared) || undefined,
    disabled: flag(s.disable) || undefined,
    nodes: splitList(s.nodes),
    path: s.path,
    server: s.server,
    export: s.export,
    share: s.share,
    datastore: s.datastore,
    namespace: s.namespace,
    pool: s.pool,
    vgname: s.vgname,
    thinpool: s.thinpool,
    portal: s.portal,
    target: s.target,
    monhost: s.monhost,
    fs_name: s["fs-name"],
    username: s.username,
    format: s.format,
    prune_backups: s["prune-backups"] ? parsePropertyString(s["prune-backups"]) : undefined,
    max_protected_backups: s["max-protected-backups"],
    options: s.options,
  });
}

/** A /cluster/resources storage entry as per-node status. */
function summarizeStorageStatus(r: Obj): Obj {
  return compact({
    node: r.node,
    status: r.status,
    usage: usage(r.disk, r.maxdisk),
    used_pct: percent(r.disk, r.maxdisk),
    free: r.maxdisk ? formatBytes(Number(r.maxdisk) - Number(r.disk ?? 0)) : undefined,
  });
}

function summarizeVolume(v: Obj): Obj {
  return compact({
    volid: v.volid,
    content: v.content,
    format: v.format,
    size: formatBytes(v.size ?? v["approximate-size"]),
    used: v.used ? formatBytes(v.used) : undefined,
    vmid: v.vmid !== undefined ? Number(v.vmid) : undefined,
    ctime: isoTime(v.ctime),
    notes: v.notes,
    protected: flag(v.protected) || undefined,
    parent: v.parent,
    encrypted: v.encrypted,
    verification: v.verification?.state,
  });
}

function summarizeDisk(d: Obj): Obj {
  const wear = Number(d.wearout);
  return compact({
    devpath: d.devpath,
    type: d.type,
    model: d.model,
    vendor: d.vendor && d.vendor !== "unknown" ? String(d.vendor).trim() : undefined,
    serial: d.serial,
    size: formatBytes(d.size),
    rpm: Number(d.rpm) > 0 ? Number(d.rpm) : undefined,
    health: d.health,
    // The API reports remaining life (100 = new); the web UI shows 100 - value as "Wearout".
    wearout_pct: d.wearout !== undefined && d.wearout !== "N/A" && Number.isFinite(wear) ? 100 - wear : undefined,
    used_by: d.used,
    gpt: flag(d.gpt) || undefined,
    mounted: flag(d.mounted) || undefined,
    osdid: Number(d.osdid) >= 0 ? Number(d.osdid) : undefined,
    parent: d.parent,
    wwn: d.wwn,
    by_id: d.by_id_link,
  });
}

function summarizeVdev(v: Obj): Obj {
  return compact({
    name: v.name,
    state: v.state,
    read: v.read,
    write: v.write,
    cksum: v.cksum,
    msg: v.msg,
    children: Array.isArray(v.children) ? v.children.map(summarizeVdev) : undefined,
  });
}

/** Storage ID from a volume ID like "local:iso/debian.iso". */
function storageOf(volume: string): string | undefined {
  const idx = volume.indexOf(":");
  return idx > 0 ? volume.slice(0, idx) : undefined;
}

async function storageExists(client: ProxmoxClient, storage: string): Promise<boolean> {
  const list = await client.get<Obj[]>("/storage");
  return list.some((s) => s.storage === storage);
}

const storageArg = z.string().describe("Storage ID, e.g. \"local\" or \"local-lvm\" (see proxmox_list_storage)");
const volumeArg = z.string().describe("Volume ID, e.g. \"local:iso/debian-12.iso\" or \"local-lvm:vm-100-disk-0\" (see proxmox_list_storage_content)");
const volStorageArg = z.string().optional().describe("Storage ID; taken from the volume ID's prefix when omitted");
const blockDeviceArg = z.string().describe("Block device path, e.g. \"/dev/sdb\" (see proxmox_list_disks)");

const pruneArg = z
  .object({
    keep_all: z.boolean().optional().describe("Keep all backups (overrides the others)"),
    keep_last: z.number().int().min(0).optional().describe("Keep the last N backups"),
    keep_hourly: z.number().int().min(0).optional().describe("Keep backups for the last N hours"),
    keep_daily: z.number().int().min(0).optional().describe("Keep backups for the last N days"),
    keep_weekly: z.number().int().min(0).optional().describe("Keep backups for the last N weeks"),
    keep_monthly: z.number().int().min(0).optional().describe("Keep backups for the last N months"),
    keep_yearly: z.number().int().min(0).optional().describe("Keep backups for the last N years"),
  })
  .optional()
  .describe("Backup retention for this storage (prune-backups), e.g. {keep_last: 3, keep_daily: 7}");

function pruneString(p: Obj | undefined): string | undefined {
  if (!p) return undefined;
  const parts = Object.entries(p)
    .filter(([, v]) => v !== undefined)
    .map(([k, v]) => `${k.replace(/_/g, "-")}=${typeof v === "boolean" ? (v ? 1 : 0) : v}`);
  return parts.length ? parts.join(",") : undefined;
}

/** Scan types that make the node connect to a server named in the arguments. */
const REMOTE_SCAN_TYPES = new Set(["nfs", "cifs", "pbs", "iscsi"]);

export function registerStorageTools(ctx: ToolContext): void {
  const { client } = ctx;

  // ---------------------------------------------------------------- storage definitions

  defineTool(ctx, "proxmox_list_storage", {
    toolset: "storage",
    title: "List storage",
    description:
      "List the datacenter's storage definitions (local dirs, LVM, ZFS, NFS, CIFS, PBS, Ceph...) with type, allowed content, shared/enabled flags, node restriction, server/path/pool details and backup retention, merged with each node's live status and usage. Use proxmox_list_storage_content to see what's on one.",
    input: {
      type: z.enum(STORAGE_TYPES).optional().describe("Only storage of this type"),
      content: z.enum(CONTENT_TYPES).optional().describe("Only storage that allows this content type"),
      node: z.string().optional().describe("Only storage available on this node"),
      search: searchArg,
      raw: rawArg,
    },
    handler: async (a) => {
      const [configs, resources] = await Promise.all([
        client.get<Obj[]>("/storage", defined({ type: a.type })),
        client.get<Obj[]>("/cluster/resources", { type: "storage" }).catch(() => [] as Obj[]),
      ]);
      let list = configs;
      if (a.content) list = list.filter((s) => (splitList(s.content) ?? []).includes(a.content!));
      if (a.node) {
        list = list.filter((s) => {
          const nodes = splitList(s.nodes);
          return nodes ? nodes.includes(a.node!) : resources.some((r) => r.storage === s.storage && r.node === a.node) || !resources.length;
        });
      }
      list.sort((x, y) => String(x.storage).localeCompare(String(y.storage)));
      const res = filterAndLimit(list, { search: a.search }, (s) => [s.storage, s.type, s.path, s.server, s.pool, s.content]);
      if (a.raw) return { ...res, items: redactStorage(res.items) };
      return {
        ...res,
        items: res.items.map((s) => {
          const status = resources
            .filter((r) => r.storage === s.storage && (!a.node || r.node === a.node))
            .sort((x, y) => String(x.node).localeCompare(String(y.node)))
            .map(summarizeStorageStatus);
          return compact({ ...summarizeStorageConfig(s), status });
        }),
      };
    },
  });

  defineTool(ctx, "proxmox_get_storage", {
    toolset: "storage",
    title: "Get storage",
    description:
      "Get one storage definition (full config, secrets redacted) and its status/usage on each node. Pass `node` for that node's detailed status (active, enabled, available bytes).",
    input: { storage: storageArg, node: z.string().optional().describe("Also fetch detailed status on this node") },
    handler: async (a) => {
      const [config, resources, nodeStatus] = await Promise.all([
        client.get<Obj>(`/storage/${enc(a.storage)}`),
        client.get<Obj[]>("/cluster/resources", { type: "storage" }).catch(() => [] as Obj[]),
        a.node ? client.get<Obj>(`/nodes/${enc(a.node)}/storage/${enc(a.storage)}/status`) : Promise.resolve(undefined),
      ]);
      return compact({
        summary: summarizeStorageConfig(config),
        config: redactStorage(config),
        status: resources.filter((r) => r.storage === a.storage).map(summarizeStorageStatus),
        node_status: nodeStatus
          ? compact({
              node: a.node,
              type: nodeStatus.type,
              active: flag(nodeStatus.active),
              enabled: flag(nodeStatus.enabled),
              shared: flag(nodeStatus.shared),
              content: splitList(nodeStatus.content),
              usage: usage(nodeStatus.used, nodeStatus.total),
              available: formatBytes(nodeStatus.avail),
            })
          : undefined,
      });
    },
  });

  defineTool(ctx, "proxmox_save_storage", {
    toolset: "storage",
    title: "Create or update storage",
    description:
      "Create a storage definition, or update an existing one (decided by whether the storage ID exists). Typed fields cover dir, nfs, cifs, lvm, lvmthin, zfspool, pbs, cephfs, rbd and iscsi; anything else goes in `extra`. On update, type and location fields (path, export, share, vgname, thinpool, datastore, portal, target) can't be changed. Use proxmox_scan_storage first to discover NFS exports, CIFS shares, PBS datastores, VGs and pools. Passwords and keys are redacted from the output.",
    input: {
      storage: z.string().describe("Storage ID to create or update, e.g. \"nas-backup\""),
      type: z.enum(STORAGE_TYPES).optional().describe("Storage type (required when creating)"),
      content: z.array(z.enum(CONTENT_TYPES)).optional().describe("Allowed content types; images = VM disks, rootdir = container volumes"),
      nodes: z.array(z.string()).optional().describe("Restrict the storage to these nodes (default all nodes)"),
      shared: z.boolean().optional().describe("Mark as shared: same content on all nodes (needed for migration without copying; e.g. for a dir on a cluster filesystem)"),
      disable: z.boolean().optional().describe("Disable the storage"),
      path: z.string().optional().describe("dir/btrfs: filesystem path (create only)"),
      server: z.string().optional().describe("nfs/cifs/pbs: server name or IP"),
      export: z.string().optional().describe("nfs: export path (create only)"),
      share: z.string().optional().describe("cifs: share name (create only)"),
      subdir: z.string().optional().describe("cifs/cephfs: subdirectory to mount"),
      domain: z.string().optional().describe("cifs: SMB domain/workgroup"),
      smbversion: z.enum(["default", "2.0", "2.1", "3", "3.0", "3.11"]).optional().describe("cifs: SMB protocol version"),
      options: z.string().optional().describe("nfs/cifs: mount options, e.g. \"vers=4.2\""),
      username: z.string().optional().describe("cifs/pbs: user name (pbs: user@realm or API token ID); rbd: Ceph user id"),
      password: z.string().optional().describe("cifs/pbs: password or PBS API token secret"),
      datastore: z.string().optional().describe("pbs: datastore name (create only)"),
      namespace: z.string().optional().describe("pbs: namespace"),
      fingerprint: z.string().optional().describe("pbs: server certificate SHA-256 fingerprint"),
      encryption_key: z.string().optional().describe("pbs: client-side encryption key, or \"autogen\" to generate one"),
      port: z.number().int().optional().describe("pbs/esxi: non-default port"),
      vgname: z.string().optional().describe("lvm/lvmthin: volume group name (create only)"),
      thinpool: z.string().optional().describe("lvmthin: thin pool LV name (create only)"),
      saferemove: z.boolean().optional().describe("lvm: zero-out data when removing LVs"),
      pool: z.string().optional().describe("zfspool: ZFS pool/dataset, e.g. \"rpool/data\"; rbd: Ceph pool"),
      blocksize: z.string().optional().describe("zfspool: block size, e.g. \"16k\""),
      sparse: z.boolean().optional().describe("zfspool: thin-provisioned (sparse) zvols"),
      monhost: z.string().optional().describe("rbd/cephfs (external Ceph): monitor addresses"),
      fs_name: z.string().optional().describe("cephfs: Ceph filesystem name"),
      krbd: z.boolean().optional().describe("rbd: always use the krbd kernel module"),
      keyring: z.string().optional().describe("rbd/cephfs (external Ceph): client keyring contents"),
      portal: z.string().optional().describe("iscsi: portal IP or name (create only)"),
      target: z.string().optional().describe("iscsi: target IQN (create only)"),
      format: z.enum(["raw", "qcow2", "subvol", "vmdk"]).optional().describe("Default image format"),
      preallocation: z.enum(["off", "metadata", "falloc", "full"]).optional().describe("Preallocation for raw/qcow2 images"),
      is_mountpoint: z.string().optional().describe("dir: \"yes\" (or a path) if the path is an externally managed mount point; storage goes offline when not mounted"),
      prune_backups: pruneArg,
      max_protected_backups: z.number().int().min(-1).optional().describe("Maximum protected backups per guest (-1 = unlimited)"),
      bwlimit: z.string().optional().describe("I/O bandwidth limits in KiB/s, e.g. \"default=100000,restore=50000\""),
      delete: deleteKeysArg,
      digest: digestArg,
      extra: extraArg,
    },
    write: true,
    destructive: false,
    idempotent: true,
    handler: async (a) => {
      const exists = await storageExists(client, a.storage);
      const params: Obj = defined(withExtra({
        type: a.type,
        content: a.content?.join(","),
        nodes: a.nodes?.join(","),
        shared: a.shared,
        disable: a.disable,
        path: a.path,
        server: a.server,
        export: a.export,
        share: a.share,
        subdir: a.subdir,
        domain: a.domain,
        smbversion: a.smbversion,
        options: a.options,
        username: a.username,
        password: a.password,
        datastore: a.datastore,
        namespace: a.namespace,
        fingerprint: a.fingerprint,
        "encryption-key": a.encryption_key,
        port: a.port,
        vgname: a.vgname,
        thinpool: a.thinpool,
        saferemove: a.saferemove,
        pool: a.pool,
        blocksize: a.blocksize,
        sparse: a.sparse,
        monhost: a.monhost,
        "fs-name": a.fs_name,
        krbd: a.krbd,
        keyring: a.keyring,
        portal: a.portal,
        target: a.target,
        format: a.format,
        preallocation: a.preallocation,
        is_mountpoint: a.is_mountpoint,
        "prune-backups": pruneString(a.prune_backups),
        "max-protected-backups": a.max_protected_backups,
        bwlimit: a.bwlimit,
      }, a.extra));
      let result: unknown;
      if (exists) {
        const fixed = CREATE_ONLY.filter((k) => params[k] !== undefined);
        if (fixed.length) {
          throw new Error(
            `Storage ${a.storage} already exists and ${fixed.join(", ")} can't be changed on update; delete and recreate it, or drop those arguments`,
          );
        }
        result = await client.put(`/storage/${enc(a.storage)}`, { ...params, delete: deleteParam(a.delete), digest: a.digest });
      } else {
        if (!a.type && !params.type) throw new Error(`Storage ${a.storage} doesn't exist yet; pass \`type\` to create it`);
        if (a.delete?.length || a.digest) throw new Error("`delete` and `digest` only apply when updating an existing storage");
        result = await client.post("/storage", { storage: a.storage, ...params });
      }
      const generatedKey = a.encryption_key === "autogen" || params["encryption-key"] === "autogen";
      return compact({
        storage: a.storage,
        action: exists ? "updated" : "created",
        result: result ? redactStorage(result) : undefined,
        note: generatedKey
          ? `An encryption key was generated and stored at /etc/pve/priv/storage/${a.storage}.enc; back it up somewhere safe, backups can't be restored without it.`
          : undefined,
      });
    },
  });

  defineTool(ctx, "proxmox_delete_storage", {
    toolset: "storage",
    title: "Delete storage",
    description:
      "Remove a storage definition from the datacenter configuration. The data on the underlying disk/share is not deleted, but guests with disks on it will break. To destroy a local ZFS pool/LVM/directory and its disks use proxmox_delete_disk_storage.",
    input: { storage: storageArg },
    delete: true,
    handler: async (a) => {
      await client.delete(`/storage/${enc(a.storage)}`);
      return { storage: a.storage, deleted: true };
    },
  });

  // ---------------------------------------------------------------- content

  defineTool(ctx, "proxmox_list_storage_content", {
    toolset: "storage",
    title: "List storage content",
    description:
      "List the volumes on a storage: ISO images, container templates, backups, VM/CT disks, snippets and import files, with format, size, owner VMID, creation time, notes and protection. Filter by content type or VMID (e.g. all backups of guest 100).",
    input: {
      node: nodeArg,
      storage: storageArg,
      content: z.enum(CONTENT_TYPES).optional().describe("Only this content type"),
      vmid: z.number().int().optional().describe("Only volumes owned by this guest"),
      search: searchArg,
      limit: limitArg(500),
      raw: rawArg,
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const items = await client.get<Obj[]>(`/nodes/${enc(node)}/storage/${enc(a.storage)}/content`, defined({ content: a.content, vmid: a.vmid }));
      items.sort((x, y) => String(x.volid).localeCompare(String(y.volid)));
      const res = filterAndLimit(items, { search: a.search, limit: a.limit ?? 500 }, (v) => [v.volid, v.notes, v.vmid, v.format]);
      return { node, storage: a.storage, ...res, items: a.raw ? res.items : res.items.map(summarizeVolume) };
    },
  });

  defineTool(ctx, "proxmox_get_volume", {
    toolset: "storage",
    title: "Get volume attributes",
    description: "Get a volume's attributes: path on disk, format, size, used space, notes and protection flag.",
    input: { node: nodeArg, volume: volumeArg, storage: volStorageArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const storage = a.storage ?? storageOf(a.volume);
      if (!storage) throw new Error("Pass `storage` or a full volume ID (storage:volume)");
      const v = await client.get<Obj>(`/nodes/${enc(node)}/storage/${enc(storage)}/content/${enc(a.volume)}`);
      return compact({
        volume: a.volume,
        node,
        path: v.path,
        format: v.format,
        size: formatBytes(v.size),
        size_bytes: v.size,
        used: v.used ? formatBytes(v.used) : undefined,
        notes: v.notes,
        protected: flag(v.protected),
      });
    },
  });

  defineTool(ctx, "proxmox_update_volume", {
    toolset: "storage",
    title: "Update volume attributes",
    description: "Set a volume's notes or protection flag. Protection (backups only) prevents pruning and deletion until removed.",
    input: {
      node: nodeArg,
      volume: volumeArg,
      storage: volStorageArg,
      notes: z.string().optional().describe("New notes (\"\" clears them)"),
      protected: z.boolean().optional().describe("Protect (true) or unprotect (false) the volume; only supported for backups"),
    },
    write: true,
    destructive: false,
    idempotent: true,
    handler: async (a) => {
      if (a.notes === undefined && a.protected === undefined) throw new Error("Nothing to change; pass `notes` and/or `protected`");
      const node = await client.resolveNode(a.node);
      const storage = a.storage ?? storageOf(a.volume);
      if (!storage) throw new Error("Pass `storage` or a full volume ID (storage:volume)");
      await client.put(`/nodes/${enc(node)}/storage/${enc(storage)}/content/${enc(a.volume)}`, defined({ notes: a.notes, protected: a.protected }));
      return compact({ volume: a.volume, updated: true, notes: a.notes, protected: a.protected });
    },
  });

  defineTool(ctx, "proxmox_delete_volume", {
    toolset: "storage",
    title: "Delete volume",
    description:
      "Permanently delete a volume (ISO, template, backup or disk image) from storage. Disks still referenced by a guest config can't be deleted; detach them first. Protected backups must be unprotected with proxmox_update_volume.",
    input: { node: nodeArg, volume: volumeArg, storage: volStorageArg, wait: waitArg },
    delete: true,
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const storage = a.storage ?? storageOf(a.volume);
      if (!storage) throw new Error("Pass `storage` or a full volume ID (storage:volume)");
      const res = await client.delete(`/nodes/${enc(node)}/storage/${enc(storage)}/content/${enc(a.volume)}`);
      return runTask(client, res, a.wait, { volume: a.volume, node, action: "delete" });
    },
  });

  defineTool(ctx, "proxmox_allocate_disk", {
    toolset: "storage",
    title: "Allocate disk image",
    description:
      "Allocate a new, empty disk image on a storage, owned by a guest (it shows up as unused until attached). Most of the time adding a disk via the VM/container config tools is easier; use this for pre-allocating. Returns the new volume ID.",
    input: {
      node: nodeArg,
      storage: storageArg,
      vmid: z.number().int().min(100).describe("Owner guest VMID"),
      filename: z.string().describe("Volume name, e.g. \"vm-100-disk-2\" (add .qcow2/.raw on file-based storage)"),
      size: z.string().describe("Size with suffix M or G (plain number = KiB), e.g. \"32G\""),
      format: z.enum(["raw", "qcow2", "subvol", "vmdk"]).optional().describe("Image format (default depends on the storage)"),
    },
    write: true,
    destructive: false,
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const res = await client.post(
        `/nodes/${enc(node)}/storage/${enc(a.storage)}/content`,
        defined({ vmid: a.vmid, filename: a.filename, size: a.size, format: a.format }),
      );
      return { node, storage: a.storage, volid: res };
    },
  });

  // ---------------------------------------------------------------- downloads and templates

  defineTool(ctx, "proxmox_download_to_storage", {
    toolset: "storage",
    title: "Download file to storage",
    description:
      "Have a node download an ISO image, container template (vztmpl) or importable disk/OVA (import) from a URL straight onto a storage, optionally verifying a checksum and decompressing. Use proxmox_query_url_metadata to check the URL and file name first; for official LXC templates use proxmox_download_appliance_template.",
    input: {
      node: nodeArg,
      storage: storageArg,
      url: z.string().describe("URL to download"),
      content: z.enum(["iso", "vztmpl", "import"]).describe("Content type: iso, vztmpl (container template) or import (disk images/OVA)"),
      filename: z.string().describe("Target file name, e.g. \"debian-12.7.0-amd64-netinst.iso\" (normalized by Proxmox)"),
      checksum: z.string().optional().describe("Expected checksum of the file"),
      checksum_algorithm: z.enum(["md5", "sha1", "sha224", "sha256", "sha384", "sha512"]).optional().describe("Checksum algorithm (required with checksum)"),
      compression: z.string().optional().describe("Decompress the download with this algorithm, e.g. \"zst\", \"gz\", \"lzo\" (ISOs only)"),
      verify_certificates: z.boolean().optional().describe("Verify the server's TLS certificate (default true)"),
      wait: waitArg,
    },
    write: true,
    destructive: false,
    handler: async (a) => {
      if (a.checksum && !a.checksum_algorithm) throw new Error("Pass checksum_algorithm together with checksum");
      const node = await client.resolveNode(a.node);
      const res = await client.post(
        `/nodes/${enc(node)}/storage/${enc(a.storage)}/download-url`,
        defined({
          url: a.url,
          content: a.content,
          filename: a.filename,
          checksum: a.checksum,
          "checksum-algorithm": a.checksum_algorithm,
          compression: a.compression,
          "verify-certificates": a.verify_certificates,
        }),
      );
      return runTask(client, res, a.wait, { node, storage: a.storage, volid: `${a.storage}:${a.content}/${a.filename}` });
    },
  });

  defineTool(ctx, "proxmox_list_appliance_templates", {
    toolset: "storage",
    title: "List appliance templates",
    description:
      "List the LXC container templates available for download from the Proxmox appliance index (Debian, Ubuntu, Alpine..., and TurnKey apps). Download one with proxmox_download_appliance_template. If the list is empty, the node may need `pveam update`.",
    input: {
      node: nodeArg,
      section: z.string().optional().describe("Only this section, e.g. \"system\" or \"turnkeylinux\""),
      search: searchArg,
      limit: limitArg(200),
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      let items = await client.get<Obj[]>(`/nodes/${enc(node)}/aplinfo`);
      if (a.section) items = items.filter((t) => t.section === a.section);
      items.sort((x, y) => String(x.section).localeCompare(String(y.section)) || String(x.template).localeCompare(String(y.template)));
      const res = filterAndLimit(items, { search: a.search, limit: a.limit ?? 200 }, (t) => [t.template, t.package, t.os, t.headline]);
      return {
        node,
        ...res,
        items: res.items.map((t) =>
          compact({ template: t.template, section: t.section, os: t.os, version: t.version, headline: t.headline, type: t.type, architecture: t.architecture }),
        ),
      };
    },
  });

  defineTool(ctx, "proxmox_download_appliance_template", {
    toolset: "storage",
    title: "Download appliance template",
    description:
      "Download an LXC template from the Proxmox appliance index onto a storage that allows vztmpl content. Template names come from proxmox_list_appliance_templates.",
    input: {
      node: nodeArg,
      storage: storageArg,
      template: z.string().describe("Template name, e.g. \"debian-12-standard_12.7-1_amd64.tar.zst\""),
      wait: waitArg,
    },
    write: true,
    destructive: false,
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const res = await client.post(`/nodes/${enc(node)}/aplinfo`, { storage: a.storage, template: a.template });
      return runTask(client, res, a.wait, { node, storage: a.storage, volid: `${a.storage}:vztmpl/${a.template}` });
    },
  });

  // ---------------------------------------------------------------- stats and discovery

  defineTool(ctx, "proxmox_get_storage_stats", {
    toolset: "storage",
    title: "Get storage usage history",
    description: "Summarize a storage's usage over time from its RRD data (current, min, max, growth over the period) with a few sample points. Useful for capacity planning.",
    input: {
      node: nodeArg,
      storage: storageArg,
      timeframe: z.enum(["hour", "day", "week", "month", "year"]).optional().describe("Period (default day)"),
      cf: z.enum(["AVERAGE", "MAX"]).optional().describe("Consolidation function (default AVERAGE)"),
      samples: z.number().int().min(0).max(100).optional().describe("Number of evenly spaced sample points to include (default 12)"),
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const timeframe = a.timeframe ?? "day";
      const rows = await client.get<Obj[]>(`/nodes/${enc(node)}/storage/${enc(a.storage)}/rrddata`, defined({ timeframe, cf: a.cf }));
      const pts = rows.filter((r) => Number.isFinite(Number(r.used)) && r.used !== undefined && r.used !== null);
      if (!pts.length) return { node, storage: a.storage, timeframe, points: 0, note: "No usage data for this period" };
      const used = pts.map((p) => Number(p.used));
      const first = pts[0];
      const last = pts[pts.length - 1];
      const n = Math.min(a.samples ?? 12, pts.length);
      const step = n > 1 ? (pts.length - 1) / (n - 1) : 0;
      const samples = Array.from({ length: n }, (_, i) => pts[Math.round(i * step)]).map((p) =>
        compact({ time: isoTime(p.time), used: formatBytes(p.used), used_pct: percent(p.used, p.total) }),
      );
      const growth = Number(last.used) - Number(first.used);
      return compact({
        node,
        storage: a.storage,
        timeframe,
        points: pts.length,
        from: isoTime(first.time),
        to: isoTime(last.time),
        current: usage(last.used, last.total),
        min_used: formatBytes(Math.min(...used)),
        max_used: formatBytes(Math.max(...used)),
        growth: `${growth >= 0 ? "+" : ""}${formatBytes(growth)}`,
        samples,
      });
    },
  });

  defineTool(ctx, "proxmox_scan_storage", {
    toolset: "storage",
    title: "Scan for storage",
    description:
      "Discover storage that can be added with proxmox_save_storage: NFS exports or CIFS shares on a server, PBS datastores, iSCSI targets on a portal, or local LVM volume groups, LVM thin pools and ZFS pools on a node." +
      (ctx.allowWrites
        ? ""
        : " Only the local scans (lvm, lvmthin, zfs) are available: nfs, cifs, pbs and iscsi make the node contact the given server, possibly with credentials, which needs write access (PROXMOX_ALLOW_WRITES=true)."),
    input: {
      node: nodeArg,
      type: z.enum(["nfs", "cifs", "pbs", "iscsi", "lvm", "lvmthin", "zfs"]).describe("What to scan for"),
      server: z.string().optional().describe("nfs/cifs/pbs: server name or IP"),
      username: z.string().optional().describe("cifs/pbs: user name (pbs: user@realm or token ID; required for pbs)"),
      password: z.string().optional().describe("cifs/pbs: password or token secret (required for pbs)"),
      domain: z.string().optional().describe("cifs: SMB domain/workgroup"),
      fingerprint: z.string().optional().describe("pbs: server certificate fingerprint"),
      port: z.number().int().optional().describe("pbs: port (default 8007)"),
      portal: z.string().optional().describe("iscsi: portal IP or name, optionally with :port"),
      vg: z.string().optional().describe("lvmthin: volume group to list thin pools of"),
    },
    handler: async (a) => {
      // Remote scans make the node connect to an arbitrary server (and send it credentials), so they count as writes.
      if (REMOTE_SCAN_TYPES.has(a.type) && !ctx.allowWrites) {
        throw new Error(`Scanning ${a.type} makes the node contact another server and needs write access (PROXMOX_ALLOW_WRITES=true); lvm, lvmthin and zfs scans work read-only`);
      }
      const node = await client.resolveNode(a.node);
      const need = (field: keyof typeof a) => {
        if (a[field] === undefined) throw new Error(`Scanning ${a.type} requires \`${String(field)}\``);
      };
      let params: Obj = {};
      switch (a.type) {
        case "nfs":
          need("server");
          params = { server: a.server };
          break;
        case "cifs":
          need("server");
          params = defined({ server: a.server, username: a.username, password: a.password, domain: a.domain });
          break;
        case "pbs":
          need("server");
          need("username");
          need("password");
          params = defined({ server: a.server, username: a.username, password: a.password, fingerprint: a.fingerprint, port: a.port });
          break;
        case "iscsi":
          need("portal");
          params = { portal: a.portal };
          break;
        case "lvmthin":
          need("vg");
          params = { vg: a.vg };
          break;
      }
      const items = await client.get<Obj[]>(`/nodes/${enc(node)}/scan/${a.type}`, params);
      return { node, type: a.type, count: items.length, items: items.map((i) => compact(i)) };
    },
  });

  // ---------------------------------------------------------------- physical disks

  defineTool(ctx, "proxmox_list_disks", {
    toolset: "storage",
    title: "List physical disks",
    description:
      "List a node's physical disks with device path, model, serial, size, type (ssd/hdd/nvme/usb), SMART health, wearout and what uses them (LVM, ZFS, Ceph OSD, partitions, mounted). Use type=unused to find disks free for new storage.",
    input: {
      node: nodeArg,
      type: z.enum(["unused", "journal_disks"]).optional().describe("Only unused disks, or disks usable as Ceph journal/DB"),
      include_partitions: z.boolean().optional().describe("Also list partitions"),
      skip_smart: z.boolean().optional().describe("Skip SMART health checks (faster)"),
      raw: rawArg,
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const disks = await client.get<Obj[]>(
        `/nodes/${enc(node)}/disks/list`,
        defined({ type: a.type, "include-partitions": a.include_partitions, skipsmart: a.skip_smart }),
      );
      disks.sort((x, y) => String(x.devpath).localeCompare(String(y.devpath)));
      return { node, count: disks.length, disks: a.raw ? disks : disks.map(summarizeDisk) };
    },
  });

  defineTool(ctx, "proxmox_get_disk_smart", {
    toolset: "storage",
    title: "Get disk SMART data",
    description: "Get a disk's SMART health and attributes (ATA attribute table, or the text report for NVMe/SAS).",
    input: {
      node: nodeArg,
      disk: blockDeviceArg,
      health_only: z.boolean().optional().describe("Only return the overall health status"),
      raw: rawArg,
    },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const s = await client.get<Obj>(`/nodes/${enc(node)}/disks/smart`, defined({ disk: a.disk, healthonly: a.health_only }));
      if (a.raw) return s;
      return compact({
        node,
        disk: a.disk,
        health: s.health,
        type: s.type,
        attributes: Array.isArray(s.attributes)
          ? s.attributes.map((x: Obj) =>
              compact({ id: x.id, name: x.name, value: x.value, worst: x.worst, threshold: x.threshold, raw: x.raw, fail: x.fail && x.fail !== "-" ? x.fail : undefined }),
            )
          : undefined,
        text: s.text,
      });
    },
  });

  defineTool(ctx, "proxmox_list_zfs_pools", {
    toolset: "storage",
    title: "List ZFS pools",
    description: "List a node's ZFS pools (zpools) with size, allocation, fragmentation, dedup ratio and health. Use proxmox_get_zfs_pool for the vdev tree and scrub status.",
    input: { node: nodeArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const pools = await client.get<Obj[]>(`/nodes/${enc(node)}/disks/zfs`);
      return {
        node,
        pools: pools.map((p) =>
          compact({
            name: p.name,
            health: p.health,
            usage: usage(p.alloc, p.size),
            free: formatBytes(p.free),
            frag_pct: p.frag,
            dedup: p.dedup,
          }),
        ),
      };
    },
  });

  defineTool(ctx, "proxmox_get_zfs_pool", {
    toolset: "storage",
    title: "Get ZFS pool status",
    description: "Get a zpool's detailed status (like `zpool status`): state, status/action messages, last scrub/resilver, errors and the vdev tree with read/write/checksum error counters.",
    input: { node: nodeArg, name: z.string().describe("Pool name, e.g. \"rpool\"") },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const p = await client.get<Obj>(`/nodes/${enc(node)}/disks/zfs/${enc(a.name)}`);
      return compact({
        node,
        name: p.name,
        state: p.state,
        status: p.status,
        action: p.action,
        scan: p.scan,
        errors: p.errors,
        vdevs: Array.isArray(p.children) ? p.children.map(summarizeVdev) : undefined,
      });
    },
  });

  defineTool(ctx, "proxmox_list_lvm", {
    toolset: "storage",
    title: "List LVM volume groups",
    description: "List a node's LVM volume groups with size, free space and physical volumes.",
    input: { node: nodeArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const res = await client.get<Obj>(`/nodes/${enc(node)}/disks/lvm`);
      const vgs: Obj[] = res?.children ?? [];
      return {
        node,
        volume_groups: vgs.map((vg) =>
          compact({
            name: vg.name,
            usage: usage(Number(vg.size) - Number(vg.free), vg.size),
            free: formatBytes(vg.free),
            physical_volumes: (vg.children ?? []).map((pv: Obj) => compact({ name: pv.name, size: formatBytes(pv.size), free: formatBytes(pv.free) })),
          }),
        ),
      };
    },
  });

  defineTool(ctx, "proxmox_list_lvm_thin", {
    toolset: "storage",
    title: "List LVM thin pools",
    description: "List a node's LVM thin pools with data and metadata usage. Thin pools that run out of metadata space fail badly, so watch metadata_usage.",
    input: { node: nodeArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const pools = await client.get<Obj[]>(`/nodes/${enc(node)}/disks/lvmthin`);
      return {
        node,
        thin_pools: pools.map((p) =>
          compact({ name: p.lv, volume_group: p.vg, usage: usage(p.used, p.lv_size), metadata_usage: usage(p.metadata_used, p.metadata_size) }),
        ),
      };
    },
  });

  defineTool(ctx, "proxmox_list_directory_mounts", {
    toolset: "storage",
    title: "List directory mounts",
    description: "List the directory storages Proxmox manages on a node's disks (filesystems mounted under /mnt/pve/<name> via systemd mount units).",
    input: { node: nodeArg },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const dirs = await client.get<Obj[]>(`/nodes/${enc(node)}/disks/directory`);
      return {
        node,
        directories: dirs.map((d) => compact({ path: d.path, device: d.device, filesystem: d.type, options: d.options, unit_file: d.unitfile })),
      };
    },
  });

  defineTool(ctx, "proxmox_create_disk_storage", {
    toolset: "storage",
    title: "Create storage on disks",
    description:
      "Create local storage on unused disks of a node: a ZFS pool (one or more devices with a RAID level), an LVM volume group, an LVM thin pool, or a formatted directory mounted at /mnt/pve/<name>. ALL DATA ON THE DEVICES IS LOST. Find free disks with proxmox_list_disks type=unused. With add_storage the matching storage definition is added too.",
    input: {
      node: nodeArg,
      kind: z.enum(["zfs", "lvm", "lvmthin", "directory"]).describe("What to create"),
      name: z.string().describe("Name of the pool/VG/thin pool/directory (and of the storage when add_storage)"),
      devices: z.array(z.string()).min(1).describe("Block devices, e.g. [\"/dev/sdb\"]; several only for zfs"),
      add_storage: z.boolean().optional().describe("Also add a storage definition for it (default false)"),
      raidlevel: z
        .enum(["single", "mirror", "raid10", "raidz", "raidz2", "raidz3", "draid", "draid2", "draid3"])
        .optional()
        .describe("zfs: RAID level (default single for one device, else required)"),
      ashift: z.number().int().min(9).max(16).optional().describe("zfs: sector size exponent (default 12)"),
      compression: z.enum(["on", "off", "gzip", "lz4", "lzjb", "zle", "zstd"]).optional().describe("zfs: compression (default on)"),
      draid_config: z.string().optional().describe("zfs dRAID: \"data=<n>,spares=<n>\""),
      filesystem: z.enum(["ext4", "xfs"]).optional().describe("directory: filesystem (default ext4)"),
      wait: waitArg,
    },
    write: true,
    destructive: true,
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      let params: Obj;
      if (a.kind === "zfs") {
        const raidlevel = a.raidlevel ?? (a.devices.length === 1 ? "single" : undefined);
        if (!raidlevel) throw new Error("Pass `raidlevel` when creating a ZFS pool on several devices");
        params = defined({
          name: a.name,
          devices: a.devices.join(","),
          raidlevel,
          ashift: a.ashift,
          compression: a.compression,
          "draid-config": a.draid_config,
          add_storage: a.add_storage,
        });
      } else {
        if (a.devices.length !== 1) throw new Error(`${a.kind} takes exactly one device`);
        params = defined({ name: a.name, device: a.devices[0], add_storage: a.add_storage, filesystem: a.kind === "directory" ? a.filesystem : undefined });
      }
      const res = await client.post(`/nodes/${enc(node)}/disks/${a.kind}`, params);
      return runTask(client, res, a.wait, { node, kind: a.kind, name: a.name });
    },
  });

  defineTool(ctx, "proxmox_delete_disk_storage", {
    toolset: "storage",
    title: "Destroy storage on disks",
    description:
      "Destroy a node-local ZFS pool, LVM volume group, LVM thin pool or managed directory mount. ALL DATA ON IT IS LOST. cleanup_config also removes/updates the storage definition; cleanup_disks wipes the disks for reuse.",
    input: {
      node: nodeArg,
      kind: z.enum(["zfs", "lvm", "lvmthin", "directory"]).describe("What to destroy"),
      name: z.string().describe("Pool/VG/thin pool/directory name"),
      volume_group: z.string().optional().describe("lvmthin: the volume group the thin pool is in (required)"),
      cleanup_config: z.boolean().optional().describe("Remove the storage definition (or this node from it)"),
      cleanup_disks: z.boolean().optional().describe("Also wipe the underlying disks"),
      wait: waitArg,
    },
    delete: true,
    destructive: true,
    handler: async (a) => {
      if (a.kind === "lvmthin" && !a.volume_group) throw new Error("Pass `volume_group` to delete an LVM thin pool");
      const node = await client.resolveNode(a.node);
      const res = await client.delete(
        `/nodes/${enc(node)}/disks/${a.kind}/${enc(a.name)}`,
        defined({
          "cleanup-config": a.cleanup_config,
          "cleanup-disks": a.cleanup_disks,
          "volume-group": a.kind === "lvmthin" ? a.volume_group : undefined,
        }),
      );
      return runTask(client, res, a.wait, { node, kind: a.kind, name: a.name, action: "delete" });
    },
  });

  defineTool(ctx, "proxmox_init_disk_gpt", {
    toolset: "storage",
    title: "Initialize disk with GPT",
    description: "Write a new, empty GPT partition table to a disk so it can be used (e.g. for Ceph or manual partitioning). Existing partitions are lost.",
    input: { node: nodeArg, disk: blockDeviceArg, uuid: z.string().optional().describe("UUID for the GPT table"), wait: waitArg },
    // Destroys the disk's partition table (and so its data), like proxmox_wipe_disk.
    delete: true,
    destructive: true,
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const res = await client.post(`/nodes/${enc(node)}/disks/initgpt`, defined({ disk: a.disk, uuid: a.uuid }));
      return runTask(client, res, a.wait, { node, disk: a.disk });
    },
  });

  defineTool(ctx, "proxmox_wipe_disk", {
    toolset: "storage",
    title: "Wipe disk",
    description: "Wipe a disk or partition (filesystem/LVM/ZFS signatures and the start of the device) so it shows as unused. All data on it is lost. Refuses disks that are in use.",
    input: { node: nodeArg, disk: blockDeviceArg, wait: waitArg },
    delete: true,
    destructive: true,
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const res = await client.put(`/nodes/${enc(node)}/disks/wipedisk`, { disk: a.disk });
      return runTask(client, res, a.wait, { node, disk: a.disk });
    },
  });
}
