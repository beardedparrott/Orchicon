import fs from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";

// ask-poll-focus.test.ts — THE ASK POLLERS MUST SURVIVE AN UNFOCUSED TAB.
//
// The operator, mid-session: "every time a card was passed, I had to hit refresh on the browser for it to
// appear." A page reload was the only thing that showed the card.
//
// THE MECHANISM, and why it is asserted here rather than left to a reader. TanStack defaults
// `refetchIntervalInBackground` to FALSE, which PAUSES an interval poller whenever its window is not
// focused — and this app sets `refetchOnWindowFocus: false` GLOBALLY (main.tsx). So a poller did not run
// while the operator was in another window AND did not catch up on return: the refresh they pressed was the
// only thing that re-ran the query. A card is an operator-blocking wait, so it is the one case where
// reaching someone who is NOT looking at the tab is the entire point.
//
// A source guard is the right instrument because the option is invisible at the call site in the route: a
// future edit that drops it would leave every unit test green while the bug returned exactly as reported.
// This is the same idiom ask-transcript-poll.test.ts already uses for the same reason.

describe("the Ask pollers keep running while the tab is unfocused", () => {
  const src = fs.readFileSync(path.join(__dirname, "..", "api", "askOrchicon.ts"), "utf8");

  const hookBody = (name: string): string => {
    const at = src.indexOf(`export function ${name}(`);
    expect(at, `${name} not found in askOrchicon.ts`).toBeGreaterThan(-1);
    // Up to the next top-level export — enough to cover this hook's useQuery options.
    const next = src.indexOf("\nexport function ", at + 1);
    return src.slice(at, next === -1 ? undefined : next);
  };

  it("the CARD discovery read opts in — this is the reported one", () => {
    const body = hookBody("usePendingAsks");
    expect(body).toContain("refetchIntervalInBackground: (opts?.refetchInterval ?? false) !== false");
    expect(body).toContain("refetchOnWindowFocus: true");
  });

  it("the transcript page opts in — the reply froze the same way", () => {
    const body = hookBody("useListMessages");
    expect(body).toContain("refetchIntervalInBackground: (opts?.refetchInterval ?? false) !== false");
    expect(body).toContain("refetchOnWindowFocus: true");
  });

  it("the conversation list opts in — running markers and Stop must survive a hidden tab", () => {
    const body = hookBody("useListConversations");
    expect(body).toContain("refetchIntervalInBackground: (opts?.refetchInterval ?? false) !== false");
  });

  it("stays OFF when the caller passes no cadence, so an idle app does not poll in the background", () => {
    // The gate is the cadence itself: `false !== false` is false, so a query the caller did not ask to
    // poll does not start polling merely because the tab went to the background.
    const body = hookBody("usePendingAsks");
    expect(body).toContain("refetchInterval: opts?.refetchInterval ?? false");
  });
});
