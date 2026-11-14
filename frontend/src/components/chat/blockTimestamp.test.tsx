// blockTimestamp.test.tsx — every block in the Ask transcript carries its time, and both bubbles draw it.
//
// The operator: "Is there any way we can also print the timestamps of every block in both the GUI and TUI
// for both user and assistant messages?" The GUI already HAD the instant (every row's createdAt) and the
// assistant bubble already had an `at` prop the route never passed, so this pins the drawing rather than
// the plumbing — and pins the ZONE, which screenkit/format.go requires of the TUI for a documented reason
// ("all times in orch to actually show local time and print the time zone by the time"). The GUI now
// stamps the same way, so the two clients agree about an instant.
import { describe, expect, it } from "vitest";
import { renderToStaticMarkup } from "react-dom/server";
import { UserBubble } from "./UserBubble";
import { AssistantBubble } from "./AssistantBubble";

/** The zone abbreviation the platform reports for this instant (locale-dependent in a browser, so compare
 *  against the same source the component uses rather than a hard-coded "CDT"). */
function expectedZone(at: number): string {
  const parts = new Intl.DateTimeFormat(undefined, { timeZoneName: "short" })
    .formatToParts(new Date(at))
    .find((p) => p.type === "timeZoneName");
  return parts?.value ?? "";
}

const at = new Date(2026, 8, 18, 14, 30, 5).getTime();

/** The clock text this component should produce, asked of the same API it uses. */
function expectedClock(ms: number): string {
  return new Date(ms).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    timeZoneName: "short",
  });
}
const text = (html: string) => html.replace(/<[^>]+>/g, " ").replace(/\s+/g, " ").trim();

describe("transcript block timestamps", () => {
  it("draws the operator's message time, dim, beside the label", () => {
    const html = renderToStaticMarkup(<UserBubble text="hello" at={at} />);
    expect(text(html)).toMatch(/goal/i);
    // LOCALE-INDEPENDENT: the browser decides 12h vs 24h (this host renders "02:30:05 PM"), so assert the
    // instant this formatter produces rather than a hard-coded shape.
    expect(text(html)).toContain(expectedClock(at));
    // Dim + tabular figures, so a column of times reads as a column.
    expect(html).toContain("tabular-nums");
  });

  it("draws the assistant's reply time (the prop the route never passed)", () => {
    const html = renderToStaticMarkup(<AssistantBubble text="reply" at={at} />);
    expect(text(html)).toContain(expectedClock(at));
    expect(html).toContain("tabular-nums");
  });

  it("stamps the ZONE, as the TUI does — a bare clock is what screenkit/format.go exists to prevent", () => {
    const zone = expectedZone(at);
    expect(zone).not.toBe("");
    expect(text(renderToStaticMarkup(<UserBubble text="hello" at={at} />))).toContain(zone);
    expect(text(renderToStaticMarkup(<AssistantBubble text="reply" at={at} />))).toContain(zone);
  });

  it("draws NO time when it has no instant, rather than inventing 1970", () => {
    // An unstamped live fragment, or a row written before timestamps. "12:00:00 AM" would read as fact.
    const html = renderToStaticMarkup(<UserBubble text="hello" />);
    expect(html).not.toMatch(/\d{1,2}:\d{2}:\d{2}/);
    expect(html).not.toContain("1970");
  });

  it("keeps the full instant in the tooltip, so a second-precision clock is still auditable", () => {
    const html = renderToStaticMarkup(<UserBubble text="hello" at={at} />);
    expect(html).toContain("title=");
  });
});
