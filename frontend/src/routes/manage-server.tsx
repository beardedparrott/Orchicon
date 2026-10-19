import { createFileRoute, redirect } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import { cn } from "@/lib/utils";
import {
  desktopInfo,
  desktopInstance,
  isDesktop,
  planeControl,
  switchPlane,
  portOf,
  redirectTarget,
  type PlaneControl,
  type ResolutionDTO,
  type StatusDTO,
} from "@/lib/desktop";
import {
  currentStatus,
  describeAction,
  nextView,
  pickerOffered,
  resolveChoice,
  stateLabel,
  statusReadFailure,
  type View,
} from "@/lib/manage-server";

// Manage Server — the Control-nav home for the operations only the DESKTOP shell can
// perform, in front of the connection state that matters most: not connected.
//
// DESKTOP-ONLY, AND BY ABSENCE. In a browser this entry is not rendered (nav-config's
// desktopOnly flag), this route REDIRECTS to /dashboard, and nothing here explains
// why. A permanent, context-free disclaimer about a control nobody in a browser is
// looking for is clutter, not clarity (Plane Lifecycle Control AC4).
export const Route = createFileRoute("/manage-server")({
  beforeLoad: ({ location }) => {
    // One decision, made by the one desktop signal (lib/desktop.ts). A direct browser
    // hit lands on /dashboard without rendering any of the page below.
    const target = redirectTarget(location.pathname, isDesktop());
    if (target === "/dashboard") throw redirect({ to: "/dashboard" });
  },
  component: ManageServerPage,
});

// A SECTION IS ADDED BY PUSHING AN ENTRY INTO SECTIONS AND WRITING ITS COMPONENT.
// Nothing else changes: the route, the nav entry and the page layout stay put — which
// is the whole point of a sectioned surface rather than a one-off screen. The next
// server-side operation (logs, rebuild, backups) appends here; it does not need a
// second nav entry.
type Section = {
  id: string;
  title: string;
  blurb: string;
  Component: () => React.ReactElement;
};
const SECTIONS: Section[] = [
  {
    id: "plane",
    title: "Plane",
    blurb: "Start, stop and inspect the plane this app talks to.",
    Component: PlaneSection,
  },
];

function ManageServerPage() {
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Manage Server</h1>
        <p className="text-sm text-muted-foreground">
          Local server operations for this machine. Nothing here goes through
          the plane's API.
        </p>
      </div>
      {SECTIONS.map((section) => (
        <Card key={section.id} data-testid={`section-${section.id}`}>
          <CardHeader>
            <h2 className="text-lg font-semibold leading-none tracking-tight">
              {section.title}
            </h2>
            <CardDescription>{section.blurb}</CardDescription>
          </CardHeader>
          <CardContent>
            <section.Component />
          </CardContent>
        </Card>
      ))}
    </div>
  );
}

const STATE_BADGE: Record<
  string,
  "default" | "secondary" | "destructive" | "outline"
> = {
  connected: "default",
  connecting: "outline",
  offline: "secondary",
  // Distinct by design: an unhealthy or superseded plane is NOT an absent one.
  "plane-error": "destructive",
};

function PlaneSection() {
  const plane = planeControl();
  const info = desktopInfo();
  const toast = useToast();
  const [status, setStatus] = useState<StatusDTO | null>(null);
  // WHICH instance `status` describes. Without this a status read for the previous
  // instance would be shown beside the newly selected one's name and port.
  const [statusFor, setStatusFor] = useState<string | null>(null);
  // A status READ that failed. Shown, never swallowed: "connecting" forever with no
  // failure reported is the same dishonesty as an unverified success.
  const [statusError, setStatusError] = useState<string | null>(null);
  const [resolution, setResolution] = useState<ResolutionDTO | null>(null);
  const [manual, setManual] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  const [view, setView] = useState<View>(() => nextView(null, null, false));
  const [announcement, setAnnouncement] = useState("");
  const [switching, setSwitching] = useState(false);
  const [switchError, setSwitchError] = useState<string | null>(null);
  // Read once: the payload is injected per document load, so a switch needs a reload.
  const isDesktopApp = info !== null;
  const primaryRef = useRef<HTMLButtonElement>(null);

  const onSwitch = async () => {
    if (!instance) return;
    setSwitching(true);
    setSwitchError(null);
    try {
      await switchPlane(instance);
      // The shell has already retargeted the proxy. Reload so desktopInfo (the
      // instance name, both versions and the mismatch verdict) is re-read from the
      // freshly injected document.
      window.location.reload();
    } catch (err) {
      // Never silently leave the window on the old plane while the button says it
      // connected: say what failed and stay put.
      setSwitchError(err instanceof Error ? err.message : String(err));
      setSwitching(false);
    }
  };

  const choice = resolveChoice(desktopInstance(), resolution);
  const instance = manual ?? choice.instance;

  // Resolve WHICH INSTANCE first — the app must never start "the plane" without saying
  // which. The configured URL's control port IS the instance; an unrecognised port
  // asks rather than guessing.
  useEffect(() => {
    if (!plane) return;
    let cancelled = false;
    const port = portOf(info?.planeURL ?? "");
    plane
      .ResolveControlPort(port)
      .then((res) => {
        if (!cancelled) setResolution(res);
      })
      .catch(() => {
        if (!cancelled)
          setResolution({
            Instance: "",
            ControlPort: port,
            Known: false,
            Running: [],
          });
      });
    return () => {
      cancelled = true;
    };
  }, [plane, info?.planeURL]);

  const refresh = async (): Promise<StatusDTO | null> => {
    if (!plane || !instance) return null;
    const want = instance;
    try {
      const next = await plane.Status(want);
      setStatus(next);
      setStatusFor(want);
      setStatusError(null);
      return next;
    } catch (err) {
      // Make no claim about a plane whose status could not be read, and SAY so.
      setStatus(null);
      setStatusFor(null);
      setStatusError(statusReadFailure(want, err));
      return null;
    }
  };

  // A 2s poll that PAUSES while the window is hidden: a hidden desktop window must not
  // probe /healthz forever. It keeps running while connected on purpose — a plane that
  // dies under a mounted window must not keep showing a stale green.
  useEffect(() => {
    if (!plane || !instance) return;
    let cancelled = false;
    const tick = () => {
      if (cancelled) return;
      if (
        typeof document !== "undefined" &&
        document.visibilityState === "hidden"
      )
        return;
      void refresh();
    };
    tick();
    const timer = setInterval(tick, 2000);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [plane, instance]);

  // What the screen may show: a status read for THIS instance, nothing else.
  const current = currentStatus(instance, statusFor, status);

  // Derive what the screen shows whenever the trusted status or the in-flight flag changes.
  useEffect(() => {
    setView((prev) => nextView(prev, current, pending));
  }, [current, pending]);

  // Focus moves to the primary action when the OFFLINE state APPEARS — once, on the
  // transition, never on a poll tick.
  const focusTarget = view.focusTarget;
  useEffect(() => {
    if (focusTarget === "primary") primaryRef.current?.focus();
  }, [focusTarget, view.state]);

  const runAction = async (action: "start" | "stop" | "restart") => {
    if (!plane || !instance || pending) return;
    setPending(true);
    try {
      const call = (plane: PlaneControl) =>
        action === "start"
          ? plane.Start(instance)
          : action === "stop"
            ? plane.Stop(instance)
            : plane.Restart(instance);
      const dto = await call(plane);
      const message = describeAction(dto);
      setAnnouncement(message);
      if (dto.OK) toast.success(message);
      else toast.error(message);
    } catch (err) {
      const message = `${action} ${instance}: failed — ${err instanceof Error ? err.message : String(err)}`;
      setAnnouncement(message);
      toast.error(message);
    } finally {
      setPending(false);
      void refresh();
    }
  };

  const portText = current?.ControlPort
    ? `port ${current.ControlPort}`
    : choice.running[0]?.ControlPort
      ? `port ${choice.running[0].ControlPort}`
      : "port unknown";
  const triedUrl = current?.URL ?? info?.planeURL ?? "";

  return (
    <div className="space-y-4">
      {/* The selected instance is ON SCREEN, always: "which instance am I looking at"
          must be answerable from the page alone. */}
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <Badge
          variant={STATE_BADGE[view.state] ?? "outline"}
          data-testid="plane-state"
        >
          {stateLabel(view.state)}
        </Badge>
        <span className="font-medium">
          {instance ?? "no instance selected"}
        </span>
        <span className="text-muted-foreground">{portText}</span>
        {current?.Residency ? (
          <span className="text-muted-foreground">
            · {current.Residency}-resident
          </span>
        ) : null}
      </div>

      {statusError ? (
        // A REAL failure, named: a read that failed must never look like progress.
        <p
          className="text-sm text-destructive"
          role="alert"
          data-testid="plane-status-error"
        >
          {statusError}
        </p>
      ) : (
        <p className="text-sm text-muted-foreground" data-testid="plane-detail">
          {view.detail}
        </p>
      )}

      {view.state === "connected" && current ? (
        <dl className="grid grid-cols-2 gap-x-4 gap-y-1 text-xs text-muted-foreground">
          <dt>Health URL</dt>
          <dd className="font-mono">{current.URL}</dd>
          <dt>PID</dt>
          <dd className="font-mono">{current.PID || "—"}</dd>
          <dt>Binary</dt>
          <dd className="truncate font-mono">
            {current.Binary || "unidentified"}
          </dd>
          <dt>Drift</dt>
          <dd>{current.Drift}</dd>
        </dl>
      ) : null}

      {view.state === "offline" ? (
        <p className="text-sm">
          Nothing answers at{" "}
          <span className="font-mono">
            {triedUrl || "(configured URL unknown)"}
          </span>
          .
        </p>
      ) : null}

      {view.state === "plane-error" ? (
        <p className="text-sm text-destructive">
          Something answers but it is not healthy. This is not the same as
          nothing running.
        </p>
      ) : null}

      {choice.ask && !instance ? (
        // An unrecognised control port: ASK. A guess here would act on the wrong plane.
        <div className="flex flex-wrap items-end gap-2">
          <label className="text-sm">
            <span className="mb-1 block text-muted-foreground">
              Which instance? Its control port is not one this app recognises.
            </span>
            <Input
              value={manual ?? ""}
              onChange={(e) => setManual(e.target.value)}
              placeholder="dev, prod or test"
              className="w-48"
              aria-label="Instance name"
            />
          </label>
          {choice.running.length > 0 ? (
            <div className="flex flex-wrap gap-2">
              {choice.running.map((r) => (
                <Button
                  key={r.Instance}
                  variant="outline"
                  size="sm"
                  disabled={pending}
                  onClick={() => setManual(r.Instance)}
                >
                  {r.Instance} · {r.ControlPort} · {stateLabel(r.State)}
                </Button>
              ))}
            </div>
          ) : null}
        </div>
      ) : null}

      {pickerOffered(resolution) && instance ? (
        // More than one plane is up: the operator PICKS, and each option shows its port
        // and health.
        <div className="flex flex-wrap gap-2">
          {(resolution?.Running ?? []).map((r) => (
            <Button
              key={r.Instance}
              variant={r.Instance === instance ? "default" : "outline"}
              size="sm"
              disabled={pending}
              onClick={() => setManual(r.Instance)}
            >
              {r.Instance} · {r.ControlPort} · {stateLabel(r.State)}
            </Button>
          ))}
        </div>
      ) : null}

      {isDesktopApp && instance && instance !== info?.instance ? (
        // THE OTHER HALF OF CHOOSING. The picker above changes which instance this
        // PAGE INSPECTS; it does not change which plane the WINDOW talks to, which
        // is why picking prod still left every API call on dev. This retargets the
        // app itself (internal/desktop UsePlane) and reloads so the injected
        // desktopInfo — instance name, versions, mismatch banner — is re-read.
        <div className="flex flex-wrap items-center gap-2 rounded-lg border border-white/10 p-3">
          <span className="text-sm text-muted-foreground">
            This window is connected to <code>{info?.instance}</code>.
          </span>
          <Button
            variant="outline"
            size="sm"
            disabled={pending || switching}
            onClick={() => void onSwitch()}
          >
            {switching ? "connecting…" : `Connect to ${instance}`}
          </Button>
          {switchError ? (
            <span role="alert" className="text-sm text-destructive">
              {switchError}
            </span>
          ) : null}
        </div>
      ) : null}

      <div className="flex flex-wrap gap-2">
        <Button
          ref={primaryRef}
          disabled={pending || !instance || !plane}
          onClick={() => void runAction("start")}
          data-testid="plane-start"
        >
          Start {instance ?? "plane"}
        </Button>
        <Button
          variant="outline"
          disabled={pending || !instance || !plane}
          onClick={() => void runAction("restart")}
        >
          Restart {instance ?? "plane"}
        </Button>
        <Button
          variant="outline"
          disabled={pending || !instance || !plane}
          onClick={() => void runAction("stop")}
        >
          Stop {instance ?? "plane"}
        </Button>
      </div>

      {/* Every action reports its outcome here, including the log path on failure. */}
      <p
        role="status"
        aria-live="polite"
        className={cn(
          "min-h-[1.25rem] text-sm",
          pending && "text-muted-foreground",
        )}
      >
        {pending ? `Working on ${instance ?? "the plane"}…` : announcement}
      </p>
    </div>
  );
}
