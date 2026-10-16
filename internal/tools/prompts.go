package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mattoddie/proxmox-mcp/internal/js"
)

// PromptDef is a reusable MCP prompt that chains several tools together.
type PromptDef struct {
	Name        string
	Title       string
	Description string
	Args        []Prop
	Text        func(a *Obj) string
}

// Prompts are the server's prompts, in registration order.
var Prompts = []PromptDef{
	{
		Name:        "cluster_health_check",
		Title:       "Cluster health check",
		Description: "Overall health review of the Proxmox cluster: quorum, nodes, guests, storage, Ceph, HA, backups and recent failed tasks.",
		Text: func(*Obj) string {
			return `Do a health check of my Proxmox VE cluster.
1. proxmox_get_cluster_status: quorum, nodes online, guest counts, locked guests, storage usage.
2. proxmox_get_node_status for each node: load vs. cores, memory and swap pressure, root disk usage, uptime, kernel and PVE version differences between nodes.
3. proxmox_list_storage: any storage above 80% used, inactive or unavailable.
4. proxmox_list_tasks (errors: true): failed tasks in the last few days, grouped by type.
5. proxmox_get_ha_status if HA is in use, and proxmox_get_ceph_status if Ceph is in use (skip quietly if not configured).
6. proxmox_list_unbacked_guests and proxmox_list_backup_jobs: guests without backups, disabled jobs.
7. proxmox_list_disks on each node: SMART failures or high SSD wearout.
Report: a one-line verdict, then problems ordered by severity, each with evidence and a suggested fix. Keep healthy areas to one line each. Don't change anything.`
		},
	},
	{
		Name:        "troubleshoot_guest",
		Title:       "Troubleshoot a VM or container",
		Description: "Investigate why a VM or container is down, slow or unreachable.",
		Args: []Prop{
			P("guest", Str().D("VMID or name of the VM/container")),
			P("symptom", Str().Opt().D("What's wrong, e.g. 'won't start', 'slow', 'no network'")),
		},
		Text: func(a *Obj) string {
			symptom := ""
			if a.Truthy("symptom") {
				symptom = "; the symptom is: " + a.Str("symptom")
			}
			return fmt.Sprintf(`Troubleshoot the Proxmox guest "%s"%s.
1. Find it with proxmox_list_guests (search) to get its VMID, type and node.
2. Inspect it: proxmox_get_vm or proxmox_get_container (status, lock, config, pending changes, HA state).
3. Recent history: proxmox_list_tasks for its VMID (errors: true first), and proxmox_get_task for any failure.
4. Resources: proxmox_get_vm_stats / proxmox_get_container_stats (day) for CPU, memory, disk IO and network trends; proxmox_get_node_status for its node.
5. Network: for VMs with the guest agent, proxmox_get_guest_network; for containers, proxmox_get_container_interfaces. Check the bridge/VLAN in the config and proxmox_get_guest_firewall_status.
6. Storage: proxmox_list_storage for the storage its disks live on (full or unavailable?).
Report the most likely cause(s) with evidence and concrete next steps. Ask before making any change (starting, stopping, unlocking or reconfiguring).`, a.Str("guest"), symptom)
		},
	},
	{
		Name:        "capacity_planning",
		Title:       "Capacity planning",
		Description: "Analyse CPU, memory and storage headroom across nodes and suggest rebalancing or upgrades.",
		Args:        []Prop{P("timeframe", Enum("day", "week", "month", "year").Opt().D("Period to analyse (default week)"))},
		Text: func(a *Obj) string {
			tf := js.Str(or(a.Get("timeframe"), "week"))
			return fmt.Sprintf(`Do capacity planning for my Proxmox cluster over the last %s.
1. proxmox_get_cluster_status and proxmox_list_nodes: total and per-node CPU and memory.
2. proxmox_get_node_stats (timeframe: %s) for each node: average and peak CPU, memory, IO wait and load.
3. proxmox_list_guests (sort: memory, then sort: cpu): the heaviest guests and where they run; overcommit of vCPUs and memory per node.
4. proxmox_list_storage: usage and growth headroom per storage.
Report: headroom per node (CPU, memory, storage), overcommit ratios, hot spots, and concrete suggestions (which guests to migrate where, right-sizing over-provisioned guests, when storage will need expanding). Don't change anything.`, tf, tf)
		},
	},
	{
		Name:        "backup_audit",
		Title:       "Backup audit",
		Description: "Check that every guest is backed up, backups are recent and succeeding, and retention is sensible.",
		Text: func(*Obj) string {
			return `Audit the backups of my Proxmox cluster.
1. proxmox_list_backup_jobs: schedules, storage, mode, selection, retention; disabled jobs.
2. proxmox_list_unbacked_guests: guests not covered by any job.
3. proxmox_list_backups: for each guest, the newest backup and its age, size and verification state; guests whose newest backup is older than their schedule implies.
4. proxmox_list_tasks (type: vzdump, errors: true): recent failed backup runs and their errors (proxmox_get_task for details).
5. proxmox_list_storage: free space on the backup storages.
Report a table of guests with their last good backup, then gaps and failures ordered by risk, with fixes. Don't change anything.`
		},
	},
	{
		Name:        "security_review",
		Title:       "Security review",
		Description: "Review users, tokens, permissions, firewall, certificates and updates for security issues.",
		Text: func(*Obj) string {
			return `Review the security of my Proxmox cluster.
Check:
- proxmox_list_users and proxmox_list_api_tokens: accounts without expiry, disabled-but-present accounts, root@pam use, tokens without privilege separation, stale tokens.
- proxmox_list_tfa: admins without two-factor authentication.
- proxmox_list_acl: Administrator or broad roles granted on "/", permissions granted to individual users instead of groups.
- proxmox_get_firewall_options (scope: cluster) and proxmox_list_firewall_rules (scope: cluster): is the datacenter firewall enabled, default policies, overly broad accept rules; proxmox_get_firewall_options for each node.
- proxmox_get_node_certificates: self-signed or soon-expiring certificates.
- proxmox_list_node_updates and proxmox_list_node_repositories: pending security updates, enterprise/no-subscription repo mismatches.
- proxmox_list_realms: realms and their settings (e.g. LDAP without TLS).
Report findings as High / Medium / Low with the reason and a specific remediation. Don't change anything.`
		},
	},
	{
		Name:        "update_review",
		Title:       "Update review",
		Description: "List pending package updates on every node and propose a safe rolling upgrade order.",
		Text: func(*Obj) string {
			return `Review pending updates on my Proxmox cluster.
1. proxmox_list_nodes, then for each node proxmox_list_node_updates (refresh first with proxmox_refresh_node_updates only if I confirm) and proxmox_get_node_status for the running kernel and PVE version.
2. Flag kernel, pve-manager, qemu-server, ceph and corosync updates, and anything that needs a reboot.
3. proxmox_get_ha_status and proxmox_get_cluster_status to see what is HA-managed and which guests run on each node.
Propose a rolling upgrade plan: node order, which guests to migrate off each node first (and where), whether to set Ceph noout, and checks between nodes. Don't install or reboot anything.`
		},
	},
	{
		Name:        "snapshot_cleanup",
		Title:       "Snapshot cleanup",
		Description: "Find old or forgotten snapshots across all guests and propose which to delete.",
		Args:        []Prop{P("older_than_days", Str().Opt().D("Consider snapshots older than this many days (default 14)"))},
		Text: func(a *Obj) string {
			days := js.Num(a.Get("older_than_days"))
			if !js.Truthy(days) {
				days = 14
			}
			return fmt.Sprintf(`Find old snapshots on my Proxmox cluster.
1. proxmox_list_all_snapshots (older_than_days: %s): every snapshot older than that, with guest, age and description.
2. For guests with several snapshots, proxmox_list_snapshots to see the chain.
3. proxmox_list_storage: how full the storages holding those guests are.
Propose which snapshots look safe to delete (and which to keep, e.g. ones named or described as pre-upgrade baselines). Don't delete anything until I confirm each one.`, js.NumStr(days))
		},
	},
	{
		Name:        "deploy_vm",
		Title:       "Deploy a VM",
		Description: "Plan and create a new VM, from a template (cloud-init) or an ISO, choosing node, storage and network sensibly.",
		Args: []Prop{
			P("name", Str().D("Name for the new VM")),
			P("spec", Str().Opt().D("What it's for and how big, e.g. 'Debian 12 web server, 2 cores, 4 GB, 32 GB disk, VLAN 20'")),
		},
		Text: func(a *Obj) string {
			spec := ""
			if a.Truthy("spec") {
				spec = " (" + a.Str("spec") + ")"
			}
			return fmt.Sprintf(`Help me deploy a new VM called "%s"%s on my Proxmox cluster.
1. proxmox_list_guests (templates: only): suitable templates; otherwise proxmox_list_storage_content (content: iso) for installer ISOs.
2. proxmox_get_cluster_status and proxmox_list_storage: the node with the most headroom, and storage with room for the disk.
3. proxmox_list_node_network on that node: the right bridge (and VLAN tag) to use.
4. proxmox_get_next_vmid for an ID.
Then present the plan (node, VMID, template or ISO, CPU, memory, disk, network, cloud-init user/SSH key/IP) and wait for my confirmation. After I confirm, create it with proxmox_clone_vm (+ proxmox_update_vm_config for cloud-init and sizing, proxmox_resize_vm_disk if needed) or proxmox_create_vm, start it with proxmox_vm_power, and report its IP (proxmox_get_guest_network) once it's up.`, a.Str("name"), spec)
		},
	},
}

// PromptText renders a prompt with the given arguments, validating them like the original
// server (required arguments, enum values).
func PromptText(p PromptDef, args map[string]string) (string, error) {
	in := js.NewObj()
	for k, v := range args {
		in.Set(k, v)
	}
	a, err := ParseArgs(p.Args, in)
	if err != nil {
		return "", fmt.Errorf("Invalid arguments for prompt %s: %s", p.Name, err.Error())
	}
	return p.Text(a), nil
}

func registerPrompts(server *mcp.Server) {
	for _, p := range Prompts {
		var args []*mcp.PromptArgument
		for _, a := range p.Args {
			args = append(args, &mcp.PromptArgument{Name: a.Name, Description: a.Schema.desc, Required: !a.Schema.optional})
		}
		server.AddPrompt(&mcp.Prompt{Name: p.Name, Title: p.Title, Description: p.Description, Arguments: args},
			func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				text, err := PromptText(p, req.Params.Arguments)
				if err != nil {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
				}
				return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}}}, nil
			})
	}
}

// PromptArgDescription is an argument's description, for the docs generator.
func PromptArgDescription(a Prop) (string, bool) { return a.Schema.desc, !a.Schema.optional }
