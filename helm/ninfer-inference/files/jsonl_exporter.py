#!/usr/bin/env python3
"""Tails ninfer-serve's --request-log-jsonl output and re-exposes it as
Prometheus text exposition on :9400/metrics.

ninfer has no native /metrics route (confirmed against src/serve/*.cpp and
docs/serving.md's endpoint table -- see docs/adr/0015 and
deploy/ninfer/README.md). Its only structured signal is this JSONL stream
(schema `ninfer_serve_request_log` v21: server_start, request_start,
request_rejected, request_done, request_error, throughput events -- see
src/serve/request_log.h/.cpp). This script is the Tier 2/3 adapter ADR-0015
calls for: it does not invent metrics ninfer doesn't emit, it only
reprojects fields that are actually present in the log records.

Field provenance (checked against the upstream source, not guessed):
- request_done: result.{prompt_tokens,completion_tokens,prefix_cache_hit_tokens},
  timings_seconds.{ttft,decode}
- throughput: scheduler.{running,waiting,prefilling,decode_ready},
  context_cache.occupancy.{device_main_kv_pages,device_backend_kv_pages,host_kv_bytes}
- server_start: memory.kv_capacity_page_groups (static, logged once)

timings_seconds.decode is the whole request's decode-phase duration, not a
per-token figure -- ninfer has no native per-token (TPOT/inter-token-latency)
timing field, unlike vLLM/SGLang's inter_token_latency_seconds. Do not treat
ninfer_request_decode_seconds as directly comparable to that metric.

KV page counts are exposed as raw occupied-page gauges, not a usage
percentage: this script has not independently confirmed that
memory.kv_capacity_page_groups (server_start) and
context_cache.occupancy.device_main_kv_pages (throughput) share the same
page-group unit, so no kv_cache_usage_perc-style ratio is computed here.
"""
import json
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

LOG_PATH = os.environ.get("NINFER_REQUEST_LOG", "/var/log/ninfer/requests.jsonl")
LISTEN_PORT = int(os.environ.get("NINFER_EXPORTER_PORT", "9400"))

TTFT_BUCKETS = (0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60)
DECODE_BUCKETS = (0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10)

_lock = threading.Lock()
_state = {
    "tailing": 0,
    "ttft_bucket_counts": [0] * (len(TTFT_BUCKETS) + 1),
    "ttft_sum": 0.0,
    "ttft_count": 0,
    "decode_bucket_counts": [0] * (len(DECODE_BUCKETS) + 1),
    "decode_sum": 0.0,
    "decode_count": 0,
    "prompt_tokens_total": 0,
    "generation_tokens_total": 0,
    "prefix_cache_hit_tokens_total": 0,
    "requests_done_total": 0,
    "requests_error_total": 0,
    "requests_rejected_total": 0,
    "running": 0,
    "waiting": 0,
    "prefilling": 0,
    "decode_ready": 0,
    "device_main_kv_pages": 0,
    "device_backend_kv_pages": 0,
    "host_kv_bytes": 0,
    "kv_capacity_page_groups": None,
}


def _bucket_index(value, buckets):
    for i, upper in enumerate(buckets):
        if value <= upper:
            return i
    return len(buckets)


def _handle_event(rec):
    kind = rec.get("event")
    with _lock:
        if kind == "server_start":
            cap = rec.get("memory", {}).get("kv_capacity_page_groups")
            if cap is not None:
                _state["kv_capacity_page_groups"] = cap
        elif kind == "request_done":
            result = rec.get("result", {})
            timings = rec.get("timings_seconds", {})
            _state["requests_done_total"] += 1
            _state["prompt_tokens_total"] += result.get("prompt_tokens", 0) or 0
            _state["generation_tokens_total"] += result.get("completion_tokens", 0) or 0
            _state["prefix_cache_hit_tokens_total"] += result.get("prefix_cache_hit_tokens", 0) or 0
            ttft = timings.get("ttft")
            if ttft is not None:
                _state["ttft_sum"] += ttft
                _state["ttft_count"] += 1
                _state["ttft_bucket_counts"][_bucket_index(ttft, TTFT_BUCKETS)] += 1
            decode = timings.get("decode")
            if decode is not None:
                _state["decode_sum"] += decode
                _state["decode_count"] += 1
                _state["decode_bucket_counts"][_bucket_index(decode, DECODE_BUCKETS)] += 1
        elif kind == "request_error":
            _state["requests_error_total"] += 1
        elif kind == "request_rejected":
            _state["requests_rejected_total"] += 1
        elif kind == "throughput":
            sched = rec.get("scheduler", {})
            occ = rec.get("context_cache", {}).get("occupancy", {})
            _state["running"] = sched.get("running", _state["running"])
            _state["waiting"] = sched.get("waiting", _state["waiting"])
            _state["prefilling"] = sched.get("prefilling", _state["prefilling"])
            _state["decode_ready"] = sched.get("decode_ready", _state["decode_ready"])
            _state["device_main_kv_pages"] = occ.get(
                "device_main_kv_pages", _state["device_main_kv_pages"])
            _state["device_backend_kv_pages"] = occ.get(
                "device_backend_kv_pages", _state["device_backend_kv_pages"])
            _state["host_kv_bytes"] = occ.get("host_kv_bytes", _state["host_kv_bytes"])


def _tail_loop():
    while not os.path.exists(LOG_PATH):
        time.sleep(1)
    with _lock:
        _state["tailing"] = 1
    with open(LOG_PATH, "r") as f:
        f.seek(0, os.SEEK_END)
        while True:
            line = f.readline()
            if not line:
                time.sleep(0.5)
                continue
            line = line.strip()
            if not line:
                continue
            try:
                rec = json.loads(line)
            except ValueError:
                continue
            _handle_event(rec)


def _histogram(name, help_text, buckets, bucket_counts, total_sum, total_count):
    lines = [
        "# HELP %s %s" % (name, help_text),
        "# TYPE %s histogram" % name,
    ]
    cumulative = 0
    for upper, count in zip(buckets, bucket_counts):
        cumulative += count
        lines.append('%s_bucket{le="%s"} %d' % (name, repr(upper), cumulative))
    cumulative += bucket_counts[-1]
    lines.append('%s_bucket{le="+Inf"} %d' % (name, cumulative))
    lines.append("%s_sum %s" % (name, total_sum))
    lines.append("%s_count %d" % (name, total_count))
    return lines


def _gauge_or_counter(kind, name, help_text, value):
    return [
        "# HELP %s %s" % (name, help_text),
        "# TYPE %s %s" % (name, kind),
        "%s %s" % (name, value),
    ]


def render():
    with _lock:
        s = dict(_state)

    lines = []
    lines += _gauge_or_counter(
        "gauge", "ninfer_exporter_tailing",
        "1 once --request-log-jsonl exists and is being tailed, 0 before the file appears.",
        s["tailing"])

    lines += _histogram(
        "ninfer_request_ttft_seconds",
        "Time to first token per completed request (request_done.timings_seconds.ttft).",
        TTFT_BUCKETS, s["ttft_bucket_counts"], s["ttft_sum"], s["ttft_count"])
    lines += _histogram(
        "ninfer_request_decode_seconds",
        "Whole-request decode-phase duration (request_done.timings_seconds.decode). "
        "Not per-token -- ninfer has no native inter-token-latency field.",
        DECODE_BUCKETS, s["decode_bucket_counts"], s["decode_sum"], s["decode_count"])

    for name, key, help_text in (
        ("ninfer_prompt_tokens_total", "prompt_tokens_total",
         "Cumulative prompt tokens across completed requests."),
        ("ninfer_generation_tokens_total", "generation_tokens_total",
         "Cumulative completion tokens across completed requests."),
        ("ninfer_prefix_cache_hit_tokens_total", "prefix_cache_hit_tokens_total",
         "Cumulative prefix-cache-hit prompt tokens across completed requests."),
        ("ninfer_requests_done_total", "requests_done_total", "Completed requests."),
        ("ninfer_requests_error_total", "requests_error_total",
         "Requests that ended in request_error."),
        ("ninfer_requests_rejected_total", "requests_rejected_total",
         "Requests rejected at admission (request_rejected)."),
    ):
        lines += _gauge_or_counter("counter", name, help_text, s[key])

    for name, key, help_text in (
        ("ninfer_num_requests_running", "running",
         "In-flight active requests (throughput.scheduler.running)."),
        ("ninfer_num_requests_waiting", "waiting",
         "FIFO-queued waiting requests (throughput.scheduler.waiting)."),
        ("ninfer_num_requests_prefilling", "prefilling",
         "Requests in staged prefill (throughput.scheduler.prefilling)."),
        ("ninfer_num_requests_decode_ready", "decode_ready",
         "Requests ready for the next decode round (throughput.scheduler.decode_ready)."),
        ("ninfer_kv_cache_device_main_pages", "device_main_kv_pages",
         "Occupied Device main-KV pages (throughput.context_cache.occupancy.device_main_kv_pages). "
         "Raw page count, not a percentage."),
        ("ninfer_kv_cache_device_backend_pages", "device_backend_kv_pages",
         "Occupied Device backend-KV pages (speculative-decoding draft-head KV)."),
        ("ninfer_kv_cache_host_bytes", "host_kv_bytes",
         "Occupied Host-resident KV bytes (offloaded continuations)."),
    ):
        lines += _gauge_or_counter("gauge", name, help_text, s[key])

    if s["kv_capacity_page_groups"] is not None:
        lines += _gauge_or_counter(
            "gauge", "ninfer_kv_capacity_page_groups",
            "Configured KV capacity in page groups, logged once at server_start.",
            s["kv_capacity_page_groups"])

    return ("\n".join(lines) + "\n").encode()


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/metrics":
            body = render()
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; version=0.0.4")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, *args):
        pass


def main():
    threading.Thread(target=_tail_loop, daemon=True).start()
    HTTPServer(("0.0.0.0", LISTEN_PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
