#!/usr/bin/env python3
"""Isolated OAuth runtime test. Uses only public test credentials; no Proxmox calls."""
import base64
import hashlib
import http.client
import json
import re
import subprocess
import sys
import time
import urllib.parse
import uuid

IMAGE = sys.argv[1] if len(sys.argv) > 1 else "proxmox-mcp:ci"
NAME = "oauth-smoke-" + uuid.uuid4().hex[:12]
STATE, SECRETS = NAME + "-state", NAME + "-secrets"
ISSUER = "https://mcp.example"
REDIRECT = "https://chatgpt.com/connector_platform_oauth_redirect"
# Public fixture, deliberately not a production credential.
PASSWORD = "oauth-smoke-test-password"
HASH = "$2a$10$KWMilWRQAVDESlWCeGEPzu5S/r4zrMkk7wztMct.038H8MtNbi4am"

def docker(*args):
    return subprocess.check_output(["docker", *args], text=True).strip()

def port():
    settings = json.loads(docker("inspect", NAME))[0]
    return int(settings["NetworkSettings"]["Ports"]["8080/tcp"][0]["HostPort"])

def request(path, data=None, token=None, cookie=None, method=None):
    headers = {"Host": "mcp.example", "Origin": ISSUER}
    if data is not None:
        if path in ("/oauth/register", "/mcp"):
            data = json.dumps(data)
            headers["Content-Type"] = "application/json"
        else:
            data = urllib.parse.urlencode(data)
            headers["Content-Type"] = "application/x-www-form-urlencoded"
    if token:
        headers["Authorization"] = "Bearer " + token
    if cookie:
        headers["Cookie"] = cookie
    headers["Accept"] = "application/json, text/event-stream"
    connection = http.client.HTTPConnection("127.0.0.1", port(), timeout=10)
    try:
        connection.request(method or ("POST" if data is not None else "GET"), path, data, headers)
        response = connection.getresponse()
        return response.status, {key.lower(): value for key, value in response.getheaders()}, response.read().decode()
    finally:
        connection.close()

def ready():
    for _ in range(40):
        try:
            if request("/health")[0] == 200:
                return
        except (OSError, http.client.HTTPException):
            pass
        time.sleep(0.25)
    raise AssertionError("container did not become ready")

def success(path, data, token=None):
    status, headers, body = request(path, data, token)
    assert status == 200, (path, status)  # Never print token-bearing bodies.
    return json.loads(body)

try:
    docker("volume", "create", STATE)
    docker("volume", "create", SECRETS)
    # The helper provisions only disposable named volumes, never host directories.
    docker("run", "--rm", "--user", "0:0", "-v", STATE + ":/state",
           "-v", SECRETS + ":/secrets", "busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec", "sh", "-c",
           'chown 65532:65532 /state; chmod 700 /state; printf %s "$1" > /secrets/admin; chmod 444 /secrets/admin',
           "sh", HASH + "\n\n\r\n")
    docker("run", "-d", "--name", NAME, "--read-only", "--cap-drop=ALL",
           "--security-opt=no-new-privileges", "-p", "127.0.0.1::8080",
           "-v", STATE + ":/state", "-v", SECRETS + ":/run/secrets:ro",
           "-e", "MCP_OAUTH_ISSUER=" + ISSUER,
           "-e", "MCP_OAUTH_REDIRECT_URIS=" + REDIRECT,
           "-e", "MCP_OAUTH_STATE_DIR=/state",
           "-e", "MCP_OAUTH_ADMIN_HASH_FILE=/run/secrets/admin",
           "-e", "MCP_AUTH_TOKEN=public-test-static-token",
           "-e", "PROXMOX_URL=http://127.0.0.1:1",
           "-e", "PROXMOX_API_TOKEN=ci@pve!ci=public-fixture", IMAGE)
    ready()
    config = json.loads(docker("inspect", NAME))[0]
    assert config["Config"]["User"] == "nonroot:nonroot"
    assert config["HostConfig"]["ReadonlyRootfs"]
    status, headers, _ = request("/mcp", {})
    assert status == 401 and "resource_metadata" in headers["www-authenticate"]
    assert json.loads(request("/.well-known/oauth-protected-resource/mcp")[2])["resource"] == ISSUER + "/mcp"
    status, _, body = request("/oauth/register", {
        "redirect_uris": [REDIRECT], "token_endpoint_auth_method": "none"})
    assert status == 201
    client = json.loads(body)["client_id"]
    verifier = "a" * 43
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip("=")
    query = urllib.parse.urlencode(dict(client_id=client, redirect_uri=REDIRECT,
        resource=ISSUER + "/mcp", response_type="code", code_challenge_method="S256",
        code_challenge=challenge, state="smoke"))
    # A failed password must stay generic publicly and diagnostic server-side.
    status, headers, body = request("/oauth/authorize?" + query)
    assert status == 200
    rejected_csrf = re.search(r'name="request" value="([^"]+)"', body)[1]
    rejected_cookie = headers["set-cookie"].split(";")[0]
    status, _, body = request("/oauth/authorize", {
        "request": rejected_csrf, "decision": "allow", "password": "wrong-public-fixture"}, cookie=rejected_cookie)
    assert status == 403 and json.loads(body)["error"] == "access_denied"
    assert "password_verification_failed" not in body
    status, headers, body = request("/oauth/authorize?" + query)
    assert status == 200
    assert headers["referrer-policy"] == "same-origin"
    assert headers["cache-control"] == "no-store"
    assert "default-src 'none'" in headers["content-security-policy"]
    assert "unsafe-inline" not in headers["content-security-policy"]
    assert all(flag in headers["set-cookie"] for flag in ("Secure", "HttpOnly", "SameSite=Strict"))
    csrf = re.search(r'name="request" value="([^"]+)"', body)[1]
    cookie = headers["set-cookie"].split(";")[0]
    status, headers, _ = request("/oauth/authorize", {
        "request": csrf, "decision": "allow", "password": PASSWORD}, cookie=cookie)
    assert status == 303
    redirect = urllib.parse.parse_qs(urllib.parse.urlsplit(headers["location"]).query)
    assert redirect["iss"] == [ISSUER] and redirect["state"] == ["smoke"]
    pair = success("/oauth/token", dict(grant_type="authorization_code", client_id=client,
        code=redirect["code"][0], code_verifier=verifier, redirect_uri=REDIRECT,
        resource=ISSUER + "/mcp"))
    def tools(token):
        result = success("/mcp", {"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, token)
        assert result["result"]["tools"]
        assert all(tool.get("annotations", {}).get("readOnlyHint") for tool in result["result"]["tools"])
    tools(pair["access_token"])
    tools("public-test-static-token")
    docker("restart", NAME)
    ready()
    tools(pair["access_token"])  # Persisted state and mounted secret work after restart.
    rotated = success("/oauth/token", dict(grant_type="refresh_token", client_id=client,
        refresh_token=pair["refresh_token"], resource=ISSUER + "/mcp"))
    assert request("/mcp", {}, pair["access_token"])[0] == 401
    tools(rotated["access_token"])
    assert request("/oauth/revoke", dict(client_id=client, token=rotated["refresh_token"]))[0] == 200
    docker("restart", NAME)
    ready()
    assert request("/mcp", {}, rotated["access_token"])[0] == 401
    tools("public-test-static-token")
    # State is private and contains only digests, never issued bearer credentials.
    saved = docker("run", "--rm", "-v", STATE + ":/state:ro", "busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec",
                   "sh", "-c", "test $(stat -c %a /state/state.json) = 600 && cat /state/state.json")
    assert pair["access_token"] not in saved and pair["refresh_token"] not in saved
    logs = subprocess.run(["docker", "logs", NAME], check=True, stdout=subprocess.PIPE,
                          stderr=subprocess.STDOUT, text=True).stdout
    events = [json.loads(line) for line in logs.splitlines() if line.startswith('{')]
    assert events == [{"event": "oauth_authorization_rejected",
                       "reason": "password_verification_failed", "status": 403}]
    for secret in (PASSWORD, HASH, "wrong-public-fixture", csrf, rejected_csrf,
                   cookie, rejected_cookie, redirect["code"][0], verifier,
                   pair["access_token"], pair["refresh_token"],
                   rotated["access_token"], rotated["refresh_token"]):
        assert secret not in logs, "sensitive value appeared in server logs"
    print("OAuth Docker smoke passed: non-root/read-only, mounted secret with extra newlines, safe rejection diagnostics, fresh consent, MCP read-only tools, restart, refresh and durable revocation.")
finally:
    subprocess.run(["docker", "rm", "-f", NAME], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    subprocess.run(["docker", "volume", "rm", STATE, SECRETS], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

