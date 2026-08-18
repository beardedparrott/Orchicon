// PR-link helpers for the run/execution surfaces (parallel board view).
//
// A run's PR link is resolved in two ways, both provider-free on the read
// path:
//   1. Authoritative: the per-branch DevOps worker writes pr_url/pr_state
//      into the run's structured `run_context` (parsed server-side into
//      WorkflowRun/WorkerExecution.prUrl/prState).
//   2. Deterministic fallback: when pr_url is empty but the run has a
//      provisioned ready worktree with a branch, synthesize a GitHub
//      "new/compare PR" link from the project's stored origin (repo_slug)
//      so every provisioned git-backed run can still jump to a PR even
//      before one exists.
//
// Kept pure so it is unit-testable and theme-agnostic.

export interface PrRun {
  prUrl?: string;
  prState?: string;
  worktreeStatus?: string;
  worktreeBranch?: string;
  /** per-run project git origin slug (owner/repo) for the deterministic
   *  fallback — overrides the card-level repoSlug when set, so items in the
   *  cross-project ("all projects") view can still resolve their fallback */
  repoSlug?: string;
}

export interface PrLink {
  href: string;
  /** human label for the chip (e.g. "PR open", "No PR yet") */
  label: string;
  /** true when this is the deterministic pull/new fallback, not an
   *  actual authored PR */
  isFallback: boolean;
}

/** Resolve the effective PR link for a run, or null when there is none. */
export function prLinkForRun(run: PrRun, repoSlug?: string): PrLink | null {
  if (run.prUrl) {
    return { href: run.prUrl, label: prStateLabel(run.prState), isFallback: false };
  }
  const slug = run.repoSlug || repoSlug;
  if (run.worktreeStatus === "ready" && run.worktreeBranch && slug) {
    return {
      href: `https://github.com/${slug}/pull/new/${encodeURIComponent(run.worktreeBranch)}`,
      label: "No PR yet",
      isFallback: true,
    };
  }
  return null;
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
