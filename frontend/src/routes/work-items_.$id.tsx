import { createRoute } from "@tanstack/react-router";
import { useState } from "react";

import {
  useGetWorkItem,
  useUpdateWorkItem,
  useDeleteWorkItem,
  useAddDependency,
  useGetDependencyGraph,
} from "@/api/workItems";
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
  const addDependency = useAddDependency(item?.projectId ?? "");
  const { data: graph } = useGetDependencyGraph(item?.projectId ?? "");

  const [editing, setEditing] = useState(false);
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [acceptanceCriteria, setAcceptanceCriteria] = useState("");
  const [priority, setPriority] = useState(0);
  const [contextWindow, setContextWindow] = useState(0);

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

  const handleStatusChange = (newStatus: number) => {
    updateWorkItem.mutate({ id, status: newStatus });
  };

  const handleDelete = () => {
    deleteWorkItem.mutate(id);
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

  return (
    <div className="space-y-6">
      <div className="flex items-start justify-between">
        <div>
          <div className="flex items-center gap-2">
            <KindBadge kind={item.kind} />
            <h1 className="text-2xl font-semibold tracking-tight">
              {item.title}
            </h1>
          </div>
          <p className="mt-1 text-xs text-muted-foreground">
            v{item.version} · {item.id}
          </p>
        </div>
        <div className="flex gap-2">
          {!editing && (
            <Button
              variant="outline"
              onClick={() => {
                setTitle(item.title);
                setDescription(item.description ?? "");
                setAcceptanceCriteria(item.acceptanceCriteria ?? "");
                setPriority(item.priority);
                setContextWindow(item.contextWindow ?? 0);
                setEditing(true);
              }}
            >
              Edit
            </Button>
          )}
          {item.status !== 8 && (
            <Button
              variant="outline"
              onClick={handleDelete}
              disabled={deleteWorkItem.isPending}
            >
              {deleteWorkItem.isPending ? "Cancelling…" : "Cancel item"}
            </Button>
          )}
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-4">
        <Card>
          <CardHeader>
            <CardDescription>Status</CardDescription>
            <CardTitle className="text-base">
              <select
                value={item.status}
                onChange={(e) =>
                  handleStatusChange(Number(e.target.value))
                }
                className="rounded-md border bg-background px-2 py-1 text-sm"
                disabled={updateWorkItem.isPending}
              >
                <option value={1}>pending</option>
                <option value={2}>ready</option>
                <option value={3}>assigned</option>
                <option value={4}>running</option>
                <option value={6}>succeeded</option>
                <option value={7}>failed</option>
                <option value={8}>cancelled</option>
              </select>
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
            <CardDescription>Worker ref</CardDescription>
            <CardTitle className="text-base font-mono text-xs">
              {item.assignedWorkerRef || "unassigned"}
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
            <p className="text-sm whitespace-pre-wrap">
              {item.description || "—"}
            </p>
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
            <p className="text-sm whitespace-pre-wrap">
              {item.acceptanceCriteria || "—"}
            </p>
          )}
        </CardContent>
      </Card>

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
