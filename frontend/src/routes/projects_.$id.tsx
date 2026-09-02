import { createRoute, useNavigate } from "@tanstack/react-router";
import { useState, useEffect } from "react";
import { useForm } from "react-hook-form";
import { useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, ArrowUp, Folder } from "lucide-react";

import {
  useActivateProject,
  useArchiveProject,
  useCreateProject,
  useDeleteProject,
  useGetProject,
  useUpdateProject,
  projectKeys,
} from "@/api/projects";
import { useGetSettings } from "@/api/settings";
import { useListExecutions } from "@/api/executions";
import { useListDirPath, useUpdateProjectDir } from "@/api/projectFiles";
import { useStreamProjectEvents } from "@/api/projectEvents";
import {
  useGetProjectMCPServers,
  useSetProjectMCPServers,
} from "@/api/mcpServers";
import { EntityYamlView } from "@/components/EntityYamlView";
import { MCPPicker, type MCPConfig } from "@/components/MCPPicker";
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
import { cn } from "@/lib/utils";
import { FileBrowser } from "@/components/FileBrowser";
import { GitStrategySelect, type GitStrategy, protoToGitStrategy } from "@/components/GitStrategySelect";

import { Route as rootRoute } from "@/routes/__root";

// Project detail with inline editing. UseUpdateProject calls the existing
// UpdateProject RPC (partial update — only non-nil fields are written).
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/projects/$id",
  component: ProjectDetailPage,
});

function ProjectDetailPage() {
  const { id } = Route.useParams();
  const { data: project, isLoading, error } = useGetProject(id);
  const archiveProject = useArchiveProject();
  const deleteMutation = useDeleteProject();
  const updateProject = useUpdateProject();
  const updateProjectDir = useUpdateProjectDir();
  const activateProject = useActivateProject();
  const createProject = useCreateProject();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [editing, setEditing] = useState(false);
  const [viewMode, setViewMode] = useState<"detail" | "code">("detail");
  // True after the user saves a project directory, to remind them the
  // container must be restarted before the mount takes effect.
  const [dirChanged, setDirChanged] = useState(false);
  const [draftMaxRuns, setDraftMaxRuns] = useState("");
  const [draftGitStrategy, setDraftGitStrategy] = useState<GitStrategy>("local");
  const [savingGitStrategy, setSavingGitStrategy] = useState(false);
  const [savingMaxRuns, setSavingMaxRuns] = useState(false);
  // MCP server selection (references into Settings → Adapters → MCP).
  // Auto-refreshes on save via react-query invalidation (mcpKeys.project).
  const { data: projectMCPServers } = useGetProjectMCPServers(id);
  const setProjectMCPServers = useSetProjectMCPServers();
  const [mcpDraft, setMcpDraft] = useState<MCPConfig[]>([]);
  const [mcpDirty, setMcpDirty] = useState(false);
  const [savingMCP, setSavingMCP] = useState(false);
  useEffect(() => {
    setMcpDraft((projectMCPServers ?? []).map((srvId) => ({ id: srvId })));
    setMcpDirty(false);
  }, [projectMCPServers]);
  // Active executions (non-terminal) for the current-vs-limit meter.
  const { data: executions } = useListExecutions({ projectId: id, enabled: !!id });
  const { data: tenantSettings } = useGetSettings();
  const activeExecutions = (executions ?? []).filter((e) =>
    e.status === 1 || e.status === 2 || e.status === 3 || e.status === 4 || e.status === 5 || e.status === 6,
  ).length;

  useEffect(() => {
    setDraftMaxRuns(String(project?.maxConcurrentRuns ?? ""));
    const raw = (project as any)?.gitStrategy ?? (project as any)?.git_strategy ?? (() => {
      try {
        const g = JSON.parse((project as any)?.goals ?? "{}");
        return g.__git_strategy;
      } catch { return undefined; }
    })();
    const mapped = protoToGitStrategy(raw as any);
    if (mapped) setDraftGitStrategy(mapped);
    else setDraftGitStrategy("local");
  }, [project]);

  const { register, handleSubmit, reset } = useForm({
    defaultValues: { name: "", slug: "" },
    values: project ? { name: project.name, slug: project.slug } : undefined,
  });

  // Live event feed.
  const { events, status } = useStreamProjectEvents({
    projectId: id,
    onEvent: () => {
      qc.invalidateQueries({ queryKey: projectKeys.detail(id) });
    },
  });

  const handleArchive = async () => {
    await archiveProject.mutateAsync(id);
    navigate({ to: "/projects" });
  };

  const handleDelete = () => {
    if (
      window.confirm(
        "Permanently delete this project and all its workflows, work items, and data? This cannot be undone.",
      )
    ) {
      deleteMutation.mutate(id, {
        onSuccess: () => navigate({ to: "/projects" }),
      });
    }
  };

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading…</p>;
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        Failed to load project: {String(error)}
      </p>
    );
  }
  if (!project) {
    return null;
  }

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex min-w-0 items-center gap-2">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => navigate({ to: "/projects" })}
            className="shrink-0"
          >
            <ArrowLeft aria-hidden="true" className="h-4 w-4" />
            <span className="ml-1 hidden sm:inline">Back</span>
          </Button>
          <div className="min-w-0">
            {editing ? (
              <form
                onSubmit={handleSubmit((data) => {
                  updateProject.mutate(
                    { id, name: data.name, slug: data.slug },
                    { onSuccess: () => setEditing(false) },
                  );
                })}
                className="space-y-3"
              >
                <div className="space-y-2">
                  <Label htmlFor="name">Name</Label>
                  <Input id="name" {...register("name", { required: true })} />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="slug">Slug</Label>
                  <Input id="slug" {...register("slug")} />
                </div>
                <div className="flex gap-2">
                  <Button type="submit" disabled={updateProject.isPending}>
                    {updateProject.isPending ? "Saving…" : "Save"}
                  </Button>
                  <Button
                    type="button"
                    variant="outline"
                    onClick={() => {
                      reset();
                      setEditing(false);
                    }}
                  >
                    Cancel
                  </Button>
                </div>
              </form>
            ) : (
              <>
                <h1 className="text-lg font-semibold tracking-tight sm:text-2xl">
                  {project.name}
                </h1>
                <p className="truncate font-mono text-xs text-muted-foreground">
                  {project.slug}
                </p>
              </>
            )}
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {!editing && viewMode === "detail" && (
            <Button variant="outline" onClick={() => setEditing(true)}>
              Edit
            </Button>
          )}
          {project.status === 1 && viewMode === "detail" && (
            <Button
              onClick={() => activateProject.mutateAsync(id)}
              disabled={activateProject.isPending}
            >
              {activateProject.isPending ? "Activating…" : "Activate"}
            </Button>
          )}
          {project.status !== 4 && viewMode === "detail" && (
            <Button
              variant="outline"
              onClick={handleArchive}
              disabled={archiveProject.isPending}
            >
              {archiveProject.isPending ? "Archiving…" : "Archive"}
            </Button>
          )}
          <Button
            variant="destructive"
            onClick={handleDelete}
            disabled={deleteMutation.isPending}
          >
            {deleteMutation.isPending ? "Deleting…" : "Delete"}
          </Button>
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
            id: project.id,
            name: project.name,
            slug: project.slug,
            status: statusLabel(project.status),
            version: project.version,
            ...(project.goals && project.goals !== "{}"
              ? { goals: parseGoals(project.goals) }
              : {}),
            ...(project.projectDir ? { project_dir: project.projectDir } : {}),
            ...(project.contextFiles?.length
              ? { context_files: project.contextFiles }
              : {}),
            created_at: project.createdAt
              ? new Date(
                  Number(project.createdAt.seconds) * 1000,
                ).toISOString()
              : null,
            updated_at: project.updatedAt
              ? new Date(
                  Number(project.updatedAt.seconds) * 1000,
                ).toISOString()
              : null,
          }}
          title="Project YAML"
          onClone={async () => {
            const name = window.prompt(
              "Clone name:",
              `Clone of ${project.name}`,
            );
            if (!name) return;
            const result = await createProject.mutateAsync({ name });
            navigate({ to: `/projects/${result.id}` });
          }}
          cloneDisabled={createProject.isPending}
        />
      ) : (
        <>
      <div className="grid gap-4 md:grid-cols-3">
        <Card>
          <CardHeader>
            <CardDescription>Status</CardDescription>
            <CardTitle className="text-base capitalize">
              {statusLabel(project.status)}
            </CardTitle>
          </CardHeader>
        </Card>
        <Card>
          <CardHeader>
            <CardDescription>Version</CardDescription>
            <CardTitle className="text-base">{project.version}</CardTitle>
          </CardHeader>
        </Card>
        <Card>
          <CardHeader>
            <CardDescription>Project ID</CardDescription>
            <CardTitle className="font-mono text-xs break-all">
              {project.id}
            </CardTitle>
          </CardHeader>
        </Card>
      </div>

      {project.goals && project.goals !== "{}" && (
        <Card>
          <CardHeader>
            <CardTitle>Goals</CardTitle>
            <CardDescription>
              Key-value pairs describing the project's objectives.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <div className="divide-y rounded-md border">
              {parseGoals(project.goals).map(([key, value], i) => (
                <div key={i} className="flex gap-4 px-4 py-3 text-sm">
                  <span className="w-1/3 font-medium text-muted-foreground">
                    {key}
                  </span>
                  <span className="flex-1"><Markdown>{value}</Markdown></span>
                </div>
              ))}
            </div>
          </CardContent>
        </Card>
      )}

      {/* Main project directory */}
      {project && (
        <Card>
          <CardHeader>
            <CardTitle>Main Project Directory</CardTitle>
            <CardDescription>
              Root directory for worker operations. All file operations are
              scoped to this folder. Required — without it workers run in an
              empty temp directory.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            {project.projectDir ? (
              <div className="flex items-center gap-2 rounded-xl glass-panel px-3 py-2 font-mono text-xs border border-white/10">
                <Folder aria-hidden="true" className="h-3.5 w-3.5 shrink-0 text-amber-700 dark:text-amber-500" />
                <span className="flex-1 truncate">{project.projectDir}</span>
              </div>
            ) : (
              <p className="text-sm text-muted-foreground">No directory set.</p>
            )}
            {dirChanged && project.projectDir && (
              <div className="rounded-md border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-sm text-amber-700 dark:text-amber-400">
                Project directory saved. This directory is mounted into the
                Orchicon container from the host — <strong>restart the instance
                for the change to take effect</strong>:
                <code className="ml-1 rounded bg-muted px-1.5 py-0.5 font-mono text-xs">
                  scripts/container.sh up dev
                </code>
                {" "}(prod: <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">up prod</code>).
              </div>
            )}
            {editing && (
              <ProjectDirBrowser
                currentDir={project.projectDir || ""}
                onSelect={(path) =>
                  updateProjectDir.mutate(
                    { id: project.id, projectDir: path },
                    { onSuccess: () => setDirChanged(true) },
                  )
                }
                isSaving={updateProjectDir.isPending}
              />
            )}
          </CardContent>
        </Card>
      )}

      {/* Context files — browser within the project directory */}
      {project && (
        <FileBrowser
          projectId={project.id}
          projectDir={project.projectDir || ""}
          initialSelectedFiles={project.contextFiles || []}
          readOnly={!editing}
        />
      )}

      {/* Git strategy — how worktrees materialize */}
      {project && (
        <Card>
          <CardHeader>
            <CardTitle>Git strategy</CardTitle>
            <CardDescription>
              How worktrees materialize after success. Worktrees are always
              provisioned for isolation — this only controls branch handling.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            {editing ? (
              <>
                <GitStrategySelect value={draftGitStrategy} onValueChange={(v) => setDraftGitStrategy(v as GitStrategy)} />
                <Button
                  variant="outline"
                  disabled={savingGitStrategy}
                  onClick={() => {
                    setSavingGitStrategy(true);
                    (updateProject.mutate as any)(
                      { id: project.id, gitStrategy: draftGitStrategy, git_strategy: draftGitStrategy },
                      { onSettled: () => setSavingGitStrategy(false) },
                    );
                  }}
                >
                  {savingGitStrategy ? "Saving…" : "Save strategy"}
                </Button>
              </>
            ) : (
              <div className="space-y-2">
                <div className="flex items-center gap-2">
                  <span className="text-sm font-medium">Current strategy:</span>
                  <span className="rounded bg-muted px-2 py-1 font-mono text-xs">{draftGitStrategy}</span>
                </div>
                <p className="text-xs text-muted-foreground leading-relaxed">
                  {draftGitStrategy === "local" && "Push branch only — branch remains on origin for manual review. No PR."}
                  {draftGitStrategy === "pr" && "Push + PR — auto-creates a PR to remote (GitHub). Requires branch protection."}
                  {draftGitStrategy === "none" && "Ephemeral — no push. Work vanishes after success; only Results remain."}
                </p>
              </div>
            )}
          </CardContent>
        </Card>
      )}

      {/* MCP servers — reference-based selection into the tenant registry */}
      {project && (
        <Card>
          <CardHeader>
            <CardTitle>MCP servers</CardTitle>
            <CardDescription>
              MCP servers enabled for this project (references — editing an
              entry in Settings → Adapters → MCP updates every consumer).
              Selections here are the project defaults workers fall back to.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <MCPPicker
              value={mcpDraft}
              onChange={(configs) => {
                setMcpDraft(configs);
                setMcpDirty(true);
              }}
            />
            {editing && (
              <Button
                variant="outline"
                disabled={savingMCP || !mcpDirty}
                onClick={() => {
                  setSavingMCP(true);
                  setProjectMCPServers.mutate(
                    {
                      projectId: project.id,
                      mcpServerIds: mcpDraft.map((c) => c.id),
                    },
                    {
                      onSettled: () => setSavingMCP(false),
                      onSuccess: () => setMcpDirty(false),
                    },
                  );
                }}
              >
                {savingMCP ? "Saving…" : "Save MCP selection"}
              </Button>
            )}
            {!editing && (
              <p className="text-xs text-muted-foreground">
                {mcpDraft.length === 0
                  ? "No MCP servers selected — workers fall back to the tenant default."
                  : `${mcpDraft.length} MCP server(s) selected.`}
              </p>
            )}
          </CardContent>
        </Card>
      )}

      {/* Concurrency guard: per-project max-concurrent-runs + current vs limit */}
      {project && (
        <Card>
          <CardHeader>
            <CardTitle>Concurrency guard</CardTitle>
            <CardDescription>
              Caps how many executions may run concurrently for this project.
              The effective limit is <code>min(tenant, project)</code>; 0 on
              either side means no additional restriction. Items beyond the
              limit stay ready and dispatch when a slot frees. Non-repo
              projects (in-place execution) serialize unless this is set to{" "}
              {">"} 1 and the tenant permits it.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="flex flex-wrap items-end gap-3">
              <div className="w-48">
                <label className="mb-1 block text-sm font-medium">
                  Max concurrent runs
                </label>
                <Input
                  type="number"
                  min={0}
                  disabled={!editing}
                  value={draftMaxRuns}
                  onChange={(e) => setDraftMaxRuns(e.target.value)}
                  placeholder="0 = no additional restriction"
                />
              </div>
              {editing && (
                <Button
                  variant="outline"
                  disabled={savingMaxRuns}
                  onClick={() => {
                    setSavingMaxRuns(true);
                    updateProject.mutate(
                      { id: project.id, maxConcurrentRuns: parseInt(draftMaxRuns) || 0 },
                      { onSettled: () => setSavingMaxRuns(false) },
                    );
                  }}
                >
                  {savingMaxRuns ? "Saving…" : "Save limit"}
                </Button>
              )}
            </div>
            <div>
              <div className="mb-1 flex items-center justify-between text-sm">
                <span className="font-medium">Active executions</span>
                <span className="text-muted-foreground">
                  {activeExecutions} / {effectiveLimitLabel(project.maxConcurrentRuns ?? 0, tenantSettings?.maxConcurrentRuns ?? 0)}
                </span>
              </div>
              <div className="h-2 w-full overflow-hidden rounded-full bg-muted">
                <div
                  className={cn(
                    "h-full rounded-full transition-all",
                    atLimit(activeExecutions, project.maxConcurrentRuns ?? 0, tenantSettings?.maxConcurrentRuns ?? 0)
                      ? "bg-destructive"
                      : "bg-primary",
                  )}
                  style={{
                    width: `${meterWidth(activeExecutions, project.maxConcurrentRuns ?? 0, tenantSettings?.maxConcurrentRuns ?? 0)}%`,
                  }}
                />
              </div>
              <p className="mt-0.5 text-xs text-muted-foreground">
                Tenant cap: {tenantSettings?.maxConcurrentRuns ?? 0}
                {project.maxConcurrentRuns ? ` · Project cap: ${project.maxConcurrentRuns}` : " · Project cap: unset"}
              </p>
            </div>
          </CardContent>
        </Card>
      )}

      {/* Live event feed */}
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between">
            <div>
              <CardTitle>Live Events</CardTitle>
              <CardDescription>
                Real-time project lifecycle events (streamed via NATS).
              </CardDescription>
            </div>
            <StreamStatusBadge status={status} />
          </div>
        </CardHeader>
        <CardContent>
          {events.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No events yet. Create or update this project to see events
              stream live.
            </p>
          ) : (
            <div className="space-y-2">
              {events.map((resp, i) => {
                const evt = resp.event;
                if (!evt) return null;
                return (
                  <div
                    key={`${evt.eventId}-${i}`}
                    className="flex items-start gap-3 rounded-md border p-3 text-sm"
                  >
                    <span className="mt-0.5 text-xs font-mono text-muted-foreground">
                      {evt.occurredAt
                        ? new Date(
                            Number(evt.occurredAt.seconds) * 1000,
                          ).toLocaleTimeString()
                        : "--:--:--"}
                    </span>
                    <div className="flex-1">
                      <span className="font-medium">
                        {evt.eventType || "unknown"}
                      </span>
                      {evt.payload && evt.payload.length > 0 && (
                        <pre className="mt-1 overflow-auto rounded bg-muted p-2 text-xs text-muted-foreground">
                          {formatPayload(evt.payload)}
                        </pre>
                      )}
                    </div>
                    <span className="text-xs text-muted-foreground">
                      #{String(resp.sequence)}
                    </span>
                  </div>
                );
              })}
            </div>
          )}
        </CardContent>
      </Card>
        </>
      )}
    </div>
  );
}

function StreamStatusBadge({ status }: { status: string }) {
  const colors: Record<string, string> = {
    idle: "text-muted-foreground",
    connecting: "text-yellow-700 dark:text-yellow-600",
    open: "text-green-700 dark:text-green-600",
    reconnecting: "text-yellow-700 dark:text-yellow-600",
    closed: "text-muted-foreground",
    error: "text-destructive",
  };
  const labels: Record<string, string> = {
    idle: "idle",
    connecting: "connecting…",
    open: "live",
    reconnecting: "reconnecting…",
    closed: "disconnected",
    error: "error",
  };
  return (
    <span className={`text-xs font-medium ${colors[status] ?? ""}`}>
      ● {labels[status] ?? status}
    </span>
  );
}

function statusLabel(status: number): string {
  const labels: Record<number, string> = {
    1: "drafting",
    2: "active",
    3: "paused",
    4: "archived",
    5: "deleted",
  };
  return labels[status] ?? "unknown";
}

// effectiveLimit mirrors the server's min(tenant, project) formula where 0
// on either side means "no restriction from that side".
function effectiveLimit(projectLimit: number, tenantLimit: number): number {
  if (projectLimit === 0) return tenantLimit;
  if (tenantLimit === 0) return projectLimit;
  return Math.min(tenantLimit, projectLimit);
}

function effectiveLimitLabel(projectLimit: number, tenantLimit: number): string {
  const eff = effectiveLimit(projectLimit, tenantLimit);
  return eff === 0 ? "unlimited" : String(eff);
}

function atLimit(active: number, projectLimit: number, tenantLimit: number): boolean {
  const eff = effectiveLimit(projectLimit, tenantLimit);
  return eff > 0 && active >= eff;
}

function meterWidth(active: number, projectLimit: number, tenantLimit: number): number {
  const eff = effectiveLimit(projectLimit, tenantLimit);
  if (eff <= 0) {
    // No cap: show a sliver so the meter still renders, capped at 100%.
    return Math.min(100, active * 8);
  }
  return Math.min(100, (active / eff) * 100);
}

function parseGoals(s: string): [string, string][] {
  try {
    const m = JSON.parse(s);
    return Object.entries(m) as [string, string][];
  } catch {
    return [];
  }
}

function ProjectDirBrowser({ currentDir, onSelect, isSaving }: { currentDir: string; onSelect: (path: string) => void; isSaving: boolean }) {
  const [browsePath, setBrowsePath] = useState(currentDir || "~");
  const [showBrowser, setShowBrowser] = useState(false);
  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2 rounded-xl glass-panel px-3 py-2 font-mono text-xs border border-white/10">
        <Folder aria-hidden="true" className="h-3.5 w-3.5 shrink-0 text-amber-700 dark:text-amber-500" />
        <span className="flex-1 truncate">{currentDir || "No directory set"}</span>
        <Button variant="outline" size="sm" className="text-xs h-7 shrink-0" onClick={() => setShowBrowser(!showBrowser)}>
          {showBrowser ? "Cancel" : currentDir ? "Change" : "Set directory"}
        </Button>
      </div>
      {showBrowser && (
        <DirTree path={browsePath} onNavigate={setBrowsePath} onSelect={onSelect} isSaving={isSaving} />
      )}
    </div>
  );
}

function DirTree({ path, onNavigate, onSelect, isSaving }: { path: string; onNavigate: (p: string) => void; onSelect: (p: string) => void; isSaving: boolean }) {
  const { data, isLoading, error } = useListDirPath(path);
  const parentOf = (p: string) => {
    const parts = p.split("/").filter(Boolean);
    if (parts.length === 0) return "~";
    if (p.startsWith("~")) return parts.slice(1, -1).join("/") || "~";
    if (p.startsWith("/")) return "/" + parts.slice(0, -1).join("/") || "/";
    return parts.slice(0, -1).join("/") || "~";
  };
  if (isLoading) return <p className="text-xs text-muted-foreground py-2">Loading…</p>;
  if (error) return <p className="text-xs text-destructive py-2">Error: {String(error)}</p>;
  const dirs = (data?.entries ?? []).filter((e: any) => e.isDir);
  return (
    <div className="rounded-md border max-h-[300px] overflow-y-auto">
      <div className="flex items-center gap-2 px-3 py-2 text-sm hover:bg-muted/40 cursor-pointer border-b" onClick={() => onNavigate(parentOf(path))}>
        <ArrowUp aria-hidden="true" className="h-4 w-4 text-muted-foreground" />
        <span className="text-muted-foreground text-xs">..</span>
      </div>
      {dirs.length === 0 && <p className="px-3 py-4 text-sm text-muted-foreground">Empty directory</p>}
      {dirs.map((entry: any) => (
        <div key={entry.path} className="flex items-center gap-2 px-3 py-2 text-sm hover:bg-muted/40 cursor-pointer border-b last:border-0">
          <Folder aria-hidden="true" className="h-4 w-4 text-amber-700 dark:text-amber-500 shrink-0" />
          <span className="flex-1 truncate" onClick={() => onNavigate(entry.path)}>{entry.name}/</span>
          <Button variant="outline" size="sm" className="text-xs h-7 shrink-0" onClick={() => onSelect(entry.path)} disabled={isSaving}>
            {isSaving ? "Saving…" : "Select"}
          </Button>
        </div>
      ))}
    </div>
  );
}

function formatPayload(data: Uint8Array): string {
  try {
    return JSON.stringify(JSON.parse(new TextDecoder().decode(data)), null, 2);
  } catch {
    return `${data.length} bytes`;
  }
}
