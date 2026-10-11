import { z } from "zod";
import { compact, flag, isoTime, redactSecrets } from "../proxmox/format.js";
import {
  defined,
  defineTool,
  deleteKeysArg,
  deleteParam,
  digestArg,
  enc,
  extraArg,
  filterAndLimit,
  limitArg,
  rawArg,
  runTask,
  searchArg,
  waitArg,
  type ToolContext,
} from "./util.js";

type Obj = Record<string, any>;

const TOOLSET = "access" as const;

// ---------------------------------------------------------------- arguments

const useridArg = z.string().describe("User ID in name@realm form, e.g. alice@pve or root@pam (see proxmox_list_users)");
const groupidArg = z.string().describe("Group ID (see proxmox_list_groups)");
const roleidArg = z.string().describe("Role ID, e.g. PVEVMAdmin (see proxmox_list_roles)");
const realmArg = z.string().describe("Realm (authentication domain) ID, e.g. pam, pve or a configured LDAP/AD/OpenID realm (see proxmox_list_realms)");
const tokenidArg = z.string().describe("Token name (the part after '!', e.g. 'automation' for alice@pve!automation)");
const expireArg = z
  .union([z.number().int().min(0), z.string()])
  .optional()
  .describe("Expiry as UNIX seconds or an ISO date/time (e.g. 2027-01-31 or 2027-01-31T12:00:00Z); 0 = never expires");

// ---------------------------------------------------------------- helpers

/** UNIX seconds from epoch numbers or ISO date strings. */
export function toEpoch(value: number | string | undefined): number | undefined {
  if (value === undefined) return undefined;
  if (typeof value === "number") return value;
  if (/^\d+$/.test(value.trim())) return Number(value.trim());
  const ms = Date.parse(value);
  if (!Number.isFinite(ms)) throw new Error(`Invalid date/time "${value}"; use UNIX seconds or ISO 8601 such as 2027-01-31T12:00:00Z`);
  return Math.floor(ms / 1000);
}

/** Expiry for summaries: "never" for 0/absent, otherwise ISO. */
function expiry(value: unknown): string | undefined {
  if (value === undefined || value === null || value === "") return undefined;
  return Number(value) === 0 ? "never" : isoTime(value);
}

function splitList(value: unknown): string[] | undefined {
  if (Array.isArray(value)) return value.map(String);
  if (typeof value !== "string" || !value) return undefined;
  return value.split(/[,;\s]+/).filter(Boolean);
}

const joinList = (list?: string[]) => (list ? list.join(",") : undefined);

function realmOf(userid: string): string | undefined {
  const i = userid.lastIndexOf("@");
  return i >= 0 ? userid.slice(i + 1).split("!")[0] : undefined;
}

function summarizeUser(u: Obj): Obj {
  const lockedUntil = Number(u["tfa-locked-until"]);
  const tokens: Obj[] | undefined = Array.isArray(u.tokens) ? u.tokens : undefined;
  return compact({
    userid: u.userid,
    enabled: flag(u.enable) ?? true,
    expire: expiry(u.expire),
    realm: realmOf(String(u.userid ?? "")),
    realm_type: u["realm-type"],
    name: [u.firstname, u.lastname].filter(Boolean).join(" ") || undefined,
    email: u.email,
    groups: splitList(u.groups),
    tokens: tokens ? tokens.length : undefined,
    token_ids: tokens?.map((t) => t.tokenid),
    totp_locked: flag(u["totp-locked"]) || undefined,
    tfa_locked_until: lockedUntil > Date.now() / 1000 ? isoTime(lockedUntil) : undefined,
    comment: u.comment,
  });
}

function summarizeToken(userid: string, tokenid: string, t: Obj): Obj {
  return compact({
    id: `${userid}!${tokenid}`,
    userid,
    tokenid,
    privsep: flag(t.privsep) ?? true,
    expire: expiry(t.expire) ?? "same as user",
    comment: t.comment,
  });
}

/** Normalises a role's privileges, which come as a comma list (index) or a {priv: 1} map (get). */
function rolePrivs(r: Obj | string | undefined): string[] {
  if (!r) return [];
  if (typeof r === "string") return splitList(r) ?? [];
  return Object.entries(r)
    .filter(([, v]) => flag(v))
    .map(([k]) => k)
    .sort();
}

/** Groups privileges by prefix: { VM: ["VM.Audit", ...], Sys: [...] }. */
function groupPrivs(privs: string[]): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const p of [...privs].sort()) (out[p.split(".")[0]] ??= []).push(p);
  return out;
}

const REALM_SECRET_KEYS = ["password", "client-key", "bind_password"];

/** The realm `tfa` property string can hold a Yubico API key (type=yubico,id=...,key=...). */
const redactTfa = (tfa: unknown) => (typeof tfa === "string" ? tfa.replace(/(^|,)key=[^,]*/g, "$1key=<redacted>") : tfa);

function redactRealm(r: Obj): Obj {
  const out = redactSecrets(r);
  for (const k of REALM_SECRET_KEYS) if (out[k] !== undefined && out[k] !== "") out[k] = "<redacted>";
  if (out.tfa !== undefined) out.tfa = redactTfa(out.tfa);
  return out;
}

function summarizeTfaEntry(e: Obj): Obj {
  return compact({ id: e.id, type: e.type, description: e.description, created: isoTime(e.created), enabled: flag(e.enable) ?? true });
}

export function registerAccessTools(ctx: ToolContext): void {
  const { client } = ctx;

  // ================================================================ users

  defineTool(ctx, "proxmox_list_users", {
    toolset: TOOLSET,
    title: "List users",
    description:
      "List Proxmox users with enabled state, expiry, realm, name, email, groups, API token count and TFA lock-out state. Use proxmox_get_user for one user's details and tokens.",
    input: {
      enabled: z.boolean().optional().describe("Only enabled (true) or disabled (false) users"),
      realm: z.string().optional().describe("Only users of this realm, e.g. pve or pam"),
      group: z.string().optional().describe("Only members of this group"),
      search: searchArg,
      limit: limitArg(500),
      raw: rawArg,
    },
    handler: async (a) => {
      let users = await client.get<Obj[]>("/access/users", defined({ full: 1, enabled: a.enabled }));
      if (a.realm) users = users.filter((u) => realmOf(String(u.userid)) === a.realm);
      if (a.group) users = users.filter((u) => splitList(u.groups)?.includes(a.group!));
      users.sort((x, y) => String(x.userid).localeCompare(String(y.userid)));
      const res = filterAndLimit(users, { search: a.search, limit: a.limit ?? 500 }, (u) => [u.userid, u.firstname, u.lastname, u.email, u.comment, u.groups]);
      if (!a.raw) return { ...res, items: res.items.map(summarizeUser) };
      return { ...res, items: res.items.map((u) => redactSecrets({ ...u, keys: u.keys ? "<redacted>" : undefined })) };
    },
  });

  defineTool(ctx, "proxmox_get_user", {
    toolset: TOOLSET,
    title: "Get user",
    description:
      "Get one user's configuration: enabled state, expiry, name, email, groups, comment, API tokens and TFA entries. For effective permissions use proxmox_get_permissions.",
    input: { userid: useridArg },
    handler: async (a) => {
      const [u, tfa] = await Promise.all([
        client.get<Obj>(`/access/users/${enc(a.userid)}`),
        client.get<Obj[]>(`/access/tfa/${enc(a.userid)}`).catch(() => undefined),
      ]);
      const tokens = u.tokens && typeof u.tokens === "object" ? Object.entries(u.tokens as Obj).map(([id, t]) => summarizeToken(a.userid, id, t)) : [];
      return compact({
        ...summarizeUser({ ...u, userid: a.userid, tokens: undefined }),
        firstname: u.firstname,
        lastname: u.lastname,
        yubico_keys: u.keys ? "<set>" : undefined,
        tokens,
        tfa: tfa?.map(summarizeTfaEntry),
      });
    },
  });

  defineTool(ctx, "proxmox_save_user", {
    toolset: TOOLSET,
    title: "Create or update a user",
    description:
      "Create (create=true) or update a Proxmox user. Only the given fields change on update. `password` is only accepted on create and only for the pve realm (pam users need a Linux account on every node); change existing passwords with proxmox_change_password. Groups replace the user's groups unless append=true. Grant permissions afterwards with proxmox_update_acl.",
    write: true,
    destructive: false,
    input: {
      userid: useridArg,
      create: z.boolean().optional().describe("Create a new user (default false = update an existing one)"),
      password: z.string().optional().describe("Initial password (create only, pve realm; at least 8 characters). Never echoed back."),
      email: z.string().optional().describe("Email address"),
      firstname: z.string().optional().describe("First name"),
      lastname: z.string().optional().describe("Last name"),
      groups: z.array(z.string()).optional().describe("Group IDs the user belongs to (replaces the list unless append=true)"),
      append: z.boolean().optional().describe("On update, add `groups` to the existing groups instead of replacing them"),
      expire: expireArg,
      enable: z.boolean().optional().describe("Enable (true) or disable (false) the account"),
      comment: z.string().optional().describe("Comment"),
      keys: z.string().optional().describe("Yubico OTP key IDs for two-factor auth (legacy)"),
      extra: extraArg,
    },
    handler: async (a) => {
      if (a.password !== undefined && !a.create) {
        throw new Error("password can only be set when creating a user; use proxmox_change_password for existing users");
      }
      const params = defined({
        email: a.email,
        firstname: a.firstname,
        lastname: a.lastname,
        groups: joinList(a.groups),
        expire: toEpoch(a.expire),
        enable: a.enable,
        comment: a.comment,
        keys: a.keys,
        ...a.extra,
      });
      if (a.create) await client.post("/access/users", { userid: a.userid, ...defined({ password: a.password }), ...params });
      else await client.put(`/access/users/${enc(a.userid)}`, { ...defined({ append: a.append }), ...params });
      const u = await client.get<Obj>(`/access/users/${enc(a.userid)}`);
      return { [a.create ? "created" : "updated"]: a.userid, user: summarizeUser({ ...u, userid: a.userid, tokens: undefined }) };
    },
  });

  defineTool(ctx, "proxmox_delete_user", {
    toolset: TOOLSET,
    title: "Delete a user",
    description: "Delete a user, together with their API tokens and ACL entries. To only block logins, disable the user with proxmox_save_user enable=false.",
    delete: true,
    input: { userid: useridArg },
    handler: async (a) => {
      await client.delete(`/access/users/${enc(a.userid)}`);
      return { deleted: a.userid };
    },
  });

  defineTool(ctx, "proxmox_change_password", {
    toolset: TOOLSET,
    title: "Change a user's password",
    description:
      "Set a new password for a pve- or pam-realm user (PUT /access/password). The password is never echoed back. Changing another user's password needs User.Modify/Realm.AllocateUser; with ticket auth Proxmox may also require the caller's current password as confirmation_password.",
    write: true,
    destructive: true,
    idempotent: true,
    input: {
      userid: useridArg,
      password: z.string().min(5).describe("The new password (pve realm requires at least 8 characters)"),
      confirmation_password: z.string().optional().describe("Current password of the user making the change, if Proxmox asks for it"),
    },
    handler: async (a) => {
      await client.put("/access/password", defined({ userid: a.userid, password: a.password, "confirmation-password": a.confirmation_password }));
      return { password_changed: a.userid };
    },
  });

  // ================================================================ groups

  defineTool(ctx, "proxmox_list_groups", {
    toolset: TOOLSET,
    title: "List groups",
    description: "List user groups with their members and comments.",
    input: { search: searchArg },
    handler: async (a) => {
      const groups = await client.get<Obj[]>("/access/groups");
      groups.sort((x, y) => String(x.groupid).localeCompare(String(y.groupid)));
      const res = filterAndLimit(groups, { search: a.search }, (g) => [g.groupid, g.comment, g.users]);
      return { ...res, items: res.items.map((g) => compact({ groupid: g.groupid, members: splitList(g.users) ?? [], comment: g.comment })) };
    },
  });

  defineTool(ctx, "proxmox_get_group", {
    toolset: TOOLSET,
    title: "Get group",
    description: "Get a group's members and comment.",
    input: { groupid: groupidArg },
    handler: async (a) => {
      const g = await client.get<Obj>(`/access/groups/${enc(a.groupid)}`);
      return compact({ groupid: a.groupid, members: g.members ?? [], comment: g.comment });
    },
  });

  defineTool(ctx, "proxmox_save_group", {
    toolset: TOOLSET,
    title: "Create or update a group",
    description:
      "Create (create=true) a user group or update its comment. Membership is managed on the user side: proxmox_save_user with groups (and append=true to add).",
    write: true,
    destructive: false,
    input: {
      groupid: groupidArg,
      create: z.boolean().optional().describe("Create a new group (default false = update an existing one)"),
      comment: z.string().optional().describe("Comment"),
    },
    handler: async (a) => {
      if (a.create) await client.post("/access/groups", defined({ groupid: a.groupid, comment: a.comment }));
      else await client.put(`/access/groups/${enc(a.groupid)}`, defined({ comment: a.comment }));
      return { [a.create ? "created" : "updated"]: a.groupid };
    },
  });

  defineTool(ctx, "proxmox_delete_group", {
    toolset: TOOLSET,
    title: "Delete a group",
    description: "Delete a user group (users stay; their membership and the group's ACL entries are removed).",
    delete: true,
    input: { groupid: groupidArg },
    handler: async (a) => {
      await client.delete(`/access/groups/${enc(a.groupid)}`);
      return { deleted: a.groupid };
    },
  });

  // ================================================================ roles and privileges

  defineTool(ctx, "proxmox_list_roles", {
    toolset: TOOLSET,
    title: "List roles",
    description:
      "List roles with their privileges. Built-in roles (Administrator, PVEAdmin, PVEVMAdmin, PVEAuditor, NoAccess...) are flagged builtin=true and cannot be changed. Use privileges=false for names only.",
    input: {
      privileges: z.boolean().optional().describe("Include each role's privilege list (default true)"),
      builtin: z.boolean().optional().describe("Only built-in (true) or only custom (false) roles"),
      search: searchArg,
    },
    handler: async (a) => {
      let roles = await client.get<Obj[]>("/access/roles");
      if (a.builtin !== undefined) roles = roles.filter((r) => Boolean(flag(r.special)) === a.builtin);
      roles.sort((x, y) => String(x.roleid).localeCompare(String(y.roleid)));
      const res = filterAndLimit(roles, { search: a.search }, (r) => [r.roleid, r.privs]);
      return {
        ...res,
        items: res.items.map((r) => {
          const privs = rolePrivs(r.privs);
          return compact({
            roleid: r.roleid,
            builtin: flag(r.special) || undefined,
            privilege_count: privs.length,
            privileges: a.privileges === false ? undefined : privs,
          });
        }),
      };
    },
  });

  defineTool(ctx, "proxmox_get_role", {
    toolset: TOOLSET,
    title: "Get role",
    description: "Get the privileges of one role, grouped by area (VM, Datastore, Sys...).",
    input: { roleid: roleidArg },
    handler: async (a) => {
      const privs = rolePrivs(await client.get<Obj>(`/access/roles/${enc(a.roleid)}`));
      return { roleid: a.roleid, privilege_count: privs.length, privileges: privs, by_area: groupPrivs(privs) };
    },
  });

  defineTool(ctx, "proxmox_save_role", {
    toolset: TOOLSET,
    title: "Create or update a role",
    description:
      "Create (create=true) a custom role or change its privileges. On update the privilege list replaces the current one unless append=true. See proxmox_list_privileges for valid names. Built-in roles can't be modified.",
    write: true,
    destructive: true,
    input: {
      roleid: roleidArg,
      create: z.boolean().optional().describe("Create a new role (default false = update an existing one)"),
      privs: z.array(z.string()).optional().describe("Privileges, e.g. [\"VM.Audit\", \"VM.PowerMgmt\", \"Datastore.Audit\"]"),
      append: z.boolean().optional().describe("On update, add `privs` to the role instead of replacing its privileges"),
    },
    handler: async (a) => {
      // Without privs, an update would send an empty list and strip every privilege from the role.
      if (!a.create && a.privs === undefined) throw new Error("Give `privs` to update a role ([] removes all privileges)");
      if (a.create) await client.post("/access/roles", defined({ roleid: a.roleid, privs: joinList(a.privs) }));
      else await client.put(`/access/roles/${enc(a.roleid)}`, defined({ privs: joinList(a.privs), append: a.append }));
      const privs = rolePrivs(await client.get<Obj>(`/access/roles/${enc(a.roleid)}`));
      return { [a.create ? "created" : "updated"]: a.roleid, privileges: privs };
    },
  });

  defineTool(ctx, "proxmox_delete_role", {
    toolset: TOOLSET,
    title: "Delete a role",
    description: "Delete a custom role. ACL entries using it stop granting anything. Built-in roles can't be deleted.",
    delete: true,
    input: { roleid: roleidArg },
    handler: async (a) => {
      await client.delete(`/access/roles/${enc(a.roleid)}`);
      return { deleted: a.roleid };
    },
  });

  defineTool(ctx, "proxmox_list_privileges", {
    toolset: TOOLSET,
    title: "List privileges",
    description:
      "List every privilege Proxmox knows (derived from the built-in Administrator role, which holds them all), grouped by area. Use when building a custom role with proxmox_save_role.",
    input: {},
    handler: async () => {
      const privs = rolePrivs(await client.get<Obj>("/access/roles/Administrator"));
      return { count: privs.length, by_area: groupPrivs(privs) };
    },
  });

  // ================================================================ ACL and permissions

  defineTool(ctx, "proxmox_list_acl", {
    toolset: TOOLSET,
    title: "List ACL entries",
    description:
      "List access control entries: which role a user, group or API token has on which path (/, /vms/100, /storage/local, /pool/x, /nodes/pve1...) and whether it propagates to sub-paths. Filters combine. For what a user can effectively do, use proxmox_get_permissions.",
    input: {
      path: z.string().optional().describe("Only entries on this path or below it, e.g. /vms or /vms/100"),
      user: z.string().optional().describe("Only entries for this user or API token ID (exact)"),
      group: z.string().optional().describe("Only entries for this group"),
      role: z.string().optional().describe("Only entries granting this role"),
      type: z.enum(["user", "group", "token"]).optional().describe("Only entries of this subject type"),
      search: searchArg,
      limit: limitArg(1000),
    },
    handler: async (a) => {
      let acl = await client.get<Obj[]>("/access/acl");
      if (a.path) {
        const p = a.path === "/" ? "/" : a.path.replace(/\/+$/, "");
        acl = acl.filter((e) => p === "/" || e.path === p || String(e.path).startsWith(`${p}/`));
      }
      if (a.user) acl = acl.filter((e) => e.type !== "group" && e.ugid === a.user);
      if (a.group) acl = acl.filter((e) => e.type === "group" && e.ugid === a.group);
      if (a.role) acl = acl.filter((e) => e.roleid === a.role);
      if (a.type) acl = acl.filter((e) => e.type === a.type);
      acl.sort((x, y) => String(x.path).localeCompare(String(y.path)) || String(x.ugid).localeCompare(String(y.ugid)));
      const res = filterAndLimit(acl, { search: a.search, limit: a.limit ?? 1000 }, (e) => [e.path, e.ugid, e.roleid]);
      return {
        ...res,
        items: res.items.map((e) => ({ path: e.path, ugid: e.ugid, type: e.type, role: e.roleid, propagate: flag(e.propagate) ?? true })),
      };
    },
  });

  defineTool(ctx, "proxmox_update_acl", {
    toolset: TOOLSET,
    title: "Grant or revoke permissions",
    description:
      "Grant (default) or revoke (delete=true) roles on a path for users, groups and/or API tokens. Paths: / (everything), /vms/<vmid>, /storage/<id>, /pool/<pool>, /nodes/<node>, /sdn/zones/<zone>, /mapping/... Note privilege-separated API tokens need their own ACL entries (tokens=[\"user@realm!name\"]), and their effective rights are the intersection with the user's.",
    write: true,
    destructive: true,
    idempotent: true,
    input: {
      path: z.string().describe("ACL path, e.g. /, /vms/100, /storage/local, /pool/dev"),
      roles: z.array(z.string()).min(1).describe("Roles to grant or revoke, e.g. [\"PVEVMUser\"]"),
      users: z.array(z.string()).optional().describe("User IDs, e.g. [\"alice@pve\"]"),
      groups: z.array(z.string()).optional().describe("Group IDs"),
      tokens: z.array(z.string()).optional().describe("Full API token IDs, e.g. [\"alice@pve!automation\"]"),
      propagate: z.boolean().optional().describe("Inherit to sub-paths (default true)"),
      delete: z.boolean().optional().describe("Revoke the roles instead of granting them"),
    },
    handler: async (a) => {
      if (!a.users?.length && !a.groups?.length && !a.tokens?.length) throw new Error("Give at least one of users, groups or tokens");
      await client.put(
        "/access/acl",
        defined({
          path: a.path,
          roles: joinList(a.roles),
          users: a.users?.length ? joinList(a.users) : undefined,
          groups: a.groups?.length ? joinList(a.groups) : undefined,
          tokens: a.tokens?.length ? joinList(a.tokens) : undefined,
          propagate: a.propagate,
          delete: a.delete,
        }),
      );
      return compact({
        [a.delete ? "revoked" : "granted"]: a.roles,
        path: a.path,
        users: a.users,
        groups: a.groups,
        tokens: a.tokens,
        propagate: a.delete ? undefined : (a.propagate ?? true),
      });
    },
  });

  defineTool(ctx, "proxmox_get_permissions", {
    toolset: TOOLSET,
    title: "Get effective permissions",
    description:
      "Show effective privileges per path for the API token/user this server uses (default) or for another user or token. The first thing to check when a tool fails with 403 / 'Permission check failed': it shows exactly which privileges are held where. Use `path` to check a single path such as /vms/100.",
    input: {
      userid: z.string().optional().describe("User or full API token ID (user@realm!token) to check (default: the caller)"),
      path: z.string().optional().describe("Only this path, e.g. /vms/100 or /storage/local"),
    },
    handler: async (a) => {
      const perms = await client.get<Obj>("/access/permissions", defined({ userid: a.userid, path: a.path }));
      const paths = Object.keys(perms ?? {}).sort();
      return {
        subject: a.userid ?? client.cfg.tokenId ?? client.cfg.username ?? "(caller)",
        paths: Object.fromEntries(
          paths.map((p) => {
            const entries = Object.entries(perms[p] as Obj).sort(([x], [y]) => x.localeCompare(y));
            // The value is the propagate flag: privileges that don't propagate only apply to this exact path.
            const nonProp = entries.filter(([, v]) => !flag(v)).map(([k]) => k);
            return [p, compact({ privileges: entries.map(([k]) => k), not_propagated: nonProp })];
          }),
        ),
      };
    },
  });

  // ================================================================ API tokens

  defineTool(ctx, "proxmox_list_api_tokens", {
    toolset: TOOLSET,
    title: "List API tokens",
    description:
      "List API tokens (full ID user@realm!name, privilege separation, expiry, comment) for one user, or for all users when userid is omitted. Secrets can never be read back.",
    input: { userid: useridArg.optional(), search: searchArg },
    handler: async (a) => {
      let tokens: Obj[];
      if (a.userid) {
        const list = await client.get<Obj[]>(`/access/users/${enc(a.userid)}/token`);
        tokens = list.map((t) => summarizeToken(a.userid!, t.tokenid, t));
      } else {
        const users = await client.get<Obj[]>("/access/users", { full: 1 });
        tokens = users.flatMap((u) => (Array.isArray(u.tokens) ? u.tokens : []).map((t: Obj) => summarizeToken(u.userid, t.tokenid, t)));
      }
      tokens.sort((x, y) => x.id.localeCompare(y.id));
      return filterAndLimit(tokens, { search: a.search }, (t) => [t.id, t.comment]);
    },
  });

  defineTool(ctx, "proxmox_get_api_token", {
    toolset: TOOLSET,
    title: "Get API token",
    description: "Get one API token's settings (privilege separation, expiry, comment) and, when privilege-separated, its own ACL entries.",
    input: { userid: useridArg, tokenid: tokenidArg },
    handler: async (a) => {
      const t = await client.get<Obj>(`/access/users/${enc(a.userid)}/token/${enc(a.tokenid)}`);
      const id = `${a.userid}!${a.tokenid}`;
      const acl = await client.get<Obj[]>("/access/acl").catch(() => [] as Obj[]);
      return compact({
        ...summarizeToken(a.userid, a.tokenid, t),
        acl: acl
          .filter((e) => e.type === "token" && e.ugid === id)
          .map((e) => ({ path: e.path, role: e.roleid, propagate: flag(e.propagate) ?? true })),
      });
    },
  });

  defineTool(ctx, "proxmox_create_api_token", {
    toolset: TOOLSET,
    title: "Create an API token",
    description:
      "Create an API token for a user. The result contains the token SECRET, which Proxmox shows only this once: pass it on to the user to store safely. With privsep=true (default) the token has no rights until granted with proxmox_update_acl (tokens=[...]); with privsep=false it has the user's full rights.",
    write: true,
    destructive: false,
    input: {
      userid: useridArg,
      tokenid: tokenidArg,
      comment: z.string().optional().describe("Comment"),
      expire: expireArg,
      privsep: z.boolean().optional().describe("Privilege separation: restrict the token with its own ACLs (default true)"),
    },
    handler: async (a) => {
      const r = await client.post<Obj>(
        `/access/users/${enc(a.userid)}/token/${enc(a.tokenid)}`,
        defined({ comment: a.comment, expire: toEpoch(a.expire), privsep: a.privsep }),
      );
      const id = r?.["full-tokenid"] ?? `${a.userid}!${a.tokenid}`;
      // Deliberately not redacted: this is the only time the secret is available.
      return compact({
        created: id,
        token_id: id,
        secret: r?.value,
        authorization_header: r?.value ? `PVEAPIToken=${id}=${r.value}` : undefined,
        privsep: flag(r?.info?.privsep) ?? a.privsep ?? true,
        expire: expiry(r?.info?.expire),
        note: "Store the secret now: Proxmox cannot show it again. If lost, delete and recreate the token.",
      });
    },
  });

  defineTool(ctx, "proxmox_update_api_token", {
    toolset: TOOLSET,
    title: "Update an API token",
    description:
      "Change an API token's comment, expiry or privilege separation. regenerate=true issues a new secret (returned once, store it) and immediately invalidates the old one.",
    write: true,
    destructive: true,
    input: {
      userid: useridArg,
      tokenid: tokenidArg,
      comment: z.string().optional().describe("Comment"),
      expire: expireArg,
      privsep: z.boolean().optional().describe("Privilege separation (true = token limited to its own ACLs)"),
      regenerate: z.boolean().optional().describe("Generate a new secret; everything using the old one loses access"),
      delete: deleteKeysArg,
    },
    handler: async (a) => {
      const r = await client.put<Obj>(
        `/access/users/${enc(a.userid)}/token/${enc(a.tokenid)}`,
        defined({ comment: a.comment, expire: toEpoch(a.expire), privsep: a.privsep, regenerate: a.regenerate, delete: deleteParam(a.delete) }),
      );
      const id = `${a.userid}!${a.tokenid}`;
      return compact({
        updated: id,
        token: summarizeToken(a.userid, a.tokenid, r ?? {}),
        secret: r?.value,
        authorization_header: r?.value ? `PVEAPIToken=${id}=${r.value}` : undefined,
        note: r?.value ? "New secret shown once: store it now. The old secret no longer works." : undefined,
      });
    },
  });

  defineTool(ctx, "proxmox_delete_api_token", {
    toolset: TOOLSET,
    title: "Delete an API token",
    description: "Delete (revoke) an API token; anything using it loses access immediately.",
    delete: true,
    input: { userid: useridArg, tokenid: tokenidArg },
    handler: async (a) => {
      await client.delete(`/access/users/${enc(a.userid)}/token/${enc(a.tokenid)}`);
      return { deleted: `${a.userid}!${a.tokenid}` };
    },
  });

  // ================================================================ realms

  defineTool(ctx, "proxmox_list_realms", {
    toolset: TOOLSET,
    title: "List realms",
    description: "List authentication realms (pam, pve, LDAP, Active Directory, OpenID Connect) with type, TFA provider and comment.",
    input: {},
    handler: async () => {
      const realms = await client.get<Obj[]>("/access/domains");
      return realms.map((r) => compact({ realm: r.realm, type: r.type, tfa: redactTfa(r.tfa), comment: r.comment }));
    },
  });

  defineTool(ctx, "proxmox_get_realm", {
    toolset: TOOLSET,
    title: "Get realm",
    description: "Get an authentication realm's configuration (servers, base DN, sync options, OpenID issuer...). Bind passwords and client keys are redacted.",
    input: { realm: realmArg },
    handler: async (a) => redactRealm({ realm: a.realm, ...(await client.get<Obj>(`/access/domains/${enc(a.realm)}`)) }),
  });

  defineTool(ctx, "proxmox_save_realm", {
    toolset: TOOLSET,
    title: "Create or update a realm",
    description:
      "Create (create=true, with `type`) or update an authentication realm: LDAP/AD (server1, base_dn, user_attr, bind_dn + bind_password, mode), OpenID Connect (issuer_url, client_id, client_key, username_claim) or pam/pve (comment, default, tfa). Only the given fields change on update. Anything else via `extra` (e.g. group_dn, filter, sync_attributes, groups-claim). Secrets are never echoed back.",
    write: true,
    destructive: true,
    input: {
      realm: realmArg,
      create: z.boolean().optional().describe("Create a new realm (default false = update)"),
      type: z.enum(["pam", "pve", "ldap", "ad", "openid"]).optional().describe("Realm type (required when creating)"),
      comment: z.string().optional().describe("Description shown on the login screen"),
      default: z.boolean().optional().describe("Make this the default realm on the login screen"),
      tfa: z.string().optional().describe("Realm-enforced TFA, e.g. 'type=oath' or 'type=yubico,id=...,key=...'"),
      server1: z.string().optional().describe("LDAP/AD server address"),
      server2: z.string().optional().describe("Fallback LDAP/AD server"),
      port: z.number().int().positive().optional().describe("LDAP/AD server port"),
      mode: z.enum(["ldap", "ldaps", "ldap+starttls"]).optional().describe("LDAP/AD connection mode"),
      verify: z.boolean().optional().describe("Verify the LDAP server's TLS certificate"),
      base_dn: z.string().optional().describe("LDAP base DN for users, e.g. ou=people,dc=example,dc=com"),
      user_attr: z.string().optional().describe("LDAP user name attribute, e.g. uid (required for LDAP)"),
      bind_dn: z.string().optional().describe("LDAP bind DN"),
      bind_password: z.string().optional().describe("LDAP bind password (sent as `password`; never echoed)"),
      domain: z.string().optional().describe("AD domain, e.g. example.com (required for AD)"),
      issuer_url: z.string().optional().describe("OpenID issuer URL"),
      client_id: z.string().optional().describe("OpenID client ID"),
      client_key: z.string().optional().describe("OpenID client secret (never echoed)"),
      username_claim: z.string().optional().describe("OpenID claim used for the username (create only), e.g. email or preferred_username"),
      scopes: z.string().optional().describe("OpenID scopes (default 'email profile')"),
      autocreate: z.boolean().optional().describe("Automatically create users on first login (OpenID)"),
      sync_defaults_options: z.string().optional().describe("Default sync options, e.g. 'scope=both,enable-new=1,remove-vanished=acl;entry'"),
      delete: deleteKeysArg,
      digest: digestArg,
      extra: extraArg,
    },
    handler: async (a) => {
      const params = defined({
        comment: a.comment,
        default: a.default,
        tfa: a.tfa,
        server1: a.server1,
        server2: a.server2,
        port: a.port,
        mode: a.mode,
        verify: a.verify,
        base_dn: a.base_dn,
        user_attr: a.user_attr,
        bind_dn: a.bind_dn,
        password: a.bind_password,
        domain: a.domain,
        "issuer-url": a.issuer_url,
        "client-id": a.client_id,
        "client-key": a.client_key,
        "username-claim": a.username_claim,
        scopes: a.scopes,
        autocreate: a.autocreate,
        "sync-defaults-options": a.sync_defaults_options,
        ...a.extra,
      });
      if (a.create) {
        if (!a.type) throw new Error("type is required when creating a realm");
        await client.post("/access/domains", { realm: a.realm, type: a.type, ...params });
      } else {
        await client.put(`/access/domains/${enc(a.realm)}`, { ...params, ...defined({ delete: deleteParam(a.delete), digest: a.digest }) });
      }
      const cfg = await client.get<Obj>(`/access/domains/${enc(a.realm)}`).catch(() => undefined);
      return compact({ [a.create ? "created" : "updated"]: a.realm, realm: cfg ? redactRealm(cfg) : undefined });
    },
  });

  defineTool(ctx, "proxmox_delete_realm", {
    toolset: TOOLSET,
    title: "Delete a realm",
    description: "Delete an authentication realm. Users of that realm can no longer log in (their user entries remain until deleted). pam and pve can't be deleted.",
    delete: true,
    input: { realm: realmArg },
    handler: async (a) => {
      await client.delete(`/access/domains/${enc(a.realm)}`);
      return { deleted: a.realm };
    },
  });

  defineTool(ctx, "proxmox_sync_realm", {
    toolset: TOOLSET,
    title: "Sync users/groups from a realm",
    description:
      "Sync users and/or groups from an LDAP, AD or OpenID realm into Proxmox. Use dry_run=true first to see what would change (in the task log). Options not given fall back to the realm's sync-defaults-options. Synced groups are named <group>-<realm>.",
    write: true,
    destructive: true,
    input: {
      realm: realmArg,
      dry_run: z.boolean().optional().describe("Only report what would change; write nothing"),
      scope: z.enum(["users", "groups", "both"]).optional().describe("What to sync"),
      enable_new: z.boolean().optional().describe("Enable newly synced users (default true)"),
      remove_vanished: z
        .string()
        .optional()
        .describe("What to remove for users/groups no longer in the directory: 'none' or a ;-list of acl, properties, entry (e.g. 'acl;entry')"),
      wait: waitArg,
    },
    handler: async (a) => {
      const upid = await client.post(
        `/access/domains/${enc(a.realm)}/sync`,
        defined({ "dry-run": a.dry_run, scope: a.scope, "enable-new": a.enable_new, "remove-vanished": a.remove_vanished }),
      );
      return runTask(client, upid, a.wait, compact({ realm: a.realm, dry_run: a.dry_run || undefined }));
    },
  });

  // ================================================================ TFA

  defineTool(ctx, "proxmox_list_tfa", {
    toolset: TOOLSET,
    title: "List TFA entries",
    description:
      "List users' second factors (TOTP, WebAuthn, recovery keys, Yubico) with type, description, creation time and enabled state, plus TFA lock-outs. No secrets are returned. Unlock a locked-out user with proxmox_unlock_tfa.",
    input: { userid: useridArg.optional() },
    handler: async (a) => {
      if (a.userid) {
        const entries = await client.get<Obj[]>(`/access/tfa/${enc(a.userid)}`);
        return { userid: a.userid, entries: entries.map(summarizeTfaEntry) };
      }
      const all = await client.get<Obj[]>("/access/tfa");
      return all
        .map((u) => {
          const lockedUntil = Number(u["tfa-locked-until"]);
          return compact({
            userid: u.userid,
            totp_locked: flag(u["totp-locked"]) || undefined,
            tfa_locked_until: lockedUntil > Date.now() / 1000 ? isoTime(lockedUntil) : undefined,
            entries: (u.entries ?? []).map(summarizeTfaEntry),
          });
        })
        .sort((x, y) => String(x.userid).localeCompare(String(y.userid)));
    },
  });

  defineTool(ctx, "proxmox_delete_tfa", {
    toolset: TOOLSET,
    title: "Delete a TFA entry",
    description:
      "Remove one second factor from a user (e.g. a lost authenticator); get the ID from proxmox_list_tfa. Removing a user's last factor lets them log in with the password alone.",
    delete: true,
    input: {
      userid: useridArg,
      id: z.string().describe("TFA entry ID (from proxmox_list_tfa)"),
      password: z.string().optional().describe("Current password of the user making the change, if Proxmox requires it"),
    },
    handler: async (a) => {
      await client.delete(`/access/tfa/${enc(a.userid)}/${enc(a.id)}`, defined({ password: a.password }));
      return { deleted: a.id, userid: a.userid };
    },
  });

  defineTool(ctx, "proxmox_unlock_tfa", {
    toolset: TOOLSET,
    title: "Unlock a user's TFA",
    description: "Clear a user's TFA lock-out after too many failed second-factor attempts (see totp_locked / tfa_locked_until in proxmox_list_tfa).",
    write: true,
    destructive: false,
    idempotent: true,
    input: { userid: useridArg },
    handler: async (a) => {
      const r = await client.put(`/access/users/${enc(a.userid)}/unlock-tfa`);
      return { userid: a.userid, was_locked: flag(r) ?? undefined, unlocked: true };
    },
  });

  // ================================================================ realm sync jobs

  defineTool(ctx, "proxmox_list_realm_sync_jobs", {
    toolset: TOOLSET,
    title: "List realm sync jobs",
    description: "List scheduled realm sync jobs (periodic LDAP/AD/OpenID user and group sync) with schedule, last and next run.",
    input: {},
    handler: async () => {
      const jobs = await client.get<Obj[]>("/cluster/jobs/realm-sync");
      return jobs.map((j) =>
        compact({
          id: j.id,
          realm: j.realm,
          enabled: flag(j.enabled) ?? true,
          schedule: j.schedule,
          scope: j.scope,
          remove_vanished: j["remove-vanished"],
          last_run: isoTime(j["last-run"]),
          next_run: isoTime(j["next-run"]),
          comment: j.comment,
        }),
      );
    },
  });

  defineTool(ctx, "proxmox_save_realm_sync_job", {
    toolset: TOOLSET,
    title: "Create or update a realm sync job",
    description:
      "Create (create=true, with realm and schedule) or update a scheduled realm sync job. Check schedules with proxmox_simulate_schedule. For a one-off sync use proxmox_sync_realm.",
    write: true,
    destructive: false,
    input: {
      id: z.string().describe("Job ID"),
      create: z.boolean().optional().describe("Create a new job (default false = update)"),
      realm: z.string().optional().describe("Realm to sync (create only)"),
      schedule: z.string().optional().describe("Calendar event, e.g. 'daily' or 'sun 03:00' (required when creating)"),
      enabled: z.boolean().optional().describe("Whether the job runs"),
      scope: z.enum(["users", "groups", "both"]).optional().describe("What to sync"),
      enable_new: z.boolean().optional().describe("Enable newly synced users"),
      remove_vanished: z.string().optional().describe("'none' or a ;-list of acl, properties, entry"),
      comment: z.string().optional().describe("Comment"),
      delete: deleteKeysArg,
    },
    handler: async (a) => {
      const params = defined({
        enabled: a.enabled,
        scope: a.scope,
        "enable-new": a.enable_new,
        "remove-vanished": a.remove_vanished,
        comment: a.comment,
      });
      const path = `/cluster/jobs/realm-sync/${enc(a.id)}`;
      if (a.create) {
        if (!a.realm || !a.schedule) throw new Error("realm and schedule are required when creating a realm sync job");
        await client.post(path, { realm: a.realm, schedule: a.schedule, ...params });
      } else {
        // PUT requires the schedule; keep the current one when not changing it.
        const schedule = a.schedule ?? (await client.get<Obj>(path)).schedule;
        await client.put(path, { schedule, ...params, ...defined({ delete: deleteParam(a.delete) }) });
      }
      return { [a.create ? "created" : "updated"]: a.id, job: await client.get(path).catch(() => undefined) };
    },
  });

  defineTool(ctx, "proxmox_delete_realm_sync_job", {
    toolset: TOOLSET,
    title: "Delete a realm sync job",
    description: "Delete a scheduled realm sync job (the realm and synced users stay).",
    delete: true,
    input: { id: z.string().describe("Job ID") },
    handler: async (a) => {
      await client.delete(`/cluster/jobs/realm-sync/${enc(a.id)}`);
      return { deleted: a.id };
    },
  });
}
