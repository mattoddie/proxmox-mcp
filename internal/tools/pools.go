package tools

import (
	"context"
	"errors"
	"fmt"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

var poolsIDArg = Str().D("Pool ID, e.g. \"production\" (nested pools as \"parent/child\"; see proxmox_list_pools)")

func poolsMemberCounts(members any) *Obj {
	list := js.Objs(or(members, []any{}))
	count := func(t string) int { return len(filterObjs(list, func(m *Obj) bool { return m.Get("type") == t })) }
	return js.O("vms", count("qemu"), "containers", count("lxc"), "storage", count("storage"))
}

func poolsSummarizeMember(m *Obj) any {
	if m.Get("type") == "storage" {
		return proxmox.C(
			"type", "storage",
			"storage", m.Get("storage"),
			"node", m.Get("node"),
			"status", m.Get("status"),
			"plugin", m.Get("plugintype"),
			"usage", proxmox.Usage(m.Get("disk"), m.Get("maxdisk")),
			"used_pct", proxmox.Percent(m.Get("disk"), m.Get("maxdisk")),
		)
	}
	return summarizeGuest(m)
}

// poolsGet fetches one pool: GET /pools?poolid=X returns a one-element array on PVE 8+.
func poolsGet(ctx context.Context, client *proxmox.Client, poolid, typ any) (*Obj, error) {
	res, err := client.Get(ctx, "/pools", defined(js.O("poolid", poolid, "type", typ)))
	if err != nil {
		return nil, err
	}
	var pool *Obj
	if arr, ok := res.([]any); ok {
		for _, p := range js.Objs(arr) {
			if p.Get("poolid") == poolid {
				pool = p
				break
			}
		}
		if pool == nil && len(arr) > 0 {
			pool, _ = arr[0].(*Obj)
		}
	} else {
		pool, _ = res.(*Obj)
	}
	if pool == nil {
		return nil, fmt.Errorf("No pool %s (see proxmox_list_pools)", js.Str(poolid))
	}
	return pool, nil
}

// poolsList joins IDs with commas (undefined when empty).
func poolsList(v any) any {
	if arr, ok := v.([]any); ok && len(arr) > 0 {
		return js.Str(arr)
	}
	return js.Undef
}

func registerPools(c *Context) {
	client := c.Client

	c.Define("proxmox_list_pools", Def{
		Toolset:     "pools",
		Title:       "List resource pools",
		Description: "List resource pools (groups of VMs, containers and storage used for permissions and organisation) with their comments and member counts. Not ZFS or Ceph pools; see proxmox_get_pool for members.",
		Input:       []Prop{P("search", searchArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			pools, err := client.GetList(ctx, "/pools", nil)
			if err != nil {
				return nil, err
			}
			sortObjs(pools, func(x, y *Obj) float64 { return compareStr(x.Get("poolid"), y.Get("poolid")) })
			res := filterAndLimit(pools, a.Get("search"), js.Undef, func(p *Obj) []any { return []any{p.Get("poolid"), p.Get("comment")} })
			// The list endpoint has no members; fetch each pool (pools are few).
			return res.Map(func(p *Obj) any {
				var members any = js.Undef
				if d, err := poolsGet(ctx, client, p.Get("poolid"), js.Undef); err == nil {
					members = poolsMemberCounts(d.Get("members"))
				}
				return proxmox.C("poolid", p.Get("poolid"), "comment", p.Get("comment"), "members", members)
			}), nil
		},
	})

	c.Define("proxmox_get_pool", Def{
		Toolset:     "pools",
		Title:       "Get resource pool",
		Description: "Get a resource pool's comment and members: guests (status, node, CPU/memory use) and storage (usage).",
		Input: []Prop{
			P("poolid", poolsIDArg),
			P("type", Enum("qemu", "lxc", "storage").Opt().D("Only members of this type")),
			P("raw", rawArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			pool, err := poolsGet(ctx, client, a.Get("poolid"), a.Get("type"))
			if err != nil {
				return nil, err
			}
			if a.Truthy("raw") {
				return pool, nil
			}
			members := js.Objs(or(pool.Get("members"), []any{}))
			return proxmox.C(
				"poolid", pool.Get("poolid"),
				"comment", pool.Get("comment"),
				"counts", poolsMemberCounts(js.Array(members)),
				"guests", mapObjs(filterObjs(members, func(m *Obj) bool { return m.Get("type") != "storage" }), poolsSummarizeMember),
				"storage", mapObjs(filterObjs(members, func(m *Obj) bool { return m.Get("type") == "storage" }), poolsSummarizeMember),
			), nil
		},
	})

	c.Define("proxmox_create_pool", Def{
		Toolset:     "pools",
		Title:       "Create resource pool",
		Description: "Create a resource pool. Add VMs, containers and storage to it with proxmox_update_pool.",
		Input:       []Prop{P("poolid", poolsIDArg), P("comment", Str().Opt().D("Description"))},
		Write:       true,
		Destructive: False,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Post(ctx, "/pools", defined(js.O("poolid", a.Get("poolid"), "comment", a.Get("comment")))); err != nil {
				return nil, err
			}
			return js.O("poolid", a.Get("poolid"), "created", true), nil
		},
	})

	c.Define("proxmox_update_pool", Def{
		Toolset:     "pools",
		Title:       "Update resource pool",
		Description: "Change a resource pool's comment and add or remove member guests and storage. A guest can only be in one pool; set allow_move to move it here from another pool.",
		Input: []Prop{
			P("poolid", poolsIDArg),
			P("comment", Str().Opt().D("New description")),
			P("add_vms", Arr(Int()).Opt().D("Guest VMIDs to add")),
			P("remove_vms", Arr(Int()).Opt().D("Guest VMIDs to remove")),
			P("add_storage", Arr(Str()).Opt().D("Storage IDs to add")),
			P("remove_storage", Arr(Str()).Opt().D("Storage IDs to remove")),
			P("allow_move", Bool().Opt().D("Move added guests out of their current pool instead of failing")),
		},
		Write:       true,
		Destructive: False,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			adding := len(a.Arr("add_vms")) > 0 || len(a.Arr("add_storage")) > 0
			removing := len(a.Arr("remove_vms")) > 0 || len(a.Arr("remove_storage")) > 0
			if !a.Defined("comment") && !adding && !removing {
				return nil, errors.New("Nothing to change")
			}
			// The API's `delete` flag applies to the whole call, so adds and removes need separate requests.
			if a.Defined("comment") || adding {
				if _, err := client.Put(ctx, "/pools", defined(js.O(
					"poolid", a.Get("poolid"),
					"comment", a.Get("comment"),
					"vms", poolsList(a.Get("add_vms")),
					"storage", poolsList(a.Get("add_storage")),
					"allow-move", a.Get("allow_move"),
				))); err != nil {
					return nil, err
				}
			}
			if removing {
				if _, err := client.Put(ctx, "/pools", defined(js.O(
					"poolid", a.Get("poolid"),
					"vms", poolsList(a.Get("remove_vms")),
					"storage", poolsList(a.Get("remove_storage")),
					"delete", true,
				))); err != nil {
					return nil, err
				}
			}
			return proxmox.C(
				"poolid", a.Get("poolid"),
				"comment", a.Get("comment"),
				"added", proxmox.C("vms", a.Get("add_vms"), "storage", a.Get("add_storage")),
				"removed", proxmox.C("vms", a.Get("remove_vms"), "storage", a.Get("remove_storage")),
			), nil
		},
	})

	c.Define("proxmox_delete_pool", Def{
		Toolset:     "pools",
		Title:       "Delete resource pool",
		Description: "Delete a resource pool. It must be empty: remove its members with proxmox_update_pool first. The guests and storage themselves are not affected.",
		Input:       []Prop{P("poolid", poolsIDArg)},
		Delete:      true,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/pools", js.O("poolid", a.Get("poolid"))); err != nil {
				return nil, err
			}
			return js.O("poolid", a.Get("poolid"), "deleted", true), nil
		},
	})
}
