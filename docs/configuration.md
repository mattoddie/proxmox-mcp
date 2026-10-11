# Configuration

proxmox-mcp is configured entirely through environment variables. With Docker Compose, put them in `.env`. Copy [`example.env`](../example.env) to get started.

- [Proxmox connection](#proxmox-connection)
- [Authentication](#authentication)
- [What the assistant may do](#what-the-assistant-may-do)
- [MCP transport](#mcp-transport)
- [Docker secrets (`_FILE` variables)](#docker-secrets-_file-variables)
- [Choosing toolsets](#choosing-toolsets)
- [Example configurations](#example-configurations)

The server checks the configuration at startup. If something is wrong, it exits with a clear message such as `PROXMOX_ALLOW_DELETES=true also requires PROXMOX_ALLOW_WRITES=true`. If the container keeps restarting, check `docker compose logs`.

## Proxmox connection

| Variable | Default | Description |
|---|---|---|
| `PROXMOX_URL` | **required** | URL of any cluster node, or a load balancer in front of them, including the port: `https://pve1.example.lan:8006`. A trailing `/api2/json` is removed if you include it. |
| `PROXMOX_VERIFY_SSL` | `false` | Verify the node's TLS certificate. Leave it `false` for the self-signed certificate Proxmox ships with. Set it to `true` if the node has a trusted certificate (for example from ACME), or set `PROXMOX_CA_FILE` instead. |
| `PROXMOX_CA_FILE` | | Path to a PEM file with the CA certificate(s) to trust (in addition to the system CAs). Setting it turns certificate verification on, whatever `PROXMOX_VERIFY_SSL` says. Use the cluster's own CA, `/etc/pve/pve-root-ca.pem` on any node, to verify the default node certificates. |
| `PROXMOX_TIMEOUT_MS` | `30000` | Timeout for each API request, in milliseconds. |
| `PROXMOX_TASK_TIMEOUT_MS` | `120000` | How long tools wait for a Proxmox task (start, clone, migrate, backup…) to finish before returning. A task that takes longer keeps running and the tool returns its task ID. See [Usage](usage.md#tasks-and-wait). |
| `PROXMOX_DEFAULT_NODE` | | Node used by node-scoped tools when a call doesn't name one. Single-node installs don't need it. In a cluster without it, those tools ask for a node. |

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
| `PROXMOX_ALLOW_WRITES` | `false` | Register tools that change state: starting and stopping guests, creating, cloning, migrating and reconfiguring them, snapshots, backups, firewall, storage and more. |
| `PROXMOX_ALLOW_DELETES` | `false` | Also register tools that delete guests, snapshots, backups, volumes and configuration. Requires `PROXMOX_ALLOW_WRITES=true`. |
| `PROXMOX_ALLOW_EXEC` | `false` | Also register tools that run commands or read and write files inside VMs through the QEMU guest agent, and send QEMU monitor commands. Also allows the equivalent endpoints in the raw API tool. Requires `PROXMOX_ALLOW_WRITES=true`. |
| `PROXMOX_TOOLSETS` | `all` | Comma-separated list of [toolsets](#choosing-toolsets) to expose. |

That gives four access levels. The [tool reference](tools.md) labels every tool with the level it needs:

| Label | Registered when |
|---|---|
| 🟢 read | Always |
| 🟠 write | `PROXMOX_ALLOW_WRITES=true` |
| 🔴 delete | `PROXMOX_ALLOW_WRITES=true` and `PROXMOX_ALLOW_DELETES=true` |
| 🟣 exec | `PROXMOX_ALLOW_WRITES=true` and `PROXMOX_ALLOW_EXEC=true` |

Deletes and exec are independent of each other: you can allow either without the other.

Tools that aren't allowed are **not registered at all**. The assistant can't see them, so it can't call them or be talked into calling them. Proxmox still checks the token's own permissions on every request, so a write tool fails with `403` if the token isn't allowed to do it.

## MCP transport

| Variable | Default | Description |
|---|---|---|
| `MCP_TRANSPORT` | `http` in the Docker image (`stdio` when run with Node directly) | `http` for Streamable HTTP, or `stdio`. |
| `MCP_HTTP_HOST` | `0.0.0.0` | Address to listen on (HTTP only). |
| `MCP_HTTP_PORT` | `8080` | Port to listen on (HTTP only). |
| `MCP_AUTH_TOKEN` | | When set, every request to `/mcp` must send `Authorization: Bearer <token>`. **Set this whenever anything other than you can reach the port.** |
| `MCP_ALLOWED_HOSTS` | | Comma-separated allow-list of `Host` header values, such as `proxmox-mcp.lan,proxmox-mcp.lan:8080`. Requests with any other `Host` are rejected, which protects against DNS-rebinding attacks from web pages in your browser. |

The HTTP server exposes these endpoints:

| Path | Method | Purpose |
|---|---|---|
| `/mcp` | `POST` | The MCP endpoint (stateless Streamable HTTP, JSON responses). Other methods return `405`. Request bodies are limited to 4 MiB. |
| `/health`, `/healthz` | `GET` | Liveness check. Returns `{"status":"ok"}` and doesn't contact Proxmox. |

## Docker secrets (`_FILE` variables)

Every variable can also be read from a file: set `<NAME>_FILE` to the file's path. Leading and trailing whitespace is trimmed. This works with [Docker secrets](https://docs.docker.com/compose/how-tos/use-secrets/) and Kubernetes secret volumes:

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

## Choosing toolsets

Every tool's name, description and argument schema is sent to the model, which uses up context. With every toolset and every access level enabled there are over 300 tools, which is a lot of text, and some clients warn about or cap the number of tools. Expose only what you need:

| Toolset | Use it for |
|---|---|
| `cluster` | Cluster overview, nodes and guests at a glance. **Always include it.** |
| `nodes` | Node status, services, network, updates, certificates, logs |
| `vms` | QEMU virtual machines |
| `containers` | LXC containers |
| `agent` | Information from inside VMs via the guest agent, and (with exec) commands |
| `snapshots` | Guest snapshots |
| `storage` | Storage, volumes, ISOs and templates, physical disks, ZFS, LVM |
| `backup` | Backup jobs, backups, restores |
| `tasks` | Task history and logs. **Include it whenever you allow writes**, so the assistant can follow tasks. |
| `access` | Users, groups, roles, ACLs, tokens, realms, 2FA |
| `firewall` | Datacenter, node and guest firewall |
| `ha` | High availability |
| `sdn` | Software-defined networking |
| `ceph` | Ceph |
| `pools` | Resource pools |
| `replication` | Storage replication |
| `notifications` | Notification targets and matchers |
| `hardware` | PCI/USB devices, mappings, CPU models |
| `raw` | Any API path. Handy, but gives broad access. |

The [tool reference](tools.md) lists the tools in each toolset. A read-only setup registers far fewer tools than the full set. Some suggested sets:

| Goal | `PROXMOX_TOOLSETS` |
|---|---|
| Everyday monitoring | `cluster,nodes,vms,containers,storage,backup,tasks` |
| Running guests day to day | `cluster,vms,containers,agent,snapshots,backup,tasks` |
| Backup and snapshot hygiene | `cluster,snapshots,backup,storage,tasks` |
| Security review | `cluster,nodes,access,firewall,tasks` |
| Network and SDN | `cluster,nodes,sdn,firewall` |
| Everything (default) | `all` |

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
