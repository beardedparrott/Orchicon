import { describe, expect, it } from "vitest";
import {
  countMatches,
  filterByQuery,
  matchesQuery,
  searchStatusLabel,
} from "./transcript-search";

// Acceptance 1: case-insensitive substring filtering with a match count, an
// explicit no-match state, and clearing restoring the full transcript.

describe("transcript-search", () => {
  const msgs = [
    { text: "Hello WORLD" },
    { text: "the world is big" },
    { text: "unrelated" },
  ];
  const textOf = (m: { text: string }) => m.text;

  it("matches case-insensitively as a substring", () => {
    expect(matchesQuery("Hello WORLD", "world")).toBe(true);
    expect(matchesQuery("the world is big", "WORLD")).toBe(true);
    expect(matchesQuery("unrelated", "world")).toBe(false);
    expect(matchesQuery("anything", "")).toBe(false);
  });

  it("treats an empty query as no filter", () => {
    expect(filterByQuery(msgs, "", textOf)).toHaveLength(3);
    expect(filterByQuery(msgs, "   ", textOf)).toHaveLength(3);
  });

  it("filters to the matching messages only", () => {
    const got = filterByQuery(msgs, "world", textOf);
    expect(got).toHaveLength(2);
    expect(got.map(textOf)).toEqual(["Hello WORLD", "the world is big"]);
  });

  it("honours keep, so a pinned row survives a text filter", () => {
    const rows = [
      { text: "apple", pinned: false },
      { text: "banana", pinned: true },
    ];
    const got = filterByQuery(
      rows,
      "apple",
      (r) => r.text,
      (r) => r.pinned,
    );
    expect(got.map((r) => r.text)).toEqual(["apple", "banana"]);
  });

  it("counts matches and reports an explicit empty state", () => {
    expect(countMatches(msgs, "world", textOf)).toBe(2);
    expect(countMatches(msgs, "", textOf)).toBe(0);
    expect(searchStatusLabel("world", 2, 3)).toBe("2/3 matches");
    expect(searchStatusLabel("zzz", 0, 3)).toBe("No matches");
    expect(searchStatusLabel("", 0, 3)).toBe("");
  });
});
