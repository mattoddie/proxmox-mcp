package proxmox_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mattoddie/proxmox-mcp/internal/config"
	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

var tokenEnv = map[string]string{"PROXMOX_TOKEN_ID": proxmoxtest.TokenID, "PROXMOX_TOKEN_SECRET": proxmoxtest.TokenSecret}

func newClient(t *testing.T, env map[string]string) *proxmox.Client {
	t.Helper()
	cfg, err := config.Load(config.MapEnv(env))
	if err != nil {
		t.Fatal(err)
	}
	c, err := proxmox.New(cfg.Proxmox)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func with(base map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func mustMatch(t *testing.T, err error, re string) *proxmox.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error matching /%s/", re)
	}
	if !regexp.MustCompile(re).MatchString(err.Error()) {
		t.Fatalf("%q does not match /%s/", err.Error(), re)
	}
	var pe *proxmox.Error
	errors.As(err, &pe)
	return pe
}

func TestFormatHelpers(t *testing.T) {
	t.Run("formats and parses sizes", func(t *testing.T) {
		for in, want := range map[float64]string{1536: "1.50 KiB", 34359738368: "32 GiB", 0: "0 B", 1023: "1023 B", -2048: "-2 KiB", 1288490188.8: "1.20 GiB", 1181116006.4: "1.10 GiB", 107374182400: "100 GiB"} {
			if got := proxmox.FormatBytes(in); got != want {
				t.Errorf("FormatBytes(%v) = %v, want %s", in, got, want)
			}
		}
		if proxmox.FormatBytes("x") != js.Undef || proxmox.FormatBytes(nil) != js.Undef || proxmox.FormatBytes("") != js.Undef {
			t.Error("FormatBytes of non-numbers")
		}
		if proxmox.ParseSize("32G") != 34359738368.0 || proxmox.ParseSize("512M") != 536870912.0 || proxmox.ParseSize("1.5 GiB") != 1610612736.0 || proxmox.ParseSize("x") != js.Undef {
			t.Error("ParseSize")
		}
	})

	t.Run("formats uptime, percentages and times", func(t *testing.T) {
		if proxmox.FormatUptime(90061.0) != "1d 1h 1m" || proxmox.FormatUptime(3600.0) != "1h 0m" || proxmox.FormatUptime(59.0) != "59s" || proxmox.FormatUptime(0.0) != js.Undef {
			t.Error("FormatUptime")
		}
		if proxmox.Percent(1.0, 3.0) != 33.3 || proxmox.Percent(1.0, 0.0) != js.Undef || proxmox.FractionPct(0.031) != 3.1 || proxmox.FractionPct(nil) != js.Undef {
			t.Error("Percent")
		}
		if proxmox.IsoTime(1700000000.0) != "2023-11-14T22:13:20.000Z" || proxmox.IsoTime("0") != js.Undef {
			t.Error("IsoTime")
		}
		if proxmox.Usage(2147483648.0, 4294967296.0) != "2 GiB of 4 GiB (50%)" || proxmox.Usage(nil, 4294967296.0) != "? of 4 GiB (0%)" || proxmox.Usage(1.0, nil) != js.Undef {
			t.Error("Usage")
		}
	})

	t.Run("round-trips property strings", func(t *testing.T) {
		p := proxmox.ParsePropertyString("local-lvm:vm-100-disk-0,size=32G,ssd=1", "file")
		if js.Stringify(p, "") != `{"file":"local-lvm:vm-100-disk-0","size":"32G","ssd":"1"}` {
			t.Fatal(js.Stringify(p, ""))
		}
		if s := proxmox.ToPropertyString(p, "file"); s != "local-lvm:vm-100-disk-0,size=32G,ssd=1" {
			t.Fatal(s)
		}
		if s := proxmox.ToPropertyString(js.O("a", true, "b", false, "c", js.Undef, "d", nil, "e", 2), ""); s != "a=1,b=0,e=2" {
			t.Fatal(s)
		}
	})

	t.Run("splits VM NIC model and MAC", func(t *testing.T) {
		nets := proxmox.SummarizeNets(js.O("net0", "virtio=BC:24:11:00:00:01,bridge=vmbr0,tag=10"))
		if got := js.Stringify(nets.Get("net0"), ""); got != `{"bridge":"vmbr0","tag":"10","model":"virtio","macaddr":"BC:24:11:00:00:01"}` {
			t.Fatal(got)
		}
		disks := proxmox.SummarizeDisks(js.MustObj(proxmoxtest.CT200ConfigJSON))
		if got := js.Stringify(disks, ""); got != `{"rootfs":{"volume":"local-lvm:vm-200-disk-0","size":"8G"}}` {
			t.Fatal(got)
		}
	})

	t.Run("redacts secrets recursively", func(t *testing.T) {
		out := proxmox.RedactSecrets(js.MustObj(`{"cipassword":"x","nested":[{"password":"y","name":"ok"}],"token":"t","smtp-password":"z","keys":["a",{"secret":"s"}],"enabled":true,"password":""}`))
		if got := js.Stringify(out, ""); got != `{"cipassword":"<redacted>","nested":[{"password":"<redacted>","name":"ok"}],"token":"<redacted>","smtp-password":"<redacted>","keys":["a",{"secret":"<redacted>"}],"enabled":true,"password":""}` {
			t.Fatal(got)
		}
		out = proxmox.RedactSecrets(js.MustObj(`{"api-keys":["k1",{"x":1}],"private":{"a":1}}`))
		if got := js.Stringify(out, ""); got != `{"api-keys":["<redacted>",{"x":1}],"private":{"a":1}}` {
			t.Fatal(got)
		}
	})

	t.Run("redacts credentials inside URLs", func(t *testing.T) {
		out := proxmox.RedactSecrets(js.MustObj(`{"http_proxy":"http://bob:pw@proxy:3128","email":"a@b.c"}`))
		if got := js.Stringify(out, ""); got != `{"http_proxy":"http://bob:<redacted>@proxy:3128","email":"a@b.c"}` {
			t.Fatal(got)
		}
	})

	t.Run("redacts ACME plugin data as KEY=value lines", func(t *testing.T) {
		if got := js.Stringify(proxmox.RedactPluginData("Q0ZfVG9rZW49eApDRl9JRD15Cg=="), ""); got != `["CF_Token=<redacted>","CF_ID=<redacted>"]` {
			t.Fatal(got)
		}
		if got := js.Stringify(proxmox.RedactPluginData("AAEC"), ""); got != `["<redacted>"]` {
			t.Fatal(got)
		}
		if got := js.Stringify(proxmox.RedactPluginData("A=1\nnoequals"), ""); got != `["A=<redacted>","<redacted>"]` {
			t.Fatal(got)
		}
		out := proxmox.RedactSecrets(js.MustObj(`{"plugin":"cf","api":"cf","data":"Q0ZfVG9rZW49eA=="}`))
		if got := js.Stringify(out, ""); got != `{"plugin":"cf","api":"cf","data":"CF_Token=<redacted>"}` {
			t.Fatal(got)
		}
	})

	t.Run("normalises flags", func(t *testing.T) {
		if proxmox.Flag(1.0) != true || proxmox.Flag("0") != false || proxmox.Flag("on") != true || proxmox.Flag(nil) != js.Undef || proxmox.Flag("") != js.Undef {
			t.Error("Flag")
		}
	})

	t.Run("encodes params like the web UI", func(t *testing.T) {
		p := proxmox.EncodeParams(js.O("a", true, "b", false, "c", []any{1.0, 2.0}, "d", js.Undef, "e", "x y", "f", nil, "g", js.O("k", 1), "h", "~*'"))
		if got := proxmox.FormEncode(p); got != "a=1&b=0&c=1&c=2&e=x+y&g=%7B%22k%22%3A1%7D&h=%7E*%27" {
			t.Fatal(got)
		}
	})

	t.Run("parses UPIDs", func(t *testing.T) {
		u, err := proxmox.ParseUpid("UPID:pve1:0000ABCD:0001ABCD:65000001:qmstart:100:root@pam:")
		if err != nil || u.Node != "pve1" || u.Type != "qmstart" || u.ID != "100" || u.User != "root@pam" || u.StartTime != 0x65000001 {
			t.Fatalf("%+v %v", u, err)
		}
		_, err = proxmox.ParseUpid("nope")
		var pe *proxmox.Error
		if !errors.As(err, &pe) || pe.Message != "Not a task id (UPID): nope" {
			t.Fatal(err)
		}
		if !proxmox.IsUpid("UPID:pve1:0000ABCD:0001ABCD:65000001:qmstart:100:root@pam:") || proxmox.IsUpid(1.0) || proxmox.IsUpid("UPID:x") {
			t.Fatal("IsUpid")
		}
	})
}

func TestClientWithToken(t *testing.T) {
	mock := proxmoxtest.New(t, "token")
	client := newClient(t, with(tokenEnv, "PROXMOX_URL", mock.URL, "PROXMOX_TASK_TIMEOUT_MS", "2000"))
	ctx := context.Background()

	t.Run("sends the PVEAPIToken header and unwraps data", func(t *testing.T) {
		v, err := client.GetObj(ctx, "/version", nil)
		if err != nil || v.Get("version") != "9.0.10" {
			t.Fatal(v, err)
		}
		if got := mock.Last().Headers.Get("Authorization"); got != "PVEAPIToken="+proxmoxtest.TokenID+"="+proxmoxtest.TokenSecret {
			t.Fatal(got)
		}
		if got := mock.Last().Headers.Get("Accept"); got != "application/json" {
			t.Fatal(got)
		}
	})

	t.Run("sends GET params as query and POST params as a form body", func(t *testing.T) {
		if _, err := client.Get(ctx, "/cluster/resources", js.O("type", "vm")); err != nil {
			t.Fatal(err)
		}
		if got := js.Stringify(mock.Last().Query, ""); got != `{"type":"vm"}` {
			t.Fatal(got)
		}
		if _, err := client.Post(ctx, "/nodes/pve1/qemu/100/status/start", js.O("skiplock", true, "timeout", 30)); err != nil {
			t.Fatal(err)
		}
		req := mock.Last()
		if req.Headers.Get("Content-Type") != "application/x-www-form-urlencoded" || js.Stringify(req.Body, "") != `{"skiplock":"1","timeout":"30"}` {
			t.Fatal(req.Headers, js.Stringify(req.Body, ""))
		}
		if _, err := client.Delete(ctx, "/nodes/pve1/qemu/100", js.O("purge", true)); err != nil {
			t.Fatal(err)
		}
		if req := mock.Last(); req.Method != "DELETE" || js.Stringify(req.Query, "") != `{"purge":"1"}` || req.Body.Len() != 0 {
			t.Fatal(req)
		}
	})

	t.Run("surfaces the reason phrase and parameter errors", func(t *testing.T) {
		mock.On("POST", `^/nodes/pve1/qemu/100/config$`, func(*js.Obj, []string, proxmoxtest.Request) any {
			return proxmoxtest.Error(400, "Parameter verification failed.", map[string]string{"memory": "value must be at least 16"})
		})
		_, err := client.Post(ctx, "/nodes/pve1/qemu/100/config", js.O("memory", 1))
		pe := mustMatch(t, err, `^POST /nodes/pve1/qemu/100/config failed with HTTP 400: Parameter verification failed\. \(memory: value must be at least 16\)$`)
		if pe == nil || pe.Status != 400 {
			t.Fatal(pe)
		}
		_, err = client.Get(ctx, "/nope", nil)
		mustMatch(t, err, `^GET /nope failed with HTTP 404: no mock for GET /nope$`)
	})

	t.Run("explains permission errors", func(t *testing.T) {
		mock.On("GET", `^/access/users$`, func(*js.Obj, []string, proxmoxtest.Request) any {
			return proxmoxtest.Error(403, "Permission check failed (/access, User.Modify)")
		})
		_, err := client.Get(ctx, "/access/users", nil)
		mustMatch(t, err, `Permission check failed .*privilege separation`)
	})

	t.Run("resolves nodes and guests", func(t *testing.T) {
		_, err := client.ResolveNode(ctx, "")
		mustMatch(t, err, "has 2 nodes; pass `node` \\(one of: pve1, pve2\\)")
		if n, _ := client.ResolveNode(ctx, "pve2"); n != "pve2" {
			t.Fatal(n)
		}
		g, err := client.ResolveGuest(ctx, 101, "", "")
		if err != nil || g.Node != "pve2" || g.Type != "qemu" || g.Name != "db" {
			t.Fatal(g, err)
		}
		_, err = client.ResolveGuest(ctx, 200, "", "qemu")
		mustMatch(t, err, `^Guest 200 is a container, not a VM \(qemu\); use the proxmox_\*_container tools$`)
		_, err = client.ResolveGuest(ctx, 555, "", "")
		mustMatch(t, err, `No VM or container with ID 555`)
		if g, err := client.ResolveGuest(ctx, 555, "pve9", "lxc"); err != nil || g.Node != "pve9" {
			t.Fatal(g, err)
		}
		if p := client.GuestPath(proxmox.GuestRef{VMID: 100, Node: "pve 1", Type: "qemu"}); p != "/nodes/pve%201/qemu/100" {
			t.Fatal(p)
		}
	})

	t.Run("waits for tasks and reports their log", func(t *testing.T) {
		upid := mock.Upid("pve1", "qmstart", "100", proxmoxtest.Task{Status: "stopped", ExitStatus: "OK", Log: []string{"starting", "TASK OK"}})
		task, err := client.WaitForTask(ctx, upid, proxmox.WaitOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := js.Stringify(task, ""); !strings.Contains(got, `"type":"qmstart","id":"100","user":"root@pam","status":"stopped","exitstatus":"OK","ok":true,`) ||
			!strings.HasSuffix(got, `"duration_s":3,"log":["starting","TASK OK"]}`) {
			t.Fatal(got)
		}
	})

	t.Run("returns timedOut for tasks that keep running", func(t *testing.T) {
		upid := mock.Upid("pve1", "vzdump", "100", proxmoxtest.Task{Status: "running", Log: []string{"INFO: starting"}})
		task, err := client.WaitForTask(ctx, upid, proxmox.WaitOptions{TimeoutMs: 300, PollMs: 100})
		if err != nil || task.Get("timedOut") != true || task.Get("ok") != false || task.Defined("endtime") {
			t.Fatal(js.Stringify(task, ""), err)
		}
	})

	t.Run("treats warnings as success and reads only the log tail", func(t *testing.T) {
		upid := mock.Upid("pve2", "vzdump", "", proxmoxtest.Task{Status: "stopped", ExitStatus: "WARNINGS: 2", Log: []string{"a", "b", "c", "no content"}})
		task, err := client.GetTask(ctx, upid, 2)
		if err != nil || task.Get("ok") != true || js.Stringify(task.Get("log"), "") != `["c"]` || task.Defined("id") {
			t.Fatal(js.Stringify(task, ""), err)
		}
	})

	t.Run("never follows redirects", func(t *testing.T) {
		redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, mock.URL+r.URL.Path, http.StatusFound)
		}))
		defer redirect.Close()
		c := newClient(t, with(tokenEnv, "PROXMOX_URL", redirect.URL))
		before := len(mock.Requests())
		_, err := c.Get(ctx, "/version", nil)
		mustMatch(t, err, `^GET /version failed with HTTP 302: Found$`)
		if len(mock.Requests()) != before {
			t.Fatal("the redirect was followed")
		}
	})

	t.Run("bounds the response size", func(t *testing.T) {
		defer proxmox.SetMaxResponseBytes(1024)()
		big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":"` + strings.Repeat("x", 2048) + `"}`))
		}))
		defer big.Close()
		_, err := newClient(t, with(tokenEnv, "PROXMOX_URL", big.URL)).Get(ctx, "/version", nil)
		mustMatch(t, err, `^GET /version returned more than 0 MiB; refusing to read the rest$`)
	})

	t.Run("refuses control characters in parameters", func(t *testing.T) {
		before := len(mock.Requests())
		for _, v := range []string{"net1\vunused0", "a\x00b", "x\x1b[0m", "\x7f"} {
			_, err := client.Put(ctx, "/nodes/pve1/qemu/100/config", js.O("delete", v))
			mustMatch(t, err, `^Parameter "delete" contains a control character`)
		}
		_, err := client.Get(ctx, "/version", js.O("a\x01", "x"))
		mustMatch(t, err, `^Parameter "a\?" contains a control character`)
		if len(mock.Requests()) != before {
			t.Fatal("a request was sent")
		}
		// Multi-line text is fine.
		if _, err := client.Put(ctx, "/nodes/pve1/qemu/100/config", js.O("description", "line 1\r\nline 2\tend")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("reports non-JSON responses", func(t *testing.T) {
		html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>hi</html>")) }))
		defer html.Close()
		_, err := newClient(t, with(tokenEnv, "PROXMOX_URL", html.URL)).Get(ctx, "/version", nil)
		mustMatch(t, err, `^GET /version returned a non-JSON response \(Content-Type text/html; charset=utf-8, 15 bytes\); check that PROXMOX_URL points at the Proxmox API$`)
		if strings.Contains(err.Error(), "hi") {
			t.Fatal("the body was echoed")
		}
	})

	t.Run("does not echo a non-JSON error body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("secret upstream page"))
		}))
		defer srv.Close()
		_, err := newClient(t, with(tokenEnv, "PROXMOX_URL", srv.URL)).Get(ctx, "/version", nil)
		mustMatch(t, err, `^GET /version failed with HTTP 502: Bad Gateway$`)
		if strings.Contains(err.Error(), "secret") {
			t.Fatal("the body was echoed")
		}
	})
}

func TestClientWithPassword(t *testing.T) {
	mock := proxmoxtest.New(t, "password")
	client := newClient(t, map[string]string{"PROXMOX_URL": mock.URL, "PROXMOX_USERNAME": "root", "PROXMOX_PASSWORD": "secret"})
	ctx := context.Background()

	t.Run("logs in, sends the ticket cookie and CSRF token on writes", func(t *testing.T) {
		if _, err := client.Get(ctx, "/version", nil); err != nil {
			t.Fatal(err)
		}
		if mock.LoginCount() != 1 {
			t.Fatal(mock.LoginCount())
		}
		if _, err := client.Post(ctx, "/nodes/pve1/qemu/100/status/start", nil); err != nil {
			t.Fatal(err)
		}
		req := mock.Last()
		if req.Headers.Get("CSRFPreventionToken") != "csrf-PVE:root@pam:1" || req.Headers.Get("Cookie") != "PVEAuthCookie=PVE%3Aroot%40pam%3A1" {
			t.Fatal(req.Headers)
		}
		if _, err := client.Get(ctx, "/version", nil); err != nil || mock.Last().Headers.Get("CSRFPreventionToken") != "" {
			t.Fatal("GETs carry no CSRF token", err)
		}
	})

	t.Run("logs in again once when the ticket expires", func(t *testing.T) {
		mock.ClearTickets()
		nodes, err := client.GetList(ctx, "/nodes", nil)
		if err != nil || len(nodes) != 2 || mock.LoginCount() != 2 {
			t.Fatal(len(nodes), err, mock.LoginCount())
		}
	})

	t.Run("shares one login between concurrent requests", func(t *testing.T) {
		c := newClient(t, map[string]string{"PROXMOX_URL": mock.URL, "PROXMOX_USERNAME": "root@pam", "PROXMOX_PASSWORD": "secret"})
		before := mock.LoginCount()
		errs := make(chan error, 8)
		for i := 0; i < 8; i++ {
			go func() { _, err := c.Get(ctx, "/version", nil); errs <- err }()
		}
		for i := 0; i < 8; i++ {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		if n := mock.LoginCount() - before; n != 1 {
			t.Fatalf("%d logins", n)
		}
	})

	t.Run("reports bad credentials clearly", func(t *testing.T) {
		bad := newClient(t, map[string]string{"PROXMOX_URL": mock.URL, "PROXMOX_USERNAME": "root@pam", "PROXMOX_PASSWORD": "wrong"})
		before := len(mock.RequestsTo("/access/ticket", "POST"))
		_, err := bad.Get(ctx, "/version", nil)
		mustMatch(t, err, `^Login to Proxmox failed \(HTTP 401 authentication failure\); check PROXMOX_USERNAME`)
		// Refused credentials aren't sent again on every call.
		for i := 0; i < 3; i++ {
			_, err = bad.Get(ctx, "/version", nil)
			mustMatch(t, err, `^Login to Proxmox failed \(HTTP 401 authentication failure\); check PROXMOX_USERNAME .*\(not retrying for [1-5]s\)$`)
		}
		if n := len(mock.RequestsTo("/access/ticket", "POST")) - before; n != 1 {
			t.Fatalf("%d login attempts", n)
		}
		// Once the backoff has passed it tries again, and waits longer after another failure.
		bad.SetLoginRetryAt(time.Now())
		_, err = bad.Get(ctx, "/version", nil)
		mustMatch(t, err, `^Login to Proxmox failed \(HTTP 401 authentication failure\); check PROXMOX_USERNAME \(user@realm\) and PROXMOX_PASSWORD$`)
		wait := time.Until(bad.LoginRetryAt())
		if wait < 9*time.Second || wait > 10*time.Second {
			t.Fatal(wait)
		}
	})

	t.Run("re-logs in on 401 but not on 403", func(t *testing.T) {
		c := newClient(t, map[string]string{"PROXMOX_URL": mock.URL, "PROXMOX_USERNAME": "root@pam", "PROXMOX_PASSWORD": "secret"})
		mock.Reply("GET", `^/forbidden$`, proxmoxtest.Error(403, "Permission check failed"))
		if _, err := c.Get(ctx, "/version", nil); err != nil {
			t.Fatal(err)
		}
		before := mock.LoginCount()
		_, err := c.Get(ctx, "/forbidden", nil)
		mustMatch(t, err, `HTTP 403`)
		if mock.LoginCount() != before {
			t.Fatal("logged in again after a 403")
		}
	})

	t.Run("refuses accounts that need two-factor authentication", func(t *testing.T) {
		tfa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":{"ticket":"x","NeedTFA":1}}`))
		}))
		defer tfa.Close()
		_, err := newClient(t, map[string]string{"PROXMOX_URL": tfa.URL, "PROXMOX_USERNAME": "a@pam", "PROXMOX_PASSWORD": "b"}).Get(ctx, "/version", nil)
		mustMatch(t, err, `requires two-factor authentication; use an API token instead`)
	})

	t.Run("reports unreachable hosts", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := ln.Addr().String()
		ln.Close()
		down := newClient(t, with(tokenEnv, "PROXMOX_URL", "http://"+addr))
		_, err := down.Get(ctx, "/version", nil)
		mustMatch(t, err, `^Could not reach Proxmox at http://`+regexp.QuoteMeta(addr)+`: ECONNREFUSED$`)
	})

	t.Run("reports a body that stalls after the headers as a timeout", func(t *testing.T) {
		release := make(chan struct{})
		stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte(`{"data":`))
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
		}))
		defer stall.Close()
		defer close(release)
		slow := newClient(t, with(tokenEnv, "PROXMOX_URL", stall.URL, "PROXMOX_TIMEOUT_MS", "300"))
		_, err := slow.Get(ctx, "/version", nil)
		mustMatch(t, err, `^Timed out after 300ms talking to http://127\.0\.0\.1`)
	})
}
