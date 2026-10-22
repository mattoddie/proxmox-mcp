// Command proxmox-mcp is a Model Context Protocol server for Proxmox VE.
//
//	proxmox-mcp              serve MCP (stdio, or Streamable HTTP with MCP_TRANSPORT=http)
//	proxmox-mcp healthcheck  check the HTTP server is up (for Docker HEALTHCHECK)
//	proxmox-mcp version      print the version
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mattoddie/proxmox-mcp/internal/config"
	"github.com/mattoddie/proxmox-mcp/internal/httpserver"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
	"github.com/mattoddie/proxmox-mcp/internal/tools"
)

func main() {
	// Logs go to stderr: stdout is the MCP channel in stdio mode.
	log.SetFlags(0)
	log.SetOutput(os.Stderr)
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "healthcheck":
		err = healthcheck()
	case "version", "--version", "-v":
		fmt.Println(tools.Version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	if err != nil {
		log.Printf("proxmox-mcp: %v", err)
		os.Exit(1)
	}
}

const usage = `Usage: proxmox-mcp [command]

Commands:
  serve        Serve MCP (default). stdio, or Streamable HTTP with MCP_TRANSPORT=http
  healthcheck  Exit 0 if the HTTP server answers /health (for Docker HEALTHCHECK)
  version      Print the version

Configuration is by environment variable; see https://proxmox-mcp.mattoddie.dev/docs/configuration/
`

func serve() error {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}
	client, err := proxmox.New(cfg.Proxmox)
	if err != nil {
		return err
	}
	defer client.Close()
	t := cfg.Tools
	auth := "password (" + cfg.Proxmox.Username + ")"
	if cfg.Proxmox.TokenID != "" {
		auth = "API token " + cfg.Proxmox.TokenID
	}
	writes := "disabled"
	if t.AllowWrites {
		writes = "ENABLED"
	}
	if t.AllowDeletes {
		writes += " (with deletes)"
	}
	if t.AllowExec {
		writes += " (with exec)"
	}
	log.Printf("proxmox-mcp %s: Proxmox %s, auth %s, writes %s, toolsets %d, transport %s", tools.Version, cfg.Proxmox.URL, auth, writes, len(t.Toolsets), cfg.Transport)
	if t.AuditLog != "" {
		log.Printf("Audit log: %s", t.AuditLog)
	}
	p := cfg.Proxmox
	if strings.HasPrefix(strings.ToLower(p.URL), "http:") {
		log.Printf("WARNING: PROXMOX_URL uses plain http://, so the API token or password and every response travel in cleartext. Use https:// (port 8006) unless this is a trusted local link.")
	}
	if strings.HasPrefix(strings.ToLower(p.URL), "https:") && !p.VerifySSL && p.CACert == "" && len(p.TLSFingerprints) == 0 {
		log.Printf("WARNING: the Proxmox TLS certificate is not verified (PROXMOX_VERIFY_SSL=false), so the API token or password can be intercepted. " +
			"Pin the certificate with PROXMOX_TLS_FINGERPRINT (shown under Node > System > Certificates), set PROXMOX_CA_FILE, or use a trusted certificate with PROXMOX_VERIFY_SSL=true.")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	server := tools.NewServer(client, cfg.Tools, nil)
	if cfg.Transport == "stdio" {
		err := server.Run(ctx, &mcp.StdioTransport{})
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	ln, err := httpserver.Listen(cfg.HTTP)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           httpserver.Handler(cfg.HTTP, server),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: a tool call waiting for a Proxmox task can take minutes.
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	return httpserver.Serve(srv, ln, cfg.HTTP)
}

// healthcheckAddr is where the server listening on MCP_HTTP_HOST can be reached locally: that
// address itself, or loopback when it listens on every interface.
func healthcheckAddr(host, port string) string {
	switch strings.Trim(host, "[]") {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), port)
}

// healthcheck asks the local HTTP server for /health; with stdio there is nothing to check.
func healthcheck() error {
	get := func(name string) (string, error) { return config.Lookup(os.LookupEnv, name) }
	transport, err := get("MCP_TRANSPORT")
	if err != nil {
		return err
	}
	if transport == "stdio" {
		return nil
	}
	host, err := get("MCP_HTTP_HOST")
	if err != nil {
		return err
	}
	port, err := get("MCP_HTTP_PORT")
	if err != nil {
		return err
	}
	if port == "" {
		port = "8080"
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + healthcheckAddr(host, port) + "/health")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned %s", resp.Status)
	}
	return nil
}
