import { createRoute, Link, useNavigate } from "@tanstack/react-router";
import { useMemo, useState } from "react";
import { ArrowLeft } from "lucide-react";

import {
  useCreateWorkItem,
  useGetWorkItem,
  useUpdateWorkItem,
  useDeleteWorkItem,
  useHardDeleteWorkItem,
  useAddDependency,
  useRemoveDependency,
  useGetDependencyGraph,
  useListWorkItems,
} from "@/api/workItems";
import { useListProjects } from "@/api/projects";
import { useListWorkflows } from "@/api/workflows";
import { EntityYamlView } from "@/components/EntityYamlView";
import { FileBrowser } from "@/components/FileBrowser";
import { Markdown } from "@/components/markdown";
import { RuntimeImageSelect } from "@/components/RuntimeImageSelect";
import { RecurringScheduleForm, formatRecurrence } from "@/components/work-items/RecurringScheduleForm";
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
import { KindPill, PositionBadge } from "@/components/work-items/work-item-badges";
import { WorkItemParentSelect } from "@/components/work-items/work-item-parent-select";
import { computeSequencePositions } from "@/components/work-items/sequence-utils";
import { kindLabel, kindMeta, statusMeta, isTerminal, MANUALLY_UNMOVABLE_STATUSES } from "@/components/work-items/work-item-meta";
import { cn } from "@/lib/utils";
import { Timestamp } from "@bufbuild/protobuf";
import { RecurringSchedule, WorkItemKind, WorkItemStatus } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { Route as rootRoute } from "@/routes/__root";

// Work item detail (docs/10 §5, docs/02 §2.2). Shows the item's kind,
// status, hierarchy position, and allows editing all mutable fields and
// adding dependencies (edges in the work DAG — cycles are rejected
// server-side via recursive CTE).
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/work-items/$id",
  component: WorkItemDetailPage,
});

function WorkItemDetailPage() {
  const { id } = Route.useParams();
  const { data: item, isLoading, error } = useGetWorkItem(id);
  const updateWorkItem = useUpdateWorkItem(item?.projectId ?? "");
  const deleteWorkItem = useDeleteWorkItem(item?.projectId ?? "");
  const hardDeleteWorkItem = useHardDeleteWorkItem(item?.projectId ?? "");
  const addDependency = useAddDependency(item?.projectId ?? "");
  const removeDependency = useRemoveDependency(item?.projectId ?? "");
  const createWorkItem = useCreateWorkItem();
  const { data: graph } = useGetDependencyGraph(item?.projectId ?? "");
  const { data: projects } = useListProjects();
  const navigate = useNavigate();

  const [editing, setEditing] = useState(false);
  const [viewMode, setViewMode] = useState<"detail" | "code">("detail");
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [acceptanceCriteria, setAcceptanceCriteria] = useState("");
  const [acceptanceReview, setAcceptanceReview] = useState("");
  const [priority, setPriority] = useState(0);
  const [contextWindow, setContextWindow] = useState(0);
  const [status, setStatus] = useState(0);
  const [editProjectId, setEditProjectId] = useState("");
  const [editWorkflowId, setEditWorkflowId] = useState("");
  const [editRuntimeImage, setEditRuntimeImage] = useState("");
  const [editScheduledStartAt, setEditScheduledStartAt] = useState("");
  const [editAutoStartWorkflow, setEditAutoStartWorkflow] = useState(false);
  const [editParentId, setEditParentId] = useState("");
  const [editKind, setEditKind] = useState(0);
  const [editContextFiles, setEditContextFiles] = useState<string[]>([]);
  const [editRecurringSchedule, setEditRecurringSchedule] = useState<RecurringSchedule | undefined>(undefined);

  const { data: workflows } = useListWorkflows({ status: 2, templatesOnly: true }); // published templates only

  // Candidate parents while editing. Fetched from the *edit* project so
  // the dropdown switches when the user also reassigns the item (the
  // dependency graph above is keyed on the item's current project and
  // would go stale). Only enabled in edit mode.
  const { data: editProjectItems } = useListWorkItems(editing ? editProjectId : "", {
    enabled: editing,
  });

  // Whether this item has direct children — "has children" is the sequence
  // determinant. A parent with children is a sequence run (its children each
  // run their own bound workflows in chain order); its own workflow binding
  // is ignored. Derived from the edit project's items (the editor already
  // fetches them), so the schedule/start card can show for a parent even
  // without a workflow selected.
  const hasChildren = useMemo(
    () => (editProjectItems ?? []).some((i) => i.parentId === id),
    [editProjectItems, id],
  );

  const [depTarget, setDepTarget] = useState("");
  const [depType, setDepType] = useState(1); // BLOCKS

  // Quick lookup for dependency display
  const itemsById = useMemo(
    () => new Map((graph?.nodes ?? []).map((n) => [n.id, n])),
    [graph],
  );

  // Chain position within its parent's siblings (sequence-child rank),
  // derived from the already-loaded project graph — shows the item's order
  // in its sequence chain on the detail page.
  const chainPosition = useMemo(
    () => computeSequencePositions(graph?.nodes ?? []).get(id),
    [graph, id],
  );

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading…</p>;
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        Failed to load work item: {String(error)}
      </p>
    );
  }
  if (!item) {
    return null;
  }

  // Dependencies involving this item.
  const incomingDeps = graph?.edges?.filter((e) => e.toId === id) ?? [];
  const outgoingDeps = graph?.edges?.filter((e) => e.fromId === id) ?? [];


  const handleSoftDelete = () => {
    if (
      window.confirm(
        "Cancel this work item? The status will be set to cancelled and it will be hidden from the board.",
      )
    ) {
      deleteWorkItem.mutate(id);
    }
  };

  const handleHardDelete = () => {
    if (
      window.confirm(
        "Permanently delete this work item and all its dependencies? This cannot be undone.",
      )
    ) {
      hardDeleteWorkItem.mutate(id, {
        onSuccess: () => navigate({ to: "/work-items" }),
      });
    }
  };

  const handleAddDep = () => {
    if (!depTarget || depTarget === id) return;
    addDependency.mutate(
      { projectId: item.projectId, fromId: id, toId: depTarget, type: depType },
      { onSuccess: () => setDepTarget("") },
    );
  };

  const siblingItems = graph?.nodes?.filter(
    (n) => n.id !== id && n.projectId === item.projectId,
  );

  // The item's parent, resolved from the already-loaded project graph.
  const parentItem = item.parentId ? itemsById.get(item.parentId) : undefined;

  // Direct children — used by the kind-switch confirmation (children that
  // can no longer sit under the item after a switch move to its parent).
  const directChildren = graph?.nodes?.filter((n) => n.parentId === id) ?? [];

  const projectName =
    projects?.find((p) => p.id === item.projectId)?.name ?? item.projectId;

  return (
    <div className="space-y-6">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex min-w-0 items-center gap-2">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => navigate({ to: "/work-items" })}
            className="shrink-0"
          >
            <ArrowLeft className="h-4 w-4" />
            <span className="ml-1 hidden sm:inline">Back</span>
          </Button>
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <KindPill kind={item.kind} />
              <h1 className="text-lg font-semibold tracking-tight sm:text-2xl">
                {item.title}
              </h1>
            </div>
            <p className="mt-1 truncate text-xs text-muted-foreground">
              v{item.version} · {item.id}
            </p>
            <p className="truncate text-xs text-muted-foreground">
              Project:{" "}
              <Link
                to="/projects/$id"
                params={{ id: item.projectId }}
                className="font-medium hover:underline"
              >
                {projectName}
              </Link>
            </p>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {!editing && viewMode === "detail" && (
            <Button
              variant="outline"
              onClick={() => {
                setTitle(item.title);
                setDescription(item.description ?? "");
                setAcceptanceCriteria(item.acceptanceCriteria ?? "");
                setAcceptanceReview(item.acceptanceReview ?? "");
                setPriority(item.priority);
                setContextWindow(item.contextWindow ?? 0);
                setEditProjectId(item.projectId);
                setEditWorkflowId(item.workflowId ?? "");
                setEditRuntimeImage(item.runtimeImage ?? "");
                setEditParentId(item.parentId ?? "");
                setEditScheduledStartAt(
                  item.scheduledStartAt
                    ? localDatetimeString(
                        new Date(Number(item.scheduledStartAt.seconds) * 1000),
                      )
                    : "",
                );
                // "Start immediately on save" always defaults to OFF when
                // opening the editor — even for items whose stored
                // auto_start_workflow is true (legacy rows created before
                // the default flipped). Saving an edit (e.g. a kind switch)
                // must never kick off a run the user did not explicitly ask
                // for; they opt in by checking the box.
                setEditAutoStartWorkflow(false);
                setStatus(item.status);
                setEditKind(item.kind);
                setEditContextFiles(item.contextFiles ?? []);
                setEditRecurringSchedule(
                  item.recurringSchedule
                    ? new RecurringSchedule(item.recurringSchedule)
                    : undefined,
                );
                setEditing(true);
              }}
            >
              Edit
            </Button>
          )}
          <Button
            variant="outline"
            onClick={handleSoftDelete}
            disabled={deleteWorkItem.isPending || item.status === WorkItemStatus.CANCELLED}
          >
            {deleteWorkItem.isPending ? "Cancelling…" : "Cancel item"}
          </Button>
          <Button
            variant="destructive"
            onClick={handleHardDelete}
            disabled={hardDeleteWorkItem.isPending}
          >
            {hardDeleteWorkItem.isPending ? "Deleting…" : "Delete"}
          </Button>
          <Button
            variant="outline"
            onClick={() =>
              setViewMode(viewMode === "detail" ? "code" : "detail")
            }
            title={
              viewMode === "detail"
                ? "Switch to code view"
                : "Switch to detail view"
            }
          >
            {viewMode === "detail" ? "Code" : "Detail"}
          </Button>
        </div>
      </div>

      {viewMode === "code" ? (
        <EntityYamlView
          data={{
            id: item.id,
            title: item.title,
            kind: ({
              [WorkItemKind.EPIC]: "epic",
              [WorkItemKind.FEATURE]: "feature",
              [WorkItemKind.TASK]: "task",
              [WorkItemKind.SUBTASK]: "subtask",
            } as Record<number, string>)[item.kind] ?? "unknown",
            project_id: item.projectId,
            parent_id: item.parentId || undefined,
            status: ({
              [WorkItemStatus.PENDING]: "pending",
              [WorkItemStatus.SCHEDULED]: "scheduled",
              [WorkItemStatus.READY]: "ready",
              [WorkItemStatus.ASSIGNED]: "assigned",
              [WorkItemStatus.RUNNING]: "running",
              [WorkItemStatus.SUCCEEDED]: "succeeded",
              [WorkItemStatus.FAILED]: "failed",
              [WorkItemStatus.CANCELLED]: "cancelled",
              [WorkItemStatus.RECOVERING]: "recovering",
              [WorkItemStatus.RECURRING]: "recurring",
            } as Record<number, string>)[item.status] ?? "unknown",
            priority: item.priority,
            description: item.description || undefined,
            acceptance_criteria: item.acceptanceCriteria || undefined,
            acceptance_review: item.acceptanceReview || undefined,
            workflow_id: item.workflowId || undefined,
            workflow_run_id: item.workflowRunId || undefined,
            assigned_worker_ref: item.assignedWorkerRef || undefined,
            context_window: item.contextWindow || undefined,
            context_files:
              item.contextFiles && item.contextFiles.length > 0
                ? item.contextFiles
                : undefined,
            recurring_schedule: item.recurringSchedule
              ? {
                  frequency: item.recurringSchedule.frequency,
                  interval: item.recurringSchedule.interval,
                  days: item.recurringSchedule.days.length > 0 ? item.recurringSchedule.days : undefined,
                  start_date: item.recurringSchedule.startDate || undefined,
                  start_time: item.recurringSchedule.startTime || undefined,
                }
              : undefined,
            version: item.version,
            created_at: item.createdAt
              ? new Date(Number(item.createdAt.seconds) * 1000).toISOString()
              : null,
            updated_at: item.updatedAt
              ? new Date(Number(item.updatedAt.seconds) * 1000).toISOString()
              : null,
          }}
          title="Work Item YAML"
          editable
          onSave={(parsed) => {
            const statusMap: Record<string, number> = {
              pending: WorkItemStatus.PENDING,
              scheduled: WorkItemStatus.SCHEDULED,
              ready: WorkItemStatus.READY,
              assigned: WorkItemStatus.ASSIGNED,
              running: WorkItemStatus.RUNNING,
              succeeded: WorkItemStatus.SUCCEEDED,
              failed: WorkItemStatus.FAILED,
              cancelled: WorkItemStatus.CANCELLED,
              recurring: WorkItemStatus.RECURRING,
            };
            // Always include all known fields from the YAML. Optional text
            // fields default to "" so removing a line from YAML clears it.
            const str = (key: string): string => String(parsed[key] ?? "");
            const num = (key: string): number | undefined => {
              const v = parsed[key];
              return typeof v === "number" ? v : undefined;
            };
            updateWorkItem.mutate({
              id,
              title: str("title") || item.title,
              description: str("description"),
              acceptanceCriteria: str("acceptance_criteria"),
              acceptanceReview: str("acceptance_review"),
              priority: num("priority"),
              status: typeof parsed.status === "string" ? statusMap[parsed.status] : undefined,
              projectId: str("project_id"),
              workflowId: str("workflow_id"),
              workflowRunId: str("workflow_run_id"),
              // context_files is sent when the YAML carries it as an
              // array; an absent/empty array clears the selection.
              contextFiles: Array.isArray(parsed.context_files)
                ? { files: parsed.context_files.map(String) }
                : undefined,
              // parent_id is only sent when the YAML actually carries it.
              // Sending "" means "clear parent", which the server rejects
              // for non-epics — so a child whose parent line is absent
              // (orphan, or line removed) stays unchanged instead of
              // erroring on every save.
              parentId: str("parent_id") || undefined,
              // recurring_schedule is parsed from the YAML object if present;
              // an absent field leaves it unchanged.
              recurringSchedule: parsed.recurring_schedule
                ? new RecurringSchedule({
                    frequency: String((parsed.recurring_schedule as Record<string, unknown>).frequency ?? "daily"),
                    interval: Number((parsed.recurring_schedule as Record<string, unknown>).interval ?? 1),
                    days: Array.isArray((parsed.recurring_schedule as Record<string, unknown>).days)
                      ? ((parsed.recurring_schedule as Record<string, unknown>).days as unknown[]).map(String)
                      : [],
                    startDate: String((parsed.recurring_schedule as Record<string, unknown>).start_date ?? ""),
                    startTime: String((parsed.recurring_schedule as Record<string, unknown>).start_time ?? ""),
                  })
                : undefined,
            });
          }}
          saveDisabled={updateWorkItem.isPending}
          onClone={async () => {
            const title = window.prompt(
              "Clone title:",
              `Clone of ${item.title}`,
            );
            if (!title) return;
            const result = await createWorkItem.mutateAsync({
              title,
              projectId: item.projectId,
              kind: item.kind,
              parentId: item.parentId ?? undefined,
              description: item.description,
              acceptanceCriteria: item.acceptanceCriteria,
              priority: item.priority,
              contextWindow: item.contextWindow,
            });
            navigate({ to: `/work-items/${result.id}` });
          }}
          cloneDisabled={createWorkItem.isPending}
        />
      ) : (
      <>

      {editing && (editWorkflowId || hasChildren) && (
        <Card>
          <CardHeader>
            <CardTitle>Scheduled start</CardTitle>
            <CardDescription>
              {hasChildren
                ? "Run this item's children sequentially — each child runs its own bound workflow, one after another in chain order."
                : "Leave empty to start immediately. Set a time to schedule the run."}
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <div>
              <Label htmlFor="scheduledStart">Scheduled start time</Label>
              <input
                id="scheduledStart"
                type="datetime-local"
                value={editScheduledStartAt}
                onChange={(e) => { setEditScheduledStartAt(e.target.value); if (e.target.value) setEditAutoStartWorkflow(false); }}
                className="mt-1 h-9 w-full rounded-md border bg-background px-2 text-sm"
              />
            </div>
            <div className="flex items-center gap-2">
              <input
                type="checkbox"
                id="autoStart"
                checked={editAutoStartWorkflow}
                onChange={(e) => { setEditAutoStartWorkflow(e.target.checked); if (e.target.checked) setEditScheduledStartAt(""); }}
                className="h-4 w-4 rounded border-input"
              />
              <Label htmlFor="autoStart">Start immediately on save</Label>
            </div>
          </CardContent>
        </Card>
      )}

      {editing && (
        <Card>
          <CardHeader>
            <CardTitle>Recurring schedule</CardTitle>
            <CardDescription>
              Set a recurrence pattern. Setting this flips the item to
              recurring status; clearing it resets to non-recurring.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <RecurringScheduleForm
              value={editRecurringSchedule}
              onChange={setEditRecurringSchedule}
            />
          </CardContent>
        </Card>
      )}

      <div className="grid gap-4 md:grid-cols-4">
        <Card>
          <CardHeader>
            <CardDescription>Status</CardDescription>
            <CardTitle className="text-base">
              {editing ? (
                <select
                  value={status}
                  onChange={(e) => {
                    const next = Number(e.target.value);
                    setStatus(next);
                    // Switching away from recurring clears the schedule
                    if (next !== WorkItemStatus.RECURRING && editRecurringSchedule) {
                      setEditRecurringSchedule(new RecurringSchedule());
                    }
                  }}
                  className="rounded-md border bg-background px-2 py-1 text-sm"
                >
                  <option value={WorkItemStatus.PENDING}>pending</option>
                  <option value={WorkItemStatus.READY}>ready</option>
                  <option value={WorkItemStatus.ASSIGNED}>assigned</option>
                  <option value={WorkItemStatus.RUNNING}>running</option>
                  <option value={WorkItemStatus.SUCCEEDED}>succeeded</option>
                  <option value={WorkItemStatus.FAILED}>failed</option>
                  <option value={WorkItemStatus.CANCELLED}>cancelled</option>
                  <option value={WorkItemStatus.RECOVERING}>recovering</option>
                  <option value={WorkItemStatus.SCHEDULED}>scheduled</option>
                  <option value={WorkItemStatus.RECURRING}>recurring</option>
                </select>
              ) : (
                ({
                  [WorkItemStatus.PENDING]: "pending",
                  [WorkItemStatus.SCHEDULED]: "scheduled",
                  [WorkItemStatus.READY]: "ready",
                  [WorkItemStatus.ASSIGNED]: "assigned",
                  [WorkItemStatus.RUNNING]: "running",
                  [WorkItemStatus.SUCCEEDED]: "succeeded",
                  [WorkItemStatus.FAILED]: "failed",
                  [WorkItemStatus.CANCELLED]: "cancelled",
                  [WorkItemStatus.RECOVERING]: "recovering",
                  [WorkItemStatus.RECURRING]: "recurring",
                } as Record<number, string>)[item.status] ?? "unknown"
              )}
            </CardTitle>
          </CardHeader>
        </Card>
        {/* Type — switch between hierarchy kinds (ADR-WIT-1). Switching
            resolves the parent/child tree server-side; the save handler
            confirms the consequences first. Disabled while the item is
            executing (running/checkpointing/recovering). */}
        <Card>
          <CardHeader>
            <CardDescription>Type</CardDescription>
            <CardTitle className="text-base">
              {editing ? (
                <select
                  value={editKind}
                  disabled={MANUALLY_UNMOVABLE_STATUSES.has(item.status)}
                  onChange={(e) => setEditKind(Number(e.target.value))}
                  title={
                    MANUALLY_UNMOVABLE_STATUSES.has(item.status)
                      ? "Type cannot change while the item is running"
                      : "Switch to a different work item kind"
                  }
                  className="rounded-md border bg-background px-2 py-1 text-sm disabled:cursor-not-allowed disabled:opacity-50"
                >
                  <option value={WorkItemKind.EPIC}>Epic</option>
                  <option value={WorkItemKind.FEATURE}>Feature</option>
                  <option value={WorkItemKind.TASK}>Task</option>
                  <option value={WorkItemKind.SUBTASK}>Subtask</option>
                </select>
              ) : (
                <span className="inline-flex items-center gap-2">
                  <KindPill kind={item.kind} />
                </span>
              )}
            </CardTitle>
          </CardHeader>
        </Card>
        {/* Parent — shown for children (view) / all non-epics (edit). The
            dropdown candidates come from the edit project's items so they
            switch when the item is reassigned. Uses the searchable
            parent picker (ADR-WIT-5); candidates are filtered by the
            SELECTED kind (a switched-to deeper kind offers deeper
            parents, an epic switched to a non-epic forces a pick). */}
                {item.parentId || (editing && editKind !== WorkItemKind.EPIC) ? (
          <Card>
            <CardHeader>
              <CardDescription>Parent</CardDescription>
              <CardTitle className="text-base">
                {editing && editKind !== WorkItemKind.EPIC ? (
                  <WorkItemParentSelect
                    items={editProjectItems ?? []}
                    childKind={editKind}
                    value={editParentId ?? ""}
                    onChange={setEditParentId}
                    excludeId={id}
                    invalid={!editParentId}
                    error={
                      !editParentId
                        ? `A ${kindLabel(editKind)} requires a parent.`
                        : undefined
                    }
                  />
                ) : parentItem ? (
                  <Link
                    to="/work-items/$id"
                    params={{ id: item.parentId }}
                    className="inline-flex items-center gap-2 font-medium hover:underline"
                    title={parentItem.title}
                  >
                    <KindPill kind={parentItem.kind} />
                    <span className="truncate">{parentItem.title}</span>
                  </Link>
                ) : (
                  item.parentId
                )}
              </CardTitle>
            </CardHeader>
          </Card>
        ) : null}
        {chainPosition ? (
          <Card>
            <CardHeader>
              <CardDescription>Chain position</CardDescription>
              <CardTitle className="text-base">
                <PositionBadge position={chainPosition} />
              </CardTitle>
            </CardHeader>
          </Card>
        ) : null}
        {(item.nextRunAt || item.scheduledStartAt) && (
          <Card>
            <CardHeader>
              <CardDescription>Next Run</CardDescription>
              <CardTitle className="text-sm font-normal">
                {new Date(
                  Number(
                    (item.nextRunAt ?? item.scheduledStartAt).seconds,
                  ) * 1000,
                ).toLocaleString()}
              </CardTitle>
            </CardHeader>
          </Card>
        )}
        {item.recurringSchedule && (
          <Card>
            <CardHeader>
              <CardDescription>Recurrence</CardDescription>
              <CardTitle className="text-sm font-normal">
                {formatRecurrence(item.recurringSchedule)}
              </CardTitle>
            </CardHeader>
          </Card>
        )}
        <Card>
          <CardHeader>
            <CardDescription>Priority</CardDescription>
            <CardTitle className="text-base">
              {editing ? (
                <Input
                  type="number"
                  min={0}
                  max={100}
                  value={priority}
                  onChange={(e) => setPriority(Number(e.target.value))}
                  className="h-8 w-20"
                />
              ) : (
                item.priority
              )}
            </CardTitle>
          </CardHeader>
        </Card>
        <Card>
          <CardHeader>
            <CardDescription>Context window</CardDescription>
            <CardTitle className="text-base">
              {editing ? (
                <Input
                  type="number"
                  min={0}
                  max={1000000}
                  value={contextWindow}
                  onChange={(e) => setContextWindow(Number(e.target.value))}
                  className="h-8 w-24"
                />
              ) : (
                item.contextWindow || "—"
              )}
            </CardTitle>
          </CardHeader>
        </Card>
        <Card>
          <CardHeader>
            <CardDescription>Workflow template</CardDescription>
            <CardTitle className="text-base">
              {editing ? (
                <select
                  value={editWorkflowId}
                  onChange={(e) => setEditWorkflowId(e.target.value)}
                  className="w-full rounded-md border bg-background px-2 py-1 text-sm"
                >
                  <option value="">-- No workflow --</option>
                  {(workflows ?? []).map((wf) => (
                    <option key={wf.id} value={wf.id}>
                      {wf.name}
                    </option>
                  ))}
                </select>
              ) : (
                (() => {
                  const wf = workflows?.find((w) => w.id === item.workflowId);
                  return wf ? wf.name : "none (unbound)";
                })()
              )}
            </CardTitle>
            {item.workflowRunId && (
              <CardDescription className="mt-1 text-xs">
                Active run: {item.workflowRunId.slice(0, 12)}…
              </CardDescription>
            )}
            {hasChildren && (
              <CardDescription className="mt-1 text-xs text-muted-foreground">
                This item has children — it runs as a sequence, so its own
                workflow is ignored. Each child runs its own workflow in chain
                order.
              </CardDescription>
            )}
          </CardHeader>
        </Card>
        <Card>
          <CardHeader>
            <CardDescription>Runtime image</CardDescription>
            <CardTitle className="text-base">
              {editing ? (
                <RuntimeImageSelect
                  value={editRuntimeImage}
                  onChange={setEditRuntimeImage}
                />
              ) : (
                item.runtimeImage || "default (base image)"
              )}
            </CardTitle>
          </CardHeader>
        </Card>
      </div>

      {/* Description */}
      <Card>
        <CardHeader>
          <CardTitle>Description</CardTitle>
        </CardHeader>
        <CardContent>
          {editing ? (
            <Textarea
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              className="min-h-[80px]"
            />
          ) : (
            <Markdown>{item.description}</Markdown>
          )}
        </CardContent>
      </Card>

      {/* Acceptance criteria */}
      <Card>
        <CardHeader>
          <CardTitle>Acceptance criteria</CardTitle>
        </CardHeader>
        <CardContent>
          {editing ? (
            <Textarea
              value={acceptanceCriteria}
              onChange={(e) => setAcceptanceCriteria(e.target.value)}
              className="min-h-[80px]"
            />
          ) : (
            <Markdown>{item.acceptanceCriteria}</Markdown>
          )}
        </CardContent>
      </Card>

      {/* Acceptance review — auto-populated by the WorkflowReconciler
          when a bound workflow run completes; editable by a reviewer */}
      <Card>
        <CardHeader>
          <CardTitle>Acceptance Review</CardTitle>
          <CardDescription>
            Summary of the final work done, generated automatically when a
            bound workflow run completes.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {editing ? (
            <Textarea
              value={acceptanceReview}
              onChange={(e) => setAcceptanceReview(e.target.value)}
              className="min-h-[80px]"
              placeholder="Auto-populated on workflow completion — extend or correct as needed."
            />
          ) : item.acceptanceReview ? (
            <Markdown>{item.acceptanceReview}</Markdown>
          ) : (
            <p className="text-sm text-muted-foreground">
              No acceptance review yet — populated automatically when a
              bound workflow run completes.
            </p>
          )}
        </CardContent>
      </Card>

      {/* Context files — files AND directories, exactly like projects */}
      {(() => {
        const project = projects?.find((p) => p.id === (editing ? editProjectId : item.projectId));
        if (!project?.projectDir) {
          return (
            <Card>
              <CardHeader>
                <CardTitle>Work Item Context Files</CardTitle>
                <CardDescription>
                  The project for this item has no project directory set —
                  context files cannot be added until it does.
                </CardDescription>
              </CardHeader>
            </Card>
          );
        }
        return (
          <FileBrowser
            projectId={project.id}
            projectDir={project.projectDir}
            initialSelectedFiles={
              editing ? editContextFiles : item.contextFiles ?? []
            }
            readOnly={!editing}
            onChange={setEditContextFiles}
            title="Work Item Context Files"
            description="Expand folders and check files or directories to include as context for the worker, exactly like project context files."
            emptyHint="Context files selected for this work item. Click Edit to modify."
          />
        );
      })()}

      {/* Project (editable) */}
      {editing && (
        <Card>
          <CardHeader>
            <CardTitle>Project</CardTitle>
            <CardDescription>
              Reassign to a different project. The target must be active.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <select
              className="flex h-9 w-full rounded-md border border-input bg-background px-3 py-1 text-sm"
              value={editProjectId}
              onChange={(e) => {
                setEditProjectId(e.target.value);
                // The parent dropdown is repopulated from the target
                // project; reset the selection so a stale parent from the
                // old project is never kept (server re-validates anyway).
                setEditParentId("");
              }}
            >
              {(projects ?? []).map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name}
                </option>
              ))}
            </select>
          </CardContent>
        </Card>
      )}

      {/* Edit save/cancel */}
      {editing && (
        <div className="flex gap-2">
          <Button
            onClick={() => {
              const kindChanging = editKind !== item.kind;
              // Epic → non-epic without a parent: force the pick before
              // enabling Save (ADR-WIT-1 — the server requires it too).
              if (kindChanging && editKind !== 1 && !editParentId) {
                window.alert(
                  `A ${kindLabel(editKind)} requires a parent. Choose one in the Parent card first.`,
                );
                return;
              }
              if (kindChanging) {
                // Describe the automatic resolution before confirming
                // (ADR-WIT-2): children that can no longer sit under the
                // item move to its parent; non-schedulable kinds clear
                // worker/schedule/status.
                const moving = directChildren.filter(
                  (c) => depthForKind(c.kind) <= depthForKind(editKind),
                );
                const lines = [
                  `Switch type from ${kindLabel(item.kind)} to ${kindLabel(editKind)}?`,
                ];
                if (moving.length > 0) {
                  lines.push(
                    `\n${moving.length} child item${moving.length === 1 ? "" : "s"} will move under the parent:`,
                    moving.map((c) => `  • ${c.title}`).join("\n"),
                  );
                }
                if (editKind === WorkItemKind.EPIC || editKind === WorkItemKind.FEATURE) {
                  lines.push(
                    "\nWorker assignment, scheduled start, recurring schedule, and ready/assigned/scheduled status will be cleared.",
                  );
                }
                if (!window.confirm(lines.join("\n"))) return;
              }
              updateWorkItem.mutate(
                {
                  id,
                  title,
                  description,
                  acceptanceCriteria,
                  acceptanceReview,
                  priority,
                  contextWindow,
                  status,
                  projectId: editProjectId,
                  workflowId: editWorkflowId,
                  runtimeImage: editRuntimeImage || undefined,
                  scheduledStartAt: editScheduledStartAt
                    ? Timestamp.fromDate(new Date(editScheduledStartAt))
                    : undefined,
                  autoStartWorkflow: editAutoStartWorkflow,
                  parentId: editParentId || undefined,
                  kind: kindChanging ? editKind : undefined,
                  contextFiles: { files: editContextFiles },
                  recurringSchedule:
                    kindChanging && (editKind === WorkItemKind.EPIC || editKind === WorkItemKind.FEATURE)
                      ? new RecurringSchedule()
                      : editRecurringSchedule,
                },
                { onSuccess: () => setEditing(false) },
              );
            }}
            disabled={updateWorkItem.isPending || !title.trim()}
          >
            {updateWorkItem.isPending ? "Saving…" : "Save changes"}
          </Button>
          <Button variant="outline" onClick={() => setEditing(false)}>
            Cancel
          </Button>
        </div>
      )}

      {/* Dependencies (DAG edges — docs/02 §2.2, docs/09 §3.2) */}
      <Card>
        <CardHeader>
          <CardTitle>Dependencies</CardTitle>
          <CardDescription>
            Edges in the work DAG. Cycles are rejected at admission (recursive
            CTE — docs/09 §11).
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {/* Add dependency form */}
          <div className="flex items-end gap-2">
            <div className="flex-1 space-y-1">
              <Label htmlFor="depTarget">Add dependency to</Label>
              <select
                id="depTarget"
                className="flex h-9 w-full rounded-md border border-input bg-background px-3 py-1 text-sm"
                value={depTarget}
                onChange={(e) => setDepTarget(e.target.value)}
              >
                <option value="">— Select work item —</option>
                {(siblingItems ?? []).map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.title} ({kindLabel(s.kind)})
                  </option>
                ))}
              </select>
            </div>
            <div className="space-y-1">
              <Label htmlFor="depType">Type</Label>
              <select
                id="depType"
                className="flex h-9 rounded-md border border-input bg-background px-3 py-1 text-sm"
                value={depType}
                onChange={(e) => setDepType(Number(e.target.value))}
              >
                <option value={1}>blocks</option>
                <option value={2}>depends_on</option>
                <option value={3}>relates_to</option>
              </select>
            </div>
            <Button
              onClick={handleAddDep}
              disabled={!depTarget || addDependency.isPending}
            >
              Add
            </Button>
          </div>

          {addDependency.error && (
            <p className="text-sm text-destructive">
              {String(addDependency.error.message ?? addDependency.error)}
            </p>
          )}

          {/* Dependency lists */}
          <div className="grid gap-4 md:grid-cols-2">
            {/* Incoming (what this item depends on) */}
            <div>
              <h4 className="text-xs font-medium uppercase text-muted-foreground">
                Depends on ({incomingDeps.length})
              </h4>
              <div className="mt-2 space-y-1.5">
                {incomingDeps.length === 0 && (
                  <p className="text-xs text-muted-foreground">None</p>
                )}
                {incomingDeps.map((dep) => {
                  const from = graph?.nodes?.find((n) => n.id === dep.fromId);
                  const fromItem = itemsById.get(dep.fromId);
                  return (
                    <div
                      key={dep.id}
                      className={cn(
                        "group flex items-center gap-2 rounded-md border p-2 text-xs transition-colors hover:bg-accent/50",
                        fromItem && isTerminal(fromItem.status) && "opacity-60",
                      )}
                    >
                      {fromItem ? (
                        <>
                          <span className={cn("inline-flex h-4 w-4 shrink-0 items-center justify-center rounded text-[10px] font-bold", kindMeta(fromItem.kind).badge)}>
                            {kindMeta(fromItem.kind).shortLabel}
                          </span>
                          <Link
                            to="/work-items/$id"
                            params={{ id: dep.fromId }}
                            className="min-w-0 flex-1 truncate font-medium hover:underline"
                          >
                            {from?.title ?? dep.fromId}
                          </Link>
                          <span className={cn("inline-flex shrink-0 items-center gap-1 rounded-full px-1.5 py-0.5 text-[10px] font-medium", statusMeta(fromItem.status).pill)}>
                            <span className={cn("h-1 w-1 rounded-full", statusMeta(fromItem.status).dot)} />
                            {statusMeta(fromItem.status).label}
                          </span>
                        </>
                      ) : (
                        <span className="font-medium">{from?.title ?? dep.fromId}</span>
                      )}
                      <span className="shrink-0 text-muted-foreground">
                        ({depTypeLabel(dep.type)})
                      </span>
                      <button
                        type="button"
                        onClick={() => {
                          if (window.confirm(`Remove dependency from "${from?.title ?? dep.fromId}"?`)) {
                            removeDependency.mutate(dep.id);
                          }
                        }}
                        disabled={removeDependency.isPending}
                        className="ml-auto shrink-0 rounded p-0.5 text-muted-foreground opacity-0 transition-opacity hover:bg-destructive/10 hover:text-destructive group-hover:opacity-100"
                        aria-label={`Remove dependency from ${from?.title ?? dep.fromId}`}
                      >
                        ×
                      </button>
                    </div>
                  );
                })}
              </div>
            </div>

            {/* Outgoing (what depends on this item) */}
            <div>
              <h4 className="text-xs font-medium uppercase text-muted-foreground">
                Blocks ({outgoingDeps.length})
              </h4>
              <div className="mt-2 space-y-1.5">
                {outgoingDeps.length === 0 && (
                  <p className="text-xs text-muted-foreground">None</p>
                )}
                {outgoingDeps.map((dep) => {
                  const to = graph?.nodes?.find((n) => n.id === dep.toId);
                  const toItem = itemsById.get(dep.toId);
                  return (
                    <div
                      key={dep.id}
                      className={cn(
                        "group flex items-center gap-2 rounded-md border p-2 text-xs transition-colors hover:bg-accent/50",
                        toItem && isTerminal(toItem.status) && "opacity-60",
                      )}
                    >
                      {toItem ? (
                        <>
                          <span className={cn("inline-flex h-4 w-4 shrink-0 items-center justify-center rounded text-[10px] font-bold", kindMeta(toItem.kind).badge)}>
                            {kindMeta(toItem.kind).shortLabel}
                          </span>
                          <Link
                            to="/work-items/$id"
                            params={{ id: dep.toId }}
                            className="min-w-0 flex-1 truncate font-medium hover:underline"
                          >
                            {to?.title ?? dep.toId}
                          </Link>
                          <span className={cn("inline-flex shrink-0 items-center gap-1 rounded-full px-1.5 py-0.5 text-[10px] font-medium", statusMeta(toItem.status).pill)}>
                            <span className={cn("h-1 w-1 rounded-full", statusMeta(toItem.status).dot)} />
                            {statusMeta(toItem.status).label}
                          </span>
                        </>
                      ) : (
                        <span className="font-medium">{to?.title ?? dep.toId}</span>
                      )}
                      <span className="shrink-0 text-muted-foreground">
                        ({depTypeLabel(dep.type)})
                      </span>
                      <button
                        type="button"
                        onClick={() => {
                          if (window.confirm(`Remove dependency to "${to?.title ?? dep.toId}"?`)) {
                            removeDependency.mutate(dep.id);
                          }
                        }}
                        disabled={removeDependency.isPending}
                        className="ml-auto shrink-0 rounded p-0.5 text-muted-foreground opacity-0 transition-opacity hover:bg-destructive/10 hover:text-destructive group-hover:opacity-100"
                        aria-label={`Remove dependency to ${to?.title ?? dep.toId}`}
                      >
                        ×
                      </button>
                    </div>
                  );
                })}
              </div>
            </div>
          </div>
        </CardContent>
      </Card>
        </>
      )}
    </div>
  );
}

function depTypeLabel(type: number): string {
  const labels: Record<number, string> = {
    1: "blocks",
    2: "depends_on",
    3: "relates_to",
  };
  return labels[type] ?? "unknown";
}

// Hierarchy depth of a work item kind (epic=1 … subtask=4, matching the
// proto enum values). Unknown/recovery kinds are not valid parents via
// the API, so they map to 0 (never shallower than a real kind).
function depthForKind(kind: number): number {
  return kind >= 1 && kind <= 4 ? kind : 0;
}

function localDatetimeString(d: Date): string {
  const pad = (n: number) => n.toString().padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}
