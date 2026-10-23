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
  /**
   * The credential for the shell's own plane-control endpoints. Injected per run
   * and never written to disk; empty when the shell runs without one.
   */
  controlToken: string;
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
  const raw = (globalThis as unknown as Record<string, unknown>)[
    DESKTOP_GLOBAL
  ];
  if (!raw || typeof raw !== "object") return null;
  const candidate = raw as Partial<DesktopInfo>;
  if (candidate.desktop !== true) return null;
  if (
    typeof candidate.planeURL !== "string" ||
    typeof candidate.instance !== "string"
  )
    return null;
  return {
    desktop: true,
    planeURL: candidate.planeURL,
    instance: candidate.instance,
    appTag: typeof candidate.appTag === "string" ? candidate.appTag : "",
    planeTag: typeof candidate.planeTag === "string" ? candidate.planeTag : "",
    versionMismatch: candidate.versionMismatch === true,
    controlToken:
      typeof candidate.controlToken === "string" ? candidate.controlToken : "",
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
export function redirectTarget(
  pathname: string,
  desktop: boolean,
): string | null {
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

/** CONTROL_PATH mirrors internal/desktop/control.go's controlPath. */
const CONTROL_PATH = "/__orchicon_desktop/";

/** CONTROL_TOKEN_HEADER mirrors control.go's controlTokenHeader. */
const CONTROL_TOKEN_HEADER = "X-Orchicon-Desktop";

/**
 * httpPlaneControl talks to the SHELL, not to the plane.
 *
 * This is the path the desktop app actually uses. Wails injects its JS bindings
 * into ITS OWN document only, and the app deliberately does not run there: the
 * SPA is served from the shell's loopback http origin so the HttpOnly refresh
 * cookie works and a Connect server-stream is not buffered through a scheme
 * handler. On that origin `window.go` simply does not exist — which is why
 * Manage Server sat on "contacting the plane…" with every control inert.
 *
 * Same origin, so no CORS and no absolute URL: paths resolve against
 * window.location, which IS the shell.
 */
function httpPlaneControl(token: string): PlaneControl {
  const call = async <T>(path: string, init?: RequestInit): Promise<T> => {
    const res = await fetch(CONTROL_PATH + path, {
      ...init,
      headers: {
        ...(init?.body ? { "Content-Type": "application/json" } : {}),
        [CONTROL_TOKEN_HEADER]: token,
        ...(init?.headers ?? {}),
      },
      // The shell answers with fresh state every time; a cached 200 is a lie
      // about a plane that may have stopped since.
      cache: "no-store",
    });
    const body = (await res.json().catch(() => null)) as
      T | { error?: string } | null;
    // A shell-side failure is REPORTED, never swallowed into an empty success:
    // "started" must only ever come from an action that really ran.
    if (body && typeof body === "object" && "error" in body && body.error) {
      throw new Error(body.error);
    }
    if (!res.ok) throw new Error(`desktop shell answered ${res.status}`);
    return body as T;
  };

  return {
    Instances: () => call<StatusDTO[]>("instances"),
    Status: (instance: string) =>
      call<StatusDTO>(`status?instance=${encodeURIComponent(instance)}`),
    ResolveControlPort: (port: number) =>
      call<ResolutionDTO>(`resolve?port=${port}`),
    Start: (instance: string) =>
      call<ActionDTO>("start", {
        method: "POST",
        body: JSON.stringify({ instance }),
      }),
    Stop: (instance: string) =>
      call<ActionDTO>("stop", {
        method: "POST",
        body: JSON.stringify({ instance }),
      }),
    Restart: (instance: string) =>
      call<ActionDTO>("restart", {
        method: "POST",
        body: JSON.stringify({ instance }),
      }),
  };
}

/**
 * switchAndRestart points the app at another instance AND restarts it.
 *
 * WHY A RESTART AND NOT A RELOAD. The operator: "When you connect to another
 * instance, the top menu disappears and you are left on the manage server screen.
 * If you close the desktop app and re-open, THEN you are connected to the other
 * instance. We may want to build in an ability to just auto restart the desktop
 * app once someone hits connect."
 *
 * A window is initialised from the injected payload, the proxy target, the version
 * tags and a session store bound to the old plane's DATABASE. A reload rebuilds
 * only what the document carries, which is why the app arrives half-switched — a
 * different server is a different database with different users. `switchPlane`
 * retargets the proxy and persists the choice; this asks the shell to replace its
 * own process, which rebuilds everything by construction.
 *
 * THE RESPONSE ARRIVES BEFORE THE RESTART, so a caller can report "connected,
 * restarting" instead of dying on a socket the shell just closed.
 */
export async function switchAndRestart(instance: string): Promise<DesktopInfo> {
  const info = desktopInfo();
  if (!info) throw new Error("this page is not running in the desktop app");
  const res = await fetch(CONTROL_PATH + "switch", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      [CONTROL_TOKEN_HEADER]: info.controlToken,
    },
    body: JSON.stringify({ instance }),
    cache: "no-store",
  });
  const body = (await res.json().catch(() => null)) as
    | DesktopInfo
    | { error?: string }
    | null;
  if (body && typeof body === "object" && "error" in body && body.error)
    throw new Error(body.error);
  if (!res.ok) throw new Error(`desktop shell answered ${res.status}`);
  return body as DesktopInfo;
}

/**
 * switchPlane points THIS WINDOW at another instance's plane and returns the
 * shell's refreshed Info.
 *
 * Distinct from choosing which instance to INSPECT (the Manage Server picker):
 * this retargets the app — every subsequent API call goes to the new plane — and
 * the shell remembers the choice for the next launch. The caller reloads the
 * window afterwards, because desktopInfo (instance, versions, mismatch banner) is
 * injected into the document and only changes on a fresh load.
 *
 * A browser has no shell to switch, so it throws rather than pretending.
 */
export async function switchPlane(instance: string): Promise<DesktopInfo> {
  const info = desktopInfo();
  if (!info) throw new Error("this page is not running in the desktop app");
  const res = await fetch(CONTROL_PATH + "plane", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      [CONTROL_TOKEN_HEADER]: info.controlToken,
    },
    body: JSON.stringify({ instance }),
    cache: "no-store",
  });
  const body = (await res.json().catch(() => null)) as
    DesktopInfo | { error?: string } | null;
  if (body && typeof body === "object" && "error" in body && body.error)
    throw new Error(body.error);
  if (!res.ok) throw new Error(`desktop shell answered ${res.status}`);
  return body as DesktopInfo;
}

/**
 * planeControl resolves the plane-control surface, or null in a browser / in a test.
 *
 * ORDER, and both arms matter:
 *   1. the Wails binding, when one exists (a shell that serves its own document —
 *      kept so this file works whichever transport the shell ships);
 *   2. over HTTP to the shell, when THIS PAGE IS THE DESKTOP APP. The decision is
 *      `desktopInfo() !== null`, which is the ONE desktop signal, not a second
 *      detection: in a browser the payload is absent, so this returns null and
 *      Manage Server stays unreachable.
 */
export function planeControl(): PlaneControl | null {
  const g = globalThis as { go?: { planebind?: { Service?: unknown } } };
  const svc = g.go?.planebind?.Service;
  if (svc && typeof svc === "object") return svc as PlaneControl;

  const info = desktopInfo();
  if (!info) return null;
  // A shell that installed no control surface answers 404; that is indistinguishable
  // from a browser's absence, and the page already handles `null`.
  return httpPlaneControl(info.controlToken);
}
