export type Theme = {
  id: string;
  name: string;
  mode: "light" | "dark";
  /** Preview swatch colors for the theme picker */
  swatches: string[];
};

export const LIGHT_THEMES: Theme[] = [
  {
    id: "neutral",
    name: "Neutral",
    mode: "light",
    swatches: ["#ffffff", "#f5f5f5", "#e5e5e5", "#171717", "#ef4444"],
  },
  {
    id: "warm",
    name: "Warm",
    mode: "light",
    swatches: ["#fefcf5", "#f5f0e6", "#e8dfd1", "#2c2416", "#e8553d"],
  },
  {
    id: "cool",
    name: "Cool",
    mode: "light",
    swatches: ["#f8fafc", "#f0f4f8", "#e2e8f0", "#0f172a", "#ef4444"],
  },
  {
    id: "zinc",
    name: "Zinc",
    mode: "light",
    swatches: ["#ffffff", "#f4f4f5", "#e4e4e7", "#18181b", "#ef4444"],
  },
  {
    id: "rose",
    name: "Rose",
    mode: "light",
    swatches: ["#fef8f9", "#f5e8ec", "#ecdaE0", "#2c1b21", "#e73d5b"],
  },
  {
    id: "sky",
    name: "Sky",
    mode: "light",
    swatches: ["#f4faff", "#e8f4fd", "#daeef8", "#162b3d", "#ef4444"],
  },
  {
    id: "emerald",
    name: "Emerald",
    mode: "light",
    swatches: ["#f4faf8", "#e6f5ee", "#d4ece2", "#14382a", "#ef4444"],
  },
  {
    id: "violet",
    name: "Violet",
    mode: "light",
    swatches: ["#f8f6fd", "#eeebf8", "#e2ddf0", "#21173d", "#ef4444"],
  },
  {
    id: "amber",
    name: "Amber",
    mode: "light",
    swatches: ["#fef9ec", "#faf0d6", "#f5e4bc", "#26200e", "#ef4444"],
  },
  {
    id: "teal",
    name: "Teal",
    mode: "light",
    swatches: ["#f4fbfb", "#e6f5f4", "#d4eceb", "#11302e", "#ef4444"],
  },
];

export const DARK_THEMES: Theme[] = [
  {
    id: "midnight",
    name: "Midnight",
    mode: "dark",
    swatches: ["#111827", "#1a2332", "#2a3448", "#93b4e6", "#bf4040"],
  },
  {
    id: "charcoal",
    name: "Charcoal",
    mode: "dark",
    swatches: ["#1a1614", "#211d1a", "#302a26", "#b0a69b", "#bf4040"],
  },
  {
    id: "storm",
    name: "Storm",
    mode: "dark",
    swatches: ["#12161e", "#1a1f2a", "#2a3040", "#9aaccc", "#bf4040"],
  },
  {
    id: "dim",
    name: "Dim",
    mode: "dark",
    swatches: ["#141417", "#1b1b20", "#2c2c33", "#c9c9d6", "#b33d3d"],
  },
  {
    id: "mauve",
    name: "Mauve",
    mode: "dark",
    swatches: ["#161214", "#1e191c", "#312a2e", "#d4a3b3", "#bf4040"],
  },
  {
    id: "cobalt",
    name: "Cobalt",
    mode: "dark",
    swatches: ["#111821", "#1a2332", "#2a344a", "#6ba1e0", "#bf4040"],
  },
  {
    id: "pine",
    name: "Pine",
    mode: "dark",
    swatches: ["#0f1714", "#18221d", "#28352e", "#66b896", "#bf4040"],
  },
  {
    id: "plum",
    name: "Plum",
    mode: "dark",
    swatches: ["#16121e", "#1e1930", "#302a42", "#a68ed4", "#bf4040"],
  },
  {
    id: "ember",
    name: "Ember",
    mode: "dark",
    swatches: ["#1a1512", "#221d19", "#332d26", "#cfa470", "#bf4040"],
  },
  {
    id: "abyss",
    name: "Abyss",
    mode: "dark",
    swatches: ["#0f1716", "#182220", "#283533", "#5cb8b0", "#bf4040"],
  },
];

export const ALL_THEMES = [...LIGHT_THEMES, ...DARK_THEMES];

export function getTheme(id: string): Theme | undefined {
  return ALL_THEMES.find((t) => t.id === id);
}

export function themeClass(themeId: string, mode: "light" | "dark"): string {
  return mode === "dark" ? `${themeId} dark` : themeId;
}
