/**
 * Helpers that turn raw Proxmox API objects into compact, model-friendly
 * summaries: human-readable sizes and times, parsed property strings and
 * redacted secrets.
 */

type Obj = Record<string, any>;

/** Drops undefined, null, "" and empty arrays/objects so summaries stay small. */
export function compact<T extends Obj>(obj: T): Partial<T> {
  const out: Obj = {};
  for (const [k, v] of Object.entries(obj)) {
    if (v === undefined || v === null || v === "") continue;
    if (Array.isArray(v) && v.length === 0) continue;
    if (typeof v === "object" && !Array.isArray(v) && Object.keys(v).length === 0) continue;
    out[k] = v;
  }
  return out as Partial<T>;
}

/** 1536 → "1.5 KiB". */
export function formatBytes(bytes: unknown): string | undefined {
  const n = Number(bytes);
  if (bytes === undefined || bytes === null || bytes === "" || !Number.isFinite(n)) return undefined;
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  let v = Math.abs(n);
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  const s = i === 0 ? String(v) : v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2);
  return `${n < 0 ? "-" : ""}${s.replace(/\.0+$/, "")} ${units[i]}`;
}

/** 90061 → "1d 1h 1m". */
export function formatUptime(seconds: unknown): string | undefined {
  const n = Math.floor(Number(seconds));
  if (!Number.isFinite(n) || n <= 0) return undefined;
  const d = Math.floor(n / 86400);
  const h = Math.floor((n % 86400) / 3600);
  const m = Math.floor((n % 3600) / 60);
  if (d) return `${d}d ${h}h ${m}m`;
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m`;
  return `${n}s`;
}

/** used/total as a percentage with one decimal, e.g. 42.3. */
export function percent(used: unknown, total: unknown): number | undefined {
  const u = Number(used);
  const t = Number(total);
  if (!Number.isFinite(u) || !Number.isFinite(t) || t <= 0) return undefined;
  return Math.round((u / t) * 1000) / 10;
}

/** A 0..1 fraction (as Proxmox reports CPU) as a percentage with one decimal. */
export function fractionPct(value: unknown): number | undefined {
  const n = Number(value);
  if (value === undefined || value === null || !Number.isFinite(n)) return undefined;
  return Math.round(n * 1000) / 10;
}

/** Epoch seconds → ISO 8601. */
export function isoTime(epochSeconds: unknown): string | undefined {
  const n = Number(epochSeconds);
  if (!Number.isFinite(n) || n <= 0) return undefined;
  return new Date(n * 1000).toISOString();
}

/** "used of total (pct%)" for memory/disk, or undefined when unknown. */
export function usage(used: unknown, total: unknown): string | undefined {
  const t = Number(total);
  if (!Number.isFinite(t) || t <= 0) return undefined;
  return `${formatBytes(used) ?? "?"} of ${formatBytes(total)} (${percent(used, total) ?? 0}%)`;
}

/**
 * Parses a Proxmox property string such as
 * "local-lvm:vm-100-disk-0,size=32G,ssd=1" into
 * { _default: "local-lvm:vm-100-disk-0", size: "32G", ssd: "1" }.
 * `defaultKey` names the first, key-less value (e.g. "file" for disks).
 */
export function parsePropertyString(value: unknown, defaultKey = "_default"): Record<string, string> {
  const out: Record<string, string> = {};
  if (typeof value !== "string") return out;
  for (const part of value.split(",")) {
    if (!part) continue;
    const idx = part.indexOf("=");
    if (idx < 0) out[defaultKey] = part;
    else out[part.slice(0, idx)] = part.slice(idx + 1);
  }
  return out;
}

/** The inverse of parsePropertyString; keys with undefined values are skipped. */
export function toPropertyString(obj: Record<string, unknown>, defaultKey = "_default"): string {
  const parts: string[] = [];
  if (obj[defaultKey] !== undefined) parts.push(String(obj[defaultKey]));
  for (const [k, v] of Object.entries(obj)) {
    if (k === defaultKey || v === undefined || v === null) continue;
    parts.push(`${k}=${typeof v === "boolean" ? (v ? 1 : 0) : v}`);
  }
  return parts.join(",");
}

/** Size strings like "32G", "512M", "1T" → bytes. */
export function parseSize(size: unknown): number | undefined {
  if (typeof size === "number") return size;
  if (typeof size !== "string") return undefined;
  const m = size.trim().match(/^(\d+(?:\.\d+)?)\s*([KMGTP]?)i?B?$/i);
  if (!m) return undefined;
  const pow = { "": 0, K: 1, M: 2, G: 3, T: 4, P: 5 }[m[2].toUpperCase() as "" | "K" | "M" | "G" | "T" | "P"];
  return Math.round(Number(m[1]) * 1024 ** pow);
}

/** Config keys that hold disks/volumes on VMs (scsi0, virtio1, efidisk0...) and containers (rootfs, mp0). */
export const DISK_KEY_RE = /^(ide|sata|scsi|virtio|efidisk|tpmstate|unused)\d+$|^(rootfs|mp\d+)$/;
/** Network interface keys on VMs and containers. */
export const NET_KEY_RE = /^net\d+$/;

export function isDiskKey(key: string): boolean {
  return DISK_KEY_RE.test(key);
}

/** Disk-ish config entries as parsed objects, e.g. { scsi0: { file: "local-lvm:vm-100-disk-0", size: "32G" } }. */
export function summarizeDisks(config: Obj): Record<string, Record<string, string>> {
  const out: Record<string, Record<string, string>> = {};
  for (const [k, v] of Object.entries(config)) {
    if (!isDiskKey(k) || typeof v !== "string") continue;
    out[k] = parsePropertyString(v, k === "rootfs" || k.startsWith("mp") ? "volume" : "file");
  }
  return out;
}

/** Network config entries as parsed objects (model/MAC, bridge, VLAN tag, firewall, IPs for containers). */
export function summarizeNets(config: Obj): Record<string, Record<string, string>> {
  const out: Record<string, Record<string, string>> = {};
  for (const [k, v] of Object.entries(config)) {
    if (!NET_KEY_RE.test(k) || typeof v !== "string") continue;
    const parsed = parsePropertyString(v);
    // VM NICs start with "virtio=AA:BB:..." (model=mac); split it out.
    for (const model of ["virtio", "e1000", "e1000e", "rtl8139", "vmxnet3"]) {
      if (parsed[model]) {
        parsed.model = model;
        parsed.macaddr = parsed[model];
        delete parsed[model];
      }
    }
    out[k] = parsed;
  }
  return out;
}

const SECRET_KEY_RE = /pass(word|wd|phrase)?$|secret|private|^token$|apikey|api[-_]?key|^subscription-?key$|^client-key$|^encryption-key$|^keyring$|cipassword|smtp-password|hmac|^ticket$|^otel-headers$/i;
/** user:password@ inside URLs (e.g. an http_proxy setting). */
const URL_CREDENTIALS_RE = /(\b[a-z][a-z0-9+.-]*:\/\/[^\s\/:@]+:)[^\s\/@]+@/gi;
const REDACTED = "<redacted>";

/**
 * Replaces the values of password/secret/key-like fields with "<redacted>",
 * recursively. Used for raw objects returned to the model.
 */
export function redactSecrets<T>(value: T): T {
  if (Array.isArray(value)) return value.map((v) => redactSecrets(v)) as T;
  if (value && typeof value === "object") {
    const out: Obj = {};
    for (const [k, v] of Object.entries(value as Obj)) {
      if (SECRET_KEY_RE.test(k) && v !== undefined && v !== null && v !== "" && typeof v !== "object" && typeof v !== "boolean") {
        out[k] = REDACTED;
      } else if (SECRET_KEY_RE.test(k) && Array.isArray(v)) {
        out[k] = v.map((x) => (x !== null && typeof x === "object" ? redactSecrets(x) : REDACTED));
      } else {
        out[k] = redactSecrets(v);
      }
    }
    return out as T;
  }
  if (typeof value === "string" && value.includes("@")) return value.replace(URL_CREDENTIALS_RE, `$1${REDACTED}@`) as T;
  return value;
}

/** Proxmox booleans arrive as 0/1 numbers or strings; this normalises them. */
export function flag(value: unknown): boolean | undefined {
  if (value === undefined || value === null || value === "") return undefined;
  return value === true || value === 1 || value === "1" || value === "on" || value === "yes";
}
