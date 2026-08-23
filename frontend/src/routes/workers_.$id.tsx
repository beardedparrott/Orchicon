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
  useUpdateWorker,
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
import type { WorkerVersion } from "@/api/gen/orchicon/api/v1/worker_pb";

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
  "tokens": 500000,
  "cost_usd": 0.5,
  "wall_clock_seconds": 3600,
  "tool_call_count": 100,
  "compact_max_turns": 12
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

// promptFields returns the four structured prompt fields from a version.
// Newer versions expose them directly; legacy versions only carry the
// composed `system_prompt`, so fall back to parsing its `# Heading` sections.
function promptFields(v: WorkerVersion | undefined): Pick<EditFormData, "role" | "skills" | "behavior" | "agentsMd"> {
  const empty = { role: "", skills: "", behavior: "", agentsMd: "" };
  if (!v) return empty;
  if (v.role || v.skills || v.behavior || v.agentsMd) {
    return { role: v.role, skills: v.skills, behavior: v.behavior, agentsMd: v.agentsMd };
  }
  const sp = v.systemPrompt || "";
  const extract = (heading: string) =>
    sp.match(new RegExp(`# ${heading}\n\n([\\s\\S]*?)(?=\\n# |\\n*$)`))?.[1] ?? "";
  return {
    role: extract("Role"),
    skills: extract("Skills"),
    behavior: extract("Behavior"),
    agentsMd: extract("AGENTS\\.md"),
  };
}

function WorkerDetailPage() {
  const { id } = Route.useParams();
  const { data, isLoading, error } = useGetWorker(id);
  const { data: versions } = useListWorkerVersions(id);
  const publishVersion = usePublishWorkerVersion();
  const deprecateWorker = useDeprecateWorker();
  const retireWorker = useRetireWorker();
  const updateWorker = useUpdateWorker();
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
    values: (selectedVersion ?? latestVersion)
      ? (() => {
          const pf = promptFields(selectedVersion ?? latestVersion);
          return {
            runtimeRef: (selectedVersion ?? latestVersion)!.runtimeRef ?? "",
            modelRef: (selectedVersion ?? latestVersion)!.modelRef ?? "",
            role: pf.role,
            skills: pf.skills,
            behavior: pf.behavior,
            agentsMd: pf.agentsMd,
            permissions: (selectedVersion ?? latestVersion)!.permissions || DEFAULT_PERMISSIONS,
            gatedTools: (selectedVersion ?? latestVersion)!.gatedTools || "[]",
            budgetOverrides: (selectedVersion ?? latestVersion)!.budgetOverrides || DEFAULT_BUDGETS,
            contextSources: (selectedVersion ?? latestVersion)!.contextSources || "[]",
            versionNote: (selectedVersion ?? latestVersion)!.versionNote ?? "",
          };
        })()
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
          {editing && (
            <>
              <Button type="submit" form="draftForm" disabled={updateVersion.isPending}>
                {updateVersion.isPending ? "Saving…" : "Save"}
              </Button>
              <Button variant="outline" onClick={() => setEditing(false)}>
                Cancel
              </Button>
            </>
          )}
          {draftVersion && viewMode === "detail" && !editing && !selectedVersionId && (
            <>
              <Button onClick={() => { setEditing(true); setSelectedVersionId(draftVersion.id); }}>Edit</Button>
              <Button
                onClick={() => {
                  handleSubmit(async (formData) => {
                    await updateVersion.mutateAsync({
                      workerId: id,
                      versionId: draftVersion.id,
                      runtimeRef: formData.runtimeRef,
                      modelRef: formData.modelRef,
                      role: formData.role,
                      skills: formData.skills,
                      behavior: formData.behavior,
                      agentsMd: formData.agentsMd,
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
            const pf = promptFields(latestVersion);
            const result = await createWorker.mutateAsync({
              name,
              // The server dedupes slugs per tenant (-2, -3, ...) so a
              // repeated clone can never collide on the slug index.
              slug: `${worker.slug}-clone`,
              description: worker.description,
              purpose: worker.purpose,
              runtimeRef: latestVersion?.runtimeRef,
              modelRef: latestVersion?.modelRef,
              role: pf.role,
              skills: pf.skills,
              behavior: pf.behavior,
              agentsMd: pf.agentsMd,
              permissions: latestVersion?.permissions,
              gatedTools: latestVersion?.gatedTools,
              budgetOverrides: latestVersion?.budgetOverrides,
              contextSources: latestVersion?.contextSources,
              concurrencyLimit: latestVersion?.concurrencyLimit ?? 0,
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
              {(selectedVersion ?? latestVersion)?.runtimeRef || "—"}
            </CardTitle>
          </CardHeader>
        </Card>
        <Card className="min-w-0">
          <CardHeader>
            <CardDescription>Model</CardDescription>
            <CardTitle className="break-all text-base font-mono text-sm">
              {(selectedVersion ?? latestVersion)?.modelRef || "—"}
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

      {/* Worker header editor (visible when a draft version exists) */}
      {isEditingEnabled && (
        <Card>
          <CardHeader>
            <CardTitle>Worker details</CardTitle>
            <CardDescription>
              Name, description, and purpose — saved on blur.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="editName">Name</Label>
              <Input
                id="editName"
                defaultValue={worker.name}
                onBlur={(e) => {
                  const val = e.target.value.trim();
                  if (val && val !== worker.name) {
                    updateWorker.mutate({ id: worker.id, name: val });
                  }
                }}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="editDesc">Description</Label>
              <Textarea
                id="editDesc"
                className="min-h-[60px] text-sm"
                defaultValue={worker.description}
                onBlur={(e) => {
                  const val = e.target.value.trim();
                  if (val !== worker.description) {
                    updateWorker.mutate({ id: worker.id, description: val });
                  }
                }}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="editPurpose">Purpose</Label>
              <Textarea
                id="editPurpose"
                className="min-h-[60px] text-sm"
                defaultValue={worker.purpose}
                onBlur={(e) => {
                  const val = e.target.value.trim();
                  if (val !== worker.purpose) {
                    updateWorker.mutate({ id: worker.id, purpose: val });
                  }
                }}
              />
            </div>
          </CardContent>
        </Card>
      )}

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
                  role: formData.role,
                  skills: formData.skills,
                  behavior: formData.behavior,
                  agentsMd: formData.agentsMd,
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
  const pf = promptFields(version);
  const role = pf.role;
  const skills = pf.skills;
  const behavior = pf.behavior;
  const agents = pf.agentsMd;
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
