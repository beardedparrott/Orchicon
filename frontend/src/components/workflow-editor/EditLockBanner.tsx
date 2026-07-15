// Edit lock banner + status badges for the workflow editor.
//
// The banner shows three states based on the workflow edit lock
// (docs/07 §3.3): acquired, held by another user, or absent (transient
// while the lock request is in flight).
//
// Status badges are small pill-shaped components used in the version
// history and runs lists below the canvas.

import { cn } from "@/lib/utils";

export function EditLockBanner({
  lockAcquired,
  lockHeldByOther,
  heldBy,
}: {
  lockAcquired: boolean;
  lockHeldByOther: boolean;
  heldBy: string;
}) {
  if (lockAcquired) {
    return (
      <div
        className="flex items-start gap-2 rounded-md border border-emerald-300/60 bg-emerald-50 p-2.5 text-sm text-emerald-900 dark:border-emerald-800/60 dark:bg-emerald-950/40 dark:text-emerald-100"
        role="status"
      >
        <span aria-hidden className="mt-0.5 inline-block h-2 w-2 shrink-0 rounded-full bg-emerald-500" />
        <span>
          <strong className="font-semibold">Edit lock acquired</strong> — you can edit
          this workflow. <kbd className="rounded border bg-background/60 px-1 font-mono text-[10px]">Save</kbd>{" "}
          persists the draft; <kbd className="rounded border bg-background/60 px-1 font-mono text-[10px]">Publish</kbd>{" "}
          makes it immutable.
        </span>
      </div>
    );
  }
  if (lockHeldByOther) {
    return (
      <div
        className="flex items-start gap-2 rounded-md border border-amber-300/60 bg-amber-50 p-2.5 text-sm text-amber-900 dark:border-amber-800/60 dark:bg-amber-950/40 dark:text-amber-100"
        role="status"
      >
        <span aria-hidden className="mt-0.5 inline-block h-2 w-2 shrink-0 rounded-full bg-amber-500" />
        <span>
          <strong className="font-semibold">Currently being edited by</strong>{" "}
          <span className="font-mono text-xs">{heldBy || "another user"}</span> — viewing
          read-only. The lock expires automatically on disconnect.
        </span>
      </div>
    );
  }
  return null;
}

export function VersionStatusBadge({ status }: { status: number }) {
  const labels: Record<number, string> = {
    1: "draft",
    2: "published",
    3: "deprecated",
  };
  const styles: Record<number, string> = {
    1: "bg-blue-100 text-blue-800 dark:bg-blue-950/60 dark:text-blue-200",
    2: "bg-emerald-100 text-emerald-800 dark:bg-emerald-950/60 dark:text-emerald-200",
    3: "bg-amber-100 text-amber-800 dark:bg-amber-950/60 dark:text-amber-200",
  };
  return (
    <span
      className={cn(
        "rounded-full px-2 py-0.5 text-[10px] font-medium",
        styles[status] ?? "bg-muted text-muted-foreground",
      )}
    >
      {labels[status] ?? "unknown"}
    </span>
  );
}

export function RunStatusBadge({ status }: { status: number }) {
  const labels: Record<number, string> = {
    1: "pending",
    2: "running",
    3: "completed",
    4: "failed",
    5: "aborted",
    6: "paused",
  };
  const styles: Record<number, string> = {
    1: "bg-gray-200 text-gray-700 dark:bg-gray-800 dark:text-gray-300",
    2: "bg-blue-100 text-blue-800 dark:bg-blue-950/60 dark:text-blue-200",
    3: "bg-emerald-100 text-emerald-800 dark:bg-emerald-950/60 dark:text-emerald-200",
    4: "bg-rose-100 text-rose-800 dark:bg-rose-950/60 dark:text-rose-200",
    5: "bg-gray-300 text-gray-700 dark:bg-gray-700 dark:text-gray-200",
    6: "bg-amber-100 text-amber-800 dark:bg-amber-950/60 dark:text-amber-200",
  };
  return (
    <span
      className={cn(
        "rounded-full px-2 py-0.5 text-[10px] font-medium",
        styles[status] ?? "bg-muted text-muted-foreground",
      )}
    >
      {labels[status] ?? "unknown"}
    </span>
  );
}
