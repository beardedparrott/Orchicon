# QA — E2E proof: local run with no engine → opt in → same workflow in a container

QA step (step 4 of 5) for the acceptance procedure in `docs/runtime-e2e-acceptance.md` /
`scripts/runtime-e2e.sh`. The deliverable of this item is the PROCEDURE (AC10: it files
defects against siblings, it patches no product code), so QA's job is: does the procedure
actually discriminate — i.e. does every assertion PASS on a correct system and FAIL on a
broken one — and are the parts that can run here green?

## What this host can and cannot prove

This runtime container has **no container engine** (docker/podman/nerdctl/docker-compose
all absent) and **no root**, so step 2's provisioning (a system-package install) and every
step that needs a real container (3–8) cannot be executed here. That is a property of the
QA host, not of the change: the harness refuses to pretend otherwise (it exits 2 with a
validity reason rather than manufacturing a pass). Everything that does not need an engine
was executed for real, and the two rewritten assertions were additionally driven against a
STUB engine so their discriminating power is observed, not asserted.

| # | claim | executed here | verdict |
|---|---|---|---|
| 0 | offline honesty gate for the harness (`scripts/tests/runtime-e2e-assertions/run.sh`) | yes | **PASS — 91/91** |
| 0 | preflight rejects an invalid host (ports held) with exit 2, no defect row | yes | **PASS** |
| 0 | preflight accepts an engine-free host with free ports (positive path) | yes | **PASS** (1 SKIP: node_modules) |
| 4 | pristine-reset probe: pristine → no defect; leaked state → defect row | yes (stub engine) | **PASS** both directions |
| 8 | all-native absence needle: no container → clean; container created → defect | yes (stub engine) | **PASS** both directions |
| 1–8 | the real session on a provisioned engine host | **no** | UNPROVEN here (needs an engine host) |

## Defects found and fixed in the harness (a harness that red-lights a correct system

…is worth as little as one that green-lights a broken one. Six assertions of that class
were found and fixed; the offline gate grew from 80 to **91** assertions so none can be
edited back.)

1. **False FAIL, AC1 — a malformed Connect path.** `getproject` was requested at
   `/orchicon/api/v1.ProjectService/GetProject` (slashes instead of dots). That is a 404
   whose error body is non-empty, so `gated_contains … EXECUTION_MODE_LOCAL` would FAIL on
   a correct system AND name `internal/scheduler/bridge.go` as the owner. Fixed to
   `/orchicon.api.v1.…`; the gate now asserts **every** `api_post` path is well-formed.
2. **False FAIL, AC4 — pristine reset asserted container IDENTITY.** `pool.go:303-332`
   relocates the reset container under the SAME pool name (`poolName(envKey)`), so "run B
   was served by a container that is not exactly run A's lease" is false on a correct
   system; worse, the fallback (`tail -1` of `ps`) could probe a container that never held
   the state and PASS vacuously. Step 4 now probes **every** candidate (run A's container
   and any new one) for up to 60s (a reset lands on release), and records each probe.
3. **Missing assertion, AC4 — run B itself was never checked.** Step 4 now asserts run B
   reached `WORKFLOW_RUN_STATUS_COMPLETED` before the probe means anything.
4. **Vacuous PASS, AC8 — the absence needle could never exist.** The assertion searched
   `08-all-native.txt` for `created: <name>`, a string that file never contains, so it
   always PASSed. The needle is now written into the capture only when the engine's own
   before/after diff found a new container.
5. **False FAIL, AC6 — the project image was CLEARED when no tag was given.** Posting an
   empty `default_runtime_image` clears the project default (`project.proto:177-179`), so
   `--image-id` without `--custom-image` silently un-set the image; the update is now sent
   only when the tag is known.
6. **False defect, step 0 — machine state filed against a sibling.** A host without
   `frontend/node_modules` produced a FAIL row owned by `Makefile fe-install`, and
   `--preflight-only` still exited 0 with `failures=1`. The harness never drives the UI
   (step 6 takes `--image-id`), so that row is now a SKIP with the real reason; too little
   disk now makes the run INVALID (exit 2), like a held port.
7. **False FAIL risk, AC5 — a run that never started blamed the pool.** Step 5 now asserts
   the changed-host-input run was accepted before diffing containers.

## Assertions re-verified against the product (satisfiability of the needles)

Every needle the harness greps for was confirmed present in the owning source:
`installPhase("checking the container engine")` / `"verifying the container engine — running
trivial container"` / `"container engine ready"` (`cmd/orchicon/install_engine.go:100,
150,160`), `"runtime image already up to date"` (`internal/runtimeimage/service.go:366`),
`"image not allowed"` (`internal/runtime/daemon.go:549`), `"workflow runtime ensured"`
(`internal/runtime/lifecycle.go:983`), `--label orchicon.workflow`
(`internal/runtime/engine.go:202`), `ORCHICON_ENGINE_VERIFY_IMAGE`
(`cmd/orchicon/install_engine.go:151`). Proto field names were checked too: `executionMode`,
`gitStrategy`, `defaultRuntimeImage` (`project.proto:176-180`, field-mask semantics — the
harness's partial updates do not clobber each other), `runContext` and `steps` as JSON
string (`workflow_service.proto:180-193,300-305`), `runtime_image` on `WorkflowRun`
(`workflow.proto:97`) — the AC6 pin assertion is therefore satisfiable.

## Evidence (AC9)

`qa-evidence/runtime-e2e/20261017T170552Z/` — `00-offline-honesty-gate.log` (91/91),
`01-preflight-ports-held.log` (exit 2 + the reason), `02-preflight-free-ports.log` (valid
host, one SKIP), `03-stub-engine-sim.log` + `03-stub-engine-sim.sh` (the four stub-engine
cases: pristine, leaked, no-container, container-created), `04-dry-run-plan.txt`,
`results.tsv`, `defects.tsv` (empty by design — no product defect was observed; AC10 files
sibling defects, it does not manufacture them).

## Not proven here, and what remains for the operator

Steps 1–8 need a host with no engine that the operator may install into (root, or
macOS/Windows). Run `scripts/runtime-e2e.sh` there; it exits 2 on an invalid host, 1 on a
real failure with a `defects.tsv` row naming the owning `file:line`, 0 only when the same
workflow completed locally with no engine and then inside a container.

## AC8 contention (unchanged, filed rather than reinterpreted)

`internal/runtime/lifecycle.go:958-975` creates a container for EVERY runtime-mode run, so
"an all-native workflow creates no container" holds only in local mode
(`scheduler/bridge.go:60-68`). Step 8 measures the local-mode half and records the
runtime-mode half as a defect against that file — it is never reinterpreted into a pass.
