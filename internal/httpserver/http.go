// Package httpserver serves MCP over Streamable HTTP, with bearer auth, a Host/Origin allow-list
// and a health endpoint.
package httpserver

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mattoddie/proxmox-mcp/internal/config"
)

// maxBodyBytes bounds a request body; tool arguments are small.
const maxBodyBytes = 4 << 20

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(body)
	_, _ = w.Write(b)
}

type rpcErrorBody struct {
	JSONRPC string `json:"jsonrpc"`
	Error   struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	ID any `json:"id"`
}

func rpcError(w http.ResponseWriter, status int, message string) {
	var body rpcErrorBody
	body.JSONRPC = "2.0"
	body.Error.Code = -32000
	body.Error.Message = message
	writeJSON(w, status, body)
}

var bearerRe = regexp.MustCompile(`(?is)^Bearer\s+(.+)$`)

func tokenMatches(header, token string) bool {
	presented := ""
	if m := bearerRe.FindStringSubmatch(header); m != nil {
		presented = m[1]
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

// splitHostPort splits "host", "host:port", "[v6]" or "[v6]:port" (lower-cased) into a hostname
// without brackets and a port ("" when none is given). A bare IPv6 address has no port.
func splitHostPort(s string) (host, port string) {
	s = strings.ToLower(s)
	if h, p, err := net.SplitHostPort(s); err == nil {
		return strings.Trim(h, "[]"), p
	}
	return strings.Trim(s, "[]"), ""
}

// hostAllowed reports whether a Host header value is on the allow-list. An entry without a port
// matches any port (the port the server is reached on varies behind Docker and proxies); an
// entry with one matches only that port.
func hostAllowed(allowed []string, hostHeader string) bool {
	host, port := splitHostPort(hostHeader)
	if host == "" {
		return false
	}
	for _, a := range allowed {
		ah, ap := splitHostPort(a)
		if ah == host && (ap == "" || ap == port) {
			return true
		}
	}
	return false
}

// originAllowed reports whether an Origin header value is on the allow-list. Only http and https
// origins can match. An entry without a port matches only the scheme's default port, so a page
// on another port of an allowed host (another app on localhost, say) is refused; list host:port
// to allow one.
func originAllowed(allowed []string, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	defaultPort := map[string]string{"http": "80", "https": "443"}[scheme]
	if defaultPort == "" {
		return false
	}
	host, port := splitHostPort(u.Host)
	if port == "" {
		port = defaultPort
	}
	for _, a := range allowed {
		ah, ap := splitHostPort(a)
		if ap == "" {
			ap = defaultPort
		}
		if ah == host && ap == port {
			return true
		}
	}
	return false
}

// Handler serves /mcp (stateless Streamable HTTP, so the container can be restarted or scaled
// freely) and /health.
func Handler(cfg config.HTTP, server *mcp.Server) http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		// Host checks are MCP_ALLOWED_HOSTS's job, as in the original server: the SDK's built-in
		// localhost check would refuse a same-host reverse proxy that passes the public Host on.
		DisableLocalhostProtection: true,
		MaxRequestBodyBytes:        maxBodyBytes,
	})
	mux := http.NewServeMux()
	health := func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
	mux.HandleFunc("/health", health)
	mux.HandleFunc("/healthz", health)
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if len(cfg.AllowedHosts) > 0 {
			host := strings.ToLower(r.Host)
			if !hostAllowed(cfg.AllowedHosts, host) {
				rpcError(w, http.StatusForbidden, "Host "+host+" is not allowed")
				return
			}
			// Browsers send Origin on cross-site requests; refuse pages served from other hosts (DNS rebinding, CSRF).
			if origins, sent := r.Header["Origin"]; sent {
				origin := strings.Join(origins, ", ")
				if len(origins) != 1 || !originAllowed(cfg.AllowedHosts, origin) {
					rpcError(w, http.StatusForbidden, "Origin "+origin+" is not allowed")
					return
				}
			}
		}
		if cfg.AuthToken != "" && !tokenMatches(r.Header.Get("Authorization"), cfg.AuthToken) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			rpcError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		if r.Method != http.MethodPost {
			// Stateless mode: no server-initiated SSE stream and no sessions to delete.
			w.Header().Set("Allow", "POST")
			rpcError(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil {
			rpcError(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(body) > maxBodyBytes {
			rpcError(w, http.StatusBadRequest, "Request body too large")
			return
		}
		if len(body) > 0 && !json.Valid(body) {
			rpcError(w, http.StatusBadRequest, "Invalid JSON body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		mcpHandler.ServeHTTP(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found. The MCP endpoint is /mcp"})
	})
	return mux
}

// Addr is the listen address.
func Addr(cfg config.HTTP) string {
	return net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
}

// Listen checks the auth settings and opens the listener, so a missing token fails before
// anything is served.
func Listen(cfg config.HTTP) (net.Listener, error) {
	if err := config.CheckHTTPAuth(cfg); err != nil {
		return nil, err
	}
	return net.Listen("tcp", Addr(cfg))
}

// Serve serves on ln until the server is shut down.
func Serve(srv *http.Server, ln net.Listener, cfg config.HTTP) error {
	auth := ""
	if cfg.AuthToken != "" {
		auth = " (bearer auth enabled)"
	}
	log.Printf("proxmox-mcp listening on http://%s:%d/mcp%s", cfg.Host, cfg.Port, auth)
	if cfg.AuthToken == "" {
		log.Printf("WARNING: MCP_ALLOW_UNAUTHENTICATED=true: anyone who can reach this port can use every enabled tool. Only run it like this behind something else that authenticates.")
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
