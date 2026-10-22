package proxmox

// Dry runs and the record of what a tool call sent.
//
// A tool call puts a *Call in its context. RequestMeta then records every state-changing request
// it sends (for the audit log) and, in a dry run, stops the first one before anything is sent:
// GETs, and POSTs marked with AsRead (such as a guest agent ping), still go through, so a preview
// can be built from live state.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/mattoddie/proxmox-mcp/internal/js"
)

// Call records the requests made for one tool call.
type Call struct {
	mu          sync.Mutex
	dryRun      bool
	sent        []SentRequest
	fetched     []fetched
	intercepted *Intercepted
	blocked     int
}

// SentRequest is a state-changing request that was sent to Proxmox.
type SentRequest struct {
	Method string
	Path   string
	// Status is the HTTP status, or 0 when no response arrived.
	Status int
}

// Intercepted is the first state-changing request of a dry run, which was not sent.
type Intercepted struct {
	Method string
	Path   string
	Params *js.Obj
	// Current is the object the request would change, when the call read it earlier (or the
	// preview read it afterwards).
	Current *js.Obj
}

type fetched struct {
	path string
	data any
}

type callKey struct{}

type readKey struct{}

// WithCall returns a context whose requests are recorded in call.
func WithCall(ctx context.Context, call *Call) context.Context {
	return context.WithValue(ctx, callKey{}, call)
}

// CallFrom returns the Call in ctx, or nil.
func CallFrom(ctx context.Context) *Call {
	call, _ := ctx.Value(callKey{}).(*Call)
	return call
}

// AsRead marks the requests made with the returned context as reads even when they aren't GETs
// (Proxmox has a few, such as the guest agent's ping): a dry run sends them, and they are not
// recorded as changes.
func AsRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, readKey{}, true)
}

func isRead(ctx context.Context, method string) bool {
	if method == "GET" {
		return true
	}
	r, _ := ctx.Value(readKey{}).(bool)
	return r
}

// SetDryRun makes the call a dry run: no state-changing request is sent.
func (c *Call) SetDryRun() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dryRun = true
}

// DryRun reports whether the call is a dry run.
func (c *Call) DryRun() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dryRun
}

// Sent returns the state-changing requests that were sent.
func (c *Call) Sent() []SentRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]SentRequest(nil), c.sent...)
}

// Intercepted returns the first request a dry run stopped, or nil, and how many state-changing
// requests were stopped in all.
func (c *Call) Intercepted() (*Intercepted, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.intercepted, c.blocked
}

func (c *Call) recordSent(method, path string, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, SentRequest{method, path, status})
}

func (c *Call) remember(path string, data any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dryRun {
		c.fetched = append(c.fetched, fetched{path, data})
	}
}

// intercept records a request a dry run stops and returns the error the tool sees.
func (c *Call) intercept(method, path string, params *js.Obj) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blocked++
	if c.intercepted == nil {
		var p *js.Obj
		if params != nil {
			p = params.Clone()
		}
		i := &Intercepted{Method: method, Path: path, Params: p}
		for j := len(c.fetched) - 1; j >= 0 && method != "POST"; j-- {
			if o, ok := c.fetched[j].data.(*js.Obj); ok && o != nil && c.fetched[j].path == path {
				i.Current = o
				break
			}
		}
		c.intercepted = i
	}
	return &DryRunError{Method: method, Path: path}
}

// DryRunError is returned for a request a dry run didn't send.
type DryRunError struct{ Method, Path string }

func (e *DryRunError) Error() string {
	return fmt.Sprintf("dry run: %s %s was not sent", e.Method, e.Path)
}

// ConfigPath reports whether a request changes the object read from the same path: every PUT
// (Proxmox updates objects in place) and a POST to a guest's /config (asynchronous VM config
// updates). The preview reads the path to show what would change.
func (i *Intercepted) ConfigPath() bool {
	return i.Method == "PUT" || (i.Method == "POST" && strings.HasSuffix(i.Path, "/config"))
}

// controlParams are request parameters that steer an update rather than set a value.
var controlParams = map[string]bool{"digest": true, "delete": true, "revert": true, "skiplock": true, "background_delay": true, "force": true}

// Preview describes an intercepted request for the model: what would be sent, with secrets
// redacted, and for an update of an object that could be read, the keys that would change with
// their values before and after.
func (i *Intercepted) Preview() *js.Obj {
	send := js.O("method", i.Method, "path", i.Path)
	out := js.O("dry_run", true, "would_send", send)
	if i.Params != nil && i.Params.Len() > 0 {
		send.Set("params", RedactSecrets(i.Params))
	}
	if i.Current == nil {
		return out
	}
	if i.Method == "DELETE" {
		out.Set("current", RedactSecrets(i.Current))
		return out
	}
	if i.ConfigPath() {
		out.Set("changes", Diff(i.Current, i.Params))
	}
	return out
}

// Diff lists the keys a Proxmox update would change, as {key: {from, to}}: the parameters whose
// value differs from the current one as Proxmox would store it (booleans as 1/0), and the keys
// named in `delete`, whose value becomes null. Proxmox updates are partial, so keys the request
// doesn't mention are left alone and not listed. Secret values are redacted.
func Diff(current, params *js.Obj) *js.Obj {
	out := js.NewObj()
	if params == nil {
		return out
	}
	sent := map[string][]string{}
	var keys []string
	for _, p := range EncodeParams(params) {
		if _, seen := sent[p.Key]; !seen {
			keys = append(keys, p.Key)
		}
		sent[p.Key] = append(sent[p.Key], p.Value)
	}
	for _, k := range keys {
		if controlParams[k] {
			continue
		}
		to := strings.Join(sent[k], ",")
		from := current.Get(k)
		if !js.IsUndef(from) && from != nil && wireValue(from) == to {
			continue
		}
		var toVal any = to
		if v := params.Get(k); len(sent[k]) == 1 {
			toVal = v
		}
		out.Set(k, change(k, from, toVal))
	}
	for _, k := range strings.FieldsFunc(js.Str(or(params.Get("delete"), "")), func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		if from := current.Get(k); !js.IsUndef(from) && !out.Has(k) {
			out.Set(k, change(k, from, nil))
		}
	}
	return out
}

func or(v, fallback any) any {
	if js.Nullish(v) {
		return fallback
	}
	return v
}

// wireValue is a current config value as a parameter would carry it.
func wireValue(v any) string {
	switch t := v.(type) {
	case bool:
		if t {
			return "1"
		}
		return "0"
	}
	return js.Str(v)
}

func change(key string, from, to any) *js.Obj {
	if js.IsUndef(from) {
		from = nil
	}
	if IsSecretKey(key) {
		if from != nil {
			from = Redacted
		}
		if to != nil {
			to = Redacted
		}
	}
	return js.O("from", from, "to", to)
}
