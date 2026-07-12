import { createRoute } from "@tanstack/react-router";

import { Button } from "@/components/ui/button";
import { Route as rootRoute } from "@/routes/__root";

// Projects list — placeholder for the Projects slice (docs/10 §5).
// The data-access path (Connect-ES → control plane → data-access layer)
// is wired in a later phase.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/projects",
  component: ProjectsPage,
});

function ProjectsPage() {
  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-semibold tracking-tight">Projects</h1>
        <Button>New Project</Button>
      </div>
      <p className="text-sm text-muted-foreground">
        Projects are the persistent source of truth and the trust boundary
        (docs/02 §2.1). The project list and detail views arrive in the
        Projects slice.
      </p>
    </div>
  );
}
