# Security policy

proxmox-mcp controls virtualisation infrastructure, so security reports are taken seriously and handled as a priority.

## Supported versions

Security fixes go into the latest release. Please upgrade to the newest version before reporting.

| Version | Supported |
|---|---|
| Latest release | ✅ |
| Older releases | ❌ |

## Reporting a vulnerability

**Please don't report security vulnerabilities in public issues, discussions or pull requests.**

Report them privately through GitHub's **[private vulnerability reporting](https://github.com/mattoddie/proxmox-mcp/security/advisories/new)**: go to the repository's **Security** tab and click **Report a vulnerability**.

Please include:

- A description of the issue and its impact
- Steps to reproduce, or a proof of concept
- The affected version(s) and configuration, such as transport, write/delete/exec settings and toolsets
- Any suggested fix, if you have one

## What to expect

- An acknowledgement within **3 working days**.
- An initial assessment within **7 days**, including whether the report is accepted and a rough timeline.
- A fix released as soon as practical, with a GitHub security advisory and credit to you, unless you'd rather stay anonymous.

Please give us a reasonable amount of time to fix the issue before you disclose it publicly.

## Scope

In scope:

- Bypassing `MCP_AUTH_TOKEN`, `MCP_ALLOWED_HOSTS`, or the write, delete, exec or toolset gating. Examples would be performing a write or delete when it's disabled, or reaching a guest-agent exec or file-write endpoint through the raw API tool without `PROXMOX_ALLOW_EXEC`.
- Leaking the Proxmox token secret, password or ticket, or secret fields that should be redacted
- Vulnerabilities in the Docker image or the release pipeline

Out of scope:

- Issues that need an attacker who already holds a valid `MCP_AUTH_TOKEN` and uses only the tools that configuration allows. That's intended behaviour.
- Prompt injection that leads the model to call tools the configuration allows, for example through a VM description or guest agent output. This is a known limitation of AI assistants. [docs/security.md](docs/security.md) explains how to limit the impact.
- Anything the Proxmox token's own permissions allow. Scoping the token is the operator's job.
- Vulnerabilities in Proxmox VE itself. Report those to Proxmox Server Solutions through the security contact listed on [proxmox.com](https://www.proxmox.com).

For guidance on deploying proxmox-mcp securely, see [docs/security.md](docs/security.md).
