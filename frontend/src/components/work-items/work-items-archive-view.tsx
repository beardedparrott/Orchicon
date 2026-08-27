// Dedicated Archive view for the Work Items page. Lists archived work items
// (fetched with ListWorkItems include_archived=true) and offers Restore per
// row. Unlike the board/tree, it is a simple flat list — archived items have
// no active hierarchy, so there is no cascade/selection.
//
// Per design: restored items return to their prior terminal status (the
// archived_from_status the server recorded at archive time), never pending.

import { Archive as ArchiveIcon, RotateCcw } from "lucide-react";

import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { Button } from "@/components/ui/button";
import { KindPill } from "@/components/work-items/work-item-badges";
import { statusMeta, statusMetaFromString } from "@/components/work-items/work-item-meta";
import { cn } from "@/lib/utils";

export interface WorkItemsArchiveViewProps {
  items?: WorkItem[];
  isLoading: boolean;
  error?: unknown;
  /** @param id the archived work item id to restore */
  onRestore: (id: string) => void;
  restorePending: boolean;
}

/** Format an archived-at timestamp to a compact local date+time. */
function formatArchivedAt(archivedAt?: WorkItem["archivedAt"]): string {
  if (!archivedAt) return "";
  const d = new Date(Number(archivedAt.seconds) * 1000);
  return d.toLocaleString([], {
    month: "short",
    day: "numeric",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function WorkItemsArchiveView({
  items,
  isLoading,
  error,
  onRestore,
  restorePending,
}: WorkItemsArchiveViewProps) {
  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading archived work items…</p>;
  }
  if (error) {
    return <p className="text-sm text-destructive">Failed to load archived work items: {String(error)}</p>;
  }
  const archived = items ?? [];
  if (archived.length === 0) {
    return (
      <div className="flex flex-col items-center gap-2 py-16 text-center text-muted-foreground">
        <ArchiveIcon aria-hidden="true" className="h-8 w-8" />
        <p className="text-sm">No archived work items.</p>
      </div>
    );
  }
  return (
    <div className="space-y-2">
      {archived.map((item) => {
        const original = item.archivedFromStatus
          ? statusMetaFromString(item.archivedFromStatus)
          : statusMeta(0);
        return (
          <div
            key={item.id}
            className="flex items-center gap-3 rounded-2xl glass-panel px-4 py-3"
          >
            <KindPill kind={item.kind} />
            <div className="min-w-0 flex-1">
              <p className="truncate text-sm font-medium text-foreground">{item.title}</p>
              <p className="mt-0.5 flex flex-wrap items-center gap-x-3 gap-y-0.5 text-xs text-muted-foreground">
                <span className={cn("inline-flex items-center gap-1 rounded-full px-1.5 py-0.5 text-[10px] font-medium", original.pill)}>
                  <span className={cn("h-1 w-1 rounded-full", original.dot)} />
                  Archived from: {original.label}
                </span>
                {item.archivedAt && (
                  <span>Archived {formatArchivedAt(item.archivedAt)}</span>
                )}
                <span className="truncate">v{item.version}</span>
              </p>
            </div>
            <Button
              variant="outline"
              size="sm"
              onClick={() => onRestore(item.id)}
              disabled={restorePending}
              title="Restore this work item to the active views"
            >
              <RotateCcw aria-hidden="true" className="mr-1 h-3.5 w-3.5" />
              Restore
            </Button>
          </div>
        );
      })}
    </div>
  );
}
