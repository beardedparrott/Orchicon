import { createRoute } from "@tanstack/react-router";
import { useState, useMemo } from "react";
import { Sun, Moon, Check, Save } from "lucide-react";

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

function SettingsPage() {
  const currentTheme = useThemeStore((s) => s.theme);
  const currentMode = useThemeStore((s) => s.mode);
  const setTheme = useThemeStore((s) => s.setTheme);
  const setMode = useThemeStore((s) => s.setMode);

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

  // Initialize draft fields when settings load
  const initialized = useMemo(() => {
    if (settings) {
      setDraftWorkerModel(settings.defaultWorkerModel ?? "");
      setDraftAskOrchiconModel(settings.defaultAskOrchiconModel ?? "");
      setDraftNoProgress(String(settings.stallNoProgressWindowSeconds ?? ""));
      setDraftNoFileDiff(String(settings.stallNoFileDiffWindowSeconds ?? ""));
      setDraftTextLoop(String(settings.stallTextLoopWindowSeconds ?? ""));
      setDraftRepetitionCount(String(settings.stallRepetitionCount ?? ""));
      setDraftRepetitionWindow(String(settings.stallRepetitionWindowSeconds ?? ""));
      return true;
    }
    return false;
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

  const hasChanges = initialized;

  return (
    <div className="mx-auto max-w-4xl space-y-8">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Settings</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          Configure tenant-level defaults and preferences.
        </p>
      </div>

      {/* === Appearance === */}
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

      {/* Light themes */}
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

      {/* Dark themes */}
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

      {/* === Defaults === */}
      <section>
        <h2 className="mb-3 text-sm font-medium text-muted-foreground">DEFAULTS</h2>

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
          <Card className="mt-4">
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
          <div className="mt-4 flex justify-end">
            <Button onClick={handleSave} disabled={saving}>
              <Save className="mr-2 h-4 w-4" />
              {saving ? "Saving…" : "Save settings"}
            </Button>
          </div>
        )}
      </section>
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
            <Check className="h-3 w-3" />
          </span>
        )}
      </div>
    </button>
  );
}
