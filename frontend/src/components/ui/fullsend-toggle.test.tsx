import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import fs from "node:fs";
import path from "node:path";
import { FullsendToggle } from "./fullsend-toggle";

// FullsendToggle is the GUI's FULLSEND control, rendered immediately LEFT of the conversation
// mode dropdown. What is pinned here is the part that is a SAFETY property rather than a
// styling choice: the operator must be able to tell, at a glance and without hovering, whether
// Orchicon has stopped asking for permission — and must be told what "on" does NOT cover.
const html = (on: boolean) =>
  renderToStaticMarkup(createElement(FullsendToggle, { on, onChange: () => {} }));

describe("FullsendToggle", () => {
  it("is unmistakably ON — the state is in the label AND the accessible name", () => {
    const on = html(true);
    expect(on).toContain("FULLSEND");
    // The accessible name is what a screen reader reports, so it may not be left to the
    // visual treatment.
    expect(on).toContain('aria-label="Fullsend is on"');
    expect(on).toContain('data-fullsend="on"');
    // A filled destructive chip, not a tint.
    expect(on).toContain("bg-destructive");
  });

  it("is quiet when off, and still names the state", () => {
    const off = html(false);
    expect(off).not.toContain("bg-destructive");
    expect(off).toContain('aria-label="Fullsend is off"');
    expect(off).toContain('data-fullsend="off"');
    expect(off).toContain("Fullsend");
  });

  // THE BOUNDARY MUST BE STATED IN THE CONTROL. An operator who reads "stop asking" and then
  // finds their own deny list still refusing has been misled about their policy — the one
  // failure a consent surface must not have.
  it("says what still refuses while it is on", () => {
    const on = html(true);
    expect(on).toContain("deny list");
    expect(on).toContain("never-allow");
    expect(on).toContain("still refuse");
  });

  // The control is a combobox over an explicit pair rather than a switch, so choosing is a
  // decision rather than a nudge — and the OFF option must be present and named.
  it("offers an explicit pair of states", () => {
    const off = html(false);
    expect(off).toContain('role="combobox"');
    expect(off).toContain('aria-haspopup="listbox"');
    // The trigger names the CURRENT state; the menu (which is portalled and only rendered
    // when open) is not part of this markup. Both states are therefore named by the trigger's
    // own label, which is what makes the pair readable without opening it.
    expect(off).toMatch(/Fullsend/);
    expect(html(true)).toMatch(/FULLSEND/);
  });

  it("can be disabled while a turn is streaming", () => {
    const disabled = renderToStaticMarkup(
      createElement(FullsendToggle, { on: true, onChange: () => {}, disabled: true }),
    );
    expect(disabled).toContain("disabled");
  });
});


// THE COMPONENT CANNOT BE DISABLED MID-TURN BY THE PAGE, and this guard is a SOURCE SCAN
// because the fact being protected is a choice made at the CALL SITE, not a behaviour of the
// component — a rendering test can only see the props it was handed, and the bug was the props
// the page handed it.
//
// A `disabled={isStreaming}` sat on this usage. It was mine, copied by reflex from the MODEL
// picker beside it, which genuinely cannot change mid-turn (the running session belongs to the
// model that opened it). Fullsend has no such constraint — the consent layer reads the flag at
// EACH DECISION and the bash guard re-reads it per invocation — and mid-turn is its PRIMARY use
// case: you reach for it when you are already being asked too often, which is exactly when the
// operator found it greyed out.
//
// The repo already reads its own sources from tests for exactly this class of fact
// (AskCard.test.tsx reads the route; internal/tui/chat's parity test reads frontend/src).
describe("FullsendToggle — wiring", () => {
  const routeSrc = fs.readFileSync(
    path.join(__dirname, "../../routes/ask-orchicon.tsx"),
    "utf8",
  );
  const usage = (() => {
    const start = routeSrc.indexOf("<FullsendToggle");
    expect(start).toBeGreaterThan(-1);
    return routeSrc.slice(start, routeSrc.indexOf("/>", start));
  })();

  it("is NOT disabled while a turn is streaming", () => {
    expect(usage).not.toMatch(/disabled=/);
    expect(usage).not.toMatch(/isStreaming/);
  });

  // The operator's placement ask — "a drop down to the left of the ask mode drop down" — and
  // the order also mirrors the TUI's composer row (stats · FULLSEND · mode), so the two clients
  // read the same way.
  it("renders to the LEFT of the mode dropdown", () => {
    const fullsendAt = routeSrc.indexOf("<FullsendToggle");
    const modeAt = routeSrc.indexOf("<ModeToggle");
    expect(fullsendAt).toBeGreaterThan(-1);
    expect(modeAt).toBeGreaterThan(-1);
    expect(fullsendAt).toBeLessThan(modeAt);
  });
});
