package tools

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

func notificationsTestB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

const (
	notificationsTestTargets = `[
		{"name":"mail-to-root","type":"sendmail","origin":"builtin","comment":"Send mails to root@pam's email address"},
		{"name":"gotify1","type":"gotify","disable":1},
		{"name":"smtp1","type":"smtp","origin":"user-created"},
		{"name":"hook","type":"webhook"}]`
	notificationsTestSmtp1  = `{"name":"smtp1","server":"mail.example.com","from-address":"pve@example.com","username":"pve","password":"smtppw","mode":"tls","mailto":["ops@example.com"]}`
	notificationsTestGotify = `{"name":"gotify1","server":"https://gotify","token":"gotify-token","disable":1}`
)

func notificationsTestHook() *Obj {
	b := notificationsTestB64
	return js.O(
		"name", "hook",
		"url", "https://hooks.example.com/{{ secrets.path }}",
		"method", "post",
		"header", []any{"name=Content-Type,value=" + b("application/json"), "name=Authorization,value=" + b("Bearer xyz")},
		"body", b(`{"text":"{{ message }}"}`),
		"secret", []any{"name=path,value=" + b("s3cret"), "name=other"},
		"digest", "d1",
	)
}

func TestNotifications(t *testing.T) {
	env := startEnv(t)
	b64 := notificationsTestB64
	setup := func() {
		env.mock.Reset()
		hook := js.Stringify(notificationsTestHook(), "")
		env.mock.Reply("GET", `^/cluster/notifications/targets$`, notificationsTestTargets)
		env.mock.Reply("GET", `^/cluster/notifications/endpoints/smtp$`, "["+notificationsTestSmtp1+"]")
		env.mock.Reply("GET", `^/cluster/notifications/endpoints/gotify$`, "["+notificationsTestGotify+"]")
		env.mock.Reply("GET", `^/cluster/notifications/endpoints/webhook$`, "["+hook+"]")
		env.mock.Reply("GET", `^/cluster/notifications/endpoints/sendmail$`, `[{"name":"mail-to-root","mailto-user":["root@pam"],"origin":"builtin"}]`)
		env.mock.Reply("GET", `^/cluster/notifications/endpoints/smtp/smtp1$`, notificationsTestSmtp1)
		env.mock.Reply("GET", `^/cluster/notifications/endpoints/gotify/gotify1$`, notificationsTestGotify)
		env.mock.Reply("GET", `^/cluster/notifications/endpoints/webhook/hook$`, hook)
	}
	find := func(list any, name string) any {
		for _, x := range list.([]any) {
			if at(x, "name") == name {
				return x
			}
		}
		return js.Undef
	}

	t.Run("lists targets, optionally with redacted details", func(t *testing.T) {
		setup()
		list := env.call(t, "proxmox_list_notification_targets", nil)
		eq(t, pluck(list, "name"), `["gotify1","hook","mail-to-root","smtp1"]`)
		eqAt(t, list, "0", `{"name":"gotify1","type":"gotify","disabled":true}`)
		eqAt(t, list, "2.origin", "builtin")
		if n := len(env.mock.RequestsTo("/cluster/notifications/endpoints", "")); n != 0 {
			t.Fatalf("%d endpoint requests", n)
		}

		detailed := env.call(t, "proxmox_list_notification_targets", `{"details":true}`)
		text := js.Stringify(detailed, "")
		for _, secret := range []string{"smtppw", "gotify-token", "s3cret", "Bearer xyz"} {
			if strings.Contains(text, secret) {
				t.Fatalf("leaked %s", secret)
			}
		}
		smtp := find(detailed, "smtp1")
		eqAt(t, smtp, "config.server", "mail.example.com")
		eqAt(t, smtp, "config.password", "<redacted>")
		eqAt(t, find(detailed, "mail-to-root"), "config", `{"mailto-user":["root@pam"]}`)
	})

	t.Run("gets endpoints, looking up the type and redacting secrets", func(t *testing.T) {
		setup()
		g := env.call(t, "proxmox_get_notification_endpoint", `{"name":"gotify1"}`)
		eqAt(t, g, "type", "gotify")
		eqAt(t, g, "token", "<redacted>")
		eqAt(t, g, "disabled", true)

		h := env.call(t, "proxmox_get_notification_endpoint", `{"name":"hook","type":"webhook"}`)
		if n := len(env.mock.RequestsTo("/cluster/notifications/targets", "")); n != 1 {
			t.Fatalf("%d target lookups, want 1 (only for the gotify lookup)", n)
		}
		eqAt(t, h, "header", `{"Content-Type":"application/json","Authorization":"<redacted>"}`)
		eqAt(t, h, "secret", `{"path":"<redacted>","other":"<redacted>"}`)
		eqStr(t, at(h, "body"), `{"text":"{{ message }}"}`)
		eqAt(t, h, "digest", "d1")

		raw := env.call(t, "proxmox_get_notification_endpoint", `{"name":"hook","type":"webhook","raw":true}`)
		rawText := js.Stringify(raw, "")
		if strings.Contains(rawText, b64("s3cret")) {
			t.Fatal("raw output leaked a secret")
		}
		if strings.Contains(rawText, b64("Bearer xyz")) {
			t.Fatal("raw output leaked the Authorization header")
		}
		found := false
		for _, x := range at(raw, "header").([]any) {
			if strings.HasPrefix(js.Str(x), "name=Content-Type,value=") {
				found = true
			}
		}
		if !found {
			t.Fatal("Content-Type header missing")
		}

		match(t, env.errorText(t, "proxmox_get_notification_endpoint", `{"name":"nope"}`), `No notification target named "nope"`)
	})

	t.Run("creates endpoints of each type with the right parameters", func(t *testing.T) {
		setup()
		env.call(t, "proxmox_save_notification_endpoint", `{"type":"smtp","name":"smtp2","create":true,"server":"mail","from_address":"pve@x","port":587,"mode":"starttls","username":"u","password":"pw","mailto":["a@x","b@x"],"mailto_user":["root@pam"]}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, "/cluster/notifications/endpoints/smtp")
		eq(t, w.Body, `{"name":"smtp2","mailto":["a@x","b@x"],"mailto-user":"root@pam","from-address":"pve@x","server":"mail","port":"587","mode":"starttls","username":"u","password":"pw"}`)

		env.call(t, "proxmox_save_notification_endpoint", `{"type":"gotify","name":"g2","create":true,"server":"https://g","token":"t"}`)
		eq(t, env.mock.LastWrite().Body, `{"name":"g2","server":"https://g","token":"t"}`)

		env.call(t, "proxmox_save_notification_endpoint", `{"type":"sendmail","name":"m2","create":true,"mailto_user":["root@pam"],"author":"PVE"}`)
		eq(t, env.mock.LastWrite().Body, `{"name":"m2","mailto-user":"root@pam","author":"PVE"}`)

		res := env.call(t, "proxmox_save_notification_endpoint", `{"type":"webhook","name":"hook","create":true,"url":"https://hooks/{{ secrets.path }}","method":"post","headers":{"Content-Type":"application/json"},"body":"{{ message }}","secrets":{"path":"s3cret"}}`)
		w = env.mock.LastWrite()
		eqStr(t, w.Path, "/cluster/notifications/endpoints/webhook")
		eqStr(t, w.Body.Get("header"), "name=Content-Type,value="+b64("application/json"))
		eqStr(t, w.Body.Get("body"), b64("{{ message }}"))
		eqStr(t, w.Body.Get("secret"), "name=path,value="+b64("s3cret"))
		eqAt(t, res, "created", "hook")
		if strings.Contains(js.Stringify(res, ""), "s3cret") {
			t.Fatal("leaked the secret")
		}
	})

	t.Run("updates endpoints and validates fields per type", func(t *testing.T) {
		setup()
		res := env.call(t, "proxmox_save_notification_endpoint", `{"type":"smtp","name":"smtp1","password":"new","delete":["author","comment"],"digest":"abc"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, "/cluster/notifications/endpoints/smtp/smtp1")
		eq(t, w.Body, `{"password":"new","delete":["author","comment"],"digest":"abc"}`)
		eqAt(t, res, "endpoint.password", "<redacted>")

		n := len(env.mock.Requests())
		match(t, env.errorText(t, "proxmox_save_notification_endpoint", `{"type":"gotify","name":"g","url":"x","mailto":["a"]}`), `Not valid for gotify endpoints: mailto, url`)
		match(t, env.errorText(t, "proxmox_save_notification_endpoint", `{"type":"webhook","name":"w","create":true,"url":"x"}`), `Required to create a webhook endpoint: method`)
		match(t, env.errorText(t, "proxmox_save_notification_endpoint", `{"type":"sendmail","name":"m","create":true}`), `mailto or mailto_user`)
		match(t, env.errorText(t, "proxmox_save_notification_endpoint", `{"type":"smtp","name":"s","create":true,"server":"x"}`), `from_address`)
		if len(env.mock.Requests()) != n {
			t.Fatal("validation errors must not reach the API")
		}
	})

	t.Run("deletes endpoints", func(t *testing.T) {
		setup()
		eq(t, env.call(t, "proxmox_delete_notification_endpoint", `{"name":"smtp1"}`), `{"deleted":"smtp1","type":"smtp"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "DELETE")
		eqStr(t, w.Path, "/cluster/notifications/endpoints/smtp/smtp1")
	})

	t.Run("sends a test notification", func(t *testing.T) {
		setup()
		eq(t, env.call(t, "proxmox_test_notification_target", `{"name":"smtp1"}`), `{"tested":"smtp1","sent":true}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, "/cluster/notifications/targets/smtp1/test")

		env.mock.On("POST", `test$`, func(*Obj, []string, proxmoxtest.Request) any { return proxmoxtest.Error(500, "could not send mail") })
		match(t, env.errorText(t, "proxmox_test_notification_target", `{"name":"smtp1"}`), `could not send mail`)
	})

	t.Run("lists and gets matchers", func(t *testing.T) {
		setup()
		env.mock.Reply("GET", `^/cluster/notifications/matchers$`, `[
			{"name":"default-matcher","origin":"builtin","target":["mail-to-root"],"comment":"Route all to root"},
			{"name":"backups","match-field":["exact:type=vzdump"],"match-severity":["error"],"mode":"any","invert-match":1,"disable":1,"target":["gotify1"]}]`)
		env.mock.Reply("GET", `^/cluster/notifications/matchers/backups$`, `{"match-field":["exact:type=vzdump"],"target":["gotify1"],"digest":"dd"}`)
		list := env.call(t, "proxmox_list_notification_matchers", nil)
		eqAt(t, list, "0", `{"name":"backups","disabled":true,"mode":"any","invert_match":true,"match_severity":["error"],"match_field":["exact:type=vzdump"],"targets":["gotify1"]}`)
		eqAt(t, list, "1.mode", "all")
		eq(t, env.call(t, "proxmox_get_notification_matcher", `{"name":"backups"}`), `{"name":"backups","mode":"all","match_field":["exact:type=vzdump"],"targets":["gotify1"],"digest":"dd"}`)
	})

	t.Run("saves and deletes matchers", func(t *testing.T) {
		setup()
		env.mock.Reply("GET", `^/cluster/notifications/matchers/m1$`, `{"target":["smtp1"],"match-severity":["warning","error"]}`)
		res := env.call(t, "proxmox_save_notification_matcher", `{"name":"m1","create":true,"targets":["smtp1"],"match_severity":["warning","error"],"match_field":["regex:hostname=^pve1$"],"match_calendar":["mon..fri 9-17"],"mode":"all","invert_match":false}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Method, "POST")
		eqStr(t, w.Path, "/cluster/notifications/matchers")
		eq(t, w.Body, `{"name":"m1","target":"smtp1","match-severity":["warning","error"],"match-field":"regex:hostname=^pve1$","match-calendar":"mon..fri 9-17","mode":"all","invert-match":"0"}`)
		eqAt(t, res, "matcher.targets", `["smtp1"]`)

		env.call(t, "proxmox_save_notification_matcher", `{"name":"m1","disable":true,"delete":["match-calendar"]}`)
		w = env.mock.LastWrite()
		eqStr(t, w.Method, "PUT")
		eqStr(t, w.Path, "/cluster/notifications/matchers/m1")
		eq(t, w.Body, `{"disable":"1","delete":"match-calendar"}`)

		env.call(t, "proxmox_delete_notification_matcher", `{"name":"m1"}`)
		eqStr(t, env.mock.LastWrite().Method, "DELETE")
		eqStr(t, env.mock.LastWrite().Path, "/cluster/notifications/matchers/m1")
	})

	t.Run("lists matcher fields and known values", func(t *testing.T) {
		setup()
		env.mock.Reply("GET", `^/cluster/notifications/matcher-fields$`, `[{"name":"type"},{"name":"hostname"},{"name":"job-id"}]`)
		env.mock.Reply("GET", `^/cluster/notifications/matcher-field-values$`, `[
			{"field":"type","value":"vzdump"},{"field":"type","value":"replication"},
			{"field":"hostname","value":"pve1"},{"field":"job-id","value":"backup-1","comment":"nightly"}]`)
		res := env.call(t, "proxmox_get_notification_matcher_fields", nil)
		eqAt(t, res, "fields", `[{"field":"hostname","values":["pve1"]},{"field":"job-id","values":[{"value":"backup-1","comment":"nightly"}]},{"field":"type","values":["vzdump","replication"]}]`)
	})

	t.Run("redacts endpoint secrets", func(t *testing.T) {
		r := notificationsRedactEndpoint(js.O("name", "x", "token", "t", "password", "p", "header", []any{"name=X-Api-Key,value=" + b64("k"), "name=Accept,value=" + b64("*/*")}))
		eqAt(t, r, "token", "<redacted>")
		eqAt(t, r, "password", "<redacted>")
		eqAt(t, r, "header", `{"X-Api-Key":"<redacted>","Accept":"*/*"}`)
	})
}
