package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"html/template"
	"net/http"
	"net/url"
	"strings"
)

// Only these static style/script blocks may execute; client data stays in
// html/template's escaped text and attribute contexts.
func consentPolicy(redirects []string) string {
	// Chrome checks the form's redirect chain against the submitting page's CSP.
	// Use only the server's validated configuration, never request parameters.
	formAction := "'self'"
	seen := map[string]bool{}
	for _, redirect := range redirects {
		u, err := url.Parse(redirect)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
			strings.ContainsAny(u.Host, "*;'\" \t\r\n") {
			continue
		}
		origin := u.Scheme + "://" + u.Host
		if !seen[origin] {
			formAction += " " + origin
			seen[origin] = true
		}
	}
	style := sha256.Sum256([]byte(consentStyle))
	script := sha256.Sum256([]byte(consentScript))
	return "default-src 'none'; form-action " + formAction + "; frame-ancestors 'none'; base-uri 'none'; style-src 'sha256-" +
		base64.StdEncoding.EncodeToString(style[:]) + "'; script-src 'sha256-" +
		base64.StdEncoding.EncodeToString(script[:]) + "'"
}

const consentStyle = `
:root { color-scheme: light dark; --bg: #f3f5f8; --card: #fff; --text: #182230; --muted: #526174; --border: #d4dbe5; --accent: #175cd3; --on-accent: #fff; }
* { box-sizing: border-box; }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; padding: 24px; background: var(--bg); color: var(--text); font: 1rem/1.6 system-ui, sans-serif; }
main { width: 100%; max-width: 560px; padding: 32px; border: 1px solid var(--border); border-radius: 20px; background: var(--card); box-shadow: 0 16px 48px #0000000a; }
.eyebrow { margin: 0; color: var(--muted); font-size: .8rem; font-weight: 700; letter-spacing: .08em; text-transform: uppercase; }
h1 { margin: 8px 0 12px; font-size: 1.85rem; line-height: 1.2; }
h2 { margin: 24px 0 8px; font-size: 1.05rem; }
p { margin: 8px 0; }
.muted, .hint { color: var(--muted); }
.hint { font-size: .875rem; }
dl { margin: 24px 0; padding: 16px; border: 1px solid var(--border); border-radius: 12px; }
dt { margin-top: 12px; color: var(--muted); font-size: .8rem; font-weight: 600; }
dt:first-child { margin-top: 0; }
dd { margin: 2px 0 0; overflow-wrap: anywhere; }
code { font-size: .875rem; }
label { display: block; margin: 24px 0 8px; font-weight: 600; }
.password { display: flex; gap: 8px; }
input { width: 100%; min-width: 0; padding: 12px; color: var(--text); background: var(--card); border: 1px solid var(--border); border-radius: 8px; font: inherit; }
button { min-height: 46px; padding: 10px 16px; border: 1px solid var(--border); border-radius: 8px; color: var(--text); background: var(--card); font: inherit; font-weight: 600; cursor: pointer; }
button:hover { filter: brightness(.92); }
input:focus-visible, button:focus-visible { outline: 3px solid var(--accent); outline-offset: 3px; }
.actions { display: flex; gap: 12px; margin: 24px 0; }
.actions button { flex: 1; }
.primary { color: var(--on-accent); background: var(--accent); border-color: var(--accent); }
[hidden] { display: none; }
@media (prefers-color-scheme: dark) { :root { --bg: #101828; --card: #1d2939; --text: #f2f4f7; --muted: #b7c4d5; --border: #475467; --accent: #84adff; --on-accent: #101828; } }
@media (max-width: 480px) { body { padding: 12px; } main { padding: 24px 20px; } h1 { font-size: 1.6rem; } .actions { flex-direction: column; } }
`

const consentScript = `
const password = document.getElementById('password');
const toggle = document.getElementById('toggle-password');
toggle.hidden = false;
toggle.addEventListener('click', () => {
  const show = password.type === 'password';
  password.type = show ? 'text' : 'password';
  toggle.textContent = show ? 'Hide' : 'Show';
  toggle.setAttribute('aria-label', show ? 'Hide password' : 'Show password');
  toggle.setAttribute('aria-pressed', String(show));
});
`

var consent = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Authorize Proxmox MCP</title>
<style>` + consentStyle + `</style>
</head>
<body>
<main aria-labelledby="title">
<p class="eyebrow">Proxmox MCP · Connection request</p>
<h1 id="title">Authorize access</h1>
<p class="muted">Review this client's details before connecting it to your Proxmox MCP server.</p>
<dl>
<dt>Client name (unverified)</dt><dd>{{if .Name}}{{.Name}}{{else}}Not provided{{end}}</dd>
<dt>Client ID</dt><dd><code>{{.ClientID}}</code></dd>
<dt>Redirect destination</dt><dd>{{.Redirect}}</dd>
<dt>Requested scope</dt><dd><code>mcp</code> — access to enabled MCP tools</dd>
</dl>
<h2>What you are allowing</h2>
<p>This client can use all currently enabled Proxmox tools. Depending on server configuration, these may include reading data, making changes, deleting resources or executing commands. Server permission controls and Proxmox account permissions still apply.</p>
<p class="hint">Only authorize a client you trust. The display name does not verify its identity.</p>
<form method="post" action="/oauth/authorize">
<input type="hidden" name="request" value="{{.ID}}">
<label for="password">Administrator password</label>
<div class="password">
<input id="password" type="password" name="password" required autocomplete="current-password" aria-describedby="password-help">
<button id="toggle-password" type="button" aria-controls="password" aria-label="Show password" aria-pressed="false" hidden>Show</button>
</div>
<p id="password-help" class="hint">Enter the MCP server administrator password to approve. You can deny without entering a password.</p>
<div class="actions">
<button class="primary" name="decision" value="allow">Authorize</button>
<button name="decision" value="deny" formnovalidate>Deny</button>
</div>
</form>
<p class="hint">This request expires after 5 minutes. If it expires or fails, start the connection again from your client.</p>
</main>
<script>` + consentScript + `</script>
</body>
</html>`))

// Keep failure reasons generic: never distinguish a bad password from an invalid
// or expired request/cookie. Requests are one-time, so recovery starts at the client.
func consentFail(w http.ResponseWriter, status int, code string) {
	description := "Unable to authorize this request. Start the connection again from your client and check your administrator password."
	if status == http.StatusTooManyRequests {
		description = "Too many requests. Wait a moment, then start the connection again from your client."
	} else if status == http.StatusServiceUnavailable {
		description = "Authorization is temporarily unavailable. Try again later or contact your server administrator."
	} else if status == http.StatusBadRequest {
		description = "This authorization request is invalid. Start the connection again from your client."
	}
	jsonReply(w, status, map[string]string{"error": code, "error_description": description})
}
