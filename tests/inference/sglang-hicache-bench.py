#!/usr/bin/env python3
"""Small, dependency-free SGLang benchmark for the HiCache experiment."""

import argparse
import concurrent.futures
import json
import statistics
import threading
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path


def request(base_url, payload, api_key, stream=False, timeout=900):
    headers = {"Content-Type": "application/json"}
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"
    request_obj = urllib.request.Request(
        base_url + "/v1/chat/completions",
        data=json.dumps(payload).encode("utf-8"), method="POST", headers=headers,
    )
    started = time.perf_counter()
    first_token = None
    token_times = []
    text = ""
    try:
        with urllib.request.urlopen(request_obj, timeout=timeout) as response:
            if not stream:
                body = json.loads(response.read())
                elapsed = time.perf_counter() - started
                choice = body.get("choices", [{}])[0]
                return {
                    "http_status": response.status, "elapsed_s": elapsed,
                    "prompt_tokens": body.get("usage", {}).get("prompt_tokens"),
                    "completion_tokens": body.get("usage", {}).get("completion_tokens"),
                    "text": choice.get("message", {}).get("content", ""),
                }
            for raw_line in response:
                if not raw_line.startswith(b"data: "):
                    continue
                data = raw_line[6:].strip()
                if data == b"[DONE]":
                    break
                event = json.loads(data)
                delta = event.get("choices", [{}])[0].get("delta", {}).get("content")
                if delta:
                    now = time.perf_counter()
                    first_token = first_token or now
                    token_times.append(now)
                    text += delta
        elapsed = time.perf_counter() - started
        intervals = [b - a for a, b in zip(token_times, token_times[1:])]
        return {
            "http_status": 200, "elapsed_s": elapsed,
            "ttft_s": (first_token - started) if first_token else None,
            "completion_chunks": len(token_times), "text": text,
            "median_tpot_s": statistics.median(intervals) if intervals else None,
            "p95_tpot_s": percentile(intervals, 95) if intervals else None,
            "decode_chunks_per_s": len(token_times) / (token_times[-1] - first_token)
            if len(token_times) > 1 else None,
        }
    except urllib.error.HTTPError as exc:
        return {"http_status": exc.code, "elapsed_s": time.perf_counter() - started,
                "error": exc.read().decode("utf-8", errors="replace")[:2000]}
    except Exception as exc:  # diagnostic harness must preserve the error
        return {"http_status": None, "elapsed_s": time.perf_counter() - started,
                "error": repr(exc)}


def percentile(values, point):
    if not values:
        return None
    ordered = sorted(values)
    return ordered[min(len(ordered) - 1, round((len(ordered) - 1) * point / 100))]


def prompt(words, marker):
    # The nonce is the first token-bearing material: sessions cannot share a
    # common prefix even though the filler word is deliberately inexpensive.
    nonce = uuid.uuid4().hex
    return (f"dialogue-nonce-{nonce} {marker} " + ("apple " * words)
            + f"\nВ конце этой независимой истории маркер {marker}. Ответь только маркером.")


def payload(content, output, stream):
    return {
        "model": "qwen38-nvfp4", "messages": [{"role": "user", "content": content}],
        "max_tokens": output, "temperature": 0.0, "stream": stream,
        "chat_template_kwargs": {"enable_thinking": False},
        "ignore_eos": stream,
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", default="http://127.0.0.1:30001")
    parser.add_argument("--case", choices=("smoke", "decode", "long", "concurrent"), required=True)
    parser.add_argument("--words", type=int, default=0)
    parser.add_argument("--requests", type=int, default=1)
    parser.add_argument("--output", type=int, default=256)
    parser.add_argument("--api-key-file", default="/home/llmstack/.config/sglang/api_key")
    args = parser.parse_args()
    api_key = Path(args.api_key_file).read_text(encoding="utf-8").strip()
    if args.case == "smoke":
        result = request(args.base_url, payload("Ответь ровно словом READY.", 8, False), api_key)
        result["marker_ok"] = result.get("text", "").strip() == "READY"
        print(json.dumps(result, ensure_ascii=False))
        raise SystemExit(0 if result.get("http_status") == 200 and result["marker_ok"] else 1)
    if args.case == "decode":
        result = request(args.base_url, payload(
            "Напиши технический текст о KV cache. Не заканчивай ответ раньше лимита.",
            args.output, True), api_key, stream=True)
        print(json.dumps(result, ensure_ascii=False))
        raise SystemExit(0 if result.get("http_status") == 200 else 1)

    barrier = threading.Barrier(args.requests)
    def one(index):
        marker = f"MARKER-{index}-{uuid.uuid4().hex[:12]}"
        barrier.wait()
        return request(args.base_url, payload(prompt(args.words, marker), args.output,
                                              args.case == "concurrent"),
                       api_key, stream=args.case == "concurrent") | {"marker": marker}

    started = time.perf_counter()
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.requests) as executor:
        results = list(executor.map(one, range(args.requests)))
    for item in results:
        item["marker_ok"] = item["marker"] in item.get("text", "")
    print(json.dumps({"case": args.case, "words": args.words, "requests": args.requests,
                      "output": args.output, "wall_s": time.perf_counter() - started,
                      "results": results}, ensure_ascii=False))
    raise SystemExit(0 if all(
        item.get("http_status") == 200 and item["marker_ok"] for item in results
    ) else 1)


if __name__ == "__main__":
    main()
