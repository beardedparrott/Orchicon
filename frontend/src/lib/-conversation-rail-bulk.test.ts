// -conversation-rail-bulk.test.ts
//
// THE GAP THIS PINS. The conversations rail could be scoped by project and its rows deleted one at a
// time. It had no text search, no selection, and no bulk verb — so "delete every conversation named
// like this" was a click-per-row exercise, and there was no way to act on a SET at all.
//
// The pure parts of that are testable without rendering: what a query matches, and — the part that
// actually matters — that a bulk action acts on the SELECTION INTERSECTED WITH WHAT IS VISIBLE, never on
// a set that could include conversations the filter has hidden.

import { describe, expect, it } from "vitest";
import {
  conversationMatchesSearch,
  filterConversationsBySearch,
  ALL_PROJECTS,
  filterConversationsByScope,
  conversationDisplayTitle,
} from "./conversationProjects";

interface RailConv {
  id: string;
  title?: string;
  lastMessagePreview?: string;
  projectId?: string;
}

/** visibleSelection mirrors the rail's `visibleSelectedConvIds`: selection ∩ what the rail is showing. */
function visibleSelection(
  convs: RailConv[],
  projectScope: string,
  query: string,
  selected: ReadonlySet<string>,
): string[] {
  const scoped = filterConversationsByScope(convs, projectScope);
  const visible = filterConversationsBySearch(scoped, query);
  return visible.map((c) => c.id).filter((id) => selected.has(id));
}

const convs: RailConv[] = [
  { id: "c1", title: "junk scratch", projectId: "p1" },
  { id: "c2", title: "junk test", projectId: "p1" },
  { id: "c3", title: "keep this", projectId: "p1" },
  { id: "c4", title: "junk elsewhere", projectId: "p2" },
];

describe("the rail's search", () => {
  it("narrows by title so a bulk action can target a naming pattern", () => {
    const got = filterConversationsBySearch(convs, "junk").map((c) => c.id);
    expect(got).toEqual(["c1", "c2", "c4"]);
  });

  it("is case-insensitive and matches fragments", () => {
    expect(conversationMatchesSearch({ id: "x", title: "Weatherapp" }, "WEATHER")).toBe(true);
  });

  it("shows everything when the box is empty", () => {
    expect(filterConversationsBySearch(convs, "")).toHaveLength(4);
  });
});

describe("bulk actions act on the selection ∩ what is visible", () => {
  it("deletes only the matched-and-selected rows", () => {
    // The operator searched "junk", selected all (c1, c2, c4 — every match in view) and hits Delete.
    const selected = new Set(["c1", "c2", "c4"]);
    const victims = visibleSelection(convs, ALL_PROJECTS, "junk", selected);
    expect(victims).toEqual(["c1", "c2", "c4"]);
    // "keep this" was never matched, so it can never be swept up by the gesture.
    expect(victims).not.toContain("c3");
  });

  it("cannot reach a conversation the project scope has hidden", () => {
    // c4 matches "junk" but lives in p2. Scoped to p1 it must not be deletable, even if it was
    // selected before the scope changed — that is the failure this rule exists to prevent.
    const selected = new Set(["c1", "c2", "c3", "c4"]);
    const victims = visibleSelection(convs, "p1", "junk", selected);
    expect(victims).toEqual(["c1", "c2"]);
    expect(victims).not.toContain("c4");
  });

  it("cannot reach a row the search has filtered out", () => {
    // Selecting everything first, THEN searching, must not let a bulk delete take the hidden rows.
    const selected = new Set(["c1", "c2", "c3", "c4"]);
    const victims = visibleSelection(convs, ALL_PROJECTS, "keep", selected);
    expect(victims).toEqual(["c3"]);
  });

  it("returns nothing when the selection is empty, so no bulk action runs by accident", () => {
    expect(visibleSelection(convs, ALL_PROJECTS, "junk", new Set())).toEqual([]);
  });
});

describe("select-all-of-what-is-shown", () => {
  it("selects exactly the filtered set", () => {
    const visible = filterConversationsBySearch(
      filterConversationsByScope(convs, ALL_PROJECTS),
      "junk",
    );
    expect(visible.map((c) => c.id)).toEqual(["c1", "c2", "c4"]);
  });
});

describe("empty titles are searchable by the label the rail draws", () => {
  // THE REPORTED BUG. A large share of conversations in a real tenant have an EMPTY stored title, and
  // the rail draws those as "New conversation". A search for that visible text matched NONE of them, so
  // the filter looked broken for exactly the rows an operator was trying to clean up.
  const untitled = [
    { id: "u1", title: "", lastMessagePreview: "" },
    { id: "u2", title: "", lastMessagePreview: "something unrelated" },
    { id: "u3", title: "   ", lastMessagePreview: "" },
    { id: "n1", title: "MCP token fix", lastMessagePreview: "" },
  ];

  it("matches an empty title on the placeholder the rail shows", () => {
    const got = filterConversationsBySearch(untitled, "new").map((c) => c.id);
    expect(got).toEqual(["u1", "u2", "u3"]);
  });

  it("matches the full placeholder phrase", () => {
    const got = filterConversationsBySearch(untitled, "new conversation").map((c) => c.id);
    expect(got).toEqual(["u1", "u2", "u3"]);
  });

  it("still matches titled conversations and does not sweep them into the placeholder set", () => {
    const got = filterConversationsBySearch(untitled, "token").map((c) => c.id);
    expect(got).toEqual(["n1"]);
  });

  it("the display label is the one the rail renders", () => {
    expect(conversationDisplayTitle({ title: "" })).toBe("New conversation");
    expect(conversationDisplayTitle({ title: "   " })).toBe("New conversation");
    expect(conversationDisplayTitle({ title: "Real name" })).toBe("Real name");
  });
});
