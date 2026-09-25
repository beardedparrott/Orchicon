import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";
import { isAskUserToolCall, parseAskUserArgs } from "./AskCard";

describe("parseAskUserArgs", () => {
  it("accepts object options with descriptions", () => {
    const got = parseAskUserArgs(
      JSON.stringify({
        question: "Which branch?",
        options: [
          { label: "develop", description: "integration" },
          { label: "main" },
        ],
      }),
    );
    expect(got).not.toBeNull();
    expect(got!.question).toBe("Which branch?");
    expect(got!.options.map((o) => o.label)).toEqual(["develop", "main"]);
    expect(got!.options[0].description).toBe("integration");
  });

  it("accepts plain-string options", () => {
    const got = parseAskUserArgs(
      JSON.stringify({ question: "Pick one", options: ["a", "b"] }),
    );
    expect(got!.options.map((o) => o.label)).toEqual(["a", "b"]);
  });

  it("reads allow_other", () => {
    const got = parseAskUserArgs(
      JSON.stringify({ question: "Title?", options: [], allow_other: true }),
    );
    expect(got!.allowOther).toBe(true);
    expect(got!.options).toEqual([]);
  });

  it("returns null (never throws) on malformed arguments", () => {
    expect(parseAskUserArgs("{not json")).toBeNull();
    expect(parseAskUserArgs(undefined)).toBeNull();
    expect(parseAskUserArgs(JSON.stringify({ options: ["a"] }))).toBeNull();
  });

  it("recognises the ask_user call on both naming forms", () => {
    expect(isAskUserToolCall("ask_user")).toBe(true);
    expect(isAskUserToolCall("orchicon_ask_user")).toBe(true);
    expect(isAskUserToolCall("list_projects")).toBe(false);
  });
});

// Source-scan checks (mirrors MCPPicker.test.tsx): verify the card and its
// wiring into the transcript route. Rendering is intentionally not exercised
// here — these assertions pin the CONTRACT (interactivity only when
// unanswered; the reply goes out through the existing send path).
describe("AskCard wiring (clarifying-question card)", () => {
  const card = fs.readFileSync(path.join(__dirname, "AskCard.tsx"), "utf8");
  const route = fs.readFileSync(
    path.join(__dirname, "../../routes/ask-orchicon.tsx"),
    "utf8",
  );

  it("renders a settled (non-clickable) card when answered", () => {
    expect(card).toContain('data-testid="ask-card"');
    expect(card).toContain("disabled={answered || !onSelect}");
    expect(card).toContain("onSelect?.(o.label)");
  });

  it("names the consent-vs-clarifying distinction in the doc comment", () => {
    expect(card).toMatch(/CONSENT/);
    expect(card).toMatch(/BLOCKING/);
  });

  it("the transcript renders the recorded ask_user call as the card", () => {
    expect(route).toContain(
      'import { AskCard, isAskUserToolCall, parseAskUserArgs } from "@/components/ask/AskCard"',
    );
    expect(route).toContain("isAskUserToolCall(c.functionName)");
    expect(route).toContain("parseAskUserArgs(askCall.arguments)");
    expect(route).toContain("<AskCard");
  });

  it("selecting an option sends it as a normal user message via handleSendMessage", () => {
    expect(route).toContain("onSelectOption={handleSendMessage}");
    // Interactivity is gated on nothing following the assistant message.
    expect(route).toContain("answered={msg.id !== lastMessageId}");
  });
});
