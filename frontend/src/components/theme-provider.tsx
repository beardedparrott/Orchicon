import { useEffect, type ReactNode } from "react";
import { useThemeStore } from "@/lib/theme-store";

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
  }, [theme, mode]);

  return <>{children}</>;
}
