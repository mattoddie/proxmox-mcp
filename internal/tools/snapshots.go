package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

var snapshotsSnapnameArg = Str().Min(1).D("Snapshot name (see proxmox_list_snapshots)")

const snapshotsDay = 86400

func snapshotsKind(g proxmox.GuestRef) string {
	if g.Type == "qemu" {
		return "vm"
	}
	return "container"
}

func snapshotsNow() float64 { return float64(time.Now().UnixMilli()) / 1000 }

func snapshotsAgeDays(snaptime any, now float64) any {
	t := js.Num(snaptime)
	if !js.IsFinite(t) || t <= 0 {
		return js.Undef
	}
	return math.Floor((now-t)/snapshotsDay*10) / 10
}

// snapshotsCurrentParent is the snapshot the guest currently runs from: the parent of the
// pseudo-entry "current".
func snapshotsCurrentParent(list []*Obj) any {
	for _, s := range list {
		if s.Get("name") == "current" {
			return s.Get("parent")
		}
	}
	return js.Undef
}

func snapshotsSummarize(s *Obj, current any, now float64) *Obj {
	var description any = js.Undef
	if d, ok := s.Get("description").(string); ok {
		description = strings.TrimSpace(d)
	}
	return proxmox.C(
		"name", s.Get("name"),
		"parent", s.Get("parent"),
		"description", description,
		"time", proxmox.IsoTime(s.Get("snaptime")),
		"age_days", snapshotsAgeDays(s.Get("snaptime"), now),
		"vmstate", orUndef(proxmox.Flag(s.Get("vmstate"))),
		"current", ifThen(s.Get("name") == current, true),
	)
}

func snapshotsSnaptime(s *Obj) float64 { return js.Num(or(s.Get("snaptime"), 0.0)) }

// snapshotsTree renders indented lines showing the snapshot tree, with "(current)" on the
// snapshot the guest is based on.
func snapshotsTree(snaps []*Obj, current any) []any {
	names := map[string]bool{}
	for _, s := range snaps {
		names[js.Str(s.Get("name"))] = true
	}
	children := map[string][]*Obj{}
	for _, s := range snaps {
		p := ""
		if s.Truthy("parent") && names[js.Str(s.Get("parent"))] {
			p = js.Str(s.Get("parent"))
		}
		children[p] = append(children[p], s)
	}
	lines := []any{}
	// visited guards against parent loops in a broken snapshot list.
	visited := map[*Obj]bool{}
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		kids := children[parent]
		sortObjs(kids, func(a, b *Obj) float64 { return snapshotsSnaptime(a) - snapshotsSnaptime(b) })
		for _, s := range kids {
			if visited[s] {
				continue
			}
			visited[s] = true
			line := strings.Repeat("  ", depth) + js.Str(s.Get("name"))
			if iso, ok := proxmox.IsoTime(s.Get("snaptime")).(string); ok {
				line += " [" + strings.Replace(iso[:16], "T", " ", 1) + "]"
			}
			if s.Get("name") == current {
				line += " (current)"
			}
			lines = append(lines, line)
			walk(js.Str(s.Get("name")), depth+1)
		}
	}
	walk("", 0)
	if !js.Truthy(current) && len(snaps) > 0 {
		lines = append(lines, "(guest is not based on any snapshot)")
	}
	return lines
}

func registerSnapshots(c *Context) {
	client := c.Client
	guest := func(ctx context.Context, a *Obj) (proxmox.GuestRef, error) { return c.guest(ctx, a, "") }
	snapPath := func(g proxmox.GuestRef, name any) string { return client.GuestPath(g) + "/snapshot/" + enc(name) }

	// ---------------------------------------------------------------- read

	c.Define("proxmox_list_snapshots", Def{
		Toolset:     "snapshots",
		Title:       "List snapshots of a guest",
		Description: "List the snapshots of a VM or container (the type is detected from the VMID): name, parent, description, creation time, age, whether RAM state was saved (VMs), and which snapshot the guest currently runs from, plus an indented tree view. For stale snapshots across all guests use proxmox_list_all_snapshots.",
		Input:       []Prop{P("vmid", vmidArg), P("node", guestNodeArg), P("raw", rawArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := guest(ctx, a)
			if err != nil {
				return nil, err
			}
			raw, err := client.Get(ctx, client.GuestPath(g)+"/snapshot", nil)
			if err != nil {
				return nil, err
			}
			if a.Truthy("raw") {
				return proxmox.RedactSecrets(raw), nil
			}
			list := js.Objs(raw)
			current := snapshotsCurrentParent(list)
			snaps := filterObjs(list, func(s *Obj) bool { return s.Get("name") != "current" })
			sortObjs(snaps, func(x, y *Obj) float64 { return snapshotsSnaptime(x) - snapshotsSnaptime(y) })
			now := snapshotsNow()
			return proxmox.C(
				"vmid", g.VMID,
				"name", g.Name,
				"type", snapshotsKind(g),
				"node", g.Node,
				"count", len(snaps),
				"current_snapshot", current,
				"tree", snapshotsTree(snaps, current),
				"snapshots", mapObjs(snaps, func(s *Obj) any { return snapshotsSummarize(s, current, now) }),
			), nil
		},
	})

	c.Define("proxmox_get_snapshot", Def{
		Toolset:     "snapshots",
		Title:       "Get snapshot details",
		Description: "Get a snapshot of a VM or container: its description, time, parent, whether RAM was saved, and the guest configuration as it was at that snapshot (disks and networks summarized, secrets redacted). Useful to see what a rollback would restore.",
		Input:       []Prop{P("vmid", vmidArg), P("node", guestNodeArg), P("snapname", snapshotsSnapnameArg), P("raw", rawArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := guest(ctx, a)
			if err != nil {
				return nil, err
			}
			v, err := client.Get(ctx, snapPath(g, a.Get("snapname"))+"/config", nil)
			if err != nil {
				return nil, err
			}
			if a.Truthy("raw") {
				return proxmox.RedactSecrets(v), nil
			}
			config, _ := v.(*Obj)
			rest := js.NewObj()
			for _, k := range config.Keys() {
				switch k {
				case "description", "snaptime", "parent", "vmstate", "snapstate":
				default:
					rest.Set(k, config.Get(k))
				}
			}
			var description any = js.Undef
			if d, ok := config.Get("description").(string); ok {
				description = strings.TrimSpace(d)
			}
			return proxmox.C(
				"vmid", g.VMID,
				"type", snapshotsKind(g),
				"node", g.Node,
				"snapname", a.Get("snapname"),
				"description", description,
				"time", proxmox.IsoTime(config.Get("snaptime")),
				"parent", config.Get("parent"),
				"vmstate", ifThen(config.Truthy("vmstate"), true),
				"state", config.Get("snapstate"),
				"disks", proxmox.SummarizeDisks(rest),
				"nets", proxmox.SummarizeNets(rest),
				"config", proxmox.RedactSecrets(rest),
			), nil
		},
	})

	c.Define("proxmox_list_all_snapshots", Def{
		Toolset:     "snapshots",
		Title:       "List snapshots across all guests",
		Description: "List snapshots of every VM and container in the cluster (or a filtered set), oldest first, with guest, node, time, age in days and description. Use older_than_days to find stale snapshots that hold back storage space and slow down disks. Guests whose snapshots can't be read are reported under errors.",
		Input: []Prop{
			P("older_than_days", Num().Min(0).Opt().D("Only snapshots older than this many days")),
			P("node", Str().Opt().D("Only guests on this node")),
			P("type", Enum("vm", "container").Opt().D("Only VMs or only containers")),
			P("vmids", Arr(Int().Min(100)).Opt().D("Only these guests")),
			P("search", searchArg),
			P("limit", limitArg(500)),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			guests, err := client.GetList(ctx, "/cluster/resources", js.O("type", "vm"))
			if err != nil {
				return nil, err
			}
			guests = filterObjs(guests, func(r *Obj) bool { return r.Get("type") == "qemu" || r.Get("type") == "lxc" })
			if a.Truthy("node") {
				guests = filterObjs(guests, func(r *Obj) bool { return r.Get("node") == a.Get("node") })
			}
			if a.Truthy("type") {
				want := pick(a.Get("type") == "vm", "qemu", "lxc")
				guests = filterObjs(guests, func(r *Obj) bool { return r.Get("type") == want })
			}
			if vmids, ok := a.Get("vmids").([]any); ok {
				guests = filterObjs(guests, func(r *Obj) bool {
					for _, v := range vmids {
						if v == r.Num("vmid") {
							return true
						}
					}
					return false
				})
			}
			// Offline nodes can't answer; skip their guests instead of failing every request.
			now := snapshotsNow()
			errs := []any{}
			var snaps []*Obj
			for _, r := range guests {
				list, err := client.GetList(ctx, "/nodes/"+enc(r.Get("node"))+"/"+js.Str(r.Get("type"))+"/"+js.Str(r.Get("vmid"))+"/snapshot", nil)
				if err != nil {
					errs = append(errs, js.O("vmid", r.Num("vmid"), "node", r.Get("node"), "error", errMessage(err)))
					continue
				}
				current := snapshotsCurrentParent(list)
				for _, s := range list {
					if s.Get("name") == "current" {
						continue
					}
					snaps = append(snaps, js.Spread(
						js.O("vmid", r.Num("vmid"), "guest", r.Get("name"), "type", pick(r.Get("type") == "qemu", "vm", "container"), "node", r.Get("node")),
						snapshotsSummarize(s, current, now),
						js.O("snaptime", snapshotsSnaptime(s)),
					))
				}
			}
			if a.Defined("older_than_days") {
				cutoff := now - a.Num("older_than_days")*snapshotsDay
				snaps = filterObjs(snaps, func(s *Obj) bool { return s.Num("snaptime") > 0 && s.Num("snaptime") < cutoff })
			}
			sortObjs(snaps, func(x, y *Obj) float64 {
				if d := x.Num("snaptime") - y.Num("snaptime"); js.Truthy(d) {
					return d
				}
				return x.Num("vmid") - y.Num("vmid")
			})
			res := filterAndLimit(snaps, a.Get("search"), or(a.Get("limit"), 500), func(s *Obj) []any {
				return []any{s.Get("vmid"), s.Get("guest"), s.Get("name"), s.Get("description"), s.Get("node")}
			})
			items := mapObjs(res.Items, func(s *Obj) any {
				out := s.Clone()
				out.Delete("snaptime")
				return out
			})
			return proxmox.C(
				"guests_checked", len(guests),
				"total", res.Total,
				"returned", res.Returned,
				"items", items,
				"errors", errs,
			), nil
		},
	})

	// ---------------------------------------------------------------- write

	c.Define("proxmox_create_snapshot", Def{
		Toolset:     "snapshots",
		Title:       "Create a snapshot",
		Description: "Take a snapshot of a VM or container (type detected from the VMID), e.g. before an upgrade. vmstate=true also saves a running VM's RAM so a rollback resumes exactly where it was (VMs only; takes longer and uses disk space). The guest's storage must support snapshots (e.g. LVM-thin, ZFS, Ceph, qcow2); check containers with proxmox_check_container_feature.",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("snapname", Str().Pattern(`^[A-Za-z][A-Za-z0-9_-]{1,39}$`, "2-40 characters: a letter, then letters, digits, - or _").D("Snapshot name: starts with a letter; letters, digits, - and _ only, e.g. pre_upgrade_2026_10")),
			P("description", Str().Opt().D("Description of the snapshot")),
			P("vmstate", Bool().Opt().D("Include RAM state (running VMs only)")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := guest(ctx, a)
			if err != nil {
				return nil, err
			}
			if a.Truthy("vmstate") && g.Type != "qemu" {
				return nil, errors.New("vmstate is only supported for VMs, not containers")
			}
			if g.Template {
				return nil, fmt.Errorf("Guest %s is a template; templates can't be snapshotted", js.NumStr(g.VMID))
			}
			upid, err := client.Post(ctx, client.GuestPath(g)+"/snapshot", defined(js.O(
				"snapname", a.Get("snapname"), "description", a.Get("description"), "vmstate", ifThen(g.Type == "qemu", a.Get("vmstate")),
			)))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("vmid", g.VMID, "type", snapshotsKind(g), "node", g.Node, "snapname", a.Get("snapname")))
		},
	})

	c.Define("proxmox_rollback_snapshot", Def{
		Toolset:     "snapshots",
		Title:       "Roll back to a snapshot",
		Description: "Roll a VM or container back to a snapshot. Destructive: all changes since the snapshot (disk contents and config) are lost, and a running guest is stopped. start=true starts the guest afterwards; VM snapshots that include RAM resume automatically. Newer snapshots are kept, except that on ZFS storage only the most recent snapshot can be rolled back to (delete newer ones first with proxmox_delete_snapshot).",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("snapname", snapshotsSnapnameArg),
			P("start", Bool().Opt().D("Start the guest after the rollback (default false)")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := guest(ctx, a)
			if err != nil {
				return nil, err
			}
			upid, err := client.Post(ctx, snapPath(g, a.Get("snapname"))+"/rollback", defined(js.O("start", a.Get("start"))))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("vmid", g.VMID, "type", snapshotsKind(g), "node", g.Node, "snapname", a.Get("snapname"), "rolled_back", true))
		},
	})

	c.Define("proxmox_update_snapshot", Def{
		Toolset:     "snapshots",
		Title:       "Update a snapshot description",
		Description: "Change the description of a VM or container snapshot. The snapshot's contents can't be changed.",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input:       []Prop{P("vmid", vmidArg), P("node", guestNodeArg), P("snapname", snapshotsSnapnameArg), P("description", Str().D("New description (empty string clears it)"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := guest(ctx, a)
			if err != nil {
				return nil, err
			}
			if _, err := client.Put(ctx, snapPath(g, a.Get("snapname"))+"/config", js.O("description", a.Get("description"))); err != nil {
				return nil, err
			}
			return js.O("vmid", g.VMID, "snapname", a.Get("snapname"), "description", a.Get("description"), "updated", true), nil
		},
	})

	c.Define("proxmox_delete_snapshot", Def{
		Toolset:     "snapshots",
		Title:       "Delete a snapshot",
		Description: "Delete a snapshot of a VM or container, merging its data so the guest's current state is unaffected (other snapshots are kept). force=true removes it from the config even if deleting the disk snapshots fails (may leave orphaned storage snapshots).",
		Delete:      true,
		Input: []Prop{
			P("vmid", vmidArg),
			P("node", guestNodeArg),
			P("snapname", snapshotsSnapnameArg),
			P("force", Bool().Opt().D("Remove from the config even if removing disk snapshots fails")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := guest(ctx, a)
			if err != nil {
				return nil, err
			}
			upid, err := client.Delete(ctx, snapPath(g, a.Get("snapname")), defined(js.O("force", a.Get("force"))))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("vmid", g.VMID, "type", snapshotsKind(g), "node", g.Node, "snapname", a.Get("snapname"), "deleted", true))
		},
	})
}
