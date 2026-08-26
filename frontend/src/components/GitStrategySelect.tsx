import { Label } from "@/components/ui/label";

export type GitStrategy = "local" | "pr" | "none";
export type GitStrategyNullable = GitStrategy | "inherit";

export const GIT_STRATEGY_OPTIONS: { value: GitStrategy; label: string; description: string }[] = [
  {
    value: "local",
    label: "Local — push branch only",
    description: "Commit in isolated worktree and push the branch to origin. No PR. Branch remains on origin for manual review or direct merge. Preserves parallel isolation, no GitHub review required. Worktree is pruned but branch is durable.",
  },
  {
    value: "pr",
    label: "PR — push + create PR",
    description: "Push branch and auto-create a PR to the remote (GitHub). Requires a GitHub remote and respects branch protection. Best when you require human review before merging to develop. Full audit trail via PR.",
  },
  {
    value: "none",
    label: "Ephemeral — no push",
    description: "Don't push at all. Work lives only in execution Results/artifacts. Worktree is pruned and branch discarded on success. Best for checks, scrapes, and recurring monitors where no code change should persist.",
  },
];

export function gitStrategyDescription(value: GitStrategy | string | undefined): string {
  const opt = GIT_STRATEGY_OPTIONS.find((o) => o.value === value);
  return opt?.description ?? "";
}

export function GitStrategySelect({
  value,
  onValueChange,
  includeInherit,
  inheritDescription,
}: {
  value: GitStrategyNullable;
  onValueChange: (v: GitStrategyNullable) => void;
  includeInherit?: boolean;
  inheritDescription?: string;
}) {
  const isInherit = value === "inherit";
  const effectiveDescription = isInherit
    ? inheritDescription ?? "Use the project's default git strategy. The workflow inherits whatever the project is configured to use."
    : gitStrategyDescription(value);

  return (
    <div className="space-y-2">
      <Label htmlFor="git-strategy">Git strategy</Label>
      <select
        id="git-strategy"
        value={value}
        onChange={(e) => onValueChange(e.target.value as GitStrategyNullable)}
        className="flex h-9 w-full rounded-md border border-input bg-background px-3 py-1 text-sm"
      >
        {includeInherit && <option value="inherit">Inherit from project (default)</option>}
        {GIT_STRATEGY_OPTIONS.map((opt) => (
          <option key={opt.value} value={opt.value}>
            {opt.label}
          </option>
        ))}
      </select>
      {effectiveDescription && (
        <p className="text-xs text-muted-foreground leading-relaxed">{effectiveDescription}</p>
      )}
      <p className="text-xs text-muted-foreground">
        Worktrees are <span className="font-medium">always</span> provisioned for isolation — even for automation. This only controls what happens to the branch after success.
      </p>
    </div>
  );
}

export function gitStrategyToProto(v: GitStrategy | GitStrategyNullable): number | undefined {
  switch (v) {
    case "local": return 1; // GIT_STRATEGY_LOCAL
    case "pr": return 2; // GIT_STRATEGY_PR
    case "none": return 3; // GIT_STRATEGY_NONE
    case "inherit": return undefined;
    default: return undefined;
  }
}
