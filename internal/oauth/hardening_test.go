package oauth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	xoauth "golang.org/x/oauth2"
	"golang.org/x/time/rate"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCIMD(t *testing.T) {
	s := setup(t)
	id := "https://chatgpt.com/oauth/client.json"
	doc := fmt.Sprintf(`{"client_id":%q,"client_name":"ChatGPT","redirect_uris":[%q],"token_endpoint_auth_method":"private_key_jwt","token_endpoint_auth_methods_supported":["none","private_key_jwt"]}`, id, s.cfg.Redirects[0])
	calls := 0
	s.metadataHTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != id {
			t.Fatal("unexpected fetch")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(doc))}, nil
	})
	for _, bad := range []string{"https://127.0.0.1/oauth/client.json", "https://chatgpt.com.evil/oauth/client.json", id + "?x=1", "https://user@chatgpt.com/oauth/client.json", "https://chatgpt.com:443/oauth/client.json", "https://chatgpt.com/oauth/../client.json"} {
		if _, e := s.fetchMetadata(context.Background(), bad); e == nil {
			t.Fatal("untrusted client accepted", bad)
		}
	}
	if calls != 0 {
		t.Fatal("SSRF fetch")
	}
	v := xoauth.GenerateVerifier()
	code := authorize(t, s, id, v)
	tokens := decode(t, exchange(s, id, code, v))
	if !s.Valid(tokens["access_token"].(string)) {
		t.Fatal("CIMD access")
	}
	original := doc
	for _, bad := range []string{strings.Replace(original, id, "https://wrong.example", 1), strings.Replace(original, `["none","private_key_jwt"]`, `["private_key_jwt"]`, 1), original + "{}", strings.Replace(original, `"client_name":"ChatGPT"`, `"client_name":"ChatGPT","client_name":"evil"`, 1)} {
		doc = bad
		if _, e := s.fetchMetadata(context.Background(), id); e == nil {
			t.Fatal("bad document accepted", bad)
		}
	}
	// A remote redirect must never be followed, even to another public host.
	s.metadataHTTP.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://127.0.0.1/private"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if _, e := s.fetchMetadata(context.Background(), id); e == nil {
		t.Fatal("redirect accepted")
	}
}
func TestStrictRegistration(t *testing.T) {
	s := setup(t)
	valid := fmt.Sprintf(`{"redirect_uris":[%q],"token_endpoint_auth_method":"none"}`, s.cfg.Redirects[0])
	for _, tc := range []struct{ body, content string }{
		{valid + "{}", "application/json"},
		{strings.Replace(valid, `"none"`, `"none","token_endpoint_auth_method":"client_secret_basic"`, 1), "application/json"},
		{strings.Replace(valid, `"none"`, `"client_secret_basic"`, 1), "application/json"},
		{valid, "text/plain"}, {valid, "application/json-bogus"},
		{strings.Repeat("x", 16385), "application/json"},
	} {
		r := httptest.NewRequest("POST", s.cfg.Issuer+"/oauth/register", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", tc.content)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatal(w.Code, tc.body)
		}
	}
	if len(s.state.Clients) != 0 {
		t.Fatal("rejected clients persisted")
	}
}
func TestCodeReplayRevokesAndConfigChangeRevokes(t *testing.T) {
	s := setup(t)
	id := register(t, s)
	v := xoauth.GenerateVerifier()
	c := authorize(t, s, id, v)
	tokens := decode(t, exchange(s, id, c, v))
	if exchange(s, id, c, v).Code != 400 || s.Valid(tokens["access_token"].(string)) {
		t.Fatal("code replay did not revoke")
	}
	c = authorize(t, s, id, v)
	tokens = decode(t, exchange(s, id, c, v))
	_ = s.Close()
	hash, _ := bcrypt.GenerateFromPassword([]byte("different-password"), 10)
	cfg := s.cfg
	cfg.AdminHash = string(hash)
	next, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer next.Close()
	if next.Valid(tokens["access_token"].(string)) || len(next.state.Codes) > 0 {
		t.Fatal("credential change did not revoke")
	}
	if _, ok := next.state.Clients[id]; !ok {
		t.Fatal("client ID lost")
	}
}
func TestInterruptedSnapshotRefusesRestart(t *testing.T) {
	s := setup(t)
	id := register(t, s)
	v := xoauth.GenerateVerifier()
	c := authorize(t, s, id, v)
	tokens := decode(t, exchange(s, id, c, v))
	path := filepath.Join(s.cfg.Directory, "state.json")
	if e := os.Remove(path); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(path, 0700); e != nil {
		t.Fatal(e)
	}
	w := request(s, "POST", "/oauth/revoke", url.Values{"client_id": {id}, "token": {tokens["access_token"].(string)}}, nil)
	if w.Code != 503 || s.Valid(tokens["access_token"].(string)) {
		t.Fatal("failure not closed")
	}
	if _, e := os.Stat(filepath.Join(s.cfg.Directory, "pending-write")); e != nil {
		t.Fatal("missing recovery marker", e)
	}
	_ = s.Close()
	if recovered, e := New(s.cfg); e == nil {
		recovered.Close()
		t.Fatal("interrupted state accepted")
	}
}
func TestIndependentRateLimits(t *testing.T) {
	s := setup(t)
	s.registrationLimit = rate.NewLimiter(rate.Every(time.Hour), 1)
	id := register(t, s)
	r := httptest.NewRequest("POST", s.cfg.Issuer+"/oauth/register", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("registration rate limit")
	}
	if w = request(s, "POST", "/oauth/revoke", url.Values{"client_id": {id}, "token": {"unknown"}}, nil); w.Code != 200 {
		t.Fatal("registration blocks revocation")
	}
	s.passwordLimit = rate.NewLimiter(rate.Every(time.Hour), 1)
	// Consume one failed password attempt, then ensure a fresh consent is throttled.
	for i := 0; i < 2; i++ {
		q := authQuery(s, id, xoauth.GenerateVerifier())
		w = request(s, "GET", "/oauth/authorize", q, nil)
		match := requestID(w.Body.String())
		cookie := w.Result().Cookies()[0]
		w = request(s, "POST", "/oauth/authorize", url.Values{"request": {match}, "decision": {"allow"}, "password": {"wrong"}}, cookie)
		if i == 0 && w.Code != 403 || i == 1 && w.Code != 429 {
			t.Fatal(i, w.Code)
		}
	}
}

func requestID(body string) string {
	return regexp.MustCompile(`name="request" value="([^"]+)"`).FindStringSubmatch(body)[1]
}

type blockingBody struct{ entered, release chan struct{} }

func (b *blockingBody) Read([]byte) (int, error) { close(b.entered); <-b.release; return 0, io.EOF }
func (*blockingBody) Close() error               { return nil }
func TestSlowBodiesDoNotBlockMCPValidation(t *testing.T) {
	s := setup(t)
	body := &blockingBody{make(chan struct{}), make(chan struct{})}
	r := httptest.NewRequest("POST", s.cfg.Issuer+"/oauth/token", nil)
	r.Body = body
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	done := make(chan struct{})
	go func() { s.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	<-body.entered
	validated := make(chan bool, 1)
	go func() { validated <- s.Valid("not-a-token") }()
	select {
	case <-validated:
	case <-time.After(time.Second):
		close(body.release)
		<-done
		t.Fatal("slow body held authentication lock")
	}
	close(body.release)
	<-done
}
func TestPrivateStateAndIssuerValidation(t *testing.T) {
	s := setup(t)
	for _, issuer := range []string{"https://mcp.example?", "https://mcp.example#"} {
		cfg := s.cfg
		cfg.Issuer = issuer
		if cfg.Validate() == nil {
			t.Fatal(issuer)
		}
	}
	_ = s.Close()
	path := filepath.Join(s.cfg.Directory, "state.json")
	if e := os.Chmod(path, 0644); e != nil {
		t.Fatal(e)
	}
	if next, e := New(s.cfg); e == nil {
		next.Close()
		t.Fatal("public state accepted")
	}
}

func TestNativeOriginBoundary(t *testing.T) {
	s := setup(t)
	for _, origin := range []string{"https://evil.example", "http://mcp.example", "null"} {
		r := httptest.NewRequest("POST", s.resource(), nil)
		r.Header.Set("Origin", origin)
		if s.RequestAllowed(r) {
			t.Fatal("untrusted origin", origin)
		}
	}
	r := httptest.NewRequest("POST", s.resource(), nil)
	if !s.RequestAllowed(r) {
		t.Fatal("server-to-server request rejected")
	}
	r.Header.Set("Origin", s.cfg.Issuer)
	if !s.RequestAllowed(r) {
		t.Fatal("same-origin request rejected")
	}
	r.Header.Add("Origin", s.cfg.Issuer)
	if s.RequestAllowed(r) {
		t.Fatal("duplicate origin")
	}
	r = httptest.NewRequest("POST", "https://wrong.example/mcp", nil)
	if s.RequestAllowed(r) {
		t.Fatal("wrong host")
	}
}
