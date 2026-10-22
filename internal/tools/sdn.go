package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

var sdnKinds = []string{"zone", "vnet", "subnet", "controller", "ipam", "dns"}

// sdnKindInfo is the collection path and identifier parameter name of each SDN object kind.
var sdnKindInfo = map[string]struct{ collection, key string }{
	"zone":       {"zones", "zone"},
	"vnet":       {"vnets", "vnet"},
	"subnet":     {"subnets", "subnet"},
	"controller": {"controllers", "controller"},
	"ipam":       {"ipams", "ipam"},
	"dns":        {"dns", "dns"},
}

// sdnPendingKinds are the kinds whose list/get endpoints understand pending/running.
var sdnPendingKinds = map[string]bool{"zone": true, "vnet": true, "subnet": true, "controller": true}

var errSdnVnetRequired = errors.New("`vnet` is required for subnets (subnets belong to a vnet)")

func sdnCollectionPath(kind, vnet string) (string, error) {
	if kind == "subnet" {
		if vnet == "" {
			return "", errSdnVnetRequired
		}
		return "/cluster/sdn/vnets/" + enc(vnet) + "/subnets", nil
	}
	return "/cluster/sdn/" + sdnKindInfo[kind].collection, nil
}

func sdnObjectPath(kind, id, vnet string) (string, error) {
	p, err := sdnCollectionPath(kind, vnet)
	if err != nil {
		return "", err
	}
	return p + "/" + enc(id), nil
}

// sdnSubnetID translates a subnet CIDR into its ID. Subnets are stored under
// "<zone>-<ip>-<prefix>" (e.g. zone1-10.0.0.0-24) and the per-subnet endpoints look them up by
// that ID; only creation takes the plain CIDR.
func sdnSubnetID(ctx context.Context, c *Context, vnet, id string) (string, error) {
	if !strings.Contains(id, "/") {
		return id, nil
	}
	if vnet == "" {
		return "", errSdnVnetRequired
	}
	vAny, err := c.Client.Get(ctx, "/cluster/sdn/vnets/"+enc(vnet), js.O("pending", true))
	if err != nil {
		return "", err
	}
	v, _ := vAny.(*Obj)
	zone := or(v.Get("zone"), v.ObjAt("pending").Get("zone"))
	if !js.Truthy(zone) {
		return "", fmt.Errorf("Could not determine the zone of vnet %s; give the subnet ID (<zone>-<ip>-<prefix>) instead of the CIDR", vnet)
	}
	return js.Str(zone) + "-" + strings.Replace(id, "/", "-", 1), nil
}

// sdnResolvedObjectPath is sdnObjectPath, with subnet CIDRs translated to subnet IDs.
func sdnResolvedObjectPath(ctx context.Context, c *Context, kind, id, vnet string) (string, error) {
	if kind == "subnet" {
		var err error
		if id, err = sdnSubnetID(ctx, c, vnet, id); err != nil {
			return "", err
		}
	}
	return sdnObjectPath(kind, id, vnet)
}

var (
	sdnKindArg          = Enum(sdnKinds...).D("SDN object kind: zone, vnet, subnet (needs `vnet`), controller (BGP/EVPN/IS-IS), ipam or dns (DNS plugin)")
	sdnVnetForSubnetArg = Str().Opt().D("VNet the subnet belongs to (required for kind=subnet)")
	sdnPendingArg       = Bool().Opt().D("Include uncommitted changes: each object shows `state` (new/changed/deleted) and the `pending` values not yet applied with proxmox_apply_sdn")
	sdnRunningArg       = Bool().Opt().D("Show the running (last applied) configuration instead of the edited one")
	sdnLockTokenArg     = Str().Opt().D("SDN lock token, only needed when the global SDN configuration has been locked by someone else (PVE 9); normally omit")
	sdnCreateArg        = Bool().Opt().D("Create a new object (default false = update an existing one; only the given fields change)")
	sdnNodeListArg      = Arr(Str()).Opt().D("Restrict to these cluster nodes (default all nodes)")
)

const sdnNote = "Changes are staged as pending; run proxmox_apply_sdn to activate them on the nodes."

// sdnRedact is RedactSecrets plus the PowerDNS plugin's API key, which is stored under the generic name `key`.
func sdnRedact(value any) any {
	out := proxmox.RedactSecrets(value)
	switch x := out.(type) {
	case []any:
		res := make([]any, len(x))
		for i, o := range x {
			res[i] = sdnRedact(o)
		}
		return res
	case *Obj:
		if k, ok := x.Get("key").(string); ok && k != "" {
			x.Set("key", "<redacted>")
		}
	}
	return out
}

// sdnSummarizeObject drops the digest and redacts plugin credentials (IPAM tokens, DNS keys).
func sdnSummarizeObject(o *Obj) any {
	rest := o.Clone()
	rest.Delete("digest")
	return sdnRedact(proxmox.Compact(rest))
}

// sdnJoinList joins a list with commas (undefined when empty).
func sdnJoinList(v any) any { return deleteParam(v) }

type sdnSaveOpts struct {
	vnet       string
	createOnly *Obj
	deleteKeys any
	digest     any
	lockToken  any
	extra      any
}

// sdnSaveObject is the shared create-or-update flow for the save tools.
func sdnSaveObject(ctx context.Context, c *Context, kind, id string, create bool, params *Obj, opts sdnSaveOpts) (any, error) {
	client := c.Client
	common := js.Spread(params, js.O("lock-token", opts.lockToken))
	if create {
		coll, err := sdnCollectionPath(kind, opts.vnet)
		if err != nil {
			return nil, err
		}
		body, err := definedExtra(js.Spread(js.O(sdnKindInfo[kind].key, id), opts.createOnly, common), opts.extra)
		if err != nil {
			return nil, err
		}
		if _, err := client.Post(ctx, coll, body); err != nil {
			return nil, err
		}
	}
	// After a successful create, failing to resolve the read-back path must not turn the result into an error.
	path, err := sdnResolvedObjectPath(ctx, c, kind, id, opts.vnet)
	if err != nil {
		if !create {
			return nil, err
		}
		path = ""
	}
	if !create {
		body, err := definedExtra(js.Spread(common, js.O("delete", deleteParam(opts.deleteKeys), "digest", opts.digest)), opts.extra)
		if err != nil {
			return nil, err
		}
		if _, err := client.Put(ctx, path, body); err != nil {
			return nil, err
		}
	}
	var object any = js.Undef
	if path != "" {
		var q *Obj
		if sdnPendingKinds[kind] {
			q = js.O("pending", true)
		}
		if rb, err := client.Get(ctx, path, q); err == nil {
			if o, ok := rb.(*Obj); ok && o != nil {
				object = sdnSummarizeObject(o)
			}
		}
	}
	return proxmox.C("saved", id, "kind", kind, "action", pick(create, "created", "updated"), "object", object, "note", sdnNote), nil
}

func registerSdn(c *Context) {
	client := c.Client

	// ---------------------------------------------------------------- read

	c.Define("proxmox_list_sdn_objects", Def{
		Toolset:     "sdn",
		List:        &listing{},
		Title:       "List SDN objects",
		Description: "List Software-Defined Networking objects of one kind: zones, vnets, subnets of a vnet, controllers, IPAMs or DNS plugins. Use pending=true to see uncommitted changes (state new/changed/deleted). Related: proxmox_get_sdn_object for one object, proxmox_get_sdn_status for the live state on nodes.",
		Input: []Prop{
			P("kind", sdnKindArg),
			P("vnet", sdnVnetForSubnetArg),
			P("type", Str().Opt().D("Only objects of this plugin type, e.g. vlan, vxlan, evpn, simple, qinq (zones); bgp, evpn, isis (controllers); pve, netbox, phpipam (ipams)")),
			P("pending", sdnPendingArg),
			P("running", sdnRunningArg),
			P("search", searchArg),
			P("limit", limitArg(500)),
			P("raw", rawArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			kind := a.Str("kind")
			supportsPending := sdnPendingKinds[kind]
			typeFilterServer := contains([]string{"zone", "controller", "ipam", "dns"}, kind)
			path, err := sdnCollectionPath(kind, a.Str("vnet"))
			if err != nil {
				return nil, err
			}
			list, err := client.GetList(ctx, path, defined(js.O(
				"pending", ifThen(supportsPending, a.Get("pending")),
				"running", ifThen(supportsPending, a.Get("running")),
				"type", ifThen(typeFilterServer, a.Get("type")),
			)))
			if err != nil {
				return nil, err
			}
			key := sdnKindInfo[kind].key
			if a.Truthy("type") && !typeFilterServer {
				list = filterObjs(list, func(o *Obj) bool { return o.Get("type") == a.Get("type") })
			}
			sortObjs(list, func(x, y *Obj) float64 {
				return compareStr(or(x.Get(key), or(x.Get("id"), "")), or(y.Get(key), or(y.Get("id"), "")))
			})
			res := filterAndLimit(list, a.Get("search"), or(a.Get("limit"), 500), func(o *Obj) []any {
				return []any{o.Get(key), o.Get("id"), o.Get("type"), o.Get("zone"), o.Get("alias"), o.Get("cidr"), o.Get("state")}
			})
			var items any
			if a.Truthy("raw") {
				items = sdnRedact(js.Array(res.Items))
			} else {
				items = mapObjs(res.Items, sdnSummarizeObject)
			}
			return js.Spread(js.O("kind", kind), res.Obj(items)), nil
		},
	})

	c.Define("proxmox_get_sdn_object", Def{
		Toolset:     "sdn",
		Title:       "Get an SDN object",
		Description: "Get the full configuration of one SDN zone, vnet, subnet, controller, IPAM or DNS plugin, including its digest for optimistic locking. With pending=true, uncommitted changes are shown under `pending`.",
		Input: []Prop{
			P("kind", sdnKindArg),
			P("id", Str().D("Object ID (zone/vnet/controller/ipam/dns name, or subnet ID such as 'zone1-10.0.0.0-24' or its CIDR '10.0.0.0/24')")),
			P("vnet", sdnVnetForSubnetArg),
			P("pending", sdnPendingArg),
			P("running", sdnRunningArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			kind := a.Str("kind")
			supportsPending := sdnPendingKinds[kind]
			path, err := sdnResolvedObjectPath(ctx, c, kind, a.Str("id"), a.Str("vnet"))
			if err != nil {
				return nil, err
			}
			obj, err := client.Get(ctx, path, defined(js.O("pending", ifThen(supportsPending, a.Get("pending")), "running", ifThen(supportsPending, a.Get("running")))))
			if err != nil {
				return nil, err
			}
			return sdnRedact(obj), nil
		},
	})

	c.Define("proxmox_get_sdn_status", Def{
		Toolset:     "sdn",
		Title:       "Get SDN status on nodes",
		Description: "Show the live SDN state on nodes: each zone's status (available/pending/error) per node and the status of the vnets in it. Use it after proxmox_apply_sdn to confirm the configuration came up, or to find nodes where a zone is in error.",
		Input: []Prop{
			P("node", Str().Opt().D("Only this node (default all online nodes)")),
			P("zone", Str().Opt().D("Only this zone")),
			P("vnets", Bool().Opt().D("Include per-vnet status inside each zone (default true)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			var nodes []string
			if a.Truthy("node") {
				nodes = []string{a.Str("node")}
			} else {
				list, err := client.ListNodes(ctx)
				if err != nil {
					return nil, err
				}
				for _, n := range list {
					if n.Get("status") == "online" {
						nodes = append(nodes, js.Str(n.Get("node")))
					}
				}
				sort.Strings(nodes)
			}
			out := make([]any, 0, len(nodes))
			for _, node := range nodes {
				zones, err := client.GetList(ctx, "/nodes/"+enc(node)+"/sdn/zones", nil)
				if err != nil {
					out = append(out, js.O("node", node, "error", errMessage(err)))
					continue
				}
				if a.Truthy("zone") {
					zones = filterObjs(zones, func(z *Obj) bool { return z.Get("zone") == a.Get("zone") })
				}
				sortObjs(zones, func(x, y *Obj) float64 { return compareStr(x.Get("zone"), y.Get("zone")) })
				detailed := make([]*Obj, 0, len(zones))
				for _, z := range zones {
					if a.Get("vnets") == false {
						detailed = append(detailed, proxmox.C("zone", z.Get("zone"), "status", z.Get("status")))
						continue
					}
					var vnets any = js.Undef
					if content, err := client.Get(ctx, "/nodes/"+enc(node)+"/sdn/zones/"+enc(z.Get("zone"))+"/content", nil); err == nil {
						if arr, ok := content.([]any); ok {
							vnets = mapObjs(js.Objs(arr), func(v *Obj) any {
								return proxmox.C("vnet", v.Get("vnet"), "status", v.Get("status"), "statusmsg", v.Get("statusmsg"))
							})
						}
					}
					detailed = append(detailed, proxmox.C("zone", z.Get("zone"), "status", z.Get("status"), "vnets", vnets))
				}
				problems := []any{}
				for _, z := range detailed {
					if z.Get("status") != "available" {
						problems = append(problems, z.Get("zone"))
					}
				}
				out = append(out, proxmox.C("node", node, "zones", js.Array(detailed), "not_available", problems))
			}
			return js.O("nodes", out), nil
		},
	})

	c.Define("proxmox_get_sdn_pending_diff", Def{
		Toolset:     "sdn",
		Title:       "Preview SDN apply (dry run)",
		Description: "Dry-run proxmox_apply_sdn for one node: shows the diff between the running and the pending network interfaces (/etc/network/interfaces.d/sdn) and FRR routing configuration. Use it to review staged SDN changes before applying them.",
		Input:       []Prop{P("node", Str().Opt().D("Node to compute the diff for (default PROXMOX_DEFAULT_NODE or the only node)"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			dAny, err := client.Get(ctx, "/cluster/sdn/dry-run", js.O("node", node))
			if err != nil {
				return nil, err
			}
			diff, _ := dAny.(*Obj)
			return proxmox.C(
				"node", node,
				"interfaces_diff", orUndef(diff.Get("interfaces-diff")),
				"frr_diff", orUndef(diff.Get("frr-diff")),
				"note", ifThen(!diff.Truthy("interfaces-diff") && !diff.Truthy("frr-diff"), "No differences: nothing pending for this node"),
			), nil
		},
	})

	c.Define("proxmox_get_sdn_ipam_status", Def{
		Toolset:     "sdn",
		Title:       "List IPAM entries",
		Description: "List the IP allocations recorded in an SDN IPAM (default the built-in 'pve' IPAM): IP, MAC, hostname, VMID, vnet/subnet and gateway entries. Use it to find which guest holds an address or which IPs are free. Related: proxmox_save_sdn_vnet_ip to add or change a mapping.",
		Input: []Prop{
			P("ipam", Str().Opt().D("IPAM ID (default 'pve')")),
			P("vnet", Str().Opt().D("Only entries in this vnet")),
			P("vmid", Int().Opt().D("Only entries of this VM/container")),
			P("search", searchArg),
			P("limit", limitArg(1000)),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			ipam := or(a.Get("ipam"), "pve")
			entries, err := client.GetList(ctx, "/cluster/sdn/ipams/"+enc(ipam)+"/status", nil)
			if err != nil {
				return nil, err
			}
			if a.Truthy("vnet") {
				entries = filterObjs(entries, func(e *Obj) bool { return e.Get("vnet") == a.Get("vnet") })
			}
			if a.Defined("vmid") {
				entries = filterObjs(entries, func(e *Obj) bool { return e.Num("vmid") == a.Num("vmid") })
			}
			res := filterAndLimit(entries, a.Get("search"), or(a.Get("limit"), 1000), func(e *Obj) []any {
				return []any{e.Get("ip"), e.Get("mac"), e.Get("hostname"), e.Get("vmid"), e.Get("vnet"), e.Get("subnet"), e.Get("zone")}
			})
			items := mapObjs(res.Items, func(e *Obj) any {
				return proxmox.Compact(js.Spread(e, js.O("gateway", ifThen(e.Truthy("gateway"), true))))
			})
			return js.Spread(js.O("ipam", ipam), res.Obj(items)), nil
		},
	})

	c.Define("proxmox_list_sdn_fabrics", Def{
		Toolset:     "sdn",
		Title:       "List SDN fabrics",
		Description: "List SDN fabrics (PVE 9 underlay networks: OpenFabric, OSPF, WireGuard, BGP) with their nodes. Give `node` and `fabric` to also read that node's live fabric routes, neighbors and interfaces. Read-only; fabrics are edited in the web UI or with proxmox_api_request.",
		Input: []Prop{
			P("pending", sdnPendingArg),
			P("running", sdnRunningArg),
			P("node", Str().Opt().D("With `fabric`: read this node's live routes/neighbors/interfaces for the fabric")),
			P("fabric", Str().Opt().D("Fabric ID for the live node status")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			allAny, err := client.Get(ctx, "/cluster/sdn/fabrics/all", defined(js.O("pending", a.Get("pending"), "running", a.Get("running"))))
			if err != nil {
				return nil, err
			}
			all, _ := allAny.(*Obj)
			out := js.O("fabrics", proxmox.RedactSecrets(or(all.Get("fabrics"), []any{})), "nodes", proxmox.RedactSecrets(or(all.Get("nodes"), []any{})))
			if a.Truthy("node") && a.Truthy("fabric") {
				base := "/nodes/" + enc(a.Get("node")) + "/sdn/fabrics/" + enc(a.Get("fabric"))
				get := func(p string) any {
					v, err := client.Get(ctx, base+"/"+p, nil)
					if err != nil {
						return js.O("error", errMessage(err))
					}
					return v
				}
				routes, neighbors, interfaces := get("routes"), get("neighbors"), get("interfaces")
				out.Set("status", js.O("node", a.Get("node"), "fabric", a.Get("fabric"), "routes", routes, "neighbors", neighbors, "interfaces", interfaces))
			}
			return out, nil
		},
	})

	c.Define("proxmox_list_sdn_route_policies", Def{
		Toolset:     "sdn",
		Title:       "List SDN prefix lists and route maps",
		Description: "List SDN routing policy objects (PVE 9): prefix lists with their entries, and route maps with their entries (match/set clauses). Read-only. These are referenced by controllers (route-map-in/out) and fabrics (route_filter).",
		Input:       []Prop{P("pending", sdnPendingArg), P("running", sdnRunningArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			params := defined(js.O("pending", a.Get("pending"), "running", a.Get("running")))
			orError := func(path string, q *Obj) any {
				v, err := client.Get(ctx, path, q)
				if err != nil {
					return js.O("error", errMessage(err))
				}
				return v
			}
			prefixLists := orError("/cluster/sdn/prefix-lists", js.Spread(params, js.O("verbose", true)))
			routeMapEntries := orError("/cluster/sdn/route-maps/entries", params)
			routeMaps := routeMapEntries
			if arr, ok := routeMapEntries.([]any); ok {
				grouped := js.NewObj()
				for _, e := range js.Objs(arr) {
					id := js.Str(e.Get("route-map-id"))
					rest := e.Clone()
					rest.Delete("route-map-id")
					rest.Delete("digest")
					list, _ := grouped.Get(id).([]any)
					grouped.Set(id, append(list, proxmox.Compact(rest)))
				}
				for _, k := range grouped.Keys() {
					list := js.Objs(grouped.Get(k))
					sortObjs(list, func(x, y *Obj) float64 { return x.Num("order") - y.Num("order") })
					grouped.Set(k, js.Array(list))
				}
				routeMaps = grouped
			}
			var pl any = prefixLists
			if arr, ok := prefixLists.([]any); ok {
				pl = mapObjs(js.Objs(arr), func(p *Obj) any { return proxmox.Compact(js.Spread(p, js.O("digest", js.Undef))) })
			}
			return js.O("prefix_lists", pl, "route_maps", routeMaps), nil
		},
	})

	// ---------------------------------------------------------------- save

	c.Define("proxmox_save_sdn_zone", Def{
		Toolset:     "sdn",
		Title:       "Create or update an SDN zone",
		Description: "Create (create=true) or update an SDN zone. Zone types: simple (isolated, optional SNAT/DHCP), vlan (on a VLAN-aware bridge), qinq, vxlan (overlay between peers), evpn (needs an EVPN controller). Only given fields change on update. " + sdnNote,
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("zone", Str().D("Zone ID (2-8 alphanumeric characters, starting with a letter)")),
			P("create", sdnCreateArg),
			P("type", Enum("simple", "vlan", "qinq", "vxlan", "evpn", "faucet").Opt().D("Zone type (required when creating; can't be changed)")),
			P("bridge", Str().Opt().D("Bridge for VLAN/QinQ zones, e.g. vmbr0")),
			P("tag", Int().Opt().D("Service VLAN tag (QinQ outer tag)")),
			P("vlan_protocol", Enum("802.1q", "802.1ad").Opt().D("QinQ VLAN protocol")),
			P("peers", Arr(Str()).Opt().D("VXLAN peer IPs (usually the nodes' IPs)")),
			P("controller", Str().Opt().D("EVPN controller ID")),
			P("vrf_vxlan", Int().Opt().D("EVPN: VNI of the zone VRF")),
			P("exitnodes", Arr(Str()).Opt().D("EVPN: nodes acting as exit gateways")),
			P("exitnodes_primary", Str().Opt().D("EVPN: preferred exit node")),
			P("advertise_subnets", Bool().Opt().D("EVPN: advertise subnets (type-5 routes)")),
			P("mtu", Int().Opt().D("MTU of the vnet bridges in this zone")),
			P("nodes", sdnNodeListArg),
			P("ipam", Str().Opt().D("IPAM to use, e.g. 'pve'")),
			P("dhcp", Enum("dnsmasq").Opt().D("DHCP backend for automatic DHCP in simple/EVPN zones")),
			P("dns", Str().Opt().D("DNS plugin ID")),
			P("reversedns", Str().Opt().D("Reverse DNS plugin ID")),
			P("dnszone", Str().Opt().D("DNS domain zone, e.g. lab.example.com")),
			P("fabric", Str().Opt().D("PVE 9 SDN fabric used as underlay (VXLAN)")),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("lock_token", sdnLockTokenArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if a.Truthy("create") && !a.Truthy("type") {
				return nil, errors.New("`type` is required when creating a zone")
			}
			return sdnSaveObject(ctx, c, "zone", a.Str("zone"), a.Truthy("create"), js.O(
				"bridge", a.Get("bridge"),
				"tag", a.Get("tag"),
				"vlan-protocol", a.Get("vlan_protocol"),
				"peers", sdnJoinList(a.Get("peers")),
				"controller", a.Get("controller"),
				"vrf-vxlan", a.Get("vrf_vxlan"),
				"exitnodes", sdnJoinList(a.Get("exitnodes")),
				"exitnodes-primary", a.Get("exitnodes_primary"),
				"advertise-subnets", a.Get("advertise_subnets"),
				"mtu", a.Get("mtu"),
				"nodes", sdnJoinList(a.Get("nodes")),
				"ipam", a.Get("ipam"),
				"dhcp", a.Get("dhcp"),
				"dns", a.Get("dns"),
				"reversedns", a.Get("reversedns"),
				"dnszone", a.Get("dnszone"),
				"fabric", a.Get("fabric"),
			), sdnSaveOpts{createOnly: js.O("type", a.Get("type")), deleteKeys: a.Get("delete"), digest: a.Get("digest"), lockToken: a.Get("lock_token"), extra: a.Get("extra")})
		},
	})

	c.Define("proxmox_save_sdn_vnet", Def{
		Toolset:     "sdn",
		Title:       "Create or update an SDN vnet",
		Description: "Create (create=true) or update an SDN vnet: a virtual network in a zone that guests attach to like a bridge (bridge=<vnet>). Only given fields change on update. " + sdnNote,
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("vnet", Str().D("VNet ID (up to 8 alphanumeric characters); also the bridge name guests use")),
			P("create", sdnCreateArg),
			P("zone", Str().Opt().D("Zone the vnet belongs to (required when creating)")),
			P("tag", Int().Opt().D("VLAN tag (VLAN/QinQ zones) or VXLAN VNI (VXLAN/EVPN zones)")),
			P("alias", Str().Opt().D("Descriptive alias")),
			P("vlanaware", Bool().Opt().D("Allow guests to use VLAN tags inside this vnet")),
			P("isolate_ports", Bool().Opt().D("Isolate guest ports from each other (only uplink traffic)")),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("lock_token", sdnLockTokenArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if a.Truthy("create") && !a.Truthy("zone") {
				return nil, errors.New("`zone` is required when creating a vnet")
			}
			return sdnSaveObject(ctx, c, "vnet", a.Str("vnet"), a.Truthy("create"),
				js.O("zone", a.Get("zone"), "tag", a.Get("tag"), "alias", a.Get("alias"), "vlanaware", a.Get("vlanaware"), "isolate-ports", a.Get("isolate_ports")),
				sdnSaveOpts{deleteKeys: a.Get("delete"), digest: a.Get("digest"), lockToken: a.Get("lock_token"), extra: a.Get("extra")})
		},
	})

	c.Define("proxmox_save_sdn_subnet", Def{
		Toolset:     "sdn",
		Title:       "Create or update an SDN subnet",
		Description: "Create (create=true) or update a subnet of an SDN vnet: gateway, SNAT and DHCP ranges (DHCP needs dhcp=dnsmasq on the zone). Only given fields change on update. " + sdnNote,
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("vnet", Str().D("VNet the subnet belongs to")),
			P("subnet", Str().D("Subnet in CIDR form, e.g. 10.10.0.0/24 (or its ID such as 'zone1-10.10.0.0-24' when updating)")),
			P("create", sdnCreateArg),
			P("gateway", Str().Opt().D("Gateway IP, assigned on the vnet in layer-3 zones")),
			P("snat", Bool().Opt().D("Masquerade (SNAT) traffic leaving this subnet")),
			P("dhcp_range", Arr(Object(P("start", Str().D("First address")), P("end", Str().D("Last address")))).Opt().D("DHCP ranges (replaces the existing list)")),
			P("dhcp_dns_server", Str().Opt().D("DNS server handed out by DHCP")),
			P("dnszoneprefix", Str().Opt().D("DNS zone prefix, e.g. 'adm' -> <host>.adm.<dnszone>")),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("lock_token", sdnLockTokenArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			var ranges any = js.Undef
			if a.Defined("dhcp_range") {
				ranges = mapObjs(a.Objs("dhcp_range"), func(r *Obj) any {
					return "start-address=" + js.Str(r.Get("start")) + ",end-address=" + js.Str(r.Get("end"))
				})
			}
			return sdnSaveObject(ctx, c, "subnet", a.Str("subnet"), a.Truthy("create"), js.O(
				"gateway", a.Get("gateway"),
				"snat", a.Get("snat"),
				"dhcp-range", ranges,
				"dhcp-dns-server", a.Get("dhcp_dns_server"),
				"dnszoneprefix", a.Get("dnszoneprefix"),
			), sdnSaveOpts{vnet: a.Str("vnet"), createOnly: js.O("type", "subnet"), deleteKeys: a.Get("delete"), digest: a.Get("digest"), lockToken: a.Get("lock_token"), extra: a.Get("extra")})
		},
	})

	c.Define("proxmox_save_sdn_controller", Def{
		Toolset:     "sdn",
		Title:       "Create or update an SDN controller",
		Description: "Create (create=true) or update an SDN controller: evpn (the BGP-EVPN control plane for EVPN zones), bgp (per-node BGP peering/underlay), isis or faucet. Only given fields change on update. " + sdnNote,
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("controller", Str().D("Controller ID")),
			P("create", sdnCreateArg),
			P("type", Enum("evpn", "bgp", "isis", "faucet").Opt().D("Controller type (required when creating)")),
			P("asn", Int().Opt().D("Autonomous system number (evpn/bgp)")),
			P("peers", Arr(Str()).Opt().D("Peer IP addresses (evpn/bgp)")),
			P("node", Str().Opt().D("Node this controller applies to (bgp/isis)")),
			P("ebgp", Bool().Opt().D("Use eBGP (bgp)")),
			P("ebgp_multihop", Int().Opt().D("eBGP multihop count (bgp)")),
			P("loopback", Str().Opt().D("Loopback interface providing the router IP (bgp)")),
			P("bgp_mode", Enum("auto", "external", "internal").Opt().D("eBGP/iBGP selection (evpn)")),
			P("fabric", Str().Opt().D("PVE 9 SDN fabric used as underlay (evpn)")),
			P("isis_domain", Str().Opt().D("IS-IS domain name (isis)")),
			P("isis_ifaces", Arr(Str()).Opt().D("Interfaces running IS-IS (isis)")),
			P("isis_net", Str().Opt().D("IS-IS network entity title (isis)")),
			P("route_map_in", Str().Opt().D("Route map applied to incoming routes")),
			P("route_map_out", Str().Opt().D("Route map applied to outgoing routes")),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("lock_token", sdnLockTokenArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if a.Truthy("create") && !a.Truthy("type") {
				return nil, errors.New("`type` is required when creating a controller")
			}
			return sdnSaveObject(ctx, c, "controller", a.Str("controller"), a.Truthy("create"), js.O(
				"asn", a.Get("asn"),
				"peers", sdnJoinList(a.Get("peers")),
				"node", a.Get("node"),
				"ebgp", a.Get("ebgp"),
				"ebgp-multihop", a.Get("ebgp_multihop"),
				"loopback", a.Get("loopback"),
				"bgp-mode", a.Get("bgp_mode"),
				"fabric", a.Get("fabric"),
				"isis-domain", a.Get("isis_domain"),
				"isis-ifaces", sdnJoinList(a.Get("isis_ifaces")),
				"isis-net", a.Get("isis_net"),
				"route-map-in", a.Get("route_map_in"),
				"route-map-out", a.Get("route_map_out"),
			), sdnSaveOpts{createOnly: js.O("type", a.Get("type")), deleteKeys: a.Get("delete"), digest: a.Get("digest"), lockToken: a.Get("lock_token"), extra: a.Get("extra")})
		},
	})

	c.Define("proxmox_save_sdn_ipam", Def{
		Toolset:     "sdn",
		Title:       "Create or update an SDN IPAM",
		Description: "Create (create=true) or update an IPAM plugin: pve (built-in), netbox or phpipam (external, need url and token). Only given fields change on update. " + sdnNote,
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("ipam", Str().D("IPAM ID")),
			P("create", sdnCreateArg),
			P("type", Enum("pve", "netbox", "phpipam").Opt().D("IPAM type (required when creating)")),
			P("url", Str().Opt().D("API URL of the external IPAM")),
			P("token", Str().Opt().D("API token of the external IPAM")),
			P("section", Int().Opt().D("phpIPAM section ID")),
			P("fingerprint", Str().Opt().D("Expected TLS certificate SHA-256 fingerprint")),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("lock_token", sdnLockTokenArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if a.Truthy("create") && !a.Truthy("type") {
				return nil, errors.New("`type` is required when creating an IPAM")
			}
			return sdnSaveObject(ctx, c, "ipam", a.Str("ipam"), a.Truthy("create"),
				js.O("url", a.Get("url"), "token", a.Get("token"), "section", a.Get("section"), "fingerprint", a.Get("fingerprint")),
				sdnSaveOpts{createOnly: js.O("type", a.Get("type")), deleteKeys: a.Get("delete"), digest: a.Get("digest"), lockToken: a.Get("lock_token"), extra: a.Get("extra")})
		},
	})

	c.Define("proxmox_save_sdn_dns", Def{
		Toolset:     "sdn",
		Title:       "Create or update an SDN DNS plugin",
		Description: "Create (create=true) or update an SDN DNS plugin (PowerDNS) used to register guest hostnames for zones with dns/dnszone set. Only given fields change on update. " + sdnNote,
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("dns", Str().D("DNS plugin ID")),
			P("create", sdnCreateArg),
			P("type", Enum("powerdns").Opt().D("Plugin type (required when creating)")),
			P("url", Str().Opt().D("PowerDNS API URL (required when creating), e.g. http://pdns:8081/api/v1/servers/localhost")),
			P("key", Str().Opt().D("PowerDNS API key (required when creating)")),
			P("ttl", Int().Opt().D("TTL of created records")),
			P("reversemaskv6", Int().Opt().D("IPv6 reverse zone mask")),
			P("fingerprint", Str().Opt().D("Expected TLS certificate SHA-256 fingerprint")),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("lock_token", sdnLockTokenArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if a.Truthy("create") && (!a.Truthy("type") || !a.Truthy("url") || !a.Truthy("key")) {
				return nil, errors.New("`type`, `url` and `key` are required when creating a DNS plugin")
			}
			return sdnSaveObject(ctx, c, "dns", a.Str("dns"), a.Truthy("create"),
				js.O("url", a.Get("url"), "key", a.Get("key"), "ttl", a.Get("ttl"), "reversemaskv6", a.Get("reversemaskv6"), "fingerprint", a.Get("fingerprint")),
				sdnSaveOpts{createOnly: js.O("type", a.Get("type")), deleteKeys: a.Get("delete"), digest: a.Get("digest"), lockToken: a.Get("lock_token"), extra: a.Get("extra")})
		},
	})

	c.Define("proxmox_delete_sdn_object", Def{
		Toolset:     "sdn",
		Title:       "Delete an SDN object",
		Description: "Delete an SDN zone, vnet, subnet, controller, IPAM or DNS plugin. Objects still in use (e.g. a zone with vnets, a vnet with subnets) are refused by Proxmox. " + sdnNote,
		Delete:      true,
		Input: []Prop{
			P("kind", sdnKindArg),
			P("id", Str().D("Object ID (for subnets the CIDR or subnet ID)")),
			P("vnet", sdnVnetForSubnetArg),
			P("lock_token", sdnLockTokenArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			path, err := sdnResolvedObjectPath(ctx, c, a.Str("kind"), a.Str("id"), a.Str("vnet"))
			if err != nil {
				return nil, err
			}
			if _, err := client.Delete(ctx, path, defined(js.O("lock-token", a.Get("lock_token")))); err != nil {
				return nil, err
			}
			return js.O("deleted", a.Get("id"), "kind", a.Get("kind"), "note", sdnNote), nil
		},
	})

	vnetZone := func(ctx context.Context, a *Obj) (any, error) {
		zone := a.Get("zone")
		if js.Nullish(zone) {
			v, err := client.GetObj(ctx, "/cluster/sdn/vnets/"+enc(a.Get("vnet")), nil)
			if err != nil {
				return nil, err
			}
			zone = v.Get("zone")
		}
		if !js.Truthy(zone) {
			return nil, fmt.Errorf("Could not determine the zone of vnet %s; pass `zone`", a.Str("vnet"))
		}
		return zone, nil
	}

	c.Define("proxmox_save_sdn_vnet_ip", Def{
		Toolset:     "sdn",
		Title:       "Create or update a vnet IP mapping",
		Description: "Create (create=true) or update an IP-to-MAC mapping in a vnet's IPAM, e.g. to reserve a fixed DHCP address for a guest. Takes effect immediately (no apply needed). See proxmox_get_sdn_ipam_status for current entries.",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("vnet", Str().D("VNet ID")),
			P("zone", Str().Opt().D("Zone of the vnet (looked up from the vnet when omitted)")),
			P("ip", Str().D("IP address")),
			P("mac", Str().Opt().D("MAC address to associate")),
			P("vmid", Int().Opt().D("VM/container ID owning the IP (update only)")),
			P("create", sdnCreateArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			zone, err := vnetZone(ctx, a)
			if err != nil {
				return nil, err
			}
			create := a.Truthy("create")
			params := defined(js.O("zone", zone, "ip", a.Get("ip"), "mac", a.Get("mac"), "vmid", ifThen(!create, a.Get("vmid"))))
			path := "/cluster/sdn/vnets/" + enc(a.Get("vnet")) + "/ips"
			if create {
				_, err = client.Post(ctx, path, params)
			} else {
				_, err = client.Put(ctx, path, params)
			}
			if err != nil {
				return nil, err
			}
			return js.O("saved", a.Get("ip"), "vnet", a.Get("vnet"), "zone", zone, "action", pick(create, "created", "updated")), nil
		},
	})

	c.Define("proxmox_delete_sdn_vnet_ip", Def{
		Toolset:     "sdn",
		Title:       "Delete a vnet IP mapping",
		Description: "Remove an IP mapping (IPAM entry) from a vnet, freeing the address.",
		Delete:      true,
		Input: []Prop{
			P("vnet", Str().D("VNet ID")),
			P("zone", Str().Opt().D("Zone of the vnet (looked up from the vnet when omitted)")),
			P("ip", Str().D("IP address to remove")),
			P("mac", Str().Opt().D("MAC address of the mapping")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			zone, err := vnetZone(ctx, a)
			if err != nil {
				return nil, err
			}
			if _, err := client.Delete(ctx, "/cluster/sdn/vnets/"+enc(a.Get("vnet"))+"/ips", defined(js.O("zone", zone, "ip", a.Get("ip"), "mac", a.Get("mac")))); err != nil {
				return nil, err
			}
			return js.O("deleted", a.Get("ip"), "vnet", a.Get("vnet"), "zone", zone), nil
		},
	})

	// ---------------------------------------------------------------- apply / rollback

	c.Define("proxmox_apply_sdn", Def{
		Toolset:     "sdn",
		Title:       "Apply SDN configuration",
		Description: "Apply all pending SDN changes: writes the network and FRR configuration to every node and reloads networking. This can briefly disrupt guest networking on SDN vnets. Review with proxmox_list_sdn_objects pending=true or proxmox_get_sdn_pending_diff first, and check proxmox_get_sdn_status afterwards.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("lock_token", sdnLockTokenArg),
			P("release_lock", Bool().Opt().D("With lock_token: release the lock after applying (default true)")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			upid, err := client.Put(ctx, "/cluster/sdn", defined(js.O("lock-token", a.Get("lock_token"), "release-lock", a.Get("release_lock"))))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("action", "apply-sdn"))
		},
	})

	c.Define("proxmox_rollback_sdn", Def{
		Toolset:     "sdn",
		Title:       "Discard pending SDN changes",
		Description: "Roll back all pending (not yet applied) SDN changes to the running configuration (PVE 9). Irreversible: staged edits are lost. Running networking is not touched.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("lock_token", sdnLockTokenArg),
			P("release_lock", Bool().Opt().D("With lock_token: release the lock afterwards (default true)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Post(ctx, "/cluster/sdn/rollback", defined(js.O("lock-token", a.Get("lock_token"), "release-lock", a.Get("release_lock")))); err != nil {
				return nil, err
			}
			return js.O("rolled_back", true), nil
		},
	})
}
