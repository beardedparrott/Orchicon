import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { TranscriptSearchBar, transcriptSearchKey } from "./TranscriptSearchBar";

// Acceptance 1 (the toolbar half): a visible match count, an explicit no-match
// state, and prev/next affordances. Rendered server-side (the repo's component
// test pattern), so this asserts the markup the operator sees.

const noop = () => {};

function render(props: {
  query: string;
  matchCount: number;
  total: number;
}): string {
  return renderToStaticMarkup(
    createElement(TranscriptSearchBar, {
      ...props,
      onQueryChange: noop,
      onNext: noop,
      onPrev: noop,
      onClear: noop,
    }),
  );
}

describe("TranscriptSearchBar", () => {
  it("always offers the search field", () => {
    expect(render({ query: "", matchCount: 0, total: 9 })).toContain(
      'aria-label="Search transcript"',
    );
  });

  it("shows no status or stepper until a query is typed", () => {
    const html = render({ query: "", matchCount: 0, total: 9 });
    expect(html).not.toContain("matches");
    expect(html).not.toContain("Previous match");
  });

  it("shows the match count and prev/next for a matching query", () => {
    const html = render({ query: "world", matchCount: 3, total: 9 });
    expect(html).toContain("3/9 matches");
    expect(html).toContain("Previous match");
    expect(html).toContain("Next match");
    expect(html).toContain("Clear search");
  });

  it("shows an explicit empty state when nothing matches", () => {
    const html = render({ query: "zzz", matchCount: 0, total: 9 });
    expect(html).toContain("No matches");
  });
});

// Acceptance 4 (the field's keyboard half). transcriptSearchKey is the single
// source of truth for which keys the search field consumes; it is pure so the
// contract is asserted without a DOM (the repo has no jsdom/@testing-library).
describe("transcriptSearchKey", () => {
  it("clears on Escape, and steps next on Enter / previous on Shift+Enter", () => {
    expect(transcriptSearchKey("Escape", false, 3)).toBe("clear");
    expect(transcriptSearchKey("Enter", false, 3)).toBe("next");
    expect(transcriptSearchKey("Enter", true, 3)).toBe("prev");
  });

  it("is a NO-OP on Enter / Shift+Enter when nothing matches", () => {
    // The buttons are disabled on an empty match set; the keyboard must respect
    // the same state rather than stepping nothing.
    expect(transcriptSearchKey("Enter", false, 0)).toBeNull();
    expect(transcriptSearchKey("Enter", true, 0)).toBeNull();
  });

  it("still clears on Escape even with no matches (only stepping is gated)", () => {
    expect(transcriptSearchKey("Escape", false, 0)).toBe("clear");
  });

  it("ignores every other key, so ordinary typing just filters", () => {
    expect(transcriptSearchKey("a", false, 3)).toBeNull();
    expect(transcriptSearchKey("Tab", false, 3)).toBeNull();
    expect(transcriptSearchKey("ArrowDown", false, 3)).toBeNull();
  });
});
