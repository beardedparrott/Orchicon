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
- **[`docs/troubleshooting-missing-pr-links.md`](./docs/troubleshooting-missing-pr-links.md)** —
  why a work item can read `succeeded` with no branch or `PR merged` link
  (deleted runs strip provenance; `local` git strategy never produced a PR;
  a lost mirror write), and how to classify one item from the others.

## Technology Stack

- **Control plane**: Go (single binary, k8s-style reconcilers)
- **One shape**: the plane and its services (Postgres, NATS, Tempo/Loki/VictoriaMetrics) are host processes of your user, one set per instance — see [ARCHITECTURE.md §One shape](ARCHITECTURE.md)
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

The installer downloads the binary, then runs `orchicon install` to set up everything: start the instance's services and plane on this host, register the runtime daemon, and print how to connect / start / stop. (Pass `--no-setup` to install only the binary.)

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

### Desktop app (optional)

The desktop app is a **client**. It does not replace the plane: it talks to a running
Orchicon instance over loopback (`http://127.0.0.1:8080` by default), and if none is
running it starts one for you — which takes a few seconds on first launch. The **CLI is
still the canonical entry point** (`orchicon install`, `orchicon start`,
`orchicon status`), but once the app is installed you do not need it to use Orchicon. The
app is optional: everything the app does, the web UI at the same URL does too.

```bash
# Linux / macOS — install it alongside the CLI
curl -fsSL https://orchicon.dev/install | bash -s -- --desktop
```

```powershell
# Windows — the same, from PowerShell
irm https://orchicon.dev/install.ps1 | iex   # with -Desktop, see USERGUIDE
```

Or download the package for your platform from
[GitHub Releases](https://github.com/beardedparrott/Orchicon/releases): a `.deb` and an
`.AppImage` for Linux, a signed **and notarised** `.app` for macOS, and an NSIS installer
(or portable zip) for Windows. Each platform's gap, stated plainly:

- **macOS** — the `.app` is signed, notarised and **stapled**, so Gatekeeper opens it with
  no bypass, online or off. If you see "cannot be opened", the copy you have is a local
  `make` build rather than the release artifact.
- **Windows 11** — WebView2 ships with the OS, so it installs and launches with no extra
  step. **Windows 10** may not have it: the installer provisions it during setup, and the
  portable zip shows a dialog naming the WebView2 Evergreen bootstrapper
  (<https://go.microsoft.com/fwlink/p/?LinkId=2124703>) rather than opening a blank window.
- **Linux** — the `.deb` declares `libwebkit2gtk-4.0-37`. If it is somehow missing,
  `orchicon-desktop` prints the missing library and the one command that installs it. On a
  distro that dropped WebKitGTK 4.0 (Ubuntu 24.04+), use the AppImage, which bundles it.

The installer never uses `sudo` and never writes to `/Applications` or `/usr`: on Linux the
AppImage lands in the install directory, on macOS the `.app` goes to `~/Applications`. The
full walkthrough is in [USERGUIDE.md §3 — The two clients](USERGUIDE.md#3-the-two-clients).

### Options

| Flag | Description |
|---|---|
| `--version <tag>` | Install a specific version (e.g. `v0.4.5`). Default: latest. |
| `--install-dir <dir>` | Installation directory (default: `~/.local/bin`). On Windows this is a **WSL path** (the binary installs inside the distro). |
| `--no-setup` | Install the binary only — do not pull images / start the runtime daemon / launch the container. |
| `--desktop` | Also install the desktop app (a GUI client for the same plane). Linux: the AppImage; macOS: the signed `.app` into `~/Applications`. Never uses `sudo`. Without it, the install prints where to get the app. |
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
on this host — see [ARCHITECTURE.md §One shape](ARCHITECTURE.md).

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

Plane start/stop lives in the desktop app's **Manage Server** page (Control nav, desktop shell only)
and in `orchicon plane <status|start|stop|restart> [instance]`. The web app has no lifecycle control by
design: a browser cannot spawn a process, so no lifecycle RPC exists.

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
| `frontend/` | Vite + React + Connect-ES + TanStack Router + shadcn/ui |
| `site/` | Static landing page (`orchicon.dev`) |
| `scripts/` | Installers, CI gates, dev controller |

### Testing — ALWAYS THE DEV INSTANCE, NEVER PROD

**This is a hard rule, not a preference.** Tests that open a database CREATE REAL ROWS. The DB-backed
suites (~130 files reading `ORCHICON_TEST_DSN`) create projects, work items, workers and executions by
design, and they clean up only their own tenants.

#### The rule

> **Point `ORCHICON_TEST_DSN` at the DEV instance's database. Never at prod.**
>
> If you are in a shell that is running or has exported a PROD plane, do not run `go test` in it against
> a remote DSN. Check before you run:

```bash
echo "instance=$ORCHICON_INSTANCE  plane=$ORCHICON_POSTGRES_DSN  tests=$ORCHICON_TEST_DSN"
```

#### Why this is written down

It was learned the expensive way. A full `go test ./...` was run from a shell that had a live prod plane
in it — the launcher had exported that plane's `ORCHICON_INSTANCE=prod` and `ORCHICON_POSTGRES_DSN` — and
`ORCHICON_TEST_DSN` was set to *that same prod database* so the suites could run. They ran. Result: **~125
test projects in the deployment tenant and 281 throwaway test tenants in a live instance**, plus a
saturated plane whose in-flight executions failed with `execution lost: control plane restarted or runtime
container gone`.

The mistake was one variable assignment. It was *possible* because nothing in the code distinguished "a
database" from "THIS database", and every existing guard is about isolation **between tests**, not
protection **from the live one**.

#### The guard (in the code, so it cannot be forgotten)

`db.Open` now **refuses to open a production database from a test binary**. Every database handle in the
tree goes through it. You will get a failing test that names the variable to change, rather than silent
writes to production:

```
db: REFUSING to open a production database from a test binary — the DSN being opened IS the ambient
ORCHICON_POSTGRES_DSN — the database the running plane uses.
  opening: postgres://orchicon:***@localhost:5433/orchicon?sslmode=disable
  ORCHICON_INSTANCE="prod"  ORCHICON_POSTGRES_DSN="postgres://orchicon:***@..."
  Point ORCHICON_TEST_DSN at the DEV instance's database instead ...
```

It fires only for a **test binary** (a running server is never refused — the plane opening its own
production database is the product), and only when the DSN being opened is the ambient plane's own
database, or the shell is prod-labelled and the target is not a loopback test database.

`ORCHICON_TEST_ALLOW_PROD=1` is the deliberate opt-out, for a scratch clone that happens to be
prod-labelled. **Reaching production is a decision with a name, never an accident of which shell you
happened to be in.**

#### Which DSN to use

DB-backed tests **skip** when `ORCHICON_TEST_DSN` is unset, so the default `make test` is safe by
omission. To actually exercise them, use the DEV instance's database:

```bash
# the dev instance (services container on 5432) — the correct target for tests
export ORCHICON_TEST_DSN="postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable"
make test
```

Note the dev plane publishes on **5432** and production runs as a host process on **5433**. They are one
port apart and both are "localhost" — which is exactly how the incident above was able to happen.

### Build and test speed

Measured on a developer host (warm CPU, warm `GOCACHE`). Run `time make <target>` after a change to
re-measure — these are the numbers the choices below were made from, not guesses.

| Step | Time | Note |
|---|---|---|
| `go build ./...` (99 packages) | **5.4 s** | warm |
| `go vet ./...` | **3.8 s** | warm |
| `go test ./...` | **28 s** | `-count=1`, DB tests included |
| `make gen-check` | **3.0 s** | |
| `make install-ps1-test` | **27 s** | pwsh harness |
| `make install-path-test` | **23 s** | real installer against a real artifact |
| **`make cross-compile`** | **139 s** | 6 platforms, cold Go cache **every run** |

**The Go test suite is not the slow part.** Cross-compile alone was ~60% of the Go gate, and it was cold on
every single run — see below.

#### `cross-compile`: the one big lever

The target keeps its build cache in a **throwaway** directory (`mktemp` + `trap … EXIT`), deliberately: it
writes ~1.1 GB per platform, and letting that into `GOCACHE` was an unbounded-growth problem. The cost is that
every run recompiles from scratch.

`CROSS_CACHE_DIR` makes that cache persistent while keeping it outside `GOCACHE` (so the isolation the
throwaway rule protects is preserved):

```bash
make cross-compile CROSS_CACHE_DIR=.cross-compile/cache   # first run: ~133 s (populates)
make cross-compile CROSS_CACHE_DIR=.cross-compile/cache   # later runs: ~13 s
make cross-cache-clean                                    # remove it
```

**139 s → 12.7 s, a 10.5× saving on repeat runs.** It is opt-in because the cache is 6.8 GB and a default
that size is a surprise; set it in your shell if you run the gate often:

```bash
export CROSS_CACHE_DIR="$PWD/.cross-compile/cache"
```

Caching a Go build cache across runs is safe: entries are content-keyed, so a restored entry can only be a
*miss*, never a wrong answer.

#### For a fast inner loop, skip the platform compile

`cross-compile` catches *platform-specific* build breaks — it does not change when you are editing logic and
running one package. The quick path:

```bash
go test ./internal/db/          # one package
make test                       # whole suite (DB tests skip unless ORCHICON_TEST_DSN is set)
make ci-go-base                 # the gate WITHOUT cross-compile
make ci-go                      # the complete gate, cross-compile included
```

`make test` reuses Go's test-result cache, so a repeat run with no changes is nearly instant. That caching is
why a local "it's fast" can disagree with a fresh CI run — use `go test -count=1 ./...` when you want the
honest number.

#### In CI

`cross-compile` used to run **serially at the end of `go-ci`**, which is the worst place for the slowest
target. It is now its **own job, in parallel** with `go-ci` (`make ci-go-base` in that job). It carries **no
cache** on the runner: the 6.8 GB cache costs more to upload and download than the ~130 s it would save, so
the runner pays the cold cost and the win comes from the parallelism.

### CI gate

```bash
make ci          # buf lint + codegen + go vet/test + RLS gate
```

> **Note on DB-backed tests in CI.** CI sets `DB_URL` but **not** `ORCHICON_TEST_DSN`, so the ~130 test files
> that need a live database **skip there** — locally, with `ORCHICON_TEST_DSN` set, they run. Measured cost of
> running them against a migrated local database: **~0 s of the 28 s suite**, so enabling them in CI
> (`ORCHICON_TEST_DSN=$DB_URL`, after the migration step) would close that coverage gap almost for free.
> It is not enabled here because it changes what gates a merge, and connection-pool pressure on a CI Postgres
> service is worth watching on the first runs rather than assumed.

The RLS gate (see ARCHITECTURE.md §Key Architecture Invariants) fails if any `tenant_id`-bearing table
lacks the `tenant_isolation` policy.

## License

Copyright © 2026 beardedparrott. All rights reserved.

This software is provided free of charge for personal and non-commercial
use. You may use, copy, and modify it for your own non-commercial
purposes. Redistribution, sublicensing, or integration into commercial
products that generate revenue requires explicit written permission from
the owner. See the [LICENSE](./LICENSE) file for the full terms.
