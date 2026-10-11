# Troubleshooting

Start with the logs: `docker compose logs proxmox-mcp`. Configuration errors stop the server at startup with a clear message. Proxmox errors show up in the tool results the assistant sees, and you can usually ask it *"what error did you get?"*.

- [The container keeps restarting](#the-container-keeps-restarting)
- [The client can't connect to the server](#the-client-cant-connect-to-the-server)
- [Login and permission errors](#login-and-permission-errors)
- [Certificate and connection errors](#certificate-and-connection-errors)
- ["Pass `node`"](#pass-node)
- ["No VM or container with ID"](#no-vm-or-container-with-id)
- [Guest agent errors](#guest-agent-errors)
- [Tasks that fail or time out](#tasks-that-fail-or-time-out)
- ["Not implemented" and other version errors](#not-implemented-and-other-version-errors)
- [A change didn't take effect](#a-change-didnt-take-effect)
- [Tools are missing](#tools-are-missing)
- [Still stuck?](#still-stuck)

## The container keeps restarting

Run `docker compose logs proxmox-mcp`. The last line names the problem, for example:

| Message | Fix |
|---|---|
| `PROXMOX_URL is required (e.g. https://pve.example.lan:8006)` | Set `PROXMOX_URL` in `.env`, and check that `env_file: .env` is in `compose.yaml`. |
| `PROXMOX_URL must use https:// (or http://)` | Add the scheme: `https://pve1:8006`, not `pve1:8006`. |
| `Set either an API token (PROXMOX_TOKEN_ID and PROXMOX_TOKEN_SECRET) or both PROXMOX_USERNAME and PROXMOX_PASSWORD` | Add credentials. |
| `The API token id must look like user@realm!tokenid (e.g. root@pam!mcp)` | `PROXMOX_TOKEN_ID` is the **full** token ID including the user, such as `mcp@pve!mcp`, not just `mcp`. |
| `Set both PROXMOX_TOKEN_ID and PROXMOX_TOKEN_SECRET` | One of the pair is missing or empty. |
| `PROXMOX_API_TOKEN must look like user@realm!tokenid=secret` | The combined form needs the `=` between the token ID and the secret. |
| `PROXMOX_ALLOW_DELETES=true also requires PROXMOX_ALLOW_WRITES=true` | Turn on writes too, or turn deletes off. The same applies to `PROXMOX_ALLOW_EXEC`. |
| `Unknown toolset "x" in PROXMOX_TOOLSETS` | Fix the typo. The message lists the valid names. |
| `Could not read PROXMOX_TOKEN_SECRET_FILE (…)` | The secret file isn't mounted where the variable points. |
| `Could not read PROXMOX_CA_FILE (…)` | The certificate file isn't mounted where the variable points. |
| `MCP_HTTP_PORT must be a positive integer` | Fix the value. The same applies to the `_TIMEOUT_MS` variables. |

## The client can't connect to the server

1. **Is it running?** `curl http://<docker-host>:8080/health` should return `{"status":"ok"}`.
2. **Right host?** The port is published on the machine running Docker. If Docker runs on another machine (for example with `DOCKER_HOST`), `localhost` won't work.
3. **Right path?** The endpoint is `/mcp`, so the URL is `http://<docker-host>:8080/mcp`.
4. **`401 Unauthorized`:** the client isn't sending `Authorization: Bearer <MCP_AUTH_TOKEN>`, or the token doesn't match. Watch for stray quotes or spaces in `.env`.
5. **`403 Host … is not allowed`:** add the hostname the client uses to `MCP_ALLOWED_HOSTS`, or unset it.
6. **`405 Method not allowed`:** the client tried to open an SSE stream with `GET`. The server is stateless and only accepts `POST`. Make sure the client is set to *Streamable HTTP* (often called `http`), not `sse`.

## Login and permission errors

**`… failed with HTTP 401: … (check the API token or credentials)`**

- Check `PROXMOX_TOKEN_ID` is the full ID (`mcp@pve!mcp`) and the secret was copied completely.
- Check the token still exists and hasn't expired: **Datacenter → Permissions → API Tokens**, or `pveum user token list mcp@pve`.
- Check the user is enabled and hasn't expired. A disabled user disables its tokens too.

**`… failed with HTTP 403: Permission check failed (…) (the token or user lacks the privilege for this; for API tokens check privilege separation and the token's own ACLs)`**

The request reached Proxmox, but the token isn't allowed to do it. The text in the first brackets names the path and privilege Proxmox checked, such as `/vms/100, VM.PowerMgmt`.

- **Privilege separation.** If the token was created with privilege separation (the default, and the ticked box in the web UI), it has **no permissions** until you grant some to the token itself: `pveum acl modify / --tokens 'mcp@pve!mcp' --roles PVEAuditor`. Or recreate it with `--privsep 0` so it inherits the user's permissions.
- **Missing privilege.** Grant a role that includes the privilege on that path. See [Getting started](getting-started.md#2-create-a-user-and-api-token) for typical roles. Common gaps: `Sys.Syslog` for node logs, `VM.Monitor` (8.x) or `VM.GuestAgent.Audit` (9.x) for guest agent information, and `SDN.Use` on `/sdn/zones/<zone>/<bridge>` for attaching a NIC to a bridge.
- **A read-only token with writes enabled.** If the token only has `PVEAuditor`, write tools fail with this error even when `PROXMOX_ALLOW_WRITES=true`. That's Proxmox doing its job.
- **Check what the token can do** with `pveum user token permissions mcp@pve mcp`.

**`Login to Proxmox failed (HTTP 401 …); check PROXMOX_USERNAME (user@realm) and PROXMOX_PASSWORD`**

- The username needs a realm: `root@pam` or `mcp@pve`. A bare `admin` becomes `admin@pam` (or `@PROXMOX_REALM`).
- `pam` users are Linux accounts on the node; `pve` users exist only in Proxmox. Make sure you use the right realm.
- Check the password by logging in to the web UI with it.

**`This account requires two-factor authentication; use an API token instead (PROXMOX_TOKEN_ID/PROXMOX_TOKEN_SECRET)`**

Password logins can't complete 2FA. Create an API token for the user. Tokens aren't affected by 2FA.

## Certificate and connection errors

**`Could not reach Proxmox at https://…: ECONNREFUSED / ETIMEDOUT / ENOTFOUND / EHOSTUNREACH`**

- Test from the Docker host: `curl -k https://pve1.example.lan:8006/api2/json/version`. It should return `401`, which proves the API is reachable. The container uses the Docker host's network path.
- Include the port, `:8006`, in `PROXMOX_URL`.
- Hostnames must resolve **inside the container**. If local DNS names don't resolve there, use an IP address.
- A host firewall on the node, or the Proxmox datacenter firewall, may block port 8006 from the Docker host.

**`Could not reach Proxmox at …: … (Proxmox uses a self-signed certificate by default; set PROXMOX_VERIFY_SSL=false or PROXMOX_CA_FILE)`**

You set `PROXMOX_VERIFY_SSL=true`, but the node's certificate isn't trusted. Either set it back to `false`, or keep it on and set `PROXMOX_CA_FILE` to the cluster CA (`/etc/pve/pve-root-ca.pem` from any node). If the error mentions the hostname or altnames, connect using a name or address that's in the node's certificate.

**`Timed out after 30000ms talking to https://…`**

A single API request took longer than `PROXMOX_TIMEOUT_MS`. Some calls are slow on big or busy clusters, such as SMART data, storage scans or large task lists. Raise `PROXMOX_TIMEOUT_MS`, for example to `60000`. If every call times out, the node is probably unreachable or overloaded.

## "Pass `node`"

**``This cluster has 3 nodes; pass `node` (one of: pve1, pve2, pve3) or set PROXMOX_DEFAULT_NODE``**

A node-scoped tool was called without a node in a multi-node cluster. Usually the assistant just retries with a node. To make one node the default, set `PROXMOX_DEFAULT_NODE`. Guest tools don't need this: they find the node from the VMID.

## "No VM or container with ID"

**`No VM or container with ID 123 (see proxmox_list_guests)`**

- The guest doesn't exist, or it was just deleted or migrated.
- The token can't see it. Proxmox only lists guests the token has at least `VM.Audit` on, so a token scoped to a pool can't find guests outside it.

**`Guest 200 is a container, not a VM (qemu); use the proxmox_*_container tools`**

VMs and containers have separate tools. The assistant usually corrects itself after this error.

## Guest agent errors

| Message | Fix |
|---|---|
| `The QEMU guest agent is not enabled for VM 100…` | Turn on **Options → QEMU Guest Agent** (`agent: 1`), install `qemu-guest-agent` in the guest, then stop and start the VM from Proxmox. |
| `The QEMU guest agent in VM 100 is not responding…` | The option is on but the agent isn't running inside the guest. Start the `qemu-guest-agent` service, and check it starts at boot. |
| `VM 100 is not running; the guest agent only works in a running VM.` | Start the VM first. |
| `The guest agent in VM 100 did not answer in time; it may be busy or hung.` | Try again. If it keeps happening, restart the agent service inside the guest. |

On Windows, install the agent from the VirtIO drivers ISO. Containers don't have a guest agent.

## Tasks that fail or time out

**`Task qmstart 100 failed: <exit status>` followed by `--- task log (tail) ---`**

Proxmox ran the task and it failed. The log tail usually says why, for example a missing storage, a locked guest, not enough memory on the node, or a disk image that's in use. Ask the assistant to read the full log with `proxmox_get_task_log`.

**A tool returned `status: "running"` with a note to check `proxmox_get_task`**

That isn't an error. The task took longer than `PROXMOX_TASK_TIMEOUT_MS` and is still running in Proxmox. Ask the assistant to check on it, or raise the timeout. Behind a reverse proxy, make sure the proxy's read timeout is longer than the task timeout, or the client sees a gateway error first.

**`VM is locked (backup)` / `(migrate)` / `(snapshot)`**

Another task is working on the guest. Wait for it to finish (`proxmox_list_tasks` with `running: true`). If a crashed task left a stale lock, clear it on the node with `qm unlock <vmid>` or `pct unlock <vmid>`, but only once you're sure nothing is running.

## "Not implemented" and other version errors

**`… failed with HTTP 501: … (not implemented on this Proxmox VE version)`**

The endpoint doesn't exist on your Proxmox VE version, for example bulk guest actions or HA rules on 8.x. See [Compatibility](compatibility.md).

**`… failed with HTTP 400: Parameter verification failed. (name: …)`**

Proxmox rejected an argument. The part in brackets names each bad parameter and why. If it's a parameter passed through `extra`, check its name and format in the [API viewer](https://pve.proxmox.com/pve-docs/api-viewer/) for your version.

## A change didn't take effect

- **Pending changes.** Many VM hardware changes, and some container changes, only apply after the guest is stopped and started from Proxmox. The result lists them under `pending_changes`. A reboot from inside a VM isn't enough.
- **SDN changes** aren't live until they're applied. Ask the assistant to apply pending SDN changes.
- **Firewall rules** only take effect if the firewall is enabled at the datacenter level and on the guest's NIC (`firewall=1`), as well as on the guest itself.
- If Proxmox rejected the change, the tool result contains its message.

## Tools are missing

Tools are only registered when they're allowed:

- Write tools need `PROXMOX_ALLOW_WRITES=true`. Delete tools also need `PROXMOX_ALLOW_DELETES=true`, and exec tools also need `PROXMOX_ALLOW_EXEC=true`.
- Each tool's toolset must be listed in `PROXMOX_TOOLSETS`.
- Restart the container after changing `.env` with `docker compose up -d`, and reconnect or restart your MCP client so it fetches the new tool list.
- Check the first log line: it shows whether writes, deletes and exec are enabled, and how many toolsets are loaded.

## Still stuck?

Search the [existing issues](https://github.com/mattoddie/proxmox-mcp/issues) or [open a bug report](https://github.com/mattoddie/proxmox-mcp/issues/new?template=bug_report.yml). Include your Proxmox VE version, whether it's a single node or a cluster, the authentication method, the proxmox-mcp version (the first log line) and the exact error. **Remove any token secrets, passwords and public IPs first.**
