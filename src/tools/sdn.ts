import { z } from "zod";
import type { ProxmoxClient } from "../proxmox/client.js";
import { compact, redactSecrets } from "../proxmox/format.js";
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
  runTask,
  searchArg,
  waitArg,
  withExtra,
  type ToolContext,
} from "./util.js";

type Obj = Record<string, any>;

const KINDS = ["zone", "vnet", "subnet", "controller", "ipam", "dns"] as const;
type Kind = (typeof KINDS)[number];

/** Collection path and identifier parameter name of each SDN object kind. */
const KIND_INFO: Record<Kind, { collection: string; key: string }> = {
  zone: { collection: "zones", key: "zone" },
  vnet: { collection: "vnets", key: "vnet" },
  subnet: { collection: "subnets", key: "subnet" },
  controller: { collection: "controllers", key: "controller" },
  ipam: { collection: "ipams", key: "ipam" },
  dns: { collection: "dns", key: "dns" },
};

/** Kinds whose list/get endpoints understand pending/running. */
const PENDING_KINDS = new Set<Kind>(["zone", "vnet", "subnet", "controller"]);

function collectionPath(kind: Kind, vnet?: string): string {
  if (kind === "subnet") {
    if (!vnet) throw new Error("`vnet` is required for subnets (subnets belong to a vnet)");
    return `/cluster/sdn/vnets/${enc(vnet)}/subnets`;
  }
  return `/cluster/sdn/${KIND_INFO[kind].collection}`;
}

function objectPath(kind: Kind, id: string, vnet?: string): string {
  return `${collectionPath(kind, vnet)}/${enc(id)}`;
}

/**
 * Subnets are stored under "<zone>-<ip>-<prefix>" (e.g. zone1-10.0.0.0-24) and the per-subnet endpoints look
 * them up by that ID; only creation takes the plain CIDR. Translate a CIDR using the vnet's zone.
 */
async function subnetId(client: ProxmoxClient, vnet: string | undefined, id: string): Promise<string> {
  if (!id.includes("/")) return id;
  if (!vnet) throw new Error("`vnet` is required for subnets (subnets belong to a vnet)");
  const v = await client.get<Obj>(`/cluster/sdn/vnets/${enc(vnet)}`, { pending: true });
  const zone = v?.zone ?? v?.pending?.zone;
  if (!zone) throw new Error(`Could not determine the zone of vnet ${vnet}; give the subnet ID (<zone>-<ip>-<prefix>) instead of the CIDR`);
  return `${zone}-${id.replace("/", "-")}`;
}

/** objectPath, with subnet CIDRs translated to subnet IDs. */
async function resolvedObjectPath(client: ProxmoxClient, kind: Kind, id: string, vnet?: string): Promise<string> {
  return objectPath(kind, kind === "subnet" ? await subnetId(client, vnet, id) : id, vnet);
}

const kindArg = z
  .enum(KINDS)
  .describe("SDN object kind: zone, vnet, subnet (needs `vnet`), controller (BGP/EVPN/IS-IS), ipam or dns (DNS plugin)");
const vnetForSubnetArg = z.string().optional().describe("VNet the subnet belongs to (required for kind=subnet)");
const pendingArg = z
  .boolean()
  .optional()
  .describe("Include uncommitted changes: each object shows `state` (new/changed/deleted) and the `pending` values not yet applied with proxmox_apply_sdn");
const runningArg = z.boolean().optional().describe("Show the running (last applied) configuration instead of the edited one");
const lockTokenArg = z
  .string()
  .optional()
  .describe("SDN lock token, only needed when the global SDN configuration has been locked by someone else (PVE 9); normally omit");
const createArg = z.boolean().optional().describe("Create a new object (default false = update an existing one; only the given fields change)");

const SDN_NOTE = "Changes are staged as pending; run proxmox_apply_sdn to activate them on the nodes.";

/** redactSecrets plus the PowerDNS plugin's API key, which is stored under the generic name `key`. */
function redactSdn<T>(value: T): T {
  const out: any = redactSecrets(value);
  if (Array.isArray(out)) return out.map((o) => redactSdn(o)) as T;
  if (out && typeof out === "object" && typeof out.key === "string" && out.key) out.key = "<redacted>";
  return out;
}

/** Drops the digest and redacts plugin credentials (IPAM tokens, DNS keys). */
function summarizeObject(o: Obj): Obj {
  const { digest: _digest, ...rest } = o;
  return redactSdn(compact(rest));
}

/** Shared create-or-update flow for the save tools. */
async function saveObject(
  ctx: ToolContext,
  kind: Kind,
  id: string,
  create: boolean | undefined,
  params: Obj,
  opts: { vnet?: string; createOnly?: Obj; deleteKeys?: string[]; digest?: string; lockToken?: string; extra?: Obj },
): Promise<Obj> {
  const { client } = ctx;
  const common = { ...params, "lock-token": opts.lockToken };
  if (create) {
    await client.post(collectionPath(kind, opts.vnet), defined(withExtra({ [KIND_INFO[kind].key]: id, ...opts.createOnly, ...common }, opts.extra)));
  }
  // After a successful create, failing to resolve the read-back path must not turn the result into an error.
  const path = create ? await resolvedObjectPath(client, kind, id, opts.vnet).catch(() => undefined) : await resolvedObjectPath(client, kind, id, opts.vnet);
  if (!create) {
    await client.put(
      path!,
      defined(withExtra({ ...common, delete: deleteParam(opts.deleteKeys), digest: opts.digest }, opts.extra)),
    );
  }
  const readBack = path
    ? await client.get<Obj>(path, PENDING_KINDS.has(kind) ? { pending: true } : undefined).catch(() => undefined)
    : undefined;
  return compact({ saved: id, kind, action: create ? "created" : "updated", object: readBack ? summarizeObject(readBack) : undefined, note: SDN_NOTE });
}

const nodeListArg = z.array(z.string()).optional().describe("Restrict to these cluster nodes (default all nodes)");
const joinList = (v?: string[]) => (v?.length ? v.join(",") : undefined);

export function registerSdnTools(ctx: ToolContext): void {
  const { client } = ctx;

  // ---------------------------------------------------------------- read

  defineTool(ctx, "proxmox_list_sdn_objects", {
    toolset: "sdn",
    title: "List SDN objects",
    description:
      "List Software-Defined Networking objects of one kind: zones, vnets, subnets of a vnet, controllers, IPAMs or DNS plugins. Use pending=true to see uncommitted changes (state new/changed/deleted). Related: proxmox_get_sdn_object for one object, proxmox_get_sdn_status for the live state on nodes.",
    input: {
      kind: kindArg,
      vnet: vnetForSubnetArg,
      type: z.string().optional().describe("Only objects of this plugin type, e.g. vlan, vxlan, evpn, simple, qinq (zones); bgp, evpn, isis (controllers); pve, netbox, phpipam (ipams)"),
      pending: pendingArg,
      running: runningArg,
      search: searchArg,
      limit: limitArg(500),
      raw: rawArg,
    },
    handler: async (a) => {
      const supportsPending = PENDING_KINDS.has(a.kind);
      const typeFilterServer = ["zone", "controller", "ipam", "dns"].includes(a.kind);
      const items = await client.get<Obj[]>(
        collectionPath(a.kind, a.vnet),
        defined({
          pending: supportsPending ? a.pending : undefined,
          running: supportsPending ? a.running : undefined,
          type: typeFilterServer ? a.type : undefined,
        }),
      );
      const key = KIND_INFO[a.kind].key;
      let list = items;
      if (a.type && !typeFilterServer) list = list.filter((o) => o.type === a.type);
      list.sort((x, y) => String(x[key] ?? x.id ?? "").localeCompare(String(y[key] ?? y.id ?? "")));
      const res = filterAndLimit(list, { search: a.search, limit: a.limit ?? 500 }, (o) => [o[key], o.id, o.type, o.zone, o.alias, o.cidr, o.state]);
      return { kind: a.kind, ...res, items: a.raw ? redactSdn(res.items) : res.items.map(summarizeObject) };
    },
  });

  defineTool(ctx, "proxmox_get_sdn_object", {
    toolset: "sdn",
    title: "Get an SDN object",
    description:
      "Get the full configuration of one SDN zone, vnet, subnet, controller, IPAM or DNS plugin, including its digest for optimistic locking. With pending=true, uncommitted changes are shown under `pending`.",
    input: {
      kind: kindArg,
      id: z.string().describe("Object ID (zone/vnet/controller/ipam/dns name, or subnet ID such as 'zone1-10.0.0.0-24' or its CIDR '10.0.0.0/24')"),
      vnet: vnetForSubnetArg,
      pending: pendingArg,
      running: runningArg,
    },
    handler: async (a) => {
      const supportsPending = PENDING_KINDS.has(a.kind);
      const obj = await client.get<Obj>(
        await resolvedObjectPath(client, a.kind, a.id, a.vnet),
        defined({ pending: supportsPending ? a.pending : undefined, running: supportsPending ? a.running : undefined }),
      );
      return redactSdn(obj);
    },
  });

  defineTool(ctx, "proxmox_get_sdn_status", {
    toolset: "sdn",
    title: "Get SDN status on nodes",
    description:
      "Show the live SDN state on nodes: each zone's status (available/pending/error) per node and the status of the vnets in it. Use it after proxmox_apply_sdn to confirm the configuration came up, or to find nodes where a zone is in error.",
    input: {
      node: z.string().optional().describe("Only this node (default all online nodes)"),
      zone: z.string().optional().describe("Only this zone"),
      vnets: z.boolean().optional().describe("Include per-vnet status inside each zone (default true)"),
    },
    handler: async (a) => {
      const nodes = a.node ? [a.node] : (await client.listNodes()).filter((n) => n.status === "online").map((n) => n.node).sort();
      const out = await Promise.all(
        nodes.map(async (node) => {
          let zones: Obj[];
          try {
            zones = await client.get<Obj[]>(`/nodes/${enc(node)}/sdn/zones`);
          } catch (err) {
            return { node, error: (err as Error).message };
          }
          if (a.zone) zones = zones.filter((z) => z.zone === a.zone);
          zones.sort((x, y) => String(x.zone).localeCompare(String(y.zone)));
          const detailed = await Promise.all(
            zones.map(async (z) => {
              if (a.vnets === false) return compact({ zone: z.zone, status: z.status });
              const content = await client.get<Obj[]>(`/nodes/${enc(node)}/sdn/zones/${enc(z.zone)}/content`).catch(() => undefined);
              return compact({
                zone: z.zone,
                status: z.status,
                vnets: content?.map((v) => compact({ vnet: v.vnet, status: v.status, statusmsg: v.statusmsg })),
              });
            }),
          );
          const problems = detailed.filter((z) => z.status !== "available").map((z) => z.zone);
          return compact({ node, zones: detailed, not_available: problems });
        }),
      );
      return { nodes: out };
    },
  });

  defineTool(ctx, "proxmox_get_sdn_pending_diff", {
    toolset: "sdn",
    title: "Preview SDN apply (dry run)",
    description:
      "Dry-run proxmox_apply_sdn for one node: shows the diff between the running and the pending network interfaces (/etc/network/interfaces.d/sdn) and FRR routing configuration. Use it to review staged SDN changes before applying them.",
    input: { node: z.string().optional().describe("Node to compute the diff for (default PROXMOX_DEFAULT_NODE or the only node)") },
    handler: async (a) => {
      const node = await client.resolveNode(a.node);
      const diff = await client.get<Obj>("/cluster/sdn/dry-run", { node });
      return compact({
        node,
        interfaces_diff: diff?.["interfaces-diff"] || undefined,
        frr_diff: diff?.["frr-diff"] || undefined,
        note: !diff?.["interfaces-diff"] && !diff?.["frr-diff"] ? "No differences: nothing pending for this node" : undefined,
      });
    },
  });

  defineTool(ctx, "proxmox_get_sdn_ipam_status", {
    toolset: "sdn",
    title: "List IPAM entries",
    description:
      "List the IP allocations recorded in an SDN IPAM (default the built-in 'pve' IPAM): IP, MAC, hostname, VMID, vnet/subnet and gateway entries. Use it to find which guest holds an address or which IPs are free. Related: proxmox_save_sdn_vnet_ip to add or change a mapping.",
    input: {
      ipam: z.string().optional().describe("IPAM ID (default 'pve')"),
      vnet: z.string().optional().describe("Only entries in this vnet"),
      vmid: z.number().int().optional().describe("Only entries of this VM/container"),
      search: searchArg,
      limit: limitArg(1000),
    },
    handler: async (a) => {
      const ipam = a.ipam ?? "pve";
      let entries = await client.get<Obj[]>(`/cluster/sdn/ipams/${enc(ipam)}/status`);
      if (a.vnet) entries = entries.filter((e) => e.vnet === a.vnet);
      if (a.vmid !== undefined) entries = entries.filter((e) => Number(e.vmid) === a.vmid);
      const res = filterAndLimit(entries, { search: a.search, limit: a.limit ?? 1000 }, (e) => [e.ip, e.mac, e.hostname, e.vmid, e.vnet, e.subnet, e.zone]);
      return { ipam, ...res, items: res.items.map((e) => compact({ ...e, gateway: e.gateway ? true : undefined })) };
    },
  });

  defineTool(ctx, "proxmox_list_sdn_fabrics", {
    toolset: "sdn",
    title: "List SDN fabrics",
    description:
      "List SDN fabrics (PVE 9 underlay networks: OpenFabric, OSPF, WireGuard, BGP) with their nodes. Give `node` and `fabric` to also read that node's live fabric routes, neighbors and interfaces. Read-only; fabrics are edited in the web UI or with proxmox_api_request.",
    input: {
      pending: pendingArg,
      running: runningArg,
      node: z.string().optional().describe("With `fabric`: read this node's live routes/neighbors/interfaces for the fabric"),
      fabric: z.string().optional().describe("Fabric ID for the live node status"),
    },
    handler: async (a) => {
      const all = await client.get<Obj>("/cluster/sdn/fabrics/all", defined({ pending: a.pending, running: a.running }));
      const out: Obj = { fabrics: redactSecrets(all?.fabrics ?? []), nodes: redactSecrets(all?.nodes ?? []) };
      if (a.node && a.fabric) {
        const base = `/nodes/${enc(a.node)}/sdn/fabrics/${enc(a.fabric)}`;
        const get = (p: string) => client.get(`${base}/${p}`).catch((e: Error) => ({ error: e.message }));
        const [routes, neighbors, interfaces] = await Promise.all([get("routes"), get("neighbors"), get("interfaces")]);
        out.status = { node: a.node, fabric: a.fabric, routes, neighbors, interfaces };
      }
      return out;
    },
  });

  defineTool(ctx, "proxmox_list_sdn_route_policies", {
    toolset: "sdn",
    title: "List SDN prefix lists and route maps",
    description:
      "List SDN routing policy objects (PVE 9): prefix lists with their entries, and route maps with their entries (match/set clauses). Read-only. These are referenced by controllers (route-map-in/out) and fabrics (route_filter).",
    input: { pending: pendingArg, running: runningArg },
    handler: async (a) => {
      const params = defined({ pending: a.pending, running: a.running });
      const [prefixLists, routeMapEntries] = await Promise.all([
        client.get<Obj[]>("/cluster/sdn/prefix-lists", { ...params, verbose: true }).catch((e: Error) => ({ error: e.message })),
        client.get<Obj[]>("/cluster/sdn/route-maps/entries", params).catch((e: Error) => ({ error: e.message })),
      ]);
      let routeMaps: unknown = routeMapEntries;
      if (Array.isArray(routeMapEntries)) {
        const grouped: Record<string, Obj[]> = {};
        for (const e of routeMapEntries) {
          const { "route-map-id": id, digest: _d, ...rest } = e;
          (grouped[id] ??= []).push(compact(rest));
        }
        for (const list of Object.values(grouped)) list.sort((x, y) => Number(x.order) - Number(y.order));
        routeMaps = grouped;
      }
      return {
        prefix_lists: Array.isArray(prefixLists) ? prefixLists.map((p) => compact({ ...p, digest: undefined })) : prefixLists,
        route_maps: routeMaps,
      };
    },
  });

  // ---------------------------------------------------------------- save

  defineTool(ctx, "proxmox_save_sdn_zone", {
    toolset: "sdn",
    title: "Create or update an SDN zone",
    description: `Create (create=true) or update an SDN zone. Zone types: simple (isolated, optional SNAT/DHCP), vlan (on a VLAN-aware bridge), qinq, vxlan (overlay between peers), evpn (needs an EVPN controller). Only given fields change on update. ${SDN_NOTE}`,
    write: true,
    destructive: false,
    input: {
      zone: z.string().describe("Zone ID (2-8 alphanumeric characters, starting with a letter)"),
      create: createArg,
      type: z.enum(["simple", "vlan", "qinq", "vxlan", "evpn", "faucet"]).optional().describe("Zone type (required when creating; can't be changed)"),
      bridge: z.string().optional().describe("Bridge for VLAN/QinQ zones, e.g. vmbr0"),
      tag: z.number().int().optional().describe("Service VLAN tag (QinQ outer tag)"),
      vlan_protocol: z.enum(["802.1q", "802.1ad"]).optional().describe("QinQ VLAN protocol"),
      peers: z.array(z.string()).optional().describe("VXLAN peer IPs (usually the nodes' IPs)"),
      controller: z.string().optional().describe("EVPN controller ID"),
      vrf_vxlan: z.number().int().optional().describe("EVPN: VNI of the zone VRF"),
      exitnodes: z.array(z.string()).optional().describe("EVPN: nodes acting as exit gateways"),
      exitnodes_primary: z.string().optional().describe("EVPN: preferred exit node"),
      advertise_subnets: z.boolean().optional().describe("EVPN: advertise subnets (type-5 routes)"),
      mtu: z.number().int().optional().describe("MTU of the vnet bridges in this zone"),
      nodes: nodeListArg,
      ipam: z.string().optional().describe("IPAM to use, e.g. 'pve'"),
      dhcp: z.enum(["dnsmasq"]).optional().describe("DHCP backend for automatic DHCP in simple/EVPN zones"),
      dns: z.string().optional().describe("DNS plugin ID"),
      reversedns: z.string().optional().describe("Reverse DNS plugin ID"),
      dnszone: z.string().optional().describe("DNS domain zone, e.g. lab.example.com"),
      fabric: z.string().optional().describe("PVE 9 SDN fabric used as underlay (VXLAN)"),
      delete: deleteKeysArg,
      digest: digestArg,
      lock_token: lockTokenArg,
      extra: extraArg,
    },
    handler: async (a) => {
      if (a.create && !a.type) throw new Error("`type` is required when creating a zone");
      return saveObject(
        ctx,
        "zone",
        a.zone,
        a.create,
        {
          bridge: a.bridge,
          tag: a.tag,
          "vlan-protocol": a.vlan_protocol,
          peers: joinList(a.peers),
          controller: a.controller,
          "vrf-vxlan": a.vrf_vxlan,
          exitnodes: joinList(a.exitnodes),
          "exitnodes-primary": a.exitnodes_primary,
          "advertise-subnets": a.advertise_subnets,
          mtu: a.mtu,
          nodes: joinList(a.nodes),
          ipam: a.ipam,
          dhcp: a.dhcp,
          dns: a.dns,
          reversedns: a.reversedns,
          dnszone: a.dnszone,
          fabric: a.fabric,
        },
        { createOnly: { type: a.type }, deleteKeys: a.delete, digest: a.digest, lockToken: a.lock_token, extra: a.extra },
      );
    },
  });

  defineTool(ctx, "proxmox_save_sdn_vnet", {
    toolset: "sdn",
    title: "Create or update an SDN vnet",
    description: `Create (create=true) or update an SDN vnet: a virtual network in a zone that guests attach to like a bridge (bridge=<vnet>). Only given fields change on update. ${SDN_NOTE}`,
    write: true,
    destructive: false,
    input: {
      vnet: z.string().describe("VNet ID (up to 8 alphanumeric characters); also the bridge name guests use"),
      create: createArg,
      zone: z.string().optional().describe("Zone the vnet belongs to (required when creating)"),
      tag: z.number().int().optional().describe("VLAN tag (VLAN/QinQ zones) or VXLAN VNI (VXLAN/EVPN zones)"),
      alias: z.string().optional().describe("Descriptive alias"),
      vlanaware: z.boolean().optional().describe("Allow guests to use VLAN tags inside this vnet"),
      isolate_ports: z.boolean().optional().describe("Isolate guest ports from each other (only uplink traffic)"),
      delete: deleteKeysArg,
      digest: digestArg,
      lock_token: lockTokenArg,
      extra: extraArg,
    },
    handler: async (a) => {
      if (a.create && !a.zone) throw new Error("`zone` is required when creating a vnet");
      return saveObject(
        ctx,
        "vnet",
        a.vnet,
        a.create,
        { zone: a.zone, tag: a.tag, alias: a.alias, vlanaware: a.vlanaware, "isolate-ports": a.isolate_ports },
        { deleteKeys: a.delete, digest: a.digest, lockToken: a.lock_token, extra: a.extra },
      );
    },
  });

  defineTool(ctx, "proxmox_save_sdn_subnet", {
    toolset: "sdn",
    title: "Create or update an SDN subnet",
    description: `Create (create=true) or update a subnet of an SDN vnet: gateway, SNAT and DHCP ranges (DHCP needs dhcp=dnsmasq on the zone). Only given fields change on update. ${SDN_NOTE}`,
    write: true,
    destructive: false,
    input: {
      vnet: z.string().describe("VNet the subnet belongs to"),
      subnet: z.string().describe("Subnet in CIDR form, e.g. 10.10.0.0/24 (or its ID such as 'zone1-10.10.0.0-24' when updating)"),
      create: createArg,
      gateway: z.string().optional().describe("Gateway IP, assigned on the vnet in layer-3 zones"),
      snat: z.boolean().optional().describe("Masquerade (SNAT) traffic leaving this subnet"),
      dhcp_range: z
        .array(z.object({ start: z.string().describe("First address"), end: z.string().describe("Last address") }))
        .optional()
        .describe("DHCP ranges (replaces the existing list)"),
      dhcp_dns_server: z.string().optional().describe("DNS server handed out by DHCP"),
      dnszoneprefix: z.string().optional().describe("DNS zone prefix, e.g. 'adm' -> <host>.adm.<dnszone>"),
      delete: deleteKeysArg,
      digest: digestArg,
      lock_token: lockTokenArg,
      extra: extraArg,
    },
    handler: async (a) =>
      saveObject(
        ctx,
        "subnet",
        a.subnet,
        a.create,
        {
          gateway: a.gateway,
          snat: a.snat,
          "dhcp-range": a.dhcp_range?.map((r) => `start-address=${r.start},end-address=${r.end}`),
          "dhcp-dns-server": a.dhcp_dns_server,
          dnszoneprefix: a.dnszoneprefix,
        },
        { vnet: a.vnet, createOnly: { type: "subnet" }, deleteKeys: a.delete, digest: a.digest, lockToken: a.lock_token, extra: a.extra },
      ),
  });

  defineTool(ctx, "proxmox_save_sdn_controller", {
    toolset: "sdn",
    title: "Create or update an SDN controller",
    description: `Create (create=true) or update an SDN controller: evpn (the BGP-EVPN control plane for EVPN zones), bgp (per-node BGP peering/underlay), isis or faucet. Only given fields change on update. ${SDN_NOTE}`,
    write: true,
    destructive: false,
    input: {
      controller: z.string().describe("Controller ID"),
      create: createArg,
      type: z.enum(["evpn", "bgp", "isis", "faucet"]).optional().describe("Controller type (required when creating)"),
      asn: z.number().int().optional().describe("Autonomous system number (evpn/bgp)"),
      peers: z.array(z.string()).optional().describe("Peer IP addresses (evpn/bgp)"),
      node: z.string().optional().describe("Node this controller applies to (bgp/isis)"),
      ebgp: z.boolean().optional().describe("Use eBGP (bgp)"),
      ebgp_multihop: z.number().int().optional().describe("eBGP multihop count (bgp)"),
      loopback: z.string().optional().describe("Loopback interface providing the router IP (bgp)"),
      bgp_mode: z.enum(["auto", "external", "internal"]).optional().describe("eBGP/iBGP selection (evpn)"),
      fabric: z.string().optional().describe("PVE 9 SDN fabric used as underlay (evpn)"),
      isis_domain: z.string().optional().describe("IS-IS domain name (isis)"),
      isis_ifaces: z.array(z.string()).optional().describe("Interfaces running IS-IS (isis)"),
      isis_net: z.string().optional().describe("IS-IS network entity title (isis)"),
      route_map_in: z.string().optional().describe("Route map applied to incoming routes"),
      route_map_out: z.string().optional().describe("Route map applied to outgoing routes"),
      delete: deleteKeysArg,
      digest: digestArg,
      lock_token: lockTokenArg,
      extra: extraArg,
    },
    handler: async (a) => {
      if (a.create && !a.type) throw new Error("`type` is required when creating a controller");
      return saveObject(
        ctx,
        "controller",
        a.controller,
        a.create,
        {
          asn: a.asn,
          peers: joinList(a.peers),
          node: a.node,
          ebgp: a.ebgp,
          "ebgp-multihop": a.ebgp_multihop,
          loopback: a.loopback,
          "bgp-mode": a.bgp_mode,
          fabric: a.fabric,
          "isis-domain": a.isis_domain,
          "isis-ifaces": joinList(a.isis_ifaces),
          "isis-net": a.isis_net,
          "route-map-in": a.route_map_in,
          "route-map-out": a.route_map_out,
        },
        { createOnly: { type: a.type }, deleteKeys: a.delete, digest: a.digest, lockToken: a.lock_token, extra: a.extra },
      );
    },
  });

  defineTool(ctx, "proxmox_save_sdn_ipam", {
    toolset: "sdn",
    title: "Create or update an SDN IPAM",
    description: `Create (create=true) or update an IPAM plugin: pve (built-in), netbox or phpipam (external, need url and token). Only given fields change on update. ${SDN_NOTE}`,
    write: true,
    destructive: false,
    input: {
      ipam: z.string().describe("IPAM ID"),
      create: createArg,
      type: z.enum(["pve", "netbox", "phpipam"]).optional().describe("IPAM type (required when creating)"),
      url: z.string().optional().describe("API URL of the external IPAM"),
      token: z.string().optional().describe("API token of the external IPAM"),
      section: z.number().int().optional().describe("phpIPAM section ID"),
      fingerprint: z.string().optional().describe("Expected TLS certificate SHA-256 fingerprint"),
      delete: deleteKeysArg,
      digest: digestArg,
      lock_token: lockTokenArg,
      extra: extraArg,
    },
    handler: async (a) => {
      if (a.create && !a.type) throw new Error("`type` is required when creating an IPAM");
      return saveObject(
        ctx,
        "ipam",
        a.ipam,
        a.create,
        { url: a.url, token: a.token, section: a.section, fingerprint: a.fingerprint },
        { createOnly: { type: a.type }, deleteKeys: a.delete, digest: a.digest, lockToken: a.lock_token, extra: a.extra },
      );
    },
  });

  defineTool(ctx, "proxmox_save_sdn_dns", {
    toolset: "sdn",
    title: "Create or update an SDN DNS plugin",
    description: `Create (create=true) or update an SDN DNS plugin (PowerDNS) used to register guest hostnames for zones with dns/dnszone set. Only given fields change on update. ${SDN_NOTE}`,
    write: true,
    destructive: false,
    input: {
      dns: z.string().describe("DNS plugin ID"),
      create: createArg,
      type: z.enum(["powerdns"]).optional().describe("Plugin type (required when creating)"),
      url: z.string().optional().describe("PowerDNS API URL (required when creating), e.g. http://pdns:8081/api/v1/servers/localhost"),
      key: z.string().optional().describe("PowerDNS API key (required when creating)"),
      ttl: z.number().int().optional().describe("TTL of created records"),
      reversemaskv6: z.number().int().optional().describe("IPv6 reverse zone mask"),
      fingerprint: z.string().optional().describe("Expected TLS certificate SHA-256 fingerprint"),
      delete: deleteKeysArg,
      digest: digestArg,
      lock_token: lockTokenArg,
      extra: extraArg,
    },
    handler: async (a) => {
      if (a.create && (!a.type || !a.url || !a.key)) throw new Error("`type`, `url` and `key` are required when creating a DNS plugin");
      return saveObject(
        ctx,
        "dns",
        a.dns,
        a.create,
        { url: a.url, key: a.key, ttl: a.ttl, reversemaskv6: a.reversemaskv6, fingerprint: a.fingerprint },
        { createOnly: { type: a.type }, deleteKeys: a.delete, digest: a.digest, lockToken: a.lock_token, extra: a.extra },
      );
    },
  });

  defineTool(ctx, "proxmox_delete_sdn_object", {
    toolset: "sdn",
    title: "Delete an SDN object",
    description: `Delete an SDN zone, vnet, subnet, controller, IPAM or DNS plugin. Objects still in use (e.g. a zone with vnets, a vnet with subnets) are refused by Proxmox. ${SDN_NOTE}`,
    delete: true,
    input: {
      kind: kindArg,
      id: z.string().describe("Object ID (for subnets the CIDR or subnet ID)"),
      vnet: vnetForSubnetArg,
      lock_token: lockTokenArg,
    },
    handler: async (a) => {
      await client.delete(await resolvedObjectPath(client, a.kind, a.id, a.vnet), defined({ "lock-token": a.lock_token }));
      return { deleted: a.id, kind: a.kind, note: SDN_NOTE };
    },
  });

  defineTool(ctx, "proxmox_save_sdn_vnet_ip", {
    toolset: "sdn",
    title: "Create or update a vnet IP mapping",
    description:
      "Create (create=true) or update an IP-to-MAC mapping in a vnet's IPAM, e.g. to reserve a fixed DHCP address for a guest. Takes effect immediately (no apply needed). See proxmox_get_sdn_ipam_status for current entries.",
    write: true,
    destructive: false,
    input: {
      vnet: z.string().describe("VNet ID"),
      zone: z.string().optional().describe("Zone of the vnet (looked up from the vnet when omitted)"),
      ip: z.string().describe("IP address"),
      mac: z.string().optional().describe("MAC address to associate"),
      vmid: z.number().int().optional().describe("VM/container ID owning the IP (update only)"),
      create: createArg,
    },
    handler: async (a) => {
      const zone = a.zone ?? (await client.get<Obj>(`/cluster/sdn/vnets/${enc(a.vnet)}`)).zone;
      if (!zone) throw new Error(`Could not determine the zone of vnet ${a.vnet}; pass \`zone\``);
      const params = defined({ zone, ip: a.ip, mac: a.mac, vmid: a.create ? undefined : a.vmid });
      if (a.create) await client.post(`/cluster/sdn/vnets/${enc(a.vnet)}/ips`, params);
      else await client.put(`/cluster/sdn/vnets/${enc(a.vnet)}/ips`, params);
      return { saved: a.ip, vnet: a.vnet, zone, action: a.create ? "created" : "updated" };
    },
  });

  defineTool(ctx, "proxmox_delete_sdn_vnet_ip", {
    toolset: "sdn",
    title: "Delete a vnet IP mapping",
    description: "Remove an IP mapping (IPAM entry) from a vnet, freeing the address.",
    delete: true,
    input: {
      vnet: z.string().describe("VNet ID"),
      zone: z.string().optional().describe("Zone of the vnet (looked up from the vnet when omitted)"),
      ip: z.string().describe("IP address to remove"),
      mac: z.string().optional().describe("MAC address of the mapping"),
    },
    handler: async (a) => {
      const zone = a.zone ?? (await client.get<Obj>(`/cluster/sdn/vnets/${enc(a.vnet)}`)).zone;
      if (!zone) throw new Error(`Could not determine the zone of vnet ${a.vnet}; pass \`zone\``);
      await client.delete(`/cluster/sdn/vnets/${enc(a.vnet)}/ips`, defined({ zone, ip: a.ip, mac: a.mac }));
      return { deleted: a.ip, vnet: a.vnet, zone };
    },
  });

  // ---------------------------------------------------------------- apply / rollback

  defineTool(ctx, "proxmox_apply_sdn", {
    toolset: "sdn",
    title: "Apply SDN configuration",
    description:
      "Apply all pending SDN changes: writes the network and FRR configuration to every node and reloads networking. This can briefly disrupt guest networking on SDN vnets. Review with proxmox_list_sdn_objects pending=true or proxmox_get_sdn_pending_diff first, and check proxmox_get_sdn_status afterwards.",
    write: true,
    destructive: true,
    input: {
      lock_token: lockTokenArg,
      release_lock: z.boolean().optional().describe("With lock_token: release the lock after applying (default true)"),
      wait: waitArg,
    },
    handler: async (a) => {
      const upid = await client.put("/cluster/sdn", defined({ "lock-token": a.lock_token, "release-lock": a.release_lock }));
      return runTask(client, upid, a.wait, { action: "apply-sdn" });
    },
  });

  defineTool(ctx, "proxmox_rollback_sdn", {
    toolset: "sdn",
    title: "Discard pending SDN changes",
    description:
      "Roll back all pending (not yet applied) SDN changes to the running configuration (PVE 9). Irreversible: staged edits are lost. Running networking is not touched.",
    write: true,
    destructive: true,
    input: {
      lock_token: lockTokenArg,
      release_lock: z.boolean().optional().describe("With lock_token: release the lock afterwards (default true)"),
    },
    handler: async (a) => {
      await client.post("/cluster/sdn/rollback", defined({ "lock-token": a.lock_token, "release-lock": a.release_lock }));
      return { rolled_back: true };
    },
  });
}
