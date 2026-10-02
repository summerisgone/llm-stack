"""Tests for agent_pipe.py against a fake broker. Needs httpx and pydantic
(both in the Open WebUI image):
uv run --no-project --with httpx --with pydantic python3 -m unittest discover -s config/openwebui
"""
import asyncio
import json
import os
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

try:
    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    from agent_pipe import Pipe
except ImportError as ex:  # httpx / pydantic missing
    Pipe = None
    MISSING = str(ex)


class Broker(BaseHTTPRequestHandler):
    events = []
    seen = []

    def log_message(self, *args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        Broker.seen.append((self.path, self.headers.get("Authorization"), body))
        if self.path == "/v1/agent/permissions":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b'{"ok":true}')
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/x-ndjson")
        self.end_headers()
        for e in Broker.events:
            self.wfile.write((json.dumps(e) + "\n").encode())
            self.wfile.flush()


@unittest.skipIf(Pipe is None, "httpx/pydantic not installed: %s" % (Pipe is None and MISSING))
class PipeTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = ThreadingHTTPServer(("127.0.0.1", 0), Broker)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def run_pipe(self, events, call=None, token="tok", messages=None, files=None):
        Broker.events, Broker.seen = events, []
        p = Pipe()
        p.valves.BROKER_URL = "http://127.0.0.1:%d" % self.srv.server_address[1]
        statuses = []

        async def emit(e):
            statuses.append(e["data"]["description"])

        async def go():
            body = {"model": "agent_pipe.dsh-agent", "messages": messages or [
                {"role": "system", "content": "s"}, {"role": "user", "content": "a"},
                {"role": "assistant", "content": "x"}, {"role": "user", "content": [{"type": "text", "text": "b"}]}]}
            gen = await p.pipe(body, __metadata__={"chat_id": "c1"}, __oauth_token__={"access_token": token} if token else None,
                               __event_emitter__=emit, __event_call__=call, __files__=files)
            return [x async for x in gen]

        return asyncio.run(go()), statuses

    def test_turn_with_approved_permission(self):
        asked = []

        async def call(e):
            asked.append(e)
            return True

        out, statuses = self.run_pipe([
            {"type": "turn", "turn": "t1"},
            {"type": "reasoning", "text": "hm"},
            {"type": "tool", "id": "1", "title": "ls", "status": "pending"},
            {"type": "permission", "turn": "t1", "request": "1", "title": "ls", "text": "ls -la"},
            {"type": "tool", "id": "1", "status": "completed", "text": "a.txt"},
            {"type": "text", "text": "done"},
            {"type": "done", "stop": "end_turn"},
        ], call=call)
        path, authz, req = Broker.seen[0]
        self.assertEqual((path, authz), ("/v1/agent/turns", "Bearer tok"))
        self.assertEqual(req, {"model": "dsh-agent", "chat_id": "c1", "users": ["a", "b"]})
        self.assertEqual(Broker.seen[1][2], {"model": "dsh-agent", "turn": "t1", "request": "1", "allow": True})
        self.assertEqual(asked[0]["type"], "confirmation")
        self.assertEqual(out[0], {"choices": [{"index": 0, "delta": {"reasoning_content": "hm"}}]})
        self.assertIn("<summary>ls (completed)</summary>", out[1])
        self.assertIn("a.txt", out[1])
        self.assertEqual(out[2], "done")
        self.assertIn("Approved: ls", statuses)

    def test_no_browser_denies(self):
        out, _ = self.run_pipe([
            {"type": "permission", "turn": "t1", "request": "1", "title": "rm"},
            {"type": "done", "stop": "cancelled"},
        ])
        self.assertFalse(Broker.seen[1][2]["allow"])
        self.assertEqual(out, ["\n\n_Turn ended: cancelled._"])

    def test_browser_error_denies(self):
        async def call(e):
            return {"error": "Client session disconnected."}

        self.run_pipe([{"type": "permission", "turn": "t1", "request": "1", "title": "rm"}, {"type": "done"}], call=call)
        self.assertFalse(Broker.seen[1][2]["allow"])

    def test_failures_are_errors(self):
        out, _ = self.run_pipe([{"type": "text", "text": "partial"}])
        self.assertEqual(out[0], "partial")
        self.assertIn("outcome is unknown", out[1]["error"]["detail"])
        out, _ = self.run_pipe([{"type": "error", "message": "boom"}])
        self.assertEqual(out, [{"error": {"detail": "boom"}}])
        out, _ = self.run_pipe([], token=None)
        self.assertIn("sign in again", out[0]["error"]["detail"])
        self.assertEqual(Broker.seen, [])
        out, _ = self.run_pipe([], files=[{"id": "f"}])
        self.assertIn("text only", out[0]["error"]["detail"])
        self.assertEqual(Broker.seen, [])

    def test_models(self):
        p = Pipe()
        p.valves.MODELS = "dsh-agent=DeepSeek agent, opencode-agent"
        self.assertEqual([m["id"] for m in p.pipes()], ["dsh-agent", "opencode-agent"])


if __name__ == "__main__":
    unittest.main()
