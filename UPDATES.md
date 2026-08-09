# UPDATES.md

> Track record of what has been shipped, phase-by-phase.
> Read this before starting any work to understand the current state.
>
> **Lifecycle:** one row per PR, appended to the top (newest first). Row
> numbers are MONOTONIC — never renumber, the release-notes boundary derives
> from the previous release's copy. At release time the GitHub Release body
> is generated from these rows (scripts/gen-release-notes.sh). After a
> release, run `scripts/gen-release-notes.sh --trim` on the next develop
> merge to drop released rows and keep only the current cycle.

| # | Type | Phase | Summary |
|---|---|---|---|
| 115 | Bug fix | Sequence engine | "Has children" is now the sequence determinant: a parent with children routes to the sequence engine (validating its subtree) even with a stale workflow binding — no more silent individualized runs; the stale binding is cleared on fire. |
| 114 | Bug fix | UI | Fixed drag-to-reorder: the post-drag click no longer navigates into the item (tree + board), reorder works in the All-projects view (project_id derived from the items), and sequence children now show #N chain badges in both views. |
| 113 | Bug fix | Runtime lifecycle | Fixed leaked runtime containers from runs stuck "running" forever: fail empty-step-DAG / missing-version runs at start (container reaped, sequence chain halts), fail task steps whose work item or execution row was deleted instead of waiting forever, and reject empty-DAG child workflows at sequence schedule time. |
| 112 | Bug fix | Session transport | Hardened the session transport against truncated final turns: async fire-and-forget follow-ups (no dropped browser connections, reply collected on a request-independent context), completion-signal probe at session settle, truncated-final-turn detection, and a loop-decision re-ask budget that counts real re-asks. |
| 111 | Bug fix | Sequence engine | Fixed the scheduled-fire scan crash (SQL NULL into *string aborted every pass) and the mid-run reorder that armed a pending sibling ahead of an in-flight child. |
| 110 | Bug fix | Sequence engine | Added the reconcileParent sequence-parent guard, the Ask Orchicon reorder tool, and sequence UI polish (chips, badges, filter-aware tree reorder). |
| 109 | Bug fix | Sequence engine | Fixed tree drag-to-reorder to derive order from the true sort_order sibling rank; default display sort is now chain order. |
| 108 | Feature | Sequence engine | Added sequential multi-workflow runs: sort_order ordering, the sequence engine (fire/advance/halt/retry/park), schedule-time validation, and schedules/board/tree UI. |
