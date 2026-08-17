# AGENTS.worker.md — Orchicon Worker Rules

Read on demand by Orchicon workers. Your role, task, acceptance criteria, and the ORCHICON WORKER SUMMARY contract are already in your system prompt. This file holds the cross-cutting rules every worker must follow; when in doubt, follow your system prompt.

## Rule 1 — Branch discipline

- Never commit to, push to, or PR into `main` or `develop`. `develop` is the integration branch; `main` is release-only and managed by the human.
- Run `git branch --show-current` at the very start of every task. Work on the branch this workflow created for the work item; create one off `develop` only if none exists.
- PRs must explicitly target `develop` (`gh pr create --base develop`) — `main` is the default branch and an unspecified base silently lands on the release branch.

## Token discipline

- Prefer compact tool output: `git status --short`, `git log --oneline -5`, `git branch --list`, single-line `gh` queries. When reading `.orchicon/<run>/` files, read `summary`, `facts_learned`, and `issues` in full, but only `grep` `touched_files` for paths relevant to your task.
- Batch tool calls — each call re-sends the whole conversation to the model, so fewer, larger calls are dramatically cheaper.
- Compact commands are still real commands: verify state with actual tool calls and never fabricate output. Batching combines commands, never results.

## Facts ledger

- Record established facts, root causes, environment gotchas, and decisions as `FACTS LEARNED:` lines in your final summary — one per line — so later steps inherit them instead of re-deriving them.
- Never re-verify a fact already recorded by an earlier step. If you believe one is wrong, append a correcting `FACTS LEARNED:` line rather than silently re-deriving it.

## Read the run's `.orchicon/` files

- `.orchicon/<run_id>/` holds what earlier steps produced: `facts_learned`, `status`, `summary`, `issues`, `touched_files`, `worker`, `attachments/`. Read the ones relevant to your step before starting — they are the authoritative feedback.

## Verify, don't assume

- Every claim about the repository, branch, PR, or merge state must come from a real command you ran. If a command fails, report the real error — never fabricate success.

## Environment baseline (established facts — do not re-verify)

- `/tmp` is noexec in the runtime containers: Go's default TMPDIR there makes test binaries fail with exec-format errors. Use an exec-able dir (e.g. `GOTMPDIR=$PWD/.gtmp`) for `go test` / `make ci`.
- `:orchicon-dev` runtime containers boot a sandbox plane at `http://localhost:8080` with Postgres on `localhost:5432` (user `orchicon`) and `ORCHICON_TELEMETRY=none`. Base/`:gui` images boot no plane.
- The runtime supervisor runs the daemon self-copy present at container start — a container started before a feature landed runs the pre-feature binary.
- Pre-existing gofmt drift and whole-repo semgrep findings exist at base. Only NEW findings introduced by your change matter.
- The repo is PUBLIC; do not attempt to re-privatize it.

## Summary contract

End your response with the literal line `ORCHICON WORKER SUMMARY:` followed by one word — `success` or `failure` — and a short paragraph. The first word routes the workflow. Keep the summary under ~500 tokens; it is re-embedded into every later step's prompt.