import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// The operator: "When I hit the compact button it just went grey for awhile
// until a message popped up that the compaction had completed. There is no
// indicator that it is actually working."
//
// The button was `disabled` for the whole RPC (compactBlocked reported
// "Working…") and the base Button style dims disabled to 50% opacity — so a
// compaction that legitimately runs for tens of seconds (server-side it runs a
// summarize MODEL CALL) looked identical to a broken button. Its only feedback
// was the completion toast, i.e. AFTER the wait.
//
// The fix reports progress DURING the wait:
//   - a spinner + "Compacting" label + an elapsed timer on the control;
//   - aria-busy bound to the in-flight state;
//   - the busy state restored to full opacity so "working" never reads as
//     "dead";
//   - driven by the MUTATION's own lifecycle, so it cannot stick.
//
// A note on why there is no percentage here: CompactConversation is a UNARY
// RPC. It performs a deterministic reduce stage and then one model call, and
// streams no intermediate progress. An elapsed timer is the only honest
// indicator; a progress bar would imply a measurement the server never sends.

describe("ask-orchicon compact control (in-flight progress)", () => {
  const src = fs.readFileSync(path.join(__dirname, "ask-orchicon.tsx"), "utf8");

  // Extract just the compact <Button> so the assertions below cannot be
  // satisfied by an unrelated control elsewhere in this very large route.
  function compactButtonSource(): string {
    const start = src.indexOf('data-testid="ask-compact"');
    if (start < 0) throw new Error("compact button not found");
    const open = src.lastIndexOf("<Button", start);
    const close = src.indexOf("</Button>", start);
    return src.slice(open, close);
  }

  it("shows a spinner and a live elapsed timer while compacting", () => {
    const btn = compactButtonSource();
    // The spinner (house convention: lucide Loader2 + animate-spin).
    expect(btn).toContain("Loader2");
    expect(btn).toContain("animate-spin");
    expect(btn).toContain("Compacting");
    // The one progress fact the client actually has: how long it has been
    // running. Reused from the ui kit rather than re-implemented.
    expect(btn).toContain("LiveDuration");
    expect(btn).toContain("startedAt={compactStartedAt}");
    expect(src).toContain(
      'import { LiveDuration } from "@/components/ui/live-duration";'
    );
  });

  it("binds aria-busy to the in-flight state (a11y + a stable test hook)", () => {
    const btn = compactButtonSource();
    expect(btn).toContain("aria-busy={compacting}");
    expect(btn).toContain('data-compacting={compacting ? "true" : "false"}');
  });

  it("keeps the running control at full opacity instead of a dead grey", () => {
    const btn = compactButtonSource();
    // The base Button variant dims disabled controls to 50%; the busy state
    // supersedes it (cn is tailwind-merge, so the later class wins) and tints
    // the label so "working" is unmistakable.
    expect(btn).toContain("disabled:opacity-100");
    expect(src).toMatch(
      /compacting\s*\n?\s*\?\s*"disabled:opacity-100 text-cyan-600 dark:text-cyan-400"/
    );
  });

  it("drives the indicator from the mutation lifecycle, so it cannot stick", () => {
    // A hand-rolled boolean would stay true if the mutation threw past its
    // reset. isPending settles with the mutation by construction, and the flag
    // is threaded from the page (where the mutation lives) into the composer.
    expect(src).toContain("compacting={compactConv.isPending}");
    expect(src).toContain("compacting?: boolean;");
    expect(src).toContain("compacting = false,");
    // And the elapsed clock is bracketed around the await: set before, cleared
    // in the finally.
    expect(src).toContain("setCompactStartedAt(Date.now());");
    expect(src).toContain("setCompactStartedAt(null);");
    expect(src).toMatch(/finally \{\n\s*setSending\(false\);\n\s*setCompactStartedAt\(null\);/);
  });

  it("no longer reports a RUNNING compaction as a refusal reason", () => {
    // Regression: "Working…" in compactBlocked is what rendered the mute grey
    // button. It must now be guarded so it can never cover the busy state.
    expect(src).toContain(': !compacting && sending');
    const btn = compactButtonSource();
    // And the tooltip explains WHY the wait can be long, rather than showing
    // the generic invitation-to-click.
    expect(btn).toContain("the server is summarizing this conversation");
    expect(btn).toContain("runs a model call");
  });

  it("stays disabled while compacting, so it can never double-fire", () => {
    // A second fire would race the server-side rewrite of the same history.
    // Progress rendering must NOT be achieved by re-enabling the control.
    expect(src).toContain(
      'const compactDisabled = compacting || compactBlocked !== "";'
    );
    const btn = compactButtonSource();
    expect(btn).toContain("disabled={compactDisabled}");
  });

  it("keeps the pre-click refusals and the post-click verdict intact", () => {
    // The two conditions runCompact refuses on are still surfaced BEFORE the
    // click (the reason on the control beats a toast after it).
    expect(src).toContain('"Open a conversation first"');
    expect(src).toContain('"A turn is in flight — stop it before compacting"');
    // A DECLINE is still an info toast, never a success.
    expect(src).toContain('kind: res.compacted ? "success" : "info"');
  });
});
