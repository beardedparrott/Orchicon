import { ArrowDown, ArrowUp, ArrowUpDown } from "lucide-react";

import {
  toggleSort,
  type SortKey,
  type SortState,
} from "@/components/cost-explorer/utils";
import { cn } from "@/lib/utils";

// SortControls is a compact toolbar of sort pills for the non-tabular cost
// panels (the rollup summary list and the By Workflow tree). Clicking a pill
// toggles the sort via toggleSort; the active pill carries a direction arrow.
export function SortControls({
  sort,
  onChange,
  options = [
    { key: "name", label: "Name" },
    { key: "cost", label: "Cost" },
    { key: "tokens", label: "Tokens" },
    { key: "finished", label: "Finished" },
  ],
}: {
  sort: SortState<SortKey>;
  onChange: (sort: SortState<SortKey>) => void;
  options?: { key: SortKey; label: string }[];
}) {
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {options.map((opt) => {
        const active = sort.key === opt.key;
        const Icon = !active ? ArrowUpDown : sort.dir === "asc" ? ArrowUp : ArrowDown;
        return (
          <button
            key={opt.key}
            type="button"
            onClick={() => onChange(toggleSort(sort, opt.key))}
            aria-pressed={active}
            className={cn(
              "inline-flex items-center gap-1 rounded-md border px-2.5 py-1 text-xs font-medium text-muted-foreground hover:bg-accent",
              active && "border-primary text-foreground",
            )}
          >
            {opt.label}
            <Icon className="h-3 w-3 opacity-70" />
          </button>
        );
      })}
    </div>
  );
}
