# Orchicon — User Guide

> **Orchicon** is an AI orchestration and operations platform. It coordinates
> autonomous AI work as reliable, observable, recoverable and manageable systems
> by separating **orchestration** from **execution**: the control plane manages
> projects, workers, scheduling, policies, telemetry, recovery and governance,
> while pluggable runtimes execute the work.
>
> **Orchicon orchestrates. Runtimes execute.**

This is the operator's guide. It covers installing Orchicon, then every screen in
both clients — the **web GUI** and the **terminal client (TUI)** — explaining what
each one is for and how to use it.

For how Orchicon is *built* (architecture, data model, development, deployment,
environment variables, troubleshooting), see [`ARCHITECTURE.md`](./ARCHITECTURE.md).

---

## Table of Contents

**Getting started**

1. [Installation](#1-installation)
2. [First run](#2-first-run)
3. [The two clients](#3-the-two-clients)

**Part I — The web GUI**

4. [Getting around](#4-getting-around-the-gui)
5. [Ask Orchicon](#5-ask-orchicon)
6. [Overview: Dashboard, Telemetry, Cost Explorer](#6-overview-domain)
7. [Work: Projects, Work Items, Runtime Images](#7-work-domain)
8. [Execution: Workers, Workflows, Executions, Schedules, Recovery](#8-execution-domain)
9. [Automation: Recurring Items, Idea Cloud](#9-automation-domain)
10. [Enforcement: Approvals, Policies](#10-enforcement-domain)
11. [Control: Webhooks, Adapters, Settings, Admin, MCP](#11-control-domain)
12. [Account screens: signing in and creating an account](#12-account-screens)

**Part II — The terminal client**

13. [Getting around the TUI](#13-getting-around-the-tui)
14. [F1 — Ask Orchicon](#14-f1--ask-orchicon)
15. [F2 — Overview](#15-f2--overview)
16. [F3 — Work](#16-f3--work)
17. [F4 — Execution](#17-f4--execution)
18. [F5 — Automation](#18-f5--automation)
19. [F6 — Enforcement](#19-f6--enforcement)
20. [F7 — Control](#20-f7--control)
21. [Keyboard and mouse reference](#21-keyboard-and-mouse-reference)

**Part III — Accounts, access and the command line**

22. [Authentication](#22-authentication)
23. [Roles and entitlements](#23-roles-and-entitlements)
24. [Tenancy](#24-tenancy)
25. [Command-line reference](#25-command-line-reference)

---

## 1. Installation

### Prerequisites

- **Go** 1.26+ (for building from source)
- **Node.js** 22+ (for frontend development)
- **Docker** *or* **Podman** — **optional**. It is needed only for *runtime containers*: several
  workflows run each worker step in a disposable container. Orchicon detects an engine, offers to
  install Podman when one is missing, and runs without one entirely (the plane and its database
  run as your own user's processes; see *Which shape you get*).
- **curl** + **tar** (for one-liner install)
- **buf** and **atlas** (install via `make tools`)
- *(optional)* an adapter CLI — only if you want to dispatch work to an external
  runtime such as OpenCode. Orchicon's built-in engine needs **nothing installed**.

> **Adapters are optional, and Orchicon never ships one.** Its built-in native
> engine runs sessions inside the control plane, so a fresh install is complete
> with **no adapter CLI present** — nothing is probed for, and nothing fails. If
> you want to run work on OpenCode — or on the **Claude** adapter (provider
> `anthropic`), which authenticates with your own claude.ai login — you install it
> yourself, and **where you install it matters**: see
> [Installing an adapter CLI](#installing-an-adapter-cli) below. The images contain
> no adapter binary, which keeps the product redistributable regardless of an
> adapter's licence (Claude Code's terms, for example, prohibit bundling it with a
> product).

### Installing an adapter CLI

**Install it as your own user — never with `sudo`, and on Windows not on the
Windows side.** Both rules come from one fact: Orchicon runs work in **two
places**. A standalone dispatch runs on the **plane**, which sees your `PATH`. A
**workflow run** is executed in its own **runtime container**, which sees only what
Orchicon bind-mounts into it — the adapter paths under your home directory listed
below. So an install outside your home works in your shell, and for standalone
dispatches, and **fails for every workflow run**.

| How you installed it | Your shell | The plane | Workflow-run containers |
|---|---|---|---|
| Official installer → `~/.opencode/bin` | ✅ | ✅ | ✅ **use this** |
| `npm`/`bun` global **in your home** (`~/.npm-global/bin`, `~/.bun/bin`, an `nvm` prefix under `~/.nvm`) | ✅ | ✅ | ❌ |
| `sudo npm i -g` → `/usr/local/bin` | ✅ | ⚠️ if the plane's `PATH` reaches it | ❌ |
| A Windows install (`opencode.exe`) | ⚠️ through WSL interop | ❌ | ❌ |

Orchicon resolves the binary in this order: `ORCHICON_OPENCODE_BIN`, then `PATH`,
then `~/.opencode/bin`, `~/.local/bin`, `~/.npm-global/bin`, `~/.bun/bin`. **Only
the first of those home locations is mounted into workflow-run containers**, which
is why it is the one to use.

#### OpenCode

```bash
curl -fsSL https://opencode.ai/install | bash
```

That writes `~/.opencode/bin/opencode` — the one location that both the plane *and*
its runtime containers can use.

**Do not install it with `sudo`.** `sudo npm i -g opencode` writes to root's npm
prefix, normally `/usr/local/bin`, and no container mounts that path. The binary
then sits on your `PATH` in the shell while every workflow run fails to find it.
The plane warns about exactly this at startup:

```
opencode resolves to a path the runtime containers cannot see — workflow-run executions will not find it
```

**On Windows, install it inside WSL.** The whole stack runs in a WSL2 distro (see
[One-line install (Windows PowerShell)](#one-line-install-windows-powershell--via-wsl2)),
so the CLI has to be in that same distro. A Windows `opencode.exe` is reachable
from a WSL shell through interop, which makes it look installed — it is not usable
by the plane or its containers. Inside the distro, run the command above as your
own user.

**If you install it while the plane is already running**, note that the discovery
of OpenCode's models and MCP servers happens **at plane startup**. A plane that
booted before the CLI existed keeps reporting `model discovery is not configured`,
so install the CLI and then **restart the plane** with `orchicon restart` — the
model picker's adapter tier will then list the OpenCode models. (`orchicon install`
will *not* do it: it is idempotent and deliberately leaves a healthy plane alone, so
the only other way was `serve --stop` and an install.) *Dispatched work* does not need
the restart: the adapter resolves the binary when it dispatches.

**Escape hatch.** If the CLI lives somewhere unusual, point Orchicon at it rather
than moving it:

```bash
export ORCHICON_OPENCODE_BIN=/path/to/opencode     # must be an existing file
export ORCHICON_CLAUDE_BIN=/path/to/claude
```

A value that does not exist is reported as a **wrong variable**, not as a missing
CLI — a typo in the path says so instead of sending you off to install something
you already have.

#### Claude

The **Claude** adapter (provider `anthropic`) resolves from `~/.local/bin/claude`,
with its install at `~/.local/share/claude`; both are mounted into runtime
containers, and the same *as-your-user, never-`sudo`* rule applies. Sign in on the
host once so the CLI carries your own login.

### One-line install (Linux / macOS)

```bash
curl -fsSL https://orchicon.dev/install | bash
```

The installer downloads the binary, then runs `orchicon install` to set up
**everything**: pull the published images
(`ghcr.io/beardedparrott/orchicon` + `orchicon-runtime`), start the host-side
runtime daemon, start this instance's **own** Postgres and NATS on your host, start
the plane, and print how to connect / start / stop. Pass `--no-setup` to install only
the binary (headless / CI).

### One-line install (Windows PowerShell — via WSL2)

```powershell
irm https://orchicon.dev/install.ps1 | iex
```

Orchicon's runtime layer (the runtime daemon, its unix socket, and the container
mounts) is **POSIX-only** — there is no native Windows port. On Windows the whole
stack therefore runs inside **WSL2**, and the installer orchestrates it:

1. **WSL2 provisioning** — the script detects WSL and a Linux distro, ensures WSL2
   is the default version, and prints exact next steps if WSL or a distro is
   missing (Windows 10 21H2+ / Windows 11: run `wsl --install` in an admin shell
   and reboot).
2. **Docker check inside WSL** — confirms `docker version` works inside the distro
   (Docker Desktop with WSL2 integration, or Docker Engine installed in the distro).
   When it does not, it says **which** of the three causes you have — no docker CLI in
   the distro at all, a CLI with no daemon running, or Docker Desktop running with the
   WSL integration off for this distro — and quotes what the distro itself reported.
3. **Linux binary** — it downloads the **Linux** release asset
   (`orchicon_<ver>_linux_<arch>.tar.gz`, arch mapped from the Windows processor)
     and installs it inside the distro (default `~/.local/bin`). There is no Windows
     binary to download: releases publish linux and darwin only.
4. **One-command setup** — it runs `orchicon install` inside WSL: pull the
   published images, start the runtime daemon, start this instance's Postgres and
   NATS, start the plane, wait for health. This is the slow part (minutes on a first
   install), so
   every step is named as it starts and the output is **streamed live** rather than
   replayed at the end. It also asks about the host ports — press ENTER to accept
   each default.
5. **Connection info** — WSL2 forwards `localhost`, so it prints the
   Windows-visible URLs: `http://localhost:8080` (control plane) and
   `http://localhost:3002` (Grafana). If the URLs do not answer, check Windows
   Defender Firewall or add a `netsh interface portproxy` port forward.

**The installer never closes your PowerShell window.** Whatever goes wrong it prints
what failed and the next steps, then returns to the prompt with your session intact —
a failing install is not a window that vanishes. The whole run is transcribed to
`%TEMP%\orchicon-install-<timestamp>.log` and that path is printed when the run starts
(and again if anything halts); `ORCHICON_INSTALL_LOG` picks the file, or `off` disables
it. If the stack setup fails, the **binary is still installed** and the installer
prints the exact command that resumes it — a re-run is idempotent.

**Prerequisites:**

- **Windows 10 21H2+ / Windows 11**. First-time WSL users run `wsl --install`
  from an elevated PowerShell and reboot; the installer guides through this if it
  is not yet set up.
- **Docker Desktop** with the **WSL integration** enabled for your distro
  (Settings → Resources → WSL Integration), or Docker Engine running inside the
  distro.
- *(optional)* an adapter CLI such as OpenCode, installed **inside WSL** as your
  own user — see [Adapter CLIs go in WSL](#installing-an-adapter-cli) and the note
  below on the Windows/WSL boundary.

**Adapter CLIs go in WSL.** If you use the OpenCode or Claude adapter, install that
CLI **inside the distro** with the commands in
[Installing an adapter CLI](#installing-an-adapter-cli) — not on the Windows side.
WSL reaches Windows executables through interop, so a Windows `opencode.exe`
appears to run in a WSL shell, but neither the plane nor its runtime containers
can use it. The same `sudo` rule applies: install as your own user, because only
your home directory is mounted into the containers the work runs in.

**Project directories are WSL paths.** The UI's `project_dir` is a host path.
Inside WSL2 a Windows project `C:\Users\you\projects\Foo` is mounted by Docker
Desktop at `/mnt/c/Users/you/projects/Foo` — enter that **WSL path** in the
project form. There is no Windows↔WSL path translation layer; the UI accepts
whatever path the plane's filesystem (WSL) can resolve.

**Options** (PowerShell form of the Linux flags): `-Version`, `-InstallDir` (a
**WSL** path, default `~/.local/bin`), `-NoSetup`, `-Uninstall` (stops the
container instances and removes the binary; the WSL distro is left intact),
`-Clean`, `-ForceClean`, `-DryRun`.

  > **Windows is covered by an offline harness, not by a full install.** A harness
  > (`scripts/tests/install-ps1-wsl-encoding/`, run in CI as the `windows-installer` job)
  > exercises the installer's own functions against a `wsl.exe` double and runs its failure
  > paths inside a child PowerShell.
  >
  > What it CANNOT see is a Windows PowerShell 5.1 quoting fault — and one shipped: scripts
  > were handed to `wsl.exe` as inline arguments, 5.1 stripped every embedded double quote
  > before bash saw them, and the install died on a mangled script while the same scripts
  > worked on Linux. The transport is now **base64** (an alphabet containing no quote, so
  > there is nothing to strip) and the harness asserts both the round trip and the absence of
  > the hazard. A real end-to-end run still needs a Windows machine; the happy path after
  > Docker is confirmed is the identical Linux code path exercised on Linux hosts.

### macOS: Gatekeeper and verification

The macOS binaries are **not** code-signed or notarised. That is a deliberate decision rather
than an oversight: a Developer ID certificate costs USD 99/yr, and the install path that we
document is already Gatekeeper-clean because **`curl` sets no quarantine bit** — only a browser
download does. Signed artifacts are a job away (`sign-macos` in
`.github/workflows/release.yml`, dormant until `MACOS_SIGNING_ENABLED` is `true` and a
certificate exists); until then, these are the exact instructions.

**Installed with the one-liner?** Nothing to do — no quarantine bit, no prompt. If macOS still
refuses to run it, the binary was replaced or moved from a browser download; go to the case
below.

**Downloaded the archive in a browser?** The archive carries the quarantine mark and the
extracted binary inherits it, so Gatekeeper blocks it. Clear the mark on that one file:

```bash
xattr -d com.apple.quarantine /path/to/orchicon
```

This removes the mark from one file. It does **not** disable Gatekeeper, and it is not a
security setting to lower — every other unsigned binary on the machine stays blocked.

**Verify what you downloaded.** Every release publishes `SHA256SUMS` beside the archives, and
both installers check it before extracting; a mismatch aborts the install, prints both hashes
and installs nothing. To check by hand:

```bash
shasum -a 256 orchicon_<version>_darwin_<arch>.tar.gz   # `sha256sum` does not exist on macOS
```

and compare with the `SHA256SUMS` asset on the release page.

**Homebrew (planned, not available).** There is no tap today. A formula that lags a release is
worse than the exact command above — it installs a stale binary and the version in `orchicon
version` no longer matches the docs — so the tap lands when there is a release process to update
it automatically. It is the intended future channel on macOS, precisely because it sidesteps
quarantine entirely.

### Install options

| Flag | Description |
|---|---|
| `--version <tag>` | Install a specific version (e.g. `v0.4.0`). Default: latest. |
| `ORCHICON_INSTALL_BASE_URL` | **Environment.** Where release assets are fetched from. Defaults to the published GitHub release. Set it to install from a mirror, or to point an install at a pre-release channel. It is also what makes the install path testable: `scripts/tests/install-path` serves a locally-built artifact and runs this very installer against it (`make install-path-test`). |
| `--install-dir <dir>` | Installation directory (default: `~/.local/bin`). |
| `--instance <name>` | Which instance to act on: `dev` (default), `test`, or `prod`. Each has its own container, ports, data volume and host state. **`install` asks, and so does every destructive verb — `--uninstall`, `--purge`, `--remove-binary`, `--clean` and `--force-clean`** — with `dev` as the fallback for an install, and no fallback for the destructive verbs (an empty answer refuses there, so a stray Enter cannot select a stack to destroy). Passing `--instance` skips the question on every one. Also settable as `ORCHICON_INSTANCE`. |
| `--uninstall` | Stop everything **this instance** runs — its plane and its container. The executables are removed **only when no other instance still needs them**, because one install directory serves every instance. **Data preserved** (and it says where). Asks which instance unless `--instance` is given. |
| `--purge` | The same, and also destroy this instance's data — its Docker volume and host state directory. Irreversible. |
| `--remove-binary` | Remove just the `orchicon` and `orch` executables. Every container, volume and instance's state is left alone. If something is running off them it **asks** before stopping it, and declining changes nothing. |
| `--yes` | Answer the prompts affirmatively, for non-interactive use. |

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

### What gets installed

| Path | Contents |
|---|---|
| `<install-dir>/orchicon` | The `orchicon` binary (control plane + embedded frontend + migrations + container configs). On Windows this lives **inside the WSL2 distro** (`~/.local/bin/orchicon`). |
| `~/.local/share/orchicon/` | Runtime state, PID files, logs (`.dev/`), blob store (`data/`). On Windows, under the WSL distro's home. |

### Which shape you get

**Everything runs on your host, as your own user's processes.** The plane, Postgres and NATS are all
local processes of the account that installed Orchicon. That is what lets the plane reach your project
directories and your git identity directly, and it is the only supported shape — there is no
container-resident instance any more, and `ORCHICON_PLANE_RESIDENCY=container` is now **refused** rather
than silently ignored.

**A container engine is used for exactly one thing: a workflow's runtime container** — the disposable
container a run's worker steps execute in. It is not a carrier for the instance, so its absence is a
supported mode: with no engine Orchicon installs and runs normally, and projects that have not opted into
runtime containers are unaffected. `orchicon install` detects an engine, offers to install Podman when
there is none, and verifies the engine works before reporting runtime containers available.

### Starting, stopping and restarting

Once installed, these are the three commands a user needs. Each takes an optional instance
(`dev`, `prod` or `test`) and defaults to `dev`:

```bash
orchicon start [instance]     # services, then the plane — after a reboot, this is the one
orchicon stop  [instance]     # plane, then services — idempotent, safe to run twice
orchicon restart [instance]   # re-read the environment (PATH, a newly installed adapter CLI)
```

The instance can be named positionally or with the flag — both work, on every lifecycle verb:

```bash
orchicon restart prod
orchicon restart --instance prod
orchicon stop --instance=prod
```

`ORCHICON_INSTANCE` is the fallback when neither is given, so an existing script that exports it keeps
working. Naming two instances at once is refused rather than guessed.

> **There is no boot-time autostart.** After a machine reboot, run `orchicon start`. Orchicon installs
> no systemd unit and no launchd job, so nothing starts the plane for you.

> **You are only asked the setup questions once per instance.** The host ports and the optional
> runtime-image download are recorded in the instance's state dir, so a later `orchicon install`,
> `make instance-up` or `make rebuild-<instance>` does not ask again. To change the runtime-image answer
> deliberately, run with `ORCHICON_RUNTIME_IMAGES_ASK=1`.

> **Ports held by a previous run.** If a start cannot bind because something still owns `5432`/`4222`,
> `orchicon stop <instance>` and `orchicon services down <instance>` will stop the instance's **own**
> leftovers — including the container an older version created, which is what usually holds them. If the
> holder is **not** this instance's (a database you started yourself, a sibling instance, an unrelated
> service), nothing is killed: the command names the pid and the executable and stops, so you decide.
> `services down --force` widens it only as far as this instance's own processes.

`install` and `start` are not the same verb, and the difference is deliberate:

| | `orchicon install` | `orchicon start` |
|---|---|---|
| what it's for | setting the machine up | bringing an installed instance back up |
| resolves ports / prompts | **yes** | no — it reads the recorded ports |
| pulls images / probes the engine | **yes** | no |
| on a machine with nothing installed | installs it | refuses, and tells you to run `install` |
| idempotent | yes (converges) | yes |

If you want one command that does everything and you do not mind it being slower, `orchicon install`
remains correct on an already-installed machine — it converges rather than duplicating.

### Ports and instances

An install resolves every published port for its instance, and **three instance columns** exist —
`dev`, `prod` and a throwaway `test` — so several instances can run side by side without colliding:

| Port | dev | prod | test |
|---|---|---|---|
| Web UI + API (`ORCHICON_CONTROL_PORT`) | 8080 | 8091 | 8092 |
| Grafana (`ORCHICON_GRAFANA_PORT`) | 3002 | 3003 | 3004 |

**Setting the variable pins the port and skips the prompt** — the non-interactive path for a scripted
install. Left unset, a port already in use raises a prompt on your terminal offering the next free one;
`ORCHICON_STRICT_PORTS=1` makes that conflict **fatal instead**, if you would rather fail than move a
port. The remaining service ports (Postgres, NATS, OTLP, Tempo, Loki, VictoriaMetrics) follow the same
pattern with their own per-instance defaults — the full table is in ARCHITECTURE → *Host residency*.

### Running the whole stack in a container — retired

This used to be documented as an alternative shape. It is **no longer supported as a way to run an
instance**: the plane and its services always run on the host, and a container is now only a workflow's
runtime container. There is no per-instance container, no instance image to pull, and no docker volume
holding an instance's database.

If you are upgrading from a version that did this, your cluster is most likely still in a docker volume.
Orchicon refuses to start on an empty host data dir while such a volume exists, so nothing is silently
replaced — migrate it once, then continue:

```bash
docker run --rm -v orchicon_postgres-data:/src alpine tar cf - -C /src . | tar xf - -C \
  ~/.local/share/orchicon-dev/postgres
```

(The `orchicon container` verb still exists and is still used — internally, as the supervisor that host
services re-exec — but it is not a supported way to run an instance.)

### Build from source

```bash
git clone https://github.com/beardedparrott/Orchicon.git
cd Orchicon
make build                  # → bin/orchicon (the plane + its CLI verbs)
make instance-up            # start the dev instance (http://localhost:8080)
```

`make instance-up` starts the dev instance's own Postgres and NATS and then the plane — the same
shape `orchicon install` produces. `make instance-build` builds the binaries, and
`make instance-down` / `make instance-restart` are the stop and restart for a repo checkout.

With the frontend in dev mode: `make fe-install` (first time) then `make fe-dev` —
the Vite dev server on `:5173` proxies the API to `:8080`. Migrations run automatically
when the plane boots (embedded runner).

### Verify the installation

```bash
orchicon version
curl http://localhost:8080/healthz   # {"status":"ok"}
```

Both installers also verify the **download** before installing anything: they fetch
`SHA256SUMS` from the release and compare it with the archive, and a mismatch aborts the
install with both hashes printed (nothing is written). To check a manual download yourself:

```bash
shasum -a 256 orchicon_<version>_darwin_arm64.tar.gz   # macOS
sha256sum orchicon_<version>_linux_amd64.tar.gz       # Linux
Get-FileHash .\orchicon_<version>_linux_amd64.tar.gz  # Windows PowerShell
```

and compare the hash with the `SHA256SUMS` asset on the release page. `--no-verify` skips
the check (only for a mirror you control).

---

### Instances, and removing one cleanly

`dev`, `test` and `prod` are three **instances** of the same product on one host: separate container,
separate ports, separate data volume, separate host state.

> **Every verb that changes something asks which instance** unless `--instance` is given. `install` asks
> with `dev` as the fallback; every destructive verb — `--uninstall`, `--purge`, `--remove-binary`, `--clean`
> and `--force-clean` — asks with **no** fallback, because a stray Enter must not select a stack to destroy.
> With no terminal at all, an install takes `dev` and a destructive verb refuses.
>
> That includes `--force-clean`, which destroys a data volume: it is the most dangerous verb here and it was
> the last one to become instance-bound.

**The ports are recorded, not re-derived.** `install` prompts for the host ports and writes what it bound
to the instance's state dir (`ports.json`). Anything that prints connection info reads that record, so a
URL can never point at a port the install did not bind.

```bash
# remove the test instance: its plane and container. Data preserved; binaries left, since dev/prod need them.
curl -fsSL https://orchicon.dev/install | bash -s -- --uninstall --instance test

# the same, and destroy test's data too.
curl -fsSL https://orchicon.dev/install | bash -s -- --purge --instance test

# the executables only — nothing else is touched.
curl -fsSL https://orchicon.dev/install | bash -s -- --remove-binary
```

**Two layers, and they are not the same scope.** Everything above the executables is **per instance**. The
`orchicon`/`orch` binaries and the runtime daemon are **machine-wide**: one install directory serves every
instance, which is why `--uninstall` removes the binaries only once no other instance remains, and says so
when it leaves them. Removing a binary that another instance is running from is what leaves a server that
answers and cannot work — so `--remove-binary` asks first when anything is running off them.

> **If `ORCHICON_INSTANCE` is set in your shell, the prompts are skipped for
> `--clean`/`--force-clean` and that value is used.** A shell started from inside a running plane inherits
> that plane's environment — including its instance. Pass `--instance` explicitly when it matters.

### Testing an installer change (canary)

The installers are the one artifact a user runs **before** we do, so an installer fault is discovered by
whoever happens to install next. That has happened twice, which is what this is for.

Every merge to `develop` publishes a **canary**: the current develop code, built into the release's exact
asset shape, on a prerelease. The installer you run is always develop's; the artifacts are the current
build. Asset names are stable, so the command does not change between merges.

**Windows (PowerShell):**

```powershell
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/beardedparrott/Orchicon/develop/scripts/install.ps1))) -Version canary
```

**Linux / macOS:**

```bash
curl -fsSL https://raw.githubusercontent.com/beardedparrott/Orchicon/develop/scripts/install.sh | bash -s -- --version canary
```

**To test the installer script alone**, against the last released version, drop the version flag:

```powershell
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/beardedparrott/Orchicon/develop/scripts/install.ps1)))
```

That is the shorter loop and the one that matters most, because the script — not the artifact — is what
has broken each time.

**It is not a release.** The canary is a *prerelease*, so `releases/latest` (which both installers resolve
when no version is given) never sees it, and no published release is affected. The version a canary
installs reports `vcanary`; check the tag's commit to see which develop state it carries.

## 2. First run

A fresh Orchicon plane has **no accounts**. Authentication is real from the first
request — there is no anonymous or synthetic dev-login bypass — so the first thing
you do is create your account.

1. **Install and start the stack** (§1). The UI is at `http://localhost:8080`.
2. **Create the first account.** Open the UI and click **Create an account**
   (the sign-in screen offers it as "New here? Create an account"). The **first
   account created on a tenant with no admin becomes the tenant admin** — granted
   automatically with account creation. Later accounts are plain `user`
   identities with no entitlements until an admin grants them a role.
3. **Create a project.** *Work → Projects → New Project*. A project carries a
   `project_dir` (where workers operate), goals, and context files that are
   injected into worker prompts.
4. **Define a worker.** *Execution → Workers → New Worker*. A worker is a reusable
   persona: Role / Skills / Behavior / AGENTS.md prompt fields, a model, and
   budgets. Save it as a draft, then **publish** it — published versions are
   immutable and dispatchable.
5. **Create a work item.** *Work → Work Items → New Work Item*. Pick the project
   and a kind (Epic → Feature → Task → Subtask), add a description and acceptance
   criteria, and assign the worker.
6. **Bind a workflow and run it.** Every run is workflow-driven, so bind a
   workflow (and optionally a runtime image) and start it — immediately, on a
   schedule, or on a recurrence.
7. **Watch it.** *Execution → Executions* streams the worker's output live.

New planes also ship **canned workers** and **canned workflows**, so you can run
something end to end without authoring either from scratch.

**Canned workers** — reusable personas, ready to dispatch:

| Worker | What it does |
|---|---|
| **Senior Software Engineer** | Full-stack development: implements features and fixes bugs |
| **PR Reviewer** | Code review — finds bugs, security issues and correctness problems |
| **QA Engineer** | Functional and regression testing against the acceptance criteria |
| **DevOps Engineer** | Opens and merges the pull request once work is approved |
| **Design Approver** | Worker-backed approval of an architecture/design plan |
| **Code Approver** | Worker-backed approval of a completed implementation |
| **Principal Software Architect** | Architecture design, decision records, technical strategy |
| **- Vision** variants | Copies of the above on a vision-capable model, adding visual verification of UI work (screenshots, accessibility, responsive checks) |

**Canned workflows** — ready-made step DAGs you can bind to a work item:

| Workflow | What it does |
|---|---|
| **SDLC (non-human)** | The full development pipeline with no human gates: Architect → Senior Software Engineer → PR Reviewer → gate → QA Engineer → gate → DevOps (opens and merges the PR), with a conflict loop that sends merge conflicts back to DevOps |
| **SDLC (human approval)** | The same pipeline with two human gates: a **Design Approval** after the Architect, and a **Code Approval** after QA. The run waits for you at each gate |
| **Automation Research** | Planner → Analyst → Synthesizer: researches your market and proposes new work (see §9.2) |
| **Quick Work** | The fast path: one worker implements, verifies, commits, pushes and merges the pull request. No separate review or DevOps step — use it for small, self-contained changes |

**You do not have to use any of this to get value from Orchicon.** Projects,
workers, work items and workflows are the *orchestration* half of the product —
they are how you run autonomous work at scale, in the background, on a schedule,
with recovery and an audit trail. But Orchicon is equally usable as an ordinary
agentic coding harness, with none of them:

> **Just want to work on your code with an agent?** Open **Ask Orchicon**, set the
> conversation to **Iteration** mode, and point it at a directory. It reads your
> files, edits them, runs your tests and commits — the same tools the workers use,
> against your real filesystem, with you approving each write or command. No
> project, no worker, no work item, no workflow. You can add those later if you
> want the autonomous half.

So the two halves are independent: skip the orchestration entirely, use only the
orchestration, or use the harness first and grow into the rest. Nothing in §3–§11
below is a prerequisite for the harness in §5.

---

## 3. The two clients

Orchicon has two first-class clients that sit on the same control plane and the
same data. Neither is a viewer for the other.

| | **Web GUI** | **Terminal client (`orch`)** |
|---|---|---|
| Where | `http://localhost:8080` | your terminal, via the `orch` command |
| Input | mouse-first, with keyboard support | keyboard-first, with full mouse support |
| Domains | 7 nav groups + Ask Orchicon | 7 tabs (F1–F7) + Ask Orchicon |
| Command surface | a native control on every screen; **no slash palette** | a slash palette and `/` command list |
| Best for | visual work — the workflow canvas, the board, dashboards | speed, remote sessions, watching a run live |

Both clients expose the same read **and write** surface: creating, editing,
publishing, approving, cancelling, retrying and bulk operations are all available
from either. A change made in one is a change made in the other, because both read
and write the same plane API.

**Their default looks differ, deliberately.** The GUI opens on **Teal Depths** and the
TUI's launch default is the **`forest`** palette — each chosen for its own surface rather
than matched to the other. Each is a *default, not a pin*: a stored preference always
wins, and every palette is available in both clients.

---

# Part I — The web GUI

## 4. Getting around the GUI

The GUI is a single-page app with a **top bar** and a content area. There is **no
sidebar** — navigation lives entirely in the top bar.

### The top bar

Left to right, the bar holds:

- **The Orchicon mark** — click it to return to Ask Orchicon.
- **Ask Orchicon** — its own button, directly beside the mark.
- **The seven domain menus** — Overview, Work, Execution, Automation, Enforcement
  and Control.
- **Notifications**, the **theme toggle**, and your **profile menu** (Settings,
  Admin, and Sign out) on the right.

### Getting to a screen

Every screen is **two clicks: click a domain menu in the top bar, then click the
screen in the dropdown that opens.**

| Domain menu | Screens inside |
|---|---|
| **Ask Orchicon** | *(its own button — no menu)* |
| **Overview** | Dashboard · Telemetry · Cost Explorer |
| **Work** | Projects · Work Items · Runtime Images |
| **Execution** | Workers · Workflows · Executions · Schedules · Recovery |
| **Automation** | Recurring Items · Idea Cloud |
| **Enforcement** | Approvals · Policies |
| **Control** | Webhooks · Adapters · Settings · Admin *(admin only)* |

The menus work from the keyboard too: open one with **Enter** or **Space**, move
with the **arrow keys**, select with **Enter**, and close with **Escape**. Once you
are on a screen, a **breadcrumb** under the top bar names the domain and screen you
are in.

**On a phone or a narrow window** the domain menus collapse into a **hamburger
button** on the left, which opens a drawer with the same screens.

**Opening the app lands you on Ask Orchicon** — asking is the front door, not a
dashboard.

**Theme and appearance** are set in **Control → Settings → Appearance** (light/dark
plus 20 theme variants — 10 light, 10 dark).

**Long-running lists refresh themselves.** Screens that show live state (Work
Items, Schedules, Executions) update on a short interval, pause while the tab is
hidden, and refresh when the window regains focus. A `Live HH:MM:SS` indicator
makes that visible.

**"Commands" in the GUI means the actions and buttons on each screen** — documented
per screen below. (The terminal client is different: it has a slash-command
palette. See Part II.)

---

## 5. Ask Orchicon

**Where:** the **Ask Orchicon** button in the top bar — it sits directly beside the Orchicon mark

**Purpose.** A conversational partner that shares the platform's understanding of
your project. It can answer questions, read your files, plan and author work, do
the work itself, and dispatch it — depending on the **mode** you select.

### Layout

- **Transcript** (centre) — the conversation, with streaming replies, reasoning
  bubbles, tool-call cards, and an **activity line** that reports what is happening
  while a turn runs.
- **Conversations panel** (right) — your history. Switch, resume, rename or
  delete conversations. A **pulsing dot and Stop button** appear on any
  conversation with a turn running, so a turn is stoppable even when you are
  looking at a different conversation.
- **Composer** (bottom) — the input, with a **project pill**, a mode dropdown and a **model chip**.

### The three modes

Mode is per-conversation, shown as a dropdown in the composer. All three share one
identity, one project awareness and one tool surface; they differ in their
**disposition toward action** — and the boundary is **enforced by the platform**,
not requested in prose.

| Mode | Disposition | What it does |
|---|---|---|
| **Brainstorm** | plans and decides | Does **everything in Orchicon that a human can do through the GUI** — it creates and edits projects, workers, work items and workflows; schedules and re-schedules work; fires runs and sequences; approves, retries and cancels; and reads and updates your settings. What it will not do is **act on your filesystem**: no writing files, no running commands. It investigates, designs, asks clarifying questions, and authors the work. |
| **Iteration** | does the work | Cuts the branch, edits files, runs the tests, commits — working alongside you in this session. It **cannot** author work items or dispatch runs: it is refused the plan and dispatch tools, because doing the work and planning the work are different jobs. |
| **Quick Work** | dispatches it | Creates an ephemeral worker, workflow and work item, fires the run, and cleans up after itself. The hands-off path: it hands the task over rather than doing it in front of you. |

Each mode **refuses** the tools its disposition does not own. Brainstorm and Quick
Work are withheld `write`, `edit` and `bash`; Iteration is refused the plan and
dispatch tools. The refusal arrives as the tool's result, so the model relays it
rather than silently failing. **Only you can change the mode**: the model has no tool
for it, and the mode selector is the only route — so when it asks you to switch, it
is genuinely asking.

Toggling is **session-free**: the mode is read per turn, so the same session
persists across a switch and the next message simply carries the new persona. A
mode switch **supersedes the transcript** — an earlier instruction describing a
different mode no longer applies.

### What you can do from the composer

| Control | What it does |
|---|---|
| **Send** | Send the message. Enter sends; Shift+Enter inserts a newline. |
| **Stop** | Abort the in-flight reply. The turn stops generating immediately and the session stays alive for your next message. |
| **Compact** | Summarize the conversation history to free context. Disabled, with the reason shown, when there is no conversation or a turn is in flight. |
| **Project pill** | Choose the **project (workspace)** this conversation belongs to. Before a conversation exists it sets the project the new chat is created in; on an open conversation it moves it, the same action as the sidebar's "move to project". It falls back to whatever project the sidebar is scoped to, so the two controls cannot disagree. |
| **Mode dropdown** | Switch between Brainstorm / Iteration / Quick Work. |
| **Model chip** | Opens the three-tier **model picker** (adapter → provider → searchable model) and sets the model for the open conversation. |
| **Paperclip** | Attach a file to the next message. |
| **Diff toggle** | Slide out the file-diff pane for the current work. |
| **Grants** | Opens this conversation's **active session grants** — the directories you have approved for it. The button carries a count when any are active (e.g. `Grants (2)`), and each grant can be **revoked** from the panel. This is where you see and take back what you have already allowed. |
| **Scope** | Opens this conversation's **scope**: its own **MCP servers** and **skill files**, plus the project's shown read-only for reference. This is where you add an MCP server to one conversation rather than to a whole project (see §11.5). |

**The conversation's scope.** Each Ask conversation owns its own **MCP servers**
and **skill files**, managed from the conversation's scope panel. The project's
context files appear there read-only for reference. See §11.5.

**Sending mid-reply interjects.** If a reply is streaming, sending supersedes the
current turn rather than queueing behind it: the running turn is cancelled, the
model stops generating, and your message starts a fresh turn. The superseded
turn's partial content is kept as an ordinary message — an interjection is
intentional, not a failure.

**Drafts are cleared on send and restored only on failure**, so a failed send
never forces retyping and a successful send never leaves stale text behind.

### What the agent can do

Orchicon can:

- **Answer questions** about Orchicon, your projects, workers, work items and
  workflows.
- **Create, read, update and delete** projects, work items, workers, workflows and
  other entities.
- **Create project directories** on the filesystem with optional scaffolding
  (`src/`, `docs/`, `tests/`).
- **Read a project's files** — list a project's directory and read files inside it
  (read-only, path-traversal-safe).
- **Diagnose failures** — "Why did the last workflow fail?"
- **Check usage and costs** — "How much have I spent?"
- **View and update settings** — "Show my settings", "Update my default model".

It asks clarifying questions before mutating data and refuses non-Orchicon
requests.

### What asks, and what never does

This is the part worth reading before you grant anything.

- **Reads never ask.** Reading a file, listing a directory, searching — no
  consent card.
- **Writes and executions ask**, each with a **card** naming the tool and the
  exact target, in whichever client you are in.
- **A session grant** covers a directory and everything beneath it for that
  conversation, and the card says so — naming the directory it would cover rather
  than leaving you to guess its reach.
- **A persistent deny list** (your SSH keys, cloud credentials, `gh` config,
  `.netrc`, Docker config) is absolute.
- **The never-allow class** — `sudo`, `dd`, `mkfs*`, partition and LVM tooling —
  can never be approved by anyone, including you.
- **FULLSEND** waives the *prompt* and nothing else. An entry on your deny list
  still refuses, and the never-allow class is still unreachable. It is per
  conversation, revocable, recorded in the audit trail, and lives in memory — so a
  fresh plane starts with it off and a bypass cannot outlive the session you
  enabled it in.
- **Orchicon will not destroy the directory it is working in.** A command that
  would delete the project it is running in — or a directory holding it — is
  refused outright, and **no approval can override it**, FULLSEND included.

### Failure and stall behaviour

You should never be left watching a spinner:

- **A stall is nudged first.** A worker that is generating text or calling tools
  gets an advisory probe sent into its live session; only total silence or an
  unbroken loop escalates to an abort.
- **A dropped connection keeps the turn.** A lost stream shows "Connection lost —
  still working…" and completion is resolved by the message poll, which works
  across socket drops, refreshes and other devices.
- **The Stop button survives a refresh.** Turn state is server-side, so reopening
  a conversation with a turn running re-attaches the stream slot and brings back
  Stop, the thinking indicator and the completion poll.
- **A failed turn is retryable.** Failed, timed-out and stopped turns are
  persisted as error messages with a **Retry** affordance in the same
  conversation.
- **One turn per conversation at a time.** A second send while a reply is pending
  is rejected (the input is disabled while streaming, and sending instead
  interjects).

---

## 6. Overview domain

Read-only. These surfaces answer "what is happening across the plane right now,
and what is it costing me?" — they expose no mutations.

### 6.1 Dashboard

**Where:** top bar → **Overview** → **Dashboard**

**Purpose.** The aggregate state of the plane in one view.

**What it shows.** Executions and work items grouped by status, worker and
runtime-image health, recent activity, and cost rollups (per project, plus
per-workflow costs).

**How to use it.** Open it first after a fresh load to see whether anything is
running, failing, or stuck. Every figure links through to the domain that owns it.

**Actions.** Read-only. Use the links to drill into Executions, Work Items,
Workers, Recovery, or Cost Explorer.

### 6.2 Telemetry

**Where:** top bar → **Overview** → **Telemetry**

**Purpose.** Traces, metrics and logs, and the entry point to the embedded Grafana
stack.

**What it shows.** A trace list with span detail, plus the live telemetry
subscription (footnote status, refresh on event, automatic reconnect).

**How to use it.**

1. Open Telemetry and pick a trace — typically the one for a failing execution.
2. Expand the trace to see spans and their timings, to find where the time went or
   where the error originated.
3. Use the `trace_id` shown on an audit event or execution to correlate the same
   work across surfaces.
4. For raw exploration, the **embedded Grafana UI** is available at `/grafana`
   (Tempo for traces, Loki for logs, VictoriaMetrics for metrics).

**Actions.** Read-only, plus the Grafana jump-off.

### 6.3 Cost Explorer

**Where:** top bar → **Overview** → **Cost Explorer**

**Purpose.** Where the money went, at several levels of detail.

**What it shows.**

- **Overview** (default tab): total tokens, total cost and execution counts, plus a
  per-model spend panel. Totals are **all-time** by default when no window is
  requested, matching the per-model sum — the two surfaces always agree.
- **Cost Explorer**: per-provider and per-model spend with drill-down
  (Project → Task → Execution → Model).
- **By Workflow**: cost per workflow run with per-step detail. A run's row is
  labelled with the bound work item's name where there is one, falling back to the
  run ID for one-shot runs.
- **Credits**: tenant-level usage.

**How to use it.** Sort by cost to find the expensive models, then drill into a
project or a single run to see which step is responsible. Cross-check against the
per-execution numbers on the execution detail page (peak working set, cumulative
spend, cache hit rate).

**Actions.** Read-only.

> **Reading the numbers.** Cost is cache-aware, and the `tokens` budget counts
> **fresh tokens only** — re-sent context does not count against it. So a long-running
> worker cannot trip its token ceiling just by re-reading what it already holds, while
> the cost figure still accounts for that re-sending.

---

## 7. Work domain

The domain where you describe *what* should be done and *where*.

### 7.1 Projects

**Where:** top bar → **Work** → **Projects** (the list; a project opens its own detail page, and **New Project** creates one)

**Purpose.** A project is the top-level container for work. It owns a
`project_dir` — the directory workers operate in — plus goals and context files
that are injected into every worker prompt.

**The fields, and why you'd set them.**

| Field | What it is | Why you'd set it |
|---|---|---|
| **Name** / **Slug** | The display name, and an optional short slug used in URLs and references | Set the slug once if you will refer to the project by name in scripts or URLs |
| **Project directory** | The directory workers operate in — and the boundary they are held to | Always set it. It is also the only directory guaranteed to be mounted into the containers where workers run, so everything a worker may touch must live under it |
| **Git strategy** | How a run handles branches and pull requests — see below | Choose the outcome you want: a pushed branch, a reviewed PR, or nothing pushed at all |
| **Default runtime image** | The container image work items use unless they set their own. Empty = the base image | Set it when your projects need extra tooling (a language runtime, a browser, a CLI) so you are not setting it per work item |
| **Execution mode** | `runtime` — always run in a container (default), or `local` — run in-process on the host | Keep `runtime` for isolation. Use `local` when a task genuinely needs the host's own environment, accepting that it is no longer contained |
| **Max concurrent runs** | Caps how many executions may run at once. `0` = no additional restriction | Set it when a project would otherwise saturate the machine or race itself; the effective limit is `min(tenant, project)` |
| **Goals** | Free-form markdown describing what the project is for | Worth writing: goals are injected into worker prompts, so they steer every task in the project |
| **Context files** | Files or directories injected into every worker prompt, on top of the work item's own | Point them at the conventions, specs or reference material a worker should always have |
| **MCP servers** / **Skill files** | The tools and skills this project's sessions may use (see §11.5) | Add them when the work needs external systems (an issue tracker, a database, a docs server) |
| **Status** | Active or paused | Pause a project to stop it accepting new work without deleting anything |

**Git strategy, in detail.** This is the field most worth understanding, because it
decides what a run leaves behind:

| Strategy | What the run does | Best when |
|---|---|---|
| **Local** (default) | Commits in an isolated worktree and **pushes the branch** to origin. No pull request is opened; the branch stays on origin for you to review or merge by hand | You want the work preserved and inspectable, but do not require a review step |
| **PR** | Pushes the branch **and opens a pull request**, respecting branch protection | You require human review before anything reaches your main branch — this gives you a full audit trail in the forge |
| **Ephemeral** | **Pushes nothing.** The work lives in the run's results and artifacts, and the worktree and branch are discarded | Checks, scrapes and recurring monitors, where no code change should persist |

You can set the strategy per project and override it per workflow; the workflow's
value wins when both are set.

**How to use it.**

1. **Create** — *Projects → New Project*. Give it a name, a slug, and optionally
   goals (markdown).
2. **Set the directory** — `project_dir` is where workers operate, and it is the
   boundary they are held to. On Windows/WSL2 this must be the **WSL path** (see
   §1).
3. **Add context files** — files or directories injected into the prompt. A
   directory is listed and read in full by the worker.
4. **Edit** — open the project's detail page to change the title, goals, directory
   or context files.

**Key rules.**

- **Every context path must live inside the project's `project_dir`** — the only
  directory guaranteed to be mounted into the containers where workers run. The
  file browser is rooted at `project_dir` so the UI cannot offer an
  out-of-project path in the first place, and the API rejects one that arrives by
  another route.
- **Projects have a status.** A project can be paused and reactivated; only active
  projects accept new work.
- **A project can override the tenant's concurrency limit** (Project → Concurrency
  guard). The effective limit is `min(tenant, project)`, where `0` on either side
  means no additional restriction.

**MCP servers belong to a project.** The project detail page carries an **MCP
servers** panel managing the definitions this project **owns** — add one manually
(stdio or streamable-HTTP) or one-click from the curated catalog, and store any
credential it needs. Definitions are owner-scoped: a project owns its own, and
there is no shared tenant list to inherit from (see §11.5). The same page manages
the project's **skill files**.

**Actions on the detail page.** Edit fields, manage context files and MCP servers,
pause/activate, set the concurrency guard, and open the project's work items —
plus archive and delete.

### 7.2 Work Items

**Where:** top bar → **Work** → **Work Items** (the tree and board; a work item opens its detail page, **New Work Item** creates one)

**Purpose.** The unit of work — and, taken together, **a full project-management suite** that happens to be attached to autonomous execution.

If you are used to a tracker, everything you expect is here: a **hierarchy** (Epic → Feature → Task → Subtask, max 4 levels), **dependencies** between items (`depends_on` / `blocks`, enforced as a real DAG with cycle protection), **priority**, **assignees**, **schedules and recurrences**, **search and filter**, **bulk operations**, **archive and restore**, a **Kanban board** you can drag cards across, a **tree** view, a **dependency graph**, **acceptance criteria** and a generated **acceptance review** on completion, and a **per-item comment-free audit trail** through the plane's own event log.

What makes it more than a tracker is that an item is not just a record — **it is what gets executed.** Assign it a worker, bind a workflow, and the ticket *does itself*, reports back, and recovers if it fails. You can run the whole suite as your board of record, or use it purely as the control surface for autonomous work, or both.

**Two views, one filter bar.** The list page has a **Tree** and a **Board** that
share the same filter bar, selection set and auto-refresh loop.

- **Tree** — the free hierarchy with cascade (subtree) selection, tri-state parent
  checkboxes, indent guides, and file-explorer auto-expand when a filter is active
  (ancestors of matches are shown so filtered results stay reachable).
- **Board** — a kanban with one column per server status. Cards **drag and drop**
  between columns; drops are **server-confirmed** (no optimistic transitions),
  with a transient "moving…" state and toasts. A per-card **"Move to…"** menu does
  the same thing for keyboard and touch users.
- **Archive** — archived items and the status they restore to.

**How to use it.**

1. **Create** — pick the project and a kind, add a description and acceptance
   criteria, and assign a worker.
2. **Organize** — set a parent to nest an item. Only epics are top-level, so a
   child cannot be un-parented; a cross-project move requires a parent in the
   target project.
3. **Sequence** — drag siblings into a **chain order**. Only this drag mutates
   order; the filter bar's sort never does.
4. **Depend** — add `depends_on` / `blocks` edges to build the DAG. `relates_to`
   is symmetric and non-ordering, so it is exempt from cycle detection.
5. **Run** — bind a workflow and start it (§8).

**Things worth knowing:**

- **Blocked is a real status.** An item that is armed but cannot dispatch because
  an upstream dependency is not yet terminal-success shows as **`blocked`** (its
  own teal pill, distinct from `pending`/`scheduled`) with the blocking items
  named — so you can see *why* nothing is dispatching. It clears automatically
  once the dependency is met. Only a terminal **success** unblocks: a failed or
  cancelled blocker leaves the dependent blocked and visible for you to resolve.
  Blocked is system-managed — it is not manually movable on the board.
- **`skipped` is yours to set.** It is a terminal status the sequence engine consumes as
  *terminal success* and passes over, so setting it is how you tell a chain **"do not run
  this child"** without cancelling it. It is offered by the status control on the board and
  the detail page, by the TUI's status editor (`s`), and through the Ask Orchicon tool. Unlike
  `blocked` and the in-flight states, a human assigns this one deliberately.
- **Kind switching re-resolves the tree.** Changing an item's kind walks the
  parent up to the nearest shallower ancestor, moves children that can no longer
  sit beneath it under its resolved parent, and — for a non-schedulable kind
  (Epic/Feature) — clears the worker assignment and schedule so a re-typed item
  can never be dispatched. The UI confirms the consequences before saving ("N
  child items will move under the parent").
- **Auto-start is opt-in.** "Start immediately on save" always opens **unchecked**,
  so saving an edit never starts a run unless you tick it deliberately.
- **Work item context files** work exactly like a project's — files are inlined,
  directories are expanded into a listing with an instruction to read every file
  in them. Every path must be inside `project_dir`.
- **Archive and restore.** A terminal item can be archived, hiding it from every
  normal view; archiving is blocked while the item has children. Restore returns
  it with the terminal status it was archived from.

**Position badges.** A `#N` chain-order badge appears on every child of a parent,
derived from the true chain order rather than display order — so the sequence
stays unambiguous even when you sort by title or priority.

**The work item's detail page.** Kind badge, state pill, parent, priority,
budgets, context window, runtime image, worker, workflow, schedule, chain
position, plus the description and acceptance-criteria bodies. For a parent, a
**Parent** card with a searchable picker in edit mode.

**The Acceptance Review.** When a bound run completes, the item gains an
**Acceptance Review** — a deterministic, human-readable aggregation of the run's
own step results rendered as "What was delivered" (and, on a failed run, "Not
delivered / needs attention"). It is not an LLM summarizer call: the step
summaries are the workers' own account of the work done. It is editable, so an
auto-generated review can be corrected.

**Status while bound to a run.** A work item bound to a workflow run is a **shared
input reference** — every step reads the same ticket, and the item stays `running`
for the whole run rather than being mutated per step. It reaches
`succeeded`/`failed` only when the whole run finishes. Because the ticket is never
written per step, **two steps bound to the same ticket can run in parallel**.

**Actions.** Create, edit every mutable field, change status and priority,
schedule, assign/unassign a worker, reorder children, archive/restore, delete
(**permanent** — the same deletion the GUI's Delete performs; an item with children
is refused. For a reversible change, set the status to **cancelled**). Bulk
operations act on the current selection.

### 7.3 Runtime Images

**Where:** top bar → **Work** → **Runtime Images** (a runtime image opens its detail page, **New Runtime Image** creates one)

**Purpose.** The container image a worker execution runs inside. A runtime image
is a buildable spec — apt packages, toolchains, environment, and an optional
Dockerfile override.

**How to use it.**

1. **Create** — *Runtime Images → New Runtime Image*. Name it and give it a slug.
2. **Specify it** — apt packages, toolchain install lines (pip/npm/mise/curl),
   environment as a JSON object, and optionally a raw Dockerfile override.
3. **Build** — press Build. The build streams its logs live, and the row's status
   pill moves `draft → building → ready` (or `failed`, with the error recorded).
4. **Select it** — set it per work item or across a selection in bulk. An empty
   value means the base image.

**Note.** The stock images include a base image, a `:gui` variant with headless GUI
libraries, an `:orchicon-dev` image with the full Orchicon toolchain, and a
`:web-research` image. **No runtime image is downloaded for you.** The stock list
is a set of specs, not downloads: a shipped image that is not present on this
machine shows as **not downloaded** and is never built in the background. Open the
row and press **Fetch** to download it (idempotent — nothing is downloaded if it is
already present). A workflow run whose image is not downloaded fails fast with a
message naming the image and that action — it does not wait on a multi-GB pull.

**Actions.** Create, edit the spec, **Fetch** (download a published image, live log),
**Deploy** (build the spec, live log), delete.

---

## 8. Execution domain

The domain where the work actually happens.

### 8.1 Workers

**Where:** top bar → **Execution** → **Workers** (a worker opens its detail page, **New Worker** creates one)

**Purpose.** A worker is a reusable agent persona — the unit that is dispatched to
do work. It carries prompt fields, a model, budgets, and permissions.

**The prompt fields.** Role, Skills, Behavior and AGENTS.md are the **editable
source of truth**; the server composes them into the system prompt the model
receives. Saving a draft round-trips these fields exactly as entered.

**Lifecycle: draft → published → deprecated → retired.**

- A worker starts as a **draft**.
- **Publishing** makes a version immutable and dispatchable.
- A published version's fields are still editable **in place** — saving an edit
  republishes that version *without advancing the version number*. Use **New
  version** when you want the next number.
- **Deprecating** retires a published version from new assignment; **setting
  active** picks the version new bindings resolve to.

**How to use it.**

1. **Create** — *Workers → New Worker*, filling the header (name, slug, purpose,
   description, plane role) and the prompt fields.
2. **Choose a model** — via the three-tier **model picker** (adapter → provider →
   searchable model). The picker's adapter tier lists the dispatcher's registered
   kinds, the native `orchicon` kind first and seeding a fresh selection.
3. **Set budgets** — per-worker `budget_overrides` for tokens, cost, wall clock,
   tool calls and compaction. A worker's own value always overrides the tenant
   default, per field.
4. **Set permissions and gated tools**, a concurrency limit, and MCP servers
   (carried inline on the published version, immutable with it).
5. **Publish** it.

**Version management on the detail page.** Edit (header + every version field, in
place), new version (created and published in one call), publish a draft,
deprecate, set active. A cancelled edit writes nothing at all — no form creates a
draft on open, so a cancelled edit cannot strand a worker in draft.

**Canned workers.** Seeded in the dev tenant and available immediately: Senior
Software Engineer, PR Reviewer, QA Engineer, DevOps Engineer, Design Approver,
Code Approver, Principal Software Architect, and the **- Vision** variants (copies
on a vision-capable model, adding UI/design-system/accessibility skills and a
Playwright-based visual verification protocol).

**What every worker receives.** Each dispatch prompt is prefixed with a **stable
prompt prefix** — a fixed identity preamble, the shared safety rules, efficiency
directives and a runtime-environment block — identical across every worker and step
of a run, so prompt caching can reuse it. Everything role- and step-specific
follows after it. Workers also get workflow-aware context: step position,
iteration count, execution history, and prior issues found.

**The summary contract.** Worker output is parsed for
`ORCHICON WORKER SUMMARY: success|failure — <summary>`. **The summary word is the
single decision signal** — a separate `_decision:` or `_issues:` line does not
override it, so a reviewer's passing remarks cannot accidentally fail a run.

**Actions.** Create, edit, new version, publish, deprecate, set active, set model
in bulk across a selection, delete (bulk-capable).

### 8.2 Workflows

**Where:** top bar → **Execution** → **Workflows** (a workflow opens its detail page, **New Workflow** creates one, and a run opens its own run page)

**Purpose.** A workflow is a **DAG of steps** that turns a work item into
autonomous work. Every run is workflow-driven — standalone dispatch is retired, so
a work item with no bound workflow cannot be scheduled, and the platform tells you
at creation rather than failing later at run time.

**The visual editor.**

1. **New Workflow** opens the React Flow canvas.
2. **Drag steps in** from the palette: Task, Decision, Approval, Parallel, Loop
   Decision, Work Item, Project, Policy.
3. **Connect steps** with edges — a directed acyclic graph with loop-back and
   success edges.
4. **Configure each step** in the Properties Panel.
5. **Save a draft**, then **publish** when ready. A workflow is a **draft** until
   published, and cannot be bound or run until then.
6. **Run it** and watch step-by-step progression.

**Step kinds worth knowing.**

- **Approval** blocks at a human (or AI) review gate and handles loop-back
  natively — no separate loop node needed. Set **Reviewer** to *Worker* to use a
  worker-backed approver (e.g. Design Approver for a plan, Code Approver for a
  completed implementation); the worker's summary decides approve/reject. The
  **step run itself is the approval record**, so Work Items stay clean — no
  "Approval: …" clutter rows.
- **Loop Decision** can depend on **multiple** upstream steps. The gate waits
  until all upstreams are terminal, then aggregates: failure is decisive — if any
  upstream failed, the chain loops back; otherwise it proceeds only when every
  upstream succeeded. This is how review and QA run in parallel and fan into a
  single gate.
- **Parallel** runs steps concurrently.
- **Task** dispatches a worker against the run's shared work item.

**Loop-back configuration.** Connect the loop outlet (the rose handle) to a
topologically-prior step to define the loop branch, and set **Max rejections** to
bound how many times a workflow can loop back before the run fails.

**Versioning.** Workflows are versioned with a version trail; a run is bound to a
specific version, so editing a workflow never changes a run in flight.

**A run's detail page.** Per-step status, the run body, and
failure diagnosis for a failed run (the failed/blocked steps and the linked failed
executions' error messages), plus the live execution detail pane.

**How runs behave.**

- **Worktrees and branches.** A git-backed run executes in an isolated worktree
  against a per-run branch off `develop`. A **retry re-attaches to the same
  branch**, so the previous attempt's commits carry over. The branch is deleted
  **only on success**, and only when provably merged.
- **PR capture.** A branch worker reports `PR_URL:` / `PR_STATE:` in its summary,
  which is captured onto the run and execution rows. Views render a PR chip only
  for completed runs with a real captured URL.
- **Git strategy** (`local` / `pr` / `none`) resolves once from a single source:
  workflow-level wins, else project-level, else `local`. `none` is genuinely
  ephemeral — a detached HEAD, no branch, no push credentials.
- **Facts ledger.** Each worker records facts it established as `FACTS LEARNED:`
  lines. The single authoritative source is
  `.orchicon/<run_id>/facts_learned`, appended per step with attribution. Workers
  are told to read it first and to treat a recorded fact as established rather
  than re-verifying it — which directly counters over-verification in review and QA
  steps.
- **The narrative.** When a run ends, the ticket's results carry a run-level
  narrative aggregating each step's summary, decision and issues, plus every
  recovery episode.

**Actions.** Create, edit steps, publish, deprecate, create a new version, start a
run, **retry a failed run**, **force progress** a stuck run.

### 8.3 Executions

**Where:** top bar → **Execution** → **Executions** (an execution opens its live detail page)

**Purpose.** The live record of every worker execution.

**How to use it.**

1. Open Executions for the full list; filter, search, sort, and select in bulk.
2. Click an execution to see its **streaming output**, conversation, cost and
   duration.
3. Follow up with a chat message into the live session where the transport
   supports it.
4. Read the honest numbers: peak working set, cumulative spend, cache hit rate.

**Watching a stalled or wedged execution.** The stall monitor routes by what the
worker is doing:

- `no_progress` (total silence) is **fatal** — the adapter hard-kills the
  subprocess and the work item transitions to failed so recovery activates.
- `text_loop`, `repetition` and `no_file_progress` get a **nudge first** — an
  advisory probe into the live session — and escalate to an abort only past the
  nudge reply and cooldown windows.
- **Repetition is result-aware**: only ERROR-status tool calls count toward the
  threshold, so the normal build-fix-iterate-debug loop is not mistaken for a
  loop.

**Actions.** Cancel (with a reason, recorded), send a mid-run message (interject),
pause/resume, bulk delete.

### 8.4 Schedules

**Where:** top bar → **Execution** → **Schedules**

**Purpose.** Everything about *when* work runs, in three lenses over the same
subject.

- **Upcoming** (default) — scheduled work items and recurring work items in
  chronological order with their next runtimes, grouped by local day (Today /
  Tomorrow / weekday). Below the agenda, a **Queued** section lists the
  not-yet-armed children of a running sequence parent, in chain order.
- **Running** — **any** currently running workflow, whether or not it carried a
  scheduled start. Sequence parents appear here labelled with a
  **multi-workflow** chip.
- **History** — **run-driven**: one card per executed run, most recent first, keyed
  on the run record rather than the item's status. This means it includes recurring
  fires whose item re-armed, *every* prior run of an item that ran more than once,
  and in-flight runs.

**How to use it.**

1. Filter by search, project, kind and run-time sort order.
2. Each card links to the work item and its bound workflow; cards with a run link
   to the run.
3. Bulk actions: cancel running schedules, cancel upcoming schedules, hard-delete
   history items.
4. Watch the live clock and countdown chips — they are driven by one page-level
   timer, paused while the tab is hidden.

**Saving a schedule flips the item to `scheduled`** — setting a start time switches
the status no matter what it was, so it appears in Upcoming and fires. The flip is
scoped to the edited item only. It is skipped while the item is in flight (an
in-flight run must not be re-armed) and when the same edit switches it to a
non-schedulable kind.

**Sequence runs.** A parent with children can run its children **one after another,
depth-first**. The parent *is* the sequence run — its own status plus its children's
describe the state fully, and "who's next" is derived, never stored, so a
crash/restart mid-chain resumes correctly. A child failing halts the chain: later
siblings stay `pending` until you fix and retry the failure, at which point the
chain continues automatically. **Schedule-time validation** rejects outright if any
child that must execute has no workflow bound, naming the offenders.

**Manual sequence control.** A sequence parent can be driven explicitly when the
derived cursor cannot act on its own:

- **Start** — re-fires the chain from child #1. *Destructive*: every descendant
  resets to pending.
- **Resume** — continues from the first non-succeeded child, keeping prior
  results.
- **Stop** — parks the chain (parent → pending, schedule cleared) so children can
  be run standalone; an in-flight child finishes naturally.

### 8.5 Recovery

**Where:** top bar → **Execution** → **Recovery** (a recovery opens its detail page)

**Purpose.** What happened after something failed, and what Orchicon proposes to
do about it.

**Recovery is opt-out, not opt-in** — and it is **scoped per failing step run**.
Each failing step run goes `recovering`, gets its own recovery cycle, and
re-dispatches with a fresh execution once recovery completes. Two steps failing on
the same ticket each get their own recovery.

**The flow:** capture → summarize → preserve → review → plan → resume, with
**L1 → L2 → L3 escalation** on repeated failures and bounded auto-relax.

**How to use it.**

1. Open Recovery to see the recoveries list and the live recovery-events stream.
2. Open a recovery to read its detail and the **continuation plan**, plus the
   **action surface the plane allows for that recovery or plan state** — you are
   only offered what is actually available.
3. Review the plan and act.

**Actions.** Approve a continuation plan, reject a plan (reason required), cancel
a recovery, mark the task succeeded. All are confirm-gated.

**The recovery summary is written to the step run**, so the replacement
execution's prompt includes the failure context — which is what stops a retry from
repeating the same failure. For git-backed runs, a failed run reuses its branch, so
a retry carries over partial work.

---

## 9. Automation domain

Where Orchicon finds work for you, rather than only executing what you hand it.

### 9.1 Recurring Items

**Where:** top bar → **Automation** → **Recurring Items** (an item opens its detail page, **New Recurring Item** creates one)

**Purpose.** A recurring work item re-fires on a cadence. This page is a dedicated
flat card list, not the work-items tree — for a recurring item, **cadence and
next-run are the primary identity**, not kind and status.

**How to use it.**

1. **Create** — *Recurring Items → New*. Choose the project, kind, workflow
   binding, and the cadence.
2. **Set the cadence** — frequency (`minute` / `hourly` / `daily` / `weekly` /
   `monthly`), interval (e.g. every 2 hours), days of the week for a weekly
   cadence, a start date, and a start time.
3. **Toggle it** — each card has an enable/pause control that persists
   `recurring_enabled`. Pausing is **not** a destructive clear of the schedule.
4. **Read the history** — the item's detail page shows per-fire run history: each
   fire's status, the bound workflow run, and that run's executions and outputs.

**Behaviour worth knowing.**

- **Firing** — the item fires immediately when due, and `next_run_at` advances to
  the next occurrence in the same pass (idempotent per due window). A leaf fires
  its bound workflow; a parent with children fires through the sequence engine.
  **No new items are spawned** — the same item is re-armed each occurrence.
- **It never goes terminal on a completed occurrence.** Success or failure, it
  returns to `recurring` with the schedule intact, so the next occurrence fires on
  time. One failed occurrence does not stop future cycles.
- **Leaving recurring clears the schedule** — switching the status away from
  `recurring`, or sending an empty schedule, clears the pattern and demotes the
  item to `pending`.

**Schedules is the same subject seen differently.** `/schedules` is the
agenda-and-history view across everything scheduled; `/recurring-items` is the
cadence-first view of the recurring ones. Both are supported; use whichever fits
the question you are asking.

### 9.2 Idea Cloud

**Where:** top bar → **Automation** → **Idea Cloud**

**Purpose.** A triage board for proposals Orchicon generated itself, kept
**separate from your real work items**.

**What actually goes on here, and why it is worth setting up.** The Idea Cloud is
where **Orchicon does your product research and proposes your next work.** You point
a scheduled crew at your market, and it comes back with feature-sized proposals —
not a summary you have to act on by hand, but **candidate work items you can accept
with one click.**

The flagship producer is the **Automation Research** workflow: a three-worker crew
(**Planner → Analyst → Synthesizer**) that surveys the competitive landscape live,
verifies every candidate against external evidence, and distills what it finds into
proposals. It targets capabilities competitors treat as headline features that your
product has **no analogue for**, so the output is genuinely new work rather than a
restatement of your backlog. It checks your existing items and the previously
rejected ideas first, so it will not re-propose something you already have or have
already declined.

**Why that is useful:** instead of maintaining a roadmap by hand — reading release
notes, comparing products, writing tickets — you review a short, evidence-backed
shortlist on a cadence and accept what is worth building. You stay the
decision-maker; the searching is done for you.

**Setting it up.** Bind the **Automation Research** workflow to a **recurring work
item** whose schedule is how often you want it to look (weekly is a reasonable
start), and give that item a brief describing what your product is and which
markets to watch. Any recurring item configured with **Outputs: ideas** lands here
too, so you can point your own research at other questions.

**Two sections:**

- **Active** (default) — automation-produced ideas awaiting triage. Excluded from
  every normal work-item view until a human acts on them.
- **Rejected** — previously dismissed ideas, kept as readable history **and as the
  memory the automation dedupe gate checks before spawning** — so a dismissed idea
  is never re-proposed.

**How to use it.**

1. Read an idea, including its **evidence** and **the run that produced it**
   (provenance).
2. **Promote** it — the only sanctioned path out of idea state. It becomes a
   normal, schedulable work item, leaving the Idea Cloud and appearing in the
   normal Work Items scope, with provenance retained.
3. **Dismiss** it — it leaves every active view and is retained as rejected
   history.

**Actions.** Promote, dismiss.

**Facts stay within a run.** An idea's facts are scoped to the run that produced
it. Across fires, lifecycle state is the record: the Rejected section is the durable,
bounded memory of what was proposed and refused, while accepted history lives in the
normal work items. No prompt block compounds with recurrence.

---

## 10. Enforcement domain

### 10.1 Approvals

**Where:** top bar → **Enforcement** → **Approvals**

**Purpose.** The human review gate for workflow **Approval** steps — pending step
approvals, in one place.

**How to use it.**

1. Open Approvals (or reach the same gate from the workflow run view).
2. Read the **upstream context** on the detail: the upstream worker's summary, the
   acceptance criteria, the touched files, the rejection reason, and the recorded
   **policy decision** (`require_approval`, and which policy).
3. **Approve** or **Reject**, with an optional reason.
4. On approval the workflow proceeds to the next downstream step; on rejection it
   loops back to the configured loop branch.

Worker-backed approval steps appear in the same list alongside human reviews, with
the approver worker's decision recorded on the step run.

**Actions.** Approve, reject (with reason).

### 10.2 Policies

**Where:** top bar → **Enforcement** → **Policies**

> **Coming soon.** Policies are not built yet — both clients say so, and neither
> offers a policy editor. Policy *decisions* are still evaluated and recorded by the
> plane, and you can read them (see below).

**So what enforces policy today?** Policy *decisions* are still evaluated and
recorded by the plane — you see them as **policy context** on the Approvals
surface, and in the **Decisions** trail under Admin. Policy is enforced at
admission, dispatch, budget, approval, recovery and completion points, with
narrowest-scope-first evaluation and a default of allow.

---

## 11. Control domain

### 11.1 Webhooks

**Where:** top bar → **Control** → **Webhooks**

**Purpose.** Data export — subscription-driven delivery of plane events to
external endpoints.

**How to use it.**

1. Open Webhooks to list subscriptions.
2. **Create** a subscription: the endpoint URL, and the events it should receive.
3. **Edit** or **delete** a subscription.
4. **Test** a subscription — sends a test delivery without waiting for a real
   event.
5. **Read the deliveries log** on the detail, where each delivery's outcome is
   recorded, and **replay** a delivery.

**Actions.** Create, edit, delete, test, list deliveries, replay a delivery.

### 11.2 Adapters

**Where:** top bar → **Control** → **Adapters**

**Purpose.** The runtime adapter registry — which runtimes the dispatcher can
route work to.

**How to use it.** Open Adapters to see each registered kind with its version,
endpoint, capability manifest and **health state**, including its last heartbeat.

**This page is read-only.** Adapters self-register with the control plane over the
sidecar contract, so there is no adapter CRUD to perform and nothing to toggle
here — a new adapter appears automatically once it registers. What you *can*
control is the **providers** behind an adapter: enable or disable those in
*Settings → Providers* (§11.3). The dispatcher then routes work to adapters whose
kind matches the model reference's adapter segment and whose heartbeat is healthy.

**The model-reference grammar.** A model reference is
`adapter/provider/model`, and its **first segment selects and routes the
per-worker adapter**. That one segment is the single source of truth for dispatch —
there is no separate runtime reference.

### 11.3 Settings

**Where:** top bar → **Control** → **Settings**

**Purpose.** The plane's configuration, in tabs.

| Tab | What it configures |
|---|---|
| **Appearance** | Light/dark, plus 20 theme variants (10 light, 10 dark). |
| **Defaults** | The default worker model and default Ask Orchicon model; recovery stall parameters; execution budgets; the liveness reaper; transport resilience. |
| **Session** | Session and token lifetimes. |
| **Backups** | Backup creation, listing, restore and pruning, with a directory browser. |
| **Secrets** | Tenant secrets — names and metadata only; values are encrypted at rest and never returned. |
| **Permissions** | The **durable operator policy** — the deny/accept list on the control plane. This is instance state, not a per-session grant. |
| **Providers** | Provider management (Settings → Adapters): enable/disable each provider, override base URLs, add custom OpenAI-compatible endpoints, store tokens, and control per-provider model visibility. |

**Defaults, in detail — the fields worth understanding:**

- **Default worker model** — the fallback when a worker version has no model
  reference set. If both this and the worker's are empty, dispatch fails; there is
  no hardcoded fallback. Both model fields use the **model picker** and are
  validated against the grammar before submit.
- **Default Ask Orchicon model** — the model the conversational agent uses. If
  empty, the conversation falls back to the **free model**, and the Ask header
  surfaces a fallback warning — because that model is rate-limited, and a silent
  provider 429 looks exactly like a "stuck" turn.
- **Stall parameters** — per-execution thresholds stored in the DB and read at
  dispatch time. A genuine hang or loop hard-kills the subprocess and routes to
  recovery; `no_file_progress` is **advisory** — the execution gets a non-terminal
  `stalled` health notice and is revived when file progress resumes.
  **Blank means the built-in default, and `0` means "use the built-in default"
  too** (they are indistinguishable from "unset"); a **negative** value
  unambiguously disables that specific check.
- **Execution budgets** — default ceilings for tokens, cost, wall clock, tool calls
  and the compaction interval, applied when a worker does not set its own. A
  worker's `budget_overrides` always wins, per field. **Wall clock** and
  **tool call count** are hard aborts; **cost**, **tokens** and
  **compact_max_turns** are soft triggers that compact the context and continue. An
  explicit `0` disables that specific gate.
- **Execution liveness reaper** — the sweep that fails executions whose runtime
  process is gone. Because the probe can false-negative on a transient hiccup, an
  execution is reaped only once it is older than the grace window **and** has
  reported not-alive for N consecutive checks.
- **Execution transport resilience** — a broken stream does **not** fail the
  execution: the client retries and the supervisor keeps the child running for the
  grace period so a re-attach can resume. Only exhausted retries fail it.

**Providers, in detail.** The Providers tab is the tenant's model-provider
configuration:

- **Enable / disable** — a disabled provider's models stop being offered to the
  picker and are not dispatched.
- **Base URL** — override the endpoint. For a **local model server** this needs
  care: `127.0.0.1` is correct for the control plane (which runs on your machine),
  but a worker inside a runtime **container** has its own localhost. Orchicon
  automatically gives the container the host's docker-bridge address when a run is
  armed, so you enter one URL — but that address crosses the docker bridge, which
  many host firewalls block by default. If container workers fail while Ask
  Orchicon keeps working, that asymmetry is the cause: allow the bridge subnet
  (e.g. `172.17.0.0/16`) to your model's port.
- **Token** — saving one writes the tenant secret automatically; you never visit
  the Secrets tab to do it.
- **Model visibility** — hide models you do not want offered. Selection is staged
  and committed with an explicit Save.
- **Custom providers** — add any OpenAI-compatible endpoint. Its ref id joins the
  model-reference grammar (e.g. `orchicon/local-models/<model>`), is immutable
  after create, and can carry **manual model entries** with context, output and
  reasoning hints when the probe cannot discover them. Deletion is blocked while
  workers still reference the provider.
- **Ollama** — carries a `num_ctx` default alongside the base URL.

**A note on Windows.** The entire runtime layer — daemon, unix socket, container
mounts — is Linux/POSIX-only. On Windows everything runs inside WSL2 (see §1).

### 11.4 Admin

**Where:** top bar → **Control** → **Admin** (or your profile menu, top right) · **admin role required**

**Purpose.** Identity, roles, API keys, the audit trail, and tenant administration.

**Identities.** Create, edit, disable/enable, and delete identities.

- Four admin-only operations drive it, all gated to `auth:write`.
- A **created identity can immediately be given a local credential** whose username
  matches its subject.
- **Disabling is reversible.** Note that a disabled identity's API keys are not yet
  blocked at resolution time — key-level enforcement is a documented follow-up, so
  disable does not revoke credentials on its own.
- **Delete** hard-deletes the identity plus its role bindings, API keys and local
  credentials in one transaction. Two guards apply: you cannot delete the identity
  you are authenticated as, and an active identity must be disabled first.

**Local accounts.** The identity row's edit modal carries the username and a
new-password field — the documented way to set or change a local account's
password after first boot. Only the password **hash** is ever stored (argon2id by
default); plaintext is never persisted, logged or returned.

**Roles.** A role carries a name and a set of entitlements. The built-in `admin`
role is **immutable** — it carries everything, cannot be renamed, re-entitled or
deleted, and is the tenant-admin bypass.

**Audit trail.** *Admin → Audit* shows two trails side by side:

- **Events** — the actor-based record of **who did what**: the action
  (`work_item.created`, `worker.published`, `auth.login`, …), the actor and auth
  method, the polymorphic target, before/after snapshots for updates, and the OTel
  `trace_id` for cross-correlation with Grafana traces. A filter bar scopes by
  action, actor, target type, target id and time window.
- **Decisions** — the Rego policy-decision trail.

Every mutating RPC and auth action writes an audit row **in the same transaction as
the mutation**, so an audit row exists if and only if the mutation committed.
Read-only calls write nothing. **Secrets never enter the trail** — snapshots carry
only non-secret fields by construction.

**Tenants.** The tenants admin surface is admin-only and retained as the foundation
for a future multi-tenant phase; a standard installation owns exactly one tenant
(§24).

**Actions.** Create/edit/disable/delete identities, set or change local credentials,
create/edit/delete roles, bind roles to identities, create/revoke/rotate API keys,
filter and export the audit trail.

### 11.5 MCP servers and skill files (owner-scoped)

There is **no tenant-level MCP screen**: an MCP server
definition is **owner-scoped**, and its owner *is* the selection. A definition
belongs to exactly one **project**, one **Ask conversation**, or one **worker
version** — there is no shared tenant list and no per-project checkbox over one.

**Where you manage them:**

| Scope | Where | Notes |
|---|---|---|
| **Project** | Project detail → MCP servers panel | Owns its definitions; add manually or one-click from the catalog |
| **Ask conversation** | The conversation's scope panel | The same panel, scoped to that conversation |
| **Worker version** | Worker detail / new → MCP servers panel | Carried **inline on the published version**, so it is immutable with it |

**What a definition is.** A name, a transport, and an enabled flag:

- **`stdio`** — a command plus args and env (e.g. `npx` with its argv).
- **`streamable-http`** — a URL plus headers.

**Credentials** stay tenant-scoped (the secrets store needs a tenant) and are never
returned or baked into config: a definition holds a `${SECRET_NAME}` reference that
is resolved at session time. At the project and conversation scopes the credential
is stored against the entry; on a worker version there is no row to attach it to,
so the reference is built into the version's own env/headers.

**Resolution is ONE union** — a scope resolves to the **project-owned ∪ the scope's
own** definitions, deduped and order-stable. There is no tenant default and no
precedence chain.

**One-click add from the catalog.** The built-in curated registry (filesystem,
github, gitlab, postgres, sqlite, fetch, playwright, sentry, slack and similar)
prefills the create form with the install spec, default config, docs link and
required environment variables.

**Installing a catalog server.** Catalog entries with an installable command carry
an **Install** button, and installation is **explicit** — never implicit at session
time. The plane detects which runtime is available on the host (`npx`, `uvx`,
`docker`); a missing runtime fails the install with a clear error rather than
silently doing nothing, and the result (runtime, command, timestamp) or the
captured error is recorded on the entry. `remote_url` entries need no install.

**Skill files** are managed the same way, at the same three scopes.

> **Worth knowing before you commit to this:** a server can no longer be defined
> once and shared by several projects. Each project keeps its own entry — the
> catalog's one-click add per scope is how you avoid retyping the details.

**The TUI reaches the same surfaces** through the conversation's **scope modal**
(`/scope`, or `/mcp` for the MCP servers and `/skills` for the skill files) and
through the project and worker-version forms.

**Troubleshooting.** *Runtime missing* → install `npx` (Node.js), `uvx`
(`pip install uv`) or Docker on the host, then retry. *Install failed* → read the
recorded error on the entry. *Enabled but not installed* → install it first; an
enabled entry with no installed server cannot connect. *Secret not stored* → use
the credential control on the entry; the plaintext is never persisted in the
entry's config.

---

## 12. Account screens

### 12.1 Login

**Where:** the sign-in screen, shown automatically when you are signed out

**Purpose.** Sign in.

**How to use it.** The page is **honest about the running plane**: it fetches the
plane's public auth capability flags and renders only the methods actually
available.

- **Local account** — username + password, verified against the embedded identity
  provider.
- **SSO** — when an external OIDC identity provider is configured, an SSO button
  takes you through the authorization-code flow.
- **Create an account** — offered as **"New here? Create an account"**, and only
  when the plane offers it (account creation availability *is* the embedded
  identity provider being enabled).

Failures return a **generic** error with no user-enumeration hint, and no identity
is auto-provisioned by a login.

**You land where you were going.** An unauthenticated visit to any protected route
redirects here with the intended destination preserved, and a successful login
returns you there.

### 12.2 Creating an account

**Where:** the sign-in screen → **New here? Create an account**

**Purpose.** Self-service account creation. The page itself is titled **Create your
account**, and its button reads **Create account**.

**How to use it.** Provide a username and password (and confirm it). Creating an
account also starts a session, so a successful sign-up lands you in the app in one
step.

**The first account becomes the admin.** On a tenant with no admin, the first
account created is granted the tenant admin role automatically. Later accounts are
plain `user` identities with **zero entitlements** until an admin grants them a
role. An existing admin is never demoted or clobbered.

**Failure modes.** A duplicate username, or a subject that already exists, returns a
generic conflict — which also blocks identity squatting on SSO handles.

---

# Part II — The terminal client

## 13. Getting around the TUI

Launch it with:

```bash
orch
```

### The tab bar

Seven areas, each bound to an **F-key** (the key printed at the tab is what you
press — there is no modifier to remember):

| Key | Tab | Screens inside |
|---|---|---|
| **F1** | Ask Orchicon | conversations + the chat composer |
| **F2** | Overview | Dashboard · Telemetry · Cost Explorer · Usage Records |
| **F3** | Work | Projects · Work Items · Runtime Images |
| **F4** | Execution | Executions · Workflow Runs · Schedules · Workflows · Workers |
| **F5** | Automation | Recurring Items · Idea Cloud · Rejected Ideas |
| **F6** | Enforcement | Policies · Pending Approvals · Recoveries |
| **F7** | Control | Secrets · Providers · Webhooks · Adapters · Settings · Permissions · Themes · Admin |

### The shell around every screen

- **Content pane** — the focused screen, split into a **list** of sources and a
  **detail** pane.
- **Chat dock** — the composer at the bottom. It is **focused at launch**, so you
  can type immediately.
- **Right rail** — the Ask conversations list, toggled with `ctrl+r` (open by
  default).
- **Left diff rail** — the file-diff panel, toggled with `d` / `shift+d` from
  content focus, or `/diff` from the composer.
- **`?`** — the help overlay, listing the keys for the screen you are on.

### Resizing a screen

Every screen that is split into panes can be **resized**: **`ctrl+←`** and
**`ctrl+→`** move the divider, giving the list or the detail pane more room. The
width you choose is remembered, so it persists between sessions. The composer
advertises the chord whenever there is a split on screen.

### Switching screens

- **F1–F7** switch areas; clicking a tab also switches and opens its menu.
- **Enter or space on the active tab** opens that area's **submenu** of
  sub-screens — up/down (or `j`/`k`) moves, enter selects, esc closes. The rows are
  click targets too.
- **`/`** opens the **command palette** above the composer, with the input still
  visible.

### The composer

- Type and press **Enter** to send.
- **`shift+enter`** or **`alt+enter`** inserts a newline (a trailing `\` then Enter
  does the same, if you prefer).
- **`ctrl+a`** takes the whole composer in one gesture: it **copies what you have
  typed to the clipboard** and clears the way for the next keystroke to replace it.
  Use it to grab a draft, or to throw one away and start again.
- **`ctrl+v`** reads the clipboard: an **image** on the clipboard is **attached to
  your next message**, while **text** on the clipboard is **pasted into the
  composer**. One key, whichever you have. If a card's free-text row is open — an ask
  card's **Other**, or an answered question's draft row — the text goes **into that row**
  instead, so you can paste an error trace or a code block straight into your answer
  (line breaks are kept; the row holds your words, not a structured field). A row you
  have handed back to the composer with `ctrl+g` does not take it.
- **`ctrl+f`** opens the **find box** — a search over the conversation you are
  reading. Type the term in the box: the transcript filters **as you type**, every
  occurrence is **highlighted** in the text so you can see *where* the match is rather
  than only that the message contains one, and the box reports the match count.
  **Enter** keeps the search and hands the keyboard to the transcript; **esc** cancels
  and puts back the transcript you had.
- **Pasted text never sends until you press Enter.**
- **`ctrl+y`** stops an in-flight reply (the GUI's Stop button). The affordance row
  offers it while a reply streams.
- **Sending while a reply streams interjects** — it supersedes the turn rather than
  queueing behind it.
- **`ctrl+g` / `esc`** toggle between content focus and the composer.

### Reading a long conversation

The transcript's navigation keys work from **the content pane** always, and from **the
composer** whenever the box is empty — while you are drafting a message they are your
message's keys instead (home/end move within your draft, the arrows move the cursor).

- **`home`** / **`end`** — jump to the **first** / **latest** message. `end` also
  resumes following the live tail.
- **`↑` / `↓`** and **`PgUp` / `PgDn`** — scroll the conversation.
- **`ctrl+f`** — the find box (see the composer list above). While a search is
  running, **`n`** / **`N`** step to the next / previous match and **`esc`** clears it.
- **`esc`** — from the composer, hands the keyboard to the transcript. The footer
  always says which side holds it.

From the content pane, **`←` / `→`** choose whether the arrows drive the conversations
rail or the conversation itself. The composer advertises all of these chords while a
conversation is open, so you do not have to remember them.

### Slash commands

Type `/` to open the palette, or `/help` to list every command with its usage.

| Command | What it does |
|---|---|
| `/help` | List all commands with usage |
| `/new` | Start a new Ask Orchicon conversation |
| `/connect` | Reopen the connection/auth screen (in place — never exits) |
| `/reconnect` | Redial every live stream |
| `/diff` | Toggle the left diff rail for the active execution/conversation |
| `/context` | Show the context injected with each message; `pin` to override |
| `/theme` | Switch the theme (no argument lists them); persisted |
| `/quit` | Exit (terminal state restored) |
| `/rename` | Rename the open conversation (no title opens a prefilled box) |
| `/delete` | Delete the open conversation |
| `/project` | Switch the **project workspace** the conversations rail shows |
| `/project-move` | Move the open conversation into another project workspace |
| `/models` | Choose the Ask model (adapter → provider → model) for the open conversation |
| `/model <ref>` | Set the model new conversations are created with |
| `/mode` | Set the conversation mode; no argument reports the current one |
| `/scope` | This conversation's scope: its MCP servers and skill files |
| `/mcp` | Open the scope modal at this conversation's MCP servers |
| `/skills` | This conversation's skill files; paths or `clear` write directly |
| `/fullsend` | Toggle FULLSEND for this conversation |
| `/compact` | Summarize the conversation to free context |
| `/attach` | Attach a file by path (pasting a path into the composer does the same) |
| `/find <text>` | Search the open conversation. The one-shot form of the find box — `ctrl+f` opens the box itself, where you type the term |

**Two names that must not be confused.** `/project` (**singular**) sets the Ask
tab's **workspace scope**. `/projects` (**plural**) is the generated navigation
command that opens the Work tab's Projects pane.

**Every list pane has a navigation command.** Each pane a screen offers has a
matching command (e.g. `/work-items`, `/runtime-images`, `/executions`), so you can
jump straight to a pane by name.

### The Ask conversations rail

With the rail open and the composer empty, the arrow keys move it **from either the
composer or the content**:

| Key | Action |
|---|---|
| `space` | Mark the highlighted conversation and step down |
| `ctrl+n` | Rename it |
| `ctrl+t` | Categorize it (the whole marked selection when there is one) |
| `ctrl+x` | Bulk-delete the marked selection (after a confirm) |
| `esc` | Clear the marks before doing anything else |

There is no bulk action below two marks — one marked row is simply the row the
cursor is on. Groupings are managed in place: an item's pane is where you put it in
a group, and a group's own row is where you rename (`e`) or delete (`ctrl+x`) it —
deleting a group moves its items to Uncategorized.

### Mouse

- **Click** tabs, list rows, panes, buttons and form controls.
- **Wheel** scrolls.
- **Drag** to select text anywhere on screen — releasing **copies it to the
  clipboard** and shows a confirmation. The copy uses the terminal's OSC 52 escape,
  so your terminal must allow clipboard writes (under tmux, `set-clipboard` must be
  on).

### Themes

Choose one in *Control → Themes* (press the action key on a row to apply and
persist it), or `/theme`. The launch default is the **`forest`** palette, matching
the GUI's default. There are 43 themes across light and dark, including **true
transparency**, where the terminal shows through and the text adapts to your
terminal's own background.

### On Windows

Chords and mouse work in Windows Terminal. Legacy conhost may degrade mouse
handling.

**Colour.** Theme backgrounds need **true colour**, and a 256-colour terminal
cannot tint a dark ground at all: the xterm-256 cube's only dark entries are
greys, so every dark theme's background renders grey or black and looks like the
theme is broken. Windows Terminal supports true colour but does not always tell a
WSL shell so, which used to require setting `COLORTERM=truecolor` by hand — **the
TUI now does this for you** when it detects WSL. If a dark background still looks
grey, the terminal genuinely is not reporting 24-bit colour: set
`COLORTERM=truecolor` in the shell that runs `orch`, or `COLORTERM=yes` to force
256-colour. `ORCHICON_TRUECOLOR=0` turns the automatic default off.

---

## 14. F1 — Ask Orchicon

**Purpose and layout.** The same conversational partner as §5 — a transcript, the
conversations rail, and a composer with a mode and model control — expressed as a
terminal surface. The **composer belongs to the shell**, not to a screen, so it
follows you across tabs.

**Screen content.** The Ask screen's own list pane is **Conversations**; the chat
itself is the dock. The tab's dropdown offers two verbs rather than panes:

- **New** — the launch page, exactly what `orch` shows on start.
- **Conversations** — the transcript view plus the conversations rail.

**How to use it.** Type into the composer and press Enter. Send a new message to
start a conversation. Use `/mode` to change disposition, `/models` to choose the
model for the open conversation, `/compact` when context grows, and `ctrl+y` to
stop a reply.

**The stat strip.** The composer's bottom-right row reports the live session:

```
<ask model> · ctx 124K/200K · 1.2M tok · cache 78% (940K) · $1.2345   [brainstorm]
```

- **Ask model** — the open conversation's model, else the tenant default.
- **Context** — occupancy against the model's window. Occupancy is the latest usage
  record's input side (prompt + cache reads + cache writes) — the *sum* across turns
  is not the context size. Omitted, never fabricated, when the window is unknown.
- **Tokens** — the session total.
- **Cache** — the hit ratio with the cached token count. Omitted rather than shown
  as a meaningless 0% when there is no input.
- **Cost** — the session's recorded cost.
- **Mode** — the persona pill sits immediately to the right of the stats.

Both clients read the same recorded usage row — neither recomputes pricing or
token counts from messages. The strip re-reads on conversation open/switch, on a
model change, and when a turn completes; a read for a conversation you have left is
dropped, and a **failed read keeps the last good numbers** rather than blanking the
strip.

**The activity line.** While a turn runs, one row reports what is happening, in a
single precedence order: a dropped socket shows "reconnecting…", an unreachable
plane shows the disconnected banner, a turn gone quiet past the watchdogs states
its verdict, and otherwise the line shows a **rotating verb** plus a **rolling tool
counter** over the last 30 seconds ("3 modifies · 1 read · last 4s"). The counter
yields to escalation, renders nothing at all with zero counted calls, and is
dropped before the verb on a pane too narrow for the whole line — so the single row
never wraps and the watchdog's verdict is never clipped away.

**Workspace scope.** `/project` scopes the conversations rail to a project, a new
chat is created **in** it, and the rail's title names it. `/project-move` moves the
open conversation to another workspace.

**MCP and skills.** `/scope` shows this conversation's scope — its MCP servers and
skill files, with the project's shown read-only. `/mcp` opens the same modal
directly at the MCP servers, and `/skills` at the skill files.

---

## 15. F2 — Overview

Read-only, mirroring the GUI's Overview domain.

| Source | What it shows | How to use it |
|---|---|---|
| **Dashboard** | Executions and work items by status, worker and runtime-image health, recent activity | Open it to see plane state at a glance; select a row for its detail |
| **Telemetry** | The trace list with span detail, plus the live telemetry subscription | Find the trace for failing work; the footer reports stream status and invalidates on event |
| **Cost Explorer** | Usage and cost aggregated by provider and by model, with a grand total | Sort by cost to find what is expensive, then drill in |
| **Usage Records** | The raw usage records table with per-record detail | The underlying rows behind the aggregates — use it when an aggregate needs explaining |

**Chords.** `enter` focuses the detail · `←`/`→` (or `h`/`l`) switch pane ·
`r` refreshes. There are no mutations in this domain in either client.

---

## 16. F3 — Work

Everything the GUI puts under Work, read **and write**.

Sources: **Projects · Work Items · Runtime Images**.

### Projects

Create (`n`), edit title/goals/project_dir (`e`), and set or create the project
directory — the directory is written through the update call and then **probed**,
so a bad path fails immediately instead of at worker dispatch.

### Work Items

The richest surface in the client. Three display groupings over the real fields:

- **Tree** — the actual parent/child DAG (max 4 levels).
- **Board** — grouped by real status, empty columns kept.
- **Archive** — archived items plus the status they restore to.

`v` cycles the view; `T` / `B` / `Z` select Tree / Board / Archive directly.

**Chords.**

| Key | Action |
|---|---|
| `n` | Create (title, kind, parent, description, acceptance criteria, priority, budgets, context window, workflow, runtime image, context files, auto-start) |
| `e` | Edit every mutable field |
| `s` | Set status and priority |
| `t` | Schedule (start time + auto-start) |
| `w` / `W` | Assign / unassign a worker |
| `J` / `K` | Reorder children — **the only sequence mutation** |
| `a` | Archive |
| `R` | Restore |
| `ctrl+x` | Delete — **permanent** (the GUI's Delete; an item with children is refused) |
| `/` then text | Search |

Search narrows the list **through collapsed nodes**: a query reveals matching items
even when their parent is collapsed (their ancestors are kept so the result keeps its
hierarchy), and clearing the query restores the collapse you had.

Every destructive chord is confirm-gated, and the list reconciles after a write.

**One rule the client enforces on you:** the Tree and Board are **display
groupings** and never renumber anything. Only `J`/`K` reorder.

### Runtime Images

Detail includes tag, status, base, version and built version, apt packages,
toolchains, environment, and any Dockerfile override, plus the last build log.

**Chords.** `n` create · `e` edit the spec (version-carried) · `b` **build**, with
logs streaming live into the pane · `ctrl+x` delete (confirm-gated).

---

## 17. F4 — Execution

Sources: **Executions · Workflow Runs · Schedules · Workflows · Workers**.

Note that **Workflows live under Execution**, not Automation — they are part of the
execution domain. Schedules sits beside the runs because it is the same subject
seen through three lenses (queued, in flight, already run).

### Executions

Live list and detail, with the execution-event stream. The detail is a live session
view that preserves your scroll position as new output arrives, plus the file-diff
pane.

**Chords.** `c` cancel (a confirm, with the reason recorded) · `i` interject a
message into the running execution. Both reconcile the list.

### Workflow Runs

Detail plus the step-runs body (per-step status), and failure diagnosis for a
failed run. A live execution detail pane sits alongside.

**Chords.** `r` retry a failed run (confirm) · `f` force progress a stuck run
(confirm).

### Schedules

The same three lenses as §8.4 — **queued**, **running**, **finished**. `v` switches
lenses. Sequence children that are queued are derived here, so a scheduled parent's
not-yet-armed children are visible.

### Workflows

List and detail, with the version trail.

**Chords.** `n` create · `e` edit steps · `p` publish · version actions, and a live
Execution detail pane for the bound run.

### Workers

List and detail, showing the version list with each version's model reference.

**Chords.** `n` creates in the details pane (no modal), loading the tenant's roles
first so the plane-role picker can offer them — and writing nothing until you save.
`e` opens **one form** covering the header plus every version field, and saves with
republish, so the version is edited **in place** (number unchanged) and the worker
ends published. `V` creates and publishes a new version in one call. `m` sets the
selected worker's model through the picker. `M` (shift+m) sets the model for **all
marked workers at once**, in one modal. `space` marks rows and `ctrl+x` deletes the
marked selection.

---

## 18. F5 — Automation

Sources: **Recurring Items · Idea Cloud · Rejected Ideas**.

Mirrors the GUI's Automation domain, with the rejected ideas surfaced as their own
pane (the same data the GUI shows as the Idea Cloud's Rejected section — and the
memory the automation dedupe gate consults).

### Recurring Items

`n` create · `e` edit · `p` pause/resume (persists the enabled flag; never a
destructive clear) · `x` delete (confirm).

The detail shows the cadence and next fire, plus **per-fire run history**: each
fire's status, the bound workflow run, and that run's executions and outputs.

### Idea Cloud

The active triage list, with provenance (`spawned_by`, the run that produced it)
and a spawned-by badge.

**Chords.** `p` promote (the only path out of idea state) · `x` then `y` to dismiss
(confirm).

### Rejected Ideas

The durable dismissed-spawn history. Readable, and the reason a dismissed idea is
never re-proposed.

---

## 19. F6 — Enforcement

Sources: **Policies · Pending Approvals · Recoveries**. The recovery-events stream
drives the footer status, because recovery rides the enforcement UX.

### Policies

> **Coming soon…** — the same as the GUI (§10.2): Policies are not built yet, so
> this pane is empty.

### Pending Approvals

The list of pending step approvals. The detail renders the upstream context — the
upstream worker's summary, acceptance criteria, touched files, the reason, and the
**policy context** for the step run — without a separate get call, because the list
response is the carrier.

**Chords.** `a` approve (reason) · `x` reject (reason required). Both are
form-gated, disable themselves while in flight, and reconcile the list after.

### Recoveries

The recoveries list plus the recovery-events stream; the detail shows the recovery
and its continuation plan, and lists the actions the plane actually allows for that
state.

**Chords.** `a` approve a continuation plan · `x` reject a plan (reason required) ·
`c` cancel a recovery · `m` mark the task succeeded. All confirm-gated.

---

## 20. F7 — Control

Sources: **Secrets · Providers · Webhooks · Adapters · Settings · Permissions ·
Themes · Admin**.

Nine sources render **one pane at a time** (focused source plus detail) — a
nine-across grid would truncate beyond reading.

### Secrets

Names and metadata only, plus create/update/delete **by name**. No code path reads
a value: the get call is never issued, so a secret's plaintext cannot be rendered.

### Providers

List providers, create/edit/delete custom providers, enable/disable, and store or
clear a provider token.

### Webhooks

Create, edit and delete subscriptions; **test** one; and read the deliveries log in
the detail body.

### Adapters

The registered kinds and each one's capability manifest, with the same
enable/disable dispatch filter as the GUI.

### Settings

Every field — models, stall knobs, the reaper, budgets, backup and log settings,
session lifetimes. `e` opens the typed form and saves through the update call. The
two model fields open the **three-tier picker** and are validated against the
pinned grammar before submit, so a malformed reference cannot be saved.

### Permissions

The **durable operator policy** (the deny/accept file on the control plane) — not a
session grant. It is instance state, and both clients read and write the same file,
so a change in either is a change in the one both read.

### Themes

The TUI's own palette set. Select a row and press the action key to apply and
persist. **Themes have no bulk operations** — `space` does not multi-select them,
because there is no meaningful action to take on several themes at once.

### Admin

The admin surface inventory plus an **explicit live permission state**, probed via
an admin-gated read. A credential without the admin scope sees "permission
required" — never a silent empty pane.

---

## 21. Keyboard and mouse reference

### Global keys

| Key | Action |
|---|---|
| **F1–F7** | Switch area |
| **enter / space** on the active tab | Open that tab's submenu of sub-screens |
| **ctrl+r** | Toggle the Ask conversations rail |
| **ctrl+g** / **esc** | Toggle content ↔ composer focus |
| **`/`** | Open the command palette |
| **`?`** | Help overlay (rendered from the live key registry) |
| **esc** in a form | Cancel — writes nothing |

### In-text editing

Inside the composer and form fields, ordinary readline-style editing applies:
arrows, `ctrl+a`/`ctrl+e` for line start/end, and the usual word motions. A form
field is where you expect it to be — `tab`/`shift+tab` move between fields, and a
key that is a literal character while you are typing is a literal character, not a
shortcut.

**Prefixes open a picker rather than acting.** A reference you cannot be expected to
type — a model reference, a parent work item, a workflow, an owner scope — is chosen
from a searchable picker in its own modal, not typed.

### Mouse

| Gesture | Action |
|---|---|
| Click | Tabs, list rows, panes, buttons, form controls |
| Wheel | Scroll |
| Drag | Select text; release copies it to the clipboard (OSC 52) |

### Reading a screen

Every list screen shows a **hint line** naming the chords available in the focused
pane, so you never have to guess. Destructive actions are confirm-gated, forms show
their validation inline, and a plane refusal is surfaced **verbatim** in the
composer dock rather than swallowed as a silent no-op.

**Empty states say why.** A pane that is empty names the reason ("no projects yet —
press n to create one") rather than showing a bare "nothing here".

---

# Part III — Accounts, access and the command line

## 22. Authentication

Orchicon authenticates **every** RPC. There is no anonymous dev bypass and no
synthetic dev-login surface — a request without a credential is rejected
everywhere. A fresh plane is bootstrapped by the operator creating their own admin
account through **Create an account** (§2).

### The embedded identity provider

The plane serves a real **OIDC authorization-code + PKCE** flow on its own origin,
so **no external identity provider is required**. It publishes the standard
discovery document, authorize, token, userinfo and JWKS endpoints. ID tokens are
signed with ES256 against a keypair deterministically derived from the configured
signing key, so the JWKS publishes only the public point — never the access-token
secret.

### Local accounts

Human passwords live **only inside the identity-provider boundary** — no service,
RPC or Ask Orchicon tool outside it touches a credential row. Only the **password
hash** is stored (argon2id by default; bcrypt accepted on verify). Plaintext is
never persisted, logged or returned.

Login and sign-up endpoints are both gated on the embedded provider being enabled —
sign-up availability *is* the provider being on. Failures return a **generic** error
with no user-enumeration hint, and no identity is auto-provisioned by a login.

### Bringing your own identity provider

For production you can point Orchicon at a real OIDC issuer and disable the
embedded provider. The BYO path is capability-aware: PKCE is used only when the
issuer advertises it, so a non-PKCE provider gets the same flow as before.

### Sessions in the browser

- **Access tokens are held in memory; refresh tokens live in HttpOnly cookies.**
- A **router-level guard** protects every app route at navigation time —
  unauthenticated visitors are redirected to login *before* any protected component
  renders, with the intended destination preserved so a successful login returns
  there. Login, sign-up and the OIDC callback are the explicit public allowlist.
- **A live session survives reloads** without re-login: the guard and the auth
  provider share one bootstrap that exchanges the refresh cookie for a new access
  token.
- **Sign-out stays signed out.** Sign-out clears the in-memory token, expires the
  refresh cookie server-side, and arms a signed-out flag that stops the guard from
  silently re-authenticating via a still-valid cookie — across reloads and
  navigations.

### API keys

For headless and CI clients, API keys are **SHA-256 hashed** and carry
least-privilege scopes.

### Bootstrap without signing up

The local-admin bootstrap is **opt-in**: it mints a credential only when you pin
both a username and a password. There is no built-in default `admin`/`admin` and no
generated password. The seed is idempotent and never clobbers an existing admin
credential. To re-arm it as **lockout recovery** after losing the admin password,
set the reset flag — same guards, and it overwrites the admin credential on next
boot while keeping the identity and its admin role binding.

## 23. Roles and entitlements

**Admin → Roles**, plus the per-identity **Manage Identity Roles** picker, drive the
role surface. A role carries a name and a set of entitlements.

- The built-in **`admin` role is immutable**: it carries everything, a binding to it
  counts as the tenant-admin bypass, and it can never be renamed, re-entitled or
  deleted — doing so would strand the plane.
- Custom roles are created, edited and deleted through the roles surface.
- Roles are **tenant-global**; project-scoped bindings are a non-goal.
- **Entitlements gate the API**, enforced by an interceptor: a credential without
  the required entitlement is refused at the boundary, not merely hidden in the UI.

## 24. Tenancy

**Each Orchicon deployment owns exactly one tenant.** The deployment is the
isolation boundary; all identities, projects, work items, executions and audit
records are scoped to the seeded deployment tenant.

- The tenant is **config-driven** and validated at boot — a misconfigured value
  fails boot rather than seeding a second tenant.
- **Every auth path resolves logins into the deployment tenant.** Identity-provider
  claims (`org`, `groups`, `tenant`) are **not** consulted for tenant
  selection — a group in your identity provider does not choose your tenant.
- **Row-level security backs it up.** Every tenant-scoped table carries a tenant
  isolation policy, so cross-tenant reads are impossible even through a direct
  query, and the data-access layer additionally scopes by tenant.
- **Membership** is the role-binding model: an identity gains membership by being
  provisioned in the tenant and by holding a role in it.
- The multi-tenant schema is retained unchanged as the foundation for a future
  SaaS phase — a pivot would be additive rather than a rewrite.

## 25. Command-line reference

The `orchicon` binary runs the plane and its supporting services.

| Command | Description |
|---|---|
| `orchicon install [--instance <name>]` | One-command **setup**: pull images, start the runtime daemon, start this instance's Postgres + NATS, start the plane, print connection info. **Idempotent** — re-running converges an existing instance and upgrades a superseded plane. It deliberately does **not** restart a plane that is already healthy on the installed binary |
| `orchicon start [instance]` | Start an **installed** instance: its services (Postgres + NATS), then its plane. Idempotent, and it never installs. This is the command to run after a reboot (there is no autostart) |
| `orchicon stop [instance]` | Stop an instance: its plane, then its services. **Idempotent** — running it on a stopped instance is a successful no-op |
| `orchicon restart [instance]` | Restart this instance so its components re-read their environment. Use it after installing an adapter CLI or changing `PATH` — see *Installing an adapter CLI* |
| `orchicon plane <status\|start\|stop\|restart> [instance]` | The **plane only** — leaves the services running. The mechanism the TUI and the desktop app call |
| `orchicon services <up\|down\|status> [instance]` | The **services only** — this instance's Postgres + NATS on the host. `up` is idempotent; `down` verifies the ports came free, and stops this instance's **own** container from an older install if that is what still holds them. `down --force` also terminates this instance's leftover processes — never one it does not own |
| `orchicon serve` | Run the control plane with the embedded frontend (headless, migrations on boot) |
| `orchicon serve --detach` / `--status` / `--stop` | Manage a background `serve` instance (PID file) |
| `orchicon container` | **Internal.** The supervisor the host services re-exec as; not a supported way to run an instance |
| `orchicon runtime-daemon` | Host process owning the container engine; spawns **per-workflow runtime containers** — the only thing a container is used for |
| `orchicon runtime-supervisor` | Runtime container PID 1 |
| `orchicon runtime-client` | Forwards dispatches into the runtime container |
| `orchicon mcp` | Start the MCP stdio server (exposes the Ask Orchicon tool registry) |
| `orchicon db` | Database maintenance: `backup`, `restore`, `list`, `prune` |
| `orchicon backfill-pr` | Idempotently backfill real PR URLs onto already-completed git-backed runs |
| `orchicon run-context <runID>` | Print a workflow run's `.orchicon` archive (per-step files plus run summary/status) for context-by-reference. Flags: `--grep <pattern>`, `--steps`/`--summary`, `--limit N` |
| `orchicon version` | Print the installed version |

**Every lifecycle verb takes the instance the same way** — a positional (`orchicon stop prod`), a flag
(`orchicon stop --instance prod`), or `ORCHICON_INSTANCE` as the fallback. Naming two at once is refused
rather than guessed, and an unknown flag is an error rather than being silently ignored. The default is
`dev`.

**Scripts:**

| Script | Description |
|---|---|
| `scripts/orchicon.sh` | Start / stop / restart / status / logs / rebuild the **host-resident** plane and its host services for any instance (dev, prod or test); delegates every lifecycle verb to the CLI's own control surface (`orchicon instance` when present, else `install` / `restart` / `serve` / `services`) |
| `scripts/orchicon.sh uninstall <dev\|prod\|test> --yes` | **Remove ONE instance and its data** — plane, container, volume, host state dir (destructive, confirmed, prints what it deletes first; never touches a sibling) |
| `scripts/lib/instances.sh` | The ONE shell home of the instance table (dev/test/prod columns with their ports and data dirs); `cmd/orchicon/install_ports.go`'s `instanceOrder` is the Go home, and `TestInstallPortDefaultsMatchShellInstanceTable` asserts the two agree |
| `scripts/container.sh` | Build / up / down / status / logs / ps / runtime-daemon / runtime-stop for the dev + prod + test container instances (the container-residency shape; `scripts/orchicon.sh` is the day-to-day entry point and the documented teardown) |

**The terminal client is a separate binary:** run `orch` for the TUI described in
Part II.

---

## Where to go next

- **[`ARCHITECTURE.md`](./ARCHITECTURE.md)** — architecture, data model, project
  structure, the Ask Orchicon permission model in full, operator setup, the
  development guide, deployment, the environment-variable reference and
  troubleshooting.
- **`?` in the terminal client** — the live key reference for whatever screen you
  are on.
- **The audit trail** (*Control → Admin → Audit*) — what changed, who changed it,
  and when.

> **Orchicon orchestrates. Runtimes execute.**

## Container engine (Podman) — detected, installed for you, verified

Runtime containers need a container engine. `orchicon install` now **detects** one, **installs
Podman for you** when there is none, brings up the **`podman machine`** on macOS and Windows,
and **proves the engine works** by running a trivial container before it reports containers
available. You never have to type a `podman` command.

- Detection order: `podman`, then `docker` (checked on `PATH` and in the platform's usual
  install locations). Podman is preferred, and it is the only engine Orchicon installs.
  Docker is detected, never installed — Docker Desktop's licence is yours to accept, not ours.
- Minimum Podman version: **4.0.0**. An older Podman is *reported* with its upgrade command
  rather than half-used.
- Installation always uses your platform's installer (`brew` on macOS, `winget` on Windows, the
  distro package manager on Linux) so your package manager keeps it updated. The exact command
  is printed before anything runs, and you are asked first. When the install cannot ask (no
  terminal, or you decline) it prints the command and continues in **local mode** — a failed or
  declined Podman install never breaks a working install.
- Availability is shown in the UI where execution mode is chosen: *containers ready*, *being
  provisioned*, or *unavailable* with the reason, and the same answer is available at
  `GET /enginez`.

Environment overrides:

| Variable | Meaning |
|---|---|
| `ORCHICON_PROVISION_ENGINE` | `ask` (default), `1` (install without asking), `0` (never install). |
| `ORCHICON_CONTAINER_ENGINE` | Pin the engine: `podman`, `docker`, or `none` (local mode). A pin is honoured strictly — a pinned engine that is missing or broken fails the install loudly instead of silently falling back to local mode. |
| `ORCHICON_PODMAN_MIN_VERSION` | Accept an older Podman than the documented minimum. |
| `ORCHICON_ENGINE_VERIFY_IMAGE` | The trivial image used to verify the engine (default `docker.io/library/alpine:3`) — point it at a local image on an air-gapped host. |
