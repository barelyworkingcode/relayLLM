#!/usr/bin/env python3
"""Recording reverse proxy for Claude Code request bodies (stdlib only).

Forwards every request unchanged to UPSTREAM and streams the response back
unbuffered (SSE flows). For each POST to /v1/messages (not count_tokens) the
raw request body is appended, one line, to OUT_FILE. Bodies only: headers are
never written anywhere.

Env:  UPSTREAM      required, e.g. http://127.0.0.1:18180
      OUT_FILE      required, JSONL file to append to
      CAPTURE_PORT  default 18990 (binds 127.0.0.1)
Use:  ANTHROPIC_BASE_URL=http://127.0.0.1:18990 claude ...
"""
import http.client
import json
import os
import sys
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HOP = {"connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
       "te", "trailer", "transfer-encoding", "upgrade", "proxy-connection"}
UP = urllib.parse.urlsplit(os.environ.get("UPSTREAM", ""))
OUT_FILE = os.environ.get("OUT_FILE", "")
lock = threading.Lock()


def record(body: bytes) -> None:
    if b"\n" in body or b"\r" in body:
        try:
            body = json.dumps(json.loads(body), separators=(",", ":"), ensure_ascii=False).encode()
            sys.stderr.write("capture: body contained a newline; re-encoded compactly\n")
        except ValueError:
            sys.stderr.write("capture: body is not JSON and has a newline; skipped\n")
            return
    with lock, open(OUT_FILE, "ab") as f:
        f.write(body + b"\n")
    sys.stderr.write(f"capture: recorded body of {len(body)} bytes\n")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("capture: %s %s\n" % (self.command, self.path))

    def read_body(self) -> bytes:
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            out = b""
            while True:
                size = int(self.rfile.readline().split(b";")[0].strip() or b"0", 16)
                if size == 0:
                    while self.rfile.readline().strip():
                        pass
                    return out
                out += self.rfile.read(size)
                self.rfile.readline()
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n else b""

    def handle_any(self):
        body = self.read_body()
        path = urllib.parse.urlsplit(self.path).path
        if (self.command == "POST" and path.startswith("/v1/messages")
                and not path.startswith("/v1/messages/count_tokens")):
            record(body)
        headers = {k: v for k, v in self.headers.items() if k.lower() not in HOP and k.lower() != "host"}
        headers["Content-Length"] = str(len(body))
        cls = http.client.HTTPSConnection if UP.scheme == "https" else http.client.HTTPConnection
        conn = cls(UP.netloc, timeout=900)
        try:
            conn.request(self.command, (UP.path.rstrip("/") + self.path), body=body or None, headers=headers)
            resp = conn.getresponse()
        except Exception as e:  # upstream unreachable
            msg = f"capture: upstream error: {e}".encode()
            self.send_response(502)
            self.send_header("Content-Length", str(len(msg)))
            self.end_headers()
            self.wfile.write(msg)
            return
        self.send_response(resp.status, resp.reason)
        for k, v in resp.getheaders():
            if k.lower() not in HOP and k.lower() != "content-length":
                self.send_header(k, v)
        head_only = self.command == "HEAD" or resp.status in (204, 304)
        if head_only:
            self.send_header("Content-Length", resp.getheader("Content-Length") or "0")
            self.end_headers()
        else:
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            try:
                while True:
                    chunk = resp.read1(65536)
                    if not chunk:
                        break
                    self.wfile.write(b"%x\r\n" % len(chunk) + chunk + b"\r\n")
                    self.wfile.flush()
                self.wfile.write(b"0\r\n\r\n")
                self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                self.close_connection = True
        conn.close()

    do_GET = do_POST = do_PUT = do_DELETE = do_PATCH = do_HEAD = do_OPTIONS = handle_any


def main():
    if not UP.scheme or not UP.netloc or not OUT_FILE:
        sys.exit("UPSTREAM and OUT_FILE are required")
    port = int(os.environ.get("CAPTURE_PORT", "18990"))
    srv = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    srv.daemon_threads = True
    sys.stderr.write(f"capture: listening on 127.0.0.1:{port} -> {os.environ['UPSTREAM']}\n")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
