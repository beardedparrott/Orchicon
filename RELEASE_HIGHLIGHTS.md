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