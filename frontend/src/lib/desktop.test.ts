import { afterEach, describe, expect, it } from "vitest";
import { desktopInfo, desktopInstance, isDesktop } from "./desktop";

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
const KEY = "__ORCHICON_DESKTOP__";
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
