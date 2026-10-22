# Configuration

proxmox-mcp is configured entirely through environment variables. With Docker Compose, put them in `.env`. Copy [`example.env`](../example.env) to get started.

- [Proxmox connection](#proxmox-connection)
- [Authentication](#authentication)
- [What the assistant may do](#what-the-assistant-may-do)
- [Audit log](#audit-log)
- [MCP transport](#mcp-transport)
- [Docker secrets (`_FILE` variables)](#docker-secrets-_file-variables)
- [Keeping the tool list small](#keeping-the-tool-list-small)
- [Example configurations](#example-configurations)

The server checks the configuration at startup. If something is wrong, it exits with a clear message such as `PROXMOX_ALLOW_DELETES=true also requires PROXMOX_ALLOW_WRITES=true`. If the container keeps restarting, check `docker compose logs`.

## Proxmox connection

| Variable | Default | Description |
|---|---|---|
| `PROXMOX_URL` | **required** | URL of any cluster node, or a load balancer in front of them, including the port: `https://pve1.example.lan:8006`. A trailing `/api2/json` is removed if you include it. It must not contain a user name or password. `http://` works but logs a warning, because the token or password then travels in cleartext. |
| `PROXMOX_TLS_FINGERPRINT` | | **Recommended with the default self-signed certificate.** The SHA-256 fingerprint of the node's certificate, as `AA:BB:…` (32 pairs) or 64 hex digits. The server then only talks to a node presenting that certificate, whether or not `PROXMOX_VERIFY_SSL` is on. Separate several with commas, for example the old and new certificate during a renewal, or each node's certificate behind a load balancer. See [Verifying the Proxmox certificate](#verifying-the-proxmox-certificate). |
| `PROXMOX_VERIFY_SSL` | unset | Verify the node's certificate chain and hostname. Set it to `true` if the node has a trusted certificate (for example from ACME). Proxmox ships a self-signed certificate, so with an `https://` URL the server **refuses to start** unless you choose how to check it: `PROXMOX_TLS_FINGERPRINT` (recommended), `PROXMOX_CA_FILE`, `PROXMOX_VERIFY_SSL=true`, or `PROXMOX_VERIFY_SSL=false` to skip verification. With `false` and no fingerprint or CA, the server logs a warning at startup. |
| `PROXMOX_CA_FILE` | | Path to a PEM file with the CA certificate(s) to trust (in addition to the system CAs). Setting it turns certificate verification on, whatever `PROXMOX_VERIFY_SSL` says. Use the cluster's own CA, `/etc/pve/pve-root-ca.pem` on any node, to verify the default node certificates. |
| `PROXMOX_TIMEOUT_MS` | `30000` | Timeout for each API request, in milliseconds. |
| `PROXMOX_TASK_TIMEOUT_MS` | `120000` | How long tools wait for a Proxmox task (start, clone, migrate, backup…) to finish before returning. A task that takes longer keeps running and the tool returns its task ID. See [Usage](usage.md#tasks-and-wait). |
| `PROXMOX_DEFAULT_NODE` | | Node used by node-scoped tools when a call doesn't name one. Single-node installs don't need it. In a cluster without it, those tools ask for a node. |

A variable set to an empty value counts as unset and gets its default. So does one set to exactly `${user_config.<key>}`, which is how Claude Desktop passes an [extension setting](clients.md#claude-desktop) that was left empty.

### Verifying the Proxmox certificate

Without verification, a machine that can intercept the traffic between proxmox-mcp and the node can read the API token or password. With an `https://` URL the server won't start until you pick one of these:

- **Pin the certificate (simplest).** Copy the SHA-256 fingerprint from **Node → System → Certificates**, select the certificate the web UI uses (`pveproxy-ssl.pem` if there is one, otherwise `pve-ssl.pem`), **View Certificate**. Or run this on the node:

  ```sh
  openssl x509 -in /etc/pve/local/pve-ssl.pem -noout -fingerprint -sha256
  ```

  Then set `PROXMOX_TLS_FINGERPRINT=AA:BB:…`. This is how Proxmox's own clients trust self-signed certificates, for example a Proxmox Backup Server storage. When the certificate changes (renewal, a new ACME certificate, a reinstalled node), update the fingerprint, or list the old and new ones while you switch over.
- **Trust the cluster CA** with `PROXMOX_CA_FILE=/path/to/pve-root-ca.pem` (copied from `/etc/pve/pve-root-ca.pem`). Covers every node's default certificate.
- **Use a trusted certificate** (for example from Proxmox's built-in ACME support) and set `PROXMOX_VERIFY_SSL=true`.
- **Skip verification** with `PROXMOX_VERIFY_SSL=false`. Only on a network you trust: the server logs a warning at startup.

Boolean variables accept `true`/`false`, `1`/`0`, `yes`/`no` and `on`/`off`. Anything else counts as `false`.

## Authentication

Set **either** an API token **or** a username and password. If both are set, the token is used.

| Variable | Description |
|---|---|
| `PROXMOX_TOKEN_ID` | The full token ID, `user@realm!tokenname`, for example `mcp@pve!mcp`. |
| `PROXMOX_TOKEN_SECRET` | The token secret (a UUID). |
| `PROXMOX_API_TOKEN` | Both in one value, the way Proxmox shows it: `mcp@pve!mcp=xxxxxxxx-…`. A leading `PVEAPIToken=` is accepted. Overrides the two variables above. |
| `PROXMOX_USERNAME` | User for password authentication, as `user@realm` (for example `mcp@pve` or `root@pam`). |
| `PROXMOX_PASSWORD` | Password for that user. |
| `PROXMOX_REALM` | Realm added to `PROXMOX_USERNAME` when it has no `@realm`. Default `pam`. |

How each method works:

- **API token** (recommended): every request sends an `Authorization: PVEAPIToken=…` header. There are no sessions or logins. See [Getting started](getting-started.md#2-create-a-user-and-api-token) for creating one with least privilege.
- **Username and password:** the server logs in on first use and keeps the ticket and CSRF token. Proxmox tickets last two hours, so the server logs in again after 90 minutes, and also logs in again and retries once if a request gets `401`. Several requests arriving together share a single login. Accounts with two-factor authentication can't log in this way. Use a token for them.

## What the assistant may do

| Variable | Default | Description |
|---|---|---|
| `PROXMOX_ALLOW_WRITES` | `false` | Register tools that change state: starting and stopping guests, creating, cloning, migrating and reconfiguring them, snapshots, backups, firewall, storage and more. Also needed for anything that makes a node connect to a server you name: remote storage scans, `proxmox_query_url_metadata` and ACME directory lookups. |
| `PROXMOX_ALLOW_DELETES` | `false` | Also register tools that delete guests, snapshots, backups, volumes and configuration. Requires `PROXMOX_ALLOW_WRITES=true`. |
| `PROXMOX_ALLOW_EXEC` | `false` | Also register tools that run commands or read and write files inside VMs through the QEMU guest agent, and send QEMU monitor commands. Also allows the equivalent endpoints in the raw API tool (every guest agent endpoint, consoles and shells, `sendkey`, the monitor and migration tunnels). Requires `PROXMOX_ALLOW_WRITES=true`. |
| `PROXMOX_ALLOW_UNREDACTED` | `false` | Offer the raw API tool's `redact: false` argument, which returns responses without secret redaction. Off, every response is redacted whatever the model asks for. |
| `PROXMOX_TOOLSETS` | `all` | Comma-separated list of [toolsets](#toolsets) to expose. See [Keeping the tool list small](#keeping-the-tool-list-small). `all` means every toolset except `raw`, which is only enabled when listed by name (e.g. `all,raw`). |

That gives four access levels. The [tool reference](tools.md) labels every tool with the level it needs:

| Label | Registered when |
|---|---|
| 🟢 read | Always |
| 🟠 write | `PROXMOX_ALLOW_WRITES=true` |
| 🔴 delete | `PROXMOX_ALLOW_WRITES=true` and `PROXMOX_ALLOW_DELETES=true` |
| 🟣 exec | `PROXMOX_ALLOW_WRITES=true` and `PROXMOX_ALLOW_EXEC=true` |

Deletes and exec are independent of each other: you can allow either without the other.

Tools that aren't allowed are **not registered at all**. The assistant can't see them, so it can't call them or be talked into calling them. Proxmox still checks the token's own permissions on every request, so a write tool fails with `403` if the token isn't allowed to do it.

Every tool that changes something also takes `dry_run`. With `dry_run: true` the tool sends nothing that changes state and returns the request it would have sent, and for an update of an object it can read, such as a guest config, the keys that would change with their values before and after. See [Previewing changes](usage.md#previewing-changes-with-dry_run).

## Audit log

| Variable | Default | Description |
|---|---|---|
| `PROXMOX_AUDIT_LOG` | (off) | Record every call of a tool that changes things. A file path appends JSON Lines to that file, which is created with mode `0600` if it doesn't exist; its directory must exist, or the server won't start. `stderr` writes the lines to standard error, next to the server's other logs. |

Each line is one call of a write, delete or exec tool, or of `proxmox_api_request` with a method other than `GET`. Read-only tools aren't logged. Dry runs, calls that failed and calls refused for invalid arguments are all logged:

```json
{"time":"2026-10-22T09:14:03.120Z","tool":"proxmox_update_vm_config","dry_run":false,"arguments":{"vmid":100,"memory_mb":8192,"extra":{"cipassword":"<redacted>"}},"requests":[{"method":"POST","path":"/nodes/pve1/qemu/100/config","status":200}],"outcome":"ok"}
```

| Field | Meaning |
|---|---|
| `time` | When the call finished, in UTC (RFC 3339). |
| `tool` | The tool name. |
| `dry_run` | Whether the call was a [dry run](usage.md#previewing-changes-with-dry_run). |
| `arguments` | The arguments, with passwords, keys and other secrets `<redacted>`. |
| `requests` | The requests that change state that were actually sent: method, path and HTTP status (`null` if no response arrived). Reads aren't listed, and nothing from the responses is logged. |
| `previewed` | For a dry run, the request that was previewed and not sent. |
| `outcome` | `ok` or `error`. |
| `error` | The error message, when `outcome` is `error`. |

A file is opened for each line, so it's safe to rotate it or delete it: the next line creates it again. In Docker, mount a writable directory and point `PROXMOX_AUDIT_LOG` at a file in it (the image runs as UID 65532). If a line can't be written, the server logs one warning and carries on.

## MCP transport

| Variable | Default | Description |
|---|---|---|
| `MCP_TRANSPORT` | `http` in the Docker image (`stdio` when the binary is run directly) | `http` for Streamable HTTP, or `stdio`. |
| `MCP_HTTP_HOST` | `127.0.0.1` (`0.0.0.0` in the Docker image) | Address to listen on (HTTP only). Outside Docker the server only listens on localhost unless you set this. |
| `MCP_HTTP_PORT` | `8080` | Port to listen on (HTTP only). |
| `MCP_AUTH_TOKEN` | **required** for `http` | Every request to `/mcp` must send `Authorization: Bearer <token>`. With the `http` transport the server refuses to start without it, unless `MCP_ALLOW_UNAUTHENTICATED=true`, and refuses tokens shorter than 16 characters. Generate one with `openssl rand -hex 32`. Not needed for `stdio`. |
| `MCP_ALLOW_UNAUTHENTICATED` | `false` | Serve `/mcp` without a token. **Only** set this when something else in front of the server authenticates every request, such as an authenticating reverse proxy, and nothing else can reach the port. It also requires `MCP_ALLOWED_HOSTS` (the server refuses to start without it), because without a token the Host check is what stops DNS rebinding. The server logs a warning at startup when it's on. |
| `MCP_ALLOWED_HOSTS` | | Comma-separated allow-list of hosts, as `host` or `host:port`, such as `proxmox-mcp.lan,localhost` (case-insensitive). Requests with any other `Host` header, and requests with an `Origin` header (sent by browsers) that isn't on the list, are rejected. For `Host`, an entry without a port matches any port. For `Origin`, an entry without a port only matches the scheme's default port (`http://localhost` but not `http://localhost:3000`); list `host:port` to allow a page on another port. An empty or `null` `Origin` is rejected. This protects against DNS-rebinding and cross-site attacks from web pages in your browser. |

The HTTP server exposes these endpoints:

| Path | Method | Purpose |
|---|---|---|
| `/mcp` | `POST` | The MCP endpoint (stateless Streamable HTTP, JSON responses). Other methods return `405`. Request bodies are limited to 4 MiB. |
| `/health`, `/healthz` | `GET` | Liveness check. Returns `{"status":"ok"}` and doesn't contact Proxmox. |

## Docker secrets (`_FILE` variables)

Every variable can also be read from a file: set `<NAME>_FILE` to the file's path. Leading and trailing whitespace is trimmed, except for `PROXMOX_PASSWORD_FILE`, where only the final line break is removed so a password can start or end with a space (`PROXMOX_PASSWORD` itself is never trimmed either). This works with [Docker secrets](https://docs.docker.com/compose/how-tos/use-secrets/) and Kubernetes secret volumes:

```yaml
services:
  proxmox-mcp:
    image: ghcr.io/mattoddie/proxmox-mcp:latest
    environment:
      PROXMOX_URL: https://pve1.example.lan:8006
      PROXMOX_TOKEN_ID: mcp@pve!mcp
      PROXMOX_TOKEN_SECRET_FILE: /run/secrets/proxmox_token_secret
      MCP_AUTH_TOKEN_FILE: /run/secrets/mcp_auth_token
    secrets: [proxmox_token_secret, mcp_auth_token]
    ports: ["8080:8080"]

secrets:
  proxmox_token_secret:
    file: ./secrets/proxmox_token_secret.txt
  mcp_auth_token:
    file: ./secrets/mcp_auth_token.txt
```

If both `NAME` and `NAME_FILE` are set, the file wins. `PROXMOX_CA_FILE` is already a path, so mount the certificate as a secret or volume and point it there.

## Keeping the tool list small

Most MCP clients send every tool's name, description and argument schema to the model with each request. With every toolset enabled that's about 131 KB for the 154 read-only tools, 344 KB for 261 tools with writes on, and 397 KB for all 313 tools with deletes and exec on too. That uses up context you'd rather spend on your conversation and your cluster's data, and some clients warn about or cap the number of tools.

`PROXMOX_TOOLSETS` is the way to keep the list small. Enable only the toolsets you need: the model sees fewer tools, picks the right one more easily, and can't reach the rest at all.

Clients with their own tool search, such as Claude Code, load tool definitions only when the model needs them, so they handle the full list fine. You don't need to narrow `PROXMOX_TOOLSETS` for them, though it still limits what the model can reach.

### Toolsets

| Toolset | Tools | Use it for |
|---|---:|---|
| `cluster` | 15 | Cluster overview, nodes and guests at a glance. **Always include it.** |
| `nodes` | 49 | Node status, services, network, updates, certificates, logs |
| `vms` | 20 | QEMU virtual machines |
| `containers` | 16 | LXC containers |
| `agent` | 13 | Information from inside VMs via the guest agent, and (with exec) commands |
| `snapshots` | 7 | Guest snapshots |
| `storage` | 25 | Storage, volumes, ISOs and templates, physical disks, ZFS, LVM |
| `backup` | 16 | Backup jobs, backups, restores |
| `tasks` | 5 | Task history and logs. **Include it whenever you allow writes**, so the assistant can follow tasks. |
| `access` | 33 | Users, groups, roles, ACLs, tokens, realms, 2FA |
| `firewall` | 21 | Datacenter, node and guest firewall |
| `ha` | 15 | High availability |
| `sdn` | 18 | Software-defined networking |
| `ceph` | 27 | Ceph |
| `pools` | 5 | Resource pools |
| `replication` | 6 | Storage replication |
| `notifications` | 10 | Notification targets and matchers |
| `hardware` | 12 | PCI/USB devices, mappings, CPU models |
| `raw` | 1 | Any API path. Handy, but gives broad access. **Not part of `all`**: list it explicitly (`all,raw`) to enable it. |

The counts include write, delete and exec tools, so a read-only setup registers far fewer tools. The [tool reference](tools.md) lists the tools in each toolset.

### Ready-made combinations

The sizes are the `tools/list` result the client receives, measured by starting the server. "With writes" is `PROXMOX_ALLOW_WRITES=true` alone; "with deletes" adds `PROXMOX_ALLOW_DELETES=true`.

| Goal | `PROXMOX_TOOLSETS` | Read-only | With writes | With deletes |
|---|---|---:|---:|---:|
| Everyday monitoring | `cluster,nodes,vms,containers,storage,backup,tasks` | 76 tools, 67 KB | 127 tools, 178 KB | 144 tools, 196 KB |
| Running guests day to day | `cluster,vms,containers,agent,snapshots,backup,tasks` | 45 tools, 42 KB | 77 tools, 119 KB | 85 tools, 129 KB |
| Backup and snapshot hygiene | `cluster,snapshots,backup,storage,tasks` | 40 tools, 36 KB | 58 tools, 74 KB | 68 tools, 86 KB |
| Security review | `cluster,nodes,access,firewall,tasks` | 65 tools, 52 KB | 105 tools, 118 KB | 123 tools, 133 KB |
| Network and SDN | `cluster,nodes,sdn,firewall` | 53 tools, 46 KB | 90 tools, 113 KB | 103 tools, 126 KB |
| Everything except the raw API tool (default) | `all` | 154 tools, 131 KB | 261 tools, 344 KB | 306 tools, 388 KB |
| Everything, including the raw API tool | `all,raw` | 155 tools, 133 KB | 262 tools, 346 KB | 307 tools, 389 KB |

Turning on exec as well adds the guest agent and monitor tools: `all` grows to 313 tools and 397 KB.

List tools also take `search`, `fields` and, on long lists, `limit`, so the model can fetch only the items and fields it needs instead of whole lists.

## Example configurations

**Read-only monitoring** (the safest setup):

```sh
PROXMOX_URL=https://pve1.example.lan:8006
PROXMOX_API_TOKEN=mcp@pve!mcp=xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
MCP_AUTH_TOKEN=...
PROXMOX_TOOLSETS=cluster,nodes,vms,containers,storage,backup,tasks
```

**Day-to-day admin.** Changes are allowed, but nothing can be deleted and nothing runs inside guests:

```sh
PROXMOX_URL=https://pve1.example.lan:8006
PROXMOX_API_TOKEN=mcp@pve!mcp=...
MCP_AUTH_TOKEN=...
PROXMOX_ALLOW_WRITES=true
PROXMOX_TASK_TIMEOUT_MS=300000   # wait up to 5 minutes for clones and migrations
```

**Single node with its self-signed certificate pinned:**

```sh
PROXMOX_URL=https://192.168.1.10:8006
PROXMOX_API_TOKEN=mcp@pve!mcp=...
PROXMOX_TLS_FINGERPRINT=AB:CD:EF:...   # from Node > System > Certificates
MCP_AUTH_TOKEN=...
```

**Cluster with a default node and a verified certificate:**

```sh
PROXMOX_URL=https://pve1.example.lan:8006
PROXMOX_API_TOKEN=mcp@pve!mcp=...
PROXMOX_VERIFY_SSL=true
PROXMOX_CA_FILE=/certs/pve-root-ca.pem   # copied from /etc/pve/pve-root-ca.pem
PROXMOX_DEFAULT_NODE=pve1
MCP_AUTH_TOKEN=...
```

**Password authentication** (for a quick test; prefer a token):

```sh
PROXMOX_URL=https://192.168.1.10:8006
PROXMOX_USERNAME=mcp@pve
PROXMOX_PASSWORD=...
MCP_AUTH_TOKEN=...
```
