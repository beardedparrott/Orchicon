// Schedules page (design-notes/create-new-page-for-schedules.md).
//
// Shows all currently scheduled work items in chronological order with
// their next runtimes ("Upcoming", the default view), plus a history of
// items that already ran ("History"). All data comes from existing
// Connect-ES clients (AGENTS.md invariants #1/#2): Upcoming is a
// status = WORK_ITEM_STATUS_SCHEDULED query; History is a broad fetch
// filtered client-side to items that have a scheduled_start_at and a
// terminal status (the ListWorkItems API accepts exactly one status
// filter, so a terminal-status union cannot be fetched in one call —
// the fetch is isolated behind useListWorkItems so a future backend
// filter/RPC can swap in without touching this page's layout).
//
// Recurring scheduled tasks are a future backend feature (design §5).
// Every card reserves a right-aligned frequency slot that today renders
// a muted "One-time" chip; when recurrence lands, only `recurrenceBadge`
// changes — the slot already exists so the card layout does not reflow.
import { useEffect, useMemo, useState } from "react";
import { Link, createRoute } from "@tanstack/react-router";
import { z } from "zod";
import {
  Ban,
  CalendarClock,
  Clock,
  SearchX,
  Trash2,
  Workflow,
} from "lucide-react";
import type { Timestamp } from "@bufbuild/protobuf";

import {
  useBatchDeleteWorkItems,
  useDeleteWorkItem,
  useListWorkItems,
} from "@/api/workItems";
import { useListProjects } from "@/api/projects";
import {
  WorkItemStatus,
  type WorkItem as WorkItemProto,
} from "@/api/gen/orchicon/api/v1/work_item_pb";
import type { Project } from "@/api/gen/orchicon/api/v1/project_pb";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

const schedulesSearchSchema = z.object({
  view: z.enum(["upcoming", "history"]).optional(),
  projectId: z.string().optional(),
});

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/schedules",
  validateSearch: schedulesSearchSchema,
  component: SchedulesPage,
});

// Terminal statuses that count as "previously ran" for History
// (succeeded / failed / cancelled).
const TERMINAL_STATUSES = new Set([6, 7, 8]);

// Schedulable kinds: Task, Subtask, and the recovery kinds. Epics and
// features cannot be scheduled, so they are not offered in the filter.
const SCHEDULABLE_KINDS = [
  { value: "3", label: "Task" },
  { value: "4", label: "Subtask" },
  { value: "5", label: "Recovery: Stop" },
  { value: "6", label: "Recovery: Summarize & Restart" },
  { value: "7", label: "Recovery: Human Escalation" },
  { value: "8", label: "Recovery: Retry" },
];

function SchedulesPage() {
  const { view = "upcoming", projectId = "" } = Route.useSearch();
  const navigate = Route.useNavigate();
  const { data: projects } = useListProjects();
  const [search, setSearch] = useState("");
  const [kindFilter, setKindFilter] = useState("");
  const [sortOrder, setSortOrder] = useState(view === "upcoming" ? "asc" : "desc");
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const now = useNow();

  // Reset the sort direction to the view's default when the view changes.
  useEffect(() => {
    setSortOrder(view === "upcoming" ? "asc" : "desc");
    setSelected(new Set());
  }, [view]);

  const hasProjects = projects && projects.length > 0;
  const cancelScheduled = useDeleteWorkItem(projectId);
  const batchDelete = useBatchDeleteWorkItems();

  const goView = (nextView: "upcoming" | "history") => {
    navigate({ search: (prev) => ({ ...prev, view: nextView }) });
  };

  const goProject = (id: string) => {
    navigate({ search: (prev) => ({ ...prev, projectId: id || undefined }) });
  };

  const toggleSelect = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const toggleSelectAll = (items: WorkItemProto[]) => {
    if (selected.size === items.length) {
      setSelected(new Set());
    } else {
      setSelected(new Set(items.map((i) => i.id)));
    }
  };

  const handleCancelSelected = () => {
    if (selected.size === 0) return;
    const count = selected.size;
    if (
      !window.confirm(
        `Cancel ${count} scheduled work item${count === 1 ? "" : "s"}? Cancelling a schedule transitions the work item to cancelled.`,
      )
    ) {
      return;
    }
    Array.from(selected).forEach((id) => cancelScheduled.mutate(id));
    setSelected(new Set());
  };

  const handleBatchDelete = () => {
    if (selected.size === 0) return;
    const count = selected.size;
    if (
      !window.confirm(
        `Permanently delete ${count} work item${count === 1 ? "" : "s"}? This cannot be undone.`,
      )
    ) {
      return;
    }
    batchDelete.mutate(Array.from(selected), {
      onSuccess: () => setSelected(new Set()),
    });
  };

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Schedules</h1>
          <p className="text-sm text-muted-foreground">
            Upcoming runs of scheduled work items, in chronological order.
          </p>
        </div>
        <LiveClock now={now} />
      </div>

      {/* View toggle + filter bar (AGENTS.md UI-consistency rule). */}
      <div className="flex flex-wrap items-center gap-3">
        <div className="flex rounded-md border" role="group" aria-label="View">
          <button
            type="button"
            aria-pressed={view === "upcoming"}
            className={cn(
              "rounded-l-md px-3 py-1.5 text-sm font-medium transition-colors",
              view === "upcoming"
                ? "bg-accent text-accent-foreground"
                : "text-muted-foreground hover:bg-accent/50",
            )}
            onClick={() => goView("upcoming")}
          >
            Upcoming
          </button>
          <button
            type="button"
            aria-pressed={view === "history"}
            className={cn(
              "rounded-r-md px-3 py-1.5 text-sm font-medium transition-colors",
              view === "history"
                ? "bg-accent text-accent-foreground"
                : "text-muted-foreground hover:bg-accent/50",
            )}
            onClick={() => goView("history")}
          >
            History
          </button>
        </div>

        <Input
          placeholder="Search title…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="h-9 w-48"
        />

        <select
          value={projectId}
          onChange={(e) => goProject(e.target.value)}
          disabled={!projects || projects.length === 0}
          className="h-9 rounded-md border border-input bg-transparent px-3 text-sm shadow-sm"
        >
          <option value="">All projects</option>
          {projects && projects.length > 0 ? (
            projects.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name}
              </option>
            ))
          ) : (
            <option value="" disabled>
              No projects available
            </option>
          )}
        </select>

        <select
          value={kindFilter}
          onChange={(e) => setKindFilter(e.target.value)}
          className="h-9 rounded-md border border-input bg-transparent px-3 text-sm shadow-sm"
        >
          <option value="">All kinds</option>
          {SCHEDULABLE_KINDS.map((k) => (
            <option key={k.value} value={k.value}>
              {k.label}
            </option>
          ))}
        </select>

        <select
          value={view === "upcoming" ? "next_run" : "last_run"}
          className="h-9 rounded-md border border-input bg-transparent px-3 text-sm shadow-sm"
          aria-label="Sort by"
        >
          <option value={view === "upcoming" ? "next_run" : "last_run"}>
            {view === "upcoming" ? "Sort: next run" : "Sort: last run"}
          </option>
        </select>

        <select
          value={sortOrder}
          onChange={(e) => setSortOrder(e.target.value)}
          className="h-9 rounded-md border border-input bg-transparent px-3 text-sm shadow-sm"
          aria-label="Sort order"
        >
          <option value="asc">Asc</option>
          <option value="desc">Desc</option>
        </select>

        {selected.size > 0 &&
          (view === "upcoming" ? (
            <Button
              variant="destructive"
              size="sm"
              onClick={handleCancelSelected}
              disabled={cancelScheduled.isPending}
            >
              <Ban className="mr-1 h-3.5 w-3.5" />
              Cancel {selected.size} selected
            </Button>
          ) : (
            <Button
              variant="destructive"
              size="sm"
              onClick={handleBatchDelete}
              disabled={batchDelete.isPending}
            >
              <Trash2 className="mr-1 h-3.5 w-3.5" />
              Delete {selected.size} selected
            </Button>
          ))}
      </div>

      {!hasProjects && (
        <Card>
          <CardHeader>
            <CardTitle>No project selected</CardTitle>
            <CardDescription>
              Create a project first to start scheduling work items.
            </CardDescription>
          </CardHeader>
        </Card>
      )}

      {hasProjects && view === "upcoming" && (
        <UpcomingView
          projectId={projectId}
          search={search}
          kindFilter={kindFilter}
          sortOrder={sortOrder}
          now={now}
          projects={projects}
          selected={selected}
          onToggleSelect={toggleSelect}
          onToggleSelectAll={toggleSelectAll}
        />
      )}

      {hasProjects && view === "history" && (
        <HistoryView
          projectId={projectId}
          search={search}
          kindFilter={kindFilter}
          sortOrder={sortOrder}
          projects={projects}
          selected={selected}
          onToggleSelect={toggleSelect}
          onToggleSelectAll={toggleSelectAll}
        />
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Upcoming view
// ---------------------------------------------------------------------------

function UpcomingView({
  projectId,
  search,
  kindFilter,
  sortOrder,
  now,
  projects,
  selected,
  onToggleSelect,
  onToggleSelectAll,
}: {
  projectId: string;
  search: string;
  kindFilter: string;
  sortOrder: string;
  now: number;
  projects?: Project[];
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  onToggleSelectAll: (items: WorkItemProto[]) => void;
}) {
  const {
    data: scheduled,
    isLoading,
    error,
  } = useListWorkItems(projectId, {
    status: WorkItemStatus.SCHEDULED,
    search: search || undefined,
    refetchInterval: 5_000,
  });

  // Kind filter + chronological sort are client-side (the server sort_by
  // only supports title/priority/created_at; scheduled_start_at is not
  // one of them).
  const items = useMemo(() => {
    const base = kindFilter
      ? (scheduled ?? []).filter((i) => i.kind === Number(kindFilter))
      : (scheduled ?? []);
    const sorted = [...base].sort(
      (a, b) => tsToMs(a.scheduledStartAt) - tsToMs(b.scheduledStartAt),
    );
    return sortOrder === "asc" ? sorted : sorted.reverse();
  }, [scheduled, kindFilter, sortOrder]);

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading…</p>;
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        Failed to load schedules: {String(error)}
      </p>
    );
  }
  if (items.length === 0) {
    return (
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <SearchX className="h-5 w-5 text-muted-foreground" />
            No upcoming schedules
          </CardTitle>
          <CardDescription>
            Scheduled work items will appear here. Set a scheduled start time
            on a task or subtask.
          </CardDescription>
        </CardHeader>
      </Card>
    );
  }

  const groups = groupByDay(items, now);

  return (
    <div className="space-y-6">
      <StatsStrip items={items} now={now} />
      <div className="flex items-center gap-2 px-2 py-1">
        <input
          type="checkbox"
          checked={selected.size === items.length}
          onChange={() => onToggleSelectAll(items)}
          className="h-4 w-4 rounded border-input"
          aria-label="Select all upcoming schedules"
        />
        <span aria-live="polite" className="text-xs text-muted-foreground">
          {selected.size > 0
            ? `${selected.size} of ${items.length} selected`
            : `${items.length} scheduled item${items.length === 1 ? "" : "s"}`}
        </span>
      </div>
      {groups.map((group, gi) => (
        <AgendaGroup
          key={group.key}
          group={group}
          isLastGroup={gi === groups.length - 1}
          now={now}
          projects={projects}
          selected={selected}
          onToggleSelect={onToggleSelect}
        />
      ))}
    </div>
  );
}

function AgendaGroup({
  group,
  isLastGroup,
  now,
  projects,
  selected,
  onToggleSelect,
}: {
  group: { key: string; label: string; items: WorkItemProto[] };
  isLastGroup: boolean;
  now: number;
  projects?: Project[];
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
}) {
  return (
    <section aria-labelledby={group.key}>
      <div className="flex items-center gap-3">
        <h2
          id={group.key}
          className="text-sm font-semibold uppercase tracking-wide text-muted-foreground"
        >
          {group.label}
        </h2>
        <div className="h-px flex-1 bg-border" />
        <span className="text-xs text-muted-foreground">
          {group.items.length}
        </span>
      </div>
      <ul className="mt-3 space-y-3">
        {group.items.map((item, ii) => {
          const isLast = isLastGroup && ii === group.items.length - 1;
          return (
            <li key={item.id} className="flex gap-3">
              <div className="hidden w-16 shrink-0 pt-4 text-right font-mono text-xs tabular-nums text-muted-foreground sm:block">
                {formatTime(tsToMs(item.scheduledStartAt))}
              </div>
              <div className="relative flex flex-col items-center">
                <span
                  aria-hidden
                  className="mt-4 h-2.5 w-2.5 shrink-0 rounded-full ring-2 ring-background"
                  style={{ backgroundColor: kindDotColor(item.kind) }}
                />
                {!isLast && (
                  <span
                    aria-hidden
                    className="absolute left-1/2 top-6 h-[calc(100%+0.75rem)] w-px -translate-x-1/2 bg-border"
                  />
                )}
              </div>
              <div className="min-w-0 flex-1">
                <ScheduleCard
                  item={item}
                  now={now}
                  projects={projects}
                  selected={selected.has(item.id)}
                  onToggleSelect={onToggleSelect}
                />
              </div>
            </li>
          );
        })}
      </ul>
    </section>
  );
}

function ScheduleCard({
  item,
  now,
  projects,
  selected,
  onToggleSelect,
}: {
  item: WorkItemProto;
  now: number;
  projects?: Project[];
  selected: boolean;
  onToggleSelect: (id: string) => void;
}) {
  const projectName = projects?.find((p) => p.id === item.projectId)?.name;
  const scheduledAt = tsToMs(item.scheduledStartAt);
  return (
    <div className="group flex items-center gap-2">
      <input
        type="checkbox"
        checked={selected}
        onChange={() => onToggleSelect(item.id)}
        className="h-4 w-4 shrink-0 rounded border-input"
        aria-label={`Select ${item.title}`}
      />
      <Link
        to="/work-items/$id"
        params={{ id: item.id }}
        className="min-w-0 flex-1"
      >
        <Card className="transition-colors hover:bg-accent">
          <CardContent className="flex flex-col gap-2 p-4 sm:flex-row sm:items-center sm:justify-between">
            <div className="flex min-w-0 items-center gap-3">
              <KindDot kind={item.kind} />
              <div className="min-w-0 flex-1 overflow-hidden">
                <div className="flex items-center gap-2">
                  <span className="truncate text-sm font-medium group-hover:underline">
                    {item.title}
                  </span>
                  <KindBadge kind={item.kind} />
                  <StatusPill status={item.status} />
                </div>
                <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
                  {projectName && <span>{projectName}</span>}
                  <WorkflowChip workflowId={item.workflowId} />
                </div>
              </div>
            </div>
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs sm:shrink-0">
              <span className="inline-flex items-center gap-1 font-mono tabular-nums">
                <Clock className="h-3 w-3" />
                {formatTime(scheduledAt)}
              </span>
              <CountdownChip target={scheduledAt} now={now} />
              <span className="rounded-full border border-input px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
                {recurrenceBadge(item)}
              </span>
            </div>
          </CardContent>
        </Card>
      </Link>
    </div>
  );
}

// ---------------------------------------------------------------------------
// History view
// ---------------------------------------------------------------------------

function HistoryView({
  projectId,
  search,
  kindFilter,
  sortOrder,
  projects,
  selected,
  onToggleSelect,
  onToggleSelectAll,
}: {
  projectId: string;
  search: string;
  kindFilter: string;
  sortOrder: string;
  projects?: Project[];
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  onToggleSelectAll: (items: WorkItemProto[]) => void;
}) {
  const {
    data: allItems,
    isLoading,
    error,
  } = useListWorkItems(projectId, {
    search: search || undefined,
    refetchInterval: 30_000,
  });

  // History = items that had a scheduled start and reached a terminal
  // status (the API accepts one status filter, so this union is derived
  // client-side; see the header comment).
  const items = useMemo(() => {
    const base = (allItems ?? []).filter(
      (i) =>
        i.scheduledStartAt &&
        TERMINAL_STATUSES.has(i.status) &&
        (!kindFilter || i.kind === Number(kindFilter)),
    );
    const sorted = [...base].sort(
      (a, b) => tsToMs(b.scheduledStartAt) - tsToMs(a.scheduledStartAt),
    );
    return sortOrder === "desc" ? sorted : sorted.reverse();
  }, [allItems, kindFilter, sortOrder]);

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading…</p>;
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        Failed to load schedule history: {String(error)}
      </p>
    );
  }
  if (items.length === 0) {
    return (
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <SearchX className="h-5 w-5 text-muted-foreground" />
            No past runs yet
          </CardTitle>
          <CardDescription>
            Scheduled work items that have run will appear here.
          </CardDescription>
        </CardHeader>
      </Card>
    );
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2 px-2 py-1">
        <input
          type="checkbox"
          checked={selected.size === items.length}
          onChange={() => onToggleSelectAll(items)}
          className="h-4 w-4 rounded border-input"
          aria-label="Select all history items"
        />
        <span aria-live="polite" className="text-xs text-muted-foreground">
          {selected.size > 0
            ? `${selected.size} of ${items.length} selected`
            : `${items.length} past run item${items.length === 1 ? "" : "s"}`}
        </span>
      </div>
      <ul className="space-y-3">
        {items.map((item, ii) => (
          <li key={item.id} className="flex gap-3">
            <div className="hidden w-16 shrink-0 pt-4 text-right font-mono text-xs tabular-nums text-muted-foreground sm:block">
              {formatTime(tsToMs(item.scheduledStartAt))}
            </div>
            <div className="relative flex flex-col items-center">
              <span
                aria-hidden
                className="mt-4 h-2.5 w-2.5 shrink-0 rounded-full ring-2 ring-background"
                style={{ backgroundColor: kindDotColor(item.kind) }}
              />
              {ii !== items.length - 1 && (
                <span
                  aria-hidden
                  className="absolute left-1/2 top-6 h-[calc(100%+0.75rem)] w-px -translate-x-1/2 bg-border"
                />
              )}
            </div>
            <div className="min-w-0 flex-1">
              <HistoryCard
                item={item}
                projects={projects}
                selected={selected.has(item.id)}
                onToggleSelect={onToggleSelect}
              />
            </div>
          </li>
        ))}
      </ul>
    </div>
  );
}

function HistoryCard({
  item,
  projects,
  selected,
  onToggleSelect,
}: {
  item: WorkItemProto;
  projects?: Project[];
  selected: boolean;
  onToggleSelect: (id: string) => void;
}) {
  const projectName = projects?.find((p) => p.id === item.projectId)?.name;
  const ranAt = tsToMs(item.scheduledStartAt);
  return (
    <div className="group flex items-center gap-2">
      <input
        type="checkbox"
        checked={selected}
        onChange={() => onToggleSelect(item.id)}
        className="h-4 w-4 shrink-0 rounded border-input"
        aria-label={`Select ${item.title}`}
      />
      <Link
        to="/work-items/$id"
        params={{ id: item.id }}
        className="min-w-0 flex-1"
      >
        <Card className="transition-colors hover:bg-accent">
          <CardContent className="flex flex-col gap-2 p-4 sm:flex-row sm:items-center sm:justify-between">
            <div className="flex min-w-0 items-center gap-3">
              <KindDot kind={item.kind} />
              <div className="min-w-0 flex-1 overflow-hidden">
                <div className="flex items-center gap-2">
                  <span className="truncate text-sm font-medium group-hover:underline">
                    {item.title}
                  </span>
                  <KindBadge kind={item.kind} />
                  <StatusPill status={item.status} />
                </div>
                <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
                  {projectName && <span>{projectName}</span>}
                  <span className="inline-flex items-center gap-1">
                    <Clock className="h-3 w-3" />
                    Ran {formatDate(ranAt)}
                  </span>
                </div>
              </div>
            </div>
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs sm:shrink-0">
              <WorkflowChip workflowId={item.workflowId} />
              {item.workflowId && item.workflowRunId && (
                <RunChip
                  workflowId={item.workflowId}
                  runId={item.workflowRunId}
                />
              )}
            </div>
          </CardContent>
        </Card>
      </Link>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Shared bits
// ---------------------------------------------------------------------------

// One page-level `now` ticker drives the LiveClock, CountdownChips, and the
// stats strip. The interval pauses while the tab is hidden (browsers
// throttle background timers anyway; this makes it explicit and cheap).
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

// The "fancy" header clock. role="timer" + aria-label; content is NOT in a
// live region so it is never announced every second.
function LiveClock({ now }: { now: number }) {
  const d = new Date(now);
  const timeHM = d.toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
  const timeS = d.toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
  const date = d.toLocaleDateString([], {
    weekday: "short",
    month: "short",
    day: "numeric",
  });
  return (
    <div
      role="timer"
      aria-label="Current time"
      className="flex items-center gap-2 rounded-lg border bg-card px-3 py-2"
    >
      <span className="h-2 w-2 animate-pulse rounded-full bg-emerald-500 motion-reduce:animate-none" />
      <span className="font-mono text-sm font-medium tabular-nums text-foreground">
        {timeHM}
        <span className="hidden sm:inline">{timeS.slice(timeHM.length)}</span>
      </span>
      <span className="hidden text-xs text-muted-foreground sm:inline">
        {date}
      </span>
    </div>
  );
}

// Optional polish strip: upcoming count, next run + countdown, due today.
function StatsStrip({ items, now }: { items: WorkItemProto[]; now: number }) {
  if (items.length === 0) return null;
  const nextMs = tsToMs(items[0].scheduledStartAt);
  const dueToday = items.filter((i) =>
    isSameLocalDay(tsToMs(i.scheduledStartAt), now),
  ).length;
  return (
    <div className="flex flex-wrap items-center gap-x-5 gap-y-1.5 rounded-lg border bg-card px-4 py-2.5 text-xs text-muted-foreground">
      <span className="inline-flex items-center gap-1.5">
        <span className="h-2 w-2 rounded-full bg-primary/70" />
        {items.length} upcoming
      </span>
      <span className="inline-flex items-center gap-1.5">
        <span className="h-2 w-2 rounded-full bg-emerald-500/70" />
        next {formatTime(nextMs)} · {formatCountdown(nextMs, now)}
      </span>
      <span className="inline-flex items-center gap-1.5">
        <span className="h-2 w-2 rounded-full bg-amber-500/70" />
        {dueToday} due today
      </span>
    </div>
  );
}

// Countdown chip with a descriptive accessible name ("Next run in 2 hours
// 13 minutes") recomputed on render — updated only on focus/read, never
// spammed via a live region.
function CountdownChip({ target, now }: { target: number; now: number }) {
  const diff = target - now;
  const text = diff <= 0 ? "now" : formatCountdown(target, now);
  return (
    <span
      aria-label={countdownAria(target, now)}
      title={countdownAria(target, now)}
      className={cn(
        "rounded-full px-1.5 py-0.5 text-xs font-medium",
        diff <= 0
          ? "bg-emerald-100 text-emerald-800"
          : "bg-muted text-muted-foreground",
      )}
    >
      {text}
    </span>
  );
}

function KindDot({ kind }: { kind: number }) {
  return (
    <span
      aria-hidden
      className={cn(
        "h-2.5 w-2.5 shrink-0 rounded-full",
        kindDotColor(kind),
      )}
    />
  );
}

// Kind dot palette reuses the work-items KindBadge hues (Epic purple,
// Feature indigo, Task blue, Subtask cyan, recovery amber/rose) — no new
// colors invented for this page.
function kindDotColor(kind: number): string {
  const colors: Record<number, string> = {
    1: "bg-purple-500",
    2: "bg-indigo-500",
    3: "bg-blue-500",
    4: "bg-cyan-500",
    5: "bg-amber-500",
    6: "bg-amber-500",
    7: "bg-rose-500",
    8: "bg-amber-500",
  };
  return colors[kind] ?? "bg-muted";
}

function KindBadge({ kind }: { kind: number }) {
  const labels: Record<number, string> = {
    1: "E",
    2: "F",
    3: "T",
    4: "S",
    5: "R",
    6: "R",
    7: "R",
    8: "R",
  };
  const styles: Record<number, string> = {
    1: "bg-purple-100 text-purple-800",
    2: "bg-indigo-100 text-indigo-800",
    3: "bg-blue-100 text-blue-800",
    4: "bg-cyan-100 text-cyan-800",
    5: "bg-amber-100 text-amber-800",
    6: "bg-amber-100 text-amber-800",
    7: "bg-rose-100 text-rose-800",
    8: "bg-amber-100 text-amber-800",
  };
  return (
    <span
      className={cn(
        "inline-flex h-5 w-5 items-center justify-center rounded text-xs font-bold",
        styles[kind] ?? "bg-muted text-muted-foreground",
      )}
    >
      {labels[kind] ?? "?"}
    </span>
  );
}

function StatusPill({ status }: { status: number }) {
  const labels: Record<number, string> = {
    1: "pending",
    10: "scheduled",
    2: "ready",
    3: "assigned",
    4: "running",
    5: "checkpointing",
    6: "succeeded",
    7: "failed",
    8: "cancelled",
    9: "recovering",
  };
  const styles: Record<number, string> = {
    1: "bg-gray-100 text-gray-700",
    10: "bg-purple-100 text-purple-800",
    2: "bg-blue-100 text-blue-800",
    3: "bg-yellow-100 text-yellow-800",
    4: "bg-green-100 text-green-800",
    5: "bg-orange-100 text-orange-800",
    6: "bg-green-600 text-white",
    7: "bg-red-100 text-red-800",
    8: "bg-gray-200 text-gray-600",
    9: "bg-orange-600 text-white",
  };
  return (
    <span
      className={cn(
        "rounded-full px-2 py-0.5 text-xs font-medium",
        styles[status] ?? "bg-muted text-muted-foreground",
      )}
    >
      {labels[status] ?? "unknown"}
    </span>
  );
}

// Small outline chip linking to the workflow template. When a scheduled
// item is not workflow-bound (rare — scheduling is a workflow feature),
// render a muted "unbound" chip instead of a dead link.
function WorkflowChip({ workflowId }: { workflowId: string }) {
  if (!workflowId) {
    return (
      <span className="text-xs italic text-muted-foreground/70">unbound</span>
    );
  }
  return (
    <Link
      to="/workflows/$id"
      params={{ id: workflowId }}
      title={workflowId}
      className="inline-flex max-w-[12rem] items-center gap-1 rounded-md border border-input bg-background px-1.5 py-0.5 font-mono text-xs text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
    >
      <Workflow className="h-3 w-3 shrink-0" />
      <span className="truncate">{workflowId}</span>
    </Link>
  );
}

// History-only chip linking to the workflow run that consumed this item.
function RunChip({ workflowId, runId }: { workflowId: string; runId: string }) {
  return (
    <Link
      to="/workflows/$id/runs/$runId"
      params={{ id: workflowId, runId }}
      title={`${workflowId} / ${runId}`}
      className="inline-flex max-w-[12rem] items-center gap-1 rounded-md border border-input bg-background px-1.5 py-0.5 font-mono text-xs text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
    >
      <CalendarClock className="h-3 w-3 shrink-0" />
      <span className="truncate">run {runId.slice(-8)}</span>
    </Link>
  );
}

// Recurring scheduled tasks are a future backend feature (design §5).
// Today every scheduled item is one-time; when a recurrence/cron field
// lands on the work item, ONLY this helper changes — the frequency slot
// already exists on every card, so the layout does not reflow. The `item`
// parameter is the future recurrence contract (kept for the signature).
// eslint-disable-next-line @typescript-eslint/no-unused-vars
function recurrenceBadge(_item: WorkItemProto): string {
  return "One-time";
}

// ---------------------------------------------------------------------------
// Time helpers
// ---------------------------------------------------------------------------

function tsToMs(ts?: Timestamp): number {
  if (!ts) return 0;
  return Number(ts.seconds) * 1000;
}

function formatTime(ts: number): string {
  return new Date(ts).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

function formatDate(ts: number): string {
  const d = new Date(ts);
  const date = d.toLocaleDateString([], { month: "short", day: "numeric" });
  return `${date} ${formatTime(ts)}`;
}

function isSameLocalDay(a: number, b: number): boolean {
  const da = new Date(a);
  const db = new Date(b);
  return (
    da.getFullYear() === db.getFullYear() &&
    da.getMonth() === db.getMonth() &&
    da.getDate() === db.getDate()
  );
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

// Group already-sorted upcoming items into "Today" / "Tomorrow" / weekday
// date groups (local calendar days).
function groupByDay(items: WorkItemProto[], now: number) {
  const startOfDay = (ts: number) => {
    const d = new Date(ts);
    d.setHours(0, 0, 0, 0);
    return d.getTime();
  };
  const today = startOfDay(now);
  const groups: { key: string; label: string; items: WorkItemProto[] }[] = [];
  for (const item of items) {
    const ts = tsToMs(item.scheduledStartAt);
    const day = startOfDay(ts);
    const diffDays = Math.round((day - today) / 86_400_000);
    let label: string;
    if (diffDays === 0) label = "Today";
    else if (diffDays === 1) label = "Tomorrow";
    else
      label = new Date(ts).toLocaleDateString([], {
        weekday: "short",
        month: "short",
        day: "numeric",
      });
    const existing = groups.find((g) => g.key === String(day));
    if (existing) existing.items.push(item);
    else groups.push({ key: String(day), label, items: [item] });
  }
  return groups;
}
