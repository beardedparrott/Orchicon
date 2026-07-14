import { Link, createRoute } from "@tanstack/react-router";

import { useListProjects } from "@/api/projects";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

// Projects list (docs/10 §5). Fetches via Connect-ES + TanStack Query;
// the UI reflects server state only (AGENTS.md invariant #1).
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/projects",
  component: ProjectsPage,
});

function ProjectsPage() {
  const { data: projects, isLoading, error } = useListProjects();

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Projects</h1>
          <p className="text-sm text-muted-foreground">
            The persistent source of truth and trust boundary for work
            state.
          </p>
        </div>
        <Button asChild>
          <Link to="/projects_/new">New Project</Link>
        </Button>
      </div>

      {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
      {error && (
        <p className="text-sm text-destructive">
          Failed to load projects: {String(error)}
        </p>
      )}

      {projects && projects.length === 0 && (
        <Card>
          <CardHeader>
            <CardTitle>No projects yet</CardTitle>
            <CardDescription>
              Create your first project to start orchestrating autonomous
              AI work.
            </CardDescription>
          </CardHeader>
        </Card>
      )}

      {projects && projects.length > 0 && (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {projects.map((p) => (
            <Link key={p.id} to="/projects_/$id" params={{ id: p.id }}>
              <Card className="transition-colors hover:bg-accent">
                <CardHeader>
                  <CardTitle className="flex items-center justify-between">
                    <span className="truncate">{p.name}</span>
                    <StatusBadge status={p.status} />
                  </CardTitle>
                  <CardDescription className="font-mono text-xs">
                    {p.slug}
                  </CardDescription>
                </CardHeader>
                <CardContent>
                  <p className="text-xs text-muted-foreground">
                    v{p.version}
                  </p>
                </CardContent>
              </Card>
            </Link>
          ))}
        </div>
      )}
    </div>
  );
}

// StatusBadge renders a colored pill for the project lifecycle state.
// Colors mirror the domain model (docs/02 §2.1).
function StatusBadge({ status }: { status: number }) {
  const label = STATUS_LABELS[status] ?? "unknown";
  return (
    <span
      className={cn(
        "rounded-full px-2 py-0.5 text-xs font-medium",
        STATUS_STYLES[status] ?? "bg-muted text-muted-foreground"
      )}
    >
      {label}
    </span>
  );
}

const STATUS_LABELS: Record<number, string> = {
  1: "drafting",
  2: "active",
  3: "paused",
  4: "archived",
  5: "deleted",
};

const STATUS_STYLES: Record<number, string> = {
  1: "bg-blue-100 text-blue-800",
  2: "bg-green-100 text-green-800",
  3: "bg-yellow-100 text-yellow-800",
  4: "bg-gray-200 text-gray-700",
  5: "bg-red-100 text-red-800",
};
