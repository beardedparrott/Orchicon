// PR-link helpers for the run/execution surfaces (parallel board view).
//
// A run's PR link is resolved only from the authoritative per-branch DevOps
// worker-authored pr_url/pr_state captured into the run's structured
// `run_context` (parsed server-side into WorkflowRun/WorkerExecution
// prUrl/prState). There is no synthesized fallback: a run that has no captured
// PR renders no chip. The link renders only for completed (terminal) runs,
// where the PR — if any — is a settled fact.
//
// Kept pure so it is unit-testable and theme-agnostic.

export interface PrRun {
  prUrl?: string;
  prState?: string;
  /** worktree git branch — used for display in the run footer (not for
   *  PR-link resolution, which is driven by prUrl + completed) */
  worktreeBranch?: string;
  /** true when the run has reached a terminal/completed state — only then does
   *  an authored PR count as settled and a link render */
  completed?: boolean;
}

export interface PrLink {
  href: string;
  /** human label for the chip (e.g. "PR merged") */
  label: string;
}

/** Resolve the effective PR link for a completed run, or null when there is
 *  none (in-flight run, or completed run with no captured PR). */
export function prLinkForRun(run: PrRun): PrLink | null {
  if (run.completed && run.prUrl) {
    return { href: run.prUrl, label: prStateLabel(run.prState) };
  }
  return null;
}

/** True when a WorkerExecution has reached a terminal (settled) state.
 *  Matches ExecutionStatus.TERMINATED|FAILED_TO_START|SUCCEEDED|FAILED. */
export function isTerminalExecutionStatus(status?: number): boolean {
  return status === 7 || status === 8 || status === 9 || status === 10;
}

/** Human label for a PR state value (empty/unknown → generic "View PR"). */
export function prStateLabel(state?: string): string {
  switch (state) {
    case "open":
      return "PR open";
    case "merged":
      return "PR merged";
    case "draft":
      return "PR draft";
    case "closed":
      return "PR closed";
    case "none":
      return "No PR";
    default:
      return "View PR";
  }
}
