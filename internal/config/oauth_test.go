package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestOAuthConfigAndSecretFile(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("test-only-password"), 10)
	path := filepath.Join(t.TempDir(), "admin")
	if e := os.WriteFile(path, append(hash, '\n'), 0600); e != nil {
		t.Fatal(e)
	}
	e := env("PROXMOX_URL", "https://pve:8006", "MCP_TRANSPORT", "http",
		"MCP_OAUTH_ISSUER", "https://mcp.example", "MCP_OAUTH_ADMIN_HASH_FILE", path,
		"MCP_OAUTH_STATE_DIR", "/state", "MCP_OAUTH_REDIRECT_URIS", "https://chatgpt.com/connector_platform_oauth_redirect")
	cfg, err := Load(e)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.OAuth.AdminHash != string(hash) || cfg.HTTP.AuthToken != "" || cfg.Tools.AllowWrites || cfg.Tools.AllowDeletes || cfg.Tools.AllowExec {
		t.Fatal("OAuth changed permissions or secret loading")
	}
	if err = CheckHTTPAuth(cfg.HTTP); err != nil {
		t.Fatal(err)
	}
	cfg.HTTP.AllowUnauthenticated = true
	if CheckHTTPAuth(cfg.HTTP) == nil {
		t.Fatal("unauthenticated mode accepted")
	}
	cfg.HTTP.AllowUnauthenticated = false
	cfg.HTTP.AuthToken = "short"
	if CheckHTTPAuth(cfg.HTTP) == nil {
		t.Fatal("weak static token accepted")
	}
	cfg.HTTP.AuthToken = "valid-static-token"
	if CheckHTTPAuth(cfg.HTTP) != nil {
		t.Fatal("legacy auth rejected")
	}
}

func TestOAuthHashFileWhitespaceAndPrecedence(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("test-only-password"), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"2a", "2b", "2y"} {
		for name, padding := range map[string][2]string{
			"none":       {"", ""},
			"lf":         {"", "\n"},
			"crlf":       {"", "\r\n"},
			"extra_lf":   {"", "\n\n\n"},
			"extra_crlf": {"", "\r\n\r\n"},
			"surround":   {"\n \t", " \t\r\n\n"},
		} {
			t.Run(variant+"/"+name, func(t *testing.T) {
				want := strings.Replace(string(hash), "$2a$", "$"+variant+"$", 1)
				path := filepath.Join(t.TempDir(), "admin")
				if err := os.WriteFile(path, []byte(padding[0]+want+padding[1]), 0600); err != nil {
					t.Fatal(err)
				}
				cfg, err := Load(env("PROXMOX_URL", "https://pve:8006", "MCP_TRANSPORT", "http",
					"MCP_OAUTH_ISSUER", "https://mcp.example", "MCP_OAUTH_ADMIN_HASH_FILE", path,
					// The mounted file must override a stale environment hash.
					"MCP_OAUTH_ADMIN_HASH", "not-a-hash", "MCP_OAUTH_STATE_DIR", "/state",
					"MCP_OAUTH_REDIRECT_URIS", "https://chatgpt.com/connector_platform_oauth_redirect"))
				if err != nil {
					t.Fatal(err)
				}
				if cfg.HTTP.OAuth.AdminHash != want {
					t.Fatal("hash file whitespace was not normalized")
				}
				if bcrypt.CompareHashAndPassword([]byte(cfg.HTTP.OAuth.AdminHash), []byte("test-only-password")) != nil {
					t.Fatal("loaded bcrypt hash rejected correct password")
				}
			})
		}
	}
}
