package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

// Regression tests for arguments that delete data, leak secrets or make the node connect out,
// beyond what the tool's own access level suggests.

func TestSecurityDeleteGates(t *testing.T) {
	env := startEnv(t)
	noDelete := connect(t, env.client, toolOptions("write"))

	t.Run("raw tool refuses VM config updates with force without delete access", func(t *testing.T) {
		env.mock.Reset()
		for _, method := range []string{"POST", "PUT"} {
			args := js.O("method", method, "path", "/nodes/pve1/qemu/100/config", "params", js.O("delete", "scsi1", "force", 1))
			match(t, errorTextOf(t, callOn(t, noDelete, "proxmox_api_request", args)), `removes disks with force.*PROXMOX_ALLOW_DELETES`, method)
		}
		if n := len(env.mock.Writes()); n != 0 {
			t.Fatalf("%d writes reached the API", n)
		}
		// Without force the disk is only detached, which a write may do.
		callOn(t, noDelete, "proxmox_api_request", `{"method":"PUT","path":"/nodes/pve1/qemu/100/config","params":{"delete":"scsi1"}}`)
		eqStr(t, env.mock.LastWrite().Path, "/nodes/pve1/qemu/100/config")
		// With delete access it goes through.
		env.call(t, "proxmox_api_request", `{"method":"PUT","path":"/nodes/pve1/qemu/100/config","params":{"delete":"scsi1","force":1}}`)
		eqStr(t, at(env.mock.LastWrite().Body, "force"), "1")
	})

	t.Run("move tools refuse delete_source without delete access", func(t *testing.T) {
		env.mock.Reset()
		match(t, errorTextOf(t, callOn(t, noDelete, "proxmox_move_vm_disk", `{"vmid":100,"disk":"scsi0","storage":"ceph","delete_source":true}`)), `PROXMOX_ALLOW_DELETES`)
		match(t, errorTextOf(t, callOn(t, noDelete, "proxmox_move_container_volume", `{"vmid":200,"volume":"rootfs","storage":"ceph","delete_source":true}`)), `PROXMOX_ALLOW_DELETES`)
		if n := len(env.mock.Writes()); n != 0 {
			t.Fatalf("%d writes reached the API", n)
		}
		callOn(t, noDelete, "proxmox_move_vm_disk", `{"vmid":100,"disk":"scsi0","storage":"ceph","delete_source":false,"wait":false}`)
		eqStr(t, env.mock.LastWrite().Path, "/nodes/pve1/qemu/100/move_disk")
		env.call(t, "proxmox_move_container_volume", `{"vmid":200,"volume":"rootfs","storage":"ceph","delete_source":true,"wait":false}`)
		eqStr(t, at(env.mock.LastWrite().Body, "delete"), "1")
	})

	t.Run("backups don't prune without delete access", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("POST", `^/nodes/([^/]+)/vzdump$`, func(_ *Obj, m []string, _ proxmoxtest.Request) any { return env.mock.Upid(m[1], "vzdump", "") })
		for _, args := range []string{
			`{"vmids":[100],"retention":{"keep_last":1}}`,
			`{"vmids":[100],"remove_old":true}`,
		} {
			match(t, errorTextOf(t, callOn(t, noDelete, "proxmox_backup_guests", args)), `PROXMOX_ALLOW_DELETES`, args)
		}
		if n := len(env.mock.Writes()); n != 0 {
			t.Fatalf("%d writes reached the API", n)
		}
		callOn(t, noDelete, "proxmox_backup_guests", `{"vmids":[100]}`)
		eqStr(t, at(env.mock.LastWrite().Body, "remove"), "0")
		// With delete access Proxmox's default (prune) applies.
		env.call(t, "proxmox_backup_guests", `{"vmids":[100]}`)
		eqAt(t, env.mock.LastWrite().Body, "remove", js.Undef)
	})

	t.Run("backup jobs don't prune without delete access", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/backup$`, `[{"id":"nightly","storage":"local","schedule":"daily","vmid":"100"}]`)
		for _, args := range []string{
			`{"id":"nightly","retention":{"keep_last":1}}`,
			`{"id":"nightly","remove_old":true}`,
			`{"id":"nightly","delete":["remove"]}`,
			`{"id":"new","vmids":[100],"remove_old":true}`,
		} {
			match(t, errorTextOf(t, callOn(t, noDelete, "proxmox_save_backup_job", args)), `PROXMOX_ALLOW_DELETES`, args)
		}
		if n := len(env.mock.Writes()); n != 0 {
			t.Fatalf("%d writes reached the API", n)
		}
		// New jobs are created with remove=0; updates leave it alone.
		callOn(t, noDelete, "proxmox_save_backup_job", `{"id":"new","vmids":[100],"schedule":"daily"}`)
		eqStr(t, env.mock.LastWrite().Method, "POST")
		eqStr(t, at(env.mock.LastWrite().Body, "remove"), "0")
		callOn(t, noDelete, "proxmox_save_backup_job", `{"id":"nightly","schedule":"weekly"}`)
		eqStr(t, env.mock.LastWrite().Method, "PUT")
		eqAt(t, env.mock.LastWrite().Body, "remove", js.Undef)

		// Running a job overrides its retention.
		env.mock.Reply("GET", `^/cluster/backup/nightly$`, `{"id":"nightly","vmid":"100","storage":"local","prune-backups":"keep-last=1","remove":1}`)
		env.mock.On("POST", `^/nodes/([^/]+)/vzdump$`, func(_ *Obj, m []string, _ proxmoxtest.Request) any { return env.mock.Upid(m[1], "vzdump", "") })
		callOn(t, noDelete, "proxmox_run_backup_job", `{"id":"nightly"}`)
		body := env.mock.LastWrite().Body
		eqStr(t, at(body, "remove"), "0")
		eqAt(t, body, "prune-backups", js.Undef)
		env.call(t, "proxmox_run_backup_job", `{"id":"nightly"}`)
		eqStr(t, at(env.mock.LastWrite().Body, "prune-backups"), "keep-last=1")
	})

	t.Run("raw tool refuses pruning backups without delete access", func(t *testing.T) {
		env.mock.Reset()
		for _, tc := range []struct{ method, path, params string }{
			{"POST", "/nodes/pve1/vzdump", `{"vmid":"100"}`},
			{"POST", "/nodes/pve1/vzdump", `{"vmid":"100","remove":1}`},
			{"POST", "/nodes/pve1/vzdump", `{"vmid":"100","remove":0,"prune-backups":"keep-last=1"}`},
			{"POST", "/cluster/backup", `{"vmid":"100","schedule":"daily"}`},
			{"PUT", "/cluster/backup/nightly", `{"remove":1}`},
			{"PUT", "/cluster/backup/nightly", `{"delete":"remove"}`},
		} {
			args := js.O("method", tc.method, "path", tc.path, "params", js.MustParse(tc.params))
			match(t, errorTextOf(t, callOn(t, noDelete, "proxmox_api_request", args)), `PROXMOX_ALLOW_DELETES`, tc.path, tc.params)
		}
		if n := len(env.mock.Writes()); n != 0 {
			t.Fatalf("%d writes reached the API", n)
		}
		callOn(t, noDelete, "proxmox_api_request", `{"method":"POST","path":"/nodes/pve1/vzdump","params":{"vmid":"100","remove":0}}`)
		eqStr(t, env.mock.LastWrite().Path, "/nodes/pve1/vzdump")
		callOn(t, noDelete, "proxmox_api_request", `{"method":"PUT","path":"/cluster/backup/nightly","params":{"schedule":"weekly"}}`)
		eqStr(t, env.mock.LastWrite().Path, "/cluster/backup/nightly")
	})

	t.Run("realm sync removing vanished entries or ACLs needs delete access", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("POST", `^/access/domains/corp/sync$`, env.mock.Upid("pve1", "auth-realm-sync", "corp"))
		env.mock.Reply("GET", `^/access/domains/corp$`, `{"type":"ldap","sync-defaults-options":"scope=users,remove-vanished=acl;properties"}`)
		for _, args := range []string{
			`{"realm":"corp","remove_vanished":"entry"}`,
			`{"realm":"corp","remove_vanished":"properties;acl"}`,
			`{"realm":"corp"}`, // from the realm's defaults
		} {
			match(t, errorTextOf(t, callOn(t, noDelete, "proxmox_sync_realm", args)), `PROXMOX_ALLOW_DELETES`, args)
		}
		match(t, errorTextOf(t, callOn(t, noDelete, "proxmox_save_realm_sync_job", `{"id":"j","create":true,"realm":"corp","schedule":"daily","remove_vanished":"entry"}`)), `PROXMOX_ALLOW_DELETES`)
		for _, args := range []string{
			`{"method":"POST","path":"/access/domains/corp/sync","params":{"remove-vanished":"acl"}}`,
			`{"method":"PUT","path":"/cluster/jobs/realm-sync/j","params":{"remove-vanished":"entry;properties"}}`,
		} {
			match(t, errorTextOf(t, callOn(t, noDelete, "proxmox_api_request", args)), `PROXMOX_ALLOW_DELETES`, args)
		}
		if n := len(env.mock.Writes()); n != 0 {
			t.Fatalf("%d writes reached the API", n)
		}
		// A dry run, none or properties only are fine.
		callOn(t, noDelete, "proxmox_sync_realm", `{"realm":"corp","dry_run":true}`)
		callOn(t, noDelete, "proxmox_sync_realm", `{"realm":"corp","remove_vanished":"properties"}`)
		eq(t, env.mock.LastWrite().Body, `{"remove-vanished":"properties"}`)
		callOn(t, noDelete, "proxmox_api_request", `{"method":"POST","path":"/access/domains/corp/sync","params":{"remove-vanished":"entry","dry-run":1}}`)
		eqAt(t, env.call(t, "proxmox_sync_realm", `{"realm":"corp","remove_vanished":"entry"}`), "status", "ok")
	})
}

func TestSecurityRedaction(t *testing.T) {
	env := startEnv(t)
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	opts := toolOptions("all")
	opts.AuditLog = path
	s := connect(t, env.client, opts)

	const pem = "-----BEGIN PRIVATE KEY-----\nMIIfake\n-----END PRIVATE KEY-----"
	res := parseResult(t, callOn(t, s, "proxmox_upload_node_certificate", js.O("node", "pve1", "certificates", "CERT", "key", pem, "dry_run", true)))
	eqAt(t, res, "would_send.params.key", proxmox.Redacted)
	eqAt(t, res, "would_send.params.certificates", "CERT")

	res = parseResult(t, callOn(t, s, "proxmox_save_acme_plugin", `{"id":"cf","create":true,"api":"cf","data":{"CF_Token":"tok3n"},"dry_run":true}`))
	eqAt(t, res, "would_send.params.data", proxmox.Redacted)

	env.mock.Reply("GET", `^/cluster/acme/plugins/cf$`, `{"plugin":"cf","type":"dns","api":"cf"}`)
	callOn(t, s, "proxmox_save_acme_plugin", `{"id":"cf","create":true,"api":"cf","data":{"CF_Token":"tok3n"}}`)

	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"MIIfake", "tok3n"} {
		if strings.Contains(string(text), secret) {
			t.Errorf("the audit log contains %q", secret)
		}
	}
	entries := readAudit(t, path)
	eqAt(t, entries[0], "arguments.key", proxmox.Redacted)
	eqAt(t, entries[2], "arguments.data", `{"CF_Token":"<redacted>"}`)

	// Underscored argument names match like Proxmox's hyphenated ones.
	for _, k := range []string{"client_key", "encryption_key", "client-key", "subscription_key"} {
		if !proxmox.IsSecretKey(k) {
			t.Errorf("%s is not treated as a secret", k)
		}
	}
	eqAt(t, proxmox.RedactSecrets(js.O("client_key", "abc")), "client_key", proxmox.Redacted)
}

func TestSecurityOutboundRequests(t *testing.T) {
	env := startEnv(t)
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	opts := toolOptions("write")
	opts.AuditLog = path
	s := connect(t, env.client, opts)
	env.mock.On("GET", `^/nodes/pve1/scan/(\w+)$`, func(_ *Obj, m []string, _ proxmoxtest.Request) any { return []any{js.O("kind", m[1])} })

	t.Run("dry runs don't make the node connect out", func(t *testing.T) {
		for _, tc := range []struct{ tool, args, path string }{
			{"proxmox_query_url_metadata", `{"node":"pve1","url":"http://192.0.2.10/x.iso","dry_run":true}`, "/nodes/pve1/query-url-metadata"},
			{"proxmox_scan_storage", `{"node":"pve1","type":"nfs","server":"192.0.2.10","dry_run":true}`, "/nodes/pve1/scan/nfs"},
			{"proxmox_list_acme_directories", `{"directory":"http://192.0.2.10/","dry_run":true}`, "/cluster/acme/meta"},
			{"proxmox_api_request", `{"path":"/nodes/pve1/scan/nfs","params":{"server":"192.0.2.10"},"dry_run":true}`, "/nodes/pve1/scan/nfs"},
		} {
			env.mock.Reply("GET", `^/cluster/acme/directories$`, `[]`)
			before := len(env.mock.RequestsTo(tc.path, "GET"))
			res := parseResult(t, callOn(t, s, tc.tool, tc.args))
			eqAt(t, res, "would_send.method", "GET", tc.tool)
			eqAt(t, res, "would_send.path", tc.path, tc.tool)
			if n := len(env.mock.RequestsTo(tc.path, "GET")); n != before {
				t.Errorf("%s: the request was sent in a dry run", tc.tool)
			}
		}
		// Local scans have nothing to stop.
		res := parseResult(t, callOn(t, s, "proxmox_scan_storage", `{"node":"pve1","type":"zfs","dry_run":true}`))
		eqAt(t, res, "would_send", nil)
		eqAt(t, res, "result.count", 1)
	})

	t.Run("outbound requests are audited, local reads aren't", func(t *testing.T) {
		_ = os.Remove(path)
		callOn(t, s, "proxmox_scan_storage", `{"node":"pve1","type":"zfs"}`)
		callOn(t, s, "proxmox_api_request", `{"path":"/nodes/pve1/scan/zfs"}`)
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("local scans were audited: %v", readAudit(t, path))
		}
		callOn(t, s, "proxmox_scan_storage", `{"node":"pve1","type":"nfs","server":"192.0.2.10"}`)
		callOn(t, s, "proxmox_api_request", `{"path":"/nodes/pve1/scan/nfs","params":{"server":"192.0.2.10"}}`)
		entries := readAudit(t, path)
		if len(entries) != 2 {
			t.Fatalf("%d entries: %v", len(entries), entries)
		}
		for _, e := range entries {
			eqAt(t, e, "requests.0", `{"method":"GET","path":"/nodes/pve1/scan/nfs","status":200}`)
		}
	})
}
