import { describe, expect, it } from "vitest";
import { groupByPhase, groupPhaseGroups, mergeSessionItems, type ChatItem } from "./sessionItems";

const text = (key: string, t: string, at: number, live?: boolean, phase?: string): ChatItem => ({
  kind: "text",
  text: t,
  at,
  key,
  live,
  phase,
});
const reasoning = (key: string, t: string, at: number, live?: boolean, phase?: string): ChatItem => ({
  kind: "reasoning",
  text: t,
  at,
  key,
  live,
  phase,
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

  it("merges interleaved live reasoning/text chunks into ONE reasoning bubble and ONE text bubble", () => {
    const merged = mergeSessionItems([], [
      reasoning("r1", "think ", 100, true),
      text("l1", "answer ", 101, true),
      reasoning("r2", "harder", 102, true),
    ]);
    expect(merged).toHaveLength(2);
    // First-appearance order: reasoning (r1) arrived before the text chunk.
    expect(merged[0].kind).toBe("reasoning");
    expect((merged[0] as { text: string }).text).toBe("think harder");
    expect(merged[1].kind).toBe("text");
    expect((merged[1] as { text: string }).text).toBe("answer ");
  });

  it("seals reasoning bubbles on a tool boundary", () => {
    const merged = mergeSessionItems([], [
      reasoning("r1", "think about input ", 100, true),
      tool("t2", "bash", 200),
      reasoning("r3", "think about result", 300, true),
    ]);
    expect(merged).toHaveLength(3);
    expect(merged[0].kind).toBe("reasoning");
    expect((merged[0] as { text: string }).text).toBe("think about input ");
    expect(merged[1].kind).toBe("tool");
    expect(merged[2].kind).toBe("reasoning");
    expect((merged[2] as { text: string }).text).toBe("think about result");
  });

  it("groups live reasoning chunks even without the live flag (latent bug)", () => {
    // liveItems used to omit `live` on reasoning items; grouping must not
    // depend on the flag.
    const merged = mergeSessionItems([], [
      reasoning("r1", "think ", 100),
      reasoning("r2", "deeper", 101),
    ]);
    expect(merged).toHaveLength(1);
    expect(merged[0].kind).toBe("reasoning");
    expect((merged[0] as { text: string }).text).toBe("think deeper");
  });

  it("merges history reasoning parts of one step into one bubble", () => {
    // The terminal view renders the transcript alone — per-chunk reasoning
    // parts of the same step must converge into ONE bubble like the live
    // view.
    const merged = mergeSessionItems(
      [
        reasoning("r1", "think ", 100, false, "step-1"),
        reasoning("r2", "deeper", 101, false, "step-1"),
      ],
      [],
    );
    expect(merged).toHaveLength(1);
    expect(merged[0].kind).toBe("reasoning");
    expect((merged[0] as { text: string }).text).toBe("think deeper");
  });

  it("keeps history and live phases distinct while preserving dedupe", () => {
    // Phase keys are the grouping boundary: history text (step-*) and a
    // not-yet-flushed live chunk (live-*) of the same step must NOT merge
    // into one bubble, and covered live chunks are still dropped.
    const history = [text("t1", "already said", 500, false, "step-1")];
    const live = [
      text("l1", "covered ", 400, true, "live-1"), // covered by history → dropped
      text("l2", "new chunk", 600, true, "live-1"), // newer → kept
    ];
    const merged = mergeSessionItems(history, live);
    expect(merged).toHaveLength(2);
    expect(merged[1].kind).toBe("text");
    expect((merged[1] as { text: string }).text).toBe("new chunk");
  });

  it("does not merge across live phases (tool boundary between chunks)", () => {
    const merged = mergeSessionItems([], [
      reasoning("r1", "first think ", 100, true, "live-0"),
      tool("t2", "bash", 200),
      reasoning("r3", "second think", 300, true, "live-1"),
    ]);
    expect(merged).toHaveLength(3);
    expect((merged[0] as { text: string }).text).toBe("first think ");
    expect((merged[2] as { text: string }).text).toBe("second think");
  });

  it("still groups consecutive live text and keeps one text bubble when interleaved with reasoning", () => {
    const merged = mergeSessionItems([], [
      text("l1", "The quick ", 100, true),
      reasoning("r1", "hmm ", 101, true),
      text("l2", "brown fox", 102, true),
    ]);
    const texts = merged.filter((i) => i.kind === "text");
    expect(texts).toHaveLength(1);
    expect((texts[0] as { text: string }).text).toBe("The quick brown fox");
    const reasonings = merged.filter((i) => i.kind === "reasoning");
    expect(reasonings).toHaveLength(1);
    expect((reasonings[0] as { text: string }).text).toBe("hmm ");
  });

  it("groupPhaseGroups splits per (kind, phase) and keeps first-appearance order", () => {
    const groups = groupPhaseGroups([
      { kind: "reasoning", text: "a", phase: "live-1" },
      { kind: "text", text: "b", phase: "live-1" },
      { kind: "text", text: "c", phase: "live-1" },
      { kind: "reasoning", text: "d", phase: "live-2" },
    ]);
    expect(groups.map((g) => g.map((x) => x.kind))).toEqual([
      ["reasoning"],
      ["text", "text"],
      ["reasoning"],
    ]);
  });

  it("groupPhaseGroups absorbs artifact-kind items into the open text group", () => {
    const groups = groupPhaseGroups(
      [
        { kind: "text", text: "a", phase: "live-1" },
        { kind: "artifact", name: "f", type: "text", content: "x", at: 1, key: "a1", phase: "live-1" },
        { kind: "text", text: "b", phase: "live-1" },
      ],
      new Set(["artifact"]),
    );
    expect(groups).toHaveLength(1);
    expect(groups[0].map((x) => x.kind)).toEqual(["text", "artifact", "text"]);
  });

  it("groupByPhase is idempotent over already-grouped phase-tagged items", () => {
    const items = mergeSessionItems([], [
      reasoning("r1", "think ", 100, true, "live-0"),
      reasoning("r2", "deeper", 101, true, "live-0"),
    ]);
    expect(items).toHaveLength(1);
    const again = groupByPhase(items);
    expect(again).toHaveLength(1);
    expect((again[0] as { text: string }).text).toBe("think deeper");
  });
});
