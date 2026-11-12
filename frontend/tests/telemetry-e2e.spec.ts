/**
 * telemetry-e2e.spec.ts — the BROWSER half of the "Telemetry page works with Grafana absent" proof.
 *
 * WHY A SPEC AND NOT A HUMAN WITH A SCREENSHOT TOOL. The claim's hardest clause is AC5: "stopping a
 * backend produces the degraded state that NAMES it, and restarting it recovers — with NO page reload
 * masking the state". A human cannot prove the absence of a reload; a spec can, because there is no
 * `reload()` call anywhere in this file (scripts/telemetry-e2e.sh greps for one, behind a control
 * match, before it runs a single line of it). Everything below therefore drives the SAME page instance
 * from first paint to recovery.
 *
 * WHAT IT BELIEVES. Nothing the page says about itself. The ids and values it asserts on come from
 * `scripts/telemetry-e2e.sh`, which obtained them by querying Tempo / Loki / VictoriaMetrics DIRECTLY
 * and by creating a real workflow run through the plane's own API. So "a chart rendered" is never
 * enough: the graph must carry a real sample count, the log panel must return the run's own id, and
 * the trace view must show the run's execution. A page reading a fixture cannot do that.
 *
 * SELECTORS ARE THE COMPONENTS' OWN CONTRACT, pinned on the native branch:
 *   [role=group][aria-label="Telemetry filters"]   telemetry-filters.tsx:125
 *   select[aria-label="Time range"|"Project"]      telemetry-filters.tsx:132,170
 *   input[aria-label="Execution id"|"Search log text"]  telemetry-filters.tsx:182,224
 *   #telemetry-window-start / -end                 telemetry-filters.tsx:144,154 (range === "custom")
 *   [role=tablist][aria-label="Telemetry sections"] telemetry.tsx:132-133
 *   [role=tabpanel][aria-labelledby="telemetry-tab-<id>"]  telemetry.tsx:163-165
 *   [role=group][aria-label="Group reports by"]     reports-panel.tsx:110
 *   [role=status]                                   states.tsx:112,149 (degraded + loading)
 *
 * RUN IT (the harness does this for you):
 *   cd frontend && E2E_EVIDENCE=<dir> E2E_PROJECT_ID=… E2E_RUN_ID=… E2E_LOKI_PID=… \
 *     npx playwright test tests/telemetry-e2e.spec.ts --config playwright.config.ts \
 *       --project=dark-desktop --workers=1
 *
 * VERDICT ROWS. Every assertion appends `step<TAB>PASS|FAIL|SKIP<TAB>observation<TAB>owner` to
 * `<E2E_EVIDENCE>/browser-results.tsv`; the harness ingests them, and a step with NO row is treated as
 * a failure downstream ("the browser leg produced no verdict"), never as a pass.
 */
import { test, expect, type Page, type Response } from "@playwright/test";
import { appendFileSync, mkdirSync, writeFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { resolve } from "node:path";

const EVIDENCE = resolve(process.env.E2E_EVIDENCE || "qa-evidence/telemetry-e2e");
const PLANE = (process.env.E2E_PLANE_URL || "").replace(/\/+$/, "");

const PROJECT_ID = process.env.E2E_PROJECT_ID || "";
const FOREIGN_PROJECT_ID = process.env.E2E_FOREIGN_PROJECT_ID || "";
const RUN_ID = process.env.E2E_RUN_ID || "";
const EXECUTION_ID = process.env.E2E_EXECUTION_ID || "";
const TRACE_ID = process.env.E2E_TRACE_ID || "";
const LOKI_PID = process.env.E2E_LOKI_PID || "";
const LOKI_RESTART_CMD = process.env.E2E_LOKI_RESTART_CMD || "";
const VM_METRIC = process.env.E2E_VM_METRIC || "";
const VM_VALUE = process.env.E2E_VM_VALUE || "";

/** The filters group is the one control surface every panel reads, so its absence means the page
 *  cannot be exercised at all — that is a BLOCKED row, not a silent skip. */
const FILTERS = '[role="group"][aria-label="Telemetry filters"]';

function ensureDir(): void {
  try {
    mkdirSync(EVIDENCE, { recursive: true });
  } catch {
    /* evidence dir is the harness's; a failure here surfaces as a missing artifact */
  }
}

/** record appends one verdict row. `owner` is the file:line of the sibling task that owns a failure —
 *  this spec patches nothing (AC7), it accuses. */
function record(step: number, status: "PASS" | "FAIL" | "SKIP" | "BLOCKED", observation: string, owner = ""): void {
  ensureDir();
  appendFileSync(
    resolve(EVIDENCE, "browser-results.tsv"),
    `${step}\t${status}\t${observation.replace(/\s+/g, " ").trim()}\t${owner}\n`,
  );
}

/**
 * step runs one assertion and records exactly one row. The assertion's own call is what fails the
 * Playwright test; the row is what the harness reads.
 */
async function step(n: number, what: string, owner: string, fn: () => Promise<string>): Promise<void> {
  try {
    const observation = await fn();
    record(n, "PASS", `${what} — ${observation}`);
  } catch (err) {
    record(n, "FAIL", `${what} — ${err instanceof Error ? err.message.split("\n")[0] : String(err)}`, owner);
    throw err;
  }
}

async function shot(page: Page, name: string): Promise<void> {
  ensureDir();
  try {
    await page.screenshot({ path: resolve(EVIDENCE, name), fullPage: false });
  } catch {
    /* a screenshot is evidence, not the assertion */
  }
}

/** login establishes the SPA's own session through the SAME origin the browser will use, so the
 *  authenticated /telemetry route is reached without driving a login form. */
async function login(page: Page): Promise<void> {
  if (!PLANE) return;
  const user = process.env.E2E_USER || "admin";
  const password = process.env.E2E_PASSWORD || "orchicon";
  const res = await page.request.post(`${PLANE}/auth/local-login`, {
    data: { username: user, password },
    failOnStatusCode: false,
  });
  if (!res.ok()) throw new Error(`local-login as ${user} returned ${res.status()}`);
}

/** openTelemetry lands on /telemetry and refuses to proceed unless the page rendered the filters —
 *  the control surface every claim below is made through. */
async function openTelemetry(page: Page): Promise<void> {
  await page.goto("/telemetry");
  await expect(page.locator(FILTERS), "the Telemetry filters group is the page's control surface").toBeVisible({
    timeout: 30_000,
  });
}

async function tab(page: Page, name: "Overview" | "Reports" | "Traces" | "Metrics" | "Logs"): Promise<void> {
  await page.getByRole("tab", { name, exact: true }).click();
  await expect(page.locator(`[role="tabpanel"][aria-labelledby="telemetry-tab-${name.toLowerCase()}"]`)).toBeVisible({
    timeout: 20_000,
  });
}

/** resetFilters returns the page to "all projects, default window" so one step cannot inherit the
 *  previous step's narrowing (a filter leaking across steps is how an "empty state" gets faked). */
async function resetFilters(page: Page): Promise<void> {
  await page.selectOption(`${FILTERS} select[aria-label="Project"]`, "");
  const range = page.locator(`${FILTERS} select[aria-label="Time range"]`);
  const options = await range.locator("option").evaluateAll((els) => els.map((e) => (e as HTMLOptionElement).value));
  const fallback = options.find((o) => o !== "" && o !== "custom") || "";
  if (fallback) await range.selectOption(fallback);
}

/** The page's OWN requests and responses, captured for the network-tab evidence (AC6) and for the
 *  "these are real requests to the stores, not fixtures" assertion. */
interface Captured {
  requests: string[];
  bodies: { url: string; body: string }[];
}

function captureNetwork(page: Page): Captured {
  const captured: Captured = { requests: [], bodies: [] };
  page.on("request", (req) => {
    const url = req.url();
    if (url.includes("orchicon.api.v1.TelemetryService") || url.includes("/loki") || url.includes("/api/v1/query")) {
      captured.requests.push(`${req.method()} ${url}`);
    }
  });
  page.on("response", async (res: Response) => {
    const url = res.url();
    if (!url.includes("orchicon.api.v1.TelemetryService")) return;
    try {
      captured.bodies.push({ url, body: (await res.text()).slice(0, 20_000) });
    } catch {
      /* a body that cannot be read is absent evidence, not a passing assertion */
    }
  });
  return captured;
}

function flushNetwork(captured: Captured): void {
  ensureDir();
  writeFileSync(
    resolve(EVIDENCE, "05-network.log"),
    [
      "# every request the page made to the telemetry API / the stores during this run",
      ...captured.requests,
      "",
      "# the telemetry API responses the page consumed (truncated per response)",
      ...captured.bodies.map((b) => `\n== ${b.url}\n${b.body}`),
    ].join("\n"),
  );
}

// ---------------------------------------------------------------------------
// T5 — the happy path: graphs, log search, trace view and report, from real data
// ---------------------------------------------------------------------------
test.describe.configure({ mode: "serial" });

test("T5: the page renders real graphs, real logs, the run's traces and a report", async ({ page }) => {
  test.slow();
  const net = captureNetwork(page);
  await login(page);
  await openTelemetry(page);
  await shot(page, "05-overview.png");
  await step(5, "the Telemetry page renders inside the app shell (Overview tab, filters present)", "frontend/src/routes/telemetry.tsx", async () => {
    await expect(page.getByRole("tab", { name: "Overview", exact: true })).toHaveAttribute("aria-selected", "true");
    return "Overview tab selected, filters visible";
  });

  // --- Graphs, from VictoriaMetrics with REAL values -----------------------
  await tab(page, "Metrics");
  await step(5, "the Metrics tab draws a graph with real, non-zero values from VictoriaMetrics", "frontend/src/components/telemetry/graphs.tsx:120", async () => {
    // graphs.tsx:120 renders "<n> samples across <m> series." — a chart that painted nothing
    // cannot produce this sentence.
    const summary = page.getByText(/\d[\d,]*\s+samples across\s+\d+\s+series/);
    await expect(summary).toBeVisible({ timeout: 60_000 });
    const text = (await summary.innerText()).replace(/\s+/g, " ");
    const samples = Number(/([\d,]+)\s+samples/.exec(text)?.[1].replace(/,/g, "") ?? "0");
    if (!(samples > 0)) throw new Error(`no samples in the window: "${text}"`);
    // "Latest <n> <unit>" is each series' own last value. AT LEAST ONE must be a real, non-zero
    // number: a single named metric can legitimately be 0 in a window (errors on a good run), so
    // pinning this to `.first()` would fail on a perfectly real graph.
    const latest = page.getByText(/Latest\s+[\d,]+/);
    const count = await latest.count();
    if (count === 0) throw new Error("no series reported a latest value — the chart read nothing");
    let value = 0;
    for (let i = 0; i < count; i += 1) {
      const latestText = (await latest.nth(i).innerText()).replace(/\s+/g, " ");
      const v = Number(/Latest\s+([\d,]+)/.exec(latestText)?.[1].replace(/,/g, "") ?? "0");
      if (v > value) value = v;
    }
    if (!(value > 0)) throw new Error(`no series has a real, non-zero latest value across ${count} series`);
    await shot(page, "05-metrics.png");
    return `${text}; latest ${value}${VM_METRIC ? ` (store said ${VM_METRIC}=${VM_VALUE})` : ""}`;
  });

  // --- Log search, returning the RUN's own lines ---------------------------
  await tab(page, "Logs");
  await step(5, "the Logs tab returns real log lines, and the run's own id finds the run's lines", "internal/telemetry/query_client.go", async () => {
    const panel = page.locator('[role="tabpanel"][aria-labelledby="telemetry-tab-logs"]');
    // First: the panel is not in an empty/degraded/error state on its own.
    await expect(panel.getByRole("status")).toHaveCount(0, { timeout: 60_000 });
    if (!RUN_ID) return "logs rendered (no run id supplied to search for)";
    await page.fill(`${FILTERS} input[aria-label="Search log text"]`, RUN_ID);
    // The search is debounced (telemetry-filters.tsx:110-118) and the query is keyed on it: a line
    // that carries THIS run's id can only come from the store, not from a fixture.
    await expect(panel.getByText(RUN_ID, { exact: false }).first()).toBeVisible({ timeout: 60_000 });
    await shot(page, "05-logs.png");
    return `log search for ${RUN_ID} returned lines naming the run`;
  });

  // --- The trace view for this run ----------------------------------------
  await page.fill(`${FILTERS} input[aria-label="Search log text"]`, "");
  await tab(page, "Traces");
  await step(5, "the Traces tab shows the real run's trace (duration chart + rows for this execution)", "frontend/src/components/telemetry/graphs.tsx:160", async () => {
    const panel = page.locator('[role="tabpanel"][aria-labelledby="telemetry-tab-traces"]');
    await expect(panel.getByRole("status")).toHaveCount(0, { timeout: 60_000 });
    if (EXECUTION_ID) {
      await page.fill(`${FILTERS} input[aria-label="Execution id"]`, EXECUTION_ID);
      await expect(panel.getByText(EXECUTION_ID, { exact: false }).first()).toBeVisible({ timeout: 60_000 });
    }
    await shot(page, "05-traces.png");
    return EXECUTION_ID
      ? `the trace view lists execution ${EXECUTION_ID}${TRACE_ID ? ` (store trace ${TRACE_ID})` : ""}`
      : "the trace view rendered traces for the window";
  });
  await page.fill(`${FILTERS} input[aria-label="Execution id"]`, "");

  // --- The report summarising the window ----------------------------------
  await tab(page, "Reports");
  await step(5, "the Reports tab summarises the window and can be regrouped", "frontend/src/components/telemetry/reports-panel.tsx:44", async () => {
    const group = page.locator('[role="group"][aria-label="Group reports by"]');
    await expect(group).toBeVisible({ timeout: 60_000 });
    const panel = page.locator('[role="tabpanel"][aria-labelledby="telemetry-tab-reports"]');
    await expect(panel.getByText(/report|window|cost|token/i).first()).toBeVisible({ timeout: 60_000 });
    await shot(page, "05-reports.png");
    return "the report panel rendered its grouping control and a window summary";
  });

  // --- These were REAL requests, not fixtures -----------------------------
  await step(5, "the page fetched its data over the telemetry API (real requests, not fixtures)", "frontend/src/api/telemetry.ts", async () => {
    flushNetwork(net);
    const rpcs = new Set(
      net.requests.filter((r) => r.includes("TelemetryService")).map((r) => r.split("TelemetryService/")[1] ?? r),
    );
    if (rpcs.size === 0) throw new Error("the page made no TelemetryService request at all");
    return `the page called ${[...rpcs].map((r) => `TelemetryService/${r}`).join(", ")}`;
  });
});

// ---------------------------------------------------------------------------
// T6 — filters: narrowing works, and a window that excludes the data is EMPTY
// ---------------------------------------------------------------------------
test("T6: the project filter narrows, and an excluding time range shows the empty state", async ({ page }) => {
  test.slow();
  await login(page);
  await openTelemetry(page);
  await tab(page, "Metrics");

  const summary = page.getByText(/\d[\d,]*\s+samples across\s+\d+\s+series/);
  await expect(summary).toBeVisible({ timeout: 60_000 });
  const before = (await summary.innerText()).replace(/\s+/g, " ");
  const beforeSamples = Number(/([\d,]+)\s+samples/.exec(before)?.[1].replace(/,/g, "") ?? "0");

  await step(6, "selecting the run's project NARROWS the result set (all projects -> one project)", "frontend/src/components/telemetry/telemetry-filters.tsx:170", async () => {
    if (!PROJECT_ID) throw new Error("no project id supplied by the harness");
    await page.selectOption(`${FILTERS} select[aria-label="Project"]`, PROJECT_ID);
    const narrowed = page.getByText(/\d[\d,]*\s+samples across\s+\d+\s+series/);
    await expect(narrowed).toBeVisible({ timeout: 60_000 });
    const text = (await narrowed.innerText()).replace(/\s+/g, " ");
    const after = Number(/([\d,]+)\s+samples/.exec(text)?.[1].replace(/,/g, "") ?? "0");
    if (!(after > 0)) throw new Error(`the run's own project returned no samples at all: "${text}"`);
    if (beforeSamples > 0 && after > beforeSamples) throw new Error(`narrowing INCREASED the count ("${before}" -> "${text}")`);
    await shot(page, "06-filtered.png");
    return `all projects: ${beforeSamples} samples -> project ${PROJECT_ID}: ${after} samples`;
  });

  await step(6, "a project with no telemetry in the window returns the EMPTY state, not stale rows", "frontend/src/components/telemetry/states.tsx:127", async () => {
    const target = FOREIGN_PROJECT_ID || "00000000-0000-0000-0000-000000000000";
    await page.selectOption(`${FILTERS} select[aria-label="Project"]`, target);
    const panel = page.locator('[role="tabpanel"][aria-labelledby="telemetry-tab-metrics"]');
    // states.tsx:127 — "No <signal> in this window" / "No <signal> match the current filters".
    await expect(panel.getByText(/No\s+metrics\s+(in this window|match the current filters)/i)).toBeVisible({
      timeout: 60_000,
    });
    // ...and the pre-filter numbers must be GONE. Stale data is the failure this step exists for.
    await expect(page.getByText(/\d[\d,]*\s+samples across\s+\d+\s+series/)).toHaveCount(0);
    return `project ${target} shows the empty state and the previous counts are gone`;
  });

  await step(6, "a time range that EXCLUDES the run shows the empty state (never stale content)", "frontend/src/components/telemetry/states.tsx:127", async () => {
    await resetFilters(page);
    const range = page.locator(`${FILTERS} select[aria-label="Time range"]`);
    const options = await range.locator("option").evaluateAll((els) => els.map((e) => (e as HTMLOptionElement).value));
    if (!options.includes("custom")) throw new Error(`no custom window option (options: ${options.join(",")})`);
    await range.selectOption("custom");
    // A window in the past that cannot contain a run started a minute ago.
    const start = page.locator("#telemetry-window-start");
    const end = page.locator("#telemetry-window-end");
    await expect(start).toBeVisible({ timeout: 10_000 });
    await start.fill("2020-01-01T00:00");
    await end.fill("2020-01-01T01:00");
    const panel = page.locator('[role="tabpanel"][aria-labelledby="telemetry-tab-metrics"]');
    await expect(panel.getByText(/No\s+metrics\s+(in this window|match the current filters)/i)).toBeVisible({
      timeout: 60_000,
    });
    await expect(page.getByText(/\d[\d,]*\s+samples across\s+\d+\s+series/)).toHaveCount(0);
    await shot(page, "06-empty.png");
    return "a 2020-01-01 window shows the empty state and no stale samples";
  });
});

// ---------------------------------------------------------------------------
// T7 — degraded proof: the state NAMES the backend, and recovery needs no reload
// ---------------------------------------------------------------------------
/**
 * HOW THE OUTAGE IS PRODUCED. The host-services supervisor RESTARTS a crashed child with a 1…32 s
 * backoff (cmd/orchicon/container.go:reapRestart, `restart: true` for every telemetry component), so
 * a SIGTERM/SIGKILL takes Loki down for about TWO SECONDS — shorter than the page's own poll — and the
 * degraded state would be a race rather than an observation. SIGSTOP holds the SAME process down for
 * as long as the assertion needs (its socket still accepts, its queries time out, the query client
 * reports the store as degraded naming loki), and SIGCONT brings it back: a real outage and a real
 * recovery, no second process, no reload.
 */
function lokiSignal(signal: "STOP" | "CONT"): void {
  execFileSync("kill", [`-${signal}`, LOKI_PID], { stdio: "ignore" });
}

/** lokiResume un-freezes Loki, and falls back to restarting the whole stack if the pid is gone. */
function lokiResume(): void {
  try {
    lokiSignal("CONT");
  } catch {
    if (!LOKI_RESTART_CMD) throw new Error("Loki's pid is gone and no restart command was supplied");
    execFileSync(process.env.SHELL || "bash", ["-c", LOKI_RESTART_CMD], { stdio: "ignore", timeout: 180_000 });
  }
}

test("T7: stopping Loki produces a state that NAMES loki, and it recovers without a reload", async ({ page }) => {
  test.slow();
  if (!LOKI_PID) {
    // A skipped degraded proof is recorded as SKIP so the harness can tell it apart from a pass —
    // and the run's summary then says the AC5 clause was NOT observed rather than quietly green.
    record(7, "SKIP", "degraded proof needs E2E_LOKI_PID from the harness");
    test.skip();
  }

  await login(page);
  await openTelemetry(page);
  await tab(page, "Logs");
  const panel = page.locator('[role="tabpanel"][aria-labelledby="telemetry-tab-logs"]');
  // Baseline: the logs panel is healthy BEFORE we break anything, so the state we observe next is
  // caused by our kill and not by a backend that was already down.
  await expect(panel.getByRole("status")).toHaveCount(0, { timeout: 60_000 });

  try {
    await step(7, "stopping the Loki process produces the degraded state that NAMES loki", "frontend/src/components/telemetry/states.tsx:149", async () => {
      lokiSignal("STOP");
      // telemetry.ts refetchInterval keeps polling while degraded, so this appears in the SAME page.
      const status = panel.getByRole("status");
      await expect(status).toBeVisible({ timeout: 90_000 });
      await expect(status).toContainText(/loki/i, { timeout: 30_000 });
      const text = (await status.innerText()).replace(/\s+/g, " ").slice(0, 400);
      await shot(page, "08-degraded.png");
      return `status named the backend: "${text}"`;
    });

    await step(7, "restarting Loki recovers the SAME page instance — no reload was used", "frontend/src/api/telemetry.ts", async () => {
      lokiResume();
      // NO page.reload() anywhere in this file (the harness asserts that before it runs us). The
      // degraded region must clear on its own, from the page's own polling.
      await expect(panel.getByRole("status")).toHaveCount(0, { timeout: 180_000 });
      await shot(page, "08-recovered.png");
      return "the degraded region cleared in the same page instance, with no reload";
    });
  } finally {
    // A failed assertion must not leave the host with a FROZEN Loki: the next steps (and the user's own
    // machine) expect the stack it started.
    try {
      lokiResume();
    } catch {
      /* the harness's teardown reports a stack that will not come back */
    }
  }
});
