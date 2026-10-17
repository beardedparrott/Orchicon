# Runtime-container E2E acceptance — harness run 20261017T165234Z

- passes: 2   failures: 0   skips: 1   (a SKIP is NOT a pass)
- marker: `RUNTIME-E2E-20261017T165234Z-2268` (unique per run, so a match cannot be a leftover)
- instance: `dev` (pg 5432 / nats 4222 / control 8080)
- project: `<none>`  workflow: `<none>`

**RUN INVALID:** port(s) 5432 4222 8080 are already held, so this run cannot prove that the instance's OWN plane is what it drove. Free them, or shift the run with --pg-port/--nats-port/--control-port.

## per-AC verdict

| AC | verdict |
|---|---|
| 0 | SKIP |
| 1 | not run |
| 2 | not run |
| 3 | not run |
| 4 | not run |
| 5 | not run |
| 6 | not run |
| 7 | not run |
| 8 | not run |

## evidence (independent ground truth)

```
date:      2026-10-17T16:52:34Z
host:      Linux 26fd589386f6 7.2.0-1-cachyos #1 SMP PREEMPT_DYNAMIC Thu, 20 Aug 2026 06:06:30 +0000 x86_64 GNU/Linux
hostname:  26fd589386f6
repo:      /home/beardedparrott/projects/Orchicon/.orchicon-worktrees/01M55C4TG8BMKRR7TGCQ9C9T0F
marker:    RUNTIME-E2E-20261017T165234Z-2268
engines:   none found (GOOD — this is the claim being proven)
wanted:    podman
head:      25714ae3c2b1542c89b833d7fe1c931b0f12dd6b
branch:    e2e-proof-local-run-with-no-engine-opt-in-same-workflow-in-a-container-bmkrr7tgcq9c9t0f
dirty:
   M Makefile
   M docs/no-engine-e2e-acceptance.md
  ?? docs/runtime-e2e-acceptance.md
  ?? qa-evidence/runtime-e2e/
  ?? scripts/runtime-e2e.sh
  ?? scripts/tests/runtime-e2e-assertions/
go:        go version go1.26.4 linux/amd64
node/npm:  v22.23.2 / 10.9.8
frontend:  node_modules MISSING (the Runtime Images page cannot be driven)
ports:     dev pg=5432 nats=4222 control=8080
held now (must be empty):
  5432 HELD
  4222 HELD
  8080 HELD
disk:
  Filesystem     1048576-blocks   Used Available Capacity Mounted on
  /dev/nvme0n1p7         597952 395102    185666      69% /home/beardedparrott/projects/Orchicon
```

## failures — each names the file that owns it (AC10)

none.

## evidence files

- 00-preflight.log
- defects.tsv
- results.tsv
- step-0.log
- summary.md
