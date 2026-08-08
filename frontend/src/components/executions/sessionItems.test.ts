import { describe, expect, it } from "vitest";
import { mergeSessionItems, type ChatItem } from "./sessionItems";

const text = (key: string, t: string, at: number, live?: boolean): ChatItem => ({
  kind: "text",
  text: t,
  at,
  key,
  live,
});
const reasoning = (key: string, t: string, at: number, live?: boolean): ChatItem => ({
  kind: "reasoning",
  text: t,
  at,
  key,
  live,
});
const tool = (key: string, toolName: string, at: number): ChatItem => ({
  kind: "tool",
  tool: { id: key, toolName, input: "", output: "", at },
  key,
});

describe("mergeSessionItems", () => {
  it("renders full history when live is empty (joining mid-run)", () => {
    const history = [
      text("t1", "hello", 100),
      tool("t2", "bash", 200),
      text("t3", "world", 300),
    ];
    const merged = mergeSessionItems(history, []);
    expect(merged).toHaveLength(3);
    expect(merged[0].key).toBe("t1");
    expect(merged[2].key).toBe("t3");
  });

  it("keeps live events newer than the transcript and drops covered ones", () => {
    const history = [text("t1", "already said", 500)];
    const live = [
      text("l1", "old chunk ", 400, true), // covered by history (at < 500) → dropped
      text("l2", "new chunk", 600, true), // newer → kept
    ];
    const merged = mergeSessionItems(history, live);
    expect(merged).toHaveLength(2); // history + only the newer live chunk
    expect(merged[1].kind).toBe("text");
    expect((merged[1] as { text: string }).text).toBe("new chunk");
  });

  it("groups consecutive live text chunks into one growing bubble", () => {
    const merged = mergeSessionItems([], [
      text("l1", "The quick ", 100, true),
      text("l2", "brown fox ", 101, true),
      text("l3", "jumps", 102, true),
    ]);
    expect(merged).toHaveLength(1);
    expect(merged[0].kind).toBe("text");
    expect((merged[0] as { text: string }).text).toBe("The quick brown fox jumps");
  });

  it("groups consecutive live reasoning chunks into one reasoning bubble", () => {
    const merged = mergeSessionItems([], [
      reasoning("r1", "thinking ", 100, true),
      reasoning("r2", "deeper", 101, true),
    ]);
    expect(merged).toHaveLength(1);
    expect(merged[0].kind).toBe("reasoning");
    expect((merged[0] as { text: string }).text).toBe("thinking deeper");
  });

  it("does not merge across kinds (reasoning then text)", () => {
    const merged = mergeSessionItems([], [
      reasoning("r1", "think", 100, true),
      text("l1", "answer", 200, true),
    ]);
    expect(merged).toHaveLength(2);
    expect(merged[0].kind).toBe("reasoning");
    expect(merged[1].kind).toBe("text");
  });

  it("interleaves history and fresh live items chronologically", () => {
    const history = [text("t1", "earlier", 100), tool("t2", "bash", 200)];
    const live = [text("l1", "streaming", 300, true)];
    const merged = mergeSessionItems(history, live);
    expect(merged.map((i) => i.kind)).toEqual(["text", "tool", "text"]);
    const last = merged[2];
    expect(last.kind).toBe("text");
    expect((last as { text: string }).text).toBe("streaming");
  });
});
