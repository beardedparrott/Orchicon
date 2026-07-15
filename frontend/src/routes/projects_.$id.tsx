import { createRoute, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { useForm } from "react-hook-form";
import { useQueryClient } from "@tanstack/react-query";

import {
  useArchiveProject,
  useDeleteProject,
  useGetProject,
  useUpdateProject,
  projectKeys,
} from "@/api/projects";
import { useStreamProjectEvents } from "@/api/projectEvents";
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
import { Route as rootRoute } from "@/routes/__root";

// Project detail with inline editing. UseUpdateProject calls the existing
// UpdateProject RPC (partial update — only non-nil fields are written).
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/projects/$id",
  component: ProjectDetailPage,
});

function ProjectDetailPage() {
  const { id } = Route.useParams();
  const { data: project, isLoading, error } = useGetProject(id);
  const archiveProject = useArchiveProject();
  const deleteMutation = useDeleteProject();
  const updateProject = useUpdateProject();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [editing, setEditing] = useState(false);

  const { register, handleSubmit, reset } = useForm({
    defaultValues: { name: "", slug: "" },
    values: project ? { name: project.name, slug: project.slug } : undefined,
  });

  // Live event feed.
  const { events, status } = useStreamProjectEvents({
    projectId: id,
    onEvent: () => {
      qc.invalidateQueries({ queryKey: projectKeys.detail(id) });
    },
  });

  const handleArchive = async () => {
    await archiveProject.mutateAsync(id);
    navigate({ to: "/projects" });
  };

  const handleDelete = () => {
    if (
      window.confirm(
        "Permanently delete this project and all its workflows, work items, and data? This cannot be undone.",
      )
    ) {
      deleteMutation.mutate(id, {
        onSuccess: () => navigate({ to: "/projects" }),
      });
    }
  };

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading…</p>;
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        Failed to load project: {String(error)}
      </p>
    );
  }
  if (!project) {
    return null;
  }

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-start justify-between">
        <div>
          {editing ? (
            <form
              onSubmit={handleSubmit((data) => {
                updateProject.mutate(
                  { id, name: data.name, slug: data.slug },
                  { onSuccess: () => setEditing(false) },
                );
              })}
              className="space-y-3"
            >
              <div className="space-y-2">
                <Label htmlFor="name">Name</Label>
                <Input id="name" {...register("name", { required: true })} />
              </div>
              <div className="space-y-2">
                <Label htmlFor="slug">Slug</Label>
                <Input id="slug" {...register("slug")} />
              </div>
              <div className="flex gap-2">
                <Button type="submit" disabled={updateProject.isPending}>
                  {updateProject.isPending ? "Saving…" : "Save"}
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => {
                    reset();
                    setEditing(false);
                  }}
                >
                  Cancel
                </Button>
              </div>
            </form>
          ) : (
            <>
              <h1 className="text-2xl font-semibold tracking-tight">
                {project.name}
              </h1>
              <p className="font-mono text-xs text-muted-foreground">
                {project.slug}
              </p>
            </>
          )}
        </div>
        <div className="flex gap-2">
          {!editing && (
            <Button variant="outline" onClick={() => setEditing(true)}>
              Edit
            </Button>
          )}
          {project.status !== 4 && (
            <Button
              variant="outline"
              onClick={handleArchive}
              disabled={archiveProject.isPending}
            >
              {archiveProject.isPending ? "Archiving…" : "Archive"}
            </Button>
          )}
          <Button
            variant="destructive"
            onClick={handleDelete}
            disabled={deleteMutation.isPending}
          >
            {deleteMutation.isPending ? "Deleting…" : "Delete"}
          </Button>
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-3">
        <Card>
          <CardHeader>
            <CardDescription>Status</CardDescription>
            <CardTitle className="text-base capitalize">
              {statusLabel(project.status)}
            </CardTitle>
          </CardHeader>
        </Card>
        <Card>
          <CardHeader>
            <CardDescription>Version</CardDescription>
            <CardTitle className="text-base">{project.version}</CardTitle>
          </CardHeader>
        </Card>
        <Card>
          <CardHeader>
            <CardDescription>Project ID</CardDescription>
            <CardTitle className="font-mono text-xs break-all">
              {project.id}
            </CardTitle>
          </CardHeader>
        </Card>
      </div>

      {project.goals && project.goals !== "{}" && (
        <Card>
          <CardHeader>
            <CardTitle>Goals</CardTitle>
            <CardDescription>
              Key-value pairs describing the project's objectives.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <div className="divide-y rounded-md border">
              {parseGoals(project.goals).map(([key, value], i) => (
                <div key={i} className="flex gap-4 px-4 py-3 text-sm">
                  <span className="w-1/3 font-medium text-muted-foreground">
                    {key}
                  </span>
                  <span className="flex-1">{value}</span>
                </div>
              ))}
            </div>
          </CardContent>
        </Card>
      )}

      {/* Live event feed */}
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between">
            <div>
              <CardTitle>Live Events</CardTitle>
              <CardDescription>
                Real-time project lifecycle events (streamed via NATS).
              </CardDescription>
            </div>
            <StreamStatusBadge status={status} />
          </div>
        </CardHeader>
        <CardContent>
          {events.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No events yet. Create or update this project to see events
              stream live.
            </p>
          ) : (
            <div className="space-y-2">
              {events.map((resp, i) => {
                const evt = resp.event;
                if (!evt) return null;
                return (
                  <div
                    key={`${evt.eventId}-${i}`}
                    className="flex items-start gap-3 rounded-md border p-3 text-sm"
                  >
                    <span className="mt-0.5 text-xs font-mono text-muted-foreground">
                      {evt.occurredAt
                        ? new Date(
                            Number(evt.occurredAt.seconds) * 1000,
                          ).toLocaleTimeString()
                        : "--:--:--"}
                    </span>
                    <div className="flex-1">
                      <span className="font-medium">
                        {evt.eventType || "unknown"}
                      </span>
                      {evt.payload && evt.payload.length > 0 && (
                        <pre className="mt-1 overflow-auto rounded bg-muted p-2 text-xs text-muted-foreground">
                          {formatPayload(evt.payload)}
                        </pre>
                      )}
                    </div>
                    <span className="text-xs text-muted-foreground">
                      #{String(resp.sequence)}
                    </span>
                  </div>
                );
              })}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function StreamStatusBadge({ status }: { status: string }) {
  const colors: Record<string, string> = {
    idle: "text-muted-foreground",
    connecting: "text-yellow-600",
    open: "text-green-600",
    reconnecting: "text-yellow-600",
    closed: "text-muted-foreground",
    error: "text-destructive",
  };
  const labels: Record<string, string> = {
    idle: "idle",
    connecting: "connecting…",
    open: "live",
    reconnecting: "reconnecting…",
    closed: "disconnected",
    error: "error",
  };
  return (
    <span className={`text-xs font-medium ${colors[status] ?? ""}`}>
      ● {labels[status] ?? status}
    </span>
  );
}

function statusLabel(status: number): string {
  const labels: Record<number, string> = {
    1: "drafting",
    2: "active",
    3: "paused",
    4: "archived",
    5: "deleted",
  };
  return labels[status] ?? "unknown";
}

function parseGoals(s: string): [string, string][] {
  try {
    const m = JSON.parse(s);
    return Object.entries(m) as [string, string][];
  } catch {
    return [];
  }
}

function formatPayload(data: Uint8Array): string {
  try {
    return JSON.stringify(JSON.parse(new TextDecoder().decode(data)), null, 2);
  } catch {
    return `${data.length} bytes`;
  }
}
