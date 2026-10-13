"""
title: Secret redaction
description: Masks credentials in chat messages before the model sees them and in the stored chat (docs/adr/0024 decision 8)
version: 0.1.0
"""

# Global Open WebUI Filter, installed by `make openwebui-secret-filter`.
#
# Open WebUI stores the user message before filters run (main.py:
# insert_chat_turn), so redacting the request alone would leave the secret in
# the chat DB. inlet therefore also rewrites every stored message that
# matches; outlet does the same for the reply and returns redacted messages,
# which the browser writes over its own copy. Pattern-based, best effort.

import re

from pydantic import BaseModel, Field

PATTERNS = [
    r"gh[pousr]_[A-Za-z0-9]{20,}",
    r"github_pat_[A-Za-z0-9_]{20,}",
    r"glpat-[A-Za-z0-9_-]{20,}",
    r"sk-[A-Za-z0-9_-]{20,}",
    r"xox[abpr]-[A-Za-z0-9-]{10,}",
    r"AKIA[0-9A-Z]{16}",
    r"-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----",
]
SECRET = re.compile("|".join(f"(?:{p})" for p in PATTERNS))
MASK = "[REDACTED]"


def redact(value):
    """Returns (redacted value, whether anything matched) for message content,
    which is a string or a list of parts."""
    if isinstance(value, str):
        out, n = SECRET.subn(MASK, value)
        return out, n > 0
    if isinstance(value, list):
        hit = False
        parts = []
        for part in value:
            if isinstance(part, dict) and isinstance(part.get("text"), str):
                text, h = redact(part["text"])
                part = {**part, "text": text}
                hit = hit or h
            parts.append(part)
        return parts, hit
    return value, False


async def redact_stored(chat_id):
    """Rewrites the stored copy of every message in the chat that matches."""
    if not chat_id or chat_id.startswith("local:"):
        return
    from open_webui.models.chats import Chats

    messages = await Chats.get_messages_map_by_chat_id(chat_id) or {}
    for message_id, message in messages.items():
        content, hit = redact(message.get("content"))
        if hit:
            await Chats.upsert_message_to_chat_by_id_and_message_id(chat_id, message_id, {"content": content}, touch=False)


class Filter:
    class Valves(BaseModel):
        priority: int = Field(default=-100, description="Runs before every other filter.")

    def __init__(self):
        self.valves = self.Valves()

    async def inlet(self, body: dict, __metadata__: dict = None, __event_emitter__=None) -> dict:
        hit = False
        for message in body.get("messages", []):
            content, h = redact(message.get("content"))
            if h:
                message["content"] = content
                hit = True
        if hit:
            await redact_stored((__metadata__ or {}).get("chat_id"))
            if __event_emitter__:
                await __event_emitter__({"type": "notification", "data": {"type": "warning", "content": "A credential in your message was masked and not sent to the model. Store credentials for agents under Connections in the platform dashboard instead."}})
        return body

    async def outlet(self, body: dict, __metadata__: dict = None) -> dict:
        hit = False
        for message in body.get("messages", []):
            content, h = redact(message.get("content"))
            if h:
                message["content"] = content
                hit = True
        if hit:
            await redact_stored(body.get("chat_id") or (__metadata__ or {}).get("chat_id"))
        return body
