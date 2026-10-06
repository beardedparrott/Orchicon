import { existsSync } from "node:fs";
import { resolve } from "node:path";
import { defineConfig, devices } from "@playwright/test";

// THE BROWSER BUNDLE IS RESOLVED, NOT ASSUMED.
//
// playwright.activity-e2e.config.ts pins PLAYWRIGHT_BROWSERS_PATH to <repo>/.dev/pw because the runtime
// image exports a /ms-playwright for a DIFFERENT chromium build. That pin is wrong on a host checkout:
// here the browsers live in playwright's own default cache (~/.cache/ms-playwright), and pinning a
// non-existent directory fails with "Executable doesn't exist at …/.dev/pw/…". So instead of pinning,
// pick the FIRST location that actually holds the revision @playwright/test 1.62 was built for, and
// otherwise leave the default alone so Playwright prints its own install hint.
const PLAYWRIGHT_REV = "1234";
const revBin = (dir: string) =>
  resolve(dir, `chromium_headless_shell-${PLAYWRIGHT_REV}`, "chrome-headless-shell-linux64", "chrome-headless-shell");
const candidates = [
  process.env.PLAYWRIGHT_BROWSERS_PATH,
  resolve(process.cwd(), "..", ".dev", "pw"),
  resolve(process.env.HOME ?? "/root", ".cache", "ms-playwright"),
].filter((d): d is string => !!d);
const usable = candidates.find((d) => existsSync(revBin(d)));
if (usable) process.env.PLAYWRIGHT_BROWSERS_PATH = usable;

/**
 * playwright.mcp-repro.config.ts — a CLICK-level repro harness for one component contract.
 *
 * WHY A DEDICATED CONFIG, AND WHY THERE IS NO webServer HERE.
 *
 * The DEV PLANE (8080) serves the SPA bundle it embeds, and that embedded bundle is what the
 * operator's browser actually loads. Declaring a vite `webServer` entry (as playwright.config.ts
 * does) would stand up a SECOND, DIFFERENT build and silently test that instead — the exact
 * "tested a different artifact than the one shipped" failure this repro exists to expose. So the
 * spec runs against whatever the plane serves, and the operator brings the plane.
 *
 * Desktop only: the claim under test is DOM/behavioural (does clicking a panel control submit the
 * host form), not a viewport or theme claim, so the other five projects would re-assert the same
 * DOM at different widths.
 */
export default defineConfig({
  testDir: "./tests",
  fullyParallel: false,
  workers: 1,
  reporter: "list",
  use: {
    baseURL: process.env.PLAYWRIGHT_BASE_URL || "http://localhost:8080",
    trace: "on-first-retry",
  },
  projects: [
    { name: "dark-desktop", use: { ...devices["Desktop Chrome"], viewport: { width: 1280, height: 800 } } },
  ],
});
