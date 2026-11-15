# AGENTS.md

Notes for AI coding agents working on proxmox-mcp, an MCP server for Proxmox VE. [CONTRIBUTING.md](CONTRIBUTING.md) is the full contributor guide and [docs/development.md](docs/development.md) explains the architecture. Read them before larger changes.

## Commands

```sh
make lint         # gofmt and go vet
make test         # go test ./...
make docs         # regenerate the generated docs and manifest after changing tools
make docs-check   # CI's check that the generated files match the code
```

## Layout

- `cmd/proxmox-mcp/`: the server binary
- `internal/tools/`: the MCP tools, grouped by toolset
- `internal/proxmox/`: the Proxmox VE API client
- `docs/`: the user docs. `website/` (the site at proxmox-mcp.mattoddie.dev) copies them in at build time, so edit `docs/`, not the website.

## Rules

- The server is read-only by default. Every tool that changes something needs the right access level: see "Adding or changing tools" in CONTRIBUTING.md.
- Don't add dependencies without asking.
- Never put real secrets, hostnames, IP or MAC addresses in code, tests or fixtures. Use obviously fake values such as `192.0.2.1`.
- Add a line under **Unreleased** in CHANGELOG.md for anything users will notice. Tool names, arguments, prompts and configuration variables are the public API, so mark changes to them **Breaking:**.
- Commit messages and pull request titles use Conventional Commits. Pull requests are squash-merged.
