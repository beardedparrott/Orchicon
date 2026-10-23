/**
 * servers.ts — the client for the shell's SERVERS surface.
 *
 * Phase 1 of the GUI-driven design: the desktop app owns install/uninstall, and this is how the page reaches
 * it. Same origin, same per-run token, same `no-store` discipline as the plane-control client beside it —
 * because it is the same shell answering, on a disjoint set of paths.
 *
 * WHY IT TALKS TO THE SHELL AND NOT TO THE PLANE: installing or uninstalling an instance is a HOST
 * operation. There is no lifecycle RPC on the plane's API, and there deliberately is not one — a plane
 * cannot uninstall the machine it is running on.
 *
 * WHY IT DOES NOT OPEN A WEBSOCKET OR A STREAM: install is minutes long, so the shell SPAWNS it and the
 * page POLLS the setup record. A stream would have to survive the app being closed and reopened, which the
 * record already does for free.
 */

import { desktopInfo } from "./desktop";

/** mirrors internal/instancectl.Instance */
export interface ServerInstance {
  name: string;
  dataDir: string;
  controlPort: number;
  url: string;
  plane: {
    Instance: string;
    ControlPort: number;
    URL: string;
    State: string;
    Detail: string;
    Residency: string;
    PID: number;
    Binary: string;
    Drift: string;
    Healthy: boolean;
    LogPath: string;
  };
  setup?: SetupProgress;
  known: boolean;
}

/** mirrors internal/instancectl.SetupProgress */
export interface SetupProgress {
  instance: string;
  state: string;
  phase: string;
  label: string;
  percent: number;
  detail: string;
  started_at: string;
  updated_at: string;
  pid: number;
  error: string;
}

/** mirrors internal/instancectl.Result */
export interface ServerResult {
  action: string;
  instance: string;
  outcome: string;
  ok: boolean;
  message: string;
  logPath: string;
}

/** The setup states this page renders. Mirrors internal/instancectl's constants. */
export const SETUP_RUNNING = "running";
export const SETUP_DONE = "done";
export const SETUP_FAILED = "failed";
export const SETUP_CANCELLED = "cancelled";
/** COMPUTED by the Go side: a "running" record whose process is gone. */
export const SETUP_STALE = "stale";

/** SERVERS_PATH mirrors internal/desktop/servers.go's serversPath. */
const SERVERS_PATH = "/__orchicon_desktop/servers/";
const CONTROL_TOKEN_HEADER = "X-Orchicon-Desktop";

export type Servers = {
  List(): Promise<ServerInstance[]>;
  Setup(instance: string): Promise<SetupProgress | null>;
  Install(instance: string, opts?: ServerOpts): Promise<ServerResult>;
  Uninstall(instance: string, opts: ServerOpts): Promise<ServerResult>;
  Start(instance: string): Promise<ServerResult>;
  Stop(instance: string): Promise<ServerResult>;
  Restart(instance: string): Promise<ServerResult>;
};

export interface ServerOpts {
  purge?: boolean;
  /** Must be true alongside purge. The shell refuses a purge without it, at the boundary. */
  confirmPurge?: boolean;
  runtimeImages?: string;
  /** Omit on a reinstall: pinning renumbers an existing instance's database. */
  ports?: Record<string, number>;
}

function httpServers(token: string): Servers {
  const call = async <T>(path: string, init?: RequestInit): Promise<T> => {
    const res = await fetch(SERVERS_PATH + path, {
      ...init,
      headers: {
        ...(init?.body ? { "Content-Type": "application/json" } : {}),
        [CONTROL_TOKEN_HEADER]: token,
        ...(init?.headers ?? {}),
      },
      // Fresh state every time: a cached list is a lie about a server that may have stopped.
      cache: "no-store",
    });
    const body = (await res.json().catch(() => null)) as
      | (T & { error?: string })
      | null;
    // A shell-side failure is REPORTED, never swallowed into an empty success: "installed" must only ever
    // come from an operation that really ran.
    if (body && typeof body === "object" && "error" in body && body.error) {
      throw new Error(body.error);
    }
    if (!res.ok) throw new Error(`desktop shell answered ${res.status}`);
    return body as T;
  };

  const verb = (
    path: string,
    instance: string,
    opts?: ServerOpts,
  ): Promise<ServerResult> =>
    call<{ result: ServerResult }>(path, {
      method: "POST",
      body: JSON.stringify({
        instance,
        purge: opts?.purge === true,
        confirmPurge: opts?.confirmPurge === true,
        runtimeImages: opts?.runtimeImages ?? "",
        ports: opts?.ports,
      }),
    }).then((r) => r.result);

  return {
    List: () =>
      call<{ instances: ServerInstance[] | null }>("list").then(
        (r) => r.instances ?? [],
      ),
    Setup: (instance: string) =>
      call<{ setup: SetupProgress | null }>(
        `setup?instance=${encodeURIComponent(instance)}`,
      ).then((r) => r.setup ?? null),
    Install: (instance, opts) => verb("install", instance, opts),
    Uninstall: (instance, opts) => verb("uninstall", instance, opts),
    Start: (instance) => verb("start", instance),
    Stop: (instance) => verb("stop", instance),
    Restart: (instance) => verb("restart", instance),
  };
}

/**
 * servers returns the shell's servers surface, or null when this page is not running inside the desktop
 * shell.
 *
 * NULL IS THE HONEST ANSWER IN A BROWSER. The same page is served by the plane's own web UI, where there is
 * no shell to ask and installing an instance is meaningless — so the section renders "management is local
 * to the desktop app" rather than failing.
 */
export function servers(): Servers | null {
  const info = desktopInfo();
  if (!info) return null;
  return httpServers(info.controlToken);
}

/** isSetupActive reports whether a record describes work still in flight (or a dead attempt to report). */
export function isSetupActive(p: SetupProgress | null | undefined): boolean {
  if (!p) return false;
  return p.state === SETUP_RUNNING || p.state === SETUP_STALE;
}

/**
 * setupLabel renders a record as one short line for a row.
 *
 * A STALE RECORD SAYS SO. "Setup stopped responding" is the whole reason the Go side computes that state:
 * a detached install that was killed cannot write "failed", so without this the row would show a progress
 * bar that never moves and never explains itself.
 */
export function setupLabel(p: SetupProgress | null | undefined): string {
  if (!p) return "";
  switch (p.state) {
    case SETUP_RUNNING:
      return p.label || "Setting up…";
    case SETUP_DONE:
      return "Setup complete";
    case SETUP_FAILED:
      return p.error ? `Setup failed: ${p.error}` : "Setup failed";
    case SETUP_CANCELLED:
      return "Setup cancelled";
    case SETUP_STALE:
      return "Setup stopped responding — retry";
    default:
      return p.label || p.state;
  }
}
