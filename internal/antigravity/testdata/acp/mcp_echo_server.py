#!/usr/bin/env python3
"""A trivial stdio MCP server used only to record how `agy acp` names and calls MCP tools.

It implements just enough of MCP (initialize, tools/list, tools/call) over
newline-delimited JSON-RPC on stdin/stdout. It is a test fixture, not a tool:
it echoes back the `text` argument and nothing else.

Usage (agy passes this as `command`/`args` in session/new.mcpServers):
    python3 -I internal/antigravity/testdata/acp/mcp_echo_server.py
"""

import json
import sys

TOOLS = [
    {
        "name": "echo",
        "description": "Echo the given text back to the caller.",
        "inputSchema": {
            "type": "object",
            "properties": {"text": {"type": "string", "description": "Text to echo."}},
            "required": ["text"],
        },
    }
]


def reply(msg_id, result):
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": msg_id, "result": result}) + "\n")
    sys.stdout.flush()


def main():
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except ValueError:
            continue
        method = msg.get("method")
        msg_id = msg.get("id")
        if method == "initialize":
            reply(msg_id, {
                "protocolVersion": msg.get("params", {}).get("protocolVersion", "2024-11-05"),
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "orchicon-echo", "version": "0.1.0"},
            })
        elif method == "notifications/initialized":
            continue
        elif method == "tools/list":
            reply(msg_id, {"tools": TOOLS})
        elif method == "tools/call":
            params = msg.get("params") or {}
            if params.get("name") != "echo":
                sys.stdout.write(json.dumps({
                    "jsonrpc": "2.0", "id": msg_id,
                    "error": {"code": -32602, "message": "unknown tool: %s" % params.get("name")},
                }) + "\n")
                sys.stdout.flush()
                continue
            text = str((params.get("arguments") or {}).get("text", ""))
            reply(msg_id, {"content": [{"type": "text", "text": "echo: " + text}], "isError": False})
        elif msg_id is not None:
            reply(msg_id, {})


if __name__ == "__main__":
    main()
