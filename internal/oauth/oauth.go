// Package oauth provides the optional single-administrator MCP authorization server.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"
	"golang.org/x/time/rate"
)

type Config struct {
	Issuer, AdminHash, Directory string
	Redirects                    []string
}

func (c Config) Enabled() bool { return c.Issuer != "" }
func (c Config) Validate() error {
	if !c.Enabled() {
		if c.AdminHash != "" || c.Directory != "" || len(c.Redirects) > 0 {
			return errors.New("partial OAuth configuration: MCP_OAUTH_ISSUER required")
		}
		return nil
	}
	u, e := url.Parse(c.Issuer)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || c.Issuer != "https://"+u.Host {
		return errors.New("MCP_OAUTH_ISSUER must be an HTTPS origin without a trailing slash")
	}
	if cost, err := bcrypt.Cost([]byte(c.AdminHash)); err != nil || cost < 10 || cost > 14 {
		return errors.New("MCP_OAUTH_ADMIN_HASH must be a bcrypt hash with cost 10 through 14")
	}
	if !filepath.IsAbs(c.Directory) || len(c.Redirects) == 0 {
		return errors.New("OAuth requires an absolute state directory and exact redirect allowlist")
	}
	for _, s := range c.Redirects {
		u, e := url.Parse(s)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.Contains(s, "#") {
			return errors.New("OAuth redirect allowlist must contain exact HTTPS URLs")
		}
	}
	return nil
}

type client struct {
	Redirects []string `json:"redirect_uris"`
	Name      string   `json:"client_name"`
}
type grant struct {
	Client, Resource, Access, Refresh, Code string
	Used                                    []string
	AccessUntil, Until                      time.Time
}
type code struct {
	Client, Redirect, Challenge, Resource string
	Until                                 time.Time
}
type state struct {
	Binding string
	Clients map[string]client
	Codes   map[string]code
	Grants  map[string]grant
}
type pending struct {
	Query  url.Values
	Cookie string
	Until  time.Time
}
type Server struct {
	cfg               Config
	mu                sync.Mutex
	state             state
	pending           map[string]pending
	lock              *flock.Flock
	limit             *rate.Limiter
	registrationLimit *rate.Limiter
	passwordLimit     *rate.Limiter
	metadataHTTP      *http.Client
	now               func() time.Time
	broken            bool
	diagnostics       *log.Logger
}

func random() string { return base64.RawURLEncoding.EncodeToString(randBytes()) }
func randBytes() []byte {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return b
}
func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func New(c Config) (*Server, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	if !c.Enabled() {
		return nil, errors.New("OAuth disabled")
	}
	if e := os.MkdirAll(c.Directory, 0700); e != nil {
		return nil, e
	}
	info, e := os.Lstat(c.Directory)
	if e != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("OAuth state directory must be private (0700)")
	}
	f := flock.New(filepath.Join(c.Directory, "lock"))
	locked, e := f.TryLock()
	if e != nil || !locked {
		return nil, errors.New("OAuth state already in use")
	}
	s := &Server{diagnostics: log.Default(), cfg: c, lock: f, pending: map[string]pending{}, limit: rate.NewLimiter(10, 20), registrationLimit: rate.NewLimiter(rate.Every(10*time.Second), 5), passwordLimit: rate.NewLimiter(rate.Every(3*time.Second), 5), now: time.Now, metadataHTTP: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	s.state = state{Clients: map[string]client{}, Codes: map[string]code{}, Grants: map[string]grant{}}
	if _, e := os.Lstat(filepath.Join(c.Directory, "pending-write")); e == nil || !os.IsNotExist(e) {
		s.Close()
		return nil, errors.New("OAuth interrupted write: reset state before recovery")
	}
	b, e := readState(c.Directory)
	if e == nil {
		e = json.Unmarshal(b, &s.state)
	}
	if e != nil && !os.IsNotExist(e) {
		s.Close()
		return nil, errors.New("cannot load OAuth state")
	}
	if s.state.Clients == nil || s.state.Codes == nil || s.state.Grants == nil {
		s.Close()
		return nil, errors.New("invalid OAuth state")
	}
	binding := digest(c.Issuer + "\x00" + c.AdminHash + "\x00" + strings.Join(c.Redirects, "\x00"))
	if s.state.Binding != binding {
		s.state.Binding = binding
		s.state.Codes = map[string]code{}
		s.state.Grants = map[string]grant{}
	}
	if e = s.save(); e != nil {
		s.Close()
		return nil, errors.New("cannot persist OAuth state")
	}
	return s, nil
}
func (s *Server) Close() error { return s.lock.Close() }
func (s *Server) save() (err error) {
	defer func() {
		if err != nil {
			s.broken = true
		}
	}()
	// A durable marker prevents restart from resurrecting an older snapshot.
	marker := filepath.Join(s.cfg.Directory, "pending-write")
	f, e := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if e != nil {
		return e
	}
	e = f.Sync()
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = syncDirectory(s.cfg.Directory); e != nil {
		return e
	}
	b, e := json.Marshal(s.state)
	if e != nil {
		return e
	}
	f, e = os.CreateTemp(s.cfg.Directory, ".state-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce = f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(f.Name(), filepath.Join(s.cfg.Directory, "state.json")); e != nil {
		return e
	}
	if e = syncDirectory(s.cfg.Directory); e != nil {
		return e
	}
	if e = os.Remove(marker); e != nil {
		return e
	}
	return syncDirectory(s.cfg.Directory)
}
func jsonReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code string) {
	jsonReply(w, status, map[string]string{"error": code})
}
func (s *Server) resource() string { return s.cfg.Issuer + "/mcp" }
func (s *Server) Challenge() string {
	return fmt.Sprintf("Bearer scope=\"mcp\", resource_metadata=%q", s.cfg.Issuer+"/.well-known/oauth-protected-resource/mcp")
}
func (s *Server) Valid(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken {
		return false
	}
	h := digest(token)
	for _, g := range s.state.Grants {
		if g.Access == h && s.now().Before(g.AccessUntil) && s.now().Before(g.Until) && g.Resource == s.resource() {
			return true
		}
	}
	return false
}
func (s *Server) Register(m *http.ServeMux) {
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server", "/oauth/register", "/oauth/authorize", "/oauth/token", "/oauth/revoke"} {
		m.Handle(p, s)
	}
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	u, _ := url.Parse(s.cfg.Issuer)
	if r.Host != u.Host {
		s.postRejection(r, "invalid_host", 403)
		fail(w, 403, "invalid_request")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/.well-known/") {
		if r.Method != "GET" {
			fail(w, 405, "invalid_request")
			return
		}
		if strings.Contains(r.URL.Path, "oauth-protected-resource") {
			jsonReply(w, 200, map[string]any{"resource": s.resource(), "authorization_servers": []string{s.cfg.Issuer}, "scopes_supported": []string{"mcp"}, "bearer_methods_supported": []string{"header"}})
		} else {
			jsonReply(w, 200, map[string]any{"issuer": s.cfg.Issuer, "authorization_endpoint": s.cfg.Issuer + "/oauth/authorize", "token_endpoint": s.cfg.Issuer + "/oauth/token", "registration_endpoint": s.cfg.Issuer + "/oauth/register", "revocation_endpoint": s.cfg.Issuer + "/oauth/revoke", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}, "scopes_supported": []string{"mcp"}, "authorization_response_iss_parameter_supported": true, "client_id_metadata_document_supported": true, "revocation_endpoint_auth_methods_supported": []string{"none"}})
		}
		return
	}
	limiter := s.limit
	if r.URL.Path == "/oauth/register" {
		limiter = s.registrationLimit
	}
	if !limiter.Allow() {
		w.Header().Set("Retry-After", "10")
		s.postRejection(r, "rate_limited", 429)
		fail(w, 429, "temporarily_unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	// Never hold the state mutex while reading a client-controlled body.
	if r.Method == "POST" {
		b, e := io.ReadAll(r.Body)
		if e != nil {
			s.postRejection(r, "malformed_request", 400)
			fail(w, 400, "invalid_request")
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		want := "application/x-www-form-urlencoded"
		if r.URL.Path == "/oauth/register" {
			want = "application/json"
		}
		media, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if e != nil || media != want {
			s.postRejection(r, "malformed_request", 400)
			fail(w, 400, "invalid_request")
			return
		}
		if want != "application/json" && (r.ParseForm() != nil || !single(r.PostForm)) {
			s.postRejection(r, "malformed_request", 400)
			fail(w, 400, "invalid_request")
			return
		}
	}
	var resolved *client
	if r.URL.Path == "/oauth/authorize" && r.Method == "GET" {
		cid := r.URL.Query().Get("client_id")
		if strings.HasPrefix(cid, "https:") {
			c, e := s.fetchMetadata(r.Context(), cid)
			if e != nil {
				fail(w, 400, "invalid_client")
				return
			}
			resolved = &c
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken {
		s.postRejection(r, "state_persistence_failed", 503)
		fail(w, 503, "temporarily_unavailable")
		return
	}
	if resolved != nil {
		cid := r.URL.Query().Get("client_id")
		if _, ok := s.state.Clients[cid]; !ok && len(s.state.Clients) >= 100 {
			fail(w, 503, "temporarily_unavailable")
			return
		}
		s.state.Clients[cid] = *resolved
	}
	// Keep only the submitted request until authorize can diagnose its expiry.
	// Requests rejected before consent lookup still receive normal cleanup.
	submittedConsent := ""
	if r.Method == "POST" && r.URL.Path == "/oauth/authorize" && r.URL.RawQuery == "" && len(r.Header.Values("Origin")) == 1 && r.Header.Get("Origin") == s.cfg.Issuer {
		submittedConsent = digest(r.PostForm.Get("request"))
	}
	for k, v := range s.pending {
		if k == submittedConsent {
			continue
		}
		if !s.now().Before(v.Until) {
			delete(s.pending, k)
		}
	}
	for k, v := range s.state.Codes {
		if !s.now().Before(v.Until) {
			delete(s.state.Codes, k)
		}
	}
	for k, v := range s.state.Grants {
		if !s.now().Before(v.Until) {
			delete(s.state.Grants, k)
		}
	}
	if r.URL.Path == "/oauth/register" {
		s.registration(w, r)
		return
	}
	if r.URL.Path == "/oauth/authorize" {
		s.authorize(w, r)
		return
	}
	if r.Method != "POST" || r.URL.RawQuery != "" {
		fail(w, 405, "invalid_request")
		return
	}
	if r.ParseForm() != nil || !single(r.PostForm) {
		fail(w, 400, "invalid_request")
		return
	}
	if r.URL.Path == "/oauth/revoke" {
		s.revoke(w, r)
		return
	}
	s.token(w, r)
}
func single(v url.Values) bool {
	for _, a := range v {
		if len(a) != 1 {
			return false
		}
	}
	return true
}
func contains(a []string, b string) bool {
	for _, v := range a {
		if v == b {
			return true
		}
	}
	return false
}
func (s *Server) registration(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || r.URL.RawQuery != "" {
		fail(w, 405, "invalid_request")
		return
	}
	var v metadata
	if decodeMetadata(r.Body, &v) != nil || v.Auth != "none" || v.Methods != nil || !v.validPublic() {
		fail(w, 400, "invalid_client_metadata")
		return
	}
	for _, u := range v.Redirects {
		if !contains(s.cfg.Redirects, u) {
			fail(w, 400, "invalid_redirect_uri")
			return
		}
	}
	if len(s.state.Clients) >= 100 {
		fail(w, 503, "temporarily_unavailable")
		return
	}
	id := random()
	s.state.Clients[id] = client{v.Redirects, v.Name}
	if s.save() != nil {
		delete(s.state.Clients, id)
		fail(w, 503, "temporarily_unavailable")
		return
	}
	jsonReply(w, 201, map[string]any{"client_id": id, "client_name": v.Name, "redirect_uris": v.Redirects, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "scope": "mcp"})
}

var challengeRE = regexp.MustCompile("^[A-Za-z0-9_-]{43}$")
var verifierRE = regexp.MustCompile("^[A-Za-z0-9._~-]{43,128}$")

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		q, e := url.ParseQuery(r.URL.RawQuery)
		c, ok := s.state.Clients[q.Get("client_id")]
		if e != nil || len(r.URL.RawQuery) > 8192 || !single(q) || !ok || !contains(c.Redirects, q.Get("redirect_uri")) || !contains(s.cfg.Redirects, q.Get("redirect_uri")) || len(q.Get("state")) > 1024 {
			consentFail(w, 400, "invalid_request")
			return
		}
		errorCode := ""
		switch {
		case q.Get("response_type") != "code":
			errorCode = "unsupported_response_type"
		case q.Get("resource") != s.resource():
			errorCode = "invalid_target"
		case q.Get("scope") != "" && q.Get("scope") != "mcp":
			errorCode = "invalid_scope"
		case q.Get("code_challenge_method") != "S256" || !challengeRE.MatchString(q.Get("code_challenge")):
			errorCode = "invalid_request"
		}
		if errorCode != "" {
			s.authorizationError(w, r, q, errorCode)
			return
		}
		if len(s.pending) >= 100 {
			consentFail(w, 503, "temporarily_unavailable")
			return
		}
		id, cookie := random(), random()
		s.pending[digest(id)] = pending{q, digest(cookie), s.now().Add(5 * time.Minute)}
		http.SetCookie(w, &http.Cookie{Name: "__Host-mcp-consent", Value: cookie, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 300})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// no-referrer makes native form submissions send Origin: null in Chrome.
		// Preserve the same-origin Origin required by the strict POST check below.
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", consentPolicy(s.cfg.Redirects))
		_ = consent.Execute(w, struct{ Name, Redirect, ID, ClientID string }{c.Name, q.Get("redirect_uri"), id, q.Get("client_id")})
		return
	}
	if r.Method != "POST" || r.URL.RawQuery != "" || r.ParseForm() != nil || !single(r.PostForm) {
		s.rejectConsent(w, r, "malformed_request", 403, "access_denied")
		return
	}
	if len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != s.cfg.Issuer {
		s.rejectConsent(w, r, "invalid_origin", 403, "access_denied")
		return
	}
	if r.PostForm.Get("request") == "" {
		s.rejectConsent(w, r, "consent_request_missing", 403, "access_denied")
		return
	}
	id := digest(r.PostForm.Get("request"))
	p, ok := s.pending[id]
	if !ok {
		s.rejectConsent(w, r, "consent_request_unknown", 403, "access_denied")
		return
	}
	if !s.now().Before(p.Until) {
		delete(s.pending, id)
		s.rejectConsent(w, r, "consent_request_expired", 403, "access_denied")
		return
	}
	cookie, e := r.Cookie("__Host-mcp-consent")
	if e != nil {
		s.rejectConsent(w, r, "consent_cookie_missing", 403, "access_denied")
		return
	}
	if subtle.ConstantTimeCompare([]byte(p.Cookie), []byte(digest(cookie.Value))) != 1 {
		s.rejectConsent(w, r, "consent_cookie_mismatch", 403, "access_denied")
		return
	}
	delete(s.pending, id)
	decision := r.PostForm.Get("decision")
	q := p.Query
	dest, _ := url.Parse(q.Get("redirect_uri"))
	values := dest.Query()
	for _, k := range []string{"code", "error", "error_description", "state", "iss"} {
		values.Del(k)
	}
	if q.Has("state") {
		values.Set("state", q.Get("state"))
	}
	values.Set("iss", s.cfg.Issuer)
	if decision == "deny" {
		values.Set("error", "access_denied")
	} else {
		if !s.passwordLimit.Allow() {
			w.Header().Set("Retry-After", "3")
			s.rejectConsent(w, r, "password_rate_limited", 429, "temporarily_unavailable")
			return
		}
		if decision != "allow" {
			s.rejectConsent(w, r, "invalid_decision", 403, "access_denied")
			return
		}
		password := r.PostForm.Get("password")
		// bcrypt truncates long inputs when comparing; reject them before hashing.
		if len(password) > 72 {
			s.rejectConsent(w, r, "password_too_long", 403, "access_denied")
			return
		}
		// The one-time request was consumed under lock; bcrypt must not block MCP.
		s.mu.Unlock()
		err := bcrypt.CompareHashAndPassword([]byte(s.cfg.AdminHash), []byte(password))
		s.mu.Lock()
		if s.broken {
			s.rejectConsent(w, r, "state_persistence_failed", 503, "temporarily_unavailable")
			return
		}
		if err != nil {
			s.rejectConsent(w, r, "password_verification_failed", 403, "access_denied")
			return
		}
		if len(s.state.Codes) >= 100 {
			s.rejectConsent(w, r, "authorization_capacity_exceeded", 503, "temporarily_unavailable")
			return
		}
		raw := random()
		hash := digest(raw)
		s.state.Codes[hash] = code{q.Get("client_id"), q.Get("redirect_uri"), q.Get("code_challenge"), q.Get("resource"), s.now().Add(2 * time.Minute)}
		if s.save() != nil {
			delete(s.state.Codes, hash)
			s.rejectConsent(w, r, "state_persistence_failed", 503, "temporarily_unavailable")
			return
		}
		values.Set("code", raw)
	}
	dest.RawQuery = values.Encode()
	http.Redirect(w, r, dest.String(), 303)
}
func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	q := r.PostForm
	cid := q.Get("client_id")
	if _, ok := s.state.Clients[cid]; !ok || r.Header.Get("Authorization") != "" || q.Get("client_secret") != "" || q.Get("client_assertion") != "" || q.Get("client_assertion_type") != "" {
		fail(w, 401, "invalid_client")
		return
	}
	if q.Get("scope") != "" && q.Get("scope") != "mcp" {
		fail(w, 400, "invalid_scope")
		return
	}
	if q.Get("resource") != s.resource() {
		fail(w, 400, "invalid_target")
		return
	}
	var g grant
	id := ""
	switch q.Get("grant_type") {
	case "authorization_code":
		h := digest(q.Get("code"))
		c, ok := s.state.Codes[h]
		if !ok {
			for k, g := range s.state.Grants {
				if g.Client == cid && g.Code == h {
					delete(s.state.Grants, k)
					if s.save() != nil {
						fail(w, 503, "temporarily_unavailable")
						return
					}
				}
			}
			fail(w, 400, "invalid_grant")
			return
		}
		if c.Client != cid || c.Redirect != q.Get("redirect_uri") || !s.now().Before(c.Until) || !verifierRE.MatchString(q.Get("code_verifier")) || oauth2.S256ChallengeFromVerifier(q.Get("code_verifier")) != c.Challenge {
			fail(w, 400, "invalid_grant")
			return
		}
		delete(s.state.Codes, h)
		if len(s.state.Grants) >= 1000 {
			fail(w, 503, "temporarily_unavailable")
			return
		}
		id = random()
		g = grant{Client: cid, Resource: c.Resource, Code: h, Until: s.now().Add(30 * 24 * time.Hour)}
	case "refresh_token":
		h := digest(q.Get("refresh_token"))
		for k, v := range s.state.Grants {
			if v.Client != cid {
				continue
			}
			if contains(v.Used, h) {
				delete(s.state.Grants, k)
				if s.save() != nil {
					fail(w, 503, "temporarily_unavailable")
					return
				}
				fail(w, 400, "invalid_grant")
				return
			}
			if v.Refresh == h {
				id = k
				g = v
			}
		}
		if id == "" || g.Resource != s.resource() || !s.now().Before(g.Until) || len(g.Used) >= 1000 {
			fail(w, 400, "invalid_grant")
			return
		}
		g.Used = append(g.Used, g.Refresh)
	default:
		fail(w, 400, "unsupported_grant_type")
		return
	}
	access, refresh := random(), random()
	g.Access = digest(access)
	g.Refresh = digest(refresh)
	g.AccessUntil = s.now().Add(15 * time.Minute)
	if g.Until.Before(g.AccessUntil) {
		g.AccessUntil = g.Until
	}
	s.state.Grants[id] = g
	if s.save() != nil {
		delete(s.state.Grants, id)
		fail(w, 503, "temporarily_unavailable")
		return
	}
	jsonReply(w, 200, map[string]any{"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": int(g.AccessUntil.Sub(s.now()).Seconds()), "scope": "mcp"})
}
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.state.Clients[r.PostForm.Get("client_id")]; !ok || r.Header.Get("Authorization") != "" || r.PostForm.Get("client_secret") != "" || r.PostForm.Get("client_assertion") != "" {
		fail(w, 400, "invalid_client")
		return
	}
	h := digest(r.PostForm.Get("token"))
	for id, g := range s.state.Grants {
		if g.Client == r.PostForm.Get("client_id") && (g.Access == h || g.Refresh == h || contains(g.Used, h)) {
			delete(s.state.Grants, id)
		}
	}
	if s.save() != nil {
		fail(w, 503, "temporarily_unavailable")
		return
	}
	w.WriteHeader(200)
}

// Only call after exact client and redirect validation.
func (s *Server) authorizationError(w http.ResponseWriter, r *http.Request, q url.Values, code string) {
	dest, _ := url.Parse(q.Get("redirect_uri"))
	values := dest.Query()
	for _, k := range []string{"code", "error", "error_description", "state", "iss"} {
		values.Del(k)
	}
	values.Set("error", code)
	values.Set("iss", s.cfg.Issuer)
	if q.Has("state") {
		values.Set("state", q.Get("state"))
	}
	dest.RawQuery = values.Encode()
	http.Redirect(w, r, dest.String(), 303)
}

// RequestAllowed applies canonical Host/Origin checks to native OAuth MCP calls.
// Static Bearer callers retain their existing transport allowlist behavior.
func (s *Server) RequestAllowed(r *http.Request) bool {
	u, _ := url.Parse(s.cfg.Issuer)
	if r.Host != u.Host {
		return false
	}
	origins, sent := r.Header["Origin"]
	return !sent || (len(origins) == 1 && origins[0] == s.cfg.Issuer)
}
