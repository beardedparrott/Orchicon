# Telemetry page with Grafana absent — harness run 20261018T115641Z

- claim under test: **the Telemetry page works, from real telemetry, on a machine with no Grafana**
- instance: `dev`   plane: `http://127.0.0.1:8080`   binary: `/tmp/orchicon/telemetry-e2e/bin/orchicon`
- real run: project `?` / run `?` / execution `?` / correlation `?` / trace `?`
- stores: tempo `http://127.0.0.1:3200`   loki `http://127.0.0.1:3100`   victoria-metrics `http://127.0.0.1:8428`
- passes: 5   failures: 0   skips: 0   blocked: 1

**NOT A VALID PROOF:** the Playwright browser harness is installed: frontend/node_modules/@playwright is absent — run 'cd frontend && pnpm install'

## verdict

**The claim is NOT proven.** the Playwright browser harness is installed: frontend/node_modules/@playwright is absent — run 'cd frontend && pnpm install'
No test was satisfied; see `defects.tsv` for the sibling task that owns each gap.

## per-step verdict

| step | what it proves | verdict |
|---|---|---|
| 0 | validity precondition: the sibling surfaces exist on this branch | BLOCKED (see defects.tsv) |
| 1 | AC1 — Grafana is genuinely absent (binary/process/image/config + /grafana 404) | PASS |
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

none.

## evidence files

- 00-preflight.log
- 01-grafana-absent.log
- defects.tsv
- results.tsv
- run.env
- summary.md
