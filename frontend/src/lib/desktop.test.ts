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

/**
 * THE DESKTOP FLAG IS INJECTED BY THE SHELL AND MUST BE UNFORGEABLE FROM THE
 * PAGE. These tests pin both halves of that: a browser (no global) is never
 * "desktop", and a frozen desktop object survives being read.
 *
 * NO DOM IS USED, DELIBERATELY. This suite runs in vitest's `node` environment
 * (this repo has no jsdom — its component tests render to a string instead), so
 * the flag is defined on `globalThis`. In a browser or a webview `window` IS
 * `globalThis`, so this defines the property on exactly the object the shell's
 * `Object.defineProperty(window, ...)` writes to, and exercises exactly the
 * descriptor it ships.
 */
const KEY = DESKTOP_GLOBAL;
const g = globalThis as unknown as Record<string, unknown>;

function setGlobal(value: unknown) {
  Object.defineProperty(g, KEY, { value, configurable: true, writable: true });
}

afterEach(() => {
  // The forgery test installs a NON-configurable property on purpose, so the
  // delete has to be conditional: `delete` on a non-configurable property throws
  // in module (strict) scope.
  const descriptor = Object.getOwnPropertyDescriptor(g, KEY);
  if (descriptor?.configurable) delete g[KEY];
  delete g.go;
});

describe("desktop signal", () => {
  it("is off in a browser", () => {
    expect(desktopInfo()).toBeNull();
    expect(isDesktop()).toBe(false);
    expect(desktopInstance()).toBeNull();
  });

  it("reads the shell's payload", () => {
    setGlobal(
      Object.freeze({
        desktop: true,
        planeURL: "http://127.0.0.1:8091",
        instance: "prod",
        appTag: "v1.0.0",
        planeTag: "v1.1.0",
        versionMismatch: true,
      }),
    );
    expect(isDesktop()).toBe(true);
    expect(desktopInstance()).toBe("prod");
    expect(desktopInfo()?.versionMismatch).toBe(true);
  });

  it("refuses anything that is not the shell's payload", () => {
    setGlobal({ desktop: false });
    expect(desktopInfo()).toBeNull();
    setGlobal({ desktop: true }); // missing planeURL/instance: a half-truth
    expect(desktopInfo()).toBeNull();
    setGlobal("yes");
    expect(desktopInfo()).toBeNull();
  });

  it("does not treat the shell's 'unknown' instance as a decision", () => {
    // internal/desktop/info.go BuildInfo writes "unknown" when the control port
    // maps to no known instance. The Manage Server page must ASK there, never
    // start a plane under a name nobody resolved.
    setGlobal({ desktop: true, planeURL: "http://127.0.0.1:9999", instance: "unknown" });
    expect(desktopInstance()).toBeNull();
  });

  it("cannot be turned on from the page", () => {
    // A frozen, non-configurable, non-writable property is what the shell ships.
    // Re-defining it must throw, and the value must not change.
    Object.defineProperty(g, KEY, {
      value: Object.freeze({
        desktop: true,
        planeURL: "http://127.0.0.1:8080",
        instance: "dev",
        appTag: "a",
        planeTag: "b",
        versionMismatch: false,
      }),
      configurable: false,
      writable: false,
    });
    expect(() => {
      Object.defineProperty(g, KEY, { value: { desktop: true, planeURL: "http://evil", instance: "prod" } });
    }).toThrow();
    expect(desktopInstance()).toBe("dev");
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

describe("the plane control binding", () => {
  it("is absent in a browser, and the bound service when the shell provides it", () => {
    expect(planeControl()).toBeNull();
    const svc = { Status: () => Promise.resolve() };
    g.go = { planebind: { Service: svc } };
    expect(planeControl()).toBe(svc);
  });
});
