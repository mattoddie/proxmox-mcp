package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

// ---------------------------------------------------------------- shared arguments

var (
	vmsDiskRe      = regexp.MustCompile(`^(ide[0-3]|sata[0-5]|scsi([0-9]|[12][0-9]|30)|virtio([0-9]|1[0-5])|efidisk0|tpmstate0)$`)
	vmsDiskKeyArg  = Str().Pattern(`^(ide[0-3]|sata[0-5]|scsi([0-9]|[12][0-9]|30)|virtio([0-9]|1[0-5])|efidisk0|tpmstate0)$`, "a disk key such as scsi0, virtio1, sata0, ide0, efidisk0 or tpmstate0").D("Disk key, e.g. scsi0, virtio1, sata0")
	vmsSkiplockArg = Bool().Opt().D("Ignore a config lock (root@pam only; use with care)")
	vmsOstypes     = []string{"other", "wxp", "w2k", "w2k3", "w2k8", "wvista", "win7", "win8", "win10", "win11", "l24", "l26", "solaris"}
	vmsScsihw      = []string{"lsi", "lsi53c810", "virtio-scsi-pci", "virtio-scsi-single", "megasas", "pvscsi"}
	vmsNicModels   = []string{"virtio", "e1000", "e1000e", "rtl8139", "vmxnet3"}
	vmsOptionsArg  = Rec(Union(Str(), Num(), Bool())).Opt().D("Extra property-string options, e.g. {discard: \"on\", ssd: true, iothread: true}")
)

// vmsCommonFields are the config fields that create and update share; mapped to API params by vmsCommonParams.
func vmsCommonFields() []Prop {
	return []Prop{
		P("name", Str().Opt().D("VM name (DNS-style: letters, digits, '-' and '.')")),
		P("memory_mb", Int().Pos().Opt().D("Memory in MiB, e.g. 4096")),
		P("balloon_mb", Int().Min(0).Opt().D("Minimum memory for the balloon driver in MiB (0 disables ballooning)")),
		P("cores", Int().Pos().Opt().D("CPU cores per socket")),
		P("sockets", Int().Pos().Opt().D("CPU sockets")),
		P("cpu", Str().Opt().D("CPU type, e.g. host, x86-64-v2-AES, kvm64 (or a full cpu property string)")),
		P("numa", Bool().Opt().D("Enable NUMA")),
		P("ostype", Enum(vmsOstypes...).Opt().D("Guest OS type: l26 = Linux 2.6+, win11/win10..., other")),
		P("scsihw", Enum(vmsScsihw...).Opt().D("SCSI controller model (virtio-scsi-single recommended)")),
		P("bios", Enum("seabios", "ovmf").Opt().D("Firmware: seabios (legacy BIOS) or ovmf (UEFI; needs an EFI disk)")),
		P("machine", Str().Opt().D("QEMU machine type, e.g. q35, pc, pc-q35-9.0")),
		P("vga", Str().Opt().D("Display, e.g. std, virtio, qxl, serial0, none")),
		P("agent", Union(Bool(), Str()).Opt().D("QEMU guest agent: true/false, or a property string like 'enabled=1,fstrim_cloned_disks=1'")),
		P("onboot", Bool().Opt().D("Start the VM when the node boots")),
		P("startup", Str().Opt().D("Startup/shutdown order and delays, e.g. 'order=2,up=30,down=60'")),
		P("protection", Bool().Opt().D("Protect the VM and its disks from removal")),
		P("tags", Arr(Str()).Opt().D("Tags (replaces all existing tags)")),
		P("description", Str().Opt().D("Notes shown in the VM summary (Markdown)")),
		P("boot_order", Arr(Str()).Opt().D("Boot device order, e.g. [\"scsi0\", \"ide2\", \"net0\"]")),
		P("hotplug", Str().Opt().D("Hotplug features, e.g. 'network,disk,usb' or '0' to disable")),
		P("ciuser", Str().Opt().D("cloud-init: user to create/configure")),
		P("cipassword", Str().Opt().D("cloud-init: password for the user (prefer sshkeys)")),
		P("sshkeys", Union(Str(), Arr(Str())).Opt().D("cloud-init: public SSH keys (OpenSSH format, one per entry or newline-separated). URL-encoded for Proxmox automatically.")),
		P("ipconfig", RecKeys(Str().Pattern(`^\d+$`), Str()).Opt().D("cloud-init IP config per NIC index, e.g. {\"0\": \"ip=dhcp\", \"1\": \"ip=10.0.0.5/24,gw=10.0.0.1\"} (ip6=auto/dhcp also allowed)")),
		P("nameserver", Str().Opt().D("cloud-init: DNS servers (space-separated)")),
		P("searchdomain", Str().Opt().D("cloud-init: DNS search domains")),
		P("citype", Enum("configdrive2", "nocloud", "opennebula").Opt().D("cloud-init data format (default depends on ostype)")),
		P("ciupgrade", Bool().Opt().D("cloud-init: upgrade packages on first boot (default true)")),
		P("cicustom", Str().Opt().D("cloud-init: custom snippet files, e.g. 'user=local:snippets/user.yaml'")),
	}
}

func vmsJoin(v any, sep string) string {
	arr, _ := v.([]any)
	parts := make([]string, len(arr))
	for i, x := range arr {
		if !js.Nullish(x) {
			parts[i] = js.Str(x)
		}
	}
	return strings.Join(parts, sep)
}

func vmsCommonParams(a *Obj) *Obj {
	ipconfig := js.NewObj()
	if ip := a.ObjAt("ipconfig"); ip != nil {
		for _, i := range ip.Keys() {
			ipconfig.Set("ipconfig"+i, ip.Get(i))
		}
	}
	agent := a.Get("agent")
	if b, ok := agent.(bool); ok {
		agent = pick(b, "1", "0")
	}
	var tags any = js.Undef
	if a.Truthy("tags") {
		tags = vmsJoin(a.Get("tags"), ";")
	}
	var boot any = js.Undef
	if a.Truthy("boot_order") {
		boot = "order=" + vmsJoin(a.Get("boot_order"), ";")
	}
	return defined(js.Spread(js.O(
		"name", a.Get("name"),
		"memory", a.Get("memory_mb"),
		"balloon", a.Get("balloon_mb"),
		"cores", a.Get("cores"),
		"sockets", a.Get("sockets"),
		"cpu", a.Get("cpu"),
		"numa", a.Get("numa"),
		"ostype", a.Get("ostype"),
		"scsihw", a.Get("scsihw"),
		"bios", a.Get("bios"),
		"machine", a.Get("machine"),
		"vga", a.Get("vga"),
		"agent", agent,
		"onboot", a.Get("onboot"),
		"startup", a.Get("startup"),
		"protection", a.Get("protection"),
		"tags", tags,
		"description", a.Get("description"),
		"boot", boot,
		"hotplug", a.Get("hotplug"),
		"ciuser", a.Get("ciuser"),
		"cipassword", a.Get("cipassword"),
		"sshkeys", vmsEncodeSshKeys(a.Get("sshkeys")),
		"nameserver", a.Get("nameserver"),
		"searchdomain", a.Get("searchdomain"),
		"citype", a.Get("citype"),
		"ciupgrade", a.Get("ciupgrade"),
		"cicustom", a.Get("cicustom"),
	), ipconfig))
}

// vmsEncodeSshKeys: Proxmox wants sshkeys URL-encoded (and rejects '+' for spaces), one key per line.
func vmsEncodeSshKeys(keys any) any {
	if js.IsUndef(keys) {
		return js.Undef
	}
	text := js.Str(keys)
	if _, ok := keys.([]any); ok {
		text = vmsJoin(keys, "\n")
	}
	return js.EncodeURIComponent(strings.TrimFunc(text, unicode.IsSpace))
}

// ---------------------------------------------------------------- summaries

// vmsRedactValue redacts a single config value by its key (e.g. cipassword).
func vmsRedactValue(key string, value any) any {
	if js.IsUndef(value) {
		return js.Undef
	}
	return proxmox.RedactSecrets(js.O(key, value)).(*Obj).Get(key)
}

func vmsSplitTags(tags any) any {
	if !js.Truthy(tags) {
		return js.Undef
	}
	return splitTags(js.Str(tags))
}

var (
	vmsLineRe  = regexp.MustCompile(`\r?\n`)
	vmsSpaceRe = regexp.MustCompile(`\s+`)
)

// vmsDescribeSshKeys gives the key type and comment of each SSH key, so the model sees which
// keys are set without the key material.
func vmsDescribeSshKeys(value any) any {
	text, ok := value.(string)
	if !ok || text == "" {
		return js.Undef
	}
	if d, err := decodeURIComponent(text); err == nil {
		text = d
	}
	out := []any{}
	for _, l := range vmsLineRe.Split(text, -1) {
		l = strings.TrimFunc(l, unicode.IsSpace)
		if l == "" {
			continue
		}
		parts := vmsSpaceRe.Split(l, -1)
		var keep []string
		if parts[0] != "" {
			keep = append(keep, parts[0])
		}
		if len(parts) > 2 {
			if rest := strings.Join(parts[2:], " "); rest != "" {
				keep = append(keep, rest)
			}
		}
		out = append(out, strings.Join(keep, " "))
	}
	return out
}

var (
	vmsDeviceKeyRe = regexp.MustCompile(`^(hostpci|usb|serial|parallel|audio|rng|virtiofs|watchdog|ivshmem|numa)\d*$`)
	vmsIpconfigRe  = regexp.MustCompile(`^ipconfig\d+$`)
	vmsCloudinitRe = regexp.MustCompile(`cloudinit`)
)

func vmsSummarizeConfig(c *Obj) *Obj {
	mem := proxmox.ParsePropertyString(c.Get("memory"), "current")
	ipconfig := js.NewObj()
	devices := js.NewObj()
	for _, k := range c.Keys() {
		if vmsIpconfigRe.MatchString(k) {
			ipconfig.Set(k, c.Get(k))
		}
	}
	for _, k := range c.Keys() {
		if vmsDeviceKeyRe.MatchString(k) {
			devices.Set(k, c.Get(k))
		}
	}
	disks := proxmox.SummarizeDisks(c)
	hasCloudInit := false
	for _, k := range disks.Keys() {
		if vmsCloudinitRe.MatchString(js.Str(or(disks.ObjAt(k).Get("file"), ""))) {
			hasCloudInit = true
		}
	}
	boot := js.NewObj()
	if _, ok := c.Get("boot").(string); ok {
		boot = proxmox.ParsePropertyString(c.Get("boot"), "legacy")
	}
	var balloon any = js.Undef
	if c.Defined("balloon") {
		if c.Num("balloon") == 0 {
			balloon = "disabled"
		} else {
			balloon = proxmox.FormatBytes(c.Num("balloon") * 1024 * 1024)
		}
	}
	var bootOrder any = js.Undef
	if boot.Truthy("order") {
		bootOrder = js.Array(strings.Split(js.Str(boot.Get("order")), ";"))
	} else if c.Truthy("bootdisk") {
		bootOrder = []any{c.Get("bootdisk")}
	}
	var cloudInit any = js.Undef
	if hasCloudInit || c.Truthy("ciuser") || c.Truthy("cipassword") || c.Truthy("sshkeys") || c.Truthy("cicustom") || ipconfig.Len() > 0 {
		cloudInit = proxmox.C(
			"drive", ifThen(hasCloudInit, true),
			"type", c.Get("citype"),
			"user", c.Get("ciuser"),
			"password_set", ifThen(c.Truthy("cipassword"), true),
			"ssh_keys", vmsDescribeSshKeys(c.Get("sshkeys")),
			"ipconfig", ipconfig,
			"nameserver", c.Get("nameserver"),
			"searchdomain", c.Get("searchdomain"),
			"upgrade", proxmox.Flag(c.Get("ciupgrade")),
			"custom", c.Get("cicustom"),
		)
	}
	return proxmox.C(
		"name", c.Get("name"),
		"template", ifThen(js.Truthy(proxmox.Flag(c.Get("template"))), true),
		"lock", c.Get("lock"),
		"ostype", c.Get("ostype"),
		"cpu", proxmox.C(
			"sockets", js.Num(or(c.Get("sockets"), 1)),
			"cores", js.Num(or(c.Get("cores"), 1)),
			"vcpus", c.Get("vcpus"),
			"type", c.Get("cpu"),
			"limit", c.Get("cpulimit"),
			"units", c.Get("cpuunits"),
			"affinity", c.Get("affinity"),
			"numa", orUndef(proxmox.Flag(c.Get("numa"))),
		),
		"memory", proxmox.C(
			"size", ifThen(mem.Truthy("current"), proxmox.FormatBytes(mem.Num("current")*1024*1024)),
			"balloon_min", balloon,
			"hugepages", c.Get("hugepages"),
			"shares", c.Get("shares"),
		),
		"bios", c.Get("bios"),
		"machine", c.Get("machine"),
		"scsihw", c.Get("scsihw"),
		"vga", c.Get("vga"),
		"boot_order", bootOrder,
		"disks", disks,
		"nets", proxmox.SummarizeNets(c),
		"devices", devices,
		"agent", c.Get("agent"),
		"onboot", proxmox.Flag(c.Get("onboot")),
		"startup", c.Get("startup"),
		"protection", orUndef(proxmox.Flag(c.Get("protection"))),
		"hotplug", c.Get("hotplug"),
		"cloud_init", cloudInit,
		"tags", vmsSplitTags(c.Get("tags")),
		"description", c.Get("description"),
		"digest", c.Get("digest"),
	)
}

func vmsSummarizeStatus(s *Obj) *Obj {
	running := s.Get("status") == "running"
	pressure := proxmox.C(
		"cpu_some", orUndef(s.Get("pressurecpusome")),
		"cpu_full", orUndef(s.Get("pressurecpufull")),
		"io_some", orUndef(s.Get("pressureiosome")),
		"io_full", orUndef(s.Get("pressureiofull")),
		"memory_some", orUndef(s.Get("pressurememorysome")),
		"memory_full", orUndef(s.Get("pressurememoryfull")),
	)
	var memory any
	if running {
		memory = proxmox.Usage(s.Get("mem"), s.Get("maxmem"))
	} else {
		memory = proxmox.FormatBytes(s.Get("maxmem"))
	}
	var ha any = js.Undef
	if h := s.ObjAt("ha"); h.Truthy("managed") {
		ha = proxmox.C("state", h.Get("state"), "group", h.Get("group"))
	}
	return proxmox.C(
		"status", s.Get("status"),
		"qmpstatus", ifThen(s.Truthy("qmpstatus") && s.Get("qmpstatus") != s.Get("status"), s.Get("qmpstatus")),
		"lock", s.Get("lock"),
		"template", orUndef(proxmox.Flag(s.Get("template"))),
		"uptime", ifThen(running, proxmox.FormatUptime(s.Get("uptime"))),
		"cpus", s.Get("cpus"),
		"cpu_pct", ifThen(running, proxmox.FractionPct(s.Get("cpu"))),
		"memory", memory,
		"host_memory", ifThen(running, proxmox.FormatBytes(s.Get("memhost"))),
		"balloon_target", ifThen(running && s.Truthy("balloon") && s.Num("balloon") != s.Num("maxmem"), proxmox.FormatBytes(s.Get("balloon"))),
		"disk_read", ifThen(running, proxmox.FormatBytes(s.Get("diskread"))),
		"disk_write", ifThen(running, proxmox.FormatBytes(s.Get("diskwrite"))),
		"net_in", ifThen(running, proxmox.FormatBytes(s.Get("netin"))),
		"net_out", ifThen(running, proxmox.FormatBytes(s.Get("netout"))),
		"pressure_pct", ifThen(running, pressure),
		"pid", s.Get("pid"),
		"running_qemu", s.Get("running-qemu"),
		"running_machine", s.Get("running-machine"),
		"agent_enabled", proxmox.Flag(s.Get("agent")),
		"ha", ha,
		"tags", vmsSplitTags(s.Get("tags")),
	)
}

func vmsDeleteFlag(v any) any {
	if !js.Truthy(v) {
		return js.Undef
	}
	if js.Num(v) == 2 {
		return "force"
	}
	return true
}

// vmsPendingChanges lists the entries of /pending (or /cloudinit) that differ from the running
// config, with secrets redacted.
func vmsPendingChanges(entries []*Obj) []any {
	out := []any{}
	for _, e := range entries {
		if e.Get("key") == "digest" || !(e.Defined("pending") || e.Truthy("delete")) {
			continue
		}
		key := js.Str(e.Get("key"))
		out = append(out, proxmox.C(
			"key", e.Get("key"),
			"current", vmsRedactValue(key, e.Get("value")),
			"pending", vmsRedactValue(key, e.Get("pending")),
			"delete", vmsDeleteFlag(e.Get("delete")),
		))
	}
	return out
}

// ---------------------------------------------------------------- RRD statistics

type vmsMetric struct {
	name string
	get  func(p *Obj) (float64, bool)
	fmt  func(n float64) any
}

func vmsNum(v any) (float64, bool) {
	if js.Nullish(v) || v == "" {
		return 0, false
	}
	n := js.Num(v)
	return n, js.IsFinite(n)
}

func vmsField(k string) func(*Obj) (float64, bool) {
	return func(p *Obj) (float64, bool) { return vmsNum(p.Get(k)) }
}

func vmsRate(n float64) any   { return js.Str(proxmox.FormatBytes(n)) + "/s" }
func vmsRound1(n float64) any { return js.Round(n*10) / 10 }
func vmsBytes(n float64) any  { return proxmox.FormatBytes(js.Round(n)) }

var vmsMetrics = []vmsMetric{
	{"cpu_pct", func(p *Obj) (float64, bool) {
		n, ok := vmsNum(p.Get("cpu"))
		return n * 100, ok
	}, vmsRound1},
	{"memory", vmsField("mem"), vmsBytes},
	{"host_memory", vmsField("memhost"), vmsBytes},
	{"net_in", vmsField("netin"), vmsRate},
	{"net_out", vmsField("netout"), vmsRate},
	{"disk_read", vmsField("diskread"), vmsRate},
	{"disk_write", vmsField("diskwrite"), vmsRate},
	{"pressure_cpu_some_pct", vmsField("pressurecpusome"), vmsRound1},
	{"pressure_io_some_pct", vmsField("pressureiosome"), vmsRound1},
	{"pressure_memory_some_pct", vmsField("pressurememorysome"), vmsRound1},
}

func vmsValues(pts []*Obj, m vmsMetric) []float64 {
	var vals []float64
	for _, p := range pts {
		if v, ok := m.get(p); ok {
			vals = append(vals, v)
		}
	}
	return vals
}

func vmsSummarizeRrd(points []*Obj, seriesPoints int) *Obj {
	var pts []*Obj
	for _, p := range points {
		if _, ok := vmsNum(p.Get("time")); ok {
			pts = append(pts, p)
		}
	}
	sortObjs(pts, func(x, y *Obj) float64 { return x.Num("time") - y.Num("time") })
	summary := js.NewObj()
	for _, m := range vmsMetrics {
		vals := vmsValues(pts, m)
		if len(vals) == 0 {
			continue
		}
		sum, lo, hi := 0.0, math.Inf(1), math.Inf(-1)
		for _, v := range vals {
			sum += v
			lo = math.Min(lo, v)
			hi = math.Max(hi, v)
		}
		summary.Set(m.name, js.O("min", m.fmt(lo), "avg", m.fmt(sum/float64(len(vals))), "max", m.fmt(hi)))
	}
	maxmem, haveMax := 0.0, false
	for _, p := range pts {
		if v, ok := vmsNum(p.Get("maxmem")); ok {
			maxmem, haveMax = v, true
		}
	}
	if mem := summary.ObjAt("memory"); mem != nil && haveMax && maxmem != 0 {
		mem.Set("of", proxmox.FormatBytes(maxmem))
	}
	series := []any{}
	if seriesPoints > 0 && len(pts) > 0 {
		size := int(math.Ceil(float64(len(pts)) / float64(seriesPoints)))
		for i := 0; i < len(pts); i += size {
			bucket := pts[i:min(i+size, len(pts))]
			row := js.O("time", proxmox.IsoTime(bucket[0].Get("time")))
			for _, m := range vmsMetrics {
				if !summary.Truthy(m.name) {
					continue
				}
				vals := vmsValues(bucket, m)
				if len(vals) > 0 {
					sum := 0.0
					for _, v := range vals {
						sum += v
					}
					row.Set(m.name, m.fmt(sum/float64(len(vals))))
				}
			}
			series = append(series, row)
		}
	}
	var from, to any = js.Undef, js.Undef
	if len(pts) > 0 {
		from = proxmox.IsoTime(pts[0].Get("time"))
		to = proxmox.IsoTime(pts[len(pts)-1].Get("time"))
	}
	return proxmox.C("from", from, "to", to, "samples", len(pts), "summary", summary, "series", series)
}

// ---------------------------------------------------------------- sendkey text conversion

var vmsShifted = map[rune]string{
	'!': "1", '@': "2", '#': "3", '$': "4", '%': "5", '^': "6", '&': "7", '*': "8", '(': "9", ')': "0",
	'_': "minus", '+': "equal", '{': "bracket_left", '}': "bracket_right", '|': "backslash", ':': "semicolon",
	'"': "apostrophe", '<': "comma", '>': "dot", '?': "slash", '~': "grave_accent",
}

var vmsPlain = map[rune]string{
	' ': "spc", '-': "minus", '=': "equal", '[': "bracket_left", ']': "bracket_right", '\\': "backslash", ';': "semicolon",
	'\'': "apostrophe", ',': "comma", '.': "dot", '/': "slash", '`': "grave_accent", '\n': "ret", '\t': "tab",
}

// vmsTextToKeys turns text into QEMU sendkey names for a US keyboard layout.
func vmsTextToKeys(text string) ([]string, error) {
	keys := []string{}
	for _, ch := range text {
		switch {
		case ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9':
			keys = append(keys, string(ch))
		case ch >= 'A' && ch <= 'Z':
			keys = append(keys, "shift-"+strings.ToLower(string(ch)))
		case vmsPlain[ch] != "":
			keys = append(keys, vmsPlain[ch])
		case vmsShifted[ch] != "":
			keys = append(keys, "shift-"+vmsShifted[ch])
		case ch == '\r':
			continue
		default:
			return nil, fmt.Errorf("Cannot type character %s with sendkey (US layout, ASCII only)", js.Quote(string(ch)))
		}
	}
	return keys, nil
}

// ---------------------------------------------------------------- disk spec building for create

var vmsDiskSpec = Object(
	P("bus", Enum("scsi", "virtio", "sata", "ide").Opt().D("Controller bus (default scsi)")),
	P("index", Int().Min(0).Max(30).Opt().D("Slot number on the bus (default: next free)")),
	P("storage", Str().Opt().D("Storage to allocate on (default: the top-level `storage`)")),
	P("size_gb", Num().Pos().Opt().D("Size in GiB (required unless import_from or spec is given)")),
	P("import_from", Str().Opt().D("Import an existing image as this disk, e.g. 'local:import/debian-12.qcow2' or a volume ID")),
	P("format", Enum("raw", "qcow2", "vmdk").Opt().D("Image format on file-based storage")),
	P("options", vmsOptionsArg),
	P("spec", Str().Opt().D("Raw Proxmox disk property string (overrides storage/size/options), e.g. 'local-lvm:32,ssd=1'")),
)

var vmsNetSpec = Object(
	P("model", Enum(vmsNicModels...).Opt().D("NIC model (default virtio)")),
	P("bridge", Str().Opt().D("Bridge (default vmbr0)")),
	P("vlan", Int().Min(1).Max(4094).Opt().D("VLAN tag")),
	P("firewall", Bool().Opt().D("Enable the Proxmox firewall on this NIC")),
	P("macaddr", Str().Opt().D("MAC address (default: generated)")),
	P("options", vmsOptionsArg),
	P("spec", Str().Opt().D("Raw Proxmox net property string (overrides the other fields), e.g. 'virtio,bridge=vmbr1,tag=20'")),
)

var vmsBusMax = map[string]int{"scsi": 30, "virtio": 15, "sata": 5, "ide": 3}

type vmsSlotAllocator struct{ used map[string]bool }

func (s *vmsSlotAllocator) take(key string) (string, error) {
	if s.used[key] {
		return "", fmt.Errorf("Disk slot %s is used twice", key)
	}
	if !vmsDiskRe.MatchString(key) {
		return "", fmt.Errorf("Invalid disk slot %s", key)
	}
	s.used[key] = true
	return key, nil
}

func (s *vmsSlotAllocator) next(bus string, prefer ...int) (string, error) {
	order := append([]int(nil), prefer...)
	for n := 0; n <= vmsBusMax[bus]; n++ {
		order = append(order, n)
	}
	for _, i := range order {
		if key := fmt.Sprintf("%s%d", bus, i); !s.used[key] {
			return s.take(key)
		}
	}
	return "", fmt.Errorf("No free %s slot left", bus)
}

func vmsBuildDisk(d *Obj, defaultStorage any) (string, error) {
	if d.Truthy("spec") {
		return d.Str("spec"), nil
	}
	storage := or(d.Get("storage"), defaultStorage)
	if !js.Truthy(storage) {
		return "", errors.New("Each disk needs a storage (or set the top-level `storage`), or a raw `spec`")
	}
	if !d.Truthy("import_from") && !d.Defined("size_gb") {
		return "", errors.New("Each disk needs size_gb (or import_from / spec)")
	}
	size := d.Get("size_gb")
	if d.Truthy("import_from") {
		size = 0.0
	}
	o := js.O("_default", js.Str(storage)+":"+js.Str(size), "import-from", d.Get("import_from"), "format", d.Get("format"))
	if opts := d.ObjAt("options"); opts != nil {
		o.Assign(opts)
	}
	return proxmox.ToPropertyString(o, "_default"), nil
}

func vmsBuildNet(n *Obj) string {
	if n.Truthy("spec") {
		return n.Str("spec")
	}
	model := or(n.Get("model"), "virtio")
	def := model
	if n.Truthy("macaddr") {
		def = js.Str(model) + "=" + js.Str(n.Get("macaddr"))
	}
	o := js.O("_default", def, "bridge", or(n.Get("bridge"), "vmbr0"), "tag", n.Get("vlan"), "firewall", n.Get("firewall"))
	if opts := n.ObjAt("options"); opts != nil {
		o.Assign(opts)
	}
	return proxmox.ToPropertyString(o, "_default")
}

var vmsBootKeyRe = regexp.MustCompile(`^([a-z]+)(\d+)$`)

// vmsBootSort orders boot devices: disks in bus order (scsi, virtio, sata, ide) then index,
// CD-ROM (ide2) last among disks.
func vmsBootSort(x, y string) float64 {
	rank := func(k string) float64 {
		m := vmsBootKeyRe.FindStringSubmatch(k)
		bus := -1
		for i, b := range []string{"scsi", "virtio", "sata", "ide"} {
			if b == m[1] {
				bus = i
			}
		}
		if k == "ide2" {
			return 1000
		}
		return float64(bus*100) + js.Num(m[2])
	}
	return rank(x) - rank(y)
}

var vmsCloudInitSecretRe = regexp.MustCompile(`(?im)^(\s*(?:password|passwd|hashed_passwd|plain_text_passwd)\s*:\s*).+$`)

// vmsRedactCloudInitDoc hides password hashes and similar secrets in generated cloud-init YAML.
func vmsRedactCloudInitDoc(text string) string {
	return vmsCloudInitSecretRe.ReplaceAllString(text, "${1}<redacted>")
}

var vmsDigitsRe = regexp.MustCompile(`\d+|\D+`)

// vmsCompareNumeric is localeCompare(x, y, "en", {numeric: true}): digit runs compare by value.
func vmsCompareNumeric(x, y string) int {
	a, b := vmsDigitsRe.FindAllString(x, -1), vmsDigitsRe.FindAllString(y, -1)
	for i := 0; i < len(a) && i < len(b); i++ {
		da, db := a[i][0] >= '0' && a[i][0] <= '9', b[i][0] >= '0' && b[i][0] <= '9'
		if da && db {
			na, nb := js.Num(a[i]), js.Num(b[i])
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
			continue
		}
		if c := js.LocaleCompare(a[i], b[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return js.LocaleCompare(x, y)
}

// ---------------------------------------------------------------- tools

func registerVms(c *Context) {
	client := c.Client
	vm := func(ctx context.Context, a *Obj) (proxmox.GuestRef, error) { return c.guest(ctx, a, "qemu") }
	nextID := func(ctx context.Context) (float64, error) {
		v, err := client.Get(ctx, "/cluster/nextid", nil)
		if err != nil {
			return 0, err
		}
		return js.Num(v), nil
	}
	ref := func(g proxmox.GuestRef) *Obj { return proxmox.C("vmid", g.VMID, "name", g.Name, "node", g.Node) }
	refWith := func(g proxmox.GuestRef, kv ...any) *Obj { return js.Spread(ref(g), js.O(kv...)) }
	pending := func(ctx context.Context, base string) []*Obj {
		v, err := client.Get(ctx, base+"/pending", nil)
		if err != nil {
			return []*Obj{}
		}
		return js.Objs(v)
	}

	// ---------------------------------------------------------------- read

	c.Define("proxmox_list_vms", Def{
		Toolset:     "vms",
		List:        &listing{},
		Title:       "List VMs with live details",
		Description: "List QEMU VMs straight from the nodes (GET /nodes/{node}/qemu?full=1), with details proxmox_list_guests lacks: disk I/O counters, host memory, PSI pressure stalls and the running QEMU version/machine. Without `node`, queries every online node. For a quick cluster-wide list with filters by tag/pool use proxmox_list_guests instead.",
		Input: []Prop{
			P("node", Str().Opt().D("Only this node (default: all online nodes)")),
			P("status", Enum("running", "stopped").Opt().D("Only VMs in this state")),
			P("search", searchArg),
			P("limit", limitArg(500)),
			P("raw", rawArg),
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
			}
			errs := []any{}
			vms := []*Obj{}
			for _, node := range nodes {
				list, err := client.Get(ctx, "/nodes/"+js.EncodeURIComponent(node)+"/qemu", js.O("full", 1))
				if err != nil {
					errs = append(errs, js.O("node", node, "error", errMessage(err)))
					continue
				}
				for _, v := range js.Objs(list) {
					vms = append(vms, js.Spread(v, js.O("node", node)))
				}
			}
			if a.Truthy("status") {
				vms = filterObjs(vms, func(v *Obj) bool { return v.Get("status") == a.Get("status") })
			}
			sortObjs(vms, func(x, y *Obj) float64 { return x.Num("vmid") - y.Num("vmid") })
			res := filterAndLimit(vms, a.Get("search"), or(a.Get("limit"), 500), func(v *Obj) []any {
				return []any{v.Get("vmid"), v.Get("name"), v.Get("tags"), v.Get("node")}
			})
			var items any
			if a.Truthy("raw") {
				items = proxmox.RedactSecrets(js.Array(res.Items))
			} else {
				items = mapObjs(res.Items, func(v *Obj) any {
					return proxmox.Compact(js.Spread(js.O("vmid", v.Num("vmid"), "name", v.Get("name"), "node", v.Get("node")), vmsSummarizeStatus(v)))
				})
			}
			return proxmox.Compact(js.Spread(res.Obj(nil), js.O("items", items, "errors", errs))), nil
		},
	})

	c.Define("proxmox_get_vm", Def{
		Toolset:     "vms",
		Title:       "Get VM details",
		Description: "Get a VM's live status (run state, CPU, memory, disk/network I/O, uptime, HA, lock, guest agent, QEMU version) together with a summary of its configuration (CPU, memory, disks, NICs, boot order, firmware, cloud-init, tags, notes) and any pending changes that need a restart. Use proxmox_get_vm_config for the raw config.",
		Input:       []Prop{P("vmid", vmidArg), P("node", guestNodeArg), P("raw", rawArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			base := client.GuestPath(g)
			status, err := client.Get(ctx, base+"/status/current", nil)
			if err != nil {
				return nil, err
			}
			config, err := client.Get(ctx, base+"/config", js.O("current", 1))
			if err != nil {
				return nil, err
			}
			pend, err := client.Get(ctx, base+"/pending", nil)
			if err != nil {
				pend = []any{}
			}
			if a.Truthy("raw") {
				return proxmox.RedactSecrets(js.O("vmid", g.VMID, "node", g.Node, "status", status, "config", config, "pending", pend)), nil
			}
			statusObj, _ := status.(*Obj)
			configObj, _ := config.(*Obj)
			changes := vmsPendingChanges(js.Objs(pend))
			return proxmox.Compact(js.Spread(
				js.O("vmid", g.VMID, "node", g.Node),
				vmsSummarizeStatus(statusObj),
				js.O(
					"config", vmsSummarizeConfig(configObj),
					"pending_changes", changes,
					"note", ifThen(len(changes) > 0, "Pending changes apply at the next VM restart (proxmox_vm_power action=reboot) or can be reverted with proxmox_update_vm_config revert=[...]"),
				),
			)), nil
		},
	})

	c.Define("proxmox_get_vm_config", Def{
		Toolset:     "vms",
		Title:       "Get VM configuration",
		Description: "Get a VM's configuration as Proxmox stores it (key → value, secrets redacted). By default pending changes are shown applied; current=true shows the running values instead, pending=true lists every key with its current and pending value, and snapshot reads the config saved in a snapshot. The `digest` can be passed to proxmox_update_vm_config for safe concurrent edits.",
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("current", Bool().Opt().D("Show the current (running) values instead of values with pending changes applied")),
			P("pending", Bool().Opt().D("List each key with its current value, pending value and pending deletion")),
			P("snapshot", Str().Opt().D("Read the configuration stored in this snapshot")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			base := client.GuestPath(g)
			if a.Truthy("pending") {
				entries, err := client.GetList(ctx, base+"/pending", nil)
				if err != nil {
					return nil, err
				}
				return js.O("vmid", g.VMID, "node", g.Node, "entries", mapObjs(entries, func(e *Obj) any {
					key := js.Str(e.Get("key"))
					return proxmox.C(
						"key", e.Get("key"),
						"value", vmsRedactValue(key, e.Get("value")),
						"pending", vmsRedactValue(key, e.Get("pending")),
						"delete", vmsDeleteFlag(e.Get("delete")),
					)
				})), nil
			}
			config, err := client.GetObj(ctx, base+"/config", defined(js.O("current", a.Get("current"), "snapshot", a.Get("snapshot"))))
			if err != nil {
				return nil, err
			}
			keys := config.Keys()
			sort.SliceStable(keys, func(i, j int) bool { return vmsCompareNumeric(keys[i], keys[j]) < 0 })
			sorted := js.NewObj()
			for _, k := range keys {
				sorted.Set(k, config.Get(k))
			}
			return js.O("vmid", g.VMID, "node", g.Node, "config", proxmox.RedactSecrets(sorted)), nil
		},
	})

	c.Define("proxmox_get_vm_stats", Def{
		Toolset:     "vms",
		Title:       "Get VM performance history",
		Description: "Get a VM's performance history from Proxmox's RRD database: min/avg/max of CPU %, memory, network and disk throughput and pressure stalls over the last hour/day/week/month/year, plus a downsampled time series. Use for 'was it busy?', trend and capacity questions.",
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("timeframe", Enum("hour", "day", "week", "month", "year").Opt().D("Time window (default hour)")),
			P("cf", Enum("AVERAGE", "MAX").Opt().D("Consolidation function for each data point (default AVERAGE)")),
			P("points", Int().Min(0).Max(200).Opt().D("Number of points in the returned series (default 12; 0 = summary only)")),
			P("raw", rawArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			timeframe := or(a.Get("timeframe"), "hour")
			cf := or(a.Get("cf"), "AVERAGE")
			data, err := client.Get(ctx, client.GuestPath(g)+"/rrddata", js.O("timeframe", timeframe, "cf", cf))
			if err != nil {
				return nil, err
			}
			out := js.O("vmid", g.VMID, "node", g.Node, "timeframe", timeframe, "cf", cf)
			if a.Truthy("raw") {
				return out.Set("data", data), nil
			}
			return js.Spread(out, vmsSummarizeRrd(js.Objs(data), int(js.Num(or(a.Get("points"), 12))))), nil
		},
	})

	c.Define("proxmox_get_vm_cloudinit", Def{
		Toolset:     "vms",
		Title:       "Get VM cloud-init data",
		Description: "Show a VM's cloud-init settings with current vs pending values (pending ones need proxmox_regenerate_vm_cloudinit or a restart to reach the guest), plus the generated user-data, network-config and meta-data as the guest will see them. Passwords are redacted.",
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("dump", Arr(Enum("user", "network", "meta")).Opt().D("Which generated documents to include (default all three; [] for none)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			base := client.GuestPath(g)
			types := []any{"user", "network", "meta"}
			if a.Defined("dump") {
				types = a.Arr("dump")
			}
			entries, err := client.GetList(ctx, base+"/cloudinit", nil)
			if err != nil {
				return nil, err
			}
			generated := js.NewObj()
			for _, typ := range types {
				text, err := client.Get(ctx, base+"/cloudinit/dump", js.O("type", typ))
				if err != nil {
					generated.Set(js.Str(typ), "Error: "+errMessage(err))
					continue
				}
				generated.Set(js.Str(typ), vmsRedactCloudInitDoc(js.Str(or(text, ""))))
			}
			var settings []*Obj
			for _, e := range entries {
				if e.Get("key") == "digest" {
					continue
				}
				key := js.Str(e.Get("key"))
				var value, pend any
				if key == "sshkeys" {
					value, pend = vmsDescribeSshKeys(e.Get("value")), vmsDescribeSshKeys(e.Get("pending"))
				} else {
					value, pend = vmsRedactValue(key, e.Get("value")), vmsRedactValue(key, e.Get("pending"))
				}
				settings = append(settings, proxmox.C("key", e.Get("key"), "value", value, "pending", pend, "delete", ifThen(e.Truthy("delete"), true)))
			}
			pendingKeys := []any{}
			for _, s := range settings {
				if s.Defined("pending") || s.Truthy("delete") {
					pendingKeys = append(pendingKeys, s.Get("key"))
				}
			}
			if settings == nil {
				settings = []*Obj{}
			}
			return proxmox.C(
				"vmid", g.VMID,
				"node", g.Node,
				"settings", js.Array(settings),
				"pending_keys", pendingKeys,
				"note", ifThen(len(pendingKeys) > 0, "Some values are pending: run proxmox_regenerate_vm_cloudinit (or restart the VM) to rebuild the cloud-init drive"),
				"generated", generated,
			), nil
		},
	})

	c.Define("proxmox_check_vm_migration", Def{
		Toolset:     "vms",
		Title:       "Check VM migration preconditions",
		Description: "Check whether and where a VM can be migrated before calling proxmox_migrate_vm: allowed and blocked target nodes (with unavailable storages or blocking HA rules), local disks that would need with_local_disks/targetstorage, local devices (PCI/USB passthrough) that block migration, and mapped resources.",
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("target", Str().Opt().D("Target node to check specifically")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			r, err := client.GetObj(ctx, client.GuestPath(g)+"/migrate", defined(js.O("target", a.Get("target"))))
			if err != nil {
				return nil, err
			}
			var conntrack any = js.Undef
			if r.Defined("has-dbus-vmstate") {
				conntrack = proxmox.Flag(r.Get("has-dbus-vmstate"))
			}
			return proxmox.Compact(js.Spread(ref(g), js.O(
				"running", proxmox.Flag(r.Get("running")),
				"allowed_nodes", r.Get("allowed_nodes"),
				"not_allowed_nodes", r.Get("not_allowed_nodes"),
				"local_disks", mapObjs(js.Objs(or(r.Get("local_disks"), []any{})), func(d *Obj) any {
					return proxmox.C(
						"volid", d.Get("volid"),
						"size", proxmox.FormatBytes(d.Get("size")),
						"cdrom", orUndef(proxmox.Flag(d.Get("cdrom"))),
						"unused", orUndef(proxmox.Flag(d.Get("is_unused"))),
						"drive", d.Get("drivename"),
					)
				}),
				"local_resources", r.Get("local_resources"),
				"mapped_resources", or(r.Get("mapped-resource-info"), r.Get("mapped-resources")),
				"dependent_ha_resources", r.Get("dependent-ha-resources"),
				"can_migrate_conntrack", conntrack,
			))), nil
		},
	})

	c.Define("proxmox_check_vm_feature", Def{
		Toolset:     "vms",
		Title:       "Check VM feature support",
		Description: "Check whether a VM's storage supports snapshots, linked clones ('clone') or full copies ('copy'), optionally for a given snapshot, and on which nodes. Useful before proxmox_clone_vm or taking snapshots.",
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("feature", Enum("snapshot", "clone", "copy").D("Feature to check")),
			P("snapname", Str().Opt().D("Check for this snapshot")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			r, err := client.GetObj(ctx, client.GuestPath(g)+"/feature", defined(js.O("feature", a.Get("feature"), "snapname", a.Get("snapname"))))
			if err != nil {
				return nil, err
			}
			return proxmox.Compact(js.Spread(ref(g), js.O("feature", a.Get("feature"), "available", or(proxmox.Flag(r.Get("hasFeature")), false), "nodes", r.Get("nodes")))), nil
		},
	})

	// ---------------------------------------------------------------- write

	c.Define("proxmox_vm_power", Def{
		Toolset:     "vms",
		Title:       "Start, stop or reboot a VM",
		Description: "Change a VM's power state. start; shutdown (clean ACPI shutdown, optionally force_stop after timeout); stop (hard power-off, may lose data); reboot (clean, applies pending config); reset (hard reset); suspend (pause in RAM); resume; hibernate (save RAM to disk and stop; resumes on next start). Reports without acting when the VM is already in the requested state.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("action", Enum("start", "stop", "shutdown", "reboot", "reset", "suspend", "resume", "hibernate").D("Power action")),
			P("timeout", Int().Pos().Opt().D("Seconds to wait for start/stop/shutdown/reboot to complete")),
			P("force_stop", Bool().Opt().D("shutdown: hard-stop the VM if it hasn't shut down within the timeout")),
			P("keep_active", Bool().Opt().D("stop/shutdown: don't deactivate storage volumes")),
			P("overrule_shutdown", Bool().Opt().D("stop: abort running shutdown tasks first")),
			P("state_storage", Str().Opt().D("hibernate: storage for the saved RAM state")),
			P("machine", Str().Opt().D("start: override the QEMU machine type for this boot")),
			P("skiplock", vmsSkiplockArg),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			action := a.Str("action")
			for _, rule := range []struct {
				field   string
				actions []string
			}{
				{"timeout", []string{"start", "stop", "shutdown", "reboot"}},
				{"force_stop", []string{"shutdown"}},
				{"keep_active", []string{"stop", "shutdown"}},
				{"overrule_shutdown", []string{"stop"}},
				{"state_storage", []string{"hibernate"}},
				{"machine", []string{"start"}},
				{"skiplock", []string{"start", "stop", "shutdown", "reset", "suspend", "resume", "hibernate"}},
			} {
				if a.Defined(rule.field) && !contains(rule.actions, action) {
					return nil, fmt.Errorf("%s only applies to: %s", rule.field, strings.Join(rule.actions, ", "))
				}
			}
			if g.Template {
				return nil, fmt.Errorf("VM %s is a template and cannot be started; clone it with proxmox_clone_vm", js.NumStr(g.VMID))
			}
			if action == "start" && g.Status == "running" {
				return refWith(g, "action", action, "status", "already running"), nil
			}
			if (action == "stop" || action == "shutdown") && g.Status == "stopped" {
				return refWith(g, "action", action, "status", "already stopped"), nil
			}
			endpoint := action
			if action == "hibernate" {
				endpoint = "suspend"
			}
			upid, err := client.Post(ctx, client.GuestPath(g)+"/status/"+endpoint, defined(js.O(
				"timeout", a.Get("timeout"),
				"forceStop", a.Get("force_stop"),
				"keepActive", a.Get("keep_active"),
				"overrule-shutdown", a.Get("overrule_shutdown"),
				"todisk", ifThen(action == "hibernate", true),
				"statestorage", a.Get("state_storage"),
				"machine", a.Get("machine"),
				"skiplock", a.Get("skiplock"),
			)))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), refWith(g, "action", action))
		},
	})

	createInput := append([]Prop{
		P("node", nodeArg),
		P("vmid", Int().Min(100).Max(999999999).Opt().D("VMID for the new VM (default: next free ID)")),
	}, vmsCommonFields()...)
	createInput = append(createInput,
		P("storage", Str().Opt().D("Default storage for disks, EFI/TPM state and the cloud-init drive when they don't name one, e.g. local-lvm")),
		P("disks", Arr(vmsDiskSpec).Opt().D("Disks, e.g. [{bus: \"scsi\", storage: \"local-lvm\", size_gb: 32, options: {discard: \"on\", ssd: true, iothread: true}}]")),
		P("iso", Str().Opt().D("ISO volume to attach as CD-ROM on ide2, e.g. 'local:iso/debian-12.iso'")),
		P("nets", Arr(vmsNetSpec).Opt().D("Network interfaces net0, net1... (default one virtio NIC on vmbr0; [] for none)")),
		P("efi_storage", Str().Opt().D("Storage for the EFI vars disk (used with bios=ovmf; default `storage` / first disk's storage)")),
		P("efi_pre_enrolled_keys", Bool().Opt().D("Pre-enroll Secure Boot keys in the EFI disk (needed for Windows 11 Secure Boot)")),
		P("tpm_storage", Str().Opt().D("Add a TPM 2.0 state disk on this storage (needed for Windows 11)")),
		P("cloudinit_storage", Str().Opt().D("Add a cloud-init drive on this storage (ide2, or ide0 when an ISO uses ide2)")),
		P("start", Bool().Opt().D("Start the VM after creating it")),
		P("pool", Str().Opt().D("Add the VM to this resource pool")),
		P("extra", extraArg),
		P("wait", waitArg),
	)

	c.Define("proxmox_create_vm", Def{
		Toolset:     "vms",
		Title:       "Create a VM",
		Description: "Create a QEMU VM. Give disks as typed objects (bus, storage, size_gb, options, or import_from to import a cloud image) or raw specs, an ISO to attach as CD-ROM, NICs (default one virtio NIC on vmbr0), UEFI (bios=ovmf adds an EFI disk) and TPM, and cloud-init settings (setting cloudinit_storage adds the cloud-init drive). VMID defaults to the next free one. Anything else can go in `extra` with Proxmox's parameter names (e.g. hostpci0, serial0, args). To copy an existing VM or template use proxmox_clone_vm; to restore a backup use the backup tools.",
		Write:       true,
		Destructive: False,
		Input:       createInput,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			vmid := a.Get("vmid")
			if js.Nullish(vmid) {
				id, err := nextID(ctx)
				if err != nil {
					return nil, err
				}
				vmid = id
			}
			slots := &vmsSlotAllocator{used: map[string]bool{}}
			params := js.NewObj()
			if a.Truthy("iso") {
				key, err := slots.take("ide2")
				if err != nil {
					return nil, err
				}
				params.Set(key, a.Str("iso")+",media=cdrom")
			}
			disks := a.Objs("disks")
			for _, d := range disks {
				bus := js.Str(or(d.Get("bus"), "scsi"))
				var key string
				if d.Defined("index") {
					key, err = slots.take(bus + js.Str(d.Get("index")))
				} else {
					key, err = slots.next(bus)
				}
				if err != nil {
					return nil, err
				}
				spec, err := vmsBuildDisk(d, a.Get("storage"))
				if err != nil {
					return nil, err
				}
				params.Set(key, spec)
			}
			firstStorage := a.Get("storage")
			if js.Nullish(firstStorage) {
				firstStorage = js.Undef
				for _, d := range disks {
					if d.Truthy("storage") {
						firstStorage = d.Get("storage")
						break
					}
				}
			}
			if a.Truthy("cloudinit_storage") {
				key, err := slots.next("ide", 2, 0, 1, 3)
				if err != nil {
					return nil, err
				}
				params.Set(key, a.Str("cloudinit_storage")+":cloudinit")
			}
			if a.Get("bios") == "ovmf" {
				efi := or(a.Get("efi_storage"), firstStorage)
				if !js.Truthy(efi) {
					return nil, errors.New("bios=ovmf needs an EFI disk: set efi_storage (or storage)")
				}
				params.Set("efidisk0", proxmox.ToPropertyString(js.O("_default", js.Str(efi)+":1", "efitype", "4m", "pre-enrolled-keys", a.Get("efi_pre_enrolled_keys")), "_default"))
			} else if a.Truthy("efi_storage") {
				return nil, errors.New("efi_storage only applies with bios=ovmf")
			}
			if a.Truthy("tpm_storage") {
				params.Set("tpmstate0", a.Str("tpm_storage")+":1,version=v2.0")
			}
			nets := []*Obj{js.NewObj()}
			if a.Present("nets") {
				nets = a.Objs("nets")
			}
			for i, n := range nets {
				params.Set(fmt.Sprintf("net%d", i), vmsBuildNet(n))
			}
			var boot []string
			hasBoot := false
			if a.Present("boot_order") {
				boot, hasBoot = a.Strs("boot_order"), true
			} else if len(disks) > 0 || a.Truthy("iso") {
				hasBoot = true
				for _, k := range params.Keys() {
					if vmsDiskRe.MatchString(k) && !strings.HasPrefix(k, "efidisk") && !strings.HasPrefix(k, "tpmstate") && !vmsCloudinitRe.MatchString(js.Str(params.Get(k))) {
						boot = append(boot, k)
					}
				}
				sort.SliceStable(boot, func(i, j int) bool { return vmsBootSort(boot[i], boot[j]) < 0 })
			}
			var bootParam any = js.Undef
			if hasBoot && len(boot) > 0 {
				bootParam = "order=" + strings.Join(boot, ";")
			}
			body := js.Spread(js.O("vmid", vmid), vmsCommonParams(a))
			body.Set("scsihw", or(a.Get("scsihw"), "virtio-scsi-single"))
			body.Set("ostype", or(a.Get("ostype"), "l26"))
			body.Assign(params)
			body.Set("boot", bootParam)
			body.Set("start", a.Get("start"))
			body.Set("pool", a.Get("pool"))
			// These turn a create into a backup restore, which with force overwrites an existing VM; use proxmox_restore_backup.
			body, err = withExtra(body, a.Get("extra"), "archive", "force", "unique", "live-restore")
			if err != nil {
				return nil, err
			}
			if err := checkGuestConfigGates(c, body, "; see proxmox_unlink_vm_disk"); err != nil {
				return nil, err
			}
			upid, err := client.Post(ctx, "/nodes/"+js.EncodeURIComponent(node)+"/qemu", defined(body))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("vmid", vmid, "node", node, "name", a.Get("name")))
		},
	})

	c.Define("proxmox_clone_vm", Def{
		Toolset:     "vms",
		Title:       "Clone a VM or template",
		Description: "Copy a VM or template to a new VM. Templates get a fast linked clone by default (full=true for an independent copy); normal VMs are always copied in full. newid defaults to the next free VMID. target (another node) only works when the source disks are on shared storage. Clone from a snapshot with snapname. Use proxmox_check_vm_feature feature=clone to see if linked clones are possible.",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("vmid", vmidArg.D("Source VM or template ID")),
			P("node", guestNodeArg),
			P("newid", Int().Min(100).Max(999999999).Opt().D("VMID for the clone (default: next free ID)")),
			P("name", Str().Opt().D("Name for the clone")),
			P("full", Bool().Opt().D("Full copy instead of a linked clone (templates only; normal VMs are always full; implied by storage/format)")),
			P("target", Str().Opt().D("Node to create the clone on (source must be on shared storage)")),
			P("storage", Str().Opt().D("Target storage for a full clone")),
			P("format", Enum("raw", "qcow2", "vmdk").Opt().D("Disk format for a full clone")),
			P("pool", Str().Opt().D("Add the clone to this pool")),
			P("description", Str().Opt().D("Notes for the clone")),
			P("snapname", Str().Opt().D("Clone from this snapshot")),
			P("bwlimit", Int().Pos().Opt().D("I/O bandwidth limit in KiB/s")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			storageOrFormat := a.Truthy("storage") || a.Truthy("format")
			if storageOrFormat && a.Get("full") == false {
				return nil, errors.New("storage and format only apply to full clones")
			}
			// Proxmox defaults templates to linked clones and then rejects storage/format, so ask for a full clone explicitly.
			full := or(a.Get("full"), ifThen(storageOrFormat, true))
			newid := a.Get("newid")
			if js.Nullish(newid) {
				id, err := nextID(ctx)
				if err != nil {
					return nil, err
				}
				newid = id
			}
			upid, err := client.Post(ctx, client.GuestPath(g)+"/clone", defined(js.O(
				"newid", newid,
				"name", a.Get("name"),
				"full", full,
				"target", a.Get("target"),
				"storage", a.Get("storage"),
				"format", a.Get("format"),
				"pool", a.Get("pool"),
				"description", a.Get("description"),
				"snapname", a.Get("snapname"),
				"bwlimit", a.Get("bwlimit"),
			)))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("source", g.VMID, "newid", newid, "name", a.Get("name"), "node", or(a.Get("target"), g.Node)))
		},
	})

	updateInput := append([]Prop{P("vmid", vmidArg), P("node", guestNodeArg)}, vmsCommonFields()...)
	updateInput = append(updateInput,
		P("vcpus", Int().Pos().Opt().D("Number of hot-plugged vCPUs (≤ sockets×cores)")),
		P("cpulimit", Num().Min(0).Opt().D("CPU usage limit in cores (0 = unlimited)")),
		P("cpuunits", Int().Min(1).Max(262144).Opt().D("CPU scheduler weight")),
		P("delete", deleteKeysArg),
		P("revert", Arr(Str()).Opt().D("Pending changes to revert, by key")),
		P("digest", digestArg),
		P("skiplock", vmsSkiplockArg),
		P("extra", extraArg),
		P("wait", waitArg),
	)

	c.Define("proxmox_update_vm_config", Def{
		Toolset:     "vms",
		Title:       "Update VM configuration",
		Description: "Change a VM's settings; only the given fields change. Common fields are typed; for disks, NICs and devices use `extra` with Proxmox keys, e.g. {scsi1: \"local-lvm:20,ssd=1\"} (adds a new 20 GiB disk), {net1: \"virtio,bridge=vmbr1\"}, {hostpci0: \"0000:01:00.0\"}. On a running VM, changes that can't be hot-plugged (e.g. CPU type, memory without hotplug) become pending until the next reboot; the result lists them. `delete` removes keys (deleting a disk key only detaches it to unusedN, but deleting an unusedN key destroys that volume and needs delete permission; proxmox_unlink_vm_disk is the explicit way to destroy disks). `revert` drops pending changes. Use proxmox_resize_vm_disk to grow disks.",
		Write:       true,
		Destructive: True,
		Input:       updateInput,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if extra := a.ObjAt("extra"); extra != nil && extra.Has("force") {
				return nil, errors.New("`force` (physically deleting disks) is not allowed here; use proxmox_unlink_vm_disk")
			}
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			base := js.Spread(vmsCommonParams(a), js.O(
				"vcpus", a.Get("vcpus"),
				"cpulimit", a.Get("cpulimit"),
				"cpuunits", a.Get("cpuunits"),
				"delete", deleteParam(a.Get("delete")),
				"revert", deleteParam(a.Get("revert")),
				"digest", a.Get("digest"),
				"skiplock", a.Get("skiplock"),
			))
			params, err := definedExtra(base, a.Get("extra"))
			if err != nil {
				return nil, err
			}
			updated := []any{}
			for _, k := range params.Keys() {
				if k != "digest" && k != "skiplock" {
					updated = append(updated, k)
				}
			}
			if len(updated) == 0 {
				return nil, errors.New("Nothing to change: give at least one setting, delete or revert")
			}
			if err := checkGuestConfigGates(c, params, "; see proxmox_unlink_vm_disk"); err != nil {
				return nil, err
			}
			upid, err := client.Post(ctx, client.GuestPath(g)+"/config", params)
			if err != nil {
				return nil, err
			}
			result, err := runTask(ctx, client, upid, a.Get("wait"), refWith(g, "updated", updated))
			if err != nil {
				return nil, err
			}
			if result.Truthy("upid") {
				return result, nil
			}
			changes := vmsPendingChanges(pending(ctx, client.GuestPath(g)))
			return proxmox.Compact(js.Spread(result, js.O(
				"pending_changes", changes,
				"note", ifThen(len(changes) > 0, "Some changes are pending and take effect after a reboot (proxmox_vm_power action=reboot)"),
			))), nil
		},
	})

	c.Define("proxmox_resize_vm_disk", Def{
		Toolset:     "vms",
		Title:       "Grow a VM disk",
		Description: "Grow a VM disk, e.g. size='+10G' to add 10 GiB or '64G' for an absolute size. Disks can't be shrunk. Works on running VMs; the partition and filesystem inside the guest still need extending afterwards.",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("disk", vmsDiskKeyArg),
			P("size", Str().Pattern(`^\+?\d+(\.\d+)?[KMGT]?$`, "a size like +10G or 64G").D("New size ('64G') or increment ('+10G'); units K, M, G, T")),
			P("digest", digestArg),
			P("skiplock", vmsSkiplockArg),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			upid, err := client.Put(ctx, client.GuestPath(g)+"/resize", defined(js.O("disk", a.Get("disk"), "size", a.Get("size"), "digest", a.Get("digest"), "skiplock", a.Get("skiplock"))))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), refWith(g, "disk", a.Get("disk"), "size", a.Get("size")))
		},
	})

	c.Define("proxmox_move_vm_disk", Def{
		Toolset:     "vms",
		Title:       "Move a VM disk",
		Description: "Move a VM disk to another storage (works live on running VMs) or reassign it to another VM (target_vmid, optionally target_disk). After a storage move the old copy is kept as an unused disk unless delete_source=true.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("disk", vmsDiskKeyArg),
			P("storage", Str().Opt().D("Target storage")),
			P("format", Enum("raw", "qcow2", "vmdk").Opt().D("Target format (storage moves only)")),
			P("delete_source", Bool().Opt().D("Delete the original disk after a successful copy (default keep it as unusedN)")),
			P("target_vmid", Int().Min(100).Opt().D("Reassign the disk to this VM instead (same node)")),
			P("target_disk", vmsDiskKeyArg.Opt().D("Disk key on the target VM (default: same key)")),
			P("bwlimit", Int().Pos().Opt().D("I/O bandwidth limit in KiB/s")),
			P("digest", digestArg),
			P("target_digest", Str().Opt().D("Only proceed if the target VM's config digest matches")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if !a.Truthy("storage") && !a.Defined("target_vmid") {
				return nil, errors.New("Give a target storage or a target_vmid")
			}
			if a.Truthy("storage") && a.Defined("target_vmid") {
				return nil, errors.New("Give either storage or target_vmid, not both")
			}
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			upid, err := client.Post(ctx, client.GuestPath(g)+"/move_disk", defined(js.O(
				"disk", a.Get("disk"),
				"storage", a.Get("storage"),
				"format", a.Get("format"),
				"delete", a.Get("delete_source"),
				"target-vmid", a.Get("target_vmid"),
				"target-disk", a.Get("target_disk"),
				"bwlimit", a.Get("bwlimit"),
				"digest", a.Get("digest"),
				"target-digest", a.Get("target_digest"),
			)))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), proxmox.Compact(refWith(g, "disk", a.Get("disk"), "storage", a.Get("storage"), "target_vmid", a.Get("target_vmid"))))
		},
	})

	c.Define("proxmox_unlink_vm_disk", Def{
		Toolset:     "vms",
		Title:       "Detach or delete VM disks",
		Description: "Remove disks from a VM. Without destroy, disks are detached and kept as unusedN entries (data intact, re-attachable via proxmox_update_vm_config). With destroy=true the volumes are permanently deleted. Also deletes unusedN entries' volumes when given with destroy=true.",
		Delete:      true,
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("disks", Arr(Str().Pattern(`^(ide|sata|scsi|virtio|unused|efidisk|tpmstate)\d+$`, "a disk key like scsi1 or unused0")).Min(1).D("Disk keys to remove, e.g. [\"scsi1\", \"unused0\"]")),
			P("destroy", Bool().Opt().D("Permanently delete the disk images (default false: detach to unusedN)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			if _, err := client.Put(ctx, client.GuestPath(g)+"/unlink", defined(js.O("idlist", vmsJoin(a.Get("disks"), ","), "force", a.Get("destroy")))); err != nil {
				return nil, err
			}
			return refWith(g, "disks", a.Get("disks"), "result", pick(a.Truthy("destroy"), "destroyed", "detached (now unused disks)")), nil
		},
	})

	c.Define("proxmox_migrate_vm", Def{
		Toolset:     "vms",
		Title:       "Migrate a VM",
		Description: "Move a VM to another node in the cluster. Running VMs are live-migrated (online defaults to true when running). VMs with local disks need with_local_disks=true (optionally targetstorage). Check first with proxmox_check_vm_migration.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("target", Str().D("Target node")),
			P("online", Bool().Opt().D("Live-migrate a running VM (default true when running)")),
			P("with_local_disks", Bool().Opt().D("Also migrate local disks (live storage migration)")),
			P("targetstorage", Str().Opt().D("Target storage, or a mapping 'src:dst,src2:dst2' (with local disks)")),
			P("migration_type", Enum("secure", "insecure").Opt().D("insecure skips the SSH tunnel (trusted networks only)")),
			P("migration_network", Str().Opt().D("CIDR of the network to migrate over")),
			P("bwlimit", Int().Pos().Opt().D("Bandwidth limit in KiB/s")),
			P("with_conntrack_state", Bool().Opt().D("Also migrate firewall conntrack entries (running VMs)")),
			P("force", Bool().Opt().D("Allow migrating VMs with local devices (root only)")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			if a.Str("target") == g.Node {
				return nil, fmt.Errorf("VM %s is already on %s", js.NumStr(g.VMID), g.Node)
			}
			online := or(a.Get("online"), ifThen(g.Status == "running", true))
			upid, err := client.Post(ctx, client.GuestPath(g)+"/migrate", defined(js.O(
				"target", a.Get("target"),
				"online", online,
				"with-local-disks", a.Get("with_local_disks"),
				"targetstorage", a.Get("targetstorage"),
				"migration_type", a.Get("migration_type"),
				"migration_network", a.Get("migration_network"),
				"bwlimit", a.Get("bwlimit"),
				"with-conntrack-state", a.Get("with_conntrack_state"),
				"force", a.Get("force"),
			)))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), refWith(g, "from", g.Node, "target", a.Get("target"), "online", or(online, false)))
		},
	})

	c.Define("proxmox_convert_vm_to_template", Def{
		Toolset:     "vms",
		Title:       "Convert a VM to a template",
		Description: "Turn a stopped VM into a template (irreversible: templates can't be started, only cloned with proxmox_clone_vm). Its disks become read-only base images.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("disk", vmsDiskKeyArg.Opt().D("Convert only this disk to a base image")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			if g.Template {
				return refWith(g, "status", "already a template"), nil
			}
			if g.Status == "running" {
				return nil, fmt.Errorf("VM %s is running; shut it down first (proxmox_vm_power action=shutdown)", js.NumStr(g.VMID))
			}
			upid, err := client.Post(ctx, client.GuestPath(g)+"/template", defined(js.O("disk", a.Get("disk"))))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), refWith(g, "template", true))
		},
	})

	c.Define("proxmox_delete_vm", Def{
		Toolset:     "vms",
		Title:       "Delete a VM",
		Description: "Permanently destroy a VM and all its disks. Refuses a running VM unless stop=true (hard-stops it first). purge=true also removes it from backup/replication jobs and HA; destroy_unreferenced_disks=true also deletes orphaned disks carrying its VMID.",
		Delete:      true,
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("stop", Bool().Opt().D("Hard-stop the VM first if it is running")),
			P("purge", Bool().Opt().D("Also remove the VMID from backup jobs, replication jobs and HA")),
			P("destroy_unreferenced_disks", Bool().Opt().D("Also destroy disks with this VMID not referenced in the config")),
			P("skiplock", vmsSkiplockArg),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			base := client.GuestPath(g)
			if g.Status != "stopped" && !g.Template {
				if !a.Truthy("stop") {
					return nil, fmt.Errorf("VM %s (%s) is %s; stop it first or pass stop=true", js.NumStr(g.VMID), js.Str(or(g.Name, "unnamed")), js.Str(g.Status))
				}
				upid, err := client.Post(ctx, base+"/status/stop", defined(js.O("skiplock", a.Get("skiplock"))))
				if err != nil {
					return nil, err
				}
				stopped, err := runTask(ctx, client, upid, true, nil)
				if err != nil {
					return nil, err
				}
				if stopped.Get("status") != "ok" {
					return nil, fmt.Errorf("Could not confirm VM %s stopped: %s", js.NumStr(g.VMID), js.Stringify(stopped, ""))
				}
			}
			upid, err := client.Delete(ctx, base, defined(js.O("purge", a.Get("purge"), "destroy-unreferenced-disks", a.Get("destroy_unreferenced_disks"), "skiplock", a.Get("skiplock"))))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), refWith(g, "deleted", true))
		},
	})

	c.Define("proxmox_regenerate_vm_cloudinit", Def{
		Toolset:     "vms",
		Title:       "Regenerate VM cloud-init drive",
		Description: "Rebuild a VM's cloud-init drive from its current settings so pending cloud-init changes (user, keys, IPs, DNS) reach the guest. The guest applies them on its next boot (cloud-init normally runs per instance).",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input:       []Prop{P("vmid", vmidArg), P("node", guestNodeArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			if _, err := client.Put(ctx, client.GuestPath(g)+"/cloudinit", nil); err != nil {
				return nil, err
			}
			return refWith(g, "regenerated", true), nil
		},
	})

	c.Define("proxmox_send_vm_key", Def{
		Toolset:     "vms",
		Title:       "Send keystrokes to a VM",
		Description: "Press keys on a VM's virtual keyboard, as on its console: special keys/combos with `keys` (QEMU names, e.g. 'ctrl-alt-delete', 'ret', 'esc', 'tab', 'f2', 'up', 'alt-f4') and/or type ASCII `text` (US layout, '\\n' presses Enter). Useful for boot menus, installers or a hung login prompt. Keys are sent one at a time; nothing is read back.",
		Exec:        true,
		Destructive: True,
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("keys", Arr(Str().Pattern(`^[a-z0-9_]+(-[a-z0-9_]+)*$`, "a QEMU key name like ret or ctrl-alt-delete")).Opt().D("Key names sent before `text`")),
			P("text", Str().Max(2000).Opt().D("Text to type (ASCII, US keyboard layout)")),
			P("skiplock", vmsSkiplockArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			keys := a.Strs("keys")
			if a.Truthy("text") {
				typed, err := vmsTextToKeys(a.Str("text"))
				if err != nil {
					return nil, err
				}
				keys = append(keys, typed...)
			}
			if len(keys) == 0 {
				return nil, errors.New("Give keys and/or text")
			}
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			if g.Status != "running" {
				return nil, fmt.Errorf("VM %s is %s", js.NumStr(g.VMID), js.Str(or(g.Status, "not running")))
			}
			for _, key := range keys {
				if _, err := client.Put(ctx, client.GuestPath(g)+"/sendkey", defined(js.O("key", key, "skiplock", a.Get("skiplock")))); err != nil {
					return nil, err
				}
			}
			return refWith(g, "sent", len(keys), "keys", ifThen(len(keys) <= 50, keys)), nil
		},
	})

	c.Define("proxmox_vm_monitor_command", Def{
		Toolset:     "vms",
		Title:       "Run a QEMU monitor command",
		Description: "Run a QEMU human monitor (HMP) command on a running VM and return its output, e.g. 'info status', 'info block', 'info network', 'info cpus', 'info balloon', 'info migrate'. Non-info commands (e.g. 'balloon 2048', 'device_del') change the live VM; prefer the dedicated tools where they exist.",
		Exec:        true,
		Destructive: True,
		Input:       []Prop{P("vmid", vmidArg), P("node", guestNodeArg), P("command", Str().Min(1).D("Monitor command, e.g. 'info block'"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := vm(ctx, a)
			if err != nil {
				return nil, err
			}
			output, err := client.Post(ctx, client.GuestPath(g)+"/monitor", js.O("command", a.Get("command")))
			if err != nil {
				return nil, err
			}
			if s, ok := output.(string); ok {
				output = strings.TrimRightFunc(s, unicode.IsSpace)
			}
			return refWith(g, "command", a.Get("command"), "output", output), nil
		},
	})
}
