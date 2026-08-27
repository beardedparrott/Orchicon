// Recurring Item detail (feature 4.3) — schedule-first. Shows the cadence
// editor (edit cadence + enable/pause), the per-fire run-history ledger
// (status, timestamps, run id + outputs), and a provenance/settings summary
// (workflow binding, runtime image, context files, spawned_by provenance).
// Kind/type/parent/priority cards and the dependency DAG are gone — this is
// an automation, not a work item.

import { createRoute, Link, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import {
  ArrowLeft,
  CalendarClock,
  ExternalLink,
  Pause,
  Play,
  Repeat,
  Workflow,
} from "lucide-react";

import {
  useGetWorkItem,
  useGetWorkItemRunHistory,
  useUpdateWorkItem,
} from "@/api/workItems";
import { useListWorkflows } from "@/api/workflows";
import { useListProjects } from "@/api/projects";
import { FileBrowser } from "@/components/FileBrowser";
import { RuntimeImageSelect } from "@/components/RuntimeImageSelect";
import { RecurringScheduleForm, formatRecurrence } from "@/components/work-items/RecurringScheduleForm";
import { RecurringBadge, RunStatusBadge } from "@/components/work-items/work-item-badges";
import { useToast } from "@/components/ui/toast";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";
import {
  RecurringSchedule,
  type WorkItem,
} from "@/api/gen/orchicon/api/v1/work_item_pb";
import type { RecurringRunHistoryEntry } from "@/api/gen/orchicon/api/v1/work_item_service_pb";
import { Route as rootRoute } from "@/routes/__root";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/recurring-items/$id",
  component: WorkItemDetailPage,
});

function WorkItemDetailPage() {
  const { id } = Route.useParams();
  const { data: item, isLoading, error } = useGetWorkItem(id);

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading…</p>;
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        Failed to load recurring item: {String(error)}
      </p>
    );
  }
  if (!item) return null;
  return <RecurringItemDetail item={item} />;
}

function RecurringItemDetail({ item }: { item: WorkItem }) {
  const navigate = useNavigate();
  const toast = useToast();
  const updateWorkItem = useUpdateWorkItem(item.projectId);
  const { data: history = [] } = useGetWorkItemRunHistory(item.id);
  const { data: workflows } = useListWorkflows({ status: 2, templatesOnly: true });
  const { data: projects } = useListProjects();
  const projectDir = projects?.find((p) => p.id === item.projectId)?.projectDir ?? "";

  const [editing, setEditing] = useState(false);
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [acceptanceCriteria, setAcceptanceCriteria] = useState("");
  const [workflowId, setWorkflowId] = useState("");
  const [runtimeImage, setRuntimeImage] = useState("");
  const [contextFiles, setContextFiles] = useState<string[]>([]);
  const [recurringSchedule, setRecurringSchedule] = useState<RecurringSchedule | undefined>(undefined);

  const enabled = item.recurringEnabled !== false;
  const nextRunTs = item.nextRunAt ?? item.scheduledStartAt;
  const nextMs = nextRunTs ? Number(nextRunTs.seconds) * 1000 : 0;

  const beginEdit = () => {
    setTitle(item.title);
    setDescription(item.description ?? "");
    setAcceptanceCriteria(item.acceptanceCriteria ?? "");
    setWorkflowId(item.workflowId ?? "");
    setRuntimeImage(item.runtimeImage ?? "");
    setContextFiles(item.contextFiles ?? []);
    setRecurringSchedule(
      item.recurringSchedule ? new RecurringSchedule(item.recurringSchedule) : undefined,
    );
    setEditing(true);
  };

  const handleSave = async () => {
    updateWorkItem.mutate(
      {
        id: item.id,
        title,
        description: description || undefined,
        acceptanceCriteria: acceptanceCriteria || undefined,
        workflowId: workflowId || undefined,
        runtimeImage: runtimeImage || undefined,
        // UpdateWorkItemRequest.context_files is a ContextFiles wrapper (an
        // empty list clears the selection), not the repeated array CREATE uses.
        contextFiles: { files: contextFiles },
        recurringSchedule: recurringSchedule ? new RecurringSchedule(recurringSchedule) : undefined,
      },
      {
        onSuccess: () => {
          toast.success("Recurring item updated.");
          setEditing(false);
        },
        onError: (e) => toast.error(`Failed to update: ${String(e)}`),
      },
    );
  };

  const handleToggleEnabled = () => {
    updateWorkItem.mutate(
      { id: item.id, recurringEnabled: !enabled },
      {
        onSuccess: () =>
          toast.success(enabled ? "Paused" : "Resumed"),
        onError: (e) => toast.error(`Failed to update: ${String(e)}`),
      },
    );
  };

  const workflowName = workflows?.find((w) => w.id === item.workflowId)?.name;

  const ideaOutputs = item.recurringSchedule?.outputsMode === "idea";

  return (
    <div className="space-y-6">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex min-w-0 items-center gap-2">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => navigate({ to: "/recurring-items" })}
            className="shrink-0"
          >
            <ArrowLeft aria-hidden="true" className="h-4 w-4" />
            <span className="ml-1 hidden sm:inline">Back</span>
          </Button>
          <div className="min-w-0">
            <div className="flex min-w-0 items-center gap-2">
              <Repeat aria-hidden="true" className="h-5 w-5 shrink-0 text-fuchsia-500" />
              <h1 className="min-w-0 break-words [overflow-wrap:anywhere] text-lg font-semibold tracking-tight sm:text-2xl">
                {item.title}
              </h1>
              <RecurringBadge />
            </div>
            <p className="mt-1 truncate text-xs text-muted-foreground">
              v{item.version} · {item.id}
            </p>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="outline" onClick={handleToggleEnabled} disabled={updateWorkItem.isPending}>
            {enabled ? (
              <>
                <Pause aria-hidden="true" className="h-4 w-4" />
                Pause
              </>
            ) : (
              <>
                <Play aria-hidden="true" className="h-4 w-4" />
                Resume
              </>
            )}
          </Button>
          {!editing && (
            <Button variant="outline" onClick={beginEdit}>
              Edit
            </Button>
          )}
          <Button
            variant="outline"
            onClick={() => navigate({ to: "/recurring-items" })}
          >
            Close
          </Button>
        </div>
      </div>

      {editing ? (
        <Card>
          <CardHeader>
            <CardTitle>Edit automation</CardTitle>
            <CardDescription>
              Change the title, workflow binding, runtime image, context files,
              cadence, or output mode. Cadence changes re-arm the next run.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <form
              onSubmit={(e) => {
                e.preventDefault();
                void handleSave();
              }}
              className="space-y-4"
            >
              <div className="space-y-2">
                <Label htmlFor="title">Title</Label>
                <Input id="title" value={title} onChange={(e) => setTitle(e.target.value)} />
              </div>

              <div className="space-y-2">
                <Label htmlFor="workflow">Workflow</Label>
                <select
                  id="workflow"
                  className="flex h-11 min-h-[44px] w-full rounded-xl glass-input px-3 py-1 text-sm sm:h-9"
                  value={workflowId}
                  onChange={(e) => setWorkflowId(e.target.value)}
                >
                  <option value="">-- No workflow --</option>
                  {(workflows ?? []).map((wf) => (
                    <option key={wf.id} value={wf.id}>
                      {wf.name}
                    </option>
                  ))}
                </select>
                {!workflowId && (
                  <p className="text-xs text-destructive">
                    A workflow binding is required to schedule a recurring item.
                  </p>
                )}
              </div>

              <div className="space-y-2">
                <Label htmlFor="runtimeImage">Runtime image</Label>
                <RuntimeImageSelect value={runtimeImage} onChange={setRuntimeImage} />
              </div>

              {projectDir ? (
                <FileBrowser
                  projectId={item.projectId}
                  projectDir={projectDir}
                  initialSelectedFiles={contextFiles}
                  onChange={setContextFiles}
                  title="Automation Context Files"
                  description="Expand folders and check files or directories to include as context for the worker."
                />
              ) : (
                <p className="text-xs text-muted-foreground">
                  This project has no project directory — context files are
                  not available for this automation.
                </p>
              )}

              {recurringSchedule ? (
                <div className="rounded-2xl glass-panel p-3 space-y-3">
                  <RecurringScheduleForm
                    value={recurringSchedule}
                    onChange={setRecurringSchedule}
                  />
                  <div className="flex items-start gap-2">
                    <input
                      type="checkbox"
                      id="ideaOutputs"
                      checked={ideaOutputsFromSchedule(recurringSchedule)}
                      onChange={(e) => {
                        const mode = e.target.checked ? "idea" : "standard";
                        setRecurringSchedule(
                          new RecurringSchedule({ ...recurringSchedule, outputsMode: mode }),
                        );
                      }}
                      className="mt-1 h-4 w-4 rounded border-input"
                    />
                    <div>
                      <Label htmlFor="ideaOutputs">Outputs: ideas</Label>
                      <p className="text-xs text-muted-foreground">
                        Each fire&apos;s spawned work items land in IDEA state —
                        hidden from normal Work Items until promoted.
                      </p>
                    </div>
                  </div>
                </div>
              ) : (
                <p className="text-xs text-muted-foreground">
                  No recurring schedule — <button type="button" className="underline" onClick={() => setRecurringSchedule(new RecurringSchedule())}>add one</button>.
                </p>
              )}

              <div className="space-y-2">
                <Label htmlFor="description">Description (optional)</Label>
                <Textarea id="description" rows={4} value={description} onChange={(e) => setDescription(e.target.value)} />
              </div>
              <div className="space-y-2">
                <Label htmlFor="acceptanceCriteria">Acceptance criteria (optional)</Label>
                <Textarea id="acceptanceCriteria" rows={3} value={acceptanceCriteria} onChange={(e) => setAcceptanceCriteria(e.target.value)} />
              </div>

              {updateWorkItem.error && (
                <p className="text-sm text-destructive">
                  Failed to update: {String(updateWorkItem.error)}
                </p>
              )}

              <div className="flex justify-end gap-2">
                <Button type="button" variant="outline" onClick={() => setEditing(false)}>
                  Cancel
                </Button>
                <Button type="submit" disabled={updateWorkItem.isPending || !workflowId}>
                  {updateWorkItem.isPending ? "Saving…" : "Save"}
                </Button>
              </div>
            </form>
          </CardContent>
        </Card>
      ) : (
        <div className="grid gap-4 lg:grid-cols-3">
          {/* Schedule / cadence editor (read-only summary + live toggle). */}
          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2 text-base">
                <CalendarClock aria-hidden="true" className="h-4 w-4" />
                Schedule
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              <div className="break-words text-sm font-medium [overflow-wrap:anywhere]">
                {formatRecurrence(item.recurringSchedule)}
              </div>
              <RecurringScheduleForm value={item.recurringSchedule} onChange={() => {}} readOnly />
              <div className="border-t border-border/60 pt-2 text-sm">
                <span className="text-xs text-muted-foreground">Next run</span>
                <span className="ml-2 font-medium">
                  {enabled && nextMs ? new Date(nextMs).toLocaleString() : enabled ? "Not scheduled" : "Paused"}
                </span>
              </div>
              <div className="flex items-center gap-2">
                <span
                  className={cn(
                    "rounded-full px-2 py-0.5 text-xs font-medium",
                    enabled
                      ? "bg-emerald-100 text-emerald-800"
                      : "bg-muted text-muted-foreground",
                  )}
                >
                  {enabled ? "Active" : "Paused"}
                </span>
                {ideaOutputs && (
                  <span className="rounded-full px-2 py-0.5 text-xs font-medium bg-fuchsia-500/15 text-fuchsia-800">
                    Outputs: ideas
                  </span>
                )}
              </div>
            </CardContent>
          </Card>

          {/* Provenance / settings summary. */}
          <Card>
            <CardHeader>
              <CardTitle className="text-base">Settings</CardTitle>
            </CardHeader>
            <CardContent className="space-y-3 text-sm">
              <div>
                <div className="text-xs text-muted-foreground">Workflow</div>
                {item.workflowId ? (
                  <Link
                    to="/workflows/$id"
                    params={{ id: item.workflowId }}
                    className="inline-flex items-center gap-1 font-medium hover:underline"
                  >
                    <Workflow aria-hidden="true" className="h-3.5 w-3.5" />
                    {workflowName ?? item.workflowId}
                  </Link>
                ) : (
                  <span className="italic text-muted-foreground/70">none (unbound)</span>
                )}
              </div>
              <div>
                <div className="text-xs text-muted-foreground">Runtime image</div>
                <div className="break-words [overflow-wrap:anywhere]">
                  {item.runtimeImage || "default (base)"}
                </div>
              </div>
              <div>
                <div className="text-xs text-muted-foreground">Context files</div>
                {item.contextFiles && item.contextFiles.length > 0 ? (
                  <ul className="list-inside list-disc space-y-0.5">
                    {item.contextFiles.map((f) => (
                      <li key={f} className="break-all text-xs">{f}</li>
                    ))}
                  </ul>
                ) : (
                  <span className="italic text-muted-foreground/70">none</span>
                )}
              </div>
              {item.spawnedBy && (
                <div>
                  <div className="text-xs text-muted-foreground">Spawned by</div>
                  <div className="break-all text-xs">
                    <Link
                      to="/recurring-items/$id"
                      params={{ id: item.spawnedBy }}
                      className="font-medium hover:underline"
                    >
                      {item.spawnedBy}
                    </Link>
                    {item.spawnedByRunId ? ` · run ${shortId(item.spawnedByRunId)}` : ""}
                  </div>
                </div>
              )}
            </CardContent>
          </Card>

          {/* Run history ledger (per-fire status, timestamps, run + outputs). */}
          <Card className="lg:col-span-1">
            <CardHeader>
              <CardTitle className="text-base">Run history</CardTitle>
            </CardHeader>
            <CardContent>
              {history.length === 0 ? (
                <p className="text-sm italic text-muted-foreground">-</p>
              ) : (
                <ol className="space-y-3">
                  {history.map((entry) => (
                    <RunHistoryRow key={entry.id} entry={entry} workflowId={item.workflowId ?? ""} />
                  ))}
                </ol>
              )}
            </CardContent>
          </Card>
        </div>
      )}
    </div>
  );
}

function RunHistoryRow({
  entry,
  workflowId,
}: {
  entry: RecurringRunHistoryEntry;
  workflowId: string;
}) {
  const fired = entry.status === "fired";
  return (
    <li className="rounded-xl border border-border/60 p-3">
      <div className="flex items-center justify-between gap-2">
        <span className="text-xs text-muted-foreground">
          {formatTimestamp(entry.fireAt)}
        </span>
        <span
          className={cn(
            "rounded-full px-2 py-0.5 text-[10px] font-medium",
            fired
              ? "bg-emerald-100 text-emerald-800"
              : "bg-rose-100 text-rose-800",
          )}
        >
          {entry.status}
        </span>
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-2 text-xs">
        {entry.workflowRunId && (
          <RunStatusBadge status={runStatusToNumber(entry.runStatus)} />
        )}
        {entry.workflowRunId && workflowId ? (
          <Link
            to="/workflows/$id/runs/$runId"
            params={{ id: workflowId, runId: entry.workflowRunId }}
            className="inline-flex items-center gap-1 font-medium hover:underline"
          >
            run {shortId(entry.workflowRunId)}
            <ExternalLink aria-hidden="true" className="h-3 w-3" />
          </Link>
        ) : entry.workflowRunId ? (
          <span className="font-mono">{shortId(entry.workflowRunId)}</span>
        ) : null}
        {entry.error && (
          <span className="w-full break-words text-rose-500 [overflow-wrap:anywhere]">
            {entry.error}
          </span>
        )}
      </div>
      {entry.executions.length > 0 && (
        <div className="mt-2 space-y-1 border-t border-border/40 pt-2">
          <div className="text-[10px] uppercase tracking-wide text-muted-foreground">
            Outputs
          </div>
          {entry.executions.map((exec) => (
            <Link
              key={exec.id}
              to="/executions/$id"
              params={{ id: exec.id }}
              className="flex items-center justify-between gap-2 rounded px-1 py-0.5 text-xs hover:bg-accent"
            >
              <span className="truncate font-mono">{shortId(exec.id)}</span>
              <span className="shrink-0 text-muted-foreground">
                {exec.status}
                {exec.output ? " · has output" : ""}
              </span>
            </Link>
          ))}
        </div>
      )}
    </li>
  );
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function ideaOutputsFromSchedule(s: RecurringSchedule): boolean {
  return s.outputsMode === "idea";
}

function shortId(id: string): string {
  return id.length > 8 ? `${id.slice(0, 8)}…` : id;
}

function formatTimestamp(ts?: { seconds: bigint | number; nanos?: number }): string {
  if (!ts) return "";
  const ms = Number(ts.seconds) * 1000;
  return new Date(ms).toLocaleString([], {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

/** Map a stored workflow-run status string to its WorkflowRunStatus number
 *  for RunStatusBadge (pending/running/completed/failed/aborted/paused). */
function runStatusToNumber(status: string): number {
  switch (status) {
    case "pending":
      return 1;
    case "running":
      return 2;
    case "completed":
      return 3;
    case "failed":
      return 4;
    case "aborted":
      return 5;
    case "paused":
      return 6;
    default:
      return 0;
  }
}
