import { test, expect } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
const ROUTES = ["/ask-orchicon", "/dashboard", "/projects", "/work-items", "/workers", "/workflows", "/executions", "/cost-explorer", "/telemetry"];
test.describe("a11y axe", () => {
  for (const route of ROUTES) {
    for (const mode of ["dark","light"] as const) {
      test(`${route} ${mode} has no serious violations`, async ({ page }) => {
        await page.addInitScript((m: string) => {
          try {
            localStorage.setItem("orchicon_mode", m);
            const theme = m === "dark" ? "orchicon-dark" : "orchicon-light";
            document.documentElement.setAttribute("data-theme", theme);
            if (m === "dark") document.documentElement.classList.add("dark");
            else document.documentElement.classList.remove("dark");
          } catch {}
        }, mode);
        await page.goto(route, { waitUntil: "networkidle" });
        const results = await new AxeBuilder({ page }).withTags(["wcag2a","wcag2aa"]).analyze();
        const serious = results.violations.filter((v) => v.impact === "critical" || v.impact === "serious");
        expect(serious, JSON.stringify(serious, null, 2)).toEqual([]);
      });
    }
  }
});
