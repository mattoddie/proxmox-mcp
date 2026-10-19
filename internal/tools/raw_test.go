package tools

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
)

func TestRawTool(t *testing.T) {
	env := startEnv(t)

	t.Run("limits the raw tool by permission level", func(t *testing.T) {
		eqAt(t, env.call(t, "proxmox_api_request", `{"path":"/version"}`), "version", "9.0.10")
		res := env.call(t, "proxmox_api_request", `{"method":"POST","path":"/nodes/pve1/qemu/100/status/start","params":{"skiplock":true}}`)
		eq(t, res, nil)
		eq(t, env.mock.LastWrite().Body, `{"skiplock":"1"}`)
		match(t, env.errorText(t, "proxmox_api_request", `{"path":"/nodes/../x"}`), `must not contain`)

		readOnly := connect(t, env.client, toolOptions("read"))
		match(t, errorTextOf(t, callOn(t, readOnly, "proxmox_api_request", `{"method":"POST","path":"/nodes/pve1/qemu/100/status/start"}`)), `Write access is disabled`)

		noDelete := connect(t, env.client, toolOptions("write"))
		match(t, errorTextOf(t, callOn(t, noDelete, "proxmox_api_request", `{"method":"DELETE","path":"/nodes/pve1/qemu/100"}`)), `Delete access is disabled`)

		noExec := connect(t, env.client, toolOptions("delete"))
		match(t, errorTextOf(t, callOn(t, noExec, "proxmox_api_request", `{"method":"POST","path":"/nodes/pve1/qemu/100/agent/exec","params":{"command":"id"}}`)), `PROXMOX_ALLOW_EXEC`)
	})

	t.Run("blocks guest file reads (GET) and encoded path tricks without exec access", func(t *testing.T) {
		noExec := connect(t, env.client, toolOptions("delete"))
		for _, tc := range []struct{ path, method, want string }{
			{"/nodes/pve1/qemu/100/agent/file-read", "GET", `PROXMOX_ALLOW_EXEC`},
			{"/nodes/pve1/qemu/100/agent/exec-status", "GET", `PROXMOX_ALLOW_EXEC`},
			{"/nodes/pve1/qemu/100/agent/file%2Dread", "GET", `PROXMOX_ALLOW_EXEC`},
			{"/nodes/pve1/qemu/100/agent//exec", "POST", `PROXMOX_ALLOW_EXEC`},
			{"/nodes/pve1/qemu/100/agent/x/%2e%2e/file-read", "GET", `must not contain`},
			{"/nodes/pve1/qemu/100/agent/./file-read", "GET", `must not contain`},
			{`/nodes/pve1/qemu/100/agent\file-read`, "GET", `must not contain`},
			{"/nodes/pve1/qemu/100/agent%5cfile-read", "GET", `must not contain`},
			{"/nodes/pve1/qemu/100/agent%2f..%2ffile-read", "GET", `must not contain`},
			{"/nodes/pve1/qemu/100/status?x=/agent", "GET", `must not contain`},
			{"/nodes/pve1/qemu/100/status#/agent", "GET", `must not contain`},
			{"/nodes/%zz", "GET", `percent-encoded`},
			{"/nodes/%C3", "GET", `percent-encoded`},
		} {
			args := js.O("method", tc.method, "path", tc.path, "params", js.O("file", "/etc/shadow"))
			match(t, errorTextOf(t, callOn(t, noExec, "proxmox_api_request", args)), tc.want, tc.path)
		}
	})

	t.Run("blocks control characters that URL parsing would strip, also in read-only mode", func(t *testing.T) {
		env.mock.Reset()
		for _, level := range []string{"read", "delete"} {
			s := connect(t, env.client, toolOptions(level))
			for _, path := range []string{
				"/nodes/pve1/qemu/100/agent/file-re\tad",
				"/nodes/pve1/qemu/100/agent/file-re\nad",
				"/nodes/pve1/qemu/100/agent/file-re\r\nad",
				"/nodes/pve1/qemu/100/agent/file-re%09ad",
				"/nodes/pve1/qemu/100/x/.\t./agent/file-read",
				"/nodes/pve1/qemu/100/agent/file-read%00",
				"/nodes/pve1/status\x7f",
			} {
				args := js.O("path", path, "params", js.O("file", "/etc/shadow"))
				match(t, errorTextOf(t, callOn(t, s, "proxmox_api_request", args)), `control characters`, level+" "+path)
			}
		}
		if n := len(env.mock.Requests()); n != 0 {
			t.Fatalf("%d requests reached the API", n)
		}
	})

	t.Run("blocks console, sendkey and every guest agent endpoint without exec access", func(t *testing.T) {
		env.mock.Reset()
		noExec := connect(t, env.client, toolOptions("delete"))
		for _, path := range []string{
			"/nodes/pve1/qemu/100/sendkey",
			"/nodes/pve1/qemu/100/vncwebsocket",
			"/nodes/pve1/qemu/100/vncproxy",
			"/nodes/pve1/qemu/100/termproxy",
			"/nodes/pve1/qemu/100/spiceproxy",
			"/nodes/pve1/qemu/100/monitor",
			"/nodes/pve1/qemu/100/mtunnel",
			"/nodes/pve1/qemu/100/mtunnelwebsocket",
			"/nodes/pve1/lxc/200/termproxy",
			"/nodes/pve1/lxc/200/vncproxy",
			"/nodes/pve1/lxc/200/spiceproxy",
			"/nodes/pve1/lxc/200/mtunnel",
			"/nodes/pve1/termproxy",
			"/nodes/pve1/vncshell",
			"/nodes/pve1/spiceshell",
			"/nodes/pve1/vncwebsocket",
			"/nodes/pve1/execute",
			"/nodes/pve1/qemu/100/agent",
			"/nodes/pve1/qemu/100/agent/get-osinfo",
			"/nodes/pve1/qemu/100/agent/fsfreeze-freeze",
			"/nodes/pve1/qemu/100/agent/set-user-password",
			"/nodes/pve1/qemu/100/Agent/File-Read",
			"/nodes/pve1/qemu/100/SENDKEY",
			"/nodes/pve1/qemu/100/%61gent/exec",
			"/api2/json/nodes/pve1/qemu/100/sendkey",
		} {
			match(t, errorTextOf(t, callOn(t, noExec, "proxmox_api_request", js.O("method", "POST", "path", path))), `PROXMOX_ALLOW_EXEC`, path)
		}
		if n := len(env.mock.Requests()); n != 0 {
			t.Fatalf("%d requests reached the API", n)
		}
	})

	t.Run("needs delete access for requests that destroy data without DELETE", func(t *testing.T) {
		cases := []struct {
			method, path string
			params       *Obj
		}{
			{"PUT", "/nodes/pve1/disks/wipedisk", js.O("disk", "/dev/sdb")},
			{"POST", "/nodes/pve1/disks/initgpt", js.O("disk", "/dev/sdb")},
			{"POST", "/nodes/pve1//disks/INITGPT/", js.O("disk", "/dev/sdb")},
			{"POST", "/nodes/pve1/disks/%77ipedisk", js.O("disk", "/dev/sdb")},
			{"POST", "/nodes/pve1/qemu", js.O("vmid", 100, "archive", "local:backup/vzdump-qemu-100.vma.zst", "force", 1)},
			{"POST", "/nodes/pve1/qemu", js.O("vmid", 100, "archive", "x", "force", "true")},
			{"POST", "/nodes/pve1/qemu", js.O("vmid", 100, "archive", "x", "force", true)},
			{"POST", "/nodes/pve1/lxc", js.O("vmid", 200, "ostemplate", "x", "restore", 1, "force", "yes")},
			{"POST", "/nodes/pve1/lxc", js.O("vmid", 200, "ostemplate", "x", "restore", 1, "force", []any{"0", "1"})},
			{"PUT", "/nodes/pve1/qemu/100/config", js.O("delete", "unused0")},
			{"POST", "/nodes/pve1/qemu/100/config", js.O("delete", "net1,unused3")},
			{"PUT", "/nodes/pve1/qemu/100/config", js.O("delete", "net1\vunused0")},
			{"PUT", "/nodes/pve1/qemu/100/config", js.O("delete", []any{"net1", "unused0"})},
			{"PUT", "/nodes/pve1/lxc/200/config", js.O("delete", "mp0;unused1")},
			{"PUT", "/nodes/pve1/qemu/100/unlink", js.O("idlist", "scsi1", "force", 1)},
			{"POST", "/nodes/pve1/qemu/100/move_disk", js.O("disk", "scsi0", "storage", "b", "delete", 1)},
			{"POST", "/nodes/pve1/lxc/200/move_volume", js.O("volume", "rootfs", "storage", "b", "delete", "on")},
			{"POST", "/nodes/pve1/qemu/100/remote_migrate", js.O("target-endpoint", "host=192.0.2.9,apitoken=x", "target-bridge", "vmbr0", "target-storage", "local")},
			{"POST", "/nodes/pve1/lxc/200/remote_migrate", js.O("target-endpoint", "host=192.0.2.9")},
			{"POST", "/nodes/pve1/qemu/100/status/stop", js.O("purge", 1)},
			{"POST", "/cluster/anything", js.O("destroy-unreferenced-disks", 1)},
		}
		noDelete := connect(t, env.client, toolOptions("write"))
		env.mock.Reset()
		for _, tc := range cases {
			args := js.O("method", tc.method, "path", tc.path, "params", tc.params)
			match(t, errorTextOf(t, callOn(t, noDelete, "proxmox_api_request", args)), `needs delete access\. Set PROXMOX_ALLOW_DELETES=true`, tc.method+" "+tc.path)
		}
		if n := len(env.mock.Requests()); n != 0 {
			t.Fatalf("%d requests reached the API", n)
		}
		// Allowed with deletes on.
		withDelete := connect(t, env.client, toolOptions("delete"))
		for _, tc := range cases {
			args := js.O("method", tc.method, "path", tc.path, "params", tc.params)
			// (The client refuses the \v case outright: no parameter may carry control characters.)
			if res := callOn(t, withDelete, "proxmox_api_request", args); res.IsError && !strings.Contains(resultText(res), "contains a control character") {
				t.Fatalf("%s %s with deletes: %s", tc.method, tc.path, resultText(res))
			}
		}
		// The safe variants stay plain writes.
		env.mock.Reset()
		for _, tc := range []struct {
			method, path string
			params       *Obj
		}{
			{"POST", "/nodes/pve1/qemu", js.O("vmid", 101, "archive", "x")},
			{"POST", "/nodes/pve1/qemu", js.O("vmid", 101, "archive", "x", "force", 0)},
			{"POST", "/nodes/pve1/lxc", js.O("vmid", 201, "ostemplate", "x", "restore", 1, "force", "false")},
			{"PUT", "/nodes/pve1/qemu/100/config", js.O("delete", "net1,scsi1")},
			{"PUT", "/nodes/pve1/qemu/100/config", js.O("delete", "unusedx")},
			{"PUT", "/nodes/pve1/qemu/100/unlink", js.O("idlist", "scsi1")},
			{"POST", "/nodes/pve1/qemu/100/move_disk", js.O("disk", "scsi0", "storage", "b", "delete", 0)},
			{"POST", "/nodes/pve1/qemu/100/migrate", js.O("target", "pve2")},
		} {
			args := js.O("method", tc.method, "path", tc.path, "params", tc.params)
			if res := callOn(t, noDelete, "proxmox_api_request", args); res.IsError {
				t.Fatalf("%s %s: %s", tc.method, tc.path, resultText(res))
			}
		}
		if n := len(env.mock.Requests()); n != 8 {
			t.Fatalf("%d requests reached the API, want 8", n)
		}
	})

	t.Run("needs write access for GETs that make the node connect out", func(t *testing.T) {
		readOnly := connect(t, env.client, toolOptions("read"))
		env.mock.Reset()
		for _, tc := range []struct {
			path   string
			params *Obj
		}{
			{"/nodes/pve1/scan/nfs", js.O("server", "192.0.2.10")},
			{"/nodes/pve1/scan/cifs", js.O("server", "192.0.2.10", "username", "u", "password", "p")},
			{"/nodes/pve1/scan/pbs", js.O("server", "192.0.2.10", "username", "u@pbs", "password", "p")},
			{"/nodes/pve1/scan/iscsi", js.O("portal", "192.0.2.10")},
			{"/nodes/pve1/scan/glusterfs", js.O("server", "192.0.2.10")},
			{"/nodes/pve1/scan/NFS/", js.O("server", "192.0.2.10")},
			{"/nodes/pve1/query-url-metadata", js.O("url", "http://169.254.169.254/latest/meta-data/")},
			{"/cluster/acme/meta", js.O("directory", "http://192.0.2.10/")},
			{"/cluster/acme/tos", js.O("directory", "http://192.0.2.10/")},
		} {
			args := js.O("path", tc.path, "params", tc.params)
			match(t, errorTextOf(t, callOn(t, readOnly, "proxmox_api_request", args)), `needs write access\. Set PROXMOX_ALLOW_WRITES=true`, tc.path)
		}
		if n := len(env.mock.Requests()); n != 0 {
			t.Fatalf("%d requests reached the API", n)
		}
		// Local scans and the default ACME directory stay reads.
		for _, path := range []string{"/nodes/pve1/scan/zfs", "/nodes/pve1/scan/lvm", "/cluster/acme/meta"} {
			noMatch(t, resultText(callOn(t, readOnly, "proxmox_api_request", js.O("path", path))), `needs write access`, path)
		}
		// With writes on they go through.
		write := connect(t, env.client, toolOptions("write"))
		env.mock.Reply("GET", `^/nodes/pve1/scan/nfs$`, `[]`)
		eq(t, parseResult(t, callOn(t, write, "proxmox_api_request", js.O("path", "/nodes/pve1/scan/nfs", "params", js.O("server", "nas")))), `[]`)
	})

	t.Run("needs exec access for backup file downloads and hook scripts", func(t *testing.T) {
		noExec := connect(t, env.client, toolOptions("delete"))
		env.mock.Reset()
		for _, tc := range []struct {
			method, path string
			params       *Obj
		}{
			{"GET", "/nodes/pve1/storage/pbs/file-restore/download", js.O("volume", "pbs:backup/vm/100/2026-01-01T00:00:00Z", "filepath", "L2V0Yy9zaGFkb3c=")},
			{"GET", "/nodes/pve1/storage/pbs/File-Restore//Download", js.O("volume", "x", "filepath", "x")},
			{"PUT", "/nodes/pve1/qemu/100/config", js.O("hookscript", "local:snippets/hook.sh")},
			{"POST", "/nodes/pve1/lxc", js.O("vmid", 300, "ostemplate", "x", "hookscript", "local:snippets/hook.sh")},
			{"POST", "/nodes/pve1/vzdump", js.O("vmid", 100, "script", "/root/hook.sh")},
			{"PUT", "/cluster/backup/job1", js.O("script", "/root/hook.sh")},
		} {
			args := js.O("method", tc.method, "path", tc.path, "params", tc.params)
			match(t, errorTextOf(t, callOn(t, noExec, "proxmox_api_request", args)), `PROXMOX_ALLOW_EXEC`, tc.method+" "+tc.path)
		}
		if n := len(env.mock.Requests()); n != 0 {
			t.Fatalf("%d requests reached the API", n)
		}
		// A backup's file list is still a read.
		env.mock.Reply("GET", `/file-restore/list$`, `[]`)
		eq(t, parseResult(t, callOn(t, connect(t, env.client, toolOptions("read")), "proxmox_api_request", js.O("path", "/nodes/pve1/storage/pbs/file-restore/list", "params", js.O("volume", "x", "filepath", "/")))), `[]`)
	})

	t.Run("checks the normalised path that is actually sent", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `^/nodes/pve1/status$`, `{"ok":true}`)
		eqAt(t, env.call(t, "proxmox_api_request", `{"path":"/api2/json/nodes/pve1/status"}`), "ok", true)
		eqStr(t, env.mock.Last().Path, "/nodes/pve1/status")
		match(t, env.errorText(t, "proxmox_api_request", `{"path":"nodes/pve1/status"}`), `must be absolute`)
		// Only a leading /api2/json/ is stripped; anything else stays under /api2/json.
		env.mock.Reply("GET", `^/api2/jsonx$`, `{"odd":true}`)
		eqAt(t, env.call(t, "proxmox_api_request", `{"path":"/api2/jsonx"}`), "odd", true)
	})

	t.Run("redacts ACME DNS plugin credentials", func(t *testing.T) {
		data := base64.StdEncoding.EncodeToString([]byte("CF_Token=not-a-real-token\nCF_Account_ID=abc123\n"))
		env.mock.Reply("GET", `^/cluster/acme/plugins$`, js.Array([]*Obj{
			js.O("plugin", "cf", "type", "dns", "api", "cf", "data", data, "digest", "d1"),
			js.O("plugin", "standalone", "type", "standalone"),
		}))
		env.mock.Reply("GET", `^/cluster/acme/plugins/cf$`, js.O("data", data, "digest", "d1"))
		list := env.call(t, "proxmox_api_request", `{"path":"/cluster/acme/plugins"}`)
		eqAt(t, list, "0.data", "CF_Token=<redacted>\nCF_Account_ID=<redacted>")
		eqAt(t, list, "0.api", "cf")
		eqAt(t, list, "1", `{"plugin":"standalone","type":"standalone"}`)
		// A single plugin's config doesn't repeat type/api, so the path decides.
		one := env.call(t, "proxmox_api_request", `{"path":"/cluster/acme/plugins/cf"}`)
		eqAt(t, one, "data", "CF_Token=<redacted>\nCF_Account_ID=<redacted>")
		if strings.Contains(js.Stringify([]any{list, one}, ""), data) {
			t.Fatal("plugin data leaked")
		}
		// Unrelated `data` fields are left alone.
		env.mock.Reply("GET", `^/nodes/pve1/rrddata$`, `[{"data":"x=1"}]`)
		eq(t, env.call(t, "proxmox_api_request", `{"path":"/nodes/pve1/rrddata"}`), `[{"data":"x=1"}]`)
		// redact=false returns it as is.
		eqAt(t, env.call(t, "proxmox_api_request", `{"path":"/cluster/acme/plugins/cf","redact":false}`), "data", data)
	})

	t.Run("redacts secrets by default", func(t *testing.T) {
		env.mock.Reply("GET", `^/access/users$`, `[{"userid":"a@pve","password":"x","keys":"y"}]`)
		eq(t, env.call(t, "proxmox_api_request", `{"path":"/access/users"}`), `[{"userid":"a@pve","password":"<redacted>","keys":"y"}]`)
	})

	t.Run("allows guest file reads with exec access", func(t *testing.T) {
		env.mock.Reply("GET", `/agent/file-read$`, `{"content":"x","truncated":0}`)
		res := env.call(t, "proxmox_api_request", `{"path":"/nodes/pve1/qemu/100/agent/file-read","params":{"file":"/etc/hostname"}}`)
		eqAt(t, res, "content", "x")
		eq(t, env.mock.Last().Query, `{"file":"/etc/hostname"}`)
	})
}
