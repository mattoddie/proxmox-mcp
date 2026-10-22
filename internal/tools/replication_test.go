package tools

import (
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

const replicationTestJobs = `[
  {"id":"100-0","guest":100,"jobnum":0,"target":"pve2","type":"local","schedule":"*/5","rate":50,"comment":"web"},
  {"id":"200-0","guest":200,"jobnum":0,"target":"pve2","type":"local","disable":1},
  {"id":"101-1","guest":101,"jobnum":1,"target":"pve1","type":"local","digest":"x"}
]`

func TestReplicationTools(t *testing.T) {
	env := startEnv(t)
	jobs := js.MustParse(replicationTestJobs).([]any)
	reset := func() {
		env.mock.Reset()
		env.mock.Reply("GET", `^/cluster/replication$`, replicationTestJobs)
		env.mock.Reply("GET", `^/nodes/pve1/replication$`, `[
			{"id":"100-0","guest":100,"last_sync":1700000000,"next_sync":1700000300,"duration":3.456,"fail_count":0},
			{"id":"200-0","guest":200,"last_try":1700000000,"fail_count":3,"error":"no space left"}]`)
		env.mock.Reply("GET", `^/nodes/pve2/replication$`, `[{"id":"101-1","guest":101,"pid":1234,"last_sync":1700000000}]`)
	}

	t.Run("lists jobs with status from each source node", func(t *testing.T) {
		reset()
		res := env.call(t, "proxmox_list_replication_jobs", nil)
		eqAt(t, res, "total", 3)
		eq(t, pluck(at(res, "items"), "id"), `["100-0","101-1","200-0"]`)
		eqAt(t, res, "items.0", `{
			"id":"100-0","guest":100,"target":"pve2","schedule":"*/5","rate_mbps":50,"comment":"web",
			"status":{"source":"pve1","state":"ok","last_sync":"2023-11-14T22:13:20.000Z","next_sync":"2023-11-14T22:18:20.000Z","duration_s":3.5}}`)
		db, dns := at(res, "items.1"), at(res, "items.2")
		eqAt(t, db, "schedule", "*/15")
		eqAt(t, db, "status.source", "pve2")
		eqAt(t, db, "status.state", "running")
		eqAt(t, dns, "disabled", true)
		eqAt(t, dns, "status.state", "error")
		eqAt(t, dns, "status.fail_count", 3)
		eqAt(t, dns, "status.error", "no space left")
	})

	t.Run("filters jobs by guest and target", func(t *testing.T) {
		reset()
		eq(t, pluck(at(env.call(t, "proxmox_list_replication_jobs", `{"guest":101}`), "items"), "id"), `["101-1"]`)
		eq(t, pluck(at(env.call(t, "proxmox_list_replication_jobs", `{"target":"pve2"}`), "items"), "id"), `["100-0","200-0"]`)
		eq(t, env.call(t, "proxmox_list_replication_jobs", `{"guest":9000}`), `{"total":0,"returned":0,"items":[]}`)
	})

	t.Run("gets a job with status from the guest's node", func(t *testing.T) {
		reset()
		env.mock.Reply("GET", `^/cluster/replication/101-1$`, js.Stringify(jobs[2], ""))
		env.mock.Reply("GET", `^/nodes/pve2/replication/101-1/status$`, `{"id":"101-1","last_sync":1700000000,"fail_count":1,"error":"timeout"}`)
		res := env.call(t, "proxmox_get_replication_job", `{"id":"101-1"}`)
		eqAt(t, res, "digest", "x")
		eqAt(t, res, "status.source", "pve2")
		eqAt(t, res, "status.state", "error")
		eqAt(t, res, "status.error", "timeout")
	})

	t.Run("still returns the config when the status lookup fails", func(t *testing.T) {
		reset()
		env.mock.Reply("GET", `^/cluster/replication/100-0$`, js.Stringify(jobs[0], ""))
		res := env.call(t, "proxmox_get_replication_job", `{"id":"100-0"}`)
		eqAt(t, res, "target", "pve2")
		match(t, at(res, "status_error"), `404`)
	})

	t.Run("validates job IDs", func(t *testing.T) {
		reset()
		if !env.callRaw(t, "proxmox_get_replication_job", `{"id":"abc"}`).IsError {
			t.Fatal("expected an error")
		}
	})

	t.Run("creates jobs with the next free job number", func(t *testing.T) {
		reset()
		res := env.call(t, "proxmox_save_replication_job", `{"guest":101,"target":"pve1","schedule":"*/30","rate":10}`)
		eq(t, res, `{"id":"101-2","action":"created","target":"pve1"}`)
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["POST","/cluster/replication"]`)
		eq(t, w.Body, `{"id":"101-2","type":"local","target":"pve1","schedule":"*/30","rate":"10"}`)

		first := env.call(t, "proxmox_save_replication_job", `{"guest":9000,"target":"pve2"}`)
		eqAt(t, first, "id", "9000-0")
		explicit := env.call(t, "proxmox_save_replication_job", `{"id":"100-3","target":"pve2"}`)
		eqAt(t, explicit, "action", "created")
		eqAt(t, env.mock.LastWrite().Body, "id", "100-3")
	})

	t.Run("updates existing jobs", func(t *testing.T) {
		reset()
		res := env.call(t, "proxmox_save_replication_job", `{"id":"100-0","disable":true,"delete":["rate"],"digest":"abc"}`)
		eqAt(t, res, "action", "updated")
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path}, `["PUT","/cluster/replication/100-0"]`)
		eq(t, w.Body, `{"disable":"1","delete":"rate","digest":"abc"}`)
	})

	t.Run("rejects invalid save requests", func(t *testing.T) {
		reset()
		match(t, env.errorText(t, "proxmox_save_replication_job", `{"id":"100-0","target":"pve3"}`), `can't be changed`)
		match(t, env.errorText(t, "proxmox_save_replication_job", `{"guest":100}`), "Pass `target`")
		match(t, env.errorText(t, "proxmox_save_replication_job", `{"target":"pve2"}`), "Pass `guest`")
		match(t, env.errorText(t, "proxmox_save_replication_job", `{"id":"100-9","guest":101,"target":"pve2"}`), `doesn't belong`)
	})

	t.Run("deletes jobs with keep/force", func(t *testing.T) {
		reset()
		res := env.call(t, "proxmox_delete_replication_job", `{"id":"100-0","keep":true}`)
		eqAt(t, res, "status", "marked for removal")
		w := env.mock.LastWrite()
		eq(t, []any{w.Method, w.Path, w.Query}, []any{"DELETE", "/cluster/replication/100-0", js.O("keep", "1")})
		forced := env.call(t, "proxmox_delete_replication_job", `{"id":"100-0","force":true}`)
		eqAt(t, forced, "status", "removed")
		eq(t, env.mock.LastWrite().Query, `{"force":"1"}`)
	})

	t.Run("schedules a job now on the source node", func(t *testing.T) {
		reset()
		env.mock.Reply("POST", `schedule_now$`, "100-0")
		res := env.call(t, "proxmox_run_replication_now", `{"id":"100-0"}`)
		eqAt(t, res, "node", "pve1")
		eqAt(t, res, "status", "scheduled")
		eqStr(t, env.mock.LastWrite().Path, "/nodes/pve1/replication/100-0/schedule_now")

		upid := env.mock.Upid("pve2", "replicate", "101-1")
		env.mock.Reply("POST", `schedule_now$`, upid)
		task := env.call(t, "proxmox_run_replication_now", `{"id":"101-1","node":"pve2"}`)
		eqAt(t, task, "status", "ok")
	})

	t.Run("reads the job log", func(t *testing.T) {
		reset()
		env.mock.On("GET", `^/nodes/pve1/replication/100-0/log$`, func(*Obj, []string, proxmoxtest.Request) any {
			return proxmoxtest.WithTotal{Data: []any{js.O("n", 6, "t", "start replication job"), js.O("n", 7, "t", "end replication job")}, Total: 7}
		})
		res := env.call(t, "proxmox_get_replication_log", `{"id":"100-0","start":5,"limit":2}`)
		eq(t, env.mock.RequestsTo(`^.*/log$`, "")[0].Query, `{"start":"5","limit":"2"}`)
		eq(t, res, `{"id":"100-0","node":"pve1","total_lines":7,"lines":["start replication job","end replication job"]}`)
		match(t, env.errorText(t, "proxmox_get_replication_log", `{"id":"555-0"}`), `No VM or container with ID 555`)
	})
}
