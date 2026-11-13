package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/config"
	"github.com/mattoddie/proxmox-mcp/internal/oauth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/crypto/bcrypt"
	xoauth "golang.org/x/oauth2"
)

func TestNativeOAuthMCP(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("password"), 10)
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	// Exercise the real configuration loader with an htpasswd-style bcrypt hash
	// and multiple trailing line breaks, then complete authorization and MCP calls.
	hashPath := filepath.Join(t.TempDir(), "admin")
	if err := os.WriteFile(hashPath, []byte(strings.Replace(string(hash), "$2a$", "$2y$", 1)+"\n\n\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(config.MapEnv(map[string]string{
		"PROXMOX_URL": "http://pve:8006", "PROXMOX_API_TOKEN": "test@pve!test=public-fixture",
		"MCP_TRANSPORT": "http", "MCP_AUTH_TOKEN": "legacy-static-token",
		"MCP_OAUTH_ISSUER": "https://mcp.example", "MCP_OAUTH_ADMIN_HASH_FILE": hashPath,
		"MCP_OAUTH_STATE_DIR": dir, "MCP_OAUTH_REDIRECT_URIS": "https://chatgpt.com/connector_platform_oauth_redirect",
	}))
	if err != nil {
		t.Fatal(err)
	}
	cfg := loaded.HTTP
	auth, e := oauth.New(cfg.OAuth)
	if e != nil {
		t.Fatal(e)
	}
	defer auth.Close()
	h := HandlerWithOAuth(cfg, mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil), auth)
	do := func(method, path, body, token string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://mcp.example"+path, strings.NewReader(body))
		if path == "/oauth/register" || path == "/mcp" {
			r.Header.Set("Content-Type", "application/json")
		} else {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		r.Header.Set("Origin", cfg.OAuth.Issuer)
		r.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := do("POST", "/mcp", "{}", "", nil)
	if w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), "oauth-protected-resource/mcp") {
		t.Fatal(w.Code, w.Header())
	}
	w = do("POST", "/oauth/register", `{"redirect_uris":["https://chatgpt.com/connector_platform_oauth_redirect"],"token_endpoint_auth_method":"none"}`, "", nil)
	var result map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	id, ok := result["client_id"].(string)
	if !ok {
		t.Fatal(w.Body.String())
	}
	verifier := xoauth.GenerateVerifier()
	q := url.Values{"client_id": {id}, "redirect_uri": {cfg.OAuth.Redirects[0]}, "response_type": {"code"}, "resource": {"https://mcp.example/mcp"}, "code_challenge_method": {"S256"}, "code_challenge": {xoauth.S256ChallengeFromVerifier(verifier)}}
	w = do("GET", "/oauth/authorize?"+q.Encode(), "", "", nil)
	match := regexp.MustCompile(`name="request" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if len(match) < 2 {
		t.Fatal(w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	w = do("POST", "/oauth/authorize", url.Values{"request": {match[1]}, "password": {"password"}, "decision": {"allow"}}.Encode(), "", cookie)
	redirect, _ := url.Parse(w.Header().Get("Location"))
	w = do("POST", "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {id}, "code": {redirect.Query().Get("code")}, "code_verifier": {verifier}, "resource": {"https://mcp.example/mcp"}, "redirect_uri": {cfg.OAuth.Redirects[0]}}.Encode(), "", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	token, ok := result["access_token"].(string)
	if !ok {
		t.Fatal(w.Body.String())
	}
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	for _, credential := range []string{token, cfg.AuthToken} {
		w = do("POST", "/mcp", initialize, credential, nil)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w = do("POST", "/mcp", initialize, "invalid", nil)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	// Force a failed snapshot: OAuth fails closed, static Bearer stays available.
	path := filepath.Join(dir, "state.json")
	if e = os.Remove(path); e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(path, 0700); e != nil {
		t.Fatal(e)
	}
	w = do("POST", "/oauth/revoke", url.Values{"client_id": {id}, "token": {token}}.Encode(), "", nil)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	if w = do("POST", "/mcp", initialize, token, nil); w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), "invalid_token") {
		t.Fatal(w.Code, w.Header())
	}
	if w = do("POST", "/mcp", initialize, cfg.AuthToken, nil); w.Code != 200 {
		t.Fatal("OAuth failure broke static auth", w.Code)
	}
}

func TestOAuthWithoutConfigStillRequiresAuthentication(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("password"), 10)
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	auth, e := oauth.New(oauth.Config{Issuer: "https://mcp.example", AdminHash: string(hash), Directory: dir, Redirects: []string{"https://chatgpt.com/connector_platform_oauth_redirect"}})
	if e != nil {
		t.Fatal(e)
	}
	defer auth.Close()
	h := HandlerWithOAuth(config.HTTP{}, mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil), auth)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "https://mcp.example/mcp", strings.NewReader("{}")))
	if w.Code != 401 {
		t.Fatal("OAuth handler accepted unauthenticated request")
	}
}
