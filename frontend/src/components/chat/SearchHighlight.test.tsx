// SearchHighlight.test.tsx — the highlight is asserted by PARSED MARKUP, not a source grep.
//
// The operator: "it doesn't highlight the findings so it makes it rather difficult in a large stream of
// text to find where the found item is on the page. We should add a highlight to the conversation text
// for each item the search finds."
//
// renderToStaticMarkup is the repo's way to assert a component without jsdom (see ActivityLine.test.tsx).
// The component is hook-free, so these are real DOM assertions.
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { SearchHighlight } from "./SearchHighlight";

const html = (text: string, query: string) =>
  renderToStaticMarkup(createElement(SearchHighlight, { text, query }));

describe("SearchHighlight", () => {
  it("marks every occurrence, not just the first", () => {
    // Three real occurrences. (An earlier fixture was wrong rather than the code: "orphan" and "oracle"
    // contain no "orc" — only "orca" does — and the assertion caught the bad fixture, which is what it
    // is for.)
    const out = html("orc in orca and orc again", "orc");
    expect(out.match(/data-testid="transcript-search-hit"/g)?.length).toBe(3);
  });

  it("counts occurrence-wise, not message-wise", () => {
    // The filter counts MATCHING MESSAGES; the highlight is per OCCURRENCE. Both are needed and they are
    // deliberately different numbers.
    const out = html("aaa", "a");
    expect(out.match(/data-testid="transcript-search-hit"/g)?.length).toBe(3);
  });

  it("marks with <mark>, which is what a screen reader announces as a search hit", () => {
    const out = html("the needle is here", "needle");
    expect(out).toContain("<mark");
    expect(out).toContain('data-testid="transcript-search-hit"');
    expect(out).toContain("needle");
  });

  it("PRESERVES THE ORIGINAL CASE of the match", () => {
    // A split on a lowercased copy would render "NEEDLE" as "needle" — the match must be shown as written.
    const out = html("a NEEDLE here", "needle");
    expect(out).toContain("NEEDLE");
    expect(out).not.toContain(">needle<");
  });

  it("keeps the surrounding text exactly, in order", () => {
    const out = html("before NEEDLE after", "needle");
    const flat = out.replace(/<[^>]+>/g, "");
    expect(flat).toBe("before NEEDLE after");
  });

  it("is an EXACT no-op when the query is empty — the default state", () => {
    const out = html("nothing is marked here", "");
    expect(out).not.toContain("<mark");
    expect(out).toBe("nothing is marked here");
  });

  it("is an exact no-op when nothing matches", () => {
    const out = html("nothing is marked here", "zzz");
    expect(out).not.toContain("<mark");
    expect(out).toBe("nothing is marked here");
  });

  it("does not mark a whitespace-only query", () => {
    const out = html("a b c", "   ");
    expect(out).not.toContain("<mark");
  });

  it("handles a match at the very start and very end", () => {
    const out = html("needle in the middle needle", "needle");
    expect(out.match(/data-testid="transcript-search-hit"/g)?.length).toBe(2);
    expect(out.replace(/<[^>]+>/g, "")).toBe("needle in the middle needle");
  });
});
