package tools

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

var (
	// rawExecPathRe matches API paths that run commands, open consoles, send input or read/write
	// files in guests or on nodes; gated by PROXMOX_ALLOW_EXEC. Checked for every method, since
	// agent file-read and exec-status are GETs. This covers every guest agent endpoint, consoles
	// and shells (termproxy, vncshell, spiceshell, vncproxy, spiceproxy, vncwebsocket), migration
	// tunnels, the QEMU monitor, sendkey (keystrokes into a console) and the node's bulk `execute`.
	rawExecPathRe = regexp.MustCompile(`(?i)/agent\b|/(termproxy|vncshell|spiceshell|vncproxy|spiceproxy|vncwebsocket|mtunnel|mtunnelwebsocket|monitor|sendkey)\b|/nodes/[^/]+/execute\b`)

	// rawControlCharRe matches ASCII control characters: URL parsing may strip tab, CR and LF,
	// so they could hide a blocked path.
	rawControlCharRe = regexp.MustCompile(`[\x00-\x1f\x7f]`)

	rawQueryRe      = regexp.MustCompile(`[?#\\]`)
	rawDotSegmentRe = regexp.MustCompile(`/\.(/|$)`)
	rawSlashesRe    = regexp.MustCompile(`/{2,}`)
	rawAcmeRe       = regexp.MustCompile(`(?i)^/cluster/acme/plugins\b`)
)

var errRawControl = errors.New("path must not contain control characters (tab, newline, NUL...)")

// rawCheckPath fails unless p (a decoded path relative to /api2/json) is a plain absolute path
// the exec check can trust.
func rawCheckPath(p string) error {
	if rawControlCharRe.MatchString(p) {
		return errRawControl
	}
	if !strings.HasPrefix(p, "/") || strings.Contains(p, "..") || rawQueryRe.MatchString(p) || rawDotSegmentRe.MatchString(p) {
		return errors.New("path must be absolute, must not contain '..' or './' segments and must not include a query string (use params)")
	}
	return nil
}

// decodeURIComponent decodes percent-escapes, failing on malformed ones and on escapes that
// don't form valid UTF-8, like JavaScript's decodeURIComponent.
func decodeURIComponent(s string) (string, error) {
	out, err := url.PathUnescape(s)
	if err != nil || !utf8.ValidString(out) {
		return "", errors.New("path is not validly percent-encoded")
	}
	return out, nil
}

// rawSentPath is the path the client puts on the wire for a request to path, decoded.
func rawSentPath(client *proxmox.Client, path string) (string, error) {
	u, err := url.Parse(client.APIURL(path))
	if err != nil {
		return "", errors.New("path is not validly percent-encoded")
	}
	return decodeURIComponent(u.EscapedPath())
}

func registerRaw(c *Context) {
	client := c.Client
	desc := "Call any Proxmox VE API endpoint directly, for anything the other tools don't cover. `path` is relative to /api2/json, e.g. /nodes/pve1/qemu/100/firewall/options. Parameters go in the query string for GET/DELETE and in the form body for POST/PUT. "
	switch {
	case !c.AllowWrites:
		desc += "Only GET is allowed because write access is disabled (PROXMOX_ALLOW_WRITES=false)."
	case c.AllowDeletes:
		desc += "All HTTP methods are allowed."
	default:
		desc += "DELETE is not allowed because delete access is disabled (PROXMOX_ALLOW_DELETES=false)."
	}
	if !c.AllowExec {
		desc += " Command execution, console and guest file endpoints (all guest agent endpoints, shells and consoles, monitor, sendkey) are blocked (PROXMOX_ALLOW_EXEC=false)."
	}
	// Appended, so the sentences above stay as they were.
	if c.AllowWrites && !c.AllowDeletes {
		desc += " Requests that destroy data without DELETE are refused too: wiping or initialising disks, restoring over a guest (force), deleting unusedN volumes, unlink with force, move_disk/move_volume with delete, purge or destroy-unreferenced-disks, and remote_migrate."
	}

	c.Define("proxmox_api_request", Def{
		Toolset:     "raw",
		Title:       "Raw Proxmox API request",
		Description: desc,
		ReadOnly:    boolPtr(!c.AllowWrites),
		Destructive: boolPtr(c.AllowWrites),
		Input: []Prop{
			P("method", Enum("GET", "POST", "PUT", "DELETE").Opt().D("HTTP method (default GET)")),
			P("path", Str().D("Path relative to /api2/json, starting with /")),
			P("params", Rec(Union(Str(), Num(), Bool(), Arr(Union(Str(), Num())))).Opt().D("API parameters (booleans are sent as 1/0, arrays as repeated keys)")),
			P("redact", Bool().Opt().D("Redact password/secret/key fields in the response (default true)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			method := js.Str(or(a.Get("method"), "GET"))
			if method != "GET" && !c.AllowWrites {
				return nil, errors.New("Write access is disabled. Set PROXMOX_ALLOW_WRITES=true to allow non-GET requests.")
			}
			if method == "DELETE" && !c.AllowDeletes {
				return nil, errors.New("Delete access is disabled. Set PROXMOX_ALLOW_DELETES=true to allow DELETE requests.")
			}
			// Check the decoded path: %2e%2e decodes to "..", and a client that normalises paths
			// would drop "./" segments, treat "\" as "/" or strip tabs and newlines, which could
			// otherwise sneak past the checks below.
			raw := a.Str("path")
			if rawControlCharRe.MatchString(raw) {
				return nil, errRawControl
			}
			path := raw
			if strings.HasPrefix(path, "/api2/json/") {
				path = strings.TrimPrefix(path, "/api2/json")
			}
			decoded, err := decodeURIComponent(path)
			if err != nil {
				return nil, err
			}
			if err := rawCheckPath(decoded); err != nil {
				return nil, err
			}
			// Then check what will actually be sent, decoded again.
			base, err := rawSentPath(client, "/")
			if err != nil {
				return nil, err
			}
			sent, err := rawSentPath(client, path)
			if err != nil {
				return nil, err
			}
			if !strings.HasPrefix(sent, base) {
				return nil, errors.New("path must stay under /api2/json")
			}
			sent = sent[len(base)-1:]
			if err := rawCheckPath(sent); err != nil {
				return nil, err
			}
			blocked := false
			for _, p := range []string{decoded, sent} {
				if rawExecPathRe.MatchString(rawSlashesRe.ReplaceAllString(p, "/")) {
					blocked = true
				}
			}
			if blocked && !c.AllowExec {
				return nil, errors.New("This endpoint runs commands, opens a console, sends input or reads/writes files in a guest or on a node. Set PROXMOX_ALLOW_EXEC=true to allow it.")
			}
			params, _ := a.Get("params").(*Obj)
			if err := rawGateError(c, method, []string{decoded, sent}, params); err != nil {
				return nil, err
			}
			data, err := client.Request(ctx, method, path, params)
			if err != nil {
				return nil, err
			}
			if a.Get("redact") == false {
				return data, nil
			}
			// ACME plugin `data` is a KEY=value blob of DNS API credentials, which the field-name check can't see.
			return proxmox.RedactSecrets(data, proxmox.RedactOptions{AcmePluginData: rawAcmeRe.MatchString(sent)}), nil
		},
	})
}

// rawRule is an endpoint (or parameter) the raw tool must gate beyond its HTTP method, because
// the curated tools treat it as a delete, a write or exec.
type rawRule struct {
	path *regexp.Regexp
	// nonGET restricts the rule to POST, PUT and DELETE.
	nonGET bool
	// when, if set, decides from the parameters whether the rule applies.
	when   func(p *Obj) bool
	gate   Gate
	reason string
}

func rawParamTrue(key string) func(*Obj) bool {
	return func(p *Obj) bool { return paramTruthy(p.Get(key)) }
}

func rawParamSet(keys ...string) func(*Obj) bool {
	return func(p *Obj) bool {
		for _, k := range keys {
			if p.Present(k) {
				return true
			}
		}
		return false
	}
}

// rawRules: matched case-insensitively against the decoded path and the path that is sent,
// with repeated slashes collapsed.
var rawRules = []rawRule{
	// ---- delete-equivalent: these destroy data without using DELETE.
	{path: regexp.MustCompile(`(?i)^/nodes/[^/]+/disks/(wipedisk|initgpt)/?$`), gate: Delete,
		reason: "wipes a disk or replaces its partition table, destroying its data"},
	{path: regexp.MustCompile(`(?i)^/nodes/[^/]+/(qemu|lxc)/?$`), nonGET: true, when: rawParamTrue("force"), gate: Delete,
		reason: "restores over an existing guest (force), destroying its disks and config"},
	{path: regexp.MustCompile(`(?i)^/nodes/[^/]+/(qemu|lxc)(/|$)`), nonGET: true, when: func(p *Obj) bool { return deletesUnusedVolume(p.Get("delete")) }, gate: Delete,
		reason: "deletes an unusedN entry, which destroys its volume"},
	{path: regexp.MustCompile(`(?i)^/nodes/[^/]+/qemu/[^/]+/unlink/?$`), when: rawParamTrue("force"), gate: Delete,
		reason: "unlinks disks with force, which destroys the volumes"},
	{path: regexp.MustCompile(`(?i)^/nodes/[^/]+/(qemu/[^/]+/move_disk|lxc/[^/]+/move_volume)/?$`), when: rawParamTrue("delete"), gate: Delete,
		reason: "deletes the source volume after moving it"},
	// Remote migration copies the guest's disks and config to another cluster of the caller's
	// choosing (with delete=1 it also removes the original). That is data leaving the cluster
	// and possibly a destroyed guest, not command execution, so it sits with deletes.
	{path: regexp.MustCompile(`(?i)^/nodes/[^/]+/(qemu|lxc)/[^/]+/remote_migrate/?$`), gate: Delete,
		reason: "migrates the guest to another cluster (copying its data out, and with delete=1 removing the original)"},
	{path: regexp.MustCompile(`^/`), nonGET: true, when: func(p *Obj) bool {
		return paramTruthy(p.Get("purge")) || paramTruthy(p.Get("destroy-unreferenced-disks"))
	}, gate: Delete,
		reason: "purges configuration or destroys unreferenced disks"},
}

// rawGateError checks every rule that applies to a request and fails on the first one whose
// setting is off.
func rawGateError(c *Context, method string, paths []string, params *Obj) error {
	for _, r := range rawRules {
		if (r.nonGET && method == "GET") || (r.when != nil && !r.when(params)) {
			continue
		}
		matched := false
		for _, p := range paths {
			if r.path.MatchString(rawSlashesRe.ReplaceAllString(p, "/")) {
				matched = true
			}
		}
		if !matched {
			continue
		}
		switch {
		case r.gate == Write && !c.AllowWrites:
			return fmt.Errorf("This request %s, so it needs write access. Set PROXMOX_ALLOW_WRITES=true to allow it.", r.reason)
		case r.gate == Delete && !c.AllowDeletes:
			return fmt.Errorf("This request %s, so it needs delete access. Set PROXMOX_ALLOW_DELETES=true to allow it.", r.reason)
		case r.gate == Exec && !c.AllowExec:
			return fmt.Errorf("This request %s, so it needs exec access. Set PROXMOX_ALLOW_EXEC=true to allow it.", r.reason)
		}
	}
	return nil
}
