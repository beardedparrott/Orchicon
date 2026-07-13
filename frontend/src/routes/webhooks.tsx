import { useState } from "react";
import { createRoute } from "@tanstack/react-router";

import {
  useListSubscriptions,
  useCreateSubscription,
  useDeleteSubscription,
  useTestSubscription,
  useListDeliveries,
  useReplayDelivery,
} from "@/api/webhooks";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

// Webhook subscription management (docs/10 §5, docs/07 §3.11).
// Create/list/test/delete subscriptions + view delivery attempts +
// replay dead-lettered events.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/webhooks",
  component: WebhooksPage,
});

function WebhooksPage() {
  const [selected, setSelected] = useState<string>("");
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Webhooks</h1>
        <p className="text-sm text-muted-foreground">
          Deliver events to external systems via HTTP POST with HMAC
          signing + retries + dead-letter.
        </p>
      </div>
      <div className="grid gap-6 md:grid-cols-2">
        <SubscriptionsPanel onSelect={setSelected} selected={selected} />
        <DeliveriesPanel subscriptionId={selected} />
      </div>
    </div>
  );
}

function SubscriptionsPanel({
  selected,
  onSelect,
}: {
  selected: string;
  onSelect: (id: string) => void;
}) {
  const { data, isLoading } = useListSubscriptions();
  const create = useCreateSubscription();
  const del = useDeleteSubscription();
  const test = useTestSubscription();
  const [name, setName] = useState("");
  const [url, setUrl] = useState("https://example.com/webhook");
  const [filter, setFilter] = useState("*");

  return (
    <Card>
      <CardHeader>
        <CardTitle>Subscriptions</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="grid gap-2">
          <Label htmlFor="wh-name">Name</Label>
          <Input id="wh-name" value={name} onChange={(e) => setName(e.target.value)} />
          <Label htmlFor="wh-url">Target URL</Label>
          <Input id="wh-url" value={url} onChange={(e) => setUrl(e.target.value)} />
          <Label htmlFor="wh-filter">Event filter</Label>
          <Input id="wh-filter" value={filter} onChange={(e) => setFilter(e.target.value)} />
          <Button
            onClick={() =>
              create.mutate({ name, targetUrl: url, eventFilter: filter })
            }
            disabled={!name || !url || create.isPending}
          >
            Create
          </Button>
        </div>
        <div className="space-y-2">
          {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
          {data?.map((s) => (
            <button
              key={s.id}
              onClick={() => onSelect(s.id)}
              className={cn(
                "w-full rounded-md border p-3 text-left transition-colors",
                selected === s.id ? "border-primary bg-accent" : "hover:bg-accent"
              )}
            >
              <div className="flex items-center justify-between">
                <span className="font-medium">{s.name}</span>
                <span className="text-xs text-muted-foreground">{s.status}</span>
              </div>
              <div className="mt-1 font-mono text-xs text-muted-foreground">{s.targetUrl}</div>
              <div className="mt-1 flex items-center gap-2">
                <span className="rounded bg-muted px-1.5 py-0.5 text-xs">{s.eventFilter}</span>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={(e) => {
                    e.stopPropagation();
                    test.mutate(s.id);
                  }}
                >
                  Test
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={(e) => {
                    e.stopPropagation();
                    del.mutate(s.id);
                  }}
                >
                  Delete
                </Button>
              </div>
            </button>
          ))}
          {data && data.length === 0 && (
            <p className="text-sm text-muted-foreground">No subscriptions yet.</p>
          )}
        </div>
      </CardContent>
    </Card>
  );
}

function DeliveriesPanel({ subscriptionId }: { subscriptionId: string }) {
  const { data, isLoading } = useListDeliveries(subscriptionId || undefined);
  const replay = useReplayDelivery();
  return (
    <Card>
      <CardHeader>
        <CardTitle>Delivery attempts</CardTitle>
      </CardHeader>
      <CardContent>
        {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
        {!subscriptionId && (
          <p className="text-sm text-muted-foreground">
            Select a subscription to view its delivery attempts.
          </p>
        )}
        <div className="space-y-2">
          {data?.map((d) => (
            <div key={d.id} className="rounded-md border p-3 text-sm">
              <div className="flex items-center justify-between">
                <span className="font-mono text-xs">{d.eventType}</span>
                <span
                  className={cn(
                    "rounded px-1.5 py-0.5 text-xs",
                    d.status === "delivered"
                      ? "bg-green-100 text-green-800"
                      : d.status === "dead_letter"
                      ? "bg-red-100 text-red-800"
                      : "bg-yellow-100 text-yellow-800"
                  )}
                >
                  {d.status}
                </span>
              </div>
              <div className="mt-1 text-xs text-muted-foreground">
                attempt {d.attempt} · HTTP {d.statusCode || "—"}
                {d.error && ` · ${d.error}`}
              </div>
              {d.status === "dead_letter" && (
                <Button
                  variant="outline"
                  size="sm"
                  className="mt-2"
                  onClick={() => replay.mutate(d.id)}
                >
                  Replay
                </Button>
              )}
            </div>
          ))}
          {data && data.length === 0 && subscriptionId && (
            <p className="text-sm text-muted-foreground">No deliveries yet.</p>
          )}
        </div>
      </CardContent>
    </Card>
  );
}
