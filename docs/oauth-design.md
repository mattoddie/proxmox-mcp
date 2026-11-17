# Native OAuth design

The existing stateless `/mcp` transport wraps the Go MCP SDK; config gates static
Bearer auth and tools independently enforce write/delete/exec permissions. CI runs
race tests, lint, vulnerability checks, bundles and Docker smoke tests. Release
publishing and the existing compose file remain unchanged.

Caela's OAuth discovery, strict authorization request validation and token service
were inspected as architectural references. This implementation shares the
protocol boundaries, not PHP code.

## Security decisions (before implementation)

* OAuth is opt-in. A canonical HTTPS origin is the issuer, and issuer + `/mcp`
  is the sole resource. Never infer either from forwarded headers.
* Public clients use dynamic registration (`none` authentication), exact HTTPS
  redirect allowlisting, authorization code and mandatory S256 PKCE. No implicit,
  password or client credentials grants or wildcard redirects. Review adds restricted
  ChatGPT CIMD discovery with bounded HTTPS fetching; arbitrary metadata hosts
  remain unsupported.
* One operator configured bcrypt credential authenticates every consent. No signup
  or ambient login session. Consent is an explicit POST bound to a random,
  short-lived HttpOnly Secure SameSite cookie and server-side request.
* Opaque random codes and tokens; only SHA-256 digests persist. Code lifetime 2m,
  access 15m, grant/refresh absolute lifetime 30d. Refresh rotation and replay revoke
  the entire grant, including access. Revocation deletes a grant.
* Private 0600 atomic state snapshots under a 0700 directory; process lock prevents
  multiple instances. Serialized mutations prevent concurrent code/refresh replay.
  Fail closed on persistence errors. Backups contain security-sensitive metadata.
* OAuth scope `mcp` delegates the existing enabled tools; it never changes server
  permissions. Existing read-only defaults and static Bearer credentials remain.
* Bounded request/state sizes and global rate limits protect public endpoints.
  No credentials, authorization queries, codes or tokens are logged.

Implementation stays in internal/oauth plus narrow config/transport wiring to ease
upstream merges. Automated tests cover protocol and authentication boundaries.
Real ChatGPT validation requires a separately approved HTTPS test deployment.


## Pre-merge hardening

CIMD, issuer-bearing error redirects, strict registration parsing, independent
registration/password limits and code replay revocation were added after review.
Snapshots now have a durable interrupted-write marker and are bound to the issuer,
administrator hash and redirect policy. Changed policy revokes codes/grants.
Blocking bodies, metadata requests and bcrypt run outside the state mutex.
See oauth.md for recovery and the remaining single-operator limitations.
