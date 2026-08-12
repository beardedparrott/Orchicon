// SequenceControls — manual Start / Resume / Stop buttons for a sequence
// parent (a work item with children IS a sequence run; see
// sequence-utils.ts). Display-only gating from the parent's derived
// status — the server does the real validation (ControlSequence RPC), so
// a stale render can never fire an invalid gesture:
//   - START re-fires the chain from child #1 (destructive — confirm-gated,
//     wipes prior child successes); enabled when not actively sequencing.
//   - RESUME continues from the first non-succeeded child (keeps state);
//     enabled when halted (failed chain) or parked (pending with children).
//   - STOP parks the chain (parent → pending, schedule cleared) so
//     children can be run standalone; an in-flight child finishes
//     naturally; enabled when running/failed.
//
// Invariant #1: no business logic in the frontend — the visible buttons
// are a pure function of the parent's server-reported status, and every
// click goes through the generated ControlSequence client.

import { Loader2, Pause, Play, RotateCcw } from "lucide-react";
import { useState } from "react";

import { useControlSequence } from "@/api/workItems";
import { SequenceAction } from "@/api/gen/orchicon/api/v1/work_item_service_pb";
import { WorkItemStatus, type WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { useToast } from "@/components/ui/toast";
import { cn } from "@/lib/utils";

const ACTIVE = new Set([
  WorkItemStatus.RUNNING,
  WorkItemStatus.CHECKPOINTING,
  WorkItemStatus.RECOVERING,
]);

const isActiveRun = (status: WorkItemStatus) => ACTIVE.has(status);

/** Which gestures are available for a parent's current status. */
function availableActions(status: WorkItemStatus): SequenceAction[] {
  const out: SequenceAction[] = [];
  if (!isActiveRun(status)) out.push(SequenceAction.START);
  if (status === WorkItemStatus.FAILED || status === WorkItemStatus.PENDING) {
    out.push(SequenceAction.RESUME);
  }
  if (status === WorkItemStatus.RUNNING || status === WorkItemStatus.FAILED) {
    out.push(SequenceAction.STOP);
  }
  return out;
}

const ACTION_META: Record<
  SequenceAction,
  { label: string; icon: typeof Play; title: string }
> = {
  [SequenceAction.START]: {
    label: "Start",
    icon: Play,
    title:
      "Re-fire the chain from child #1. Destructive: every descendant resets to pending, wiping prior successes.",
  },
  [SequenceAction.RESUME]: {
    label: "Resume",
    icon: RotateCcw,
    title:
      "Continue from the first non-succeeded child, keeping prior results.",
  },
  [SequenceAction.STOP]: {
    label: "Stop",
    icon: Pause,
    title:
      "Park the chain (parent → pending, schedule cleared). An in-flight child finishes naturally.",
  },
  [SequenceAction.UNSPECIFIED]: { label: "", icon: Play, title: "" },
};

export function SequenceControls({
  item,
  className,
}: {
  /** the sequence parent (an item with children) */
  item: WorkItem;
  className?: string;
}) {
  const { mutate, isPending } = useControlSequence();
  const toast = useToast();
  const [confirming, setConfirming] = useState<SequenceAction | null>(null);

  const run = (action: SequenceAction) => {
    mutate(
      { id: item.id, action },
      {
        onSuccess: (updated) => {
          toast.success(
            action === SequenceAction.START
              ? `"${updated.title}" is now ${updated.status}.`
              : action === SequenceAction.RESUME
                ? `"${updated.title}" resumed — now ${updated.status}.`
                : `"${updated.title}" stopped — now ${updated.status}.`,
            {
              title:
                action === SequenceAction.START
                  ? "Sequence started"
                  : action === SequenceAction.RESUME
                    ? "Sequence resumed"
                    : "Sequence stopped",
            },
          );
        },
        onError: (err) => {
          toast.error(err instanceof Error ? err.message : String(err), {
            title: "Could not control sequence",
          });
        },
      },
    );
  };

  const actions = availableActions(item.status);
  if (actions.length === 0) return null;

  return (
    <span
      className={cn("inline-flex items-center gap-0.5", className)}
      onClick={(e) => e.stopPropagation()}
      onPointerDown={(e) => e.stopPropagation()}
      onKeyDown={(e) => e.stopPropagation()}
    >
      {actions.map((action) => {
        const meta = ACTION_META[action];
        const Icon = meta.icon;
        const destructive = action === SequenceAction.START;
        return (
          <button
            key={action}
            type="button"
            disabled={isPending}
            aria-label={`${meta.label} sequence "${item.title}"`}
            title={meta.title}
            onClick={() => {
              if (destructive) {
                if (confirming === action) {
                  setConfirming(null);
                  run(action);
                } else {
                  setConfirming(action);
                  window.setTimeout(() => setConfirming(null), 4000);
                }
                return;
              }
              run(action);
            }}
            className={cn(
              "inline-flex h-5 items-center gap-1 rounded px-1.5 text-[11px] font-medium transition-colors focus-visible:ring-2 focus-visible:ring-ring",
              destructive
                ? confirming === action
                  ? "bg-red-500/20 text-red-700 dark:text-red-300"
                  : "text-muted-foreground hover:bg-accent hover:text-foreground"
                : "text-muted-foreground hover:bg-accent hover:text-foreground",
            )}
          >
            {isPending ? (
              <Loader2 aria-hidden className="h-3 w-3 animate-spin" />
            ) : (
              <Icon aria-hidden className="h-3 w-3" />
            )}
            {confirming === action ? "Confirm?" : meta.label}
          </button>
        );
      })}
    </span>
  );
}
