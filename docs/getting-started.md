# Getting started

This guide takes you from nothing to asking your first question about your Proxmox cluster. It takes about ten minutes.

- [What you need](#what-you-need)
- [1. Find your Proxmox URL](#1-find-your-proxmox-url)
- [2. Create a user and API token](#2-create-a-user-and-api-token)
- [3. Run the server](#3-run-the-server)
- [4. Connect your assistant](#4-connect-your-assistant)
- [5. Ask something](#5-ask-something)
- [Next steps](#next-steps)

## What you need

- **Proxmox VE 8 or 9**, either a single node or a cluster. See [Compatibility](compatibility.md) for details.
- **Shell access to a node as root**, or a web UI login with permission to manage users, to create the API token.
- **A machine that runs Docker** and can reach a Proxmox node on port 8006. This could be a NAS, a home server, a small VM or container on the cluster itself, or your laptop. The image supports both `amd64` and `arm64`.
- **An MCP client**, such as [Claude Code](https://claude.com/claude-code), Claude Desktop, VS Code, Cursor or Codex.

## 1. Find your Proxmox URL

This is the address you use to open the Proxmox web interface, without any path, for example `https://192.168.1.10:8006` or `https://pve1.example.lan:8006`. Include the port.

In a cluster, any node can answer for the whole cluster, so pick one that's usually up. If that node goes down, the server can't reach Proxmox until it's back. For more resilience, point `PROXMOX_URL` at a load balancer or a DNS name that resolves to several nodes.

## 2. Create a user and API token

Use a dedicated user with an API token. Tokens don't expire unless you set an expiry, can't be used to log in to the web UI, work with accounts that have two-factor authentication, and can be revoked on their own. You can also give the server a username and password (see [Configuration](configuration.md#authentication)), but a token is better.

> [!IMPORTANT]
> Proxmox checks every request against the token's permissions, whatever proxmox-mcp is configured to allow. Giving the token only the privileges it needs is your strongest protection. Don't use `root@pam` or a token with the `Administrator` role unless you really mean to.

### How token permissions work

An API token belongs to a user. When you create it you choose whether it has **privilege separation**:

| Privilege separation | The token's permissions are | Good for |
|---|---|---|
| Off (`--privsep 0`) | Exactly the user's permissions | A dedicated user that exists only for proxmox-mcp. Simplest. |
| On (`--privsep 1`, the default) | Only what's granted to the token itself, and never more than the user has | Giving one user several tokens with different, narrower rights |

With privilege separation on, a new token has **no permissions at all** until you add ACL entries for the token itself. This is the most common cause of `403` errors. See [Troubleshooting](troubleshooting.md#login-and-permission-errors).

### Option A: Read-only (recommended to start)

Run these on any node as root:

```sh
# A user in the Proxmox VE realm. It needs no password because it only uses a token.
pveum user add mcp@pve --comment "proxmox-mcp"

# Read-only access to everything
pveum acl modify / --users mcp@pve --roles PVEAuditor

# A token with the same permissions as the user
pveum user token add mcp@pve mcp --privsep 0
```

The last command prints a table with the `full-tokenid` (`mcp@pve!mcp`) and the `value`, which is the secret. Copy the secret now. You can't see it again later.

`PVEAuditor` can read nearly everything the read tools use. A few reads need privileges it doesn't include:

- **Node system log and journal** need `Sys.Syslog`.
- **Guest agent information**, such as a VM's IP addresses, OS and filesystems, needs `VM.Monitor` on Proxmox VE 8 or `VM.GuestAgent.Audit` on Proxmox VE 9. Check `pveum role list` on your version to see whether `PVEAuditor` includes it.
- **Some access-control listings**, such as other users' API tokens and two-factor entries, only show what the account is allowed to manage.

If you want those reads, create a custom read-only role instead of using `PVEAuditor`. On Proxmox VE 9:

```sh
pveum role add MCPReader --privs "Sys.Audit Sys.Syslog VM.Audit VM.GuestAgent.Audit Datastore.Audit Pool.Audit SDN.Audit Mapping.Audit"
pveum acl modify / --users mcp@pve --roles MCPReader
```

On Proxmox VE 8, use `VM.Monitor` in place of `VM.GuestAgent.Audit`. Be aware that on version 8 `VM.Monitor` also allows guest agent commands and the QEMU monitor through the Proxmox API, so leave it out if you want Proxmox itself to block those. `pveum` rejects privilege names that don't exist on your version, so a typo can't slip through.

### Option B: Operator (writes)

To let the assistant start, stop, create, clone, snapshot, migrate and back up guests, grant broader roles on the relevant paths:

```sh
pveum user add mcp@pve --comment "proxmox-mcp"
pveum acl modify /          --users mcp@pve --roles PVEAuditor        # read everything
pveum acl modify /vms       --users mcp@pve --roles PVEVMAdmin        # manage all guests
pveum acl modify /storage   --users mcp@pve --roles PVEDatastoreUser  # allocate disks, back up to storage
pveum acl modify /sdn/zones --users mcp@pve --roles PVESDNUser        # attach NICs to bridges and VNets
pveum user token add mcp@pve mcp --privsep 0
```

Some notes:

- `PVEVMAdmin` includes every guest privilege, including the ones that let the Proxmox API run commands in guests through the guest agent. proxmox-mcp blocks those unless `PROXMOX_ALLOW_EXEC=true`, but if you want Proxmox to block them too, build a custom role from the individual `VM.*` privileges instead.
- Downloading ISOs and templates to storage needs `Datastore.AllocateTemplate`, which is in `PVEDatastoreAdmin`, not `PVEDatastoreUser`.
- Node administration, such as managing services, installing updates or rebooting nodes, needs `Sys.Modify` and `Sys.PowerMgmt` on `/nodes`. The `PVESysAdmin` role covers them. Only grant it if you want the assistant doing that.
- To limit the assistant to some guests, grant `PVEVMAdmin` on a resource pool (`/pool/<name>`) instead of `/vms`. See [Security](security.md#scope-the-token).

### Option C: The web UI

1. **Datacenter → Permissions → Users → Add.** Set the user name to `mcp`, the realm to *Proxmox VE authentication server*, and leave the password empty.
2. **Datacenter → Permissions → Add → User Permission.** Set the path to `/`, the user to `mcp@pve` and the role to `PVEAuditor`. Add more entries for the operator roles above if you want writes.
3. **Datacenter → Permissions → API Tokens → Add.** Choose the user `mcp@pve`, set the token ID to `mcp`, and **untick Privilege Separation**. Copy the secret from the dialogue that follows.

If you leave Privilege Separation ticked, also add **API Token Permission** entries for `mcp@pve!mcp` with the same paths and roles.

## 3. Run the server

Create a folder and download the compose file and the example settings:

```sh
mkdir proxmox-mcp && cd proxmox-mcp
curl -fsSLO https://raw.githubusercontent.com/mattoddie/proxmox-mcp/main/compose.yaml
curl -fsSL https://raw.githubusercontent.com/mattoddie/proxmox-mcp/main/example.env -o .env
```

Edit `.env` and set at least:

```sh
PROXMOX_URL=https://192.168.1.10:8006
PROXMOX_TOKEN_ID=mcp@pve!mcp
PROXMOX_TOKEN_SECRET=xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
MCP_AUTH_TOKEN=a-long-random-string   # required; generate one with: openssl rand -hex 32
PROXMOX_TLS_FINGERPRINT=AB:CD:...      # recommended: the node certificate's SHA-256 fingerprint
```

You can also paste the token as one value, the way Proxmox shows it: `PROXMOX_API_TOKEN=mcp@pve!mcp=xxxxxxxx-…`.

The server won't start in HTTP mode without `MCP_AUTH_TOKEN`. For `PROXMOX_TLS_FINGERPRINT`, open **Node → System → Certificates** in the Proxmox web UI, select the certificate the web UI uses (`pveproxy-ssl.pem` if there is one, otherwise `pve-ssl.pem`) and copy the SHA-256 fingerprint from **View Certificate**. Without it (or another way to verify the certificate, see [Configuration](configuration.md#verifying-the-proxmox-certificate)) the server logs a warning that the connection to Proxmox isn't verified.

Start it:

```sh
docker compose up -d
docker compose logs -f proxmox-mcp
```

You should see lines like these:

```text
proxmox-mcp 1.0.0: Proxmox https://192.168.1.10:8006, auth API token mcp@pve!mcp, writes disabled, toolsets 19, transport http
proxmox-mcp listening on http://0.0.0.0:8080/mcp (bearer auth enabled)
```

Check that it's up from another machine:

```sh
curl http://<docker-host>:8080/health
# {"status":"ok"}
```

> [!TIP]
> `/health` only shows that the server is running. It doesn't contact Proxmox. A wrong URL, token or certificate setting shows up as an error the first time a tool is used, so step 5 is the real test.

## 4. Connect your assistant

For **Claude Code**:

```sh
claude mcp add --transport http proxmox http://<docker-host>:8080/mcp \
  --header "Authorization: Bearer <MCP_AUTH_TOKEN>"
```

Run `claude mcp list` to check that `proxmox` shows as connected. For other clients, see [Connecting MCP clients](clients.md).

## 5. Ask something

Start a new conversation and try:

- *"Give me an overview of my Proxmox cluster."*
- *"Which VMs and containers are using the most memory?"*
- *"Show me any failed tasks from the last few days."*

The assistant calls tools like `proxmox_get_cluster_status` and `proxmox_list_guests` and summarises the results. In Claude Code you can also run the built-in prompts as slash commands, for example `/mcp__proxmox__cluster_health_check`.

If something goes wrong, the tool's error message usually says what to fix. [Troubleshooting](troubleshooting.md) covers the common errors.

## Next steps

- **Let it make changes.** Give the token the permissions it needs, set `PROXMOX_ALLOW_WRITES=true`, and read [Security](security.md) first.
- **Expose fewer tools** with `PROXMOX_TOOLSETS` for a faster, more focused assistant. See [Configuration](configuration.md#choosing-toolsets).
- **Put it behind HTTPS** if it's reachable outside your LAN. See [Deployment](deployment.md#tls-with-a-reverse-proxy).
- **Learn what it can do** in [Using proxmox-mcp](usage.md) and the [tool reference](tools.md).
