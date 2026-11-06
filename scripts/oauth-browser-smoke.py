#!/usr/bin/env python3
"""Browser regression against a locally built server. No external service calls.

Usage: python3 scripts/oauth-browser-smoke.py /path/to/proxmox-mcp
Requires Playwright and its Chromium browser.
Browser routes provide synthetic HTTPS origins and forward issuer requests to the
real local HTTP server, preserving the browser's Origin, form body and cookies.
"""
import base64
import hashlib
import http.client
import json
import os
import socket
import subprocess
import sys
import tempfile
import time
import urllib.parse

from playwright.sync_api import sync_playwright

ISSUER = "https://mcp.example"
CALLBACK = "https://chatgpt.com/connector_platform_oauth_redirect"
PASSWORD = "oauth-smoke-test-password"
HASH = "$2a$10$KWMilWRQAVDESlWCeGEPzu5S/r4zrMkk7wztMct.038H8MtNbi4am"
BASE_CSP = "default-src 'none'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

def main():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]

    def request(path, method="GET", body=None, headers=None):
        connection = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
        try:
            outgoing = dict(headers or {})
            outgoing["Host"] = "mcp.example"
            connection.request(method, path, body, outgoing)
            response = connection.getresponse()
            return response.status, dict(response.getheaders()), response.read()
        finally:
            connection.close()

    with tempfile.TemporaryDirectory() as state:
        env = dict(os.environ, MCP_OAUTH_ISSUER=ISSUER,
                   MCP_OAUTH_REDIRECT_URIS=CALLBACK, MCP_OAUTH_ADMIN_HASH=HASH,
                   MCP_OAUTH_STATE_DIR=state, MCP_HTTP_PORT=str(port),
                   MCP_AUTH_TOKEN="public-browser-fixture",
                   PROXMOX_URL="http://127.0.0.1:1",
                   PROXMOX_API_TOKEN="ci@pve!ci=public-fixture")
        # A file binding would override the public fixture.
        env.pop("MCP_OAUTH_ADMIN_HASH_FILE", None)
        server = subprocess.Popen([sys.argv[1]], env=env,
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            for _ in range(100):
                try:
                    if request("/health")[0] == 200:
                        break
                except OSError:
                    pass
                if server.poll() is not None:
                    raise AssertionError("server exited")
                time.sleep(0.1)
            else:
                raise AssertionError("server did not start")
            status, _, body = request("/oauth/register", "POST", json.dumps({
                "redirect_uris": [CALLBACK], "token_endpoint_auth_method": "none"
            }), {"Content-Type": "application/json"})
            assert status == 201
            client = json.loads(body)["client_id"]
            verifier = "a" * 43
            challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip("=")
            query = urllib.parse.urlencode(dict(
                client_id=client, redirect_uri=CALLBACK, resource=ISSUER + "/mcp",
                response_type="code", code_challenge_method="S256",
                code_challenge=challenge, state="browser"))
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch()
                try:
                    # The control proves this browser enforces redirect form-action.
                    for old_policy in (True, False):
                        context = browser.new_context()
                        page = context.new_page()
                        posts, callbacks, violations = [], [], []
                        page.on("console", lambda msg: violations.append(msg.text)
                                if "form-action" in msg.text else None)

                        def route_request(route):
                            req = route.request
                            if req.url.startswith(CALLBACK):
                                callbacks.append(req.url)
                                route.fulfill(status=200, content_type="text/html",
                                              body="<title>Callback reached</title>")
                                return
                            assert req.url.startswith(ISSUER + "/")
                            parsed = urllib.parse.urlsplit(req.url)
                            path = parsed.path + ("?" + parsed.query if parsed.query else "")
                            status, headers, body = request(path, req.method, req.post_data_buffer,
                                                            req.all_headers())
                            if req.method == "POST":
                                posts.append((status, headers))
                            if old_policy and req.method == "GET" and parsed.path == "/oauth/authorize":
                                headers["Content-Security-Policy"] = headers["Content-Security-Policy"].replace(
                                    "form-action 'self' https://chatgpt.com", "form-action 'self'")
                            route.fulfill(status=status, headers=headers, body=body)

                        context.route("**/*", route_request)
                        page.goto(ISSUER + "/oauth/authorize?" + query)
                        page.locator("#password").fill(PASSWORD)
                        page.get_by_role("button", name="Authorize", exact=True).click(no_wait_after=True)
                        if old_policy:
                            for _ in range(100):
                                if violations:
                                    break
                                page.wait_for_timeout(50)
                            assert violations and not callbacks, "old CSP did not block callback"
                        else:
                            page.wait_for_url(CALLBACK + "?**")
                            assert len(callbacks) == 1 and not violations
                            values = urllib.parse.parse_qs(urllib.parse.urlsplit(callbacks[0]).query)
                            assert values["code"] and values["state"] == ["browser"]
                            assert values["iss"] == [ISSUER]
                        assert len(posts) == 1 and posts[0][0] == 303
                        assert posts[0][1]["Content-Security-Policy"] == BASE_CSP
                        context.close()
                finally:
                    browser.close()
            print("PASS: old CSP blocks; configured CSP permits form POST -> 303 -> cross-origin callback")
        finally:
            server.terminate()
            server.wait(timeout=10)

if __name__ == "__main__":
    main()
