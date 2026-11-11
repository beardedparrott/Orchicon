// verifyNotice.test.ts — THE EXECUTION PAGE MUST NOT CALL A NOTICE AN ERROR.
//
// REPORTED, from a live quick-worker run: an execution that SUCCEEDED rendered a red ERROR card reading
// "verification notice: the platform could not confirm this step mechanically (3 of 5 check(s) confirmed)…".
// Nothing had failed. The verification notice is written into the same error_message column a real failure
// uses, so the page has to tell them apart — and the platform marks its advisories with a prefix.
//
// The prefix is a WIRE CONTRACT with internal/verify.NoticePrefix in Go, so it is pinned here by value: a
// change on either side that is not mirrored shows up as a red failure on a successful step, which is exactly
// the report this file exists for.
import { describe, expect, it } from "vitest";
import { isNoticeMessage, stripNoticePrefix } from "./verifyNotice";

const REPORTED =
  "verification notice: the platform could not confirm this step mechanically (3 of 5 check(s) confirmed) — " +
  "unverified: no_new_failures: no baseline was captured for this step";

describe("isNoticeMessage", () => {
  it("recognises the reported verification notice", () => {
    expect(isNoticeMessage(REPORTED)).toBe(true);
  });

  it("tolerates surrounding whitespace, which a stored field may carry", () => {
    expect(isNoticeMessage("  " + REPORTED + "\n")).toBe(true);
  });

  it("does NOT treat a real failure as a notice", () => {
    for (const msg of [
      "worker reported failure in its own ORCHICON WORKER SUMMARY: the gate is red",
      "claude session exited without a terminal result message (exit code 1)",
      "container exited 137",
      "",
      null,
      undefined,
    ]) {
      expect(isNoticeMessage(msg)).toBe(false);
    }
  });

  it("does not match the prefix anywhere but the start", () => {
    // A failure whose text merely QUOTES the prefix must still render as a failure.
    expect(isNoticeMessage("the worker said: verification notice: nothing ran")).toBe(false);
  });
});

describe("stripNoticePrefix", () => {
  it("removes the prefix so the note reads as a sentence under the Note heading", () => {
    expect(stripNoticePrefix(REPORTED)).toMatch(/^the platform could not confirm/);
  });

  it("leaves a message without the prefix untouched", () => {
    const plain = "container exited 137";
    expect(stripNoticePrefix(plain)).toBe(plain);
  });
});
