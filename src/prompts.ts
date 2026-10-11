import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { GetPromptResult } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";

function userPrompt(text: string): GetPromptResult {
  return { messages: [{ role: "user", content: { type: "text", text } }] };
}

/** Reusable MCP prompts that chain several tools together. */
export function registerPrompts(server: McpServer): void {
  server.registerPrompt(
    "cluster_health_check",
    {
      title: "Cluster health check",
      description: "Overall health review of the Proxmox cluster: quorum, nodes, guests, storage, Ceph, HA, backups and recent failed tasks.",
    },
    () =>
      userPrompt(
        `Do a health check of my Proxmox VE cluster.
1. proxmox_get_cluster_status: quorum, nodes online, guest counts, locked guests, storage usage.
2. proxmox_get_node_status for each node: load vs. cores, memory and swap pressure, root disk usage, uptime, kernel and PVE version differences between nodes.
3. proxmox_list_storage: any storage above 80% used, inactive or unavailable.
4. proxmox_list_tasks (errors: true): failed tasks in the last few days, grouped by type.
5. proxmox_get_ha_status if HA is in use, and proxmox_get_ceph_status if Ceph is in use (skip quietly if not configured).
6. proxmox_list_unbacked_guests and proxmox_list_backup_jobs: guests without backups, disabled jobs.
7. proxmox_list_disks on each node: SMART failures or high SSD wearout.
Report: a one-line verdict, then problems ordered by severity, each with evidence and a suggested fix. Keep healthy areas to one line each. Don't change anything.`,
      ),
  );

  server.registerPrompt(
    "troubleshoot_guest",
    {
      title: "Troubleshoot a VM or container",
      description: "Investigate why a VM or container is down, slow or unreachable.",
      argsSchema: {
        guest: z.string().describe("VMID or name of the VM/container"),
        symptom: z.string().optional().describe("What's wrong, e.g. 'won't start', 'slow', 'no network'"),
      },
    },
    ({ guest, symptom }) =>
      userPrompt(
        `Troubleshoot the Proxmox guest "${guest}"${symptom ? `; the symptom is: ${symptom}` : ""}.
1. Find it with proxmox_list_guests (search) to get its VMID, type and node.
2. Inspect it: proxmox_get_vm or proxmox_get_container (status, lock, config, pending changes, HA state).
3. Recent history: proxmox_list_tasks for its VMID (errors: true first), and proxmox_get_task for any failure.
4. Resources: proxmox_get_vm_stats / proxmox_get_container_stats (day) for CPU, memory, disk IO and network trends; proxmox_get_node_status for its node.
5. Network: for VMs with the guest agent, proxmox_get_guest_network; for containers, proxmox_get_container_interfaces. Check the bridge/VLAN in the config and proxmox_get_guest_firewall_status.
6. Storage: proxmox_list_storage for the storage its disks live on (full or unavailable?).
Report the most likely cause(s) with evidence and concrete next steps. Ask before making any change (starting, stopping, unlocking or reconfiguring).`,
      ),
  );

  server.registerPrompt(
    "capacity_planning",
    {
      title: "Capacity planning",
      description: "Analyse CPU, memory and storage headroom across nodes and suggest rebalancing or upgrades.",
      argsSchema: { timeframe: z.enum(["day", "week", "month", "year"]).optional().describe("Period to analyse (default week)") },
    },
    ({ timeframe }) =>
      userPrompt(
        `Do capacity planning for my Proxmox cluster over the last ${timeframe ?? "week"}.
1. proxmox_get_cluster_status and proxmox_list_nodes: total and per-node CPU and memory.
2. proxmox_get_node_stats (timeframe: ${timeframe ?? "week"}) for each node: average and peak CPU, memory, IO wait and load.
3. proxmox_list_guests (sort: memory, then sort: cpu): the heaviest guests and where they run; overcommit of vCPUs and memory per node.
4. proxmox_list_storage: usage and growth headroom per storage.
Report: headroom per node (CPU, memory, storage), overcommit ratios, hot spots, and concrete suggestions (which guests to migrate where, right-sizing over-provisioned guests, when storage will need expanding). Don't change anything.`,
      ),
  );

  server.registerPrompt(
    "backup_audit",
    {
      title: "Backup audit",
      description: "Check that every guest is backed up, backups are recent and succeeding, and retention is sensible.",
    },
    () =>
      userPrompt(
        `Audit the backups of my Proxmox cluster.
1. proxmox_list_backup_jobs: schedules, storage, mode, selection, retention; disabled jobs.
2. proxmox_list_unbacked_guests: guests not covered by any job.
3. proxmox_list_backups: for each guest, the newest backup and its age, size and verification state; guests whose newest backup is older than their schedule implies.
4. proxmox_list_tasks (type: vzdump, errors: true): recent failed backup runs and their errors (proxmox_get_task for details).
5. proxmox_list_storage: free space on the backup storages.
Report a table of guests with their last good backup, then gaps and failures ordered by risk, with fixes. Don't change anything.`,
      ),
  );

  server.registerPrompt(
    "security_review",
    {
      title: "Security review",
      description: "Review users, tokens, permissions, firewall, certificates and updates for security issues.",
    },
    () =>
      userPrompt(
        `Review the security of my Proxmox cluster.
Check:
- proxmox_list_users and proxmox_list_api_tokens: accounts without expiry, disabled-but-present accounts, root@pam use, tokens without privilege separation, stale tokens.
- proxmox_list_tfa: admins without two-factor authentication.
- proxmox_list_acl: Administrator or broad roles granted on "/", permissions granted to individual users instead of groups.
- proxmox_get_firewall_options (scope: cluster) and proxmox_list_firewall_rules (scope: cluster): is the datacenter firewall enabled, default policies, overly broad accept rules; proxmox_get_firewall_options for each node.
- proxmox_get_node_certificates: self-signed or soon-expiring certificates.
- proxmox_list_node_updates and proxmox_list_node_repositories: pending security updates, enterprise/no-subscription repo mismatches.
- proxmox_list_realms: realms and their settings (e.g. LDAP without TLS).
Report findings as High / Medium / Low with the reason and a specific remediation. Don't change anything.`,
      ),
  );

  server.registerPrompt(
    "update_review",
    {
      title: "Update review",
      description: "List pending package updates on every node and propose a safe rolling upgrade order.",
    },
    () =>
      userPrompt(
        `Review pending updates on my Proxmox cluster.
1. proxmox_list_nodes, then for each node proxmox_list_node_updates (refresh first with proxmox_refresh_node_updates only if I confirm) and proxmox_get_node_status for the running kernel and PVE version.
2. Flag kernel, pve-manager, qemu-server, ceph and corosync updates, and anything that needs a reboot.
3. proxmox_get_ha_status and proxmox_get_cluster_status to see what is HA-managed and which guests run on each node.
Propose a rolling upgrade plan: node order, which guests to migrate off each node first (and where), whether to set Ceph noout, and checks between nodes. Don't install or reboot anything.`,
      ),
  );

  server.registerPrompt(
    "snapshot_cleanup",
    {
      title: "Snapshot cleanup",
      description: "Find old or forgotten snapshots across all guests and propose which to delete.",
      argsSchema: { older_than_days: z.string().optional().describe("Consider snapshots older than this many days (default 14)") },
    },
    ({ older_than_days }) =>
      userPrompt(
        `Find old snapshots on my Proxmox cluster.
1. proxmox_list_all_snapshots (older_than_days: ${Number(older_than_days) || 14}): every snapshot older than that, with guest, age and description.
2. For guests with several snapshots, proxmox_list_snapshots to see the chain.
3. proxmox_list_storage: how full the storages holding those guests are.
Propose which snapshots look safe to delete (and which to keep, e.g. ones named or described as pre-upgrade baselines). Don't delete anything until I confirm each one.`,
      ),
  );

  server.registerPrompt(
    "deploy_vm",
    {
      title: "Deploy a VM",
      description: "Plan and create a new VM, from a template (cloud-init) or an ISO, choosing node, storage and network sensibly.",
      argsSchema: {
        name: z.string().describe("Name for the new VM"),
        spec: z.string().optional().describe("What it's for and how big, e.g. 'Debian 12 web server, 2 cores, 4 GB, 32 GB disk, VLAN 20'"),
      },
    },
    ({ name, spec }) =>
      userPrompt(
        `Help me deploy a new VM called "${name}"${spec ? ` (${spec})` : ""} on my Proxmox cluster.
1. proxmox_list_guests (templates: only): suitable templates; otherwise proxmox_list_storage_content (content: iso) for installer ISOs.
2. proxmox_get_cluster_status and proxmox_list_storage: the node with the most headroom, and storage with room for the disk.
3. proxmox_list_node_network on that node: the right bridge (and VLAN tag) to use.
4. proxmox_get_next_vmid for an ID.
Then present the plan (node, VMID, template or ISO, CPU, memory, disk, network, cloud-init user/SSH key/IP) and wait for my confirmation. After I confirm, create it with proxmox_clone_vm (+ proxmox_update_vm_config for cloud-init and sizing, proxmox_resize_vm_disk if needed) or proxmox_create_vm, start it with proxmox_vm_power, and report its IP (proxmox_get_guest_network) once it's up.`,
      ),
  );
}
