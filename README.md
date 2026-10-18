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

- **an installer you can trust with more than one stack**: The installer grew a real vocabulary and a measure of consent.
- **a canary install path on `develop`**: The installer could only be tested after it shipped, which is how two releases carried a broken one.
- **one profile per instance, and one instance per process**: `orch` is meant to run more than once at a time; it could not.
- **in-transcript search, in both clients**: Find a phrase in a long conversation and step through the matches with `n`/`N` (TUI) or prev/next (GUI),
- **the Ask pane is live as it is written**: A reply now streams into the transcript as the model writes it, instead of appearing only when the turn
- **A payload-too-large refusal recovers instead of wedging.** A `413` from the gateway matched none of the
- **A restart can no longer lose your conversation.** The history reducer knew about images and nothing else,
- **Freeing `ctrl+f` for search.** It had been "attach a file by path" in the TUI and nothing in the GUI;
- **A permission card settles on the server's answer**, not on a guess about whether a turn is still running.
- **The release pruner had been a silent no-op its whole life** — the cap it exists to enforce was never
- **The GUI's default theme is Teal Depths again**, three points lighter, and the app background is a solid
- **A canary binary no longer ships without a frontend**, which had made a canary install serve an auth error
- **The footer band stops jumping.** An empty card line was collapsing it, and the screen bounced.
- **The installer's common commands are in the install docs**, rather than only in the README's flags.

Full details: [release notes on GitHub](https://github.com/beardedparrott/Orchicon/releases).

## Installation

### One-line install (Linux / macOS)

```bash
curl -fsSL https://orchicon.dev/install | bash
```

The installer downloads the binary, then runs `orchicon install` to set up everything: pull the published images, start the runtime daemon, launch the single-container instance, and print how to connect / start / stop. (Pass `--no-setup` to install only the binary.)

> **macOS: no Gatekeeper override needed.** `curl` sets no quarantine bit, so a binary installed by the command above runs without a "cannot be opened because the developer cannot be verified" prompt. Orchicon's macOS binaries are **not** code-signed or notarised (a Developer ID costs USD 99/yr — see the `sign-macos` job in `.github/workflows/release.yml`), so one case does need one command: if you downloaded the `.tar.gz` in a **browser** rather than with the one-liner, macOS marked the archive, and the extracted binary inherits the mark. Clear it on that one file — this does not disable Gatekeeper:
>
> ```bash
> xattr -d com.apple.quarantine /path/to/orchicon
> ```
>
> Every release also publishes `SHA256SUMS`; both installers verify the download against it and refuse a mismatch before anything is installed. To check a manual download by hand: `shasum -a 256 orchicon_<version>_darwin_<arch>.tar.gz` and compare it with that file. See [USERGUIDE.md §1 — macOS: Gatekeeper and verification](USERGUIDE.md#macos-gatekeeper-and-verification).

> **Adapters are optional — the native engine needs nothing installed.** Orchicon ships its own runtime engine and runs sessions inside the control plane, so a fresh install is complete on its own: no adapter CLI, no binary probe, no serve. Install an external adapter (OpenCode, and future ones like Claude Code / Codex) only if you want to run work on that runtime. Orchicon **never bundles or ships** an adapter CLI — your own host install is bind-mounted into the containers at runtime, which keeps the product redistributable regardless of an adapter's licence (Claude Code's terms prohibit bundling).
>
> To use OpenCode as a runtime, **install it as your own user — never with `sudo`**:
>
> ```bash
> curl -fsSL https://opencode.ai/install | bash
> ```
>
> That writes `~/.opencode/bin/opencode`, the one location both the plane and its runtime containers can use. `sudo npm i -g` writes to `/usr/local/bin`, which no container mounts — the binary works in your shell and every execution fails to find it. **On Windows, install it inside WSL**, not on the Windows side: the stack runs in a WSL2 distro and a Windows `opencode.exe` is reachable through interop but unusable by the plane. See [USERGUIDE.md §1](USERGUIDE.md#installing-an-adapter-cli) for the full rules.
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

> **Windows: there is no `.exe`, so SmartScreen has no Orchicon binary to warn about.** The stack runs inside WSL2 and the installer installs the **Linux** binary there. The one thing Windows does mark is a **saved** `install.ps1`: if you right-clicked "Save link as" instead of using `irm … | iex`, PowerShell may refuse to run it and SmartScreen may warn — clear the mark on that one file with `Unblock-File -LiteralPath .\install.ps1`. The documented `irm https://orchicon.dev/install.ps1 | iex` writes no file at all and trips nothing.

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
| `--no-verify` | Skip the `SHA256SUMS` check on the downloaded archive. The check is what proves the archive is the one the release published; only skip it against a mirror you control. |
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
| `make instance-up` / `instance-down` / `instance-status` | Start / stop / inspect an instance on the host (no image, no instance container) |
| `make runtime-build` | Build the workflow runtime images with podman (version-gated) |
| `make runtime-daemon` / `runtime-stop` | Start / stop the host-side runtime daemon |
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
- Docker or podman — needed ONLY for the workflow runtime (the runtime images and the
  per-run containers). The instance itself runs on the host: Postgres/NATS are
  started by `cmd/orchicon services`, and no image is built for it.
- [`buf`](https://buf.build) and [`atlas`](https://atlasgo.io) — install
  with `make tools`

### Quick start

```bash
make instance-build           # build bin/orchicon + bin/orch (binaries only — no image)
make instance-up              # start the dev instance on :8080 (host plane + services)
# or: scripts/orchicon.sh start dev
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
