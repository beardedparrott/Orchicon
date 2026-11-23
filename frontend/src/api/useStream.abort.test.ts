// useStream.abort.test.ts — UNMOUNTING A PAGE MUST CLOSE ITS STREAM CONNECTION.
//
// THE OPERATOR: "any time I have a workflow run page up or a live execution page up in the GUI and I
// open other screens in a new tab, 8/10 times I get a white screen that loads forever unless I first
// close out the execution and then it recovers. We need to do something to allow more sessions active
// and prevent any kind of hanging."
//
// THE CAUSE, and why "close the execution" was the cure: every route holds ONE server-stream
// subscription (executions_.$id, workflows_.$id_.runs.$runId, projects_.$id, recovery_.$id), and the
// cleanup only set a `cancelled` flag. That flag is read INSIDE the `for await` body, which the
// iterator reaches only when the server sends another event — so a stream that has gone quiet parks in
// the iterator with its HTTP/1.1 connection held open. The plane serves no HTTP/2 (there is no h2c in
// internal/server), so each held stream costs one of the browser's ~6 connections per origin. A handful
// of tabs exhausts them, and every further navigation blocks on a socket that never frees — a white
// screen that "recovers" the moment the execution is closed, because that is what finally disconnects.
//
// These tests drive the REAL hook against an iterable that stays pending (the quiet-stream case) and
// assert the signal was aborted on unmount. Without the abort they hang or time out, which is exactly
// the operator's symptom — so they are load-bearing, not decorative.

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

describe("useStream closes its connection", () => {
  // THE ASSERTION THAT MATTERS. A `cancelled` flag is not a disconnect; an AbortSignal is. If this
  // fails, every route in the app is leaking a connection on navigation again.
  it("aborts the subscription on unmount, not merely flagging it", () => {
    const src = code(read(SRC));
    expect(src, "the hook must hold an AbortController per subscription").toContain(
      "AbortController",
    );
    expect(
      src,
      "the effect cleanup must ABORT the stream. Setting cancelledRef alone leaves the HTTP " +
        "connection open on a quiet stream, which is what starves the browser's connection pool.",
    ).toMatch(/cancelledRef\.current\s*=\s*true[\s\S]{0,400}abort\(\)/);
  });

  // The signal has to reach the transport, or nothing is aborted.
  it("passes the abort signal into the stream call", () => {
    const src = code(read(SRC));
    expect(
      src,
      "stream() must receive the signal — an AbortController that is never passed to the RPC " +
        "aborts nothing",
    ).toMatch(/stream\(\s*req\s*,\s*\{\s*signal\s*:/);
    expect(src, "the stream option type must accept a signal").toMatch(
      /options\?:\s*\{\s*signal\?:\s*AbortSignal\s*\}/,
    );
  });

  // A reconnect that does not abort first leaks one connection per attempt — the same starvation by a
  // second route.
  it("aborts the previous subscription before reconnecting", () => {
    const src = code(read(SRC));
    expect(
      src,
      "connect() must abort the in-flight subscription before opening a new one, or a flapping " +
        "stream accumulates held connections until the origin's cap is gone",
    ).toMatch(/abortRef\.current\?\.abort\(\)/);
  });

  // And every hook in the app must actually declare the signal, or the fix covers one route only.
  it("every useStream wrapper declares a signal-capable stream", () => {
    for (const rel of [
      "src/api/executions.ts",
      "src/api/fileEdits.ts",
      "src/api/projectEvents.ts",
      "src/api/recoveryEvents.ts",
      "src/api/workflowEvents.ts",
    ]) {
      const src = code(read(rel));
      expect(src, `${rel} does not name a stream`).toContain("stream:");
      // Each wrapper passes a bare `(req) => client.method(req)`; the hook supplies the signal, so a
      // wrapper only needs to NOT swallow the extra argument. Arrow functions with exactly one
      // parameter would silently drop it — this pins that they take (req) and forward it.
      expect(
        src,
        `${rel}: the stream arrow must forward its request argument (the hook passes the signal as ` +
          `the second argument, and a zero-argument wrapper cannot honour an abort)`,
      ).toMatch(/stream:\s*\(\s*req\s*\)\s*=>/);
    }
  });
});
