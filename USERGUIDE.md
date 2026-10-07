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
12. [Account screens: Login and Sign up](#12-account-screens)

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
- **Docker** (for the single-container deployment; the headless `orchicon serve`
  binary needs no external services)
- **curl** + **tar** (for one-liner install)
- **buf** and **atlas** (install via `make tools`)
- *(optional)* an adapter CLI — only if you want to dispatch work to an external
  runtime such as OpenCode. Orchicon's built-in engine needs **nothing installed**.

> **Adapters are optional, and Orchicon never ships one.** Its built-in native
> engine runs sessions inside the control plane, so a fresh install is complete
> with **no adapter CLI present** — nothing is probed for, and nothing fails. If
> you want to run work on OpenCode — or on the **Claude** adapter (provider
> `anthropic`), which authenticates with your own host claude.ai login — install
> it **on the host** and, for Claude, sign in there once. The CLI is bind-mounted
> into the containers at runtime and resolved from `PATH` (`~/.opencode/bin` for
> OpenCode, `~/.local/bin` for Claude). The images contain no adapter binary,
> which keeps the product redistributable regardless of an adapter's licence
> (Claude Code's terms, for example, prohibit bundling it with a product).

### One-line install (Linux / macOS)

```bash
curl -fsSL https://orchicon.dev/install | bash
```

The installer downloads the binary, then runs `orchicon install` to set up
**everything**: pull the published images
(`ghcr.io/beardedparrott/orchicon` + `orchicon-runtime`), start the host-side
runtime daemon, launch the single-container instance, and print how to connect /
start / stop. Pass `--no-setup` to install only the binary (headless / CI).

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
   (Docker Desktop with WSL2 integration, or Docker Engine installed in the
   distro) and prints setup steps if not.
3. **Linux binary** — it downloads the **Linux** release asset
   (`orchicon_<ver>_linux_<arch>.tar.gz`, arch mapped from the Windows processor)
   and installs it inside the distro (default `~/.local/bin`); it never downloads
   the Windows binary.
4. **One-command setup** — it runs `orchicon install` inside WSL: pull the
   published images, start the runtime daemon, launch the single-container
   instance, wait for health.
5. **Connection info** — WSL2 forwards `localhost`, so it prints the
   Windows-visible URLs: `http://localhost:8080` (control plane) and
   `http://localhost:3002` (Grafana). If the URLs do not answer, check Windows
   Defender Firewall or add a `netsh interface portproxy` port forward.

**Prerequisites:**

- **Windows 10 21H2+ / Windows 11**. First-time WSL users run `wsl --install`
  from an elevated PowerShell and reboot; the installer guides through this if it
  is not yet set up.
- **Docker Desktop** with the **WSL integration** enabled for your distro
  (Settings → Resources → WSL Integration), or Docker Engine running inside the
  distro.

**Project directories are WSL paths.** The UI's `project_dir` is a host path.
Inside WSL2 a Windows project `C:\Users\you\projects\Foo` is mounted by Docker
Desktop at `/mnt/c/Users/you/projects/Foo` — enter that **WSL path** in the
project form. There is no Windows↔WSL path translation layer; the UI accepts
whatever path the plane's filesystem (WSL) can resolve.

**Options** (PowerShell form of the Linux flags): `-Version`, `-InstallDir` (a
**WSL** path, default `~/.local/bin`), `-NoSetup`, `-Uninstall` (stops the
container instances and removes the binary; the WSL distro is left intact),
`-Clean`, `-ForceClean`, `-DryRun`.

> **Not verified on real Windows** — the WSL2 installer ships as-is for testing.
> The underlying flow (`orchicon install`) is the identical Linux code path
> exercised on Linux hosts.

### Install options

| Flag | Description |
|---|---|
| `--version <tag>` | Install a specific version (e.g. `v0.4.0`). Default: latest. |
| `--install-dir <dir>` | Installation directory (default: `~/.local/bin`). |
| `--uninstall` | Remove Orchicon from the install directory. |
| `--dry-run` | Print what would happen without making changes. |
| `--clean` | Stop any running instance, remove old binary, then install latest. Preserves all data. |
| `--force-clean` / `--nuke` | Remove container instances + data volumes, blob store, runtime state, then install latest. **All data lost.** |

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

### Run the container directly

The whole Orchicon stack (Postgres, NATS, Tempo/Loki/VictoriaMetrics/Grafana,
control plane) runs in one container:

```bash
docker run --rm -p 8080:8080 -p 3002:3000 -v orchicon-data:/var/lib/orchicon ghcr.io/beardedparrott/orchicon
```

The `orchicon` binary is the PID-1 supervisor (`orchicon container`).

### Build from source

```bash
git clone https://github.com/beardedparrott/Orchicon.git
cd Orchicon
make build                  # → bin/orchicon (headless control plane + PID-1 supervisor)
make container-build        # build the single-container image
scripts/container.sh up dev # start the dev instance (http://localhost:8080)
```

With the frontend in dev mode: `make fe-install` (first time) then `make fe-dev` —
the Vite dev server on `:5173` proxies the API to `:8080`. Migrations run
automatically on container boot (embedded runner).

### Verify the installation

```bash
orchicon version
curl http://localhost:8080/healthz   # {"status":"ok"}
```

---

## 2. First run

A fresh Orchicon plane has **no accounts**. Authentication is real from the first
request — there is no anonymous or synthetic dev-login bypass — so the first thing
you do is create your account.

1. **Install and start the stack** (§1). The UI is at `http://localhost:8080`.
2. **Create the first account.** Open the UI and click **Sign up**. The **first
   sign-up on a tenant with no admin becomes the tenant admin** — granted
   atomically with account creation. Subsequent sign-ups are plain `user`
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

New planes also ship **canned workers** (Senior Software Engineer, PR Reviewer, QA
Engineer, DevOps Engineer, Design/Code Approver, Principal Software Architect, and
their **- Vision** variants), so you can run something end to end without writing
a worker from scratch.

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

**They share one default look.** The GUI's default dark theme is **Teal Depths**
and the TUI's launch default is the matching **`teal`** palette, so a fresh GUI and
a fresh TUI open in the same colours. Each is a *default, not a pin* — a stored
preference always wins.

---

# Part I — The web GUI

## 4. Getting around the GUI

The GUI is a single-page app with a persistent **sidebar**, a **topbar**, and a
content area.

**The sidebar** groups screens into seven domains. Each domain is a collapsible
section; the active domain and item are highlighted, and the topbar shows your
position as a breadcrumb.

| Domain | Screens |
|---|---|
| *(top level)* | **Ask Orchicon** |
| **Overview** | Dashboard · Telemetry · Cost Explorer |
| **Work** | Projects · Work Items · Runtime Images |
| **Execution** | Workers · Workflows · Executions · Schedules · Recovery |
| **Automation** | Recurring Items · Idea Cloud |
| **Enforcement** | Approvals · Policies |
| **Control** | Webhooks · Adapters · Settings · Admin *(admin only)* |

**Note on the course of the app.** `/` (the root) opens **Ask Orchicon**, not a
dashboard — asking is the front door. A few convenience paths redirect to their
canonical screen: `/home` and `/overview` → Dashboard, `/usage` → Cost Explorer,
`/conversations` → Ask Orchicon.

**Theme and appearance** are set in *Settings → Appearance* (light/dark plus 20
theme variants — 10 light, 10 dark).

**Long-running lists refresh themselves.** Screens that show live state (Work
Items, Schedules, Executions) poll on a short interval, pause while the tab is
hidden, and refetch when the window regains focus. A `Live HH:MM:SS` indicator
makes the refresh visible rather than mysterious.

There is deliberately **no GUI command palette**. The GUI is mouse-first and gives
every per-conversation command a native control, so a palette would re-implement
controls that already exist. "Commands" in the GUI therefore means **the actions
and buttons on each screen** — documented per screen below.

---

## 5. Ask Orchicon

**Route:** `/ask-orchicon` · top of the sidebar

**Purpose.** A conversational partner that shares the platform's understanding of
your project. It can answer questions, read your files, plan and author work, do
the work itself, and dispatch it — depending on the **mode** you select.

### Layout

- **Transcript** (centre) — the conversation, with streaming replies, reasoning
  bubbles, tool-call cards, and an **activity line** that reports what is happening
  while a turn runs.
- **Conversations sidebar** (right) — your history. Switch, resume, rename or
  delete conversations. A **pulsing dot and Stop button** appear on any
  conversation with a turn running, so a turn is stoppable even when you are
  looking at a different conversation.
- **Composer** (bottom) — the input, with a mode dropdown and a **model chip**.

### The three modes

Mode is per-conversation, shown as a dropdown in the composer. All three share one
identity, one project awareness and one tool surface; they differ in their
**disposition toward action** — and the boundary is **enforced by the platform**,
not requested in prose.

| Mode | Disposition | What it does |
|---|---|---|
| **Brainstorm** | plans and decides | Investigates, designs, asks clarifying questions, authors work items. Refuses to write or execute. |
| **Iteration** | does the work | Cuts the branch, edits files, runs the tests, commits — working alongside you in this session. |
| **Quick Work** | dispatches it | Creates an ephemeral worker, workflow and work item, fires the run, and cleans up after itself. |

Each mode **refuses** the tools its disposition does not own. Brainstorm and Quick
Work are withheld `write`, `edit` and `bash`; Iteration is refused the plan and
dispatch tools. The refusal arrives as the tool's result, so the model relays it
rather than silently failing. There is **no mode-setting tool** — the model cannot
switch itself, and the mode selector is the only route. This is what makes "ask
the user to switch" a real instruction rather than a bluff.

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
| **Mode dropdown** | Switch between Brainstorm / Iteration / Quick Work. |
| **Model chip** | Opens the three-tier **model picker** (adapter → provider → searchable model) and sets the model for the open conversation. |
| **Paperclip** | Attach a file to the next message. |
| **Diff sidebar toggle** | Slide out the file-diff pane for the current work. |

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

**Route:** `/dashboard`

**Purpose.** The aggregate state of the plane in one view.

**What it shows.** Executions and work items grouped by status, worker and
runtime-image health, recent activity, and cost rollups (per project, plus
per-workflow costs).

**How to use it.** Open it first after a fresh load to see whether anything is
running, failing, or stuck. Every figure links through to the domain that owns it.

**Actions.** Read-only. Use the links to drill into Executions, Work Items,
Workers, Recovery, or Cost Explorer.

### 6.2 Telemetry

**Route:** `/telemetry`

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

**Route:** `/cost-explorer`

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

> **A note on cache-aware cost.** Cost is cache-aware, and the `tokens` budget gate
> counts **fresh tokens only** — cache reads are excluded, because cache reads are
> re-sends of context already counted. A long-context worker cannot trip its token
> ceiling by re-reading what it already holds; cache reads still govern real spend
> through the cost gate.

---

## 7. Work domain

The domain where you describe *what* should be done and *where*.

### 7.1 Projects

**Routes:** `/projects` (list) · `/projects/new` · `/projects/$id` (detail)

**Purpose.** A project is the top-level container for work. It owns a
`project_dir` — the directory workers operate in — plus goals and context files
that are injected into every worker prompt.

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

**Routes:** `/work-items` (tree + board) · `/work-items/new` · `/work-items/$id`
(detail) · `/work-items/graph`

**Purpose.** The unit of work. Work items form a hierarchy — **Epic → Feature →
Task → Subtask**, max 4 levels — and a dependency **DAG** on top of it.

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

**Things that behave in a specific, deliberate way:**

- **Blocked is a real status.** An item that is armed but cannot dispatch because
  an upstream dependency is not yet terminal-success shows as **`blocked`** (its
  own teal pill, distinct from `pending`/`scheduled`) with the blocking items
  named — so you can see *why* nothing is dispatching. It clears automatically
  once the dependency is met. Only a terminal **success** unblocks: a failed or
  cancelled blocker leaves the dependent blocked and visible for you to resolve.
  Blocked is system-managed — it is not manually movable on the board.
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

**Detail page (`/work-items/$id`).** Kind badge, state pill, parent, priority,
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
(→ cancelled). Bulk operations act on the current selection.

### 7.3 Runtime Images

**Routes:** `/runtime-images` (list) · `/runtime-images/new` ·
`/runtime-images/$id` (detail)

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
`:web-research` image.

**Actions.** Create, edit the spec, **build** (with live streamed logs), delete.

---

## 8. Execution domain

The domain where the work actually happens.

### 8.1 Workers

**Routes:** `/workers` (list) · `/workers/new` · `/workers/$id` (detail)

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
single decision signal** — there is deliberately no separate `_decision:` or
`_issues:` channel that can override it, which removes a class of false failures
where a reviewer's prose was misparsed as an issues block.

**Actions.** Create, edit, new version, publish, deprecate, set active, set model
in bulk across a selection, delete (bulk-capable).

### 8.2 Workflows

**Routes:** `/workflows` (list) · `/workflows/new` · `/workflows/$id` (detail) ·
`/workflows/$id/runs/$runId` (run detail)

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

**Run detail (`/workflows/$id/runs/$runId`).** Per-step status, the run body, and
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

**Routes:** `/executions` (list) · `/executions/$id` (detail)

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

**Route:** `/schedules`

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

**Routes:** `/recovery` (list) · `/recovery/$id` (detail)

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

**Routes:** `/recurring-items` (list) · `/recurring-items/new` ·
`/recurring-items/$id` (detail)

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

**Route:** `/idea-cloud`

**Purpose.** A triage board for proposals Orchicon generated itself, kept
**separate from your real work items**.

**The flagship producer** is the **Automation Research** pipeline (a three-worker
crew — Planner → Analyst → Synthesizer) that surveys the market live, verifies each
candidate against external evidence, and distills feature proposals. Any recurring
fire configured with "Outputs: ideas" lands here too.

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

**No cross-fire carry-forward.** An idea's facts are scoped to its run, deliberately.
Across fires, lifecycle state is the ledger: the Rejected section is the durable,
bounded memory of what was proposed and refused, while accepted history lives in the
normal work items. No prompt block compounds with recurrence.

---

## 10. Enforcement domain

### 10.1 Approvals

**Route:** `/approvals`

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

**Route:** `/policies`

> **Coming soon.** The Policies surface is a **deliberate placeholder** — the route
> stays registered (so the nav entry leads somewhere honest rather than 404ing) and
> both clients say "Coming soon…", using the same words so they cannot disagree
> about the state of the feature. The RPCs and the proto remain in place, so the
> surface returns when the design is settled; it is the untested UI shape that was
> removed, not the capability.

**So what enforces policy today?** Policy *decisions* are still evaluated and
recorded by the plane — you see them as **policy context** on the Approvals
surface, and in the **Decisions** trail under Admin. Policy is enforced at
admission, dispatch, budget, approval, recovery and completion points, with
narrowest-scope-first evaluation and a default of allow.

---

## 11. Control domain

### 11.1 Webhooks

**Route:** `/webhooks`

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

**Route:** `/adapters`

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

**Route:** `/settings`

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

**Route:** `/admin` · **admin role required**

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

There is **no tenant-level MCP screen**, and that is deliberate: an MCP server
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

> **The honest consequence, stated rather than hidden:** a server can no longer be
> defined once and inherited by several projects. The catalog's one-click add per
> scope is the mitigation.

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

**Route:** `/login`

**Purpose.** Sign in.

**How to use it.** The page is **honest about the running plane**: it fetches the
plane's public auth capability flags and renders only the methods actually
available.

- **Local account** — username + password, verified against the embedded identity
  provider.
- **SSO** — when an external OIDC identity provider is configured, an SSO button
  takes you through the authorization-code flow.
- **Sign up** — linked only when the plane advertises it (sign-up availability *is*
  the embedded provider being enabled).

Failures return a **generic** error with no user-enumeration hint, and no identity
is auto-provisioned by a login.

**You land where you were going.** An unauthenticated visit to any protected route
redirects here with the intended destination preserved, and a successful login
returns you there.

### 12.2 Sign up

**Route:** `/signup`

**Purpose.** Self-service account creation.

**How to use it.** Provide a username and password. Creating an account also starts
a session — the server mints the token pair and sets the refresh cookie — so a
successful sign-up lands you in the app in one step.

**The first account becomes the admin.** On a tenant with no admin, the first
sign-up is granted the tenant admin role atomically with account creation.
Subsequent sign-ups are plain `user` identities with **zero entitlements** until an
admin grants them a role. An existing admin is never demoted or clobbered.

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

**Why F-keys and not Ctrl+1..7?** Because it was measured: `ctrl+3` arrives as
ESCAPE and `ctrl+8` as BACKSPACE (a control byte is `digit & 0x1f`, so 3 and 8
collide with escape and delete), `alt+<digit>` is claimed by the terminal emulator
before any program sees it, and `shift+<digit>` arrives as the shifted symbol.
F1–F7 is delivered cleanly, is claimed by nothing, and collides with nothing.

### The shell around every screen

- **Content pane** — the focused screen, split into a **list** of sources and a
  **detail** pane.
- **Chat dock** — the composer at the bottom. It is **focused at launch**, so you
  can type immediately.
- **Right rail** — the Ask conversations list, toggled with `ctrl+r` (open by
  default).
- **Left diff rail** — the file-diff sidebar, toggled with `d` / `shift+d` from
  content focus, or `/diff` from the composer.
- **`?`** — the help overlay, rendered **from the live key registry**, so it cannot
  drift from actual behaviour.

### Switching screens

- **F1–F7** switch areas; clicking a tab also switches and opens its menu.
- **Enter or space on the active tab** opens that area's **submenu** of
  sub-screens — up/down (or `j`/`k`) moves, enter selects, esc closes. The rows are
  click targets too.
- **`/`** opens the **command palette** above the composer, with the input still
  visible.

### The composer

- Type and press **Enter** to send.
- **`alt+enter`**, or a trailing `\` then Enter, inserts a newline.
- **Pasted text never sends until you press Enter.**
- **`ctrl+y`** stops an in-flight reply (the GUI's Stop button). The affordance row
  offers it while a reply streams.
- **Sending while a reply streams interjects** — it supersedes the turn rather than
  queueing behind it.
- **`ctrl+v`** attaches an image from the clipboard; **`ctrl+f`** attaches a file
  by path. Both land in the pending set for the next message and are reported in
  the strip above the composer.
- **`ctrl+g` / `esc`** toggle between content focus and the composer.

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
| `/attach` | Attach a file (not supported in the TUI — use `ctrl+v`/`ctrl+f`) |

**Two names that must not be confused.** `/project` (**singular**) sets the Ask
tab's **workspace scope**. `/projects` (**plural**) is the generated navigation
command that opens the Work tab's Projects pane.

**Navigation commands are generated from the screens themselves.** Every list pane
a screen actually has becomes a command (e.g. `/work-items`, `/runtime-images`,
`/executions`), so the command list cannot drift from what the screens offer. There
are no notice-only "use the web GUI" commands.

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
persist it), or `/theme`. The launch default is the **`teal`** palette, matching
the GUI's default. There are 43 themes across light and dark, including **true
transparency**, where the terminal shows through and the text adapts to your
terminal's own background.

### On Windows

Chords and mouse work in Windows Terminal. Legacy conhost may degrade mouse
handling.

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
| `x` | Delete (→ cancelled) |
| `/` then text | Search |

Every destructive chord is confirm-gated, and the list reconciles after a write.

**One rule the client enforces on you:** the Tree and Board are **display
groupings** and never renumber anything. Only `J`/`K` reorder.

### Runtime Images

Detail includes tag, status, base, version and built version, apt packages,
toolchains, environment, and any Dockerfile override, plus the last build log.

**Chords.** `n` create · `e` edit the spec (version-carried) · `b` **build**, with
logs streaming live into the pane · `x` delete (confirm-gated).

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

> **Coming soon…** — the same deliberate placeholder as the GUI (§10.2). The pane
> lists nothing and binds no chords, and it **fetches nothing**: issuing a list call
> behind a "coming soon" pane would be a request nothing can act on. Keeping the
> source registered is what keeps the section discoverable, so the nav entry does
> not silently disappear.

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
account through **Sign up** (§2).

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
  claims (`org`, `groups`, `tenant`) are deliberately **not** consulted for tenant
  selection.
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
| `orchicon serve` | Run the control plane with the embedded frontend (headless, migrations on boot) |
| `orchicon serve --detach` / `--stop` | Manage a background `serve` instance (PID file; logs in `.dev/logs/`) |
| `orchicon container` | Run the whole stack as PID 1 (the single-container image) |
| `orchicon install` | One-command setup: pull images, start the runtime daemon, launch the container, print connection info |
| `orchicon runtime-daemon` | Host process owning the Docker socket; spawns per-workflow runtime containers |
| `orchicon runtime-supervisor` | Runtime container PID 1 |
| `orchicon runtime-client` | Forwards dispatches into the runtime container |
| `orchicon mcp` | Start the MCP stdio server (exposes the Ask Orchicon tool registry) |
| `orchicon db` | Database maintenance: `backup`, `restore`, `list`, `prune` |
| `orchicon backfill-pr` | Idempotently backfill real PR URLs onto already-completed git-backed runs |
| `orchicon run-context <runID>` | Print a workflow run's `.orchicon` archive (per-step files plus run summary/status) for context-by-reference. Flags: `--grep <pattern>`, `--steps`/`--summary`, `--limit N` |
| `orchicon version` | Print the installed version |

**Scripts:**

| Script | Description |
|---|---|
| `scripts/container.sh` | Build / up / down / status / logs / ps / runtime-daemon / runtime-stop for the dev + prod container instances |
| `scripts/orchicon.sh` | Start / stop / restart / status / logs / rebuild the **host-resident** plane (dev or prod), services container included |

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
