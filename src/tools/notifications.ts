import { z } from "zod";
import { compact, flag, parsePropertyString, redactSecrets } from "../proxmox/format.js";
import { defined, defineTool, digestArg, enc, extraArg, rawArg, type ToolContext } from "./util.js";

type Obj = Record<string, any>;

const TOOLSET = "notifications" as const;
const BASE = "/cluster/notifications";
const ENDPOINT_TYPES = ["sendmail", "smtp", "gotify", "webhook"] as const;
type EndpointType = (typeof ENDPOINT_TYPES)[number];
const REDACTED = "<redacted>";

const nameArg = z.string().describe("Endpoint/target name (see proxmox_list_notification_targets)");
const matcherNameArg = z.string().describe("Matcher name (see proxmox_list_notification_matchers)");
const endpointTypeArg = z.enum(ENDPOINT_TYPES).describe("Endpoint type: sendmail (local mail), smtp, gotify or webhook");
const deleteArrayArg = z
  .array(z.string())
  .optional()
  .describe("Settings to remove/reset, e.g. [\"comment\", \"mailto-user\"] (Proxmox names)");

// Header names whose values are credentials, so webhook outputs hide them.
const SENSITIVE_HEADER_RE = /auth|token|key|secret|cookie|pass|signature/i;

function b64decode(value: string | undefined): string | undefined {
  if (value === undefined) return undefined;
  try {
    return Buffer.from(value, "base64").toString("utf8");
  } catch {
    return value;
  }
}

const b64 = (value: string) => Buffer.from(value, "utf8").toString("base64");

/** "name=X,value=<base64>" entries → { X: decoded value } with credentials redacted. */
function decodePairs(list: unknown, redactAll: boolean): Record<string, string> | undefined {
  if (!Array.isArray(list) || !list.length) return undefined;
  const out: Record<string, string> = {};
  for (const item of list) {
    const p = parsePropertyString(item);
    const name = p.name ?? p._default ?? "?";
    if (redactAll || SENSITIVE_HEADER_RE.test(name)) out[name] = p.value !== undefined || redactAll ? REDACTED : "";
    else out[name] = b64decode(p.value) ?? "";
  }
  return out;
}

/** Endpoint config with secrets hidden and webhook header/body/secret fields decoded. */
export function redactEndpoint(e: Obj): Obj {
  const out: Obj = redactSecrets({ ...e });
  for (const k of ["token", "password"]) if (out[k] !== undefined) out[k] = REDACTED;
  if (e.header !== undefined) out.header = decodePairs(e.header, false);
  if (e.secret !== undefined) out.secret = decodePairs(e.secret, true);
  if (typeof e.body === "string" && e.body) out.body = b64decode(e.body);
  return out;
}

function summarizeMatcher(m: Obj): Obj {
  return compact({
    name: m.name,
    disabled: flag(m.disable) || undefined,
    origin: m.origin,
    mode: m.mode ?? "all",
    invert_match: flag(m["invert-match"]) || undefined,
    match_severity: m["match-severity"],
    match_field: m["match-field"],
    match_calendar: m["match-calendar"],
    targets: m.target,
    comment: m.comment,
  });
}

export function registerNotificationsTools(ctx: ToolContext): void {
  const { client } = ctx;

  async function endpointType(name: string, type?: EndpointType): Promise<EndpointType> {
    if (type) return type;
    const targets = await client.get<Obj[]>(`${BASE}/targets`);
    const t = targets.find((x) => x.name === name);
    if (!t) throw new Error(`No notification target named "${name}" (see proxmox_list_notification_targets)`);
    return t.type as EndpointType;
  }

  // ---------------------------------------------------------------- targets / endpoints

  defineTool(ctx, "proxmox_list_notification_targets", {
    toolset: TOOLSET,
    title: "List notification targets",
    description:
      "List all notification targets (endpoints of every type: sendmail, smtp, gotify, webhook) with type, disabled state, origin and comment. details=true adds each endpoint's configuration (secrets redacted). Matchers decide which notifications go to which target; see proxmox_list_notification_matchers.",
    input: { details: z.boolean().optional().describe("Include each endpoint's full configuration (one extra call per type)") },
    handler: async (a) => {
      const targets = await client.get<Obj[]>(`${BASE}/targets`);
      const config = new Map<string, Obj>();
      if (a.details) {
        const types = [...new Set(targets.map((t) => t.type as EndpointType))].filter((t) => ENDPOINT_TYPES.includes(t));
        const lists = await Promise.all(types.map((t) => client.get<Obj[]>(`${BASE}/endpoints/${t}`).catch(() => [] as Obj[])));
        types.forEach((t, i) => lists[i].forEach((e) => config.set(`${t}/${e.name}`, e)));
      }
      return targets
        .map((t) => {
          const cfg = config.get(`${t.type}/${t.name}`);
          const rest = cfg ? redactEndpoint(cfg) : undefined;
          if (rest) for (const k of ["name", "comment", "disable", "origin"]) delete rest[k];
          return compact({ name: t.name, type: t.type, disabled: flag(t.disable) || undefined, origin: t.origin, comment: t.comment, config: rest });
        })
        .sort((x, y) => String(x.name).localeCompare(String(y.name)));
    },
  });

  defineTool(ctx, "proxmox_get_notification_endpoint", {
    toolset: TOOLSET,
    title: "Get notification endpoint",
    description:
      "Get one notification endpoint's configuration. The type is looked up when omitted. Tokens, passwords and webhook secrets are redacted; webhook headers and body are shown decoded (credential-like headers redacted).",
    input: { name: nameArg, type: endpointTypeArg.optional(), raw: rawArg },
    handler: async (a) => {
      const type = await endpointType(a.name, a.type);
      const e = await client.get<Obj>(`${BASE}/endpoints/${type}/${enc(a.name)}`);
      if (a.raw) {
        // Keep the raw property strings, but blank the base64 values of credential-like headers (e.g. Authorization).
        const header = Array.isArray(e.header)
          ? e.header.map((h: unknown) => {
              const p = parsePropertyString(h);
              return SENSITIVE_HEADER_RE.test(p.name ?? "") && p.value !== undefined ? `name=${p.name},value=${REDACTED}` : h;
            })
          : e.header;
        return redactSecrets({ type, ...e, header, ...(e.secret ? { secret: decodePairs(e.secret, true) } : {}) });
      }
      return compact({ type, ...redactEndpoint(e), disable: undefined, disabled: flag(e.disable) || undefined });
    },
  });

  defineTool(ctx, "proxmox_save_notification_endpoint", {
    toolset: TOOLSET,
    title: "Create or update a notification endpoint",
    description:
      "Create (create=true) or update a notification endpoint of any type. Only the given fields change on update. " +
      "sendmail: mailto and/or mailto_user, from_address, author. " +
      "smtp: server, from_address (required), port, mode, username, password, mailto/mailto_user, author. " +
      "gotify: server URL and token (required). " +
      "webhook: url and method (required), headers, body (plain text; Proxmox templates like {{ title }} / {{ message }} work), secrets (referenced as {{ secrets.<name> }}). " +
      "Header, body and secret values are base64-encoded for you. Afterwards route notifications to it with proxmox_save_notification_matcher and try it with proxmox_test_notification_target.",
    write: true,
    destructive: false,
    input: {
      type: endpointTypeArg,
      name: nameArg,
      create: z.boolean().optional().describe("Create a new endpoint (default false = update)"),
      comment: z.string().optional().describe("Comment"),
      disable: z.boolean().optional().describe("Disable the endpoint"),
      mailto: z.array(z.string()).optional().describe("sendmail/smtp: recipient email addresses"),
      mailto_user: z.array(z.string()).optional().describe("sendmail/smtp: users whose configured email is used, e.g. [\"root@pam\"]"),
      from_address: z.string().optional().describe("sendmail/smtp: From address (required for smtp)"),
      author: z.string().optional().describe("sendmail/smtp: author name (default 'Proxmox VE')"),
      server: z.string().optional().describe("smtp: server host name; gotify: server URL"),
      port: z.number().int().positive().optional().describe("smtp: port (default 465 tls, 587 starttls, 25 insecure)"),
      mode: z.enum(["insecure", "starttls", "tls"]).optional().describe("smtp: encryption (default tls)"),
      username: z.string().optional().describe("smtp: login user name"),
      password: z.string().optional().describe("smtp: login password (never echoed)"),
      token: z.string().optional().describe("gotify: application token (never echoed)"),
      url: z.string().optional().describe("webhook: URL (may use {{ secrets.<name> }})"),
      method: z.enum(["post", "put", "get"]).optional().describe("webhook: HTTP method"),
      headers: z.record(z.string(), z.string()).optional().describe("webhook: HTTP headers as {name: value} (replaces all headers)"),
      body: z.string().optional().describe("webhook: request body template in plain text"),
      secrets: z.record(z.string(), z.string()).optional().describe("webhook: secrets as {name: value} (replaces all secrets; never echoed)"),
      delete: deleteArrayArg,
      digest: digestArg,
      extra: extraArg,
    },
    handler: async (a) => {
      const t = a.type;
      const mail = t === "sendmail" || t === "smtp";
      const misplaced = Object.entries({
        mailto: a.mailto !== undefined && !mail,
        mailto_user: a.mailto_user !== undefined && !mail,
        from_address: a.from_address !== undefined && !mail,
        author: a.author !== undefined && !mail,
        server: a.server !== undefined && t !== "smtp" && t !== "gotify",
        port: a.port !== undefined && t !== "smtp",
        mode: a.mode !== undefined && t !== "smtp",
        username: a.username !== undefined && t !== "smtp",
        password: a.password !== undefined && t !== "smtp",
        token: a.token !== undefined && t !== "gotify",
        url: a.url !== undefined && t !== "webhook",
        method: a.method !== undefined && t !== "webhook",
        headers: a.headers !== undefined && t !== "webhook",
        body: a.body !== undefined && t !== "webhook",
        secrets: a.secrets !== undefined && t !== "webhook",
      })
        .filter(([, bad]) => bad)
        .map(([k]) => k);
      if (misplaced.length) throw new Error(`Not valid for ${t} endpoints: ${misplaced.join(", ")}`);
      if (a.create) {
        const required: Record<EndpointType, [string, unknown][]> = {
          sendmail: [],
          smtp: [["server", a.server], ["from_address", a.from_address]],
          gotify: [["server", a.server], ["token", a.token]],
          webhook: [["url", a.url], ["method", a.method]],
        };
        const missing = required[t].filter(([, v]) => v === undefined).map(([k]) => k);
        if (t === "sendmail" && !a.mailto?.length && !a.mailto_user?.length) missing.push("mailto or mailto_user");
        if (missing.length) throw new Error(`Required to create a ${t} endpoint: ${missing.join(", ")}`);
      }
      const params = defined({
        comment: a.comment,
        disable: a.disable,
        mailto: a.mailto,
        "mailto-user": a.mailto_user,
        "from-address": a.from_address,
        author: a.author,
        server: a.server,
        port: a.port,
        mode: a.mode,
        username: a.username,
        password: a.password,
        token: a.token,
        url: a.url,
        method: a.method,
        header: a.headers ? Object.entries(a.headers).map(([k, v]) => `name=${k},value=${b64(v)}`) : undefined,
        body: a.body !== undefined ? b64(a.body) : undefined,
        secret: a.secrets ? Object.entries(a.secrets).map(([k, v]) => `name=${k},value=${b64(v)}`) : undefined,
        ...a.extra,
      });
      if (a.create) await client.post(`${BASE}/endpoints/${t}`, { name: a.name, ...params });
      else await client.put(`${BASE}/endpoints/${t}/${enc(a.name)}`, { ...params, ...defined({ delete: a.delete?.length ? a.delete : undefined, digest: a.digest }) });
      const saved = await client.get<Obj>(`${BASE}/endpoints/${t}/${enc(a.name)}`).catch(() => undefined);
      return compact({ [a.create ? "created" : "updated"]: a.name, type: t, endpoint: saved ? redactEndpoint(saved) : undefined });
    },
  });

  defineTool(ctx, "proxmox_delete_notification_endpoint", {
    toolset: TOOLSET,
    title: "Delete a notification endpoint",
    description: "Delete a notification endpoint (the type is looked up when omitted). Remove it from matchers first, or they will reference a missing target.",
    delete: true,
    input: { name: nameArg, type: endpointTypeArg.optional() },
    handler: async (a) => {
      const type = await endpointType(a.name, a.type);
      await client.delete(`${BASE}/endpoints/${type}/${enc(a.name)}`);
      return { deleted: a.name, type };
    },
  });

  defineTool(ctx, "proxmox_test_notification_target", {
    toolset: TOOLSET,
    title: "Send a test notification",
    description: "Send a test notification to a target to check it is configured correctly (mail delivered, webhook reachable...). Errors from the endpoint are returned.",
    write: true,
    destructive: false,
    input: { name: nameArg },
    handler: async (a) => {
      await client.post(`${BASE}/targets/${enc(a.name)}/test`);
      return { tested: a.name, sent: true };
    },
  });

  // ---------------------------------------------------------------- matchers

  defineTool(ctx, "proxmox_list_notification_matchers", {
    toolset: TOOLSET,
    title: "List notification matchers",
    description:
      "List notification matchers: rules that route notifications (by severity, metadata fields such as type/hostname/job-id, and calendar time) to targets.",
    input: {},
    handler: async () => {
      const matchers = await client.get<Obj[]>(`${BASE}/matchers`);
      return matchers.map(summarizeMatcher).sort((x, y) => String(x.name).localeCompare(String(y.name)));
    },
  });

  defineTool(ctx, "proxmox_get_notification_matcher", {
    toolset: TOOLSET,
    title: "Get notification matcher",
    description: "Get one notification matcher's rules and targets (plus its digest for safe updates).",
    input: { name: matcherNameArg },
    handler: async (a) => {
      const m = await client.get<Obj>(`${BASE}/matchers/${enc(a.name)}`);
      return compact({ ...summarizeMatcher({ name: a.name, ...m }), digest: m.digest });
    },
  });

  defineTool(ctx, "proxmox_save_notification_matcher", {
    toolset: TOOLSET,
    title: "Create or update a notification matcher",
    description:
      "Create (create=true) or update a notification matcher. Given lists replace the existing ones. A matcher with no match rules matches everything. " +
      "match_severity: info, notice, warning, error, unknown. match_field: '(exact|regex):<field>=<value>', e.g. 'exact:type=vzdump' or 'regex:hostname=^pve'; see proxmox_get_notification_matcher_fields for known fields and values. " +
      "match_calendar: time windows like 'mon..fri 8-17'. mode: all (default) or any of the rules must match.",
    write: true,
    destructive: false,
    input: {
      name: matcherNameArg,
      create: z.boolean().optional().describe("Create a new matcher (default false = update)"),
      targets: z.array(z.string()).optional().describe("Target names to notify on match"),
      match_severity: z.array(z.enum(["info", "notice", "warning", "error", "unknown"])).optional().describe("Severities to match"),
      match_field: z.array(z.string()).optional().describe("Metadata rules, e.g. [\"exact:type=vzdump\", \"regex:hostname=^pve1$\"]"),
      match_calendar: z.array(z.string()).optional().describe("Calendar windows, e.g. [\"mon..fri 9-17\"]"),
      mode: z.enum(["all", "any"]).optional().describe("Whether all (default) or any of the rules must match"),
      invert_match: z.boolean().optional().describe("Invert the whole matcher"),
      disable: z.boolean().optional().describe("Disable the matcher"),
      comment: z.string().optional().describe("Comment"),
      delete: deleteArrayArg,
      digest: digestArg,
      extra: extraArg,
    },
    handler: async (a) => {
      const params = defined({
        target: a.targets,
        "match-severity": a.match_severity,
        "match-field": a.match_field,
        "match-calendar": a.match_calendar,
        mode: a.mode,
        "invert-match": a.invert_match,
        disable: a.disable,
        comment: a.comment,
        ...a.extra,
      });
      if (a.create) await client.post(`${BASE}/matchers`, { name: a.name, ...params });
      else await client.put(`${BASE}/matchers/${enc(a.name)}`, { ...params, ...defined({ delete: a.delete?.length ? a.delete : undefined, digest: a.digest }) });
      const m = await client.get<Obj>(`${BASE}/matchers/${enc(a.name)}`).catch(() => undefined);
      return compact({ [a.create ? "created" : "updated"]: a.name, matcher: m ? summarizeMatcher({ name: a.name, ...m }) : undefined });
    },
  });

  defineTool(ctx, "proxmox_delete_notification_matcher", {
    toolset: TOOLSET,
    title: "Delete a notification matcher",
    description: "Delete a notification matcher. Notifications it routed will no longer reach its targets.",
    delete: true,
    input: { name: matcherNameArg },
    handler: async (a) => {
      await client.delete(`${BASE}/matchers/${enc(a.name)}`);
      return { deleted: a.name };
    },
  });

  defineTool(ctx, "proxmox_get_notification_matcher_fields", {
    toolset: TOOLSET,
    title: "Get notification matcher fields",
    description:
      "List the metadata fields notifications carry (type, hostname, job-id...) and their known values, for writing match_field rules in proxmox_save_notification_matcher.",
    input: {},
    handler: async () => {
      const [fields, values] = await Promise.all([
        client.get<Obj[]>(`${BASE}/matcher-fields`),
        client.get<Obj[]>(`${BASE}/matcher-field-values`).catch(() => [] as Obj[]),
      ]);
      const out: Record<string, Obj[]> = {};
      for (const f of fields) out[f.name] = [];
      for (const v of values) (out[v.field] ??= []).push(compact({ value: v.value, comment: v.comment }));
      return {
        fields: Object.entries(out)
          .sort(([x], [y]) => x.localeCompare(y))
          .map(([field, vals]) => compact({ field, values: vals.map((v) => (v.comment ? v : v.value)) })),
        example: "exact:type=vzdump",
      };
    },
  });

}
