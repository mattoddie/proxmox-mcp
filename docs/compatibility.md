# Compatibility

## Proxmox VE versions

| Version | Support |
|---|---|
| Proxmox VE 9.x | ✅ The reference version. Every tool is written against its API. |
| Proxmox VE 8.x | ✅ Supported. A few newer features aren't available (see below). |
| Proxmox VE 7.x and older | ❓ Not tested. Many read tools will work, but these versions are end-of-life. Please upgrade. |

The server talks to the standard Proxmox VE REST API (`/api2/json`) on port 8006, the same API the web UI uses. It works with a single node or a cluster of any size. Proxmox documents the API in its [API viewer](https://pve.proxmox.com/pve-docs/api-viewer/), and keeps it stable within a major version, so new minor releases rarely break anything.

### Features that need a recent version

Some tools use endpoints that only exist on newer releases. Where a feature isn't available, the tool returns an error rather than failing silently, usually `HTTP 501` with *"not implemented on this Proxmox VE version"*, or `HTTP 404`. Nothing else breaks.

| Feature | Needs |
|---|---|
| Bulk start, shut down, suspend and migrate (`proxmox_bulk_guest_action`) | Proxmox VE 9 |
| HA rules (node affinity and resource affinity) | Proxmox VE 9, which replaces HA groups with rules. On 8.x, use HA groups. |
| SDN fabrics | Proxmox VE 9 |
| Guest agent privileges (`VM.GuestAgent.*`) | Proxmox VE 9. On 8.x the equivalent privilege is `VM.Monitor`. See [Getting started](getting-started.md#option-a-read-only-recommended-to-start). |
| Resource mappings for PCI and USB devices | Proxmox VE 8 |
| Notification targets and matchers | Proxmox VE 8.1 or later |

If you find a tool that needs a newer version than this table says, please [report it](https://github.com/mattoddie/proxmox-mcp/issues/new?template=compatibility.yml).

## Authentication

| Method | Support |
|---|---|
| API token, any realm (`pam`, `pve`, LDAP, AD, OpenID) | ✅ Recommended. Works with accounts that have 2FA. |
| Username and password, `pam` or `pve` realm | ✅ The ticket is renewed automatically. |
| Username and password on an account with 2FA | ❌ Use an API token. |
| Username and password for LDAP or AD users | ✅ As long as the realm accepts password logins and the account has no 2FA. |
| OpenID Connect login | ❌ It needs a browser. Use an API token for the user instead. |

## Storage, Ceph and backups

- **Storage types.** Storage tools work with every storage type Proxmox supports: directory, LVM, LVM-thin, ZFS, NFS, CIFS, iSCSI, Ceph RBD, CephFS, Proxmox Backup Server and others. Some operations only apply to some types, for example thin provisioning or snapshot support. Proxmox returns an error when an operation doesn't apply.
- **Ceph.** The `ceph` toolset needs Ceph installed and configured through Proxmox (*Datacenter → Ceph*). On clusters without Ceph its tools return an error, and the `cluster_health_check` prompt skips Ceph. External Ceph clusters used only as RBD storage are visible through the storage tools, but the `ceph` toolset can't manage them.
- **Proxmox Backup Server.** PBS datastores added as storage in Proxmox VE work with the backup tools: listing backups with their verification state, backing up, restoring and pruning. proxmox-mcp talks to Proxmox VE only, not to the PBS API, so PBS-side features such as verify jobs, sync jobs, garbage collection and tape aren't covered.
- **Local backups.** vzdump backups on directory, NFS or CIFS storage are fully supported.

## Guest agent

The `agent` toolset and the guest-network tools need the **QEMU guest agent** installed and running inside the VM, and the *QEMU Guest Agent* option turned on in the VM's options (`agent: 1`). After turning the option on, the VM must be stopped and started from Proxmox, not just rebooted from inside. Containers don't use the agent: their interfaces come from Proxmox directly.

## Running proxmox-mcp

| Component | Requirement |
|---|---|
| Docker image | `linux/amd64` or `linux/arm64` (for example Raspberry Pi 4/5) |
| Go (only to build without Docker) | The version in `go.mod`. The binary itself is static and has no runtime dependencies. |
| MCP clients | Any client supporting Streamable HTTP or stdio. See [Connecting MCP clients](clients.md). |

## Not covered yet

These are known gaps. Contributions are welcome:

- The Proxmox Backup Server API itself (only PBS storage as seen from Proxmox VE is covered)
- Proxmox Mail Gateway and Proxmox Datacenter Manager
- Interactive consoles (VNC, SPICE and terminal sessions)
- Creating or joining clusters

## Help improve this page

Tested proxmox-mcp on a version or setup not listed here? Open a [compatibility report](https://github.com/mattoddie/proxmox-mcp/issues/new?template=compatibility.yml), whether it worked or not. It helps everyone.
