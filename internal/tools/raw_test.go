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
