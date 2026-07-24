import { createRoute, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { useForm } from "react-hook-form";
import { ArrowLeft } from "lucide-react";

import {
  useCreateWorker,
  useCreateWorkerVersion,
  useDeleteWorker,
  useDeleteWorkerVersion,
  useDeprecateWorker,
  useGetWorker,
  useGetWorkerVersion,
  useListWorkerVersions,
  usePublishWorkerVersion,
  useRetireWorker,
  useRevertWorkerVersionToDraft,
  useSetActiveWorkerVersion,
  useUpdateWorkerVersion,
} from "@/api/workers";
import { EntityYamlView } from "@/components/EntityYamlView";
import { FileInputButton } from "@/components/FileInputButton";
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
  role: string;
  skills: string;
  behavior: string;
  agentsMd: string;
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
  const createWorker = useCreateWorker();
  const navigate = useNavigate();
  const deleteMutation = useDeleteWorker();
  const setActiveVersion = useSetActiveWorkerVersion();
  const revertVersion = useRevertWorkerVersionToDraft();
  const deleteVersion = useDeleteWorkerVersion();
  const { data: latestData } = useGetWorker(id);
  const latestVersion = latestData?.latestVersion;
  const [editing, setEditing] = useState(false);
  const [viewMode, setViewMode] = useState<"detail" | "code">("detail");
  const [selectedVersionId, setSelectedVersionId] = useState<string | undefined>();
  const { data: selectedVersionData } = useGetWorkerVersion(selectedVersionId ?? "");
  const selectedVersion = selectedVersionId
    ? selectedVersionData ?? versions?.find((v) => v.id === selectedVersionId)
    : latestVersion;

  const { register, handleSubmit, setValue, watch, formState: { errors } } = useForm<EditFormData>({
    defaultValues: {
      runtimeRef: "",
      modelRef: "",
      role: "",
      skills: "",
      behavior: "",
      agentsMd: "",
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
          role: latestVersion.systemPrompt?.match(/# Role\n\n([\s\S]*?)(?=\n# |\n*$)/)?.[1] ?? "",
          skills: latestVersion.systemPrompt?.match(/# Skills\n\n([\s\S]*?)(?=\n# |\n*$)/)?.[1] ?? "",
          behavior: latestVersion.systemPrompt?.match(/# Behavior\n\n([\s\S]*?)(?=\n# |\n*$)/)?.[1] ?? "",
          agentsMd: latestVersion.systemPrompt?.match(/# AGENTS\.md\n\n([\s\S]*?)(?=\n# |\n*$)/)?.[1] ?? "",
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
  const isPublished = worker.status === 2;
  const isDeprecated = worker.status === 3;
  const isRetired = worker.status === 4;

  const draftVersion = versions?.find((v) => v.status === 1);
  const isEditingEnabled = draftVersion && editing;

  return (
    <div className="mx-auto max-w-6xl space-y-6">
      {/* Header + lifecycle actions */}
      <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div className="flex min-w-0 items-center gap-2">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => navigate({ to: "/workers" })}
            className="shrink-0"
          >
            <ArrowLeft className="h-4 w-4" />
            <span className="ml-1 hidden sm:inline">Back</span>
          </Button>
          <div className="min-w-0">
            <h1 className="text-lg font-semibold tracking-tight sm:text-2xl">
              {worker.name}
            </h1>
            <p className="truncate font-mono text-xs text-muted-foreground">
              {worker.slug}
            </p>
          </div>
        </div>
        {/* Action buttons: wrap on narrow viewports so the row doesn't
            force a horizontal scroll on phones. Each button stays its
            natural width; gap-2 + flex-wrap drops them onto multiple
            lines cleanly. */}
        <div className="flex flex-wrap items-center gap-2">
          {selectedVersionId && selectedVersion && selectedVersion.version !== worker.currentVersion && selectedVersion.status === 2 && (
            <Button variant="outline" onClick={() => setActiveVersion.mutate({ workerId: id, version: selectedVersion.version })}>
              {setActiveVersion.isPending ? "Setting…" : "Make Active"}
            </Button>
          )}
          {selectedVersionId && selectedVersion && (
            <>
              {selectedVersion.status === 1 && (
                <Button onClick={() => setEditing(true)}>Edit</Button>
              )}
              {selectedVersion.status === 2 && (
                <Button onClick={async () => {
                  await revertVersion.mutateAsync({ versionId: selectedVersion.id });
                  setEditing(true);
                }}>
                  Edit (revert to draft)
                </Button>
              )}
              <Button variant="outline" className="text-destructive border-destructive/50" onClick={() => {
                if (window.confirm("Delete version v" + selectedVersion.version + "? This cannot be undone.")) {
                  deleteVersion.mutate({ workerId: id, versionId: selectedVersion.id });
                }
              }}>
                Delete
              </Button>
            </>
          )}
          {draftVersion && viewMode === "detail" && !selectedVersionId && (
            <>
              {editing ? (
                <>
                  <Button type="submit" form="draftForm" disabled={updateVersion.isPending}>
                    {updateVersion.isPending ? "Saving…" : "Save"}
                  </Button>
                  <Button
                    variant="outline"
                    onClick={() => setEditing(false)}
                  >
                    Cancel
                  </Button>
                </>
              ) : (
                <>
                  <Button onClick={() => setEditing(true)}>Edit</Button>
                  <Button
                    onClick={() => {
                      handleSubmit(async (formData) => {
                        await updateVersion.mutateAsync({
                          workerId: id,
                          versionId: draftVersion.id,
                          runtimeRef: formData.runtimeRef,
                          modelRef: formData.modelRef,
                          systemPrompt: [formData.role, formData.skills, formData.behavior, formData.agentsMd].filter(Boolean).join("\n\n"),
                          permissions: formData.permissions,
                          gatedTools: formData.gatedTools,
                          budgetOverrides: formData.budgetOverrides,
                          contextSources: formData.contextSources,
                          versionNote: formData.versionNote,
                        });
                        publishVersion.mutateAsync(id);
                      })();
                    }}
                    disabled={updateVersion.isPending || publishVersion.isPending}
                  >
                    {publishVersion.isPending
                      ? "Publishing…"
                      : "Publish v" + (draftVersion.version)}
                  </Button>
                </>
              )}
            </>
          )}
          {isPublished && !draftVersion && viewMode === "detail" && (
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
          )}
          {isPublished && viewMode === "detail" && (
            <Button
              variant="outline"
              onClick={() => deprecateWorker.mutateAsync(id)}
              disabled={deprecateWorker.isPending}
            >
              {deprecateWorker.isPending ? "Deprecating…" : "Deprecate"}
            </Button>
          )}
          {isDeprecated && viewMode === "detail" && (
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
            id: worker.id,
            name: worker.name,
            slug: worker.slug,
            description: worker.description || undefined,
            purpose: worker.purpose || undefined,
            status: statusLabel(worker.status),
            current_version: worker.currentVersion,
            ...(latestVersion
              ? {
                  latest_version: {
                    version: latestVersion.version,
                    status: versionStatusLabel(latestVersion.status),
                    runtime_ref: latestVersion.runtimeRef,
                    model_ref: latestVersion.modelRef,
                    system_prompt: latestVersion.systemPrompt || undefined,
                    permissions: safeParseJson(latestVersion.permissions),
                    gated_tools: safeParseJson(latestVersion.gatedTools),
                    budget_overrides: safeParseJson(
                      latestVersion.budgetOverrides,
                    ),
                    context_sources: safeParseJson(
                      latestVersion.contextSources,
                    ),
                    concurrency_limit: latestVersion.concurrencyLimit,
                    execution_policy_ref:
                      latestVersion.executionPolicyRef || undefined,
                  },
                }
              : {}),
          }}
          title="Worker YAML"
          onClone={async () => {
            const name = window.prompt(
              "Clone name:",
              `Clone of ${worker.name}`,
            );
            if (!name) return;
            const result = await createWorker.mutateAsync({
              name,
              slug: worker.slug,
              description: worker.description,
              purpose: worker.purpose,
            });
            navigate({ to: `/workers/${result.worker.id}` });
          }}
          cloneDisabled={createWorker.isPending}
        />
      ) : (
      <>
      {/* Status cards: scale column count with viewport so phones
          don't get a horizontally-scrolling 5-column row. */}
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-5">
        <Card className="min-w-0">
          <CardHeader>
            <CardDescription>Status</CardDescription>
            <CardTitle className="text-base capitalize">
              {statusLabel(worker.status)}
            </CardTitle>
          </CardHeader>
        </Card>
        <Card className="min-w-0">
          <CardHeader>
            <CardDescription>Current version</CardDescription>
            <CardTitle className="text-base">
              v{worker.currentVersion || "—"}
            </CardTitle>
          </CardHeader>
        </Card>
        <Card className="min-w-0">
          <CardHeader>
            <CardDescription>Runtime</CardDescription>
            <CardTitle className="break-all text-base font-mono text-sm">
              {latestVersion?.runtimeRef || "—"}
            </CardTitle>
          </CardHeader>
        </Card>
        <Card className="min-w-0">
          <CardHeader>
            <CardDescription>Model</CardDescription>
            <CardTitle className="break-all text-base font-mono text-sm">
              {latestVersion?.modelRef || "—"}
            </CardTitle>
          </CardHeader>
        </Card>
        <Card className="min-w-0">
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
              id="draftForm"
              onSubmit={handleSubmit(async (formData) => {
                await updateVersion.mutateAsync({
                  workerId: id,
                  versionId: draftVersion.id,
                  runtimeRef: formData.runtimeRef,
                  modelRef: formData.modelRef,
                  systemPrompt: [formData.role, formData.skills, formData.behavior, formData.agentsMd].filter(Boolean).join("\n\n"),
                  permissions: formData.permissions,
                  gatedTools: formData.gatedTools,
                  budgetOverrides: formData.budgetOverrides,
                  contextSources: formData.contextSources,
                  versionNote: formData.versionNote,
                });
                setEditing(false);
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
                <div className="flex items-center justify-between">
                  <Label htmlFor="role">Role</Label>
                  <FileInputButton onLoad={(c) => setValue("role", c, { shouldValidate: true })} />
                </div>
                <Textarea id="role" className="min-h-[80px] font-mono text-xs" {...register("role")} />
              </div>
              <div className="space-y-2">
                <div className="flex items-center justify-between">
                  <Label htmlFor="skills">Skills</Label>
                  <FileInputButton onLoad={(c) => setValue("skills", c, { shouldValidate: true })} multiple label="Load files" />
                </div>
                <Textarea id="skills" className="min-h-[80px] font-mono text-xs" {...register("skills")} />
              </div>
              <div className="space-y-2">
                <div className="flex items-center justify-between">
                  <Label htmlFor="behavior">Behavior</Label>
                  <FileInputButton onLoad={(c) => setValue("behavior", c, { shouldValidate: true })} />
                </div>
                <Textarea id="behavior" className="min-h-[80px] font-mono text-xs" {...register("behavior")} />
              </div>
              <div className="space-y-2">
                <div className="flex items-center justify-between">
                  <Label htmlFor="agentsMd">AGENTS.md</Label>
                  <FileInputButton onLoad={(c) => setValue("agentsMd", c, { shouldValidate: true })} accept=".md,.txt" multiple label="Load file(s)" />
                </div>
                <Textarea id="agentsMd" className="min-h-[120px] font-mono text-xs" {...register("agentsMd")} />
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
            </form>
          </CardContent>
        </Card>
      )}

      {/* Versions card — list + detail in one place */}
      <Card>
        <CardHeader>
          <CardTitle>Versions</CardTitle>
          <CardDescription>
            Published versions are immutable. Click a version to view its details. Drafts can be edited and deleted.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="space-y-2">
            {(versions ?? []).length === 0 ? (
              <p className="text-sm text-muted-foreground">No versions yet.</p>
            ) : (
              (versions ?? []).map((ver) => {
                const isLatest = ver.version === worker.currentVersion;
                const isSelected = selectedVersionId === ver.id || (!selectedVersionId && isLatest && !selectedVersionData);
                return (
                  <div key={ver.id} className={cn("rounded-md border", isSelected && "border-primary")}>
                    <button
                      type="button"
                      className={cn(
                        "flex w-full items-center gap-3 p-2 text-left text-sm hover:bg-accent",
                        isSelected && "bg-accent/50",
                      )}
                      onClick={() => setSelectedVersionId(selectedVersionId === ver.id ? undefined : ver.id)}
                    >
                      <span className="font-mono font-medium">v{ver.version}</span>
                      <WorkerVersionStatusBadge status={ver.status} />
                      {ver.versionNote && <span className="min-w-0 truncate text-xs text-muted-foreground">{ver.versionNote}</span>}
                      {ver.publishedAt && (
                        <span className="shrink-0 text-xs text-muted-foreground">
                          {new Date(Number(ver.publishedAt.seconds) * 1000).toLocaleDateString()}
                        </span>
                      )}
                      {isLatest && (
                        <span className="shrink-0 rounded bg-blue-100 px-1.5 py-0.5 text-[10px] font-medium text-blue-800 dark:bg-blue-950/60 dark:text-blue-100">
                          active
                        </span>
                      )}
                      <span className="flex-1" />
                    </button>
                    {isSelected && selectedVersion && selectedVersion.id === ver.id && !isEditingEnabled && (
                      <VersionDetailPanel version={selectedVersion} />
                    )}
                  </div>
                );
              })
            )}
          </div>
        </CardContent>
      </Card>
        </>
      )}
    </div>
  );
}

function safeParseJson(s: string): unknown {
  try {
    return JSON.parse(s);
  } catch {
    return s;
  }
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

function WorkerVersionStatusBadge({ status }: { status: number }) {
  const labels: Record<number, string> = { 1: "draft", 2: "published", 3: "deprecated", 4: "retired" };
  const styles: Record<number, string> = {
    1: "bg-yellow-100 text-yellow-800",
    2: "bg-green-100 text-green-800",
    3: "bg-orange-100 text-orange-800",
    4: "bg-gray-200 text-gray-600",
  };
  return <span className={cn("rounded-full px-2 py-0.5 text-[10px] font-medium", styles[status] ?? "")}>{labels[status] ?? "unknown"}</span>;
}
function VersionDetailPanel({ version }: { version: import("@/api/gen/orchicon/api/v1/worker_pb").WorkerVersion }) {
  const sp = version.systemPrompt || "";
  const extract = (heading: string) => {
    const re = new RegExp(`# ${heading}\n\n([\\s\\S]*?)(?=\\n# |\\n*$)`);
    return sp.match(re)?.[1]?.trim() || "";
  };
  const role = extract("Role");
  const skills = extract("Skills");
  const behavior = extract("Behavior");
  const agents = extract("AGENTS\\.md");
  return (
    <div className="border-t p-3 space-y-4">
      <div className="grid gap-4 md:grid-cols-2">
        {role && <div><h4 className="text-xs font-medium uppercase text-muted-foreground">Role</h4><Markdown>{role}</Markdown></div>}
        {skills && <div><h4 className="text-xs font-medium uppercase text-muted-foreground">Skills</h4><Markdown>{skills}</Markdown></div>}
        {behavior && <div><h4 className="text-xs font-medium uppercase text-muted-foreground">Behavior</h4><Markdown>{behavior}</Markdown></div>}
        {agents && <div><h4 className="text-xs font-medium uppercase text-muted-foreground">AGENTS.md</h4><Markdown>{agents}</Markdown></div>}
      </div>
      <div className="grid gap-4 md:grid-cols-2 text-sm">
        <JsonField label="Permissions" value={version.permissions} />
        <JsonField label="Gated tools" value={version.gatedTools} />
        <JsonField label="Budget overrides" value={version.budgetOverrides} />
        <JsonField label="Context sources" value={version.contextSources} />
      </div>
      <div className="grid gap-4 md:grid-cols-2 text-sm">
        <JsonField label="Runtime" value={version.runtimeRef || "—"} />
        <JsonField label="Model" value={version.modelRef || "—"} />
      </div>
      <div className="grid gap-4 md:grid-cols-2 text-sm">
        <div>
          <h4 className="text-xs font-medium uppercase text-muted-foreground">Concurrency limit</h4>
          <p className="mt-1">{version.concurrencyLimit}</p>
        </div>
        <div>
          <h4 className="text-xs font-medium uppercase text-muted-foreground">Execution policy ref</h4>
          <p className="mt-1 font-mono text-xs">{version.executionPolicyRef || "—"}</p>
        </div>
      </div>
    </div>
  );
}

function formatJson(s: string): string {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}
