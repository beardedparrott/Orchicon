import { ArrowDown, ArrowUp, ArrowUpDown } from "lucide-react";

import type { SortDir } from "@/components/cost-explorer/utils";
import { cn } from "@/lib/utils";

// SortableHeader is a sortable <th> for the Recent usage records table. The
// active column shows an up/down arrow; clicking toggles direction via
// toggleSort.
export function SortableHeader<K extends string>({
  label,
  sortKey,
  activeKey,
  dir,
  onSort,
  align = "left",
  className,
}: {
  label: string;
  sortKey: K;
  activeKey: K;
  dir: SortDir;
  onSort: (key: K) => void;
  align?: "left" | "right";
  className?: string;
}) {
  const active = activeKey === sortKey;
  const Icon = !active ? ArrowUpDown : dir === "asc" ? ArrowUp : ArrowDown;
  return (
    <th
      className={cn(
        "px-1 py-1 pr-3 font-medium uppercase tracking-wide text-xs",
        align === "right" && "text-right",
        className,
      )}
    >
      <button
        type="button"
        onClick={() => onSort(sortKey)}
        aria-label={`Sort by ${label}`}
        aria-sort={active ? (dir === "asc" ? "ascending" : "descending") : undefined}
        className={cn(
          "inline-flex items-center gap-1 text-muted-foreground hover:text-foreground",
          active && "text-foreground",
          align === "right" && "flex-row-reverse",
        )}
      >
        {label}
        <Icon className="h-3 w-3 opacity-70" />
      </button>
    </th>
  );
}
