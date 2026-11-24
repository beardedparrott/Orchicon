#!/usr/bin/env python3
"""Record an `agy acp` JSON-RPC transcript into a fixture file.

This is a spike recorder, not production code: it spawns the real `agy acp`
stdio server, drives one scripted scenario, and writes every line in both
directions to a `.jsonl` fixture under internal/antigravity/testdata/acp/.

Line 1 of every fixture is a header object describing the environment that
produced it, so a later reader can tell a real recording from a synthetic one.

Usage:
    python3 -I scripts/record-agy-acp.py --scenario simple_turn --prompt 'Reply OK.'

Scenarios are driven by scripts/agy_acp_scenarios.py (see SCENARIOS there) so
this file stays a transport + redaction layer.

Redaction is applied on write: OAuth tokens, token-named keys and
`GEMINI_API_KEY=...` are replaced with `<redacted>`, and the recorder's home
directory is replaced with /home/USER. No other rewriting happens.
"""

import argparse
import json
import os
import queue
import re
import subprocess
import sys
import threading
import time

REDACTED = "<redacted>"

# OAuth bearer tokens, Google API keys, and anything named like a token.
TOKEN_VALUE_RE = re.compile(r"ya29\.[A-Za-z0-9._\-]+|AIza[0-9A-Za-z_\-]{20,}")
SENSITIVE_KEY_RE = re.compile(
    r"^(access_token|refresh_token|id_token|accessToken|refreshToken|idToken|"
    r"api_key|apiKey|apiKeyValue|authorization|Authorization|token|secret|"
    r"client_secret|GEMINI_API_KEY|password)$"
)
ENV_KEY_RE = re.compile(r"(GEMINI_API_KEY|GOOGLE_API_KEY|GEMINI_API_KEY=)\s*=\s*\S+")


def _redact_string(s, home):
    if home and home != "/home/USER":
        s = s.replace(home, "/home/USER")
    s = TOKEN_VALUE_RE.sub(REDACTED, s)
    s = ENV_KEY_RE.sub(lambda m: m.group(1) + "=" + REDACTED, s)
    return s


def redact(obj, home):
    """Recursively redact secrets in a parsed JSON value."""
    if isinstance(obj, dict):
        out = {}
        for k, v in obj.items():
            if SENSITIVE_KEY_RE.match(k) and isinstance(v, str) and v:
                out[k] = REDACTED
            else:
                out[k] = redact(v, home)
        return out
    if isinstance(obj, list):
        return [redact(v, home) for v in obj]
    if isinstance(obj, str):
        return _redact_string(obj, home)
    return obj


def encode(value, home):
    """Serialize one wire line: parsed+redacted when valid JSON, else redacted text."""
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except (ValueError, TypeError):
            return json.dumps({"raw": _redact_string(value, home)}, ensure_ascii=False)
    return json.dumps(redact(value, home), ensure_ascii=False, separators=(",", ":"))


class Recorder:
    """Owns the agy subprocess, the transcript file and the request/response plumbing."""

    def __init__(self, argv, out_path, header, perm_policy="allow_once", verbose=False):
        self.argv = argv
        self.out_path = out_path
        self.header = header
        self.perm_policy = perm_policy
        self.verbose = verbose
        self.home = os.path.expanduser("~")
        self._next_id = 0
        self._pending = {}
        self._lock = threading.Lock()
        self.notifications = []  # every s2c notification seen, in order
        self.server_requests = []  # every s2c request seen (permission, fs, terminal)
        self.stderr_lines = []
        self.proc = None
        self._fh = None
        self._stop = threading.Event()
        self._perm_answers = 0

    # -- transcript -------------------------------------------------------

    def _write(self, direction, payload):
        line = json.dumps({"dir": direction, "line": payload}, ensure_ascii=False, separators=(",", ":"))
        with self._lock:
            self._fh.write(line + "\n")
            self._fh.flush()
        if self.verbose:
            sys.stderr.write("[" + direction + "] " + payload[:400] + "\n")

    def _write_header(self):
        self._fh.write(json.dumps({"header": redact(self.header, self.home)}, ensure_ascii=False, separators=(",", ":")) + "\n")
        self._fh.flush()

    # -- transport --------------------------------------------------------

    def start(self):
        os.makedirs(os.path.dirname(self.out_path), exist_ok=True)
        self._fh = open(self.out_path, "w", encoding="utf-8")
        self._write_header()
        self.proc = subprocess.Popen(
            self.argv,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            bufsize=1,
            universal_newlines=True,
            cwd=self.header.get("cwd") or None,
        )
        threading.Thread(target=self._read_stdout, daemon=True).start()
        threading.Thread(target=self._read_stderr, daemon=True).start()
        return self

    def _read_stderr(self):
        for raw in self.proc.stderr:
            self.stderr_lines.append(_redact_string(raw.rstrip("\n"), self.home))
            if self.verbose:
                sys.stderr.write("[agy-stderr] " + raw)

    def _read_stdout(self):
        for raw in self.proc.stdout:
            raw = raw.rstrip("\n")
            if not raw.strip():
                continue
            try:
                msg = json.loads(raw)
            except ValueError:
                self._write("s2c", raw)
                continue
            self._write("s2c", encode(msg, self.home))
            self._dispatch(msg)
        self._stop.set()
        with self._lock:
            for q in self._pending.values():
                q.put(None)

    def _dispatch(self, msg):
        if "id" in msg and ("result" in msg or "error" in msg) and "method" not in msg:
            with self._lock:
                q = self._pending.get(msg["id"])
            if q is not None:
                q.put(msg)
            return
        if "method" in msg and "id" in msg:
            # server -> client request
            self.server_requests.append(msg)
            self._handle_server_request(msg)
            return
        if "method" in msg:
            self.notifications.append(msg)

    def _send(self, payload):
        raw = json.dumps(payload, ensure_ascii=False)
        self._write("c2s", encode(raw, self.home))
        if self.proc.poll() is not None:
            raise RuntimeError("agy exited (code %s) before c2s send: %s" % (self.proc.returncode, raw[:200]))
        self.proc.stdin.write(raw + "\n")
        self.proc.stdin.flush()

    def notify(self, method, params=None):
        payload = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            payload["params"] = params
        self._send(payload)

    def request(self, method, params=None, timeout=180, expect_error=False):
        self._next_id += 1
        req_id = self._next_id
        payload = {"jsonrpc": "2.0", "id": req_id, "method": method}
        if params is not None:
            payload["params"] = params
        q = queue.Queue()
        with self._lock:
            self._pending[req_id] = q
        self._send(payload)
        try:
            resp = q.get(timeout=timeout)
        except queue.Empty:
            raise RuntimeError("timeout waiting for %s response (id=%s)" % (method, req_id))
        if resp is None:
            raise RuntimeError("agy closed the pipe while waiting for %s (id=%s)" % (method, req_id))
        if "error" in resp and not expect_error:
            raise RuntimeError("error response for %s: %s" % (method, json.dumps(resp["error"])[:400]))
        return resp

    # -- server -> client requests ---------------------------------------

    def _handle_server_request(self, msg):
        method = msg.get("method")
        params = msg.get("params") or {}
        if method == "session/request_permission":
            self._perm_answers += 1
            option_id = self._pick_option(params)
            if option_id is None:
                result = {"outcome": {"outcome": "cancelled"}}
            else:
                result = {"outcome": {"outcome": "selected", "optionId": option_id}}
            self._reply(msg["id"], result)
            return
        if method == "fs/read_text_file":
            path = params.get("path") or ""
            try:
                with open(path, encoding="utf-8", errors="replace") as fh:
                    text = fh.read()
                self._reply(msg["id"], {"content": text})
            except OSError as exc:
                self._reply_error(msg["id"], -32000, "read failed: %s" % exc)
            return
        if method == "fs/write_text_file":
            path = params.get("path") or ""
            try:
                os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
                with open(path, "w", encoding="utf-8") as fh:
                    fh.write(params.get("content") or "")
                self._reply(msg["id"], {})
            except OSError as exc:
                self._reply_error(msg["id"], -32000, "write failed: %s" % exc)
            return
        if method and method.startswith("terminal/"):
            self._reply(msg["id"], self._terminal(method, params))
            return
        # Unknown client method: record the refusal instead of pretending.
        self._reply_error(msg["id"], -32601, "method not supported by recorder: %s" % method)

    def _terminal(self, method, params):
        if method == "terminal/create":
            import tempfile
            out = tempfile.NamedTemporaryFile(prefix="acp-term-", suffix=".log", delete=False)
            out.close()
            subprocess.run(
                ["bash", "-lc", params.get("command") or ""] + list(params.get("args") or []),
                cwd=params.get("cwd") or None,
                stdout=open(out.name, "w"),
                stderr=subprocess.STDOUT,
            )
            return {"terminalId": out.name}
        if method == "terminal/output":
            path = params.get("terminalId") or ""
            try:
                with open(path, encoding="utf-8", errors="replace") as fh:
                    text = fh.read()
            except OSError:
                text = ""
            return {"output": text, "truncated": False}
        if method in ("terminal/wait_for_exit", "terminal/exit_status"):
            return {"exitCode": 0}
        if method == "terminal/release":
            try:
                os.unlink(params.get("terminalId") or "")
            except OSError:
                pass
            return {}
        if method == "terminal/kill":
            return {}
        return {}

    def _reply(self, req_id, result):
        self._send({"jsonrpc": "2.0", "id": req_id, "result": result})

    def _reply_error(self, req_id, code, message):
        self._send({"jsonrpc": "2.0", "id": req_id, "error": {"code": code, "message": message}})

    def _pick_option(self, params):
        """Choose an optionId per the scenario's permission policy."""
        options = params.get("options") or []
        wanted = self.perm_policy
        if wanted == "none":
            return None
        if wanted == "allow_once" or wanted == "allow":
            for opt in options:
                if opt.get("optionId") == "allow_once" or opt.get("kind") == "allow_once":
                    return opt.get("optionId")
            for opt in options:
                if str(opt.get("kind") or "").startswith("allow"):
                    return opt.get("optionId")
        if wanted == "allow_always":
            for opt in options:
                if opt.get("optionId") == "allow_always" or opt.get("kind") == "allow_always":
                    return opt.get("optionId")
            return self._pick_option_with(params, "allow_always")
        if wanted in ("reject_once", "reject"):
            for opt in options:
                if str(opt.get("kind") or "").startswith("reject"):
                    return opt.get("optionId")
        if wanted == "cancelled":
            return None
        # Fall back to the first option rather than hanging the recording.
        return options[0].get("optionId") if options else None

    def _pick_option_with(self, params, kind):
        for opt in params.get("options") or []:
            if opt.get("optionId") == kind:
                return opt.get("optionId")
        return None

    # -- waits ------------------------------------------------------------

    def wait_notification(self, method, timeout=180, since=0):
        """Block until a notification with this method appears at/after index `since`."""
        deadline = time.time() + timeout
        while time.time() < deadline:
            for msg in self.notifications[since:]:
                if msg.get("method") == method:
                    return msg
            if self._stop.is_set():
                return None
            time.sleep(0.05)
        return None

    def wait_turn_end(self, req_id, timeout=240):
        """Wait for the session/prompt response for req_id, then drain briefly."""
        resp = self.request_wait_handle(req_id, timeout)
        time.sleep(0.4)
        return resp

    def request_wait_handle(self, req_id, timeout=240):
        q = self._pending.get(req_id)
        if q is None:
            raise RuntimeError("no pending request id %s" % req_id)
        try:
            resp = q.get(timeout=timeout)
        except queue.Empty:
            raise RuntimeError("timeout waiting for response id=%s" % req_id)
        if resp is None:
            raise RuntimeError("agy closed the pipe while waiting for id=%s" % req_id)
        return resp

    # -- shutdown ---------------------------------------------------------

    def close(self, join_seconds=5):
        try:
            if self.proc and self.proc.stdin and not self.proc.stdin.closed:
                self.proc.stdin.close()
        except OSError:
            pass
        if self.proc:
            try:
                self.proc.wait(timeout=join_seconds)
            except subprocess.TimeoutExpired:
                self.proc.terminate()
                try:
                    self.proc.wait(timeout=join_seconds)
                except subprocess.TimeoutExpired:
                    self.proc.kill()
        if self._fh:
            self._fh.close()


def agy_version(argv0="agy"):
    try:
        out = subprocess.run([argv0, "--version"], capture_output=True, text=True, timeout=60)
        return (out.stdout or out.stderr).strip().splitlines()[0]
    except (OSError, subprocess.SubprocessError, IndexError):
        return "unknown"


def make_header(args, agy_argv, scenario):
    import platform

    return {
        "agy_version": agy_version(agy_argv[0]),
        "os": platform.system().lower(),
        "kernel": platform.release(),
        "arch": platform.machine(),
        "auth_method": args.auth_method,
        "auth_note": args.auth_note,
        "scenario": scenario,
        "recorded": time.strftime("%Y-%m-%d"),
        "argv": agy_argv,
        "cwd": args.cwd,
        "recorder": "scripts/record-agy-acp.py",
    }


# ---------------------------------------------------------------------------
# Scenario driver
# ---------------------------------------------------------------------------

def note(rec, text):
    """Record a recorder annotation (not wire data) so the fixture is self-explaining."""
    rec._fh.write(json.dumps({"dir": "meta", "line": {"note": _redact_string(text, rec.home)}},
                             ensure_ascii=False, separators=(",", ":")) + "\n")
    rec._fh.flush()


def do_initialize(rec, client_capabilities=None):
    caps = client_capabilities if client_capabilities is not None else {
        "fs": {"readTextFile": True, "writeTextFile": True},
        "terminal": True,
    }
    return rec.request("initialize", {
        "protocolVersion": 1,
        "clientCapabilities": caps,
        "clientInfo": {"name": "orchicon-spike-recorder", "version": "0.1.0"},
    })


def do_authenticate(rec, method_id="oauth-personal"):
    return rec.request("authenticate", {"methodId": method_id})


CURRENT_ARGS = None  # set by main(); lets every scenario honour --model and --mode


def apply_model_and_mode(rec, sid):
    """Pin the cheapest model before any prompt so a fixture never costs premium tokens."""
    args = CURRENT_ARGS
    if args is None:
        return
    if args.model:
        r = rec.request("session/set_model", {"sessionId": sid, "modelId": args.model}, expect_error=True)
        note(rec, "set_model(%s) -> %s" % (args.model, json.dumps(r.get("result") or r.get("error"))[:300]))
    if args.mode:
        r = rec.request("session/set_mode", {"sessionId": sid, "modeId": args.mode}, expect_error=True)
        note(rec, "set_mode(%s) -> %s" % (args.mode, json.dumps(r.get("result") or r.get("error"))[:300]))


def do_new_session(rec, cwd, mcp_servers=None, system_prompt_append=None, additional_directories=None):
    params = {"cwd": cwd, "mcpServers": mcp_servers if mcp_servers is not None else []}
    if additional_directories:
        params["additionalDirectories"] = additional_directories
    if system_prompt_append:
        params["_meta"] = {"antigravity": {"systemPrompt": {"append": system_prompt_append}}}
    resp = rec.request("session/new", params)
    sid = (resp.get("result") or {}).get("sessionId")
    if sid:
        apply_model_and_mode(rec, sid)
    return resp


def session_id_of(resp):
    result = resp.get("result") or {}
    sid = result.get("sessionId")
    if not sid:
        raise RuntimeError("session/new returned no sessionId: %s" % json.dumps(result)[:300])
    return sid


def do_prompt(rec, sid, text, wait=True, timeout=300):
    rec._next_id += 1
    req_id = rec._next_id
    q = queue.Queue()
    with rec._lock:
        rec._pending[req_id] = q
    rec._send({"jsonrpc": "2.0", "id": req_id,
               "method": "session/prompt",
               "params": {"sessionId": sid, "prompt": [{"type": "text", "text": text}]}})
    if not wait:
        return req_id
    resp = rec.request_wait_handle(req_id, timeout)
    time.sleep(0.5)
    return resp


def pick_model(rec, available, preferred):
    ids = [m.get("modelId") for m in available or []]
    if preferred:
        return preferred
    return ids[0] if ids else None


def after_common(rec, args):
    """Retry the session/new-with-_meta call in case the shape is rejected."""
    return None


SCENARIOS = {}


def scenario(name):
    def register(fn):
        SCENARIOS[name] = fn
        return fn
    return register


@scenario("handshake_auth")
def s_handshake(rec, args):
    init = do_initialize(rec)
    auth = do_authenticate(rec, args.auth_method)
    tier = (((auth.get("result") or {}).get("_meta") or {}).get("antigravity") or {})
    if tier:
        note(rec, "authenticate _meta.antigravity: " + json.dumps(tier)[:400])
    sid = session_id_of(do_new_session(
        rec, args.cwd,
        mcp_servers=[],
        system_prompt_append="You are recorded by the Orchicon spike recorder. Ignore this line.",
    ))
    note(rec, "session/new sessionId=" + sid)
    # No prompt: this fixture is free. Record the startup notifications that arrived.
    time.sleep(2.0)
    seen = sorted({m.get("method") for m in rec.notifications})
    note(rec, "startup s2c notifications seen: " + json.dumps(seen))


@scenario("auth_required")
def s_auth_required(rec, args):
    rec.request("initialize", {
        "protocolVersion": 1,
        "clientCapabilities": {"fs": {"readTextFile": True, "writeTextFile": True}, "terminal": True},
        "clientInfo": {"name": "orchicon-spike-recorder", "version": "0.1.0"},
    })
    # No authenticate call at all: does session/new itself require auth?
    resp = rec.request("session/new", {"cwd": args.cwd, "mcpServers": []}, expect_error=True, timeout=120)
    note(rec, "session/new WITHOUT authenticate: " + json.dumps(resp.get("result") or resp.get("error"))[:400])
    # An unsupported auth method id.
    r = rec.request("authenticate", {"methodId": "no-such-auth-method"}, expect_error=True, timeout=60)
    note(rec, "authenticate(no-such-auth-method): " + json.dumps(r.get("result") or r.get("error"))[:400])
    # The api-key method with NO key present: the shape the auth-failure mapper parses.
    r = rec.request("authenticate", {"methodId": "gemini-api-key"}, expect_error=True, timeout=120)
    note(rec, "authenticate(gemini-api-key) with no key: " + json.dumps(r.get("result") or r.get("error"))[:600])
    sid = (resp.get("result") or {}).get("sessionId")
    if sid:
        # Does a turn on this unauthenticated profile fail, and with what error?
        r = rec.request("session/set_model", {"sessionId": sid, "modelId": "gemini-3.8-flash-low"},
                        expect_error=True, timeout=60)
        note(rec, "set_model(gemini-3.8-flash-low): " + json.dumps(r.get("result") or r.get("error"))[:400])
        r = rec.request("session/prompt", {"sessionId": sid,
                                           "prompt": [{"type": "text", "text": "Reply with the single word OK."}]},
                        expect_error=True, timeout=180)
        note(rec, "prompt on unauthenticated profile: " + json.dumps(r.get("result") or r.get("error"))[:600])
    time.sleep(0.5)


@scenario("unknown_model")
def s_unknown_model(rec, args):
    do_initialize(rec)
    do_authenticate(rec, args.auth_method)
    sid = session_id_of(do_new_session(rec, args.cwd))
    rec.request("session/set_model", {"sessionId": sid, "modelId": "nonexistent-x"},
                expect_error=True, timeout=120)
    time.sleep(0.5)


@scenario("simple_turn")
def s_simple_turn(rec, args):
    do_initialize(rec)
    do_authenticate(rec, args.auth_method)
    sid = session_id_of(do_new_session(rec, args.cwd))
    if args.model:
        rec.request("session/set_model", {"sessionId": sid, "modelId": args.model}, expect_error=True)
    resp = do_prompt(rec, sid, args.prompt or "Reply with the single word OK. Do not use any tools.")
    note(rec, "session/prompt result: " + json.dumps(resp.get("result") or resp.get("error"))[:600])
    seen = sorted({m.get("method") for m in rec.notifications})
    note(rec, "s2c notifications seen: " + json.dumps(seen))


@scenario("thinking_turn")
def s_thinking_turn(rec, args):
    do_initialize(rec)
    do_authenticate(rec, args.auth_method)
    sid = session_id_of(do_new_session(rec, args.cwd))
    model = pick_model(rec, (do_new_session.__dict__.get("last_models") or []), args.model)
    if args.model:
        r = rec.request("session/set_model", {"sessionId": sid, "modelId": args.model}, expect_error=True)
        note(rec, "set_model -> " + json.dumps(r.get("result") or r.get("error"))[:300])
    resp = do_prompt(rec, sid, args.prompt or "What is 17*23? Think step by step, then give the number.")
    note(rec, "session/prompt result: " + json.dumps(resp.get("result") or resp.get("error"))[:600])
    kinds = []
    for m in rec.notifications:
        if m.get("method") == "session/update":
            kinds.append(((m.get("params") or {}).get("update") or {}).get("sessionUpdate"))
    note(rec, "session/update variants seen: " + json.dumps(sorted({k for k in kinds if k})))
    note(rec, "agent_thought_chunk present: " + json.dumps("agent_thought_chunk" in kinds))


@scenario("tool_read_file")
def s_tool_read_file(rec, args):
    seed = os.path.join(args.cwd, "hello.txt")
    os.makedirs(args.cwd, exist_ok=True)
    with open(seed, "w", encoding="utf-8") as fh:
        fh.write("hello from the orchicon acp spike\nsecond line\n")
    do_initialize(rec)
    do_authenticate(rec, args.auth_method)
    sid = session_id_of(do_new_session(rec, args.cwd))
    if args.mode:
        r = rec.request("session/set_mode", {"sessionId": sid, "modeId": args.mode}, expect_error=True)
        note(rec, "set_mode -> " + json.dumps(r.get("result") or r.get("error"))[:300])
    resp = do_prompt(rec, sid, args.prompt or "Read hello.txt and print its first line. Use a tool.")
    note(rec, "session/prompt result: " + json.dumps(resp.get("result") or resp.get("error"))[:600])
    kinds = []
    for m in rec.notifications:
        if m.get("method") == "session/update":
            kinds.append(((m.get("params") or {}).get("update") or {}).get("sessionUpdate"))
    note(rec, "session/update variants seen: " + json.dumps(sorted({k for k in kinds if k})))
    note(rec, "s2c client requests seen: " + json.dumps(sorted({m.get("method") for m in rec.server_requests})))


@scenario("tool_run_command_permission_allow")
def s_perm_allow(rec, args):
    do_initialize(rec)
    do_authenticate(rec, args.auth_method)
    sid = session_id_of(do_new_session(rec, args.cwd))
    resp = do_prompt(rec, sid, args.prompt or
                     "Run the shell command `echo hi` with your command tool and show me the output.")
    note(rec, "session/prompt result: " + json.dumps(resp.get("result") or resp.get("error"))[:600])
    perms = [m for m in rec.server_requests if m.get("method") == "session/request_permission"]
    note(rec, "permission requests: %d" % len(perms))
    for p in perms:
        note(rec, "permission params: " + json.dumps((p.get("params") or {}))[:1500])
    note(rec, "s2c client requests seen: " + json.dumps(sorted({m.get("method") for m in rec.server_requests})))


@scenario("tool_permission_reject")
def s_perm_reject(rec, args):
    rec.perm_policy = "reject_once"
    do_initialize(rec)
    do_authenticate(rec, args.auth_method)
    sid = session_id_of(do_new_session(rec, args.cwd))
    resp = do_prompt(rec, sid, args.prompt or
                     "Run the shell command `echo rejected` with your command tool and show me the output.")
    note(rec, "session/prompt result: " + json.dumps(resp.get("result") or resp.get("error"))[:600])
    perms = [m for m in rec.server_requests if m.get("method") == "session/request_permission"]
    note(rec, "permission requests: %d" % len(perms))


@scenario("file_edit_diff")
def s_file_edit_diff(rec, args):
    do_initialize(rec)
    do_authenticate(rec, args.auth_method)
    sid = session_id_of(do_new_session(rec, args.cwd))
    resp = do_prompt(rec, sid, args.prompt or
                     "Create a file named a.txt in the current directory containing exactly the letter x. "
                     "Then change its contents to exactly the letter y. Use your file tools.")
    note(rec, "session/prompt result: " + json.dumps(resp.get("result") or resp.get("error"))[:600])
    kinds = []
    for m in rec.notifications:
        if m.get("method") == "session/update":
            kinds.append(((m.get("params") or {}).get("update") or {}).get("sessionUpdate"))
    note(rec, "session/update variants seen: " + json.dumps(sorted({k for k in kinds if k})))


@scenario("ask_question")
def s_ask_question(rec, args):
    do_initialize(rec)
    do_authenticate(rec, args.auth_method)
    sid = session_id_of(do_new_session(rec, args.cwd))
    resp = do_prompt(rec, sid, args.prompt or
                     "Use your ask_question tool to ask me to choose between red and blue. Ask exactly one question.")
    note(rec, "session/prompt result: " + json.dumps(resp.get("result") or resp.get("error"))[:600])
    note(rec, "s2c client requests seen: " + json.dumps(sorted({m.get("method") for m in rec.server_requests})))
    for p in rec.server_requests:
        note(rec, p.get("method") + " params: " + json.dumps(p.get("params") or {})[:1500])


@scenario("mcp_tool_call")
def s_mcp_tool_call(rec, args):
    if not args.mcp_stdio:
        raise RuntimeError("--mcp-stdio is required for the mcp_tool_call scenario")
    do_initialize(rec)
    do_authenticate(rec, args.auth_method)
    server = {"name": "echo", "command": args.mcp_stdio.split()[0],
              "args": args.mcp_stdio.split()[1:]}
    resp = do_new_session(rec, args.cwd, mcp_servers=[server])
    sid = session_id_of(resp)
    note(rec, "session/new result (mcp section): " + json.dumps(resp.get("result") or {})[:1200])
    r = do_prompt(rec, sid, args.prompt or
                  "Call the MCP tool named echo with the text \"hi\" and report exactly what it returns.")
    note(rec, "session/prompt result: " + json.dumps(r.get("result") or r.get("error"))[:600])
    kinds = []
    for m in rec.notifications:
        if m.get("method") == "session/update":
            u = (m.get("params") or {}).get("update") or {}
            kinds.append(u.get("sessionUpdate"))
            if u.get("sessionUpdate") in ("tool_call", "tool_call_update"):
                note(rec, "tool update: " + json.dumps(u)[:1200])
    note(rec, "session/update variants seen: " + json.dumps(sorted({k for k in kinds if k})))
    note(rec, "s2c client requests seen: " + json.dumps(sorted({m.get("method") for m in rec.server_requests})))


@scenario("cancel_midturn")
def s_cancel_midturn(rec, args):
    do_initialize(rec)
    do_authenticate(rec, args.auth_method)
    sid = session_id_of(do_new_session(rec, args.cwd))
    before = len(rec.notifications)
    req_id = do_prompt(rec, sid, args.prompt or
                       "Write at least 500 words describing, slowly and in detail, the history of the pencil.",
                       wait=False)
    got = None
    for _ in range(600):
        for m in rec.notifications[before:]:
            u = (m.get("params") or {}).get("update") or {}
            if u.get("sessionUpdate") in ("agent_message_chunk", "agent_thought_chunk"):
                got = u.get("sessionUpdate")
                break
        if got:
            break
        time.sleep(0.05)
    note(rec, "first streamed chunk type before cancel: " + json.dumps(got))
    time.sleep(0.4)
    rec.notify("session/cancel", {"sessionId": sid})
    resp = rec.request_wait_handle(req_id, 180)
    time.sleep(0.5)
    note(rec, "session/prompt result after cancel: " + json.dumps(resp.get("result") or resp.get("error"))[:600])
    # Second attempt: cancel while a permission request is pending.
    before2 = len(rec.notifications)
    rec.perm_policy = "cancelled"
    req2 = do_prompt(rec, sid, "Run the shell command `echo still-here` and show me the output.", wait=False)
    time.sleep(1.0)
    rec.notify("session/cancel", {"sessionId": sid})
    try:
        resp2 = rec.request_wait_handle(req2, 180)
        note(rec, "2nd prompt (cancel with pending permission) result: "
             + json.dumps(resp2.get("result") or resp2.get("error"))[:600])
    except RuntimeError as exc:
        note(rec, "2nd prompt while-cancel did not settle: %s" % exc)


@scenario("load_session")
def s_load_session(rec, args):
    do_initialize(rec)
    do_authenticate(rec, args.auth_method)
    sid = args.session_id
    if not sid:
        raise RuntimeError("--session-id is required for the load_session scenario")
    r = rec.request("session/list", {}, expect_error=True)
    note(rec, "session/list -> " + json.dumps(r.get("result") or r.get("error"))[:2000])
    r = rec.request("session/load", {"sessionId": sid, "cwd": args.cwd, "mcpServers": []}, expect_error=True, timeout=180)
    note(rec, "session/load -> " + json.dumps(r.get("result") or r.get("error"))[:800])
    time.sleep(2.0)
    kinds = []
    for m in rec.notifications:
        if m.get("method") == "session/update":
            kinds.append(((m.get("params") or {}).get("update") or {}).get("sessionUpdate"))
    note(rec, "session/update variants seen on load: " + json.dumps(sorted({k for k in kinds if k})))
    note(rec, "notification count on load: %d" % len(rec.notifications))
    r = rec.request("session/resume", {"sessionId": sid, "cwd": args.cwd, "mcpServers": []}, expect_error=True, timeout=180)
    note(rec, "session/resume -> " + json.dumps(r.get("result") or r.get("error"))[:800])
    for candidate in ("gemini-4-neon-low", "claude-sonnet-5-5-low"):
        r = rec.request("session/set_model", {"sessionId": sid, "modelId": candidate}, expect_error=True)
        note(rec, "set_model(%s) -> %s" % (candidate, json.dumps(r.get("result") or r.get("error"))[:400]))
    r = rec.request("session/set_mode", {"sessionId": sid, "modeId": "accept-edits"}, expect_error=True)
    note(rec, "set_mode(accept-edits) -> " + json.dumps(r.get("result") or r.get("error"))[:400])


@scenario("compact")
def s_compact(rec, args):
    do_initialize(rec)
    do_authenticate(rec, args.auth_method)
    sid = session_id_of(do_new_session(rec, args.cwd))
    resp = do_prompt(rec, sid, args.prompt or "Reply with the single word READY.")
    note(rec, "turn 1 result: " + json.dumps(resp.get("result") or resp.get("error"))[:400])
    cmds = [((m.get("params") or {}).get("update") or {})
            for m in rec.notifications
            if m.get("method") == "session/update"
            and ((m.get("params") or {}).get("update") or {}).get("sessionUpdate") == "available_commands_update"]
    note(rec, "available_commands_update: " + json.dumps(cmds)[:2000])
    resp = do_prompt(rec, sid, "/compact")
    note(rec, "compact prompt result: " + json.dumps(resp.get("result") or resp.get("error"))[:600])
    time.sleep(0.5)


# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--scenario", required=True, choices=sorted(SCENARIOS))
    ap.add_argument("--out", default=None, help="fixture path (default testdata/acp/<scenario>.jsonl)")
    ap.add_argument("--agy", default="agy")
    ap.add_argument("--agy-arg", action="append", default=[], help="extra `agy acp` flag")
    ap.add_argument("--cwd", default=None, help="scratch dir used as session cwd")
    ap.add_argument("--prompt", default=None)
    ap.add_argument("--model", default=None)
    ap.add_argument("--mode", default=None)
    ap.add_argument("--perm", default="allow_once",
                    choices=["allow_once", "allow_always", "reject_once", "cancelled", "none"])
    ap.add_argument("--mcp-stdio", default=None, help="command line for a stdio MCP server")
    ap.add_argument("--session-id", default=None)
    ap.add_argument("--auth-method", default="oauth-personal")
    ap.add_argument("--auth-note", default="interactive OAuth token already present in the OS keyring")
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args(argv)
    global CURRENT_ARGS
    CURRENT_ARGS = args

    if not args.cwd:
        args.cwd = "/tmp/orchicon/acp/%s" % args.scenario
    os.makedirs(args.cwd, exist_ok=True)
    if not args.out:
        args.out = os.path.join("internal/antigravity/testdata/acp", args.scenario + ".jsonl")

    agy_argv = [args.agy, "acp"] + list(args.agy_arg)
    header = make_header(args, agy_argv, args.scenario)
    rec = Recorder(agy_argv, args.out, header, perm_policy=args.perm, verbose=args.verbose)
    status = "ok"
    try:
        rec.start()
        SCENARIOS[args.scenario](rec, args)
    except Exception as exc:  # keep the real (negative) transcript, annotate why
        status = "error: %s" % exc
        try:
            note(rec, "recorder stopped early: %s" % exc)
        except Exception:
            pass
        sys.stderr.write("[%s] %s\n" % (args.scenario, exc))
    finally:
        rec.close()
    sys.stderr.write("[%s] wrote %s (%s)\n" % (args.scenario, args.out, status))
    return 0 if status == "ok" else 1


if __name__ == "__main__":
    sys.exit(main())
