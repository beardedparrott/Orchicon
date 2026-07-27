import { createRoute } from "@tanstack/react-router";
import { useState, useEffect } from "react";
import { Sun, Moon, Check, Save, BookOpen, Palette, SlidersHorizontal } from "lucide-react";

import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";
import { LIGHT_THEMES, DARK_THEMES } from "@/lib/themes";
import { useThemeStore } from "@/lib/theme-store";
import { useGetSettings, useUpdateSettings } from "@/api/settings";
import { ModelPicker } from "@/components/ModelPicker";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/settings",
  component: SettingsPage,
});

type SettingsTab = "appearance" | "defaults" | "guide";

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
        {[
          ["appearance", "Appearance", Palette],
          ["defaults", "Defaults", SlidersHorizontal],
          ["guide", "User Guide", BookOpen],
        ].map(([id, label, Icon]) => (
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
      {tab === "guide" && <UserGuideTab />}
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
            OpenTelemetry pipeline exporting to SigNoz/ClickHouse. Cost Explorer
            shows spend by Project, Task, Execution, Model, or Workflow.
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
