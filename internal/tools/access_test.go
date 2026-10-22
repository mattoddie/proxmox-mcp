package tools

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

var accessTestFuture = time.Now().Unix() + 3600

func accessTestUsers() string {
	return fmt.Sprintf(`[
  {"userid":"root@pam","enable":1,"expire":0,"firstname":"Root","email":"root@example.com","realm-type":"pam","tokens":[{"tokenid":"test","privsep":0,"expire":0}]},
  {"userid":"alice@pve","enable":1,"expire":1830297600,"firstname":"Alice","lastname":"Smith","groups":"admins,ops","realm-type":"pve",
   "tokens":[{"tokenid":"ci","privsep":1,"comment":"CI"},{"tokenid":"ro"}],"totp-locked":1,"tfa-locked-until":%d},
  {"userid":"bob@ldap","enable":0,"groups":"ops","realm-type":"ldap","keys":"secretkey"}
]`, accessTestFuture)
}

const accessTestACL = `[
  {"path":"/","ugid":"admins","type":"group","roleid":"Administrator","propagate":1},
  {"path":"/vms/100","ugid":"alice@pve","type":"user","roleid":"PVEVMUser","propagate":0},
  {"path":"/vms","ugid":"alice@pve!ci","type":"token","roleid":"PVEVMAdmin","propagate":1},
  {"path":"/storage/local","ugid":"bob@ldap","type":"user","roleid":"PVEDatastoreUser","propagate":1},
  {"path":"/vmsx","ugid":"bob@ldap","type":"user","roleid":"PVEAuditor","propagate":1}
]`

func accessReset(env *testEnv) {
	env.mock.Reset()
	env.mock.Reply("GET", `^/access/users$`, accessTestUsers())
	env.mock.Reply("GET", `^/access/acl$`, accessTestACL)
	env.mock.Reply("GET", `^/access/roles/Administrator$`, `{"VM.Audit":1,"VM.PowerMgmt":1,"Sys.Audit":1,"Datastore.Audit":1,"Datastore.Allocate":1}`)
}

func accessFind(items any, userid string) any {
	for _, u := range js.Objs(items) {
		if u.Get("userid") == userid {
			return u
		}
	}
	return js.Undef
}

func accessISO(sec int64) string { return proxmox.ISO(float64(sec) * 1000) }

func TestAccess(t *testing.T) {
	env := startEnv(t)

	// ---------------------------------------------------------------- users

	t.Run("lists users with summaries and filters", func(t *testing.T) {
		accessReset(env)
		res := env.call(t, "proxmox_list_users", nil)
		eqAt(t, res, "total", 3)
		eqStr(t, at(env.mock.RequestsTo("/access/users", "GET")[0].Query, "full"), "1")
		alice := accessFind(at(res, "items"), "alice@pve")
		eqAt(t, alice, "enabled", true)
		eqAt(t, alice, "expire", accessISO(1830297600))
		eqAt(t, alice, "realm", "pve")
		eqAt(t, alice, "name", "Alice Smith")
		eqAt(t, alice, "groups", `["admins","ops"]`)
		eqAt(t, alice, "tokens", 2)
		eqAt(t, alice, "token_ids", `["ci","ro"]`)
		eqAt(t, alice, "totp_locked", true)
		truthy(t, at(alice, "tfa_locked_until"))
		eqAt(t, accessFind(at(res, "items"), "root@pam"), "expire", "never")
		eqAt(t, accessFind(at(res, "items"), "bob@ldap"), "enabled", false)

		eq(t, pluck(at(env.call(t, "proxmox_list_users", `{"realm":"pve"}`), "items"), "userid"), `["alice@pve"]`)
		eq(t, pluck(at(env.call(t, "proxmox_list_users", `{"group":"ops"}`), "items"), "userid"), `["alice@pve","bob@ldap"]`)
		eqAt(t, env.call(t, "proxmox_list_users", `{"search":"smith"}`), "total", 1)
		env.call(t, "proxmox_list_users", `{"enabled":false}`)
		reqs := env.mock.RequestsTo("/access/users", "GET")
		eqStr(t, at(reqs[len(reqs)-1].Query, "enabled"), "0")
		raw := env.call(t, "proxmox_list_users", `{"raw":true}`)
		eqAt(t, accessFind(at(raw, "items"), "bob@ldap"), "keys", "<redacted>")
	})

	t.Run("gets a user with tokens and TFA entries", func(t *testing.T) {
		accessReset(env)
		env.mock.Reply("GET", `^/access/users/alice%40pve$`, `{"enable":1,"firstname":"Alice","groups":["admins"],"tokens":{"ci":{"privsep":1,"expire":0,"comment":"CI"}},"keys":"x"}`)
		env.mock.Reply("GET", `^/access/tfa/alice%40pve$`, `[{"id":"totp1","type":"totp","description":"phone","created":1700000000,"enable":1}]`)
		u := env.call(t, "proxmox_get_user", `{"userid":"alice@pve"}`)
		eqAt(t, u, "userid", "alice@pve")
		eqAt(t, u, "groups", `["admins"]`)
		eqAt(t, u, "yubico_keys", "<set>")
		eqAt(t, u, "tokens", `[{"id":"alice@pve!ci","userid":"alice@pve","tokenid":"ci","privsep":true,"expire":"never","comment":"CI"}]`)
		eqAt(t, u, "tfa.0.type", "totp")
		eqAt(t, u, "tfa.0.created", accessISO(1700000000))
	})

	t.Run("creates a user without echoing the password", func(t *testing.T) {
		accessReset(env)
		env.mock.Reply("GET", `^/access/users/carol%40pve$`, `{"enable":1,"email":"c@example.com","groups":["ops"]}`)
		res := env.call(t, "proxmox_save_user", `{"userid":"carol@pve","create":true,"password":"s3cretPassw0rd","email":"c@example.com","groups":["ops","dev"],"expire":"2027-01-01T00:00:00Z","enable":true}`)
		w := env.mock.RequestsTo("/access/users", "POST")[0]
		eqStr(t, w.Path, "/access/users")
		eqAt(t, w.Body, "userid", "carol@pve")
		eqAt(t, w.Body, "password", "s3cretPassw0rd")
		eqAt(t, w.Body, "groups", "ops,dev")
		eqStr(t, at(w.Body, "expire"), fmt.Sprint(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).Unix()))
		eqStr(t, at(w.Body, "enable"), "1")
		eqAt(t, res, "created", "carol@pve")
		if strings.Contains(js.Stringify(res, ""), "s3cretPassw0rd") {
			t.Fatal("password echoed")
		}
	})

	t.Run("updates a user with append, and refuses passwords on update", func(t *testing.T) {
		accessReset(env)
		env.mock.Reply("GET", `^/access/users/alice%40pve$`, `{"enable":0}`)
		res := env.call(t, "proxmox_save_user", `{"userid":"alice@pve","groups":["dev"],"append":true,"enable":false,"expire":0}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, "/access/users/alice%40pve")
		eq(t, w.Body, `{"append":"1","groups":"dev","enable":"0","expire":"0"}`)
		eqAt(t, res, "updated", "alice@pve")
		eqAt(t, res, "user.enabled", false)

		match(t, env.errorText(t, "proxmox_save_user", `{"userid":"alice@pve","password":"newpassword"}`), `proxmox_change_password`)
		match(t, env.errorText(t, "proxmox_save_user", `{"userid":"alice@pve","expire":"not a date"}`), `Invalid date`)
	})

	t.Run("deletes a user", func(t *testing.T) {
		accessReset(env)
		eq(t, env.call(t, "proxmox_delete_user", `{"userid":"bob@ldap"}`), `{"deleted":"bob@ldap"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eqStr(t, w.Path, "/access/users/bob%40ldap")
	})

	t.Run("changes a password without echoing it", func(t *testing.T) {
		accessReset(env)
		res := env.call(t, "proxmox_change_password", `{"userid":"alice@pve","password":"BrandNewPass1","confirmation_password":"OldPass"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, "/access/password")
		eq(t, w.Body, `{"userid":"alice@pve","password":"BrandNewPass1","confirmation-password":"OldPass"}`)
		eq(t, res, `{"password_changed":"alice@pve"}`)
	})

	// ---------------------------------------------------------------- groups

	t.Run("lists, gets, saves and deletes groups", func(t *testing.T) {
		accessReset(env)
		env.mock.Reply("GET", `^/access/groups$`, `[{"groupid":"ops","users":"alice@pve,bob@ldap","comment":"Operations"},{"groupid":"admins","users":"alice@pve"}]`)
		env.mock.Reply("GET", `^/access/groups/ops$`, `{"members":["alice@pve","bob@ldap"],"comment":"Operations"}`)
		list := env.call(t, "proxmox_list_groups", nil)
		eqAt(t, list, "items.0", `{"groupid":"admins","members":["alice@pve"]}`)
		eqAt(t, list, "items.1.members", `["alice@pve","bob@ldap"]`)
		eqAt(t, env.call(t, "proxmox_list_groups", `{"search":"operations"}`), "total", 1)

		eq(t, env.call(t, "proxmox_get_group", `{"groupid":"ops"}`), `{"groupid":"ops","members":["alice@pve","bob@ldap"],"comment":"Operations"}`)

		env.call(t, "proxmox_save_group", `{"groupid":"dev","create":true,"comment":"Developers"}`)
		eq(t, env.mock.LastWrite().Body, `{"groupid":"dev","comment":"Developers"}`)
		eqStr(t, env.mock.LastWrite().Path, "/access/groups")

		env.call(t, "proxmox_save_group", `{"groupid":"dev","comment":"Devs"}`)
		eqStr(t, env.mock.LastWrite().Method, "PUT")
		eqStr(t, env.mock.LastWrite().Path, "/access/groups/dev")

		env.call(t, "proxmox_delete_group", `{"groupid":"dev"}`)
		eqStr(t, env.mock.LastWrite().Method, "DELETE")
		eqStr(t, env.mock.LastWrite().Path, "/access/groups/dev")
	})

	// ---------------------------------------------------------------- roles

	t.Run("lists roles flagging built-ins", func(t *testing.T) {
		accessReset(env)
		env.mock.Reply("GET", `^/access/roles$`, `[{"roleid":"PVEAuditor","special":1,"privs":"VM.Audit,Sys.Audit"},{"roleid":"Custom","privs":"VM.PowerMgmt"},{"roleid":"NoAccess","special":1,"privs":""}]`)
		all := env.call(t, "proxmox_list_roles", nil)
		eqAt(t, all, "items.0", `{"roleid":"Custom","privilege_count":1,"privileges":["VM.PowerMgmt"]}`)
		eqAt(t, all, "items.2", `{"roleid":"PVEAuditor","builtin":true,"privilege_count":2,"privileges":["VM.Audit","Sys.Audit"]}`)
		custom := env.call(t, "proxmox_list_roles", `{"builtin":false,"privileges":false}`)
		eqAt(t, custom, "items", `[{"roleid":"Custom","privilege_count":1}]`)
	})

	t.Run("gets a role and the privilege list", func(t *testing.T) {
		accessReset(env)
		env.mock.Reply("GET", `^/access/roles/Custom$`, `{"VM.Audit":1,"VM.PowerMgmt":1,"Sys.Audit":1}`)
		r := env.call(t, "proxmox_get_role", `{"roleid":"Custom"}`)
		eqAt(t, r, "privileges", `["Sys.Audit","VM.Audit","VM.PowerMgmt"]`)
		eqAt(t, r, "by_area", `{"Sys":["Sys.Audit"],"VM":["VM.Audit","VM.PowerMgmt"]}`)

		p := env.call(t, "proxmox_list_privileges", nil)
		eqAt(t, p, "count", 5)
		eqAt(t, p, "by_area.Datastore", `["Datastore.Allocate","Datastore.Audit"]`)
	})

	t.Run("saves and deletes roles", func(t *testing.T) {
		accessReset(env)
		env.mock.Reply("GET", `^/access/roles/Ops$`, `{"VM.Audit":1,"VM.PowerMgmt":1}`)
		created := env.call(t, "proxmox_save_role", `{"roleid":"Ops","create":true,"privs":["VM.Audit","VM.PowerMgmt"]}`)
		eq(t, env.mock.LastWrite().Body, `{"roleid":"Ops","privs":"VM.Audit,VM.PowerMgmt"}`)
		eqStr(t, env.mock.LastWrite().Method, "POST")
		eqAt(t, created, "privileges", `["VM.Audit","VM.PowerMgmt"]`)

		env.call(t, "proxmox_save_role", `{"roleid":"Ops","privs":["VM.Console"],"append":true}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, "/access/roles/Ops")
		eq(t, w.Body, `{"privs":"VM.Console","append":"1"}`)
		// An update without privs would wipe the role, so it is refused.
		match(t, env.errorText(t, "proxmox_save_role", `{"roleid":"Ops"}`), "Give `privs`")
		env.call(t, "proxmox_save_role", `{"roleid":"Ops","privs":[]}`)
		eq(t, env.mock.LastWrite().Body, `{"privs":""}`)

		env.call(t, "proxmox_delete_role", `{"roleid":"Ops"}`)
		eqStr(t, env.mock.LastWrite().Method, "DELETE")
		eqStr(t, env.mock.LastWrite().Path, "/access/roles/Ops")
	})

	// ---------------------------------------------------------------- ACL / permissions

	t.Run("lists and filters ACL entries", func(t *testing.T) {
		accessReset(env)
		all := env.call(t, "proxmox_list_acl", nil)
		eqAt(t, all, "total", 5)
		eqAt(t, all, "items.0", `{"path":"/","ugid":"admins","type":"group","role":"Administrator","propagate":true}`)

		vms := env.call(t, "proxmox_list_acl", `{"path":"/vms"}`)
		eq(t, pluck(at(vms, "items"), "path"), `["/vms","/vms/100"]`)
		alice := env.call(t, "proxmox_list_acl", `{"user":"alice@pve"}`)
		eqAt(t, alice, "items", `[{"path":"/vms/100","ugid":"alice@pve","type":"user","role":"PVEVMUser","propagate":false}]`)
		eqAt(t, env.call(t, "proxmox_list_acl", `{"group":"admins"}`), "total", 1)
		eqAt(t, env.call(t, "proxmox_list_acl", `{"type":"token"}`), "items.0.ugid", "alice@pve!ci")
		eqAt(t, env.call(t, "proxmox_list_acl", `{"role":"PVEDatastoreUser"}`), "total", 1)
	})

	t.Run("grants and revokes ACL entries", func(t *testing.T) {
		accessReset(env)
		res := env.call(t, "proxmox_update_acl", `{"path":"/vms/100","roles":["PVEVMUser"],"users":["alice@pve","bob@ldap"],"tokens":["alice@pve!ci"]}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, "/access/acl")
		eq(t, w.Body, `{"path":"/vms/100","roles":"PVEVMUser","users":"alice@pve,bob@ldap","tokens":"alice@pve!ci"}`)
		eqAt(t, res, "granted", `["PVEVMUser"]`)
		eqAt(t, res, "propagate", true)

		rev := env.call(t, "proxmox_update_acl", `{"path":"/","roles":["Administrator"],"groups":["admins"],"delete":true,"propagate":false}`)
		eq(t, env.mock.LastWrite().Body, `{"path":"/","roles":"Administrator","groups":"admins","propagate":"0","delete":"1"}`)
		eqAt(t, rev, "revoked", `["Administrator"]`)

		n := len(env.mock.Requests())
		match(t, env.errorText(t, "proxmox_update_acl", `{"path":"/","roles":["PVEAuditor"]}`), `at least one of users, groups or tokens`)
		if len(env.mock.Requests()) != n {
			t.Fatal("request sent")
		}
	})

	t.Run("returns effective permissions", func(t *testing.T) {
		accessReset(env)
		env.mock.Reply("GET", `^/access/permissions$`, `{"/":{"Sys.Audit":1,"VM.Audit":1},"/vms/100":{"VM.PowerMgmt":0,"VM.Audit":1}}`)
		own := env.callObj(t, "proxmox_get_permissions", nil)
		eqAt(t, own, "subject", "root@pam!test")
		eq(t, own.ObjAt("paths").Get("/"), `{"privileges":["Sys.Audit","VM.Audit"]}`)
		eq(t, own.ObjAt("paths").Get("/vms/100"), `{"privileges":["VM.Audit","VM.PowerMgmt"],"not_propagated":["VM.PowerMgmt"]}`)

		other := env.call(t, "proxmox_get_permissions", `{"userid":"alice@pve","path":"/vms/100"}`)
		eqAt(t, other, "subject", "alice@pve")
		reqs := env.mock.RequestsTo("/access/permissions", "")
		eq(t, reqs[len(reqs)-1].Query, `{"userid":"alice@pve","path":"/vms/100"}`)
	})

	// ---------------------------------------------------------------- tokens

	t.Run("lists API tokens for all users or one user", func(t *testing.T) {
		accessReset(env)
		all := env.call(t, "proxmox_list_api_tokens", nil)
		eq(t, pluck(at(all, "items"), "id"), `["alice@pve!ci","alice@pve!ro","root@pam!test"]`)
		eqAt(t, all, "items.0", `{"id":"alice@pve!ci","userid":"alice@pve","tokenid":"ci","privsep":true,"expire":"same as user","comment":"CI"}`)
		eqAt(t, all, "items.2.privsep", false)
		eqAt(t, all, "items.2.expire", "never")

		env.mock.Reply("GET", `^/access/users/alice%40pve/token$`, `[{"tokenid":"ci","privsep":1}]`)
		one := env.call(t, "proxmox_list_api_tokens", `{"userid":"alice@pve"}`)
		eq(t, pluck(at(one, "items"), "id"), `["alice@pve!ci"]`)
	})

	t.Run("gets an API token with its ACL", func(t *testing.T) {
		accessReset(env)
		env.mock.Reply("GET", `^/access/users/alice%40pve/token/ci$`, fmt.Sprintf(`{"privsep":1,"expire":%d,"comment":"CI"}`, accessTestFuture))
		tok := env.call(t, "proxmox_get_api_token", `{"userid":"alice@pve","tokenid":"ci"}`)
		eqAt(t, tok, "id", "alice@pve!ci")
		eqAt(t, tok, "expire", accessISO(accessTestFuture))
		eqAt(t, tok, "acl", `[{"path":"/vms","role":"PVEVMAdmin","propagate":true}]`)
	})

	t.Run("creates an API token and returns the secret once", func(t *testing.T) {
		accessReset(env)
		env.mock.Reply("POST", `^/access/users/alice%40pve/token/deploy$`, `{"full-tokenid":"alice@pve!deploy","value":"aaaa-bbbb-cccc","info":{"privsep":1,"expire":0}}`)
		r := env.call(t, "proxmox_create_api_token", `{"userid":"alice@pve","tokenid":"deploy","comment":"deploy","privsep":true,"expire":0}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, "/access/users/alice%40pve/token/deploy")
		eq(t, w.Body, `{"comment":"deploy","expire":"0","privsep":"1"}`)
		eqAt(t, r, "token_id", "alice@pve!deploy")
		eqAt(t, r, "secret", "aaaa-bbbb-cccc")
		eqAt(t, r, "authorization_header", "PVEAPIToken=alice@pve!deploy=aaaa-bbbb-cccc")
		eqAt(t, r, "privsep", true)
		match(t, at(r, "note"), `cannot show it again`)
	})

	t.Run("updates, regenerates and deletes API tokens", func(t *testing.T) {
		accessReset(env)
		env.mock.On("PUT", `^/access/users/alice%40pve/token/ci$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			if p.Get("regenerate") == "1" {
				return js.O("privsep", 1, "value", "new-secret", "full-tokenid", "alice@pve!ci")
			}
			return js.O("privsep", 0, "comment", p.Get("comment"))
		})
		upd := env.call(t, "proxmox_update_api_token", `{"userid":"alice@pve","tokenid":"ci","comment":"new","privsep":false,"delete":["expire"]}`)
		eq(t, env.mock.LastWrite().Body, `{"comment":"new","privsep":"0","delete":"expire"}`)
		eqAt(t, upd, "token.privsep", false)
		eqAt(t, upd, "secret", js.Undef)

		regen := env.call(t, "proxmox_update_api_token", `{"userid":"alice@pve","tokenid":"ci","regenerate":true}`)
		eqAt(t, regen, "secret", "new-secret")
		match(t, at(regen, "note"), `old secret no longer works`)

		env.call(t, "proxmox_delete_api_token", `{"userid":"alice@pve","tokenid":"ci"}`)
		eqStr(t, env.mock.LastWrite().Method, "DELETE")
		eqStr(t, env.mock.LastWrite().Path, "/access/users/alice%40pve/token/ci")
	})

	// ---------------------------------------------------------------- realms

	t.Run("lists and gets realms with secrets redacted", func(t *testing.T) {
		accessReset(env)
		env.mock.Reply("GET", `^/access/domains$`, `[{"realm":"pam","type":"pam","comment":"Linux PAM"},{"realm":"corp","type":"ldap","tfa":"oath"},{"realm":"yk","type":"pve","tfa":"type=yubico,id=42,key=yubisecret,url=https://yk"}]`)
		env.mock.Reply("GET", `^/access/domains/corp$`, `{"type":"ldap","server1":"ldap.corp","base_dn":"dc=corp","bind_dn":"cn=bind,dc=corp","password":"bindpw","client-key":"oidc-secret","digest":"d1"}`)
		list := env.call(t, "proxmox_list_realms", nil)
		eqAt(t, list, "items.1", `{"realm":"corp","type":"ldap","tfa":"oath"}`)
		eqAt(t, list, "items.2.tfa", "type=yubico,id=42,key=<redacted>,url=https://yk")
		r := env.call(t, "proxmox_get_realm", `{"realm":"corp"}`)
		eqAt(t, r, "realm", "corp")
		eqAt(t, r, "server1", "ldap.corp")
		eqAt(t, r, "password", "<redacted>")
		eqAt(t, r, "client-key", "<redacted>")
		if strings.Contains(js.Stringify(r, ""), "bindpw") {
			t.Fatal("bind password leaked")
		}
	})

	t.Run("creates and updates realms", func(t *testing.T) {
		accessReset(env)
		env.mock.Reply("GET", `^/access/domains/corp$`, `{"type":"ldap","server1":"ldap.corp","password":"bindpw"}`)
		created := env.call(t, "proxmox_save_realm", `{"realm":"corp","create":true,"type":"ldap","server1":"ldap.corp","base_dn":"dc=corp","user_attr":"uid","bind_dn":"cn=bind,dc=corp","bind_password":"bindpw","mode":"ldaps","extra":{"group_dn":"ou=groups,dc=corp"}}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, "/access/domains")
		eq(t, w.Body, `{"realm":"corp","type":"ldap","server1":"ldap.corp","mode":"ldaps","base_dn":"dc=corp","user_attr":"uid","bind_dn":"cn=bind,dc=corp","password":"bindpw","group_dn":"ou=groups,dc=corp"}`)
		if strings.Contains(js.Stringify(created, ""), "bindpw") {
			t.Fatal("bind password echoed")
		}

		env.call(t, "proxmox_save_realm", `{"realm":"sso","issuer_url":"https://idp","client_id":"pve","client_key":"k","delete":["comment"],"digest":"d"}`)
		u := env.mock.LastWrite()
		eqStr(t, u.Method, "PUT")
		eqStr(t, u.Path, "/access/domains/sso")
		eq(t, u.Body, `{"issuer-url":"https://idp","client-id":"pve","client-key":"k","delete":"comment","digest":"d"}`)

		match(t, env.errorText(t, "proxmox_save_realm", `{"realm":"x","create":true}`), `type is required`)
	})

	t.Run("deletes and syncs realms", func(t *testing.T) {
		accessReset(env)
		env.call(t, "proxmox_delete_realm", `{"realm":"corp"}`)
		eqStr(t, env.mock.LastWrite().Method, "DELETE")
		eqStr(t, env.mock.LastWrite().Path, "/access/domains/corp")

		upid := env.mock.Upid("pve1", "auth-realm-sync", "corp")
		env.mock.Reply("POST", `^/access/domains/corp/sync$`, upid)
		res := env.call(t, "proxmox_sync_realm", `{"realm":"corp","dry_run":true,"scope":"both","remove_vanished":"acl;entry"}`)
		eqAt(t, res, "status", "ok")
		eqAt(t, res, "realm", "corp")
		eqAt(t, res, "dry_run", true)
		eq(t, env.mock.RequestsTo("/access/domains/corp/sync", "POST")[0].Body, `{"dry-run":"1","scope":"both","remove-vanished":"acl;entry"}`)

		failed := env.mock.Upid("pve1", "auth-realm-sync", "corp", proxmoxtest.Task{Status: "stopped", ExitStatus: "bind failed", Log: []string{"error: bind failed"}})
		env.mock.Reply("POST", `^/access/domains/corp/sync$`, failed)
		match(t, env.errorText(t, "proxmox_sync_realm", `{"realm":"corp"}`), `bind failed`)

		eqAt(t, env.call(t, "proxmox_sync_realm", `{"realm":"corp","wait":false}`), "status", "started")
	})

	// ---------------------------------------------------------------- TFA

	t.Run("lists TFA entries without secrets", func(t *testing.T) {
		accessReset(env)
		env.mock.Reply("GET", `^/access/tfa$`, fmt.Sprintf(`[
			{"userid":"root@pam","entries":[{"id":"r1","type":"recovery","description":"","created":1700000000}]},
			{"userid":"alice@pve","totp-locked":1,"tfa-locked-until":%d,"entries":[{"id":"t1","type":"totp","description":"phone","created":1700000000,"enable":0}]}]`, accessTestFuture))
		env.mock.Reply("GET", `^/access/tfa/alice%40pve$`, `[{"id":"t1","type":"totp","description":"phone","created":1700000000,"enable":1}]`)
		all := env.call(t, "proxmox_list_tfa", nil)
		eqAt(t, all, "items.0.userid", "alice@pve")
		eqAt(t, all, "items.0.totp_locked", true)
		truthy(t, at(all, "items.0.tfa_locked_until"))
		eqAt(t, all, "items.0.entries.0.enabled", false)
		eqAt(t, all, "items.1.entries.0", js.O("id", "r1", "type", "recovery", "created", accessISO(1700000000), "enabled", true))

		one := env.call(t, "proxmox_list_tfa", `{"userid":"alice@pve"}`)
		eqAt(t, one, "userid", "alice@pve")
		eqAt(t, one, "items.0.description", "phone")
	})

	t.Run("deletes TFA entries and unlocks TFA", func(t *testing.T) {
		accessReset(env)
		env.call(t, "proxmox_delete_tfa", `{"userid":"alice@pve","id":"t1"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eqStr(t, w.Path, "/access/tfa/alice%40pve/t1")

		env.mock.Reply("PUT", `^/access/users/alice%40pve/unlock-tfa$`, 1)
		r := env.call(t, "proxmox_unlock_tfa", `{"userid":"alice@pve"}`)
		eqStr(t, env.mock.LastWrite().Path, "/access/users/alice%40pve/unlock-tfa")
		eq(t, r, `{"userid":"alice@pve","was_locked":true,"unlocked":true}`)
	})

	// ---------------------------------------------------------------- realm sync jobs

	t.Run("lists, saves and deletes realm sync jobs", func(t *testing.T) {
		accessReset(env)
		env.mock.Reply("GET", `^/cluster/jobs/realm-sync$`, `[{"id":"nightly","realm":"corp","enabled":1,"schedule":"daily","scope":"both","last-run":1700000000,"next-run":1700086400}]`)
		env.mock.Reply("GET", `^/cluster/jobs/realm-sync/nightly$`, `{"id":"nightly","realm":"corp","schedule":"daily"}`)
		jobs := env.call(t, "proxmox_list_realm_sync_jobs", nil)
		eqAt(t, jobs, "items.0", js.O("id", "nightly", "realm", "corp", "enabled", true, "schedule", "daily", "scope", "both",
			"last_run", accessISO(1700000000), "next_run", accessISO(1700086400)))

		env.call(t, "proxmox_save_realm_sync_job", `{"id":"nightly","create":true,"realm":"corp","schedule":"daily","scope":"users","enable_new":false}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, "/cluster/jobs/realm-sync/nightly")
		eq(t, w.Body, `{"realm":"corp","schedule":"daily","scope":"users","enable-new":"0"}`)

		// Update without schedule re-sends the current one (the API requires it).
		env.call(t, "proxmox_save_realm_sync_job", `{"id":"nightly","enabled":false,"remove_vanished":"acl"}`)
		w = env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eq(t, w.Body, `{"schedule":"daily","enabled":"0","remove-vanished":"acl"}`)

		match(t, env.errorText(t, "proxmox_save_realm_sync_job", `{"id":"x","create":true}`), `realm and schedule are required`)

		env.call(t, "proxmox_delete_realm_sync_job", `{"id":"nightly"}`)
		eqStr(t, env.mock.LastWrite().Method, "DELETE")
		eqStr(t, env.mock.LastWrite().Path, "/cluster/jobs/realm-sync/nightly")
	})

	t.Run("converts expiry dates", func(t *testing.T) {
		check := func(in any, want any) {
			t.Helper()
			got, err := accessToEpoch(in)
			if err != nil {
				t.Fatalf("%v: %v", in, err)
			}
			eq(t, got, want, in)
		}
		check(js.Undef, js.Undef)
		check(0.0, 0)
		check("1700000000", 1700000000)
		check("2027-01-01", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).Unix())
		check("2027-01-01T12:00:00+01:00", time.Date(2027, 1, 1, 11, 0, 0, 0, time.UTC).Unix())
		check("2027-02-30", time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC).Unix())
		check("2027-01-01T24:00:00Z", time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC).Unix())
		check("Fri, 01 Jan 2027 00:00:00 GMT", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).Unix())
		for _, bad := range []string{"tomorrow-ish", "2027-13-01", "not a date"} {
			_, err := accessToEpoch(bad)
			if err == nil || !strings.Contains(err.Error(), "Invalid date") {
				t.Fatalf("%s: %v", bad, err)
			}
		}
	})
}
