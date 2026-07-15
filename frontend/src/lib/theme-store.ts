import { create } from "zustand";
import { type Theme, getTheme } from "@/lib/themes";

const STORAGE_THEME_KEY = "orchicon_theme";
const STORAGE_MODE_KEY = "orchicon_mode";

function loadTheme(): string {
  try {
    return localStorage.getItem(STORAGE_THEME_KEY) || "zinc";
  } catch {
    return "zinc";
  }
}

function loadMode(): "light" | "dark" {
  try {
    const m = localStorage.getItem(STORAGE_MODE_KEY);
    if (m === "light" || m === "dark") return m;
  } catch {
    /* ignore */
  }
  return "dark";
}

export type ThemeState = {
  theme: string;
  mode: "light" | "dark";
  resolvedTheme: Theme | undefined;
  setTheme: (theme: string) => void;
  setMode: (mode: "light" | "dark") => void;
  toggleMode: () => void;
};

function apply(theme: string, mode: "light" | "dark") {
  const root = document.documentElement;
  if (mode === "dark") {
    root.classList.add("dark");
  } else {
    root.classList.remove("dark");
  }
  root.setAttribute("data-theme", theme);
}

export const useThemeStore = create<ThemeState>((set) => {
  const initialTheme = loadTheme();
  const initialMode = loadMode();
  apply(initialTheme, initialMode);

  return {
    theme: initialTheme,
    mode: initialMode,
    resolvedTheme: getTheme(initialTheme),

    setTheme: (t) => {
      const mode = useThemeStore.getState().mode;
      apply(t, mode);
      try {
        localStorage.setItem(STORAGE_THEME_KEY, t);
      } catch {
        /* ignore */
      }
      set({ theme: t, resolvedTheme: getTheme(t) });
    },

    setMode: (m) => {
      const theme = useThemeStore.getState().theme;
      apply(theme, m);
      try {
        localStorage.setItem(STORAGE_MODE_KEY, m);
      } catch {
        /* ignore */
      }
      set({ mode: m });
    },

    toggleMode: () => {
      const current = useThemeStore.getState().mode;
      const next = current === "dark" ? "light" : "dark";
      useThemeStore.getState().setMode(next);
    },
  };
});
