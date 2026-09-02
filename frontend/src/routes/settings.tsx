import * as React from "react";
import { createRoute } from "@tanstack/react-router";
import { useState, useEffect, useCallback } from "react";
import { Sun, Moon, Check, Save, BookOpen, Palette, SlidersHorizontal, Database, Download, RotateCcw, Folder, ArrowUp, Loader2, Trash2, Clock, Plug, Cable } from "lucide-react";
import { ProvidersTab } from "@/components/ProvidersTab";
import { MCPServersTab } from "@/components/MCPServersTab";

import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";
import { LIGHT_THEMES, DARK_THEMES } from "@/lib/themes";
import { useThemeStore } from "@/lib/theme-store";
import { emptyWarnings, parseBudgetDefaults, buildBudgetDefaults, defaultCompactTiers, type BudgetWarnings, type CompactTiers } from "@/lib/budget-defaults";
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

type SettingsTab = "appearance" | "defaults" | "session" | "backups" | "secrets" | "providers" | "mcp" | "guide";

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

      <div className="flex flex-wrap gap-2 border-b border-white/10 pb-px">
        {([
          ["appearance", "Appearance", Palette],
          ["defaults", "Defaults", SlidersHorizontal],
          ["session", "Session", Clock],
          ["backups", "Backups", Database],
          ["secrets", "Secrets", Database],
          ["providers", "Providers", Plug],
          ["mcp", "MCP", Cable],
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
      {tab === "session" && <SessionTab />}
      {tab === "backups" && <BackupsTab />}
      {tab === "secrets" && <SecretsTab />}
      {tab === "providers" && <ProvidersTab />}
      {tab === "mcp" && <MCPServersTab />}
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
              <Download aria-hidden="true" className="mr-2 h-4 w-4" />
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
                              <RotateCcw aria-hidden="true" className="mr-1 h-3 w-3" />
                              {restoring === b.name ? "Restoring…" : "Restore"}
                            </Button>
                            <Button
                              variant="outline"
                              size="sm"
                              onClick={() => handleDelete(b.name)}
                              disabled={deleting === b.name || restoring === b.name}
                              className="text-destructive hover:text-destructive"
                            >
                              <Trash2 aria-hidden="true" className="mr-1 h-3 w-3" />
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
        <div className="rounded-2xl glass-panel px-4 py-2 text-sm border border-white/10">{message}</div>
      )}

      {!loadingSettings && (
        <div className="flex justify-end">
          <Button onClick={handleSave} disabled={saving || loadingSettings}>
            <Save aria-hidden="true" className="mr-2 h-4 w-4" />
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
                <Sun aria-hidden="true" className="h-4 w-4" />
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
                <Moon aria-hidden="true" className="h-4 w-4" />
                Dark
              </button>
            </div>
          </CardContent>
        </Card>
      </section>

      <section>
        <h2 className="mb-3 flex items-center gap-2 text-sm font-medium text-muted-foreground">
          <Sun aria-hidden="true" className="h-4 w-4" />
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
          <Moon aria-hidden="true" className="h-4 w-4" />
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
  const [draftNudgeMax, setDraftNudgeMax] = useState("");
  const [draftNudgeReplyWindow, setDraftNudgeReplyWindow] = useState("");
  const [draftNudgeCooldown, setDraftNudgeCooldown] = useState("");
  const [draftToolHang, setDraftToolHang] = useState("");
  const [draftBudgetTokens, setDraftBudgetTokens] = useState("");
  const [draftBudgetCost, setDraftBudgetCost] = useState("");
  const [draftBudgetWallClock, setDraftBudgetWallClock] = useState("");
  const [draftBudgetToolCalls, setDraftBudgetToolCalls] = useState("");
  const [draftCompactMaxTurns, setDraftCompactMaxTurns] = useState("");
  const [draftCompactTiers, setDraftCompactTiers] = useState<CompactTiers>(defaultCompactTiers);
  const [draftWarn, setDraftWarn] = useState<BudgetWarnings>(emptyWarnings);
  const [draftReapGrace, setDraftReapGrace] = useState("");
  const [draftReapFailures, setDraftReapFailures] = useState("");
  const [draftLogDirectory, setDraftLogDirectory] = useState("");
  const [draftLogMaxSize, setDraftLogMaxSize] = useState("");
  const [draftLogRollInterval, setDraftLogRollInterval] = useState("");
  const [draftLogRetention, setDraftLogRetention] = useState("");
  const [draftLogMaxFiles, setDraftLogMaxFiles] = useState("");
  const [draftMaxConcurrentRuns, setDraftMaxConcurrentRuns] = useState("");
  const [saving, setSaving] = useState(false);
  // Save errors MUST surface (QA round 3: try/finally with no catch
  // swallowed them — a rejected save looked identical to a successful one,
  // and the old flagged ref reloaded on the next visit: "it's not saving").
  const [saveError, setSaveError] = useState<string | null>(null);

  useEffect(() => {
    if (settings) {
      setDraftWorkerModel(settings.defaultWorkerModel ?? "");
      setDraftAskOrchiconModel(settings.defaultAskOrchiconModel ?? "");
      setDraftNoProgress(String(settings.stallNoProgressWindowSeconds ?? ""));
      setDraftNoFileDiff(String(settings.stallNoFileDiffWindowSeconds ?? ""));
      setDraftTextLoop(String(settings.stallTextLoopWindowSeconds ?? ""));
      setDraftRepetitionCount(String(settings.stallRepetitionCount ?? ""));
      setDraftRepetitionWindow(String(settings.stallRepetitionWindowSeconds ?? ""));
      setDraftNudgeMax(String(settings.stallNudgeMax ?? ""));
      setDraftNudgeReplyWindow(String(settings.stallNudgeReplyWindowSeconds ?? ""));
      setDraftNudgeCooldown(String(settings.stallNudgeCooldownSeconds ?? ""));
      setDraftToolHang(String(settings.stallToolHangSeconds ?? ""));
      const budget = parseBudgetDefaults(settings.defaultBudgetOverrides);
      setDraftBudgetTokens(budget.tokens);
      setDraftBudgetCost(budget.costUsd);
      setDraftBudgetWallClock(budget.wallClockSeconds);
      setDraftBudgetToolCalls(budget.toolCallCount);
      setDraftCompactMaxTurns(budget.compactMaxTurns);
      setDraftCompactTiers(budget.compactTiers);
      setDraftWarn(budget.warnings);
      setDraftReapGrace(String(settings.executionReapGraceSeconds ?? ""));
      setDraftReapFailures(String(settings.executionReapConsecutiveFailures ?? ""));
      setDraftLogDirectory(settings.logDirectory ?? "");
      setDraftLogMaxSize(settings.logMaxSizeMb ? String(settings.logMaxSizeMb) : "");
      setDraftLogRollInterval(settings.logRollIntervalHours ? String(settings.logRollIntervalHours) : "");
      setDraftLogRetention(settings.logRetentionDays ? String(settings.logRetentionDays) : "");
      setDraftLogMaxFiles(settings.logMaxFiles ? String(settings.logMaxFiles) : "");
      setDraftMaxConcurrentRuns(String(settings.maxConcurrentRuns ?? ""));
    }
  }, [settings]);

  async function handleSave() {
    setSaving(true);
    setSaveError(null);
    try {
      await updateSettings.mutateAsync({
        defaultWorkerModel: draftWorkerModel,
        defaultAskOrchiconModel: draftAskOrchiconModel,
        stallNoProgressWindowSeconds: parseInt(draftNoProgress) || 0,
        stallNoFileDiffWindowSeconds: parseInt(draftNoFileDiff) || 0,
        stallTextLoopWindowSeconds: parseInt(draftTextLoop) || 0,
        stallRepetitionCount: parseInt(draftRepetitionCount) || 0,
        stallRepetitionWindowSeconds: parseInt(draftRepetitionWindow) || 0,
        stallNudgeMax: parseInt(draftNudgeMax) || 0,
        stallNudgeReplyWindowSeconds: parseInt(draftNudgeReplyWindow) || 0,
        stallNudgeCooldownSeconds: parseInt(draftNudgeCooldown) || 0,
        stallToolHangSeconds: parseInt(draftToolHang) || 0,
        defaultBudgetOverrides: buildBudgetDefaults(
          draftBudgetTokens,
          draftBudgetCost,
          draftBudgetWallClock,
          draftBudgetToolCalls,
          draftCompactMaxTurns,
          draftCompactTiers,
          draftWarn,
        ),
        executionReapGraceSeconds: parseInt(draftReapGrace) || 0,
        executionReapConsecutiveFailures: parseInt(draftReapFailures) || 0,
        logDirectory: draftLogDirectory,
        logMaxSizeMb: parseInt(draftLogMaxSize) || 0,
        logRollIntervalHours: parseInt(draftLogRollInterval) || 0,
        logRetentionDays: parseInt(draftLogRetention) || 0,
        logMaxFiles: parseInt(draftLogMaxFiles) || 0,
        maxConcurrentRuns: parseInt(draftMaxConcurrentRuns) || 0,
      } as any);
    } catch (e) {
      // Surface the rejection (validation errors included) — never swallow.
      setSaveError(String(e));
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="space-y-8">
      {isLoading && <p className="text-sm text-muted-foreground">Loading settings…</p>}

      {!isLoading && (
        <Card className="relative z-20">
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

      {saveError && (
        <p role="alert" className="text-sm text-destructive">
          Save failed: {saveError}
        </p>
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
                description="No file modifications within this window. Empty/0 = built-in default (900) — a fresh tenant keeps real stall detection. Set a negative value (e.g. -1) to disable this check."
                value={draftNoFileDiff}
                onChange={setDraftNoFileDiff}
                placeholder="900"
              />
              <StallField
                label="Text loop window (seconds)"
                description="No meaningful action within this window. Empty/0 = built-in default (600) — a fresh tenant keeps real stall detection. Set a negative value (e.g. -1) to disable this check."
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
                label="Nudge max"
                description="Max nudges to a live session before an advisory stall escalates to a kill + recovery"
                value={draftNudgeMax}
                onChange={setDraftNudgeMax}
                placeholder="2"
              />
              <StallField
                label="Nudge reply window (seconds)"
                description="How long a nudge waits for the worker to break the pattern before the next trip escalates"
                value={draftNudgeReplyWindow}
                onChange={setDraftNudgeReplyWindow}
                placeholder="300"
              />
              <StallField
                label="Nudge cooldown (seconds)"
                description="Minimum gap between nudges"
                value={draftNudgeCooldown}
                onChange={setDraftNudgeCooldown}
                placeholder="60"
              />
              <StallField
                label="Tool hang (seconds)"
                description="A tool call with no events for longer than this is cancelled natively (synthesized cancelled result + course-correcting redirect injected). 0 = default 180s; negative = disabled."
                value={draftToolHang}
                onChange={setDraftToolHang}
                placeholder="180"
              />
            </div>
          </CardContent>
        </Card>
      )}

      {!isLoading && (
        <Card>
          <CardHeader>
            <CardTitle>Execution budget (defaults)</CardTitle>
            <CardDescription>
              Default per-execution budget ceilings applied when a worker does not
              set its own value for a field. A worker&apos;s <code>budget_overrides</code>{" "}
              overrides these per-field. Values are the tenant-level defaults; leave a
              field empty to fall back to the built-in default.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid gap-4 sm:grid-cols-2">
              <StallField
                label="Token limit"
                description="Max tokens per execution. Empty = built-in default (500,000)."
                value={draftBudgetTokens}
                onChange={setDraftBudgetTokens}
                placeholder="1000000"
              />
              <StallField
                label="Cost limit (USD)"
                description="Max spend per execution. Empty = built-in default ($0.50)."
                value={draftBudgetCost}
                onChange={setDraftBudgetCost}
                placeholder="10"
              />
              <StallField
                label="Wall clock timeout (seconds)"
                description="Hard per-execution deadline. Empty = built-in default (3600). A worker can set 0 to disable its own timeout."
                value={draftBudgetWallClock}
                onChange={setDraftBudgetWallClock}
                placeholder="3600"
              />
              <StallField
                label="Tool call limit"
                description="Max tool calls per execution — a hard stop (unlike the token/cost/turn gates below, a tool call already made can't be undone by compacting). Empty = built-in default (100). Set 0 to disable."
                value={draftBudgetToolCalls}
                onChange={setDraftBudgetToolCalls}
                placeholder="100"
              />
              <StallField
                label="Compact interval (turns)"
                description="Force a context compact at least every N turns, regardless of cost — bounds cumulative cache-read resend on long chatty sessions even when per-turn spend stays cheap. Empty = built-in default (30). Set 0 to rely on the cost/token gates only."
                value={draftCompactMaxTurns}
                onChange={setDraftCompactMaxTurns}
                placeholder="30"
              />
            </div>

            <BudgetWarningsEditor
              value={draftWarn}
              onChange={setDraftWarn}
            />

            <CompactTiersEditor
              value={draftCompactTiers}
              onChange={setDraftCompactTiers}
            />
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
            </div>
          </CardContent>
        </Card>
      )}

      {!isLoading && (
        <Card>
          <CardHeader>
            <CardTitle>Dispatch concurrency</CardTitle>
            <CardDescription>
              Tenant-wide cap on how many worker executions may run
              concurrently. Enforced at dispatch time: projects at their cap
              hold ready items until a running execution frees a slot. A
              project can override this per-project (Project &rarr;
              Concurrency guard); the effective limit for a project is{" "}
              <code>min(tenant, project)</code>, where 0 on either side means
              no additional restriction.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <StallField
              label="Max concurrent runs (tenant)"
              description="0 = no tenant-wide cap. Non-repo projects (in-place execution) serialize by default unless they explicitly opt in."
              value={draftMaxConcurrentRuns}
              onChange={setDraftMaxConcurrentRuns}
              placeholder="0"
            />
          </CardContent>
        </Card>
      )}

      {!isLoading && (
        <Card>
          <CardHeader>
            <CardTitle>Log management</CardTitle>
            <CardDescription>
              Rotating on-disk serve logs. The serve process rotates the active
              log file when it exceeds the size ceiling or the roll interval
              elapses, and prunes old rotated files past the retention window.
              Values are applied live to a running detached serve (no restart
              needed). Empty fields keep the current env/config default. Zero
              means &quot;use the built-in default&quot;.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div>
              <label className="mb-1 block text-sm font-medium">
                Log directory
              </label>
              <Input
                value={draftLogDirectory}
                onChange={(e) => setDraftLogDirectory(e.target.value)}
                placeholder=".dev/logs"
              />
              <p className="mt-0.5 text-xs text-muted-foreground">
                Where serve log files are stored. Empty = default (env
                ORCHICON_LOG_DIR, else .dev/logs).
              </p>
            </div>
            <div className="grid gap-4 sm:grid-cols-2">
              <StallField
                label="Max log size (MB)"
                description="Rotate a file once it exceeds this size. Empty = default (100)."
                value={draftLogMaxSize}
                onChange={setDraftLogMaxSize}
                placeholder="100"
              />
              <StallField
                label="Roll interval (hours)"
                description="Rotate by time even when under the size ceiling. 24 = daily, 1 = hourly. Empty = default (24)."
                value={draftLogRollInterval}
                onChange={setDraftLogRollInterval}
                placeholder="24"
              />
              <StallField
                label="Retention (days)"
                description="Delete rotated log files older than this. Empty = default (7)."
                value={draftLogRetention}
                onChange={setDraftLogRetention}
                placeholder="7"
              />
              <StallField
                label="Max rotated files"
                description="Keep at most this many rotated files (newest kept). Empty = default (7)."
                value={draftLogMaxFiles}
                onChange={setDraftLogMaxFiles}
                placeholder="7"
              />
            </div>
          </CardContent>
        </Card>
      )}

      {!isLoading && (
        <div className="flex justify-end">
          <Button onClick={handleSave} disabled={saving}>
            <Save aria-hidden="true" className="mr-2 h-4 w-4" />
            {saving ? "Saving…" : "Save settings"}
          </Button>
        </div>
      )}
    </div>
  );
}

function UserGuideTab() {
  return (
    <div className="space-y-8 text-sm leading-relaxed">
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <BookOpen aria-hidden="true" className="h-5 w-5" />
            Orchicon User Guide
          </CardTitle>
          <CardDescription>
            How Orchicon works and how to use it, end to end. The in-app
            reference stays high level; DOCUMENTATION.md has the full detail.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-6">
          <div>
            <h3 className="font-medium mb-1">The big idea</h3>
            <p className="text-muted-foreground">
              Orchicon separates <strong>orchestration</strong> from{" "}
              <strong>execution</strong>. You configure the plan (projects,
              workers, work items, workflows, schedules, policies); the control
              plane dispatches autonomous AI workers to execute it and watches
              for failures, stalls, and cost. Everything you configure lives in
              this UI and can also be driven conversationally through{" "}
              <strong>Ask Orchicon</strong>.
            </p>
          </div>

          <div>
            <h3 className="font-medium mb-1">Typical flow</h3>
            <ol className="list-decimal pl-5 space-y-1 text-muted-foreground">
              <li>
                <strong>Create a project</strong> — the top-level container with
                a directory, goals, and context files that workers read.
              </li>
              <li>
                <strong>Define a worker</strong> — a reusable persona (Role,
                Skills, Behavior, AGENTS.md, model, budget). Draft it, then{" "}
                <strong>publish</strong>; published versions are immutable and
                dispatchable.
              </li>
              <li>
                <strong>Create a work item</strong> — Epic → Feature → Task →
                Subtask, optionally with dependency edges and acceptance
                criteria. Assign it a worker.
              </li>
              <li>
                <strong>Build a workflow</strong> — a DAG of steps (task,
                human/AI approval, loop decision, sub-work-item) that turns the
                work item into autonomous work. Many steps can run in parallel.
              </li>
              <li>
                <strong>Run it</strong> — start it now, schedule a one-time
                start, make it recurring, or chain multiple work items into a
                sequence that runs one after another.
              </li>
              <li>
                <strong>Watch & verify</strong> — executions stream their
                output; the run view shows step state, PRs for git-backed work,
                the acceptance review on completion, and any recovery episodes.
              </li>
            </ol>
          </div>

          <div>
            <h3 className="font-medium mb-1">Scheduling & sequences</h3>
            <p className="text-muted-foreground">
              A work item can be <strong>scheduled</strong> (one-time start),
              <strong> recurring</strong> (minute/hourly/daily/weekly/monthly
              cadence — it re-arms instead of going terminal between
              occurrences), or, when it has children, become a{" "}
              <strong>sequence</strong> that runs its children one after another
              (an epic whose features run in chain order, depth-first). Sequence
              order is derived from your drag order on the board/tree.
            </p>
          </div>

          <div>
            <h3 className="font-medium mb-1">Dependencies & blocked work</h3>
            <p className="text-muted-foreground">
              Work items can depend on each other (<code>depends_on</code> /
              <code>blocks</code>, forming a DAG with cycle protection). An item
              waiting on an upstream success shows a distinct{" "}
              <strong>blocked</strong> status with the blocking items named — it
              clears automatically once the dependency is met.
            </p>
          </div>

          <div>
            <h3 className="font-medium mb-1">Ideas &amp; automation</h3>
            <p className="text-muted-foreground">
              Orchicon can find work for you, not just execute it. A{" "}
              <strong>recurring work item</strong> re-fires on a cadence — the
              flagship example is the <strong>Automation Research</strong>{" "}
              pipeline (Automation → Recurring Items): a three-worker crew
              (Planner → Analyst → Synthesizer) that surveys the market live,
              verifies each candidate against external evidence, and distills
              feature proposals. Proposals land in the <strong>Idea Cloud</strong>{" "}
              (Automation → Idea Cloud), a triage board separate from your real
              work items. Each idea carries its evidence and the run that
              produced it: <strong>promote</strong> it to turn it into real,
              schedulable work, or <strong>dismiss</strong> it — dismissed ideas
              are kept as rejected history (the Idea Cloud's Rejected view) so
              automation never re-proposes them.
            </p>
          </div>

          <div>
            <h3 className="font-medium mb-1">When something fails</h3>
            <p className="text-muted-foreground">
              Failures recover automatically by default: capture → summarize →
              preserve → review → plan → resume, with L1→L2→L3 escalation on
              repeated failures. A stalled worker is <strong>nudged first</strong>{" "}
              (an in-session message); only total silence or an unbroken loop
              escalates to an abort. For git-backed runs, a failed run reuses
              its branch so a retry carries over partial work.
            </p>
          </div>

          <div>
            <h3 className="font-medium mb-1">Governance & visibility</h3>
            <p className="text-muted-foreground">
              Policies (Rego/OPA) gate admission, dispatch, budget, approval,
              and recovery. Roles carry entitlements and bind to identities
              (Admin → Roles / Manage Identity Roles; the{" "}
              <code>admin</code> role is immutable). Every mutation writes a
              row to the audit trail (Admin → Audit). Telemetry & Cost shows
              traces, logs, metrics, and spend per project/task/execution/model/
              workflow.
            </p>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <SlidersHorizontal aria-hidden="true" className="h-5 w-5" />
            Settings explained
          </CardTitle>
          <CardDescription>
            What each tab on this Settings page controls.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-5">
          <div>
            <h3 className="font-medium mb-1">Appearance</h3>
              <p className="text-muted-foreground">
                Pick light or dark mode and a theme for each — 20 hand-tuned
                variants (10 light + 10 dark). Cosmetic only — nothing here
                changes platform behavior. The whole shell is also usable on a
                phone.
              </p>
          </div>
          <div>
            <h3 className="font-medium mb-1">Defaults</h3>
            <ul className="list-disc pl-5 space-y-1 text-muted-foreground">
              <li>
                <strong>Default models</strong> — used when a worker/feature
                doesn't pin a model. If both are empty, dispatch fails; you
                generally want a worker model and an Ask Orchicon model set.
              </li>
              <li>
                <strong>Recovery stall parameters</strong> — per-execution stall
                thresholds: no progress (fatal silence), no file diff, text
                loop, and tool-call repetition (count within a window; only
                erroring calls count). Nudge max / reply window / cooldown
                control how a stalled but responsive worker is probed before
                escalation. Zero = use the built-in/env default.
              </li>
              <li>
                <strong>Execution budget defaults</strong> — ceilings (tokens,
                cost, wall clock, tool calls) applied when a worker/plan doesn't
                override them. Protect against runaway spend.
              </li>
              <li>
                <strong>Execution liveness reaper</strong> — fails executions
                whose runtime process vanished (plane restart, lost container);
                grace + consecutive not-alive probes prevent false kills.
              </li>
              <li>
                <strong>Dispatch concurrency</strong> — tenant-wide cap on
                concurrent runs; effective limit per project is{" "}
                <code>min(tenant, project)</code>.
              </li>
              <li>
                <strong>Log management</strong> — rotate/prune serve logs:
                directory, max size, roll interval, retention, max rotated
                files. Applied live to a running serve.
              </li>
            </ul>
          </div>
          <div>
            <h3 className="font-medium mb-1">Session</h3>
            <p className="text-muted-foreground">
              Token lifetimes: the access token (short, refreshed silently) and
              the refresh token (HttpOnly cookie, keeps you signed in). Shorter
              access-TTL = more frequent transparent refreshes; longer
              refresh-TTL = longer stay signed-in.
            </p>
          </div>
          <div>
            <h3 className="font-medium mb-1">Backups</h3>
            <p className="text-muted-foreground">
              Scheduled DB snapshots (cron, retention, target directory) plus
              one-click "create backup now" / restore / delete. Restoring
              returns the whole instance to a previous snapshot.
            </p>
          </div>
          <div>
            <h3 className="font-medium mb-1">User Guide</h3>
            <p className="text-muted-foreground">
              This page. Live in-session guidance for the whole control plane.
            </p>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}

function SessionTab() {
  const { data: settings, isLoading } = useGetSettings();
  const updateSettings = useUpdateSettings();

  const [draftAccessTtl, setDraftAccessTtl] = useState("");
  const [draftRefreshTtl, setDraftRefreshTtl] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (settings) {
      setDraftAccessTtl(settings.sessionAccessTokenTtlSeconds ? String(settings.sessionAccessTokenTtlSeconds) : "900");
      setDraftRefreshTtl(settings.sessionRefreshTokenTtlSeconds ? String(settings.sessionRefreshTokenTtlSeconds) : "86400");
    }
  }, [settings]);

  async function handleSave() {
    setSaving(true);
    try {
      await updateSettings.mutateAsync({
        sessionAccessTokenTtlSeconds: parseInt(draftAccessTtl) || 0,
        sessionRefreshTokenTtlSeconds: parseInt(draftRefreshTtl) || 0,
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
            <CardTitle>Session timeout</CardTitle>
            <CardDescription>
              Controls how long authentication tokens remain valid. A shorter access-TTL
              means more frequent transparent refreshes; a longer refresh-TTL keeps the
              HttpOnly refresh cookie alive longer. Zero fields keep the current value.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid gap-4 sm:grid-cols-2">
              <StallField
                label="Access token TTL (seconds)"
                description="How long an access token is valid. Range: 30–86400. Default: 900 (15 min)."
                value={draftAccessTtl}
                onChange={setDraftAccessTtl}
                placeholder="900"
              />
              <StallField
                label="Refresh token TTL (seconds)"
                description="How long a refresh token (HttpOnly cookie) is valid. Range: 300–31536000. Default: 86400 (24 hours). Must exceed access token TTL."
                value={draftRefreshTtl}
                onChange={setDraftRefreshTtl}
                placeholder="86400"
              />
            </div>
          </CardContent>
        </Card>
      )}

      {!isLoading && (
        <div className="flex justify-end">
          <Button onClick={handleSave} disabled={saving}>
            <Save aria-hidden="true" className="mr-2 h-4 w-4" />
            {saving ? "Saving…" : "Save session settings"}
          </Button>
        </div>
      )}
    </div>
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
            <Check aria-hidden="true" className="h-3 w-3" />
          </span>
        )}
      </div>
    </button>
  );
}

// ─── Budget warnings (warn → escalate → final ladder) ─────────────────────
//
// Each dimension (tokens/cost/tool-calls/time) has three thresholds, as
// fractions of its limit, and three escalating message templates. The
// thresholds are applied uniformly; messages may be left blank to fall back
// to Orchicon's built-in demanding copy.

function BudgetWarningsEditor({
  value,
  onChange,
}: {
  value: BudgetWarnings;
  onChange: (v: BudgetWarnings) => void;
}) {
  const setFrac = (dim: keyof BudgetWarnings, idx: number, v: string) => {
    const next: BudgetWarnings = { ...value, [dim]: { ...value[dim], fracs: [...value[dim].fracs] } };
    next[dim].fracs[idx] = v;
    onChange(next);
  };
  const setMsg = (dim: keyof BudgetWarnings, idx: number, v: string) => {
    const next: BudgetWarnings = { ...value, [dim]: { ...value[dim], msgs: [...value[dim].msgs] } };
    next[dim].msgs[idx] = v;
    onChange(next);
  };
  return (
    <div className="mt-4 border-t pt-4">
      <h4 className="text-sm font-medium">Budget warnings (warn → escalate → final)</h4>
      <p className="mt-1 text-xs text-muted-foreground">
        Each dimension warns before its limit, escalating to a hard abort at 100%. Set the
        three thresholds as fractions of the limit (e.g. 0.5 = 50%). Messages are injected
        into the session at each stage; blank = built-in demanding copy.
      </p>
      {([
        ["tokens", "Token warnings"],
        ["costUsd", "Cost warnings"],
        ["toolCallCount", "Tool-call warnings"],
        ["wallClockSeconds", "Time warnings"],
      ] as const).map(([dim, label]) => (
        <div key={dim} className="mt-2 space-y-2">
          <label className="mb-0.5 block text-xs font-medium text-muted-foreground">{label}</label>
          <div className="grid gap-3 sm:grid-cols-3">
            {(["warn", "escalate", "final"] as const).map((tier, idx) => (
              <div key={tier}>
                <label className="mb-0.5 block text-xs text-muted-foreground capitalize">{tier} threshold</label>
                <Input
                  type="number"
                  step="0.01"
                  min="0"
                  max="1"
                  value={value[dim].fracs[idx]}
                  onChange={(e) => setFrac(dim, idx, e.target.value)}
                  placeholder={["0.25", "0.5", "0.75"][idx]}
                />
              </div>
            ))}
          </div>
          {(["warn", "escalate", "final"] as const).map((tier, idx) => (
            <div key={tier}>
              <label className="mb-0.5 block text-xs text-muted-foreground capitalize">{tier} message</label>
              <Input
                type="text"
                value={value[dim].msgs[idx]}
                onChange={(e) => setMsg(dim, idx, e.target.value)}
                placeholder={`Message sent at the ${tier} stage`}
              />
            </div>
          ))}
        </div>
      ))}
    </div>
  );
}

// ─── Compaction policy (per budget-ladder tier) ────────────────────────────
//
// Each budget-ladder tier (warn / escalate / final) can ALSO trigger a context
// compact, or only inject its warning message. Compaction is lossy — it
// collapses the working detail and forces the worker to re-read/re-derive,
// which is itself more tool calls and more re-sent context. So it is an
// explicit operator choice per tier, not a hidden side effect of crossing a
// spend threshold. Reads/writes the budget JSON `compact_tiers` array.

function CompactTiersEditor({
  value,
  onChange,
}: {
  value: CompactTiers;
  onChange: (v: CompactTiers) => void;
}) {
  const setTier = (idx: number, v: boolean) => {
    const next: CompactTiers = [...value];
    next[idx] = v;
    onChange(next);
  };
  const tiers: [string, string, string][] = [
    [
      "warn",
      "Warn",
      "Compact the session when the worker first crosses a warning tier (~25% of a limit). Off by default — the earliest stage is the most disruptive to interrupt, and the worker can still correct course without a destructive collapse.",
    ],
    [
      "escalate",
      "Escalate",
      "Compact when the worker is deeply into budget (~50% of a limit). On by default — shrinking the re-sent working set before the hard abort.",
    ],
    [
      "final",
      "Final",
      "Compact at the final warning (~75% of a limit), just before the hard abort. On by default so re-sent context is as small as possible right before the stop.",
    ],
  ];
  return (
    <div className="mt-4 border-t pt-4">
      <h4 className="text-sm font-medium">Compaction policy (per warning tier)</h4>
      <p className="mt-1 text-xs text-muted-foreground">
        Choose which warning tiers also collapse the session. Compaction is lossy
        and interrupts the worker mid-flight, so it is off at the earliest tier by
        default and on only once the worker is genuinely deep in budget. Turning
        all three off leaves the turn-count hygiene gate + the hard abort as the
        only context-management mechanisms.
      </p>
      <div className="mt-3 space-y-2">
        {tiers.map(([key, label, desc], idx) => (
          <label key={key} className="flex items-start gap-3">
            <input
              type="checkbox"
              checked={value[idx]}
              onChange={(e) => setTier(idx, e.target.checked)}
              className="mt-1 h-4 w-4 shrink-0 rounded border-input accent-primary"
            />
            <span>
              <span className="text-sm font-medium">{label}</span>
              <span className="block text-xs text-muted-foreground">{desc}</span>
            </span>
          </label>
        ))}
      </div>
    </div>
  );
}

// ─── Backup directory browser (server-side filesystem tree) ───────


function SecretsTab() {
  const [secrets, setSecrets] = React.useState<any[]>([]);
  const [name, setName] = React.useState("");
  const [value, setValue] = React.useState("");
  const [desc, setDesc] = React.useState("");
  const [msg, setMsg] = React.useState<string | null>(null);
  const [loading, setLoading] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const { secretsClient } = await import("@/api/clients");
      const res: any = await secretsClient.listSecrets({});
      setSecrets(res.secrets || []);
    } catch (e: any) { setMsg(String(e.message||e)) } finally { setLoading(false) }
  }, []);
  React.useEffect(() => { load() }, [load]);

  const create = async () => {
    setMsg(null);
    try {
      const { secretsClient } = await import("@/api/clients");
      await secretsClient.createSecret({ name, value, description: desc });
      setName(""); setValue(""); setDesc(""); load(); setMsg("Secret created.");
    } catch (e: any) { setMsg(String(e.message||e)) }
  };
  const del = async (id: string) => {
    if (!confirm("Delete secret?")) return;
    const { secretsClient } = await import("@/api/clients");
    await secretsClient.deleteSecret({ id });
    load();
  };
  return (
    <div className="space-y-4">
      <Card>
        <CardHeader><CardTitle>Secrets</CardTitle><CardDescription>Tenant-scoped encrypted secrets (e.g. TAVILY_API_KEY). Values are encrypted at rest (AES-256-GCM) and injected as container env at dispatch. Never stored in plaintext.</CardDescription></CardHeader>
        <CardContent className="space-y-4">
          <div className="grid gap-2 sm:grid-cols-3">
            <Input placeholder="NAME (e.g. TAVILY_API_KEY)" value={name} onChange={(e:any)=>setName(e.target.value.toUpperCase())} />
            <Input placeholder="value" type="password" value={value} onChange={(e:any)=>setValue(e.target.value)} />
            <Input placeholder="description" value={desc} onChange={(e:any)=>setDesc(e.target.value)} />
          </div>
          <Button onClick={create} disabled={!name||!value}>Create secret</Button>
          {msg && <p className="text-sm text-muted-foreground">{msg}</p>}
          {loading ? <p className="text-sm">Loading…</p> : (
            <table className="w-full text-sm"><thead><tr className="text-muted-foreground text-left"><th>Name</th><th>Description</th><th></th></tr></thead><tbody>{secrets.map((s:any)=>(<tr key={s.id} className="border-t"><td className="font-mono py-2">{s.name}</td><td>{s.description}</td><td><Button variant="outline" size="sm" onClick={()=>del(s.id)}>Delete</Button></td></tr>))}</tbody></table>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

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
        <ArrowUp aria-hidden="true" className="h-4 w-4 text-muted-foreground" />
        <span className="truncate text-xs text-muted-foreground">..</span>
        <span className="ml-auto truncate font-mono text-xs text-muted-foreground">{path}</span>
      </div>

      {isLoading && (
        <div className="flex items-center gap-2 px-3 py-4 text-sm text-muted-foreground">
          <Loader2 aria-hidden="true" className="h-4 w-4 animate-spin" />
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
          <Folder aria-hidden="true" className="h-4 w-4 text-amber-700 dark:text-amber-500 shrink-0" />
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
