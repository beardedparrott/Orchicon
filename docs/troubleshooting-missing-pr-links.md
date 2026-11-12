# Troubleshooting: a work item says `succeeded` but has no PR / merge link

**Symptom.** A contiguous block of work items in the Work Items tree read `succeeded`, but the rows carry no
branch and no `PR merged` pill — while the items on either side of them do. It looks like a gap, and it is one.

**One-line answer.** `succeeded` is the **work item's own field**. A PR link is **provenance that lives on the
workflow run and the worker execution**. Delete either and the status survives while the evidence does not:
the item keeps `succeeded` and keeps a `workflow_run_id` pointing at a run that no longer exists.

---

## What was measured on this instance

On the deployment tenant, of **46 `succeeded` work items, 22 have no worker execution at all** — 14 tasks,
7 features, 1 epic. (Features and epics never execute; their children do. The 14 **tasks** are the anomaly.)

For every one of those tasks:

| Check | Result |
|---|---|
| `worker_executions` rows with `task_id = item.id` | **0** |
| `work_items.workflow_run_id` | **set** (non-empty, a real ULID) |
| `EXISTS (SELECT 1 FROM workflow_runs WHERE id = item.workflow_run_id)` | **false — the run is gone** |
| `pr_url` on the item path | nothing to read |

So the item is not "missing a PR". It is pointing at **a run that no longer exists**, and the PR was never
stored on the item itself.

### Why the dangling reference is possible

There is **no foreign key from `work_items.workflow_run_id` to `workflow_runs`**. The only FK in that
relationship runs the other way:

```sql
-- db/migrations/20260721050000_workflow_runs_work_item_id_on_delete_set_null.sql
ALTER TABLE workflow_runs
  ADD CONSTRAINT workflow_runs_work_item_id_fkey
    FOREIGN KEY (work_item_id) REFERENCES work_items(id)
    ON DELETE SET NULL;
```

That constraint protects *runs* from losing their *work item*. Nothing protects a *work item* from losing its
*run*. So a deleted run leaves every work item that referenced it permanently dangling — silently, because
nothing errors.

This instance's audit trail shows the deletions that can do it, and they have happened at volume:
`workflow.deleted` 305 (09-06), 49 (08-25), 42 (09-05), 24 (09-17) — **deleting a workflow cascades to its
entire run history** — plus `execution.deleted` / `execution.batch_deleted` events including 85 single deletes
on 10-18.

> **The practical consequence for anyone trimming history:** deleting workflow runs and executions is a
> supported, useful maintenance act — but it **strips work items of their provenance**. The items keep reading
> `succeeded` with no branch, no PR and no run. That is expected, not corruption, and it cannot be reconstructed
> from what is left.

---

## The full decision chain, for an item that still HAS its run

If the run exists and the link is still missing, walk these in order — each is a real branch in
`internal/scheduler/workflow_reconciler.go`.

### 1. Did it run under a workflow run at all?

The whole PR mechanism hangs off `workflow_runs`:

- `deterministicPRForBranch(repoSlug, run.WorktreeBranch)` verifies the PR on GitHub;
- the result is merged into the run's `run_context` JSONB (`pr_url`, `pr_state`);
- and mirrored onto `worker_executions.pr_url` / `pr_state`, which is what the **work-item card** reads
  (migration `20260824010000_worker_executions_pr.sql`: *"…mirrored from the parent workflow run at dispatch"*).

A **standalone dispatch** — a work item assigned a worker directly, with no workflow bound — creates an
execution with `workflow_run_id = ''`. There is no run to hold a branch, no `run_context` to merge into and
nothing to mirror from, so the work succeeds and no link can ever appear.

### 2. Did the run have a worktree branch?

A PR needs a head. `workflow_runs.worktree_branch` is set at arm time; a run whose worktree was `none`
(ephemeral), or whose provisioning was `failed`/`skipped`, has none.

### 3. Was the effective git strategy `pr`?

**A PR is only required — and only looked for — under `git_strategy = pr`.** From the reconciler:

> *"Guard rail: PR-producing steps must have a PR URL on success — but only when the effective git strategy is
> "pr" … for "local" (push branch only) and "none" (ephemeral) a PR is not required and success without pr_url
> is still success."*

Resolution order (`db.EffectiveGitStrategy`): **workflow → project → `local`**. The default is `local`
(`db/migrations/20260910000000_add_git_strategy.sql`), and `CreateProject` defaults to it too. Everything that
ran while the project was `local` legitimately has no PR link, and `succeeded` is correct.

### 4. Could the PR be verified?

`deterministicPRForBranch` returns `verified=false` when `gh` is unavailable/unauthenticated, `projects.repo_slug`
is NULL (git detection never ran), or no branch was recorded. On `false` the code deliberately degrades to
already-recorded sources rather than failing a possibly-valid run. If no source has a PR, the step succeeds
with no link.

### 5. Was the mirror write lost? (run shows the PR, item does not)

A genuine, previously-hit bug, documented in the reconciler:

> *"THIS WRITE USED A STALE VERSION AND WAS BEST-EFFORT, which is how a merged PR became invisible. Field
> incident (run `01M54EJY52ZW1NSDAQ5X07JN38`): `exec.Version` was captured earlier in this pass, the mirror lost
> the optimistic-concurrency race, and the only trace was `WARN step-success: update execution PR fields
> (best-effort) … error="db: not found"`. The run still completed with pr_url in run_context, so the RUN page
> showed the PR — but the work-item card reads the EXECUTION row, so it showed nothing at all."*

**The tell that separates this from every cause above:** the **run** has `run_context->>'pr_url'` set and the
**execution** row's `pr_url` is NULL. If the run has no PR either, it is causes 1–4 or a deleted run.

Both mitigations are now in place: the mirror re-reads the execution for its current version on each attempt
(`retryStaleVersion`), and the work-item card reads the run as well as the execution.

---

## Classify an item

Per **work item**, not per execution — a card shows a pill when *any* of the item's executions has one, so a
per-execution join reports false "no PR" rows for items whose PR sits on a sibling execution:

```sql
SELECT wi.title,
       count(e.id)                                            AS executions,
       bool_or(COALESCE(e.workflow_run_id,'') <> '')           AS has_run,
       bool_or(COALESCE(e.worktree_branch,'') <> '')           AS has_branch,
       bool_or(COALESCE(e.pr_url,'') <> '')                    AS item_has_pr,
       bool_or(COALESCE(wr.run_context->>'pr_url','') <> '')   AS run_has_pr,
       p.git_strategy,
       COALESCE(wf.git_strategy,'(inherit)')                   AS workflow_strategy,
       COALESCE(p.repo_slug,'<null>')                          AS repo_slug,
       -- does the run the item points at still exist?
       (wi.workflow_run_id <> '' AND NOT EXISTS (
           SELECT 1 FROM workflow_runs w2 WHERE w2.id = wi.workflow_run_id)) AS run_is_gone
FROM work_items wi
LEFT JOIN projects          p  ON p.id  = wi.project_id
LEFT JOIN workflows         wf ON wf.id = wi.workflow_id
LEFT JOIN worker_executions e  ON e.task_id = wi.id AND e.tenant_id = wi.tenant_id
LEFT JOIN workflow_runs     wr ON wr.id = e.workflow_run_id
WHERE wi.tenant_id = 'tnt_dev'
  AND wi.status = 'succeeded'
GROUP BY wi.id, wi.title, wi.updated_at, p.git_strategy, wf.git_strategy, p.repo_slug
HAVING NOT bool_or(COALESCE(e.pr_url,'') <> '')
ORDER BY wi.updated_at DESC;
```

| What you see | Cause | Recoverable? |
|---|---|---|
| `executions = 0` **and** `run_is_gone = true` | run/executions were deleted — provenance stripped | **No.** Nothing left to read |
| `executions = 0`, `has_run = false`, run id empty | ran standalone — never had a run | No backfill |
| `has_branch = false` | run had no worktree branch | No — nothing to open a PR from |
| `project_strategy`/`workflow_strategy` = `local`/`none` | no PR was ever expected | Not a defect |
| `run_has_pr = true`, `item_has_pr = false` | **the lost mirror (cause 5)** | **Yes** — re-stamp / `backfill-pr` |
| `repo_slug = <null>` | git detection never ran | Set the slug/strategy, then backfill |

## Repair

**Cause 5, or any run whose branch already has a PR:**

```bash
orchicon backfill-pr --dry-run    # report what it would stamp
orchicon backfill-pr              # resolve each terminal git-backed run's branch via gh
```

Idempotent and out-of-band: it fills runs with no PR recorded, and runs whose execution columns are empty even
when `run_context` already carries a verified PR. Runs whose branch resolves to no PR are left alone.

**Deleted provenance (the dominant case here) cannot be repaired** — the run and executions are gone. The
`PR merged` pill returns only if the work is re-run.

## Preventing the gap

- **Set the project to `git_strategy = pr`** before generating work. Under the `local` default no PR is
  required and none is surfaced.
- **Run work through a workflow** (a sequence, or a workflow-bound item) rather than dispatching standalone, if
  PR traceability matters.
- **Keep `gh` authenticated** on the plane's host — it is what makes verification deterministic.
- **Expect a gap when you trim history.** Deleting workflow runs or executions is supported and useful, and it
  will leave `succeeded` items with no branch, no PR and a dangling `workflow_run_id`. If you want the links to
  survive a clean-up, export the PR URLs first — see also `docs/outbox-retention.md` for the same
  "what is safe to prune" reasoning.
