package tools

import (
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

const agentTestPath = "/nodes/pve1/qemu/100/agent"

func agentTestLastPath(t *testing.T, env *testEnv, path string) {
	t.Helper()
	eqStr(t, env.mock.LastWrite().Path, path)
}

func TestAgentTools(t *testing.T) {
	env := startEnv(t)

	t.Run("combines guest info and tolerates individual failures", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("POST", `/agent/ping$`, `{"result":{}}`).
			Reply("GET", `/agent/get-osinfo$`, `{"result":{"pretty-name":"Debian GNU/Linux 12 (bookworm)","id":"debian","version-id":"12","kernel-release":"6.1.0-18-amd64","machine":"x86_64"}}`).
			Reply("GET", `/agent/get-host-name$`, `{"result":{"host-name":"web01"}}`).
			Reply("GET", `/agent/get-timezone$`, `{"result":{"zone":"UTC","offset":0}}`).
			On("GET", `/agent/get-time$`, func(*Obj, []string, proxmoxtest.Request) any {
				return js.O("result", float64(time.Now().UnixMilli())*1e6)
			}).
			Reply("GET", `/agent/get-vcpus$`, `{"result":[{"logical-id":0,"online":true},{"logical-id":1,"online":false}]}`).
			On("GET", `/agent/info$`, func(*Obj, []string, proxmoxtest.Request) any { return proxmoxtest.Error(500, "command disabled") })
		r := env.call(t, "proxmox_get_guest_info", `{"vmid":100}`)
		eqAt(t, r, "node", "pve1")
		eqAt(t, r, "hostname", "web01")
		eqAt(t, r, "os.name", "Debian GNU/Linux 12 (bookworm)")
		eqAt(t, r, "os.kernel", "6.1.0-18-amd64")
		eqAt(t, r, "vcpus", `{"total":2,"online":1}`)
		eqAt(t, r, "timezone.zone", "UTC")
		if skew := js.Num(at(r, "clock_skew_s")); !(math.Abs(skew) < 5) {
			t.Fatalf("clock skew %v", skew)
		}
		match(t, at(r, "errors.info"), `command disabled`)
		eqAt(t, r, "agent_version", js.Undef)
	})

	t.Run("explains when the agent is not running or not configured", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("POST", `/agent/ping$`, func(*Obj, []string, proxmoxtest.Request) any {
			return proxmoxtest.Error(500, "QEMU guest agent is not running")
		})
		match(t, env.errorText(t, "proxmox_get_guest_info", `{"vmid":100}`), `not responding.*qemu-guest-agent service`)
		env.mock.On("GET", `/agent/get-fsinfo$`, func(*Obj, []string, proxmoxtest.Request) any {
			return proxmoxtest.Error(500, "No QEMU guest agent configured")
		})
		match(t, env.errorText(t, "proxmox_get_guest_filesystems", `{"vmid":100}`), `not enabled for VM 100.*agent=1`)
	})

	t.Run("rejects containers", func(t *testing.T) {
		env.mock.Reset()
		match(t, env.errorText(t, "proxmox_get_guest_network", `{"vmid":200}`), `is a container, not a VM`)
	})

	t.Run("summarises network interfaces and hides loopback", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `/agent/network-get-interfaces$`, `{"result":[
			{"name":"lo","hardware-address":"00:00:00:00:00:00","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"127.0.0.1","prefix":8}]},
			{"name":"eth0","hardware-address":"bc:24:11:00:00:01","ip-addresses":[
				{"ip-address-type":"ipv4","ip-address":"10.0.10.5","prefix":24},{"ip-address-type":"ipv6","ip-address":"fe80::1","prefix":64}],
			 "statistics":{"rx-bytes":1048576,"tx-bytes":2048,"rx-packets":10,"tx-packets":5,"rx-errs":0,"rx-dropped":3}}]}`)
		r := env.call(t, "proxmox_get_guest_network", `{"vmid":100}`)
		eqAt(t, r, "ipv4", `["10.0.10.5"]`)
		eqAt(t, r, "interfaces.length", 1)
		eqAt(t, r, "interfaces.0", `{"name":"eth0","mac":"bc:24:11:00:00:01","ipv4":["10.0.10.5/24"],"ipv6":["fe80::1/64"],"rx":"1 MiB","tx":"2 KiB",
			"rx_packets":10,"tx_packets":5,"rx_dropped":3}`)
		eqAt(t, env.call(t, "proxmox_get_guest_network", `{"vmid":100,"include_loopback":true}`), "interfaces.length", 2)
	})

	t.Run("summarises filesystems", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `/agent/get-fsinfo$`, `{"result":[
			{"name":"sda2","mountpoint":"/boot","type":"ext4","used-bytes":104857600,"total-bytes":1073741824,"disk":[{"dev":"/dev/sda2","serial":"drive-scsi0","bus-type":"scsi"}]},
			{"name":"sda1","mountpoint":"/","type":"ext4","used-bytes":8589934592,"total-bytes":34359738368,"disk":[]}]}`)
		r := env.call(t, "proxmox_get_guest_filesystems", `{"vmid":100}`)
		eq(t, pluck(at(r, "filesystems"), "mountpoint"), `["/","/boot"]`)
		eqAt(t, r, "filesystems.0.usage", "8 GiB of 32 GiB (25%)")
		eqAt(t, r, "filesystems.0.used_pct", 25)
		eqAt(t, r, "filesystems.1.disks", `[{"dev":"/dev/sda2","serial":"drive-scsi0","bus":"scsi"}]`)
	})

	t.Run("lists users and memory blocks", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("GET", `/agent/get-users$`, `{"result":[{"user":"matt","login-time":1700000000.5}]}`)
		u := env.call(t, "proxmox_get_guest_users", `{"vmid":100}`)
		eqAt(t, u, "users", `[{"user":"matt","login_time":"2023-11-14T22:13:20.500Z"}]`)

		env.mock.Reply("GET", `/agent/get-memory-blocks$`, `{"result":[{"phys-index":0,"online":true},{"phys-index":1,"online":true},{"phys-index":2,"online":false}]}`).
			Reply("GET", `/agent/get-memory-block-info$`, `{"result":{"size":134217728}}`)
		m := env.call(t, "proxmox_get_guest_memory_blocks", `{"vmid":100}`)
		eqAt(t, m, "blocks", 3)
		eqAt(t, m, "online_blocks", 2)
		eqAt(t, m, "block_size", "128 MiB")
		eqAt(t, m, "online_memory", "256 MiB")
	})

	t.Run("freezes, thaws and reports freeze state", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("POST", `/agent/fsfreeze-(freeze|thaw|status)$`, func(_ *Obj, m []string, _ proxmoxtest.Request) any {
			if m[1] == "status" {
				return js.O("result", "thawed")
			}
			return js.O("result", 2)
		})
		f := env.call(t, "proxmox_guest_fs_freeze", `{"vmid":100,"action":"freeze"}`)
		agentTestLastPath(t, env, agentTestPath+"/fsfreeze-freeze")
		eqAt(t, f, "filesystems", 2)
		eqAt(t, env.call(t, "proxmox_guest_fs_freeze", `{"vmid":100,"action":"status"}`), "status", "thawed")
	})

	t.Run("runs fstrim and agent power actions", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("POST", `/agent/fstrim$`, `{"result":{"paths":[{"path":"/","trimmed":1073741824},{"path":"/boot","error":"not supported"}]}}`)
		tr := env.call(t, "proxmox_guest_fstrim", `{"vmid":100}`)
		eqAt(t, tr, "paths", `[{"path":"/","trimmed":"1 GiB"},{"path":"/boot","error":"not supported"}]`)

		p := env.call(t, "proxmox_guest_power", `{"vmid":100,"action":"suspend-disk"}`)
		eqAt(t, p, "status", "requested")
		agentTestLastPath(t, env, agentTestPath+"/suspend-disk")
	})

	t.Run("runs a command and polls until it exits", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("POST", `/agent/exec$`, `{"pid":4242}`)
		var polls atomic.Int32
		env.mock.On("GET", `/agent/exec-status$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			if p.Get("pid") != "4242" {
				t.Errorf("pid=%s", js.Str(p.Get("pid")))
			}
			if polls.Add(1) < 2 {
				return js.O("exited", 0)
			}
			return js.MustParse(`{"exited":1,"exitcode":0,"out-data":"hello\n","err-data":"warn\n","out-truncated":1}`)
		})
		r := env.call(t, "proxmox_guest_exec", `{"vmid":100,"command":["/bin/sh","-c","echo hello"],"input_data":"stdin!"}`)
		w := env.mock.RequestsTo(agentTestPath+"/exec", "POST")[0]
		eqAt(t, w.Body, "command", `["/bin/sh","-c","echo hello"]`)
		eqAt(t, w.Body, "input-data", "stdin!")
		eqAt(t, r, "pid", 4242)
		eqAt(t, r, "exited", true)
		eqAt(t, r, "exitcode", 0)
		eqAt(t, r, "stdout", "hello\n")
		eqAt(t, r, "stderr", "warn\n")
		eqAt(t, r, "stdout_truncated", true)
		if n := polls.Load(); n != 2 {
			t.Fatalf("%d polls", n)
		}
	})

	t.Run("returns the pid when a command outlives the timeout, and checks it later", func(t *testing.T) {
		env.mock.Reset()
		env.mock.Reply("POST", `/agent/exec$`, `{"pid":7}`)
		env.mock.Reply("GET", `/agent/exec-status$`, `{"exited":0}`)
		r := env.call(t, "proxmox_guest_exec", `{"vmid":100,"command":["sleep","100"],"timeout_s":0}`)
		eqAt(t, r, "exited", false)
		match(t, at(r, "note"), `proxmox_get_guest_exec_status \(pid 7\)`)

		env.mock.Reply("GET", `/agent/exec-status$`, `{"exited":1,"exitcode":1,"signal":9}`)
		s := env.call(t, "proxmox_get_guest_exec_status", `{"vmid":100,"pid":7}`)
		eqAt(t, s, "exited", true)
		eqAt(t, s, "exitcode", 1)
		eqAt(t, s, "signal", 9)
	})

	t.Run("reads files as text or base64", func(t *testing.T) {
		env.mock.Reset()
		env.mock.On("GET", `/agent/file-read$`, func(p *Obj, _ []string, _ proxmoxtest.Request) any {
			if p.Get("decode") == "0" {
				return js.O("content", "aGk=")
			}
			return js.O("content", "PRETTY_NAME=Debian\n", "truncated", 1)
		})
		tx := env.call(t, "proxmox_guest_read_file", `{"vmid":100,"file":"/etc/os-release"}`)
		eqAt(t, tx, "content", "PRETTY_NAME=Debian\n")
		eqAt(t, tx, "truncated", true)
		eqAt(t, tx, "bytes", 19)
		reqs := env.mock.RequestsTo(agentTestPath+"/file-read", "")
		q := reqs[len(reqs)-1].Query
		eqAt(t, q, "file", "/etc/os-release")
		eqStr(t, at(q, "count"), "1048576")
		eqAt(t, q, "decode", js.Undef)

		b := env.call(t, "proxmox_guest_read_file", `{"vmid":100,"file":"/bin/x","encoding":"base64","offset":10,"count":2}`)
		eqAt(t, b, "content", "aGk=")
		eqAt(t, b, "bytes", 2)
		eqAt(t, b, "truncated", false)
		reqs = env.mock.RequestsTo(agentTestPath+"/file-read", "")
		q2 := reqs[len(reqs)-1].Query
		eq(t, []any{q2.Get("decode"), q2.Get("offset"), q2.Get("count")}, []any{"0", "10", "2"})
	})

	t.Run("writes files", func(t *testing.T) {
		env.mock.Reset()
		r := env.call(t, "proxmox_guest_write_file", `{"vmid":100,"file":"/tmp/a.txt","content":"hello"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Path, agentTestPath+"/file-write")
		eq(t, w.Body, `{"file":"/tmp/a.txt","content":"hello"}`)
		eqAt(t, r, "bytes", 5)

		env.call(t, "proxmox_guest_write_file", `{"vmid":100,"file":"/tmp/b","content":"aGk=","encoding":"base64"}`)
		eqStr(t, at(env.mock.LastWrite().Body, "encode"), "0")
	})

	t.Run("sets a user password without echoing it", func(t *testing.T) {
		env.mock.Reset()
		r := env.call(t, "proxmox_guest_set_password", `{"vmid":100,"username":"root","password":"s3cret!"}`)
		w := env.mock.LastWrite()
		eqStr(t, w.Path, agentTestPath+"/set-user-password")
		eq(t, w.Body, `{"username":"root","password":"s3cret!"}`)
		if strings.Contains(js.Stringify(r, ""), "s3cret!") {
			t.Fatal("password echoed")
		}
	})
}
