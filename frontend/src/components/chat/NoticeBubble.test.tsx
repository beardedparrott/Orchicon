import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { NoticeBubble } from "./NoticeBubble";

// The notice is the platform's own record that a conversation's context was collapsed.
// What it must do, and what these pin: say who is speaking, keep the paragraphs (the
// last of which carries the archive path), and never look like the model's reply.

const noticeText =
  "Context compacted to keep this conversation inside the model's window. " +
  "compacted 2343 messages into 1 summary + 7 recent messages.\n\n" +
  "This ran automatically: the conversation reached the model's context window.\n\n" +
  "The pre-collapse transcript was preserved at /var/lib/orchicon/ask-history/x.bak and can be read directly.";

describe("NoticeBubble", () => {
  it("labels itself as a notice, so it cannot be mistaken for the model", () => {
    const html = renderToStaticMarkup(createElement(NoticeBubble, { text: noticeText }));
    expect(html.toLowerCase()).toContain("notice");
  });

  it("keeps every paragraph, including the one carrying the archive path", () => {
    const html = renderToStaticMarkup(createElement(NoticeBubble, { text: noticeText }));
    // One <p> per paragraph (plus the label), rather than one re-flowed block.
    expect(html.match(/<p/g)?.length).toBe(4);
    expect(html).toContain("compacted 2343 messages");
    expect(html).toContain("x.bak");
  });

  it("tolerates a single-paragraph notice and blank input", () => {
    const one = renderToStaticMarkup(createElement(NoticeBubble, { text: "Context compacted." }));
    expect(one).toContain("Context compacted.");

    const empty = renderToStaticMarkup(createElement(NoticeBubble, { text: "" }));
    // The label still renders (it is the shell's identity); no paragraph does.
    expect(empty.match(/<p/g)?.length).toBe(1);
  });
});
