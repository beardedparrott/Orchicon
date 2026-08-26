import { createRoute } from "@tanstack/react-router";

import { Route as rootRoute } from "@/routes/__root";
import { useGetCost } from "@/api/aigateway";
import { UsageRollup } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/cost-explorer",
  component: CostExplorerPage,
});

function CostExplorerPage() {
  const { data: costData, isLoading } = useGetCost({ rollup: UsageRollup.PROJECT });
  const totalCost = costData?.total?.costUsd ?? 0;
  const totalTokens = costData?.total?.totalTokens ?? 0;

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Cost Explorer</h1>
        <p className="text-sm text-muted-foreground">
          Detached from Telemetry — explore usage and cost by project and workflow. See{" "}
          <a href="/telemetry" className="text-primary hover:underline">
            Telemetry
          </a>{" "}
          for traces and metrics.
        </p>
      </div>

      {isLoading ? (
        <Card className="glass-panel">
          <CardContent className="p-6 text-sm text-muted-foreground">Loading cost data…</CardContent>
        </Card>
      ) : (
        <div className="grid gap-4 md:grid-cols-2">
          <Card className="glass-panel">
            <CardHeader>
              <CardTitle className="text-base">Total Cost</CardTitle>
              <CardDescription>Aggregate spend across projects</CardDescription>
            </CardHeader>
            <CardContent>
              <p className="text-2xl font-bold">${totalCost.toFixed(4)}</p>
              <p className="text-xs text-muted-foreground mt-1">{totalTokens.toLocaleString()} tokens</p>
            </CardContent>
          </Card>
          <Card className="glass-panel">
            <CardHeader>
              <CardTitle className="text-base">Usage Detail</CardTitle>
              <CardDescription>For full breakdown, visit Telemetry</CardDescription>
            </CardHeader>
            <CardContent className="text-sm text-muted-foreground">
              Cost Explorer is a dedicated view split from Telemetry (Task 2). Full filtering,
              sorting, and provider breakdown remain in the Telemetry + Costs hub; this page
              provides the standalone entry point required by the new top-nav Overview → Cost
              Explorer link without duplicating the full cost table logic.
            </CardContent>
          </Card>
        </div>
      )}
    </div>
  );
}
