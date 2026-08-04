import { createRoute } from "@tanstack/react-router";
import { useState, useEffect, useCallback } from "react";
import { Sun, Moon, Check, Save, BookOpen, Palette, SlidersHorizontal, Database, Download, RotateCcw, Folder, ArrowUp, Loader2, Trash2 } from "lucide-react";

import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";
import { LIGHT_THEMES, DARK_THEMES } from "@/lib/themes";
import { useThemeStore } from "@/lib/theme-store";
import { useGetSettings, useUpdateSettings, useGetBackups, useCreateBackup, useRestoreBackup, useDeleteBackup } from "@/api/settings";
import { useListDirPath } from "@/api/projectFiles";
import { ModelPicker } from "@/components/ModelPicker";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/settings",
  component: SettingsPage,
});

type SettingsTab = "appearance" | "defaults" | "backups" | "guide";

function SettingsPage() {
  const [tab, setTab] = useState<SettingsTab>("appearance");

  return (
    <div className="mx-auto max-w-4xl space-y-8">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Settings</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          Configure tenant-level defaults and preferences.
        </p>
      </div>

      <div className="flex flex-wrap gap-2 border-b pb-px">
        {([
          ["appearance", "Appearance", Palette],
          ["defaults", "Defaults", SlidersHorizontal],
          ["backups", "Backups", Database],
          ["guide", "User Guide", BookOpen],
        ] as const).map(([id, label, Icon]) => (
          <button
            key={id}
            onClick={() => setTab(id as SettingsTab)}
            className={cn(
              "flex items-center gap-2 rounded-t-md px-3 py-2 text-sm font-medium transition-colors",
              tab === id
                ? "border-b-2 border-primary text-foreground"
                : "text-muted-foreground hover:text-foreground",
            )}
          >
            <Icon className="h-4 w-4" />
            {label}
          </button>
        ))}
      </div>

      {tab === "appearance" && <AppearanceTab />}
      {tab === "defaults" && <DefaultsTab />}
      {tab === "backups" && <BackupsTab />}
      {tab === "guide" && <UserGuideTab />}
    </div>
  );
}

const presetCrons = new Set(["", "0 * * * *", "0 */6 * * *", "0 3 * * *", "0 3 * * 0"]);

function describeCron(cron: string): string {
  const map: Record<string, string> = {
    "0 * * * *": "every hour",
    "0 */6 * * *": "every 6 hours",
    "0 3 * * *": "daily at 3:00 AM",
    "0 3 * * 0": "weekly on Sunday at 3:00 AM",
  };
  return map[cron] || cron;
}

function BackupsTab() {
  const { data: settings, isLoading: loadingSettings } = useGetSettings();
  const updateSettings = useUpdateSettings();
  const { data: backups, isLoading: loadingBackups, refetch: refetchBackups } = useGetBackups();
  const createBackup = useCreateBackup();
  const restoreBackup = useRestoreBackup();
  const deleteBackup = useDeleteBackup();

  const [draftSchedule, setDraftSchedule] = useState("");
  const [draftRetention, setDraftRetention] = useState("");
  const [draftDirectory, setDraftDirectory] = useState("");
  const [showBackupDirPicker, setShowBackupDirPicker] = useState(false);
  const [backupBrowsePath, setBackupBrowsePath] = useState("");
  const [saving, setSaving] = useState(false);
  const [restoring, setRestoring] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);
  const [message, setMessage] = useState<string | null>(null);

  useEffect(() => {
    if (settings) {
      setDraftSchedule(settings.backupSchedule ?? "");
      setDraftRetention(String(settings.backupRetentionDays ?? ""));
      setDraftDirectory(settings.backupDirectory ?? "");
    }
  }, [settings]);

  const handleSave = useCallback(async () => {
    setSaving(true);
    setMessage(null);
    try {
      await updateSettings.mutateAsync({
        backupSchedule: draftSchedule,
        backupRetentionDays: parseInt(draftRetention) || 0,
        backupDirectory: draftDirectory,
      } as any);
      setMessage("Backup settings saved.");
    } catch (e: any) {
      setMessage(`Error: ${e.message}`);
    } finally {
      setSaving(false);
    }
  }, [draftSchedule, draftRetention, draftDirectory, updateSettings]);

  const handleCreateBackup = useCallback(async () => {
    setMessage(null);
    try {
      const info = await createBackup.mutateAsync();
      const sizeMB = Number(info.sizeBytes) / 1024 / 1024;
      setMessage(`Backup created: ${info.name} (${sizeMB.toFixed(1)} MB)`);
      refetchBackups();
    } catch (e: any) {
      setMessage(`Error: ${e.message}`);
    }
  }, [createBackup, refetchBackups]);

  const handleRestore = useCallback(async (name: string) => {
    if (!window.confirm(`Restore from "${name}"? This will replace the current database.`)) return;
    setRestoring(name);
    setMessage(null);
    try {
      await restoreBackup.mutateAsync({ name });
      setMessage(`Restored from "${name}". Refreshing...`);
      setTimeout(() => window.location.reload(), 2000);
    } catch (e: any) {
      setMessage(`Error: ${e.message}`);
      setRestoring(null);
    }
  }, [restoreBackup]);

  const handleDelete = useCallback(async (name: string) => {
    if (!window.confirm(`Delete backup "${name}"? This cannot be undone.`)) return;
    setDeleting(name);
    setMessage(null);
    try {
      await deleteBackup.mutateAsync({ name });
      setMessage(`Deleted "${name}".`);
    } catch (e: any) {
      setMessage(`Error: ${e.message}`);
    } finally {
      setDeleting(null);
    }
  }, [deleteBackup]);

  return (
    <div className="space-y-8">
      {loadingSettings && <p className="text-sm text-muted-foreground">Loading settings…</p>}

      {!loadingSettings && (
        <Card>
          <CardHeader>
            <CardTitle>Backup configuration</CardTitle>
            <CardDescription>
              Schedule automatic database snapshots and set retention. All backups
              within the retention period are kept — restoring an older backup does
              not delete newer snapshots.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid gap-4 sm:grid-cols-2">
              <div>
                <label className="mb-1 block text-sm font-medium">
                  Backup schedule
                </label>
                <div className="flex gap-1.5 mb-2 whitespace-nowrap">
                  {[
                    { label: "Off", cron: "" },
                    { label: "Hourly", cron: "0 * * * *" },
                    { label: "Every 6h", cron: "0 */6 * * *" },
                    { label: "Daily 3am", cron: "0 3 * * *" },
                    { label: "Weekly Sun", cron: "0 3 * * 0" },
                    { label: "Custom", cron: "__custom__" },
                  ].map((p) => (
                    <button
                      key={p.label}
                      type="button"
                      onClick={() => {
                        if (p.cron === "__custom__") {
                          setDraftSchedule(draftSchedule || "0 3 * * *");
                        } else {
                          setDraftSchedule(p.cron);
                        }
                      }}
                      className={`rounded-md border px-2.5 py-1 text-xs font-medium transition-colors shrink-0 ${
                        p.cron === "__custom__"
                          ? !presetCrons.has(draftSchedule)
                            ? "border-primary bg-primary/10 text-primary"
                            : "border-border text-muted-foreground hover:border-muted-foreground/50"
                          : draftSchedule === p.cron
                            ? "border-primary bg-primary/10 text-primary"
                            : "border-border text-muted-foreground hover:border-muted-foreground/50"
                      }`}
                    >
                      {p.label}
                    </button>
                  ))}
                </div>
                {draftSchedule !== "" && (
                  <Input
                    value={draftSchedule}
                    onChange={(e) => setDraftSchedule(e.target.value)}
                    placeholder="0 3 * * *"
                  />
                )}
                <p className="mt-0.5 text-xs text-muted-foreground">
                  {draftSchedule ? `Next run: ${describeCron(draftSchedule)}` : "Automatic backups disabled."}
                </p>
              </div>
              <div>
                <label className="mb-1 block text-sm font-medium">
                  Retention (days)
                </label>
                <Input
                  type="number"
                  value={draftRetention}
                  onChange={(e) => setDraftRetention(e.target.value)}
                  placeholder="30"
                />
                <p className="mt-0.5 text-xs text-muted-foreground">
                  0 = keep backups forever.
                </p>
              </div>
            </div>
            <div>
              <label className="mb-1 block text-sm font-medium">
                Backup directory
              </label>
              <div className="flex gap-2">
                <Input
                  id="backup-dir-input"
                  value={draftDirectory}
                  onChange={(e) => setDraftDirectory(e.target.value)}
                  placeholder="~/.local/share/orchicon/backups/"
                  className="flex-1"
                />
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => {
                    setShowBackupDirPicker(!showBackupDirPicker);
                    setBackupBrowsePath(draftDirectory || "~");
                  }}
                >
                  {showBackupDirPicker ? "Cancel" : "Browse"}
                </Button>
              </div>
              <p className="mt-0.5 text-xs text-muted-foreground">
                Where snapshots are stored. Empty = default location.
              </p>
            </div>

            {showBackupDirPicker && (
              <BackupDirBrowser
                path={backupBrowsePath}
                onSelect={(p) => {
                  setDraftDirectory(p);
                  setShowBackupDirPicker(false);
                }}
                onNavigate={setBackupBrowsePath}
              />
            )}
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle>Snapshots</CardTitle>
          <CardDescription>
            Create an instant backup or restore from a previous snapshot.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center gap-3">
            <Button onClick={handleCreateBackup} disabled={createBackup.isPending}>
              <Download className="mr-2 h-4 w-4" />
              {createBackup.isPending ? "Creating…" : "Create backup now"}
            </Button>
          </div>

          {loadingBackups && <p className="text-sm text-muted-foreground">Loading backups…</p>}

          {!loadingBackups && backups && backups.length > 0 && (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b text-left text-muted-foreground">
                    <th className="pb-2 pr-4 font-medium">Name</th>
                    <th className="pb-2 pr-4 font-medium">Size</th>
                    <th className="pb-2 pr-4 font-medium">Created</th>
                    <th className="pb-2 font-medium">Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {backups.map((b) => {
                    const sizeMB = (Number(b.sizeBytes) / 1024 / 1024).toFixed(1);
                    const date = new Date(b.createdAt).toLocaleString();
                    return (
                      <tr key={b.name} className="border-b last:border-0 hover:bg-muted/50">
                        <td className="py-2 pr-4 font-mono text-xs">{b.name}</td>
                        <td className="py-2 pr-4">{sizeMB} MB</td>
                        <td className="py-2 pr-4">{date}</td>
                        <td className="py-2">
                          <div className="flex items-center gap-2">
                            <Button
                              variant="outline"
                              size="sm"
                              onClick={() => handleRestore(b.name)}
                              disabled={restoring === b.name || deleting === b.name}
                            >
                              <RotateCcw className="mr-1 h-3 w-3" />
                              {restoring === b.name ? "Restoring…" : "Restore"}
                            </Button>
                            <Button
                              variant="outline"
                              size="sm"
                              onClick={() => handleDelete(b.name)}
                              disabled={deleting === b.name || restoring === b.name}
                              className="text-destructive hover:text-destructive"
                            >
                              <Trash2 className="mr-1 h-3 w-3" />
                              {deleting === b.name ? "Deleting…" : "Delete"}
                            </Button>
                          </div>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}

          {!loadingBackups && (!backups || backups.length === 0) && (
            <p className="text-sm text-muted-foreground">No backups yet.</p>
          )}
        </CardContent>
      </Card>

      {message && (
        <div className="rounded-md border bg-muted px-4 py-2 text-sm">{message}</div>
      )}

      {!loadingSettings && (
        <div className="flex justify-end">
          <Button onClick={handleSave} disabled={saving || loadingSettings}>
            <Save className="mr-2 h-4 w-4" />
            {saving ? "Saving…" : "Save backup settings"}
          </Button>
        </div>
      )}
    </div>
  );
}

function AppearanceTab() {
  const currentTheme = useThemeStore((s) => s.theme);
  const currentMode = useThemeStore((s) => s.mode);
  const setTheme = useThemeStore((s) => s.setTheme);
  const setMode = useThemeStore((s) => s.setMode);

  return (
    <div className="space-y-8">
      <section>
        <h2 className="mb-3 text-sm font-medium text-muted-foreground">APPEARANCE</h2>
        <Card>
          <CardContent className="p-4">
            <div className="flex items-center gap-4">
              <button
                onClick={() => setMode("light")}
                className={cn(
                  "flex flex-1 items-center justify-center gap-2 rounded-lg border-2 px-4 py-3 text-sm font-medium transition-all",
                  currentMode === "light"
                    ? "border-primary bg-primary/5 text-foreground"
                    : "border-border text-muted-foreground hover:border-muted-foreground/50",
                )}
              >
                <Sun className="h-4 w-4" />
                Light
              </button>
              <button
                onClick={() => setMode("dark")}
                className={cn(
                  "flex flex-1 items-center justify-center gap-2 rounded-lg border-2 px-4 py-3 text-sm font-medium transition-all",
                  currentMode === "dark"
                    ? "border-primary bg-primary/5 text-foreground"
                    : "border-border text-muted-foreground hover:border-muted-foreground/50",
                )}
              >
                <Moon className="h-4 w-4" />
                Dark
              </button>
            </div>
          </CardContent>
        </Card>
      </section>

      <section>
        <h2 className="mb-3 flex items-center gap-2 text-sm font-medium text-muted-foreground">
          <Sun className="h-4 w-4" />
          LIGHT THEMES
        </h2>
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
          {LIGHT_THEMES.map((theme) => (
            <ThemeCard
              key={theme.id}
              theme={theme}
              active={currentTheme === theme.id && currentMode === "light"}
              onClick={() => {
                setTheme(theme.id);
                setMode("light");
              }}
            />
          ))}
        </div>
      </section>

      <section>
        <h2 className="mb-3 flex items-center gap-2 text-sm font-medium text-muted-foreground">
          <Moon className="h-4 w-4" />
          DARK THEMES
        </h2>
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
          {DARK_THEMES.map((theme) => (
            <ThemeCard
              key={theme.id}
              theme={theme}
              active={currentTheme === theme.id && currentMode === "dark"}
              onClick={() => {
                setTheme(theme.id);
                setMode("dark");
              }}
            />
          ))}
        </div>
      </section>
    </div>
  );
}

function DefaultsTab() {
  const { data: settings, isLoading } = useGetSettings();
  const updateSettings = useUpdateSettings();

  const [draftWorkerModel, setDraftWorkerModel] = useState("");
  const [draftAskOrchiconModel, setDraftAskOrchiconModel] = useState("");
  const [draftNoProgress, setDraftNoProgress] = useState("");
  const [draftNoFileDiff, setDraftNoFileDiff] = useState("");
  const [draftTextLoop, setDraftTextLoop] = useState("");
  const [draftRepetitionCount, setDraftRepetitionCount] = useState("");
  const [draftRepetitionWindow, setDraftRepetitionWindow] = useState("");
  const [draftWallClockSeconds, setDraftWallClockSeconds] = useState("");
  const [draftReapGrace, setDraftReapGrace] = useState("");
  const [draftReapFailures, setDraftReapFailures] = useState("");
  const [draftReconnectAttempts, setDraftReconnectAttempts] = useState("");
  const [draftReconnectGrace, setDraftReconnectGrace] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (settings) {
      setDraftWorkerModel(settings.defaultWorkerModel ?? "");
      setDraftAskOrchiconModel(settings.defaultAskOrchiconModel ?? "");
      setDraftNoProgress(String(settings.stallNoProgressWindowSeconds ?? ""));
      setDraftNoFileDiff(String(settings.stallNoFileDiffWindowSeconds ?? ""));
      setDraftTextLoop(String(settings.stallTextLoopWindowSeconds ?? ""));
      setDraftRepetitionCount(String(settings.stallRepetitionCount ?? ""));
      setDraftRepetitionWindow(String(settings.stallRepetitionWindowSeconds ?? ""));
      setDraftWallClockSeconds(String(settings.stallWallClockSeconds ?? ""));
      setDraftReapGrace(String(settings.executionReapGraceSeconds ?? ""));
      setDraftReapFailures(String(settings.executionReapConsecutiveFailures ?? ""));
      setDraftReconnectAttempts(String(settings.executionReconnectAttempts ?? ""));
      setDraftReconnectGrace(String(settings.executionReconnectGraceSeconds ?? ""));
    }
  }, [settings]);

  async function handleSave() {
    setSaving(true);
    try {
      await updateSettings.mutateAsync({
        defaultWorkerModel: draftWorkerModel,
        defaultAskOrchiconModel: draftAskOrchiconModel,
        stallNoProgressWindowSeconds: parseInt(draftNoProgress) || 0,
        stallNoFileDiffWindowSeconds: parseInt(draftNoFileDiff) || 0,
        stallTextLoopWindowSeconds: parseInt(draftTextLoop) || 0,
        stallRepetitionCount: parseInt(draftRepetitionCount) || 0,
        stallRepetitionWindowSeconds: parseInt(draftRepetitionWindow) || 0,
        stallWallClockSeconds: parseInt(draftWallClockSeconds) || 0,
        executionReapGraceSeconds: parseInt(draftReapGrace) || 0,
        executionReapConsecutiveFailures: parseInt(draftReapFailures) || 0,
        executionReconnectAttempts: parseInt(draftReconnectAttempts) || 0,
        executionReconnectGraceSeconds: parseInt(draftReconnectGrace) || 0,
      } as any);
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="space-y-8">
      {isLoading && <p className="text-sm text-muted-foreground">Loading settings…</p>}

      {!isLoading && (
        <Card>
          <CardHeader>
            <CardTitle>Default models</CardTitle>
            <CardDescription>
              When a worker does not specify a model, or when a feature has no model
              configured, these defaults are used. If both are empty, dispatch will fail.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div>
              <label className="mb-1 block text-sm font-medium">
                Default worker model
              </label>
              <ModelPicker
                value={draftWorkerModel}
                onChange={setDraftWorkerModel}
              />
            </div>
            <div>
              <label className="mb-1 block text-sm font-medium">
                Default Ask Orchicon model
              </label>
              <ModelPicker
                value={draftAskOrchiconModel}
                onChange={setDraftAskOrchiconModel}
              />
            </div>
          </CardContent>
        </Card>
      )}

      {!isLoading && (
        <Card>
          <CardHeader>
            <CardTitle>Recovery stall parameters</CardTitle>
            <CardDescription>
              Per-execution stall detection thresholds. Zero means "use the env-var
              default or sensible built-in default". These are read at dispatch time
              with env-var fallback for dev/debugging overrides.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid gap-4 sm:grid-cols-2">
              <StallField
                label="No progress window (seconds)"
                description="No step_finish or new tokens within this window"
                value={draftNoProgress}
                onChange={setDraftNoProgress}
                placeholder="300"
              />
              <StallField
                label="No file diff window (seconds)"
                description="No file modifications within this window. 0 = disabled"
                value={draftNoFileDiff}
                onChange={setDraftNoFileDiff}
                placeholder="900"
              />
              <StallField
                label="Text loop window (seconds)"
                description="No meaningful action within this window. 0 = disabled"
                value={draftTextLoop}
                onChange={setDraftTextLoop}
                placeholder="600"
              />
              <StallField
                label="Repetition count"
                description="Same tool-call signature repeated this many times"
                value={draftRepetitionCount}
                onChange={setDraftRepetitionCount}
                placeholder="5"
              />
              <StallField
                label="Repetition window (seconds)"
                description="Window for repetition detection"
                value={draftRepetitionWindow}
                onChange={setDraftRepetitionWindow}
                placeholder="300"
              />
              <StallField
                label="Wall clock timeout (seconds)"
                description="Hard per-execution timeout. 0 = disabled. Default 3600 (1 hour)."
                value={draftWallClockSeconds}
                onChange={setDraftWallClockSeconds}
                placeholder="3600"
              />
            </div>
          </CardContent>
        </Card>
      )}

      {!isLoading && (
        <Card>
          <CardHeader>
            <CardTitle>Execution liveness reaper</CardTitle>
            <CardDescription>
              Fails executions whose runtime process is gone (plane restart, lost
              runtime container). The liveness probe can false-negative on a transient
              docker/socket hiccup, so the reaper only acts once an execution is old
              enough AND has been reported not-alive several times in a row. Zero means
              "use the env var or built-in default".
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid gap-4 sm:grid-cols-2">
              <StallField
                label="Grace window (seconds)"
                description="Min age before an execution is eligible for reaping"
                value={draftReapGrace}
                onChange={setDraftReapGrace}
                placeholder="60"
              />
              <StallField
                label="Consecutive not-alive probes"
                description="Not-alive checks in a row before the execution is reaped"
                value={draftReapFailures}
                onChange={setDraftReapFailures}
                placeholder="3"
              />
              <StallField
                label="Reconnect attempts"
                description="Client retries of a broken exec stream before giving up"
                value={draftReconnectAttempts}
                onChange={setDraftReconnectAttempts}
                placeholder="3"
              />
              <StallField
                label="Reconnect grace (seconds)"
                description="How long the supervisor keeps an orphaned child before killing it"
                value={draftReconnectGrace}
                onChange={setDraftReconnectGrace}
                placeholder="60"
              />
            </div>
          </CardContent>
        </Card>
      )}

      {!isLoading && (
        <div className="flex justify-end">
          <Button onClick={handleSave} disabled={saving}>
            <Save className="mr-2 h-4 w-4" />
            {saving ? "Saving…" : "Save settings"}
          </Button>
        </div>
      )}
    </div>
  );
}

function UserGuideTab() {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <BookOpen className="h-5 w-5" />
          Orchicon User Guide
        </CardTitle>
        <CardDescription>
          Quick-reference guide to the control plane. See DOCUMENTATION.md for
          the full reference.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-6 text-sm leading-relaxed">
        <div>
          <h3 className="font-medium mb-1">Projects</h3>
          <p className="text-muted-foreground">
            Top-level organizational unit. Each project has a directory, goals,
            and context files that workers use as reference.
          </p>
        </div>
        <div>
          <h3 className="font-medium mb-1">Workers</h3>
          <p className="text-muted-foreground">
            Reusable execution profiles with Role, Skills, Behavior, and AGENTS.md
            fields. Workers are versioned; published versions are immutable.
          </p>
        </div>
        <div>
          <h3 className="font-medium mb-1">Work Items</h3>
          <p className="text-muted-foreground">
            Units of work dispatched to workers. Forms a DAG (Epic → Feature →
            Task → Subtask). Dependencies enforce ordering.
          </p>
        </div>
        <div>
          <h3 className="font-medium mb-1">Workflows</h3>
          <p className="text-muted-foreground">
            DAG of steps (Task, Approval, Loop Decision, Work Item, Project) that
            orchestrates autonomous work. Templates can be run multiple times.
          </p>
        </div>
        <div>
          <h3 className="font-medium mb-1">Recovery</h3>
          <p className="text-muted-foreground">
            When a worker execution fails, recovery runs automatically through a
            6-step workflow (capture → summarize → preserve → review → plan →
            resume). L1→L2→L3 escalation on repeated failures.
          </p>
        </div>
        <div>
          <h3 className="font-medium mb-1">Policies</h3>
          <p className="text-muted-foreground">
            Rego-based policy engine evaluated at decision points (admission,
            dispatch, budget, approval, recovery).
          </p>
        </div>
        <div>
          <h3 className="font-medium mb-1">Telemetry & Cost</h3>
          <p className="text-muted-foreground">
            OpenTelemetry pipeline exporting to the Grafana stack (Tempo /
            Loki / VictoriaMetrics). Cost Explorer shows spend by Project, Task, Execution, Model, or Workflow.
          </p>
        </div>
        <div>
          <h3 className="font-medium mb-1">Settings</h3>
          <p className="text-muted-foreground">
            Tenant-level defaults for models and recovery stall parameters.
            Appearance controls light/dark mode and theme selection.
          </p>
        </div>
      </CardContent>
    </Card>
  );
}

function StallField({
  label,
  description,
  value,
  onChange,
  placeholder,
}: {
  label: string;
  description: string;
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
}) {
  return (
    <div>
      <label className="mb-1 block text-sm font-medium">{label}</label>
      <Input
        type="number"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
      />
      <p className="mt-0.5 text-xs text-muted-foreground">{description}</p>
    </div>
  );
}

function ThemeCard({
  theme,
  active,
  onClick,
}: {
  theme: { id: string; name: string; swatches: string[] };
  active: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "group relative overflow-hidden rounded-xl border-2 p-3 text-left transition-all hover:shadow-md",
        active
          ? "border-primary ring-1 ring-primary"
          : "border-border hover:border-muted-foreground/40",
      )}
    >
      <div className="mb-2 flex gap-1">
        {theme.swatches.map((color, i) => (
          <div
            key={i}
            className="h-6 flex-1 rounded-md"
            style={{ backgroundColor: color }}
          />
        ))}
      </div>
      <div className="flex items-center justify-between">
        <span className="text-sm font-medium">{theme.name}</span>
        {active && (
          <span className="flex h-5 w-5 items-center justify-center rounded-full bg-primary text-primary-foreground">
            <Check className="h-3 w-3" />
          </span>
        )}
      </div>
    </button>
  );
}

// ─── Backup directory browser (server-side filesystem tree) ───────

interface BackupDirBrowserProps {
  path: string;
  onSelect: (path: string) => void;
  onNavigate: (path: string) => void;
}

function BackupDirBrowser({ path, onSelect, onNavigate }: BackupDirBrowserProps) {
  const { data, isLoading, error } = useListDirPath(path);

  const dirs = (data?.entries ?? []).filter((e) => e.isDir);
  const files = (data?.entries ?? []).filter((e) => !e.isDir);

  const parentOf = (p: string) => {
    const parts = p.split("/").filter(Boolean);
    if (parts.length === 0) return "~";
    if (p.startsWith("~")) {
      const rel = parts.slice(1, -1).join("/");
      return rel ? `~/${rel}` : "~";
    }
    if (p.startsWith("/")) {
      const parent = "/" + parts.slice(0, -1).join("/");
      return parent === "" ? "/" : parent;
    }
    return parts.slice(0, -1).join("/") || "~";
  };

  return (
    <div className="rounded-md border">
      <div
        className="flex items-center gap-2 border-b px-3 py-2 text-sm hover:bg-muted/40 cursor-pointer"
        onClick={() => onNavigate(parentOf(path))}
      >
        <ArrowUp className="h-4 w-4 text-muted-foreground" />
        <span className="truncate text-xs text-muted-foreground">..</span>
        <span className="ml-auto truncate font-mono text-xs text-muted-foreground">{path}</span>
      </div>

      {isLoading && (
        <div className="flex items-center gap-2 px-3 py-4 text-sm text-muted-foreground">
          <Loader2 className="h-4 w-4 animate-spin" />
          Loading…
        </div>
      )}

      {error && (
        <p className="px-3 py-4 text-sm text-destructive">Error: {String(error)}</p>
      )}

      {!isLoading && !error && dirs.length === 0 && files.length === 0 && (
        <p className="px-3 py-4 text-sm text-muted-foreground">Empty directory</p>
      )}

      {dirs.map((entry) => (
        <div
          key={entry.path}
          className="flex items-center gap-2 border-b px-3 py-2 text-sm hover:bg-muted/40 cursor-pointer last:border-0"
        >
          <Folder className="h-4 w-4 text-amber-500 shrink-0" />
          <span
            className="flex-1 truncate"
            onClick={() => onNavigate(entry.path)}
          >
            {entry.name}/
          </span>
          <Button
            variant="outline"
            size="sm"
            className="text-xs h-7 shrink-0"
            onClick={() => onSelect(entry.path)}
          >
            Use this folder
          </Button>
        </div>
      ))}

      {files.length > 0 && (
        <p className="px-3 py-1 text-xs text-muted-foreground">
          {files.length} file{files.length !== 1 ? "s" : ""} (select a folder)
        </p>
      )}
    </div>
  );
}
