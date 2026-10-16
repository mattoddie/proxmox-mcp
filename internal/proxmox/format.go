package proxmox

import (
	"encoding/base64"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/mattoddie/proxmox-mcp/internal/js"
)

// Helpers that turn raw Proxmox API objects into compact, model-friendly summaries: human-readable
// sizes and times, parsed property strings and redacted secrets. Functions that may have no
// answer return js.Undef, so their results can go straight into js.O.

// Compact drops undefined, null, "" and empty arrays/objects so summaries stay small.
func Compact(o *js.Obj) *js.Obj {
	out := js.NewObj()
	for _, k := range o.Keys() {
		v := o.Get(k)
		switch x := v.(type) {
		case nil, js.Undefined:
			continue
		case string:
			if x == "" {
				continue
			}
		case []any:
			if len(x) == 0 {
				continue
			}
		case *js.Obj:
			if x.Len() == 0 {
				continue
			}
		}
		out.Set(k, v)
	}
	return out
}

// C is Compact(js.O(kv...)).
func C(kv ...any) *js.Obj { return Compact(js.O(kv...)) }

// FormatBytes turns 1536 into "1.50 KiB" (undefined for non-numbers).
func FormatBytes(bytes any) any {
	if js.Nullish(bytes) || bytes == "" {
		return js.Undef
	}
	n := js.Num(bytes)
	if !js.IsFinite(n) {
		return js.Undef
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	v := math.Abs(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	var s string
	switch {
	case i == 0:
		s = js.NumStr(v)
	case v >= 100:
		s = js.ToFixed(v, 0)
	case v >= 10:
		s = js.ToFixed(v, 1)
	default:
		s = js.ToFixed(v, 2)
	}
	s = trailingZeros.ReplaceAllString(s, "")
	sign := ""
	if n < 0 {
		sign = "-"
	}
	return sign + s + " " + units[i]
}

var trailingZeros = regexp.MustCompile(`\.0+$`)

// FormatUptime turns 90061 into "1d 1h 1m".
func FormatUptime(seconds any) any {
	n := math.Floor(js.Num(seconds))
	if !js.IsFinite(n) || n <= 0 {
		return js.Undef
	}
	d := math.Floor(n / 86400)
	h := math.Floor(math.Mod(n, 86400) / 3600)
	m := math.Floor(math.Mod(n, 3600) / 60)
	switch {
	case d != 0:
		return js.NumStr(d) + "d " + js.NumStr(h) + "h " + js.NumStr(m) + "m"
	case h != 0:
		return js.NumStr(h) + "h " + js.NumStr(m) + "m"
	case m != 0:
		return js.NumStr(m) + "m"
	}
	return js.NumStr(n) + "s"
}

// Percent is used/total as a percentage with one decimal, e.g. 42.3.
func Percent(used, total any) any {
	u, t := js.Num(used), js.Num(total)
	if !js.IsFinite(u) || !js.IsFinite(t) || t <= 0 {
		return js.Undef
	}
	return js.Round(u/t*1000) / 10
}

// FractionPct is a 0..1 fraction (as Proxmox reports CPU) as a percentage with one decimal.
func FractionPct(value any) any {
	if js.Nullish(value) {
		return js.Undef
	}
	n := js.Num(value)
	if !js.IsFinite(n) {
		return js.Undef
	}
	return js.Round(n*1000) / 10
}

// ISO formats epoch milliseconds like Date.prototype.toISOString.
func ISO(ms float64) string {
	t := time.UnixMilli(int64(ms)).UTC()
	if t.Year() < 0 || t.Year() > 9999 {
		return t.Format("+002006-01-02T15:04:05.000Z")
	}
	return t.Format("2006-01-02T15:04:05.000Z")
}

// IsoTime turns epoch seconds into ISO 8601.
func IsoTime(epochSeconds any) any {
	n := js.Num(epochSeconds)
	if !js.IsFinite(n) || n <= 0 || n*1000 > 8.64e15 {
		return js.Undef
	}
	return ISO(n * 1000)
}

// Usage is "used of total (pct%)" for memory/disk, or undefined when unknown.
func Usage(used, total any) any {
	t := js.Num(total)
	if !js.IsFinite(t) || t <= 0 {
		return js.Undef
	}
	u := FormatBytes(used)
	if js.Nullish(u) {
		u = "?"
	}
	p := Percent(used, total)
	if js.Nullish(p) {
		p = 0.0
	}
	return js.Str(u) + " of " + js.Str(FormatBytes(total)) + " (" + js.Str(p) + "%)"
}

// ParsePropertyString parses "local-lvm:vm-100-disk-0,size=32G,ssd=1" into
// {defaultKey: "local-lvm:vm-100-disk-0", size: "32G", ssd: "1"}.
func ParsePropertyString(value any, defaultKey string) *js.Obj {
	if defaultKey == "" {
		defaultKey = "_default"
	}
	out := js.NewObj()
	s, ok := value.(string)
	if !ok {
		return out
	}
	for _, part := range strings.Split(s, ",") {
		if part == "" {
			continue
		}
		if idx := strings.Index(part, "="); idx < 0 {
			out.Set(defaultKey, part)
		} else {
			out.Set(part[:idx], part[idx+1:])
		}
	}
	return out
}

// ToPropertyString is the inverse of ParsePropertyString; undefined and null values are skipped.
func ToPropertyString(o *js.Obj, defaultKey string) string {
	if defaultKey == "" {
		defaultKey = "_default"
	}
	var parts []string
	if o.Defined(defaultKey) {
		parts = append(parts, js.Str(o.Get(defaultKey)))
	}
	for _, k := range o.Keys() {
		v := o.Get(k)
		if k == defaultKey || js.Nullish(v) {
			continue
		}
		if b, ok := v.(bool); ok {
			if b {
				v = 1.0
			} else {
				v = 0.0
			}
		}
		parts = append(parts, k+"="+js.Str(v))
	}
	return strings.Join(parts, ",")
}

var sizeRe = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)\s*([KMGTP]?)i?B?$`)

// ParseSize turns size strings like "32G", "512M" or "1T" into bytes.
func ParseSize(size any) any {
	switch x := size.(type) {
	case float64:
		return x
	case string:
		m := sizeRe.FindStringSubmatch(strings.TrimFunc(x, isSpace))
		if m == nil {
			return js.Undef
		}
		pow := strings.Index("KMGTP", strings.ToUpper(m[2])) + 1
		if m[2] == "" {
			pow = 0
		}
		return js.Round(js.Num(m[1]) * math.Pow(1024, float64(pow)))
	}
	return js.Undef
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' || r == 0xa0 || r == 0xfeff
}

// DiskKeyRe matches config keys that hold disks/volumes on VMs (scsi0, virtio1, efidisk0...)
// and containers (rootfs, mp0).
var DiskKeyRe = regexp.MustCompile(`^(ide|sata|scsi|virtio|efidisk|tpmstate|unused)\d+$|^(rootfs|mp\d+)$`)

// NetKeyRe matches network interface keys on VMs and containers.
var NetKeyRe = regexp.MustCompile(`^net\d+$`)

// IsDiskKey reports whether a config key holds a disk or volume.
func IsDiskKey(key string) bool { return DiskKeyRe.MatchString(key) }

// SummarizeDisks returns disk-ish config entries as parsed objects, e.g.
// {scsi0: {file: "local-lvm:vm-100-disk-0", size: "32G"}}.
func SummarizeDisks(config *js.Obj) *js.Obj {
	out := js.NewObj()
	for _, k := range config.Keys() {
		v, ok := config.Get(k).(string)
		if !IsDiskKey(k) || !ok {
			continue
		}
		def := "file"
		if k == "rootfs" || strings.HasPrefix(k, "mp") {
			def = "volume"
		}
		out.Set(k, ParsePropertyString(v, def))
	}
	return out
}

// SummarizeNets returns network config entries as parsed objects (model/MAC, bridge, VLAN tag,
// firewall, IPs for containers).
func SummarizeNets(config *js.Obj) *js.Obj {
	out := js.NewObj()
	for _, k := range config.Keys() {
		v, ok := config.Get(k).(string)
		if !NetKeyRe.MatchString(k) || !ok {
			continue
		}
		parsed := ParsePropertyString(v, "")
		// VM NICs start with "virtio=AA:BB:..." (model=mac); split it out.
		for _, model := range []string{"virtio", "e1000", "e1000e", "rtl8139", "vmxnet3"} {
			if parsed.Truthy(model) {
				parsed.Set("model", model)
				parsed.Set("macaddr", parsed.Get(model))
				parsed.Delete(model)
			}
		}
		out.Set(k, parsed)
	}
	return out
}

var (
	secretKeyRe     = regexp.MustCompile(`(?i)pass(word|wd|phrase)?$|secret|private|^token$|apikey|api[-_]?key|^subscription-?key$|^client-key$|^encryption-key$|^keyring$|cipassword|smtp-password|hmac|^ticket$|^otel-headers$`)
	urlCredentialRe = regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://[^\s/:@]+:)[^\s/@]+@`)
	base64Re        = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)
	whitespaceRe    = regexp.MustCompile(`\s+`)
	binaryRe        = regexp.MustCompile(`[\x00-\x08\x0e-\x1f\x{FFFD}]`)
	lineSplitRe     = regexp.MustCompile(`\r?\n`)
)

// Redacted replaces secret values.
const Redacted = "<redacted>"

// IsSecretKey reports whether a field name looks like it holds a secret.
func IsSecretKey(k string) bool { return secretKeyRe.MatchString(k) }

// RedactPluginData keeps only the keys of an ACME plugin's `data`, which holds DNS API
// credentials as KEY=value lines and which the API returns base64-encoded. (Splitting the base64
// text itself at "=" would leak everything but the padding.) Undefined for empty data.
func RedactPluginData(data any) any {
	s, ok := data.(string)
	if !ok || s == "" {
		return js.Undef
	}
	text := s
	compactB64 := whitespaceRe.ReplaceAllString(s, "")
	if base64Re.MatchString(compactB64) && len(compactB64)%4 == 0 {
		decoded := decodeBase64Lenient(compactB64)
		if strings.Contains(decoded, "=") && !binaryRe.MatchString(decoded) {
			text = decoded
		} else {
			return []any{Redacted}
		}
	}
	var out []any
	for _, l := range lineSplitRe.Split(text, -1) {
		if strings.TrimFunc(l, isSpace) == "" {
			continue
		}
		if idx := strings.Index(l, "="); idx >= 0 {
			out = append(out, l[:idx]+"="+Redacted)
		} else {
			out = append(out, Redacted)
		}
	}
	if out == nil {
		out = []any{}
	}
	return out
}

// decodeBase64Lenient decodes like Buffer.from(s, "base64").toString("utf8"): invalid UTF-8
// becomes U+FFFD.
func decodeBase64Lenient(s string) string {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		b, _ = base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
	}
	return strings.ToValidUTF8(string(b), "�")
}

// isAcmePlugin reports whether o is an ACME DNS plugin config ({plugin, type: "dns", api, data}),
// whose `data` holds the DNS API credentials.
func isAcmePlugin(o *js.Obj) bool {
	_, isStr := o.Get("data").(string)
	return isStr && (o.Get("type") == "dns" || (o.Defined("api") && o.Defined("plugin")))
}

// RedactOptions tune RedactSecrets.
type RedactOptions struct {
	// AcmePluginData treats every `data` string as ACME plugin credentials (e.g. for anything
	// under /cluster/acme/plugins).
	AcmePluginData bool
}

// RedactSecrets replaces the values of password/secret/key-like fields with "<redacted>",
// recursively, and user:password@ credentials inside URLs.
func RedactSecrets(value any, opts ...RedactOptions) any {
	var opt RedactOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	return redact(value, opt)
}

func redact(value any, opt RedactOptions) any {
	switch x := value.(type) {
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = redact(v, opt)
		}
		return out
	case *js.Obj:
		if x == nil {
			return x
		}
		out := js.NewObj()
		_, dataStr := x.Get("data").(string)
		acme := isAcmePlugin(x) || (opt.AcmePluginData && dataStr)
		for _, k := range x.Keys() {
			v := x.Get(k)
			secret := secretKeyRe.MatchString(k)
			switch {
			case acme && k == "data" && js.Truthy(v):
				lines := RedactPluginData(v).([]any)
				parts := make([]string, len(lines))
				for i, l := range lines {
					parts[i] = l.(string)
				}
				out.Set(k, strings.Join(parts, "\n"))
			case secret && isRedactableScalar(v):
				out.Set(k, Redacted)
			case secret && isArray(v):
				arr := v.([]any)
				red := make([]any, len(arr))
				for i, e := range arr {
					if _, isObj := e.(*js.Obj); isObj {
						red[i] = redact(e, opt)
					} else if _, isArr := e.([]any); isArr {
						red[i] = redact(e, opt)
					} else {
						red[i] = Redacted
					}
				}
				out.Set(k, red)
			default:
				out.Set(k, redact(v, opt))
			}
		}
		return out
	case string:
		if strings.Contains(x, "@") {
			return urlCredentialRe.ReplaceAllString(x, "${1}"+Redacted+"@")
		}
	}
	return value
}

func isArray(v any) bool {
	_, ok := v.([]any)
	return ok
}

// isRedactableScalar is the "value is set and not an object or boolean" test.
func isRedactableScalar(v any) bool {
	switch x := v.(type) {
	case nil, js.Undefined, bool, *js.Obj, []any:
		return false
	case string:
		return x != ""
	}
	return true
}

// Flag normalises Proxmox booleans, which arrive as 0/1 numbers or strings (undefined when unset).
func Flag(value any) any {
	if js.Nullish(value) || value == "" {
		return js.Undef
	}
	switch x := value.(type) {
	case bool:
		return x
	case float64:
		return x == 1
	case string:
		return x == "1" || x == "on" || x == "yes"
	}
	return false
}
