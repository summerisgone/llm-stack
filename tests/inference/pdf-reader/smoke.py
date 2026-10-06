#!/usr/bin/env python3
"""Exercise Open WebUI Files API extraction, not an independent PDF library."""
import argparse
import json
import os
from pathlib import Path
import re
import socket
import sys
import time
import urllib.error
import uuid

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))
from lib_endpoints import Endpoints  # noqa: E402


def api_key():
    key = os.environ.get("OPENWEBUI_API_KEY")
    if not key and (ROOT / ".env").is_file():
        for line in (ROOT / ".env").read_text().splitlines():
            if line.startswith("OPENWEBUI_API_KEY="):
                key = line.split("=", 1)[1].strip().strip("\"'")
    if not key:
        raise RuntimeError("OPENWEBUI_API_KEY is required (Open WebUI key, not a stack PAT)")
    return key


class Smoke:
    def __init__(self, key, timeout):
        self.ep = Endpoints()
        self.base = os.environ.get("OPENWEBUI_SMOKE_URL", self.ep.webui).rstrip("/")
        self.key = key
        self.timeout = timeout
        self.created = []

    def request(self, method, path, data=None, content_type=None, anonymous=False, raw=False):
        headers = {} if anonymous else {"Authorization": f"Bearer {self.key}"}
        if content_type:
            headers["Content-Type"] = content_type
        with self.ep.open(method, self.base + path, data=data, headers=headers) as response:
            body = response.read()
            if raw:
                if response.headers.get_content_type() != "application/pdf":
                    raise AssertionError("download did not return application/pdf")
                return body
            if response.headers.get_content_type() != "application/json":
                raise AssertionError(f"{method} {path}: expected JSON, possibly got the SPA")
            return json.loads(body)

    def upload(self, name, body):
        boundary = "pdf-smoke-" + uuid.uuid4().hex
        data = (
            f'--{boundary}\r\nContent-Disposition: form-data; name="file"; '
            f'filename="pdf-smoke-{uuid.uuid4().hex}-{name}"\r\n'
            'Content-Type: application/pdf\r\n\r\n'
        ).encode() + body + f"\r\n--{boundary}--\r\n".encode()
        result = self.request("POST", "/api/v1/files/?process=true&process_in_background=true",
                              data, f"multipart/form-data; boundary={boundary}")
        file_id = result.get("id")
        if not isinstance(file_id, str) or not re.fullmatch(r"[A-Za-z0-9_-]+", file_id):
            raise AssertionError("upload did not return a safe file ID")
        self.created.append(file_id)
        return file_id

    def wait(self, file_id):
        deadline = time.monotonic() + self.timeout
        while time.monotonic() < deadline:
            result = self.request("GET", f"/api/v1/files/{file_id}/process/status")
            status = result.get("status")
            if status in ("completed", "failed"):
                return status
            if status not in ("pending", "processing"):
                raise AssertionError(f"unexpected processing status: {status!r}")
            time.sleep(1)
        raise AssertionError(f"processing timeout after {self.timeout}s (file {file_id})")

    def text(self, file_id):
        content = self.request("GET", f"/api/v1/files/{file_id}/data/content").get("content")
        if not isinstance(content, str):
            raise AssertionError("extracted content is not a string")
        return " ".join(content.split())

    def denied(self, path):
        try:
            self.request("GET", path, anonymous=True)
        except urllib.error.HTTPError as exc:
            if exc.code in (401, 403):
                return
            raise
        raise AssertionError("anonymous caller could read a private file")

    def cleanup(self):
        errors = []
        for file_id in self.created:
            try:
                result = self.request("DELETE", f"/api/v1/files/{file_id}")
                if result.get("message") != "File deleted successfully":
                    raise AssertionError("delete not acknowledged")
                try:
                    self.request("GET", f"/api/v1/files/{file_id}")
                except urllib.error.HTTPError as exc:
                    if exc.code != 404:
                        raise
                else:
                    raise AssertionError("file still visible after deletion")
            except Exception as exc:
                errors.append(f"{file_id}: {type(exc).__name__}")
        if errors:
            raise AssertionError("cleanup failed; remove only these smoke files: " + ", ".join(errors))

    def run(self, ocr):
        directory = Path(__file__).resolve().parent
        source = (directory / "text.pdf").read_bytes()
        file_id = self.upload("text.pdf", source)
        if self.wait(file_id) != "completed":
            raise AssertionError("text PDF processing failed (inspect extraction/embedding logs)")
        content = self.text(file_id)
        for expected in ("PDF_READER_PAGE_ONE_7F31", "PDF_READER_PAGE_TWO_9C82",
                         "договор Север", "12345", "invoice North", "Бумага", "17", "2345"):
            if expected not in content:
                raise AssertionError(f"missing extracted text: {expected}")
        if content.index("PDF_READER_PAGE_ONE_7F31") > content.index("PDF_READER_PAGE_TWO_9C82"):
            raise AssertionError("pages extracted in reverse order")
        if self.request("GET", f"/api/v1/files/{file_id}/content", raw=True) != source:
            raise AssertionError("downloaded PDF differs from upload")
        self.denied(f"/api/v1/files/{file_id}/content")
        self.denied(f"/api/v1/files/{file_id}/data/content")
        print("PASS text: two pages, Cyrillic, English, table values, download, anonymous denial", flush=True)

        for name, body in (("corrupt.pdf", b"%PDF-1.7\nnot a PDF\n"),
                           ("locked.pdf", (directory / "locked.pdf").read_bytes())):
            file_id = self.upload(name, body)
            if self.wait(file_id) != "failed":
                raise AssertionError(f"{name}: invalid input reported as successfully processed")
            record = self.request("GET", f"/api/v1/files/{file_id}")
            if not (record.get("data") or {}).get("error"):
                raise AssertionError(f"{name}: no error explaining failed processing")
            if self.text(file_id):
                raise AssertionError(f"{name}: unexpected extracted content")
            print(f"PASS rejected {name} with an explicit processing error", flush=True)

        if ocr:
            file_id = self.upload("scan.pdf", (directory / "scan.pdf").read_bytes())
            if self.wait(file_id) != "completed":
                raise AssertionError("image-only PDF processing failed; OCR is required by --ocr")
            content = self.text(file_id)
            for expected in ("48271", "67890", "Скан документа"):
                if expected not in content:
                    raise AssertionError(f"OCR did not extract {expected!r}")
            print("PASS OCR: image-only page, English digits and Cyrillic", flush=True)
        else:
            print("NOT TESTED OCR (run with --ocr); this result covers text PDFs only", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ocr", action="store_true", help="require extraction from an image-only scan")
    parser.add_argument("--timeout", type=int, default=120, help="processing deadline per file in seconds")
    args = parser.parse_args()
    if args.timeout <= 0:
        parser.error("--timeout must be positive")
    socket.setdefaulttimeout(min(20, args.timeout))
    smoke = Smoke(api_key(), args.timeout)
    try:
        smoke.run(args.ocr)
    finally:
        smoke.cleanup()
    print("PASS PDF smoke; all created files deleted")


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        # Do not dump response bodies or headers containing credentials.
        print(f"FAIL PDF smoke: {exc}", file=sys.stderr)
        sys.exit(1)
