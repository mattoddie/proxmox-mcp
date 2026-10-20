# Changelog

All notable changes to this project are documented here. The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

For proxmox-mcp, the "public API" is the set of tool names, their arguments, the prompts and the configuration variables. Renaming or removing any of these is a breaking change.

## [Unreleased]

## [0.4.0] - 2026-10-20

### Security

- **Breaking:** the raw API tool (`proxmox_api_request`) is no longer enabled by default. `PROXMOX_TOOLSETS=all` (the default) now means every toolset except `raw`, because the raw tool can reach endpoints that the curated tools gate more finely. To keep using it, list it by name, for example `PROXMOX_TOOLSETS=all,raw`.
- With writes on and deletes off, the raw API tool could still destroy data without a `DELETE`: `PUT …/disks/wipedisk`, `POST …/disks/initgpt`, restoring over an existing guest (`POST /nodes/{node}/qemu|lxc` with `force`), deleting `unusedN` volumes through a config `delete` list, `unlink` with `force`, `move_disk`/`move_volume` with `delete`, `purge`/`destroy-unreferenced-disks`, and `remote_migrate` (which copies a guest to another cluster). These now need `PROXMOX_ALLOW_DELETES`, like the curated tools that do the same.
- Read-only, the raw API tool could make a node connect to a server of the caller's choosing with a `GET`: remote storage scans (`scan/nfs`, `cifs`, `pbs`, `iscsi`, `glusterfs`, which `proxmox_scan_storage` already gated), `query-url-metadata` and ACME `meta`/`tos` with a `directory`. These now need `PROXMOX_ALLOW_WRITES`.
- The raw API tool now needs `PROXMOX_ALLOW_EXEC` for backup `file-restore/download` (it reads files out of a guest's backup) and for any request that sets `hookscript` or vzdump `script`, as the curated tools already did.
- **Breaking:** `proxmox_query_url_metadata` is now a write tool (`PROXMOX_ALLOW_WRITES`), because the node fetches any URL it is given, including internal addresses. `proxmox_list_acme_directories` still lists the known directories read-only, but fetching a `directory`'s metadata now needs write access for the same reason.
- The check that `proxmox_update_vm_config` and `proxmox_update_container_config` need delete access to delete an `unusedN` entry (which destroys its volume) could be bypassed by separating keys with a vertical tab or NUL, which Proxmox also splits on. The VM, container and raw tools now share one check that splits the list on anything that can't be part of a key.
- Request parameters may no longer contain control characters other than tab, newline and carriage return; such requests are refused before they are sent. Send binary guest file content base64-encoded.
- **Breaking:** the raw API tool's `redact: false` argument let the model turn secret redaction off by itself. It now only exists when the operator sets the new `PROXMOX_ALLOW_UNREDACTED=true`; otherwise every raw response is redacted.
- **Breaking:** `MCP_ALLOW_UNAUTHENTICATED=true` now also requires `MCP_ALLOWED_HOSTS`, and the server refuses to start without it. With no token, the Host allow-list is the only thing stopping a web page from reaching the server through DNS rebinding.
- **Breaking:** `MCP_AUTH_TOKEN` must be at least 16 characters long. `openssl rand -hex 32` gives 64.
- `MCP_ALLOWED_HOSTS` port handling is stricter for `Origin`: an entry without a port now only matches the scheme's default port, so `localhost` no longer lets a page on `http://localhost:3000` through; list `localhost:3000` to allow it. `Host` entries without a port still match any port, and entries with a port match only that port. More than one `Origin` header is rejected.
- The HTTP server now has a 60-second read timeout and a 120-second idle timeout, so slow or idle clients can't hold connections open indefinitely. There is still no write timeout, because a tool waiting for a Proxmox task can take minutes.
- **Breaking:** with an `https://` `PROXMOX_URL`, the server no longer connects without verifying the certificate by default. If `PROXMOX_VERIFY_SSL` is unset and neither `PROXMOX_TLS_FINGERPRINT` nor `PROXMOX_CA_FILE` is set, it refuses to start and explains the options. Pin the certificate with `PROXMOX_TLS_FINGERPRINT` (recommended), trust a CA with `PROXMOX_CA_FILE`, set `PROXMOX_VERIFY_SSL=true` for a trusted certificate, or set `PROXMOX_VERIFY_SSL=false` to keep the old behaviour (with the startup warning). `example.env` no longer sets `PROXMOX_VERIFY_SSL=false`.
- `PROXMOX_URL` may no longer contain a user name or password (`user:pass@`), and a plain `http://` URL now logs a warning that credentials travel in cleartext.
- After Proxmox rejects the username and password, the server waits before logging in again (5 seconds, doubling up to 5 minutes) instead of retrying on every tool call. It still logs in again only after a `401`, never a `403`.
- Proxmox responses larger than 64 MiB are refused instead of read into memory, and JSON nested more than 256 levels deep is rejected.
- Proxmox API errors no longer quote the body of a non-JSON response (only its status, content type and size), so a page served by something other than Proxmox isn't passed on to the model.

### Changed

- Built with Go 1.27 (`toolchain go1.27.2`, matching the `golang:1.27` build image). The Dockerfile pins both base images by digest, and CI runs `govulncheck` (pinned to v1.9.0).

### Fixed

- `PROXMOX_PASSWORD` was trimmed, so a password starting or ending with a space couldn't log in. It is now used as given; from `PROXMOX_PASSWORD_FILE` only the final line break is removed.
- `proxmox-mcp healthcheck` (the Docker health check) always connected to `127.0.0.1`, so it failed for a server listening on a specific address. It now connects to `MCP_HTTP_HOST`, or to loopback when that is unset, `0.0.0.0` or `::`, and honours `MCP_HTTP_PORT_FILE` and `MCP_HTTP_HOST_FILE`.

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

[Unreleased]: https://github.com/mattoddie/proxmox-mcp/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/mattoddie/proxmox-mcp/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/mattoddie/proxmox-mcp/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/mattoddie/proxmox-mcp/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/mattoddie/proxmox-mcp/releases/tag/v0.1.0
