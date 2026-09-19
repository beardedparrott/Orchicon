import { describe, expect, it } from "vitest";

import type { Node } from "mdast";

import { softBreaksToHardBreaks } from "./remarkHardBreaks";

// The operator: "It should respect the format that it was typed in, including newlines, bullets, numbered
// lists, etc." These pin the newline half — the half that was missing.

type N = { type: string; value?: string; children?: N[] };

function para(text: string): N {
  return { type: "paragraph", children: [{ type: "text", value: text }] };
}

function root(...children: N[]): N {
  return { type: "root", children };
}

function run(tree: N): N {
  softBreaksToHardBreaks(tree as unknown as Node);
  return tree;
}

function paraChildren(tree: N): N[] | undefined {
  return tree.children?.[0].children;
}

describe("softBreaksToHardBreaks", () => {
  it("turns a typed newline into a hard break", () => {
    const tree = run(root(para("first line\nsecond line")));
    expect(paraChildren(tree)).toEqual([
      { type: "text", value: "first line" },
      { type: "break" },
      { type: "text", value: "second line" },
    ]);
  });

  it("handles several lines in one message", () => {
    const tree = run(root(para("one\ntwo\nthree")));
    expect(paraChildren(tree)?.map((n) => n.type)).toEqual([
      "text",
      "break",
      "text",
      "break",
      "text",
    ]);
  });

  it("leaves text with no newline completely alone", () => {
    const text = { type: "text", value: "one line only" };
    const tree = run(root({ type: "paragraph", children: [text] }));
    // THE SAME NODE, not an equivalent one: a transform that rebuilt every text node would churn the tree
    // (and any position data on it) for a message that needed nothing.
    expect(paraChildren(tree)?.[0]).toBe(text);
  });

  it("does not invent empty text nodes around a newline", () => {
    const tree = run(root(para("a\n")));
    expect(paraChildren(tree)).toEqual([{ type: "text", value: "a" }, { type: "break" }]);
  });

  it("reaches a paragraph nested in a blockquote or a list item", () => {
    const quoted = run(root({ type: "blockquote", children: [para("x\ny")] }));
    expect(quoted.children?.[0].children?.[0].children?.map((n) => n.type)).toEqual([
      "text",
      "break",
      "text",
    ]);

    const item = run(root({ type: "listItem", children: [para("p\nq")] }));
    expect(item.children?.[0].children?.[0].children?.map((n) => n.type)).toEqual([
      "text",
      "break",
      "text",
    ]);
  });

  it("does not touch a fenced code block's newlines", () => {
    const code = { type: "code", value: "line a\nline b\nline c" };
    const tree = run(root(code));
    // A `code` node carries its body in `value` and has no children, so the walk never descends into it. A
    // transform that split code on newlines would shred every pasted block the operator sent.
    expect(tree.children?.[0]).toBe(code);
    expect(tree.children?.[0].value).toBe("line a\nline b\nline c");
  });
});
