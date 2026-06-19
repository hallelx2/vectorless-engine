#!/usr/bin/env python3
"""
Local viewer for the vectorless-engine.

Serves the single-page viewer (index.html) AND reverse-proxies every
request under /engine/* to the engine on :7654. Same-origin, so the
browser never makes a cross-origin call — no CORS config needed on the
engine (the OSS `engine --local` binary emits no CORS headers).

    python serve.py                  # viewer on http://localhost:7655, engine assumed on :7654
    VIEWER_PORT=8000 ENGINE_URL=http://localhost:7654 python serve.py

This is the minimal local-app shell tracked as HAL-188.
"""
import os
import sys
import urllib.request
import urllib.error
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
ENGINE_URL = os.environ.get("ENGINE_URL", "http://localhost:7654").rstrip("/")
PORT = int(os.environ.get("VIEWER_PORT", "7655"))
# Bind host. Default localhost-only for local dev safety; set HOST=0.0.0.0 to
# expose it (the all-in-one Docker image does this so the mapped port works).
HOST = os.environ.get("HOST", "127.0.0.1")

# Hop-by-hop / host headers we must not forward verbatim.
_SKIP_REQ = {"host", "connection", "content-length", "accept-encoding"}
_SKIP_RESP = {"transfer-encoding", "connection", "content-encoding", "content-length"}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    # ---- static viewer ----
    def _serve_index(self):
        try:
            with open(os.path.join(HERE, "index.html"), "rb") as f:
                body = f.read()
        except FileNotFoundError:
            self.send_error(404, "index.html not found next to serve.py")
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    # ---- reverse proxy to the engine ----
    def _proxy(self, method):
        target = ENGINE_URL + self.path[len("/engine"):]
        length = int(self.headers.get("Content-Length", 0) or 0)
        body = self.rfile.read(length) if length else None

        req = urllib.request.Request(target, data=body, method=method)
        for k, v in self.headers.items():
            if k.lower() not in _SKIP_REQ:
                req.add_header(k, v)

        try:
            resp = urllib.request.urlopen(req, timeout=300)
            data = resp.read()
            status = resp.status
            headers = resp.getheaders()
        except urllib.error.HTTPError as e:
            data = e.read()
            status = e.code
            headers = list(e.headers.items())
        except urllib.error.URLError as e:
            msg = f'{{"error":"cannot reach engine at {ENGINE_URL}: {e.reason}"}}'.encode()
            self.send_response(502)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(msg)))
            self.end_headers()
            self.wfile.write(msg)
            return

        self.send_response(status)
        sent_ct = False
        for k, v in headers:
            if k.lower() in _SKIP_RESP:
                continue
            if k.lower() == "content-type":
                sent_ct = True
            self.send_header(k, v)
        if not sent_ct:
            self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    _CT = {".html": "text/html; charset=utf-8", ".svg": "image/svg+xml",
           ".css": "text/css", ".js": "text/javascript", ".ico": "image/x-icon",
           ".png": "image/png"}

    def _serve_static(self, path):
        rel = path.lstrip("/") or "index.html"
        # contain to this directory — no traversal
        full = os.path.normpath(os.path.join(HERE, rel))
        if not full.startswith(HERE) or not os.path.isfile(full):
            self.send_error(404)
            return
        with open(full, "rb") as f:
            body = f.read()
        ext = os.path.splitext(full)[1].lower()
        self.send_response(200)
        self.send_header("Content-Type", self._CT.get(ext, "application/octet-stream"))
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        if self.path.startswith("/engine/"):
            self._proxy("GET")
        else:
            self._serve_static(path)

    def do_POST(self):
        if self.path.startswith("/engine/"):
            self._proxy("POST")
        else:
            self.send_error(404)

    def do_DELETE(self):
        if self.path.startswith("/engine/"):
            self._proxy("DELETE")
        else:
            self.send_error(404)

    def log_message(self, *a):  # quiet
        pass


if __name__ == "__main__":
    print(f"Vectorless local viewer -> http://localhost:{PORT}  (bind {HOST}:{PORT})")
    print(f"Proxying /engine/* -> {ENGINE_URL}")
    try:
        ThreadingHTTPServer((HOST, PORT), Handler).serve_forever()
    except KeyboardInterrupt:
        sys.exit(0)
