# Changelog

All notable changes to this project are documented here. The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

For proxmox-mcp, the "public API" is the set of tool names, their arguments, the prompts and the configuration variables. Renaming or removing any of these is a breaking change.

## [Unreleased]

## [0.1.0] - 2026-10-11

### Added

- MCP server for Proxmox VE with 314 tools in 19 toolsets: cluster, nodes, vms, containers, agent, snapshots, storage, backup, tasks, access, firewall, ha, sdn, ceph, pools, replication, notifications, hardware and raw.
- Eight built-in prompts: `cluster_health_check`, `troubleshoot_guest`, `capacity_planning`, `backup_audit`, `security_review`, `update_review`, `snapshot_cleanup` and `deploy_vm`.
- Authentication with an API token (`PROXMOX_TOKEN_ID` and `PROXMOX_TOKEN_SECRET`, or `PROXMOX_API_TOKEN`) or a username and password, with automatic ticket renewal.
- Guests addressed by VMID, with the node looked up automatically, and `PROXMOX_DEFAULT_NODE` for node-scoped tools in a cluster.
- Task handling: tools wait for Proxmox tasks (`PROXMOX_TASK_TIMEOUT_MS`) and report the result with the end of the task log, or return the UPID with `wait: false`.
- Four access levels: read-only by default, `PROXMOX_ALLOW_WRITES`, `PROXMOX_ALLOW_DELETES` and `PROXMOX_ALLOW_EXEC` (guest agent commands and file access, QEMU monitor). `PROXMOX_TOOLSETS` limits the tools exposed. Secrets are redacted from output.
- Streamable HTTP (stateless) and stdio transports, with bearer-token auth and a `Host` header allow-list for HTTP.
- TLS verification options for Proxmox (`PROXMOX_VERIFY_SSL`, `PROXMOX_CA_FILE`).
- `_FILE` variants of every variable for Docker secrets.
- Multi-arch Docker image (`linux/amd64`, `linux/arm64`) published to GHCR on each release, with SBOM and provenance attestation.
- Documentation, including a tool reference generated from the code.

[Unreleased]: https://github.com/mattoddie/proxmox-mcp/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/mattoddie/proxmox-mcp/releases/tag/v0.1.0
