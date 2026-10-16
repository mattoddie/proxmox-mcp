# Contributing to proxmox-mcp

Thanks for wanting to help! proxmox-mcp is a community project, and every kind of contribution is welcome: bug reports, compatibility reports from your cluster, documentation fixes, new tools and code review.

By taking part you agree to follow the [code of conduct](CODE_OF_CONDUCT.md).

- [Ways to contribute](#ways-to-contribute)
- [Development setup](#development-setup)
- [Making a change](#making-a-change)
- [Pull request guidelines](#pull-request-guidelines)
- [Commit messages](#commit-messages)
- [Adding or changing tools](#adding-or-changing-tools)
- [Releases](#releases)
- [Getting help](#getting-help)

## Ways to contribute

- **Report a bug.** Use the [bug report form](https://github.com/mattoddie/proxmox-mcp/issues/new?template=bug_report.yml). Include your Proxmox VE version, whether it's a single node or a cluster, the authentication method and the exact error, and strip out token secrets, passwords and public IPs.
- **Report compatibility.** Proxmox setups vary a lot: versions, storage types, Ceph, HA, SDN, PBS. A [compatibility report](https://github.com/mattoddie/proxmox-mcp/issues/new?template=compatibility.yml) saying *"works on a 3-node PVE 8.4 cluster with Ceph"* is genuinely useful.
- **Suggest a feature.** Open a [feature request](https://github.com/mattoddie/proxmox-mcp/issues/new?template=feature_request.yml). If you know the API endpoint involved, from the [API viewer](https://pve.proxmox.com/pve-docs/api-viewer/) or your browser's dev tools while using the web UI, include it. It's often most of the work.
- **Improve the docs.** Typos, unclear steps and missing examples are all fair game. Small docs fixes can go straight to a pull request.
- **Write code.** Look for issues labelled [`good first issue`](https://github.com/mattoddie/proxmox-mcp/labels/good%20first%20issue) or [`help wanted`](https://github.com/mattoddie/proxmox-mcp/labels/help%20wanted). For anything big, please open an issue first so we can agree on the approach before you invest time.
- **Report a vulnerability privately.** See [SECURITY.md](SECURITY.md). Please don't open a public issue for it.

## Development setup

You need **Go** (the version in `go.mod`; Go downloads the pinned toolchain itself) and, to build the image, **Docker**.

```sh
git clone https://github.com/<you>/proxmox-mcp.git   # your fork
cd proxmox-mcp
make test
```

`make test` runs the whole suite against a built-in mock Proxmox API in a few seconds. You don't need a real Proxmox server to contribute.

See [docs/development.md](docs/development.md) for the architecture, the mock and a walkthrough of adding a tool.

## Making a change

1. **Fork** the repository and create a branch from `main`, for example `git checkout -b feat/ha-rules`.
2. **Make your change** with tests. Keep it focused: one feature or fix per pull request.
3. **Run the checks** that CI runs:

   ```sh
   make lint            # gofmt + go vet
   make test            # tests (CI also runs them with -race)
   make docs-check      # docs/tools.md matches the code
   docker build .       # optional: the image builds and the tests pass inside it
   ```

4. **Update the docs.** If you changed tools, run `make docs`. Update `README.md` or `docs/` if behaviour changed, and add a line under **Unreleased** in [CHANGELOG.md](CHANGELOG.md) for anything users will notice.
5. **Open a pull request** against `main` and fill in the template.

## Pull request guidelines

- CI must be green. It runs gofmt, go vet and the tests (with the race detector), checks the generated docs, builds the image for amd64 and arm64, and smoke-tests the container.
- Match the existing style: `gofmt`, small focused functions, and comments that explain *why* rather than *what*.
- **No new dependencies without discussion.** The project deliberately stays small. The only direct dependency is the official Go MCP SDK (`github.com/modelcontextprotocol/go-sdk`).
- Never include real token secrets, passwords, hostnames, MAC addresses, public IPs or other personal infrastructure data in code, tests, fixtures or screenshots. Use obviously fake values like `pve1`, `BC:24:11:00:00:01` and `192.0.2.1`.
- Maintainers may push small fixes to your branch or squash commits when merging.
- Label suggestions (`bug`, `enhancement`, `new-tool`, `documentation`, `breaking`, …) are welcome. Labels sort the auto-generated release notes.

## Commit messages

- Use the imperative mood with a subject of 72 characters or fewer, for example *"Add HA rule tools"* or *"Fix container clone ignoring target storage"*.
- Add a body that explains **why** when it isn't obvious.
- Reference issues with `Fixes #123` where relevant.

## Adding or changing tools

The full guide is in [docs/development.md](docs/development.md#adding-a-tool). The short version:

- Put the tool in the right toolset file under `internal/tools/`, using `c.Define()`.
- Mark its access level correctly: `Write: true` for anything that changes state, `Delete: true` for anything that deletes guests, data or configuration, and `Exec: true` for anything that runs commands or reads or writes files inside guests. Set `Destructive` for writes that stop, restart, migrate, roll back or reconfigure. **This gating is the project's main safety feature, so reviewers check it carefully.**
- Address guests by VMID and resolve the node automatically. Offer `wait` for anything that starts a Proxmox task.
- Updates change only the fields passed. Offer `delete`, `digest` and `extra` where they apply.
- Return compact summaries, offer `raw: true`, and redact secrets.
- Write the description for an AI model: what the tool does, side effects, units and version requirements.
- Add tests against the mock, then run `make docs`.

Changing an existing tool's name or arguments in an incompatible way, or moving it to a less restrictive access level, is a **breaking change**. Call it out in the PR and the changelog.

## Releases

Releases are made by maintainers by publishing a GitHub release. That triggers the workflow that builds and pushes the Docker image. See [docs/releasing.md](docs/releasing.md).

## Getting help

Ask in [GitHub Discussions](https://github.com/mattoddie/proxmox-mcp/discussions), or comment on the issue you're working on. No question is too small.
