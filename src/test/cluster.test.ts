import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { startEnv, type TestEnv } from "./helpers.js";

describe("cluster tools: secret redaction", () => {
  let env: TestEnv;
  before(async () => {
    env = await startEnv();
  });
  after(() => env.stop());
  beforeEach(() => env.mock.reset());

  it("hides proxy credentials in datacenter options", async () => {
    env.mock.on("GET", /^\/cluster\/options$/, () => ({ keyboard: "en-gb", http_proxy: "http://bob:s3cret@proxy.lan:3128" }));
    const res = await env.call("proxmox_get_cluster_options");
    assert.equal(res.http_proxy, "http://<redacted>@proxy.lan:3128");
    assert.equal(res.keyboard, "en-gb");
    const plain = "http://proxy.lan:3128";
    env.mock.on("GET", /^\/cluster\/options$/, () => ({ http_proxy: plain }));
    assert.equal((await env.call("proxmox_get_cluster_options")).http_proxy, plain);
  });

  it("hides metric server tokens and OpenTelemetry headers", async () => {
    env.mock.on("GET", /^\/cluster\/metrics\/server$/, () => [
      { id: "influx", type: "influxdb", server: "10.0.0.5", port: 8086, token: "abc" },
      { id: "otel", type: "opentelemetry", server: "otel.lan", port: 4318, "otel-headers": "eyJBdXRob3JpemF0aW9uIjoiQmVhcmVyIHgifQ==" },
    ]);
    const res = await env.call("proxmox_list_metric_servers");
    assert.equal(res[0].token, "<redacted>");
    assert.equal(res[1]["otel-headers"], "<redacted>");
    assert.equal(res[1].server, "otel.lan");
  });
});
