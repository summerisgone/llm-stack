"""
title: Personal agents (interactive)
description: Agent chats through agent-broker's interaction protocol (docs/adr/0021): tool progress, permission prompts, Stop.
version: 0.1.0
"""

# Installed and updated by scripts/openwebui-agent-pipe; do not edit in the
# UI. Open WebUI v0.11.4 injects __oauth_token__ (the user's Keycloak
# session) and __event_call__ (a browser round trip) into a Pipe. Identity
# reaches the broker only as that token; nothing from __user__ is sent.
import asyncio
import html
import json

import httpx
from pydantic import BaseModel, Field


def _text(content):
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "".join(p.get("text", "") for p in content if p.get("type") == "text")
    return ""


def _has_attachment(content):
    return isinstance(content, list) and any(p.get("type") != "text" for p in content)


class Pipe:
    class Valves(BaseModel):
        BROKER_URL: str = Field(default="http://agent-broker.agents.svc.cluster.local:8080")
        MODELS: str = Field(
            default="dsh-agent=DeepSeek agent",
            description="Comma-separated broker model id=title; each needs AGENT_PIPE_RUNTIMES in the broker.",
        )
        PERMISSION_TIMEOUT: int = Field(
            default=270, description="Seconds to wait for the user's answer; then the request is denied."
        )

    def __init__(self):
        self.valves = self.Valves()

    def pipes(self):
        out = []
        for item in self.valves.MODELS.split(","):
            model, _, title = item.strip().partition("=")
            if model:
                out.append({"id": model, "name": f"{title or model} (personal, interactive)"})
        return out

    async def pipe(self, body, __metadata__=None, __oauth_token__=None, __event_emitter__=None,
                   __event_call__=None, __files__=None, __task__=None):
        if __task__:
            return ""
        return self._turn(body, __metadata__ or {}, __oauth_token__, __event_emitter__, __event_call__, __files__)

    async def _turn(self, body, metadata, oauth_token, emit, call, files):
        async def status(text, done=False):
            if emit:
                await emit({"type": "status", "data": {"description": text, "done": done}})

        def error(msg):
            return {"error": {"detail": msg}}

        token = (oauth_token or {}).get("access_token")
        if not token:
            yield error("No Keycloak session for this user; sign out of Open WebUI and sign in again.")
            return
        messages = [m for m in body.get("messages", []) if m.get("role") == "user"]
        if files or (messages and _has_attachment(messages[-1].get("content"))):
            yield error("Agent chats accept text only; attachments are not passed to the agent.")
            return
        model = body.get("model", "").split(".", 1)[-1]
        headers = {"Authorization": f"Bearer {token}"}
        broker = self.valves.BROKER_URL.rstrip("/")
        req = {"model": model, "chat_id": metadata.get("chat_id"), "users": [_text(m.get("content")) for m in messages]}
        tools = {}
        ended = False
        timeout = httpx.Timeout(10.0, read=None)
        try:
            async with httpx.AsyncClient(timeout=timeout) as client:
                async with client.stream("POST", f"{broker}/v1/agent/turns", json=req, headers=headers) as res:
                    if res.status_code != 200:
                        raw = await res.aread()
                        try:
                            msg = json.loads(raw)["error"]["message"]
                        except Exception:
                            msg = raw.decode("utf-8", "replace")[:500]
                        yield error(f"Agent broker answered {res.status_code}: {msg}")
                        return
                    async for line in res.aiter_lines():
                        if not line.strip():
                            continue
                        e = json.loads(line)
                        kind = e.get("type")
                        if kind == "status":
                            await status(e.get("text", ""))
                        elif kind == "notice":
                            yield e.get("text", "") + "\n\n"
                        elif kind == "text":
                            yield e.get("text", "")
                        elif kind == "reasoning":
                            yield {"choices": [{"index": 0, "delta": {"reasoning_content": e.get("text", "")}}]}
                        elif kind == "tool":
                            t = tools.setdefault(e.get("id"), {})
                            t.update({k: v for k, v in e.items() if v})
                            title = t.get("title") or "tool"
                            st = t.get("status") or "pending"
                            await status(f"{title}: {st}")
                            if st in ("completed", "failed"):
                                yield self._details(title, st, t.get("text", ""))
                        elif kind == "permission":
                            await status(f"Waiting for your approval: {e.get('title')}")
                            allow = await self._ask(call, e)
                            await client.post(
                                f"{broker}/v1/agent/permissions", headers=headers,
                                json={"model": model, "turn": e.get("turn"), "request": e.get("request"), "allow": allow},
                            )
                            await status(("Approved: " if allow else "Denied: ") + str(e.get("title")))
                        elif kind == "done":
                            ended = True
                            stop = e.get("stop")
                            if stop and stop != "end_turn":
                                yield f"\n\n_Turn ended: {stop}._"
                            await status("", done=True)
                        elif kind == "error":
                            ended = True
                            await status("", done=True)
                            yield error(e.get("message", "agent error"))
        except httpx.HTTPError as ex:
            yield error(f"Agent broker unreachable: {type(ex).__name__}: {ex!r}")
            return
        if not ended:
            yield error("The agent connection ended before the turn finished; its outcome is unknown.")

    async def _ask(self, call, e):
        # No browser (API client, closed tab), timeout or anything but an
        # explicit yes is a denial.
        if call is None:
            return False
        text = (e.get("text") or "")[:2000]
        try:
            answer = await asyncio.wait_for(
                call({"type": "confirmation", "data": {
                    "title": f"Allow the agent to run: {e.get('title')}?",
                    "message": text or "The agent asks for permission to run this tool.",
                }}),
                timeout=self.valves.PERMISSION_TIMEOUT,
            )
        except asyncio.TimeoutError:
            return False
        return answer is True

    @staticmethod
    def _details(title, status, text):
        summary = html.escape(f"{title} ({status})")
        body = f"\n\n````\n{text[:4000]}\n````\n" if text else "\n"
        return f"\n\n<details>\n<summary>{summary}</summary>{body}</details>\n\n"
