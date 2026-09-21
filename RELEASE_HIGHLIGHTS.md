# Release Highlights

> Curated, reader-facing highlights for the current release cycle.
>
> This file is the narrative preface for two surfaces:
>   - the GitHub Release body (prepended above the detailed UPDATES.md
>     rows by `scripts/gen-release-notes.sh`), and
>   - README.md's "Last Release Changes" section (the first paragraph
>     only, synced by the same script — keep it tight; the full story
>     lives on the GitHub release page).
>
> Update this file at the START of a release cut (before the develop→main
> merge PR): describe what shipped as whole features — what it is, why
> you'd use it — not commit-by-commit detail. The `## vX.Y.Z` heading
> tells the tooling which version these highlights describe; update it
> when you cut.

## v0.3.1

### New: orchicon.dev, rebuilt around the product instead of around the pitch
The landing page no longer describes the platform in the abstract — it shows it. Side-by-side tours of the web app and the terminal client, each with real screenshots rather than mockups, the six steps that take an idea to a merged pull request, the three Ask modes the platform actually enforces, scheduled market research and the Idea Cloud, and budgets as a constraint rather than a report. It is also self-contained: one HTML file with inline styles and the marks inline as SVG, so it loads fast and has no build step beyond copying the installers in.

### New: A real brand in the app — the mark, and a favicon that actually exists
The app's logo was a placeholder: a rounded square with a gradient and a generic icon, which reads as unfinished rather than as Orchicon. Its favicon was worse — it pointed at a file that has never existed on disk, so every page load requested a 404 and no browser ever showed an icon in a tab, bookmark, or history entry. Both are now the Orchicon mark, drawn from the same source the website uses. It follows the theme rather than being a fixed colour: the five leading segments inherit the surrounding text, so the mark renders as ink on a light theme and as light on a dark one, with the closing segment keeping the brand signal green.

### Also in this release

- **The terminal client's panes stop showing coloured whitespace.** A detail pane pads short lines through a viewport, which writes one spelling of a "reset" escape, while the repair that re-asserts the pane's background only knew the other — so the padding was left unpainted and rendered in the *terminal's* colour instead of the pane's. Every markdown line shorter than the pane showed it: a heading, a bullet's last wrap, a paragraph's tail. It was never specific to one theme, which is why it looked wrong on all of them.

## v0.3.0

### New: OpenCode is optional — Orchicon runs on its own engine
Orchicon no longer requires an external runtime CLI: its own native engine runs sessions inside the control plane, and the OpenCode serve is started **only when something actually needs it**. Before this release, OpenCode was a hard prerequisite — install the CLI or nothing runs, and on a host without it the installer refused. Now the plane computes its own adapter demand set from the model refs you actually use, and a plane that needs no OpenCode never probes for the binary, never starts a serve, and never mounts it into a container. The model picker defaults to the native engine, container mounts are decided per run rather than per platform, and the installer no longer insists you install a runtime. **You can run Orchicon end to end today with no adapter CLI installed at all** — and if you already use OpenCode, nothing about your setup changes.

### New: A complete terminal client — the whole product in the TUI
`orch` is no longer a launcher alongside the GUI: it is a full client, with read *and* write parity across **all seven domains**. Ask Orchicon, Overview, Work, Execution, Automation, Enforcement and Control are all first-class from the terminal — creating, editing, publishing, approving, cancelling, retrying, bulk operations and live streams included. It has a real boxed multi-line composer with slash commands and a command palette, a slide-out diff sidebar, click-to-copy on your own messages, and **43 themes** across light and dark — including **true transparency**, where the terminal shows through the whole client and the text adapts to your terminal's own background so it stays readable.

### New: Work is workflow-first — every run is a workflow
Every run is now a workflow: standalone dispatch is retired, so a work item with no bound workflow cannot be scheduled, and the platform tells you when you create it rather than failing later at run time. A whole backlog bound to nothing used to be silently stranded. Binding a **workflow and a runtime image** is now a bulk operation across a selection, so unblocking a backlog is one gesture instead of a form per item, and the editor can no longer quietly clear a binding you did not touch.

### New: Ask Orchicon — enforced modes, real context management, and nothing lost
Its three modes are now enforced by the **platform**, not requested in prose: the tools a mode may not use are withheld from it and refused at the point of execution, so "Brainstorm will not write your files" is a refusal a model cannot talk its way past — and the boundary is adapter-agnostic, so a new runtime inherits it. Conversations manage their own context: compaction on demand, a proactive pressure gate, and recovery from overflow instead of a permanently wedged conversation. Every message and action is persisted **live**, so a timeout, abort or dropped socket no longer loses a turn, and Ask runs on **any** adapter rather than only OpenCode.

### New: Runs execute in containers, and the plane heals itself
Worker executions run in an isolated container per workflow run, drawn from a warm pool so dispatch never cold-starts, and reset between runs so no state crosses a boundary. A native fast path removed a three-minute dispatch stall. Stale runtime daemons and leaked containers are reaped, containers a run does not need are no longer mounted, and liveness probes no longer kill healthy workers. Runs survive backend failures instead of wedging.

### New: Quick Work hands off end to end
The dispatch mode whose whole purpose is to hand work over now actually does it: it asks the model question, confirms git, and publishes the workflow and work item so a run fires — with the DevOps step opening and merging the pull request.

### Also in this release

- OpenCode is now **optional everywhere**: the installer no longer warns that a runtime CLI is missing, and the docs describe adapters as pluggable rather than required.
- **Scheduler and dispatch**: cancel and abort genuinely stop the model session; recovery survives DAG pass limits; PR-merge loops and orphaned branch references fixed; tool-wedge recovery no longer kills a live turn; a tool-hang is redirected instead of orphaning the worker.
- **Ask Orchicon sessions**: follow-ups resolve against the *execution's* adapter rather than the host; tool-call replay no longer 400s after a model switch; attachments deliver; the phantom "budget" workers reported in Ask is gone.
- **Terminal client**: a key that produced a send could be silently swallowed; clicking now places the caret on wrapped and scrolled text; a stale shell reference stopped every notice (and half of every theme switch) from landing; lazy screen loads, scroll preservation and tab focus corrected.
- **Diff pipeline and telemetry**: the file-edit ledger no longer reports empty on live runs; diffs are server-computed; per-event invalidations are coalesced; the outbox is throttled with retention so a chatty run cannot flood the database.
- **Runtime images**: build from the terminal with live logs, and set them across a selection in bulk.

## v0.2.0

### New: Autonomous research & the Idea Cloud — Orchicon finds the work, you approve it

Orchicon can now run its own product research and propose what to build next. Put it on a schedule and a three-worker crew goes to work on its own: the **Planner** surveys the market live (agent platforms, harnesses, adjacent categories), the **Analyst** verifies each candidate against external evidence, and the **Synthesizer** distills it all into feature proposals. Proposals don't go straight into your backlog — they land in the **Idea Cloud**, a triage board separate from real work. Each idea carries its evidence and the run that produced it; promote it to turn it into real work items, or dismiss it and it's remembered as rejected history so it never comes back. Workers can even check the cloud themselves before spawning, so duplicates can't pile up. Instead of maintaining a backlog by hand, Orchicon surfaces genuinely new, feature-sized opportunities from the outside world — and you stay the decision-maker.

### New: Cost discipline, built in — budgets that actually enforce

v0.2.0 treats token spend as a first-class constraint. Every execution now runs under configurable budget ceilings — tokens, dollars, wall-clock, tool calls — with sensible built-in defaults calibrated from real usage telemetry. When a session gets expensive, Orchicon trims its context and keeps going instead of resending an ever-growing history; a turn-count limit caps how often that can happen; and truly hard limits (wall clock, tool calls) stop a runaway worker outright. Cache re-sends no longer count as work, so a long task can't trip its token budget just by re-reading what it already knows. New custom tools back this up end-to-end: workers read, write, and search files in batches and consume bounded, token-friendly lists everywhere — the same discipline visible to you on every execution page (peak working set, cumulative spend, cache hit rate). Net effect: materially lower cost per run, with the guarantee that a stuck or chatty worker can't silently burn money.

### New: A sleeker, faster interface — on desktop and phone

The control plane UI has been redesigned for clarity and speed: a cleaner shell and navigation, category folders for organizing workers, workflows, and conversations, drag-and-drop that just works, and an expanded theme system — 20 hand-tuned themes across light and dark. The mobile experience has been reworked end-to-end: layouts, touch targets, and navigation all behave properly on a phone, so you can check runs, approve work, and triage ideas from anywhere. Execution pages now show honest numbers — real model context windows, working set vs. cumulative totals, live token and cost usage.

### Also in this release

- **Runtime reliability**: self-healing container pools (stale daemons and leaked containers eliminated), stale-binary detection, and runs that self-heal across backend failures instead of wedging.
- **Scheduler resilience**: cancel/abort actually stops the model session; recovery survives DAG pass limits; PR-merge loops and orphaned branch references fixed.
- **Automation pipeline hardening**: role-scoped plane access with deny-by-default security, loud failures instead of silent no-ops, and a dedicated Rejected view for the Idea Cloud.
- **Developer experience**: one-command full rebuild (`make rebuild-dev` / `rebuild-prod`), automatic version tagging on develop, BuildKit-cached container builds.

Plus bug fixes across the runtime, scheduler, frontend, and settings — the full itemized list is on the [GitHub release page](https://github.com/beardedparrott/Orchicon/releases).