package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeRefusesHTTPWithoutAToken(t *testing.T) {
	t.Setenv("PROXMOX_URL", "https://pve.invalid")
	t.Setenv("PROXMOX_TOKEN_ID", "root@pam!test")
	t.Setenv("PROXMOX_TOKEN_SECRET", "x")
	t.Setenv("MCP_TRANSPORT", "http")
	t.Setenv("MCP_AUTH_TOKEN", "")
	if err := serve(); err == nil || !strings.Contains(err.Error(), "MCP_AUTH_TOKEN is required") {
		t.Fatalf("serve without a token: %v", err)
	}
}

func TestHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ok.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ok.URL, "http://"))
	t.Setenv("MCP_TRANSPORT", "http")
	t.Setenv("MCP_HTTP_PORT", port)
	if err := healthcheck(); err != nil {
		t.Fatal(err)
	}
	ok.Close()
	if err := healthcheck(); err == nil {
		t.Fatal("a stopped server is unhealthy")
	}
	t.Setenv("MCP_TRANSPORT", "stdio")
	if err := healthcheck(); err != nil {
		t.Fatal("stdio has nothing to check")
	}
}

func TestHealthcheckFollowsTheListenAddress(t *testing.T) {
	for host, want := range map[string]string{
		"":            "127.0.0.1:8080",
		"0.0.0.0":     "127.0.0.1:8080",
		"::":          "[::1]:8080",
		"[::]":        "[::1]:8080",
		"192.0.2.5":   "192.0.2.5:8080",
		"localhost":   "localhost:8080",
		"::1":         "[::1]:8080",
		"10.1.2.3":    "10.1.2.3:8080",
		"[fd00::1]":   "[fd00::1]:8080",
		"mcp.example": "mcp.example:8080",
	} {
		if got := healthcheckAddr(host, "8080"); got != want {
			t.Errorf("MCP_HTTP_HOST=%q: %s, want %s", host, got, want)
		}
	}

	ok := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer ok.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ok.URL, "http://"))
	t.Setenv("MCP_TRANSPORT", "http")
	t.Setenv("MCP_HTTP_PORT", "1")
	portFile := filepath.Join(t.TempDir(), "port")
	if err := os.WriteFile(portFile, []byte(port+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCP_HTTP_PORT_FILE", portFile)
	for _, host := range []string{"127.0.0.1", "0.0.0.0", ""} {
		t.Setenv("MCP_HTTP_HOST", host)
		if err := healthcheck(); err != nil {
			t.Fatalf("MCP_HTTP_HOST=%q: %v", host, err)
		}
	}
}
