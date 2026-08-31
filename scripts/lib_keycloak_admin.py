"""Shared Keycloak admin-API access for the kc-user-*/kc-pat-issue scripts.

Import this; it is not meant to be run directly. `/sso/admin` is refused at
the public edge (see docs/operations/README.md), and the admin console
itself cannot be driven through a port-forward either: `KC_HOSTNAME_ADMIN`
is pinned to a fixed `http://localhost:8888/sso` origin and the console
expects that same public origin back for its third-party cookie check,
which a forwarded port never supplies. The admin REST API has no such
check, so every caller here goes through a port-forward and talks to it
directly instead.
"""
import json
import os
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


class KeycloakAdmin:
    def __init__(self):
        self.namespace = os.environ.get("K8S_NAMESPACE", "airgap-ai-stack")
        self.realm = os.environ.get("STACK_REALM", "ai-stack")
        self.admin_user = os.environ.get("KEYCLOAK_ADMIN_USER", "admin")
        self.admin_password = os.environ.get("KEYCLOAK_ADMIN_PASSWORD", "admin-demo-only")
        self._proc = None
        self.admin_api = None
        self.admin_issuer = None
        self.token = None

    def __enter__(self):
        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
        sock.close()

        self._proc = subprocess.Popen(
            ["kubectl", "-n", self.namespace, "port-forward", "svc/keycloak", f"{port}:8080"],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        discovery = f"http://127.0.0.1:{port}/sso/realms/{self.realm}/.well-known/openid-configuration"
        for _ in range(15):
            try:
                urllib.request.urlopen(discovery, timeout=2)
                break
            except Exception:
                time.sleep(1)
        else:
            self._cleanup()
            sys.exit("Keycloak port-forward did not become ready")

        self.admin_issuer = f"http://127.0.0.1:{port}/sso/realms/master"
        self.admin_api = f"http://127.0.0.1:{port}/sso/admin/realms/{self.realm}"
        self.token = self._fetch_token()
        return self

    def __exit__(self, exc_type, exc_value, traceback):
        self._cleanup()

    def _cleanup(self):
        if self._proc:
            self._proc.terminate()
            try:
                self._proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self._proc.kill()

    def _fetch_token(self):
        form = urllib.parse.urlencode(
            {
                "grant_type": "password",
                "client_id": "admin-cli",
                "username": self.admin_user,
                "password": self.admin_password,
            }
        ).encode()
        with urllib.request.urlopen(f"{self.admin_issuer}/protocol/openid-connect/token", data=form) as resp:
            return json.load(resp)["access_token"]

    def request(self, method, path, data=None):
        url = path if path.startswith("http") else f"{self.admin_api}{path}"
        headers = {"Authorization": f"Bearer {self.token}"}
        body = None
        if data is not None:
            body = json.dumps(data).encode()
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(url, data=body, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req) as resp:
                raw = resp.read()
                return json.loads(raw) if raw else None
        except urllib.error.HTTPError as e:
            sys.exit(f"{method} {url} -> {e.code}: {e.read().decode(errors='replace')}")
