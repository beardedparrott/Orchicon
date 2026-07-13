import { createRoute } from "@tanstack/react-router";
import { useEffect, useState } from "react";

import {
  useDeprecateWorker,
  useGetEditLock,
  useGetWorker,
  useListWorkerVersions,
  usePublishWorkerVersion,
  useAcquireEditLock,
  useReleaseEditLock,
  useRetireWorker,
} from "@/api/workers";
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

// Worker detail (docs/10 §5, docs/05 §4, §5). Shows the worker header,
// latest version, version history, and lifecycle controls (publish,
// deprecate, retire). Edit locks prevent concurrent edits in the visual
// editor (docs/07 §3.3) — the lock is acquired on first interaction and
// released on unmount.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/workers/$id",
  component: WorkerDetailPage,
});

function WorkerDetailPage() {
  const { id } = Route.useParams();
  const { data, isLoading, error } = useGetWorker(id);
  const { data: versions } = useListWorkerVersions(id);
  const { data: editLock } = useGetEditLock(id);
  const publishVersion = usePublishWorkerVersion();
  const deprecateWorker = useDeprecateWorker();
  const retireWorker = useRetireWorker();
  const acquireLock = useAcquireEditLock();
  const releaseLock = useReleaseEditLock();

  // Edit lock lifecycle (docs/07 §3.3). Acquire when the user enters the
  // editor; release on unmount. A TTL expires it automatically if the
  // tab is closed without cleanup.
  const [lockActor] = useState(() => `user-${Math.random().toString(36).slice(2, 8)}`);
  const [lockAcquired, setLockAcquired] = useState(false);

  // Attempt to acquire the lock once on mount.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      const res = await acquireLock.mutateAsync({
        workerId: id,
        actor: lockActor,
      });
      if (!cancelled) setLockAcquired(res.acquired);
    })();
    return () => {
      cancelled = true;
      // Release the lock on unmount (best-effort).
      releaseLock.mutate({ workerId: id, actor: lockActor });
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id]);

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading…</p>;
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        Failed to load worker: {String(error)}
      </p>
    );
  }
  if (!data) {
    return null;
  }

  const { worker, latestVersion } = data;
  const isDraft = worker.status === 1;
  const isPublished = worker.status === 2;
  const isDeprecated = worker.status === 3;
  const lockHeldByOther =
    editLock && editLock.heldBy && editLock.heldBy !== lockActor;

  return (
    <div className="space-y-6">
      <div className="flex items-start justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">
            {worker.name}
          </h1>
          <p className="font-mono text-xs text-muted-foreground">
            {worker.slug}
          </p>
        </div>
        <div className="flex gap-2">
          {isDraft && (
            <Button
              onClick={() => publishVersion.mutateAsync(id)}
              disabled={publishVersion.isPending}
            >
              {publishVersion.isPending ? "Publishing…" : "Publish v" + (worker.currentVersion + 1)}
            </Button>
          )}
          {isPublished && (
            <Button
              variant="outline"
              onClick={() => deprecateWorker.mutateAsync(id)}
              disabled={deprecateWorker.isPending}
            >
              {deprecateWorker.isPending ? "Deprecating…" : "Deprecate"}
            </Button>
          )}
          {isDeprecated && (
            <Button
              variant="destructive"
              onClick={() => retireWorker.mutateAsync(id)}
              disabled={retireWorker.isPending}
            >
              {retireWorker.isPending ? "Retiring…" : "Retire"}
            </Button>
          )}
        </div>
      </div>

      {/* Edit lock indicator (docs/07 §3.3) */}
      <EditLockBanner
        lockHeldByOther={!!lockHeldByOther}
        heldBy={editLock?.heldBy ?? ""}
        lockAcquired={lockAcquired}
      />

      <div className="grid gap-4 md:grid-cols-4">
        <Card>
          <CardHeader>
            <CardDescription>Status</CardDescription>
            <CardTitle className="text-base capitalize">
              {statusLabel(worker.status)}
            </CardTitle>
          </CardHeader>
        </Card>
        <Card>
          <CardHeader>
            <CardDescription>Current version</CardDescription>
            <CardTitle className="text-base">
              v{worker.currentVersion || "—"}
            </CardTitle>
          </CardHeader>
        </Card>
        <Card>
          <CardHeader>
            <CardDescription>Runtime</CardDescription>
            <CardTitle className="text-base font-mono text-sm">
              {latestVersion?.runtimeRef || "—"}
            </CardTitle>
          </CardHeader>
        </Card>
        <Card>
          <CardHeader>
            <CardDescription>Model</CardDescription>
            <CardTitle className="text-base font-mono text-sm">
              {latestVersion?.modelRef || "—"}
            </CardTitle>
          </CardHeader>
        </Card>
      </div>

      {latestVersion && (
        <Card>
          <CardHeader>
            <CardTitle>Latest version (v{latestVersion.version})</CardTitle>
            <CardDescription>
              {latestVersion.versionNote || "No version note"}
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            {latestVersion.systemPrompt && (
              <div>
                <h4 className="text-xs font-medium uppercase text-muted-foreground">
                  System prompt
                </h4>
                <pre className="mt-1 max-h-60 overflow-auto rounded-md bg-muted p-4 text-xs">
                  {latestVersion.systemPrompt}
                </pre>
              </div>
            )}
            <div className="grid gap-4 md:grid-cols-2">
              <JsonField label="Permissions" value={latestVersion.permissions} />
              <JsonField label="Gated tools" value={latestVersion.gatedTools} />
              <JsonField label="Budget overrides" value={latestVersion.budgetOverrides} />
              <JsonField label="Context sources" value={latestVersion.contextSources} />
            </div>
            <div className="grid gap-4 md:grid-cols-2">
              <div>
                <h4 className="text-xs font-medium uppercase text-muted-foreground">
                  Concurrency limit
                </h4>
                <p className="mt-1 text-sm">{latestVersion.concurrencyLimit}</p>
              </div>
              <div>
                <h4 className="text-xs font-medium uppercase text-muted-foreground">
                  Execution policy ref
                </h4>
                <p className="mt-1 text-sm font-mono text-xs">
                  {latestVersion.executionPolicyRef || "—"}
                </p>
              </div>
            </div>
          </CardContent>
        </Card>
      )}

      {/* Version history (docs/05 §5) */}
      <Card>
        <CardHeader>
          <CardTitle>Version history</CardTitle>
          <CardDescription>
            All versions of this worker, newest first. A published version
            is immutable; changes create a new version.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {versions && versions.length === 0 && (
            <p className="text-sm text-muted-foreground">No versions yet.</p>
          )}
          {versions && versions.length > 0 && (
            <div className="space-y-2">
              {versions.map((v) => (
                <div
                  key={v.id}
                  className="flex items-start gap-3 rounded-md border p-3 text-sm"
                >
                  <span className="mt-0.5 font-mono text-xs font-medium">
                    v{v.version}
                  </span>
                  <div className="flex-1">
                    <div className="flex items-center gap-2">
                      <VersionStatusBadge status={v.status} />
                      <span className="text-xs text-muted-foreground">
                        {v.modelRef}
                      </span>
                    </div>
                    {v.versionNote && (
                      <p className="mt-1 text-xs text-muted-foreground">
                        {v.versionNote}
                      </p>
                    )}
                  </div>
                  {v.publishedAt && (
                    <span className="text-xs text-muted-foreground">
                      {new Date(
                        Number(v.publishedAt.seconds) * 1000,
                      ).toLocaleDateString()}
                    </span>
                  )}
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

// EditLockBanner shows the edit lock state (docs/07 §3.3). If another
// user holds the lock, the editor is read-only. If this client holds
// the lock, editing is enabled. Lock expires automatically on TTL.
function EditLockBanner({
  lockHeldByOther,
  heldBy,
  lockAcquired,
}: {
  lockHeldByOther: boolean;
  heldBy: string;
  lockAcquired: boolean;
}) {
  if (lockAcquired) {
    return (
      <div className="rounded-md border border-green-200 bg-green-50 p-3 text-sm text-green-800">
        ● Edit lock acquired — you can edit this worker.
      </div>
    );
  }
  if (lockHeldByOther) {
    return (
      <div className="rounded-md border border-yellow-200 bg-yellow-50 p-3 text-sm text-yellow-800">
        ⏳ Currently being edited by{" "}
        <span className="font-mono">{heldBy}</span> — viewing read-only. The
        lock expires automatically on disconnect.
      </div>
    );
  }
  return null;
}

function VersionStatusBadge({ status }: { status: number }) {
  const labels: Record<number, string> = {
    1: "draft",
    2: "published",
    3: "deprecated",
  };
  const styles: Record<number, string> = {
    1: "bg-blue-100 text-blue-800",
    2: "bg-green-100 text-green-800",
    3: "bg-yellow-100 text-yellow-800",
  };
  return (
    <span
      className={cn(
        "rounded-full px-2 py-0.5 text-xs font-medium",
        styles[status] ?? "bg-muted text-muted-foreground",
      )}
    >
      {labels[status] ?? "unknown"}
    </span>
  );
}

function JsonField({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <h4 className="text-xs font-medium uppercase text-muted-foreground">
        {label}
      </h4>
      <pre className="mt-1 max-h-40 overflow-auto rounded-md bg-muted p-3 text-xs">
        {formatJson(value)}
      </pre>
    </div>
  );
}

function statusLabel(status: number): string {
  const labels: Record<number, string> = {
    1: "draft",
    2: "published",
    3: "deprecated",
    4: "retired",
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
