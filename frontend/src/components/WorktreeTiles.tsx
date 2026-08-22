// WorktreeTiles — shared worktree/branch display used by the execution
// detail and workflow-run detail views. One status→display mapping keeps
// both views consistent (docs/10 §5 metadata tiles):
//   - ready    → status + branch + path (all three present)
//   - skipped  / failed → neutral "Runs in place" in-place state, no branch
//     (the non-repo fallback; provisioning failure falls back the same way)
//   - pending  → status only, provisioning in progress
//   - pruned   → status only, the worktree was reaped
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
      return [{ label: "Worktree", value: "Pruned" }];
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
