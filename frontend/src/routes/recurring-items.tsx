// Recurring Items page (feature 4.3) — a dedicated FLAT card list, NOT the
// Work Items tree/board/filterbar. Recurring items are first-class
// automations: cadence + next-run are the primary identity (not kind/status),
// the identity is fuchsia/repeat, and each card exposes an enable/pause
// toggle that persists the item's `recurring_enabled` flag (never a
// destructive clear of the schedule).
//
// The page owns exactly one list query: `useListWorkItems(ONLY_RECURRING)`.
// Per-item last-run status comes from a single project-scoped
// `useListWorkflowRuns` grouped by work item (one query, no N+1).

import { createRoute, Link } from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";
import { CalendarClock, Pause, Play, Repeat } from "lucide-react";

import { useListWorkItems, useUpdateWorkItem } from "@/api/workItems";
import { useListProjects } from "@/api/projects";
import { useListWorkflowRuns } from "@/api/workflows";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { useToast } from "@/components/ui/toast";
import { RecurringBadge, RunStatusBadge } from "@/components/work-items/work-item-badges";
import { cn } from "@/lib/utils";
import { RecurringFilter } from "@/api/gen/orchicon/api/v1/work_item_service_pb";
import { type RecurringSchedule, type WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import type { WorkflowRun } from "@/api/gen/orchicon/api/v1/workflow_pb";
import { Route as rootRoute } from "@/routes/__root";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/recurring-items",
  component: RecurringItemsPage,
});

function RecurringItemsPage() {
  const { data: projects } = useListProjects();
  const [projectId, setProjectId] = useState<string>("");

  // The ONE list query the page owns: recurring items only, auto-refreshed.
  const { data: items, isLoading, error } = useListWorkItems(projectId, {
    recurringFilter: RecurringFilter.ONLY_RECURRING,
    refetchInterval: 5_000,
  });

  // One project-scoped run query, grouped by work item for per-card last-run
  // status (newest run per item; avoids N+1 per-card useListWorkflowRuns).
  const { data: runs } = useListWorkflowRuns({
    projectId: projectId || undefined,
    sortBy: "started_at",
    sortOrder: "desc",
  });
  const lastRunByItem = useMemo(() => {
    const m = new Map<string, WorkflowRun>();
    for (const run of runs ?? []) {
      if (!run.workItemId) continue;
      if (!m.has(run.workItemId)) m.set(run.workItemId, run);
    }
    return m;
  }, [runs]);

  const now = useNow(1000);

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-center justify-between gap-4">
        <div>
          <h1 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
            <Repeat aria-hidden="true" className="h-6 w-6 text-fuchsia-500" />
            Recurring Items
          </h1>
          <p className="text-sm text-muted-foreground">
            Automations that fire on a cadence. Each item runs its bound
            workflow on schedule and records a per-fire history.
          </p>
        </div>
        <div className="flex items-center gap-2">
          {!isLoading && <LiveRefreshIndicator />}
          <Button asChild>
            <Link to="/recurring-items/new" search={{ projectId: projectId ?? "" }}>
              New Recurring Item
            </Link>
          </Button>
        </div>
      </div>

      {/* Project selector — empty = all projects, matching Work Items. */}
      <div className="flex flex-wrap items-center gap-3">
        <label
          htmlFor="recurringProject"
          className="text-sm font-medium text-muted-foreground"
        >
          Project
        </label>
        <select
          id="recurringProject"
          className="flex h-11 min-h-[44px] w-full max-w-xs rounded-xl glass-input px-3 py-1 text-sm sm:h-9"
          value={projectId}
          onChange={(e) => setProjectId(e.target.value)}
        >
          <option value="">All projects</option>
          {(projects ?? []).map((p) => (
            <option key={p.id} value={p.id}>
              {p.name}
            </option>
          ))}
        </select>
      </div>

      {isLoading ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : error ? (
        <p className="text-sm text-destructive">
          Failed to load recurring items: {String(error)}
        </p>
      ) : (items ?? []).length === 0 ? (
        <EmptyState />
      ) : (
        <div className="grid gap-3 md:grid-cols-2">
          {(items ?? []).map((item) => (
            <RecurringItemCard
              key={item.id}
              item={item}
              now={now}
              lastRun={lastRunByItem.get(item.id)}
            />
          ))}
        </div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// RecurringItemCard — the flat-list row. Fuchsia/repeat identity, cadence,
// live next-run countdown, last-run status, and a persisted enable/pause
// toggle. Never renders the work-item KindBadge/status pill as the primary
// identity — "recurring automation" wins.
// ---------------------------------------------------------------------------

function RecurringItemCard({
  item,
  now,
  lastRun,
}: {
  item: WorkItem;
  now: number;
  lastRun?: WorkflowRun;
}) {
  const updateWorkItem = useUpdateWorkItem(item.projectId);
  const toast = useToast();
  // `recurring_enabled` defaults to true; treat an unset legacy row as active.
  const enabled = item.recurringEnabled !== false;

  const nextRunTs = item.nextRunAt ?? item.scheduledStartAt;
  const nextMs = nextRunTs ? Number(nextRunTs.seconds) * 1000 : 0;

  const cadence = humanCadence(item.recurringSchedule);

  const handleToggle = () => {
    updateWorkItem.mutate(
      { id: item.id, recurringEnabled: !enabled },
      {
        onSuccess: () =>
          toast.success(
            enabled ? `Paused "${item.title}"` : `Resumed "${item.title}"`,
          ),
        onError: (e) => toast.error(`Failed to update: ${String(e)}`),
      },
    );
  };

  return (
    <Card className="group border-l-2 border-l-fuchsia-500 transition-colors hover:border-l-fuchsia-600">
      <CardHeader className="pb-2">
        <div className="flex items-start justify-between gap-2">
          <CardTitle className="flex min-w-0 items-center gap-2 text-base">
            <Repeat
              aria-hidden="true"
              className="h-4 w-4 shrink-0 text-fuchsia-500"
            />
            <Link
              to="/recurring-items/$id"
              params={{ id: item.id }}
              className="min-w-0 truncate [overflow-wrap:anywhere] hover:underline"
            >
              {item.title}
            </Link>
          </CardTitle>
          <RecurringBadge />
        </div>
          <CardDescription className="flex flex-wrap items-center gap-x-3 gap-y-1 pt-1">
            <span className="inline-flex items-center gap-1 font-medium text-foreground">
              <CalendarClock aria-hidden="true" className="h-3.5 w-3.5" />
              {cadence}
            </span>
            {!enabled && (
              <span className="rounded-full px-2 py-0.5 text-xs font-medium bg-muted text-muted-foreground">
                Paused
              </span>
            )}
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-2">
          <div className="flex items-center justify-between gap-2">
            <div className="flex items-center gap-2 text-sm">
              {enabled && nextMs > 0 ? (
                <>
                  <span className="text-xs text-muted-foreground">Next run</span>
                  <CountdownChip target={nextMs} now={now} />
                </>
              ) : enabled ? (
                <span className="text-xs italic text-muted-foreground/70">
                  Next run not scheduled
                </span>
              ) : (
                <span className="text-xs text-muted-foreground">
                  Paused — no active next run
                </span>
              )}
            </div>
            {/* Enable/pause — persisted via recurring_enabled, never a
                destructive schedule clear. */}
            <Button
              variant="outline"
              size="sm"
              onClick={handleToggle}
              disabled={updateWorkItem.isPending}
            >
              {enabled ? (
                <>
                  <Pause aria-hidden="true" className="h-3.5 w-3.5" />
                  Pause
                </>
              ) : (
                <>
                  <Play aria-hidden="true" className="h-3.5 w-3.5" />
                  Resume
                </>
              )}
            </Button>
          </div>

          <div className="flex items-center gap-2 border-t border-border/60 pt-2 text-xs">
            <span className="text-muted-foreground">Last run</span>
            {lastRun ? (
              <>
                <RunStatusBadge status={lastRun.status} />
                <span className="text-muted-foreground">
                  {formatTimestamp(lastRun.startedAt)}
                </span>
              </>
            ) : (
              <span className="italic text-muted-foreground/70">Never</span>
            )}
          </div>
        </CardContent>
      </Card>
  );
}

function EmptyState() {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Repeat aria-hidden="true" className="h-5 w-5 text-fuchsia-500" />
          No recurring items yet
        </CardTitle>
        <CardDescription>
          A recurring item is a first-class automation: pick a cadence and a
          workflow, and Orchicon fires the workflow on schedule, recording a
          per-fire history. Create one to get started.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <Button asChild>
          <Link to="/recurring-items/new" search={{ projectId: "" }}>
            Create your first recurring item
          </Link>
        </Button>
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// Live refresh indicator — mirrors the Schedules LiveClock pattern: a pulsing
// dot + "Live" chip driven by the same page-level `now` ticker.
// ---------------------------------------------------------------------------

function LiveRefreshIndicator() {
  const now = useNow(1000);
  const time = new Date(now).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
  return (
    <div
      role="timer"
      aria-label={`Auto-refreshes every 5 seconds. Last refreshed at ${time}`}
      className="flex items-center gap-2 rounded-2xl glass-panel px-3 py-2"
    >
      <span className="h-2 w-2 animate-pulse rounded-full bg-fuchsia-500 motion-reduce:animate-none" />
      <span className="font-mono text-xs font-medium tabular-nums text-foreground">
        Live {time}
      </span>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function tsToMs(ts?: { seconds: bigint | number; nanos?: number }): number {
  if (!ts) return 0;
  return Number(ts.seconds) * 1000;
}

function formatTimestamp(ts?: { seconds: bigint | number; nanos?: number }): string {
  const ms = tsToMs(ts);
  if (!ms) return "";
  return new Date(ms).toLocaleString([], {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

/** Human cadence: "Every 2 hours", "Every day", "Every week (Mon, Wed)". */
function humanCadence(s: RecurringSchedule | undefined): string {
  if (!s || (!s.frequency && !s.interval && s.days.length === 0)) {
    return "One-time";
  }
  const f = s.frequency || "daily";
  const i = s.interval || 1;
  const unit =
    f === "minute"
      ? i > 1
        ? "minutes"
        : "minute"
      : f === "hourly"
        ? i > 1
          ? "hours"
          : "hour"
        : f === "daily"
          ? i > 1
            ? "days"
            : "day"
          : f === "weekly"
            ? i > 1
              ? "weeks"
              : "week"
            : i > 1
              ? "months"
              : "month";
  const base = `Every ${i > 1 ? `${i} ` : ""}${unit}`;
  if (f === "weekly" && s.days.length > 0) return `${base} (${s.days.join(", ")})`;
  return base;
}

function formatCountdown(target: number, now: number): string {
  const diff = target - now;
  if (diff <= 0) return "now";
  const minutes = Math.floor(diff / 60_000);
  if (minutes < 1) return "in <1m";
  if (minutes < 60) return `in ${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const remMinutes = minutes % 60;
  if (hours < 24) {
    return remMinutes > 0 ? `in ${hours}h ${remMinutes}m` : `in ${hours}h`;
  }
  const days = Math.floor(hours / 24);
  const remHours = hours % 24;
  return remHours > 0 ? `in ${days}d ${remHours}h` : `in ${days}d`;
}

function countdownAria(target: number, now: number): string {
  const diff = target - now;
  if (diff <= 0) return "Next run now";
  const totalMinutes = Math.max(1, Math.round(diff / 60_000));
  const days = Math.floor(totalMinutes / 1440);
  const hours = Math.floor((totalMinutes % 1440) / 60);
  const minutes = totalMinutes % 60;
  const parts: string[] = [];
  if (days > 0) parts.push(`${days} day${days === 1 ? "" : "s"}`);
  if (hours > 0) parts.push(`${hours} hour${hours === 1 ? "" : "s"}`);
  if (minutes > 0 || parts.length === 0)
    parts.push(`${minutes} minute${minutes === 1 ? "" : "s"}`);
  return `Next run in ${parts.join(" ")}`;
}

/** Live countdown chip (from Schedules' CountdownChip). */
function CountdownChip({ target, now }: { target: number; now: number }) {
  const diff = target - now;
  const text = diff <= 0 ? "now" : formatCountdown(target, now);
  return (
    <span
      aria-label={countdownAria(target, now)}
      title={countdownAria(target, now)}
      className={cn(
        "rounded-full px-2 py-0.5 text-xs font-medium",
        diff <= 0
          ? "bg-emerald-100 text-emerald-800"
          : "bg-muted text-muted-foreground",
      )}
    >
      {text}
    </span>
  );
}

// One page-level `now` ticker (pauses when the tab is hidden), mirroring the
// Schedules page's useNow — drives the live countdown + the live indicator.
function useNow(intervalMs = 1000) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    let timer: ReturnType<typeof setInterval> | undefined;
    const start = () => {
      if (timer) return;
      setNow(Date.now());
      timer = setInterval(() => setNow(Date.now()), intervalMs);
    };
    const stop = () => {
      if (timer) {
        clearInterval(timer);
        timer = undefined;
      }
    };
    const onVisibility = () => {
      if (document.hidden) stop();
      else start();
    };
    start();
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      stop();
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [intervalMs]);
  return now;
}
