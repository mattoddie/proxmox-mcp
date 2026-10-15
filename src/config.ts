import { readFileSync } from "node:fs";

export type TransportType = "stdio" | "http";

export interface ProxmoxConfig {
  url: string;
  /** Full API token id, e.g. root@pam!mcp. */
  tokenId?: string;
  tokenSecret?: string;
  /** user@realm for ticket (password) authentication. */
  username?: string;
  password?: string;
  verifySsl: boolean;
  /** PEM bundle trusted in addition to the system CAs. */
  caCert?: string;
  /**
   * Pinned SHA-256 fingerprints of the server certificate, as "AA:BB:..." (PROXMOX_TLS_FINGERPRINT).
   * When set, the certificate must match one of them, whether or not chain verification is on.
   */
  tlsFingerprints?: string[];
  timeoutMs: number;
  /** How long tools wait for a Proxmox task (UPID) to finish before returning. */
  taskTimeoutMs: number;
  /** Node used when a tool needs one and none is given; otherwise the only node or an error. */
  defaultNode?: string;
}

export const TOOLSETS = [
  "cluster",
  "nodes",
  "vms",
  "containers",
  "agent",
  "snapshots",
  "storage",
  "backup",
  "tasks",
  "access",
  "firewall",
  "ha",
  "sdn",
  "ceph",
  "pools",
  "replication",
  "notifications",
  "hardware",
  "raw",
] as const;
export type Toolset = (typeof TOOLSETS)[number];

export interface ToolOptions {
  allowWrites: boolean;
  /** Tools that delete guests or configuration; requires allowWrites as well. */
  allowDeletes: boolean;
  /** Tools that run commands or write files inside guests and on nodes; requires allowWrites as well. */
  allowExec: boolean;
  toolsets: ReadonlySet<Toolset>;
}

export interface Config {
  proxmox: ProxmoxConfig;
  tools: ToolOptions;
  transport: TransportType;
  http: {
    host: string;
    port: number;
    authToken?: string;
    /** Serve /mcp without a bearer token (MCP_ALLOW_UNAUTHENTICATED); only behind something else that authenticates. */
    allowUnauthenticated: boolean;
    /** Lower-cased Host (and Origin) allow-list. */
    allowedHosts?: string[];
  };
}

export const AUTH_TOKEN_REQUIRED =
  "MCP_AUTH_TOKEN is required for the http transport (generate one with: openssl rand -hex 32); set MCP_ALLOW_UNAUTHENTICATED=true only behind something else that authenticates";

type Env = Record<string, string | undefined>;

/**
 * Reads a variable, also honouring the `<NAME>_FILE` convention so secrets can
 * be supplied as Docker secrets mounted under /run/secrets.
 */
function read(env: Env, name: string): string | undefined {
  const file = env[`${name}_FILE`];
  if (file) {
    try {
      return readFileSync(file, "utf8").trim() || undefined;
    } catch (err) {
      throw new Error(`Could not read ${name}_FILE (${file}): ${(err as Error).message}`);
    }
  }
  const value = env[name]?.trim();
  return value ? value : undefined;
}

/**
 * Parses PROXMOX_TLS_FINGERPRINT: one or more comma-separated SHA-256 fingerprints, as Proxmox shows them
 * ("AA:BB:...", optionally prefixed "sha256:" or "SHA256 Fingerprint=") or as 64 hex digits, into "AA:BB:..." form.
 */
export function parseFingerprints(value: string): string[] {
  return value
    .split(",")
    .map((v) => v.trim())
    .filter(Boolean)
    .map((v) => {
      const hex = v.replace(/^(sha-?256\s*(fingerprint)?\s*[=:]?\s*)/i, "").replace(/[:\s]/g, "");
      if (!/^[0-9a-f]{64}$/i.test(hex)) {
        throw new Error(`PROXMOX_TLS_FINGERPRINT must be a SHA-256 fingerprint (AA:BB:... with 32 pairs, or 64 hex digits), got "${v}"`);
      }
      return hex.toUpperCase().match(/../g)!.join(":");
    });
}

function bool(value: string | undefined, fallback: boolean): boolean {
  if (value === undefined) return fallback;
  return ["1", "true", "yes", "on"].includes(value.toLowerCase());
}

function int(name: string, value: string | undefined, fallback: number): number {
  if (value === undefined) return fallback;
  const n = Number.parseInt(value, 10);
  if (!Number.isFinite(n) || n <= 0) throw new Error(`${name} must be a positive integer, got "${value}"`);
  return n;
}

/**
 * Accepts the token as PROXMOX_TOKEN_ID + PROXMOX_TOKEN_SECRET, or as one
 * PROXMOX_API_TOKEN in the form Proxmox shows it: user@realm!tokenid=secret.
 */
function parseToken(env: Env): { tokenId?: string; tokenSecret?: string } {
  const combined = read(env, "PROXMOX_API_TOKEN")?.replace(/^PVEAPIToken=/, "");
  let tokenId = read(env, "PROXMOX_TOKEN_ID");
  let tokenSecret = read(env, "PROXMOX_TOKEN_SECRET");
  if (combined) {
    const idx = combined.indexOf("=");
    if (idx <= 0) throw new Error("PROXMOX_API_TOKEN must look like user@realm!tokenid=secret");
    tokenId = combined.slice(0, idx);
    tokenSecret = combined.slice(idx + 1);
  }
  if (tokenId && !/^[^@\s]+@[^!\s]+![^\s=]+$/.test(tokenId)) {
    throw new Error(`The API token id must look like user@realm!tokenid (e.g. root@pam!mcp), got "${tokenId}"`);
  }
  if (Boolean(tokenId) !== Boolean(tokenSecret)) {
    throw new Error("Set both PROXMOX_TOKEN_ID and PROXMOX_TOKEN_SECRET (or PROXMOX_API_TOKEN=user@realm!tokenid=secret)");
  }
  return { tokenId, tokenSecret };
}

export function loadConfig(env: Env = process.env): Config {
  const url = read(env, "PROXMOX_URL");
  if (!url) throw new Error("PROXMOX_URL is required (e.g. https://pve.example.lan:8006)");
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    throw new Error(`PROXMOX_URL is not a valid URL: ${url}`);
  }
  if (!["http:", "https:"].includes(parsed.protocol)) throw new Error(`PROXMOX_URL must use https:// (or http://), got ${url}`);

  const { tokenId, tokenSecret } = parseToken(env);
  let username = read(env, "PROXMOX_USERNAME");
  const password = read(env, "PROXMOX_PASSWORD");
  if (username && !username.includes("@")) username = `${username}@${read(env, "PROXMOX_REALM") ?? "pam"}`;
  if (!tokenId && !(username && password)) {
    throw new Error("Set either an API token (PROXMOX_TOKEN_ID and PROXMOX_TOKEN_SECRET) or both PROXMOX_USERNAME and PROXMOX_PASSWORD");
  }

  let caCert: string | undefined;
  const caFile = read(env, "PROXMOX_CA_FILE");
  if (caFile) {
    try {
      caCert = readFileSync(caFile, "utf8");
    } catch (err) {
      throw new Error(`Could not read PROXMOX_CA_FILE (${caFile}): ${(err as Error).message}`);
    }
  }

  const fingerprintValue = read(env, "PROXMOX_TLS_FINGERPRINT");
  const tlsFingerprints = fingerprintValue ? parseFingerprints(fingerprintValue) : undefined;
  if (tlsFingerprints && parsed.protocol !== "https:") throw new Error("PROXMOX_TLS_FINGERPRINT needs an https:// PROXMOX_URL");

  const transport = (read(env, "MCP_TRANSPORT") ?? "stdio") as TransportType;
  if (!["stdio", "http"].includes(transport)) {
    throw new Error(`MCP_TRANSPORT must be stdio or http, got "${transport}"`);
  }

  const allowWrites = bool(read(env, "PROXMOX_ALLOW_WRITES"), false);
  const allowDeletes = bool(read(env, "PROXMOX_ALLOW_DELETES"), false);
  const allowExec = bool(read(env, "PROXMOX_ALLOW_EXEC"), false);
  if (allowDeletes && !allowWrites) {
    throw new Error("PROXMOX_ALLOW_DELETES=true also requires PROXMOX_ALLOW_WRITES=true");
  }
  if (allowExec && !allowWrites) {
    throw new Error("PROXMOX_ALLOW_EXEC=true also requires PROXMOX_ALLOW_WRITES=true");
  }

  const toolsetList = (read(env, "PROXMOX_TOOLSETS") ?? "all")
    .split(",")
    .map((t) => t.trim().toLowerCase())
    .filter(Boolean);
  const toolsets = new Set<Toolset>();
  for (const t of toolsetList) {
    if (t === "all") TOOLSETS.forEach((x) => toolsets.add(x));
    else if ((TOOLSETS as readonly string[]).includes(t)) toolsets.add(t as Toolset);
    else throw new Error(`Unknown toolset "${t}" in PROXMOX_TOOLSETS; valid: all, ${TOOLSETS.join(", ")}`);
  }

  const allowedHosts = read(env, "MCP_ALLOWED_HOSTS")
    ?.split(",")
    .map((h) => h.trim().toLowerCase())
    .filter(Boolean);
  const authToken = read(env, "MCP_AUTH_TOKEN");
  const allowUnauthenticated = bool(read(env, "MCP_ALLOW_UNAUTHENTICATED"), false);
  if (transport === "http" && !authToken && !allowUnauthenticated) throw new Error(AUTH_TOKEN_REQUIRED);

  return {
    proxmox: {
      url: url.replace(/\/+$/, "").replace(/\/api2\/json$/, ""),
      tokenId,
      tokenSecret,
      username: tokenId ? undefined : username,
      password: tokenId ? undefined : password,
      verifySsl: caCert !== undefined || bool(read(env, "PROXMOX_VERIFY_SSL"), false),
      caCert,
      tlsFingerprints: tlsFingerprints?.length ? tlsFingerprints : undefined,
      timeoutMs: int("PROXMOX_TIMEOUT_MS", read(env, "PROXMOX_TIMEOUT_MS"), 30000),
      taskTimeoutMs: int("PROXMOX_TASK_TIMEOUT_MS", read(env, "PROXMOX_TASK_TIMEOUT_MS"), 120000),
      defaultNode: read(env, "PROXMOX_DEFAULT_NODE"),
    },
    tools: { allowWrites, allowDeletes, allowExec, toolsets },
    transport,
    http: {
      // Loopback unless told otherwise; the Docker image sets MCP_HTTP_HOST=0.0.0.0.
      host: read(env, "MCP_HTTP_HOST") ?? "127.0.0.1",
      port: int("MCP_HTTP_PORT", read(env, "MCP_HTTP_PORT"), 8080),
      authToken,
      allowUnauthenticated,
      allowedHosts: allowedHosts?.length ? allowedHosts : undefined,
    },
  };
}
