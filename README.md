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
- **Single container**: `orchicon container` runs the whole stack (Postgres, NATS, Tempo/Loki/VictoriaMetrics, control plane) as PID 1 — see [ARCHITECTURE.md §Single-Container Deployment](ARCHITECTURE.md)
- **API**: Protobuf + Connect (gRPC + REST + streaming from one schema)
- **Database**: PostgreSQL 16 with RLS + transactional outbox
- **Event bus**: NATS JetStream
- **Telemetry**: OpenTelemetry → the telemetry backends (Tempo + Loki + VictoriaMetrics), read by the native Telemetry panes — fully separated infra
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
# Windows (PowerShell) — NATIVE: installs orchicon.exe + orch.exe, no WSL2 or Docker required
irm https://orchicon.dev/install.ps1 | iex

# ...and only if you also want a workflow's runtime CONTAINER (this provisions the WSL2 substrate):
& ([scriptblock]::Create((irm https://orchicon.dev/install.ps1))) -Containers
```

### Single container (Docker)

The whole Orchicon stack (Postgres, NATS, Tempo/Loki/VictoriaMetrics, control plane) runs in one container:

```bash
docker run --rm -p 8080:8080 -v orchicon-data:/var/lib/orchicon ghcr.io/beardedparrott/orchicon
```

The `orchicon` binary is the PID-1 supervisor (`orchicon container`). See [ARCHITECTURE.md §Single-Container Deployment](ARCHITECTURE.md) for the lifecycle script, env vars, and data-preservation notes.

### Windows

```powershell
irm https://orchicon.dev/install.ps1 | iex
```

Orchicon runs on Windows as **native processes of the user who installs it**: `install.ps1` places `orchicon.exe` and `orch.exe` in `%LOCALAPPDATA%\Programs\Orchicon`, adds that directory to your **user** `PATH`, and runs the one-command setup there. **WSL2 is needed only for a workflow's runtime container**, which the installer provisions when you ask for it (`-Containers`); a plain install never runs `wsl` and works on a machine that has none.

The installer never closes your PowerShell window: a failure prints what failed and the next steps, then returns to the prompt with your session intact. The long steps (binary acquisition, stack setup) stream their output as they run, the setup step asks about host ports (ENTER accepts each default), and the full log lands in `%TEMP%`. See [USERGUIDE.md §1 — Installation](USERGUIDE.md#1-installation).

> **Windows SmartScreen.** The installer downloads `orchicon.exe` itself, so the binary it installs carries no mark-of-the-web and runs without a prompt. If you instead downloaded the **zip in a browser**, Windows marks that archive and SmartScreen warns *"Windows protected your PC"* the first time you run the extracted `orchicon.exe`: click **More info → Run anyway**, or clear the mark on the archive **before extracting it** with `Unblock-File -LiteralPath .\orchicon_<version>_windows_amd64.zip`. Neither step changes a Windows security setting — every other unsigned download on the machine stays blocked. A **saved** `install.ps1` (right-click → "Save link as") can be marked the same way: `Unblock-File -LiteralPath .\install.ps1`. The documented `irm https://orchicon.dev/install.ps1 | iex` writes no file at all and trips nothing.

Prerequisites:
- **Windows 10 21H2+ / Windows 11.** A native install needs no WSL2 distro and no Docker Desktop. `-Containers` additionally needs WSL2 with a Linux distro (first-time users: run `wsl --install` in an admin shell, then reboot — the installer will guide you).
- A **container engine is optional**: the plane and its services run as host processes, so nothing needs Docker. An engine is only used for a workflow's *runtime container*, and only if you opt in.

Project directories are entered in the UI as **host paths** — with a native install that stays the Windows path (`C:\Users\you\projects\Foo`), not a `/mnt/c/…` translation. See [USERGUIDE.md §1 — Installation](USERGUIDE.md#1-installation) for details.

### Options

| Flag | Description |
|---|---|
| `--version <tag>` | Install a specific version (e.g. `v0.4.5`). Default: latest. |
| `--install-dir <dir>` | Installation directory (default: `~/.local/bin`). The PowerShell installer's `-InstallDir` defaults to `%LOCALAPPDATA%\Programs\Orchicon` and needs no administrator rights. |
| `--no-setup` | Install the binary only — do not start the host services, the runtime daemon or the plane. |
| `--no-verify` | Skip the `SHA256SUMS` check on the downloaded archive. The check is what proves the archive is the one the release published; only skip it against a mirror you control. |
| `--uninstall` | Remove Orchicon from the install directory. |
| `--dry-run` | Print what would happen without making changes. |
| `--clean` | Stop this instance's plane and host services, remove old binary, then install latest. All user data preserved. |
| `--force-clean` / `--nuke` | Wipe everything for this instance: destroy its data directory (database, blob store, runtime state), then install latest. **All data lost.** |

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
| `orchicon install` | One-command setup: acquire the infrastructure binaries (Postgres, NATS), start the runtime daemon, start this instance's services and plane, print connection info. Offers the optional runtime images (default: none) |
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
