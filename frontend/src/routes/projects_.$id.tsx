import { createRoute, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { useForm } from "react-hook-form";
import { useQueryClient } from "@tanstack/react-query";
import { ArrowLeft } from "lucide-react";

import {
  useActivateProject,
  useArchiveProject,
  useCreateProject,
  useDeleteProject,
  useGetProject,
  useUpdateProject,
  projectKeys,
} from "@/api/projects";
import { useStreamProjectEvents } from "@/api/projectEvents";
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
import { FileBrowser } from "@/components/FileBrowser";

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
  const activateProject = useActivateProject();
  const createProject = useCreateProject();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [editing, setEditing] = useState(false);
  const [viewMode, setViewMode] = useState<"detail" | "code">("detail");

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
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex min-w-0 items-center gap-2">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => navigate({ to: "/projects" })}
            className="shrink-0"
          >
            <ArrowLeft className="h-4 w-4" />
            <span className="ml-1 hidden sm:inline">Back</span>
          </Button>
          <div className="min-w-0">
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
                <h1 className="text-lg font-semibold tracking-tight sm:text-2xl">
                  {project.name}
                </h1>
                <p className="truncate font-mono text-xs text-muted-foreground">
                  {project.slug}
                </p>
              </>
            )}
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {!editing && viewMode === "detail" && (
            <Button variant="outline" onClick={() => setEditing(true)}>
              Edit
            </Button>
          )}
          {project.status === 1 && viewMode === "detail" && (
            <Button
              onClick={() => activateProject.mutateAsync(id)}
              disabled={activateProject.isPending}
            >
              {activateProject.isPending ? "Activating…" : "Activate"}
            </Button>
          )}
          {project.status !== 4 && viewMode === "detail" && (
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
            id: project.id,
            name: project.name,
            slug: project.slug,
            status: statusLabel(project.status),
            version: project.version,
            ...(project.goals && project.goals !== "{}"
              ? { goals: parseGoals(project.goals) }
              : {}),
            ...(project.projectDir ? { project_dir: project.projectDir } : {}),
            ...(project.contextFiles?.length
              ? { context_files: project.contextFiles }
              : {}),
            created_at: project.createdAt
              ? new Date(
                  Number(project.createdAt.seconds) * 1000,
                ).toISOString()
              : null,
            updated_at: project.updatedAt
              ? new Date(
                  Number(project.updatedAt.seconds) * 1000,
                ).toISOString()
              : null,
          }}
          title="Project YAML"
          onClone={async () => {
            const name = window.prompt(
              "Clone name:",
              `Clone of ${project.name}`,
            );
            if (!name) return;
            const result = await createProject.mutateAsync({ name });
            navigate({ to: `/projects/${result.id}` });
          }}
          cloneDisabled={createProject.isPending}
        />
      ) : (
        <>
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
                  <span className="flex-1"><Markdown>{value}</Markdown></span>
                </div>
              ))}
            </div>
          </CardContent>
        </Card>
      )}

      {/* File browser section */}
      {project && (
        <FileBrowser
          projectId={project.id}
          projectDir={project.projectDir || ""}
          initialSelectedFiles={project.contextFiles || []}
          readOnly={!editing}
        />
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
        </>
      )}
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
