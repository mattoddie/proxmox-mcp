package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mattoddie/proxmox-mcp/internal/config"
	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox/proxmoxtest"
)

// Test harness: a token-auth mock Proxmox, a client pointed at it and an MCP client session
// with every tool enabled (the Go port of the original test helpers).

// toolOptions enables everything up to level: read, write, delete, exec or all.
func toolOptions(level string) config.Tools {
	t := config.AllTools()
	t.AllowWrites = level != "read"
	t.AllowDeletes = level == "delete" || level == "all"
	t.AllowExec = level == "exec" || level == "all"
	return t
}

// connect starts a server with the given options and returns a connected client session.
func connect(t testing.TB, client *proxmox.Client, opts config.Tools) *mcp.ClientSession {
	t.Helper()
	server := NewServer(client, opts, nil)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Close() })
	return cs
}

type testEnv struct {
	t      testing.TB
	mock   *proxmoxtest.Mock
	client *proxmox.Client
	mcp    *mcp.ClientSession
}

// startEnv starts the mock, a client and a session with every tool enabled; extra settings
// (e.g. PROXMOX_DEFAULT_NODE) go in env.
func startEnv(t testing.TB, env ...map[string]string) *testEnv {
	t.Helper()
	mock := proxmoxtest.New(t, "token")
	vars := map[string]string{
		"PROXMOX_URL":             mock.URL,
		"PROXMOX_TOKEN_ID":        proxmoxtest.TokenID,
		"PROXMOX_TOKEN_SECRET":    proxmoxtest.TokenSecret,
		"PROXMOX_TASK_TIMEOUT_MS": "3000",
	}
	for _, e := range env {
		for k, v := range e {
			vars[k] = v
		}
	}
	cfg, err := config.Load(config.MapEnv(vars))
	if err != nil {
		t.Fatal(err)
	}
	client, err := proxmox.New(cfg.Proxmox)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return &testEnv{t: t, mock: mock, client: client, mcp: connect(t, client, toolOptions("all"))}
}

// argsJSON turns arguments given as a JSON string, a map or nil into raw JSON.
func argsJSON(t testing.TB, args any) json.RawMessage {
	t.Helper()
	switch a := args.(type) {
	case nil:
		return json.RawMessage("{}")
	case string:
		if a == "" {
			return json.RawMessage("{}")
		}
		if _, err := js.Parse([]byte(a)); err != nil {
			t.Fatalf("bad test arguments %s: %v", a, err)
		}
		return json.RawMessage(a)
	case *js.Obj:
		return json.RawMessage(js.Stringify(a, ""))
	}
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// callOn calls a tool on a session and returns the raw result.
func callOn(t testing.TB, s *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: argsJSON(t, args)})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func resultText(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

// parseResult parses a successful result's JSON text, failing the test on isError.
func parseResult(t testing.TB, res *mcp.CallToolResult) any {
	t.Helper()
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(res))
	}
	v, err := js.Parse([]byte(resultText(res)))
	if err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, resultText(res))
	}
	return v
}

// errorTextOf asserts that a call failed and returns its error text.
func errorTextOf(t testing.TB, res *mcp.CallToolResult) string {
	t.Helper()
	if !res.IsError {
		t.Fatalf("expected an error, got: %s", resultText(res))
	}
	return resultText(res)
}

// call calls a tool and parses its JSON result (failing on isError).
func (e *testEnv) call(t testing.TB, name string, args any) any {
	t.Helper()
	return parseResult(t, callOn(t, e.mcp, name, args))
}

// callObj is call for tools that return an object.
func (e *testEnv) callObj(t testing.TB, name string, args any) *Obj {
	t.Helper()
	v := e.call(t, name, args)
	o, ok := v.(*Obj)
	if !ok {
		t.Fatalf("%s returned %s, not an object", name, js.Stringify(v, ""))
	}
	return o
}

// callRaw calls a tool and returns the raw result.
func (e *testEnv) callRaw(t testing.TB, name string, args any) *mcp.CallToolResult {
	t.Helper()
	return callOn(t, e.mcp, name, args)
}

// errorText calls a tool, asserts it failed and returns the error text.
func (e *testEnv) errorText(t testing.TB, name string, args any) string {
	t.Helper()
	return errorTextOf(t, e.callRaw(t, name, args))
}

// ---------------------------------------------------------------- assertions

// at walks a dotted path ("items.0.vmid") into a value; missing steps give undefined.
func at(v any, path string) any {
	if path == "" {
		return v
	}
	for _, step := range strings.Split(path, ".") {
		switch x := v.(type) {
		case *Obj:
			v = x.Get(step)
		case []any:
			if step == "length" {
				return float64(len(x))
			}
			i, err := strconv.Atoi(step)
			if err != nil || i < 0 || i >= len(x) {
				return js.Undef
			}
			v = x[i]
		case string:
			if step == "length" {
				return float64(js.Len16(x))
			}
			return js.Undef
		default:
			return js.Undef
		}
	}
	return v
}

// jsEqual compares JSON values, ignoring object key order (like assert.deepEqual).
func jsEqual(a, b any) bool {
	a, b = js.V(a), js.V(b)
	switch x := a.(type) {
	case *Obj:
		y, ok := b.(*Obj)
		if !ok {
			return false
		}
		xk, yk := definedKeys(x), definedKeys(y)
		if len(xk) != len(yk) {
			return false
		}
		for i := range xk {
			if xk[i] != yk[i] || !jsEqual(x.Get(xk[i]), y.Get(yk[i])) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case float64:
		y, ok := b.(float64)
		return ok && (x == y || (math.IsNaN(x) && math.IsNaN(y)))
	}
	return a == b
}

func definedKeys(o *Obj) []string {
	var out []string
	for _, k := range o.Keys() {
		if !js.IsUndef(o.Get(k)) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// want parses an expected value: JSON text, or a Go value.
func want(v any) any {
	if s, ok := v.(string); ok {
		if p, err := js.Parse([]byte(s)); err == nil {
			return p
		}
	}
	return js.V(v)
}

// eq asserts that got equals the expected value (JSON text or a Go value), ignoring key order.
func eq(t testing.TB, got any, expected any, msg ...any) {
	t.Helper()
	w := want(expected)
	if !jsEqual(got, w) {
		t.Fatalf("%sgot  %s\nwant %s", prefix(msg), js.Stringify(got, ""), js.Stringify(w, ""))
	}
}

// eqAt asserts the value at a dotted path.
func eqAt(t testing.TB, v any, path string, expected any, msg ...any) {
	t.Helper()
	w := want(expected)
	got := at(v, path)
	if !jsEqual(got, w) {
		t.Fatalf("%s%s: got %s, want %s", prefix(msg), path, js.Stringify(got, ""), js.Stringify(w, ""))
	}
}

// eqStr asserts a string value exactly (no JSON parsing of the expectation).
func eqStr(t testing.TB, got any, expected string, msg ...any) {
	t.Helper()
	if got != expected {
		t.Fatalf("%sgot %s, want %q", prefix(msg), js.Stringify(got, ""), expected)
	}
}

func prefix(msg []any) string {
	if len(msg) == 0 {
		return ""
	}
	return fmt.Sprint(msg...) + ": "
}

// match asserts that text matches a regular expression.
func match(t testing.TB, text any, re string, msg ...any) {
	t.Helper()
	s, ok := text.(string)
	if !ok {
		s = js.Stringify(text, "")
	}
	if !regexp.MustCompile(re).MatchString(s) {
		t.Fatalf("%s%q does not match /%s/", prefix(msg), s, re)
	}
}

// noMatch asserts that text does not match a regular expression.
func noMatch(t testing.TB, text any, re string, msg ...any) {
	t.Helper()
	s, ok := text.(string)
	if !ok {
		s = js.Stringify(text, "")
	}
	if regexp.MustCompile(re).MatchString(s) {
		t.Fatalf("%s%q matches /%s/", prefix(msg), s, re)
	}
}

// truthy asserts a JavaScript-truthy value.
func truthy(t testing.TB, v any, msg ...any) {
	t.Helper()
	if !js.Truthy(v) {
		t.Fatalf("%sexpected a truthy value, got %s", prefix(msg), js.Stringify(v, ""))
	}
}

// falsy asserts a JavaScript-falsy value.
func falsy(t testing.TB, v any, msg ...any) {
	t.Helper()
	if js.Truthy(v) {
		t.Fatalf("%sexpected a falsy value, got %s", prefix(msg), js.Stringify(v, ""))
	}
}

// pluck maps an array of objects to one field ("items.map(g => g.vmid)").
// A list tool's result is plucked from its items.
func pluck(v any, field string) []any {
	if o, ok := v.(*js.Obj); ok && o != nil {
		v = o.Get("items")
	}
	arr, _ := v.([]any)
	out := make([]any, len(arr))
	for i, x := range arr {
		out[i] = at(x, field)
	}
	return out
}

// toolNames lists a session's tools.
func toolNames(t testing.TB, s *mcp.ClientSession) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for tool, err := range s.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		out[tool.Name] = true
	}
	return out
}

// testClient is a client for a URL that is never contacted.
func testClient(t testing.TB) *proxmox.Client {
	t.Helper()
	cfg, err := config.Load(config.MapEnv(map[string]string{"PROXMOX_URL": "https://pve.invalid", "PROXMOX_VERIFY_SSL": "false", "PROXMOX_TOKEN_ID": proxmoxtest.TokenID, "PROXMOX_TOKEN_SECRET": proxmoxtest.TokenSecret}))
	if err != nil {
		t.Fatal(err)
	}
	c, err := proxmox.New(cfg.Proxmox)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
