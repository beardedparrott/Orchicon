import { createRoute, Link, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { ArrowLeft } from "lucide-react";

import {
  useCreateWorkItem,
  useGetWorkItem,
  useUpdateWorkItem,
  useDeleteWorkItem,
  useHardDeleteWorkItem,
  useAddDependency,
  useGetDependencyGraph,
} from "@/api/workItems";
import { useListProjects } from "@/api/projects";
import { useListWorkflows } from "@/api/workflows";
import { EntityYamlView } from "@/components/EntityYamlView";
import { Markdown } from "@/components/markdown";
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
import { Timestamp } from "@bufbuild/protobuf";
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
  const createWorkItem = useCreateWorkItem();
  const { data: graph } = useGetDependencyGraph(item?.projectId ?? "");
  const { data: projects } = useListProjects();
  const navigate = useNavigate();

  const [editing, setEditing] = useState(false);
  const [viewMode, setViewMode] = useState<"detail" | "code">("detail");
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [acceptanceCriteria, setAcceptanceCriteria] = useState("");
  const [priority, setPriority] = useState(0);
  const [contextWindow, setContextWindow] = useState(0);
  const [status, setStatus] = useState(0);
  const [editProjectId, setEditProjectId] = useState("");
  const [editWorkflowId, setEditWorkflowId] = useState("");
  const [editScheduledStartAt, setEditScheduledStartAt] = useState("");
  const [editAutoStartWorkflow, setEditAutoStartWorkflow] = useState(true);

  const { data: workflows } = useListWorkflows({ status: 2 }); // published only

  const [depTarget, setDepTarget] = useState("");
  const [depType, setDepType] = useState(1); // BLOCKS

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
              <KindBadge kind={item.kind} />
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
                setPriority(item.priority);
                setContextWindow(item.contextWindow ?? 0);
                setEditProjectId(item.projectId);
                setEditWorkflowId(item.workflowId ?? "");
                setEditScheduledStartAt("");
                setEditAutoStartWorkflow(item.autoStartWorkflow ?? true);
                setStatus(item.status);
                setEditing(true);
              }}
            >
              Edit
            </Button>
          )}
          <Button
            variant="outline"
            onClick={handleSoftDelete}
            disabled={deleteWorkItem.isPending || item.status === 8}
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
              1: "epic",
              2: "feature",
              3: "task",
              4: "subtask",
            } as Record<number, string>)[item.kind] ?? "unknown",
            project_id: item.projectId,
            parent_id: item.parentId || undefined,
            status: ({
              1: "pending",
              2: "ready",
              3: "assigned",
              4: "running",
              6: "succeeded",
              7: "failed",
              8: "cancelled",
            } as Record<number, string>)[item.status] ?? "unknown",
            priority: item.priority,
            description: item.description || undefined,
            acceptance_criteria: item.acceptanceCriteria || undefined,
            workflow_id: item.workflowId || undefined,
            workflow_run_id: item.workflowRunId || undefined,
            assigned_worker_ref: item.assignedWorkerRef || undefined,
            context_window: item.contextWindow || undefined,
            version: item.version,
            created_at: item.createdAt
              ? new Date(Number(item.createdAt.seconds) * 1000).toISOString()
              : null,
            updated_at: item.updatedAt
              ? new Date(Number(item.updatedAt.seconds) * 1000).toISOString()
              : null,
          }}
          title="Work Item YAML"
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

      {editing && editWorkflowId && (
        <Card>
          <CardHeader>
            <CardTitle>Scheduled start</CardTitle>
            <CardDescription>
              Leave empty to start immediately on save (if auto-start is enabled).
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <div>
              <Label htmlFor="scheduledStart">Scheduled start time</Label>
              <input
                id="scheduledStart"
                type="datetime-local"
                value={editScheduledStartAt}
                onChange={(e) => setEditScheduledStartAt(e.target.value)}
                className="mt-1 h-9 w-full rounded-md border bg-background px-2 text-sm"
              />
            </div>
            <div className="flex items-center gap-2">
              <input
                type="checkbox"
                id="autoStart"
                checked={editAutoStartWorkflow}
                onChange={(e) => setEditAutoStartWorkflow(e.target.checked)}
                className="h-4 w-4 rounded border-input"
              />
              <Label htmlFor="autoStart">Auto-start workflow on save</Label>
            </div>
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
                  onChange={(e) => setStatus(Number(e.target.value))}
                  className="rounded-md border bg-background px-2 py-1 text-sm"
                >
                  <option value={1}>pending</option>
                  <option value={2}>ready</option>
                  <option value={3}>assigned</option>
                  <option value={4}>running</option>
                  <option value={6}>succeeded</option>
                  <option value={7}>failed</option>
                  <option value={8}>cancelled</option>
                </select>
              ) : (
                ({
                  1: "pending",
                  2: "ready",
                  3: "assigned",
                  4: "running",
                  6: "succeeded",
                  7: "failed",
                  8: "cancelled",
                } as Record<number, string>)[item.status] ?? "unknown"
              )}
            </CardTitle>
          </CardHeader>
        </Card>
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
              onChange={(e) => setEditProjectId(e.target.value)}
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
            onClick={() =>
              updateWorkItem.mutate(
                {
                  id,
                  title,
                  description,
                  acceptanceCriteria,
                  priority,
                  contextWindow,
                  status,
                  projectId: editProjectId,
                  workflowId: editWorkflowId || undefined,
                  scheduledStartAt: editScheduledStartAt
                    ? Timestamp.fromDate(new Date(editScheduledStartAt))
                    : undefined,
                  autoStartWorkflow: editWorkflowId ? editAutoStartWorkflow : undefined,
                },
                { onSuccess: () => setEditing(false) },
              )
            }
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

          <div className="grid gap-4 md:grid-cols-2">
            <div>
              <h4 className="text-xs font-medium uppercase text-muted-foreground">
                Depends on ({incomingDeps.length})
              </h4>
              <div className="mt-2 space-y-1">
                {incomingDeps.length === 0 && (
                  <p className="text-xs text-muted-foreground">None</p>
                )}
                {incomingDeps.map((dep) => {
                  const from = graph?.nodes?.find(
                    (n) => n.id === dep.fromId,
                  );
                  return (
                    <div
                      key={dep.id}
                      className="rounded-md border p-2 text-xs"
                    >
                      <span className="font-medium">{from?.title ?? dep.fromId}</span>
                      <span className="ml-2 text-muted-foreground">
                        → {depTypeLabel(dep.type)}
                      </span>
                    </div>
                  );
                })}
              </div>
            </div>
            <div>
              <h4 className="text-xs font-medium uppercase text-muted-foreground">
                Blocks ({outgoingDeps.length})
              </h4>
              <div className="mt-2 space-y-1">
                {outgoingDeps.length === 0 && (
                  <p className="text-xs text-muted-foreground">None</p>
                )}
                {outgoingDeps.map((dep) => {
                  const to = graph?.nodes?.find((n) => n.id === dep.toId);
                  return (
                    <div
                      key={dep.id}
                      className="rounded-md border p-2 text-xs"
                    >
                      <span className="font-medium">{to?.title ?? dep.toId}</span>
                      <span className="ml-2 text-muted-foreground">
                        ({depTypeLabel(dep.type)})
                      </span>
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

function KindBadge({ kind }: { kind: number }) {
  const labels: Record<number, string> = {
    1: "Epic",
    2: "Feature",
    3: "Task",
    4: "Subtask",
  };
  const styles: Record<number, string> = {
    1: "bg-purple-100 text-purple-800",
    2: "bg-indigo-100 text-indigo-800",
    3: "bg-blue-100 text-blue-800",
    4: "bg-cyan-100 text-cyan-800",
  };
  return (
    <span
      className={cn(
        "rounded-full px-2 py-0.5 text-xs font-medium",
        styles[kind] ?? "bg-muted text-muted-foreground",
      )}
    >
      {labels[kind] ?? "unknown"}
    </span>
  );
}

function kindLabel(kind: number): string {
  const labels: Record<number, string> = {
    1: "epic",
    2: "feature",
    3: "task",
    4: "subtask",
  };
  return labels[kind] ?? "unknown";
}

function depTypeLabel(type: number): string {
  const labels: Record<number, string> = {
    1: "blocks",
    2: "depends_on",
    3: "relates_to",
  };
  return labels[type] ?? "unknown";
}
