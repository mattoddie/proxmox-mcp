import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { errorText, startEnv, type TestEnv } from "./helpers.js";
import { WithTotal } from "./mock-proxmox.js";

const JOBS = [
  { id: "100-0", guest: 100, jobnum: 0, target: "pve2", type: "local", schedule: "*/5", rate: 50, comment: "web" },
  { id: "200-0", guest: 200, jobnum: 0, target: "pve2", type: "local", disable: 1 },
  { id: "101-1", guest: 101, jobnum: 1, target: "pve1", type: "local", digest: "x" },
];

describe("replication tools", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv();
  });
  after(() => env.stop());
  beforeEach(() => {
    env.mock.reset();
    env.mock.on("GET", /^\/cluster\/replication$/, () => structuredClone(JOBS));
    env.mock.on("GET", /^\/nodes\/pve1\/replication$/, () => [
      { id: "100-0", guest: 100, last_sync: 1700000000, next_sync: 1700000300, duration: 3.456, fail_count: 0 },
      { id: "200-0", guest: 200, last_try: 1700000000, fail_count: 3, error: "no space left" },
    ]);
    env.mock.on("GET", /^\/nodes\/pve2\/replication$/, () => [{ id: "101-1", guest: 101, pid: 1234, last_sync: 1700000000 }]);
  });

  it("lists jobs with status from each source node", async () => {
    const res = await env.call("proxmox_list_replication_jobs");
    assert.equal(res.total, 3);
    assert.deepEqual(
      res.items.map((j: any) => j.id),
      ["100-0", "101-1", "200-0"],
    );
    const [web, db, dns] = res.items;
    assert.deepEqual(web, {
      id: "100-0",
      guest: 100,
      target: "pve2",
      schedule: "*/5",
      rate_mbps: 50,
      comment: "web",
      status: {
        source: "pve1",
        state: "ok",
        last_sync: "2023-11-14T22:13:20.000Z",
        next_sync: "2023-11-14T22:18:20.000Z",
        duration_s: 3.5,
      },
    });
    assert.equal(db.schedule, "*/15");
    assert.equal(db.status.source, "pve2");
    assert.equal(db.status.state, "running");
    assert.equal(dns.disabled, true);
    assert.equal(dns.status.state, "error");
    assert.equal(dns.status.fail_count, 3);
    assert.equal(dns.status.error, "no space left");
  });

  it("filters jobs by guest and target", async () => {
    const byGuest = await env.call("proxmox_list_replication_jobs", { guest: 101 });
    assert.deepEqual(byGuest.items.map((j: any) => j.id), ["101-1"]);
    const byTarget = await env.call("proxmox_list_replication_jobs", { target: "pve2" });
    assert.deepEqual(byTarget.items.map((j: any) => j.id), ["100-0", "200-0"]);
    const none = await env.call("proxmox_list_replication_jobs", { guest: 9000 });
    assert.deepEqual(none, { total: 0, items: [] });
  });

  it("gets a job with status from the guest's node", async () => {
    env.mock.on("GET", /^\/cluster\/replication\/101-1$/, () => structuredClone(JOBS[2]));
    env.mock.on("GET", /^\/nodes\/pve2\/replication\/101-1\/status$/, () => ({ id: "101-1", last_sync: 1700000000, fail_count: 1, error: "timeout" }));
    const res = await env.call("proxmox_get_replication_job", { id: "101-1" });
    assert.equal(res.digest, "x");
    assert.equal(res.status.source, "pve2");
    assert.equal(res.status.state, "error");
    assert.equal(res.status.error, "timeout");
  });

  it("still returns the config when the status lookup fails", async () => {
    env.mock.on("GET", /^\/cluster\/replication\/100-0$/, () => structuredClone(JOBS[0]));
    const res = await env.call("proxmox_get_replication_job", { id: "100-0" });
    assert.equal(res.target, "pve2");
    assert.match(res.status_error, /404/);
  });

  it("validates job IDs", async () => {
    const res = await env.callRaw("proxmox_get_replication_job", { id: "abc" });
    assert.equal(res.isError, true);
  });

  it("creates jobs with the next free job number", async () => {
    const res = await env.call("proxmox_save_replication_job", { guest: 101, target: "pve1", schedule: "*/30", rate: 10 });
    assert.deepEqual(res, { id: "101-2", action: "created", target: "pve1" });
    const w = env.mock.lastWrite()!;
    assert.deepEqual([w.method, w.path], ["POST", "/cluster/replication"]);
    assert.deepEqual(w.body, { id: "101-2", type: "local", target: "pve1", schedule: "*/30", rate: "10" });

    const first = await env.call("proxmox_save_replication_job", { guest: 9000, target: "pve2" });
    assert.equal(first.id, "9000-0");
    const explicit = await env.call("proxmox_save_replication_job", { id: "100-3", target: "pve2" });
    assert.equal(explicit.action, "created");
    assert.equal(env.mock.lastWrite()!.body.id, "100-3");
  });

  it("updates existing jobs", async () => {
    const res = await env.call("proxmox_save_replication_job", { id: "100-0", disable: true, delete: ["rate"], digest: "abc" });
    assert.equal(res.action, "updated");
    const w = env.mock.lastWrite()!;
    assert.deepEqual([w.method, w.path], ["PUT", "/cluster/replication/100-0"]);
    assert.deepEqual(w.body, { disable: "1", delete: "rate", digest: "abc" });
  });

  it("rejects invalid save requests", async () => {
    assert.match(errorText(await env.callRaw("proxmox_save_replication_job", { id: "100-0", target: "pve3" })), /can't be changed/);
    assert.match(errorText(await env.callRaw("proxmox_save_replication_job", { guest: 100 })), /Pass `target`/);
    assert.match(errorText(await env.callRaw("proxmox_save_replication_job", { target: "pve2" })), /Pass `guest`/);
    assert.match(errorText(await env.callRaw("proxmox_save_replication_job", { id: "100-9", guest: 101, target: "pve2" })), /doesn't belong/);
  });

  it("deletes jobs with keep/force", async () => {
    const res = await env.call("proxmox_delete_replication_job", { id: "100-0", keep: true });
    assert.equal(res.status, "marked for removal");
    let w = env.mock.lastWrite()!;
    assert.deepEqual([w.method, w.path, w.query], ["DELETE", "/cluster/replication/100-0", { keep: "1" }]);
    const forced = await env.call("proxmox_delete_replication_job", { id: "100-0", force: true });
    assert.equal(forced.status, "removed");
    w = env.mock.lastWrite()!;
    assert.deepEqual(w.query, { force: "1" });
  });

  it("schedules a job now on the source node", async () => {
    env.mock.on("POST", /schedule_now$/, () => "100-0");
    const res = await env.call("proxmox_run_replication_now", { id: "100-0" });
    assert.equal(res.node, "pve1");
    assert.equal(res.status, "scheduled");
    assert.equal(env.mock.lastWrite()!.path, "/nodes/pve1/replication/100-0/schedule_now");

    const upid = env.mock.upid("pve2", "replicate", "101-1");
    env.mock.on("POST", /schedule_now$/, () => upid);
    const task = await env.call("proxmox_run_replication_now", { id: "101-1", node: "pve2" });
    assert.equal(task.status, "ok");
  });

  it("reads the job log", async () => {
    env.mock.on("GET", /^\/nodes\/pve1\/replication\/100-0\/log$/, () =>
      new WithTotal([{ n: 6, t: "start replication job" }, { n: 7, t: "end replication job" }], 7),
    );
    const res = await env.call("proxmox_get_replication_log", { id: "100-0", start: 5, limit: 2 });
    assert.deepEqual(env.mock.requestsTo(/\/log$/)[0].query, { start: "5", limit: "2" });
    assert.deepEqual(res, { id: "100-0", node: "pve1", total_lines: 7, lines: ["start replication job", "end replication job"] });
    assert.match(errorText(await env.callRaw("proxmox_get_replication_log", { id: "555-0" })), /No VM or container with ID 555/);
  });
});
