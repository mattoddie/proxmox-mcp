import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { toEpoch } from "../tools/access.js";
import { errorText, startEnv, type TestEnv } from "./helpers.js";

const FUTURE = Math.floor(Date.now() / 1000) + 3600;

const USERS = [
  {
    userid: "root@pam",
    enable: 1,
    expire: 0,
    firstname: "Root",
    email: "root@example.com",
    "realm-type": "pam",
    tokens: [{ tokenid: "test", privsep: 0, expire: 0 }],
  },
  {
    userid: "alice@pve",
    enable: 1,
    expire: 1830297600,
    firstname: "Alice",
    lastname: "Smith",
    groups: "admins,ops",
    "realm-type": "pve",
    tokens: [{ tokenid: "ci", privsep: 1, comment: "CI" }, { tokenid: "ro" }],
    "totp-locked": 1,
    "tfa-locked-until": FUTURE,
  },
  { userid: "bob@ldap", enable: 0, groups: "ops", "realm-type": "ldap", keys: "secretkey" },
];

const ADMIN_PRIVS = { "VM.Audit": 1, "VM.PowerMgmt": 1, "Sys.Audit": 1, "Datastore.Audit": 1, "Datastore.Allocate": 1 };

const ACL = [
  { path: "/", ugid: "admins", type: "group", roleid: "Administrator", propagate: 1 },
  { path: "/vms/100", ugid: "alice@pve", type: "user", roleid: "PVEVMUser", propagate: 0 },
  { path: "/vms", ugid: "alice@pve!ci", type: "token", roleid: "PVEVMAdmin", propagate: 1 },
  { path: "/storage/local", ugid: "bob@ldap", type: "user", roleid: "PVEDatastoreUser", propagate: 1 },
  { path: "/vmsx", ugid: "bob@ldap", type: "user", roleid: "PVEAuditor", propagate: 1 },
];

describe("access tools", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv();
  });
  after(() => env.stop());

  beforeEach(() => {
    env.mock.reset();
    env.mock.on("GET", /^\/access\/users$/, () => structuredClone(USERS));
    env.mock.on("GET", /^\/access\/acl$/, () => structuredClone(ACL));
    env.mock.on("GET", /^\/access\/roles\/Administrator$/, () => ADMIN_PRIVS);
  });

  // ---------------------------------------------------------------- users

  it("lists users with summaries and filters", async () => {
    const res = await env.call("proxmox_list_users");
    assert.equal(res.total, 3);
    assert.equal(env.mock.requestsTo("/access/users", "GET")[0].query.full, "1");
    const alice = res.items.find((u: any) => u.userid === "alice@pve");
    assert.equal(alice.enabled, true);
    assert.equal(alice.expire, new Date(1830297600 * 1000).toISOString());
    assert.equal(alice.realm, "pve");
    assert.equal(alice.name, "Alice Smith");
    assert.deepEqual(alice.groups, ["admins", "ops"]);
    assert.equal(alice.tokens, 2);
    assert.deepEqual(alice.token_ids, ["ci", "ro"]);
    assert.equal(alice.totp_locked, true);
    assert.ok(alice.tfa_locked_until);
    const root = res.items.find((u: any) => u.userid === "root@pam");
    assert.equal(root.expire, "never");
    const bob = res.items.find((u: any) => u.userid === "bob@ldap");
    assert.equal(bob.enabled, false);

    assert.deepEqual((await env.call("proxmox_list_users", { realm: "pve" })).items.map((u: any) => u.userid), ["alice@pve"]);
    assert.deepEqual((await env.call("proxmox_list_users", { group: "ops" })).items.map((u: any) => u.userid), ["alice@pve", "bob@ldap"]);
    assert.equal((await env.call("proxmox_list_users", { search: "smith" })).total, 1);
    await env.call("proxmox_list_users", { enabled: false });
    assert.equal(env.mock.requestsTo("/access/users", "GET").at(-1)!.query.enabled, "0");
    const raw = await env.call("proxmox_list_users", { raw: true });
    assert.equal(raw.items.find((u: any) => u.userid === "bob@ldap").keys, "<redacted>");
  });

  it("gets a user with tokens and TFA entries", async () => {
    env.mock.on("GET", /^\/access\/users\/alice%40pve$/, () => ({
      enable: 1,
      firstname: "Alice",
      groups: ["admins"],
      tokens: { ci: { privsep: 1, expire: 0, comment: "CI" } },
      keys: "x",
    }));
    env.mock.on("GET", /^\/access\/tfa\/alice%40pve$/, () => [{ id: "totp1", type: "totp", description: "phone", created: 1700000000, enable: 1 }]);
    const u = await env.call("proxmox_get_user", { userid: "alice@pve" });
    assert.equal(u.userid, "alice@pve");
    assert.deepEqual(u.groups, ["admins"]);
    assert.equal(u.yubico_keys, "<set>");
    assert.deepEqual(u.tokens, [{ id: "alice@pve!ci", userid: "alice@pve", tokenid: "ci", privsep: true, expire: "never", comment: "CI" }]);
    assert.equal(u.tfa[0].type, "totp");
    assert.equal(u.tfa[0].created, new Date(1700000000 * 1000).toISOString());
  });

  it("creates a user without echoing the password", async () => {
    env.mock.on("GET", /^\/access\/users\/carol%40pve$/, () => ({ enable: 1, email: "c@example.com", groups: ["ops"] }));
    const res = await env.call("proxmox_save_user", {
      userid: "carol@pve",
      create: true,
      password: "s3cretPassw0rd",
      email: "c@example.com",
      groups: ["ops", "dev"],
      expire: "2027-01-01T00:00:00Z",
      enable: true,
    });
    const w = env.mock.requestsTo("/access/users", "POST")[0];
    assert.equal(w.path, "/access/users");
    assert.equal(w.body.userid, "carol@pve");
    assert.equal(w.body.password, "s3cretPassw0rd");
    assert.equal(w.body.groups, "ops,dev");
    assert.equal(w.body.expire, String(Date.UTC(2027, 0, 1) / 1000));
    assert.equal(w.body.enable, "1");
    assert.equal(res.created, "carol@pve");
    assert.ok(!JSON.stringify(res).includes("s3cretPassw0rd"));
  });

  it("updates a user with append, and refuses passwords on update", async () => {
    env.mock.on("GET", /^\/access\/users\/alice%40pve$/, () => ({ enable: 0 }));
    const res = await env.call("proxmox_save_user", { userid: "alice@pve", groups: ["dev"], append: true, enable: false, expire: 0 });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, "/access/users/alice%40pve");
    assert.deepEqual(w.body, { append: "1", groups: "dev", enable: "0", expire: "0" });
    assert.equal(res.updated, "alice@pve");
    assert.equal(res.user.enabled, false);

    const err = errorText(await env.callRaw("proxmox_save_user", { userid: "alice@pve", password: "newpassword" }));
    assert.match(err, /proxmox_change_password/);
    assert.match(errorText(await env.callRaw("proxmox_save_user", { userid: "alice@pve", expire: "not a date" })), /Invalid date/);
  });

  it("deletes a user", async () => {
    assert.deepEqual(await env.call("proxmox_delete_user", { userid: "bob@ldap" }), { deleted: "bob@ldap" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "DELETE");
    assert.equal(w.path, "/access/users/bob%40ldap");
  });

  it("changes a password without echoing it", async () => {
    const res = await env.call("proxmox_change_password", { userid: "alice@pve", password: "BrandNewPass1", confirmation_password: "OldPass" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, "/access/password");
    assert.deepEqual(w.body, { userid: "alice@pve", password: "BrandNewPass1", "confirmation-password": "OldPass" });
    assert.deepEqual(res, { password_changed: "alice@pve" });
  });

  // ---------------------------------------------------------------- groups

  it("lists, gets, saves and deletes groups", async () => {
    env.mock.on("GET", /^\/access\/groups$/, () => [
      { groupid: "ops", users: "alice@pve,bob@ldap", comment: "Operations" },
      { groupid: "admins", users: "alice@pve" },
    ]);
    env.mock.on("GET", /^\/access\/groups\/ops$/, () => ({ members: ["alice@pve", "bob@ldap"], comment: "Operations" }));
    const list = await env.call("proxmox_list_groups");
    assert.deepEqual(list.items[0], { groupid: "admins", members: ["alice@pve"] });
    assert.deepEqual(list.items[1].members, ["alice@pve", "bob@ldap"]);
    assert.equal((await env.call("proxmox_list_groups", { search: "operations" })).total, 1);

    const g = await env.call("proxmox_get_group", { groupid: "ops" });
    assert.deepEqual(g, { groupid: "ops", members: ["alice@pve", "bob@ldap"], comment: "Operations" });

    await env.call("proxmox_save_group", { groupid: "dev", create: true, comment: "Developers" });
    assert.deepEqual(env.mock.lastWrite()!.body, { groupid: "dev", comment: "Developers" });
    assert.equal(env.mock.lastWrite()!.path, "/access/groups");

    await env.call("proxmox_save_group", { groupid: "dev", comment: "Devs" });
    assert.equal(env.mock.lastWrite()!.method, "PUT");
    assert.equal(env.mock.lastWrite()!.path, "/access/groups/dev");

    await env.call("proxmox_delete_group", { groupid: "dev" });
    assert.equal(env.mock.lastWrite()!.method, "DELETE");
    assert.equal(env.mock.lastWrite()!.path, "/access/groups/dev");
  });

  // ---------------------------------------------------------------- roles

  it("lists roles flagging built-ins", async () => {
    env.mock.on("GET", /^\/access\/roles$/, () => [
      { roleid: "PVEAuditor", special: 1, privs: "VM.Audit,Sys.Audit" },
      { roleid: "Custom", privs: "VM.PowerMgmt" },
      { roleid: "NoAccess", special: 1, privs: "" },
    ]);
    const all = await env.call("proxmox_list_roles");
    assert.deepEqual(all.items[0], { roleid: "Custom", privilege_count: 1, privileges: ["VM.PowerMgmt"] });
    assert.deepEqual(all.items[2], { roleid: "PVEAuditor", builtin: true, privilege_count: 2, privileges: ["VM.Audit", "Sys.Audit"] });
    const custom = await env.call("proxmox_list_roles", { builtin: false, privileges: false });
    assert.deepEqual(custom.items, [{ roleid: "Custom", privilege_count: 1 }]);
  });

  it("gets a role and the privilege list", async () => {
    env.mock.on("GET", /^\/access\/roles\/Custom$/, () => ({ "VM.Audit": 1, "VM.PowerMgmt": 1, "Sys.Audit": 1 }));
    const r = await env.call("proxmox_get_role", { roleid: "Custom" });
    assert.deepEqual(r.privileges, ["Sys.Audit", "VM.Audit", "VM.PowerMgmt"]);
    assert.deepEqual(r.by_area, { Sys: ["Sys.Audit"], VM: ["VM.Audit", "VM.PowerMgmt"] });

    const p = await env.call("proxmox_list_privileges");
    assert.equal(p.count, 5);
    assert.deepEqual(p.by_area.Datastore, ["Datastore.Allocate", "Datastore.Audit"]);
  });

  it("saves and deletes roles", async () => {
    env.mock.on("GET", /^\/access\/roles\/Ops$/, () => ({ "VM.Audit": 1, "VM.PowerMgmt": 1 }));
    const created = await env.call("proxmox_save_role", { roleid: "Ops", create: true, privs: ["VM.Audit", "VM.PowerMgmt"] });
    assert.deepEqual(env.mock.lastWrite()!.body, { roleid: "Ops", privs: "VM.Audit,VM.PowerMgmt" });
    assert.equal(env.mock.lastWrite()!.method, "POST");
    assert.deepEqual(created.privileges, ["VM.Audit", "VM.PowerMgmt"]);

    await env.call("proxmox_save_role", { roleid: "Ops", privs: ["VM.Console"], append: true });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, "/access/roles/Ops");
    assert.deepEqual(w.body, { privs: "VM.Console", append: "1" });
    // An update without privs would wipe the role, so it is refused.
    assert.match(errorText(await env.callRaw("proxmox_save_role", { roleid: "Ops" })), /Give `privs`/);
    await env.call("proxmox_save_role", { roleid: "Ops", privs: [] });
    assert.deepEqual(env.mock.lastWrite()!.body, { privs: "" });

    await env.call("proxmox_delete_role", { roleid: "Ops" });
    assert.equal(env.mock.lastWrite()!.method, "DELETE");
    assert.equal(env.mock.lastWrite()!.path, "/access/roles/Ops");
  });

  // ---------------------------------------------------------------- ACL / permissions

  it("lists and filters ACL entries", async () => {
    const all = await env.call("proxmox_list_acl");
    assert.equal(all.total, 5);
    assert.deepEqual(all.items[0], { path: "/", ugid: "admins", type: "group", role: "Administrator", propagate: true });

    const vms = await env.call("proxmox_list_acl", { path: "/vms" });
    assert.deepEqual(vms.items.map((e: any) => e.path).sort(), ["/vms", "/vms/100"]);
    const alice = await env.call("proxmox_list_acl", { user: "alice@pve" });
    assert.deepEqual(alice.items, [{ path: "/vms/100", ugid: "alice@pve", type: "user", role: "PVEVMUser", propagate: false }]);
    assert.equal((await env.call("proxmox_list_acl", { group: "admins" })).total, 1);
    assert.equal((await env.call("proxmox_list_acl", { type: "token" })).items[0].ugid, "alice@pve!ci");
    assert.equal((await env.call("proxmox_list_acl", { role: "PVEDatastoreUser" })).total, 1);
  });

  it("grants and revokes ACL entries", async () => {
    const res = await env.call("proxmox_update_acl", { path: "/vms/100", roles: ["PVEVMUser"], users: ["alice@pve", "bob@ldap"], tokens: ["alice@pve!ci"] });
    let w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, "/access/acl");
    assert.deepEqual(w.body, { path: "/vms/100", roles: "PVEVMUser", users: "alice@pve,bob@ldap", tokens: "alice@pve!ci" });
    assert.deepEqual(res.granted, ["PVEVMUser"]);
    assert.equal(res.propagate, true);

    const rev = await env.call("proxmox_update_acl", { path: "/", roles: ["Administrator"], groups: ["admins"], delete: true, propagate: false });
    w = env.mock.lastWrite()!;
    assert.deepEqual(w.body, { path: "/", roles: "Administrator", groups: "admins", propagate: "0", delete: "1" });
    assert.deepEqual(rev.revoked, ["Administrator"]);

    const n = env.mock.requests.length;
    assert.match(errorText(await env.callRaw("proxmox_update_acl", { path: "/", roles: ["PVEAuditor"] })), /at least one of users, groups or tokens/);
    assert.equal(env.mock.requests.length, n);
  });

  it("returns effective permissions", async () => {
    env.mock.on("GET", /^\/access\/permissions$/, () => ({
      "/": { "Sys.Audit": 1, "VM.Audit": 1 },
      "/vms/100": { "VM.PowerMgmt": 0, "VM.Audit": 1 },
    }));
    const own = await env.call("proxmox_get_permissions");
    assert.equal(own.subject, "root@pam!test");
    assert.deepEqual(own.paths["/"], { privileges: ["Sys.Audit", "VM.Audit"] });
    assert.deepEqual(own.paths["/vms/100"], { privileges: ["VM.Audit", "VM.PowerMgmt"], not_propagated: ["VM.PowerMgmt"] });

    const other = await env.call("proxmox_get_permissions", { userid: "alice@pve", path: "/vms/100" });
    assert.equal(other.subject, "alice@pve");
    const q = env.mock.requestsTo("/access/permissions").at(-1)!.query;
    assert.deepEqual(q, { userid: "alice@pve", path: "/vms/100" });
  });

  // ---------------------------------------------------------------- tokens

  it("lists API tokens for all users or one user", async () => {
    const all = await env.call("proxmox_list_api_tokens");
    assert.deepEqual(all.items.map((t: any) => t.id), ["alice@pve!ci", "alice@pve!ro", "root@pam!test"]);
    assert.deepEqual(all.items[0], { id: "alice@pve!ci", userid: "alice@pve", tokenid: "ci", privsep: true, expire: "same as user", comment: "CI" });
    assert.equal(all.items[2].privsep, false);
    assert.equal(all.items[2].expire, "never");

    env.mock.on("GET", /^\/access\/users\/alice%40pve\/token$/, () => [{ tokenid: "ci", privsep: 1 }]);
    const one = await env.call("proxmox_list_api_tokens", { userid: "alice@pve" });
    assert.deepEqual(one.items.map((t: any) => t.id), ["alice@pve!ci"]);
  });

  it("gets an API token with its ACL", async () => {
    env.mock.on("GET", /^\/access\/users\/alice%40pve\/token\/ci$/, () => ({ privsep: 1, expire: FUTURE, comment: "CI" }));
    const t = await env.call("proxmox_get_api_token", { userid: "alice@pve", tokenid: "ci" });
    assert.equal(t.id, "alice@pve!ci");
    assert.equal(t.expire, new Date(FUTURE * 1000).toISOString());
    assert.deepEqual(t.acl, [{ path: "/vms", role: "PVEVMAdmin", propagate: true }]);
  });

  it("creates an API token and returns the secret once", async () => {
    env.mock.on("POST", /^\/access\/users\/alice%40pve\/token\/deploy$/, () => ({
      "full-tokenid": "alice@pve!deploy",
      value: "aaaa-bbbb-cccc",
      info: { privsep: 1, expire: 0 },
    }));
    const r = await env.call("proxmox_create_api_token", { userid: "alice@pve", tokenid: "deploy", comment: "deploy", privsep: true, expire: 0 });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.equal(w.path, "/access/users/alice%40pve/token/deploy");
    assert.deepEqual(w.body, { comment: "deploy", expire: "0", privsep: "1" });
    assert.equal(r.token_id, "alice@pve!deploy");
    assert.equal(r.secret, "aaaa-bbbb-cccc");
    assert.equal(r.authorization_header, "PVEAPIToken=alice@pve!deploy=aaaa-bbbb-cccc");
    assert.equal(r.privsep, true);
    assert.match(r.note, /cannot show it again/);
  });

  it("updates, regenerates and deletes API tokens", async () => {
    env.mock.on("PUT", /^\/access\/users\/alice%40pve\/token\/ci$/, (p) =>
      p.regenerate === "1" ? { privsep: 1, value: "new-secret", "full-tokenid": "alice@pve!ci" } : { privsep: 0, comment: p.comment },
    );
    const upd = await env.call("proxmox_update_api_token", { userid: "alice@pve", tokenid: "ci", comment: "new", privsep: false, delete: ["expire"] });
    assert.deepEqual(env.mock.lastWrite()!.body, { comment: "new", privsep: "0", delete: "expire" });
    assert.equal(upd.token.privsep, false);
    assert.equal(upd.secret, undefined);

    const regen = await env.call("proxmox_update_api_token", { userid: "alice@pve", tokenid: "ci", regenerate: true });
    assert.equal(regen.secret, "new-secret");
    assert.match(regen.note, /old secret no longer works/);

    await env.call("proxmox_delete_api_token", { userid: "alice@pve", tokenid: "ci" });
    assert.equal(env.mock.lastWrite()!.method, "DELETE");
    assert.equal(env.mock.lastWrite()!.path, "/access/users/alice%40pve/token/ci");
  });

  // ---------------------------------------------------------------- realms

  it("lists and gets realms with secrets redacted", async () => {
    env.mock.on("GET", /^\/access\/domains$/, () => [
      { realm: "pam", type: "pam", comment: "Linux PAM" },
      { realm: "corp", type: "ldap", tfa: "oath" },
      { realm: "yk", type: "pve", tfa: "type=yubico,id=42,key=yubisecret,url=https://yk" },
    ]);
    env.mock.on("GET", /^\/access\/domains\/corp$/, () => ({
      type: "ldap",
      server1: "ldap.corp",
      base_dn: "dc=corp",
      bind_dn: "cn=bind,dc=corp",
      password: "bindpw",
      "client-key": "oidc-secret",
      digest: "d1",
    }));
    const list = await env.call("proxmox_list_realms");
    assert.deepEqual(list[1], { realm: "corp", type: "ldap", tfa: "oath" });
    assert.equal(list[2].tfa, "type=yubico,id=42,key=<redacted>,url=https://yk");
    const r = await env.call("proxmox_get_realm", { realm: "corp" });
    assert.equal(r.realm, "corp");
    assert.equal(r.server1, "ldap.corp");
    assert.equal(r.password, "<redacted>");
    assert.equal(r["client-key"], "<redacted>");
    assert.ok(!JSON.stringify(r).includes("bindpw"));
  });

  it("creates and updates realms", async () => {
    env.mock.on("GET", /^\/access\/domains\/corp$/, () => ({ type: "ldap", server1: "ldap.corp", password: "bindpw" }));
    const created = await env.call("proxmox_save_realm", {
      realm: "corp",
      create: true,
      type: "ldap",
      server1: "ldap.corp",
      base_dn: "dc=corp",
      user_attr: "uid",
      bind_dn: "cn=bind,dc=corp",
      bind_password: "bindpw",
      mode: "ldaps",
      extra: { group_dn: "ou=groups,dc=corp" },
    });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.equal(w.path, "/access/domains");
    assert.deepEqual(w.body, {
      realm: "corp",
      type: "ldap",
      server1: "ldap.corp",
      mode: "ldaps",
      base_dn: "dc=corp",
      user_attr: "uid",
      bind_dn: "cn=bind,dc=corp",
      password: "bindpw",
      group_dn: "ou=groups,dc=corp",
    });
    assert.ok(!JSON.stringify(created).includes("bindpw"));

    await env.call("proxmox_save_realm", { realm: "sso", issuer_url: "https://idp", client_id: "pve", client_key: "k", delete: ["comment"], digest: "d" });
    const u = env.mock.lastWrite()!;
    assert.equal(u.method, "PUT");
    assert.equal(u.path, "/access/domains/sso");
    assert.deepEqual(u.body, { "issuer-url": "https://idp", "client-id": "pve", "client-key": "k", delete: "comment", digest: "d" });

    assert.match(errorText(await env.callRaw("proxmox_save_realm", { realm: "x", create: true })), /type is required/);
  });

  it("deletes and syncs realms", async () => {
    await env.call("proxmox_delete_realm", { realm: "corp" });
    assert.equal(env.mock.lastWrite()!.method, "DELETE");
    assert.equal(env.mock.lastWrite()!.path, "/access/domains/corp");

    const upid = env.mock.upid("pve1", "auth-realm-sync", "corp");
    env.mock.on("POST", /^\/access\/domains\/corp\/sync$/, () => upid);
    const res = await env.call("proxmox_sync_realm", { realm: "corp", dry_run: true, scope: "both", remove_vanished: "acl;entry" });
    assert.equal(res.status, "ok");
    assert.equal(res.realm, "corp");
    assert.equal(res.dry_run, true);
    const w = env.mock.requestsTo("/access/domains/corp/sync", "POST")[0];
    assert.deepEqual(w.body, { "dry-run": "1", scope: "both", "remove-vanished": "acl;entry" });

    const failed = env.mock.upid("pve1", "auth-realm-sync", "corp", { status: "stopped", exitstatus: "bind failed", log: ["error: bind failed"] });
    env.mock.on("POST", /^\/access\/domains\/corp\/sync$/, () => failed);
    assert.match(errorText(await env.callRaw("proxmox_sync_realm", { realm: "corp" })), /bind failed/);

    const started = await env.call("proxmox_sync_realm", { realm: "corp", wait: false });
    assert.equal(started.status, "started");
  });

  // ---------------------------------------------------------------- TFA

  it("lists TFA entries without secrets", async () => {
    env.mock.on("GET", /^\/access\/tfa$/, () => [
      { userid: "root@pam", entries: [{ id: "r1", type: "recovery", description: "", created: 1700000000 }] },
      { userid: "alice@pve", "totp-locked": 1, "tfa-locked-until": FUTURE, entries: [{ id: "t1", type: "totp", description: "phone", created: 1700000000, enable: 0 }] },
    ]);
    env.mock.on("GET", /^\/access\/tfa\/alice%40pve$/, () => [{ id: "t1", type: "totp", description: "phone", created: 1700000000, enable: 1 }]);
    const all = await env.call("proxmox_list_tfa");
    assert.equal(all[0].userid, "alice@pve");
    assert.equal(all[0].totp_locked, true);
    assert.ok(all[0].tfa_locked_until);
    assert.equal(all[0].entries[0].enabled, false);
    assert.deepEqual(all[1].entries[0], { id: "r1", type: "recovery", created: new Date(1700000000 * 1000).toISOString(), enabled: true });

    const one = await env.call("proxmox_list_tfa", { userid: "alice@pve" });
    assert.equal(one.userid, "alice@pve");
    assert.equal(one.entries[0].description, "phone");
  });

  it("deletes TFA entries and unlocks TFA", async () => {
    await env.call("proxmox_delete_tfa", { userid: "alice@pve", id: "t1" });
    let w = env.mock.lastWrite()!;
    assert.equal(w.method, "DELETE");
    assert.equal(w.path, "/access/tfa/alice%40pve/t1");

    env.mock.on("PUT", /^\/access\/users\/alice%40pve\/unlock-tfa$/, () => 1);
    const r = await env.call("proxmox_unlock_tfa", { userid: "alice@pve" });
    w = env.mock.lastWrite()!;
    assert.equal(w.path, "/access/users/alice%40pve/unlock-tfa");
    assert.deepEqual(r, { userid: "alice@pve", was_locked: true, unlocked: true });
  });

  // ---------------------------------------------------------------- realm sync jobs

  it("lists, saves and deletes realm sync jobs", async () => {
    env.mock.on("GET", /^\/cluster\/jobs\/realm-sync$/, () => [
      { id: "nightly", realm: "corp", enabled: 1, schedule: "daily", scope: "both", "last-run": 1700000000, "next-run": 1700086400 },
    ]);
    env.mock.on("GET", /^\/cluster\/jobs\/realm-sync\/nightly$/, () => ({ id: "nightly", realm: "corp", schedule: "daily" }));
    const jobs = await env.call("proxmox_list_realm_sync_jobs");
    assert.deepEqual(jobs[0], {
      id: "nightly",
      realm: "corp",
      enabled: true,
      schedule: "daily",
      scope: "both",
      last_run: new Date(1700000000 * 1000).toISOString(),
      next_run: new Date(1700086400 * 1000).toISOString(),
    });

    await env.call("proxmox_save_realm_sync_job", { id: "nightly", create: true, realm: "corp", schedule: "daily", scope: "users", enable_new: false });
    let w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.equal(w.path, "/cluster/jobs/realm-sync/nightly");
    assert.deepEqual(w.body, { realm: "corp", schedule: "daily", scope: "users", "enable-new": "0" });

    // Update without schedule re-sends the current one (the API requires it).
    await env.call("proxmox_save_realm_sync_job", { id: "nightly", enabled: false, remove_vanished: "acl" });
    w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.deepEqual(w.body, { schedule: "daily", enabled: "0", "remove-vanished": "acl" });

    assert.match(errorText(await env.callRaw("proxmox_save_realm_sync_job", { id: "x", create: true })), /realm and schedule are required/);

    await env.call("proxmox_delete_realm_sync_job", { id: "nightly" });
    assert.equal(env.mock.lastWrite()!.method, "DELETE");
    assert.equal(env.mock.lastWrite()!.path, "/cluster/jobs/realm-sync/nightly");
  });

  it("converts expiry dates", () => {
    assert.equal(toEpoch(undefined), undefined);
    assert.equal(toEpoch(0), 0);
    assert.equal(toEpoch("1700000000"), 1700000000);
    assert.equal(toEpoch("2027-01-01"), Date.UTC(2027, 0, 1) / 1000);
    assert.throws(() => toEpoch("tomorrow-ish"), /Invalid date/);
  });
});
