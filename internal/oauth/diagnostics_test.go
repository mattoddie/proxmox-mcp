package oauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"
	"golang.org/x/time/rate"
)

type failedConsentBody struct{}

func (failedConsentBody) Read([]byte) (int, error) {
	return 0, errors.New("sensitive-body-read-error")
}
func (failedConsentBody) Close() error { return nil }

func TestAuthorizationRejectionDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		status       int
	}{
		{"host", "invalid_host", 403},
		{"origin_missing", "invalid_origin", 403},
		{"origin_null", "invalid_origin", 403},
		{"origin_foreign", "invalid_origin", 403},
		{"origin_duplicate", "invalid_origin", 403},
		{"query", "malformed_request", 403},
		{"content_type", "malformed_request", 400},
		{"content_type_malformed", "malformed_request", 400},
		{"form_duplicate", "malformed_request", 400},
		{"form_malformed", "malformed_request", 400},
		{"body_oversized", "malformed_request", 400},
		{"body_read_error", "malformed_request", 400},
		{"request_missing", "consent_request_missing", 403},
		{"request_unknown", "consent_request_unknown", 403},
		{"request_expired", "consent_request_expired", 403},
		{"cookie_missing", "consent_cookie_missing", 403},
		{"cookie_empty", "consent_cookie_mismatch", 403},
		{"cookie_mismatch", "consent_cookie_mismatch", 403},
		{"decision_missing", "invalid_decision", 403},
		{"decision_invalid", "invalid_decision", 403},
		{"password_wrong", "password_verification_failed", 403},
		{"password_missing", "password_verification_failed", 403},
		{"password_long", "password_too_long", 403},
		{"password_multibyte", "password_too_long", 403},
		{"rate_global", "rate_limited", 429},
		{"rate_password", "password_rate_limited", 429},
		{"state_save", "state_persistence_failed", 503},
		{"state_broken", "state_persistence_failed", 503},
		{"capacity", "authorization_capacity_exceeded", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setup(t)
			var logs bytes.Buffer
			s.diagnostics = log.New(&logs, "", 0)
			clientID := register(t, s)
			q := authQuery(s, clientID, oauth2.GenerateVerifier())
			q.Set("state", "sensitive-oauth-state")
			get := request(s, "GET", "/oauth/authorize", q, nil)
			if get.Code != 200 {
				t.Fatal(get.Code)
			}
			id := requestID(get.Body.String())
			cookie := get.Result().Cookies()[0]
			originalCookie := cookie.Value
			form := url.Values{"request": {id}, "decision": {"allow"}, "password": {"test-password"}}
			path, contentType := "/oauth/authorize", "application/x-www-form-urlencoded"
			origin := []string{s.cfg.Issuer}
			var body io.Reader
			switch tc.name {
			case "origin_missing":
				origin = nil
			case "origin_null":
				origin = []string{"null"}
			case "origin_foreign":
				origin = []string{"https://sensitive-origin.example"}
			case "origin_duplicate":
				origin = append(origin, s.cfg.Issuer)
			case "query":
				path += "?password=sensitive-query&code=sensitive-code&token=sensitive-token"
			case "content_type":
				contentType = "text/plain"
			case "content_type_malformed":
				contentType += ";;sensitive-content-type"
			case "form_duplicate":
				form.Add("password", "sensitive-duplicate-password")
			case "form_malformed":
				body = strings.NewReader(form.Encode() + "&sensitive-form=%zz")
			case "body_oversized":
				body = strings.NewReader("sensitive-body=" + strings.Repeat("x", 16385))
			case "body_read_error":
				body = failedConsentBody{}
			case "request_missing":
				form.Del("request")
			case "request_unknown":
				form.Set("request", "sensitive-unknown-request")
			case "request_expired":
				until := s.pending[digest(id)].Until
				s.now = func() time.Time { return until } // Exact expiry boundary.
			case "cookie_missing":
				cookie = nil
			case "cookie_empty":
				cookie.Value = ""
			case "cookie_mismatch":
				cookie.Value = "sensitive-other-cookie"
			case "decision_missing":
				form.Del("decision")
			case "decision_invalid":
				form.Set("decision", "sensitive-decision")
			case "password_wrong":
				form.Set("password", "sensitive-wrong-password")
			case "password_missing":
				form.Del("password")
			case "password_long":
				form.Set("password", strings.Repeat("x", 73))
			case "password_multibyte":
				form.Set("password", strings.Repeat("é", 37))
			case "rate_global":
				s.limit = rate.NewLimiter(rate.Every(time.Hour), 0)
			case "rate_password":
				s.passwordLimit = rate.NewLimiter(rate.Every(time.Hour), 0)
			case "state_save":
				s.cfg.Directory = filepath.Join(s.cfg.Directory, "sensitive-missing-directory")
			case "state_broken":
				s.broken = true
			case "capacity":
				for i := 0; i < 100; i++ {
					s.state.Codes[fmt.Sprint(i)] = code{Until: s.now().Add(time.Minute)}
				}
			}
			if body == nil {
				body = strings.NewReader(form.Encode())
			}
			r := httptest.NewRequest("POST", s.cfg.Issuer+path, body)
			r.Header.Set("Content-Type", contentType)
			r.Header["Origin"] = origin
			r.Header.Set("Authorization", "Bearer sensitive-header-token")
			if cookie != nil {
				r.AddCookie(cookie)
			}
			if tc.name == "host" {
				r.Host = "sensitive-host.example"
			}
			codesBefore := len(s.state.Codes)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != tc.status || w.Header().Get("Location") != "" || len(s.state.Codes) != codesBefore {
				t.Fatalf("status=%d; location present=%t; codes=%d", w.Code, w.Header().Get("Location") != "", len(s.state.Codes))
			}
			// Exactly these three fields may be logged, with no request-derived values.
			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatal("expected exactly one JSON diagnostic", err)
			}
			if len(event) != 3 || event["event"] != "oauth_authorization_rejected" || event["reason"] != tc.reason || event["status"] != float64(tc.status) {
				t.Fatal("unexpected diagnostic schema or reason")
			}
			for _, secret := range []string{"sensitive-", id, originalCookie, clientID, s.cfg.AdminHash, "test-password", form.Encode()} {
				if secret != "" && strings.Contains(logs.String(), secret) {
					t.Fatal("diagnostics disclosed request or configuration data")
				}
			}
			// Preserve the exact public error format and generic descriptions.
			want := httptest.NewRecorder()
			publicCode := "access_denied"
			if tc.status == 400 || tc.name == "host" {
				publicCode = "invalid_request"
			} else if tc.status == 429 || tc.status == 503 {
				publicCode = "temporarily_unavailable"
			}
			if tc.status == 400 || tc.name == "host" || tc.name == "rate_global" || tc.name == "state_broken" {
				fail(want, tc.status, publicCode)
			} else {
				consentFail(want, tc.status, publicCode)
			}
			if w.Body.String() != want.Body.String() {
				t.Fatal("public error changed or exposed diagnostic")
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("security headers lost")
			}
			if tc.status == 429 && w.Header().Get("Retry-After") == "" {
				t.Fatal("rate limit retry header lost")
			}
			if tc.name == "state_save" && !s.broken {
				t.Fatal("storage failure did not fail closed")
			}
			if tc.name == "request_expired" {
				if _, ok := s.pending[digest(id)]; ok {
					t.Fatal("expired consent was retained")
				}
			}
			if tc.reason == "invalid_decision" || strings.HasPrefix(tc.reason, "password_") || tc.name == "state_save" {
				if _, ok := s.pending[digest(id)]; ok {
					t.Fatal("one-time consent was not consumed")
				}
			}
		})
	}
}

func TestAuthorizationSuccessDenialAndReplayDiagnostics(t *testing.T) {
	for _, decision := range []string{"allow", "deny"} {
		t.Run(decision, func(t *testing.T) {
			s := setup(t)
			var logs bytes.Buffer
			s.diagnostics = log.New(&logs, "", 0)
			clientID := register(t, s)
			w := request(s, "GET", "/oauth/authorize", authQuery(s, clientID, oauth2.GenerateVerifier()), nil)
			form := url.Values{"request": {requestID(w.Body.String())}, "decision": {decision}}
			if decision == "allow" {
				// Spaces are part of a password; never trim submitted passwords.
				password := " " + strings.Repeat("x", 70) + " "
				hash, err := bcrypt.GenerateFromPassword([]byte(password), 10)
				if err != nil {
					t.Fatal(err)
				}
				s.cfg.AdminHash = string(hash)
				form.Set("password", password)
			}
			cookie := w.Result().Cookies()[0]
			w = request(s, "POST", "/oauth/authorize", form, cookie)
			if w.Code != 303 || logs.Len() != 0 {
				t.Fatal("valid consent or denial rejected/logged")
			}
			u, err := url.Parse(w.Header().Get("Location"))
			if err != nil || u.Query().Get("state") != "original-state" || u.Query().Get("iss") != s.cfg.Issuer {
				t.Fatal("OAuth redirect binding changed")
			}
			if decision == "allow" && u.Query().Get("code") == "" || decision == "deny" && (u.Query().Get("error") != "access_denied" || len(s.state.Codes) != 0) {
				t.Fatal("unexpected authorization result")
			}
			codes := len(s.state.Codes)
			w = request(s, "POST", "/oauth/authorize", form, cookie)
			if w.Code != 403 || !strings.Contains(logs.String(), `"reason":"consent_request_unknown"`) || len(s.state.Codes) != codes {
				t.Fatal("replayed consent accepted or incorrectly diagnosed")
			}
		})
	}
}

func TestDiagnosticsIgnoreOtherEndpoints(t *testing.T) {
	s := setup(t)
	var logs bytes.Buffer
	s.diagnostics = log.New(&logs, "", 0)
	for _, path := range []string{"/noop", "/oauth/token", "/oauth/revoke", "/oauth/register"} {
		request(s, "POST", path, nil, nil)
	}
	if logs.Len() != 0 {
		t.Fatal("authorization diagnostics included other endpoints")
	}
}
