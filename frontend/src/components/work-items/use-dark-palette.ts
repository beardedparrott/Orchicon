// Palette-darkness hook for theme-aware badges (ADR-7).
//
// The app's default config is zinc (a LIGHT theme) + dark mode: the
// `.dark` class is present on <html> but zinc defines no dark-palette
// overrides, so the page keeps a light background while Tailwind's
// `dark:` variants are active. Keying badge colors off `.dark` alone
// made badge text light-on-light (1.13:1 — WCAG fail). Only the RESOLVED
// theme's own mode says what the palette really is, so that is what this
// hook reads.

import { useThemeStore } from "@/lib/theme-store";

/** True when the active palette is actually dark (dark mode + a dark
 *  theme). Light themes in dark mode report false — their palette stays
 *  light, so they need the light-palette badge variants. */
export function useDarkPalette(): boolean {
  const mode = useThemeStore((s) => s.mode);
  const themeMode = useThemeStore((s) => s.resolvedTheme?.mode);
  return mode === "dark" && themeMode === "dark";
}
