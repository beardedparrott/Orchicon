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
import { RUN_CONTROL_META, controlLabelFor, hasEverRun, restartTitle, runControlsFor } from "./run-controls";

/**
 * Which gestures to show. THE RULE LIVES IN run-controls.ts so the work item page, the run page
 * and the schedules page cannot drift; this keeps the name the existing callers and tests use.
 */
export function availableActions(status: WorkItemStatus, hasChildren: boolean): SequenceAction[] {
  return runControlsFor(status, hasChildren);
}

const ACTION_META: Record<
  SequenceAction,
  { label: string; icon: typeof Play; title: string }
> = {
  // RESTART IS THE OLD START. It was always a destructive re-fire; the label now says so, and
  // therefore so does the confirm step.
  [SequenceAction.RESTART]: {
    label: RUN_CONTROL_META[SequenceAction.RESTART].label,
    icon: Play,
    title: RUN_CONTROL_META[SequenceAction.RESTART].title,
  },
  [SequenceAction.RESUME]: {
    label: RUN_CONTROL_META[SequenceAction.RESUME].label,
    icon: RotateCcw,
    title:
      "Continue the work that exists. Parent: from the first non-succeeded child. Leaf: the bound run is resumed in place — succeeded steps stay succeeded and the failed ones carry the previous attempt's notes.",
  },
  [SequenceAction.PAUSE]: {
    label: RUN_CONTROL_META[SequenceAction.PAUSE].label,
    icon: Pause,
    title: RUN_CONTROL_META[SequenceAction.PAUSE].title,
  },
  [SequenceAction.STOP]: {
    label: "Stop",
    icon: Pause,
    title:
      "Halt this work item. For a parent: stops the whole subtree and aborts in-flight runs — confirm to abort running children. For a leaf: prefer Pause, which keeps the worktree and branch; Stop aborts the run.",
  },
  [SequenceAction.UNSPECIFIED]: { label: "", icon: Play, title: "" },
};



export function SequenceControls({
  item,
  hasChildren,
  className,
  only,
}: {
  /** the work item to control (parent or leaf) */
  item: WorkItem;
  /** whether the item has children — the sequence-parent determinant */
  hasChildren: boolean;
  className?: string;
  /**
   * Restrict the rendered gestures to this set. Used by the Schedules views, where which gestures
   * make sense is decided by the VIEW rather than the item's status: Running offers the two stops,
   * History offers the two continuations, Upcoming offers nothing at all.
   *
   * Enforcing it here rather than filtering at each call site means a view cannot accidentally
   * render a gesture its own rule excludes.
   */
  only?: readonly SequenceAction[];
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
            action === SequenceAction.RESTART
              ? `"${updated.title}" is now ${updated.status}.`
              : action === SequenceAction.RESUME
                ? `"${updated.title}" resumed — now ${updated.status}.`
                : `"${updated.title}" stopped — now ${updated.status}.`,
            {
              title:
                action === SequenceAction.RESTART
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

  let actions = availableActions(item.status, hasChildren);
  if (only) actions = actions.filter((a) => only.includes(a));
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
        // START VS RESTART: the same call, but the honest word depends on whether anything has run.
        // An item that has never been dispatched has nothing to restart and nothing to discard.
        //
        // THE `!` HERE WAS INVERTED and shipped: `!isFiredOnce(status)` is true for a PENDING item,
        // so every never-run item was labelled "Restart". The unit test called restartLabel(false)
        // directly and never exercised this wiring — which is why a green suite showed the wrong
        // word on every row. Fixed to the plain reading: has run if it has a run id OR has ever been
        // dispatched.
        // ONE PURE DECISION, in run-controls.ts, so a test can assert what this row RENDERS. The
        // previous inline version had an inverted negation that no helper-level test could see.
        const label = controlLabelFor(action, item.status, meta.label);
        const titleText =
          action === SequenceAction.RESTART
            ? restartTitle(hasEverRun(item.status))
            : meta.title;
        const Icon = meta.icon;
        const destructive = action === SequenceAction.RESTART || action === SequenceAction.STOP;
        return (
          <button
            key={action}
            type="button"
            disabled={isPending}
            aria-label={`${label} "${item.title}"`}
            title={titleText}
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
              <Loader2 aria-hidden="true" className="h-3 w-3 animate-spin" />
            ) : (
              <Icon aria-hidden="true" className="h-3 w-3" />
            )}
            {/* THE VISIBLE TEXT IS `label`, NOT `meta.label`.
                This rendered meta.label — the static ACTION_META word, which is "Restart" — while the
                computed `label` went only to aria-label. So Start/Restart was correct in the
                accessibility tree and wrong on screen, and a helper-level test could not see it: the
                component computed the right value and then did not use it.
                One source for both, so a screen reader and the pixels cannot disagree. */}
            {confirming === action
              ? action === SequenceAction.STOP
                ? "Abort running children?"
                : "Confirm?"
              : label}
          </button>
        );
      })}
    </span>
  );
}
