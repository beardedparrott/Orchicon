import { create } from "zustand";
import { type Theme, getTheme } from "@/lib/themes";

const STORAGE_THEME_LIGHT_KEY = "orchicon_theme_light";
const STORAGE_THEME_DARK_KEY = "orchicon_theme_dark";
const STORAGE_MODE_KEY = "orchicon_mode";
const STORAGE_THEME_LEGACY_KEY = "orchicon_theme";

const DEFAULT_LIGHT_THEME = "orchicon-light";
const DEFAULT_DARK_THEME = "orchicon-dark";

function loadMode(): "light" | "dark" {
  try {
    const m = localStorage.getItem(STORAGE_MODE_KEY);
    if (m === "light" || m === "dark") return m;
  } catch {
    /* ignore */
  }
  return "dark";
}

function loadThemeSlots(): { light: string; dark: string } {
  let light = DEFAULT_LIGHT_THEME;
  let dark = DEFAULT_DARK_THEME;
  try {
    const legacy = localStorage.getItem(STORAGE_THEME_LEGACY_KEY);
    if (legacy && !getTheme(legacy)) {
      // Unknown legacy id — fall back to defaults
    } else if (legacy) {
      const def = getTheme(legacy);
      if (def?.mode === "light") light = legacy;
      else if (def?.mode === "dark") dark = legacy;
    }
    const sl = localStorage.getItem(STORAGE_THEME_LIGHT_KEY);
    if (sl && getTheme(sl)) light = sl;
    else if (sl && !getTheme(sl)) light = DEFAULT_LIGHT_THEME;
    const sd = localStorage.getItem(STORAGE_THEME_DARK_KEY);
    if (sd && getTheme(sd)) dark = sd;
    else if (sd && !getTheme(sd)) dark = DEFAULT_DARK_THEME;
  } catch {
    /* ignore */
  }
  return { light, dark };
}

export type ThemeState = {
  theme: string;
  lightTheme: string;
  darkTheme: string;
  mode: "light" | "dark";
  resolvedTheme: Theme | undefined;
  setTheme: (theme: string) => void;
  setMode: (mode: "light" | "dark") => void;
  toggleMode: () => void;
};

function apply(theme: string, mode: "light" | "dark") {
  const root = document.documentElement;
  if (mode === "dark") root.classList.add("dark");
  else root.classList.remove("dark");
  root.setAttribute("data-theme", theme);
}

export const useThemeStore = create<ThemeState>((set, get) => {
  const slots = loadThemeSlots();
  const initialMode = loadMode();
  const initialTheme = initialMode === "dark" ? slots.dark : slots.light;
  apply(initialTheme, initialMode);

  const persist = (light: string, dark: string) => {
    try {
      localStorage.setItem(STORAGE_THEME_LIGHT_KEY, light);
      localStorage.setItem(STORAGE_THEME_DARK_KEY, dark);
    } catch {
      /* ignore */
    }
  };

  return {
    theme: initialTheme,
    lightTheme: slots.light,
    darkTheme: slots.dark,
    mode: initialMode,
    resolvedTheme: getTheme(initialTheme),

    setTheme: (t) => {
      const def = getTheme(t);
      if (!def) return;
      const state = get();
      const light = def.mode === "light" ? t : state.lightTheme;
      const dark = def.mode === "dark" ? t : state.darkTheme;
      const active = state.mode === "dark" ? dark : light;
      apply(active, state.mode);
      persist(light, dark);
      set({ theme: active, lightTheme: light, darkTheme: dark, resolvedTheme: getTheme(active) });
    },

    setMode: (m) => {
      const state = get();
      const active = m === "dark" ? state.darkTheme : state.lightTheme;
      apply(active, m);
      try {
        localStorage.setItem(STORAGE_MODE_KEY, m);
      } catch {
        /* ignore */
      }
      set({ mode: m, theme: active, resolvedTheme: getTheme(active) });
    },

    toggleMode: () => {
      get().setMode(get().mode === "dark" ? "light" : "dark");
    },
  };
});
