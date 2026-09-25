import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// SessionGrants — the compact list of this conversation's ACTIVE grants, with
// revoke.
//
// This suite follows the source-scan harness the card tests already use: the
// repo ships only vitest (no jsdom, no @testing-library), and the behaviours
// that matter here are the CONTRACT — the store is the single source of truth
// (the response's refreshed list is written into the cache, never patched
// locally), revoke goes through the RPC, and the empty state says so.
describe("SessionGrants", () => {
  const src = fs.readFileSync(path.join(__dirname, "SessionGrants.tsx"), "utf8");
  const api = fs.readFileSync(
    path.join(__dirname, "../../api/askOrchicon.ts"),
    "utf8",
  );

  it("is a compact disclosure that lists each grant with when it was given", () => {
    expect(src).toContain('data-testid="session-grants-trigger"');
    expect(src).toContain('data-testid="session-grants-panel"');
    expect(src).toContain('data-testid="session-grant-row"');
    expect(src).toContain("relativeGrantAge(Number(g.grantedAtUnix))");
    expect(src).toContain("g.directory");
  });

  it("renders the empty state rather than an empty shell", () => {
    expect(src).toContain('data-testid="session-grants-empty"');
    expect(src).toContain("No session grants for this conversation.");
  });

  it("revokes through the RPC and takes the refreshed list as the truth", () => {
    expect(src).toContain('data-testid="session-grant-revoke"');
    expect(src).toContain("revoke.mutate(g.directory");
    expect(api).toContain("revokePermissionGrant({ conversationId, directory })");
    // The response IS the list: the cache is SET from it, never patched.
    expect(api).toContain("qc.setQueryData(");
    expect(api).toContain("res.grants ?? []");
  });

  it("says a deny entry outranks a grant, and that revoke applies to the next tool call", () => {
    expect(src).toContain("always outranks a grant");
    expect(src).toContain("next tool call");
    // Escape closes the panel (nothing here is pending).
    expect(src).toContain('e.key === "Escape"');
  });

  // The panel is a 320px disclosure anchored `right-0` to a trigger that sits
  // mid-header. On a 375px viewport its left edge lands OFF screen and clips the
  // granted directory — the one thing the list exists to show — so it is pushed
  // back into view by a measured amount (popoverNudge, unit-tested in
  // lib/ask-consent.test.ts).
  it("applies the nudge to the panel's right edge, measured from real geometry", () => {
    expect(src).toContain("popoverNudge(anchor.getBoundingClientRect().right, panel.offsetWidth)");
    expect(src).toContain("style={nudge ? { right: -nudge } : undefined}");
    // The width is also viewport-capped, so a very narrow screen cannot clip.
    expect(src).toContain("max-w-[calc(100vw-1rem)]");
  });

  it("is mounted in the conversation header, keyed to the active conversation", () => {
    const route = fs.readFileSync(
      path.join(__dirname, "../../routes/ask-orchicon.tsx"),
      "utf8",
    );
    expect(route).toContain(
      'import { SessionGrants } from "@/components/ask/SessionGrants"',
    );
    expect(route).toContain("<SessionGrants conversationId={activeConvId");
  });
});
