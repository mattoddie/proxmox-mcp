package tools

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

// capacityRatio is allocated/physical rounded to two decimals (undefined without a physical size).
func capacityRatio(allocated, physical float64) any {
	if physical <= 0 {
		return js.Undef
	}
	return js.Round(allocated/physical*100) / 100
}

type capacityGuests struct {
	running, total     int
	cpuRunning, cpuAll float64
	memRunning, memAll float64
}

// defineCapacityReport registers proxmox_get_capacity_report (cluster toolset).
func defineCapacityReport(c *Context) {
	client := c.Client
	c.Define("proxmox_get_capacity_report", Def{
		Toolset:     "cluster",
		Title:       "Get capacity and overcommit report",
		Description: "Compare what guests are allocated with the physical capacity, per node and for the cluster: vCPUs against the node's CPU threads, configured guest memory against the node's RAM, and (unless storage=false) the provisioned size of VM and container disks on each storage against its size, which shows thin-provisioning overcommit. Each figure is given for running guests and for all guests (as if every guest were started); templates are left out. A ratio above 1 means overcommitted. Read-only. The storage part lists the disk volumes on each storage, so it takes longer on big clusters.",
		Input: []Prop{
			P("node", Str().Opt().D("Only this node (and its storage)")),
			P("storage", Bool().Opt().D("Include storage allocation (default true)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			resources, err := client.GetList(ctx, "/cluster/resources", nil)
			if err != nil {
				return nil, err
			}
			only := a.Str("node")
			guests := map[string]*capacityGuests{}
			var nodes, storages []*Obj
			for _, r := range resources {
				node := js.Str(r.Get("node"))
				if only != "" && node != only {
					continue
				}
				switch r.Get("type") {
				case "node":
					nodes = append(nodes, r)
				case "storage":
					storages = append(storages, r)
				case "qemu", "lxc":
					if r.Truthy("template") {
						continue
					}
					g := guests[node]
					if g == nil {
						g = &capacityGuests{}
						guests[node] = g
					}
					g.total++
					g.cpuAll += capNum(r, "maxcpu")
					g.memAll += capNum(r, "maxmem")
					if r.Get("status") == "running" {
						g.running++
						g.cpuRunning += capNum(r, "maxcpu")
						g.memRunning += capNum(r, "maxmem")
					}
				}
			}
			if only != "" && len(nodes) == 0 {
				return nil, fmt.Errorf("Node %s not found in the cluster (see proxmox_list_nodes)", js.Quote(only))
			}
			sort.SliceStable(nodes, func(i, j int) bool { return js.Str(nodes[i].Get("node")) < js.Str(nodes[j].Get("node")) })
			total := &capacityGuests{}
			var cores, mem, memUsed float64
			nodeOut := []any{}
			for _, n := range nodes {
				g := guests[js.Str(n.Get("node"))]
				if g == nil {
					g = &capacityGuests{}
				}
				total.running += g.running
				total.total += g.total
				total.cpuRunning += g.cpuRunning
				total.cpuAll += g.cpuAll
				total.memRunning += g.memRunning
				total.memAll += g.memAll
				cores += capNum(n, "maxcpu")
				mem += capNum(n, "maxmem")
				memUsed += capNum(n, "mem")
				nodeOut = append(nodeOut, proxmox.C(
					"node", n.Get("node"),
					"status", n.Get("status"),
					"guests", js.O("running", g.running, "total", g.total),
					"cpu", capacityCPU(capNum(n, "maxcpu"), g),
					"memory", capacityMemory(capNum(n, "maxmem"), capNum(n, "mem"), g),
				))
			}
			out := js.O(
				"nodes", nodeOut,
				"totals", proxmox.C(
					"guests", js.O("running", total.running, "total", total.total),
					"cpu", capacityCPU(cores, total),
					"memory", capacityMemory(mem, memUsed, total),
				),
			)
			if a.Get("storage") != false {
				out.Set("storage", capacityStorage(ctx, client, storages, nodes))
			}
			return out, nil
		},
	})
}

func capacityCPU(threads float64, g *capacityGuests) *Obj {
	return proxmox.C(
		"threads", ifThen(threads > 0, threads),
		"vcpus_running", g.cpuRunning,
		"vcpus_allocated", g.cpuAll,
		"ratio_running", capacityRatio(g.cpuRunning, threads),
		"ratio_allocated", capacityRatio(g.cpuAll, threads),
	)
}

func capacityMemory(physical, used float64, g *capacityGuests) *Obj {
	return proxmox.C(
		"physical", ifThen(physical > 0, proxmox.FormatBytes(physical)),
		"used", ifThen(physical > 0, proxmox.FormatBytes(used)),
		"allocated_running", proxmox.FormatBytes(g.memRunning),
		"allocated", proxmox.FormatBytes(g.memAll),
		"ratio_running", capacityRatio(g.memRunning, physical),
		"ratio_allocated", capacityRatio(g.memAll, physical),
	)
}

// capacityStorage sums the provisioned size of the guest disk volumes on each storage that holds
// them. Shared storage is listed once, through the first online node that has it.
func capacityStorage(ctx context.Context, client *proxmox.Client, storages, nodes []*Obj) []any {
	online := map[string]bool{}
	for _, n := range nodes {
		online[js.Str(n.Get("node"))] = n.Get("status") == "online"
	}
	seenShared := map[string]bool{}
	out := []any{}
	for _, s := range storages {
		content := storageSplitList(s.Get("content"))
		var kinds []string
		for _, k := range []string{"images", "rootdir"} {
			if storageListIncludes(content, k) {
				kinds = append(kinds, k)
			}
		}
		node := js.Str(s.Get("node"))
		id := js.Str(s.Get("storage"))
		shared := s.Truthy("shared")
		if len(kinds) == 0 || s.Get("status") != "available" || !online[node] || (shared && seenShared[id]) {
			continue
		}
		if shared {
			seenShared[id] = true
		}
		var allocated float64
		volumes := 0
		var listErr error
		seen := map[string]bool{}
		for _, k := range kinds {
			vols, err := client.GetList(ctx, "/nodes/"+enc(node)+"/storage/"+enc(id)+"/content", js.O("content", k))
			if err != nil {
				listErr = err
				break
			}
			for _, v := range vols {
				volid := js.Str(v.Get("volid"))
				if seen[volid] {
					continue
				}
				seen[volid] = true
				volumes++
				if v.Defined("size") {
					allocated += capNum(v, "size")
				} else {
					allocated += capNum(v, "approximate-size")
				}
			}
		}
		size, used := capNum(s, "maxdisk"), capNum(s, "disk")
		entry := proxmox.C(
			"storage", id,
			"node", ifThen(!shared, node),
			"shared", ifThen(shared, true),
			"type", s.Get("plugintype"),
			"size", proxmox.FormatBytes(size),
			"used", proxmox.FormatBytes(used),
			"used_pct", proxmox.Percent(used, size),
		)
		if listErr != nil {
			entry.Set("error", errMessage(listErr))
		} else {
			entry.Assign(proxmox.C(
				"volumes", volumes,
				"allocated", proxmox.FormatBytes(allocated),
				"ratio_allocated", capacityRatio(allocated, size),
				"overcommitted", ifThen(size > 0 && allocated > size, true),
			))
		}
		out = append(out, entry)
	}
	return out
}

// capNum reads a number field, treating a missing or non-numeric one as 0.
func capNum(o *Obj, k string) float64 {
	n := o.Num(k)
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0
	}
	return n
}
