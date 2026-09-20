// Bulk-set `workflow_id` + `runtime_image` on the selected work items.
//
// The bulk counterpart of the single-item form: ONE workflow picker, ONE runtime-image picker, and
// one write per marked item. It sits beside `batch-run.ts` on the same marked-selection primitive
// and reuses that file's partial/skip reporting idiom (one summary, counted failures).
//
// The three-state pickers (leave unchanged / clear / set) are what make the two operations
// INDEPENDENT: an untouched field is omitted from the request entirely, so a workflow-only set
// cannot clobber a runtime image.

import { useEffect, useMemo, useRef, useState } from "react";
import { AlertTriangle, Loader2, PencilLine } from "lucide-react";

import { Button } from "@/components/ui/button";
import { useAvailableRuntimeImages } from "@/api/runtimeImages";
import {
  SET_CLEAR,
  SET_SKIP,
  bulkSetConfirmText,
  buildSetTarget,
  useRunnableWorkflowOptions,
  type SetTarget,
} from "@/components/work-items/batch-set";
import { cn } from "@/lib/utils";

interface BulkSetDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** How many items the operation will touch (the visible-selected count). */
  count: number;
  /** Ids of the selection that are sequence parents — their own binding is INERT. */
  parentIds: string[];
  onSubmit: (target: SetTarget) => void;
  isPending: boolean;
}

export function BulkSetDialog({
  open,
  onOpenChange,
  count,
  parentIds,
  onSubmit,
  isPending,
}: BulkSetDialogProps) {
  const [wfSel, setWfSel] = useState<string>(SET_SKIP);
  const [imgSel, setImgSel] = useState<string>(SET_SKIP);
  const dialogRef = useRef<HTMLDialogElement>(null);

  const { options: workflows, hidden } = useRunnableWorkflowOptions();
  const images = useAvailableRuntimeImages();
  const imageOptions = useMemo(() => {
    const stock = images.data?.stockImages ?? [];
    const custom = images.data?.customImages ?? [];
    return [...stock, ...custom].filter((img, i, arr) => img && arr.indexOf(img) === i);
  }, [images.data]);

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    if (open) dialog.showModal();
    else dialog.close();
  }, [open]);

  useEffect(() => {
    if (!open) {
      // Reset on close so reopening starts from "leave unchanged" on both pickers.
      setWfSel(SET_SKIP);
      setImgSel(SET_SKIP);
    }
  }, [open]);

  const target = buildSetTarget(wfSel, imgSel);
  const nothingChosen = Object.keys(target).length === 0;
  const canApply = !isPending && !nothingChosen;

  const workflowLabel = workflows.find((w) => w.id === wfSel)?.name ?? "";

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!canApply) return;
    // The confirm names the COUNT and the VALUE(S), and calls out an inert sequence-parent binding
    // — the same wording the TUI's confirm uses.
    const ok = window.confirm(
      bulkSetConfirmText(count, target, workflowLabel, parentIds.length),
    );
    if (!ok) return;
    onSubmit(target);
  };

  return (
    <dialog
      ref={dialogRef}
      onClose={() => onOpenChange(false)}
      className={cn(
        "rounded-2xl glass-menu text-foreground p-0 shadow-2xl backdrop:bg-black/50",
        "w-full max-w-lg",
      )}
      onClick={(e) => {
        if (e.target === dialogRef.current) onOpenChange(false);
      }}
      aria-label={`Set workflow and runtime image on ${count} work items`}
    >
      <form onSubmit={handleSubmit} className="p-6">
        <h2 className="text-lg font-semibold mb-1 flex items-center gap-2">
          <PencilLine aria-hidden="true" className="h-4 w-4" />
          Set workflow &amp; runtime image on {count} work item{count === 1 ? "" : "s"}
        </h2>
        <p className="text-xs text-muted-foreground mb-4">
          Both fields are optional: leave one <span className="font-medium">unchanged</span> and it is
          not sent at all. Choose <span className="font-medium">clear</span> to unbind a workflow
          (empty = unbind) or drop back to the base image.
        </p>

        <div className="space-y-4">
          <div className="space-y-1">
            <label htmlFor="bulk-set-workflow" className="block text-sm font-medium">
              Workflow
            </label>
            <select
              id="bulk-set-workflow"
              value={wfSel}
              onChange={(e) => setWfSel(e.target.value)}
              className="h-9 w-full rounded-xl glass-input px-3 text-sm focus-visible:ring-2 focus-visible:ring-ring"
            >
              <option value={SET_SKIP}>— leave workflow unchanged —</option>
              <option value={SET_CLEAR}>— clear (unbind) —</option>
              {workflows.map((w) => (
                <option key={w.id} value={w.id}>
                  {w.name}
                </option>
              ))}
            </select>
            {hidden > 0 && (
              <p className="text-xs text-muted-foreground">
                {hidden} workflow{hidden === 1 ? "" : "s"} hidden — not published, or has no steps.
                Binding one would produce an item that cannot run.
              </p>
            )}
          </div>

          <div className="space-y-1">
            <label htmlFor="bulk-set-image" className="block text-sm font-medium">
              Runtime image
            </label>
            <select
              id="bulk-set-image"
              value={imgSel}
              onChange={(e) => setImgSel(e.target.value)}
              className="h-9 w-full rounded-xl glass-input px-3 text-sm focus-visible:ring-2 focus-visible:ring-ring"
            >
              <option value={SET_SKIP}>— leave image unchanged —</option>
              <option value={SET_CLEAR}>— clear (base image) —</option>
              {imageOptions.map((tag) => (
                <option key={tag} value={tag}>
                  {tag}
                </option>
              ))}
            </select>
          </div>

          {parentIds.length > 0 && (
            <div className="rounded-md border border-amber-300/60 bg-amber-50/40 dark:bg-amber-950/20 dark:border-amber-900/60 p-3 text-xs text-muted-foreground">
              <div className="flex items-start gap-2">
                <AlertTriangle
                  aria-hidden="true"
                  className="h-3.5 w-3.5 mt-0.5 shrink-0 text-amber-700 dark:text-amber-400"
                />
                <p>
                  {parentIds.length} of the selection{" "}
                  {parentIds.length === 1 ? "is a SEQUENCE PARENT" : "are SEQUENCE PARENTS"} — a
                  parent&apos;s own workflow/image binding is <span className="font-medium">INERT</span>;
                  its children each run their own workflows. The value is stored anyway and does not
                  route the parent.
                </p>
              </div>
            </div>
          )}

          {nothingChosen && (
            <p className="text-xs text-muted-foreground">
              Choose a workflow or a runtime image to set (or to clear).
            </p>
          )}
        </div>

        <div className="mt-6 flex justify-end gap-2">
          <Button type="button" variant="outline" onClick={() => onOpenChange(false)} disabled={isPending}>
            Cancel
          </Button>
          <Button
            type="submit"
            disabled={!canApply}
            title={
              nothingChosen
                ? "Choose a workflow or a runtime image to set (or to clear)"
                : "Apply to every selected work item"
            }
          >
            {isPending ? (
              <>
                <Loader2 aria-hidden="true" className="mr-1 h-3.5 w-3.5 animate-spin" />
                Applying…
              </>
            ) : (
              <>Apply</>
            )}
          </Button>
        </div>
      </form>
    </dialog>
  );
}
