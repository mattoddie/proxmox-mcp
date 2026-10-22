package tools

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

// ---------------------------------------------------------------- argument shapes

var (
	containersVolumeKeyArg = Str().Pattern(`^(rootfs|mp\d+)$`, "must be rootfs or mpN").D("Container volume: rootfs or a mount point key such as mp0")

	containersTagsArg = Arr(Str()).Opt().D("Tags, e.g. [\"prod\", \"dns\"] (replaces existing tags)")

	containersNetSpec = Object(
		P("name", Str().Opt().D("Interface name inside the container (default eth0, eth1... by position)")),
		P("bridge", Str().Opt().D("Host bridge (default vmbr0)")),
		P("ip", Str().Opt().D("IPv4: \"dhcp\", \"manual\" or CIDR such as \"192.168.1.50/24\"")),
		P("gw", Str().Opt().D("IPv4 gateway (with a static ip)")),
		P("ip6", Str().Opt().D("IPv6: \"auto\" (SLAAC), \"dhcp\", \"manual\" or CIDR")),
		P("gw6", Str().Opt().D("IPv6 gateway")),
		P("vlan", Int().Min(1).Max(4094).Opt().D("VLAN tag")),
		P("firewall", Bool().Opt().D("Enable the Proxmox firewall on this interface")),
		P("hwaddr", Str().Opt().D("MAC address (default: generated)")),
		P("mtu", Int().Min(64).Max(65535).Opt().D("MTU")),
		P("rate", Num().Pos().Opt().D("Rate limit in MB/s")),
		P("link_down", Bool().Opt().D("Disconnect the interface")),
	)

	containersMountSpec = Object(
		P("path", Str().Opt().D("Mount path inside the container, e.g. /data (required for new mount points)")),
		P("storage", Str().Opt().D("Storage to allocate a new volume on (with size_gb)")),
		P("size_gb", Num().Pos().Opt().D("Size of the new volume in GiB (with storage)")),
		P("volume", Str().Opt().D("Existing volume ID, or a host directory to bind-mount such as /mnt/tank/media (instead of storage+size_gb)")),
		P("backup", Bool().Opt().D("Include the volume in backups")),
		P("ro", Bool().Opt().D("Mount read-only")),
		P("acl", Bool().Opt().D("Enable/disable ACL support")),
		P("quota", Bool().Opt().D("Enable user quotas inside the container")),
		P("replicate", Bool().Opt().D("Include in storage replication jobs (default true)")),
		P("shared", Bool().Opt().D("Mark a non-volume mount as available on all nodes")),
		P("mountoptions", Str().Opt().D("Extra mount options separated by ';', e.g. noatime;nodev")),
	)

	containersFeaturesSpec = Object(
		P("nesting", Bool().Opt().D("Allow nested containers/namespaces (needed by systemd in modern distros and by Docker)")),
		P("keyctl", Bool().Opt().D("Allow the keyctl() syscall (unprivileged only; needed by Docker)")),
		P("fuse", Bool().Opt().D("Allow FUSE mounts")),
		P("mknod", Bool().Opt().D("Allow mknod for some device nodes (experimental)")),
		P("force_rw_sys", Bool().Opt().D("Mount /sys read-write in unprivileged containers")),
		P("mount", Str().Opt().D("Allowed filesystem types to mount, separated by ';', e.g. nfs;cifs (privileged only)")),
	).D("Container features; booleans set to false are written as 0")
)

// ---------------------------------------------------------------- helpers

// containersNetString builds a netN property string, overlaying spec on an existing value (so
// the MAC and other options survive).
func containersNetString(spec *Obj, index int, existing any) string {
	var base *Obj
	if js.Truthy(existing) {
		base = proxmox.ParsePropertyString(existing, "")
	} else {
		base = js.O("name", fmt.Sprintf("eth%d", index), "bridge", "vmbr0")
	}
	rest := js.NewObj()
	for _, k := range spec.Keys() {
		if k != "vlan" && k != "link_down" {
			rest.Set(k, spec.Get(k))
		}
	}
	rest.Set("tag", spec.Get("vlan"))
	rest.Set("link_down", spec.Get("link_down"))
	merged := js.Spread(base, defined(rest))
	if !js.Truthy(existing) && js.IsUndef(merged.Get("ip")) && js.IsUndef(merged.Get("ip6")) {
		merged.Set("ip", "dhcp")
	}
	// The property string has no default key; keep name first for readability.
	out := js.O("name", merged.Get("name"))
	for _, k := range merged.Keys() {
		if k != "name" {
			out.Set(k, merged.Get(k))
		}
	}
	return proxmox.ToPropertyString(out, "")
}

var containersTrailingColonRe = regexp.MustCompile(`:$`)

func containersMountString(spec *Obj, key string, existing any) (string, error) {
	base := js.NewObj()
	if js.Truthy(existing) {
		base = proxmox.ParsePropertyString(existing, "volume")
	}
	volume := spec.Get("volume")
	if js.Nullish(volume) {
		if js.Truthy(spec.Get("storage")) {
			volume = js.Str(spec.Get("storage")) + ":" + js.Str(or(spec.Get("size_gb"), ""))
		} else {
			volume = base.Get("volume")
		}
	}
	if !js.Truthy(volume) || containersTrailingColonRe.MatchString(js.Str(volume)) {
		return "", fmt.Errorf("%s: give either volume, or storage with size_gb", key)
	}
	path := or(spec.Get("path"), base.Get("mp"))
	if !js.Truthy(path) && key != "rootfs" {
		return "", fmt.Errorf("%s: path is required for a new mount point", key)
	}
	opts := js.NewObj()
	for _, k := range spec.Keys() {
		switch k {
		case "path", "storage", "size_gb", "volume":
		default:
			opts.Set(k, spec.Get(k))
		}
	}
	return proxmox.ToPropertyString(js.Spread(base, js.O("volume", volume, "mp", ifThen(key != "rootfs", path)), defined(opts)), "volume"), nil
}

func containersFeaturesString(f *Obj, existing any) string {
	return proxmox.ToPropertyString(js.Spread(proxmox.ParsePropertyString(existing, ""), defined(f)), "")
}

func containersParseFeatures(value any) any {
	if !js.Truthy(value) {
		return js.Undef
	}
	p := proxmox.ParsePropertyString(value, "")
	out := js.NewObj()
	for _, k := range p.Keys() {
		if k == "mount" {
			out.Set(k, p.Get(k))
		} else {
			out.Set(k, proxmox.Flag(p.Get(k)))
		}
	}
	return out
}

func containersSplitTags(tags any) any {
	if !js.Truthy(tags) {
		return js.Undef
	}
	return splitTags(js.Str(tags))
}

// containersPendingChanges lists pending changes from /pending: keys whose new value applies on
// the next restart, or that will be deleted.
func containersPendingChanges(items []*Obj) []any {
	out := []any{}
	for _, p := range items {
		if !js.IsUndef(p.Get("pending")) || p.Truthy("delete") {
			out = append(out, containersPendingEntry(p, true))
		}
	}
	return out
}

// containersPendingEntry is one /pending entry, with secret-looking values (by config key name) redacted.
func containersPendingEntry(p *Obj, changesOnly bool) *Obj {
	key := js.Str(p.Get("key"))
	hide := func(v any) any {
		if js.IsUndef(v) {
			return js.Undef
		}
		return proxmox.RedactSecrets(js.O(key, v)).(*Obj).Get(key)
	}
	valueKey := "value"
	if changesOnly {
		valueKey = "current"
	}
	return proxmox.C(
		"key", p.Get("key"),
		valueKey, hide(p.Get("value")),
		"pending", hide(p.Get("pending")),
		"delete", ifThen(p.Truthy("delete"), true),
	)
}

// containersSummarizeInterfaces lists live addresses from /interfaces (the format differs
// slightly between versions).
func containersSummarizeInterfaces(ifaces []*Obj) []any {
	out := []any{}
	for _, i := range ifaces {
		if i.Get("name") == "lo" {
			continue
		}
		var addrs []string
		for _, a := range js.Objs(or(i.Get("ip-addresses"), []any{})) {
			s := js.Str(a.Get("ip-address"))
			if !js.IsUndef(a.Get("prefix")) {
				s += "/" + js.Str(a.Get("prefix"))
			}
			addrs = append(addrs, s)
		}
		for _, k := range []string{"inet", "inet6"} {
			v := i.Get(k)
			if arr, ok := v.([]any); ok {
				for _, x := range arr {
					addrs = append(addrs, js.Str(x))
				}
			} else if js.Truthy(v) {
				addrs = append(addrs, js.Str(v))
			}
		}
		seen := map[string]bool{}
		unique := []any{}
		for _, a := range addrs {
			if !seen[a] {
				seen[a] = true
				unique = append(unique, a)
			}
		}
		out = append(out, proxmox.C("name", i.Get("name"), "hwaddr", or(i.Get("hwaddr"), i.Get("hardware-address")), "addresses", unique))
	}
	return out
}

var containersDevKeyRe = regexp.MustCompile(`^dev\d+$`)

func containersSummarizeConfig(c *Obj) *Obj {
	devices := js.NewObj()
	for _, k := range c.Keys() {
		if containersDevKeyRe.MatchString(k) {
			devices.Set(k, c.Get(k))
		}
	}
	return proxmox.C(
		"hostname", c.Get("hostname"),
		"description", c.Get("description"),
		"arch", c.Get("arch"),
		"ostype", c.Get("ostype"),
		"unprivileged", or(proxmox.Flag(c.Get("unprivileged")), false),
		"template", orUndef(proxmox.Flag(c.Get("template"))),
		"protection", orUndef(proxmox.Flag(c.Get("protection"))),
		"onboot", or(proxmox.Flag(c.Get("onboot")), false),
		"startup", c.Get("startup"),
		"lock", c.Get("lock"),
		"cores", or(c.Get("cores"), "all host cores"),
		"cpulimit", c.Get("cpulimit"),
		"cpuunits", c.Get("cpuunits"),
		"memory", proxmox.FormatBytes(js.Num(or(c.Get("memory"), 512.0))*1024*1024),
		"swap", proxmox.FormatBytes(js.Num(or(c.Get("swap"), 512.0))*1024*1024),
		"features", containersParseFeatures(c.Get("features")),
		"volumes", proxmox.SummarizeDisks(c),
		"nets", proxmox.SummarizeNets(c),
		"nameserver", c.Get("nameserver"),
		"searchdomain", c.Get("searchdomain"),
		"timezone", c.Get("timezone"),
		"tags", containersSplitTags(c.Get("tags")),
		"hookscript", c.Get("hookscript"),
		"devices", devices,
		"digest", c.Get("digest"),
	)
}

func containersSummarizeStatus(s *Obj) *Obj {
	running := s.Get("status") == "running"
	var memory any
	if running {
		memory = proxmox.Usage(s.Get("mem"), s.Get("maxmem"))
	} else {
		memory = proxmox.FormatBytes(s.Get("maxmem"))
	}
	var rootfs any = js.Undef
	if s.Truthy("maxdisk") {
		if s.Truthy("disk") {
			rootfs = proxmox.Usage(s.Get("disk"), s.Get("maxdisk"))
		} else {
			rootfs = proxmox.FormatBytes(s.Get("maxdisk"))
		}
	}
	return proxmox.C(
		"status", s.Get("status"),
		"lock", s.Get("lock"),
		"ha", ifThen(s.ObjAt("ha").Truthy("managed"), s.Get("ha")),
		"pid", ifThen(running, s.Get("pid")),
		"uptime", ifThen(running, proxmox.FormatUptime(s.Get("uptime"))),
		"cpus", s.Get("cpus"),
		"cpu_pct", ifThen(running, proxmox.FractionPct(s.Get("cpu"))),
		"memory", memory,
		"swap", ifThen(running && s.Truthy("maxswap"), proxmox.Usage(s.Get("swap"), s.Get("maxswap"))),
		"rootfs", rootfs,
		"net_in", ifThen(running, proxmox.FormatBytes(s.Get("netin"))),
		"net_out", ifThen(running, proxmox.FormatBytes(s.Get("netout"))),
		"disk_read", ifThen(running, proxmox.FormatBytes(s.Get("diskread"))),
		"disk_write", ifThen(running, proxmox.FormatBytes(s.Get("diskwrite"))),
	)
}

func containersSeriesStats(points []*Obj, key string, f func(float64) any) any {
	var vals []float64
	for _, p := range points {
		if v := p.Num(key); js.IsFinite(v) {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return js.Undef
	}
	sum, mx := 0.0, vals[0]
	for _, v := range vals {
		sum += v
		if v > mx {
			mx = v
		}
	}
	return js.O("avg", f(sum/float64(len(vals))), "max", f(mx), "latest", f(vals[len(vals)-1]))
}

func containersRate(v float64) any {
	return js.Str(or(proxmox.FormatBytes(js.Round(v)), "0 B")) + "/s"
}

func containersBytes(v float64) any { return proxmox.FormatBytes(js.Round(v)) }

func containersNextVmid(ctx context.Context, c *Context) (any, error) {
	v, err := c.Client.Get(ctx, "/cluster/nextid", nil)
	if err != nil {
		return nil, err
	}
	return js.Num(v), nil
}

// containersJoinTags is tags?.join(";").
func containersJoinTags(tags any) any {
	arr, ok := tags.([]any)
	if !ok {
		return js.Undef
	}
	parts := make([]string, len(arr))
	for i, t := range arr {
		parts[i] = js.Str(t)
	}
	return strings.Join(parts, ";")
}

func registerContainers(c *Context) {
	client := c.Client
	ct := func(ctx context.Context, a *Obj) (proxmox.GuestRef, error) { return c.guest(ctx, a, "lxc") }

	// ---------------------------------------------------------------- read

	c.Define("proxmox_list_containers", Def{
		Toolset:     "containers",
		List:        &listing{},
		Title:       "List containers",
		Description: "List LXC containers across the cluster with status, node, CPU, memory, disk, uptime, tags and pool. For VMs and containers together use proxmox_list_guests.",
		Input: []Prop{
			P("node", Str().Opt().D("Only containers on this node")),
			P("status", Enum("running", "stopped").Opt().D("Only containers in this state")),
			P("templates", Enum("include", "exclude", "only").Opt().D("How to treat templates (default include)")),
			P("search", searchArg),
			P("limit", limitArg(500)),
			P("raw", rawArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			all, err := client.GetList(ctx, "/cluster/resources", js.O("type", "vm"))
			if err != nil {
				return nil, err
			}
			items := filterObjs(all, func(r *Obj) bool { return r.Get("type") == "lxc" })
			if a.Truthy("node") {
				items = filterObjs(items, func(r *Obj) bool { return r.Get("node") == a.Get("node") })
			}
			if a.Truthy("status") {
				items = filterObjs(items, func(r *Obj) bool { return r.Get("status") == a.Get("status") })
			}
			switch a.Get("templates") {
			case "exclude":
				items = filterObjs(items, func(r *Obj) bool { return !r.Truthy("template") })
			case "only":
				items = filterObjs(items, func(r *Obj) bool { return r.Truthy("template") })
			}
			sortObjs(items, func(x, y *Obj) float64 { return x.Num("vmid") - y.Num("vmid") })
			res := filterAndLimit(items, a.Get("search"), or(a.Get("limit"), 500), func(r *Obj) []any {
				return []any{r.Get("vmid"), r.Get("name"), r.Get("tags"), r.Get("node"), r.Get("pool")}
			})
			if a.Truthy("raw") {
				return res.Obj(proxmox.RedactSecrets(js.Array(res.Items))), nil
			}
			return res.Map(func(r *Obj) any { return summarizeGuest(r) }), nil
		},
	})

	c.Define("proxmox_get_container", Def{
		Toolset:     "containers",
		Title:       "Get container details",
		Description: "Get an LXC container's live status (CPU, memory, swap, rootfs usage, uptime) together with a summary of its configuration: hostname, OS, cores, memory, swap, rootfs and mount points, network interfaces with configured IPs, live IP addresses (when running), features, unprivileged, onboot, tags and pending changes that apply on next restart. Use proxmox_get_container_config for the full raw config.",
		Input:       []Prop{P("vmid", vmidArg), P("node", guestNodeArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := ct(ctx, a)
			if err != nil {
				return nil, err
			}
			base := client.GuestPath(g)
			status, err := client.GetObj(ctx, base+"/status/current", nil)
			if err != nil {
				return nil, err
			}
			config, err := client.GetObj(ctx, base+"/config", nil)
			if err != nil {
				return nil, err
			}
			pending, err := client.GetList(ctx, base+"/pending", nil)
			if err != nil {
				pending = []*Obj{}
			}
			var ifaces any = js.Undef
			if status.Get("status") == "running" {
				if v, err := client.Get(ctx, base+"/interfaces", nil); err == nil {
					ifaces = v
				}
			}
			var ipAddresses any = js.Undef
			if js.Truthy(ifaces) {
				ipAddresses = containersSummarizeInterfaces(js.Objs(ifaces))
			}
			return proxmox.Compact(js.Spread(
				js.O(
					"vmid", g.VMID,
					"name", or(status.Get("name"), config.Get("hostname")),
					"node", g.Node,
					"type", "container",
					"template", orUndef(proxmox.Flag(status.Get("template"))),
				),
				containersSummarizeStatus(status),
				js.O(
					"ip_addresses", ipAddresses,
					"config", containersSummarizeConfig(config),
					"pending_changes", containersPendingChanges(pending),
				),
			)), nil
		},
	})

	c.Define("proxmox_get_container_config", Def{
		Toolset:     "containers",
		Title:       "Get container configuration",
		Description: "Get an LXC container's full configuration as stored by Proxmox (secrets redacted). Options: current=true for the running values instead of pending ones, snapshot to read the config as saved in a snapshot, or pending=true to list each key with its current and pending value. The returned digest can be passed to proxmox_update_container_config for optimistic locking.",
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("current", Bool().Opt().D("Return current values instead of values including pending changes")),
			P("snapshot", Str().Opt().D("Read the config saved in this snapshot")),
			P("pending", Bool().Opt().D("Return the key/current/pending listing from the pending endpoint instead")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := ct(ctx, a)
			if err != nil {
				return nil, err
			}
			base := client.GuestPath(g)
			if a.Truthy("pending") {
				items, err := client.GetList(ctx, base+"/pending", nil)
				if err != nil {
					return nil, err
				}
				return js.O(
					"vmid", g.VMID,
					"node", g.Node,
					"pending_changes", containersPendingChanges(items),
					"config", mapObjs(items, func(p *Obj) any { return containersPendingEntry(p, false) }),
				), nil
			}
			config, err := client.Get(ctx, base+"/config", defined(js.O("current", a.Get("current"), "snapshot", a.Get("snapshot"))))
			if err != nil {
				return nil, err
			}
			return js.O("vmid", g.VMID, "node", g.Node, "config", proxmox.RedactSecrets(config)), nil
		},
	})

	c.Define("proxmox_get_container_interfaces", Def{
		Toolset:     "containers",
		Title:       "Get container IP addresses",
		Description: "Get the live network interfaces and IP addresses (IPv4 and IPv6) inside a running LXC container, including addresses obtained via DHCP/SLAAC that the config doesn't show. The container must be running.",
		Input:       []Prop{P("vmid", vmidArg), P("node", guestNodeArg), P("raw", rawArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := ct(ctx, a)
			if err != nil {
				return nil, err
			}
			if js.Truthy(g.Status) && g.Status != "running" {
				return nil, fmt.Errorf("Container %s is %s; it must be running to read live interfaces", js.NumStr(g.VMID), js.Str(g.Status))
			}
			ifaces, err := client.Get(ctx, client.GuestPath(g)+"/interfaces", nil)
			if err != nil {
				return nil, err
			}
			var out any = ifaces
			if !a.Truthy("raw") {
				out = containersSummarizeInterfaces(js.Objs(ifaces))
			}
			return js.O("vmid", g.VMID, "node", g.Node, "interfaces", out), nil
		},
	})

	c.Define("proxmox_get_container_stats", Def{
		Toolset:     "containers",
		Title:       "Get container performance history",
		Description: "Summarize an LXC container's performance history from RRD data: average, maximum and latest CPU %, memory, swap, disk usage, network and disk I/O rates over the chosen timeframe. Set points=true to also get the individual data points.",
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("timeframe", Enum("hour", "day", "week", "month", "year").Opt().D("Time window (default hour)")),
			P("cf", Enum("AVERAGE", "MAX").Opt().D("RRD consolidation function (default AVERAGE)")),
			P("points", Bool().Opt().D("Also return the data points (compacted)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := ct(ctx, a)
			if err != nil {
				return nil, err
			}
			timeframe := or(a.Get("timeframe"), "hour")
			data, err := client.GetList(ctx, client.GuestPath(g)+"/rrddata", defined(js.O("timeframe", timeframe, "cf", a.Get("cf"))))
			if err != nil {
				return nil, err
			}
			pts := filterObjs(data, func(p *Obj) bool { return !js.IsUndef(p.Get("time")) && p.Len() > 1 })
			last := js.NewObj()
			var first *Obj
			if len(pts) > 0 {
				last = pts[len(pts)-1]
				first = pts[0]
			}
			var points any = js.Undef
			if a.Truthy("points") {
				points = mapObjs(pts, func(p *Obj) any {
					rate := func(k string) any {
						if js.IsUndef(p.Get(k)) {
							return js.Undef
						}
						return containersRate(p.Num(k))
					}
					return proxmox.C(
						"time", proxmox.IsoTime(p.Get("time")),
						"cpu_pct", proxmox.FractionPct(p.Get("cpu")),
						"mem", proxmox.FormatBytes(p.Get("mem")),
						"netin", rate("netin"),
						"netout", rate("netout"),
					)
				})
			}
			return proxmox.C(
				"vmid", g.VMID,
				"node", g.Node,
				"timeframe", timeframe,
				"samples", len(pts),
				"from", proxmox.IsoTime(first.Get("time")),
				"to", proxmox.IsoTime(last.Get("time")),
				"cpus", last.Get("maxcpu"),
				"cpu_pct", containersSeriesStats(pts, "cpu", func(v float64) any { return proxmox.FractionPct(v) }),
				"memory", containersSeriesStats(pts, "mem", containersBytes),
				"memory_total", proxmox.FormatBytes(last.Get("maxmem")),
				"swap", containersSeriesStats(pts, "swap", containersBytes),
				"disk", containersSeriesStats(pts, "disk", containersBytes),
				"disk_total", proxmox.FormatBytes(last.Get("maxdisk")),
				"net_in", containersSeriesStats(pts, "netin", containersRate),
				"net_out", containersSeriesStats(pts, "netout", containersRate),
				"disk_read", containersSeriesStats(pts, "diskread", containersRate),
				"disk_write", containersSeriesStats(pts, "diskwrite", containersRate),
				"points", points,
			), nil
		},
	})

	c.Define("proxmox_check_container_feature", Def{
		Toolset:     "containers",
		Title:       "Check container feature support",
		Description: "Check whether a container's storage supports snapshots, cloning (linked) or copying (full clone), optionally from a given snapshot, and on which nodes. Useful before proxmox_create_snapshot or proxmox_clone_container.",
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("feature", Enum("snapshot", "clone", "copy").D("Feature to check")),
			P("snapname", Str().Opt().D("Check from this snapshot")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := ct(ctx, a)
			if err != nil {
				return nil, err
			}
			v, err := client.Get(ctx, client.GuestPath(g)+"/feature", defined(js.O("feature", a.Get("feature"), "snapname", a.Get("snapname"))))
			if err != nil {
				return nil, err
			}
			res, _ := v.(*Obj)
			return proxmox.C("vmid", g.VMID, "node", g.Node, "feature", a.Get("feature"), "available", res.Truthy("hasFeature"), "nodes", res.Get("nodes")), nil
		},
	})

	c.Define("proxmox_check_container_migration", Def{
		Toolset:     "containers",
		Title:       "Check container migration preconditions",
		Description: "Check whether an LXC container can be migrated, and to which nodes: reports running state, local disks/mount points that block migration, and allowed/not-allowed target nodes. Call before proxmox_migrate_container.",
		Input:       []Prop{P("vmid", vmidArg), P("node", guestNodeArg), P("target", Str().Opt().D("Target node to check"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := ct(ctx, a)
			if err != nil {
				return nil, err
			}
			res, err := client.Get(ctx, client.GuestPath(g)+"/migrate", defined(js.O("target", a.Get("target"))))
			if err != nil {
				return nil, err
			}
			return js.Spread(js.O("vmid", g.VMID, "node", g.Node), res), nil
		},
	})

	// ---------------------------------------------------------------- write

	c.Define("proxmox_container_power", Def{
		Toolset:     "containers",
		Title:       "Start, stop or reboot a container",
		Description: "Change an LXC container's power state: start, stop (immediate, like pulling the plug), shutdown (clean, with optional timeout and force_stop), reboot (clean shutdown and start; applies pending changes), suspend or resume (experimental for containers). Returns once the task finishes unless wait=false.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("action", Enum("start", "stop", "shutdown", "reboot", "suspend", "resume").D("Power action")),
			P("timeout", Int().Pos().Opt().D("Seconds to wait for a clean shutdown (shutdown/reboot; Proxmox default 60)")),
			P("force_stop", Bool().Opt().D("Hard-stop if the clean shutdown doesn't finish within the timeout (shutdown only)")),
			P("overrule_shutdown", Bool().Opt().D("Abort a running shutdown task before stopping (stop only)")),
			P("skiplock", Bool().Opt().D("Ignore locks (start/stop; root only)")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := ct(ctx, a)
			if err != nil {
				return nil, err
			}
			action := js.Str(a.Get("action"))
			if g.Template && action == "start" {
				return nil, fmt.Errorf("Container %s is a template and cannot be started; clone it first", js.NumStr(g.VMID))
			}
			params := js.NewObj()
			switch action {
			case "shutdown":
				params.Assign(defined(js.O("timeout", a.Get("timeout"), "forceStop", a.Get("force_stop"))))
			case "reboot":
				params.Assign(defined(js.O("timeout", a.Get("timeout"))))
			case "stop":
				params.Assign(defined(js.O("overrule-shutdown", a.Get("overrule_shutdown"), "skiplock", a.Get("skiplock"))))
			case "start":
				params.Assign(defined(js.O("skiplock", a.Get("skiplock"))))
			}
			upid, err := client.Post(ctx, client.GuestPath(g)+"/status/"+action, params)
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("vmid", g.VMID, "node", g.Node, "action", action))
		},
	})

	c.Define("proxmox_create_container", Def{
		Toolset:     "containers",
		Title:       "Create a container",
		Description: "Create an LXC container from an OS template (a vztmpl volume such as local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst; list a storage's vztmpl content to find one). The VMID defaults to the next free ID. The root disk is allocated on rootfs_storage (default size 8 GiB). Without `nets` one interface eth0 on vmbr0 with DHCP is added; pass nets: [] for none. A new interface defaults to ip=dhcp unless ip or ip6 is given. Set password and/or ssh_public_keys for root access. Unprivileged containers are the Proxmox default for new containers; features.nesting is recommended for systemd-based distros. Anything else via `extra` (e.g. dev0, cpulimit; hookscript needs exec permission).",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("node", nodeArg),
			P("vmid", Int().Min(100).Opt().D("VMID for the new container (default: next free ID)")),
			P("ostemplate", Str().D("OS template volume, e.g. local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst")),
			P("hostname", Str().Opt().D("Hostname")),
			P("password", Str().Opt().D("Root password inside the container")),
			P("ssh_public_keys", Str().Opt().D("Root's authorized SSH public keys (OpenSSH format, one per line)")),
			P("rootfs_storage", Str().Opt().D("Storage for the root disk, e.g. local-lvm (default: Proxmox's default storage `local`)")),
			P("rootfs_size_gb", Num().Pos().Opt().D("Root disk size in GiB (default 8)")),
			P("mount_points", Arr(containersMountSpec).Opt().D("Additional mount points, assigned to mp0, mp1... in order")),
			P("nets", Arr(containersNetSpec).Opt().D("Network interfaces, assigned to net0, net1... in order")),
			P("cores", Int().Pos().Opt().D("CPU cores (default: all host cores)")),
			P("memory", Int().Pos().Opt().D("Memory in MiB (default 512)")),
			P("swap", Int().Min(0).Opt().D("Swap in MiB (default 512)")),
			P("unprivileged", Bool().Opt().D("Run as an unprivileged container (recommended; Proxmox default for new containers)")),
			P("features", containersFeaturesSpec.Opt()),
			P("ostype", Str().Opt().D("OS type (normally detected from the template), e.g. debian, ubuntu, alpine")),
			P("arch", Enum("amd64", "i386", "arm64", "armhf", "riscv32", "riscv64").Opt().D("Architecture (default amd64)")),
			P("nameserver", Str().Opt().D("DNS server(s), space-separated (default: copied from the host)")),
			P("searchdomain", Str().Opt().D("DNS search domain(s) (default: copied from the host)")),
			P("timezone", Str().Opt().D("Time zone, e.g. Europe/London, or \"host\"")),
			P("onboot", Bool().Opt().D("Start when the node boots")),
			P("start", Bool().Opt().D("Start the container once created")),
			P("protection", Bool().Opt().D("Protect against removal")),
			P("pool", Str().Opt().D("Add to this resource pool")),
			P("tags", containersTagsArg),
			P("description", Str().Opt().D("Notes shown in the container's summary")),
			P("wait", waitArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			vmid := a.Get("vmid")
			if js.Nullish(vmid) {
				if vmid, err = containersNextVmid(ctx, c); err != nil {
					return nil, err
				}
			}
			var features any = js.Undef
			if f, ok := a.Get("features").(*Obj); ok {
				features = containersFeaturesString(f, js.Undef)
			}
			params := js.O(
				"vmid", vmid,
				"ostemplate", a.Get("ostemplate"),
				"hostname", a.Get("hostname"),
				"password", a.Get("password"),
				"ssh-public-keys", a.Get("ssh_public_keys"),
				// Always sent: without it Proxmox allocates 4 GiB on `local` rather than the documented 8 GiB.
				"rootfs", js.Str(or(a.Get("rootfs_storage"), "local"))+":"+js.Str(or(a.Get("rootfs_size_gb"), 8.0)),
				"cores", a.Get("cores"),
				"memory", a.Get("memory"),
				"swap", a.Get("swap"),
				"unprivileged", a.Get("unprivileged"),
				"features", features,
				"ostype", a.Get("ostype"),
				"arch", a.Get("arch"),
				"nameserver", a.Get("nameserver"),
				"searchdomain", a.Get("searchdomain"),
				"timezone", a.Get("timezone"),
				"onboot", a.Get("onboot"),
				"start", a.Get("start"),
				"protection", a.Get("protection"),
				"pool", a.Get("pool"),
				"tags", containersJoinTags(a.Get("tags")),
				"description", a.Get("description"),
			)
			for i, m := range js.Objs(or(a.Get("mount_points"), []any{})) {
				key := fmt.Sprintf("mp%d", i)
				s, err := containersMountString(m, key, js.Undef)
				if err != nil {
					return nil, err
				}
				params.Set(key, s)
			}
			nets := a.Get("nets")
			if js.Nullish(nets) {
				nets = []any{js.NewObj()}
			}
			for i, n := range js.Objs(nets) {
				params.Set(fmt.Sprintf("net%d", i), containersNetString(n, i, js.Undef))
			}
			// restore/force/unique turn a create into a backup restore, which with force overwrites an
			// existing container; use proxmox_restore_backup.
			body, err := definedExtra(params, a.Get("extra"), "restore", "force", "unique", "ignore-unpack-errors")
			if err != nil {
				return nil, err
			}
			if err := checkGuestConfigGates(c, body, ""); err != nil {
				return nil, err
			}
			upid, err := client.Post(ctx, "/nodes/"+js.EncodeURIComponent(node)+"/lxc", body)
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("vmid", vmid, "node", node, "hostname", a.Get("hostname")))
		},
	})

	c.Define("proxmox_clone_container", Def{
		Toolset:     "containers",
		Title:       "Clone a container",
		Description: "Clone an LXC container or container template to a new VMID (default: next free ID). Templates get a linked clone unless full=true; normal containers are always fully copied (stop the source, or clone from a snapshot with snapname, since a running container can't be copied directly). storage sets the target storage for full clones; target places the clone on another node (shared storage only).",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("vmid", vmidArg.D("Source container ID")),
			P("node", guestNodeArg),
			P("newid", Int().Min(100).Opt().D("VMID for the clone (default: next free ID)")),
			P("hostname", Str().Opt().D("Hostname of the clone")),
			P("description", Str().Opt().D("Notes for the clone")),
			P("full", Bool().Opt().D("Full copy instead of a linked clone (templates only; default linked unless storage is given)")),
			P("storage", Str().Opt().D("Target storage for a full clone")),
			P("target", Str().Opt().D("Target node (source must be on shared storage)")),
			P("pool", Str().Opt().D("Add the clone to this pool")),
			P("snapname", Str().Opt().D("Clone from this snapshot")),
			P("bwlimit", Num().Pos().Opt().D("I/O bandwidth limit in KiB/s")),
			P("wait", waitArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := ct(ctx, a)
			if err != nil {
				return nil, err
			}
			if a.Truthy("storage") && a.Get("full") == false {
				return nil, errors.New("storage only applies to full clones")
			}
			newid := a.Get("newid")
			if js.Nullish(newid) {
				if newid, err = containersNextVmid(ctx, c); err != nil {
					return nil, err
				}
			}
			params, err := definedExtra(js.O(
				"newid", newid,
				"hostname", a.Get("hostname"),
				"description", a.Get("description"),
				// Templates default to linked clones, for which Proxmox rejects storage.
				"full", or(a.Get("full"), ifThen(a.Truthy("storage"), true)),
				"storage", a.Get("storage"),
				"target", a.Get("target"),
				"pool", a.Get("pool"),
				"snapname", a.Get("snapname"),
				"bwlimit", a.Get("bwlimit"),
			), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			upid, err := client.Post(ctx, client.GuestPath(g)+"/clone", params)
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("source", g.VMID, "newid", newid, "node", or(a.Get("target"), g.Node)))
		},
	})

	c.Define("proxmox_update_container_config", Def{
		Toolset:     "containers",
		Title:       "Update container configuration",
		Description: "Change an LXC container's configuration; only the given fields change. `nets` and `mount_points` are keyed by netN/mpN and merged with the existing entry (e.g. {\"net0\": {\"ip\": \"10.0.0.5/24\", \"gw\": \"10.0.0.1\"}} keeps the bridge and MAC); `features` is merged too. New mount points with storage+size_gb are allocated. Memory, swap, cores and network changes usually apply live; others become pending until a reboot (see pending_changes in the result). Use delete to remove keys (removing an mpN volume keeps it as unusedN; deleting an unusedN key destroys the volume and needs delete permission), revert to discard pending changes, extra for anything else. To grow a disk use proxmox_resize_container_disk.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("hostname", Str().Opt().D("Hostname")),
			P("cores", Int().Pos().Opt().D("CPU cores")),
			P("cpulimit", Num().Min(0).Opt().D("CPU time limit (0 = unlimited; 2 = two cores' worth)")),
			P("cpuunits", Int().Min(0).Opt().D("CPU weight relative to other guests")),
			P("memory", Int().Pos().Opt().D("Memory in MiB")),
			P("swap", Int().Min(0).Opt().D("Swap in MiB")),
			P("nets", RecKeys(Str().Pattern(`^net\d+$`), containersNetSpec).Opt().D("Network interfaces to add or change, keyed by net0, net1...")),
			P("mount_points", RecKeys(Str().Pattern(`^mp\d+$`), containersMountSpec).Opt().D("Mount points to add or change, keyed by mp0, mp1...")),
			P("features", containersFeaturesSpec.Opt()),
			P("nameserver", Str().Opt().D("DNS server(s)")),
			P("searchdomain", Str().Opt().D("DNS search domain(s)")),
			P("timezone", Str().Opt().D("Time zone or \"host\"")),
			P("onboot", Bool().Opt().D("Start when the node boots")),
			P("startup", Str().Opt().D("Startup order/delays, e.g. \"order=2,up=30\"")),
			P("protection", Bool().Opt().D("Protect against removal")),
			P("tags", containersTagsArg),
			P("description", Str().Opt().D("Notes")),
			P("delete", deleteKeysArg),
			P("revert", Arr(Str()).Opt().D("Pending changes to discard, by key")),
			P("digest", digestArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := ct(ctx, a)
			if err != nil {
				return nil, err
			}
			base := client.GuestPath(g)
			current := js.NewObj()
			if a.Truthy("nets") || a.Truthy("mount_points") || a.Truthy("features") {
				if current, err = client.GetObj(ctx, base+"/config", nil); err != nil {
					return nil, err
				}
			}
			var features any = js.Undef
			if f, ok := a.Get("features").(*Obj); ok {
				features = containersFeaturesString(f, current.Get("features"))
			}
			typed := js.O(
				"hostname", a.Get("hostname"),
				"cores", a.Get("cores"),
				"cpulimit", a.Get("cpulimit"),
				"cpuunits", a.Get("cpuunits"),
				"memory", a.Get("memory"),
				"swap", a.Get("swap"),
				"features", features,
				"nameserver", a.Get("nameserver"),
				"searchdomain", a.Get("searchdomain"),
				"timezone", a.Get("timezone"),
				"onboot", a.Get("onboot"),
				"startup", a.Get("startup"),
				"protection", a.Get("protection"),
				"tags", containersJoinTags(a.Get("tags")),
				"description", a.Get("description"),
				"delete", deleteParam(a.Get("delete")),
				"revert", deleteParam(a.Get("revert")),
				"digest", a.Get("digest"),
			)
			if nets := a.ObjAt("nets"); nets != nil {
				for _, key := range nets.Keys() {
					typed.Set(key, containersNetString(nets.ObjAt(key), int(js.Num(key[3:])), current.Get(key)))
				}
			}
			if mps := a.ObjAt("mount_points"); mps != nil {
				for _, key := range mps.Keys() {
					s, err := containersMountString(mps.ObjAt(key), key, current.Get(key))
					if err != nil {
						return nil, err
					}
					typed.Set(key, s)
				}
			}
			params, err := definedExtra(typed, a.Get("extra"))
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
			if err := checkGuestConfigGates(c, params, ""); err != nil {
				return nil, err
			}
			if _, err := client.Put(ctx, base+"/config", params); err != nil {
				return nil, err
			}
			pending, err := client.GetList(ctx, base+"/pending", nil)
			if err != nil {
				pending = []*Obj{}
			}
			return proxmox.C("vmid", g.VMID, "node", g.Node, "updated", updated, "pending_changes", containersPendingChanges(pending)), nil
		},
	})

	c.Define("proxmox_resize_container_disk", Def{
		Toolset:     "containers",
		Title:       "Resize a container disk",
		Description: "Grow a container's rootfs or mount point volume. size is absolute (\"20G\") or relative (\"+5G\"). Shrinking is not supported. The filesystem is grown automatically, also while running.",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("disk", containersVolumeKeyArg),
			P("size", Str().Pattern(`^\+?\d+(\.\d+)?[KMGT]?$`, "e.g. 20G or +5G").D("New size, e.g. \"20G\", or an increase such as \"+5G\"")),
			P("digest", digestArg),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := ct(ctx, a)
			if err != nil {
				return nil, err
			}
			upid, err := client.Put(ctx, client.GuestPath(g)+"/resize", defined(js.O("disk", a.Get("disk"), "size", a.Get("size"), "digest", a.Get("digest"))))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("vmid", g.VMID, "node", g.Node, "disk", a.Get("disk"), "size", a.Get("size")))
		},
	})

	c.Define("proxmox_move_container_volume", Def{
		Toolset:     "containers",
		Title:       "Move a container volume",
		Description: "Move a container's rootfs or mount point to another storage (storage), or reassign a volume to another container (target_vmid, optional target_volume). The container usually must be stopped to move rootfs. By default the original is kept as an unusedN entry; delete_source=true removes it after a successful copy.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("volume", Str().Pattern(`^(rootfs|mp\d+|unused\d+)$`).D("Volume to move: rootfs, mpN or unusedN")),
			P("storage", Str().Opt().D("Target storage")),
			P("target_vmid", Int().Min(100).Opt().D("Reassign the volume to this container instead")),
			P("target_volume", Str().Opt().D("Config key on the target container (default: same key)")),
			P("delete_source", Bool().Opt().D("Delete the original volume after copying (default false: kept as unused)")),
			P("bwlimit", Num().Pos().Opt().D("I/O bandwidth limit in KiB/s")),
			P("digest", digestArg),
			P("target_digest", Str().Opt().D("Only proceed if the target container's config digest matches")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if !a.Truthy("storage") && js.IsUndef(a.Get("target_vmid")) {
				return nil, errors.New("Give storage (move to storage) or target_vmid (reassign to container)")
			}
			g, err := ct(ctx, a)
			if err != nil {
				return nil, err
			}
			upid, err := client.Post(ctx, client.GuestPath(g)+"/move_volume", defined(js.O(
				"volume", a.Get("volume"),
				"storage", a.Get("storage"),
				"target-vmid", a.Get("target_vmid"),
				"target-volume", a.Get("target_volume"),
				"delete", a.Get("delete_source"),
				"bwlimit", a.Get("bwlimit"),
				"digest", a.Get("digest"),
				"target-digest", a.Get("target_digest"),
			)))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), proxmox.C("vmid", g.VMID, "node", g.Node, "volume", a.Get("volume"), "storage", a.Get("storage"), "target_vmid", a.Get("target_vmid")))
		},
	})

	c.Define("proxmox_migrate_container", Def{
		Toolset:     "containers",
		Title:       "Migrate a container",
		Description: "Move an LXC container to another node. Containers can't be live-migrated: a running container needs restart=true (it is shut down, moved and started again on the target, with brief downtime). Stopped containers move offline. Check first with proxmox_check_container_migration. target_storage maps local volumes to storage on the target.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("target", Str().D("Target node")),
			P("restart", Bool().Opt().D("Restart migration for a running container (required if running)")),
			P("timeout", Int().Pos().Opt().D("Shutdown timeout in seconds for restart migration (default 180)")),
			P("target_storage", Str().Opt().D("Target storage, or a mapping like \"local-lvm:fast,local:local\"")),
			P("bwlimit", Num().Pos().Opt().D("I/O bandwidth limit in KiB/s")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := ct(ctx, a)
			if err != nil {
				return nil, err
			}
			if g.Node == a.Get("target") {
				return nil, fmt.Errorf("Container %s is already on %s", js.NumStr(g.VMID), js.Str(a.Get("target")))
			}
			if g.Status == "running" && !a.Truthy("restart") {
				return nil, fmt.Errorf("Container %s is running; LXC can't live-migrate. Pass restart=true for a restart migration (brief downtime), or stop it first.", js.NumStr(g.VMID))
			}
			upid, err := client.Post(ctx, client.GuestPath(g)+"/migrate", defined(js.O(
				"target", a.Get("target"), "restart", a.Get("restart"), "timeout", a.Get("timeout"), "target-storage", a.Get("target_storage"), "bwlimit", a.Get("bwlimit"),
			)))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("vmid", g.VMID, "from", g.Node, "to", a.Get("target")))
		},
	})

	c.Define("proxmox_convert_container_to_template", Def{
		Toolset:     "containers",
		Title:       "Convert a container to a template",
		Description: "Turn a stopped LXC container into a template for proxmox_clone_container. This cannot be undone: templates can't be started or edited like normal containers.",
		Write:       true,
		Destructive: True,
		Input:       []Prop{P("vmid", vmidArg), P("node", guestNodeArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := ct(ctx, a)
			if err != nil {
				return nil, err
			}
			if g.Template {
				return nil, fmt.Errorf("Container %s is already a template", js.NumStr(g.VMID))
			}
			if g.Status == "running" {
				return nil, fmt.Errorf("Container %s is running; shut it down first (proxmox_container_power)", js.NumStr(g.VMID))
			}
			res, err := client.Post(ctx, client.GuestPath(g)+"/template", nil)
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, res, true, js.O("vmid", g.VMID, "node", g.Node, "template", true))
		},
	})

	c.Define("proxmox_delete_container", Def{
		Toolset:     "containers",
		Title:       "Delete a container",
		Description: "Permanently destroy an LXC container and all its volumes. It must be stopped unless force=true. purge also removes it from backup jobs, replication and HA. Cannot be undone; consider a backup first.",
		Delete:      true,
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("purge", Bool().Opt().D("Also remove from backup jobs, replication jobs and HA")),
			P("destroy_unreferenced_disks", Bool().Opt().D("Also destroy disks with this VMID on any storage that aren't in the config")),
			P("force", Bool().Opt().D("Destroy even if running")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := ct(ctx, a)
			if err != nil {
				return nil, err
			}
			if g.Status == "running" && !a.Truthy("force") {
				return nil, fmt.Errorf("Container %s is running; stop it first or pass force=true", js.NumStr(g.VMID))
			}
			upid, err := client.Delete(ctx, client.GuestPath(g), defined(js.O(
				"purge", a.Get("purge"), "destroy-unreferenced-disks", a.Get("destroy_unreferenced_disks"), "force", a.Get("force"),
			)))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("vmid", g.VMID, "node", g.Node, "name", g.Name, "deleted", true))
		},
	})
}
