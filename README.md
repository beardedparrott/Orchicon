# Orchicon

AI orchestration and operations platform that coordinates autonomous AI
work as reliable, observable, recoverable, and manageable systems.

Orchicon separates **orchestration** from **execution**: it manages
projects, workers, scheduling, policies, telemetry, recovery, and
governance, while pluggable runtimes execute the work.

> Orchicon orchestrates. Runtimes execute.

## Documentation

The documentation lives at the project root:

- **[`USERGUIDE.md`](./USERGUIDE.md)** — the operator's guide. Installation, first
  run, and a screen-by-screen walkthrough of **every screen in both clients** (the
  web GUI and the terminal client), plus accounts, roles and the command-line
  reference. **Start here if you are using Orchicon.**
- **[`ARCHITECTURE.md`](./ARCHITECTURE.md)** — how Orchicon is built:
  architecture, project structure, the data model, the Ask Orchicon permission
  model, operator setup, development, deployment, environment variables and
  troubleshooting.

## Technology Stack

- **Control plane**: Go (single binary, k8s-style reconcilers)
- **Single container**: `orchicon container` runs the whole stack (Postgres, NATS, Tempo/Loki/VictoriaMetrics/Grafana, control plane) as PID 1 — see [ARCHITECTURE.md §Single-Container Deployment](ARCHITECTURE.md)
- **API**: Protobuf + Connect (gRPC + REST + streaming from one schema)
- **Database**: PostgreSQL 16 with RLS + transactional outbox
- **Event bus**: NATS JetStream
- **Telemetry**: OpenTelemetry → Grafana stack (Tempo + Loki + VictoriaMetrics) — fully separated infra
- **Policy**: Rego (Open Policy Agent)
- **Runtime adapters**: pluggable gRPC sidecars — a **built-in native engine runs by default and needs nothing installed**, plus external adapters such as OpenCode. Adapters are optional and never bundled.
- **Frontend**: TypeScript + React + Vite + Connect-ES

## Last Release Changes

- **Ask Orchicon can do the work**: Ask Orchicon was a conversation you could read your project *with*; it now runs the same file and shell suite the workers use, against your real filesystem, scoped to the conversation's project.
- **FULLSEND**: A gate that cannot be opened on purpose gets bypassed by accident: mid-task, approving card after card, you stop reading them.
- **A question pauses the turn instead of talking to itself**: Asking a clarifying question used to be record-and-continue — the model wrote the question down, kept going, and your answer arrived as an unrelated message.
- **The plane runs on your host, with the services containerized**: Host residency is the default shape: the control plane runs as a host process while Postgres, NATS and the Grafana telemetry stack stay in one container reached over loopback.
- **Orchicon will not destroy the directory it is working in**: A command that would delete the project it is running in — or the directory holding it — is now **refused outright, and no approval can override it**.
- **The one-command installer no longer deletes YOUR directories.** `--force-clean` (and `--nuke`) removed `data`, `.dev` and `bin` as **relative names**, and the installer never changed directory — so they resolved against wherever you happened to be standing. Anyone who ran the documented command from inside a project lost *that project's* `bin/` and `data/`. It is anchored to Orchicon's own state directory now.
- **A run that executes in your working tree no longer discards your uncommitted work.** Tidying a shared checkout after a run ran `git reset --hard` and `git clean -fd`, and its only signal was "the checkout is dirty" — which is exactly what your own unsaved edits look like. Your work is stashed first and recoverable from `git stash list`, and if it cannot be stashed the tidying is skipped rather than the work being lost.
- **`make clean-docker` only touches Orchicon's containers.** It used to prune stopped containers and unused volumes across the whole Docker host, removing other projects' containers and data on any machine with more than Orchicon on it.
- **A card settles for every client, and survives a reload.** Answering in the terminal settles the same question in the browser, in a second tab, and after a page reload — the resolution is written into the turn's durable record rather than only broadcast to whoever happened to be watching at that moment.
- **Refusals say what actually happened.** A timeout is *expired* rather than an operator denial; an unreadable policy file is reported as a policy problem; a rule that refuses a call is attributed to the rule, not to an operator who was never asked. It matters because the model reads the reason and decides what to do next from it.
- **A turn that dies mid-work keeps its work.** Streaming reasoning was never finalized, so an interrupted turn lost the thinking entirely — and a completed answer discarded it even on a clean turn. Both now survive in the record.
- **Ephemeral runs recover.** A run whose git strategy is `none` creates no branch to resume onto, and was retried blindly; recovery now recognises that shape instead of failing it.
- **Settings: blank means the built-in default, and `0` means disabled.** They were the same value, so leaving a field blank could silently switch a control off.
- **Installer:** a WSL distro name containing a NUL byte no longer corrupts the generated config, and an empty variable expands safely.
- **Terminal client:** the Schedules lenses order history the way the browser does and derive queued sequence children; a send the server refuses because a turn is already running is delivered rather than bounced; the detail pane's paint and the terminal's colour profile are resolved rather than assumed.

Full details: [release notes on GitHub](https://github.com/beardedparrott/Orchicon/releases).

## Installation

### One-line install (Linux / macOS)

```bash
curl -fsSL https://orchicon.dev/install | bash
```

The installer downloads the binary, then runs `orchicon install` to set up everything: pull the published images, start the runtime daemon, launch the single-container instance, and print how to connect / start / stop. (Pass `--no-setup` to install only the binary.)

> **Adapters are optional — the native engine needs nothing installed.** Orchicon ships its own runtime engine and runs sessions inside the control plane, so a fresh install is complete on its own: no adapter CLI, no binary probe, no serve. Install an external adapter (OpenCode, and future ones like Claude Code / Codex) only if you want to run work on that runtime. Orchicon **never bundles or ships** an adapter CLI — your own host install is bind-mounted into the containers at runtime, which keeps the product redistributable regardless of an adapter's licence (Claude Code's terms prohibit bundling).
>
> To use OpenCode as a runtime:
>
> ```bash
> curl -fsSL https://opencode.ai/install | bash
> ```
>
> `orchicon install` no longer requires it. A plane whose model refs need no adapter never probes for the binary, never starts a serve, and never mounts it into a container — see [ARCHITECTURE.md](ARCHITECTURE.md).

```powershell
# Windows (PowerShell) — runs the stack inside WSL2
irm https://orchicon.dev/install.ps1 | iex
```

### Single container (Docker)

The whole Orchicon stack (Postgres, NATS, Tempo/Loki/VictoriaMetrics/Grafana, control plane) runs in one container:

```bash
docker run --rm -p 8080:8080 -p 3002:3000 -v orchicon-data:/var/lib/orchicon ghcr.io/beardedparrott/orchicon
```

The `orchicon` binary is the PID-1 supervisor (`orchicon container`). See [ARCHITECTURE.md §Single-Container Deployment](ARCHITECTURE.md) for the lifecycle script, env vars, and data-preservation notes.

### Windows (WSL2)

```powershell
irm https://orchicon.dev/install.ps1 | iex
```

Orchicon's runtime layer (runtime daemon, unix socket, container mounts) is POSIX-only, so on Windows the **whole stack runs inside WSL2**. The installer provisions/detects WSL2, installs the **Linux** binary inside the distro, and runs the one-command setup there. WSL2 forwards `localhost`, so the UIs open from Windows at the same URLs as on Linux: `http://localhost:8080` (control plane) and `http://localhost:3002` (Grafana).

Prerequisites:
- **Windows 10 21H2+ / Windows 11**, with WSL2 and a Linux distro (first-time users: run `wsl --install` in an admin shell, then reboot — the installer will guide you).
- **Docker Desktop** with WSL2 integration enabled for your distro (or Docker Engine installed inside it).

Project directories are entered in the UI as their **WSL path** — a Windows project `C:\Users\you\projects\Foo` is `/mnt/c/Users/you/projects/Foo` inside WSL. See [USERGUIDE.md §1 — Installation](USERGUIDE.md#1-installation) for details.

### Options

| Flag | Description |
|---|---|
| `--version <tag>` | Install a specific version (e.g. `v0.4.0`). Default: latest. |
| `--install-dir <dir>` | Installation directory (default: `~/.local/bin`). On Windows this is a **WSL path** (the binary installs inside the distro). |
| `--no-setup` | Install the binary only — do not pull images / start the runtime daemon / launch the container. |
| `--uninstall` | Remove Orchicon from the install directory. |
| `--dry-run` | Print what would happen without making changes. |
| `--clean` | Stop dev containers, remove old binary, then install latest. All user data preserved. |
| `--force-clean` / `--nuke` | Wipe everything: destroy Docker volumes, remove blob store data and runtime state, then install latest. **All data lost.** |

```bash
# Install a specific version
curl -fsSL https://orchicon.dev/install | bash -s -- --version v0.4.0

# Uninstall
curl -fsSL https://orchicon.dev/install | bash -s -- --uninstall

# Clean upgrade (preserves data)
curl -fsSL https://orchicon.dev/install | bash -s -- --clean

# Force clean and reinstall (destroys all data)
curl -fsSL https://orchicon.dev/install | bash -s -- --force-clean
```

After installation, verify with `orchicon version`. The full stack runs
as a single container — see [Single container](#single-container-docker)
and [ARCHITECTURE.md §Single-Container Deployment](ARCHITECTURE.md).

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
| `orchicon runtime-daemon` | Host process owning the Docker socket; owns the warm pool of per-workflow runtime containers |
| `orchicon runtime-supervisor` | Runtime container PID 1 (hosts the container's adapter serve, when the run needs one) |
| `orchicon runtime-client` | Forwards daemon requests (serve handshake / ping) into the runtime container |
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

Generated code is committed (see ARCHITECTURE.md §Code Generation).

### Layout

| Path | Concern |
|---|---|---|
| `ARCHITECTURE.md` | Architecture, data model, development, deployment, ops |
| `USERGUIDE.md` | Operator's guide — installation + every screen in the GUI and TUI |
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

The RLS gate (see ARCHITECTURE.md §Key Architecture Invariants) fails if any `tenant_id`-bearing table
lacks the `tenant_isolation` policy.

## License

Copyright © 2026 beardedparrott. All rights reserved.

This software is provided free of charge for personal and non-commercial
use. You may use, copy, and modify it for your own non-commercial
purposes. Redistribution, sublicensing, or integration into commercial
products that generate revenue requires explicit written permission from
the owner. See the [LICENSE](./LICENSE) file for the full terms.
