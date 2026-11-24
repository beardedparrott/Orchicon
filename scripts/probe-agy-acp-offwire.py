#!/usr/bin/env python3
"""Record the two `agy acp` facts that live OFF the ACP wire.

Some design-critical behaviour cannot appear in an ACP transcript at all:

  * the MCP handshake (agy <-> MCP server) is a *second* JSON-RPC channel that
    agy opens itself, so no `session/update` or `session/new` line can show the
    `server/discover` / `initialize` it performs; and
  * the `agy acp` startup flags (`--process-per-session`, `--disable-telemetry`,
    `--sandbox`) change process topology, not wire traffic, so an ACP transcript
    looks identical with and without them.

Rather than assert those in prose, this script records them as real observations
into a committed JSONL artifact (`internal/antigravity/testdata/acp/offwire-probes.jsonl`)
that the protocol doc can cite by line. Nothing here spends a model turn: both
probes stop after `initialize` + `session/new`.

Usage:
    python3 -I scripts/probe-agy-acp-offwire.py
    python3 -I scripts/probe-agy-acp-offwire.py --out /tmp/orchicon/offwire.jsonl

Every line is `{"probe", "step", "detail"}` (or a `header` line first) so a
reader can follow the order in which each observation was made.
"""

import argparse
import json
import os
import platform
import subprocess
import sys
import tempfile
import time

# A minimal stdio MCP server that records every message agy sends it.
# It is written to a temp file at run time so the probe stays one self-contained
# script; the observations it produces are what gets committed.
SHIM = r'''
import json, sys
LOG = sys.argv[1]
def note(direction, payload):
    with open(LOG, "a") as fh:
        fh.write(json.dumps({"direction": direction, "payload": payload}) + "\n")
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        msg = json.loads(line)
    except ValueError:
        continue
    note("agy->mcp", msg)
    mid = msg.get("id")
    if mid is None:                       # notification
        continue
    method = msg.get("method")
    if method == "initialize":
        params = msg.get("params") or {}
        result = {
            "protocolVersion": params.get("protocolVersion"),
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "orchicon-probe-shim", "version": "0.1.0"},
        }
    elif method == "tools/list":
        result = {"tools": [{"name": "echo", "description": "echo",
                             "inputSchema": {"type": "object",
                                             "properties": {"text": {"type": "string"}}}}]}
    elif method == "tools/call":
        result = {"content": [{"type": "text", "text": "echo-ok"}], "isError": False}
    else:
        result = {}
    out = {"jsonrpc": "2.0", "id": mid, "result": result}
    note("mcp->agy", out)
    sys.stdout.write(json.dumps(out) + "\n")
    sys.stdout.flush()
'''


def write_line(fh, obj):
    fh.write(json.dumps(obj, ensure_ascii=False, separators=(",", ":")) + "\n")
    fh.flush()


def agy_version(argv0="agy"):
    try:
        out = subprocess.run([argv0, "--version"], capture_output=True, text=True, timeout=60)
        return (out.stdout or out.stderr).strip().splitlines()[0]
    except (OSError, subprocess.SubprocessError, IndexError):
        return "unknown"


class Server:
    """Spawn `agy acp`, do initialize + session/new, and stop."""

    def __init__(self, argv, cwd, mcp_servers=None):
        self.argv = argv
        self.cwd = cwd
        self.mcp_servers = mcp_servers or []
        self.proc = None
        self.replies = {}

    def __enter__(self):
        self.proc = subprocess.Popen(
            self.argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL, text=True, bufsize=1, cwd=self.cwd,
        )
        return self

    def send(self, payload):
        self.proc.stdin.write(json.dumps(payload) + "\n")
        self.proc.stdin.flush()

    def read_until(self, want_id, timeout=120):
        deadline = time.time() + timeout
        while time.time() < deadline:
            line = self.proc.stdout.readline()
            if not line:
                break
            line = line.strip()
            if not line:
                continue
            try:
                msg = json.loads(line)
            except ValueError:
                continue
            if str(msg.get("id")) == str(want_id):
                self.replies[want_id] = msg
                return msg
        return None

    def handshake(self):
        self.send({"jsonrpc": "2.0", "id": 1, "method": "initialize",
                   "params": {"protocolVersion": 1,
                              "clientCapabilities": {"fs": {"readTextFile": True,
                                                            "writeTextFile": True},
                                                     "terminal": True},
                              "clientInfo": {"name": "orchicon-offwire-probe",
                                             "version": "0.1.0"}}})
        init = self.read_until(1)
        self.send({"jsonrpc": "2.0", "id": 2, "method": "session/new",
                   "params": {"cwd": self.cwd, "mcpServers": self.mcp_servers}})
        new = self.read_until(2)
        return init, new

    def __exit__(self, *exc):
        try:
            self.proc.stdin.close()
        except OSError:
            pass
        try:
            self.proc.wait(timeout=8)
        except subprocess.TimeoutExpired:
            self.proc.kill()


def summarise(new):
    """Reduce a session/new reply to the fields a flag probe cares about."""
    if not new:
        return {"reply": "no response"}
    if "error" in new:
        return {"error": new["error"]}
    result = new.get("result") or {}
    models = (result.get("models") or {})
    return {
        "sessionId": "present" if result.get("sessionId") else "missing",
        "availableModelCount": len(models.get("availableModels") or []),
        "currentModelId": models.get("currentModelId"),
        "replyKeys": sorted(result.keys()),
    }


def probe_mcp_handshake(fh, agy, cwd):
    """Record the MCP-layer handshake agy performs with a stdio server."""
    shim_dir = tempfile.mkdtemp(prefix="agy-mcp-probe-")
    shim_path = os.path.join(shim_dir, "shim.py")
    log_path = os.path.join(shim_dir, "shim.log")
    with open(shim_path, "w", encoding="utf-8") as fh_s:
        fh_s.write(SHIM)
    write_line(fh, {"probe": "mcp_handshake", "step": "shim_written",
                    "detail": {"path": "/tmp/agy-mcp-probe-*/shim.py"}})
    mcp = [{"name": "echo", "command": sys.executable,
            "args": ["-I", shim_path, log_path]}]
    with Server([agy, "acp"], cwd, mcp_servers=mcp) as srv:
        _init, new = srv.handshake()
        write_line(fh, {"probe": "mcp_handshake", "step": "session_new",
                        "detail": summarise(new)})
    time.sleep(1.0)
    seen = []
    try:
        with open(log_path, encoding="utf-8") as fh_l:
            for raw in fh_l:
                try:
                    seen.append(json.loads(raw))
                except ValueError:
                    continue
    except OSError as exc:
        write_line(fh, {"probe": "mcp_handshake", "step": "shim_log_unreadable",
                        "detail": str(exc)})
        return
    write_line(fh, {"probe": "mcp_handshake", "step": "message_count",
                    "detail": len(seen)})
    for entry in seen:
        payload = entry.get("payload") or {}
        write_line(fh, {"probe": "mcp_handshake", "step": entry.get("direction"),
                        "detail": payload})
    del seen


def probe_flags(fh, agy, cwd):
    """Record initialize + session/new under each startup flag."""
    for flags in ([], ["--process-per-session"], ["--disable-telemetry"], ["--sandbox"]):
        label = " ".join(flags) if flags else "(no flag)"
        try:
            with Server([agy, "acp"] + flags, cwd) as srv:
                init, new = srv.handshake()
                write_line(fh, {
                    "probe": "flags",
                    "step": "handshake",
                    "detail": {
                        "flags": flags,
                        "label": label,
                        "initialize": "error" if (init or {}).get("error") else "ok",
                        "session_new": summarise(new),
                    },
                })
        except OSError as exc:
            write_line(fh, {"probe": "flags", "step": "spawn_failed",
                            "detail": {"flags": flags, "error": str(exc)}})


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--agy", default="agy")
    ap.add_argument("--out", default="internal/antigravity/testdata/acp/offwire-probes.jsonl")
    ap.add_argument("--cwd", default="/tmp/orchicon/acp/offwire")
    args = ap.parse_args(argv)
    os.makedirs(args.cwd, exist_ok=True)
    os.makedirs(os.path.dirname(args.out) or ".", exist_ok=True)
    with open(args.out, "w", encoding="utf-8") as fh:
        write_line(fh, {"header": {
            "artifact": "offwire-probes",
            "agy_version": agy_version(args.agy),
            "os": platform.system().lower(),
            "kernel": platform.release(),
            "arch": platform.machine(),
            "recorded": time.strftime("%Y-%m-%d"),
            "recorder": "scripts/probe-agy-acp-offwire.py",
            "why": "the MCP handshake is off the ACP wire and the acp startup "
                   "flags do not change wire traffic, so neither can appear in a "
                   "transcript under testdata/acp/*.jsonl",
            "model_turns_spent": 0,
        }})
        probe_mcp_handshake(fh, args.agy, args.cwd)
        probe_flags(fh, args.agy, args.cwd)
    sys.stderr.write("[offwire] wrote %s\n" % args.out)
    return 0


if __name__ == "__main__":
    sys.exit(main())
