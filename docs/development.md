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

You need Node.js 22 or later (CI tests 22 and 24) and Docker for building the image. The image is built on the current Node.js LTS (`node:24-alpine`); Dependabot skips odd-numbered Node majors, which are short-lived "Current" releases, so it only proposes even-numbered majors, which become LTS.

```sh
git clone https://github.com/mattoddie/proxmox-mcp.git
cd proxmox-mcp
npm ci
npm test            # type-checks, builds and runs all tests against a mock Proxmox API
```

| Script | What it does |
|---|---|
| `npm run build` | Compile TypeScript from `src/` to `dist/` |
| `npm test` | Build, then run `dist/test/*.test.js` with the Node test runner |
| `npm start` | Run the built server (`node dist/index.js`) |
| `npm run docs:tools` | Regenerate [`docs/tools.md`](tools.md) from the tool definitions |
| `npm run docs:check` | Fail if `docs/tools.md` is out of date (CI runs this) |

The project deliberately has only three runtime dependencies: `@modelcontextprotocol/sdk`, `zod` and `undici`. There's no test framework, linter or bundler. Please discuss before adding a dependency.

## Project layout

```text
src/
├── index.ts            # entry point: load config, pick transport, handle signals
├── config.ts           # environment variables → typed Config (validation lives here)
├── server.ts           # builds the McpServer, its instructions, and registers every toolset and prompt
├── http.ts             # stateless Streamable HTTP transport, auth, Host allow-list, /health
├── prompts.ts          # the built-in MCP prompts
├── proxmox/
│   ├── client.ts       # ProxmoxClient: token/ticket auth, requests, errors, node/guest resolution, tasks
│   └── format.ts       # summary helpers: sizes, times, property strings, disks/NICs, redaction
├── tools/
│   ├── util.ts         # defineTool(), shared arguments (vmid, node, wait, extra, delete, digest…), runTask()
│   └── <toolset>.ts    # one file per toolset: cluster, nodes, vms, containers, agent, …
├── scripts/
│   └── gen-tool-docs.ts
└── test/
    ├── mock-proxmox.ts # an in-process fake Proxmox VE API with a two-node cluster
    ├── helpers.ts      # startEnv(), connect(), parse(), errorText()
    └── *.test.ts
```

## How a request flows

1. An MCP client calls a tool, for example `proxmox_vm_power {vmid: 101, action: "start"}`.
2. The MCP SDK validates the arguments against the tool's zod schema.
3. The handler registered by `defineTool()` runs. For guest tools it first calls `client.resolveGuest(vmid)`, which finds the guest's node and type in `/cluster/resources`. For node tools it calls `client.resolveNode(node)`.
4. `ProxmoxClient.request()` authenticates (a `PVEAPIToken` header, or a ticket cookie plus CSRF token that's renewed before it expires and on `401`), sends parameters the way the web UI does (query string for `GET`/`DELETE`, form body for `POST`/`PUT`), and unwraps the `{data}` envelope. Failures become a `ProxmoxError` with Proxmox's reason, any per-parameter errors and a hint.
5. If Proxmox returns a task ID (UPID), the handler passes it to `runTask()`, which waits for the task with `client.waitForTask()` unless `wait: false`, and turns a failed task into an error with the end of its log.
6. The handler summarises the result (`proxmox/format.ts`), redacts secrets and returns plain data.
7. `defineTool()` serialises that to JSON text. A thrown error becomes `isError: true` with the message, so the model can see what went wrong and adjust.

## Adding a tool

Tools are defined with `defineTool()` in the file for their toolset. Here's a complete example from `containers.ts`:

```ts
defineTool(ctx, "proxmox_resize_container_disk", {
  toolset: "containers",                  // which PROXMOX_TOOLSETS entry enables it
  title: "Resize a container disk",       // short human title
  description:
    "Grow a container's rootfs or mount point volume. size is absolute (\"20G\") or relative (\"+5G\"). Shrinking is not supported. The filesystem is grown automatically, also while running.",
  write: true,                            // only registered with PROXMOX_ALLOW_WRITES=true
  destructive: false,                     // growing a disk can't lose data
  input: {
    vmid: vmidArg,
    node: guestNodeArg,                   // optional; looked up from the VMID
    disk: volumeKeyArg,
    size: z.string().regex(/^\+?\d+(\.\d+)?[KMGT]?$/, "e.g. 20G or +5G").describe("New size, e.g. \"20G\", or an increase such as \"+5G\""),
    digest: digestArg,
    wait: waitArg,
  },
  handler: async (a) => {
    const g = await ct(a.vmid, a.node);   // resolveGuest(), insisting on a container
    const upid = await client.put(`${client.guestPath(g)}/resize`, defined({ disk: a.disk, size: a.size, digest: a.digest }));
    return runTask(client, upid, a.wait, { vmid: g.vmid, node: g.node, disk: a.disk, size: a.size });
  },
});
```

The `defineTool()` options that control registration:

| Option | Effect |
|---|---|
| `toolset` | Required. The tool is only registered when this toolset is enabled. |
| `write: true` | The tool changes state. Only registered when writes are enabled. Marked `readOnlyHint: false`. |
| `delete: true` | The tool deletes guests, data or configuration. Only registered when deletes are enabled too. Marked destructive. |
| `exec: true` | The tool runs commands or reads or writes files inside guests, or talks to the QEMU monitor. Only registered when exec is enabled too. Marked destructive unless `readOnly` is set. |
| `destructive` | Overrides the destructive hint for write tools, so clients confirm before running them. Set it to `true` for anything that stops, restarts, migrates, rolls back or reconfigures; `false` for harmless writes. |
| `idempotent: true` | Repeating the call with the same arguments has no further effect. Sets `idempotentHint`. |
| `readOnly` | Overrides the read-only hint. Used, for example, by exec tools that only read. |

`delete` and `exec` imply `write`, so a tool with `delete: true` is never registered without writes.

The shared arguments in `tools/util.ts` keep tools consistent:

| Argument | Use it for |
|---|---|
| `vmidArg`, `guestNodeArg` | Tools that act on a guest. Resolve with `client.resolveGuest(vmid, { node, type })`. |
| `nodeArg`, `nodeRequiredArg` | Node-scoped tools. Resolve with `client.resolveNode(node)`. |
| `waitArg` | Any tool whose API call returns a UPID. Pass the result through `runTask()`. |
| `searchArg`, `limitArg(n)`, `rawArg` | List tools. Use `filterAndLimit()` for consistent `{total, returned, items}` results. |
| `extraArg` | Raw Proxmox parameters for options the typed arguments don't cover. Merge them with `withExtra(params, a.extra, reserved?)` **before** `defined()`: it refuses keys the tool sets itself (even ones left undefined) and any `reserved` keys, so `extra` can't override checked or gated parameters. Run permission checks on the merged result. |
| `deleteKeysArg` | Config keys to remove, sent with `deleteParam()` as Proxmox's `delete` parameter. |
| `digestArg` | Optimistic locking for config updates. |

Checklist for a new tool:

1. Put it in the right `src/tools/<toolset>.ts`. If it needs a new toolset, add it to `TOOLSETS` in `config.ts`, describe it in `TOOLSET_INFO` in `scripts/gen-tool-docs.ts`, and register it in `server.ts`.
2. Mark its access level correctly with `write`, `delete` or `exec`, and set `destructive` deliberately.
3. Address guests by VMID and resolve the node; never make the model look up a node it doesn't need to know.
4. For anything that starts a task, offer `wait` and use `runTask()`.
5. For updates, change only the fields passed, and offer `delete`, `digest` and `extra` where they apply. For named objects, follow the `save_*` convention with `create: true`.
6. Return a **compact summary** and offer `raw: true` for the full object. Run anything that might contain secrets through `redactSecrets()`.
7. Write descriptions for the model: what the tool does, when to use it, units, side effects (*"the guest is stopped"*), and any version requirement (*"Proxmox VE 9+"*).
8. Add tests in `src/test/<toolset>.test.ts` using the mock.
9. Run `npm run docs:tools` and commit the updated `docs/tools.md`. If it's a notable feature, update the README table and [CHANGELOG](../CHANGELOG.md).

## Tool design guidelines

- **Fewer, broader tools beat many narrow ones.** One `proxmox_vm_power` with an `action` is easier for a model than separate start, stop, shutdown and reboot tools.
- **Never surprise.** Updates only change what was asked. Destructive actions are flagged. Deletes and exec are behind their own switches.
- **Errors should teach.** Throw messages that tell the model, or the user, how to fix the call: *"target is required for migrate"*. `ProxmoxError` already adds hints for `401`, `403` and `501`.
- **Respect Proxmox's own permissions.** Don't work around a `403`; report it. The token's ACLs are a security layer the user chose.
- **Be honest about versions.** If an endpoint only exists on some Proxmox VE versions, say so in the description and let the `501` or `404` explain itself.

## Testing

Tests use Node's built-in test runner against `MockProxmox` (`src/test/mock-proxmox.ts`), a small HTTP server that imitates the Proxmox VE API:

- A fixed two-node cluster (`pve1`, `pve2`) with VMs 100, 101 and template 9000, container 200 and three storages. `NODES`, `GUESTS`, `STORAGES`, `VM_100_CONFIG` and `CT_200_CONFIG` are exported for assertions.
- Token authentication by default, or `new MockProxmox({ auth: "password" })` for the ticket and CSRF flow (user `root@pam`, password `secret`).
- `on(method, regex, handler)` adds a route. Later routes take precedence over earlier ones and over the fixtures. Return the `data` payload, or `mock.error(status, reason, errors?)` for an HTTP error.
- `upid(node, type, id, task)` makes a task ID with a given final state and log. Task status and log lookups work for any UPID; unknown ones report `stopped`/`OK`.
- Unmatched `POST`, `PUT` and `DELETE` requests succeed with `null`, so simple write tools can be tested by inspecting what was sent. Unmatched `GET`s return `404`.
- `requests`, `requestsTo(prefix, method?)` and `lastWrite()` show what the server sent. Bodies are decoded from the form encoding.

`startEnv()` in `helpers.ts` starts the mock, a client pointed at it and an MCP client with **every** tool enabled, and returns `call()` (parses the JSON result and fails on errors) and `callRaw()` (for error assertions with `errorText()`). A typical test:

```ts
import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { errorText, startEnv, type TestEnv } from "./helpers.js";

describe("vm power", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv();
  });
  after(() => env.stop());
  beforeEach(() => env.mock.reset());

  it("starts a VM and waits for the task", async () => {
    // VM 101 is a stopped VM on pve2 in the fixtures; the tool finds its node itself.
    const upid = env.mock.upid("pve2", "qmstart", "101");
    env.mock.on("POST", /^\/nodes\/pve2\/qemu\/101\/status\/start$/, () => upid);
    const res = await env.call("proxmox_vm_power", { vmid: 101, action: "start" });
    assert.equal(res.status, "ok");
    assert.equal(env.mock.lastWrite()?.path, "/nodes/pve2/qemu/101/status/start");
  });

  it("reports a failed task with its log", async () => {
    const upid = env.mock.upid("pve2", "qmstart", "101", { status: "stopped", exitstatus: "start failed", log: ["kvm: out of memory"] });
    env.mock.on("POST", /\/qemu\/101\/status\/start$/, () => upid);
    const text = errorText(await env.callRaw("proxmox_vm_power", { vmid: 101, action: "start" }));
    assert.match(text, /out of memory/);
  });
});
```

To test that a tool is gated, connect with fewer permissions using `connect(client, toolOptions("read"))` and check it isn't listed. `toolOptions()` takes `"read"`, `"write"`, `"delete"`, `"exec"` or `"all"`.

Run a single file with `npm run build && node --test dist/test/vms.test.js`.

## Running against a real Proxmox

With the [MCP Inspector](https://github.com/modelcontextprotocol/inspector):

```sh
npm run build
PROXMOX_URL=https://pve1:8006 PROXMOX_API_TOKEN='mcp@pve!mcp=...' npx @modelcontextprotocol/inspector node dist/index.js
```

Or build the image and point Claude Code at it:

```sh
docker build -t proxmox-mcp:dev .
docker run --rm -p 8080:8080 --env-file .env proxmox-mcp:dev
claude mcp add --transport http proxmox-dev http://localhost:8080/mcp --header "Authorization: Bearer $MCP_AUTH_TOKEN"
```

Please test write tools on a lab cluster or throwaway guests, not production. Proxmox VE runs happily as a nested VM, which makes a good test bed, and a token scoped to a test pool keeps your other guests safe.

## Docs that are generated

[`docs/tools.md`](tools.md) is generated by `src/scripts/gen-tool-docs.ts`. The script starts the server in-process several times with different settings, to work out each tool's toolset and access level, and lists the registered tools and prompts. Don't edit `docs/tools.md` by hand. Change the tool's title, description or argument `.describe()` text and run `npm run docs:tools`. CI fails if the committed file is stale.

## The website

The site at [proxmox-mcp.mattoddie.dev](https://proxmox-mcp.mattoddie.dev) lives in [`website/`](../website/README.md). It has a hand-built landing page plus these docs rendered with Astro Starlight. It has its own `package.json`, so its dependencies never reach the server or the Docker image.

The Markdown in `docs/` is the single source: `website/scripts/sync-docs.mjs` copies it into the site at build time. It turns each page's first `# Heading` into the page title, drops hand-written "Contents" lists, rewrites links between `.md` files to site URLs and converts GitHub alerts (`> [!NOTE]`) to Starlight asides. So write docs as normal GitHub Markdown, start each page with a `# Title`, and link between pages with relative `.md` links.

To preview the site locally:

```sh
cd website
npm ci
npm run dev
```
