package tools

// Shared handling for list tools. Every proxmox_list_* tool that returns one list returns
// {total, returned, items} (plus any context such as the node) and accepts the same narrowing
// arguments, so a model can ask for exactly what it needs:
//
//   - search: case-insensitive substring match (tools with their own search keep it)
//   - limit:  maximum number of items (only on lists that can grow long)
//   - fields: keep only these fields in each item (dot paths for nested ones)
//
// To make a tool a list tool, set Def.List. The tool returns its items as an array, or as an
// object with the array under listing.Items; the listing adds the missing arguments and shapes
// the result. Tools that already return {total, returned, items} from filterAndLimit only gain
// fields.

import (
	"context"
	"fmt"
	"strings"

	"github.com/mattoddie/proxmox-mcp/internal/js"
)

// listing describes how a list tool narrows its results.
type listing struct {
	// Items is the key of the result object that holds the list, for tools that return an
	// object with context (such as the node). Empty: the tool returns the array itself, or
	// already returns items.
	Items string
	// Limit is the default for the limit argument when the tool doesn't declare one itself.
	// Zero means no limit argument: use it for short configuration lists.
	Limit int
}

// fieldsProp and listSearchProp carry no description, because they appear on every list tool and
// tools/list would grow by kilobytes. The server instructions (listHelp) explain them once.
var (
	fieldsProp     = P("fields", Arr(Str()).Opt())
	listSearchProp = P("search", Str().Opt())
)

// ListArgDocs describes the list arguments for docs/tools.md, which has room for the
// descriptions tools/list leaves out.
var ListArgDocs = map[string]string{
	"search": "Case-insensitive substring match on the items' fields.",
	"fields": "Keep only these fields in each item. Dot paths (`parent.child`) reach nested fields.",
}

// listHelp is the server instructions' explanation of the list arguments.
const listHelp = "List tools return {total, returned, items} and take `search` (case-insensitive substring match), `fields` (keep only these fields in each item; dot paths reach nested fields) and, on long lists, `limit`. Use them to fetch only what you need. "

func hasProp(props []Prop, name string) bool {
	for _, p := range props {
		if p.Name == name {
			return true
		}
	}
	return false
}

// wrap adds the list arguments the tool doesn't declare and shapes its results.
func (l *listing) wrap(props []Prop, handler Handler) ([]Prop, Handler) {
	ownSearch, ownLimit := hasProp(props, "search"), hasProp(props, "limit")
	props = append([]Prop{}, props...)
	if !ownSearch {
		props = append(props, listSearchProp)
	}
	limit := 0
	if !ownLimit && l.Limit > 0 {
		props = append(props, P("limit", limitArg(l.Limit)))
		limit = l.Limit
	}
	props = append(props, fieldsProp)
	return props, func(ctx context.Context, a *Obj) (any, error) {
		out, err := handler(ctx, a)
		if err != nil {
			return out, err
		}
		search := ""
		if !ownSearch {
			search = strings.ToLower(a.Str("search"))
		}
		n := 0
		if limit > 0 {
			n = int(js.Num(or(a.Get("limit"), limit)))
		}
		return l.shape(out, search, n, a.Arr("fields")), nil
	}
}

// shape turns a tool's result into {total, returned, items}, applying search (when the tool has
// no search of its own), the limit and fields. Results it doesn't recognise pass through.
func (l *listing) shape(out any, search string, limit int, fields []any) any {
	if arr, ok := out.([]any); ok {
		return l.narrow(js.O(), "", arr, search, limit, fields)
	}
	o, ok := out.(*Obj)
	if !ok || o == nil {
		return out
	}
	key := l.Items
	if key == "" {
		key = "items"
	}
	arr, ok := o.Get(key).([]any)
	if !ok {
		return out
	}
	if key == "items" && o.Has("returned") {
		// Already narrowed by the tool (filterAndLimit): only fields is left.
		if projected, hint := selectFields(arr, fields); len(fields) > 0 {
			o.Set("items", projected)
			if hint != "" {
				o.Set("hint", hint)
			}
		}
		return o
	}
	return l.narrow(o, key, arr, search, limit, fields)
}

// narrow builds the result: base's other keys (the context), then total, returned and items in
// place of key. A `count` the tool reported is replaced by total.
func (l *listing) narrow(base *Obj, key string, arr []any, search string, limit int, fields []any) *Obj {
	items := arr
	if search != "" {
		items = []any{}
		for _, it := range arr {
			if matchesSearch(it, search) {
				items = append(items, it)
			}
		}
	}
	total := len(items)
	if limit > 0 && limit < len(items) {
		items = items[:limit]
	}
	if items == nil {
		items = []any{}
	}
	res := js.NewObj()
	var hint string
	if len(fields) > 0 {
		items, hint = selectFields(items, fields)
	}
	set := func() { res.Set("total", total).Set("returned", len(items)).Set("items", items) }
	placed := false
	for _, k := range base.Keys() {
		switch k {
		case key:
			set()
			placed = true
		case "count", "total", "returned", "items":
		default:
			res.Set(k, base.Get(k))
		}
	}
	if !placed {
		set()
	}
	if hint != "" {
		res.Set("hint", hint)
	}
	return res
}

// matchesSearch reports whether any top-level value of an item (or a scalar item itself), or any
// scalar in an array value, contains q (already lower-case).
func matchesSearch(item any, q string) bool {
	o, ok := item.(*Obj)
	if !ok || o == nil {
		return containsFold(item, q)
	}
	for _, k := range o.Keys() {
		if containsFold(o.Get(k), q) {
			return true
		}
	}
	return false
}

// containsFold reports whether a scalar, or any scalar in an array, contains q.
func containsFold(v any, q string) bool {
	if list, ok := v.([]any); ok {
		for _, e := range list {
			if containsFold(e, q) {
				return true
			}
		}
		return false
	}
	switch v.(type) {
	case string, float64, int, bool:
		return strings.Contains(strings.ToLower(js.Str(v)), q)
	}
	return false
}

// selectFields keeps only the requested fields in each item. When no item has any of them, the
// hint names the fields the first item does have.
func selectFields(items []any, fields []any) ([]any, string) {
	var paths [][]string
	var names []string
	for _, f := range fields {
		s, _ := f.(string)
		s = strings.Trim(strings.TrimSpace(s), ".")
		if s == "" {
			continue
		}
		paths = append(paths, strings.Split(s, "."))
		names = append(names, s)
	}
	if len(paths) == 0 {
		return items, ""
	}
	out := make([]any, len(items))
	found := false
	for i, it := range items {
		v, ok := pickPaths(it, paths)
		if !ok {
			v = js.NewObj()
		}
		found = found || ok
		out[i] = v
	}
	if found || len(items) == 0 {
		return out, ""
	}
	var avail []string
	if o, ok := items[0].(*Obj); ok && o != nil {
		avail = o.Keys()
	}
	return out, fmt.Sprintf("None of the requested fields (%s) exist. Available fields: %s.", strings.Join(names, ", "), strings.Join(avail, ", "))
}

// pickPaths copies the paths that exist in v, keeping their nesting. Arrays along a path are
// walked element by element, so disks.size keeps each disk's size.
func pickPaths(v any, paths [][]string) (any, bool) {
	if list, ok := v.([]any); ok {
		out := []any{}
		for _, e := range list {
			if p, ok := pickPaths(e, paths); ok {
				out = append(out, p)
			}
		}
		return out, len(out) > 0
	}
	o, ok := v.(*Obj)
	if !ok || o == nil {
		return js.Undef, false
	}
	out := js.NewObj()
	var heads []string
	tails := map[string][][]string{}
	for _, p := range paths {
		if _, seen := tails[p[0]]; !seen {
			heads = append(heads, p[0])
		}
		tails[p[0]] = append(tails[p[0]], p[1:])
	}
	for _, h := range heads {
		if !o.Defined(h) {
			continue
		}
		val := o.Get(h)
		whole := false
		var sub [][]string
		for _, t := range tails[h] {
			if len(t) == 0 {
				whole = true
			} else {
				sub = append(sub, t)
			}
		}
		if whole {
			out.Set(h, val)
		} else if p, ok := pickPaths(val, sub); ok {
			out.Set(h, p)
		}
	}
	return out, out.Len() > 0
}
