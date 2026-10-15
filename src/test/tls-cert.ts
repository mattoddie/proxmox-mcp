import { generateKeyPairSync, sign, X509Certificate } from "node:crypto";

/**
 * Builds a throwaway self-signed certificate for 127.0.0.1/localhost in memory, so TLS tests need neither
 * openssl nor a private key checked into the repo. Minimal DER encoding of an X.509 v3 certificate.
 */

function der(tag: number, content: Buffer): Buffer {
  const len = content.length;
  const head =
    len < 0x80 ? Buffer.from([tag, len]) : len < 0x100 ? Buffer.from([tag, 0x81, len]) : Buffer.from([tag, 0x82, len >> 8, len & 0xff]);
  return Buffer.concat([head, content]);
}

const seq = (...items: Buffer[]) => der(0x30, Buffer.concat(items));

function oid(dotted: string): Buffer {
  const [a, b, ...rest] = dotted.split(".").map(Number);
  const bytes = [40 * a + b];
  for (const n of rest) {
    const chunk = [n & 0x7f];
    for (let v = n >> 7; v > 0; v >>= 7) chunk.unshift((v & 0x7f) | 0x80);
    bytes.push(...chunk);
  }
  return der(0x06, Buffer.from(bytes));
}

const ECDSA_SHA256 = seq(oid("1.2.840.10045.4.3.2"));

function name(cn: string): Buffer {
  return seq(der(0x31, seq(oid("2.5.4.3"), der(0x0c, Buffer.from(cn)))));
}

export interface TestCert {
  key: string;
  cert: string;
  /** SHA-256 fingerprint as "AA:BB:...". */
  fingerprint256: string;
}

export function selfSignedCert(cn = "proxmox-mcp test"): TestCert {
  const { privateKey, publicKey } = generateKeyPairSync("ec", { namedCurve: "prime256v1" });
  const san = seq(der(0x82, Buffer.from("localhost")), der(0x87, Buffer.from([127, 0, 0, 1])));
  const tbs = seq(
    der(0xa0, der(0x02, Buffer.from([2]))), // version v3
    der(0x02, Buffer.from([0x01, 0x23, 0x45, 0x67])), // serial
    ECDSA_SHA256,
    name(cn),
    seq(der(0x17, Buffer.from("250101000000Z")), der(0x17, Buffer.from("491231235959Z"))),
    name(cn),
    publicKey.export({ type: "spki", format: "der" }),
    der(0xa3, seq(seq(oid("2.5.29.17"), der(0x04, san)))),
  );
  const signature = sign("sha256", tbs, privateKey);
  const certDer = seq(tbs, ECDSA_SHA256, der(0x03, Buffer.concat([Buffer.from([0]), signature])));
  const b64 = certDer.toString("base64").match(/.{1,64}/g)!.join("\n");
  return {
    key: privateKey.export({ type: "pkcs8", format: "pem" }).toString(),
    cert: `-----BEGIN CERTIFICATE-----\n${b64}\n-----END CERTIFICATE-----\n`,
    fingerprint256: new X509Certificate(certDer).fingerprint256,
  };
}
