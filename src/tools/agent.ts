import { z } from "zod";
import { type GuestRef, ProxmoxError } from "../proxmox/client.js";
import { compact, formatBytes, isoTime, percent, usage } from "../proxmox/format.js";
import { defined, defineTool, guestNodeArg, rawArg, vmidArg, type ToolContext } from "./util.js";

type Obj = Record<string, any>;

/** The agent endpoints wrap the QGA reply as { result: ... }; exec/file endpoints don't. */
function unwrap(res: any): any {
  return res && typeof res === "object" && !Array.isArray(res) && "result" in res ? res.result : res;
}

/** Turns Proxmox's terse guest agent failures into actionable messages. */
export function agentError(err: unknown, vmid: number): Error {
  if (!(err instanceof ProxmoxError)) return err instanceof Error ? err : new Error(String(err));
  const msg = err.message;
  if (/No QEMU guest agent configured/i.test(msg)) {
    return new ProxmoxError(
      `The QEMU guest agent is not enabled for VM ${vmid}. Enable it in the VM options (agent=1 in the VM config), install and start qemu-guest-agent inside the guest, then fully stop and start the VM. (${msg})`,
      err.status,
    );
  }
  if (/guest agent is not running/i.test(msg)) {
    return new ProxmoxError(
      `The QEMU guest agent in VM ${vmid} is not responding. Check that the VM is running and that the qemu-guest-agent service is installed and running inside the guest. (${msg})`,
      err.status,
    );
  }
  if (/VM \d+ not running/i.test(msg)) return new ProxmoxError(`VM ${vmid} is not running; the guest agent only works in a running VM. (${msg})`, err.status);
  if (/got timeout|timeout/i.test(msg)) {
    return new ProxmoxError(`The guest agent in VM ${vmid} did not answer in time; it may be busy or hung. (${msg})`, err.status);
  }
  return err;
}

/** ip-address entries → "addr/prefix". */
function addresses(iface: Obj, kind: "ipv4" | "ipv6"): string[] {
  return ((iface["ip-addresses"] ?? []) as Obj[])
    .filter((a) => a["ip-address-type"] === kind)
    .map((a) => (a.prefix !== undefined ? `${a["ip-address"]}/${a.prefix}` : String(a["ip-address"])));
}

function summarizeInterface(i: Obj): Obj {
  const s: Obj = i.statistics ?? {};
  const nz = (v: unknown) => (Number(v) ? Number(v) : undefined);
  return compact({
    name: i.name,
    mac: i["hardware-address"],
    ipv4: addresses(i, "ipv4"),
    ipv6: addresses(i, "ipv6"),
    rx: formatBytes(s["rx-bytes"]),
    tx: formatBytes(s["tx-bytes"]),
    rx_packets: s["rx-packets"],
    tx_packets: s["tx-packets"],
    rx_errors: nz(s["rx-errs"]),
    tx_errors: nz(s["tx-errs"]),
    rx_dropped: nz(s["rx-dropped"]),
    tx_dropped: nz(s["tx-dropped"]),
  });
}

function isLoopback(i: Obj): boolean {
  return i.name === "lo" || /loopback/i.test(String(i.name)) || addresses(i, "ipv4").some((a) => a.startsWith("127."));
}

function summarizeFilesystem(f: Obj): Obj {
  const used = f["used-bytes"];
  const total = f["total-bytes"];
  return compact({
    mountpoint: f.mountpoint,
    type: f.type,
    device: f.name,
    usage: usage(used, total),
    used_pct: percent(used, total),
    disks: ((f.disk ?? []) as Obj[]).map((d) => compact({ dev: d.dev, serial: d.serial, bus: d["bus-type"], target: d.target, unit: d.unit })),
  });
}

const POWER_ACTIONS = ["shutdown", "suspend-ram", "suspend-disk", "suspend-hybrid"] as const;

export function registerAgentTools(ctx: ToolContext): void {
  const { client } = ctx;

  const resolveVm = (vmid: number, node?: string) => client.resolveGuest(vmid, { node, type: "qemu" });

  /** Calls an agent sub-endpoint (e.g. "get-osinfo") and unwraps the result, with friendly errors. */
  async function agent(g: GuestRef, method: "GET" | "POST", command: string, params?: Obj, raw = false): Promise<any> {
    try {
      const res = await client.request(method, `${client.guestPath(g)}/agent/${command}`, params);
      return raw ? res : unwrap(res);
    } catch (err) {
      throw agentError(err, g.vmid);
    }
  }

  const baseInput = { vmid: vmidArg, node: guestNodeArg };

  // ---------------------------------------------------------------- read

  defineTool(ctx, "proxmox_get_guest_info", {
    toolset: "agent",
    title: "Get guest OS info (agent)",
    description:
      "Ask a running VM's QEMU guest agent about the guest: OS name/version/kernel, hostname, timezone, guest clock (and its skew from this server), vCPUs online and the agent version with its disabled commands. VMs only (containers need no agent). Individual queries that fail are reported under `errors` instead of failing the call. Use proxmox_get_guest_network for IP addresses.",
    input: { ...baseInput, raw: rawArg },
    handler: async (a) => {
      const g = await resolveVm(a.vmid, a.node);
      // Ping first so a missing/stopped agent gives one clear error instead of seven.
      await agent(g, "POST", "ping");
      const queries = ["get-osinfo", "get-host-name", "get-timezone", "get-time", "get-vcpus", "info"] as const;
      const settled = await Promise.allSettled(queries.map((q) => agent(g, "GET", q)));
      const r: Obj = {};
      const errors: Obj = {};
      settled.forEach((s, i) => {
        if (s.status === "fulfilled") r[queries[i]] = s.value;
        else errors[queries[i]] = s.reason instanceof Error ? s.reason.message : String(s.reason);
      });
      if (a.raw) return compact({ vmid: g.vmid, node: g.node, ...r, errors });
      const os: Obj = r["get-osinfo"] ?? {};
      const tz: Obj = r["get-timezone"] ?? {};
      const timeNs = Number(r["get-time"]);
      const vcpus: Obj[] | undefined = r["get-vcpus"];
      const info: Obj | undefined = r.info;
      const commands: Obj[] = info?.supported_commands ?? [];
      return compact({
        vmid: g.vmid,
        name: g.name,
        node: g.node,
        agent: "running",
        os: compact({
          name: os["pretty-name"] ?? os.name,
          id: os.id,
          version: os.version,
          version_id: os["version-id"],
          kernel: os["kernel-release"],
          kernel_version: os["kernel-version"],
          machine: os.machine,
          variant: os.variant,
        }),
        hostname: r["get-host-name"]?.["host-name"],
        timezone: compact({ zone: tz.zone, utc_offset_s: tz.offset }),
        guest_time: Number.isFinite(timeNs) && timeNs > 0 ? new Date(timeNs / 1e6).toISOString() : undefined,
        clock_skew_s: Number.isFinite(timeNs) && timeNs > 0 ? Math.round((timeNs / 1e6 - Date.now()) / 100) / 10 : undefined,
        vcpus: vcpus ? { total: vcpus.length, online: vcpus.filter((v) => v.online).length } : undefined,
        agent_version: info?.version,
        disabled_commands: commands.filter((c) => c.enabled === false).map((c) => c.name),
        errors,
      });
    },
  });

  defineTool(ctx, "proxmox_get_guest_network", {
    toolset: "agent",
    title: "Get guest network interfaces (agent)",
    description:
      "List a running VM's network interfaces as seen inside the guest (via the QEMU guest agent): name, MAC, IPv4/IPv6 addresses with prefix, and traffic counters. The reliable way to find a VM's IP address. Loopback is hidden unless include_loopback=true. VMs only; for containers use proxmox_get_container_interfaces.",
    input: {
      ...baseInput,
      include_loopback: z.boolean().optional().describe("Include the loopback interface (default false)"),
      raw: rawArg,
    },
    handler: async (a) => {
      const g = await resolveVm(a.vmid, a.node);
      const ifaces: Obj[] = (await agent(g, "GET", "network-get-interfaces")) ?? [];
      if (a.raw) return ifaces;
      const shown = a.include_loopback ? ifaces : ifaces.filter((i) => !isLoopback(i));
      const items = shown.map(summarizeInterface);
      return {
        vmid: g.vmid,
        node: g.node,
        ipv4: items.flatMap((i) => (i.ipv4 ?? []).map((x: string) => x.split("/")[0])),
        interfaces: items,
      };
    },
  });

  defineTool(ctx, "proxmox_get_guest_filesystems", {
    toolset: "agent",
    title: "Get guest filesystems (agent)",
    description:
      "List a running VM's mounted filesystems from inside the guest (QEMU guest agent get-fsinfo): mountpoint, type, used/total space and backing disks. Use it to check real disk usage inside a VM (Proxmox's own disk figures for VMs are not filesystem usage). VMs only.",
    input: { ...baseInput, raw: rawArg },
    handler: async (a) => {
      const g = await resolveVm(a.vmid, a.node);
      const fs: Obj[] = (await agent(g, "GET", "get-fsinfo")) ?? [];
      if (a.raw) return fs;
      const items = fs.map(summarizeFilesystem).sort((x, y) => String(x.mountpoint).localeCompare(String(y.mountpoint)));
      return { vmid: g.vmid, node: g.node, filesystems: items };
    },
  });

  defineTool(ctx, "proxmox_get_guest_users", {
    toolset: "agent",
    title: "Get logged-in guest users (agent)",
    description: "List the users currently logged in to a running VM (QEMU guest agent get-users), with domain and login time. VMs only.",
    input: baseInput,
    handler: async (a) => {
      const g = await resolveVm(a.vmid, a.node);
      const users: Obj[] = (await agent(g, "GET", "get-users")) ?? [];
      return {
        vmid: g.vmid,
        node: g.node,
        users: users.map((u) => compact({ user: u.user, domain: u.domain, login_time: isoTime(u["login-time"]) })),
      };
    },
  });

  defineTool(ctx, "proxmox_get_guest_memory_blocks", {
    toolset: "agent",
    title: "Get guest memory blocks (agent)",
    description:
      "Show a running VM's memory blocks as the guest sees them (QEMU guest agent get-memory-blocks/get-memory-block-info): block size and how many blocks/how much memory is online. Useful after memory hotplug. VMs only; not all guest OSes support it.",
    input: baseInput,
    handler: async (a) => {
      const g = await resolveVm(a.vmid, a.node);
      const [blocks, info] = await Promise.all([
        agent(g, "GET", "get-memory-blocks") as Promise<Obj[]>,
        agent(g, "GET", "get-memory-block-info").catch(() => undefined) as Promise<Obj | undefined>,
      ]);
      const list = blocks ?? [];
      const online = list.filter((b) => b.online).length;
      const size = Number(info?.size);
      return compact({
        vmid: g.vmid,
        node: g.node,
        block_size: formatBytes(info?.size),
        blocks: list.length,
        online_blocks: online,
        offline_blocks: list.length - online,
        online_memory: Number.isFinite(size) && size > 0 ? formatBytes(online * size) : undefined,
      });
    },
  });

  // ---------------------------------------------------------------- write

  defineTool(ctx, "proxmox_guest_fs_freeze", {
    toolset: "agent",
    title: "Freeze/thaw guest filesystems (agent)",
    description:
      "Freeze, thaw or check the freeze state of a running VM's filesystems through the QEMU guest agent (fsfreeze). While frozen, writes inside the guest block, so always thaw promptly. Backups and snapshots already freeze/thaw automatically when the agent is enabled; use this for external snapshotting or to recover a guest left frozen (action=status, then thaw).",
    write: true,
    destructive: true,
    input: {
      ...baseInput,
      action: z.enum(["freeze", "thaw", "status"]).describe("freeze: freeze all filesystems; thaw: unfreeze them; status: report 'frozen' or 'thawed'"),
    },
    handler: async (a) => {
      const g = await resolveVm(a.vmid, a.node);
      const res = await agent(g, "POST", `fsfreeze-${a.action}`);
      if (a.action === "status") return { vmid: g.vmid, node: g.node, status: res };
      return compact({ vmid: g.vmid, node: g.node, action: a.action, filesystems: typeof res === "number" ? res : undefined, result: typeof res === "number" ? undefined : res });
    },
  });

  defineTool(ctx, "proxmox_guest_fstrim", {
    toolset: "agent",
    title: "Trim guest filesystems (agent)",
    description:
      "Run fstrim inside a running VM via the QEMU guest agent so freed blocks are discarded and thin-provisioned storage can reclaim space. Needs discard enabled on the VM's disks to have any effect on the host. Can take a while on large disks.",
    write: true,
    destructive: false,
    idempotent: true,
    input: baseInput,
    handler: async (a) => {
      const g = await resolveVm(a.vmid, a.node);
      const res: Obj = (await agent(g, "POST", "fstrim")) ?? {};
      const paths: Obj[] = res.paths ?? [];
      return {
        vmid: g.vmid,
        node: g.node,
        paths: paths.map((p) => compact({ path: p.path, trimmed: formatBytes(p.trimmed), error: p.error })),
      };
    },
  });

  defineTool(ctx, "proxmox_guest_power", {
    toolset: "agent",
    title: "Shut down/suspend via agent",
    description:
      "Ask the guest OS, through the QEMU guest agent, to shut down or suspend (to RAM, disk or hybrid). The request returns immediately; the guest then acts on it. Prefer proxmox_vm_power action=shutdown, which already uses the agent for shutdown when it is enabled; this is for agent-driven suspend or when you specifically want the in-guest path.",
    write: true,
    destructive: true,
    input: {
      ...baseInput,
      action: z.enum(POWER_ACTIONS).describe("shutdown, suspend-ram (sleep), suspend-disk (hibernate) or suspend-hybrid"),
    },
    handler: async (a) => {
      const g = await resolveVm(a.vmid, a.node);
      await agent(g, "POST", a.action);
      return { vmid: g.vmid, node: g.node, action: a.action, status: "requested" };
    },
  });

  // ---------------------------------------------------------------- exec

  async function pollExec(g: GuestRef, pid: number, timeoutMs: number): Promise<Obj> {
    const deadline = Date.now() + timeoutMs;
    let delay = 200;
    for (;;) {
      const st: Obj = await agent(g, "GET", "exec-status", { pid }, true);
      if (st?.exited || Date.now() + delay > deadline) return st ?? {};
      await new Promise((r) => setTimeout(r, delay));
      delay = Math.min(delay * 1.5, 2000);
    }
  }

  function execResult(g: GuestRef, pid: number, st: Obj, started?: number): Obj {
    const exited = Boolean(st.exited);
    return compact({
      vmid: g.vmid,
      node: g.node,
      pid,
      exited,
      exitcode: st.exitcode,
      signal: st.signal,
      // Proxmox already base64-decodes out-data/err-data from the agent.
      stdout: st["out-data"],
      stderr: st["err-data"],
      stdout_truncated: st["out-truncated"] ? true : undefined,
      stderr_truncated: st["err-truncated"] ? true : undefined,
      duration_ms: started !== undefined && exited ? Date.now() - started : undefined,
      note: exited ? undefined : `Still running; fetch the result later with proxmox_get_guest_exec_status (pid ${pid})`,
    });
  }

  defineTool(ctx, "proxmox_guest_exec", {
    toolset: "agent",
    title: "Run a command in a VM (agent)",
    description:
      "Run a command inside a running VM through the QEMU guest agent and wait for it (up to timeout_s) to return exit code, stdout and stderr. `command` is the program plus arguments with no shell: use [\"/bin/sh\", \"-c\", \"...\"] for pipes/redirection on Linux or [\"cmd.exe\", \"/c\", \"...\"] / [\"powershell.exe\", \"-Command\", \"...\"] on Windows. Runs as the agent's user (root/SYSTEM). If it runs longer than timeout_s the pid is returned; follow up with proxmox_get_guest_exec_status. Output captured by the agent is size-limited (see the *_truncated flags).",
    exec: true,
    input: {
      ...baseInput,
      command: z.array(z.string()).min(1).describe("Program and arguments, e.g. [\"/bin/sh\", \"-c\", \"df -h\"] or [\"systemctl\", \"status\", \"nginx\"]"),
      input_data: z.string().max(65536).optional().describe("Data passed to the command's standard input (max 65536 characters)"),
      timeout_s: z.number().int().min(0).max(600).optional().describe("How long to wait for the command to finish (default 30; 0 = return the pid immediately)"),
    },
    handler: async (a) => {
      const g = await resolveVm(a.vmid, a.node);
      const started = Date.now();
      const res: Obj = await agent(g, "POST", "exec", defined({ command: a.command, "input-data": a.input_data }), true);
      const pid = Number(res?.pid);
      if (!Number.isFinite(pid)) throw new ProxmoxError(`The guest agent did not return a pid for the command (got ${JSON.stringify(res)})`);
      const timeoutMs = (a.timeout_s ?? 30) * 1000;
      if (timeoutMs === 0) return execResult(g, pid, { exited: false });
      const st = await pollExec(g, pid, timeoutMs);
      return execResult(g, pid, st, started);
    },
  });

  defineTool(ctx, "proxmox_get_guest_exec_status", {
    toolset: "agent",
    title: "Get guest command result (agent)",
    description:
      "Check on a command started with proxmox_guest_exec: whether it has exited and, if so, its exit code, stdout and stderr. Note the guest agent forgets a command's result once it has been read after it exited.",
    exec: true,
    readOnly: true,
    input: {
      ...baseInput,
      pid: z.number().int().positive().describe("The pid returned by proxmox_guest_exec"),
      wait_s: z.number().int().min(0).max(600).optional().describe("Wait up to this many seconds for the command to exit (default 0: check once)"),
    },
    handler: async (a) => {
      const g = await resolveVm(a.vmid, a.node);
      const st = await pollExec(g, a.pid, (a.wait_s ?? 0) * 1000);
      return execResult(g, a.pid, st);
    },
  });

  defineTool(ctx, "proxmox_guest_read_file", {
    toolset: "agent",
    title: "Read a file in a VM (agent)",
    description:
      "Read a file from inside a running VM through the QEMU guest agent (e.g. a log or config file). Returns text by default, or base64 for binary files (encoding=base64). Reads at most `count` bytes from `offset` (Proxmox caps a read at 16 MiB); `truncated: true` means the end of the file was not reached, so read on with a higher offset. Gated behind exec permission because it can read any file in the guest.",
    exec: true,
    readOnly: true,
    input: {
      ...baseInput,
      file: z.string().min(1).describe("Absolute path inside the guest, e.g. /etc/os-release or C:\\Windows\\System32\\drivers\\etc\\hosts"),
      offset: z.number().int().min(0).optional().describe("Byte offset to start reading at (default 0)"),
      count: z
        .number()
        .int()
        .positive()
        .max(16777216)
        .optional()
        .describe("Maximum bytes to read (default 1048576 = 1 MiB, max 16 MiB)"),
      encoding: z.enum(["text", "base64"]).optional().describe("text (default) decodes the content; base64 returns it encoded, for binary files"),
    },
    handler: async (a) => {
      const g = await resolveVm(a.vmid, a.node);
      const base64 = a.encoding === "base64";
      const res: Obj = await agent(
        g,
        "GET",
        "file-read",
        defined({ file: a.file, offset: a.offset, count: a.count ?? 1048576, decode: base64 ? false : undefined }),
        true,
      );
      const content = String(res?.content ?? "");
      return compact({
        vmid: g.vmid,
        node: g.node,
        file: a.file,
        offset: a.offset,
        encoding: base64 ? "base64" : "text",
        bytes: base64 ? Buffer.from(content, "base64").length : Buffer.byteLength(content),
        truncated: res?.truncated ? true : false,
        content,
      });
    },
  });

  defineTool(ctx, "proxmox_guest_write_file", {
    toolset: "agent",
    title: "Write a file in a VM (agent)",
    description:
      "Write (create or overwrite) a file inside a running VM through the QEMU guest agent. Content is text by default; pass encoding=base64 with base64 content for binary files. Proxmox limits content to 61440 characters (about 60 KiB of text or 45 KiB of binary as base64). Overwrites the whole file without a backup; there is no append mode (read, modify and write back instead).",
    exec: true,
    destructive: true,
    idempotent: true,
    input: {
      ...baseInput,
      file: z.string().min(1).describe("Absolute path inside the guest"),
      // Proxmox caps the parameter at 61440 characters; larger files need several writes or a different route.
      content: z.string().max(61440).describe("File content (text, or base64 when encoding=base64); at most 61440 characters per call"),
      encoding: z.enum(["text", "base64"]).optional().describe("text (default) or base64 (content is already base64-encoded)"),
    },
    handler: async (a) => {
      const g = await resolveVm(a.vmid, a.node);
      const base64 = a.encoding === "base64";
      await agent(g, "POST", "file-write", { file: a.file, content: a.content, encode: base64 ? false : undefined }, true);
      const bytes = base64 ? Buffer.from(a.content, "base64").length : Buffer.byteLength(a.content);
      return { vmid: g.vmid, node: g.node, file: a.file, written: formatBytes(bytes), bytes };
    },
  });

  defineTool(ctx, "proxmox_guest_set_password", {
    toolset: "agent",
    title: "Set a guest user's password (agent)",
    description:
      "Set the password of a user account inside a running VM through the QEMU guest agent (e.g. to regain access). The user must already exist. With crypted=true, `password` is an already-hashed crypt() string. For cloud-init VMs, the cipassword option is the persistent alternative.",
    exec: true,
    destructive: true,
    idempotent: true,
    input: {
      ...baseInput,
      username: z.string().min(1).describe("User account inside the guest, e.g. root or Administrator"),
      password: z.string().min(1).describe("The new password (or a crypt() hash with crypted=true)"),
      crypted: z.boolean().optional().describe("The password is already a crypt() hash (default false)"),
    },
    handler: async (a) => {
      const g = await resolveVm(a.vmid, a.node);
      await agent(g, "POST", "set-user-password", defined({ username: a.username, password: a.password, crypted: a.crypted }), true);
      return { vmid: g.vmid, node: g.node, username: a.username, status: "password changed" };
    },
  });
}
