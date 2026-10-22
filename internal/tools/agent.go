package tools

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

// agentUnwrap: the agent endpoints wrap the QGA reply as {result: ...}; exec/file endpoints don't.
func agentUnwrap(res any) any {
	if o, ok := res.(*Obj); ok && o != nil && o.Has("result") {
		return o.Get("result")
	}
	return res
}

var (
	agentNotConfiguredRe = regexp.MustCompile(`(?i)No QEMU guest agent configured`)
	agentNotRunningRe    = regexp.MustCompile(`(?i)guest agent is not running`)
	agentVMNotRunningRe  = regexp.MustCompile(`(?i)VM \d+ not running`)
	agentTimeoutRe       = regexp.MustCompile(`(?i)got timeout|timeout`)
	agentLoopbackRe      = regexp.MustCompile(`(?i)loopback`)
)

// agentError turns Proxmox's terse guest agent failures into actionable messages.
func agentError(err error, vmid float64) error {
	var pe *proxmox.Error
	if !errors.As(err, &pe) {
		return err
	}
	msg := pe.Message
	id := js.NumStr(vmid)
	switch {
	case agentNotConfiguredRe.MatchString(msg):
		return &proxmox.Error{Message: "The QEMU guest agent is not enabled for VM " + id + ". Enable it in the VM options (agent=1 in the VM config), install and start qemu-guest-agent inside the guest, then fully stop and start the VM. (" + msg + ")", Status: pe.Status}
	case agentNotRunningRe.MatchString(msg):
		return &proxmox.Error{Message: "The QEMU guest agent in VM " + id + " is not responding. Check that the VM is running and that the qemu-guest-agent service is installed and running inside the guest. (" + msg + ")", Status: pe.Status}
	case agentVMNotRunningRe.MatchString(msg):
		return &proxmox.Error{Message: "VM " + id + " is not running; the guest agent only works in a running VM. (" + msg + ")", Status: pe.Status}
	case agentTimeoutRe.MatchString(msg):
		return &proxmox.Error{Message: "The guest agent in VM " + id + " did not answer in time; it may be busy or hung. (" + msg + ")", Status: pe.Status}
	}
	return pe
}

// agentAddresses turns ip-address entries into "addr/prefix".
func agentAddresses(iface *Obj, kind string) []any {
	out := []any{}
	for _, a := range js.Objs(or(iface.Get("ip-addresses"), []any{})) {
		if a.Get("ip-address-type") != kind {
			continue
		}
		if a.Defined("prefix") {
			out = append(out, js.Str(a.Get("ip-address"))+"/"+js.Str(a.Get("prefix")))
		} else {
			out = append(out, js.Str(a.Get("ip-address")))
		}
	}
	return out
}

func agentSummarizeInterface(i *Obj) *Obj {
	s := i.ObjAt("statistics")
	nz := func(v any) any {
		if n := js.Num(v); js.Truthy(n) {
			return n
		}
		return js.Undef
	}
	return proxmox.C(
		"name", i.Get("name"),
		"mac", i.Get("hardware-address"),
		"ipv4", agentAddresses(i, "ipv4"),
		"ipv6", agentAddresses(i, "ipv6"),
		"rx", proxmox.FormatBytes(s.Get("rx-bytes")),
		"tx", proxmox.FormatBytes(s.Get("tx-bytes")),
		"rx_packets", s.Get("rx-packets"),
		"tx_packets", s.Get("tx-packets"),
		"rx_errors", nz(s.Get("rx-errs")),
		"tx_errors", nz(s.Get("tx-errs")),
		"rx_dropped", nz(s.Get("rx-dropped")),
		"tx_dropped", nz(s.Get("tx-dropped")),
	)
}

func agentIsLoopback(i *Obj) bool {
	if i.Get("name") == "lo" || agentLoopbackRe.MatchString(js.Str(i.Get("name"))) {
		return true
	}
	for _, a := range agentAddresses(i, "ipv4") {
		if strings.HasPrefix(a.(string), "127.") {
			return true
		}
	}
	return false
}

func agentSummarizeFilesystem(f *Obj) *Obj {
	used, total := f.Get("used-bytes"), f.Get("total-bytes")
	return proxmox.C(
		"mountpoint", f.Get("mountpoint"),
		"type", f.Get("type"),
		"device", f.Get("name"),
		"usage", proxmox.Usage(used, total),
		"used_pct", proxmox.Percent(used, total),
		"disks", mapObjs(js.Objs(or(f.Get("disk"), []any{})), func(d *Obj) any {
			return proxmox.C("dev", d.Get("dev"), "serial", d.Get("serial"), "bus", d.Get("bus-type"), "target", d.Get("target"), "unit", d.Get("unit"))
		}),
	)
}

// agentObj treats a value as an object, like property access on a JavaScript value would.
func agentObj(v any) *Obj {
	if o, ok := v.(*Obj); ok && o != nil {
		return o
	}
	return js.NewObj()
}

func registerAgent(c *Context) {
	client := c.Client

	resolveVm := func(ctx context.Context, a *Obj) (proxmox.GuestRef, error) { return c.guest(ctx, a, "qemu") }

	// agent calls an agent sub-endpoint (e.g. "get-osinfo") and unwraps the result, with friendly errors.
	agent := func(ctx context.Context, g proxmox.GuestRef, method, command string, params *Obj, raw bool) (any, error) {
		if command == "ping" {
			// A POST that changes nothing: dry runs send it.
			ctx = proxmox.AsRead(ctx)
		}
		res, err := client.Request(ctx, method, client.GuestPath(g)+"/agent/"+command, params)
		if err != nil {
			return nil, agentError(err, g.VMID)
		}
		if raw {
			return res, nil
		}
		return agentUnwrap(res), nil
	}

	baseInput := func(more ...Prop) []Prop {
		return append([]Prop{P("vmid", vmidArg), P("node", guestNodeArg)}, more...)
	}

	// ---------------------------------------------------------------- read

	c.Define("proxmox_get_guest_info", Def{
		Toolset:     "agent",
		Title:       "Get guest OS info (agent)",
		Description: "Ask a running VM's QEMU guest agent about the guest: OS name/version/kernel, hostname, timezone, guest clock (and its skew from this server), vCPUs online and the agent version with its disabled commands. VMs only (containers need no agent). Individual queries that fail are reported under `errors` instead of failing the call. Use proxmox_get_guest_network for IP addresses.",
		Input:       baseInput(P("raw", rawArg)),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := resolveVm(ctx, a)
			if err != nil {
				return nil, err
			}
			// Ping first so a missing/stopped agent gives one clear error instead of seven.
			if _, err := agent(ctx, g, "POST", "ping", nil, false); err != nil {
				return nil, err
			}
			r := js.NewObj()
			errs := js.NewObj()
			for _, q := range []string{"get-osinfo", "get-host-name", "get-timezone", "get-time", "get-vcpus", "info"} {
				v, err := agent(ctx, g, "GET", q, nil, false)
				if err != nil {
					errs.Set(q, errMessage(err))
				} else {
					r.Set(q, v)
				}
			}
			if a.Truthy("raw") {
				return proxmox.Compact(js.Spread(js.O("vmid", g.VMID, "node", g.Node), r, js.O("errors", errs))), nil
			}
			os := agentObj(or(r.Get("get-osinfo"), js.NewObj()))
			tz := agentObj(or(r.Get("get-timezone"), js.NewObj()))
			timeNs := js.Num(r.Get("get-time"))
			info := agentObj(r.Get("info"))
			commands := js.Objs(or(info.Get("supported_commands"), []any{}))
			var guestTime, skew any = js.Undef, js.Undef
			if js.IsFinite(timeNs) && timeNs > 0 {
				guestTime = proxmox.ISO(timeNs / 1e6)
				skew = js.Round((timeNs/1e6-float64(time.Now().UnixMilli()))/100) / 10
			}
			var vcpus any = js.Undef
			if list, ok := r.Get("get-vcpus").([]any); ok {
				online := 0
				for _, v := range js.Objs(list) {
					if v.Truthy("online") {
						online++
					}
				}
				vcpus = js.O("total", len(list), "online", online)
			}
			disabled := []any{}
			for _, cmd := range commands {
				if cmd.Get("enabled") == false {
					disabled = append(disabled, cmd.Get("name"))
				}
			}
			return proxmox.C(
				"vmid", g.VMID,
				"name", g.Name,
				"node", g.Node,
				"agent", "running",
				"os", proxmox.C(
					"name", or(os.Get("pretty-name"), os.Get("name")),
					"id", os.Get("id"),
					"version", os.Get("version"),
					"version_id", os.Get("version-id"),
					"kernel", os.Get("kernel-release"),
					"kernel_version", os.Get("kernel-version"),
					"machine", os.Get("machine"),
					"variant", os.Get("variant"),
				),
				"hostname", agentObj(r.Get("get-host-name")).Get("host-name"),
				"timezone", proxmox.C("zone", tz.Get("zone"), "utc_offset_s", tz.Get("offset")),
				"guest_time", guestTime,
				"clock_skew_s", skew,
				"vcpus", vcpus,
				"agent_version", info.Get("version"),
				"disabled_commands", disabled,
				"errors", errs,
			), nil
		},
	})

	c.Define("proxmox_get_guest_network", Def{
		Toolset:     "agent",
		Title:       "Get guest network interfaces (agent)",
		Description: "List a running VM's network interfaces as seen inside the guest (via the QEMU guest agent): name, MAC, IPv4/IPv6 addresses with prefix, and traffic counters. The reliable way to find a VM's IP address. Loopback is hidden unless include_loopback=true. VMs only; for containers use proxmox_get_container_interfaces.",
		Input: baseInput(
			P("include_loopback", Bool().Opt().D("Include the loopback interface (default false)")),
			P("raw", rawArg),
		),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := resolveVm(ctx, a)
			if err != nil {
				return nil, err
			}
			res, err := agent(ctx, g, "GET", "network-get-interfaces", nil, false)
			if err != nil {
				return nil, err
			}
			res = or(res, []any{})
			if a.Truthy("raw") {
				return res, nil
			}
			ifaces := js.Objs(res)
			if !a.Truthy("include_loopback") {
				ifaces = filterObjs(ifaces, func(i *Obj) bool { return !agentIsLoopback(i) })
			}
			items := make([]*Obj, len(ifaces))
			ipv4 := []any{}
			for n, i := range ifaces {
				items[n] = agentSummarizeInterface(i)
				for _, x := range items[n].Arr("ipv4") {
					addr, _, _ := strings.Cut(js.Str(x), "/")
					ipv4 = append(ipv4, addr)
				}
			}
			return js.O("vmid", g.VMID, "node", g.Node, "ipv4", ipv4, "interfaces", js.Array(items)), nil
		},
	})

	c.Define("proxmox_get_guest_filesystems", Def{
		Toolset:     "agent",
		Title:       "Get guest filesystems (agent)",
		Description: "List a running VM's mounted filesystems from inside the guest (QEMU guest agent get-fsinfo): mountpoint, type, used/total space and backing disks. Use it to check real disk usage inside a VM (Proxmox's own disk figures for VMs are not filesystem usage). VMs only.",
		Input:       baseInput(P("raw", rawArg)),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := resolveVm(ctx, a)
			if err != nil {
				return nil, err
			}
			res, err := agent(ctx, g, "GET", "get-fsinfo", nil, false)
			if err != nil {
				return nil, err
			}
			res = or(res, []any{})
			if a.Truthy("raw") {
				return res, nil
			}
			fs := js.Objs(res)
			items := make([]*Obj, len(fs))
			for i, f := range fs {
				items[i] = agentSummarizeFilesystem(f)
			}
			sortObjs(items, func(x, y *Obj) float64 { return compareStr(x.Get("mountpoint"), y.Get("mountpoint")) })
			return js.O("vmid", g.VMID, "node", g.Node, "filesystems", js.Array(items)), nil
		},
	})

	c.Define("proxmox_get_guest_users", Def{
		Toolset:     "agent",
		Title:       "Get logged-in guest users (agent)",
		Description: "List the users currently logged in to a running VM (QEMU guest agent get-users), with domain and login time. VMs only.",
		Input:       baseInput(),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := resolveVm(ctx, a)
			if err != nil {
				return nil, err
			}
			res, err := agent(ctx, g, "GET", "get-users", nil, false)
			if err != nil {
				return nil, err
			}
			users := js.Objs(or(res, []any{}))
			return js.O("vmid", g.VMID, "node", g.Node, "users", mapObjs(users, func(u *Obj) any {
				return proxmox.C("user", u.Get("user"), "domain", u.Get("domain"), "login_time", proxmox.IsoTime(u.Get("login-time")))
			})), nil
		},
	})

	c.Define("proxmox_get_guest_memory_blocks", Def{
		Toolset:     "agent",
		Title:       "Get guest memory blocks (agent)",
		Description: "Show a running VM's memory blocks as the guest sees them (QEMU guest agent get-memory-blocks/get-memory-block-info): block size and how many blocks/how much memory is online. Useful after memory hotplug. VMs only; not all guest OSes support it.",
		Input:       baseInput(),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := resolveVm(ctx, a)
			if err != nil {
				return nil, err
			}
			blocks, err := agent(ctx, g, "GET", "get-memory-blocks", nil, false)
			if err != nil {
				return nil, err
			}
			infoV, err := agent(ctx, g, "GET", "get-memory-block-info", nil, false)
			if err != nil {
				infoV = js.Undef
			}
			info := agentObj(infoV)
			list := js.Objs(or(blocks, []any{}))
			online := len(filterObjs(list, func(b *Obj) bool { return b.Truthy("online") }))
			size := info.Num("size")
			return proxmox.C(
				"vmid", g.VMID,
				"node", g.Node,
				"block_size", proxmox.FormatBytes(info.Get("size")),
				"blocks", len(list),
				"online_blocks", online,
				"offline_blocks", len(list)-online,
				"online_memory", ifThen(js.IsFinite(size) && size > 0, proxmox.FormatBytes(float64(online)*size)),
			), nil
		},
	})

	// ---------------------------------------------------------------- write

	c.Define("proxmox_guest_fs_freeze", Def{
		Toolset:     "agent",
		Title:       "Freeze/thaw guest filesystems (agent)",
		Description: "Freeze, thaw or check the freeze state of a running VM's filesystems through the QEMU guest agent (fsfreeze). While frozen, writes inside the guest block, so always thaw promptly. Backups and snapshots already freeze/thaw automatically when the agent is enabled; use this for external snapshotting or to recover a guest left frozen (action=status, then thaw).",
		Write:       true,
		Destructive: True,
		Input:       baseInput(P("action", Enum("freeze", "thaw", "status").D("freeze: freeze all filesystems; thaw: unfreeze them; status: report 'frozen' or 'thawed'"))),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := resolveVm(ctx, a)
			if err != nil {
				return nil, err
			}
			res, err := agent(ctx, g, "POST", "fsfreeze-"+a.Str("action"), nil, false)
			if err != nil {
				return nil, err
			}
			if a.Get("action") == "status" {
				return js.O("vmid", g.VMID, "node", g.Node, "status", res), nil
			}
			_, isNum := res.(float64)
			return proxmox.C("vmid", g.VMID, "node", g.Node, "action", a.Get("action"), "filesystems", ifThen(isNum, res), "result", ifThen(!isNum, res)), nil
		},
	})

	c.Define("proxmox_guest_fstrim", Def{
		Toolset:     "agent",
		Title:       "Trim guest filesystems (agent)",
		Description: "Run fstrim inside a running VM via the QEMU guest agent so freed blocks are discarded and thin-provisioned storage can reclaim space. Needs discard enabled on the VM's disks to have any effect on the host. Can take a while on large disks.",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input:       baseInput(),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := resolveVm(ctx, a)
			if err != nil {
				return nil, err
			}
			res, err := agent(ctx, g, "POST", "fstrim", nil, false)
			if err != nil {
				return nil, err
			}
			paths := js.Objs(or(agentObj(or(res, js.NewObj())).Get("paths"), []any{}))
			return js.O("vmid", g.VMID, "node", g.Node, "paths", mapObjs(paths, func(p *Obj) any {
				return proxmox.C("path", p.Get("path"), "trimmed", proxmox.FormatBytes(p.Get("trimmed")), "error", p.Get("error"))
			})), nil
		},
	})

	c.Define("proxmox_guest_power", Def{
		Toolset:     "agent",
		Title:       "Shut down/suspend via agent",
		Description: "Ask the guest OS, through the QEMU guest agent, to shut down or suspend (to RAM, disk or hybrid). The request returns immediately; the guest then acts on it. Prefer proxmox_vm_power action=shutdown, which already uses the agent for shutdown when it is enabled; this is for agent-driven suspend or when you specifically want the in-guest path.",
		Write:       true,
		Destructive: True,
		Input:       baseInput(P("action", Enum("shutdown", "suspend-ram", "suspend-disk", "suspend-hybrid").D("shutdown, suspend-ram (sleep), suspend-disk (hibernate) or suspend-hybrid"))),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := resolveVm(ctx, a)
			if err != nil {
				return nil, err
			}
			if _, err := agent(ctx, g, "POST", a.Str("action"), nil, false); err != nil {
				return nil, err
			}
			return js.O("vmid", g.VMID, "node", g.Node, "action", a.Get("action"), "status", "requested"), nil
		},
	})

	// ---------------------------------------------------------------- exec

	pollExec := func(ctx context.Context, g proxmox.GuestRef, pid float64, timeoutMs float64) (*Obj, error) {
		deadline := time.Now().Add(time.Duration(timeoutMs * float64(time.Millisecond)))
		delay := 200.0
		for {
			st, err := agent(ctx, g, "GET", "exec-status", js.O("pid", pid), true)
			if err != nil {
				return nil, err
			}
			wait := time.Duration(delay * float64(time.Millisecond))
			if agentObj(st).Truthy("exited") || time.Now().Add(wait).After(deadline) {
				return agentObj(st), nil
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			delay = math.Min(delay*1.5, 2000)
		}
	}

	execResult := func(g proxmox.GuestRef, pid float64, st *Obj, started *time.Time) *Obj {
		exited := st.Truthy("exited")
		var duration any = js.Undef
		if started != nil && exited {
			duration = float64(time.Since(*started).Milliseconds())
		}
		return proxmox.C(
			"vmid", g.VMID,
			"node", g.Node,
			"pid", pid,
			"exited", exited,
			"exitcode", st.Get("exitcode"),
			"signal", st.Get("signal"),
			// Proxmox already base64-decodes out-data/err-data from the agent.
			"stdout", st.Get("out-data"),
			"stderr", st.Get("err-data"),
			"stdout_truncated", ifThen(st.Truthy("out-truncated"), true),
			"stderr_truncated", ifThen(st.Truthy("err-truncated"), true),
			"duration_ms", duration,
			"note", ifThen(!exited, "Still running; fetch the result later with proxmox_get_guest_exec_status (pid "+js.NumStr(pid)+")"),
		)
	}

	c.Define("proxmox_guest_exec", Def{
		Toolset:     "agent",
		Title:       "Run a command in a VM (agent)",
		Description: "Run a command inside a running VM through the QEMU guest agent and wait for it (up to timeout_s) to return exit code, stdout and stderr. `command` is the program plus arguments with no shell: use [\"/bin/sh\", \"-c\", \"...\"] for pipes/redirection on Linux or [\"cmd.exe\", \"/c\", \"...\"] / [\"powershell.exe\", \"-Command\", \"...\"] on Windows. Runs as the agent's user (root/SYSTEM). If it runs longer than timeout_s the pid is returned; follow up with proxmox_get_guest_exec_status. Output captured by the agent is size-limited (see the *_truncated flags).",
		Exec:        true,
		Input: baseInput(
			P("command", Arr(Str()).Min(1).D("Program and arguments, e.g. [\"/bin/sh\", \"-c\", \"df -h\"] or [\"systemctl\", \"status\", \"nginx\"]")),
			P("input_data", Str().Max(65536).Opt().D("Data passed to the command's standard input (max 65536 characters)")),
			P("timeout_s", Int().Min(0).Max(600).Opt().D("How long to wait for the command to finish (default 30; 0 = return the pid immediately)")),
		),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := resolveVm(ctx, a)
			if err != nil {
				return nil, err
			}
			started := time.Now()
			res, err := agent(ctx, g, "POST", "exec", defined(js.O("command", a.Get("command"), "input-data", a.Get("input_data"))), true)
			if err != nil {
				return nil, err
			}
			var pidV any = js.Undef
			if o, ok := res.(*Obj); ok {
				pidV = o.Get("pid")
			}
			pid := js.Num(pidV)
			if !js.IsFinite(pid) {
				return nil, proxmox.Errorf("The guest agent did not return a pid for the command (got %s)", js.Stringify(res, ""))
			}
			timeoutMs := js.Num(or(a.Get("timeout_s"), 30)) * 1000
			if timeoutMs == 0 {
				return execResult(g, pid, js.O("exited", false), nil), nil
			}
			st, err := pollExec(ctx, g, pid, timeoutMs)
			if err != nil {
				return nil, err
			}
			return execResult(g, pid, st, &started), nil
		},
	})

	c.Define("proxmox_get_guest_exec_status", Def{
		Toolset:     "agent",
		Title:       "Get guest command result (agent)",
		Description: "Check on a command started with proxmox_guest_exec: whether it has exited and, if so, its exit code, stdout and stderr. Note the guest agent forgets a command's result once it has been read after it exited.",
		Exec:        true,
		ReadOnly:    True,
		Input: baseInput(
			P("pid", Int().Pos().D("The pid returned by proxmox_guest_exec")),
			P("wait_s", Int().Min(0).Max(600).Opt().D("Wait up to this many seconds for the command to exit (default 0: check once)")),
		),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := resolveVm(ctx, a)
			if err != nil {
				return nil, err
			}
			st, err := pollExec(ctx, g, a.Num("pid"), js.Num(or(a.Get("wait_s"), 0))*1000)
			if err != nil {
				return nil, err
			}
			return execResult(g, a.Num("pid"), st, nil), nil
		},
	})

	c.Define("proxmox_guest_read_file", Def{
		Toolset:     "agent",
		Title:       "Read a file in a VM (agent)",
		Description: "Read a file from inside a running VM through the QEMU guest agent (e.g. a log or config file). Returns text by default, or base64 for binary files (encoding=base64). Reads at most `count` bytes from `offset` (Proxmox caps a read at 16 MiB); `truncated: true` means the end of the file was not reached, so read on with a higher offset. Gated behind exec permission because it can read any file in the guest.",
		Exec:        true,
		ReadOnly:    True,
		Input: baseInput(
			P("file", Str().Min(1).D("Absolute path inside the guest, e.g. /etc/os-release or C:\\Windows\\System32\\drivers\\etc\\hosts")),
			P("offset", Int().Min(0).Opt().D("Byte offset to start reading at (default 0)")),
			P("count", Int().Pos().Max(16777216).Opt().D("Maximum bytes to read (default 1048576 = 1 MiB, max 16 MiB)")),
			P("encoding", Enum("text", "base64").Opt().D("text (default) decodes the content; base64 returns it encoded, for binary files")),
		),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := resolveVm(ctx, a)
			if err != nil {
				return nil, err
			}
			b64 := a.Get("encoding") == "base64"
			res, err := agent(ctx, g, "GET", "file-read", defined(js.O(
				"file", a.Get("file"),
				"offset", a.Get("offset"),
				"count", or(a.Get("count"), 1048576),
				"decode", ifThen(b64, false),
			)), true)
			if err != nil {
				return nil, err
			}
			ro := agentObj(res)
			content := js.Str(or(ro.Get("content"), ""))
			bytes := len(content)
			if b64 {
				bytes = len(backupDecodeBase64(content))
			}
			return proxmox.C(
				"vmid", g.VMID,
				"node", g.Node,
				"file", a.Get("file"),
				"offset", a.Get("offset"),
				"encoding", pick(b64, "base64", "text"),
				"bytes", bytes,
				"truncated", ro.Truthy("truncated"),
				"content", content,
			), nil
		},
	})

	c.Define("proxmox_guest_write_file", Def{
		Toolset:     "agent",
		Title:       "Write a file in a VM (agent)",
		Description: "Write (create or overwrite) a file inside a running VM through the QEMU guest agent. Content is text by default; pass encoding=base64 with base64 content for binary files. Proxmox limits content to 61440 characters (about 60 KiB of text or 45 KiB of binary as base64). Overwrites the whole file without a backup; there is no append mode (read, modify and write back instead).",
		Exec:        true,
		Destructive: True,
		Idempotent:  true,
		Input: baseInput(
			P("file", Str().Min(1).D("Absolute path inside the guest")),
			// Proxmox caps the parameter at 61440 characters; larger files need several writes or a different route.
			P("content", Str().Max(61440).D("File content (text, or base64 when encoding=base64); at most 61440 characters per call")),
			P("encoding", Enum("text", "base64").Opt().D("text (default) or base64 (content is already base64-encoded)")),
		),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := resolveVm(ctx, a)
			if err != nil {
				return nil, err
			}
			b64 := a.Get("encoding") == "base64"
			if _, err := agent(ctx, g, "POST", "file-write", js.O("file", a.Get("file"), "content", a.Get("content"), "encode", ifThen(b64, false)), true); err != nil {
				return nil, err
			}
			bytes := len(a.Str("content"))
			if b64 {
				bytes = len(backupDecodeBase64(a.Str("content")))
			}
			return js.O("vmid", g.VMID, "node", g.Node, "file", a.Get("file"), "written", proxmox.FormatBytes(bytes), "bytes", bytes), nil
		},
	})

	c.Define("proxmox_guest_set_password", Def{
		Toolset:     "agent",
		Title:       "Set a guest user's password (agent)",
		Description: "Set the password of a user account inside a running VM through the QEMU guest agent (e.g. to regain access). The user must already exist. With crypted=true, `password` is an already-hashed crypt() string. For cloud-init VMs, the cipassword option is the persistent alternative.",
		Exec:        true,
		Destructive: True,
		Idempotent:  true,
		Input: baseInput(
			P("username", Str().Min(1).D("User account inside the guest, e.g. root or Administrator")),
			P("password", Str().Min(1).D("The new password (or a crypt() hash with crypted=true)")),
			P("crypted", Bool().Opt().D("The password is already a crypt() hash (default false)")),
		),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := resolveVm(ctx, a)
			if err != nil {
				return nil, err
			}
			if _, err := agent(ctx, g, "POST", "set-user-password", defined(js.O("username", a.Get("username"), "password", a.Get("password"), "crypted", a.Get("crypted"))), true); err != nil {
				return nil, err
			}
			return js.O("vmid", g.VMID, "node", g.Node, "username", a.Get("username"), "status", "password changed"), nil
		},
	})
}
