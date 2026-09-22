# Running Orchicon outside the control-plane container

**Status:** research memo — findings and options. No decision taken, nothing
implemented. The decision record (ADR) belongs after the operator picks a scope.

**Date:** 2026-09-22

**Verified against:** `develop` @ `3945f6b7`.

**Question:** can `orchicon` — or at minimum Ask Orchicon — run *outside* the
control-plane container, so it behaves like any other harness on the machine?
Runtime containers stay, and the services (Postgres, NATS, telemetry) stay where
they are.

Environment facts below marked *(observed)* were read from **inside a live plane
container** (the session this memo was written in), not inferred.

## 1. What runs where today

| Component | Where it runs | Evidence |
|---|---|---|
| Plane (`serve`: reconcilers, RPC, sessions) | in the container | `cmd/orchicon/serve.go`; started by the container supervisor |
| Postgres, NATS, OTel collector, Tempo, Loki, VictoriaMetrics, Grafana | in the container | `cmd/orchicon/container.go:547-630` |
| The runtime daemon (owns the Docker socket) | **on the host** | `cmd/orchicon/runtime.go:17-21` — *"Run it on the host, not in a container"*; socket mounted at `/var/run/orchicon-runtime` (`scripts/container.sh:522`) |
| Worker sessions | inside the always-container **run** container | `internal/db/project.go:67-71` (`ExecutionModeRuntime` is the default); containers are spawned by the daemon |
| The opencode serve for local/in-process executions | in the container — it is a child of the plane | `internal/opencode/servehost.go:36-45` (spawned from the plane's own PATH — `exec.LookPath("opencode")` at `:334` — and binds `127.0.0.1:<port>`) |
| Ask Orchicon's turn loop | in the container, in-plane | `internal/askorchicon/chat.go` drive; no dispatcher hop today (`docs/ask-orchicon-adapter-design.md` §1) |
| Ask Orchicon's file/shell suite (`read`/`write`/`edit`/`bash`/…) | **in the plane process** | `internal/askorchicon/native_tools.go:14-35` — `orchicon.NewHostTools` → `internal/worktree`, root = the tenant's first active `project_dir` (`AskFileRoot`, `:139`) |
| `orch` (TUI client) | on the host | `cmd/orch/main.go:1-15` — *"carries zero control-plane code"* |

So exactly **one** long-lived Orchicon process runs on the host today (the runtime
daemon), and it is deliberately narrow: it owns the Docker socket and serves a
workflow-runtime API over a unix socket. Everything that thinks — the loop, the
tools, the sessions — happens in the container. (`orch` is a client, not a
harness: it holds no control-plane code and executes no tools.)

### 1.1 What the container is given *(observed)*

- `ORCHICON_PROJECT_ROOTS` (default `$HOME`) is bind-mounted **writable**;
  `scripts/container.sh:475-487`. In this session `/home/beardedparrott` is
  `rw`, and a write to `$HOME` from the file/shell suite **succeeded**.
- The read-only mounts (`~/.opencode`, `~/.gitconfig`, `~/.git-credentials`,
  `~/.config/gh`, `~/.local/share/gh`, `~/.config/opencode`) are re-asserted
  *inside* that root; the ordering is deliberate and commented
  (`scripts/container.sh:470-474`) — moving them above the root would silently
  make opencode's config writable by the plane.
- Project dirs are mounted at **the same absolute path** on both sides
  (`scripts/container.sh:531`), so paths the plane reports are already
  host-valid.
- The plane runs as the host uid/gid (`ORCHICON_HOST_UID/GID`, asserted at
  `scripts/container.sh:577-579`) — *(observed)* `id` = `1000:1000`, the
  operator's own user.
- No Docker socket (`/var/run/docker.sock` absent *(observed)* — by design; the
  host daemon owns it), and only the plane + Grafana ports are published
  (`scripts/container.sh:47,56`) — Postgres and NATS are **not** published, so
  the host cannot reach them at `localhost` today.

### 1.2 The one guard that is actually doing the containment

`internal/guard` shims dangerous binaries ahead of the tool environment's PATH:
always-block (`sudo`, `dd`, `mkfs*`, `fdisk`, `parted`, `shred`, `wipefs`, LVM,
`mkswap`) and path-scoped (`rm`, `chmod`, `chown`, `mv`, `cp`, `ln` — allowed
only when every path argument resolves inside the project dir).

Where it is wired today, by path:

| Path | Guard source |
|---|---|
| Ask Orchicon's file/shell suite | `AskGuardEnviron` (`internal/askorchicon/ask_guard.go:80-96`) |
| Worker runs in the runtime container | the runtime agent prepends the shim to the container env (`internal/runtime/agent.go:456`) |
| In-process opencode (local mode) | `internal/opencode/guard.go` → `guard.NewExecutionGuard(projectDir)` |
| **Native HostTools bash, in-process** | **not wired** — `envForBash` is documented as set by the Ask path only, and `nil` leaves the inherited env untouched (`internal/orchicon/hosttools.go:57-62, 86-90`) |

That last row is a reading, not a test, and it is narrower than it sounds: it
only applies when native bash executes **in-process** (local mode / standalone /
headless). In the default always-container mode bash is routed into the run
container, whose env already carries the shim. Worth confirming before it is
relied on — it is the one path where the guard may not be the backstop.

The Ask guard's own comments state the intent plainly (`ask_guard.go:80-86`:
without it, *an Ask conversation could `rm -rf` outside the project while a
worker would be refused*) and the guard's limits:

> The guard is defense-in-depth, not a substitute for containers: a determined
> worker could still call `/bin/rm` by absolute path or write its own binary.
> That residual risk is what the containerized execution option closes.
> — `internal/guard/guard.go:36-39`

It also records *why* it exists: the 2026-07-30 `/home` wipe, which came from a
destructive command issued from inside a subprocess that the opencode permission
rules could not see (`guard.go:11-20`).

*(observed)* The shim is a PATH overlay over **named binaries**, so anything that
does not go through a shimmed name is unconstrained: a shell redirection
(`echo x > $HOME/file`), `tee`, `sed -i`, `python -c`, `git`, `install` all write
outside the project directory. The write probe above used exactly that.

## 2. What the container actually costs today

Three concrete gaps — each is a capability the operator wants and cannot have:

1. **Docker.** There is no Docker socket and *(observed)* no `docker` binary in
   the container. So neither Ask nor a local-mode execution can build or run a
   container, inspect one, or use the host's images. The runtime daemon
   compensates for exactly one caller (the scheduler's per-run containers) with
   a narrow API — nothing else can ask the host to run anything.
2. **Host-local services.** *(observed)* There is no `host.docker.internal`
   alias, so a service bound to the host's loopback — a local model
   (`:11434`), a dev server, a database the operator is working with — is not
   reachable by name from the plane. This is the "Ask Orchicon can't use my
   local model" class of friction.
3. **The operator's own MCP servers, when they need the host.** The code already
   names this one: an `orchicon` MCP that `docker exec`s, or a local Node-based
   Playwright MCP, *"cannot run inside the sandbox — an unresolvable MCP hangs
   the serve's event loop"*, so they are **skipped** for runtime serves
   (`internal/opencode/config.go:428-434`, `internal/opencode/adapter.go:299-303`).
   The user's own tools are dropped to fit the harness into the box.

What is **not** a gap, contrary to the intuition: file access and toolchains.
Because `$HOME` is mounted writable and the project is at its host path, the
in-container harness can already read and edit the real tree, run the real
`go build`/`go test` (this memo's own verification ran the full Go suite from
inside the container, reaching the toolchain through the mounted `$HOME`), and
push via the mounted git credentials. *(observed: `docker`, `go`, `node`, `npm`,
`tmux` are absent from the container's PATH — the toolchains are reachable only
because the trees that hold them are mounted.)*

## 3. What "outside the container" can mean

Four scopes. They are not alternatives you must pick one of; they stack.

| Scope | What moves out | Rough effort (my judgement) |
|---|---|---|
| **A. Tools** | the file/shell suite + `bash` execution → a host process | days |
| **B. Harness** | the session loop + tools → a host session server, dispatched to like an adapter | weeks |
| **C. Plane** | the whole `serve` process → the host, services left in the container | days-to-weeks, mostly deployment |
| **D. Reach** | nothing moves; the container is given the reach it lacks | hours |

### Scope A — the tools run on the host

The engine already exists and is root-parameterised: `orchicon.NewHostTools(root)`
with an injectable bash environment (`internal/askorchicon/native_tools.go:170-175`),
over the shared `internal/worktree` composite engine. The host-side pattern also
already exists: a daemon that owns host-side work and serves it over a socket
directory bind-mounted into the container (`/var/run/orchicon-runtime`).

Work: a host-side runner exposing the suite over that socket (same containment
root and guard environment as today); a client implementing the same interface
the plane already calls (`ToolRegistry`), wired at the single seam that resolves
the Ask tools (`native_tools.go:173`); and the refusal/approval semantics kept
plane-side so the mode gate (`internal/askmode`) is unchanged.

Payoff: `bash`, builds, git, and the operator's toolchains execute as a normal
host process, and the operator's host-needing MCP servers stop being skipped.
Cost: the tool calls cross a process boundary (latency, failure modes, timeouts),
and the host runner must be trusted — see §5.

### Scope B — the harness runs on the host (the opencode-equivalent destination)

This is what "Orchicon as a harness, like opencode" means concretely: a host
process that hosts sessions, with the plane as orchestrator.

The pieces that make it more tractable than it sounds:

- The native engine is already a library (`internal/orchicon`: session, loop,
  transcript, prompts) that the bridge drives in-process; a host process can
  host the same engine.
- The transport shape to mirror already exists: `internal/opencode/session.go`
  (HTTP+SSE: create session, send message, stream, abort) is the contract a
  session host implements, and the Ask design has already committed to an
  **adapter-neutral `sessionTurnClient`** (`docs/ask-orchicon-adapter-design.md`
  §2.2, §2.3) for exactly this class of move.
- Adapter routing is pluggable by construction: `Dispatcher.Register/Resolve`
  (ADR-0003) plus `adapter.DemandSet` (`internal/adapter/demand.go`) already
  decide *which kinds this plane needs a serve for* — a host harness is another
  kind to demand, and a native-only plane keeps costing nothing.

Work: the serve surface around the native loop; a dispatcher adapter that dials
it; auth (the opencode serve's basic-auth pattern, or the socket's trust model);
transcript/part persistence (today the plane records `execution_session_parts`
for the UI, so a host loop must ship parts back or the recording must move);
worktree/root semantics for host execution; and the same per-project opt-in
question as Scope A.

Payoff: the loop, the tools, the model client, and the MCP connections all run
where the work is — no container round trip, no skipped MCP servers, no
container-network surprises. This is the version of Orchicon that competes with
opencode on its own terms.

### Scope C — the whole plane on the host, services left in the container

Cheaper than it looks, because the plane is already a host-runnable binary and
reads **everything** from its environment: `ORCHICON_POSTGRES_DSN`,
`ORCHICON_NATS_URL`, `ORCHICON_TEMPO_URL`/`LOKI`/`VM`/`GRAFANA`, `HTTP_ADDR`,
`DATA_DIR`, `BLOB_DIR` (`internal/config/config.go:182-189`; the container
supervisor sets the same names for the in-container plane, `container.go:701-706`).
The runtime socket is already host-side, so the planner→daemon path keeps
working unchanged.

What it needs:
- Published service ports, loopback-only: `-p 127.0.0.1:5432:5432` (Postgres),
  `4222` (NATS), `4317/4318` (OTel in), and Grafana if the plane should link it.
- A decision about which process owns the reconcilers: one plane, in one place
  (the container's `orchicon serve` would have to stop being started).
- A second supported deployment shape, documented and tested — that is the real
  cost, not the code.
- The `orchicon` binary on the host (`scripts/install-local.sh` already builds
  and installs one; `orchicon serve --detach` is the native run path).

Note on the DB: the plane's own DSN would then be the live one, which is
correct — the DSN fence in `ExecutionMode=local` exists to stop **workers**
writing to the live plane's database from in-process runs, not to constrain the
plane itself (`internal/db/prompt.go:109-160`, `project.go:53-71`).

### Scope D — keep the container, widen its reach

- `--add-host=host.docker.internal:host-gateway` on the plane container makes
  host-local services reachable by a stable name (closes gap 2 for a one-line
  change, and gives the prompt somewhere honest to point).
- Widen the runtime daemon's narrow API to the host capabilities the harness
  needs (docker build/run/inspect, and whatever else the operator's MCP servers
  need) — one privileged owner on the host, no socket in any container.
- `ORCHICON_PROJECT_ROOTS` already narrows what the plane can touch; it is not
  currently a security boundary in practice (§1.1).

## 4. Recommendation

Staged, cheapest-first, each step independently useful:

1. **D-lite now** (hours): `host.docker.internal` on the plane container, and a
   line in the runtime prompt that names it. Local models and dev servers become
   reachable; nothing else changes.
2. **D-next** (days): extend the runtime daemon's API to the host capabilities
   the harness lacks (docker verbs first), so the operator's docker-needing MCP
   servers stop being skipped and Ask can build/run containers without the
   socket ever entering the container.
3. **A** (days): move the file/shell suite to a host runner over the existing
   socket pattern. This is where "performs like any other harness" is actually
   felt, with the loop left in the plane.
4. **B** (weeks): the host-side harness as a first-class runtime, per-project
   opt-in, using the demand set and the adapter-neutral session client the Ask
   design already commits to. This is the opencode-competitor landing.
5. **C** (optional): hold it until/unless a fully native dev loop is wanted —
   it delivers the same capabilities as A+B while adding a second deployment
   shape to keep working.

I would not start with B or C. Both are more work than the friction they remove
today, and A+D-next capture most of the felt benefit.

## 5. Security: what actually changes

The honest starting point — the container is **not** currently a strong boundary
for the Ask/local path:

- It runs as the operator's own uid, with `$HOME` mounted writable.
- Containment for that path is the **guard**, which is a PATH overlay over named
  binaries and is porous by design and by its own documentation.
- The strong boundary in this system is the **per-run runtime container**
  (always-container is the default for dispatched work: `project.go:67-71`,
  `internal/runtime/`), which gets the run's worktree at its absolute path plus
  the adapter dirs it needs, read-only (`internal/runtime/bootprofile.go:175-205`)
  — not `$HOME` wholesale.

So moving the harness out of the plane container changes:

| | in the plane container | on the host |
|---|---|---|
| Filesystem reach | `$HOME` rw (mount config) | everything the user can touch |
| Guard | same shims | same shims (keep them) |
| Namespace isolation | PID/net/proc separate | none |
| Docker | none (socket stays on the host) | the user's docker access |
| Host-localhost services | unreachable | native |

For the Ask/local population, the *privilege* delta is small (same uid, same
guard, `$HOME` already writable); what is lost is namespace separation, and what
is gained is exactly the capability the operator is asking for. For dispatched
worker runs, **nothing should change** — keep the runtime container as the
default, so a harness that runs on the host is never where untrusted work lands.

Mitigations I would require of any Scope A/B landing:

- keep the guard on the host harness's PATH, and keep the project directory as
  the containment root;
- make host execution **opt-in per project**, mirroring `ExecutionMode`, so the
  default stays container-bounded and the choice is visible in the project row;
- state the truth in the prompt (the repo already has the pattern for this: an
  honest environment block that differs from the container block,
  `internal/db/prompt.go:120-160`);
- never mount the Docker socket into a container — route privileged host work
  through the one host-side daemon that already owns it;
- and, separately from this work: treat the plane's writable `$HOME` mount as
  the thing worth tightening. It is the largest standing exposure in the current
  design, and it exists for convenience (toolchains, git credentials), not by
  necessity.

## 6. Open questions for the operator

1. Which is the goal — more capability for Ask's tools (A), Orchicon-as-a-harness
   competing with opencode (B), or a fully native dev loop (C)? The stages differ
   by an order of magnitude in effort.
2. Should host execution be the default for Ask, or opt-in per project?
3. One host daemon with a wider API (the runtime daemon grown up), or a second
   process with its own socket?
4. Does the harness get Docker directly (host process, never a container), or
   only through the daemon's API?
5. Is the writable `$HOME` mount on the plane container worth narrowing now,
   independently of this work?

## 7. Limits of this memo

- Nothing here was implemented or prototyped; the effort bands are judgement, not
  measurements.
- The environment facts are from **one** deployment (this dev instance, default
  mount config). A narrowed `ORCHICON_PROJECT_ROOTS` changes §1.1 and §2's "not
  a gap" conclusion.
- I could not test a host-side plane from inside the container (by definition),
  so Scope C's port/publishing list is read from the code, not exercised.
- `ExecutionMode=local` was read, not run end-to-end; its DSN fence was not
  exercised.
- The per-run runtime container's exact mount list comes from the plane's request
  (`internal/runtime/daemon.go`, `MountSpec`) plus `adapterHostMounts`; I read
  both, but did not capture a live container's mounts.
