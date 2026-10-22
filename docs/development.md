# Development

This page is for people working on proxmox-mcp itself. Start with [CONTRIBUTING.md](../CONTRIBUTING.md) for the workflow, and use this page for how the code fits together.

- [Setup](#setup)
- [Project layout](#project-layout)
- [How a request flows](#how-a-request-flows)
- [Adding a tool](#adding-a-tool)
- [Tool design guidelines](#tool-design-guidelines)
- [Testing](#testing)
- [Running against a real Proxmox](#running-against-a-real-proxmox)
- [Docs that are generated](#docs-that-are-generated)
- [The website](#the-website)

## Setup

You need Go (the version in `go.mod`; `go` downloads the pinned toolchain itself) and Docker for building the image. The image is a static binary on [distroless](https://github.com/GoogleContainerTools/distroless) `static-debian13:nonroot`. Both base images in the Dockerfile are pinned by digest (Dependabot updates them), and the build image's Go version matches the `toolchain` in `go.mod`.

```sh
git clone https://github.com/mattoddie/proxmox-mcp.git
cd proxmox-mcp
make test           # runs all tests against a mock Proxmox API
```

| Command | What it does |
|---|---|
| `make build` | Build `bin/proxmox-mcp` |
| `make test` | `go test ./...` |
| `make lint` | `gofmt` and `go vet` (CI runs this) |
| `make docs` | Regenerate [`docs/tools.md`](tools.md) from the tool definitions |
| `make docs-check` | Fail if `docs/tools.md` is out of date (CI runs this) |
| `make vulncheck` | Scan the dependencies with `govulncheck` (pinned; CI runs it too) |
| `make image` | Build the Docker image |

The binary has subcommands: `proxmox-mcp` (or `proxmox-mcp serve`) runs the server, `proxmox-mcp healthcheck` is the image's health check and `proxmox-mcp version` prints the version, which release builds set with `-ldflags`.

The project deliberately has one direct dependency, the official [Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk). There's no test framework or linter beyond the Go toolchain. Please discuss before adding a dependency.

## Project layout

```text
cmd/
├── proxmox-mcp/        # entry point: load config, pick transport, handle signals; healthcheck and version
├── gen-tool-docs/      # writes docs/tools.md
└── mcpb/               # keeps mcpb/manifest.json in step with the tools, and packs the Claude Desktop bundles
internal/
├── config/             # environment variables → typed Config (validation lives here)
├── httpserver/         # stateless Streamable HTTP transport, auth, Host/Origin allow-list, /health
├── js/                 # JSON values with JavaScript semantics (ordered objects, undefined, number formatting)
├── proxmox/
│   ├── client.go       # Client: token/ticket auth, requests, errors, TLS pinning, node/guest resolution, tasks
│   ├── format.go       # summary helpers: sizes, times, property strings, disks/NICs, redaction
│   ├── dryrun.go       # dry runs and the record of state-changing requests, per tool call
│   └── proxmoxtest/    # an in-process fake Proxmox VE API with a two-node cluster
└── tools/
    ├── tools.go        # NewServer(), its instructions, Define() and the access gates
    ├── schema.go       # argument schemas: JSON Schema output and validation
    ├── helpers.go      # shared arguments (vmid, node, wait, extra, delete, digest…), withExtra(), runTask()
    ├── guard.go        # dry_run and the audit log for tools that change things
    ├── list.go         # the listing helper: search, limit, fields and {total, returned, items}
    ├── prompts.go      # the built-in MCP prompts
    ├── <toolset>.go    # one file per toolset: cluster, nodes, vms, containers, agent, …
    └── *_test.go       # tests against proxmoxtest, plus testdata/
```

The server was first written in TypeScript. The tools reshape loosely typed API data, so `internal/js` keeps the JavaScript semantics the original code relied on (key order, `undefined` versus `null`, `Number()`/`String()` conversions, `toFixed`), which keeps the output identical. `testdata/typescript-reference.json` records what the TypeScript server listed (every tool's schema, annotations and access level, and the prompts), and the reference tests check the Go server against it.

## How a request flows

1. An MCP client calls a tool, for example `proxmox_vm_power {vmid: 101, action: "start"}`.
2. The handler that `Define()` registers validates the arguments against the tool's schema (types, ranges, enums, patterns, required arguments) and drops unknown ones.
3. The tool's handler runs. For guest tools it first calls `client.ResolveGuest()`, which finds the guest's node and type in `/cluster/resources`. For node tools it calls `client.ResolveNode()`.
4. `Client.Request()` authenticates (a `PVEAPIToken` header, or a ticket cookie plus CSRF token that's renewed before it expires and on `401`), sends parameters the way the web UI does (query string for `GET`/`DELETE`, form body for `POST`/`PUT`), never follows redirects, and unwraps the `{data}` envelope. Failures become a `*proxmox.Error` with Proxmox's reason, any per-parameter errors and a hint.
5. If Proxmox returns a task ID (UPID), the handler passes it to `runTask()`, which waits for the task with `client.WaitForTask()` unless `wait: false`, and turns a failed task into an error with the end of its log.
6. The handler summarises the result (`proxmox/format.go`), redacts secrets and returns plain data.
7. `Define()` serialises that to JSON text. A returned error becomes `isError: true` with the message, so the model can see what went wrong and adjust.

## Adding a tool

Tools are defined with `c.Define()` in the file for their toolset. Here's an example in the style of `containers.go`:

```go
c.Define("proxmox_resize_container_disk", Def{
	Toolset:     "containers",             // which PROXMOX_TOOLSETS entry enables it
	Title:       "Resize a container disk", // short human title
	Description: "Grow a container's rootfs or mount point volume. size is absolute (\"20G\") or relative (\"+5G\"). ...",
	Write:       true,                     // only registered with PROXMOX_ALLOW_WRITES=true
	Destructive: False,                    // growing a disk can't lose data
	Input: []Prop{
		P("vmid", vmidArg),
		P("node", guestNodeArg),           // optional; looked up from the VMID
		P("disk", Str().Pattern(`^(rootfs|mp\d+)$`, "must be rootfs or mpN").D("Volume to resize")),
		P("size", Str().Pattern(`^\+?\d+(\.\d+)?[KMGT]?$`, "e.g. 20G or +5G").D("New size, e.g. \"20G\", or an increase such as \"+5G\"")),
		P("digest", digestArg),
		P("wait", waitArg),
	},
	Handler: func(ctx context.Context, a *Obj) (any, error) {
		g, err := c.guest(ctx, a, "lxc") // ResolveGuest(), insisting on a container
		if err != nil {
			return nil, err
		}
		upid, err := client.Put(ctx, client.GuestPath(g)+"/resize", defined(js.O("disk", a.Get("disk"), "size", a.Get("size"), "digest", a.Get("digest"))))
		if err != nil {
			return nil, err
		}
		return runTask(ctx, client, upid, a.Get("wait"), js.O("vmid", g.VMID, "node", g.Node, "disk", a.Get("disk"), "size", a.Get("size")))
	},
})
```

Arguments arrive as an ordered `*js.Obj`: an omitted argument is `js.Undef`, so `a.Get("x")` can go straight into a parameter object and `defined()` drops it. Build results with `js.O(...)` in the order the fields should appear, and `proxmox.C(...)` (or `proxmox.Compact`) to drop empty ones.

The `Def` fields that control registration:

| Field | Effect |
|---|---|
| `Toolset` | Required. The tool is only registered when this toolset is enabled. |
| `Write: true` | The tool changes state. Only registered when writes are enabled. Marked `readOnlyHint: false`. |
| `Delete: true` | The tool deletes guests, data or configuration. Only registered when deletes are enabled too. Marked destructive. |
| `Exec: true` | The tool runs commands or reads or writes files inside guests, or talks to the QEMU monitor. Only registered when exec is enabled too. Marked destructive unless `ReadOnly` is set. |
| `Destructive` | Overrides the destructive hint for write tools, so clients confirm before running them. Set it to `True` for anything that stops, restarts, migrates, rolls back or reconfigures; `False` for harmless writes. |
| `Idempotent: true` | Repeating the call with the same arguments has no further effect. Sets `idempotentHint`. |
| `ReadOnly` | Overrides the read-only hint. Used, for example, by exec tools that only read. |
| `List` | Makes it a list tool: see below. |
| `Reads` | For a tool that changes things only for some arguments (the raw API tool), says which calls are reads, so they aren't audited. |
| `NativeDryRun` | The tool has its own `dry_run` that Proxmox runs (realm sync), so the server doesn't add one. |

`Delete` and `Exec` imply `Write`, so a tool with `Delete: true` is never registered without writes.

Every tool that changes something gets a `dry_run` argument automatically, and its calls go to the audit log. In a dry run `Client.Request()` sends reads but stops the first request that would change something, and the tool returns a preview of it instead. A `POST` that only reads (such as the guest agent's ping) must use `proxmox.AsRead(ctx)` so a dry run still sends it.

List tools set `List: &listing{}`. The handler returns its items as an array, or as an object holding the array under `listing.Items` (such as `{node, pools: [...]}`), and the listing adds `search`, `fields` and, with `Limit` set, `limit`, then shapes the result as `{total, returned, items}`. Tools that already filter with `filterAndLimit()` keep their own `search` and `limit` and only gain `fields`.

The shared arguments in `tools/helpers.go` keep tools consistent:

| Argument | Use it for |
|---|---|
| `vmidArg`, `guestNodeArg` | Tools that act on a guest. Resolve with `c.guest(ctx, a, "qemu" \| "lxc" \| "")`. |
| `nodeArg`, `nodeRequiredArg` | Node-scoped tools. Resolve with `c.node(ctx, a)`. |
| `waitArg` | Any tool whose API call returns a UPID. Pass the result through `runTask()`. |
| `searchArg`, `limitArg(n)`, `rawArg` | List tools with their own filtering. Use `filterAndLimit()` and set `List`. |
| `extraArg` | Raw Proxmox parameters for options the typed arguments don't cover. Merge them with `withExtra(params, a.Get("extra"), reserved...)` **before** `defined()` (or use `definedExtra()`): it refuses keys the tool sets itself (even ones left undefined) and any `reserved` keys, so `extra` can't override checked or gated parameters. Run permission checks on the merged result. |
| `deleteKeysArg` | Config keys to remove, sent with `deleteParam()` as Proxmox's `delete` parameter. |
| `digestArg` | Optimistic locking for config updates. |

Checklist for a new tool:

1. Put it in the right `internal/tools/<toolset>.go`. If it needs a new toolset, add it to `config.Toolsets`, describe it in `toolsetInfo` in `cmd/gen-tool-docs`, and register it in `NewServer()`.
2. Mark its access level correctly with `Write`, `Delete` or `Exec`, and set `Destructive` deliberately.
3. Address guests by VMID and resolve the node; never make the model look up a node it doesn't need to know.
4. For anything that starts a task, offer `wait` and use `runTask()`.
5. For updates, change only the fields passed, and offer `delete`, `digest` and `extra` where they apply. For named objects, follow the `save_*` convention with `create: true`.
6. Return a **compact summary** and offer `raw: true` for the full object. Run anything that might contain secrets through `proxmox.RedactSecrets()`.
7. Write descriptions for the model: what the tool does, when to use it, units, side effects (*"the guest is stopped"*), and any version requirement (*"Proxmox VE 9+"*).
8. Add tests in `internal/tools/<toolset>_test.go` using the mock.
9. Run `make docs` and commit the updated `docs/tools.md`. Tools that aren't in `internal/tools/testdata/typescript-reference.json` fail the reference test's "no extra tools" check, so add an entry for a new tool there too (or, after a deliberate change to an existing tool, update its entry). If it's a notable feature, update the README table and [CHANGELOG](../CHANGELOG.md).

## Tool design guidelines

- **Fewer, broader tools beat many narrow ones.** One `proxmox_vm_power` with an `action` is easier for a model than separate start, stop, shutdown and reboot tools.
- **Never surprise.** Updates only change what was asked. Destructive actions are flagged. Deletes and exec are behind their own switches.
- **Errors should teach.** Return messages that tell the model, or the user, how to fix the call: *"target is required for migrate"*. `proxmox.Error` already adds hints for `401`, `403` and `501`.
- **Respect Proxmox's own permissions.** Don't work around a `403`; report it. The token's ACLs are a security layer the user chose.
- **Be honest about versions.** If an endpoint only exists on some Proxmox VE versions, say so in the description and let the `501` or `404` explain itself.

## Testing

Tests use the standard `go test` against `proxmoxtest.Mock` (`internal/proxmox/proxmoxtest`), a small HTTP server that imitates the Proxmox VE API:

- A fixed two-node cluster (`pve1`, `pve2`) with VMs 100, 101 and template 9000, container 200 and three storages. `NodesJSON`, `GuestsJSON`, `StoragesJSON`, `VM100ConfigJSON` and `CT200ConfigJSON` are exported for assertions.
- Token authentication by default, or `proxmoxtest.New(t, "password")` for the ticket and CSRF flow (user `root@pam`, password `secret`).
- `On(method, regexp, handler)` adds a route and `Reply(method, regexp, data)` one with a fixed answer. Later routes take precedence over earlier ones and over the fixtures. Return the `data` payload, or `proxmoxtest.Error(status, reason, errors...)` for an HTTP error.
- `Upid(node, type, id, task...)` makes a task ID with a given final state and log. Task status and log lookups work for any UPID; unknown ones report `stopped`/`OK`.
- Unmatched `POST`, `PUT` and `DELETE` requests succeed with `null`, so simple write tools can be tested by inspecting what was sent. Unmatched `GET`s return `404`.
- `Requests()`, `RequestsTo(prefix, method)`, `Last()` and `LastWrite()` show what the server sent. Bodies are decoded from the form encoding.

`startEnv(t)` in `env_test.go` starts the mock, a client pointed at it and an MCP session with **every** tool enabled; `env.call(t, name, args)` parses the JSON result (failing on errors) and `env.errorText(t, name, args)` asserts an error. Arguments can be a JSON string or a `js.O(...)`. A typical test:

```go
func TestVmPower(t *testing.T) {
	env := startEnv(t)

	t.Run("starts a VM and waits for the task", func(t *testing.T) {
		// VM 101 is a stopped VM on pve2 in the fixtures; the tool finds its node itself.
		upid := env.mock.Upid("pve2", "qmstart", "101")
		env.mock.Reply("POST", `^/nodes/pve2/qemu/101/status/start$`, upid)
		res := env.call(t, "proxmox_vm_power", `{"vmid":101,"action":"start"}`)
		eqAt(t, res, "status", "ok")
		eqStr(t, env.mock.LastWrite().Path, "/nodes/pve2/qemu/101/status/start")
	})

	t.Run("reports a failed task with its log", func(t *testing.T) {
		upid := env.mock.Upid("pve2", "qmstart", "101", proxmoxtest.Task{Status: "stopped", ExitStatus: "start failed", Log: []string{"kvm: out of memory"}})
		env.mock.Reply("POST", `/qemu/101/status/start$`, upid)
		match(t, env.errorText(t, "proxmox_vm_power", `{"vmid":101,"action":"start"}`), `out of memory`)
	})
}
```

To test that a tool is gated, connect with fewer permissions using `connect(t, client, toolOptions("read"))` and check it isn't listed. `toolOptions()` takes `"read"`, `"write"`, `"delete"`, `"exec"` or `"all"`.

Run one toolset's tests with `go test ./internal/tools -run 'TestVms|TestReferenceTools/vms/'`. CI also runs the suite with the race detector (`CGO_ENABLED=1 go test -race ./...`).

## Running against a real Proxmox

With the [MCP Inspector](https://github.com/modelcontextprotocol/inspector):

```sh
make build
PROXMOX_URL=https://pve1:8006 PROXMOX_VERIFY_SSL=false PROXMOX_API_TOKEN='mcp@pve!mcp=...' npx @modelcontextprotocol/inspector bin/proxmox-mcp
```

Or build the image and point Claude Code at it:

```sh
docker build -t proxmox-mcp:dev .
docker run --rm -p 8080:8080 --env-file .env proxmox-mcp:dev
claude mcp add --transport http proxmox-dev http://localhost:8080/mcp --header "Authorization: Bearer $MCP_AUTH_TOKEN"
```

Please test write tools on a lab cluster or throwaway guests, not production. Proxmox VE runs happily as a nested VM, which makes a good test bed, and a token scoped to a test pool keeps your other guests safe.

## Docs that are generated

[`docs/tools.md`](tools.md) is generated by `cmd/gen-tool-docs`. It starts the server in-process with every tool enabled, lists the registered tools and prompts, and takes each tool's toolset and access level from its definition. Don't edit `docs/tools.md` by hand. Change the tool's title, description or argument `.D()` text and run `make docs`. CI fails if the committed file is stale.

The tool and prompt lists in [`mcpb/manifest.json`](../mcpb/manifest.json), the Claude Desktop bundle manifest, are generated the same way by `cmd/mcpb manifest`, which starts the server with the manifest's default settings. `make docs` updates both. Edit the rest of the manifest (settings, descriptions) by hand. `make mcpb VERSION=x.y.z` builds the bundles into `dist/`.

## The website

The site at [proxmox-mcp.mattoddie.dev](https://proxmox-mcp.mattoddie.dev) lives in [`website/`](../website/README.md). It has a hand-built landing page plus these docs rendered with Astro Starlight. It has its own `package.json`, so its dependencies never reach the server or the Docker image.

The Markdown in `docs/` is the single source: `website/scripts/sync-docs.mjs` copies it into the site at build time. It turns each page's first `# Heading` into the page title, drops hand-written "Contents" lists, rewrites links between `.md` files to site URLs and converts GitHub alerts (`> [!NOTE]`) to Starlight asides. So write docs as normal GitHub Markdown, start each page with a `# Title`, and link between pages with relative `.md` links.

To preview the site locally:

```sh
cd website
npm ci
npm run dev
```
