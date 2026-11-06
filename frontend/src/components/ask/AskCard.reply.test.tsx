// AskCard.reply.test.tsx — A STUCK REPLY MUST NOT DISABLE THE CARD FOR EVER.
//
// REPORTED: "clicking an answer sometimes doesn't work unless I first refresh the page."
//
// `askInFlight[askId]` gates the reply handler and drives each card's `busy` prop, which disables every option
// button. It is cleared only in `finally` — so a reply that never settles leaves the entry in place, the guard
// swallows every later click, and the card looks live with buttons that do nothing. A refresh resets this
// component's state, which is exactly why refreshing appeared to fix it.
//
// These are source assertions because the behaviour lives in a route handler that needs a live client to run;
// the meaningful property is that the reply is BOUNDED, so `finally` always gets its chance.
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

const routeSrc = readFileSync(resolve(__dirname, "../../routes/ask-orchicon.tsx"), "utf8");

describe("ask reply is bounded", () => {
  it("wraps the reply RPC in a deadline so the in-flight flag always clears", () => {
    expect(routeSrc).toContain("withReplyDeadline(");
    expect(routeSrc).toContain("REPLY_DEADLINE_MS");
    // The flag is still cleared in finally — that is what makes the deadline effective.
    expect(routeSrc).toMatch(/finally \{\s*setAskInFlight/);
  });

  it("scopes a settled answer to its conversation, not to a bare ask id", () => {
    // The id alone was not an identity: the server's per-turn ledger reused `tc-3`, and this map is module-level
    // and never cleared, so a later turn's card could be drawn already-answered.
    expect(routeSrc).toContain("function settledKey(convId: string, askId: string)");
    expect(routeSrc).toContain("settledAsks.set(settledKey(convId, askId), answer)");
    expect(routeSrc).toContain("settledAnswerOf(convId, call.id)");
  });
});
