import { createRoute, useNavigate } from "@tanstack/react-router";

import { useArchiveProject, useGetProject } from "@/api/projects";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Route as rootRoute } from "@/routes/__root";

// Project detail (docs/10 §5). Shows the project's lifecycle state,
// goals, and version. Archive is a server-confirmed mutation (no
// optimistic transition — docs/10 invariant #3).
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/projects/$id",
  component: ProjectDetailPage,
});

function ProjectDetailPage() {
  const { id } = Route.useParams();
  const { data: project, isLoading, error } = useGetProject(id);
  const archiveProject = useArchiveProject();
  const navigate = useNavigate();

  const handleArchive = async () => {
    await archiveProject.mutateAsync(id);
    navigate({ to: "/projects" });
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
      <div className="flex items-start justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">
            {project.name}
          </h1>
          <p className="font-mono text-xs text-muted-foreground">
            {project.slug}
          </p>
        </div>
        <div className="flex gap-2">
          {project.status !== 4 && (
            <Button
              variant="outline"
              onClick={handleArchive}
              disabled={archiveProject.isPending}
            >
              {archiveProject.isPending ? "Archiving…" : "Archive"}
            </Button>
          )}
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
              JSON document describing the project's objectives.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <pre className="overflow-auto rounded-md bg-muted p-4 text-xs">
              {formatJson(project.goals)}
            </pre>
          </CardContent>
        </Card>
      )}
    </div>
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

function formatJson(s: string): string {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}
