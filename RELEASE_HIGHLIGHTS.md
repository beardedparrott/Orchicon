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

## v0.4.5

### New: The control plane cannot be taken down by one bad tool call
Ask Orchicon's `update_work_item` tool dereferenced a workflow id that a scheduled sequence parent holds as NULL **by construction** — a shape the API routes to the sequence chain, but which this path skipped. The panic then ran on a background goroutine that the HTTP server's per-connection recovery does not cover, so a single malformed tool call took down the **whole control plane** instead of failing that one call. Three of them are in this instance's own log. It is fixed — and so is the reason it was nearly impossible to find from the outside: the plane runs on the host, so the panic is written to the instance's own serve log (`$ORCHICON_SERVE_STATE_DIR/logs/orchicon.log`) and never appears in `docker logs`.

### New: A long conversation no longer makes the terminal unresponsive
Reading a 250-message conversation re-laid out **every** message on **every** frame of **every** pane — markdown, styling and box drawing for each — in order to draw the handful of rows that actually fit on screen. Measured, on the frame the client redraws while doing nothing else: 7.52 ms and 132,779 allocations, down to **266 µs and 652 allocations** (28× faster, 200× fewer allocations); 20× while streaming. Each message's rendering is now remembered until something it reads genuinely changes, and an accidental quadratic — counting an item's lines by scanning to the end of the accumulated transcript, once per item — went with it.

### New: A timeout measures silence, not age — and a question says what it is waiting for
Two separate limits measured how **long** a turn had been running, so the more honest work a turn was doing, the more likely it was to be killed at thirty minutes and the failure blamed on the model — naming a model that was perfectly fine. Both are inactivity bounds now: a turn that is still working is never killed for its age, and a turn that has genuinely gone quiet still ends, with the same honest message. A turn parked on a question of **yours** is now reported as exactly that, and the question is recorded as *unanswered* rather than as a refusal — so coming back and answering it resumes the conversation, instead of the question having to be asked again.

### New: A compacted conversation says so, and keeps what the work depends on
A long conversation is periodically summarized so it fits the model's window, and until now that happened **silently**: the only record was a line in the server log, where one conversation collapsed 2,343 messages into a single summary with nothing in the transcript to explain why the assistant no longer remembered what had been said. The collapse is now recorded in the conversation itself, in both clients. Two things that made it lossier than it looked are addressed too. The identifiers the work depends on — entity ids, file paths, commit names, the tools used — are extracted **before** the collapse and carried through verbatim, because the part of the transcript they lived in (tool arguments and tool output) is dropped before the summarizer ever sees it. And a history too large to save is compressed rather than silently not saved at all, which is what used to leave a long conversation reverting to a stale copy of itself after a restart.

### Also in this release

- **The terminal client's ask card offers "Other", the way the browser's does.** The recorded "Orchicon asks" card had no free-text row at all — only a footer pointing at the composer — and clicking the row sent the literal word `Other` as the answer. The row is on the card now, and what you type is sent as your next message.
- **A path that would destroy the scope can never be approved.** On top of the deny list and the never-allow class: a request whose target is a directory that contains the project you are working in is refused outright, by nobody's approval.
- **The Windows builds compile, so a release can actually be published.** The 0.4.0 cut could not produce its release assets; the build matrix is green across every platform it ships to.
- **The build no longer depends on your shell profile.** The makefile resolves its own copy of the schema tooling instead of trusting whatever `PATH` happens to contain at the moment you build.
- **Docs CI validates Mermaid diagrams**, so a diagram no other check can see cannot silently break in the published documentation.

## v0.4.0

### New: Ask Orchicon can do the work — and asks before it does
Ask Orchicon was a conversation you could read your project *with*; it now runs the same file and shell suite the workers use, against your real filesystem, scoped to the conversation's project. Nothing that writes or executes happens without your say-so: each one opens a card in whichever client you are in, naming the tool and the exact target. A **session grant** covers a directory and everything beneath it for that conversation, and the row says so — naming the directory it would cover rather than leaving you to guess its reach. A persistent **deny list** (your SSH keys, cloud credentials, `gh` config, `.netrc`, Docker config) is absolute, and the destructive class — `sudo`, `dd`, `mkfs*`, partition and LVM tooling — can never be approved by anyone, including you. Reads never ask. The prompt describes that boundary as it really is, including what it does not cover.

### New: FULLSEND — stop the prompts deliberately, rather than by accident
A gate that cannot be opened on purpose gets bypassed by accident: mid-task, approving card after card, you stop reading them. FULLSEND is the honest version of that — one explicit, revocable mode per conversation, shown as a badge in the terminal composer and a dropdown in the browser. It waives the *prompt* and nothing else: an entry on your deny list still refuses, and the never-allow class is still unreachable. It lives in memory, so a fresh plane starts with it off and a bypass cannot outlive the session you enabled it in; it is recorded in the audit trail; it can be toggled mid-turn; and turning it on approves a permission card already on screen rather than leaving the turn waiting on it.

### New: A question pauses the turn instead of talking to itself
Asking a clarifying question used to be record-and-continue — the model wrote the question down, kept going, and your answer arrived as an unrelated message. The call **blocks** now: the question appears as a card, the turn waits exactly where it was, and what you answer becomes the tool's result, so the model resumes holding your words rather than guessing what they referred to.

### New: The plane runs on your host, with the services containerized
Host residency is the default shape: the control plane runs as a host process while Postgres, NATS and the Grafana telemetry stack stay in one container reached over loopback. It is the same install and the same binary — what changes is that the plane's runtime, file access and process tree are the host's rather than a container's. The rollback is one word, and each instance (`dev`, `prod`) chooses its shape independently, so one can migrate while the other does not.

### New: Orchicon will not destroy the directory it is working in

A command that would delete the project it is running in — or the directory holding it — is now **refused outright, and no approval can override it**. `rm -rf /home` from a project, `rm -rf ~`, `rm -rf /`, and a `rm -rf` of anything that *contains* the project (or a directory you granted) are all refused, on top of your deny list rather than instead of it.

THIS IS A DELIBERATE BEHAVIOUR CHANGE, and it is the one thing in this release that can refuse a
command you did not explicitly forbid. It exists because the alternative was worse: with FULLSEND on,
the sandbox's usual checks are waived *by design* — that is what the mode is for — and an ancestor of
the project was covered by none of them, so the gate that stops you approving card after card also
stood aside for the one command that takes everything with it.

Working *on* the project is untouched: deleting a build directory, clearing `dist`, a recursive
`chmod` on the project root all still work, because acting on the thing you opened is ordinary work.
What is refused is destroying the thing that *holds* it. Pressing FULLSEND does not lift this, and
neither does a session grant — see *Enforcement, not just prompting* in the documentation for the
rule and the two lists it uses.

### Also in this release

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