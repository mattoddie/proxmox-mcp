import { z } from "zod";
import type { GuestRef } from "../proxmox/client.js";
import { compact, flag, isoTime, redactSecrets, summarizeDisks, summarizeNets } from "../proxmox/format.js";
import { defined, defineTool, enc, filterAndLimit, guestNodeArg, limitArg, rawArg, runTask, searchArg, vmidArg, waitArg, type ToolContext } from "./util.js";

type Obj = Record<string, any>;

const snapnameArg = z.string().min(1).describe("Snapshot name (see proxmox_list_snapshots)");

const DAY_S = 86400;

function kind(g: GuestRef): string {
  return g.type === "qemu" ? "vm" : "container";
}

function ageDays(snaptime: unknown, now: number): number | undefined {
  const t = Number(snaptime);
  return Number.isFinite(t) && t > 0 ? Math.floor(((now - t) / DAY_S) * 10) / 10 : undefined;
}

/** The snapshot the guest currently runs from: the parent of the pseudo-entry "current". */
function currentParent(list: Obj[]): string | undefined {
  return list.find((s) => s.name === "current")?.parent;
}

function summarizeSnapshot(s: Obj, current: string | undefined, now: number): Obj {
  return compact({
    name: s.name,
    parent: s.parent,
    description: typeof s.description === "string" ? s.description.trim() : undefined,
    time: isoTime(s.snaptime),
    age_days: ageDays(s.snaptime, now),
    vmstate: flag(s.vmstate) || undefined,
    current: s.name === current ? true : undefined,
  });
}

/** Indented lines showing the snapshot tree, with "(current)" on the snapshot the guest is based on. */
function snapshotTree(snaps: Obj[], current: string | undefined): string[] {
  const names = new Set(snaps.map((s) => s.name));
  const children = new Map<string, Obj[]>();
  for (const s of snaps) {
    const p = s.parent && names.has(s.parent) ? s.parent : "";
    children.set(p, [...(children.get(p) ?? []), s]);
  }
  const lines: string[] = [];
  const walk = (parent: string, depth: number) => {
    const kids = (children.get(parent) ?? []).sort((a, b) => Number(a.snaptime ?? 0) - Number(b.snaptime ?? 0));
    for (const s of kids) {
      const when = isoTime(s.snaptime)?.slice(0, 16).replace("T", " ");
      lines.push(`${"  ".repeat(depth)}${s.name}${when ? ` [${when}]` : ""}${s.name === current ? " (current)" : ""}`);
      walk(s.name, depth + 1);
    }
  };
  walk("", 0);
  if (!current && snaps.length) lines.push("(guest is not based on any snapshot)");
  return lines;
}

/** Runs `fn` over `items` with at most `limit` in flight. */
async function mapLimit<T, R>(items: T[], limit: number, fn: (item: T) => Promise<R>): Promise<R[]> {
  const out: R[] = new Array(items.length);
  let next = 0;
  const worker = async () => {
    while (next < items.length) {
      const i = next++;
      out[i] = await fn(items[i]);
    }
  };
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, worker));
  return out;
}

export function registerSnapshotsTools(ctx: ToolContext): void {
  const { client } = ctx;
  const guest = (vmid: number, node?: string) => client.resolveGuest(vmid, { node });
  const snapPath = (g: GuestRef, name: string) => `${client.guestPath(g)}/snapshot/${enc(name)}`;

  // ---------------------------------------------------------------- read

  defineTool(ctx, "proxmox_list_snapshots", {
    toolset: "snapshots",
    title: "List snapshots of a guest",
    description:
      "List the snapshots of a VM or container (the type is detected from the VMID): name, parent, description, creation time, age, whether RAM state was saved (VMs), and which snapshot the guest currently runs from, plus an indented tree view. For stale snapshots across all guests use proxmox_list_all_snapshots.",
    input: { vmid: vmidArg, node: guestNodeArg, raw: rawArg },
    handler: async (a) => {
      const g = await guest(a.vmid, a.node);
      const list = await client.get<Obj[]>(`${client.guestPath(g)}/snapshot`);
      if (a.raw) return redactSecrets(list);
      const current = currentParent(list);
      const snaps = list.filter((s) => s.name !== "current").sort((x, y) => Number(x.snaptime ?? 0) - Number(y.snaptime ?? 0));
      const now = Date.now() / 1000;
      return compact({
        vmid: g.vmid,
        name: g.name,
        type: kind(g),
        node: g.node,
        count: snaps.length,
        current_snapshot: current,
        tree: snapshotTree(snaps, current),
        snapshots: snaps.map((s) => summarizeSnapshot(s, current, now)),
      });
    },
  });

  defineTool(ctx, "proxmox_get_snapshot", {
    toolset: "snapshots",
    title: "Get snapshot details",
    description:
      "Get a snapshot of a VM or container: its description, time, parent, whether RAM was saved, and the guest configuration as it was at that snapshot (disks and networks summarized, secrets redacted). Useful to see what a rollback would restore.",
    input: { vmid: vmidArg, node: guestNodeArg, snapname: snapnameArg, raw: rawArg },
    handler: async (a) => {
      const g = await guest(a.vmid, a.node);
      const config = await client.get<Obj>(`${snapPath(g, a.snapname)}/config`);
      if (a.raw) return redactSecrets(config);
      const { description, snaptime, parent, vmstate, snapstate, ...rest } = config;
      return compact({
        vmid: g.vmid,
        type: kind(g),
        node: g.node,
        snapname: a.snapname,
        description: typeof description === "string" ? description.trim() : undefined,
        time: isoTime(snaptime),
        parent,
        vmstate: vmstate ? true : undefined,
        state: snapstate,
        disks: summarizeDisks(rest),
        nets: summarizeNets(rest),
        config: redactSecrets(rest),
      });
    },
  });

  defineTool(ctx, "proxmox_list_all_snapshots", {
    toolset: "snapshots",
    title: "List snapshots across all guests",
    description:
      "List snapshots of every VM and container in the cluster (or a filtered set), oldest first, with guest, node, time, age in days and description. Use older_than_days to find stale snapshots that hold back storage space and slow down disks. Guests whose snapshots can't be read are reported under errors.",
    input: {
      older_than_days: z.number().min(0).optional().describe("Only snapshots older than this many days"),
      node: z.string().optional().describe("Only guests on this node"),
      type: z.enum(["vm", "container"]).optional().describe("Only VMs or only containers"),
      vmids: z.array(z.number().int().min(100)).optional().describe("Only these guests"),
      search: searchArg,
      limit: limitArg(500),
    },
    handler: async (a) => {
      let guests = await client.get<Obj[]>("/cluster/resources", { type: "vm" });
      guests = guests.filter((r) => r.type === "qemu" || r.type === "lxc");
      if (a.node) guests = guests.filter((r) => r.node === a.node);
      if (a.type) guests = guests.filter((r) => r.type === (a.type === "vm" ? "qemu" : "lxc"));
      if (a.vmids) guests = guests.filter((r) => a.vmids!.includes(Number(r.vmid)));
      // Offline nodes can't answer; skip their guests instead of failing every request.
      const now = Date.now() / 1000;
      const errors: Obj[] = [];
      const results = await mapLimit(guests, 8, async (r) => {
        try {
          const list = await client.get<Obj[]>(`/nodes/${enc(r.node)}/${r.type}/${r.vmid}/snapshot`);
          const current = currentParent(list);
          return list
            .filter((s) => s.name !== "current")
            .map((s): Obj => ({
              vmid: Number(r.vmid),
              guest: r.name,
              type: r.type === "qemu" ? "vm" : "container",
              node: r.node,
              ...summarizeSnapshot(s, current, now),
              snaptime: Number(s.snaptime ?? 0),
            }));
        } catch (err) {
          errors.push({ vmid: Number(r.vmid), node: r.node, error: (err as Error).message });
          return [];
        }
      });
      let snaps = results.flat();
      if (a.older_than_days !== undefined) {
        const cutoff = now - a.older_than_days * DAY_S;
        snaps = snaps.filter((s) => s.snaptime > 0 && s.snaptime < cutoff);
      }
      snaps.sort((x, y) => x.snaptime - y.snaptime || x.vmid - y.vmid);
      const res = filterAndLimit(snaps, { search: a.search, limit: a.limit ?? 500 }, (s) => [s.vmid, s.guest, s.name, s.description, s.node]);
      return compact({
        guests_checked: guests.length,
        ...res,
        items: res.items.map(({ snaptime: _t, ...s }) => s),
        errors,
      });
    },
  });

  // ---------------------------------------------------------------- write

  defineTool(ctx, "proxmox_create_snapshot", {
    toolset: "snapshots",
    title: "Create a snapshot",
    description:
      "Take a snapshot of a VM or container (type detected from the VMID), e.g. before an upgrade. vmstate=true also saves a running VM's RAM so a rollback resumes exactly where it was (VMs only; takes longer and uses disk space). The guest's storage must support snapshots (e.g. LVM-thin, ZFS, Ceph, qcow2); check containers with proxmox_check_container_feature.",
    write: true,
    destructive: false,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      snapname: z
        .string()
        .regex(/^[A-Za-z][A-Za-z0-9_-]{1,39}$/, "2-40 characters: a letter, then letters, digits, - or _")
        .describe("Snapshot name: starts with a letter; letters, digits, - and _ only, e.g. pre_upgrade_2026_10"),
      description: z.string().optional().describe("Description of the snapshot"),
      vmstate: z.boolean().optional().describe("Include RAM state (running VMs only)"),
      wait: waitArg,
    },
    handler: async (a) => {
      const g = await guest(a.vmid, a.node);
      if (a.vmstate && g.type !== "qemu") throw new Error("vmstate is only supported for VMs, not containers");
      if (g.template) throw new Error(`Guest ${g.vmid} is a template; templates can't be snapshotted`);
      const upid = await client.post(
        `${client.guestPath(g)}/snapshot`,
        defined({ snapname: a.snapname, description: a.description, vmstate: g.type === "qemu" ? a.vmstate : undefined }),
      );
      return runTask(client, upid, a.wait, { vmid: g.vmid, type: kind(g), node: g.node, snapname: a.snapname });
    },
  });

  defineTool(ctx, "proxmox_rollback_snapshot", {
    toolset: "snapshots",
    title: "Roll back to a snapshot",
    description:
      "Roll a VM or container back to a snapshot. Destructive: all changes since the snapshot (disk contents and config) are lost, and a running guest is stopped. start=true starts the guest afterwards; VM snapshots that include RAM resume automatically. Newer snapshots are kept, except that on ZFS storage only the most recent snapshot can be rolled back to (delete newer ones first with proxmox_delete_snapshot).",
    write: true,
    destructive: true,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      snapname: snapnameArg,
      start: z.boolean().optional().describe("Start the guest after the rollback (default false)"),
      wait: waitArg,
    },
    handler: async (a) => {
      const g = await guest(a.vmid, a.node);
      const upid = await client.post(`${snapPath(g, a.snapname)}/rollback`, defined({ start: a.start }));
      return runTask(client, upid, a.wait, { vmid: g.vmid, type: kind(g), node: g.node, snapname: a.snapname, rolled_back: true });
    },
  });

  defineTool(ctx, "proxmox_update_snapshot", {
    toolset: "snapshots",
    title: "Update a snapshot description",
    description: "Change the description of a VM or container snapshot. The snapshot's contents can't be changed.",
    write: true,
    destructive: false,
    idempotent: true,
    input: { vmid: vmidArg, node: guestNodeArg, snapname: snapnameArg, description: z.string().describe("New description (empty string clears it)") },
    handler: async (a) => {
      const g = await guest(a.vmid, a.node);
      await client.put(`${snapPath(g, a.snapname)}/config`, { description: a.description });
      return { vmid: g.vmid, snapname: a.snapname, description: a.description, updated: true };
    },
  });

  defineTool(ctx, "proxmox_delete_snapshot", {
    toolset: "snapshots",
    title: "Delete a snapshot",
    description:
      "Delete a snapshot of a VM or container, merging its data so the guest's current state is unaffected (other snapshots are kept). force=true removes it from the config even if deleting the disk snapshots fails (may leave orphaned storage snapshots).",
    delete: true,
    input: {
      vmid: vmidArg,
      node: guestNodeArg,
      snapname: snapnameArg,
      force: z.boolean().optional().describe("Remove from the config even if removing disk snapshots fails"),
      wait: waitArg,
    },
    handler: async (a) => {
      const g = await guest(a.vmid, a.node);
      const upid = await client.delete(snapPath(g, a.snapname), defined({ force: a.force }));
      return runTask(client, upid, a.wait, { vmid: g.vmid, type: kind(g), node: g.node, snapname: a.snapname, deleted: true });
    },
  });
}
