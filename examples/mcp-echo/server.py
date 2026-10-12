#!/usr/bin/env python3
# Copyright 2026 BAIRAV Foundation
# SPDX-License-Identifier: Apache-2.0
#
# mcp-echo — a tiny reference MCP server for BAIRAVS Skill demos.
#
# Speaks the Model Context Protocol over SSE (Server-Sent Events).
# Exposes a single tool called `echo` that returns whatever text the
# caller sends in. Deliberately minimal — the point is to show
# `bairav skill deploy` end-to-end with an image that actually runs,
# not to be a production MCP server.
#
# Endpoints:
#   GET  /health       liveness probe (200 "ok")
#   GET  /sse          SSE stream — MCP transport per spec 2025-06
#   POST /messages     JSON-RPC endpoint for tool calls
#
# One file, stdlib only, no external deps. Runs in the tiny python:alpine
# image (~50MB). If you want to see the raw MCP handshake, curl /sse and
# watch the `endpoint` event stream past.

from __future__ import annotations

import json
import os
import queue
import sys
import threading
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

PORT = int(os.environ.get("PORT", "3000"))

# Session ID → outbound message queue. The SSE handler pops from here
# and streams to the client; the POST handler pushes replies here.
# Sessions are keyed by a UUID handed to the client on /sse connect.
_SESSIONS: dict[str, "queue.Queue[str]"] = {}
_SESSIONS_LOCK = threading.Lock()

SERVER_INFO = {
    "name": "bairav-mcp-echo",
    "version": "1.0.0",
}

# The single tool this server exposes. Real MCP servers would list many.
TOOLS = [
    {
        "name": "echo",
        "description": "Return the caller's text unchanged. Useful for verifying MCP wiring.",
        "inputSchema": {
            "type": "object",
            "properties": {"text": {"type": "string"}},
            "required": ["text"],
        },
    },
]


def _handle_rpc(msg: dict[str, Any]) -> dict[str, Any] | None:
    """Handle a JSON-RPC 2.0 request per MCP spec. Returns the reply dict
    or None for notifications (which get no reply)."""
    method = msg.get("method")
    msg_id = msg.get("id")

    # Notifications carry no id and expect no reply.
    if msg_id is None:
        return None

    if method == "initialize":
        return _reply(msg_id, {
            "protocolVersion": "2025-06-18",
            "capabilities": {"tools": {}},
            "serverInfo": SERVER_INFO,
        })

    if method == "tools/list":
        return _reply(msg_id, {"tools": TOOLS})

    if method == "tools/call":
        params = msg.get("params", {})
        if params.get("name") != "echo":
            return _error(msg_id, -32602, f"unknown tool: {params.get('name')}")
        text = params.get("arguments", {}).get("text", "")
        return _reply(msg_id, {
            "content": [{"type": "text", "text": text}],
            "isError": False,
        })

    return _error(msg_id, -32601, f"method not found: {method}")


def _reply(msg_id: Any, result: dict[str, Any]) -> dict[str, Any]:
    return {"jsonrpc": "2.0", "id": msg_id, "result": result}


def _error(msg_id: Any, code: int, message: str) -> dict[str, Any]:
    return {"jsonrpc": "2.0", "id": msg_id, "error": {"code": code, "message": message}}


class Handler(BaseHTTPRequestHandler):
    # Quiet the default access log — the container captures stdout and
    # BaseHTTPRequestHandler's default log is noisy for a demo image.
    def log_message(self, fmt: str, *args: Any) -> None:
        sys.stderr.write("mcp-echo: " + fmt % args + "\n")

    # CORS: lets the MCP Hub on bairavs.com (or any page) talk to this demo
    # server straight from the browser. Fine for a reference echo server
    # with no data; do NOT copy this wide-open policy into a real server.
    def end_headers(self) -> None:
        self.send_header("Access-Control-Allow-Origin", "*")
        super().end_headers()

    def do_OPTIONS(self) -> None:
        self.send_response(204)
        self.send_header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
        self.send_header("Access-Control-Allow-Headers", "Content-Type")
        # Chrome's Private Network Access: a public https page calling
        # 127.0.0.1 must be explicitly allowed by the local server.
        self.send_header("Access-Control-Allow-Private-Network", "true")
        self.end_headers()

    def do_GET(self) -> None:
        if self.path == "/health":
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.end_headers()
            self.wfile.write(b"ok")
            return

        if self.path.startswith("/sse"):
            self._serve_sse()
            return

        self.send_response(404)
        self.end_headers()

    def do_POST(self) -> None:
        if not self.path.startswith("/messages"):
            self.send_response(404)
            self.end_headers()
            return

        # session_id ties this POST to a live SSE stream.
        session_id = self._query_param("session_id")
        with _SESSIONS_LOCK:
            q = _SESSIONS.get(session_id or "")
        if q is None:
            self.send_response(400)
            self.end_headers()
            self.wfile.write(b"unknown or expired session_id")
            return

        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length) if length else b""
        try:
            msg = json.loads(body)
        except json.JSONDecodeError:
            self.send_response(400)
            self.end_headers()
            self.wfile.write(b"invalid JSON")
            return

        reply = _handle_rpc(msg)
        # Acknowledge immediately; the actual reply travels over SSE.
        self.send_response(202)
        self.end_headers()
        if reply is not None:
            q.put(json.dumps(reply))

    def _serve_sse(self) -> None:
        session_id = uuid.uuid4().hex
        q: queue.Queue[str] = queue.Queue()
        with _SESSIONS_LOCK:
            _SESSIONS[session_id] = q

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "keep-alive")
        self.end_headers()

        # First SSE event tells the client where to POST replies.
        # This is the MCP SSE-transport handshake.
        endpoint = f"/messages?session_id={session_id}"
        self._send_event("endpoint", endpoint)

        try:
            while True:
                # Blocking pop — a real server would also send keepalives
                # every ~15s, but for demo purposes the client's own
                # request cycle keeps the stream alive.
                payload = q.get()
                self._send_event("message", payload)
        except (BrokenPipeError, ConnectionResetError):
            pass
        finally:
            with _SESSIONS_LOCK:
                _SESSIONS.pop(session_id, None)

    def _send_event(self, event: str, data: str) -> None:
        try:
            self.wfile.write(f"event: {event}\ndata: {data}\n\n".encode())
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            raise

    def _query_param(self, key: str) -> str | None:
        if "?" not in self.path:
            return None
        _, qs = self.path.split("?", 1)
        for pair in qs.split("&"):
            if "=" not in pair:
                continue
            k, v = pair.split("=", 1)
            if k == key:
                return v
        return None


def main() -> None:
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    sys.stderr.write(f"mcp-echo listening on :{PORT}\n")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        sys.stderr.write("mcp-echo shutting down\n")


if __name__ == "__main__":
    main()
