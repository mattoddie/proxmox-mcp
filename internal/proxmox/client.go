// Package proxmox is a client for the Proxmox VE REST API (/api2/json).
package proxmox

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mattoddie/proxmox-mcp/internal/config"
	"github.com/mattoddie/proxmox-mcp/internal/js"
)

// Error is an error from Proxmox or from reaching it. Status is the HTTP status, or 0.
type Error struct {
	Message string
	Status  int
}

func (e *Error) Error() string { return e.Message }

// Errorf returns a *Error without an HTTP status.
func Errorf(format string, args ...any) error {
	return &Error{Message: fmt.Sprintf(format, args...)}
}

// GuestRef identifies a VM (qemu) or container (lxc) and the node it is on.
type GuestRef struct {
	VMID     float64
	Node     string
	Type     string // "qemu" or "lxc"
	Name     any
	Status   any
	Template bool
}

// ticketMaxAge: a ticket lives for 2h; renew well before that.
const ticketMaxAge = 90 * time.Minute

var upidRe = regexp.MustCompile(`^UPID:([^:]+):[0-9A-Fa-f]+:[0-9A-Fa-f]+:([0-9A-Fa-f]+):([^:]*):([^:]*):([^:]*):`)

// IsUpid reports whether value is a task ID.
func IsUpid(value any) bool {
	s, ok := value.(string)
	return ok && upidRe.MatchString(s)
}

// Upid holds the parts of a task ID.
type Upid struct {
	Node      string
	StartTime float64
	Type      string
	ID        string
	User      string
}

// ParseUpid splits a UPID into its parts: node, start time, task type, object id and user.
func ParseUpid(upid string) (Upid, error) {
	m := upidRe.FindStringSubmatch(upid)
	if m == nil {
		return Upid{}, Errorf("Not a task id (UPID): %s", upid)
	}
	start, _ := strconv.ParseUint(m[2], 16, 64)
	return Upid{Node: m[1], StartTime: float64(start), Type: m[3], ID: m[4], User: m[5]}, nil
}

// Param is one encoded request parameter.
type Param struct{ Key, Value string }

// EncodeParams flattens parameters the way the web UI sends them: undefined and null values are
// dropped, booleans become 1/0, arrays repeat the key and objects are sent as JSON.
func EncodeParams(params *js.Obj) []Param {
	var out []Param
	for _, k := range params.Keys() {
		v := params.Get(k)
		if js.Nullish(v) {
			continue
		}
		values, ok := v.([]any)
		if !ok {
			values = []any{v}
		}
		for _, x := range values {
			switch t := x.(type) {
			case nil, js.Undefined:
				continue
			case bool:
				if t {
					out = append(out, Param{k, "1"})
				} else {
					out = append(out, Param{k, "0"})
				}
			case *js.Obj, []any:
				out = append(out, Param{k, js.Stringify(t, "")})
			default:
				out = append(out, Param{k, js.Str(t)})
			}
		}
	}
	return out
}

// FormEncode serialises parameters as application/x-www-form-urlencoded, byte for byte like
// URLSearchParams.toString().
func FormEncode(params []Param) string {
	var b strings.Builder
	for i, p := range params {
		if i > 0 {
			b.WriteByte('&')
		}
		formEscape(&b, p.Key)
		b.WriteByte('=')
		formEscape(&b, p.Value)
	}
	return b.String()
}

func formEscape(b *strings.Builder, s string) {
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '*', c == '-', c == '.', c == '_':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		}
	}
}

// Client talks to the Proxmox VE API. It authenticates with an API token (recommended) or with
// a username/password ticket, which is renewed automatically. Parameters are sent the way the
// Proxmox web UI sends them: in the query string for GET/DELETE and form-encoded for POST/PUT.
type Client struct {
	cfg config.Proxmox
	hc  *http.Client

	mu       sync.Mutex // guards ticket and the login backoff
	ticket   *ticket
	loginMu  sync.Mutex // one login at a time; waiters reuse its result
	loginGen int
	// After Proxmox rejects the credentials, logins are not retried until loginRetryAt, so a
	// wrong password isn't sent again on every tool call (and doesn't trip lockouts).
	loginFailures int
	loginRetryAt  time.Time
	loginErr      error
}

// Login backoff after rejected credentials: 5s, doubling up to 5 minutes.
const (
	loginBackoffMin = 5 * time.Second
	loginBackoffMax = 5 * time.Minute
)

// maxResponseBytes bounds a Proxmox response body; the largest real ones (big task logs, node
// reports) are a few MiB. A variable so tests can lower it.
var maxResponseBytes = 64 << 20

type ticket struct {
	value, csrf string
	at          time.Time
}

// New creates a client.
func New(cfg config.Proxmox) (*Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CACert != "" {
		// A custom CA turns verification on; it is trusted alongside the system CAs.
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(cfg.CACert)) {
			return nil, errors.New("PROXMOX_CA_FILE contains no PEM certificates")
		}
		tlsCfg.RootCAs = pool
	} else {
		tlsCfg.InsecureSkipVerify = !cfg.VerifySSL //nolint:gosec // the user's choice; the startup warning covers it
	}
	if len(cfg.TLSFingerprints) > 0 {
		// Checked on every connection (Go only resumes TLS sessions with a session cache, which
		// this config doesn't have), after chain verification when that is on.
		pins := cfg.TLSFingerprints
		tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
			presented := "no certificate"
			if len(cs.PeerCertificates) > 0 {
				presented = Fingerprint(cs.PeerCertificates[0].Raw)
				for _, p := range pins {
					if p == presented {
						return nil
					}
				}
			}
			return &pinError{fmt.Sprintf("TLS certificate fingerprint mismatch: the server presented SHA-256 %s, which is not in PROXMOX_TLS_FINGERPRINT", presented)}
		}
	}
	transport := &http.Transport{
		// Like the original undici dispatcher: no proxy from the environment.
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: time.Duration(cfg.TimeoutMs) * time.Millisecond, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   time.Duration(cfg.TimeoutMs) * time.Millisecond,
		ResponseHeaderTimeout: time.Duration(cfg.TimeoutMs) * time.Millisecond,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
	}
	return &Client{
		cfg: cfg,
		hc: &http.Client{
			Transport: transport,
			Timeout:   time.Duration(cfg.TimeoutMs) * time.Millisecond,
			// Never follow redirects: they would carry the credentials to wherever they point.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

type pinError struct{ msg string }

func (e *pinError) Error() string { return e.msg }

// Fingerprint is a certificate's SHA-256 fingerprint as "AA:BB:...".
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// Config returns the client's settings.
func (c *Client) Config() config.Proxmox { return c.cfg }

// DefaultNode is PROXMOX_DEFAULT_NODE.
func (c *Client) DefaultNode() string { return c.cfg.DefaultNode }

// TaskTimeoutMs is PROXMOX_TASK_TIMEOUT_MS.
func (c *Client) TaskTimeoutMs() int { return c.cfg.TaskTimeoutMs }

// Close releases idle connections.
func (c *Client) Close() { c.hc.CloseIdleConnections() }

// Get sends a GET request; params may be nil.
func (c *Client) Get(ctx context.Context, path string, params *js.Obj) (any, error) {
	return c.Request(ctx, "GET", path, params)
}

// Post sends a POST request.
func (c *Client) Post(ctx context.Context, path string, params *js.Obj) (any, error) {
	return c.Request(ctx, "POST", path, params)
}

// Put sends a PUT request.
func (c *Client) Put(ctx context.Context, path string, params *js.Obj) (any, error) {
	return c.Request(ctx, "PUT", path, params)
}

// Delete sends a DELETE request.
func (c *Client) Delete(ctx context.Context, path string, params *js.Obj) (any, error) {
	return c.Request(ctx, "DELETE", path, params)
}

// GetObj is Get for endpoints that return an object; anything else gives an empty object.
func (c *Client) GetObj(ctx context.Context, path string, params *js.Obj) (*js.Obj, error) {
	v, err := c.Get(ctx, path, params)
	if err != nil {
		return nil, err
	}
	if o, ok := v.(*js.Obj); ok && o != nil {
		return o, nil
	}
	return js.NewObj(), nil
}

// GetList is Get for endpoints that return an array of objects.
func (c *Client) GetList(ctx context.Context, path string, params *js.Obj) ([]*js.Obj, error) {
	v, err := c.Get(ctx, path, params)
	if err != nil {
		return nil, err
	}
	return js.Objs(v), nil
}

// Request sends a request. path is relative to /api2/json, e.g. /cluster/resources. It returns
// the `data` member of the response.
func (c *Client) Request(ctx context.Context, method, path string, params *js.Obj) (any, error) {
	data, _, err := c.RequestMeta(ctx, method, path, params)
	return data, err
}

// RequestMeta is Request that also returns the response's top-level `total` and `changes`
// (task logs report `total`, network config reports pending `changes`), when present.
func (c *Client) RequestMeta(ctx context.Context, method, path string, params *js.Obj) (any, *js.Obj, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	call := CallFrom(ctx)
	changes := !isRead(ctx, method)
	if changes && call != nil && call.DryRun() {
		// Stopped before logging in, so a dry run never sends anything but reads (and the login
		// those need).
		return nil, nil, call.intercept(method, path, params)
	}
	if c.cfg.TokenID == "" {
		if err := c.ensureTicket(ctx); err != nil {
			return nil, nil, err
		}
	}
	data, meta, err := c.requestMeta(ctx, method, path, params, changes, call)
	if err == nil && method == "GET" && call != nil {
		call.remember(path, data)
	}
	return data, meta, err
}

func (c *Client) requestMeta(ctx context.Context, method, path string, params *js.Obj, changes bool, call *Call) (any, *js.Obj, error) {
	res, body, err := c.send(ctx, method, path, params, true)
	if changes && call != nil && (err != nil || res.StatusCode != http.StatusUnauthorized || c.cfg.TokenID != "") {
		// Recorded once per request: a 401 with a ticket is retried below and recorded then.
		status := 0
		if res != nil {
			status = res.StatusCode
		}
		call.recordSent(method, path, status)
	}
	if err != nil {
		return nil, nil, err
	}
	if res.StatusCode == http.StatusUnauthorized && c.cfg.TokenID == "" {
		// Ticket expired or revoked: log in again and retry once.
		c.mu.Lock()
		c.ticket = nil
		c.mu.Unlock()
		if err := c.login(ctx, true); err != nil {
			if changes && call != nil {
				call.recordSent(method, path, http.StatusUnauthorized)
			}
			return nil, nil, err
		}
		res, body, err = c.send(ctx, method, path, params, true)
		if changes && call != nil {
			status := 0
			if res != nil {
				status = res.StatusCode
			}
			call.recordSent(method, path, status)
		}
		if err != nil {
			return nil, nil, err
		}
	}
	return c.parse(res, body, method, path)
}

// APIURL is the URL a request for path (relative to /api2/json) is sent to.
func (c *Client) APIURL(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return c.cfg.URL + "/api2/json" + path
}

// ListNodes returns the cluster's nodes with their online status.
func (c *Client) ListNodes(ctx context.Context) ([]*js.Obj, error) {
	return c.GetList(ctx, "/nodes", nil)
}

// ResolveNode picks the node for node-scoped calls: the one given, else PROXMOX_DEFAULT_NODE,
// else the only node of a single-node install.
func (c *Client) ResolveNode(ctx context.Context, node string) (string, error) {
	if node != "" {
		return node, nil
	}
	if c.cfg.DefaultNode != "" {
		return c.cfg.DefaultNode, nil
	}
	nodes, err := c.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	if len(nodes) == 1 {
		return js.Str(nodes[0].Get("node")), nil
	}
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = js.Str(n.Get("node"))
	}
	sortStrings(names)
	return "", Errorf("This cluster has %d nodes; pass `node` (one of: %s) or set PROXMOX_DEFAULT_NODE", len(nodes), strings.Join(names, ", "))
}

// ResolveGuest finds which node a VM or container lives on and whether it is a VM (qemu) or a
// container (lxc). node and typ are optional.
func (c *Client) ResolveGuest(ctx context.Context, vmid float64, node, typ string) (GuestRef, error) {
	resources, err := c.GetList(ctx, "/cluster/resources", js.O("type", "vm"))
	if err != nil {
		return GuestRef{}, err
	}
	var match *js.Obj
	for _, r := range resources {
		if js.Num(r.Get("vmid")) == vmid {
			match = r
			break
		}
	}
	if match == nil {
		if node != "" && typ != "" {
			return GuestRef{VMID: vmid, Node: node, Type: typ, Name: js.Undef, Status: js.Undef}, nil
		}
		return GuestRef{}, Errorf("No VM or container with ID %s (see proxmox_list_guests)", js.NumStr(vmid))
	}
	mtype := js.Str(match.Get("type"))
	if typ != "" && mtype != typ {
		want := "a container (lxc)"
		if typ == "qemu" {
			want = "a VM (qemu)"
		}
		is, tools := "a container", "proxmox_*_container"
		if mtype == "qemu" {
			is, tools = "a VM", "proxmox_*_vm"
		}
		return GuestRef{}, Errorf("Guest %s is %s, not %s; use the %s tools", js.NumStr(vmid), is, want, tools)
	}
	if node == "" {
		node = js.Str(match.Get("node"))
	}
	return GuestRef{
		VMID:     js.Num(match.Get("vmid")),
		Node:     node,
		Type:     mtype,
		Name:     match.Get("name"),
		Status:   match.Get("status"),
		Template: js.Truthy(match.Get("template")),
	}, nil
}

// GuestPath is the API path prefix of a guest, e.g. /nodes/pve1/qemu/100.
func (c *Client) GuestPath(g GuestRef) string {
	return "/nodes/" + js.EncodeURIComponent(g.Node) + "/" + g.Type + "/" + js.NumStr(g.VMID)
}

// GetTask returns a task's current status plus the tail of its log.
func (c *Client) GetTask(ctx context.Context, upid string, logLines int) (*js.Obj, error) {
	u, err := ParseUpid(upid)
	if err != nil {
		return nil, err
	}
	base := "/nodes/" + js.EncodeURIComponent(u.Node) + "/tasks/" + js.EncodeURIComponent(upid)
	st, err := c.Get(ctx, base+"/status", nil)
	if err != nil {
		return nil, err
	}
	log := any(js.Undef)
	if logLines > 0 {
		// The log endpoint pages from the start; ask for the total first so we can show the end.
		total := 0.0
		if _, meta, err := c.RequestMeta(ctx, "GET", base+"/log", js.O("start", 0, "limit", 1)); err == nil {
			total = js.Num(meta.Or("total", 0.0))
		}
		if !js.IsFinite(total) || total <= 0 {
			total = 0
		}
		start := math.Max(0, total-float64(logLines))
		lines := []any{}
		if v, err := c.Get(ctx, base+"/log", js.O("start", start, "limit", logLines)); err == nil {
			if arr, ok := v.([]any); ok {
				lines = arr
			}
		}
		out := []any{}
		for _, l := range js.Objs(lines) {
			t := l.Get("t")
			if js.Nullish(t) {
				t = ""
			}
			if s := js.Str(t); s != "no content" {
				out = append(out, s)
			}
		}
		log = out
	}
	return taskResult(upid, st, log), nil
}

// WaitOptions tune WaitForTask; zero values take the defaults.
type WaitOptions struct {
	TimeoutMs int
	PollMs    int
	LogLines  int
}

// WaitForTask polls a task until it finishes, up to the timeout (default
// PROXMOX_TASK_TIMEOUT_MS). It returns early with timedOut=true rather than failing when the
// task is still running.
func (c *Client) WaitForTask(ctx context.Context, upid string, opts WaitOptions) (*js.Obj, error) {
	u, err := ParseUpid(upid)
	if err != nil {
		return nil, err
	}
	timeout := opts.TimeoutMs
	if timeout == 0 {
		timeout = c.cfg.TaskTimeoutMs
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Millisecond)
	delay := 500.0
	if opts.PollMs > 0 {
		delay = float64(opts.PollMs)
	}
	logLines := 30
	if opts.LogLines > 0 {
		logLines = opts.LogLines
	}
	for {
		st, err := c.GetObj(ctx, "/nodes/"+js.EncodeURIComponent(u.Node)+"/tasks/"+js.EncodeURIComponent(upid)+"/status", nil)
		if err != nil {
			return nil, err
		}
		if st.Get("status") != "running" {
			return c.GetTask(ctx, upid, logLines)
		}
		wait := time.Duration(delay * float64(time.Millisecond))
		if time.Now().Add(wait).After(deadline) {
			tail := 10
			if opts.LogLines > 0 {
				tail = opts.LogLines
			}
			res, err := c.GetTask(ctx, upid, tail)
			if err != nil {
				return nil, err
			}
			return res.Set("timedOut", true), nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		delay = math.Min(delay*1.5, 3000)
	}
}

func taskResult(upid string, stAny any, log any) *js.Obj {
	parts, _ := ParseUpid(upid)
	st, _ := stAny.(*js.Obj)
	or := func(k string, fallback any) any { return st.Or(k, fallback) }
	stopped := st.Get("status") == "stopped"
	start := js.Num(or("starttime", parts.StartTime))
	end := js.Num(st.Get("endtime"))
	id := or("id", parts.ID)
	if !js.Truthy(id) {
		id = js.Undef
	}
	exit := st.Get("exitstatus")
	exitText := ""
	if !js.Nullish(exit) {
		exitText = js.Str(exit)
	}
	ok := stopped && (exit == "OK" || strings.HasPrefix(exitText, "WARNINGS"))
	endtime, duration := any(js.Undef), any(js.Undef)
	if stopped {
		endtime = IsoTime(end)
		if js.IsFinite(end) && end >= start {
			duration = end - start
		}
	}
	return js.O(
		"upid", upid,
		"node", or("node", parts.Node),
		"type", or("type", parts.Type),
		"id", id,
		"user", or("user", parts.User),
		"status", or("status", "unknown"),
		"exitstatus", exit,
		"ok", ok,
		"starttime", IsoTime(start),
		"endtime", endtime,
		"duration_s", duration,
		"log", log,
	)
}

// ---------------------------------------------------------------- transport

func (c *Client) ensureTicket(ctx context.Context) error {
	c.mu.Lock()
	t := c.ticket
	c.mu.Unlock()
	if t == nil || time.Since(t.at) > ticketMaxAge {
		return c.login(ctx, false)
	}
	return nil
}

// login gets a new ticket. Concurrent callers share one login: whoever waited for another
// login in progress uses its result instead of logging in again.
func (c *Client) login(ctx context.Context, force bool) error {
	c.mu.Lock()
	gen := c.loginGen
	c.mu.Unlock()
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	c.mu.Lock()
	if c.loginGen != gen && c.ticket != nil {
		c.mu.Unlock()
		return nil
	}
	if !force && c.ticket != nil && time.Since(c.ticket.at) <= ticketMaxAge {
		c.mu.Unlock()
		return nil
	}
	if wait := time.Until(c.loginRetryAt); wait > 0 && c.loginErr != nil {
		err := c.loginErr
		c.mu.Unlock()
		return &Error{Message: fmt.Sprintf("%s (not retrying for %s)", errMessage(err), wait.Round(time.Second)), Status: errStatus(err)}
	}
	c.mu.Unlock()
	t, err := c.doLogin(ctx)
	if err != nil {
		var pe *Error
		if errors.As(err, &pe) && pe.Status >= 400 && pe.Status < 500 {
			// The credentials were refused; network errors and 5xx are retried as before.
			c.mu.Lock()
			c.loginFailures++
			backoff := loginBackoffMin << min(c.loginFailures-1, 10)
			c.loginRetryAt = time.Now().Add(min(backoff, loginBackoffMax))
			c.loginErr = err
			c.mu.Unlock()
		}
		return err
	}
	c.mu.Lock()
	c.ticket = t
	c.loginGen++
	c.loginFailures, c.loginErr = 0, nil
	c.mu.Unlock()
	return nil
}

func errMessage(err error) string {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Message
	}
	return err.Error()
}

func errStatus(err error) int {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Status
	}
	return 0
}

func (c *Client) doLogin(ctx context.Context) (*ticket, error) {
	res, body, err := c.send(ctx, "POST", "/access/ticket", js.O("username", c.cfg.Username, "password", c.cfg.Password), false)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, &Error{
			Message: fmt.Sprintf("Login to Proxmox failed (HTTP %d %s); check PROXMOX_USERNAME (user@realm) and PROXMOX_PASSWORD", res.StatusCode, statusText(res)),
			Status:  res.StatusCode,
		}
	}
	var data *js.Obj
	if v, err := js.Parse(body); err == nil {
		if o, ok := v.(*js.Obj); ok {
			data = o.ObjAt("data")
		}
	}
	if data.Truthy("NeedTFA") {
		return nil, Errorf("This account requires two-factor authentication; use an API token instead (PROXMOX_TOKEN_ID/PROXMOX_TOKEN_SECRET)")
	}
	if !data.Truthy("ticket") {
		return nil, Errorf("Login to Proxmox failed: no ticket in the response")
	}
	return &ticket{value: js.Str(data.Get("ticket")), csrf: js.Str(data.Get("CSRFPreventionToken")), at: time.Now()}, nil
}

// paramControlRe matches control characters other than tab, newline and carriage return, which
// multi-line values (descriptions, certificates, notes) legitimately contain.
var paramControlRe = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)

// checkParamChars refuses parameters with control characters. Proxmox splits lists on some of
// them (NUL, \v), so they could hide an entry from a check made on this side, and no
// legitimate text parameter needs them.
func checkParamChars(params []Param) error {
	for _, p := range params {
		if paramControlRe.MatchString(p.Key) || paramControlRe.MatchString(p.Value) {
			return Errorf("Parameter %q contains a control character (only tab, newline and carriage return are allowed; send binary data base64-encoded)", strings.ToValidUTF8(paramControlRe.ReplaceAllString(p.Key, "?"), "?"))
		}
	}
	return nil
}

// send makes one request and reads the whole body.
func (c *Client) send(ctx context.Context, method, path string, params *js.Obj, auth bool) (*http.Response, []byte, error) {
	target := c.APIURL(path)
	encoded := EncodeParams(params)
	if auth {
		if err := checkParamChars(encoded); err != nil {
			return nil, nil, err
		}
	}
	var body io.Reader
	if method == "GET" || method == "DELETE" {
		if len(encoded) > 0 {
			target += "?" + FormEncode(encoded)
		}
	} else {
		body = strings.NewReader(FormEncode(encoded))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, nil, c.networkError(err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if auth {
		if c.cfg.TokenID != "" {
			req.Header.Set("Authorization", "PVEAPIToken="+c.cfg.TokenID+"="+c.cfg.TokenSecret)
		} else {
			c.mu.Lock()
			t := c.ticket
			c.mu.Unlock()
			if t != nil {
				req.Header.Set("Cookie", "PVEAuthCookie="+js.EncodeURIComponent(t.value))
				if method != "GET" {
					req.Header.Set("CSRFPreventionToken", t.csrf)
				}
			}
		}
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, c.networkError(err)
	}
	defer res.Body.Close()
	// A timeout or dropped connection while reading the body is reported like one before the headers.
	data, err := io.ReadAll(io.LimitReader(res.Body, int64(maxResponseBytes)+1))
	if err != nil {
		return nil, nil, c.networkError(err)
	}
	if len(data) > maxResponseBytes {
		return nil, nil, &Error{Message: fmt.Sprintf("%s %s returned more than %d MiB; refusing to read the rest", method, path, maxResponseBytes>>20), Status: res.StatusCode}
	}
	return res, data, nil
}

// describeBody summarises a response body by its type and size, without its content.
func describeBody(res *http.Response, body []byte) string {
	ct := res.Header.Get("Content-Type")
	if ct == "" {
		ct = "none"
	}
	return fmt.Sprintf("(Content-Type %s, %d bytes)", ct, len(body))
}

// statusText is the HTTP reason phrase as the server sent it.
func statusText(res *http.Response) string {
	code := strconv.Itoa(res.StatusCode)
	return strings.TrimSpace(strings.TrimPrefix(res.Status, code))
}

func (c *Client) parse(res *http.Response, body []byte, method, path string) (any, *js.Obj, error) {
	text := string(body)
	var parsed any = js.Undef
	if text != "" {
		if v, err := js.Parse(body); err == nil {
			parsed = v
		}
	}
	jsonObj, _ := parsed.(*js.Obj)
	if res.StatusCode < 200 || res.StatusCode > 299 {
		// Proxmox puts the message in the HTTP reason phrase and per-parameter errors in `errors`.
		var reason any = statusText(res)
		if reason == "" {
			reason = jsonObj.Get("message")
			if !js.Truthy(reason) {
				// Not the body itself: whatever answered may not be Proxmox, and its page
				// shouldn't end up in front of the model.
				reason = http.StatusText(res.StatusCode) + " " + describeBody(res, body)
			}
		}
		paramErrors := ""
		if errs, ok := jsonObj.Get("errors").(*js.Obj); ok {
			var parts []string
			for _, k := range errs.Keys() {
				parts = append(parts, k+": "+strings.TrimSpace(js.Str(errs.Get(k))))
			}
			paramErrors = strings.Join(parts, "; ")
		} else if arr, ok := jsonObj.Get("errors").([]any); ok {
			var parts []string
			for i, v := range arr {
				parts = append(parts, strconv.Itoa(i)+": "+strings.TrimSpace(js.Str(v)))
			}
			paramErrors = strings.Join(parts, "; ")
		}
		hint := ""
		switch res.StatusCode {
		case 401:
			hint = " (check the API token or credentials)"
		case 403:
			hint = " (the token or user lacks the privilege for this; for API tokens check privilege separation and the token's own ACLs)"
		case 501:
			hint = " (not implemented on this Proxmox VE version)"
		}
		if paramErrors != "" {
			paramErrors = " (" + paramErrors + ")"
		}
		return nil, nil, &Error{
			Message: fmt.Sprintf("%s %s failed with HTTP %d: %s%s%s", method, path, res.StatusCode, strings.TrimSpace(js.Str(reason)), paramErrors, hint),
			Status:  res.StatusCode,
		}
	}
	if js.IsUndef(parsed) {
		if text == "" {
			return js.Undef, js.NewObj(), nil
		}
		return nil, nil, &Error{Message: fmt.Sprintf("%s %s returned a non-JSON response %s; check that PROXMOX_URL points at the Proxmox API", method, path, describeBody(res, body)), Status: res.StatusCode}
	}
	data := parsed
	if jsonObj != nil && jsonObj.Has("data") {
		data = jsonObj.Get("data")
	}
	meta := js.NewObj()
	for _, key := range []string{"total", "changes"} {
		if !jsonObj.Defined(key) {
			continue
		}
		switch d := data.(type) {
		case *js.Obj:
			if d != nil && !d.Has(key) {
				meta.Set(key, jsonObj.Get(key))
			}
		case []any:
			meta.Set(key, jsonObj.Get(key))
		}
	}
	return data, meta, nil
}

// networkError reports a failure to reach Proxmox like the original server did: timeouts as
// "Timed out after ...", other errors with the system error code where there is one, plus a
// hint for certificate problems.
func (c *Client) networkError(err error) error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return Errorf("Timed out after %dms talking to %s", c.cfg.TimeoutMs, c.cfg.URL)
	}
	cause := errorCause(err)
	hint := ""
	if tlsHintRe.MatchString(cause) {
		hint = " (Proxmox uses a self-signed certificate by default; pin it with PROXMOX_TLS_FINGERPRINT, trust its CA with PROXMOX_CA_FILE, or set PROXMOX_VERIFY_SSL=false)"
	}
	return Errorf("Could not reach Proxmox at %s: %s%s", c.cfg.URL, cause, hint)
}

var tlsHintRe = regexp.MustCompile(`(?i)CERT|SELF_SIGNED|UNABLE_TO_VERIFY`)

// errorCause names the underlying problem the way Node does (ECONNREFUSED, ENOTFOUND,
// DEPTH_ZERO_SELF_SIGNED_CERT...), or gives the innermost error message.
func errorCause(err error) string {
	var pin *pinError
	if errors.As(err, &pin) {
		return pin.msg
	}
	var unknown x509.UnknownAuthorityError
	if errors.As(err, &unknown) {
		if unknown.Cert != nil && unknown.Cert.Subject.String() == unknown.Cert.Issuer.String() {
			return "DEPTH_ZERO_SELF_SIGNED_CERT"
		}
		return "UNABLE_TO_GET_ISSUER_CERT_LOCALLY"
	}
	var host x509.HostnameError
	if errors.As(err, &host) {
		return "ERR_TLS_CERT_ALTNAME_INVALID"
	}
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) {
		if invalid.Reason == x509.Expired {
			return "CERT_HAS_EXPIRED"
		}
		return "CERT_INVALID: " + invalid.Error()
	}
	var tlsVerify *tls.CertificateVerificationError
	if errors.As(err, &tlsVerify) {
		return "CERT_VERIFICATION_FAILED: " + tlsVerify.Err.Error()
	}
	for _, e := range []struct {
		errno syscall.Errno
		code  string
	}{
		{syscall.ECONNREFUSED, "ECONNREFUSED"}, {syscall.ECONNRESET, "ECONNRESET"}, {syscall.EHOSTUNREACH, "EHOSTUNREACH"},
		{syscall.ENETUNREACH, "ENETUNREACH"}, {syscall.ECONNABORTED, "ECONNABORTED"}, {syscall.EPIPE, "EPIPE"},
	} {
		if errors.Is(err, e.errno) {
			return e.code
		}
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.IsNotFound {
			return "ENOTFOUND"
		}
		return "EAI_AGAIN"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return "other side closed"
	}
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err.Error()
		}
		err = next
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
