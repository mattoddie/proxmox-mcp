# Deployment

- [Docker Compose](#docker-compose)
- [Image tags](#image-tags)
- [Upgrading](#upgrading)
- [Building from source](#building-from-source)
- [TLS with a reverse proxy](#tls-with-a-reverse-proxy)
- [Hardening](#hardening)
- [Running on the cluster itself](#running-on-the-cluster-itself)
- [Running without Docker](#running-without-docker)

## Docker Compose

The repository's [`compose.yaml`](../compose.yaml) is a good starting point:

```yaml
services:
  proxmox-mcp:
    image: ghcr.io/mattoddie/proxmox-mcp:latest
    container_name: proxmox-mcp
    restart: unless-stopped
    env_file: .env
    ports:
      - "8080:8080"
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
```

```sh
docker compose up -d          # start
docker compose logs -f        # follow logs
docker compose ps             # shows "healthy" once the health check passes
docker compose down           # stop
```

The image has a built-in health check that calls `/health` every 30 seconds. It only checks that the server is running, not that Proxmox is reachable. It connects to `MCP_HTTP_HOST` on `MCP_HTTP_PORT`, or to loopback when the server listens on all interfaces (`0.0.0.0`, `::` or unset).

To listen on one interface only, such as a LAN address or localhost when you use a reverse proxy, change the port mapping to `"192.168.1.20:8080:8080"` or `"127.0.0.1:8080:8080"`.

To verify Proxmox's certificate, the simplest option is to pin its fingerprint with `PROXMOX_TLS_FINGERPRINT` in `.env` (see [Configuration](configuration.md#verifying-the-proxmox-certificate)). To verify it with the cluster CA instead, mount it read-only and point `PROXMOX_CA_FILE` at it:

```yaml
    volumes:
      - ./pve-root-ca.pem:/certs/pve-root-ca.pem:ro
    environment:
      PROXMOX_VERIFY_SSL: "true"
      PROXMOX_CA_FILE: /certs/pve-root-ca.pem
```

Copy the file from any node with `scp root@pve1:/etc/pve/pve-root-ca.pem .`. The container runs as UID 65532, so the file must be world-readable (`chmod 644`).

## Image tags

Images are published to the GitHub Container Registry at **`ghcr.io/mattoddie/proxmox-mcp`** for `linux/amd64` and `linux/arm64`. Each [GitHub release](https://github.com/mattoddie/proxmox-mcp/releases) publishes these tags:

| Tag | Example | Moves? |
|---|---|---|
| `X.Y.Z` | `1.4.2` | Never. Pin this for full reproducibility. |
| `X.Y` | `1.4` | Moves to the latest patch release (bug fixes). |
| `X` | `1` | Moves to the latest minor release (new features, no breaking changes). Not published for `0.x`. |
| `latest` | | Moves to the newest stable release. |

Pre-releases, such as `1.5.0-rc.1`, are only published under their exact version, never as `latest`.

Every image includes an SBOM and build provenance attestation. To verify that an image was built by this repository's release workflow:

```sh
gh attestation verify oci://ghcr.io/mattoddie/proxmox-mcp:1.4.2 --owner mattoddie
```

## Upgrading

```sh
docker compose pull
docker compose up -d
```

Check the [changelog](../CHANGELOG.md) or the release notes before upgrading across a major version. After an upgrade, reconnect or restart your MCP client so it fetches the new tool list.

## Building from source

```sh
git clone https://github.com/mattoddie/proxmox-mcp.git
cd proxmox-mcp
docker build -t proxmox-mcp .
```

The tests run as part of the image build, so a broken build never produces an image. To use a local build with Compose, replace `image:` in `compose.yaml` with `build: .` and run `docker compose up -d --build`.

## TLS with a reverse proxy

The server speaks plain HTTP. If it's reachable from outside a trusted network, put a TLS-terminating reverse proxy in front of it and keep `MCP_AUTH_TOKEN` set.

**Caddy**, which gets certificates automatically:

```text
proxmox-mcp.example.com {
    reverse_proxy proxmox-mcp:8080
}
```

**nginx:**

```nginx
server {
    listen 443 ssl;
    server_name proxmox-mcp.example.com;
    ssl_certificate     /etc/ssl/certs/proxmox-mcp.pem;
    ssl_certificate_key /etc/ssl/private/proxmox-mcp.key;

    location /mcp {
        proxy_pass http://proxmox-mcp:8080;
        proxy_set_header Host $host;
        proxy_read_timeout 600s;   # tools wait for Proxmox tasks (PROXMOX_TASK_TIMEOUT_MS)
    }
}
```

**Traefik** labels:

```yaml
services:
  proxmox-mcp:
    image: ghcr.io/mattoddie/proxmox-mcp:latest
    env_file: .env
    labels:
      - traefik.enable=true
      - traefik.http.routers.proxmox-mcp.rule=Host(`proxmox-mcp.example.com`)
      - traefik.http.routers.proxmox-mcp.tls.certresolver=letsencrypt
      - traefik.http.services.proxmox-mcp.loadbalancer.server.port=8080
```

When the server sits behind a proxy, don't publish its port. Set `MCP_ALLOWED_HOSTS=proxmox-mcp.example.com` so it only accepts requests for that hostname.

Tools that start a Proxmox task wait for it for up to `PROXMOX_TASK_TIMEOUT_MS` (two minutes by default), and some, such as guest agent commands, have their own timeouts. Make sure the proxy's read timeout is longer than the longest wait you've configured, or the client sees a gateway timeout while the task is still running.

> [!WARNING]
> Don't expose proxmox-mcp directly to the internet. Anyone with the token can control your cluster with the API token's permissions. For remote access, a VPN (WireGuard, Tailscale) is safer than a public endpoint.

## Hardening

The image is distroless (no shell or package manager), already runs as the unprivileged `nonroot` user (UID 65532) and doesn't need a writable filesystem, and the default `compose.yaml` already sets `read_only`, `cap_drop: [ALL]` and `no-new-privileges`. You can lock it down further:

```yaml
services:
  proxmox-mcp:
    image: ghcr.io/mattoddie/proxmox-mcp:1.4.2   # pinned
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    mem_limit: 256m
    pids_limit: 100
    env_file: .env
    ports: ["127.0.0.1:8080:8080"]
```

On the Proxmox side, the most effective hardening is a narrowly scoped API token. See [Security](security.md) for choosing permissions and toolsets.

## Running on the cluster itself

It's common to run proxmox-mcp in a small VM or LXC container on the cluster it manages. That works, with two things to keep in mind:

- If the assistant can stop or delete guests, it can stop or delete the one proxmox-mcp runs in. Turn on **Protection** in that guest's options to block deleting it, and to block stopping it too, keep it out of the token's reach: put the guests the assistant should manage in a pool and grant the token rights on that pool rather than on `/vms`.
- Point `PROXMOX_URL` at a node, or a load-balanced name, that stays up while the guest migrates.

Running Docker directly on a Proxmox node is possible but not recommended by Proxmox. Use a VM or container instead.

## Running without Docker

Docker is the supported way to run proxmox-mcp, but the server is a single static Go binary with no runtime dependencies. Build it with Go (the version in `go.mod`), or copy `/proxmox-mcp` out of the image:

```sh
git clone https://github.com/mattoddie/proxmox-mcp.git
cd proxmox-mcp
make build   # or: go build -o bin/proxmox-mcp ./cmd/proxmox-mcp
PROXMOX_URL=https://pve1:8006 PROXMOX_TLS_FINGERPRINT=AB:CD:... PROXMOX_API_TOKEN='mcp@pve!mcp=...' bin/proxmox-mcp   # stdio by default
MCP_TRANSPORT=http MCP_AUTH_TOKEN=... PROXMOX_URL=... PROXMOX_API_TOKEN=... bin/proxmox-mcp   # listens on 127.0.0.1:8080
```

Run this way, the HTTP server listens on `127.0.0.1` only. Set `MCP_HTTP_HOST=0.0.0.0` (or a specific address) to accept connections from other machines.
