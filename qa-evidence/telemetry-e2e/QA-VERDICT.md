# QA pass — `scripts/telemetry-e2e.sh` (Grafana-absent Telemetry page)

Run by the QA step of work item `01M57D7D8EB1M023F515EAMAE4`, inside the disposable
`orchicon-runtime:orchicon-dev` container (no root, no host of its own).

## What was actually executed

| run | subject | command | result |
|---|---|---|---|
| plan | run branch | `scripts/telemetry-e2e.sh --dry-run` | exit 0 — step plan printed |
| gate | run branch (`develop` @ e95924e91 + the harness) | `scripts/telemetry-e2e.sh --preflight-only` | **exit 2 / BLOCKED** — 8 surface rows naming the two sibling branches + 1 host row (no `frontend/node_modules`); see `20261018T115547Z/` |
| gate | integrated subject (`origin/build-the-native-…` + `origin/remove-grafana-…` + this harness) | `scripts/telemetry-e2e.sh --preflight-only` | **exit 0** — every surface present, and all five AC1 host-level absence probes PASS; see `20261018T115539Z-surfaces-present/` |

The integrated subject is a THROWAWAY local branch (`e2e-telemetry-integration`, never pushed,
removed after the run). It exists only to prove the gate is not a blanket refusal: it goes green
exactly when the two sibling branches are merged, and red (with named owners) when they are not.

## Acceptance criteria, as far as this host can honestly take them

- **AC1 (Grafana absent)** — PASS on the integrated subject, by independent probes: no `grafana*`
  binary on `PATH`; the process search proves itself with a control pattern before any empty result
  counts; no `grafana` process; no `deploy/**` grafana config; no grafana container image. The
  `/grafana` → 404 half needs a running plane and was **not** exercised here (see below).
- **AC2/AC3/AC4/AC5 (real run, graphs/logs/traces/report, filters, empty state, degraded naming)** —
  **not executed**: they require Tempo, Loki, VictoriaMetrics, otelcol and a completed workflow run
  on a real host. This container ships Postgres 15 and `nats-server` but **no telemetry backends**,
  and `orchicon start` would have to download ~300 MB of them plus run a real workflow to
  completion — beyond this step's box. No verdict is claimed for them, and `summary.md` says exactly
  that ("not run"), which is the behaviour this harness is built for: an unexecuted assert is never
  a pass.
- **AC6 (evidence)** — the evidence directory is emitted per run (this file plus the two preflight
  directories). No screenshots exist, because no browser leg could run; nothing is fabricated.
- **AC7 (no patching inside this task)** — respected: the only code change made here is a text
  correction to the harness's own rendered evidence table (it named `orchicon services up`, which
  deliberately runs with `ORCHICON_TELEMETRY=none`; the harness runs
  `ORCHICON_TELEMETRY=embedded orchicon start`). No product code and no sibling assertion changed.

## Verdict

The harness is honest on this machine in both directions: it refuses a green verdict on a subject
that lacks the feature (exit 2, defect rows naming `origin/build-the-native-…` and
`origin/remove-grafana-…`), and it accepts the subject that has it (exit 0, AC1 probes green). The
epic's user-facing claim is therefore **NOT yet proven** — it can only be proven on a host that can
run the telemetry stack and a real completed workflow run, which this container is not.
