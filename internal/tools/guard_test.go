package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/config"
	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

func TestDryRun(t *testing.T) {
	env := startEnv(t)

	t.Run("previews a config update as the keys that would change", func(t *testing.T) {
		env.mock.Reset()
		res := env.call(t, "proxmox_update_vm_config", `{"vmid":100,"memory_mb":8192,"cores":4,"delete":["ide2"],"extra":{"cipassword":"new"},"digest":"abc123","dry_run":true}`)
		if n := len(env.mock.Writes()); n != 0 {
			t.Fatalf("%d writes in a dry run", n)
		}
		eqAt(t, res, "dry_run", true)
		eqAt(t, res, "would_send", `{"method":"POST","path":"/nodes/pve1/qemu/100/config","params":{"memory":8192,"cores":4,"delete":"ide2","digest":"abc123","cipassword":"<redacted>"}}`)
		// cores is unchanged; the password is redacted on both sides.
		eqAt(t, res, "changes", `{"memory":{"from":"4096","to":8192},"cipassword":{"from":"<redacted>","to":"<redacted>"},"ide2":{"from":"local:iso/debian-12.iso,media=cdrom","to":null}}`)
		match(t, at(res, "note"), `^Nothing was changed`)
	})

	t.Run("says when an update would change nothing", func(t *testing.T) {
		env.mock.Reset()
		res := env.call(t, "proxmox_update_vm_config", `{"vmid":100,"memory_mb":4096,"dry_run":true}`)
		eqAt(t, res, "changes", `{}`)
		match(t, at(res, "note"), `^The request would not change any value`)
	})

	t.Run("reads the object a PUT would change", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/access/users/alice%40pve$`, `{"email":"a@example.com","comment":"old","enable":1}`)
		res := env.call(t, "proxmox_save_user", `{"userid":"alice@pve","comment":"new","enable":true,"dry_run":true}`)
		if n := len(env.mock.Writes()); n != 0 {
			t.Fatalf("%d writes in a dry run", n)
		}
		eqAt(t, res, "would_send.method", "PUT")
		eqAt(t, res, "changes", `{"comment":{"from":"old","to":"new"}}`)
	})

	t.Run("shows what a delete would remove", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/access/users/bob%40pve$`, `{"email":"bob@example.com","enable":1}`)
		res := env.call(t, "proxmox_delete_user", `{"userid":"bob@pve","dry_run":true}`)
		if n := len(env.mock.Writes()); n != 0 {
			t.Fatalf("%d writes in a dry run", n)
		}
		eqAt(t, res, "would_send", `{"method":"DELETE","path":"/access/users/bob%40pve"}`)
		eqAt(t, res, "current", `{"email":"bob@example.com","enable":1}`)
	})

	t.Run("shows the body of a create and stops before waiting for a task", func(t *testing.T) {
		env.mock.Reset()
		res := env.call(t, "proxmox_create_pool", `{"poolid":"lab","comment":"test","dry_run":true}`)
		eqAt(t, res, "would_send", `{"method":"POST","path":"/pools","params":{"poolid":"lab","comment":"test"}}`)
		eqAt(t, res, "changes", js.Undef)
		if n := len(env.mock.Writes()); n != 0 {
			t.Fatalf("%d writes in a dry run", n)
		}
	})

	t.Run("still fails on invalid arguments", func(t *testing.T) {
		env.mock.Reset()
		match(t, env.errorText(t, "proxmox_update_vm_config", `{"vmid":100,"dry_run":true}`), `Nothing to change`)
	})

	t.Run("sends GETs from the raw tool and stops writes", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/version$`, `{"version":"8.4"}`)
		eqStr(t, at(env.call(t, "proxmox_api_request", `{"path":"/version","dry_run":true}`), "result.version"), "8.4")
		res := env.call(t, "proxmox_api_request", `{"method":"POST","path":"/nodes/pve1/wakeonlan","dry_run":true}`)
		eqAt(t, res, "would_send.path", "/nodes/pve1/wakeonlan")
		if n := len(env.mock.Writes()); n != 0 {
			t.Fatalf("%d writes in a dry run", n)
		}
	})

	t.Run("sends the guest agent ping, which changes nothing", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("POST", `/agent/ping$`, `{}`)
		res := env.call(t, "proxmox_guest_exec", `{"vmid":100,"command":["uptime"],"dry_run":true}`)
		eqAt(t, res, "would_send.path", "/nodes/pve1/qemu/100/agent/exec")
		for _, w := range env.mock.Writes() {
			if !strings.HasSuffix(w.Path, "/agent/ping") {
				t.Fatalf("sent %s %s in a dry run", w.Method, w.Path)
			}
		}
	})

	t.Run("keeps realm sync's own dry run, which Proxmox runs", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("POST", `^/access/domains/corp/sync$`, func(*Obj, []string, proxmoxtest.Request) any {
			return env.mock.Upid("pve1", "auth-realm-sync-test", "corp")
		})
		res := env.call(t, "proxmox_sync_realm", `{"realm":"corp","dry_run":true}`)
		eqAt(t, res, "dry_run", true)
		eqAt(t, res, "status", "ok")
		eq(t, env.mock.RequestsTo("/access/domains/corp/sync", "POST")[0].Body, `{"dry-run":"1"}`)
	})

	t.Run("is only offered on tools that change things", func(t *testing.T) {
		_, registered, _ := listTools(t, config.AllTools())
		for name, info := range registered {
			has := hasProp(info.Props, "dry_run")
			want := info.Gate != Read && name != "proxmox_get_guest_exec_status" && name != "proxmox_guest_read_file"
			if name == "proxmox_api_request" || name == "proxmox_query_url_metadata" {
				want = true
			}
			if has != want {
				t.Errorf("%s (%s): dry_run %v", name, info.Gate, has)
			}
		}
	})
}

func readAudit(t *testing.T, path string) []*Obj {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []*Obj
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		out = append(out, js.MustObj(line))
	}
	return out
}

func TestAuditLog(t *testing.T) {
	env := startEnv(t)
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	opts := toolOptions("all")
	opts.AuditLog = path
	s := connect(t, env.client, opts)

	env.mock.Reply("GET", `^/version$`, `{"version":"8.4"}`)
	callOn(t, s, "proxmox_list_nodes", nil)
	callOn(t, s, "proxmox_api_request", `{"path":"/version"}`)
	if _, err := os.Stat(path); err == nil && len(readAudit(t, path)) > 0 {
		t.Fatal("reads were logged")
	}

	env.mock.Reply("GET", `^/access/users/carol%40pve$`, `{"enable":1}`)
	callOn(t, s, "proxmox_save_user", `{"userid":"carol@pve","create":true,"password":"s3cretPassw0rd"}`)
	callOn(t, s, "proxmox_update_vm_config", `{"vmid":100,"memory_mb":8192,"dry_run":true}`)
	env.mock.On("DELETE", `^/pools$`, func(*Obj, []string, proxmoxtest.Request) any { return proxmoxtest.Error(500, "pool not empty") })
	callOn(t, s, "proxmox_delete_pool", `{"poolid":"lab"}`)
	callOn(t, s, "proxmox_update_vm_config", `{"vmid":"x"}`)

	entries := readAudit(t, path)
	if len(entries) != 4 {
		t.Fatalf("%d entries: %v", len(entries), entries)
	}
	if text, _ := os.ReadFile(path); strings.Contains(string(text), "s3cretPassw0rd") {
		t.Fatal("the password was logged")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != config.AuditLogMode {
		t.Errorf("mode %v", info.Mode())
	}
	user, preview, failed, invalid := entries[0], entries[1], entries[2], entries[3]
	match(t, at(user, "time"), `^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)
	eqAt(t, user, "tool", "proxmox_save_user")
	eqAt(t, user, "dry_run", false)
	eqAt(t, user, "arguments.password", proxmox.Redacted)
	eqAt(t, user, "requests.0", `{"method":"POST","path":"/access/users","status":200}`)
	eqAt(t, user, "outcome", "ok")

	eqAt(t, preview, "dry_run", true)
	eqAt(t, preview, "requests", `[]`)
	eqAt(t, preview, "previewed", `{"method":"POST","path":"/nodes/pve1/qemu/100/config"}`)

	eqAt(t, failed, "outcome", "error")
	eqAt(t, failed, "requests.0.status", 500)
	truthy(t, at(failed, "error"))

	eqAt(t, invalid, "outcome", "error")
	match(t, at(invalid, "error"), `Input validation error`)
	eqAt(t, invalid, "arguments", `{"vmid":"x"}`)
}

func TestAuditLogStderr(t *testing.T) {
	var b strings.Builder
	l := newAuditLog(config.AuditLogStderr)
	l.stderr = &b
	l.write("{}\n")
	eqStr(t, b.String(), "{}\n")
	if newAuditLog("") != nil {
		t.Fatal("an empty target gave a log")
	}
}
