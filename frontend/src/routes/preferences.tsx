import { createRoute } from "@tanstack/react-router";
import { Sun, Moon, Check } from "lucide-react";

import { Card, CardContent } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";
import { LIGHT_THEMES, DARK_THEMES } from "@/lib/themes";
import { useThemeStore } from "@/lib/theme-store";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/preferences",
  component: PreferencesPage,
});

function PreferencesPage() {
  const currentTheme = useThemeStore((s) => s.theme);
  const currentMode = useThemeStore((s) => s.mode);
  const setTheme = useThemeStore((s) => s.setTheme);
  const setMode = useThemeStore((s) => s.setMode);

  return (
    <div className="mx-auto max-w-4xl space-y-8">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">
          Preferences
        </h1>
        <p className="mt-1 text-sm text-muted-foreground">
          Customize the appearance of the Orchicon control plane.
        </p>
      </div>

      {/* Mode toggle */}
      <section>
        <h2 className="mb-3 text-sm font-medium text-muted-foreground">
          APPEARANCE
        </h2>
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
      {/* Swatch strip */}
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
