<div align="center">

<img src="docs/assets/logo.svg" alt="proxmox-mcp logo" width="112" height="112">

# proxmox-mcp

**Talk to your Proxmox cluster.**

An open-source [Model Context Protocol](https://modelcontextprotocol.io) server for **Proxmox VE**. It lets Claude and other AI assistants monitor and manage your nodes, VMs, containers, storage, backups, firewall and more, all from one Docker container.

[![CI](https://github.com/mattoddie/proxmox-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/mattoddie/proxmox-mcp/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/mattoddie/proxmox-mcp?sort=semver)](https://github.com/mattoddie/proxmox-mcp/releases)
[![Container image](https://img.shields.io/badge/ghcr.io-mattoddie%2Fproxmox--mcp-2496ED?logo=docker&logoColor=white)](https://github.com/mattoddie/proxmox-mcp/pkgs/container/proxmox-mcp)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

**[proxmox-mcp.mattoddie.dev](https://proxmox-mcp.mattoddie.dev)**

[Quick start](#quick-start) · [Documentation](https://proxmox-mcp.mattoddie.dev/docs/) · [Tool reference](docs/tools.md) · [Contributing](CONTRIBUTING.md)

</div>

---

> **"Why is the database VM so slow today?"**
> *VM 101 (db) on pve2 has averaged 94% CPU since 09:00, and pve2's IO wait has been above 30% for the same period. Its disk lives on `local-lvm`, which is 91% full, and last night's backup task to the same disk is still running. Want me to show the backup task's log, or plan a migration of the VM to pve1, which has 40 GiB of free memory?*

Ask about your cluster in plain English. The assistant picks from **300+ tools** to look things up and, if you allow it, make changes. Some examples:

- 🩺 *"Give me a health check of the cluster."*
- 📈 *"Which VMs use the most memory, and which node has the most headroom?"*
- 💾 *"Which guests aren't backed up, and did last night's backups succeed?"*
- 🧹 *"Find snapshots older than a month."*
- 🔐 *"Review users, API tokens and permissions for anything risky."*
- 🚀 *"Clone the Debian template into a new VM called web-02 with 4 GB of RAM on VLAN 20."*

## Highlights

- **🐳 Docker-first.** There's one small, distroless image (`ghcr.io/mattoddie/proxmox-mcp`) for `linux/amd64` and `linux/arm64`. It serves Streamable HTTP for a long-running service, or stdio for `docker run -i`.
- **🛡️ Safe by default.** The server is read-only unless you turn on writes. Deleting needs a second switch, and running commands inside guests needs a third. Tools that aren't allowed aren't registered at all, destructive tools are labelled so your MCP client asks before running them, and passwords and keys are redacted from output.
- **🧰 Broad coverage.** It covers 19 toolsets: cluster, nodes, VMs, containers, guest agent, snapshots, storage, backups, tasks, access control, firewall, HA, SDN, Ceph, pools, replication, notifications, hardware and a raw API tool.
- **⏱️ Task-aware.** Long operations such as starts, clones, migrations and backups run as Proxmox tasks. Tools wait for them and report the result with the end of the task log, or hand back the task ID to follow up later.
- **🎯 Addressed by VMID.** Ask about VM 100 and the server finds which node it's on. Single-node installs need no node names at all.
- **🎛️ Tunable.** Expose only the toolsets you need, which keeps the model's context small and focused.
- **🔑 Least privilege.** Use a dedicated API token scoped with Proxmox's own roles and ACLs, so Proxmox enforces the limits too.
- **🧪 Tested.** The test suite runs against a mock Proxmox API on every build.

## Quick start

You need Docker, a Proxmox VE 8 or 9 server or cluster, and an MCP client such as [Claude Code](https://claude.com/claude-code).

**1. Create an API token.** On any node, as root, create a read-only user and token:

```sh
pveum user add mcp@pve --comment "proxmox-mcp"
pveum acl modify / --users mcp@pve --roles PVEAuditor
pveum user token add mcp@pve mcp --privsep 0
```

Copy the token secret from the output. You can't see it again later. [Getting started](docs/getting-started.md#2-create-a-user-and-api-token) explains the options, including a token that can make changes.

**2. Start the server.**

```sh
mkdir proxmox-mcp && cd proxmox-mcp
curl -fsSLO https://raw.githubusercontent.com/mattoddie/proxmox-mcp/main/compose.yaml
curl -fsSL https://raw.githubusercontent.com/mattoddie/proxmox-mcp/main/example.env -o .env
# Edit .env: set PROXMOX_URL, PROXMOX_TOKEN_ID (mcp@pve!mcp), PROXMOX_TOKEN_SECRET,
# a random MCP_AUTH_TOKEN (required; e.g. `openssl rand -hex 32`) and how to check the
# Proxmox certificate (required for https): PROXMOX_TLS_FINGERPRINT (recommended; the
# SHA-256 fingerprint from Node > System > Certificates), or PROXMOX_VERIFY_SSL=false
docker compose up -d
```

**3. Connect your assistant.**

```sh
claude mcp add --transport http proxmox http://<docker-host>:8080/mcp \
  --header "Authorization: Bearer <MCP_AUTH_TOKEN>"
```

Now ask *"How's my Proxmox cluster doing?"* in Claude Code.

Using **Claude Desktop, VS Code, Cursor or Codex**? See [Connecting MCP clients](docs/clients.md). Prefer not to run a service? Use [stdio mode](docs/clients.md#stdio-mode) with `docker run -i`.

## Turning on changes

Out of the box the assistant can look but not touch. To let it make changes, add these to `.env` and run `docker compose up -d`:

```sh
PROXMOX_ALLOW_WRITES=true    # start/stop guests, create, clone, migrate, reconfigure, snapshot, back up…
PROXMOX_ALLOW_DELETES=true   # also allow deleting guests, snapshots, backups and configuration (needs writes)
PROXMOX_ALLOW_EXEC=true      # also allow commands and file access inside VMs via the guest agent (needs writes)
PROXMOX_TOOLSETS=cluster,nodes,vms,containers,snapshots,backup,tasks   # optional: expose fewer tools
```

The token needs matching Proxmox permissions too. Read [Security](docs/security.md) before turning on writes for a production cluster.

## What it can do

| Toolset | What's covered |
|---|---|
| `cluster` | Cluster overview, nodes and guests at a glance, datacenter options, corosync config, metric servers, bulk guest actions |
| `nodes` | Node status and statistics, services, network interfaces, DNS, time, updates and repositories, subscriptions, certificates and ACME, logs, node power |
| `vms` | QEMU VMs: status, configuration, power, create, clone, disks, migration, templates, cloud-init, statistics |
| `containers` | LXC containers: status, configuration, power, create, clone, volumes, migration, templates, interfaces, statistics |
| `agent` | QEMU guest agent: OS, network and filesystem information, freeze/trim, and (with exec) commands and file access |
| `snapshots` | VM and container snapshots: list, create, roll back, delete, and a cluster-wide view of old snapshots |
| `storage` | Storage definitions and usage, volumes, ISO and template downloads, physical disks, SMART, ZFS and LVM |
| `backup` | vzdump backup jobs, ad-hoc backups, backup listing, restores, pruning, guests without backups |
| `tasks` | Proxmox tasks: list, inspect, read logs, wait for and stop them |
| `access` | Users, groups, roles, ACLs, API tokens, realms, effective permissions, two-factor authentication |
| `firewall` | Rules, options, aliases, IP sets and security groups at datacenter, node and guest level, firewall log |
| `ha` | High availability status, resources, groups and rules |
| `sdn` | SDN zones, VNets, subnets, controllers, IPAM and DNS, applying pending changes |
| `ceph` | Ceph health, OSDs, pools, monitors, managers, metadata servers, CephFS, flags |
| `pools` | Resource pools and their members |
| `replication` | Storage replication jobs, status and logs |
| `notifications` | Notification targets and matchers |
| `hardware` | PCI and USB devices, mediated devices, resource mappings, CPU models |
| `raw` | An escape hatch that calls any Proxmox API path. Off unless `PROXMOX_TOOLSETS` lists `raw` (e.g. `all,raw`) |

See the **[tool reference](docs/tools.md)** for every tool and argument. Ready-made prompts like `cluster_health_check`, `backup_audit` and `troubleshoot_guest` chain the tools together for common jobs.

## Documentation

The docs are also published at **[proxmox-mcp.mattoddie.dev/docs](https://proxmox-mcp.mattoddie.dev/docs/)**, with search.

| | |
|---|---|
| 🚀 [Getting started](docs/getting-started.md) | Step-by-step setup, from API token to first question |
| 🔌 [Connecting MCP clients](docs/clients.md) | Claude Code, Claude Desktop, VS Code, Cursor, Codex, stdio mode |
| ⚙️ [Configuration](docs/configuration.md) | Every environment variable, Docker secrets, toolsets |
| 🐳 [Deployment](docs/deployment.md) | Compose, image tags, upgrades, TLS reverse proxy, hardening |
| 💬 [Using proxmox-mcp](docs/usage.md) | Example conversations, prompts, tasks, how changes and secrets work |
| 🧰 [Tool reference](docs/tools.md) | Every tool and prompt, generated from the code |
| 🛡️ [Security](docs/security.md) | Threat model, least-privilege tokens, safe write setups |
| 📡 [Compatibility](docs/compatibility.md) | Supported Proxmox VE versions, Ceph, PBS, known gaps |
| 🩹 [Troubleshooting](docs/troubleshooting.md) | Common errors and how to fix them |
| 🛠️ [Development](docs/development.md) | Architecture, adding a tool, testing |

## Contributing

Contributions are very welcome, whether that's bug reports, compatibility reports from your cluster, docs or new tools. Read the [contributing guide](CONTRIBUTING.md) to get started, and please follow the [code of conduct](CODE_OF_CONDUCT.md). To report a vulnerability, see the [security policy](SECURITY.md). Please don't open a public issue for one.

## License

[MIT](LICENSE) © Matt Oddie

*proxmox-mcp is an independent community project. It is not affiliated with, endorsed by or sponsored by Proxmox Server Solutions GmbH. Proxmox is a registered trademark of Proxmox Server Solutions GmbH.*
