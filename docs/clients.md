# Connecting MCP clients

proxmox-mcp supports both standard MCP transports:

| Transport | How it runs | Best for |
|---|---|---|
| **Streamable HTTP** (the default in the image) | One long-running container. Clients connect to `http://<host>:8080/mcp`. | Most setups. One server can be shared by several clients and machines. |
| **stdio** | The client starts a fresh container for each session with `docker run -i`. | A single machine, no network service, or clients that only support stdio. |

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

Claude Desktop starts local MCP servers over stdio, so use [stdio mode](#stdio-mode). Open **Settings → Developer → Edit Config** and add:

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

Some clients limit how many tools they accept, or slow down with a lot of tools. If yours does, narrow `PROXMOX_TOOLSETS`. See [Configuration](configuration.md#choosing-toolsets).

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
