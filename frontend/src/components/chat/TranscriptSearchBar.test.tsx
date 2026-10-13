import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { TranscriptSearchBar } from "./TranscriptSearchBar";

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
