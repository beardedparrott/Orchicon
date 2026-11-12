import { existsSync } from "node:fs";
import { resolve } from "node:path";
import { defineConfig, devices } from "@playwright/test";

/**
 * playwright.telemetry-e2e.config.ts — the browser leg of the "Telemetry page with Grafana absent" proof
 * (frontend/tests/telemetry-e2e.spec.ts, driven by scripts/telemetry-e2e.sh).
 *
 * WHY A DEDICATED CONFIG, AND WHY THE BASE URL IS THE PLANE.
 * The claim under test is about THE PLANE'S OWN PAGE: `orchicon` serves the built SPA itself
 * (cmd/orchicon/serve_state.go — embedded FS + index.html fallback for client routes), so /telemetry and
 * the plain-HTTP session endpoints (/auth/local-login) live on ONE origin. That matters: the spec logs in
 * by POSTing to `${E2E_PLANE_URL}/auth/local-login` and expects the cookie to be sent by the page, which is
 * only true when the page IS that origin.
 *
 * The shared playwright.config.ts points baseURL at http://localhost:5173 and declares a `webServer` that
 * starts `pnpm dev`. Used here that would load the SPA from the vite DEV server — whose proxy target is a
 * hard-coded :8080 — and the login cookie would belong to a different origin than the page, so the run
 * would measure the wrong plane (or nothing at all). Hence: no webServer, and the base URL comes from the
 * harness's plane URL.
 *
 * Desktop only (`dark-desktop`): the claim is about the data the page reads, not about six viewports.
 * workers=1 and no retries: the degraded step stops and restarts a backend on the host, and one test's
 * outage must never overlap another's.
 */

// THE BROWSER BUNDLE IS PINNED TO THE PROJECT WHEN THAT BUNDLE EXISTS, for the same reason
// playwright.activity-e2e.config.ts does it: the runtime image exports PLAYWRIGHT_BROWSERS_PATH for a
// DIFFERENT playwright build, and inheriting it makes `playwright test` fail with "Executable doesn't
// exist at …". Guarded on the bundle actually being there so a normal developer checkout is untouched.
const PROJECT_BUNDLE = resolve(process.cwd(), "..", ".dev", "pw");
const revBin = (dir: string) =>
  resolve(dir, "chromium_headless_shell-1234", "chrome-headless-shell-linux64", "chrome-headless-shell");
const inherited = process.env.PLAYWRIGHT_BROWSERS_PATH || "";
if (existsSync(PROJECT_BUNDLE) && (!inherited || !existsSync(revBin(inherited)))) {
  process.env.PLAYWRIGHT_BROWSERS_PATH = PROJECT_BUNDLE;
}

const PLANE = (process.env.E2E_PLANE_URL || process.env.PLAYWRIGHT_BASE_URL || "http://127.0.0.1:8080").replace(
  /\/+$/,
  "",
);

export default defineConfig({
  testDir: "./tests",
  testMatch: /telemetry-e2e\.spec\.ts/,
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 0,
  reporter: "list",
  use: {
    // The page under test is the plane's own origin — the same one /auth/local-login sets its cookie on.
    baseURL: PLANE,
    trace: "on-first-retry",
  },
  projects: [
    { name: "dark-desktop", use: { ...devices["Desktop Chrome"], viewport: { width: 1280, height: 800 } } },
  ],
  // NO `webServer`: the harness has already started the host-resident plane (`orchicon start`), and the
  // plane is what serves the SPA. A dev server here would be a second, proxied copy of the page.
});
