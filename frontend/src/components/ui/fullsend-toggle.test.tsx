import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
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
