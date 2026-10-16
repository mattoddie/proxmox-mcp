package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

var storageTypes = []string{
	"btrfs", "cephfs", "cifs", "dir", "esxi", "iscsi", "iscsidirect", "lvm", "lvmthin", "nfs", "pbs", "rbd", "zfs", "zfspool",
}

var storageContentTypes = []string{"images", "rootdir", "iso", "vztmpl", "backup", "snippets", "import"}

// storageCreateOnly lists options POST /storage accepts but PUT /storage/{storage} does not (fixed once the storage exists).
var storageCreateOnly = []string{"type", "path", "export", "share", "portal", "target", "vgname", "thinpool", "datastore", "base", "authsupported", "iscsiprovider"}

// storageSecretRe: RedactSecrets doesn't know these storage-specific secret names.
var storageSecretRe = regexp.MustCompile(`^(encryption-key|keyring|password)$`)

func storageRedact(value any) any {
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case []any:
			out := make([]any, len(x))
			for i, e := range x {
				out[i] = walk(e)
			}
			return out
		case *Obj:
			if x == nil {
				return x
			}
			out := js.NewObj()
			for _, k := range x.Keys() {
				val := x.Get(k)
				if s, ok := val.(string); ok && s != "" && storageSecretRe.MatchString(k) {
					out.Set(k, "<redacted>")
				} else {
					out.Set(k, walk(val))
				}
			}
			return out
		}
		return v
	}
	return walk(proxmox.RedactSecrets(value))
}

var storageListSplitRe = regexp.MustCompile(`[,;\s]+`)

// storageSplitList splits a comma/semicolon/space separated list; undefined unless v is a non-empty string.
func storageSplitList(v any) any {
	s, ok := v.(string)
	if !ok || s == "" {
		return js.Undef
	}
	out := []any{}
	for _, p := range storageListSplitRe.Split(s, -1) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func storageListIncludes(list any, want any) bool {
	arr, _ := list.([]any)
	for _, x := range arr {
		if x == want {
			return true
		}
	}
	return false
}

// storageSummarizeConfig summarizes a storage definition from /storage.
func storageSummarizeConfig(s *Obj) *Obj {
	var prune any = js.Undef
	if s.Truthy("prune-backups") {
		prune = proxmox.ParsePropertyString(s.Get("prune-backups"), "")
	}
	return proxmox.C(
		"storage", s.Get("storage"),
		"type", s.Get("type"),
		"content", storageSplitList(s.Get("content")),
		"shared", orUndef(proxmox.Flag(s.Get("shared"))),
		"disabled", orUndef(proxmox.Flag(s.Get("disable"))),
		"nodes", storageSplitList(s.Get("nodes")),
		"path", s.Get("path"),
		"server", s.Get("server"),
		"export", s.Get("export"),
		"share", s.Get("share"),
		"datastore", s.Get("datastore"),
		"namespace", s.Get("namespace"),
		"pool", s.Get("pool"),
		"vgname", s.Get("vgname"),
		"thinpool", s.Get("thinpool"),
		"portal", s.Get("portal"),
		"target", s.Get("target"),
		"monhost", s.Get("monhost"),
		"fs_name", s.Get("fs-name"),
		"username", s.Get("username"),
		"format", s.Get("format"),
		"prune_backups", prune,
		"max_protected_backups", s.Get("max-protected-backups"),
		"options", s.Get("options"),
	)
}

// storageSummarizeStatus summarizes a /cluster/resources storage entry as per-node status.
func storageSummarizeStatus(r *Obj) any {
	var free any = js.Undef
	if r.Truthy("maxdisk") {
		free = proxmox.FormatBytes(r.Num("maxdisk") - js.Num(or(r.Get("disk"), 0)))
	}
	return proxmox.C(
		"node", r.Get("node"),
		"status", r.Get("status"),
		"usage", proxmox.Usage(r.Get("disk"), r.Get("maxdisk")),
		"used_pct", proxmox.Percent(r.Get("disk"), r.Get("maxdisk")),
		"free", free,
	)
}

func storageSummarizeVolume(v *Obj) any {
	var vmid any = js.Undef
	if v.Defined("vmid") {
		vmid = v.Num("vmid")
	}
	return proxmox.C(
		"volid", v.Get("volid"),
		"content", v.Get("content"),
		"format", v.Get("format"),
		"size", proxmox.FormatBytes(or(v.Get("size"), v.Get("approximate-size"))),
		"used", ifThen(v.Truthy("used"), proxmox.FormatBytes(v.Get("used"))),
		"vmid", vmid,
		"ctime", proxmox.IsoTime(v.Get("ctime")),
		"notes", v.Get("notes"),
		"protected", orUndef(proxmox.Flag(v.Get("protected"))),
		"parent", v.Get("parent"),
		"encrypted", v.Get("encrypted"),
		"verification", v.ObjAt("verification").Get("state"),
	)
}

func storageSummarizeDisk(d *Obj) any {
	wear := d.Num("wearout")
	var vendor any = js.Undef
	if d.Truthy("vendor") && d.Get("vendor") != "unknown" {
		vendor = strings.TrimSpace(js.Str(d.Get("vendor")))
	}
	return proxmox.C(
		"devpath", d.Get("devpath"),
		"type", d.Get("type"),
		"model", d.Get("model"),
		"vendor", vendor,
		"serial", d.Get("serial"),
		"size", proxmox.FormatBytes(d.Get("size")),
		"rpm", ifThen(d.Num("rpm") > 0, d.Num("rpm")),
		"health", d.Get("health"),
		// The API reports remaining life (100 = new); the web UI shows 100 - value as "Wearout".
		"wearout_pct", ifThen(d.Defined("wearout") && d.Get("wearout") != "N/A" && js.IsFinite(wear), 100-wear),
		"used_by", d.Get("used"),
		"gpt", orUndef(proxmox.Flag(d.Get("gpt"))),
		"mounted", orUndef(proxmox.Flag(d.Get("mounted"))),
		"osdid", ifThen(d.Num("osdid") >= 0, d.Num("osdid")),
		"parent", d.Get("parent"),
		"wwn", d.Get("wwn"),
		"by_id", d.Get("by_id_link"),
	)
}

func storageSummarizeVdev(v *Obj) any {
	var children any = js.Undef
	if arr, ok := v.Get("children").([]any); ok {
		children = mapObjs(js.Objs(arr), storageSummarizeVdev)
	}
	return proxmox.C(
		"name", v.Get("name"),
		"state", v.Get("state"),
		"read", v.Get("read"),
		"write", v.Get("write"),
		"cksum", v.Get("cksum"),
		"msg", v.Get("msg"),
		"children", children,
	)
}

// storageOf returns the storage ID from a volume ID like "local:iso/debian.iso" (undefined when there is none).
func storageOf(volume string) any {
	if idx := strings.Index(volume, ":"); idx > 0 {
		return volume[:idx]
	}
	return js.Undef
}

func storageExists(ctx context.Context, c *Context, storage any) (bool, error) {
	list, err := c.Client.GetList(ctx, "/storage", nil)
	if err != nil {
		return false, err
	}
	for _, s := range list {
		if s.Get("storage") == storage {
			return true, nil
		}
	}
	return false, nil
}

var (
	storageArg       = Str().D("Storage ID, e.g. \"local\" or \"local-lvm\" (see proxmox_list_storage)")
	storageVolumeArg = Str().D("Volume ID, e.g. \"local:iso/debian-12.iso\" or \"local-lvm:vm-100-disk-0\" (see proxmox_list_storage_content)")
	storageVolStArg  = Str().Opt().D("Storage ID; taken from the volume ID's prefix when omitted")
	storageBlockArg  = Str().D("Block device path, e.g. \"/dev/sdb\" (see proxmox_list_disks)")

	storagePruneArg = Object(
		P("keep_all", Bool().Opt().D("Keep all backups (overrides the others)")),
		P("keep_last", Int().Min(0).Opt().D("Keep the last N backups")),
		P("keep_hourly", Int().Min(0).Opt().D("Keep backups for the last N hours")),
		P("keep_daily", Int().Min(0).Opt().D("Keep backups for the last N days")),
		P("keep_weekly", Int().Min(0).Opt().D("Keep backups for the last N weeks")),
		P("keep_monthly", Int().Min(0).Opt().D("Keep backups for the last N months")),
		P("keep_yearly", Int().Min(0).Opt().D("Keep backups for the last N years")),
	).Opt().D("Backup retention for this storage (prune-backups), e.g. {keep_last: 3, keep_daily: 7}")
)

func storagePruneString(p any) any {
	o, ok := p.(*Obj)
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

// storageRemoteScanTypes are scan types that make the node connect to a server named in the arguments.
var storageRemoteScanTypes = map[string]bool{"nfs": true, "cifs": true, "pbs": true, "iscsi": true}

// storageVolumeStorage is `a.storage ?? storageOf(a.volume)`, failing when neither gives one.
func storageVolumeStorage(a *Obj) (string, error) {
	storage := or(a.Get("storage"), storageOf(a.Str("volume")))
	if !js.Truthy(storage) {
		return "", errors.New("Pass `storage` or a full volume ID (storage:volume)")
	}
	return js.Str(storage), nil
}

func storageJoin(v any) any {
	arr, ok := v.([]any)
	if !ok {
		return js.Undef
	}
	parts := make([]string, len(arr))
	for i, x := range arr {
		parts[i] = js.Str(x)
	}
	return strings.Join(parts, ",")
}

func registerStorage(c *Context) {
	client := c.Client

	// ---------------------------------------------------------------- storage definitions

	c.Define("proxmox_list_storage", Def{
		Toolset:     "storage",
		Title:       "List storage",
		Description: "List the datacenter's storage definitions (local dirs, LVM, ZFS, NFS, CIFS, PBS, Ceph...) with type, allowed content, shared/enabled flags, node restriction, server/path/pool details and backup retention, merged with each node's live status and usage. Use proxmox_list_storage_content to see what's on one.",
		Input: []Prop{
			P("type", Enum(storageTypes...).Opt().D("Only storage of this type")),
			P("content", Enum(storageContentTypes...).Opt().D("Only storage that allows this content type")),
			P("node", Str().Opt().D("Only storage available on this node")),
			P("search", searchArg),
			P("raw", rawArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			configs, err := client.GetList(ctx, "/storage", defined(js.O("type", a.Get("type"))))
			if err != nil {
				return nil, err
			}
			resources, err := client.GetList(ctx, "/cluster/resources", js.O("type", "storage"))
			if err != nil {
				resources = []*Obj{}
			}
			list := configs
			if a.Truthy("content") {
				list = filterObjs(list, func(s *Obj) bool { return storageListIncludes(storageSplitList(s.Get("content")), a.Get("content")) })
			}
			if a.Truthy("node") {
				list = filterObjs(list, func(s *Obj) bool {
					nodes := storageSplitList(s.Get("nodes"))
					if !js.IsUndef(nodes) {
						return storageListIncludes(nodes, a.Get("node"))
					}
					for _, r := range resources {
						if r.Get("storage") == s.Get("storage") && r.Get("node") == a.Get("node") {
							return true
						}
					}
					return len(resources) == 0
				})
			}
			sortObjs(list, func(x, y *Obj) float64 { return compareStr(x.Get("storage"), y.Get("storage")) })
			res := filterAndLimit(list, a.Get("search"), js.Undef, func(s *Obj) []any {
				return []any{s.Get("storage"), s.Get("type"), s.Get("path"), s.Get("server"), s.Get("pool"), s.Get("content")}
			})
			if a.Truthy("raw") {
				return res.Obj(storageRedact(js.Array(res.Items))), nil
			}
			return res.Map(func(s *Obj) any {
				status := filterObjs(resources, func(r *Obj) bool {
					return r.Get("storage") == s.Get("storage") && (!a.Truthy("node") || r.Get("node") == a.Get("node"))
				})
				sortObjs(status, func(x, y *Obj) float64 { return compareStr(x.Get("node"), y.Get("node")) })
				return proxmox.Compact(js.Spread(storageSummarizeConfig(s), js.O("status", mapObjs(status, storageSummarizeStatus))))
			}), nil
		},
	})

	c.Define("proxmox_get_storage", Def{
		Toolset:     "storage",
		Title:       "Get storage",
		Description: "Get one storage definition (full config, secrets redacted) and its status/usage on each node. Pass `node` for that node's detailed status (active, enabled, available bytes).",
		Input: []Prop{
			P("storage", storageArg),
			P("node", Str().Opt().D("Also fetch detailed status on this node")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			config, err := client.GetObj(ctx, "/storage/"+enc(a.Get("storage")), nil)
			if err != nil {
				return nil, err
			}
			resources, err := client.GetList(ctx, "/cluster/resources", js.O("type", "storage"))
			if err != nil {
				resources = []*Obj{}
			}
			var nodeStatus any = js.Undef
			if a.Truthy("node") {
				if nodeStatus, err = client.Get(ctx, "/nodes/"+enc(a.Get("node"))+"/storage/"+enc(a.Get("storage"))+"/status", nil); err != nil {
					return nil, err
				}
			}
			var ns any = js.Undef
			if js.Truthy(nodeStatus) {
				st, _ := nodeStatus.(*Obj)
				ns = proxmox.C(
					"node", a.Get("node"),
					"type", st.Get("type"),
					"active", proxmox.Flag(st.Get("active")),
					"enabled", proxmox.Flag(st.Get("enabled")),
					"shared", proxmox.Flag(st.Get("shared")),
					"content", storageSplitList(st.Get("content")),
					"usage", proxmox.Usage(st.Get("used"), st.Get("total")),
					"available", proxmox.FormatBytes(st.Get("avail")),
				)
			}
			return proxmox.C(
				"summary", storageSummarizeConfig(config),
				"config", storageRedact(config),
				"status", mapObjs(filterObjs(resources, func(r *Obj) bool { return r.Get("storage") == a.Get("storage") }), storageSummarizeStatus),
				"node_status", ns,
			), nil
		},
	})

	c.Define("proxmox_save_storage", Def{
		Toolset:     "storage",
		Title:       "Create or update storage",
		Description: "Create a storage definition, or update an existing one (decided by whether the storage ID exists). Typed fields cover dir, nfs, cifs, lvm, lvmthin, zfspool, pbs, cephfs, rbd and iscsi; anything else goes in `extra`. On update, type and location fields (path, export, share, vgname, thinpool, datastore, portal, target) can't be changed. Use proxmox_scan_storage first to discover NFS exports, CIFS shares, PBS datastores, VGs and pools. Passwords and keys are redacted from the output.",
		Input: []Prop{
			P("storage", Str().D("Storage ID to create or update, e.g. \"nas-backup\"")),
			P("type", Enum(storageTypes...).Opt().D("Storage type (required when creating)")),
			P("content", Arr(Enum(storageContentTypes...)).Opt().D("Allowed content types; images = VM disks, rootdir = container volumes")),
			P("nodes", Arr(Str()).Opt().D("Restrict the storage to these nodes (default all nodes)")),
			P("shared", Bool().Opt().D("Mark as shared: same content on all nodes (needed for migration without copying; e.g. for a dir on a cluster filesystem)")),
			P("disable", Bool().Opt().D("Disable the storage")),
			P("path", Str().Opt().D("dir/btrfs: filesystem path (create only)")),
			P("server", Str().Opt().D("nfs/cifs/pbs: server name or IP")),
			P("export", Str().Opt().D("nfs: export path (create only)")),
			P("share", Str().Opt().D("cifs: share name (create only)")),
			P("subdir", Str().Opt().D("cifs/cephfs: subdirectory to mount")),
			P("domain", Str().Opt().D("cifs: SMB domain/workgroup")),
			// zod renders enum values in JavaScript object key order, so the integer-like "3" comes first.
			P("smbversion", Enum("3", "default", "2.0", "2.1", "3.0", "3.11").Opt().D("cifs: SMB protocol version")),
			P("options", Str().Opt().D("nfs/cifs: mount options, e.g. \"vers=4.2\"")),
			P("username", Str().Opt().D("cifs/pbs: user name (pbs: user@realm or API token ID); rbd: Ceph user id")),
			P("password", Str().Opt().D("cifs/pbs: password or PBS API token secret")),
			P("datastore", Str().Opt().D("pbs: datastore name (create only)")),
			P("namespace", Str().Opt().D("pbs: namespace")),
			P("fingerprint", Str().Opt().D("pbs: server certificate SHA-256 fingerprint")),
			P("encryption_key", Str().Opt().D("pbs: client-side encryption key, or \"autogen\" to generate one")),
			P("port", Int().Opt().D("pbs/esxi: non-default port")),
			P("vgname", Str().Opt().D("lvm/lvmthin: volume group name (create only)")),
			P("thinpool", Str().Opt().D("lvmthin: thin pool LV name (create only)")),
			P("saferemove", Bool().Opt().D("lvm: zero-out data when removing LVs")),
			P("pool", Str().Opt().D("zfspool: ZFS pool/dataset, e.g. \"rpool/data\"; rbd: Ceph pool")),
			P("blocksize", Str().Opt().D("zfspool: block size, e.g. \"16k\"")),
			P("sparse", Bool().Opt().D("zfspool: thin-provisioned (sparse) zvols")),
			P("monhost", Str().Opt().D("rbd/cephfs (external Ceph): monitor addresses")),
			P("fs_name", Str().Opt().D("cephfs: Ceph filesystem name")),
			P("krbd", Bool().Opt().D("rbd: always use the krbd kernel module")),
			P("keyring", Str().Opt().D("rbd/cephfs (external Ceph): client keyring contents")),
			P("portal", Str().Opt().D("iscsi: portal IP or name (create only)")),
			P("target", Str().Opt().D("iscsi: target IQN (create only)")),
			P("format", Enum("raw", "qcow2", "subvol", "vmdk").Opt().D("Default image format")),
			P("preallocation", Enum("off", "metadata", "falloc", "full").Opt().D("Preallocation for raw/qcow2 images")),
			P("is_mountpoint", Str().Opt().D("dir: \"yes\" (or a path) if the path is an externally managed mount point; storage goes offline when not mounted")),
			P("prune_backups", storagePruneArg),
			P("max_protected_backups", Int().Min(-1).Opt().D("Maximum protected backups per guest (-1 = unlimited)")),
			P("bwlimit", Str().Opt().D("I/O bandwidth limits in KiB/s, e.g. \"default=100000,restore=50000\"")),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("extra", extraArg),
		},
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			exists, err := storageExists(ctx, c, a.Get("storage"))
			if err != nil {
				return nil, err
			}
			params, err := definedExtra(js.O(
				"type", a.Get("type"),
				"content", storageJoin(a.Get("content")),
				"nodes", storageJoin(a.Get("nodes")),
				"shared", a.Get("shared"),
				"disable", a.Get("disable"),
				"path", a.Get("path"),
				"server", a.Get("server"),
				"export", a.Get("export"),
				"share", a.Get("share"),
				"subdir", a.Get("subdir"),
				"domain", a.Get("domain"),
				"smbversion", a.Get("smbversion"),
				"options", a.Get("options"),
				"username", a.Get("username"),
				"password", a.Get("password"),
				"datastore", a.Get("datastore"),
				"namespace", a.Get("namespace"),
				"fingerprint", a.Get("fingerprint"),
				"encryption-key", a.Get("encryption_key"),
				"port", a.Get("port"),
				"vgname", a.Get("vgname"),
				"thinpool", a.Get("thinpool"),
				"saferemove", a.Get("saferemove"),
				"pool", a.Get("pool"),
				"blocksize", a.Get("blocksize"),
				"sparse", a.Get("sparse"),
				"monhost", a.Get("monhost"),
				"fs-name", a.Get("fs_name"),
				"krbd", a.Get("krbd"),
				"keyring", a.Get("keyring"),
				"portal", a.Get("portal"),
				"target", a.Get("target"),
				"format", a.Get("format"),
				"preallocation", a.Get("preallocation"),
				"is_mountpoint", a.Get("is_mountpoint"),
				"prune-backups", storagePruneString(a.Get("prune_backups")),
				"max-protected-backups", a.Get("max_protected_backups"),
				"bwlimit", a.Get("bwlimit"),
			), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			storage := a.Str("storage")
			var result any
			if exists {
				var fixed []string
				for _, k := range storageCreateOnly {
					if params.Defined(k) {
						fixed = append(fixed, k)
					}
				}
				if len(fixed) > 0 {
					return nil, fmt.Errorf("Storage %s already exists and %s can't be changed on update; delete and recreate it, or drop those arguments", storage, strings.Join(fixed, ", "))
				}
				result, err = client.Put(ctx, "/storage/"+enc(storage), js.Spread(params, js.O("delete", deleteParam(a.Get("delete")), "digest", a.Get("digest"))))
			} else {
				if !a.Truthy("type") && !params.Truthy("type") {
					return nil, fmt.Errorf("Storage %s doesn't exist yet; pass `type` to create it", storage)
				}
				if len(a.Arr("delete")) > 0 || a.Truthy("digest") {
					return nil, errors.New("`delete` and `digest` only apply when updating an existing storage")
				}
				result, err = client.Post(ctx, "/storage", js.Spread(js.O("storage", storage), params))
			}
			if err != nil {
				return nil, err
			}
			generatedKey := a.Get("encryption_key") == "autogen" || params.Get("encryption-key") == "autogen"
			return proxmox.C(
				"storage", storage,
				"action", pick(exists, "updated", "created"),
				"result", ifThen(js.Truthy(result), storageRedact(result)),
				"note", ifThen(generatedKey, "An encryption key was generated and stored at /etc/pve/priv/storage/"+storage+".enc; back it up somewhere safe, backups can't be restored without it."),
			), nil
		},
	})

	c.Define("proxmox_delete_storage", Def{
		Toolset:     "storage",
		Title:       "Delete storage",
		Description: "Remove a storage definition from the datacenter configuration. The data on the underlying disk/share is not deleted, but guests with disks on it will break. To destroy a local ZFS pool/LVM/directory and its disks use proxmox_delete_disk_storage.",
		Input:       []Prop{P("storage", storageArg)},
		Delete:      true,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/storage/"+enc(a.Get("storage")), nil); err != nil {
				return nil, err
			}
			return js.O("storage", a.Get("storage"), "deleted", true), nil
		},
	})

	// ---------------------------------------------------------------- content

	c.Define("proxmox_list_storage_content", Def{
		Toolset:     "storage",
		Title:       "List storage content",
		Description: "List the volumes on a storage: ISO images, container templates, backups, VM/CT disks, snippets and import files, with format, size, owner VMID, creation time, notes and protection. Filter by content type or VMID (e.g. all backups of guest 100).",
		Input: []Prop{
			P("node", nodeArg),
			P("storage", storageArg),
			P("content", Enum(storageContentTypes...).Opt().D("Only this content type")),
			P("vmid", Int().Opt().D("Only volumes owned by this guest")),
			P("search", searchArg),
			P("limit", limitArg(500)),
			P("raw", rawArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			items, err := client.GetList(ctx, "/nodes/"+enc(node)+"/storage/"+enc(a.Get("storage"))+"/content", defined(js.O("content", a.Get("content"), "vmid", a.Get("vmid"))))
			if err != nil {
				return nil, err
			}
			sortObjs(items, func(x, y *Obj) float64 { return compareStr(x.Get("volid"), y.Get("volid")) })
			res := filterAndLimit(items, a.Get("search"), or(a.Get("limit"), 500), func(v *Obj) []any {
				return []any{v.Get("volid"), v.Get("notes"), v.Get("vmid"), v.Get("format")}
			})
			var out *Obj
			if a.Truthy("raw") {
				out = res.Obj(nil)
			} else {
				out = res.Map(storageSummarizeVolume)
			}
			return js.Spread(js.O("node", node, "storage", a.Get("storage")), out), nil
		},
	})

	c.Define("proxmox_get_volume", Def{
		Toolset:     "storage",
		Title:       "Get volume attributes",
		Description: "Get a volume's attributes: path on disk, format, size, used space, notes and protection flag.",
		Input:       []Prop{P("node", nodeArg), P("volume", storageVolumeArg), P("storage", storageVolStArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			storage, err := storageVolumeStorage(a)
			if err != nil {
				return nil, err
			}
			v, err := client.GetObj(ctx, "/nodes/"+enc(node)+"/storage/"+enc(storage)+"/content/"+enc(a.Get("volume")), nil)
			if err != nil {
				return nil, err
			}
			return proxmox.C(
				"volume", a.Get("volume"),
				"node", node,
				"path", v.Get("path"),
				"format", v.Get("format"),
				"size", proxmox.FormatBytes(v.Get("size")),
				"size_bytes", v.Get("size"),
				"used", ifThen(v.Truthy("used"), proxmox.FormatBytes(v.Get("used"))),
				"notes", v.Get("notes"),
				"protected", proxmox.Flag(v.Get("protected")),
			), nil
		},
	})

	c.Define("proxmox_update_volume", Def{
		Toolset:     "storage",
		Title:       "Update volume attributes",
		Description: "Set a volume's notes or protection flag. Protection (backups only) prevents pruning and deletion until removed.",
		Input: []Prop{
			P("node", nodeArg),
			P("volume", storageVolumeArg),
			P("storage", storageVolStArg),
			P("notes", Str().Opt().D("New notes (\"\" clears them)")),
			P("protected", Bool().Opt().D("Protect (true) or unprotect (false) the volume; only supported for backups")),
		},
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if !a.Defined("notes") && !a.Defined("protected") {
				return nil, errors.New("Nothing to change; pass `notes` and/or `protected`")
			}
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			storage, err := storageVolumeStorage(a)
			if err != nil {
				return nil, err
			}
			if _, err := client.Put(ctx, "/nodes/"+enc(node)+"/storage/"+enc(storage)+"/content/"+enc(a.Get("volume")), defined(js.O("notes", a.Get("notes"), "protected", a.Get("protected")))); err != nil {
				return nil, err
			}
			return proxmox.C("volume", a.Get("volume"), "updated", true, "notes", a.Get("notes"), "protected", a.Get("protected")), nil
		},
	})

	c.Define("proxmox_delete_volume", Def{
		Toolset:     "storage",
		Title:       "Delete volume",
		Description: "Permanently delete a volume (ISO, template, backup or disk image) from storage. Disks still referenced by a guest config can't be deleted; detach them first. Protected backups must be unprotected with proxmox_update_volume.",
		Input:       []Prop{P("node", nodeArg), P("volume", storageVolumeArg), P("storage", storageVolStArg), P("wait", waitArg)},
		Delete:      true,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			storage, err := storageVolumeStorage(a)
			if err != nil {
				return nil, err
			}
			res, err := client.Delete(ctx, "/nodes/"+enc(node)+"/storage/"+enc(storage)+"/content/"+enc(a.Get("volume")), nil)
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, res, a.Get("wait"), js.O("volume", a.Get("volume"), "node", node, "action", "delete"))
		},
	})

	c.Define("proxmox_allocate_disk", Def{
		Toolset:     "storage",
		Title:       "Allocate disk image",
		Description: "Allocate a new, empty disk image on a storage, owned by a guest (it shows up as unused until attached). Most of the time adding a disk via the VM/container config tools is easier; use this for pre-allocating. Returns the new volume ID.",
		Input: []Prop{
			P("node", nodeArg),
			P("storage", storageArg),
			P("vmid", Int().Min(100).D("Owner guest VMID")),
			P("filename", Str().D("Volume name, e.g. \"vm-100-disk-2\" (add .qcow2/.raw on file-based storage)")),
			P("size", Str().D("Size with suffix M or G (plain number = KiB), e.g. \"32G\"")),
			P("format", Enum("raw", "qcow2", "subvol", "vmdk").Opt().D("Image format (default depends on the storage)")),
		},
		Write:       true,
		Destructive: False,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			res, err := client.Post(ctx, "/nodes/"+enc(node)+"/storage/"+enc(a.Get("storage"))+"/content",
				defined(js.O("vmid", a.Get("vmid"), "filename", a.Get("filename"), "size", a.Get("size"), "format", a.Get("format"))))
			if err != nil {
				return nil, err
			}
			return js.O("node", node, "storage", a.Get("storage"), "volid", res), nil
		},
	})

	// ---------------------------------------------------------------- downloads and templates

	c.Define("proxmox_download_to_storage", Def{
		Toolset:     "storage",
		Title:       "Download file to storage",
		Description: "Have a node download an ISO image, container template (vztmpl) or importable disk/OVA (import) from a URL straight onto a storage, optionally verifying a checksum and decompressing. Use proxmox_query_url_metadata to check the URL and file name first; for official LXC templates use proxmox_download_appliance_template.",
		Input: []Prop{
			P("node", nodeArg),
			P("storage", storageArg),
			P("url", Str().D("URL to download")),
			P("content", Enum("iso", "vztmpl", "import").D("Content type: iso, vztmpl (container template) or import (disk images/OVA)")),
			P("filename", Str().D("Target file name, e.g. \"debian-12.7.0-amd64-netinst.iso\" (normalized by Proxmox)")),
			P("checksum", Str().Opt().D("Expected checksum of the file")),
			P("checksum_algorithm", Enum("md5", "sha1", "sha224", "sha256", "sha384", "sha512").Opt().D("Checksum algorithm (required with checksum)")),
			P("compression", Str().Opt().D("Decompress the download with this algorithm, e.g. \"zst\", \"gz\", \"lzo\" (ISOs only)")),
			P("verify_certificates", Bool().Opt().D("Verify the server's TLS certificate (default true)")),
			P("wait", waitArg),
		},
		Write:       true,
		Destructive: False,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if a.Truthy("checksum") && !a.Truthy("checksum_algorithm") {
				return nil, errors.New("Pass checksum_algorithm together with checksum")
			}
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			res, err := client.Post(ctx, "/nodes/"+enc(node)+"/storage/"+enc(a.Get("storage"))+"/download-url", defined(js.O(
				"url", a.Get("url"),
				"content", a.Get("content"),
				"filename", a.Get("filename"),
				"checksum", a.Get("checksum"),
				"checksum-algorithm", a.Get("checksum_algorithm"),
				"compression", a.Get("compression"),
				"verify-certificates", a.Get("verify_certificates"),
			)))
			if err != nil {
				return nil, err
			}
			volid := js.Str(a.Get("storage")) + ":" + js.Str(a.Get("content")) + "/" + js.Str(a.Get("filename"))
			return runTask(ctx, client, res, a.Get("wait"), js.O("node", node, "storage", a.Get("storage"), "volid", volid))
		},
	})

	c.Define("proxmox_list_appliance_templates", Def{
		Toolset:     "storage",
		Title:       "List appliance templates",
		Description: "List the LXC container templates available for download from the Proxmox appliance index (Debian, Ubuntu, Alpine..., and TurnKey apps). Download one with proxmox_download_appliance_template. If the list is empty, the node may need `pveam update`.",
		Input: []Prop{
			P("node", nodeArg),
			P("section", Str().Opt().D("Only this section, e.g. \"system\" or \"turnkeylinux\"")),
			P("search", searchArg),
			P("limit", limitArg(200)),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			items, err := client.GetList(ctx, "/nodes/"+enc(node)+"/aplinfo", nil)
			if err != nil {
				return nil, err
			}
			if a.Truthy("section") {
				items = filterObjs(items, func(t *Obj) bool { return t.Get("section") == a.Get("section") })
			}
			sortObjs(items, func(x, y *Obj) float64 {
				if r := compareStr(x.Get("section"), y.Get("section")); r != 0 {
					return r
				}
				return compareStr(x.Get("template"), y.Get("template"))
			})
			res := filterAndLimit(items, a.Get("search"), or(a.Get("limit"), 200), func(t *Obj) []any {
				return []any{t.Get("template"), t.Get("package"), t.Get("os"), t.Get("headline")}
			})
			return js.Spread(js.O("node", node), res.Map(func(t *Obj) any {
				return proxmox.C("template", t.Get("template"), "section", t.Get("section"), "os", t.Get("os"), "version", t.Get("version"), "headline", t.Get("headline"), "type", t.Get("type"), "architecture", t.Get("architecture"))
			})), nil
		},
	})

	c.Define("proxmox_download_appliance_template", Def{
		Toolset:     "storage",
		Title:       "Download appliance template",
		Description: "Download an LXC template from the Proxmox appliance index onto a storage that allows vztmpl content. Template names come from proxmox_list_appliance_templates.",
		Input: []Prop{
			P("node", nodeArg),
			P("storage", storageArg),
			P("template", Str().D("Template name, e.g. \"debian-12-standard_12.7-1_amd64.tar.zst\"")),
			P("wait", waitArg),
		},
		Write:       true,
		Destructive: False,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			res, err := client.Post(ctx, "/nodes/"+enc(node)+"/aplinfo", js.O("storage", a.Get("storage"), "template", a.Get("template")))
			if err != nil {
				return nil, err
			}
			volid := js.Str(a.Get("storage")) + ":vztmpl/" + js.Str(a.Get("template"))
			return runTask(ctx, client, res, a.Get("wait"), js.O("node", node, "storage", a.Get("storage"), "volid", volid))
		},
	})

	// ---------------------------------------------------------------- stats and discovery

	c.Define("proxmox_get_storage_stats", Def{
		Toolset:     "storage",
		Title:       "Get storage usage history",
		Description: "Summarize a storage's usage over time from its RRD data (current, min, max, growth over the period) with a few sample points. Useful for capacity planning.",
		Input: []Prop{
			P("node", nodeArg),
			P("storage", storageArg),
			P("timeframe", Enum("hour", "day", "week", "month", "year").Opt().D("Period (default day)")),
			P("cf", Enum("AVERAGE", "MAX").Opt().D("Consolidation function (default AVERAGE)")),
			P("samples", Int().Min(0).Max(100).Opt().D("Number of evenly spaced sample points to include (default 12)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			timeframe := or(a.Get("timeframe"), "day")
			rows, err := client.GetList(ctx, "/nodes/"+enc(node)+"/storage/"+enc(a.Get("storage"))+"/rrddata", defined(js.O("timeframe", timeframe, "cf", a.Get("cf"))))
			if err != nil {
				return nil, err
			}
			pts := filterObjs(rows, func(r *Obj) bool { return js.IsFinite(r.Num("used")) && r.Present("used") })
			if len(pts) == 0 {
				return js.O("node", node, "storage", a.Get("storage"), "timeframe", timeframe, "points", 0, "note", "No usage data for this period"), nil
			}
			minUsed, maxUsed := math.Inf(1), math.Inf(-1)
			for _, p := range pts {
				u := p.Num("used")
				minUsed = math.Min(minUsed, u)
				maxUsed = math.Max(maxUsed, u)
			}
			first, last := pts[0], pts[len(pts)-1]
			n := int(math.Min(js.Num(or(a.Get("samples"), 12)), float64(len(pts))))
			step := 0.0
			if n > 1 {
				step = float64(len(pts)-1) / float64(n-1)
			}
			samples := make([]any, n)
			for i := range samples {
				p := pts[int(js.Round(float64(i)*step))]
				samples[i] = proxmox.C("time", proxmox.IsoTime(p.Get("time")), "used", proxmox.FormatBytes(p.Get("used")), "used_pct", proxmox.Percent(p.Get("used"), p.Get("total")))
			}
			growth := last.Num("used") - first.Num("used")
			sign := ""
			if growth >= 0 {
				sign = "+"
			}
			return proxmox.C(
				"node", node,
				"storage", a.Get("storage"),
				"timeframe", timeframe,
				"points", len(pts),
				"from", proxmox.IsoTime(first.Get("time")),
				"to", proxmox.IsoTime(last.Get("time")),
				"current", proxmox.Usage(last.Get("used"), last.Get("total")),
				"min_used", proxmox.FormatBytes(minUsed),
				"max_used", proxmox.FormatBytes(maxUsed),
				"growth", sign+js.Str(proxmox.FormatBytes(growth)),
				"samples", samples,
			), nil
		},
	})

	c.Define("proxmox_scan_storage", Def{
		Toolset:     "storage",
		Title:       "Scan for storage",
		Description: "Discover storage that can be added with proxmox_save_storage: NFS exports or CIFS shares on a server, PBS datastores, iSCSI targets on a portal, or local LVM volume groups, LVM thin pools and ZFS pools on a node. The nfs, cifs, pbs and iscsi scans make the node contact the given server, possibly with credentials, so they need write access (PROXMOX_ALLOW_WRITES=true); the local scans are always available.",
		Input: []Prop{
			P("node", nodeArg),
			P("type", Enum("nfs", "cifs", "pbs", "iscsi", "lvm", "lvmthin", "zfs").D("What to scan for")),
			P("server", Str().Opt().D("nfs/cifs/pbs: server name or IP")),
			P("username", Str().Opt().D("cifs/pbs: user name (pbs: user@realm or token ID; required for pbs)")),
			P("password", Str().Opt().D("cifs/pbs: password or token secret (required for pbs)")),
			P("domain", Str().Opt().D("cifs: SMB domain/workgroup")),
			P("fingerprint", Str().Opt().D("pbs: server certificate fingerprint")),
			P("port", Int().Opt().D("pbs: port (default 8007)")),
			P("portal", Str().Opt().D("iscsi: portal IP or name, optionally with :port")),
			P("vg", Str().Opt().D("lvmthin: volume group to list thin pools of")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			typ := a.Str("type")
			// Remote scans make the node connect to an arbitrary server (and send it credentials), so they count as writes.
			if storageRemoteScanTypes[typ] && !c.AllowWrites {
				return nil, fmt.Errorf("Scanning %s makes the node contact another server and needs write access (PROXMOX_ALLOW_WRITES=true); lvm, lvmthin and zfs scans work read-only", typ)
			}
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			need := func(fields ...string) error {
				for _, f := range fields {
					if !a.Defined(f) {
						return fmt.Errorf("Scanning %s requires `%s`", typ, f)
					}
				}
				return nil
			}
			params := js.NewObj()
			switch typ {
			case "nfs":
				err = need("server")
				params = js.O("server", a.Get("server"))
			case "cifs":
				err = need("server")
				params = defined(js.O("server", a.Get("server"), "username", a.Get("username"), "password", a.Get("password"), "domain", a.Get("domain")))
			case "pbs":
				err = need("server", "username", "password")
				params = defined(js.O("server", a.Get("server"), "username", a.Get("username"), "password", a.Get("password"), "fingerprint", a.Get("fingerprint"), "port", a.Get("port")))
			case "iscsi":
				err = need("portal")
				params = js.O("portal", a.Get("portal"))
			case "lvmthin":
				err = need("vg")
				params = js.O("vg", a.Get("vg"))
			}
			if err != nil {
				return nil, err
			}
			items, err := client.GetList(ctx, "/nodes/"+enc(node)+"/scan/"+typ, params)
			if err != nil {
				return nil, err
			}
			return js.O("node", node, "type", typ, "count", len(items), "items", mapObjs(items, func(i *Obj) any { return proxmox.Compact(i) })), nil
		},
	})

	// ---------------------------------------------------------------- physical disks

	c.Define("proxmox_list_disks", Def{
		Toolset:     "storage",
		Title:       "List physical disks",
		Description: "List a node's physical disks with device path, model, serial, size, type (ssd/hdd/nvme/usb), SMART health, wearout and what uses them (LVM, ZFS, Ceph OSD, partitions, mounted). Use type=unused to find disks free for new storage.",
		Input: []Prop{
			P("node", nodeArg),
			P("type", Enum("unused", "journal_disks").Opt().D("Only unused disks, or disks usable as Ceph journal/DB")),
			P("include_partitions", Bool().Opt().D("Also list partitions")),
			P("skip_smart", Bool().Opt().D("Skip SMART health checks (faster)")),
			P("raw", rawArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			disks, err := client.GetList(ctx, "/nodes/"+enc(node)+"/disks/list",
				defined(js.O("type", a.Get("type"), "include-partitions", a.Get("include_partitions"), "skipsmart", a.Get("skip_smart"))))
			if err != nil {
				return nil, err
			}
			sortObjs(disks, func(x, y *Obj) float64 { return compareStr(x.Get("devpath"), y.Get("devpath")) })
			var out any = js.Array(disks)
			if !a.Truthy("raw") {
				out = mapObjs(disks, storageSummarizeDisk)
			}
			return js.O("node", node, "count", len(disks), "disks", out), nil
		},
	})

	c.Define("proxmox_get_disk_smart", Def{
		Toolset:     "storage",
		Title:       "Get disk SMART data",
		Description: "Get a disk's SMART health and attributes (ATA attribute table, or the text report for NVMe/SAS).",
		Input: []Prop{
			P("node", nodeArg),
			P("disk", storageBlockArg),
			P("health_only", Bool().Opt().D("Only return the overall health status")),
			P("raw", rawArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			raw, err := client.Get(ctx, "/nodes/"+enc(node)+"/disks/smart", defined(js.O("disk", a.Get("disk"), "healthonly", a.Get("health_only"))))
			if err != nil {
				return nil, err
			}
			if a.Truthy("raw") {
				return raw, nil
			}
			s, _ := raw.(*Obj)
			var attrs any = js.Undef
			if arr, ok := s.Get("attributes").([]any); ok {
				attrs = mapObjs(js.Objs(arr), func(x *Obj) any {
					return proxmox.C("id", x.Get("id"), "name", x.Get("name"), "value", x.Get("value"), "worst", x.Get("worst"), "threshold", x.Get("threshold"), "raw", x.Get("raw"),
						"fail", ifThen(x.Truthy("fail") && x.Get("fail") != "-", x.Get("fail")))
				})
			}
			return proxmox.C("node", node, "disk", a.Get("disk"), "health", s.Get("health"), "type", s.Get("type"), "attributes", attrs, "text", s.Get("text")), nil
		},
	})

	c.Define("proxmox_list_zfs_pools", Def{
		Toolset:     "storage",
		Title:       "List ZFS pools",
		Description: "List a node's ZFS pools (zpools) with size, allocation, fragmentation, dedup ratio and health. Use proxmox_get_zfs_pool for the vdev tree and scrub status.",
		Input:       []Prop{P("node", nodeArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			pools, err := client.GetList(ctx, "/nodes/"+enc(node)+"/disks/zfs", nil)
			if err != nil {
				return nil, err
			}
			return js.O("node", node, "pools", mapObjs(pools, func(p *Obj) any {
				return proxmox.C("name", p.Get("name"), "health", p.Get("health"), "usage", proxmox.Usage(p.Get("alloc"), p.Get("size")),
					"free", proxmox.FormatBytes(p.Get("free")), "frag_pct", p.Get("frag"), "dedup", p.Get("dedup"))
			})), nil
		},
	})

	c.Define("proxmox_get_zfs_pool", Def{
		Toolset:     "storage",
		Title:       "Get ZFS pool status",
		Description: "Get a zpool's detailed status (like `zpool status`): state, status/action messages, last scrub/resilver, errors and the vdev tree with read/write/checksum error counters.",
		Input:       []Prop{P("node", nodeArg), P("name", Str().D("Pool name, e.g. \"rpool\""))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			p, err := client.GetObj(ctx, "/nodes/"+enc(node)+"/disks/zfs/"+enc(a.Get("name")), nil)
			if err != nil {
				return nil, err
			}
			var vdevs any = js.Undef
			if arr, ok := p.Get("children").([]any); ok {
				vdevs = mapObjs(js.Objs(arr), storageSummarizeVdev)
			}
			return proxmox.C("node", node, "name", p.Get("name"), "state", p.Get("state"), "status", p.Get("status"), "action", p.Get("action"),
				"scan", p.Get("scan"), "errors", p.Get("errors"), "vdevs", vdevs), nil
		},
	})

	c.Define("proxmox_list_lvm", Def{
		Toolset:     "storage",
		Title:       "List LVM volume groups",
		Description: "List a node's LVM volume groups with size, free space and physical volumes.",
		Input:       []Prop{P("node", nodeArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			res, err := client.Get(ctx, "/nodes/"+enc(node)+"/disks/lvm", nil)
			if err != nil {
				return nil, err
			}
			ro, _ := res.(*Obj)
			vgs := js.Objs(or(ro.Get("children"), []any{}))
			return js.O("node", node, "volume_groups", mapObjs(vgs, func(vg *Obj) any {
				return proxmox.C(
					"name", vg.Get("name"),
					"usage", proxmox.Usage(vg.Num("size")-vg.Num("free"), vg.Get("size")),
					"free", proxmox.FormatBytes(vg.Get("free")),
					"physical_volumes", mapObjs(js.Objs(or(vg.Get("children"), []any{})), func(pv *Obj) any {
						return proxmox.C("name", pv.Get("name"), "size", proxmox.FormatBytes(pv.Get("size")), "free", proxmox.FormatBytes(pv.Get("free")))
					}),
				)
			})), nil
		},
	})

	c.Define("proxmox_list_lvm_thin", Def{
		Toolset:     "storage",
		Title:       "List LVM thin pools",
		Description: "List a node's LVM thin pools with data and metadata usage. Thin pools that run out of metadata space fail badly, so watch metadata_usage.",
		Input:       []Prop{P("node", nodeArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			pools, err := client.GetList(ctx, "/nodes/"+enc(node)+"/disks/lvmthin", nil)
			if err != nil {
				return nil, err
			}
			return js.O("node", node, "thin_pools", mapObjs(pools, func(p *Obj) any {
				return proxmox.C("name", p.Get("lv"), "volume_group", p.Get("vg"), "usage", proxmox.Usage(p.Get("used"), p.Get("lv_size")),
					"metadata_usage", proxmox.Usage(p.Get("metadata_used"), p.Get("metadata_size")))
			})), nil
		},
	})

	c.Define("proxmox_list_directory_mounts", Def{
		Toolset:     "storage",
		Title:       "List directory mounts",
		Description: "List the directory storages Proxmox manages on a node's disks (filesystems mounted under /mnt/pve/<name> via systemd mount units).",
		Input:       []Prop{P("node", nodeArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			dirs, err := client.GetList(ctx, "/nodes/"+enc(node)+"/disks/directory", nil)
			if err != nil {
				return nil, err
			}
			return js.O("node", node, "directories", mapObjs(dirs, func(d *Obj) any {
				return proxmox.C("path", d.Get("path"), "device", d.Get("device"), "filesystem", d.Get("type"), "options", d.Get("options"), "unit_file", d.Get("unitfile"))
			})), nil
		},
	})

	c.Define("proxmox_create_disk_storage", Def{
		Toolset:     "storage",
		Title:       "Create storage on disks",
		Description: "Create local storage on unused disks of a node: a ZFS pool (one or more devices with a RAID level), an LVM volume group, an LVM thin pool, or a formatted directory mounted at /mnt/pve/<name>. ALL DATA ON THE DEVICES IS LOST. Find free disks with proxmox_list_disks type=unused. With add_storage the matching storage definition is added too.",
		Input: []Prop{
			P("node", nodeArg),
			P("kind", Enum("zfs", "lvm", "lvmthin", "directory").D("What to create")),
			P("name", Str().D("Name of the pool/VG/thin pool/directory (and of the storage when add_storage)")),
			P("devices", Arr(Str()).Min(1).D("Block devices, e.g. [\"/dev/sdb\"]; several only for zfs")),
			P("add_storage", Bool().Opt().D("Also add a storage definition for it (default false)")),
			P("raidlevel", Enum("single", "mirror", "raid10", "raidz", "raidz2", "raidz3", "draid", "draid2", "draid3").Opt().D("zfs: RAID level (default single for one device, else required)")),
			P("ashift", Int().Min(9).Max(16).Opt().D("zfs: sector size exponent (default 12)")),
			P("compression", Enum("on", "off", "gzip", "lz4", "lzjb", "zle", "zstd").Opt().D("zfs: compression (default on)")),
			P("draid_config", Str().Opt().D("zfs dRAID: \"data=<n>,spares=<n>\"")),
			P("filesystem", Enum("ext4", "xfs").Opt().D("directory: filesystem (default ext4)")),
			P("wait", waitArg),
		},
		Write:       true,
		Destructive: True,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			kind := a.Str("kind")
			devices := a.Arr("devices")
			var params *Obj
			if kind == "zfs" {
				raidlevel := or(a.Get("raidlevel"), ifThen(len(devices) == 1, "single"))
				if !js.Truthy(raidlevel) {
					return nil, errors.New("Pass `raidlevel` when creating a ZFS pool on several devices")
				}
				params = defined(js.O(
					"name", a.Get("name"),
					"devices", storageJoin(devices),
					"raidlevel", raidlevel,
					"ashift", a.Get("ashift"),
					"compression", a.Get("compression"),
					"draid-config", a.Get("draid_config"),
					"add_storage", a.Get("add_storage"),
				))
			} else {
				if len(devices) != 1 {
					return nil, fmt.Errorf("%s takes exactly one device", kind)
				}
				params = defined(js.O("name", a.Get("name"), "device", devices[0], "add_storage", a.Get("add_storage"), "filesystem", ifThen(kind == "directory", a.Get("filesystem"))))
			}
			res, err := client.Post(ctx, "/nodes/"+enc(node)+"/disks/"+kind, params)
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, res, a.Get("wait"), js.O("node", node, "kind", kind, "name", a.Get("name")))
		},
	})

	c.Define("proxmox_delete_disk_storage", Def{
		Toolset:     "storage",
		Title:       "Destroy storage on disks",
		Description: "Destroy a node-local ZFS pool, LVM volume group, LVM thin pool or managed directory mount. ALL DATA ON IT IS LOST. cleanup_config also removes/updates the storage definition; cleanup_disks wipes the disks for reuse.",
		Input: []Prop{
			P("node", nodeArg),
			P("kind", Enum("zfs", "lvm", "lvmthin", "directory").D("What to destroy")),
			P("name", Str().D("Pool/VG/thin pool/directory name")),
			P("volume_group", Str().Opt().D("lvmthin: the volume group the thin pool is in (required)")),
			P("cleanup_config", Bool().Opt().D("Remove the storage definition (or this node from it)")),
			P("cleanup_disks", Bool().Opt().D("Also wipe the underlying disks")),
			P("wait", waitArg),
		},
		Delete:      true,
		Destructive: True,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			kind := a.Str("kind")
			if kind == "lvmthin" && !a.Truthy("volume_group") {
				return nil, errors.New("Pass `volume_group` to delete an LVM thin pool")
			}
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			res, err := client.Delete(ctx, "/nodes/"+enc(node)+"/disks/"+kind+"/"+enc(a.Get("name")), defined(js.O(
				"cleanup-config", a.Get("cleanup_config"),
				"cleanup-disks", a.Get("cleanup_disks"),
				"volume-group", ifThen(kind == "lvmthin", a.Get("volume_group")),
			)))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, res, a.Get("wait"), js.O("node", node, "kind", kind, "name", a.Get("name"), "action", "delete"))
		},
	})

	c.Define("proxmox_init_disk_gpt", Def{
		Toolset:     "storage",
		Title:       "Initialize disk with GPT",
		Description: "Write a new, empty GPT partition table to a disk so it can be used (e.g. for Ceph or manual partitioning). Existing partitions are lost.",
		Input:       []Prop{P("node", nodeArg), P("disk", storageBlockArg), P("uuid", Str().Opt().D("UUID for the GPT table")), P("wait", waitArg)},
		// Destroys the disk's partition table (and so its data), like proxmox_wipe_disk.
		Delete:      true,
		Destructive: True,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			res, err := client.Post(ctx, "/nodes/"+enc(node)+"/disks/initgpt", defined(js.O("disk", a.Get("disk"), "uuid", a.Get("uuid"))))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, res, a.Get("wait"), js.O("node", node, "disk", a.Get("disk")))
		},
	})

	c.Define("proxmox_wipe_disk", Def{
		Toolset:     "storage",
		Title:       "Wipe disk",
		Description: "Wipe a disk or partition (filesystem/LVM/ZFS signatures and the start of the device) so it shows as unused. All data on it is lost. Refuses disks that are in use.",
		Input:       []Prop{P("node", nodeArg), P("disk", storageBlockArg), P("wait", waitArg)},
		Delete:      true,
		Destructive: True,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := c.node(ctx, a)
			if err != nil {
				return nil, err
			}
			res, err := client.Put(ctx, "/nodes/"+enc(node)+"/disks/wipedisk", js.O("disk", a.Get("disk")))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, res, a.Get("wait"), js.O("node", node, "disk", a.Get("disk")))
		},
	})
}
