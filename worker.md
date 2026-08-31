# worker.md — Orchicon Worker Rules

This file is injected into every Orchicon worker session. Your role, task, acceptance criteria, and the ORCHICON WORKER SUMMARY contract are already in your system prompt. This file holds the cross-cutting rules every worker must follow; when in doubt, follow your system prompt.

**You are an Orchicon worker — do NOT read `developer.md`.** That file is for the human developer and Ask Orchicon, not for worker sessions. Follow only this file and your system prompt; never load `developer.md` into your context.

## Rule 0 — Work within your provisioned worktree

- **The working directory IS your worktree.** Git-backed runs are dispatched with the working directory set to an isolated worktree at `<project_dir>/.orchicon-worktrees/<runID>/`. All file operations, builds, and tests must happen inside this worktree.
- Do all your work inside the worktree; never write to `.orchicon/` or any other control-plane directory.
- **GOTMPDIR and all scratch directories must live inside the worktree.** Use e.g. `GOTMPDIR=$PWD/.gotmp` for `go test` / `make ci`. The runtime's `/tmp` is a private tmpfs (exec-capable, so Go test binaries run fine — but it is wiped at run end), while scratch must stay inside the worktree — never create `.gotmp/`, `.go-tmp/`, `.qa-gotmp/`, or `.gtmp/` under `.orchicon/` or anywhere else outside your worktree.
- Scratch inside the worktree is already ignored by the repo `.gitignore`, so it is never committed.
- **Runs without a provisioned worktree** (non-worktree dispatches, e.g. non-repo projects) work in place directly in `project_dir` — the scratch-inside-worktree rule applies to worktree runs; those runs keep the existing in-place guidance.

## Rule 1 — Branch discipline

- Never commit to, push to, or PR into `main` or `develop`. `develop` is the integration branch; `main` is release-only and managed by the human.
- Run `git branch --show-current` at the very start of every task. Work on the branch this workflow created for the work item; create one off `develop` only if none exists.
- PRs must explicitly target `develop` (`gh pr create --base develop`) — `main` is the default branch and an unspecified base silently lands on the release branch.

## Token discipline

- Prefer compact tool output: `git status --short`, `git log --oneline -5`, `git branch --list`, single-line `gh` queries. When reading `.orchicon/<run>/` files, read `summary`, `facts_learned`, and `issues` in full, but only `grep` `touched_files` for paths relevant to your task.
- **File access: use the composite batch tools.** When `batch_read` / `batch_grep` / `batch_write` are available, use them for every file read, search, and write — never repeated single `read`/`grep`/`write`/`edit` calls, and never re-read a file whose content is already in context.
- Batch tool calls — each call re-sends the whole conversation to the model, so fewer, larger calls are dramatically cheaper.
- Compact commands are still real commands: verify state with actual tool calls and never fabricate output. Batching combines commands, never results.

## Facts ledger

- Record established facts, root causes, environment gotchas, and decisions as `FACTS LEARNED:` lines in your final summary — one per line — so later steps inherit them instead of re-deriving them.
- Never re-verify a fact already recorded by an earlier step. If you believe one is wrong, append a correcting `FACTS LEARNED:` line rather than silently re-deriving it.
- On a recurring work item, your prompt may carry a **carry-forward block** (`## Carry-forward — facts learned by the previous fire`): facts the *previous fire* of this same item established. Treat them exactly like facts from an earlier step — do not re-verify; correct by appending.

## Read the run's `.orchicon/` files

- `.orchicon/<run_id>/` holds what earlier steps produced: `facts_learned`, `status`, `summary`, `issues`, `touched_files`, `worker`, `attachments/`. Read the ones relevant to your step before starting — they are the authoritative feedback.

## Verify, don't assume

- Every claim about the repository, branch, PR, or merge state must come from a real command you ran. If a command fails, report the real error — never fabricate success.

## Platform changes: keep Ask Orchicon in sync

- If you add/change/remove a first-class entity, RPC, or user-facing capability, update the Ask Orchicon tool registry to match (`internal/askorchicon/tools.go` + the tool files) so the Orchicon MCP/Ask Orchicon surface never drifts from what the platform actually does.

## Environment baseline (established facts — do not re-verify)

- The runtime container's `/tmp` is a private **exec-capable** tmpfs: Go's default TMPDIR there works, so `go test`/`make ci` run without relocation. It is wiped at run end — keep durable work in the project/worktree, and point `GOCACHE`/`GOTMPDIR` inside the worktree only to keep scratch tidy (never under `.orchicon/`).
- `:orchicon-dev` runtime containers boot a sandbox plane at `http://localhost:8080` with Postgres on `localhost:5432` (user `orchicon`) and `ORCHICON_TELEMETRY=none`. Base/`:gui` images boot no plane.
- The runtime supervisor runs the daemon self-copy present at container start — a container started before a feature landed runs the pre-feature binary.
- Pre-existing gofmt drift and whole-repo semgrep findings exist at base. Only NEW findings introduced by your change matter. Verify against the base (`git diff origin/develop`) once; do not fix unrelated files. Scope semgrep to the files you touched:
  ```bash
  semgrep scan --config .orchicon/semgrep_orchicon.yml --error $(git diff --name-only origin/develop...HEAD | grep -E '\.go$')
  ```
- The repo is PUBLIC; do not attempt to re-privatize it.

## Cleanup before reporting

Before writing your `ORCHICON WORKER SUMMARY:` line, clean up the temporary files you created during execution:

- Remove the GOTMPDIR and any other scratch directories you created inside your worktree (`.gotmp/`, `.go-tmp/`, `.qa-gotmp/`, `.gtmp/`, and any equivalent you used).
- The worktree itself is pruned by the platform after the run, but you must clean up your own mess so `.orchicon/` and the shared control-plane directory never accumulate scratch.
- Cleanup is a mandatory step — do not report `ORCHICON WORKER SUMMARY` until your scratch is removed.

## Commit-on-finish contract (worktree runs)

For **git-backed runs** (your worktree at `<project_dir>/.orchicon-worktrees/<runID>/` on branch `<branch>` — `worktree_status=ready`):

- Before writing `ORCHICON WORKER SUMMARY:`, **commit ALL changes** to the feature branch and **push to origin**.
- Verify: `git status --porcelain` must be clean (modulo gitignored scratch: `.gotmp/`, `.go-tmp/`, `.qa-gotmp/`, `.gtmp/`, `.gocache/`, `.cache-build/`, `architecture-notes/`, `design-notes/`, `.orchicon/`, `.orchicon-worktrees/`).
- Minimal recipe: `git status --porcelain && git diff --stat && git log --oneline -3`; `git add <files>`; `git commit -m "feat: <summary>"`; `git push origin <branch>`.
- **WHY:** downstream steps (QA Engineer, PR Reviewer, Code Approver, DevOps Engineer) run in **pristine sibling worktrees** created from the committed branch tip — **only committed + pushed work is visible to them**. Uncommitted working-tree changes are invisible and cause loops (observed: run `01M11YDJ03054YZ4THACMD7HXT` — 4 fixes implemented but 0 commits; QA blocked, PR Reviewer hacked `git -C <parent> diff`; extra iteration to `d25632c`).

For **in-place runs** (`worktree_status != ready`, no branch — you work directly in `project_dir`): do NOT create branches, commit, or push — work in place. The `GitGuidanceBlock` in `internal/db/prompt.go` already selects this path.

Do not report `ORCHICON WORKER SUMMARY: success` until the worktree is clean and pushed (git-backed) or files are persisted in place (non-worktree).

## Orchicon tool channels

You may see two Orchicon MCP tool families — they are deliberately separate:

- **`orchicon_plane_*` (real instance)** — available on **every** runtime image (base, `:gui`, web-research, `:orchicon-dev`) whenever your worker's **role** grants plane access. The image is irrelevant — the gate is your role's entitlements, never the runtime image. Operates on the REAL instance your work item was created on, through the plane API. Use `orchicon_plane_list_idea_items` to read the Idea Cloud (the dedupe gate) and `orchicon_plane_create_idea_item` to spawn idea-state work items — IDEA landing is forced by that tool (provenance from the run's trusted context, never call arguments); a refused spawn or a response without `idea_state: true` is a LOUD platform error to record, never a success.
- **`orchicon_*` (sandbox)** — available only on `:orchicon-dev` images. Operates on the disposable in-container sandbox plane (its own Postgres, `http://localhost:8080`). Use for DB/migration testing and throwaway records.

If your worker has a role but you see **no** `orchicon_plane_*` tools, that is a **platform bug** (the per-run credential mint failed) — record it as a `FACTS LEARNED:` line and fall back to shipping manifests for the UI; do **NOT** conclude that real-instance access is dev-runtime-only.

Hard rules: **never** use sandbox tools to inspect real work items; **never** use plane tools for throwaway records or migration tests.

## Summary contract

End your response with the literal line `ORCHICON WORKER SUMMARY:` followed by one word — `success` or `failure` — and a short paragraph. The first word routes the workflow. Keep the summary under ~500 tokens; it is re-embedded into every later step's prompt.