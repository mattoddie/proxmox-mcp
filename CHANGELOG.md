# Changelog

All notable changes to this project are documented here. The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

For proxmox-mcp, the "public API" is the set of tool names, their arguments, the prompts and the configuration variables. Renaming or removing any of these is a breaking change.

## [Unreleased]

## [0.3.0] - 2026-10-16

### Changed

- The server is rewritten in Go, on the official Go MCP SDK. There are no behaviour changes: tool names, titles, descriptions, arguments, access levels and annotations, the prompts, the configuration variables, and tool output and error messages are the same as in 0.2.0, which a reference test checks against what the TypeScript server listed.
- The Docker image is now a static binary on distroless (`gcr.io/distroless/static-debian13:nonroot`): no shell, package manager or Node.js, and much smaller. It runs as UID 65532 instead of 1000, so files you mount (such as `PROXMOX_CA_FILE` or `_FILE` secrets) must be readable by that user. The environment defaults, port and health check behaviour are unchanged.
- Without Docker, proxmox-mcp is a single binary (`proxmox-mcp`, with `version` and `healthcheck` subcommands) instead of a Node.js 22+ program.
- A few protocol details differ slightly, with no effect on clients: `tools/list` and `prompts/list` are sorted by name; defaults are left out of listings (`readOnlyHint: false`, `required: false` on optional prompt arguments, `execution.taskSupport: "forbidden"`); calling an unknown tool returns a JSON-RPC error instead of an error result; a `/mcp` request without the `Accept: application/json, text/event-stream` header gets HTTP 400 instead of 406; and sorting by name uses a close approximation of the JavaScript locale collation.

## [0.2.0] - 2026-10-15

### Security

- The raw API tool (`proxmox_api_request`) could be steered past its exec check, even in read-only mode: URL parsing strips tabs and newlines, so a path such as `…/agent/file-re<TAB>ad` was checked as harmless but sent as `…/agent/file-read`. Paths with control characters are now refused, and the checks also run on the normalised path that is actually sent, which must stay under `/api2/json`.
- The raw API tool's exec gate now also covers `sendkey`, `vncwebsocket`, `mtunnelwebsocket` and every guest agent endpoint (not just exec and file access), and matches paths case-insensitively.
- `extra` could override parameters a tool had already checked: `proxmox_restore_backup` with `extra: {vmid, force}` restored over a different, existing guest, and `proxmox_create_vm`/`proxmox_create_container` with `extra: {archive|restore, force}` overwrote an existing guest without delete permission. See *Changed*.
- **Breaking:** the HTTP transport no longer starts without `MCP_AUTH_TOKEN`. Set `MCP_ALLOW_UNAUTHENTICATED=true` only if something in front of the server authenticates every request; the server then logs a warning.
- With `MCP_ALLOWED_HOSTS` set, requests whose `Origin` header names another host are now rejected as well.
- New `PROXMOX_TLS_FINGERPRINT` pins the Proxmox server certificate by its SHA-256 fingerprint, so the default self-signed certificate can be verified. The server logs a startup warning when TLS verification is off and no fingerprint or CA is configured.
- The raw API tool now redacts the DNS API credentials in ACME plugin configs (`data`), keeping only the key names.
- `proxmox_scan_storage` for `nfs`, `cifs`, `pbs` and `iscsi` makes the node connect to a server of the caller's choosing, possibly with credentials, so it now needs write access. Local `lvm`, `lvmthin` and `zfs` scans stay read-only.
- `proxmox_init_disk_gpt` destroys a disk's partition table and now needs delete access, like `proxmox_wipe_disk`.
- The Docker image is built on Node.js 24 LTS instead of Node.js 25, which reaches end of life on 2026-06-01.

### Changed

- **Breaking:** `extra` can no longer override a tool's typed arguments. Keys that a typed argument maps to (even when it isn't given) are refused with `extra must not set …`, as are reserved keys such as `archive`, `restore`, `force` and `unique` on the create tools and the guest selection on backups. Pass these through the typed arguments (use `proxmox_restore_backup` to restore).
- **Breaking:** `MCP_HTTP_HOST` now defaults to `127.0.0.1` when running outside Docker. The Docker image still listens on `0.0.0.0`.
- `MCP_ALLOWED_HOSTS` entries are compared case-insensitively.
- `docs/security.md` explains why writes without exec aren't a hard boundary for a privileged token (QEMU `args`, container bind mounts, `cicustom`, hook scripts).
- Dependabot no longer proposes odd-numbered (non-LTS) Node.js majors for the Docker image.

### Fixed

- A response body that stalled or broke off after the headers was reported as a bare `TimeoutError`/`terminated` error instead of `Timed out after … talking to …`.

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

[Unreleased]: https://github.com/mattoddie/proxmox-mcp/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/mattoddie/proxmox-mcp/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/mattoddie/proxmox-mcp/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/mattoddie/proxmox-mcp/releases/tag/v0.1.0
