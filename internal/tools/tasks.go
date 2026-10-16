package tools

import (
	"context"
	"math"
	"strings"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

var taskUpidArg = Str().D("Task ID (UPID), e.g. UPID:pve1:000A1B2C:0123ABCD:6701F3A2:qmstart:100:root@pam:")

func taskSummarize(t *Obj) any {
	start := t.Num("starttime")
	end := t.Num("endtime")
	status := t.Get("status")
	if js.Nullish(status) {
		status = ifThen(!t.Truthy("endtime"), "running")
	}
	return proxmox.C(
		"upid", t.Get("upid"),
		"node", t.Get("node"),
		"type", t.Get("type"),
		"id", orUndef(t.Get("id")),
		"user", t.Get("user"),
		"status", status,
		"starttime", proxmox.IsoTime(start),
		"endtime", proxmox.IsoTime(end),
		"duration_s", ifThen(js.IsFinite(end) && end >= start, end-start),
	)
}

func registerTasks(c *Context) {
	client := c.Client

	c.Define("proxmox_list_tasks", Def{
		Toolset:     "tasks",
		Title:       "List tasks",
		Description: "List Proxmox tasks (backups, migrations, starts, clones...). Without `node`, lists recent tasks across the whole cluster; with `node`, reads that node's task history with server-side filters. Use errors=true to find failures.",
		Input: []Prop{
			P("node", Str().Opt().D("Only this node, with full history and server-side filters (default: recent tasks cluster-wide)")),
			P("vmid", Int().Opt().D("Only tasks for this VM/container")),
			P("type", Str().Opt().D("Only this task type, e.g. vzdump, qmstart, qmigrate, vzcreate, qmclone")),
			P("user", Str().Opt().D("Only tasks started by this user, e.g. root@pam")),
			P("errors", Bool().Opt().D("Only failed tasks")),
			P("running", Bool().Opt().D("Only tasks that are still running")),
			P("since", Int().Opt().D("Only tasks started after this UNIX time (node only)")),
			P("search", searchArg),
			P("limit", limitArg(50)),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			var tasks []*Obj
			var err error
			if a.Truthy("node") {
				tasks, err = client.GetList(ctx, "/nodes/"+enc(a.Get("node"))+"/tasks", defined(js.O(
					"vmid", a.Get("vmid"),
					"typefilter", a.Get("type"),
					"userfilter", a.Get("user"),
					"errors", a.Get("errors"),
					"since", a.Get("since"),
					"source", pick(a.Truthy("running"), "active", "all"),
					"limit", or(a.Get("limit"), 50),
				)))
				if err != nil {
					return nil, err
				}
			} else {
				if tasks, err = client.GetList(ctx, "/cluster/tasks", nil); err != nil {
					return nil, err
				}
				if a.Defined("vmid") {
					tasks = filterObjs(tasks, func(t *Obj) bool { return js.Str(t.Get("id")) == js.Str(a.Get("vmid")) })
				}
				if a.Truthy("type") {
					tasks = filterObjs(tasks, func(t *Obj) bool { return t.Get("type") == a.Get("type") })
				}
				if a.Truthy("user") {
					tasks = filterObjs(tasks, func(t *Obj) bool { return t.Get("user") == a.Get("user") })
				}
				if a.Truthy("errors") {
					tasks = filterObjs(tasks, func(t *Obj) bool {
						return t.Truthy("endtime") && t.Truthy("status") && t.Get("status") != "OK" && !strings.HasPrefix(js.Str(t.Get("status")), "WARNINGS")
					})
				}
			}
			if a.Truthy("running") {
				tasks = filterObjs(tasks, func(t *Obj) bool { return !t.Truthy("endtime") })
			}
			sortObjs(tasks, func(x, y *Obj) float64 { return y.Num("starttime") - x.Num("starttime") })
			res := filterAndLimit(tasks, a.Get("search"), or(a.Get("limit"), 50), func(t *Obj) []any {
				return []any{t.Get("type"), t.Get("id"), t.Get("user"), t.Get("status"), t.Get("node")}
			})
			return res.Map(taskSummarize), nil
		},
	})

	c.Define("proxmox_get_task", Def{
		Toolset:     "tasks",
		Title:       "Get task status",
		Description: "Get a task's status (running/stopped), exit status and the last lines of its log.",
		Input: []Prop{
			P("upid", taskUpidArg),
			P("log_lines", Int().Min(0).Max(1000).Opt().D("How many trailing log lines to include (default 30)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			return client.GetTask(ctx, a.Str("upid"), int(js.Num(or(a.Get("log_lines"), 30))))
		},
	})

	c.Define("proxmox_get_task_log", Def{
		Toolset:     "tasks",
		Title:       "Read task log",
		Description: "Read a task's log, paging from the start (or the end with tail=true).",
		Input: []Prop{
			P("upid", taskUpidArg),
			P("start", Int().Min(0).Opt().D("First line (0-based, default 0)")),
			P("limit", Int().Pos().Max(5000).Opt().D("Number of lines (default 500)")),
			P("tail", Bool().Opt().D("Return the last `limit` lines instead")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			u, err := proxmox.ParseUpid(a.Str("upid"))
			if err != nil {
				return nil, err
			}
			path := "/nodes/" + enc(u.Node) + "/tasks/" + enc(a.Get("upid")) + "/log"
			limit := js.Num(or(a.Get("limit"), 500))
			start := js.Num(or(a.Get("start"), 0))
			if a.Truthy("tail") {
				_, meta, err := client.RequestMeta(ctx, "GET", path, js.O("start", 0, "limit", 1))
				if err != nil {
					return nil, err
				}
				total := js.Num(meta.Or("total", 0))
				start = math.Max(0, total-limit)
			}
			lines, meta, err := client.RequestMeta(ctx, "GET", path, js.O("start", start, "limit", limit))
			if err != nil {
				return nil, err
			}
			return js.O("start", start, "total", meta.Get("total"), "lines", mapObjs(js.Objs(lines), func(l *Obj) any { return l.Get("t") })), nil
		},
	})

	c.Define("proxmox_wait_for_task", Def{
		Toolset:     "tasks",
		Title:       "Wait for a task",
		Description: "Wait until a running task finishes (or the timeout passes) and return its outcome and log tail.",
		Input: []Prop{
			P("upid", taskUpidArg),
			P("timeout_s", Int().Pos().Max(3600).Opt().D("How long to wait in seconds (default PROXMOX_TASK_TIMEOUT_MS)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			return client.WaitForTask(ctx, a.Str("upid"), proxmox.WaitOptions{TimeoutMs: int(a.Num("timeout_s")*1000) * boolInt(a.Truthy("timeout_s"))})
		},
	})

	c.Define("proxmox_stop_task", Def{
		Toolset:     "tasks",
		Title:       "Stop a task",
		Description: "Abort a running task (e.g. a stuck backup or migration).",
		Write:       true,
		Destructive: True,
		Input:       []Prop{P("upid", taskUpidArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			u, err := proxmox.ParseUpid(a.Str("upid"))
			if err != nil {
				return nil, err
			}
			if _, err := client.Delete(ctx, "/nodes/"+enc(u.Node)+"/tasks/"+enc(a.Get("upid")), nil); err != nil {
				return nil, err
			}
			return js.O("stopped", a.Get("upid")), nil
		},
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
