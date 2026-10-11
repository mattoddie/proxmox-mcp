import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { redactEndpoint } from "../tools/notifications.js";
import { errorText, startEnv, type TestEnv } from "./helpers.js";

const b64 = (s: string) => Buffer.from(s).toString("base64");

const TARGETS = [
  { name: "mail-to-root", type: "sendmail", origin: "builtin", comment: "Send mails to root@pam's email address" },
  { name: "gotify1", type: "gotify", disable: 1 },
  { name: "smtp1", type: "smtp", origin: "user-created" },
  { name: "hook", type: "webhook" },
];

const SMTP1 = { name: "smtp1", server: "mail.example.com", "from-address": "pve@example.com", username: "pve", password: "smtppw", mode: "tls", mailto: ["ops@example.com"] };
const GOTIFY1 = { name: "gotify1", server: "https://gotify", token: "gotify-token", disable: 1 };
const HOOK = {
  name: "hook",
  url: "https://hooks.example.com/{{ secrets.path }}",
  method: "post",
  header: [`name=Content-Type,value=${b64("application/json")}`, `name=Authorization,value=${b64("Bearer xyz")}`],
  body: b64('{"text":"{{ message }}"}'),
  secret: [`name=path,value=${b64("s3cret")}`, "name=other"],
  digest: "d1",
};

describe("notifications tools", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv();
  });
  after(() => env.stop());

  beforeEach(() => {
    env.mock.reset();
    env.mock.on("GET", /^\/cluster\/notifications\/targets$/, () => structuredClone(TARGETS));
    env.mock.on("GET", /^\/cluster\/notifications\/endpoints\/smtp$/, () => [structuredClone(SMTP1)]);
    env.mock.on("GET", /^\/cluster\/notifications\/endpoints\/gotify$/, () => [structuredClone(GOTIFY1)]);
    env.mock.on("GET", /^\/cluster\/notifications\/endpoints\/webhook$/, () => [structuredClone(HOOK)]);
    env.mock.on("GET", /^\/cluster\/notifications\/endpoints\/sendmail$/, () => [{ name: "mail-to-root", "mailto-user": ["root@pam"], origin: "builtin" }]);
    env.mock.on("GET", /^\/cluster\/notifications\/endpoints\/smtp\/smtp1$/, () => structuredClone(SMTP1));
    env.mock.on("GET", /^\/cluster\/notifications\/endpoints\/gotify\/gotify1$/, () => structuredClone(GOTIFY1));
    env.mock.on("GET", /^\/cluster\/notifications\/endpoints\/webhook\/hook$/, () => structuredClone(HOOK));
  });

  it("lists targets, optionally with redacted details", async () => {
    const list = await env.call("proxmox_list_notification_targets");
    assert.deepEqual(list.map((t: any) => t.name), ["gotify1", "hook", "mail-to-root", "smtp1"]);
    assert.deepEqual(list[0], { name: "gotify1", type: "gotify", disabled: true });
    assert.equal(list[2].origin, "builtin");
    assert.equal(env.mock.requestsTo("/cluster/notifications/endpoints").length, 0);

    const detailed = await env.call("proxmox_list_notification_targets", { details: true });
    const text = JSON.stringify(detailed);
    for (const secret of ["smtppw", "gotify-token", "s3cret", "Bearer xyz"]) assert.ok(!text.includes(secret), `leaked ${secret}`);
    const smtp = detailed.find((t: any) => t.name === "smtp1");
    assert.equal(smtp.config.server, "mail.example.com");
    assert.equal(smtp.config.password, "<redacted>");
    assert.deepEqual(detailed.find((t: any) => t.name === "mail-to-root").config, { "mailto-user": ["root@pam"] });
  });

  it("gets endpoints, looking up the type and redacting secrets", async () => {
    const g = await env.call("proxmox_get_notification_endpoint", { name: "gotify1" });
    assert.equal(g.type, "gotify");
    assert.equal(g.token, "<redacted>");
    assert.equal(g.disabled, true);

    const h = await env.call("proxmox_get_notification_endpoint", { name: "hook", type: "webhook" });
    assert.equal(env.mock.requestsTo("/cluster/notifications/targets").length, 1); // only for the gotify lookup
    assert.deepEqual(h.header, { "Content-Type": "application/json", Authorization: "<redacted>" });
    assert.deepEqual(h.secret, { path: "<redacted>", other: "<redacted>" });
    assert.equal(h.body, '{"text":"{{ message }}"}');
    assert.equal(h.digest, "d1");

    const raw = await env.call("proxmox_get_notification_endpoint", { name: "hook", type: "webhook", raw: true });
    assert.ok(!JSON.stringify(raw).includes(b64("s3cret")));
    assert.ok(!JSON.stringify(raw).includes(b64("Bearer xyz")), "raw output leaked the Authorization header");
    assert.ok(raw.header.some((h: string) => h.startsWith("name=Content-Type,value=")));

    assert.match(errorText(await env.callRaw("proxmox_get_notification_endpoint", { name: "nope" })), /No notification target named "nope"/);
  });

  it("creates endpoints of each type with the right parameters", async () => {
    await env.call("proxmox_save_notification_endpoint", {
      type: "smtp",
      name: "smtp2",
      create: true,
      server: "mail",
      from_address: "pve@x",
      port: 587,
      mode: "starttls",
      username: "u",
      password: "pw",
      mailto: ["a@x", "b@x"],
      mailto_user: ["root@pam"],
    });
    let w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.equal(w.path, "/cluster/notifications/endpoints/smtp");
    assert.deepEqual(w.body, {
      name: "smtp2",
      mailto: ["a@x", "b@x"],
      "mailto-user": "root@pam",
      "from-address": "pve@x",
      server: "mail",
      port: "587",
      mode: "starttls",
      username: "u",
      password: "pw",
    });

    await env.call("proxmox_save_notification_endpoint", { type: "gotify", name: "g2", create: true, server: "https://g", token: "t" });
    assert.deepEqual(env.mock.lastWrite()!.body, { name: "g2", server: "https://g", token: "t" });

    await env.call("proxmox_save_notification_endpoint", { type: "sendmail", name: "m2", create: true, mailto_user: ["root@pam"], author: "PVE" });
    assert.deepEqual(env.mock.lastWrite()!.body, { name: "m2", "mailto-user": "root@pam", author: "PVE" });

    const res = await env.call("proxmox_save_notification_endpoint", {
      type: "webhook",
      name: "hook",
      create: true,
      url: "https://hooks/{{ secrets.path }}",
      method: "post",
      headers: { "Content-Type": "application/json" },
      body: "{{ message }}",
      secrets: { path: "s3cret" },
    });
    w = env.mock.lastWrite()!;
    assert.equal(w.path, "/cluster/notifications/endpoints/webhook");
    assert.equal(w.body.header, `name=Content-Type,value=${b64("application/json")}`);
    assert.equal(w.body.body, b64("{{ message }}"));
    assert.equal(w.body.secret, `name=path,value=${b64("s3cret")}`);
    assert.equal(res.created, "hook");
    assert.ok(!JSON.stringify(res).includes("s3cret"));
  });

  it("updates endpoints and validates fields per type", async () => {
    const res = await env.call("proxmox_save_notification_endpoint", { type: "smtp", name: "smtp1", password: "new", delete: ["author", "comment"], digest: "abc" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, "/cluster/notifications/endpoints/smtp/smtp1");
    assert.deepEqual(w.body, { password: "new", delete: ["author", "comment"], digest: "abc" });
    assert.equal(res.endpoint.password, "<redacted>");

    const n = env.mock.requests.length;
    assert.match(errorText(await env.callRaw("proxmox_save_notification_endpoint", { type: "gotify", name: "g", url: "x", mailto: ["a"] })), /Not valid for gotify endpoints: mailto, url/);
    assert.match(errorText(await env.callRaw("proxmox_save_notification_endpoint", { type: "webhook", name: "w", create: true, url: "x" })), /Required to create a webhook endpoint: method/);
    assert.match(errorText(await env.callRaw("proxmox_save_notification_endpoint", { type: "sendmail", name: "m", create: true })), /mailto or mailto_user/);
    assert.match(errorText(await env.callRaw("proxmox_save_notification_endpoint", { type: "smtp", name: "s", create: true, server: "x" })), /from_address/);
    assert.equal(env.mock.requests.length, n);
  });

  it("deletes endpoints", async () => {
    const res = await env.call("proxmox_delete_notification_endpoint", { name: "smtp1" });
    assert.deepEqual(res, { deleted: "smtp1", type: "smtp" });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "DELETE");
    assert.equal(w.path, "/cluster/notifications/endpoints/smtp/smtp1");
  });

  it("sends a test notification", async () => {
    assert.deepEqual(await env.call("proxmox_test_notification_target", { name: "smtp1" }), { tested: "smtp1", sent: true });
    const w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.equal(w.path, "/cluster/notifications/targets/smtp1/test");

    env.mock.on("POST", /test$/, () => env.mock.error(500, "could not send mail"));
    assert.match(errorText(await env.callRaw("proxmox_test_notification_target", { name: "smtp1" })), /could not send mail/);
  });

  it("lists and gets matchers", async () => {
    env.mock.on("GET", /^\/cluster\/notifications\/matchers$/, () => [
      { name: "default-matcher", origin: "builtin", target: ["mail-to-root"], comment: "Route all to root" },
      { name: "backups", "match-field": ["exact:type=vzdump"], "match-severity": ["error"], mode: "any", "invert-match": 1, disable: 1, target: ["gotify1"] },
    ]);
    env.mock.on("GET", /^\/cluster\/notifications\/matchers\/backups$/, () => ({ "match-field": ["exact:type=vzdump"], target: ["gotify1"], digest: "dd" }));
    const list = await env.call("proxmox_list_notification_matchers");
    assert.deepEqual(list[0], {
      name: "backups",
      disabled: true,
      mode: "any",
      invert_match: true,
      match_severity: ["error"],
      match_field: ["exact:type=vzdump"],
      targets: ["gotify1"],
    });
    assert.equal(list[1].mode, "all");
    const m = await env.call("proxmox_get_notification_matcher", { name: "backups" });
    assert.deepEqual(m, { name: "backups", mode: "all", match_field: ["exact:type=vzdump"], targets: ["gotify1"], digest: "dd" });
  });

  it("saves and deletes matchers", async () => {
    env.mock.on("GET", /^\/cluster\/notifications\/matchers\/m1$/, () => ({ target: ["smtp1"], "match-severity": ["warning", "error"] }));
    const res = await env.call("proxmox_save_notification_matcher", {
      name: "m1",
      create: true,
      targets: ["smtp1"],
      match_severity: ["warning", "error"],
      match_field: ["regex:hostname=^pve1$"],
      match_calendar: ["mon..fri 9-17"],
      mode: "all",
      invert_match: false,
    });
    let w = env.mock.lastWrite()!;
    assert.equal(w.method, "POST");
    assert.equal(w.path, "/cluster/notifications/matchers");
    assert.deepEqual(w.body, {
      name: "m1",
      target: "smtp1",
      "match-severity": ["warning", "error"],
      "match-field": "regex:hostname=^pve1$",
      "match-calendar": "mon..fri 9-17",
      mode: "all",
      "invert-match": "0",
    });
    assert.deepEqual(res.matcher.targets, ["smtp1"]);

    await env.call("proxmox_save_notification_matcher", { name: "m1", disable: true, delete: ["match-calendar"] });
    w = env.mock.lastWrite()!;
    assert.equal(w.method, "PUT");
    assert.equal(w.path, "/cluster/notifications/matchers/m1");
    assert.deepEqual(w.body, { disable: "1", delete: "match-calendar" });

    await env.call("proxmox_delete_notification_matcher", { name: "m1" });
    assert.equal(env.mock.lastWrite()!.method, "DELETE");
    assert.equal(env.mock.lastWrite()!.path, "/cluster/notifications/matchers/m1");
  });

  it("lists matcher fields and known values", async () => {
    env.mock.on("GET", /^\/cluster\/notifications\/matcher-fields$/, () => [{ name: "type" }, { name: "hostname" }, { name: "job-id" }]);
    env.mock.on("GET", /^\/cluster\/notifications\/matcher-field-values$/, () => [
      { field: "type", value: "vzdump" },
      { field: "type", value: "replication" },
      { field: "hostname", value: "pve1" },
      { field: "job-id", value: "backup-1", comment: "nightly" },
    ]);
    const res = await env.call("proxmox_get_notification_matcher_fields");
    assert.deepEqual(res.fields, [
      { field: "hostname", values: ["pve1"] },
      { field: "job-id", values: [{ value: "backup-1", comment: "nightly" }] },
      { field: "type", values: ["vzdump", "replication"] },
    ]);
  });

  it("redacts endpoint secrets", () => {
    const r = redactEndpoint({ name: "x", token: "t", password: "p", header: ["name=X-Api-Key,value=" + b64("k"), "name=Accept,value=" + b64("*/*")] });
    assert.equal(r.token, "<redacted>");
    assert.equal(r.password, "<redacted>");
    assert.deepEqual(r.header, { "X-Api-Key": "<redacted>", Accept: "*/*" });
  });
});
