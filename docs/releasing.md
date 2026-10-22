# Releasing

Releases are driven by **GitHub releases**. Publishing a release with a tag like `v1.2.3` triggers the [Release workflow](../.github/workflows/release.yml). That workflow builds the Docker image for `linux/amd64` and `linux/arm64` and pushes it to `ghcr.io/mattoddie/proxmox-mcp`, attaches the MCP Bundles to the release and, for stable releases, publishes the server to the [MCP Registry](https://registry.modelcontextprotocol.io) as `io.github.mattoddie/proxmox-mcp`. Nothing is published by merging to `main` alone.

- [Versioning](#versioning)
- [Release checklist](#release-checklist)
- [Pre-releases](#pre-releases)
- [What the workflow does](#what-the-workflow-does)
- [If something goes wrong](#if-something-goes-wrong)
- [Repository setup](#repository-setup)

## Versioning

The project follows [Semantic Versioning](https://semver.org). Its public API is the set of **tool names, their arguments, prompts and the environment variables**:

| Change | Bump | Examples |
|---|---|---|
| Breaking | **major** (`2.0.0`) | Renaming or removing a tool, argument, prompt or variable; moving a tool to a less restrictive access level; changing a default so that more is allowed |
| New features | **minor** (`1.3.0`) | New tools, toolsets, arguments or prompts |
| Fixes | **patch** (`1.2.4`) | Bug fixes, compatibility fixes, docs, dependency updates |

While the version is `0.x`, minor bumps may contain breaking changes. Call them out in the release notes.

The version comes **from the tag**. `package.json` stays at `0.0.0-dev` in the repository, and release builds stamp the tag's version into the image. The server reports it to MCP clients and prints it on its first log line.

## Release checklist

1. **Check `main` is green** on [CI](https://github.com/mattoddie/proxmox-mcp/actions/workflows/ci.yml).
2. **Update the changelog.** In a small PR titled `chore(release): v1.2.0`, move the **Unreleased** entries in [`CHANGELOG.md`](../CHANGELOG.md) under a new heading, `## [1.2.0] - YYYY-MM-DD`, and update the comparison links at the bottom. Fill in the final tool and prompt counts from the top of [`docs/tools.md`](tools.md). Merge it.
3. **Publish the release.** Use either the CLI or the web UI.

   With the CLI:

   ```sh
   gh release create v1.2.0 --target main --title "v1.2.0" --generate-notes
   ```

   Or in the web UI: **Releases → Draft a new release → Choose a tag → type `v1.2.0` → Create new tag on publish**, target `main`. Click **Generate release notes**, edit them as needed, then **Publish release**.

4. **Watch the workflow.** Open **Actions → Release**. It takes a few minutes. When it finishes, the release notes gain a *Container image* section with the pull command, digest and tags. Then the MCP Bundles for Claude Desktop are built, attached to the release and listed in a *Claude Desktop* section. Last, a stable release is published to the MCP Registry.
5. **Check the image:** `docker pull ghcr.io/mattoddie/proxmox-mcp:1.2.0`.
6. **Check the registry entry:** `curl -s "https://registry.modelcontextprotocol.io/v0/servers?search=io.github.mattoddie/proxmox-mcp&version=latest" | jq '.servers[].server.version'` prints `"1.2.0"`.

Release notes are generated from merged PR titles and grouped by label. The groups are set in [`.github/release.yml`](../.github/release.yml). Label PRs `breaking`, `enhancement`, `new-tool`, `bug`, `compatibility`, `documentation` or `dependencies` to sort them, or `skip-changelog` to leave a PR out.

## Pre-releases

To test a release candidate, use a pre-release tag and tick **Set as a pre-release**:

```sh
gh release create v1.3.0-rc.1 --target main --prerelease --generate-notes
```

Pre-releases are published only under their exact version (`ghcr.io/mattoddie/proxmox-mcp:1.3.0-rc.1`). They never move `latest`, `1` or `1.3`. They get MCP Bundles but aren't published to the MCP Registry, where a published version can't be changed or reused.

## What the workflow does

| Step | Detail |
|---|---|
| Validate the tag | Must match `vMAJOR.MINOR.PATCH` with an optional `-prerelease` suffix, otherwise the workflow fails before building. |
| Build | `docker buildx` for `linux/amd64` and `linux/arm64`, from scratch with no build cache. The test suite runs inside the build, so failing tests mean no image. |
| Tag | `X.Y.Z` always. For stable releases also `X.Y`, `X` (from `1.0.0` on) and `latest`. |
| Push | To `ghcr.io/mattoddie/proxmox-mcp` using the workflow's `GITHUB_TOKEN`. No personal tokens are needed. |
| Supply chain | Attaches an SBOM and SLSA provenance to the image, and pushes a signed build provenance attestation (`gh attestation verify`). |
| Release notes | Appends the pull command, digest and tags. |
| MCP Bundles | After the image, `make mcpb` builds one `.mcpb` per platform (macOS, Windows and Linux, amd64 and arm64), validates each manifest with the `mcpb` CLI, attests them and uploads them to the release. |
| MCP Registry | Stable releases only, after the image and the bundles. Downloads the bundles from the release, stamps the version, image tag and each bundle's SHA-256 into [`server.json`](../server.json) (`make server-json`), installs the pinned `mcp-publisher`, checks its checksum, validates the file, logs in with GitHub OIDC (no secrets) and publishes. |

## If something goes wrong

- **The workflow failed** for a transient reason: re-run it from the Actions tab. It's safe to re-run, and it pushes the same tags again.
- **The release was wrong** (bad code or wrong tag): don't move or reuse a published tag, because people may already have pulled it. Fix the problem on `main` and publish the next patch version. You can delete the bad GitHub release and mark it as such in the changelog, but leave the image tag alone.
- **Wrong tag format** (e.g. `1.2.0` without the `v`): delete the release and tag (`gh release delete 1.2.0 --cleanup-tag`) and publish again with the right tag.
- **The MCP Registry job failed.** Its log says why. A failure before *Publish* (the download, the checksum, validation, or the registry being unavailable) is safe to retry: open the workflow run and click **Re-run failed jobs**, which re-runs only *Publish to the MCP Registry*. If the version already reached the registry, the job notices and skips *Publish*. A published version can't be changed: if its `server.json` was wrong, fix it on `main` and release the next patch version.

### Publishing to the MCP Registry by hand

If the job can't publish (for example, GitHub OIDC is down), publish from your own machine. You need `gh`, `jq`, `make`, `sha256sum` and `mcp-publisher` from the [registry's releases](https://github.com/modelcontextprotocol/registry/releases) at the version pinned in the Release workflow, checked against the release's `registry_<version>_checksums.txt`. Then, from a clean checkout of the release tag:

```sh
git checkout v1.2.0
gh release download v1.2.0 --pattern '*.mcpb' --dir dist
make server-json VERSION=1.2.0          # writes dist/server.json
mcp-publisher validate dist/server.json
mcp-publisher login github              # in the browser, as an owner of the mattoddie organisation
mcp-publisher publish dist/server.json
mcp-publisher logout
```

The bundles must come from the release (not a local `make mcpb`), because `server.json` carries their SHA-256 checksums and clients check them before installing.

## Repository setup

### Rulesets

Two rulesets protect the repository. You can view and edit them under **Settings → Rules → Rulesets**, and that's where to create them if you're setting up a fresh copy.

**Protect main** (the default branch):

- Changes land only through pull requests. Nobody can push directly, force-push or delete the branch, and there are no bypasses.
- The CI checks *Test* and *Docker image* must pass, and the branch must be up to date with `main`.
- Review threads must be resolved before merging. The only merge method is squash.
- No approving review is required yet, because GitHub doesn't let authors approve their own pull requests and the project has a single maintainer. Raise this when there are more maintainers.

**Protect release tags** (`v*`):

- Only repository admins can create, move or delete version tags, so only maintainers can publish releases. Published versions can't be re-pointed.

> [!NOTE]
> Required checks are matched by job name. If you rename a job in [`ci.yml`](../.github/workflows/ci.yml), update the *Protect main* ruleset in the same change. Otherwise pull requests wait forever for a check that never reports.

### Container package visibility

New GHCR packages normally start private, and GitHub doesn't offer an API to change this. A package linked to a public repository usually becomes public automatically. If it starts private, open the package page (**Your profile → Packages → proxmox-mcp → Package settings**) and choose **Change visibility → Public** under **Danger Zone**.

The package links to the repository automatically through the image's `org.opencontainers.image.source` label, which makes it show in the repository sidebar.

### MCP Registry

The server is listed in the [MCP Registry](https://registry.modelcontextprotocol.io) as `io.github.mattoddie/proxmox-mcp`, described by [`server.json`](../server.json). The Release workflow logs in with GitHub OIDC, which grants the `io.github.mattoddie/*` namespace to workflows in the organisation's repositories, so there are no secrets to set up. The registry checks that the image belongs to the server through its `io.modelcontextprotocol.server.name` label (set in the [Dockerfile](../Dockerfile)), and that the image is public.

`server.json` stays at version `0.0.0-dev` in the repository, like the bundle manifest; `make server-json` stamps the release's version into it. A test (`TestServerJSON` in `cmd/mcpb`) keeps its settings in line with the bundle's, so a new setting goes in both [`mcpb/manifest.json`](../mcpb/manifest.json) and `server.json`. CI validates the stamped file with `mcp-publisher validate`. To update `mcp-publisher`, change `MCP_PUBLISHER_VERSION` and `MCP_PUBLISHER_SHA256` in both [`ci.yml`](../.github/workflows/ci.yml) and [`release.yml`](../.github/workflows/release.yml).
