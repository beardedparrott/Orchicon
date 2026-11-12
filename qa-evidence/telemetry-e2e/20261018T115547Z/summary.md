# Telemetry page with Grafana absent — harness run 20261018T115547Z

- claim under test: **the Telemetry page works, from real telemetry, on a machine with no Grafana**
- instance: `dev`   plane: `http://127.0.0.1:8080`   binary: `/tmp/orchicon/telemetry-e2e/bin/orchicon`
- real run: project `?` / run `?` / execution `?` / correlation `?` / trace `?`
- stores: tempo `http://127.0.0.1:3200`   loki `http://127.0.0.1:3100`   victoria-metrics `http://127.0.0.1:8428`
- passes: 4   failures: 1   skips: 0   blocked: 9

**NOT A VALID PROOF:** telemetry.tsx no longer renders <GrafanaEmbed> (owner: origin/build-the-native-telemetry-page-graphs-reports-search-lookup-filter-yhxnxf4c6m9b9023:frontend/src/routes/telemetry.tsx)

## verdict

**The claim is NOT proven.** telemetry.tsx no longer renders <GrafanaEmbed> (owner: origin/build-the-native-telemetry-page-graphs-reports-search-lookup-filter-yhxnxf4c6m9b9023:frontend/src/routes/telemetry.tsx)
No test was satisfied; see `defects.tsv` for the sibling task that owns each gap.

## per-step verdict

| step | what it proves | verdict |
|---|---|---|
| 0 | validity precondition: the sibling surfaces exist on this branch | BLOCKED (see defects.tsv) |
| 1 | AC1 — Grafana is genuinely absent (binary/process/image/config + /grafana 404) | **FAIL** (see defects.tsv) |
| 2 | the telemetry stack runs as HOST processes (Tempo, Loki, VictoriaMetrics, collector) | not run |
| 3 | AC2 — a REAL workflow run was created, started and completed | not run |
| 4 | AC2/AC3 — the stores hold THIS run, queried directly, and the plane returns the same ids | not run |
| 5 | AC3 — graphs, log search, trace view and report render from the stores | not run |
| 6 | AC4 — the project filter narrows, and an excluding window shows the EMPTY state | not run |
| 7 | AC5 — a stopped backend is NAMED, and recovery happens without a reload | not run |
| 8 | teardown releases what this run started | not run |

## evidence (independent of the page's own word)

- `01-grafana-absent.log` + `01-grafana-absent-route.log` — the absence probes, including the
  control pattern that had to match before an empty `pgrep` was allowed to mean "none".
- `02-stack-up.log` — `ORCHICON_TELEMETRY=embedded orchicon start` plus the `/proc/<pid>/exe` residency probe per backend.
- `03-*.json` — the run as the plane's own API reported it (project, work item, workflow, run, poll).
- `04-stores/*.json` — Tempo, Loki and VictoriaMetrics queried DIRECTLY, plus the plane's own
  QueryTraces/GetTelemetryReport answers for the same window.
- `05-playwright.out`, `browser-results.tsv`, `05-*.png` … `08-*.png` — the browser half:
  real clicks, real screenshots, and the request log of what the page actually fetched.
- `run.env` — the ids every later probe keys on.

## defects — each names the sibling task that owns the fix (AC7)

| step | assertion | owner |
|---|---|---|
| 0 | telemetry.tsx no longer renders <GrafanaEmbed> [BLOCKED: the page under test is still the Grafana iframe embed] | `origin/build-the-native-telemetry-page-graphs-reports-search-lookup-filter-yhxnxf4c6m9b9023:frontend/src/routes/telemetry.tsx` |
| 0 | telemetry.tsx no longer imports GRAFANA_UI_URL [BLOCKED: the /grafana proxy URL is still wired into the page] | `origin/remove-grafana-from-the-plane-proxy-iframe-config-proc-port-2qnyk4c19p8csyj5:frontend/src/routes/telemetry.tsx` |
| 0 | native telemetry component graphs.tsx exists [BLOCKED: the component the page renders is not in the branch under test] | `origin/build-the-native-telemetry-page-graphs-reports-search-lookup-filter-yhxnxf4c6m9b9023:frontend/src/components/telemetry/graphs.tsx` |
| 0 | native telemetry component reports-panel.tsx exists [BLOCKED: the component the page renders is not in the branch under test] | `origin/build-the-native-telemetry-page-graphs-reports-search-lookup-filter-yhxnxf4c6m9b9023:frontend/src/components/telemetry/reports-panel.tsx` |
| 0 | native telemetry component states.tsx exists [BLOCKED: the component the page renders is not in the branch under test] | `origin/build-the-native-telemetry-page-graphs-reports-search-lookup-filter-yhxnxf4c6m9b9023:frontend/src/components/telemetry/states.tsx` |
| 0 | native telemetry component telemetry-filters.tsx exists [BLOCKED: the component the page renders is not in the branch under test] | `origin/build-the-native-telemetry-page-graphs-reports-search-lookup-filter-yhxnxf4c6m9b9023:frontend/src/components/telemetry/telemetry-filters.tsx` |
| 0 | the GetTelemetryReport RPC exists (the Reports tab's data path) [BLOCKED: no report RPC, so the report cannot be read from a real window] | `origin/build-the-native-telemetry-page-graphs-reports-search-lookup-filter-yhxnxf4c6m9b9023:proto/orchicon/api/v1/telemetry_service.proto` |
| 0 | the responses carry degraded_reason (the state must NAME the backend) [BLOCKED: a degraded state that cannot name its backend fails AC5] | `origin/build-the-native-telemetry-page-graphs-reports-search-lookup-filter-yhxnxf4c6m9b9023:proto/orchicon/api/v1/telemetry_service.proto` |
| 1 | no grafana config / provisioning file ships in the repo (deploy/**) | `origin/remove-grafana-from-the-plane-proxy-iframe-config-proc-port-2qnyk4c19p8csyj5:deploy/configs/grafana.ini` |

## evidence files

- 00-preflight.log
- 01-grafana-absent.log
- defects.tsv
- results.tsv
- run.env
- summary.md
