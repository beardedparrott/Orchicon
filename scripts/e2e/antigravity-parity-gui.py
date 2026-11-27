#!/usr/bin/env python3
"""antigravity-parity-gui.py — the GUI half of the antigravity parity state captures (child 8, AC3/AC4).

WHY A SCRIPT IN THE TREE AND NOT A SCREENSHOT TOOL THE OPERATOR RAN BY HAND. AC3 asks for a capture of
EACH listed state, which means the evidence is only worth anything if the next person can reproduce it.
A PNG produced by an uncommitted script in /tmp is an assertion, not evidence: nobody can tell which
state it shows or whether the fixture has since changed. So the harness that produced the PNGs lives
here, next to the TUI half (`internal/tui/antigravity_pty_capture_test.go`), and BOTH read the same
fixture plane (`internal/testfixtures/antigravityparity`) — one answer set, two clients.

PYTHON PLAYWRIGHT, NOT THE NODE ONE. `@playwright/test` in frontend/ pins a browser revision that this
host's global bundle does not carry; the Python package ships a Chromium that works here. That is the
only reason, and it is recorded so the next run does not re-litigate it.

WHAT IT DOES, per state:
  1. (re)start the fixture plane on :18080 in that state — one process per state, because a state is a
     fixed ANSWER, not a toggle;
  2. capture the surface that state is about (the Ask model picker, except mode-refused which is a
     conversation, and the AC4 adapter card which is any state);
  3. ASSERT the words on the page. A capture whose assertion is only "a PNG exists" cannot fail, so it
     cannot be evidence. Every row lands in gui-captures.tsv.

RUN IT (the vite dev server on :5174 must be up, or pass --serve to let this script start it):

    cd frontend && npx vite --config vite.activity-e2e.config.ts --host 127.0.0.1 --port 5174 &
    python3 scripts/e2e/antigravity-parity-gui.py

The plane binds 127.0.0.1:18080, NOT :8080, so this can never touch the dev or prod plane.
"""
from __future__ import annotations

import argparse
import json
import os
import pathlib
import shutil
import socket
import subprocess
import sys
import time
import urllib.request

PLANE_PORT = 18080
VITE_PORT = 5174
TOKEN = "e2e-access-token"
CONV_ID = "conv-agy-parity"

# Each state, the surface it is about, and the words that must be on the page for the capture to count.
# The strings are the SERVER's (adapter.AntigravityNotInstalledMessage, adapter.AntigravityAuthRequiredMessage
# and the vendored-snapshot note) — never a paraphrase, for the same reason the TUI test asserts constants.
STATES = [
    ("loading", "picker", ["Loading models"]),
    ("not-installed", "picker", ["Antigravity CLI (`agy`) not found", "install from"]),
    ("auth-error", "picker", ["Antigravity is not signed in on the host"]),
    ("stale", "picker", ["may be stale", "gemini-4-carbon-high"]),
    ("mode-refused", "conversation", ["REFUSED BY THE PLATFORM", "run_command", "read_url_content"]),
    ("live", "picker", ["gemini-4-carbon-high", "1.0M"]),
]
# AC4 gets its own row: the adapter card is the same on any state, so it is captured once against live.
AC4 = ("ac4-adapters", "adapters", ["Auth method", "oauth-personal"])


def port_open(port: int) -> bool:
    with socket.socket() as s:
        s.settimeout(0.5)
        return s.connect_ex(("127.0.0.1", port)) == 0


def wait_http(url: str, budget: float = 40.0) -> bool:
    deadline = time.time() + budget
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=2) as r:
                if r.status < 500:
                    return True
        except Exception:
            time.sleep(0.5)
    return False


def start_plane(root: pathlib.Path, state: str) -> subprocess.Popen:
    """Start the fixture plane in one state. The previous plane is killed first: the port is the
    identity here, and a stale plane on 18080 would make every later capture a statement about the
    wrong state."""
    stop_planes()
    log = pathlib.Path("/tmp/orchicon") / f"agyplane-{state}.log"
    log.parent.mkdir(parents=True, exist_ok=True)
    proc = subprocess.Popen(
        ["go", "run", "./internal/testfixtures/antigravityparity/cmd/agy-parity-plane",
         "-addr", f"127.0.0.1:{PLANE_PORT}", "-state", state],
        cwd=root, stdout=log.open("w"), stderr=subprocess.STDOUT,
    )
    if not wait_http(f"http://127.0.0.1:{PLANE_PORT}/healthz"):
        proc.kill()
        raise SystemExit(f"fixture plane did not answer /healthz for state {state} (log: {log})")
    return proc


def stop_planes() -> None:
    subprocess.run(["pkill", "-f", "agy-parity-plane"], check=False,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(1.0)


def seed(page) -> None:
    """Authenticate the SPA the way its own OIDC callback does: stash the bearer where session.ts looks.

    The fixture answers /auth/session, so this is the same session resolution a logged-in operator gets,
    not a bypass of the real route guard.
    """
    page.add_init_script(
        "() => { try { sessionStorage.setItem('orchicon_access_token', '%s'); } catch(e){} }" % TOKEN
    )


def capture_picker(page, base: str) -> str:
    """The composer's chip opens the SAME three-tier picker an agy-defaulted plane lands in.

    Not the Settings page: it hosts two pickers and the fixture's worker default is deliberately absent
    from its catalog, so that page opens onto a flagged ref instead of the ask default this capture is
    about.
    """
    page.goto(f"{base}/ask-orchicon", wait_until="domcontentloaded")
    page.wait_for_timeout(5000)
    chip = page.locator('[data-testid="ask-model-chip"]').first
    if chip.count() == 0:
        return page.inner_text("body")
    chip.click()
    page.wait_for_timeout(4000)
    panel = page.locator('[data-testid="ask-model-panel"]').first
    return panel.inner_text() if panel.count() else page.inner_text("body")


def capture_conversation(page, base: str) -> str:
    page.goto(f"{base}/ask-orchicon?conversationId={CONV_ID}", wait_until="domcontentloaded")
    page.wait_for_timeout(7000)
    return page.inner_text("body")


def capture_adapters(page, base: str) -> str:
    page.goto(f"{base}/adapters", wait_until="domcontentloaded")
    page.wait_for_timeout(6000)
    return page.inner_text("body")


SURFACES = {"picker": capture_picker, "conversation": capture_conversation, "adapters": capture_adapters}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", default=str(pathlib.Path(__file__).resolve().parents[2]),
                    help="project root (the plane is `go run` from here)")
    ap.add_argument("--out", default=None, help="evidence dir (default qa-evidence/antigravity-parity/gui)")
    ap.add_argument("--base", default=f"http://127.0.0.1:{VITE_PORT}", help="the SPA dev server")
    ap.add_argument("--only", default=None, help="capture one state (default: all of them)")
    args = ap.parse_args()

    root = pathlib.Path(args.root).resolve()
    out = pathlib.Path(args.out or root / "qa-evidence/antigravity-parity/gui")
    out.mkdir(parents=True, exist_ok=True)

    from playwright.sync_api import sync_playwright  # imported late so --help works without it

    wanted = [s for s in STATES if args.only in (None, s[0])]
    rows: list[tuple[str, str, str, str]] = []
    failures: list[str] = []

    with sync_playwright() as p:
        browser = p.chromium.launch(args=["--no-sandbox"])
        ctx = browser.new_context(viewport={"width": 1400, "height": 950})
        for state, surface, wants in wanted:
            start_plane(root, state)
            page = ctx.new_page()
            seed(page)
            text = SURFACES[surface](page, args.base)
            shot = out / f"gui-{state}.png"
            page.screenshot(path=str(shot))
            page.close()
            missing = [w for w in wants if w not in text]
            rows.append((state, surface, "PASS" if not missing else "FAIL",
                         "all of: " + ", ".join(wants) if not missing else "missing " + repr(missing)))
            if missing:
                failures.append(f"{state}: the page never showed {missing}")
            print(f"--- {state} ({surface}) -> {shot}")

        # AC4: the adapter card, against the live plane (the card is identical in every state).
        start_plane(root, "live")
        page = ctx.new_page()
        seed(page)
        state, surface, wants = AC4
        text = SURFACES[surface](page, args.base)
        shot = out / f"gui-{state}.png"
        page.screenshot(path=str(shot), full_page=True)
        page.close()
        # The ToS text is the SERVER's constant (authmethod.ToSNotice); the assertion is on its opening
        # words because the card wraps it, and on the selector being present at all.
        wants = wants + ["Antigravity personal sign-in is used through Google's own"]
        missing = [w for w in wants if w not in text]
        rows.append((state, surface, "PASS" if not missing else "FAIL",
                     "adapter card: auth method + ToS notice" if not missing else "missing " + repr(missing)))
        if missing:
            failures.append(f"{state}: the adapter card never showed {missing}")
        print(f"--- {state} ({surface}) -> {shot}")
        browser.close()

    stop_planes()

    tsv = out / "gui-captures.tsv"
    tsv.write_text("state\tsurface\tverdict\tobservation\n" +
                   "".join("\t".join(r) + "\n" for r in rows))
    (out / "gui-captures.json").write_text(json.dumps(
        [{"state": s, "surface": sf, "verdict": v, "observation": o} for s, sf, v, o in rows], indent=2))
    print(f"\ncaptures: {tsv}")
    if failures:
        print("FAILURES:\n  " + "\n  ".join(failures), file=sys.stderr)
        return 1
    print("OK — every state painted the words it claims")
    return 0


if __name__ == "__main__":
    sys.exit(main())
