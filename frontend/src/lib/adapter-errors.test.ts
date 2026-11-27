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
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { ANTIGRAVITY_NOT_INSTALLED_MESSAGE, translateAdapterError } from "./adapter-errors";

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

// --- The antigravity auth path (AC11) ---------------------------------------
//
// The antigravity model tier fails with a CodeFailedPrecondition whose MESSAGE is
// adapter.AntigravityAuthRequiredMessage — a complete, actionable sentence written
// on the Go side (internal/adapter/antigravity_auth.go). The claim AC11 asks us to
// verify is that translateAdapterError needs NO special case for it: the sentence
// must reach the operator verbatim, with no Connect-code prefix, no rewording, and
// nothing else substituted.
//
// The sentence is spelled out here rather than imported: it is the string the
// operator sees, and a test that imported it could not notice it changing.

const ANTIGRAVITY_AUTH_MESSAGE =
  "Antigravity is not signed in on the host — run `agy` and sign in, " +
  "or configure a Gemini API key (GEMINI_API_KEY) on the host, then re-run. " +
  "Orchicon never signs in for you and stores no Google credential of its own. " +
  "IN CONTAINER MODE this is stricter than the claude adapter: agy's personal OAuth credential lives " +
  "in the HOST OS KEYRING, which cannot be bind-mounted into a runtime container — so a container " +
  "session needs GEMINI_API_KEY supplied to the plane (or to this run's secrets), and signing in on " +
  "the host alone is NOT enough.";

describe("translateAdapterError — antigravity auth failure", () => {
  it("passes the actionable sentence through verbatim, with no Connect code", () => {
    // Both tiers the picker asks for, and both shapes the error can arrive in: a
    // ConnectError whose message carries the code prefix, and the bare sentence.
    const prefixed = new Error(`ConnectError: [failed_precondition] ${ANTIGRAVITY_AUTH_MESSAGE}`);
    for (const tier of ["providers", "models"] as const) {
      for (const err of [prefixed, new Error(ANTIGRAVITY_AUTH_MESSAGE), ANTIGRAVITY_AUTH_MESSAGE]) {
        const { message } = translateAdapterError(err, "antigravity", tier);
        expect(message).toBe(ANTIGRAVITY_AUTH_MESSAGE);
        // The old, broken rendering of this tier was "[not_found] db: not found".
        expect(message).not.toContain("not_found");
        expect(message).not.toContain("db: not found");
        expect(message).not.toContain("ConnectError");
        expect(message).not.toContain("[failed_precondition]");
        // It must stay a sentence that names the fix.
        expect(message).toContain("run `agy` and sign in");
        expect(message).toContain("GEMINI_API_KEY");
      }
    }
  });

  it("does not reword it into the opencode CLI advice", () => {
    const { message, detail } = translateAdapterError(
      new Error(`ConnectError: [failed_precondition] ${ANTIGRAVITY_AUTH_MESSAGE}`),
      "antigravity",
      "models",
    );
    expect(message).not.toContain("opencode CLI");
    expect(message).not.toContain("“orchicon” adapter");
    // Unclassified path: the message IS the raw text, so nothing is hidden from a
    // bug report either.
    expect(detail).toBe("");
  });
});

// --- the antigravity INSTALL precondition (the empty / not-installed state) -------------------------
//
// A host with no `agy` and a host with a signed-out `agy` are two operator problems with two fixes. The
// server classifies them separately (internal/antigravity/modellist.go: ErrModelListNotInstalled vs
// ErrModelListAuthRequired) and sends adapter.AntigravityNotInstalledMessage for the first. This half
// pins that the GUI (a) renders that sentence verbatim, and (b) has NOT drifted from the Go constant.

describe("translateAdapterError — antigravity not installed", () => {
  it("renders the install sentence verbatim for the resolver's own wording", () => {
    // What connect delivers for the install precondition: CodeUnavailable carrying the sentence.
    const shapes = [
      new Error(`ConnectError: [unavailable] ${ANTIGRAVITY_NOT_INSTALLED_MESSAGE}`),
      new Error(ANTIGRAVITY_NOT_INSTALLED_MESSAGE),
      ANTIGRAVITY_NOT_INSTALLED_MESSAGE,
      // And the resolver's raw text, for the case where the classification did not run.
      new Error("antigravity: the `agy` CLI is not on PATH"),
    ];
    for (const tier of ["providers", "models"] as const) {
      for (const err of shapes) {
        const { message } = translateAdapterError(err, "antigravity", tier);
        expect(message).toBe(ANTIGRAVITY_NOT_INSTALLED_MESSAGE);
        // It must NOT be reworded into the sign-in advice: there is nothing to sign in to.
        expect(message).not.toContain("run `agy` and sign in");
        expect(message).not.toContain("opencode CLI");
        expect(message).not.toContain("ConnectError");
        expect(message).not.toContain("[unavailable]");
      }
    }
  });

  it("does not steal the opencode CLI advice for an antigravity path error", () => {
    // The opencode arm matches /install opencode on PATH/ — the antigravity install state must be
    // recognised FIRST, or an agy-missing failure renders "try the orchicon adapter instead".
    const { message } = translateAdapterError(
      new Error("antigravity: the `agy` CLI is not on PATH; install Antigravity"),
      "antigravity",
      "models",
    );
    expect(message).toBe(ANTIGRAVITY_NOT_INSTALLED_MESSAGE);
    expect(message).not.toContain("“orchicon” adapter");
  });

  it("THE DRIFT TEST: the TS constant matches the Go constant byte for byte", () => {
    // The sentence lives on the server (internal/adapter/antigravity_auth.go) and is mirrored here ONLY
    // so a client-side empty state has something to render with no RPC error in hand. Reading the Go
    // source is the point: an import cannot notice the Go side changing, a byte comparison can.
    const go = readFileSync(resolve(process.cwd(), "../internal/adapter/antigravity_auth.go"), "utf8");
    // The constant is declared as a concatenation across two lines; join the quoted pieces.
    const decl = /const AntigravityNotInstalledMessage = ([\s\S]*?)\n\n/.exec(go);
    expect(decl, "AntigravityNotInstalledMessage not found in internal/adapter/antigravity_auth.go").not.toBeNull();
    const goValue = [...decl![1].matchAll(/"((?:[^"\\]|\\.)*)"/g)]
      .map((m) => m[1].replace(/\\"/g, '"').replace(/\\n/g, "\n"))
      .join("");
    expect(goValue).toBe(ANTIGRAVITY_NOT_INSTALLED_MESSAGE);
  });

  it("stays a DIFFERENT sentence from the sign-in one", () => {
    expect(ANTIGRAVITY_NOT_INSTALLED_MESSAGE).not.toBe(ANTIGRAVITY_AUTH_MESSAGE);
    expect(ANTIGRAVITY_NOT_INSTALLED_MESSAGE).not.toContain("GEMINI_API_KEY");
    expect(ANTIGRAVITY_AUTH_MESSAGE).not.toContain("antigravity.google/download");
  });
});
