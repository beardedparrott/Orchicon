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
  {
    id: "orchicon-light-gruvbox",
    name: "Gruvbox Hearth",
    mode: "light",
    swatches: ["#fbf1c7", "#ebdbb2", "#b57614", "#af3a03", "#79740e"],
  },
  {
    id: "orchicon-light-ember",
    name: "Ember",
    mode: "light",
    swatches: ["#fff5f0", "#ffffff", "#e11d48", "#f59e0b", "#ea580c"],
  },
  {
    id: "orchicon-light-forest",
    name: "Forest",
    mode: "light",
    swatches: ["#f0fdf4", "#ffffff", "#059669", "#0891b2", "#0d9488"],
  },
  {
    id: "orchicon-light-ocean",
    name: "Ocean",
    mode: "light",
    swatches: ["#f0f9ff", "#ffffff", "#0284c7", "#4f46e5", "#0891b2"],
  },
  {
    id: "orchicon-light-violet",
    name: "Violet",
    mode: "light",
    swatches: ["#f5f3ff", "#ffffff", "#7c3aed", "#4f46e5", "#c026d3"],
  },
  {
    id: "orchicon-light-amber",
    name: "Amber Dune",
    mode: "light",
    swatches: ["#fffbeb", "#ffffff", "#b45309", "#ea580c", "#ca8a04"],
  },
  {
    id: "orchicon-light-slate",
    name: "Slate Mist",
    mode: "light",
    swatches: ["#f1f5f9", "#ffffff", "#475569", "#0891b2", "#7c3aed"],
  },
  {
    id: "orchicon-light-rose",
    name: "Rose Blush",
    mode: "light",
    swatches: ["#fff1f2", "#ffffff", "#e11d48", "#ec4899", "#7c3aed"],
  },
  {
    id: "orchicon-light-teal",
    name: "Teal Lagoon",
    mode: "light",
    swatches: ["#f0fdfa", "#ffffff", "#0d9488", "#0891b2", "#059669"],
  },
];

export const DARK_THEMES: Theme[] = [
  {
    id: "orchicon-dark",
    name: "Obsidian",
    mode: "dark",
    swatches: ["#0e1525", "#1a2744", "#38bdf8", "#6366f1", "#a855f7"],
  },
  {
    id: "orchicon-dark-ember",
    name: "Ember Night",
    mode: "dark",
    swatches: ["#1c1310", "#2a1e1a", "#fb7185", "#fbbf24", "#fb923c"],
  },
  {
    id: "orchicon-dark-forest",
    name: "Forest Night",
    mode: "dark",
    swatches: ["#101a14", "#1a2e22", "#34d399", "#22d3ee", "#2dd4bf"],
  },
  {
    id: "orchicon-dark-ocean",
    name: "Ocean Abyss",
    mode: "dark",
    swatches: ["#0f1a25", "#1a2e44", "#38bdf8", "#818cf8", "#22d3ee"],
  },
  {
    id: "orchicon-dark-violet",
    name: "Violet Dusk",
    mode: "dark",
    swatches: ["#18141f", "#2a2140", "#a78bfa", "#818cf8", "#e879f9"],
  },
  {
    id: "orchicon-dark-amber",
    name: "Amber Ember",
    mode: "dark",
    swatches: ["#1c1810", "#2e2818", "#fbbf24", "#fb923c", "#facc15"],
  },
  {
    id: "orchicon-dark-slate",
    name: "Slate Shadow",
    mode: "dark",
    swatches: ["#12151c", "#1e293b", "#94a3b8", "#22d3ee", "#a78bfa"],
  },
  {
    id: "orchicon-dark-rose",
    name: "Rose Noir",
    mode: "dark",
    swatches: ["#1c1214", "#2e1a22", "#fb7185", "#f472b6", "#a78bfa"],
  },
  {
    id: "orchicon-dark-teal",
    name: "Teal Depths",
    mode: "dark",
    swatches: ["#0f1c1c", "#1a2e2e", "#2dd4bf", "#22d3ee", "#34d399"],
  },
  {
    id: "orchicon-dark-crimson",
    name: "Crimson Eclipse",
    mode: "dark",
    swatches: ["#1c1214", "#2e1a1e", "#f87171", "#fb7185", "#fbbf24"],
  },
];

export const ALL_THEMES = [...LIGHT_THEMES, ...DARK_THEMES];

export function getTheme(id: string): Theme | undefined {
  return ALL_THEMES.find((t) => t.id === id);
}

export function themeClass(themeId: string, mode: "light" | "dark"): string {
  return mode === "dark" ? `${themeId} dark` : themeId;
}
