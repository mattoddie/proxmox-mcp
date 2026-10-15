import assert from "node:assert/strict";
import { createServer, type Server } from "node:https";
import type { AddressInfo } from "node:net";
import { after, before, describe, it } from "node:test";
import { loadConfig, parseFingerprints } from "../config.js";
import { ProxmoxClient } from "../proxmox/client.js";
import { TEST_TOKEN_ID, TEST_TOKEN_SECRET } from "./mock-proxmox.js";
import { selfSignedCert } from "./tls-cert.js";

const TOKEN_ENV = { PROXMOX_TOKEN_ID: TEST_TOKEN_ID, PROXMOX_TOKEN_SECRET: TEST_TOKEN_SECRET };

describe("PROXMOX_TLS_FINGERPRINT parsing", () => {
  const colon = "AB:".repeat(31) + "CD";
  it("accepts colon-separated, plain hex and prefixed fingerprints, normalised to AA:BB form", () => {
    assert.deepEqual(parseFingerprints(colon.toLowerCase()), [colon]);
    assert.deepEqual(parseFingerprints(colon.replace(/:/g, "")), [colon]);
    assert.deepEqual(parseFingerprints(`sha256 Fingerprint=${colon}`), [colon]);
    assert.deepEqual(parseFingerprints(`${colon}, ${"12".repeat(32)}`), [colon, "12:".repeat(31) + "12"]);
  });

  it("rejects anything that isn't a SHA-256 fingerprint", () => {
    assert.throws(() => parseFingerprints("AB:CD"), /must be a SHA-256 fingerprint/);
    assert.throws(() => parseFingerprints("zz".repeat(32)), /must be a SHA-256 fingerprint/);
    assert.throws(
      () => loadConfig({ PROXMOX_URL: "http://pve:8006", ...TOKEN_ENV, PROXMOX_TLS_FINGERPRINT: colon }),
      /needs an https:\/\/ PROXMOX_URL/,
    );
    assert.deepEqual(loadConfig({ PROXMOX_URL: "https://pve:8006", ...TOKEN_ENV, PROXMOX_TLS_FINGERPRINT: colon }).proxmox.tlsFingerprints, [colon]);
  });
});

describe("TLS certificate pinning", () => {
  const tls = selfSignedCert();
  let server: Server;
  let url: string;
  const clients: ProxmoxClient[] = [];

  function client(env: Record<string, string>): ProxmoxClient {
    const c = new ProxmoxClient(loadConfig({ PROXMOX_URL: url, ...TOKEN_ENV, PROXMOX_TIMEOUT_MS: "3000", ...env }).proxmox);
    clients.push(c);
    return c;
  }

  before(async () => {
    server = createServer({ key: tls.key, cert: tls.cert }, (_req, res) => {
      res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify({ data: { version: "9.0.0" } }));
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    url = `https://127.0.0.1:${(server.address() as AddressInfo).port}`;
  });
  after(async () => {
    await Promise.all(clients.map((c) => c.close()));
    server.close();
  });

  it("connects when the certificate matches the pinned fingerprint", async () => {
    assert.deepEqual(await client({ PROXMOX_TLS_FINGERPRINT: tls.fingerprint256 }).get("/version"), { version: "9.0.0" });
    // Also as lower-case hex without colons, and alongside a second (e.g. renewed) certificate's fingerprint.
    const hex = tls.fingerprint256.replace(/:/g, "").toLowerCase();
    assert.deepEqual(await client({ PROXMOX_TLS_FINGERPRINT: `${"00".repeat(32)},${hex}` }).get("/version"), { version: "9.0.0" });
    // With PROXMOX_VERIFY_SSL=true the chain is checked as well as the pin, so a self-signed certificate is still refused.
    await assert.rejects(client({ PROXMOX_TLS_FINGERPRINT: hex, PROXMOX_VERIFY_SSL: "true" }).get("/version"), /Could not reach Proxmox/);
  });

  it("refuses a certificate that doesn't match, even with verification off", async () => {
    const wrong = "AB:".repeat(31) + "CD";
    await assert.rejects(
      client({ PROXMOX_TLS_FINGERPRINT: wrong }).get("/version"),
      (err: Error) => /fingerprint mismatch/.test(err.message) && err.message.includes(tls.fingerprint256),
    );
    // Not pinned, verification off: accepted (the startup warning covers this case).
    assert.deepEqual(await client({}).get("/version"), { version: "9.0.0" });
    // Not pinned, verification on: the self-signed certificate is refused with a hint about pinning.
    await assert.rejects(client({ PROXMOX_VERIFY_SSL: "true" }).get("/version"), /PROXMOX_TLS_FINGERPRINT/);
  });
});
