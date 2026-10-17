# QA report — no-container-engine developer session (acceptance)

Status: **template — the QA step executes the run and fills every `_TODO_` below.**
Procedure: [`docs/no-engine-e2e-acceptance.md`](docs/no-engine-e2e-acceptance.md).
Harness: [`scripts/no-engine-e2e.sh`](scripts/no-engine-e2e.sh).

This report is the evidence half of the acceptance criteria. It is filled **from the
harness's own evidence files only** (`results.tsv`, `defects.tsv`, the per-step logs and the
independent captures), never from recollection and never from what a script claimed about
itself. This feature **files** defects; it does not patch a sibling's code (AC11).

## 1. Verdict

One line per acceptance criterion. `evidence` names the file under
`qa-evidence/no-engine-e2e/<UTC>/` that proves it.

| AC | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Every documented step works on a clean checkout with no container engine | _TODO_ | `00-preflight.log` (engine-absence), `step-1.log` … `step-9.log` |
| 2 | `make run` works against locally-started services; `make migrate` / `make rls-check` work against them | _TODO_ | `step-1.log`, `01-healthz.txt`, `02-migrate.log`, `02-migrate-idempotent.log`, `02-rls.log` |
| 3 | A code change is verifiably live after the documented rebuild path; the stale-`dist` mode does not occur | _TODO_ | `04-served-bundle-after.txt` (marker present), `04-served-bundle-stale-control.txt` (marker absent), `04-served-bundle-force-fe.txt` |
| 4 | Status reports real state (instance, up/down, healthy) and never claims unverified health | _TODO_ | `05-status-script.txt` beside `05-status-groundtruth.txt` |
| 5 | Logs are reachable through the documented command | _TODO_ | `06-logs.txt`, `06-plane-log.txt`, `06-workaround-run-log-tail.txt` |
| 6 | Two instances coexist and are independently controllable | _TODO_ | `step-7.log`, `07-prod-run.log`, both `/healthz` codes in `step-7.log` |
| 7 | Cleanup prunes only Orchicon's artifacts and is a safe no-op otherwise | _TODO_ | `step-8.log`, `08-share-before.txt`, `08-share-after.txt`, sentinel check |
| 8 | Stopping leaves no orphaned service process | _TODO_ | `09-ports.txt`, `09-services-status.txt` |
| 9 | At no point is a container command (or source reading) needed to know what is running | _TODO_ | `10-blindspots.md` |
| 10 | Evidence is attached (commands, output, `/healthz`, process state, the live UI change) | _TODO_ | the whole evidence directory |
| 11 | Any failure is filed as a defect against the owning sibling task, not patched here | _TODO_ | `defects.tsv` → issues (list the issue numbers) |

## 2. Environment

- Run at: _TODO_ (UTC stamp; matches the evidence directory name)
- Host / kernel: _TODO_ (from `00-preflight.log`)
- HEAD sha and branch: _TODO_
- **No container engine:** _TODO_ (paste the `command -v docker podman nerdctl docker-compose`
  result from `00-preflight.log` — all four absent is the validity precondition)
- Ports: `_TODO_` (the instance ports used, and whether overrides were needed because the
  host already had something bound)
- Skips: _TODO_ (list every `SKIP` from `results.tsv` with its reason — a SKIP is a gap in
  the proof, not a pass)

## 3. Steps 1-10

For each step: the documented command, an excerpt of what it printed, the **independent**
evidence that agreed (or disagreed) with it, and the verdict. Copy the excerpt from the
evidence file; do not paraphrase it.

### Step 1 — cold start
Command: `make build` → `make services` → `make run`
Observed: _TODO_
Independent: `GET /healthz` = _TODO_ (`01-healthz.txt`), port owner pid = _TODO_
Verdict: _TODO_

### Step 2 — migrate + rls-check
Command: `make migrate` (twice), `make rls-check` (with `psql` masked)
Observed: _TODO_
Independent: the second migrate was a no-op = _TODO_; the gate printed `RLS gate OK` and not
`RLS gate SKIPPED` = _TODO_
Verdict: _TODO_

### Step 3 — connect
Command: `bin/orch --url http://127.0.0.1:_TODO_ --token …` under a PTY
Observed (screen capture): _TODO_
Independent: no `dial tcp` / `connection refused` in the capture = _TODO_
Verdict: _TODO_

### Step 4 — change and rebuild, verifiably live
Marker: _TODO_ (from `summary.md`)
Observed: marker present in the **served** bundle after rebuild = _TODO_
Adversarial stale-`dist` control: `frontend/dist/index.html` forced 1h into the future; the
served bundle _TODO_ (correct = the marker is gone)
Escape hatch: `make build force-fe=1` clears it = _TODO_
Working tree restored = _TODO_
Verdict: _TODO_ (AC3)

### Step 5 — state
Command: `scripts/orchicon.sh status _TODO_inst`
Observed: _TODO_ (paste `05-status-script.txt`)
Independent: `05-status-groundtruth.txt` — `/healthz` code, listening ports, socket owners,
`services status` output = _TODO_
Verdict: _TODO_ (AC4)

### Step 6 — logs
Command: `timeout 10 scripts/orchicon.sh logs _TODO_inst`
Observed: _TODO_
Independent: does the plane log file exist at `~/.local/share/orchicon-<inst>/serve/logs/orchicon.log`
and does its tail show steps 1-4? _TODO_
Verdict: _TODO_ (AC5)

### Step 7 — two instances
Commands: prod alongside dev, `make migrate instance=prod`, then stop dev only
Observed: _TODO_
Independent: both `/healthz` codes, per-instance ports, distinct DSNs = _TODO_
Verdict: _TODO_ (AC6)

### Step 8 — hygiene
Commands: `make clean-docker` twice, with a sentinel outside Orchicon's artifacts
Observed: _TODO_ (exit codes, any prune message)
Independent: `08-share-before.txt` vs `08-share-after.txt`; sentinel intact = _TODO_
Verdict: _TODO_ (AC7)

### Step 9 — stop
Commands: stop both planes, `make services-down` for both instances
Observed: _TODO_
Independent: `09-ports.txt` — every instance port free, no orphaned postgres/nats = _TODO_;
`services status` exit code = _TODO_
Verdict: _TODO_ (AC8)

### Step 10 — no blind spots
Table: see `10-blindspots.md`.
Any cell knowable only from source or a container command = _TODO_
Verdict: _TODO_ (AC9)

## 4. Defects

One row per failure. Every row is **filed** (never fixed here) against the sibling task that
owns the file. The owner `file:line` comes from `defects.tsv`; add the issue number once the
DevOps step files it.

| # | Symptom the developer hits | Owning file / sibling | Evidence | Issue |
|---|---|---|---|---|
| _TODO_ | _TODO_ | _TODO_ | _TODO_ | _TODO_ |

Predicted by the plan, confirm or refute each with this run's evidence:

| # | Predicted defect | Confirmed? | Evidence |
|---|---|---|---|
| D1 | `scripts/orchicon.sh logs` needs an engine and shows the container's log, not the host plane's (`scripts/orchicon.sh:127`, `scripts/container.sh:logs_instance`, required source `scripts/container.sh:478`) | _TODO_ | `06-logs.txt` |
| D2 | `scripts/orchicon.sh status` false-reports "not created" with no engine and never reports the services half (`scripts/container.sh:1603-1630`) | _TODO_ | `05-status-script.txt` |
| D3 | the host-plane launcher requires a docker bridge, so `scripts/orchicon.sh start` is not a valid cold start (`scripts/container.sh:bridge_ip`) | _TODO_ | runbook decision 1 |
| D4 | `make clean-docker` is not engine-aware (`Makefile:413-416`) | _TODO_ | `step-8.log` |
| D5 | `make run` is foreground and writes no plane log (`Makefile:272-278`) | _TODO_ | `06-plane-log.txt` (absent) |
| D6 | only `container-rebuild` forces `force-fe=1`, so the stale-`dist` mode is still reachable on the engine-free rebuild path (`Makefile:569-581`) | _TODO_ | `04-served-bundle-stale-control.txt` |

## 5. Not-blind evidence

Paste `10-blindspots.md`: for each step, the documented command whose output alone answers
"what is running, and why", plus the independent evidence that agreed with it.

_TODO_

## 6. Known limits of this run

- _TODO_ (e.g. `psql` masked for step 2 so the downloaded-psql rung is the one proven; port
  overrides used because the host already had something bound; step 3 SKIP reason)
- Nothing in this run patched a sibling's file. Every failure above is filed, per AC11.
