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

- **a permission card you can finally answer**: The card that asks whether a call may proceed had three ways of failing you, and all three are closed.
- **the pane says what it is waiting for**: The conversation's status line no longer reads the stream's silence while a card is waiting on you.
- **a Windows installer that reports itself**: On Windows the stack runs inside WSL2, and the installer had two ways of leaving you with nothing to go
- **choose your ports, and uninstall safely**: Installing alongside something that already holds a default port no longer means a raw bind error: the
- **Iteration works in its own worktree**: Iteration mode now does its work in its own git worktree, on one branch per conversation, rather than in
- **work items you can skip on purpose**: `skipped` was already a real status — the sequence engine consumed it, and it carried the meaning
- Paste into a permission card's free-text row works again.
- `ctrl+x` on a work item is the GUI's DELETE, not a soft cancel.
- A work-item search reaches matches inside collapsed nodes.
- A worker version's `concurrency_limit` is enforced. It was stored, plumbed and displayed everywhere and
- Backups work on a host-resident instance — the configuration a fresh install now produces. `pg_dump` was
- The transcript's activity line stays in step with the turn: its "newest call Ns ago" age no longer counts

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

The installer never closes your PowerShell window: a failure prints what failed and the next steps, then returns to the prompt with your session intact. The long steps (image pull, stack setup) stream their output as they run, the setup step asks about host ports (ENTER accepts each default), and the full log lands in `%TEMP%`. See [USERGUIDE.md §1 — Installation](USERGUIDE.md#1-installation).

Prerequisites:
- **Windows 10 21H2+ / Windows 11**, with WSL2 and a Linux distro (first-time users: run `wsl --install` in an admin shell, then reboot — the installer will guide you).
- **Docker Desktop** with WSL2 integration enabled for your distro (or Docker Engine installed inside it).

Project directories are entered in the UI as their **WSL path** — a Windows project `C:\Users\you\projects\Foo` is `/mnt/c/Users/you/projects/Foo` inside WSL. See [USERGUIDE.md §1 — Installation](USERGUIDE.md#1-installation) for details.

### Options

| Flag | Description |
|---|---|
| `--version <tag>` | Install a specific version (e.g. `v0.4.5`). Default: latest. |
| `--install-dir <dir>` | Installation directory (default: `~/.local/bin`). On Windows this is a **WSL path** (the binary installs inside the distro). |
| `--no-setup` | Install the binary only — do not pull images / start the runtime daemon / launch the container. |
| `--uninstall` | Remove Orchicon from the install directory. |
| `--dry-run` | Print what would happen without making changes. |
| `--clean` | Stop dev containers, remove old binary, then install latest. All user data preserved. |
| `--force-clean` / `--nuke` | Wipe everything: destroy Docker volumes, remove blob store data and runtime state, then install latest. **All data lost.** |

```bash
# Install a specific version
curl -fsSL https://orchicon.dev/install | bash -s -- --version v0.4.5

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
scripts/orchicon.sh start dev   # full dev environment (host plane + services)
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
