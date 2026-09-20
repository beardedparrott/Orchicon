# ADR-0011: Guarded Compaction & Agent Memory (SQLite FTS5)

## Status

Accepted (2026-09-02). Full design: `architecture-notes/context-management-guarded-compaction-agent-memory-sqlite-fts5.md`.

## Context

The native adapter (`internal/orchicon`, ADR-0007/0009) has no compaction today: transcript JSONL grows unbounded, long executions risk overflowing the selected model's real context window, and every re-send re-bills a growing prefix. Tool results dominate history and are mostly dead weight after their turn. Separately there is no durable cross-session channel: ADR-0009's `orchicon_memory_note` is in-session only and dies with the execution, so knowledge gained in one execution cannot inform the next in the same project.

Operator directives (fix-branch QA, Sept 2026) bind this work: NO synthesized data anywhere; compaction triggers read LIVE hints only (probe-sourced or catalog-ENRICHED context windows; Ollama `/api/show` true metadata incl. `num_ctx`); when a hint is absent, WARN and never guess; budget-compact triggers read real stream usage from usage records (no estimated/interpolated counts); reuse the existing shared pipelines (budget-compact, OTel) — no parallel implementations.

## Decision

### 1. Guarded compaction (native, in-process; not a model summarize)

New `internal/orchicon/compaction.go` implements a deterministic history compaction: recent turns verbatim (default 6); goal (first user message) pinned; ACs live in the static prefix and are never in evictable history; middle-only eviction of OLD TOOL-RESULT messages (`RoleTool`), originals disk-offloaded to `<project_dir>/.orchicon/offload/<execution_id>.jsonl`; a digest-marker user message injected at the eviction boundary (per-line tool name + offload path/seq) so the model knows what left and can re-read an original on demand. At-most-once per threshold band, min-turn floor, per-execution cap, re-arm only after forward progress.

Compaction fires on exactly TWO trigger classes:
- **(a) True context-window pressure** — resolved per session by a new `ContextWindowSource` (`internal/orchicon/contextwindow.go`) reading LIVE hints only, in order: Ollama `/api/show` (+ `/api/ps` effective when loaded) → catalog-ENRICHED `ModelInfo.Context` via `catalog.GetModel` → manual entries. When NO hint resolves: the sourcing WARN stands and compaction is NOT armed on the window trigger (never guesses). Armed when live accumulated `InputTokens + CacheReadTokens` crosses `pressure_frac` (default 0.8) of the true window.
- **(b) The budget gate** — the merged budget JSON (`manifest.Budgets`) evaluated via the shared facade (below) with LIVE usage only.

Cache-first ordering (ADR-0009) preserved: block 0 (static prefix) is never rewritten; compaction and the digest mutate only post-breakpoint content (history / mutable-zone block 1), asserted byte-stable by test.

### 2. Agent memory on SQLite FTS5

New leaf package `internal/agentmemory` (pure-Go `modernc.org/sqlite`, no CGO) at `<project_dir>/.orchicon/memory.db`, one FTS5 virtual table (`memory_entries`) with `title`/`body`/`tags` indexed, metadata columns (`tenant_id`, `project_dir`, `execution_id`, `worker_id`, timestamps) `UNINDEXED`, `tokenize='porter unicode61'`. Ranked search via `bm25()`, snippet via `highlight()`; writes are delete+re-insert on update (smoke-validated). Search input is pre-quoted as a single phrase to neutralize FTS5 operators.

SQLite-vs-Postgres: memory is per-PROJECT durable scratch whose lifecycle belongs to the project dir (alongside transcripts/offload); Postgres is the control plane's operational store — worker-runtime access to it would open a new cross-boundary secrets/network channel for every execution and pollute tenant RLS isolation with project-scoped agent data. FTS5 ships ranked full-text search in-process; a local file cannot silently "look alive" through a remote fallback (ADR-0010 spirit).

Four session-scoped native tools (`memory_write`, `memory_search`, `memory_read`, `memory_list`), name-gated like the existing `orchicon_memory_note` (which remains for ephemeral notes). Isolation is SQL filters (`tenant_id` + `project_dir` on every query) with writer tags — NOT per-execution DBs; the DB path derives from `manifest.ProjectDir` (survives per-run worktree pruning) so cross-session reads work after per-execution isolation. Store injected via `SessionConfig.MemoryStore`; nil → tools return a clear unavailable error, never a silent no-op.

Session-start memory digest: a capped (5 entries, ≤1 KiB) titles+tags list rendered by `renderMutableZone()` INSIDE the existing Cache:false block 1 — after the cache breakpoint — so block 0 stays byte-stable.

### 3. Settings (BudgetLadder pattern) + shared-pipeline reuse

Tenant `tenant_settings` gains typed columns via one migration (`context_compaction_enabled`, `context_compaction_pressure_frac`, `context_recent_turns`, `memory_enabled`, `memory_digest_entries`) carried through the existing `BudgetJSON`/`mergeBudgets` transport, so per-worker override JSONB (`budget_overrides`) layers per-worker opt-out over tenant defaults with no new dispatch plumbing.

A new `internal/budget` facade is extracted from `internal/opencode/compact.go` (mechanical move + export; opencode keeps compiling thin aliases, tests unchanged) so the native engine evaluates the SAME budget ladder the opencode adapter uses — one budget-compact pipeline. OTel metrics reuse `telemetry.Meter()` patterns — one pipeline. No stub/mock/estimated paths anywhere (grep-class audit in test plan).

## Consequences

- Long native executions stay inside the model's real window without a lossy model summarize; budget-breach compacts fire exactly once per band and the session continues.
- Durable project memory with ranked search; later executions in the same project inherit earlier findings; per-execution isolation is cheap (SQL filters + writer tags), not a fresh DB.
- Cache breakpoint contract intact: block-0 byte stability asserted by test.
- Cost: `modernc.org/sqlite` (+ `modernc.org/libc`) dependency tree; one migration; budget-facade extraction touches the opencode adapter (compile + existing tests guard it). Memory tools are worker-native, not platform RPCs — no Ask Orchicon registry change (revisit if they become first-class).
