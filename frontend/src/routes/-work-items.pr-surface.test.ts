import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// The work-item card's PR source, pinned.
//
// THE FIELD FAILURE THIS PINS (2026-10-17, run 01M54EJY52ZW1NSDAQ5X07JN38). The
// run COMPLETED with PR #737 merged, and `run_context` carried
// {"pr_url":"…/pull/737","pr_state":"merged"}. But the pass that mirrors that PR
// onto the EXECUTION row is best-effort, and it lost its optimistic-concurrency
// race:
//
//	03:46:43 WARN step-success: update execution PR fields (best-effort)
//	         execution=01M54FE48… error="db: not found"
//
// The run page still showed the PR (it reads the run). The work-item card did
// not — because it built its PR surface ONLY from executions. One silent WARN,
// no chip, and no way for an operator to tell "no PR" from "mirror lost".
//
// So the card reads the RUN first (run_context, written in the transaction that
// completes the run, and the value the deterministic PR check verified against
// GitHub) and keeps executions only as the fallback for items that have no run.
//
// There is no DOM harness in this repo (no @testing-library/react), so this
// asserts structure in the same idiom as RunBlockageCard.test.ts.
const src = fs.readFileSync(path.join(__dirname, "work-items.tsx"), "utf8");

describe("work-item PR surface", () => {
  it("reads the RUN as the authoritative source", () => {
    expect(src).toContain("useListWorkflowRuns");
    expect(src).toContain("r.workItemId");
    expect(src).toContain("r.prUrl");
    expect(src).toContain("r.prState");
  });

  it("still reads executions, as the fallback for items with no run", () => {
    expect(src).toContain("useListExecutions");
    expect(src).toContain("isTerminalExecutionStatus(e.status)");
  });

  it("lets the run WIN for an item that has one", () => {
    // The fallback must never overwrite the run's PR: an in-flight or
    // mirror-less execution row would otherwise mask a settled, merged PR —
    // which is exactly the failure being fixed.
    expect(src).toContain("if (!e.taskId || m.has(e.taskId)) continue;");
  });

  it("populates from runs BEFORE it considers executions", () => {
    const runLoop = src.indexOf("for (const r of workflowRuns ?? [])");
    const execLoop = src.indexOf("for (const e of executions ?? [])");
    expect(runLoop).toBeGreaterThan(-1);
    expect(execLoop).toBeGreaterThan(-1);
    expect(runLoop).toBeLessThan(execLoop);
  });

  it("treats a run as settled when terminal, not only when it succeeded", () => {
    // A run that FAILED can still have authored a PR, and its state is just as
    // settled as a success — the chip must not require a green run.
    expect(src).toContain("WorkflowRunStatus.COMPLETED");
    expect(src).toContain("WorkflowRunStatus.FAILED");
    expect(src).toContain("WorkflowRunStatus.ABORTED");
  });

  it("does not gate the PR on the item's own status", () => {
    // The incident item was `succeeded` with no chip, and the one before it was
    // wrongly `failed` WITH a merged PR. Neither item status is the right
    // question; the SETTLED RUN is.
    const block = src.slice(src.indexOf("const runsByItem = useMemo"), src.indexOf("const blockState"));
    expect(block).not.toContain("item.status");
  });
});
