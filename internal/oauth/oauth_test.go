package oauth

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"
	"golang.org/x/time/rate"
)

func setup(t *testing.T) *Server {
	t.Helper()
	hash, _ := bcrypt.GenerateFromPassword([]byte("test-password"), 10)
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	s, e := New(Config{"https://mcp.example", string(hash), dir, []string{"https://chatgpt.com/connector_platform_oauth_redirect"}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.limit = rate.NewLimiter(rate.Inf, 1)
	s.registrationLimit = rate.NewLimiter(rate.Inf, 1)
	s.passwordLimit = rate.NewLimiter(rate.Inf, 1)
	return s
}
func request(s *Server, method, path string, v url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	body := ""
	if method == "GET" {
		path += "?" + v.Encode()
	} else {
		body = v.Encode()
	}
	r := httptest.NewRequest(method, "https://mcp.example"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", s.cfg.Issuer)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func register(t *testing.T, s *Server) string {
	t.Helper()
	r := httptest.NewRequest("POST", "https://mcp.example/oauth/register", strings.NewReader(`{"redirect_uris":["https://chatgpt.com/connector_platform_oauth_redirect"],"token_endpoint_auth_method":"none"}`))
	w := httptest.NewRecorder()
	r.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	return decode(t, w)["client_id"].(string)
}
func authQuery(s *Server, id, verifier string) url.Values {
	return url.Values{"client_id": {id}, "redirect_uri": {s.cfg.Redirects[0]}, "response_type": {"code"}, "resource": {s.resource()}, "code_challenge_method": {"S256"}, "code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)}, "state": {"original-state"}}
}
func authorize(t *testing.T, s *Server, id, verifier string) string {
	t.Helper()
	w := request(s, "GET", "/oauth/authorize", authQuery(s, id, verifier), nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	token := regexp.MustCompile(`name="request" value="([^"]+)"`).FindStringSubmatch(w.Body.String())[1]
	w = request(s, "POST", "/oauth/authorize", url.Values{"request": {token}, "password": {"test-password"}, "decision": {"allow"}}, w.Result().Cookies()[0])
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	u, _ := url.Parse(w.Header().Get("Location"))
	if u.Query().Get("state") != "original-state" || u.Query().Get("iss") != s.cfg.Issuer {
		t.Fatal(u)
	}
	return u.Query().Get("code")
}
func exchange(s *Server, id, c, v string) *httptest.ResponseRecorder {
	return request(s, "POST", "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {id}, "code": {c}, "code_verifier": {v}, "redirect_uri": {s.cfg.Redirects[0]}, "resource": {s.resource()}}, nil)
}
func TestFlowRotationReplayExpiryAndPersistence(t *testing.T) {
	s := setup(t)
	id := register(t, s)
	v := oauth2.GenerateVerifier()
	c := authorize(t, s, id, v)
	if w := exchange(s, id, c, oauth2.GenerateVerifier()); w.Code != 400 {
		t.Fatal(w.Code)
	}
	w := exchange(s, id, c, v)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	tokens := decode(t, w)
	if !s.Valid(tokens["access_token"].(string)) {
		t.Fatal("access rejected")
	}

	raw, e := os.ReadFile(s.cfg.Directory + "/state.json")
	if e != nil || strings.Contains(string(raw), tokens["access_token"].(string)) || strings.Contains(string(raw), tokens["refresh_token"].(string)) {
		t.Fatal("plaintext secret persisted")
	}
	refresh := url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "refresh_token": {tokens["refresh_token"].(string)}, "resource": {s.resource()}}
	w = request(s, "POST", "/oauth/token", refresh, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	next := decode(t, w)
	if s.Valid(tokens["access_token"].(string)) || !s.Valid(next["access_token"].(string)) {
		t.Fatal("rotation")
	}
	if request(s, "POST", "/oauth/token", refresh, nil).Code != 400 || s.Valid(next["access_token"].(string)) {
		t.Fatal("refresh replay must revoke family")
	}
	c = authorize(t, s, id, v)
	w = exchange(s, id, c, v)
	tokens = decode(t, w)
	s.now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	if s.Valid(tokens["access_token"].(string)) {
		t.Fatal("expired access")
	}
	s.now = time.Now
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s2, e := New(s.cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s2.Close()
	if !s2.Valid(tokens["access_token"].(string)) {
		t.Fatal("restart lost token")
	}
	s2.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	if s2.Valid(tokens["access_token"].(string)) {
		t.Fatal("expired grant")
	}
}
func TestAuthorizationBoundaries(t *testing.T) {
	s := setup(t)
	id := register(t, s)
	v := oauth2.GenerateVerifier()
	for _, field := range []string{"redirect_uri", "resource", "code_challenge_method", "response_type", "scope"} {
		q := authQuery(s, id, v)
		q.Set(field, "invalid")
		w := request(s, "GET", "/oauth/authorize", q, nil)
		if field == "redirect_uri" {
			if w.Code != 400 || w.Header().Get("Location") != "" {
				t.Fatal(field, w.Code)
			}
		} else {
			u, _ := url.Parse(w.Header().Get("Location"))
			if w.Code != 303 || u.Query().Get("iss") != s.cfg.Issuer || u.Query().Get("error") == "" {
				t.Fatal(field, w.Code)
			}
		}
	}
	for _, mode := range []string{"cookie", "password", "origin", "deny"} {
		w := request(s, "GET", "/oauth/authorize", authQuery(s, id, v), nil)
		token := regexp.MustCompile(`name="request" value="([^"]+)"`).FindStringSubmatch(w.Body.String())[1]
		cookie := w.Result().Cookies()[0]
		password := "test-password"
		decision := "allow"
		if mode == "cookie" {
			cookie = nil
		}
		if mode == "password" {
			password = "wrong"
		}
		if mode == "deny" {
			decision = "deny"
		}
		values := url.Values{"request": {token}, "password": {password}, "decision": {decision}}
		if mode == "origin" {
			r := httptest.NewRequest("POST", "https://mcp.example/oauth/authorize", strings.NewReader(values.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.AddCookie(cookie)
			w = httptest.NewRecorder()
			s.ServeHTTP(w, r)
		} else {
			w = request(s, "POST", "/oauth/authorize", values, cookie)
		}
		if mode == "deny" {
			if !strings.Contains(w.Header().Get("Location"), "error=access_denied") {
				t.Fatal("denial")
			}
		} else if w.Code != 403 {
			t.Fatal(mode, w.Code)
		}
		if len(s.state.Codes) != 0 {
			t.Fatal("unauthorized code")
		}
	}
}
func TestDiscoveryRegistrationRevocation(t *testing.T) {
	s := setup(t)
	w := request(s, "GET", "/.well-known/oauth-authorization-server", nil, nil)
	if decode(t, w)["issuer"] != s.cfg.Issuer {
		t.Fatal("issuer")
	}
	w = request(s, "GET", "/.well-known/oauth-protected-resource/mcp", nil, nil)
	if decode(t, w)["resource"] != s.resource() {
		t.Fatal("resource")
	}
	r := httptest.NewRequest("POST", "https://mcp.example/oauth/register", strings.NewReader(`{"redirect_uris":["https://evil.example/callback"]}`))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("redirect")
	}
	id := register(t, s)
	v := oauth2.GenerateVerifier()
	c := authorize(t, s, id, v)
	tokens := decode(t, exchange(s, id, c, v))
	w = request(s, "POST", "/oauth/revoke", url.Values{"client_id": {id}, "token": {tokens["refresh_token"].(string)}}, nil)
	if w.Code != 200 || s.Valid(tokens["access_token"].(string)) {
		t.Fatal("revocation")
	}
	if _, e := New(s.cfg); e == nil {
		t.Fatal("multi-process lock")
	}
	r = httptest.NewRequest("GET", "https://evil.example/.well-known/oauth-authorization-server", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	b, _ := io.ReadAll(w.Result().Body)
	if w.Code != 403 {
		t.Fatal(string(b))
	}
}

func TestTokenRejectionsAndStorageFailure(t *testing.T) {
	s := setup(t)
	id := register(t, s)
	v := oauth2.GenerateVerifier()
	c := authorize(t, s, id, v)
	base := url.Values{"grant_type": {"authorization_code"}, "client_id": {id}, "code": {c}, "code_verifier": {v}, "redirect_uri": {s.cfg.Redirects[0]}, "resource": {s.resource()}}
	for _, field := range []string{"client_id", "resource", "redirect_uri", "grant_type"} {
		q := url.Values{}
		for k, a := range base {
			q[k] = append([]string{}, a...)
		}
		q.Set(field, "wrong")
		if w := request(s, "POST", "/oauth/token", q, nil); w.Code < 400 {
			t.Fatal(field, w.Code)
		}
	}
	s.now = func() time.Time { return time.Now().Add(3 * time.Minute) }
	if w := request(s, "POST", "/oauth/token", base, nil); w.Code != 400 {
		t.Fatal("expired code")
	}
	s.now = time.Now
	c = authorize(t, s, id, v)
	base.Set("code", c)
	tokens := decode(t, request(s, "POST", "/oauth/token", base, nil))
	s.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	q := url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "resource": {s.resource()}, "refresh_token": {tokens["refresh_token"].(string)}}
	if w := request(s, "POST", "/oauth/token", q, nil); w.Code != 400 {
		t.Fatal("expired refresh")
	}
	s.now = time.Now
	c = authorize(t, s, id, v)
	base.Set("code", c)
	tokens = decode(t, request(s, "POST", "/oauth/token", base, nil))
	// A failed snapshot must disable even previously valid credentials.
	s.cfg.Directory = s.cfg.Directory + "/missing"
	q = url.Values{"client_id": {id}, "token": {tokens["refresh_token"].(string)}}
	if w := request(s, "POST", "/oauth/revoke", q, nil); w.Code != 503 {
		t.Fatal("expected storage failure")
	}
	if s.Valid(tokens["access_token"].(string)) {
		t.Fatal("failed closed")
	}
}

func TestConfigValidation(t *testing.T) {
	s := setup(t)
	for _, issuer := range []string{"http://mcp.example", "https://mcp.example/", "https://user@mcp.example", "https://mcp.example/path"} {
		c := s.cfg
		c.Issuer = issuer
		if c.Validate() == nil {
			t.Fatal(issuer)
		}
	}
	c := s.cfg
	c.AdminHash = "plaintext"
	if c.Validate() == nil {
		t.Fatal("plaintext accepted")
	}
}

func TestConcurrentCodeConsumption(t *testing.T) {
	s := setup(t)
	id := register(t, s)
	v := oauth2.GenerateVerifier()
	c := authorize(t, s, id, v)
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- exchange(s, id, c, v).Code }()
	}
	first, second := <-results, <-results
	if !((first == 200 && second == 400) || (first == 400 && second == 200)) {
		t.Fatal(first, second)
	}
}
