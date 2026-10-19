// Package config reads proxmox-mcp's settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/mattoddie/proxmox-mcp/internal/js"
)

// Toolsets are the groups of tools that can be switched on and off with PROXMOX_TOOLSETS, in
// registration order.
var Toolsets = []string{
	"cluster", "nodes", "vms", "containers", "agent", "snapshots", "storage", "backup", "tasks", "access",
	"firewall", "ha", "sdn", "ceph", "pools", "replication", "notifications", "hardware", "raw",
}

// OptInToolsets are left out of "all" (and so of the default) and must be listed by name. The
// raw API tool reaches every endpoint, including ones the curated tools gate more finely, so it
// is never enabled without being asked for.
var OptInToolsets = map[string]bool{"raw": true}

// Proxmox holds the API connection settings.
type Proxmox struct {
	URL string
	// TokenID is the full API token id, e.g. root@pam!mcp.
	TokenID     string
	TokenSecret string
	// Username is user@realm for ticket (password) authentication.
	Username  string
	Password  string
	VerifySSL bool
	// CACert is a PEM bundle trusted in addition to the system CAs.
	CACert string
	// TLSFingerprints are pinned SHA-256 fingerprints of the server certificate, as "AA:BB:...".
	// When set, the certificate must match one of them, whether or not chain verification is on.
	TLSFingerprints []string
	TimeoutMs       int
	// TaskTimeoutMs is how long tools wait for a Proxmox task (UPID) to finish before returning.
	TaskTimeoutMs int
	// DefaultNode is used when a tool needs a node and none is given.
	DefaultNode string
}

// Tools controls which tools are registered.
type Tools struct {
	AllowWrites bool
	// AllowDeletes enables tools that delete guests or configuration; requires AllowWrites.
	AllowDeletes bool
	// AllowExec enables tools that run commands or write files in guests and on nodes; requires AllowWrites.
	AllowExec bool
	// AllowUnredacted lets the raw API tool return responses without secret redaction
	// (its `redact: false` argument); PROXMOX_ALLOW_UNREDACTED.
	AllowUnredacted bool
	Toolsets        map[string]bool
}

// AllTools enables every tool, for documentation and tests.
func AllTools() Tools {
	t := Tools{AllowWrites: true, AllowDeletes: true, AllowExec: true, AllowUnredacted: true, Toolsets: map[string]bool{}}
	for _, ts := range Toolsets {
		t.Toolsets[ts] = true
	}
	return t
}

// HTTP holds the Streamable HTTP transport settings.
type HTTP struct {
	Host      string
	Port      int
	AuthToken string
	// AllowUnauthenticated serves /mcp without a bearer token (MCP_ALLOW_UNAUTHENTICATED); only
	// behind something else that authenticates.
	AllowUnauthenticated bool
	// AllowedHosts is the lower-cased Host (and Origin) allow-list.
	AllowedHosts []string
}

// Config is the whole configuration.
type Config struct {
	Proxmox   Proxmox
	Tools     Tools
	Transport string // "stdio" or "http"
	HTTP      HTTP
}

// ErrAuthTokenRequired is returned for the http transport without MCP_AUTH_TOKEN.
var ErrAuthTokenRequired = errors.New("MCP_AUTH_TOKEN is required for the http transport (generate one with: openssl rand -hex 32); set MCP_ALLOW_UNAUTHENTICATED=true only behind something else that authenticates")

// Env looks up a variable; os.LookupEnv in production, a map in tests.
type Env func(string) (string, bool)

// MapEnv adapts a map for tests.
func MapEnv(m map[string]string) Env {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// read returns a trimmed variable ("" when unset or blank), also honouring the <NAME>_FILE
// convention so secrets can be supplied as Docker secrets mounted under /run/secrets.
func read(env Env, name string) (string, error) {
	if file, _ := env(name + "_FILE"); file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("Could not read %s_FILE (%s): %s", name, file, errText(err))
		}
		return strings.TrimSpace(string(b)), nil
	}
	v, _ := env(name)
	return strings.TrimSpace(v), nil
}

// errText is an OS error's message without Go's "open <path>: " prefix, which the caller already shows.
func errText(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

var (
	fingerprintPrefix = regexp.MustCompile(`(?i)^(sha-?256\s*(fingerprint)?\s*[=:]?\s*)`)
	fingerprintSep    = regexp.MustCompile(`[:\s]`)
	hex64             = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	tokenIDRe         = regexp.MustCompile(`^[^@\s]+@[^!\s]+![^\s=]+$`)
)

// ParseFingerprints parses PROXMOX_TLS_FINGERPRINT: one or more comma-separated SHA-256
// fingerprints, as Proxmox shows them ("AA:BB:...", optionally prefixed "sha256:" or
// "SHA256 Fingerprint=") or as 64 hex digits, into "AA:BB:..." form.
func ParseFingerprints(value string) ([]string, error) {
	var out []string
	for _, v := range strings.Split(value, ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		hex := fingerprintSep.ReplaceAllString(fingerprintPrefix.ReplaceAllString(v, ""), "")
		if !hex64.MatchString(hex) {
			return nil, fmt.Errorf(`PROXMOX_TLS_FINGERPRINT must be a SHA-256 fingerprint (AA:BB:... with 32 pairs, or 64 hex digits), got "%s"`, v)
		}
		hex = strings.ToUpper(hex)
		pairs := make([]string, 0, 32)
		for i := 0; i < len(hex); i += 2 {
			pairs = append(pairs, hex[i:i+2])
		}
		out = append(out, strings.Join(pairs, ":"))
	}
	return out, nil
}

func boolValue(v string, fallback bool) bool {
	if v == "" {
		return fallback
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// positiveInt parses like Number.parseInt(value, 10) (leading digits; "30s" is 30).
func positiveInt(name, v string, fallback int) (int, error) {
	if v == "" {
		return fallback, nil
	}
	n := js.ParseInt(v)
	if !js.IsFinite(n) || n <= 0 {
		return 0, fmt.Errorf(`%s must be a positive integer, got "%s"`, name, v)
	}
	return int(n), nil
}

// parseToken accepts the token as PROXMOX_TOKEN_ID + PROXMOX_TOKEN_SECRET, or as one
// PROXMOX_API_TOKEN in the form Proxmox shows it: user@realm!tokenid=secret.
func parseToken(env Env) (id, secret string, err error) {
	combined, err := read(env, "PROXMOX_API_TOKEN")
	if err != nil {
		return "", "", err
	}
	combined = strings.TrimPrefix(combined, "PVEAPIToken=")
	if id, err = read(env, "PROXMOX_TOKEN_ID"); err != nil {
		return "", "", err
	}
	if secret, err = read(env, "PROXMOX_TOKEN_SECRET"); err != nil {
		return "", "", err
	}
	if combined != "" {
		idx := strings.Index(combined, "=")
		if idx <= 0 {
			return "", "", errors.New("PROXMOX_API_TOKEN must look like user@realm!tokenid=secret")
		}
		id, secret = combined[:idx], combined[idx+1:]
	}
	if id != "" && !tokenIDRe.MatchString(id) {
		return "", "", fmt.Errorf(`The API token id must look like user@realm!tokenid (e.g. root@pam!mcp), got "%s"`, id)
	}
	if (id != "") != (secret != "") {
		return "", "", errors.New("Set both PROXMOX_TOKEN_ID and PROXMOX_TOKEN_SECRET (or PROXMOX_API_TOKEN=user@realm!tokenid=secret)")
	}
	return id, secret, nil
}

var (
	trailingSlashes = regexp.MustCompile(`/+$`)
	apiSuffix       = regexp.MustCompile(`/api2/json$`)
)

// Load reads and validates the configuration.
func Load(env Env) (*Config, error) {
	var firstErr error
	get := func(name string) string {
		v, err := read(env, name)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return v
	}

	rawURL := get("PROXMOX_URL")
	if firstErr != nil {
		return nil, firstErr
	}
	if rawURL == "" {
		return nil, errors.New("PROXMOX_URL is required (e.g. https://pve.example.lan:8006)")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || (parsed.Host == "" && parsed.Opaque == "") {
		return nil, fmt.Errorf("PROXMOX_URL is not a valid URL: %s", rawURL)
	}
	if scheme := strings.ToLower(parsed.Scheme); scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("PROXMOX_URL must use https:// (or http://), got %s", rawURL)
	}
	https := strings.EqualFold(parsed.Scheme, "https")

	tokenID, tokenSecret, err := parseToken(env)
	if err != nil {
		return nil, err
	}
	username := get("PROXMOX_USERNAME")
	password := get("PROXMOX_PASSWORD")
	if username != "" && !strings.Contains(username, "@") {
		realm := get("PROXMOX_REALM")
		if realm == "" {
			realm = "pam"
		}
		username += "@" + realm
	}
	if firstErr != nil {
		return nil, firstErr
	}
	if tokenID == "" && (username == "" || password == "") {
		return nil, errors.New("Set either an API token (PROXMOX_TOKEN_ID and PROXMOX_TOKEN_SECRET) or both PROXMOX_USERNAME and PROXMOX_PASSWORD")
	}

	var caCert string
	if caFile := get("PROXMOX_CA_FILE"); caFile != "" {
		b, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("Could not read PROXMOX_CA_FILE (%s): %s", caFile, errText(err))
		}
		caCert = string(b)
	}

	var fingerprints []string
	if fp := get("PROXMOX_TLS_FINGERPRINT"); fp != "" {
		if fingerprints, err = ParseFingerprints(fp); err != nil {
			return nil, err
		}
		if !https {
			return nil, errors.New("PROXMOX_TLS_FINGERPRINT needs an https:// PROXMOX_URL")
		}
	}

	transport := get("MCP_TRANSPORT")
	if transport == "" {
		transport = "stdio"
	}
	if transport != "stdio" && transport != "http" {
		return nil, fmt.Errorf(`MCP_TRANSPORT must be stdio or http, got "%s"`, transport)
	}

	allowWrites := boolValue(get("PROXMOX_ALLOW_WRITES"), false)
	allowDeletes := boolValue(get("PROXMOX_ALLOW_DELETES"), false)
	allowExec := boolValue(get("PROXMOX_ALLOW_EXEC"), false)
	allowUnredacted := boolValue(get("PROXMOX_ALLOW_UNREDACTED"), false)
	if firstErr != nil {
		return nil, firstErr
	}
	if allowDeletes && !allowWrites {
		return nil, errors.New("PROXMOX_ALLOW_DELETES=true also requires PROXMOX_ALLOW_WRITES=true")
	}
	if allowExec && !allowWrites {
		return nil, errors.New("PROXMOX_ALLOW_EXEC=true also requires PROXMOX_ALLOW_WRITES=true")
	}

	toolsetList := get("PROXMOX_TOOLSETS")
	if toolsetList == "" {
		toolsetList = "all"
	}
	toolsets := map[string]bool{}
	for _, t := range strings.Split(toolsetList, ",") {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if t == "all" {
			for _, x := range Toolsets {
				if !OptInToolsets[x] {
					toolsets[x] = true
				}
			}
			continue
		}
		known := false
		for _, x := range Toolsets {
			if x == t {
				known = true
			}
		}
		if !known {
			return nil, fmt.Errorf(`Unknown toolset "%s" in PROXMOX_TOOLSETS; valid: all, %s`, t, strings.Join(Toolsets, ", "))
		}
		toolsets[t] = true
	}

	var allowedHosts []string
	for _, h := range strings.Split(get("MCP_ALLOWED_HOSTS"), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			allowedHosts = append(allowedHosts, h)
		}
	}
	authToken := get("MCP_AUTH_TOKEN")
	allowUnauthenticated := boolValue(get("MCP_ALLOW_UNAUTHENTICATED"), false)
	if firstErr != nil {
		return nil, firstErr
	}
	if transport == "http" && authToken == "" && !allowUnauthenticated {
		return nil, ErrAuthTokenRequired
	}

	timeoutMs, err := positiveInt("PROXMOX_TIMEOUT_MS", get("PROXMOX_TIMEOUT_MS"), 30000)
	if err != nil {
		return nil, err
	}
	taskTimeoutMs, err := positiveInt("PROXMOX_TASK_TIMEOUT_MS", get("PROXMOX_TASK_TIMEOUT_MS"), 120000)
	if err != nil {
		return nil, err
	}
	host := get("MCP_HTTP_HOST")
	if host == "" {
		// Loopback unless told otherwise; the Docker image sets MCP_HTTP_HOST=0.0.0.0.
		host = "127.0.0.1"
	}
	port, err := positiveInt("MCP_HTTP_PORT", get("MCP_HTTP_PORT"), 8080)
	if err != nil {
		return nil, err
	}
	defaultNode := get("PROXMOX_DEFAULT_NODE")
	verify := caCert != "" || boolValue(get("PROXMOX_VERIFY_SSL"), false)
	if firstErr != nil {
		return nil, firstErr
	}

	cfg := &Config{
		Proxmox: Proxmox{
			URL:             apiSuffix.ReplaceAllString(trailingSlashes.ReplaceAllString(rawURL, ""), ""),
			TokenID:         tokenID,
			TokenSecret:     tokenSecret,
			VerifySSL:       verify,
			CACert:          caCert,
			TLSFingerprints: fingerprints,
			TimeoutMs:       timeoutMs,
			TaskTimeoutMs:   taskTimeoutMs,
			DefaultNode:     defaultNode,
		},
		Tools:     Tools{AllowWrites: allowWrites, AllowDeletes: allowDeletes, AllowExec: allowExec, AllowUnredacted: allowUnredacted, Toolsets: toolsets},
		Transport: transport,
		HTTP: HTTP{
			Host:                 host,
			Port:                 port,
			AuthToken:            authToken,
			AllowUnauthenticated: allowUnauthenticated,
			AllowedHosts:         allowedHosts,
		},
	}
	if tokenID == "" {
		cfg.Proxmox.Username, cfg.Proxmox.Password = username, password
	}
	return cfg, nil
}
