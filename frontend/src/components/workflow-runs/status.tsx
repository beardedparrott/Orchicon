// Status pills shared by the run Graph view and the Heads-Up tiles so
// both views read identically. (Moved out of the run route file; the
// Graph JSX that renders them is unchanged.)
import { cn } from "@/lib/utils";
import {
  EXEC_STATUS_LABELS,
  EXEC_STATUS_STYLES,
  STEP_RUN_STATUS_COLORS,
  STEP_RUN_STATUS_LABELS,
} from "./headsUp";

export function StepStatusPill({ status }: { status: number }) {
  return (
    <span
      className={cn(
        "rounded-full px-2 py-0.5 text-xs font-medium",
        STEP_RUN_STATUS_COLORS[status] ?? "bg-gray-200 text-gray-700",
      )}
    >
      {STEP_RUN_STATUS_LABELS[status] ?? "pending"}
    </span>
  );
}

export function ExecStatusBadge({ status }: { status: number }) {
  return (
    <span
      className={cn(
        "rounded-full px-2 py-0.5 text-xs font-medium",
        EXEC_STATUS_STYLES[status] ?? "bg-muted text-muted-foreground",
      )}
    >
      {EXEC_STATUS_LABELS[status] ?? "unknown"}
    </span>
  );
}
