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

import type { StatusMeta } from "@/components/work-items/work-item-meta";

/** True when the active palette is actually dark (dark mode + a dark
 *  theme). Light themes in dark mode report false — their palette stays
 *  light, so they need the light-palette badge variants. */
export function useDarkPalette(): boolean {
  const mode = useThemeStore((s) => s.mode);
  const themeMode = useThemeStore((s) => s.resolvedTheme?.mode);
  return mode === "dark" && themeMode === "dark";
}

/**
 * The palette decision as a PURE function — `useStatusPillClasses` is only the
 * binding of this to the theme store.
 *
 * Split out so the decision itself is directly testable: React's static renderer
 * reads zustand's INITIAL state (useSyncExternalStore's server snapshot), so a
 * component-level test cannot switch palettes in-process. The rule "which class
 * set does a pill use" is what must not regress, and a pure function lets every
 * status be checked in both palettes rather than one example in one theme.
 */
export function statusPillClasses(meta: StatusMeta, isDarkPalette: boolean): string {
  return isDarkPalette ? meta.pillDark : meta.pill;
}

/**
 * Resolve a StatusMeta's pill classes for the ACTIVE palette.
 *
 * This is the ONE place the light/dark choice is made, so a badge cannot pick the
 * wrong set. It exists because one did: the archive view's "Archived from" chip
 * read `original.pill` directly (the LIGHT-palette classes), so on a dark theme it
 * painted `text-emerald-800` on `bg-emerald-500/15` over a dark-green page — green
 * on green, which is exactly how the operator reported it. Every other badge in
 * the app already went through `useDarkPalette`; the chips built inline did not,
 * because the decision was copyable rather than shared.
 */
export function useStatusPillClasses(meta: StatusMeta): string {
  return statusPillClasses(meta, useDarkPalette());
}
