# Orchicon

AI orchestration and operations platform that coordinates autonomous AI
work as reliable, observable, recoverable, and manageable systems.

Orchicon separates **orchestration** from **execution**: it manages
projects, workers, scheduling, policies, telemetry, recovery, and
governance, while pluggable runtimes execute the work.

> Orchicon orchestrates. Runtimes execute.

## Documentation

The comprehensive project documentation lives in
[`DOCUMENTATION.md`](./DOCUMENTATION.md) at the project root. It
covers architecture, project structure, installation, development,
deployment, troubleshooting, and every subsystem.

## Technology Stack

- **Control plane**: Go (single binary, k8s-style reconcilers)
- **Single container**: `orchicon container` runs the whole stack (Postgres, NATS, Tempo/Loki/VictoriaMetrics/Grafana, control plane) as PID 1 — see [DOCUMENTATION.md §Single-Container Deployment](DOCUMENTATION.md)
- **API**: Protobuf + Connect (gRPC + REST + streaming from one schema)
- **Database**: PostgreSQL 16 with RLS + transactional outbox
- **Event bus**: NATS JetStream
- **Telemetry**: OpenTelemetry → Grafana stack (Tempo + Loki + VictoriaMetrics) — fully separated infra
- **Policy**: Rego (Open Policy Agent)
- **Runtime adapters**: gRPC sidecars (OpenCode first)
- **Frontend**: TypeScript + React + Vite + Connect-ES

## Last Release Changes

### v0.1.181 (2026-08-04)

| Type | Change |
|---|---|
| Feature | **Rotating, size-bounded serve logs with Settings-driven management.** A run-away component previously grew `.dev/logs/orchicon.log` unbounded (observed at 300+ GB). The detached serve now writes through a rotating file writer: it rotates the active log by size (default 100 MB) or by time (default daily, whichever comes first) and prunes rotated files past the retention window (default 7 days / 7 files). The serve child re-points fds 1/2 onto the current file after each rotation so panics/stray prints stay in the log. Config precedence: **Settings → Defaults → Log management** (new `log_directory`, `log_max_size_mb`, `log_roll_interval_hours`, `log_retention_days`, `log_max_files`) live-applied every ~5s with no restart, then `ORCHICON_LOG_*` env vars, then built-in defaults. The single-container instances also pin Docker's `json-file` driver to `max-size=100m`/`max-file=7`. |
| Chore | **Disk hygiene: telemetry retention + container image + Go cache.** (1) The embedded **Loki config had NO retention** — telemetry logs accumulated forever in the instance volume; it now prunes after 14 days (`retention_period: 336h` + compactor), Tempo's trace retention is pinned to 14 days, and VictoriaMetrics stays at 30 days. (2) Repeated builds orphaned ~229 dangling images (~240 GB); `scripts/container.sh build` and the runtime daemon's custom-image build now prune dangling images after each build, and the runtime daemon's orphan sweep runs immediately at start. (3) The Go build cache grew to 35 GB; new `make clean` / `make cache-check` / `make clean-docker` targets manage it and reclaim Docker build leftovers. One-time cleanup removed the legacy SigNoz/ClickHouse images + orphaned compose-era volumes (~9 GB) and the stale pre-single-container binaries. |

### v0.1.180 (2026-08-04)

| Type | Change |
|---|---|
| Bug fix | **A stalled execution now actually recovers.** The stall monitor's `OnStall` marked an execution `unhealthy`, but the workflow reconciler's `pollTaskStep` only treated `failed`/`failed_to_start`/`terminated` as terminal-failure — so a stalled execution sat `unhealthy` forever and **no recovery ever fired** (observed on dev run `01KZ5G3JPR4R18NJRTJFKX3CXH`: the PR Reviewer hit `stalled:no_progress`, the subprocess leaked in the runtime container for 48+ minutes, and zero `recovery_executions` rows were created). Fix: a genuine hang/loop signal (`no_progress`, `text_loop`, `repetition`) now **hard-kills the subprocess** (local: `Process.Kill`; runtime: SIGKILL via the supervisor) so the execution lands in `failed` through the normal `OnResult(false)` path — which `pollTaskStep` already turns into recovery. `no_file_progress` stays **advisory-only**: a reviewer/QA worker may legitimately produce output for long stretches without touching files, so killing it would reap a healthy execution (the SSE worker flagged `no_file_progress` completed successfully moments later). The stall reason is now threaded into the terminal error message. |
| Bug fix | **The wall-clock timeout is a hard stop that completes.** `budget_overrides.wall_clock_seconds` is enforced as a `context.WithDeadline` that kills the subprocess even while the model is producing output (the runaway-spend backstop) — but the terminal DB writeback was passed the *deadline-exhausted* context, so `OnResult` failed with `context deadline exceeded` and the execution stayed `running` forever (verified live on a wall-clock E2E). The deadline is now applied only to the subprocess/exec context; the callback context stays clean so `failed` + recovery always land. Also: an **absent** `wall_clock_seconds` now defaults to **3600s** (was: no deadline), so every execution has a hard cap even a slow-but-progressing worker can't exceed; explicit `0` still disables it. Verified E2E on dev with a real free-model workflow: each execution was killed at exactly the deadline, landed `failed`, triggered recovery, re-dispatched, and the run failed cleanly at `max_attempts`. |
| Feature | **Budget defaults are now a tenant setting (with per-worker override).** The per-execution budget ceilings — `tokens`, `cost_usd`, `wall_clock_seconds`, `tool_call_count` — are now configurable under **Settings → Defaults → Execution budget**. The tenant value is the **default**; a worker's own `budget_overrides` always override it per-field (merged at dispatch: worker wins each key over the tenant default), and empty tenant fields fall back to the built-ins (millions of tokens, $10, **3600s wall clock**, 100 tool calls). The wall-clock default closes the gap where an empty `{}` budget left a long-running worker with **no hard stop at all** (the PR Reviewer ran 48+ min unchecked). New `tenant_settings.default_budget_overrides` JSON column; the reconciler merges tenant+worker budgets into `ExecutionManifest.Budgets`. `ORCHICON_STALL_WALL_CLOCK_SECONDS` env override remains. Verified end-to-end (API update → read-back round-trip + UI renders the section in Chrome). |

### v0.1.177 (2026-08-03)

| Type | Change |
|---|---|
| Bug fix | **AI approvers no longer double-dispatch.** The worker-backed approval step creates a dedicated approval work item ("ready" + assigned worker), and the standalone TaskReconciler scan dispatched it in parallel with the workflow's inline dispatch — two approver executions ran for one step, and once the run finished the orphaned ticket got dispatched again as a ghost execution (failing because its runtime container was already reaped). The scan now skips any item that belongs to a workflow run: workflow steps are dispatched exclusively by the workflow reconciler's inline (step-run) path. |
| Bug fix | **AI approval steps actually complete.** The approval step polled the approval work item's status, which under the run-bound model never tracks the approver's execution — so the step sat "running" forever even after the approver succeeded. Approval steps now poll their own execution (same as task steps), and the approve/reject decision routes exactly as before. |
| Bug fix | **A rejected approval no longer fires the success branch.** When the AI approver rejected, the loop-back left the superseded approval step "succeeded", so the downstream success-branch step dispatched in the same pass while the loop also re-ran. Superseded steps no longer satisfy dependencies, and the loop now re-creates the approval step (pending) so the work is re-approved before the success branch can fire. Verified: through 3 rejection loops the success-branch step stayed pending, then the run failed cleanly at max_iterations (which also fixed a pre-existing "step failed" loop where a rejected approval at max_iterations never committed its failure). |
| Chore | Docs updated (README, UPDATES.md). Verified E2E on dev with the free model: the approve path ran exactly 3 executions and completed; the reject path looped correctly and failed cleanly; the previously-stuck run `01KZ43ZTTFRTA2VZ7FTV56Z007` self-healed (approval completed with decision `success`, pipeline resumed). |

### v0.1.176 (2026-08-03)

| Type | Change |
|---|---|
| Feature | **Runtime adapter CLIs are mounted, never baked.** Orchicon no longer ships opencode in its container images — the operator installs it on the host and it's bind-mounted into the main + runtime containers at runtime (`~/.opencode`, read-only, bin on PATH). This keeps the product redistributable regardless of an adapter's license (Claude Code's terms, for example, prohibit bundling). The installer now verifies opencode is present and fails with a clear message otherwise; the supervisor's allowlist (`runtimeBinAllowlist`) is structured so adding `claude`/`codex` later is a one-line change. |
| Bug fix | **Runtime container creation no longer races.** The reconciler's `EnsureForRun` and the adapter's self-heal `Create` could call `docker run` for the same workflow name simultaneously — one won, the other hit "name already in use", removed the winner's container mid-setup, and the exec landed on a container being recreated (failures surfaced as `exit status 1`). Container creation is now serialized in the daemon. |
| Feature | **Execution-liveness reaper is now tunable + far less trigger-happy.** The reaper that fails executions whose runtime process is gone used to reap on a **single** not-alive probe (with a hardcoded 60s fresh-execution grace) — and the probe can false-negative on a transient docker/socket hiccup, so a healthy, actively-working execution got killed and forced into recovery. Both knobs are now tenant settings under **Settings → Execution liveness reaper**: a grace window (default 60s) and a **consecutive not-alive probes** threshold (default 3) — an execution is only reaped once it's old enough AND has been reported not-alive 3 sweeps in a row. Env overrides `ORCHICON_REAP_GRACE_SECONDS` / `ORCHICON_REAP_CONSECUTIVE_FAILURES`. |
| Feature | **Exec streams survive transient transport blips (reconnect, not kill).** A broken control-plane ↔ runtime-supervisor stream no longer fails the execution: the adapter retries the exec stream for the same `exec_id` (**reconnect attempts**, default 3), the supervisor keeps the child running through a **reconnect grace** (default 60s) instead of killing it on disconnect, and a re-attach resumes the run in place. Explicit termination (wall-clock deadline, abort, shutdown) still kills the child promptly via an explicit signal. Configured under **Settings → Execution transport resilience**; env overrides `ORCHICON_RECONNECT_ATTEMPTS` / `ORCHICON_RECONNECT_GRACE_SECONDS`. |
| Bug fix | **Steps can no longer be marked `succeeded` off a stale execution.** A workflow step that shares a bound work item with other steps (and other runs) used to fall back to "the latest execution for the work item" when its own execution link was missing — which could be a *different step's* or a *previous run's* execution, marking work done that never ran (observed: the DevOps step "succeeded" in 2ms, no execution, on the strength of an hours-old architect execution). A task step with no execution link now waits instead. |
| Bug fix | **Execution results land on the right step.** `propagateStepRunResults` used to find the step run by searching result JSON for the work item id — and since every step run is seeded with the same `created_at`, the `LIMIT 1` was arbitrary among steps sharing a bound work item, dumping an execution's summary/decision onto the wrong step's card. It now keys by `worker_execution_id`, which unambiguously identifies the step that actually ran the execution. |
| Bug fix | **Recovery can no longer deadlock the workflow reconciler.** When a task step failed, `pollTaskStep` called `TriggerOnFailure` *synchronously inside its own reconcile transaction* — and `TriggerOnFailure` opens its **own transaction on a separate connection**. If the same pass had already re-dispatched the recovering step and written the (shared) work item row, the trigger's child transaction blocked on the pass's own row lock while the pass synchronously waited for the trigger to return: a cross-connection self-deadlock Postgres cannot detect. The reconciler goroutine wedged holding the transaction, the run stayed `running`, the step stuck in `recovering`, and the runtime container was never reaped (the terminalization path lives inside the stuck pass) — observed on run `01KZ30ER243X3VT8HW8QQNGQPB` after a provider-side "Unexpected server error" killed the SSE execution. Recovery triggers are now collected during the pass and invoked **after** the transaction commits (same pattern as inline dispatch) — both `pollTaskStep` (summarize_restart) and `dispatchStep` (loop_decision) sites. |
| Bug fix | **Re-dispatched recovering steps no longer re-trigger recovery on the stale failed execution.** `dispatchStep` set a recovering step back to `running` while leaving its old *failed* `worker_execution_id` attached, so `pollTaskStep` saw the stale failure in the next inner iteration and re-triggered recovery (attempt 1→2→3 until terminal failure), racing the inline dispatch that was to link the replacement execution. The step run's `worker_execution_id` is now cleared on re-dispatch, so the step waits for the new execution link; a 15s grace guards against a lost dispatch (inline `DispatchTask` failure / adapter down), routing the step to recovery instead of hanging as `running` forever — which also fixes a pre-existing hang on a lost *first* dispatch. |
| Feature | **Work items are now shared input references during a run — parallel steps on one ticket just work.** A workflow-bound ticket goes `running` at run start and `succeeded`/`failed` at run end, and is never mutated per-step: `dispatchStep` no longer writes `assigned_worker_ref`/`workflow_step_id`/`prompt_context`/status onto the ticket, dispatch is scoped per step run (`DispatchTask(taskID, stepRunID)`), the composite prompt + worker live on the step run, and each step gets its own execution. Two steps bound to the same ticket can run in parallel (each with its own execution, results, and summary) — the earlier "only one of two simultaneously-ready steps gets an execution" race is eliminated, not serialized. Loop decisions and prompt context read from the upstream **step run's** result, not the ticket. At run end the ticket's `results` carry a run-level narrative (`_run_narrative`) aggregating every step's summary/decision/issues plus each recovery episode. |
| Feature | **Recovery is scoped per failing step run — nothing is lost.** Each failing step gets its own full 6-step recovery cycle (capture → summarize → preserve → review → plan → resume), keyed by the failed execution, so two steps failing on the same ticket each recover independently instead of one swallowing the other. The ticket is never flipped to `recovering`/`ready`; the recovery summary lands on the step run (`_recovery_summary`) so the replacement execution's prompt includes the failure context (no "same failure twice" loop). The run terminal-state check also no longer wrongly marks a run `completed` when its only step fails (a `Failed` step now also clears the all-succeeded flag). |
| Chore | Docs updated (README, DOCUMENTATION.md, AGENTS.md) for the work-item-as-input model. Verified E2E on dev: two parallel steps on one ticket each got their own execution and succeeded (real free-model output); a single-step failure ran its full recovery cycle, re-dispatched, failed again, and the run/ticket correctly ended `failed` with the recovery episode in the narrative. |
| Chore | Docs updated (README, DOCUMENTATION.md, AGENTS.md, landing page) to make the mount-never-bake licensing policy explicit. Verified E2E on dev: images contain no opencode; a single-step workflow ran through the mounted opencode in the runtime container (execution succeeded with real output, run completed, container reaped). |


## Installation

### One-line install (Linux / macOS)

```bash
curl -fsSL https://orchicon.dev/install | bash
```

The installer downloads the binary, then runs `orchicon install` to set up everything: pull the published images, start the runtime daemon, launch the single-container instance, and print how to connect / start / stop. (Pass `--no-setup` to install only the binary.)

> **Runtime adapter CLI required on the host (installed by you, never shipped).** Orchicon does **not** bundle opencode (or any future adapter CLI like Claude Code / Codex) in its images — the operator installs it on the host and it is bind-mounted into the containers at runtime. This keeps the product redistributable regardless of an adapter's license (Claude Code's terms prohibit bundling). Install opencode first:
>
> ```bash
> curl -fsSL https://opencode.ai/install | bash
> ```
>
> `orchicon install` verifies it's present and fails with a clear message otherwise.

```powershell
# Windows (PowerShell) — runs the stack inside WSL2
irm https://orchicon.dev/install.ps1 | iex
```

### Single container (Docker)

The whole Orchicon stack (Postgres, NATS, Tempo/Loki/VictoriaMetrics/Grafana, control plane) runs in one container:

```bash
docker run --rm -p 8080:8080 -p 3002:3000 -v orchicon-data:/var/lib/orchicon ghcr.io/beardedparrott/orchicon
```

The `orchicon` binary is the PID-1 supervisor (`orchicon container`). See [DOCUMENTATION.md §Single-Container Deployment](DOCUMENTATION.md) for the lifecycle script, env vars, and data-preservation notes.

### Windows (WSL2)

```powershell
irm https://orchicon.dev/install.ps1 | iex
```

Orchicon's runtime layer (runtime daemon, unix socket, container mounts) is POSIX-only, so on Windows the **whole stack runs inside WSL2**. The installer provisions/detects WSL2, installs the **Linux** binary inside the distro, and runs the one-command setup there. WSL2 forwards `localhost`, so the UIs open from Windows at the same URLs as on Linux: `http://localhost:8080` (control plane) and `http://localhost:3002` (Grafana).

Prerequisites:
- **Windows 10 21H2+ / Windows 11**, with WSL2 and a Linux distro (first-time users: run `wsl --install` in an admin shell, then reboot — the installer will guide you).
- **Docker Desktop** with WSL2 integration enabled for your distro (or Docker Engine installed inside it).

Project directories are entered in the UI as their **WSL path** — a Windows project `C:\Users\you\projects\Foo` is `/mnt/c/Users/you/projects/Foo` inside WSL. See [DOCUMENTATION.md §Installation Guide](DOCUMENTATION.md) for details.

### Options

| Flag | Description |
|---|---|
| `--version <tag>` | Install a specific version (e.g. `v0.2.0`). Default: latest. |
| `--install-dir <dir>` | Installation directory (default: `~/.local/bin`). On Windows this is a **WSL path** (the binary installs inside the distro). |
| `--no-setup` | Install the binary only — do not pull images / start the runtime daemon / launch the container. |
| `--uninstall` | Remove Orchicon from the install directory. |
| `--dry-run` | Print what would happen without making changes. |
| `--clean` | Stop dev containers, remove old binary, then install latest. All user data preserved. |
| `--force-clean` / `--nuke` | Wipe everything: destroy Docker volumes, remove blob store data and runtime state, then install latest. **All data lost.** |

```bash
# Install a specific version
curl -fsSL https://orchicon.dev/install | bash -s -- --version v0.2.0

# Uninstall
curl -fsSL https://orchicon.dev/install | bash -s -- --uninstall

# Clean upgrade (preserves data)
curl -fsSL https://orchicon.dev/install | bash -s -- --clean

# Force clean and reinstall (destroys all data)
curl -fsSL https://orchicon.dev/install | bash -s -- --force-clean
```

After installation, verify with `orchicon version`. The full stack runs
as a single container — see [Single container](#single-container-docker)
and [DOCUMENTATION.md §Single-Container Deployment](DOCUMENTATION.md).

> **Note:** Pre-built binaries are published to [GitHub
> Releases](https://github.com/beardedparrott/Orchicon/releases). If no
> releases exist yet (pre-v1), build from source instead:

### What gets installed

| Path | Contents |
|---|---|
| `<install-dir>/orchicon` | The `orchicon` binary (control plane + embedded frontend) |
| `~/.local/share/orchicon/` | Runtime state, PID files, logs (`.dev/`), blob store (`data/`) |

### Commands

| Command | Description |
|---|---|
| `orchicon serve` | Run the control plane with embedded frontend (headless) |
| `orchicon serve --detach` / `--stop` | Fork/stop a background server |
| `orchicon container` | Run the whole stack as PID 1 (container image) |
| `orchicon install` | One-command setup: pull images, start the runtime daemon, launch the container, print connection info |
| `orchicon runtime-daemon` | Host process owning the Docker socket; spawns per-workflow runtime containers |
| `orchicon runtime-supervisor` | Runtime container PID 1 (streams `opencode run`) |
| `orchicon runtime-client` | Forwards dispatches into the runtime container |
| `scripts/container.sh up dev\|prod` | Start a single-container instance |
| `scripts/container.sh runtime-daemon` / `runtime-stop` | Start / stop the runtime daemon |
| `orchicon version` | Print the installed version |

```bash
git clone https://github.com/beardedparrott/Orchicon.git
cd Orchicon
make build          # → bin/orchicon
make dev-start      # full dev environment
```

## Development

The control plane is Go; the frontend is TypeScript + Vite. All common
tasks are in the `Makefile` (`make help`).

### Prerequisites

- Go 1.26+
- Node 22+
- Docker (the single-container deployment needs only Docker)
- [`buf`](https://buf.build) and [`atlas`](https://atlasgo.io) — install
  with `make tools`

### Quick start

```bash
make container-build          # build bin/orchicon + the container image
make container-up             # start the dev instance on :8080 (:3002 Grafana)
# or: scripts/container.sh up dev
curl http://localhost:8080/healthz   # {"status":"ok"}
```

Frontend development against a running instance: `make fe-install` (first
time) then `make fe-dev` — the Vite dev server on :5173 proxies the API to
:8080. Migrations run automatically on container boot (embedded runner).

### Authentication

The control plane authenticates every RPC. In local mode
(`ORCHICON_OIDC_ISSUER=local`) a built-in dev identity provider mints
short-lived access tokens + refresh tokens with no external IdP — the
full auth flow is verifiable locally. Production sets a real OIDC
issuer (`ORCHICON_MODE=production` enforces this on boot). The frontend
login page (`/login`) offers both the dev IdP and OIDC SSO. See
`.env.example` for the auth config variables.

### Codegen

The Protobuf schema (`proto/`) is the single source of truth. One
schema generates the Go (connect-go) and TypeScript (Connect-ES)
clients:

```bash
make gen          # buf generate → api/gen/go + frontend/src/api/gen
```

Generated code is committed (see DOCUMENTATION.md §Code Generation).

### Layout

| Path | Concern |
|---|---|---|
| `DOCUMENTATION.md` | Comprehensive project documentation |
| `cmd/orchicon/` | Control-plane binary entry point + `dev` subcommand |
| `internal/` | api, auth, config, db, domain, eventbus, outbox, reconciler, server, telemetry, migrate, middleware, rbac, tenant, blobstore, webhook, version |
| `assets.go` | go:embed directives for container configs, migrations, frontend |
| `proto/` | Protobuf schema (`orchicon.api.v1`, `orchicon.adapter.v1`) |
| `api/gen/` | Generated Go code |
| `db/` | Atlas declarative schema + versioned migrations |
| `deploy/container/` | Single-container image (Dockerfile + embedded runtime configs) |
| `frontend/` | Vite + React + Connect-ES + TanStack Router + shadcn/ui |
| `site/` | Static landing page (`orchicon.dev`) |
| `scripts/` | Installers, CI gates, dev controller |

### CI gate

```bash
make ci          # buf lint + codegen + go vet/test + RLS gate
```

The RLS gate (see DOCUMENTATION.md §Key Architecture Invariants) fails if any `tenant_id`-bearing table
lacks the `tenant_isolation` policy.

## License

Copyright © 2026 beardedparrott. All rights reserved.

This software is provided free of charge for personal and non-commercial
use. You may use, copy, and modify it for your own non-commercial
purposes. Redistribution, sublicensing, or integration into commercial
products that generate revenue requires explicit written permission from
the owner. See the [LICENSE](./LICENSE) file for the full terms.
