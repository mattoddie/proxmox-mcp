package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

// ---------------------------------------------------------------- common arguments

var (
	nodeArg = Str().Opt().D("Node name. Defaults to PROXMOX_DEFAULT_NODE, or the only node on a single-node install. See proxmox_list_nodes.")

	nodeRequiredArg = Str().D("Node name (see proxmox_list_nodes)")

	// guestNodeArg is for tools that act on a guest: the node is looked up from the VMID when omitted.
	guestNodeArg = Str().Opt().D("Node the guest is on. Looked up automatically from the VMID when omitted.")

	vmidArg = Int().Min(100).Max(999999999).D("VM or container ID (VMID), e.g. 100")

	waitArg = Bool().Opt().D("Wait for the Proxmox task to finish and return its result and log (default true, up to PROXMOX_TASK_TIMEOUT_MS). With false the task ID (UPID) is returned immediately; follow it with proxmox_get_task.")

	searchArg = Str().Opt().D("Case-insensitive substring filter on names, IDs, tags and similar fields")

	rawArg = Bool().Opt().D("Return the full unprocessed objects from the API (larger)")

	// extraArg holds free-form extra parameters passed to the API call, for options the typed
	// arguments don't cover. Names are Proxmox's own (see the API viewer).
	extraArg = Rec(Union(Str(), Num(), Bool())).Opt().D("Additional raw Proxmox API parameters (advanced; names as in the Proxmox API viewer) for options the typed arguments don't cover. Parameters the tool sets from its typed arguments can't be repeated here.")

	// deleteKeysArg lists config keys to remove, sent as Proxmox's `delete` parameter.
	deleteKeysArg = Arr(Str()).Opt().D("Config keys to remove/reset to default, e.g. [\"net1\", \"description\"] (sent as the API's `delete` parameter)")

	digestArg = Str().Opt().D("Only apply the change if the current config digest matches (optimistic locking; from the matching get tool)")
)

func limitArg(def int) *Schema {
	return Int().Pos().Max(10000).Opt().D(fmt.Sprintf("Maximum number of results (default %d)", def))
}

// ---------------------------------------------------------------- parameters

// withExtra merges `extra` into request parameters built from typed arguments. Keys the tool
// sets itself (present in params, even with an undefined value because the argument was
// omitted) and the reserved keys are refused, so extra can't override a value the tool
// validated or gated on (vmid, force, delete...). Pass params before defined() so omitted typed
// arguments still count.
func withExtra(params *Obj, extra any, reserved ...string) (*Obj, error) {
	x, isObj := extra.(*Obj)
	if !isObj || x == nil {
		return params, nil
	}
	var clash []string
	for _, k := range x.Keys() {
		if params.Has(k) || contains(reserved, k) {
			clash = append(clash, "`"+k+"`")
		}
	}
	if len(clash) > 0 {
		return nil, fmt.Errorf("extra must not set %s: use the tool's typed arguments (extra is only for parameters the tool doesn't handle itself)", strings.Join(clash, ", "))
	}
	return js.Spread(params, x), nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// defined copies only the keys whose value is not undefined (lets "" and false through, unlike Compact).
func defined(o *Obj) *Obj {
	out := js.NewObj()
	for _, k := range o.Keys() {
		if v := o.Get(k); !js.IsUndef(v) {
			out.Set(k, v)
		}
	}
	return out
}

// definedExtra is defined(withExtra(params, extra, reserved...)).
func definedExtra(params *Obj, extra any, reserved ...string) (*Obj, error) {
	p, err := withExtra(params, extra, reserved...)
	if err != nil {
		return nil, err
	}
	return defined(p), nil
}

// deleteParam joins config keys for Proxmox's comma-separated `delete` parameter (undefined when empty).
func deleteParam(keys any) any {
	arr, ok := keys.([]any)
	if !ok || len(arr) == 0 {
		return js.Undef
	}
	return js.Str(arr)
}

// enc is encodeURIComponent.
func enc(v any) string { return js.EncodeURIComponent(js.Str(v)) }

// ---------------------------------------------------------------- results

// FL is the result of filterAndLimit.
type FL struct {
	Total, Returned int
	Items           []*Obj
}

// Obj renders the result with the given items (the filtered items when nil).
func (r FL) Obj(items any) *Obj {
	if items == nil {
		items = js.Array(r.Items)
	}
	return js.O("total", r.Total, "returned", r.Returned, "items", items)
}

// Map renders the result with each item transformed.
func (r FL) Map(f func(*Obj) any) *Obj {
	out := make([]any, len(r.Items))
	for i, it := range r.Items {
		out[i] = f(it)
	}
	return r.Obj(out)
}

// filterAndLimit applies a case-insensitive substring search over some fields, then a limit
// (limit is a number, or undefined for none).
func filterAndLimit(items []*Obj, search any, limit any, fields func(*Obj) []any) FL {
	out := items
	if s, ok := search.(string); ok && s != "" {
		q := strings.ToLower(s)
		out = nil
		for _, it := range items {
			for _, f := range fields(it) {
				if !js.Nullish(f) && strings.Contains(strings.ToLower(js.Str(f)), q) {
					out = append(out, it)
					break
				}
			}
		}
	}
	total := len(out)
	if !js.IsUndef(limit) && limit != nil {
		n := int(js.Num(limit))
		if n < 0 {
			n = max(len(out)+n, 0)
		}
		if n < len(out) {
			out = out[:n]
		}
	}
	if out == nil {
		out = []*Obj{}
	}
	return FL{Total: total, Returned: len(out), Items: out}
}

// or returns v, or fallback when v is undefined or null (v ?? fallback).
func or(v, fallback any) any {
	if js.Nullish(v) {
		return fallback
	}
	return v
}

// orUndef returns v when it is truthy, else undefined (v || undefined).
func orUndef(v any) any {
	if js.Truthy(v) {
		return v
	}
	return js.Undef
}

// ifThen returns v when cond holds, else undefined (cond ? v : undefined).
func ifThen(cond bool, v any) any {
	if cond {
		return v
	}
	return js.Undef
}

// pick returns a when cond holds, else b.
func pick(cond bool, a, b any) any {
	if cond {
		return a
	}
	return b
}

// mapObjs applies f to each object.
func mapObjs(items []*Obj, f func(*Obj) any) []any {
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = f(it)
	}
	return out
}

// filterObjs keeps the objects for which f holds.
func filterObjs(items []*Obj, f func(*Obj) bool) []*Obj {
	out := []*Obj{}
	for _, it := range items {
		if f(it) {
			out = append(out, it)
		}
	}
	return out
}

// sortObjs sorts stably with a JavaScript-style comparator (negative: a first).
func sortObjs(items []*Obj, cmp func(a, b *Obj) float64) {
	sort.SliceStable(items, func(i, j int) bool { return cmp(items[i], items[j]) < 0 })
}

// compareStr is a.localeCompare(b) as a comparator result.
func compareStr(a, b any) float64 { return float64(js.LocaleCompare(js.Str(a), js.Str(b))) }

// ---------------------------------------------------------------- guests and nodes

// node resolves the `node` argument (PROXMOX_DEFAULT_NODE, or the only node, when omitted).
func (c *Context) node(ctx context.Context, a *Obj) (string, error) {
	return c.Client.ResolveNode(ctx, a.Str("node"))
}

// guest resolves the guest named by the `vmid` (and optional `node`) arguments; typ is "qemu",
// "lxc" or "" for either.
func (c *Context) guest(ctx context.Context, a *Obj, typ string) (proxmox.GuestRef, error) {
	return c.Client.ResolveGuest(ctx, a.Num("vmid"), a.Str("node"), typ)
}

// ---------------------------------------------------------------- tasks

// runTask handles an API result that may be a task ID (UPID). By default it waits for the
// task, failing with the log tail if it fails; with wait=false it returns the UPID straight
// away. Non-UPID results are passed through as `result`.
func runTask(ctx context.Context, client *proxmox.Client, result any, wait any, extra *Obj) (*Obj, error) {
	if extra == nil {
		extra = js.NewObj()
	}
	if !proxmox.IsUpid(result) {
		return proxmox.Compact(js.Spread(extra, js.O("result", result))), nil
	}
	if wait == false {
		return js.Spread(extra, js.O("upid", result, "status", "started", "note", "Follow progress with proxmox_get_task")), nil
	}
	task, err := client.WaitForTask(ctx, result.(string), proxmox.WaitOptions{})
	if err != nil {
		return nil, err
	}
	if task.Truthy("timedOut") {
		return js.Spread(extra, js.O(
			"status", "running",
			"note", fmt.Sprintf("Still running after %ss; check with proxmox_get_task", js.NumStr(js.Round(float64(client.TaskTimeoutMs())/1000))),
			"task", proxmox.Compact(task),
		)), nil
	}
	if !task.Truthy("ok") {
		lines := task.Arr("log")
		if len(lines) > 15 {
			lines = lines[len(lines)-15:]
		}
		tail := ""
		if len(lines) > 0 {
			parts := make([]string, len(lines))
			for i, l := range lines {
				parts[i] = js.Str(l)
			}
			if t := strings.Join(parts, "\n"); t != "" {
				tail = "\n--- task log (tail) ---\n" + t
			}
		}
		return nil, fmt.Errorf("Task %s %s failed: %s%s\n(UPID %s)", js.Str(or(task.Get("type"), "")), js.Str(or(task.Get("id"), "")), js.Str(or(task.Get("exitstatus"), "unknown error")), tail, js.Str(task.Get("upid")))
	}
	return js.Spread(extra, js.O("status", "ok", "task", proxmox.Compact(task))), nil
}
