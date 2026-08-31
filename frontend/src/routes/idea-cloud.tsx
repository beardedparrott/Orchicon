// Idea Cloud (feature 5.2).
//
// List-only page for automation-produced ideas, with two sections. ACTIVE
// (default): automation-produced ideas in status=idea — spawned when a
// recurring fire with "Outputs: ideas" fired, excluded from every normal
// work-item view until a human triages them here. REJECTED: idea spawns a
// human dismissed (cancelled + retained provenance), kept as readable
// history — and as the memory the automation dedupe gate checks before
// spawning, so a rejected idea is never silently re-proposed. Approve
// promotes an idea to a normal pending work item (it leaves Idea Cloud and
// appears in Work Items); Dismiss discards it into Rejected. Both are
// server-enforced status transitions — the card moves sections on the next
// refetch because ListIdeas is scoped by server-side status predicates.
//
// Cheap-by-design: list-only with per-card expand (click a card to reveal
// the full description + acceptance criteria). No dedicated detail route.

import { createRoute } from "@tanstack/react-router";
import { ChevronDown, Lightbulb, Search, X } from "lucide-react";
import { useEffect, useState } from "react";

import { useDismissIdea, useListIdeas, usePromoteIdea } from "@/api/ideas";
import { useListProjects } from "@/api/projects";
import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import {
  KindBadge,
  StatusPill,
} from "@/components/work-items/work-item-badges";
import { priorityLabel, relativeAge } from "@/components/work-items/work-item-meta";
import { useDarkPalette } from "@/components/work-items/use-dark-palette";
import { useDebouncedValue } from "@/components/work-items/use-debounced-value";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/idea-cloud",
  component: IdeaCloudPage,
});

function IdeaCloudPage() {
  const { data: projects } = useListProjects();
  const [projectId, setProjectId] = useState<string>("");
  const [search, setSearch] = useState("");
  const [sortBy, setSortBy] = useState("created_at");
  const [sortOrder, setSortOrder] = useState("desc");
  // Which section is shown: the Idea Cloud (active) or the rejected
  // graveyard. Rejected cards are read-only history — no promote path.
  const [section, setSection] = useState<"active" | "rejected">("active");
  const debouncedSearch = useDebouncedValue(search, 300);

  const hasProjects = projects && projects.length > 0;
  const promote = usePromoteIdea();
  const dismiss = useDismissIdea();
  const toast = useToast();

  const {
    data: ideas,
    isLoading,
    error,
    dataUpdatedAt,
    isFetching,
  } = useListIdeas(projectId, {
    search: debouncedSearch || undefined,
    sortBy: sortBy || undefined,
    sortOrder: sortOrder || undefined,
    state: section,
  });

  const selectClass =
    "h-9 rounded-xl glass-input px-3 text-sm focus-visible:ring-2 focus-visible:ring-ring";

  const handleApprove = (idea: WorkItem) => {
    if (
      !window.confirm(
        `Approve this idea?\n\n"${idea.title}" will be promoted to a normal work item in Work Items.`,
      )
    )
      return;
    promote.mutate(idea.id, {
      onSuccess: (item) => {
        toast.success(`Promoted "${item.title}" to a work item.`);
      },
      onError: (e) => {
        toast.error(`Failed to approve: ${String(e)}`);
      },
    });
  };

  const handleDismiss = (idea: WorkItem) => {
    if (
      !window.confirm(
        `Dismiss this idea?\n\n"${idea.title}" will be cancelled and moved to the Rejected section — it stays visible there as history, and research fires will see it as rejected instead of re-proposing it.`,
      )
    )
      return;
    dismiss.mutate(idea.id, {
      onSuccess: (item) => {
        toast.success(`Dismissed "${item.title}" (moved to Rejected).`);
      },
      onError: (e) => {
        toast.error(`Failed to dismiss: ${String(e)}`);
      },
    });
  };

  const empty = ideas && ideas.length === 0;

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-center justify-between gap-4 shrink-0">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Idea Cloud</h1>
          <p className="max-w-2xl text-sm text-muted-foreground">
            Ideas are automation-produced items awaiting your triage — spawned
            when a recurring item with &ldquo;Outputs: ideas&rdquo; fires.
            <span className="font-medium text-foreground"> Approve</span> promotes
            an idea to a normal work item;{" "}
            <span className="font-medium text-foreground">Dismiss</span> discards
            it into Rejected, where it stays as readable history.
          </p>
        </div>
        <LiveRefreshIndicator lastUpdated={dataUpdatedAt} isFetching={isFetching} />
      </div>

      <div className="flex flex-wrap items-center gap-3 shrink-0">
        <div
          role="tablist"
          aria-label="Idea section"
          className="flex h-9 items-center rounded-xl glass-input p-1"
        >
          <button
            type="button"
            role="tab"
            aria-selected={section === "active"}
            onClick={() => setSection("active")}
            className={cn(
              "h-7 rounded-lg px-3 text-sm transition-colors",
              section === "active"
                ? "bg-background shadow-sm font-medium text-foreground"
                : "text-muted-foreground hover:text-foreground",
            )}
          >
            Active
          </button>
          <button
            type="button"
            role="tab"
            aria-selected={section === "rejected"}
            onClick={() => setSection("rejected")}
            className={cn(
              "h-7 rounded-lg px-3 text-sm transition-colors",
              section === "rejected"
                ? "bg-background shadow-sm font-medium text-foreground"
                : "text-muted-foreground hover:text-foreground",
            )}
          >
            Rejected
          </button>
        </div>

        <select
          value={projectId}
          onChange={(e) => setProjectId(e.target.value)}
          disabled={!projects || projects.length === 0}
          aria-label="Project"
          className={selectClass}
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

        <div className="relative min-w-[160px] flex-1">
          <Search
            aria-hidden="true"
            className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground"
          />
          <Input
            placeholder="Search ideas…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            className="h-11 sm:h-9 min-h-[44px] w-full pl-8"
          />
        </div>

        <select
          value={sortBy}
          onChange={(e) => setSortBy(e.target.value)}
          aria-label="Sort by"
          className={selectClass}
        >
          <option value="created_at">Sort: created</option>
          <option value="title">Sort: title</option>
          <option value="priority">Sort: priority</option>
        </select>

        <select
          value={sortOrder}
          onChange={(e) => setSortOrder(e.target.value)}
          aria-label="Sort order"
          className={selectClass}
        >
          <option value="desc">Desc</option>
          <option value="asc">Asc</option>
        </select>
      </div>

      {!hasProjects && (
        <Card>
          <CardHeader>
            <CardTitle>No project selected</CardTitle>
            <CardDescription>
              Create a project first to start triaging ideas.
            </CardDescription>
          </CardHeader>
        </Card>
      )}

      {hasProjects && (
        <>
          {!projectId && (
            <p className="text-xs text-muted-foreground">
              Showing ideas across all projects — select a project above to
              scope the list.
            </p>
          )}
          {isLoading && (
            <p className="text-sm text-muted-foreground">Loading ideas…</p>
          )}
          {error && (
            <Card>
              <CardHeader>
                <CardTitle>Couldn&rsquo;t load ideas</CardTitle>
                <CardDescription>{String(error)}</CardDescription>
              </CardHeader>
            </Card>
          )}
          {!isLoading && !error && empty && section === "active" && (
            <Card className="flex flex-col items-center justify-center gap-3 py-12 text-center">
              <span className="flex h-12 w-12 items-center justify-center rounded-full bg-lime-500/15 text-lime-600 dark:text-lime-300">
                <Lightbulb aria-hidden="true" className="h-6 w-6" />
              </span>
              <div>
                <p className="font-medium text-foreground">No ideas yet</p>
                <p className="max-w-sm text-sm text-muted-foreground">
                  Ideas appear here when a recurring item with &ldquo;Outputs:
                  ideas&rdquo; fires. Approve to promote or Dismiss to discard.
                </p>
              </div>
            </Card>
          )}
          {!isLoading && !error && empty && section === "rejected" && (
            <Card className="flex flex-col items-center justify-center gap-3 py-12 text-center">
              <span className="flex h-12 w-12 items-center justify-center rounded-full bg-rose-500/15 text-rose-600 dark:text-rose-300">
                <X aria-hidden="true" className="h-6 w-6" />
              </span>
              <div>
                <p className="font-medium text-foreground">Nothing rejected yet</p>
                <p className="max-w-sm text-sm text-muted-foreground">
                  Dismissed ideas land here as history. Research fires read
                  this section before spawning, so a rejection is remembered.
                </p>
              </div>
            </Card>
          )}
          {!isLoading && !error && !empty && (
            <ul className="flex flex-col gap-3">
              {(ideas ?? []).map((idea) => (
                <IdeaCard
                  key={idea.id}
                  idea={idea}
                  rejected={section === "rejected"}
                  approving={promote.isPending}
                  dismissing={dismiss.isPending}
                  onApprove={() => handleApprove(idea)}
                  onDismiss={() => handleDismiss(idea)}
                />
              ))}
            </ul>
          )}
        </>
      )}
    </div>
  );
}

function IdeaCard({
  idea,
  rejected,
  approving,
  dismissing,
  onApprove,
  onDismiss,
}: {
  idea: WorkItem;
  rejected: boolean;
  approving: boolean;
  dismissing: boolean;
  onApprove: () => void;
  onDismiss: () => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const isDark = useDarkPalette();
  const priority = priorityLabel(idea.priority);
  const age = relativeAge(idea.createdAt);
  const runId = idea.spawnedByRunId;
  const runSnippet = runId ? runId.slice(0, 8) : "";

  return (
    <Card className="p-4">
      <button
        type="button"
        onClick={() => setExpanded((e) => !e)}
        aria-expanded={expanded}
        className="block w-full text-left"
      >
        <div className="flex items-center gap-2">
          <KindBadge kind={idea.kind} />
          <StatusPill status={idea.status} />
          {priority && (
            <span className="shrink-0 font-mono text-xs tabular-nums text-muted-foreground">
              {priority}
            </span>
          )}
          <span className="ml-auto flex shrink-0 items-center gap-1">
            <ChevronDown
              aria-hidden="true"
              className={cn(
                "h-4 w-4 text-muted-foreground transition-transform",
                expanded && "rotate-180",
              )}
            />
          </span>
        </div>
        <div className="mt-2 min-w-0 break-words [overflow-wrap:anywhere] text-sm font-medium leading-snug text-foreground">
          {idea.title}
        </div>
        {idea.description && (
          <p className="mt-1 line-clamp-2 text-sm text-muted-foreground">
            {idea.description}
          </p>
        )}
        <div className="mt-2 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
          {idea.spawnedByTitle && (
            <span className="inline-flex min-w-0 items-center gap-1">
              <span className="shrink-0">spawned by</span>
              <span
                className="min-w-0 truncate font-medium text-foreground"
                title={idea.spawnedByTitle}
              >
                {idea.spawnedByTitle}
              </span>
            </span>
          )}
          {idea.spawnedByRunId && (
            <span className="inline-flex min-w-0 items-center gap-1">
              <span className="shrink-0">run</span>
              <span
                className="font-mono text-foreground"
                title={idea.spawnedByRunId}
              >
                {runSnippet}
              </span>
            </span>
          )}
          {age && (
            <span className="shrink-0" title={absoluteTime(idea)}>
              {age}
            </span>
          )}
        </div>
      </button>

      {expanded && (
        <div className="mt-3 space-y-3 border-t border-border pt-3">
          {idea.description && (
            <div>
              <div className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
                Description
              </div>
              <div className="mt-1 whitespace-pre-wrap break-words text-sm text-foreground">
                {idea.description}
              </div>
            </div>
          )}
          {idea.acceptanceCriteria && (
            <div>
              <div className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
                Acceptance criteria
              </div>
              <div className="mt-1 whitespace-pre-wrap break-words text-sm text-foreground">
                {idea.acceptanceCriteria}
              </div>
            </div>
          )}
          {!idea.description && !idea.acceptanceCriteria && (
            <p className="text-sm text-muted-foreground">
              No description or acceptance criteria.
            </p>
          )}
        </div>
      )}

      {rejected ? (
        // Rejected history is read-only: the sanctioned lifecycle is
        // promote/dismiss from ACTIVE; a dismissed spawn stays a cancelled
        // terminal item here.
        <p className="mt-3 text-xs text-muted-foreground">
          Rejected — kept as history; research fires check this section before
          spawning.
        </p>
      ) : (
        <div className="mt-3 flex items-center gap-2">
          <Button
            size="sm"
            variant="default"
            onClick={onApprove}
            disabled={approving || dismissing}
          >
            Approve
          </Button>
          <Button
            size="sm"
            variant="outline"
            onClick={onDismiss}
            disabled={approving || dismissing}
            className={cn(
              isDark ? "text-rose-300 hover:text-rose-200" : "text-rose-600 hover:text-rose-500",
            )}
          >
            <X aria-hidden="true" className="h-3.5 w-3.5" />
            Dismiss
          </Button>
        </div>
      )}
    </Card>
  );
}

function absoluteTime(idea: WorkItem): string {
  if (!idea.createdAt) return "";
  const ms = Number(idea.createdAt.seconds) * 1000;
  const d = new Date(ms);
  return `${d.toLocaleDateString()} ${d.toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  })}`;
}

// Live refresh indicator (design §4/§5.5 — mirrors Work Items'): pulsing
// dot + last-refresh time, paused while the tab is hidden.
function LiveRefreshIndicator({
  lastUpdated,
  isFetching,
}: {
  lastUpdated?: number;
  isFetching: boolean;
}) {
  const now = useNow(1000);
  const d = new Date(lastUpdated ?? now);
  const time = d.toLocaleTimeString([], {
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
      <span
        className={cn(
          "h-2 w-2 animate-pulse rounded-full motion-reduce:animate-none",
          isFetching ? "bg-sky-500" : "bg-emerald-500",
        )}
      />
      <span className="font-mono text-xs font-medium tabular-nums text-foreground">
        Live {time}
      </span>
    </div>
  );
}

// One page-level `now` ticker for the live indicator; pauses when the tab
// is hidden (browsers throttle background timers anyway; this makes it
// explicit and cheap). Mirrors Work Items / Schedules' useNow.
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
