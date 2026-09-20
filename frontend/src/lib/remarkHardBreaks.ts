// remarkHardBreaks — A NEWLINE THE OPERATOR TYPED IS A LINE BREAK.
//
// The operator, on their own messages in both clients:
//
//   "I would like the user sent message to be formatted properly on the screen. Right now it's all just a
//    bunch of text bunched up. It should respect the format that it was typed in, including newlines,
//    bullets, numbered lists, etc."
//
// BULLETS AND NUMBERED LISTS ALREADY WORKED — remark-gfm parses them and markdown.tsx has ul/ol/li
// components. The thing that was missing is the NEWLINE. Under CommonMark a single newline INSIDE a
// paragraph is a SPACE, so a message typed across four lines arrives as one flowing block and is then
// re-wrapped to the pane width. Every line the operator broke is gone.
//
// CommonMark is right for prose the MODEL wrote — that text is machine-wrapped, and joining its lines is what
// lets it reflow to any width. It is wrong for text the OPERATOR wrote, where the line they broke is a line
// they meant. So this is applied to user-authored surfaces only (see markdown.tsx's preserveBreaks prop):
// turning it on globally would re-wrap every assistant answer and every stored description in the product.
//
// IT IS INLINED RATHER THAN DEPENDING ON remark-breaks. The published plugin does exactly this and would be
// the obvious choice, but adding a dependency means a lockfile change and an install that cannot be verified
// from here. The transform is small enough to own outright, and owning it means no version skew between the
// two clients' idea of what a typed newline is (the TUI's md renderer carries the same option —
// md.RenderUserOnSpans).

import type { Node } from "mdast";

// The slice of an mdast node this walk needs. Structural rather than the full mdast union, because the walk
// only ever reads `type`/`value` and rewrites `children` — and because the union's parent types disagree about
// their children's type (Root holds RootContent, Paragraph holds PhrasingContent), which makes a single generic
// assignment impossible without a cast anyway. The cast happens ONCE, at the entry point.
interface Walkable {
  type: string;
  value?: string;
  children?: Walkable[];
}

function walk(node: Walkable): void {
  if (!Array.isArray(node.children)) return;
  const out: Walkable[] = [];
  for (const child of node.children) {
    if (child.type === "text" && typeof child.value === "string" && child.value.includes("\n")) {
      // SPLIT ON THE NEWLINE AND PUT A `break` BETWEEN THE PARTS. A `break` node is mdast's hard break, which
      // react-markdown renders as <br> — the same thing the operator's own Shift+Enter-in-markdown produces.
      const parts = child.value.split("\n");
      parts.forEach((part, i) => {
        if (i > 0) out.push({ type: "break" });
        // EMPTY PARTS ARE DROPPED: a leading/trailing/doubled newline would otherwise add empty text nodes,
        // and a run of blank lines is a paragraph break the parser has already handled.
        if (part !== "") out.push({ type: "text", value: part });
      });
      continue;
    }
    // RECURSE FIRST, so a paragraph nested in a blockquote, a list item or a table cell is reached. Nodes with
    // no children (code, html, inlineCode, thematicBreak) fall out at the guard above — which is what keeps a
    // fenced block's newlines untouched.
    walk(child);
    out.push(child);
  }
  node.children = out;
}

/**
 * softBreaksToHardBreaks rewrites every soft line break in the tree into a hard one. Exported for its own
 * tests — the plugin below is the only production caller.
 */
export function softBreaksToHardBreaks(tree: Node): void {
  walk(tree as unknown as Walkable);
}

/**
 * remarkHardBreaks is a unified/remark plugin: a newline in the source is a line break in the output.
 * Passing the tree as `Node` (rather than mdast's Root) matches remark's Transformer signature exactly, so it
 * slots into react-markdown's remarkPlugins without a cast at the call site.
 */
export function remarkHardBreaks() {
  return (tree: Node): void => {
    softBreaksToHardBreaks(tree);
  };
}
