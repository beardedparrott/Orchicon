import { createFileRoute, redirect } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import { cn } from "@/lib/utils";
import {
  desktopInfo,
  desktopInstance,
  isDesktop,
  planeControl,
  portOf,
  redirectTarget,
  type PlaneControl,
  type ResolutionDTO,
  type StatusDTO,
} from "@/lib/desktop";
import {
  describeAction,
  nextView,
  pickerOffered,
  resolveChoice,
  stateLabel,
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
type Section = { id: string; title: string; blurb: string; Component: () => React.ReactElement };
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
          Local server operations for this machine. Nothing here goes through the plane's API.
        </p>
      </div>
      {SECTIONS.map((section) => (
        <Card key={section.id} data-testid={`section-${section.id}`}>
          <CardHeader>
            <h2 className="text-lg font-semibold leading-none tracking-tight">{section.title}</h2>
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

const STATE_BADGE: Record<string, "default" | "secondary" | "destructive" | "outline"> = {
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
  const [resolution, setResolution] = useState<ResolutionDTO | null>(null);
  const [manual, setManual] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  const [view, setView] = useState<View>(() => nextView(null, null, false));
  const [announcement, setAnnouncement] = useState("");
  const primaryRef = useRef<HTMLButtonElement>(null);

  const choice = resolveChoice(desktopInstance(), resolution);
  const instance = manual ?? choice.instance;

  // Resolve WHICH INSTANCE first — the app must never start "the plane" without saying
  // which. The configured URL's control port IS the instance; an unrecognised port
  // asks rather than guessing.
  useEffect(() => {
    if (!plane) return;
    let cancelled = false;
    const port = portOf(info?.planeUrl ?? "");
    plane
      .ResolveControlPort(port)
      .then((res) => {
        if (!cancelled) setResolution(res);
      })
      .catch(() => {
        if (!cancelled) setResolution({ Instance: "", ControlPort: port, Known: false, Running: [] });
      });
    return () => {
      cancelled = true;
    };
  }, [plane, info?.planeUrl]);

  const refresh = async (): Promise<StatusDTO | null> => {
    if (!plane || !instance) return null;
    try {
      const next = await plane.Status(instance);
      setStatus(next);
      return next;
    } catch {
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
      if (typeof document !== "undefined" && document.visibilityState === "hidden") return;
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

  // Derive what the screen shows whenever status or the in-flight flag changes.
  useEffect(() => {
    setView((prev) => nextView(prev, status, pending));
  }, [status, pending]);

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
        action === "start" ? plane.Start(instance) : action === "stop" ? plane.Stop(instance) : plane.Restart(instance);
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

  const portText = status?.ControlPort ? `port ${status.ControlPort}` : choice.running[0]?.ControlPort ? `port ${choice.running[0].ControlPort}` : "port unknown";
  const triedUrl = status?.URL ?? info?.planeUrl ?? "";

  return (
    <div className="space-y-4">
      {/* The selected instance is ON SCREEN, always: "which instance am I looking at"
          must be answerable from the page alone. */}
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <Badge variant={STATE_BADGE[view.state] ?? "outline"} data-testid="plane-state">
          {stateLabel(view.state)}
        </Badge>
        <span className="font-medium">{instance ?? "no instance selected"}</span>
        <span className="text-muted-foreground">{portText}</span>
        {status?.Residency ? <span className="text-muted-foreground">· {status.Residency}-resident</span> : null}
      </div>

      <p className="text-sm text-muted-foreground" data-testid="plane-detail">
        {view.detail}
      </p>

      {view.state === "connected" && status ? (
        <dl className="grid grid-cols-2 gap-x-4 gap-y-1 text-xs text-muted-foreground">
          <dt>Health URL</dt>
          <dd className="font-mono">{status.URL}</dd>
          <dt>PID</dt>
          <dd className="font-mono">{status.PID || "—"}</dd>
          <dt>Binary</dt>
          <dd className="truncate font-mono">{status.Binary || "unidentified"}</dd>
          <dt>Drift</dt>
          <dd>{status.Drift}</dd>
        </dl>
      ) : null}

      {view.state === "offline" ? (
        <p className="text-sm">
          Nothing answers at <span className="font-mono">{triedUrl || "(configured URL unknown)"}</span>.
        </p>
      ) : null}

      {view.state === "plane-error" ? (
        <p className="text-sm text-destructive">
          Something answers but it is not healthy. This is not the same as nothing running.
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
                <Button key={r.Instance} variant="outline" size="sm" disabled={pending} onClick={() => setManual(r.Instance)}>
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
      <p role="status" aria-live="polite" className={cn("min-h-[1.25rem] text-sm", pending && "text-muted-foreground")}>
        {pending ? `Working on ${instance ?? "the plane"}…` : announcement}
      </p>
    </div>
  );
}
