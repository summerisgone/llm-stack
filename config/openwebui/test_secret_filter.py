"""Tests for secret_filter.py with a fake chat store. Needs pydantic:
uv run --no-project --with httpx --with pydantic python3 -m unittest discover -s config/openwebui
"""
import asyncio
import os
import sys
import types
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))


class FakeChats:
    def __init__(self, messages):
        self.messages = messages
        self.writes = []

    async def get_messages_map_by_chat_id(self, chat_id):
        return self.messages

    async def upsert_message_to_chat_by_id_and_message_id(self, chat_id, message_id, message, touch=True):
        self.writes.append((chat_id, message_id, message))
        self.messages[message_id] = {**self.messages[message_id], **message}


def install(chats):
    mod = types.ModuleType("open_webui.models.chats")
    mod.Chats = chats
    sys.modules["open_webui"] = types.ModuleType("open_webui")
    sys.modules["open_webui.models"] = types.ModuleType("open_webui.models")
    sys.modules["open_webui.models.chats"] = mod


import secret_filter  # noqa: E402

TOKEN = "github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz"


class SecretFilterTest(unittest.TestCase):
    def test_inlet_redacts_request_and_stored_message(self):
        chats = FakeChats({"u1": {"role": "user", "content": "use " + TOKEN}, "a1": {"role": "assistant", "content": ""}})
        install(chats)
        events = []

        async def emit(e):
            events.append(e)

        body = {"messages": [{"role": "user", "content": "use " + TOKEN}]}
        out = asyncio.run(secret_filter.Filter().inlet(body, {"chat_id": "c1"}, emit))
        self.assertEqual(out["messages"][0]["content"], "use [REDACTED]")
        self.assertEqual(chats.writes, [("c1", "u1", {"content": "use [REDACTED]"})])
        self.assertEqual(events[0]["data"]["type"], "warning")

    def test_clean_message_touches_nothing(self):
        chats = FakeChats({"u1": {"role": "user", "content": "hello"}})
        install(chats)
        body = {"messages": [{"role": "user", "content": "hello sk-short"}]}
        out = asyncio.run(secret_filter.Filter().inlet(body, {"chat_id": "c1"}))
        self.assertEqual(out["messages"][0]["content"], "hello sk-short")
        self.assertEqual(chats.writes, [])

    def test_multipart_and_private_key(self):
        key = "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\ndef\n-----END OPENSSH PRIVATE KEY-----"
        content, hit = secret_filter.redact([{"type": "text", "text": "k: " + key}, {"type": "image_url", "image_url": {"url": "x"}}])
        self.assertTrue(hit)
        self.assertEqual(content[0]["text"], "k: [REDACTED]")
        self.assertEqual(content[1]["type"], "image_url")

    def test_outlet_redacts_reply(self):
        chats = FakeChats({"a1": {"role": "assistant", "content": "token ghp_" + "a" * 36}})
        install(chats)
        body = {"chat_id": "c1", "messages": [{"id": "a1", "role": "assistant", "content": "token ghp_" + "a" * 36}]}
        out = asyncio.run(secret_filter.Filter().outlet(body))
        self.assertEqual(out["messages"][0]["content"], "token [REDACTED]")
        self.assertEqual(chats.messages["a1"]["content"], "token [REDACTED]")


if __name__ == "__main__":
    unittest.main()
