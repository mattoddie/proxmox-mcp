package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func TestConsentHeadersAndSubmission(t *testing.T) {
	s := setup(t)
	id := register(t, s)
	verifier := oauth2.GenerateVerifier()
	w := request(s, "GET", "/oauth/authorize", authQuery(s, id, verifier), nil)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	for key, want := range map[string]string{"Referrer-Policy": "same-origin", "Cache-Control": "no-store", "Pragma": "no-cache", "X-Content-Type-Options": "nosniff", "Content-Type": "text/html; charset=utf-8"} {
		if got := w.Header().Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	csp := w.Header().Get("Content-Security-Policy")
	callback, _ := url.Parse(s.cfg.Redirects[0])
	if !strings.Contains(csp, "form-action 'self' "+callback.Scheme+"://"+callback.Host+";") {
		t.Fatal("configured callback origin blocked")
	}
	for _, directive := range []string{"default-src 'none'", "form-action 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("missing CSP directive %q", directive)
		}
	}
	if strings.Contains(csp, "unsafe-") {
		t.Fatal("CSP must not allow arbitrary inline content")
	}
	// Hash the rendered bytes, so whitespace or template transformations cannot
	// silently break the styles or the show/hide control in a browser.
	for tag, directive := range map[string]string{"style": "style-src", "script": "script-src"} {
		blocks := regexp.MustCompile("(?s)<"+tag+">(.*?)</"+tag+">").FindAllStringSubmatch(w.Body.String(), -1)
		if len(blocks) != 1 {
			t.Fatalf("expected one %s block", tag)
		}
		hash := sha256.Sum256([]byte(blocks[0][1]))
		if !strings.Contains(csp, directive+" 'sha256-"+base64.StdEncoding.EncodeToString(hash[:])+"'") {
			t.Errorf("rendered %s is blocked by CSP", tag)
		}
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("expected consent cookie")
	}
	cookie := cookies[0]
	if cookie.Name != "__Host-mcp-consent" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" || cookie.MaxAge != 300 {
		t.Fatal("consent cookie protections changed")
	}
	match := regexp.MustCompile(`name="request" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if len(match) != 2 {
		t.Fatal("missing one-time request")
	}
	form := url.Values{"request": {match[1]}, "password": {"test-password"}, "decision": {"allow"}}
	// Even with a valid cookie and request, opaque and foreign origins fail.
	for _, origins := range [][]string{nil, {"null"}, {"https://evil.example"}, {s.cfg.Issuer, s.cfg.Issuer}} {
		r := httptest.NewRequest("POST", s.cfg.Issuer+"/oauth/authorize", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origins != nil {
			r.Header["Origin"] = origins
		}
		r.AddCookie(cookie)
		rejected := httptest.NewRecorder()
		s.ServeHTTP(rejected, r)
		if rejected.Code != 403 || len(s.state.Codes) != 0 {
			t.Fatal("invalid Origin authorized")
		}
		if !strings.Contains(rejected.Body.String(), "Start the connection again") {
			t.Fatal("missing understandable recovery message")
		}
	}
	w = request(s, "POST", "/oauth/authorize", form, cookie)
	dest, err := url.Parse(w.Header().Get("Location"))
	if err != nil || w.Code != 303 {
		t.Fatal("same-origin submission failed", w.Code)
	}
	if got := w.Header().Get("Content-Security-Policy"); got != "default-src 'none'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'" {
		t.Fatalf("POST response policy changed: %q", got)
	}
	q := dest.Query()
	if q.Get("code") == "" || q.Get("state") != "original-state" || q.Get("iss") != s.cfg.Issuer {
		t.Fatal("missing authorization response fields")
	}
	if exchange(s, id, q.Get("code"), verifier).Code != 200 {
		t.Fatal("PKCE exchange failed")
	}
	if request(s, "POST", "/oauth/authorize", form, cookie).Code != 403 {
		t.Fatal("consent request reused")
	}
	metadata := request(s, "GET", "/.well-known/oauth-authorization-server", nil, nil)
	if metadata.Header().Get("Referrer-Policy") != "no-referrer" || strings.Contains(metadata.Header().Get("Content-Security-Policy"), "script-src") {
		t.Fatal("consent policy leaked to other endpoints")
	}
}

func TestConsentEscapesClientDetails(t *testing.T) {
	s := setup(t)
	id := register(t, s)
	name := `<img src=x onerror="alert(1)">`
	s.state.Clients[id] = client{Redirects: s.cfg.Redirects, Name: name}
	w := request(s, "GET", "/oauth/authorize", authQuery(s, id, oauth2.GenerateVerifier()), nil)
	body := w.Body.String()
	if w.Code != 200 || strings.Contains(body, name) || !strings.Contains(body, html.EscapeString(name)) {
		t.Fatal("client name not escaped")
	}
	for _, detail := range []string{id, s.cfg.Redirects[0], "Requested scope", "Unverified", "deleting resources", "formnovalidate", `lang="en"`, `for="password"`} {
		if !strings.Contains(strings.ToLower(body), strings.ToLower(detail)) {
			t.Errorf("missing consent detail %q", detail)
		}
	}
}

func TestConsentPolicyConfiguredOrigins(t *testing.T) {
	redirects := []string{
		"https://chatgpt.com/connector_platform_oauth_redirect",
		"https://chatgpt.com/another/path?next=https://evil.example",
		"https://client.example:8443/callback",
	}
	policy := consentPolicy(redirects)
	wantForm := "form-action 'self' https://chatgpt.com https://client.example:8443"
	parts := strings.Split(policy, "; ")
	if len(parts) != 6 || parts[1] != wantForm {
		t.Fatalf("unexpected policy: %q", policy)
	}
	// Only form-action may differ from the original restrictive policy.
	baseline := strings.Split(consentPolicy(nil), "; ")
	for i := range parts {
		if i != 1 && parts[i] != baseline[i] {
			t.Fatalf("directive changed: %q", parts[i])
		}
	}
	for _, forbidden := range []string{"evil.example", "unsafe-", "*", "/callback", "connector_platform"} {
		if strings.Contains(policy, forbidden) {
			t.Fatalf("unexpected CSP source %q: %s", forbidden, policy)
		}
	}
	// Defense in depth: malformed configuration cannot inject CSP directives.
	if got := consentPolicy([]string{"https://*.example/callback", "https://bad;host/callback", "http://client.example/callback"}); got != consentPolicy(nil) {
		t.Fatalf("unsafe source accepted: %q", got)
	}
}

func TestConsentPolicyRejectsRequestOrigins(t *testing.T) {
	s := setup(t)
	id := register(t, s)
	q := authQuery(s, id, oauth2.GenerateVerifier())
	q.Set("redirect_uri", "https://unapproved.example/callback")
	w := request(s, "GET", "/oauth/authorize", q, nil)
	if w.Code != http.StatusBadRequest || strings.Contains(w.Header().Get("Content-Security-Policy"), "unapproved.example") {
		t.Fatal("unapproved request origin entered CSP")
	}
}
