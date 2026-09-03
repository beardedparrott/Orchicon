import { test, expect } from "@playwright/test";
import type { Page } from "@playwright/test";
import { NAV_GROUPS, ASK_ORCHICON } from "../src/lib/nav-config";
const ROUTES = [...new Set([ASK_ORCHICON.to, ...NAV_GROUPS.flatMap((g) => g.items.map((i) => i.to)), "/projects","/work-items","/workers","/workflows","/executions"] as string[])];

async function setTheme(page: Page, mode: "dark"|"light") {
  await page.addInitScript((m: string) => {
    try {
      localStorage.setItem("orchicon_mode", m);
      const theme = m === "dark" ? "orchicon-dark" : "orchicon-light";
      localStorage.setItem("orchicon_theme_dark", "orchicon-dark");
      localStorage.setItem("orchicon_theme_light", "orchicon-light");
      document.documentElement.setAttribute("data-theme", theme);
      if (m === "dark") document.documentElement.classList.add("dark");
      else document.documentElement.classList.remove("dark");
    } catch { /* localStorage may be unavailable — theme pre-set is best-effort */ }
  }, mode);
}
for (const route of ROUTES) {
  test(`snapshot ${route}`, async ({ page }, testInfo) => {
    const isLight = testInfo.project.name.includes("light");
    const mode = isLight ? "light" : "dark";
    await setTheme(page, mode);
    const errors: string[] = [];
    page.on("console", (msg) => { if (msg.type() === "error") errors.push(msg.text()); });
    await page.goto(route, { waitUntil: "networkidle" });
    await page.waitForTimeout(500);
    const name = route.replace(/\//g, "_").replace(/^_/, "") || "home";
    await expect(page).toHaveScreenshot(`${name}-${mode}-${testInfo.project.name.includes("mobile") ? "mobile" : "desktop"}.png`, { maxDiffPixels: 200, threshold: 0.2 });
    expect(errors, `console errors on ${route}`).toEqual([]);
  });
}
