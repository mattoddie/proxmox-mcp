package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

var (
	replicationJobIDArg = Str().Pattern(`^[1-9][0-9]{2,8}-\d{1,9}$`, "Job IDs look like <VMID>-<number>, e.g. 100-0").
				D("Replication job ID: <guest VMID>-<job number>, e.g. \"100-0\" (see proxmox_list_replication_jobs)")

	replicationJobNodeArg = Str().Opt().D("Node the guest currently runs on (the replication source). Looked up from the guest when omitted.")
)

// replicationSummarizeStatus summarizes the status fields from /nodes/{node}/replication.
func replicationSummarizeStatus(s *Obj) any {
	if s == nil {
		return js.Undef
	}
	var state string
	switch {
	case s.Truthy("pid"):
		state = "running"
	case s.Truthy("error"):
		state = "error"
	case s.Num("fail_count") > 0:
		state = "failing"
	case s.Truthy("last_sync"):
		state = "ok"
	default:
		state = "pending"
	}
	return proxmox.C(
		"source", s.Get("source"),
		"state", state,
		"last_sync", proxmox.IsoTime(s.Get("last_sync")),
		"last_try", proxmox.IsoTime(s.Get("last_try")),
		"next_sync", proxmox.IsoTime(s.Get("next_sync")),
		"duration_s", ifThen(s.Defined("duration"), js.Round(s.Num("duration")*10)/10),
		"fail_count", orUndef(s.Num("fail_count")),
		"error", s.Get("error"),
	)
}

// replicationGuestOf is Number(j.guest ?? String(j.id).split("-")[0]).
func replicationGuestOf(j *Obj) float64 {
	return js.Num(or(j.Get("guest"), strings.Split(js.Str(j.Get("id")), "-")[0]))
}

func replicationSummarizeJob(j *Obj, status *Obj) *Obj {
	guest := js.Num(strings.Split(js.Str(j.Get("id")), "-")[0])
	if j.Defined("guest") {
		guest = j.Num("guest")
	}
	return proxmox.C(
		"id", j.Get("id"),
		"guest", guest,
		"target", j.Get("target"),
		"schedule", or(j.Get("schedule"), "*/15"),
		"rate_mbps", ifThen(j.Defined("rate"), j.Num("rate")),
		"disabled", orUndef(proxmox.Flag(j.Get("disable"))),
		"remove_job", j.Get("remove_job"),
		"comment", j.Get("comment"),
		"status", replicationSummarizeStatus(status),
	)
}

// replicationGuestNodes maps VMID to node from /cluster/resources.
func replicationGuestNodes(ctx context.Context, c *Context) map[float64]string {
	out := map[float64]string{}
	res, err := c.Client.GetList(ctx, "/cluster/resources", js.O("type", "vm"))
	if err != nil {
		return out
	}
	for _, r := range res {
		out[r.Num("vmid")] = js.Str(r.Get("node"))
	}
	return out
}

func replicationSourceNode(ctx context.Context, c *Context, id, node string) (string, error) {
	if node != "" {
		return node, nil
	}
	vmid := js.Num(strings.Split(id, "-")[0])
	g, err := c.Client.ResolveGuest(ctx, vmid, "", "")
	if err != nil {
		return "", err
	}
	return g.Node, nil
}

// replicationCompareNumeric is a.localeCompare(b, undefined, { numeric: true }): digit runs
// compare by value.
func replicationCompareNumeric(a, b string) float64 {
	split := func(s string) []string {
		var parts []string
		for i := 0; i < len(s); {
			j := i
			digit := s[i] >= '0' && s[i] <= '9'
			for j < len(s) && (s[j] >= '0' && s[j] <= '9') == digit {
				j++
			}
			parts = append(parts, s[i:j])
			i = j
		}
		return parts
	}
	pa, pb := split(a), split(b)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, y := pa[i], pb[i]
		if x == y {
			continue
		}
		if x[0] >= '0' && x[0] <= '9' && y[0] >= '0' && y[0] <= '9' {
			nx, _ := new(big.Int).SetString(x, 10)
			ny, _ := new(big.Int).SetString(y, 10)
			if c := nx.Cmp(ny); c != 0 {
				return float64(c)
			}
			continue
		}
		if c := js.LocaleCompare(x, y); c != 0 {
			return float64(c)
		}
	}
	return float64(len(pa) - len(pb))
}

func registerReplication(c *Context) {
	client := c.Client

	c.Define("proxmox_list_replication_jobs", Def{
		Toolset:     "replication",
		List:        &listing{},
		Title:       "List replication jobs",
		Description: "List storage replication jobs (ZFS-based guest replication to another node) with guest, target, schedule, rate limit and live status from the source node: last/next sync, duration, failure count and last error.",
		Input: []Prop{
			P("guest", Int().Opt().D("Only jobs for this guest VMID")),
			P("target", Str().Opt().D("Only jobs replicating to this node")),
			P("raw", rawArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			jobs, err := client.GetList(ctx, "/cluster/replication", nil)
			if err != nil {
				return nil, err
			}
			if a.Defined("guest") {
				jobs = filterObjs(jobs, func(j *Obj) bool { return replicationGuestOf(j) == a.Num("guest") })
			}
			if a.Truthy("target") {
				jobs = filterObjs(jobs, func(j *Obj) bool { return j.Get("target") == a.Get("target") })
			}
			sortObjs(jobs, func(x, y *Obj) float64 {
				return replicationCompareNumeric(js.Str(x.Get("id")), js.Str(y.Get("id")))
			})
			if len(jobs) == 0 {
				return js.O("total", 0, "items", []any{}), nil
			}

			// Status lives on the node the guest currently runs on.
			nodes := replicationGuestNodes(ctx, c)
			sourceOf := func(j *Obj) any {
				if n, ok := nodes[replicationGuestOf(j)]; ok {
					return n
				}
				return j.Get("source")
			}
			var unique []string
			seen := map[string]bool{}
			for _, j := range jobs {
				s := sourceOf(j)
				if !js.Truthy(s) {
					continue
				}
				n := js.Str(s)
				if !seen[n] {
					seen[n] = true
					unique = append(unique, n)
				}
			}
			status := map[string]*Obj{}
			for _, n := range unique {
				list, err := client.GetList(ctx, "/nodes/"+enc(n)+"/replication", nil)
				if err != nil {
					continue
				}
				for _, s := range list {
					status[js.Str(s.Get("id"))] = js.Spread(js.O("source", n), s)
				}
			}
			lookup := func(j *Obj) *Obj {
				if id, ok := j.Get("id").(string); ok {
					return status[id]
				}
				return nil
			}
			return js.O(
				"total", len(jobs),
				"items", mapObjs(jobs, func(j *Obj) any {
					if a.Truthy("raw") {
						st := any(js.Undef)
						if s := lookup(j); s != nil {
							st = s
						}
						return js.Spread(j, js.O("status", st))
					}
					return replicationSummarizeJob(j, lookup(j))
				}),
			), nil
		},
	})

	c.Define("proxmox_get_replication_job", Def{
		Toolset:     "replication",
		Title:       "Get replication job",
		Description: "Get one replication job's configuration (with digest for safe updates) and its current status on the source node.",
		Input:       []Prop{P("id", replicationJobIDArg), P("node", replicationJobNodeArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			id := a.Str("id")
			job, err := client.GetObj(ctx, "/cluster/replication/"+enc(id), nil)
			if err != nil {
				return nil, err
			}
			var status *Obj
			statusError := any(js.Undef)
			node, err := replicationSourceNode(ctx, c, id, a.Str("node"))
			if err == nil {
				var st *Obj
				st, err = client.GetObj(ctx, "/nodes/"+enc(node)+"/replication/"+enc(id)+"/status", nil)
				if err == nil {
					status = js.Spread(js.O("source", node), st)
				}
			}
			if err != nil {
				statusError = errMessage(err)
			}
			return proxmox.Compact(js.Spread(replicationSummarizeJob(job, status), js.O("digest", job.Get("digest"), "status_error", statusError))), nil
		},
	})

	c.Define("proxmox_save_replication_job", Def{
		Toolset:     "replication",
		Title:       "Create or update replication job",
		Description: "Create a replication job for a guest (pass `guest` and `target`; the job number is picked automatically) or update an existing one (pass `id`). Replication needs the guest's disks on ZFS storage that exists on both nodes. Schedules use systemd calendar syntax, e.g. \"*/15\" (every 15 min, default), \"*/5\", \"hourly\", \"22:30\".",
		Input: []Prop{
			P("id", replicationJobIDArg.Opt().D("Job ID to update, or to create with a specific job number, e.g. \"100-0\"")),
			P("guest", Int().Min(100).Opt().D("Guest VMID (to create a job without picking the job number)")),
			P("target", Str().Opt().D("Target node (required when creating; can't be changed afterwards)")),
			P("schedule", Str().Opt().D("Schedule in systemd calendar-event format (default \"*/15\")")),
			P("rate", Num().Pos().Opt().D("Bandwidth limit in MB/s")),
			P("comment", Str().Opt().D("Description")),
			P("disable", Bool().Opt().D("Disable (true) or enable (false) the job")),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("extra", extraArg),
		},
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			jobs, err := client.GetList(ctx, "/cluster/replication", nil)
			if err != nil {
				return nil, err
			}
			params, err := definedExtra(js.O("schedule", a.Get("schedule"), "rate", a.Get("rate"), "comment", a.Get("comment"), "disable", a.Get("disable")), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			var existing *Obj
			if a.Truthy("id") {
				for _, j := range jobs {
					if j.Get("id") == a.Get("id") {
						existing = j
						break
					}
				}
			}
			if existing != nil {
				if a.Truthy("target") && a.Get("target") != existing.Get("target") {
					return nil, fmt.Errorf("Job %s replicates to %s; the target can't be changed (create a new job)", js.Str(a.Get("id")), js.Str(existing.Get("target")))
				}
				if _, err := client.Put(ctx, "/cluster/replication/"+enc(a.Get("id")), js.Spread(params, js.O("delete", deleteParam(a.Get("delete")), "digest", a.Get("digest")))); err != nil {
					return nil, err
				}
				return js.O("id", a.Get("id"), "action", "updated"), nil
			}
			if !a.Truthy("target") {
				if a.Truthy("id") {
					return nil, fmt.Errorf("No job %s; pass `target` to create it", js.Str(a.Get("id")))
				}
				return nil, errors.New("Pass `target` (and `guest`) to create a job")
			}
			id := a.Str("id")
			if id == "" {
				if !a.Defined("guest") {
					return nil, errors.New("Pass `guest` (or a full `id`) to create a job")
				}
				guest := js.Str(a.Get("guest"))
				next := 0.0
				var used []float64
				for _, j := range jobs {
					jid := js.Str(j.Get("id"))
					if strings.HasPrefix(jid, guest+"-") {
						parts := strings.Split(jid, "-")
						if len(parts) > 1 {
							used = append(used, js.Num(parts[1]))
						} else {
							used = append(used, math.NaN())
						}
					}
				}
				if len(used) > 0 {
					m := math.Inf(-1)
					for _, u := range used {
						m = math.Max(m, u)
					}
					next = m + 1
				}
				id = guest + "-" + js.NumStr(next)
			} else if a.Defined("guest") && !strings.HasPrefix(id, js.Str(a.Get("guest"))+"-") {
				return nil, fmt.Errorf("Job ID %s doesn't belong to guest %s", id, js.Str(a.Get("guest")))
			}
			if _, err := client.Post(ctx, "/cluster/replication", js.Spread(js.O("id", id, "type", "local", "target", a.Get("target")), params)); err != nil {
				return nil, err
			}
			return js.O("id", id, "action", "created", "target", a.Get("target")), nil
		},
	})

	c.Define("proxmox_delete_replication_job", Def{
		Toolset:     "replication",
		Title:       "Delete replication job",
		Description: "Mark a replication job for removal. The job then removes its replication snapshots (and, unless keep is set, the replicated volumes on the target) and deletes itself on its next run. force removes only the config entry without cleanup.",
		Input: []Prop{
			P("id", replicationJobIDArg),
			P("keep", Bool().Opt().D("Keep the replicated data on the target node")),
			P("force", Bool().Opt().D("Remove the job config immediately without any cleanup")),
		},
		Delete: true,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/cluster/replication/"+enc(a.Get("id")), defined(js.O("keep", a.Get("keep"), "force", a.Get("force")))); err != nil {
				return nil, err
			}
			return proxmox.C("id", a.Get("id"), "status", pick(a.Truthy("force"), "removed", "marked for removal"), "keep", a.Get("keep")), nil
		},
	})

	c.Define("proxmox_run_replication_now", Def{
		Toolset:     "replication",
		Title:       "Run replication now",
		Description: "Schedule a replication job to run as soon as possible instead of waiting for its schedule. Check the result with proxmox_get_replication_job or proxmox_get_replication_log.",
		Input:       []Prop{P("id", replicationJobIDArg), P("node", replicationJobNodeArg), P("wait", waitArg)},
		Write:       true,
		Destructive: False,
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := replicationSourceNode(ctx, c, a.Str("id"), a.Str("node"))
			if err != nil {
				return nil, err
			}
			res, err := client.Post(ctx, "/nodes/"+enc(node)+"/replication/"+enc(a.Get("id"))+"/schedule_now", nil)
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, res, a.Get("wait"), js.O("id", a.Get("id"), "node", node, "status", "scheduled"))
		},
	})

	c.Define("proxmox_get_replication_log", Def{
		Toolset:     "replication",
		Title:       "Get replication log",
		Description: "Read the log of a replication job's last run from the source node, e.g. to diagnose a failing job.",
		Input: []Prop{
			P("id", replicationJobIDArg),
			P("node", replicationJobNodeArg),
			P("start", Int().Min(0).Opt().D("First line to return (default 0)")),
			P("limit", Int().Pos().Max(5000).Opt().D("Maximum lines (default 500)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			node, err := replicationSourceNode(ctx, c, a.Str("id"), a.Str("node"))
			if err != nil {
				return nil, err
			}
			lines, meta, err := client.RequestMeta(ctx, "GET", "/nodes/"+enc(node)+"/replication/"+enc(a.Get("id"))+"/log", js.O("start", or(a.Get("start"), 0), "limit", or(a.Get("limit"), 500)))
			if err != nil {
				return nil, err
			}
			out := []any{}
			for _, l := range js.Objs(lines) {
				if t := js.Str(or(l.Get("t"), "")); t != "no content" {
					out = append(out, t)
				}
			}
			return proxmox.C("id", a.Get("id"), "node", node, "total_lines", meta.Get("total"), "lines", out), nil
		},
	})
}
