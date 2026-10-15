import { z } from "zod";
import type { ProxmoxClient } from "../proxmox/client.js";
import { compact, flag, isoTime } from "../proxmox/format.js";
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
  rawArg,
  searchArg,
  withExtra,
  type ToolContext,
} from "./util.js";

type Obj = Record<string, any>;

const sidArg = z
  .union([z.string(), z.number().int()])
  .describe("HA resource ID like vm:100 or ct:200, or just the VMID (100) which is resolved to vm:/ct: automatically");

const sidListArg = z
  .array(z.union([z.string(), z.number().int()]))
  .describe("HA resources, e.g. [\"vm:100\", \"ct:200\"] or plain VMIDs [100, 200]");

const nodeListArg = z
  .array(z.string())
  .describe("Nodes with optional priority (higher is preferred), e.g. [\"pve1:2\", \"pve2:1\"] or [\"pve1\", \"pve2\"]");

const STATES = ["started", "stopped", "enabled", "disabled", "ignored"] as const;

/** vm:100 / ct:200 for plain VMIDs (looked up in /cluster/resources); other forms pass through. */
async function canonicalSids(client: ProxmoxClient, sids: (string | number)[]): Promise<string[]> {
  const numeric = sids.some((s) => /^\d+$/.test(String(s).trim()));
  const guests = numeric ? await client.get<Obj[]>("/cluster/resources", { type: "vm" }) : [];
  return sids.map((s) => {
    const str = String(s).trim();
    if (!/^\d+$/.test(str)) return str;
    const g = guests.find((r) => Number(r.vmid) === Number(str));
    if (!g) throw new Error(`No VM or container with ID ${str}; give the HA resource ID as vm:${str} or ct:${str}`);
    return `${g.type === "lxc" ? "ct" : "vm"}:${str}`;
  });
}

async function canonicalSid(client: ProxmoxClient, sid: string | number): Promise<string> {
  return (await canonicalSids(client, [sid]))[0];
}

function vmidOf(sid: string): number | undefined {
  const m = sid.match(/^(?:vm|ct):(\d+)$/);
  return m ? Number(m[1]) : undefined;
}

function summarizeResource(r: Obj, status?: Obj, guest?: Obj): Obj {
  return compact({
    sid: r.sid,
    name: guest?.name,
    type: r.type,
    state: r.state ?? "started",
    group: r.group,
    max_restart: r.max_restart,
    max_relocate: r.max_relocate,
    failback: r.failback !== undefined ? Boolean(flag(r.failback)) : undefined,
    auto_rebalance: r["auto-rebalance"] !== undefined ? Boolean(flag(r["auto-rebalance"])) : undefined,
    comment: r.comment,
    current: status ? compact({ node: status.node, state: status.state, crm_state: status.crm_state, request_state: status.request_state, status: status.status }) : undefined,
  });
}

function summarizeGroup(g: Obj): Obj {
  return compact({
    group: g.group,
    nodes: g.nodes ? String(g.nodes).split(",") : undefined,
    restricted: Boolean(flag(g.restricted)),
    nofailback: Boolean(flag(g.nofailback)),
    comment: g.comment,
  });
}

function summarizeRule(r: Obj): Obj {
  return compact({
    rule: r.rule,
    type: r.type,
    resources: r.resources ? String(r.resources).split(",") : undefined,
    nodes: r.nodes ? String(r.nodes).split(",") : undefined,
    affinity: r.affinity,
    strict: r.strict !== undefined ? Boolean(flag(r.strict)) : undefined,
    disabled: flag(r.disable) ? true : undefined,
    comment: r.comment,
  });
}

export function registerHaTools(ctx: ToolContext): void {
  const { client } = ctx;

  // ---------------------------------------------------------------- status

  defineTool(ctx, "proxmox_get_ha_status", {
    toolset: "ha",
    title: "Get HA status",
    description:
      "High-availability overview: quorum, the current CRM master, each node's LRM state (active/idle/maintenance, last update), fencing/arming state (PVE 9.1+), and every HA-managed service with its node and state. Use proxmox_list_ha_resources for the configured settings.",
    input: { raw: rawArg },
    handler: async (a) => {
      const [current, manager] = await Promise.all([
        client.get<Obj[]>("/cluster/ha/status/current"),
        client.get<Obj>("/cluster/ha/status/manager_status").catch(() => undefined),
      ]);
      if (a.raw) return { current, manager_status: manager };
      const quorum = current.find((e) => e.type === "quorum");
      const master = current.find((e) => e.type === "master");
      const fencing = current.find((e) => e.type === "fencing");
      const ms = manager?.manager_status ?? {};
      const lrmStatus: Obj = manager?.lrm_status ?? {};
      const nodeStatus: Obj = ms.node_status ?? {};
      const lrmEntries = current.filter((e) => e.type === "lrm");
      const nodeNames = [...new Set([...lrmEntries.map((e) => e.node), ...Object.keys(nodeStatus), ...Object.keys(lrmStatus)])].sort();
      const nodes = nodeNames.map((node) => {
        const entry = lrmEntries.find((e) => e.node === node);
        const lrm = lrmStatus[node] ?? {};
        return compact({
          node,
          crm_node_state: nodeStatus[node],
          lrm_state: lrm.state,
          lrm_mode: lrm.mode,
          status: entry?.status,
          last_update: isoTime(entry?.timestamp ?? lrm.timestamp),
        });
      });
      const services = current
        .filter((e) => e.type === "service")
        .map((e) => compact({ sid: e.sid, node: e.node, state: e.state, crm_state: e.crm_state, request_state: e.request_state, status: e.status }));
      return compact({
        quorum: quorum ? compact({ quorate: Boolean(flag(quorum.quorate)), node: quorum.node, status: quorum.status }) : undefined,
        master: master ? compact({ node: master.node, status: master.status, last_update: isoTime(master.timestamp) }) : undefined,
        fencing: fencing ? compact({ armed_state: fencing["armed-state"], resource_mode: fencing.resource_mode, status: fencing.status }) : undefined,
        nodes,
        services,
        note: services.length ? undefined : "No HA-managed resources (add one with proxmox_save_ha_resource)",
      });
    },
  });

  // ---------------------------------------------------------------- resources

  defineTool(ctx, "proxmox_list_ha_resources", {
    toolset: "ha",
    title: "List HA resources",
    description:
      "List HA-managed VMs and containers with their requested state, group, restart/relocate limits, failback, comment, guest name and current status (node and CRM state).",
    input: {
      type: z.enum(["vm", "ct"]).optional().describe("Only VMs or only containers"),
      search: searchArg,
      limit: limitArg(500),
    },
    handler: async (a) => {
      const [resources, current, guests] = await Promise.all([
        client.get<Obj[]>("/cluster/ha/resources", defined({ type: a.type })),
        client.get<Obj[]>("/cluster/ha/status/current").catch(() => [] as Obj[]),
        client.get<Obj[]>("/cluster/resources", { type: "vm" }).catch(() => [] as Obj[]),
      ]);
      const status = new Map(current.filter((e) => e.type === "service").map((e) => [e.sid, e]));
      const items = resources.map((r) => summarizeResource(r, status.get(r.sid), guests.find((g) => Number(g.vmid) === vmidOf(r.sid))));
      return filterAndLimit(items, { search: a.search, limit: a.limit ?? 500 }, (r) => [r.sid, r.name, r.group, r.comment, r.state]);
    },
  });

  defineTool(ctx, "proxmox_get_ha_resource", {
    toolset: "ha",
    title: "Get an HA resource",
    description: "Get one HA resource's configuration, its current status, and the HA rules that reference it (PVE 9+).",
    input: { sid: sidArg },
    handler: async (a) => {
      const sid = await canonicalSid(client, a.sid);
      const [r, current, rules] = await Promise.all([
        client.get<Obj>(`/cluster/ha/resources/${enc(sid)}`),
        client.get<Obj[]>("/cluster/ha/status/current").catch(() => [] as Obj[]),
        client.get<Obj[]>("/cluster/ha/rules", { resource: sid }).catch(() => undefined),
      ]);
      const status = current.find((e) => e.type === "service" && e.sid === sid);
      return compact({ ...summarizeResource({ sid, ...r }, status), rules: rules?.map(summarizeRule), digest: r.digest });
    },
  });

  defineTool(ctx, "proxmox_save_ha_resource", {
    toolset: "ha",
    title: "Add or update an HA resource",
    description:
      "Put a VM/container under HA management, or update its HA settings if it already is (only the given fields change). state: started (keep running), stopped, disabled (stopped, no recovery), ignored (HA hands off). New resources default to state started, which starts the guest.",
    write: true,
    destructive: true,
    input: {
      sid: sidArg,
      state: z.enum(STATES).optional().describe("Requested state (default started)"),
      group: z.string().optional().describe("HA group (PVE 8; replaced by node-affinity rules in PVE 9)"),
      max_restart: z.number().int().min(0).optional().describe("Restart attempts on the same node after a failed start (default 1)"),
      max_relocate: z.number().int().min(0).optional().describe("Relocation attempts to other nodes after failed restarts (default 1)"),
      failback: z.boolean().optional().describe("Move back to the highest-priority node when it returns (PVE 9, default true)"),
      auto_rebalance: z.boolean().optional().describe("May be moved by automatic load rebalancing (PVE 9.1+, default true)"),
      comment: z.string().optional().describe("Description"),
      delete: deleteKeysArg,
      digest: digestArg,
      extra: extraArg,
    },
    handler: async (a) => {
      const sid = await canonicalSid(client, a.sid);
      const existing = (await client.get<Obj[]>("/cluster/ha/resources")).some((r) => r.sid === sid);
      const params = defined(withExtra({
        state: a.state,
        group: a.group,
        max_restart: a.max_restart,
        max_relocate: a.max_relocate,
        failback: a.failback,
        "auto-rebalance": a.auto_rebalance,
        comment: a.comment,
      }, a.extra));
      if (existing) {
        await client.put(`/cluster/ha/resources/${enc(sid)}`, defined({ ...params, delete: deleteParam(a.delete), digest: a.digest }));
      } else {
        if (a.delete?.length) throw new Error(`${sid} is not an HA resource yet; delete only applies to updates`);
        await client.post("/cluster/ha/resources", { sid, ...params });
      }
      const r = await client.get<Obj>(`/cluster/ha/resources/${enc(sid)}`);
      return { [existing ? "updated" : "created"]: sid, resource: summarizeResource({ sid, ...r }) };
    },
  });

  defineTool(ctx, "proxmox_delete_ha_resource", {
    toolset: "ha",
    title: "Remove an HA resource",
    description:
      "Take a VM/container out of HA management (the guest itself is untouched and keeps running). By default it is also removed from HA rules, deleting rules left empty; purge=false keeps the rules.",
    delete: true,
    destructive: true,
    input: { sid: sidArg, purge: z.boolean().optional().describe("Also remove it from HA rules (default true)") },
    handler: async (a) => {
      const sid = await canonicalSid(client, a.sid);
      await client.delete(`/cluster/ha/resources/${enc(sid)}`, defined({ purge: a.purge }));
      return { deleted: sid };
    },
  });

  defineTool(ctx, "proxmox_ha_migrate", {
    toolset: "ha",
    title: "Migrate or relocate an HA resource",
    description:
      "Ask the HA manager to move an HA-managed guest to another node. migrate = live/online migration; relocate = stop on the old node and start on the target. For HA-managed guests use this rather than the normal migrate tools. The request is queued; follow it with proxmox_get_ha_status. PVE 9 reports resources that block the move (affinity rules) or move along with it.",
    write: true,
    destructive: true,
    input: {
      sid: sidArg,
      action: z.enum(["migrate", "relocate"]).describe("migrate (online) or relocate (stop + start)"),
      node: z.string().describe("Target node"),
    },
    handler: async (a) => {
      const sid = await canonicalSid(client, a.sid);
      const result = await client.post<Obj | null>(`/cluster/ha/resources/${enc(sid)}/${a.action}`, { node: a.node });
      return compact({
        sid,
        action: a.action,
        target: a.node,
        requested: true,
        blocking_resources: result?.["blocking-resources"],
        comigrated_resources: result?.["comigrated-resources"],
        note: "The HA manager carries this out asynchronously; check proxmox_get_ha_status",
      });
    },
  });

  // ---------------------------------------------------------------- groups (PVE 8)

  defineTool(ctx, "proxmox_list_ha_groups", {
    toolset: "ha",
    title: "List HA groups",
    description: "List HA groups (node lists with priorities, restricted, nofailback). Deprecated in PVE 9, where groups are migrated to node-affinity rules (proxmox_list_ha_rules).",
    input: {},
    handler: async () => (await client.get<Obj[]>("/cluster/ha/groups")).map(summarizeGroup),
  });

  defineTool(ctx, "proxmox_get_ha_group", {
    toolset: "ha",
    title: "Get an HA group",
    description: "Get one HA group's configuration (PVE 8; deprecated in PVE 9 in favour of HA rules).",
    input: { group: z.string().describe("HA group name") },
    handler: async (a) => {
      const g = await client.get<Obj>(`/cluster/ha/groups/${enc(a.group)}`);
      return compact({ ...summarizeGroup({ group: a.group, ...g }), digest: g.digest });
    },
  });

  defineTool(ctx, "proxmox_save_ha_group", {
    toolset: "ha",
    title: "Create or update an HA group",
    description:
      "Create an HA group or update an existing one (PVE 8; on PVE 9 use proxmox_save_ha_rule with type node-affinity). restricted: resources may only run on the group's nodes; nofailback: don't move back when a higher-priority node returns.",
    write: true,
    destructive: true,
    input: {
      group: z.string().describe("HA group name"),
      nodes: nodeListArg.optional().describe("Nodes with optional priority, e.g. [\"pve1:2\", \"pve2\"] (required when creating)"),
      restricted: z.boolean().optional().describe("Only run on the listed nodes"),
      nofailback: z.boolean().optional().describe("Don't migrate back to a higher-priority node"),
      comment: z.string().optional().describe("Description"),
      delete: deleteKeysArg,
      digest: digestArg,
      extra: extraArg,
    },
    handler: async (a) => {
      const exists = (await client.get<Obj[]>("/cluster/ha/groups")).some((g) => g.group === a.group);
      const params = defined(withExtra({ nodes: a.nodes?.join(","), restricted: a.restricted, nofailback: a.nofailback, comment: a.comment }, a.extra));
      if (exists) {
        await client.put(`/cluster/ha/groups/${enc(a.group)}`, defined({ ...params, delete: deleteParam(a.delete), digest: a.digest }));
      } else {
        if (!a.nodes?.length) throw new Error("nodes is required when creating an HA group");
        await client.post("/cluster/ha/groups", { group: a.group, ...params });
      }
      return { [exists ? "updated" : "created"]: a.group };
    },
  });

  defineTool(ctx, "proxmox_delete_ha_group", {
    toolset: "ha",
    title: "Delete an HA group",
    description: "Delete an HA group (PVE 8). Fails while HA resources still use it.",
    delete: true,
    input: { group: z.string().describe("HA group name") },
    handler: async (a) => {
      await client.delete(`/cluster/ha/groups/${enc(a.group)}`);
      return { deleted: a.group };
    },
  });

  // ---------------------------------------------------------------- rules (PVE 9)

  defineTool(ctx, "proxmox_list_ha_rules", {
    toolset: "ha",
    title: "List HA rules",
    description:
      "List HA rules (PVE 9+): node-affinity rules (which nodes resources prefer or must run on, with priorities) and resource-affinity rules (keep resources together or apart).",
    input: {
      type: z.enum(["node-affinity", "resource-affinity"]).optional().describe("Only this rule type"),
      resource: sidArg.optional().describe("Only rules affecting this resource (vm:100, ct:200 or a VMID)"),
    },
    handler: async (a) => {
      const resource = a.resource !== undefined ? await canonicalSid(client, a.resource) : undefined;
      const rules = await client.get<Obj[]>("/cluster/ha/rules", defined({ type: a.type, resource }));
      return rules.map(summarizeRule);
    },
  });

  defineTool(ctx, "proxmox_get_ha_rule", {
    toolset: "ha",
    title: "Get an HA rule",
    description: "Get one HA rule (PVE 9+) with its digest.",
    input: { rule: z.string().describe("HA rule ID") },
    handler: async (a) => {
      const r = await client.get<Obj>(`/cluster/ha/rules/${enc(a.rule)}`);
      return compact({ ...summarizeRule({ rule: a.rule, ...r }), digest: r.digest });
    },
  });

  defineTool(ctx, "proxmox_save_ha_rule", {
    toolset: "ha",
    title: "Create or update an HA rule",
    description:
      "Create or update an HA rule (PVE 9+). node-affinity: run `resources` on `nodes` (with priorities; strict=true means only those nodes; affinity negative = avoid them). resource-affinity: affinity positive keeps `resources` on the same node, negative keeps them on different nodes. Only the given fields change on update.",
    write: true,
    destructive: true,
    input: {
      rule: z.string().describe("HA rule ID"),
      type: z.enum(["node-affinity", "resource-affinity"]).optional().describe("Rule type (required when creating)"),
      resources: sidListArg.optional().describe("HA resources the rule applies to, e.g. [\"vm:100\", \"ct:200\"] or VMIDs (required when creating)"),
      nodes: nodeListArg.optional().describe("node-affinity: nodes with optional priority, e.g. [\"pve1:2\", \"pve2:1\"] (required when creating)"),
      affinity: z.enum(["positive", "negative"]).optional().describe("positive (together / on these nodes) or negative (apart / not on these nodes). Required for resource-affinity"),
      strict: z.boolean().optional().describe("node-affinity: only ever run on the listed nodes (default false)"),
      disable: z.boolean().optional().describe("Disable the rule without deleting it"),
      comment: z.string().optional().describe("Description"),
      delete: deleteKeysArg,
      digest: digestArg,
      extra: extraArg,
    },
    handler: async (a) => {
      const existing = (await client.get<Obj[]>("/cluster/ha/rules")).find((r) => r.rule === a.rule);
      const resources = a.resources ? (await canonicalSids(client, a.resources)).join(",") : undefined;
      const params = defined(withExtra({
        resources,
        nodes: a.nodes?.join(","),
        affinity: a.affinity,
        strict: a.strict,
        disable: a.disable,
        comment: a.comment,
      }, a.extra));
      if (existing) {
        if (a.type && existing.type && a.type !== existing.type) throw new Error(`Rule ${a.rule} is a ${existing.type} rule; its type can't be changed`);
        // The section-config update schema is selected by `type`, so it must be sent on updates too.
        await client.put(`/cluster/ha/rules/${enc(a.rule)}`, defined({ type: a.type ?? existing.type, ...params, delete: deleteParam(a.delete), digest: a.digest }));
      } else {
        if (!a.type) throw new Error("type is required when creating an HA rule");
        if (!resources) throw new Error("resources is required when creating an HA rule");
        if (a.type === "node-affinity" && !a.nodes?.length) throw new Error("nodes is required for a node-affinity rule");
        if (a.type === "resource-affinity" && !a.affinity) throw new Error("affinity is required for a resource-affinity rule");
        await client.post("/cluster/ha/rules", { rule: a.rule, type: a.type, ...params });
      }
      const r = await client.get<Obj>(`/cluster/ha/rules/${enc(a.rule)}`).catch(() => undefined);
      return compact({ [existing ? "updated" : "created"]: a.rule, rule: r ? summarizeRule({ rule: a.rule, ...r }) : undefined });
    },
  });

  defineTool(ctx, "proxmox_delete_ha_rule", {
    toolset: "ha",
    title: "Delete an HA rule",
    description: "Delete an HA rule (PVE 9+). The resources it referenced stay HA-managed.",
    delete: true,
    input: { rule: z.string().describe("HA rule ID") },
    handler: async (a) => {
      await client.delete(`/cluster/ha/rules/${enc(a.rule)}`);
      return { deleted: a.rule };
    },
  });

  // ---------------------------------------------------------------- arming (PVE 9.1+)

  defineTool(ctx, "proxmox_set_ha_armed", {
    toolset: "ha",
    title: "Arm or disarm the HA stack",
    description:
      "Disarm the HA stack cluster-wide (releases all watchdogs, so nodes won't self-fence; e.g. for network or switch maintenance) or re-arm it afterwards. PVE 9.1+. resource_mode for disarm: freeze (no HA commands/state changes are applied) or ignore (HA stops managing resources, which can be handled manually). Check the result with proxmox_get_ha_status.",
    write: true,
    destructive: true,
    input: {
      action: z.enum(["arm", "disarm"]).describe("arm or disarm"),
      resource_mode: z.enum(["freeze", "ignore"]).optional().describe("How HA resources are handled while disarmed (required for disarm)"),
    },
    handler: async (a) => {
      if (a.action === "disarm") {
        if (!a.resource_mode) throw new Error("resource_mode (freeze or ignore) is required to disarm");
        await client.post("/cluster/ha/status/disarm-ha", { "resource-mode": a.resource_mode });
      } else {
        await client.post("/cluster/ha/status/arm-ha");
      }
      return { requested: a.action, resource_mode: a.resource_mode, note: "Check progress with proxmox_get_ha_status (fencing.armed_state)" };
    },
  });
}
