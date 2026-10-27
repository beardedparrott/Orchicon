# Runtime-container E2E acceptance — harness run 20261017T170552Z

- passes: 4   failures: 0   skips: 1   (a SKIP is NOT a pass)
- marker: `RUNTIME-E2E-20261017T170552Z-18920` (unique per run, so a match cannot be a leftover)
- instance: `dev` (pg 15432 / nats 14222 / control 18080)
- project: `<none>`  workflow: `<none>`

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
date:      2026-10-17T17:05:52Z
host:      Linux 26fd589386f6 7.2.0-1-cachyos #1 SMP PREEMPT_DYNAMIC Thu, 20 Aug 2026 06:06:30 +0000 x86_64 GNU/Linux
hostname:  26fd589386f6
repo:      <repo>/.orchicon-worktrees/01M55C4TG8BMKRR7TGCQ9C9T0F
marker:    RUNTIME-E2E-20261017T170552Z-18920
engines:   none found (GOOD — this is the claim being proven)
wanted:    podman
head:      2a0bc0d00f11ae7b3c12c0ab246414518de91e7f
branch:    e2e-proof-local-run-with-no-engine-opt-in-same-workflow-in-a-container-bmkrr7tgcq9c9t0f
dirty:
   M scripts/runtime-e2e.sh
   M scripts/tests/runtime-e2e-assertions/run.sh
  ?? qa-evidence/runtime-e2e/20261017T170552Z/
go:        go version go1.26.4 linux/amd64
node/npm:  v22.23.2 / 10.9.8
frontend:  node_modules MISSING (the Runtime Images page cannot be driven)
ports:     dev pg=15432 nats=14222 control=18080
held now (must be empty):
disk:
  Filesystem     1048576-blocks   Used Available Capacity Mounted on
  /dev/nvme0n1p7         597952 395155    185645      69% <repo>
```

## failures — each names the file that owns it (AC10)

none.

## evidence files

- 00-preflight.log
- defects.tsv
- results.tsv
- step-0.log
- summary.md
