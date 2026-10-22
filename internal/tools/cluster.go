package tools

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

var tagSplitRe = regexp.MustCompile(`[;,\s]+`)

// splitTags splits a Proxmox tag list ("a;b c,d") into its non-empty tags.
func splitTags(s string) []any {
	out := []any{}
	for _, t := range tagSplitRe.Split(s, -1) {
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// summarizeGuest is a compact summary of a /cluster/resources guest entry (VM or container).
func summarizeGuest(r *Obj) *Obj {
	running := r.Get("status") == "running"
	typ := r.Get("type")
	switch typ {
	case "qemu":
		typ = "vm"
	case "lxc":
		typ = "container"
	}
	var memory any
	if running {
		memory = proxmox.Usage(r.Get("mem"), r.Get("maxmem"))
	} else {
		memory = proxmox.FormatBytes(r.Get("maxmem"))
	}
	var disk any = js.Undef
	if r.Truthy("maxdisk") {
		if r.Truthy("disk") {
			disk = proxmox.Usage(r.Get("disk"), r.Get("maxdisk"))
		} else {
			disk = proxmox.FormatBytes(r.Get("maxdisk"))
		}
	}
	var tags any = js.Undef
	if r.Truthy("tags") {
		tags = splitTags(js.Str(r.Get("tags")))
	}
	return proxmox.C(
		"vmid", r.Num("vmid"),
		"name", r.Get("name"),
		"type", typ,
		"node", r.Get("node"),
		"status", r.Get("status"),
		"template", ifThen(r.Truthy("template"), true),
		"lock", r.Get("lock"),
		"hastate", r.Get("hastate"),
		"pool", r.Get("pool"),
		"tags", tags,
		"cpus", r.Get("maxcpu"),
		"cpu_pct", ifThen(running, proxmox.FractionPct(r.Get("cpu"))),
		"memory", memory,
		"disk", disk,
		"uptime", ifThen(running, proxmox.FormatUptime(r.Get("uptime"))),
		"net_in", ifThen(running, proxmox.FormatBytes(r.Get("netin"))),
		"net_out", ifThen(running, proxmox.FormatBytes(r.Get("netout"))),
	)
}

// summarizeNode is a compact summary of a node entry from /nodes or /cluster/resources.
func summarizeNode(n *Obj) *Obj {
	return proxmox.C(
		"node", n.Get("node"),
		"status", n.Get("status"),
		"cpu_pct", ifThen(n.Get("status") == "online", proxmox.FractionPct(n.Get("cpu"))),
		"cpus", n.Get("maxcpu"),
		"memory", proxmox.Usage(n.Get("mem"), n.Get("maxmem")),
		"root_disk", proxmox.Usage(n.Get("disk"), n.Get("maxdisk")),
		"uptime", proxmox.FormatUptime(n.Get("uptime")),
		"subscription_level", orUndef(n.Get("level")),
		"ssl_fingerprint", n.Get("ssl_fingerprint"),
	)
}

func clusterSummarizeStorage(s *Obj) any {
	return proxmox.C(
		"storage", s.Get("storage"),
		"node", s.Get("node"),
		"type", s.Get("plugintype"),
		"status", s.Get("status"),
		"shared", ifThen(s.Truthy("shared"), true),
		"content", s.Get("content"),
		"usage", proxmox.Usage(s.Get("disk"), s.Get("maxdisk")),
		"used_pct", proxmox.Percent(s.Get("disk"), s.Get("maxdisk")),
	)
}

var clusterProxyCredRe = regexp.MustCompile(`//[^/@\s]+@`)

// clusterRedactConfig is RedactSecrets plus secrets it can't recognise by key name: credentials
// in the http_proxy URL (user:password@host) and OpenTelemetry auth headers.
func clusterRedactConfig(value any) any {
	out := proxmox.RedactSecrets(value)
	fix := func(o *Obj) {
		if s, ok := o.Get("http_proxy").(string); ok {
			o.Set("http_proxy", clusterProxyCredRe.ReplaceAllString(s, "//<redacted>@"))
		}
		if o.Truthy("otel-headers") {
			o.Set("otel-headers", "<redacted>")
		}
	}
	switch x := out.(type) {
	case []any:
		for _, e := range x {
			if o, ok := e.(*Obj); ok {
				fix(o)
			}
		}
	case *Obj:
		fix(x)
	}
	return out
}

func registerCluster(c *Context) {
	client := c.Client

	// ---------------------------------------------------------------- read

	c.Define("proxmox_get_version", Def{
		Toolset:     "cluster",
		Title:       "Get Proxmox VE version",
		Description: "Get the Proxmox VE version and release of the API endpoint this server talks to.",
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			return client.Get(ctx, "/version", nil)
		},
	})

	c.Define("proxmox_get_cluster_status", Def{
		Toolset:     "cluster",
		Title:       "Get cluster overview",
		Description: "One-call overview of the whole cluster: name, quorum, Proxmox version, every node (online state, CPU, memory, uptime), guest counts by state, totals, storage usage and HA state. A good first call.",
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			status, err := client.GetList(ctx, "/cluster/status", nil)
			if err != nil {
				return nil, err
			}
			resources, err := client.GetList(ctx, "/cluster/resources", nil)
			if err != nil {
				return nil, err
			}
			version, verr := client.GetObj(ctx, "/version", nil)
			var cluster *Obj
			nodeStatus := map[string]*Obj{}
			for _, s := range status {
				if s.Get("type") == "cluster" && cluster == nil {
					cluster = s
				}
				if s.Get("type") == "node" {
					nodeStatus[js.Str(s.Get("name"))] = s
				}
			}
			byType := func(pred func(*Obj) bool) []*Obj { return filterObjs(resources, pred) }
			nodes := byType(func(r *Obj) bool { return r.Get("type") == "node" })
			guests := byType(func(r *Obj) bool { return r.Get("type") == "qemu" || r.Get("type") == "lxc" })
			storages := byType(func(r *Obj) bool { return r.Get("type") == "storage" })
			count := func(list []*Obj, pred func(*Obj) bool) int { return len(filterObjs(list, pred)) }
			sum := func(list []*Obj, k string) float64 {
				n := 0.0
				for _, r := range list {
					if v := r.Num(k); js.Truthy(v) {
						n += v
					}
				}
				return n
			}
			online := filterObjs(nodes, func(n *Obj) bool { return n.Get("status") == "online" })

			var clusterInfo *Obj
			if cluster != nil {
				clusterInfo = js.O("name", cluster.Get("name"), "quorate", cluster.Truthy("quorate"), "nodes", cluster.Get("nodes"), "config_version", cluster.Get("version"))
			} else {
				clusterInfo = js.O("name", "(standalone node, no cluster)")
			}
			var versionText any = js.Undef
			if verr == nil {
				versionText = js.Str(version.Get("version")) + " (release " + js.Str(version.Get("release")) + ")"
			}
			var locked []any
			for _, g := range guests {
				if g.Truthy("lock") {
					locked = append(locked, js.O("vmid", g.Get("vmid"), "name", g.Get("name"), "lock", g.Get("lock")))
				}
			}
			if locked == nil {
				locked = []any{}
			}
			isType := func(g *Obj, t string) bool { return g.Get("type") == t }
			cpuPct := any(js.Undef)
			if len(online) > 0 {
				cpuPct = proxmox.FractionPct(sum(online, "cpu") / float64(len(online)))
			}
			return proxmox.C(
				"cluster", clusterInfo,
				"version", versionText,
				"nodes", mapObjs(nodes, func(n *Obj) any {
					ns := nodeStatus[js.Str(n.Get("node"))]
					return proxmox.Compact(js.Spread(summarizeNode(n), js.O("ip", ns.Get("ip"), "local", ifThen(ns.Truthy("local"), true))))
				}),
				"guests", js.O(
					"vms", js.O(
						"running", count(guests, func(g *Obj) bool { return isType(g, "qemu") && g.Get("status") == "running" && !g.Truthy("template") }),
						"stopped", count(guests, func(g *Obj) bool { return isType(g, "qemu") && g.Get("status") != "running" && !g.Truthy("template") }),
						"templates", count(guests, func(g *Obj) bool { return isType(g, "qemu") && g.Truthy("template") }),
					),
					"containers", js.O(
						"running", count(guests, func(g *Obj) bool { return isType(g, "lxc") && g.Get("status") == "running" }),
						"stopped", count(guests, func(g *Obj) bool { return isType(g, "lxc") && g.Get("status") != "running" && !g.Truthy("template") }),
						"templates", count(guests, func(g *Obj) bool { return isType(g, "lxc") && g.Truthy("template") }),
					),
					"locked", locked,
					"ha_managed", count(guests, func(g *Obj) bool { return g.Truthy("hastate") }),
				),
				"totals", js.O(
					"cpus", sum(online, "maxcpu"),
					"cpu_pct", cpuPct,
					"memory", proxmox.Usage(sum(online, "mem"), sum(online, "maxmem")),
				),
				"storage", mapObjs(storages, clusterSummarizeStorage),
			), nil
		},
	})

	c.Define("proxmox_list_nodes", Def{
		Toolset:     "cluster",
		List:        &listing{},
		Title:       "List nodes",
		Description: "List the cluster's nodes with online state, CPU and memory usage, uptime and subscription level.",
		Input:       []Prop{P("raw", rawArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			nodes, err := client.GetList(ctx, "/nodes", nil)
			if err != nil {
				return nil, err
			}
			sortObjs(nodes, func(x, y *Obj) float64 { return compareStr(x.Get("node"), y.Get("node")) })
			if a.Truthy("raw") {
				return proxmox.RedactSecrets(js.Array(nodes)), nil
			}
			return mapObjs(nodes, func(n *Obj) any { return summarizeNode(n) }), nil
		},
	})

	c.Define("proxmox_list_guests", Def{
		Toolset:     "cluster",
		List:        &listing{},
		Title:       "List VMs and containers",
		Description: "List virtual machines and containers across the cluster with status, node, CPU, memory, disk, uptime, tags and pool. Filter by type, node, status, tag, pool or a search string, and sort by resource use.",
		Input: []Prop{
			P("type", Enum("vm", "container").Opt().D("Only VMs (qemu) or only containers (lxc)")),
			P("node", Str().Opt().D("Only guests on this node")),
			P("status", Enum("running", "stopped", "paused", "suspended").Opt().D("Only guests in this state")),
			P("tag", Str().Opt().D("Only guests with this tag")),
			P("pool", Str().Opt().D("Only guests in this resource pool")),
			P("templates", Enum("include", "exclude", "only").Opt().D("How to treat templates (default include)")),
			P("search", searchArg),
			P("sort", Enum("vmid", "name", "cpu", "memory", "disk", "uptime").Opt().D("Sort order (default vmid); cpu/memory/disk/uptime sort highest first")),
			P("limit", limitArg(500)),
			P("raw", rawArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			guests, err := client.GetList(ctx, "/cluster/resources", js.O("type", "vm"))
			if err != nil {
				return nil, err
			}
			if a.Truthy("type") {
				want := pick(a.Get("type") == "vm", "qemu", "lxc")
				guests = filterObjs(guests, func(g *Obj) bool { return g.Get("type") == want })
			}
			if a.Truthy("node") {
				guests = filterObjs(guests, func(g *Obj) bool { return g.Get("node") == a.Get("node") })
			}
			if a.Truthy("status") {
				guests = filterObjs(guests, func(g *Obj) bool { return g.Get("status") == a.Get("status") })
			}
			if a.Truthy("tag") {
				tag := strings.ToLower(a.Str("tag"))
				guests = filterObjs(guests, func(g *Obj) bool {
					return contains(tagSplitRe.Split(strings.ToLower(js.Str(or(g.Get("tags"), ""))), -1), tag)
				})
			}
			if a.Truthy("pool") {
				guests = filterObjs(guests, func(g *Obj) bool { return g.Get("pool") == a.Get("pool") })
			}
			switch a.Get("templates") {
			case "exclude":
				guests = filterObjs(guests, func(g *Obj) bool { return !g.Truthy("template") })
			case "only":
				guests = filterObjs(guests, func(g *Obj) bool { return g.Truthy("template") })
			}
			numOr0 := func(v float64) float64 {
				if js.Truthy(v) {
					return v
				}
				return 0
			}
			sortKey := js.Str(or(a.Get("sort"), "vmid"))
			if sortKey == "name" {
				sortObjs(guests, func(x, y *Obj) float64 {
					return compareStr(or(x.Get("name"), ""), or(y.Get("name"), ""))
				})
			} else {
				key := map[string]func(*Obj) float64{
					"vmid":   func(g *Obj) float64 { return g.Num("vmid") },
					"cpu":    func(g *Obj) float64 { return -numOr0(g.Num("cpu")) },
					"memory": func(g *Obj) float64 { return -numOr0(g.Num("mem")) },
					"disk": func(g *Obj) float64 {
						if d := g.Num("disk"); js.Truthy(d) {
							return -d
						}
						return -numOr0(g.Num("maxdisk"))
					},
					"uptime": func(g *Obj) float64 { return -numOr0(g.Num("uptime")) },
				}[sortKey]
				sortObjs(guests, func(x, y *Obj) float64 { return key(x) - key(y) })
			}
			res := filterAndLimit(guests, a.Get("search"), or(a.Get("limit"), 500), func(g *Obj) []any {
				return []any{g.Get("vmid"), g.Get("name"), g.Get("tags"), g.Get("node"), g.Get("pool")}
			})
			if a.Truthy("raw") {
				return res.Obj(proxmox.RedactSecrets(js.Array(res.Items))), nil
			}
			return res.Map(func(g *Obj) any { return summarizeGuest(g) }), nil
		},
	})

	c.Define("proxmox_list_resources", Def{
		Toolset:     "cluster",
		List:        &listing{},
		Title:       "List cluster resources",
		Description: "List cluster resources of any kind from /cluster/resources: nodes, guests, storage, pools and SDN zones. Use proxmox_list_guests for a friendlier guest list.",
		Input: []Prop{
			P("type", Enum("node", "vm", "storage", "pool", "sdn").Opt().D("Resource type (default all)")),
			P("search", searchArg),
			P("limit", limitArg(1000)),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			items, err := client.GetList(ctx, "/cluster/resources", defined(js.O("type", a.Get("type"))))
			if err != nil {
				return nil, err
			}
			res := filterAndLimit(items, a.Get("search"), or(a.Get("limit"), 1000), func(r *Obj) []any {
				return []any{r.Get("id"), r.Get("name"), r.Get("node"), r.Get("storage"), r.Get("pool"), r.Get("tags")}
			})
			return res.Map(func(r *Obj) any { return proxmox.Compact(r) }), nil
		},
	})

	c.Define("proxmox_get_next_vmid", Def{
		Toolset:     "cluster",
		Title:       "Get next free VMID",
		Description: "Get the next free VM/container ID, or check whether a specific ID is free.",
		Input:       []Prop{P("vmid", Int().Min(100).Opt().D("Check whether this ID is free instead"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			id, err := client.Get(ctx, "/cluster/nextid", defined(js.O("vmid", a.Get("vmid"))))
			if err != nil {
				if a.Defined("vmid") {
					return js.O("vmid", a.Get("vmid"), "free", false, "reason", errMessage(err)), nil
				}
				return nil, err
			}
			return js.O("vmid", js.Num(id), "free", true), nil
		},
	})

	c.Define("proxmox_get_cluster_log", Def{
		Toolset:     "cluster",
		Title:       "Get cluster log",
		Description: "Read recent entries of the cluster log (task starts/ends, logins and other cluster events from all nodes).",
		Input: []Prop{
			P("max", Int().Pos().Max(5000).Opt().D("Maximum number of entries (default 50)")),
			P("search", searchArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			entries, err := client.GetList(ctx, "/cluster/log", js.O("max", or(a.Get("max"), 50)))
			if err != nil {
				return nil, err
			}
			res := filterAndLimit(entries, a.Get("search"), js.Undef, func(e *Obj) []any {
				return []any{e.Get("msg"), e.Get("node"), e.Get("user"), e.Get("tag")}
			})
			return res.Map(func(e *Obj) any {
				return proxmox.C("time", proxmox.IsoTime(e.Get("time")), "node", e.Get("node"), "user", e.Get("user"), "tag", e.Get("tag"), "severity", e.Get("pri"), "msg", e.Get("msg"))
			}), nil
		},
	})

	c.Define("proxmox_get_cluster_options", Def{
		Toolset:     "cluster",
		Title:       "Get datacenter options",
		Description: "Get datacenter-wide options (datacenter.cfg): keyboard, console, migration network/type, HA shutdown policy, bandwidth limits, MAC prefix, tag style, next-id range and more.",
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			v, err := client.Get(ctx, "/cluster/options", nil)
			if err != nil {
				return nil, err
			}
			return clusterRedactConfig(v), nil
		},
	})

	c.Define("proxmox_get_cluster_config", Def{
		Toolset:     "cluster",
		Title:       "Get cluster (corosync) configuration",
		Description: "Get the corosync cluster configuration: member nodes with their IDs, votes and link addresses, the totem settings, and the join information (fingerprint, addresses) used to add nodes.",
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			orError := func(path string) any {
				v, err := client.Get(ctx, path, nil)
				if err != nil {
					return js.O("error", errMessage(err))
				}
				return v
			}
			nodes := orError("/cluster/config/nodes")
			totem := orError("/cluster/config/totem")
			join := orError("/cluster/config/join")
			qdevice, err := client.Get(ctx, "/cluster/config/qdevice", nil)
			if err != nil {
				qdevice = js.Undef
			}
			return proxmox.C("nodes", nodes, "totem", totem, "join", proxmox.RedactSecrets(join), "qdevice", qdevice), nil
		},
	})

	c.Define("proxmox_list_metric_servers", Def{
		Toolset:     "cluster",
		List:        &listing{},
		Title:       "List metric servers",
		Description: "List external metric servers (InfluxDB, Graphite, OpenTelemetry) that Proxmox sends performance data to.",
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			v, err := client.Get(ctx, "/cluster/metrics/server", nil)
			if err != nil {
				return nil, err
			}
			return clusterRedactConfig(v), nil
		},
	})

	c.Define("proxmox_simulate_schedule", Def{
		Toolset:     "cluster",
		Title:       "Simulate a job schedule",
		Description: "Show the next run times of a Proxmox calendar-event schedule (as used by backup, replication and other jobs), e.g. 'mon..fri 02:00' or '*/15'. Useful to check a schedule before saving it.",
		Input: []Prop{
			P("schedule", Str().D("Calendar event, e.g. 'daily', 'sat 03:30', '*/2:00'")),
			P("iterations", Int().Pos().Max(100).Opt().D("How many run times to show (default 10)")),
			P("starttime", Int().Opt().D("Start from this UNIX time instead of now")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			runs, err := client.GetList(ctx, "/cluster/jobs/schedule-analyze", defined(js.O("schedule", a.Get("schedule"), "iterations", or(a.Get("iterations"), 10), "starttime", a.Get("starttime"))))
			if err != nil {
				return nil, err
			}
			return mapObjs(runs, func(r *Obj) any { return or(proxmox.IsoTime(r.Get("timestamp")), r.Get("utc")) }), nil
		},
	})

	// ---------------------------------------------------------------- write

	c.Define("proxmox_update_cluster_options", Def{
		Toolset:     "cluster",
		Title:       "Update datacenter options",
		Description: "Change datacenter-wide options (datacenter.cfg). Only the given fields change. Common ones are typed; anything else (bwlimit, ha, crs, migration, next-id, tag-style, u2f, webauthn, notify...) can be set via `extra` in Proxmox's property-string format.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("keyboard", Str().Opt().D("Default VNC keyboard layout, e.g. en-gb")),
			P("language", Str().Opt().D("Default GUI language, e.g. en")),
			P("console", Enum("applet", "vv", "html5", "xtermjs").Opt().D("Default console viewer")),
			P("email_from", Str().Opt().D("Sender address for notification emails")),
			P("http_proxy", Str().Opt().D("Proxy for downloads (e.g. http://proxy:3128)")),
			P("mac_prefix", Str().Opt().D("Prefix for auto-generated MAC addresses")),
			P("max_workers", Int().Pos().Opt().D("Maximum parallel workers for bulk actions")),
			P("migration", Str().Opt().D("Migration settings, e.g. 'type=insecure,network=10.0.0.0/24'")),
			P("ha", Str().Opt().D("HA settings, e.g. 'shutdown_policy=migrate'")),
			P("description", Str().Opt().D("Datacenter notes (Markdown)")),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			params, err := definedExtra(js.O(
				"keyboard", a.Get("keyboard"),
				"language", a.Get("language"),
				"console", a.Get("console"),
				"email_from", a.Get("email_from"),
				"http_proxy", a.Get("http_proxy"),
				"mac_prefix", a.Get("mac_prefix"),
				"max_workers", a.Get("max_workers"),
				"migration", a.Get("migration"),
				"ha", a.Get("ha"),
				"description", a.Get("description"),
				"delete", deleteParam(a.Get("delete")),
				"digest", a.Get("digest"),
			), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			if _, err := client.Put(ctx, "/cluster/options", params); err != nil {
				return nil, err
			}
			opts, err := client.Get(ctx, "/cluster/options", nil)
			if err != nil {
				return nil, err
			}
			return js.O("updated", true, "options", clusterRedactConfig(opts)), nil
		},
	})

	c.Define("proxmox_save_metric_server", Def{
		Toolset:     "cluster",
		Title:       "Create or update a metric server",
		Description: "Create (create=true) or update an external metric server for InfluxDB, Graphite or OpenTelemetry. Only the given fields change on update.",
		Write:       true,
		Input: []Prop{
			P("id", Str().D("Metric server ID")),
			P("create", Bool().Opt().D("Create a new server (default false = update an existing one)")),
			P("type", Enum("influxdb", "graphite", "opentelemetry").Opt().D("Server type (required when creating)")),
			P("server", Str().Opt().D("Server address (required when creating)")),
			P("port", Int().Pos().Opt().D("Server port (required when creating)")),
			P("disable", Bool().Opt().D("Disable sending metrics")),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			create := a.Truthy("create")
			params, err := definedExtra(js.O("type", ifThen(create, a.Get("type")), "server", a.Get("server"), "port", a.Get("port"), "disable", a.Get("disable")), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			path := "/cluster/metrics/server/" + enc(a.Get("id"))
			if create {
				if !a.Truthy("type") || !a.Truthy("server") || !a.Truthy("port") {
					return nil, errors.New("type, server and port are required when creating a metric server")
				}
				_, err = client.Post(ctx, path, params)
			} else {
				_, err = client.Put(ctx, path, params)
			}
			if err != nil {
				return nil, err
			}
			server, err := client.Get(ctx, path, nil)
			if err != nil {
				return nil, err
			}
			return js.O("saved", a.Get("id"), "server", clusterRedactConfig(server)), nil
		},
	})

	c.Define("proxmox_delete_metric_server", Def{
		Toolset:     "cluster",
		Title:       "Delete a metric server",
		Description: "Remove an external metric server configuration.",
		Delete:      true,
		Input:       []Prop{P("id", Str().D("Metric server ID"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/cluster/metrics/server/"+enc(a.Get("id")), nil); err != nil {
				return nil, err
			}
			return js.O("deleted", a.Get("id")), nil
		},
	})

	c.Define("proxmox_bulk_guest_action", Def{
		Toolset:     "cluster",
		Title:       "Bulk start, shut down, suspend or migrate guests",
		Description: "Start, shut down, suspend or migrate many VMs and containers at once with Proxmox's bulk action (Proxmox VE 9+). Select guests with `vmids`; for migrate also give `target`. Runs as one task.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("action", Enum("start", "shutdown", "suspend", "migrate").D("What to do")),
			P("vmids", Arr(Int().Min(100)).Min(1).D("Guest IDs to act on")),
			P("target", Str().Opt().D("Target node (migrate only)")),
			P("online", Bool().Opt().D("Live-migrate running VMs and restart-migrate running containers (migrate only)")),
			P("timeout", Int().Pos().Opt().D("Shutdown timeout in seconds before giving up (shutdown only)")),
			P("force_stop", Bool().Opt().D("Hard-stop guests that don't shut down within the timeout (shutdown only)")),
			P("with_local_disks", Bool().Opt().D("Also migrate local disks of running VMs (migrate only)")),
			P("to_disk", Bool().Opt().D("Suspend to disk (hibernate) instead of to RAM (suspend only)")),
			P("max_workers", Int().Pos().Opt().D("How many guests to handle in parallel")),
			P("wait", waitArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if a.Get("action") == "migrate" && !a.Truthy("target") {
				return nil, errors.New("target is required for migrate")
			}
			params, err := definedExtra(js.O(
				"vms", a.Get("vmids"),
				"target", a.Get("target"),
				"online", a.Get("online"),
				"timeout", a.Get("timeout"),
				"force-stop", a.Get("force_stop"),
				"max-workers", a.Get("max_workers"),
				"with-local-disks", a.Get("with_local_disks"),
				"to-disk", a.Get("to_disk"),
			), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			upid, err := client.Post(ctx, "/cluster/bulk-action/guest/"+js.Str(a.Get("action")), params)
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("action", a.Get("action"), "vmids", a.Get("vmids")))
		},
	})
}
