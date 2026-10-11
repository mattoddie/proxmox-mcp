import { z } from "zod";
import type { ProxmoxClient } from "../proxmox/client.js";
import { compact, percent, usage } from "../proxmox/format.js";
import { summarizeGuest } from "./cluster.js";
import { defined, defineTool, rawArg, searchArg, filterAndLimit, type ToolContext } from "./util.js";

type Obj = Record<string, any>;

const poolIdArg = z.string().describe("Pool ID, e.g. \"production\" (nested pools as \"parent/child\"; see proxmox_list_pools)");

function memberCounts(members: Obj[] = []): Obj {
  return {
    vms: members.filter((m) => m.type === "qemu").length,
    containers: members.filter((m) => m.type === "lxc").length,
    storage: members.filter((m) => m.type === "storage").length,
  };
}

function summarizeMember(m: Obj): Obj {
  if (m.type === "storage") {
    return compact({
      type: "storage",
      storage: m.storage,
      node: m.node,
      status: m.status,
      plugin: m.plugintype,
      usage: usage(m.disk, m.maxdisk),
      used_pct: percent(m.disk, m.maxdisk),
    });
  }
  return summarizeGuest(m);
}

/** GET /pools?poolid=X returns a one-element array on PVE 8+. */
async function getPool(client: ProxmoxClient, poolid: string, type?: string): Promise<Obj> {
  const res = await client.get<Obj[] | Obj>("/pools", defined({ poolid, type }));
  const pool = Array.isArray(res) ? res.find((p) => p.poolid === poolid) ?? res[0] : res;
  if (!pool) throw new Error(`No pool ${poolid} (see proxmox_list_pools)`);
  return pool;
}

const list = (v?: (string | number)[]) => (v?.length ? v.join(",") : undefined);

export function registerPoolsTools(ctx: ToolContext): void {
  const { client } = ctx;

  defineTool(ctx, "proxmox_list_pools", {
    toolset: "pools",
    title: "List resource pools",
    description:
      "List resource pools (groups of VMs, containers and storage used for permissions and organisation) with their comments and member counts. Not ZFS or Ceph pools; see proxmox_get_pool for members.",
    input: { search: searchArg },
    handler: async (a) => {
      const pools = await client.get<Obj[]>("/pools");
      pools.sort((x, y) => String(x.poolid).localeCompare(String(y.poolid)));
      const res = filterAndLimit(pools, { search: a.search }, (p) => [p.poolid, p.comment]);
      // The list endpoint has no members; fetch each pool (pools are few).
      const detailed = await Promise.all(res.items.map((p) => getPool(client, p.poolid).catch(() => undefined)));
      return {
        ...res,
        items: res.items.map((p, i) => compact({ poolid: p.poolid, comment: p.comment, members: detailed[i] ? memberCounts(detailed[i]!.members) : undefined })),
      };
    },
  });

  defineTool(ctx, "proxmox_get_pool", {
    toolset: "pools",
    title: "Get resource pool",
    description: "Get a resource pool's comment and members: guests (status, node, CPU/memory use) and storage (usage).",
    input: {
      poolid: poolIdArg,
      type: z.enum(["qemu", "lxc", "storage"]).optional().describe("Only members of this type"),
      raw: rawArg,
    },
    handler: async (a) => {
      const pool = await getPool(client, a.poolid, a.type);
      if (a.raw) return pool;
      const members: Obj[] = pool.members ?? [];
      return compact({
        poolid: pool.poolid,
        comment: pool.comment,
        counts: memberCounts(members),
        guests: members.filter((m) => m.type !== "storage").map(summarizeMember),
        storage: members.filter((m) => m.type === "storage").map(summarizeMember),
      });
    },
  });

  defineTool(ctx, "proxmox_create_pool", {
    toolset: "pools",
    title: "Create resource pool",
    description: "Create a resource pool. Add VMs, containers and storage to it with proxmox_update_pool.",
    input: { poolid: poolIdArg, comment: z.string().optional().describe("Description") },
    write: true,
    destructive: false,
    handler: async (a) => {
      await client.post("/pools", defined({ poolid: a.poolid, comment: a.comment }));
      return { poolid: a.poolid, created: true };
    },
  });

  defineTool(ctx, "proxmox_update_pool", {
    toolset: "pools",
    title: "Update resource pool",
    description:
      "Change a resource pool's comment and add or remove member guests and storage. A guest can only be in one pool; set allow_move to move it here from another pool.",
    input: {
      poolid: poolIdArg,
      comment: z.string().optional().describe("New description"),
      add_vms: z.array(z.number().int()).optional().describe("Guest VMIDs to add"),
      remove_vms: z.array(z.number().int()).optional().describe("Guest VMIDs to remove"),
      add_storage: z.array(z.string()).optional().describe("Storage IDs to add"),
      remove_storage: z.array(z.string()).optional().describe("Storage IDs to remove"),
      allow_move: z.boolean().optional().describe("Move added guests out of their current pool instead of failing"),
    },
    write: true,
    destructive: false,
    handler: async (a) => {
      const adding = a.add_vms?.length || a.add_storage?.length;
      const removing = a.remove_vms?.length || a.remove_storage?.length;
      if (a.comment === undefined && !adding && !removing) throw new Error("Nothing to change");
      // The API's `delete` flag applies to the whole call, so adds and removes need separate requests.
      if (a.comment !== undefined || adding) {
        await client.put(
          "/pools",
          defined({ poolid: a.poolid, comment: a.comment, vms: list(a.add_vms), storage: list(a.add_storage), "allow-move": a.allow_move }),
        );
      }
      if (removing) {
        await client.put("/pools", defined({ poolid: a.poolid, vms: list(a.remove_vms), storage: list(a.remove_storage), delete: true }));
      }
      return compact({
        poolid: a.poolid,
        comment: a.comment,
        added: compact({ vms: a.add_vms, storage: a.add_storage }),
        removed: compact({ vms: a.remove_vms, storage: a.remove_storage }),
      });
    },
  });

  defineTool(ctx, "proxmox_delete_pool", {
    toolset: "pools",
    title: "Delete resource pool",
    description: "Delete a resource pool. It must be empty: remove its members with proxmox_update_pool first. The guests and storage themselves are not affected.",
    input: { poolid: poolIdArg },
    delete: true,
    handler: async (a) => {
      await client.delete("/pools", { poolid: a.poolid });
      return { poolid: a.poolid, deleted: true };
    },
  });
}
