// Bulk worker-model change helpers.
//
// Pure functions used by the BulkChangeWorkerModelDialog (preview) and
// reusable by other callers that want the same per-worker classification
// without dragging in the React component. The server is the source of
// truth at apply time; this is a local preview computed from the loaded
// worker list so the dialog can show the user what they are about to
// commit without an extra round trip.

import {
  WorkerStatus,
  type Worker,
} from "@/api/gen/orchicon/api/v1/worker_pb";
import { BulkUpdateWorkerModelSkipReason } from "@/api/gen/orchicon/api/v1/worker_service_pb";

// SkipReasonLabel turns a SkipReason enum value into a short,
// human-readable string for the dialog body + per-worker result rows.
// Centralized so the dialog and the result summary share one copy of
// the labels.
export const SKIP_REASON_LABEL: Record<number, string> = {
  [BulkUpdateWorkerModelSkipReason.NOT_FOUND]: "not found",
  [BulkUpdateWorkerModelSkipReason.DEPRECATED]: "deprecated",
  [BulkUpdateWorkerModelSkipReason.RETIRED]: "retired",
  [BulkUpdateWorkerModelSkipReason.NO_PUBLISHED_VERSION]: "no published version",
  [BulkUpdateWorkerModelSkipReason.UNSPECIFIED]: "skipped",
};

export interface BulkPreview {
  updatable: number;
  skipped: Record<string, number>;
}

// buildPreview classifies every selected worker id by what the server
// will do at apply time. The buckets match the per-worker branching in
// BulkUpdateWorkerModel:
//   - draft → updatable
//   - published (with or without an orphan draft on top) → updatable
//   - deprecated / retired / no published version → skipped (reason)
//   - id not present in the loaded list → skipped (not found)
//
// Workers loaded with status=deprecated or status=retired can never be
// updated via this path; workers with currentVersion<=0 have no
// published version to edit. ids not in the list are treated as deleted
// between page-load and apply-time (the server's authoritative NOT_FOUND
// is still authoritative at apply time — this is a preview, not a
// promise).
export function buildBulkPreview(
  workers: Worker[] | undefined,
  selectedIds: string[],
): BulkPreview {
  const updatable: string[] = [];
  const skipped: Record<string, number> = {};
  if (!workers) return { updatable: 0, skipped };
  const byId = new Map(workers.map((w) => [w.id, w] as const));
  for (const id of selectedIds) {
    const w = byId.get(id);
    if (!w) {
      skipped["not found"] = (skipped["not found"] ?? 0) + 1;
      continue;
    }
    if (w.status === WorkerStatus.DEPRECATED) {
      skipped["deprecated"] = (skipped["deprecated"] ?? 0) + 1;
      continue;
    }
    if (w.status === WorkerStatus.RETIRED) {
      skipped["retired"] = (skipped["retired"] ?? 0) + 1;
      continue;
    }
    if (w.currentVersion <= 0) {
      skipped["no published version"] =
        (skipped["no published version"] ?? 0) + 1;
      continue;
    }
    updatable.push(id);
  }
  return { updatable: updatable.length, skipped };
}
