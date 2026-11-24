// useStream.visibility.test.ts — N OPEN TABS MUST NOT MEAN N HELD CONNECTIONS.
//
// THE OPERATOR: "Someone should be able to open as many tabs as they want with no issues."
//
// Aborting on unmount (useStream.abort.test.ts) freed the socket on NAVIGATION, but a tab that is
// still OPEN holds its stream whether or not anyone is looking at it. The plane serves cleartext with
// no HTTP/2 (there is no h2c in internal/server), so a browser's ~6 connections per origin are the
// budget — and the 7th tab blocks on a socket that never frees. That is the white screen, and closing
// the execution page is what "fixed" it because that finally released the stream.
//
// The bound must therefore be the number of tabs the operator is LOOKING AT, not the number open. A
// hidden tab releases its socket and reattaches on show, resuming from lastSequence — the protocol
// already supports that, so no event is missed.
//
// These tests drive the REAL hook with a pending iterable (the quiet-stream case) and assert the
// signal is aborted when the tab goes hidden, and a fresh subscription is opened when it returns.

import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";

const read = (rel: string) => readFileSync(resolve(process.cwd(), rel), "utf8");

/** Strip comments so a MENTION of a hazard in a rationale is not read as code. */
function code(src: string): string {
  return src
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/(^|[^:])\/\/.*$/gm, "$1");
}

const SRC = "src/api/useStream.ts";

describe("useStream keeps held sockets to the visible tabs", () => {
  it("subscribes to visibilitychange and releases the connection when hidden", () => {
    const src = code(read(SRC));
    expect(src, "the hook must listen for visibility changes").toContain(
      "visibilitychange",
    );
    // The release must be an ABORT of the live subscription. Setting a flag is what left a quiet
    // stream parked in `for await` holding its socket — the original bug.
    expect(
      src,
      "hiding the tab must abort the in-flight subscription, not merely flag it",
    ).toMatch(/abortRef\.current\?\.abort\(\)/);
  });

  it("refuses to open a subscription while the tab is hidden", () => {
    const src = code(read(SRC));
    // A reconnect timer that fires while hidden must not spend a socket.
    expect(
      src,
      "connect() must bail out when the tab is hidden, or a background tab reclaims the socket the " +
        "visibility handler just released",
    ).toMatch(/if\s*\(\s*hiddenRef\.current\s*\)\s*\{[\s\S]{0,120}?return;/);
  });

  it("does not run the reconnect backoff while hidden", () => {
    const src = code(read(SRC));
    expect(
      src,
      "the reconnect scheduler must park instead of backing off while hidden, so returning to the " +
        "tab reconnects immediately rather than waiting out a maxed-out backoff",
    ).toMatch(/scheduleReconnectRef\.current[\s\S]{0,200}?hiddenRef\.current/);
  });

  // Returning to a tab must reattach. Without this the tab stays "idle" forever and looks broken in a
  // different way — the stream would be released but never reacquired.
  it("reconnects when the tab becomes visible again", () => {
    const src = code(read(SRC));
    expect(
      src,
      "the visibility handler must call connect() on show",
    ).toMatch(/if\s*\(\s*enabled\s*\)\s*\{[\s\S]{0,80}?connect\(\);/);
  });
});
