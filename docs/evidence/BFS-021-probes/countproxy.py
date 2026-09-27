#!/usr/bin/env python3
"""countproxy.py — a logging reverse proxy for the bunker WebDAV surface.

Why this exists. BFS-012's truncate diagnostic measured something that looks
contradictory: a single `os.truncate()` through the mount recorded a REFUSED write
(conflicts.jsonl grew, code=hash_mismatch) AND the server's file was truncated by the
same call, with no error reaching the caller. Two explanations are possible — the
kernel/daemon issuing two SETATTRs (the second carrying the base the refusal had just
corrected), or one request that both refused and wrote. Which one it is changes the
finding from "an odd count" to "a refusal that does not stop the mutation", so it is
measured rather than argued.

This proxy sits between the mount and the repo's own surface and writes one JSON line
per HTTP request (method, path, status, conditional headers, body sizes). It adds no
behaviour: it forwards everything verbatim, including WebDAV verbs and bodies.

usage: countproxy.py --upstream http://127.0.0.1:PORT --listen-port P --log FILE
"""
from __future__ import annotations

import argparse
import http.client
import json
import socketserver
import sys
import threading
from http.server import BaseHTTPRequestHandler
from urllib.parse import urlsplit

UPSTREAM = None
LOGFILE = None
LOCK = threading.Lock()
SEQ = 0


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_args):  # silence the default stderr logging
        return

    def _forward(self) -> None:
        global SEQ
        up = urlsplit(UPSTREAM)
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else None
        conn = http.client.HTTPConnection(up.hostname, up.port, timeout=60)
        headers = {k: v for k, v in self.headers.items() if k.lower() != "host"}
        conn.request(self.command, self.path, body=body, headers=headers)
        resp = conn.getresponse()
        payload = resp.read()
        with LOCK:
            SEQ += 1
            rec = {
                "n": SEQ,
                "method": self.command,
                "path": self.path,
                "status": resp.status,
                "if_match": (self.headers.get("If-Match") or "")[:80],
                "if_none_match": (self.headers.get("If-None-Match") or "")[:40],
                "x_bunker_op": self.headers.get("X-Bunker-Op") or "",
                "x_bunker_hash": (self.headers.get("X-Bunker-Hash") or "")[:80],
                "req_bytes": len(body) if body else 0,
                "resp_bytes": len(payload),
            }
            with open(LOGFILE, "a") as fh:
                fh.write(json.dumps(rec) + "\n")
        self.send_response(resp.status)
        for k, v in resp.getheaders():
            if k.lower() in ("transfer-encoding", "connection", "content-length"):
                continue
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)
        conn.close()

    do_GET = do_HEAD = do_PUT = do_POST = do_DELETE = do_OPTIONS = _forward
    do_PROPFIND = do_PROPPATCH = do_MKCOL = do_COPY = do_MOVE = do_LOCK = _forward
    do_UNLOCK = do_REPORT = do_PATCH = _forward


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


def main() -> int:
    global UPSTREAM, LOGFILE
    ap = argparse.ArgumentParser()
    ap.add_argument("--upstream", required=True)
    ap.add_argument("--listen-port", type=int, required=True)
    ap.add_argument("--log", required=True)
    args = ap.parse_args()
    UPSTREAM, LOGFILE = args.upstream, args.log
    open(LOGFILE, "w").close()
    srv = Server(("127.0.0.1", args.listen_port), Handler)
    print(f"counting-proxy listening on 127.0.0.1:{args.listen_port} -> {args.upstream}", flush=True)
    srv.serve_forever()
    return 0


if __name__ == "__main__":
    sys.exit(main())
