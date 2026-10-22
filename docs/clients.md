# Connecting MCP clients

proxmox-mcp supports both standard MCP transports:

| Transport | How it runs | Best for |
|---|---|---|
| **Streamable HTTP** (the default in the image) | One long-running container. Clients connect to `http://<host>:8080/mcp`. | Most setups. One server can be shared by several clients and machines. |
| **stdio** | The client starts a fresh container for each session with `docker run -i`. | A single machine, no network service, or clients that only support stdio. |

On **Claude Desktop** you can skip Docker altogether: install the [extension bundle](#claude-desktop), which runs the server for you over stdio.

The HTTP endpoint is **stateless**: each request is handled on its own, so you can restart the container at any time. Every request must send `Authorization: Bearer <token>` with your `MCP_AUTH_TOKEN`; the server won't start in HTTP mode without one.

In the examples below, replace `<docker-host>` with the machine running the container and `<token>` with your `MCP_AUTH_TOKEN`. The server is called `proxmox` in every example. If you use a different name, the prompt slash commands change to match.

- [Claude Code](#claude-code)
- [Claude Desktop](#claude-desktop)
- [VS Code (GitHub Copilot)](#vs-code-github-copilot)
- [Cursor](#cursor)
- [OpenAI Codex CLI](#openai-codex-cli)
- [Other clients](#other-clients)
- [stdio mode](#stdio-mode)

## Claude Code

```sh
claude mcp add --transport http proxmox http://<docker-host>:8080/mcp \
  --header "Authorization: Bearer <token>"
```

Add `--scope user` to make it available in every project, or `--scope project` to save it in a `.mcp.json` you can share with your team. Don't commit the token: in `.mcp.json`, reference an environment variable instead:

```json
{
  "mcpServers": {
    "proxmox": {
      "type": "http",
      "url": "http://<docker-host>:8080/mcp",
      "headers": { "Authorization": "Bearer ${PROXMOX_MCP_TOKEN}" }
    }
  }
}
```

Run `claude mcp list` to check the connection, and `/mcp` inside Claude Code to see the tools. The built-in prompts are available as slash commands, such as `/mcp__proxmox__cluster_health_check` or `/mcp__proxmox__troubleshoot_guest 101 "won't start"`.

## Claude Desktop

The easiest way is the **extension bundle**: a single `.mcpb` file that installs proxmox-mcp into Claude Desktop with a settings form. It needs no Docker, no service and no `MCP_AUTH_TOKEN`, because Claude Desktop runs the server itself over stdio.

### Install the extension

1. Open the [latest release](https://github.com/mattoddie/proxmox-mcp/releases/latest) and download the bundle for your computer:

   | Computer | Bundle |
   |---|---|
   | Mac with Apple silicon (M1 and later) | `proxmox-mcp-<version>-darwin-arm64.mcpb` |
   | Mac with an Intel processor | `proxmox-mcp-<version>-darwin-amd64.mcpb` |
   | Windows on x64 (most PCs) | `proxmox-mcp-<version>-windows-amd64.mcpb` |
   | Windows on Arm | `proxmox-mcp-<version>-windows-arm64.mcpb` |

   There are Linux bundles too (`linux-amd64`, `linux-arm64`) for other clients that install MCP Bundles.

2. Double-click the file, or drag it onto **Settings → Extensions** in Claude Desktop, and click **Install**.
3. Fill in the settings and turn the extension on. The tools appear under the tools (🔨) menu, and the prompts under the **+** menu. You can allow or block each tool separately there.

To update, install the bundle from a newer release the same way.

### Settings

| Setting | Variable | What to enter |
|---|---|---|
| **Proxmox URL** (required) | `PROXMOX_URL` | Any node's address with the port, such as `https://192.168.1.10:8006`. See [Getting started](getting-started.md#1-find-your-proxmox-url). |
| **API token** | `PROXMOX_API_TOKEN` | **Recommended.** The whole token as `user@realm!tokenid=secret`, such as `mcp@pve!mcp=xxxxxxxx-…`. See [Getting started](getting-started.md#2-create-a-user-and-api-token). |
| **Username**, **Password** | `PROXMOX_USERNAME`, `PROXMOX_PASSWORD` | A user such as `mcp@pve`, instead of a token. Accounts with two-factor authentication can't log in this way. |
| **TLS certificate fingerprint** | `PROXMOX_TLS_FINGERPRINT` | **Recommended.** The SHA-256 fingerprint of the node's certificate, shown under **Node → System → Certificates**, so nobody on the network can impersonate it. |
| **Verify certificate (true/false)** | `PROXMOX_VERIFY_SSL` | Leave empty when you set a fingerprint or CA file. `true` for a publicly trusted certificate, `false` to skip the check (not recommended). |
| **CA certificate file** | `PROXMOX_CA_FILE` | A PEM file with the CA that signed the node's certificate (the cluster's is `/etc/pve/pve-root-ca.pem` on any node), instead of a fingerprint. |
| **Default node** | `PROXMOX_DEFAULT_NODE` | The node used when a request doesn't name one. Not needed on a single-node install. |
| **Allow changes** | `PROXMOX_ALLOW_WRITES` | Off by default, which keeps the assistant read-only. Read [Security](security.md) before turning it on. |
| **Allow deletes** | `PROXMOX_ALLOW_DELETES` | Also allows deleting guests, volumes, snapshots, backups and configuration. Needs **Allow changes**. |
| **Allow exec** | `PROXMOX_ALLOW_EXEC` | Also allows running commands and reading and writing files inside VMs through the guest agent. Needs **Allow changes**. |
| **Toolsets** | `PROXMOX_TOOLSETS` | Optional, such as `cluster,nodes,vms,containers,backup,tasks`. Empty means all except `raw`, which is 131 KB of the model's context read-only and up to 397 KB with everything allowed. See [Keeping the tool list small](configuration.md#keeping-the-tool-list-small). |
| **Audit log file** | `PROXMOX_AUDIT_LOG` | Optional full path to a file, such as `/Users/you/proxmox-mcp-audit.jsonl`, that records every change Claude makes or previews. The folder must exist; the file is created. `stderr` writes the entries to the extension's log instead. Off when empty. See [Audit log](configuration.md#audit-log). |

Fill in either the API token or the username and password. Settings you leave empty use their defaults, as described in [Configuration](configuration.md).

For an `https` URL, the server won't start until you say how to check the node's certificate. Proxmox uses a self-signed certificate, so **set the TLS certificate fingerprint**. Copy it from **Node → System → Certificates** in the web UI, or run this on the node:

```sh
openssl x509 -in /etc/pve/local/pve-ssl.pem -noout -fingerprint -sha256
```

See [Verifying the Proxmox certificate](configuration.md#verifying-the-proxmox-certificate) for more.

### If it doesn't start

- **Read the error.** Claude Desktop shows that the extension failed to start. The server's log, which says what's wrong (for example a missing certificate choice or a bad fingerprint), is in `~/Library/Logs/Claude/` on macOS and `%APPDATA%\Claude\logs\` on Windows, in a file named after the extension.
- **macOS blocks the server.** The binary in the bundle isn't notarised by Apple. If macOS says it can't check it for malicious software, open **System Settings → Privacy & Security** and click **Open Anyway** next to the message about `proxmox-mcp`, then turn the extension off and on again.
- **Allow deletes or Allow exec without Allow changes.** The server refuses to start, because both need writes. Turn on **Allow changes** too.

### Or run it with Docker

If you'd rather run the container image, use [stdio mode](#stdio-mode). Open **Settings → Developer → Edit Config** and add:

```json
{
  "mcpServers": {
    "proxmox": {
      "command": "docker",
      "args": [
        "run", "-i", "--rm",
        "-e", "MCP_TRANSPORT=stdio",
        "-e", "PROXMOX_URL", "-e", "PROXMOX_TOKEN_ID", "-e", "PROXMOX_TOKEN_SECRET", "-e", "PROXMOX_TLS_FINGERPRINT",
        "ghcr.io/mattoddie/proxmox-mcp:latest"
      ],
      "env": {
        "PROXMOX_URL": "https://192.168.1.10:8006",
        "PROXMOX_TOKEN_ID": "mcp@pve!mcp",
        "PROXMOX_TOKEN_SECRET": "your-token-secret",
        "PROXMOX_TLS_FINGERPRINT": "AB:CD:..."
      }
    }
  }
}
```

Restart Claude Desktop. The tools appear under the tools (🔨) menu, and the prompts appear under **+ → Add from proxmox**.

To turn on writes, add `"-e", "PROXMOX_ALLOW_WRITES=true"` to `args`.

## VS Code (GitHub Copilot)

Create `.vscode/mcp.json` in your workspace, or run **MCP: Add Server** from the command palette:

```json
{
  "inputs": [
    { "id": "proxmox-token", "type": "promptString", "description": "proxmox-mcp token", "password": true }
  ],
  "servers": {
    "proxmox": {
      "type": "http",
      "url": "http://<docker-host>:8080/mcp",
      "headers": { "Authorization": "Bearer ${input:proxmox-token}" }
    }
  }
}
```

VS Code asks for the token once and stores it securely. Use the tools from Copilot Chat in **Agent** mode.

## Cursor

Add to `~/.cursor/mcp.json` for all projects, or `.cursor/mcp.json` for one project:

```json
{
  "mcpServers": {
    "proxmox": {
      "url": "http://<docker-host>:8080/mcp",
      "headers": { "Authorization": "Bearer <token>" }
    }
  }
}
```

## OpenAI Codex CLI

Add to `~/.codex/config.toml`:

```toml
[mcp_servers.proxmox]
url = "http://<docker-host>:8080/mcp"
bearer_token_env_var = "PROXMOX_MCP_TOKEN"
```

Then `export PROXMOX_MCP_TOKEN=<token>` before running `codex`.

## Other clients

Any client that supports MCP **Streamable HTTP** works. Point it at `http://<docker-host>:8080/mcp` and send the bearer token header.

If your client only supports stdio, you have two options. You can use [stdio mode](#stdio-mode), or you can keep the shared HTTP server and connect through a stdio-to-HTTP bridge such as [`mcp-remote`](https://www.npmjs.com/package/mcp-remote):

```json
{
  "command": "npx",
  "args": ["-y", "mcp-remote", "http://<docker-host>:8080/mcp", "--header", "Authorization: Bearer <token>"]
}
```

Some clients limit how many tools they accept, or slow down with a lot of tools. If yours does, narrow `PROXMOX_TOOLSETS`. See [Configuration](configuration.md#keeping-the-tool-list-small).

## stdio mode

In stdio mode the MCP client runs the container itself and talks to it over stdin/stdout. No port is opened, and no `MCP_AUTH_TOKEN` is needed because only the client can reach the server.

The general shape is:

```sh
docker run -i --rm \
  -e MCP_TRANSPORT=stdio \
  -e PROXMOX_URL=https://192.168.1.10:8006 \
  -e PROXMOX_TOKEN_ID='mcp@pve!mcp' \
  -e PROXMOX_TOKEN_SECRET=your-token-secret \
  -e PROXMOX_TLS_FINGERPRINT=AB:CD:... \
  ghcr.io/mattoddie/proxmox-mcp:latest
```

Tips:

- **Quote the token ID in a shell.** The `!` in `mcp@pve!mcp` triggers history expansion in interactive bash and zsh. Single quotes avoid it. JSON configs and `.env` files don't need quoting.
- **Keep secrets out of the client config.** Pass `-e NAME` without a value so Docker copies the variable from the client's environment, as in the Claude Desktop example. You can also use `--env-file /path/to/proxmox.env`.
- **Always pass `-i` and never `-t`.** A TTY corrupts the MCP message stream.
- **Docker on another machine** works too. Set `DOCKER_HOST` in the client's `env`, and note that `PROXMOX_URL` must be reachable from that machine.
- **Pin a version** (for example `:1.2.3`) so a new `latest` doesn't surprise you. See [Deployment](deployment.md#image-tags).
