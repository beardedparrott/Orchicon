import { Link, createRoute } from "@tanstack/react-router";

import { useListPolicies } from "@/api/policies";
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

// Policies list (docs/10 §5, docs/02 §2.5). Tier 1 Rego-only baseline.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/policies",
  component: PoliciesPage,
});

function PoliciesPage() {
  const { data: policies, isLoading, error } = useListPolicies();

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Policies</h1>
          <p className="text-sm text-muted-foreground">
            Rego-based decision-point policies (Tier 1). Evaluate at
            admission, dispatch, budget, approval, recovery, and completion.
          </p>
        </div>
        <Button asChild>
          <Link to="/policies/new">New Policy</Link>
        </Button>
      </div>

      {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
      {error && (
        <p className="text-sm text-destructive">
          Failed to load policies: {String(error)}
        </p>
      )}

      {policies && policies.length === 0 && (
        <Card>
          <CardHeader>
            <CardTitle>No policies yet</CardTitle>
            <CardDescription>
              Create a policy, write its Rego module, and publish it to
              govern a decision point. Narrowest scope wins; first
              definitive decision wins.
            </CardDescription>
          </CardHeader>
        </Card>
      )}

      {policies && policies.length > 0 && (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {policies.map((p) => (
            <Link key={p.id} to="/policies_/$id" params={{ id: p.id }}>
              <Card className="transition-colors hover:bg-accent">
                <CardHeader>
                  <CardTitle className="flex items-center justify-between">
                    <span className="truncate">{p.name}</span>
                    <StatusBadge status={p.status} />
                  </CardTitle>
                  <CardDescription>
                    <span className="text-xs">
                      v{p.currentVersion || "— (draft)"}
                    </span>
                  </CardDescription>
                </CardHeader>
                <CardContent />
              </Card>
            </Link>
          ))}
        </div>
      )}
    </div>
  );
}

function StatusBadge({ status }: { status: number }) {
  const label = STATUS_LABELS[status] ?? "unknown";
  return (
    <span
      className={cn(
        "rounded-full px-2 py-0.5 text-xs font-medium",
        STATUS_STYLES[status] ?? "bg-muted text-muted-foreground",
      )}
    >
      {label}
    </span>
  );
}

const STATUS_LABELS: Record<number, string> = {
  1: "draft",
  2: "published",
  3: "superseded",
};

const STATUS_STYLES: Record<number, string> = {
  1: "bg-blue-100 text-blue-800",
  2: "bg-green-100 text-green-800",
  3: "bg-yellow-100 text-yellow-800",
};
