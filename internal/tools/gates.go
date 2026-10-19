package tools

import (
	"errors"
	"regexp"
	"strings"

	"github.com/mattoddie/proxmox-mcp/internal/js"
)

// Shared checks for request parameters that need more than the tool's own access level. The
// typed tools and the raw API tool both use them, so the two can't drift apart.

var (
	// configKeySepRe splits a Proxmox key list such as `delete`. Proxmox splits on commas,
	// semicolons, whitespace (including \v) and NUL; splitting on anything that can't be part of
	// a key covers all of those and whatever else a parser might accept.
	configKeySepRe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
	unusedKeyRe    = regexp.MustCompile(`(?i)^unused\d+$`)
)

// configKeys returns the keys listed in a `delete`-style parameter: a string, or an array of
// them (the raw tool sends arrays as repeated keys).
func configKeys(v any) []string {
	var out []string
	values, ok := v.([]any)
	if !ok {
		values = []any{v}
	}
	for _, x := range values {
		if js.Nullish(x) {
			continue
		}
		for _, k := range configKeySepRe.Split(js.Str(x), -1) {
			if k != "" {
				out = append(out, k)
			}
		}
	}
	return out
}

// deletesUnusedVolume reports whether a `delete` parameter removes an unusedN key, which
// destroys that volume rather than just detaching it.
func deletesUnusedVolume(v any) bool {
	for _, k := range configKeys(v) {
		if unusedKeyRe.MatchString(k) {
			return true
		}
	}
	return false
}

// checkGuestConfigGates guards guest config parameters that reach beyond a plain config write:
// deleting an unusedN key destroys that volume, and a hookscript runs as root on the node at
// every lifecycle event. hint is appended to the delete error (e.g. the tool that does it
// explicitly).
func checkGuestConfigGates(c *Context, params *Obj, hint string) error {
	if !c.AllowDeletes && deletesUnusedVolume(params.Get("delete")) {
		return errors.New("Deleting an unusedN entry permanently destroys its volume and needs delete permission (PROXMOX_ALLOW_DELETES)" + hint)
	}
	if !c.AllowExec && params.Defined("hookscript") {
		return errors.New("hookscript runs a script as root on the node, so setting it needs exec permission (PROXMOX_ALLOW_EXEC)")
	}
	return nil
}

// paramTruthy reports whether a raw API parameter would be read by Proxmox as a true boolean
// (Proxmox accepts 1/0, true/false, yes/no and on/off). Anything else non-empty counts as true,
// so an unusual spelling can't slip past a check.
func paramTruthy(v any) bool {
	switch t := v.(type) {
	case nil, js.Undefined:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case []any:
		for _, x := range t {
			if paramTruthy(x) {
				return true
			}
		}
		return false
	}
	switch strings.ToLower(strings.TrimSpace(js.Str(v))) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}
