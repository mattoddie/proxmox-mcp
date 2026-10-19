package tools

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

func nodesPath(node any) string { return "/nodes/" + enc(node) }

// nodesObj returns v as an object, or an empty one (property access on anything else is undefined).
func nodesObj(v any) *Obj {
	if o, ok := v.(*Obj); ok && o != nil {
		return o
	}
	return js.NewObj()
}

// nodesTruncate cuts long text (reports, changelogs) to max characters, saying so.
func nodesTruncate(text any, max int) *Obj {
	var s string
	switch x := text.(type) {
	case string:
		s = x
	case nil, js.Undefined:
		s = ""
	default:
		s = js.Stringify(x, "")
	}
	n := js.Len16(s)
	if n <= max {
		return js.O("text", s)
	}
	return js.O("text", js.Slice16(s, max), "truncated", true, "total_chars", n,
		"note", fmt.Sprintf("Truncated to %d of %d characters; raise max_chars to see more", max, n))
}

var (
	nodesDigitsRe   = regexp.MustCompile(`^\d+$`)
	nodesOffsetRe   = regexp.MustCompile(`[zZ]|[+-]\d\d:?\d\d$`)
	nodesISODateRe  = regexp.MustCompile(`^([+-]\d{6}|\d{4})(?:-(\d{2})(?:-(\d{2}))?)?$`)
	nodesISOTimeRe  = regexp.MustCompile(`^([+-]\d{6}|\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2})(?:\.(\d{1,9}))?)?(Z|[+-]\d{2}:?\d{2})?$`)
	nodesAcmeDomRe  = regexp.MustCompile(`^acmedomain\d+$`)
	nodesKernelPkgs = regexp.MustCompile(`^(proxmox-kernel|pve-kernel|proxmox-default-kernel)`)
)

// nodesDateParse parses the ISO 8601 forms Date.parse accepts: a date alone is UTC, a date and
// time without an offset is local time.
func nodesDateParse(s string) (float64, bool) {
	atoi := func(x string) int {
		n := 0
		fmt.Sscan(x, &n)
		return n
	}
	if m := nodesISODateRe.FindStringSubmatch(s); m != nil {
		mon, day := 1, 1
		if m[2] != "" {
			mon = atoi(m[2])
		}
		if m[3] != "" {
			day = atoi(m[3])
		}
		if mon < 1 || mon > 12 || day < 1 || day > 31 {
			return 0, false
		}
		t := time.Date(atoi(m[1]), time.Month(mon), day, 0, 0, 0, 0, time.UTC)
		if t.Day() != day {
			return 0, false
		}
		return float64(t.UnixMilli()), true
	}
	m := nodesISOTimeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	mon, day, hh, mm, ss := atoi(m[2]), atoi(m[3]), atoi(m[4]), atoi(m[5]), atoi(m[6])
	if mon < 1 || mon > 12 || day < 1 || day > 31 || hh > 24 || mm > 59 || ss > 59 || (hh == 24 && (mm != 0 || ss != 0)) {
		return 0, false
	}
	ms := 0
	if m[7] != "" {
		frac := (m[7] + "00")[:3]
		ms = atoi(frac)
	}
	loc := time.Local
	if z := m[8]; z != "" {
		if z == "Z" {
			loc = time.UTC
		} else {
			z = strings.Replace(z, ":", "", 1)
			off := atoi(z[1:3])*3600 + atoi(z[3:5])*60
			if z[0] == '-' {
				off = -off
			}
			loc = time.FixedZone("", off)
		}
	}
	t := time.Date(atoi(m[1]), time.Month(mon), day, hh, mm, ss, ms*int(time.Millisecond), loc)
	if t.Day() != day && hh != 24 {
		return 0, false
	}
	return float64(t.UnixMilli()), true
}

// nodesToEpoch accepts epoch seconds (number or numeric string) or an ISO / "YYYY-MM-DD HH:MM:SS"
// date and returns epoch seconds.
func nodesToEpoch(value any) (any, error) {
	switch v := value.(type) {
	case js.Undefined:
		return js.Undef, nil
	case float64:
		return math.Floor(v), nil
	case string:
		trimmed := strings.TrimSpace(v)
		if nodesDigitsRe.MatchString(trimmed) {
			return js.Num(trimmed), nil
		}
		s := v
		if !strings.Contains(v, "T") && !nodesOffsetRe.MatchString(v) {
			s = strings.Replace(v, " ", "T", 1)
		}
		ms, ok := nodesDateParse(s)
		if !ok {
			return nil, fmt.Errorf(`Not a valid time: "%s" (use epoch seconds or e.g. 2026-01-31T12:00:00Z)`, v)
		}
		return math.Floor(ms / 1000), nil
	}
	return js.Undef, nil
}

// nodesIfaceList joins interface lists (bridge ports, bond slaves), which are space-separated in
// /etc/network/interfaces.
func nodesIfaceList(v any) any {
	arr, ok := v.([]any)
	if !ok {
		return js.Undef
	}
	parts := make([]string, len(arr))
	for i, x := range arr {
		parts[i] = js.Str(x)
	}
	return strings.Join(parts, " ")
}

func nodesSummarizeSubscription(v any) any {
	if !js.Truthy(v) {
		return js.Undef
	}
	s := nodesObj(v)
	return proxmox.C(
		"status", s.Get("status"),
		"level", s.Get("level"),
		"product", s.Get("productname"),
		"next_due_date", s.Get("nextduedate"),
		"registered", s.Get("regdate"),
		"sockets", s.Get("sockets"),
		"message", s.Get("message"),
		"last_checked", proxmox.IsoTime(s.Get("checktime")),
		// The key itself is a credential; only say whether one is set.
		"key_set", ifThen(s.Truthy("key"), true),
	)
}

// nodesSummarizeCert gives certificate details without the PEM body (public, but large and noisy).
func nodesSummarizeCert(c *Obj) any {
	notafter := c.Num("notafter")
	var daysLeft any = js.Undef
	if js.IsFinite(notafter) && notafter > 0 {
		daysLeft = math.Floor((notafter*1000 - float64(time.Now().UnixMilli())) / 86400000)
	}
	var publicKey any = js.Undef
	if c.Truthy("public-key-type") {
		bits := ""
		if c.Truthy("public-key-bits") {
			bits = " " + js.Str(c.Get("public-key-bits")) + " bits"
		}
		publicKey = js.Str(c.Get("public-key-type")) + bits
	}
	return proxmox.C(
		"filename", c.Get("filename"),
		"subject", c.Get("subject"),
		"issuer", c.Get("issuer"),
		"san", c.Get("san"),
		"not_before", proxmox.IsoTime(c.Get("notbefore")),
		"not_after", proxmox.IsoTime(c.Get("notafter")),
		"days_left", daysLeft,
		"fingerprint", c.Get("fingerprint"),
		"public_key", publicKey,
	)
}

func nodesSummarizeIface(i *Obj) any {
	boolOr := func(k string, fallback any) any {
		if i.Defined(k) {
			return js.Truthy(i.Get(k))
		}
		return fallback
	}
	addr := func(cidrKey, addrKey, maskKey string) any {
		if !js.Nullish(i.Get(cidrKey)) {
			return i.Get(cidrKey)
		}
		if !i.Truthy(addrKey) {
			return js.Undef
		}
		s := js.Str(i.Get(addrKey))
		if i.Truthy(maskKey) {
			s += "/" + js.Str(i.Get(maskKey))
		}
		return s
	}
	comments := i.Get("comments")
	if s, ok := comments.(string); ok {
		comments = strings.TrimSpace(s)
	}
	return proxmox.C(
		"iface", i.Get("iface"),
		"type", i.Get("type"),
		"active", boolOr("active", false),
		"autostart", boolOr("autostart", js.Undef),
		"exists", boolOr("exists", js.Undef),
		"method", i.Get("method"),
		"cidr", addr("cidr", "address", "netmask"),
		"gateway", i.Get("gateway"),
		"method6", i.Get("method6"),
		"cidr6", addr("cidr6", "address6", "netmask6"),
		"gateway6", i.Get("gateway6"),
		"mtu", i.Get("mtu"),
		"bridge_ports", i.Get("bridge_ports"),
		"bridge_vlan_aware", ifThen(i.Truthy("bridge_vlan_aware"), true),
		"bridge_vids", i.Get("bridge_vids"),
		"slaves", i.Get("slaves"),
		"bond_mode", i.Get("bond_mode"),
		"bond_primary", i.Get("bond-primary"),
		"bond_xmit_hash_policy", i.Get("bond_xmit_hash_policy"),
		"vlan_raw_device", i.Get("vlan-raw-device"),
		"vlan_id", i.Get("vlan-id"),
		"ovs_bridge", i.Get("ovs_bridge"),
		"ovs_ports", i.Get("ovs_ports"),
		"ovs_bonds", i.Get("ovs_bonds"),
		"ovs_tag", i.Get("ovs_tag"),
		"comments", comments,
	)
}

// ---------------------------------------------------------------- RRD statistics

var (
	// nodesByteMetricRe matches byte-valued RRD metrics (net* are bytes per second).
	nodesByteMetricRe = regexp.MustCompile(`^(mem|swap|root|arc|net)`)
	// nodesSeriesMetrics are the metrics in the default series.
	nodesSeriesMetrics = []string{"cpu", "iowait", "loadavg", "memused", "netin", "netout"}
)

// nodesFractionMetric reports metrics Proxmox gives as 0..1 fractions.
func nodesFractionMetric(name string) bool { return name == "cpu" || name == "iowait" }

func nodesFormatMetric(name string, v float64) any {
	if nodesFractionMetric(name) {
		return proxmox.FractionPct(v)
	}
	if nodesByteMetricRe.MatchString(name) {
		suffix := ""
		if strings.HasPrefix(name, "net") {
			suffix = "/s"
		}
		return js.Str(proxmox.FormatBytes(js.Round(v))) + suffix
	}
	return js.Round(v*100) / 100
}

func nodesMetricLabel(name string) string {
	if nodesFractionMetric(name) {
		return name + "_pct"
	}
	return name
}

// nodesNumericValue reports whether v counts as a number (Number.isFinite(Number(v)), not null/undefined).
func nodesNumericValue(v any) bool { return !js.Nullish(v) && js.IsFinite(js.Num(v)) }

// nodesSummarizeRrd summarises RRD rows as min/avg/max/last per metric plus a downsampled series.
func nodesSummarizeRrd(rowsAny any, metricsFilter []string, points int) *Obj {
	var valid []*Obj
	if arr, ok := rowsAny.([]any); ok {
		for _, r := range arr {
			if o, ok := r.(*Obj); ok && o != nil && js.IsFinite(o.Num("time")) {
				valid = append(valid, o)
			}
		}
	}
	var names []string
	has := map[string]bool{}
	for _, r := range valid {
		for _, k := range r.Keys() {
			v := r.Get(k)
			if k != "time" && js.IsFinite(js.Num(v)) && v != nil && !has[k] {
				has[k] = true
				names = append(names, k)
			}
		}
	}
	var wanted []string
	if len(metricsFilter) > 0 {
		for _, n := range names {
			if contains(metricsFilter, n) {
				wanted = append(wanted, n)
			}
		}
	} else {
		wanted = append([]string(nil), names...)
		sort.Strings(wanted)
	}
	values := func(rows []*Obj, name string) []float64 {
		var out []float64
		for _, r := range rows {
			if v := r.Get(name); nodesNumericValue(v) {
				out = append(out, js.Num(v))
			}
		}
		return out
	}
	metrics := js.NewObj()
	for _, name := range wanted {
		vals := values(valid, name)
		if len(vals) == 0 {
			continue
		}
		sum, lo, hi := 0.0, math.Inf(1), math.Inf(-1)
		for _, v := range vals {
			sum += v
			lo = math.Min(lo, v)
			hi = math.Max(hi, v)
		}
		metrics.Set(nodesMetricLabel(name), js.O(
			"min", nodesFormatMetric(name, lo),
			"avg", nodesFormatMetric(name, sum/float64(len(vals))),
			"max", nodesFormatMetric(name, hi),
			"last", nodesFormatMetric(name, vals[len(vals)-1]),
		))
	}
	// Downsample to `points` buckets by averaging so the model sees the shape without hundreds of rows.
	base := nodesSeriesMetrics
	if len(metricsFilter) > 0 {
		base = wanted
	}
	var seriesNames []string
	for _, n := range base {
		if has[n] {
			seriesNames = append(seriesNames, n)
		}
	}
	series := []any{}
	buckets := min(points, len(valid))
	for b := 0; b < buckets; b++ {
		slice := valid[b*len(valid)/buckets : (b+1)*len(valid)/buckets]
		point := js.O("time", proxmox.IsoTime(slice[len(slice)-1].Get("time")))
		for _, name := range seriesNames {
			vals := values(slice, name)
			if len(vals) > 0 {
				sum := 0.0
				for _, v := range vals {
					sum += v
				}
				point.Set(nodesMetricLabel(name), nodesFormatMetric(name, sum/float64(len(vals))))
			}
		}
		series = append(series, point)
	}
	var from, to any = js.Undef, js.Undef
	if len(valid) > 0 {
		from = proxmox.IsoTime(valid[0].Get("time"))
		to = proxmox.IsoTime(valid[len(valid)-1].Get("time"))
	}
	return proxmox.C("samples", len(valid), "from", from, "to", to, "metrics", metrics, "series", series)
}

// ---------------------------------------------------------------- APT

// nodesJoin is arr?.join(" ") for an optional array.
func nodesJoin(v any) any {
	arr, ok := v.([]any)
	if !ok {
		return js.Undef
	}
	parts := make([]string, len(arr))
	for i, x := range arr {
		if !js.Nullish(x) {
			parts[i] = js.Str(x)
		}
	}
	return strings.Join(parts, " ")
}

func nodesSummarizeRepositories(r *Obj) *Obj {
	infos := js.Objs(or(r.Get("infos"), []any{}))
	repos := []any{}
	for _, file := range js.Objs(or(r.Get("files"), []any{})) {
		for index, repo := range js.Objs(or(file.Get("repositories"), []any{})) {
			notes := []any{}
			for _, i := range infos {
				if i.Get("path") == file.Get("path") && i.Num("index") == float64(index) {
					prop := ""
					if i.Truthy("property") {
						prop = " (" + js.Str(i.Get("property")) + ")"
					}
					notes = append(notes, js.Str(i.Get("kind"))+": "+js.Str(i.Get("message"))+prop)
				}
			}
			var comment any = js.Undef
			if s, ok := repo.Get("Comment").(string); ok {
				comment = strings.TrimSpace(s)
			}
			repos = append(repos, proxmox.C(
				"path", file.Get("path"),
				"index", index,
				"enabled", repo.Truthy("Enabled"),
				"types", nodesJoin(repo.Get("Types")),
				"uris", nodesJoin(repo.Get("URIs")),
				"suites", nodesJoin(repo.Get("Suites")),
				"components", nodesJoin(repo.Get("Components")),
				"comment", comment,
				"notes", notes,
			))
		}
	}
	errs := mapObjs(js.Objs(or(r.Get("errors"), []any{})), func(e *Obj) any {
		return js.Str(e.Get("path")) + ": " + js.Str(e.Get("error"))
	})
	warnings := mapObjs(filterObjs(infos, func(i *Obj) bool { return i.Get("kind") == "warning" }), func(i *Obj) any {
		return js.Str(i.Get("path")) + "#" + js.Str(i.Get("index")) + ": " + js.Str(i.Get("message"))
	})
	standard := mapObjs(js.Objs(or(r.Get("standard-repos"), []any{})), func(s *Obj) any {
		status := "not configured"
		if !js.Nullish(s.Get("status")) {
			status = js.Str(pick(s.Truthy("status"), "enabled", "disabled"))
		}
		return js.O("handle", s.Get("handle"), "name", s.Get("name"), "status", status)
	})
	return proxmox.C("digest", r.Get("digest"), "repositories", repos, "errors", errs, "warnings", warnings, "standard_repos", standard)
}

// ---------------------------------------------------------------- ACME

func nodesSummarizePlugin(v any) any {
	p := nodesObj(v)
	copied := p.Clone()
	copied.Set("data", js.Undef)
	return proxmox.Compact(js.Spread(proxmox.RedactSecrets(copied), js.O("data", js.Undef, "data_keys", proxmox.RedactPluginData(p.Get("data")))))
}

// nodesEncodePluginData encodes plugin credentials the way the web UI does: KEY=value lines, base64.
func nodesEncodePluginData(data *Obj) string {
	lines := make([]string, 0, data.Len())
	for _, k := range data.Keys() {
		lines = append(lines, k+"="+js.Str(data.Get(k)))
	}
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(lines, "\n")))
}

func nodesGetConfig(ctx context.Context, client *proxmox.Client, node string) (*Obj, error) {
	cfg, err := client.GetObj(ctx, nodesPath(node)+"/config", nil)
	if err != nil {
		return nil, err
	}
	out := cfg.Clone()
	for _, k := range cfg.Keys() {
		v := cfg.Get(k)
		if k == "acme" || nodesAcmeDomRe.MatchString(k) {
			out.Set(k, proxmox.ParsePropertyString(v, pick(k == "acme", "account", "domain").(string)))
		}
		if k == "wakeonlan" {
			out.Set(k, proxmox.ParsePropertyString(v, "mac"))
		}
		if k == "location" {
			out.Set(k, proxmox.ParsePropertyString(v, ""))
		}
	}
	return out, nil
}

// nodesNaturalCompare is localeCompare with { numeric: true }: digit runs compare by value.
func nodesNaturalCompare(a, b string) float64 {
	tokens := func(s string) []string {
		var out []string
		for i := 0; i < len(s); {
			j := i + 1
			digit := s[i] >= '0' && s[i] <= '9'
			for j < len(s) && (s[j] >= '0' && s[j] <= '9') == digit {
				j++
			}
			out = append(out, s[i:j])
			i = j
		}
		return out
	}
	isDigits := func(s string) bool { return s != "" && s[0] >= '0' && s[0] <= '9' }
	ta, tb := tokens(a), tokens(b)
	for i := 0; i < len(ta) && i < len(tb); i++ {
		x, y := ta[i], tb[i]
		if isDigits(x) && isDigits(y) {
			nx, ny := strings.TrimLeft(x, "0"), strings.TrimLeft(y, "0")
			if len(nx) != len(ny) {
				if len(nx) < len(ny) {
					return -1
				}
				return 1
			}
			if nx != ny {
				if nx < ny {
					return -1
				}
				return 1
			}
			continue
		}
		if c := js.LocaleCompare(x, y); c != 0 && strings.ToLower(x) != strings.ToLower(y) {
			// Compare the remainder from here so a shorter prefix sorts first, as collation does.
			return float64(js.LocaleCompare(strings.ToLower(strings.Join(ta[i:], "")), strings.ToLower(strings.Join(tb[i:], ""))))
		}
	}
	if len(ta) != len(tb) {
		if len(ta) < len(tb) {
			return -1
		}
		return 1
	}
	return float64(js.LocaleCompare(a, b))
}

// nodesFilterStrings is filterAndLimit over strings, without a limit.
func nodesFilterStrings(items []string, search any) []any {
	out := []any{}
	q, _ := search.(string)
	for _, s := range items {
		if q == "" || strings.Contains(strings.ToLower(s), strings.ToLower(q)) {
			out = append(out, s)
		}
	}
	return out
}

var (
	nodesIfaceTypes = []string{"bridge", "bond", "eth", "alias", "vlan", "fabric", "OVSBridge", "OVSBond", "OVSPort", "OVSIntPort", "vnet", "unknown"}
	nodesBondModes  = []string{"balance-rr", "active-backup", "balance-xor", "broadcast", "802.3ad", "balance-tlb", "balance-alb", "balance-slb", "lacp-balance-slb", "lacp-balance-tcp"}
)

func nodesMaxCharsArg(def int) *Schema {
	return Int().Pos().Max(5_000_000).Opt().D(fmt.Sprintf("Truncate the text to this many characters (default %d)", def))
}

func nodesTimeArg(what string) *Schema {
	return Union(Str(), Num()).Opt().D(what + `: epoch seconds or a date like "2026-01-31 12:00:00" / ISO 8601`)
}

func registerNodes(c *Context) {
	client := c.Client

	// ================================================================ status & statistics

	c.Define("proxmox_get_node_status", Def{
		Toolset:     "nodes",
		Title:       "Get node status",
		Description: "Detailed status of one node: CPU model/sockets/cores and usage, IO wait, load average, memory/swap/root filesystem usage, kernel, Proxmox VE version, boot mode (EFI/BIOS, secure boot), uptime, KSM sharing and the subscription status. Use proxmox_list_nodes for all nodes at a glance and proxmox_get_node_stats for history.",
		Input:       []Prop{P("node", nodeArg), P("raw", rawArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			stAny, err := client.Get(ctx, nodesPath(node)+"/status", nil)
			if err != nil {
				return nil, err
			}
			version, err := client.Get(ctx, nodesPath(node)+"/version", nil)
			if err != nil {
				version = js.Undef
			}
			sub, err := client.Get(ctx, nodesPath(node)+"/subscription", nil)
			if err != nil {
				sub = js.Undef
			}
			if a.Truthy("raw") {
				return proxmox.RedactSecrets(js.O("status", stAny, "version", version, "subscription", nodesSummarizeSubscription(sub))), nil
			}
			st := nodesObj(stAny)
			ci := nodesObj(st.Or("cpuinfo", js.NewObj()))
			var kernel any = st.Get("kversion")
			if k := st.Get("current-kernel"); js.Truthy(k) {
				ko := nodesObj(k)
				kernel = strings.TrimSpace(js.Str(ko.Or("sysname", "")) + " " + js.Str(ko.Or("release", "")) + " (" + js.Str(ko.Or("machine", "?")) + ")")
			}
			var versionText any = js.Undef
			if js.Truthy(version) {
				v := nodesObj(version)
				versionText = js.Str(v.Get("version")) + " (release " + js.Str(v.Get("release")) + ", " + js.Str(v.Get("repoid")) + ")"
			}
			var boot any = js.Undef
			if bi := st.Get("boot-info"); js.Truthy(bi) {
				b := nodesObj(bi)
				boot = proxmox.C("mode", b.Get("mode"), "secure_boot", b.Get("secureboot"))
			}
			mem, swap, rootfs, ksm := st.ObjAt("memory"), st.ObjAt("swap"), st.ObjAt("rootfs"), st.ObjAt("ksm")
			return proxmox.C(
				"node", node,
				"uptime", proxmox.FormatUptime(st.Get("uptime")),
				"cpu_pct", proxmox.FractionPct(st.Get("cpu")),
				"iowait_pct", proxmox.FractionPct(st.Get("wait")),
				"loadavg", st.Get("loadavg"),
				"cpu", proxmox.C("model", ci.Get("model"), "sockets", ci.Get("sockets"), "cores", ci.Get("cores"), "threads", ci.Get("cpus"), "mhz", ifThen(ci.Truthy("mhz"), ci.Num("mhz"))),
				"memory", proxmox.Usage(mem.Get("used"), mem.Get("total")),
				"memory_available", proxmox.FormatBytes(mem.Get("available")),
				"swap", proxmox.Usage(swap.Get("used"), swap.Get("total")),
				"root_fs", proxmox.Usage(rootfs.Get("used"), rootfs.Get("total")),
				"ksm_shared", ifThen(ksm.Truthy("shared"), proxmox.FormatBytes(ksm.Get("shared"))),
				"kernel", kernel,
				"pve_version", st.Get("pveversion"),
				"version", versionText,
				"boot", boot,
				"subscription", nodesSummarizeSubscription(sub),
			), nil
		},
	})

	c.Define("proxmox_get_node_stats", Def{
		Toolset:     "nodes",
		Title:       "Get node performance history",
		Description: "Node performance history from the RRD database (CPU, IO wait, load, memory, swap, root disk, network, ARC...) over a timeframe, summarized as min/avg/max/last per metric plus a short downsampled series. Network values are bytes per second.",
		Input: []Prop{
			P("node", nodeArg),
			P("timeframe", Enum("hour", "day", "week", "month", "year", "decade").Opt().D("Time window (default hour)")),
			P("cf", Enum("AVERAGE", "MAX").Opt().D("RRD consolidation function (default AVERAGE)")),
			P("metrics", Arr(Str()).Opt().D("Only these metrics, e.g. [\"cpu\", \"memused\", \"netin\"] (default all)")),
			P("points", Int().Min(1).Max(500).Opt().D("Number of points in the downsampled series (default 12)")),
			P("raw", rawArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			timeframe := or(a.Get("timeframe"), "hour")
			rows, err := client.Get(ctx, nodesPath(node)+"/rrddata", defined(js.O("timeframe", timeframe, "cf", a.Get("cf"))))
			if err != nil {
				return nil, err
			}
			if a.Truthy("raw") {
				return rows, nil
			}
			return js.Spread(js.O("node", node, "timeframe", timeframe, "cf", or(a.Get("cf"), "AVERAGE")),
				nodesSummarizeRrd(rows, a.Strs("metrics"), int(js.Num(or(a.Get("points"), 12))))), nil
		},
	})

	c.Define("proxmox_get_node_netstat", Def{
		Toolset:     "nodes",
		Title:       "Get guest network counters",
		Description: "Traffic counters of the guests' tap/veth network devices on a node (bytes in/out per device and VMID), as seen from the host.",
		Input:       []Prop{P("node", nodeArg), P("vmid", Int().Opt().D("Only this guest"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			rows, err := client.GetList(ctx, nodesPath(node)+"/netstat", nil)
			if err != nil {
				return nil, err
			}
			if a.Defined("vmid") {
				rows = filterObjs(rows, func(r *Obj) bool { return r.Num("vmid") == a.Num("vmid") })
			}
			return mapObjs(rows, func(r *Obj) any {
				return proxmox.C("vmid", ifThen(r.Defined("vmid"), r.Num("vmid")), "dev", r.Get("dev"), "in", proxmox.FormatBytes(r.Get("in")), "out", proxmox.FormatBytes(r.Get("out")))
			}), nil
		},
	})

	c.Define("proxmox_get_node_report", Def{
		Toolset:     "nodes",
		Title:       "Get node system report",
		Description: "The node's pvereport: a long text dump of versions, hardware, storage, network, cluster and guest configuration, as used for support requests. Large; truncated to max_chars. Can take a while to generate.",
		Input:       []Prop{P("node", nodeArg), P("max_chars", nodesMaxCharsArg(50000))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			report, err := client.Get(ctx, nodesPath(node)+"/report", nil)
			if err != nil {
				return nil, err
			}
			return js.Spread(js.O("node", node), nodesTruncate(report, int(js.Num(or(a.Get("max_chars"), 50000))))), nil
		},
	})

	c.Define("proxmox_query_url_metadata", Def{
		Toolset:     "nodes",
		Title:       "Query URL metadata",
		Description: "Ask a node to look up a URL's file name, size and MIME type (HEAD request from the node). Useful before proxmox_download_to_storage to check the URL and pick the file name.",
		// The node connects to any URL it is given (including internal addresses), so this is
		// gated like proxmox_download_to_storage rather than treated as a read.
		Write: true,
		Input: []Prop{
			P("node", nodeArg),
			P("url", Str().D("The URL to inspect, e.g. https://cdimage.debian.org/.../debian-13.iso")),
			P("verify_certificates", Bool().Opt().D("Verify TLS certificates (default true)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			v, err := client.Get(ctx, nodesPath(node)+"/query-url-metadata", defined(js.O("url", a.Get("url"), "verify-certificates", a.Get("verify_certificates"))))
			if err != nil {
				return nil, err
			}
			m := nodesObj(v)
			return proxmox.C("url", a.Get("url"), "filename", m.Get("filename"), "mimetype", m.Get("mimetype"), "size", proxmox.FormatBytes(m.Get("size")), "size_bytes", m.Get("size")), nil
		},
	})

	// ================================================================ services

	c.Define("proxmox_list_node_services", Def{
		Toolset:     "nodes",
		Title:       "List node services",
		Description: "List the Proxmox-related system services on a node (pveproxy, pvedaemon, corosync, pve-cluster, chrony, sshd, postfix...) with their running state and whether they are enabled. Failed services are listed first.",
		Input:       []Prop{P("node", nodeArg), P("search", searchArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			services, err := client.GetList(ctx, nodesPath(node)+"/services", nil)
			if err != nil {
				return nil, err
			}
			rank := func(s *Obj) float64 {
				if s.Get("active-state") == "failed" {
					return 0
				}
				return 1
			}
			sortObjs(services, func(x, y *Obj) float64 {
				if d := rank(x) - rank(y); d != 0 {
					return d
				}
				return compareStr(x.Get("service"), y.Get("service"))
			})
			res := filterAndLimit(services, a.Get("search"), js.Undef, func(s *Obj) []any {
				return []any{s.Get("service"), s.Get("name"), s.Get("desc")}
			})
			failed := mapObjs(filterObjs(services, func(s *Obj) bool { return s.Get("active-state") == "failed" }), func(s *Obj) any { return s.Get("service") })
			return js.Spread(js.O("node", node, "failed", failed), res.Map(func(s *Obj) any {
				return proxmox.C("service", s.Get("service"), "description", s.Get("desc"), "active", s.Get("active-state"), "state", s.Get("state"), "unit", s.Get("unit-state"))
			})), nil
		},
	})

	c.Define("proxmox_manage_node_service", Def{
		Toolset:     "nodes",
		Title:       "Start, stop, restart or reload a node service",
		Description: "Start, stop, restart or reload a system service on a node, e.g. restart pveproxy after a certificate change. Stopping pve-cluster, corosync, pveproxy or pvedaemon can cut off API access or cluster communication; prefer reload where possible.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("node", nodeArg),
			P("service", Str().D("Service ID, e.g. pveproxy, pvedaemon, pvestatd, corosync, pve-cluster, chrony, sshd, postfix (see proxmox_list_node_services)")),
			P("action", Enum("start", "stop", "restart", "reload").D("What to do; reload falls back to restart if the service can't reload")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			base := nodesPath(node) + "/services/" + enc(a.Get("service"))
			upid, err := client.Post(ctx, base+"/"+js.Str(a.Get("action")), nil)
			if err != nil {
				return nil, err
			}
			res, err := runTask(ctx, client, upid, a.Get("wait"), js.O("node", node, "service", a.Get("service"), "action", a.Get("action")))
			if err != nil {
				return nil, err
			}
			if a.Get("wait") == false {
				return res, nil
			}
			var state any = js.Undef
			if s, err := client.Get(ctx, base+"/state", nil); err == nil && js.Truthy(s) {
				so := nodesObj(s)
				state = proxmox.C("active", so.Get("active-state"), "state", so.Get("state"), "unit", so.Get("unit-state"))
			}
			return proxmox.Compact(js.Spread(res, js.O("state", state))), nil
		},
	})

	// ================================================================ network

	c.Define("proxmox_list_node_network", Def{
		Toolset:     "nodes",
		Title:       "List node network interfaces",
		Description: "List a node's network configuration: physical NICs, Linux bridges, bonds, VLANs and OVS objects with addresses, gateways, ports and comments. Interfaces that are configured but not active are flagged; that usually means pending (unapplied) changes from proxmox_save_node_network_interface, or a down link. Apply with proxmox_apply_node_network or discard with proxmox_revert_node_network.",
		Input: []Prop{
			P("node", nodeArg),
			P("type", Enum("bridge", "bond", "eth", "alias", "vlan", "fabric", "OVSBridge", "OVSBond", "OVSPort", "OVSIntPort", "vnet", "any_bridge", "any_local_bridge", "include_sdn").Opt().D("Only this interface type (any_bridge = Linux and OVS bridges)")),
			P("search", searchArg),
			P("raw", rawArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			data, meta, err := client.RequestMeta(ctx, "GET", nodesPath(node)+"/network", defined(js.O("type", a.Get("type"))))
			if err != nil {
				return nil, err
			}
			// Proxmox returns the pending diff as a top-level `changes` attribute.
			changes := meta.Get("changes")
			ifaces := js.Objs(data)
			sortObjs(ifaces, func(x, y *Obj) float64 { return nodesNaturalCompare(js.Str(x.Get("iface")), js.Str(y.Get("iface"))) })
			res := filterAndLimit(ifaces, a.Get("search"), js.Undef, func(i *Obj) []any {
				return []any{i.Get("iface"), i.Get("type"), i.Get("cidr"), i.Get("address"), i.Get("comments"), i.Get("bridge_ports"), i.Get("slaves")}
			})
			var items []any
			if a.Truthy("raw") {
				items = js.Array(res.Items)
			} else {
				items = mapObjs(res.Items, nodesSummarizeIface)
			}
			inactive := mapObjs(filterObjs(res.Items, func(i *Obj) bool { return !i.Truthy("active") && i.Get("type") != "unknown" }), func(i *Obj) any { return i.Get("iface") })
			var pending any = js.Undef
			if s, ok := changes.(string); ok && s != "" {
				pending = s
			}
			return proxmox.C(
				"node", node,
				"total", res.Total,
				"inactive_or_pending", inactive,
				"pending_changes", pending,
				"note", ifThen(len(inactive) > 0, "Interfaces listed in inactive_or_pending are not up: either the link is down or the change is pending until proxmox_apply_node_network."),
				"items", items,
			), nil
		},
	})

	c.Define("proxmox_get_node_network_interface", Def{
		Toolset:     "nodes",
		Title:       "Get a node network interface",
		Description: "Get the configuration of one network interface on a node (as stored in /etc/network/interfaces, including pending changes).",
		Input:       []Prop{P("node", nodeArg), P("iface", Str().D("Interface name, e.g. vmbr0, bond0, eno1, vmbr0.20")), P("raw", rawArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			i, err := client.Get(ctx, nodesPath(node)+"/network/"+enc(a.Get("iface")), nil)
			if err != nil {
				return nil, err
			}
			if a.Truthy("raw") {
				return i, nil
			}
			return nodesSummarizeIface(js.Spread(js.O("iface", a.Get("iface")), i)), nil
		},
	})

	c.Define("proxmox_save_node_network_interface", Def{
		Toolset:     "nodes",
		Title:       "Create or update a node network interface",
		Description: "Create (create=true) or update a network interface on a node: Linux bridge, bond, VLAN, NIC address etc. Changes are only written to /etc/network/interfaces.new and stay pending until proxmox_apply_node_network (or are discarded with proxmox_revert_node_network). On update, only the given fields change; remove settings with `delete`. Lists of ports/slaves are given as arrays.",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("node", nodeArg),
			P("iface", Str().D("Interface name, e.g. vmbr1, bond0, vmbr0.20, vlan20")),
			P("create", Bool().Opt().D("Create a new interface (default false = update an existing one)")),
			P("type", Enum(nodesIfaceTypes...).Opt().D("Interface type (required when creating; looked up when updating)")),
			P("cidr", Str().Opt().D("IPv4 address with prefix, e.g. 192.168.1.10/24")),
			P("gateway", Str().Opt().D("IPv4 default gateway")),
			P("cidr6", Str().Opt().D("IPv6 address with prefix")),
			P("gateway6", Str().Opt().D("IPv6 default gateway")),
			P("bridge_ports", Arr(Str()).Opt().D("Bridge ports, e.g. [\"eno1\"] (bridge); [] for none")),
			P("bridge_vlan_aware", Bool().Opt().D("Make the bridge VLAN aware (bridge)")),
			P("bridge_vids", Str().Opt().D("Allowed VLANs on a VLAN-aware bridge, e.g. '2-4094' or '10 20 100-200'")),
			P("slaves", Arr(Str()).Opt().D("Bond member interfaces, e.g. [\"eno1\", \"eno2\"] (bond)")),
			P("bond_mode", Enum(nodesBondModes...).Opt().D("Bonding mode (bond)")),
			P("bond_primary", Str().Opt().D("Primary interface for active-backup bonds")),
			P("bond_xmit_hash_policy", Enum("layer2", "layer2+3", "layer3+4").Opt().D("Transmit hash policy for balance-xor/802.3ad bonds")),
			P("vlan_raw_device", Str().Opt().D("Parent interface of a VLAN, e.g. vmbr0 (vlan)")),
			P("vlan_id", Int().Min(1).Max(4094).Opt().D("VLAN tag for a custom-named VLAN interface (vlan)")),
			P("mtu", Int().Min(1280).Max(65520).Opt().D("MTU")),
			P("autostart", Bool().Opt().D("Bring the interface up at boot")),
			P("comments", Str().Opt().D("Comment")),
			P("delete", deleteKeysArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			typ := a.Get("type")
			if !js.Truthy(typ) {
				if a.Truthy("create") {
					return nil, errors.New("type is required when creating an interface")
				}
				// The update endpoint requires `type` too, so keep the current one.
				cur, err := client.GetObj(ctx, nodesPath(node)+"/network/"+enc(a.Get("iface")), nil)
				if err != nil {
					return nil, err
				}
				typ = cur.Get("type")
			}
			params, err := definedExtra(js.O(
				"type", typ,
				"cidr", a.Get("cidr"),
				"gateway", a.Get("gateway"),
				"cidr6", a.Get("cidr6"),
				"gateway6", a.Get("gateway6"),
				"bridge_ports", nodesIfaceList(a.Get("bridge_ports")),
				"bridge_vlan_aware", a.Get("bridge_vlan_aware"),
				"bridge_vids", a.Get("bridge_vids"),
				"slaves", nodesIfaceList(a.Get("slaves")),
				"bond_mode", a.Get("bond_mode"),
				"bond-primary", a.Get("bond_primary"),
				"bond_xmit_hash_policy", a.Get("bond_xmit_hash_policy"),
				"vlan-raw-device", a.Get("vlan_raw_device"),
				"vlan-id", a.Get("vlan_id"),
				"mtu", a.Get("mtu"),
				"autostart", a.Get("autostart"),
				"comments", a.Get("comments"),
			), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			if a.Truthy("create") {
				_, err = client.Post(ctx, nodesPath(node)+"/network", js.Spread(js.O("iface", a.Get("iface")), params))
			} else {
				_, err = client.Put(ctx, nodesPath(node)+"/network/"+enc(a.Get("iface")), defined(js.Spread(params, js.O("delete", deleteParam(a.Get("delete"))))))
			}
			if err != nil {
				return nil, err
			}
			return js.O(
				"node", node,
				"iface", a.Get("iface"),
				js.Str(pick(a.Truthy("create"), "created", "updated")), true,
				"pending", true,
				"note", "Pending until applied: review with proxmox_list_node_network, then proxmox_apply_node_network (or proxmox_revert_node_network to discard).",
			), nil
		},
	})

	c.Define("proxmox_delete_node_network_interface", Def{
		Toolset:     "nodes",
		Title:       "Delete a node network interface",
		Description: "Remove a network interface (bridge, bond, VLAN...) from a node's configuration. Like other network edits it stays pending until proxmox_apply_node_network. Guests attached to a removed bridge lose connectivity once applied.",
		Delete:      true,
		Input:       []Prop{P("node", nodeArg), P("iface", Str().D("Interface name, e.g. vmbr1"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			if _, err := client.Delete(ctx, nodesPath(node)+"/network/"+enc(a.Get("iface")), nil); err != nil {
				return nil, err
			}
			return js.O("node", node, "deleted", a.Get("iface"), "pending", true, "note", "Apply with proxmox_apply_node_network or discard with proxmox_revert_node_network."), nil
		},
	})

	c.Define("proxmox_apply_node_network", Def{
		Toolset:     "nodes",
		Title:       "Apply pending network changes",
		Description: "Apply a node's pending network changes (ifreload). Can interrupt connectivity to the node and its guests, and a wrong bridge/gateway can lock you out of the node; review pending changes with proxmox_list_node_network first.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("node", nodeArg),
			P("regenerate_frr", Bool().Opt().D("Also regenerate the FRR (SDN fabric routing) configuration")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			upid, err := client.Put(ctx, nodesPath(node)+"/network", defined(js.O("regenerate-frr", a.Get("regenerate_frr"))))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("node", node, "applied", true))
		},
	})

	c.Define("proxmox_revert_node_network", Def{
		Toolset:     "nodes",
		Title:       "Discard pending network changes",
		Description: "Discard all pending (unapplied) network changes on a node, keeping the running configuration.",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input:       []Prop{P("node", nodeArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			if _, err := client.Delete(ctx, nodesPath(node)+"/network", nil); err != nil {
				return nil, err
			}
			return js.O("node", node, "reverted", true), nil
		},
	})

	// ================================================================ DNS, hosts, time

	c.Define("proxmox_get_node_dns", Def{
		Toolset:     "nodes",
		Title:       "Get node DNS settings",
		Description: "Get a node's DNS resolver settings: search domain and up to three name servers.",
		Input:       []Prop{P("node", nodeArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			dns, err := client.Get(ctx, nodesPath(node)+"/dns", nil)
			if err != nil {
				return nil, err
			}
			return js.Spread(js.O("node", node), dns), nil
		},
	})

	c.Define("proxmox_set_node_dns", Def{
		Toolset:     "nodes",
		Title:       "Set node DNS settings",
		Description: "Set a node's DNS search domain and name servers (/etc/resolv.conf). Fields left out keep their current value; pass an empty string to clear dns2/dns3.",
		Write:       true,
		Destructive: True,
		Idempotent:  true,
		Input: []Prop{
			P("node", nodeArg),
			P("search", Str().Opt().D("Search domain, e.g. example.lan")),
			P("dns1", Str().Opt().D("First name server IP")),
			P("dns2", Str().Opt().D("Second name server IP")),
			P("dns3", Str().Opt().D("Third name server IP")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			cur, err := client.GetObj(ctx, nodesPath(node)+"/dns", nil)
			if err != nil {
				return nil, err
			}
			// The API replaces the whole resolver config, so start from the current values.
			merged := js.Spread(
				js.O("search", cur.Get("search"), "dns1", cur.Get("dns1"), "dns2", cur.Get("dns2"), "dns3", cur.Get("dns3")),
				defined(js.O("search", a.Get("search"), "dns1", a.Get("dns1"), "dns2", a.Get("dns2"), "dns3", a.Get("dns3"))),
			)
			if !merged.Truthy("search") {
				return nil, errors.New("search (domain) is required")
			}
			if _, err := client.Put(ctx, nodesPath(node)+"/dns", proxmox.Compact(merged)); err != nil {
				return nil, err
			}
			dns, err := client.Get(ctx, nodesPath(node)+"/dns", nil)
			if err != nil {
				return nil, err
			}
			return js.O("node", node, "updated", true, "dns", dns), nil
		},
	})

	c.Define("proxmox_get_node_hosts", Def{
		Toolset:     "nodes",
		Title:       "Get node /etc/hosts",
		Description: "Read a node's /etc/hosts file and its digest (pass the digest to proxmox_set_node_hosts to avoid overwriting concurrent edits).",
		Input:       []Prop{P("node", nodeArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			h, err := client.GetObj(ctx, nodesPath(node)+"/hosts", nil)
			if err != nil {
				return nil, err
			}
			return js.O("node", node, "digest", h.Get("digest"), "content", h.Get("data")), nil
		},
	})

	c.Define("proxmox_set_node_hosts", Def{
		Toolset:     "nodes",
		Title:       "Write node /etc/hosts",
		Description: "Replace a node's /etc/hosts with the given content. Read it first with proxmox_get_node_hosts and pass its digest. The node's own name must keep resolving to its cluster IP or pve-cluster can break.",
		Write:       true,
		Destructive: True,
		Idempotent:  true,
		Input: []Prop{
			P("node", nodeArg),
			P("content", Str().D("The complete new content of /etc/hosts")),
			P("digest", digestArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			if _, err := client.Post(ctx, nodesPath(node)+"/hosts", defined(js.O("data", a.Get("content"), "digest", a.Get("digest")))); err != nil {
				return nil, err
			}
			return js.O("node", node, "updated", true), nil
		},
	})

	c.Define("proxmox_get_node_time", Def{
		Toolset:     "nodes",
		Title:       "Get node time and time zone",
		Description: "Get a node's current time (UTC), local time and configured time zone. Useful for spotting clock drift between nodes.",
		Input:       []Prop{P("node", nodeArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			t, err := client.GetObj(ctx, nodesPath(node)+"/time", nil)
			if err != nil {
				return nil, err
			}
			offset := t.Num("localtime") - t.Num("time")
			now := float64(time.Now().UnixMilli()) / 1000
			return proxmox.C(
				"node", node,
				"timezone", t.Get("timezone"),
				"utc", proxmox.IsoTime(t.Get("time")),
				"utc_offset_hours", ifThen(js.IsFinite(offset), js.Round(offset/3600*100)/100),
				"drift_s", ifThen(js.IsFinite(t.Num("time")), js.Round(t.Num("time")-now)),
			), nil
		},
	})

	c.Define("proxmox_set_node_timezone", Def{
		Toolset:     "nodes",
		Title:       "Set node time zone",
		Description: "Set a node's time zone, e.g. Europe/London or UTC.",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input:       []Prop{P("node", nodeArg), P("timezone", Str().D("Time zone name from zone.tab, e.g. Europe/London, America/New_York, UTC"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			if _, err := client.Put(ctx, nodesPath(node)+"/time", js.O("timezone", a.Get("timezone"))); err != nil {
				return nil, err
			}
			return js.O("node", node, "timezone", a.Get("timezone"), "updated", true), nil
		},
	})

	// ================================================================ node config

	c.Define("proxmox_get_node_config", Def{
		Toolset:     "nodes",
		Title:       "Get node options",
		Description: "Get a node's own options (/etc/pve/nodes/<node>/config): notes/description, wake-on-LAN MAC, ACME account and domains, start-on-boot delay, ballooning target and location, plus the digest for safe updates.",
		Input:       []Prop{P("node", nodeArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			cfg, err := nodesGetConfig(ctx, client, node)
			if err != nil {
				return nil, err
			}
			return js.Spread(js.O("node", node), cfg), nil
		},
	})

	c.Define("proxmox_update_node_config", Def{
		Toolset:     "nodes",
		Title:       "Update node options",
		Description: "Change a node's options: notes/description, wake-on-LAN MAC, ACME account and certificate domains (used by proxmox_node_acme_certificate), start-on-boot delay, ballooning target. Only the given fields change; remove with `delete`.",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("node", nodeArg),
			P("description", Str().Opt().D("Node notes (Markdown), shown in the web UI")),
			P("wakeonlan", Str().Opt().D("Wake-on-LAN MAC address, optionally with options, e.g. 'aa:bb:cc:dd:ee:ff,bind-interface=vmbr0'")),
			P("startall_onboot_delay", Int().Min(0).Max(300).Opt().D("Seconds to wait before starting on-boot guests")),
			P("ballooning_target", Int().Min(0).Max(100).Opt().D("RAM usage target for ballooning, in percent")),
			P("acme_account", Str().Opt().D("ACME account name for this node's certificate (see proxmox_list_acme_accounts)")),
			P("acme_domains", Arr(Object(
				P("domain", Str().D("Domain name, e.g. pve1.example.com")),
				P("plugin", Str().Opt().D("ACME plugin ID (default standalone HTTP validation)")),
				P("alias", Str().Opt().D("DNS alias domain for challenge delegation")),
			)).Max(6).Opt().D("Certificate domains (acmedomain0..5); replaces the existing list")),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			var acme any = js.Undef
			if a.Defined("acme_account") {
				acme = "account=" + js.Str(a.Get("acme_account"))
			}
			params := defined(js.O(
				"description", a.Get("description"),
				"wakeonlan", a.Get("wakeonlan"),
				"startall-onboot-delay", a.Get("startall_onboot_delay"),
				"ballooning-target", a.Get("ballooning_target"),
				"acme", acme,
				"digest", a.Get("digest"),
			))
			del := append([]any{}, a.Arr("delete")...)
			domains, hasDomains := a.Get("acme_domains").([]any)
			if hasDomains {
				for i, d := range js.Objs(domains) {
					parts := []string{}
					for _, p := range []any{d.Get("domain"), ifThen(d.Truthy("plugin"), "plugin="+js.Str(d.Get("plugin"))), ifThen(d.Truthy("alias"), "alias="+js.Str(d.Get("alias")))} {
						if js.Truthy(p) {
							parts = append(parts, js.Str(p))
						}
					}
					params.Set(fmt.Sprintf("acmedomain%d", i), strings.Join(parts, ","))
				}
				for i := len(domains); i < 6; i++ {
					del = append(del, fmt.Sprintf("acmedomain%d", i))
				}
			}
			// Clearing acmedomainN keys that were never set is harmless, but only send them if they exist to keep the request clean.
			deleteKeys := del
			if hasDomains && len(domains) < 6 {
				cur, err := client.GetObj(ctx, nodesPath(node)+"/config", nil)
				if err != nil {
					return nil, err
				}
				deleteKeys = []any{}
				for _, k := range del {
					if ks := js.Str(k); !nodesAcmeDomRe.MatchString(ks) || cur.Has(ks) {
						deleteKeys = append(deleteKeys, k)
					}
				}
			}
			body, err := definedExtra(js.Spread(params, js.O("delete", deleteParam(deleteKeys))), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			if _, err := client.Put(ctx, nodesPath(node)+"/config", body); err != nil {
				return nil, err
			}
			cfg, err := nodesGetConfig(ctx, client, node)
			if err != nil {
				return nil, err
			}
			return js.O("node", node, "updated", true, "config", cfg), nil
		},
	})

	// ================================================================ power & bulk actions

	c.Define("proxmox_node_power", Def{
		Toolset:     "nodes",
		Title:       "Reboot or shut down a node",
		Description: "Reboot or shut down a Proxmox node. Running guests are shut down or stopped by the node first (HA guests may be migrated per the HA shutdown policy). Consider proxmox_node_bulk_action migrate first to move guests away. A shut-down node can only be brought back by hand, IPMI or proxmox_wake_node.",
		Write:       true,
		Destructive: True,
		Input:       []Prop{P("node", nodeRequiredArg), P("command", Enum("reboot", "shutdown").D("reboot or shutdown"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Post(ctx, nodesPath(a.Get("node"))+"/status", js.O("command", a.Get("command"))); err != nil {
				return nil, err
			}
			return js.O("node", a.Get("node"), "command", a.Get("command"), "status", "requested"), nil
		},
	})

	c.Define("proxmox_wake_node", Def{
		Toolset:     "nodes",
		Title:       "Wake a node with wake-on-LAN",
		Description: "Send a wake-on-LAN packet to a powered-off node (sent by another cluster node). The node needs its wake-on-LAN MAC set in its options (proxmox_update_node_config wakeonlan).",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input:       []Prop{P("node", nodeRequiredArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			mac, err := client.Post(ctx, nodesPath(a.Get("node"))+"/wakeonlan", nil)
			if err != nil {
				return nil, err
			}
			_, isStr := mac.(string)
			return proxmox.C("node", a.Get("node"), "sent", true, "mac", ifThen(isStr, mac)), nil
		},
	})

	c.Define("proxmox_node_bulk_action", Def{
		Toolset:     "nodes",
		Title:       "Start, stop, suspend or migrate all guests on a node",
		Description: "Run a bulk action over the guests on one node: start (by default only guests with onboot=1; force=true for all), stop (shut down, hard-stopping after a timeout), suspend (VMs only) or migrate (all guests to `target`, e.g. before node maintenance). Restrict with `vmids`. Runs as one task. For a selection across nodes use proxmox_bulk_guest_action.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("node", nodeArg),
			P("action", Enum("start", "stop", "suspend", "migrate").D("What to do with the node's guests")),
			P("vmids", Arr(Int().Min(100)).Opt().D("Only these guests (default all on the node)")),
			P("target", Str().Opt().D("Target node (migrate only, required)")),
			P("force", Bool().Opt().D("start: also start guests without onboot=1")),
			P("force_stop", Bool().Opt().D("stop: hard-stop guests that don't shut down within the timeout (default true)")),
			P("timeout", Int().Pos().Opt().D("stop: per-guest shutdown timeout in seconds (default 180)")),
			P("with_local_disks", Bool().Opt().D("migrate: live-migrate local disks too")),
			P("max_workers", Int().Pos().Opt().D("Parallel jobs (default from datacenter max_workers; migrate needs one of the two)")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			action := js.Str(a.Get("action"))
			if action == "migrate" && !a.Truthy("target") {
				return nil, errors.New("target is required for migrate")
			}
			endpoint := map[string]string{"start": "startall", "stop": "stopall", "suspend": "suspendall", "migrate": "migrateall"}[action]
			vmids := a.Arr("vmids")
			params := defined(js.O(
				"vms", ifThen(len(vmids) > 0, js.Str(vmids)),
				"max-workers", a.Get("max_workers"),
				"force", ifThen(action == "start", a.Get("force")),
				"force-stop", ifThen(action == "stop", a.Get("force_stop")),
				"timeout", ifThen(action == "stop", a.Get("timeout")),
				"target", ifThen(action == "migrate", a.Get("target")),
				"with-local-disks", ifThen(action == "migrate", a.Get("with_local_disks")),
			))
			upid, err := client.Post(ctx, nodesPath(node)+"/"+endpoint, params)
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), proxmox.C("node", node, "action", action, "target", a.Get("target"), "vmids", a.Get("vmids")))
		},
	})

	// ================================================================ logs

	c.Define("proxmox_get_node_syslog", Def{
		Toolset:     "nodes",
		Title:       "Read node syslog",
		Description: "Read a node's system log (journal rendered as syslog lines), optionally for one service and a time range. Page with start/limit. For the raw systemd journal with 'last N lines' semantics use proxmox_get_node_journal.",
		Input: []Prop{
			P("node", nodeArg),
			P("service", Str().Opt().D("Only this service/unit, e.g. pveproxy, pvedaemon, corosync, pve-cluster")),
			P("since", Str().Opt().D("Start time, e.g. '2026-01-31 08:00:00' or 'yesterday'")),
			P("until", Str().Opt().D("End time, same format as since")),
			P("start", Int().Min(0).Opt().D("Line offset to start from (default 0)")),
			P("limit", limitArg(200)),
			P("search", searchArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			data, meta, err := client.RequestMeta(ctx, "GET", nodesPath(node)+"/syslog", defined(js.O(
				"service", a.Get("service"), "since", a.Get("since"), "until", a.Get("until"), "start", a.Get("start"), "limit", or(a.Get("limit"), 200),
			)))
			if err != nil {
				return nil, err
			}
			lines := js.Objs(data)
			var text []string
			for _, l := range lines {
				if t := js.Str(or(l.Get("t"), "")); t != "no content" {
					text = append(text, t)
				}
			}
			items := nodesFilterStrings(text, a.Get("search"))
			return js.O("node", node, "total", or(meta.Get("total"), len(lines)), "returned", len(items), "lines", items), nil
		},
	})

	c.Define("proxmox_get_node_journal", Def{
		Toolset:     "nodes",
		Title:       "Read node systemd journal",
		Description: "Read a node's systemd journal: the last N entries (default 100) or a time range, optionally only one unit, a syslog identifier, a priority level or kernel messages.",
		Input: []Prop{
			P("node", nodeArg),
			P("lastentries", Int().Pos().Max(10000).Opt().D("Only the last N entries (default 100 when no range is given)")),
			P("since", nodesTimeArg("Start of the range")),
			P("until", nodesTimeArg("End of the range")),
			P("unit", Str().Opt().D("Only this systemd unit, e.g. pveproxy (.service implied)")),
			P("identifier", Str().Opt().D("Only messages whose syslog identifier matches this glob, e.g. 'pve*'")),
			P("priority", Str().Opt().D("Only this priority or more severe: 0 (emerg) .. 7 (debug), or a range 'LOW..HIGH'; e.g. 3 for errors")),
			P("kernel", Bool().Opt().D("Only kernel messages")),
			P("search", searchArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			since, err := nodesToEpoch(a.Get("since"))
			if err != nil {
				return nil, err
			}
			until, err := nodesToEpoch(a.Get("until"))
			if err != nil {
				return nil, err
			}
			lastentries := a.Get("lastentries")
			if js.Nullish(lastentries) {
				lastentries = ifThen(js.IsUndef(since) && js.IsUndef(until), 100)
			}
			data, err := client.Get(ctx, nodesPath(node)+"/journal", defined(js.O(
				"lastentries", lastentries, "since", since, "until", until, "unit", a.Get("unit"), "service", a.Get("identifier"), "priority", a.Get("priority"), "kernel", a.Get("kernel"),
			)))
			if err != nil {
				return nil, err
			}
			arr, _ := data.([]any)
			text := make([]string, len(arr))
			for i, l := range arr {
				if s, ok := l.(string); ok {
					text[i] = s
				} else {
					text[i] = js.Stringify(l, "")
				}
			}
			items := nodesFilterStrings(text, a.Get("search"))
			return js.O("node", node, "returned", len(items), "lines", items), nil
		},
	})

	// ================================================================ updates & repositories

	c.Define("proxmox_list_node_updates", Def{
		Toolset:     "nodes",
		Title:       "List available package updates",
		Description: "List the package updates available on a node (from the last package index refresh) with installed and new versions, origin and priority, plus counts. Run proxmox_refresh_node_updates first for fresh results. Installing updates is not available through the API.",
		Input:       []Prop{P("node", nodeArg), P("search", searchArg), P("limit", limitArg(300))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			pkgs, err := client.GetList(ctx, nodesPath(node)+"/apt/update", nil)
			if err != nil {
				return nil, err
			}
			sortObjs(pkgs, func(x, y *Obj) float64 { return compareStr(x.Get("Package"), y.Get("Package")) })
			byOrigin := js.NewObj()
			kernel := false
			for _, p := range pkgs {
				origin := js.Str(or(p.Get("Origin"), "unknown"))
				byOrigin.Set(origin, js.Num(or(byOrigin.Get(origin), 0))+1)
				if nodesKernelPkgs.MatchString(js.Str(p.Get("Package"))) {
					kernel = true
				}
			}
			res := filterAndLimit(pkgs, a.Get("search"), or(a.Get("limit"), 300), func(p *Obj) []any {
				return []any{p.Get("Package"), p.Get("Title"), p.Get("Origin"), p.Get("Section")}
			})
			return proxmox.Compact(js.Spread(
				js.O(
					"node", node,
					"by_origin", byOrigin,
					"kernel_update", ifThen(kernel, true),
					"note", ifThen(kernel, "Includes a kernel update; a reboot is needed to run it after upgrading."),
				),
				res.Map(func(p *Obj) any {
					return proxmox.C("package", p.Get("Package"), "installed", p.Get("OldVersion"), "new", p.Get("Version"), "origin", p.Get("Origin"), "priority", p.Get("Priority"), "title", p.Get("Title"))
				}),
			)), nil
		},
	})

	c.Define("proxmox_refresh_node_updates", Def{
		Toolset:     "nodes",
		Title:       "Refresh package index",
		Description: "Refresh a node's package index (apt-get update) so proxmox_list_node_updates shows current updates. Runs as a task.",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input:       []Prop{P("node", nodeArg), P("notify", Bool().Opt().D("Send a notification about new packages")), P("wait", waitArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			upid, err := client.Post(ctx, nodesPath(node)+"/apt/update", defined(js.O("notify", a.Get("notify"))))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("node", node))
		},
	})

	c.Define("proxmox_get_package_changelog", Def{
		Toolset:     "nodes",
		Title:       "Get package changelog",
		Description: "Get the changelog of a package available on a node, e.g. to see what an update to pve-manager or proxmox-kernel changes. Truncated to max_chars.",
		Input: []Prop{
			P("node", nodeArg),
			P("package", Str().D("Package name, e.g. pve-manager")),
			P("version", Str().Opt().D("Package version (default the candidate version)")),
			P("max_chars", nodesMaxCharsArg(20000)),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			text, err := client.Get(ctx, nodesPath(node)+"/apt/changelog", defined(js.O("name", a.Get("package"), "version", a.Get("version"))))
			if err != nil {
				return nil, err
			}
			return js.Spread(js.O("node", node, "package", a.Get("package")), nodesTruncate(text, int(js.Num(or(a.Get("max_chars"), 20000))))), nil
		},
	})

	c.Define("proxmox_get_node_package_versions", Def{
		Toolset:     "nodes",
		Title:       "Get Proxmox package versions",
		Description: "List the installed versions of the important Proxmox packages on a node (like `pveversion -v`), including the running kernel.",
		Input:       []Prop{P("node", nodeArg), P("search", searchArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			pkgs, err := client.GetList(ctx, nodesPath(node)+"/apt/versions", nil)
			if err != nil {
				return nil, err
			}
			res := filterAndLimit(pkgs, a.Get("search"), js.Undef, func(p *Obj) []any { return []any{p.Get("Package"), p.Get("Title")} })
			var runningKernel, managerVersion any = js.Undef, js.Undef
			for _, p := range pkgs {
				if p.Get("Package") == "proxmox-ve" {
					runningKernel = p.Get("RunningKernel")
					break
				}
			}
			for _, p := range pkgs {
				if p.Truthy("ManagerVersion") {
					managerVersion = p.Get("ManagerVersion")
					break
				}
			}
			return proxmox.Compact(js.Spread(
				js.O("node", node, "running_kernel", runningKernel, "manager_version", managerVersion),
				res.Map(func(p *Obj) any {
					installed := p.Get("CurrentState") == "Installed"
					return proxmox.C("package", p.Get("Package"), "version", ifThen(installed || !p.Truthy("CurrentState"), p.Get("Version")), "state", ifThen(!installed, p.Get("CurrentState")))
				}),
			)), nil
		},
	})

	c.Define("proxmox_list_node_repositories", Def{
		Toolset:     "nodes",
		Title:       "List APT repositories",
		Description: "List a node's APT repositories by file with enabled/disabled state, URIs, suites and components, any parse errors and warnings (e.g. enterprise repo without subscription, mixed suites), and which standard Proxmox repositories are configured. Use path/index and the digest with proxmox_set_node_repository.",
		Input:       []Prop{P("node", nodeArg), P("raw", rawArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			r, err := client.Get(ctx, nodesPath(node)+"/apt/repositories", nil)
			if err != nil {
				return nil, err
			}
			if a.Truthy("raw") {
				return r, nil
			}
			return js.Spread(js.O("node", node), nodesSummarizeRepositories(nodesObj(r))), nil
		},
	})

	c.Define("proxmox_set_node_repository", Def{
		Toolset:     "nodes",
		Title:       "Enable, disable or add an APT repository",
		Description: "Enable or disable an existing APT repository (give path and index from proxmox_list_node_repositories plus enabled), or add a standard Proxmox repository by handle (e.g. no-subscription, enterprise, test, ceph-squid-no-subscription). Refresh with proxmox_refresh_node_updates afterwards.",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input: []Prop{
			P("node", nodeArg),
			P("handle", Str().Opt().D("Add this standard repository, e.g. 'no-subscription' or 'enterprise' (see standard_repos)")),
			P("path", Str().Opt().D("File of the repository to change, e.g. /etc/apt/sources.list.d/pve-enterprise.sources")),
			P("index", Int().Min(0).Opt().D("Index of the repository within the file")),
			P("enabled", Bool().Opt().D("Enable (true) or disable (false) the repository at path/index")),
			P("digest", digestArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			base := nodesPath(node) + "/apt/repositories"
			if a.Truthy("handle") {
				if a.Defined("path") || a.Defined("index") {
					return nil, errors.New("Give either handle (add a standard repo) or path+index (change one), not both")
				}
				_, err = client.Put(ctx, base, defined(js.O("handle", a.Get("handle"), "digest", a.Get("digest"))))
			} else {
				if !a.Defined("path") || !a.Defined("index") || !a.Defined("enabled") {
					return nil, errors.New("To change a repository give path, index and enabled; to add a standard repository give handle")
				}
				_, err = client.Post(ctx, base, defined(js.O("path", a.Get("path"), "index", a.Get("index"), "enabled", a.Get("enabled"), "digest", a.Get("digest"))))
			}
			if err != nil {
				return nil, err
			}
			r, err := client.GetObj(ctx, base, nil)
			if err != nil {
				return nil, err
			}
			return js.Spread(js.O("node", node, "updated", true), nodesSummarizeRepositories(r)), nil
		},
	})

	// ================================================================ subscription

	c.Define("proxmox_get_node_subscription", Def{
		Toolset:     "nodes",
		Title:       "Get node subscription",
		Description: "Get a node's subscription status, level, product, sockets and next due date. The key itself is never shown.",
		Input:       []Prop{P("node", nodeArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			sub, err := client.Get(ctx, nodesPath(node)+"/subscription", nil)
			if err != nil {
				return nil, err
			}
			return js.Spread(js.O("node", node), nodesSummarizeSubscription(sub)), nil
		},
	})

	c.Define("proxmox_set_node_subscription", Def{
		Toolset:     "nodes",
		Title:       "Set or refresh node subscription",
		Description: "Set a node's subscription key (key given), or re-check the existing subscription with the Proxmox shop server (no key). Returns the resulting status; the key is never echoed back.",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input: []Prop{
			P("node", nodeArg),
			P("key", Str().Opt().D("Subscription key, e.g. pve2c-0123456789 (omit to just refresh the status)")),
			P("force", Bool().Opt().D("Refresh: contact the server even if the cached status is still valid")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			path := nodesPath(node) + "/subscription"
			if a.Truthy("key") {
				_, err = client.Put(ctx, path, js.O("key", a.Get("key")))
			} else {
				_, err = client.Post(ctx, path, defined(js.O("force", a.Get("force"))))
			}
			if err != nil {
				return nil, err
			}
			sub, err := client.Get(ctx, path, nil)
			if err != nil {
				return nil, err
			}
			return js.Spread(js.O("node", node, js.Str(pick(a.Truthy("key"), "key_set", "refreshed")), true), nodesSummarizeSubscription(sub)), nil
		},
	})

	c.Define("proxmox_delete_node_subscription", Def{
		Toolset:     "nodes",
		Title:       "Remove node subscription key",
		Description: "Remove the subscription key from a node (e.g. before moving it to other hardware). The enterprise repository stops working without it.",
		Delete:      true,
		Input:       []Prop{P("node", nodeArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			if _, err := client.Delete(ctx, nodesPath(node)+"/subscription", nil); err != nil {
				return nil, err
			}
			return js.O("node", node, "deleted", true), nil
		},
	})

	// ================================================================ certificates

	c.Define("proxmox_get_node_certificates", Def{
		Toolset:     "nodes",
		Title:       "Get node certificates",
		Description: "List the TLS certificates of a node's web interface/API (pve-root-ca, pve-ssl and any custom or ACME pveproxy-ssl certificate) with subject, issuer, SANs, validity, days left and fingerprint. Private keys are never returned.",
		Input:       []Prop{P("node", nodeArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			certs, err := client.GetList(ctx, nodesPath(node)+"/certificates/info", nil)
			if err != nil {
				return nil, err
			}
			return js.O("node", node, "certificates", mapObjs(certs, nodesSummarizeCert)), nil
		},
	})

	c.Define("proxmox_upload_node_certificate", Def{
		Toolset:     "nodes",
		Title:       "Upload a custom node certificate",
		Description: "Install a custom TLS certificate (PEM chain) and private key for a node's web interface/API (pveproxy-ssl). Replaces an existing custom/ACME certificate only with force=true; restart=true restarts pveproxy to use it. The key is sent to Proxmox only and never returned.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("node", nodeArg),
			P("certificates", Str().D("PEM certificate chain (server certificate first)")),
			P("key", Str().Opt().D("PEM private key (required unless a key is already installed)")),
			P("force", Bool().Opt().D("Overwrite an existing custom or ACME certificate")),
			P("restart", Bool().Opt().D("Restart pveproxy to load the new certificate")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			res, err := client.Post(ctx, nodesPath(node)+"/certificates/custom", defined(js.O("certificates", a.Get("certificates"), "key", a.Get("key"), "force", a.Get("force"), "restart", a.Get("restart"))))
			if err != nil {
				return nil, err
			}
			var cert any = js.Undef
			switch x := res.(type) {
			case *Obj:
				cert = nodesSummarizeCert(x)
			case []any:
				cert = nodesSummarizeCert(js.NewObj())
			}
			return proxmox.C("node", node, "uploaded", true, "certificate", cert), nil
		},
	})

	c.Define("proxmox_delete_node_certificate", Def{
		Toolset:     "nodes",
		Title:       "Delete the custom node certificate",
		Description: "Remove a node's custom (or ACME) web certificate and key, falling back to the self-signed pve-ssl certificate. restart=true restarts pveproxy.",
		Delete:      true,
		Input:       []Prop{P("node", nodeArg), P("restart", Bool().Opt().D("Restart pveproxy afterwards"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			if _, err := client.Delete(ctx, nodesPath(node)+"/certificates/custom", defined(js.O("restart", a.Get("restart")))); err != nil {
				return nil, err
			}
			return js.O("node", node, "deleted", true), nil
		},
	})

	c.Define("proxmox_node_acme_certificate", Def{
		Toolset:     "nodes",
		Title:       "Order, renew or revoke an ACME certificate",
		Description: "Order a new ACME (e.g. Let's Encrypt) certificate for a node, renew it, or revoke it at the CA. The node's ACME account and domains must be set first (proxmox_update_node_config acme_account/acme_domains; accounts via proxmox_save_acme_account, DNS plugins via proxmox_save_acme_plugin). Runs as a task; pveproxy reloads the new certificate. revoke needs delete access (PROXMOX_ALLOW_DELETES).",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("node", nodeArg),
			P("action", Enum("order", "renew", "revoke").D("order a new certificate, renew the existing one, or revoke it")),
			P("force", Bool().Opt().D("order: overwrite an existing custom certificate; renew: renew even if more than 30 days remain")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			action := js.Str(a.Get("action"))
			// Revoking destroys the certificate at the CA, so it needs delete access like the other delete tools.
			if action == "revoke" && !c.AllowDeletes {
				return nil, errors.New("Revoking needs delete access (PROXMOX_ALLOW_DELETES=true)")
			}
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			path := nodesPath(node) + "/certificates/acme/certificate"
			params := defined(js.O("force", ifThen(action != "revoke", a.Get("force"))))
			var upid any
			switch action {
			case "order":
				upid, err = client.Post(ctx, path, params)
			case "renew":
				upid, err = client.Put(ctx, path, params)
			default:
				upid, err = client.Delete(ctx, path, nil)
			}
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("node", node, "action", action))
		},
	})

	// ================================================================ cluster ACME accounts & plugins

	c.Define("proxmox_list_acme_accounts", Def{
		Toolset:     "nodes",
		Title:       "List ACME accounts",
		Description: "List the cluster's registered ACME accounts (used to order node certificates), with their CA directory and contacts.",
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			accounts, err := client.GetList(ctx, "/cluster/acme/account", nil)
			if err != nil {
				return nil, err
			}
			return mapObjs(accounts, func(acc *Obj) any {
				d := js.NewObj()
				if v, err := client.Get(ctx, "/cluster/acme/account/"+enc(acc.Get("name")), nil); err == nil {
					d = nodesObj(v)
				}
				account := d.ObjAt("account")
				return proxmox.C("name", acc.Get("name"), "directory", d.Get("directory"), "status", account.Get("status"), "contact", account.Get("contact"))
			}), nil
		},
	})

	c.Define("proxmox_get_acme_account", Def{
		Toolset:     "nodes",
		Title:       "Get ACME account",
		Description: "Get an ACME account's details: CA directory, account URL (location), status, contacts and accepted terms of service. Keys are redacted.",
		Input:       []Prop{P("name", Str().Opt().D("Account name (default 'default')"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			name := or(a.Get("name"), "default")
			v, err := client.Get(ctx, "/cluster/acme/account/"+enc(name), nil)
			if err != nil {
				return nil, err
			}
			return js.Spread(js.O("name", name), proxmox.RedactSecrets(v)), nil
		},
	})

	c.Define("proxmox_save_acme_account", Def{
		Toolset:     "nodes",
		Title:       "Register or update an ACME account",
		Description: "Register a new ACME account with a CA (create=true; Let's Encrypt by default, accepting its terms of service with tos_url from proxmox_list_acme_directories), or update an existing account's contact email (or refresh it when no contact is given). Runs as a task.",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("name", Str().Opt().D("Account name (default 'default')")),
			P("create", Bool().Opt().D("Register a new account (default false = update)")),
			P("contact", Str().Opt().D("Contact email address(es), comma-separated (required when creating)")),
			P("directory", Str().Opt().D("ACME directory URL (create only; default Let's Encrypt production)")),
			P("tos_url", Str().Opt().D("Terms of service URL being agreed to (create only; required by most CAs)")),
			P("eab_kid", Str().Opt().D("External account binding key ID (create only, for CAs that require EAB)")),
			P("eab_hmac_key", Str().Opt().D("External account binding HMAC key (create only)")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			name := or(a.Get("name"), "default")
			var upid any
			var err error
			if a.Truthy("create") {
				if !a.Truthy("contact") {
					return nil, errors.New("contact is required when registering an ACME account")
				}
				upid, err = client.Post(ctx, "/cluster/acme/account", defined(js.O(
					"name", name, "contact", a.Get("contact"), "directory", a.Get("directory"), "tos_url", a.Get("tos_url"), "eab-kid", a.Get("eab_kid"), "eab-hmac-key", a.Get("eab_hmac_key"),
				)))
			} else {
				upid, err = client.Put(ctx, "/cluster/acme/account/"+enc(name), defined(js.O("contact", a.Get("contact"))))
			}
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("name", name, js.Str(pick(a.Truthy("create"), "registered", "updated")), true))
		},
	})

	c.Define("proxmox_delete_acme_account", Def{
		Toolset:     "nodes",
		Title:       "Deactivate an ACME account",
		Description: "Deactivate an ACME account at the CA and remove it from the cluster. Nodes using it can no longer order or renew certificates. Runs as a task.",
		Delete:      true,
		Input:       []Prop{P("name", Str().D("Account name")), P("wait", waitArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			upid, err := client.Delete(ctx, "/cluster/acme/account/"+enc(a.Get("name")), nil)
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("name", a.Get("name"), "deleted", true))
		},
	})

	c.Define("proxmox_list_acme_directories", Def{
		Toolset:     "nodes",
		Title:       "List ACME directories",
		Description: "List the known ACME CA directories (Let's Encrypt production/staging...). With `directory`, also fetch that CA's metadata: terms-of-service URL (needed to register an account) and whether external account binding is required. Fetching `directory` makes the node connect to that URL, so it needs write access.",
		Input:       []Prop{P("directory", Str().Opt().D("Also fetch metadata for this directory URL"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			directories, err := client.Get(ctx, "/cluster/acme/directories", nil)
			if err != nil {
				return nil, err
			}
			if !a.Truthy("directory") {
				return js.O("directories", directories), nil
			}
			// Fetching a directory's metadata makes the node connect to the given URL.
			if !c.AllowWrites {
				return nil, proxmox.Errorf("Fetching metadata for `directory` makes the node connect to that URL and needs write access (PROXMOX_ALLOW_WRITES=true); without it only the known directories are listed")
			}
			meta, err := client.Get(ctx, "/cluster/acme/meta", js.O("directory", a.Get("directory")))
			if err != nil {
				// Older versions only have /tos.
				tos, err := client.Get(ctx, "/cluster/acme/tos", js.O("directory", a.Get("directory")))
				if err != nil {
					return nil, err
				}
				meta = js.O("termsOfService", tos)
			}
			return js.O("directories", directories, "meta", proxmox.Compact(js.Spread(js.O("directory", a.Get("directory")), meta))), nil
		},
	})

	c.Define("proxmox_list_acme_plugins", Def{
		Toolset:     "nodes",
		Title:       "List ACME challenge plugins",
		Description: "List the cluster's ACME challenge plugins (standalone HTTP and DNS API plugins). Credentials in plugin data are redacted; only their keys are shown.",
		Input:       []Prop{P("type", Enum("dns", "standalone").Opt().D("Only plugins of this type"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			plugins, err := client.GetList(ctx, "/cluster/acme/plugins", defined(js.O("type", a.Get("type"))))
			if err != nil {
				return nil, err
			}
			return mapObjs(plugins, func(p *Obj) any { return nodesSummarizePlugin(p) }), nil
		},
	})

	c.Define("proxmox_get_acme_plugin", Def{
		Toolset:     "nodes",
		Title:       "Get ACME plugin",
		Description: "Get one ACME challenge plugin's configuration (DNS API, nodes, validation delay). Credential values are redacted.",
		Input:       []Prop{P("id", Str().D("Plugin ID"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			p, err := client.Get(ctx, "/cluster/acme/plugins/"+enc(a.Get("id")), nil)
			if err != nil {
				return nil, err
			}
			return nodesSummarizePlugin(p), nil
		},
	})

	c.Define("proxmox_get_acme_challenge_schema", Def{
		Toolset:     "nodes",
		Title:       "Get ACME DNS plugin schema",
		Description: "Show which credential fields each ACME DNS API plugin needs (e.g. CF_Token for Cloudflare), to fill `data` in proxmox_save_acme_plugin. Filter by api name.",
		Input:       []Prop{P("api", Str().Opt().D("Only this DNS API, e.g. cf, ovh, hetzner, route53 is 'aws'"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			schema, err := client.GetList(ctx, "/cluster/acme/challenge-schema", nil)
			if err != nil {
				return nil, err
			}
			if a.Truthy("api") {
				schema = filterObjs(schema, func(s *Obj) bool { return s.Get("id") == a.Get("api") })
			}
			return mapObjs(schema, func(s *Obj) any {
				fields := s.ObjAt("schema").Get("fields")
				if js.Nullish(fields) {
					fields = s.Get("schema")
				}
				return proxmox.C("api", s.Get("id"), "name", s.Get("name"), "type", s.Get("type"), "fields", fields)
			}), nil
		},
	})

	c.Define("proxmox_save_acme_plugin", Def{
		Toolset:     "nodes",
		Title:       "Create or update an ACME plugin",
		Description: "Create (create=true) or update an ACME challenge plugin. For DNS validation give api (e.g. cf) and data with the API credentials as key/value pairs (see proxmox_get_acme_challenge_schema); they are base64-encoded for Proxmox and never echoed back. On update, data replaces all existing credentials.",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("id", Str().D("Plugin ID, e.g. cloudflare")),
			P("create", Bool().Opt().D("Create a new plugin (default false = update)")),
			P("type", Enum("dns", "standalone").Opt().D("Challenge type (create only; default dns)")),
			P("api", Str().Opt().D("DNS API plugin name, e.g. cf, ovh, hetzner, aws, gcloud, desec")),
			P("data", Rec(Str()).Opt().D("DNS API credentials, e.g. {\"CF_Token\": \"...\", \"CF_Account_ID\": \"...\"}")),
			P("nodes", Arr(Str()).Opt().D("Restrict the plugin to these nodes (default all)")),
			P("validation_delay", Int().Min(0).Max(172800).Opt().D("Seconds to wait before validation, for slow DNS propagation (default 30)")),
			P("disable", Bool().Opt().D("Disable the plugin")),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			var data any = js.Undef
			if d, ok := a.Get("data").(*Obj); ok {
				data = nodesEncodePluginData(d)
			}
			var nodes any = js.Undef
			if arr, ok := a.Get("nodes").([]any); ok {
				nodes = js.Str(arr)
			}
			params := defined(js.O(
				"api", a.Get("api"),
				"data", data,
				"nodes", nodes,
				"validation-delay", a.Get("validation_delay"),
				"disable", a.Get("disable"),
			))
			var err error
			if a.Truthy("create") {
				_, err = client.Post(ctx, "/cluster/acme/plugins", js.Spread(js.O("id", a.Get("id"), "type", or(a.Get("type"), "dns")), params))
			} else {
				_, err = client.Put(ctx, "/cluster/acme/plugins/"+enc(a.Get("id")), defined(js.Spread(params, js.O("delete", deleteParam(a.Get("delete")), "digest", a.Get("digest")))))
			}
			if err != nil {
				return nil, err
			}
			p, err := client.Get(ctx, "/cluster/acme/plugins/"+enc(a.Get("id")), nil)
			if err != nil {
				return nil, err
			}
			return js.O("saved", a.Get("id"), "plugin", nodesSummarizePlugin(p)), nil
		},
	})

	c.Define("proxmox_delete_acme_plugin", Def{
		Toolset:     "nodes",
		Title:       "Delete an ACME plugin",
		Description: "Delete an ACME challenge plugin. Node certificate domains that use it can no longer be validated.",
		Delete:      true,
		Input:       []Prop{P("id", Str().D("Plugin ID"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/cluster/acme/plugins/"+enc(a.Get("id")), nil); err != nil {
				return nil, err
			}
			return js.O("deleted", a.Get("id")), nil
		},
	})
}
