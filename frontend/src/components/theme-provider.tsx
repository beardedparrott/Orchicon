import { useEffect, type ReactNode } from "react";
import { useThemeStore } from "@/lib/theme-store";
import { applyNoBackdropBlurClass } from "@/lib/theme-store";

export function ThemeProvider({ children }: { children: ReactNode }) {
  const theme = useThemeStore((s) => s.theme);
  const mode = useThemeStore((s) => s.mode);

  useEffect(() => {
    const root = document.documentElement;
    if (mode === "dark") {
      root.classList.add("dark");
    } else {
      root.classList.remove("dark");
    }
    root.setAttribute("data-theme", theme);
    applyNoBackdropBlurClass();
  }, [theme, mode]);

  // Re-evaluate backdrop-blur fallback on mount (hardwareConcurrency etc.)
  useEffect(() => {
    applyNoBackdropBlurClass();
    // Listen for prefers-reduced-transparency / reduced-motion changes
    const mqTrans = window.matchMedia("(prefers-reduced-transparency: reduce)");
    const mqMotion = window.matchMedia("(prefers-reduced-motion: reduce)");
    const handler = () => applyNoBackdropBlurClass();
    mqTrans.addEventListener?.("change", handler);
    mqMotion.addEventListener?.("change", handler);
    return () => {
      mqTrans.removeEventListener?.("change", handler);
      mqMotion.removeEventListener?.("change", handler);
    };
  }, []);

  return <>{children}</>;
}
