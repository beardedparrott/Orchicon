import { useEffect, useMemo, useRef, useState } from "react";
import {
  AlertTriangle,
  CheckCircle2,
  Loader2,
  PencilLine,
  XCircle,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { ModelPicker } from "@/components/ModelPicker";
import {
  buildBulkPreview,
  SKIP_REASON_LABEL,
} from "@/components/bulkChangeWorkerModel";
import { cn } from "@/lib/utils";
import type { BulkUpdateWorkerModelResult } from "@/api/gen/orchicon/api/v1/worker_service_pb";
import type { Worker } from "@/api/gen/orchicon/api/v1/worker_pb";

interface BulkChangeWorkerModelDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  selectedIds: string[];
  workers: Worker[] | undefined;
  onSubmit: (input: { workerIds: string[]; modelRef: string }) => void;
  isPending: boolean;
  error?: Error | null;
  // results is the response from the last successful apply; when set the
  // dialog renders the per-worker result summary instead of the input form.
  results?: BulkUpdateWorkerModelResult[] | null;
}

export function BulkChangeWorkerModelDialog({
  open,
  onOpenChange,
  selectedIds,
  workers,
  onSubmit,
  isPending,
  error,
  results,
}: BulkChangeWorkerModelDialogProps) {
  const [modelRef, setModelRef] = useState("");
  const dialogRef = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    if (open) dialog.showModal();
    else dialog.close();
  }, [open]);

  useEffect(() => {
    if (!open) {
      // Reset state on close so reopening starts fresh.
      setModelRef("");
    }
  }, [open]);

  const handleClose = () => {
    onOpenChange(false);
  };

  const preview = useMemo(
    () => buildBulkPreview(workers, selectedIds),
    [workers, selectedIds],
  );

  const canApply =
    !isPending &&
    modelRef !== "" &&
    preview.updatable > 0;

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!canApply) return;
    onSubmit({ workerIds: selectedIds, modelRef });
  };

  return (
    <dialog
      ref={dialogRef}
      onClose={handleClose}
      className={cn(
        "rounded-2xl glass-menu text-foreground p-0 shadow-2xl backdrop:bg-black/50",
        "w-full max-w-lg",
      )}
      onClick={(e) => {
        if (e.target === dialogRef.current) handleClose();
      }}
    >
      <form onSubmit={handleSubmit} className="p-6">
        <h2 className="text-lg font-semibold mb-1 flex items-center gap-2">
          <PencilLine className="h-4 w-4" />
          Change model for {selectedIds.length} selected worker
          {selectedIds.length === 1 ? "" : "s"}
        </h2>
        <p className="text-xs text-muted-foreground mb-4">
          Set <span className="font-mono">model_ref</span> on every
          selected worker and publish the affected version in one
          operation. Version numbers do not change — bulk change
          mirrors the manual edit-then-republish flow.
        </p>

        {results ? (
          <BulkResultSummary results={results} workers={workers} />
        ) : (
          <>
            <div className="space-y-2">
              <label
                htmlFor="bulk-model"
                className="block text-sm font-medium"
              >
                New model <span className="text-destructive">*</span>
              </label>
              <ModelPicker value={modelRef} onChange={setModelRef} />
            </div>

            <div className="mt-4 rounded-md border border-amber-300/60 bg-amber-50/40 dark:bg-amber-950/20 dark:border-amber-900/60 p-3 text-xs text-muted-foreground">
              <div className="flex items-start gap-2">
                <AlertTriangle className="h-3.5 w-3.5 mt-0.5 shrink-0 text-amber-600 dark:text-amber-400" />
                <div className="space-y-1">
                  <p className="font-medium text-foreground">
                    Dispatch-time resolution
                  </p>
                  <p>
                    Existing/in-flight executions and runs pinned to a
                    specific worker version are NOT retroactively
                    changed. Only runs that resolve the latest published
                    version on dispatch will see the new model.
                  </p>
                  <p>
                    For "all workers fall back to X" use the tenant
                    default worker model in Settings instead.
                  </p>
                </div>
              </div>
            </div>

            <div className="mt-4 text-xs text-muted-foreground">
              <p>
                <span className="font-medium text-foreground">
                  {preview.updatable}
                </span>{" "}
                worker{preview.updatable === 1 ? "" : "s"} will be
                updated and published.
                {Object.keys(preview.skipped).length > 0 && (
                  <>
                    {" "}
                    <span className="font-medium text-foreground">
                      {Object.values(preview.skipped).reduce(
                        (a, b) => a + b,
                        0,
                      )}
                    </span>{" "}
                    will be skipped —{" "}
                    {Object.entries(preview.skipped)
                      .map(([reason, n]) => `${reason}: ${n}`)
                      .join(", ")}
                    .
                  </>
                )}
              </p>
            </div>

            {error && (
              <p className="mt-3 text-xs text-destructive">
                Failed to apply: {String(error.message ?? error)}
              </p>
            )}
          </>
        )}

        <div className="mt-6 flex justify-end gap-2">
          {results ? (
            <Button type="button" onClick={handleClose}>
              Close
            </Button>
          ) : (
            <>
              <Button
                type="button"
                variant="outline"
                onClick={handleClose}
                disabled={isPending}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={!canApply}>
                {isPending ? (
                  <>
                    <Loader2 className="mr-1 h-3.5 w-3.5 animate-spin" />
                    Applying…
                  </>
                ) : (
                  <>Apply</>
                )}
              </Button>
            </>
          )}
        </div>
      </form>
    </dialog>
  );
}

function BulkResultSummary({
  results,
  workers,
}: {
  results: BulkUpdateWorkerModelResult[];
  workers: Worker[] | undefined;
}) {
  const byId = new Map((workers ?? []).map((w) => [w.id, w] as const));
  const updated = results.filter((r) => r.outcome.case === "updated");
  const skipped = results.filter((r) => r.outcome.case === "skipped");
  const errors = results.filter((r) => r.outcome.case === "error");

  return (
    <div className="space-y-3">
      <div className="rounded-2xl glass-panel p-3 text-xs">
        <p className="font-medium text-foreground">
          {updated.length} updated · {skipped.length} skipped · {errors.length} error
          {updated.length + skipped.length + errors.length === 1 ? "" : "s"}
        </p>
        <p className="text-muted-foreground mt-1">
          Existing/pinned runs are not retroactively changed; only
          future dispatches resolving the latest published version
          pick up the new model.
        </p>
      </div>
      <div className="max-h-64 overflow-y-auto rounded-md border divide-y">
        {results.map((r) => {
          const w = byId.get(r.workerId);
          const name = w?.name ?? r.workerId;
          switch (r.outcome.case) {
            case "updated": {
              const u = r.outcome.value;
              return (
                <div
                  key={r.workerId}
                  className="flex items-start gap-2 px-3 py-2 text-xs"
                >
                  <CheckCircle2 className="h-3.5 w-3.5 mt-0.5 shrink-0 text-green-600" />
                  <div className="min-w-0 flex-1">
                    <p className="font-medium truncate">{name}</p>
                    <p className="text-muted-foreground">
                      v{u.version} (unchanged) → model set and
                      published · <span className="font-mono">{u.modelRef}</span>
                    </p>
                  </div>
                </div>
              );
            }
            case "skipped": {
              const reason = SKIP_REASON_LABEL[r.outcome.value.reason] ?? "skipped";
              return (
                <div
                  key={r.workerId}
                  className="flex items-start gap-2 px-3 py-2 text-xs"
                >
                  <AlertTriangle className="h-3.5 w-3.5 mt-0.5 shrink-0 text-amber-600" />
                  <div className="min-w-0 flex-1">
                    <p className="font-medium truncate">{name}</p>
                    <p className="text-muted-foreground">
                      skipped — {reason}
                    </p>
                  </div>
                </div>
              );
            }
            case "error": {
              const e = r.outcome.value;
              return (
                <div
                  key={r.workerId}
                  className="flex items-start gap-2 px-3 py-2 text-xs"
                >
                  <XCircle className="h-3.5 w-3.5 mt-0.5 shrink-0 text-destructive" />
                  <div className="min-w-0 flex-1">
                    <p className="font-medium truncate">{name}</p>
                    <p className="text-destructive">
                      error — {e.message || "unknown"}
                    </p>
                  </div>
                </div>
              );
            }
            default:
              return null;
          }
        })}
      </div>
      <Card className="border-muted">
        <CardHeader className="pb-2">
          <CardTitle className="text-sm">Why did this skip some workers?</CardTitle>
          <CardDescription className="text-xs">
            Deprecated and retired workers cannot be edited via this
            path; the bulk operation leaves them untouched and reports
            them in the summary above.
          </CardDescription>
        </CardHeader>
      </Card>
    </div>
  );
}
