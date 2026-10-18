import { afterEach, describe, expect, it } from "vitest";
import {
  DESKTOP_GLOBAL,
  desktopInfo,
  desktopInstance,
  isDesktop,
  planeControl,
  portOf,
  redirectTarget,
} from "./desktop";

// desktop.test.ts — the ONE desktop signal, both ways round, in the node environment
// (this suite has no jsdom, which is why the helper reads globalThis rather than
// window).

const g = globalThis as Record<string, unknown>;

afterEach(() => {
  delete g[DESKTOP_GLOBAL];
  delete g.go;
});

describe("the desktop signal", () => {
  it("reports a browser when the shell installed nothing", () => {
    expect(isDesktop()).toBe(false);
    expect(desktopInfo()).toBeNull();
    expect(desktopInstance()).toBeUndefined();
    expect(planeControl()).toBeNull();
  });

  it("reports the shell's info when the global is present", () => {
    g[DESKTOP_GLOBAL] = { planeUrl: "http://localhost:8091", instance: "prod" };
    expect(isDesktop()).toBe(true);
    expect(desktopInfo()).toEqual({ planeUrl: "http://localhost:8091", instance: "prod" });
    expect(desktopInstance()).toBe("prod");
  });

  it("does not invent an instance the shell did not name", () => {
    g[DESKTOP_GLOBAL] = { planeUrl: "http://localhost:8080" };
    expect(desktopInstance()).toBeUndefined();
  });

  it("treats a malformed global as a browser rather than a desktop", () => {
    g[DESKTOP_GLOBAL] = { instance: "dev" };
    expect(isDesktop()).toBe(false);
  });

  it("exposes the Wails binding when it is there, and null when it is not", () => {
    expect(planeControl()).toBeNull();
    const svc = { Status: () => Promise.resolve() };
    g.go = { planebind: { Service: svc } };
    expect(planeControl()).toBe(svc);
  });
});

describe("redirectTarget", () => {
  // AC1: in a BROWSER a direct hit to /manage-server goes to /dashboard — absence,
  // not an explanation.
  it("redirects /manage-server to /dashboard in a browser", () => {
    expect(redirectTarget("/manage-server", false)).toBe("/dashboard");
    expect(redirectTarget("/manage-server/", false)).toBe("/dashboard");
  });

  it("leaves the route alone in the desktop shell", () => {
    expect(redirectTarget("/manage-server", true)).toBeNull();
  });

  it("never redirects any other route", () => {
    for (const path of ["/dashboard", "/settings", "/", "/manage-servers"]) {
      expect(redirectTarget(path, false)).toBeNull();
      expect(redirectTarget(path, true)).toBeNull();
    }
  });
});

describe("portOf", () => {
  it("reads the control port out of the configured plane URL", () => {
    expect(portOf("http://localhost:8080")).toBe(8080);
    expect(portOf("http://localhost:8091")).toBe(8091);
    expect(portOf("https://127.0.0.1:8092/some/path")).toBe(8092);
  });

  it("returns 0 rather than guessing when there is no port", () => {
    expect(portOf("http://localhost")).toBe(0);
    expect(portOf("")).toBe(0);
  });
});
