/**
 * adapter-errors.test.ts — the operator must never be shown a transport error.
 *
 * Reported verbatim:
 *
 *   "Failed to load providers: ConnectError: [unimplemented] model discovery is not configured (set
 *    ORCHICON_OPENCODE_BIN or install opencode on PATH). Failed to load models: ConnectError:
 *    [unimplemented] model discovery is not configured (set ORCHICON_OPENCODE_BIN or install
 *    opencode on PATH)"
 *
 * Three things are wrong with that string and each is asserted below:
 *   1. it names the TRANSPORT ("ConnectError: [unimplemented]"), not the problem;
 *   2. it repeats itself once per tier;
 *   3. it never says what to do — the actionable half ("use the orchicon adapter, it needs nothing")
 *      does not appear at all.
 */

import { describe, expect, it } from "vitest";

import { translateAdapterError } from "./adapter-errors";

/** The real error text, as it reaches the browser. */
const DISCOVERY_ERR = new Error(
  "ConnectError: [unimplemented] model discovery is not configured (set ORCHICON_OPENCODE_BIN or install opencode on PATH)",
);

describe("translateAdapterError", () => {
  it("never renders a Connect code or a Go error wrapper", () => {
    const { message, detail } = translateAdapterError(DISCOVERY_ERR, "opencode", "providers");
    for (const leaked of ["ConnectError", "[unimplemented]", "ORCHICON_OPENCODE_BIN"]) {
      expect(message).not.toContain(leaked);
      expect(detail).not.toContain(leaked);
    }
  });

  it("names the missing thing and offers the adapter that needs nothing installed", () => {
    const { message } = translateAdapterError(DISCOVERY_ERR, "opencode", "providers");
    expect(message).toContain("opencode CLI");
    expect(message).toContain("orchicon");
    // It must read as guidance, not as a failure dump.
    expect(message).not.toMatch(/^Failed to load/);
  });

  it("says the same thing for the model tier as for the provider tier", () => {
    // The reported symptom printed the identical cause twice in two different shapes. One cause, one
    // sentence — the tier does not change the fix.
    const a = translateAdapterError(DISCOVERY_ERR, "opencode", "providers").message;
    const b = translateAdapterError(DISCOVERY_ERR, "opencode", "models").message;
    expect(a).toBe(b);
  });

  it("accepts a plain string as well as an Error (connect-query surfaces both)", () => {
    const fromString = translateAdapterError(String(DISCOVERY_ERR.message), "opencode", "models");
    const fromError = translateAdapterError(DISCOVERY_ERR, "opencode", "models");
    expect(fromString.message).toBe(fromError.message);
  });

  it("does NOT invent a friendly sentence for an unclassified failure", () => {
    // A friendly-sounding wrapper around an unknown error is how a real bug becomes invisible. The
    // operator gets the real words (minus the transport prefix, which is never their problem).
    const { message } = translateAdapterError(
      new Error("ConnectError: [internal] pq: connection refused"),
      "orchicon",
      "models",
    );
    expect(message).toContain("connection refused");
    expect(message).not.toContain("ConnectError");
    expect(message).not.toContain("opencode CLI");
  });

  it("keeps the raw text reachable for a bug report when it is not shown inline", () => {
    const { message, detail } = translateAdapterError(
      new Error("ConnectError: [unknown] something new"),
      "orchicon",
      "providers",
    );
    // Unclassified: the message IS the raw text, and there is no hidden remainder.
    expect(message).toContain("something new");
    expect(detail).toBe("");
  });
});
