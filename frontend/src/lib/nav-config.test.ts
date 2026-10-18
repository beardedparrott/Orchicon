import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import {
  NAV_GROUPS,
  getActiveDomain,
  getActiveItem,
  getBreadcrumbs,
  visibleNavGroups,
} from "./nav-config";

// nav-config.test.ts — AC1 and AC3: the Manage Server entry exists ONCE, in the Control
// group, carrying the desktopOnly marker; a browser NEVER sees it through ANY path
// (groups, active item, active domain, breadcrumbs).

describe("the desktopOnly nav gate", () => {
  it("puts Manage Server in the Control group", () => {
    const control = NAV_GROUPS.find((g) => g.label === "Control");
    expect(control, "the Control group must still exist").toBeTruthy();
    const item = control?.items.find((it) => it.to === "/manage-server");
    expect(item, "Control must carry the Manage Server entry").toBeTruthy();
    expect(item?.label).toBe("Manage Server");
    expect(item?.desktopOnly).toBe(true);
  });

  it("renders no trace of it in a browser — no group, no item, no notice", () => {
    const groups = visibleNavGroups(false);
    for (const group of groups) {
      for (const item of group.items) {
        expect(item.to).not.toBe("/manage-server");
        // Absence means absence: no placeholder, no disabled entry, no "see the
        // desktop app" row smuggled in under another path.
        expect(item.label.toLowerCase()).not.toContain("manage server");
      }
    }
    expect(JSON.stringify(groups)).not.toContain("manage-server");
  });

  it("renders it in the desktop shell, inside Control", () => {
    const groups = visibleNavGroups(true);
    const control = groups.find((g) => g.label === "Control");
    expect(control?.items.some((it) => it.to === "/manage-server")).toBe(true);
  });

  it("keeps the admin filter composing with the desktop filter", () => {
    // The browser view must still show Control (Webhooks/Adapters/Settings), so the
    // filter narrows the group rather than dropping it.
    const control = visibleNavGroups(false).find((g) => g.label === "Control");
    expect(control).toBeTruthy();
    expect(control?.items.length).toBeGreaterThan(0);
  });
});

describe("resolvers cannot resolve a filtered-out item", () => {
  it("resolves Manage Server in the desktop shell", () => {
    expect(getActiveItem("/manage-server", true)?.to).toBe("/manage-server");
    expect(getActiveDomain("/manage-server", true)?.label).toBe("Control");
    expect(getBreadcrumbs("/manage-server", true)).toEqual([
      { label: "Control" },
      { label: "Manage Server", to: "/manage-server" },
    ]);
  });

  it("resolves NOTHING for it in a browser", () => {
    expect(getActiveItem("/manage-server", false)).toBeNull();
    expect(getActiveDomain("/manage-server", false)).toBeNull();
    expect(getBreadcrumbs("/manage-server", false)).toEqual([]);
  });

  it("still resolves ordinary entries in both clients", () => {
    for (const desktop of [false, true]) {
      expect(getActiveItem("/settings", desktop)?.label).toBe("Settings");
      expect(getActiveDomain("/settings", desktop)?.label).toBe("Control");
    }
  });
});

describe("no consumer can bypass the gate", () => {
  it("app-shell enumerates the nav only through visibleNavGroups", () => {
    const src = readFileSync(new URL("../components/app-shell.tsx", import.meta.url), "utf8");
    expect(src).toContain("visibleNavGroups(");
    // The second path this test exists to prevent.
    expect(src).not.toContain("NAV_GROUPS.map(");
  });

  it("breadcrumbs pass the desktop signal through", () => {
    const src = readFileSync(new URL("../components/breadcrumbs.tsx", import.meta.url), "utf8");
    expect(src).toContain("getBreadcrumbs(path, isDesktop())");
  });
});
