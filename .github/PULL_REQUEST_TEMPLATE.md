<!-- Title: use Conventional Commits, e.g. "fix(containers): honour the target storage when cloning". It becomes the commit on main. See CONTRIBUTING.md. -->

## What does this change?

<!-- A short description of the change and why it's needed. Link any related issue: "Fixes #123". -->

## How was it tested?

<!-- e.g. new unit tests against the mock, `make test`, and/or tried against a real Proxmox VE (version, single node or cluster). -->

## Checklist

- [ ] `make lint` and `make test` pass
- [ ] New or changed tools have tests, and `make docs` has been run
- [ ] New tools have the right access level (`write`, `delete` or `exec`) and `destructive` flag
- [ ] Docs updated where behaviour changed (README / `docs/`)
- [ ] `CHANGELOG.md` has an entry under **Unreleased** for user-facing changes
- [ ] No token secrets, passwords or personal infrastructure details in code, tests or screenshots
