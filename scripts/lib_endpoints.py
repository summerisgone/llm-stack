"""Shared endpoint derivation for kc-pat-issue — a Python port of the
relevant parts of lib-endpoints.sh. Import this; it is not meant to be run
directly.

Two routing profiles, same as the shell library:

  path mode  (STACK_BASE_URL set)   remote profile. One public origin,
                                     routes selected by path only.
  host mode  (STACK_BASE_URL unset) local-mac profile. Host-based
                                     *.localhost routes on the development
                                     edge :8080. STACK_TUNNEL_PORT redirects
                                     those hostnames at a local tunnel port,
                                     the same way stack_curl's --connect-to
                                     does in the shell scripts.
"""
import os
import urllib.parse
import urllib.request


class Endpoints:
    def __init__(self):
        self.realm = os.environ.get("STACK_REALM", "ai-stack")
        base = os.environ.get("STACK_BASE_URL", "").rstrip("/")
        self.tunnel_port = os.environ.get("STACK_TUNNEL_PORT")

        if base:
            self.mode = "path"
            sso_base = f"{base}/sso"
            self.issuer = f"{sso_base}/realms/{self.realm}"
            self.dashboard = f"{base}/platform"
            self.api = f"{base}/v1"
        else:
            self.mode = "host"
            sso_base = "http://sso.ai.localhost:8080/sso"
            self.issuer = f"{sso_base}/realms/{self.realm}"
            self.dashboard = "http://tokens.ai.localhost:8080"
            self.api = "http://api.ai.localhost:8080/v1"

    def open(self, method, url, data=None, headers=None):
        headers = dict(headers or {})
        if self.mode == "host" and self.tunnel_port:
            parts = urllib.parse.urlsplit(url)
            headers.setdefault("Host", parts.netloc)
            url = urllib.parse.urlunsplit(
                (parts.scheme, f"127.0.0.1:{self.tunnel_port}", parts.path, parts.query, parts.fragment)
            )
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        return urllib.request.urlopen(req)
