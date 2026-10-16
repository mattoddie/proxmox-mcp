package tools

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

var (
	haSidArg = Union(Str(), Int()).D("HA resource ID like vm:100 or ct:200, or just the VMID (100) which is resolved to vm:/ct: automatically")

	haSidListArg = Arr(Union(Str(), Int())).D("HA resources, e.g. [\"vm:100\", \"ct:200\"] or plain VMIDs [100, 200]")

	haNodeListArg = Arr(Str()).D("Nodes with optional priority (higher is preferred), e.g. [\"pve1:2\", \"pve2:1\"] or [\"pve1\", \"pve2\"]")

	haStates = []string{"started", "stopped", "enabled", "disabled", "ignored"}

	haDigitsRe = regexp.MustCompile(`^\d+$`)
	haVmidRe   = regexp.MustCompile(`^(?:vm|ct):(\d+)$`)
)

// haCanonicalSids turns plain VMIDs into vm:100 / ct:200 (looked up in /cluster/resources); other forms pass through.
func haCanonicalSids(ctx context.Context, client *proxmox.Client, sids []any) ([]string, error) {
	numeric := false
	for _, s := range sids {
		if haDigitsRe.MatchString(strings.TrimSpace(js.Str(s))) {
			numeric = true
		}
	}
	guests := []*Obj{}
	if numeric {
		var err error
		if guests, err = client.GetList(ctx, "/cluster/resources", js.O("type", "vm")); err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(sids))
	for _, s := range sids {
		str := strings.TrimSpace(js.Str(s))
		if !haDigitsRe.MatchString(str) {
			out = append(out, str)
			continue
		}
		var g *Obj
		for _, r := range guests {
			if r.Num("vmid") == js.Num(str) {
				g = r
				break
			}
		}
		if g == nil {
			return nil, fmt.Errorf("No VM or container with ID %s; give the HA resource ID as vm:%s or ct:%s", str, str, str)
		}
		out = append(out, js.Str(pick(g.Get("type") == "lxc", "ct", "vm"))+":"+str)
	}
	return out, nil
}

func haCanonicalSid(ctx context.Context, client *proxmox.Client, sid any) (string, error) {
	out, err := haCanonicalSids(ctx, client, []any{sid})
	if err != nil {
		return "", err
	}
	return out[0], nil
}

// haVmidOf is the VMID of vm:N / ct:N, or undefined.
func haVmidOf(sid any) any {
	s, _ := sid.(string)
	if m := haVmidRe.FindStringSubmatch(s); m != nil {
		return js.Num(m[1])
	}
	return js.Undef
}

func haSplit(v any) any {
	if !js.Truthy(v) {
		return js.Undef
	}
	return js.Array(strings.Split(js.Str(v), ","))
}

func haSummarizeResource(r, status, guest *Obj) *Obj {
	var current any = js.Undef
	if status != nil {
		current = proxmox.C("node", status.Get("node"), "state", status.Get("state"), "crm_state", status.Get("crm_state"), "request_state", status.Get("request_state"), "status", status.Get("status"))
	}
	return proxmox.C(
		"sid", r.Get("sid"),
		"name", guest.Get("name"),
		"type", r.Get("type"),
		"state", or(r.Get("state"), "started"),
		"group", r.Get("group"),
		"max_restart", r.Get("max_restart"),
		"max_relocate", r.Get("max_relocate"),
		"failback", ifThen(r.Defined("failback"), js.Truthy(proxmox.Flag(r.Get("failback")))),
		"auto_rebalance", ifThen(r.Defined("auto-rebalance"), js.Truthy(proxmox.Flag(r.Get("auto-rebalance")))),
		"comment", r.Get("comment"),
		"current", current,
	)
}

func haSummarizeGroup(g *Obj) any {
	return proxmox.C(
		"group", g.Get("group"),
		"nodes", haSplit(g.Get("nodes")),
		"restricted", js.Truthy(proxmox.Flag(g.Get("restricted"))),
		"nofailback", js.Truthy(proxmox.Flag(g.Get("nofailback"))),
		"comment", g.Get("comment"),
	)
}

func haSummarizeRule(r *Obj) any {
	return proxmox.C(
		"rule", r.Get("rule"),
		"type", r.Get("type"),
		"resources", haSplit(r.Get("resources")),
		"nodes", haSplit(r.Get("nodes")),
		"affinity", r.Get("affinity"),
		"strict", ifThen(r.Defined("strict"), js.Truthy(proxmox.Flag(r.Get("strict")))),
		"disabled", ifThen(js.Truthy(proxmox.Flag(r.Get("disable"))), true),
		"comment", r.Get("comment"),
	)
}

// haJoin is arr?.join(",").
func haJoin(v any) any {
	arr, ok := v.([]any)
	if !ok {
		return js.Undef
	}
	return js.Str(arr)
}

func registerHa(c *Context) {
	client := c.Client

	// ---------------------------------------------------------------- status

	c.Define("proxmox_get_ha_status", Def{
		Toolset:     "ha",
		Title:       "Get HA status",
		Description: "High-availability overview: quorum, the current CRM master, each node's LRM state (active/idle/maintenance, last update), fencing/arming state (PVE 9.1+), and every HA-managed service with its node and state. Use proxmox_list_ha_resources for the configured settings.",
		Input:       []Prop{P("raw", rawArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			currentV, err := client.Get(ctx, "/cluster/ha/status/current", nil)
			if err != nil {
				return nil, err
			}
			manager, merr := client.Get(ctx, "/cluster/ha/status/manager_status", nil)
			if merr != nil {
				manager = js.Undef
			}
			if a.Truthy("raw") {
				return js.O("current", currentV, "manager_status", manager), nil
			}
			current := js.Objs(currentV)
			find := func(t string) *Obj {
				for _, e := range current {
					if e.Get("type") == t {
						return e
					}
				}
				return nil
			}
			quorum, master, fencing := find("quorum"), find("master"), find("fencing")
			mgr, _ := manager.(*Obj)
			ms, _ := or(mgr.Get("manager_status"), js.NewObj()).(*Obj)
			lrmStatus, _ := or(mgr.Get("lrm_status"), js.NewObj()).(*Obj)
			nodeStatus, _ := or(ms.Get("node_status"), js.NewObj()).(*Obj)
			lrmEntries := filterObjs(current, func(e *Obj) bool { return e.Get("type") == "lrm" })
			var names []any
			seen := map[string]bool{}
			addName := func(v any) {
				k := js.Stringify(v, "")
				if !seen[k] {
					seen[k] = true
					names = append(names, v)
				}
			}
			for _, e := range lrmEntries {
				addName(e.Get("node"))
			}
			for _, k := range nodeStatus.Keys() {
				addName(k)
			}
			for _, k := range lrmStatus.Keys() {
				addName(k)
			}
			// Array.prototype.sort(): undefined last, everything else by string.
			sort.SliceStable(names, func(i, j int) bool {
				ui, uj := js.IsUndef(names[i]), js.IsUndef(names[j])
				if ui || uj {
					return !ui && uj
				}
				return js.Str(names[i]) < js.Str(names[j])
			})
			nodes := make([]any, 0, len(names))
			for _, node := range names {
				var entry *Obj
				for _, e := range lrmEntries {
					if e.Get("node") == node {
						entry = e
						break
					}
				}
				lrm, _ := or(lrmStatus.Get(js.Str(node)), js.NewObj()).(*Obj)
				nodes = append(nodes, proxmox.C(
					"node", node,
					"crm_node_state", nodeStatus.Get(js.Str(node)),
					"lrm_state", lrm.Get("state"),
					"lrm_mode", lrm.Get("mode"),
					"status", entry.Get("status"),
					"last_update", proxmox.IsoTime(or(entry.Get("timestamp"), lrm.Get("timestamp"))),
				))
			}
			services := mapObjs(filterObjs(current, func(e *Obj) bool { return e.Get("type") == "service" }), func(e *Obj) any {
				return proxmox.C("sid", e.Get("sid"), "node", e.Get("node"), "state", e.Get("state"), "crm_state", e.Get("crm_state"), "request_state", e.Get("request_state"), "status", e.Get("status"))
			})
			var q, m, f any = js.Undef, js.Undef, js.Undef
			if quorum != nil {
				q = proxmox.C("quorate", js.Truthy(proxmox.Flag(quorum.Get("quorate"))), "node", quorum.Get("node"), "status", quorum.Get("status"))
			}
			if master != nil {
				m = proxmox.C("node", master.Get("node"), "status", master.Get("status"), "last_update", proxmox.IsoTime(master.Get("timestamp")))
			}
			if fencing != nil {
				f = proxmox.C("armed_state", fencing.Get("armed-state"), "resource_mode", fencing.Get("resource_mode"), "status", fencing.Get("status"))
			}
			return proxmox.C(
				"quorum", q,
				"master", m,
				"fencing", f,
				"nodes", nodes,
				"services", services,
				"note", ifThen(len(services) == 0, "No HA-managed resources (add one with proxmox_save_ha_resource)"),
			), nil
		},
	})

	// ---------------------------------------------------------------- resources

	c.Define("proxmox_list_ha_resources", Def{
		Toolset:     "ha",
		Title:       "List HA resources",
		Description: "List HA-managed VMs and containers with their requested state, group, restart/relocate limits, failback, comment, guest name and current status (node and CRM state).",
		Input: []Prop{
			P("type", Enum("vm", "ct").Opt().D("Only VMs or only containers")),
			P("search", searchArg),
			P("limit", limitArg(500)),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			resources, err := client.GetList(ctx, "/cluster/ha/resources", defined(js.O("type", a.Get("type"))))
			if err != nil {
				return nil, err
			}
			current, err := client.GetList(ctx, "/cluster/ha/status/current", nil)
			if err != nil {
				current = []*Obj{}
			}
			guests, err := client.GetList(ctx, "/cluster/resources", js.O("type", "vm"))
			if err != nil {
				guests = []*Obj{}
			}
			status := map[string]*Obj{}
			for _, e := range current {
				if e.Get("type") == "service" {
					status[js.Stringify(e.Get("sid"), "")] = e
				}
			}
			items := make([]*Obj, len(resources))
			for i, r := range resources {
				vmid := haVmidOf(r.Get("sid"))
				var guest *Obj
				for _, g := range guests {
					if !js.IsUndef(vmid) && g.Num("vmid") == vmid {
						guest = g
						break
					}
				}
				items[i] = haSummarizeResource(r, status[js.Stringify(r.Get("sid"), "")], guest)
			}
			return filterAndLimit(items, a.Get("search"), or(a.Get("limit"), 500), func(r *Obj) []any {
				return []any{r.Get("sid"), r.Get("name"), r.Get("group"), r.Get("comment"), r.Get("state")}
			}).Obj(nil), nil
		},
	})

	c.Define("proxmox_get_ha_resource", Def{
		Toolset:     "ha",
		Title:       "Get an HA resource",
		Description: "Get one HA resource's configuration, its current status, and the HA rules that reference it (PVE 9+).",
		Input:       []Prop{P("sid", haSidArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			sid, err := haCanonicalSid(ctx, client, a.Get("sid"))
			if err != nil {
				return nil, err
			}
			r, err := client.GetObj(ctx, "/cluster/ha/resources/"+enc(sid), nil)
			if err != nil {
				return nil, err
			}
			current, err := client.GetList(ctx, "/cluster/ha/status/current", nil)
			if err != nil {
				current = []*Obj{}
			}
			var rules any = js.Undef
			if v, err := client.Get(ctx, "/cluster/ha/rules", js.O("resource", sid)); err == nil {
				if arr, ok := v.([]any); ok {
					rules = mapObjs(js.Objs(arr), haSummarizeRule)
				}
			}
			var status *Obj
			for _, e := range current {
				if e.Get("type") == "service" && e.Get("sid") == sid {
					status = e
					break
				}
			}
			return proxmox.Compact(js.Spread(haSummarizeResource(js.Spread(js.O("sid", sid), r), status, nil), js.O("rules", rules, "digest", r.Get("digest")))), nil
		},
	})

	c.Define("proxmox_save_ha_resource", Def{
		Toolset:     "ha",
		Title:       "Add or update an HA resource",
		Description: "Put a VM/container under HA management, or update its HA settings if it already is (only the given fields change). state: started (keep running), stopped, disabled (stopped, no recovery), ignored (HA hands off). New resources default to state started, which starts the guest.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("sid", haSidArg),
			P("state", Enum(haStates...).Opt().D("Requested state (default started)")),
			P("group", Str().Opt().D("HA group (PVE 8; replaced by node-affinity rules in PVE 9)")),
			P("max_restart", Int().Min(0).Opt().D("Restart attempts on the same node after a failed start (default 1)")),
			P("max_relocate", Int().Min(0).Opt().D("Relocation attempts to other nodes after failed restarts (default 1)")),
			P("failback", Bool().Opt().D("Move back to the highest-priority node when it returns (PVE 9, default true)")),
			P("auto_rebalance", Bool().Opt().D("May be moved by automatic load rebalancing (PVE 9.1+, default true)")),
			P("comment", Str().Opt().D("Description")),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			sid, err := haCanonicalSid(ctx, client, a.Get("sid"))
			if err != nil {
				return nil, err
			}
			list, err := client.GetList(ctx, "/cluster/ha/resources", nil)
			if err != nil {
				return nil, err
			}
			existing := len(filterObjs(list, func(r *Obj) bool { return r.Get("sid") == sid })) > 0
			params, err := definedExtra(js.O(
				"state", a.Get("state"),
				"group", a.Get("group"),
				"max_restart", a.Get("max_restart"),
				"max_relocate", a.Get("max_relocate"),
				"failback", a.Get("failback"),
				"auto-rebalance", a.Get("auto_rebalance"),
				"comment", a.Get("comment"),
			), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			if existing {
				if _, err := client.Put(ctx, "/cluster/ha/resources/"+enc(sid), defined(js.Spread(params, js.O("delete", deleteParam(a.Get("delete")), "digest", a.Get("digest"))))); err != nil {
					return nil, err
				}
			} else {
				if len(a.Arr("delete")) > 0 {
					return nil, fmt.Errorf("%s is not an HA resource yet; delete only applies to updates", sid)
				}
				if _, err := client.Post(ctx, "/cluster/ha/resources", js.Spread(js.O("sid", sid), params)); err != nil {
					return nil, err
				}
			}
			r, err := client.GetObj(ctx, "/cluster/ha/resources/"+enc(sid), nil)
			if err != nil {
				return nil, err
			}
			return js.O(js.Str(pick(existing, "updated", "created")), sid, "resource", haSummarizeResource(js.Spread(js.O("sid", sid), r), nil, nil)), nil
		},
	})

	c.Define("proxmox_delete_ha_resource", Def{
		Toolset:     "ha",
		Title:       "Remove an HA resource",
		Description: "Take a VM/container out of HA management (the guest itself is untouched and keeps running). By default it is also removed from HA rules, deleting rules left empty; purge=false keeps the rules.",
		Delete:      true,
		Destructive: True,
		Input:       []Prop{P("sid", haSidArg), P("purge", Bool().Opt().D("Also remove it from HA rules (default true)"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			sid, err := haCanonicalSid(ctx, client, a.Get("sid"))
			if err != nil {
				return nil, err
			}
			if _, err := client.Delete(ctx, "/cluster/ha/resources/"+enc(sid), defined(js.O("purge", a.Get("purge")))); err != nil {
				return nil, err
			}
			return js.O("deleted", sid), nil
		},
	})

	c.Define("proxmox_ha_migrate", Def{
		Toolset:     "ha",
		Title:       "Migrate or relocate an HA resource",
		Description: "Ask the HA manager to move an HA-managed guest to another node. migrate = live/online migration; relocate = stop on the old node and start on the target. For HA-managed guests use this rather than the normal migrate tools. The request is queued; follow it with proxmox_get_ha_status. PVE 9 reports resources that block the move (affinity rules) or move along with it.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("sid", haSidArg),
			P("action", Enum("migrate", "relocate").D("migrate (online) or relocate (stop + start)")),
			P("node", Str().D("Target node")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			sid, err := haCanonicalSid(ctx, client, a.Get("sid"))
			if err != nil {
				return nil, err
			}
			v, err := client.Post(ctx, "/cluster/ha/resources/"+enc(sid)+"/"+js.Str(a.Get("action")), js.O("node", a.Get("node")))
			if err != nil {
				return nil, err
			}
			result, _ := v.(*Obj)
			return proxmox.C(
				"sid", sid,
				"action", a.Get("action"),
				"target", a.Get("node"),
				"requested", true,
				"blocking_resources", result.Get("blocking-resources"),
				"comigrated_resources", result.Get("comigrated-resources"),
				"note", "The HA manager carries this out asynchronously; check proxmox_get_ha_status",
			), nil
		},
	})

	// ---------------------------------------------------------------- groups (PVE 8)

	c.Define("proxmox_list_ha_groups", Def{
		Toolset:     "ha",
		Title:       "List HA groups",
		Description: "List HA groups (node lists with priorities, restricted, nofailback). Deprecated in PVE 9, where groups are migrated to node-affinity rules (proxmox_list_ha_rules).",
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			groups, err := client.GetList(ctx, "/cluster/ha/groups", nil)
			if err != nil {
				return nil, err
			}
			return mapObjs(groups, haSummarizeGroup), nil
		},
	})

	c.Define("proxmox_get_ha_group", Def{
		Toolset:     "ha",
		Title:       "Get an HA group",
		Description: "Get one HA group's configuration (PVE 8; deprecated in PVE 9 in favour of HA rules).",
		Input:       []Prop{P("group", Str().D("HA group name"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := client.GetObj(ctx, "/cluster/ha/groups/"+enc(a.Get("group")), nil)
			if err != nil {
				return nil, err
			}
			return proxmox.Compact(js.Spread(haSummarizeGroup(js.Spread(js.O("group", a.Get("group")), g)), js.O("digest", g.Get("digest")))), nil
		},
	})

	c.Define("proxmox_save_ha_group", Def{
		Toolset:     "ha",
		Title:       "Create or update an HA group",
		Description: "Create an HA group or update an existing one (PVE 8; on PVE 9 use proxmox_save_ha_rule with type node-affinity). restricted: resources may only run on the group's nodes; nofailback: don't move back when a higher-priority node returns.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("group", Str().D("HA group name")),
			P("nodes", haNodeListArg.Opt().D("Nodes with optional priority, e.g. [\"pve1:2\", \"pve2\"] (required when creating)")),
			P("restricted", Bool().Opt().D("Only run on the listed nodes")),
			P("nofailback", Bool().Opt().D("Don't migrate back to a higher-priority node")),
			P("comment", Str().Opt().D("Description")),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			groups, err := client.GetList(ctx, "/cluster/ha/groups", nil)
			if err != nil {
				return nil, err
			}
			exists := len(filterObjs(groups, func(g *Obj) bool { return g.Get("group") == a.Get("group") })) > 0
			params, err := definedExtra(js.O("nodes", haJoin(a.Get("nodes")), "restricted", a.Get("restricted"), "nofailback", a.Get("nofailback"), "comment", a.Get("comment")), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			if exists {
				if _, err := client.Put(ctx, "/cluster/ha/groups/"+enc(a.Get("group")), defined(js.Spread(params, js.O("delete", deleteParam(a.Get("delete")), "digest", a.Get("digest"))))); err != nil {
					return nil, err
				}
			} else {
				if len(a.Arr("nodes")) == 0 {
					return nil, errors.New("nodes is required when creating an HA group")
				}
				if _, err := client.Post(ctx, "/cluster/ha/groups", js.Spread(js.O("group", a.Get("group")), params)); err != nil {
					return nil, err
				}
			}
			return js.O(js.Str(pick(exists, "updated", "created")), a.Get("group")), nil
		},
	})

	c.Define("proxmox_delete_ha_group", Def{
		Toolset:     "ha",
		Title:       "Delete an HA group",
		Description: "Delete an HA group (PVE 8). Fails while HA resources still use it.",
		Delete:      true,
		Input:       []Prop{P("group", Str().D("HA group name"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/cluster/ha/groups/"+enc(a.Get("group")), nil); err != nil {
				return nil, err
			}
			return js.O("deleted", a.Get("group")), nil
		},
	})

	// ---------------------------------------------------------------- rules (PVE 9)

	c.Define("proxmox_list_ha_rules", Def{
		Toolset:     "ha",
		Title:       "List HA rules",
		Description: "List HA rules (PVE 9+): node-affinity rules (which nodes resources prefer or must run on, with priorities) and resource-affinity rules (keep resources together or apart).",
		Input: []Prop{
			P("type", Enum("node-affinity", "resource-affinity").Opt().D("Only this rule type")),
			P("resource", haSidArg.Opt().D("Only rules affecting this resource (vm:100, ct:200 or a VMID)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			var resource any = js.Undef
			if a.Defined("resource") {
				sid, err := haCanonicalSid(ctx, client, a.Get("resource"))
				if err != nil {
					return nil, err
				}
				resource = sid
			}
			rules, err := client.GetList(ctx, "/cluster/ha/rules", defined(js.O("type", a.Get("type"), "resource", resource)))
			if err != nil {
				return nil, err
			}
			return mapObjs(rules, haSummarizeRule), nil
		},
	})

	c.Define("proxmox_get_ha_rule", Def{
		Toolset:     "ha",
		Title:       "Get an HA rule",
		Description: "Get one HA rule (PVE 9+) with its digest.",
		Input:       []Prop{P("rule", Str().D("HA rule ID"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			r, err := client.GetObj(ctx, "/cluster/ha/rules/"+enc(a.Get("rule")), nil)
			if err != nil {
				return nil, err
			}
			return proxmox.Compact(js.Spread(haSummarizeRule(js.Spread(js.O("rule", a.Get("rule")), r)), js.O("digest", r.Get("digest")))), nil
		},
	})

	c.Define("proxmox_save_ha_rule", Def{
		Toolset:     "ha",
		Title:       "Create or update an HA rule",
		Description: "Create or update an HA rule (PVE 9+). node-affinity: run `resources` on `nodes` (with priorities; strict=true means only those nodes; affinity negative = avoid them). resource-affinity: affinity positive keeps `resources` on the same node, negative keeps them on different nodes. Only the given fields change on update.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("rule", Str().D("HA rule ID")),
			P("type", Enum("node-affinity", "resource-affinity").Opt().D("Rule type (required when creating)")),
			P("resources", haSidListArg.Opt().D("HA resources the rule applies to, e.g. [\"vm:100\", \"ct:200\"] or VMIDs (required when creating)")),
			P("nodes", haNodeListArg.Opt().D("node-affinity: nodes with optional priority, e.g. [\"pve1:2\", \"pve2:1\"] (required when creating)")),
			P("affinity", Enum("positive", "negative").Opt().D("positive (together / on these nodes) or negative (apart / not on these nodes). Required for resource-affinity")),
			P("strict", Bool().Opt().D("node-affinity: only ever run on the listed nodes (default false)")),
			P("disable", Bool().Opt().D("Disable the rule without deleting it")),
			P("comment", Str().Opt().D("Description")),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			list, err := client.GetList(ctx, "/cluster/ha/rules", nil)
			if err != nil {
				return nil, err
			}
			var existing *Obj
			for _, r := range list {
				if r.Get("rule") == a.Get("rule") {
					existing = r
					break
				}
			}
			var resources any = js.Undef
			if a.Truthy("resources") {
				sids, err := haCanonicalSids(ctx, client, a.Arr("resources"))
				if err != nil {
					return nil, err
				}
				resources = strings.Join(sids, ",")
			}
			params, err := definedExtra(js.O(
				"resources", resources,
				"nodes", haJoin(a.Get("nodes")),
				"affinity", a.Get("affinity"),
				"strict", a.Get("strict"),
				"disable", a.Get("disable"),
				"comment", a.Get("comment"),
			), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			ruleID := js.Str(a.Get("rule"))
			if existing != nil {
				if a.Truthy("type") && existing.Truthy("type") && a.Get("type") != existing.Get("type") {
					return nil, fmt.Errorf("Rule %s is a %s rule; its type can't be changed", ruleID, js.Str(existing.Get("type")))
				}
				// The section-config update schema is selected by `type`, so it must be sent on updates too.
				if _, err := client.Put(ctx, "/cluster/ha/rules/"+enc(ruleID), defined(js.Spread(js.O("type", or(a.Get("type"), existing.Get("type"))), params, js.O("delete", deleteParam(a.Get("delete")), "digest", a.Get("digest"))))); err != nil {
					return nil, err
				}
			} else {
				if !a.Truthy("type") {
					return nil, errors.New("type is required when creating an HA rule")
				}
				if !js.Truthy(resources) {
					return nil, errors.New("resources is required when creating an HA rule")
				}
				if a.Get("type") == "node-affinity" && len(a.Arr("nodes")) == 0 {
					return nil, errors.New("nodes is required for a node-affinity rule")
				}
				if a.Get("type") == "resource-affinity" && !a.Truthy("affinity") {
					return nil, errors.New("affinity is required for a resource-affinity rule")
				}
				if _, err := client.Post(ctx, "/cluster/ha/rules", js.Spread(js.O("rule", a.Get("rule"), "type", a.Get("type")), params)); err != nil {
					return nil, err
				}
			}
			var rule any = js.Undef
			if v, err := client.Get(ctx, "/cluster/ha/rules/"+enc(ruleID), nil); err == nil && js.Truthy(v) {
				rule = haSummarizeRule(js.Spread(js.O("rule", a.Get("rule")), v))
			}
			return proxmox.C(js.Str(pick(existing != nil, "updated", "created")), a.Get("rule"), "rule", rule), nil
		},
	})

	c.Define("proxmox_delete_ha_rule", Def{
		Toolset:     "ha",
		Title:       "Delete an HA rule",
		Description: "Delete an HA rule (PVE 9+). The resources it referenced stay HA-managed.",
		Delete:      true,
		Input:       []Prop{P("rule", Str().D("HA rule ID"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/cluster/ha/rules/"+enc(a.Get("rule")), nil); err != nil {
				return nil, err
			}
			return js.O("deleted", a.Get("rule")), nil
		},
	})

	// ---------------------------------------------------------------- arming (PVE 9.1+)

	c.Define("proxmox_set_ha_armed", Def{
		Toolset:     "ha",
		Title:       "Arm or disarm the HA stack",
		Description: "Disarm the HA stack cluster-wide (releases all watchdogs, so nodes won't self-fence; e.g. for network or switch maintenance) or re-arm it afterwards. PVE 9.1+. resource_mode for disarm: freeze (no HA commands/state changes are applied) or ignore (HA stops managing resources, which can be handled manually). Check the result with proxmox_get_ha_status.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("action", Enum("arm", "disarm").D("arm or disarm")),
			P("resource_mode", Enum("freeze", "ignore").Opt().D("How HA resources are handled while disarmed (required for disarm)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if a.Get("action") == "disarm" {
				if !a.Truthy("resource_mode") {
					return nil, errors.New("resource_mode (freeze or ignore) is required to disarm")
				}
				if _, err := client.Post(ctx, "/cluster/ha/status/disarm-ha", js.O("resource-mode", a.Get("resource_mode"))); err != nil {
					return nil, err
				}
			} else if _, err := client.Post(ctx, "/cluster/ha/status/arm-ha", nil); err != nil {
				return nil, err
			}
			return js.O("requested", a.Get("action"), "resource_mode", a.Get("resource_mode"), "note", "Check progress with proxmox_get_ha_status (fencing.armed_state)"), nil
		},
	})
}
