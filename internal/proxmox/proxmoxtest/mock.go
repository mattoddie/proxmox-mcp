// Package proxmoxtest is a small fake Proxmox VE API for tests. It serves a fixed two-node
// cluster (see the fixtures below), records every request, answers task status/log lookups for
// any UPID, and lets tests add routes with On. Unmatched POST/PUT/DELETE requests succeed with
// null data so simple write tools can be tested by inspecting the recorded requests.
package proxmoxtest

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
)

// The test API token.
const (
	TokenID     = "root@pam!test"
	TokenSecret = "00000000-1111-2222-3333-444444444444"
)

// Request is a recorded request.
type Request struct {
	Method string
	// Path is relative to /api2/json, without the query string, e.g. /nodes/pve1/qemu/100/status/start.
	Path string
	// Query and Body hold the parameters; repeated keys become arrays.
	Query   *js.Obj
	Body    *js.Obj
	Headers http.Header
}

// HTTPError is returned by handlers to send an HTTP error.
type HTTPError struct {
	Status int
	Reason string
	Errors map[string]string
}

// WithTotal is returned by routes that send a top-level `total` next to `data`, like the task log endpoint.
type WithTotal struct {
	Data  []any
	Total int
}

// Handler serves a custom route. It receives the merged query+body params, the regexp match and
// the request, and returns the `data` payload, a *HTTPError or a WithTotal.
type Handler func(params *js.Obj, match []string, req Request) any

// Task is a task's state.
type Task struct {
	Status     string // "running" or "stopped"
	ExitStatus string
	Log        []string
}

// TaskOK is a finished, successful task.
var TaskOK = Task{Status: "stopped", ExitStatus: "OK", Log: []string{"TASK OK"}}

type route struct {
	method  string
	pattern *regexp.Regexp
	fn      Handler
}

// Mock is the fake API.
type Mock struct {
	URL string
	// Auth is "token" (requires the test API token) or "password" (requires a ticket from /access/ticket).
	auth string

	mu          sync.Mutex
	requests    []Request
	tasks       map[string]Task
	tickets     map[string]bool
	loginCount  int
	handlers    []route
	upidCounter int
	server      *httptest.Server
}

// New starts a mock with "token" or "password" auth; it is closed when the test ends.
func New(t testing.TB, auth string) *Mock {
	m := &Mock{auth: auth, tasks: map[string]Task{}, tickets: map[string]bool{}}
	m.server = httptest.NewServer(http.HandlerFunc(m.serve))
	m.URL = m.server.URL
	t.Cleanup(m.server.Close)
	return m
}

// NewUnstarted returns a mock handler without a listener, for serving it over TLS.
func NewUnstarted(auth string) *Mock {
	return &Mock{auth: auth, tasks: map[string]Task{}, tickets: map[string]bool{}}
}

// ServeHTTP serves the fake API.
func (m *Mock) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.serve(w, r) }

// On registers a route. pattern is matched against the path relative to /api2/json, e.g.
// `^/nodes/([^/]+)/qemu/(\d+)/config$`. Later registrations take precedence.
func (m *Mock) On(method, pattern string, fn Handler) *Mock {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers = append([]route{{method, regexp.MustCompile(pattern), fn}}, m.handlers...)
	return m
}

// Reply registers a route that always returns data. A string that is valid JSON is parsed
// fresh on every request; any other string (such as a UPID) is returned as is.
func (m *Mock) Reply(method, pattern string, data any) *Mock {
	return m.On(method, pattern, func(*js.Obj, []string, Request) any {
		if s, ok := data.(string); ok {
			if v, err := js.Parse([]byte(s)); err == nil {
				return v
			}
			return s
		}
		return data
	})
}

// Reset removes all custom routes, recorded requests and tasks (fixtures stay).
func (m *Mock) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers = nil
	m.requests = nil
	m.tasks = map[string]Task{}
}

// Error builds an HTTP error for a handler to return.
func Error(status int, reason string, errs ...map[string]string) *HTTPError {
	e := &HTTPError{Status: status, Reason: reason}
	if len(errs) > 0 {
		e.Errors = errs[0]
	}
	return e
}

// Upid makes a UPID for a task on node and registers its final state (default stopped/OK).
func (m *Mock) Upid(node, typ, id string, task ...Task) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if node == "" {
		node = "pve1"
	}
	m.upidCounter++
	start := strings.ToUpper(strconv.FormatInt(int64(0x65000000+m.upidCounter), 16))
	upid := fmt.Sprintf("UPID:%s:0000ABCD:0001ABCD:%s:%s:%s:root@pam:", node, start, typ, id)
	t := TaskOK
	if len(task) > 0 {
		t = task[0]
	}
	m.tasks[upid] = t
	return upid
}

// SetTask changes a task's state.
func (m *Mock) SetTask(upid string, t Task) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[upid] = t
}

// Requests returns the recorded requests.
func (m *Mock) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Request(nil), m.requests...)
}

// Last returns the last recorded request.
func (m *Mock) Last() Request {
	r := m.Requests()
	if len(r) == 0 {
		return Request{}
	}
	return r[len(r)-1]
}

// RequestsTo returns the requests whose method matches (any when "") and whose path starts with
// prefix, or matches it when prefix starts with "^".
func (m *Mock) RequestsTo(prefix, method string) []Request {
	var re *regexp.Regexp
	if strings.HasPrefix(prefix, "^") {
		re = regexp.MustCompile(prefix)
	}
	var out []Request
	for _, r := range m.Requests() {
		if method != "" && r.Method != method {
			continue
		}
		if (re != nil && re.MatchString(r.Path)) || (re == nil && strings.HasPrefix(r.Path, prefix)) {
			out = append(out, r)
		}
	}
	return out
}

// Writes returns the recorded requests that aren't GETs.
func (m *Mock) Writes() []Request {
	var out []Request
	for _, r := range m.Requests() {
		if r.Method != "GET" {
			out = append(out, r)
		}
	}
	return out
}

// LastWrite returns the last recorded request that isn't a GET (the change a write tool made).
func (m *Mock) LastWrite() Request {
	w := m.Writes()
	if len(w) == 0 {
		return Request{}
	}
	return w[len(w)-1]
}

// LoginCount is the number of successful logins.
func (m *Mock) LoginCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loginCount
}

// ClearTickets forgets every ticket, to simulate expiry.
func (m *Mock) ClearTickets() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tickets = map[string]bool{}
}

func formToObject(values url.Values, order []string) *js.Obj {
	out := js.NewObj()
	for _, k := range order {
		if out.Has(k) {
			continue
		}
		vs := values[k]
		if len(vs) == 1 {
			out.Set(k, vs[0])
		} else {
			out.Set(k, js.Array(vs))
		}
	}
	return out
}

// keyOrder lists the keys of a form-encoded string in order of first appearance.
func keyOrder(raw string) []string {
	var keys []string
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, _, _ := strings.Cut(part, "=")
		k, _ = url.QueryUnescape(k)
		keys = append(keys, k)
	}
	return keys
}

func (m *Mock) serve(w http.ResponseWriter, r *http.Request) {
	bodyBytes, _ := io.ReadAll(r.Body)
	text := string(bodyBytes)
	// The path as sent (still percent-encoded), like the original mock's url.pathname.
	rawPath, _, _ := strings.Cut(r.RequestURI, "?")
	path := strings.TrimPrefix(rawPath, "/api2/json")
	query, _ := url.ParseQuery(r.URL.RawQuery)
	body, _ := url.ParseQuery(text)
	rec := Request{
		Method:  r.Method,
		Path:    path,
		Query:   formToObject(query, keyOrder(r.URL.RawQuery)),
		Body:    formToObject(body, keyOrder(text)),
		Headers: r.Header.Clone(),
	}
	if !strings.HasPrefix(rawPath, "/api2/json/") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	m.mu.Lock()
	m.requests = append(m.requests, rec)
	m.mu.Unlock()

	status, reason := 200, "OK"
	var payload *js.Obj
	out := m.route(rec)
	switch x := out.(type) {
	case *HTTPError:
		status, reason = x.Status, x.Reason
		payload = js.O("data", nil)
		if x.Errors != nil {
			payload.Set("errors", js.V(x.Errors))
		}
	case WithTotal:
		payload = js.O("data", x.Data, "total", x.Total)
	default:
		if js.IsUndef(out) {
			out = nil
		}
		payload = js.O("data", out)
	}
	writeResponse(w, status, reason, js.Stringify(payload, ""))
}

// writeResponse sends a JSON response. Proxmox puts error messages in the HTTP reason phrase,
// which net/http can't customise, so those responses are written on the hijacked connection.
func writeResponse(w http.ResponseWriter, status int, reason, body string) {
	if reason == http.StatusText(status) || reason == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
		return
	}
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		w.WriteHeader(status)
		return
	}
	defer conn.Close()
	fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", status, reason, len(body), body)
	_ = buf.Flush()
}

func (m *Mock) authorized(req Request) bool {
	if m.auth == "token" {
		return req.Headers.Get("Authorization") == "PVEAPIToken="+TokenID+"="+TokenSecret
	}
	ticket := ""
	for _, c := range strings.Split(req.Headers.Get("Cookie"), ";") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(c), "PVEAuthCookie="); ok {
			ticket, _ = url.PathUnescape(v)
		}
	}
	m.mu.Lock()
	ok := m.tickets[ticket]
	m.mu.Unlock()
	if !ok {
		return false
	}
	if req.Method != "GET" && req.Headers.Get("CSRFPreventionToken") != "csrf-"+ticket {
		return false
	}
	return true
}

var taskRe = regexp.MustCompile(`^/nodes/([^/]+)/tasks/([^/]+)/(status|log)$`)

func (m *Mock) route(req Request) any {
	params := js.Spread(req.Query, req.Body)

	if req.Method == "POST" && req.Path == "/access/ticket" {
		if params.Get("username") != "root@pam" || params.Get("password") != "secret" {
			return Error(401, "authentication failure")
		}
		m.mu.Lock()
		m.loginCount++
		ticket := fmt.Sprintf("PVE:root@pam:%d", m.loginCount)
		m.tickets[ticket] = true
		m.mu.Unlock()
		return js.O("ticket", ticket, "CSRFPreventionToken", "csrf-"+ticket, "username", "root@pam", "cap", js.NewObj())
	}
	if !m.authorized(req) {
		return Error(401, "No ticket")
	}

	m.mu.Lock()
	handlers := append([]route(nil), m.handlers...)
	m.mu.Unlock()
	for _, h := range handlers {
		if h.method != req.Method {
			continue
		}
		if match := h.pattern.FindStringSubmatch(req.Path); match != nil {
			return h.fn(params, match, req)
		}
	}

	if t := taskRe.FindStringSubmatch(req.Path); t != nil && req.Method == "GET" {
		upid, _ := url.PathUnescape(t[2])
		m.mu.Lock()
		task, ok := m.tasks[upid]
		m.mu.Unlock()
		if !ok {
			task = TaskOK
		}
		if t[3] == "status" {
			parts := strings.Split(upid, ":")
			for len(parts) < 8 {
				parts = append(parts, "")
			}
			start, _ := strconv.ParseInt(parts[4], 16, 64)
			out := js.O("upid", upid, "node", parts[1], "type", parts[5], "id", parts[6], "user", parts[7], "status", task.Status)
			if task.Status == "stopped" {
				out.Set("exitstatus", task.ExitStatus)
			}
			out.Set("starttime", start)
			if task.Status == "stopped" {
				out.Set("endtime", start+3)
			}
			out.Set("pid", 1)
			return out
		}
		lines := make([]any, len(task.Log))
		for i, l := range task.Log {
			lines[i] = js.O("n", i+1, "t", l)
		}
		start := 0
		if params.Defined("start") {
			start = int(js.Num(params.Get("start")))
		}
		limit := 50
		if params.Defined("limit") {
			limit = int(js.Num(params.Get("limit")))
		}
		end := min(start+limit, len(lines))
		if start > len(lines) {
			start = len(lines)
		}
		return WithTotal{Data: lines[start:max(end, start)], Total: len(lines)}
	}

	if f, ok := fixtures[req.Method+" "+req.Path]; ok {
		return f(params)
	}
	if req.Method == "GET" {
		return Error(404, "no mock for GET "+req.Path)
	}
	return nil
}
