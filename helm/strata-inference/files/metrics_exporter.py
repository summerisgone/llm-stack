#!/usr/bin/env python3
"""Re-exposes Strata's JSON GET /metrics as Prometheus text on :9400/metrics.

Strata's /metrics is the web Monitor's feed (serve/server.py metrics()), not
Prometheus text. This is the ADR 0015 Tier 2/3 adapter: it reprojects only
fields that response has. `totals` are sums since the server started, so they
are counters (they restart from zero with the engine); `live` gives the state
and the FIFO depth. strata_up is 0 while the server does not answer, which is
also the case while the model is still loading.
"""
import json
import os
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

URL = os.environ.get("STRATA_URL", "http://127.0.0.1:8000") + "/metrics"
PORT = int(os.environ.get("STRATA_EXPORTER_PORT", "9400"))
KEY = os.environ.get("STRATA_API_KEY", "")

COUNTERS = [
    ("requests", "strata_requests_total", "Finished requests.", 1),
    ("prompt_tokens", "strata_prompt_tokens_total", "Prompt tokens, read and reused.", 1),
    ("reused", "strata_prompt_reused_tokens_total", "Prompt tokens reused from the previous conversation state.", 1),
    ("output_tokens", "strata_output_tokens_total", "Generated tokens.", 1),
    ("prompt_ms", "strata_prompt_seconds_total", "Time spent reading prompts.", 0.001),
    ("decode_ms", "strata_decode_seconds_total", "Time spent generating.", 0.001),
    ("drafts_offered", "strata_drafts_offered_total", "MTP draft tokens offered.", 1),
    ("drafts_accepted", "strata_drafts_accepted_total", "MTP draft tokens accepted.", 1),
]
STATES = ("idle", "reading", "generating", "unloaded")


def scrape() -> str:
    try:
        req = urllib.request.Request(URL, headers={"Authorization": "Bearer " + KEY} if KEY else {})
        with urllib.request.urlopen(req, timeout=5) as resp:
            data = json.load(resp)
    except Exception:
        return "# TYPE strata_up gauge\nstrata_up 0\n"
    out = ["# TYPE strata_up gauge", "strata_up 1"]
    totals = data.get("totals") or {}
    for key, name, help_, scale in COUNTERS:
        if isinstance(totals.get(key), (int, float)):
            out += [f"# HELP {name} {help_}", f"# TYPE {name} counter", f"{name} {totals[key] * scale}"]
    live = data.get("live") or {}
    out += ["# HELP strata_queued Requests waiting behind the one being served.", "# TYPE strata_queued gauge",
            f"strata_queued {live.get('queued') or 0}",
            "# HELP strata_state 1 for the server's current state.", "# TYPE strata_state gauge"]
    out += [f'strata_state{{state="{s}"}} {int(live.get("state") == s)}' for s in STATES]
    return "\n".join(out) + "\n"


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/metrics":
            self.send_response(404)
            self.end_headers()
            return
        body = scrape().encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    HTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
