import { createRoute } from "@tanstack/react-router";

import { useListAdapters } from "@/api/executions";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

// Adapter registry (docs/07 §3.7, docs/04 §2). Lists registered runtime
// adapters with their kind, version, capabilities, and health state.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/adapters",
  component: AdaptersPage,
});

function AdaptersPage() {
  const { data: adapters, isLoading, error } = useListAdapters();

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Adapters</h1>
        <p className="text-sm text-muted-foreground">
          Registered runtime adapters offering execution capabilities. The
          scheduler dispatches work to adapters that match the worker's
          runtime_ref and have healthy heartbeats.
        </p>
      </div>

      {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
      {error && (
        <p className="text-sm text-destructive">
          Failed to load adapters: {String(error)}
        </p>
      )}

      {adapters && adapters.length === 0 && (
        <Card>
          <CardHeader>
            <CardTitle>No adapters registered</CardTitle>
            <CardDescription>
              An adapter process registers itself with the control plane via
              the RuntimeAdapterService gRPC contract (docs/04 §2). The dev
              adapter is seeded automatically on boot.
            </CardDescription>
          </CardHeader>
        </Card>
      )}

      {adapters && adapters.length > 0 && (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {adapters.map((a) => (
            <Card key={a.id}>
              <CardHeader>
                <CardTitle className="flex items-center justify-between">
                  <span className="font-mono">{a.kind}</span>
                  <AdapterStatusBadge status={a.status} />
                </CardTitle>
                <CardDescription className="font-mono text-xs">
                  v{a.version} · {a.endpoint}
                </CardDescription>
              </CardHeader>
              <CardContent>
                <div className="space-y-2 text-xs">
                  <div>
                    <span className="text-muted-foreground">ID: </span>
                    <span className="font-mono">{a.id}</span>
                  </div>
                  <div>
                    <span className="text-muted-foreground">Max concurrent: </span>
                    {a.maxConcurrentExecutions}
                  </div>
                  {a.lastHeartbeatAt && (
                    <div>
                      <span className="text-muted-foreground">Last heartbeat: </span>
                      {new Date(
                        Number(a.lastHeartbeatAt.seconds) * 1000,
                      ).toLocaleTimeString()}
                    </div>
                  )}
                  {a.capabilities && a.capabilities !== "{}" && (
                    <div>
                      <span className="text-muted-foreground">Capabilities:</span>
                      <pre className="mt-1 max-h-40 overflow-auto rounded-md bg-muted p-2 text-xs">
                        {formatJson(a.capabilities)}
                      </pre>
                    </div>
                  )}
                </div>
              </CardContent>
            </Card>
          ))}
        </div>
      )}
    </div>
  );
}

function AdapterStatusBadge({ status }: { status: string }) {
  const styles: Record<string, string> = {
    registered: "bg-blue-100 text-blue-800",
    ready: "bg-green-100 text-green-800",
    draining: "bg-yellow-100 text-yellow-800",
    expired: "bg-red-100 text-red-800",
  };
  return (
    <span
      className={cn(
        "rounded-full px-2 py-0.5 text-xs font-medium",
        styles[status] ?? "bg-muted text-muted-foreground",
      )}
    >
      {status}
    </span>
  );
}

function formatJson(s: string): string {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}
