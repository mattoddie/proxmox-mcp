package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Restrict outbound discovery to OpenAI's documented client document paths.
// No user-selected host, redirect, query, port or arbitrary metadata URL is fetched.
var chatGPTDocument = regexp.MustCompile(`^https://chatgpt[.]com/oauth/(?:[A-Za-z0-9_-]{1,128}/)?client[.]json$`)

type metadata struct {
	ID        string   `json:"client_id"`
	Redirects []string `json:"redirect_uris"`
	Name      string   `json:"client_name"`
	Auth      string   `json:"token_endpoint_auth_method"`
	Methods   []string `json:"token_endpoint_auth_methods_supported"`
	Grants    []string `json:"grant_types"`
	Responses []string `json:"response_types"`
	Scope     string   `json:"scope"`
}

func (v metadata) validPublic() bool {
	if len(v.Redirects) == 0 || len(v.Redirects) > 10 || len(v.Name) > 128 || (v.Scope != "" && v.Scope != "mcp") {
		return false
	}
	// CIMD's plural methods are capabilities, not a preference. Honor none when
	// supported even if the transitional singular preference is private_key_jwt.
	if v.Methods != nil {
		if !contains(v.Methods, "none") {
			return false
		}
	} else if v.Auth != "none" {
		return false
	}
	for _, g := range v.Grants {
		if g != "authorization_code" && g != "refresh_token" {
			return false
		}
	}
	if len(v.Grants) > 0 && !contains(v.Grants, "authorization_code") {
		return false
	}
	if len(v.Responses) > 0 && (len(v.Responses) != 1 || v.Responses[0] != "code") {
		return false
	}
	for _, raw := range v.Redirects {
		u, e := url.Parse(raw)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.Contains(raw, "#") {
			return false
		}
	}
	return true
}

// decodeMetadata ignores extension fields but rejects duplicate keys, trailing
// objects and oversized responses, preventing divergent JSON interpretations.
func decodeMetadata(r io.Reader, v *metadata) error {
	b, e := io.ReadAll(io.LimitReader(r, 16385))
	if e != nil || len(b) > 16384 {
		return errors.New("oversized metadata")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	t, e := d.Token()
	if e != nil || t != json.Delim('{') {
		return errors.New("invalid metadata")
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		t, e = d.Token()
		if e != nil {
			return e
		}
		key, ok := t.(string)
		if !ok {
			return errors.New("invalid key")
		}
		if _, ok = fields[key]; ok {
			return errors.New("duplicate key")
		}
		var value json.RawMessage
		if e = d.Decode(&value); e != nil {
			return e
		}
		fields[key] = value
	}
	if _, e = d.Token(); e != nil {
		return e
	}
	if _, e = d.Token(); e != io.EOF {
		return errors.New("trailing metadata")
	}
	return json.Unmarshal(b, v)
}
func (s *Server) fetchMetadata(ctx context.Context, id string) (client, error) {
	if !chatGPTDocument.MatchString(id) {
		return client{}, errors.New("untrusted client document")
	}
	req, e := http.NewRequestWithContext(ctx, "GET", id, nil)
	if e != nil {
		return client{}, e
	}
	req.Header.Set("Accept", "application/json")
	response, e := s.metadataHTTP.Do(req)
	if e != nil {
		return client{}, e
	}
	defer response.Body.Close()
	media, _, e := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if e != nil || media != "application/json" || response.StatusCode != 200 {
		return client{}, errors.New("invalid metadata response")
	}
	var v metadata
	if e = decodeMetadata(response.Body, &v); e != nil || v.ID != id || v.Name == "" || !v.validPublic() {
		return client{}, errors.New("invalid client metadata")
	}
	return client{Redirects: v.Redirects, Name: v.Name}, nil
}
