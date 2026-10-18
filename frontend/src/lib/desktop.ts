// desktop.ts — THE ONE place the SPA decides "am I the desktop shell".
//
// A SECOND, AD-HOC DETECTION ANYWHERE IS A BUG. Three surfaces read this file:
//
//   1. the nav gate (lib/nav-config.ts:visibleNavGroups) — a `desktopOnly` item
//      is rendered only when this says desktop;
//   2. the /manage-server route (routes/manage-server.tsx:beforeLoad) — a browser
//      hit REDIRECTS to /dashboard (absence, not an explanation);
//   3. the page's plane-control actions — the binding is reachable only here.
//
// Any two of them disagreeing is exactly how the Manage Server entry leaks into
// the browser, which is the thing the brief forbids.
//
// The signal is one frozen global installed by the Wails shell
// (`globalThis.__ORCHICON_DESKTOP__ = { planeUrl, instance? }`,
// writable:false, configurable:false). It is read through a CAST rather than a
// declared `window` field: vitest runs this suite in the NODE environment (no
// jsdom), so touching `window` would throw instead of returning "not desktop".

export type DesktopInfo = {
  /** The configured plane URL, e.g. "http://localhost:8080". */
  planeUrl: string;
  /** The instance the shell was configured with, when it knew one. */
  instance?: string;
};

/** The key the Wails shell installs. Renaming it is a one-file change, here only. */
export const DESKTOP_GLOBAL = "__ORCHICON_DESKTOP__";

type GlobalWithDesktop = { [DESKTOP_GLOBAL]?: unknown };

/** desktopInfo returns the shell's info, or null when this is a browser. */
export function desktopInfo(): DesktopInfo | null {
  const raw = (globalThis as GlobalWithDesktop)[DESKTOP_GLOBAL];
  if (!raw || typeof raw !== "object") return null;
  const info = raw as { planeUrl?: unknown; instance?: unknown };
  if (typeof info.planeUrl !== "string" || info.planeUrl === "") return null;
  return {
    planeUrl: info.planeUrl,
    instance: typeof info.instance === "string" && info.instance !== "" ? info.instance : undefined,
  };
}

/** isDesktop reports whether this SPA instance is the Wails desktop shell. */
export function isDesktop(): boolean {
  return desktopInfo() !== null;
}

/** desktopInstance is the instance the shell named, if any (never a guess). */
export function desktopInstance(): string | undefined {
  return desktopInfo()?.instance;
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
// The Wails binding, resolved by PATH rather than by importing generated
// `wailsjs/go/...` files: those do not exist until the shell builds, and this
// file must typecheck on a branch with no shell.
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
