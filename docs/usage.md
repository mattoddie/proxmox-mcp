# Using proxmox-mcp

Once the server is connected, just talk to your assistant about your cluster. This page shows what works well and explains how the tools behave, so you know what to expect.

- [Example questions](#example-questions)
- [Built-in prompts](#built-in-prompts)
- [Guests, VMIDs and nodes](#guests-vmids-and-nodes)
- [Tasks and `wait`](#tasks-and-wait)
- [Making changes](#making-changes)
- [Running commands in guests](#running-commands-in-guests)
- [Summaries, raw objects and secrets](#summaries-raw-objects-and-secrets)
- [The `extra` argument](#the-extra-argument)
- [The raw API tool](#the-raw-api-tool)
- [Tips for good results](#tips-for-good-results)

## Example questions

**Health and capacity**

- *"How's the cluster? Anything I should worry about?"*
- *"Which node has the most free memory, and how overcommitted is each node on vCPUs?"*
- *"Show the CPU and IO wait on pve2 over the last week."* (`proxmox_get_node_stats`)
- *"Is any storage more than 80% full?"*
- *"Do any disks have SMART errors or high SSD wear?"* (`proxmox_list_disks`)

**VMs and containers**

- *"List running VMs sorted by memory use."*
- *"Why won't VM 101 start?"* The assistant checks its config, lock, recent tasks and storage.
- *"What's the IP address of the web VM?"* (`proxmox_get_guest_network`, needs the guest agent)
- *"Show me the network config of container 200."*
- *"Shut down VM 105 and start it again once it's off."* (writes)
- *"Give the db VM 8 GB of memory and 4 cores."* (writes)

**Backups and snapshots**

- *"Which guests aren't covered by any backup job?"* (`proxmox_list_unbacked_guests`)
- *"Did last night's backups succeed? Show me the errors if not."*
- *"Find snapshots older than 30 days."* (`proxmox_list_all_snapshots`)
- *"Snapshot VM 100 as 'pre-upgrade' before I patch it."* (writes)

**Nodes and maintenance**

- *"Which nodes have pending updates, and do any need a reboot?"*
- *"When do the node certificates expire?"*
- *"Are all nodes on the same Proxmox version and kernel?"*
- *"Migrate everything off pve2 so I can reboot it."* (writes)

**Security and access**

- *"Which users have Administrator on /, and which API tokens have no expiry?"*
- *"Who doesn't have two-factor authentication?"*
- *"Is the datacenter firewall on? Show the cluster-level rules."*

**Tasks**

- *"What failed in the last 24 hours?"*
- *"Show me the log of the last backup task for VM 100."*

## Built-in prompts

The server includes eight prompts. Each one is a ready-made, multi-step request that tells the model which tools to use and how to report. Most clients show them as slash commands or in a prompt picker. In Claude Code they're `/mcp__proxmox__<name>`, for example `/mcp__proxmox__cluster_health_check`.

| Prompt | Arguments | What it does |
|---|---|---|
| `cluster_health_check` | | Checks quorum, nodes, storage, failed tasks, HA, Ceph, backups and disk health, then reports problems ordered by severity. |
| `troubleshoot_guest` | `guest`, `symptom` (optional) | Finds a VM or container by VMID or name and checks its state, tasks, resources, network and storage to work out what's wrong. |
| `capacity_planning` | `timeframe` (optional: day, week, month, year) | Analyses CPU, memory and storage headroom per node and suggests rebalancing or right-sizing. |
| `backup_audit` | | Checks that every guest is backed up, backups are recent and succeeding, and backup storage has room. |
| `security_review` | | Reviews users, tokens, 2FA, ACLs, firewall, certificates, updates and realms, and reports findings as High, Medium or Low. |
| `update_review` | | Lists pending updates on each node and proposes a rolling upgrade order, including which guests to migrate first. |
| `snapshot_cleanup` | `older_than_days` (optional, default 14) | Finds old snapshots across all guests and proposes which to delete. |
| `deploy_vm` | `name`, `spec` (optional) | Plans a new VM (template or ISO, node, storage, bridge, VMID), waits for your confirmation, then creates and starts it. |

All of them except `deploy_vm` are read-only: they tell the model not to change anything, or to ask first. `deploy_vm` only creates the VM after you confirm the plan, and needs writes enabled to do so.

## Guests, VMIDs and nodes

Proxmox identifies every VM and container by its numeric **VMID**, which is unique across the cluster. proxmox-mcp uses it everywhere:

- **Guest tools take a `vmid` and find the node themselves.** They look the guest up in the cluster's resource list, so you never have to say which node VM 100 is on. You can pass `node` to skip the lookup.
- **VM tools and container tools are separate** (`proxmox_get_vm`, `proxmox_get_container` and so on), because Proxmox treats them differently. If the assistant uses the wrong kind, the error says which tools to use instead.
- **Names work in conversation.** Say *"the web VM"* and the assistant finds its VMID with `proxmox_list_guests` first.
- **Node-scoped tools** (node status, services, disks, storage content and so on) take an optional `node`. Without it they use `PROXMOX_DEFAULT_NODE`, or the only node on a single-node install. In a cluster without a default they fail with a message listing the node names, and the assistant asks or picks one.

## Tasks and `wait`

Anything slow in Proxmox runs as a **task**: starting and stopping guests, cloning, migrating, creating, backing up, restoring, resizing disks and more. Each task has an ID called a **UPID**, such as `UPID:pve1:000A1B2C:0123ABCD:6701F3A2:qmstart:100:root@pam:`, and a log.

Tools that start a task have a `wait` argument:

- **`wait: true`** (the default): the tool waits for the task to finish, for up to `PROXMOX_TASK_TIMEOUT_MS` (two minutes by default). It returns `status: "ok"` with the task's duration and the end of its log. If the task fails, the tool returns an error with the task's exit status and the last lines of its log, so the model can see why.
- **If the timeout passes first**, the tool doesn't fail. It returns `status: "running"` with the UPID, and the task carries on in Proxmox.
- **`wait: false`**: the tool returns the UPID straight away with `status: "started"`.

To follow up on a task, the assistant uses the `tasks` toolset: `proxmox_get_task` for its status and log tail, `proxmox_wait_for_task` to wait longer, `proxmox_get_task_log` for the full log, `proxmox_list_tasks` for history (filter by node, VMID, type, user or `errors: true`), and `proxmox_stop_task` to abort one (writes).

> [!TIP]
> For long jobs like big clones, migrations or backups, either raise `PROXMOX_TASK_TIMEOUT_MS` or ask the assistant to *"start it and check back"*. Keep `tasks` in `PROXMOX_TOOLSETS` whenever writes are on.

## Making changes

With `PROXMOX_ALLOW_WRITES=true` the assistant can change things. Good clients ask before running tools marked *destructive*, such as power actions, migrations, rollbacks and configuration changes. The server also tells the model to confirm with you before stopping, rebooting, migrating, restoring, reconfiguring or deleting. It's still a good habit to ask for a plan first: *"What would you change? Don't do it yet."*

### Updating configuration

`update_*` tools, such as `proxmox_update_vm_config`, change **only the fields you pass**. Everything else stays as it is. Some arguments help with the details:

- **`delete`** removes configuration keys or resets them to their defaults, for example `delete: ["net1", "description"]`. It's sent as Proxmox's own `delete` parameter and doesn't delete the guest. Removing a disk this way leaves it as an `unusedN` volume rather than destroying it.
- **`digest`** applies the change only if the configuration hasn't changed since you read it. The matching get tool returns the current digest. If someone else changed the guest in between, Proxmox rejects the update rather than overwriting their change.
- **Pending changes.** Some settings can't change on a running guest, such as most hardware on a VM. Proxmox stores them as pending until the next full stop and start. The result lists `pending_changes` so you know a restart is needed. A reboot from inside the guest isn't enough for VMs. It has to be stopped and started from Proxmox.

### Create or update: `save_*` tools

Named configuration objects, such as users, groups, roles, realms, metric servers, notification targets and SDN zones, are managed with one `proxmox_save_*` tool each:

- **With `create: true`** it **creates** a new object. Required fields are checked first.
- **Without it** it **updates** the existing object, changing **only the fields you pass**.

### Deleting

Deleting needs `PROXMOX_ALLOW_DELETES=true` as well. With it off, the delete tools don't exist and the raw API tool refuses `DELETE` requests. Deleting a guest also deletes its disks, so a recent backup matters. Proxmox refuses to delete guests with **Protection** turned on, whatever proxmox-mcp allows.

Some changes are hard to undo even without deletes: rolling back a snapshot discards everything since the snapshot, and restoring a backup over an existing guest replaces it. These tools are marked destructive so your client asks first.

## Running commands in guests

With `PROXMOX_ALLOW_EXEC=true` (and writes), the `agent` toolset gains tools that work **inside** VMs through the QEMU guest agent:

- `proxmox_guest_exec` runs a program with arguments, with no shell unless you ask for one (`["/bin/sh", "-c", "…"]`), and returns the exit code, stdout and stderr. It runs as the agent's user, which is root or SYSTEM. Commands that run longer than the timeout return a PID to check with `proxmox_get_guest_exec_status`.
- `proxmox_guest_read_file` and `proxmox_guest_write_file` read and write any file in the guest.
- `proxmox_guest_set_password` sets a user's password in the guest.

Sending QEMU monitor commands to a VM also needs exec. Containers don't have a guest agent, so these tools only work with VMs that have the agent installed and enabled.

Exec effectively gives the assistant root on every VM the token can reach. Read [Security](security.md) before turning it on. Without exec, the read-only agent tools, such as the guest's OS, IP addresses, filesystems and logged-in users, still work.

## Summaries, raw objects and secrets

- **Compact by default.** List and get tools return trimmed summaries with the useful fields, readable units (GiB, percentages, ISO timestamps, uptimes like `3d 4h 12m`) and parsed property strings. For example, a VM's `scsi0` becomes an object with `file`, `size` and options instead of one long string. Filters like `search`, `type`, `node`, `status` and `limit` keep responses small.
- **`raw: true`** on many list and get tools returns the API's full objects when you need a field the summary leaves out.
- **Secrets are redacted.** Values of fields that look like passwords, secrets, keys or tickets, such as `cipassword`, realm bind passwords and notification target secrets, show as `<redacted>`. This applies to raw objects too.

There are deliberate exceptions. `proxmox_create_api_token` returns the new token's secret, because Proxmox only shows it once. The raw API tool returns unredacted data if called with `redact: false`. And `proxmox_guest_read_file` returns whatever the file contains.

## The `extra` argument

Tools have typed arguments for the common settings. The Proxmox API has many more options, and new versions add more. For anything not covered, `extra` accepts an object of raw API parameters that are added to the request:

> *"Set VM 100's CPU type to host. Use `extra` with `cpu: "host"`."*

`extra` can't override what the tool sets itself: a parameter that one of the tool's typed arguments maps to (even when that argument isn't given) is refused, as are a few reserved ones such as `archive`, `restore` and `force` on the create tools. That keeps the tool's checks honest, so for example `extra` can't point a restore at another VMID or turn a create into an overwrite. Use the typed argument instead.

Parameter names are Proxmox's own, as shown in the [API viewer](https://pve.proxmox.com/pve-docs/api-viewer/). To see the current values, ask the assistant to fetch the object with `raw: true` first.

## The raw API tool

`proxmox_api_request` (toolset `raw`) calls any path under `/api2/json`, such as `/nodes/pve1/qemu/100/firewall/options` or `/cluster/ha/status/current`. It's a fallback for things the other tools don't cover. It follows the same switches as everything else:

| Settings | Allowed methods |
|---|---|
| Default (read-only) | `GET` |
| `PROXMOX_ALLOW_WRITES=true` | `GET`, `POST`, `PUT` |
| `+ PROXMOX_ALLOW_DELETES=true` | All, including `DELETE` |

Without `PROXMOX_ALLOW_EXEC`, it also refuses to call endpoints that run commands, open consoles, send input or read or write files: every guest agent endpoint (including `exec`, `file-read`, `file-write` and `set-user-password`), the QEMU monitor, `sendkey`, console and shell proxies and websockets, migration tunnels and the node `execute` endpoint. Paths with control characters, `..` or `./` segments, or a query string are refused, and the checks run on the path exactly as it will be sent.

Responses are redacted unless the call passes `redact: false`, including the DNS API credentials in ACME plugin configs (only the key names are shown). Leave `raw` out of `PROXMOX_TOOLSETS` if you want the assistant limited to the curated tools.

## Tips for good results

- **Name guests by VMID when you can.** *"VM 101"* is unambiguous; *"the database"* might match several guests.
- **Be specific about scope.** *"Containers on pve2"* or *"the last week"* lead to tighter tool calls.
- **Ask for evidence.** *"…and show the numbers you based that on"* keeps answers grounded.
- **Plan, then apply.** For changes, ask for a plan, review it, then say *"go ahead"*. Take a snapshot or backup first for anything risky.
- **Fewer tools, better focus.** If the assistant picks the wrong tools, narrow `PROXMOX_TOOLSETS`.
- **Check compatibility.** Some features need a recent Proxmox VE version. If a tool returns `501` or *"not implemented"*, see [Compatibility](compatibility.md).
