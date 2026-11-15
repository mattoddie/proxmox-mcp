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

// tagPattern is Proxmox's pve-tag format.
const tagPattern = `^[a-zA-Z0-9_][a-zA-Z0-9_+.-]*$`

var (
	tagArg      = Str().Pattern(tagPattern, "a tag: letters, digits, _ + - . (not starting with + - .)")
	tagListArg  = Arr(tagArg)
	tagColorRe  = regexp.MustCompile(`^[0-9a-fA-F]{6}(:[0-9a-fA-F]{6})?$`)
	tagOptsKeys = []string{"tag-style", "user-tag-access", "registered-tags"}
)

// tagsKey is how tags compare: case-insensitively, as Proxmox does unless tag-style sets case-sensitive.
func tagsKey(tag string, caseSensitive bool) string {
	if caseSensitive {
		return tag
	}
	return strings.ToLower(tag)
}

// tagsApply returns the tags after removing and adding, keeping the existing order and appending new ones.
func tagsApply(current []string, add, remove []string, caseSensitive bool) []string {
	drop := map[string]bool{}
	for _, t := range remove {
		drop[tagsKey(t, caseSensitive)] = true
	}
	out := []string{}
	have := map[string]bool{}
	for _, t := range append(append([]string{}, current...), add...) {
		k := tagsKey(t, caseSensitive)
		if have[k] || (drop[k] && !contains(add, t)) {
			continue
		}
		have[k] = true
		out = append(out, t)
	}
	return out
}

// tagsObj reads a property-string option that the API may return as a string or as an object.
func tagsObj(v any) *Obj {
	if o, ok := v.(*Obj); ok && o != nil {
		return js.Spread(o)
	}
	return proxmox.ParsePropertyString(v, "")
}

// plannedChanges is what a tool that makes several changes returns in a dry run instead of
// sending them, so the preview lists every change rather than only the first.
type plannedChanges struct{ plan *Obj }

func isDryRun(ctx context.Context) bool {
	call := proxmox.CallFrom(ctx)
	return call != nil && call.DryRun()
}

func defineTagTools(c *Context) {
	client := c.Client

	c.Define("proxmox_bulk_tag_guests", Def{
		Toolset:     "cluster",
		Title:       "Add or remove tags on many guests",
		Description: "Add and/or remove tags on many VMs and containers at once. Select guests by `vmids`, by `with_tag` (every guest that has that tag) or both (guests matching either). Other tags are kept, in their order; new ones are appended. Tags compare case-insensitively unless the datacenter's tag-style sets case-sensitive. Rename a tag with with_tag=old, add=[new], remove=[old]. Only guests whose tags change are updated; a failure on one guest doesn't stop the others and is reported. dry_run=true lists every change without making it.",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input: []Prop{
			P("vmids", Arr(Int().Min(100)).Opt().D("Guest IDs to change")),
			P("with_tag", tagArg.Opt().D("Also change every guest that has this tag")),
			P("add", tagListArg.Opt().D("Tags to add, e.g. [\"prod\", \"web\"]")),
			P("remove", tagListArg.Opt().D("Tags to remove")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			vmids := a.Get("vmids")
			if (!a.Present("vmids") || len(vmids.([]any)) == 0) && !a.Truthy("with_tag") {
				return nil, errors.New("Select guests with vmids and/or with_tag")
			}
			add, remove := a.Strs("add"), a.Strs("remove")
			if len(add) == 0 && len(remove) == 0 {
				return nil, errors.New("Give tags to add and/or remove")
			}
			opts, err := client.GetObj(ctx, "/cluster/options", nil)
			if err != nil {
				return nil, err
			}
			caseSensitive := paramTruthy(tagsObj(opts.Get("tag-style")).Get("case-sensitive"))
			guests, err := client.GetList(ctx, "/cluster/resources", js.O("type", "vm"))
			if err != nil {
				return nil, err
			}
			wanted := map[float64]bool{}
			if arr, ok := vmids.([]any); ok {
				for _, v := range arr {
					wanted[js.Num(v)] = true
				}
			}
			found := map[float64]bool{}
			var selected []*Obj
			withTag := tagsKey(a.Str("with_tag"), caseSensitive)
			for _, g := range guests {
				id := g.Num("vmid")
				match := wanted[id]
				if !match && a.Truthy("with_tag") {
					for _, t := range splitTags(js.Str(or(g.Get("tags"), ""))) {
						if tagsKey(js.Str(t), caseSensitive) == withTag {
							match = true
						}
					}
				}
				if match && !found[id] {
					found[id] = true
					selected = append(selected, g)
				}
			}
			var missing []string
			for id := range wanted {
				if !found[id] {
					missing = append(missing, js.NumStr(id))
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				return nil, fmt.Errorf("No guest with VMID %s (see proxmox_list_guests); nothing was changed", strings.Join(missing, ", "))
			}
			sort.SliceStable(selected, func(i, j int) bool { return selected[i].Num("vmid") < selected[j].Num("vmid") })
			dry := isDryRun(ctx)
			var results []any
			changed, unchanged, failed := 0, 0, 0
			for _, g := range selected {
				var before []string
				for _, t := range splitTags(js.Str(or(g.Get("tags"), ""))) {
					before = append(before, js.Str(t))
				}
				after := tagsApply(before, add, remove, caseSensitive)
				entry := proxmox.C("vmid", g.Get("vmid"), "name", g.Get("name"), "node", g.Get("node"), "type", g.Get("type"), "before", strArr(before), "after", strArr(after))
				if strings.Join(before, ";") == strings.Join(after, ";") {
					unchanged++
					results = append(results, entry.Set("status", "unchanged"))
					continue
				}
				if dry {
					changed++
					results = append(results, entry.Set("status", "would change"))
					continue
				}
				ref := proxmox.GuestRef{VMID: g.Num("vmid"), Node: js.Str(g.Get("node")), Type: js.Str(g.Get("type"))}
				params := js.O("tags", strings.Join(after, ";"))
				if len(after) == 0 {
					params = js.O("delete", "tags")
				}
				if _, err := client.Put(ctx, client.GuestPath(ref)+"/config", params); err != nil {
					failed++
					results = append(results, entry.Set("status", "failed").Set("error", errMessage(err)))
					continue
				}
				changed++
				results = append(results, entry.Set("status", "changed"))
			}
			if results == nil {
				results = []any{}
			}
			out := js.O("changed", changed, "unchanged", unchanged, "failed", failed, "guests", results)
			if dry {
				return plannedChanges{out}, nil
			}
			return out, nil
		},
	})

	c.Define("proxmox_set_tag_options", Def{
		Toolset:     "cluster",
		Title:       "Set datacenter tag options",
		Description: "Change the datacenter's tag settings. tag-style: how the web UI shows tags (shape, ordering, colours per tag) and whether tags compare case-sensitively. user-tag-access: which tags users without Sys.Modify on / may set or remove on guests they manage (none, a list, existing ones or any). registered-tags: tags that only users with Sys.Modify on / may set or remove. Only the given fields change; within color_map only the named tags change (\"\" removes a tag's colour). Use reset to remove a whole setting. Needs Sys.Modify on /. See current values with proxmox_get_cluster_options.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("shape", Enum("full", "circle", "dense", "none").Opt().D("Tag shape in the web UI tree (default circle)")),
			P("ordering", Enum("config", "alphabetical").Opt().D("Sort tags alphabetically (default) or keep their config order")),
			P("case_sensitive", Bool().Opt().D("Compare tags case-sensitively when removing duplicates (default false)")),
			P("color_map", Rec(Str()).Opt().D("Tag colours as tag → background hex or background:text hex, e.g. {\"prod\": \"ff0000\", \"dev\": \"00aa00:ffffff\"}; \"\" removes a tag's colour")),
			P("user_allow", Enum("none", "list", "existing", "free").Opt().D("Tags users without Sys.Modify on / may use: none, those in user_allow_list (list), those plus tags already on their guests (existing), or any (free, the default)")),
			P("user_allow_list", tagListArg.Opt().D("Tags users may use with user_allow list or existing (replaces the list; [] clears it)")),
			P("registered_tags", tagListArg.Opt().D("Tags only users with Sys.Modify on / may set or remove (replaces the list; [] clears it)")),
			P("reset", Arr(Enum(tagOptsKeys...)).Opt().D("Remove these settings entirely, going back to Proxmox's defaults")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			opts, err := client.GetObj(ctx, "/cluster/options", nil)
			if err != nil {
				return nil, err
			}
			reset := a.Strs("reset")
			params := js.NewObj()
			var del []string
			touched := func(keys ...string) bool {
				for _, k := range keys {
					if a.Present(k) {
						return true
					}
				}
				return false
			}
			check := func(key string, fields ...string) error {
				if contains(reset, key) && touched(fields...) {
					return fmt.Errorf("reset %s can't be combined with its fields (%s)", key, strings.Join(fields, ", "))
				}
				return nil
			}
			if err := check("tag-style", "shape", "ordering", "case_sensitive", "color_map"); err != nil {
				return nil, err
			}
			if err := check("user-tag-access", "user_allow", "user_allow_list"); err != nil {
				return nil, err
			}
			if err := check("registered-tags", "registered_tags"); err != nil {
				return nil, err
			}
			if touched("shape", "ordering", "case_sensitive", "color_map") {
				style := tagsObj(opts.Get("tag-style"))
				for arg, key := range map[string]string{"shape": "shape", "ordering": "ordering", "case_sensitive": "case-sensitive"} {
					if a.Present(arg) {
						style.Set(key, a.Get(arg))
					}
				}
				if cm := a.ObjAt("color_map"); cm != nil {
					colors := js.NewObj()
					for _, entry := range strings.Split(js.Str(or(style.Get("color-map"), "")), ";") {
						if tag, color, ok := strings.Cut(entry, ":"); ok && tag != "" {
							colors.Set(tag, color)
						}
					}
					for _, tag := range cm.Keys() {
						color := js.Str(cm.Get(tag))
						if !regexp.MustCompile(tagPattern).MatchString(tag) {
							return nil, fmt.Errorf("color_map: %s is not a valid tag", js.Quote(tag))
						}
						if color == "" {
							colors.Delete(tag)
							continue
						}
						if !tagColorRe.MatchString(color) {
							return nil, fmt.Errorf("color_map: colour for %s must be a hex colour like ff0000 or ff0000:ffffff", tag)
						}
						colors.Set(tag, color)
					}
					var parts []string
					for _, tag := range colors.Keys() {
						parts = append(parts, tag+":"+js.Str(colors.Get(tag)))
					}
					style.Set("color-map", ifThen(len(parts) > 0, strings.Join(parts, ";")))
				}
				if s := proxmox.ToPropertyString(style, ""); s != "" {
					params.Set("tag-style", s)
				} else {
					del = append(del, "tag-style")
				}
			}
			if touched("user_allow", "user_allow_list") {
				access := tagsObj(opts.Get("user-tag-access"))
				if a.Present("user_allow") {
					access.Set("user-allow", a.Get("user_allow"))
				}
				if a.Present("user_allow_list") {
					list := a.Strs("user_allow_list")
					access.Set("user-allow-list", ifThen(len(list) > 0, strings.Join(list, ";")))
				}
				if s := proxmox.ToPropertyString(access, ""); s != "" {
					params.Set("user-tag-access", s)
				} else {
					del = append(del, "user-tag-access")
				}
			}
			if a.Present("registered_tags") {
				if list := a.Strs("registered_tags"); len(list) > 0 {
					params.Set("registered-tags", strings.Join(list, ";"))
				} else {
					del = append(del, "registered-tags")
				}
			}
			del = append(del, reset...)
			if params.Len() == 0 && len(del) == 0 {
				return nil, errors.New("Nothing to change: give at least one tag setting or reset")
			}
			if len(del) > 0 {
				params.Set("delete", strings.Join(del, ","))
			}
			if _, err := client.Put(ctx, "/cluster/options", params); err != nil {
				return nil, err
			}
			after, err := client.GetObj(ctx, "/cluster/options", nil)
			if err != nil {
				return nil, err
			}
			out := js.O("updated", true)
			for _, k := range tagOptsKeys {
				out.Set(k, after.Get(k))
			}
			return proxmox.Compact(out), nil
		},
	})
}

func strArr(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}
