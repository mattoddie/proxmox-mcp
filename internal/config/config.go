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
	// AuditLog is where calls of tools that change things are logged (PROXMOX_AUDIT_LOG): a file
	// path, AuditLogStderr, or "" for no log.
	AuditLog string
}

// AuditLogStderr is the PROXMOX_AUDIT_LOG value that writes the audit log to standard error.
const AuditLogStderr = "stderr"

// AuditLogMode is the permission an audit log file is created with: only the server's user can
// read it, since it records what was changed and with which arguments.
const AuditLogMode = 0o600

// OpenAuditLog opens an audit log file for appending, creating it if needed.
func OpenAuditLog(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, AuditLogMode)
}

// CheckAuditLog checks that PROXMOX_AUDIT_LOG can be written, so a mistake is reported at
// startup rather than on the first change.
func CheckAuditLog(path string) error {
	if path == "" || path == AuditLogStderr {
		return nil
	}
	f, err := OpenAuditLog(path)
	if err != nil {
		return fmt.Errorf("PROXMOX_AUDIT_LOG: cannot write to %s: %s. Its directory must exist and be writable by the server (in Docker, mount a writable directory; the image runs as UID 65532)", path, errText(err))
	}
	return f.Close()
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

// MinAuthTokenLength is the shortest MCP_AUTH_TOKEN accepted; `openssl rand -hex 32` gives 64.
const MinAuthTokenLength = 16

// CheckHTTPAuth validates the http transport's auth settings: a token of at least
// MinAuthTokenLength characters, or MCP_ALLOW_UNAUTHENTICATED=true together with a Host
// allow-list.
func CheckHTTPAuth(h HTTP) error {
	if h.AuthToken == "" {
		if !h.AllowUnauthenticated {
			return ErrAuthTokenRequired
		}
		if len(h.AllowedHosts) == 0 {
			return ErrUnauthenticatedNeedsHosts
		}
		return nil
	}
	if len(h.AuthToken) < MinAuthTokenLength {
		return fmt.Errorf("MCP_AUTH_TOKEN must be at least %d characters long (generate one with: openssl rand -hex 32)", MinAuthTokenLength)
	}
	return nil
}

// ErrUnauthenticatedNeedsHosts is returned for MCP_ALLOW_UNAUTHENTICATED=true without MCP_ALLOWED_HOSTS.
var ErrUnauthenticatedNeedsHosts = errors.New("MCP_ALLOW_UNAUTHENTICATED=true also requires MCP_ALLOWED_HOSTS: without a token, the Host allow-list is what stops a web page from reaching the server through DNS rebinding. List the host names clients use, e.g. MCP_ALLOWED_HOSTS=mcp.example.lan,localhost")

// Env looks up a variable; os.LookupEnv in production, a map in tests.
type Env func(string) (string, bool)

// MapEnv adapts a map for tests.
func MapEnv(m map[string]string) Env {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// placeholder matches a ${user_config.<key>} reference that an MCP Bundle host such as Claude
// Desktop passed through unchanged because the user left an optional setting empty. It means
// "not set", not a value.
var placeholder = regexp.MustCompile(`^\$\{user_config\.[^}]*\}$`)

// lookup returns a variable as set, or "" when it is unset or an unfilled bundle placeholder.
func lookup(env Env, name string) string {
	v, _ := env(name)
	if placeholder.MatchString(strings.TrimSpace(v)) {
		return ""
	}
	return v
}

// read returns a trimmed variable ("" when unset, blank or an unfilled bundle placeholder), also
// honouring the <NAME>_FILE convention so secrets can be supplied as Docker secrets mounted under
// /run/secrets.
func read(env Env, name string) (string, error) {
	if file := strings.TrimSpace(lookup(env, name+"_FILE")); file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("Could not read %s_FILE (%s): %s", name, file, errText(err))
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(lookup(env, name)), nil
}

// readSecret is read for a value that may legitimately start or end with spaces, such as a
// password: the environment value is kept as is, and a _FILE only loses its final line break.
func readSecret(env Env, name string) (string, error) {
	if file := strings.TrimSpace(lookup(env, name+"_FILE")); file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("Could not read %s_FILE (%s): %s", name, file, errText(err))
		}
		s := strings.TrimSuffix(string(b), "\n")
		return strings.TrimSuffix(s, "\r"), nil
	}
	return lookup(env, name), nil
}

// ErrTLSChoiceRequired is returned for an https PROXMOX_URL with no way to check the certificate chosen.
var ErrTLSChoiceRequired = errors.New("PROXMOX_URL uses https, but no way to check the Proxmox certificate is configured. Proxmox ships a self-signed certificate, so choose one: " +
	"PROXMOX_TLS_FINGERPRINT=<SHA-256 fingerprint> (recommended; shown under Node > System > Certificates, or run on the node: openssl x509 -in /etc/pve/local/pve-ssl.pem -noout -fingerprint -sha256), " +
	"PROXMOX_CA_FILE=<CA certificate file>, PROXMOX_VERIFY_SSL=true for a publicly trusted certificate, " +
	"or PROXMOX_VERIFY_SSL=false to skip verification (insecure: the token or password can be intercepted)")

// Lookup reads one variable the way Load does: trimmed, honouring <NAME>_FILE.
func Lookup(env Env, name string) (string, error) { return read(env, name) }

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
	if parsed.User != nil {
		// Not echoed: the URL holds a password.
		return nil, errors.New("PROXMOX_URL must not contain a user name or password (user:pass@); use an API token (PROXMOX_API_TOKEN) or PROXMOX_USERNAME and PROXMOX_PASSWORD")
	}
	https := strings.EqualFold(parsed.Scheme, "https")

	tokenID, tokenSecret, err := parseToken(env)
	if err != nil {
		return nil, err
	}
	username := get("PROXMOX_USERNAME")
	password, err := readSecret(env, "PROXMOX_PASSWORD")
	if err != nil {
		return nil, err
	}
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
	auditLog := get("PROXMOX_AUDIT_LOG")
	if firstErr != nil {
		return nil, firstErr
	}
	if allowDeletes && !allowWrites {
		return nil, errors.New("PROXMOX_ALLOW_DELETES=true also requires PROXMOX_ALLOW_WRITES=true")
	}
	if allowExec && !allowWrites {
		return nil, errors.New("PROXMOX_ALLOW_EXEC=true also requires PROXMOX_ALLOW_WRITES=true")
	}

	if err := CheckAuditLog(auditLog); err != nil {
		return nil, err
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
	if transport == "http" {
		if err := CheckHTTPAuth(HTTP{AuthToken: authToken, AllowUnauthenticated: allowUnauthenticated, AllowedHosts: allowedHosts}); err != nil {
			return nil, err
		}
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
	verifySetting := get("PROXMOX_VERIFY_SSL")
	verify := caCert != "" || boolValue(verifySetting, false)
	if firstErr != nil {
		return nil, firstErr
	}
	// Proxmox ships a self-signed certificate, so an unverified connection used to be the silent
	// default. Now the operator has to choose: pin it, trust a CA, verify, or explicitly not.
	if https && verifySetting == "" && caCert == "" && len(fingerprints) == 0 {
		return nil, ErrTLSChoiceRequired
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
		Tools:     Tools{AllowWrites: allowWrites, AllowDeletes: allowDeletes, AllowExec: allowExec, AllowUnredacted: allowUnredacted, Toolsets: toolsets, AuditLog: auditLog},
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
