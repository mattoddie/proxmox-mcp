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

// ---------------------------------------------------------------- scopes

var firewallScopeText = map[string]string{
	"cluster": "cluster = datacenter-wide",
	"node":    "node = a host's own firewall",
	"guest":   "guest = a VM or container (give vmid)",
	"group":   "group = a security group's rules (give group)",
}

// firewallScopeArgs is the scope argument trio/quartet, restricted to the levels where an endpoint exists.
func firewallScopeArgs(scopes ...string) []Prop {
	texts := make([]string, len(scopes))
	for i, s := range scopes {
		texts[i] = firewallScopeText[s]
	}
	inferred := ""
	if contains(scopes, "guest") {
		inferred += "guest if vmid is given, "
	}
	if contains(scopes, "group") {
		inferred += "group if group is given, "
	}
	if contains(scopes, "node") {
		inferred += "node if node is given, "
	}
	fallback := scopes[0]
	if contains(scopes, "cluster") {
		fallback = "cluster"
	}
	props := []Prop{
		P("scope", Enum(scopes...).Opt().D(fmt.Sprintf("Firewall level: %s. Inferred when omitted: %sotherwise %s.", strings.Join(texts, "; "), inferred, fallback))),
		P("node", Str().Opt().D("Node name: the host for scope node (defaults to PROXMOX_DEFAULT_NODE or the only node), or the guest's node for scope guest (looked up from vmid when omitted)")),
	}
	if contains(scopes, "guest") {
		props = append(props, P("vmid", vmidArg.Opt().D("VM or container ID, for scope guest")))
	}
	if contains(scopes, "group") {
		props = append(props, P("group", Str().Opt().D("Security group name, for scope group (see proxmox_list_firewall_groups)")))
	}
	return props
}

// firewallScopeRef is a resolved firewall level.
type firewallScopeRef struct {
	scope string
	// fw is the firewall base path, e.g. /cluster/firewall or /nodes/pve1/qemu/100/firewall.
	fw string
	// rules is the rules collection path (for groups, the group itself).
	rules string
	// target is what the scope points at, echoed in results.
	target *Obj
}

func firewallResolveScope(ctx context.Context, client *proxmox.Client, a *Obj, allowed []string) (firewallScopeRef, error) {
	fallback := allowed[0]
	if contains(allowed, "cluster") {
		fallback = "cluster"
	}
	inferred := fallback
	switch {
	case a.Defined("vmid") && contains(allowed, "guest"):
		inferred = "guest"
	case a.Truthy("group") && contains(allowed, "group"):
		inferred = "group"
	case a.Truthy("node") && contains(allowed, "node"):
		inferred = "node"
	}
	scope := js.Str(or(a.Get("scope"), inferred))
	if !contains(allowed, scope) {
		return firewallScopeRef{}, fmt.Errorf(`scope "%s" is not supported here; use one of: %s`, scope, strings.Join(allowed, ", "))
	}
	if scope != "guest" && a.Defined("vmid") {
		return firewallScopeRef{}, fmt.Errorf(`vmid only applies to scope "guest" (scope is "%s")`, scope)
	}
	if scope != "group" && a.Truthy("group") {
		return firewallScopeRef{}, fmt.Errorf(`group only applies to scope "group" (scope is "%s")`, scope)
	}
	switch scope {
	case "cluster":
		if a.Truthy("node") {
			return firewallScopeRef{}, errors.New(`node does not apply to scope "cluster"; use scope "node" for a host firewall`)
		}
		return firewallScopeRef{scope, "/cluster/firewall", "/cluster/firewall/rules", js.O("scope", scope)}, nil
	case "node":
		node, err := client.ResolveNode(ctx, a.Str("node"))
		if err != nil {
			return firewallScopeRef{}, err
		}
		fw := "/nodes/" + enc(node) + "/firewall"
		return firewallScopeRef{scope, fw, fw + "/rules", js.O("scope", scope, "node", node)}, nil
	case "guest":
		if !a.Defined("vmid") {
			return firewallScopeRef{}, errors.New(`vmid is required for scope "guest"`)
		}
		g, err := client.ResolveGuest(ctx, a.Num("vmid"), a.Str("node"), "")
		if err != nil {
			return firewallScopeRef{}, err
		}
		fw := client.GuestPath(g) + "/firewall"
		return firewallScopeRef{scope, fw, fw + "/rules", proxmox.C("scope", scope, "vmid", g.VMID, "node", g.Node, "type", pick(g.Type == "qemu", "vm", "container"), "name", g.Name)}, nil
	default: // group
		if !a.Truthy("group") {
			return firewallScopeRef{}, errors.New(`group is required for scope "group"`)
		}
		return firewallScopeRef{scope, "/cluster/firewall", "/cluster/firewall/groups/" + enc(a.Get("group")), js.O("scope", scope, "group", a.Get("group"))}, nil
	}
}

// ---------------------------------------------------------------- rules

var firewallLogLevels = []string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug", "nolog"}

func firewallLogLevelArg(what string) *Schema {
	return Enum(firewallLogLevels...).Opt().D("Log level for " + what + " (nolog = off)")
}

// firewallRuleLine is a one-line rendering of a rule, close to the firewall config file syntax.
func firewallRuleLine(r *Obj) string {
	var parts []string
	if r.Defined("enable") && !js.Truthy(proxmox.Flag(r.Get("enable"))) {
		parts = append(parts, "[disabled]")
	}
	if r.Get("type") == "group" {
		parts = append(parts, "GROUP "+js.Str(r.Get("action")))
	} else {
		action := js.Str(or(r.Get("action"), "?"))
		if r.Truthy("macro") {
			action = js.Str(r.Get("macro")) + "(" + js.Str(r.Get("action")) + ")"
		}
		parts = append(parts, strings.ToUpper(js.Str(or(r.Get("type"), "?"))), action)
	}
	opt := func(name string, v any) {
		if !js.Nullish(v) && v != "" {
			parts = append(parts, "-"+name+" "+js.Str(v))
		}
	}
	opt("i", r.Get("iface"))
	opt("source", r.Get("source"))
	opt("dest", r.Get("dest"))
	opt("p", r.Get("proto"))
	opt("sport", r.Get("sport"))
	opt("dport", r.Get("dport"))
	opt("icmp-type", r.Get("icmp-type"))
	if r.Truthy("log") && r.Get("log") != "nolog" {
		opt("log", r.Get("log"))
	}
	line := strings.Join(parts, " ")
	if r.Truthy("comment") {
		line += " # " + js.Str(r.Get("comment"))
	}
	return line
}

func firewallSummarizeRule(r *Obj) *Obj {
	return proxmox.C(
		"pos", ifThen(r.Defined("pos"), r.Num("pos")),
		"rule", firewallRuleLine(r),
		"type", r.Get("type"),
		"action", r.Get("action"),
		"macro", r.Get("macro"),
		"proto", r.Get("proto"),
		"source", r.Get("source"),
		"dest", r.Get("dest"),
		"sport", r.Get("sport"),
		"dport", r.Get("dport"),
		"icmp_type", r.Get("icmp-type"),
		"iface", r.Get("iface"),
		"enable", ifThen(r.Defined("enable"), js.Truthy(proxmox.Flag(r.Get("enable")))),
		"log", r.Get("log"),
		"comment", r.Get("comment"),
	)
}

var firewallRuleFields = []Prop{
	P("type", Enum("in", "out", "forward", "group").Opt().D("Direction: in, out, forward (PVE 8.2+, nftables) or group (insert a security group; action = group name). Required when creating")),
	P("action", Str().Opt().D("ACCEPT, DROP or REJECT, or the security group name when type is group. Required when creating")),
	P("macro", Str().Opt().D("Predefined service macro, e.g. SSH, HTTP, HTTPS, DNS, Ping (see proxmox_list_firewall_macros). Replaces proto/dport")),
	P("proto", Str().Opt().D("IP protocol, e.g. tcp, udp, icmp, icmpv6, or a number")),
	P("source", Str().Opt().D("Source address: IP, CIDR, range, comma list, IP set (+name, +dc/name, +guest/name) or alias name (see proxmox_list_firewall_refs)")),
	P("dest", Str().Opt().D("Destination address, same syntax as source")),
	P("sport", Str().Opt().D("Source port(s): number, service name, range 1000:2000 or comma list")),
	P("dport", Str().Opt().D("Destination port(s): number, service name, range 1000:2000 or comma list")),
	P("icmp_type", Str().Opt().D("ICMP type (only with proto icmp/icmpv6), e.g. echo-request")),
	P("iface", Str().Opt().D("Interface: net0, net1... for guests; any interface name for hosts")),
	P("enable", Bool().Opt().D("Whether the rule is active (new rules are disabled unless enable=true)")),
	P("log", firewallLogLevelArg("packets matching this rule")),
	P("comment", Str().Opt().D("Comment")),
}

func firewallRuleParams(a *Obj) *Obj {
	return defined(js.O(
		"type", a.Get("type"),
		"action", a.Get("action"),
		"macro", a.Get("macro"),
		"proto", a.Get("proto"),
		"source", a.Get("source"),
		"dest", a.Get("dest"),
		"sport", a.Get("sport"),
		"dport", a.Get("dport"),
		"icmp-type", a.Get("icmp_type"),
		"iface", a.Get("iface"),
		"enable", a.Get("enable"),
		"log", a.Get("log"),
		"comment", a.Get("comment"),
	))
}

// ---------------------------------------------------------------- options

var firewallOptionKeys = map[string][]string{
	"cluster": {"enable", "policy_in", "policy_out", "policy_forward", "ebtables", "log_ratelimit"},
	"node": {
		"enable",
		"log_level_in",
		"log_level_out",
		"log_level_forward",
		"log_nf_conntrack",
		"ndp",
		"nf_conntrack_allow_invalid",
		"nf_conntrack_helpers",
		"nf_conntrack_max",
		"nf_conntrack_tcp_timeout_established",
		"nf_conntrack_tcp_timeout_syn_recv",
		"nftables",
		"nosmurfs",
		"protection_synflood",
		"protection_synflood_burst",
		"protection_synflood_rate",
		"smurf_log_level",
		"tcp_flags_log_level",
		"tcpflags",
	},
	"guest": {"enable", "policy_in", "policy_out", "dhcp", "ipfilter", "log_level_in", "log_level_out", "macfilter", "ndp", "radv"},
}

// firewallOptionDefaults are Proxmox's documented defaults, used to show the effective value of unset options.
func firewallOptionDefaults(scope string) *Obj {
	switch scope {
	case "cluster":
		return js.O("enable", false, "policy_in", "DROP", "policy_out", "ACCEPT", "policy_forward", "ACCEPT", "ebtables", true)
	case "node":
		return js.O("enable", true, "nftables", false, "ndp", true, "tcpflags", false, "nosmurfs", true, "log_level_in", "nolog", "log_level_out", "nolog")
	}
	return js.O("enable", false, "policy_in", "DROP", "policy_out", "ACCEPT", "dhcp", false, "macfilter", true, "ipfilter", false, "ndp", true, "radv", false)
}

var firewallBoolOptions = []string{
	"enable", "ebtables", "log_nf_conntrack", "ndp", "nf_conntrack_allow_invalid", "nftables", "nosmurfs",
	"protection_synflood", "tcpflags", "dhcp", "ipfilter", "macfilter", "radv",
}

func firewallEffectiveOptions(scope string, opts *Obj) *Obj {
	out := firewallOptionDefaults(scope)
	for _, k := range opts.Keys() {
		if k == "digest" {
			continue
		}
		v := opts.Get(k)
		if contains(firewallBoolOptions, k) {
			out.Set(k, js.Truthy(proxmox.Flag(v)))
		} else {
			out.Set(k, v)
		}
	}
	return out
}

// ---------------------------------------------------------------- helpers

func firewallSummarizeIpsetEntry(e *Obj) any {
	return proxmox.C("cidr", e.Get("cidr"), "nomatch", ifThen(js.Truthy(proxmox.Flag(e.Get("nomatch"))), true), "comment", e.Get("comment"))
}

var firewallNetKeyRe = regexp.MustCompile(`^net\d+$`)

func firewallNicFirewall(config *Obj) []*Obj {
	var keys []string
	for _, k := range config.Keys() {
		if firewallNetKeyRe.MatchString(k) {
			keys = append(keys, k)
		}
	}
	sort.SliceStable(keys, func(i, j int) bool { return js.Num(keys[i][3:]) < js.Num(keys[j][3:]) })
	out := []*Obj{}
	for _, k := range keys {
		p := proxmox.ParsePropertyString(config.Get(k), "")
		out = append(out, proxmox.C("nic", k, "bridge", p.Get("bridge"), "tag", p.Get("tag"), "firewall", js.Truthy(proxmox.Flag(p.Get("firewall"))), "ip", p.Get("ip"), "ip6", p.Get("ip6")))
	}
	return out
}

func firewallSortByPos(rules []*Obj) {
	sortObjs(rules, func(x, y *Obj) float64 { return x.Num("pos") - y.Num("pos") })
}

func firewallCountEnabled(rules []*Obj) int {
	return len(filterObjs(rules, func(r *Obj) bool { return js.Truthy(proxmox.Flag(r.Get("enable"))) }))
}

// firewallGetSoft is client.Get(...).catch(() => undefined).
func firewallGetSoft(ctx context.Context, client *proxmox.Client, path string) any {
	v, err := client.Get(ctx, path, nil)
	if err != nil {
		return js.Undef
	}
	return v
}

func registerFirewall(c *Context) {
	client := c.Client
	all := []string{"cluster", "node", "guest", "group"}
	optScopes := []string{"cluster", "node", "guest"}
	cg := []string{"cluster", "guest"}
	withProps := func(groups ...[]Prop) []Prop {
		var out []Prop
		for _, g := range groups {
			out = append(out, g...)
		}
		return out
	}

	// ---------------------------------------------------------------- rules

	c.Define("proxmox_list_firewall_rules", Def{
		Toolset:     "firewall",
		Title:       "List firewall rules",
		Description: "List the firewall rules of the datacenter (cluster), a node, a VM/container (guest) or a security group, in evaluation order (pos 0 first). Each rule has a readable one-line form like 'IN SSH(ACCEPT) -i net0 -source +mgmt # admins'. Rules of type group pull in a security group's rules. For 'why can't I reach VM X' start with proxmox_get_guest_firewall_status.",
		Input:       withProps(firewallScopeArgs(all...), []Prop{P("raw", rawArg)}),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			s, err := firewallResolveScope(ctx, client, a, all)
			if err != nil {
				return nil, err
			}
			rules, err := client.GetList(ctx, s.rules, nil)
			if err != nil {
				return nil, err
			}
			firewallSortByPos(rules)
			var digest any = js.Undef
			if len(rules) > 0 {
				digest = rules[0].Get("digest")
			}
			var list any
			if a.Truthy("raw") {
				list = js.Array(rules)
			} else {
				list = mapObjs(rules, func(r *Obj) any { return firewallSummarizeRule(r) })
			}
			return proxmox.Compact(js.Spread(s.target, js.O(
				"digest", digest,
				"count", len(rules),
				"enabled", firewallCountEnabled(rules),
				"rules", list,
			))), nil
		},
	})

	c.Define("proxmox_get_firewall_rule", Def{
		Toolset:     "firewall",
		Title:       "Get a firewall rule",
		Description: "Get one firewall rule by position at any level (cluster, node, guest or security group), with its digest for safe updates.",
		Input:       withProps(firewallScopeArgs(all...), []Prop{P("pos", Int().Min(0).D("Rule position (from proxmox_list_firewall_rules)"))}),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			s, err := firewallResolveScope(ctx, client, a, all)
			if err != nil {
				return nil, err
			}
			r, err := client.GetObj(ctx, s.rules+"/"+js.Str(a.Get("pos")), nil)
			if err != nil {
				return nil, err
			}
			return proxmox.Compact(js.Spread(s.target, firewallSummarizeRule(js.Spread(js.O("pos", a.Get("pos")), r)), js.O("digest", r.Get("digest")))), nil
		},
	})

	c.Define("proxmox_save_firewall_rule", Def{
		Toolset: "firewall",
		Title:   "Create, update or move a firewall rule",
		Description: "Create a firewall rule (no `pos`; type and action required) or update the rule at `pos` (only the given fields change; `delete` clears fields). Works at cluster, node, guest and security-group level. " +
			"New rules are inserted at the top (position 0) unless `insert_at` is given; rules are evaluated top-down and the first match wins. Use `moveto` with `pos` to reorder. " +
			"Rules only take effect where the firewall is enabled (see proxmox_get_guest_firewall_status / proxmox_update_firewall_options), and new rules are disabled unless enable=true.",
		Write:       true,
		Destructive: True,
		Input: withProps(firewallScopeArgs(all...), []Prop{
			P("pos", Int().Min(0).Opt().D("Position of the rule to update; omit to create a new rule")),
			P("insert_at", Int().Min(0).Opt().D("When creating: position to insert the new rule at (default 0 = top)")),
			P("moveto", Int().Min(0).Opt().D("With pos: move the rule to this position (applied after any field changes)")),
		}, firewallRuleFields, []Prop{
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("extra", extraArg),
		}),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			s, err := firewallResolveScope(ctx, client, a, all)
			if err != nil {
				return nil, err
			}
			fields, err := withExtra(js.Spread(firewallRuleParams(a), js.O("delete", deleteParam(a.Get("delete")))), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			changes := false
			for _, k := range fields.Keys() {
				if !js.IsUndef(fields.Get(k)) {
					changes = true
				}
			}
			if !a.Defined("pos") {
				if a.Defined("moveto") {
					return nil, errors.New("moveto needs pos (the rule to move); use insert_at to place a new rule")
				}
				if !a.Truthy("type") || !a.Truthy("action") {
					return nil, errors.New("type and action are required when creating a rule")
				}
				if len(a.Arr("delete")) > 0 {
					return nil, errors.New("delete only applies when updating a rule (give pos)")
				}
				if _, err := client.Post(ctx, s.rules, defined(js.Spread(fields, js.O("pos", a.Get("insert_at"), "digest", a.Get("digest"))))); err != nil {
					return nil, err
				}
				pos := or(a.Get("insert_at"), 0.0)
				created := firewallGetSoft(ctx, client, s.rules+"/"+js.Str(pos))
				var rule any = js.Undef
				if js.Truthy(created) {
					rule = firewallSummarizeRule(js.Spread(js.O("pos", pos), created))
				}
				return proxmox.Compact(js.Spread(s.target, js.O("created", true, "pos", pos, "rule", rule))), nil
			}
			if a.Defined("insert_at") {
				return nil, errors.New("insert_at only applies when creating; use moveto to move an existing rule")
			}
			if !changes && !a.Defined("moveto") {
				return nil, errors.New("Nothing to change: give rule fields, delete or moveto")
			}
			rulePath := s.rules + "/" + js.Str(a.Get("pos"))
			if changes {
				if _, err := client.Put(ctx, rulePath, defined(js.Spread(fields, js.O("digest", a.Get("digest"))))); err != nil {
					return nil, err
				}
			}
			// Proxmox ignores all other fields when moveto is present, so the move is a separate call.
			if a.Defined("moveto") {
				if _, err := client.Put(ctx, rulePath, defined(js.O("moveto", a.Get("moveto"), "digest", ifThen(!changes, a.Get("digest"))))); err != nil {
					return nil, err
				}
			}
			pos := or(a.Get("moveto"), a.Get("pos"))
			r := firewallGetSoft(ctx, client, s.rules+"/"+js.Str(pos))
			var rule any = js.Undef
			if js.Truthy(r) {
				rule = firewallSummarizeRule(js.Spread(js.O("pos", pos), r))
			}
			return proxmox.Compact(js.Spread(s.target, js.O(
				"updated", true,
				"moved", ifThen(a.Defined("moveto"), js.O("from", a.Get("pos"), "to", a.Get("moveto"))),
				"pos", pos,
				"rule", rule,
			))), nil
		},
	})

	c.Define("proxmox_delete_firewall_rule", Def{
		Toolset:     "firewall",
		Title:       "Delete a firewall rule",
		Description: "Delete the firewall rule at `pos` (cluster, node, guest or security group). Later rules move up one position, so re-list before deleting several; pass `digest` to guard against concurrent edits.",
		Delete:      true,
		Destructive: True,
		Input:       withProps(firewallScopeArgs(all...), []Prop{P("pos", Int().Min(0).D("Position of the rule to delete")), P("digest", digestArg)}),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			s, err := firewallResolveScope(ctx, client, a, all)
			if err != nil {
				return nil, err
			}
			path := s.rules + "/" + js.Str(a.Get("pos"))
			before := firewallGetSoft(ctx, client, path)
			if _, err := client.Delete(ctx, path, defined(js.O("digest", a.Get("digest")))); err != nil {
				return nil, err
			}
			var rule any = js.Undef
			if js.Truthy(before) {
				b, _ := before.(*Obj)
				rule = firewallRuleLine(b)
			}
			return proxmox.Compact(js.Spread(s.target, js.O("deleted", a.Get("pos"), "rule", rule))), nil
		},
	})

	// ---------------------------------------------------------------- options

	c.Define("proxmox_get_firewall_options", Def{
		Toolset:     "firewall",
		Title:       "Get firewall options",
		Description: "Get firewall options for the datacenter (enable, default policies, ebtables, log rate limit), a node (enable, nftables, conntrack, synflood/smurf/tcpflags protection, log levels) or a guest (enable, policies, MAC/IP filter, DHCP, NDP, RA, log levels). `effective` fills in Proxmox's defaults for unset options.",
		Input:       firewallScopeArgs(optScopes...),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			s, err := firewallResolveScope(ctx, client, a, optScopes)
			if err != nil {
				return nil, err
			}
			opts, err := client.GetObj(ctx, s.fw+"/options", nil)
			if err != nil {
				return nil, err
			}
			return proxmox.Compact(js.Spread(s.target, js.O(
				"options", proxmox.Compact(js.Spread(opts, js.O("digest", js.Undef))),
				"effective", firewallEffectiveOptions(s.scope, opts),
				"digest", opts.Get("digest"),
			))), nil
		},
	})

	c.Define("proxmox_update_firewall_options", Def{
		Toolset: "firewall",
		Title:   "Update firewall options",
		Description: "Change firewall options at datacenter (cluster), node or guest level; only the given fields change. Options differ per level: " +
			"cluster: " + strings.Join(firewallOptionKeys["cluster"], ", ") + "; node: " + strings.Join(firewallOptionKeys["node"], ", ") + "; guest: " + strings.Join(firewallOptionKeys["guest"], ", ") + ". " +
			"Careful: enabling the datacenter firewall with policy_in DROP blocks everything not allowed by rules, including the web UI/SSH from networks outside the management IP set.",
		Write:       true,
		Destructive: True,
		Input: withProps(firewallScopeArgs(optScopes...), []Prop{
			P("enable", Bool().Opt().D("Enable the firewall at this level (datacenter: the master switch for everything)")),
			P("policy_in", Enum("ACCEPT", "REJECT", "DROP").Opt().D("Default inbound policy (cluster, guest)")),
			P("policy_out", Enum("ACCEPT", "REJECT", "DROP").Opt().D("Default outbound policy (cluster, guest)")),
			P("policy_forward", Enum("ACCEPT", "DROP").Opt().D("Default forward policy (cluster; nftables)")),
			P("ebtables", Bool().Opt().D("Enable ebtables rules cluster wide (cluster)")),
			P("log_ratelimit", Str().Opt().D("Log rate limit, e.g. 'enable=1,rate=1/second,burst=5' (cluster)")),
			P("log_level_in", firewallLogLevelArg("incoming traffic (node, guest)")),
			P("log_level_out", firewallLogLevelArg("outgoing traffic (node, guest)")),
			P("log_level_forward", firewallLogLevelArg("forwarded traffic (node)")),
			P("nftables", Bool().Opt().D("Use the nftables-based proxmox-firewall (node)")),
			P("ndp", Bool().Opt().D("Allow NDP / IPv6 neighbor discovery (node, guest)")),
			P("tcpflags", Bool().Opt().D("Filter illegal TCP flag combinations (node)")),
			P("tcp_flags_log_level", firewallLogLevelArg("the illegal TCP flags filter (node)")),
			P("nosmurfs", Bool().Opt().D("Enable the SMURFS filter (node)")),
			P("smurf_log_level", firewallLogLevelArg("the SMURFS filter (node)")),
			P("log_nf_conntrack", Bool().Opt().D("Log conntrack information (node)")),
			P("nf_conntrack_allow_invalid", Bool().Opt().D("Allow invalid packets on connection tracking (node)")),
			P("nf_conntrack_helpers", Str().Opt().D("Conntrack helpers, e.g. 'ftp,tftp' (node)")),
			P("nf_conntrack_max", Int().Opt().D("Maximum tracked connections (node)")),
			P("nf_conntrack_tcp_timeout_established", Int().Opt().D("Conntrack established timeout in seconds (node)")),
			P("nf_conntrack_tcp_timeout_syn_recv", Int().Opt().D("Conntrack SYN-RECV timeout in seconds (node)")),
			P("protection_synflood", Bool().Opt().D("Enable SYN flood protection (node)")),
			P("protection_synflood_rate", Int().Opt().D("SYN flood rate limit, SYN/s per source IP (node)")),
			P("protection_synflood_burst", Int().Opt().D("SYN flood burst per source IP (node)")),
			P("dhcp", Bool().Opt().D("Allow DHCP (guest)")),
			P("macfilter", Bool().Opt().D("MAC address filter: drop traffic from MACs other than the NIC's (guest)")),
			P("ipfilter", Bool().Opt().D("IP filter: only allow the guest's own IPs (ipfilter-netX IP sets / link-local) (guest)")),
			P("radv", Bool().Opt().D("Allow the guest to send IPv6 router advertisements (guest)")),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("extra", extraArg),
		}),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			s, err := firewallResolveScope(ctx, client, a, optScopes)
			if err != nil {
				return nil, err
			}
			scope := s.scope
			var allKeys []string
			for _, ts := range []string{"cluster", "node", "guest"} {
				allKeys = append(allKeys, firewallOptionKeys[ts]...)
			}
			var given, bad []string
			for _, k := range a.Keys() {
				if contains(allKeys, k) && a.Defined(k) {
					given = append(given, k)
					if !contains(firewallOptionKeys[scope], k) {
						bad = append(bad, k)
					}
				}
			}
			if len(bad) > 0 {
				return nil, fmt.Errorf("Not available at %s level: %s (allowed: %s)", scope, strings.Join(bad, ", "), strings.Join(firewallOptionKeys[scope], ", "))
			}
			base := js.NewObj()
			for _, k := range given {
				base.Set(k, a.Get(k))
			}
			base.Set("delete", deleteParam(a.Get("delete")))
			base.Set("digest", a.Get("digest"))
			params, err := definedExtra(base, a.Get("extra"))
			if err != nil {
				return nil, err
			}
			var updated []any
			for _, k := range params.Keys() {
				if k != "digest" {
					updated = append(updated, k)
				}
			}
			if len(updated) == 0 {
				return nil, errors.New("Nothing to change")
			}
			if _, err := client.Put(ctx, s.fw+"/options", params); err != nil {
				return nil, err
			}
			opts, err := client.GetObj(ctx, s.fw+"/options", nil)
			if err != nil {
				return nil, err
			}
			return proxmox.Compact(js.Spread(s.target, js.O("updated", updated, "effective", firewallEffectiveOptions(scope, opts)))), nil
		},
	})

	// ---------------------------------------------------------------- aliases

	c.Define("proxmox_list_firewall_aliases", Def{
		Toolset:     "firewall",
		Title:       "List firewall aliases",
		Description: "List IP/network aliases defined at datacenter (cluster) or guest level. Aliases are named addresses usable in rule source/dest.",
		Input:       withProps(firewallScopeArgs(cg...), []Prop{P("search", searchArg)}),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			s, err := firewallResolveScope(ctx, client, a, cg)
			if err != nil {
				return nil, err
			}
			items, err := client.GetList(ctx, s.fw+"/aliases", nil)
			if err != nil {
				return nil, err
			}
			res := filterAndLimit(items, a.Get("search"), js.Undef, func(x *Obj) []any {
				return []any{x.Get("name"), x.Get("cidr"), x.Get("comment")}
			})
			return js.Spread(s.target, res.Map(func(x *Obj) any {
				return proxmox.C("name", x.Get("name"), "cidr", x.Get("cidr"), "comment", x.Get("comment"))
			})), nil
		},
	})

	c.Define("proxmox_save_firewall_alias", Def{
		Toolset:     "firewall",
		Title:       "Create or update a firewall alias",
		Description: "Create an IP/network alias, or update it if it already exists (cidr/comment; `new_name` renames it). Datacenter (cluster) or guest level. Rules referencing a renamed alias are not rewritten.",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input: withProps(firewallScopeArgs(cg...), []Prop{
			P("name", Str().D("Alias name")),
			P("cidr", Str().Opt().D("IP address or network in CIDR form, e.g. 10.0.0.5 or 10.0.0.0/24 (required when creating)")),
			P("comment", Str().Opt().D("Comment")),
			P("new_name", Str().Opt().D("Rename the existing alias to this")),
			P("digest", digestArg),
		}),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			s, err := firewallResolveScope(ctx, client, a, cg)
			if err != nil {
				return nil, err
			}
			list, err := client.GetList(ctx, s.fw+"/aliases", nil)
			if err != nil {
				return nil, err
			}
			var existing *Obj
			for _, x := range list {
				if x.Get("name") == a.Get("name") {
					existing = x
					break
				}
			}
			if existing == nil {
				if !a.Truthy("cidr") {
					return nil, fmt.Errorf("Alias %s does not exist; cidr is required to create it", js.Str(a.Get("name")))
				}
				if a.Truthy("new_name") {
					return nil, fmt.Errorf("Alias %s does not exist, so it can't be renamed", js.Str(a.Get("name")))
				}
				if _, err := client.Post(ctx, s.fw+"/aliases", defined(js.O("name", a.Get("name"), "cidr", a.Get("cidr"), "comment", a.Get("comment")))); err != nil {
					return nil, err
				}
				return js.Spread(s.target, js.O("created", a.Get("name"), "cidr", a.Get("cidr"))), nil
			}
			// PUT requires cidr even when only the comment or name changes.
			cidr := or(a.Get("cidr"), existing.Get("cidr"))
			if _, err := client.Put(ctx, s.fw+"/aliases/"+enc(a.Get("name")), defined(js.O("cidr", cidr, "comment", a.Get("comment"), "rename", a.Get("new_name"), "digest", a.Get("digest")))); err != nil {
				return nil, err
			}
			return proxmox.Compact(js.Spread(s.target, js.O(
				"updated", or(a.Get("new_name"), a.Get("name")),
				"renamed_from", ifThen(a.Truthy("new_name"), a.Get("name")),
				"cidr", cidr,
			))), nil
		},
	})

	c.Define("proxmox_delete_firewall_alias", Def{
		Toolset:     "firewall",
		Title:       "Delete a firewall alias",
		Description: "Delete an IP/network alias at datacenter (cluster) or guest level. Fails while rules still reference it.",
		Delete:      true,
		Input:       withProps(firewallScopeArgs(cg...), []Prop{P("name", Str().D("Alias name")), P("digest", digestArg)}),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			s, err := firewallResolveScope(ctx, client, a, cg)
			if err != nil {
				return nil, err
			}
			if _, err := client.Delete(ctx, s.fw+"/aliases/"+enc(a.Get("name")), defined(js.O("digest", a.Get("digest")))); err != nil {
				return nil, err
			}
			return js.Spread(s.target, js.O("deleted", a.Get("name"))), nil
		},
	})

	// ---------------------------------------------------------------- IP sets

	c.Define("proxmox_list_firewall_ipsets", Def{
		Toolset:     "firewall",
		Title:       "List firewall IP sets",
		Description: "List IP sets at datacenter (cluster) or guest level with their entries (CIDRs, nomatch exclusions, comments). Reference them in rules as +name (or +dc/name, +guest/name). Special sets: 'management' (hosts allowed to reach the web UI/SSH when the firewall is on), 'blacklist', and per-NIC 'ipfilter-netX' on guests.",
		Input:       withProps(firewallScopeArgs(cg...), []Prop{P("name", Str().Opt().D("Only this IP set")), P("search", searchArg)}),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			s, err := firewallResolveScope(ctx, client, a, cg)
			if err != nil {
				return nil, err
			}
			sets, err := client.GetList(ctx, s.fw+"/ipset", nil)
			if err != nil {
				return nil, err
			}
			if a.Truthy("name") {
				sets = filterObjs(sets, func(x *Obj) bool { return x.Get("name") == a.Get("name") })
				if len(sets) == 0 {
					return nil, fmt.Errorf("No IP set named %s", js.Str(a.Get("name")))
				}
			}
			res := filterAndLimit(sets, a.Get("search"), js.Undef, func(x *Obj) []any { return []any{x.Get("name"), x.Get("comment")} })
			items := []any{}
			for _, set := range res.Items {
				entries, err := client.GetList(ctx, s.fw+"/ipset/"+enc(set.Get("name")), nil)
				if err != nil {
					return nil, err
				}
				var digest any = js.Undef
				if len(entries) > 0 {
					digest = entries[0].Get("digest")
				}
				items = append(items, proxmox.C("name", set.Get("name"), "comment", set.Get("comment"), "count", len(entries), "entries", mapObjs(entries, firewallSummarizeIpsetEntry), "digest", digest))
			}
			return js.Spread(s.target, res.Obj(items)), nil
		},
	})

	c.Define("proxmox_save_firewall_ipset", Def{
		Toolset:     "firewall",
		Title:       "Create or update a firewall IP set",
		Description: "Create an IP set, or update an existing one's comment / rename it (`new_name`). Datacenter (cluster) or guest level. Add entries with proxmox_save_ipset_entry.",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input: withProps(firewallScopeArgs(cg...), []Prop{
			P("name", Str().D("IP set name")),
			P("comment", Str().Opt().D("Comment")),
			P("new_name", Str().Opt().D("Rename the existing IP set to this")),
			P("digest", digestArg),
		}),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			s, err := firewallResolveScope(ctx, client, a, cg)
			if err != nil {
				return nil, err
			}
			sets, err := client.GetList(ctx, s.fw+"/ipset", nil)
			if err != nil {
				return nil, err
			}
			exists := len(filterObjs(sets, func(x *Obj) bool { return x.Get("name") == a.Get("name") })) > 0
			if !exists {
				if a.Truthy("new_name") {
					return nil, fmt.Errorf("IP set %s does not exist, so it can't be renamed", js.Str(a.Get("name")))
				}
				if _, err := client.Post(ctx, s.fw+"/ipset", defined(js.O("name", a.Get("name"), "comment", a.Get("comment")))); err != nil {
					return nil, err
				}
				return js.Spread(s.target, js.O("created", a.Get("name"))), nil
			}
			// Proxmox's convention: `rename` is the existing name and `name` the new one.
			if _, err := client.Post(ctx, s.fw+"/ipset", defined(js.O("name", or(a.Get("new_name"), a.Get("name")), "rename", a.Get("name"), "comment", a.Get("comment"), "digest", a.Get("digest")))); err != nil {
				return nil, err
			}
			return proxmox.Compact(js.Spread(s.target, js.O("updated", or(a.Get("new_name"), a.Get("name")), "renamed_from", ifThen(a.Truthy("new_name"), a.Get("name"))))), nil
		},
	})

	c.Define("proxmox_delete_firewall_ipset", Def{
		Toolset:     "firewall",
		Title:       "Delete a firewall IP set",
		Description: "Delete an IP set at datacenter (cluster) or guest level. Fails if it still has entries unless force=true.",
		Delete:      true,
		Input:       withProps(firewallScopeArgs(cg...), []Prop{P("name", Str().D("IP set name")), P("force", Bool().Opt().D("Also delete all its entries"))}),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			s, err := firewallResolveScope(ctx, client, a, cg)
			if err != nil {
				return nil, err
			}
			if _, err := client.Delete(ctx, s.fw+"/ipset/"+enc(a.Get("name")), defined(js.O("force", a.Get("force")))); err != nil {
				return nil, err
			}
			return js.Spread(s.target, js.O("deleted", a.Get("name"))), nil
		},
	})

	c.Define("proxmox_save_ipset_entry", Def{
		Toolset:     "firewall",
		Title:       "Add or update an IP set entry",
		Description: "Add an IP/network to an IP set, or update an existing entry's comment/nomatch flag. nomatch=true makes the entry an exclusion. Datacenter (cluster) or guest level.",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input: withProps(firewallScopeArgs(cg...), []Prop{
			P("ipset", Str().D("IP set name")),
			P("cidr", Str().D("IP, network in CIDR form, or alias name, e.g. 10.0.0.0/24")),
			P("comment", Str().Opt().D("Comment")),
			P("nomatch", Bool().Opt().D("Exclude this address from the set instead of including it")),
			P("digest", digestArg),
		}),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			s, err := firewallResolveScope(ctx, client, a, cg)
			if err != nil {
				return nil, err
			}
			path := s.fw + "/ipset/" + enc(a.Get("ipset"))
			entries, err := client.GetList(ctx, path, nil)
			if err != nil {
				return nil, err
			}
			exists := len(filterObjs(entries, func(e *Obj) bool { return e.Get("cidr") == a.Get("cidr") })) > 0
			if exists {
				_, err = client.Put(ctx, path+"/"+enc(a.Get("cidr")), defined(js.O("comment", a.Get("comment"), "nomatch", a.Get("nomatch"), "digest", a.Get("digest"))))
			} else {
				_, err = client.Post(ctx, path, defined(js.O("cidr", a.Get("cidr"), "comment", a.Get("comment"), "nomatch", a.Get("nomatch"))))
			}
			if err != nil {
				return nil, err
			}
			return js.Spread(s.target, js.O("ipset", a.Get("ipset"), js.Str(pick(exists, "updated", "added")), a.Get("cidr"))), nil
		},
	})

	c.Define("proxmox_delete_ipset_entry", Def{
		Toolset:     "firewall",
		Title:       "Remove an IP set entry",
		Description: "Remove an IP/network from an IP set at datacenter (cluster) or guest level.",
		Delete:      true,
		Input: withProps(firewallScopeArgs(cg...), []Prop{
			P("ipset", Str().D("IP set name")),
			P("cidr", Str().D("The entry to remove, exactly as listed, e.g. 10.0.0.0/24")),
			P("digest", digestArg),
		}),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			s, err := firewallResolveScope(ctx, client, a, cg)
			if err != nil {
				return nil, err
			}
			if _, err := client.Delete(ctx, s.fw+"/ipset/"+enc(a.Get("ipset"))+"/"+enc(a.Get("cidr")), defined(js.O("digest", a.Get("digest")))); err != nil {
				return nil, err
			}
			return js.Spread(s.target, js.O("ipset", a.Get("ipset"), "removed", a.Get("cidr"))), nil
		},
	})

	// ---------------------------------------------------------------- security groups

	c.Define("proxmox_list_firewall_groups", Def{
		Toolset:     "firewall",
		Title:       "List firewall security groups",
		Description: "List the datacenter's security groups (reusable rule sets) with their comment and rule count. See a group's rules with proxmox_list_firewall_rules scope=group; apply a group to a guest/node with a rule of type group.",
		Input:       []Prop{P("search", searchArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			groups, err := client.GetList(ctx, "/cluster/firewall/groups", nil)
			if err != nil {
				return nil, err
			}
			res := filterAndLimit(groups, a.Get("search"), js.Undef, func(g *Obj) []any { return []any{g.Get("group"), g.Get("comment")} })
			items := []any{}
			for _, g := range res.Items {
				v, err := client.Get(ctx, "/cluster/firewall/groups/"+enc(g.Get("group")), nil)
				var count, enabled any = js.Undef, js.Undef
				if err == nil {
					if arr, ok := v.([]any); ok {
						rules := js.Objs(arr)
						count, enabled = len(rules), firewallCountEnabled(rules)
					}
				}
				items = append(items, proxmox.C("group", g.Get("group"), "comment", g.Get("comment"), "rules", count, "enabled_rules", enabled))
			}
			return res.Obj(items), nil
		},
	})

	c.Define("proxmox_save_firewall_group", Def{
		Toolset:     "firewall",
		Title:       "Create or update a security group",
		Description: "Create a datacenter security group, or update an existing one's comment / rename it (`new_name`). Add rules to it with proxmox_save_firewall_rule scope=group.",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input: []Prop{
			P("group", Str().D("Security group name (2-18 chars, letters, digits, - and _)")),
			P("comment", Str().Opt().D("Comment")),
			P("new_name", Str().Opt().D("Rename the existing group to this")),
			P("digest", digestArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			groups, err := client.GetList(ctx, "/cluster/firewall/groups", nil)
			if err != nil {
				return nil, err
			}
			exists := len(filterObjs(groups, func(g *Obj) bool { return g.Get("group") == a.Get("group") })) > 0
			if !exists {
				if a.Truthy("new_name") {
					return nil, fmt.Errorf("Security group %s does not exist, so it can't be renamed", js.Str(a.Get("group")))
				}
				if _, err := client.Post(ctx, "/cluster/firewall/groups", defined(js.O("group", a.Get("group"), "comment", a.Get("comment")))); err != nil {
					return nil, err
				}
				return js.O("created", a.Get("group")), nil
			}
			// As for IP sets, `rename` is the current name and `group` the new one.
			if _, err := client.Post(ctx, "/cluster/firewall/groups", defined(js.O("group", or(a.Get("new_name"), a.Get("group")), "rename", a.Get("group"), "comment", a.Get("comment"), "digest", a.Get("digest")))); err != nil {
				return nil, err
			}
			return proxmox.C("updated", or(a.Get("new_name"), a.Get("group")), "renamed_from", ifThen(a.Truthy("new_name"), a.Get("group"))), nil
		},
	})

	c.Define("proxmox_delete_firewall_group", Def{
		Toolset:     "firewall",
		Title:       "Delete a security group",
		Description: "Delete a datacenter security group. Proxmox refuses while the group still has rules or is referenced by rules; remove those first.",
		Delete:      true,
		Input:       []Prop{P("group", Str().D("Security group name"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/cluster/firewall/groups/"+enc(a.Get("group")), nil); err != nil {
				return nil, err
			}
			return js.O("deleted", a.Get("group")), nil
		},
	})

	// ---------------------------------------------------------------- log, refs, macros

	logScopes := []string{"node", "guest"}
	c.Define("proxmox_get_firewall_log", Def{
		Toolset:     "firewall",
		Title:       "Read the firewall log",
		Description: "Read the firewall log of a node (all its traffic, including guests on it) or of one guest. Only packets matching rules/policies with a log level other than nolog are logged; set log_level_in on the options or `log` on a rule to see drops.",
		Input: withProps(firewallScopeArgs(logScopes...), []Prop{
			P("limit", limitArg(100)),
			P("start", Int().Min(0).Opt().D("Skip this many lines (paging)")),
			P("since", Int().Opt().D("Only entries since this UNIX time")),
			P("until", Int().Opt().D("Only entries until this UNIX time")),
			P("search", searchArg),
		}),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			s, err := firewallResolveScope(ctx, client, a, logScopes)
			if err != nil {
				return nil, err
			}
			lines, err := client.GetList(ctx, s.fw+"/log", defined(js.O("limit", or(a.Get("limit"), 100), "start", a.Get("start"), "since", a.Get("since"), "until", a.Get("until"))))
			if err != nil {
				return nil, err
			}
			res := filterAndLimit(lines, a.Get("search"), js.Undef, func(l *Obj) []any { return []any{l.Get("t")} })
			return js.Spread(s.target, res.Map(func(l *Obj) any { return l.Get("t") })), nil
		},
	})

	c.Define("proxmox_list_firewall_refs", Def{
		Toolset:     "firewall",
		Title:       "List firewall alias/IP set references",
		Description: "List the aliases and IP sets that can be used in rule source/dest at datacenter (cluster) or guest level, with the exact reference string to use (e.g. +dc/management, dc/myalias).",
		Input:       withProps(firewallScopeArgs(cg...), []Prop{P("type", Enum("alias", "ipset").Opt().D("Only aliases or only IP sets"))}),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			s, err := firewallResolveScope(ctx, client, a, cg)
			if err != nil {
				return nil, err
			}
			refs, err := client.GetList(ctx, s.fw+"/refs", defined(js.O("type", a.Get("type"))))
			if err != nil {
				return nil, err
			}
			return js.Spread(s.target, js.O("items", mapObjs(refs, func(r *Obj) any {
				return proxmox.C("ref", r.Get("ref"), "type", r.Get("type"), "name", r.Get("name"), "scope", r.Get("scope"), "comment", r.Get("comment"))
			}))), nil
		},
	})

	c.Define("proxmox_list_firewall_macros", Def{
		Toolset:     "firewall",
		Title:       "List firewall macros",
		Description: "List the predefined firewall macros (SSH, HTTP, DNS, Ceph, ...) usable as a rule's `macro` instead of proto/port.",
		Input:       []Prop{P("search", searchArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			macros, err := client.GetList(ctx, "/cluster/firewall/macros", nil)
			if err != nil {
				return nil, err
			}
			res := filterAndLimit(macros, a.Get("search"), js.Undef, func(m *Obj) []any { return []any{m.Get("macro"), m.Get("descr")} })
			return res.Map(func(m *Obj) any { return proxmox.C("macro", m.Get("macro"), "description", m.Get("descr")) }), nil
		},
	})

	// ---------------------------------------------------------------- whole picture

	c.Define("proxmox_get_guest_firewall_status", Def{
		Toolset:     "firewall",
		Title:       "Get a guest's firewall status",
		Description: "Whole-picture firewall view for one VM/container, for questions like 'why can't I reach VM X': whether the firewall is enabled at datacenter, node and guest level and on each NIC (firewall=1), the effective policies, the guest's rules (one line each), IP/MAC filters, and plain-language findings about what is blocking or not filtering traffic.",
		Input:       []Prop{P("vmid", vmidArg), P("node", Str().Opt().D("Node the guest is on. Looked up automatically from the VMID when omitted."))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := client.ResolveGuest(ctx, a.Num("vmid"), a.Str("node"), "")
			if err != nil {
				return nil, err
			}
			base := client.GuestPath(g)
			soft := func(path string) any {
				v, err := client.Get(ctx, path, nil)
				if err != nil {
					return js.O("error", errMessage(err))
				}
				return v
			}
			dcV := soft("/cluster/firewall/options")
			hostV := soft("/nodes/" + enc(g.Node) + "/firewall/options")
			guestV := soft(base + "/firewall/options")
			config, err := client.GetObj(ctx, base+"/config", nil)
			if err != nil {
				return nil, err
			}
			rules := soft(base + "/firewall/rules")
			asObj := func(v any) *Obj {
				if o, ok := v.(*Obj); ok {
					return o
				}
				return js.NewObj()
			}
			dc, host, guest := asObj(dcV), asObj(hostV), asObj(guestV)
			okObj := func(v any) bool {
				o, _ := v.(*Obj)
				return !o.Truthy("error")
			}
			var dcEff, hostEff, guestEff *Obj
			if okObj(dcV) {
				dcEff = firewallEffectiveOptions("cluster", dc)
			}
			if okObj(hostV) {
				hostEff = firewallEffectiveOptions("node", host)
			}
			if okObj(guestV) {
				guestEff = firewallEffectiveOptions("guest", guest)
			}
			nics := firewallNicFirewall(config)
			rulesArr, rulesIsArr := rules.([]any)
			ruleList := []*Obj{}
			if rulesIsArr {
				ruleList = js.Objs(rulesArr)
				firewallSortByPos(ruleList)
			}
			active := filterObjs(ruleList, func(r *Obj) bool { return js.Truthy(proxmox.Flag(r.Get("enable"))) })

			findings := []any{}
			add := func(s string) { findings = append(findings, s) }
			if dcEff != nil && !dcEff.Truthy("enable") {
				add("Datacenter firewall is disabled: no firewall rules are applied anywhere, so the Proxmox firewall is not blocking this guest.")
			}
			if guestEff != nil && !guestEff.Truthy("enable") {
				add("Guest firewall is disabled: the guest's rules and policies are not applied.")
			}
			var unfiltered []string
			anyFiltered := false
			for _, n := range nics {
				if n.Truthy("firewall") {
					anyFiltered = true
				} else {
					unfiltered = append(unfiltered, js.Str(n.Get("nic")))
				}
			}
			if len(unfiltered) > 0 {
				add("NICs without firewall=1 bypass the guest firewall: " + strings.Join(unfiltered, ", ") + ".")
			}
			filtering := dcEff.Truthy("enable") && guestEff.Truthy("enable") && anyFiltered
			if filtering && guestEff != nil {
				inAccept := len(filterObjs(active, func(r *Obj) bool { return r.Get("type") == "in" && r.Get("action") == "ACCEPT" }))
				groups := len(filterObjs(active, func(r *Obj) bool { return r.Get("type") == "group" }))
				policyIn := js.Str(guestEff.Get("policy_in"))
				if guestEff.Get("policy_in") != "ACCEPT" && inAccept == 0 && groups == 0 {
					add("Inbound policy is " + policyIn + " and no enabled inbound ACCEPT rules or security groups exist: all incoming traffic is blocked.")
				} else if guestEff.Get("policy_in") != "ACCEPT" {
					add("Inbound policy is " + policyIn + ": only traffic matching an enabled ACCEPT rule (or security group) gets in.")
				}
				if guestEff.Get("policy_out") != "ACCEPT" {
					add("Outbound policy is " + js.Str(guestEff.Get("policy_out")) + ": outgoing traffic needs explicit out rules.")
				}
				if guestEff.Truthy("ipfilter") {
					add("IP filter is on: traffic from addresses other than the guest's configured IPs (ipfilter-netX IP sets) is dropped.")
				}
				if disabled := len(ruleList) - len(active); disabled != 0 {
					add(fmt.Sprintf("%d rule(s) are disabled.", disabled))
				}
				if guestEff.Get("log_level_in") == "nolog" || !guestEff.Truthy("log_level_in") {
					add("Inbound drops are not logged; set log_level_in (proxmox_update_firewall_options) and read proxmox_get_firewall_log to see them.")
				}
			}
			if hostEff != nil && !hostEff.Truthy("enable") {
				add("The node's host firewall is disabled; this affects only traffic to the host itself, not guest rules.")
			}

			var datacenter, nodeFw, guestFw, rulesOut any
			if dcEff != nil {
				datacenter = proxmox.C("enabled", dcEff.Get("enable"), "policy_in", dcEff.Get("policy_in"), "policy_out", dcEff.Get("policy_out"), "policy_forward", dcEff.Get("policy_forward"))
			} else {
				datacenter = js.O("error", dc.Get("error"))
			}
			if hostEff != nil {
				nodeFw = proxmox.C("enabled", hostEff.Get("enable"), "nftables", hostEff.Get("nftables"))
			} else {
				nodeFw = js.O("error", host.Get("error"))
			}
			if guestEff != nil {
				guestFw = proxmox.C(
					"enabled", guestEff.Get("enable"),
					"policy_in", guestEff.Get("policy_in"),
					"policy_out", guestEff.Get("policy_out"),
					"macfilter", guestEff.Get("macfilter"),
					"ipfilter", guestEff.Get("ipfilter"),
					"dhcp", guestEff.Get("dhcp"),
					"ndp", guestEff.Get("ndp"),
					"radv", guestEff.Get("radv"),
					"log_level_in", guestEff.Get("log_level_in"),
					"log_level_out", guestEff.Get("log_level_out"),
				)
			} else {
				guestFw = js.O("error", guest.Get("error"))
			}
			if rulesIsArr {
				rulesOut = js.O("count", len(ruleList), "enabled", len(active), "list", mapObjs(ruleList, func(r *Obj) any {
					return js.Str(r.Get("pos")) + ": " + firewallRuleLine(r)
				}))
			} else {
				rulesOut = js.O("error", asObj(rules).Get("error"))
			}
			return proxmox.C(
				"vmid", g.VMID,
				"name", g.Name,
				"node", g.Node,
				"type", pick(g.Type == "qemu", "vm", "container"),
				"filtering_active", filtering,
				"datacenter", datacenter,
				"node_firewall", nodeFw,
				"guest_firewall", guestFw,
				"nics", js.Array(nics),
				"rules", rulesOut,
				"findings", findings,
			), nil
		},
	})
}
