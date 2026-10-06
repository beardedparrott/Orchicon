import { defineConfig, devices } from "@playwright/test";

/**
 * playwright.activity-e2e.config.ts — the GUI half of the activity-line end-to-end capstone.
 *
 * WHY A DEDICATED CONFIG. The capstone needs TWO servers up at once — the shared fixture plane
 * (internal/testfixtures/activitye2e, which the Go TUI leg talks to as well) and the SPA dev server
 * proxying /orchicon.api.v1 to it. Declaring both as Playwright `webServer` entries makes the whole
 * observation ONE self-contained command: Playwright starts them, waits for each url, runs the
 * spec, and tears them down. Nothing depends on a server a previous shell left running, so the
 * harness cannot silently attach to a stale plane with a contaminated ledger (which is exactly the
 * false "counters disagree" a hand-started plane produced during development).
 *
 * The plane binds 18080, NOT the 8080 the SPA's default proxy targets, so this run can never touch
 * the container's own sandbox plane. vite.activity-e2e.config.ts points the proxy at it.
 *
 * Desktop only (`dark-desktop`): the cross-client claim is a claim about the LINE, and the six
 * viewport projects would only re-assert the same DOM at different widths.
 */
export default defineConfig({
  testDir: "./tests",
  testMatch: /activity-line-e2e\.spec\.ts/,
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 0,
  reporter: "list",
  use: {
    baseURL: process.env.PLAYWRIGHT_BASE_URL || "http://127.0.0.1:5174",
    trace: "on-first-retry",
  },
  projects: [
    {
      name: "dark-desktop",
      use: { ...devices["Desktop Chrome"], viewport: { width: 1280, height: 800 } },
    },
  ],
  webServer: [
    {
      // The SHARED fixture plane. `go run` is deliberate: it compiles the current source, so the
      // plane the browser sees is the one in this tree, not a stale binary.
      command:
        "go run ./internal/testfixtures/activitye2e/cmd/activitye2e -addr 127.0.0.1:18080",
      cwd: "..",
      url: "http://127.0.0.1:18080/__e2e/state",
      reuseExistingServer: false,
      timeout: 120_000,
      env: {
        GOMODCACHE: process.env.GOMODCACHE || "../.dev/mod",
        GOCACHE: process.env.GOCACHE || "/tmp/orchicon/gocache",
        GOTMPDIR: process.env.GOTMPDIR || "/tmp/orchicon/gotmp",
        ORCH_ACTIVITY_E2E_TRACE: process.env.ORCH_ACTIVITY_E2E_TRACE || "",
      },
    },
    {
      // The SPA dev server, proxying the Connect API + auth routes to the plane above.
      command:
        "node_modules/.bin/vite --config vite.activity-e2e.config.ts --host 127.0.0.1 --port 5174",
      url: "http://127.0.0.1:5174",
      reuseExistingServer: false,
      timeout: 120_000,
    },
  ],
  expect: {
    toHaveScreenshot: { maxDiffPixels: 200, threshold: 0.2 },
  },
});
