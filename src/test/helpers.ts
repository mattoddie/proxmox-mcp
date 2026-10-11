import assert from "node:assert/strict";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { loadConfig, TOOLSETS, type ToolOptions } from "../config.js";
import { ProxmoxClient } from "../proxmox/client.js";
import { createServer } from "../server.js";
import { MockProxmox, TEST_TOKEN_ID, TEST_TOKEN_SECRET } from "./mock-proxmox.js";

/** Tool options with everything enabled up to the given level. */
export function toolOptions(level: "read" | "write" | "delete" | "exec" | "all" = "all"): ToolOptions {
  const allowWrites = level !== "read";
  return {
    allowWrites,
    allowDeletes: level === "delete" || level === "all",
    allowExec: level === "exec" || level === "all",
    toolsets: new Set(TOOLSETS),
  };
}

export async function connect(client: ProxmoxClient, options: ToolOptions = toolOptions()): Promise<Client> {
  const server = createServer(client, options);
  const [a, b] = InMemoryTransport.createLinkedPair();
  await server.connect(a);
  const mcp = new Client({ name: "test", version: "0" });
  await mcp.connect(b);
  return mcp;
}

/** Parse a successful tool result's JSON text, failing the test on isError. */
export function parse(result: any): any {
  assert.equal(result.isError, undefined, result.content?.[0]?.text);
  return JSON.parse(result.content[0].text);
}

/** Assert a tool call failed, and return its error text. */
export function errorText(result: any): string {
  assert.equal(result.isError, true, `expected an error, got: ${result.content?.[0]?.text}`);
  return result.content[0].text as string;
}

export interface TestEnv {
  mock: MockProxmox;
  client: ProxmoxClient;
  /** MCP client connected to a server with every tool enabled. */
  mcp: Client;
  /** Call a tool and parse its JSON result (fails on isError). */
  call: (name: string, args?: Record<string, unknown>) => Promise<any>;
  /** Call a tool and return the raw result (for error assertions). */
  callRaw: (name: string, args?: Record<string, unknown>) => Promise<any>;
  stop: () => Promise<void>;
}

/**
 * Start a token-auth mock Proxmox, a client pointed at it and an MCP client
 * with every tool enabled. Call stop() in after().
 */
export async function startEnv(env: Record<string, string> = {}): Promise<TestEnv> {
  const mock = new MockProxmox({ auth: "token" });
  await mock.start();
  const cfg = loadConfig({
    PROXMOX_URL: mock.url,
    PROXMOX_TOKEN_ID: TEST_TOKEN_ID,
    PROXMOX_TOKEN_SECRET: TEST_TOKEN_SECRET,
    PROXMOX_TASK_TIMEOUT_MS: "3000",
    ...env,
  });
  const client = new ProxmoxClient(cfg.proxmox);
  let mcp: Client;
  try {
    mcp = await connect(client, toolOptions("all"));
  } catch (err) {
    // Don't leave the mock listening, or the test file hangs instead of failing.
    await client.close();
    await mock.stop();
    throw err;
  }
  const callRaw = (name: string, args: Record<string, unknown> = {}) => mcp.callTool({ name, arguments: args });
  return {
    mock,
    client,
    mcp,
    callRaw,
    call: async (name, args) => parse(await callRaw(name, args)),
    stop: async () => {
      await mcp.close();
      await client.close();
      await mock.stop();
    },
  };
}
