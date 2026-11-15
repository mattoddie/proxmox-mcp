package tools

import (
	"testing"
)

func TestTags(t *testing.T) {
	env := startEnv(t)

	t.Run("adds and removes tags across guests", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/options$`, `{}`)
		// VM 100 has prod;web, VM 101 none, CT 200 infra.
		res := env.call(t, "proxmox_bulk_tag_guests", `{"vmids":[100,101,200],"add":["backup"],"remove":["WEB"]}`)
		eqAt(t, res, "changed", 3)
		eqAt(t, res, "guests.0.after", []any{"prod", "backup"})
		writes := env.mock.Writes()
		eq(t, len(writes), 3)
		eqStr(t, writes[0].Path, "/nodes/pve1/qemu/100/config")
		eqStr(t, writes[0].Body.Get("tags"), "prod;backup")
		eqStr(t, writes[1].Path, "/nodes/pve2/qemu/101/config")
		eqStr(t, writes[1].Body.Get("tags"), "backup")
		eqStr(t, writes[2].Path, "/nodes/pve1/lxc/200/config")
		eqStr(t, writes[2].Body.Get("tags"), "infra;backup")
	})

	t.Run("renames a tag on the guests that have it and skips unchanged ones", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/options$`, `{}`)
		res := env.call(t, "proxmox_bulk_tag_guests", `{"with_tag":"prod","vmids":[200],"add":["infra"],"remove":["prod"]}`)
		eqAt(t, res, "changed", 1)
		eqAt(t, res, "unchanged", 1)
		eqAt(t, res, "guests.1.status", "unchanged")
		w := env.mock.LastWrite()
		eqStr(t, w.Path, "/nodes/pve1/qemu/100/config")
		eqStr(t, w.Body.Get("tags"), "web;infra")
	})

	t.Run("deletes the tags key when no tags are left", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/options$`, `{}`)
		env.call(t, "proxmox_bulk_tag_guests", `{"vmids":[200],"remove":["infra"]}`)
		eqStr(t, env.mock.LastWrite().Body.Get("delete"), "tags")
	})

	t.Run("respects case-sensitive tag-style", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/options$`, `{"tag-style":"case-sensitive=1"}`)
		res := env.call(t, "proxmox_bulk_tag_guests", `{"vmids":[100],"remove":["WEB"]}`)
		eqAt(t, res, "unchanged", 1)
		vmsNoWrites(t, env)
	})

	t.Run("previews every change in a dry run", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/options$`, `{}`)
		res := env.call(t, "proxmox_bulk_tag_guests", `{"vmids":[100,200],"add":["x"],"dry_run":true}`)
		eqAt(t, res, "dry_run", true)
		eqAt(t, res, "changed", 2)
		eqAt(t, res, "guests.1.status", "would change")
		match(t, at(res, "note"), "Nothing was changed")
		vmsNoWrites(t, env)
	})

	t.Run("refuses bad selections before changing anything", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/options$`, `{}`)
		match(t, env.errorText(t, "proxmox_bulk_tag_guests", `{"vmids":[100,555],"add":["x"]}`), `No guest with VMID 555`)
		match(t, env.errorText(t, "proxmox_bulk_tag_guests", `{"add":["x"]}`), `Select guests`)
		match(t, env.errorText(t, "proxmox_bulk_tag_guests", `{"vmids":[100]}`), `add and/or remove`)
		match(t, env.errorText(t, "proxmox_bulk_tag_guests", `{"vmids":[100],"add":["bad tag"]}`), `add`)
		vmsNoWrites(t, env)
	})

	t.Run("merges tag-style and user-tag-access", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/options$`, `{"tag-style":{"shape":"dense","color-map":"prod:ff0000;dev:00ff00"},"user-tag-access":"user-allow=list,user-allow-list=a;b"}`)
		env.call(t, "proxmox_set_tag_options", `{"ordering":"config","color_map":{"dev":"","web":"0000ff:ffffff"},"user_allow":"existing","registered_tags":["prod","infra"]}`)
		b := env.mock.LastWrite().Body
		eqStr(t, b.Get("tag-style"), "shape=dense,color-map=prod:ff0000;web:0000ff:ffffff,ordering=config")
		eqStr(t, b.Get("user-tag-access"), "user-allow=existing,user-allow-list=a;b")
		eqStr(t, b.Get("registered-tags"), "prod;infra")
		falsy(t, b.Has("delete"), "delete")
	})

	t.Run("clears and resets settings", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/options$`, `{"user-tag-access":"user-allow-list=a"}`)
		env.call(t, "proxmox_set_tag_options", `{"user_allow_list":[],"registered_tags":[],"reset":["tag-style"]}`)
		eqStr(t, env.mock.LastWrite().Body.Get("delete"), "user-tag-access,registered-tags,tag-style")
		match(t, env.errorText(t, "proxmox_set_tag_options", `{"shape":"full","reset":["tag-style"]}`), `can't be combined`)
		match(t, env.errorText(t, "proxmox_set_tag_options", `{"color_map":{"prod":"red"}}`), `hex colour`)
		match(t, env.errorText(t, "proxmox_set_tag_options", `{}`), `Nothing to change`)
	})

	t.Run("is gated behind writes", func(t *testing.T) {
		names := toolNames(t, connect(t, env.client, toolOptions("read")))
		falsy(t, names["proxmox_bulk_tag_guests"])
		falsy(t, names["proxmox_set_tag_options"])
	})
}
