import { createRoute, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { useForm } from "react-hook-form";

import {
  useCreateWorkerVersion,
  useDeleteWorker,
  useDeprecateWorker,
  useGetWorker,
  useListWorkerVersions,
  usePublishWorkerVersion,
  useRetireWorker,
  useUpdateWorkerVersion,
} from "@/api/workers";
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
import { Textarea } from "@/components/ui/textarea";
import { ModelPicker } from "@/components/ModelPicker";
import {
  BudgetSection,
  ContextSourcesSection,
  GatedToolsSection,
  PermissionsSection,
} from "@/components/WorkerFormSections";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

// Worker detail page: read-only for published/deprecated/retired workers;
// editable for draft workers. Published workers get a "New version" button
// that creates a draft fork. No edit lock — this is not the visual editor
// canvas (docs/07 §3.3).
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/workers/$id",
  component: WorkerDetailPage,
});

const DEFAULT_PERMISSIONS = `{
  "allow_all_tools": false,
  "allow_read": true,
  "allow_write": false,
  "model_providers": []
}`;

const DEFAULT_BUDGETS = `{
  "max_prompt_tokens": 0,
  "max_completion_tokens": 0,
  "max_cost_usd": 0
}`;

// Fields on UpdateWorkerVersionRequest — only version-level fields,
// not worker header fields (name, slug, description, purpose).
interface EditFormData {
  runtimeRef: string;
  modelRef: string;
  systemPrompt: string;
  permissions: string;
  gatedTools: string;
  budgetOverrides: string;
  contextSources: string;
  versionNote: string;
}

function WorkerDetailPage() {
  const { id } = Route.useParams();
  const { data, isLoading, error } = useGetWorker(id);
  const { data: versions } = useListWorkerVersions(id);
  const publishVersion = usePublishWorkerVersion();
  const deprecateWorker = useDeprecateWorker();
  const retireWorker = useRetireWorker();
  const updateVersion = useUpdateWorkerVersion();
  const createVersion = useCreateWorkerVersion();
  const navigate = useNavigate();
  const deleteMutation = useDeleteWorker();

  const { data: latestData } = useGetWorker(id);
  const latestVersion = latestData?.latestVersion;
  const [editing, setEditing] = useState(false);

  const { register, handleSubmit, setValue, watch, formState: { errors } } = useForm<EditFormData>({
    defaultValues: {
      runtimeRef: "",
      modelRef: "",
      systemPrompt: "",
      permissions: DEFAULT_PERMISSIONS,
      gatedTools: "[]",
      budgetOverrides: DEFAULT_BUDGETS,
      contextSources: "[]",
      versionNote: "",
    },
    values: latestVersion
      ? {
          runtimeRef: latestVersion.runtimeRef ?? "",
          modelRef: latestVersion.modelRef ?? "",
          systemPrompt: latestVersion.systemPrompt ?? "",
          permissions: latestVersion.permissions || DEFAULT_PERMISSIONS,
          gatedTools: latestVersion.gatedTools || "[]",
          budgetOverrides: latestVersion.budgetOverrides || DEFAULT_BUDGETS,
          contextSources: latestVersion.contextSources || "[]",
          versionNote: latestVersion.versionNote ?? "",
        }
      : undefined,
  });

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

  const { worker } = data;
  const isDraft = worker.status === 1;
  const isPublished = worker.status === 2;
  const isDeprecated = worker.status === 3;
  const isRetired = worker.status === 4;

  const draftVersion = isDraft
    ? versions?.find((v) => v.status === 1)
    : undefined;
  const isEditingEnabled = isDraft && editing && draftVersion;

  return (
    <div className="space-y-6">
      {/* Header + lifecycle actions */}
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
          {isDraft && latestVersion && !editing && (
            <Button onClick={() => setEditing(true)}>Edit</Button>
          )}
          {isDraft && latestVersion && (
            <Button
              onClick={() => publishVersion.mutateAsync(id)}
              disabled={publishVersion.isPending}
            >
              {publishVersion.isPending
                ? "Publishing…"
                : "Publish v" + (worker.currentVersion + 1)}
            </Button>
          )}
          {isPublished && (
            <>
              <Button
                onClick={() =>
                  createVersion.mutate(
                    { workerId: id },
                    {
                      onSuccess: () => setEditing(true),
                    },
                  )
                }
                disabled={createVersion.isPending}
              >
                {createVersion.isPending ? "Creating…" : "New version"}
              </Button>
              <Button
                variant="outline"
                onClick={() => deprecateWorker.mutateAsync(id)}
                disabled={deprecateWorker.isPending}
              >
                {deprecateWorker.isPending ? "Deprecating…" : "Deprecate"}
              </Button>
            </>
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
          {(isDeprecated || isRetired) && (
            <Button
              variant="destructive"
              onClick={() => {
                if (
                  window.confirm(
                    "Permanently delete this worker and all its versions? This cannot be undone.",
                  )
                ) {
                  deleteMutation.mutate(id, {
                    onSuccess: () => navigate({ to: "/workers" }),
                  });
                }
              }}
              disabled={deleteMutation.isPending}
            >
              {deleteMutation.isPending ? "Deleting…" : "Delete"}
            </Button>
          )}
        </div>
      </div>

      {/* Status cards */}
      <div className="grid gap-4 md:grid-cols-5">
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
        <Card>
          <CardHeader>
            <CardDescription>Purpose</CardDescription>
            <CardTitle className="text-sm font-normal leading-snug">
              {worker.purpose || "—"}
            </CardTitle>
          </CardHeader>
        </Card>
      </div>

      {/* Inline editor for draft versions */}
      {isEditingEnabled && (
        <Card>
          <CardHeader>
            <CardTitle>Edit draft v{draftVersion.version}</CardTitle>
            <CardDescription>
              Changes are saved immediately. JSON fields use structured
              controls — select options with descriptions instead of editing
              raw JSON.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-6">
            <form
              onSubmit={handleSubmit((formData) => {
                updateVersion.mutate(
                  {
                    workerId: id,
                    versionId: draftVersion.id,
                    runtimeRef: formData.runtimeRef,
                    modelRef: formData.modelRef,
                    systemPrompt: formData.systemPrompt,
                    permissions: formData.permissions,
                    gatedTools: formData.gatedTools,
                    budgetOverrides: formData.budgetOverrides,
                    contextSources: formData.contextSources,
                    versionNote: formData.versionNote,
                  },
                  {
                    onSuccess: () => setEditing(false),
                  },
                );
              })}
              className="space-y-6"
            >
              <div className="space-y-2">
                <Label htmlFor="versionNote">Version note</Label>
                <Input id="versionNote" {...register("versionNote")} />
              </div>

              <div className="grid gap-4 md:grid-cols-2">
                <div className="space-y-2">
                  <Label htmlFor="runtimeRef">Runtime</Label>
                  <Input id="runtimeRef" {...register("runtimeRef")} />
                </div>
                <div className="space-y-2">
                  <ModelPicker
                    value={watch("modelRef")}
                    onChange={(val) => setValue("modelRef", val)}
                  />
                </div>
              </div>

              <div className="space-y-2">
                <Label htmlFor="systemPrompt">System prompt</Label>
                <Textarea
                  id="systemPrompt"
                  className="min-h-[120px] font-mono text-xs"
                  {...register("systemPrompt")}
                />
              </div>

              <div className="space-y-2 rounded-lg border p-4">
                <Label>Permissions</Label>
                <PermissionsSection
                  value={watch("permissions")}
                  onChange={(v) => setValue("permissions", v)}
                />
              </div>

              <div className="space-y-2 rounded-lg border p-4">
                <Label>Gated tools (Tier 2 — per-call approval)</Label>
                <GatedToolsSection
                  value={watch("gatedTools")}
                  onChange={(v) => setValue("gatedTools", v)}
                />
              </div>

              <div className="space-y-2 rounded-lg border p-4">
                <Label>Budget overrides</Label>
                <BudgetSection
                  value={watch("budgetOverrides")}
                  onChange={(v) => setValue("budgetOverrides", v)}
                />
              </div>

              <div className="space-y-2 rounded-lg border p-4">
                <Label>Context sources</Label>
                <ContextSourcesSection
                  value={watch("contextSources")}
                  onChange={(v) => setValue("contextSources", v)}
                />
              </div>

              {errors.permissions && (
                <p className="text-xs text-destructive">
                  {errors.permissions.message}
                </p>
              )}
              {errors.gatedTools && (
                <p className="text-xs text-destructive">
                  {errors.gatedTools.message}
                </p>
              )}

              <div className="flex gap-2">
                <Button type="submit" disabled={updateVersion.isPending}>
                  {updateVersion.isPending ? "Saving…" : "Save changes"}
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => setEditing(false)}
                >
                  Cancel
                </Button>
              </div>
            </form>
          </CardContent>
        </Card>
      )}

      {/* Read-only version detail (shown when not editing) */}
      {!isEditingEnabled && latestVersion && (
        <Card>
          <CardHeader>
            <CardTitle>
              Version v{latestVersion.version}
              {latestVersion.versionNote
                ? ` — ${latestVersion.versionNote}`
                : ""}
            </CardTitle>
            <CardDescription>
              {versionStatusLabel(latestVersion.status)}
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
              <JsonField
                label="Permissions"
                value={latestVersion.permissions}
              />
              <JsonField
                label="Gated tools"
                value={latestVersion.gatedTools}
              />
              <JsonField
                label="Budget overrides"
                value={latestVersion.budgetOverrides}
              />
              <JsonField
                label="Context sources"
                value={latestVersion.contextSources}
              />
            </div>
            <div className="grid gap-4 md:grid-cols-2">
              <div>
                <h4 className="text-xs font-medium uppercase text-muted-foreground">
                  Concurrency limit
                </h4>
                <p className="mt-1 text-sm">
                  {latestVersion.concurrencyLimit}
                </p>
              </div>
              <div>
                <h4 className="text-xs font-medium uppercase text-muted-foreground">
                  Execution policy ref
                </h4>
                <p className="mt-1 font-mono text-xs">
                  {latestVersion.executionPolicyRef || "—"}
                </p>
              </div>
            </div>
          </CardContent>
        </Card>
      )}

      {/* Version history */}
      <Card>
        <CardHeader>
          <CardTitle>Version history</CardTitle>
          <CardDescription>
            All versions of this worker, newest first. A published version is
            immutable; changes create a new version.
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

function versionStatusLabel(status: number): string {
  const labels: Record<number, string> = {
    1: "Draft — editable, not yet published",
    2: "Published — immutable snapshot",
    3: "Deprecated — no new bindings",
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
