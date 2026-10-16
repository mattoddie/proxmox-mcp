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

var portSuffix = regexp.MustCompile(`:\d+$`)

// hostAllowed reports whether host (a Host header value or an Origin's host, lower-cased) or
// its hostname is on the allow-list.
func hostAllowed(allowed []string, host string) bool {
	hostname := portSuffix.ReplaceAllString(host, "")
	for _, a := range allowed {
		if a == host || a == hostname {
			return true
		}
	}
	return false
}

// originHost is an Origin header's host[:port], lower-cased, or "" for "null" or an unparsable origin.
func originHost(origin string) string {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Host)
	// Like a browser's URL parser, drop the scheme's default port.
	if (u.Scheme == "https" && strings.HasSuffix(host, ":443")) || (u.Scheme == "http" && strings.HasSuffix(host, ":80")) {
		host = host[:strings.LastIndex(host, ":")]
	}
	return host
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
				if oh := originHost(origin); oh == "" || !hostAllowed(cfg.AllowedHosts, oh) {
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
	if cfg.AuthToken == "" && !cfg.AllowUnauthenticated {
		return nil, config.ErrAuthTokenRequired
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
