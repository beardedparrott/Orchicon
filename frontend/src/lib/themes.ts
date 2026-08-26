export type Theme = {
  id: string;
  name: string;
  mode: "light" | "dark";
  /** Preview swatch colors for the theme picker */
  swatches: string[];
};

export const LIGHT_THEMES: Theme[] = [
  {
    id: "orchicon-light",
    name: "Lumen",
    mode: "light",
    swatches: ["#f8fafc", "#ffffff", "#06b6d4", "#6366f1", "#a855f7"],
  },
];

export const DARK_THEMES: Theme[] = [
  {
    id: "orchicon-dark",
    name: "Obsidian",
    mode: "dark",
    swatches: ["#030712", "#0f172a", "#38bdf8", "#6366f1", "#a855f7"],
  },
];

export const ALL_THEMES = [...LIGHT_THEMES, ...DARK_THEMES];

export function getTheme(id: string): Theme | undefined {
  return ALL_THEMES.find((t) => t.id === id);
}

export function themeClass(themeId: string, mode: "light" | "dark"): string {
  return mode === "dark" ? `${themeId} dark` : themeId;
}
