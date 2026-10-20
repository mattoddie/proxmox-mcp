package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// PROXMOX_VERIFY_SSL=false is set throughout (the URLs are https), except where a test is about it.
var tokenEnv = map[string]string{"PROXMOX_TOKEN_ID": "root@pam!test", "PROXMOX_TOKEN_SECRET": "00000000-1111-2222-3333-444444444444", "PROXMOX_VERIFY_SSL": "false"}

func env(kv ...string) Env {
	m := map[string]string{}
	for k, v := range tokenEnv {
		m[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return MapEnv(m)
}

func plain(kv ...string) Env {
	m := map[string]string{"PROXMOX_VERIFY_SSL": "false"}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return MapEnv(m)
}

func wantErr(t *testing.T, e Env, re string) {
	t.Helper()
	_, err := Load(e)
	if err == nil || !regexp.MustCompile(re).MatchString(err.Error()) {
		t.Fatalf("got %v, want an error matching /%s/", err, re)
	}
}

func load(t *testing.T, e Env) *Config {
	t.Helper()
	cfg, err := Load(e)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestConfig(t *testing.T) {
	t.Run("requires a URL and credentials", func(t *testing.T) {
		wantErr(t, plain(), `PROXMOX_URL is required`)
		wantErr(t, plain("PROXMOX_URL", "https://pve:8006"), `API token .* or both PROXMOX_USERNAME`)
		wantErr(t, env("PROXMOX_URL", "ftp://pve"), `must use https`)
		wantErr(t, env("PROXMOX_URL", "not a url"), `PROXMOX_URL is not a valid URL: not a url`)
		wantErr(t, plain("PROXMOX_URL", "https://pve", "PROXMOX_USERNAME", "root"), `or both PROXMOX_USERNAME and PROXMOX_PASSWORD`)
	})

	t.Run("applies defaults", func(t *testing.T) {
		cfg := load(t, env("PROXMOX_URL", "https://pve:8006/api2/json/"))
		p := cfg.Proxmox
		if p.URL != "https://pve:8006" || p.VerifySSL || p.TimeoutMs != 30000 || p.TaskTimeoutMs != 120000 || p.DefaultNode != "" {
			t.Fatalf("%+v", p)
		}
		tl := cfg.Tools
		if tl.AllowWrites || tl.AllowDeletes || tl.AllowExec || len(tl.Toolsets) != len(Toolsets)-1 || tl.Toolsets["raw"] {
			t.Fatalf("%+v", tl)
		}
		h := cfg.HTTP
		if cfg.Transport != "stdio" || h.Host != "127.0.0.1" || h.Port != 8080 || h.AllowUnauthenticated || h.AllowedHosts != nil {
			t.Fatalf("%+v %+v", cfg.Transport, h)
		}
		if load(t, env("PROXMOX_URL", "https://pve//")).Proxmox.URL != "https://pve" {
			t.Fatal("trailing slashes")
		}
	})

	t.Run("requires MCP_AUTH_TOKEN for the http transport unless explicitly waived", func(t *testing.T) {
		base := []string{"PROXMOX_URL", "https://pve", "MCP_TRANSPORT", "http"}
		wantErr(t, env(base...), `^MCP_AUTH_TOKEN is required for the http transport \(generate one with: openssl rand -hex 32\); set MCP_ALLOW_UNAUTHENTICATED=true only behind something else that authenticates$`)
		wantErr(t, env(append(base, "MCP_AUTH_TOKEN", "  ")...), `MCP_AUTH_TOKEN is required`)
		wantErr(t, env(append(base, "MCP_ALLOW_UNAUTHENTICATED", "false")...), `MCP_AUTH_TOKEN is required`)
		if load(t, env(append(base, "MCP_AUTH_TOKEN", "xxxxxxxxxxxxxxxx")...)).HTTP.AuthToken != "xxxxxxxxxxxxxxxx" {
			t.Fatal("token")
		}
		// Short tokens can be guessed.
		wantErr(t, env(append(base, "MCP_AUTH_TOKEN", "t")...), `^MCP_AUTH_TOKEN must be at least 16 characters long`)
		wantErr(t, env(append(base, "MCP_AUTH_TOKEN", "xxxxxxxxxxxxxxx")...), `at least 16 characters`)
		// Without a token, only a Host allow-list stops DNS rebinding.
		wantErr(t, env(append(base, "MCP_ALLOW_UNAUTHENTICATED", "true")...), `^MCP_ALLOW_UNAUTHENTICATED=true also requires MCP_ALLOWED_HOSTS`)
		open := load(t, env(append(base, "MCP_ALLOW_UNAUTHENTICATED", "true", "MCP_ALLOWED_HOSTS", "mcp.lan")...))
		if !open.HTTP.AllowUnauthenticated || open.HTTP.AuthToken != "" {
			t.Fatalf("%+v", open.HTTP)
		}
		// stdio ignores the HTTP settings.
		load(t, env("PROXMOX_URL", "https://pve", "MCP_AUTH_TOKEN", "short", "MCP_ALLOW_UNAUTHENTICATED", "true"))
		// stdio doesn't listen anywhere, so it needs no token.
		if load(t, env("PROXMOX_URL", "https://pve")).HTTP.AuthToken != "" {
			t.Fatal("stdio")
		}
		wantErr(t, env("PROXMOX_URL", "https://pve", "MCP_TRANSPORT", "sse"), `MCP_TRANSPORT must be stdio or http, got "sse"`)
	})

	t.Run("lower-cases MCP_ALLOWED_HOSTS", func(t *testing.T) {
		cfg := load(t, env("PROXMOX_URL", "https://pve", "MCP_ALLOWED_HOSTS", "MCP.Example.LAN, Localhost:8080,"))
		if !reflect.DeepEqual(cfg.HTTP.AllowedHosts, []string{"mcp.example.lan", "localhost:8080"}) {
			t.Fatal(cfg.HTTP.AllowedHosts)
		}
	})

	t.Run("parses the combined token form and validates token ids", func(t *testing.T) {
		cfg := load(t, plain("PROXMOX_URL", "https://pve", "PROXMOX_API_TOKEN", "PVEAPIToken=mcp@pve!ro=abc-def"))
		if cfg.Proxmox.TokenID != "mcp@pve!ro" || cfg.Proxmox.TokenSecret != "abc-def" {
			t.Fatalf("%+v", cfg.Proxmox)
		}
		wantErr(t, plain("PROXMOX_URL", "https://pve", "PROXMOX_TOKEN_ID", "root", "PROXMOX_TOKEN_SECRET", "x"), `user@realm!tokenid`)
		wantErr(t, plain("PROXMOX_URL", "https://pve", "PROXMOX_TOKEN_ID", "root@pam!x"), `Set both`)
		wantErr(t, plain("PROXMOX_URL", "https://pve", "PROXMOX_API_TOKEN", "=x"), `PROXMOX_API_TOKEN must look like user@realm!tokenid=secret`)
		// A token wins over a password, which is then not kept.
		both := load(t, env("PROXMOX_URL", "https://pve", "PROXMOX_USERNAME", "a", "PROXMOX_PASSWORD", "b"))
		if both.Proxmox.Username != "" || both.Proxmox.Password != "" {
			t.Fatalf("%+v", both.Proxmox)
		}
	})

	t.Run("adds the default realm to bare usernames", func(t *testing.T) {
		if u := load(t, plain("PROXMOX_URL", "https://pve", "PROXMOX_USERNAME", "admin", "PROXMOX_PASSWORD", "pw")).Proxmox.Username; u != "admin@pam" {
			t.Fatal(u)
		}
		if u := load(t, plain("PROXMOX_URL", "https://pve", "PROXMOX_USERNAME", "admin", "PROXMOX_PASSWORD", "pw", "PROXMOX_REALM", "pve")).Proxmox.Username; u != "admin@pve" {
			t.Fatal(u)
		}
	})

	t.Run("parses permission gating and toolsets", func(t *testing.T) {
		wantErr(t, env("PROXMOX_URL", "https://pve", "PROXMOX_ALLOW_DELETES", "true"), `also requires PROXMOX_ALLOW_WRITES`)
		wantErr(t, env("PROXMOX_URL", "https://pve", "PROXMOX_ALLOW_EXEC", "true"), `also requires PROXMOX_ALLOW_WRITES`)
		cfg := load(t, env("PROXMOX_URL", "https://pve", "PROXMOX_ALLOW_WRITES", "1", "PROXMOX_ALLOW_DELETES", "yes", "PROXMOX_ALLOW_EXEC", "on", "PROXMOX_TOOLSETS", "cluster, VMS"))
		if !cfg.Tools.AllowDeletes || !cfg.Tools.AllowExec || cfg.Tools.AllowUnredacted {
			t.Fatalf("%+v", cfg.Tools)
		}
		if !load(t, env("PROXMOX_URL", "https://pve", "PROXMOX_ALLOW_UNREDACTED", "true")).Tools.AllowUnredacted {
			t.Fatal("PROXMOX_ALLOW_UNREDACTED")
		}
		var names []string
		for ts := range cfg.Tools.Toolsets {
			names = append(names, ts)
		}
		sort.Strings(names)
		if !reflect.DeepEqual(names, []string{"cluster", "vms"}) {
			t.Fatal(names)
		}
		wantErr(t, env("PROXMOX_URL", "https://pve", "PROXMOX_TOOLSETS", "cluster,bogus"), `Unknown toolset "bogus"`)
		// Anything but 1/true/yes/on is false, as before.
		if load(t, env("PROXMOX_URL", "https://pve", "PROXMOX_ALLOW_WRITES", "maybe")).Tools.AllowWrites {
			t.Fatal("maybe is false")
		}
	})

	t.Run("makes the operator choose how to check an https certificate", func(t *testing.T) {
		base := map[string]string{"PROXMOX_URL": "https://pve:8006", "PROXMOX_API_TOKEN": "root@pam!t=x"}
		e := func(kv ...string) Env {
			m := map[string]string{}
			for k, v := range base {
				m[k] = v
			}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i]] = kv[i+1]
			}
			return MapEnv(m)
		}
		wantErr(t, e(), `^PROXMOX_URL uses https, but no way to check the Proxmox certificate is configured\..*PROXMOX_TLS_FINGERPRINT=.*PROXMOX_CA_FILE=.*PROXMOX_VERIFY_SSL=true.*PROXMOX_VERIFY_SSL=false`)
		wantErr(t, e("PROXMOX_VERIFY_SSL", " "), `no way to check the Proxmox certificate`)
		if load(t, e("PROXMOX_VERIFY_SSL", "false")).Proxmox.VerifySSL {
			t.Fatal("explicitly off")
		}
		if !load(t, e("PROXMOX_VERIFY_SSL", "true")).Proxmox.VerifySSL {
			t.Fatal("on")
		}
		load(t, e("PROXMOX_TLS_FINGERPRINT", strings.Repeat("ab", 32)))
		ca := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(ca, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		load(t, e("PROXMOX_CA_FILE", ca))
		// Plain http has no certificate to check.
		load(t, e("PROXMOX_URL", "http://pve:8006"))
	})

	t.Run("refuses credentials in PROXMOX_URL without echoing them", func(t *testing.T) {
		for _, u := range []string{"https://root:hunter2@pve:8006", "https://root@pve:8006", "http://:hunter2@pve"} {
			_, err := Load(env("PROXMOX_URL", u))
			if err == nil || !strings.Contains(err.Error(), "must not contain a user name or password") || strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("%s: %v", u, err)
			}
		}
	})

	t.Run("keeps a password's leading and trailing spaces", func(t *testing.T) {
		pw := load(t, plain("PROXMOX_URL", "https://pve", "PROXMOX_USERNAME", "admin@pve", "PROXMOX_PASSWORD", "  pass word ")).Proxmox.Password
		if pw != "  pass word " {
			t.Fatalf("%q", pw)
		}
		file := filepath.Join(t.TempDir(), "pw")
		if err := os.WriteFile(file, []byte(" pw \r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if pw := load(t, plain("PROXMOX_URL", "https://pve", "PROXMOX_USERNAME", "admin@pve", "PROXMOX_PASSWORD_FILE", file)).Proxmox.Password; pw != " pw " {
			t.Fatalf("%q", pw)
		}
	})

	t.Run("leaves the raw toolset out of the default and of all", func(t *testing.T) {
		for _, list := range []string{"", "all", "ALL, cluster"} {
			ts := load(t, env("PROXMOX_URL", "https://pve", "PROXMOX_TOOLSETS", list)).Tools.Toolsets
			if ts["raw"] || !ts["cluster"] || !ts["hardware"] || len(ts) != len(Toolsets)-1 {
				t.Fatalf("PROXMOX_TOOLSETS=%q: %v", list, ts)
			}
		}
		for _, list := range []string{"all,raw", "raw, all", "RAW"} {
			if !load(t, env("PROXMOX_URL", "https://pve", "PROXMOX_TOOLSETS", list)).Tools.Toolsets["raw"] {
				t.Fatalf("PROXMOX_TOOLSETS=%q should enable raw", list)
			}
		}
		if ts := load(t, env("PROXMOX_URL", "https://pve", "PROXMOX_TOOLSETS", "all,raw")).Tools.Toolsets; len(ts) != len(Toolsets) {
			t.Fatal(ts)
		}
	})

	t.Run("parses numbers like parseInt", func(t *testing.T) {
		cfg := load(t, env("PROXMOX_URL", "https://pve", "PROXMOX_TIMEOUT_MS", "5000ms", "PROXMOX_TASK_TIMEOUT_MS", "60000", "MCP_HTTP_PORT", "9000"))
		if cfg.Proxmox.TimeoutMs != 5000 || cfg.Proxmox.TaskTimeoutMs != 60000 || cfg.HTTP.Port != 9000 {
			t.Fatalf("%+v", cfg)
		}
		wantErr(t, env("PROXMOX_URL", "https://pve", "PROXMOX_TASK_TIMEOUT_MS", "0"), `PROXMOX_TASK_TIMEOUT_MS must be a positive integer, got "0"`)
		wantErr(t, env("PROXMOX_URL", "https://pve", "MCP_HTTP_PORT", "http"), `MCP_HTTP_PORT must be a positive integer`)
	})

	t.Run("reads secrets from _FILE variables", func(t *testing.T) {
		dir := t.TempDir()
		secret := filepath.Join(dir, "secret")
		if err := os.WriteFile(secret, []byte("  s3cret-s3cret-s3cret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := load(t, plain("PROXMOX_URL", "https://pve", "PROXMOX_TOKEN_ID", "a@pam!b", "PROXMOX_TOKEN_SECRET_FILE", secret, "PROXMOX_TOKEN_SECRET", "ignored", "MCP_TRANSPORT", "http", "MCP_AUTH_TOKEN_FILE", secret))
		if cfg.Proxmox.TokenSecret != "s3cret-s3cret-s3cret" || cfg.HTTP.AuthToken != "s3cret-s3cret-s3cret" {
			t.Fatalf("%+v %+v", cfg.Proxmox, cfg.HTTP)
		}
		wantErr(t, plain("PROXMOX_URL_FILE", filepath.Join(dir, "missing")), `^Could not read PROXMOX_URL_FILE \(.*missing\): no such file or directory$`)
		cfg = load(t, env("PROXMOX_URL", "https://pve", "PROXMOX_DEFAULT_NODE", "pve2", "PROXMOX_VERIFY_SSL", "TRUE"))
		if cfg.Proxmox.DefaultNode != "pve2" || !cfg.Proxmox.VerifySSL {
			t.Fatalf("%+v", cfg.Proxmox)
		}
	})
}
