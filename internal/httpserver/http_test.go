package httpserver

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mattoddie/proxmox-mcp/internal/config"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
	"github.com/mattoddie/proxmox-mcp/internal/tools"
)

func server(t *testing.T, kv ...string) (*httptest.Server, config.HTTP) {
	t.Helper()
	m := map[string]string{
		"PROXMOX_URL": "https://pve.invalid", "PROXMOX_TOKEN_ID": "root@pam!test", "PROXMOX_TOKEN_SECRET": "x",
		"MCP_TRANSPORT": "http",
	}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	cfg, err := config.Load(config.MapEnv(m))
	if err != nil {
		t.Fatal(err)
	}
	client, err := proxmox.New(cfg.Proxmox)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(cfg.HTTP, tools.NewServer(client, cfg.Tools, nil)))
	t.Cleanup(srv.Close)
	return srv, cfg.HTTP
}

type response struct {
	status int
	body   string
	header http.Header
}

func do(t *testing.T, method, url, body string, headers ...string) response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			req.Host = headers[i+1]
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return response{res.StatusCode, string(b), res.Header}
}

const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`

func TestHTTPTransport(t *testing.T) {
	srv, _ := server(t, "MCP_AUTH_TOKEN", "s3cret")

	t.Run("serves /health without auth", func(t *testing.T) {
		for _, p := range []string{"/health", "/healthz"} {
			r := do(t, "GET", srv.URL+p, "")
			if r.status != 200 || r.body != `{"status":"ok"}` {
				t.Fatal(p, r)
			}
		}
		if r := do(t, "GET", srv.URL+"/other", ""); r.status != 404 || r.body != `{"error":"Not found. The MCP endpoint is /mcp"}` {
			t.Fatal(r)
		}
	})

	t.Run("rejects requests without the bearer token", func(t *testing.T) {
		r := do(t, "POST", srv.URL+"/mcp", "{}", "Content-Type", "application/json")
		if r.status != 401 || r.header.Get("WWW-Authenticate") != "Bearer" || r.body != `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Unauthorized"},"id":null}` {
			t.Fatal(r)
		}
		for _, auth := range []string{"Bearer wrong", "s3cret", "Basic s3cret", "Bearer s3cret2", "Bearer "} {
			if r := do(t, "POST", srv.URL+"/mcp", "{}", "Authorization", auth); r.status != 401 {
				t.Fatal(auth, r.status)
			}
		}
		// The scheme is case-insensitive.
		if r := do(t, "GET", srv.URL+"/mcp", "", "Authorization", "bearer s3cret"); r.status != 405 {
			t.Fatal(r.status)
		}
	})

	t.Run("allows only POST once past auth", func(t *testing.T) {
		r := do(t, "GET", srv.URL+"/mcp", "", "Authorization", "Bearer s3cret")
		if r.status != 405 || r.header.Get("Allow") != "POST" || !strings.Contains(r.body, "Method not allowed") {
			t.Fatal(r)
		}
		if r := do(t, "DELETE", srv.URL+"/mcp", "", "Authorization", "Bearer s3cret"); r.status != 405 {
			t.Fatal(r.status)
		}
	})

	t.Run("refuses bodies that are too large or not JSON", func(t *testing.T) {
		r := do(t, "POST", srv.URL+"/mcp", "{nope", "Authorization", "Bearer s3cret", "Content-Type", "application/json")
		if r.status != 400 || !strings.Contains(r.body, "Invalid JSON body") {
			t.Fatal(r)
		}
		big := `{"x":"` + strings.Repeat("a", maxBodyBytes) + `"}`
		r = do(t, "POST", srv.URL+"/mcp", big, "Authorization", "Bearer s3cret", "Content-Type", "application/json")
		if r.status != 400 || !strings.Contains(r.body, "Request body too large") {
			t.Fatal(r.status, r.body)
		}
	})

	t.Run("answers MCP requests with the token", func(t *testing.T) {
		r := do(t, "POST", srv.URL+"/mcp", initialize, "Authorization", "Bearer s3cret", "Content-Type", "application/json", "Accept", "application/json, text/event-stream")
		if r.status != 200 || !strings.Contains(r.body, `"name":"proxmox-mcp"`) {
			t.Fatal(r)
		}
	})

	t.Run("lists tools over HTTP with the token", func(t *testing.T) {
		transport := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{"s3cret"}}}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(context.Background(), transport, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		found := false
		for tool, err := range cs.Tools(context.Background(), nil) {
			if err != nil {
				t.Fatal(err)
			}
			found = found || tool.Name == "proxmox_get_cluster_status"
		}
		if !found {
			t.Fatal("proxmox_get_cluster_status not listed")
		}
	})
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestListenRequiresAToken(t *testing.T) {
	t.Run("refuses to start without a token unless unauthenticated access is allowed", func(t *testing.T) {
		cfg := config.HTTP{Host: "127.0.0.1", Port: 0}
		if _, err := Listen(cfg); err != config.ErrAuthTokenRequired {
			t.Fatal(err)
		}
		cfg.AllowUnauthenticated = true
		ln, err := Listen(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		if host, _, _ := net.SplitHostPort(ln.Addr().String()); host != "127.0.0.1" {
			t.Fatal(host)
		}
		srv, _ := server(t, "MCP_ALLOW_UNAUTHENTICATED", "true")
		if r := do(t, "GET", srv.URL+"/mcp", ""); r.status != 405 { // past auth: only the method is wrong
			t.Fatal(r.status)
		}
	})
}

func TestHostAndOriginAllowList(t *testing.T) {
	srv, _ := server(t, "MCP_AUTH_TOKEN", "s3cret", "MCP_ALLOWED_HOSTS", "MCP.Example.LAN,localhost")
	status := func(headers ...string) int {
		t.Helper()
		return do(t, "GET", srv.URL+"/mcp", "", append([]string{"Authorization", "Bearer s3cret"}, headers...)...).status
	}

	t.Run("matches Host case-insensitively against lower-cased entries", func(t *testing.T) {
		for host, want := range map[string]int{"mcp.example.lan:8080": 405, "MCP.EXAMPLE.LAN": 405, "localhost:3000": 405, "evil.example": 403, "mcp.example.lan.evil": 403} {
			if got := status("Host", host); got != want {
				t.Errorf("Host %s: %d, want %d", host, got, want)
			}
		}
		r := do(t, "GET", srv.URL+"/mcp", "", "Host", "evil.example")
		if r.body != `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Host evil.example is not allowed"},"id":null}` {
			t.Fatal(r.body)
		}
	})

	t.Run("rejects requests from other origins", func(t *testing.T) {
		for origin, want := range map[string]int{
			"https://mcp.example.lan":     405,
			"http://localhost:3000":       405,
			"https://MCP.example.lan:443": 405,
			"https://evil.example":        403,
			"null":                        403,
			"":                            403,
		} {
			if got := status("Host", "mcp.example.lan", "Origin", origin); got != want {
				t.Errorf("Origin %q: %d, want %d", origin, got, want)
			}
		}
	})

	t.Run("checks the allow-list before auth", func(t *testing.T) {
		if r := do(t, "GET", srv.URL+"/mcp", "", "Host", "evil.example"); r.status != 403 {
			t.Fatal(r.status)
		}
	})
}

func TestNoHostCheckWithoutAllowList(t *testing.T) {
	// Like the original server, without MCP_ALLOWED_HOSTS any Host is served (e.g. behind a
	// reverse proxy on the same host that passes the public Host on).
	srv, _ := server(t, "MCP_AUTH_TOKEN", "s3cret")
	if r := do(t, "GET", srv.URL+"/mcp", "", "Authorization", "Bearer s3cret", "Host", "mcp.example.com"); r.status != 405 {
		t.Fatal(r.status)
	}
}
