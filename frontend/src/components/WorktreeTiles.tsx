// WorktreeTiles — shared worktree/branch display used by the execution
// detail and workflow-run detail views. One status→display mapping keeps
// both views consistent (docs/10 §5 metadata tiles):
//   - ready    → status + branch + path (all three present)
//   - skipped  / failed → neutral "Runs in place" in-place state, no branch
//     (the non-repo fallback; provisioning failure falls back the same way)
//   - pending  → status only, provisioning in progress
//   - pruned   → the CHECKOUT was reaped; the branch is reported separately and
//     survives. "Pruned" on its own read as data loss when only a disposable
//     checkout had gone.
//   - empty/other → render nothing; no branch is ever shown unless the
//     worktree status is "ready".
export type WorktreeTile = { label: string; value: string };

export function worktreeTileItems(
  worktreeStatus?: string,
  worktreeBranch?: string,
  worktreePath?: string,
): WorktreeTile[] {
  switch (worktreeStatus) {
    case "ready":
      return [
        { label: "Worktree", value: "Ready" },
        ...(worktreeBranch ? [{ label: "Branch", value: worktreeBranch }] : []),
        ...(worktreePath ? [{ label: "Path", value: worktreePath }] : []),
      ];
    case "skipped":
    case "failed":
      return [{ label: "Worktree", value: "Runs in place" }];
    case "pending":
      return [{ label: "Worktree", value: "Pending" }];
    case "pruned":
      // "Pruned" READ AS DATA LOSS AND IS THE OPPOSITE.
      //
      // Two different things share this status and the operator cannot tell them apart:
      //   - the WORKTREE (a disposable checkout) was reaped — a few seconds to rebuild
      //   - the BRANCH (the commits, the actual work) was deleted — unrecoverable
      // On abort, only the first is true: abort keeps the branch and reaps the checkout. Saying
      // "Pruned" flatly implied the work was gone when the branch was sitting on origin.
      //
      // So the tile names WHICH thing went and states the durable half. A branch that survives is
      // reported by name, because it is the thing an operator would resume or merge.
      return [
        { label: "Worktree", value: "Reaped (checkout removed)" },
        ...(worktreeBranch
          ? [{ label: "Branch", value: worktreeBranch + " — kept" }]
          : [{ label: "Branch", value: "None recorded" }]),
      ];
    default:
      return [];
  }
}

// WorktreeTiles renders the worktree tile grid (theme-aware, same card-tile
// styling as the execution context footer). Returns null when there is no
// worktree state to surface.
export function WorktreeTiles({
  worktreeStatus,
  worktreeBranch,
  worktreePath,
}: {
  worktreeStatus?: string;
  worktreeBranch?: string;
  worktreePath?: string;
}) {
  const items = worktreeTileItems(worktreeStatus, worktreeBranch, worktreePath);
  if (items.length === 0) return null;
  return (
    <dl className="grid grid-cols-1 gap-3 text-sm sm:grid-cols-2">
      {items.map((item) => (
        <div key={item.label}>
          <dt className="text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
            {item.label}
          </dt>
          <dd className="mt-0.5 break-all font-mono text-xs">{item.value}</dd>
        </div>
      ))}
    </dl>
  );
}
