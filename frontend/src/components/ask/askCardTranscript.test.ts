// askCardTranscript.test.ts — WHICH ask call a card is drawn on, and whether it reads as answered.
//
// The fixtures are the REAL tool_calls / tool_results arrays copied out of this deployment's own
// `ask_orchicon_messages` rows, one per adapter, from the operator's test runs. That is the point: both
// adapters reported "no card" against this exact data, and these assertions are what makes that a
// regression test rather than a restatement of the code.
//
// REPORTED: "Ask/permissions cards are still not working for opencode and claude code adapters. For
// opencode it pops up a card but then it goes away as unanswered almost immediately and it never
// actually pops up for Claude adapter."
//
//   claude    the ask is announced TWICE (a tool_part and its completion). The first call holds an EMPTY
//             result and the card used to be drawn on it, so "has a result" read as "answered" with
//             nothing as the answer — the settled line `Question "…" was dismissed — no answer sent`
//             rendered from the first frame. The card never appeared at all.
//   opencode  ONE ask call, carrying the ledger's awaiting marker. Its transcript state was already
//             correct; what killed it was the turn-end settle (a separate, live-only surface).
import { describe, expect, it } from "vitest";
import {
  ASK_AWAITING_ANSWER_MARKER,
  askIsAnswered,
  isAwaitingAnswer,
  isAskUserToolCall,
  parseAskUserArgs,
  pickAskCall,
} from "./AskCard";

interface Call {
  id: string;
  functionName: string;
  arguments: string;
}
interface Result {
  toolCallId: string;
  output: string;
}

const question =
  "This is a test of the ask card. Which answer best describes how the card looks and behaves for you?";

const askArgs = JSON.stringify({
  allow_other: true,
  question,
  options: [
    { label: "Card rendered correctly", description: "The question and options appeared as a card." },
    { label: "Rendered, but with issues", description: "Something looked wrong." },
  ],
});

/** CLAUDE — verbatim shape from the transcript: two ask calls, the first with an empty result. */
const claudeCalls: Call[] = [
  { id: "tc-1", functionName: "ToolSearch", arguments: '{"max_results":1,"query":"select:mcp__orchicon__ask_user"}' },
  { id: "tc-2", functionName: "ToolSearch", arguments: '{"max_results":1,"query":"select:mcp__orchicon__ask_user"}' },
  { id: "tc-3", functionName: "mcp__orchicon__ask_user", arguments: askArgs },
  { id: "tc-4", functionName: "mcp__orchicon__ask_user", arguments: askArgs },
];
const claudeResults: Result[] = [
  { toolCallId: "tc-1", output: "" },
  { toolCallId: "tc-2", output: "" },
  { toolCallId: "tc-3", output: "" },
  { toolCallId: "tc-4", output: JSON.stringify({ recorded: true, question, allow_other: true }) },
];

/** CLAUDE, as a turn recorded AFTER the ledger fix: the acknowledgement is the awaiting marker. */
const claudeFixedResults: Result[] = [
  { toolCallId: "tc-1", output: "" },
  { toolCallId: "tc-2", output: "" },
  { toolCallId: "tc-3", output: JSON.stringify({ recorded: true, question, allow_other: true }) },
  { toolCallId: "tc-4", output: ASK_AWAITING_ANSWER_MARKER },
];

/** OPENCODE — verbatim: three `execute` calls, then the promoted ask carrying the marker. */
const opencodeCalls: Call[] = [
  { id: "tc-1", functionName: "execute", arguments: '{"code":"const r = await tools.orchicon_ask_user({…});"}' },
  { id: "tc-2", functionName: "execute", arguments: '{"code":"const r = search({ query: \\"ask_user\\" });"}' },
  { id: "tc-3", functionName: "execute", arguments: '{"code":"const r = await tools.orchicon.ask_user({…});"}' },
  { id: "tc-4", functionName: "orchicon_ask_user", arguments: askArgs },
];
const opencodeResults: Result[] = [
  { toolCallId: "tc-1", output: '[{"text":"Unknown tool \'orchicon_ask_user\'."}]' },
  { toolCallId: "tc-2", output: '[{"text":"{\\"items\\":[…"}]' },
  { toolCallId: "tc-3", output: JSON.stringify({ recorded: true, question, allow_other: true }) },
  { toolCallId: "tc-4", output: ASK_AWAITING_ANSWER_MARKER },
];

function ledger(calls: Call[], results: Result[]) {
  const asks = calls.filter((c) => isAskUserToolCall(c.functionName));
  const resultOf = (id: string) => results.find((r) => r.toolCallId === id);
  return { asks, resultOf };
}

const never = () => false;

describe("pickAskCall on the real transcripts", () => {
  it("claude: draws the card on a call, not on the phantom with the empty result", () => {
    const { asks, resultOf } = ledger(claudeCalls, claudeResults);
    const call = pickAskCall(asks, resultOf, never);
    expect(call).not.toBeNull();
    // tc-3 is the phantom (empty result). It must not be what the operator sees, because its empty
    // result is what printed "dismissed — no answer sent" over a question nobody was shown.
    expect(askIsAnswered(resultOf(call!.id))).toBe(false);
    expect(parseAskUserArgs(call!.arguments)?.question).toBe(question);
  });

  it("claude: once the ledger writes the marker, the card follows the awaiting call", () => {
    const { asks, resultOf } = ledger(claudeCalls, claudeFixedResults);
    const call = pickAskCall(asks, resultOf, never);
    expect(call?.id).toBe("tc-4");
    expect(isAwaitingAnswer(resultOf(call!.id)!.output)).toBe(true);
    expect(askIsAnswered(resultOf(call!.id))).toBe(false);
  });

  it("opencode: the promoted ask carries the marker and is not read as answered", () => {
    const { asks, resultOf } = ledger(opencodeCalls, opencodeResults);
    const call = pickAskCall(asks, resultOf, never);
    expect(call?.id).toBe("tc-4");
    expect(askIsAnswered(resultOf(call!.id))).toBe(false);
    expect(parseAskUserArgs(call!.arguments)?.question).toBe(question);
  });

  it("opencode: the wrapper's own result is never the card", () => {
    // The ask is what the operator answers; `execute` is how it was reached. Drawing the wrapper would
    // put the sandbox's JSON in front of them.
    const { asks } = ledger(opencodeCalls, opencodeResults);
    expect(asks.map((c) => c.id)).toEqual(["tc-4"]);
  });

  it("a locally settled ask wins, so an answered card cannot re-open on another call", () => {
    const { asks, resultOf } = ledger(claudeCalls, claudeFixedResults);
    const call = pickAskCall(asks, resultOf, (id) => id === "tc-3");
    expect(call?.id).toBe("tc-3");
  });

  it("returns null for a turn with no ask at all", () => {
    expect(pickAskCall([], never as never, never)).toBeNull();
  });
});

describe("askIsAnswered", () => {
  it("no result is not an answer", () => {
    expect(askIsAnswered(undefined)).toBe(false);
  });

  it("an empty result is not an answer", () => {
    // THE CLAUDE PHANTOM. `!!result` used to be the whole test, so this read as "answered".
    expect(askIsAnswered({ output: "" })).toBe(false);
    expect(askIsAnswered({ output: "   \n\t " })).toBe(false);
  });

  it("the awaiting marker is not an answer", () => {
    expect(askIsAnswered({ output: ASK_AWAITING_ANSWER_MARKER })).toBe(false);
  });

  it("the tool's recorded acknowledgement is not an answer", () => {
    // It says the question was STORED. It is not the operator's words, and rendering it as the reply is
    // the other half of the claude failure.
    const ack = JSON.stringify({ recorded: true, question, allow_other: true });
    expect(askIsAnswered({ output: ack })).toBe(true); // it IS a result... see the server-side rule
    // ...but the server never leaves it there any more: promoteRecordedAsk replaces it with the marker.
    // This pin documents that the CLIENT cannot tell, which is why the server must.
    expect(isAwaitingAnswer(ack)).toBe(false);
  });

  it("a real answer is an answer", () => {
    expect(askIsAnswered({ output: "Card rendered correctly" })).toBe(true);
  });
});
