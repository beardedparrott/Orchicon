# Telemetry page E2E evidence — `scripts/telemetry-e2e.sh`

This directory holds the evidence a run of `scripts/telemetry-e2e.sh` emits. The claim it supports is
the epic's outermost one: **on a machine with no Grafana installed, `/telemetry` shows graphs, log
search, a trace view and a report built from telemetry that real work produced — with filters that
narrow, a window that can honestly come back empty, and a degraded state that names the backend it
lost and recovers live.**

Nothing here is a screenshot pile. Every artifact below is produced by an INDEPENDENT probe, and the
page is only ever allowed to CONFIRM what the stores already said.

## Re-running it

```bash
# 1. the subject must carry BOTH sibling branches (native page + Grafana removal).
#    If it does not, the harness exits 2 / BLOCKED and files a defect against the sibling —
#    it does NOT go green. See "the gate" below.
git switch e2e/telemetry-proof          # develop + native-telemetry page + grafana removal

# 2. one command, all steps (it builds a scratch `orchicon` binary if you do not pass --bin)
scripts/telemetry-e2e.sh --instance dev

# useful partial runs
scripts/telemetry-e2e.sh --dry-run          # print the plan, execute nothing
scripts/telemetry-e2e.sh --preflight-only   # only the "is this branch a valid subject" gate
scripts/telemetry-e2e.sh --skip-browser     # AC1 + store truth (no browser needed)
scripts/telemetry-e2e.sh --run-driver ./my-run.sh   # use your own real, worker-backed run
```

Exit codes: `0` every assertion passed · `1` at least one assertion FAILED (defect named with its
`file:line`) · `2` NOT a valid proof (a surface is absent, the host cannot run the stack, or a store
never answered). A run that cannot fail is not a proof, so `2` and `1` are both "the claim is not
proven".

Environment: `ORCHICON_E2E_USER` / `ORCHICON_E2E_PASSWORD` (local-login for the Connect driver),
`ORCHICON_E2E_ROOT` (scratch dir), `ORCHICON_E2E_POLL_SECONDS` (run-completion budget, default 600).

## What each artifact proves

| file | proves |
|---|---|
| `00-preflight.log` | the sibling surfaces this claim is ABOUT exist on the branch under test (native page with no `GrafanaEmbed`/`GRAFANA_UI_URL`, the telemetry components, the query client, the `GetTelemetryReport` RPC, `degraded_reason`). Missing ⇒ BLOCKED + defect row. |
| `01-grafana-absent.log` | **AC1:** no `grafana` binary on `PATH`, no grafana process (behind a CONTROL pattern that had to match first, so an empty result means "none" and not "the search is broken"), no grafana config under `deploy/`, no grafana container image. |
| `01-grafana-absent-route.log` | **AC1:** the plane's `/grafana` route answers **404** while `/healthz` answers 200 — the proxy/iframe really is gone, not merely unreferenced. |
| `09-grafana-absent-final.log` | **AC1 re-checked at the end:** a grafana that appeared mid-run voids the claim. |
| `02-stack-up.log` | the telemetry stack runs as **host processes**: `orchicon start` (NOT `services up`, which deliberately runs with `ORCHICON_TELEMETRY=none`) plus Tempo `:3200`, Loki `:3100`, VictoriaMetrics `:8428`, otelcol `:4317`, each proven by the pid that owns the listening socket and by `/proc/<pid>/exe`. The store ports come from the INSTANCE's own `ports.json`, so `--instance prod` probes prod's ports. A containerised backend is not host-resident and fails this step. |
| `03-*.json`, `03-run-driver.out` | **AC2:** the run is real — a LOCAL-mode project, a work item, a workflow published and started through the plane's own Connect API, and the poll that shows it reached a terminal state. Not a seeded row. |
| `04-stores/*.json` | **AC2/AC3 (anti-fixture):** Tempo, Loki and VictoriaMetrics queried **directly**, bypassing Orchicon, plus the plane's own `QueryTraces`/`GetTelemetryReport` answers for the same window. The ids the browser leg asserts on come from here. |
| `05-playwright.out`, `browser-results.tsv`, `05-*.png`, `05-network.log` | **AC3:** Overview → Metrics (a real, non-zero sample count and latest value) → Logs (a search for the run's own id returns the run's lines) → Traces (the run's execution) → Reports (the window summary), with the page's actual requests and responses captured beside the screenshots. |
| `06-filtered.png`, `06-empty.png`, `browser-results.tsv` step 6 | **AC4:** selecting the run's project NARROWS the result set (and never increases it), a project with no data shows the empty state, and a window that excludes the run shows the **empty** state with the pre-filter counts GONE — stale content fails this step. |
| `08-degraded.png`, `08-recovered.png`, `browser-results.tsv` step 7 | **AC5:** stopping Loki produces the `role="status"` degraded state that **names loki**, and bringing it back clears that state **in the same page instance**. The outage is a `SIGSTOP` and the recovery a `SIGCONT` on the SAME pid: the host-services supervisor restarts a crashed child within ~2s (`cmd/orchicon/container.go:reapRestart`), so a SIGKILL would make the outage shorter than the page's own poll and the assertion a race. The harness greps the spec for `reload(` before running it, so "no reload masked the recovery" is a measured fact. |
| `results.tsv` | `step · PASS/FAIL/BLOCKED/SKIP · observation · evidence-file` for every assertion. |
| `defects.tsv` | `step · assertion · owning sibling path:line` — this harness patches nothing (AC7); it accuses the task that owns the fix. |
| `summary.md` | the verdict, the per-step table, and the honest sentence: proven, or NOT proven and why. |
| `run.env` | the ids every probe keys on (project, foreign project, run, execution, correlation, trace, Loki pid, the VM metric and its value). |

## The gate (why this directory may be absent)

`scripts/telemetry-e2e.sh` refuses to report a proof for a branch that does not carry the feature. On
`develop` today the Telemetry page is still the Grafana iframe embed (`GrafanaEmbed`,
`GRAFANA_UI_URL`), there are no filters (so AC4 cannot be exercised at all), and `deploy/configs`
still ships Grafana's config — the native page and the Grafana removal both live on unmerged
branches. A run there emits **no evidence directory with a green verdict**: it exits `2` with
`defects.tsv` rows naming each missing sibling. That is the correct outcome, and it is why this
directory contains a README and not a fabricated run.

## Reproducing the browser half alone

```bash
cd frontend
E2E_EVIDENCE=<dir> E2E_PLANE_URL=http://127.0.0.1:<plane-port> \
E2E_PROJECT_ID=… E2E_RUN_ID=… E2E_EXECUTION_ID=… E2E_TRACE_ID=… \
E2E_LOKI_PID=… E2E_LOKI_RESTART_CMD='<bin> stop --instance dev && ORCHICON_TELEMETRY=embedded <bin> start --instance dev' \
  npx playwright test tests/telemetry-e2e.spec.ts --config playwright.telemetry-e2e.config.ts \
    --project=dark-desktop --workers=1
```

That config exists because the page under test is **the plane's own origin** (`cmd/orchicon/serve_state.go`
serves the SPA, and `/auth/local-login` sets its cookie on that same origin). The shared
`playwright.config.ts` would load the SPA from the vite dev server on `:5173`, whose proxy targets a
hard-coded `:8080` — a different plane, with a session cookie the page cannot even send.
