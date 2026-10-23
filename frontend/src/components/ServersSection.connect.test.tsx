// Source-contract test for the Servers page's switch verb (repo idiom: no jsdom and no
// @testing-library in this project, so component contracts are asserted on the rendered
// markup and on the component source — see DiffSidebar.railHandle.test.tsx and AskCard.test.tsx).
//
// WHAT THIS PINS, and why each one is worth a test rather than a comment:
//
//   1. "THERE IS NO WAY TO SWITCH TO A DIFFERENT SERVER" — the operator's words. A `Connect to …`
//      button DID exist, in a second card, and only when more than one plane happened to be RUNNING
//      when the page resolved its ports. So the guard here is not "a button exists" (it always did) but
//      "the button is on the row and is not gated on how many planes are up".
//
//   2. THE FLICKER. The page polled every 1.5s during a setup and called setItems with a fresh array
//      every time, re-rendering identical rows and re-firing the progress bar's width transition. The
//      guard is that a poll result is compared before it is applied.

import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import fs from "node:fs";
import path from "node:path";

import { ServersSection } from "./ServersSection";

const src = fs.readFileSync(path.join(__dirname, "ServersSection.tsx"), "utf8");

/** render is the SSR smoke test: the component still mounts and picks its state without a shell. */
function render() {
  return renderToStaticMarkup(createElement(ServersSection));
}

describe("ServersSection — switching the window's server", () => {
  it("reaches the shell's own switchPlane rather than re-implementing a switch", () => {
    // One implementation. The Plane section already retargets the app (internal/desktop UsePlane via
    // POST /__orchicon_desktop/plane); a second path would be a second way to get it wrong.
    expect(src).toContain('import { desktopInfo, switchPlane } from "@/lib/desktop"');
    expect(src).toContain("await switchPlane(name)");
  });

  it("offers Connect ON THE ROW, not in a separate card", () => {
    // The verb has to be where the list is: that is the surface the operator was looking at when they
    // reported there was no way to switch.
    expect(src).toContain("server-connect-");
    expect(src).toContain("onConnect");
  });

  it("does NOT hide the switch behind the inspection picker's condition", () => {
    // THE REGRESSION. `pickerOffered` answers "is there more than one RUNNING plane to CHOOSE WHICH TO
    // INSPECT" — a different question from "may I point this window at this server". Using it to gate the
    // switch is precisely why a list of servers offered no way to switch to any of them.
    expect(src).not.toContain("pickerOffered");
    // And the row's condition is about WHICH server, not about how many are up.
    expect(src).toContain("{!isCurrent && (");
  });

  it("says which server the window is on, so the list is not ambiguous", () => {
    expect(src).toContain("server-current-");
    expect(src).toContain("this window");
  });

  it("stays put and reports a failed switch instead of reloading over it", () => {
    // A reload after a rejected switch would land on the OLD plane with no trace of the failure — which
    // is indistinguishable from the button doing nothing.
    expect(src).toContain("catch");
    expect(src).toContain("could not connect to");
  });
});

describe("ServersSection — the poll does not redraw what did not change", () => {
  it("compares a poll result before applying it", () => {
    expect(src).toContain("lastSeen");
    expect(src).toContain("JSON.stringify(got)");
    expect(src).toContain("seen !== lastSeen.current");
  });

  it("renders the indeterminate bar as its own element", () => {
    // The flicker was a single element whose className toggled between "determinate at 5%" and
    // "indeterminate, pulse at 100%" while transition-[width] was active — so the bar slammed between
    // full and narrow every time a phase changed whether it could report a percentage.
    expect(src).toContain("server-setup-indeterminate");
    expect(src).not.toContain('setup.percent > 0 ? "" : "animate-pulse"');
  });
});

describe("ServersSection — it still renders", () => {
  it("mounts without a desktop shell and says why there is nothing to manage", () => {
    // No __ORCHICON_DESKTOP__ global in this environment, which is exactly the browser case. The page
    // must explain itself rather than offer buttons that cannot work.
    const html = render();
    expect(html).toContain('data-testid="servers-denied"');
    expect(html).toContain("local to the machine running the desktop app");
  });

  it("is the file this test read", () => {
    // VACUITY. Every assertion above is a substring check; a rename that left `src` empty would make the
    // negative assertions ("not.toContain") pass for the wrong reason.
    expect(src.length).toBeGreaterThan(5000);
    expect(src).toContain("export function ServersSection");
  });
});
