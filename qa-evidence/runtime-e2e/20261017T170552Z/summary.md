# QA run — runtime-container E2E acceptance harness (2026-10-17T17:05:52Z)

Full write-up: `qa-report-runtime-container-e2e.md` (repo root).

- offline honesty gate: **91/91 PASS** (`00-offline-honesty-gate.log`)
- preflight on THIS host (ports 5432/4222/8080 held): **exit 2, run invalid**
  (`01-preflight-ports-held.log`) — the abort names the host condition, files no defect
- preflight with free ports: **valid, 4 PASS, 1 SKIP** (node_modules missing; the harness
  never drives the UI) (`02-preflight-free-ports.log`)
- stub-engine simulation of the two rewritten assertions (`03-stub-engine-sim.sh`):
  - step 4 pristine container → 4 PASS, 0 defects
  - step 4 leaked state → 4 defects (the assertion discriminates)
  - step 8 no new container → no needle, 0 defects
  - step 8 new container → needle written, 2 defects (the needle is reachable)
- real session steps 1–8: **UNPROVEN on this host** — no container engine and no root, so
  provisioning (step 2) and every container-reading step cannot run here.
- product defects observed: **none** (`defects.tsv` carries only its header).

Six false-verdict assertions were fixed and pinned by the gate (80 → 91 assertions); see
`qa-report-runtime-container-e2e.md` for the list and the product-side re-verification.
