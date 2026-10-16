package tools

import (
	"context"
	"encoding/base64"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

// ---------------------------------------------------------------- arguments

var (
	backupModeArg = Enum("snapshot", "suspend", "stop").Opt().D("Backup mode: snapshot (live, default), suspend (brief pause) or stop (shuts the guest down for a consistent backup, then restarts it)")

	backupCompressArg = Enum("0", "1", "gzip", "lzo", "zstd").Opt().D("Compression: zstd (recommended), gzip, lzo, or 0 for none. Ignored for Proxmox Backup Server storages")

	backupRetentionArg = Object(
		P("keep_all", Bool().Opt().D("Keep all backups (disables the other options)")),
		P("keep_last", Int().Min(0).Opt().D("Keep the last N backups")),
		P("keep_hourly", Int().Min(0).Opt().D("Keep backups for the last N hours (one per hour)")),
		P("keep_daily", Int().Min(0).Opt().D("Keep backups for the last N days (one per day)")),
		P("keep_weekly", Int().Min(0).Opt().D("Keep backups for the last N weeks")),
		P("keep_monthly", Int().Min(0).Opt().D("Keep backups for the last N months")),
		P("keep_yearly", Int().Min(0).Opt().D("Keep backups for the last N years")),
	).Opt().D("Retention (prune-backups) settings, e.g. {keep_last: 3, keep_daily: 7, keep_weekly: 4}")

	backupNotesTemplateArg = Str().Opt().D("Notes for the backup(s); may use {{cluster}}, {{guestname}}, {{node}} and {{vmid}}, e.g. \"{{guestname}} before upgrade\"")

	backupVolidArg = Str().Min(3).D("Backup volume ID as listed by proxmox_list_backups, e.g. local:backup/vzdump-qemu-100-2025_01_01-02_00_00.vma.zst or pbs:backup/vm/100/2025-01-01T02:00:00Z")

	backupNodeArg = Str().Opt().D("Node to access the storage from. Needed for node-local storages present on several nodes (use the node shown by proxmox_list_backups); looked up automatically otherwise")
)

// backupRetentionString turns {keep_last: 3} into "keep-last=3" (undefined when empty).
func backupRetentionString(r any) any {
	o, ok := r.(*Obj)
	if !ok || o == nil {
		return js.Undef
	}
	var parts []string
	for _, k := range o.Keys() {
		v := o.Get(k)
		if js.IsUndef(v) {
			continue
		}
		if b, isBool := v.(bool); isBool {
			if b {
				v = 1.0
			} else {
				v = 0.0
			}
		}
		parts = append(parts, strings.ReplaceAll(k, "_", "-")+"="+js.Str(v))
	}
	if len(parts) == 0 {
		return js.Undef
	}
	return strings.Join(parts, ",")
}

// backupParseRetention normalises prune-backups, which arrives as a property string or (in job
// listings) an object, to {keep_last: 3}.
func backupParseRetention(value any) any {
	if js.Nullish(value) || value == "" {
		return js.Undef
	}
	var obj *Obj
	if s, ok := value.(string); ok {
		obj = proxmox.ParsePropertyString(s, "")
	} else if o, ok := value.(*Obj); ok {
		obj = o
	} else {
		obj = js.NewObj()
	}
	out := js.NewObj()
	for _, k := range obj.Keys() {
		v := obj.Get(k)
		if k == "keep-all" {
			out.Set(strings.ReplaceAll(k, "-", "_"), proxmox.Flag(v))
		} else {
			out.Set(strings.ReplaceAll(k, "-", "_"), js.Num(v))
		}
	}
	return out
}

// backupToParam turns job/defaults fields that may come back as objects into property strings.
func backupToParam(value any) any {
	if o, ok := value.(*Obj); ok && o != nil {
		return proxmox.ToPropertyString(o, "")
	}
	return value
}

var backupVmidSplitRe = regexp.MustCompile(`[,\s]+`)

func backupVmidList(value any) []any {
	out := []any{}
	for _, p := range backupVmidSplitRe.Split(js.Str(or(value, "")), -1) {
		if p == "" {
			continue
		}
		if n := js.StrToNum(p); js.IsFinite(n) {
			out = append(out, n)
		}
	}
	return out
}

// ---------------------------------------------------------------- summaries

func backupSummarizeJob(j *Obj) *Obj {
	var selection any
	switch {
	case j.Truthy("all"):
		selection = proxmox.C("all", true, "exclude", ifThen(j.Truthy("exclude"), backupVmidList(j.Get("exclude"))))
	case j.Truthy("pool"):
		selection = js.O("pool", j.Get("pool"))
	default:
		selection = js.O("vmids", backupVmidList(j.Get("vmid")))
	}
	enabled := any(true)
	if j.Defined("enabled") {
		enabled = proxmox.Flag(j.Get("enabled"))
	}
	schedule := j.Get("schedule")
	if js.Nullish(schedule) {
		schedule = ifThen(j.Truthy("starttime"), js.Str(or(j.Get("dow"), "daily"))+" "+js.Str(j.Get("starttime")))
	}
	removeOld := any(js.Undef)
	if j.Defined("remove") {
		removeOld = proxmox.Flag(j.Get("remove"))
	}
	return proxmox.C(
		"id", j.Get("id"),
		"enabled", enabled,
		"schedule", schedule,
		"next_run", proxmox.IsoTime(j.Get("next-run")),
		"storage", j.Get("storage"),
		"mode", or(j.Get("mode"), "snapshot"),
		"selection", selection,
		"node", j.Get("node"),
		"compress", j.Get("compress"),
		"retention", backupParseRetention(j.Get("prune-backups")),
		"remove_old", removeOld,
		"protected", orUndef(proxmox.Flag(j.Get("protected"))),
		"notes_template", j.Get("notes-template"),
		"comment", j.Get("comment"),
		"repeat_missed", orUndef(proxmox.Flag(j.Get("repeat-missed"))),
		"notification_mode", j.Get("notification-mode"),
		"mailto", j.Get("mailto"),
	)
}

var (
	backupQemuVolRe = regexp.MustCompile(`vzdump-qemu-|backup/vm/|\.vma(\.|$)`)
	backupLxcVolRe  = regexp.MustCompile(`vzdump-(lxc|openvz)-|backup/ct/`)
)

// backupGuestType is the guest type of a backup listing entry or volid: "qemu", "lxc" or "" if unknown.
func backupGuestType(b any) string {
	e, ok := b.(*Obj)
	if !ok {
		e = js.O("volid", b)
	}
	if st := e.Get("subtype"); st == "qemu" || st == "lxc" {
		return st.(string)
	}
	switch e.Get("format") {
	case "pbs-vm":
		return "qemu"
	case "pbs-ct":
		return "lxc"
	}
	v := js.Str(or(e.Get("volid"), ""))
	if backupQemuVolRe.MatchString(v) {
		return "qemu"
	}
	if backupLxcVolRe.MatchString(v) {
		return "lxc"
	}
	return ""
}

// backupTypeName maps qemu/lxc to vm/container (undefined otherwise).
func backupTypeName(t string) any {
	switch t {
	case "qemu":
		return "vm"
	case "lxc":
		return "container"
	}
	return js.Undef
}

func backupSummarizeBackup(b *Obj) any {
	size := or(b.Get("size"), b.Get("approximate-size"))
	return proxmox.C(
		"volid", b.Get("volid"),
		"storage", b.Get("_storage"),
		"node", b.Get("_node"),
		"vmid", ifThen(b.Defined("vmid"), b.Num("vmid")),
		"type", backupTypeName(backupGuestType(b)),
		"time", proxmox.IsoTime(b.Get("ctime")),
		"size", proxmox.FormatBytes(size),
		"size_bytes", ifThen(!js.IsUndef(size), js.Num(size)),
		"format", b.Get("format"),
		"notes", b.Get("notes"),
		"protected", orUndef(proxmox.Flag(b.Get("protected"))),
		"encrypted", ifThen(b.Truthy("encrypted"), true),
		"verification", b.ObjAt("verification").Get("state"),
	)
}

// backupStorageOf is the storage ID of a volid: the part before the first colon.
func backupStorageOf(volid string) (string, error) {
	i := strings.Index(volid, ":")
	if i <= 0 {
		return "", proxmox.Errorf("Not a volume ID: %s (expected storage:path, see proxmox_list_backups)", volid)
	}
	return volid[:i], nil
}

// backupNodeForStorage picks a node that can reach storage: the given node, else any online
// node for shared storage, else the single node that has it, else the default node.
func backupNodeForStorage(ctx context.Context, client *proxmox.Client, storage, node string) (string, error) {
	if node != "" {
		return node, nil
	}
	all, err := client.GetList(ctx, "/cluster/resources", js.O("type", "storage"))
	if err != nil {
		return "", err
	}
	entries := filterObjs(all, func(s *Obj) bool { return s.Get("storage") == storage && s.Get("status") != "unknown" })
	if len(entries) == 0 {
		return "", proxmox.Errorf("Storage %s was not found on any online node", storage)
	}
	def := client.DefaultNode()
	hasDefault := def != "" && len(filterObjs(entries, func(s *Obj) bool { return s.Get("node") == def })) > 0
	shared := len(filterObjs(entries, func(s *Obj) bool { return js.Truthy(proxmox.Flag(s.Get("shared"))) })) > 0
	if shared || len(entries) == 1 {
		if hasDefault {
			return def, nil
		}
		return js.Str(entries[0].Get("node")), nil
	}
	if hasDefault {
		return def, nil
	}
	names := make([]string, len(entries))
	for i, s := range entries {
		names[i] = js.Str(s.Get("node"))
	}
	sort.Strings(names)
	return "", proxmox.Errorf("Storage %s is local to several nodes (%s); pass `node` (the node shown by proxmox_list_backups)", storage, strings.Join(names, ", "))
}

var backupSectionRe = regexp.MustCompile(`^\[([^\]]+)\]\s*$`)

// backupParseConfigText splits an `extractconfig` text into the main config and snapshot section names.
func backupParseConfigText(text string) (*Obj, []any) {
	config := js.NewObj()
	snapshots := []any{}
	inMain := true
	for _, line := range strings.Split(text, "\n") {
		if section := backupSectionRe.FindStringSubmatch(line); section != nil {
			inMain = false
			// [PENDING] and [special:cloudinit] are not snapshots.
			if section[1] != "PENDING" && !strings.HasPrefix(section[1], "special:") {
				snapshots = append(snapshots, section[1])
			}
			continue
		}
		if !inMain || strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			// vzdump adds "#qmdump#map:..." disk mapping lines to VMA configs; they aren't part of the notes.
			if strings.HasPrefix(line, "#qmdump#") {
				continue
			}
			text := line[1:]
			if decoded, err := decodeURIComponent(text); err == nil {
				text = decoded
			}
			if config.Defined("description") {
				config.Set("description", js.Str(config.Get("description"))+"\n"+text)
			} else {
				config.Set("description", text)
			}
			continue
		}
		if idx := strings.Index(line, ":"); idx > 0 {
			config.Set(strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:]))
		}
	}
	return config, snapshots
}

// backupCheckHookParams: vzdump's `script` and a guest's `hookscript` run as root on the node,
// so they need exec permission too.
func backupCheckHookParams(c *Context, extra any) error {
	x, _ := extra.(*Obj)
	for _, k := range []string{"script", "hookscript"} {
		if x.Defined(k) {
			if !c.AllowExec {
				return proxmox.Errorf("%s runs a script as root on the node, so setting it needs exec permission (PROXMOX_ALLOW_EXEC)", k)
			}
			return nil
		}
	}
	return nil
}

// backupRestoreReserved are restore parameters that pick the target or the overwrite
// behaviour; never taken from `extra` (either guest type).
var backupRestoreReserved = []string{"vmid", "force", "archive", "ostemplate", "restore", "unique", "storage", "live-restore"}

// backupJobOnlyKeys are job keys that describe the schedule/job rather than vzdump options.
var backupJobOnlyKeys = map[string]bool{"id": true, "type": true, "schedule": true, "enabled": true, "comment": true, "repeat-missed": true, "next-run": true, "starttime": true, "dow": true, "node": true, "digest": true}

// backupNodeParams is one vzdump run: a node and its parameters.
type backupNodeParams struct {
	node   string
	params *Obj
}

// backupNodeGroup is VMIDs grouped by node, in order of first appearance.
type backupNodeGroup struct {
	node string
	ids  []any
}

func backupJoin(ids []any) string { return js.Str(ids) }

func registerBackup(c *Context) {
	client := c.Client

	// clusterMap returns the online nodes and where each guest lives.
	clusterMap := func(ctx context.Context) ([]string, map[float64]*Obj, error) {
		res, err := client.GetList(ctx, "/cluster/resources", nil)
		if err != nil {
			return nil, nil, err
		}
		var nodes []string
		guests := map[float64]*Obj{}
		for _, r := range res {
			if r.Get("type") == "node" && r.Get("status") == "online" {
				nodes = append(nodes, js.Str(r.Get("node")))
			}
			if t := r.Get("type"); t == "qemu" || t == "lxc" {
				guests[r.Num("vmid")] = r
			}
		}
		return nodes, guests, nil
	}

	// runVzdump starts one vzdump per node and handles the tasks (in parallel).
	runVzdump := func(ctx context.Context, perNode []backupNodeParams, wait any) ([]any, error) {
		results := make([]any, len(perNode))
		errs := make([]error, len(perNode))
		var wg sync.WaitGroup
		for i, p := range perNode {
			wg.Add(1)
			go func(i int, node string, params *Obj) {
				defer wg.Done()
				upid, err := client.Post(ctx, "/nodes/"+enc(node)+"/vzdump", params)
				if err != nil {
					errs[i] = err
					return
				}
				vmids := ifThen(params.Truthy("vmid"), backupVmidList(params.Get("vmid")))
				res, err := runTask(ctx, client, upid, wait, proxmox.C("node", node, "vmids", vmids, "all", ifThen(params.Truthy("all"), true), "pool", params.Get("pool")))
				if err != nil {
					results[i] = js.O("node", node, "vmids", vmids, "status", "failed", "error", errMessage(err))
					return
				}
				results[i] = res
			}(i, p.node, p.params)
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				return nil, err
			}
		}
		return results, nil
	}

	// groupByNode groups VMIDs by the node they live on, failing on unknown ones.
	groupByNode := func(vmids []any, guests map[float64]*Obj) ([]backupNodeGroup, error) {
		var missing []any
		for _, v := range vmids {
			if _, ok := guests[js.Num(v)]; !ok {
				missing = append(missing, v)
			}
		}
		if len(missing) > 0 {
			parts := make([]string, len(missing))
			for i, m := range missing {
				parts[i] = js.Str(m)
			}
			return nil, proxmox.Errorf("No VM or container with ID %s (see proxmox_list_guests)", strings.Join(parts, ", "))
		}
		var out []backupNodeGroup
		for _, v := range vmids {
			node := js.Str(guests[js.Num(v)].Get("node"))
			found := false
			for i := range out {
				if out[i].node == node {
					out[i].ids = append(out[i].ids, v)
					found = true
				}
			}
			if !found {
				out = append(out, backupNodeGroup{node, []any{v}})
			}
		}
		return out, nil
	}

	// ---------------------------------------------------------------- jobs

	c.Define("proxmox_list_backup_jobs", Def{
		Toolset:     "backup",
		Title:       "List backup jobs",
		Description: "List the cluster's scheduled backup (vzdump) jobs: schedule, next run, enabled, target storage, mode, which guests (all/VMID list/pool, exclusions), compression and retention. See proxmox_get_backup_job for one job with the guests and disks it covers, and proxmox_list_unbacked_guests for guests no job covers.",
		Input:       []Prop{P("search", searchArg), P("raw", rawArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			jobs, err := client.GetList(ctx, "/cluster/backup", nil)
			if err != nil {
				return nil, err
			}
			sortObjs(jobs, func(x, y *Obj) float64 { return compareStr(x.Get("id"), y.Get("id")) })
			res := filterAndLimit(jobs, a.Get("search"), js.Undef, func(j *Obj) []any {
				return []any{j.Get("id"), j.Get("storage"), j.Get("comment"), j.Get("vmid"), j.Get("pool"), j.Get("schedule")}
			})
			if a.Truthy("raw") {
				return res.Obj(proxmox.RedactSecrets(js.Array(res.Items))), nil
			}
			return res.Map(func(j *Obj) any { return backupSummarizeJob(j) }), nil
		},
	})

	c.Define("proxmox_get_backup_job", Def{
		Toolset:     "backup",
		Title:       "Get backup job",
		Description: "Get one backup job's settings plus the guests it currently includes and, per guest, which disks are backed up (and why others are excluded, e.g. backup=0).",
		Input: []Prop{
			P("id", Str().Min(1).D("Backup job ID (see proxmox_list_backup_jobs)")),
			P("include_volumes", Bool().Opt().D("Include the covered guests and their disks (default true)")),
			P("raw", rawArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			path := "/cluster/backup/" + enc(a.Get("id"))
			job, err := client.Get(ctx, path, nil)
			if err != nil {
				return nil, err
			}
			var vols any = js.Undef
			if a.Get("include_volumes") != false {
				if v, err := client.Get(ctx, path+"/included_volumes", nil); err == nil {
					vols = v
				}
			}
			guests := any(js.Undef)
			if vo, ok := vols.(*Obj); ok {
				if children, ok := vo.Get("children").([]any); ok {
					guests = mapObjs(js.Objs(children), func(g *Obj) any {
						typ := g.Get("type")
						switch typ {
						case "qemu":
							typ = "vm"
						case "lxc":
							typ = "container"
						}
						vols := js.Objs(or(g.Get("children"), []any{}))
						return proxmox.C(
							"vmid", g.Num("id"),
							"name", g.Get("name"),
							"type", typ,
							"volumes", mapObjs(vols, func(v *Obj) any {
								return proxmox.C("id", v.Get("id"), "name", v.Get("name"), "included", v.Truthy("included"), "reason", v.Get("reason"))
							}),
						)
					})
				}
			}
			var base *Obj
			if a.Truthy("raw") {
				base = js.O("job", proxmox.RedactSecrets(job))
			} else {
				base = backupSummarizeJob(js.Spread(js.O("id", a.Get("id")), job))
			}
			return proxmox.Compact(js.Spread(base, js.O("guests", guests))), nil
		},
	})

	c.Define("proxmox_save_backup_job", Def{
		Toolset:     "backup",
		Title:       "Create or update backup job",
		Description: "Create a scheduled backup job, or update an existing one when `id` names one. Choose guests with exactly one of vmids, all (optionally with exclude) or pool; on update, giving a new selection replaces the old one. schedule uses systemd calendar syntax, e.g. \"daily\", \"sat 02:00\", \"mon..fri 21:30\" (check with proxmox_simulate_schedule). Only given fields change on update. Run a job immediately with proxmox_run_backup_job.",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input: []Prop{
			P("id", Str().Opt().D("Job ID. Updates the job if it exists; otherwise creates it with this ID (auto-generated when omitted)")),
			P("schedule", Str().Opt().D("When to run, systemd calendar event, e.g. \"daily\", \"02:30\", \"sun 01:00\"")),
			P("enabled", Bool().Opt().D("Enable or disable the job")),
			P("storage", Str().Opt().D("Target storage (must allow 'backup' content)")),
			P("vmids", Arr(Int()).Opt().D("Back up these guests")),
			P("all", Bool().Opt().D("Back up all guests (optionally minus `exclude`)")),
			P("exclude", Arr(Int()).Opt().D("With all=true: guests to skip")),
			P("pool", Str().Opt().D("Back up all guests in this resource pool")),
			P("node", Str().Opt().D("Only run on this node (default: every node backs up its own guests)")),
			P("mode", backupModeArg),
			P("compress", backupCompressArg),
			P("retention", backupRetentionArg),
			P("remove_old", Bool().Opt().D("Prune older backups after the run according to the retention (Proxmox default true)")),
			P("notes_template", backupNotesTemplateArg),
			P("protected", Bool().Opt().D("Mark the resulting backups as protected (never pruned or deleted)")),
			P("comment", Str().Opt().D("Job description")),
			P("repeat_missed", Bool().Opt().D("Run as soon as possible if a run was missed (e.g. the node was off)")),
			P("bwlimit", Int().Min(0).Opt().D("I/O bandwidth limit in KiB/s (0 = unlimited)")),
			P("delete", deleteKeysArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if err := backupCheckHookParams(c, a.Get("extra")); err != nil {
				return nil, err
			}
			exists := false
			if a.Truthy("id") {
				jobs, err := client.GetList(ctx, "/cluster/backup", nil)
				if err != nil {
					return nil, err
				}
				for _, j := range jobs {
					if j.Get("id") == a.Get("id") {
						exists = true
					}
				}
			}
			selectionGiven := a.Defined("vmids") || a.Defined("all") || a.Defined("pool")
			if !exists && !selectionGiven {
				return nil, proxmox.Errorf("A new backup job needs a selection: pass vmids, all=true or pool")
			}
			toDelete := append([]any{}, a.Arr("delete")...)
			if exists && selectionGiven {
				// Swapping selection type: clear the old one, or Proxmox keeps both.
				if !a.Defined("vmids") {
					toDelete = append(toDelete, "vmid")
				}
				if !a.Defined("pool") {
					toDelete = append(toDelete, "pool")
				}
				if !a.Truthy("all") {
					toDelete = append(toDelete, "all")
				}
				if !a.Truthy("all") && !a.Defined("exclude") {
					toDelete = append(toDelete, "exclude")
				}
			}
			joined := func(k string) any {
				if !a.Defined(k) {
					return js.Undef
				}
				return backupJoin(a.Arr(k))
			}
			params, err := definedExtra(js.O(
				"schedule", a.Get("schedule"),
				"enabled", a.Get("enabled"),
				"storage", a.Get("storage"),
				"vmid", joined("vmids"),
				"all", orUndef(a.Get("all")),
				"exclude", joined("exclude"),
				"pool", a.Get("pool"),
				"node", a.Get("node"),
				"mode", a.Get("mode"),
				"compress", a.Get("compress"),
				"prune-backups", backupRetentionString(a.Get("retention")),
				"remove", a.Get("remove_old"),
				"notes-template", a.Get("notes_template"),
				"protected", a.Get("protected"),
				"comment", a.Get("comment"),
				"repeat-missed", a.Get("repeat_missed"),
				"bwlimit", a.Get("bwlimit"),
			), a.Get("extra"), "id", "delete")
			if err != nil {
				return nil, err
			}
			if exists {
				var unique []any
				seen := map[string]bool{}
				for _, k := range toDelete {
					if s := js.Str(k); !seen[s] {
						seen[s] = true
						unique = append(unique, k)
					}
				}
				if _, err := client.Put(ctx, "/cluster/backup/"+enc(a.Get("id")), js.Spread(params, js.O("delete", deleteParam(unique)))); err != nil {
					return nil, err
				}
				return js.O("id", a.Get("id"), "status", "updated"), nil
			}
			if _, err := client.Post(ctx, "/cluster/backup", js.Spread(js.O("id", a.Get("id")), params)); err != nil {
				return nil, err
			}
			// The API doesn't return the generated ID; find the job we just made.
			if !a.Truthy("id") {
				jobs, err := client.GetList(ctx, "/cluster/backup", nil)
				if err != nil {
					jobs = nil
				}
				var match *Obj
				for _, j := range jobs {
					if j.Get("storage") == params.Get("storage") && j.Get("schedule") == params.Get("schedule") && j.Get("comment") == params.Get("comment") {
						match = j
					}
				}
				return proxmox.C("id", match.Get("id"), "status", "created"), nil
			}
			return js.O("id", a.Get("id"), "status", "created"), nil
		},
	})

	c.Define("proxmox_delete_backup_job", Def{
		Toolset:     "backup",
		Title:       "Delete backup job",
		Description: "Delete a scheduled backup job. Existing backups are kept. To pause a job instead, use proxmox_save_backup_job with enabled=false.",
		Delete:      true,
		Idempotent:  true,
		Input:       []Prop{P("id", Str().Min(1).D("Backup job ID (see proxmox_list_backup_jobs)"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/cluster/backup/"+enc(a.Get("id")), nil); err != nil {
				return nil, err
			}
			return js.O("id", a.Get("id"), "status", "deleted"), nil
		},
	})

	c.Define("proxmox_run_backup_job", Def{
		Toolset:     "backup",
		Title:       "Run backup job now",
		Description: "Run a scheduled backup job immediately with its own settings (storage, mode, compression, retention...), the way the scheduler would: one vzdump task per node for the guests on that node. By default waits for all tasks; with wait=false returns the task IDs. Note that with remove_old (the default) old backups are pruned per the job's retention.",
		Write:       true,
		Destructive: True,
		Input:       []Prop{P("id", Str().Min(1).D("Backup job ID (see proxmox_list_backup_jobs)")), P("wait", waitArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			job, err := client.GetObj(ctx, "/cluster/backup/"+enc(a.Get("id")), nil)
			if err != nil {
				return nil, err
			}
			base := js.NewObj()
			for _, k := range job.Keys() {
				if v := job.Get(k); !backupJobOnlyKeys[k] && !js.Nullish(v) {
					base.Set(k, backupToParam(v))
				}
			}
			nodes, guests, err := clusterMap(ctx)
			if err != nil {
				return nil, err
			}
			var perNode []backupNodeParams
			if job.Truthy("vmid") && !job.Truthy("all") && !job.Truthy("pool") {
				vmids := backupVmidList(job.Get("vmid"))
				// Guests deleted since the job was made are skipped, like the scheduler does.
				var present []any
				for _, v := range vmids {
					if _, ok := guests[js.Num(v)]; ok {
						present = append(present, v)
					}
				}
				if len(present) == 0 {
					parts := make([]string, len(vmids))
					for i, v := range vmids {
						parts[i] = js.Str(v)
					}
					return nil, proxmox.Errorf("None of the job's guests (%s) exist any more", strings.Join(parts, ", "))
				}
				groups, err := groupByNode(present, guests)
				if err != nil {
					return nil, err
				}
				for _, g := range groups {
					if job.Truthy("node") && g.node != job.Get("node") {
						continue
					}
					perNode = append(perNode, backupNodeParams{g.node, js.Spread(base, js.O("vmid", backupJoin(g.ids)))})
				}
			} else {
				targets := nodes
				if job.Truthy("node") {
					targets = []string{js.Str(job.Get("node"))}
				}
				excluded := backupVmidList(job.Get("exclude"))
				for _, node := range targets {
					// Skip nodes without any matching guest to avoid empty-run errors.
					has := false
					for _, g := range guests {
						if g.Get("node") != node {
							continue
						}
						if job.Truthy("pool") {
							if g.Get("pool") == job.Get("pool") {
								has = true
							}
						} else {
							in := false
							for _, x := range excluded {
								if x == g.Num("vmid") {
									in = true
								}
							}
							if !in {
								has = true
							}
						}
					}
					if has {
						perNode = append(perNode, backupNodeParams{node, js.Spread(base)})
					}
				}
			}
			if len(perNode) == 0 {
				return nil, proxmox.Errorf("Backup job %s matches no guests on any online node", js.Str(a.Get("id")))
			}
			tasks, err := runVzdump(ctx, perNode, a.Get("wait"))
			if err != nil {
				return nil, err
			}
			return js.O("id", a.Get("id"), "tasks", tasks), nil
		},
	})

	c.Define("proxmox_list_unbacked_guests", Def{
		Toolset:     "backup",
		Title:       "List guests without backup job",
		Description: "List VMs and containers that no backup job covers, with node and status. Use it to audit backup coverage; add them with proxmox_save_backup_job or back them up once with proxmox_backup_guests.",
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			list, err := client.GetList(ctx, "/cluster/backup-info/not-backed-up", nil)
			if err != nil {
				return nil, err
			}
			_, guests, err := clusterMap(ctx)
			if err != nil {
				return nil, err
			}
			items := make([]*Obj, 0, len(list))
			for _, g := range list {
				r := guests[g.Num("vmid")]
				typ := g.Get("type")
				switch typ {
				case "qemu":
					typ = "vm"
				case "lxc":
					typ = "container"
				}
				items = append(items, proxmox.C(
					"vmid", g.Num("vmid"),
					"name", or(g.Get("name"), r.Get("name")),
					"type", typ,
					"node", r.Get("node"),
					"status", r.Get("status"),
					"template", ifThen(r.Truthy("template"), true),
				))
			}
			sortObjs(items, func(x, y *Obj) float64 { return x.Num("vmid") - y.Num("vmid") })
			return js.O("total", len(items), "items", js.Array(items)), nil
		},
	})

	// ---------------------------------------------------------------- ad-hoc backup

	c.Define("proxmox_backup_guests", Def{
		Toolset:     "backup",
		Title:       "Back up guests now",
		Description: "Back up one or more VMs/containers now (vzdump), e.g. before an upgrade. Guests are grouped by node and one task runs per node. Defaults come from the node's vzdump defaults and the storage. Use protected=true or notes_template to label a one-off backup, and remove_old=false to avoid pruning older backups as a side effect. Waits for completion by default (large guests can exceed the wait; follow with proxmox_get_task).",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("vmids", Arr(Int().Min(100)).Min(1).D("Guests to back up, e.g. [100, 200]")),
			P("storage", Str().Opt().D("Target storage (default: from vzdump defaults, usually 'local')")),
			P("mode", backupModeArg),
			P("compress", backupCompressArg),
			P("notes_template", backupNotesTemplateArg),
			P("protected", Bool().Opt().D("Protect the new backups from pruning and deletion")),
			P("remove_old", Bool().Opt().D("Prune older backups of these guests per the retention afterwards (Proxmox default true)")),
			P("retention", backupRetentionArg),
			P("bwlimit", Int().Min(0).Opt().D("I/O bandwidth limit in KiB/s")),
			P("extra", extraArg),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if err := backupCheckHookParams(c, a.Get("extra")); err != nil {
				return nil, err
			}
			_, guests, err := clusterMap(ctx)
			if err != nil {
				return nil, err
			}
			var unique []any
			seen := map[float64]bool{}
			for _, v := range a.Arr("vmids") {
				if n := js.Num(v); !seen[n] {
					seen[n] = true
					unique = append(unique, v)
				}
			}
			groups, err := groupByNode(unique, guests)
			if err != nil {
				return nil, err
			}
			common, err := definedExtra(js.O(
				"storage", a.Get("storage"),
				"mode", a.Get("mode"),
				"compress", a.Get("compress"),
				"notes-template", a.Get("notes_template"),
				"protected", a.Get("protected"),
				"remove", a.Get("remove_old"),
				"prune-backups", backupRetentionString(a.Get("retention")),
				"bwlimit", a.Get("bwlimit"),
			), a.Get("extra"), "vmid", "all", "pool", "exclude")
			if err != nil {
				return nil, err
			}
			perNode := make([]backupNodeParams, len(groups))
			for i, g := range groups {
				perNode[i] = backupNodeParams{g.node, js.Spread(common, js.O("vmid", backupJoin(g.ids)))}
			}
			tasks, err := runVzdump(ctx, perNode, a.Get("wait"))
			if err != nil {
				return nil, err
			}
			failed := 0
			for _, t := range js.Objs(tasks) {
				if t.Get("status") == "failed" {
					failed++
				}
			}
			var status any = js.Undef
			if failed > 0 {
				status = pick(failed == len(tasks), "failed", "partially failed")
			}
			return proxmox.C("status", status, "tasks", tasks), nil
		},
	})

	// ---------------------------------------------------------------- backup volumes

	c.Define("proxmox_list_backups", Def{
		Toolset:     "backup",
		Title:       "List backups",
		Description: "List backup archives across every storage that holds backups (local dirs, NFS/CIFS, Proxmox Backup Server...), newest first: volume ID, guest, time, size, format, notes, protection and PBS verification state. Filter by vmid, type, storage, node or age. Shared storages are listed once. Use the volid with proxmox_restore_backup, proxmox_get_backup_config, proxmox_update_backup or proxmox_delete_backup.",
		Input: []Prop{
			P("vmid", Int().Opt().D("Only backups of this guest")),
			P("type", Enum("vm", "container").Opt().D("Only VM or only container backups")),
			P("storage", Str().Opt().D("Only this storage")),
			P("node", Str().Opt().D("Only storages on this node")),
			P("older_than_days", Num().Min(0).Opt().D("Only backups older than this many days")),
			P("newer_than_days", Num().Min(0).Opt().D("Only backups newer than this many days")),
			P("search", searchArg),
			P("limit", limitArg(100)),
			P("raw", rawArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			all, err := client.GetList(ctx, "/cluster/resources", js.O("type", "storage"))
			if err != nil {
				return nil, err
			}
			storages := filterObjs(all, func(s *Obj) bool {
				return s.Get("status") == "available" &&
					contains(strings.Split(js.Str(or(s.Get("content"), "")), ","), "backup") &&
					(!a.Truthy("storage") || s.Get("storage") == a.Get("storage")) &&
					(!a.Truthy("node") || s.Get("node") == a.Get("node"))
			})
			if a.Truthy("storage") && len(storages) == 0 {
				on := ""
				if a.Truthy("node") {
					on = " on " + js.Str(a.Get("node"))
				}
				return nil, proxmox.Errorf("Storage %s is not an available backup storage%s", js.Str(a.Get("storage")), on)
			}
			seenShared := map[string]bool{}
			targets := filterObjs(storages, func(s *Obj) bool {
				if !js.Truthy(proxmox.Flag(s.Get("shared"))) {
					return true
				}
				id := js.Str(s.Get("storage"))
				if seenShared[id] {
					return false
				}
				seenShared[id] = true
				return true
			})
			errs := []any{}
			var backups []*Obj
			for _, s := range targets {
				items, err := client.Get(ctx, "/nodes/"+enc(s.Get("node"))+"/storage/"+enc(s.Get("storage"))+"/content", defined(js.O("content", "backup", "vmid", a.Get("vmid"))))
				if err != nil {
					errs = append(errs, js.O("storage", s.Get("storage"), "node", s.Get("node"), "error", errMessage(err)))
					continue
				}
				arr, ok := items.([]any)
				if !ok {
					errs = append(errs, js.O("storage", s.Get("storage"), "node", s.Get("node"), "error", "items.map is not a function"))
					continue
				}
				shared := js.Truthy(proxmox.Flag(s.Get("shared")))
				for _, b := range js.Objs(arr) {
					backups = append(backups, js.Spread(b, js.O("_storage", s.Get("storage"), "_node", ifThen(!shared, s.Get("node")))))
				}
			}
			if a.Defined("vmid") {
				backups = filterObjs(backups, func(b *Obj) bool { return b.Num("vmid") == a.Num("vmid") })
			}
			if a.Truthy("type") {
				want := pick(a.Get("type") == "vm", "qemu", "lxc")
				backups = filterObjs(backups, func(b *Obj) bool { return backupGuestType(b) == want })
			}
			now := float64(time.Now().UnixMilli()) / 1000
			if a.Defined("older_than_days") {
				backups = filterObjs(backups, func(b *Obj) bool { return b.Num("ctime") < now-a.Num("older_than_days")*86400 })
			}
			if a.Defined("newer_than_days") {
				backups = filterObjs(backups, func(b *Obj) bool { return b.Num("ctime") >= now-a.Num("newer_than_days")*86400 })
			}
			if backups == nil {
				backups = []*Obj{}
			}
			sortObjs(backups, func(x, y *Obj) float64 { return js.Num(or(y.Get("ctime"), 0.0)) - js.Num(or(x.Get("ctime"), 0.0)) })
			res := filterAndLimit(backups, a.Get("search"), or(a.Get("limit"), 100), func(b *Obj) []any {
				return []any{b.Get("volid"), b.Get("vmid"), b.Get("notes"), b.Get("_storage")}
			})
			totalBytes := 0.0
			for _, b := range backups {
				if n := js.Num(or(b.Get("size"), b.Get("approximate-size"))); js.Truthy(n) {
					totalBytes += n
				}
			}
			var items any
			if a.Truthy("raw") {
				items = proxmox.RedactSecrets(js.Array(res.Items))
			} else {
				items = mapObjs(res.Items, backupSummarizeBackup)
			}
			return proxmox.C(
				"total", res.Total,
				"returned", res.Returned,
				"items", items,
				"total_size", proxmox.FormatBytes(totalBytes),
				"errors", errs,
			), nil
		},
	})

	backupConfigLineRe := regexp.MustCompile(`^([^:#\[]+):\s*(.*)$`)

	c.Define("proxmox_get_backup_config", Def{
		Toolset:     "backup",
		Title:       "Show config inside a backup",
		Description: "Show the guest configuration stored inside a backup archive (cores, memory, disks, network...) without restoring it, e.g. to check what a backup contains or which storage its disks need. Secrets are redacted.",
		Input: []Prop{
			P("volid", backupVolidArg),
			P("node", backupNodeArg),
			P("raw", Bool().Opt().D("Return the config file text as stored (secrets redacted)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			storage, err := backupStorageOf(a.Str("volid"))
			if err != nil {
				return nil, err
			}
			node, err := backupNodeForStorage(ctx, client, storage, a.Str("node"))
			if err != nil {
				return nil, err
			}
			v, err := client.Get(ctx, "/nodes/"+enc(node)+"/vzdump/extractconfig", js.O("volume", a.Get("volid")))
			if err != nil {
				return nil, err
			}
			text := js.Str(or(v, ""))
			if a.Truthy("raw") {
				lines := strings.Split(text, "\n")
				for i, l := range lines {
					m := backupConfigLineRe.FindStringSubmatch(l)
					if m == nil {
						continue
					}
					k := strings.TrimSpace(m[1])
					if red := proxmox.RedactSecrets(js.O(k, m[2])).(*Obj); red.Get(k) != m[2] {
						lines[i] = m[1] + ": <redacted>"
					}
				}
				return js.O("volid", a.Get("volid"), "config", strings.Join(lines, "\n")), nil
			}
			config, snapshots := backupParseConfigText(text)
			return proxmox.C("volid", a.Get("volid"), "type", backupTypeName(backupGuestType(a.Get("volid"))), "config", proxmox.RedactSecrets(config), "snapshots", snapshots), nil
		},
	})

	c.Define("proxmox_restore_backup", Def{
		Toolset:     "backup",
		Title:       "Restore a backup",
		Description: "Restore a VM or container backup (type detected from the archive) to a new VMID, or over an existing guest with force=true. Overwriting destroys the existing guest's disks and config, needs delete permission, and the guest must be stopped. Use unique=true when restoring a copy alongside the original (new MAC addresses). The target node defaults to the node holding the backup's storage; restoring from local storage only works on that node. Find a free VMID with proxmox_get_next_vmid.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("volid", backupVolidArg),
			P("vmid", vmidArg.D("VMID to restore to: a free one for a new guest, or the existing one with force=true")),
			P("node", Str().Opt().D("Node to restore on (default: the node holding the backup's storage)")),
			P("type", Enum("vm", "container").Opt().D("Override the detected guest type")),
			P("storage", Str().Opt().D("Storage for the restored disks (default: as in the backup)")),
			P("force", Bool().Opt().D("Overwrite the existing guest with this VMID (destroys its current disks)")),
			P("unique", Bool().Opt().D("Assign new random MAC addresses (use for copies)")),
			P("start", Bool().Opt().D("Start the guest after restoring")),
			P("pool", Str().Opt().D("Add the restored guest to this resource pool")),
			P("bwlimit", Int().Min(0).Opt().D("I/O bandwidth limit in KiB/s")),
			P("live_restore", Bool().Opt().D("VMs only: start the VM immediately and restore in the background (PBS backups)")),
			P("unprivileged", Bool().Opt().D("Containers only: restore as unprivileged (default: as in the backup)")),
			P("extra", extraArg),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if err := backupCheckHookParams(c, a.Get("extra")); err != nil {
				return nil, err
			}
			typ := backupGuestType(a.Get("volid"))
			if a.Truthy("type") {
				typ = js.Str(pick(a.Get("type") == "vm", "qemu", "lxc"))
			}
			if typ == "" {
				return nil, proxmox.Errorf("Can't tell whether %s is a VM or container backup; pass type", js.Str(a.Get("volid")))
			}
			common := js.O("vmid", a.Get("vmid"), "storage", a.Get("storage"), "force", a.Get("force"), "unique", a.Get("unique"), "start", a.Get("start"), "pool", a.Get("pool"), "bwlimit", a.Get("bwlimit"))
			// Built (and `extra` checked) before the checks below, so they see exactly what is sent: `extra` can't swap
			// in another vmid or add force/archive behind the overwrite check.
			var params *Obj
			var err error
			if typ == "qemu" {
				params, err = definedExtra(js.Spread(common, js.O("archive", a.Get("volid"), "live-restore", a.Get("live_restore"))), a.Get("extra"), backupRestoreReserved...)
			} else {
				params, err = definedExtra(js.Spread(common, js.O("ostemplate", a.Get("volid"), "restore", true, "unprivileged", a.Get("unprivileged"))), a.Get("extra"), backupRestoreReserved...)
			}
			if err != nil {
				return nil, err
			}
			if a.Truthy("live_restore") && typ != "qemu" {
				return nil, proxmox.Errorf("live_restore only applies to VM backups")
			}
			if a.Defined("unprivileged") && typ != "lxc" {
				return nil, proxmox.Errorf("unprivileged only applies to container backups")
			}
			vmid := params.Num("vmid")
			force := params.Truthy("force")
			resources, err := client.GetList(ctx, "/cluster/resources", js.O("type", "vm"))
			if err != nil {
				return nil, err
			}
			var existing *Obj
			for _, r := range resources {
				if r.Num("vmid") == vmid {
					existing = r
					break
				}
			}
			kind := func(t any) string {
				if t == "qemu" {
					return "VM"
				}
				return "container"
			}
			if existing != nil && !force {
				return nil, proxmox.Errorf("VMID %s is in use by %s \"%s\" on %s; pick a free VMID (proxmox_get_next_vmid) or pass force=true to overwrite it",
					js.NumStr(vmid), kind(existing.Get("type")), js.Str(or(existing.Get("name"), "")), js.Str(existing.Get("node")))
			}
			if force && existing != nil {
				if !c.AllowDeletes {
					return nil, proxmox.Errorf("Overwriting an existing guest destroys it and needs delete permission (PROXMOX_ALLOW_DELETES); restore to a new VMID instead")
				}
				if existing.Get("type") != typ {
					return nil, proxmox.Errorf("VMID %s is a %s but the backup is a %s", js.NumStr(vmid), kind(existing.Get("type")), kind(typ))
				}
				if existing.Get("status") == "running" {
					return nil, proxmox.Errorf("Guest %s is running; stop it before restoring over it", js.NumStr(vmid))
				}
			}
			var node string
			switch {
			case a.Present("node"):
				node = a.Str("node")
			case force && existing != nil:
				node = js.Str(existing.Get("node"))
			default:
				storage, err := backupStorageOf(a.Str("volid"))
				if err != nil {
					return nil, err
				}
				if node, err = backupNodeForStorage(ctx, client, storage, ""); err != nil {
					return nil, err
				}
			}
			upid, err := client.Post(ctx, "/nodes/"+enc(node)+"/"+typ, params)
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), js.O("vmid", vmid, "node", node, "type", pick(typ == "qemu", "vm", "container"), "volid", a.Get("volid")))
		},
	})

	c.Define("proxmox_update_backup", Def{
		Toolset:     "backup",
		Title:       "Update backup notes/protection",
		Description: "Change a backup's notes or protection flag. Protected backups are never pruned and can't be deleted until unprotected; use it to keep a known-good backup.",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input: []Prop{
			P("volid", backupVolidArg),
			P("node", backupNodeArg),
			P("notes", Str().Opt().D("New notes (replaces existing; \"\" clears them)")),
			P("protected", Bool().Opt().D("Protect (true) or unprotect (false) the backup")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if !a.Defined("notes") && !a.Defined("protected") {
				return nil, proxmox.Errorf("Nothing to change: pass notes and/or protected")
			}
			storage, err := backupStorageOf(a.Str("volid"))
			if err != nil {
				return nil, err
			}
			node, err := backupNodeForStorage(ctx, client, storage, a.Str("node"))
			if err != nil {
				return nil, err
			}
			if _, err := client.Put(ctx, "/nodes/"+enc(node)+"/storage/"+enc(storage)+"/content/"+enc(a.Get("volid")), defined(js.O("notes", a.Get("notes"), "protected", a.Get("protected")))); err != nil {
				return nil, err
			}
			return proxmox.C("volid", a.Get("volid"), "notes", a.Get("notes"), "protected", a.Get("protected"), "status", "updated"), nil
		},
	})

	c.Define("proxmox_delete_backup", Def{
		Toolset:     "backup",
		Title:       "Delete a backup",
		Description: "Permanently delete a backup archive. Protected backups must be unprotected first (proxmox_update_backup). Cannot be undone.",
		Delete:      true,
		Input:       []Prop{P("volid", backupVolidArg), P("node", backupNodeArg), P("wait", waitArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			storage, err := backupStorageOf(a.Str("volid"))
			if err != nil {
				return nil, err
			}
			node, err := backupNodeForStorage(ctx, client, storage, a.Str("node"))
			if err != nil {
				return nil, err
			}
			res, err := client.Delete(ctx, "/nodes/"+enc(node)+"/storage/"+enc(storage)+"/content/"+enc(a.Get("volid")), nil)
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, res, a.Get("wait"), js.O("volid", a.Get("volid"), "node", node, "status", "deleted"))
		},
	})

	pruneInput := []Prop{
		P("storage", Str().Min(1).D("Backup storage to prune")),
		P("node", Str().Opt().D("Node to access the storage from (needed for node-local storages present on several nodes)")),
		P("vmid", Int().Opt().D("Only this guest's backups")),
		P("type", Enum("vm", "container").Opt().D("Only VM or only container backups")),
		P("retention", backupRetentionArg.D("Retention to apply instead of the storage's configured prune-backups, e.g. {keep_last: 3}")),
	}
	pruneParams := func(a *Obj) *Obj {
		var typ any = js.Undef
		if a.Truthy("type") {
			typ = pick(a.Get("type") == "vm", "qemu", "lxc")
		}
		return defined(js.O("vmid", a.Get("vmid"), "type", typ, "prune-backups", backupRetentionString(a.Get("retention"))))
	}

	c.Define("proxmox_preview_backup_prune", Def{
		Toolset:     "backup",
		Title:       "Preview backup pruning",
		Description: "Dry run of pruning a backup storage: shows which backups the retention rules (the storage's own, or `retention`) would keep and which they would remove. Nothing is deleted; run proxmox_prune_backups with the same arguments to apply.",
		Input:       pruneInput,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := backupNodeForStorage(ctx, client, a.Str("storage"), a.Str("node"))
			if err != nil {
				return nil, err
			}
			list, err := client.GetList(ctx, "/nodes/"+enc(node)+"/storage/"+enc(a.Get("storage"))+"/prunebackups", pruneParams(a))
			if err != nil {
				return nil, err
			}
			sortObjs(list, func(x, y *Obj) float64 { return js.Num(or(y.Get("ctime"), 0.0)) - js.Num(or(x.Get("ctime"), 0.0)) })
			items := make([]*Obj, len(list))
			for i, b := range list {
				typ := b.Get("type")
				switch typ {
				case "qemu":
					typ = "vm"
				case "lxc":
					typ = "container"
				}
				items[i] = proxmox.C("volid", b.Get("volid"), "vmid", b.Get("vmid"), "type", typ, "time", proxmox.IsoTime(b.Get("ctime")), "mark", b.Get("mark"))
			}
			count := func(m string) int { return len(filterObjs(items, func(i *Obj) bool { return i.Get("mark") == m })) }
			wouldRemove := []any{}
			for _, i := range items {
				if i.Get("mark") == "remove" {
					wouldRemove = append(wouldRemove, i.Get("volid"))
				}
			}
			return js.O(
				"storage", a.Get("storage"),
				"node", node,
				"keep", count("keep"),
				"remove", count("remove"),
				"protected", count("protected"),
				"renamed", count("renamed"),
				"would_remove", wouldRemove,
				"items", js.Array(items),
			), nil
		},
	})

	c.Define("proxmox_prune_backups", Def{
		Toolset:     "backup",
		Title:       "Prune backups",
		Description: "Delete the backups on a storage that the retention rules don't keep (the storage's prune-backups, or `retention`). Permanent; check first with proxmox_preview_backup_prune using the same arguments. Protected backups and archives with non-standard names are never removed.",
		Delete:      true,
		Input:       append(append([]Prop{}, pruneInput...), P("wait", waitArg)),
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := backupNodeForStorage(ctx, client, a.Str("storage"), a.Str("node"))
			if err != nil {
				return nil, err
			}
			res, err := client.Delete(ctx, "/nodes/"+enc(node)+"/storage/"+enc(a.Get("storage"))+"/prunebackups", pruneParams(a))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, res, a.Get("wait"), js.O("storage", a.Get("storage"), "node", node))
		},
	})

	c.Define("proxmox_get_vzdump_defaults", Def{
		Toolset:     "backup",
		Title:       "Get backup defaults",
		Description: "Show the vzdump defaults a node applies to backups (from /etc/vzdump.conf, merged with a storage's settings when `storage` is given): default storage, mode, compression, retention, bandwidth limits and so on.",
		Input:       []Prop{P("node", nodeArg), P("storage", Str().Opt().D("Include this storage's settings (e.g. its retention)"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			d, err := client.GetObj(ctx, "/nodes/"+enc(node)+"/vzdump/defaults", defined(js.O("storage", a.Get("storage"))))
			if err != nil {
				return nil, err
			}
			out := proxmox.RedactSecrets(js.Spread(d)).(*Obj)
			if out.Defined("prune-backups") {
				out.Set("retention", backupParseRetention(out.Get("prune-backups")))
			}
			return js.Spread(js.O("node", node), out), nil
		},
	})

	c.Define("proxmox_list_backup_files", Def{
		Toolset:     "backup",
		Title:       "Browse files in a PBS backup",
		Description: "Browse the files inside a Proxmox Backup Server backup (single-file restore): start at path \"/\" to see the archives (disk images/filesystem archives), then drill into directories. Only works for PBS storages. Opening VM disk images starts a temporary restore VM on the node and can take a while on first access.",
		Input: []Prop{
			P("storage", Str().Min(1).D("PBS storage ID")),
			P("volid", backupVolidArg),
			P("path", Str().Opt().D("Path to list, as shown in earlier results (default \"/\")")),
			P("node", Str().Opt().D("Node to run the file restore on (default: any node with the storage)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := backupNodeForStorage(ctx, client, a.Str("storage"), a.Str("node"))
			if err != nil {
				return nil, err
			}
			path := "/"
			if a.Truthy("path") && a.Get("path") != "/" {
				path = a.Str("path")
			}
			filepath := "/"
			if path != "/" {
				filepath = base64.StdEncoding.EncodeToString([]byte(path))
			}
			entries, err := client.GetList(ctx, "/nodes/"+enc(node)+"/storage/"+enc(a.Get("storage"))+"/file-restore/list", js.O("volume", a.Get("volid"), "filepath", filepath))
			if err != nil {
				return nil, err
			}
			return js.O(
				"volid", a.Get("volid"),
				"path", path,
				"entries", mapObjs(entries, func(e *Obj) any {
					leaf := e.Truthy("leaf")
					return proxmox.C(
						"name", e.Get("text"),
						"path", ifThen(e.Truthy("filepath"), string(backupDecodeBase64(js.Str(e.Get("filepath"))))),
						"type", e.Get("type"),
						"directory", ifThen(!leaf, true),
						"size", ifThen(leaf, proxmox.FormatBytes(e.Get("size"))),
						"mtime", proxmox.IsoTime(e.Get("mtime")),
					)
				}),
			), nil
		},
	})
}

// backupDecodeBase64 decodes like Buffer.from(s, "base64"): characters outside the base64 (or
// base64url) alphabet are skipped, decoding stops at padding, and invalid UTF-8 later shows as U+FFFD.
func backupDecodeBase64(s string) []byte {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '=':
			i = len(s)
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '+', ch == '/':
			b.WriteByte(ch)
		case ch == '-':
			b.WriteByte('+')
		case ch == '_':
			b.WriteByte('/')
		}
	}
	clean := b.String()
	if len(clean)%4 == 1 {
		clean = clean[:len(clean)-1]
	}
	out, err := base64.RawStdEncoding.DecodeString(clean)
	if err != nil {
		return nil
	}
	return out
}
