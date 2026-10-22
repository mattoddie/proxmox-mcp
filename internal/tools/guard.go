package tools

// Safety layers for tools that change things: a dry_run argument that previews the first change
// without sending it, and an audit log entry for every call (PROXMOX_AUDIT_LOG).

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mattoddie/proxmox-mcp/internal/config"
	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

// dryRunProp is the dry_run argument every tool that changes things gets. Its description is
// short because it is repeated on every write tool; the server instructions explain it once.
var dryRunProp = P("dry_run", Bool().Opt().D("Preview the request (and changed keys) without sending it"))

const dryRunNote = "Nothing was changed: this was a dry run. would_send is the first request that changes something, and it was not sent. Later steps of the tool (such as waiting for a task or a follow-up request) were not simulated. Call the tool again without dry_run to make the change."

// guarded is what the guard learns about one call while it runs.
type guarded struct {
	args *Obj
	// read is true when the arguments make this call a read (Def.Reads).
	read   bool
	dryRun bool
}

// dryRun runs a handler as a dry run: requests that change something are stopped and the first
// one is returned as a preview, with the changed keys for a config update.
func (c *Context) dryRun(ctx context.Context, call *proxmox.Call, a *Obj, handler Handler) (any, error) {
	call.SetDryRun()
	out, err := handler(ctx, a)
	req, _ := call.Intercepted()
	if req == nil {
		if err != nil {
			return nil, err
		}
		return js.O("dry_run", true, "would_send", nil, "note", "The tool would not send any request that changes something. result is what it returned.", "result", out), nil
	}
	if req.Current == nil && (req.ConfigPath() || req.Method == "DELETE") {
		// Read what the request would change, so the preview can show it. Reads go through in a
		// dry run; a path that can't be read (or isn't an object) just gives no comparison.
		if v, err := c.Client.Get(proxmox.AsRead(ctx), req.Path, nil); err == nil {
			if o, ok := v.(*Obj); ok && o != nil {
				req.Current = o
			}
		}
	}
	preview := req.Preview()
	note := dryRunNote
	if changes, ok := preview.Get("changes").(*Obj); ok && changes.Len() == 0 {
		note = "The request would not change any value. " + note
	}
	return preview.Set("note", note), nil
}

// auditLog writes one JSON line per call of a tool that changes things.
type auditLog struct {
	target string
	stderr io.Writer
	mu     sync.Mutex
	warned sync.Once
}

func newAuditLog(target string) *auditLog {
	if target == "" {
		return nil
	}
	return &auditLog{target: target, stderr: os.Stderr}
}

func (l *auditLog) record(tool string, raw json.RawMessage, state *guarded, call *proxmox.Call, res *mcp.CallToolResult) {
	defer func() {
		if r := recover(); r != nil {
			l.warn(r)
		}
	}()
	var args any = js.Undef
	if state.args != nil {
		args = state.args
	} else if len(raw) > 0 {
		if parsed, err := js.Parse(raw); err == nil {
			args = parsed
		}
	}
	requests := []any{}
	for _, r := range call.Sent() {
		requests = append(requests, js.O("method", r.Method, "path", r.Path, "status", ifThen(r.Status != 0, r.Status)))
	}
	entry := js.O(
		"time", time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		"tool", tool,
		"dry_run", state.dryRun,
		"arguments", proxmox.RedactSecrets(args),
		"requests", requests,
	)
	if p, _ := call.Intercepted(); p != nil {
		entry.Set("previewed", js.O("method", p.Method, "path", p.Path))
	}
	if res != nil && res.IsError {
		msg := ""
		if len(res.Content) > 0 {
			if t, ok := res.Content[0].(*mcp.TextContent); ok {
				msg = strings.TrimPrefix(t.Text, "Error: ")
			}
		}
		entry.Set("outcome", "error").Set("error", msg)
	} else {
		entry.Set("outcome", "ok")
	}
	l.write(js.Stringify(entry, "") + "\n")
}

func (l *auditLog) write(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var err error
	if l.target == config.AuditLogStderr {
		_, err = io.WriteString(l.stderr, line)
	} else {
		var f *os.File
		// Opened for each entry, so a rotated or deleted log is recreated.
		if f, err = config.OpenAuditLog(l.target); err == nil {
			_, err = f.WriteString(line)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err != nil {
		l.warn(err)
	}
}

// warn reports the first audit log failure; tool calls carry on regardless.
func (l *auditLog) warn(err any) {
	l.warned.Do(func() {
		log.Printf("proxmox-mcp: WARNING: could not write the audit log (%s): %v. Tool calls carry on; this warning is not repeated.", l.target, err)
	})
}

// dryRunHelp is the server instructions' explanation of dry runs.
const dryRunHelp = "Tools that change things take dry_run=true to preview the request without sending it (for config updates, the keys that would change, before and after); use it when the user wants to see a change first. "
