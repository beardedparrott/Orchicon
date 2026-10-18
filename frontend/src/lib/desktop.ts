// THE ONE AND ONLY DESKTOP SIGNAL.
//
// The shell (internal/desktop/spa.go) injects window.__ORCHICON_DESKTOP__ into
// index.html with `configurable: false`, `writable: false` and Object.freeze, so
// page script can neither define it nor mutate it — the flag cannot be turned on
// from the SPA. In a browser the plane never injects it, so it is `undefined`.
//
// NOTHING ELSE MAY READ THAT GLOBAL. A second detection (window.runtime,
// user-agent sniffing, location.protocol) is a bug: two answers to "am I the
// desktop app?" is exactly how the Manage Server page ends up rendering plane
// controls in a browser. The Manage Server page calls isDesktop().

export interface DesktopInfo {
  desktop: true;
  /** The plane this window is configured with (NOT the shell's own origin). */
  planeURL: string;
  /** dev / prod / test, resolved from the plane's control port; "unknown" otherwise. */
  instance: string;
  /** The desktop binary's own version (internal/version). */
  appTag: string;
  /** The plane's version, or "unreachable" when it did not answer. */
  planeTag: string;
  /** True only when both tags are known and differ. */
  versionMismatch: boolean;
}

const GLOBAL_KEY = "__ORCHICON_DESKTOP__";

/**
 * desktopInfo returns the shell's payload, or null in a browser.
 *
 * READ THROUGH `globalThis`, NOT `window`: in a browser or a webview the two are
 * the SAME object (the shell's injection lands on `globalThis` because `window`
 * IS `globalThis`), so the lookup is identical — but `globalThis` also exists in
 * the DOM-less environment this repo's vitest suite runs in (there is no jsdom
 * here; component tests render to a string instead), which is what lets the
 * forgery test run at all.
 *
 * The shape is validated rather than trusted: a global of any other shape (an
 * old shell, a page script that got there first) yields null, never a
 * half-truth the Manage Server page could act on.
 */
export function desktopInfo(): DesktopInfo | null {
  const raw = (globalThis as unknown as Record<string, unknown>)[GLOBAL_KEY];
  if (!raw || typeof raw !== "object") return null;
  const candidate = raw as Partial<DesktopInfo>;
  if (candidate.desktop !== true) return null;
  if (typeof candidate.planeURL !== "string" || typeof candidate.instance !== "string") return null;
  return {
    desktop: true,
    planeURL: candidate.planeURL,
    instance: candidate.instance,
    appTag: typeof candidate.appTag === "string" ? candidate.appTag : "",
    planeTag: typeof candidate.planeTag === "string" ? candidate.planeTag : "",
    versionMismatch: candidate.versionMismatch === true,
  };
}

/** isDesktop reports whether this page is running inside the desktop shell. */
export function isDesktop(): boolean {
  return desktopInfo() !== null;
}

/** desktopInstance is the instance the shell is pointed at, or null in a browser. */
export function desktopInstance(): string | null {
  return desktopInfo()?.instance ?? null;
}
