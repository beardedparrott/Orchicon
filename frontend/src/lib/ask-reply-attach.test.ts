import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";
import { answeredTurnToAttach } from "./ask-reply-attach";

// ask-reply-attach.test.ts — THE GUI LEG OF THE ANSWERED-ASK REPLY FIX.
//
// The operator, on a `claude/…` conversation: "the model's reply to that answer never appears until I
// leave the conversation and come back — but only when the reply is short."
//
// The server was returning no way to name the turn an MCP-served answer starts, so no client could
// attach to it; the GUI's re-attach (rail poll, turn_in_flight) and its transcript query (gated on
// turnInFlight) both depended on that turn being visible to a POLL, and a short reply finished inside
// the gap. `assistant_message_id` closes it, and this file pins the decision plus the wiring, because
// the route cannot be rendered by this setup — the idiom ask-transcript-poll.test.ts already uses here.

describe("answeredTurnToAttach", () => {
  it("names the new turn an applied answer started", () => {
    expect(
      answeredTurnToAttach({ applied: true, expired: false, assistantMessageId: "a-2" }),
    ).toBe("a-2");
  });

  it("attaches to nothing when the answer started no turn (the live-ask path)", () => {
    // The orchicon adapter answers INSIDE the turn that asked; that client is already consuming that
    // turn's stream, so attaching elsewhere would abandon it.
    expect(
      answeredTurnToAttach({ applied: true, expired: false, assistantMessageId: "" }),
    ).toBe("");
  });

  it("attaches to nothing for an expired or unapplied verdict, however it is shaped", () => {
    expect(
      answeredTurnToAttach({ applied: false, expired: true, assistantMessageId: "a-9" }),
    ).toBe("");
    expect(
      answeredTurnToAttach({ applied: false, expired: false, assistantMessageId: "a-9" }),
    ).toBe("");
    expect(
      answeredTurnToAttach({ applied: true, expired: true, assistantMessageId: "a-9" }),
    ).toBe("");
  });
});

describe("the reply handler in ask-orchicon", () => {
  const src = fs.readFileSync(
    path.join(__dirname, "..", "routes", "ask-orchicon.tsx"),
    "utf8",
  );

  it("arms a stream slot for the turn the answer started", () => {
    // A GREEN decision function with no caller would leave the reported symptom in place.
    expect(src).toContain("@/lib/ask-reply-attach");
    expect(src).toContain("answeredTurnToAttach({");
    expect(src).toContain("pendingReplyId: turnToAttach");
  });

  it("marks the armed slot RESTORED, so the existing watch re-dial runs for it", () => {
    // The re-dial effect only fires for a slot this component did not dispatch. Arming without this
    // flag leaves a slot that never dials — the same dead end in a new place.
    expect(src).toContain("restoredWatchRef.current[convId] = true;");
  });
});
