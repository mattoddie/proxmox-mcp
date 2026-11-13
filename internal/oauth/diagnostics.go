package oauth

import (
	"encoding/json"
	"net/http"
)

// postRejection emits only server-selected fields. Never pass request values or
// underlying errors here: even parse and filesystem errors can contain secrets.
// Keep diagnostics server-side; public OAuth responses remain generic.
func (s *Server) postRejection(r *http.Request, reason string, status int) {
	if r.Method != "POST" || r.URL.Path != "/oauth/authorize" {
		return
	}
	event := struct {
		Event  string `json:"event"`
		Reason string `json:"reason"`
		Status int    `json:"status"`
	}{"oauth_authorization_rejected", reason, status}
	b, _ := json.Marshal(event)
	s.diagnostics.Print(string(b))
}

func (s *Server) rejectConsent(w http.ResponseWriter, r *http.Request, reason string, status int, code string) {
	s.postRejection(r, reason, status)
	consentFail(w, status, code)
}
