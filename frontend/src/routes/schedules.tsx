// Schedules page (design-notes/create-new-page-for-schedules.md).
//
// Shows all currently scheduled work items in chronological order with
// their next runtimes ("Upcoming", the default view), a view of items
// that have fired and are actively executing ("Running"), plus a history
// of workflow runs that already executed ("History"). All data comes from
// existing Connect-ES clients (AGENTS.md invariants #1/#2): Upcoming is a
// status = WORK_ITEM_STATUS_SCHEDULED query plus a client-side "queued"
// section of pending sequence children; Running is a broad item fetch
// filtered client-side to items with an in-flight workflow run (status
// RUNNING / CHECKPOINTING / RECOVERING, workflow bound); History is
// run-driven — it enumerates the tenant's workflow_runs via the extended
// ListWorkflowRuns RPC (scoped by project, sorted by each run's real
// started_at) and renders one card per executed run, resolving each run's
// bound work item / workflow for its title and kind.
//
// Sequence runs (a parent with children and no bound workflow) fan out to
// per-child workflows: the engine resets every descendant to pending and
// arms one child at a time. Only the parent carries scheduled_start_at,
// so the children never match the SCHEDULED query — the Upcoming view
// derives them from the full project list (see QueuedSection). In History,
// a completed child's executed run appears like any other run (it carries
// a real started_at in workflow_runs).
//
// Recurring work items (status = RECURRING, next_run_at set) are fetched
// alongside scheduled items and merged into the Upcoming view. The
// recurrenceBadge already renders the schedule's frequency; recurring
// items use next_run_at as their effective fire time instead of
// scheduled_start_at.
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
  useDeleteWorkItem,
  useListWorkItems,
  useRemoveSchedule,
} from "@/api/workItems"
import { RecurringFilter } from "@/api/gen/orchicon/api/v1/work_item_service_pb";
import { useListProjects } from "@/api/projects";
import {
  useListWorkflowRuns,
  useListWorkflows,
} from "@/api/workflows";
import {
  WorkItemStatus,
  type WorkItem as WorkItemProto,
} from "@/api/gen/orchicon/api/v1/work_item_pb";
import type { WorkflowRun, Workflow as WorkflowProto } from "@/api/gen/orchicon/api/v1/workflow_pb";
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
import { KindBadge, KindDot, StatusPill, MultiWorkflowChip, PositionBadge, RecurringBadge, RunStatusBadge } from "@/components/work-items/work-item-badges";
import { showRecurringBadge } from "@/components/work-items/work-item-meta";
import {
  computeSequencePositions,
  sequenceParentIds,
} from "@/components/work-items/sequence-utils";
import { cn } from "@/lib/utils";
import { LiveDuration } from "@/components/ui/live-duration";
import { formatRecurrence } from "@/components/work-items/RecurringScheduleForm";
import { Route as rootRoute } from "@/routes/__root";
import {
  ACTIVE_RUNNING_STATUSES,
  historyRunRanAt,
  isHistoryRun,
  queuedSequenceChildren,
  upcomingSortTime,
} from "@/lib/schedules-model";

const schedulesSearchSchema = z.object({
  view: z.enum(["upcoming", "running", "history"]).optional(),
  projectId: z.string().optional(),
});

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/schedules",
  validateSearch: schedulesSearchSchema,
  component: SchedulesPage,
});

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
  const [sortOrder, setSortOrder] = useState(view === "history" ? "desc" : "asc");
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const now = useNow();

  // Reset the sort direction to the view's default when the view changes.
  useEffect(() => {
    setSortOrder(view === "history" ? "desc" : "asc");
    setSelected(new Set());
  }, [view]);

  const hasProjects = projects && projects.length > 0;
  const cancelScheduled = useDeleteWorkItem(projectId);
  const removeSchedule = useRemoveSchedule(projectId);

  const goView = (nextView: "upcoming" | "running" | "history") => {
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

  const handleRemoveSchedule = () => {
    if (selected.size === 0) return;
    const count = selected.size;
    if (
      !window.confirm(
        `Remove ${count} schedule${count === 1 ? "" : "s"}? The work items will remain unchanged.`,
      )
    ) {
      return;
    }
    Array.from(selected).forEach((id) => removeSchedule.mutate(id));
    setSelected(new Set());
  };

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Schedules</h1>
          <p className="text-sm text-muted-foreground">
            Scheduled work items — upcoming, running, and past runs.
          </p>
        </div>
        <LiveClock now={now} />
      </div>

      {/* View toggle + filter bar (AGENTS.md UI-consistency rule). */}
      <div className="flex flex-wrap items-center gap-3 rounded-2xl glass-panel p-3 border border-white/10">
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
            aria-pressed={view === "running"}
            className={cn(
              "px-3 py-1.5 text-sm font-medium transition-colors",
              view === "running"
                ? "bg-accent text-accent-foreground"
                : "text-muted-foreground hover:bg-accent/50",
            )}
            onClick={() => goView("running")}
          >
            Running
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
          className="h-9 rounded-xl glass-input px-3 text-sm"
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
          className="h-9 rounded-xl glass-input px-3 text-sm"
        >
          <option value="">All kinds</option>
          {SCHEDULABLE_KINDS.map((k) => (
            <option key={k.value} value={k.value}>
              {k.label}
            </option>
          ))}
        </select>

        <select
          value={view === "upcoming" ? "next_run" : view === "history" ? "last_run" : "start_time"}
          className="h-9 rounded-xl glass-input px-3 text-sm"
          aria-label="Sort by"
        >
          <option value={view === "upcoming" ? "next_run" : view === "history" ? "last_run" : "start_time"}>
            {view === "upcoming"
              ? "Sort: next run"
              : view === "history"
                ? "Sort: last run"
                : "Sort: start time"}
          </option>
        </select>

        <select
          value={sortOrder}
          onChange={(e) => setSortOrder(e.target.value)}
          className="h-9 rounded-xl glass-input px-3 text-sm"
          aria-label="Sort order"
        >
          <option value="asc">Asc</option>
          <option value="desc">Desc</option>
        </select>

        {selected.size > 0 &&
          (view === "history" ? (
            <Button
              variant="destructive"
              size="sm"
              onClick={handleRemoveSchedule}
              disabled={removeSchedule.isPending}
            >
              <Trash2 className="mr-1 h-3.5 w-3.5" />
              Remove {selected.size} schedule{selected.size === 1 ? "" : "s"}
            </Button>
          ) : (
            <Button
              variant="destructive"
              size="sm"
              onClick={handleCancelSelected}
              disabled={cancelScheduled.isPending}
            >
              <Ban className="mr-1 h-3.5 w-3.5" />
              Cancel {selected.size} selected
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

      {hasProjects && view === "running" && (
        <RunningView
          projectId={projectId}
          search={search}
          kindFilter={kindFilter}
          sortOrder={sortOrder}
          projects={projects}
          selected={selected}
          onToggleSelect={toggleSelect}
          onToggleSelectAll={toggleSelectAll}
          now={now}
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
    isLoading: scheduledLoading,
    error: scheduledError,
  } = useListWorkItems(projectId, {
    status: WorkItemStatus.SCHEDULED,
    search: search || undefined,
    refetchInterval: 5_000,
    recurringFilter: RecurringFilter.EXCLUDE_RECURRING,
  });
  const {
    data: recurring,
    isLoading: recurringLoading,
    error: recurringError,
  } = useListWorkItems(projectId, {
    status: WorkItemStatus.RECURRING,
    search: search || undefined,
    refetchInterval: 5_000,
  });
  // The full project list is needed for the sequence derivations below (a
  // scheduled sequence parent's children are pending, never scheduled), so
  // the query is isolated behind its own load/error flags.
  const {
    data: allItems,
    isLoading: allLoading,
    error: allError,
  } = useListWorkItems(projectId, {
    search: search || undefined,
    refetchInterval: 5_000,
  });
  const parentIds = useMemo(() => sequenceParentIds(allItems ?? []), [allItems]);
  const positions = useMemo(
    () => computeSequencePositions(allItems ?? []),
    [allItems],
  );

  // Queued sequence children: a running sequence parent's not-yet-armed
  // children. The sequence engine resets every descendant to pending on
  // fire and arms one child at a time (parent + current child show under
  // Running), so the remaining children are pending — NOT scheduled — and
  // never match the SCHEDULED query above. Deriving them from the full
  // project list keeps them visible under Upcoming until their turn.
  const queued = useMemo(
    () => queuedSequenceChildren(allItems ?? [], kindFilter),
    [allItems, kindFilter],
  );

  // Kind filter + chronological sort are client-side (the server sort_by
  // only supports title/priority/created_at; scheduled_start_at is not
  // one of them). Recurring items use next_run_at as their effective
  // fire time.
  const items = useMemo(() => {
    const scheduledFiltered = kindFilter
      ? (scheduled ?? []).filter((i) => i.kind === Number(kindFilter))
      : (scheduled ?? []);
    // recurring items are now on Automation → Recurring Items (not Upcoming)
    void recurring;
    const all = [...scheduledFiltered];
    const sorted = [...all].sort(
      (a, b) => upcomingSortTime(a) - upcomingSortTime(b),
    );
    return sortOrder === "asc" ? sorted : sorted.reverse();
  }, [scheduled, kindFilter, sortOrder]);

  const isLoading = scheduledLoading || recurringLoading;
  const loadError = scheduledError || recurringError;

  if (isLoading || allLoading) {
    return <p className="text-sm text-muted-foreground">Loading…</p>;
  }
  if (loadError || allError) {
    return (
      <p className="text-sm text-destructive">
        Failed to load schedules: {String(loadError || allError)}
      </p>
    );
  }
  if (items.length === 0 && queued.length === 0) {
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
          checked={selected.size === items.length + queued.length}
          onChange={() => onToggleSelectAll([...queued, ...items])}
          className="h-4 w-4 rounded border-input"
          aria-label="Select all upcoming schedules"
        />
        <span aria-live="polite" className="text-xs text-muted-foreground">
          {selected.size > 0
            ? `${selected.size} of ${items.length + queued.length} selected`
            : `${items.length + queued.length} scheduled item${items.length + queued.length === 1 ? "" : "s"}`}
        </span>
      </div>
      {queued.length > 0 && (
        <QueuedSection
          queued={queued}
          projects={projects}
          selected={selected}
          onToggleSelect={onToggleSelect}
          parentIds={parentIds}
          positions={positions}
        />
      )}
      {groups.map((group, gi) => (
        <AgendaGroup
          key={group.key}
          group={group}
          isLastGroup={gi === groups.length - 1}
          now={now}
          projects={projects}
          selected={selected}
          onToggleSelect={onToggleSelect}
          parentIds={parentIds}
        />
      ))}
    </div>
  );
}

// Queued section: the pending children of a running sequence parent. They
// have no scheduled_start_at of their own (only the sequence parent is
// scheduled), so they can't join the day-grouped agenda — this section
// sits above it and orders them by chain order.
function QueuedSection({
  queued,
  projects,
  selected,
  onToggleSelect,
  parentIds,
  positions,
}: {
  queued: WorkItemProto[];
  projects?: Project[];
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  parentIds: Set<string>;
  positions: Map<string, number>;
}) {
  return (
    <section aria-label="Queued sequence children">
      <div className="flex items-center gap-3">
        <h2 className="text-sm font-semibold uppercase tracking-wide text-muted-foreground">
          Queued
        </h2>
        <div className="h-px flex-1 bg-border" />
        <span className="text-xs text-muted-foreground">{queued.length}</span>
      </div>
      <ul className="mt-3 space-y-3">
        {queued.map((item, ii) => (
          <li key={item.id} className="flex gap-3">
            <div className="hidden w-16 shrink-0 pt-4 text-right font-mono text-xs tabular-nums text-muted-foreground sm:block">
              {positions.get(item.id) ? `#${positions.get(item.id)}` : "…"}
            </div>
            <div className="relative flex flex-col items-center">
              <KindDot
                kind={item.kind}
                className="mt-4 ring-2 ring-background"
              />
              {ii !== queued.length - 1 && (
                <span
                  aria-hidden
                  className="absolute left-1/2 top-6 h-[calc(100%+0.75rem)] w-px -translate-x-1/2 bg-border"
                />
              )}
            </div>
            <div className="min-w-0 flex-1">
              <QueuedCard
                item={item}
                projects={projects}
                selected={selected.has(item.id)}
                onToggleSelect={onToggleSelect}
                isSequenceChild={!!item.parentId && parentIds.has(item.parentId)}
                position={positions.get(item.id)}
              />
            </div>
          </li>
        ))}
      </ul>
    </section>
  );
}

function QueuedCard({
  item,
  projects,
  selected,
  onToggleSelect,
  isSequenceChild,
  position,
}: {
  item: WorkItemProto;
  projects?: Project[];
  selected: boolean;
  onToggleSelect: (id: string) => void;
  isSequenceChild: boolean;
  position?: number;
}) {
  const projectName = projects?.find((p) => p.id === item.projectId)?.name;
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
                  {isSequenceChild && <MultiWorkflowChip />}
                  {position ? <PositionBadge position={position} /> : null}
                  {showRecurringBadge(item) && <RecurringBadge />}
                  <StatusPill status={item.status} />
                </div>
                <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
                  {projectName && <span>{projectName}</span>}
                  <span className="inline-flex items-center gap-1">
                    <Clock className="h-3 w-3" />
                    Queued — waits for the current step
                  </span>
                </div>
              </div>
            </div>
          </CardContent>
        </Card>
      </Link>
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
  parentIds,
}: {
  group: { key: string; label: string; items: WorkItemProto[] };
  isLastGroup: boolean;
  now: number;
  projects?: Project[];
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  parentIds: Set<string>;
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
                {formatTime(upcomingSortTime(item))}
              </div>
              <div className="relative flex flex-col items-center">
                <KindDot
                  kind={item.kind}
                  className="mt-4 ring-2 ring-background"
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
                  isSequenceParent={parentIds.has(item.id)}
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
  isSequenceParent,
}: {
  item: WorkItemProto;
  now: number;
  projects?: Project[];
  selected: boolean;
  onToggleSelect: (id: string) => void;
  isSequenceParent: boolean;
}) {
  const projectName = projects?.find((p) => p.id === item.projectId)?.name;
  const fireTime = upcomingSortTime(item);
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
                  {isSequenceParent && <MultiWorkflowChip />}
                  {showRecurringBadge(item) && <RecurringBadge />}
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
                {formatTime(fireTime)}
              </span>
              <CountdownChip target={fireTime} now={now} />
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
// Running view
// ---------------------------------------------------------------------------

function RunningView({
  projectId,
  search,
  kindFilter,
  sortOrder,
  projects,
  selected,
  onToggleSelect,
  onToggleSelectAll,
  now,
}: {
  projectId: string;
  search: string;
  kindFilter: string;
  sortOrder: string;
  projects?: Project[];
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  onToggleSelectAll: (items: WorkItemProto[]) => void;
  now: number;
}) {
  const {
    data: allItems,
    isLoading,
    error,
  } = useListWorkItems(projectId, {
    search: search || undefined,
    refetchInterval: 5_000,
  });

  // Running = items with an in-flight workflow run. A run started without
  // a schedule (manual start / "Start immediately on save") leaves the
  // ticket running with workflow_run_id set but scheduled_start_at NULL,
  // so the membership must not require a scheduled start (ADR-002 in
  // architecture-notes/running-workflows-not-showing-in-schedules.md).
  // SEQUENCE parents (children + no bound workflow) have no
  // workflow_run_id, so the predicate is extended: an item in an active
  // status that is someone's parent counts too. The API accepts one
  // status filter, so this union is derived client-side.
  const parentIds = useMemo(() => sequenceParentIds(allItems ?? []), [allItems]);
  const positions = useMemo(() => computeSequencePositions(allItems ?? []), [allItems]);
  const items = useMemo(() => {
    const base = (allItems ?? []).filter((i) => {
      const boundRun = i.workflowRunId && ACTIVE_RUNNING_STATUSES.has(i.status);
      const sequenceParent =
        !i.workflowRunId && ACTIVE_RUNNING_STATUSES.has(i.status) && parentIds.has(i.id);
      return (boundRun || sequenceParent) && (!kindFilter || i.kind === Number(kindFilter));
    });
    const sorted = [...base].sort(
      (a, b) =>
        runningStartedAt(a) - runningStartedAt(b),
    );
    return sortOrder === "asc" ? sorted : sorted.reverse();
  }, [allItems, kindFilter, sortOrder, parentIds]);

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading…</p>;
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        Failed to load running schedules: {String(error)}
      </p>
    );
  }
  if (items.length === 0) {
    return (
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <SearchX className="h-5 w-5 text-muted-foreground" />
            No schedules running
          </CardTitle>
          <CardDescription>
            Work items with a workflow run in flight will appear here while
            they are running.
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
          aria-label="Select all running schedules"
        />
        <span aria-live="polite" className="text-xs text-muted-foreground">
          {selected.size > 0
            ? `${selected.size} of ${items.length} selected`
            : `${items.length} running schedule${items.length === 1 ? "" : "s"}`}
        </span>
      </div>
      <ul className="space-y-3">
        {items.map((item, ii) => (
          <li key={item.id} className="flex gap-3">
            <div className="hidden w-16 shrink-0 pt-4 text-right font-mono text-xs tabular-nums text-muted-foreground sm:block">
              {formatTime(runningStartedAt(item))}
            </div>
            <div className="relative flex flex-col items-center">
              <KindDot
                kind={item.kind}
                className="mt-4 ring-2 ring-background"
              />
              {ii !== items.length - 1 && (
                <span
                  aria-hidden
                  className="absolute left-1/2 top-6 h-[calc(100%+0.75rem)] w-px -translate-x-1/2 bg-border"
                />
              )}
            </div>
            <div className="min-w-0 flex-1">
              <RunningCard
                item={item}
                projects={projects}
                selected={selected.has(item.id)}
                onToggleSelect={onToggleSelect}
                isSequenceParent={parentIds.has(item.id)}
                position={positions.get(item.id)}
                isSequenceChild={!!item.parentId && parentIds.has(item.parentId)}
                now={now}
              />
            </div>
          </li>
        ))}
      </ul>
    </div>
  );
}

function RunningCard({
  item,
  projects,
  selected,
  onToggleSelect,
  isSequenceParent,
  position,
  isSequenceChild,
  now,
}: {
  item: WorkItemProto;
  projects?: Project[];
  selected: boolean;
  onToggleSelect: (id: string) => void;
  isSequenceParent: boolean;
  position?: number;
  isSequenceChild: boolean;
  now: number;
}) {
  const projectName = projects?.find((p) => p.id === item.projectId)?.name;
  const startedAt = runningStartedAt(item);
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
                  {(isSequenceParent || isSequenceChild) && <MultiWorkflowChip />}
                  {isSequenceChild && position ? <PositionBadge position={position} /> : null}
                  {showRecurringBadge(item) && <RecurringBadge />}
                  <StatusPill status={item.status} />
                </div>
                <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
                  {projectName && <span>{projectName}</span>}
                  <span className="inline-flex items-center gap-1">
                    <Clock className="h-3 w-3" />
                    Started {formatDate(startedAt)}
                  </span>
                </div>
              </div>
            </div>
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs sm:shrink-0">
              <LiveDuration startedAt={startedAt} now={now} className="font-mono text-xs tabular-nums text-muted-foreground" />
              {!isSequenceParent && <WorkflowChip workflowId={item.workflowId} />}
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
  // History is run-driven: enumerate the tenant's workflow runs for the
  // selected scope (project, or all projects when empty) ordered by each
  // run's real started_at. Every executed run appears — recurring fires
  // whose work item re-armed to SCHEDULED/RECURRING, prior runs of a work
  // item that ran more than once, and in-flight runs all carry a real
  // started_at (the authoritative workflow_runs record), so none of them
  // are dropped by a work-item-status derivation.
  const {
    data: runs,
    isLoading: runsLoading,
    error: runsError,
  } = useListWorkflowRuns({
    projectId: projectId || undefined,
    sortBy: "started_at",
    sortOrder,
    pageSize: 1000,
    refetchInterval: 30_000,
  });
  // The FULL project work item list (no search/kind filter) so every run's
  // bound item resolves; search/kind are applied client-side below.
  const {
    data: allItems,
    isLoading: itemsLoading,
    error: itemsError,
  } = useListWorkItems(projectId, {
    refetchInterval: 30_000,
  });
  // All tenant workflows (including templates) so unbound one-shot runs —
  // and bound runs whose workflow is a tenant-level template — render a
  // human-readable workflow name.
  const { data: workflows } = useListWorkflows();

  const itemsById = useMemo(() => {
    const m = new Map<string, WorkItemProto>();
    for (const i of allItems ?? []) m.set(i.id, i);
    return m;
  }, [allItems]);
  const workflowsById = useMemo(() => {
    const m = new Map<string, WorkflowProto>();
    for (const w of workflows ?? []) m.set(w.id, w);
    return m;
  }, [workflows]);

  // Resolve each run to a bound item (when it has one) and apply
  // search/kind client-side, then order by the run's actual start time.
  const entries = useMemo(() => {
    const base = (runs ?? []).filter((run) => {
      if (!isHistoryRun(run)) return false;
      const item = run.workItemId ? itemsById.get(run.workItemId) : undefined;
      if (kindFilter && (!item || item.kind !== Number(kindFilter))) return false;
      if (search) {
        const itemMatch = item?.title.toLowerCase().includes(search.toLowerCase());
        const workflowName = workflowsById.get(run.workflowId)?.name.toLowerCase();
        const workflowMatch =
          (workflowName ?? "").includes(search.toLowerCase()) ||
          run.workflowId.toLowerCase().includes(search.toLowerCase());
        if (!itemMatch && !workflowMatch) return false;
      }
      return true;
    });
    const sorted = [...base].sort(
      (a, b) => historyRunRanAt(b) - historyRunRanAt(a),
    );
    return sortOrder === "desc" ? sorted : sorted.reverse();
  }, [runs, itemsById, workflowsById, kindFilter, search, sortOrder]);

  const isLoading = runsLoading || itemsLoading;
  const error = runsError || itemsError;

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
  if (entries.length === 0) {
    return (
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <SearchX className="h-5 w-5 text-muted-foreground" />
            No past runs yet
          </CardTitle>
          <CardDescription>
            Workflow runs that have executed will appear here.
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
          checked={selected.size === entries.length}
          onChange={() => onToggleSelectAll(
            entries
              .map((r) => (r.workItemId ? itemsById.get(r.workItemId) : undefined))
              .filter((i): i is WorkItemProto => !!i),
          )}
          className="h-4 w-4 rounded border-input"
          aria-label="Select all history items"
        />
        <span aria-live="polite" className="text-xs text-muted-foreground">
          {selected.size > 0
            ? `${selected.size} of ${entries.length} selected`
            : `${entries.length} past run${entries.length === 1 ? "" : "s"}`}
        </span>
      </div>
      <ul className="space-y-3">
        {entries.map((run, ii) => (
          <li key={run.id} className="flex gap-3">
            <div className="hidden w-16 shrink-0 pt-4 text-right font-mono text-xs tabular-nums text-muted-foreground sm:block">
              {formatTime(historyRunRanAt(run))}
            </div>
            <div className="relative flex flex-col items-center">
              <KindDot
                kind={itemsById.get(run.workItemId ?? "")?.kind ?? 0}
                className="mt-4 ring-2 ring-background"
              />
              {ii !== entries.length - 1 && (
                <span
                  aria-hidden
                  className="absolute left-1/2 top-6 h-[calc(100%+0.75rem)] w-px -translate-x-1/2 bg-border"
                />
              )}
            </div>
            <div className="min-w-0 flex-1">
              <HistoryCard
                run={run}
                item={run.workItemId ? itemsById.get(run.workItemId) : undefined}
                workflow={workflowsById.get(run.workflowId)}
                projects={projects}
                selected={run.workItemId ? selected.has(run.workItemId) : false}
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
  run,
  item,
  workflow,
  projects,
  selected,
  onToggleSelect,
}: {
  run: WorkflowRun;
  item?: WorkItemProto;
  workflow?: WorkflowProto;
  projects?: Project[];
  selected: boolean;
  onToggleSelect: (id: string) => void;
}) {
  const projectName = projects?.find((p) => p.id === run.projectId)?.name;
  const ranAt = historyRunRanAt(run);
  const startedAt = run.startedAt ? tsToMs(run.startedAt) : ranAt;
  const endedAt = run.endedAt ? tsToMs(run.endedAt) : undefined;
  const title = item?.title ?? workflow?.name ?? run.workflowId;
  const itemId = item?.id;
  return (
    <div className="group flex items-center gap-2">
      <input
        type="checkbox"
        checked={selected}
        onChange={() => itemId && onToggleSelect(itemId)}
        disabled={!itemId}
        className="h-4 w-4 shrink-0 rounded border-input"
        aria-label={`Select ${title}`}
      />
      <Link
        to="/work-items/$id"
        params={{ id: itemId ?? "" }}
        disabled={!itemId}
        className="min-w-0 flex-1"
      >
        <Card className="transition-colors hover:bg-accent">
          <CardContent className="flex flex-col gap-2 p-4 sm:flex-row sm:items-center sm:justify-between">
            <div className="flex min-w-0 items-center gap-3">
              <KindDot kind={item?.kind ?? 0} />
              <div className="min-w-0 flex-1 overflow-hidden">
                <div className="flex items-center gap-2">
                  <span className="truncate text-sm font-medium group-hover:underline">
                    {title}
                  </span>
                  {item && <KindBadge kind={item.kind} />}
                  {item && showRecurringBadge(item) && <RecurringBadge />}
                  <RunStatusBadge status={run.status} />
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
              <LiveDuration startedAt={startedAt} endedAt={endedAt} className="font-mono text-xs tabular-nums text-muted-foreground" />
              <WorkflowChip workflowId={run.workflowId} />
              {run.workflowId && (
                <RunChip
                  workflowId={run.workflowId}
                  runId={run.id}
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
      className="flex items-center gap-2 rounded-2xl glass-panel px-3 py-2"
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
  const nextMs = upcomingSortTime(items[0]);
  const dueToday = items.filter((i) =>
    isSameLocalDay(upcomingSortTime(i), now),
  ).length;
  return (
    <div className="flex flex-wrap items-center gap-x-5 gap-y-1.5 rounded-2xl glass-panel px-4 py-2.5 text-xs text-muted-foreground">
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

// Recurring scheduled tasks: when a recurrence/cron field lands on the work
// item, this helper returns the human-readable badge string. The frequency
// slot already exists on every card, so the layout does not reflow.
function recurrenceBadge(item: WorkItemProto): string {
  return formatRecurrence(item.recurringSchedule);
}

// ---------------------------------------------------------------------------
// Time helpers
// ---------------------------------------------------------------------------

function tsToMs(ts?: Timestamp): number {
  if (!ts) return 0;
  return Number(ts.seconds) * 1000;
}

// upcomingSortTime lives in schedules-model.ts (unit-tested): it returns
// the effective fire time for an Upcoming-view item. Scheduled items use
// scheduled_start_at; recurring items use next_run_at (the computed next
// occurrence).

// runningStartedAt is the effective start time for a Running-view item. A
// running ticket started without a schedule has no scheduled_start_at, so
// fall back to updatedAt (the reconciler bumps it while the run is in
// flight) and then createdAt.
function runningStartedAt(item: WorkItemProto): number {
  return (
    tsToMs(item.scheduledStartAt) ||
    tsToMs(item.updatedAt) ||
    tsToMs(item.createdAt)
  );
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
    const ts = upcomingSortTime(item);
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
