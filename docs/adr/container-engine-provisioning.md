# ADR: container engine provisioning (detect, install, `podman machine`)

Status: accepted. Deliberately unnumbered — `0013` is claimed by the boundary ADR in PR #737.

## Context

Runtime containers are an execution mode the operator chooses in the UI. Until now the engine
behind them was an assumption: `orchicon install` probed `PATH` for a literal `docker` binary
and, if it was missing, silently installed in local mode. "Containers are available" therefore
meant "you happened to have Docker", and the operator's only recourse was to work out which
engine to install and how.

The operator chose the opposite: Orchicon detects and provisions the engine, including
`podman machine` on macOS/Windows, so the user never types a `podman` command.

## Decision

1. **One library owns the truth: `internal/containerengine`.** The installer (`cmd/orchicon`,
   `package main`) and the plane (`internal/api`) must answer the same question with the same
   rules; `package main` cannot be imported, so the rules live in a package both can call. The
   engine search order is: `ORCHICON_CONTAINER_ENGINE` pin → `podman` on `PATH` → podman in the
   platform's conventional locations → `docker`. Podman first because it is the engine we can
   legally provision and it is rootless by default; docker second so existing users are
   unaffected.
2. **Minimum Podman version 4.0.0**, in one constant, in the error message, and overridable
   with `ORCHICON_PODMAN_MIN_VERSION`. 4.0 is the first release whose `podman machine list
   --format json` / `machine init --now` shape and docker-compatible flags this code relies on.
   A too-old engine is REPORTED with its upgrade command, never attempted.
3. **We provision with the platform's installer, and never bundle a runtime.** macOS
   `brew install podman`; Windows `winget install RedHat.Podman` (fallback
   `RedHat.Podman-Desktop`); Linux the first of `dnf`/`apt-get`/`zypper`/`pacman`/`apk`.
   Podman is Apache-2.0, so bundling would be permissible — we decline anyway: the user's
   package manager owns upgrades and security patches, we never ship a second copy of a runtime
   that can drift, and nothing we redistribute can be complained about later. This is the same
   "mount, never bake" posture the adapter CLIs follow. Docker is DETECTED, never installed:
   Docker Desktop's licence is not ours to accept on a user's behalf.
4. **Never silent.** The exact command is printed before anything runs, and the install asks
   first (`ORCHICON_PROVISION_ENGINE=1|0|ask`, default `ask`). A run with no controlling
   terminal prints the command and declines — a piped install can never install a VM image
   behind the operator's back. A root-requiring package manager is printed with its `sudo`
   prefix and never escalated by us.
5. **`podman machine` states are distinct.** Absent → `machine init --now`; stopped →
   `machine start`; running → nothing; failed → the engine's own reason plus two remedies
   (`podman machine rm` then retry, or `ORCHICON_CONTAINER_ENGINE=none`). Linux (and the WSL
   install path, which runs the Linux binary) answers "not applicable" and runs nothing.
   Reachability is proven by polling `podman info`, so "the VM process exists" never counts as
   "the engine answers".
6. **"Installed" is not "working".** Before containers are reported available, a trivial
   container runs (`<bin> run --rm docker.io/library/alpine:3 /bin/true`). The first run may
   have to pull, so it is bounded generously rather than health-checked.
7. **Availability is visible where execution mode is chosen.** `GET /enginez` (plain JSON next
   to `/versionz`, no proto change) serves `{engine, version, minimum, available, state,
   reason, machine}` with a 30s memo and `?refresh=1`; the projects UI renders
   ready / provisioning / unavailable-with-reason under the execution-mode select.
8. **A failure never breaks a working install.** Every rung falls through to the existing host
   shape (host services + in-process runs). The one fatal case is an explicit
   `ORCHICON_CONTAINER_ENGINE` pin, because an operator who named the engine asked for it.

## Consequences

- A fresh macOS/Windows host can go from nothing to containers in one `orchicon install`, and
  a Linux host gets the distro package (or a printed `sudo` line when the install is not root).
- The install path now drives the RESOLVED engine binary rather than a hardcoded `docker`
  (`engineCommand` in `cmd/orchicon/install_engine.go`, and `DockerBin` in `runtime.go`), which
  is the seam the daemon-repoint work item also consumes. Podman's docker-compatible CLI is
  what makes that substitution safe.
- Availability has two sources of truth at the edges: the install's decision (reported in the
  summary) and the live probe (`/enginez`). They agree because they share
  `containerengine.Inspect`; the UI never re-derives rules of its own.
- Provisioning is opt-in, so an operator who declines stays in local mode with a printed
  command and no partial state — the marker file is removed on both success and failure.
