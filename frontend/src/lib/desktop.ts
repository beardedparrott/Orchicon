// THE ONE AND ONLY DESKTOP SIGNAL — and the one place plane control is reached.
//
// The shell (internal/desktop/spa.go) injects window.__ORCHICON_DESKTOP__ into
// index.html with `configurable: false`, `writable: false` and Object.freeze, so
// page script can neither define it nor mutate it — the flag cannot be turned on
// from the SPA. In a browser the plane never injects it, so it is `undefined`.
//
// NOTHING ELSE MAY READ THAT GLOBAL. A second detection (window.runtime,
// user-agent sniffing, location.protocol) is a bug: two answers to "am I the
// desktop app?" is exactly how the Manage Server page ends up rendering plane
// controls in a browser.
//
// Three surfaces read this file, and any two of them disagreeing is the leak the
// brief forbids:
//
//   1. the nav gate (components/app-shell.tsx → nav-config.ts) — a `desktopOnly`
//      item is rendered only when this says desktop;
//   2. the /manage-server route (routes/manage-server.tsx:beforeLoad) — a browser
//      hit REDIRECTS to /dashboard (absence, not an explanation);
//   3. the page's plane-control actions — planeControl() below is reachable only
//      when the shell bound it.

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

/** The key the Wails shell installs. Renaming it is a one-file change, here only. */
export const DESKTOP_GLOBAL = "__ORCHICON_DESKTOP__";

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
  const raw = (globalThis as unknown as Record<string, unknown>)[DESKTOP_GLOBAL];
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

/**
 * desktopInstance is the instance the shell is pointed at, or null in a browser.
 *
 * "unknown" is the shell SAYING it could not resolve one (internal/desktop/info.go
 * BuildInfo). That is not a decision: the Manage Server page must ASK which
 * instance, never start "the plane" under a name nobody resolved.
 */
export function desktopInstance(): string | null {
  const instance = desktopInfo()?.instance;
  return instance && instance !== "unknown" ? instance : null;
}

/** portOf parses the control port out of a configured plane URL. 0 when unparseable. */
export function portOf(planeUrl: string): number {
  // A URL host is "host:port"; the port is what follows the LAST colon in the
  // authority, so a scheme's own colon cannot be mistaken for it.
  const m = /^[a-zA-Z][a-zA-Z0-9+.-]*:\/\/([^/?#]*)/.exec(planeUrl);
  const authority = m ? m[1] : planeUrl;
  const port = /:(\d+)$/.exec(authority);
  return port ? Number(port[1]) : 0;
}

/**
 * redirectTarget is the route gate, as a pure function so it is testable in node
 * (importing the route module would pull the whole generated route tree in).
 *
 * DESKTOP-ONLY, BY ABSENCE: in a browser /manage-server REDIRECTS to /dashboard.
 * No notice, no hint, no "use the desktop app" copy — a permanent, context-free
 * disclaimer about a control nobody in a browser is looking for is clutter, not
 * clarity (Plane Lifecycle Control AC4).
 */
export function redirectTarget(pathname: string, desktop: boolean): string | null {
  if (pathname === "/manage-server" || pathname.startsWith("/manage-server/")) {
    return desktop ? null : "/dashboard";
  }
  return null;
}

// ---------------------------------------------------------------------------
// The plane control binding, resolved by PATH rather than by importing generated
// `wailsjs/go/...` files: those do not exist until the shell builds (and never
// exist in the browser bundle or in these tests).
// ---------------------------------------------------------------------------

/** A plane's connection state. FOUR states, and `plane-error` is never `offline`. */
export type PlaneState = "connecting" | "connected" | "offline" | "plane-error";

export type StatusDTO = {
  Instance: string;
  ControlPort: number;
  URL: string;
  State: PlaneState;
  /** What was FOUND: the HTTP status, the drift verdict, or "no answer". */
  Detail: string;
  Residency: string;
  PID: number;
  Binary: string;
  Drift: string;
  Healthy: boolean;
  LogPath: string;
};

export type ResolutionDTO = {
  Instance: string;
  ControlPort: number;
  Known: boolean;
  Running: StatusDTO[];
};

export type ActionDTO = {
  Instance: string;
  Action: string;
  Outcome: string;
  OK: boolean;
  LogPath: string;
  Message: string;
};

/** The bound surface of internal/planebind.Service — the shared control mechanism. */
export type PlaneControl = {
  Instances(): Promise<StatusDTO[]>;
  Status(instance: string): Promise<StatusDTO>;
  ResolveControlPort(port: number): Promise<ResolutionDTO>;
  Start(instance: string): Promise<ActionDTO>;
  Stop(instance: string): Promise<ActionDTO>;
  Restart(instance: string): Promise<ActionDTO>;
};

/** planeControl resolves the Wails binding, or null in a browser / in a test. */
export function planeControl(): PlaneControl | null {
  const g = globalThis as { go?: { planebind?: { Service?: unknown } } };
  const svc = g.go?.planebind?.Service;
  return svc && typeof svc === "object" ? (svc as PlaneControl) : null;
}
