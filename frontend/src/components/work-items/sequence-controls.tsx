// SequenceControls — manual Start / Resume / Stop buttons for ANY work
// item, shown on the work-item overview page (board + tree) only — not on
// the detail page. The buttons are a pure
// function of the item's server-reported status + whether it has children
// (the server does the real validation via ControlSequence, so a stale
// render can never fire an invalid gesture):
//
//   - PARENT (has children — a sequence run): START re-fires the chain from
//     child #1 (destructive — confirm-gated, wipes prior child successes);
//     RESUME continues from the first non-succeeded child (keeps state);
//     STOP halts the WHOLE subtree — every descendant parks to pending and
//     any in-flight workflow run is aborted, so a stopped chain stays
//     stopped until started/resumed again.
//   - LEAF (no children — runs its own bound workflow): START fires the
//     item's bound workflow immediately; RESUME re-arms a failed/cancelled
//     leaf; STOP parks the leaf and aborts its run.
//
// Invariant #1: no business logic in the frontend — the visible buttons
// are a pure function of server-reported state, and every click goes
// through the generated ControlSequence client.

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

const QUEUED = new Set([WorkItemStatus.READY, WorkItemStatus.ASSIGNED]);

const isActiveRun = (status: WorkItemStatus) => ACTIVE.has(status);
const isQueued = (status: WorkItemStatus) => QUEUED.has(status);

/** Which gestures are available for an item's current status + shape. */
export function availableActions(status: WorkItemStatus, hasChildren: boolean): SequenceAction[] {
  const out: SequenceAction[] = [];
  if (hasChildren) {
    // Sequence parent.
    if (!isActiveRun(status)) out.push(SequenceAction.START);
    if (status === WorkItemStatus.FAILED || status === WorkItemStatus.PENDING) {
      out.push(SequenceAction.RESUME);
    }
    if (status !== WorkItemStatus.PENDING && status !== WorkItemStatus.SUCCEEDED) {
      out.push(SequenceAction.STOP);
    }
  } else {
    // Leaf — runs its own bound workflow.
    if (!isActiveRun(status) && !isQueued(status)) out.push(SequenceAction.START);
    if (status === WorkItemStatus.FAILED || status === WorkItemStatus.CANCELLED) {
      out.push(SequenceAction.RESUME);
    }
    if (status !== WorkItemStatus.SUCCEEDED && status !== WorkItemStatus.CANCELLED) {
      out.push(SequenceAction.STOP);
    }
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
      "Fire this work item now. For a parent (has children): re-fires the chain from child #1 — destructive, wipes prior successes. For a leaf: starts its bound workflow.",
  },
  [SequenceAction.RESUME]: {
    label: "Resume",
    icon: RotateCcw,
    title:
      "Re-arm this work item. For a parent: continues from the first non-succeeded child, keeping prior results. For a leaf: re-runs its workflow.",
  },
  [SequenceAction.STOP]: {
    label: "Stop",
    icon: Pause,
    title:
      "Halt this work item. For a parent: stops the whole subtree and aborts in-flight runs. For a leaf: stops just this item and aborts its run.",
  },
  [SequenceAction.UNSPECIFIED]: { label: "", icon: Play, title: "" },
};

export function SequenceControls({
  item,
  hasChildren,
  className,
}: {
  /** the work item to control (parent or leaf) */
  item: WorkItem;
  /** whether the item has children — the sequence-parent determinant */
  hasChildren: boolean;
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
                  ? "Started"
                  : action === SequenceAction.RESUME
                    ? "Resumed"
                    : "Stopped",
            },
          );
        },
        onError: (err) => {
          toast.error(err instanceof Error ? err.message : String(err), {
            title: "Could not control work item",
          });
        },
      },
    );
  };

  const actions = availableActions(item.status, hasChildren);
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
            aria-label={`${meta.label} "${item.title}"`}
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
