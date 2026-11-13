# Native OAuth for ChatGPT

OAuth is optional and HTTP-only. Existing MCP_AUTH_TOKEN clients continue to work
alongside it. Never combine OAuth with MCP_ALLOW_UNAUTHENTICATED.

## Configuration

| Variable | Value |
| --- | --- |
| MCP_TRANSPORT | http |
| MCP_OAUTH_ISSUER | Canonical public HTTPS origin, e.g. https://mcp.example.com (no path or trailing slash) |
| MCP_OAUTH_ADMIN_HASH | bcrypt hash of a unique strong administrator password |
| MCP_OAUTH_STATE_DIR | Absolute persistent directory path, mode 0700 |
| MCP_OAUTH_REDIRECT_URIS | Comma-separated exact allowed HTTPS callback URLs |
| MCP_ALLOWED_HOSTS | Include the issuer hostname and existing allowed hosts |

These variables support the existing _FILE convention. Prefer
MCP_OAUTH_ADMIN_HASH_FILE with a mounted secret. Generate a hash using a trusted
local password manager or htpasswd -nBC 12 admin; retain only the portion after
admin:. Use cost 12 through 14 (the server accepts 10 through 14). Passwords must be at most 72 bytes. Never put the plaintext password in environment
variables, command arguments or compose files.

Keep existing Proxmox configuration. Write, delete and exec permissions retain
their existing defaults. OAuth scope mcp delegates only currently enabled tools;
it cannot override server controls. All authorized clients use the same configured
Proxmox service identity. This is a single-operator server.

## Separate Docker test deployment

Build the PR image using docker build -t proxmox-mcp:oauth-test .
Test using a separate container, port and environment file, without replacing the
production container. Mount a private directory owned by UID 65532 with mode 0700
at /oauth-state, set MCP_OAUTH_STATE_DIR=/oauth-state, and mount the administrator
hash read-only at /run/secrets/oauth-admin with
MCP_OAUTH_ADMIN_HASH_FILE=/run/secrets/oauth-admin.

Retain --read-only, --cap-drop=ALL and --security-opt=no-new-privileges. Publish a
separate local port such as 127.0.0.1:18080:8080. Supply the existing Proxmox
read-only API credential in the separate env file. Use a separately approved HTTPS
test endpoint forwarding the canonical Host unchanged. OAuth endpoints require
that exact Host (including port if configured); native OAuth MCP calls do too,
and any supplied Origin must exactly equal the HTTPS issuer. Static Bearer calls
retain the existing MCP_ALLOWED_HOSTS behavior; forwarded headers never determine
the issuer. The existing compose file, release publishing and Cloudflare
configuration are unchanged.

## ChatGPT connection

1. Find the exact callback URL in the current ChatGPT custom MCP OAuth flow.
   Add it to MCP_OAUTH_REDIRECT_URIS. A common value is
   https://chatgpt.com/connector_platform_oauth_redirect; verify it in the client.
   Wildcards and unlisted redirects are refused.
2. Add https://mcp.example.com/mcp as a custom MCP server. Choose OAuth and leave
   client credentials blank. CIMD is preferred; DCR remains available.
3. ChatGPT discovers the server, identifies itself using CIMD or registers a public client, requests scope mcp
   (or omits scope), and uses Authorization Code with PKCE S256.
4. Check the displayed client and redirect. Enter the administrator password and
   explicitly Authorize, or Deny. There is no public signup or automatic consent.
5. Token exchange must use the same redirect and the verifier. Both authorization
   and token/refresh requests require resource=https://mcp.example.com/mcp.
   MCP calls carry the access token in the Authorization Bearer header.

Discovery endpoints are /.well-known/oauth-protected-resource/mcp (also a root
alias) and /.well-known/oauth-authorization-server. Unauthorized MCP calls return
401 with scope and resource_metadata Bearer challenges. RFC 7591 DCR requires
an explicit token_endpoint_auth_method=none. Duplicate JSON keys, trailing
objects, invalid content types and unsupported authentication methods are rejected.

CIMD supports only OpenAI's documented HTTPS client document paths:
https://chatgpt.com/oauth/client.json and
https://chatgpt.com/oauth/{callback_id}/client.json. No arbitrary host, query, port
or redirect is fetched. Fetches have a five-second timeout and 16 KiB limit;
client_id must exactly match the requested document URL. The requested callback
must occur both in the document and the operator's redirect allowlist. The plural
token_endpoint_auth_methods_supported field takes precedence over the legacy
singular preference; it must include none. This handles ChatGPT documents that
prefer private_key_jwt but also support none. Confidential authentication,
private_key_jwt, other CIMD providers, OIDC identity claims and workspace email
domain restrictions are intentionally unsupported.

Protocol references checked 2026-11-05:
[MCP authorization](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization),
[MCP client registration](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization/client-registration),
and [OpenAI authentication](https://developers.openai.com/plugins/build/auth).
Copy the exact callback and CIMD URL from the current management UI; the stable
callback above differs from the outdated example in the first version of this PR.

A real ChatGPT connection requires a separately approved HTTPS integration test.
Automated tests do not prove compatibility with every current ChatGPT variant.

## Security and lifecycle

Codes expire after two minutes, access tokens after fifteen minutes, and grants
after thirty days absolutely. Refresh rotates both tokens; reuse of an old
refresh token revokes the entire grant. Authorization-code replay also revokes
the credentials issued by that code. After 1000 rotations, new consent is
required. Tokens are random 256-bit opaque values; only SHA-256 digests persist.
PKCE uses golang.org/x/oauth2 and password verification uses x/crypto/bcrypt.
No custom cryptographic algorithms or JWT signing keys are used.

Revoke using a form POST to /oauth/revoke with client_id and token (access or
refresh). Unknown tokens return 200 for a known public client. Keep these credentials out of logs and shell
history. To revoke everything, stop the process, remove state.json and any
pending-write marker, then restart. Never remove the marker while retaining an
older snapshot after a failed write. A changed administrator hash, issuer or
redirect allowlist automatically revokes existing grants/codes on startup while
retaining DCR client IDs. Upgrading from the original PR snapshot similarly
invalidates existing grants once. Existing static tokens are independent. Runtime OAuth storage failures leave static
Bearer calls available; invalid OAuth startup state prevents the whole HTTP
server from starting until recovered. Static tokens are revoked separately by
replacing MCP_AUTH_TOKEN.

Use one process per state directory on a local durable filesystem. A maintained
cross-platform file lock rejects a second process; serialized atomic 0600
snapshots persist code consumption and rotation. Do not share state across
replicas or restore stale backups: this can resurrect revoked credentials.
Backups contain sensitive client/grant metadata; protect and encrypt them at the
storage layer. Persistence errors disable OAuth until operator recovery. A durable pending-write
marker makes restart refuse interrupted snapshots. Reset both state.json and the
marker after stopping the process if revocation durability is uncertain. Startup
also verifies writability before listening. State files must be regular, private
files (0600), with an 80 MiB read limit. Linux/local-filesystem OAuth persistence
is the tested deployment target; successful Windows compilation does not verify
Windows filesystem durability semantics.

Registration is capped at 100 clients (operator reset required if exhausted),
pending consent/codes at 100 each, grants at 1000. DCR is separately limited to
one request per ten seconds (burst five); other OAuth endpoints use ten/second
(burst twenty). Administrator verification additionally uses one attempt per
three seconds (burst five). Limits are process-local, global across source IPs
and reset on restart. Retry-After accompanies throttling. Registration flooding
cannot consume the token/revocation limiter. Bodies, CIMD network reads and
bcrypt run outside the state mutex so slow callers do not block MCP token checks.

Public DCR capacity can still be exhausted by a sustained attacker; reset state
or restrict registration at an ingress you administer if necessary. CIMD avoids
per-connection registrations but is not authenticated client identity: only the
administrator password and explicit consent authorize a grant. Client names are
untrusted; inspect the displayed client ID and callback. There is no MFA, account
recovery, federation or per-user RBAC. These limits are deliberate for a
single-operator private service and are not a general public identity platform.

Consent requires a one-time request, a Secure HttpOnly SameSite Strict cookie,
same-origin POST and the correct administrator password. Keep TLS and the state
volume protected. The HTTP listener expects TLS termination in front of it.
Do not log authorization query strings, headers, bodies or token responses at
your ingress. The application never logs OAuth credentials.


## Authorization rejection diagnostics

Rejected `POST /oauth/authorize` requests emit one structured JSON event to the
server log (stderr in the container). For example:

```json
{"event":"oauth_authorization_rejected","reason":"password_verification_failed","status":403}
```

Only the fixed event name, a server-selected reason code and the HTTP status are
logged. Passwords, hashes, cookies, request IDs, tokens, authorization codes,
request bodies, headers and query parameters are never included. Public OAuth
errors and consent failure descriptions remain generic. Successful approval and
explicit denial do not emit rejection events.

| Reason | Meaning |
| --- | --- |
| `invalid_host` | Host does not exactly match the configured issuer host. |
| `invalid_origin` | Origin is missing, duplicated, opaque (`null`) or does not exactly match the issuer. |
| `malformed_request` | Invalid content type, unreadable/oversized body, malformed/duplicate form values or a query string on the POST. |
| `consent_request_missing` | Form has no nonempty consent request ID. |
| `consent_request_unknown` | No pending request matches; includes replay, restart or expiry already removed by cleanup. |
| `consent_request_expired` | The submitted pending request has reached its five-minute expiry. |
| `consent_cookie_missing` | No usable consent cookie was supplied. |
| `consent_cookie_mismatch` | Cookie does not match the pending request; opening another consent page in the same browser can replace it. |
| `invalid_decision` | Decision is neither `allow` nor `deny`. |
| `password_verification_failed` | bcrypt did not verify the submitted password. |
| `password_too_long` | Submitted password exceeds bcrypt's 72-byte limit (bytes, not characters). |
| `rate_limited` | General OAuth request limiter rejected the request. |
| `password_rate_limited` | Administrator password attempt limiter rejected the request. |
| `state_persistence_failed` | Saving authorization state failed, or an earlier persistence failure disabled OAuth. |
| `authorization_capacity_exceeded` | The pending authorization code limit was reached. |

Checks run in order and report the first rejection only. A 403 with a verified
password does not establish a bcrypt failure: inspect the server-side reason.
Keep ingress logging restricted as described above; do not enable body or cookie
logging to troubleshoot consent. An unavailable request must be restarted from
the client, because pending consent lives in one process and is consumed once.

`MCP_OAUTH_ADMIN_HASH_FILE` takes precedence over `MCP_OAUTH_ADMIN_HASH`. The
configuration loader already removes surrounding whitespace, including multiple
LF or CRLF line breaks, from the hash file. Additional trailing newlines do not
explain a password mismatch. The mounted file must contain only the bcrypt hash,
without an htpasswd username prefix. `$2a$`, `$2b$` and htpasswd-style `$2y$` hashes
are covered by regression tests. Submitted passwords are never trimmed; leading
and trailing spaces remain significant. Invalid decisions and passwords over
72 bytes are rejected before running bcrypt, while approval attempts retain the
password rate limit and one-time consent consumption.

## Automated Docker verification

Run python3 scripts/oauth-docker-smoke.py proxmox-mcp:ci after building the image.
It creates and removes only uniquely named test containers and volumes. A pinned
BusyBox helper prepares state ownership and a public test-only mounted hash;
the application still runs in the distroless image as UID 65532, read-only, with
all capabilities dropped. The test covers a mounted hash with extra newlines, safe password rejection diagnostics, fresh administrator consent, DCR, resource
binding, OAuth and static Bearer MCP calls, read-only tool listings, restart,
refresh and durable revocation. It never calls a Proxmox API.

The existing PR workflow already runs Go race tests, lint, generated docs,
vulnerability checks and full multi-stage/multi-architecture Docker builds.
The OAuth runtime smoke now runs after its full linux/amd64 image build.
