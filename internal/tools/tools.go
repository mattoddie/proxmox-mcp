// Package tools defines proxmox-mcp's MCP tools and prompts.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mattoddie/proxmox-mcp/internal/config"
	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

// Version is the server version; release builds set it with -ldflags "-X ...tools.Version=1.2.3".
var Version = "0.0.0-dev"

// ServerName is the MCP server name.
const ServerName = "proxmox-mcp"

// Obj is a JSON object from the API or an argument object.
type Obj = js.Obj

// Gate is the setting a tool needs before it is registered.
type Gate int

const (
	Read   Gate = iota // always registered
	Write              // PROXMOX_ALLOW_WRITES
	Delete             // PROXMOX_ALLOW_WRITES and PROXMOX_ALLOW_DELETES
	Exec               // PROXMOX_ALLOW_WRITES and PROXMOX_ALLOW_EXEC
)

func (g Gate) String() string {
	return [...]string{"read", "write", "delete", "exec"}[g]
}

// Context is shared by the tool definitions.
type Context struct {
	Server *mcp.Server
	Client *proxmox.Client
	config.Tools
	// Registered records each registered tool's toolset and gate, for the docs generator and tests.
	Registered map[string]Info
}

// Info describes a registered tool.
type Info struct {
	Toolset string
	Gate    Gate
	Props   []Prop
	// Order is the registration order (the SDK lists tools by name).
	Order int
}

// Handler runs a tool with its validated arguments and returns a value to render as JSON.
type Handler func(ctx context.Context, a *Obj) (any, error)

// Def describes a tool.
type Def struct {
	// Toolset the tool belongs to; it is only registered when the toolset is enabled (PROXMOX_TOOLSETS).
	Toolset     string
	Title       string
	Description string
	Input       []Prop
	// Write marks tools that change state; only registered when writes are enabled.
	Write bool
	// Delete marks tools that delete guests or configuration; only registered when deletes are enabled too.
	Delete bool
	// Exec marks tools that run commands or write files in guests/nodes; only registered when exec is enabled too.
	Exec bool
	// Destructive overrides the destructiveHint annotation (defaults to Delete || Exec).
	Destructive *bool
	// Idempotent: repeating the call with the same arguments has no further effect.
	Idempotent bool
	// ReadOnly overrides the readOnlyHint annotation (defaults to !write).
	ReadOnly *bool
	Handler  Handler
}

func boolPtr(b bool) *bool { return &b }

// True and False are for Def.Destructive and Def.ReadOnly.
var (
	True  = boolPtr(true)
	False = boolPtr(false)
)

// gate is the setting the definition needs.
func (d Def) gate() Gate {
	switch {
	case d.Exec:
		return Exec
	case d.Delete:
		return Delete
	case d.Write:
		return Write
	}
	return Read
}

// Define registers a tool if its toolset and gate are enabled.
func (c *Context) Define(name string, d Def) {
	if !c.Toolsets[d.Toolset] {
		return
	}
	write := d.Write || d.Delete || d.Exec
	if write && !c.AllowWrites {
		return
	}
	if d.Delete && !c.AllowDeletes {
		return
	}
	if d.Exec && !c.AllowExec {
		return
	}
	readOnly := !write
	if d.ReadOnly != nil {
		readOnly = *d.ReadOnly
	}
	ann := &mcp.ToolAnnotations{Title: d.Title, ReadOnlyHint: readOnly, IdempotentHint: d.Idempotent, OpenWorldHint: False}
	if !readOnly {
		ann.DestructiveHint = boolPtr(d.Delete || d.Exec)
		if d.Destructive != nil {
			ann.DestructiveHint = d.Destructive
		}
	}
	schema, _ := json.Marshal(InputSchema(d.Input))
	tool := &mcp.Tool{Name: name, Title: d.Title, Description: d.Description, Annotations: ann, InputSchema: json.RawMessage(schema)}
	props, handler := d.Input, d.Handler
	c.Server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var raw *js.Obj
		if args := req.Params.Arguments; len(args) > 0 && string(args) != "null" {
			v, err := js.Parse(args)
			o, isObj := v.(*js.Obj)
			if err != nil || !isObj {
				return validationError(name, "Invalid input: expected object, received "+typeName(v)), nil
			}
			raw = o
		}
		a, err := ParseArgs(props, raw)
		if err != nil {
			return validationError(name, err.Error()), nil
		}
		out, err := handler(ctx, a)
		if err != nil {
			return fail(err), nil
		}
		return ok(out), nil
	})
	if c.Registered != nil {
		c.Registered[name] = Info{Toolset: d.Toolset, Gate: d.gate(), Props: d.Input, Order: len(c.Registered)}
	}
}

func validationError(tool, msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("MCP error -32602: Input validation error: Invalid arguments for tool %s: %s", tool, msg)}},
		IsError: true,
	}
}

// ok renders a handler's value as JSON indented by two spaces.
func ok(data any) *mcp.CallToolResult {
	text := js.Stringify(data, "  ")
	if text == "undefined" {
		text = "null"
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func fail(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Error: " + err.Error()}}, IsError: true}
}

// Errorf is a plain tool error.
func Errorf(format string, args ...any) error { return fmt.Errorf(format, args...) }

// errMessage is err's message, like (err as Error).message.
func errMessage(err error) string {
	var pe *proxmox.Error
	if errors.As(err, &pe) {
		return pe.Message
	}
	return err.Error()
}

// Instructions are the server instructions for the given options.
func Instructions(opts config.Tools) string {
	var b strings.Builder
	b.WriteString("Tools for monitoring and managing a Proxmox VE cluster: nodes, VMs (qemu), containers (lxc), storage, backups, snapshots, firewall, HA, SDN, Ceph and access control. ")
	b.WriteString("Start with proxmox_get_cluster_status, proxmox_list_guests or proxmox_list_nodes. ")
	b.WriteString("Guests are addressed by VMID; the node is looked up automatically when omitted. Node-scoped tools default to PROXMOX_DEFAULT_NODE or the only node. ")
	b.WriteString("Long-running operations are Proxmox tasks: by default tools wait for them and report the outcome with the end of the task log; pass wait=false to return the task ID (UPID) and follow it with proxmox_get_task. ")
	if opts.AllowWrites {
		b.WriteString("Write tools are enabled: confirm with the user before stopping, rebooting, migrating, restoring or reconfiguring guests and nodes, and before any delete. ")
	} else {
		b.WriteString("The server is read-only; write tools are disabled (PROXMOX_ALLOW_WRITES=false). ")
	}
	if opts.AllowExec {
		b.WriteString("Command execution in guests and on nodes is enabled; show the user the exact command before running it.")
	}
	return b.String()
}

// NewServer creates the MCP server with the enabled tools and the prompts. registered, when
// not nil, receives each registered tool's toolset and gate.
func NewServer(client *proxmox.Client, opts config.Tools, registered map[string]Info) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: Version}, &mcp.ServerOptions{
		Instructions: Instructions(opts),
	})
	c := &Context{Server: server, Client: client, Tools: opts, Registered: registered}
	registerCluster(c)
	registerNodes(c)
	registerVms(c)
	registerContainers(c)
	registerAgent(c)
	registerSnapshots(c)
	registerStorage(c)
	registerBackup(c)
	registerTasks(c)
	registerAccess(c)
	registerFirewall(c)
	registerHa(c)
	registerSdn(c)
	registerCeph(c)
	registerPools(c)
	registerReplication(c)
	registerNotifications(c)
	registerHardware(c)
	registerRaw(c)
	registerPrompts(server)
	return server
}
