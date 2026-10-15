#!/usr/bin/env node
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { loadConfig } from "./config.js";
import { startHttpServer } from "./http.js";
import { ProxmoxClient } from "./proxmox/client.js";
import { createServer, SERVER_VERSION } from "./server.js";

async function main(): Promise<void> {
  const cfg = loadConfig();
  const client = new ProxmoxClient(cfg.proxmox);
  const { tools } = cfg;
  // Logs go to stderr: stdout is the MCP channel in stdio mode.
  console.error(
    `proxmox-mcp ${SERVER_VERSION}: Proxmox ${cfg.proxmox.url}, auth ${cfg.proxmox.tokenId ? `API token ${cfg.proxmox.tokenId}` : `password (${cfg.proxmox.username})`}, ` +
      `writes ${tools.allowWrites ? "ENABLED" : "disabled"}${tools.allowDeletes ? " (with deletes)" : ""}${tools.allowExec ? " (with exec)" : ""}, ` +
      `toolsets ${tools.toolsets.size}, transport ${cfg.transport}`,
  );
  const { verifySsl, caCert, tlsFingerprints, url } = cfg.proxmox;
  if (url.startsWith("https:") && !verifySsl && !caCert && !tlsFingerprints) {
    console.error(
      "WARNING: the Proxmox TLS certificate is not verified (PROXMOX_VERIFY_SSL=false), so the API token or password can be intercepted. " +
        "Pin the certificate with PROXMOX_TLS_FINGERPRINT (shown under Node > System > Certificates), set PROXMOX_CA_FILE, or use a trusted certificate with PROXMOX_VERIFY_SSL=true.",
    );
  }

  let shutdown: () => Promise<void>;
  if (cfg.transport === "http") {
    const httpServer = await startHttpServer(cfg, () => createServer(client, cfg.tools));
    shutdown = async () => {
      httpServer.close();
      await client.close();
    };
  } else {
    const server = createServer(client, cfg.tools);
    await server.connect(new StdioServerTransport());
    shutdown = async () => {
      await server.close();
      await client.close();
    };
  }

  for (const signal of ["SIGINT", "SIGTERM"] as const) {
    process.once(signal, () => {
      void shutdown().finally(() => process.exit(0));
    });
  }
}

main().catch((err) => {
  console.error(`proxmox-mcp: ${err instanceof Error ? err.message : err}`);
  process.exit(1);
});
