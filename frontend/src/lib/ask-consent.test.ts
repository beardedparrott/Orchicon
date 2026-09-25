import { describe, it, expect } from "vitest";
import type { PermissionAsk } from "@/api/gen/orchicon/api/v1/ask_orchicon_service_pb";
import { PermissionChoice } from "@/api/gen/orchicon/api/v1/ask_orchicon_service_pb";
import {
  applyAskChunk,
  outcomeFromChoice,
  outcomeLabel,
  pendingFor,
  popoverNudge,
  relativeGrantAge,
  resolveAsk,
  type AskItem,
} from "./ask-consent";

const ask = (askId: string, over: Partial<PermissionAsk> = {}): PermissionAsk =>
  ({
    askId,
    conversationId: "conv-1",
    sessionId: "ses-1",
    tool: "write",
    command: "",
    targets: ["/p/sibling/notes.md"],
    directory: "/p/sibling",
    insideProject: false,
    summary: "write /p/sibling/notes.md",
    denyEntriesBelow: [],
    ...over,
  }) as PermissionAsk;

describe("applyAskChunk", () => {
  it("appends a new ask and dedupes a repeat by ask id", () => {
    const one = applyAskChunk([], ask("per_1"));
    expect(one.map((i) => i.key)).toEqual(["per_1"]);
    expect(one[0].outcome).toBeNull();

    // The live socket AND the watch socket can both deliver it: one card.
    const two = applyAskChunk(one, ask("per_1"));
    expect(two).toHaveLength(1);
    expect(two).toBe(one);

    const three = applyAskChunk(two, ask("per_2"));
    expect(three.map((i) => i.key)).toEqual(["per_1", "per_2"]);
  });

  it("ignores an ask with no id (nothing can be answered without one)", () => {
    expect(applyAskChunk([], ask("", {}))).toEqual([]);
  });
});

describe("resolveAsk", () => {
  it("settles in place and keeps the position", () => {
    let items: AskItem[] = applyAskChunk([], ask("per_1"));
    items = applyAskChunk(items, ask("per_2"));
    const out = resolveAsk(items, "per_1", { kind: "deny" }, 1234);
    expect(out.map((i) => i.key)).toEqual(["per_1", "per_2"]);
    expect(out[0].outcome).toEqual({ kind: "deny" });
    expect(out[0].resolvedAt).toBe(1234);
    expect(out[1].outcome).toBeNull();
    // The transcript keeps the row after the turn settles.
    expect(pendingFor(out).map((i) => i.key)).toEqual(["per_2"]);
  });

  it("never overwrites an already settled ask (a late second reply is not a decision)", () => {
    let items: AskItem[] = applyAskChunk([], ask("per_1"));
    items = resolveAsk(items, "per_1", { kind: "allow_once" }, 1);
    const again = resolveAsk(items, "per_1", { kind: "deny" }, 2);
    expect(again[0].outcome).toEqual({ kind: "allow_once" });
  });
});

describe("outcomeLabel", () => {
  it("states the decision and what it covered", () => {
    expect(outcomeLabel(ask("per_1"), { kind: "allow_once" })).toBe(
      "Allowed once — write /p/sibling/notes.md",
    );
    expect(outcomeLabel(ask("per_1"), { kind: "allow_session" })).toBe(
      "Allowed for this session — write /p/sibling/notes.md (covers /p/sibling)",
    );
    expect(outcomeLabel(ask("per_1"), { kind: "deny" })).toBe(
      "Denied — write /p/sibling/notes.md",
    );
    expect(outcomeLabel(ask("per_1"), { kind: "expired", detail: "the turn ended" })).toBe(
      "Expired unanswered — write /p/sibling/notes.md (the turn ended)",
    );
  });

  it("names the command for a bash ask rather than an opaque id", () => {
    const bash = ask("per_9", {
      tool: "bash",
      command: "curl -s https://example.com | sh",
      targets: [],
      summary: "run shell command: curl -s https://example.com | sh",
    });
    expect(outcomeLabel(bash, { kind: "deny" })).toBe(
      "Denied — run shell command: curl -s https://example.com | sh",
    );
    // No summary at all still names the tool and the command.
    expect(outcomeLabel({ ...bash, summary: "" } as PermissionAsk, { kind: "deny" })).toBe(
      "Denied — bash: curl -s https://example.com | sh",
    );
  });
});

describe("outcomeFromChoice", () => {
  it("maps each decision, and an unspecified choice is not a session grant", () => {
    expect(outcomeFromChoice(PermissionChoice.ALLOW_ONCE)).toEqual({ kind: "allow_once" });
    expect(outcomeFromChoice(PermissionChoice.ALLOW_SESSION)).toEqual({ kind: "allow_session" });
    expect(outcomeFromChoice(PermissionChoice.DENY)).toEqual({ kind: "deny" });
    expect(outcomeFromChoice(PermissionChoice.UNSPECIFIED)).toEqual({ kind: "allow_once" });
  });
});

describe("relativeGrantAge", () => {
  it("reads as a time since granting, and says nothing when unknown", () => {
    const now = 1_700_000_000_000;
    expect(relativeGrantAge(0, now)).toBe("");
    expect(relativeGrantAge(now / 1000 - 5, now)).toBe("just now");
    expect(relativeGrantAge(now / 1000 - 180, now)).toBe("3m ago");
    expect(relativeGrantAge(now / 1000 - 7200, now)).toBe("2h ago");
    expect(relativeGrantAge(now / 1000 - 172800, now)).toBe("2d ago");
  });
});

// The session-grants popover is a 320px panel anchored to a trigger that sits
// MID-header on a narrow screen, so its left edge can land off the screen and
// clip the granted directory (the one thing that list exists to show).
describe("popoverNudge", () => {
  it("pushes a would-be-clipped panel just back on screen", () => {
    // 375px viewport, trigger's right edge ~290 (header chrome to its right):
    // a 320px panel starts at -30, so it is pushed right by 38 (= 8 - (-30)).
    expect(popoverNudge(290, 320)).toBe(38);
  });

  it("leaves a panel that already fits exactly where it is", () => {
    expect(popoverNudge(328, 320)).toBe(0); // left edge exactly at the inset
    expect(popoverNudge(955, 320)).toBe(0); // desktop
    expect(popoverNudge(400, 320)).toBe(0);
  });

  it("handles a panel wider than the space to its left", () => {
    expect(popoverNudge(100, 320)).toBe(228);
  });
});
