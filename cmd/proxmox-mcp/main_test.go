package main

import (
	"net"
	"net/http"
	"net/http/httptest"
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
