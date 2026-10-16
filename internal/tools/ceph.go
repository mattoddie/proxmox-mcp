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

var cephFlags = []string{"nobackfill", "nodeep-scrub", "nodown", "noin", "noout", "norebalance", "norecover", "noscrub", "notieragent", "noup", "pause"}

var cephFlagHelp = map[string]string{
	"nobackfill":   "suspend PG backfilling",
	"nodeep-scrub": "disable deep scrubbing",
	"nodown":       "ignore OSD failure reports (OSDs won't be marked down)",
	"noin":         "don't mark restarted OSDs back in",
	"noout":        "don't mark down OSDs out (use during maintenance)",
	"norebalance":  "suspend PG rebalancing",
	"norecover":    "suspend PG recovery",
	"noscrub":      "disable scrubbing",
	"notieragent":  "suspend cache tiering",
	"noup":         "don't allow OSDs to start",
	"pause":        "pause ALL client reads and writes",
}

// cephFlagArgs has one optional boolean argument per flag (the first dash becomes an underscore).
func cephFlagArgs() []Prop {
	out := make([]Prop, len(cephFlags))
	for i, f := range cephFlags {
		out[i] = P(strings.Replace(f, "-", "_", 1), Bool().Opt().D("true sets, false clears: "+cephFlagHelp[f]))
	}
	return out
}

var cephServiceTypes = []string{"mon", "mgr", "mds"}

var (
	cephNodeArg   = Str().Opt().D("Node to query through (any node with Ceph installed; default PROXMOX_DEFAULT_NODE or the first online node). Results are cluster-wide.")
	cephTargetArg = Str().D("Node to act on (where the daemon/disk is)")
	cephOsdIDArg  = Int().Min(0).D("OSD ID number, e.g. 3 for osd.3")
)

// cephNode picks the node for cluster-wide Ceph reads, which work through any node: unlike
// ResolveNode it doesn't insist on a single-node cluster, it just picks the first online node.
func cephNode(ctx context.Context, c *Context, node string) (string, error) {
	if node != "" {
		return node, nil
	}
	if d := c.Client.DefaultNode(); d != "" {
		return d, nil
	}
	nodes, err := c.Client.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	var online []string
	for _, n := range nodes {
		if n.Get("status") == "online" {
			online = append(online, js.Str(n.Get("node")))
		}
	}
	sort.Strings(online)
	if len(online) == 0 {
		return "", errors.New("No online node found to query Ceph through; pass `node`")
	}
	return online[0], nil
}

func cephBase(node string) string { return "/nodes/" + enc(node) + "/ceph" }

// cephProp is JavaScript property access on any value: objects give the property, anything
// else undefined.
func cephProp(v any, k string) any {
	if o, ok := v.(*Obj); ok {
		return o.Get(k)
	}
	return js.Undef
}

// cephObj is `v ?? {}` for a value used as an object.
func cephObj(v any) *Obj {
	if o, ok := v.(*Obj); ok && o != nil {
		return o
	}
	return js.NewObj()
}

// ---------------------------------------------------------------- status

func cephSummarizeStatus(s *Obj) *Obj {
	health := cephObj(or(s.Get("health"), js.NewObj()))
	checksObj := cephObj(or(health.Get("checks"), js.NewObj()))
	checks := []any{}
	for _, code := range checksObj.Keys() {
		cv := checksObj.Get(code)
		summary := cephProp(cv, "summary")
		var detail any = js.Undef
		if arr, ok := cephProp(cv, "detail").([]any); ok {
			if len(arr) > 5 {
				arr = arr[:5]
			}
			d := make([]any, len(arr))
			for i, x := range arr {
				d[i] = or(cephProp(x, "message"), x)
			}
			detail = d
		}
		checks = append(checks, proxmox.C(
			"code", code,
			"severity", cephProp(cv, "severity"),
			"summary", or(cephProp(summary, "message"), summary),
			"count", cephProp(summary, "count"),
			"muted", ifThen(js.Truthy(cephProp(cv, "muted")), true),
			"detail", detail,
		))
	}
	// The osdmap was nested one level deeper (osdmap.osdmap) before Ceph Quincy.
	osd := cephObj(or(cephProp(s.Get("osdmap"), "osdmap"), or(s.Get("osdmap"), js.NewObj())))
	pg := cephObj(or(s.Get("pgmap"), js.NewObj()))
	mons := js.Objs(or(cephProp(s.Get("monmap"), "mons"), []any{}))
	mgr := cephObj(or(s.Get("mgrmap"), js.NewObj()))
	var standbys any = js.Undef
	if arr, ok := mgr.Get("standbys").([]any); ok {
		standbys = mapObjs(js.Objs(arr), func(x *Obj) any { return x.Get("name") })
	}
	fs := cephObj(or(s.Get("fsmap"), js.NewObj()))

	quorum := s.Get("quorum_names")
	includes := func(n any) bool {
		switch q := quorum.(type) {
		case []any:
			for _, x := range q {
				if jsEqualStrict(x, n) {
					return true
				}
			}
		case string:
			return strings.Contains(q, js.Str(n))
		}
		return false
	}
	outOfQuorum := []any{}
	for _, m := range mons {
		n := m.Get("name")
		if js.Truthy(quorum) && !includes(n) {
			outOfQuorum = append(outOfQuorum, n)
		}
	}
	var monTotal any = float64(len(mons))
	if len(mons) == 0 {
		monTotal = cephProp(s.Get("monmap"), "num_mons")
	}

	var states any = js.Undef
	if arr, ok := pg.Get("pgs_by_state").([]any); ok {
		st := js.NewObj()
		for _, p := range js.Objs(arr) {
			st.Set(js.Str(p.Get("state_name")), p.Get("count"))
		}
		states = st
	}
	perSec := func(k string) any {
		if pg.Truthy(k) {
			return js.Str(proxmox.FormatBytes(pg.Get(k))) + "/s"
		}
		return js.Undef
	}
	pct := func(k string) any {
		return ifThen(pg.Defined(k), js.Round(pg.Num(k)*1000)/10)
	}
	var cephfs any = js.Undef
	if byRank, ok := fs.Get("by_rank").([]any); ok && len(byRank) > 0 {
		cephfs = proxmox.C(
			"epoch", fs.Get("epoch"),
			"mds", mapObjs(js.Objs(byRank), func(r *Obj) any { return js.Str(r.Get("name")) + ": " + js.Str(r.Get("status")) }),
			"standby", fs.Get("up:standby"),
		)
	}
	return proxmox.C(
		"health", health.Get("status"),
		"checks", checks,
		"fsid", s.Get("fsid"),
		"monitors", proxmox.C(
			"total", or(orFalsy(monTotal), cephProp(s.Get("monmap"), "num_mons")),
			"in_quorum", s.Get("quorum_names"),
			"out_of_quorum", outOfQuorum,
		),
		"managers", proxmox.C(
			"active", mgr.Get("active_name"),
			"available", ifThen(mgr.Defined("available"), mgr.Truthy("available")),
			"standbys", or(standbys, mgr.Get("num_standbys")),
		),
		"osds", proxmox.C(
			"total", osd.Get("num_osds"),
			"up", osd.Get("num_up_osds"),
			"in", osd.Get("num_in_osds"),
			"down", ifThen(osd.Defined("num_osds") && osd.Defined("num_up_osds"), osd.Num("num_osds")-osd.Num("num_up_osds")),
			"remapped_pgs", orUndef(osd.Get("num_remapped_pgs")),
		),
		"pgs", proxmox.C(
			"total", pg.Get("num_pgs"),
			"states", states,
			"degraded_pct", pct("degraded_ratio"),
			"misplaced_pct", pct("misplaced_ratio"),
			"recovering_per_sec", perSec("recovering_bytes_per_sec"),
		),
		"usage", proxmox.C(
			"raw", proxmox.Usage(pg.Get("bytes_used"), pg.Get("bytes_total")),
			"available", proxmox.FormatBytes(pg.Get("bytes_avail")),
			"data_stored", proxmox.FormatBytes(pg.Get("data_bytes")),
			"pools", pg.Get("num_pools"),
			"objects", pg.Get("num_objects"),
		),
		"io", proxmox.C(
			"read", perSec("read_bytes_sec"),
			"write", perSec("write_bytes_sec"),
			"read_ops", pg.Get("read_op_per_sec"),
			"write_ops", pg.Get("write_op_per_sec"),
		),
		"cephfs", cephfs,
	)
}

// orFalsy is `v || undefined` for the left side of a further `||`.
func orFalsy(v any) any { return orUndef(v) }

// jsEqualStrict is ===  for JSON values.
func jsEqualStrict(a, b any) bool {
	switch x := a.(type) {
	case *Obj, []any:
		return false
	case float64:
		y, ok := b.(float64)
		return ok && x == y
	}
	return a == b
}

// ---------------------------------------------------------------- OSD tree

// cephWalkTree flattens the OSD tree into OSD leaves (with their host) and host buckets.
func cephWalkTree(root any) (osds []*Obj, hosts []*Obj) {
	var visit func(n any, host any)
	visit = func(nv any, host any) {
		n, ok := nv.(*Obj)
		if !ok || n == nil {
			return
		}
		if n.Get("type") == "osd" {
			osds = append(osds, js.Spread(n, js.O("host", or(n.Get("host"), host))))
			return
		}
		h := host
		if n.Get("type") == "host" {
			h = n.Get("name")
			hosts = append(hosts, n)
		}
		for _, ch := range n.Arr("children") {
			visit(ch, h)
		}
	}
	visit(root, js.Undef)
	return osds, hosts
}

func cephSummarizeOsd(o *Obj) any {
	var latency any = js.Undef
	if o.Defined("apply_latency_ms") || o.Defined("commit_latency_ms") {
		latency = js.O("apply", o.Get("apply_latency_ms"), "commit", o.Get("commit_latency_ms"))
	}
	return proxmox.C(
		"id", o.Get("id"),
		"name", o.Get("name"),
		"host", o.Get("host"),
		"status", o.Get("status"),
		"in", ifThen(o.Defined("in"), js.Truthy(o.Num("in"))),
		"device_class", o.Get("device_class"),
		"usage", proxmox.Usage(o.Get("bytes_used"), o.Get("total_space")),
		"used_pct", ifThen(o.Defined("percent_used"), js.Round(o.Num("percent_used")*10)/10),
		"pgs", o.Get("pgs"),
		"crush_weight", o.Get("crush_weight"),
		"reweight", o.Get("reweight"),
		"latency_ms", latency,
		"type", o.Get("osdtype"),
		"version", o.Get("version"),
		"db_dev", ifThen(o.Truthy("dbdev"), true),
		"wal_dev", ifThen(o.Truthy("waldev"), true),
	)
}

type cephTree struct {
	flags any
	osds  []*Obj
	hosts []*Obj
}

func cephOsdTree(ctx context.Context, c *Context, node string) (cephTree, error) {
	v, err := c.Client.Get(ctx, cephBase(node)+"/osd", nil)
	if err != nil {
		return cephTree{}, err
	}
	osds, hosts := cephWalkTree(or(cephProp(v, "root"), js.NewObj()))
	return cephTree{flags: cephProp(v, "flags"), osds: osds, hosts: hosts}, nil
}

// cephOsdHost finds the node hosting an OSD from the tree when not given: OSD actions must go there.
func cephOsdHost(ctx context.Context, c *Context, osdid float64, node string) (string, error) {
	if node != "" {
		return node, nil
	}
	qn, err := cephNode(ctx, c, "")
	if err != nil {
		return "", err
	}
	tree, err := cephOsdTree(ctx, c, qn)
	if err != nil {
		return "", err
	}
	var osd *Obj
	for _, o := range tree.osds {
		if o.Num("id") == osdid {
			osd = o
			break
		}
	}
	if osd == nil {
		return "", fmt.Errorf("OSD %s not found in the Ceph OSD tree (see proxmox_list_ceph_osds)", js.NumStr(osdid))
	}
	if !osd.Truthy("host") {
		return "", fmt.Errorf("Could not determine which node hosts osd.%s; pass `node`", js.NumStr(osdid))
	}
	return js.Str(osd.Get("host")), nil
}

// ---------------------------------------------------------------- pools

func cephSummarizePool(p *Obj) any {
	var application any = js.Undef
	if p.Truthy("application_metadata") {
		switch am := p.Get("application_metadata").(type) {
		case *Obj:
			application = strings.Join(am.Keys(), ",")
		case []any:
			idx := make([]string, len(am))
			for i := range am {
				idx[i] = fmt.Sprint(i)
			}
			application = strings.Join(idx, ",")
		default:
			application = ""
		}
	}
	return proxmox.C(
		"name", p.Get("pool_name"),
		"id", p.Get("pool"),
		"type", p.Get("type"),
		"size", p.Get("size"),
		"min_size", p.Get("min_size"),
		"pg_num", p.Get("pg_num"),
		"pg_num_optimal", p.Get("pg_num_final"),
		"pg_num_min", p.Get("pg_num_min"),
		"autoscale", p.Get("pg_autoscale_mode"),
		"application", application,
		"crush_rule", or(p.Get("crush_rule_name"), p.Get("crush_rule")),
		"used", proxmox.FormatBytes(p.Get("bytes_used")),
		"used_pct", ifThen(p.Defined("percent_used"), js.Round(p.Num("percent_used")*1000)/10),
		"target_size", ifThen(p.Truthy("target_size"), proxmox.FormatBytes(p.Get("target_size"))),
		"target_size_ratio", p.Get("target_size_ratio"),
	)
}

func cephSummarizeDaemon(d *Obj) any {
	return proxmox.C(
		"name", d.Get("name"),
		"host", d.Get("host"),
		"state", d.Get("state"),
		"quorum", ifThen(d.Defined("quorum"), d.Truthy("quorum")),
		"rank", d.Get("rank"),
		"fs", d.Get("fs_name"),
		"standby_replay", ifThen(d.Truthy("standby_replay"), true),
		"addr", d.Get("addr"),
		"version", d.Get("ceph_version_short"),
		"service_enabled", ifThen(d.Defined("service"), d.Truthy("service")),
	)
}

var (
	cephSecretNameRe = regexp.MustCompile(`(?i)pass(word|wd)?\b|secret|token|(^|[/_-])key$`)
	cephConfKeyRe    = regexp.MustCompile(`(?im)^(\s*key\s*=\s*).+$`)
	cephUnitRe       = regexp.MustCompile(`^ceph-(mon|mgr|mds|osd)@(.+?)(\.service)?$`)
)

func registerCeph(c *Context) {
	client := c.Client

	// ---------------------------------------------------------------- read

	c.Define("proxmox_get_ceph_status", Def{
		Toolset:     "ceph",
		Title:       "Get Ceph status",
		Description: "Summarized Ceph cluster health: HEALTH_OK/WARN/ERR with each health check, monitor quorum, active/standby managers, OSD counts (up/in/down), PG states, raw usage and client I/O. Start here for any Ceph question; drill down with proxmox_list_ceph_osds, proxmox_list_ceph_pools or proxmox_list_ceph_services.",
		Input:       []Prop{P("node", cephNodeArg), P("raw", rawArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			// /cluster/ceph/status needs no node; fall back to the node alias if it fails (e.g. older versions).
			status, err := client.Get(ctx, "/cluster/ceph/status", nil)
			if err != nil {
				node, err := cephNode(ctx, c, a.Str("node"))
				if err != nil {
					return nil, err
				}
				if status, err = client.Get(ctx, cephBase(node)+"/status", nil); err != nil {
					return nil, err
				}
			}
			var muted any = js.Undef
			if m, err := client.Get(ctx, "/cluster/ceph/health-mute", nil); err == nil {
				muted = m
			}
			if a.Truthy("raw") {
				return proxmox.RedactSecrets(status), nil
			}
			var mutedChecks any = js.Undef
			if arr, ok := muted.([]any); ok {
				mutedChecks = mapObjs(js.Objs(arr), func(m *Obj) any {
					return proxmox.C("code", m.Get("code"), "ttl", m.Get("ttl"), "sticky", orUndef(m.Get("sticky")))
				})
			}
			return proxmox.Compact(js.Spread(cephSummarizeStatus(cephObj(status)), js.O("muted_checks", mutedChecks))), nil
		},
	})

	c.Define("proxmox_list_ceph_osds", Def{
		Toolset:     "ceph",
		Title:       "List Ceph OSDs",
		Description: "List Ceph OSDs from the OSD tree: host, up/down, in/out, device class, usage, PGs, CRUSH weight, latency and version, plus the per-host grouping and cluster-wide OSD flags (noout etc.). Filter by host, status or device class.",
		Input: []Prop{
			P("node", cephNodeArg),
			P("host", Str().Opt().D("Only OSDs on this host")),
			P("status", Enum("up", "down", "in", "out").Opt().D("Only OSDs in this state")),
			P("device_class", Str().Opt().D("Only this device class, e.g. hdd, ssd, nvme")),
			P("search", searchArg),
			P("limit", limitArg(1000)),
			P("raw", rawArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := cephNode(ctx, c, a.Str("node"))
			if err != nil {
				return nil, err
			}
			tree, err := cephOsdTree(ctx, c, node)
			if err != nil {
				return nil, err
			}
			list := append([]*Obj(nil), tree.osds...)
			if a.Truthy("host") {
				list = filterObjs(list, func(o *Obj) bool { return o.Get("host") == a.Get("host") })
			}
			switch st := a.Get("status"); st {
			case "up", "down":
				list = filterObjs(list, func(o *Obj) bool { return o.Get("status") == st })
			case "in":
				list = filterObjs(list, func(o *Obj) bool { return o.Num("in") == 1 })
			case "out":
				list = filterObjs(list, func(o *Obj) bool { return o.Num("in") != 1 })
			}
			if a.Truthy("device_class") {
				list = filterObjs(list, func(o *Obj) bool { return o.Get("device_class") == a.Get("device_class") })
			}
			sortObjs(list, func(x, y *Obj) float64 { return x.Num("id") - y.Num("id") })
			res := filterAndLimit(list, a.Get("search"), or(a.Get("limit"), 1000), func(o *Obj) []any {
				return []any{o.Get("name"), o.Get("host"), o.Get("device_class"), o.Get("status")}
			})
			hostNames := make([]string, len(tree.hosts))
			for i, h := range tree.hosts {
				hostNames[i] = js.Str(h.Get("name"))
			}
			sort.Strings(hostNames)
			var items any
			if a.Truthy("raw") {
				items = js.Array(res.Items)
			} else {
				items = mapObjs(res.Items, cephSummarizeOsd)
			}
			return proxmox.C(
				"flags", tree.flags,
				"summary", js.O(
					"total", len(tree.osds),
					"up", len(filterObjs(tree.osds, func(o *Obj) bool { return o.Get("status") == "up" })),
					"in", len(filterObjs(tree.osds, func(o *Obj) bool { return o.Num("in") == 1 })),
					"hosts", hostNames,
				),
				"total", res.Total,
				"returned", res.Returned,
				"items", items,
			), nil
		},
	})

	c.Define("proxmox_get_ceph_osd", Def{
		Toolset:     "ceph",
		Title:       "Get Ceph OSD details",
		Description: "Get one OSD's details: daemon metadata (devices, BlueStore, memory, version) and its logical volume info (block/DB/WAL devices).",
		Input:       []Prop{P("osdid", cephOsdIDArg), P("node", Str().Opt().D("Node hosting the OSD (looked up from the OSD tree when omitted)"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := cephOsdHost(ctx, c, a.Num("osdid"), a.Str("node"))
			if err != nil {
				return nil, err
			}
			base := cephBase(node) + "/osd/" + js.Str(a.Get("osdid"))
			metadata, err := client.Get(ctx, base+"/metadata", nil)
			if err != nil {
				return nil, err
			}
			lv, err := client.Get(ctx, base+"/lv-info", nil)
			if err != nil {
				lv = js.O("error", errMessage(err))
			}
			return proxmox.C("osd", a.Get("osdid"), "node", node, "metadata", proxmox.RedactSecrets(metadata), "lv_info", lv), nil
		},
	})

	c.Define("proxmox_list_ceph_pools", Def{
		Toolset:     "ceph",
		Title:       "List Ceph pools",
		Description: "List Ceph pools with replication (size/min_size), PG count and autoscale mode (with the autoscaler's optimal PG count), application, CRUSH rule and usage %. Related: proxmox_get_ceph_pool, proxmox_update_ceph_pool.",
		Input:       []Prop{P("node", cephNodeArg), P("search", searchArg), P("raw", rawArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := cephNode(ctx, c, a.Str("node"))
			if err != nil {
				return nil, err
			}
			pools, err := client.GetList(ctx, cephBase(node)+"/pool", nil)
			if err != nil {
				return nil, err
			}
			sortObjs(pools, func(x, y *Obj) float64 { return compareStr(x.Get("pool_name"), y.Get("pool_name")) })
			res := filterAndLimit(pools, a.Get("search"), js.Undef, func(p *Obj) []any {
				return []any{p.Get("pool_name"), p.Get("crush_rule_name"), p.Get("type")}
			})
			if a.Truthy("raw") {
				return res.Obj(nil), nil
			}
			return res.Map(cephSummarizePool), nil
		},
	})

	c.Define("proxmox_get_ceph_pool", Def{
		Toolset:     "ceph",
		Title:       "Get Ceph pool",
		Description: "Get one Ceph pool's settings (size, min_size, PGs, autoscaler, CRUSH rule, application, protection flags) and, with statistics=true, its usage and I/O statistics.",
		Input: []Prop{
			P("name", Str().D("Pool name")),
			P("statistics", Bool().Opt().D("Include usage and I/O statistics (default true)")),
			P("node", cephNodeArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := cephNode(ctx, c, a.Str("node"))
			if err != nil {
				return nil, err
			}
			return client.Get(ctx, cephBase(node)+"/pool/"+enc(a.Get("name"))+"/status", js.O("verbose", or(a.Get("statistics"), true)))
		},
	})

	c.Define("proxmox_list_ceph_services", Def{
		Toolset:     "ceph",
		Title:       "List Ceph monitors, managers and metadata servers",
		Description: "List Ceph daemons of one type or all: monitors (quorum, rank), managers (active/standby) and metadata servers (MDS state, CephFS, rank), with host, address and version.",
		Input: []Prop{
			P("type", Enum("mon", "mgr", "mds", "all").Opt().D("Daemon type (default all)")),
			P("node", cephNodeArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := cephNode(ctx, c, a.Str("node"))
			if err != nil {
				return nil, err
			}
			types := cephServiceTypes
			if a.Truthy("type") && a.Get("type") != "all" {
				types = []string{a.Str("type")}
			}
			out := js.NewObj()
			for _, t := range types {
				v, err := client.Get(ctx, cephBase(node)+"/"+t, nil)
				if err != nil {
					out.Set(t, js.O("error", errMessage(err)))
					continue
				}
				arr, ok := v.([]any)
				if !ok {
					out.Set(t, js.O("error", "list.sort is not a function"))
					continue
				}
				list := js.Objs(arr)
				sortObjs(list, func(x, y *Obj) float64 { return compareStr(x.Get("name"), y.Get("name")) })
				out.Set(t, mapObjs(list, cephSummarizeDaemon))
			}
			return out, nil
		},
	})

	c.Define("proxmox_list_ceph_fs", Def{
		Toolset:     "ceph",
		Title:       "List CephFS file systems",
		Description: "List CephFS file systems with their metadata and data pools. MDS daemons serving them are listed by proxmox_list_ceph_services.",
		Input:       []Prop{P("node", cephNodeArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := cephNode(ctx, c, a.Str("node"))
			if err != nil {
				return nil, err
			}
			fs, err := client.GetList(ctx, cephBase(node)+"/fs", nil)
			if err != nil {
				return nil, err
			}
			return mapObjs(fs, func(f *Obj) any {
				return proxmox.C("name", f.Get("name"), "metadata_pool", f.Get("metadata_pool"), "data_pools", or(f.Get("data_pools"), []any{f.Get("data_pool")}))
			}), nil
		},
	})

	c.Define("proxmox_get_ceph_flags", Def{
		Toolset:     "ceph",
		Title:       "Get Ceph OSD flags",
		Description: "Show which cluster-wide Ceph OSD flags are set (noout, noscrub, nodeep-scrub, norebalance, pause...). Change them with proxmox_set_ceph_flags.",
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			flags, err := client.GetList(ctx, "/cluster/ceph/flags", nil)
			if err != nil {
				return nil, err
			}
			return js.O(
				"set", mapObjs(filterObjs(flags, func(f *Obj) bool { return f.Truthy("value") }), func(f *Obj) any { return f.Get("name") }),
				"flags", mapObjs(flags, func(f *Obj) any {
					return js.O("name", f.Get("name"), "set", f.Truthy("value"), "description", f.Get("description"))
				}),
			), nil
		},
	})

	c.Define("proxmox_get_ceph_crush", Def{
		Toolset:     "ceph",
		Title:       "Get Ceph CRUSH map and rules",
		Description: "Get the decompiled CRUSH map (devices, buckets, rules) and the list of CRUSH rule names usable as a pool's crush_rule.",
		Input:       []Prop{P("node", cephNodeArg), P("rules_only", Bool().Opt().D("Only list the rule names (much shorter)"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := cephNode(ctx, c, a.Str("node"))
			if err != nil {
				return nil, err
			}
			rules, err := client.GetList(ctx, cephBase(node)+"/rules", nil)
			if err != nil {
				return nil, err
			}
			names := mapObjs(rules, func(r *Obj) any { return r.Get("name") })
			if a.Truthy("rules_only") {
				return js.O("rules", names), nil
			}
			crush, err := client.Get(ctx, cephBase(node)+"/crush", nil)
			if err != nil {
				return nil, err
			}
			return js.O("rules", names, "crush_map", crush), nil
		},
	})

	c.Define("proxmox_get_ceph_config", Def{
		Toolset:     "ceph",
		Title:       "Get Ceph configuration",
		Description: "Read Ceph configuration: the configuration database (`ceph config dump`: section, name, value) and/or the ceph.conf file. Secrets such as keys are redacted.",
		Input: []Prop{
			P("source", Enum("db", "file", "both").Opt().D("Configuration database, ceph.conf file, or both (default both)")),
			P("search", searchArg),
			P("node", cephNodeArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := cephNode(ctx, c, a.Str("node"))
			if err != nil {
				return nil, err
			}
			src := js.Str(or(a.Get("source"), "both"))
			out := js.NewObj()
			if src != "file" {
				db, err := client.GetList(ctx, cephBase(node)+"/cfg/db", nil)
				if err != nil {
					return nil, err
				}
				res := filterAndLimit(db, a.Get("search"), js.Undef, func(e *Obj) []any {
					return []any{e.Get("section"), e.Get("name"), e.Get("value"), e.Get("mask")}
				})
				// The values sit under a generic "value" key, so redact by option name (e.g. mgr/dashboard passwords, rgw keystone secrets).
				out.Set("db", mapObjs(res.Items, func(e *Obj) any {
					value := e.Get("value")
					if cephSecretNameRe.MatchString(js.Str(or(e.Get("name"), ""))) && js.Truthy(value) {
						value = "<redacted>"
					}
					return proxmox.C(
						"section", e.Get("section"),
						"name", e.Get("name"),
						"value", value,
						"mask", e.Get("mask"),
						"level", e.Get("level"),
						"runtime", e.Get("can_update_at_runtime"),
					)
				}))
			}
			if src != "db" {
				raw, err := client.Get(ctx, cephBase(node)+"/cfg/raw", nil)
				if err != nil {
					return nil, err
				}
				// ceph.conf rarely holds secrets, but keyring-style "key = ..." lines can appear.
				out.Set("file", cephConfKeyRe.ReplaceAllString(js.Str(or(raw, "")), "${1}<redacted>"))
			}
			return out, nil
		},
	})

	c.Define("proxmox_get_ceph_log", Def{
		Toolset:     "ceph",
		Title:       "Read Ceph log",
		Description: "Read lines of the Ceph cluster log (ceph.log) from a node: health changes, OSD up/down events, slow ops and scrub errors.",
		Input: []Prop{
			P("node", cephNodeArg),
			P("limit", Int().Pos().Max(5000).Opt().D("Number of lines (default 50)")),
			P("start", Int().Min(0).Opt().D("Offset of the first line (0-based)")),
			P("search", searchArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := cephNode(ctx, c, a.Str("node"))
			if err != nil {
				return nil, err
			}
			lines, err := client.GetList(ctx, cephBase(node)+"/log", defined(js.O("limit", or(a.Get("limit"), 50), "start", a.Get("start"))))
			if err != nil {
				return nil, err
			}
			res := filterAndLimit(lines, a.Get("search"), js.Undef, func(l *Obj) []any { return []any{l.Get("t")} })
			return js.Spread(js.O("node", node), res.Map(func(l *Obj) any { return l.Get("t") })), nil
		},
	})

	c.Define("proxmox_get_ceph_metadata", Def{
		Toolset:     "ceph",
		Title:       "Get Ceph daemon metadata",
		Description: "Get metadata of every Ceph daemon (mon, mgr, mds, osd) and node: host, Ceph version and release, memory. Useful to spot daemons running an outdated version after an upgrade.",
		Input:       []Prop{P("raw", rawArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			meta, err := client.Get(ctx, "/cluster/ceph/metadata", nil)
			if err != nil {
				return nil, err
			}
			if a.Truthy("raw") {
				return proxmox.RedactSecrets(meta), nil
			}
			out := js.NewObj()
			metaObj, _ := meta.(*Obj)
			for _, typ := range metaObj.Keys() {
				var ids []string
				var values []any
				switch entries := metaObj.Get(typ).(type) {
				case *Obj:
					if entries == nil {
						continue
					}
					ids = entries.Keys()
					for _, k := range ids {
						values = append(values, entries.Get(k))
					}
				case []any:
					for i, e := range entries {
						ids = append(ids, fmt.Sprint(i))
						values = append(values, e)
					}
				default:
					continue
				}
				items := make([]any, len(ids))
				for i, id := range ids {
					e := values[i]
					var memory any = js.Undef
					if js.Truthy(cephProp(e, "mem_total_kb")) {
						memory = proxmox.FormatBytes(js.Num(cephProp(e, "mem_total_kb")) * 1024)
					}
					items[i] = proxmox.C(
						"id", id,
						"host", cephProp(e, "hostname"),
						"version", or(cephProp(e, "ceph_version_short"), or(cephProp(cephProp(e, "version"), "str"), cephProp(e, "version"))),
						"release", cephProp(e, "ceph_release"),
						"memory", memory,
					)
				}
				out.Set(typ, items)
			}
			return out, nil
		},
	})

	c.Define("proxmox_check_ceph_safety", Def{
		Toolset:     "ceph",
		Title:       "Check if stopping/destroying a Ceph daemon is safe",
		Description: "Ask Ceph whether stopping or destroying an OSD, monitor or MDS is safe right now (e.g. no PGs would become unavailable). Call before proxmox_delete_ceph_osd, proxmox_delete_ceph_service or stopping services.",
		Input: []Prop{
			P("service", Enum("osd", "mon", "mds").D("Daemon type")),
			P("id", Str().D("Daemon ID, e.g. '3' for osd.3 or the monitor name")),
			P("action", Enum("stop", "destroy").D("Intended action")),
			P("node", cephNodeArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := cephNode(ctx, c, a.Str("node"))
			if err != nil {
				return nil, err
			}
			r, err := client.Get(ctx, cephBase(node)+"/cmd-safety", js.O("service", a.Get("service"), "id", a.Get("id"), "action", a.Get("action")))
			if err != nil {
				return nil, err
			}
			return proxmox.C("service", a.Get("service"), "id", a.Get("id"), "action", a.Get("action"), "safe", js.Truthy(cephProp(r, "safe")), "status", cephProp(r, "status")), nil
		},
	})

	// ---------------------------------------------------------------- OSD write

	c.Define("proxmox_control_ceph_osd", Def{
		Toolset:     "ceph",
		Title:       "Mark OSD in/out or scrub it",
		Description: "Mark an OSD in or out of data placement, or start a (deep) scrub. Marking out starts moving its data elsewhere (rebalancing); mark in to bring it back. Set the noout flag instead for short maintenance.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("osdid", cephOsdIDArg),
			P("action", Enum("in", "out", "scrub", "deep-scrub").D("What to do")),
			P("node", Str().Opt().D("Node hosting the OSD (looked up when omitted)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := cephOsdHost(ctx, c, a.Num("osdid"), a.Str("node"))
			if err != nil {
				return nil, err
			}
			base := cephBase(node) + "/osd/" + js.Str(a.Get("osdid"))
			action := a.Str("action")
			if action == "scrub" || action == "deep-scrub" {
				_, err = client.Post(ctx, base+"/scrub", js.O("deep", action == "deep-scrub"))
			} else {
				_, err = client.Post(ctx, base+"/"+action, nil)
			}
			if err != nil {
				return nil, err
			}
			return js.O("osd", a.Get("osdid"), "node", node, "action", action, "done", true), nil
		},
	})

	c.Define("proxmox_create_ceph_osd", Def{
		Toolset:     "ceph",
		Title:       "Create a Ceph OSD",
		Description: "Create a Ceph OSD on an unused disk of a node. DESTROYS ALL DATA on the disk (and on db_dev/wal_dev partitions it uses). List candidate disks with the node disk tools first.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("node", cephTargetArg),
			P("dev", Str().D("Block device, e.g. /dev/sdb")),
			P("device_class", Str().Opt().D("CRUSH device class, e.g. hdd, ssd, nvme (default auto-detected)")),
			P("db_dev", Str().Opt().D("Separate device for the BlueStore DB")),
			P("db_dev_size", Num().Pos().Opt().D("DB size in GiB")),
			P("wal_dev", Str().Opt().D("Separate device for the BlueStore WAL")),
			P("wal_dev_size", Num().Pos().Opt().D("WAL size in GiB")),
			P("encrypted", Bool().Opt().D("Encrypt the OSD")),
			P("osds_per_device", Int().Pos().Opt().D("OSDs per device (fast NVMe only; excludes db_dev/wal_dev)")),
			P("wait", waitArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			params, err := definedExtra(js.O(
				"dev", a.Get("dev"),
				"crush-device-class", a.Get("device_class"),
				"db_dev", a.Get("db_dev"),
				"db_dev_size", a.Get("db_dev_size"),
				"wal_dev", a.Get("wal_dev"),
				"wal_dev_size", a.Get("wal_dev_size"),
				"encrypted", a.Get("encrypted"),
				"osds-per-device", a.Get("osds_per_device"),
			), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			upid, err := client.Post(ctx, cephBase(a.Str("node"))+"/osd", params)
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("node", a.Get("node"), "dev", a.Get("dev")))
		},
	})

	c.Define("proxmox_delete_ceph_osd", Def{
		Toolset:     "ceph",
		Title:       "Destroy a Ceph OSD",
		Description: "Destroy an OSD and remove it from the cluster. The OSD must be out and stopped first (proxmox_control_ceph_osd action=out, then proxmox_control_ceph_service action=stop service=osd.<id>) and data rebalanced; check with proxmox_check_ceph_safety. cleanup=true also wipes the disk.",
		Delete:      true,
		Input: []Prop{
			P("osdid", cephOsdIDArg),
			P("node", Str().Opt().D("Node hosting the OSD (looked up when omitted)")),
			P("cleanup", Bool().Opt().D("Also zap the logical volumes and wipe the disk")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := cephOsdHost(ctx, c, a.Num("osdid"), a.Str("node"))
			if err != nil {
				return nil, err
			}
			upid, err := client.Delete(ctx, cephBase(node)+"/osd/"+js.Str(a.Get("osdid")), defined(js.O("cleanup", a.Get("cleanup"))))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("osd", a.Get("osdid"), "node", node))
		},
	})

	// ---------------------------------------------------------------- pools write

	poolSettings := []Prop{
		P("size", Int().Min(1).Max(7).Opt().D("Number of replicas (default 3)")),
		P("min_size", Int().Min(1).Max(7).Opt().D("Minimum replicas needed to serve I/O (default 2)")),
		P("pg_num", Int().Min(1).Max(32768).Opt().D("Number of placement groups")),
		P("pg_num_min", Int().Max(32768).Opt().D("Minimum PG count for the autoscaler")),
		P("pg_autoscale_mode", Enum("on", "off", "warn").Opt().D("PG autoscaler mode")),
		P("application", Enum("rbd", "cephfs", "rgw").Opt().D("Pool application (default rbd)")),
		P("crush_rule", Str().Opt().D("CRUSH rule name (see proxmox_get_ceph_crush)")),
		P("target_size", Str().Opt().D("Expected pool size for the autoscaler, e.g. 500G")),
		P("target_size_ratio", Num().Opt().D("Expected share of the cluster capacity for the autoscaler")),
	}
	withSettings := func(first []Prop, rest ...Prop) []Prop {
		out := append(append(append([]Prop{}, first...), poolSettings...), rest...)
		return out
	}

	c.Define("proxmox_create_ceph_pool", Def{
		Toolset:     "ceph",
		Title:       "Create a Ceph pool",
		Description: "Create a Ceph pool, optionally erasure-coded and optionally added as Proxmox storage (add_storages). Defaults: size 3, min_size 2, application rbd.",
		Write:       true,
		Destructive: False,
		Input: withSettings([]Prop{P("name", Str().D("Pool name"))},
			P("add_storages", Bool().Opt().D("Also create Proxmox storage entries for the pool")),
			P("erasure_coding", Str().Opt().D("Create an erasure-coded pool, e.g. 'k=2,m=1' (property string; see the Proxmox docs)")),
			P("node", cephNodeArg),
			P("wait", waitArg),
			P("extra", extraArg),
		),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := cephNode(ctx, c, a.Str("node"))
			if err != nil {
				return nil, err
			}
			params, err := definedExtra(js.O(
				"name", a.Get("name"),
				"size", a.Get("size"),
				"min_size", a.Get("min_size"),
				"pg_num", a.Get("pg_num"),
				"pg_num_min", a.Get("pg_num_min"),
				"pg_autoscale_mode", a.Get("pg_autoscale_mode"),
				"application", a.Get("application"),
				"crush_rule", a.Get("crush_rule"),
				"target_size", a.Get("target_size"),
				"target_size_ratio", a.Get("target_size_ratio"),
				"add_storages", a.Get("add_storages"),
				"erasure-coding", a.Get("erasure_coding"),
			), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			upid, err := client.Post(ctx, cephBase(node)+"/pool", params)
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("pool", a.Get("name")))
		},
	})

	c.Define("proxmox_update_ceph_pool", Def{
		Toolset:     "ceph",
		Title:       "Change Ceph pool settings",
		Description: "Change a pool's size/min_size, PG count, autoscaler, application or CRUSH rule. Only given fields change. Changing size, pg_num or crush_rule moves data and can load the cluster for a long time.",
		Write:       true,
		Destructive: True,
		Input:       withSettings([]Prop{P("name", Str().D("Pool name"))}, P("node", cephNodeArg), P("wait", waitArg), P("extra", extraArg)),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := cephNode(ctx, c, a.Str("node"))
			if err != nil {
				return nil, err
			}
			params, err := definedExtra(js.O(
				"size", a.Get("size"),
				"min_size", a.Get("min_size"),
				"pg_num", a.Get("pg_num"),
				"pg_num_min", a.Get("pg_num_min"),
				"pg_autoscale_mode", a.Get("pg_autoscale_mode"),
				"application", a.Get("application"),
				"crush_rule", a.Get("crush_rule"),
				"target_size", a.Get("target_size"),
				"target_size_ratio", a.Get("target_size_ratio"),
			), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			if params.Len() == 0 {
				return nil, errors.New("Nothing to change: give at least one setting")
			}
			upid, err := client.Put(ctx, cephBase(node)+"/pool/"+enc(a.Get("name")), params)
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("pool", a.Get("name"), "changed", params.Keys()))
		},
	})

	c.Define("proxmox_delete_ceph_pool", Def{
		Toolset:     "ceph",
		Title:       "Destroy a Ceph pool",
		Description: "Destroy a Ceph pool and ALL data in it. Refused while in use unless force=true. Optionally removes the Proxmox storage entries for it.",
		Delete:      true,
		Input: []Prop{
			P("name", Str().D("Pool name")),
			P("force", Bool().Opt().D("Destroy even if guest disks still use it")),
			P("remove_storages", Bool().Opt().D("Also remove Proxmox storage entries for this pool")),
			P("remove_ecprofile", Bool().Opt().D("Remove the erasure-code profile (default true where applicable)")),
			P("node", cephNodeArg),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := cephNode(ctx, c, a.Str("node"))
			if err != nil {
				return nil, err
			}
			upid, err := client.Delete(ctx, cephBase(node)+"/pool/"+enc(a.Get("name")),
				defined(js.O("force", a.Get("force"), "remove_storages", a.Get("remove_storages"), "remove_ecprofile", a.Get("remove_ecprofile"))))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("pool", a.Get("name")))
		},
	})

	// ---------------------------------------------------------------- mon / mgr / mds

	c.Define("proxmox_create_ceph_service", Def{
		Toolset:     "ceph",
		Title:       "Create a Ceph monitor, manager or MDS",
		Description: "Create a Ceph monitor (mon), manager (mgr) or metadata server (mds) on a node. The daemon ID defaults to the node name. The first monitor also gets a manager.",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("type", Enum(cephServiceTypes...).D("Daemon type")),
			P("node", cephTargetArg),
			P("id", Str().Opt().D("Daemon ID (default the node name)")),
			P("mon_address", Arr(Str()).Opt().D("mon only: IP address(es) in the Ceph public network (default auto-detected)")),
			P("hotstandby", Bool().Opt().D("mds only: poll the active MDS's log for faster failover")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			id := or(a.Get("id"), a.Get("node"))
			var monAddress any = js.Undef
			if a.Get("type") == "mon" && a.Defined("mon_address") {
				monAddress = js.Str(a.Get("mon_address"))
			}
			params := defined(js.O(
				"mon-address", monAddress,
				"hotstandby", ifThen(a.Get("type") == "mds", a.Get("hotstandby")),
			))
			upid, err := client.Post(ctx, cephBase(a.Str("node"))+"/"+a.Str("type")+"/"+enc(id), params)
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("type", a.Get("type"), "id", id, "node", a.Get("node")))
		},
	})

	c.Define("proxmox_delete_ceph_service", Def{
		Toolset:     "ceph",
		Title:       "Destroy a Ceph monitor, manager or MDS",
		Description: "Destroy a Ceph monitor, manager or metadata server. The last monitor can't be removed; keep enough monitors for quorum. Check with proxmox_check_ceph_safety first.",
		Delete:      true,
		Input: []Prop{
			P("type", Enum(cephServiceTypes...).D("Daemon type")),
			P("node", cephTargetArg),
			P("id", Str().D("Daemon ID (see proxmox_list_ceph_services)")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			upid, err := client.Delete(ctx, cephBase(a.Str("node"))+"/"+a.Str("type")+"/"+enc(a.Get("id")), nil)
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("type", a.Get("type"), "id", a.Get("id"), "node", a.Get("node")))
		},
	})

	c.Define("proxmox_create_ceph_fs", Def{
		Toolset:     "ceph",
		Title:       "Create a CephFS",
		Description: "Create a CephFS file system (with its data and metadata pools). Needs at least one MDS (proxmox_create_ceph_service type=mds). add_storage=true also adds it as Proxmox storage.",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("name", Str().Opt().D("File system name (default cephfs)")),
			P("node", cephTargetArg),
			P("pg_num", Int().Pos().Opt().D("PGs of the data pool (default 128; metadata pool gets a quarter)")),
			P("add_storage", Bool().Opt().D("Add it as Proxmox storage")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			name := or(a.Get("name"), "cephfs")
			upid, err := client.Post(ctx, cephBase(a.Str("node"))+"/fs/"+enc(name), defined(js.O("pg_num", a.Get("pg_num"), "add-storage", a.Get("add_storage"))))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("fs", name))
		},
	})

	c.Define("proxmox_delete_ceph_fs", Def{
		Toolset:     "ceph",
		Title:       "Destroy a CephFS",
		Description: "Destroy a CephFS file system. Refused while an enabled Proxmox cephfs storage uses it. remove_pools=true also deletes its pools and ALL data.",
		Delete:      true,
		Input: []Prop{
			P("name", Str().D("File system name")),
			P("node", cephTargetArg),
			P("remove_pools", Bool().Opt().D("Also remove the data and metadata pools")),
			P("remove_storages", Bool().Opt().D("Also remove the Proxmox storage entries for it")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			upid, err := client.Delete(ctx, cephBase(a.Str("node"))+"/fs/"+enc(a.Get("name")), defined(js.O("remove-pools", a.Get("remove_pools"), "remove-storages", a.Get("remove_storages"))))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("fs", a.Get("name")))
		},
	})

	// ---------------------------------------------------------------- flags / health

	c.Define("proxmox_set_ceph_flags", Def{
		Toolset:     "ceph",
		Title:       "Set or clear Ceph OSD flags",
		Description: "Set (true) or clear (false) cluster-wide Ceph OSD flags; omitted flags are left alone (nodeep-scrub is nodeep_scrub here). Typical: noout=true before node maintenance, then noout=false afterwards. pause=true stops ALL client I/O.",
		Write:       true,
		Destructive: True,
		Input:       append(cephFlagArgs(), P("wait", waitArg)),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			all := js.NewObj()
			for _, f := range cephFlags {
				all.Set(f, a.Get(strings.Replace(f, "-", "_", 1)))
			}
			flags := defined(all)
			if flags.Len() == 0 {
				return nil, errors.New("Give at least one flag to set or clear")
			}
			upid, err := client.Put(ctx, "/cluster/ceph/flags", flags)
			if err != nil {
				return nil, err
			}
			result, err := runTask(ctx, client, upid, a.Get("wait"), js.O("changed", flags))
			if err != nil {
				return nil, err
			}
			var nowSet any = js.Undef
			if v, err := client.Get(ctx, "/cluster/ceph/flags", nil); err == nil {
				if arr, ok := v.([]any); ok {
					nowSet = mapObjs(filterObjs(js.Objs(arr), func(f *Obj) bool { return f.Truthy("value") }), func(f *Obj) any { return f.Get("name") })
				}
			}
			return proxmox.Compact(js.Spread(result, js.O("now_set", nowSet))), nil
		},
	})

	c.Define("proxmox_mute_ceph_health_check", Def{
		Toolset:     "ceph",
		Title:       "Mute or unmute a Ceph health check",
		Description: "Mute (or unmute) a Ceph health check such as AUTH_INSECURE_CLIENT_KEY_TYPE so it no longer degrades the health status; it stays visible in proxmox_get_ceph_status. Optionally for a limited time (ttl).",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input: []Prop{
			P("code", Str().D("Health check code from proxmox_get_ceph_status, e.g. OSD_NEARFULL")),
			P("mute", Bool().Opt().D("true to mute (default), false to unmute")),
			P("ttl", Str().Opt().D("Mute duration, e.g. '2h', '3d', '1w' (default no expiry)")),
			P("sticky", Bool().Opt().D("Keep the mute even if the check gets worse")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			value := or(a.Get("mute"), true)
			on := js.Truthy(value)
			if _, err := client.Put(ctx, "/cluster/ceph/health-mute/"+enc(a.Get("code")), defined(js.O("value", value, "ttl", ifThen(on, a.Get("ttl")), "sticky", ifThen(on, a.Get("sticky"))))); err != nil {
				return nil, err
			}
			return js.O("code", a.Get("code"), "muted", value), nil
		},
	})

	// ---------------------------------------------------------------- node services

	c.Define("proxmox_control_ceph_service", Def{
		Toolset:     "ceph",
		Title:       "Start, stop or restart Ceph services",
		Description: "Start, stop or restart Ceph services on a node: all of them (default ceph.target), all of one type ('osd', 'mon', 'mgr', 'mds') or one daemon as <type>.<id>, e.g. 'osd.3', 'mon.pve1', 'mgr.pve1', 'mds.pve1'. Stopping can make data unavailable; check proxmox_check_ceph_safety and consider the noout flag.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("action", Enum("start", "stop", "restart").D("What to do")),
			P("node", cephTargetArg),
			P("service", Str().Opt().D("Service as <type>[.<id>], e.g. osd.3, mon.pve1 or osd (all OSDs); default ceph.target = all Ceph services on the node")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			// Proxmox wants <type>.<id>; accept the systemd unit names (ceph-osd@3) models tend to use too.
			var service any = js.Undef
			if a.Defined("service") {
				service = cephUnitRe.ReplaceAllString(a.Str("service"), "${1}.${2}")
			}
			upid, err := client.Post(ctx, cephBase(a.Str("node"))+"/"+a.Str("action"), defined(js.O("service", service)))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("node", a.Get("node"), "action", a.Get("action"), "service", or(service, "ceph.target")))
		},
	})

	c.Define("proxmox_init_ceph", Def{
		Toolset:     "ceph",
		Title:       "Initialise Ceph configuration",
		Description: "Create the initial Ceph configuration (ceph.conf) for the cluster with the public and optional cluster network. Run once on the first node after installing Ceph, then create monitors. Existing settings are preserved on re-run.",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input: []Prop{
			P("node", cephTargetArg),
			P("network", Str().Opt().D("Public network for Ceph traffic in CIDR, e.g. 10.10.10.0/24")),
			P("cluster_network", Str().Opt().D("Separate network for OSD replication/heartbeat traffic (CIDR)")),
			P("size", Int().Min(1).Max(7).Opt().D("Default pool replicas (default 3)")),
			P("min_size", Int().Min(1).Max(7).Opt().D("Default pool min_size (default 2)")),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			params, err := definedExtra(js.O("network", a.Get("network"), "cluster-network", a.Get("cluster_network"), "size", a.Get("size"), "min_size", a.Get("min_size")), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			if _, err := client.Post(ctx, cephBase(a.Str("node"))+"/init", params); err != nil {
				return nil, err
			}
			return js.O("initialised", true, "node", a.Get("node")), nil
		},
	})
}
