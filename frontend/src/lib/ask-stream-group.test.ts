import { describe, it, expect } from "vitest";
import { groupStreamItems, nextChunkKey, type StreamItem } from "./ask-stream-group";

const t = (text: string, at: number, key: string, phase = "p-0"): StreamItem => ({
  kind: "text",
  text,
  at,
  key,
  phase,
});
const r = (text: string, at: number, key: string, phase = "p-0"): StreamItem => ({
  kind: "reasoning",
  text,
  at,
  key,
  phase,
});

describe("groupStreamItems", () => {
  it("preserves interleaving: text(A) → reasoning(B) → text(C) renders in arrival order", () => {
    const out = groupStreamItems([t("A", 1, "st-1"), r("B", 2, "sr-2"), t("C", 3, "st-3")]);
    expect(out.map((i) => [i.kind, i.text])).toEqual([
      ["text", "A"],
      ["reasoning", "B"],
      ["text", "C"],
    ]);
  });

  it("coalesces only consecutive same-kind chunks", () => {
    const out = groupStreamItems([
      t("A1", 1, "st-1"),
      t("A2", 2, "st-2"),
      r("B1", 3, "sr-3"),
      r("B2", 4, "sr-4"),
    ]);
    expect(out.map((i) => [i.kind, i.text])).toEqual([
      ["text", "A1A2"],
      ["reasoning", "B1B2"],
    ]);
    // Coalesced run keeps the first chunk's key (stable identity).
    expect(out[0].key).toBe("st-1");
    expect(out[1].key).toBe("sr-3");
  });

  it("phase change breaks a run", () => {
    const out = groupStreamItems([t("A", 1, "st-1", "p-0"), t("B", 2, "st-2", "p-1")]);
    expect(out.map((i) => i.text)).toEqual(["A", "B"]);
  });

  it("user/error items break runs and pass through", () => {
    const out = groupStreamItems([
      t("A", 1, "st-1"),
      { kind: "error", text: "boom", at: 2, key: "e-1" },
      t("B", 3, "st-3"),
    ]);
    expect(out.map((i) => [i.kind, i.text])).toEqual([
      ["text", "A"],
      ["error", "boom"],
      ["text", "B"],
    ]);
  });

  it("empty input yields empty output", () => {
    expect(groupStreamItems([])).toEqual([]);
  });
});

describe("nextChunkKey", () => {
  it("builds deterministic sequence keys", () => {
    expect(nextChunkKey("st", 7)).toBe("st-7");
    expect(nextChunkKey("sr", 7)).toBe("sr-7");
  });
});
