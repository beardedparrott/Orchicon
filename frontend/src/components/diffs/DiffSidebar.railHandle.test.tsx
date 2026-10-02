// Source-contract test for the rail's resize handle (repo idiom: no jsdom and
// no @testing-library in this project, so component contracts are asserted on
// the rendered markup and on the component source — see BuildLogViewer.test.tsx
// and AskCard.test.tsx).

import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import fs from "node:fs";
import path from "node:path";

import { DiffSidebar } from "./DiffSidebar";

const src = fs.readFileSync(path.join(__dirname, "DiffSidebar.tsx"), "utf8");
const hookSrc = fs.readFileSync(
  path.join(__dirname, "../../lib/diff/useRailResize.ts"),
  "utf8",
);

function render(overrides: Record<string, unknown> = {}) {
  const props = {
    open: true,
    onClose: () => {},
    ownerKind: "execution",
    ownerId: "exec-1",
    tab: "diff",
    onTabChange: () => {},
    selectedPath: "",
    onSelectPath: () => {},
    ...overrides,
  };
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  return renderToStaticMarkup(createElement(DiffSidebar, props as any));
}

describe("DiffSidebar inline rail — resize handle", () => {
  it("wires the handle as an ARIA window splitter with value semantics", () => {
    expect(hookSrc).toContain('role: "separator"');
    expect(hookSrc).toContain('"aria-orientation": "vertical"');
    expect(hookSrc).toContain("aria-valuenow");
    expect(hookSrc).toContain("aria-valuemin");
    expect(hookSrc).toContain("aria-valuemax");
    expect(hookSrc).toContain("tabIndex: 0");
  });

  it("renders the handle with the separator role in the inline rail", () => {
    const html = render();
    expect(html).toContain('role="separator"');
    expect(html).toContain('aria-orientation="vertical"');
    expect(html).toContain('aria-valuemin="320"');
    // Default 480 on an unmeasured container (no layout in this harness).
    expect(html).toContain('aria-valuenow="480"');
    expect(html).toContain("cursor-col-resize");
  });

  it("keeps the rail's tabs and their aria-pressed after the handle is added", () => {
    const html = render();
    for (const label of ["Diff", "Tree", "Timeline"]) {
      expect(html).toContain(`>${label}</button>`);
    }
    expect(html).toContain('aria-pressed="true"');
    // Order: the three tabs, then close, then the resize handle — the handle
    // must NOT be inserted before the tabs.
    const tabs = html.indexOf(">Diff</button>");
    const close = html.indexOf("Close diff sidebar");
    const handle = html.lastIndexOf('role="separator"');
    expect(tabs).toBeGreaterThan(-1);
    expect(close).toBeGreaterThan(tabs);
    expect(handle).toBeGreaterThan(close);
  });

  it("drops the unshrinkable min-w-[480px] so the container clamp can work", () => {
    expect(src).not.toContain("min-w-[480px]");
  });

  it("suspends the width transition while dragging", () => {
    expect(src).toContain('!dragging && "transition-[width] duration-300 ease-in-out"');
  });

  it("puts the width on the rail as an inline style rather than a fighting class", () => {
    expect(src).toContain("style={{ width: effectiveWidth }}");
  });

  it("lets the inner aside follow the outer width (w-full, not a fixed 480)", () => {
    const asideStart = src.lastIndexOf("<aside");
    const aside = src.slice(asideStart, src.indexOf(">", asideStart));
    expect(aside).toContain("w-full");
    expect(aside).not.toContain("w-[480px]");
  });

  it("releases the pointer and clears the drag on up/cancel (no capture leak)", () => {
    expect(hookSrc).toContain("setPointerCapture");
    expect(hookSrc).toContain("releasePointerCapture");
    expect(hookSrc).toContain("onPointerCancel");
    expect(hookSrc).toContain("onLostPointerCapture");
    // The slide transition is gated on this flag, so it must actually flip back.
    expect(hookSrc).toContain("setDragging(false)");
  });
});

describe("DiffSidebar — drawer variant is unregressed", () => {
  it("keeps the drawer's own min(480px,88vw) width", () => {
    expect(src).toContain("w-[min(480px,88vw)]");
  });

  it("gives the drawer no resize handle", () => {
    const drawerStart = src.indexOf("w-[min(480px,88vw)]");
    const drawerEnd = src.indexOf("</div>", src.indexOf("Loading diff…", drawerStart));
    const drawer = src.slice(drawerStart, drawerEnd);
    expect(drawer).not.toContain('role="separator"');
    expect(drawer).not.toContain("handleProps");
  });
});
