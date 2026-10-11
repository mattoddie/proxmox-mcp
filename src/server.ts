import { readFileSync } from "node:fs";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { ToolOptions } from "./config.js";
import { registerPrompts } from "./prompts.js";
import type { ProxmoxClient } from "./proxmox/client.js";
import { registerAccessTools } from "./tools/access.js";
import { registerAgentTools } from "./tools/agent.js";
import { registerBackupTools } from "./tools/backup.js";
import { registerCephTools } from "./tools/ceph.js";
import { registerClusterTools } from "./tools/cluster.js";
import { registerContainersTools } from "./tools/containers.js";
import { registerFirewallTools } from "./tools/firewall.js";
import { registerHaTools } from "./tools/ha.js";
import { registerHardwareTools } from "./tools/hardware.js";
import { registerNodesTools } from "./tools/nodes.js";
import { registerNotificationsTools } from "./tools/notifications.js";
import { registerPoolsTools } from "./tools/pools.js";
import { registerRawTools } from "./tools/raw.js";
import { registerReplicationTools } from "./tools/replication.js";
import { registerSdnTools } from "./tools/sdn.js";
import { registerSnapshotsTools } from "./tools/snapshots.js";
import { registerStorageTools } from "./tools/storage.js";
import { registerTasksTools } from "./tools/tasks.js";
import { registerVmsTools } from "./tools/vms.js";

export const SERVER_NAME = "proxmox-mcp";

function readVersion(): string {
  try {
    return JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8")).version;
  } catch {
    return "0.0.0-dev";
  }
}

/** The package version; release builds stamp it from the Git tag. */
export const SERVER_VERSION: string = readVersion();

export function createServer(client: ProxmoxClient, options: ToolOptions): McpServer {
  const server = new McpServer(
    { name: SERVER_NAME, version: SERVER_VERSION },
    {
      instructions:
        "Tools for monitoring and managing a Proxmox VE cluster: nodes, VMs (qemu), containers (lxc), storage, backups, snapshots, firewall, HA, SDN, Ceph and access control. " +
        "Start with proxmox_get_cluster_status, proxmox_list_guests or proxmox_list_nodes. " +
        "Guests are addressed by VMID; the node is looked up automatically when omitted. Node-scoped tools default to PROXMOX_DEFAULT_NODE or the only node. " +
        "Long-running operations are Proxmox tasks: by default tools wait for them and report the outcome with the end of the task log; pass wait=false to return the task ID (UPID) and follow it with proxmox_get_task. " +
        (options.allowWrites
          ? "Write tools are enabled: confirm with the user before stopping, rebooting, migrating, restoring or reconfiguring guests and nodes, and before any delete. "
          : "The server is read-only; write tools are disabled (PROXMOX_ALLOW_WRITES=false). ") +
        (options.allowExec ? "Command execution in guests and on nodes is enabled; show the user the exact command before running it." : ""),
    },
  );
  const ctx = { server, client, ...options };
  registerClusterTools(ctx);
  registerNodesTools(ctx);
  registerVmsTools(ctx);
  registerContainersTools(ctx);
  registerAgentTools(ctx);
  registerSnapshotsTools(ctx);
  registerStorageTools(ctx);
  registerBackupTools(ctx);
  registerTasksTools(ctx);
  registerAccessTools(ctx);
  registerFirewallTools(ctx);
  registerHaTools(ctx);
  registerSdnTools(ctx);
  registerCephTools(ctx);
  registerPoolsTools(ctx);
  registerReplicationTools(ctx);
  registerNotificationsTools(ctx);
  registerHardwareTools(ctx);
  registerRawTools(ctx);
  registerPrompts(server);
  return server;
}
