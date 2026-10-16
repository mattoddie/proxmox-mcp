package tools

import (
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

func poolsTestEnv(t *testing.T) *testEnv {
	t.Helper()
	env := startEnv(t)
	return env
}

func poolsReset(env *testEnv) {
	guests := js.MustParse(proxmoxtest.GuestsJSON).([]any)
	storages := js.MustParse(proxmoxtest.StoragesJSON).([]any)
	pools := map[string]*Obj{
		"production": js.O("poolid", "production", "comment", "Prod", "members", []any{guests[0], guests[2], storages[1]}),
		"empty":      js.O("poolid", "empty"),
	}
	env.mock.Reset()
	env.mock.On("GET", `^/pools$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
		if !p.Truthy("poolid") {
			return []any{
				js.O("poolid", "production", "comment", "Prod"),
				js.O("poolid", "empty", "comment", js.Undef),
			}
		}
		pool := pools[js.Str(p.Get("poolid"))]
		if pool == nil {
			return proxmoxtest.Error(404, "pool '"+js.Str(p.Get("poolid"))+"' does not exist")
		}
		members := []any{}
		for _, m := range js.Objs(or(pool.Get("members"), []any{})) {
			if !p.Truthy("type") || m.Get("type") == p.Get("type") {
				members = append(members, m)
			}
		}
		return []any{js.MustParse(js.Stringify(js.Spread(pool, js.O("members", members)), ""))}
	})
}

func TestPools(t *testing.T) {
	env := poolsTestEnv(t)

	t.Run("lists pools with member counts", func(t *testing.T) {
		poolsReset(env)
		res := env.call(t, "proxmox_list_pools", nil)
		eqAt(t, res, "total", 2)
		eqAt(t, res, "items.1", `{"poolid":"production","comment":"Prod","members":{"vms":1,"containers":1,"storage":1}}`)
		eqAt(t, res, "items.0.members", `{"vms":0,"containers":0,"storage":0}`)
		eqAt(t, env.call(t, "proxmox_list_pools", `{"search":"prod"}`), "total", 1)
	})

	t.Run("gets a pool with summarized members", func(t *testing.T) {
		poolsReset(env)
		res := env.call(t, "proxmox_get_pool", `{"poolid":"production"}`)
		eqAt(t, env.mock.RequestsTo("/pools", "GET")[0].Query, "poolid", "production")
		eqAt(t, res, "counts", `{"vms":1,"containers":1,"storage":1}`)
		eqAt(t, res, "guests.0.vmid", 100)
		eqAt(t, res, "guests.0.type", "vm")
		eqAt(t, res, "guests.1.type", "container")
		eqAt(t, res, "storage.0.storage", "local-lvm")
		eqAt(t, res, "storage.0.used_pct", 12.5)

		onlyStorage := env.call(t, "proxmox_get_pool", `{"poolid":"production","type":"storage"}`)
		eqAt(t, onlyStorage, "guests", js.Undef)
		eqAt(t, env.mock.Last().Query, "type", "storage")

		raw := env.call(t, "proxmox_get_pool", `{"poolid":"production","raw":true}`)
		eqAt(t, raw, "members.length", 3)

		match(t, env.errorText(t, "proxmox_get_pool", `{"poolid":"nope"}`), `does not exist`)
	})

	t.Run("creates pools", func(t *testing.T) {
		poolsReset(env)
		env.call(t, "proxmox_create_pool", `{"poolid":"lab","comment":"Testing"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, "/pools")
		eq(t, w.Body, `{"poolid":"lab","comment":"Testing"}`)
	})

	t.Run("adds and removes members in separate requests", func(t *testing.T) {
		poolsReset(env)
		res := env.call(t, "proxmox_update_pool", `{"poolid":"production","comment":"Production","add_vms":[101],"add_storage":["local"],"remove_vms":[200],"allow_move":true}`)
		puts := env.mock.RequestsTo("/pools", "PUT")
		if len(puts) != 2 {
			t.Fatalf("%d PUTs", len(puts))
		}
		eq(t, puts[0].Body, `{"poolid":"production","comment":"Production","vms":"101","storage":"local","allow-move":"1"}`)
		eq(t, puts[1].Body, `{"poolid":"production","vms":"200","delete":"1"}`)
		eqAt(t, res, "added", `{"vms":[101],"storage":["local"]}`)
		eqAt(t, res, "removed", `{"vms":[200]}`)
	})

	t.Run("only sends a removal request when only removing", func(t *testing.T) {
		poolsReset(env)
		env.call(t, "proxmox_update_pool", `{"poolid":"production","remove_storage":["local-lvm"]}`)
		puts := env.mock.RequestsTo("/pools", "PUT")
		if len(puts) != 1 {
			t.Fatalf("%d PUTs", len(puts))
		}
		eq(t, puts[0].Body, `{"poolid":"production","storage":"local-lvm","delete":"1"}`)
		match(t, env.errorText(t, "proxmox_update_pool", `{"poolid":"production"}`), `Nothing to change`)
	})

	t.Run("deletes pools via the poolid query parameter", func(t *testing.T) {
		poolsReset(env)
		env.call(t, "proxmox_delete_pool", `{"poolid":"parent/child"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eqStr(t, w.Path, "/pools")
		eq(t, w.Query, `{"poolid":"parent/child"}`)
	})
}
