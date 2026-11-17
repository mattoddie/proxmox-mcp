# Changelog

All notable changes to this project are documented here. The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

For proxmox-mcp, the "public API" is the set of tool names, their arguments, the prompts and the configuration variables. Renaming or removing any of these is a breaking change.

## [Unreleased]

These changes align tool, argument and setting names with the other mattoddie MCP servers before 1.0. Old names are not accepted.

### Changed

- **Breaking:** tools renamed so the verb comes first: `proxmox_vm_power` → `proxmox_control_vm_power`, `proxmox_container_power` → `proxmox_control_container_power`, `proxmox_node_power` → `proxmox_control_node_power`, `proxmox_guest_power` → `proxmox_control_guest_power`, `proxmox_guest_fs_freeze` → `proxmox_control_guest_fs_freeze`, `proxmox_guest_fstrim` → `proxmox_trim_guest_filesystems`, `proxmox_guest_exec` → `proxmox_run_guest_command`, `proxmox_get_guest_exec_status` → `proxmox_get_guest_command_status`, `proxmox_guest_read_file` → `proxmox_read_guest_file`, `proxmox_guest_write_file` → `proxmox_write_guest_file`, `proxmox_guest_set_password` → `proxmox_set_guest_password`, `proxmox_ha_migrate` → `proxmox_migrate_ha_resource`, `proxmox_node_acme_certificate` → `proxmox_control_node_acme_certificate`, `proxmox_node_bulk_action` → `proxmox_run_node_bulk_action`, `proxmox_bulk_guest_action` → `proxmox_run_bulk_guest_action`, `proxmox_bulk_tag_guests` → `proxmox_tag_guests`, `proxmox_vm_monitor_command` → `proxmox_run_vm_monitor_command`, `proxmox_manage_node_service` → `proxmox_control_node_service` and `proxmox_run_replication_now` → `proxmox_run_replication_job`. Update any `PROXMOX_DENY_TOOLS` list that names them.
- **Breaking:** `PROXMOX_DEFAULT_NODE` is now `PROXMOX_NODE`, and `PROXMOX_ALLOW_UNREDACTED` is now `PROXMOX_ALLOW_SECRETS`. The Claude Desktop bundle's default node setting is stored under a new key, so set it again after updating.
- **Breaking:** the raw API tool's `redact: false` is now `include_secrets: true` (still only offered with `PROXMOX_ALLOW_SECRETS=true`).
- **Breaking:** counts are `limit`: `max` on `proxmox_get_cluster_log`, `log_lines` on `proxmox_get_task` and `lastentries` on `proxmox_get_node_journal`.
- **Breaking:** line offsets on the log tools are `offset` instead of `start`: `proxmox_get_node_syslog`, `proxmox_get_firewall_log`, `proxmox_get_ceph_log`, `proxmox_get_replication_log` and `proxmox_get_task_log` (whose result reports `offset` instead of `start`). `start` meaning "start the guest" is unchanged.
- **Breaking:** time windows are `from`/`to` instead of `since`/`until`, always an ISO 8601 date or date-time string (a date alone is midnight UTC, a time without an offset is proxmox-mcp's local time): `proxmox_get_node_syslog`, `proxmox_get_node_journal`, `proxmox_get_firewall_log` and `proxmox_list_tasks` (`from` only). The firewall log and task list took UNIX times before, and the syslog took journalctl strings such as `yesterday`; the syslog now converts the times to the node's local time.
- **Breaking:** seconds to wait are `timeout`: `timeout_s` on `proxmox_run_guest_command` and `proxmox_wait_for_task`, `wait_s` on `proxmox_get_guest_command_status`.
- **Breaking:** `proxmox_list_privileges` returns `{total, returned, items}` like the other list tools, one item per privilege with its area, instead of `count` and `by_area`. It gains `search` and `fields`.

### Fixed

- **Breaking:** boolean settings only accept `true`/`false`, `1`/`0`, `yes`/`no` and `on`/`off`, and anything else stops the server. Before, any other value meant `false`, so `PROXMOX_VERIFY_SSL=ture` silently turned certificate verification off and skipped the check that makes you choose how to verify an `https` connection. Number settings must be plain positive whole numbers: `PROXMOX_TIMEOUT_MS=30s` is now an error instead of 30.

## [0.6.0] - 2026-11-15

### Added

- **Import from ESXi and OVA/OVF.** `proxmox_get_import_metadata` reads what Proxmox recognises in an ESXi VM or an OVA/OVF appliance on an import storage (suggested settings, disks, NICs and warnings), and `proxmox_import_vm` (write) creates the VM from it, importing each disk onto a target storage with `import-from`. Proxmox VE 8.2+ (OVA/OVF 8.3+).
- **Capacity report.** `proxmox_get_capacity_report` compares what guests are allocated with the physical capacity per node and for the cluster: vCPUs against CPU threads, guest memory against RAM, and the provisioned size of guest disks against each storage's size (thin-provisioning overcommit), for running guests and for all guests.
- **`PROXMOX_DENY_TOOLS`**: comma-separated tool names never to register, whatever the toolsets and access level allow. An unknown name stops the server at startup. It's also a setting in the Claude Desktop bundle and the MCP Registry entry.
- **Tag management.** `proxmox_bulk_tag_guests` (write) adds and removes tags across many VMs and containers, chosen by VMID or by an existing tag (so it can rename a tag), with a dry run that lists every change. `proxmox_set_tag_options` (write) sets the datacenter's `tag-style` (shape, ordering, colours, case sensitivity), `user-tag-access` and `registered-tags`, changing only the fields given.
- proxmox-mcp now has 319 tools (up from 314) and 8 prompts.

### Security

- **Breaking:** without `PROXMOX_ALLOW_DELETES`, arguments that delete data are refused: `delete_source` on `proxmox_move_vm_disk` and `proxmox_move_container_volume`, `retention` and `remove_old=true` on `proxmox_backup_guests` and `proxmox_save_backup_job`, and `remove_vanished` with `entry` or `acl` on `proxmox_sync_realm` (also when it comes from the realm's sync defaults, unless `dry_run`) and `proxmox_save_realm_sync_job`. Backups made without delete access are sent with `remove=0`, because Proxmox prunes older backups by default: `proxmox_backup_guests` and `proxmox_run_backup_job` no longer prune, and new backup jobs are created with `remove_old=false`.
- **Breaking:** the raw API tool refuses the same without delete access: a VM `config` update with `force` (which deletes removed disks), `vzdump` without `remove=0` or with `prune-backups`, backup jobs that prune, and realm sync with `remove-vanished` containing `entry` or `acl`.
- Remote storage scans (`proxmox_scan_storage` with nfs, cifs, pbs or iscsi), `proxmox_list_acme_directories` with `directory`, `proxmox_query_url_metadata` and the matching raw API `GET`s make the node connect out, so with writes on they now take `dry_run` (which no longer sends them) and are recorded in the audit log. `proxmox_scan_storage` and `proxmox_list_acme_directories` are no longer marked read-only when writes are on.
- Secrets under generic names are now redacted in the audit log and dry-run previews: the private `key` of `proxmox_upload_node_certificate`, the subscription `key`, the PowerDNS `key` and the ACME plugin `data`. Field names with underscores, such as `client_key` and `encryption_key`, are matched like their hyphenated forms.
- `proxmox_save_storage`, `proxmox_save_user`, `proxmox_save_backup_job`, `proxmox_set_node_repository` and `proxmox_save_notification_endpoint` are now marked destructive, because their updates can overwrite or disable what's there.

### Changed

- The `go` directive in `go.mod` is now 1.26, since Go 1.25 is no longer supported. The toolchain stays Go 1.27.4.

## [0.5.2] - 2026-11-13

### Added

- Each release now has plain binaries for macOS, Linux and Windows on `amd64` and `arm64`, with a `SHA256SUMS` file and build provenance attestations, so you can run proxmox-mcp without Docker. See [Running without Docker](docs/deployment.md#running-without-docker).

### Security

- Built with Go 1.27.4, which fixes an HTTP/1 server crash in the standard library's `net/http` (GO-2026-6859).

## [0.5.1] - 2026-10-22

### Added

- **Listed in the MCP Registry** as `io.github.mattoddie/proxmox-mcp`. Each stable release publishes [`server.json`](server.json) to the [MCP Registry](https://registry.modelcontextprotocol.io), with the Docker image (run over stdio) and the Claude Desktop bundles, so clients that browse the registry can install proxmox-mcp and ask for its settings. The image gains the `io.modelcontextprotocol.server.name` label the registry checks, `make server-json VERSION=x.y.z` stamps the version and bundle checksums, and CI validates the file and checks its settings match the bundle's.

## [0.5.0] - 2026-10-22

### Added

- **Claude Desktop bundles.** Each release now has an MCP Bundle (`.mcpb`) for macOS, Windows and Linux on amd64 and arm64. Open it in Claude Desktop and fill in a settings form: URL, API token or username and password, certificate check, default node, the write, delete and exec switches, toolsets and audit log. No Docker needed. `make mcpb VERSION=x.y.z` builds them, and CI validates them with the `mcpb` CLI.
- **Dry runs.** Every tool that changes something, and the raw API tool, takes `dry_run`. With `true` the tool sends nothing that changes state and returns the request it would have sent. For updates of an object it can read, such as a guest config, a user or a firewall rule, it lists only the keys that would change, with their values before and after (secrets redacted); for deletes, the object that would be removed.
- **Audit log.** `PROXMOX_AUDIT_LOG` records every call of a tool that changes things (and of the raw API tool with a method other than `GET`) as a JSON line: time, tool, arguments with secrets redacted, whether it was a dry run, the requests sent with their HTTP status, and the outcome. Set it to a file path (created with mode `0600`) or `stderr`.
- **`fields` on every list tool**, to keep only the named fields in each item (dot paths reach nested ones). List tools that had no `search` gain it, and `proxmox_list_backup_files` gains `limit` (default 500).
- A variable set to exactly `${user_config.<key>}` now counts as unset, like an empty one. Claude Desktop passes that placeholder for an extension setting left empty.
- `docs/configuration.md` has a *Keeping the tool list small* section with each toolset's tool count and the `tools/list` size of common combinations.

### Changed

- **Breaking:** list tools now all return `{total, returned, items}`, plus their context such as `node`. These returned a plain array before: `proxmox_list_nodes`, `proxmox_list_realms`, `proxmox_list_tfa` (all users), `proxmox_list_realm_sync_jobs`, `proxmox_list_ceph_fs`, `proxmox_list_metric_servers`, `proxmox_list_ha_groups`, `proxmox_list_ha_rules`, `proxmox_list_custom_cpu_models`, `proxmox_list_acme_accounts`, `proxmox_list_acme_plugins`, `proxmox_list_notification_targets` and `proxmox_list_notification_matchers`. These kept their list under another key, which is now `items` (and a `count` is now `total`): `proxmox_list_tfa` with `userid` (`entries`), `proxmox_list_backup_files` (`entries`), `proxmox_list_firewall_rules` (`rules`), `proxmox_list_node_mdev_types` (`types`), `proxmox_list_acme_directories` (`directories`), `proxmox_list_zfs_pools` (`pools`), `proxmox_list_lvm` (`volume_groups`), `proxmox_list_lvm_thin` (`thin_pools`), `proxmox_list_directory_mounts` (`directories`), `proxmox_list_disks` (`disks`) and `proxmox_list_snapshots` (`snapshots`). `proxmox_list_firewall_refs`, `proxmox_list_unbacked_guests` and `proxmox_list_replication_jobs` gain `returned`.
- The server instructions explain the list arguments and, with writes on, `dry_run`.

### Security

- Built with Go 1.27.3, which fixes a `crypto/x509` vulnerability in the standard library (GO-2026-6809).

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

[Unreleased]: https://github.com/mattoddie/proxmox-mcp/compare/v0.6.0...HEAD
[0.6.0]: https://github.com/mattoddie/proxmox-mcp/compare/v0.5.2...v0.6.0
[0.5.2]: https://github.com/mattoddie/proxmox-mcp/compare/v0.5.1...v0.5.2
[0.5.1]: https://github.com/mattoddie/proxmox-mcp/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/mattoddie/proxmox-mcp/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/mattoddie/proxmox-mcp/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/mattoddie/proxmox-mcp/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/mattoddie/proxmox-mcp/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/mattoddie/proxmox-mcp/releases/tag/v0.1.0
