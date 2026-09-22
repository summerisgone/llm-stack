# Authoring an Open WebUI tool

A tool is one Python module, stored in the database and executed inside the Open WebUI container.
This file covers the module contract, the patterns that survive contact with a real deployment, and a
knowledge-upload helper worth copying verbatim.

## Module contract

```python
"""
title: My Collector
description: One line - this is what the model sees when deciding to call the tool
version: 0.1.0
requirements: trafilatura, beautifulsoup4, lxml
"""

import asyncio
from pydantic import BaseModel, Field


class Tools:
    class Valves(BaseModel):
        OWUI_URL: str = Field("http://localhost:8080", description="Open WebUI from inside the container")
        API_KEY: str = Field("", description="API key of the user owning the knowledge base")
        KNOWLEDGE_ID: str = Field("", description="Target collection")

    def __init__(self):
        self.valves = self.Valves()

    async def do_the_thing(self, __event_emitter__=None) -> str:
        """
        What this does, in the language the model should reason about.
        This docstring becomes the function description in the tool spec - write it for the model.
        """
        return "result the model will read"
```

What matters here:

- **The docstring is the API.** It is what lands in the tool spec and drives whether the model calls it.
  Method parameters become the JSON schema, so keep them few and obvious.
- **Valves are the config surface.** Anything environment-specific - URLs, ids, thresholds, keyword lists -
  belongs in a Valve so it can be changed through the API without redeploying source. Defaults should be
  sane enough that a missing valve does not produce silent nonsense; check the required ones up front and
  return a clear message instead of throwing.
- **`__event_emitter__` is progress reporting.** Each emit shows up in the chat's `statusHistory`, which is
  often the only forensic trail you get for a run.
- **The return value is what the model reads.** Return structured text with the facts it needs to
  summarize - counts, identifiers, the classification you already computed. Do not make it re-derive
  things the script knows.

```python
async def status(msg, done=False):
    if __event_emitter__:
        await __event_emitter__({"type": "status", "data": {"description": msg, "done": done}})
```

`self.valves.X` is read at call time, so a valve change takes effect on the next run with no redeploy.

## Container realities

The module runs in the Open WebUI pod, with its packages and its network. Verify both before designing:

```bash
kubectl -n <ns> exec <pod> -- python3 -c "import bs4, trafilatura, httpx, lxml; print('ok')"
kubectl -n <ns> exec <pod> -- sh -c 'for h in example.com api.example.org; do
  printf "%-28s " "$h"; curl -4 -sS -m 12 -o /dev/null -w "%{http_code}\n" "https://$h" || echo FAIL; done'
```

Egress is frequently partial - a host that resolves may still be unreachable, and `-4` vs default can
differ when only an AAAA record exists. `ConnectTimeout` with a working `getent` means packets are being
dropped, which is a firewall question, not a code question. Test the specific hosts your tool needs
before writing the client for them.

State that must survive between runs goes in SQLite under `/app/backend/data/` - that path is the
container's persistent volume. A `seen` table keyed by external id is the usual shape for a collector.

## Dry-run before deploying

Parsing and filtering logic is worth exercising in the pod before it ever becomes a tool version. Pipe a
harness that appends test code to the module source and prints what matched:

```bash
{ cat my_tool.py; cat <<'PY'

import httpx as _h
t = Tools()
c = _h.Client(timeout=30, follow_redirects=True, headers={"User-Agent": "Mozilla/5.0"})
rows = t._parse_listing(c.get("https://example.com/list").text)
print("parsed:", len(rows))
for r in rows[:5]:
    print(" ", r["id"], r["title"][:60], "| topic:", t._topic(r))
PY
} | kubectl -n <ns> exec -i <pod> -- python3 -
```

This runs with the real packages against the real network from the real host, and costs seconds. It is
much faster than discovering a selector bug through an automation run and a paraphrased error message.

## Make failures legible

Several httpx exceptions stringify to nothing - `ConnectTimeout('')`. An error report built with
`str(ex)` then reads as `url: ` and tells you nothing at all.

```python
except Exception as ex:
    errors.append(f"{url}: {type(ex).__name__}: {ex!r}")
```

Return the error list in the tool's output, capped. The model will paraphrase it, so when you actually
need the raw text, run the tool with a prompt that demands verbatim output and no summarizing.

Avoid blanket `except: return None` in helpers - a swallowed exception in a fetch helper turns a network
diagnosis into guesswork. Catch where you can report.

## Writing into a knowledge base

The three-step ingestion with its mandatory wait, as a helper:

```python
async def _upload(self, client, filename: str, text: str) -> None:
    base = self.valves.OWUI_URL.rstrip("/")
    h = {"Authorization": f"Bearer {self.valves.API_KEY}"}
    r = await client.post(
        f"{base}/api/v1/files/", headers=h,
        files={"file": (filename, text.encode("utf-8"), "text/markdown")},
    )
    r.raise_for_status()
    fid = r.json()["id"]
    for _ in range(90):  # processing is async; file/add returns 400 until it completes
        st = (await client.get(f"{base}/api/v1/files/{fid}/process/status", headers=h)).json().get("status")
        if st == "completed":
            break
        if st == "failed":
            raise RuntimeError(f"processing failed: {filename}")
        await asyncio.sleep(2)
    else:
        raise TimeoutError(f"processing timeout: {filename}")
    r = await client.post(
        f"{base}/api/v1/knowledge/{self.valves.KNOWLEDGE_ID}/file/add",
        headers=h, json={"file_id": fid},
    )
    r.raise_for_status()
```

`OWUI_URL` is `http://localhost:8080` from inside the container - the tool talks to its own process, so
this path needs no gateway and no upstream credential. That is why collection keeps working even when
inference is broken.

Put classification into the document itself:

```markdown
# Title

- Topic: Small models
- Date: 2026-09-20 14:32 UTC
- Source: <url>
- Score: 513
```

A later summarizing prompt can then sort by a fact rather than re-inferring the category from the title,
which it will do differently on each run.

## Deploy and iterate

```bash
python3 - <<'EOF'
import json
json.dump({"id": "my_tool", "name": "My Tool",
           "meta": {"description": "...", "manifest": {"title": "My Tool", "version": "0.1.0",
                                                       "requirements": "trafilatura"}},
           "content": open("my_tool.py").read()},
          open("/tmp/tool.json", "w"), ensure_ascii=False)
EOF
curl -s -X POST "$BASE/api/v1/tools/id/my_tool/update" -H "Authorization: Bearer $KEY" \
     -H "Content-Type: application/json" -d @/tmp/tool.json -o /dev/null -w "%{http_code}\n"
```

Then push valves separately (the full set - it replaces), and fire the automation that uses the tool.
Tools do not execute from `/api/chat/completions`; that endpoint only returns the model's intended
`tool_calls`. The automation is the way to run one headlessly.

Verify by state: knowledge `file_count`, the assistant message's `content`, and the tool's own
`statusHistory` entries. The run's `status` field will say `success` either way.
