import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

/**
 * -ask-composer-char-counter.test.ts — THE WEB COMPOSER WARNS AS THE MESSAGE BOUND IS APPROACHED.
 *
 * The bound was 10,000 and the composer said nothing, so the operator met it mid-paste — after the text was
 * already in the box. Two properties matter and neither is cosmetic:
 *
 *   1. the counter appears only over the last fifth (a count under every sentence is noise, and noise is
 *      ignored by the time it matters), and
 *   2. the number it warns at is the number the SERVER enforces (internal/chatlimits), because a client that
 *      warns at a different number then refuses at another is worse than no warning at all.
 *
 * A RENDERING test would be better but cannot reach it: the composer needs a full tRPC/query/stream context
 * and is not exported for isolated mount — the same limitation -ask-composer-model-chip.test.ts records, and
 * the same technique answers it.
 */
const SRC = readFileSync(resolve(__dirname, "ask-orchicon.tsx"), "utf8");

describe("Ask composer character counter", () => {
  it("renders the counter behind the warn threshold, never unconditionally", () => {
    expect(
      SRC,
      "the char counter is rendered without the CHAR_WARN_AT guard again — a count under every sentence " +
        "is the noise the threshold exists to avoid.",
    ).toMatch(/\{\s*text\.length\s*>=\s*CHAR_WARN_AT\s*&&/);
  });

  it("warns at 80% of a bound of 100,000 — the same numbers the server enforces", () => {
    expect(SRC, "MAX_MESSAGE_CHARS is not 100,000").toMatch(
      /const MAX_MESSAGE_CHARS = 100_000;/,
    );
    expect(SRC, "CHAR_WARN_AT is not derived from the bound").toMatch(
      /const CHAR_WARN_AT = Math\.floor\(MAX_MESSAGE_CHARS \* 0\.8\);/,
    );
  });

  it("turns the counter red only once a send would be refused", () => {
    expect(
      SRC,
      "the counter no longer distinguishes over-the-limit — the operator learns the send will fail only " +
        "after pressing it.",
    ).toMatch(/text\.length > MAX_MESSAGE_CHARS[\s\S]{0,120}text-destructive/);
  });

  it("names both numbers, so the operator can see how much to trim", () => {
    expect(SRC).toMatch(/text\.length\.toLocaleString\(\)/);
    expect(SRC).toMatch(/MAX_MESSAGE_CHARS\.toLocaleString\(\)/);
  });
});
