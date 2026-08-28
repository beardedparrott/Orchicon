# Architecture Notes — Bug: workflow runs leave the project (develop) checkout dirty — blocks pulls

**Work item:** `bug-workflow-runs-leave-the-project-develop-checkout-dirty-blocks-pulls-ew994m932qnkx114`  
**Branch:** `bug-workflow-runs-leave-the-project-develop-checkout-dirty-blocks-pulls-ew994m932qnkx114`  
**Author (Step 1 — Principal Software Architect):** muse-spark-1.2  
**Date:** 2026-08-28

---

## 1. Context

Workflow runs for Orchicon itself (develop checkout at `project_dir`) repeatedly left the main checkout dirty, causing `git pull` to fail with `Your local changes to the following files would be overwritten by merge`. Observed 2026-08-28 across runs for Automations tasks #434/#435/#436:

- Runs *did* use worktrees (`worker_executions.worktree_status='ready'`, paths under `.orchicon-worktrees/<runID>`, per-run branches). Yet after squash-merge of PRs #434–#436, the develop checkout contained a partial older snapshot (e.g. `internal/askorchicon/tool_runtime_images.go` matched merged #436 byte-for-byte while `api.go`/`tools.go` were pre-#435, missing secrets wiring), a stray untracked `internal/askorchicon/_batch_test2.go`, and a deleted `frontend/dist/.gitkeep`. Conflict set: `api.go`, `askorchicon/service.go`, `tool_runtime_images.go`, `tools.go`, `db/worker.go`, `worker/service.go`, `work_item.proto`, `work_item_service.proto`, `worker_service.proto`.
- Run-level `worktree_status` ended `pruned` with empty `worktree_path`/`worktree_branch`, while `worker_executions` still held `ready` paths. `.orchicon-worktrees/` contained stale dirs with no `.git` pointer — prune left directories behind.
- Two `WIP on develop` stashes existed (same pollution), dropped after manual checkout restore.
- Branch protection: `develop` and `main` require `linear_history=true` + 1 review. Three merge strategies allowed → PRs are squash-merged in practice. `delete_branch_on_merge=true` fixed 2026-08-28 (auto-delete head branches). Prior to fix, 13 worker PR branches lingered (including a `*-step-qa-*` sub-branch that should never have been pushed).

### Likely mechanism (to confirm in source)

- **In-place fallback (`worktree_status='skipped'`):** `internal/scheduler/worktree_reconciler.go` marks a run `skipped` when project has no `project_dir` or not a git work tree; `internal/scheduler/workflow_reconciler.go: ~969-975, ~2899` notes in-place runs are never told to work on a branch. `internal/db/prompt.go:GitGuidanceBlock` confirms: skipped runs execute directly in `project_dir` (the live checkout) with no branch isolation. Any file edits remain as uncommitted changes on `develop`.
- **Commit-on-finish contract (PR #426):** `worker.md` commit-on-finish assumes `worktree_status=ready` (branch + worktree). For `skipped` runs the worker is told *not* to commit/push/PR — changes stay on the main checkout.
- **Prune incompleteness:** `WorktreeReconciler.scan` prunes `ready` run/step-run worktrees and sweeps orphan branches, but physical directory removal vs. DB state (`worktree_path` cleared while execution rows still `ready`) can diverge if `git worktree remove` succeeds and `rm -rf` of the stale dir is not verified, or if a concurrent provision recreates a dir.
- **Squash-merge branch leak:** `git branch --merged` cannot detect squash-merged branches (tip not ancestor). Nothing cleaned them; step-level sub-branches (`*step-qa*`) leaked to origin.

### Non-goals

- Changing the `orchicon serve` runtime identity (observed: running binary predated #436 — MCP tools need rebuild/restart; out of scope for this bug but noted as operability gap).
- Removing worktrees entirely or making the repo non-git.

---

## 2. Decision — ADR-2026-08-28: Harden worktree isolation and close the in-place dirty-checkout path

We adopt **Option A (preferred worktree) + hardened fallback + complete prune + GH-native branch hygiene**. The core principle: *a workflow run must never mutate the main checkout's working tree*.

### 2.1 ADR — Worktree is mandatory for git-backed projects; in-place fallback is disabled for git repos

**Context:** Today `skipped` is reachable for a git-repo project when detection is stale/missing or provisioning races. That is the entire dirty-checkout surface.

**Decision:**
- For any project where `resolveGitBacked` reports `gitBacked=true`, the only terminal worktree states are `ready`, `failed`, or `pruned`. `skipped` is *not* a valid steady state — it is reserved for **non-git projects** (no `project_dir` or not a work tree) and one-shot runs with no bound project.
- If provisioning fails for a git-backed run, mark `worktree_status='failed'` with the git error (do not silently degrade to `skipped`). `WorkflowReconciler` treats `failed` as a terminal provisioning error for that run; retry requires explicit human reset or a new run (fail-closed, not fail-open to dirty).
- Keep the existing `holdInPlaceDispatch` guard (D3) as defense-in-depth: a `pending` git-backed run that has not yet reached `ready` is held pre-dispatch so neither inline `DispatchTask` nor `TaskReconciler` scan creates an execution in the shared `project_dir`.

**Alternatives considered:**
- *Unbounded skipped fallback (status quo):* maximizes availability but violates isolation and blocks pulls — rejected.
- *Allow skipped with snapshot/restore (stash):* feasible but adds complexity and still risks merge conflicts if reset is incomplete; kept only as emergency path for non-git projects.

**Consequences:**
- Positive: eliminates the observed pollution vector entirely for the Orchicon repo itself.
- Negative: transient `git worktree add` failures now fail the run instead of running in place; mitigated by idempotent retry and bounded scan concurrency.
- Operational: `gitDetectionTTL` (24h) remains; stale detection still holds dispatch via `holdInPlaceDispatch` until re-detection.

### 2.2 ADR — Hard guard and restore for any skipped run that still executes (non-git / emergency path)

Even though git-backed runs will not be `skipped`, we harden the fallback so a future misclassification or a non-git project cannot pollute a git checkout.

**Decision:**
- **Pre-dispatch clean check:** before admitting a `skipped` run against a git work tree, run `git status --porcelain` in `project_dir`. If dirty (modified/deleted/untracked that is not ignored), refuse admission: mark `worktree_status='failed'` with `checkout dirty — refusing in-place dispatch; clean the working tree and retry`. Never dispatch in-place against a dirty checkout.
- **Post-run restore (configurable, default on for git checkouts):** after a `skipped` run reaches terminal, the `WorktreeReconciler` (or a new `CheckoutSanitizer` step in `WorkflowReconciler` terminalization) runs `git reset --hard HEAD && git clean -fd` in `project_dir` (not `-fdx` — preserve ignored build artifacts; `frontend/dist/.gitkeep` is tracked and must survive). On failure, log and surface as `worktree_status='failed'` with instruction to clean manually; never leave dirty state silently. Make the restore behavior flag-gated (`ORCHICON_IN_PLACE_RESTORE=1` default for git repos).
- **Serialization:** non-repo in-place runs remain gated by `DispatchLimiter` atomic admission (D3) so at most one `skipped` run mutates the shared dir at a time.

**Trade-offs:**
- `git clean -fd` removes untracked non-ignored files (e.g. `_batch_test2.go`) — exactly the stray-file class observed. `-fdx` would also remove ignored `.gotmp/` etc. but those are gitignored and live inside worktrees for git-backed runs; for in-place non-git projects there is no ignored pollution contract — `-fd` is safer default.
- `reset --hard` is destructive to uncommitted work; but an in-place dispatched worker's edits on `develop` *are* the dirty state we must discard — and the pre-dispatch dirty check already ensures no human work is clobbered.

### 2.3 ADR — Prune cleanup must be complete and consistent

**Context:** Run row cleared `worktree_path`/`branch` to empty while executions still `ready` with paths; `.orchicon-worktrees/<id>` dirs remained with no `.git` pointer — not valid worktrees but not removed. Branches leaked as local refs even after merge.

**Decision:**
- **Atomic prune contract:** `pruneOne` / `pruneStepRunOne` must verify physical removal before DB state transition. Order: `git worktree remove --force <path>` → `rm -rf <path>` if still exists → verify `!dirOccupied(path)` and `!branchExists` (if branch was to be deleted) → then `worktree_status='pruned'` + clear `worktree_path`/`branch` in one optimistic-concurrency update. If verification fails, keep `ready` and retry next scan (do not clear DB while dirt remains).
- **Orphan directory sweep:** add a scan pass that lists `.orchicon-worktrees/*` entries with no corresponding `workflow_runs`/`workflow_step_runs` row in `ready`/`pruned` that references them, and no valid git worktree registration (`git worktree list` + `isOrchiconWorktreeArtifact` / `isOrchiconContainer` checks). Remove orphan dirs (respect `stagingPrefix` exclusion). This reclaims the observed stale dirs.
- **DB consistency:** run-level `worktree_path`/`branch` and execution-level paths must be updated together where possible; at minimum, never clear the run row while any child execution still claims `ready` with a path under that run's namespace — prune children first, then parent (already ordered in `scan`).

**Alternatives:** background `git worktree prune` + `git clean` cron — less precise; explicit sweep is deterministic.

### 2.4 ADR — Branch hygiene: GH-native deletion, never `git branch --merged`

**Decision:**
- Rely on GitHub `delete_branch_on_merge=true` (already enabled) as primary.
- Worker merge step (DevOps Engineer) must also explicitly delete the head branch via `gh api repos/{owner}/{repo}/git/refs/heads/{branch} -X DELETE` or `gh api` equivalent after a successful merge, for non-GH / manual-merge paths — idempotent, no-op if already deleted.
- Never use `git branch --merged develop` to detect merged worker branches; squash merges make tips unreachable — use `gh pr list --state merged --head <branch>` or `gh pr view <branch> --json state` (as `deterministicPRForBranch` already does) to prove merge.
- **Step-level sub-branches (`*-step-*`, e.g. `*-step-qa-*`) must never be pushed to origin.** Enforce by: (a) push allowlist in `WorktreeReconciler.provisionWorktree` / worker `git push` wrapper — only push `branchName` (run branch) and never `branchWorktreeName` step branches; (b) pre-push hook / CI check that rejects refs matching `*-step-*`.

### 2.5 ADR — Regression test

**Decision:** Add a regression test that runs a minimal workflow against the Orchicon repo itself (git-backed project) and asserts the develop checkout is clean after the run.

- Test shape: `internal/scheduler/worktree_reconciler_test.go` (or new `regression_dirty_checkout_test.go`) integration test: create a workflow run bound to the Orchicon project, let `WorktreeReconciler` provision, simulate a terminal run, run prune, then assert `git status --porcelain` in `projectDir` is empty and `.orchicon-worktrees/<runID>` does not exist and `branchExists(branch)==false` after completion. Include a skipped-path subtest: mark a run `skipped`, write a stray untracked file + modify a tracked file in `projectDir`, then assert post-restore `git status --porcelain` is empty and stray file removed.
- Gate in CI: `make ci` must run this test against a temp git repo (not the real checkout) to avoid flakiness.

---

## 3. Consequences

### Positive
- `git pull`/`merge` on the main checkout never fails with `local changes would be overwritten` after a workflow run — isolation is architectural, not advisory.
- No stray untracked files (`_batch_test2.go`) or deleted tracked files (`.gitkeep`) left behind.
- No stale `.orchicon-worktrees/<id>` dirs or leaked local branches after squash merges.

### Negative / Costs
- Git-backed runs that transiently fail to provision now fail the run (instead of silently running dirty). Requires operator to retry or fix `git worktree add` (disk, permissions).
- Post-restore `reset --hard` + `clean -fd` is destructive if a human left uncommitted work in `project_dir` — mitigated by pre-dispatch dirty check that refuses to run in that state.

### Risks and mitigations
- **Risk:** `git status --porcelain` pre-check races with a concurrent human edit. Mitigation: hold dispatch + advisory lock + limiter; worst case the run fails and human cleans.
- **Risk:** `git clean -fd` deletes a human's untracked file. Mitigation: only runs for `skipped` terminal restores, and only after refusing to dispatch when dirty — human dirt is never silently deleted.

---

## 4. Implementation delta — exact files and functions to change

### Primary

- **`internal/scheduler/worktree_reconciler.go`**
  - `reconcileOne`: remove the `!gitBacked → markSkipped` degradation path for git-backed projects ever reaching `skipped`; instead `markFailed` when `git worktree add` fails or when `dirOccupied` is not an orchicon artifact/container. Keep `skipped` only for `!found` / `!gitBacked` (non-git) and for `ProjectID==""`.
  - `resolveGitBacked` / caching: no change, but ensure the pre-dispatch hold sees fresh detection (already there).
  - `pruneOne` / `pruneStepRunOne`: harden to verify physical removal before DB clear; add `verifyPruned` helper; order child-then-parent prune.
  - `sweepOrphanBranches` / `sweepOrphanRun` / `sweepOrphanStepBranch`: keep, but add **orphan directory sweep** `sweepOrphanDirs(projectDir)` that enumerates `.orchicon-worktrees/*` and removes entries with no DB row and no valid worktree registration.
  - New helpers: `isDirtyWorkTree(ctx, projectDir) bool`, `restoreWorkTree(ctx, projectDir) error` (`reset --hard HEAD` + `clean -fd`), `sweepOrphanDirs`.

- **`internal/scheduler/workflow_reconciler.go`**
  - `holdInPlaceDispatch`: already holds `pending` git-backed runs until `worktree_status` leaves `pending`; ensure it also holds when `gitBacked==true` and `worktreeStatus==pending` regardless of limiter (currently does — keep).
  - Terminalization: after a `skipped` run completes/failed, invoke `restoreWorkTree` (or delegate to `WorktreeReconciler` via `worktreeNotifier` with a restore key) and do not mark run terminal until restore verifies clean.
  - `resolveRuntimeImage` / `failRunAtStart`: no change.

- **`internal/db/prompt.go`**
  - `GitGuidanceBlock`: already selects in-place guidance for `skipped`; ensure it never tells a `failed` git-backed run to work in place.

### Secondary

- **`internal/scheduler/task_reconciler.go` / `internal/scheduler/dispatch`**: ensure `DispatchTask` respects `holdInPlaceDispatch` and `worktree_status != ready` gate for git-backed runs (already gated via `WorkflowReconciler` hold + `WorktreeReconciler` admission).
- **Branch hygiene:**
  - `scripts/` or `internal/scheduler/worktree_reconciler.go:deleteBranch` / `deleteDeadBranch`: keep proof-deletion gates; ensure explicit `gh api` deletion after merge in DevOps worker (see `internal/db/prompt.go` + worker system prompt for DevOps role).
  - Worker git push wrapper (wherever `git push origin <branch>` is emitted — search `batch_grep` for `git push`): add allowlist so step-run branch `branchWorktreeName` is never pushed; add CI check `scripts/check_no_step_branch_push.sh`.
  - `.github/workflows/*` or `scripts/`: document `delete_branch_on_merge=true` is the source of truth; remove any `git branch --merged` cleanup logic.

- **Tests:**
  - `internal/scheduler/worktree_reconciler_test.go` (or new `regression_dirty_checkout_test.go`): add `TestWorktreePruneLeavesCheckoutClean`, `TestInPlaceRestoreCleansStrayFile`, `TestSkippedRefusedWhenDirty`, `TestOrphanDirSweep`.
  - `internal/scheduler/workflow_reconciler_test.go`: add `TestGitBackedRunNeverSkipped`.

- **Docs:**
  - `DOCUMENTATION.md` § Scheduler/Worktree: update to state worktree-mandatory for git projects, describe restore guard and branch hygiene.

---

## 5. Review checklist

- **Does the design scale? What breaks at 10x?**
  - Worktree per run is O(runs) dirs + branches. At 10x, `git worktree list` and branch sweep are bounded (`orphanBranchSweepLimit=32`, scan batch 16, `maxWorktreeCandidatePages=4`). Dir sweep is O(entries) but `.orchicon-worktrees` is namespaced by run ID, so enumeration is cheap. No shared mutable checkout at 10x because git-backed runs no longer serialize on `project_dir`.

- **Are we building the right thing? (problem fit)**
  - Yes: directly closes the observed dirty-checkout vector (in-place fallback + incomplete prune) rather than papering over with periodic `git checkout -- .`. The pre-dispatch dirty check converts silent pollution into an explicit failure.

- **Security, observability, operability considered?**
  - Security: no new secret handling; `reset --hard` is scoped to `project_dir` and gated on prior dirty check.
  - Observability: log `isDirtyWorkTree` refusals and `restoreWorkTree` outcomes at `Warn`; expose `worktree_status='failed'` reason to the run's `run_context` / execution output so the operator knows to clean.
  - Operability: running `orchicon serve` binary predating a merged feature remains a restart gap — note in runbook to `make container-rebuild instance=dev` after merges that touch the plane.

- **Trade-offs documented? Alternatives explored?**
  - Yes: §2.1–2.2 document mandatory-worktree vs. stash-restore; §2.3 atomic prune vs. cron; §2.4 GH-native vs. `branch --merged`.

- **Is the design consistent with existing architecture?**
  - Yes: preserves k8s-style idempotent reconcilers, optimistic concurrency, transactional outbox, per-kind advisory locks, and the `WorktreeReconciler` as the sole worktree lifecycle owner. `DispatchLimiter` D3 and `holdInPlaceDispatch` remain the serialization seams for the non-git path.

---

## 6. Handoff to Design Approver (Step 2) and Senior Engineer (Step 3)

- **Design Approver:** confirm the mandatory-worktree gate for git repos and the `reset --hard` + `clean -fd` restore scope (not `-fdx`). Flag if the team prefers fail-open fallback for availability.
- **Senior Engineer:** implement the delta in §4 verbatim; do not expand scope. Keep changes behind the existing `gitDetectionTTL` and `DispatchLimiter` seams. Add the regression test before marking the work done.

---

*Architecture notes live at `architecture-notes/bug-workflow-runs-leave-the-project-develop-checkout-dirty-blocks-pulls.md` (gitignored, retained as run artifact for downstream steps).*
